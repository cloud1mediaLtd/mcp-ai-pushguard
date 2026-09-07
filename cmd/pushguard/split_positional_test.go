package main

import (
	"slices"
	"testing"
)

// `await <job-id> --timeout 2400` silently used the 900-second default,
// because Go's flag package stops parsing at the first non-flag argument — and
// that is the exact order this command's own usage string documents. The cost
// was a wait that reported a timeout on a job which was only queued behind
// others, which reads as a broken gate rather than a busy queue.
func TestSplitPositionalAcceptsFlagsOnEitherSide(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantID   string
		wantRest []string
	}{
		{"flag after the operand", []string{"pg_abc", "--timeout", "2400"}, "pg_abc", []string{"--timeout", "2400"}},
		// Flags first is the order Go's parser already handles, so the splitter
		// deliberately leaves it alone and handleAwait reads fs.Arg(0).
		{"flag before the operand", []string{"--timeout", "2400", "pg_abc"}, "", []string{"--timeout", "2400", "pg_abc"}},
		{"operand only", []string{"pg_abc"}, "pg_abc", []string{}},
		// A flag VALUE is a bare word too; taking it as the operand is the bug
		// the naive version had.
		{"single-dash form", []string{"pg_abc", "-timeout=60"}, "pg_abc", []string{"-timeout=60"}},
		{"no operand, bare flag value", []string{"--timeout", "60"}, "", []string{"--timeout", "60"}},
		{"nothing at all", []string{}, "", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, rest := splitPositional(tc.args)
			if id != tc.wantID {
				t.Errorf("job id = %q, want %q", id, tc.wantID)
			}
			if !slices.Equal(rest, tc.wantRest) {
				t.Errorf("remaining args = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

// Only the FIRST bare argument is the operand; a later one is left for the flag
// parser to reject rather than silently swallowed as a second job id.
func TestSplitPositionalTakesOnlyTheFirstOperand(t *testing.T) {
	id, rest := splitPositional([]string{"pg_first", "pg_second"})
	if id != "pg_first" {
		t.Errorf("job id = %q, want pg_first", id)
	}
	if !slices.Equal(rest, []string{"pg_second"}) {
		t.Errorf("remaining args = %v, want the second operand left for the parser", rest)
	}
}

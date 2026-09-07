package core

import (
	"regexp"
	"strings"
)

// A failed job's logs are mostly success. A Go test suite that fails one
// assertion still prints several hundred "ok" lines, and the tail an agent or a
// human gets back is whatever happened to run last -- which is usually not the
// thing that failed.
//
// So a blind tail makes the caller grep the log themselves to answer the only
// question they asked: why did this fail? These are the markers that answer it.
//
// Ordered most specific first. The first pattern that matches anything wins,
// so a run with a real assertion failure reports that rather than the generic
// "make: *** [target] Error 2" it also emitted.
var failureMarkers = []*regexp.Regexp{
	// Go test: the failing test and its message.
	regexp.MustCompile(`^\s*--- FAIL: `),
	// Go build/vet/lint diagnostics: path:line:col: message.
	regexp.MustCompile(`^[^\s:]+\.go:\d+:\d+: `),
	// Node/vitest/jest.
	regexp.MustCompile(`^\s*(?:✗|×|FAIL|●) `),
	// Package-level Go failures, after the specific ones above.
	regexp.MustCompile(`^(?:FAIL|ok)?\s*FAIL\s`),
	// Panics and fatals.
	regexp.MustCompile(`^(?:panic:|fatal error:|fatal: )`),
	// A tool that failed for its own reasons rather than because the code is
	// wrong -- "Error: parallel golangci-lint is running", a missing binary, a
	// database that would not start. Ranked here because it is a real cause and
	// make is not, but below the code-level markers: a run with BOTH a failing
	// test and a noisy tool error is better summarised by the failing test.
	regexp.MustCompile(`^(?:Error|ERROR|error): `),
	// Make, last: it is the consequence of one of the above, not the cause.
	regexp.MustCompile(`^make(?:\[\d+\])?: \*\*\* `),
}

// FailureContext is how many lines after a matched marker are worth keeping.
// A Go test failure puts its message on the following line or two; more than
// this and unrelated output starts bleeding in.
const FailureContext = 3

// ExtractFailure returns the lines of log that explain a failure, or "" when
// none of the markers match.
//
// It returns at most maxLines, taking the LAST matches rather than the first:
// a run that fails many packages is best summarised by where it gave up, and
// truncating from the front would bury that behind the earliest failure.
func ExtractFailure(logs string, maxLines int) string {
	if strings.TrimSpace(logs) == "" || maxLines <= 0 {
		return ""
	}
	lines := strings.Split(logs, "\n")

	for _, marker := range failureMarkers {
		keep := map[int]bool{}
		for i, line := range lines {
			if !marker.MatchString(line) {
				continue
			}
			for j := i; j < len(lines) && j <= i+FailureContext; j++ {
				keep[j] = true
			}
		}
		if len(keep) == 0 {
			continue
		}

		indexes := make([]int, 0, len(keep))
		for i := range keep {
			indexes = append(indexes, i)
		}
		// keep is a set, so sort before slicing -- and drop from the FRONT so
		// the last failure survives.
		sortInts(indexes)
		if len(indexes) > maxLines {
			indexes = indexes[len(indexes)-maxLines:]
		}

		out := make([]string, 0, len(indexes))
		previous := -2
		for _, i := range indexes {
			if previous >= 0 && i > previous+1 {
				out = append(out, "  …")
			}
			out = append(out, lines[i])
			previous = i
		}
		return strings.Join(out, "\n")
	}
	return ""
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

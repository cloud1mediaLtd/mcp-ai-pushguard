package core

import (
	"strings"
	"testing"
)

// Both fixtures are real output from this repository's own consumer
// (c1m-server `make pre-push`), not invented. That matters: the value of the
// extractor is entirely in whether it beats a blind tail on the logs people
// actually get, and both of these were cases where the tail did not answer the
// question.
func TestExtractFailureFindsTheFailingTestUnderHundredsOfPassingLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 300; i++ {
		b.WriteString("ok  \tgithub.com/cloud1media/c1m-server/internal/workers/pkg\t1.4s\n")
	}
	b.WriteString("--- FAIL: TestInternalImportAliasesDoNotGrow (0.13s)\n")
	b.WriteString("    package_naming_test.go:171: aliased internal imports fell to 1997, below budget 1998 — bank it.\n")
	b.WriteString("FAIL\n")
	b.WriteString("FAIL\tgithub.com/cloud1media/c1m-server/tests/architecture\t34.818s\n")
	b.WriteString("make[1]: *** [test] Error 1\n")

	got := ExtractFailure(b.String(), 20)
	if got == "" {
		t.Fatal("no failure extracted from a log that plainly contains one")
	}
	if !strings.Contains(got, "TestInternalImportAliasesDoNotGrow") {
		t.Errorf("extract did not name the failing test:\n%s", got)
	}
	if !strings.Contains(got, "below budget 1998") {
		t.Errorf("extract dropped the message that says what to do:\n%s", got)
	}
	if strings.Contains(got, "ok  \tgithub.com") {
		t.Errorf("extract included passing packages — that is the noise it exists to remove:\n%s", got)
	}
}

// The failure that cost the most today: a test file no longer compiled after a
// return type changed. `go build ./...` cannot see it because it does not build
// test files, so the only signal was in the lint stage's output.
func TestExtractFailureFindsACompileDiagnostic(t *testing.T) {
	logs := strings.Join([]string{
		"./scripts/check-stale-imports.sh",
		"Stale import guard passed: no deleted package paths referenced.",
		"golangci-lint run ./...",
		"cmd/worker/workspace_app_projection_test.go:33:49: invalid operation: k.AppManifestLoader() != manifest (mismatched types *apps.Loader and *access.Runtime) (typecheck)",
		"package main",
		"1 issues:",
		"* typecheck: 1",
		"make[1]: *** [lint] Error 1",
	}, "\n")

	got := ExtractFailure(logs, 20)
	if !strings.Contains(got, "mismatched types") {
		t.Fatalf("extract did not surface the compile error:\n%s", got)
	}
	if strings.Contains(got, "Stale import guard passed") {
		t.Errorf("extract included a passing stage:\n%s", got)
	}
}

// Make is the consequence, never the cause. It is still the answer when it is
// the only marker present.
func TestExtractFailurePrefersTheCauseOverMake(t *testing.T) {
	logs := "--- FAIL: TestThing (0.1s)\n    thing_test.go:9: wanted X\nmake: *** [test] Error 2"
	got := ExtractFailure(logs, 20)
	if !strings.Contains(got, "TestThing") {
		t.Fatalf("extract chose make over the failing test:\n%s", got)
	}

	onlyMake := "building...\nmake: *** [build] Error 2"
	if got := ExtractFailure(onlyMake, 20); !strings.Contains(got, "make: ***") {
		t.Fatalf("make is the only marker present and must be reported:\n%s", got)
	}
}

// A run that fails many packages is best summarised by where it gave up.
func TestExtractFailureKeepsTheLastFailuresWhenTruncating(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("--- FAIL: TestNumber" + string(rune('A'+i%26)) + "\n")
	}
	b.WriteString("--- FAIL: TestFinalOne\n")

	got := ExtractFailure(b.String(), 5)
	if !strings.Contains(got, "TestFinalOne") {
		t.Fatalf("truncation dropped the last failure, which is the one that stopped the run:\n%s", got)
	}
	if lines := strings.Count(got, "\n") + 1; lines > 5 {
		t.Fatalf("returned %d lines, want at most 5", lines)
	}
}

func TestExtractFailureReturnsNothingForAPassingRun(t *testing.T) {
	logs := "ok  \tpkg/a\t1.0s\nok  \tpkg/b\t2.0s\npre-push: PASS"
	if got := ExtractFailure(logs, 20); got != "" {
		t.Fatalf("a passing run must extract nothing, got:\n%s", got)
	}
	if got := ExtractFailure("", 20); got != "" {
		t.Fatalf("empty logs must extract nothing, got: %q", got)
	}
}

// A tool that fell over for its own reasons. Found by running this extractor
// against a real job: it reported `make: *** [lint] Error 3`, the consequence,
// while the line that actually explained it sat two above.
func TestExtractFailureReportsAToolErrorRatherThanMake(t *testing.T) {
	logs := strings.Join([]string{
		"./scripts/check-stale-imports.sh",
		"Stale import guard passed: no deleted package paths referenced.",
		"golangci-lint run ./...",
		"Error: parallel golangci-lint is running",
		"The command is terminated due to an error: parallel golangci-lint is running",
		"make: *** [lint] Error 3",
	}, "\n")

	got := ExtractFailure(logs, 20)
	if !strings.Contains(got, "parallel golangci-lint is running") {
		t.Fatalf("extract reported the consequence instead of the cause:\n%s", got)
	}
}

// But a genuine code failure still outranks a noisy tool error.
func TestExtractFailurePrefersACodeFailureOverAToolError(t *testing.T) {
	logs := strings.Join([]string{
		"Error: some tool complaining",
		"--- FAIL: TestRealProblem (0.1s)",
		"    real_test.go:12: wanted 3, got 4",
		"make: *** [test] Error 2",
	}, "\n")

	got := ExtractFailure(logs, 20)
	if !strings.Contains(got, "TestRealProblem") {
		t.Fatalf("a real test failure must outrank a tool error:\n%s", got)
	}
}

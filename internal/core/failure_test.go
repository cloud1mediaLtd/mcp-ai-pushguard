package core

import (
	"strings"
	"testing"
)

// Both fixtures are modeled on real output from a large Go monorepo's
// `make pre-push`, not invented. That matters: the value of the extractor is
// entirely in whether it beats a blind tail on the logs people actually get,
// and both of these were cases where the tail did not answer the question.
func TestExtractFailureFindsTheFailingTestUnderHundredsOfPassingLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 300; i++ {
		b.WriteString("ok  \tgithub.com/example/backend/internal/workers/pkg\t1.4s\n")
	}
	b.WriteString("--- FAIL: TestInternalImportAliasesDoNotGrow (0.13s)\n")
	b.WriteString("    package_naming_test.go:171: aliased internal imports fell to 1997, below budget 1998 — bank it.\n")
	b.WriteString("FAIL\n")
	b.WriteString("FAIL\tgithub.com/example/backend/tests/architecture\t34.818s\n")
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

// The regression this exists for: two agents read one failed job through
// `pushguard status`, reported three failures and four respectively, and the
// log held five. ExtractFailure is a bounded window that drops from the front,
// so each saw a different subset and each believed it was complete.
func TestFailureNamesListsEveryFailure(t *testing.T) {
	logs := strings.Join([]string{
		"--- FAIL: TestAuthorizationReviewInventory (0.00s)",
		"    authorization_test.go:46: inventory changed",
		"--- FAIL: TestEveryRoutingModuleIsCatalogued (0.00s)",
		"--- FAIL: TestInternalImportAliasesDoNotGrow (0.12s)",
		"--- FAIL: TestMutatingOpsDeclareIdempotency (0.00s)",
		"--- FAIL: TestRawEndpointInventoryDoesNotGrow (0.08s)",
	}, "\n")

	got := FailureNames(logs)
	want := []string{
		"TestAuthorizationReviewInventory", "TestEveryRoutingModuleIsCatalogued",
		"TestInternalImportAliasesDoNotGrow", "TestMutatingOpsDeclareIdempotency",
		"TestRawEndpointInventoryDoesNotGrow",
	}
	if len(got) != len(want) {
		t.Fatalf("named %d failures, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("failure %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A long log must not lose earlier failures the way the excerpt does.
func TestFailureNamesSurvivesALogLongerThanTheExcerpt(t *testing.T) {
	var b strings.Builder
	b.WriteString("--- FAIL: TestFirst (0.00s)\n")
	for range 200 {
		b.WriteString("some other output line\n")
	}
	b.WriteString("--- FAIL: TestLast (0.00s)\n")

	got := FailureNames(b.String())
	if len(got) != 2 || got[0] != "TestFirst" || got[1] != "TestLast" {
		t.Fatalf("expected both failures across a long log, got %v", got)
	}
	// The excerpt genuinely cannot show both — which is why the names exist.
	if excerpt := ExtractFailure(b.String(), 30); strings.Contains(excerpt, "TestFirst") {
		t.Log("excerpt happened to include the first failure; the names are still the reliable list")
	}
}

// Subtests belong to their parent; five subtest failures of one test are one
// broken test, not five.
func TestFailureNamesCollapsesSubtests(t *testing.T) {
	logs := "--- FAIL: TestParent (0.00s)\n    --- FAIL: TestParent/case_one (0.00s)\n    --- FAIL: TestParent/case_two (0.00s)\n"
	if got := FailureNames(logs); len(got) != 1 || got[0] != "TestParent" {
		t.Fatalf("expected one parent failure, got %v", got)
	}
}

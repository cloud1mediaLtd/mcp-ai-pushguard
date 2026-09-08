package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pushguard/pushguard/internal/git"
)

type runnerFunc func(context.Context, *Job, string) (int, error)

func (r runnerFunc) Run(ctx context.Context, j *Job, dir string) (int, error) { return r(ctx, j, dir) }

func testEngine(t *testing.T, r RunnerInterface) *Engine {
	t.Helper()
	wm, err := git.NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(EngineConfig{DefaultTimeout: time.Second}, newFakeStore(), wm, r, nil, nil)
	t.Cleanup(e.Stop)
	return e
}

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestEnqueueCommittedDefaultAndExplicitSnapshot(t *testing.T) {
	repo := initTempGitRepo(t)
	base := gitOutput(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("dirty"), 0600); err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, nil)
	for _, snapshot := range []bool{false, true} {
		j, _, err := e.Enqueue(EnqueueRequest{RepoPath: repo, Snapshot: snapshot, Command: []string{"true"}})
		if err != nil {
			t.Fatal(err)
		}
		content := gitOutput(t, repo, "show", j.Ref+":README.md")
		if snapshot && (content != "dirty" || j.SourceKind != "snapshot" || j.Ref == base) {
			t.Fatalf("snapshot wrong: %+v content %q", j, content)
		}
		if !snapshot && (content != "test" || j.Ref != base || j.SourceKind != "commit") {
			t.Fatalf("committed default wrong: %+v content %q", j, content)
		}
		expectedBase := ""
		if snapshot {
			expectedBase = base
		}
		if j.CommitSHA != j.Ref || j.BaseSHA != expectedBase || j.RequestedRef != "HEAD" {
			t.Fatalf("wrong identity: %+v", j)
		}
	}
	if got := gitOutput(t, repo, "diff", "--", "README.md"); !strings.Contains(got, "dirty") {
		t.Fatal("enqueue changed working files")
	}
}

func TestEnqueueReusesOnlyActiveMatchingVerification(t *testing.T) {
	repo := initTempGitRepo(t)
	e := testEngine(t, nil)
	req := EnqueueRequest{RepoPath: repo, Command: []string{"true"}, DedupeKey: "pre-push:sha"}
	first, _, err := e.Enqueue(req)
	if err != nil {
		t.Fatal(err)
	}
	second, position, err := e.Enqueue(req)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || !second.Reused || position != 0 {
		t.Fatalf("active duplicate = %+v at position %d, first=%s", second, position, first.ID)
	}
	stored, err := e.store.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.setResult(0, StatusPassed, "")
	stored.finalize(time.Now())
	if err := e.store.Update(stored); err != nil {
		t.Fatal(err)
	}
	third, _, err := e.Enqueue(req)
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID || third.Reused {
		t.Fatalf("completed job was reused: %+v", third)
	}
	if third.DedupeKey != req.DedupeKey {
		t.Fatalf("dedupe key lost: %+v", third)
	}
}

func TestEnqueueRecordsDaemonBuildForJobLogs(t *testing.T) {
	repo := initTempGitRepo(t)
	wm, err := git.NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(EngineConfig{DefaultTimeout: time.Second, Build: "0.2.0+test-build"}, newFakeStore(), wm, nil, nil, nil)
	t.Cleanup(e.Stop)
	job, _, err := e.Enqueue(EnqueueRequest{RepoPath: repo, Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if job.DaemonBuild != "0.2.0+test-build" {
		t.Fatalf("job daemon build = %q", job.DaemonBuild)
	}
}

func TestEnqueueDoesNotReuseSnapshots(t *testing.T) {
	repo := initTempGitRepo(t)
	e := testEngine(t, nil)
	for i := 0; i < 2; i++ {
		job, _, err := e.Enqueue(EnqueueRequest{RepoPath: repo, Snapshot: true, Command: []string{"true"}, DedupeKey: "same-key"})
		if err != nil {
			t.Fatal(err)
		}
		if job.Reused {
			t.Fatalf("snapshot %d unexpectedly reused", i)
		}
	}
}

func TestEnqueueConcurrentDuplicatesShareOneActiveJob(t *testing.T) {
	repo := initTempGitRepo(t)
	e := testEngine(t, nil)
	const callers = 8
	ids := make(chan string, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, _, err := e.Enqueue(EnqueueRequest{RepoPath: repo, Command: []string{"true"}, DedupeKey: "same-gate"})
			if err != nil {
				errs <- err
				return
			}
			ids <- job.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("duplicate jobs %s and %s", first, id)
		}
	}
	if len(e.ListJobs(0)) != 1 {
		t.Fatalf("stored jobs = %d", len(e.ListJobs(0)))
	}
}

func TestEnqueueFreezesMovingRefAndCleansBeforeCompletion(t *testing.T) {
	repo := initTempGitRepo(t)
	base := gitOutput(t, repo, "rev-parse", "HEAD")
	e := testEngine(t, runnerFunc(func(_ context.Context, j *Job, dir string) (int, error) {
		content, err := os.ReadFile(filepath.Join(dir, "README.md"))
		if err != nil || string(content) != "test" {
			return 1, errors.New("wrong checkout")
		}
		return 0, nil
	}))
	j, _, err := e.Enqueue(EnqueueRequest{RepoPath: repo, Ref: "HEAD", Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("next"), 0600); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "add", "README.md")
	gitOutput(t, repo, "commit", "-m", "next")
	e.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done, err := e.AwaitJob(ctx, j.ID, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusPassed || done.CommitSHA != base {
		t.Fatalf("wrong result: %+v", done)
	}
	if _, err := os.Stat(done.WorktreeDir); !os.IsNotExist(err) {
		t.Fatalf("terminal result preceded cleanup: %v", err)
	}
}

func TestEnqueueRejectsUnsafeAndMissingVerification(t *testing.T) {
	repo := initTempGitRepo(t)
	e := testEngine(t, nil)
	tests := []struct {
		name string
		req  EnqueueRequest
	}{
		{"auto-push", EnqueueRequest{AutoPush: true}},
		{"conflicting-source", EnqueueRequest{Snapshot: true, Ref: "HEAD"}},
		{"negative-timeout", EnqueueRequest{Timeout: -time.Second}},
		{"unknown-ref", EnqueueRequest{Ref: "does-not-exist"}},
		{"tree-ref", EnqueueRequest{Ref: "HEAD^{tree}"}},
		{"no-command", EnqueueRequest{}},
		{"blank-command", EnqueueRequest{Command: []string{"  "}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.RepoPath = repo
			if _, _, err := e.Enqueue(tt.req); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	if len(e.ListJobs(0)) != 0 {
		t.Fatal("rejected request left job records")
	}
}

func TestQueueFullAndDrainingRejectWithoutRecords(t *testing.T) {
	repo := initTempGitRepo(t)
	e := testEngine(t, nil)
	e.queue = make(chan *Job, 1)
	req := EnqueueRequest{RepoPath: repo, Command: []string{"true"}}
	if _, _, err := e.Enqueue(req); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Enqueue(EnqueueRequest{RepoPath: repo, Command: []string{"echo second"}}); err == nil {
		t.Fatal("full queue accepted job")
	}
	if len(e.ListJobs(0)) != 1 {
		t.Fatal("full queue created ghost job")
	}
	if e.TryDrainIdle(0) {
		t.Fatal("drained with accepted work")
	}
	e.Stop()
	e.Stop()
	if _, _, err := e.Enqueue(req); err == nil {
		t.Fatal("stopped engine accepted job")
	}
}

func TestQueuedCancellationFinalizesWithoutRunning(t *testing.T) {
	repo := initTempGitRepo(t)
	e := testEngine(t, runnerFunc(func(context.Context, *Job, string) (int, error) { t.Error("cancelled job ran"); return 0, nil }))
	j, _, err := e.Enqueue(EnqueueRequest{RepoPath: repo, Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	done, err := e.AwaitJob(context.Background(), j.ID, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusCancelled || done.FinishedAt == nil {
		t.Fatalf("unfinished cancellation: %+v", done)
	}
	if e.ActiveJobCount() != 0 {
		t.Fatal("cancelled job still active")
	}
	e.Start()
	e.Stop()
}

func TestRunningCancellationWaitsForCleanup(t *testing.T) {
	repo := initTempGitRepo(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	e := testEngine(t, runnerFunc(func(ctx context.Context, _ *Job, _ string) (int, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return 130, ctx.Err()
	}))
	j, _, err := e.Enqueue(EnqueueRequest{RepoPath: repo, Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	e.Start()
	<-started
	if err := e.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	<-cancelled
	current, err := e.GetJob(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.IsFinished() {
		t.Fatal("cancellation published before runner returned")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done, err := e.AwaitJob(ctx, j.ID, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusCancelled || done.FinishedAt == nil {
		t.Fatalf("wrong cancellation: %+v", done)
	}
	if _, err := os.Stat(done.WorktreeDir); !os.IsNotExist(err) {
		t.Fatal("worktree still present")
	}
}

func TestRequestedTimeoutAppliesToExecution(t *testing.T) {
	repo := initTempGitRepo(t)
	e := testEngine(t, runnerFunc(func(ctx context.Context, _ *Job, _ string) (int, error) { <-ctx.Done(); return 130, ctx.Err() }))
	j, _, err := e.Enqueue(EnqueueRequest{RepoPath: repo, Command: []string{"true"}, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	e.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done, err := e.AwaitJob(ctx, j.ID, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusFailed || !strings.Contains(done.Error, "timed out") {
		t.Fatalf("timeout not applied: %+v", done)
	}
}

func TestIdleAdmissionIsAtomic(t *testing.T) {
	repo := initTempGitRepo(t)
	for range 10 {
		e := testEngine(t, nil)
		var wg sync.WaitGroup
		var accepted, drained bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, err := e.Enqueue(EnqueueRequest{RepoPath: repo, Command: []string{"true"}})
			accepted = err == nil
		}()
		go func() { defer wg.Done(); drained = e.TryDrainIdle(0) }()
		wg.Wait()
		if accepted && drained {
			t.Fatal("idle drain raced an accepted job")
		}
	}
}

func TestRequestTimeoutJSONUsesSeconds(t *testing.T) {
	req := EnqueueRequest{Timeout: 15 * time.Second, Snapshot: true}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"timeout_seconds":15`) {
		t.Fatalf("wrong timeout unit: %s", data)
	}
	var decoded EnqueueRequest
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.Timeout != req.Timeout || !decoded.Snapshot {
		t.Fatalf("roundtrip: %+v %v", decoded, err)
	}
	for _, input := range []string{`{"timeout_seconds":-1}`, `{"timeout_seconds":1.5}`, `{"timeout_seconds":9223372036854775807}`} {
		if err := json.Unmarshal([]byte(input), &decoded); err == nil {
			t.Fatalf("accepted invalid timeout: %s", input)
		}
	}
}

type terminalStore struct {
	*fakeStore
	entered chan struct{}
	release chan struct{}
	fail    bool
}

func (s *terminalStore) Update(job *Job) error {
	if job.StatusValue() == StatusPassed {
		close(s.entered)
		<-s.release
		if s.fail {
			return errors.New("disk unavailable")
		}
	}
	return s.fakeStore.Update(job)
}

func TestTerminalSuccessWaitsForPersistence(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "persistence-failure"
		}
		t.Run(name, func(t *testing.T) {
			repo := initTempGitRepo(t)
			e := testEngine(t, runnerFunc(func(context.Context, *Job, string) (int, error) { return 0, nil }))
			s := &terminalStore{fakeStore: newFakeStore(), entered: make(chan struct{}), release: make(chan struct{}), fail: fail}
			e.store = s
			j, _, err := e.Enqueue(EnqueueRequest{RepoPath: repo, Command: []string{"true"}})
			if err != nil {
				t.Fatal(err)
			}
			e.Start()
			<-s.entered
			current, err := e.GetJob(j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.IsFinished() {
				t.Fatal("published success before durable write completed")
			}
			close(s.release)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done, err := e.AwaitJob(ctx, j.ID, time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if fail && (done.Status != StatusFailed || !strings.Contains(done.Error, "persist terminal result")) {
				t.Fatalf("lost persistence failure: %+v", done)
			}
			if !fail && done.Status != StatusPassed {
				t.Fatalf("unexpected result: %+v", done)
			}
		})
	}
}

func TestDetectionReadsSelectedCommitMetadata(t *testing.T) {
	repo := initTempGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte("test:\n\ttrue\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "add", "Makefile")
	gitOutput(t, repo, "commit", "-m", "add test gate")
	old := gitOutput(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte("check:\n\ttrue\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "add", "Makefile")
	gitOutput(t, repo, "commit", "-m", "rename gate")
	if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte("pre-push:\n\ttrue\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, nil)
	e.detectCommand = func(dir string) []string {
		data, err := os.ReadFile(filepath.Join(dir, "Makefile"))
		if err != nil {
			return nil
		}
		target, _, _ := strings.Cut(string(data), ":")
		return []string{"make", target}
	}
	for _, tt := range []struct {
		name, ref, want string
		snapshot        bool
	}{
		{name: "older-commit", ref: old, want: "test"},
		{name: "committed-head", want: "check"},
		{name: "explicit-snapshot", snapshot: true, want: "pre-push"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			j, _, err := e.Enqueue(EnqueueRequest{RepoPath: repo, Ref: tt.ref, Snapshot: tt.snapshot})
			if err != nil {
				t.Fatal(err)
			}
			if len(j.Command) != 2 || j.Command[1] != tt.want {
				t.Fatalf("detected from wrong tree: %v want %q", j.Command, tt.want)
			}
		})
	}
}

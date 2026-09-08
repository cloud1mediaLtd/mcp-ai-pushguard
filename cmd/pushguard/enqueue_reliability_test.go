package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pushguard/pushguard/internal/core"
	"github.com/pushguard/pushguard/internal/daemon"
	pgit "github.com/pushguard/pushguard/internal/git"
	"github.com/pushguard/pushguard/internal/store"
	"github.com/pushguard/pushguard/internal/verification/application"
	"github.com/pushguard/pushguard/pkg/client"
)

type verdictRunner struct {
	code   int
	cancel bool
}

func (r verdictRunner) Run(ctx context.Context, _ *core.Job, _ string) (int, error) {
	if r.cancel {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return r.code, nil
}

func enqueueRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s: %v", out, err)
		}
	}
	return dir
}

func TestEnqueueWaitUsesVerdictCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner verdictRunner
		want   int
	}{
		{"pass", verdictRunner{}, 0}, {"failure_exit_two", verdictRunner{code: 2}, 1},
		{"failure_exit_three", verdictRunner{code: 3}, 1}, {"cancelled", verdictRunner{cancel: true}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "pg-cli-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			wm, err := pgit.NewWorktreeManager(filepath.Join(dir, "trees"))
			if err != nil {
				t.Fatal(err)
			}
			mem := store.NewMemoryStore(10)
			engine := core.NewEngine(core.EngineConfig{}, mem, wm, tc.runner, nil, nil)
			engine.Start()
			defer engine.Stop()
			srv := daemon.NewServer(application.NewEngineService(engine), daemon.Config{SocketPath: filepath.Join(dir, "s")})
			if err := srv.Start(); err != nil {
				t.Fatal(err)
			}
			defer srv.Stop(context.Background())
			c := client.New(filepath.Join(dir, "s"))
			done := make(chan struct{})
			if tc.runner.cancel {
				go func() {
					defer close(done)
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					deadline := time.NewTimer(5 * time.Second)
					defer deadline.Stop()
					for {
						select {
						case <-deadline.C:
							return
						case <-ticker.C:
							jobs := mem.List(0)
							if len(jobs) > 0 {
								_ = engine.Cancel(jobs[0].ID)
								return
							}
						}
					}
				}()
			} else {
				close(done)
			}
			got := runEnqueueWithClient([]string{"--repo", enqueueRepo(t), "--cmd", "printf '%s' 'two words' && true", "--wait", "--wait-timeout", "5"}, c)
			<-done
			if got != tc.want {
				t.Fatalf("exit=%d want=%d", got, tc.want)
			}
			jobs := mem.List(0)
			if len(jobs) != 1 {
				t.Fatalf("jobs=%d", len(jobs))
			}
			if cmd := jobs[0].Command; len(cmd) != 1 || cmd[0] != "printf '%s' 'two words' && true" {
				t.Fatalf("command lost quoting: %#v", cmd)
			}
		})
	}
}

func TestEnqueueRejectsBeforeContactingDaemon(t *testing.T) {
	for _, args := range [][]string{{"--auto-push"}, {"--snapshot", "--ref", "HEAD"}, {"--timeout", "0"}} {
		if code := runEnqueueWithClient(args, client.New(filepath.Join(t.TempDir(), "absent"))); code != 4 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

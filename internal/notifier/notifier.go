package notifier

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/pushguard/pushguard/internal/core"
)

// Notifier dispatches alerts when a job finishes.
type Notifier interface {
	Notify(job *core.Job) error
}

// MultiNotifier chains multiple notifiers together.
type MultiNotifier struct {
	notifiers []Notifier
}

// NewMultiNotifier creates a composite notifier.
func NewMultiNotifier(notifiers ...Notifier) *MultiNotifier {
	return &MultiNotifier{notifiers: notifiers}
}

// Notify triggers all registered notifiers.
func (m *MultiNotifier) Notify(job *core.Job) error {
	var errs []string
	for _, n := range m.notifiers {
		if err := n.Notify(job); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("notifier errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// MacOSNotifier triggers system notification banners and alert sounds on
// macOS. It prefers the `terminal-notifier` CLI (if installed) because plain
// AppleScript's `display notification` has no click action at all — clicking
// one just falls back to whatever macOS does with an unidentified sender
// (commonly: reveal an unrelated, effectively empty folder). terminal-notifier
// lets a click open the job's actual log file instead, and lets repeated
// notifications from the same repo collapse into one stack (`-group`) instead
// of piling up individually — both matter once this runs dozens of times a
// day. Without it installed, PushGuard falls back to the old plain banner:
// less useful on click, but no worse than before.
type MacOSNotifier struct {
	// terminalNotifierPath is resolved once at construction so every Notify
	// call doesn't re-run a PATH lookup.
	terminalNotifierPath string
}

// NewMacOSNotifier creates a macOS notification provider.
func NewMacOSNotifier() *MacOSNotifier {
	path, _ := exec.LookPath("terminal-notifier")
	return &MacOSNotifier{terminalNotifierPath: path}
}

// Notify displays a desktop banner on macOS.
func (m *MacOSNotifier) Notify(job *core.Job) error {
	if runtime.GOOS != "darwin" {
		return nil
	}

	title := "PushGuard: Tests Passed ✅"
	subtitle := fmt.Sprintf("[%s] %s (%s)", job.ID, job.Name, job.DurationString())
	sound := "Glass"

	if job.Status == core.StatusFailed {
		title = "PushGuard: Tests Failed ❌"
		subtitle = fmt.Sprintf("[%s] %s (exit %d)", job.ID, job.Name, job.ExitCode)
		sound = "Basso"
	} else if job.Status == core.StatusCancelled {
		title = "PushGuard: Job Cancelled ⚠️"
		subtitle = fmt.Sprintf("[%s] %s", job.ID, job.Name)
		sound = "Ping"
	}

	if job.AutoPush && job.Status == core.StatusPassed {
		subtitle += " • Auto-pushed to remote"
	}

	if m.terminalNotifierPath != "" {
		args := []string{
			"-title", title,
			"-subtitle", subtitle,
			"-message", strings.Join(job.Command, " "),
			"-sound", sound,
			// Same repo's notifications replace each other in Notification
			// Center instead of stacking up one-by-one across dozens of runs.
			"-group", "pushguard-" + job.RepoPath,
		}
		if job.LogFilePath != "" {
			args = append(args, "-open", "file://"+job.LogFilePath)
		}
		cmd := exec.Command(m.terminalNotifierPath, args...)
		return cmd.Run()
	}

	// Fallback: plain AppleScript banner. No click action is possible here —
	// that's a limitation of `display notification` itself, not a bug in
	// this code. Install terminal-notifier (`brew install terminal-notifier`)
	// to get a banner whose click opens the actual job log.
	script := fmt.Sprintf(
		`display notification %q with title %q sound name %q`,
		subtitle,
		title,
		sound,
	)

	cmd := exec.Command("osascript", "-e", script)
	return cmd.Run()
}

// TerminalBellNotifier rings the terminal bell (\a).
type TerminalBellNotifier struct{}

func (b *TerminalBellNotifier) Notify(job *core.Job) error {
	_, _ = os.Stdout.WriteString("\a")
	return nil
}

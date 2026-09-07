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

// MacOSNotifier uses osascript to trigger system notification banners and alert sounds on macOS.
type MacOSNotifier struct{}

// NewMacOSNotifier creates a macOS notification provider.
func NewMacOSNotifier() *MacOSNotifier {
	return &MacOSNotifier{}
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

	// AppleScript: display notification "subtitle" with title "title" sound name "sound"
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

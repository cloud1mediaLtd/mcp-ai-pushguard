// Package domain holds verification concepts that do not depend on Git,
// storage, process execution, or transports.
package domain

import "fmt"

// Status is the lifecycle state of a verification job.
type Status string

const (
	Queued    Status = "queued"
	Running   Status = "running"
	Passed    Status = "passed"
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
)

// CanTransition documents the only legal lifecycle transitions. The runtime
// service uses this invariant when evolving executor state.
func CanTransition(from, to Status) bool {
	switch from {
	case Queued:
		return to == Running || to == Cancelled || to == Failed
	case Running:
		return to == Passed || to == Failed || to == Cancelled
	default:
		return false
	}
}

// ValidateTransition returns a useful error instead of silently overwriting a
// terminal verdict.
func ValidateTransition(from, to Status) error {
	if CanTransition(from, to) {
		return nil
	}
	return fmt.Errorf("illegal verification lifecycle transition: %s to %s", from, to)
}

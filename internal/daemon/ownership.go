package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Ownership holds exclusive access to a daemon's shared persisted state.
// Acquire it before recovery or cleanup, and retain it until shutdown finishes.
type Ownership struct {
	file *os.File
	once sync.Once
	err  error
}

// AcquireOwnership prevents competing daemons from recovering each other's jobs.
func AcquireOwnership(stateDir string) (*Ownership, error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("daemon state already owned or unavailable: %w", err)
	}
	return &Ownership{file: f}, nil
}

// Close releases ownership. The lock file is retained to avoid inode races.
func (o *Ownership) Close() error {
	o.once.Do(func() { o.err = o.file.Close() })
	return o.err
}

// AssertSocketAvailable refuses recovery or cleanup while a listener owns the
// socket, including legacy daemons that predate the shared-state lock. It does
// not remove stale sockets; Start handles that after ownership is established.
func AssertSocketAvailable(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("socket path is not a socket: %s", path)
	}
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		return fmt.Errorf("daemon socket already active: %s", path)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return nil
	}
	return fmt.Errorf("cannot establish socket availability: %w", err)
}

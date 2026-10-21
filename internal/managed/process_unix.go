//go:build unix

package managed

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Lock takes the data directory for this process. The lock is the kernel's,
// so a process that dies however it dies gives it up.
func Lock(path string) (io.Closer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil
}

// WatchParent calls stop once the process pid is gone, checking every second.
//
// A process that has exited cannot be signalled, and an orphan is adopted by
// another, so either says the parent has gone.
func WatchParent(ctx context.Context, pid int, stop func(reason string)) error {
	if pid <= 0 {
		return errors.New("no parent process id")
	}
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return errors.New("the parent process is not running")
	}
	parent := os.Getppid() == pid
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) || (parent && os.Getppid() != pid) {
				stop("parent exited")
				return
			}
		}
	}()
	return nil
}

// IgnoreBrokenPipe makes a write to a standard stream nobody reads fail
// rather than kill the process: by default Go exits on SIGPIPE from fd 1 or
// 2, and an application that closed the events pipe early must not take the
// instance down with it mid-job.
func IgnoreBrokenPipe() { signal.Ignore(syscall.SIGPIPE) }

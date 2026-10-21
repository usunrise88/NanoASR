//go:build windows

package managed

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// Lock takes the data directory for this process. The lock belongs to the
// handle, so a process that dies however it dies gives it up.
func Lock(path string) (io.Closer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil
}

// WatchParent calls stop once the process pid exits. It waits on the
// process handle, which needs no rights beyond the user's own.
func WatchParent(ctx context.Context, pid int, stop func(reason string)) error {
	if pid <= 0 {
		return errors.New("no parent process id")
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return err
	}
	go func() {
		defer windows.CloseHandle(h)
		for ctx.Err() == nil {
			ev, err := windows.WaitForSingleObject(h, 1000)
			if err != nil {
				return
			}
			if ev == windows.WAIT_OBJECT_0 {
				stop("parent exited")
				return
			}
		}
	}()
	return nil
}

// IgnoreBrokenPipe has nothing to do on Windows: a write to a closed pipe
// fails there without a signal.
func IgnoreBrokenPipe() {}

// LowerPriority puts the process below normal priority, which any process
// may do to itself.
func LowerPriority() error {
	return windows.SetPriorityClass(windows.CurrentProcess(), windows.BELOW_NORMAL_PRIORITY_CLASS)
}

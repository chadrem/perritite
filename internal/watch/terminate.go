package watch

import (
	"errors"
	"os"
	"runtime"
	"syscall"
)

// Terminate asks pid to exit: SIGTERM on Unix, so the waiter prints STOPPED
// and exits cleanly; a hard kill on Windows, which has no SIGTERM (safe,
// because state is always written atomically). Callers only signal a pid
// read from a lock that is currently held, so it is the live waiter's own.
func Terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		err = p.Kill()
	} else {
		err = p.Signal(syscall.SIGTERM)
	}
	if errors.Is(err, os.ErrProcessDone) {
		return nil // it exited on its own in the meantime
	}
	return err
}

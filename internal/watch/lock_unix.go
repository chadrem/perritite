//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package watch

import (
	"errors"
	"os"
	"syscall"
)

// lockFile opens path and takes an exclusive, non-blocking flock on it. The
// kernel drops the lock when the process dies, however it dies, so a
// killed waiter can never leave a stale lock behind.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errLocked
		}
		return nil, err
	}
	return f, nil
}

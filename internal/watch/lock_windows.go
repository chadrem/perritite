//go:build windows

package watch

import (
	"errors"
	"os"
	"syscall"
)

const errorSharingViolation syscall.Errno = 32

// lockFile opens path for writing while denying write access to everyone
// else (share mode FILE_SHARE_READ). A second waiter's open fails with a
// sharing violation; readers (ALREADY, stop) can still read the holder's
// pid. Windows closes the handle when the process dies, releasing the lock.
func lockFile(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return nil, errLocked
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

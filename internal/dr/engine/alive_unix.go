//go:build !windows

package engine

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process with this id exists. Signal 0 checks without sending one;
// EPERM means it exists but belongs to someone else.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

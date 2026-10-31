//go:build windows

package engine

import "os"

// processAlive reports whether a process with this id exists. On Windows FindProcess opens a handle
// and fails when there is no such process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}

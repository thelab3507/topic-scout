//go:build windows

package store

import "syscall"

// stillActive is the exit code Windows reports for a process that has not ended.
const stillActive = 259

// processAlive reports whether a process with this id is running.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == syscall.ERROR_ACCESS_DENIED // it exists but is not ours to inspect
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

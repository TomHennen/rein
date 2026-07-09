package srt

import "syscall"

// socketAFUnixSucceeds attempts to create an AF_UNIX socket and reports whether
// it SUCCEEDED. srt's seccomp filter blocks socket(AF_UNIX,…) creation (the
// guard that stops the agent reaching keyring/ssh-agent unix sockets), so a
// success here means seccomp is NOT in effect — a fail-open. On success the fd
// is closed immediately; the probe only needs the create/deny signal.
func socketAFUnixSucceeds() bool {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return false
	}
	_ = syscall.Close(fd)
	return true
}

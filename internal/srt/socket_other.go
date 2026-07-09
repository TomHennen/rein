//go:build !linux

package srt

// socketAFUnixSucceeds is a stub on non-Linux platforms. srt's Linux seccomp
// path does not apply here (macOS uses sandbox-exec — CP5 territory), so the
// probe treats the socket check as "not applicable" by reporting failure
// (i.e. as if the socket were blocked). The denyRead content check still runs.
func socketAFUnixSucceeds() bool { return false }

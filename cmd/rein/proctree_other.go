//go:build !linux && !darwin

package main

// procTreePlatform marker for unsupported platforms. detectWriteIntent
// surfaces this as "proc-tree fallback not implemented for <GOOS>" in
// the helper log; the user still gets correct behavior via the
// rein-git shim's REIN_GIT_OP env var (the primary signal).
const procTreePlatform = "unsupported"

// detectFromProcTree is a no-op on platforms other than linux and
// darwin. Cross-compilation stays green; runtime behavior is "no proc-
// tree signal available," which detectWriteIntent fails-closed on
// (returns read tier). The shim's REIN_GIT_OP env var remains the
// authoritative discriminator on these platforms.
func detectFromProcTree() (bool, string) {
	return false, ""
}

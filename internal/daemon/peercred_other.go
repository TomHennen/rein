//go:build !linux

package daemon

import (
	"fmt"
	"net"
)

// peerUID has no portable implementation off Linux yet (the BSDs/macOS use
// LOCAL_PEERCRED with a different API). Phase 1 pins the daemon to Linux
// (CLAUDE.md), so on other platforms we fail closed: an unverifiable peer is
// treated as a hard error, which handleConn turns into a dropped connection.
// This keeps the package compiling everywhere while never serving a peer it
// cannot authenticate.
func peerUID(conn *net.UnixConn) (uint32, error) {
	return 0, fmt.Errorf("peer-uid check unsupported on this platform (Linux-only)")
}

//go:build linux

package daemon

import (
	"fmt"
	"net"
	"syscall"
)

// peerUID returns the uid of the process on the far end of conn, read from the
// kernel via SO_PEERCRED. The credentials are stamped by the kernel at
// connect(2) time and cannot be forged by the peer, so this is the
// authoritative answer to "who connected".
//
// SO_PEERCRED / GetsockoptUcred are Linux-only, hence the build tag. A
// non-Linux build would need its own implementation (LOCAL_PEERCRED on the
// BSDs/macOS); until then the package only compiles the peer-uid check on
// Linux, which is the only supported daemon host in Phase 1.
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("syscall conn: %w", err)
	}
	var ucred *syscall.Ucred
	var sockErr error
	if ctrlErr := raw.Control(func(fd uintptr) {
		ucred, sockErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); ctrlErr != nil {
		return 0, fmt.Errorf("control fd: %w", ctrlErr)
	}
	if sockErr != nil {
		return 0, fmt.Errorf("getsockopt SO_PEERCRED: %w", sockErr)
	}
	return ucred.Uid, nil
}

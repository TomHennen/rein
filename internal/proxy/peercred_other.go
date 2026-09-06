//go:build !linux

package proxy

import (
	"errors"
	"net"
)

// peerUIDMatches has no /proc/net/tcp to consult off Linux. The sandboxed
// shape is Linux-only today (srt + bubblewrap); refuse every TCP peer rather
// than serve the external-proxy listener unchecked.
func peerUIDMatches(conn net.Conn, uid int) error {
	return errors.New("peer-uid check is not implemented on this platform; the external-proxy listener is Linux-only")
}

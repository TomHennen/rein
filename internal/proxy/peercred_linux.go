package proxy

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// peerUIDMatches refuses a loopback TCP peer not owned by uid. TCP has no
// SO_PEERCRED, so the owner comes from /proc/net/tcp and /proc/net/tcp6: the
// row whose local address is the PEER's, whose remote address is OUR listener,
// and whose state is ESTABLISHED (01). Exactly one row must match (TIME_WAIT
// leftovers of a reused tuple are state 06 and uid 0, so they never do); zero
// or several rows refuse. A dual-stack peer to 127.0.0.1 shows up in tcp6 as
// ::ffff:7f00:1, so both tables are always read.
func peerUIDMatches(conn net.Conn, uid int) error {
	peer, ok := conn.RemoteAddr().(*net.TCPAddr)
	local, ok2 := conn.LocalAddr().(*net.TCPAddr)
	if !ok || !ok2 {
		return errors.New("not a TCP connection")
	}
	return peerUIDFromProc(peer, local, uid, "/proc/net/tcp", "/proc/net/tcp6")
}

func peerUIDFromProc(peer, local *net.TCPAddr, uid int, tables ...string) error {
	var owner []int
	for _, t := range tables {
		f, err := os.Open(t)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 8 || fields[3] != "01" {
				continue
			}
			lAddr, lPort, err := decodeProcAddr(fields[1])
			if err != nil {
				continue
			}
			rAddr, rPort, err := decodeProcAddr(fields[2])
			if err != nil {
				continue
			}
			if lPort != peer.Port || rPort != local.Port || !lAddr.Equal(peer.IP) || !rAddr.Equal(local.IP) {
				continue
			}
			u, err := strconv.Atoi(fields[7])
			if err != nil {
				continue
			}
			owner = append(owner, u)
		}
		f.Close()
	}
	switch {
	case len(owner) == 0:
		return fmt.Errorf("no ESTABLISHED socket row for peer %s", peer)
	case len(owner) > 1:
		return fmt.Errorf("ambiguous socket rows (%d) for peer %s", len(owner), peer)
	case owner[0] != uid:
		return fmt.Errorf("peer %s is owned by uid %d, not %d", peer, owner[0], uid)
	}
	return nil
}

// decodeProcAddr parses "HEXADDR:HEXPORT" from /proc/net/tcp{,6}: the address
// is one (IPv4) or four (IPv6) little-endian 32-bit words.
func decodeProcAddr(s string) (net.IP, int, error) {
	addrHex, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, errors.New("no port")
	}
	port, err := strconv.ParseInt(portHex, 16, 32)
	if err != nil {
		return nil, 0, err
	}
	raw, err := hex.DecodeString(addrHex)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil, 0, errors.New("bad address")
	}
	ip := make(net.IP, len(raw))
	for w := 0; w < len(raw); w += 4 {
		ip[w], ip[w+1], ip[w+2], ip[w+3] = raw[w+3], raw[w+2], raw[w+1], raw[w]
	}
	return ip, int(port), nil
}

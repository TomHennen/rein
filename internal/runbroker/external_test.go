package runbroker

import (
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/proxy"
)

// TestStart_ExternalProxyShape: with the #185 trio the host binds a loopback
// port and reports it; a partial trio fails closed; the mitm shape reports 0.
func TestStart_ExternalProxyShape(t *testing.T) {
	pol, err := proxy.NewEgressPolicy(proxy.EgressRestricted, []string{"pypi.org"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Start(Config{
		SessionID:        "s",
		SocketPath:       filepath.Join(t.TempDir(), "p.sock"),
		Logger:           log.New(io.Discard, "", 0),
		CAKeystore:       keystore.NewFileKeystore(t.TempDir()),
		allowAutoApprove: true,
		Egress:           pol,
		ProxySecret:      "sec",
		ProbeNonce:       "nonce",
	})
	if err != nil {
		t.Fatalf("Start external: %v", err)
	}
	defer h.Close()
	if h.ProxyPort() == 0 {
		t.Error("external shape reported no proxy port")
	}

	_, err = Start(Config{
		SessionID:        "s",
		SocketPath:       filepath.Join(t.TempDir(), "p.sock"),
		Logger:           log.New(io.Discard, "", 0),
		CAKeystore:       keystore.NewFileKeystore(t.TempDir()),
		allowAutoApprove: true,
		ProxySecret:      "sec", // no policy, no nonce
	})
	if err == nil || !strings.Contains(err.Error(), "must all be set") {
		t.Errorf("partial trio: err = %v, want a fail-closed error", err)
	}

	m, err := Start(Config{
		SessionID:        "s",
		SocketPath:       filepath.Join(t.TempDir(), "p.sock"),
		Logger:           log.New(io.Discard, "", 0),
		CAKeystore:       keystore.NewFileKeystore(t.TempDir()),
		allowAutoApprove: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.ProxyPort() != 0 {
		t.Errorf("mitm shape reported a proxy port %d", m.ProxyPort())
	}
}

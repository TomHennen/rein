package appsetup

import (
	"bytes"
	"strings"
	"testing"
)

func TestDetectHeadless(t *testing.T) {
	// detectHeadless early-returns non-headless when a browser override
	// is installed (test hook); ensure it's nil for these cases.
	orig := browserOpenerOverride
	browserOpenerOverride = nil
	t.Cleanup(func() { browserOpenerOverride = orig })

	tests := []struct {
		name         string
		sshConn      string
		display      string
		wayland      string
		user         string
		wantHeadless bool
		wantHost     string
		wantUser     string
	}{
		{name: "ssh no display", sshConn: "192.168.64.1 51454 192.168.64.3 22", user: "admin", wantHeadless: true, wantHost: "192.168.64.3", wantUser: "admin"},
		{name: "ssh with X11 forwarding", sshConn: "192.168.64.1 51454 192.168.64.3 22", display: ":10.0", wantHeadless: false},
		{name: "ssh with wayland", sshConn: "1 2 3 4", wayland: "wayland-0", wantHeadless: false},
		{name: "local no ssh", wantHeadless: false},
		{name: "ssh malformed conn", sshConn: "garbage", user: "bob", wantHeadless: true, wantHost: "", wantUser: "bob"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SSH_CONNECTION", tt.sshConn)
			t.Setenv("DISPLAY", tt.display)
			t.Setenv("WAYLAND_DISPLAY", tt.wayland)
			t.Setenv("USER", tt.user)
			hi := detectHeadless()
			if hi.headless != tt.wantHeadless {
				t.Fatalf("headless=%v want %v", hi.headless, tt.wantHeadless)
			}
			if tt.wantHeadless {
				if hi.sshHost != tt.wantHost {
					t.Errorf("sshHost=%q want %q", hi.sshHost, tt.wantHost)
				}
				if hi.sshUser != tt.wantUser {
					t.Errorf("sshUser=%q want %q", hi.sshUser, tt.wantUser)
				}
			}
		})
	}
}

func TestPrintBrowserInstructions_Desktop(t *testing.T) {
	var b bytes.Buffer
	printBrowserInstructions(&b, 41234, false, headlessInfo{})
	out := b.String()
	if !strings.Contains(out, "http://127.0.0.1:41234/") {
		t.Errorf("missing URL in:\n%s", out)
	}
	if strings.Contains(out, "ssh -L") {
		t.Errorf("desktop output should not show ssh -L:\n%s", out)
	}
}

func TestPrintBrowserInstructions_Headless(t *testing.T) {
	var b bytes.Buffer
	printBrowserInstructions(&b, 41234, false, headlessInfo{headless: true, sshUser: "admin", sshHost: "192.168.64.3"})
	out := b.String()
	for _, want := range []string{
		"ssh -L 41234:127.0.0.1:41234 admin@192.168.64.3",
		"http://127.0.0.1:41234/",
		"rein init --port 41234", // tip shown when port is not already pinned
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestPrintBrowserInstructions_HeadlessPinnedHidesTip(t *testing.T) {
	var b bytes.Buffer
	printBrowserInstructions(&b, 41234, true, headlessInfo{headless: true, sshHost: "host-only"})
	out := b.String()
	if strings.Contains(out, "--port") {
		t.Errorf("an already-pinned port should not show the --port tip:\n%s", out)
	}
	// Host known but user unknown → host-only target, no leading "@".
	if !strings.Contains(out, "ssh -L 41234:127.0.0.1:41234 host-only") {
		t.Errorf("expected host-only ssh target in:\n%s", out)
	}
}

func TestPrintBrowserInstructions_HeadlessUnknownTarget(t *testing.T) {
	var b bytes.Buffer
	printBrowserInstructions(&b, 5000, false, headlessInfo{headless: true})
	out := b.String()
	if !strings.Contains(out, "<user>@<host>") {
		t.Errorf("expected placeholder target when ssh info unknown:\n%s", out)
	}
}

func TestBindLoopback_PinnedPort(t *testing.T) {
	// Grab a free ephemeral port, release it, then prove bindLoopback can
	// pin that exact port (the --port path). SO_REUSEADDR makes the
	// immediate rebind reliable.
	ln, port, err := bindLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()

	ln2, got, err := bindLoopback(port)
	if err != nil {
		t.Fatalf("pin port %d: %v", port, err)
	}
	defer ln2.Close()
	if got != port {
		t.Errorf("bound port %d, want pinned %d", got, port)
	}
}

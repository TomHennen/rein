package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckPlacement(t *testing.T) {
	tmp := t.TempDir()
	bind := filepath.Join(tmp, "workdir")
	if err := os.MkdirAll(bind, 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		socket    string
		forbidden []string
		wantErr   bool
	}{
		{"outside all", filepath.Join(tmp, "run", "proxy.sock"), []string{bind}, false},
		{"directly inside", filepath.Join(bind, "proxy.sock"), []string{bind}, true},
		{"nested inside", filepath.Join(bind, "a", "b", "proxy.sock"), []string{bind}, true},
		{"equals forbidden dir", bind, []string{bind}, true},
		{"sibling prefix not inside", filepath.Join(tmp, "workdir-other", "s.sock"), []string{bind}, false},
		{"no forbidden dirs", filepath.Join(bind, "s.sock"), nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckPlacement(tt.socket, tt.forbidden)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckPlacement(%q, %v) err = %v, wantErr %v", tt.socket, tt.forbidden, err, tt.wantErr)
			}
		})
	}
}

func TestCheckPlacementSymlinkedSocketDir(t *testing.T) {
	tmp := t.TempDir()
	bind := filepath.Join(tmp, "workdir")
	if err := os.MkdirAll(bind, 0o700); err != nil {
		t.Fatal(err)
	}
	// A symlink OUTSIDE the bind that points INTO it must still be caught: the
	// socket resolves to a path under the bind-mount.
	link := filepath.Join(tmp, "sneaky")
	if err := os.Symlink(bind, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckPlacement(filepath.Join(link, "proxy.sock"), []string{bind}); err == nil {
		t.Fatalf("symlinked socket dir into a forbidden path was allowed")
	}
}

func TestListenSocketPerms(t *testing.T) {
	tmp := t.TempDir()
	sock := filepath.Join(tmp, "run", "proxy.sock")
	ln, err := Listen(sock, []string{filepath.Join(tmp, "work")})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	di, err := os.Stat(filepath.Dir(sock))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("socket dir mode = %o, want 0700", di.Mode().Perm())
	}
	si, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if si.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %o, want 0600", si.Mode().Perm())
	}
	if si.Mode()&os.ModeSocket == 0 {
		t.Errorf("not a socket")
	}
}

func TestListenRejectsForbiddenPlacement(t *testing.T) {
	tmp := t.TempDir()
	bind := filepath.Join(tmp, "work")
	if err := os.MkdirAll(bind, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(bind, "proxy.sock"), []string{bind}); err == nil {
		t.Fatalf("Listen allowed a socket inside a forbidden bind-mount")
	}
}

func TestListenRemovesStaleSocket(t *testing.T) {
	tmp := t.TempDir()
	sock := filepath.Join(tmp, "proxy.sock")
	// Simulate a leftover socket file from a crashed run.
	if err := os.WriteFile(sock, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(sock, nil)
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	ln.Close()
}

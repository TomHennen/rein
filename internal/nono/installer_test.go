package nono

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeTarball builds a gzip'd tar containing one file at `name` with `content`.
func makeTarball(t *testing.T, name string, content []byte, typeflag byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: typeflag}
	if typeflag == tar.TypeSymlink {
		hdr.Linkname = string(content)
		hdr.Size = 0
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if typeflag == tar.TypeReg {
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	return buf.Bytes()
}

func hexHash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// getterFor returns an HTTPGet that serves `body` with 200 for any URL, and
// records whether it was called.
func getterFor(body []byte, called *bool) func(string) (*http.Response, error) {
	return func(string) (*http.Response, error) {
		if called != nil {
			*called = true
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	}
}

// withPin registers a temporary pin for version/platform and restores the map
// after the test.
func withPin(t *testing.T, version, platform string, d digests) {
	t.Helper()
	orig := pinnedDigests
	// shallow copy so we do not mutate the vendored map across tests
	cp := map[string]map[string]digests{}
	for v, m := range orig {
		inner := map[string]digests{}
		for p, dg := range m {
			inner[p] = dg
		}
		cp[v] = inner
	}
	if cp[version] == nil {
		cp[version] = map[string]digests{}
	}
	cp[version][platform] = d
	pinnedDigests = cp
	t.Cleanup(func() { pinnedDigests = orig })
}

const testVer = "9.9.9-test"
const testPlat = "x86_64-unknown-linux-gnu"

func TestInstall_HappyPath(t *testing.T) {
	bin := []byte("this-is-the-nono-binary\x00\x01\x02")
	tgz := makeTarball(t, "nono", bin, tar.TypeReg)
	withPin(t, testVer, testPlat, digests{Tarball: hexHash(tgz), Binary: hexHash(bin)})

	dest := t.TempDir()
	var called bool
	path, err := Install(InstallParams{
		Version: testVer, Platform: testPlat, DestDir: dest,
		HTTPGet: getterFor(tgz, &called),
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !called {
		t.Fatal("HTTPGet was not called")
	}
	want := filepath.Join(dest, "nono")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed: %v", err)
	}
	if !bytes.Equal(got, bin) {
		t.Fatal("installed bytes differ from source binary")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}
	// VerifyInstalled must agree with the same pin.
	if err := VerifyInstalled(path, testVer, testPlat); err != nil {
		t.Fatalf("VerifyInstalled: %v", err)
	}
}

func TestInstall_TarballDigestMismatch_OneByteFlip(t *testing.T) {
	bin := []byte("nono-binary-bytes")
	tgz := makeTarball(t, "nono", bin, tar.TypeReg)
	// Pin the good digest, then serve a tarball with one byte flipped.
	withPin(t, testVer, testPlat, digests{Tarball: hexHash(tgz), Binary: hexHash(bin)})

	tampered := append([]byte(nil), tgz...)
	tampered[len(tampered)/2] ^= 0x01

	dest := t.TempDir()
	_, err := Install(InstallParams{
		Version: testVer, Platform: testPlat, DestDir: dest,
		HTTPGet: getterFor(tampered, nil),
	})
	if err == nil {
		t.Fatal("expected digest mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "tarball digest mismatch") {
		t.Fatalf("error = %v, want tarball digest mismatch", err)
	}
	assertNoPartial(t, dest)
}

func TestInstall_BinaryDigestMismatch(t *testing.T) {
	bin := []byte("nono-binary-bytes")
	tgz := makeTarball(t, "nono", bin, tar.TypeReg)
	// Tarball digest correct, but the vendored BINARY digest is wrong -> refuse.
	withPin(t, testVer, testPlat, digests{Tarball: hexHash(tgz), Binary: hexHash([]byte("something-else"))})

	dest := t.TempDir()
	_, err := Install(InstallParams{
		Version: testVer, Platform: testPlat, DestDir: dest,
		HTTPGet: getterFor(tgz, nil),
	})
	if err == nil || !strings.Contains(err.Error(), "binary digest mismatch") {
		t.Fatalf("error = %v, want binary digest mismatch", err)
	}
	assertNoPartial(t, dest)
}

func TestInstall_TruncatedDownload(t *testing.T) {
	bin := []byte("nono-binary-bytes")
	tgz := makeTarball(t, "nono", bin, tar.TypeReg)
	withPin(t, testVer, testPlat, digests{Tarball: hexHash(tgz), Binary: hexHash(bin)})

	// Truncate the tarball: gzip/tar read fails OR digest fails — either way,
	// fail-closed with no file placed. (Truncation before the digest check is
	// caught by the tarball digest; a truncation that still hashes differently
	// is the same path.)
	truncated := tgz[:len(tgz)-5]
	dest := t.TempDir()
	_, err := Install(InstallParams{
		Version: testVer, Platform: testPlat, DestDir: dest,
		HTTPGet: getterFor(truncated, nil),
	})
	if err == nil {
		t.Fatal("expected error on truncated download, got nil")
	}
	assertNoPartial(t, dest)
}

func TestInstall_MissingPin_NoNetwork(t *testing.T) {
	dest := t.TempDir()
	var called bool
	_, err := Install(InstallParams{
		Version: "0.0.0-unpinned", Platform: testPlat, DestDir: dest,
		HTTPGet: getterFor([]byte("should never be read"), &called),
	})
	if err == nil || !strings.Contains(err.Error(), "no vendored digest") {
		t.Fatalf("error = %v, want no vendored digest", err)
	}
	if called {
		t.Fatal("HTTPGet was called for an unpinned version; must fail before any network")
	}
	assertNoPartial(t, dest)
}

func TestInstall_TarPathTraversalRejected(t *testing.T) {
	// A tarball whose member escapes the tree must be rejected even if the
	// tarball digest matched.
	tgz := makeTarball(t, "../../evil", []byte("x"), tar.TypeReg)
	withPin(t, testVer, testPlat, digests{Tarball: hexHash(tgz), Binary: hexHash([]byte("x"))})
	dest := t.TempDir()
	_, err := Install(InstallParams{
		Version: testVer, Platform: testPlat, DestDir: dest,
		HTTPGet: getterFor(tgz, nil),
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe member path") {
		t.Fatalf("error = %v, want unsafe member path", err)
	}
	assertNoPartial(t, dest)
}

func TestInstall_NoNonoMember(t *testing.T) {
	tgz := makeTarball(t, "README", []byte("hi"), tar.TypeReg)
	withPin(t, testVer, testPlat, digests{Tarball: hexHash(tgz), Binary: hexHash([]byte("hi"))})
	dest := t.TempDir()
	_, err := Install(InstallParams{
		Version: testVer, Platform: testPlat, DestDir: dest,
		HTTPGet: getterFor(tgz, nil),
	})
	if err == nil || !strings.Contains(err.Error(), "no `nono` binary member") {
		t.Fatalf("error = %v, want no nono member", err)
	}
	assertNoPartial(t, dest)
}

func TestInstall_NonRegularNonoRejected(t *testing.T) {
	// A `nono` symlink member (would-be link attack) must be rejected.
	tgz := makeTarball(t, "nono", []byte("/etc/passwd"), tar.TypeSymlink)
	withPin(t, testVer, testPlat, digests{Tarball: hexHash(tgz), Binary: hexHash([]byte("x"))})
	dest := t.TempDir()
	_, err := Install(InstallParams{
		Version: testVer, Platform: testPlat, DestDir: dest,
		HTTPGet: getterFor(tgz, nil),
	})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error = %v, want not a regular file", err)
	}
	assertNoPartial(t, dest)
}

func TestInstall_HTTPNon200(t *testing.T) {
	withPin(t, testVer, testPlat, digests{Tarball: "aa", Binary: "bb"})
	dest := t.TempDir()
	get := func(string) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound, Status: "404 Not Found",
			Body: io.NopCloser(strings.NewReader("nope")),
		}, nil
	}
	_, err := Install(InstallParams{Version: testVer, Platform: testPlat, DestDir: dest, HTTPGet: get})
	if err == nil || !strings.Contains(err.Error(), "unexpected status") {
		t.Fatalf("error = %v, want unexpected status", err)
	}
	assertNoPartial(t, dest)
}

func TestVerifyInstalled_Mismatch(t *testing.T) {
	bin := []byte("real-binary")
	withPin(t, testVer, testPlat, digests{Tarball: "aa", Binary: hexHash(bin)})
	dir := t.TempDir()
	p := filepath.Join(dir, "nono")
	// Write DIFFERENT bytes than the pin -> mismatch.
	if err := os.WriteFile(p, []byte("tampered-on-disk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyInstalled(p, testVer, testPlat); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error = %v, want digest mismatch", err)
	}
}

func TestVerifyInstalled_MissingPin(t *testing.T) {
	if err := VerifyInstalled("/nonexistent", "0.0.0-unpinned", testPlat); err == nil ||
		!strings.Contains(err.Error(), "no vendored digest") {
		t.Fatalf("error = %v, want no vendored digest", err)
	}
}

func TestReleaseURLAndAsset(t *testing.T) {
	asset := releaseAsset("0.68.0", "x86_64-unknown-linux-gnu")
	if asset != "nono-v0.68.0-x86_64-unknown-linux-gnu.tar.gz" {
		t.Fatalf("asset = %q", asset)
	}
	url := releaseURL("0.68.0", "x86_64-unknown-linux-gnu")
	want := "https://github.com/nolabs-ai/nono/releases/download/v0.68.0/nono-v0.68.0-x86_64-unknown-linux-gnu.tar.gz"
	if url != want {
		t.Fatalf("url = %q, want %q", url, want)
	}
}

func TestPinnedDigestsPresent(t *testing.T) {
	// The vendored pin for the PinnedVersion must exist for both Linux targets.
	for _, plat := range []string{"x86_64-unknown-linux-gnu", "aarch64-unknown-linux-gnu"} {
		d, err := pinFor(PinnedVersion, plat)
		if err != nil {
			t.Fatalf("pinFor(%s,%s): %v", PinnedVersion, plat, err)
		}
		if len(d.Tarball) != 64 || len(d.Binary) != 64 {
			t.Fatalf("%s: digests not 64-hex: %+v", plat, d)
		}
	}
}

// assertNoPartial fails if any `nono` binary or leftover temp file survives in
// dest after a failed Install (fail-closed: no partial writes).
func assertNoPartial(t *testing.T, dest string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dest, "nono")); err == nil {
		t.Fatal("partial `nono` binary left behind after failed install")
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return // dir may not exist; that's fine
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".nono-") {
			t.Fatalf("leftover temp file after failed install: %s", e.Name())
		}
	}
}

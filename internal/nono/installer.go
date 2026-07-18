// Package nono handles the verified download/install of the nono sandbox
// runtime, the profile generator, and doctor health checks (design
// docs/design-nono-pivot.md §2). This file is the verified installer.
//
// Trust model: rein pins the nono VERSION and the exact SHA-256 of the release
// tarball AND the inner binary IN THIS SOURCE FILE (the trust floor, covered by
// rein's own SLSA/wrangle supply chain). The installer downloads the pinned
// tarball over HTTPS, hashes it, and compares against the vendored constant.
// It NEVER trusts the release's own SHA256SUMS.txt: fetching that over the same
// TLS channel as the binary is not independent trust — a channel compromise
// serves both. A fully compromised nono release (swapped tarball + checksums +
// forged attestations) still cannot pass this pin without editing rein's source.
//
// nono 0.68.0 releases DO carry GitHub build-provenance attestations
// (sigstore-backed, via actions/attest — verified empirically 2026-07-18).
// Attestation verification is a documented, additive UPGRADE (gate before the
// digest; the vendored digest stays as belt-and-suspenders) tracked as a
// follow-up — NOT built here, because a vendored pin is already the strongest
// trust root for a pinned version and adding a sigstore-verify dependency is a
// stop-and-ask supply-chain decision (CLAUDE.md #5). See the task handoff.
package nono

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/TomHennen/rein/internal/config"
)

// PinnedVersion is the nono release rein's profile schema + egress semantics are
// verified against (the spike ran on exactly this build). A mismatch means the
// profile shape / behavior may differ; Install refuses an unpinned version and
// doctor warns (mirrors srt.PinnedVersion policy). To bump: pick the new tag,
// download its tarballs, recompute digests out-of-band (see pinnedDigests), add
// a row, then re-run the containment prober against the new build.
const PinnedVersion = "0.68.0"

// releaseAsset is the tarball name nono publishes per platform, e.g.
// nono-v0.68.0-x86_64-unknown-linux-gnu.tar.gz. The tag carries a leading "v".
func releaseAsset(version, platform string) string {
	return fmt.Sprintf("nono-v%s-%s.tar.gz", version, platform)
}

// releaseURL is the GitHub release download URL for a platform's tarball.
func releaseURL(version, platform string) string {
	return fmt.Sprintf(
		"https://github.com/nolabs-ai/nono/releases/download/v%s/%s",
		version, releaseAsset(version, platform))
}

// digests holds the two vendored SHA-256 hashes for one (version, platform):
// Tarball gates Install's download; Binary is what VerifyInstalled re-hashes on
// disk. They are DIFFERENT files — re-hashing the on-disk binary against a
// tarball digest never matches, so both must be vendored.
type digests struct{ Tarball, Binary string }

// pinnedDigests maps version -> platform (rustc target triple nono ships) ->
// digests. VENDORED here in rein source; this map — never the fetched
// SHA256SUMS.txt — is the trust floor.
//
// Recorded 2026-07-18 for v0.68.0 (Linux only; macOS/Seatbelt is an open gate,
// design §8). Provenance of these hashes: downloaded the release tarballs,
// recomputed SHA-256 locally, cross-checked against the release SHA256SUMS.txt
// (which independently matched), then extracted the inner `nono` binary and
// hashed it. All four values below were verified this way.
var pinnedDigests = map[string]map[string]digests{
	"0.68.0": {
		"x86_64-unknown-linux-gnu": {
			Tarball: "7a70fbf554233fd5f9673acdb806534b5140137460487d0d86af49ad286c9faa",
			Binary:  "186ddc2ffa894d22c3db1feadefb9fe3e6ac092017f14fcb1e0a8ba76c137832",
		},
		"aarch64-unknown-linux-gnu": {
			Tarball: "0bb377346c5eb6a2c72a18af3b2d5637135e83bef3e77c77293cfb14d667d7a3",
			Binary:  "324f308ff42a46d0e9a420e2a44748c2074b7d3d93f07e67c24212d01cb420be",
		},
	},
}

// pinFor returns the vendored digests for (version, platform), or an error
// naming the fix. Fail-closed: a missing pin NEVER falls through to trusting a
// fetched checksum.
func pinFor(version, platform string) (digests, error) {
	byPlat, ok := pinnedDigests[version]
	if !ok {
		return digests{}, fmt.Errorf("no vendored digest for nono %s; bump internal/nono.pinnedDigests", version)
	}
	d, ok := byPlat[platform]
	if !ok {
		return digests{}, fmt.Errorf("no vendored digest for nono %s on platform %q; bump internal/nono.pinnedDigests", version, platform)
	}
	return d, nil
}

// DetectPlatform maps the host GOOS/GOARCH to the nono rustc target triple.
// Linux-scoped (design §8): everything else is a clear unsupported error.
func DetectPlatform() (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("nono install unsupported on %s; rein-on-nono is Linux-only (see design §8)", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64-unknown-linux-gnu", nil
	case "arm64":
		return "aarch64-unknown-linux-gnu", nil
	default:
		return "", fmt.Errorf("nono install unsupported on linux/%s; supported: amd64, arm64", runtime.GOARCH)
	}
}

// managedNonoDir is the rein-managed directory the nono binary lives in:
// <ConfigDir>/nono/bin. nono is invoked by ABSOLUTE path from here, never via
// exec.LookPath, so the agent's $PATH cannot shadow it (design §2.1.1).
func managedNonoDir() (string, error) {
	cd, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cd, "nono", "bin"), nil
}

// ManagedNonoPath is the only nono path rein exec's: <ConfigDir>/nono/bin/nono.
func ManagedNonoPath() (string, error) {
	dir, err := managedNonoDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "nono"), nil
}

// maxTarballBytes caps the download to guard against a hostile/oversized
// response inflating memory before the digest check. The real 0.68.0 tarballs
// are ~11 MiB; 128 MiB is generous headroom that still bounds abuse.
const maxTarballBytes = 128 << 20

// InstallParams configures Install. Zero values pick verified defaults.
type InstallParams struct {
	Version  string                                   // default PinnedVersion
	Platform string                                   // default DetectPlatform()
	DestDir  string                                   // default managedNonoDir()
	HTTPGet  func(url string) (*http.Response, error) // default http.Get; injectable for tests
}

// Install downloads the pinned nono release tarball, verifies its SHA-256
// against the vendored pin, extracts the inner `nono` binary, verifies THAT
// against the vendored binary pin, and atomically places it at DestDir/nono
// (0o755). Returns the absolute installed path.
//
// Fail-closed on ANY failure — unpinned version/platform, digest mismatch,
// oversized/truncated download, tar path-traversal, missing binary member: no
// file is placed, temp files are removed, and an unverified binary is NEVER
// installed. There is no fallback to unverified download.
func Install(p InstallParams) (string, error) {
	version := p.Version
	if version == "" {
		version = PinnedVersion
	}
	platform := p.Platform
	if platform == "" {
		var err error
		if platform, err = DetectPlatform(); err != nil {
			return "", err
		}
	}
	destDir := p.DestDir
	if destDir == "" {
		var err error
		if destDir, err = managedNonoDir(); err != nil {
			return "", err
		}
	}
	httpGet := p.HTTPGet
	if httpGet == nil {
		httpGet = http.Get
	}

	// Resolve the pin FIRST: no pin means no install, no network.
	pin, err := pinFor(version, platform)
	if err != nil {
		return "", err
	}

	// Download the tarball fully into memory (bounded), then hash. We verify
	// before touching disk so unverified bytes are never extracted.
	tarball, err := download(httpGet, releaseURL(version, platform))
	if err != nil {
		return "", err
	}
	if got := sha256Hex(tarball); got != pin.Tarball {
		return "", fmt.Errorf("nono tarball digest mismatch for %s/%s: got %s, want %s (refusing install)", version, platform, got, pin.Tarball)
	}

	// Extract the inner binary and verify it against the binary pin.
	binBytes, err := extractNonoBinary(tarball)
	if err != nil {
		return "", err
	}
	if got := sha256Hex(binBytes); got != pin.Binary {
		return "", fmt.Errorf("nono binary digest mismatch for %s/%s: got %s, want %s (refusing install)", version, platform, got, pin.Binary)
	}

	// Atomic place: temp file in DestDir -> fsync -> rename. Any error unlinks
	// the temp so no partial binary survives.
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("create nono dir: %w", err)
	}
	dest := filepath.Join(destDir, "nono")
	if err := atomicWrite(dest, binBytes, 0o755); err != nil {
		return "", err
	}
	return dest, nil
}

// VerifyInstalled recomputes the on-disk binary's SHA-256 and compares it to the
// vendored BINARY pin for (version, platform). Used by doctor and the launch
// path. Fail-closed: any read error, unpinned target, or mismatch is an error.
func VerifyInstalled(path, version, platform string) error {
	pin, err := pinFor(version, platform)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open installed nono: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash installed nono: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != pin.Binary {
		return fmt.Errorf("installed nono digest mismatch at %s: got %s, want %s (reinstall)", path, got, pin.Binary)
	}
	return nil
}

// download fetches url and returns its body, bounded by maxTarballBytes. A
// non-200 status, transport error, or oversized body is fail-closed.
func download(httpGet func(string) (*http.Response, error), url string) ([]byte, error) {
	resp, err := httpGet(url)
	if err != nil {
		return nil, fmt.Errorf("download nono: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download nono: unexpected status %s from %s", resp.Status, url)
	}
	// LimitReader+1 so an over-cap body is detected rather than silently truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTarballBytes+1))
	if err != nil {
		return nil, fmt.Errorf("download nono: %w", err)
	}
	if len(body) > maxTarballBytes {
		return nil, fmt.Errorf("download nono: body exceeds %d bytes", maxTarballBytes)
	}
	return body, nil
}

// extractNonoBinary reads the gzip'd tarball and returns the bytes of the single
// `nono` file at the archive root. Guards path-traversal (rejects absolute or
// ".." members) and refuses symlinks/hardlinks/dirs-as-the-binary. A missing or
// duplicate `nono` member is an error.
func extractNonoBinary(tarball []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, fmt.Errorf("open nono tarball gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var found []byte
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read nono tarball: %w", err)
		}
		name := hdr.Name
		// Reject path-traversal / absolute members outright (defense in depth:
		// we only ever read one member's bytes, never write tar paths).
		if filepath.IsAbs(name) || strings.Contains(name, "..") {
			return nil, fmt.Errorf("nono tarball has unsafe member path %q", name)
		}
		if filepath.Base(filepath.Clean(name)) != "nono" {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("nono tarball member %q is not a regular file (type %d)", name, hdr.Typeflag)
		}
		if found != nil {
			return nil, fmt.Errorf("nono tarball has more than one `nono` member")
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxTarballBytes+1))
		if err != nil {
			return nil, fmt.Errorf("extract nono binary: %w", err)
		}
		if int64(len(b)) > maxTarballBytes {
			return nil, fmt.Errorf("extract nono binary: member exceeds %d bytes", maxTarballBytes)
		}
		found = b
	}
	if found == nil {
		return nil, errors.New("nono tarball contains no `nono` binary member")
	}
	return found, nil
}

// atomicWrite writes data to a temp file in the same dir as dest, fsyncs it,
// then renames over dest. On any error the temp file is removed so no partial
// output remains.
func atomicWrite(dest string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".nono-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp nono: %w", err)
	}
	tmpName := tmp.Name()
	// Remove the temp on any failure; harmless no-op after a successful rename.
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp nono: %w", err)
	}
	if err = tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp nono: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp nono: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp nono: %w", err)
	}
	if err = os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("place nono: %w", err)
	}
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

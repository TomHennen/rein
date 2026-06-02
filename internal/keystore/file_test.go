package keystore

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func genTestPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	body := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: body})
}

func TestFileKeystore_SetGet_Roundtrip(t *testing.T) {
	k := NewFileKeystore(t.TempDir())
	want := []byte("hello-keystore")
	if err := k.Set("primary", want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := k.Get("primary")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("get = %q, want %q", got, want)
	}
}

func TestFileKeystore_Set_OverwriteExisting(t *testing.T) {
	k := NewFileKeystore(t.TempDir())
	if err := k.Set("primary", []byte("v1")); err != nil {
		t.Fatalf("set v1: %v", err)
	}
	if err := k.Set("primary", []byte("v2-longer")); err != nil {
		t.Fatalf("set v2: %v", err)
	}
	got, err := k.Get("primary")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != "v2-longer" {
		t.Errorf("got %q after overwrite", got)
	}
	info, err := os.Stat(k.path("primary"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode after overwrite = %#o, want 0600", mode)
	}
}

func TestFileKeystore_Set_Mode0600(t *testing.T) {
	k := NewFileKeystore(t.TempDir())
	if err := k.Set("audit", []byte("x")); err != nil {
		t.Fatalf("set: %v", err)
	}
	info, err := os.Stat(k.path("audit"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %#o, want 0600", mode)
	}
}

func TestFileKeystore_Get_RefusesLooseMode(t *testing.T) {
	k := NewFileKeystore(t.TempDir())
	if err := k.Set("primary", []byte("secret")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := os.Chmod(k.path("primary"), 0o644); err != nil {
		t.Fatalf("chmod loose: %v", err)
	}
	_, err := k.Get("primary")
	if err == nil {
		t.Fatal("expected loose-mode refusal, got nil")
	}
	if !strings.Contains(err.Error(), "group/other") {
		t.Errorf("error %q should mention group/other bits", err.Error())
	}
}

func TestFileKeystore_Get_NotFound(t *testing.T) {
	k := NewFileKeystore(t.TempDir())
	_, err := k.Get("absent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestFileKeystore_Fingerprint_NotFound(t *testing.T) {
	k := NewFileKeystore(t.TempDir())
	_, err := k.Fingerprint("absent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestFileKeystore_Delete_Idempotent(t *testing.T) {
	k := NewFileKeystore(t.TempDir())
	if err := k.Delete("absent"); err != nil {
		t.Errorf("delete missing should be nil, got %v", err)
	}
	if err := k.Set("primary", []byte("x")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := k.Delete("primary"); err != nil {
		t.Errorf("delete: %v", err)
	}
	if _, err := k.Get("primary"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}

func TestFileKeystore_Fingerprint_StableAcrossWrites(t *testing.T) {
	// Write the same PEM twice, fingerprint each time; both must match.
	// Then write a *different* PEM and confirm the fingerprint changes.
	pemBytes := genTestPEM(t)
	k := NewFileKeystore(t.TempDir())
	if err := k.Set("primary", pemBytes); err != nil {
		t.Fatalf("set: %v", err)
	}
	fp1, err := k.Fingerprint("primary")
	if err != nil {
		t.Fatalf("fingerprint #1: %v", err)
	}
	if err := k.Set("primary", pemBytes); err != nil {
		t.Fatalf("set (same): %v", err)
	}
	fp2, err := k.Fingerprint("primary")
	if err != nil {
		t.Fatalf("fingerprint #2: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint differs across identical writes: %q vs %q", fp1, fp2)
	}
	// Sanity-check: the format is base64-encoded SHA-256 of the PKIX
	// public key. Decode + re-derive.
	block, _ := pem.Decode(pemBytes)
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	pkix, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal PKIX: %v", err)
	}
	sum := sha256.Sum256(pkix)
	want := base64.StdEncoding.EncodeToString(sum[:])
	if fp1 != want {
		t.Errorf("fingerprint = %q, derived want = %q", fp1, want)
	}

	// A different PEM produces a different fingerprint.
	other := genTestPEM(t)
	if err := k.Set("primary", other); err != nil {
		t.Fatalf("set other: %v", err)
	}
	fp3, err := k.Fingerprint("primary")
	if err != nil {
		t.Fatalf("fingerprint other: %v", err)
	}
	if fp3 == fp1 {
		t.Errorf("fingerprint did not change after key rotation")
	}
}

func TestFileKeystore_Fingerprint_NotPEM(t *testing.T) {
	k := NewFileKeystore(t.TempDir())
	if err := k.Set("primary", []byte("not a pem at all")); err != nil {
		t.Fatalf("set: %v", err)
	}
	_, err := k.Fingerprint("primary")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "PEM") {
		t.Errorf("error %q should mention PEM", err.Error())
	}
}

func TestVerifyOwnership_RejectsLooseMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "loose")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := verifyOwnership(p, info, os.Getuid()); err == nil {
		t.Error("expected loose-mode refusal")
	}
}

func TestVerifyOwnership_AcceptsTightMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tight")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := verifyOwnership(p, info, os.Getuid()); err != nil {
		t.Errorf("unexpected: %v", err)
	}
}

func TestVerifyOwnership_RejectsWrongUid(t *testing.T) {
	// The function takes wantUID as a parameter so we can drive the
	// "other uid" path without needing root to chown a real file.
	dir := t.TempDir()
	p := filepath.Join(dir, "wrong")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	wrong := os.Getuid() + 1
	if err := verifyOwnership(p, info, wrong); err == nil {
		t.Error("expected wrong-uid refusal")
	} else if !strings.Contains(err.Error(), "uid") {
		t.Errorf("error %q should mention uid", err.Error())
	}
}

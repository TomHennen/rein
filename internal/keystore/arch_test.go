package keystore

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoPrivateKeyReadsOutsideKeystore is the arch test for hard-constraint
// #6 (CLAUDE.md; audit #44 §2): ALL private-key reads must go through
// internal/keystore (Get/Fingerprint), which enforces uid + 0o077 mode
// checks and is the swap point for Phase 1's daemon-backed and biometric
// backends. A future `os.ReadFile(pemPath)` anywhere else would compile
// and pass every functional test while silently bypassing those checks.
//
// Two deliberately-simple, low-false-positive rules over all non-test Go
// source in the module:
//
//	A. No raw file-read call (os.ReadFile / os.Open / os.OpenFile /
//	   ioutil.ReadFile) whose arguments contain a ".pem" string literal.
//
//	B. No single function that BOTH raw-reads a file AND parses key/PEM
//	   material (pem.Decode, x509.ParsePKCS1PrivateKey /
//	   ParsePKCS8PrivateKey / ParseECPrivateKey) — the read-then-decode
//	   shape a keystore bypass necessarily takes.
//
// Allowlisted:
//   - internal/keystore/... — the enforcement point itself;
//   - internal/srt/cabundle.go — reads and PEM-decodes CA CERTIFICATE
//     bundles (public trust anchors, never private keys).
//
// If this test fires on legitimate new code, either route the read through
// keystore.Keystore or (for certificate-only material) extend the
// allowlist here with a justification comment.
func TestNoPrivateKeyReadsOutsideKeystore(t *testing.T) {
	root := moduleRoot(t)

	allowPrefixes := []string{
		filepath.Join("internal", "keystore") + string(filepath.Separator),
	}
	allowFiles := map[string]bool{
		filepath.Join("internal", "srt", "cabundle.go"): true, // certificate bundles only
	}

	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// Skip VCS/tooling dirs, vendored code, build output, testdata,
			// and nested worktrees.
			if name != "." && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "bin" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		for _, p := range allowPrefixes {
			if strings.HasPrefix(rel, p) {
				return nil
			}
		}
		if allowFiles[rel] {
			return nil
		}
		violations = append(violations, checkFile(t, path, rel)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(violations) > 0 {
		t.Errorf("hard-constraint #6 violations (private-key reads must go through internal/keystore):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// moduleRoot walks up from the package dir to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// checkFile applies rules A and B to one source file.
func checkFile(t *testing.T, path, rel string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}

	var violations []string

	// Rule A: file-read call with a ".pem" literal anywhere in its args.
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isFileReadCall(call) {
			return true
		}
		for _, arg := range call.Args {
			if containsPEMLiteral(arg) {
				pos := fset.Position(call.Pos())
				violations = append(violations,
					fmt.Sprintf("%s:%d: raw file read of a .pem path (rule A)", rel, pos.Line))
			}
		}
		return true
	})

	// Rule B: one function that both raw-reads a file and parses PEM/key
	// material.
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		var reads, parses bool
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if isFileReadCall(call) {
				reads = true
			}
			if isKeyParseCall(call) {
				parses = true
			}
			return true
		})
		if reads && parses {
			pos := fset.Position(fd.Pos())
			violations = append(violations,
				fmt.Sprintf("%s:%d: func %s both raw-reads a file and parses PEM/key material (rule B)", rel, pos.Line, fd.Name.Name))
		}
	}
	return violations
}

// isFileReadCall matches os.ReadFile / os.Open / os.OpenFile /
// ioutil.ReadFile.
func isFileReadCall(call *ast.CallExpr) bool {
	pkg, sel, ok := selector(call)
	if !ok {
		return false
	}
	switch pkg {
	case "os":
		return sel == "ReadFile" || sel == "Open" || sel == "OpenFile"
	case "ioutil":
		return sel == "ReadFile"
	}
	return false
}

// isKeyParseCall matches pem.Decode, the x509 private-key parsers, and
// githubauth.NewApplicationTokenSource (which parses the PEM internally,
// so raw-read + that call would otherwise slip past rule B's
// stdlib-parser signal).
func isKeyParseCall(call *ast.CallExpr) bool {
	pkg, sel, ok := selector(call)
	if !ok {
		return false
	}
	switch pkg {
	case "pem":
		return sel == "Decode"
	case "x509":
		return sel == "ParsePKCS1PrivateKey" || sel == "ParsePKCS8PrivateKey" || sel == "ParseECPrivateKey"
	case "githubauth":
		return sel == "NewApplicationTokenSource"
	}
	return false
}

// selector unpacks a call of the form pkgIdent.Sel(...).
func selector(call *ast.CallExpr) (pkg, sel string, ok bool) {
	se, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	id, ok := se.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	return id.Name, se.Sel.Name, true
}

// containsPEMLiteral reports whether the expression subtree contains a
// string literal containing ".pem" (case-insensitive).
func containsPEMLiteral(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if s, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(strings.ToLower(s), ".pem") {
			found = true
			return false
		}
		return true
	})
	return found
}

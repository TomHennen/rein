package main

// Issue #61, as a RUNNABLE before/after demonstration.
//
// The credential helper had targeted panic recovery in three places but none at
// the TOP of the dispatch. A panic anywhere else exited the process NON-ZERO
// with EMPTY STDOUT — and git treats a helper that gives "no answer" as license
// to fall through to the next credential source, i.e. the developer's ambient
// PAT. That is the whole TM-G8 threat (hard-constraint #2: the helper must
// ALWAYS return a credential, never empty, never error).
//
// The regression is pinned by the assertion tests in main_test.go
// (TestCredentialHelper_*Panic*). Those prove the invariant but their output is
// just "PASS". This file DEMONSTRATES the fix — it prints the before/after the
// PR body claims, with the REAL exit codes, by running the helper in a child
// process. Observing a genuine exit-2-on-panic is only possible out-of-process;
// an in-process recover() would mask it.
//
//	go test ./cmd/rein -run TestDemo_TMG8PanicRecovery -v
//
// SECURITY NOTE: the crash is injected via testPanicHook, a test-only package
// var (main.go: "a panic seam reachable from the environment WOULD be a real
// hazard"). It is armed only inside the child TEST process below — never in the
// shipped binary — so this demo adds no env-triggered panic to production.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// readFile returns a file's contents, or "" if it is missing/unreadable.
func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// helperReq is a github.com `get` — the request that MUST be answered (a
// non-github get may stay silent; a github get falling through to the PAT is the
// bug). "\n\n" terminates git's credential protocol block.
const helperReq = "protocol=https\nhost=github.com\n\n"

// TestHelperProcess_TMG8Panic is NOT an assertion test — it is the child-process
// body the demo re-executes. Guarded by REIN_DEMO_TMG8 so an ordinary
// `go test ./...` run skips it. It arms the panic hook, then runs the helper the
// way the FIX does (guarded) or the way the pre-fix dispatch did (unguarded),
// and exits exactly as main() maps each outcome.
func TestHelperProcess_TMG8Panic(t *testing.T) {
	mode := os.Getenv("REIN_DEMO_TMG8")
	if mode == "" {
		return // ordinary test run: this is a no-op
	}
	setupHelperTestEnv(t)
	testPanicHook = func(io.Writer) { panic("boom: simulated broker-path crash") }

	// The helper's OWN stdout/stderr go to files, not fd 1/2 — the test
	// framework also writes to fd 1 in this child, and mixing the two would
	// misreport "what the helper served to git". The parent reads these files.
	// Opened up front so the unguarded panic still leaves a (0-byte) stdout file.
	outF, err := os.Create(os.Getenv("REIN_DEMO_OUT"))
	if err != nil {
		t.Fatalf("open demo stdout: %v", err)
	}
	errF, err := os.Create(os.Getenv("REIN_DEMO_ERR"))
	if err != nil {
		t.Fatalf("open demo stderr: %v", err)
	}

	switch mode {
	case "guarded":
		// The #61 fix: the top-of-dispatch guard recovers the panic, drops any
		// partial stdout, and serves the placeholder. main() exits 0 on nil.
		code := 0
		if gerr := guardHelperPanic("get", strings.NewReader(helperReq), outF, errF); gerr != nil {
			code = 1
		}
		outF.Close()
		errF.Close()
		os.Exit(code)
	case "unguarded":
		// Pre-fix: the panic escapes the dispatch. Nothing was written to
		// stdout before the crash (the broker serves AFTER the hook point), so
		// the Go runtime tears the process down with a non-zero exit and an
		// EMPTY stdout file — the exact TM-G8 fall-through condition.
		_ = runCredentialHelperEnv("get", strings.NewReader(helperReq), outF, errF)
		os.Exit(0) // unreachable: the line above panics
	}
}

// TestDemo_TMG8PanicRecovery runs the child both ways and prints the before/after.
func TestDemo_TMG8PanicRecovery(t *testing.T) {
	// run the child once. Returns:
	//   code      the REAL process exit code (2 on unrecovered panic, 0 on the fix)
	//   helperOut what the helper wrote to ITS stdout (the bytes git would read) —
	//             captured via a file so the test framework's own fd-1 chatter
	//             can't inflate the "0 bytes" story
	//   helperErr what the helper wrote to ITS stderr (the diag)
	//   procErr   the child PROCESS stderr — carries the runtime panic trace
	run := func(mode string) (code int, helperOut, helperErr, procErr string) {
		dir := t.TempDir()
		outPath := dir + "/helper-stdout"
		errPath := dir + "/helper-stderr"
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess_TMG8Panic$")
		cmd.Env = append(os.Environ(),
			"REIN_DEMO_TMG8="+mode,
			"REIN_DEMO_OUT="+outPath,
			"REIN_DEMO_ERR="+errPath,
		)
		var pErr strings.Builder
		cmd.Stderr = &pErr // fd 2: the panic trace on the pre-fix leg
		// cmd.Stdout left nil (child fd 1 -> /dev/null): framework noise dropped.
		err := cmd.Run()
		code = 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run child (%s): %v", mode, err)
		}
		return code, readFile(outPath), readFile(errPath), pErr.String()
	}

	bCode, bOut, _, bProcErr := run("unguarded")
	gCode, gOut, gErr, _ := run("guarded")

	// firstLine returns the first non-blank line — the most telling one for a
	// panic trace or a diag block.
	firstLine := func(s string) string {
		for _, ln := range strings.Split(s, "\n") {
			if t := strings.TrimSpace(ln); t != "" {
				return t
			}
		}
		return "(none)"
	}

	t.Log("")
	t.Log("issue #61 — a panic in the credential helper must NOT become empty stdout")
	t.Log("(git reads an erroring/empty helper as 'no answer' and falls back to your PAT)")
	t.Log("")
	t.Logf("  %-12s %-14s %-14s %s", "", "BEFORE (raw)", "AFTER (guarded)", "")
	t.Logf("  %-12s %-14s %-14s", "exit code", fmt.Sprintf("%d", bCode), fmt.Sprintf("%d", gCode))
	t.Logf("  %-12s %-14s %-14s", "stdout bytes", fmt.Sprintf("%d", len(bOut)), fmt.Sprintf("%d", len(gOut)))
	t.Log("")
	t.Log("  BEFORE — unguarded dispatch, panic escapes:")
	t.Logf("    exit=%d  helper stdout=%q  stderr=%q", bCode, bOut, firstLine(bProcErr))
	t.Log("  AFTER  — guarded dispatch, panic recovered:")
	t.Log("    stdout served to git:")
	for _, ln := range strings.Split(strings.TrimRight(gOut, "\n"), "\n") {
		t.Logf("      | %s", ln)
	}
	t.Logf("    exit=%d  stderr(first)=%q", gCode, firstLine(gErr))
	t.Log("")

	// --- assertions: the before/after must actually hold -------------------
	if bCode == 0 {
		t.Errorf("BEFORE: unguarded panic should crash the process non-zero, got exit 0")
	}
	if strings.Contains(bOut, "password=") {
		t.Errorf("BEFORE: unguarded panic wrote a credential to stdout (%q); expected empty", bOut)
	}
	if gCode != 0 {
		t.Errorf("AFTER: guarded helper must exit 0 (git must not read 'no answer'), got %d", gCode)
	}
	if !strings.Contains(gOut, "username=x-access-token") ||
		!strings.Contains(gOut, "password=rein-placeholder-mint-failed") {
		t.Errorf("AFTER: guarded helper must serve the placeholder credential block, got stdout:\n%q", gOut)
	}
}

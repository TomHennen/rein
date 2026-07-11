package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// These tests exercise the REAL /proc ancestor walk (audit #44 §2) — no
// stubs. TestDetectWrite in main_test.go covers detectWriteIntent's routing
// with injected signals; nothing before this file executed
// detectFromProcTree/readPPid/splitCmdline against a live /proc.

func TestReadPPid_SelfMatchesGetppid(t *testing.T) {
	got, err := readPPid(os.Getpid())
	if err != nil {
		t.Fatalf("readPPid(self): %v", err)
	}
	if want := os.Getppid(); got != want {
		t.Errorf("readPPid(self) = %d, want %d (os.Getppid)", got, want)
	}
}

func TestSplitCmdline(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []string
	}{
		{"empty", nil, nil},
		{"only-nul", []byte("\x00"), nil},
		{"single", []byte("git\x00"), []string{"git"}},
		{"argv", []byte("git\x00push\x00origin\x00main\x00"), []string{"git", "push", "origin", "main"}},
		{"no-trailing-nul", []byte("git\x00push"), []string{"git", "push"}},
		{"embedded-empty-arg", []byte("git\x00\x00push\x00"), []string{"git", "", "push"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitCmdline(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("splitCmdline(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("splitCmdline(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
		})
	}
}

// TestDetectFromProcTree_NoGitPushAncestor: the test process's real
// ancestry (go test -> shell -> ...) contains no `git push`, so the walk
// must report read intent — fail-closed routing (misclassifying a read as
// a write would over-grant; the reverse only 403s).
func TestDetectFromProcTree_NoGitPushAncestor(t *testing.T) {
	write, src := detectFromProcTree()
	if write {
		t.Errorf("detectFromProcTree() = true (src=%q) with no git-push ancestor; write intent must require positive evidence", src)
	}
	if src != "" {
		t.Errorf("src = %q, want empty when no match", src)
	}
}

// TestDetectFromProcTree_FindsGitPushAncestor exercises the POSITIVE leg
// against a real /proc: it re-executes this test binary (the helper probe
// below) as the grandchild of a shell whose /proc/<pid>/cmdline reads
// `git -c <script> push` — argv[0] is spoofed via exec.Cmd's Path/Args
// split, exactly the argv-level signal the walk keys on. The probe's
// ancestor chain is then probe -> sh(cmdline "git -c ... push") -> this
// test, so detectFromProcTree inside the probe must report write intent.
func TestDetectFromProcTree_FindsGitPushAncestor(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// $0 inside the script is "push" (sh consumes it as the $0 operand);
	// the script itself just runs the probe test in this same binary.
	script := fmt.Sprintf("%q -test.run '^TestProcTreeHelperProbe$' -test.v", exe)
	cmd := &exec.Cmd{
		Path: "/bin/sh",
		// /proc/<pid>/cmdline of this sh: git\0-c\0<script>\0push\0 —
		// isGitVerb sees base(argv[0])=="git", skips -c + its value, and
		// matches the verb "push".
		Args: []string{"git", "-c", script, "push"},
		Env:  append(os.Environ(), "REIN_PROCTREE_HELPER_PROBE=1"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper probe failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "PROCTREE_DETECTED write=true") {
		t.Errorf("probe did not detect the spoofed git-push ancestor; output:\n%s", out)
	}
}

// TestProcTreeHelperProbe is the in-child half of
// TestDetectFromProcTree_FindsGitPushAncestor. It is a no-op (skip) in a
// normal `go test` run and only does work when re-executed under the
// spoofed-argv shell above.
func TestProcTreeHelperProbe(t *testing.T) {
	if os.Getenv("REIN_PROCTREE_HELPER_PROBE") == "" {
		t.Skip("helper probe; driven by TestDetectFromProcTree_FindsGitPushAncestor")
	}
	write, src := detectFromProcTree()
	// The parent test greps for this exact marker in the child's output.
	fmt.Printf("PROCTREE_DETECTED write=%v src=%q\n", write, src)
	if !write {
		t.Fatalf("detectFromProcTree() = false inside probe; expected the `git push`-shaped ancestor to be detected")
	}
	if !strings.Contains(src, "push") {
		t.Errorf("src = %q, want the matched ancestor cmdline to contain %q", src, "push")
	}
}

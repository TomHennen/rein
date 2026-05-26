//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const procTreePlatform = "darwin"

// detectFromProcTree (darwin) snapshots the system process table via
// `ps -ax -o pid=,ppid=,command=` and walks up the ppid chain from
// our own pid. One shell-out per detection call (typical: ~10-30ms);
// well under the per-mint latency budget.
//
// Avoiding cgo + libproc deliberately: keeps the binary truly single-
// file portable, avoids a darwin-only build dependency, and the worst-
// case behavior of `ps` is benign (returns a non-zero exit or absent
// data → we conclude "no write detected" and fall through to read,
// same as Linux on /proc errors).
//
// Limitation: `ps` joins argv with single spaces, so an argv element
// containing internal whitespace will be flattened. The heuristic we
// run (`isGitVerb` looking for `git push` / `git send-pack`) only cares
// about argv[0]'s basename and a trailing verb token, so the
// flattening doesn't affect detection in practice. Matches the routing-
// signal-not-security-boundary contract documented in proctree.go.
func detectFromProcTree() (bool, string) {
	// Defense-in-depth: pin LC_ALL=C so locale-dependent column
	// formatting can't surprise the parser, pin PATH to system dirs so
	// a hostile `ps` earlier on PATH can't intercept (consistent with
	// "fail closed" — this code runs inside the credential helper),
	// and explicitly nil stdin so `ps` can't ever block on a missing
	// stream.
	cmd := exec.Command("ps", "-ax", "-o", "pid=,ppid=,command=")
	cmd.Env = []string{"LC_ALL=C", "PATH=/bin:/usr/bin"}
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return false, ""
	}

	type entry struct {
		ppid int
		cmd  string
	}
	proc := make(map[int]entry, 256)

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		proc[pid] = entry{ppid: ppid, cmd: strings.Join(fields[2:], " ")}
	}

	pid := os.Getpid()
	for i := 0; i < procTreeDepth; i++ {
		e, ok := proc[pid]
		if !ok || e.ppid <= 1 {
			return false, ""
		}
		parent, ok := proc[e.ppid]
		if !ok {
			return false, ""
		}
		argv := strings.Fields(parent.cmd)
		if isGitVerb(argv, "push") || isGitVerb(argv, "send-pack") {
			return true, parent.cmd
		}
		pid = e.ppid
	}
	return false, ""
}

//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const procTreePlatform = "linux"

// detectFromProcTree (linux) walks /proc/<pid>/status (for PPid) and
// /proc/<pid>/cmdline (for argv) up to procTreeDepth ancestors. The
// cmdline buffer is NUL-separated, so we get exact argv splitting
// without the whitespace-collapsing approximation darwin's ps path has
// to tolerate.
func detectFromProcTree() (bool, string) {
	pid := os.Getpid()
	for i := 0; i < procTreeDepth; i++ {
		ppid, err := readPPid(pid)
		if err != nil || ppid <= 1 {
			return false, ""
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", ppid))
		if err != nil {
			return false, ""
		}
		args := splitCmdline(cmdline)
		if isGitVerb(args, "push") || isGitVerb(args, "send-pack") {
			return true, strings.Join(args, " ")
		}
		pid = ppid
	}
	return false, ""
}

func readPPid(pid int) (int, error) {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				return strconv.Atoi(f[1])
			}
		}
	}
	return 0, fmt.Errorf("no PPid for pid %d", pid)
}

// splitCmdline parses a /proc/<pid>/cmdline NUL-separated buffer into argv.
func splitCmdline(b []byte) []string {
	s := string(b)
	s = strings.TrimRight(s, "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

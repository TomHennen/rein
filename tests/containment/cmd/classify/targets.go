// Probe targets are DERIVED from the emitted settings.json (plus srt's own
// default write paths and fixed canaries), so the in-sandbox probe never
// hand-maintains a list that can drift from the config (#141).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/srt"
	"github.com/TomHennen/rein/tests/containment"
)

// writeProbeName is the file the probe tries to create inside each write target.
const writeProbeName = ".rein-containment-write-probe"

// deniedCanary is a well-known host outside every allowlist — reachable in the
// sandbox means the egress boundary is gone entirely.
const deniedCanary = "example.com"

// reachableControl must be reachable under rein's default egress, so a broken
// proxy or CA cannot read as "denied host correctly unreachable".
const reachableControl = "api.github.com"

type targets struct {
	Network []string     `json:"network"`
	Files   []fileTarget `json:"files"`
	// Env names the host sets to a canary value before launch.
	Env []string `json:"env"`
	// Files to create in-sandbox; persistence is judged host-side.
	Writes []string `json:"writes"`
}

// fileTarget is readable when any non-empty file at or under Target, outside
// Except, can be read. Except holds the binds nested in a deny: content there
// is meant to show.
type fileTarget struct {
	Target string   `json:"target"`
	Except []string `json:"except,omitempty"`
}

// buildTargets derives the probe targets. home is run.sh's fixture: every
// policy entry under it must be planted. nothingToHide lists deny entries
// with no host content, whose "correctly unreadable" row is vacuous.
func buildTargets(cfg srt.Config, home string) (t targets, nothingToHide []string, err error) {
	t = targets{Env: containment.EnvCanaryNames}
	fs := cfg.Filesystem
	if err := requirePlanted(home, append(append([]string{}, fs.DenyRead...), fs.AllowRead...)); err != nil {
		return t, nil, err
	}

	local := map[string]bool{}
	for _, d := range proxy.LocalHosts {
		local[strings.ToLower(d)] = true
	}
	for _, d := range cfg.Network.AllowedDomains {
		if !local[strings.ToLower(d)] {
			t.Network = append(t.Network, d)
		}
	}
	t.Network = append(t.Network, deniedCanary)

	binds := append(append([]string{}, fs.AllowRead...), fs.AllowWrite...)
	for _, d := range fs.DenyRead {
		// The per-run socket dir is the CAPTURE run's: gone before the probe
		// run starts, so there is nothing to judge.
		if strings.HasPrefix(filepath.Base(d), "run-") && filepath.Base(filepath.Dir(d)) == "rein" {
			continue
		}
		var except []string
		for _, b := range binds {
			if strings.HasPrefix(b, d+string(filepath.Separator)) {
				except = append(except, b)
			}
		}
		if !hasContent(d, except) {
			nothingToHide = append(nothingToHide, d)
		}
		t.Files = append(t.Files, fileTarget{Target: d, Except: except})
	}
	// Allow-backs are existence-gated at mount time by srt; probing an absent
	// one reads as a false regression.
	for _, a := range fs.AllowRead {
		if _, err := os.Stat(a); err == nil {
			t.Files = append(t.Files, fileTarget{Target: a})
		}
	}

	// Write sweep: every writable bind, every denyWrite DIR pin, srt's own
	// default write paths (the substrate-chosen set that leaked in #153), and a
	// bare-$HOME canary for the deny tmpfs.
	for _, w := range fs.AllowWrite {
		// rein discards the ephemeral clone dir after the run, and it is
		// per-run: persistence is unjudgeable there.
		if strings.HasPrefix(filepath.Base(w), "rein-agent-tmp-") {
			continue
		}
		t.Writes = append(t.Writes, filepath.Join(w, writeProbeName))
	}
	for _, w := range fs.DenyWrite {
		if st, err := os.Stat(w); err == nil && st.IsDir() {
			t.Writes = append(t.Writes, filepath.Join(w, writeProbeName))
		}
	}
	for _, w := range srt.DefaultHomeWritePaths(home) {
		t.Writes = append(t.Writes, filepath.Join(w, writeProbeName))
	}
	t.Writes = append(t.Writes, filepath.Join(home, writeProbeName))
	return t, nothingToHide, nil
}

// requirePlanted fails when a policy entry under the fixture home has nothing
// planted: it could not be judged. A policy with NO entry under home means
// home is not what rein hid.
func requirePlanted(home string, entries []string) error {
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e, home+string(filepath.Separator)) {
			continue
		}
		n++
		if !hasContent(e, nil) {
			return fmt.Errorf("policy entry %s has no content in the fixture home: add it to fixtureFiles (tests/containment/cmd/classify/fixture.go)", e)
		}
	}
	if n == 0 {
		return fmt.Errorf("no policy entry is under the fixture home %s", home)
	}
	return nil
}

// hasContent reports whether a readable, non-empty file exists at or under
// path, outside except. It mirrors the in-sandbox read probe.
func hasContent(path string, except []string) bool {
	skip := map[string]bool{}
	for _, e := range except {
		skip[e] = true
	}
	found := false
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		switch {
		case err != nil:
		case skip[p] && d.IsDir():
			return filepath.SkipDir
		case skip[p] || d.IsDir():
		default:
			if f, err := os.Open(p); err == nil {
				n, _ := f.Read(make([]byte, 1))
				f.Close()
				if n > 0 {
					found = true
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	return found
}

// writeObservations judges write PERSISTENCE on the host, and removes what
// persisted. The paths are derived here, not taken from the sandbox.
func writeObservations(paths []string) []containment.Observation {
	var out []containment.Observation
	for _, p := range paths {
		_, err := os.Lstat(p)
		out = append(out, containment.Observation{Kind: containment.KindWrite, Target: p, Reachable: err == nil})
		if err == nil {
			_ = os.Remove(p)
		}
	}
	return out
}

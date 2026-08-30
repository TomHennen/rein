// -targets mode: emit the probe-target lists DERIVED from the emitted
// settings.json (plus the oracle's env denylist, srt's own default write paths,
// and fixed canaries), so the in-sandbox probe script never hand-maintains a
// list that can drift from the config (#141).
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/srt"
	"github.com/TomHennen/rein/tests/containment"
)

// WriteProbeName is the file the probe tries to create inside each write target
// dir; run.sh checks host-side persistence of exactly these paths afterwards.
const WriteProbeName = ".rein-containment-write-probe"

// deniedCanary is a well-known host outside every allowlist — reachable in the
// sandbox means the egress boundary is gone entirely.
const deniedCanary = "example.com"

// sampleFile returns the lexically-first regular file directly inside dir, or
// "" (dir absent, not a dir, or nothing sampleable).
func sampleFile(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range ents { // ReadDir sorts by name
		if e.Type().IsRegular() {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

type targets struct {
	// Network hosts to probe (reachability; the probe script adds the
	// api.github.com token heuristic on its own).
	Network []string `json:"network"`
	// Files/dirs to probe for readability.
	Files []string `json:"files"`
	// Env var names to probe for presence.
	Env []string `json:"env"`
	// Write-probe FILE paths: create in-sandbox; persistence is judged host-side.
	Writes []string `json:"writes"`
}

func emitTargets(cfg srt.Config) error {
	t := targets{Env: containment.SensitiveEnv}

	// Inject hosts other than api.github.com are skipped: the probe's only
	// honest token evidence is the api.github.com rate-limit heuristic, and a
	// reachable inject host with unverifiable token reads as a false
	// regression. Local virtual hosts (declare.rein.internal) are skipped too.
	inject := map[string]bool{}
	if cfg.Network.MitmProxy != nil {
		for _, d := range cfg.Network.MitmProxy.Domains {
			inject[strings.ToLower(d)] = true
		}
	}
	local := map[string]bool{}
	for _, d := range proxy.LocalHosts {
		local[strings.ToLower(d)] = true
	}
	for _, d := range cfg.Network.AllowedDomains {
		l := strings.ToLower(d)
		if local[l] || (inject[l] && l != "api.github.com") {
			continue
		}
		t.Network = append(t.Network, d)
	}
	t.Network = append(t.Network, deniedCanary)

	// A denied DIR is probed through a host-sampled file inside it: the deny
	// tmpfs legitimately lists allow-back scaffolding (#150), so "listing
	// non-empty" would be a false leak — "a real host file readable" is the
	// truth. Dirs with no sampleable file keep the dir probe.
	for _, d := range cfg.Filesystem.DenyRead {
		if f := sampleFile(d); f != "" {
			t.Files = append(t.Files, f)
		} else {
			t.Files = append(t.Files, d)
		}
	}
	// Allow-backs are existence-gated at mount time by srt; probing an absent
	// one reads as a false regression.
	for _, a := range cfg.Filesystem.AllowRead {
		if _, err := os.Stat(a); err == nil {
			t.Files = append(t.Files, a)
		}
	}

	// Write sweep: every writable bind, every denyWrite DIR pin, srt's own
	// default write paths (the substrate-chosen set that leaked in #153), and a
	// bare-$HOME canary for the deny tmpfs.
	for _, w := range cfg.Filesystem.AllowWrite {
		// The ephemeral clone dir is discarded by rein AFTER the run by design
		// (and is per-run, so the capture run's path never exists in the probe
		// run) — persistence is unjudgeable there.
		if strings.HasPrefix(filepath.Base(w), "rein-agent-tmp-") {
			continue
		}
		t.Writes = append(t.Writes, filepath.Join(w, WriteProbeName))
	}
	for _, w := range cfg.Filesystem.DenyWrite {
		if st, err := os.Stat(w); err == nil && st.IsDir() {
			t.Writes = append(t.Writes, filepath.Join(w, WriteProbeName))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, w := range srt.DefaultHomeWritePaths(home) {
			t.Writes = append(t.Writes, filepath.Join(w, WriteProbeName))
		}
		t.Writes = append(t.Writes, filepath.Join(home, WriteProbeName))
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(t)
}

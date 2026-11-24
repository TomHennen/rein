// Command classify is the host side of the containment harness (issue #136B;
// tests/containment/README.md). Three modes, all driven by run.sh:
//
//	classify -fixture DIR                    write the fixture $HOME
//	classify -settings S -targets            emit the probe targets
//	classify -settings S -observations O -probe-report P -tolerated T -normalized-out N
//	                                         judge the run
//
// Judging exits 3 on any leak and 4 on any untriaged "unknown" row.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TomHennen/rein/internal/srt"
	"github.com/TomHennen/rein/tests/containment"
)

func main() {
	fixtureDir := flag.String("fixture", "", "write the fixture $HOME into this dir and exit")
	settingsPath := flag.String("settings", "", "rein's emitted srt settings.json")
	emitTargets := flag.Bool("targets", false, "emit the config-derived probe targets as JSON and exit")
	obsPath := flag.String("observations", "", "the in-sandbox probe's observations")
	probeReportPath := flag.String("probe-report", "", "sandbox-probe's in-sandbox report")
	toleratedPath := flag.String("tolerated", "", "tolerated.txt")
	normOut := flag.String("normalized-out", "", "where to write the normalized golden rows")
	var subs subFlag
	flag.Var(&subs, "norm", "LABEL=PATH to normalize (repeatable, applied in order)")
	flag.Parse()

	if *fixtureDir != "" {
		check(writeFixture(*fixtureDir))
		return
	}
	if *settingsPath == "" {
		fail("-settings is required")
	}
	data, err := os.ReadFile(*settingsPath)
	check(err)
	var cfg srt.Config
	check(json.Unmarshal(data, &cfg))
	// rein, not srt, enforces egress (#185), so settings.json lists no hosts.
	cfg.Network.AllowedDomains = append(cfg.Network.AllowedDomains, reachableControl)
	home, err := os.UserHomeDir()
	check(err)
	t, nothingToHide, err := buildTargets(cfg, home)
	check(err)
	if *emitTargets {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		check(enc.Encode(t))
		return
	}
	if *obsPath == "" || *probeReportPath == "" || *toleratedPath == "" || *normOut == "" {
		fail("judging needs -observations, -probe-report, -tolerated and -normalized-out")
	}

	obs, err := loadObservations(*obsPath, *probeReportPath)
	check(err)
	obs = append(obs, writeObservations(t.Writes)...)
	results := containment.NewOracle(cfg).ClassifyAll(obs)
	counts := map[containment.Verdict]int{}
	for _, r := range results {
		fmt.Println(containment.Row{Verdict: r.Verdict, Kind: r.Observation.Kind, Target: r.Observation.Target, Reason: r.Reason})
		counts[r.Verdict]++
	}
	fmt.Printf("\nsummary: %d ok, %d leak, %d regression, %d unknown (of %d)\n",
		counts[containment.VerdictOK], counts[containment.VerdictLeak], counts[containment.VerdictRegression], counts[containment.VerdictUnknown], len(results))
	for _, d := range nothingToHide {
		fmt.Printf("NOTE: %s has no content on this host: its deny row proves nothing here\n", d)
	}

	tf, err := os.Open(*toleratedPath)
	check(err)
	tol, err := containment.ParseTolerated(tf)
	check(err)
	golden, untriaged := containment.Split(results, containment.NewNormalizer(subs, os.Getuid()), tol)
	var b strings.Builder
	for _, r := range golden {
		b.WriteString(r.String() + "\n")
	}
	check(os.WriteFile(*normOut, []byte(b.String()), 0o644))

	// A leak outranks an untriaged row, but both are always printed.
	rc := 0
	for _, r := range untriaged {
		fmt.Fprintf(os.Stderr, "classify: UNTRIAGED: %s\n    decide whether rein should deny it; if it is acceptable, add it to %s with the reason\n", r, *toleratedPath)
		rc = 4
	}
	if containment.HasLeak(results) {
		fmt.Fprintln(os.Stderr, "classify: CONTAINMENT LEAK(S) DETECTED — failing closed")
		rc = 3
	}
	os.Exit(rc)
}

func check(err error) {
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "classify:", msg)
	os.Exit(2)
}

// subFlag collects -norm LABEL=PATH pairs.
type subFlag []containment.Sub

func (s *subFlag) String() string { return fmt.Sprint(*s) }
func (s *subFlag) Set(v string) error {
	label, path, ok := strings.Cut(v, "=")
	if !ok || label == "" {
		return fmt.Errorf("want LABEL=PATH, got %q", v)
	}
	// The policy may name the path through its symlink-resolved form.
	if r, err := filepath.EvalSymlinks(path); err == nil && r != path {
		*s = append(*s, containment.Sub{Path: r, Label: "<" + label + ">"})
	}
	*s = append(*s, containment.Sub{Path: path, Label: "<" + label + ">"})
	return nil
}

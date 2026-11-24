// Command classify is the oracle CLI for the containment harness (issue #136B).
// It reads rein's emitted settings.json and a NORMALIZED observations file (the
// in-sandbox probe's result — see tests/containment/README.md), classifies each observation against the
// config-derived oracle, and prints a report. Exit 3 if any leak is found, so
// the harness / CI fails closed on a containment regression.
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
	settingsPath := flag.String("settings", "", "path to rein's emitted srt settings.json")
	obsPath := flag.String("observations", "", "path to the normalized observations JSON (see README)")
	probeReportPath := flag.String("probe-report", "", "sandbox-probe report from the IN-SANDBOX run, mapped and merged (run.sh always passes it)")
	emitTargetsFlag := flag.Bool("targets", false, "emit the config-derived probe targets as JSON and exit")
	jsonOut := flag.Bool("json", false, "emit the classified results as JSON instead of text")
	fixtureDir := flag.String("fixture", "", "write the fixture $HOME into this dir and exit")
	toleratedPath := flag.String("tolerated", "", "tolerated.txt: split the rows into golden rows and untriaged unknowns (exit 4 on any)")
	normOut := flag.String("normalized-out", "", "with -tolerated: write the normalized golden rows here")
	uid := flag.Int("uid", os.Getuid(), "with -tolerated: uid, normalized to <UID>")
	var subs subFlag
	flag.Var(&subs, "norm", "with -tolerated: LABEL=PATH to normalize (repeatable, applied in order)")
	flag.Parse()

	if *fixtureDir != "" {
		if err := writeFixture(*fixtureDir); err != nil {
			fmt.Fprintf(os.Stderr, "classify: fixture: %v\n", err)
			os.Exit(2)
		}
		return
	}
	if *settingsPath == "" || (*obsPath == "" && !*emitTargetsFlag) {
		fmt.Fprintln(os.Stderr, "usage: classify -settings settings.json (-targets | -observations obs.json [-probe-report report.json]) [-json]")
		os.Exit(2)
	}

	cfg, err := loadConfig(*settingsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "classify: load settings: %v\n", err)
		os.Exit(2)
	}
	// rein, not srt, enforces egress (#185), so settings.json lists no hosts.
	cfg.Network.AllowedDomains = append(cfg.Network.AllowedDomains, reachableControl)
	if *emitTargetsFlag {
		if err := emitTargets(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "classify: emit targets: %v\n", err)
			os.Exit(2)
		}
		return
	}
	obs, emptyDirs, err := loadObservations(*obsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "classify: load observations: %v\n", err)
		os.Exit(2)
	}
	if *probeReportPath != "" {
		empty := map[string]bool{}
		for _, d := range emptyDirs {
			empty[d] = true
		}
		mapped, err := mapProbeReport(*probeReportPath, empty)
		if err != nil {
			fmt.Fprintf(os.Stderr, "classify: map probe report: %v\n", err)
			os.Exit(2)
		}
		obs = append(obs, mapped...)
	}

	results := containment.NewOracle(cfg).ClassifyAll(obs)

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
	} else {
		printText(results)
	}

	// A leak outranks an untriaged row, but both are always printed.
	rc := 0
	if *toleratedPath != "" {
		untriaged, err := splitRows(*toleratedPath, *normOut, containment.NewNormalizer(subs, *uid), results)
		if err != nil {
			fmt.Fprintf(os.Stderr, "classify: tolerated: %v\n", err)
			os.Exit(2)
		}
		for _, r := range untriaged {
			fmt.Fprintf(os.Stderr, "classify: UNTRIAGED: %s\n    decide whether rein should deny it; if it is acceptable, add it to %s with the reason\n", r, *toleratedPath)
			rc = 4
		}
	}
	if containment.HasLeak(results) {
		fmt.Fprintln(os.Stderr, "classify: CONTAINMENT LEAK(S) DETECTED — failing closed")
		rc = 3
	}
	os.Exit(rc)
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

func splitRows(toleratedPath, normOut string, n containment.Normalizer, results []containment.Result) ([]containment.Row, error) {
	f, err := os.Open(toleratedPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tol, err := containment.ParseTolerated(f)
	if err != nil {
		return nil, err
	}
	golden, untriaged := containment.Split(results, n, tol)
	if normOut != "" {
		var b strings.Builder
		for _, r := range golden {
			b.WriteString(r.String() + "\n")
		}
		if err := os.WriteFile(normOut, []byte(b.String()), 0o644); err != nil {
			return nil, err
		}
	}
	return untriaged, nil
}

func loadConfig(path string) (srt.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return srt.Config{}, err
	}
	var cfg srt.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return srt.Config{}, err
	}
	return cfg, nil
}

func loadObservations(path string) (obs []containment.Observation, emptyDirs []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	// The normalized file is a flat object of channel arrays.
	var doc struct {
		Network []containment.Observation `json:"network"`
		Files   []containment.Observation `json:"files"`
		Env     []containment.Observation `json:"env"`
		Writes  []containment.Observation `json:"writes"`
		Sockets []containment.Observation `json:"sockets"`
		// Dirs sandbox-probe flagged readable that are empty in-sandbox.
		EmptyDirs []string `json:"empty_dirs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	// Stamp the Kind from the section, so the normalized file need not repeat it.
	var out []containment.Observation
	for _, o := range doc.Network {
		o.Kind = containment.KindNetwork
		out = append(out, o)
	}
	for _, o := range doc.Files {
		o.Kind = containment.KindFile
		out = append(out, o)
	}
	for _, o := range doc.Env {
		o.Kind = containment.KindEnv
		out = append(out, o)
	}
	for _, o := range doc.Writes {
		o.Kind = containment.KindWrite
		out = append(out, o)
	}
	for _, o := range doc.Sockets {
		o.Kind = containment.KindSocket
		out = append(out, o)
	}
	return out, doc.EmptyDirs, nil
}

func printText(results []containment.Result) {
	var leaks, regressions, unknown, ok int
	for _, r := range results {
		fmt.Println(containment.Row{Verdict: r.Verdict, Kind: r.Observation.Kind, Target: r.Observation.Target, Reason: r.Reason})
		switch r.Verdict {
		case containment.VerdictLeak:
			leaks++
		case containment.VerdictRegression:
			regressions++
		case containment.VerdictUnknown:
			unknown++
		default:
			ok++
		}
	}
	fmt.Printf("\nsummary: %d ok, %d leak, %d regression, %d unknown (of %d)\n",
		ok, leaks, regressions, unknown, len(results))
}

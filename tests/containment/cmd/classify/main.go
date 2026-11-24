// Command classify is the oracle CLI for the containment harness (issue #136B).
// It reads rein's emitted settings.json and a NORMALIZED observations file (the
// differential probe's host-vs-sandbox result, mapped into the oracle's schema
// — see tests/containment/README.md), classifies each observation against the
// config-derived oracle, and prints a report. Exit 3 if any leak is found, so
// the harness / CI fails closed on a containment regression.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/TomHennen/rein/internal/srt"
	"github.com/TomHennen/rein/tests/containment"
)

func main() {
	settingsPath := flag.String("settings", "", "path to rein's emitted srt settings.json")
	obsPath := flag.String("observations", "", "path to the normalized observations JSON (see README)")
	probeReportPath := flag.String("probe-report", "", "optional sandbox-probe report from the IN-SANDBOX run, mapped and merged")
	emitTargetsFlag := flag.Bool("targets", false, "emit the config-derived probe targets as JSON and exit")
	jsonOut := flag.Bool("json", false, "emit the classified results as JSON instead of text")
	expectPath := flag.String("expect", "", "expectations file to check the normalized rows against (exit 4 on a mismatch)")
	normOut := flag.String("normalized-out", "", "with -expect: also write the normalized rows here")
	home := flag.String("home", os.Getenv("HOME"), "with -expect: home dir, normalized to <HOME>")
	worktree := flag.String("worktree", "", "with -expect: the run's working tree, normalized to <WORKTREE>")
	uid := flag.Int("uid", os.Getuid(), "with -expect: uid, normalized to <UID>")
	flag.Parse()

	if *settingsPath == "" || (*obsPath == "" && !*emitTargetsFlag) {
		fmt.Fprintln(os.Stderr, "usage: classify -settings settings.json (-targets | -observations obs.json [-probe-report report.json]) [-json]")
		os.Exit(2)
	}

	cfg, err := loadConfig(*settingsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "classify: load settings: %v\n", err)
		os.Exit(2)
	}
	if *emitTargetsFlag {
		if err := emitTargets(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "classify: emit targets: %v\n", err)
			os.Exit(2)
		}
		return
	}
	obs, err := loadObservations(*obsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "classify: load observations: %v\n", err)
		os.Exit(2)
	}
	if *probeReportPath != "" {
		probed := map[string]bool{}
		for _, o := range obs {
			if o.Kind == containment.KindSocket {
				probed[o.Target] = true
			}
		}
		mapped, err := mapProbeReport(*probeReportPath, probed)
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

	// A leak outranks an expectation mismatch, but both are always printed.
	rc := 0
	if *expectPath != "" {
		n := containment.Normalizer{Home: *home, Worktree: *worktree, UID: *uid}
		problems, err := checkExpectations(*expectPath, *normOut, n, results)
		if err != nil {
			fmt.Fprintf(os.Stderr, "classify: expectations: %v\n", err)
			os.Exit(2)
		}
		if len(problems) > 0 {
			fmt.Fprintf(os.Stderr, "classify: %d EXPECTATION MISMATCH(ES) against %s:\n", len(problems), *expectPath)
			for _, p := range problems {
				fmt.Fprintln(os.Stderr, "  "+p)
			}
			rc = 4
		}
	}
	if containment.HasLeak(results) {
		fmt.Fprintln(os.Stderr, "classify: CONTAINMENT LEAK(S) DETECTED — failing closed")
		rc = 3
	}
	os.Exit(rc)
}

func checkExpectations(path, normOut string, n containment.Normalizer, results []containment.Result) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	exps, err := containment.ParseExpectations(f)
	if err != nil {
		return nil, err
	}
	rows := n.Rows(results)
	if normOut != "" {
		var b strings.Builder
		for _, r := range rows {
			b.WriteString(r.String() + "\n")
		}
		if err := os.WriteFile(normOut, []byte(b.String()), 0o644); err != nil {
			return nil, err
		}
	}
	exists := func(target string) bool {
		return containment.HasContent(strings.ReplaceAll(target, "<HOME>", n.Home))
	}
	return containment.Check(rows, exps, exists), nil
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

func loadObservations(path string) ([]containment.Observation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// The normalized file is a flat object of channel arrays.
	var doc struct {
		Network []containment.Observation `json:"network"`
		Files   []containment.Observation `json:"files"`
		Env     []containment.Observation `json:"env"`
		Writes  []containment.Observation `json:"writes"`
		Sockets []containment.Observation `json:"sockets"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
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
	return out, nil
}

func printText(results []containment.Result) {
	var leaks, regressions, unknown, ok int
	for _, r := range results {
		fmt.Printf("[%-10s] %-7s %s — %s\n", r.Verdict, r.Observation.Kind, r.Observation.Target, r.Reason)
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

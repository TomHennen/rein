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

	"github.com/TomHennen/rein/internal/srt"
	"github.com/TomHennen/rein/tests/containment"
)

func main() {
	settingsPath := flag.String("settings", "", "path to rein's emitted srt settings.json")
	obsPath := flag.String("observations", "", "path to the normalized observations JSON (see README)")
	jsonOut := flag.Bool("json", false, "emit the classified results as JSON instead of text")
	flag.Parse()

	if *settingsPath == "" || *obsPath == "" {
		fmt.Fprintln(os.Stderr, "usage: classify -settings settings.json -observations obs.json [-json]")
		os.Exit(2)
	}

	cfg, err := loadConfig(*settingsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "classify: load settings: %v\n", err)
		os.Exit(2)
	}
	obs, err := loadObservations(*obsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "classify: load observations: %v\n", err)
		os.Exit(2)
	}

	results := containment.NewOracle(cfg).ClassifyAll(obs)

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
	} else {
		printText(results)
	}

	if containment.HasLeak(results) {
		fmt.Fprintln(os.Stderr, "classify: CONTAINMENT LEAK(S) DETECTED — failing closed")
		os.Exit(3)
	}
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

// -probe-report mode: map sandbox-probe's native report (its IN-SANDBOX run)
// into oracle observations, merged with the supplement's. Only the channels the
// oracle can judge are mapped; ports/processes/mounts stay in the raw report
// for human triage.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/TomHennen/rein/tests/containment"
)

// probeCanary is always readable in the sandbox. A report without it means the
// enumerator did not really run, and "found nothing" would be meaningless.
const probeCanary = "/etc/passwd"

// mappedFindings are the finding types the oracle judges, and the kind each
// maps to. A report missing one (renamed upstream, scan cut short) is an error.
var mappedFindings = map[string]containment.Kind{
	"external_host_connectivity": containment.KindNetwork,
	"sensitive_readable_paths":   containment.KindFile,
	"unix_socket_detection":      containment.KindSocket,
}

type probeReport struct {
	Findings []struct {
		FindingType string          `json:"findingType"`
		Value       json.RawMessage `json:"value"`
	} `json:"findings"`
}

// mapProbeReport converts the mappable finding types. sandbox-probe reports
// only what it FOUND, so every mapped observation is Reachable=true; the
// "correctly blocked" rows come from the config-derived supplement.
//
// emptyDirs are "readable" dirs the in-sandbox probe found empty: a deny tmpfs
// mount point, which sandbox-probe flags because it can list it.
func mapProbeReport(path string, emptyDirs map[string]bool) ([]containment.Observation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rep probeReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, err
	}
	var out []containment.Observation
	seen := map[string]bool{}
	canary := false
	for _, f := range rep.Findings {
		kind, ok := mappedFindings[f.FindingType]
		if !ok {
			continue
		}
		var vals []string
		if err := json.Unmarshal(f.Value, &vals); err != nil {
			return nil, fmt.Errorf("finding %s: value is not a string list (sandbox-probe report shape changed?)", f.FindingType)
		}
		seen[f.FindingType] = true
		for _, v := range vals {
			if kind == containment.KindFile {
				canary = canary || v == probeCanary
				if emptyDirs[v] {
					continue
				}
			}
			out = append(out, containment.Observation{Kind: kind, Target: v, Reachable: true})
		}
	}
	for ft := range mappedFindings {
		if !seen[ft] {
			return nil, fmt.Errorf("report has no %s finding (scan incomplete, or sandbox-probe renamed it)", ft)
		}
	}
	if !canary {
		return nil, fmt.Errorf("report does not list %s as readable: the in-sandbox scan did not run properly", probeCanary)
	}
	return out, nil
}

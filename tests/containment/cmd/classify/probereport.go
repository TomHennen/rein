// -probe-report mode: map sandbox-probe's native report (its IN-SANDBOX run)
// into oracle observations, merged with the supplement's. Only the channels the
// oracle can judge are mapped; ports/processes/mounts stay in the raw report
// for human triage.
package main

import (
	"encoding/json"
	"os"

	"github.com/TomHennen/rein/tests/containment"
)

type probeReport struct {
	Findings []struct {
		FindingType string          `json:"findingType"`
		Value       json.RawMessage `json:"value"`
	} `json:"findings"`
}

// mapProbeReport converts the mappable finding types. sandbox-probe reports
// only what it FOUND (reached hosts, readable paths), so every mapped
// observation is Reachable=true; the "correctly blocked" rows come from the
// config-derived supplement, which probes known targets in both directions.
func mapProbeReport(path string) ([]containment.Observation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rep probeReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, err
	}
	var out []containment.Observation
	for _, f := range rep.Findings {
		var kind containment.Kind
		switch f.FindingType {
		case "external_host_connectivity":
			kind = containment.KindNetwork
		case "sensitive_readable_paths", "unix_socket_detection":
			kind = containment.KindFile
		default:
			continue
		}
		var vals []string
		if err := json.Unmarshal(f.Value, &vals); err != nil {
			continue // value not a string list (shape drift) — leave in raw report
		}
		for _, v := range vals {
			out = append(out, containment.Observation{Kind: kind, Target: v, Reachable: true})
		}
	}
	return out, nil
}

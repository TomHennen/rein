package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/tests/containment"
)

func TestMapProbeReport(t *testing.T) {
	const (
		net   = `{"findingType":"external_host_connectivity","value":[]}`
		socks = `{"findingType":"unix_socket_detection","value":["/run/a.sock"]}`
		files = `{"findingType":"sensitive_readable_paths","value":["/etc/passwd","/fx/.gnupg","/fx/.ssh"]}`
	)
	write := func(findings ...string) string {
		p := filepath.Join(t.TempDir(), "report.json")
		if err := os.WriteFile(p, []byte(`{"findings":[`+strings.Join(findings, ",")+`]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	got, err := mapProbeReport(write(net, socks, files), map[string]bool{"/fx/.gnupg": true})
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, o := range got {
		rows = append(rows, string(o.Kind)+" "+o.Target)
	}
	// The empty dir is dropped; the non-empty one and the socket are kept.
	if want := "socket /run/a.sock,file /etc/passwd,file /fx/.ssh"; strings.Join(rows, ",") != want {
		t.Errorf("got %v, want %s", rows, want)
	}
	if got[0].Kind != containment.KindSocket || !got[0].Reachable {
		t.Errorf("socket row: %+v", got[0])
	}

	// A report that cannot prove the scan ran must fail, never read as clean.
	for name, path := range map[string]string{
		"empty report":       write(),
		"finding type gone":  write(net, files),
		"no liveness canary": write(net, socks, `{"findingType":"sensitive_readable_paths","value":[]}`),
		"value shape drift":  write(net, socks, `{"findingType":"sensitive_readable_paths","value":{"a":1}}`),
	} {
		if _, err := mapProbeReport(path, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := mapProbeReport(filepath.Join(t.TempDir(), "absent.json"), nil); err == nil {
		t.Error("missing report: accepted")
	}
}

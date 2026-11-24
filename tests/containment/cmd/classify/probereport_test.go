package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TomHennen/rein/tests/containment"
)

// A socket the in-sandbox probe connect-tested is left to that observation; one
// it missed must survive as a readable-file row so it fails as untriaged.
func TestMapProbeReportKeepsUnprobedSockets(t *testing.T) {
	report := filepath.Join(t.TempDir(), "report.json")
	body := `{"findings":[{"findingType":"unix_socket_detection","value":["/run/a.sock","/run/b.sock"]}]}`
	if err := os.WriteFile(report, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := mapProbeReport(report, map[string]bool{"/run/a.sock": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Target != "/run/b.sock" || got[0].Kind != containment.KindFile || !got[0].Reachable {
		t.Fatalf("want only the unprobed socket as a readable file row, got %+v", got)
	}
}

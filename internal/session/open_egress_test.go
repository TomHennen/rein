package session

import (
	"os"
	"strings"
	"testing"
)

// TestSetOpenEgressInFile: sets, replaces in place (comment kept), unsets.
func TestSetOpenEgressInFile(t *testing.T) {
	p := writeSession(t, "id: sess_x\nrole: implement\nrepos:\n  - o/r   # keep me\n")
	u, err := SetOpenEgressInFile(p, true)
	if err != nil || !u.OpenEgress {
		t.Fatalf("set on: %+v err=%v", u, err)
	}
	body, _ := os.ReadFile(p)
	if !strings.Contains(string(body), "open_egress: true") || !strings.Contains(string(body), "# keep me") {
		t.Errorf("file after set:\n%s", body)
	}
	u, err = SetOpenEgressInFile(p, false)
	if err != nil || u.OpenEgress {
		t.Fatalf("set off: %+v err=%v", u, err)
	}
	body, _ = os.ReadFile(p)
	if strings.Count(string(body), "open_egress") != 1 || !strings.Contains(string(body), "open_egress: false") {
		t.Errorf("file after unset (must replace in place, not duplicate):\n%s", body)
	}
}

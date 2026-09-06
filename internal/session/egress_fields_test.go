package session

import "testing"

func TestValidate_EgressFields(t *testing.T) {
	good := Session{ID: "x", Repos: []string{"o/r"}, OpenEgress: true, AllowInternalHosts: []string{"build.corp:443", "10.1.2.3:8080"}}
	if err := good.Validate(); err != nil {
		t.Errorf("valid egress fields rejected: %v", err)
	}
	for _, bad := range [][]string{{"build.corp"}, {":443"}, {"build.corp:0"}, {"build.corp:70000"}, {"build.corp:x"}} {
		s := Session{ID: "x", Repos: []string{"o/r"}, AllowInternalHosts: bad}
		if err := s.Validate(); err == nil {
			t.Errorf("allow_internal_hosts %v accepted", bad)
		}
	}
}

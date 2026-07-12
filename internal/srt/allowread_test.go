package srt

import (
	"reflect"
	"testing"
)

func TestParseSandboxAllowRead(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"whitespace only", "   ", nil, false},
		{"single", "/home/x/go", []string{"/home/x/go"}, false},
		{"colon list", "/home/x/go:/home/x/.nvm", []string{"/home/x/go", "/home/x/.nvm"}, false},
		{"trailing colon tolerated", "/home/x/go:", []string{"/home/x/go"}, false},
		{"double colon tolerated", "/a::/b", []string{"/a", "/b"}, false},
		{"cleaned", "/home/x//go/", []string{"/home/x/go"}, false},
		{"relative rejected", "go", nil, true},
		{"tilde rejected", "~/go", nil, true},
		{"relative among absolutes rejected", "/a:rel:/b", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSandboxAllowRead(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("ParseSandboxAllowRead(%q) accepted a non-absolute entry", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSandboxAllowRead(%q): %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseSandboxAllowRead(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestShowHomeFromEnv: only an explicit truthy value disables the home deny;
// everything else keeps the default-on protection (fail closed on garbage).
func TestShowHomeFromEnv(t *testing.T) {
	for _, on := range []string{"1", "true", "YES", " on "} {
		if !ShowHomeFromEnv(on) {
			t.Errorf("ShowHomeFromEnv(%q) = false, want true", on)
		}
	}
	for _, off := range []string{"", "0", "false", "off", "garbage", "2"} {
		if ShowHomeFromEnv(off) {
			t.Errorf("ShowHomeFromEnv(%q) = true, want false (default-on protection)", off)
		}
	}
}

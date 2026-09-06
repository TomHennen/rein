package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSandboxExecArgs(t *testing.T) {
	ports, argv, err := parseSandboxExecArgs([]string{"--expose", "5173", "--expose", "8080", "--", "claude", "-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ports, []int{5173, 8080}) || !reflect.DeepEqual(argv, []string{"claude", "-p", "hi"}) {
		t.Errorf("got ports=%v argv=%v", ports, argv)
	}
	for _, bad := range [][]string{
		{"--expose", "5173"},                   // no --
		{"--expose", "5173", "--"},             // no command
		{"--expose", "0", "--", "x"},           // bad port
		{"--expose", "70000", "--", "x"},       // bad port
		{"--expose", "--", "x"},                // missing value
		{"--other", "--", "x"},                 // unknown flag
		{"claude", "--expose", "1", "--", "x"}, // positional before --
	} {
		if _, _, err := parseSandboxExecArgs(bad); err == nil {
			t.Errorf("parseSandboxExecArgs(%v) accepted; want an error", bad)
		}
	}
}

// TestSandboxExecArgv: no exposed ports => the agent argv is launched
// UNCHANGED (no wrapper in the process tree); with ports, the staged binary's
// sandbox-exec wraps it and the agent argv survives verbatim after --.
func TestSandboxExecArgv(t *testing.T) {
	agent := []string{"claude", "--append-system-prompt", "contract text", "-p", "go"}
	if got := sandboxExecArgv("/tmp/run/rein", nil, agent); !reflect.DeepEqual(got, agent) {
		t.Errorf("no ports: argv changed: %v", got)
	}
	got := sandboxExecArgv("/tmp/run/rein", []int{5173}, agent)
	want := append([]string{"/tmp/run/rein", "sandbox-exec", "--expose", "5173", "--"}, agent...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wrapped argv = %v, want %v", got, want)
	}
	// Round trip through the parser the wrapper uses.
	ports, argv, err := parseSandboxExecArgs(got[2:])
	if err != nil || !reflect.DeepEqual(ports, []int{5173}) || !reflect.DeepEqual(argv, agent) {
		t.Errorf("round trip: ports=%v argv=%v err=%v", ports, argv, err)
	}
}

func TestExposePortsCSV(t *testing.T) {
	if got := exposePortsCSV(nil); got != "" {
		t.Errorf("nil => %q, want empty (env var must be ABSENT when nothing is exposed)", got)
	}
	if got := exposePortsCSV([]int{5173, 8080}); got != "5173,8080" {
		t.Errorf("got %q", got)
	}
}

func TestBuildAgentContract_PortsSectionTracksExposed(t *testing.T) {
	with := buildAgentContract(contractParams{WorkTree: "/work/repo", HomeEphemeral: true, ExposePorts: []int{5173}})
	for _, want := range []string{"PORTS", "5173 -> http://localhost:5173", "127.0.0.1:<port>", "No other port"} {
		if !strings.Contains(with, want) {
			t.Errorf("ports contract missing %q\n--- contract ---\n%s", want, with)
		}
	}
	without := buildAgentContract(contractParams{WorkTree: "/work/repo", HomeEphemeral: true})
	if strings.Contains(without, "PORTS") || strings.Contains(without, "localhost:") {
		t.Errorf("contract advertises forwarding with no exposed ports\n--- contract ---\n%s", without)
	}
}

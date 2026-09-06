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

// TestSandboxExecArgv: the staged binary's sandbox-exec ALWAYS wraps the agent
// argv (it carries the proxy secret into the child's env, #185); the agent
// argv survives verbatim after --, and each exposed port becomes a flag.
func TestSandboxExecArgv(t *testing.T) {
	agent := []string{"claude", "--append-system-prompt", "contract text", "-p", "go"}
	if got := sandboxExecArgv("/tmp/run/rein", nil, agent); !reflect.DeepEqual(got, append([]string{"/tmp/run/rein", "sandbox-exec", "--"}, agent...)) {
		t.Errorf("no ports: argv = %v", got)
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

// TestRewriteProxyEnv: every proxy URL srt set gains the secret as userinfo,
// nothing else is touched, and git's basic proxy auth is pre-set.
func TestRewriteProxyEnv(t *testing.T) {
	env := []string{
		"HTTPS_PROXY=http://localhost:3128", "https_proxy=http://localhost:3128",
		"HTTP_PROXY=http://localhost:3128", "ALL_PROXY=http://localhost:3128",
		"NO_PROXY=localhost,127.0.0.1", "HOME=/home/x", "REIN_PROXY_AUTH=abc",
	}
	got := rewriteProxyEnv(env, "abc")
	want := map[string]string{
		"HTTPS_PROXY": "http://srt:abc@localhost:3128", "https_proxy": "http://srt:abc@localhost:3128",
		"HTTP_PROXY": "http://srt:abc@localhost:3128", "ALL_PROXY": "http://srt:abc@localhost:3128",
		"GIT_CONFIG_PARAMETERS": "'http.proxyAuthMethod=basic'",
	}
	seen := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		seen[k] = v
	}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("rewriteProxyEnv = %v, want %v", seen, want)
	}
	if got := rewriteProxyEnv([]string{"HOME=/home/x"}, "abc"); len(got) != 0 {
		t.Errorf("no proxy vars => nothing to set, got %v", got)
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

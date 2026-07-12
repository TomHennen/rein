package main

import (
	"reflect"
	"testing"
)

func TestDropPushUpstreamFlag(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{[]string{"push", "-u", "origin", "agent/24/x"}, []string{"push", "origin", "agent/24/x"}},
		{[]string{"push", "--set-upstream", "origin", "b"}, []string{"push", "origin", "b"}},
		{[]string{"push", "origin", "HEAD:agent/24/x"}, []string{"push", "origin", "HEAD:agent/24/x"}}, // untouched
		{[]string{"push", "-u", "origin", "-u", "b"}, []string{"push", "origin", "b"}},                 // both
	}
	for _, c := range cases {
		got := dropPushUpstreamFlag(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("dropPushUpstreamFlag(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The strip must only apply to `push` — findSubcommand gates it in main().
func TestUpstreamStripGatedToPush(t *testing.T) {
	// a non-push command that happens to carry -u as an argument value must be
	// left alone (main() only calls dropPushUpstreamFlag when the subcommand is
	// push); assert the gate, not the filter.
	if findSubcommand([]string{"config", "--get", "-u"}) == "push" {
		t.Fatal("findSubcommand misidentified a non-push command as push")
	}
	if findSubcommand([]string{"push", "-u", "origin", "b"}) != "push" {
		t.Fatal("findSubcommand should identify push")
	}
}

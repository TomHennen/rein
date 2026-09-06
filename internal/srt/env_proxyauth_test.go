package srt

import (
	"strings"
	"testing"
)

func TestBuildEnv_ProxyAuthDelivered(t *testing.T) {
	env := BuildEnv(EnvParams{Parent: []string{"PATH=/bin"}, CABundlePath: "/ca.pem", StubGHToken: "x", ProxyAuth: "s3cr3t"})
	found := false
	for _, kv := range env {
		if kv == EnvProxyAuth+"=s3cr3t" {
			found = true
		}
	}
	if !found {
		t.Errorf("REIN_PROXY_AUTH missing from %v", env)
	}
	for _, kv := range BuildEnv(EnvParams{Parent: []string{"PATH=/bin"}, CABundlePath: "/ca.pem", StubGHToken: "x"}) {
		if strings.HasPrefix(kv, EnvProxyAuth+"=") {
			t.Errorf("REIN_PROXY_AUTH set without a secret: %s", kv)
		}
	}
}

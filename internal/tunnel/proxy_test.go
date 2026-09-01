package tunnel

import (
	"strings"
	"testing"
)

func TestProxyRouteForLogRedactsCredentials(t *testing.T) {
	got := ProxyRouteForLog("https://relay.example/relay/work", "http://employee:secret@proxy.example:9090")
	if got != "http://proxy.example:9090" {
		t.Fatalf("ProxyRouteForLog() = %q", got)
	}
}

func TestProxyRouteForLogDirect(t *testing.T) {
	if got := ProxyRouteForLog("https://relay.example/relay/work", "direct"); got != "direct" {
		t.Fatalf("ProxyRouteForLog() = %q", got)
	}
}

func TestProxyRouteForLogResolvesEnvironmentKeyword(t *testing.T) {
	got := ProxyRouteForLog("https://relay.example/relay/work", "env")
	if strings.EqualFold(got, "env") || strings.EqualFold(got, "auto") {
		t.Fatalf("ProxyRouteForLog() leaked configuration keyword %q", got)
	}
}

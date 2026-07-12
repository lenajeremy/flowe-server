package executor

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", // loopback
		"169.254.169.254",         // cloud metadata (link-local)
		"10.0.0.5", "192.168.1.1", // private v4
		"172.16.0.1",
		"0.0.0.0",            // unspecified
		"fd00::1", "fc00::1", // IPv6 unique-local
		"fe80::1", // IPv6 link-local
	}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"}
	for _, s := range allowed {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
	if !isBlockedIP(nil) {
		t.Error("nil IP should be blocked")
	}
}

func TestSSRFClientBlocksLoopback(t *testing.T) {
	t.Setenv("ALLOW_PRIVATE_HTTP", "") // guard active
	client := ssrfSafeClient(2 * time.Second)
	if _, err := client.Get("http://127.0.0.1:9/"); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected a 'blocked' error dialing loopback, got: %v", err)
	}
}

func TestSSRFClientAllowsWhenOptedOut(t *testing.T) {
	t.Setenv("ALLOW_PRIVATE_HTTP", "true") // dev escape hatch
	client := ssrfSafeClient(1 * time.Second)
	// Nothing is listening on :9, so we expect a connection error — but NOT our
	// "blocked" guard error, proving the guard stepped aside.
	if _, err := client.Get("http://127.0.0.1:9/"); err != nil && strings.Contains(err.Error(), "blocked request to internal") {
		t.Fatalf("guard should be disabled, but blocked: %v", err)
	}
}

package executor

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
)

// SSRF protection for the httpRequest node (and any node fetching a user-
// supplied URL). Requests to internal/reserved address ranges are refused so a
// workflow can't reach cloud metadata (169.254.169.254), the host's own
// datastores, or other services on the private network. The check runs in the
// dialer's Control callback — i.e. against the *resolved* IP at connect time —
// which also defeats DNS-rebinding and applies on every redirect hop.
//
// ALLOW_PRIVATE_HTTP=true disables the guard for local development (so nodes can
// hit localhost); it must stay unset in production.

func allowPrivateHTTP() bool { return os.Getenv("ALLOW_PRIVATE_HTTP") == "true" }

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		ip.IsPrivate() {
		return true
	}
	// Explicitly block the cloud metadata IPs (covered by link-local, but be
	// unambiguous) and IPv6 unique-local (fc00::/7).
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
	} else if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
		return true
	}
	return false
}

// ssrfSafeControl rejects the connection when the resolved address is internal.
func ssrfSafeControl(network, address string, _ syscall.RawConn) error {
	if allowPrivateHTTP() {
		return nil
	}
	if network != "tcp4" && network != "tcp6" && network != "tcp" {
		return fmt.Errorf("blocked network %q", network)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("blocked: unresolved address %q", address)
	}
	if isBlockedIP(ip) {
		return fmt.Errorf("blocked request to internal address %s", ip)
	}
	return nil
}

// ssrfSafeClient is an HTTP client that refuses to connect to internal ranges
// and caps redirects.
func ssrfSafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: ssrfSafeControl}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			return nil
		},
	}
}

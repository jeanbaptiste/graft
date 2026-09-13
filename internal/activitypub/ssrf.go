package activitypub

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// maxResponseBody caps how much of a remote response this package will
// ever read into memory — a hostile or misbehaving server (notably one
// controlled by whoever sent us a Follow) gets cut off rather than able to
// exhaust memory.
const maxResponseBody = 1 << 20 // 1 MiB

// ValidateExternalURL rejects non-https URLs and the most obvious
// local/loopback hostnames before any network call is made. A cheap first
// check ahead of the dialer-level one NewSafeClient performs — every
// outbound target here (a Follow's actor, an actor's inbox URL, ...) is
// attacker-influenced, so both checks matter: this one for a fast, clear
// rejection; the dialer check to close the DNS-rebinding gap a
// resolve-then-separately-connect check would otherwise leave open.
func ValidateExternalURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("only https URLs are allowed, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL has no host")
	}
	if host == "localhost" {
		return fmt.Errorf("refusing to connect to localhost")
	}
	if ip := net.ParseIP(host); ip != nil && isUnsafeIP(ip) {
		return fmt.Errorf("refusing to connect to disallowed address %s", ip)
	}
	return nil
}

// NewSafeClient returns an http.Client hardened for outbound requests to
// attacker-influenced URLs: no redirects are followed (a validated URL
// could otherwise redirect somewhere unvalidated), and every connection's
// actually-resolved IP is checked against private/loopback/link-local
// ranges immediately before dialing it — not just at an earlier
// validation step, which a DNS answer that changes between check and
// connect could bypass.
func NewSafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("resolve %s: %w", host, err)
			}
			var safe net.IP
			for _, ip := range ips {
				if isUnsafeIP(ip) {
					continue
				}
				safe = ip
				break
			}
			if safe == nil {
				return nil, fmt.Errorf("refusing to connect to %s: no public address resolved", host)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(safe.String(), port))
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func isUnsafeIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

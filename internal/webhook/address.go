// Package webhook delivers a finished job to a URL the client chose.
//
// That last clause is the whole difficulty. Everything else here is an HTTP POST
// with retries; the address check is what stops a transcription API from
// doubling as a request proxy into the network the server sits in.
package webhook

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// blocked are the ranges a webhook must never reach. Cloud metadata services
// live at 169.254.169.254 and hand out credentials to anything that asks, which
// is why link-local is on the list next to the obvious private ranges.
var blocked = []net.IPNet{
	mustCIDR("0.0.0.0/8"),      // "this host"
	mustCIDR("10.0.0.0/8"),     // RFC 1918
	mustCIDR("100.64.0.0/10"),  // CGNAT
	mustCIDR("127.0.0.0/8"),    // loopback
	mustCIDR("169.254.0.0/16"), // link-local, cloud metadata
	mustCIDR("172.16.0.0/12"),  // RFC 1918
	mustCIDR("192.0.0.0/24"),   // IETF protocol assignments
	mustCIDR("192.168.0.0/16"), // RFC 1918
	mustCIDR("198.18.0.0/15"),  // benchmarking
	mustCIDR("224.0.0.0/4"),    // multicast
	mustCIDR("240.0.0.0/4"),    // reserved
	mustCIDR("::1/128"),        // loopback
	mustCIDR("fc00::/7"),       // unique local
	mustCIDR("fe80::/10"),      // link-local
	mustCIDR("ff00::/8"),       // multicast
	mustCIDR("64:ff9b:1::/48"), // IPv4/IPv6 translation
}

// ::ffff:0:0/96 is deliberately absent. net.IPNet.Contains reduces a v4-mapped
// network to its four-byte form, and a /96 mask reduced the same way is
// 0.0.0.0 — the entry would match every IPv4 address, public ones included.
// checkIP normalises mapped addresses with To4 instead, which blocks
// ::ffff:127.0.0.1 through the 127.0.0.0/8 entry where it belongs.

func mustCIDR(s string) net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("webhook: bad CIDR " + s)
	}
	return *n
}

// Resolver looks up a host. Tests substitute their own; production uses the
// system resolver.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]net.IP, error)
}

type netResolver struct{}

func (netResolver) LookupNetIP(ctx context.Context, network, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, network, host)
}

// checkURL rejects anything a webhook must not reach.
//
// Every resolved address is checked, not just the first: a name that answers
// with one public address and one private one is the standard way around a
// check that stops at the head of the list.
func checkURL(ctx context.Context, resolver Resolver, raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webhook_url is not a URL: %w", err)
	}
	// Plain HTTP would put the transcript, and the signature proving it came
	// from here, on the wire in the clear.
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("webhook_url must be https, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("webhook_url has no host")
	}
	if allowPrivate {
		return nil
	}

	// A literal address needs no lookup, and looking one up would be a way to
	// get a different answer than the one that will be dialled.
	if ip := net.ParseIP(host); ip != nil {
		return checkIP(ip)
	}

	ips, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%s resolves to nothing", host)
	}
	for _, ip := range ips {
		if err := checkIP(ip); err != nil {
			return err
		}
	}
	return nil
}

func checkIP(ip net.IP) error {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for i := range blocked {
		if blocked[i].Contains(ip) {
			return fmt.Errorf("webhook_url resolves to %s, which is not a public address", ip)
		}
	}
	return nil
}

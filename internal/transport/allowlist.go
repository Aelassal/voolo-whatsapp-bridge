// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

// Package transport holds the one network path of the bridge: an HTTP
// transport that refuses, before connecting, any host outside a compiled-in
// list of WhatsApp hosts (PROTOCOL.md §10.2, ADR-012 S6). whatsmeow's three
// HTTP clients (pre-login websocket, websocket, media) and the WhatsApp Web
// version check all use it. Proxy environment variables are ignored.
package transport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// exactHosts and suffixHosts are the allowlist. They are constants of the
// program: nothing at run time can extend them.
var (
	exactHosts  = [...]string{"web.whatsapp.com"}
	suffixHosts = [...]string{".whatsapp.net", ".whatsapp.com"}
)

// AllowedPort is the only port the bridge dials (HTTPS and WSS).
const AllowedPort = "443"

// ErrHostBlocked is returned for any connection outside the allowlist.
var ErrHostBlocked = errors.New("host_blocked")

// HostAllowed reports whether a host name (no port) is on the allowlist.
// IP literals are never allowed: WhatsApp is always reached by name.
func HostAllowed(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" || net.ParseIP(strings.Trim(h, "[]")) != nil {
		return false
	}
	for _, e := range exactHosts {
		if h == e {
			return true
		}
	}
	for _, s := range suffixHosts {
		if strings.HasSuffix(h, s) && len(h) > len(s) && isHostname(h) {
			return true
		}
	}
	return false
}

func isHostname(h string) bool {
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return !strings.Contains(h, "..")
}

// AddrAllowed reports whether a dial address "host:port" is allowed.
func AddrAllowed(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	return err == nil && port == AllowedPort && HostAllowed(host)
}

// DialFunc is the signature of a context dialer.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Guard wraps a dialer so that it refuses disallowed addresses before any
// DNS lookup or connection. onBlock, if not nil, is told about each refusal.
func Guard(base DialFunc, onBlock func()) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			if onBlock != nil {
				onBlock()
			}
			return nil, ErrHostBlocked
		}
		if !AddrAllowed(addr) {
			if onBlock != nil {
				onBlock()
			}
			return nil, ErrHostBlocked
		}
		return base(ctx, network, addr)
	}
}

// Options configures New.
type Options struct {
	// Dial is the underlying dialer (tests inject a fake). Default: net.Dialer.
	Dial DialFunc
	// Lookup resolves a host name (tests inject a fake). Default: the system resolver.
	Lookup LookupFunc
	// OnBlock is called for each refused connection or redirect.
	OnBlock func()
}

// LookupFunc resolves a host name to its addresses.
type LookupFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

// Address ranges that are never dialed (review L14, R-L3). IPv4 addresses
// inside NAT64 and 6to4 addresses are held to the IPv4 list.
var (
	deniedV4 = prefixes(
		"0.0.0.0/8",       // "this network"
		"10.0.0.0/8",      // private (RFC 1918)
		"100.64.0.0/10",   // shared, carrier-grade NAT (RFC 6598)
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments (RFC 6890)
		"192.0.2.0/24",    // TEST-NET-1
		"192.168.0.0/16",  // private
		"198.18.0.0/15",   // benchmarking (RFC 2544)
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, and the broadcast address
	)
	// Inside the global unicast block 2000::/3 (everything outside it, such
	// as ::/96, 64:ff9b:1::/48, 100::/64, fc00::/7, fe80::/10, fec0::/10 and
	// ff00::/8, is refused by that rule alone).
	deniedV6 = prefixes(
		"2001::/23",     // IETF protocol assignments: Teredo 2001::/32, benchmarking, ORCHID
		"2001:db8::/32", // documentation
	)
	global6   = netip.MustParsePrefix("2000::/3")     // IANA global unicast; the rest is reserved
	nat64     = netip.MustParsePrefix("64:ff9b::/96") // RFC 6052: IPv4 in the last 32 bits
	sixToFour = netip.MustParsePrefix("2002::/16")    // RFC 3056: IPv4 in bits 16–47
)

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func inAny(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// PublicIP reports whether ip may be dialed. For IPv6 only the global
// unicast block 2000::/3 is dialed, which leaves out loopback, unspecified,
// IPv4-compatible, unique-local, link-local, site-local, multicast, local-use
// NAT64 and discard-only addresses; inside it, Teredo and the other IETF
// protocol assignments and documentation addresses are refused. For IPv4 it
// refuses loopback, private (RFC 1918), link-local, shared (RFC 6598),
// "this network", multicast, broadcast, benchmarking, documentation and other
// reserved addresses;
// for NAT64 (64:ff9b::/96), 6to4 (2002::/16) and IPv4-mapped addresses it
// judges the IPv4 address inside.
func PublicIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if a.Is6() {
		b := a.As16()
		switch {
		case nat64.Contains(a):
			a = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
		case sixToFour.Contains(a):
			a = netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})
		default:
			return global6.Contains(a) && !inAny(a, deniedV6)
		}
	}
	return !inAny(a, deniedV4)
}

// Resolve wraps a dialer so that an allowed name is resolved first and only
// its public addresses are dialed, by IP (review L14). A poisoned resolver
// that points a WhatsApp name at this machine or the local network is refused
// before any connection. TLS still verifies the certificate for the name.
func Resolve(lookup LookupFunc, base DialFunc, onBlock func()) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, ErrHostBlocked
		}
		ips, err := lookup(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			if !PublicIP(ip.IP) || (network == "tcp4" && ip.IP.To4() == nil) || (network == "tcp6" && ip.IP.To4() != nil) {
				continue
			}
			c, err := base(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return c, nil
			}
			lastErr = err
		}
		if lastErr != nil {
			return nil, lastErr
		}
		if onBlock != nil {
			onBlock()
		}
		return nil, ErrHostBlocked
	}
}

// NewTransport returns the guarded transport: names are checked against the
// allowlist, then resolved, and only public addresses are dialed.
func NewTransport(o Options) *http.Transport {
	base := o.Dial
	if base == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		base = d.DialContext
	}
	lookup := o.Lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	t := &http.Transport{
		Proxy:                 nil, // never read HTTP(S)_PROXY / ALL_PROXY
		DialContext:           Guard(Resolve(lookup, base, o.OnBlock), o.OnBlock),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return t
}

// CheckRedirect refuses a redirect to anything but https on an allowed host.
func CheckRedirect(onBlock func()) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" || !HostAllowed(req.URL.Hostname()) || (req.URL.Port() != "" && req.URL.Port() != AllowedPort) {
			if onBlock != nil {
				onBlock()
			}
			return ErrHostBlocked
		}
		return nil
	}
}

// NewClient returns an HTTP client on the guarded transport. It has no
// overall timeout because the websocket library requires that; requests use
// contexts instead.
func NewClient(o Options) *http.Client {
	return &http.Client{Transport: NewTransport(o), CheckRedirect: CheckRedirect(o.OnBlock)}
}

// NewMediaClient returns the client for media downloads and uploads: the
// guarded transport plus the body limit of LimitBody.
func NewMediaClient(o Options) *http.Client {
	return &http.Client{Transport: LimitBody(NewTransport(o)), CheckRedirect: CheckRedirect(o.OnBlock)}
}

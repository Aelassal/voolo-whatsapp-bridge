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

// PublicIP reports whether ip may be dialed: a global unicast address that is
// not loopback, private (RFC 1918, RFC 4193), link-local, shared (RFC 6598),
// unspecified, multicast or broadcast.
func PublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1]&0xc0 == 64 { // 100.64.0.0/10, carrier-grade NAT
			return false
		}
		if v4.Equal(net.IPv4bcast) || v4[0] == 0 {
			return false
		}
	}
	return true
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

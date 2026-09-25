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
	// OnBlock is called for each refused connection or redirect.
	OnBlock func()
}

// NewTransport returns the guarded transport.
func NewTransport(o Options) *http.Transport {
	base := o.Dial
	if base == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		base = d.DialContext
	}
	t := &http.Transport{
		Proxy:                 nil, // never read HTTP(S)_PROXY / ALL_PROXY
		DialContext:           Guard(base, o.OnBlock),
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

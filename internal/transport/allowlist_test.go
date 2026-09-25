// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package transport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestHostAllowed(t *testing.T) {
	allowed := []string{"web.whatsapp.com", "WEB.WhatsApp.com.", "mmg.whatsapp.net", "media-cai1-1.cdn.whatsapp.net", "g.whatsapp.net", "static.whatsapp.com"}
	refused := []string{
		"", "whatsapp.net", "whatsapp.com", "example.com", "github.com", "evilwhatsapp.net", "whatsapp.net.evil.com",
		"web.whatsapp.com.evil.com", "127.0.0.1", "::1", "[::1]", "157.240.1.1", "localhost", "a..whatsapp.net",
		"a_b.whatsapp.net", "xn--whatsapp.net", "web.whatsapp.co", "whatsapp.org", "fbcdn.net", "facebook.com",
	}
	for _, h := range allowed {
		if !HostAllowed(h) {
			t.Errorf("%q should be allowed", h)
		}
	}
	for _, h := range refused {
		if HostAllowed(h) {
			t.Errorf("%q should be refused", h)
		}
	}
	if !AddrAllowed("web.whatsapp.com:443") || AddrAllowed("web.whatsapp.com:80") || AddrAllowed("web.whatsapp.com:5222") || AddrAllowed("web.whatsapp.com") {
		t.Error("port rule broken")
	}
}

// A dial to a host outside the list fails before the underlying dialer (and
// so before DNS or any connection) is ever called.
func TestDialRefusedBeforeConnecting(t *testing.T) {
	var baseCalls, blocks atomic.Int32
	base := func(ctx context.Context, network, addr string) (net.Conn, error) {
		baseCalls.Add(1)
		return nil, errors.New("fake dialer reached")
	}
	dial := Guard(base, func() { blocks.Add(1) })
	for _, addr := range []string{"example.com:443", "127.0.0.1:443", "web.whatsapp.com:80", "localhost:443"} {
		if _, err := dial(context.Background(), "tcp", addr); !errors.Is(err, ErrHostBlocked) {
			t.Fatalf("%s: want ErrHostBlocked, got %v", addr, err)
		}
	}
	if _, err := dial(context.Background(), "udp", "web.whatsapp.com:443"); !errors.Is(err, ErrHostBlocked) {
		t.Fatal("udp should be refused")
	}
	if baseCalls.Load() != 0 {
		t.Fatalf("underlying dialer called %d times for refused hosts", baseCalls.Load())
	}
	if blocks.Load() != 5 {
		t.Fatalf("onBlock called %d times", blocks.Load())
	}
	_, _ = dial(context.Background(), "tcp", "web.whatsapp.com:443")
	if baseCalls.Load() != 1 {
		t.Fatal("allowed host should reach the underlying dialer")
	}
}

// The real client refuses a request to a local test server: the server never
// sees a request, whatever its address.
func TestClientRefusesOtherHosts(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := NewClient(Options{})
	for _, u := range []string{srv.URL, "https://example.com/", "http://web.whatsapp.com/"} {
		resp, err := c.Get(u)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s: request went through", u)
		}
		if !errors.Is(err, ErrHostBlocked) {
			t.Fatalf("%s: want ErrHostBlocked, got %v", u, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("test server was reached")
	}
}

// Redirects are checked with the same list.
func TestRedirectToOtherHostRefused(t *testing.T) {
	var blocks atomic.Int32
	check := CheckRedirect(func() { blocks.Add(1) })
	mk := func(s string) *http.Request { u, _ := url.Parse(s); return &http.Request{URL: u} }
	if err := check(mk("https://mmg.whatsapp.net/x"), nil); err != nil {
		t.Fatalf("allowed redirect refused: %v", err)
	}
	for _, s := range []string{"https://example.com/", "http://mmg.whatsapp.net/", "https://mmg.whatsapp.net:8443/", "https://127.0.0.1/"} {
		if err := check(mk(s), nil); !errors.Is(err, ErrHostBlocked) {
			t.Errorf("%s: want ErrHostBlocked, got %v", s, err)
		}
	}
	if blocks.Load() != 4 {
		t.Fatalf("onBlock = %d", blocks.Load())
	}
}

// Proxy environment variables are ignored: the transport has no Proxy func,
// so a proxy address never replaces the real destination.
func TestProxyEnvIgnored(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1")
	tr := NewTransport(Options{})
	if tr.Proxy != nil {
		t.Fatal("transport must not use a proxy")
	}
	var seen string
	base := func(ctx context.Context, network, addr string) (net.Conn, error) {
		seen = addr
		return nil, errors.New("stop")
	}
	c := NewClient(Options{Dial: base})
	_, _ = c.Get("https://web.whatsapp.com/")
	if seen != "web.whatsapp.com:443" {
		t.Fatalf("dialed %q, want the real destination", seen)
	}
}

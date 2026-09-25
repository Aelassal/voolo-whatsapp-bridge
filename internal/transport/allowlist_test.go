// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
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
	var seen, looked string
	base := func(ctx context.Context, network, addr string) (net.Conn, error) {
		seen = addr
		return nil, errors.New("stop")
	}
	lookup := func(ctx context.Context, host string) ([]net.IPAddr, error) {
		looked = host
		return []net.IPAddr{{IP: net.ParseIP("157.240.1.1")}}, nil
	}
	c := NewClient(Options{Dial: base, Lookup: lookup})
	_, _ = c.Get("https://web.whatsapp.com/")
	if looked != "web.whatsapp.com" || seen != "157.240.1.1:443" {
		t.Fatalf("resolved %q and dialed %q, want the real destination", looked, seen)
	}
}

// Review mutation mut-2b: the allowlist is exactly these names; adding any
// host, exact or suffix, is a reviewed change to this test.
func TestAllowlistIsPinned(t *testing.T) {
	if !reflect.DeepEqual(exactHosts[:], []string{"web.whatsapp.com"}) || !reflect.DeepEqual(suffixHosts[:], []string{".whatsapp.net", ".whatsapp.com"}) {
		t.Fatalf("allowlist changed: %v %v", exactHosts, suffixHosts)
	}
}

// Review L14: a WhatsApp name that resolves to a loopback, private,
// link-local, unspecified or other non-public address is refused before any
// connection; a public address is dialed by IP.
func TestResolvedAddressMustBePublic(t *testing.T) {
	var dials []string
	var blocks atomic.Int32
	base := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials = append(dials, addr)
		return nil, errors.New("fake dialer reached")
	}
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.10", "169.254.169.254", "0.0.0.0", "100.64.0.1",
		"::1", "fe80::1", "fc00::1", "::", "::ffff:127.0.0.1", "224.0.0.1", "255.255.255.255"} {
		lookup := func(ctx context.Context, host string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
		}
		dial := Guard(Resolve(lookup, base, func() { blocks.Add(1) }), nil)
		if _, err := dial(context.Background(), "tcp", "mmg.whatsapp.net:443"); !errors.Is(err, ErrHostBlocked) {
			t.Errorf("%s: want ErrHostBlocked, got %v", ip, err)
		}
	}
	if len(dials) != 0 {
		t.Fatalf("dialed %v", dials)
	}
	if blocks.Load() != 14 {
		t.Fatalf("onBlock = %d", blocks.Load())
	}
	// Mixed answers: the non-public ones are skipped.
	lookup := func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("157.240.1.1")}}, nil
	}
	dial := Guard(Resolve(lookup, base, nil), nil)
	_, _ = dial(context.Background(), "tcp", "mmg.whatsapp.net:443")
	if len(dials) != 1 || dials[0] != "157.240.1.1:443" {
		t.Fatalf("dialed %v", dials)
	}
}

type fakeRT struct {
	resp   func() *http.Response
	closed atomic.Bool
}

func (f *fakeRT) RoundTrip(r *http.Request) (*http.Response, error) {
	resp := f.resp()
	resp.Body = &closeFlag{ReadCloser: resp.Body, f: &f.closed}
	return resp, nil
}

type closeFlag struct {
	io.ReadCloser
	f *atomic.Bool
}

func (c *closeFlag) Close() error { c.f.Store(true); return c.ReadCloser.Close() }

// endless is a body that never ends.
type endless struct{ n atomic.Int64 }

func (e *endless) Read(p []byte) (int, error) { e.n.Add(int64(len(p))); return len(p), nil }

// Review M2: with a body limit in the request context, a declared
// Content-Length above it is refused without reading, and a body that turns
// out longer is cut off with an error: memory stays bounded whatever the
// server sends. Without a limit nothing changes.
func TestBodyLimit(t *testing.T) {
	get := func(rt http.RoundTripper, ctx context.Context) ([]byte, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://mmg.whatsapp.net/v/x", nil)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}
	limited := WithBodyLimit(context.Background(), 1000)

	declared := &fakeRT{resp: func() *http.Response {
		return &http.Response{StatusCode: 200, ContentLength: 2 << 30, Body: io.NopCloser(&endless{})}
	}}
	data, err := get(LimitBody(declared), limited)
	if !errors.Is(err, ErrBodyTooLarge) || len(data) != 0 || !declared.closed.Load() {
		t.Fatalf("declared 2 GiB: %d bytes, err %v, closed %v", len(data), err, declared.closed.Load())
	}

	stream := &endless{}
	undeclared := &fakeRT{resp: func() *http.Response {
		return &http.Response{StatusCode: 200, ContentLength: -1, Body: io.NopCloser(stream)}
	}}
	data, err = get(LimitBody(undeclared), limited)
	if !errors.Is(err, ErrBodyTooLarge) || len(data) > 1000 || stream.n.Load() > 1000+64*1024 {
		t.Fatalf("endless body: %d bytes kept, %d read, err %v", len(data), stream.n.Load(), err)
	}

	lying := &fakeRT{resp: func() *http.Response {
		return &http.Response{StatusCode: 200, ContentLength: 10, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 5000)))}
	}}
	if _, err := get(LimitBody(lying), limited); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("body longer than its Content-Length: %v", err)
	}

	exact := &fakeRT{resp: func() *http.Response {
		return &http.Response{StatusCode: 200, ContentLength: 1000, Body: io.NopCloser(bytes.NewReader(make([]byte, 1000)))}
	}}
	if data, err := get(LimitBody(exact), limited); err != nil || len(data) != 1000 {
		t.Fatalf("at the limit: %d %v", len(data), err)
	}
	big := &fakeRT{resp: func() *http.Response {
		return &http.Response{StatusCode: 200, ContentLength: 5000, Body: io.NopCloser(bytes.NewReader(make([]byte, 5000)))}
	}}
	if data, err := get(LimitBody(big), context.Background()); err != nil || len(data) != 5000 {
		t.Fatalf("no limit: %d %v", len(data), err)
	}
	if n, ok := BodyLimit(limited); !ok || n != 1000 {
		t.Fatal("BodyLimit")
	}
}

// The media client of the bridge applies the body limit.
func TestMediaClientHasBodyLimit(t *testing.T) {
	c := NewMediaClient(Options{})
	if _, ok := c.Transport.(*limitTransport); !ok {
		t.Fatalf("media client transport is %T", c.Transport)
	}
}

// Re-review R-L3: translated, tunnelled and reserved forms are refused by the
// resolved-address check, with the IPv4 inside NAT64 and 6to4 addresses held
// to the IPv4 rules; ordinary public addresses still pass.
func TestPublicIPReservedAndTranslated(t *testing.T) {
	cases := []struct {
		ip     string
		public bool
	}{
		// NAT64 (RFC 6052, RFC 8215): the embedded IPv4 decides; local-use is refused.
		{"64:ff9b::7f00:1", false}, // 127.0.0.1
		{"64:ff9b::a00:1", false},  // 10.0.0.1
		{"64:ff9b::c0a8:101", false},
		{"64:ff9b::9df0:101", true}, // 157.240.1.1
		{"64:ff9b:1::a00:1", false},
		{"64:ff9b:1::9df0:101", false},
		// 6to4 (RFC 3056): the embedded IPv4 decides.
		{"2002:7f00:1::1", false},
		{"2002:c0a8:101::1", false},
		{"2002:a00:1::1", false},
		{"2002:9df0:101::1", true},
		// Teredo, site-local, IPv4-compatible, documentation, discard-only.
		{"2001::1", false},
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", false},
		{"fec0::1", false},
		{"feff::1", false},
		{"::127.0.0.1", false},
		{"::9df0:101", false},
		{"2001:db8::1", false},
		{"100::1", false},
		{"4000::1", false}, // outside the global unicast block 2000::/3
		{"e000::1", false},
		// Reserved IPv4.
		{"0.1.2.3", false},
		{"192.0.0.1", false},
		{"192.0.0.170", false},
		{"192.0.2.1", false},
		{"198.18.0.1", false},
		{"198.19.255.255", false},
		{"198.51.100.7", false},
		{"203.0.113.9", false},
		{"240.0.0.1", false},
		{"254.1.2.3", false},
		{"255.255.255.255", false},
		// The same, IPv4-mapped.
		{"::ffff:198.18.0.1", false},
		{"::ffff:240.0.0.1", false},
		// Still public.
		{"157.240.1.1", true},
		{"31.13.64.1", true},
		{"198.17.255.255", true},
		{"198.20.0.1", true},
		{"192.0.3.1", true},
		{"2a03:2880:f001::1", true},
		{"2001:2::1", false}, // benchmarking, in the IETF block 2001::/23
		{"2001:200::1", true},
		{"2001:4860::8888", true},
	}
	for _, c := range cases {
		if got := PublicIP(net.ParseIP(c.ip)); got != c.public {
			t.Errorf("PublicIP(%s) = %v, want %v", c.ip, got, c.public)
		}
	}
}

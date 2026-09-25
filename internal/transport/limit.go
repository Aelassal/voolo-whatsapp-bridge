// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
)

// ErrBodyTooLarge is returned when a response body is longer than the limit
// carried by its request's context (review M2).
var ErrBodyTooLarge = errors.New("body_too_large")

type bodyLimitKey struct{}

// WithBodyLimit returns a context under which a response body read through
// LimitBody may be at most n bytes.
func WithBodyLimit(ctx context.Context, n int64) context.Context {
	return context.WithValue(ctx, bodyLimitKey{}, n)
}

// BodyLimit returns the limit set by WithBodyLimit.
func BodyLimit(ctx context.Context) (int64, bool) {
	n, ok := ctx.Value(bodyLimitKey{}).(int64)
	return n, ok
}

type limitTransport struct{ base http.RoundTripper }

// LimitBody wraps a transport so that, for a request whose context has a body
// limit, a response that declares a longer Content-Length is refused unread
// and a longer body is cut off with ErrBodyTooLarge. A download therefore
// never holds more than the limit in memory, whatever the sender declared or
// the server sends. Requests without a limit are passed through unchanged.
func LimitBody(rt http.RoundTripper) http.RoundTripper { return &limitTransport{base: rt} }

func (l *limitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	n, ok := BodyLimit(req.Context())
	if !ok {
		return resp, nil
	}
	if resp.ContentLength > n {
		// Refused unread. The error comes from the body, not from RoundTrip,
		// so the download is not retried as a network error.
		_ = resp.Body.Close()
		resp.Body = errBody{}
		return resp, nil
	}
	resp.Body = &limitedBody{rc: resp.Body, left: n}
	return resp, nil
}

type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, ErrBodyTooLarge }
func (errBody) Close() error             { return nil }

type limitedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// At the limit: one more byte means the body is too long.
		var one [1]byte
		n, err := b.rc.Read(one[:])
		if n > 0 {
			return 0, ErrBodyTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }

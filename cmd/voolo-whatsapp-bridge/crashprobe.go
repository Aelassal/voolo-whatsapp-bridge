// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

//go:build crashprobe

package main

import "time"

// A test-only build (go build -tags crashprobe) whose process panics in a
// goroutine the bridge does not own, 300 ms after start. Release builds never
// contain it. It proves that the runtime's crash output (the panic value and
// the stack) never reaches stderr (PROTOCOL.md §10.4).
func init() {
	crashProbe = func() {
		go func() {
			time.Sleep(300 * time.Millisecond)
			var key [4]uint64
			key[0] = 0x9f3ab7c2e81d4f06
			crashWith(key, "probe for 15550100002@s.whatsapp.net")
		}()
	}
}

//go:noinline
func crashWith(key [4]uint64, msg string) { panic(msg) }

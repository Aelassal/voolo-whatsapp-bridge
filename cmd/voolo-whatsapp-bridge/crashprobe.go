// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

//go:build crashprobe

package main

import (
	"os"
	"strings"
	"time"
)

// A test-only build (go build -tags crashprobe) whose process panics in a
// goroutine the bridge does not own, 300 ms after start. Release builds never
// contain it. It proves that the runtime's crash output (the panic value and
// the stack) never reaches stderr (PROTOCOL.md §10.4). VOOLO_CRASHPROBE picks
// the case: unset, a small panic; "big", a panic whose value is 1 MiB (the
// runtime's report is then far larger than any pipe buffer, review R-L1);
// "print", no panic but 1 MiB written straight to the process's stderr, as a
// library print would.
func init() {
	mode := os.Getenv("VOOLO_CRASHPROBE")
	crashProbe = func() {
		go func() {
			time.Sleep(300 * time.Millisecond)
			var key [4]uint64
			key[0] = 0x9f3ab7c2e81d4f06
			switch mode {
			case "big":
				crashWith(key, "probe for 15550100002@s.whatsapp.net "+strings.Repeat("x", 1<<20))
			case "print":
				_, _ = os.Stderr.WriteString(strings.Repeat("probe for 15550100002@s.whatsapp.net\n", 1<<20/38))
			default:
				crashWith(key, "probe for 15550100002@s.whatsapp.net")
			}
		}()
	}
}

//go:noinline
func crashWith(key [4]uint64, msg string) { panic(msg) }

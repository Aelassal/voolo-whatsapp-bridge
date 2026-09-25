// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

// Command voolo-whatsapp-bridge links to a WhatsApp account as a linked device
// and speaks the JSON-lines protocol of PROTOCOL.md on stdin and stdout.
package main

import (
	"os"

	"github.com/Aelassal/voolo-whatsapp-bridge/internal/store"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "0.0.0-dev"

// crashProbe is nil except in test builds with -tags crashprobe.
var crashProbe func()

func main() {
	store.RestrictUmask()
	// Diagnostics go to the original stderr; raw writes to fd 2 (the Go
	// runtime's crash output, library prints) go nowhere (review M1).
	diag := isolateStderr()
	out := guardStdout()
	if crashProbe != nil {
		crashProbe()
	}
	os.Exit(run(os.Args[1:], os.Stdin, out, diag, defaultDeps()))
}

// guardStdout returns the protocol stream and points os.Stdout at the null
// device: only protocol lines may reach stdout. Anything else in the process
// that writes to os.Stdout (a library print) goes nowhere.
func guardStdout() *os.File {
	out := os.Stdout
	if devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		os.Stdout = devnull
	}
	return out
}

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

func main() {
	store.RestrictUmask()
	out := os.Stdout
	// Only protocol lines may reach stdout. Anything else in the process that
	// writes to os.Stdout (a library print) goes to the null device instead.
	if devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		os.Stdout = devnull
	}
	os.Exit(run(os.Args[1:], os.Stdin, out, os.Stderr, defaultDeps()))
}

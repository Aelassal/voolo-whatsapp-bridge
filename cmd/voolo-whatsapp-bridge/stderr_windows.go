// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

//go:build windows

package main

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// isolateStderr keeps the original stderr handle for the bridge's own JSON
// diagnostics and makes the process's standard error a pipe that is read and
// thrown away, so the Go runtime's crash output (which it writes to the
// handle GetStdHandle returns at that moment) and library prints never reach
// the client (review M1). On any failure it returns os.Stderr unchanged.
func isolateStderr() *os.File {
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		return orig
	}
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(w.Fd())); err != nil {
		r.Close()
		w.Close()
		return orig
	}
	os.Stderr = w
	go func() {
		defer func() { _ = recover() }()
		_, _ = io.Copy(io.Discard, r)
	}()
	return orig
}

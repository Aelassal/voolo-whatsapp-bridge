// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// isolateStderr keeps the original stderr handle for the bridge's own JSON
// diagnostics and makes the process's standard error the null device (NUL),
// so the Go runtime's crash output (which it writes to the handle
// GetStdHandle returns at that moment) and library prints never reach the
// client (review M1). NUL never blocks, so a crash report of any size cannot
// hang the process (review R-L1). On any failure it returns os.Stderr
// unchanged.
func isolateStderr() *os.File {
	orig := os.Stderr
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return orig
	}
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(devnull.Fd())); err != nil {
		devnull.Close()
		return orig
	}
	os.Stderr = devnull
	return orig
}

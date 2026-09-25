// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

//go:build unix

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// isolateStderr keeps a private copy of stderr for the bridge's own JSON
// diagnostics and points file descriptor 2 at the null device. Whatever else
// writes to fd 2 never reaches the client: above all the Go runtime's output
// for an unrecovered panic or a fatal error, which holds the panic value and
// stack frames (review M1; GOTRACEBACK cannot be lowered from inside the
// program). The null device never blocks, so a crash report of any size
// cannot hang the process while the runtime has stopped every goroutine; it
// still exits with code 2 (review R-L1). On any failure it returns os.Stderr
// unchanged.
func isolateStderr() *os.File {
	fd, err := unix.Dup(2)
	if err != nil {
		return os.Stderr
	}
	unix.CloseOnExec(fd)
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		unix.Close(fd)
		return os.Stderr
	}
	defer devnull.Close()
	if err := unix.Dup2(int(devnull.Fd()), 2); err != nil {
		unix.Close(fd)
		return os.Stderr
	}
	return os.NewFile(uintptr(fd), "stderr")
}

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

//go:build unix

package main

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// isolateStderr keeps a private copy of stderr for the bridge's own JSON
// diagnostics and points file descriptor 2 at a pipe that is read and thrown
// away. Whatever else writes to fd 2 never reaches the client: above all the
// Go runtime's output for an unrecovered panic or a fatal error, which holds
// the panic value and stack frames (review M1; GOTRACEBACK cannot be lowered
// from inside the program). The process then still exits with code 2. On any
// failure it returns os.Stderr unchanged.
func isolateStderr() *os.File {
	fd, err := unix.Dup(2)
	if err != nil {
		return os.Stderr
	}
	unix.CloseOnExec(fd)
	r, w, err := os.Pipe()
	if err != nil {
		unix.Close(fd)
		return os.Stderr
	}
	if err := unix.Dup2(int(w.Fd()), 2); err != nil {
		r.Close()
		w.Close()
		unix.Close(fd)
		return os.Stderr
	}
	w.Close()
	go func() {
		defer func() { _ = recover() }()
		_, _ = io.Copy(io.Discard, r)
	}()
	return os.NewFile(uintptr(fd), "stderr")
}

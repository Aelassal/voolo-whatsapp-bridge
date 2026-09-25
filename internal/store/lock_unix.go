// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

//go:build unix

package store

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive, non-blocking OS lock on path. The lock is held
// until release is called or the process exits.
func lockFile(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// RestrictUmask makes every file this process creates owner-only (0600/0700),
// including SQLite's -wal and -shm files and media hand-off files.
func RestrictUmask() { unix.Umask(0o077) }

// restrictMode sets owner-only permissions on an existing path.
func restrictMode(path string, dir bool) error {
	mode := os.FileMode(0o600)
	if dir {
		mode = 0o700
	}
	return os.Chmod(path, mode)
}

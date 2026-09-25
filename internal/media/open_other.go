// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

//go:build !unix

package media

import "os"

// openNoFollow opens path for reading. Windows has no O_NOFOLLOW: Open
// compares the handle with the checked file (os.SameFile) instead.
func openNoFollow(path string) (*os.File, error) { return os.Open(path) }

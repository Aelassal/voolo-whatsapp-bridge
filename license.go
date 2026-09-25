// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

// Package voolowhatsappbridge holds repository-level files that the program
// embeds, such as its licence text for --license.
package voolowhatsappbridge

import _ "embed"

// License is the full text of the GNU General Public License v3.0 (LICENSE).
//
//go:embed LICENSE
var License string

// SourceRepo is the public repository that holds the source of every release.
const SourceRepo = "https://github.com/Aelassal/voolo-whatsapp-bridge"

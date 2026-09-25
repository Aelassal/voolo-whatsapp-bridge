#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 The voolo-whatsapp-bridge authors
#
# The licence check ignores one module whose licence the classifier does not
# know (licenses/README.md). The exception holds only for the exact version
# and LICENSE file that were reviewed: any other version, or a changed LICENSE,
# fails here until it is reviewed again.
set -euo pipefail

module="github.com/ncruces/go-sqlite3-wasm/v6"
reviewed_version="v6.3.35304"
reviewed_sha256="13219037ddf63dbbcf174bf59525d602df7a2e30083f63be566715c858fcb19e"

go mod download "$module"
version="$(go list -m -f '{{.Version}}' "$module")"
dir="$(go list -m -f '{{.Dir}}' "$module")"
if [ "$version" != "$reviewed_version" ]; then
  echo "licence exception: $module is $version, reviewed was $reviewed_version" >&2
  exit 1
fi
sum="$(sha256sum "$dir/LICENSE" | cut -d' ' -f1)"
if [ "$sum" != "$reviewed_sha256" ]; then
  echo "licence exception: $module@$version LICENSE changed ($sum)" >&2
  exit 1
fi
echo "licence exception ok: $module@$version LICENSE $sum"

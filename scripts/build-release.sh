#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 The voolo-whatsapp-bridge authors
#
# Builds the four release binaries reproducibly and writes SHA256SUMS.
#   scripts/build-release.sh <version without v> <output dir>
# Same inputs (source tree, Go version) give the same bytes: no cgo, -trimpath,
# no VCS stamping, an empty build id, stripped symbols.
set -euo pipefail

version="${1:?version}"
out="${2:?output dir}"
case "$version" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "version must be MAJOR.MINOR.PATCH" >&2; exit 1 ;;
esac

mkdir -p "$out"
export CGO_ENABLED=0 GOFLAGS=-mod=readonly GOTOOLCHAIN=local
for target in windows/amd64 darwin/arm64 darwin/amd64 linux/amd64; do
  goos="${target%/*}"
  goarch="${target#*/}"
  ext=""
  [ "$goos" = windows ] && ext=".exe"
  name="voolo-whatsapp-bridge_${version}_${goos}_${goarch}${ext}"
  GOOS="$goos" GOARCH="$goarch" go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -X main.version=${version}" \
    -o "$out/$name" ./cmd/voolo-whatsapp-bridge
done
(cd "$out" && sha256sum voolo-whatsapp-bridge_* > SHA256SUMS)
cat "$out/SHA256SUMS"

# voolo-whatsapp-bridge

A small command-line program that links to a WhatsApp account as a **linked device** and speaks a simple, documented protocol on **stdin/stdout**: one JSON object per line. It keeps its session encrypted in a local file and talks to nothing but WhatsApp's own servers.

It is built on [whatsmeow](https://github.com/tulir/whatsmeow). Anyone can use it: a shell, a script or a desktop app can pair it, read chats and history, and send messages one at a time, using only [`PROTOCOL.md`](PROTOCOL.md).

> **Status (2026-09-25):** first implementation (task P1-A), not yet released and not yet tried on a real account. `PROTOCOL.md` and `examples/` are the agreed interface.

## Licence

- The program is licensed under the **GNU General Public License v3.0** (`LICENSE`). It links whatsmeow (MPL-2.0) and `go.mau.fi/libsignal` (GPL-3.0), so the program as a whole is GPL-3.0. Each release publishes a `go-licenses` report listing every linked module and its licence.
- `PROTOCOL.md` and every file in `examples/` are dedicated to the public domain under **CC0-1.0**, so that any program, under any licence, can implement the protocol from them. The dedication is at the top of each of those files.
- `voolo-whatsapp-bridge --license` prints the licence, and `--source` prints the URL of the exact source tag the binary was built from.

## How Voolo uses it

[Voolo](https://github.com/Aelassal) is a separate desktop app, closed source, that shows WhatsApp next to email in one inbox. The two programs stay separate:

- **Separate program, separate repository.** This repository contains no Voolo code, and Voolo contains none of this repository's code. The only interface between them is the protocol in `PROTOCOL.md`, over the program's stdin and stdout.
- **Downloaded, not bundled.** Voolo's installer does not include this program. The first time a user links WhatsApp in Voolo, Voolo downloads the release binary for that platform from this repository's GitHub Releases, checks its size and SHA-256 against the values pinned in Voolo's own source, and runs it as a child process. A new bridge version reaches Voolo users only through a Voolo update that changes the pin.
- **Local only.** The session store and every message stay on the user's computer. The program connects only to WhatsApp hosts, enforced by a compiled-in allowlist. Voolo keeps the store's encryption key in the operating system's keychain and hands it to the bridge on stdin when the program starts. The key never appears in arguments, environment variables, files or logs.
- **Conservative by design.** The protocol has no bulk, broadcast, scheduled or templated sending: one message, to one chat, at a time. The bridge never marks you online, never downloads media unless asked, and never exports your address book. Voolo adds its own limits on top: a consent screen, a send cap, pauses when WhatsApp signals throttling, and a remote pause switch (`flags.json`, below).

### `flags.json`

This repository also publishes a small signed file, used as a remote pause switch by clients such as Voolo: `https://github.com/Aelassal/voolo-whatsapp-bridge/releases/download/flags/flags.json`. It sits on a prerelease tagged `flags` that is not a program release. The format and the Ed25519 public key are in `PROTOCOL.md` §11. The bridge itself never fetches it.

## Building

Requirements: Go 1.26 or newer (releases use Go 1.27.1). No C compiler is needed: the store uses a pure-Go SQLite, [`github.com/ncruces/go-sqlite3`](https://github.com/ncruces/go-sqlite3), with its Adiantum encryption VFS.

```sh
# local build for this machine
CGO_ENABLED=0 go build -trimpath -o voolo-whatsapp-bridge ./cmd/voolo-whatsapp-bridge

# the four release binaries and SHA256SUMS, exactly as the release workflow builds them
# (CGO_ENABLED=0, -trimpath, -buildvcs=false, -ldflags "-s -w -buildid= -X main.version=$V")
scripts/build-release.sh 0.1.0 dist

go test ./...          # nothing in the tests connects to WhatsApp: a fake stands in for it
go test -race ./...    # needs a C compiler for the race detector only
```

Releases are built by GitHub Actions (`.github/workflows/release.yml`) from a tag `vX.Y.Z`. Each release publishes one binary per platform (win-x64, mac-arm64, mac-x64, linux-x64), `SHA256SUMS`, the `go-licenses` report and a build-provenance attestation. A second job rebuilds from the tag and compares the hashes, so a release is reproducible from its source. OS code signing (Authenticode, Developer ID + notarization) will be added later.

## Trying it from a shell

See `PROTOCOL.md` §12. In short: start the program, read its `hello` line, send an `init` line with a key and folders, send `pair_qr`, turn the `qr` code into a QR image with any tool, scan it with your phone in WhatsApp → Settings → Linked devices, and watch `paired`, `chat` and `message` lines arrive.

## Layout

```
cmd/voolo-whatsapp-bridge/   main: stdio loop, handshake, flags --version/--license/--source
cmd/flags-sign/              signs flags.json (used only by the flags workflow)
internal/protocol/           envelope, strict command decoding, event encoding
internal/store/              encrypted SQLite store, file lock, bridge tables
internal/transport/          host allowlist for every HTTP/WebSocket connection
internal/wa/                 whatsmeow client, event mapping, pairing, sending
internal/history/            history batches and flow control
internal/media/              media hand-off files and size caps
internal/logx/               content-free stderr diagnostics
licenses/                    go-licenses report of the linked modules
scripts/build-release.sh     reproducible build of the four release binaries
examples/                    one example line per message type (CC0)
.github/workflows/           ci (every push), release (tags v*), flags (manual, protected)
```

## Disclaimer

This is not an official WhatsApp client and is not affiliated with WhatsApp or Meta. WhatsApp's terms apply to your use of WhatsApp. Linked-device tools may lose access at any time, and in rare cases accounts that use unofficial tools have been restricted.

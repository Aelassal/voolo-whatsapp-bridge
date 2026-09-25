# Linked modules and their licences

`go-licenses.csv` is the output of `go-licenses report ./cmd/voolo-whatsapp-bridge` (github.com/google/go-licenses v1.6.0, a build-time tool that is not linked into the program) for the pinned `go.mod`. The report is the same for `GOOS=linux`, `windows` and `darwin` (`CGO_ENABLED=0`). Each release regenerates it in CI and publishes it next to the binaries.

The program as a whole is **GPL-3.0-or-later**: it links `go.mau.fi/libsignal` (GPL-3.0). Every other linked module is under a licence compatible with GPL-3.0:

| Licence | Modules |
|---|---|
| GPL-3.0 | go.mau.fi/libsignal, this program |
| MPL-2.0 (no Exhibit B) | go.mau.fi/whatsmeow, go.mau.fi/util |
| MIT | github.com/ncruces/go-sqlite3, github.com/ncruces/julianday, lukechampine.com/adiantum, github.com/beeper/argo-go, github.com/vektah/gqlparser/v2, github.com/elliotchance/orderedmap/v3, github.com/rs/zerolog, github.com/mattn/go-colorable, github.com/mattn/go-isatty |
| MIT-0 | github.com/ncruces/go-sqlite3-wasm/v6 (SQLite itself is public domain) |
| ISC | github.com/coder/websocket |
| Apache-2.0 | github.com/petermattis/goid |
| BSD-3-Clause | filippo.io/edwards25519, github.com/google/uuid, golang.org/x/{crypto,exp,net,sync,sys,text}, google.golang.org/protobuf |

## Reviewed exception

`github.com/ncruces/go-sqlite3-wasm/v6` shows as `Unknown` in the report because the tool's classifier does not know **MIT No Attribution (MIT-0)**. Its `LICENSE` file (v6.3.35304) was read by hand on 2026-09-25: it is MIT-0, a permissive licence without even an attribution condition, compatible with GPL-3.0. CI's licence check ignores this one module by name; any other `Unknown` fails the build. The exception is pinned (review L8): `scripts/check-licence-exception.sh`, run by CI and the release before the licence tool, fails unless the module is exactly `v6.3.35304` and its `LICENSE` has the reviewed SHA-256 `13219037ddf63dbbcf174bf59525d602df7a2e30083f63be566715c858fcb19e`. A new version is reviewed again before the pin moves.

# P1-A security review: `voolo-whatsapp-bridge` (branch `p1-a`)

- **Scope:** `git diff main...p1-a`, 5 commits (`e107eef`, `1315806`, `36dceb9`, `031c078`, `9882fbd`), 46 files, +8,976/-35. I read every non-test source file in full, plus the tests that guard the controls below.
- **Context read:** README.md, PROTOCOL.md, examples/, Voolo `CLAUDE.md` (non-negotiables 1–3 and 7), `docs/specs/p1-whatsapp-bridge.md`, ADR-017, ADR-010, ADR-012 (from `voolo-p1b`, read-only).
- **Focus (handoff §6):** session-key storage, the app-to-sidecar channel, sending. I also covered network, history and media, and CI/flags.
- **Reviewer:** an independent security-review session that did not write this code. I changed no code. The only commit is this file.
- **Rules kept:** I never connected to WhatsApp, used no phone number, did not run the release or flags workflows, and pushed nothing. Every runtime network test ran under `strace` with `connect`/`sendto`/`sendmsg` failures injected, so no packet left the machine. Mutations were applied one at a time and reverted with `git checkout`. The tree is clean on `p1-a`.

## Verdict

**Not ready to merge as-is: 1 High, 6 Medium, 14 Low.** The core controls are sound and have real tests behind them: the key-only-from-`init` rule, Adiantum encryption of the db/WAL/journal, the host allowlist and ignored proxies, strict command decoding, pre-init gating, single in-flight sends and the persisted `outboxId`. The High is a crash that is easy to trigger: shutting down, or logging out, while history sync is running. Its trace goes to stderr as raw Go output, and its exit code is 2, which the protocol reserves for "no init". The Mediums are about defence in depth and test coverage around the stated guarantees.

---

## Checks I ran myself

Go 1.27.1 (`$HOME/.local/go1.27.1/bin`), `GOFLAGS=-p=2`.

```
$ gofmt -l .                      → (empty)
$ go vet ./...                    → vet-ok
$ go mod verify                   → all modules verified
$ GOFLAGS=-p=2 go test -p 2 -race -count=1 ./...
?   	github.com/Aelassal/voolo-whatsapp-bridge	[no test files]
ok  	github.com/Aelassal/voolo-whatsapp-bridge/cmd/flags-sign	1.016s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/cmd/voolo-whatsapp-bridge	1.922s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/history	1.681s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/logx	1.014s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/media	1.013s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol	1.106s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/store	22.270s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/transport	1.011s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/wa	4.456s
```

**Cross-compile** (`scripts/build-release.sh 0.0.0`, `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false`, `-buildid=`):
```
235b4be705833e7c46267cd96b6a3d6f81e9f8b503fe75284da4863216ea3fb1  voolo-whatsapp-bridge_0.0.0_darwin_amd64
bd7c7e472139702f8c389a0f4ab004c0f295079d3472558230d6fa0ca15ab40a  voolo-whatsapp-bridge_0.0.0_darwin_arm64
4e1fd36adc027caa793f77d900dbcd5a32b9ca72edb585ef794496268b22af5e  voolo-whatsapp-bridge_0.0.0_linux_amd64
ee25f2e9ee1455550163dcfa4e19ffbe8a1f7f6eba1b271ba9796d722907b13a  voolo-whatsapp-bridge_0.0.0_windows_amd64.exe
```
**Reproducibility:** I rebuilt after `go clean -cache` → identical `SHA256SUMS`. I also rebuilt from a `git archive p1-a` copy at a different absolute path → identical `SHA256SUMS`. Sizes are 19.3–20.4 MB.

**Licences:** `go-licenses check ./cmd/voolo-whatsapp-bridge --allowed_licenses=GPL-3.0,MPL-2.0,MIT,ISC,Apache-2.0,BSD-2-Clause,BSD-3-Clause --ignore github.com/ncruces/go-sqlite3-wasm/v6` → exit 0.
- Without `--ignore` it fails only on `go-sqlite3-wasm/v6`, whose LICENSE I read ("MIT No Attribution License").
- A fresh `go-licenses report` matches `licenses/go-licenses.csv` exactly: 2 GPL-3.0, 2 MPL-2.0, 10 MIT, 9 BSD-3, 1 ISC, 1 Apache-2.0, 1 Unknown (MIT-0).

**Fuzzing:** I added a temporary harness, since removed, in `internal/protocol`:
- `FuzzEnvelopeAndCommands`, 90 s, 4.43 M execs. Invariants: no panic; an accepted command never has an unknown or `null` member; key and chat invariants hold. **No failures.**
- `FuzzLineReader`, 45 s, 2.89 M execs. Invariants: no line over `max`, no embedded `\n`. **No failures.**
- A directed probe found two strictness gaps (L2, L11).

**Runtime checks on the release binary (linux/amd64):**
- **File modes.** `init` with a random key and the shell umask at 022 gives `store/` 0700; `store.db`, `-wal`, `-shm` and `bridge.lock` 0600; `media/` 0700.
- **Encryption at rest.** After exit, the only file left is `store.db`, which starts with random bytes and contains no readable text (`strings` finds nothing matching sqlite, whatsmeow, voolo or CREATE).
- **Wrong key.** Gives `error{store_key_invalid, fatal:true}` and exit **3**. The `store.db` SHA-256 is identical before and after.
- **Second bridge on the same store.** Gives `store_locked` and exit **4**.
- **No `init`.** `init_timeout` after 10.006 s, exit **2**.
- **Pre-init commands.** `send_text` and `ack` before `init` get `not_initialized`. `v:2` gets `unsupported_version`. Garbage is dropped and counted.
- **Where the key goes.** `strace -s 4096` of the whole run (`openat`, `write`, `pwrite64`, `unlinkat`, `mkdirat`) shows the key never written to any file descriptor, and no file opened outside `storeDir`, `mediaDir`, `/dev/null`, `/proc` and `/sys`. There are no `/tmp` or temp files: SQLite opens only `store.db`, `-journal`, `-wal` and `-shm`, all with `O_NOFOLLOW`.
- **Proxy variables ignored.** `pair_qr` with `HTTPS_PROXY`, `HTTP_PROXY` and `ALL_PROXY` pointing at `127.0.0.1:9`, and every connect injected to fail: the only `connect()` calls were DNS to `127.0.0.53:53`. There were no attempts to the proxy and no real connection. The stderr lines were format-only (`"Client: Initial connection failed but reconnecting in background (%v)"`).

---

## Findings

### High

**H1. Shutdown or logout during history sync crashes the bridge: nil `*sql.DB`, an unrecovered goroutine, a raw Go trace on stderr, and exit 2.**
Where: `internal/wa/bridge.go:385-416` (`Stop` closes the store at :402 *before* waiting for goroutines at :410), `internal/store/store.go:199` and `:212` (`s.DB = nil`, written without synchronisation while other goroutines read `s.DB`), `internal/wa/bridge.go:364-367` (`historyLoop` goroutine with no `recover`), `internal/wa/historysync.go:220` and `:233` (`markReported`/`PutMedia` after the context check), and `bridge.go:436` (`Wipe` on logout, same pattern).

- **Reproduced.** I wrote a temporary test with 3 conversations of 20,000 history messages, dispatched the notification, waited 20 ms and called `Stop()`. The result:
  ```
  panic: runtime error: invalid memory address or nil pointer dereference
  database/sql.(*DB).conn(0x0, ...)
  ...store.(*Store).AddChats(...)   store.go:349
  ...wa.(*Bridge).markReported(...) events.go:214
  ...wa.(*Bridge).historyConversation(...) historysync.go:220
  ```
- **Why it matters:**
  - Quitting Voolo during the first history sync, which takes minutes on a real account, is the normal case.
  - The same race exists on `logout` or a remote `logged_out` during sync: `Wipe` sets `DB=nil`. The process then dies halfway through `resetAfterLogout`, so `logged_out`, `status` and `ok` may never be written.
  - Stderr gets a non-JSON Go trace (breaks PROTOCOL §10.4). The exit code is **2**, which PROTOCOL §9 defines as "no `init` within 10 s", so Voolo's supervisor will misclassify it. Shutdown does not exit 0 (breaks §6.11).
  - The async command paths (`fetch_media`, `send_*` after a failed reopen) hit the same nil `DB`. `recover` catches those, but no final reply is sent. In `beginSend` a panic inside `ReserveOutbox` happens before `release` exists, so the send slot stays taken and every later send gets `busy`.
- **Fix:**
  1. In `Stop` and `resetAfterLogout`: cancel the context, disconnect, then wait for `b.wg` (bounded), and only then `Close`/`Wipe`. Register `historyLoop` in the same wait.
  2. Make `Store` safe after close. Guard `DB` with a `sync.RWMutex` or an `atomic.Pointer`, never set it to nil, and return `ErrClosed` from every method after `Close`/`Wipe`.
  3. Wrap `historyLoop` (and every other bridge-owned goroutine) in the same `recover` as `async`.
  4. Add a regression test: `Stop()` and `logout` during a large history sync, then assert exit 0, JSON-only stderr and no panic, under `-race`.

### Medium

**M1. Content-free stderr and exit codes are not guaranteed on crash paths.**
Where: `cmd/voolo-whatsapp-bridge/run.go:70-75` (the `recover` covers only the main goroutine), `run.go:111-120` (stdin reader goroutine, no `recover`), `internal/wa/bridge.go:29` (`ExitNoInit = 2`).

- **Leaks.** Any unrecovered panic in a bridge goroutine or a whatsmeow goroutine is printed by the Go runtime straight to fd 2, with the panic value in full. A scratch demo shows this, and also that by-value array arguments (key material, if a crypto routine takes a `[N]byte` by value) are printed as hex words:
  ```
  panic: bad node for 15550100002@s.whatsapp.net
  main.use({0x1111111111111111, 0x2222222222222222, 0x3333333333333333, 0x4444444444444444}, ...)
  exit=2
  ```
  With `GOTRACEBACK=none` the frames are gone, but the `panic:` line still prints. whatsmeow has format-string panics that include values, for example `binary/encoder.go:281`/`:307` (`'%s'` of the string being packed).
- **Exit-code collision.** The Go runtime exits with **2** on every unrecovered panic and every `fatal error`, which is the same number as `ExitNoInit`.
- **Fix:**
  - Call `debug.SetTraceback("none")` first in `main`, since Voolo spawns with a minimal environment.
  - Add `recover` wrappers on every goroutine the bridge starts.
  - Move "no init" to an unused code (for example 6). The protocol is unreleased, so it can still change.
  - Record in PROTOCOL §10.4/§9 that any stderr line which is not §10.4 JSON must be dropped by the client. Voolo's sidecar manager must parse stderr strictly, keep only allow-listed `event` names and drop everything else, counting it. It should treat exit 2 as a crash.

**M2. `fetch_media` can be made to exhaust memory: the size cap trusts sender-declared metadata.**
Where: `internal/wa/commands.go:268` (the cap is checked against `d.SizeBytes`, which is `FileLength` from the *sender's* message), `:286` (whatsmeow `DownloadMediaWithPath` does an unbounded `io.ReadAll(resp.Body)` in `download.go` `downloadMedia`, with up to 5 network retries per host and a fallback across hosts), `:294` (the real size is checked only *after* the whole body is in memory).

- **Attack.** A malicious contact sends an `imageMessage` or a voice note that declares 20 KB but whose `directPath`/`fileEncSHA256` point to a 1–2 GB blob they uploaded. When Voolo fetches it, which it will do to display the image or play the voice note, the bridge buffers the whole blob (plus a decryption copy). On an 8 GB laptop this kills the process.
- **Test gap.** Removing the post-download check (mutation mut-17) **survived**: nothing tests the real size.
- **Fix:**
  - Give the media client a `RoundTripper` that, for GETs to media hosts, refuses a `Content-Length` above `cap + 10 (MAC) + 16 (padding)` and wraps `Body` in a reader that errors past that cap. `cap` is the per-kind limit the bridge already knows at fetch time; pass it through the request context.
  - Or use `DownloadMediaWithPathToFile` into a size-capped sink.
  - Add a test with a fake download larger than the cap and a small declared size.

**M3. Logout reopens the store with the *same* key, so the old store is not crypto-shredded.**
Where: `internal/wa/bridge.go:420-459` (`resetAfterLogout` wipes, then `OpenStore(ctx, init.StoreDir, key)` with the old key), PROTOCOL.md:239 (§6.4 "opens a new, empty store with the same key, so it can be linked again without a restart").

- **Problem.** ADR-017 §2 says that after unlinking, Voolo deletes the keychain entry and "a later link generates a fresh key". Adiantum is deterministic and has no per-file salt, and `os.Remove` does not overwrite. So if a client re-links in the same process (which PROTOCOL invites), the key that decrypts the old session's pages (Signal identity, sessions, pre-keys, LID map, contacts, `voolo_*` tables) stays in use and in the keychain. Leftover sectors on SSDs, snapshots and backups (Time Machine, VSS) of the old file stay decryptable.
- **Fix:**
  - After `logout` or `logged_out`, do not reopen. Report `status{stopped}` and require a restart. `init` cannot be repeated, so either the bridge exits with a defined code after the reply, or PROTOCOL states that the client must restart it with a **new** key.
  - Change PROTOCOL §6.4 to match, and make P1-B/P1-C generate a new keychain entry on relink.
  - Add a test asserting that no store is reopened under the old key.

**M4. The "never retried" guarantee has a test gap and an undocumented transport-level resend.**
Where: `internal/wa/commands.go:86`, `internal/wa/bridge_test.go:577-595`, PROTOCOL.md:247 and :350.

- **Test gap.** Mutation mut-3b (retry `SendMessage` once, but only when the error maps to `timeout`) **survived**. The only no-retry test injects a 429.
- **Hidden resend.** whatsmeow `SendMessage` itself resends the same encrypted frame through `retryFrame` (`send.go:449`, `request.go:185`) when the websocket drops while it waits for the ack and reconnects within 5 s. That is the same message id, so the server should deduplicate it. It is idempotent, not a second user-visible message, but it is a retry the protocol text says never happens. Retry receipts (a recipient's device failed to decrypt) also re-encrypt and resend the same id from `recentMessagesMap`, which is standard behaviour for the protocol.
- **Fix:**
  - Add no-retry assertions (`sendCalls == 1`) for `timeout`, `send_failed`, `not_connected` and a context cancel, for both `send_text` and `send_media`, the upload included.
  - State both whatsmeow behaviours in PROTOCOL §6.5/§10.3: same id, provider-level, never a new message.

**M5. Flags-signing and release pipeline: the environment protections are weaker than ADR-017 §5 assumes.**
Where: `.github/workflows/flags.yml:29` plus live repository settings, which I read with `gh api`, read-only:
- `environments/flags`: `deployment_branch_policy: null`, `can_admins_bypass: true`, `prevent_self_review: false`. The secret is correctly environment-scoped: `FLAGS_ED25519_PRIVATE_KEY` is present in `flags` and there are no repository-level secrets.
- `rulesets`: `[]`, so no tag protection on `v*`.

**Risk:**
- `workflow_dispatch` can run from **any branch**, and it runs that branch's `flags.yml` and `cmd/flags-sign`. A malicious or mistaken branch could print or exfiltrate the key once the run is approved. The approval screen does not show a diff.
- This is the S4 pause switch. Whoever holds the key can re-enable Deep sync after the owner has paused it: `issuedAt` monotonicity helps only against replay, not against a newly signed file.
- In `release.yml:20`/`:36` the `build` job holds `id-token: write` and `attestations: write` while it `go install`s and runs a third-party tool (`go-licenses`) and the whole test suite, so compromised tool code could mint provenance for other files.

**Fix:**
- Set the `flags` environment's deployment branches to `main` only (a protected branch), disable admin bypass, and add `if: github.ref == 'refs/heads/main'` to the `publish` job.
- Add a tag ruleset for `v*` and `flags`: creation restricted, no deletion or update.
- Move the licence report and the tests into a job without `id-token`, and let only the attest step's job hold `id-token: write`.

**M6. The bridge has no send-rate floor or cap. Single in-flight is the only brake.**
Where: `internal/wa/commands.go:50-80`. ADR-017 §6 deliberately keeps S1–S8 in Voolo main.

- The bridge is also a public, standalone program ("anyone can use it"). With `unknown_chat` limited to reported chats and joined groups, a script (or a Voolo main bug, such as a loop over the outbox) can still send back-to-back to every known chat and group as fast as WhatsApp acks, roughly 1–3 per second. That is the bulk and automated pattern non-negotiable 3 forbids, and it is the most likely way to get the owner's account banned.
- **Fix:** add a compiled-in backstop that clients cannot configure, for example at least 2 s between sends and at most 30 per 10 minutes. Refuse beyond it with `rate_limited` (retryable) and do not reserve the `outboxId`. Record it in PROTOCOL §6.5 and ADR-017 §6 as a fourth defence-in-depth check. Add a test.

### Low

**L1. The stderr scrubber can pass keys through, and the tests would not notice.**
Where: `internal/logx/logx.go:104`/`:112`.

- `ScrubCode` redacts only `@`, `+` and runs of 5 or more digits. I simulated 100,000 random 32-byte keys in hex: **6.0 %** have no 5-digit run and would pass through `logx.Code(...)` verbatim. Pairing codes and message ids also pass through.
- The `wa` harness key is `000…001`, so the digit rule always redacts it and `TestStderrIsContentFree` can never catch a key leak through `Code()`. Mutation mut-4b was caught only by the cmd test's `abab…` key.
- An event name built from key characters (mutation mut-4a, `"init_"+key[:40]`) matches `eventRe` and **survived**.

Fix: redact any `[0-9A-Fa-f]{16,}` and base64-looking runs of 16 or more. Replace `eventRe` with a compiled set of the fixed event names. Use a random key with letters in the `wa` tests and also assert that no 16-character substring of it appears.

**L2. Invalid UTF-8 is silently altered, not refused.**
Where: `internal/protocol/commands.go:168`/`:259`. `send_text` with raw `\xff`, or the escape `\ud800`, is **accepted**: `encoding/json` replaces it with U+FFFD before `textOK`'s `utf8.ValidString` runs. So the check never fails, and the user's text is changed before it is sent. This contradicts §2's "strict". Fix: `utf8.Valid(line)` on the raw line, plus rejecting lone-surrogate escapes, with `bad_request`.

**L3. `mark_read` is not scoped to reported chats.**
Where: `internal/wa/commands.go:223-248`. `mark_read` sends read receipts for any syntactically valid chat and ids, including chats the bridge never reported. In groups it does not check that the ids belong to `senderJid`. Fix: `knownChat` check, `unknown_chat` otherwise.

**L4. `send_media` file handling.**
Where: `internal/wa/commands.go:139-153`, `internal/media/media.go:106-136`.
- **Not deleted "in every case".** The file is not deleted when the command is refused before `media.Open` (`busy`, `unknown_chat`, `duplicate_outbox_id`, `not_connected`), against §6.6.
- **TOCTOU.** `Lstat` then `os.Open(path)` follows a symlink swapped in between. Low impact: the content must still pass GCM under the client's key.
- **Truncation panic.** A file truncated after `Lstat` makes `data[:12]` panic. It is recovered, but no reply is sent.

Fix: validate the path, then `defer os.Remove` at the top of `sendMedia`; open with `O_NOFOLLOW`, `f.Stat()` the handle, and check `len(data) >= Overhead`.

**L5. Permissions defence in depth.**
Where: `internal/wa/bridge.go:287`, `internal/store/lock_unix.go:38`, `lock_windows.go:37-40`.
- An existing `mediaDir` is never tightened.
- Removing `RestrictUmask` (mutation mut-11) **survived**, because the chmod loop in `open()` masks it. The `-journal` that SQLite creates with 0666 at first creation relies on the umask.
- Windows sets no ACL at all and depends on `%APPDATA%` inheritance.

Fix: `restrictMode(mediaDir)`, a test without `RestrictUmask`, and a protected owner-plus-SYSTEM DACL on `storeDir`/`mediaDir` through `SetNamedSecurityInfo` on Windows.

**L6. The outbox reservation may not survive a power loss.**
Where: `internal/store/store.go:119`. `synchronous=NORMAL` in WAL mode survives a process crash, but a power loss can roll back the last commits, so a reservation made just before dispatch can vanish, and the same `outboxId` could send twice. Voolo's own outbox is the main guard. Fix: `PRAGMA synchronous=FULL`; writes are rare enough that the cost is small.

**L7. The stdout guard is untested.**
Where: `cmd/voolo-whatsapp-bridge/main.go:23`. Removing `os.Stdout = devnull` (mutation mut-19) **survived**. Fix: a subprocess test in which a library print to `os.Stdout` never reaches the protocol stream.

**L8. The licence exception is by module path, not version.**
Where: `.github/workflows/ci.yml:38`. `--ignore github.com/ncruces/go-sqlite3-wasm/v6` would also hide a future v6 release that changed its licence. Fix: in CI, compare the SHA-256 of that module's `LICENSE` with the reviewed one.

**L9. PROTOCOL.md:56 uses a plausible real Egyptian mobile number** (`201001234567`), also in tests. Anyone copying the shell example would trigger a link notification on a stranger's phone. Fix: use the fictional `1555…` numbers used elsewhere.

**L10. Duration caps rely on declared values.**
Where: `internal/wa/commands.go:294`, `internal/media/media.go:214`. `fetch_media` rechecks the size but not the duration after download: the duration is the sender's declared `seconds`. `OggDurationSeconds` returns 0 for non-Ogg data, so a `voice` `send_media` whose content is not Ogg skips the duration cap. Fix: for voice, refuse mime types other than Ogg/Opus, or refuse `secs == 0`.

**L11. The ULID check accepts values that overflow 128 bits.**
Where: `internal/protocol/envelope.go:32`. The first character may be `8`–`Z`, so `ZZZZ…` is accepted. Fix: first character `[0-7]`.

**L12. PROTOCOL accuracy on provider behaviour:**
- §5.4: `connect_failure` for 500/503 is never emitted, because whatsmeow reconnects on its own without a `ConnectFailure` event.
- §6.2: a pairing socket failure is reported as `pair_failed{reason: timeout}` within about 60 ms. I observed this at runtime; the text says `error{pair_failed}`.
- §10.3: "no address book … read" is not literally true. whatsmeow's app-state sync stores the whole contact list in the (encrypted) store; it is only not *reported*.

Fix the text or the behaviour.

**L13. A flaky test under load.** `TestMarkRead` timed out once (5 s `expect`) while the machine was loaded during the mutation runs. It passed 8/8 on a rerun with `-race -count=8`. Worth making `initPairedConnected` wait on a condition instead of a timer.

**L14. The dialer does not check the resolved address.**
Where: `internal/transport/allowlist.go:73-89`. The allowlist is name-based. A poisoned resolver can point `*.whatsapp.net` at a loopback or LAN address, and TLS verification still stops interception. Fix, optional: in `DialContext`, resolve and refuse loopback, private, link-local and unspecified addresses before connecting.

---

## Adversarial ideas tried

| Area | Attack | Result |
|---|---|---|
| Key | Key in argv, env or a file; key in stderr or stdout during wrong-key, locked, timeout or normal runs | Not present (runtime `strace` and output greps) |
| Key | Key written to stderr (mutations mut-4a–b) | mut-4b caught; mut-4a survived (L1) |
| Store | Skip `PRAGMA hexkey` (mut-1); wrong key recreates the store (mut-20) | Caught / caught |
| Store | Plaintext in db, WAL or shm; temp or sort spill files | None. `temp_store=MEMORY` (removing it is caught, mut-12); no temp files opened at runtime |
| Store | Logout leaves `-wal`/`-shm` behind (mut-18) | Survived, but equivalent: SQLite deletes the WAL on close. See finding M3 for the real logout problem |
| Stdio | Fuzzed envelope and commands; duplicate or case-variant keys; `null`; fractional or exponent integers; over-long lines; trailing NUL | Refused (L2 aside) |
| Stdio | Commands before `init` (mut-5) | Caught. At runtime: `not_initialized` |
| Stdio | whatsmeow log arguments formatted into stderr (mut-4c) | Caught |
| Stdio | Unrecovered panic → stderr and exit code | Leaks the value, exit 2 (finding M1, H1) |
| Sending | Remove single-flight (mut-7); reserve the outbox after dispatch (mut-6); remove the `unknown_chat` check (mut-10) | Caught |
| Sending | Retry on any error (mut-3a) / only on timeout (mut-3b) | Caught / **survived** (M4) |
| Sending | Bulk, schedule or template fields; lists as `chatJid` | Refused (`no_bulk_test`, fuzz invariant) |
| Media | Symlink (mut-8); file not deleted (mut-9); size cap after download removed (mut-17); declared-small, really-huge download | Caught / caught / **survived** / unbounded (M2) |
| Network | Allowlist uses `Contains` (mut-2a); port check dropped (mut-2c); proxy from env (mut-2d); `http` redirect allowed (mut-15) | Caught |
| Network | Extra non-WhatsApp exact host (mut-2b) | **Survived.** The test lists only specific refused names. Add a test that pins the allowlist to exactly `{web.whatsapp.com}` + `{.whatsapp.net, .whatsapp.com}` |
| Network | Proxy env at runtime; connections other than DNS | None (`strace`, injected failures) |
| History | Per-chat cap off (mut-13); ack window off (mut-14) | Caught |
| Permissions | Umask off (mut-11) | Survived (L5) |
| Stdout | Stdout guard removed (mut-19) | Survived (L7) |

**Mutation summary:** 25 mutations applied, one at a time, each reverted (a further `mark_read` edit turned out to be a no-op and is not counted). **18 caught, 7 survived:** mut-2b, mut-3b, mut-4a, mut-11, mut-17, mut-19 and mut-18. mut-18 is equivalent in behaviour, as noted above, and not counted as a gap. All five mutations the task required are caught by the suite: skip hexkey (mut-1), a non-allowlisted host (mut-2a/2c/2d; mut-2b is the gap), retry (mut-3a; mut-3b is the gap), key to stderr (mut-4b; mut-4a is the gap), drop pre-init gating (mut-5). The survivors are listed in the findings. Labels: `mut-N` are mutations; H1, M1–M6 and L1–L14 are findings.

## What held up

- **Key custody.** The key arrives only in `init` (a `^[0-9a-f]{64}$` check), is applied as `PRAGMA hexkey` in the per-connection hook, is never in the URI, and never reaches any file descriptor. A wrong key gives exit 3 and leaves the store byte-identical. With no key the store is refused. The lock gives exit 4.
- **Encryption at rest.** Adiantum covers the main db, WAL and journal (the `-shm` holds only the WAL index). There are no temp files at runtime and `temp_store=MEMORY` is set. There is no plaintext marker in db, `-wal` or `-shm` (the test plus my runtime check). The whatsmeow `sqlstore` upgrade runs unchanged on `ncruces`.
- **Channel.** Strict decoding: exact member names, no duplicates or nulls, integer-only numbers, required and unknown fields in nested `limits`. The check order is envelope → `v` → type → init → payload. The 1 MiB line cap has correct discard and count. Stdin EOF gives a clean exit 0 (outside H1). The init timeout exits 2 at 10 s. `hello` comes first. There is no socket or port.
- **stderr in normal operation.** Fixed event names, format-only whatsmeow lines (every whatsmeow `Warnf`/`Errorf` format is a literal; I grepped them all), and no content in any run I made.
- **Network.** One guarded transport for all three whatsmeow clients and the version check (tested). No default client is used anywhere in the paths that run. Port 443 only, names only, no IP literals, redirects rechecked, proxies ignored (confirmed at runtime).
- **Sending.** Single in-flight (CAS). The `outboxId` is reserved before dispatch and survives restarts. Checks run in the documented order. `unknown_chat` limits sends to reported chats and joined groups. The command set is exactly the v1 eleven, with no bulk, schedule or template surface. There is no presence anywhere: whatsmeow sends presence only through `SendPresence`, which the `Client` interface does not expose. `AutomaticMessageRerequestFromPhone` defaults to false. There is no `fetch_history` peer request.
- **Media hand-off.** A fresh key, nonce and name per file, AES-256-GCM, mode 0600 with `O_EXCL`, plaintext never on disk, stale files cleaned at start, the path rule plus a regular-file check, and files deleted after `Open`.
- **History.** The S5 day and per-chat caps and the ack window of 4 are enforced and tested, and live events are not blocked by it.
- **Builds and licences.** Reproducible across cache and path. All action SHAs match their tags (checked with `gh api`). `persist-credentials: false` is set. The workflow inputs pass through `env`, with no expression injection. The flags key is environment-scoped. `flags-sign` refuses a key that does not match the published `keyId` and self-verifies. Licences are GPL-compatible, with the MIT-0 exception reviewed.

## Required before merge

1. Fix H1 and add the shutdown/logout-during-history regression test.
2. M1: `SetTraceback("none")`, goroutine `recover` wrappers, and a new code for "no init".
3. M2: a size-capped media download, with a test.
4. M3: a decision on relinking with a fresh key (a PROTOCOL §6.4 change).
5. M4: the no-retry tests and the PROTOCOL wording.
6. M5: the environment branch policy and the tag ruleset. These are settings plus a small workflow change, and they do not block the code merge, but they must be in place before the first `flags` run.
7. M6: agree with the lead whether the bridge-side send backstop goes in now, or record the decision in ADR-017 §6.

Low findings: fix them now or write them into the P1-A card with an owner and a date.

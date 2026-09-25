# P1-A security re-review: `voolo-whatsapp-bridge` (branch `p1-a`, fix commit `ef61223`)

- **Scope:** the whole of `ef61223` ("fix: P1-A security review findings"), 48 files, +2,516/-285, read against `docs/reviews/P1-A-security-review.md` (H1, M1–M6, L1–L14). I read every changed non-test file in full, plus the new tests (`internal/wa/review_test.go`, `cmd/voolo-whatsapp-bridge/handshake_test.go`, `umask_linux_test.go`, `internal/store/store_test.go`, `internal/transport/allowlist_test.go`, `internal/logx/logx_test.go`, `workflows_test.go`). I also read the whatsmeow code the fixes depend on (`download.go`, `message.go`, `client.go`) and the Go 1.27.1 runtime's Windows stderr path (`runtime/os_windows.go` `write1`).
- **Reviewer:** an independent security re-review session. It did not write the code, the fix or the first review. I changed no code; this file is the only commit.
- **Rules kept:**
  - I never connected to WhatsApp and used no real phone number.
  - I did not run the release or flags workflows, and I did not tag or push.
  - The one runtime run of the release binary was unpaired (so it never dials) and ran under `strace` with `connect`/`sendto`/`sendmsg` failures injected. It made no `connect` call.
  - Mutations and scratch tests ran in a copy of the repository in the scratchpad (`p1a-rr/repo`), each reverted with `git checkout`. The real checkout was never modified and is clean on `p1-a`.
  - I did not run `go clean -cache`. I did not touch `voolo-p1b`; I read ADR-017 with `git show` from the Voolo repository.

## Verdict

**H1 and M1–M6 are closed, and each fix has a test that fails when the fix is removed.** The first review's reproductions no longer fail. Every mutation that survived in the first review is now caught.

**No Blocker, no High. 1 new Medium, 9 new Lows.** The Medium is a gap in the H1 shutdown ordering. If a bridge goroutine outlives Stop's 1.5 s wait, it can still write history content to stdout after `logged_out` and `status {stopped}`. The Lows cover:
- the stderr pipe can hang the process on a large crash report (admitted in §14; worse on Windows);
- a forged trailing Ogg page defeats the voice-duration check;
- NAT64/6to4 gaps in the resolved-IP check;
- backstop behaviour when the clock goes back;
- residual repository settings;
- test gaps shown by ten surviving mutations;
- two small leftovers.

The Medium can be fixed without a new review cycle; it does not block the merge in the validation phase. It should be closed before the first release.

---

## Checks I ran myself

Go 1.27.1 (`$HOME/.local/go1.27.1/bin`), `GOFLAGS=-p=2`.

```
$ go version                       → go version go1.27.1 linux/amd64
$ gofmt -l .                       → (empty)
$ go vet ./...                     → vet-linux-ok
$ GOOS=windows go vet ./...        → vet-win-ok
$ GOOS=darwin go vet ./...         → vet-darwin-ok
$ go mod verify                    → all modules verified
$ go test -p 2 -race -count=1 ./...
ok  	github.com/Aelassal/voolo-whatsapp-bridge	1.011s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/cmd/flags-sign	1.017s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/cmd/voolo-whatsapp-bridge	3.581s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/history	1.696s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/logx	28.706s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/media	1.013s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol	1.115s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/store	23.643s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/transport	1.012s
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/wa	7.510s
$ go test -race -count=10 -run 'TestMarkRead|TestH1|TestM3|TestM6|TestL3|TestL4' ./internal/wa/
ok  	github.com/Aelassal/voolo-whatsapp-bridge/internal/wa	9.904s      (L13: no flake in 10 runs)
```

**Cross-compile and reproducibility.** I ran `scripts/build-release.sh 0.0.0` (a copy of the script with `-p=2` added to its `GOFLAGS`) from the checkout, then again from a `git archive p1-a` copy at another absolute path. Both gave identical `SHA256SUMS`, and the asset names match §13:
```
8f7d71e5c0a5cd6abc26118facb5f3db97c5d9c1422bbc95f54c60d3358b9438  voolo-whatsapp-bridge-darwin-amd64
9971d2fe32754d9f7384a02ba1e8e5d5ff0026fa7164bd6924e68caf3da88a3d  voolo-whatsapp-bridge-darwin-arm64
6e50a34bd914efef45beb7f769b63cb06a128ca1b966a4789514cb77dc11d06f  voolo-whatsapp-bridge-linux-amd64
084f8f3901db552ec632561b81d367916fe64ee9edb07ebcb1382bc01ce15b70  voolo-whatsapp-bridge-windows-amd64.exe
REPRODUCIBLE
```

**Licences.**
- `scripts/check-licence-exception.sh` → `licence exception ok: github.com/ncruces/go-sqlite3-wasm/v6@v6.3.35304 LICENSE 13219037…fcb19e`, exit 0.
- `go-licenses check ./cmd/voolo-whatsapp-bridge --allowed_licenses=GPL-3.0,MPL-2.0,MIT,ISC,Apache-2.0,BSD-2-Clause,BSD-3-Clause --ignore github.com/ncruces/go-sqlite3-wasm/v6` → exit 0.
- A fresh `go-licenses report` matches `licenses/go-licenses.csv` line for line.

**Repository settings (read-only `gh api`).**
- `environments/flags`: `can_admins_bypass: false`; a `required_reviewers` rule (Aelassal); `deployment_branch_policy: {protected_branches: false, custom_branch_policies: true}` with one branch policy, `main`. There are no repository-level secrets.
- `rulesets`: one active ruleset, "release tags", on `refs/tags/v*`, with rules `deletion`, `non_fast_forward` and `update`, `bypass_actors: []`.
- `branches/main/protection` → 404 "Branch not protected", and `rules/branches/main` → `[]`. See R-L5.

**Runtime checks on the release binary** (`voolo-whatsapp-bridge-linux-amd64`, shell umask 022, under `strace` with network syscalls failing):

| Check | Result |
|---|---|
| Handshake | `hello` → `init` (random key; `mediaDir` created beforehand at 0755) → `ready {paired:false}` → `status {unpaired}` |
| Process state | `/proc/<pid>/fd/2` is `pipe:[…]` (the M1 isolation); `Umask: 0077` |
| Invalid input | `send_text` with `\ud800` escaped → `bad_request` |
| Shutdown | `shutdown` → `status {stopped}`, `ok`, exit **0** |
| stderr | JSON lines only, all with listed events |
| File modes | `store/` and `media/` 0700 (media tightened from 0755); `store.db` and `bridge.lock` 0600 |
| Network | 0 `connect` calls; `dup3(7, 2)` is the stderr redirection |
| No `init` | exit **6** after 10.06 s |

**Crash-probe runs** (`-tags crashprobe`, with the probe changed in the scratch copy to panic with a large value):
- A panic value of up to 100 KB exits **2** in 0.41 s, and nothing but the `started` line reaches stderr.
- **A 130 KB or 200 KB panic value hangs the process** until it is killed (`timeout` exit 124). See R-L1.
- A runtime `fatal error: concurrent map writes` exits 2 with nothing leaked.

**Fuzzing.** `FuzzOgg` (temporary, in the scratch copy) against `media.OggDurationSeconds`: 45 s, 5.43 M execs, no panic and no negative result. A directed test found R-L2.

---

## Findings

### Blocker
None.

### High
None.

### Medium

**R-M1. After Stop gives up waiting, a history goroutine can still write the old account's content to stdout, after `logged_out` and `status {stopped}`.**
Where:
- `internal/wa/bridge.go:448-453`: Stop waits at most 1.5 s, then closes or wipes and writes the final lines anyway.
- `internal/history/history.go:39-47`: `Acquire` hands out a seq without looking at `ctx` whenever the window has room.
- `internal/wa/historysync.go:248-257`: `history_batch` and `reaction` are emitted with no `ctx` check.
- `historysync.go:189-217`: the per-message parse loop never checks `ctx`.

- **Reproduced** (scratch test `TestRR_LogoutTimeoutLateOutput`). A history notification is being processed and one `ParseWebMessage` takes 3 s (a stand-in for a slow parse of a 20,000-message conversation, or a whatsmeow call that ignores cancellation). A remote `LoggedOut` arrives. Stop logs `stop_timeout` after 1.50 s, wipes the store and writes `logged_out` and `status {stopped}`. After that, the bridge writes to stdout:
  ```
  lines after logged_out: [status {"state":"stopped"} history_batch {"seq":1,"syncType":"initial","chat":{"jid":"15550300000@s.whatsapp.net",…]
  ```
  The same happens on a plain shutdown (`TestRR_StopTimeoutThenLateOutput`: `history_batch` after `status {stopped}`).
- **Why it matters:**
  - PROTOCOL §6.4/§5.9 now promise that after a logout the store is deleted and `logged_out`, `status {stopped}` and `ok` are the last words.
  - A client that has just deleted the chats on the user's "unlink and delete" choice can receive and store a fresh batch of the unlinked account's messages.
  - This is the exact H1 scenario (logout during history sync). The crash is gone, but the ordering the fix claims ("waits for every bridge goroutine, and only then…") does not hold past 1.5 s.
  - Live events are correctly gated: `onEvent` returns once `s.ctx` is cancelled (`events.go:25`; `TestRR_EventAfterStop` writes nothing). Only the history worker leaks.
- **Fix:**
  1. `Window.Acquire`: return `ctx.Err()` first when `ctx` is done, before the room check.
  2. In `historyConversation`: check `ctx.Err()` in the message loop (for example every 256 messages) and before each emit of `history_batch`, `reaction`, `sync_progress`, contact and group lines.
  3. Defence in depth: give the Bridge a `quiesced` flag, set by Stop before it waits. From then on `emit` drops every event except the ones Stop writes itself (`error {store_io}`, `logged_out`, `status {stopped}`, `ok`).
  4. Add a regression test in which a parse hook blocks past the Stop timeout, and assert that nothing follows `status {stopped}`.

### Low

**R-L1. The stderr sink is a pipe, so a large crash report hangs the process instead of exiting 2 (M1 residual; PROTOCOL §14 admits it).**
Where: `cmd/voolo-whatsapp-bridge/stderr_unix.go:245-260`, `stderr_windows.go:284-297`.

- **What happens.** During a fatal panic the runtime freezes the world, including the draining goroutine. Once the pipe buffer and the one read in flight are full, the runtime's `write(2)` blocks for ever.
- **Measured on Linux:** a panic value of 100 KB exits 2; 130 KB or more hangs.
- **Windows is worse.** `os.Pipe` there uses `CreatePipe(…, 0)`, the default buffer of a few KiB, and the runtime writes through `GetStdHandle(STD_ERROR_HANDLE)` (`runtime/os_windows.go:562-572`). A deep whatsmeow stack, or a wrapped error value, is enough. Isolation on Windows is compile-checked only.
- **Consequence.** A hung bridge keeps `bridge.lock`, so a restart gets `store_locked` (exit 4) until Voolo kills the old process.
- **A malicious chat cannot trigger this, as far as I found.** whatsmeow recovers panics in decryption and event dispatch (`message.go:318-323`, `client.go:979-986`), and the bridge recovers its own goroutines. The value-bearing `binary/encoder.go` panics are outbound-only.
- **Related gap.** Nothing tests that a non-crash library write to fd 2 is drained. Mutation R-mut-M1c (close the read end, which turns any stray write into SIGPIPE) survives.
- **Fix:** point fd 2 at the null device instead of a pipe. On Unix, `unix.Dup2(devnull.Fd(), 2)`; on Windows, `SetStdHandle(STD_ERROR_HANDLE, NUL handle)` plus `os.Stderr = devnull`. Nothing can block and there is no drain goroutine. Add a test where the probe panics with a 1 MiB value and must exit 2 within 2 s. Then delete the §14 bullet.

**R-L2. A forged trailing Ogg page defeats the voice-duration check (L10 residual).**
Where: `internal/media/media.go:263-278`, used by `internal/wa/commands.go:229` and `:384-390`.

- `OggDurationSeconds` takes the granule of the last `"OggS"` substring anywhere in the file. It does not walk or validate the page structure: capture pattern, version 0, header and segment-table length, a stream serial that matches `OpusHead`, and a granule that never decreases.
- **Reproduced.** A 2-hour stream with a 27-byte fake page appended reads as **3 s** (`TestRR_OggTrailingForgedPage`: `real=7200s forged=3s`).
- A sender can also force 0 (a last page with granule ≤ pre-skip). `fetch_media` then falls back to the declared `seconds`.
- So `voiceMaxSeconds` and `media_ready.durationS` are still the sender's word. PROTOCOL §6.8 says `durationS` "is then the file's own duration". Only the byte cap (≤ 32 MiB) truly holds.
- **Fix:**
  - Parse pages from the start: validate each header, sum segment lengths, require one Opus stream, and take the largest granule of that serial. Treat any inconsistency as unreadable.
  - For received voice, if the file is unreadable or disagrees with `seconds` by more than a small margin, report the larger of the two and cap on it.
  - Optionally reject a bytes-per-second ratio outside Opus's range (about 0.7–64 kB/s).
  - Correct §6.8 until then.

**R-L3. The resolved-address check lets translated and reserved IPv6/IPv4 forms through (L14 residual).**
Where: `internal/transport/allowlist.go:195-208`.

- Tested with `PublicIP`. These return **public = true**:
  - NAT64 `64:ff9b::7f00:1` (127.0.0.1) and `64:ff9b::a00:1` (10.0.0.1), and local-use NAT64 `64:ff9b:1::a00:1`;
  - 6to4 `2002:7f00:1::1` and `2002:c0a8:101::1`;
  - Teredo `2001::1`, site-local `fec0::1`, IPv4-compatible `::127.0.0.1`;
  - `240.0.0.1`, `192.0.0.1`, `198.18.0.1`, `192.0.2.1`.
- Mapped `::ffff:` forms, ULA and link-local are correctly refused.
- On an IPv6-only network with NAT64, a poisoned resolver can still point a WhatsApp name at the LAN. TLS still stops interception, so this is a local-network probe on port 443 only.
- DNS rebinding between the check and the dial is closed: the dial uses the checked IP (mutation R-mut-L14a is caught).
- **Fix:** use a `netip.Prefix` deny list. For `64:ff9b::/96` and `2002::/16`, extract the embedded IPv4 and run it through the IPv4 rules. Refuse `64:ff9b:1::/48`, `2001::/32`, `fec0::/10`, `::/96`, `240.0.0.0/4`, `192.0.0.0/24`, `198.18.0.0/15` and the three TEST-NETs. Correct the §10.2 wording to match.

**R-L4. When the clock goes back, the persisted send window blocks sends until the clock catches up (M6 residual).**
Where: `internal/wa/bridge.go:342-349` (`RecentSends` loads every stamp newer than now minus 10 minutes, future ones included) and `internal/wa/commands.go:60-72` (negative ages stay in the window; `now.Sub(last) < 1 s` is true for a future `last`).

- **Reproduced** (`TestRR_BackstopClockBack`): one send, restart with the clock set back one hour, and 30 minutes later a send still gets `rate_limited_local`. This denies service only. A clock moved forward lets 30 more sends through, but whoever can move the clock owns the machine anyway.
- The per-store scope (a logout resets the window) is documented in §14 and is acceptable: re-linking needs the user's phone.
- **Fix:** at load time and in `backstopAllows`, clamp stamps later than `now` to `now` (or drop them), and log `send_refused` with a `clock` code the first time. Add a test.

**R-L5. Repository settings: most of M5 is closed, but `main` is unprotected and `v*` tag creation is unrestricted.**
Where: live settings (see Checks).

- The `flags` environment allows only a branch named `main`, but `main` has no protection and no ruleset. Anyone with push access can put arbitrary `flags.yml`/`cmd/flags-sign` code on `main`, including any workflow token holding `contents: write` (the `flags` and `release`/`publish` jobs). The next approved run then executes it with the key, and the approval screen still shows no diff.
- The tag ruleset blocks update, deletion and force-push of `v*`, but not **creation**, and it does not cover the `flags` tag.
- `prevent_self_review: false` cannot help while there is one maintainer.
- The workflow side is done and tested: the job-level `if` plus the step guard before the key, and the release `id-token` held only by `attest`.
- **Fix:** add a branch ruleset on `main` (no force-push or deletion, changes through pull requests with the CI status check required, no bypass). Add the `creation` rule to "release tags" with the owner as the only bypass actor, and add `refs/tags/flags` to it.

**R-L6. Test gaps: ten of the 42 new mutations I applied survive.** They are listed in the mutation table below. All are defence in depth behind another control that is tested, but the tests do not prove the ordering and cleanup the fix commit claims:
- **H1 ordering** (R-mut-H1a): closing the store *before* the wait survives, because `ErrClosed` masks it. Add a test that asserts the store is still open while a held history goroutine runs (for example, `st.Closed()` is false inside the hold hook after `Stop` has begun).
- **History recover** (R-mut-H1c): removing `processHistorySafe`'s recover survives; `goSafe` catches it but the history worker dies. Assert that a second notification after the panic is still processed.
- **Slot freed on panic inside `beginSend`** (R-mut-H1d): not tested. Inject a panic in `OutboxKnown`/`ReserveOutbox` through a store hook.
- **`goSafe` stopped check** (R-mut-H1f): survives. Add a test that `async` after `Stop` never runs `f`.
- **Wipe of `-wal`/`-journal`** (R-mut-M3b, M3c): survive, because SQLite removes the WAL on a clean close and a WAL database never has a `-journal`. Plant stale `store.db-wal` and `store.db-journal` files (or keep a second connection open) before `Wipe` and assert that they are gone.
- **`O_NOFOLLOW` and `SameFile`** (R-mut-L4b, L4c): each alone survives, because each covers the symlink swap. The rename-swap case (a regular file renamed over the path), which only `SameFile` catches, is not tested. The post-read length check (R-mut-L4d) is untested because the hook runs before `fstat`.
- **Voice mime check** (R-mut-L10b): survives. The only mime refusal in the tests also has non-Ogg bytes. Add an Ogg body with `mime: audio/wav`.
- **stderr drain** (R-mut-M1c): see R-L1.

**R-L7. A logout command racing a shutdown can leave the store unwiped under the old key (M3 residual, narrow).**
Where: `internal/wa/commands.go:417-428` and `bridge.go:487-492`.

- Suppose `cli.Logout` succeeds (WhatsApp has unlinked; whatsmeow has already deleted the device rows) and a `shutdown` or stdin EOF sets `b.stopped` before `loggedOutBy` runs. Then `loggedOutBy` returns silently: no wipe, no `logged_out`, no reply to `logout`, and exit 0.
- The next start with the same key opens an unpaired store, and a later link reuses the old key: exactly what M3 forbids.
- In my run the race resolved the other way (`not_connected`, because Stop's `Disconnect` won), so this is from reading the code, not a reproduction.
- **Fix:** in `loggedOutBy`, record `b.loggedOut` even when `b.stopped` is set, as long as the store is not closed yet, and have Stop read `b.loggedOut` *after* its wait. Alternatively, make Stop wait for an in-flight `logout` command before deciding between Close and Wipe.

**R-L8. ADR-017 §6 does not record the bridge-side backstop.** The first review asked for the fourth defence to be written into ADR-017 §6. `git show improve/p1-b:docs/adr/017-whatsapp-bridge-design.md` has no mention of it; only PROTOCOL §6.5 and README record it. This is in the Voolo repository, so it is for the P1-B session or the lead: add one line there (1 s floor, 30 per 10 min, persisted, `rate_limited_local`, not configurable).

**R-L9. Leftover real-looking number in tests.** `internal/wa/convert_test.go:47` and `:64` still use `+201001234567` in vCard fixtures. It is never dialled, but L9 asked for fictional `1555…` numbers everywhere. Replace it with `+15550100009`.

---

## Mutation results

Each mutation was applied in the scratch copy, the named package was tested with `go test -race -count=1`, and the file was reverted. `R-mut-*` are new in this review; `orig-*` repeat the first review's mutations, including every one that survived there.

| Mutation | What it removes or changes | Result |
|---|---|---|
| orig-mut-2b | extra exact host `example.org` in the allowlist | caught (`TestAllowlistIsPinned`) |
| orig-mut-3b | retry `SendMessage` once on `timeout` | caught (`TestM4SendNeverRetried/timeout/*`) |
| orig-mut-4a | event name not checked against the fixed set | caught (`TestEventNamesAreAFixedSet`) |
| orig-mut-11 | `store.RestrictUmask()` removed from `main` | caught (`TestBinaryUmask`) |
| orig-mut-17 | real-size `CheckFetch` after download removed | caught (`TestM2…`, `TestL10…`) |
| orig-mut-19 | `os.Stdout = devnull` removed | caught (`TestStdoutGuard`) |
| R-mut-H1a | Stop closes the store before waiting | **survived** (R-L6) |
| R-mut-H1b | `goSafe` without `recover` | caught (process panics) |
| R-mut-H1c | `processHistorySafe` without `recover` | **survived** (R-L6) |
| R-mut-H1d | `beginSend` does not free the slot on a panic | **survived** (R-L6) |
| R-mut-H1e | panicking command gets no `error {internal}` | caught |
| R-mut-H1f | `goSafe` ignores `stopped` | **survived** (R-L6) |
| R-mut-H1g | store `use()` ignores `closed` | caught (`TestMethodsAfterCloseAndWipe`) |
| R-mut-M1a | no stderr isolation | caught (`TestBinaryCrashOutputNeverReachesStderr`) |
| R-mut-M1b | `ExitNoInit = 2` | caught (`TestM1ExitCodes`, `TestInitTimeoutExits6`) |
| R-mut-M1c | read end of the stderr pipe closed | **survived** (R-L1) |
| R-mut-M2a | media client without `LimitBody` | caught (`TestMediaClientHasBodyLimit`) |
| R-mut-M2b | no `Content-Length` refusal | caught (`TestBodyLimit`) |
| R-mut-M2c | no streaming cut-off | caught (`TestBodyLimit`) |
| R-mut-M2d | `fetch_media` passes a 1000× body limit | caught (`TestM2FetchMediaBoundedByRealSize`) |
| R-mut-M3a | logout closes instead of wiping | caught (`TestM3…`, `TestH1RemoteLogout…`) |
| R-mut-M3b | `Wipe` keeps `-wal` | **survived** (R-L6) |
| R-mut-M3c | `Wipe` keeps `-journal` | **survived** (R-L6) |
| R-mut-M4b | upload retried once | caught |
| R-mut-M6a | backstop off | caught (`TestM6SendBackstop`) |
| R-mut-M6b | no 1 s floor | caught |
| R-mut-M6c | window not loaded from the store | caught |
| R-mut-M6d | 31 per window | caught |
| R-mut-M6e | each send counted twice | caught |
| R-mut-L1b | `Code()` writes any value | caught in `logx` (`TestScrubCodeRedactsRandomKeys`); survives in `wa` |
| R-mut-L2a/b/c | no UTF-8 check / no surrogate check / lone low surrogate allowed | caught (`TestInvalidUTF8Refused`, `TestInvalidExamples`) |
| R-mut-L3a/b | `mark_read` for any chat / no sender check | caught (`TestL3…`) |
| R-mut-L4a | refused `send_media` does not delete the file | caught (`TestL4…`) |
| R-mut-L4b | no `SameFile` | **survived** (O_NOFOLLOW covers it; R-L6) |
| R-mut-L4c | no `O_NOFOLLOW` | **survived** (SameFile covers it; R-L6) |
| R-mut-L4bc | neither | caught (`TestOpenChecksTheHandleItReads`) |
| R-mut-L4d | no post-read short check | **survived** (R-L6) |
| R-mut-L5b | `RestrictDir(mediaDir)` removed | caught (`TestL5…`) |
| R-mut-L6 | `synchronous=NORMAL` | caught (`TestSynchronousFull`) |
| R-mut-L10a | voice with unreadable duration allowed | caught (`TestL10…`) |
| R-mut-L10b | voice mime not checked | **survived** (R-L6) |
| R-mut-L11 | ULID first character not limited | caught (`TestULIDOverflowRefused`) |
| R-mut-L14a | dial by name after the IP check (rebinding) | caught (`TestResolvedAddressMustBePublic`) |
| R-mut-L14b | no 100.64/10 check | caught |
| R-mut-L14c | no resolve step | caught |

**Totals: 48 applied (6 repeats, 42 new); 37 caught and 11 survived.** All six repeats of the first review's survivors are now caught. Of the 11 survivors, ten are the test gaps in R-L6 and one (R-mut-M1c) is part of R-L1. R-mut-L1b is counted as caught.

## Adversarial ideas tried

| Area | Attack | Result |
|---|---|---|
| H1 | The first review's reproduction: 3 × 20,000 history messages, `Stop` after 20 ms, 5 runs under `-race` | No panic; `status {stopped}` |
| H1 | Everything at once: history, a send held at the gate, `fetch_media`, a remote `LoggedOut` and `Stop` racing, 5 runs | No panic, no race report; **0 bridge goroutines** left 200 ms after Stop (goleak-style stack scan) |
| H1 | A goroutine that ignores cancellation past the 1.5 s wait | Store calls return `ErrClosed` (`store_write_failed` on stderr); **content written after `stopped` / `logged_out`** (R-M1) |
| H1 | A live whatsmeow event after Stop | Dropped (`onEvent` checks `s.ctx`) |
| H1 | Panics in whatsmeow callbacks | Recovered by whatsmeow (`dispatchEvent`, `decryptMessages`); logged through the logx adapter as format strings only |
| H1 | Recursive `RLock` deadlock with a pending `Close` | No store method calls another; not possible |
| M1 | Large panic value / concurrent-map fatal error / bridge's own JSON lines | Hang at ≥ 130 KB (R-L1) / exit 2, nothing leaked / still on the original stderr |
| M1 | Exit codes | 0/1/3/4/5/6/7/64 distinct; 2 never chosen; 6 at runtime |
| M2 | Declared-small, really-large download; server `Content-Length` too large; retries | Cut at cap+26 per request. `ErrBodyTooLarge` is not a `net.Error`, so whatsmeow does not retry it on the same host; it moves to the next host (bounded, sequential). History and app-state blobs are unbounded, but only their own devices can send them (`message.go:888` requires `IsFromMe`) |
| M3 | Reopen after logout; wipe completeness; wipe failure | Never reopened (`opens == 1`); db, `-wal`, `-shm` and `-journal` removed while the lock is held; a failure gives `error {store_io}` then `logged_out`, exit 7, documented. Logout/shutdown race: R-L7 |
| M4 | Retry on any failure or upload | None (tests plus mutations) |
| M6 | Burst, restart, clock back, forward skew | Refused / persisted / DoS (R-L4) / bypass needs control of the machine |
| L2 | `\ud800`, `\udc00`, `\\ud800`, raw `\xff`, valid pairs | Refused / refused / refused (a real escape after `\\`) / refused / accepted |
| L4 | Symlink swap, truncation, rename swap | Refused / refused / covered by `SameFile` but untested (R-L6) |
| L14 | Rebinding, mapped, ULA, NAT64, 6to4, reserved | Closed / refused / refused / **allowed** (R-L3) |
| Ogg | Fuzzing (5.4 M execs), near-maximum granule (overflow), forged trailing page | No panic / reads 0, refused / **3 s for 2 h** (R-L2) |
| stderr | A non-constant whatsmeow format, runtime `Sub()` module names, library prints | None found (grepped); module names are constants; prints go to `/dev/null` (stdout) or the pipe (fd 2) |

## What held up

- **H1.**
  - Stop cancels, disconnects, waits, and only then closes or wipes.
  - `goSafe` adds to the WaitGroup under `mu` and refuses after Stop, so it never adds concurrently with `Wait`.
  - Every bridge goroutine recovers and logs a constant code. A panicking command answers `internal` and frees the slot.
  - `Store` methods are safe after `Close`/`Wipe` (`RWMutex`, `DB` never cleared, `ErrClosed`).
  - No goroutine leaks and no data race in the combined scenario.
- **M1.**
  - fd 2 is really redirected at runtime (`dup3(7, 2)`, `/proc/<pid>/fd/2` is a pipe), and the bridge's own lines use the saved descriptor.
  - The panic value, frames and key-shaped argument words never reach the client (binary test, plus my probe with a key in a by-value array).
  - Exit codes are distinct and 2 is left to the runtime.
  - The Windows reasoning is correct for Go 1.27.1: `write1` calls `GetStdHandle` on every write.
- **M2.** One limited `RoundTripper` for every `mediaHTTP` use, wired through the request context that whatsmeow passes to `http.NewRequestWithContext`. `Content-Length` refusal plus streaming cut-off, with the real-size check kept.
- **M3.** No reopen with the old key. The store is deleted before `logged_out`, exit 7, and PROTOCOL §6.4/§9 are rewritten to match ADR-017's "fresh key on relink".
- **M4.** No-retry tests for every error class, for text and media and for upload, plus `retryFrame` and retry receipts documented.
- **M5.** Only `attest` holds `id-token`/`attestations`, and it runs no code from the repository. The licence tool and the tests are in separate jobs without a token. Flags run only from `main` (environment policy, job `if`, and a step before the key). Admin bypass is off. The `v*` update, delete and force-push rules are active.
- **M6.** Compiled in and not configurable. It is checked after `duplicate_outbox_id` and before reservation, does not use the `outboxId`, is persisted across restarts, and is retryable with no `signal`.
- **Lows.**
  - L1: a fixed event set and a code allowlist.
  - L2: raw and escaped invalid UTF-8 is refused.
  - L3: `mark_read` only for reported chats.
  - L4: early-refusal delete, `O_NOFOLLOW`, `fstat`, and the short-read guard.
  - L5: umask 077 at runtime and `mediaDir` tightened.
  - L6: `synchronous=FULL`.
  - L7: stdout guard test.
  - L8: licence exception pinned by version and `LICENSE` hash, run before the tool in CI and release.
  - L11: ULID range.
  - L12: provider statements corrected.
  - L13: no flake in 10 stress runs.
  - L14: rebinding-safe dial by checked IP.
- **Builds.** Reproducible across path and cache state, asset names as in §13, `go mod verify` clean, licences unchanged.

## Status of the first review's findings

| Finding | Status | Note |
|---|---|---|
| H1 | **Closed** | Crash reproduction gone; tests catch removal. Ordering past the 1.5 s wait is R-M1; test gaps in R-L6 |
| M1 | **Closed** | No content on stderr; codes distinct. Residual: crash-report hang, R-L1 |
| M2 | **Closed** | Bounded per request on the real path |
| M3 | **Closed** | Residual race: R-L7 |
| M4 | **Closed** | |
| M5 | **Closed** | Workflows and the environment are done. Residual settings: R-L5 |
| M6 | **Closed** | Residuals: R-L4 (clock), R-L8 (ADR line) |
| L1 | Closed | |
| L2 | Closed | |
| L3 | Closed | |
| L4 | Closed | Rename-swap test gap: R-L6 |
| L5 | **Not closed (accepted)** | Unix is closed; the Windows ACL is a documented known limit (§14) |
| L6 | Closed | |
| L7 | Closed | |
| L8 | Closed | |
| L9 | Closed | Leftover in tests: R-L9 |
| L10 | **Partly closed** | Forged trailing page: R-L2; non-Ogg received voice keeps the declared duration (§14) |
| L11 | Closed | |
| L12 | Closed | |
| L13 | Closed | |
| L14 | **Partly closed** | NAT64/6to4/reserved ranges: R-L3 |

## Before the first release

1. R-M1: `Acquire` honours a cancelled context, the history paths check `ctx` before emitting, and `emit` is quiesced after Stop begins; with a test.
2. R-L1: send fd 2 to the null device instead of a pipe; with a large-panic test.
3. R-L5: protect `main`, and restrict `v*` creation and cover the `flags` tag, before the first `flags` run or tag.
4. The other Lows: fix them, or write them into the P1-A card with an owner and a date.

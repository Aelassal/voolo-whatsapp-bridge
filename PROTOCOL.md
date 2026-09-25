<!--
SPDX-License-Identifier: CC0-1.0

To the extent possible under law, the authors of this file have waived all copyright and
related or neighboring rights to this protocol specification (PROTOCOL.md) and to the example
messages in the examples/ folder. This work is published from: Egypt.
https://creativecommons.org/publicdomain/zero/1.0/

This dedication covers only PROTOCOL.md and examples/. The program itself is GPL-3.0 (LICENSE).
-->

# voolo-whatsapp-bridge protocol, version 1

This document is the complete interface of `voolo-whatsapp-bridge`. A client needs nothing else to drive the program: not its source, not any library. It is dedicated to the public domain (CC0-1.0, header above), so a client under any licence can implement it.

- **Protocol version:** 1 (the `v` field of every message and `hello.protocol`).
- **Status:** v1 as published with the first release. Fields may be **added** within v1. Removing or changing a field, or a new command whose absence would break a client, means v2.
- **Changes before the first release** (2026-09-25, from building the bridge, task P1-A): media caps raised by the owner (§6.1, §8); optional `limits.voiceMaxBytes` (§6.1) and `send_media.fileName` (§6.6); clarifications of existing rules in §2, §5.3, §5.4, §5.10, §5.12, §5.15, §6.2–§6.9, §9, §10 and the flags key in §11. Each clarified rule is marked *(clarified)*.
- **Changes from the P1-A security review** (2026-09-25, before the first release; marked *(review)*): a logout ends the process and the store is never reopened with the same key (§5.9, §6.4); exit codes 6 and 7, and 2 is left to the Go runtime (§4, §9); a compiled-in send backstop with the new error `rate_limited_local` (§6.5, §9); `client_outdated` is signalled only after the bridge's own refresh and retry fail (§5.4); stricter input (invalid UTF-8, ULID range; §2), `mark_read` only for reported chats (§6.7), voice notes read from the file (§6.6, §6.8, §8), bounded media downloads (§6.8, §8), the stderr event list (§10.4), release file names and versions (§13), and corrections of three statements about WhatsApp's behaviour (§5.4, §6.2, §10.3). Known limits are in §14.
- **Changes from the P1-A security re-review and the lead's decisions** (2026-09-25, before the first release; marked *(re-review)*): nothing but the final lines reaches stdout once the bridge stops, and a command still running then gets no reply (§6.11); only sends handed to WhatsApp count toward the backstop, which also survives the clock going back (§6.5); a logout confirmed during a shutdown still deletes the store (§6.4, §9); a failed store deletion still exits 7 (§6.4); voice durations from a check of the whole Ogg file (§6.6, §6.8); more refused address ranges (§10.2); stderr goes to the null device (§10.4, §14); `paired` is matched by state (§5.7); every `fatal` error names its exit code (§5.22); output is always valid UTF-8 (§1); stderr codes are their own namespace (§10.4).
- Words in capitals (MUST, MUST NOT, SHOULD) have their usual meaning.

## 1. Transport

- The client starts the program as a child process **with no arguments** and connects its stdin, stdout and stderr to pipes. There is no socket, port or named pipe.
- The program's only other arguments are for people: `--version`, `--license` and `--source` print text and exit.
- **stdout** carries protocol lines only. **stdin** carries protocol lines only. **stderr** carries diagnostics (§10) and is never part of the protocol.
- **Framing:** one JSON object per line, UTF-8, ending with `\n` (a `\r` before it is tolerated on input). JSON strings never contain a raw newline, so a line is always one message.
- *(re-review)* **Every line the bridge writes is valid UTF-8** and never holds a lone UTF-16 surrogate, escaped or not. Text from WhatsApp that is not valid UTF-8 (a broken byte, an encoded surrogate) is written with each invalid byte replaced by U+FFFD; nothing else in it changes. (Commands with such text are refused instead, §2.)
- **Maximum line size: 1,048,576 bytes** (1 MiB) including the newline, in both directions. The receiver discards a longer line up to the next `\n` and counts it. The bridge splits its own output so that no line exceeds this (history batches: §7.1).
- If stdin reaches end-of-file, the bridge shuts down as if it had received `shutdown` (§6.11).
- Binary data (media) never travels on the pipes. It is handed over as an encrypted file (§8).

## 2. Envelope

Every line in both directions is an object with exactly these five fields:

```json
{"v":1,"id":"01M3C03V80YKN5QVM42E69359G","type":"ping","ts":1790330400000,"payload":{}}
```

| Field | Type | Rules |
|---|---|---|
| `v` | integer | `1`. A different value means the peer speaks another protocol: the client SHOULD stop the bridge; the bridge answers `error {code: unsupported_version}` and ignores the line. |
| `id` | string | A ULID: 26 characters of Crockford base32 (`0-9 A-Z` without `I L O U`), unique per sender. *(review)* The first character is `0`–`7` (a ULID is 128 bits); a larger one is not a ULID. |
| `type` | string | `^[a-z][a-z0-9_]{0,63}$`. Commands and events are listed in §5–§7. |
| `ts` | integer | Sender's clock, Unix milliseconds UTC. |
| `payload` | object | Per type. `{}` when empty. |

**Replies.** A reply to a command carries `payload.replyTo` = the command's `id`. Every command gets exactly one final reply (`ok`, a typed result, or `error`), except `ack`, which gets none. Events that are not replies have no `replyTo`.

**Strictness.** The bridge decodes **commands strictly**: an unknown field anywhere in a command's payload, a wrong type or a value out of range gets `error {code: bad_request}` and the command is not executed. *(clarified)* Member names are matched exactly, including case (`chatjid` is an unknown field, not `chatJid`); a duplicated member name, a `null` value for any field, and a number written with a fraction or exponent where an integer is expected are all `bad_request`. The order of checks is: envelope, `v`, known `type` (`unknown_command`), `init` order (`not_initialized`), payload (`bad_request`). Clients SHOULD decode **events leniently**: ignore fields they do not know, so that a newer bridge can add fields within v1. A client ignores an event `type` it does not know; the bridge answers a command `type` it does not know with `error {code: unknown_command}` and does nothing else. A line that is not valid JSON, or not an object with exactly the five envelope fields, is dropped and counted. *(review)* A command payload that is not valid UTF-8, or that holds a `\uXXXX` escape of a lone UTF-16 surrogate, is `bad_request`: the bridge never replaces such text with U+FFFD and sends it.

## 3. Identifiers and values

- **Chat JID:** WhatsApp's address of a chat, without a device part. `<digits>@s.whatsapp.net` (a person, by phone number), `<digits>@lid` (a person, by WhatsApp's linked id, when no phone number is known), `<digits>-<digits>@g.us` or `<digits>@g.us` (a group). Max 128 characters. Broadcast lists, `status@broadcast` and newsletters are never reported.
- **Sender JID:** a person's JID as above. In a group it names the author, and for your own messages it is your own JID.
- **Message id:** WhatsApp's message id, a string of 1–128 characters from `[A-Za-z0-9._-]`. A message is identified by `(chatJid, messageId)`.
- **Phone number** (in `pair_phone`): digits only, 7–15 of them, international format without `+` or a leading `0` (for example `15550100001`, a fictitious number: never try a real stranger's number, the phone behind it would be asked to link).
- **Times** are Unix milliseconds UTC. **Durations** are in milliseconds unless the name ends in `S` (seconds).
- **Text** is UTF-8, max 65,536 characters. Names and titles are max 512 characters. Longer values are truncated by the bridge at a character boundary.

## 4. Session lifecycle

```
client                                   bridge
  | spawn (no args) ----------------------> |
  | <------------------------------- hello  |   protocol and version
  | init (store key, folders, limits) ----> |   first and only once; within 10 s
  | <------------------------------- ready  |   paired: true|false
  |                                         |
  |  if paired:   bridge connects by itself; status connecting -> connected
  |  if unpaired: pair_qr or pair_phone -> qr / pair_code ... -> paired
  |                                         |
  | ... events, commands, ping/pong ...     |
  | shutdown -----------------------------> |
  | <---------------------------------- ok  |   then exits with code 0
```

- The bridge accepts **no command before `init`** except `ping` and `shutdown`. Anything else gets `error {code: not_initialized}`.
- If `init` has not arrived within **10 s** of `hello`, the bridge exits with code 6. *(review: was 2, which is the Go runtime's own crash code; §9.)*
- A second `init` gets `error {code: already_initialized}`.

## 5. Events (bridge → client)

Examples of every event are in `examples/bridge-to-app/`. Optional fields are marked `?`.

### 5.1 `hello`
First line the bridge writes.
`{bridge: "voolo-whatsapp-bridge", version: "<semver>", protocol: 1, os: "windows|darwin|linux", arch: "amd64|arm64"}`
*(review)* `version` is `MAJOR.MINOR.PATCH` without a leading `v` (a development build says `0.0.0-dev`). Versions are compared as three numbers, never with a `v` and never as strings, for example against `flags.json` `maxBridgeVersion` (§11) and release tags (§13).

### 5.2 `ready` (reply to `init`)
`{replyTo, paired: bool, account?: {jid, pushName?}}`. When `paired` is true, the bridge starts connecting immediately.

### 5.3 `status`
The connection state, sent on every change.
`{state}` where `state` is:
- `unpaired`: no linked account in the store. Waiting for `pair_qr` or `pair_phone`.
- `connecting`: first connection after start or pairing.
- `connected`: online and receiving.
- `reconnecting`: the connection dropped. The bridge retries by itself.
- `stopped`: the bridge will not reconnect (after `logged_out`, or before exiting). *(review)* After `logged_out` the bridge always exits (code 7, §6.4); it never goes back to `unpaired` in the same process. *(clarified)* Also after `signal {temp_banned}`, `signal {stream_replaced}` and `signal {connect_failure}`: WhatsApp closed the connection and the bridge does not dial again by itself. The client decides (restart the bridge later, or not).

### 5.4 `signal`
A provider event that suggests throttling, a ban, a forced update or a duplicate session. The bridge reports these and never acts on them beyond what §6 says. The client decides what to do.
`{kind, code?, expiresInMs?}` where `kind` is:
- `rate_limited`: WhatsApp answered with a rate-limit error (for example 429). *(clarified)* `code` is `429`. It is sent together with the `error {code: rate_limited}` of a refused send, and for a connect failure with reason 429.
- `temp_banned`: temporary ban. `code` is WhatsApp's reason code (101, 102, 103, 104, 106, …), and `expiresInMs` is present if WhatsApp gave one.
- `stream_replaced`: another client connected with the same session. The bridge stops reconnecting.
- `client_outdated`: WhatsApp rejected the client version (405). *(review, lead decision 2026-09-25)* On the first 405 the bridge refreshes the WhatsApp Web version and reconnects once by itself, without a signal. Only if that retry is rejected again, or the refresh fails, it sends `signal {client_outdated}`, then `error {client_outdated, fatal: true}`, and exits with code 5. A pairing in progress is reopened by the retry, and ended with `pair_failed {client_outdated}` if it fails.
- `connect_failure`: another connection failure that WhatsApp's library does not retry by itself, with `code`. *(review, corrected)* 500 and 503 are retried by the connection library without any event, so they are not reported here; the client sees `status {reconnecting}` at most. A 429 is reported as `rate_limited`.
- `stream_error`: an unknown stream error, with `code` as a string.
- `keepalive_timeout`: the server stopped answering keep-alives.

### 5.5 `qr` (repeated reply to `pair_qr`)
`{replyTo, code, expiresAt, index}`. `code` is the string to encode in a QR image (max 4,096 characters). `index` counts from 0. The first code lives about 60 s and the next ones about 20 s each; after about 160 s the bridge sends `pair_failed {reason: timeout}`. A newer `qr` replaces the previous one.

### 5.6 `pair_code` (reply to `pair_phone`)
`{replyTo, code}`: an 8-character code, shown to the user as `XXXX-XXXX`, which they type on the phone (WhatsApp → Linked devices → Link a device → Link with phone number instead). The phone may show a notification instead. The code expires with the pairing window (§5.5).

### 5.7 `paired`
`{jid, lid?, pushName?, businessName?}`: linking succeeded. `status` becomes `connecting`, then `connected`, and history arrives (§7).
- *(re-review, lead decision)* `paired` is an event, not a reply: it has **no `replyTo`**. A client matches it to its `pair_qr` or `pair_phone` by state: only one pairing can run at a time (a second one gets `busy`, §6.2), so a `paired` completes the pairing in progress. It is the final reply of that command in the sense of §2 and §6.

### 5.8 `pair_failed`
`{replyTo, reason}` where `reason` is `timeout`, `rejected` (the phone refused or the code was wrong), `client_outdated` or `error`. The bridge is back to `unpaired`.

### 5.9 `logged_out`
`{reason, code?}`: the account is no longer linked. `reason` is:
- `user`: after the client's own `logout` command.
- `device_removed`: removed from the phone's Linked devices, or after long phone inactivity (code 401).
- `primary_gone`: the phone's account was logged out or moved (code 403).
- `banned`: code 406 or any logout WhatsApp describes as a ban.
- `unknown`: anything else.

The bridge has already deleted the linked device from its store when it sends this event. *(review)* It has in fact deleted the whole store file: the event follows the deletion. Then comes `status {stopped}` (and `ok` for a `logout` command), and the bridge exits with code 7. It never opens a new store in the same process (§6.4).

### 5.10 `sync_progress`
`{kind: "history"|"offline", progress?, done}`. `history` tracks the history sync the phone sends after linking (`progress` 0–100 when the phone reports it). `offline` tracks the catch-up of messages that arrived while the bridge was not running. `done: true` is sent once per kind per connection. *(clarified)* The phone does not say when it has sent its last history part, so `{kind: history, done: true}` is sent after a part that reports `progress: 100` has been fully delivered, or after 60 s without a new history part, whichever comes first. It is only sent on a connection that received history.

### 5.11 `chat`
Full metadata for one chat, sent when it first appears (usually inside history) and when it changes a lot.
`{chat: {jid, kind: "dm"|"group", name?, unreadCount?, pinned?, mutedUntil?, archived?, lastMessageAt?}}`. `mutedUntil` is a time, or `-1` for "muted forever". A chat that is not muted omits it.

### 5.12 `chat_update`
A partial change to one chat. Only the fields that changed are present.
`{jid, name?, unreadCount?, markedRead?, pinned?, mutedUntil?, archived?, aliasOf?}`
- `markedRead: true`: the chat was read on another device.
- `aliasOf`: this chat's `@lid` JID now maps to the phone-number JID in `jid`. The client SHOULD merge the two chats. After this event the bridge uses `jid`. *(clarified)* It is sent once per `@lid` chat, also across restarts (the bridge remembers which `@lid` chats it has reported, §10.1).
- *(clarified)* `mutedUntil: 0` means the chat was unmuted; `-1` means muted forever.

### 5.13 `contact`
`{jid, name?, pushName?, businessName?}`: a display name for a person who appears in a reported chat or as a group sender. The bridge never reports the whole address book.

### 5.14 `group`
`{jid, name, topic?, participants: [{jid, isAdmin?}] (max 2,048)}`, sent when a group chat is first reported and when its membership or subject changes.

### 5.15 `message`
One message, new or updated, live or from the phone's other devices (including your own sent messages).
`{message: Message}` where `Message` is:

| Field | Type | Notes |
|---|---|---|
| `chatJid` | JID | |
| `id` | message id | |
| `senderJid` | JID | author (your JID when `fromMe`) |
| `fromMe` | bool | |
| `ts` | time | when it was sent |
| `kind` | string | `text`, `image`, `video`, `sticker`, `voice` (push-to-talk), `audio`, `document`, `location`, `contact`, `poll`, `system`, `unsupported` |
| `text?` | string | body, or the caption of a media message |
| `pushName?` | string | the sender's own chosen name |
| `quoted?` | `{id, senderJid?}` | the message this one replies to |
| `mentions?` | JID[] (max 64) | |
| `forwarded?` | bool | |
| `edited?` | bool | already reflects an edit (history) |
| `media?` | `{mime, sizeBytes, durationS?, width?, height?, fileName?}` | a description only; fetch with `fetch_media` |

*(clarified)* What v1 reports: view-once messages as `unsupported` with no `media` (they can never be fetched, as in WhatsApp Web); `location` and `contact` without `text` (a contact card's phone numbers are never reported); `poll` with the question as `text`; a message that cannot be decrypted as `unsupported` (a later `message` with the same id replaces it if WhatsApp re-sends it). System notices without a body (for example "X joined") are not reported in v1, so `system` is reserved. Reactions and poll votes are not messages: reactions arrive as `reaction`, poll votes are not reported.

### 5.16 `message_update`
`{chatJid, messageId, kind: "edit"|"revoke", text?, at, by?}`. `edit` carries the new `text`. `revoke` means "deleted for everyone"; `by` is the JID that deleted it.

### 5.17 `reaction`
`{chatJid, messageId, senderJid, emoji, at}`. `emoji: ""` means the reaction was removed.

### 5.18 `receipt`
`{chatJid, messageIds: [id] (1–256), kind, senderJid?, at}`, where `kind` is `delivered`, `read` or `played` (for your messages, from the recipient; `senderJid` names who in a group), or `read_self` (you read these on another device).

### 5.19 `history_batch`
See §7. `{seq, syncType, chat, messages: [Message] (0–200), progress?}`.

### 5.20 `media_ready` (reply to `fetch_media`)
See §8. `{replyTo, messageId, path, key, sha256, mime, sizeBytes, durationS?}`.

### 5.21 `send_result` (reply to `send_text` / `send_media`)
`{replyTo, outboxId, messageId, at}`: WhatsApp's server accepted the message. `messageId` is the id it will have in every later `receipt`.

### 5.22 `ok`, `pong`, `error`
- `ok {replyTo}`: success for commands without a typed result (`logout`, `mark_read`, `shutdown`).
- `pong {replyTo}`: reply to `ping`.
- `error {replyTo?, code, retryable, fatal?}`: see §9. `fatal: true` means the bridge exits right after this line.
- *(re-review, lead decision)* Every `fatal: true` error is followed by the process's exit, with the code listed for it: `store_key_invalid` → **3**, `store_locked` → **4**, `client_outdated` → **5** (§9). No other code is ever fatal. (A logout is not announced with a fatal error: it ends with `logged_out` and exit 7, §5.9.)

## 6. Commands (client → bridge)

Examples of every command are in `examples/app-to-bridge/`. Commands are decoded strictly (§2). Timeouts are what a client SHOULD wait for the final reply before treating the command as failed.

| Command | Payload | Final reply | Timeout |
|---|---|---|---|
| `init` | §6.1 | `ready` | 10 s |
| `pair_qr` | `{}` | `qr`… then `paired` or `pair_failed` | 180 s |
| `pair_phone` | `{phone}` | `pair_code`, then `paired` or `pair_failed` | 30 s for the code |
| `logout` | `{}` | `ok` | 30 s |
| `send_text` | §6.5 | `send_result` | 60 s |
| `send_media` | §6.6 | `send_result` | 60 s |
| `mark_read` | §6.7 | `ok` | 30 s |
| `fetch_media` | `{chatJid, messageId}` | `media_ready` | 120 s |
| `ack` | `{seq}` | none | — |
| `ping` | `{}` | `pong` | 30 s |
| `shutdown` | `{}` | `ok`, then exit 0 | 3 s |

There are **no** other commands. In particular there is no command to send to several chats, to schedule, to repeat, to use a template, to broadcast, to change presence, to read the address book, to manage groups or to post a status. None will be added.

### 6.1 `init`
```
{storeKey, storeDir, mediaDir, deviceName, limits: {historyDays, historyMaxPerChat, imageMaxBytes, voiceMaxSeconds, voiceMaxBytes?}}
```
- `storeKey`: 64 lowercase hex characters (32 bytes). Required. It encrypts the session store (§10.1). The bridge never writes it anywhere.
- `storeDir`: absolute path of the folder that holds the store. The bridge creates it with owner-only permissions if missing.
- `mediaDir`: absolute path of the folder where `media_ready` files are written.
- `deviceName`: the name shown on the phone for this linked device, 1–32 characters.
- `limits`: `historyDays` 1–90, `historyMaxPerChat` 1–20,000, `imageMaxBytes` 1–16,777,216, `voiceMaxSeconds` 1–3,600, `voiceMaxBytes` 1–33,554,432 (optional; absent means 33,554,432). These ranges are the bridge's own ceilings (90 days, 20,000 messages per chat, 16 MiB images, voice notes up to 60 minutes and 32 MiB): a value outside them gets `bad_request`, never a silent raise. *(changed 2026-09-25 by the owner: the first draft had 2 MiB images and 300 s voice notes, which cut off ordinary photos and long voice notes.)*
- *(clarified)* `storeDir` and `mediaDir` must be absolute and contain no `..` element. `deviceName` has no control characters. It is the name the phone shows for a QR link; a phone-number link shows `Chrome (<OS>)` instead (§6.3).

Errors: `store_key_invalid` (the key does not open an existing store; fatal, exit 3), `store_locked` (another bridge holds the store; fatal, exit 4), `store_io`, `bad_request`.

### 6.2 `pair_qr`
Starts QR linking. Fails with `already_paired` if the store already has an account. *(clarified)* Fails with `busy` while another `pair_qr` or `pair_phone` is running. *(review, corrected)* If the linking connection cannot be started at all, the reply is `error {pair_failed}` (retryable). If it starts but closes before a code arrives (for example no network), the pairing ends with `pair_failed {reason: timeout}`, usually within a second. If no code arrives within 30 s, the reply is `error {pair_failed}`. In every case the client may simply try again.

### 6.3 `pair_phone`
`{phone}` (§3). Starts linking by code. Errors: `phone_invalid`, `already_paired`, `pair_failed`. *(clarified)* Also `busy`, as in §6.2. WhatsApp requires the name of a code-linked device to have the form `Browser (OS)`, so the phone's Linked devices list shows `Chrome (Windows)`, `Chrome (Mac OS)` or `Chrome (Linux)` for such a link (approved by the owner on 2026-09-25). No `qr` event is sent during a phone-number link; its window is the same as the QR window (§5.5).

### 6.4 `logout`
Unlinks this device on WhatsApp's side, deletes the store and exits. *(review, changed: the first draft reopened an empty store with the same key, so the old key stayed in use and in the keychain, and leftover copies of the old file stayed decryptable.)*
- The bridge stops the session, waits for its own work to finish, deletes the whole store file (`store.db` and its `-wal`, `-shm`, `-journal`), then writes, in order: `logged_out {reason: user}`, `status {stopped}`, `ok`. It then exits with **code 7**.
- A remote logout (§5.9) does the same, without the `ok`.
- The bridge never opens a store again with the key it was given. To link again, the client deletes the old key from its keychain, generates a **new** key, and starts a new bridge with it (an empty store folder, or the same folder: the old file is gone). The client SHOULD also delete the store folder; if the deletion failed, the bridge writes `error {store_io}` without `replyTo` before `logged_out`, and a new key on the old file would get `store_key_invalid`.
- *(re-review, lead decision)* **A failed deletion still ends the same way:** `error {store_io}`, `logged_out`, `status {stopped}` (and `ok` for a `logout` command), then exit **7**. The client MUST then delete the store folder itself and use a new key.
- *(re-review)* If WhatsApp confirms the `logout` while the bridge is already stopping (a `shutdown` or stdin EOF arrived meanwhile), the store is still deleted and the logout is still reported (`logged_out`, `status {stopped}`, `ok`); the bridge then exits with **7**, not 0, also after `shutdown`.
- Errors: `not_paired`; `not_connected` (retryable; nothing was unlinked); `timeout`; `internal`.

### 6.5 `send_text`
`{chatJid, text, outboxId, quotedMessageId?}`
- `text`: 1–65,536 characters.
- `outboxId`: a ULID chosen by the client, one per message the user asked to send.
- Only **one** send (`send_text` or `send_media`) may be in flight. A second one gets `busy`.
- The bridge remembers the last 1,000 `outboxId`s in the store, across restarts. A repeated one gets `duplicate_outbox_id` and nothing is sent.
- The bridge never retries a send by itself. Errors: `not_paired`, `not_connected` (nothing was sent), `busy`, `duplicate_outbox_id`, `unknown_chat`, `rate_limited_local`, `rate_limited`, `send_failed`, `timeout`, `internal`.
- *(clarified; review: `rate_limited_local` added)* Checks run in this order: `not_paired`, `not_connected`, `busy`, `unknown_chat`, `duplicate_outbox_id`, `rate_limited_local`. A command refused by one of them is not remembered, so the same `outboxId` can be used again.
- *(review)* **Send backstop.** Compiled into the bridge, not configurable: at least **1 s** between two sends, and at most **30 sends in any 10 minutes** (`send_text` and `send_media` together, also across restarts of the bridge). A send beyond it gets `error {rate_limited_local, retryable: true}`: nothing was sent, the `outboxId` is not used, and no `signal` is sent (WhatsApp said nothing). It sits above a client's own caps (Voolo's is 20 per 10 minutes) and is only a last defence against a loop or a script.
- *(re-review, lead decision; changed: sends were counted when the `outboxId` was reserved)* **Only sends handed to WhatsApp count**, from the moment they are handed over, whatever WhatsApp answers (`send_result`, `timeout`, `send_failed`, `rate_limited`). A command refused before that does not count: every check above (`not_paired` … `rate_limited_local` itself), and for `send_media` also `media_invalid`, `media_too_large` and a failed upload, even though its `outboxId` is then used.
- *(re-review)* **Clock changes.** Send times are the computer's clock. If the clock goes back, a send time that now lies in the future is treated as made 1 s before the bridge noticed (at `init`, or at the next send), so it counts for at most 10 more minutes: the backstop never blocks sends for longer than its window because of a clock change. The bridge writes `send_clock_skew` on stderr (§10.4).
- *(review)* **Transport-level resends.** "Never retried" means the bridge never sends a message again. Two things below it are not new messages: (1) if the connection drops while a send waits for WhatsApp's acknowledgement and comes back within about 5 s, the connection library writes the **same encrypted frame, with the same message id**, once more, and WhatsApp keeps one message; (2) when a recipient's device cannot decrypt a message it asks for it again (a retry receipt), and the library re-encrypts and resends **the same message id** to that device, as every WhatsApp client does. Neither creates a second message in the chat. Once those checks pass, the `outboxId` is remembered **before** the message is handed to WhatsApp, whatever happens next: after `timeout`, `send_failed` or `rate_limited`, the same `outboxId` gets `duplicate_outbox_id`, and a retry is a new user action with a new `outboxId`.
- *(clarified)* `unknown_chat`: the bridge only sends to a chat it has reported (in this run or an earlier one, remembered in the store) or to a group the account belongs to. A phone number that never appeared is refused.
- *(clarified)* The single send slot is freed before `send_result` or the send's `error` is written, so the next send may follow the reply immediately (subject to the backstop).
- *(review)* A panic inside a send is answered with `error {internal}` and frees the slot.
- *(clarified)* `quotedMessageId` refers to a message in the same chat. The bridge adds the quoted message's author when it has seen that message recently; it does not send a copy of the quoted content.

### 6.6 `send_media`
`{chatJid, outboxId, kind: "image"|"voice"|"file", path, key, sha256, mime, caption?, fileName?}`. The client writes the file in the §8 format under `mediaDir` with its own fresh key. The bridge reads, decrypts and checks it, uploads it, deletes the file, and replies. Same rules and errors as §6.5, plus `media_too_large` and `media_invalid`.
- *(clarified)* `path` must be `<mediaDir>/<32 lowercase hex>.bin`, a regular file (not a link). It is deleted once read, in every case. *(review)* Also when the command is refused before the file is read (`busy`, `unknown_chat`, `duplicate_outbox_id`, `not_connected`, …). The file is opened once and checked on the open handle, so a path swapped for a link after the check, or a file cut short, gets `media_invalid`. Caps: `image` ≤ `limits.imageMaxBytes`; `voice` ≤ `limits.voiceMaxBytes` and ≤ `limits.voiceMaxSeconds` (read from the file); `file` ≤ 33,554,432 bytes. *(review)* A `voice` must be Ogg Opus (`mime` `audio/ogg`, with or without parameters) whose duration the bridge can read; anything else is `media_invalid`. *(re-review)* The duration is read from a check of every page of the file: one Opus stream that starts with its `OpusHead`, pages that follow each other without gaps or trailing bytes, sequence numbers that increase by one, granule positions that never decrease, nothing after the end-of-stream page. A file that fails any of this is `media_invalid`. The duration is the largest granule minus the pre-skip, and never less than the file's size divided by 72,000 bytes per second (Opus's highest bitrate plus the Ogg framing). `voice` is sent as a voice note (push-to-talk). `mime`: `type/subtype` with optional parameters, for example `audio/ogg; codecs=opus`.
- *(added within v1)* `fileName?`: 1–255 characters, no `/`, `\` or control characters; the name shown for `kind: file` (default `file` plus an extension from `mime`).

### 6.7 `mark_read`
`{chatJid, messageIds: [id] (1–50), senderJid?}`. Sends read receipts for these received messages (`senderJid` is required in groups). WhatsApp applies the user's own read-receipt privacy setting. *(clarified)* In a group, all `messageIds` must be from that one `senderJid`; send one `mark_read` per sender. A group `mark_read` without `senderJid` is `bad_request`. *(review)* Only for a chat the bridge has reported (as for sends, §6.5), otherwise `unknown_chat`; and if the bridge knows that one of the `messageIds` was written by someone other than `senderJid`, `bad_request`. Errors: `not_paired`, `not_connected`, `unknown_chat`, `bad_request`, `timeout`, `internal`.

### 6.8 `fetch_media`
`{chatJid, messageId}`. Downloads the media of a message the bridge has reported, subject to the limits of `init`. Errors: `unknown_message`, `media_too_large` (checked before download, and again during and after it), `media_expired` (no longer on WhatsApp's servers), `media_unavailable`, `not_connected`, `timeout`.
- *(review)* The size a message declares is the sender's word. The bridge also cuts the download itself off once it is longer than the cap plus WhatsApp's encryption overhead (26 bytes), and refuses a server answer that announces more, so a small message pointing at a huge file costs at most the cap in memory; the answer is `media_too_large`. For a voice note the duration is read from the downloaded file where it can be (Ogg Opus, checked as in §6.6) and checked against `voiceMaxSeconds`; `media_ready.durationS` is then the file's own duration. *(re-review, corrected)* The file comes from the sender too, so it never lowers the declared duration by much: if the file cannot be read, or reads more than 2 s shorter than the message declared, `durationS` is the longer of the two and the cap is checked against it. The size cap on the real bytes (`voiceMaxBytes`) is the hard limit.
- *(clarified)* v1 fetches only `voice` and `image` messages (owner decision: video, documents, stickers and other audio stay on the phone). For any other kind the bridge keeps no download descriptor, so `fetch_media` answers `unknown_message`. `chatJid` is the JID under which the message was reported, or the phone-number JID of a chat merged through `aliasOf`. At most two downloads run at once; more wait.

### 6.9 `ack`
`{seq}`. Acknowledges a `history_batch` (§7). No reply. *(clarified)* An `ack` acknowledges that batch **and every earlier one**; an unknown or repeated `seq` is ignored.

### 6.10 `ping`
Liveness. A client SHOULD ping every 30 s and restart the bridge after two missed `pong`s.

### 6.11 `shutdown`
The bridge disconnects, flushes and closes the store, replies `ok` and exits with code 0 within 3 s. The same happens on stdin EOF (without the reply).
- *(re-review)* **Once the bridge begins to stop** (`shutdown`, stdin EOF, a fatal error, a logout), stdout carries nothing but its final lines, in this order: `error {store_io}` (only if a store deletion failed, §6.4), `logged_out` (after a logout), `status {stopped}`, `ok` for a `logout` command, and `ok` for `shutdown`. Everything else is dropped (counted on stderr as `emit_dropped`): no more history, messages, receipts or replies. A command still running then gets **no reply**. A `send_text` or `send_media` without a reply MUST be treated as `timeout` (it may or may not have been sent; its `outboxId` is used).

## 7. History and flow control

After linking, the phone sends recent history in several parts. The bridge turns them into `history_batch` events, one chat at a time:

```
{seq, syncType: "initial"|"recent"|"full"|"push_name"|"on_demand", chat: Chat, messages: [Message], progress?}
```

1. `seq` starts at 1 for each bridge process and increases by 1 per batch.
2. A batch holds at most **200 messages** and is at most **786,432 bytes** encoded. A chat with more messages is split across several batches. `chat` repeats the chat's metadata in each batch, so every batch stands alone.
3. **Window:** the bridge sends at most **4 batches** that the client has not yet acknowledged with `ack {seq}`. A client acknowledges after storing a batch. It MAY delay acks to slow the bridge down (for example while its user is typing).
4. Live events (`message`, `receipt`, …) are **not** part of the window and are written between batches, so new messages never wait for history.
5. The bridge drops history messages older than `limits.historyDays` and stops at `limits.historyMaxPerChat` messages per chat. It asks the phone for at most 90 days of history.
6. `sync_progress {kind: history, done: true}` follows the last batch.
7. Media inside history is described (`media`), never downloaded.

## 8. Media hand-off

Media bytes never travel on the pipes.

1. `fetch_media` → the bridge downloads the file from WhatsApp (whatsmeow verifies WhatsApp's hashes) and checks the size and duration limits again, on the real bytes. *(review)* The download is cut off past the cap (§6.8).
2. It writes the file to `<mediaDir>/<random 32 hex>.bin` in this format: `nonce (12 bytes) ‖ ciphertext ‖ tag (16 bytes)`, AES-256-GCM, with a **fresh random 32-byte key** for this file and no additional data. Caps (§6.1): images ≤ 16 MiB, voice notes ≤ 60 minutes and ≤ 32 MiB, checked against the description before downloading and against the real size after.
3. It replies `media_ready {replyTo, messageId, path, key, sha256, mime, sizeBytes, durationS?}`: `path` is absolute and inside `mediaDir`; `key` is the 32-byte key as 64 hex characters; `sha256` is the hex SHA-256 of the **plaintext**; `sizeBytes` is the plaintext size.
4. The client reads the file, decrypts, checks `sha256`, and **deletes the file**. The bridge deletes `.bin` files older than 1 hour in `mediaDir` at start.

`send_media` uses the same format in the other direction.

## 9. Error codes

`error {replyTo?, code, retryable, fatal?}`. `retryable` says whether the same command could succeed later without any change. The bridge never retries a send by itself either way.

| code | meaning | retryable |
|---|---|---|
| `bad_request` | payload failed strict decoding | no |
| `unknown_command` | `type` is not a command | no |
| `unsupported_version` | `v` is not 1 | no |
| `not_initialized` / `already_initialized` | `init` order | no |
| `store_key_invalid` | the key does not open the store (fatal, exit 3) | no |
| `store_locked` | another bridge uses the store (fatal, exit 4) | no |
| `store_io` | disk error in the store | yes |
| `not_paired` / `already_paired` | pairing state | no |
| `phone_invalid` | bad phone number | no |
| `pair_failed` | the pairing request was refused | yes |
| `not_connected` | offline; nothing was sent | yes |
| `busy` | a send is already in flight | yes |
| `rate_limited_local` | *(review)* the bridge's own send backstop (§6.5); nothing was sent | yes |
| `duplicate_outbox_id` | this `outboxId` was already used | no |
| `unknown_chat` / `unknown_message` | not known to the bridge | no |
| `rate_limited` | WhatsApp refused for rate reasons | yes |
| `send_failed` | WhatsApp refused the message for another reason | no |
| `timeout` | no answer from WhatsApp in time; the message **may** have been sent | no |
| `media_too_large` | above the limits | no |
| `media_expired` / `media_unavailable` | cannot be downloaded | no |
| `media_invalid` | a `send_media` file failed decryption or its hash | no |
| `host_blocked` | a connection to a host outside the allowlist was refused (§10.2); sent without `replyTo` | no |
| `client_outdated` | WhatsApp requires a newer client (fatal, exit 5) | no |
| `internal` | anything else | no |

**Exit codes** *(review: 6 and 7 added, 2 moved)*:

| code | meaning |
|---|---|
| 0 | normal end (`shutdown`, stdin EOF) *(re-review: 7 instead when a logout was confirmed meanwhile, §6.4)* |
| 1 | a panic caught on the main goroutine |
| 2 | never chosen by the bridge: the Go runtime's code for an unrecovered panic or a fatal runtime error. Treat it as a crash. |
| 3 | store key invalid |
| 4 | store locked |
| 5 | client outdated after the bridge's own retry (§5.4) |
| 6 | no `init` within 10 s |
| 7 | logged out: the store was deleted (or its deletion failed after `error {store_io}`; then the client deletes the folder); restart only with a new key (§6.4) |
| 64 | *(clarified)* unknown command-line arguments (a person's typo; clients pass none) |

Any other code, or death by a signal, is a crash.

## 10. Guarantees the bridge makes

### 10.1 Storage
- The session store is a SQLite database encrypted with Adiantum (pure-Go SQLite, `ncruces/go-sqlite3`), using the key from `init`. The database, its WAL and its journal are encrypted. Temporary tables stay in memory. Without the key the file cannot be read, and the bridge never recreates a store it cannot open.
- The folder is owner-only (`0700`, files `0600`) on macOS and Linux. On Windows it inherits the user profile's permissions (§14). *(review)* The process runs with umask `077`, so every file is owner-only from its creation, and an existing `storeDir` or `mediaDir` with wider permissions is tightened to `0700` at `init`.
- *(review)* Every commit is durable across a power loss (`PRAGMA synchronous=FULL`), above all the `outboxId` reservation made before a send.
- Besides whatsmeow's own tables, the store holds three small tables: sent `outboxId`s (the last 1,000, with the message id WhatsApp gave them and, *(re-review)* for the backstop, the time each was handed to WhatsApp), the download descriptors of reported voice and image messages (pruned after 90 days), and the JIDs of reported chats (for `unknown_chat` and `aliasOf`). It holds no message text. *(clarified: the third table.)*
- *(clarified)* The key is applied with `PRAGMA hexkey` on each new database connection, never in the file name or URI. The store file is `<storeDir>/store.db` (plus `-wal`/`-shm` while open) and the lock is `<storeDir>/bridge.lock`.
- Only one bridge can use a store at a time (an OS file lock).

### 10.2 Network
- The bridge connects only to WhatsApp hosts: `web.whatsapp.com`, `*.whatsapp.net` and `*.whatsapp.com`. Every connection, including redirects and the WhatsApp Web version check, goes through one allowlist that refuses any other host **before** connecting. The list is compiled into the program. Proxy environment variables are ignored. *(clarified)* Only TCP port 443 is dialed and redirects must stay on `https`; IP literals, `localhost` and bare `whatsapp.net`/`whatsapp.com` are refused. *(review)* An allowed name is resolved by the bridge, and only public addresses are dialed: an answer that points at a loopback, private, link-local, shared (100.64.0.0/10), unspecified, multicast or broadcast address is refused before connecting (`host_blocked`). *(re-review)* Also refused: the IPv4 blocks 0.0.0.0/8, 192.0.0.0/24, 192.0.2.0/24, 198.18.0.0/15, 198.51.100.0/24, 203.0.113.0/24 and 240.0.0.0/4; any IPv6 address outside the global unicast block 2000::/3 (which covers IPv4-compatible `::/96`, local-use NAT64 64:ff9b:1::/48, site-local fec0::/10, discard-only 100::/64, unique-local, link-local and multicast); the IETF block 2001::/23 (Teredo 2001::/32 among others) and documentation 2001:db8::/32. For NAT64 (64:ff9b::/96), 6to4 (2002::/16) and IPv4-mapped (`::ffff:0:0/96`) addresses, the IPv4 address inside is checked against the IPv4 rules. TLS still verifies the certificate for the name. Media downloads use hosts under `whatsapp.net` (for example `mmg.whatsapp.net`, `media-<site>.cdn.whatsapp.net`); the list is confirmed on a live account in the owner's test and changed only by a new release.
- The bridge never fetches `flags.json` or anything else outside WhatsApp.

### 10.3 Behaviour
- No presence is ever sent: the linked device never appears "online".
- Media is downloaded only on `fetch_media`.
- No status updates, broadcast lists or newsletters are read or reported. *(review, corrected)* The address book is never **reported**: WhatsApp's own app-state sync gives every linked device the phone's contact names, and whatsmeow keeps them in the encrypted store (they are used only for the names of reported chats and senders, §5.13). No command reads it out.
- One send at a time, never retried by the bridge, never repeated for the same `outboxId`, and no faster than the backstop (§6.5; for resends inside the connection library, see there).

### 10.4 Diagnostics (stderr)
stderr carries JSON lines `{"t":<ms>,"level":"info|warn|error","event":"<fixed name>","code"?:"…","n"?:<number>}`. There is never message text, a name, a phone number, a JID, a QR string, a pairing code, a key or a path. whatsmeow's own log messages are reduced to their constant format string, without arguments. A client may keep these lines in its own log.

*(review)* The rules that make this hold, and what a client must do:
- `event` is one of the fixed names below; any other name is written as `invalid_event`. `code` is one of the bridge's fixed codes (an error code of §9, a `status` state, a `signal` kind, a logout or pairing reason, a command or event type, or one of `too_long`, `invalid`, `event`, `history`, `command`, `async`, `alias`, `chats`, `media`, `outbox`, `phone`, `qr`), or `redacted`. *(re-review, lead decision)* A stderr `code` is its own namespace, a diagnostic label only: where its text equals an error code of §9, a state or an event type, it names that thing, but it is never an error, a reply or a state change, and a client MUST NOT act on it as if it were one. Only for `event: "whatsmeow"` is `code` a constant format string of the library, prefixed by its module (`Client/Socket: …`), with anything that looks like a number of 5+ digits, a 16+ character hex or base64 run, `@` or `+` replaced by `redacted`.
- The bridge writes its lines to its own copy of stderr and points the process's standard error at the null device (`/dev/null`, `NUL` on Windows). *(re-review: it was a pipe, which a crash report larger than its buffer could fill, hanging the process.)* Whatever else would write to stderr (the Go runtime's report of a crash, with the panic value and stack, or a library print) does not reach the client. A panic inside the bridge's own work is caught and logged as `panic_recovered` with a constant code; the command that caused it gets `error {internal}`.
- A client MUST parse stderr strictly: keep only lines that are this JSON with a listed `event`, and drop (and count) everything else.

Events, by area:

| area | events |
|---|---|
| process and stdio | `started`, `stdin_eof`, `shutdown`, `init_timeout`, `line_dropped` (`code` `too_long` or `invalid`, `n` the count), `emit_failed`, `emit_dropped` (*(re-review)* a line dropped because the bridge is stopping, §6.11; `code` its event type, `n` the count), `panic`, `panic_recovered`, `stop_timeout`, `exit_logged_out` |
| init and store | `ready`, `fatal`, `store_open_failed`, `store_close_failed`, `store_wiped`, `store_wipe_failed`, `store_write_failed`, `media_prune_failed`, `media_stale_deleted` |
| connection and pairing | `status`, `signal`, `connect_failed`, `host_blocked`, `logged_out`, `pairing_started`, `pairing_failed`, `pairing_timeout`, `paired`, `version_refreshed`, `version_refresh_failed` |
| commands | `command_refused`, `send_ok`, `send_failed`, `send_refused`, `send_clock_skew` (*(re-review)* send times after the clock were moved back, §6.5; `n` how many), `fetch_failed`, `group_info_failed`, `joined_groups_failed` |
| history | `history_capped`, `history_done`, `history_download_failed` |
| library | `whatsmeow` |

New names may be added within v1; a client that does not know a name drops the line.

## 11. `flags.json` (a pause switch for clients; the bridge does not read it)

Published at `https://github.com/Aelassal/voolo-whatsapp-bridge/releases/download/flags/flags.json` (a prerelease tagged `flags`):

```json
{"keyId":"6856d52a7f7cded1","payload":"<base64 of the payload bytes>","sig":"<base64 Ed25519 signature over those bytes>"}
```

The payload, before base64, is JSON: `{"v":1,"issuedAt":<ms>,"deepSync":{"enabled":<bool>,"maxBridgeVersion":"<semver>"}}`. A client verifies `sig` over the **exact decoded bytes** with the public key for `keyId`, refuses a payload whose `issuedAt` is lower than one it already accepted, and treats any failure as "no new information", never as "enabled". Public keys (Ed25519, raw 32 bytes, base64); a `keyId` is the first 16 hex characters of the SHA-256 of the raw public key:

| keyId | public key |
|---|---|
| `6856d52a7f7cded1` | `Ei79aPDOwNIFwhQ0MYw2feU2LDx5rlnZiGobiuYKjj4=` |

*(clarified)* `maxBridgeVersion` is `MAJOR.MINOR.PATCH` without a `v` or suffix. The payload bytes are written with no spaces, in the member order shown. The signing tool is `cmd/flags-sign`; the `flags` workflow runs it in a protected environment that needs the maintainer's approval for every run.

The file is changed only by this repository's `flags` workflow, which the maintainer approves by hand. The `flags` prerelease is never marked "latest". *(review)* The workflow runs only from `main`: the `flags` environment allows only `main` (without an administrator bypass), the job refuses any other ref, and a step checks again before the key is read. Release tags `v*` are protected by a ruleset.

## 12. Trying it from a shell

```sh
mkdir -p /tmp/wa/store /tmp/wa/media
KEY=$(head -c 32 /dev/urandom | xxd -p -c 64)   # keep it: the store needs the same key next time
( printf '{"v":1,"id":"01M3C03V95B37ZT9G5JYKGSGMP","type":"init","ts":%s,"payload":{"storeKey":"%s","storeDir":"/tmp/wa/store","mediaDir":"/tmp/wa/media","deviceName":"Shell","limits":{"historyDays":7,"historyMaxPerChat":200,"imageMaxBytes":2097152,"voiceMaxSeconds":300}}}\n' "$(date +%s000)" "$KEY"
  printf '{"v":1,"id":"01M3C03VAAH57TJGKRZX4MSSPA","type":"pair_qr","ts":%s,"payload":{}}\n' "$(date +%s000)"
  cat ) | ./voolo-whatsapp-bridge | tee /tmp/wa/out.jsonl
# In another terminal: take the "code" of the newest "qr" line and render it, for example
#   grep '"type":"qr"' /tmp/wa/out.jsonl | tail -1 | jq -r .payload.code | qrencode -t ansiutf8
# then scan it with WhatsApp → Settings → Linked devices → Link a device.
# History batches stop after 4 until you acknowledge them: type {"v":1,"id":"<new ULID>","type":"ack","ts":0,"payload":{"seq":1}} and Enter.
```

Any ULID generator works for `id` (the ones above are only examples). Real ids MUST be unique per session.

## 13. Release files and versions

*(review, lead decision 2026-09-25)*

- A release is a GitHub Release of this repository tagged `vMAJOR.MINOR.PATCH`. Its binaries are named exactly `voolo-whatsapp-bridge-<goos>-<goarch>`, plus `.exe` on Windows: `voolo-whatsapp-bridge-windows-amd64.exe`, `voolo-whatsapp-bridge-darwin-arm64`, `voolo-whatsapp-bridge-darwin-amd64`, `voolo-whatsapp-bridge-linux-amd64`. Next to them: `SHA256SUMS` (one line per binary, `<sha256>  <name>`), `go-licenses.csv`, `go-licenses-README.md`, and a build-provenance attestation for each binary.
- The version inside the binary (`--version`, `hello.version`) is the tag without its `v`. Versions are compared as three numbers, never with a `v` (§5.1, §11).
- Every release is rebuilt from its tag on a second runner and must give the same `SHA256SUMS` before it is published.

## 14. Known limits

*(review)* Findings of the P1-A security review that are not fully closed, with the reason:

- **Windows permissions** (review L5). On Windows the store and media folders are not given an explicit owner-only ACL; they inherit the per-user profile's ACL (`%APPDATA%` or `%LOCALAPPDATA%` is readable only by the user, SYSTEM and administrators by default). An explicit protected DACL needs testing on real Windows machines and is left to the Windows hardening task of the client. The store itself is encrypted either way.
- **Windows crash output** (review M1). On Windows the redirection of the process's standard error (§10.4) is built and type-checked, but not yet run in a test on Windows; a client MUST drop non-JSON stderr lines in any case.
- **The backstop counts per store** (§6.5). It survives restarts through the store, but not a deleted store; it is a last defence, the client's own caps come first.
- **Duration of non-Ogg voice** (§6.8). A received voice note that is not Ogg Opus keeps the duration its sender declared; its size is still capped on the real bytes.
- **Duration of any received voice** *(re-review)*. The file and the declared duration are both the sender's word; the bridge takes the longer where they disagree and never less than the size allows at Opus's highest bitrate (§6.6, §6.8), but a sender can still make a file read longer or shorter within those bounds. Only `voiceMaxBytes` is a hard limit.

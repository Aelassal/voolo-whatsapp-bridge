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
- Words in capitals (MUST, MUST NOT, SHOULD) have their usual meaning.

## 1. Transport

- The client starts the program as a child process **with no arguments** and connects its stdin, stdout and stderr to pipes. There is no socket, port or named pipe.
- The program's only other arguments are for people: `--version`, `--license` and `--source` print text and exit.
- **stdout** carries protocol lines only. **stdin** carries protocol lines only. **stderr** carries diagnostics (§10) and is never part of the protocol.
- **Framing:** one JSON object per line, UTF-8, ending with `\n` (a `\r` before it is tolerated on input). JSON strings never contain a raw newline, so a line is always one message.
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
| `id` | string | A ULID: 26 characters of Crockford base32 (`0-9 A-Z` without `I L O U`), unique per sender. |
| `type` | string | `^[a-z][a-z0-9_]{0,63}$`. Commands and events are listed in §5–§7. |
| `ts` | integer | Sender's clock, Unix milliseconds UTC. |
| `payload` | object | Per type. `{}` when empty. |

**Replies.** A reply to a command carries `payload.replyTo` = the command's `id`. Every command gets exactly one final reply (`ok`, a typed result, or `error`), except `ack`, which gets none. Events that are not replies have no `replyTo`.

**Strictness.** The bridge decodes **commands strictly**: an unknown field anywhere in a command's payload, a wrong type or a value out of range gets `error {code: bad_request}` and the command is not executed. *(clarified)* Member names are matched exactly, including case (`chatjid` is an unknown field, not `chatJid`); a duplicated member name, a `null` value for any field, and a number written with a fraction or exponent where an integer is expected are all `bad_request`. The order of checks is: envelope, `v`, known `type` (`unknown_command`), `init` order (`not_initialized`), payload (`bad_request`). Clients SHOULD decode **events leniently**: ignore fields they do not know, so that a newer bridge can add fields within v1. A client ignores an event `type` it does not know; the bridge answers a command `type` it does not know with `error {code: unknown_command}` and does nothing else. A line that is not valid JSON, or not an object with exactly the five envelope fields, is dropped and counted.

## 3. Identifiers and values

- **Chat JID:** WhatsApp's address of a chat, without a device part. `<digits>@s.whatsapp.net` (a person, by phone number), `<digits>@lid` (a person, by WhatsApp's linked id, when no phone number is known), `<digits>-<digits>@g.us` or `<digits>@g.us` (a group). Max 128 characters. Broadcast lists, `status@broadcast` and newsletters are never reported.
- **Sender JID:** a person's JID as above. In a group it names the author, and for your own messages it is your own JID.
- **Message id:** WhatsApp's message id, a string of 1–128 characters from `[A-Za-z0-9._-]`. A message is identified by `(chatJid, messageId)`.
- **Phone number** (in `pair_phone`): digits only, 7–15 of them, international format without `+` or a leading `0` (for example `201001234567`).
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
- If `init` has not arrived within **10 s** of `hello`, the bridge exits with code 2.
- A second `init` gets `error {code: already_initialized}`.

## 5. Events (bridge → client)

Examples of every event are in `examples/bridge-to-app/`. Optional fields are marked `?`.

### 5.1 `hello`
First line the bridge writes.
`{bridge: "voolo-whatsapp-bridge", version: "<semver>", protocol: 1, os: "windows|darwin|linux", arch: "amd64|arm64"}`

### 5.2 `ready` (reply to `init`)
`{replyTo, paired: bool, account?: {jid, pushName?}}`. When `paired` is true, the bridge starts connecting immediately.

### 5.3 `status`
The connection state, sent on every change.
`{state}` where `state` is:
- `unpaired`: no linked account in the store. Waiting for `pair_qr` or `pair_phone`.
- `connecting`: first connection after start or pairing.
- `connected`: online and receiving.
- `reconnecting`: the connection dropped. The bridge retries by itself.
- `stopped`: the bridge will not reconnect (after `logged_out`, or before exiting). *(clarified)* Also after `signal {temp_banned}`, `signal {stream_replaced}` and `signal {connect_failure}`: WhatsApp closed the connection and the bridge does not dial again by itself. The client decides (restart the bridge later, or not).

### 5.4 `signal`
A provider event that suggests throttling, a ban, a forced update or a duplicate session. The bridge reports these and never acts on them beyond what §6 says. The client decides what to do.
`{kind, code?, expiresInMs?}` where `kind` is:
- `rate_limited`: WhatsApp answered with a rate-limit error (for example 429). *(clarified)* `code` is `429`. It is sent together with the `error {code: rate_limited}` of a refused send, and for a connect failure with reason 429.
- `temp_banned`: temporary ban. `code` is WhatsApp's reason code (101, 102, 103, 104, 106, …), and `expiresInMs` is present if WhatsApp gave one.
- `stream_replaced`: another client connected with the same session. The bridge stops reconnecting.
- `client_outdated`: WhatsApp rejected the client version (405). The bridge refreshes the WhatsApp Web version once and retries; a second rejection is reported again.
- `connect_failure`: another connection failure, with `code` (for example 500, 503).
- `stream_error`: an unknown stream error, with `code` as a string.
- `keepalive_timeout`: the server stopped answering keep-alives.

### 5.5 `qr` (repeated reply to `pair_qr`)
`{replyTo, code, expiresAt, index}`. `code` is the string to encode in a QR image (max 4,096 characters). `index` counts from 0. The first code lives about 60 s and the next ones about 20 s each; after about 160 s the bridge sends `pair_failed {reason: timeout}`. A newer `qr` replaces the previous one.

### 5.6 `pair_code` (reply to `pair_phone`)
`{replyTo, code}`: an 8-character code, shown to the user as `XXXX-XXXX`, which they type on the phone (WhatsApp → Linked devices → Link a device → Link with phone number instead). The phone may show a notification instead. The code expires with the pairing window (§5.5).

### 5.7 `paired`
`{jid, lid?, pushName?, businessName?}`: linking succeeded. `status` becomes `connecting`, then `connected`, and history arrives (§7).

### 5.8 `pair_failed`
`{replyTo, reason}` where `reason` is `timeout`, `rejected` (the phone refused or the code was wrong), `client_outdated` or `error`. The bridge is back to `unpaired`.

### 5.9 `logged_out`
`{reason, code?}`: the account is no longer linked. `reason` is:
- `user`: after the client's own `logout` command.
- `device_removed`: removed from the phone's Linked devices, or after long phone inactivity (code 401).
- `primary_gone`: the phone's account was logged out or moved (code 403).
- `banned`: code 406 or any logout WhatsApp describes as a ban.
- `unknown`: anything else.

The bridge has already deleted the linked device from its store when it sends this event. `status` becomes `stopped`, then `unpaired`.

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
Starts QR linking. Fails with `already_paired` if the store already has an account. *(clarified)* Fails with `busy` while another `pair_qr` or `pair_phone` is running, and with `pair_failed` (retryable) if the linking socket could not be opened within 30 s.

### 6.3 `pair_phone`
`{phone}` (§3). Starts linking by code. Errors: `phone_invalid`, `already_paired`, `pair_failed`. *(clarified)* Also `busy`, as in §6.2. WhatsApp requires the name of a code-linked device to have the form `Browser (OS)`, so the phone's Linked devices list shows `Chrome (Windows)`, `Chrome (Mac OS)` or `Chrome (Linux)` for such a link (approved by the owner on 2026-09-25). No `qr` event is sent during a phone-number link; its window is the same as the QR window (§5.5).

### 6.4 `logout`
Unlinks this device on WhatsApp's side, deletes it from the store and sends `logged_out {reason: user}`. The client then usually sends `shutdown` and deletes the store folder. *(clarified)* The bridge deletes the whole store file (`store.db` and its `-wal`/`-shm`), then opens a new, empty store with the same key, so it can be linked again without a restart. Events in order: `logged_out`, `status {stopped}`, `status {unpaired}`, then `ok`. Errors: `not_paired`; `not_connected` (retryable; nothing was unlinked); `timeout`. A remote logout (§5.9) deletes the store the same way.

### 6.5 `send_text`
`{chatJid, text, outboxId, quotedMessageId?}`
- `text`: 1–65,536 characters.
- `outboxId`: a ULID chosen by the client, one per message the user asked to send.
- Only **one** send (`send_text` or `send_media`) may be in flight. A second one gets `busy`.
- The bridge remembers the last 1,000 `outboxId`s in the store, across restarts. A repeated one gets `duplicate_outbox_id` and nothing is sent.
- The bridge never retries a send by itself. Errors: `not_paired`, `not_connected` (nothing was sent), `busy`, `duplicate_outbox_id`, `unknown_chat`, `rate_limited`, `send_failed`, `timeout`.
- *(clarified)* Checks run in this order: `not_paired`, `not_connected`, `busy`, `unknown_chat`, `duplicate_outbox_id`. A command refused by one of them is not remembered, so the same `outboxId` can be used again. Once those checks pass, the `outboxId` is remembered **before** the message is handed to WhatsApp, whatever happens next: after `timeout`, `send_failed` or `rate_limited`, the same `outboxId` gets `duplicate_outbox_id`, and a retry is a new user action with a new `outboxId`.
- *(clarified)* `unknown_chat`: the bridge only sends to a chat it has reported (in this run or an earlier one, remembered in the store) or to a group the account belongs to. A phone number that never appeared is refused.
- *(clarified)* The single send slot is freed before `send_result` or the send's `error` is written, so the next send may follow the reply immediately.
- *(clarified)* `quotedMessageId` refers to a message in the same chat. The bridge adds the quoted message's author when it has seen that message recently; it does not send a copy of the quoted content.

### 6.6 `send_media`
`{chatJid, outboxId, kind: "image"|"voice"|"file", path, key, sha256, mime, caption?, fileName?}`. The client writes the file in the §8 format under `mediaDir` with its own fresh key. The bridge reads, decrypts and checks it, uploads it, deletes the file, and replies. Same rules and errors as §6.5, plus `media_too_large` and `media_invalid`.
- *(clarified)* `path` must be `<mediaDir>/<32 lowercase hex>.bin`, a regular file (not a link). It is deleted once read, in every case. Caps: `image` ≤ `limits.imageMaxBytes`; `voice` ≤ `limits.voiceMaxBytes` and, for Ogg Opus, ≤ `limits.voiceMaxSeconds` (read from the file); `file` ≤ 33,554,432 bytes. `voice` is sent as a voice note (push-to-talk). `mime`: `type/subtype` with optional parameters, for example `audio/ogg; codecs=opus`.
- *(added within v1)* `fileName?`: 1–255 characters, no `/`, `\` or control characters; the name shown for `kind: file` (default `file` plus an extension from `mime`).

### 6.7 `mark_read`
`{chatJid, messageIds: [id] (1–50), senderJid?}`. Sends read receipts for these received messages (`senderJid` is required in groups). WhatsApp applies the user's own read-receipt privacy setting. *(clarified)* In a group, all `messageIds` must be from that one `senderJid`; send one `mark_read` per sender. A group `mark_read` without `senderJid` is `bad_request`. Errors: `not_paired`, `not_connected`, `timeout`, `internal`.

### 6.8 `fetch_media`
`{chatJid, messageId}`. Downloads the media of a message the bridge has reported, subject to the limits of `init`. Errors: `unknown_message`, `media_too_large` (checked before download), `media_expired` (no longer on WhatsApp's servers), `media_unavailable`, `not_connected`, `timeout`.
- *(clarified)* v1 fetches only `voice` and `image` messages (owner decision: video, documents, stickers and other audio stay on the phone). For any other kind the bridge keeps no download descriptor, so `fetch_media` answers `unknown_message`. `chatJid` is the JID under which the message was reported. At most two downloads run at once; more wait.

### 6.9 `ack`
`{seq}`. Acknowledges a `history_batch` (§7). No reply. *(clarified)* An `ack` acknowledges that batch **and every earlier one**; an unknown or repeated `seq` is ignored.

### 6.10 `ping`
Liveness. A client SHOULD ping every 30 s and restart the bridge after two missed `pong`s.

### 6.11 `shutdown`
The bridge disconnects, flushes and closes the store, replies `ok` and exits with code 0 within 3 s. The same happens on stdin EOF (without the reply).

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

1. `fetch_media` → the bridge downloads the file from WhatsApp (whatsmeow verifies WhatsApp's hashes) and checks the size and duration limits again.
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

**Exit codes:** 0 normal · 1 unexpected crash · 2 no `init` within 10 s · 3 store key invalid · 4 store locked · 5 client outdated after the retry · *(clarified)* 64 unknown command-line arguments (a person's typo; clients pass none).

## 10. Guarantees the bridge makes

### 10.1 Storage
- The session store is a SQLite database encrypted with Adiantum (pure-Go SQLite, `ncruces/go-sqlite3`), using the key from `init`. The database, its WAL and its journal are encrypted. Temporary tables stay in memory. Without the key the file cannot be read, and the bridge never recreates a store it cannot open.
- The folder is owner-only (`0700`, files `0600`) on macOS and Linux. On Windows it inherits the user profile's permissions.
- Besides whatsmeow's own tables, the store holds three small tables: sent `outboxId`s (the last 1,000, with the message id WhatsApp gave them), the download descriptors of reported voice and image messages (pruned after 90 days), and the JIDs of reported chats (for `unknown_chat` and `aliasOf`). It holds no message text. *(clarified: the third table.)*
- *(clarified)* The key is applied with `PRAGMA hexkey` on each new database connection, never in the file name or URI. The store file is `<storeDir>/store.db` (plus `-wal`/`-shm` while open) and the lock is `<storeDir>/bridge.lock`.
- Only one bridge can use a store at a time (an OS file lock).

### 10.2 Network
- The bridge connects only to WhatsApp hosts: `web.whatsapp.com`, `*.whatsapp.net` and `*.whatsapp.com`. Every connection, including redirects and the WhatsApp Web version check, goes through one allowlist that refuses any other host **before** connecting. The list is compiled into the program. Proxy environment variables are ignored. *(clarified)* Only TCP port 443 is dialed and redirects must stay on `https`; IP literals, `localhost` and bare `whatsapp.net`/`whatsapp.com` are refused. Media downloads use hosts under `whatsapp.net` (for example `mmg.whatsapp.net`, `media-<site>.cdn.whatsapp.net`); the list is confirmed on a live account in the owner's test and changed only by a new release.
- The bridge never fetches `flags.json` or anything else outside WhatsApp.

### 10.3 Behaviour
- No presence is ever sent: the linked device never appears "online".
- Media is downloaded only on `fetch_media`.
- No address book, status updates, broadcast lists or newsletters are read or reported.
- One send at a time, never retried, never repeated for the same `outboxId`.

### 10.4 Diagnostics (stderr)
stderr carries JSON lines `{"t":<ms>,"level":"info|warn|error","event":"<fixed name>","code"?:"…","n"?:<number>}`. There is never message text, a name, a phone number, a JID, a QR string, a pairing code, a key or a path. whatsmeow's own log messages are reduced to their constant format string, without arguments. A client may keep these lines in its own log.

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

The file is changed only by this repository's `flags` workflow, which the maintainer approves by hand. The `flags` prerelease is never marked "latest".

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

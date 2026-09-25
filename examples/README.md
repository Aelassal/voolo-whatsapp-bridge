<!--
SPDX-License-Identifier: CC0-1.0

To the extent possible under law, the authors have waived all copyright and related or
neighboring rights to the files in this folder (examples/). This work is published from: Egypt.
https://creativecommons.org/publicdomain/zero/1.0/
-->

# Protocol examples (CC0-1.0)

One message per file, exactly as it travels on the wire: a single JSON line ending in `\n` (`PROTOCOL.md` §1–§2). Every file in this folder is dedicated to the public domain, so any client can copy these files into its own test suite whatever its licence. The program itself is GPL-3.0.

- `app-to-bridge/`: one valid example of every command (§6). The bridge MUST accept each of them.
- `bridge-to-app/`: one or more valid examples of every event (§5). A client MUST accept each of them.
- `invalid/`: lines a correct peer MUST refuse:

| File | Expected |
|---|---|
| `send_text-extra-field.json` | bridge: `error {code: bad_request}`, nothing sent (a recipient list is not part of the protocol) |
| `send_text-schedule.json` | bridge: `bad_request` (no scheduling) |
| `send_text-empty.json` | bridge: `bad_request` (text is 1–65,536 characters) |
| `pair_phone-leading-zero.json` | bridge: `bad_request` or `phone_invalid` |
| `init-short-key.json` | bridge: `bad_request` (the key is 64 hex characters) |
| `init-limits-above-ceiling.json` | bridge: `bad_request` (limits cannot exceed the ceilings) |
| `mark_read-too-many.json` | bridge: `bad_request` (at most 50 ids) |
| `wrong-version.json` | either side: not processed (`v` is not 1) |
| `extra-envelope-field.json` | either side: dropped (the envelope has exactly five fields) |
| `media_ready-path-traversal.json` | client: refuse (the path leaves `mediaDir`) |

All ids, numbers (`1555010xxxx`, fictional), names, keys and codes are made up. Timestamps are 2026-09-25T10:00:00Z (`1790330400000`) plus or minus offsets.

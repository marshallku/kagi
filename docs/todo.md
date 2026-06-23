# Future work

Ordered roughly by usefulness for automation.

## ~~v2 protocol migration~~ ✓ done (2026-06-23)

Kagi moved the Assistant to **`assistant.kagi.com`** with a JSON REST API
under `/api/*`; the old `kagi.com/assistant/*` protocol was decommissioned.
The client layer was rewritten against v2 and verified end-to-end (chat
new/follow-up/stream, base model + custom assistant, threads list/show/
rename/search, models, profiles, assistants show, HTTP server). Auth
(`kagi_session` cookie shared on `.kagi.com`) was the only piece reused as-is.

What changed:

1. `client/client.go` — host split (`APIBase = assistant.kagi.com` for `/api/*`,
   `BaseURL = kagi.com` for sign-in). `apiDo` JSON helper with 401-relogin and
   404→ErrNotFound. Three-step chat (`POST /api/conversations` →
   `POST /api/branches/{b}/messages` → `GET .../stream`). `relaySSE` parses the
   `text/event-stream` and re-emits v1-style events (`thread.json`/`tokens.json`/
   `new_message.json`) so `Send` and the HTTP server's snoop+relay stay unchanged.
2. `client/discovery.go` — `FetchInit` over `/api/init`; `FetchProfiles`
   synthesises the v1 AssistantProfile list from `models.models[]` +
   `custom_assistants[]` so `kagi models`/`kagi profiles` and the server
   discovery handlers compile unchanged.
3. `client/threads.go` — conversations mapped onto the v1 thread types/methods.
4. `client/assistants.go` — `/api/assistants` JSON REST CRUD.
5. CLI/server — relaxed the "profile required" rule (v2 needs model **or**
   profile); everything else (command verbs, HTTP routes, `ChatResult`) preserved.
6. `client/client_test.go` — table-driven `relaySSE` tests (the fragile bit).

### Follow-ups (deferred)

- **Stale config caveat**: a v1 `profile` uuid in `~/.config/kagi/config.json`
  no longer resolves and yields a (now friendlier) "profile not found" error.
  Users must re-set it from `kagi profiles`. Consider auto-detecting/clearing.
- **Conversation list pagination**: `ListThreads` reads `/api/init` (likely a
  capped page). A dedicated paginated `/api/conversations?...` endpoint exists;
  wire cursor pagination if large histories truncate.
- **New v2 capabilities** not yet surfaced: folders (`/api/folders*`), file
  upload (`/api/upload*`, multimodal), branch management
  (`/api/conversations/{id}/branches`, re-roll/edit), sharing
  (`/api/branches/{b}/share`), `thinking_preset` for reasoning models,
  `/api/conversations/by-legacy/{id}` (v1→v2 id mapping).
- **README**: refresh the stale model-id examples (e.g. `grok-4-20`).

## ~~Profile / model discovery~~ ✓ done

`kagi models` and `kagi profiles` parse `<div id="json-profile-list">` from
the `/assistant` page. `kagi config set model|profile` persists defaults.
See `docs/api.md` for the JSON schema.

Open: a small subset of base profiles in the list have empty `id` and
aren't selectable via the API (`ChatGPT`, `Claude 4.5 Haiku`, etc. — Kagi
likely materializes a synthetic profile when the user picks them in the UI).
We just hide them from `kagi profiles`. If we ever want them, capture what
the UI sends when one is picked.

## ~~Thread list / history~~ ✓ done

- `POST /assistant/thread_list` (cursor + limit). Response is the streamed
  Kagi protocol with a `thread_list.html` event carrying server-rendered
  `<li class="thread">` HTML — parsed back to typed structs by
  `client.parseThreadHTML`.
- `GET /assistant/<id>` carries two embedded JSON islands (`json-thread`,
  `json-message-list`) with the full thread + per-turn data. The chat
  bubbles in the page are populated client-side from these — don't bother
  scraping them.
- `POST /assistant/thread_modify` and `/assistant/thread_delete` take the
  same `{threads:[{id,title,saved,shared,tag_ids}]}` envelope.
- `POST /assistant/search` returns a flat `[{rank, snippet, message_id,
  thread_id, branch_id}]` array (no streaming).
- CLI: `kagi threads list/show/search/rename/save/unsave/share/unshare/delete`.

The "head message of thread X" lookup now exists, so `kagi chat -t <id>`
auto-resolves `--parent` from the last turn's `id`. Manual `--parent` is
still accepted for explicit control or for restoring an old branch.

## File upload (multimodal) — **next**

Captured but not wired in. The composer's paperclip just adds files to a
local queue; submission switches the prompt request from JSON to
`multipart/form-data` with these parts:

- `state` — the same JSON envelope, as a `application/json` blob.
- `file` (one part per attachment) — the raw file bytes.
- `__kagithumbnail` (one per image) — an 84×84 60%-quality JPEG the client
  pre-generates, used for the in-UI preview.

Plan when picking this up:

1. Add `client.PromptRequest.Files []PromptFile` and a `Send`/`Stream`
   variant that switches to multipart when `Files` is non-empty.
2. Image thumbnails: pure-Go resize is heavy; either skip the thumb (Kagi
   accepts uploads without one) or pull in `golang.org/x/image/draw`. Skip
   for v1, document the trade-off.
3. CLI: `kagi chat -f file1.pdf -f file2.png "summarize these"`.
4. Server: accept `multipart/form-data` on `/chat` with the same field
   layout, or expose `/chat/upload` returning a file-id (none of the
   captured traffic does this — uploads always ride the prompt request).

Worth doing — this is the only major capability the browser has that the
CLI doesn't.

## Branch management

- Re-rolling a response creates a new `branch_id` (`branch_list` in
  `json-message-list` shows existing branches per turn).
- `POST /assistant/message_regenerate` creates the new branch (captured in
  the JS bundle, not yet replayed).
- `POST /assistant/message_edit` rewrites a user message and re-runs the
  thread from that point.
- Probably low priority for automation.

## Lens support

- `profile.lens_id` is currently always `null`. Lenses scope the
  assistant's search to specific domains. The custom-assistant create form
  captured a list of lens ids (`0` = none, then small ints like `4248`,
  `29`, `5648`, ...). `kagi assistants create --lens <id>` already plumbs
  this through; the pieces missing are (a) a `kagi lenses list` command to
  show the available lens ids by name, and (b) per-prompt lens override
  via `chat --lens <id>` (currently the lens is whatever the profile sets).

## ~~Custom Assistant — update flow~~ ✓ done

`kagi assistants update <uuid> [-n] [-m] [--prompt] [--bang] [--internet|
--no-internet] [--personalize|--no-personalize] [--lens]`. Partial updates
work because the Go client first scrapes `/settings/custom_assistant?id=<uuid>`
to read the current state (name, base_model, bang_trigger,
custom_instructions, lens, internet/personalizations checkboxes), applies
only the flags the user actually passed, and then re-submits the whole form.
Round-trip verified: rename-only preserves prompt + flags.

`kagi assistants show <uuid>` is the read-side of the same scraper — useful
for review or for piping into a script that builds a new spec.

## ~~Session refresh~~ ✓ done

`kagi login` + transparent auto-relogin on auth fail (404/401/403/302). Session
persists in OS keyring via `zalando/go-keyring`. See `docs/decisions.md`.

### Limitation: single-account-per-machine

The keyring entry is keyed only by service name (`kagi` / `session`).
Switching `KAGI_EMAIL` to a second account on the same machine does **not**
trigger a fresh login — `resolveSession` finds the cached session for the
previous account first and uses it. Workarounds today:

- `kagi logout` between accounts.
- Set `KAGI_SESSION` explicitly (overrides keyring).

A proper fix would key the keyring entry by email (or by a stable user id
returned from /assistant), so different `KAGI_EMAIL` values fetch different
cached sessions. Defer until someone actually has this problem.

## Unit tests

The stream parser (`client.parseStream`) is the bit most likely to break if
Kagi tweaks the protocol. A small table-driven test against synthetic
NUL-delimited input would catch regressions cheaply. Same logic now also
runs through `client.streamJSON` (used by thread_list / thread_modify /
thread_delete) — adding a fixture for the `thread_list.html` payload would
guard against the HTML-shape changes too.

## HTTP API parity — `login` / `logout` / `config` stay CLI-only

Deliberate scope decision: the embedded HTTP server exposes the per-Kagi-call
operations (chat, threads, assistants, discovery, resume) but NOT the
per-host admin operations (`kagi login`, `kagi logout`, `kagi config
get/set`). Reasons:

- `login` would accept raw credentials over HTTP; even on loopback they
  end up in shell history / process logs. The CLI uses TTY-silent reads
  precisely to avoid this.
- `logout` is a destructive op on the host's keyring — easy to fire by
  accident from a remote caller.
- `config get/set` modifies `~/.config/kagi/config.json` on the server's
  host, which doesn't compose with multi-tenant access patterns.

The server already pulls credentials from the keyring/env at boot, so a
legitimate API caller never needs to hit a login endpoint. Run `kagi
login` once on the host (or set `KAGI_EMAIL`/`KAGI_PASSWORD` in the
environment) before `kagi serve`.

## Integration with `~/dev/life-assistant`

- The Discord bot currently shells out to `claude -p` for AI inference. Could
  add a `kagi:` plugin that uses the HTTP server for cheaper / different model
  routing.
- Would need the server to support a default profile (or accept profile_id per
  request, which it already does).

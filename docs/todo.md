# Future work

Ordered roughly by usefulness for automation.

## Profile / model discovery

- `GET /assistant` (the page itself) renders the profile dropdown server-side.
  Could parse the HTML for `data-model` and `data-profile-id` attributes plus
  the `/settings?p=custom_assistant&id=<UUID>` links to enumerate profiles.
- Alternatively, hunt for a JSON endpoint that returns profile list (likely
  exists for the settings page).
- Once we have it, add `kagi profiles` subcommand and a `default-profile`
  fallback so users don't need to set `KAGI_PROFILE_ID`.

## Thread list / history

- Sidebar loads threads on `/assistant` page load. Likely backed by a JSON
  endpoint like `/assistant/threads` or similar — needs capture.
- Add `kagi threads list` and `kagi threads show <id>` so callers can resume
  conversations without holding state externally.
- Once we have history, the "head message of thread X" lookup becomes
  feasible — we could relax the `--parent` requirement.

## File upload (multimodal)

- The composer area has a paperclip icon (`📎`). Drag-drop or click likely
  POSTs to a `/assistant/upload` style endpoint and returns a file id that
  goes into `focus.documents` (the `documents: []` field in the captured
  responses).
- Add `kagi chat -f file1.pdf -f file2.png "summarize these"`.

## Branch management

- Re-rolling a response creates a new `branch_id`. Currently we always pin to
  the zero UUID. To support re-rolls / forks, we'd need to:
  - Capture the re-roll request (likely the same endpoint with a non-zero
    `branch_id`).
  - Parse `messages.json[].branch_list` to know which branches exist.
- Probably low priority for automation.

## Lens support

- `profile.lens_id` is currently always `null`. Lenses scope the assistant's
  search to specific domains. Capture would just be: open a lens-enabled
  profile, send a prompt, see what lens_id is included.

## Custom Assistant CRUD

- `/settings?p=custom_assistant` page handles create/edit/delete. Would let us
  manage profiles programmatically. Lower priority — UI is fine for this.

## ~~Session refresh~~ ✓ done

`kagi login` + transparent auto-relogin on auth fail (404/401/403/302). Session
persists in OS keyring via `zalando/go-keyring`. See `docs/decisions.md`.

### Limitation: single-account-per-machine

The keyring entry is keyed only by service name (`kagi-cli` / `session`).
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
NUL-delimited input would catch regressions cheaply.

## Integration with `~/dev/life-assistant`

- The Discord bot currently shells out to `claude -p` for AI inference. Could
  add a `kagi:` plugin that uses the HTTP server for cheaper / different model
  routing.
- Would need the server to support a default profile (or accept profile_id per
  request, which it already does).

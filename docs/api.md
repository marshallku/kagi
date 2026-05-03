# Kagi Assistant — reverse-engineering notes

Captured 2026-04-28 by driving a logged-in browser via the ai-browser MCP and
inspecting network traffic for `kagi.com/assistant`.

## Authentication

Cookie-based; no separate auth token, no CSRF for the streaming endpoint.

| Cookie           | Domain     | Notes                                   |
| ---------------- | ---------- | --------------------------------------- |
| `kagi_session`   | `.kagi.com`| HttpOnly, Secure, SameSite=Lax. Required for `/assistant/prompt`. |
| `_kagi_search_`  | `kagi.com` | Search session — not needed for the prompt API. |

Sign-in form posts a `_csrf` hidden field, but follow-up XHRs to
`/assistant/prompt` do **not** require it.

## Endpoint

Single endpoint handles both new conversations and follow-ups:

```
POST https://kagi.com/assistant/prompt
Cookie: kagi_session=...
Accept: application/vnd.kagi.stream
Content-Type: application/json
Origin: https://kagi.com
Referer: https://kagi.com/assistant
User-Agent: <any normal browser UA>
```

Response is `Transfer-Encoding: chunked`, `Content-Encoding: gzip`,
`Content-Type: text/html` (yes, the streaming media type is `text/html` despite
the `Accept` we send).

### Request — new thread

```json
{
  "focus": {
    "thread_id": null,
    "branch_id": "00000000-0000-4000-0000-000000000000",
    "prompt": "What is 2+2? Answer in one word."
  },
  "profile": {
    "id": "85988101-51a4-4e88-8a27-36231e98fae2",
    "personalizations": true,
    "internet_access": true,
    "model": "grok-4-20",
    "lens_id": null
  },
  "threads": [{ "tag_ids": [], "saved": true, "shared": false }]
}
```

### Request — follow-up to existing thread

```json
{
  "focus": {
    "thread_id": "751d32f2-bef8-43ae-b911-77de0afaed2e",
    "branch_id": "00000000-0000-4000-0000-000000000000",
    "prompt": "And 3+3?",
    "message_id": "a5d5ad16-e537-4190-9218-45895818f55d"
  },
  "profile": { "...same shape as above..." }
}
```

Differences vs new:

- `focus.thread_id` is a UUID (not null).
- `focus.message_id` is the **parent** message UUID (the one being replied to).
- `threads` array is omitted entirely.

The `branch_id` is always the zero UUID (`00000000-0000-4000-0000-000000000000`)
unless the thread has multiple branches (re-rolled responses). We don't yet
exercise multi-branch flows.

## Response stream protocol

`application/vnd.kagi.stream` is a custom NUL-delimited record format.

Each record: `<type>:<payload>\0` where `<type>` is a string like
`tokens.json`, `<payload>` is JSON or HTML, and `\0` (NUL byte, `0x00`)
terminates the record. Records may contain newlines internally — only the
NUL is the delimiter. Newlines after `\0` are cosmetic.

### Event types

| Type                  | Payload                                                                |
| --------------------- | ---------------------------------------------------------------------- |
| `hi`                  | `{v, trace}` — server version + trace id, sent first.                  |
| `thread.html`         | `<li>` HTML for the sidebar entry.                                     |
| `thread.json`         | `{id, title, ack, created_at, saved, shared, branch_id, tag_ids}`.     |
| `messages.json`       | Array of prior messages in the thread (empty for new threads).         |
| `new_message.json`    | The pending message; emitted twice — first with `state: "waiting"`, last with `state: "done"` containing final `reply` (HTML) and `md` (Markdown). |
| `tokens.json`         | `{text, id, padding}` — incremental tokens. **`text` is cumulative**, not a delta. `padding` is a random string (BREACH-attack mitigation). |

The CLI prints from `new_message.json.md` at completion. The `--stream` mode
diffs `tokens.json.text` against the last seen value to emit incremental output
(but `text` is HTML, not Markdown — only the final `md` field is plain text).

### Title generation

`thread.json` is emitted twice. The first carries the user's prompt as the
title; the second (after the response is generated) carries an LLM-generated
title (e.g. "What is 2+2?" → "What Is 2+2?"). The client tracks the latest.

## Models

Discovered by inspecting the model-picker dropdown HTML on `/assistant`. List
is dynamic; check the page directly to be sure.

```
ki_quick                       Kagi quick (cheap default)
ki_research                    Kagi research
ki_deep_research               Kagi deep research
grok-4-20                      Grok 4.20 (xAI)
claude-4-sonnet
claude-4-sonnet-thinking
claude-4-7-opus-thinking
kimi-k2-5
kimi-k2-5-thinking
glm-4-7-thinking
glm-5-1
glm-5-1-thinking
```

The `profile.model` field overrides whatever default model the profile points
to. Combined with `profile.id`, you can use a Custom Assistant's
system-prompt/tooling but route to a different underlying model.

## Profiles

A "profile" in Kagi is a Custom Assistant — system prompt, default model,
internet access toggle, lens, etc. The `profile.id` is a UUID.

### Discovery: `<div id="json-profile-list" hidden>`

The `/assistant` page embeds the **entire profile catalog** as escaped JSON
inside a hidden div:

```html
<div id="json-profile-list" hidden>{&quot;profiles&quot;:[{&quot;id&quot;:&quot;...&quot;,...}]}</div>
```

Decode with `html.UnescapeString` then `json.Unmarshal`. Each entry:

```json
{
  "id": "e47dcf40-61fc-4da5-99a0-2d403ac41c00",
  "name": "수진",
  "model": "claude-4-sonnet",
  "model_name": "Claude 4.6 Sonnet",
  "model_provider": "anthropic",
  "model_provider_name": "Anthropic",
  "accessible": true,
  "deprecate": false,
  "retired": false,
  "recommended": false,
  "is_default_profile": false,
  "internet_access": false,
  "personalizations": true,
  "model_input_limit": 1000000
}
```

The list contains BOTH base profiles (one per available model, mostly with
empty `id` — system entries not user-selectable) AND user-created Custom
Assistants. Filter by `id != ""` for what's usable as `profile_id`.

`kagi models` and `kagi profiles` are the CLI surface over this; both
deduplicate and skip `deprecate`/`retired`/`!accessible` entries.

## Sign-in flow (captured 2026-04-28)

Two-step: GET the form to capture CSRF + paired session cookie, then POST.

### GET /signin

Returns the HTML sign-in page. The form contains a hidden `_csrf` token, plus
the server sets cookies (anti-CSRF + a temp session) that **must** be replayed
on the POST. Without those cookies, POST /login returns 403 even with a valid
CSRF value.

```html
<form action="https://kagi.com/login" method="post" enctype="application/x-www-form-urlencoded">
  <input type="hidden" name="_csrf" value="fJxHGYaotOG2P3-_OnyR_XlI_djnRUAzy1BwiNvc6CR06ouHelC4LeCTNkbelpD0X7mVNiiSm2ab67Bius63BA==">
  <input type="hidden" name="r" value="/assistant">
  <input type="text" name="email" autocomplete="username" required>
  <input type="password" name="password" autocomplete="current-password">
  <input type="checkbox">  <!-- "Remember me", unnamed -->
  <button type="submit">Sign In</button>
</form>
```

### POST /login

```http
POST /login HTTP/1.1
Host: kagi.com
Content-Type: application/x-www-form-urlencoded
Origin: https://kagi.com
Referer: https://kagi.com/signin
Cookie: <whatever GET /signin set>

_csrf=fJxHGYaotOG2P3-_...&r=%2Fassistant&email=user%40example.com&password=...
```

Form-encoded only. Note the form action is **`/login`**, not `/signin`. The
`r` field is the post-login redirect target (any sane path; `/` works).

Successful response:

```http
HTTP/1.1 302 Found
Location: /assistant
Set-Cookie: kagi_session=ISAqB7Rs...; Domain=.kagi.com; Path=/; HttpOnly; Secure; SameSite=Lax
```

Failure modes:

- 200 OK with the sign-in form re-rendered (wrong credentials).
- 302 Found with `Location: /signin?...` (also rejection).
- 403 Forbidden (missing the GET-sourced cookies, or CSRF token mismatch).

The Go client uses a `cookiejar.Jar` so the GET → POST chain replays the
cookies automatically; redirects are disabled (`http.ErrUseLastResponse`)
so we can read the `Set-Cookie` from the 302 directly.

## 404-as-auth-fail quirk

For unauthenticated `POST /assistant/prompt`, Kagi returns **404 Not Found**
(not 401/403). The Go client treats 404, 401, 403, and 3xx redirects to
/signin or /signup all as auth failure and triggers auto-relogin once.

This means a real 404 (e.g. malformed thread id) will also trigger one
relogin attempt, but the retry will see the same 404 and surface it as the
final error — no infinite loop.

## Thread list — `POST /assistant/thread_list`

Sidebar pagination. Streamed in the same NUL-delimited Kagi protocol as
`/assistant/prompt`, but only emits three event types: `hi`, `tags.json`, and
`thread_list.html`.

Request:

```json
{
  "cursor": {
    "ack": "2025-03-30T13:07:09Z",
    "created_at": "2025-03-30T13:04:24Z",
    "id": "0069d9df-fa0d-43c7-bcce-92faf4cc367b"
  },
  "limit": 100
}
```

Pass `cursor: null` for page 1; pass the previous response's `next_cursor`
verbatim for subsequent pages. The endpoint refuses to advance without a
cursor — there is no pure offset/limit form.

`thread_list.html` payload:

```json
{
  "html": "<div class=\"hide-if-no-threads\" data-group-name=\"Today\">...</div>...",
  "next_cursor": {"ack": "...", "created_at": "...", "id": "..."},
  "has_more": true,
  "count": 100,
  "total_counts": null
}
```

The `html` field contains server-rendered `<li class="thread">` entries
grouped by date label (`Today`, `Previous 7 days`, `All time`). Each `<li>`
exposes its metadata as data attributes:

```html
<li class="thread"
    data-code="<thread-uuid>"
    data-saved="true"
    data-public="false"
    data-tags="[]"
    data-snippet="first ~80 chars of the prompt"
    >
  <a href="/assistant/<thread-uuid>">
    <div class="title">LLM-generated title</div>
    <div class="excerpt">echo of the snippet</div>
  </a>
</li>
```

`client.parseThreadHTML` extracts these via regex (HTML structure is stable;
no JSON island for this endpoint).

## Thread detail — `GET /assistant/<thread-uuid>`

The full thread page. The conversation isn't streamed — it's embedded as two
JSON islands the JS bundle hydrates into `<div id="chat_box">` on load:

```html
<div id="json-thread" hidden>{...}</div>
<div id="json-message-list" hidden>[...]</div>
```

`json-thread` (single object):

```json
{
  "id": "8869e6c6-...",
  "title": "Greeting",
  "ack": "2026-05-02T08:37:47Z",
  "created_at": "2026-04-30T18:10:35Z",
  "saved": true,
  "shared": false,
  "branch_id": "00000000-0000-4000-0000-000000000000",
  "tag_ids": [],
  "profile": { /* full AssistantProfile snapshot */ }
}
```

`json-message-list` (array, oldest first). Each entry represents one **turn**
(user prompt + assistant reply paired into a single record):

```json
{
  "id": "a3fb461b-...",
  "thread_id": "8869e6c6-...",
  "created_at": "2026-04-30T18:10:35Z",
  "branch_list": ["00000000-0000-4000-0000-000000000000"],
  "state": "done",
  "prompt": "user input (markdown)",
  "reply": "<p>assistant output (HTML)</p>",
  "md": "assistant output (markdown)",
  "profile": { /* AssistantProfile used for this turn */ },
  "metadata": "<li>...billing HTML...</li>",
  "documents": []
}
```

The **last entry's `id`** is the value to pass as `focus.message_id` when
continuing the conversation — this is what made the previous "--parent
required with -t" CLI restriction obsolete.

The `<div id="chat_box">` itself is empty in the server response; the
browser populates it from `json-message-list` after page load. Don't bother
parsing the chat-bubble templates.

## Thread modify — `POST /assistant/thread_modify`

Bulk rename / save / share / re-tag. Streamed protocol, only emits `hi`.

```json
{
  "threads": [
    {
      "id": "8869e6c6-...",
      "title": "New Title",
      "saved": true,
      "shared": false,
      "tag_ids": []
    }
  ]
}
```

Send the **complete current state** for each thread, not a diff — the server
overwrites every listed field. If `title` is empty the server keeps the
existing title; everything else is required.

## Thread delete — `POST /assistant/thread_delete`

Same shape as `thread_modify`:

```json
{
  "threads": [
    {"id": "<uuid>", "title": "...", "saved": true, "shared": false, "tag_ids": []}
  ]
}
```

A bare `[{"id":"..."}]` envelope works for some thread states but the JS
client always sends the full snapshot — so do we (`client.DeleteThreads`
fetches each thread first to fill in the metadata).

## Thread search — `POST /assistant/search`

Full-text search across the user's threads. Returns a **plain JSON array**
(not the streamed Kagi protocol), one entry per matching message:

```json
[
  {
    "rank": 0.1,
    "snippet": "...HTML with <b>matches</b>...",
    "message_id": "7a11973a-...",
    "branch_id": "00000000-0000-4000-0000-000000000000",
    "thread_id": "8962a19b-..."
  }
]
```

Request:

```json
{"q": "search terms", "tag_id": null, "saved": null, "shared": null}
```

All filter fields are optional — omitted = no filter. `q` is matched against
message bodies (not just titles), and the snippet shows the actual hit
context.

## Custom Assistant CRUD

Driven from `/settings/custom_assistant` (the per-assistant edit page) and
`/settings/assistant` (the listing page; reuses the embedded
`json-profile-list` from `/assistant`). Both submission endpoints take
`application/x-www-form-urlencoded` with **no CSRF token** — cookie auth is
the only requirement.

### `POST /settings/ast/profiles/update` — create or update

Empty `profile_id` → create. Populated → update. Form fields:

| Field                 | Required | Notes                                                  |
| --------------------- | -------- | ------------------------------------------------------ |
| `profile_id`          |          | Empty for create, UUID for update.                     |
| `name`                | ✓        | Display name (must be unique within an account).        |
| `base_model`          | ✓        | Model id (see `kagi models`).                           |
| `custom_instructions` |          | System prompt. Send empty string to clear.              |
| `bang_trigger`        |          | Optional bang command (e.g. `code` → `!code prompt`).   |
| `internet_access`     |          | `on` (enabled) or `false` (disabled).                   |
| `personalizations`    |          | `on` or `false`.                                        |
| `selected_lens`       |          | Lens id, or `0` for none.                               |

The form on the live page sends the checkbox both as a checkbox (value `on`,
present only when checked) **and** a hidden fallback (value `false`). We
collapse this into a single field that's always present with `on`/`false`.

Success: 302 redirect back to `/settings/assistant`. No body, no echo of the
new id — the client has to look the new profile up by name in the
post-create profile list.

Failure (validation error): 200 with the form re-rendered in HTML. The Go
client surfaces this as an error.

### `POST /settings/ast/profiles/delete`

```
profile_id=<uuid>
```

302 to `/settings/assistant` on success. Cannot delete the default profile —
server returns 302 *back* to `/settings/assistant` with an error flash, which
is HTTP-indistinguishable from the success redirect. The Go client brackets
the POST with two `FetchProfiles` calls to verify the id existed beforehand
and is gone afterward, surfacing both "no such id" and "delete rejected" as
errors instead of silently succeeding.

### `GET /settings/custom_assistant?id=<uuid>` — edit page (read side)

The form's *current* values for an existing assistant. Used by the Go client
to support partial updates: fetch → mutate the fields the caller cares
about → POST the whole form back to `/settings/ast/profiles/update`. The
fields available on the page mirror the create form exactly:

- `<input name="name" value="...">`
- `<input name="bang_trigger" value="...">`
- `<textarea name="custom_instructions">...</textarea>` (HTML-entity encoded)
- `<input name="internet_access" type="checkbox" value=on [checked]>`
- `<input name="personalizations" type="checkbox" value=on [checked]>`
- `<input name="base_model" type="radio" value="..." [checked]>` (one per available model)
- `<input name="selected_lens" type="radio" value="..." [checked]>` (`value="0"` = no lens)

There's no JSON island for this page — it's a plain HTML form. The Go
client (`parseCustomAssistantForm`) extracts each field by regex.

## File upload — `POST /assistant/prompt` (multipart variant)

Files attach to a regular prompt request via `multipart/form-data` instead
of the JSON-encoded form. The browser builds the body as:

```
Content-Type: multipart/form-data; boundary=...

--boundary
Content-Disposition: form-data; name="state"
Content-Type: application/json

{"focus":{...},"profile":{...},"threads":[...]}

--boundary
Content-Disposition: form-data; name="file"; filename="screenshot.png"
Content-Type: image/png

<binary>

--boundary
Content-Disposition: form-data; name="__kagithumbnail"; filename="screenshot.png"
Content-Type: image/jpeg

<binary>  // 84x84 jpeg, only for images
--boundary--
```

Notes:

- The `state` field carries the same JSON envelope as the unilaterally
  JSON-encoded request. No new fields — `documents: []` in
  `new_message.json` will be populated server-side once attachments are
  processed.
- Each file is appended as `name="file"` (one entry per file).
- For images, the browser also generates an 84×84 thumbnail (downscaled to
  60% JPEG quality) and appends it as `name="__kagithumbnail"`. The server
  uses this for the upload preview UI. Non-image uploads omit the thumb.
- Don't set `Content-Type` manually; let the HTTP client set the multipart
  boundary.
- `Accept: application/vnd.kagi.stream` and the rest of the streaming
  response protocol are unchanged.

Not yet wired into the Go client (see `docs/todo.md`).

## Other action endpoints (not exposed in the CLI)

Captured from the JS bundle, listed for completeness:

- `POST /assistant/stop/{trace_id}` — abort an in-flight streaming response.
  The `trace_id` is the value emitted on the first `new_message.json` of a
  prompt response.
- `POST /assistant/message_regenerate` — re-roll an assistant turn (creates a
  new branch under the same parent).
- `POST /assistant/message_edit` — rewrite a user message and re-run the
  thread from that point.
- `POST /assistant/thread_open` — likely warms a thread; not investigated.
- `POST /assistant/tags/{create,modify,delete}` — tag CRUD. Bodies:
  `tags/create: [{name, color, icon_ref}]`, `tags/modify: [{id, name, color,
  icon_ref}]`, `tags/delete: [<tag-id>]`.

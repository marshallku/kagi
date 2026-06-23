# Kagi Assistant — reverse-engineering notes (v2)

Captured 2026-06-23 by driving a logged-in browser via `tabd` and inspecting
network traffic for the **new** `assistant.kagi.com` app.

> **Breaking change (2026-06).** Kagi moved the Assistant off `kagi.com/assistant`
> onto a dedicated **`assistant.kagi.com`** SvelteKit SPA backed by a JSON REST
> API under `/api/*`. The old NUL-delimited `application/vnd.kagi.stream`
> protocol on `kagi.com/assistant/*` is **decommissioned**:
>
> - `kagi.com/assistant` → 301/302 to `assistant.kagi.com/`.
> - `POST kagi.com/assistant/prompt` → HTTP 500 (dead).
> - The `<div id="json-profile-list">` discovery island is gone.
>
> The current Go client targets the old protocol and is therefore **broken for
> chat**. Only the auth layer survives unchanged. The v1 protocol notes are
> archived at the bottom of this file under "Legacy (decommissioned)".

## TL;DR — what changed

| Concern            | v1 (`kagi.com/assistant/*`)                              | v2 (`assistant.kagi.com/api/*`)                                  |
| ------------------ | ------------------------------------------------------- | ---------------------------------------------------------------- |
| Auth               | `kagi_session` cookie                                   | **same** `kagi_session` cookie (domain `.kagi.com`, shared)      |
| Send a prompt      | one `POST /assistant/prompt`                            | three calls: create conv → post message → GET SSE stream         |
| Stream format      | custom NUL-delimited `vnd.kagi.stream`                  | **standard SSE** (`text/event-stream`, `data: …` / `data: [DONE]`) |
| Thread             | `thread_id` + `message_id` + zero `branch_id`           | `conversation_uuid` + `branch_uuid` + `head_message_uuid`        |
| Discovery          | scrape `<div id="json-profile-list">` HTML              | `GET /api/init` (one JSON blob)                                  |
| Profiles           | `profiles[]` (`id`, `model`, `custom_instructions`)     | `custom_assistants[]` (`uuid`, `llm_id`, `instructions`)         |
| Thread list        | `POST /assistant/thread_list` (cursor, HTML)            | `GET /api/init` → `conversations.items[]` (JSON)                 |
| Thread detail      | `GET /assistant/{id}` (HTML islands)                    | `GET /api/conversations/{uuid}/init` (JSON)                      |
| Modify / delete    | `POST /assistant/thread_{modify,delete}` (full snapshot) | `PATCH` / `DELETE /api/conversations/{uuid}` (REST)             |
| Search             | `POST /assistant/search`                                | `GET /api/search?q=…`                                            |
| Assistant CRUD     | `POST /settings/ast/profiles/{update,delete}` (form)    | `/api/assistants`, `/api/assistants/{uuid}` (JSON REST)          |
| Folders            | —                                                       | `/api/folders*` (new)                                            |
| Upload             | multipart on `/assistant/prompt`                        | `/api/upload*` (dedicated, new)                                  |

## Authentication (unchanged)

Cookie-based; the auth cookie is shared across `kagi.com` and
`assistant.kagi.com` because it is set on the parent domain.

| Cookie         | Domain      | Notes                                                      |
| -------------- | ----------- | ---------------------------------------------------------- |
| `kagi_session` | `.kagi.com` | HttpOnly, Secure, SameSite=Lax. Sent to both subdomains.   |
| `_kagi_search_`| `kagi.com`  | Search session — not needed for the assistant API.         |

The `/api/*` endpoints take the cookie only — **no CSRF token** on JSON calls
(verified: create/delete conversations succeed with cookie auth alone).
Unauthenticated requests to `/api/init` return **401** (clean, unlike the old
404-as-auth-fail quirk).

### Sign-in flow

Unchanged from v1, except the "Login with Kagi" button on
`assistant.kagi.com/` redirects to:

```
https://kagi.com/signin?r=https%3A%2F%2Fassistant.kagi.com%2F
```

The sign-in form is identical (`_csrf`, `r`, `email`, `password` fields;
form action `POST /login`; 302 `Set-Cookie: kagi_session=…` on success). After
login, `kagi.com` redirects back to `assistant.kagi.com/` and the shared
cookie authenticates the SPA. See "Sign-in flow" under Legacy below — the
mechanics are the same; only the `r` redirect target changed.

## Error envelope

All `/api/*` errors share a typed envelope:

```json
{
  "error": {
    "code": "validation_error",
    "message": "Field required",
    "request_id": "8287e0a4-…",
    "fields": [{"loc": ["query", "q"], "message": "Field required", "type": "missing"}]
  }
}
```

Observed codes: `validation_error` (422), `method_not_allowed` (405),
plus standard 401/404. `fields[].loc` follows FastAPI/Pydantic-style
`[location, name]` tuples — handy for discovering required params.

## Bootstrap — `GET /api/init`

One call returns the entire app state. Top-level keys:

```
conversations          {items: [...], ...}   — thread list (see below)
counts                                          — sidebar counters
folders                [...]                    — folder tree (new)
folder_customization
models                 {models, assistants, default, sections}
lenses
language               "en"
legacy_import                                    — migration status from v1
settings               {...}                     — user settings (see below)
custom_assistants      [...]                     — custom assistants (was profiles)
active_conversation    null | {...}
active_error
billing
has_unlimited_access   bool
```

### `conversations.items[]` (was the thread list)

```json
{
  "uuid": "77ecad85-…",
  "title": "보안 점검용 프롬프트 개선 요청",
  "is_saved": true,
  "is_shared": false,
  "is_pinned": false,
  "model_name": "deepseek-v4-pro",
  "icon": null,
  "folder_uuid": null,
  "created_at": "2026-06-23T02:59:09.812679",   // naive UTC, no 'Z'
  "updated_at": "2026-06-23T03:01:10.831451",
  "deleted_at": null,
  "total_tokens": 2518,
  "total_cost": 0.021,
  "pinned_count": 0,
  "message_count": 6,
  "annotation_count": 0,
  "has_branches": true,
  "branch_count": 1,
  "is_owner": true
}
```

Timestamps are naive ISO-8601 (no timezone suffix) but are UTC.

### `custom_assistants[]` (was profiles)

```json
{
  "uuid": "46896181-f355-4e9d-a6b4-cbd10f8d7455",
  "name": "Claude",
  "llm_id": "claude-4-sonnet-thinking",         // was "model"
  "instructions": "You are an autoregressive…",  // was "custom_instructions"
  "bang_trigger": null,
  "internet_access": true,
  "personalizations": true,
  "lens_id": null,
  "deprecated": false,
  "retired": false,
  "successor_model_name": null,
  "created_at": "…",
  "updated_at": "…"
}
```

### `models` = `{models, assistants, default, sections}`

`models.models[]` — each entry is far richer than v1's flat list:

```json
{
  "id": "ki_quick",
  "provider": "kagi",
  "provider_label": "K",
  "display_name": "Quick",
  "context_window": 128000,
  "supports_thinking": false,
  "thinking_presets": null,
  "default_thinking_preset": null,
  "capabilities": ["fast", "vision"],
  "supported": true,
  "recommended": false,
  "scorecard": {"cost": 1, "speed": 5, "accuracy": 2, "privacy": 5, "release_date": "2026-05-22"},
  "internet_access": true,
  "requires_search": true,
  "access_level": "standard",
  "deprecated": false,
  "retired": false,
  "successor_model_id": null
}
```

`models.sections` groups model ids for the picker UI; `models.default` is the
default model id.

### `settings`

```json
{
  "custom_instructions": "I'm a web developer based in South Korea…",
  "dark_theme": null,
  "light_theme": null,
  "font_size": null,
  "submit_key": null,
  "keyboard_shortcuts": null,
  "default_builtin_profile_key": null,
  "default_custom_profile_uuid": null,
  "default_llm_id": null,
  "default_selection_kind": "last_used",
  "thread_retention_policy": "forever",   // "forever" | (default 24h auto-delete)
  "upsell_dismissed": false,
  "workspace_area": null
}
```

> **Retention note.** New conversations are created with `is_saved: true`, but
> Kagi auto-deletes unsaved threads after 24h unless the account's
> `thread_retention_policy` is `forever`. The CLI should delete its own
> throwaway test conversations rather than rely on auto-expiry.

## Chat — three-step flow

### 1. Create a conversation — `POST /api/conversations`

```json
// request
{"model_name": "ki_quick"}
```

```json
// response
{
  "conversation": {"uuid": "c97a4478-…", "title": "New chat", "model_name": "ki_quick", "message_count": 0, …},
  "default_branch": {
    "uuid": "12b540e3-…",                       // ← branch_uuid for step 2
    "conversation_uuid": "c97a4478-…",
    "head_message_uuid": null,
    "branch_point_parent_uuid": null,
    "branch_child_message_uuid": null,
    "is_default": true,
    "message_count": 0
  }
}
```

A conversation owns one or more **branches**; you post messages to a branch,
not to the conversation directly.

### 2. Post the user message — `POST /api/branches/{branch_uuid}/messages`

```json
// request
{
  "message": "What is 2+2? Answer in one word.",
  "thinking_preset": null,         // model-dependent; null when supports_thinking=false
  "model_name": "ki_quick",
  "enable_search": true,           // was profile.internet_access
  "personalization": true          // was profile.personalizations
}
```

```json
// response
{
  "branch": {"uuid": "1661ed71-…", "head_message_uuid": "13a37bb1-…", "message_count": 1, …},
  "conversation": {…},
  "user_message": {"uuid": "13a37bb1-…", "role": "user", "content": "What is 2+2?…", …},
  "stream_url":        "/api/branches/1661ed71-…/stream",
  "stream_status_url": "/api/branches/1661ed71-…/stream/status",
  "stream_cancel_url": "/api/branches/1661ed71-…/stream/cancel"
}
```

Use the returned `stream_url` (or build `…/stream?cursor=0-0`). Note the
branch uuid in the *response* can differ from the one you posted to when the
server forks a branch — always follow `stream_url`.

### 3. Read the reply — `GET /api/branches/{branch_uuid}/stream?cursor=0-0`

Standard **Server-Sent Events** (`text/event-stream; charset=utf-8`). Each
event is `id:` + `data:` (JSON), separated by a blank line; the stream ends
with a literal `data: [DONE]`.

```
id: 1782183846169-0
data: {"text":"","conversation_uuid":"b1b1…","branch_uuid":"1661…","is_final":false,"conversation_title":"Mathematical Addition Result"}

id: 1782183846172-0
data: {"text":"","conversation_uuid":"b1b1…","branch_uuid":"1661…","is_final":false,"model_name":"ki_quick","html_content":"<p>Four</p>"}

id: 1782183846424-0
data: {"text":"Four","is_final":true,"model_name":"ki_quick","model_version":"ki_quick-2026-05-22",
       "timing":{"prep_ms":0,"ttft_ms":290,"llm_streaming_ms":213,"finalize_ms":37,"backend_total_ms":542},
       "usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4,"cost_usd":0.000012,"tokens_per_second":3.0},
       "context_usage":{"context_window":128000,"total_used":4,"available":127996,"used_percent":0},
       "duration_ms":300,"assistant_message_uuid":"8f4995a3-…",
       "billing":{"limit":"none","can_proceed":true,"usage":{…},"ack":{"soft":false,"hard":false}}}

data: [DONE]
```

Field notes:

- **`text`** is the **cumulative** plain-text reply (not a delta). The final
  event carries the complete text.
- **`html_content`** is the cumulative rendered HTML.
- **`conversation_title`** appears on (usually the first) non-final event — the
  LLM-generated title. Persist the latest seen value.
- **`is_final: true`** marks the terminal payload: full `usage`, `timing`,
  `context_usage`, `billing`, and `assistant_message_uuid`.
- `cursor=0-0` requests from the start; the `id` values (`<ms>-<seq>`) are
  resumable cursors for reconnects.

### Follow-up turns

Post to the same conversation's branch again with the next
`POST /api/branches/{branch_uuid}/messages`; the branch's `head_message_uuid`
threads the reply onto the prior turn. Resolve the head via
`GET /api/conversations/{uuid}/init` → `active_branch.head_message_uuid`.

### Abort / status

- `POST /api/branches/{branch_uuid}/stream/cancel` — stop an in-flight stream.
- `GET  /api/branches/{branch_uuid}/stream/status` — poll stream state.

## Conversation detail — `GET /api/conversations/{uuid}/init`

```
{
  "conversation":   {…same shape as conversations.items[]…},
  "branches":       [ {uuid, conversation_uuid, branch_name, message_count,
                       head_message_uuid, branch_point_parent_uuid,
                       branch_child_message_uuid, is_default, updated_at}, … ],
  "active_branch":  { …one branch object… },
  "messages":       { "items": [ … ], "has_more": false },
  "context_usage":  {…}
}
```

`messages.items[]` — one entry per message (user and assistant are separate
rows, unlike v1's paired turns):

```json
{
  "uuid": "…",
  "role": "user" | "assistant",
  "content": "markdown text",
  "html_content": "<p>…</p>",
  "thinking": null, "thinking_html": null,
  "parent_message_uuid": "…",
  "branch_name": null, "branch_links": null,
  "sibling_count": 1, "sibling_index": 0, "sibling_uuids": [], "sibling_branch_uuids": [],
  "model_name": "…", "model_display_name": "…", "model_provider": "…", "model_version": "…",
  "input_tokens": …, "output_tokens": …, "cost_usd": …, "tokens_per_second": …, "duration_ms": …,
  "references": [], "attachments": [],
  "is_pinned": false, "annotation": null,
  "profile_uuid": null, "profile_revision_id": null, "profile_name": null,
  "created_at": "…"
}
```

## Conversation management (REST)

| Verb + path                                   | Purpose                                            |
| --------------------------------------------- | -------------------------------------------------- |
| `PATCH  /api/conversations/{uuid}`            | Rename / save / share / pin / move to folder.      |
| `DELETE /api/conversations/{uuid}`            | Soft-delete (→ trash). Verified: 200, no CSRF.     |
| `POST   /api/conversations/{uuid}/restore`    | Undo a soft-delete.                                |
| `DELETE /api/conversations/{uuid}/permanent`  | Hard-delete.                                       |
| `POST   /api/conversations/deleted/purge`     | Empty trash.                                       |
| `GET    /api/conversations/{uuid}/branches`   | List branches.                                     |
| `GET    /api/conversations/stats/counts`      | Sidebar counters.                                  |
| `GET    /api/conversations/by-legacy/{oldId}` | **Map a v1 thread id → v2 conversation** (migration). |
| `POST   /api/conversations/import`            | Import conversations.                              |

PATCH body shape is the typed subset of the conversation object (e.g.
`{"title": "…"}`, `{"is_saved": true}`, `{"folder_uuid": "…"}`) — send only
the fields you change (REST partial update, not the v1 full-snapshot rule).
*Exact accepted fields not yet exhaustively captured — confirm before relying.*

## Search — `GET /api/search?q=…&limit=N`

```json
// GET /api/search?q=신생아&limit=2  →
{
  "items": [
    {"conversation": {…full conversation object…}, "snippet": null, "rank": 0.182}
  ],
  "truncated": false
}
```

`q` is **required** (422 if missing; `POST` → 405 — it is GET-only). Returns
matching conversations with a relevance `rank`; `snippet` may be null.

## Custom assistants — `/api/assistants`

| Verb + path                  | Purpose                          |
| ---------------------------- | -------------------------------- |
| `GET    /api/assistants`     | List (also embedded in `/api/init` as `custom_assistants`). |
| `POST   /api/assistants`     | Create.                          |
| `GET    /api/assistants/{uuid}` | Read one.                     |
| `PATCH  /api/assistants/{uuid}` | Update (partial).             |
| `DELETE /api/assistants/{uuid}` | Delete.                       |

Body fields mirror the `custom_assistants[]` read shape: `name`, `llm_id`,
`instructions`, `bang_trigger`, `internet_access`, `personalizations`,
`lens_id`. JSON, not form-encoded. *Create/update request bodies inferred from
the read shape — capture a live create to confirm exact field names before
implementing writes.*

## Folders (new) — `/api/folders`

| Verb + path                | Purpose                |
| -------------------------- | ---------------------- |
| `GET    /api/folders`      | List (also in `/api/init`). |
| `POST   /api/folders`      | Create.                |
| `PATCH  /api/folders/{uuid}` | Rename / recolor.    |
| `DELETE /api/folders/{uuid}` | Delete.              |
| `POST   /api/folders/reorder` | Reorder.            |

Conversations reference a folder via `conversation.folder_uuid`.

## File upload (new) — `/api/upload`

Dedicated upload endpoints, separate from the prompt request (v1 rode files
inline on `/assistant/prompt` as multipart):

- `POST   /api/upload` — upload a file, returns a file id.
- `GET    /api/upload/{id}` — fetch.
- `POST   /api/upload/highlight/{id}` — (purpose unconfirmed).

Attachments then surface on messages via `message.attachments[]`. Exact
request shape not yet captured.

## Sharing — `/api/branches/{branch}/share`, `/api/shares/{id}`

- `POST   /api/branches/{branch_uuid}/share` — create a public share link.
- `GET    /api/shares/{id}` — fetch a shared conversation.

## Settings — `/api/settings`

- `GET  /api/settings` — read (also in `/api/init.settings`).
- `PATCH/POST /api/settings` — update (e.g. `custom_instructions`,
  `thread_retention_policy`, default model/profile).

## Legacy import — `/api/legacy-import/{status,retry}`

Kagi migrates v1 threads to v2 conversations server-side. `/api/init` carries a
`legacy_import` block with progress; `/api/legacy-import/status` polls it and
`/api/legacy-import/retry` re-runs a failed import. `GET
/api/conversations/by-legacy/{oldThreadId}` resolves an individual v1 thread id
to its v2 conversation.

## Billing — `/api/billing/{status,ack}`

- `GET  /api/billing/status` — usage / limits (also embedded in the final
  stream event's `billing` block and `/api/init.billing`).
- `POST /api/billing/ack` — acknowledge a soft/hard limit warning.

## Full endpoint inventory (from the SvelteKit bundle)

```
GET    /api/init
GET    /api/settings                         POST /api/settings
GET    /api/search?q=…

POST   /api/conversations
GET    /api/conversations/{uuid}             PATCH/DELETE /api/conversations/{uuid}
GET    /api/conversations/{uuid}/init
GET    /api/conversations/{uuid}/branches
POST   /api/conversations/{uuid}/restore
DELETE /api/conversations/{uuid}/permanent
GET    /api/conversations/by-legacy/{oldId}
POST   /api/conversations/import
GET    /api/conversations/stats/counts
POST   /api/conversations/deleted/purge

POST   /api/branches/{branch}/messages
GET    /api/branches/{branch}/stream         (SSE)
POST   /api/branches/{branch}/stream/cancel
GET    /api/branches/{branch}/stream/status
POST   /api/branches/{branch}/share

GET    /api/messages/{uuid}                  POST /api/messages/{uuid}/edit-response

GET    /api/assistants                       POST /api/assistants
GET    /api/assistants/{uuid}                PATCH/DELETE /api/assistants/{uuid}

GET    /api/folders                          POST /api/folders          POST /api/folders/reorder
PATCH/DELETE /api/folders/{uuid}

POST   /api/upload                           GET /api/upload/{id}        POST /api/upload/highlight/{id}
GET    /api/shares/{id}
GET    /api/billing/status                   POST /api/billing/ack
GET    /api/legacy-import/status             POST /api/legacy-import/retry
GET    /api/_debug
```

Routes containing `{x}` are path-parameterised; verbs marked above are observed
or inferred from the SPA. Write-body shapes flagged "inferred/unconfirmed"
should be captured live before implementation.

---

# Legacy (decommissioned) — v1 protocol `kagi.com/assistant/*`

> Retained for historical reference and for the migration mapping. These
> endpoints no longer function for chat (`POST /assistant/prompt` → 500) and
> the `/assistant` UI redirects to `assistant.kagi.com`. The **auth / sign-in
> flow below is still accurate** and shared with v2.

## Sign-in flow (captured 2026-04-28, still valid)

Two-step: GET the form to capture CSRF + paired session cookie, then POST.

### GET /signin

Returns the HTML sign-in page with a hidden `_csrf` token; the server sets
anti-CSRF + temp-session cookies that **must** be replayed on the POST.

```html
<form action="https://kagi.com/login" method="post">
  <input type="hidden" name="_csrf" value="…">
  <input type="hidden" name="r" value="https://assistant.kagi.com/">  <!-- v2 redirect target -->
  <input type="text" name="email">
  <input type="password" name="password">
</form>
```

### POST /login

Form-encoded (`_csrf`, `r`, `email`, `password`); form action is **`/login`**.
Success → `302 Found` with `Set-Cookie: kagi_session=…; Domain=.kagi.com`.
Failure → 200 (form re-rendered), 302 to `/signin?…`, or 403 (missing cookies).

The Go client uses a `cookiejar.Jar` so the GET → POST cookie chain replays
automatically; redirects are disabled (`http.ErrUseLastResponse`) to read the
`Set-Cookie` off the 302.

## v1 prompt protocol (dead)

`POST /assistant/prompt`, single call, custom NUL-delimited
`application/vnd.kagi.stream` (`<type>:<payload>\0`), event types `hi`,
`thread.json`, `tokens.json`, `new_message.json`, etc. Request envelope
`{focus:{thread_id,branch_id,prompt,message_id}, profile:{id,model,
internet_access,personalizations,lens_id}, threads:[…]}`. Superseded by the
three-step v2 flow above. See git history of this file for the full v1 detail.

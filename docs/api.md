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
| Send a prompt      | one `POST /assistant/prompt`                            | dispatch a turn (`/api/v2/…`) → GET SSE turn stream              |
| Stream format      | custom NUL-delimited `vnd.kagi.stream`                  | **standard SSE** of turn events (`node.delta`, …)                |
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

### Sign-in flow (Zitadel OIDC, captured 2026-10-02)

Kagi moved sign-in to a Zitadel OIDC login hosted at `account.kagi.com` (a
SvelteKit app). The old `_csrf` form at `kagi.com/signin` + `POST /login` is
gone; `GET /signin` now 302s into the OIDC chain. Implemented in
`client/auth.go`.

1. `GET https://kagi.com/signin` → 302 `/oauth2?service=zitadel&` → 302
   `https://account.kagi.com/oauth/v2/authorize?…` → 302
   `/loginname?requestId=oidc_V2_…` (200, email form). **The authorize
   `Location` carries raw spaces in `scope`** — percent-encode them before
   following or the server answers 400.
2. `POST /loginname?/email&requestId=…` with `requestId`, `loginName`.
   With `Accept: application/json` the SvelteKit action answers 200
   `{"type":"redirect","status":303,"location":"/loginname?sessionId=…&requestId=…"}`.
3. `GET` that location → password form (hidden `sessionId`, `requestId`,
   `rememberMeShown`).
4. `POST /loginname?/password&requestId=…` with the hidden inputs plus
   `loginName`, `password`, `rememberMe=on` → `{"type":"redirect",
   "location":"https://kagi.com/oauth2/callback?code=…&state=…"}`. A wrong
   password comes back as `{"type":"failure",…}`.
5. `GET` the callback → 302 `/` with `Set-Cookie: kagi_session=…;
   Domain=.kagi.com`, which authenticates `assistant.kagi.com`.

The cookie jar must carry `__Host-zitadel.useragent`, `sessions`, etc. across
hops. No captcha or MFA on a password-only account as of capture; an account
with MFA/passkeys would not reach a password form and fails loudly.

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

## Chat — turns API (captured 2026-10-11)

> **Breaking change (2026-10).** The v2-messages flow (`POST /api/conversations`
> → `POST /api/branches/{b}/messages` → `GET /api/branches/{b}/stream`) is gone:
> `POST …/messages` answers **405 Method Not Allowed** and
> `GET …/stream/status` answers 404. Chat moved to an event-sourced **turns**
> API under `/api/v2/*`. `POST /api/conversations` and
> `GET /api/conversations/{uuid}/init` still work, but the client no longer
> needs the former.

### 1a. New conversation + first turn — `POST /api/v2/conversations`

One call creates the conversation and dispatches the first turn.

```json
// request — `content` is required; everything else optional
{
  "content": "Reply with exactly: hello world",
  "model": "claude-5-5-haiku",       // or omit and pass profile_uuid alone
  "profile_uuid": "843f5964-…",      // custom assistant; its own base model is used
  "enable_search": false,
  "personalization": true,
  "lens_id": null,
  "reasoning_effort": null,          // none|minimal|low|medium|high|xhigh|max
  "attachment_uuids": null
}
```

```json
// response
{
  "conversation_uuid": "39e273b1-…",
  "branch_uuid": "79ea169d-…",
  "assistant_turn_uuid": "60625205-…",   // ← stream this; it is also the persisted message uuid
  "user_turn_uuid": "3b6b0da8-…",
  "conversation": {…}
}
```

### 1b. Follow-up turn — `POST /api/v2/branches/{branch_uuid}/respond`

Same body (plus `choice`, `plan_action`, `refused`, `current_location` for
elicitations). Resolve the branch via `GET /api/conversations/{uuid}/init` →
`active_branch.uuid`. Response: `{assistant_turn_uuid, branch_uuid,
conversation_uuid, user_turn_uuid}`. "Smart-dispatched": if the branch's latest
assistant turn is paused on `ask_user`, the content answers it and
`user_turn_uuid` is null. Follow the `branch_uuid` in the response.

`POST /api/v2/branches` forks (`fork_from_turn_uuid`, or `conversation_uuid`
for a root fork) — used for regenerate/edit; not needed by this client.

### 2. Read the reply — `GET /api/v2/turns/{assistant_turn_uuid}/stream[?cursor=…]`

SSE (`text/event-stream`). The web client omits `cursor` on the first read.
There is **no `[DONE]` sentinel**; the connection closes after the
`is_final` frame. Most frames wrap one event of the turn's log:

```
id: 1791687806289-0
data: {"event": {"seq": 1, "turn_id": "6062…", "source": "system", "type": "turn.started",
                 "payload": {"root_node_id": "msg_2a44…", "tier": "quick", "branch_uuid": "79ea…"}, …}}

data: {"event": {"seq": 2, "type": "node.created", "node_id": "msg_2a44…", "payload": {"kind": "assistant_message", "status": "running"}}}
data: {"title": "Hello World Greeting"}
data: {"event": {"seq": 3, "type": "node.created", "node_id": "text_44c9…", "parent_node_id": "msg_2a44…",
                 "payload": {"kind": "text", "format": "markdown", "status": "running", "step": 1}}}
data: {"event": {"seq": 4, "type": "node.delta", "node_id": "text_44c9…", "payload": {"delta": "hello world"}}}
data: {"event": {"seq": 5, "type": "node.completed", "node_id": "text_44c9…", "payload": {"status": "completed", "content": "hello world"}}}
data: {"event": {"seq": 7, "type": "turn.completed", "payload": {"root_node_id": "…", "usage": {"input_tokens": 4813, "output_tokens": 5, "cost_usd": 0.000725, …}}}}
data: {"is_final": true, "billing": {"limit": "none", "can_proceed": true, …}}
```

The turn is a **tree of nodes** under `root_node_id`:

| `payload.kind`      | Meaning                                                                 |
| ------------------- | ----------------------------------------------------------------------- |
| `assistant_message` | the root                                                                |
| `text`              | answer text (markdown). Deltas via `node.delta`; full text on `node.completed` / `node.replaced` `payload.content` |
| `activity`          | `type` = `thinking` (deltas too!), `tool_call` (`tool_name`, `arguments`; result in `node.completed.payload.result`) |

Event types (from the web client's reducer): `turn.started`, `turn.completed`,
`turn.failed` (`payload.error_code`), `turn.cancelled`, `turn.awaiting_input`,
`node.created`, `node.updated`, `node.delta`, `node.replaced`,
`node.completed`, `node.failed`, `node.cancelled`, `reference.added`
(citations: `ref_id`, `url`, `title`, `snippet`; the text cites them inline as
`[^ref_id]`), `source.read`. Nested sub-turns (sub-agents) appear with their
own `turn_id` and `parent_node_id`; the answer is the `text` nodes **directly
under our root**.

- **Deltas, not snapshots** — unlike the old stream, frames are small, but a
  replayed event must not be applied twice.
- **Replay renumbers ids.** Reading a *finished* turn (even with a `cursor`)
  replays the whole log under **fresh SSE ids**, so the cursor is useless for
  dedupe. Dedupe on `(turn_id, event.seq)` as the web client does.
- The stream carries **no HTML**. Read it from the persisted row (below).

### 3. Persisted turn — `GET /api/v2/branches/{branch_uuid}/turns?limit=N`

`{turns: [...], has_more}`, oldest-first within the page. Each row: `uuid`,
`role`, `content` (markdown), `html_content`, `status`
(`running|completed|failed|cancelled|awaiting_input`), `error_code`, `model`,
`model_version`, usage fields, `references`, `parent_message_uuid`, siblings.
Optional `include=events`, `before=<uuid>`. This replaces
`…/stream/status` as the "is it still running?" signal, and is the source of
the final md/HTML. `GET /api/v2/turns/{uuid}` returns one row but with
`html_content: null`.

### Abort

- `POST /api/v2/turns/{uuid}/cancel`
- `DELETE /api/v2/turns/{uuid}[?preserve_attachments=true]`

### Reconnects

The generation outlives the connection and is persisted when it finishes
(observed under the old stream: the edge reset long turns at ~5 min). The
client reconnects with `?cursor=<last id>`, dedupes on `seq`, and when the wire
goes quiet checks the persisted row: not `running` → take the persisted
answer.

## Usage cap — `GET /api/billing/status`

```json
{"limit": "hard", "reason": "cost", "can_proceed": false,
 "limit_status": "assistant_limit_hard_cost",
 "usage": {"interaction_count": 56, "included_cost_usd": 25.96,
           "included_cost_limit_usd": 25.0, "included_cost_soft_limit_usd": 20.0},
 "ack": {"soft": false, "hard": false}}
```

The same `billing` block rides along on the terminal stream frame. This matters
for stream handling: **a turn that trips the cap mid-generation just stops** —
no error frame, no `is_final`, nothing persisted — which looks exactly like a
network failure. Posting a new message once capped fails loudly instead, with
`402 … (billing_limit_reached)`. When a stream ends with no answer, check this
endpoint before blaming the transport.

A frame can also carry `is_final: true` **and** `error` together (observed:
`"Server is shutting down. Please retry your request."`). That is the turn dying
upstream with nothing saved — retrying the *connection* will not help; the
message has to be posted again.

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

POST   /api/branches/{branch}/share
POST   /api/branches/{branch}/messages/{uuid}/report
# removed 2026-10: POST /api/branches/{b}/messages (405), GET …/stream, …/stream/{status,cancel}

POST   /api/v2/conversations                 (create + first turn)
GET    /api/v2/conversations/{uuid}/init     GET /api/v2/conversations/{uuid}/branches
POST   /api/v2/branches                      (fork)
POST   /api/v2/branches/{branch}/respond     (follow-up / elicitation answer)
GET    /api/v2/branches/{branch}/turns       GET /api/v2/branches/{branch}/context
GET    /api/v2/branches/{branch}/artifacts/stream   GET …/artifacts/raw/{path}
GET    /api/v2/turns/{uuid}                  DELETE /api/v2/turns/{uuid}
GET    /api/v2/turns/{uuid}/stream           (SSE)
POST   /api/v2/turns/{uuid}/cancel

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
> the `/assistant` UI redirects to `assistant.kagi.com`. The sign-in flow below
> was replaced by Zitadel OIDC — see "Sign-in flow" above.

## Sign-in flow (captured 2026-04-28, dead since 2026-10)

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
v2 turns flow above. See git history of this file for the full v1 detail.

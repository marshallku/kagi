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

## Endpoints not yet captured

- Profile list / default profile
- Thread list (sidebar load)
- Thread detail / message history fetch
- Thread delete / rename / tag operations
- File upload (multimodal input)
- Branch management (re-rolling responses creates new branch_ids)

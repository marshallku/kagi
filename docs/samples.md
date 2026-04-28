# Captured request/response samples

Real traffic captured 2026-04-28 against `kagi.com/assistant` while logged in.
Use these as ground truth when modifying the client.

## Sample 1 — new thread

**Request:**

```http
POST /assistant/prompt HTTP/1.1
Host: kagi.com
Cookie: kagi_session=ISAqB7RsJ2PqqU-1yo8fNWj1EcY9f0KiEFQyJHeNkho.MDj00qmO6...
Accept: application/vnd.kagi.stream
Content-Type: application/json

{"focus":{"thread_id":null,"branch_id":"00000000-0000-4000-0000-000000000000","prompt":"What is 2+2? Answer in one word."},"profile":{"id":"85988101-51a4-4e88-8a27-36231e98fae2","personalizations":true,"internet_access":true,"model":"grok-4-20","lens_id":null},"threads":[{"tag_ids":[],"saved":true,"shared":false}]}
```

**Response (NUL-delimited, abbreviated):**

```
hi:{"v":"202604250033.stage.99085b7","trace":"85893030b5c130a4f61db226ee26ae46"}\0
thread.html:<li class="thread" data-code="751d32f2-...">...</li>\0
thread.json:{"id":"751d32f2-bef8-43ae-b911-77de0afaed2e","title":"What is 2+2? Answer in one word.","branch_id":"00000000-0000-4000-0000-000000000000",...}\0
messages.json:[]\0
new_message.json:{"id":"a5d5ad16-e537-4190-9218-45895818f55d","thread_id":"751d32f2-...","state":"waiting","prompt":"What is 2+2? Answer in one word.","reply":null,"md":null,...}\0
thread.json:{"id":"751d32f2-...","title":"What Is 2+2?",...}\0
tokens.json:{"text":"","id":"a5d5ad16-...","padding":"2z4ufutS4Hqf"}\0
tokens.json:{"text":"<p>Four</p>","id":"a5d5ad16-...","padding":"gugxNBy8ETcK..."}\0
new_message.json:{"id":"a5d5ad16-...","state":"done","reply":"<p>Four</p>","md":"Four",...,"metadata":"<li><span>Model</span>...<span>0.003 / 0.003</span>..."}\0
```

Note: title was renamed by the server between the two `thread.json` events
("What is 2+2? Answer in one word." → "What Is 2+2?") — the second is the
LLM-generated thread title.

## Sample 2 — follow-up to existing thread

**Request:**

```http
POST /assistant/prompt HTTP/1.1
...

{"focus":{"thread_id":"751d32f2-bef8-43ae-b911-77de0afaed2e","branch_id":"00000000-0000-4000-0000-000000000000","prompt":"And 3+3?","message_id":"a5d5ad16-e537-4190-9218-45895818f55d"},"profile":{"id":"85988101-...","personalizations":true,"internet_access":true,"model":"grok-4-20","lens_id":null}}
```

Differences vs new thread:
- `focus.thread_id` populated.
- `focus.message_id` = id of the previous response we're replying to.
- `threads` field absent.

**Response:** same envelope; `messages.json` now contains the prior message.

## Final `new_message.json` structure

The `state: "done"` event is what the client uses for the completed answer.
Key fields:

| Field          | Type      | Notes                                               |
| -------------- | --------- | --------------------------------------------------- |
| `id`           | UUID      | Message id. Use as `parent_id` for next follow-up. |
| `thread_id`    | UUID      | Thread id. Same across follow-ups.                  |
| `state`        | string    | `"waiting"` initially, `"done"` at completion.      |
| `prompt`       | string    | Echo of the user's input.                           |
| `reply`        | string    | Final HTML (e.g. `<p>Four</p>`).                    |
| `md`           | string    | Final Markdown — what the CLI prints by default.    |
| `profile`      | object    | Full profile snapshot used for this message.        |
| `metadata`     | string    | HTML with billing info: model, tokens, cost, time.  |
| `documents`    | array     | Attached files (empty when no upload).              |
| `trace_id`     | string    | Server trace id (only on `state: "waiting"` event). |

## Models discovered (page extraction)

```
grok-4-20, ki_quick, ki_research, ki_deep_research,
claude-4-sonnet, claude-4-sonnet-thinking, claude-4-7-opus-thinking,
kimi-k2-5, kimi-k2-5-thinking,
glm-4-7-thinking, glm-5-1, glm-5-1-thinking
```

## Cookies (relevant subset)

```
kagi_session   .kagi.com  HttpOnly Secure SameSite=Lax  required
_kagi_search_  kagi.com   HttpOnly Secure SameSite=Lax  search-only, ignored
```

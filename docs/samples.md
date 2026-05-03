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

## Sample 3 — `POST /assistant/thread_list`

Captured 2026-05-02. The browser sends a cursor-paginated request when the
user clicks "Load more" in the sidebar.

**Request:**

```http
POST /assistant/thread_list HTTP/1.1
Cookie: kagi_session=...
Content-Type: application/json
Accept: application/vnd.kagi.stream

{"cursor":{"ack":"2025-03-30T13:07:09Z","created_at":"2025-03-30T13:04:24Z","id":"0069d9df-fa0d-43c7-bcce-92faf4cc367b"},"limit":100}
```

**Response (NUL-delimited, abbreviated):**

```
hi:{"v":"202604301508.stage.86e133f","trace":"eeb06148ddd7031ee07c41ae3640748b"}\0
tags.json:[]\0
thread_list.html:{"html":"<div class=\"hide-if-no-threads\" data-group-name=\"Today\"><ul class=\"thread-list\"><li class=\"thread\" data-code=\"...\" data-saved=\"true\" data-public=\"false\" data-tags=\"[]\" data-snippet=\"...\"><a href=\"/assistant/...\"><div class=\"title\">...</div></a></li>...</ul></div>\n...","next_cursor":{"ack":"2025-02-03T05:41:26Z","created_at":"2025-02-03T02:15:51Z","id":"8d6bcad7-..."},"has_more":true,"count":100,"total_counts":null}\0
```

The first request (page 1) sends `"cursor": null`. Subsequent requests pass
the previous response's `next_cursor` verbatim. `has_more=false` ends
pagination.

## Sample 4 — `GET /assistant/<thread-uuid>` JSON islands

Two hidden divs near the bottom of the thread page carry the structured data
the JS hydrates from. The chat bubbles in `<div id="chat_box"></div>` are
empty in the server response — they're populated client-side from these.

```html
<div id="json-thread" hidden>{&quot;id&quot;:&quot;8869e6c6-...&quot;,&quot;title&quot;:&quot;Greeting&quot;,&quot;ack&quot;:&quot;2026-05-02T08:37:47Z&quot;,&quot;created_at&quot;:&quot;2026-04-30T18:10:35Z&quot;,&quot;saved&quot;:true,&quot;shared&quot;:false,&quot;branch_id&quot;:&quot;00000000-0000-4000-0000-000000000000&quot;,&quot;profile&quot;:{...},&quot;tag_ids&quot;:[]}</div>

<div id="json-message-list" hidden>[{&quot;id&quot;:&quot;a3fb461b-...&quot;,&quot;thread_id&quot;:&quot;8869e6c6-...&quot;,&quot;created_at&quot;:&quot;2026-04-30T18:10:35Z&quot;,&quot;branch_list&quot;:[&quot;0000...&quot;],&quot;state&quot;:&quot;done&quot;,&quot;prompt&quot;:&quot;...user input...&quot;,&quot;reply&quot;:&quot;<p>...HTML...</p>&quot;,&quot;md&quot;:&quot;...markdown...&quot;,&quot;profile&quot;:{...},&quot;metadata&quot;:&quot;<li>...&quot;,&quot;documents&quot;:[]},...]</div>
```

Decode with `html.UnescapeString` then `json.Unmarshal`. The last entry's
`id` is the parent for the next prompt.

## Sample 5 — `POST /assistant/search`

Returns a flat JSON array, not the streamed Kagi protocol. One entry per
matching message (a thread can have multiple hits).

**Request:**

```http
POST /assistant/search HTTP/1.1
Content-Type: application/json

{"q":"Greeting","tag_id":null,"saved":null,"shared":null}
```

**Response:**

```json
[
  {
    "rank": 0.1,
    "snippet": "<b>greeting</b> = hour < 12 ? 'Good morning' : ...",
    "message_id": "7a11973a-47f0-4158-a5db-8952adb39b40",
    "branch_id": "00000000-0000-4000-0000-000000000000",
    "thread_id": "8962a19b-f5be-49e9-8f97-1047a367f118"
  }
]
```

## Sample 6 — Custom Assistant create

**Request (form-encoded):**

```http
POST /settings/ast/profiles/update HTTP/1.1
Cookie: kagi_session=...
Content-Type: application/x-www-form-urlencoded
Referer: https://kagi.com/settings/assistant

profile_id=&name=My+Assistant&base_model=claude-4-sonnet&custom_instructions=You+are+a+helpful+assistant.&bang_trigger=&internet_access=on&personalizations=on&selected_lens=0
```

**Response:** 302 to `/settings/assistant`. No body. The created assistant's
UUID has to be discovered by re-fetching the profile list.

For update, populate `profile_id` with the existing UUID — the server
overwrites every field with whatever's in the form (no merge).

For delete: `POST /settings/ast/profiles/delete` with `profile_id=<uuid>`,
also returns 302 on success.

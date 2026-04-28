# Design decisions

## Why Go

- Matches the existing `~/dev/life-assistant` stack — easy to embed as a plugin
  later if useful.
- stdlib net/http + encoding/json is enough; no external deps.
- Single static binary works for both CLI and HTTP server modes.

## Why cookie auth (not ID/PW automation)

- Kagi supports OAuth (Google/Apple/MS/GitHub), QR, and password sign-in. Most
  of these aren't headless-friendly.
- Re-implementing the sign-in flow would tie us to its CSRF + form layout,
  which can change. The cookie is stable as long as the session lives.
- The user already lives inside a browser; copying the cookie is one click.
- Keeps username/password out of the config — a cookie is a single ephemeral
  secret that can be rotated by simply signing out.

Trade-off: cookie expires (looks like 30+ days based on `expires` field), so
the user has to refresh it occasionally. We accept this for simplicity.

## Why CLI + HTTP server in one binary

- User explicitly requested both.
- Subcommand pattern keeps the surface tiny: `kagi chat` for one-shots,
  `kagi serve` for long-running automation (e.g. life-assistant calling it).
- Same `client` package backs both, so the protocol logic lives in one place.

## Why `--json` and `--stream` are mutually exclusive

Combining them would emit token text first, then trailing JSON — invalid for
`jq`. The Round 3 cross-review caught this and we chose hard-fail over silent
malformed output.

## Why we require explicit `--parent` with `-t`

The Kagi follow-up request always includes `message_id` (the parent in the
thread). We don't yet have an endpoint to look up "the head of thread X", so
asking the caller to pass the message_id they got from the previous response
is the only safe option. Matches what the browser does anyway.

## Why default CLI mode prints Markdown, not streaming

`tokens.json.text` is cumulative HTML (`<p>Four</p>`), not a token delta and
not Markdown. For terminal automation, the clean `md` field from the final
`new_message.json` is far more useful. `--stream` is opt-in for users who
want the streaming feel and don't mind raw HTML output.

## Cross-review fixes applied

Three rounds of `/cross-review` (codex external review) caught the following
CRITICAL issues, all auto-fixed:

| Round | Finding                                                | Fix                                                       |
| ----- | ------------------------------------------------------ | --------------------------------------------------------- |
| 1     | CLI advertised `--parent` defaults but had no lookup   | Made `--parent` required when `-t` is set; help text fixed |
| 1     | `Send` returned success on truncated streams           | Added `done` flag; error if no `state=done` seen          |
| 1     | `/chat/stream` reported startup errors as 200+SSE      | Validate before SSE headers; return 4xx/502 properly      |
| 2     | Server didn't enforce thread_id+message_id invariant   | Added same check in `buildPrompt`                         |
| 3     | `--json` and `--stream` could both be set              | Mutual exclusion check                                    |

INFO findings deliberately not addressed:

- HTTP client/server have no timeout — intentional; LLM streams can be slow
  and we don't want to truncate legitimate long responses.
- Server can be exposed on non-loopback via `-addr` without auth — default is
  `127.0.0.1`; exposing it is the user's choice. Documenting the risk in the
  README is enough.

## Things deliberately left out

- Profile / thread list endpoints — not reverse-engineered. The user supplies
  `KAGI_PROFILE_ID` from devtools. See `docs/todo.md`.
- Tests — covered by manual end-to-end testing against real Kagi during dev.
  Unit tests for the stream parser would be cheap to add later.
- Session refresh / cookie auto-fetch — would require an OAuth/password flow.
  Out of scope for v1.

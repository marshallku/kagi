# Design decisions

## Why Go

- Matches the existing `~/dev/life-assistant` stack — easy to embed as a plugin
  later if useful.
- stdlib net/http + encoding/json is enough; no external deps.
- Single static binary works for both CLI and HTTP server modes.

## Auth: tiered fallback (env → keyring → auto-login)

Three layers, tried in order on every command:

1. `KAGI_SESSION` env var — explicit override.
2. OS keyring entry under service `kagi`, key `session`.
3. Auto-login via `KAGI_EMAIL`/`KAGI_PASSWORD` (if both set), invoked
   transparently the first time the client sees a missing/expired session.

Any successful login (explicit `kagi login` or auto-login on auth failure)
writes the new cookie back to the keyring via the client's `OnRefresh`
callback, so subsequent commands skip re-auth.

### Why keyring (libsecret on Linux, Keychain on macOS)

- Filesystem files require careful 0600 + parent-dir handling and are still
  readable by every process running as the user.
- Keyring entries are gated by the active desktop session (gnome-keyring /
  KWallet on Linux, login keychain on macOS). Sleeping the desktop locks
  them; switching users locks them.
- `zalando/go-keyring` is small, has no runtime daemon of its own, and
  delegates to whatever Secret Service is running.

Trade-off: a server-only host without a desktop session has no Secret
Service. For that case, fall back to setting `KAGI_SESSION` directly.

### Why support both `kagi login` and silent auto-login

`kagi login` is for one-time manual setup or for environments where you
want explicit failure on bad credentials. Silent auto-login is for
unattended automation — no operator needs to be available to refresh the
cookie when it expires. Both feed the same keyring entry.

`kagi login` falls back to stdin when env vars are missing: a TTY gets a
prompt + silent password read (`golang.org/x/term`); a pipe just reads two
lines (email then password). This matches the way password managers
typically feed credentials to CLIs (`pass kagi/email; pass kagi/pw`).

## Config file vs env vs keyring

Three persistence layers, picked by data type:

| Where                                    | What                          | Why                                                |
| ---------------------------------------- | ----------------------------- | -------------------------------------------------- |
| OS keyring (`kagi`/`session`)            | session cookie                | secret, machine-local, gated by desktop login.     |
| Config file (`~/.config/kagi/config.json`) | non-secret defaults (`model`) | survives reboots, doesn't pollute the env, easily inspected/edited, supports `kagi config set/get`. |
| Env vars (`KAGI_EMAIL`, `KAGI_PASSWORD`, `KAGI_SESSION`) | secrets only, sourced from password managers | natural fit for shell-driven workflows; secrets stay out of any file we write. |

`KAGI_MODEL` and `KAGI_PROFILE_ID` were both removed in favor of `kagi
config set <key> <value>`. The trigger for moving profile to config was
adding the `kagi profiles` discovery command (parses the embedded
`json-profile-list` on `/assistant`) — once you can list profile UUIDs,
managing them in a config file is strictly better than via env.

## Why we always spoof User-Agent

Set in `client.newRequest`, applied to every outbound call (signin, login,
prompt). Two reasons:

1. Kagi's CSP and rate-limiter may treat unidentified UAs differently. The
   browser-flavored UA has been verified to work end-to-end.
2. Avoids leaking that the client is a custom Go binary, which has no
   downside for legitimate usage and some upside if Kagi tightens detection.

The UA value is a real-Chrome-on-Linux template (no `HeadlessChrome`,
`python-requests`, or `Go-http-client/1.1`).

## 404-as-auth-fail handling

Kagi returns 404 (not 401/403) for unauth requests to /assistant/prompt to
obscure the endpoint. `client.isAuthFail` treats 404 as an auth-fail signal
and triggers one relogin attempt; if the retry also 404s, that's the final
error. This means a genuine 404 (bad thread/branch id) costs one extra
relogin round-trip but is otherwise correct — no infinite loop.

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

- Thread list / history endpoints — not reverse-engineered. See `docs/todo.md`.
- Tests — covered by manual end-to-end testing against real Kagi during dev.
  Unit tests for the stream parser would be cheap to add later.

# kagi

Reverse-engineered Go client for [Kagi Assistant](https://kagi.com/assistant). CLI + embedded HTTP server.

> **Disclaimer.** Unofficial, third-party project. Not affiliated with, endorsed by,
> or supported by Kagi. The protocol is reverse-engineered from observed browser
> traffic and may break at any time when Kagi updates their service. Use at your
> own risk; you are responsible for complying with [Kagi's Terms of Service](https://kagi.com/terms).
> Provided as-is, without warranty of any kind.

## Quick start

```bash
./install.sh   # builds and installs to ~/.local/bin (override BINDIR=...)

# pick one auth method:
export KAGI_EMAIL='you@example.com'           # auto-login (recommended)
export KAGI_PASSWORD='...'
# — or —
export KAGI_SESSION='<value of kagi_session cookie>'   # manual cookie

# discover & pick defaults (saved to ~/.config/kagi/config.json)
kagi models                         # list available models
kagi profiles                       # list custom assistants
kagi config set model grok-4-20     # ★ recommended ones marked
kagi config set profile <uuid>

# new conversation (will auto-login on first run, cache session in keyring)
kagi chat "What is 2+2?"

# follow-up (auto: continues most recent thread)
kagi chat --resume "And 3+3?"

# follow-up (manual: any thread, any parent message)
kagi chat -t <thread-id> --parent <message-id> "And 3+3?"

# JSON for automation
kagi chat --json "..." | jq -r .md

# HTTP server (auto-login + keyring also work here)
kagi serve -addr 127.0.0.1:8921 &
curl -s -X POST localhost:8921/chat \
  -H 'content-type: application/json' \
  -d '{"prompt":"hello"}' | jq .

# explicit session management
kagi login    # interactive (TTY: prompts, password silent) or piped:
#   printf '%s\n%s\n' "$email" "$pw" | kagi login
kagi logout   # delete cached session

# config (non-secret defaults)
kagi config get              # print all
kagi config get model        # print one key
kagi config set model ki_quick
kagi config set profile <uuid>
```

## Layout

```
kagi/
├── client/        importable Go library (Stream, Send, NewPrompt)
├── server/        HTTP wrapper (POST /chat, POST /chat/stream, GET /healthz)
├── cmd/kagi/      CLI entry (subcommands: chat, serve)
└── docs/          API analysis, decisions, todos, sample captures
```

## Auth

Resolution order on every command:

1. `KAGI_SESSION` env var (explicit override)
2. OS keyring (`kagi` / `session`, populated by `kagi login` or
   prior auto-login)
3. Silent auto-login via `KAGI_EMAIL` + `KAGI_PASSWORD`

Any successful login is written back to the keyring, so the next command
skips re-auth. Linux requires a running Secret Service (gnome-keyring,
KWallet, etc.); macOS uses the login keychain.

All outbound requests go out with a real-browser User-Agent, including the
sign-in flow.

See `docs/api.md` for the full reverse-engineering notes and `docs/decisions.md`
for design rationale.

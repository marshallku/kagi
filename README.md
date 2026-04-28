# kagi

Reverse-engineered Go client for [Kagi Assistant](https://kagi.com/assistant). CLI + embedded HTTP server.

## Quick start

```bash
./install.sh   # builds and installs to ~/.local/bin (override BINDIR=...)

# pick one auth method:
export KAGI_EMAIL='you@example.com'           # auto-login (recommended)
export KAGI_PASSWORD='...'
# — or —
export KAGI_SESSION='<value of kagi_session cookie>'   # manual cookie

export KAGI_PROFILE_ID='<your custom assistant uuid>'
export KAGI_MODEL='ki_quick'   # or grok-4-20, claude-4-sonnet, ...

# new conversation (will auto-login on first run, cache session in keyring)
kagi chat "What is 2+2?"

# follow-up (need both ids from a previous response)
kagi chat -t <thread-id> --parent <message-id> "And 3+3?"

# JSON for automation
kagi chat --json "..." | jq -r .md

# HTTP server (auto-login + keyring also work here)
kagi serve -addr 127.0.0.1:8921 &
curl -s -X POST localhost:8921/chat \
  -H 'content-type: application/json' \
  -d '{"prompt":"hello"}' | jq .

# explicit session management
kagi login    # one-shot login + cache
kagi logout   # delete cached session
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

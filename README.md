# kagi-cli

Reverse-engineered Go client for [Kagi Assistant](https://kagi.com/assistant). CLI + embedded HTTP server.

## Quick start

```bash
go build -o ~/bin/kagi ./cmd/kagi

export KAGI_SESSION='<value of kagi_session cookie>'
export KAGI_PROFILE_ID='<your custom assistant uuid>'
export KAGI_MODEL='ki_quick'   # or grok-4-20, claude-4-sonnet, ...

# new conversation
kagi chat "What is 2+2?"

# follow-up (need both ids from a previous response)
kagi chat -t <thread-id> --parent <message-id> "And 3+3?"

# JSON for automation
kagi chat --json "..." | jq -r .md

# HTTP server
kagi serve -addr 127.0.0.1:8921 &
curl -s -X POST localhost:8921/chat \
  -H 'content-type: application/json' \
  -d '{"prompt":"hello"}' | jq .
```

## Layout

```
kagi-cli/
├── client/        importable Go library (Stream, Send, NewPrompt)
├── server/        HTTP wrapper (POST /chat, POST /chat/stream, GET /healthz)
├── cmd/kagi/      CLI entry (subcommands: chat, serve)
└── docs/          API analysis, decisions, todos, sample captures
```

## Auth

Kagi uses a single session cookie (`kagi_session`, HttpOnly). Open
[kagi.com](https://kagi.com), DevTools → Application → Cookies → copy `kagi_session`.
The cookie expires; refresh from browser when 401s appear.

See `docs/api.md` for the full reverse-engineering notes and `docs/decisions.md`
for design rationale.

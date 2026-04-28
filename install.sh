#!/usr/bin/env bash
set -euo pipefail

# Install kagi to $BINDIR (default: $HOME/.local/bin).
# Override:  BINDIR=/usr/local/bin sudo ./install.sh

BINDIR="${BINDIR:-$HOME/.local/bin}"
SRCDIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

mkdir -p "$BINDIR"

echo "→ building kagi..."
( cd "$SRCDIR" && go build -trimpath -ldflags='-s -w' -o "$BINDIR/kagi" ./cmd/kagi )

echo "✓ installed: $BINDIR/kagi"

case ":$PATH:" in
    *":$BINDIR:"*)
        echo "✓ $BINDIR is on PATH"
        ;;
    *)
        echo
        echo "⚠ $BINDIR is not on PATH."
        case "${SHELL##*/}" in
            zsh)  rc="$HOME/.zshrc" ;;
            bash) rc="$HOME/.bashrc" ;;
            *)    rc="your shell's rc file" ;;
        esac
        echo "  add to $rc:"
        echo "    export PATH=\"$BINDIR:\$PATH\""
        ;;
esac

echo
echo "next: kagi help"

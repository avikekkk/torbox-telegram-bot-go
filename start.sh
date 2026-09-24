#!/usr/bin/env bash
# Run the bot in the foreground. Press Ctrl+C to stop.
set -e

cd "$(dirname "$0")"

if [[ ! -f .env ]]; then
    echo "start.sh: .env not found in $(pwd)" >&2
    echo "  cp .env.example .env   and fill it in" >&2
    exit 1
fi

# Binaries go under bin/ so the build cannot collide with a directory of the
# same name, and the repo root stays clean.
go build -o bin/torbot ./cmd/torbot
exec ./bin/torbot

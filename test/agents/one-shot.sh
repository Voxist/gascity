#!/bin/bash
# Bash agent: one-shot worker.
# Implements the same flow as prompts/one-shot.md using gc CLI commands.
# Polls hook until work appears, processes one bead, then exits.
#
# Required env vars (set by gc start):
#   GC_AGENT — this agent's name
#   GC_CITY  — path to the city directory
#   PATH     — must include gc binary

set -euo pipefail
cd "$GC_CITY"
ASSIGNEE="${GC_SESSION_NAME:-$GC_AGENT}"

while true; do
    ready=$(bd ready --assignee="$ASSIGNEE" --include-ephemeral 2>/dev/null || true)

    if echo "$ready" | grep -qE '^[^[:alnum:]]*gc-'; then
        id=$(echo "$ready" | grep -E '^[^[:alnum:]]*gc-' | head -1 | grep -oE 'gc-[[:alnum:]]+' | head -1)
        bd close "$id"
        exit 0
    fi

    sleep 0.5
done

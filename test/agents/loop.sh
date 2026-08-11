#!/bin/bash
# Bash agent: loop worker.
# Implements the same flow as prompts/loop.md using gc CLI commands.
# Continuously drains the backlog: check claim → claim from ready → close → repeat.
#
# Required env vars (set by gc start):
#   GC_AGENT — this agent's name
#   GC_CITY  — path to the city directory
#   PATH     — must include gc binary

set -euo pipefail
cd "$GC_CITY"
ASSIGNEE="${GC_SESSION_NAME:-$GC_AGENT}"

while true; do
    hooked=$(bd ready --assignee="$ASSIGNEE" --include-ephemeral 2>/dev/null || true)
    if echo "$hooked" | grep -qE '^[^[:alnum:]]*gc-'; then
        id=$(echo "$hooked" | grep -E '^[^[:alnum:]]*gc-' | head -1 | grep -oE 'gc-[[:alnum:]]+' | head -1)
        bd close "$id"
        continue
    fi

    ready=$(bd ready 2>/dev/null || true)
    if echo "$ready" | grep -qE '^[^[:alnum:]]*gc-'; then
        id=$(echo "$ready" | grep -E '^[^[:alnum:]]*gc-' | head -1 | grep -oE 'gc-[[:alnum:]]+' | head -1)
        bd update "$id" --assignee="$ASSIGNEE" 2>/dev/null || true
        continue
    fi
    sleep 0.5
done

#!/bin/bash
# Bash agent: dog warrant executor.
# Simulates the dog role in shutdown dance: checks assigned work,
# reads warrant metadata, checks target agent health,
# closes the warrant after interrogation.
#
# Required env vars (set by gc start):
#   GC_AGENT — this agent's name
#   GC_CITY  — path to the city directory
#   PATH     — must include gc and bd binaries

set -euo pipefail
cd "$GC_CITY"
ASSIGNEE="${GC_SESSION_NAME:-$GC_AGENT}"

while true; do
    hooked=$(bd ready --assignee="$ASSIGNEE" --include-ephemeral 2>/dev/null || true)
    if echo "$hooked" | grep -qE '^[^[:alnum:]]*gc-'; then
        warrant_id=$(echo "$hooked" | grep -E '^[^[:alnum:]]*gc-' | head -1 | grep -oE 'gc-[[:alnum:]]+' | head -1)

        details=$(bd show "$warrant_id" 2>/dev/null || true)
        _="${details}"

        bd close "$warrant_id" 2>/dev/null || true

        exit 0
    fi

    sleep 0.2
done

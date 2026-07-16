#!/usr/bin/env python3
# Gas City hooks for Kimi CLI.
# Installed by gc into {workDir}/.kimi/hooks/gascity-session-start.py
# Managed by `gc hooks install`; put custom Kimi hooks in separate files so
# upgrades can replace this file safely.
import json
import os
import subprocess
import sys

GC_KIMI_HOOK_VERSION = 1


def main() -> int:
    try:
        payload = json.load(sys.stdin)
    except Exception:
        payload = {}

    session_id = str(payload.get("session_id") or "").strip()
    cwd = str(payload.get("cwd") or os.getcwd()).strip() or os.getcwd()

    env = os.environ.copy()
    home = env.get("HOME", "")
    env["PATH"] = f"{home}/go/bin:{home}/.local/bin:/opt/homebrew/bin:/usr/local/bin:" + env.get("PATH", "")
    env["GC_MANAGED_SESSION_HOOK"] = "1"
    env["GC_HOOK_EVENT_NAME"] = "SessionStart"
    env["GC_PROVIDER_SESSION_ID_REQUIRED"] = "kimi"
    if session_id:
        env["GC_PROVIDER_SESSION_ID"] = session_id

    # GC_BIN is the explicit override (the deployed binary gc exports into
    # every agent process). The PATH prefix above stays for gc's own
    # downstream tool resolution; it no longer selects which gc build runs.
    gc_bin = env.get("GC_BIN") or "gc"
    proc = subprocess.run([gc_bin, "prime", "--hook"], cwd=cwd, env=env)
    return proc.returncode


if __name__ == "__main__":
    raise SystemExit(main())

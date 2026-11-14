# `gc dolt sync` per-database statuses

`gc dolt sync` fetches each database before pushing and classifies the result.
This is the status vocabulary an operator or patrol sees on stderr.

| Status | Meaning | Transient? |
| --- | --- | --- |
| `up-to-date` | Nothing to push. | — |
| `pushed` | Fast-forward push succeeded. | — |
| `behind <n>` | Remote has commits the local branch lacks — pull needed. | Yes — resolves once pulled. |
| `diverged (<a> ahead / <b> behind)` | Local and remote have both moved — manual reconcile needed. | No — needs a human/agent decision. |
| `first-push` | Remote branch does not exist yet (empty remote, or a brand-new branch); the push creates it. | — |
| `fetch timed out after <n>s (exit <rc>, attempt <i>/<n>)` | The wall-clock fetch bound (`GC_DOLT_SYNC_FETCH_TIMEOUT_SECS`) fired on attempt `<i>`: exit 124, exit 137 (the bound escalated to SIGKILL), or a failure that took the whole bound. A timed-out attempt is not retried. | Usually — raise the bound or wait for the next cycle. |
| `fetch attempt <i>/<n> failed (exit <rc>) after <elapsed>s: <first stderr line> — retrying` | One fetch attempt failed and the budget (`GC_DOLT_SYNC_FETCH_ATTEMPTS`, 1-20, default 5) allows another; the next try starts after `GC_DOLT_SYNC_FETCH_RETRY_DELAY_SECS` (0-60, default 2). A first-push signal and a timeout are not retried. | Yes — a cold store warms across attempts (vp-q3kp). |
| `fetch failed (exit <rc>) after <i> attempts, last after <elapsed>s (listener deadline <n>s)` | Every attempt the budget allowed failed, the last for a reason unrelated to the listener deadline (e.g. a corrupt remote archive). | Depends on the cause — read the replayed dolt stderr of the last attempt. |
| `...; run fetch-retry budget of <n>s spent` | Appended to a fetch failure or `COLD-OPEN WALL` line when the run's cumulative fetch time reached `GC_DOLT_SYNC_FETCH_RETRY_BUDGET_SECS` (0-3600, default 300, half of `dolt-remotes-patrol`'s 600s deadline) and stopped the retries. Every later store in the run gets a single attempt. | Yes — the next cycle starts with a fresh budget. |
| `COLD-OPEN WALL` | See below. | Not within this run — the next cycle continues the warm-up. |

## `COLD-OPEN WALL`

```
<db>: COLD-OPEN WALL — fetch killed at <elapsed>s (listener deadline <n>s);
did not converge in <i> attempt(s) this run; the next cycle continues the
warm-up — skipped (NOT pushed)
```

**What it means.** A store's first remote operation in a given Dolt server
lifetime must spool its whole remote blobset (`GitBlobstore` has no
server-side range read). If that spool takes longer than the managed
listener's hard per-query deadline (`read_timeout_millis`, live value in the
generated `dolt-config.yaml`), the listener kills the query server-side
before the spool completes. The result looks like a generic fetch failure,
but this run pushed **nothing** for the database: its off-box copy is as old
as its last successful push.

Classified by **elapsed time against the live listener deadline** (≥ 90%),
not by matching an error string: the exact text at the wall is not reliably
recorded, but the elapsed-time signature is the measured physics regardless
of which layer reports the kill (vp-9v6f9).

**It is reported after the retry budget, for this run only.** A fetch that
dies at the wall is retried like any other failure, because partial spool
progress is kept across attempts and most cold stores converge within a few
tries (vp-q3kp). The status appears only when the last attempt of the run's
budget still dies at the wall. The progress is kept, so the next cycle
continues the warm-up, and a store that converges then is pushed and drops
out of the summary. A store that keeps appearing cycle after cycle is the
one to escalate: measured, the largest store gained about 4 KB per attempt
against a 2.24 GB cache.

**The fix is not in this script.** A durable fix (an automatic warming pass
that runs its first fetch in a context that tolerates the cold-open cost)
needs an architecture decision — a dedicated maintenance listener, a chunked
push, or a supervised raise/restore window — tracked in **vp-6hb8**. This
script only makes the condition visible; it does not close the gap.

**End-of-run summary.** A run that hits the wall on one or more databases
prints a `NO OFF-BOX COPY THIS RUN:` block listing every affected store, in addition
to the per-database line above — so the condition is visible even in output
scanned quickly for the summary line only.

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
| `fetch timed out after <n>s` | The wall-clock fetch bound (`GC_DOLT_SYNC_FETCH_TIMEOUT_SECS`) fired. | Usually — raise the bound or retry. |
| `fetch failed (exit <n>) after <elapsed>s (listener deadline <n>s)` | The fetch failed for a reason unrelated to the listener deadline (e.g. a corrupt remote archive). | Depends on the cause — read the replayed dolt stderr. |
| `COLD-OPEN WALL` | See below. | **No.** |

## `COLD-OPEN WALL`

```
<db>: COLD-OPEN WALL — fetch killed at <elapsed>s (listener deadline <n>s);
NO off-box copy this server lifetime — skipped (NOT pushed)
```

**What it means.** A store's first remote operation in a given Dolt server
lifetime must spool its whole remote blobset (`GitBlobstore` has no
server-side range read). If that spool takes longer than the managed
listener's hard per-query deadline (`read_timeout_millis`, live value in the
generated `dolt-config.yaml`), the listener kills the query server-side
before the spool can persist. The result looks like a generic fetch failure,
but the database in question has **no off-box copy in this server lifetime**
— steady-state sync is, for that database, not running at all.

Classified by **elapsed time against the live listener deadline** (≥ 90%),
not by matching an error string: the exact text at the wall is not reliably
recorded, but the elapsed-time signature is the measured physics regardless
of which layer reports the kill (vp-9v6f9).

**It is not transient.** Every attempt in the affected server lifetime dies
at the same wall — no retry budget converges it. Retrying wastes a fetch
attempt and reports the same status.

**The fix is not in this script.** A durable fix (an automatic warming pass
that runs its first fetch in a context that tolerates the cold-open cost)
needs an architecture decision — a dedicated maintenance listener, a chunked
push, or a supervised raise/restore window — tracked in **vp-6hb8**. This
script only makes the condition visible; it does not close the gap.

**End-of-run summary.** A run that hits the wall on one or more databases
prints a `NO OFF-BOX COPY:` block listing every affected store, in addition
to the per-database line above — so the condition is visible even in output
scanned quickly for the summary line only.

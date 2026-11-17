# Ordering pull request check settlements

Read this when you write or review code that consumes `pr.<n>.checks` settlements and must decide which of several settlements of one commit is current.

## What a settlement carries

- A commit can settle more than once. A later settlement can carry `superseded_settlement: "true"`, and an exact copy can arrive, because delivery is at-least-once.
- `check_runs`, the attempt set: the latest GitHub check-run id per check name, sorted by name.
- `generation`: the listener record's state version. `snapshot`: the record's hash.
- `statuses`: each commit status the record holds, as `{context, state}`, sorted by context, with `state` one of `success`, `failure` and `error`. The field is left out when the record holds none. A status is no check run: it is in no attempt set, no status group and no failing name, and a status and a check run of one name are two entries.
- Dispatch's CI reconcile records a head's statuses from GitHub. The listener drops GitHub's `status` event, which names no pull request.
- A head publishes only when at least one check has a positive run id, so a status alone does not make a head publish. Legacy checks without one appear in the status groups and the failing names, never in `check_runs`.

## Order two attempt sets of one commit

Compare the ids of the names both sets share:

1. No id lower, and some id higher or a new name: newer. A new name counts as higher.
2. Every shared id equal and no new name: the same set.
3. No id higher, no new name, and some id lower: older.
4. Anything else (a higher or new id alongside a lower one): a mixed view. Drop it as a conflict.

Ignore names that only the stored set has: a check can vanish from GitHub's view, and a record recreated after the seven-day KV TTL starts sparse.

## Keep a fence

- The fence is the per-name maximum id over every set you accepted. Merge each accepted set into it, and prune nothing.
- Within one listener record, per-name ids never decrease, so the fence never decreases either.
- So a newer attempt is newer whatever its completion time, no timestamp takes part in ordering, and a name an incomplete view left out cannot later come back as new.

## At the same set, order by generation

- A lower `generation` is stale.
- An equal `generation` with the same `snapshot` is a duplicate. With a different `snapshot`, drop it as a conflict: that pair occurs only when a record was recreated and reused a generation.

## Apply a live settlement

- A live settlement (one the listener sent) may be an incomplete view of the head: a webhook was missed, or the record was recreated after the KV TTL.
- It decides the outcome of every name it reports, at any id the ordering accepted, the same run seen in place included. It says nothing about the names it leaves out.
- It decides the state of every commit status context its `statuses` names. It says nothing about a context it leaves out.
- A known failure among the names it leaves out stands, because you keep failure names, not a per-name status map. A known failed status of a context it leaves out stands the same way. The head is red while any failure remains.

## Reconcile with GitHub's rollup

- Compare the rollup's attempt set in the same way. GitHub's read is complete: its failing check runs and failing commit statuses replace the stored ones wholesale.
- Keep commit statuses apart from check-run failures, because a status has no check run. A check run with a status's name cannot retire that status: only a newer state of that context can, in a settlement's `statuses` or in GitHub's read. Only GitHub's read can retire a deleted check's failure.
- A newer rollup set merges into the fence and takes the identity, with no listener generation.
- The same set applies GitHub's verdict and keeps the listener identity for duplicate detection.
- Ignore an older set, a mixed set, and an empty set when the fence already holds ids.

## After a terminal GitHub read

A terminal read is a green or red one. GitHub's verdict then holds authority at that set:

- Accept a live settlement at the same set only if its effective outcome agrees with GitHub's verdict. The effective outcome is the check-run failures it reports, plus the stored ones it leaves out, plus the failed commit statuses it names, plus the stored commit-status failures of the contexts it leaves out. Accepting it refreshes the listener identity, and GitHub's verdict keeps its authority.
- A live settlement that disagrees is stale, whatever its generation, until the set advances.
- A pending or cancelled-only read takes the green off a green head, leaves a red head red, and ends GitHub's authority at that set. The next terminal live settlement then applies at once, under the usual generation and duplicate rules: a replay or a lower generation still does not apply.
- So the order of reads matters: a pending read after a live green takes the green off until the next terminal view.

## Known gaps

- A run whose conclusion changes in place, on the same run id: GitHub's view stands. The listener's view comes back with the next successful, non-skipped read at that set; the dropped delivery is not replayed.
- A check whose highest run is deleted on GitHub: the fence keeps that id, so a rollup that reports a lower run under that name is older until a newer run appears.
- A consumer that orders head changes by the PR's `updated_at` (one-second resolution) accepts a read of another head at an equal clock. A stale read that returns the previous head in the same second as its replacement rewinds that consumer until its next accurate, non-skipped read.
- A legacy in-progress check whose completion is never seen holds the head unsettled until it reruns: rerun that check to release it.
- A commit status reaches a settlement only once a pass of Dispatch's CI reconcile records it, which runs every 5 minutes, and only where Dispatch's App token holds Commit statuses. A new context or a changed state recorded after its head settled settles the head again, minutes after GitHub has it. Where the token does not hold Commit statuses, no settlement names a status.
- A record at its size bound records no further status, and drops the statuses it holds where a check run needs the room, so a settlement of such a head can name fewer statuses than GitHub holds.

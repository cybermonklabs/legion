# Ordering pull request check settlements

Read this when you write or review code that consumes `pr.<n>.checks` settlements and must decide which of several settlements of one commit is current.
The Legion daemon's implementation is the reference: `CompareAttemptSets`, `ClassifySettlement`, `EffectiveOutcome` and `ApplySettlement` in `packages/daemon/internal/classify`.

## What a settlement carries

- A commit can settle more than once. A later settlement can carry `superseded_settlement: "true"`, and an exact copy can arrive.
- `check_runs`, the attempt set: the latest GitHub check-run id per check name, sorted by name. `generation`: the listener record's state version. `snapshot`: the record's hash.
- A head publishes only when at least one check has a positive run id. A legacy check without one is in the status groups and `failing_checks`, never in `check_runs`.
- `statuses`: each commit status the record holds, as `{context, state, created_at}`. Dispatch's CI reconcile records them from GitHub, where its App token holds the Commit statuses permission; the listener drops GitHub's `status` event. A status alone never makes a head publish.

## Order settlements of one commit

Compare the check-run ids of the names both attempt sets share; a name only the incoming set has counts as higher:

1. Some id higher, none lower: newer.
2. Every id equal: the same set.
3. Some id lower, none higher: older, so stale.
4. Higher and lower together: a mixed view. Apply it, and decide each name by its newest run.

- Ignore names only the stored set has: a check can vanish from GitHub's view, and a record recreated after the seven-day KV TTL starts sparse.
- Keep a fence: the per-name maximum id over every set you accepted, pruning nothing. No timestamp orders check runs.
- At the same set, a higher `generation` is newer and a lower one stale. An equal `generation` with the same `snapshot` is a duplicate; with a different `snapshot`, drop it as a conflict, since only a recreated record reuses a generation.
- Order two statuses of one context by `created_at`, and keep the later. A settlement that is otherwise stale, a duplicate or a conflict still applies a status entry with a later `created_at`, and changes nothing else.

## Apply a live settlement

- A live settlement may be an incomplete view of the head: a webhook was missed, or the record was recreated. It decides every name it reports and says nothing about the names it leaves out.
- A known failure among the names it leaves out stands. The head is red while any check-run failure remains, or while a status you keep is `failure` or `error`.
- Keep commit statuses apart from check-run failures, because a status has no check run: a check run with a status's name cannot retire that status.

## Reconcile with GitHub's rollup

- GitHub's read is complete: its failing check runs and failing statuses replace the stored ones wholesale.
- Compare its attempt set in the same way. A newer set merges into the fence and takes the identity, with no listener generation; the same set applies GitHub's verdict and keeps the listener identity for duplicate detection; ignore an older set, a mixed set, and an empty set when the fence already holds ids.
- After a terminal (green or red) read, accept a live settlement at the same set only if its effective outcome agrees with GitHub's verdict: the check-run failures it reports, plus the stored ones it leaves out, plus each failed status you keep once its `statuses` merge in. One that disagrees is stale, whatever its generation, until the set advances.
- A pending or cancelled-only read takes the green off a green head, leaves a red head red, and ends GitHub's authority at that set; the next terminal live settlement then applies under the usual generation and duplicate rules.

## Known gaps

- A run whose conclusion changes in place, on the same run id: GitHub's view stands until the next successful, non-skipped read at that set.
- A check whose highest run is deleted on GitHub: the fence keeps that id, and only GitHub's read retires its failure.
- A legacy in-progress check whose completion is never seen holds the head unsettled: rerun that check.
- A consumer that orders head changes by the pull request's `updated_at` (one-second resolution) accepts a stale read of the previous head made in the same second as its replacement, until its next accurate read.

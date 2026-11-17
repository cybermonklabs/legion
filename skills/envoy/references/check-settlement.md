# Ordering pull request check settlements

Read this when you write or review code that consumes `pr.<n>.checks` settlements and must decide which of several settlements of one commit is current.

## What a settlement carries

- A commit can settle more than once. A later settlement can carry `superseded_settlement: "true"`, and an exact copy can arrive, because delivery is at-least-once.
- `check_runs`, the attempt set: the latest GitHub check-run id per check name, sorted by name.
- `generation`: the listener record's state version. `snapshot`: the record's hash.
- `statuses`: each commit status the record holds, as `{context, state, created_at}`, sorted by context, with `state` one of `success`, `failure` and `error`, and `created_at` the time GitHub created that status, in GitHub's RFC 3339 text. The field is left out when the record holds none. A status is no check run: it is in no attempt set, no status group and no failing name, and a status and a check run of one name are two entries.
- Dispatch's CI reconcile records a head's statuses from GitHub. The listener drops GitHub's `status` event, which names no pull request.
- A head publishes only when at least one check has a positive run id, so a status alone does not make a head publish. Legacy checks without one appear in the status groups and the failing names, never in `check_runs`.

## Order two attempt sets of one commit

Compare the ids of the names both sets share:

1. No id lower, and some id higher or a new name: newer. A new name counts as higher.
2. Every shared id equal and no new name: the same set.
3. No id higher, no new name, and some id lower: older.
4. Anything else (a higher or new id alongside a lower one): a mixed view. Apply it, and decide each name by its newest run when you merge it into the fence, as the Legion daemon does (`ClassifySettlement` and `mergeAttemptSets`, `packages/daemon/internal/classify`).

Ignore names that only the stored set has: a check can vanish from GitHub's view, and a record recreated after the seven-day KV TTL starts sparse.

## Keep a fence

- The fence is the per-name maximum id over every set you accepted. Merge each accepted set into it, and prune nothing.
- Within one listener record, per-name ids never decrease, so the fence never decreases either.
- So a newer attempt is newer whatever its completion time, no timestamp takes part in ordering check runs, and a name an incomplete view left out cannot later come back as new.

## Order commit statuses by `created_at`

A status has no id, and a record recreated after the KV TTL numbers its settlements afresh, so a generation does not order two statuses of one context. Each entry's `created_at` orders them:

- A settlement whose attempt set is newer or mixed, or the same at a higher generation, is newer, and you apply it whole: its check runs, failing and cancelled names, generation, snapshot and statuses.
- A settlement whose attempt set is older, or the same at a generation no higher than yours, and that names a context you keep no status of for that commit or an entry with a later `created_at` than the one you keep for its context, changes those statuses alone. Keep your check runs, failing and cancelled names, generation and snapshot as they are, so the newer settlement delivered again is still a duplicate.
- The same set includes one that names fewer checks than yours, because names that only the stored set has are ignored. A settlement that a record sent before a later check name appeared compares equal to the fence of the one it sent after, so at a generation no higher than yours it changes its newer statuses alone.
- A settlement with an older attempt set that names no such entry is older for every check run and every status. It is stale.
- At the same set, a settlement that names no such entry is ordered by its generation, as the next section says.

## At the same set, with no newer status, order by generation

- A higher `generation` is newer. It is newer with a newer status too, as the first rule above says.
- A lower `generation` is stale.
- An equal `generation` with the same `snapshot` is a duplicate. With a different `snapshot`, drop it as a conflict: that pair occurs only when a record was recreated and reused a generation. A status that changed on such a record still applies, alone, because its later `created_at` makes it news.

## Apply a live settlement

- A live settlement (one the listener sent) may be an incomplete view of the head: a webhook was missed, or the record was recreated after the KV TTL.
- It decides the outcome of every name it reports, at any id the ordering accepted, the same run seen in place included. It says nothing about the names it leaves out.
- Its `statuses` is every status the record holds for that commit. A record at its size bound, or one that a listener of an earlier release wrote, names fewer statuses than GitHub holds.
- Merge its `statuses` into the statuses you keep for that commit, one context at a time: keep the entry with the later `created_at`. When the two times are equal, keep the settlement's entry where you apply the settlement whole, and your own where it changes its statuses alone, because it is then the older view. A context it leaves out stays as you kept it, and an older entry never puts back a state that a later one replaced. The Legion daemon does the same (`ApplySettlement` and `mergedStatuses` in `packages/daemon/internal/classify`).
- A known failure among the names it leaves out stands, because you keep failure names, not a per-name status map. The head is red while any check-run failure remains, or while a status you keep for it is `failure` or `error`.

## Reconcile with GitHub's rollup

- Compare the rollup's attempt set in the same way. GitHub's read is complete: its failing check runs and failing commit statuses replace the stored ones wholesale.
- Keep commit statuses apart from check-run failures, because a status has no check run. A check run with a status's name cannot retire that status: only a settlement's entry of that context that the merge keeps, or GitHub's read, can. Only GitHub's read can retire a deleted check's failure.
- A newer rollup set merges into the fence and takes the identity, with no listener generation.
- The same set applies GitHub's verdict and keeps the listener identity for duplicate detection.
- Ignore an older set, a mixed set, and an empty set when the fence already holds ids.

## After a terminal GitHub read

A terminal read is a green or red one. GitHub's verdict then holds authority at that set:

- Accept a live settlement at the same set only if its effective outcome agrees with GitHub's verdict. The effective outcome is the check-run failures it reports, plus the stored ones it leaves out, plus each failed status among the statuses you keep for that commit once its `statuses` is merged into them. Accepting it refreshes the listener identity, and GitHub's verdict keeps its authority.
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

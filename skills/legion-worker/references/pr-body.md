# The pull request body and the simplify pass

Read it before you write or edit the pull request body, or run the simplify pass.
A check's result and the evidence each phase owes: `skill://legion-worker/references/results.md`.

## The pull request body template

The implementer writes the body from this template when the pull request opens; later phases keep it current.
Edit the live body: read it (`gh api repos/{owner}/{repo}/pulls/{number} --jq .body`), change your lines, write it back (`--method PATCH ... -F body=@body.md`).

```
## For the reviewer

**Outcome:** <one sentence a user of this repository would recognise: what someone can now do, or what stops going wrong>
**Why:** <the problem, one or two sentences, ending with the Dispatch key in parentheses — the key only, never a URL>
**Change:**
- <two to five bullets, each one behaviour a user or operator meets, never a file name>
**Look at first:** <one to three `path:line` places where a wrong decision would hurt> (the reviewer writes this line; with no reviewer step, the implementer does)
**Proven by:** <the surface and the run of the check the change turns on, one line>
**Not proven / risk:**
- <one line per claim recorded as unproven before the merge>, or the single word `none` (the reviewer writes this line; with no reviewer step, the implementer does; `none` is invalid while such a claim stands)
**Size:** <files changed, +added/−removed>

## Verification

**Threads:** <n> resolved, 0 unresolved. Each answered and resolved individually by the implementer, the pull request's author, never in bulk (the reviewer names the bot threads it accepted; it resolves none itself; with no reviewer step, the implementer resolves each bot thread it fixed or declined, with its reason in the reply):
- Thread <node id>: fixed in <commit-sha> — <one line>; resolved at <head-sha>.
- Thread <node id>: not a defect — <reason>; resolved at <head-sha>.
- Thread <node id>: accepted by the reviewer — <its reason, one line>; resolved at <head-sha>.
- Thread <node id>: still open — <what remains>; unresolved at <head-sha>.

**Thermo:** `simplify-settled-diff` once at <head-sha>, a docs-only PR included: <0 applied | applied → new head <sha>>; defects reported: <none | <n>, fixed in <sha>>; thermonuclear pair at the reviewed head <sha> over <the whole change | <last-reviewed-sha>..<sha>>, one row per half:
- `thermonuclear-deep-review`: <APPROVE | CHANGES>, <n> findings stand
- `thermonuclear-code-quality`: <APPROVE | CHANGES>, <n> findings stand
Where the diff the round reads touches no runtime code, each row reads `not run (<the whole change | <last-reviewed-sha>..<sha>> touches no runtime code)`, and the two rows of the last round the pair ran in stay beside them.
With no reviewer step, the implementer writes the pair as the one line `thermonuclear pair: not run (no reviewer step in this task)` instead of the two rows, whatever the diff touches; do not record a second pair.

**Checks:** one line per `check` block of the issue's Verification document, in the document's
order. Each is the line `dispatch result` printed when the result was recorded, or the
`Pull request line` that `dispatch results` prints for a check with `ci_check`:
- Check <block id> (<when>): <pass|fail> at <the commit>, recorded by <role>. <the reading that role recorded, cut with an ellipsis where it is long>
- Check <block id> (before_merge): <pass|fail> at <the commit>, from CI run <name>. <the check's words>
- Check <block id> (before_merge): CI run <name> has no result (<case>)
- Check <block id> (after_merge): not yet run

**Fast-follow:** "none", or one named cleanup item in a file this pull request does not touch, and where it will land.

**Chain:** stacked on <base bookmark> frozen at <sha> / not stacked.
```

GitHub refuses a body over 65,536 characters: past about 48,000, link evidence rather than inline it.

## The brief for the human

The implementer keeps `Outcome`, `Why`, `Change`, `Proven by` and `Size` true after every push.
At each round the reviewer writes `Look at first` and `Not proven / risk`, copying word for word every claim recorded as unproven (the tester's fail results, its own review).
With no reviewer step, the implementer writes both, including what its own evidence does not prove; where the body claims a check that has not concluded at the head, the implementer names that check under `Not proven / risk`.
With no reviewer step, at retro the implementer updates `Not proven / risk` after reading the tester's handoff where the task has a tester step.
A requirement there is unmet, unless only the deploy can show it and the plan names its observation.
A stale `Outcome` is a finding against the implementer; the merger rewrites nothing.

## The simplify pass and the `Thermo` line

- The implementer runs `skill://simplify-settled-diff` once per pull request, in its first implementing round, after its own results and before it completes, whatever the diff touches, a docs-only diff included.
  Applied: rerun the fast local checks and the issue's checks the simplify diff touches, and record them at the applied head.
  A defect the pass reports is fixed outside the pass and the results recorded again, never dropped as out of scope;
  the `Thermo` line names the count and the fixing commit.

- The reviewer's pair runs at the head each round reviews, over the diff that round reads, when that diff touches runtime code;
  the `Thermo` line records the range it read and a row per half, and earlier rounds' rows stay beside them.

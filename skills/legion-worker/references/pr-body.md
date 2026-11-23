# PR body, recorded results, and the simplify pass

Read it before you edit the pull request body, record or read a check's result, or run the simplify pass.

## The pull request body template

The implementer writes the body from this template when the pull request opens; later phases keep it current. Edit the live body: read it (`gh api repos/{owner}/{repo}/pulls/{number} --jq .body`), change your lines, write it back (`--method PATCH ... -F body=@body.md`).

```
## For the reviewer

**Outcome:** <one sentence a user of this repository would recognise: what someone can now do, or what stops going wrong>
**Why:** <the problem, one or two sentences, ending with the Dispatch key in parentheses — the key only, never a URL>
**Change:**
- <two to five bullets, each one behaviour a user or operator meets, never a file name>
**Look at first:** <one to three `path:line` places where a wrong decision would hurt> (the reviewer writes this line)
**Proven by:** <the surface and the run of the check the change turns on, one line>
**Not proven / risk:**
- <one line per claim recorded as unproven before the merge>, or the single word `none` (the reviewer writes this line; `none` is invalid while such a claim stands)
**Size:** <files changed, +added/−removed>

## Verification

**Threads:** <n> resolved, 0 unresolved. Each answered and resolved individually by the implementer, the pull request's author, never in bulk (the reviewer names the bot threads it accepted; it resolves none itself):
- Thread <node id>: fixed in <commit-sha> — <one line>; resolved at <head-sha>.
- Thread <node id>: not a defect — <reason>; resolved at <head-sha>.
- Thread <node id>: accepted by the reviewer — <its reason, one line>; resolved at <head-sha>.
- Thread <node id>: still open — <what remains>; unresolved at <head-sha>.

**Thermo:** `simplify-settled-diff` once at <head-sha>, a docs-only PR included: <0 applied | applied → new head <sha>>; defects reported: <none | <n>, fixed in <sha>>; thermonuclear pair at the reviewed head <sha> over <the whole change | <last-reviewed-sha>..<sha>>, one row per half:
- `thermonuclear-deep-review`: <APPROVE | CHANGES>, <n> findings stand
- `thermonuclear-code-quality`: <APPROVE | CHANGES>, <n> findings stand
Where the diff the round reads touches no runtime code, each row reads `not run (<the whole change | <last-reviewed-sha>..<sha>> touches no runtime code)`, and the two rows of the last round the pair ran in stay beside them.

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

The implementer keeps `Outcome`, `Why`, `Change`, `Proven by` and `Size` true after every push. At each round the reviewer writes `Look at first`, and `Not proven / risk`, copying word for word every claim recorded as unproven (the tester's `failures`, its own review). A requirement there is unmet, unless only the deploy can show it and the plan names its observation. A stale `Outcome` is a finding against the implementer; the merger rewrites nothing.

## What a result is

- **A check** is a `check` block of the issue's Verification document: what it `serves`, its `surface` (`browser`, `cli` or `api`), its `run`, its `when` (`before_merge`, `after_merge` or `default_branch`) and its `negative_control`. The architect writes the checks; one carrying `ci_check` is CI's (`skill://legion-worker/references/ci-checks.md`).
- **A result** is one run of one check: `dispatch result --issue <issue> --check <block id> --role <your role> --verdict pass --commit <sha>`, with `--surface`, `--run`, `--observed` and `--negative-control`. These refuse a bare `-`: use `--run-file` and its kin, one reading stdin per call. Put the printed line into the body unchanged.
- The surface is production-like (the real-process harness, a sandbox repository, a real browser, a development stack, staging) and has the resource the change touches; `--run` or `--observed` links to the run. A unit or integration test is a regression lock, never a result, except as the job a `ci_check` names.
- Record a result only after its commit is pushed (`400 RESULT_COMMIT_UNKNOWN`).
- **A check that cannot fail is not a result.** The negative control shows it failing on a deliberately wrong input. A documented procedure counts only when it ran: reading it is not a run.
- A `before_merge` result stands while its commit is the head of an open pull request Dispatch attributes to the check's issue (`packages/envoy/internal/dispatch/delivery/attribution.go`), or the final head of a merged one, and while no part of the check's block has changed. So after a push, record each check with no `ci_check` again.

## The rules every phase's evidence follows

- The tester tries to break the changed surface (wrong and boundary inputs, the second and the concurrent run, the party it must refuse) and writes the red test for a way no test would catch. Lint, types and the unit and integration suites are CI's: read them on GitHub at the head, rerun no lane as evidence, and wait for none.
- After a forward merge, the tester reruns each check the merge can reach (`skill://legion-worker/references/conflicts-and-rewrites.md`) and records each other check with no `ci_check` at the new head unrun, its `--observed` naming the earlier pass and why the merge misses it. A class search that finds an instance, or a parent's last round, takes a full test round.
- The implementer runs `skill://simplify-settled-diff` once per pull request, in its first implementing round, after its own results and before it completes, whatever the diff touches, a docs-only diff included. Applied: rerun the fast local checks and the issue's checks the simplify diff touches, and record them at the applied head. A defect the pass reports is fixed outside the pass and the results recorded again, never dropped as out of scope; the `Thermo` line names the count and the fixing commit.
- The reviewer's pair runs at the head each round reviews, over the diff that round reads, when that diff touches runtime code; the `Thermo` line records the range it read and a row per half, and earlier rounds' rows stay beside them.

## Never waiting on CI

- A red check that failed on its own is re-run once with `gh run rerun <run-id> --failed`, and its settlement waited for as `skill://envoy` says, never by sleeping. A merge-group run is never re-run.
- A required check missing at the head: read `gh pr view <n> --json mergeStateStatus` first. `DIRTY` means the head conflicts with its base, so no `pull_request` run starts and a body edit re-judges nothing: merge the base forward. A green `pull_request_target` run beside it proves nothing.

## When no surface reaches the changed path

Report it to the architect with `envoy_publish`, naming the missing surface and what it would have to do (a rig, a sandbox holding the resource, a credential, a command), and do not complete. The architect builds it and resumes you. A code path that first runs after the merge (a deploy workflow's inline step, a post-merge helper, a production-only resource) is an `after_merge` check instead, recorded after the merge by the role the project's `production_check` names (under `off` nothing records it), unless the implementer can run it against a development stack before then.

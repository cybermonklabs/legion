# PR body, recorded results, and the simplify pass

Read it before you edit the pull request body, record or read a check's result, or run the simplify pass. Every path it cites is in cybermonklabs/legion.

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

**Thermo:** `simplify-settled-diff` once at <head-sha>: <0 applied | applied → new head <sha>>; defects reported: <none | <n>, fixed in <sha>>; thermonuclear pair at the reviewed head <sha> over <the whole change | <last-reviewed-sha>..<sha>>:
<verdict>. (on a docs-only PR the simplify half and the defects are recorded as on any other diff, and the pair half reads `thermonuclear pair: not run (docs-only)`) (in a later round whose difference touches no runtime code, the pair half reads `thermonuclear pair at the reviewed head <sha>: not run (<last-reviewed-sha>..<sha> touches no runtime code)`, and the pair half of the last round the pair ran in stays beside it)

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

The implementer keeps `Outcome`, `Why`, `Change`, `Proven by` and `Size` true after every push. At each round the reviewer writes `Look at first`, and `Not proven / risk`, copying word for word every claim recorded as unproven (the tester's `failures`, its own review, a proof-check comment). A requirement there is unmet, unless only the deploy can show it and the plan names its observation. The merger reads `Outcome` and `Not proven / risk` from the body at the head it merges, and a stale `Outcome` that no longer describes the diff is a finding against the implementer, not a line the merger rewrites.

## What a result is

- **A check** is a `check` block of the issue's Verification document: what it `serves`, its `surface` (`browser`, `cli` or `api`), its `run`, its `when` (`before_merge`, `after_merge` or `default_branch`) and its `negative_control`. The architect writes the checks; nothing else decides what must be proved. One carrying `ci_check` is CI's (`skill://legion-worker/references/ci-checks.md`).
- **A result** is one run of one check: `dispatch result --issue <issue> --check <block id> --role <your role> --verdict pass --commit <sha>`, with `--surface`, `--run`, `--observed` and `--negative-control`. These refuse a bare `-`: use `--run-file` and its kin, one reading stdin per call. Put the printed line into the body unchanged.
- The surface is production-like (the real-process test harness and fixtures, a sandbox repository, a real browser, a development stack, staging, or a local stack with real migrations) and has the resource the change touches; `--run` or `--observed` links to that run, screenshot or e2e. Human review does not replace user-facing verification, and a green unit suite is not it. A unit or integration test is a regression lock, never a result, except as the job a `ci_check` names.
- Record a result only after its commit is pushed (`400 RESULT_COMMIT_UNKNOWN`).
- **A check that cannot fail is not a result.** The negative control shows it failing on a deliberately wrong input. A documented procedure counts only when it ran: reading it is not a run, and a procedure no surface available to you can run is reported under *When no surface reaches the changed path* below, never passed.
- A `before_merge` result stands while its commit is the head of an open pull request Dispatch attributes to the check's issue (`packages/envoy/internal/dispatch/delivery/attribution.go`), or the final head of a merged one, and while no part of the check's block has changed. So after a push, record each check with no `ci_check` again.
- `dispatch results --issue <issue>` prints each check with the results recorded against it and the commit each one was recorded at; `dispatch read --issue <issue>` names each requirement of the spec no check serves.
- Read `skill://legion-worker/references/pr-body-standing.md` when you judge whether a result stands: an `after_merge` or `default_branch` result, a result at none of the compared commits, or a child's result against a root's check.

## The rules every phase's evidence follows

Read `skill://legion-worker/references/pr-body-evidence.md` before you test a change, record the checks after a forward merge, run the simplify pass, or run the reviewer's pair.

## Never waiting on CI

Read `skill://legion-worker/references/pr-body-ci.md` when a check is red at the head of your pull request, a merge-group run fails, or a required check is missing at the head: it says which red is re-run, which is fixed and how its settlement is waited for.

## When no surface reaches the changed path

Report it to the architect with `envoy_publish`, naming the missing surface and what it would have to do (a rig, a sandbox holding the resource, a credential, a command), and do not complete. A check with no surface yet is this report, never a check left unrecorded. The architect builds it and resumes you. A code path that first runs after the merge (a deploy workflow's inline step, a post-merge helper, a production-only resource) is an `after_merge` check instead, recorded once the merge reaches production, on a look of the production check, by the role the project's `production_check` names (under `off` nothing records it), unless the implementer can run it against a development stack before then. It is no reason to leave a `before_merge` check unrecorded.

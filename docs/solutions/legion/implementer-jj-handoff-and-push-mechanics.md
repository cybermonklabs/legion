---
title: "Legion implementer jj handoff and push mechanics"
category: legion
tags:
  - legion
  - jj
  - handoff
  - workflow
  - implementer
date: 2026-09-24
status: active
module: legion-worker
related_issues:
  - "S392218424-1"
  - "sjawhar/legion-smoke#140"
---

# Legion implementer jj handoff and push mechanics

Reusable mechanics a Legion implementer needs when driving a per-issue jj workspace through
implement and review-correction rounds. Derived from S392218424-1 (a Stage 3 workflow proof
that appended marker lines to `README.md` only) and confirmed against the recorded
`.legion/{plan,implement,test}.json` handoffs on PR #140.

## Write the handoff on a fresh child, never on `@ == product commit`

`legion handoff write` writes `.legion/<phase>.json` into the current working-copy commit. If
`@` is still the product commit you already pushed, jj re-dirties and **rewrites that commit,
giving it a new SHA** — invalidating the product SHA cited in the PR body and the CI runs that
already validated it.

Do this instead:

```bash
jj -R "$LEGION_WORKSPACE" new                       # empty child of the pushed product commit
legion handoff write --phase implement --data '…'   # lands in the child, product SHA preserved
jj -R "$LEGION_WORKSPACE" describe -m "implement: record handoff"
jj -R "$LEGION_WORKSPACE" bookmark set legion/<KEY> -r @
jj -R "$LEGION_WORKSPACE" git push --bookmark legion/<KEY>
```

## Never `jj split` an already-pushed commit

`jj split` re-times both resulting commits (fresh timestamps) and assigns **new SHAs to both
halves**, invalidating any SHA already pushed, cited in a handoff, or referenced by a CI run /
PR-body Verification line. On S392218424-1 a split rewrote a pushed README commit
(`a21173dc → 376a9435`), forcing a re-push and re-citation of CI. Build the product commit and
the handoff commit as **distinct commits up front** (`jj new` between them); do not split after
pushing.

## Cite the stable product commit in the handoff, not the moving branch head

A handoff commit cannot cite its own SHA (it does not exist until the write completes), and the
branch head keeps advancing as later handoff commits stack. Record the **product commit SHA** as
the durable anchor. On S392218424-1, `implement.json` `headSha`/`productCommit` both point at the
product commit `e8acad7e`, while the pushed head at test time was `c344328f`
(`test.json.headSha`) — the head is ephemeral, the product commit is the anchor.

## The tester's handoff commit rides the implementer's next push

The review/test GitHub App (`legion-reviewer[bot]`) has no `contents:write`, so a
`test: record handoff` commit **cannot be pushed by the tester** — it is committed atop the
implementer's product commit and only reaches origin on the implementer's next
`jj git push`. Signature: `test.json.headSha` = implementer product commit + the tester handoff
commit on top. Preserve this trailing commit — never `jj abandon`/rebase it away — and push it.

## `.legion/` handoffs legitimately appear in the PR diff

`gh pr diff <n>` shows `.legion/plan.json`, `.legion/implement.json`, `.legion/test.json` through
every round; they are Legion process artifacts, not product changes, and are only removed by the
final pre-merge `.legion/` deletion push. The acceptance fingerprint excludes them with the jj
revset `~(.legion | docs/solutions)`:

```bash
jj -R "$LEGION_WORKSPACE" diff --from main@origin --to <product-sha> \
  --name-only '~(.legion | docs/solutions)' | grep -v '^README.md$'   # empty => exactly-one-file
```

Note this must be **one** fileset expression: jj unions positional filesets, so two separate
`'~.legion' '~docs/solutions'` arguments select everything and exclude nothing.

## Prove text-append changes on the PR diff surface, not a unit suite

For a docs/text append there is no build or unit suite. The proof is the GitHub PR diff:
`gh pr diff <n> --name-only && gh pr diff <n>`, filtered by the product fingerprint, plus a
`grep -c` on the exact marker and a **negative control** grep for a wrong marker returning `0`.
CI to expect on this repo: `ci.yml` job `check` (sleep 20 then exit 0) and `fail-on-demand.yml`
job `gate` (passes unless a `.fail-me` file exists — never add one). Cite run ids at the head
carrying the product commit.

## Per-round appends stay in the same file and issue

Each review-correction round appends one more marker line to the **same** `README.md`; the spec
(`plan.json` Requirements row 2) treats this as in-scope — do not open a new issue per round, keep
all prior markers (append-only, zero deletions). The final head must show every round's marker,
matching `implement.json.markerLines` exactly.

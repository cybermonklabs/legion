---
title: "Legion durable-workflow: multi-round review, split commits, and stable proof SHAs"
category: legion
tags:
  - legion-worker
  - implementer
  - jj
  - review-rounds
  - e2e-proof
date: 2026-09-27
status: active
module: legion-workflow
related_issues:
  - "S390606688-1"
  - "sjawhar/legion-smoke#367"
---

# Legion durable-workflow: multi-round review, split commits, and stable proof SHAs

Learnings from PR #367 (issue S390606688-1), a Stage 3 durable-workflow proof that ran the full
plan -> implement -> test -> review -> retro pipeline through three `CHANGES_REQUESTED` rounds and
a final `APPROVE`. The product change was one docs fixture (`smoke/S390606688-1.md`, one line per
round); the value is the pipeline mechanics, which generalize to any Legion issue.

## Keep the product commit and the `.legion/` handoff in separate commits

Each round is two commits: a product commit (`smoke: append review round N line`) and a distinct
handoff commit (`implement: record handoff (review round N)`). Never fold the handoff JSON into the
product commit.

Why it matters: the tester's acceptance check for "the diff touches only the product file"
(`legion gh -- pr diff <n> --name-only`) is only clean because product and ledger never co-mingle
in one hunk. The `.legion/*.json` files ride in their own commits as exempt handoff artifacts, so a
`--name-only` product view still shows exactly `{smoke/S390606688-1.md}` even though the raw
`main..@` summary lists five files (4 handoffs + 1 product).

Mechanic (jj): write the file, `jj describe` the product commit, then `jj new` an empty working
copy on top before `handoff_write`, so the handoff lands in its own commit above the product
commit. Bookmark the handoff commit (`-r @-`) as the branch tip.

## Cite a STABLE commit SHA in the E2E proof, not the mutating tip

The `E2E (implementer)` proof's `headSha` must be the commit that actually carries the product
change (here `af0d3b0c` for round 3), never `@` or the branch tip.

Failed approach worth avoiding (hit in round 1 of this issue): the first handoff cited the
pre-split working-copy SHA `e3b7f1b5`. `jj split` then carved the product change into a new commit
(`309aeff0`) and rewrote `e3b7f1b5` out of existence — it was never pushed, so the cited SHA was
unresolvable on GitHub. Fix applied: re-cite the stable product commit SHA and `jj squash` the
corrected handoff into place.

There is also a circular-dependency trap: if you embed the branch *tip* SHA into
`.legion/implement.json`, editing that file changes the handoff commit's own SHA — so the tip you
cited no longer exists. Citing the *product* commit SHA sidesteps this: adding/editing the handoff
commit on top never changes the product commit's SHA.

Rule: `proof.headSha` = the SHA of the product commit, resolvable on the pushed branch and
invariant under later handoff splits/squashes.

## Advance the bookmark forward-only across rounds

Reviewed heads advance monotonically
(`18ec987d -> 475b27e6 -> 794bf5f1 -> 19548e5f` here); each round appends new commits and pushes
the bookmark *forward* rather than rewinding history. This is a plain forward move
(`move forward from <old> to <new>`), not a rebase — no fingerprint dance is needed because GitHub
never reports the branch conflicting. The reviewer records `reviewedHead` per round, giving an
auditable forward chain that lets CI and thread state anchor to a specific head. Reserve
`--allow-backwards` for the local post-split bookmark placement only.

## Body-only review corrections: the thread count is legitimately 0/0 — say why

All three corrections were requested in the *review body* (`CHANGES_REQUESTED` text:
"append the line ... and change nothing else"), with no inline comment threads. Consequently
`legion threads resolve --pr <n>` correctly returns `no unresolved threads`, and the fix
acknowledgements are PR *comments* (citing the fix SHA + handoff SHA), not thread replies.

Document this explicitly in the PR body's `Threads` section — e.g.
"0 resolved, 0 unresolved. Rounds 1-3 changes were requested in the review bodies (no inline
threads)." — so a merger does not misread `0 resolved` as "the reviewer never asked for anything."
When corrections live in the review body there is no machine-resolvable trail; the only linkage is
prose, so cross-check each review-body ask against the appended lines rather than treating 0/0 as
"nothing to verify."

## Re-prove acceptance every round: positive keys AND a negative control

Each round, re-run on the real surface: `grep -Fq` for every key accumulated so far (round N =>
N+1 lines, `wc -l` = N+1) plus one negative control that MUST fail. The negative control is what
makes the positive greps meaningful — it proves the check discriminates rather than always passing.

Hazard: the control used here (`grep -Fq '<key>' smoke/S393198222-1.md` -> exit 1) leans on an
unrelated sibling fixture existing and lacking the key. If that sibling is ever removed, grep exit
1 for "file not found" looks identical to "key absent." Prefer a control whose failure is intrinsic
(e.g. grep a key that cannot appear in the same file) over one that depends on a neighbor file.

## Hazard: verified head may not equal the approved head

CI and both E2E proofs were proven at `ee7f444d`, but the reviewer approved a later product-only
tip (`19548e5f`) produced by a subsequent split. For a docs-only change the product content is
identical across those heads, but in general a merger MUST confirm CI is green at the *actual
approved head*, not assume an earlier tested head's status carries forward across a post-test
rewrite.

---
title: "Legion docs-only / proof PR workflow"
category: legion
tags:
  - legion
  - docs-only
  - proof
  - review-rounds
  - e2e-proof
date: 2026-09-25
status: active
module: legion-worker
related_issues:
  - "S394135277-1"
  - "sjawhar/legion-smoke#186"
---

# Legion docs-only / proof PR workflow

Reusable learnings from delivering a docs-only, plain-text change (append marker lines to
`smoke.txt`) through the full Legion plan → implement → test → review → retro workflow on
`sjawhar/legion-smoke` (issue S394135277-1, PR #186). The change itself is trivial; the value is
the workflow discipline it exercises.

## Classify docs-only at plan time and thread it through every handoff

The plan handoff (`.legion/plan.json`) fixed the classification once — "plain text, not runtime
code" — in `concerns`, `workflowRecommendation`, and `requiredSkills`, and every downstream
handoff carried `docsOnly: true`. Because the skip decision is anchored in the plan, the
implementer and reviewer omit `ce-simplify-code` and the thermonuclear deep-review/code-quality
pair, and skip the PR body's `Thermo` line, without re-justifying it per phase. The trigger is
purely "the diff touches no runtime code"; a single source/build/config line flips it back on.

## In-scope review-round follow-ups append to the SAME file in the SAME PR

The spec's Requirements/Errors explicitly permitted one more appended line per review round. Each
round advanced the existing branch with a corrective push (a new commit on `legion/<KEY>`), added
exactly one line to `smoke.txt`, and recorded it with a handoff `round` counter and a
`reviewRoundCorrection` note. The proof criterion is phrased "AC1 (with in-scope review-round
follow-ups)" so the reviewer reads the increment as spec-scoped, not scope creep. Never spin an
anticipated, spec-declared increment into a new issue or PR.

## Each corrective round is its own implementing phase — a push alone does not finish it

The daemon re-enters `implementing` for every review round and hands you a fresh working copy.
The round ends only with the `legion` tool's `handoff_complete`; pushing the corrective commit is
not the end of the phase. Rewrite `.legion/implement.json` each round (its `proof` array is
required) and commit it as `implement: record handoff` before completing.

## Separate product diff from process artifacts with the ONE-expression fileset

Assert "no other repository content file changed" against the product diff, not the raw PR file
list, which also contains `.legion/{plan,implement,test}.json`. Use the single parenthesized
union `'~(.legion | docs/solutions)'`. jj unions *positional* filesets, so two separate
`'~.legion' '~docs/solutions'` arguments select everything and exclude nothing — it must be one
expression. Only the implementer (the code-writing App with `contents` permission) deletes
`.legion/`; every other role's handoff commit rides the implementer's next push, and approval is
valid only for a head carrying no `.legion/`.

## For a plain-text change, the production-like proof is the PR diff surface + a negative control

A green CI stub (`.github/workflows/ci.yml` here is `sleep 20 && exit 0`) is not proof. Both
implementer and tester used `legion gh -- pr view <n> --json files` (`smoke.txt` MODIFIED,
`additions:N deletions:0`) plus the jj product diff, with the negative control being
`legion gh -- api repos/<owner>/<repo>/contents/smoke.txt` on the default branch showing the
lines are ABSENT there — i.e. they arrive only via the PR. The tester re-ran the surface
independently at the final head rather than trusting the handoff. When there is no runtime to
exercise, prove the artifact is present on the PR head and absent on the merge target.

## Head-SHA discipline and small operational hazards

- **Handoff commits move the head.** Each phase's handoff commit advances the branch tip, so the
  implement handoff's `headSha` is not the reviewer-approved head. When reasoning about what was
  approved, cite the reviewer-approved head (from
  `legion gh -- api repos/<o>/<r>/pulls/<n>/reviews`), never the implement handoff's SHA. For the
  proof's own `headSha`, cite the immutable product-change (correction) commit SHA — it stays in
  branch history and the reviewer re-verifies the product diff via `pr diff` regardless of the
  current tip, which sidesteps the circularity of a handoff commit that changes the very head its
  content cites.
- **Thread state:** run `legion threads resolve --pr <n> --repo <o>/<r>` before every push that
  answers a review, even when review rounds are directed through the Legion workflow rather than
  GitHub review threads (it prints `no unresolved threads`). A raw GraphQL read submitted with
  `gh api graphql -F query=@file` was refused by the credential shim with the merge-guard
  message; for a raw read use `-f query=@file` (untyped string field), but prefer
  `legion threads resolve` for thread disposition.
- **Cite exact CI job names.** Reference only jobs the workflow actually defines
  (`ci`'s `check` job here). A proof line that names a `gate`/job absent from the workflow is a
  proof-hygiene defect.
- **Retro ordering:** retro is meant to run after the `.legion/` deletion and the reviewer's
  approval of the clean head. If retro runs while `.legion/` is still present (a compressed flow),
  compute the "product diff" through the fileset exclusion, add only `docs/solutions/` (never a
  `.legion` file), and report the ordering observation to the architect.

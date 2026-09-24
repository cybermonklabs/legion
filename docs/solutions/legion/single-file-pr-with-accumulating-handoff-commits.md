---
title: "Keeping a Legion PR to one product file while handoff commits accumulate"
category: legion
tags:
  - legion
  - jj
  - handoff
  - scope-check
  - proof
date: 2026-09-24
status: active
module: legion-worker
related_issues:
  - "S393721621-1"
  - "sjawhar/legion-smoke#160"
---

# Keeping a Legion PR to one product file while handoff commits accumulate

Issue S393721621-1 proved the Legion end-to-end workflow with the smallest real change:
append one line to `smoke.txt` (Acceptance 1: exactly one product file, one added line),
then land three review-requested follow-up lines on the *same* PR (#160) as in-scope work
(Acceptance 2). Across the initial implementation and three review rounds the branch grew a
stack of `plan|implement|test: record handoff` commits carrying only `.legion/<phase>.json`,
yet the PR stayed a one-product-file change throughout. The reusable lessons:

## Scope is the product diff, not the raw diff

A branch that must change "exactly one file" legitimately accumulates many `.legion/`
process-artifact commits. The scope/fingerprint gates never diff the whole tree — they
subtract process artifacts first. A scope check MUST exclude `.legion/` and
`docs/solutions/` before asserting "exactly one product file"; the exclusion is
load-bearing, because otherwise every handoff commit reads as a second changed file and
trips REQUEST_CHANGES. Prove it with a negative control: feed the gate a synthetic second
product file (e.g. `README.md`) and confirm it exits non-zero → REQUEST_CHANGES. Both the
implementer's and tester's proofs on this issue used exactly that control.

## The handoff commit is its own commit; the working copy must be empty at completion

Each round's handoff is a dedicated commit containing only `.legion/<phase>.json`, separate
from the product commit — `jj split -m "<phase>: record handoff" .legion/<phase>.json` is the
canonical mechanic. `legion handoff complete` refuses while `.legion/<phase>.json` has
uncommitted working-copy changes ("has changes in the working copy that are not committed"),
so after writing the handoff the working copy `@` must be an empty commit sitting on top of
the committed handoff. Practically: commit the product change, then create the handoff commit
on top, then leave `@` empty (`jj new`) before completing. A clean `@` is what makes
"handoff proof SHA == pushed PR head SHA" true — reviewer and merger read GitHub only, so any
uncommitted/unpushed delta is invisible to them.

## Hazard: rewriting the pushed code commit via write-then-restore

`legion handoff write` amends the current working-copy commit to include the new
`.legion/<phase>.json`. If you then `jj restore .legion/<phase>.json` to pull it back out of
an already-pushed code commit, jj re-materializes that commit with a *new committer
timestamp* — a different SHA than the one on origin — turning the next push into a "move
sideways" (non-fast-forward) that rewrites the PR head. Avoid it: never write the handoff into
the code commit's working copy and then restore. Keep the code commit frozen once pushed and
add the handoff as a child commit (`jj new` + write, or `jj split` of the named file only).

## Only the implementer App pushes; sequence the process commits before the deletion push

Tester and reviewer commits are authored by their own Apps but those Apps cannot push (git
`Repository not found`; REST `Resource not accessible by integration`). A `test: record
handoff` split therefore reaches origin only on the *implementer's* next push and rides on
top of the implementer's fast-forward. Risk: the implementer's final push is the `.legion/`
deletion — any tester/reviewer handoff commit not already pushed can be stranded. Sequence
pushes so process commits land before the deletion push, and run the mandated pre-push
identity check (`jj log -r 'main@origin..@' -T author.email()/committer.email()`) — every
commit must carry its own role's App in both columns.

## The approved head is defined by `.legion/` absence

Reviewers may only APPROVE a head carrying no `.legion/` (differing from the reviewed head by
the `.legion/` deletion alone); while `.legion/` is present the verdict is
COMMENT/REQUEST_CHANGES. Recreating `.legion/` after deletion changes the approved head and
restarts the review loop — never re-add handoff files after the deletion push. Retro writes
only under `docs/solutions/`, which is excluded from the fingerprint, so it never re-dirties
the cleaned tree or voids the approval.

## Tooling gotcha: `gh api graphql` query passed by file is refused

The worker `gh` shim classifies each call to refuse merges and non-GET issue writes. Passing
a GraphQL query via `-F query=@file` hides the operation text from the shim, which then
defaults to the merge refusal ("Legion never merges a pull request"). Pass the query inline
with `-f query='…'` (and balance the braces — a missing closing brace yields
"Expected NAME" from the GraphQL parser).

## Gap found: the tester's E2E proof must reach the surface of record

The skill is explicit that "the reviewer and the merger verify facts on GitHub and never from
a handoff." On this issue the tester's E2E verdict (`implementerProof.verdict: "verified"`
plus its own re-run at head `94c6126b`) lived only in `.legion/test.json`, while the PR body's
`E2E (tester)` line was left `pending`. A merger reading only GitHub would see an unproven
tester line. The implementer's proof is required in *both* `.legion/implement.json` and the PR
body; the tester's resolved `E2E (tester)` line should follow the same both-places rule and be
written into the PR body/comment at the named head (pushed by the implementer App, the only one
that can push) — the handoff JSON is durable state, not the merge-gate surface.

---
title: "jj handoff topology and product-diff hygiene for Legion phase workers"
category: legion
tags:
  - jj
  - legion-workflow
  - commit-topology
  - proof-discipline
  - product-diff
date: 2026-09-24
status: active
module: legion-phase-worker
related_issues:
  - "S390384725-1"
  - "sjawhar/legion-smoke#149"
---

# jj handoff topology and product-diff hygiene for Legion phase workers

Learnings from a Legion Stage 3 workflow proof (S390384725-1, PR #149): create a file with
one line, then append one line per review round across three rounds. Trivial content; the
reusable value is in the jj mechanics and verification discipline that a phase worker repeats
on every issue.

## Build the commit chain forward with `jj new`; never `split` then patch

The branch interleaves, per stage, a **code** commit followed by that stage's **handoff**
commit (`implement`, then `test`), repeated for the initial pass plus each review round. The
clean way to produce two ordered commits is forward-only:

1. `jj new <base>`; write the code change; `jj describe -m "<code msg>"`.
2. Capture the code commit's **full SHA** now — it is stable and is what the handoff's `proof`
   should cite.
3. `jj new`; write the handoff file; `jj describe -m "<phase>: record handoff"`.

Anti-pattern that cost a rebuild here: creating the code commit, then running
`legion handoff write` (which drops the file into the **working-copy** commit), then
`jj split` to peel the handoff out. The split **reordered** the handoff below the code commit
and rewrote the code SHA; a follow-up attempt to correct the handoff file wrote it into `@`
while `@` was the *code* commit, adding the handoff to the wrong commit and conflicting the
descendant. Recovery was a clean `jj new <plan-handoff>` rebuild. Prefer the forward recipe;
after any structural op, re-read `jj status` and `jj log` before continuing.

## AC "exactly one file, +N/-0" is a claim about the *product* diff, not the raw diff

Every Legion branch carries `.legion/*.json` handoff artifacts, so an unscoped
`jj diff --summary` or `gh pr view --json files` lists `.legion/plan.json`,
`.legion/implement.json`, `.legion/test.json` **plus** the product file. Verify the product
claim against the fileset the whole workflow uses:

```
jj diff --from main@origin --to @ --stat '~(.legion | docs/solutions)'
# or, on the PR files view:
gh pr view <n> --json files --jq '[.files[] | select(.path|startswith(".legion/")|not)]'
```

This is the same `~(.legion | docs/solutions)` fileset as the rebase unchanged-diff
fingerprint and the merger's `--summary` check, so scoping to it keeps AC1, the fingerprint,
and the merge gate mutually consistent.

## Proof + negative control for a change with no unit surface

A plain-text append has no unit/integration surface; the production-like surface is the PR
Files-changed view and the contents API. Make the proof falsifiable by running the AC
assertion as an executable predicate and showing it **fails** on deliberately-broken input:

- PASS: the actual PR diff (single product file, `deletions == 0`, requested line last).
- FAIL: a diff touching a second file `[{smoke.txt},{other.txt}]`.
- FAIL: a diff carrying a deletion `[{smoke.txt +N/-1}]`.

The tester independently re-drives the same surface and re-runs the same PASS/FAIL triplet at
its own head SHA, rather than trusting the implementer's handoff.

## Review-round corrections land on the same PR/file, as a new commit on top

The plan pre-authorized a one-more-line allowance per review round. Each round's line is a new
code commit stacked above the prior tester handoff (never a new issue, never an edit of an
earlier line), so `deletions` stays 0 and the product diff simply grows `+1` per round.

## Two completion-gate gotchas

- `legion handoff write` rejects a `phase` key inside `--data` (the flag sets it) and, for
  `implement`/`test`, rejects a payload missing a well-formed `proof` array — cite a **live**
  SHA in `proof.headSha`; a SHA rewritten by a later `split`/`rebase` becomes a dead reference.
- `legion handoff complete` refuses while the handoff file has uncommitted working-copy
  changes. Because jj's `@` is always "uncommitted", add a trailing empty `jj new` so the
  handoff sits in a committed parent, then complete.

## Retro sits above the approved head without voiding approval

Retro's only durable outputs are this `docs/solutions/` document and the issue's Dispatch
message; retro writes no `.legion` file (`legion handoff write --phase retro` is not a valid
phase and returns usage). Placing the docs commit directly on the reviewer-approved head keeps
the merger's `jj diff --from <approved> --to <tip> '~docs/solutions'` check empty, so the
approval stands and the tree advances straight to the merger.

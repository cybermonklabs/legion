---
title: "Multi-round Legion implementer PRs in a shared jj workspace"
category: legion
tags:
  - legion
  - jj
  - implementer
  - handoff
  - proof
date: 2026-09-24
status: active
module: legion-worker
related_issues:
  - "S393883308-1"
  - "sjawhar/legion-smoke#172"
---

# Multi-round Legion implementer PRs in a shared jj workspace

Reusable learnings from delivering a Stage 3 smoke PR (`smoke.md`: one marker line
plus three append-only review-round lines) through the Legion implementer phase in a
shared `jj` workspace. The mechanics below cost real round-trips the first time; they
are the fast path next time.

## Keep the product change and the `.legion` handoff in separate commits — and add the handoff *on top*

Put the product change (here `smoke.md`) in its own commit, and the `.legion/<phase>.json`
handoff in a **separate commit stacked above it**. That keeps the product commit's entire
diff equal to the reviewed content, so a proof can name a commit whose content is
unambiguous while plan/implement/test JSON ride above and never pollute the content hunk.
The unchanged-diff fingerprint already exempts `.legion` and `docs/solutions`, so a handoff
commit on top never changes the product fingerprint.

Add the handoff with `jj new` (a fresh child) then `handoff_write`, **not** by amending the
product commit or by `jj split` after the fact:

- `jj split <product-file>` on a commit that already contains both files **re-mints the
  product commit SHA** (new committer timestamp) even though its tree is identical. In this
  issue the first smoke commit went `87026f03 → f8edd3d4` purely from a post-hoc split,
  invalidating the head SHA already cited in the proof and forcing a re-verify. Sequence the
  other way: commit + push the product change first, then `jj new` and write the handoff on
  top. The product commit SHA then stays stable for the rest of the phase.

## `handoff_complete` refuses a dirty working copy — end every phase with `jj new`

`legion handoff_complete` fails with `.legion/<phase>.json has changes in the working copy
that are not committed` whenever `@` still carries the handoff as working-copy changes. After
`handoff_write` → verify → `jj split`/`describe` the handoff commit → `bookmark set` → push,
run `jj new` so `@` is a clean empty commit above the committed handoff. Only then does
`handoff_complete` succeed.

## `handoff_write` data is the phase fields only

The `legion` tool's `handoff_write` rejects `schemaVersion` and `phase` inside `data`
(`handoff data field schemaVersion is not allowed`). Pass only the phase-specific fields;
the tool owns the envelope.

## The product SHA changes every review round — re-capture the proof each round

An append-only one-line change still rewrites the commit, so the product SHA churns per
round (`8ac8574a → c754a2d7 → e247783` here). There is no single stable PR head SHA; it is
stable only within a round. Re-capture `headSha` and re-run the `pr diff` proof after **every**
round rather than reusing an earlier round's SHA. In the handoff, pin `proof[].headSha` to the
**product** commit and say so — the git/PR tip is the handoff commit above it and legitimately
differs.

## Prove append-only structurally, and run the negative control for real

The proof that survives multiple rounds is structural, not narrative:

- `smoke.md added=N removed=0` — `removed=0` is what proves no earlier line was modified.
- `pr view --json files` filtered to content files == `[smoke.md]` only, treating
  `.legion/*.json` as exempt process handoffs.

Exercise the negative control against a known-bad input before asserting the real one passes:
feed the scope guard a forbidden `README.md` and confirm it exits 1; feed the removed-line
detector a synthetic tampered hunk (`-<original line>`) and confirm it fires. A proof with a
live negative control has visible teeth.

## Docs-only PRs: skip thermo/ce-simplify; Dispatch-driven reviews leave GitHub threads empty

On a text/docs-only diff, `ce-simplify-code` and the thermonuclear pair are omitted
(legion-worker docs-only rule) — record that omission in the `Thermo` line rather than
running them. When review rounds arrive via Dispatch (not GitHub review threads),
`legion threads resolve --pr <n>` is an expected no-op (`no unresolved threads`, exit 0) each
round; do not wait on GitHub thread activity. Each Dispatch-requested append is an in-scope
round of the **same** PR, not a new PR.

## The implementer push carries other roles' commits — verify identity per commit, not per list

Between implementer rounds the tester (running as the review App) adds a `test: record
handoff` commit that it cannot push. The implementer's next push carries those commits along.
When checking authorship before a push, verify **your own** commits are authored by
`legion-implementer[bot]`; earlier phases' commits authored by `legion-reviewer[bot]` are
legitimate and expected on the same list.

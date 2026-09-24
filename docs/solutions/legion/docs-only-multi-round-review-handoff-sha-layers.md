---
title: "Legion docs-only multi-round review: handoff SHA layers, tester-proof sinks, and the clean-head gap"
category: legion
tags:
  - legion-worker
  - retro
  - pr-body
  - handoff
  - review-rounds
  - jj
date: 2026-09-24
status: active
module: legion-worker
related_issues:
  - "S391388879-1"
  - "sjawhar/legion-smoke#135"
---

# Legion docs-only multi-round review: handoff SHA layers, tester-proof sinks, and the clean-head gap

Context: S391388879-1 was a docs-only workflow-proof issue — append four lines to
`README.md` across an initial implementation plus three review-round corrections — that
exercised the full Legion phase choreography (plan → implement → test → three review rounds →
retro). It surfaced three reusable pitfalls that apply to any Legion docs-only or
multi-round-review issue, independent of this particular content.

## 1. A "verified" `.legion/test.json` does not mean the PR body carries the tester proof

The tester proof has **two distinct sinks**, and they are written separately:

- `.legion/test.json` → `implementerProof {verdict, how}` plus the tester's own `proof[]` array.
- The **PR body** `E2E (tester)` line (command/run id, observation, head SHA, one negative control).

On this PR, `.legion/test.json` recorded `implementerProof.verdict: "verified"` with a complete
proof entry at head `49478afb`, yet the live PR body's `E2E (tester)` line still read
`pending tester re-verification of round 3`. The verified handoff proves the tester did the
work; it does **not** prove the PR body was updated. The reviewer and merger read GitHub, not the
handoff JSON, so a `pending` line on the body is what actually gates them.

On a docs-only PR only the `Thermo` line is omitted — **both** `E2E (implementer)` and
`E2E (tester)` remain mandatory, and approval must be refused while either is missing. When the
review App (tester) cannot edit the implementer-authored PR body, the implementer (PR-body owner)
must transcribe the verified tester proof from `.legion/test.json` onto the body. Retro and the
reviewer must cross-check the **live PR body** against the handoff, never accept the handoff JSON
as a proxy for the body line.

## 2. Recorded head SHAs live on different commit layers; only the product fingerprint is stable

With the handoff-rides-the-branch topology, each round produces, newest-to-oldest:
`test: record handoff` (`.legion`) → `implement: record handoff` (`.legion`) →
`docs: <product>` (README +1 line). Consequently the SHAs quoted in the handoffs point at
**different layers**:

- `implement.json.headSha` = the round's `docs:` **product** commit (e.g. `1174b739`).
- `test.json.headSha` = the following `.legion` handoff commit (e.g. `49478afb`).
- the actual branch **tip** = a further `test: record handoff` commit.

Three SHAs, three layers. Before trusting a quoted "head SHA", reconcile which layer it names.
The only identity that is stable across all three is the **product fingerprint** over
`'~(.legion | docs/solutions)'` (`d34420e4…` at the round-3 head). Compare that fingerprint —
not a raw tip SHA — when reasoning about whether the product actually changed. This is also what
lets each role compare against its own last verified head rather than trusting another role's
numbers.

Corollary for the implementer's own proof: never cite the handoff commit's **own** SHA inside the
handoff it commits — amending the handoff changes that SHA (self-referential drift). Cite the
stable `docs:` product commit (an ancestor) in the `proof[].headSha`.

## 3. The approvable clean head (no `.legion/`) is never the natural tip — schedule the deletion explicitly

Because every round appends one product commit and two `.legion` handoff commits, the branch tip
is always a `.legion`-carrying commit. `COMMENT`/`REQUEST_CHANGES` are correct while the head
carries `.legion`; `APPROVE` is only valid for a head carrying **no** `.legion/`. So the
`.legion/` deletion commit plus a fingerprint-equality re-check against the approved head is an
**explicit terminal step** of the round, not something the natural tip satisfies. A docs-only PR
that stops at a `.legion`-carrying tip is not yet mergeable no matter how many review rounds
passed. (In this run the reviewer approved a `.legion`-carrying head directly, which is why this
step deserves calling out.)

## Incidental: avoid the round-0 commit reorder

Round 0 wrote the implement handoff *after* the first push, then `jj split` placed the handoff
commit below the product commit; reordering it back with `jj rebase` changed committer
timestamps and therefore the product SHA, forcing a re-cite and a non-fast-forward push. Rounds
1–3 avoided this by writing the correction commit, then creating the handoff commit **on top**
with `jj new` + `jj describe` (no split, no reorder) — a clean fast-forward each time. Prefer that
order: product commit first, handoff commit stacked on top, cite the stable product SHA.

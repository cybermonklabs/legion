---
title: "A contract number's prose is history or current: write history as the released number, current as a pointer and a new entry by its id, and sweep docs/solutions too"
category: legion
tags:
  - daemon-api-contract
  - forward-merge
  - docs-solutions
  - unchanged-diff-fingerprint
  - pi-legion
  - review-round
date: 2026-10-08
status: active
module: packages/daemon
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1846"
  - "INFRA-1569"
---

# A contract number's prose is history or current: write history as the released number, current as a pointer and a new entry by its id, and sweep docs/solutions too

Rewritten for dispatch://INFRA-1569. Before it a branch picked its own contract number, and this
note also listed the files that declared the number and how to sweep them at a renumber. Since it,
no committed file declares a number above 275: a contract change adds an entry with
`bash scripts/daemon-contract-entry.sh <slug>`, and a build's number is 275 plus its count of
entries, each numbered by its place in `main`'s merge order (`CONTRIBUTING.md`).

- Classify every sentence that names a daemon API contract number. **History** (a literal, true
  forever): a released number, as `packages/daemon/internal/api/version.go`'s numbered entries,
  a `CHANGELOG.md` entry about the release that shipped it, or a runbook sentence about that
  release give it. **Current** (what the gate wants, what an operator installs, what a comment
  calls "this shape"): never a literal. Write it as a pointer, the number a build's
  `legion version --contracts` prints (or, in code, `daemonContract()` of
  `packages/contracts/src/daemon-contract.ts`), which stays true at every merge and is checkable
  where a literal is not. **A new entry**: its id, as its file `contract_<id>_<slug>.go` names it,
  never a number, since its number is its place in `main`'s merge order, which no source line
  holds.
- A sentence that enumerates a transition ("15 for the shared issue pod, 16 once the capability report
  lands") is a literal twice and a trap for the reader who takes the first number. Name the entries.
- `docs/solutions/**` is in the sweep. The unchanged-diff fingerprint's fileset
  (`'~(.legion | docs/solutions)'`, `skill://legion-worker/references/conflicts-and-rewrites.md`)
  excludes it, so a retro note from an earlier round that states a current value has no diff, no CI
  job and no fingerprint change to show it — only a reviewer's grep. A retro note writes a number as
  history ("at the release that shipped it") or as a pointer, never as current.

## Evidence

sjawhar/legion#1846 moved from 15 to 16 in its forward merge of `main` (38e27ad3) after
`@sjawhar/pi-legion@8.3.0` shipped 15 without `capabilities`. The implementer swept by hand
(`CHANGELOG.md`, `packages/pi-legion/AGENTS.md`, `docs/kubernetes.md`) and the review at 256a856b
found five current-state sentences still at 15: the plugin's npm README ("currently 15"), the
LEGION-462 upgrade runbook — where the sweep had *written* the two-number sentence — the forward
clause of `main`'s own LEGION-462 CHANGELOG entry ("beside a daemon at 15"), and two retro notes from
the first retro (b855a3d4), one saying "contract 15, which no release had shipped" after 8.3.0 had.
The two notes sat outside the fingerprint's fileset; the other three were inside it. f98f84e7
replaced each with a pointer, keeping `main`'s LEGION-462 entry opening and its 15 verbatim as
history; the tester's check was `test -f` on each pointed-at path. The cost of the miss was a whole
review round for no behaviour change — implementer f98f84e7, a tester round, review 5463772787 →
the re-approval 5463972815 — and that round was the root's third rejection, the daemon's
`review_round_cap`.

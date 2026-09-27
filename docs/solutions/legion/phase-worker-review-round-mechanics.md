---
title: "Legion phase-worker mechanics across multi-round reviews"
category: legion
tags:
  - legion
  - phase-worker
  - implementer
  - review-rounds
  - jj
  - ci-citation
date: 2026-09-27
status: active
module: legion
related_issues:
  - "S393198222-1"
  - "sjawhar/legion-smoke#363"
---

# Legion phase-worker mechanics across multi-round reviews

Distilled from Legion issue S393198222-1 / PR sjawhar/legion-smoke#363, a Stage 3
workflow proof. The product change is trivial (one docs-only fixture,
`smoke/S393198222-1.md`, one appended line per review round). The reusable value is in the
phase-worker mechanics, not the file.

## Context

Each Legion phase (plan → implement → test → review) both writes a JSON record into
`.legion/` **and** commits it to the PR branch, so the branch head advances once per phase
on top of the product commit. On this PR the heads diverged:

- product-code head (round 3 append): `140a93d8`
- test-record head: `e493dc31`
- review/approve head: `afc235255752`

## Learnings

1. **"Product-code head" and "PR head" are distinct SHAs; re-derive the head every time.**
   Because every phase commits its own `.legion/*.json`, the head a prior phase called
   "current" is already stale by the next phase. Cite the product-code head where you
   actually verified behavior (the `E2E` line) separately from the CI head. Never copy a
   previous phase's "current head" — re-run `jj log -r legion/<KEY>` first.

2. **Push, then re-cite CI (`ci`/`check`) at the exact new head.** Every append and every
   handoff commit is a push that moves the head and triggers a fresh `ci` run. After each
   push, wait for `check` at the *new* SHA and cite that run id. This PR legitimately cites
   two runs — one at the product-code head and one at the handoff head — because they are
   different objects. Watch the run with `legion gh -- run watch <id> --exit-status`, which
   redeems the credential once and survives past the 60s grant window.

3. **Structure each round as two split commits.** For every correction round: put the
   product edit in its own commit (`jj split -m "<change>" <paths>`), then write the phase
   handoff and split it into a *separate* commit (`jj split -m "<phase>: record handoff"
   .legion/<phase>.json`). Push each with the standard `-r @-` push procedure. This keeps
   the product diff reviewable in isolation from the handoff churn.

4. **`legion threads resolve` is a documented no-op on top-level reviews.** All three
   `CHANGES_REQUESTED` reviews here were top-level PR comments with zero inline threads, so
   `legion threads resolve --pr 363` reported `no unresolved threads` (0/0). Do not assume
   `CHANGES_REQUESTED` implies resolvable inline threads — still run the command before each
   review-answering push, paste its output, and map each round to its fix commit in the PR
   body's `Threads` section.

5. **Classify the diff first; a docs-only change legitimately drops gates.** A pure
   Markdown/fixture change has no runtime code, so `ce-simplify-code`, the thermonuclear
   pair, and any red regression test are all skipped (the repo's `ci.yml` has no
   lint/typecheck/test job regardless). Proof reduces to CI `check` green plus a `cat` E2E
   with a negative control (`cat <missing>` → exit 1). Record the skip explicitly in the
   `Thermo` line rather than silently omitting it.

6. **`envoy_publish` takes `message`, not `body`.** Reporting completion to the architect
   role uses `{topic, message}`; a `body` field is rejected. (The `dispatch_*` tools use
   `body`; the two are easy to conflate.)

## Fragility observed (not fixed here)

- **`.legion/` leaks to `main` unless the merger strips it.** At retro the whole `.legion/`
  tree (`plan/implement/test/review.json`) is still committed at the branch head. Its
  removal is the implementer's `.legion/` deletion push at the reviewer's direction followed
  by the merger's `--summary` check — a manual, CI-unenforced step. If skipped, the handoff
  records ship to `main`.
- **The PR body's "current head" goes self-stale.** The `Verification` block advertised
  `e493dc31` while the approving review was at `afc235255752`; the approval SHA is never
  reflected back into the body.
- **Plan A2 under-modeled reality.** A2 was written as a single conditional two-line end
  state, but the smoke drove three rounds / four lines. A check keyed on A2's literal "two
  lines" would false-fail; when reality diverges from a criterion, update the criterion text
  rather than only asserting "instruction takes precedence".
- **`fail-on-demand.yml` also runs on every push** and passes only because no `.fail-me`
  file exists — a second, unguarded check a worker reasoning "CI is just `sleep 20`" might
  overlook.

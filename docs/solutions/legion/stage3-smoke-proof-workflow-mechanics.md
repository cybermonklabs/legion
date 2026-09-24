---
title: "Legion Stage 3 smoke-proof workflow mechanics"
category: legion
tags:
  - legion-workflow
  - handoff-commits
  - negative-control
  - jj
  - review-rounds
date: 2026-09-24
status: active
module: legion-phase-workers
related_issues:
  - "S390383979-1"
  - "sjawhar/legion-smoke#130"
---

# Legion Stage 3 smoke-proof workflow mechanics

Reusable learnings from running the full Legion phase pipeline (plan → implement →
test → three review rounds → retro) on a single marker file, PR
sjawhar/legion-smoke#130. The change itself was trivial; the durable value is in the
handoff and verification mechanics, which apply to any Legion issue.

## 1. Keep the code commit and the handoff commit as distinct commits

Every product commit (`fa138e5a`, `fb04f96c`, `c659feef`, `3ca1ef5d`) is followed by a
separate `implement: record handoff` commit that touches only `.legion/*.json`. Do not
let them merge.

Hazard actually hit in review round 1: after pushing the correction commit, I ran
`legion handoff write` while `@` was still the correction commit, so `@` then held both
the code change and the new `.legion/implement.json`. Recovering with
`jj restore .legion/implement.json` cleaned the file but the correction commit's SHA had
already changed (jj recomputes the commit id on any working-copy snapshot, and the
committer timestamp moved), so the pushed head SHA no longer matched the SHA my E2E
line cited. That forced a re-verify and a PR-body SHA update.

Concrete rule: **push the code commit, then `jj new` to move off it BEFORE
`legion handoff write`.** The handoff lands on a fresh child commit; you can re-describe
or re-record it without disturbing the code commit's SHA. `legion handoff complete` also
refuses to run while the handoff file has uncommitted working-copy changes, so after the
`describe`/`split` you need one more `jj new` (an empty working copy) before completing.

## 2. Prove the gate is live with a throwaway `.fail-me` scratch branch

A green gate at the PR head proves nothing if the gate can never fail. `fail-on-demand`
(`.github/workflows/fail-on-demand.yml`) exits 1 only when a `.fail-me` file is present.

Negative control used here: push a throwaway branch
(`legion-scratch-negctl-s390383979-1`) off `main` carrying a `.fail-me` file, observe
`fail-on-demand/gate` run 35949603311 conclude `failure`, then delete the branch and push
the deletion. Record the branch name, its head SHA (`4b629239…`), and the failing run id
in the handoff `negativeControl` field and the PR `E2E` line. The workflow triggers on
`push` alone, so no PR is needed for the scratch branch — read the result with
`legion gh -- run list --branch <scratch> --workflow fail-on-demand`.

Do the negative control on a scratch branch, never on the real issue branch: it keeps the
reviewed head clean while still exercising the real GitHub Actions surface.

## 3. A review round that appends one line stays in the same file, no spec bump

The spec permitted one appended line per review round. Each round appended exactly one
line to `smoke/stage3-proof.txt` in the same PR — no new file, no new spec version
(`designGate.approvedVersion` stayed 2). Verify scope with `pr diff`: only the one
product file (plus `.legion/` ledger artifacts) should appear. A corrective push runs
`legion threads resolve --pr <n> --repo <owner>/<repo>` first even when there are no
GitHub review threads (a spec-permitted append is not a thread) — it prints
`no unresolved threads` and is a no-op, which is the expected, compliant result.

## 4. Both proofs must exist before merge

The PR must carry an `E2E (implementer)` line and the tester must record
`implementerProof.verdict: "verified"` in `.legion/test.json` with its own `proof`
array. Retro reads both; a missing or unit-only proof is retro's first durable learning
and sends the issue back. On this PR both were present at head `b6782275`.

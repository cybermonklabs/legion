---
title: "Docs-only multi-round Legion proof PRs: per-round cadence and counting proofs"
category: legion
tags:
  - legion-workflow
  - docs-only-pr
  - proof-discipline
  - jj
  - ci
date: 2026-09-25
status: active
module: legion-worker
related_issues:
  - "S393145251-1"
  - "sjawhar/legion-smoke#306"
---

# Docs-only multi-round Legion proof PRs

A docs-only change (here: appending spec-permitted lines to `SMOKE.md`) still runs the full
Legion pipeline — plan → implement → test → review (×3 rounds) → retro. The change carried no
runtime code, so `ce-simplify-code` and the thermonuclear pair were correctly skipped, and the
`Thermo` PR-body line was omitted. What follows is what a future worker driving a docs-only
multi-round proof PR should reuse.

## One line, one commit, one head, one push — per round

Each round (initial implement plus each review round) appends exactly one spec-permitted line,
produces its own commit and its own pushed head, and is proven green at that head before the
round's handoff is written:

| Round | Product commit | Handoff head |
| --- | --- | --- |
| 0 (implement) | `068f792e` | `7a8f7432` |
| 1 (review) | `989308b0` | `d10d8785` |
| 2 (review) | `4504012a` | `0bdde346` |
| 3 (review, final) | `794ee89f` | `3d68f8c2` |

Do not batch the review-round lines or backfill them: each review round must produce its own
commit + push + green CI so the history shows the cadence and each round is independently
provable at a named SHA. Record the head SHA and both CI run/job ids in that round's proof row
as it happens; reconstructing them later is lossy.

## Two pushes per round means CI runs twice per round

The observed cadence per round was: (1) `jj describe` the corrective commit, `jj new`, push the
bookmark to it (CI fires); (2) `legion handoff write` → `jj split -m "implement: record handoff"
.legion/implement.json`, push the bookmark to the handoff commit (CI fires again). Both
workflows (`ci`/`check` = `sleep 20 && exit 0`; `fail-on-demand`/`gate`) trigger on
`[push, pull_request]`, so a docs-only round still costs ~2×(20s check + gate). This is by
design; wait for green at each head rather than treating the re-run as noise. The handoff commit
changes only `.legion/*.json` and never the product file, so the product proof head stays valid
across it — cite the product head in `E2E`, and note it is unchanged at the handoff head.

## Prove a markdown diff as a counting invariant with a live negative control

There is no runtime behaviour to assert, so proof is a line-count invariant: "exactly N
spec-permitted added lines to the product file, no other product file." The load-bearing part is
the negative control, which must scale with the round and discriminate the exact spec-permitted
set from one unrequested extra line — round 3's control fed a fifth line and observed
`expected 4 spec-permitted lines, got 5 -> test FAILS`. Generate the over-count input
mechanically and prove the check fails on it; a docs proof with no exercised failing case is not
a proof. The proof surface is the real GitHub PR diff (`legion gh -- pr diff <n> --repo <o>/<r>`)
plus a workspace `jj diff --from main@origin`, never a unit suite.

## Product file vs `.legion/*.json` process artifacts share the PR diff

The committed handoffs `.legion/{plan,implement,test}.json` legitimately appear in `pr diff`
alongside the single product file. Word the acceptance and the negative control on the *product*
file set and explicitly whitelist `.legion/*.json`, or the process artifacts trip a naive "no
other change" reading. State plainly that process artifacts are expected-and-permitted in the
diff. Caveat: do not assert the handoffs were "removed at end of clean review" while they are
still committed — in this run the `.legion/` deletion step did not execute before retro, and the
files remained on the approved head (`7a9241cc`). Match handoff prose to the actual tree state.

## Corrections can arrive off-GitHub; still run `legion threads resolve`

All three review corrections arrived as daemon Dispatch messages, not as GitHub review threads,
so `legion gh -- api graphql` for `reviewThreads` returned empty and `legion threads resolve
--pr <n> --repo <o>/<r>` printed `no unresolved threads` (exit 0). Run the resolve command on
every review-answering push regardless — its `Threads: 0 resolved, 0 unresolved` output is the
evidence — and do not assume threads exist just because a review round was requested.

## `gh` shim gotcha: graphql read queries need `-f`, not `-F`

`legion gh -- api graphql -F query=@file.graphql` is refused by the worker `gh` shim (file input
`-F` is treated as a mutation: it printed the "Legion never merges" refusal). Pass the query as a
string field instead — `legion gh -- api graphql -f query='query { ... }'` — which reaches GitHub
as a read. Keep GraphQL queries single-string.

## Never arm the CI failure lever accidentally

`fail-on-demand`/`gate` fails iff a `.fail-me` sentinel exists at repo root. For a docs proof it
must stay green, so never let `.fail-me` get committed into the branch or the SMOKE.md diff. If a
round ever needs a CI-level negative control (prove CI *can* fail), that file is the intended
lever — add it, observe `gate` FAIL, remove it, observe PASS — rather than corrupting the product
file.

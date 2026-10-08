---
title: "Keep one label per PR-body line across rounds: newest round first, earlier rounds demoted, Size refreshed at each pushed head"
category: legion
tags:
  - pr-body
  - proof
  - review-rounds
  - legion-implementer
  - legion-merger
date: 2026-10-08
status: active
module: legion-phase-workflow
related_issues:
  - "LEGSMOKE-526"
  - "sjawhar/legion-smoke#525"
---

# Keep one label per PR-body line across rounds: newest round first, earlier rounds demoted, Size refreshed at each pushed head

Extends docs/solutions/legion/proof-sha-ledger-and-negative-controls-LEGSMOKE-504.md.

When a pull request goes through more than one implementing round (a CI-red cure, a review
correction), the `CI` and `E2E (implementer)` lines accumulate rather than overwrite: put the
newest round's proof first, at its own named SHA, and demote the earlier rounds under a plain
heading inside the same line (`Earlier rounds (…):`). Never start a second `**E2E (implementer):**`
or `**CI:**` label: a reader or a check that takes the first match would read a stale round.

Refresh `Size` for the head you are about to push, from `jj diff --from <base-sha> --to @- --stat`,
and PATCH the body before `legion push`; a `Size` left at an earlier head is the finding the
reviewer raises. Confirm the pull request's head with a separate read
(`legion gh -- api repos/<owner>/<repo>/pulls/<n> --jq .head.sha`) rather than from the PATCH
response, which can still name the previous head seconds after a push.

## Evidence

- PR #525 carried three implementing rounds: code head `d29219f8`, cure `8c769387`, append
  `1f361ca0`. Round 3's body edit (implement.json `rounds[2].bodyEdits`) put the two-line proof first
  and kept rounds 1–2 under `Earlier rounds (one-line form, superseded by the append):`; the first
  draft of that edit produced a second `**E2E (implementer):**` label and was corrected before the
  PATCH.
- The reviewer's round-1 finding (review.json `round1.findings[0]`) was exactly a stale `Size`:
  `7 files changed, +558/−723 at 56dd2b66…` while the reviewed head `d85dacb0` had 8 files; the body
  was refreshed to the pushed head in rounds 2 and 3 and again at retro.
- After the round-1 handoff push, `--method PATCH … --jq .head.sha` returned `d29219f8…` while the
  branch already pointed at `23667df8…`; a `pulls/525 --jq .head.sha` read five seconds later showed
  the new head.

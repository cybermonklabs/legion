---
title: "Proving smoke/ marker-file changes when CI is content-blind"
category: legion
tags:
  - smoke-proof
  - marker-file
  - content-independent-ci
  - review-rounds
date: 2026-10-19
status: active
module: smoke
related_issues:
  - "S394220170-1"
  - "sjawhar/legion-smoke#645"
---

# Proving smoke/ marker-file changes when CI is content-blind

For a `smoke/<KEY>.md` marker-file change in `sjawhar/legion-smoke`, prove the file's
exact bytes at the pull-request head; green CI proves nothing about contents.

- CI is deliberately content-independent: `ci.yml` is `sleep 20 && exit 0` with no checkout,
  and `fail-on-demand.yml` fails only when a `.fail-me` file exists. Both stay green regardless
  of the marker file, so green CI is a liveness signal only. Make the file-content read the
  acceptance criterion and record CI as liveness, never as proof.
- The title line's separator is an em dash, U+2014 (bytes `e2 80 94`), matching canonical
  siblings such as `smoke/S394802790-1.md`; a hyphen silently fails the content check. Verify at
  the byte level (`xxd` / `base64 -d`) and keep a negative control that the hyphen variant does
  not match.
- Each review round appends exactly one line `Legion proof review round N (<KEY>)` below the
  existing lines and changes nothing else. The spec caps appends at one line per round; divergent
  siblings use other wordings (`Stage 3 review round N`) or a different title — do not copy a
  sibling blindly, follow the plan's pinned wording. Keep a negative control that no (N+1)th line
  exists.
- Base each review round on the branch's remote tip (`jj new legion/<KEY>@origin`), not on your
  previous round's commit: the tester and reviewer push their handoff commits above your code
  between rounds, so the tip has advanced. Appending with `>>` leaves the earlier lines
  byte-for-byte unchanged.
- Keep the one code commit (the `smoke/` file) distinct from the `.legion/<KEY>/<phase>.json`
  handoff commit. A handoff-only push ends its head message with `skip-checks: true` (last line,
  two empty lines before it) so GitHub starts no workflow; a code push carries no such trailer.
  Verify scope per commit with `jj diff --name-only -r <commit>`, not against the whole branch
  (the branch legitimately also carries other phases' handoff commits).

## Evidence

Issue S394220170-1 / PR #645: `smoke/S394220170-1.md` reached four lines (title + three
`Legion proof review round 1/2/3 (S394220170)`) across three reviewer REQUEST_CHANGES rounds.
Each round's content was proved via the GitHub contents API at the pushed head
(`gh api repos/sjawhar/legion-smoke/contents/smoke/S394220170-1.md?ref=<sha> --jq .content | base64 -d`)
with negative controls (hyphen-variant mismatch; empty next line). CI (`ci`, `fail-on-demand`)
was green every round but recorded as liveness only. The one bot thread ("is this file change
needed?") was adjudicated not-a-defect — the change is required by the spec — and resolved.

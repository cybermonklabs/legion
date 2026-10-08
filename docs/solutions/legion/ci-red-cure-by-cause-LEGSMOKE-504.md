---
title: "Cure a red check by its cause: new commit for tree content, rerun only for a flake"
category: legion
tags:
  - ci-red
  - legion-implementer
  - legion-planner
  - fail-on-demand
date: 2026-10-08
status: active
module: legion-phase-workflow
related_issues:
  - "LEGSMOKE-504"
  - "sjawhar/legion-smoke#522"
---

# Cure a red check by its cause: new commit for tree content, rerun only for a flake

Read the failing job's step before you choose a cure. When the red comes from the tree's content
(a file the gate tests for, a failing test, a lint error), cure it with a new commit on top of the
pushed branch tip (`jj new legion/<KEY>@origin`), touching only what the cause names. Rerun
(`legion gh -- run rerun <id> --failed`) only when you can argue the failure is independent of the
tree; a content-caused red stays red on every rerun. Never rewrite pushed history and never bring
the base in to cure a red.

When the plan's red-run rule is a flat "rerun only, never a new commit", follow the cause and record
the departure in the implement handoff's `deviations` field, not only in a note, so the reviewer
reading `deviations` sees it. A planner writes the red-run rule as a decision on cause, not a flat
prohibition.

Prove the cure with the control the cause gives you for free: the same check's failure rows at the
red SHA next to its success rows at the cured SHA, plus the absence of the causing input at the
cured SHA.

## Evidence

- The proof driver pushed `cc1327ed` adding the root file `.fail-me` (25 bytes). `fail-on-demand`'s
  `gate` step is `if [ -f .fail-me ]; then exit 1; else exit 0; fi`, so runs 37715132523 (push) and
  37715138525 (pull_request) failed, while `ci` (`sleep 20 && exit 0`, no checkout) passed at the
  same SHA. The daemon moved the issue back to implementing.
- The plan's `acceptance[2].redRun` said `legion gh -- run rerun <run-id> --failed only; never a
  new commit`. A rerun could not pass with `.fail-me` in the tree. The cure was `90984f5f`, a new
  commit above `legion/LEGSMOKE-504@origin` whose diff is exactly `D .fail-me`; four rows green there,
  `contents/.fail-me?ref=90984f5f` → HTTP 404.
- The implement handoff recorded the reasoning in `rounds[1].cure` and `notes[]` but left
  `deviations: []`; the retro's fresh-eyes pass flagged that a reviewer trusting `deviations` would not
  find it.
- `ci`'s green rows at the red SHA show that a green row is not evidence about the tree unless the
  workflow reads the tree; name which lanes do before calling "all green" a verdict.

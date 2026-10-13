---
title: "Two live roles editing one PR body lose writes: fetch right before PATCH and re-read after; state Size as the merge net"
category: legion
tags:
  - pr-body
  - lost-update
  - threads
  - size-line
date: 2026-10-13
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-738"
  - "sjawhar/legion-smoke#567"
---

# Two live roles editing one PR body lose writes: fetch right before PATCH and re-read after; state Size as the merge net

- The pull request body is one shared string with no precondition on `PATCH`: a role that fetches it, edits its own lines, and writes it back overwrites every edit another role landed in between. Fetch the live body immediately before the `PATCH` (same command, no pause for editing in between: edit a copy, then re-fetch and re-apply your lines onto the fresh copy), and after the `PATCH` re-fetch and confirm the other roles' lines are still what they wrote.
- When another live role has just told you it is about to edit the body (an Envoy message saying it is proceeding to approve or to write its lines), do your own edit after its confirmation, or tell it yours is coming first. Resolving a thread the reviewer named is immediate; recording it in `Threads` can wait a minute.
- A role whose lines were lost re-applies them itself: the lines belong to the role that writes them, and the retro must not author a reviewer's `Look at first` or `Not proven / risk`.
- Write `Size:` as the merge net (the files and ±lines the squash merge will add to the base) and say what is excluded and why (`.legion/<KEY>/` handoffs, removed by retro). Recompute it at every push and once more in retro when `docs/solutions/` files are added. Never quote `gh pr view --json additions` at a head: it counts the handoffs.

## Evidence

- LEGSMOKE-738, pull request sjawhar/legion-smoke#567. At 2026-10-13T12:07Z the reviewer wrote its round-2 `Look at first` and `Not proven / risk` into the live body (review.json `bodyEdits`, body length 14991). Within the same minute the implementer, asked by the reviewer to resolve bot thread `PRRT_kwDOUCdo886r2TiD`, fetched the body, added the thread row, and wrote it back: the body read at 12:10Z carried the new `Threads: 2 resolved` row and the reviewer's round-1 lines (`Criterion 3 … unexercised at c8889216`), so the reviewer's write was lost. The fresh-eyes retro subagent found it by diffing the live body against the approval's claim; the reviewer was asked to re-apply its lines before retro's push.
- `Size:` on #567 read `1 file, +2/−0 (smoke/LEGSMOKE-738.md, the merge net)` through review, with the handoffs named as excluded, and was recomputed in retro to add the three `docs/solutions/legion/` files. The merge-net convention was also used by #565 (LEGSMOKE-728); that note was removed from `main` by #566 and is recorded again here.

---
title: "Multi-round implementer: base each corrective commit on the fetched remote tip"
category: legion
tags:
  - legion-worker
  - implementer
  - jj
  - review-rounds
date: 2026-10-19
status: active
module: legion-worker
related_issues:
  - "S392694044-1"
  - "sjawhar/legion-smoke#693"
---

# Multi-round implementer: base each corrective commit on the fetched remote tip

When an implementer returns for a review-correction round, sibling roles have already pushed
their own handoff commits to `legion/<KEY>@origin` since the last implementing round, so the
origin tip has moved past the local `@-`. Build on the moved tip, not the stale local parent.

- Before the round's first commit, run `jj -R "$LEGION_WORKSPACE" git fetch`, then
  `jj -R "$LEGION_WORKSPACE" new 'legion/<KEY>@origin'`. Basing the new commit on the fetched
  remote tip makes the later `jj git push` a fast-forward; basing it on a stale local `@-` makes
  the push a non-fast-forward that `jj git push` refuses, and forcing is never allowed.
- The file content a code commit appends is unaffected: intervening sibling commits touch only
  `.legion/`, so the marker/source file at the remote tip is identical to the one you last left.
- Keep each review round to exactly one reviewable code commit, then the handoff commit; the jj
  history then reads as one add plus one append per round, trivially auditable.

## Handoff-only pushes carry the skip-checks trailer in an exact shape

A push whose commits touch only `.legion/` must end its head commit message with
`skip-checks: true` as the last line, preceded by two blank lines. Write it with jj's trailer
overlay disabled for that one describe, or the `Omp-Session:` trailer lands after it and breaks
the required last-line shape:

```
jj -R "$LEGION_WORKSPACE" describe -r @- --config 'templates.commit_trailers=""' \
  -m "$(jj -R "$LEGION_WORKSPACE" log -r @- --no-graph -T description)"$'\n\n\nskip-checks: true'
```

A code or mixed push (anything outside `.legion/`) carries no such trailer; the daemon classifies
by changed paths, and a wrong trailer stalls the issue rather than passing it.

## Evidence

S392694044-1 ran three correction rounds on PR sjawhar/legion-smoke#693. Each round the origin
tip had advanced to the reviewer's round handoff (`d0acb20e`, `8eac7c0f`), so each corrective
commit was based on `legion/S392694044-1@origin` and pushed as a fast-forward. Implementer proof
and tester proof both drove the GitHub PR head (the `gh api .../contents` fetch) with a negative
control (`.fail-me` present → gate exit 1); the single-line marker matched the convention sample
`smoke/S394802790-1.md` byte-for-byte (em dash U+2014).

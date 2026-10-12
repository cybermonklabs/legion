---
title: "Decide a review-conditional acceptance criterion from the reviews API, not from prose"
category: legion
tags:
  - acceptance-criteria
  - review-threads
  - github-api
  - implementer
  - tester
date: 2026-10-12
status: active
module: legion-phase-workers
related_issues:
  - "LEGSMOKE-692"
  - "sjawhar/legion-smoke#559"
---

# Decide a review-conditional acceptance criterion from the reviews API, not from prose

When a spec says "only when the review asks for X", define the trigger as a query before the
first push, and let every phase run the same query:

- Count `K` as the reviews on `pulls/<n>/reviews` whose `user.login` is the reviewer App, whose
  `state` is `CHANGES_REQUESTED`, and which own at least one entry in `pulls/<n>/comments` on the
  file the criterion names that asks for X. A `COMMENTED` review, a `CHANGES_REQUESTED` review
  about something else (a stale body line, a missing proof), and any thread by another author
  count zero and are answered without touching the file.
- Record `K`, the expected content it implies (line count, byte count, digest) and the review and
  comment ids that produced it in the implement handoff and in the PR body's `Change:` bullets,
  so the production check compares against a recorded value after retro has removed the handoffs.
- When the review names the exact text, write it as file content (`printf '%s\n' '<text>' >> <file>`),
  never interpolated into a shell word, and check the result byte for byte: the earlier lines
  `cmp`-identical to the decode at the SHA that first carried them, exactly one final LF, the
  expected digest.
- `K` above the spec's scope is a report to the architect, not a second append.

## Evidence

LEGSMOKE-692 (sjawhar/legion-smoke#559): the reviewer first submitted `COMMENT` 5488384349
(no inline comment; decided nothing), then `CHANGES_REQUESTED` 5488404611 with one inline comment
4246182869 on `smoke/LEGSMOKE-692.md:1` naming the line `Legion proof review round 1 (LEGSMOKE)`.
Only the second counted: `K=1`, expected 2 lines / 95 bytes / sha256
`59032d40ccb1d4ac7944fe37ba07e5d2bb8eb5d38a4512dc46eaf7ffc7e3818a`, verified at the append head
`e53bb94d` with `cmp <(decode-at-e53bb94d | head -1) <(decode-at-f182c812 | head -1)` → exit 0 and
negative controls (one-line file, swapped lines, third line, missing final LF) each exiting 1. The
implementer, tester and reviewer each re-derived `K` from the same two API calls rather than from
the review text in a message.

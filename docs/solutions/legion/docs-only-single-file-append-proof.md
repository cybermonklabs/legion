---
title: "Proving a docs-only single-file append through the Legion pipeline"
category: legion
tags:
  - legion
  - e2e-proof
  - docs-only
  - review-rounds
  - product-diff
date: 2026-09-26
status: active
module: legion-smoke
related_issues:
  - "S392202905-1"
  - "sjawhar/legion-smoke#329"
---

# Proving a docs-only single-file append through the Legion pipeline

Context: issue S392202905-1, PR sjawhar/legion-smoke#329. A docs-only change that appends
marker lines to `README.md` (one durable-proof line plus three review-round corrections)
driven through the full Legion pipeline: plan → implement → test → review ×N → retro → merge
→ production check. The learnings below generalize to any docs-only single-file append
verified end-to-end on GitHub.

## Assert the product-diff shape, not a fixed line count

The plan handoff stated "exactly one added line," which the three review rounds falsified the
moment they appended lines 2–4 (final diff `+4/-0`). What survived every round was the
tester's shape assertion, keyed on the *invariant* rather than the transient magnitude:

```sh
legion gh -- pr view <n> --repo <owner>/<repo> --json files \
  --jq '[.files[]|select(.path|startswith(".legion/")|not)]
        | (length==1 and .[0].path=="README.md" and .[0].deletions==0)'
```

Encode "one product file, append-only (zero deletions)" — not "N lines." A count-based
assertion breaks on the next legitimate review round; a shape-based one does not.

## Keep process artifacts out of the product fingerprint

`.legion/{plan,implement,test,review}.json` are handoff commits that ride along on the PR and
show up in `pr view --json files`. They are excluded from the product-diff fingerprint
(`'~(.legion | docs/solutions)'`) and removed at final review. Every product-diff assertion
MUST filter `startswith(".legion/")` explicitly, or the process handoffs pollute the verdict
(a naive `files|length==1` reports FAIL because `.legion/plan.json` is also present).

## Namespace the marker by issue ID for a precise negative control

`README.md` already carried ~50 prior `Stage 3 review round N (Sxxxx)` lines from other
issues. Tagging every appended line with this issue's ID (`S392202905`) makes the negative
control exact:

```sh
legion gh -- api "repos/<owner>/<repo>/contents/README.md?ref=main" --jq .content \
  | base64 -d | grep -c "review round 3 (S392202905)"   # -> 0 pre-merge = genuinely new
```

A generic `grep "Stage 3 proof"` would false-positive against history and prove nothing.

## Prove against the GitHub API surface, not the local checkout

Both implementer and tester read the live surface — `pr view --json files` plus
`api .../contents/README.md?ref=<branch>|main | base64 -d | tail` — rather than a local `jj`
read. This is the production-like surface: it catches push/branch-state drift (a bookmark that
did not advance, a commit that never reached origin) that a local working-copy read would miss.
Reuse the dual pattern: PR files for the diff shape, contents-at-ref for the resulting content.

## Risk: synthetic single-line review rounds can mask off-target edits

The three review rounds each asked only to append one more identically-formatted line, and
approval ultimately came from an out-of-band instruction (`review.json` round 4,
"takes precedence"), with zero review threads. This exercises the correction loop but couples
each round to one file's tail. A reviewer focused solely on "is the next requested line
present" could miss a stray edit *above* the append. For genuine work, each review round should
re-assert the whole diff stayed append-only (the shape assertion above), not merely that the
requested line landed.

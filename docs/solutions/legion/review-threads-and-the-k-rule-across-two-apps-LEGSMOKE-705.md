---
title: "Review threads across two GitHub Apps and a third-party bot, and a K rule every role can re-derive"
category: legion
tags:
  - review-threads
  - resolveReviewThread
  - graphql
  - k-rule
  - changes-requested
date: 2026-10-13
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-705"
  - "sjawhar/legion-smoke#561"
---

# Review threads across two GitHub Apps and a third-party bot, and a K rule every role can re-derive

- Derive "did the review ask for X" from a filter every role can re-run from the API, not from the existence of a review: author (`legion-reviewer[bot]`), state (`CHANGES_REQUESTED`), and ownership of an inline comment on the file. A `COMMENTED` review, the implementer's own reply review, and another bot's thread all count zero.
- The implementer resolves every thread, its own and the ones the reviewer names as accepted. The reviewer's App cannot: it replies with its adjudication and sends the implementer the thread's node id (and its reply id) by Envoy or in its review body.
- A thread's node id is `PRRT_…` from GraphQL `reviewThreads`; the comment id from the REST `pulls/<n>/comments` call is `PRRC_…` and does not resolve. Read the thread before the call, resolve one thread per call, and record the row only after `isResolved: true`.
- Resolving a named thread and recording it in the PR body is a GitHub write, not a workspace write: do it when the reviewer's message arrives, even between your own phases, with no push and no workspace change.
- In the `Threads:` row, name who adjudicated and who resolved, and the head at the moment of resolution (which may be another role's handoff head, with the code head unchanged).

## Evidence

- Reviews on PR #561: 5497848630 `COMMENTED` (0), 5497856149 `CHANGES_REQUESTED` with inline comment 4253895304 (K=1), 5497872381 `COMMENTED` by the implementer (0), 5497931035 `COMMENTED` by `sjawhar-agent[bot]` with an inline comment (0), 5497941306/5497941430 `COMMENTED` by the reviewer (0), 5497954333 `APPROVED`. Implementer, tester and reviewer each re-derived K=1 from the same two calls.
- Thread PRRT_kwDOUCdo886rrbjy (reviewer's ask): implementer reply 4253909991 `Fixed in 60c2c57c…`, then `resolveReviewThread` → true. Thread PRRT_kwDOUCdo886rrlxH (`sjawhar-agent[bot]`'s "is this file change needed?"): reviewer adjudication reply 4253970391, named to the implementer by Envoy message 24120e4171a5bd3ea1a5853db13ec30e, resolved by the implementer at 01:58:27Z while the reviewer's round-2 phase was active.
- Predecessor (removed from `main` by #560, in history at 152231add64a): `docs/solutions/legion/review-conditional-acceptance-from-reviews-api-LEGSMOKE-692.md`.

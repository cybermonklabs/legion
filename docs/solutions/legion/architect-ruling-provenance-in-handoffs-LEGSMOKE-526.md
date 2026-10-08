---
title: "Record an architect's spec ruling in the handoff with its Envoy message id so later roles cite it instead of re-deriving it"
category: legion
tags:
  - handoffs
  - spec-departure
  - envoy
  - legion-implementer
  - legion-reviewer
date: 2026-10-08
status: active
module: legion-phase-workflow
related_issues:
  - "LEGSMOKE-526"
  - "sjawhar/legion-smoke#525"
---

# Record an architect's spec ruling in the handoff with its Envoy message id so later roles cite it instead of re-deriving it

When the plan records a spec departure and the architect rules on it over Envoy, the first phase
that acts on the ruling writes it into its handoff as a first-class field: the Envoy message id,
its timestamp, and the ruling's operative sentence quoted, not paraphrased. Every later role cites
that field and id when it measures the affected acceptance line; none re-opens the reading. The
spec itself stays as the architect left it: a phase worker never edits it.

## Evidence

- plan.json `specDepartures[0]` recorded that acceptance 3's "No existing file changes" fails
  literally because the daemon's branch-start commit `3f6d0879` removes
  `.legion/LEGSMOKE-504/{implement,plan,review,test}.json`, and proposed the reading "measured
  outside `.legion/` and `docs/solutions/`".
- The architect's ruling arrived as Envoy message `b9c222c537c100a4a67e22a378c7117e`
  (2026-10-08T13:45:06Z): the plan's reading stands, spec v2 is not amended, the removals fall under
  acceptance 3's `.legion/` carve-out, do not restore them. implement.json carries it as
  `architectRuling` with the id and timestamp.
- test.json `notes` and review.json `verified.acceptance3` (both rounds) cite "architect ruling
  b9c222c5" / "implement.json architectRuling" when they count the removed rows, and no role
  re-litigated the reading across the issue's eight handoff commits.

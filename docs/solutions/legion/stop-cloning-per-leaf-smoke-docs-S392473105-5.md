---
title: "Stop cloning a per-leaf solution doc for each identical smoke leaf"
category: legion
tags:
  - smoke
  - docs-solutions
  - retro
  - duplication
date: 2026-10-18
status: active
module: legion-worker
related_issues:
  - "S392473105-5"
  - "sjawhar/legion-smoke#620"
---

# Stop cloning a per-leaf solution doc for each identical smoke leaf

Extends docs/solutions/legion/acceptance-check-for-a-bare-key-smoke-leaf-S393243811-5.md.

Delta: the bare-key child-leaf pattern has now recurred a third time (S393243811-5, S395289629-5,
S392473105-5) with **zero** new wrinkle — same 13-byte `S#########-5\n` deliverable, same empty
child spec inherited from the parent, same `skip-checks:true` handoff head, same content-blind
`ci/check`, same "Dependent admission leaf" title that names the already-cleared daemon admission
gate rather than an in-file dependency. When a retro finds its leaf matches the two docs above
byte-for-byte and mechanic-for-mechanic, **do not author a fourth per-leaf clone.** Apply those two
docs directly (cmp content pinning, creation-commit-scoped `wc -l`, the parent-key negative control,
skip-checks placement) and let the retro's only durable commit be the `.legion/<KEY>/` removal.
A per-PR doc chain that restates identical rules once per leaf is the by-PR organization the retro
rule warns against; the reusable topic is already captured, so the correct output is this one
delta note (recording that the pattern is now confirmed stable) and nothing more.

## Evidence

- Issue S392473105-5 (child of S392473105-1), PR sjawhar/legion-smoke#620. One file
  `smoke/S392473105-5.md`, bytes `5333 3932 3437 3331 3035 2d35 0a` = `S392473105-5\n` (13 bytes),
  byte-for-byte identical in shape to `smoke/S393243811-5.md` and `smoke/S395289629-5.md`.
- Fresh-eyes retro analysis returned nine candidate learnings; every one mapped onto the two
  existing sibling docs (CI necessary-not-sufficient, cmp proof, indexical child key + parent-key
  negative control, empty-spec→inherit, title≠dependency, dual code/handoff heads, scoped diff,
  spec-driven `.md` extension). No candidate was new.
- Review approved with 0 critical/important/minor findings; tester verified the implementer's proof
  (`implementerProof.verdict: "verified"`).

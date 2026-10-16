---
title: "Claims that go stale as the branch grows"
category: legion
tags:
  - proof
  - pr-body
  - handoff
  - review
  - merge-gate
date: 2026-10-16
status: active
module: legion-worker
related_issues:
  - "SMOKE-4"
  - "sjawhar/legion-smoke#603"
---

# Claims that go stale as the branch grows

Extends docs/solutions/legion/proving-facts-the-event-erases-SMOKE-3.md.

That document stamps every read of a volatile *field* with its UTC time and the object's state.
The delta: on a Legion branch the **set of commits is itself the volatile object**, and the PR
body — unlike a handoff — is read by a human after the phase that wrote it has ended. Three
rules follow.

**Scope a class-wide claim to the SHAs you read.** Never write a present-tense quantifier over a
class of commits ("the handoff commits are authored by X", "the timeline has N commits") in a
handoff or a PR body. Enumerate the SHAs it rests on, or stamp it with the branch tip at read
time. Every later phase adds a commit to the class you quantified over.

**A re-derivation that disagrees is a finding.** The re-verifying phase's verdict covers the
text it verified, not only the acceptance criteria. When your independent read produces a
different fact than the sentence you are checking, that is a `failures` row or at least a
`keyFindings` entry plus a body edit — never something absorbed into a `verified`. Phrases like
"reproduced exactly" in `implementerProof.how` are themselves claims.

**An approval of a head that knowingly fails a criterion carries three things:** the
`Not proven / risk` bullet saying the criterion is unsatisfied *at this head*, the phase that
must fix it and the gate that would otherwise catch it, and a ceiling SHA naming which paths a
later commit may touch without voiding the approval. Without all three, the approval reads as
approval of a head that fails the criteria.

## Evidence

SMOKE-4 (`sjawhar/legion-smoke#603`).

The implementer's `E2E (implementer)` line said "Handoff commits on the branch are authored by
`legion-reviewer[bot]` / `343353696`". True at 21:16Z, when `6821c883` (`plan: record handoff`)
was the only handoff commit. By 21:22Z the implementer's own `cba0799d`
(`implement: record handoff`) was on the branch, authored and committed by
`legion-implementer[bot]` / `343353655`. Read at retro, the four handoff commits are
`6821c883` review App, `cba0799d` **implement App**, `f784e3d4` review App, `8667ebe0` review
App: the rule is the *writing role's* App, not "handoffs are the review App", and the planner's
`plan.json` F4 had seeded the wrong generality before any commit existed. No acceptance
criterion depended on it — the criterion is about who wrote `smoke/SMOKE-4.md` — but a false
sentence sat in the body for 25 minutes.

The tester recorded the refuting datum and the stale sentence in the same file:
`test.json` `proof[1].observed` names `cba0799d` as the implement App in both columns, while
`implementerProof` reads `verdict: "verified"`, `failed: 0`, `failures: []`, and `how` asserts
every claim "reproduced exactly". The reviewer then wrote the precise version into
`review.json` `verifiedOnGitHub` and reported `critical: 0, important: 0, minor: 0`. Two
independent phases held the contradiction and neither raised it; retro corrected the body.

The same moving set shows up as a control that answers differently each round:
`select(.event=="committed")|length` → `3` in `implement.json`, `4` in `test.json`, `5` in
`review.json`, `6` at retro. Each is right for its moment and none says so, so the three
handoffs read as three answers to one question.

The approval rule is this run's positive example, and it is why acceptance criterion 3 survived.
Criterion 3 forbids `.legion/SMOKE-4/` from reaching `main`, and the approved head carried four
such files. `review.json` wrote all three parts: the body bullet ("acceptance criterion 3 is
unsatisfied at this head because the branch carries `.legion/SMOKE-4/*.json`, and it rests
entirely on retro's last commit removing that directory before the merge"), the owner and gate
("Retro's last commit MUST remove `.legion/SMOKE-4/` … READY refuses a head still carrying it"),
and the ceiling ("The approved head's code is `0a2670681406379945271f5a5a5ba76581617e28`;
nothing above it may change any path outside `docs/solutions/` and `.legion/SMOKE-4/` without
voiding the approval"). Retro's two commits sit inside that ceiling.

One criterion half that was *not* disclosed, and is the same class of omission: the criterion's
machine form admits only `^(smoke/SMOKE-4\.md|docs/solutions/[^/]+/.+-SMOKE-4\.md)$`, so a retro
learning whose filename does not end `-SMOKE-4.md` fails criterion 3 exactly as surely as an
un-removed handoff. The removal dependency was called out three times; the naming dependency
never. A convention both earlier documents follow is not a disclosure.

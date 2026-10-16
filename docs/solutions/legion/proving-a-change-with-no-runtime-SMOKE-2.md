---
title: "Proving a change that has no runtime"
category: legion
tags:
  - proof
  - negative-control
  - docs-only-change
  - acceptance-criteria
date: 2026-10-16
status: active
module: legion-worker
related_issues:
  - "SMOKE-2"
  - "sjawhar/legion-smoke#589"
---

# Proving a change that has no runtime

Some changes execute nothing: a marker file, a README, a configuration value a service reads
later. They still need a proof on a production-like surface, and these rules make one possible.

- Make the hosting service the surface. When the change has no process of its own, the real
  repository's own API serving the artifact at a named SHA is the production-like surface. Read it
  at a SHA, never at a branch name or a bare `main`: a branch ref moves under you between the read
  and the write-up.
- Pin the artifact's exact bytes in the plan, as a command that reproduces them, and have every
  later phase `cmp` the served bytes against a reference it generates in its own pane. Phases then
  disagree about nothing, and no phase retypes the content.
- Cross-check the representation. Equality against a reference you also authored proves only
  self-consistency; compare the service's own content hash with the one computed locally
  (`git hash-object`), which is independent of how the API encodes the body.
- A negative control must be able to fail. Before you record one, run the check against the broken
  input and see the refusal. A guard whose allow-list already admits every path present is an
  always-empty check, not a proof, however carefully it is written.
- A red run you cite instead of creating is a control only once you have shown its failing
  precondition held there and does not hold at your head. A `conclusion: failure` field alone does
  not rule out a check that cannot fail.
- Split byte identity from fact truth, and say which phase owns each. Comparing bytes against the
  plan cannot catch a wrong fact the plan itself agreed; some phase must check the facts against
  their sources.
- An unfalsifiable claim in the artifact (a historical attestation, a provenance statement) is
  checked for presence, never for truth. Record what is checked and what is not, and never weaken
  the artifact to make it checkable.
- An acceptance criterion whose outcome is outside the tree's control — who presses merge, when an
  operator deploys — is named, assigned to the architect, and left as it is. Do not redefine it to
  make it provable, and do not engineer a check that pretends to cover it.

## Evidence (SMOKE-2, sjawhar/legion-smoke#589)

The change was one 208-byte Markdown marker, `smoke/SMOKE-2.md`, in a repository with no test
suite, no lint and no build: `.github/workflows/ci.yml`'s job `check` is `sleep 20 && exit 0`, and
`fail-on-demand.yml`'s job `gate` fails exactly when `.fail-me` is at the root.

- The plan pinned the bytes as a `printf` and every phase compared against a reference generated
  in its own pane: `gh api "repos/sjawhar/legion-smoke/contents/smoke/SMOKE-2.md?ref=71461a80bf63c57b093ee4389f3f0d34a7eaa932" --jq .content | base64 -d | cmp - "$ref"` → exit 0. Zero wording rounds; `specDepartures: []`.
- Representation cross-check: GitHub reported `.size` 208 and `.sha`
  `e1e07bddcc287dc72660fc32ccf018ea8d1b3508`, equal to `git hash-object` on the workspace file.
- Controls that did fail: the same read at `main`'s tip `4c2722b35c6a11b9131a29643440d1593dd1df0a`
  → HTTP 404; the all-zero SHA → 404 `No commit found for the ref`; a one-byte-altered reference
  (`gVisor` → `gVisoR`) → `cmp: differ: char 194, line 1`.
- The always-empty guard, found by the retro's fresh-eyes pass and recorded here rather than
  hidden: C3's pre-merge path guard filtered the branch's file list through an alternation that
  *allows* `.legion/SMOKE-2/*`, and the control offered for it — that the list contains
  `.legion/SMOKE-2/plan.json` — is a statement about the merged head, not a rejection the filter
  performed. No phase showed that grep rejecting anything. The post-merge half of the same
  criterion does have a real control (the `contents/.legion/SMOKE-2` read: 200 at the approved
  head, 404 required at the merge commit).
- Cited-not-created red run: `fail-on-demand` run `38794798972` at
  `20634d1dc539504314a2cbe34d86c10986d10e99`, conclusion `failure`. The implementer cited the
  conclusion; the tester made it a control by reading `contents/.fail-me` at that SHA (present,
  25 bytes) and at our head (404).
- Byte identity versus fact truth: the implementer proved the served bytes equal the plan's
  reference; the tester checked the facts the bytes assert (`dispatch projects` → `SMOKE` is the
  Dispatch project key, the issue key matches the filename, the date is the run date). The file's
  two historical attestations ("the first issue a Legion daemon on GKE Autopilot took from
  Dispatch to a merged pull request", "every phase ran in an Agent Sandbox pod under gVisor") were
  fenced off as unfalsifiable, with `/proc/version` → `Linux version 4.19.0-gvisor` recorded as
  corroboration only.
- Out of the tree's control: acceptance criterion 1 names the merging identity, and the two
  comparable prior runs were merged by the operator's fixture App (`gh api repos/sjawhar/legion-smoke/pulls/575 --jq '[.user.login,.merged_by.login]'` → `trajectory-s-legion-implementer[bot]`,
  `sjawhar-agent[bot]`). It stayed an `openQuestions` entry for the architect and a
  `Not proven / risk` line, and no phase invented a check for it.

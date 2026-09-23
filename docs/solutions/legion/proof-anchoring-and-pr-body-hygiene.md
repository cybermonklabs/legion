---
title: "Legion proof anchoring, PR-body hygiene, and no-finding review rounds"
category: legion
tags:
  - legion
  - jj
  - e2e-proof
  - pr-body
  - review-rounds
  - merge-gate
date: 2026-09-23
status: active
module: legion-worker
related_issues:
  - "S391024427-1"
  - "sjawhar/legion-smoke#91"
---

# Legion proof anchoring, PR-body hygiene, and no-finding review rounds

Learnings from delivering S391024427-1 (PR sjawhar/legion-smoke#91): a one-line
`README.md` append that went through three scripted `CHANGES_REQUESTED` review rounds.
Complements `docs/solutions/legion/implementer-handoff-and-correction-round-mechanics.md`
(from #89) with the concrete mechanisms that kept the E2E proof valid across the churn.

## 1. Anchor E2E proofs to the stable substantive commit, not the branch tip

**Mechanism.** jj commit ids embed the committer timestamp, so re-describing or
splitting a commit produces a new SHA even when the tree is byte-identical. In this
issue the README line was first pushed as `33634b1f`; a later `jj split` that separated
the handoff file reproduced the identical README tree as `cc6f3287` — same content,
different SHA. The branch tip then moved on every correction round (each round adds a
handoff commit) and every tester rebase.

**Action.** Point every E2E proof command at the commit that actually holds the product
change — the one whose `jj -R "$LEGION_WORKSPACE" diff -r <sha> --stat` shows the product
file (here `README.md | 1 +`) — never at the moving branch tip. The GitHub Contents API
resolves any full SHA reachable from the branch, so
`legion gh -- api 'repos/<owner>/<repo>/contents/<path>?ref=<substantive-sha>'` keeps
working after the tip advances. Both the implementer and tester proofs on #91 cited
`cc6f3287` and stayed valid through three rounds without re-running the real-surface
check. This is the general pattern: the substantive commit is the durable anchor; handoff
commits are process noise stacked above it.

## 2. Surface committed handoff proofs into the human-facing PR body

**Mechanism.** Reviewers and the merger read the PR body on GitHub, not the `.legion/*.json`
handoffs. On #91 the tester wrote a verified proof into `.legion/test.json`, but the PR
body's `E2E (tester):` line stayed `pending` and the `CI`/`Threads` lines predated the
review rounds. The approval gate ("approval requires both E2E lines present") checks the
body, so a proof living only in the handoff is invisible to it.

**Action.** Every phase that changes the head or verifies acceptance refreshes the PR body
in the same turn it writes its handoff: `legion gh -- pr edit <n> --repo <owner>/<repo>
--body-file <file>`. The tester in particular must add its `E2E (tester):` line to the
body, not only to `.legion/test.json`. Keep the body's `CI` run ids, head SHAs, `Threads`
count, and each `E2E` line current. Outside a scripted smoke, a missing `E2E (tester)`
line sends the issue back to the tester before READY — so the drift is a real gate failure,
not cosmetic.

## 3. No-finding review rounds are documentation-only corrections — never invent a diff

**Mechanism.** Three review rounds submitted `CHANGES_REQUESTED` with no inline threads
(`legion threads resolve --pr 91 --repo <owner>/<repo>` reported `no unresolved threads`,
exit 0, every round) and no actionable finding. Acceptance pinned `README.md` to exactly
one added line, forbidding any further content edit, yet the gate still required a response
each round.

**Action.** When a review requests changes but `legion threads resolve` reports zero
unresolved threads, acceptance is already satisfied, and CI is green, the minimal
responsive correction is documentation-only: `jj -R "$LEGION_WORKSPACE" new` a child on the
current head, rewrite `.legion/implement.json` (via `legion handoff write --phase implement`)
documenting the round and the no-finding disposition, `describe` it, move the bookmark, and
push. Never fabricate a product change to "have a diff", and never re-describe or split the
already-pushed substantive commit — that orphans its green CI run and changes the anchor in
section 1. The README diff (`cc6f3287`) stayed byte-identical across all three rounds.

## 4. Delete `.legion/` before the reviewer approves, not after

**Mechanism.** The plan specifies `.legion/` process artifacts are removed by the
implementer's `.legion` deletion push at the end of a clean review, before merge. On #91 the
reviewer's `APPROVED` landed on head `d948bdc0`, which still carried `.legion/implement.json`,
`.legion/test.json`, etc. Approving a head that still holds process artifacts means the
approved diff is not the mergeable diff, and the artifacts risk leaking into `main`.

**Action.** Sequence the merge gate as `legion-retro` documents it: after the final tester
recheck, the implementer pushes the `.legion/` deletion at the reviewer's direction, and the
reviewer approves *that* cleaned head. Retro then commits only under `docs/solutions/` on top
of the approved clean head (a `docs/solutions/`-only commit does not void the approval). If
approval lands on a head that still carries `.legion/` (as in this scripted smoke), treat it
as a merge-gate ordering defect: the merger's `jj diff --from <approved-sha> --to <tip-sha>
--summary` must show only `docs/solutions/` changes above the approved head before publishing
READY, and a still-present `.legion/` tree above/at the approved head is a signal to return
for the deletion push rather than merge process metadata into `main`.

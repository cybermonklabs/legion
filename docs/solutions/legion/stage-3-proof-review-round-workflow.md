---
title: "Legion review-round mechanics: moving tips, per-round handoffs, and split reviewer identities"
category: legion
tags:
  - legion-workflow
  - review-rounds
  - jj-bookmark
  - ci-head-tracking
  - review-decision
date: 2026-09-26
status: active
module: legion
related_issues:
  - "S392742634-1"
  - "sjawhar/legion-smoke#321"
---

# Legion review-round mechanics

Grounded in issue S392742634-1 / PR #321: a marker-line append to `smoke.txt` driven through
three full implement→test→review cycles. The change was trivial; the reusable value is the
workflow mechanics a future phase worker will hit on any issue.

## Each review round is a whole phase — a push does not finish it

Every round the daemon re-enters the implementer with `Phase: implementing`. The round is only
finished by calling the `legion` tool's `handoff_complete`, never by the push. On this issue the
correction workflow messages said it explicitly ("a push alone does not finish this round"), and
each round produced its own `.legion/implement.json` (per-round `completed` timestamp) followed by
`handoff_complete`, then the tester ran and did the same. Sequence per round:
append line → `describe` correction commit → `jj new` → `legion threads resolve` →
push → `handoff_write` → `split` handoff commit → push → `handoff_complete`.

## The branch tip moves every phase; re-pin CI to the live tip

Each phase appends a handoff commit that advances the single shared bookmark
`legion/S392742634-1`, so the CI-green head drifts within a round:
implementer correction (`75985806`) → implementer `implement: record handoff` (`a5f4a18b`) →
tester `test: record handoff` (`4aae83c8`, the approved head). The correction commit's smoke.txt
diff is identical to the handoff commit's, but **CI runs against whichever commit is tip**. Always
re-read the freshest tip (`jj log -r legion/<KEY>`) and cite the CI run at that head — the PR body
carried `CI SUCCESS at a5f4a18b` while the approved tip was already `4aae83c8`.

## When you rewrite the shared READY body, refresh every phase's E2E headSha

The PR body is one READY document rewritten by the latest phase. The tester's rewrite left the
`E2E (implementer)` line pinned to the stale correction head `759858069517` while the
`E2E (tester)` line cited `a5f4a18b` — the two blocks disagreed on head. When you edit the shared
body, update the sibling phase's headSha to the current tip, not only your own line.

## A split reviewer identity leaves `reviewDecision` stuck at CHANGES_REQUESTED

The round-1/2/3 `CHANGES_REQUESTED` reviews were posted by `sjawhar-agent[bot]`; the final
`APPROVE` came from `legion-reviewer[bot]`. GitHub aggregates `reviewDecision` per-reviewer, so a
different identity's `APPROVE` does **not** dismiss another identity's blocking review:
`reviewDecision` stayed `CHANGES_REQUESTED` (though `mergeStateStatus: CLEAN`,
`mergeable: MERGEABLE`) after approval. If a repo's branch protection requires
`reviewDecision == APPROVED`, the approving identity must be the same one that requested changes,
or the stale blocking reviews must be dismissed. Verify `reviewDecision`, not just the newest
review's state, before treating a PR as approved.

## Shared-workspace jj discipline: re-read the tip immediately before committing

Multiple sessions drive the one bookmark in one shared workspace; a sibling phase can advance the
tip between your read and your write. Round-3 was correctly appended below the tester's head
`78d1cb05` because the tip was re-read first. Use the standard push guard — a `behind` check that
refuses unless `@-` descends from `legion/<KEY>@origin`, then
`bookmark set -r @- --allow-backwards` and `git push --bookmark` — and never force-push on refusal.

## Corrections stay append-only and idempotent, proven with grep + negative control

Every round appended exactly one line below the existing lines, changing nothing else, proven with
`grep -Fc '<line>' smoke.txt` == 1 plus a negative control for an unwritten round
(`grep -Fc 'Stage 3 review round 7 …'` → `0`, exit 1). Reuse this scoped idempotent-append
discipline (check-before-append, exact-count assertion, negative control) instead of rewriting a
shared file.

## Credential grant is a 60-second window per call

`legion gh -- run watch` redeems one token per invocation; chaining two `run watch` calls in one
bash command exhausted the 60 s grant and the second returned `GRANT_EXPIRED (403)`. Run each
long credentialed command (a `run watch`, a run that outlasts a minute) in its own bash call so it
redeems a fresh grant.

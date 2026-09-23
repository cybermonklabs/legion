---
title: "Legion review-round loops on a stable product diff: per-round disposition, proof anchoring, and PR-body honesty"
category: legion
tags:
  - legion
  - review-rounds
  - e2e-proof
  - jj
  - pr-body
date: 2026-09-23
status: active
module: legion
related_issues:
  - "S391283701-1"
  - "sjawhar/legion-smoke#93"
---

# Legion review-round loops on a stable product diff

PR sjawhar/legion-smoke#93 (issue S391283701-1) appended one final line to `README.md`
(`Stage 3 smoke check: Legion workflow proof.`) and then absorbed three `CHANGES_REQUESTED`
review rounds that each carried **no inline threads and no actionable finding**, before the
reviewer approved head `a68a50d4ee65`. The product diff (`README.md | 1 +`) was byte-identical
across all three rounds. This is the recurring shape of a scripted or no-finding review loop; the
learnings below are the per-round procedure that keeps the branch, the proofs, and the PR body
honest without ever touching the approved product diff.

This document is the operational checklist; the mechanics it references are detailed in the two
sibling docs and are not repeated here:

- `docs/solutions/legion/proof-anchoring-and-pr-body-hygiene.md` — jj re-timestamping, anchoring
  proofs to the substantive commit SHA, PR-body drift, and the `.legion/`-in-approved-head merge
  hazard.
- `docs/solutions/legion/implementer-handoff-and-correction-round-mechanics.md` — handoff file
  naming, committing the handoff as a child of the product commit (never amending it), and
  single-use `legion gh` credentials.

## Per no-finding review round, do exactly this

1. **Confirm there is no actionable finding.** Read the review body and, decisively, the inline
   threads: `legion gh -- api 'repos/<owner>/<repo>/pulls/<n>/comments'`. Run
   `legion threads resolve --pr <n> --repo <owner>/<repo>` and record its exact output
   (`no unresolved threads`, exit 0, on #93 all three rounds). A `CHANGES_REQUESTED` with zero
   threads and an already-met, CI-green acceptance is a documentation-only round.

2. **Never fabricate a product change to "respond".** The approved spec on #93 forbade any further
   `README` content change; the single final line was already present and green. Inventing a diff
   to look responsive would orphan the product commit's green CI and violate the spec. The correct
   response is a `.legion/<phase>.json` handoff refresh that names the round, the no-finding
   disposition, and the `threads resolve` result — plus a PR-body refresh — and nothing else.

3. **Keep the handoff a child commit; never amend the pushed product commit.** Write the refreshed
   handoff, then commit it as a *new* commit on top of the current branch tip
   (`jj describe`/`jj split` on the working copy), leaving the product commit's SHA — and its CI —
   untouched. On #93 every round added an `implement: refresh handoff for review round N` commit
   above the tester's committed handoff.

4. **Carry the review App's committed handoffs forward on your push.** The tester runs as the
   review App, which cannot push (`packages/daemon/src/daemon/AGENTS.md`, GitHub Apps). Its
   `test: record handoff` commits sit on the branch waiting for the implementer's next push. When
   you push a correction round, advance the bookmark to include those commits so they reach the
   remote — do not rewrite or drop them.

5. **Anchor every E2E proof to the substantive product commit SHA, not the branch tip.** jj
   re-timestamps commits on split/rebase, so the tip SHA moves even when the product tree is
   byte-identical. On #93 the product line was first pushed as `8f4972844bdf`, re-timestamped to
   `79204cdc30f7` when the handoff was split out, and both the implementer and tester proofs cite
   `79204cdc30f7` (`legion gh -- api '.../contents/README.md?ref=79204cdc30f7...'`) — valid through
   all three rounds and the tester's rechecks with no re-run of the real GitHub surface needed.
   The GitHub Contents API resolves any reachable full SHA.

6. **Refresh the PR body in the same push — mirror the tester's proof into it.** Reviewers and the
   merger read the PR body on GitHub, not `.legion/*.json`. On #93 the tester wrote a *verified*
   proof into `.legion/test.json` (`implementerProof.verdict: verified`, anchored to the final head
   `a68a50d4`), but the PR body's `E2E (tester):` line was left at `pending` for the whole loop —
   invisible to the approval gate and the merger. This is the same drift the sibling hygiene doc
   warns about, and it recurred here, so treat it as systemic: **every phase that verifies
   acceptance edits the PR body in the same push** with `legion gh -- pr edit <n> --body-file`,
   keeping `CI` run ids, `Threads`, head SHAs, and both `E2E` lines current. Retro caught and
   repaired the drift by transcribing the committed tester proof into the body.

## Merge-gate note carried out of this run

The approval on #93 landed on head `a68a50d4`, which still carries the `.legion/` process tree
(the standard end-of-review `.legion/` deletion push was not part of this scripted proof). Before
publishing `READY`, the merger must run `jj -R "$LEGION_WORKSPACE" diff --from <approved-sha>
--to <tip-sha> --summary` and confirm only `docs/solutions/` changes sit above the approved head;
a `.legion/` change above/at the approved head is a merge-gate ordering signal to return for the
deletion push, not to merge process metadata into `main`. Retro's own commit is based directly on
the approved head and touches only `docs/solutions/`, so it is a fast-forward that leaves the
approval intact.

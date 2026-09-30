---
title: "Smoke-proof learnings, sixth run: green at a head with no check runs, the ruling that still has no owner, and the run-list short-SHA trap"
category: legion
tags:
  - legion-workflow
  - proof
  - smoke-repository
  - github-actions
  - gh-cli
  - go-daemon
  - handoff-schema
  - skip-checks
date: 2026-09-30
status: active
module: legion-smoke
related_issues:
  - "LEGSMOKE-362"
  - "sjawhar/legion-smoke#473"
  - "LEGSMOKE-358"
  - "sjawhar/legion-smoke#471"
---

# Smoke-proof learnings, sixth run: green at a head with no check runs, the ruling that still has no owner, and the run-list short-SHA trap

Retro of LEGSMOKE-362 (Stage 4b proof tree 1: the whole workflow), delivered as pull request
sjawhar/legion-smoke#473, which adds `smoke/stage4b-legsmoke-362.md` holding the one line
`Stage 4b workflow proof: LEGSMOKE-362.` (39 bytes, LF-terminated). Sixth run of the same
one-file change under the Go daemon. Both proofs on #473 were read and are complete: the
implementer's `E2E (implementer)` line names the authenticated Contents API
(`legion gh -- api "repos/sjawhar/legion-smoke/contents/smoke/stage4b-legsmoke-362.md?ref=<sha>"`)
at the code commit `15195ac4` with `?ref=main` → 404, `.fail-me` → 404, bogus-SHA
`check-runs` → 422 and `compare/main...main` → identical as controls; the tester's
`E2E (tester)` line names the same surface at the then pull-request head `9c868de6`, adds a
`?ref=<base 6021e9dd>` → 404 control and a git-blob-hash comparison (`db28ec3c…` both locally and
as served), and records the re-run of the implementer's three commands at `15195ac4`
(`.legion/test.json` `implementerProof.verdict: "verified"`). The reviewer reviewed `44e2179c` and
approved `ee54202a` (its own `review: record handoff` commit) with four green runs on it. One
fresh-eyes scout read the four handoffs and the pull request; its findings are integrated below
and the generic ones dropped. Run 5's document
(`docs/solutions/legion/smoke-proof-learnings-fifth-run-wiped-index-head-vocabulary-and-the-undecided-risk.md`,
readable with `jj file show -r '6021e9ddb55b-' <path>`) already records the head vocabulary, the
`cmp <(…)` sandbox failure, the wiped-learnings index and its one-command recovery; none of that is
repeated here.

## Green at a head with no check runs: how the reviewer proved it

New this run: the tester's `test: record handoff` push carried GitHub's `skip-checks: true`
trailer, so the head the reviewer was given, `44e2179c`, has **zero** workflow runs —
`legion gh -- run list --commit 44e2179c0b0e157853c0b42fb1571286dda2f741` prints `[]` and
`pr checks` prints nothing. The reviewer did not treat that as red or as a missing trigger. It
recorded, in one `ci` object of `.legion/review.json`:

- `reviewedHeadSkipChecksTrailer: true` and `reviewedHeadCheckRuns: 0` — what the head itself has;
- `carriedFromHead: 9c868de6…` with `runsAtCarriedHead` (36779431175, 36779431349, 36779425901,
  36779425597) and `checkRunsAtCarriedHead` — where the green actually ran;
- `daemonChecksVerdict: "green"` with `how` naming the source: `legion read_record` showing
  `pullRequest.head 44e2179c` and `checksVerdict green`, plus `check-runs` at both heads.

Then its own `review: record handoff` push (verdict `approved`, so CI runs in full) made `ee54202a`,
which got four green runs of its own (36780436662, 36780436641 pull_request; 36780434090,
36780434173 push), and the one `APPROVE` names `ee54202a`. Rule for the next reviewer: at a
`skip-checks` head the daemon's issue record, not GitHub, holds the verdict; record the skipped
head and the source-of-green head **by role inside the same object** (the fresh-eyes reader's point:
`reviewedHead` at top level and `ci.carriedFromHead` nested, related only by the prose in `how`,
reads as a discrepancy to anyone skimming the JSON), and approve only the head your own push makes
once it has runs of its own.

## Criterion 2's review half: record the literal value, never a placeholder

Spec acceptance 2 wants `reviewDecision` `APPROVED`. Before the review exists it is the empty
string. This run every phase wrote the value it read: `.legion/implement.json` `proof[1].observed`
("reviewDecision is the empty string and the reviews list is empty — … only the checks half of
criterion 2 is proven here"), `.legion/test.json` `notExercised[1]` (the same, independently, at
`9c868de6`), `.legion/review.json` `observations[1]` ("becomes observable with this round's
APPROVE"). At `ee54202a` the read is `decision: APPROVED`, `APPROVED ee54202a… legion-reviewer[bot]`.
Nobody overclaimed and nobody wrote "pending"; the production check after the merge records the
final value. Keep this convention: a half of a criterion that is not yet observable is recorded as
the literal empty/false read plus the event that will make it true.

## The acceptance-3 ruling still has no owner — third run in a row

Run 5's document already gives the architect's fix (write acceptance 3 the way the deployment
instructions have it) and the planner's rule (a risk that is the architect's goes to the architect
with `envoy_publish` at plan time; a handoff field is a record, not a delivery). Both were unread
because the document was wiped before this run's planner started, and both were broken again:

- Spec v2 acceptance 3 again says the merged pull request changes no file other than the smoke
  file "(the temporary `.legion/` handoff files are deleted before the approving review)". Under
  this Go daemon nothing deletes `.legion/` before Stage 7. At the approved head `ee54202a`,
  `legion gh -- pr view 473 --json files --jq '[.files[].path]'` returns
  `[".legion/implement.json", ".legion/plan.json", ".legion/review.json", ".legion/test.json",
  "smoke/stage4b-legsmoke-362.md"]`. This retro commit adds `docs/solutions/` on top.
- `.legion/plan.json` `concerns[0]` names the conflict and recommends extending the spec's
  exemption to `docs/solutions/`; `implement.json`, `test.json` and `review.json` each note it and
  defer to "the architect's ruling". No ruling exists on the issue, in any handoff, or in this
  session's Envoy inbox. `routingHints.skipArchitect: true` is not the reason: that flag only means
  no sub-architect on a child; the root architect was live for the whole run. The question was
  never delivered to it.
- The plan's `tasks[T8]`, `acceptance[2].provenBy` and `evidence.review` again describe a
  `.legion/` deletion push and an approval of a `.legion/`-free head, which this daemon does not
  have before Stage 7. The reviewer's `observations[0]` names the actual rule ("the clean round is
  APPROVE at the head the review handoff push makes").

Every phase recorded the file list honestly as "filtered while `.legion/` handoffs are on the
head" and the merger's `jj diff --from ee54202a --to <tip> --summary` will show only this
document, so the merge gate is intact. What is not intact is the spec. The fix location is
whatever the root architect writes acceptance 3 from (the driver's issue template or its own
habit), because two spec versions on two runs have now reproduced the same sentence. Until it
lands, a worker who meets this conflict should cite the fixture pattern by commit (`proof fixture:
remove the handoffs and learnings … (#472)`, `(#469)`, `(#466)`, `(#462)`) instead of re-flagging
it as an open question — the repository's own history is the ruling nobody wrote down.

## Small facts that cost minutes

- `legion gh -- run list --repo <r> --commit <short-sha>` prints `[]` — silently, no error — for
  an abbreviated SHA. `--commit 9c868de6d758` gave `[]`; the same call with the full 40-hex
  `9c868de6d758688b42e7ce17b9e5d7c93e9ea5fa` gave the four runs. Always pass the full SHA.
- The byte check that works in the worker sandbox is a stdin pipe:
  `printf 'Stage 4b workflow proof: LEGSMOKE-362.\n' | cmp - smoke/stage4b-legsmoke-362.md`
  (silent on match). Run 5's `cmp - <(printf …)` fails there with `Illegal seek`; this run's plan
  used the pipe form and implementer and tester both ran it unchanged.
- `jj split -m … <path>` when the path is the working copy's only change prints
  `Warning: All changes have been selected, so the original revision will become empty` and names a
  `Remaining changes: <id> (empty)` revision. That revision is not left in the graph — jj drops it
  and `@` is a fresh empty working copy above the split commit. `jj log -r 'ancestors(@, 4)'`
  showed a clean chain both times; do not go looking for an orphan.
- `legion gh -- pr checks <n> --watch` lists each job twice (`check`, `check`, `gate`, `gate`):
  the `push`-event and `pull_request`-event runs of the same workflow, not duplicated checks. This
  run it returned in 32 s at both heads; run 5 saw one `pull_request` run queue for four minutes.
  The lag is the runner's, not the workflow's.
- Opening the pull request right after the code push (before the implement handoff push) makes
  `pr create` fire the two `pull_request` runs **at the code commit**, so `15195ac4` carried four
  runs and the E2E proof and the first `CI:` line both name it. The implement handoff push then
  moved the head to `9c868de6` with four more runs, and the `CI:` line was rewritten to that head
  while the `E2E (implementer)` line kept `15195ac4` — the shape `plan.json` `concerns[2]`
  predicted. Two `pr edit --body-file` calls per implementer round is the normal count.
- The Stage 4b driver's retro message says "write the required retro handoff". There is none:
  the `legion` tool's `handoff_write` has no `retro` phase, `.legion/` is not touched, and the retro's
  outputs are this commit, the Dispatch message on the issue, and `handoff_complete` alone.

## Threads line

The reviewer verified `reviewThreads(first:100) totalCount 0` at its head (`.legion/review.json`
`threads.how`) but did not carry that fact into the pull request body, which still read the
implementer's opening `0 resolved, 0 unresolved.` — accurate, unlike run 5's stale "no review has
been posted", but uncited. This retro refreshed the line (a body edit; the head is unchanged) to
name the GraphQL count at `ee54202a` and the `APPROVED` review, so the merger's
`legion threads resolve` output replaces a current line. Run 5's rule stands: the reviewer's round
updates the `Threads` line with what it verified.

## Index of the wiped learnings

| Run | Issue / PR | Merge commit on `main` | Document |
| --- | --- | --- | --- |
| 1 | LEGSMOKE-327 / #449 | `84dd473f1445` | `docs/solutions/legion/one-file-smoke-change-proof-under-the-go-daemon.md` |
| 2 | LEGSMOKE-331 / #452 | `436363445232` | `docs/solutions/legion/smoke-proof-learnings-after-the-fixture-reset.md` |
| 3 | LEGSMOKE-339 / #460 | `5d70c6381f65` | `docs/solutions/legion/smoke-proof-run-ids-pull-request-checks-and-the-deletion-lag.md` |
| 4 | LEGSMOKE-349 / #468 | `5e491eb2f19f` | `docs/solutions/legion/smoke-proof-learnings-fourth-run-one-proof-one-sha-and-the-index-of-wiped-learnings.md` |
| 5 | LEGSMOKE-358 / #471 | `ccd7fb9541c4` | `docs/solutions/legion/smoke-proof-learnings-fifth-run-wiped-index-head-vocabulary-and-the-undecided-risk.md` |
| 6 | LEGSMOKE-362 / #473 | (this pull request's merge) | this document |

Run 5's recovery command still finds them all:
`jj -R "$LEGION_WORKSPACE" log --no-graph -r 'ancestors(main@origin, 80) & files(glob:"docs/solutions/**")' -T 'commit_id.short() ++ " " ++ description.first_line() ++ "\n"'`.

## Timeline

Admission 21:03:51Z; plan handoff 21:13:32Z; code commit 21:23:15Z; pull request 21:24:02Z;
implement handoff 21:26:18Z; test handoff 21:30:54Z; review handoff 21:35:39Z; APPROVE 21:37:29Z.
33.6 minutes admission to APPROVE; each phase waited for the proof driver's targeted Dispatch
message before acting. Five pushes and twelve workflow runs for one line: none at `10c6254a`
(plan, `skip-checks`), four at `15195ac4`, four at `9c868de6`, none at `44e2179c` (test,
`skip-checks`), four at `ee54202a`. This retro commit is the sixth push and runs both event sets
again.

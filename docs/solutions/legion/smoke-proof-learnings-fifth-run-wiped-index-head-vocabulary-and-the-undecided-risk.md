---
title: "Smoke-proof learnings, fifth run: the wiped index is only in history, four names for \"head\", and a plan risk nobody decided"
category: legion
tags:
  - legion-workflow
  - proof
  - smoke-repository
  - github-actions
  - gh-cli
  - go-daemon
  - handoff-schema
date: 2026-09-30
status: active
module: legion-smoke
related_issues:
  - "LEGSMOKE-358"
  - "sjawhar/legion-smoke#471"
  - "LEGSMOKE-349"
  - "sjawhar/legion-smoke#468"
---

# Smoke-proof learnings, fifth run: the wiped index is only in history, four names for "head", and a plan risk nobody decided

Retro of LEGSMOKE-358 (Stage 4b proof tree 1: the whole workflow), delivered as pull request
sjawhar/legion-smoke#471, which adds `smoke/stage4b-legsmoke-358.md` holding the one line
`Stage 4b workflow proof: LEGSMOKE-358.` (39 bytes, LF-terminated). Fifth run of the same
one-file change under the Go daemon. Both proofs on #471 were read and are complete: the
implementer's `E2E (implementer)` line names the authenticated Contents API
(`legion gh -- api "repos/sjawhar/legion-smoke/contents/smoke/stage4b-legsmoke-358.md?ref=<sha>"`)
at the code commit `c3d0a577` with a `?ref=main` → 404 negative control; the tester's
`E2E (tester)` line names the same surface at the then pull-request head `6ac31cd3` with two 404
controls (`?ref=main`, `.fail-me` at the head) and records the re-run of the implementer's command
at `c3d0a577` (`.legion/test.json` `implementerProof.verdict: "verified"`). The reviewer reproduced
the read at `3d86183d` and approved `63c8d1c0` (its own `review: record handoff` commit) with four
green runs on it. One fresh-eyes scout read the four handoffs and the pull request independently;
its findings are integrated below and the generic ones dropped.

## The earlier runs' learnings are discoverable only through history; read them with one command

Run 4's retro (LEGSMOKE-349, #468) built an index of the wiped learnings so the next planner could
read them. That index was itself wiped by the fixture reset
(`proof fixture: remove the handoffs and learnings … (#469)`) before this run's planner started, and
the planner — who did glob the checkout for `AGENTS.md`, `.agents/`, `.omp/` and `SKILL.md` — found
nothing, because nothing is on `main`. The result is that two of run 4's rules were broken again
this run, exactly as run 4 predicted: the plan's proof commands use `cmp - <(printf …)` (refused as
`cmp: /dev/fd/N: Illegal seek` in the worker sandbox; implementer and tester each rewrote it to a
temp file and each recorded the substitution), and the plan's `summary`, `integrationEvidence`,
`tasks[T3].steps[3]` and `tasks[T5]` again describe a `.legion/` deletion push the Go daemon does
not have before Stage 7.

Rule for the next planner, before anything else:

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" log --no-graph \
  -r 'ancestors(main@origin, 80) & files(glob:"docs/solutions/**")' \
  -T 'commit_id.short() ++ " " ++ description.first_line() ++ "\n"'
```

Every `smoke: add … marker file (#N)` squash-merge it lists carried that run's retro document;
`jj -R "$LEGION_WORKSPACE" file list -r <sha> docs` names it and
`jj -R "$LEGION_WORKSPACE" file show -r <sha> <path>` reads it. The fixture-reset commits list
there too, as the deletions. Nothing else finds these documents: not `glob`, not a `main` checkout,
not Dispatch search (the retro messages on the earlier issues name the paths but not the SHAs).

| Run | Issue / PR | Merge commit on `main` | Document |
| --- | --- | --- | --- |
| 1 | LEGSMOKE-327 / #449 | `84dd473f1445` | `docs/solutions/legion/one-file-smoke-change-proof-under-the-go-daemon.md` |
| 2 | LEGSMOKE-331 / #452 | `436363445232` | `docs/solutions/legion/smoke-proof-learnings-after-the-fixture-reset.md` |
| 3 | LEGSMOKE-339 / #460 | `5d70c6381f65` | `docs/solutions/legion/smoke-proof-run-ids-pull-request-checks-and-the-deletion-lag.md` |
| 4 | LEGSMOKE-349 / #468 | `5e491eb2f19f` | `docs/solutions/legion/smoke-proof-learnings-fourth-run-one-proof-one-sha-and-the-index-of-wiped-learnings.md` |
| 5 | LEGSMOKE-358 / #471 | (this pull request's merge) | this document |

## Four handoffs, four meanings of "head", three field names

The fresh-eyes reader's strongest finding, confirmed against the files: `.legion/implement.json`
has `codeHead` (`c3d0a577`, the code commit) and no `head`; `.legion/test.json` keeps `codeHead`
and adds `head` (`6ac31cd3`, the pull-request head when the tester ran, i.e. the implement handoff
commit); `.legion/review.json` keeps `codeHead` and adds `reviewedHead` (`3d86183d`, the test
handoff commit the reviewer read), while the approval's `commit_id` is a fourth SHA, `63c8d1c0`,
the head the reviewer's own handoff push created — recorded only in prose in `decision`. The
pull request's live `headRefOid` is that fourth SHA. `baseCommit` appears in two of the four files
(prose in `plan.json`, bare SHA in `implement.json` — the reviewer's one Minor finding) and is
absent from the other two.

Each SHA is correct for its phase; the cost is on the reader. A merger taking "the approved head"
from `review.json` gets `reviewedHead` and computes its fingerprint from the wrong commit; the
right source is `legion gh -- pr view <n> --json reviews --jq '.reviews[].commit.oid'`, as run 4
already said. Rule for every handoff on this project, so the four files read as one record:

- `codeHead`: the last commit that touches a non-handoff file (the implementer's). Same value in
  every later handoff; the `E2E (implementer)` line leads with it.
- `head`: the pull-request head the phase's proof commands were run against, i.e. the head before
  the phase's own handoff push. The `E2E (tester)` line leads with the tester's `head`.
- The approved head has no handoff field: the review handoff is committed and pushed before the
  approval exists, so the reviewer cannot record its SHA without guessing. `review.json`
  `decision` names how it is derived ("the head this handoff push creates"), and the merger reads
  the SHA from GitHub. Do not add a field that would have to be guessed.
- `baseCommit`: a bare 40-hex SHA or absent; the annotation the planner wanted ("main == main@origin
  at planning time") goes in a separate prose field.

This run's `E2E` heads did follow run 4's rule: `implement.json` `proof[0].head`, the
`E2E (implementer)` lead SHA and the tester's `implementerProof.how` all name `c3d0a577`; the
tester's `proof[].head` and `E2E (tester)` lead SHA both name `6ac31cd3`. Nobody had to reconcile
two SHAs for one proof.

## A plan risk marked "ARCHITECT DECISION NEEDED" reached nobody

`.legion/plan.json` `risks[0]` names a real conflict: spec v2 acceptance 3 says the merged pull
request changes no file other than `smoke/stage4b-legsmoke-358.md` "(the temporary `.legion/`
handoff files are deleted before the approving review)", while the deployment instructions keep
`.legion/` on the approved head until Stage 7 and `skill://legion-retro` lands this document above
it. The planner recommended treating both as workflow overhead outside the "Nothing else changes"
rule and asked for an architect decision. `implement.json`, `test.json` and `review.json` each
carry `openChoiceForArchitect: "… takes no position"`. No handoff, review body or Dispatch message
records a decision, and the merger's READY on #471 will quote a `jj diff --from 63c8d1c0 --to
<tip> --summary` that lists this file. Four roles saw the risk; the one who owned it never received
it as a question.

Two rules. For the planner: a risk whose resolution is the architect's goes to the architect with
`envoy_publish` to its role topic at plan time, carrying the facts and the recommendation — a
handoff field is a record, not a delivery. For the root architect on this project: write acceptance
3 as the deployment instructions have it — the merged pull request adds the smoke file, the
`.legion/` handoffs and the retro's `docs/solutions/` document, and the operator's follow-up
removes the last two (the `proof fixture: remove the handoffs and learnings` commits on `main` are
that follow-up) — so no phase has to choose between the spec and its standing rules. Run 4's spec
had dropped the deletion from acceptance 2; this run's spec brought it back in acceptance 3 and in
Design, so the fix belongs in whatever the architect writes the spec from, not in one spec.

## The pull request body's `Threads` line is a snapshot that goes stale at the approval

The implementer writes `**Threads:** 0 resolved, 0 unresolved … legion threads resolve not yet run:
no review has been posted` when the pull request opens, and no phase's charter names that line
again until the merger runs `legion threads resolve` before READY. On #471 the line read "no review
has been posted" under an `APPROVED` review for 16 minutes. The fresh-eyes reader called it
misleading for anyone who skims the Verification block instead of the Reviews list; it is. Retro on
this run refreshed the line (edit to the body only; the head is unchanged) to name the GraphQL
`reviewThreads.totalCount 0` at `63c8d1c0` and the approval. Rule: the reviewer's round updates the
`Threads` line with the thread count it verified at its head, so the merger's `legion threads
resolve` output replaces a current line rather than a stale one.

## Small facts that cost minutes

- `legion gh -- pr checks <n> --watch` piped through `sed` prints nothing until every check ends;
  the `pull_request`-event `ci` run at `6ac31cd3` (36766808821) sat queued for about four minutes
  after the `push`-event runs had passed. `legion gh -- run watch <run-id> --exit-status
  --interval 10` on the one queued run is the wait that returns; `pr checks` exit 8 is pending,
  not failure (run 3 and run 4 said the same).
- `pr create` starts the two `pull_request`-event runs, so the `CI:` line written before it can
  cite only the `push` runs; the sequence that worked here and on #468: body with push runs →
  `pr create --body-file` → `run watch` → `pr edit --body-file` with all four ids →
  `handoff_complete`.
- The reviewer's handoff push runs full CI (`verdict: "approved"`), so the approved head carries
  four green runs of its own (36768133749, 36768133773 push; 36768138415, 36768138492
  pull_request); the tester's `test.json`-only push carries `skip-checks: true` and `pr checks`
  prints `no checks reported` there — by design, not a missing trigger.

## Timeline

Admission 19:21:00Z; plan handoff 19:28:24Z; code commit 19:34:15Z; implement handoff 19:35:17Z;
pull request 19:36:03Z; test handoff 19:42:32Z; review handoff 19:47:37Z; APPROVE 19:52:10Z.
31.2 minutes admission to APPROVE; each phase waited for the proof driver's targeted Dispatch
message before acting. Five pushes and ten workflow runs for one line: none at `2205cd9b` (plan,
`skip-checks`), two at `c3d0a577`, four at `6ac31cd3`, none at `3d86183d` (test, `skip-checks`),
four at `63c8d1c0`. This retro commit is the sixth push and runs both event sets again.

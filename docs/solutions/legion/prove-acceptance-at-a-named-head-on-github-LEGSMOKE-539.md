---
title: "Prove a pull request's acceptance at a named head, not at the pull request"
category: legion
tags:
  - proof
  - github-api
  - negative-control
  - skip-checks
  - ci-settle
date: 2026-10-08
status: active
module: legion-workflow
related_issues:
  - "LEGSMOKE-539"
  - "sjawhar/legion-smoke#527"
---

# Prove a pull request's acceptance at a named head, not at the pull request

Record each proof at the SHA the command actually reads. `legion gh -- api repos/<o>/<r>/pulls/<n>/files`
and `legion gh -- pr checks <n>` read the pull request's tip at query time, whatever `headSha` the proof
entry names; `contents/<path>?ref=<sha>`, `run list --commit <sha>` and `compare/<base>...<sha>` read the
SHA. For a head-specific claim about which files change, use `compare/<base>...<sha>` (its `files[]`
carries `status`) or the contents API by path at that SHA, and say so in the entry.

Read file bytes with `-H "Accept: application/vnd.github.raw+json"`. Never `--jq .content | base64 -d`
for a claim about bytes or line endings.

Settle CI in one bash call with `legion gh -- pr checks <n> --repo <o>/<r> --watch --fail-fast`, then
list `legion gh -- run list --repo <o>/<r> --commit <sha> --json name,event,status,conclusion,databaseId`
and judge "every run at the SHA is `completed`/`success`, with at least one run of each required
workflow". Never assume a fixed run count: a push and a pull request each start a run.

Name every head that will carry `skip-checks: true` (the planner's, the tester's and a
changes-requested reviewer's handoff commits) in the plan and the handoffs. `run list --commit` returns
`[]` at those heads by design; judge CI at the nearest code-bearing head and say which one.

Make each negative control exercise the command under test. A control for a jq filter over
`pulls/<n>/files` is another pull request the filter must flag; a control for a raw-bytes read is a ref
where the file is absent (HTTP 404) or holds the previous state. A 404 on a different API proves nothing
about the filter.

When an outside party's commit turns CI red on the branch, keep that head as the in-branch negative
control for the CI criterion, and prove the fix by path at the new tip (`contents/<path>?ref=<tip>` →
HTTP 404), because the pull request's net file list hides a file added and then deleted.

## Evidence

- `.legion/LEGSMOKE-539/implement.json` records the same `pulls/527/files` filter three times with
  three different `headSha` values (21de6427, 728ac362, 9525be6b) and three different file lists; the
  output tracked the branch tip, not the recorded SHA. `test.json` `nextPhaseNotes.reviewer` says it
  plainly: "criteria 1-3 are head-independent for the files API".
- `plan.json` `gapAnalysis` pinned the raw media type after finding that `--jq .content | base64 -d`
  can mangle the bytes criterion 2 is about; every phase's criterion-2 entry then read 56 bytes / 1 line
  and later 95 bytes / 2 lines with `wc -c`, `wc -l`, `od -c | tail -2`.
- Skip-checks heads c54be51d (plan), 9e9b20c4 and dff45875 (test), c781cbe9 (review, changes
  requested) each returned `[]` from `run list --commit`; the implementer, tester and reviewer each
  judged criterion 4 at the adjacent code head (21de6427, 728ac362, a35722c7, 9525be6b, 2fbb4a1e,
  66198f6e), all four runs `success`.
- Criterion 1's recorded control (`contents/smoke/LEGSMOKE-539.md?ref=main` → 404) does not exercise
  the files filter; the discriminating control (`pulls/112/files` → `modified README.md`) sat on
  criterion 3 only. The next plan attaches the filter's own control to the filter's criterion.
- The proof driver's commit 27072ca6 added `.fail-me` (25 bytes) and `fail-on-demand` failed on both
  events (runs 37836509019, 37836500590); 728ac362 removed it. From then on every phase used
  `run list --commit 27072ca6` as the criterion-4 control and `contents/.fail-me?ref=<tip>` → 404 as the
  proof of removal, replacing the planner's historical control from another branch (run 37792447133).

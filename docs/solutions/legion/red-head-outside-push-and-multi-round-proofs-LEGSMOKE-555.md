---
title: "Curing an outside red head and re-stamping proofs across Legion rounds"
category: legion
tags:
  - legion-implementer
  - red-head
  - outside-push
  - handoff-schema
  - pr-body
  - proof-commands
date: 2026-10-09
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-555"
  - "sjawhar/legion-smoke#530"
---

# Curing an outside red head and re-stamping proofs across Legion rounds

Rules for a Legion phase worker whose pull request goes red on a commit it did not make, and
whose proofs must then be re-run at a new head more than once. Each rule is one the LEGSMOKE-555
run exercised; the incidents are in Evidence.

## Read the tree before you cure a red head

- First two reads, both at the full 40-character head SHA: `contents/<gate-file>?ref=<head>`
  and `commits/<head>` (author login, committer email, parents). HTTP 200 on the gate file is
  tree-caused: cure it. HTTP 404 is a flake: `legion gh -- run rerun <run-id> --failed`, push nothing.
- An author that is not a Legion role App is an outside push. Record it in `rounds[]` with
  `kind: ci-red` and the commit's identity, report it to the architect, and never describe the
  commit as your own.
- Cure from the remote tip (`jj new legion/<KEY>@origin`) with one deletion-only commit
  (`jj diff --summary` shows only `D <gate-file>`). Leave the outside commit in the chain: no
  rewrite, no abandon.
- Every proof recorded at a head that is no longer in the judged chain is re-run and re-stamped at
  the cured head; the superseded proof stays in the earlier `rounds[]` entry, not in `proof[]`.

## Prove "nothing else changed" with a contents read at the head

- The pull request files API compares base to head, so an add-then-remove pair inside the branch
  is invisible there. Assert a file's absence with `contents/<path>?ref=<head>` → HTTP 404, and
  its presence at the red head as the negative control (`{path, size}`).
- The files API takes no `ref` and answers for whatever the head is when called. A criterion that
  reads it records the head at the time of the call; the pinned surface for the same claim is the
  compare URL `compare/<base>...<head>`.

## Write proof commands a stranger can re-run

- When a criterion has arms, the recorded command prints the discriminant before it selects the arm
  (`echo "K=$K"`), so a re-run shows the input as well as the output.
- The negative control uses the same paths and variables the command creates. A control that names
  a file the command never writes is not reproducible as written.
- `run list --commit` needs the full SHA: a short SHA returns `[]`, indistinguishable from a
  skip-checks head. `[]` at a code head means "not yet judged", never a pass; assert the shape
  (non-empty, every run `completed`/`success`, at least one of each gate workflow), never a count.
- Settle checks (`pr checks --watch --fail-fast`) in a bash call of its own and read `run list`
  in a fresh call: a credential grant is written per bash call and lives 60 seconds.

## Record a later round in the implement handoff

- `deviations[]` entries are strings. `legion handoff write` refuses an object there
  (`deviations.0: Invalid input: expected string, received object`). Put the structured record
  (SHA, author, runs, parent) in the round's `rounds[]` entry and summarise it as one string in
  `deviations[]`.
- Build round N's handoff from the file on disk: read `.legion/<issue>/implement.json`, delete
  `schemaVersion`, `phase`, `completed` and `issue`, mutate `proof[]` and append to `rounds[]`,
  then pipe the JSON to `legion handoff write --phase implement`. Re-typing the payload loses the
  earlier rounds.

## Co-edit the pull request body without clobbering another role

- Re-fetch the live body immediately before each edit; change only your own lines; PATCH with
  `-F body=@file`; read back with `--jq .body | diff`. Exactly one hunk, jq's trailing newline, is
  expected; anything else is drift from another role's edit and the edit starts over.
- Each read-back-then-PATCH cycle appends one blank line to the body. Trim trailing blank lines
  before the PATCH (`printf '%s\n' "$(cat body.md)"`), or the body grows a blank line per round.
- Stamp `Size` with the SHA GitHub's counts were read at and refresh it after the round's last push
  (the handoff commit changes the counts). A `Size` stamped at the round's last-push head is
  current; one stamped earlier is a Fast-follow item, not drift.

## A review decision is a predicate, not a review id

- Gate a conditional content change on a machine-checkable predicate over the inline comments
  (`path`, `in_reply_to_id == null`, the reviewer App's login, body contains the exact literal),
  never on a review's id or state. A `COMMENT` review that decides nothing changes no byte, and a
  second review on the same head carrying the comment flips the predicate.
- Append only the literal the plan fixes, never the comment's own wording. A comment asking for
  other wording is a mismatch: append nothing, reply quoting the literal, report to the architect.

## Evidence

- Outside push: `41a3a2817c1423c09df6590ffb8a575666dd03dc` `Stage 4b proof: fail-on-demand`,
  author `sjawhar-agent[bot]`, committer `noreply@github.com`, parent the implement handoff
  `e50d2691`, adds `.fail-me` (25 bytes). `fail-on-demand` failed (runs 37879672357, 37879667414),
  `ci` passed. Cure `f2d033ce95889ae7111a99b07b9b2b4e4ea1006a` `smoke: remove .fail-me so
  fail-on-demand passes again`, `D .fail-me` only; four runs green there. The files list at the
  cured head showed only `added smoke/LEGSMOKE-555.md`; `contents/.fail-me?ref=f2d033ce` → 404,
  `?ref=41a3a281` → `{path: .fail-me, size: 25}`.
- Handoff schema refusal on the first ci-red handoff write; the string form was accepted. The
  round-2 handoff was built from disk with a 60-line bun script and kept `rounds[0..1]`.
- Review round 1: review 5465567035 was a `COMMENT` on head `25a5aa97` and moved nothing; review
  5465575671 (`CHANGES_REQUESTED`, same head) carried inline comment 4226491220 on
  `smoke/LEGSMOKE-555.md:1` quoting `Legion proof review round 1 (LEGSMOKE)`; predicate K went
  0 → 1 and the file went 56 bytes / 1 line → 95 bytes / 2 lines in `6741be7bf91bddb2adfa9ee36698f04ee3daff9f`.
- PR #530's body carried eight trailing blank lines after three roles' PATCH cycles.
- The implementer's recorded criterion-2 command computed K without printing it and hard-coded
  the `MATCH-2LINES` arm; its control named `/tmp/l555/f3` while the command wrote `$T/f`. The
  tester's command printed `K=$K`. Both findings came from the retro's fresh-eyes read and the
  reviewer's round-1 notes; the C5 re-proof line prints K and uses one path.
- Short-SHA control: `run list --commit 6741be7b` → `[]`; `--commit 6741be7bf91b…` → 4 runs.

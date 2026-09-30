---
title: "Proving a docs-only pull request on GitHub through Legion phases"
category: legion-workflow
tags:
  - legion
  - proof
  - github-actions
  - handoff
  - negative-control
  - gvisor
date: 2026-09-30
status: active
module: smoke
related_issues:
  - "LEGSMOKE-366"
  - "sjawhar/legion-smoke#475"
---

# Proving a docs-only pull request on GitHub through Legion phases

Learned on LEGSMOKE-366 (Stage 4b proof tree 1), pull request sjawhar/legion-smoke#475: one new
file `smoke/stage4b-legsmoke-366.md`, one line, proved on GitHub by the implementer, the tester,
and the reviewer through `legion gh --`. Everything below was observed in that run; anything
inferred is marked.

## The implement handoff cannot name its own head: prove at the code commit, re-check at the handoff head

The implement handoff's `proof` array must name the head SHA the proof ran at, but the commit
that carries `.legion/implement.json` becomes the new PR head the tester sees, and `legion push`
does not skip CI for it (its skip allow-list is `plan.json`, `test.json`, and a
`changes_requested` `review.json` only). So the proof in the handoff names the code commit
(`e23e374e`), and after the handoff push the implementer re-runs the same commands at the new head
(`259bc373`) and edits only the PR body's `CI` and `E2E (implementer)` lines to name both heads.
Write a `notesForTester` field predicting the head and the diff paths the tester will see; the
tester's handoff recorded that the prediction matched exactly, which is what let it verify rather
than re-litigate.

## Handoff paths pollute "exactly one path": resolve once in the plan, quote it in every phase

Legion commits `.legion/<phase>.json` onto the branch under review, so `gh pr diff --name-only`
prints handoff paths beside the real change until the `.legion/` deletion (under the Go daemon,
after the merge). The plan's `risks` pre-resolved it: implementer and tester record criterion 1 as
"the smoke file plus `.legion/` handoffs only", and the exact single-path check belongs to the
reviewer at the `.legion/`-deleted head. All three later handoffs quoted that sentence; no phase
passed the criterion early or failed it spuriously. Do the same for any process that writes files
onto the branch (handoffs, generated lockfiles, bot commits): decide in the plan which head is
authoritative for a literal "N files changed" check.

## Independent tester evidence is a different endpoint at a different granularity

Re-running the implementer's `contents?ref=<head> | base64 -d | od -c` confirms; it is not new
evidence, since `pr diff` and the contents API read the same objects. What was independent:
`gh api repos/<owner>/<repo>/commits/<code-sha> -q '.files[]'` (the code commit alone: exactly one
file, +1/-0, unaffected by handoff churn on top) and
`gh api repos/<owner>/<repo>/compare/main...<head>` (base-to-tip via a different endpoint than
`pr diff`). A tester should add at least the `commits/<code-sha>` call so the "one file" claim
survives every later handoff commit.

## `on: [push, pull_request]` gives four check runs per head, and a `skip-checks` head gives zero

Both workflows here fire on `push` and `pull_request` with no branch filter, so every head shows
two `ci/check` runs and two `fail-on-demand/gate` runs. Name all four run ids with their event on
the CI line (`gh run list --repo <r> --commit <sha> --json databaseId,name,event,conclusion`), or
a reader cannot tell two missing runs from a repository that only ever produces two. `ci` sleeps
20 seconds, so `statusCheckRollup` read straight after a push shows `PENDING`: settle with
`gh pr checks <n> --watch` before writing a verdict anywhere.

The tester's handoff-only push carried GitHub's `skip-checks: true` trailer (added by
`legion push`), so `run list --commit <tester-head>` was empty. The reviewer did not read that as
"CI missing": it recorded `reviewedHeadCheckRuns: 0`, `reviewedHeadSkipChecksTrailer: true`, and
`carriedFromHead: 259bc373` with that head's four runs. Before treating a zero-run head as red or
absent, read the commit's trailers (`gh api repos/<r>/commits/<sha>`), confirm it touched only
handoff paths, and walk back to the nearest ancestor that ran CI.

## Two absence-check negative controls, one of which never exercises the red path

`contents/<file>?ref=main` returning HTTP 404 is a real control for the contents check: it shows
the endpoint distinguishes presence from absence, so the 200 at the head is not a permissive
endpoint. The second control, "`.fail-me` is absent and `fail-on-demand.yml`'s only red path is
`if [ -f .fail-me ]; then exit 1`", is an inference from reading the workflow source plus an
absence check; no phase created `.fail-me` and watched the gate turn red, and the spec forbids
creating it on this branch. A workflow that always exits 0 would pass every proof in this chain.
When a red path is inferred from source rather than executed, say so in the proof line ("inferred
from workflow source, not triggered") instead of presenting it as equal to the executed control.
[inferred] On a disposable sandbox a one-time calibration branch that creates `.fail-me` and
observes the red run would close the gap; that is a separate issue, not a change to a reviewed PR.

## Two shell traps in this sandbox

- `gh api …?ref=main` exits 1 on the 404 you are hoping for, so a negative control placed inside
  an `&&` chain aborts the chain. Run it as its own command with `-i` and read the status line.
- Under gVisor, `cmp <(sed 's/362/366/' precedent.md) new.md` fails with
  `Illegal seek (os error 29)`: `cmp` seeks on the `/dev/fd` pipe. Use
  `sed 's/362/366/' precedent.md | cmp - new.md` (exit 0, prints nothing) or compare `sha256sum`
  digests. The inverse, `cmp precedent.md new.md` differing at the key's character, is a cheap
  control that the byte comparison would catch a wrong key.

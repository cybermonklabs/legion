---
title: "Proof records the next role can re-run verbatim: literal commands, one fetch per check set, and the blob SHA as the head-drift bridge"
category: legion
tags:
  - legion-worker
  - implementer
  - tester
  - reviewer
  - handoff-ledger
  - proof
  - negative-controls
  - blob-sha
  - head-drift
date: 2026-09-26
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-288"
  - "sjawhar/legion-smoke#346"
---

# Proof records the next role can re-run verbatim

Distilled from PR #346 (issue LEGSMOKE-288, "Stage 4b proof tree 1: the whole workflow"): one new
one-line file `smoke/stage4b-legsmoke-288.txt`. The tester re-ran the implementer's three proof
commands from its own session and compared output line for line; the reviewer's code-quality pass
then read both ledgers. Everything below is what those two readers needed and did not always get.

## A `command` field is pasted and run, never read as narrative

`.legion/implement.json` `proof[1].command` on #346 ends
`... --jq .content | base64 -d; same | wc -l; jj -R "$LEGION_WORKSPACE" file show ...`. The word
`same` is prose: it tells a human "the previous pipeline again" and tells a shell nothing. The
reviewer's `thermonuclear-code-quality` pass at `170f1980` reported it as the round's only Minor
("prose rather than a re-runnable shell command") and it became the PR's `Fast-follow` line. The
tester's `.legion/test.json` `proof[1].command` for the same criterion is the form to copy:

```bash
legion gh -- api "repos/<owner>/<repo>/contents/<path>?ref=<head-sha>" --jq .content \
  | base64 -d | tee /tmp/c.txt
wc -l < /tmp/c.txt
od -c /tmp/c.txt
grep -qx '<the exact expected line>' /tmp/c.txt; echo $?
```

One credentialed fetch, then every check runs against the saved bytes: line count, byte-level
newline shape, exact-line match. It stays inside the 60-second `legion gh` grant however many
checks follow, and each check is a separate observable the tester can diff against yours.

Rule: before `handoff_write`, read every `proof[].command` as a shell would. A back-reference
("same", "as above", "the previous command"), an unexpanded placeholder, or a `<head>` you meant
to substitute is a defect the reviewer will find in the ledger you can no longer edit (the
`.legion/` files ship to review; on this fixture they even shipped to `main` under the operator's
approval override — see
`docs/solutions/legion/operator-directed-approval-on-a-ledger-carrying-head.md` at commit
`3dba0d427577`).

## The blob SHA bridges head drift for a single-file change

Three heads carried the same product bytes: `80f8dc70` (the product commit),
`0507815f` (`implement: record handoff`), `170f1980` (`test: record handoff`, approved). The
implementer's `E2E` line names the product head; the reviewer approves the current one. The
`skill://legion-worker` fingerprint proves the two equal, but for a one-file change there is a
cheaper fact that any reader can check from GitHub alone: the blob SHA.

- Contents API: `legion gh -- api "repos/<o>/<r>/contents/<path>?ref=<head>" --jq .sha`
  printed `9a0b777f5cf5545d971786dbd1988b73a47e65e1` at both `80f8dc70` and `0507815f`
  (`.legion/test.json` `proof[1].observed`).
- Workspace: the `index 0000000000..9a0b777f5c` line of
  `jj -R "$LEGION_WORKSPACE" diff --from main@origin --to <head> --git smoke/stage4b-legsmoke-288.txt`
  is the same id.

The tester recorded it unprompted and wrote in `forReviewer`: "Product content is byte-identical
at every head on the branch (blob 9a0b777f…)". The reviewer then cited the same blob at
`170f1980`. Record the product blob SHA in the implementer's proof and in every later role's
observation; when they match, "verified at `<productHead>`, identical at `<current head>`" is a
one-line fact, not a paragraph.

## Negative controls that discriminated on a new-file add

`docs/solutions/legion/operator-gated-phases-credential-grants-and-honest-controls.md` (commit
`1aec4effdc93`, from PR #319) lists the controls that flip the verdict on a one-line *append*.
The set that did the same for a new *file* on #346, used identically by implementer, tester and
reviewer:

1. The same contents-API read at `?ref=main` → HTTP 404, and
   `jj -R "$LEGION_WORKSPACE" file show -r main@origin <path>` → `No such path`, both exit 1.
   The file does not exist before the change, so the head read cannot pass by inheritance.
2. `grep -qx 'Stage 4b workflow proof: LEGSMOKE-278.'` (the neighbouring fixture's line) against
   the decoded head content → exit 1, while the LEGSMOKE-288 line → exit 0. A copied or
   mis-keyed sibling file is the live failure mode when proofs share a directory.
3. `.fail-me` at `?ref=<head>` → HTTP 404, cited together with the workflow line that makes it
   matter (`.github/workflows/fail-on-demand.yml`: `if [ -f .fail-me ]; then exit 1; else exit 0; fi`).
   Without that citation a green `gate` job is just a green job; with it, the control shows the
   gate's only failing input is absent, so its success is not vacuous.

The tester added one more: `compare/main...0000000000000000000000000000000000000000` → HTTP 404.
That one exercises GitHub's SHA validation and would return 404 whether or not the file existed;
by the #319 rule (a control is an input that would flip *this* verdict) it is ritual. Keep the
three above; drop the null-SHA compare.

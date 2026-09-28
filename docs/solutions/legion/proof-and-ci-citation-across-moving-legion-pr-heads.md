---
title: "Proof and CI citation across moving Legion pull-request heads"
category: legion
tags:
  - e2e-proof
  - negative-control
  - github-actions
  - ci-citation
  - handoff
  - pr-body
date: 2026-09-28
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-310"
  - "sjawhar/legion-smoke#424"
---

# Proof and CI citation across moving Legion pull-request heads

## The shape of a Legion pull request

Four phases each pushed one commit to `legion/LEGSMOKE-310`: the product commit, then
`implement: record handoff`, `test: record handoff`, `review: record handoff`. Each push moved the
PR head and triggered a complete set of Actions runs — here two workflows, each on `push` and
`pull_request`, so four runs per head and sixteen for a one-line file. That is by design; the
plan's risk list forbids rebasing to "refresh" CI, and `skill://legion-worker` allows a rebase only
on a real conflict.

## Citing CI

- Cite CI **at the exact head you verified**, by workflow name and run id, in the PR body's
  `CI` line — the product head (proves the product) and your own ledger head (proves the head the
  next role reads). Filter to the head, never to "latest":

  ```bash
  legion gh -- run list --repo <owner>/<repo> --branch legion/<KEY> \
    --json name,event,status,conclusion,headSha,databaseId \
    --jq '.[] | select(.headSha=="<head-sha>") | "\(.name) \(.event) \(.status) \(.conclusion) \(.databaseId)"'
  ```

- **Job names are not workflow names.** `gh pr checks` and `statusCheckRollup` list jobs
  (`check`, `gate`); the spec and `gh run list` name workflows (`ci`, `fail-on-demand`). Map
  them once in the plan (`.legion/plan.json`, `risks[2]`) and cite workflows with run ids.
  Every later phase on LEGSMOKE-310 re-derived this mapping in its own notes; one record suffices.

- **Open the pull request immediately after the first push.** `push`-event runs start on the
  push; `pull_request`-event runs start only when the PR exists. On LEGSMOKE-310 the push runs had
  finished before `pr create` returned, and the pull_request runs then cost a second wait.

- **Watch one run per bash call.** The `legion gh` grant lives 60 seconds per bash call, so
  `legion gh -- run watch <run-id> --repo <owner>/<repo> --exit-status` runs as its own call and
  never inside a polling loop. `legion gh -- pr checks <n>` exits 8 while any run is pending and
  0 once all pass.

- Anchor identity to what does not move: the product blob sha (contents API `.sha`) or the
  unchanged-diff fingerprint, not the head SHA. See
  `acceptance-criteria-over-the-product-diff-on-ledger-carrying-branches.md`.

## The proof entry that three phases reused

Each criterion's proof carried the same five fields, in `.legion/implement.json` `proof[]`, in
`.legion/test.json` `proof[]`, and in `.legion/review.json` `verifiedOnGitHub`:

| field | rule |
| --- | --- |
| `surface` | the user-facing surface — here GitHub's compare API, contents API, and Actions runs on the sandbox repository, cross-checked from the jj workspace |
| `command` | exact and re-runnable; the tester re-ran it verbatim, so prose such as "`same \| wc -l`" (the LEGSMOKE-288 fast-follow) is a defect |
| `observed` | the literal output, including exit codes and byte counts (`od -c` → 39 bytes ending in one `\n`) |
| `headSha` | the full SHA the command ran against |
| `negativeControl` | a deliberately wrong input and the refusal seen |

Negative controls that transferred cleanly and would again on any fixture-style change:

- the new path at `?ref=<base>` → HTTP 404, and `jj file show -r main@origin <path>` →
  `No such path` (the file did not exist before the branch);
- the compare filter at the base's tip → prints nothing;
- `grep -qx '<neighbouring fixture's line>'` on the decoded content → exit 1 (the right key,
  not a copied one);
- the gate's trigger absent: `contents/.fail-me?ref=<head>` → 404, so `fail-on-demand`'s success
  is not vacuous (`.github/workflows/fail-on-demand.yml` exits 1 iff `.fail-me` exists).

When a precedent fixture exists, copy it byte-for-byte and prove it: `od -c` both files and
expect only the key to differ (`smoke/stage4b-legsmoke-288.txt` vs `-310.md`, 39 bytes each).

## Duplicate the verification, cite the description

The tester and reviewer must re-run the implementer's commands themselves; that duplication is
the point. But `productHead`, `productBlobSha`, the `pullRequest` block, and the negative-control
prose were retyped near-verbatim in three handoffs. Later phases should record their own
`observed` and `verdict` and refer to the implementer's proof by criterion id
(`implementerProof.how` does this well in `.legion/test.json`), not restate the description.
[inferred — this would be a handoff-schema convention, not something one worker can enforce]

## PR body and handoff mechanics that held up

- **Write the PR body in READY format from `pr create`,** with `pending` placeholders for `CI`,
  `E2E (implementer)`, `E2E (tester)`, and `Production`, and replace one line at a time with
  `legion gh -- pr edit <n> --body-file <file>`. Save the body before each edit and diff it after,
  so the handoff can say exactly which line changed (`.legion/test.json`, `prBodyUpdated.how`).
- **A human's targeted proof message can arrive mid-phase.** LEGSMOKE-310's arrived while the
  implement handoff was being written. Record its id in the handoff by amending, not re-emitting:

  ```bash
  cd -- "$LEGION_WORKSPACE" && bun -e 'const h = await Bun.file(".legion/<phase>.json").json(); delete h.schemaVersion; delete h.phase; delete h.completed; h.humanProofMessage = "<id> (<time>, <actor>): <instruction>"; console.log(JSON.stringify(h))' | legion handoff write --phase <phase>
  ```

- **`jj split -m "<message>" <paths>` of the whole working-copy diff** leaves the described commit
  at `@-` and a fresh empty `@` — exactly the shape the one push procedure needs
  (`bookmark set -r @-`). jj's warning "All changes have been selected, so the original revision
  will become empty" is expected, not a fault.
- **Approval on a ledger-carrying head is the Go daemon's rule, not an override.** Its role text
  says the reviewer approves a head that still carries `.legion/` and the operator removes the
  ledger from main after the merge; the merger's READY packet must say the merge lands
  `.legion/*.json` on main (`.legion/review.json`, `forMerger`). A worker whose skill text says
  "approve only the `.legion/`-deleted head" follows the daemon's role text where the two differ.

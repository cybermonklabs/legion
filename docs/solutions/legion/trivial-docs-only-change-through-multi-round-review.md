---
title: "Driving a trivial docs-only change through multi-round Legion review"
category: legion
tags:
  - legion-workflow
  - implementer
  - docs-only
  - review-rounds
  - jj-workspace
  - credential-grant
date: 2026-09-27
status: active
module: legion-worker
related_issues:
  - "S392670114-1"
  - "sjawhar/legion-smoke#374"
---

# Driving a trivial docs-only change through multi-round Legion review

Reusable patterns from S392670114-1 (PR sjawhar/legion-smoke#374): a single Markdown
marker file `smoke/S392670114-1.md` that grew one appended line per review round across
three `CHANGES_REQUESTED` rounds, then was approved. Docs-only, so `ce-simplify-code` and
the thermonuclear pair never ran.

## Scope discipline for per-round corrections

- **Quote the reviewer literally.** Each review body asked to append the exact line
  `Stage 3 review round N (S392670114)` "below the lines already there, and change nothing
  else". Writing that string byte-for-byte makes the correction trivially in-scope and
  auto-verifiable — no paraphrasing.
- **Pre-authorize the append in the plan.** `plan.json` AC2 allowed "one appended line and
  nothing else" per round, with a negative control that a second line or an edit outside
  `smoke/` is rejected. Because the append path was planned up front, three rounds of
  corrections never forced a re-plan.
- **Negative control must filter process artifacts.** The PR "Files" list carries the
  product file **and** every `.legion/*.json` handoff. Prove "no stray product files" with
  `legion gh -- pr view <n> --repo <o>/<r> --json files --jq '.files[].path | select((startswith(".legion/")|not) and (startswith("smoke/")|not))'`
  → empty. This is also the fileset the fingerprint excludes.

## Head-SHA discipline across advancing heads

- **Two pushes per round.** The product append lands as one commit; the phase handoff
  (`.legion/<phase>.json`) lands as a **separate** commit on top and is pushed again. So the
  branch head legitimately advances (implement 9c969b77 → test afdd8bba → review 4deb6033)
  while the product file is frozen. Never treat a moved head as a product change until you
  diff it head-to-head; approval proves the product unchanged by showing
  `diff <proof-head>..<approval-head>` touches only `.legion/` paths.
- **CI runs on the tip, which is the handoff commit.** After the second push, cite the CI
  run at the handoff-commit head in the PR body's `CI` line — not the product-commit head —
  because that is the head the reviewer verifies.
- **Source the full SHA from jj, never hand-type it.** A handoff's `headSha` and `proof[].headSha`
  must be the real 40-char commit id from
  `jj -R "$LEGION_WORKSPACE" log --no-graph -T 'commit_id' -r 'legion/<KEY>'`.
  A fabricated SHA tail slips past `handoff_write` schema validation (it only checks the
  field is a non-blank string) but is a false record — read it back and correct it.

## Declare, don't silently skip

- **Docs-only skips are stated everywhere.** Record "docs-only diff; no runtime code, so
  `ce-simplify-code` and the thermonuclear pair do not run" in the plan concerns, the
  implement handoff `thermo` field, and the PR body `Thermo` line, so a fresh reviewer sees
  the skip is deliberate, not forgotten.
- **Body-only findings → assert the 0/0 thread state.** When the reviewer files findings in
  the review body with no inline threads, `legion threads resolve` prints `no unresolved
  threads`; state explicitly in the PR body that the reviewer opened no inline threads and
  each finding was fixed by a named commit, so `0 resolved, 0 unresolved` doesn't read as
  unhandled work.

## Daemon/credential pitfalls actually hit this run

- **`HANDOFF_NO_RUN` when the turn started from a Dispatch message.** When a phase turn is
  kicked off by a smoke/user Dispatch message *before* the daemon's phase task lands, the
  first `handoff_complete` returns 409 `HANDOFF_NO_RUN` ("this claim has taken none"). Do
  **not** re-run the phase or write a second handoff: end the turn, let the daemon deliver
  the phase task (it arrives as the next prompt in the same session), then call
  `handoff_complete` again. All committed/pushed work stands.
- **`GRANT_EXPIRED` (403) from long credential waits.** `LEGION_GRANT` lives ~60s. A single
  `sleep 60 && legion gh -- run list …` backgrounded call outlived the grant and 403'd on
  redemption. Poll CI in **separate short bash calls** (≤45s sleep each) so the grant is
  fresh when `legion gh` redeems it; never wrap a ≥60s wait and a credentialed call in one
  invocation. On slow shared runners, expect several short polls per head.

## PR body is the living READY record

Update the same PR body sections each round (don't append a changelog). Record the per-round
fix SHA in the `Threads` section (round 1 aa9cf8f896a5, round 2 3af4b2ed2c07, round 3
9c969b7781e0), refresh the `CI` run ids and head SHA, and keep both `E2E (implementer)` and
`E2E (tester)` lines current with a production-like surface, command, observation, head SHA,
and negative control. A clean run of N correction rounds yields N+1 review passes and N+1
product lines (base + N appends); the approving `review.json` round is one higher than the
last fix round.

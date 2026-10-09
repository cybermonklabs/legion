---
title: "Implementer branch hygiene on a multi-round Legion proof: outside red pushes, thread replies, live-body edits, handoff shape"
category: legion
tags:
  - implementer
  - ci-red
  - outside-push
  - review-threads
  - pr-body
  - handoff
  - jj
date: 2026-10-09
status: active
module: legion-worker implementer
related_issues:
  - "LEGSMOKE-577"
  - "sjawhar/legion-smoke#534"
---

# Implementer branch hygiene on a multi-round Legion proof: outside red pushes, thread replies, live-body edits, handoff shape

An implementer that is started three times on one issue (first change, a CI-red cure, a review
correction) keeps the branch and its records consistent by following these rules.

- Cure a red head caused by a commit you did not author with one deletion-only commit on top of
  the remote tip (`jj new legion/<KEY>@origin`, delete the file, `jj diff --summary` → `D <path>`
  only); never rewrite, squash or abandon the outside commit. Record its author and committer
  e-mails, its run ids and the add-then-remove pair in `rounds[]` (kind `ci-red`) and one
  `deviations[]` string, and report it to the architect; never describe it as your own.
- Before any cure, run the red-head discriminator: `contents/.fail-me?ref=<S>` present means
  tree-caused (cure); 404 means the job failed on its own (`run rerun --failed`, push nothing).
- Answer a review thread with `Fixed in <sha>: <one line>` plus the Legion footer, run
  `legion threads resolve` after the push, and paste its `left open … not an acceptance` line into
  the body stamped with that head. Your reply closes nothing: the opener's `Accepted:` does, and
  the merger resolves acceptances posted after your last run.
- Edit the live pull request body in place: fetch `pulls/<N> --jq .body` into a fresh temp
  directory, diff it against your last write to see what other phases added, change only your
  lines, trim trailing blank lines with `printf '%s\n' "$(cat body.md)"`, PATCH, and read back;
  the only acceptable readback difference is jq's trailing newline. Overwriting from a local copy
  destroys the reviewer's `Look at first` and `Not proven / risk` and the tester's `E2E (tester)`.
- Rebuild a handoff from the file on disk with a small `bun` script file (not an inline `-e`
  string): delete `schemaVersion`, `phase`, `completed` and `issue`, move the superseded
  `proof[]` under the old round's `supersededProof`, append the new round, set `headSha` and
  `proof[]` to the live head, and pipe the result to `legion handoff write --phase implement`.
  Keep one source of truth per fact: `rounds[]` is the history, `proof[]` is the live head's
  proof; a `commits[]` list that restates `rounds[]` is a review finding, not safety.
- Expect `jj split` with every change selected to leave an empty, undescribed sibling of the new
  working copy. It sits on no pushed chain (`legion push` pushes `@-`), cannot be abandoned under
  the shared-operation-log rules, and is harmless; note it once and move on.
- When the daemon's task line arrives before the driver's targeted Dispatch message, reply
  `WAITING`, tell the architect which acceptance item waits, and start on the message; the gap
  ranged from ten seconds to twenty-four minutes on this issue.

## Evidence

- Outside push 405efc636f7430aca8b22d4d182f3c335e1a1566 `Stage 4b proof: fail-on-demand`
  (author `sjawhar-agent[bot]`, committer `noreply@github.com`) added `.fail-me` above the
  implement handoff 61cd067b1bb66b44adc291dc387da8d0acbaf3a7; `fail-on-demand` failed
  (37952704305, 37952698867). Cure 025e22ba774a2e5b7d03a395464cfa5bf1986c19
  `smoke: remove .fail-me so fail-on-demand passes again`: `D .fail-me` only; all four scoped runs
  green (37952899408, 37952899535, 37952895426, 37952895410). `implement.json` `rounds[1..2]`,
  `deviations[5]`.
- Thread r4232078301 (REQUEST_CHANGES 5472461336 at 35b4dec59f91a09adaaecc89ecc8aec00ef011f3):
  reply r4232094281 `Fixed in 73865aae62b7d048ff4993b8ebfa9ffcbe1dcc21: appended the review line
  as line 2; file is 95 bytes / 2 lines.`; `legion threads resolve` → `left open … newest reply
  by legion-implementer is not an acceptance` at both 73865aae and the handoff head
  c96e056c5e165723e120fb99db6ea798f8433bb0; the reviewer's `Accepted:` came with its approval
  round (APPROVE 5472633667 at 7a1734c9e8556962d47672e9249a93ae6daa1ab7).
- Live-body diff before the round-3 edit showed the reviewer's `Look at first` and two
  `Not proven / risk` lines and the tester's seven `E2E (tester)` lines added since the previous
  write; the edit kept them and changed the Change bullet, CI, Threads, E2E (implementer) and
  Size lines only.
- Handoff rebuild scripts dropped `commits[]` in round 3 on the reviewer's fast-follow
  (`review.json` `fastFollow`), recorded in `implement.json` `deviations[6]`.
- `jj split` residue: empty sibling commits 8c6acb5e, a0b2297f, 34a9a7be, 71ab0775, c9c95e34,
  ec21b5e1, none on the pushed chain.
- Message gating: the implementing task line arrived at 14:56Z, the driver's implementation
  message at 15:25Z; the CI-red task line and message arrived within ten seconds of each other.

---
title: "Operator-gated Legion phases, 60-second credential grants, and honest negative controls"
category: legion
tags:
  - legion-worker
  - deployment-instructions
  - dispatch
  - credential-grant
  - gh-shim
  - negative-control
  - e2e-proof
  - handoff-ledger
date: 2026-09-26
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-270"
  - "sjawhar/legion-smoke#319"
---

# Operator-gated Legion phases, 60-second credential grants, and honest negative controls

Distilled from PR #319 (issue LEGSMOKE-270): a one-line append to `smoke.txt` run under a
deployment instruction that made every role wait for a targeted human Dispatch message before
acting. The tooling lessons here transfer to any Legion phase; the proof lessons to any small,
verifiable artifact.

## Waiting for a targeted Dispatch message: pre-flight, then yield with `WAITING`

The LEGSMOKE deployment instruction said "do not act until a targeted human Dispatch message
gives the next exact proof operation." The message is a Dispatch frame targeted at
`session:<your session id>`; it arrives as a new prompt in your session, not as a `hub` message.
`hub wait` with no running jobs returns immediately ("No running background jobs to wait for") —
it cannot be used to block for it.

Pattern that worked on #319:

1. Do everything read-only that the phase needs anyway: bind to `$LEGION_WORKSPACE`, read the
   handoffs and the spec, `jj git fetch`, confirm the before-state (`wc -l`, `sha256sum`, last
   line) matches the plan, check `envoy_inbox` and `dispatch_read` for the message.
2. Yield with a reply whose first line starts `WAITING` and names the message you are waiting
   for. The daemon's phase-open reminder asks for exactly that line; the frame wakes you.
3. Record in the handoff the message id and time you acted on (`operatorRule` on #319:
   `bb4cc3ec… at 10:26:35Z`), so the gate is auditable.

Do not post a Dispatch progress message while waiting; the human who sends the gate message
wrote the rule and knows you are holding.

## A credential grant lives 60 s from the start of the bash call — a watch is terminal

The extension writes `$LEGION_GRANT_FILE` before each bash command; every `legion gh` call in that
command redeems from the same grant, which expires 60 seconds later. On #319 this one command:

```
legion gh -- pr checks 319 --repo sjawhar/legion-smoke --watch --fail-fast; \
legion gh -- run list --repo sjawhar/legion-smoke --commit <head> --json …
```

watched for 74 s (queue latency: the jobs themselves take 22 s and 6 s) and then failed on the
second call with `Unable to redeem LEGION_GRANT: daemon returned 403: {"code":"GRANT_EXPIRED"}`.
The credential was fine; the timing was not.

Rules:

- `--watch` (or any `gh run watch`) is the last credentialed thing in its bash call. Never `&&`- or
  `;`-chain another `legion gh`, `jj git push`, or `pr://` read after it. A fresh bash call gets a
  fresh grant for free.
- The same applies to a `task` subagent or `eval` subprocess: it reads the grant your last call
  wrote and 403s after 60 s. Credentialed reads inside a subagent must be short.
- When job durations are known and short, two or three separate un-watched `pr checks` calls
  spaced by your own work beat one long watch.

## Which negative controls discriminated on a one-line append, and which were ritual

Controls that would flip the verdict if the change were wrong (reuse verbatim):

1. The same contents-API read at `ref=main` → old last line, old sha256, old line count, and
   `grep -cx '<new line>'` → `0` exit 1. Proves the check fails on the base and cannot pass vacuously.
2. `grep -cx '<the previous tree's near-identical marker>'` → `0` at the head. Sharp here because
   the earlier tree's PR #317 had appended `Stage 4b proof LEGSMOKE-266 2026-09-26` to the same
   file and was closed unmerged — a stale copy was a live failure mode.
3. `sha256sum` plus `wc -l` read at a pinned `ref=<sha>`. The real failure modes of "append one
   line" are a double append, CRLF, and a missing trailing newline; only the hash catches all three.
4. Resolving CI by `databaseId` and `headSha` from `gh run list --commit <head>`, never from
   "checks are green" — this is what stops an earlier head's green being cited at a later one.

Controls whose outcome did not depend on the change (drop them):

- `gh api repos/<o>/<r>/commits/0000000000000000000000000000000000000000/check-runs → HTTP 422`.
  It exercises GitHub's SHA validation and returns 422 identically whether or not `smoke.txt`
  changed.
- Re-running every Acceptance 1 command at two heads after already observing an identical sha256
  at both.
- `wc -c` beside `sha256sum`.

Rule: a negative control is an input that would flip *this* verdict. If its outcome is independent
of the change, it inflates the proof and buys nothing.

## When the spec forbids exercising a gate's failure path, say so instead of decorating

`fail-on-demand.yml`'s `gate` job fails only if a file named `.fail-me` exists. The spec forbade
creating it (it would add a second file and violate Acceptance 2). So across twelve green runs,
"gate pass" proved the workflow was dispatched and nothing else — and the 422 control above sat
where the real one was forbidden, making the proof look controlled where it was not.

Rule: write `gate green; failure path not exercised (creating .fail-me would violate Acceptance 2)`
in the `E2E` line. An unexercised gate declared unexercised is honest evidence; a cosmetic control
in its place is worse than none. Proving the red path needs its own scope (for example a throwaway
branch pushed without a pull request, where only a push-event run goes red).

## Record hazards and the role→App map in the handoff, not only the happy path

`implement.json` on #319 carried `"deviations": []` and no trace of the expired-grant incident;
the proof arrays, SHAs, and run ids it did carry are issue-specific and never re-read by anyone on
a different issue. Two cheap additions make a handoff useful beyond its own tree:

- A `hazards` array: what failed, the verbatim error, the command shape that caused it, the shape
  that worked.
- The observed role→App mapping. The planner's commit `a9671447` is authored by
  `legion-reviewer[bot]`, the implementer's by `legion-implementer[bot]`. The pre-push identity
  check in `skill://legion-worker` is scoped to the commits *you* made; an earlier phase's commit
  carrying another App is correct. Writing the map down (as `implement.json.identity` did) stops a
  future worker burning a round on the false positive or, far worse, pinning `user.email` with
  `--config`, which the repository-scoped jj config shared by every issue workspace forbids.

Also: a planner's handoff describes the branch *before* the planner's own push. `plan.json` said
`legion/LEGSMOKE-270` was "not on origin"; the implementer's first push reported
`move forward from a9671447a712`, because the planner had pushed its handoff commit after writing
that sentence. Trust `jj git fetch` and `jj bookmark list -a` over the handoff's branch statement.

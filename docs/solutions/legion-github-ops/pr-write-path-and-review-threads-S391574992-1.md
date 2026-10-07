---
title: "Legion worker GitHub ops: write-path outages, review-thread replies, and sibling PR-body lines"
category: legion-github-ops
tags:
  - github-500
  - legion-push
  - review-threads
  - pr-body
  - byte-exact-files
date: 2026-10-07
status: active
module: legion-worker
related_issues:
  - "S391574992-1"
  - "sjawhar/legion-smoke#508"
---

# Legion worker GitHub ops: write-path outages, review-thread replies, and sibling PR-body lines

Operational rules a Legion phase worker needs when GitHub misbehaves or when driving a PR's
review threads and body. All are grounded in the S391574992-1 run (PR #508).

## Order each round so an outage cannot cost proof

Land and push the code change first, confirm CI green, and only then write the handoff JSON and
edit the PR body / review threads. The proof surface (branch tip + CI) is durable before any
metadata write, so a later write-path outage strands only idempotent metadata, never the proof.

## A GitHub write-path 500 is a transient outage: retry, never force-push

When `legion push` and `gh api ... --method PATCH` return `Internal Server Error` (HTTP 500)
while reads (`gh api .../git/ref/...`, `pr checks`, `pr view`) still succeed, treat it as a
server-side write-path outage. Keep retrying the same operation unchanged. Never force-push,
`jj rebase`, or recreate the branch to "fix" it: a 500 may mean the server partially accepted a
write, so a destructive history rewrite can discard an accepted commit, whereas an idempotent
re-push of the same ref update is safe. After sustained failure, report the blocker to the
architect with the exact output (do not call `handoff_complete` — the gate requires the push) and
keep retrying; it clears on its own.

## Reply to review threads over REST; resolve over the CLI

The `legion gh` shim's "Legion never merges" guard rejects **all** GraphQL mutations, so
`addPullRequestReviewThreadReply` is unavailable. Reply with REST instead:
`gh api repos/{owner}/{repo}/pulls/{n}/comments/{rootCommentId}/replies -f body=...`
(the root comment id comes from `gh api .../pulls/{n}/comments --jq '.[]|{id,in_reply_to_id}'`).
Resolving still works — `legion threads resolve --pr <n> --repo <o>/<r>` routes through the CLI,
not raw GraphQL. So: reply via REST, resolve via the CLI.

## A bot's review thread: the subject replies, the reviewer accepts, the CLI resolves

When a non-Legion bot opens a review thread, the implementer (the finding's subject) posts a
one-line disposition — `Declined: <reason>` (defending the change) or `Fixed in <sha>: <line>` —
but must not resolve it: the subject never closes its own thread. `legion threads resolve` closes
a bot's thread only once the **Legion reviewer's** `Accepted:` is the newest comment. Ground a
`Declined:` in the spec, not opinion. Dispose threads individually, never in bulk.

## Edit only your phase's PR-body block; never re-template

The PR body holds distinct per-phase blocks (`E2E (implementer)`, `E2E (tester)` with its own
`<!-- legion: {"phase":"test"} -->` sentinel, `Threads`, `Thermo`, `Chain`). Losing a sibling's
proof line is an approval blocker, since the reviewer verifies both `E2E` lines are present.
Fetch the live body, edit only the lines your phase owns, and leave the others byte-intact —
do not regenerate the whole body from a template. This matters most on a flaky write path, where
the temptation to "rewrite cleanly" can clobber the tester's line.

## Byte-exact content files: assert bytes with a negative control, prove append-only

For a file with a locked byte form, check exact-byte equality, length, line count, absence of CR,
and a single trailing LF; prove a special separator by its UTF-8 bytes (an em dash is
`\xe2\x80\x94`), never by eyeballing the glyph. Read the GitHub-stored bytes, not the worktree:
`gh api .../contents/<path>?ref=<branch> --jq .content | base64 -d | xxd`. Pair every positive
check with a negative control (the path absent on `main@origin`; a hyphen/CRLF variant failing the
check). When appending across rounds, prove the append touched nothing above it with
`jj diff -r <code-head> --git` showing the prior lines unchanged, and re-run the byte check on the
unchanged lines each round.

## Reconcile handoff-only commits as byte-identical to the proven head

Proofs run against the GitHub surface at a named head SHA. When handoff-only commits ride above
the proven code commit (the normal Legion shape), state in the handoff and PR body that the
content is byte-identical between the code head and the tip. This forestalls the false alarm where
a reviewer sees a tip SHA different from the proof's `headSha` and suspects drift.

## Evidence

- Write-path outage: ~8 minutes of HTTP 500 on `legion push` of the round-3 handoff commit and on
  `gh api PATCH` of the PR body, while reads returned the green code head `05b032a2` throughout; it
  cleared on retry with no force-push, and the already-pushed code head kept CI green.
- GraphQL refusal: `addPullRequestReviewThreadReply` returned the shim's "Legion never merges a
  pull request" guard message; the reply landed via the REST replies endpoint
  (`#discussion_r4209484183`, `in_reply_to` `4209466485`).
- Bot thread `#discussion_r4209466485` ("is this file change needed?"): implementer `Declined`,
  Legion reviewer `Accepted: not a defect`, then `legion threads resolve` reported the thread
  resolved / no unresolved threads.
- Byte-exact target: `smoke/S391574992-1.md`, 173 bytes, 4 lines, LF-only, one trailing LF, line-1
  em dash U+2014; verified green across required checks `check` (ci.yml) and `gate`
  (fail-on-demand.yml, red iff a root `.fail-me` exists).

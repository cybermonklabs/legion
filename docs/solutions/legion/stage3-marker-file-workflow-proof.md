---
title: "Stage 3 marker-file workflow proof in legion-smoke"
category: legion
tags:
  - legion-workflow
  - smoke-proof
  - proof-recording
  - review-threads
  - jj-push
date: 2026-09-30
status: active
module: legion-smoke
related_issues:
  - "S390959369-1"
  - "sjawhar/legion-smoke#453"
---

# Stage 3 marker-file workflow proof in legion-smoke

A durable-workflow smoke proof adds one Markdown marker file under `smoke/` naming its
issue, then exercises the full Legion pipeline (plan → implement → test → review → retro →
merge) with three deliberate review rounds. The learnings below apply to any docs-only
marker-file workflow-proof issue in the `sjawhar/legion-smoke` sandbox.

## Proof surface: the GitHub head, not CI

`sjawhar/legion-smoke`'s CI is a no-op — `.github/workflows/ci.yml` is `sleep 20 && exit 0`
and never inspects file contents. The acceptance surface is therefore the repository head
itself, proven by fetching the file from GitHub at the exact PR head SHA:

```
gh api repos/sjawhar/legion-smoke/contents/smoke/<KEY>.md?ref=<headSha>
```

Record the head SHA in every `proof` entry (implement and test handoffs) and in the PR body's
`E2E` lines. State in the PR body's `CI` line that CI is a no-op and cite the API-at-head check
instead of "CI green".

## Always pair a positive proof with a negative control at the same head

Every proof runs the same API for a nonexistent marker (`smoke/<KEY>-999.md?ref=<head>`) and
confirms HTTP 404. Without it the check cannot fail — a positive-only proof does not prove the
check distinguishes present from absent.

## Copy the marker-file convention verbatim

- Line 1: `<KEY> — <title>` — an em-dash (`—`, U+2014), not a hyphen. The full key
  (e.g. `S390959369-1`).
- Then one line per review round: `Stage 3 review round N (<numeric-stem>)` — the round lines use
  the numeric stem only (e.g. `S390959369`), not the `-1` suffix.

Read a sibling marker (e.g. `smoke/S394570180-1.md`) before writing; the planner's
`plan.json` `concerns` flags this convention explicitly.

## The review rounds are the content, not rework

Each `CHANGES_REQUESTED` round intentionally instructs "append the next `Stage 3 review round N`
line below the lines already there, and change nothing else". Append exactly one line per round
and push a new head; do not treat the repeated change-requests as churn. Each round is its own
`implementing` phase that ends with the `legion` tool's `handoff_complete` — a push alone does not
finish the round. Approval lands only after the final round.

## Review-thread disposition where the implementer is the finding's subject

A non-Legion bot (`sjawhar-agent`) opened one thread asking "is this file change needed?". The
implementer answers it individually (never in bulk) justifying the change against the approved
spec, but does **not** close it: the implementer is the finding's subject, and GitHub grants
resolving a review thread to the PR author's App (the implementer), so on a bot's thread only the
Legion reviewer's `Accepted:` reply closes it via `legion threads resolve`.

- Round where the implementer's reply is newest → `legion threads resolve` reports `left open`.
- After the reviewer posts `Accepted: not a defect …` → the next `legion threads resolve` reports
  `resolved … the Legion reviewer's acceptance of a bot's thread`.

Paste the command's output verbatim into the PR body's `Threads` section each round.

## jj / push mechanics under the Go daemon

- Keep an empty working copy above your described commit: `jj describe @ -m "…"` for a code
  commit, then `jj new`; `jj split -m "<phase>: record handoff" .legion/<phase>.json` for the
  handoff commit. `legion push` pushes `@-`, so the described commit must sit at `@-`.
- Every push is `legion push` (Go daemon): it runs the ancestry check against
  `legion/<KEY>@origin` and decides the skip-CI trailer.
- Verify identity before pushing: your own commits must show your role's App
  (`legion-implementer[bot]`) in both author and committer columns; earlier phases' commits
  legitimately carry their own role's App.

## `.legion/` handoffs persist through merge under the Go daemon

The `.legion/*.json` handoff files (plan/implement/test/review) are git-committed to the branch
and appear in the PR diff — they carry `phase`, `headSha`, `prNumber`, `round`, and structured
`proof`/`findings` between phase workers. Under the Go daemon they are **not** deleted before
retro/merge (the Stage 7 override); the operator removes `.legion/` in a post-merge follow-up PR.
Retro writes no `.legion` file — its commit changes only `docs/solutions/` and sits above the
reviewer's approved head without voiding the approval. Keep each phase's `headSha` accurate and
leave the PR body's `<!-- legion: {session,phase} -->` markers intact for daemon routing.

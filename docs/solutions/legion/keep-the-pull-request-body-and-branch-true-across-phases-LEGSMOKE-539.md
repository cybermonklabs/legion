---
title: "Keep the pull request body and the issue branch true across Legion phases"
category: legion
tags:
  - pr-body
  - review-threads
  - jj
  - relaunch
  - handoff
date: 2026-10-08
status: active
module: legion-workflow
related_issues:
  - "LEGSMOKE-539"
  - "sjawhar/legion-smoke#527"
---

# Keep the pull request body and the issue branch true across Legion phases

Before any body edit, read the live body into a fresh `mktemp -d` directory, strip CR (`tr -d '\r'`),
and diff it against your last copy: another phase has written lines since. Edit only your own lines.
Recompute every derived line you own (`Size`, head SHAs, run ids, thread state) from the API at write
time; a value carried forward from your previous edit is stale by the time the next phase reads it.

Treat `legion threads resolve` as a write, not a read. It resolves every unresolved thread whose newest
comment is its opener's `Accepted:` the moment it runs. Read thread state with `gh api graphql`
(`reviewThreads { isResolved comments(last: 1) { nodes { author { login } body } } }`) and run the
resolver only where the role text puts it: the implementer after a push that answers a review, the
merger before READY.

On every relaunch, `jj -R "$LEGION_WORKSPACE" git fetch` and compare `@-` with `legion/<KEY>@origin`
before changing anything. The daemon's fresh working copy can sit on your own last handoff while
another party has pushed above it; start the change with `jj -R "$LEGION_WORKSPACE" new
legion/<KEY>@origin`, never on the stale parent.

Verify a delegated worker's edit by bytes before you commit it: `wc -c`, `wc -l`, `od -c | tail`, `cmp`
against the reference file, and `jj -R "$LEGION_WORKSPACE" diff --git` showing only the intended path.
Then commit with `jj -R "$LEGION_WORKSPACE" split -m "<message>" <that path>`, so the commit carries
exactly that path and `jj status` is clean before `legion push`.

Record every head of every round in the handoff: a `heads` map naming the code head, the handoff head
and any red head, and earlier rounds kept whole under `rounds[]`. A later reader must be able to tell
which head each `proof[]` entry and each `E2E` line names without reading the transcript.

## Evidence

- The body's `Size` line read `+767/−0 … review.json +58, test.json +70` while GitHub listed
  `review.json +126`, `test.json +130` (`+827/−0` at dff45875): each phase left the others' lines
  alone and no one recomputed it, so the reviewer logged it as a `Fast-follow` inside its APPROVE
  (`.legion/LEGSMOKE-539/review.json` `fastFollow`). Retro recomputed it from `jj diff --stat`.
- During retro, `legion threads resolve --pr 527` run to look at thread state resolved thread
  r4223769272 at once (`resolved … its opener's acceptance`), a step the skill assigns to the merger.
  The graphql read above would have answered the question without writing.
- At the CI-red round the daemon's working copy sat on the implementer's handoff 5c0fb8dd while
  `legion/LEGSMOKE-539@origin` was the proof driver's 27072ca6; `jj new legion/LEGSMOKE-539@origin`
  put the fix 728ac362 on the remote tip and `legion push` moved the bookmark forward.
- Each of the three implementer commits (21de6427, 728ac362, 9525be6b) was written by a
  `deep-worker`, checked by bytes (56 → 95, `od -c` tail `w o r k f l o w \n` → `( L E G S M O K E ) \n`,
  line 2 `cmp` against `smoke/LEGSMOKE-526.md` → 0) and `jj diff --git`, then committed with
  `jj split -m … smoke/LEGSMOKE-539.md`; GitHub's file list shows the marker file and the handoffs only.
- `implement.json` `heads` names C1, red, C1b, C2b, test, review, C1c and keeps rounds 1–2 under
  `rounds[]`; `test.json` and `review.json` keep their round 1 the same way, which is how the reviewer
  verified "criterion 4 at 9525be6b and 2fbb4a1e, not at dff45875".

---
title: "jj handoff commit ordering and E2E proof headSha reachability across Legion review rounds"
category: legion
tags:
  - jj
  - legion-worker
  - handoff
  - review-rounds
  - e2e-proof
date: 2026-09-25
status: active
module: legion-implementer
related_issues:
  - "S393609902-1"
  - "sjawhar/legion-smoke#229"
---

# jj handoff commit ordering and E2E proof headSha reachability across Legion review rounds

Context: a Legion implementer carries a change through implement plus three review rounds in a
shared `jj` workspace, recording an `E2E` proof (with a `headSha`) in `.legion/implement.json`
each round. `jj` rewrites git commit SHAs on every `split`/`rebase`, so naive proof recording and
handoff commit placement break. These are the concrete, reusable mechanics.

## `jj split <paths>` puts the selected paths in the PARENT, rewriting the code commit's SHA

Running `jj split -m "implement: record handoff" .legion/implement.json` when the working copy
holds both the code change and the handoff file places `.legion/implement.json` in the **first
(parent)** commit and leaves the code change in `@` as its child — the reverse of the intended
"code below, handoff on top" order. Worse, the code commit is **rewritten to a new SHA**, so a
`headSha` you captured and pushed before the split (the SHA the PR head had at proof time) becomes
an orphaned object that is not in the branch's ancestry.

Fix: after the split, reorder so the handoff sits on top of the code commit:

```bash
jj -R "$LEGION_WORKSPACE" rebase -r <handoff-commit> --insert-after <code-commit>
```

Now the code commit is a **stable ancestor**, and its SHA is a reachable head to cite in the
proof. On this issue the marker commit settled at `947e106808a1`, the review-round lines at
`75e3fbd8` / `d3faa736` / `eecd116c`, each cited as the `headSha` for its AC — every one an
ancestor of the approved tip `8d87cc06`, hence reachable.

## Record a reachable ancestor SHA (or the jj change-id), never the pre-split tip

Any `headSha` you bake into a handoff must be an ancestor of the final branch tip. Because `jj`
rewrites SHAs on later rounds, prefer the SHA of the **stable code commit where the change lives**
(an ancestor), not the transient PR-head SHA at capture time. Rewritten commits survive as git
objects only because `jj` pins them under `refs/jj/keep/`; do not rely on that for a proof another
role must resolve on the remote.

## The tester must re-derive the proof at the LIVE PR head, not trust recorded per-commit SHAs

Downstream verification (tester, reviewer) must re-run `legion gh -- pr diff <n>` and
`pr view <n> --json files` against the **current** PR head and re-derive the cumulative product
diff, because a recorded snapshot SHA goes stale the moment a later round rebases. Verify the
cumulative change with:

```bash
jj -R "$LEGION_WORKSPACE" diff --git --from main@origin --to <head> '~.legion' \
  | awk '/^diff --git a\/README.md/{f=1;next} /^diff --git /{f=0} f && /^\+[^+]/{print substr($0,2)}'
```

## `handoff_complete` refuses when `@` IS the handoff commit — `jj new` off it first

`legion handoff_complete` errors with `.legion/<phase>.json has changes in the working copy that
are not committed` whenever `@` points at the handoff commit itself (in `jj`, `@`'s content is the
uncommitted working copy). After committing the handoff, run `jj -R "$LEGION_WORKSPACE" new @` so
`@` is a fresh empty commit above the committed handoff, then complete.

## Retro commit goes directly on the approved head, not above stray unpushed commits

At retro, another role's unpushed commit (here a late `test: record handoff`) can sit locally
between the approved head and `@`. Base the `docs/solutions/` commit on the **approved head**
(`jj -R "$LEGION_WORKSPACE" new <approved-sha>`) so the pushed tip is exactly `approved head +
docs/solutions/` and the merger's `jj diff --from <approved> --to <tip> --summary` shows only
`docs/solutions/` changes. A tip that also carries a `.legion` or product commit above the
approved head voids the approval.

## Merger/retro workers must not gate on GitHub `reviewDecision`

When per-round reviews are delivered as formal GitHub PR reviews (not inline threads), earlier
`CHANGES_REQUESTED` reviews are never dismissed, so the aggregate `reviewDecision` can remain
`CHANGES_REQUESTED` even after the Legion reviewer's final `APPROVE` on the clean head. Gate on
the Legion reviewer's own `APPROVE` at the named head plus `mergeStateStatus == CLEAN` — never on
`reviewDecision` — or a merger will false-block a genuinely approved PR.

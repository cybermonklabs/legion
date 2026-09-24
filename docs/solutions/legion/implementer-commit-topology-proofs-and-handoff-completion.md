---
title: "Legion implementer: commit topology, proof headSha, and handoff completion"
category: legion
tags:
  - legion
  - implementer
  - jj
  - handoff
  - proof
  - gh
date: 2026-09-24
status: active
module: legion-worker
related_issues:
  - "S393330512-1"
  - "sjawhar/legion-smoke#164"
---

# Legion implementer: commit topology, proof headSha, and handoff completion

Reusable mechanics for the Legion implementer phase, learned building
sjawhar/legion-smoke#164 (a one-line append to `stage3-proof.txt` across an
initial round plus three review rounds). The change was trivial; every lesson
below is about `jj`/handoff/`gh` process, not the code.

## Freeze commit topology before you record or push a proof `headSha`

A proof's `headSha` (in `.legion/implement.json` and the PR body's
`E2E (implementer)` line) must point at a commit whose SHA will not move.

- git/jj commit SHAs embed the committer timestamp, so **any** `jj split`,
  `jj rebase`, or re-parent regenerates the SHA even when the file content is
  byte-identical. A `headSha` you recorded or pushed before the rewrite is then
  stale — it names a commit no longer on the branch.
- `jj split -m "implement: record handoff" .legion/implement.json` does **not**
  put the handoff on top of the impl work. It places the *selected* file
  (`implement.json`) in the **parent** commit and leaves the impl work in the
  child/working-copy commit, and it re-parents the impl commit — changing the
  impl SHA you may have just cited.

Do this instead:

1. Make the impl work commit and let its SHA settle (this is the commit you
   cite as the proof `headSha`).
2. Put the handoff/metadata commit **on top** as the branch tip.
3. Cite the frozen **ancestor** impl commit in the proof, never the tip.

Why the tip cannot be the `headSha`: the handoff file embeds the `headSha`, and
the handoff commit's own SHA depends on that file's bytes. If the handoff commit
were the tip and cited itself, the SHA would depend on a value derived from that
same SHA — a circular reference that cannot be satisfied. Citing a frozen
ancestor breaks the cycle. The `E2E (implementer)` line's `headSha` (the impl
commit) and the CI/PR head (the tip that includes the handoff) legitimately
differ; state both.

## `legion handoff_complete` needs a clean working copy

`handoff_complete` refuses while `.legion/<phase>.json` shows as an uncommitted
working-copy change:

```
.legion/implement.json has changes in the working copy that are not committed
```

Sequence that works: write the handoff, commit it (its own commit on the
branch), set the bookmark to that commit, then `jj new` an **empty** working
copy on top so `@` has no pending change. Only then call `handoff_complete`.

## `handoff_write` accepts only phase-specific fields

Pass just the phase fields (for `implement`: `filesChanged`, `proof`,
`deviations`, …). The tool owns and injects the base fields; including
`schemaVersion`, `phase`, or `completed` is rejected:

```
handoff data field schemaVersion is not allowed
```

## Read PR review threads with REST, not GraphQL, under the `gh` shim

`legion gh -- api graphql` for a **read-only** review-thread query is refused by
the worker `gh` shim's merge/write guard (a false positive — the query mutates
nothing):

```
Legion never merges a pull request: publish READY ...
```

Use REST instead — these are not blocked and return the same information:

- `gh api repos/<owner>/<repo>/pulls/<n>/reviews` — review verdicts + bodies.
- `gh api repos/<owner>/<repo>/pulls/<n>/comments` — inline review-thread comments.

A review can request changes in the **review body** with no inline thread; in
that case `legion threads resolve --pr <n> --repo <owner>/<repo>` reports
`no unresolved threads` and the disposition lives in the PR body's `Threads`
section, keyed to the fixing commit.

## Keep the retro/merge tip diff to `docs/solutions/` only

The merger verifies `jj diff --from <approved-sha> --to <tip-sha> --summary`
shows only `docs/solutions/` changes. Base the retro documentation commit
directly on the **approved head**, not on a later unpushed handoff commit that
would drag `.legion/*.json` (or any other path) into that diff and make the
merger refuse.

Related observation: if the pre-merge `.legion/` deletion step is skipped, the
handoff artifacts stay committed in the approved head, so a strict "exactly one
file" acceptance reading is only met on the *runtime* file. Confirm whether the
flow expects `.legion/` deleted before the approved head, and satisfy "one file"
against the runtime change explicitly.

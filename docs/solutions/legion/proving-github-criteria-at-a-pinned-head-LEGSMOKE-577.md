---
title: "Proving GitHub-surface criteria at a pinned head on a multi-round Legion branch"
category: legion
tags:
  - proof
  - github-api
  - actions-run-list
  - skip-checks
  - review-comment-predicate
  - net-zero-file-pair
date: 2026-10-09
status: active
module: legion-worker proofs (implementer, tester, reviewer, merger)
related_issues:
  - "LEGSMOKE-577"
  - "sjawhar/legion-smoke#534"
---

# Proving GitHub-surface criteria at a pinned head on a multi-round Legion branch

Every phase re-proves the plan's criteria against GitHub at its own head. These rules keep a
proof reproducible after the branch has moved, after a reviewer has commented, and after an
outside commit has been added and cured.

- Pin any predicate that counts review comments to the judged head's `commit.committer.date`
  (`created_at < D`); use the unpinned count only as the trigger for acting. A head pushed before
  the comment stays `K=0` forever; the head that answers it is `K=1` by construction.
- Query Actions runs with the full 40-character SHA and filter by `headBranch`; a short SHA
  returns `[]` silently, and an unscoped list at a SHA shared with sibling branches mixes in
  their runs.
- Treat `[]` from a scoped run list as "not judged", never as a pass. A skip-checks head is
  identified only positively: `commits/<S>` shows the trailer `skip-checks: true` and files only
  under `.legion/<ISSUE>/`. Its verdict is carried from J, the nearest ancestor (walk
  `parents[0]`) whose own files include a path outside `.legion/<ISSUE>/`; record both SHAs and
  the `jj diff --from J --to S --summary` that shows only `.legion/<ISSUE>/`.
- Read the pull request's file list as the net diff. A file added and then removed on the branch
  (an add-then-remove pair) is absent from `compare/<base>...<head>` and from `pulls/<N>/files`;
  prove its absence at the head with a `contents/<path>?ref=<S>` → 404 read, and keep its
  history in the handoff's `rounds[]`, not in `proof[]`.
- Prove criterion-shaped byte content with the contents API's raw media type
  (`Accept: application/vnd.github.raw+json`) at the full SHA and `cmp` against a `printf` of the
  expected bytes; the negative controls are the other arm's `printf`, a CRLF variant and a
  no-trailing-LF variant against the same fetched bytes, each expected to exit 1.
- Re-stamp the body's `Size` line after every handoff push, since the handoff commit changes the
  additions GitHub reports; a stamped line names the SHA it was read at and is current, not drift.
- Settle CI with `pr checks --watch --fail-fast` in its own bash call, then re-read
  `pulls/<N> --jq .head.sha` and the run list in a fresh call: a credential grant is written per
  bash call and lives 60 seconds.

## Evidence

- `K` pinned: at the review-line head 73865aae62b7d048ff4993b8ebfa9ffcbe1dcc21
  (`D=2026-10-09T16:04:06Z`) the reviewer's comment 4232078301 (created 16:02:30Z) gives `K=1`;
  with `D` = the review handoff head 35b4dec59f91a09adaaecc89ecc8aec00ef011f3's date (15:59:27Z,
  pushed before the comment) the same query gives `K=0`, and the file at the reviewed parent
  5f1471658c12acefaea4ec1adfec679b32f82a95 is still 56 bytes. `implement.json` `proof[1]`.
- Short SHA: `run list --commit 73865aae` → `[]`; the full SHA → 4 runs
  (`ci` 37956401903/37956395621, `fail-on-demand` 37956401787/37956395568). `plan.json`
  `measurements` M7 measured 18 foreign push runs at the base SHA before the `headBranch` filter.
- Skip-checks carry: the tester's handoff head 5f1471658c12acefaea4ec1adfec679b32f82a95 (trailer
  true, files only `.legion/LEGSMOKE-577/test.json`, 0 scoped runs) carried criterion 3 from
  the nearest code commit; the reviewer named both the nearest green ancestor and the plan's
  strict J separately because an outside commit sat between code heads (`plan.json`
  `gapAnalysis` F6/F7).
- Net-zero pair: `.fail-me` was added by 405efc636f7430aca8b22d4d182f3c335e1a1566 and removed by
  025e22ba774a2e5b7d03a395464cfa5bf1986c19; neither path appears in `compare/c6fc9a66...73865aae`
  or in `pulls/534/files`, while `compare/c6fc9a66...405efc63` still lists `added .fail-me`
  (`implement.json` `proof[0].negativeControl`).
- Byte proof: 95 bytes / 2 lines `MATCH-2LINES`; controls `cmp: EOF on - after byte 56`,
  `differ: char 56, line 1`, `cmp: EOF on - after byte 94`, each exit 1.
- Size re-stamp: `+555` at 025e22ba → `+638` at the handoff commit 5733d96bbc406971d4c23dae22d875f2eff75149
  → `+774` at 73865aae → `+811` at c96e056c5e165723e120fb99db6ea798f8433bb0, each line naming its SHA.

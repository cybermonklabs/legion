---
title: "Proving a one-file smoke change under the Go daemon: surfaces, heads, and CI carry-over"
category: legion
tags:
  - legion-workflow
  - proof
  - smoke-repository
  - ci-skip
  - trailing-newline
date: 2026-09-30
status: active
module: legion-smoke
related_issues:
  - "LEGSMOKE-327"
  - "sjawhar/legion-smoke#449"
---

# Proving a one-file smoke change under the Go daemon: surfaces, heads, and CI carry-over

Retro of LEGSMOKE-327 (Stage 4b proof tree 1: the whole workflow), delivered as pull request
sjawhar/legion-smoke#449, which adds `smoke/LEGSMOKE-327.md` holding one line. The change itself
is trivial; what is worth keeping is how a docs-only change with no runtime was proven on a real
surface, how the head SHAs and check runs line up across five phase pushes, and where the plan's
guidance lagged the daemon it ran under.

## The proof surface when the repository has no runtime

`sjawhar/legion-smoke` has no build, test, lint, or deploy tooling. Its two workflows carry no
content signal: `ci.yml` job `check` is `sleep 20 && exit 0`, and `fail-on-demand.yml` job `gate`
fails only when a `.fail-me` file exists at the head. "CI green" therefore says nothing about the
file's bytes. The production-like surface is GitHub itself, read at the exact head SHA:

```bash
# acceptance item 1: content and trailing newline
legion gh -- api "repos/sjawhar/legion-smoke/contents/smoke/LEGSMOKE-327.md?ref=<head>" --jq .content | base64 -d
legion gh -- api "repos/sjawhar/legion-smoke/contents/smoke/LEGSMOKE-327.md?ref=<head>" --jq .content | base64 -d | wc -l   # 1
# acceptance item 2: scope
legion gh -- api "repos/sjawhar/legion-smoke/compare/main...<head>" --jq '.files[].filename'
legion gh -- pr view <n> --repo sjawhar/legion-smoke --json files --jq '.files[].path'
```

Negative controls that discriminate, all recorded in the handoffs of #449:

- `?ref=main` for the new file returns HTTP 404 `Not Found` (the file is absent before merge).
- `printf '%s' '<the line>' | wc -l` prints `0`: `wc -l` counts newline characters, so on a
  one-line file it is a trailing-newline check, not a line count. Write the file with the
  `write` tool (content ending in `\n`) or `printf '%s\n'`, and confirm with `od -c` that the
  final byte is `\n` before committing.
- A one-character change (`tree 2`) piped to `cmp - smoke/LEGSMOKE-327.md` exits 1 with
  `differ: char 35, line 1`.
- `fail-on-demand / gate` green is the negative control for "no `.fail-me` at the head".

The implementer recorded this in `.legion/implement.json` `proof` and the PR body's
`E2E (implementer)` line; the tester re-ran the same commands against the same head and recorded
`implementerProof.verdict: verified`; the reviewer re-ran them a third time. Each role's bar was
"re-ran it myself with identical output", never "the handoff says so".

## Which heads carry check runs

Five pushes moved `legion/LEGSMOKE-327` in this run. Which of them started GitHub workflows:

| Push | Head | Content | CI |
| --- | --- | --- | --- |
| planner | `e53c1950` | `.legion/plan.json` | skipped (`skip-checks: true` trailer) |
| implementer | `8bb362cf` | `smoke/LEGSMOKE-327.md` | full: `ci` 36689585969, `fail-on-demand` 36689585980 |
| implementer | `9e8df196` | `.legion/implement.json` | full: `ci` 36689761227, `fail-on-demand` 36689761291 |
| tester | `379c08b6` | `.legion/test.json` | skipped (`skip-checks: true` trailer) |
| reviewer | `f0bf2166` | `.legion/review.json`, verdict approved | full |

`legion push` adds the trailer only for handoffs whose phase guarantees a later push (plan, test,
and a review whose verdict is `changes_requested`). Consequences a future worker needs:

- The reviewer's approved head can legitimately have **zero** check runs
  (`repos/<owner>/<repo>/commits/<sha>/check-runs` returns an empty list). Cross-reference the
  `codeHeadSha` that every downstream handoff pins (test.json, review.json) and `legion state`'s
  `pullRequest.checksVerdict`, which the daemon carries from the code head. An empty list is not
  by itself a red flag.
- Every code push produces both a `push`-event and a `pull_request`-event run per workflow at
  the same head, so `gh pr checks` lists four rows per head. Cite the `pull_request` runs in the
  PR body's `CI` line; confirm `head_sha` with `repos/<owner>/<repo>/actions/runs/<id>`.
- The `E2E` line names the **code** head (`8bb362cf`), which differs from the PR head as soon as
  the implement handoff is pushed. Cite both in the `CI` line and state what the later head adds.

## PR body under the Go daemon

`.legion/*.json` stays on the branch through approval and READY; the operator removes it from
`main` after the merge (no role pushes the deletion until Stage 7). Two adjustments follow:

- Acceptance criteria phrased "the pull request touches exactly one file" are met by "exactly
  one file besides `.legion/*.json`". Say so in the PR description's first paragraph so a reader
  of the file list is not surprised.
- Plan guidance written from the shared skills can lag the daemon's rules. `.legion/plan.json`
  `evidence.review` said "APPROVE only the head that differs by the `.legion/` deletion alone";
  the reviewer correctly followed the Go-daemon role text instead and recorded the override in
  `.legion/review.json` `notes[0]`. A planner under the Go daemon should write review guidance
  for the head that still carries `.legion/`; a reviewer who finds the conflict resolves it in
  favour of the daemon's role text and records that it did.

Practical sequence that produced a complete body without a second round: write the body to a
temporary file, `legion gh -- pr create --body-file`, watch `legion gh -- pr checks <n> --watch`
until green, then `legion gh -- pr edit <n> --body-file` with the run ids filled in. The
`Thermo` line is omitted entirely on a docs-only pull request.

## Not recorded

- Filenames under `smoke/` are heterogeneous (`S<number>-1.md`, `stage3*.txt`,
  `stage4b-legsmoke-<N>.md`, `LEGSMOKE-327.md`). The approved spec fixes the exact path; do not
  infer a convention from neighbours.
- The scope guard for a review request other than one appended line (plan `risks[3]`) was never
  exercised: no review thread was opened on #449.

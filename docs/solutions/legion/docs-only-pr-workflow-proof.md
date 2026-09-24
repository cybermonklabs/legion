---
title: "Docs-only Legion PR: proving an exact-line append through the full pipeline"
category: legion
tags:
  - legion
  - docs-only
  - product-fingerprint
  - negative-control
  - jj
date: 2026-09-24
status: active
module: legion-worker
related_issues:
  - "S392849451-1"
  - "sjawhar/legion-smoke#153"
---

# Docs-only Legion PR: proving an exact-line append through the full pipeline

A docs-only change (here: appending one workflow-proof line plus three review-round
lines to `README.md`) still runs the entire Legion pipeline — plan, implement, test,
three review rounds, retro, merge — on a real GitHub PR. These are the reusable,
non-obvious mechanics that made PR #153 verifiable, plus the gotchas that cost time.

## Prove with the product fingerprint, never the raw diff

The raw branch diff carries process exhaust, not just the product change. On PR #153 the
raw `jj diff --from main@origin --to legion/S392849451-1 --stat` reported **4 files, 145
insertions** — `.legion/plan.json` (82), `.legion/test.json` (32), `.legion/implement.json`
(27), and `README.md` (4). The `.legion/*.json` handoff files ride the branch during
review and are removed by a pre-merge `.legion/` deletion push; they are process artifacts,
not the product.

Always scope the proof to the product fileset:

```bash
jj -R "$LEGION_WORKSPACE" diff --from main@origin --to legion/<KEY> \
  --stat '~(.legion | docs/solutions)'
# => README.md | 4 ++++   1 file changed, 4 insertions(+), 0 deletions(-)
```

Keep `docs/solutions` in the exclusion even when it is empty (`jj` warns
`No matching entries for paths: docs/solutions` — harmless) so the same command is portable
once retro adds learnings. This is the same fileset the reviewer/tester/merger use for the
unchanged-diff fingerprint; using it as the E2E proof keeps every role comparing the same thing.

## Negative control for an exact-line append needs issue-id discrimination

`README.md` already contained near-identical lines from a **prior** run — lines 21-23 were
`Stage 3 review round 1/2/3 (S392088555)` directly above the new `...(S392849451)` lines. A
prefix-only check (`grep '^Stage 3 review round 3'`) false-positives on the old run's lines.

The working negative control uses three independent discriminators:

1. Exact-match count of each appended line keys on the **full issue id** and must equal 1:
   `grep -c '^Stage 3 review round 3 (S392849451)$'` → `1` (0 or >1 fails — catches a missed
   append or a duplicate).
2. A deliberately wrong id must return 0:
   `grep -c '^Stage 3 review round 3 (S000000000)$'` → `0` (proves the check discriminates the
   exact id, not just the prefix). This id-discrimination control is the non-obvious must-have.
3. The product-scoped `--stat` must list **only** `README.md`; any second product file fails
   acceptance criterion 2 ("change nothing else").

## CI is two workflows, two jobs — verify both live at the head SHA

CI is split: workflow **`ci`** runs job **`check`**, and workflow **`fail-on-demand`** runs job
**`gate`**. Both must be `success`/`completed` at the head SHA. Verify against the check-runs API
at the head, not from a handoff's recorded CI block:

```bash
legion gh -- api repos/<owner>/<repo>/commits/<head-sha>/check-runs
```

Knowing `gate` belongs to `fail-on-demand` (not `ci`) avoids mistaking its separate run for a
missing job on a green build. There are commonly two `ci` runs at one head (push- and
PR-triggered); assert the conclusion, not the count.

## Per-round commit cadence and split App authorship

Each round is a fixed three-commit cadence, authored across two GitHub App identities:

```
plan: record handoff            legion-reviewer[bot]   (planner acts as the review App)
impl(<KEY>): append … line      legion-implementer[bot]
implement: record handoff       legion-implementer[bot]
test: record handoff            legion-reviewer[bot]   (tester acts as the review App)
```

Only `legion-implementer[bot]` has `contents` permission and can push. The tester's and
reviewer's handoff commits are unpushed locally and **ride the implementer's next push** —
so after the final review round the implementer's push (or the `.legion/` deletion push) is
what actually publishes the tester's round-N handoff. Do not be alarmed that the reviewer App
"authored" the test handoff; that is the role→App mapping, and the authorship check
(`author.email() | committer.email()` per commit) should confirm your own commits are the
implementer App while accepting the reviewer App on plan/test handoffs.

## Tester proof is an independent re-run; the PR body carries both E2E lines

The tester does not copy the implementer's proof — it re-executes the exact command triple
(`pr diff` + product-scoped `--stat` + contents-API `base64 -d` render + `check-runs`) and only
then sets `implementerProof.verdict: "verified"`. Both the `E2E (implementer)` and
`E2E (tester)` lines must be present in the PR body for retro to pass; retro's first durable
learning is the gap if either is missing.

## Don't manufacture a test; skip the Thermo line

`requiredSkills` were `none` and no regression test is warranted — a unit test for a one-line
README append defends no observable contract; the PR diff on the real surface *is* the proof.
For a docs-only PR the READY `Thermo` line (ce-simplify-code + thermonuclear pair) is **omitted
entirely** — there is no runtime code for either pass. The PR body must still carry the literal
`Dispatch: <KEY>` line and the READY Verification section.

## Gotchas that cost time

- **`jj log -r 'description("…")'` resolved to nothing** in this jj version even for an exact
  first-line match. Address commits by short change/commit id (or `description(exact:"…")`),
  not a substring `description("…")` revset.
- **A subagent cannot redeem the pane credential.** The per-pane grant file is written before
  each of the parent's own `bash` calls and expires ~60s later; a `task` scout's `legion gh`/
  `gh` therefore 404s on the private repo. Run credentialed GitHub reads from your own bash
  calls and hand the subagent local artifacts (jj, `.legion/*.json`) to analyze instead.

---
title: "Committing a file that names its own pull request, and reading CI at the head that has it"
category: legion
tags:
  - commit-ordering
  - pull-request
  - ci
  - skip-checks
date: 2026-10-16
status: active
module: legion-worker
related_issues:
  - "SMOKE-3"
  - "sjawhar/legion-smoke#599"
---

# Committing a file that names its own pull request, and reading CI at the head that has it

## A file that must name an artefact that does not exist yet

A record whose content names its own pull request cannot be written in one commit: the pull
request number exists only once the branch carries a commit.

- Commit the part that does not depend on the artefact, push, create the artefact, append the
  line that names it, push. Keep the order the worker skill fixes (implementation commit → push
  → `gh pr create`); do not open the pull request on an earlier phase's head to learn the number
  first, and do not predict the number — another actor can take it.
- Pin **every** proof, `E2E` line and byte comparison to the second head. State in the handoff
  that the one-line intermediate is never a reviewed, READY or merged head, and quote both SHAs,
  so a later reader does not read the intermediate as a defect.
- Check that the number you wrote is **this branch's own**:
  `gh api 'repos/{owner}/{repo}/pulls?state=all&head=<owner>:<branch>'` must list exactly that
  number. A number is guessable and a neighbouring number is a plausible-looking lie.
- Do not rewrite the pushed first commit to fold the two together: the rewrite needs the
  pushed-commit guard, and it buys nothing a second commit does not.

Where the split is impossible because the artefact is written *before* the push that creates its
own head — a phase handoff naming the head it will produce — name the SHA in the artefact that is
written **after** the push instead (the posted review, the PR body), and say in the handoff which
head that is. A handoff field holding prose where a later reader expects a SHA cannot answer
"which head was approved".

## Which head carries the CI verdict

On a Legion branch the head is often a handoff commit, and a handoff-only push carries the
`skip-checks: true` trailer.

- Read `check-runs` at the newest head whose push was **not** handoff-only. A `skip-checks` head
  returns **zero** check runs, which is neither green nor red; a verdict quoted from it is a
  verdict harvested from the wrong SHA.
- Prove the code head still stands for the current head:
  `gh api repos/{owner}/{repo}/compare/<code-head>...<current-head> --jq '[.files[].filename]|all(startswith(".legion/"))'` → `true`.
- Expect each required context **once per triggering event** when a workflow runs
  `on: [push, pull_request]` — two `gate` rows at one head is normal, and every row must be
  success.
- A pull-request body edit on a `skip-checks` head starts no workflow run, so a check that
  re-judges the body does not re-run until the next real push. Retro's own push
  (`docs/solutions/` plus the removal of `.legion/<issue>/`) is a code push and does start checks.

## Evidence

SMOKE-3 (`sjawhar/legion-smoke#599`) recorded a run in `smoke/SMOKE-3.md`, whose second line
names the pull request carrying it. The branch is `f72dfef5` (line 1 only) → `gh pr create` → 599
→ `e3f736c7` (line 2 appended, the code head) → three handoff commits pushed with the trailer.
The tester closed the loop the ordering opens, reading `pulls?state=all&head=sjawhar:legion/SMOKE-3`
→ exactly `599 open`, on a repository where fixtures #585–#598 had been opened the same day.

Check runs were measured at three handoff heads — `b338793f`, `613820ac` and `8308bdf3` — and
every one returned zero rows, while the code head `e3f736c7` returned `gate completed success`
twice (the `push` and `pull_request` runs) and `check completed success` twice. The reviewer
quoted the verdict from `e3f736c7` and proved the gap with
`compare/e3f736c7...8308bdf3 --jq '[.files[].filename]|all(startswith(".legion/"))'` → `true`.
The repository's only required context is `gate`, from
`.github/workflows/fail-on-demand.yml`, whose single step fails if and only if `.fail-me` sits at
the repository root; its red example is cited from run `38794798972`, never created.

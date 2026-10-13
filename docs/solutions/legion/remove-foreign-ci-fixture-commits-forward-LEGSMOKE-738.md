---
title: "Remove a foreign CI-fixture commit forward, record it, and judge changed files on the net diff"
category: legion
tags:
  - ci-red
  - foreign-commit
  - fail-on-demand
  - forward-fix
date: 2026-10-13
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-738"
  - "sjawhar/legion-smoke#567"
---

# Remove a foreign CI-fixture commit forward, record it, and judge changed files on the net diff

- When the daemon returns the issue to implementing with a red check, first read the red head: `gh api repos/<owner>/<repo>/commits/<red-sha> --jq '{author:.author.login, committer:.commit.committer.email, files:[.files[].filename]}'`. A commit by another identity that adds only a file your workflow fails on is a fixture, not your code.
- Cure it forward: `jj git fetch && jj new legion/<KEY>@origin`, delete the file, `jj split -m "<one line>" <file>` so the commit touches nothing else, and push as a code push with no trailer. Never rebase, squash or rewrite the foreign commit: it stays in the chain below your fix.
- Record it in the implement handoff under `foreignCommits` (sha, author, committer, message, files, the check it reddened, the commit that removed it) and tell the architect as information; the red run itself is the negative control for the workflow proof at the fix head.
- Evaluate a changed-files criterion on the net pull-request diff (`gh pr diff <n> --name-only`) and on the merge commit, never per commit: an added-then-removed file appears in neither. Run the pre-push guard (`jj diff --from 'fork_point(main@origin | @-)' --to @- --name-only`) and stop the push if it lists the fixture.

## Evidence

- LEGSMOKE-738, pull request sjawhar/legion-smoke#567. The proof driver (`sjawhar-agent[bot]`, committer `noreply@github.com`) pushed `04f2967741439bf1188d376ad2f95ba027f7b95e` adding `.fail-me`; `fail-on-demand` job `gate` failed (runs 38390844047 and 38390839054) while `ci` passed. The implementer removed it in `a17d02d9840f5c51b6d68cdc65933962f03564ca` (`D .fail-me` only); at that head `gate` and `check` were `success` (runs 38391036010, 38391035945), `.fail-me?ref=a17d02d9` answered 404, and `gh pr diff 567 --name-only` never listed `.fail-me`.
- The previous proof run (LEGSMOKE-728, #565) met the same fixture (`74373aab`, removed forward by `f68b72c7`); its note was removed from `main` by #566 and is recorded again here.

# The reference run of INFRA-3206's check k5

`prepare.sh <dir>` writes the inputs and outputs that `reference_test.go` (build tag `survivalref`, `SURVIVAL_REF_DIR=<dir>`) compares Dispatch's code-survival replay with.
`rework30.py` is `scripts/15_rework.py` of `dispatch://INFRA-3117/artifact/survival-scripts-tar-gz@v5` with a 30-day line, a flag that turns it off, and a file of the pairs it keeps; its header holds its diff against that script.

```bash
bash prepare.sh <dir>
```

It prints one line a step.
Its header says what each step reads and writes, and what each step needs: `DISPATCH_URL` and `DISPATCH_TOKEN_FILE` for step 1, and `AI_REPO`, the second repository's `owner/name`, for step 5 when a diff of it is missing.
A step whose outputs exist is skipped, a stopped step resumes, and a second run reads nothing over the network.
The repository is `$LEGION_WORKSPACE`, else the checkout holding the script, and it is only read.

## The tests

```bash
cd packages/envoy
DISPATCH_TEST_DATABASE_URL=… SURVIVAL_REF_DIR=<dir> go test -tags survivalref -timeout 60m -run 'TestReference' -v ./internal/dispatch/delivery/survival/
```

Each test fails, naming `SURVIVAL_REF_DIR`, where it is unset, and `TestReferenceIssuesAreAttributionsSourcesThreeAndFour` fails the same way without `DISPATCH_TEST_DATABASE_URL`.
The tests stream the patches from `<dir>`, one at a time.
The reading of `<dir>` that the code-survival run's live test shares with them, the chain, one commit's patch, the pull requests, their issues and the code groups, is `survivaltest/reference.go`.

| Test | What it compares |
| --- | --- |
| `TestReferenceLegionPerCommit` | every commit's added lines and every (adder, remover) pair's lines, with `lg/r_git_*.tsv` of step 4 and of the archive |
| `TestReferenceLegionGroups` | every commit's group under the reference's groups, with the archive's `lg/commits.tsv` |
| `TestReferenceIssuesAreAttributionsSourcesThreeAndFour` | `delivery.Attributor.IssueOf` over the head branch, title and body of each pull request that merged as a commit of `lg/commits.txt`, the chain's 1,962 changes, with `lg/pr_issue.tsv`'s lines of those commits; `lg/pulls.json` grows each day, and the pull requests off the chain are left out |
| `TestReferenceLegionSurvival` | the replay's changes, read with `16_window.py`'s selection, with the published survival figures |
| `TestReferenceLegionReworkPairs` | the replay's pairs within Legion, with `rework30.py`'s, and its negative control: the last day dropped from the replay alone fails the comparison |
| `TestReferenceBatchedRunsEqualOnePass` | the window measured in runs of 28 and of 7 days, with the single pass |
| `TestReferenceSecondRepositoryPerCommit` | the replay of the second repository's diffs, with the archive's `ai/r_diff_*.tsv` and `ai/r_git_*.tsv` |
| `TestReferenceReplayCost` | one 28-day run's patch bytes held and live heap, with R4's bounds |

## What `<dir>` holds

| Path | What it is |
| --- | --- |
| `steps/<n>.txt` | the line step n printed |
| `archives/<slug>.tar.gz` | the three archives as Dispatch served them (step 1) |
| `archives/survival-scripts-tar-gz/` | `scripts/` and `scripts-feasibility/`, version 5 |
| `archives/feasibility-second-repo-output-tar-gz/feasibility-second-repo/` | `lg/` and `ai/`: `shas.txt`, `commits.tsv`, `r_git_*.tsv`, `r_diff_*.tsv`, ... |
| `archives/survival-pass-output-tar-gz/out/` | the analysis pass's outputs (`12_analysis.txt`, `15_rework.json`, `16_window.json`, ...) |
| `lg/shas.txt` | the archive's 3,356 legion commits, oldest first |
| `lg/git/<sha>.diff.gz` | each commit's part of `git log --reverse --first-parent -p -U0 -M` (step 2) |
| `lg/commits.txt` | `%H %cI %P %s` of each commit, oldest first |
| `lg/pulls.json` | closed pull requests updated since 2026-08-01, into main or (stacked) onto the chain from another base: number, merge_commit_sha, head.ref, base.ref, user.login, title, body, merged_at (step 3) |
| `lg/issue-cache/<key>.txt`, `<key>.missing` | `dispatch read --issue <key>`, or Dispatch's NOT_FOUND |
| `lg/issues.tsv` | key, project, repository of every issue read (repository empty where Dispatch names none) |
| `lg/pr_issue.tsv` | merge sha, number, issue (empty for none) of every merged pull request, by attribution's sources 3 and 4 |
| `lg/merge_keys.tsv` | the same issues in `14_keys.py`'s shape (sha, number, group, key or `-`, source `branch`, `title_body` or `none`), for the merges of the chain |
| `lg/r_git_added.tsv`, `lg/r_git_deaths.tsv`, `lg/r_git.json` | `replay.py git` over the chain (step 4) |
| `lg/an/git/data/` | `headline.sh`'s data directory: `slices/`, `prs_raw.json`, `01_fetch_prs.log` (01), `cohort.json` (02), `commits.json` and `replay_commits.tsv` (commits_tsv.py), `replay_added_cat.tsv`, `replay_deaths_cat.tsv`, `replay_added.tsv`, `replay_deaths.tsv`, `merge_keys.tsv` (14) |
| `lg/an/git/out/` | `02_cohort.txt`, `14_keys.txt`, `15_rework.txt`, `15_rework.json`, `16_window.txt`, `16_window.json` |
| `lg/an/git/rework30-off/` | `rework30.py --no-30d` on `14_keys.py`'s keys: `rework30.txt`, `rework30.json`, `rework30_pairs.tsv` |
| `lg/an/k5/data/` | links to the replay's files, and `merge_keys.tsv` linked to step 3's |
| `lg/an/k5/out/` | `rework30.py` on step 3's keys: `rework30.txt`, `rework30.json`, `rework30_pairs.tsv` (adder sha, remover sha, case, lines, gap in days) |
| `lg/an/checks.txt` | step 4's comparisons with the archives, one line each |
| `bin/` | `rework30.py` linked beside the archive's `replaylib.py`, which it loads from its own directory |
| `ai/shas.txt` | the archive's 2,518 commits of the second repository |
| `ai/diffs/<xx>/<sha>.diff.gz`, `ai/diffs/stats.jsonl`, `ai/fetch.log` | `fetch_diff.py`'s diffs, its record of each fetch, and its log (step 5) |

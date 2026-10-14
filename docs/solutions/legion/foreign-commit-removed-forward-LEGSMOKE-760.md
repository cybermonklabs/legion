---
title: "A foreign commit on the issue branch is undone forward, never rebased away"
category: legion
tags:
  - ci-red
  - foreign-commit
  - jj
  - implementer
date: 2026-10-13
status: active
module: issue branch
related_issues:
  - "LEGSMOKE-760"
  - "sjawhar/legion-smoke#571"
---

# A foreign commit on the issue branch is undone forward, never rebased away

When CI goes red because a commit you did not make landed on the issue branch, first read it: `gh api repos/<owner>/<repo>/commits/<red-sha> --jq '{author:.author.login, committer:.commit.committer.email, files:[.files[].filename]}'`. Confirm the author is not a Legion App and name the paths it touched.

Fetch and start a new change on the remote bookmark, `jj git fetch && jj new legion/<KEY>@origin`, so your fix and your handoff sit above the foreign commit on changes your own App authored; `handoff_complete` refuses a handoff carried by another App's commit, and the daemon re-authors only the working copy it hands you.

Undo the foreign content in one forward commit touching nothing else, push it as a code push, and never rebase, rewrite, `--allow-backwards` or force. The net pull request diff against the base is unchanged, so a changed-files criterion still holds.

Prove the fix with the red head as the negative control: the same check-runs and contents reads at the foreign SHA show the failure and the file, the reads at yours show success and 404. Record the foreign commit in your handoff (`foreignCommits`) with its author, path and the runs it failed.

A foreign commit whose committer is `noreply@github.com` was made through GitHub's web or API; it is not evidence that anything rewrote your commits. The rewrite symptom is a commit of another role carrying you as committer.

## Evidence

Pull request `sjawhar/legion-smoke#571`. The proof driver `sjawhar-agent[bot]` pushed `d712a117469e6aeb2ab776cbeac2ad56fa227944` ("Stage 4b proof: fail-on-demand", committer `noreply@github.com`) adding `.fail-me` (25 bytes) above the implementer's handoff `060df3e8`; job `gate` failed on runs 38478575797 (push) and 38478580823 (pull_request) and the daemon returned the issue to implementing. The implementer's working copy was still above `060df3e8`; `jj new legion/LEGSMOKE-760@origin` moved it above `d712a117`, `rm -- .fail-me && jj split -m "smoke: remove .fail-me so fail-on-demand passes" .fail-me` made `d60255ff5733770bb30e553fe41a3e4a599511bf`, the pre-push guard `jj diff --from legion/LEGSMOKE-760@origin --to @- --summary` printed `D .fail-me` only, and the net diff from `main` listed the marker and the handoffs with no `.fail-me`. At `d60255ff` all four runs were `success` (38478806726, 38478806836, 38478798781, 38478798790) and `contents/.fail-me?ref=d60255ff` answered 404; at `d712a117` the same read answered 200 `{size: 25}` with `gate` failure twice.

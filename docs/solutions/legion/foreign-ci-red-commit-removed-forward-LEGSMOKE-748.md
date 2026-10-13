---
title: "A foreign commit that turns CI red on the issue branch is removed forward, never rewritten"
category: legion
tags:
  - ci-red
  - foreign-commit
  - jj
  - fail-on-demand
  - implementer
date: 2026-10-13
status: active
module: legion-workflow
related_issues:
  - "LEGSMOKE-748"
  - "sjawhar/legion-smoke#569"
---

# A foreign commit that turns CI red on the issue branch is removed forward, never rewritten

When the daemon returns an issue to implementing because a check settled red at a head you did not
push, treat the foreign commit as part of the branch's history and undo its effect in a new commit
above it.

- Read the red head first: `gh api repos/<o>/<r>/commits/<red-sha> --jq '{author:.author.login,
  committer:.commit.committer.email, files:[.files[].filename]}'` names who pushed it and what it
  touched; `gh api repos/<o>/<r>/commits/<red-sha>/check-runs` names the failing job.
- Start a fresh change on the remote tip (`jj git fetch && jj new legion/<KEY>@origin`), make one
  commit that undoes only the foreign change (`jj split -m "<message>" <path>`), run the
  allow-list pre-push guard, and push it as a code push with no `skip-checks` trailer. Never
  rebase, squash or force over the foreign commit: another tree may be built on the branch, and
  the daemon's record of the head is the pushed one.
- The identity check in `skill://legion-worker` covers only commits you made: a foreign commit
  with committer `noreply@github.com` is recorded under `foreignCommits` in `implement.json` and
  reported to the architect as information, not as a pane-identity fault.
- The red run at the foreign head is the negative control for the workflow proof at the fix head;
  cite both.

## Evidence

- LEGSMOKE-748, PR sjawhar/legion-smoke#569. The proof driver `sjawhar-agent[bot]` pushed
  `ef7a4ae8` (`Stage 4b proof: fail-on-demand`, adds `.fail-me`, 25 bytes) on top of the
  implementer's handoff `cd4b3498`; `fail-on-demand`/`gate` failed there (runs 38417836167 and
  38417825614) and the daemon's task line named the red head and check.
- Fix `52b0877c` (`smoke: remove .fail-me so fail-on-demand passes`) deleted `.fail-me` only;
  the guard printed only `smoke/LEGSMOKE-748.md` and the handoffs; `gate` and `check` succeeded
  there (runs 38418159247, 38418159253, 38418150962, 38418151001) and `.fail-me` answered HTTP 404
  at that head while HTTP 200 (size 25) at `ef7a4ae8`.
- The net pull-request diff never listed `.fail-me`, so a changed-files criterion evaluated on the
  net diff was unaffected by the add-then-delete.

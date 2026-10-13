---
title: "Remove a foreign CI-red fixture commit forward, never by rebase"
category: legion
tags:
  - legion-ci-red
  - foreign-commit
  - fail-on-demand
  - carried-verdict
date: 2026-10-13
status: active
module: smoke
related_issues:
  - "LEGSMOKE-728"
  - "sjawhar/legion-smoke#565"
---

# Remove a foreign CI-red fixture commit forward, never by rebase

When the daemon returns an issue to implementing because CI settled red at a head you did not
push, first read the red commit: `gh api repos/<o>/<r>/commits/<sha> --jq '{author:.author.login,
committer:.commit.committer.email, files:[.files[].filename]}'`. A commit by another bot identity
that adds only a fixture path (here `.fail-me`) is not an identity incident on your chain.

Fix it forward: `jj git fetch && jj new legion/<KEY>@origin`, delete the fixture, and
`jj split -m "<message>" <path>` so the commit touches that one path and nothing else. Push it
as a code push with no `skip-checks` trailer. Never rebase, squash or rewrite the foreign commit;
it stays in the chain between your handoffs and nets out of the pull request diff.

Before the push, run the branch guard `jj diff --from main@origin --to @- --name-only` and
confirm the fixture path is gone from the branch-wide diff. Record the foreign commit in
`implement.json` under `foreignCommits` (SHA, author, committer, files, effect, resolution) and
use its red run ids as the negative control of the workflow proof at the new code head.

Cite CI only at code heads. A handoff-only push carries `skip-checks: true` and starts no run, so
a CI claim at a handoff head is the nearest code head's verdict carried by the daemon; name that
code head and its run ids (push and pull_request events both) rather than writing "CI green".

## Evidence

- Foreign commit `74373aabc8812bf759beeff22655161d7d60db76` (author `sjawhar-agent[bot]`,
  committer `noreply@github.com`, files `[.fail-me]`) landed on `legion/LEGSMOKE-728` above the
  implementer's handoff `011f68d9`; `gate` failed on both events (runs 38369859121, 38369866844).
- The fix `f68b72c77e14a2643387eebfc6e6ad62bbd40e7e` touched only `.fail-me`; both workflows
  then passed on both events (runs 38370275720, 38370281054, 38370275529, 38370281020), the
  contents API answered 404 for `.fail-me` at `f68b72c7` and 200 at `74373aab`, and
  `gh pr diff 565 --name-only` no longer listed it.
- `gh run list --commit <handoff-sha>` printed nothing at every trailered handoff head
  (`011f68d9`, `8b3333ae`, `bdf7c8ac`), which is why the body's `CI:` line names the code head.

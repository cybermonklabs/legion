---
title: "Legion phase-worker jj handoff-commit hygiene and remote-surface proof discipline"
category: legion
tags:
  - legion
  - jj
  - handoff
  - verification
  - ci
date: 2026-09-24
status: active
module: packages/daemon
related_issues:
  - "S392088555-1"
  - "sjawhar/legion-smoke#144"
---

# Legion phase-worker jj handoff-commit hygiene and remote-surface proof discipline

Distilled from PR #144 (issue S392088555-1), an append-only README change driven through
plan → implement → 3 review rounds → test → retro. The code change is trivial; the durable
value is the phase-worker mechanics that repeat on every issue.

## jj handoff-commit isolation

`legion handoff complete` refuses to finish while `.legion/<phase>.json` shows as an
uncommitted change in the working-copy commit:

```
legion handoff complete: .legion/implement.json has changes in the working copy that are not committed
```

Root cause: in jj the working copy **is** `@`. If you write the handoff file while `@` is
still your described code commit, the handoff is absorbed into that commit, and the completion
check sees a dirty working copy.

Do this instead, every phase:

1. Make the code change; `jj describe @ -m "<code commit>"`.
2. `jj new` (or `jj split <handoff-path>`) so the handoff lands in its **own** commit.
3. `legion handoff write --phase <p> …`; the change now sits in a fresh `@`.
4. `jj describe @ -m "<p>: record handoff"`; move the bookmark to `@`.
5. Push (implementer only); then `jj new` to leave an empty working copy before
   `legion handoff complete`.

`legion handoff write` accepts only pipeline phases (`plan`/`implement`/`test`/`review`).
`--phase retro` is rejected exactly like a bogus phase (exit 2, usage) — retro writes no
`.legion` file; its durable output is this `docs/solutions/` doc plus a Dispatch retro message,
finished with `legion handoff complete` alone.

## headSha in a handoff cannot name its own commit

The handoff file lives inside the branch-tip commit, so it can never record that tip's SHA as
`headSha`. Reference the stable **code** commit (the implementation commit) in the handoff, and
cite the branch tip only in external artifacts (PR body CI/E2E lines), which are edited after
the push.

## Only the implementer App can push; sibling handoffs ride your next push

`legion-implementer[bot]` is the sole App with `contents` permission. Tester and reviewer
handoff commits accumulate **unpushed** in the shared workspace and appear in your local
`jj log` chain; they ride the implementer's next `jj git push`. Expect them, push forward-only,
and never rebase without a real GitHub `CONFLICTING`.

## Proof must hit the real remote surface with a negative control

Verify against the pushed remote tip, not the local worktree:

```
legion gh -- api "repos/<owner>/<repo>/contents/README.md?ref=<branch-tip>" \
  --jq .content | base64 -d | grep -nF '<exact appended line>'   # exit 0
```

Pair every positive check with a negative control (grep a sentinel that was never appended →
exit 1). For an edit this easy to fake-pass, the negative control is what makes "green" mean
something. Also assert AC2 pre-merge (`pr view … --json state,mergeCommit` → OPEN, null) to
prove the branch-protection gate is real and nothing self-merged.

## CI stub caveat in this fixture

`.github/workflows/ci.yml` is `sleep 20 && exit 0` — passing it proves nothing about content.
The controllable gate is `.github/workflows/fail-on-demand.yml`, which fails iff a `.fail-me`
file exists. Use that file, not `ci.yml`, to exercise red→green CI.

## One commit per review round

Answer each `CHANGES_REQUESTED` with a fresh corrective commit, never an amend of a pushed
base. The `.legion/implement.json` audit trail keeps distinct round commits and a stable
`filesChanged == [README.md]` invariant across the whole PR.

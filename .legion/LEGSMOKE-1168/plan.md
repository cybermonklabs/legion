# Plan: LEGSMOKE-1168 — Upgrade rehearsal push child

Issue: dispatch://LEGSMOKE-1168 (child of dispatch://LEGSMOKE-1163). Repository: sjawhar/legion-smoke, default branch `main`. Issue branch: `legion/LEGSMOKE-1168`. Fork point and base tip at planning time: `6d843f28732dd82163065cb4d2248e488f68ca0b` (`smoke: add LEGSMOKE-1070 marker file (#767)`).

## What was read

- Child spec `dispatch://LEGSMOKE-1168/artifact/spec`: empty (version 1, no body). Issue title, verbatim from `dispatch read --issue LEGSMOKE-1168`: `Upgrade rehearsal push child (/tmp/legion-e2e4b.1612995.E10GrqVK)`.
- Root spec `dispatch://LEGSMOKE-1163/artifact/spec`: "add one new Markdown file under `smoke/` holding a single line that names this issue"; scope: "A review of the pull request may ask for one more line appended to that same file; that is in scope. Nothing else changes."
- Convention: `smoke/LEGSMOKE-1070.md` is one line `LEGSMOKE-1070: <issue title>` ending in `\n` (126 bytes); commit `smoke: add LEGSMOKE-1070 marker file`.
- CI: `.github/workflows/ci.yml` job `check` (`sleep 20 && exit 0`); `.github/workflows/fail-on-demand.yml` job `gate` (fails only when `.fail-me` exists at the repo root). Both run `on: [push, pull_request]`, so each push to a branch with an open PR lists two check-runs per job at the same SHA.
- `docs/solutions/legion/prove-a-docs-only-change-by-its-bytes-at-an-explicit-sha-LEGSMOKE-1021.md`, `…/put-jq-flags-inside-the-filter-for-the-pane-gh-wrapper-LEGSMOKE-1070.md`, `…/measure-the-issue-branch-from-its-fork-point-LEGSMOKE-1016.md`: the proof pattern below follows them.
- `AGENTS.md`: every role runs `mkdir -p /tmp/legion-fixture && touch /tmp/legion-fixture/agents-md` before anything else.
- No `.legion/` directory existed at base; this plan's handoff is the first.

## Decision: the child inherits the root spec

The child spec is empty, so the change is the root spec's one-file marker, named for this child. This is a reading, not a departure: `specDepartures` is `[]`.

## Skills

- implement: `legion-worker` (+ `references/pr-body.md`, `references/default-branch-check.md`). No repository skill fits (`fixture-claude-skill`/`fixture-omp-skill` are Stage 4b fixture markers, not domain skills).
- test: `legion-worker` (+ `references/pr-body.md` for what the tester verifies and how it records its proof).
- review: `legion-worker` (+ `references/merge-gate.md`, `references/review-threads.md`).

## The change (implementer)

1. `cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" new legion/LEGSMOKE-1168@origin` — build on the pushed plan handoff commit. Never rewrite, rebase, squash or discard that commit: it is the only carrier of this plan.
2. Create `smoke/LEGSMOKE-1168.md` with exactly these bytes (81 bytes, one `\n`, no CR, no BOM):

   ```
   LEGSMOKE-1168: Upgrade rehearsal push child (/tmp/legion-e2e4b.1612995.E10GrqVK)
   ```

   sha256 `370a5ac5c0841daa96311ed13d91eb695aa481e4f485e93a558a44aa696a88fd`.
3. Commit: `jj -R "$LEGION_WORKSPACE" split -m "smoke: add LEGSMOKE-1168 marker file" smoke/LEGSMOKE-1168.md`.
4. Push (code push: no `skip-checks` trailer): `jj bookmark set legion/LEGSMOKE-1168 -r @- && jj git push --bookmark legion/LEGSMOKE-1168`. Record the head SHA as `firstCodeHead` in `implement.json`.
5. `gh pr create` from the `pr-body.md` template; body contains the line `Dispatch: LEGSMOKE-1168`; title `smoke: add LEGSMOKE-1168 marker file`.
6. Never create `.fail-me`, in any phase.

## Acceptance criteria, surface, command, expected output

All reads go to GitHub at an explicit SHA (the production-like surface for a repository file). `<base>` = `6d843f28732dd82163065cb4d2248e488f68ca0b`; `<head>` = the code head being judged. Keep jq flags inside the filter (`| tostring`), never between `--jq` and the filter.

- **AC1 — the marker file exists at the head with the exact bytes.**
  Command: `gh api 'repos/sjawhar/legion-smoke/contents/smoke/LEGSMOKE-1168.md?ref=<head>' -H 'Accept: application/vnd.github.raw' > /tmp/m.md && wc -c < /tmp/m.md && wc -l < /tmp/m.md && tail -c1 /tmp/m.md | od -An -tx1 && sha256sum < /tmp/m.md`
  Expected: `81`, `1`, ` 0a`, `370a5ac5c0841daa96311ed13d91eb695aa481e4f485e93a558a44aa696a88fd`.
  Negative control (read-only, pinned so it does not decay after merge): `gh api 'repos/sjawhar/legion-smoke/contents/smoke/LEGSMOKE-1168.md?ref=<base>'` → `HTTP 404`, exit 1. Second control: compare against the altered string `LEGSMOKE-1168x: …` → mismatch.
- **AC2 — nothing else changes.** The diff from base to head, excluding `.legion/`, is the single added path.
  Command: `gh api 'repos/sjawhar/legion-smoke/compare/<base>...<head>' --jq '[.files[] | select(.filename | startswith(".legion/") | not) | {filename,status,additions,deletions}] | tostring'`
  Expected: `[{"additions":1,"deletions":0,"filename":"smoke/LEGSMOKE-1168.md","status":"added"}]` (after an in-scope review append: `"additions":2`).
- **AC3 — CI is green at the head.**
  Command: `gh api 'repos/sjawhar/legion-smoke/commits/<head>/check-runs' --jq '[.check_runs[] | {name,status,conclusion,id}] | tostring'`
  Expected: `check` and `gate` each `completed`/`success` (two entries per job: the `pull_request` pair and the `push` pair; cite the `pull_request` pair in the CI line). An entry with `status` other than `completed` is pending (the `ci` workflow's `check` job sleeps 20 s): wait and re-read; it is not a failure. A trailered handoff-only head has zero check-runs by design; judge CI at the code head.
- **AC4 — the PR links its issue.**
  Command: `gh pr view <n> -R sjawhar/legion-smoke --json body --jq '.body' | grep -c '^Dispatch: LEGSMOKE-1168$'` → `1`.
- **AC5 — the plan commit survived the upgrade rehearsal and precedes the code.**
  Command: `jj -R "$LEGION_WORKSPACE" log -r '<plan-sha>::legion/LEGSMOKE-1168@origin' --no-graph -T 'commit_id.short() ++ "\n"'` lists `<plan-sha>` first; `<plan-sha>` is recorded in `plan.json` (`planCommit`) once the planner's push lands.
- **AC6 (conditional) — a review-requested append.** If the reviewer asks for one more line, the file becomes exactly the planned line plus the reviewer's verbatim text, each `\n`-terminated; the binding assertion at merge is that two-line content. The implementer records the new `wc -c`/sha256 in its `proof`; the tester re-runs AC1 with the updated expectations and AC2 expects `"additions":2`. No other edit is in scope.

## Proof ordering for this issue's role in the Stage 4b proof

This issue rehearses a daemon upgrade while a planner's handoff commit sits unpushed. Ordering: (1) planner commits `plan: record handoff` locally, replies `PUSH_READY`, waits; (2) on the driver's next targeted message the planner pushes that commit (handoff-only: trailer `skip-checks: true` after two blank lines) and calls `handoff_complete`; (3) the implementer starts on `legion/LEGSMOKE-1168@origin` (step 1 above). AC5 checks the result. If the planner's push is refused because the remote moved, the planner reports the refusal to the architect and never forces.

## Evidence each role records

- Implementer (`implement.json` `proof`, PR `E2E (implementer)` line): AC1 with both controls, AC2, AC3 (run ids), AC4, at `firstCodeHead`; `headSha` is that SHA.
- Tester (`test.json`): re-runs AC1 (including through the JSON `.content` route at the handoff head, if any), AC2, AC3, AC4 at the head it judges; tries to break it (altered-string control; `?ref=<base>` 404); `failures: []` and its own `proof` when none. It reruns no lint/unit suite: CI is the static read.
- Reviewer (`review.json`): approves the head whose checks it read on GitHub; may request the one in-scope append (AC6); any other request is out of scope and goes to the architect.
- Merger: READY names the head whose AC3 read is green; `.legion/LEGSMOKE-1168/` is removed by retro before that.

## Risks

- A push while the remote moved (another role's push) is refused by `jj git push`: report, never force.
- Reading the file from the shared checkout instead of at a SHA would judge whatever phase is active: every read above names `?ref=<sha>`.

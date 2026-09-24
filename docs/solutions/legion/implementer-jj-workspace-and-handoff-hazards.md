---
title: "Legion implementer: jj-workspace and handoff hazards for single-file proof PRs"
category: legion
tags:
  - legion-worker
  - jj-workspace
  - handoff
  - e2e-proof
  - negative-control
date: 2026-09-23
status: active
module: legion-worker/implementer
related_issues:
  - "S393183870-1"
  - "sjawhar/legion-smoke#122"
---

# Legion implementer: jj-workspace and handoff hazards for single-file proof PRs

Grounded in issue S393183870-1 / PR sjawhar/legion-smoke#122: a one-line README append
(`Stage 3 smoke: Legion workflow proof`, +1/-0) that nevertheless surfaced several reusable
Legion-worker mechanics hazards. The product diff stayed byte-identical through three proof
correction rounds; every hazard below cost real rework.

## 1. Never write scratch files inside `$LEGION_WORKSPACE` before `jj new`

`jj` snapshots the working copy into the current `@` commit on the next jj command. A handoff
`--data` JSON or a PR-body markdown file written into the workspace with the `write` tool is
therefore committed **into whatever `@` currently is** — in this issue it polluted the README
product commit with `.legion/implement-data.json` and `.legion/pr-body.md`, forcing a
`jj -R "$LEGION_WORKSPACE" restore -c <commit> <paths>` cleanup and a re-push.

- Write all transient scratch (handoff `--data`, PR-body text) under `/tmp`, never under the
  workspace. `legion handoff write --data "$(cat /tmp/impl.json)"` reads `/tmp` fine.
- Only real deliverables (the product change, `docs/solutions/` docs) belong in the workspace.

## 2. `legion handoff write --data` rejects the base fields

The tool injects `schemaVersion`, `phase`, and `completed` itself and rejects them in `--data`
(`handoff data field schemaVersion is not allowed`). Pass **only** phase-specific fields
(for implement: `filesChanged`, `proof`, `deviations`, `openQuestions`, ...).

## 3. `legion handoff complete` refuses a handoff that lives in the working-copy commit

The handoff file must sit in a **finalized** (non-`@`) commit. If `.legion/<phase>.json` is the
content of `@` itself, complete errors:
`.legion/<phase>.json has changes in the working copy that are not committed`. After
`legion handoff write` + `jj describe`, run `jj -R "$LEGION_WORKSPACE" new` so the handoff commit
becomes `@-` and the working copy is empty; then `legion handoff complete` succeeds. (This is why
the skill's completion gate uses `jj split` — it leaves an empty `@` on top.)

## 4. The working-copy snapshot rewrites the `@` commit's SHA

After you push a commit, the next jj command that snapshots while `@` still points at it rewrites
its committer timestamp, producing a **new SHA** — the pushed remote and the local commit diverge
(e.g. `48d71cb5` locally became `bc76a6e9`, tree identical). Consequences:

- Put the **product commit's** SHA (stable once `@` moves off it) in `proof.headSha`, not the
  transient working-copy SHA, and use the **full 40-char** SHA (a truncated prefix is ambiguous
  and drew a review round here).
- Expect to re-push after moving `@`; the PR head advances but the product tree is unchanged.

## 5. Scope the negative control to the product file's own hunk

A Legion PR carries the `.legion/*.json` handoffs, and `implement.json` contains the
negative-control's **own description text**. A whole-PR-diff `grep` for the deliberately-wrong
marker therefore matches the handoff file, not the product — a false positive that cost a
correction round (the tester independently flagged the same thing). Isolate the product file's
diff hunk first:

```bash
legion gh -- pr diff <n> --repo <owner>/<repo> \
  | awk '/^diff --git a\/README.md/{f=1;next} /^diff --git/{f=0} f'
```

Prefer a **count-based** negative control over a single wrong-string check: assert
`added_lines == 1` and `removed_lines == 0` on the isolated hunk. Counts catch *any* drift (an
extra added line, a removed line, a modified existing line), not just one anticipated bad string.

## 6. Proof-only correction rounds must never touch the product

Across three CHANGES_REQUESTED rounds the corrective commits touched `.legion/implement.json`
only; `README.md` stayed `+1/-0` with the exact marker. Keep the discipline explicit: proof/handoff
fixes live in `.legion/`, product changes live in the product file, and each corrective commit's
message says which. This keeps the approvable head's product diff transparent and prevents
accidental product drift while iterating on proof quality.

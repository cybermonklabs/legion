---
title: "Smoke-proof learnings after the fixture reset: where the previous run's record lives, and what the sandbox shell refuses"
category: legion
tags:
  - legion-workflow
  - proof
  - smoke-repository
  - handoff-schema
  - gvisor
  - fail-on-demand
date: 2026-09-30
status: active
module: legion-smoke
related_issues:
  - "LEGSMOKE-331"
  - "sjawhar/legion-smoke#452"
  - "LEGSMOKE-327"
  - "sjawhar/legion-smoke#449"
---

# Smoke-proof learnings after the fixture reset: where the previous run's record lives, and what the sandbox shell refuses

Retro of LEGSMOKE-331 (Stage 4b proof tree 1: the whole workflow), delivered as pull request
sjawhar/legion-smoke#452, which adds `smoke/LEGSMOKE-331.md` holding one line. It is the second
run of the same one-file change; the first (LEGSMOKE-327, #449) recorded the proof surface, the
`wc -l` trailing-newline gate, the head-per-push CI table, and the plan's lag on the `.legion/`
deletion in `docs/solutions/legion/one-file-smoke-change-proof-under-the-go-daemon.md`. This
document does not repeat that; it records what the second run learned that the first could not.

## A learning committed here does not reach the next run's planner

The first run's retro document was merged in #449 (commit `84dd473f`) and removed from `main`
one commit later by the fixture reset (`c48f01b1`, "proof fixture: remove the handoffs and
learnings Stage 3 runs merged"). This run's planner wrote `.legion/plan.json` against that
`main`, where `docs/` does not exist (`jj file list -r main@origin docs` prints nothing), and the
plan repeated the exact lag the first document warned about: `evidence.integration` and
`evidence.review` describe an implementer `.legion/` deletion push before APPROVE, which the Go
daemon does not have until Stage 7. The implementer's `notes[2]` and the reviewer's
`thermo.codeQuality.advisory` each had to override it again.

What follows for a future worker on this repository:

- `docs/solutions/` on `sjawhar/legion-smoke` is read by the reviewer and merger of the same pull
  request and by nobody after the fixture reset. A learning that must change how the **next**
  planner or reviewer behaves belongs in the Legion repository's role text or skills (or its
  own `docs/solutions/`), and the retro message on the Dispatch issue is the pointer to it. Write
  the smoke-repository document anyway — it is the retro's durable output for this pull request —
  but do not expect it to be found by the next tree.
- The previous run's documents and handoffs are still readable at the merge commit even though
  `main` no longer carries them: `jj -R "$LEGION_WORKSPACE" log -r 'ancestors(main@origin, 30) &
  files(glob:"docs/solutions/**")'` lists the merge commits, and `jj file show -r 84dd473f1445
  docs/solutions/legion/one-file-smoke-change-proof-under-the-go-daemon.md` prints the document.
  Dispatch search does not index the content of these files; go to jj directly.

## The handoff schema is in the previous run's merge commit

`legion handoff write --help` prints only `--phase`, `--data`, and `--workspace`; it does not
print the phase's schema, and `handoff_write` reports a missing or malformed `proof` only after
you submit. The fastest reliable template is the previous run's committed handoff of the same
phase, read from its merge commit: `jj -R "$LEGION_WORKSPACE" file show -r 84dd473f1445
.legion/implement.json` (likewise `test.json`, `review.json`). This run's implement handoff
copied that shape (`summary`, `precondition`, `commits[]`, `filesChanged`, `pullRequest`,
`ci.runs[]`, `localChecks`, `proof[]` with `criterion`/`surface`/`command`/`observed`/`headSha`/
`link`/`negativeControl`, `notes[]`) and was accepted on the first write.

## Process substitution fails in the worker sandbox

The worker pane runs under gVisor. `cmp <(sed 's/LEGSMOKE-327/LEGSMOKE-331/'
smoke/LEGSMOKE-327.md) smoke/LEGSMOKE-331.md` failed with `cmp: /dev/fd/59: Illegal seek (os
error 29)`: `cmp` seeks its inputs, and a process-substitution file descriptor is a pipe. Feed
the tools from stdin instead, which both this run's implementer and tester did successfully:

```bash
sed 's/LEGSMOKE-327/LEGSMOKE-331/' smoke/LEGSMOKE-327.md | diff - smoke/LEGSMOKE-331.md
printf '%s\n' 'LEGSMOKE-331: Stage 4b proof tree 1: the whole workflow' | cmp - smoke/LEGSMOKE-331.md
```

`od -c` on the committed file (`jj file show -r @- <path> | od -c`) remains the direct check that
the final byte is `\n`.

## The approved head carried its own check runs this time

The first run's table showed the reviewer's push running CI in full; this run confirms what that
means for the approval: the reviewer pushed `.legion/review.json` with `verdict: approved`
(`45475fdc`), waited for its four check runs (`ci / check` 36705492649 and 36705489418,
`fail-on-demand / gate` 36705492654 and 36705486385), and then submitted APPROVE naming that SHA.
The tester's head `6064d1da` in between has zero check runs (`skip-checks: true`), so
`legion gh -- pr checks 452` at that SHA prints `no checks reported`; `.legion/review.json`
`ci.reviewedHead` records why. A merger comparing the tip against the approved head should expect
the approved head to have runs and the tester's head not to, and should not read the latter as
"CI never ran". Each phase's `proof[].headSha` names the pull request head at the time that phase
ran (implement `faa32c0e`, test `bb324c60`, review `6064d1da`); `codeHeadSha` is the same
`faa32c0e` in every handoff and is the value to compare across phases.

## Not exercised

- `fail-on-demand / gate` has only ever been proven in its passing direction on this repository:
  every negative control shows `.fail-me` absent (HTTP 404, `jj file list` empty) and the job
  green. No run has planted `.fail-me` on a head to show the job turn red. A proof that needs a
  red-then-green trajectory must exercise that branch deliberately, on a throwaway head, and
  record the run id.
- Wall time of this tree from admission to APPROVE: 10:31Z to 10:58Z, five pushes and sixteen
  workflow runs for a one-line file; the retro commit is the sixth push and also runs CI in full.
  The `ci / check` job's `sleep 20` is the floor of every push.

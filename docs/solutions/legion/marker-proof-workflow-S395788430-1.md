---
title: "Byte-exact marker proofs and handoff-push mechanics in Legion smoke issues"
category: legion
tags:
  - legion
  - smoke-proof
  - jj
  - skip-checks
  - acceptance-criteria
date: 2026-10-19
status: active
module: legion-worker
related_issues:
  - "S395788430-1"
  - "sjawhar/legion-smoke#667"
---

# Byte-exact marker proofs and handoff-push mechanics in Legion smoke issues

Rules for a worker handling a single-file marker/proof issue in a Legion smoke repository
(`ci` + `fail-on-demand` workflows, `.legion/<issue>/` handoffs travelling on the branch).

## Create and verify byte-exact markers

- Write a marker line with `printf`, not a typed or copied character: `printf 'S<KEY> \xe2\x80\x94 <title>\n'` (em dash U+2014 = bytes `E2 80 94`).
- Verify every time with two checks: `diff <(printf '…\n') file` must be empty, and `cat -A file` must show `M-bM-^@M-^T` for the em dash and a trailing `$` on every line.
- Enforce exactly one trailing LF per line: `wc -l` must equal the intended line count; no CRLF, no blank lines.
- Use the reviewer's quoted bytes verbatim for appended review-round lines, not the plan's predicted wording; confirm with a negative-control diff against the wrong variant.
- Watch the suffix convention: the title keeps the full issue id (`S395788430-1 — …`) while the parenthetical key in review lines drops the `-1` (`… (S395788430)`). Follow sibling precedent; do not normalize.

## Scope file-list acceptance criteria per-commit, never per-PR

- `.legion/<issue>/*.json` handoffs are committed on the branch and show up in `gh pr diff --name-only` alongside the real change; retro removes them before merge.
- An AC of the form "exactly one file added" holds only at the per-commit level: assert it with `jj diff -r <commit> --stat`, not against the PR-level file list.

## CI is non-discriminating — inspect the diff

- `ci`/job `check` sleeps and always passes; `fail-on-demand`/job `gate` reddens only when a `.fail-me` file exists. Green CI is not proof.
- Make byte-exact diff inspection the real acceptance check. Record `.fail-me` absent as the gate responsiveness negative control.
- The PR head is often a handoff-only commit distinct from the verified code head; the daemon carries the code head's verdict forward. Read checks on the code SHA: `gh api .../commits/<codeSha>/check-runs`. Never rerun CI to confirm.

## The skip-checks trailer survives the jj Omp-Session overlay

- A handoff-only push (commits touching only `.legion/`) needs `skip-checks: true` as the **final** line of the head commit message, preceded by **two** blank lines, so GitHub starts no workflow.
- The pane's jj overlay appends an `Omp-Session:` trailer to commit messages. Running `jj describe -r @- --config 'templates.commit_trailers=""' -m "<existing description>"$'\n\n\nskip-checks: true'` does **not** strip an `Omp-Session:` trailer already present in the existing description — it only prevents appending a new one. The result is `…\n\nOmp-Session: …\n\n\nskip-checks: true`: skip-checks is still the last line after two blank lines, so GitHub honors it.
- Verify ordering with `jj log -r @- -T description | cat -A` after the describe rather than assuming the config cleared trailers.

## The standing bot thread

- A `sjawhar-agent` bot opens one review thread ("is this file change needed?"). Answer it once as not-a-defect citing the approved spec, resolve it with the author's own `gh` (`resolveReviewThread`), and record it in the PR body `Threads` section. It stays resolved across later rounds.

## Evidence

- PR sjawhar/legion-smoke#667, issue S395788430-1: marker `smoke/S395788430-1.md` created byte-exact, then three `Legion proof review round N (S395788430)` lines appended one per review round (final 4 lines).
- Plan gap analysis F1 (byte-exact), F2 (trailing LF), F5 (CI non-discrimination), F6 (predicted review text `Stage 3 review round N` differed from the reviewer's actual `Legion proof review round N`).
- Implement/test proofs: negative control diffing the ASCII-hyphen variant `S395788430-1 - …` returned a non-empty diff; `.fail-me` absent kept the gate green; checks read on code head `ca054fd8` while PR head was handoff-only `f66c886f`.
- Handoff-only pushes used `skip-checks: true` as the final line after two blank lines; `jj describe --config 'templates.commit_trailers=""'` left the pre-existing `Omp-Session:` trailer in place with skip-checks still last.

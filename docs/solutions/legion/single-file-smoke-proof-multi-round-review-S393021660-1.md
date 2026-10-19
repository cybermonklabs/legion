---
title: "Single-file smoke proofs with no-op CI and multi-round review"
category: legion
tags:
  - smoke-proof
  - acceptance-criteria
  - proof-negative-control
  - multi-round-review
  - no-op-ci
date: 2026-10-19
status: active
module: smoke
related_issues:
  - "S393021660-1"
  - "sjawhar/legion-smoke#694"
---

# Single-file smoke proofs with no-op CI and multi-round review

Rules for a worker whose deliverable is one `smoke/<KEY>.md` proof file (or any tiny docs-only
change) in a repository whose CI gates nothing about the changed path.

- Prove the acceptance command on the PR head, not in CI. When CI is a no-op, say so explicitly
  and treat the acceptance shell command as the sole authoritative machine check; re-run it at
  each head you push.
- Pair every `grep` proof with a negative control that would fail on a plausible wrong input, so
  the match is never vacuously true. Prefer `grep -qx` against the full expected line over a
  prefix match when the acceptance text (not just an id prefix) is load-bearing; make the negative
  control vary the part you actually care about.
- Follow the approved acceptance's literal form (here, a colon separator and its own grep) over an
  inferred peer convention, and verify any peer-convention claim against the real files before
  citing it as a conflict.
- Keep the first line load-bearing: append each review-round line to the end of the file, one per
  round, leaving prior lines untouched. Re-prove that line 1 is unchanged after each append.
- A tester head SHA above the implementer's proven SHA is expected and benign when the only
  intervening commits touch `.legion/` (handoff-only). The file content is identical, so the
  tester independently re-running the same command at the handoff head still proves the same tree;
  do not treat that offset as proof drift.

## Evidence

- `.github/workflows/ci.yml` is `sleep 20 && exit 0` and references nothing under `smoke/`;
  plan/implement/test handoffs each re-ran `test -f smoke/S393021660-1.md && head -1 … | grep -q '^S393021660-1:'`
  on the head and recorded exit 0.
- Every handoff recorded the negative control `grep -q '^NOPE:' -> exit 1`; the tester added
  `grep -qx 'Legion proof review round 4 (S393021660)' -> exit 1` to prove no spurious round line.
- `plan.json` flagged a "colon vs em-dash" peer conflict, but the actual peers
  (`smoke/LEGSMOKE-327.md`, `smoke/S393189958-1.md`) both use the colon form; deferring to the
  self-consistent acceptance grep was the correct tiebreak and the "conflict" was overstated.
- The final file is four lines: `S393021660-1: Primary durable workflow profile` then
  `Legion proof review round {1,2,3} (S393021660)`; `test.json` proves line 1 unaffected by the
  three appends and rounds 1–2 preserved above round 3.
- `implement.json` pinned the round-3 code head `cf28ac956ff2`; `test.json` pinned
  `68db6f57b55b`, the handoff-only commit above it — identical file content, so the tester's
  independent re-run proved the same tree.

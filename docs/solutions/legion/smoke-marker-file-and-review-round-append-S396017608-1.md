---
title: "Smoke marker file and per-round review-line append (Legion Stage-3 proof)"
category: legion
tags:
  - smoke-proof
  - review-round-append
  - exact-line-match
  - handoff-only-push
date: 2026-10-19
status: active
module: smoke
related_issues:
  - "S396017608-1"
  - "sjawhar/legion-smoke#638"
---

# Smoke marker file and per-round review-line append (Legion Stage-3 proof)

A Legion Stage-3 durable-workflow proof adds one Markdown marker file `smoke/<KEY>.md`: a naming
line plus one appended line per review round. The product surface is trivial; the value is a clean
run through the phase pipeline. These rules keep the acceptance checks green and the diff minimal.

## Rules

- **Type the naming line's separator as a literal em-dash, byte-for-byte.** The line is
  `<KEY> — <title>` where `—` is U+2014 with one ASCII space on each side — never a hyphen-minus,
  never an en-dash, never a smart-quoted dash. The acceptance check is
  `grep -qx '<KEY> — <title>' smoke/<KEY>.md`; `grep -qx` anchors the whole line, so any dash or
  spacing substitution fails with exit 1 while looking identical in most editors. Copy the dash
  from a sibling precedent rather than retyping, and confirm with `grep -qx` on the literal string
  before handoff.

- **Each review round appends exactly one line, and its wording comes from that round's reviewer
  comment — not from a precedent.** The round-line text is not a fixed template: some precedents
  use `Stage 3 review round N (<KEY>)`, others `Legion proof review round N (<KEY>)`. Read the
  exact string out of the round's reviewer instruction and append only that one line, changing
  nothing else. Prove scope with `jj diff --from <prev-head> --to <new-head> smoke/<KEY>.md`
  showing a pure `+1` diff and `wc -l` incrementing by one; a `+2` diff, a reordered line, or a
  changed trailing newline is a defect.

- **Separate code pushes from handoff-only pushes, and read CI on the code head.** Land the
  `smoke/` content in a commit that runs CI, then push the `.legion/<KEY>/*.json` handoff commit
  separately with a `skip-checks: true` trailer (last line, preceded by two blank lines) so it
  neither triggers nor needs CI. Because the handoff head skips CI, GitHub Actions results live on
  the code head; verify AC2 against that code SHA (checks `check` from `ci`, `gate` from
  `fail-on-demand`, both `success`). The gate's failure path is a planted `.fail-me` file; confirm
  none is in the diff. Never rerun a green lane — read it.

- **Handle the recurring bot review thread once.** A bot opens one thread per PR asking "is this
  file change needed?". It is needed — the spec asks for the marker file and its round lines. The
  implementer (PR author) replies on that thread declining the concern with a concrete
  justification and resolves it; the reviewer independently replies in its own words and names the
  thread id it accepted. Resolve once and keep it resolved across later rounds — do not reopen or
  re-answer each round, and do not answer as a top-level comment.

## Evidence

- PR sjawhar/legion-smoke#638, Dispatch issue S396017608-1. File `smoke/S396017608-1.md`: naming
  line `S396017608-1 — Primary durable workflow profile` plus `Legion proof review round 1/2/3
  (S396017608)`.
- Implementer and tester proofs both drove the GitHub branch surface (file read back via
  `gh api .../contents/...?ref=legion/S396017608-1 | base64 -d`) with `grep -qx`/`grep -nx` and a
  negative control against sibling `smoke/S394570180-1.md` (exit 1).
- Code head 46fb6363 carried both CI checks green; PR head 12e5401a was a handoff-only commit with
  `skip-checks: true`, so the verdict attached to the code head.
- Bot thread `PRRT_kwDOUCdo886t1tDh` answered and resolved in round 1, remained resolved through
  rounds 2 and 3; no new threads opened.

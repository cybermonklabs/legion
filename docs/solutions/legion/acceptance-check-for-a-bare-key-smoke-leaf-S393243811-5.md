---
title: "Designing a deterministic acceptance check for a bare-key smoke leaf"
category: legion
tags:
  - smoke
  - acceptance-criteria
  - proof
  - empty-spec
  - cmp
date: 2026-10-18
status: active
module: legion-worker
related_issues:
  - "S393243811-5"
  - "sjawhar/legion-smoke#615"
---

# Designing a deterministic acceptance check for a bare-key smoke leaf

A smoke leaf's deliverable in this repo is one file `smoke/<KEY>.md` whose first line is the bare
issue key and nothing else (here `S393243811-5\n`, 12 ASCII chars + one newline = 13 bytes). The
spec phrase "a single line that names this issue" has three in-repo readings — bare key,
`<KEY> — <title>`, `<KEY>: <sentence>` — and the bare key is the established precedent
(`smoke/S392703949-1.md`, `smoke/S393198222-1.md`). Pick it so the check needs no title the spec
never gave.

**Pin the content with `cmp` against a freshly printed reference, not an eyeballed diff.**
`printf '<KEY>\n' | cmp - smoke/<KEY>.md` exits 0 only on the exact bytes and is immune to a
trailing-newline or whitespace drift that `grep` or a visual read would miss. Build the reference
from the spec's own `printf`, never from the author's scratch file (that compares the author with
themselves).

**Scope the single-line / `wc -l`=1 assertion to the creation commit, never the branch tip.**
`jj file show -r <code-head> smoke/<KEY>.md | wc -l` must read the commit that created the file.
Review rounds legitimately *append* lines to the same smoke file — both cited precedents carry the
bare key on line 1 plus three appended `Stage 3 review round N` lines — so a tip-scoped line-count
assertion would wrongly fail an in-scope reviewed append. Pin the count to the creation SHA and say
so in the acceptance line.

**An empty leaf spec is not a license to invent requirements.** When the leaf's spec document is
empty, inherit the parent profile as the low-risk default and route interpretive ambiguity to the
architect as a plan decision rather than reading deliverables out of the title. Here the title
"Dependent admission leaf" suggested a dependency/ordering artifact, but the issue had no Dispatch
dependency edge (`References: none`; only `child_of S393243811-1`), and "admission" is the
daemon/controller gate already satisfied by the leaf reaching planning — not a repo deliverable.
The plan shipped a standalone one-file change and left the "populate the leaf spec vs. confirm
inheritance" call to the architect.

See also `docs/solutions/legion/self-naming-commits-and-which-head-carries-ci-SMOKE-3.md` for the
skip-checks head and reading `check-runs` at the code head: the one placement detail worth adding is
that `skip-checks: true` must be the commit message's **last** line, separated from the body by a
blank line, or GitHub starts workflows on the handoff head anyway.

## Evidence

- Issue S393243811-5, PR sjawhar/legion-smoke#615. One file `smoke/S393243811-5.md`, exactly
  `S393243811-5\n` (13 bytes).
- AC1 proved with `printf 'S393243811-5\n' | cmp - smoke/S393243811-5.md` (exit 0) and
  `jj file show -r 5e83824c smoke/S393243811-5.md | wc -l` = 1 at the creation commit.
- AC3 read `ci/check` and `fail-on-demand/gate` as `success` at code head `5e83824c`; the handoff
  head `c87b4104` carried a correctly-shaped `skip-checks: true` trailer, so GitHub ran no workflow
  on it and the daemon carried the code-head verdict.

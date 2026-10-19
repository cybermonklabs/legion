---
title: "Smoke -5 leaf markers use the bare-key form, not the -1 title form"
category: legion
tags:
  - docs-only-change
  - byte-exact-markdown
  - smoke-leaf-marker
  - naming-convention
date: 2026-10-19
status: active
module: legion-workflow
related_issues:
  - "S395788430-5"
  - "sjawhar/legion-smoke#668"
---

# Smoke -5 leaf markers use the bare-key form, not the -1 title form

Extends docs/solutions/legion/docs-only-marker-file-review-rounds-S392923308-1.md.

That document describes a `-1` root marker: first line `S<key> — <title>` (em-dash U+2014), with
per-round appended lines driven by reviewer threads. A depth-5 **leaf** marker is a different
convention, and the only decision that matters is which convention applies. The delta:

- A `smoke/S<child-key>-5.md` leaf marker is the bare child key plus a single LF and nothing else:
  `printf 'S<child-key>-5\n'`, exactly 13 bytes for an 11-character key. No em-dash title, no
  appended lines, no parent key. All sibling `-5` markers are byte-identical in this shape.
- Use the **child** key, never the parent's. `smoke/S<key>-5.md` holds `S<key>-5`; writing the
  parent key or the `-1` title form ships a wrong-but-plausible marker. The negative-control `cmp`
  that catches it differs at char 12 (the last digit before `-5` vs `-1`).
- A clean leaf typically passes with zero review rounds (no content is contested), so the
  append-per-round and per-round-thread mechanics of the `-1` doc do not apply: implement, prove,
  pass. Do not manufacture review-round appends for a leaf.
- The CI-verdict-at-code-head and single-file-guard-scoped-to-the-code-commit rules from the
  parent document hold unchanged here (the PR's `.legion/<issue>/` handoffs ride alongside the one
  marker; the handoff-only PR head carries `skip-checks: true` and the daemon inherits the code
  head's verdict).

## Evidence

PR sjawhar/legion-smoke#668 (issue S395788430-5): one file `smoke/S395788430-5.md`, content
`S395788430-5\n`, 13 bytes, byte-identical in shape to the five existing `-5` siblings
(S396017608-5, S395289629-5, S394220170-5, S393243811-5, S392473105-5), each 13 bytes. Proven at
code head 86dd6ff1: `jj diff -r 86dd6ff1 --name-only` = exactly `smoke/S395788430-5.md`,
`wc -c` = 13, `cmp` vs `printf 'S395788430-5\n'` exit 0; negative control vs `S395788430-1\n`
differs at char 12, and the path is absent at base 8f7f3ab3. CI green at 86dd6ff1 (`ci` run
39252538102, `fail-on-demand` run 39252538121). Reviewer approved with zero findings and zero
review rounds; no per-round append occurred.

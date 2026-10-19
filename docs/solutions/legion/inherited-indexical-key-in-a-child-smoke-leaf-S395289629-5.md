---
title: "Which key a child smoke leaf writes when its spec is inherited from the parent"
category: legion
tags:
  - smoke
  - acceptance-criteria
  - proof
  - empty-spec
  - child-issue
date: 2026-10-18
status: active
module: legion-worker
related_issues:
  - "S395289629-5"
  - "sjawhar/legion-smoke#617"
---

# Which key a child smoke leaf writes when its spec is inherited from the parent

Extends docs/solutions/legion/acceptance-check-for-a-bare-key-smoke-leaf-S393243811-5.md.

Delta: when a child leaf's own spec document is empty and it inherits the parent profile
("add one new Markdown file under `smoke/` holding a single line that names this issue"), the
phrase "this issue" is **indexical** — it resolves to the parent key in the document where it was
written. Write the **child** leaf key (`S395289629-5`), never the parent (`S395289629-1`): the file
the smoke run proves is the leaf that did the work, confirmed by the nearest sibling leaf marker
(`smoke/S393243811-5.md` holds its own `-5` key, not its parent's `-1`).

**Prove the child-vs-parent choice with a negative control, not just the positive grep.** A
positive `head -1 … | grep -q '<child-key>'` passes on a file that also matches the parent key as a
substring, and passes even if someone wrote the parent key. Add a control that greps the first line
for the **parent** key and asserts exit 1:
`head -1 smoke/<child>.md | grep -q '<parent-key>' && echo WRONG || echo ok`. Exit 1 is the proof
the marker names the leaf and not the inherited parent — the one acceptance failure that a
bare existence/first-line check cannot catch on its own.

## Evidence

- Issue S395289629-5 (child of S395289629-1), PR sjawhar/legion-smoke#617. Spec document empty;
  parent spec S395289629-1 inherited. One file `smoke/S395289629-5.md`, line 1 `S395289629-5`.
- Tester negative control at code head `b0989ae8`: first-line check for the parent key
  `S395289629-1` exited 1 (no match), and `jj file show -r 7e6dbdc3 smoke/S395289629-5.md` errored
  `No such path` (file absent before the change).

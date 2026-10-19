---
title: "Negative control for the fail-on-demand gate must reference the root .fail-me"
category: legion-smoke
tags:
  - smoke-leaf
  - negative-control
  - fail-on-demand
  - proof
date: 2026-10-19
status: active
module: smoke
related_issues:
  - "S394220170-5"
  - "sjawhar/legion-smoke#644"
---

# Negative control for the fail-on-demand gate must reference the root `.fail-me`

A smoke-leaf proof whose negative control claims "the pipeline can fail" must exercise the input
the gate actually reads. The `fail-on-demand` workflow's only step is
`if [ -f .fail-me ]; then exit 1; else exit 0; fi` (`.github/workflows/fail-on-demand.yml`): it
reads a **root-level** `.fail-me` and nothing under `smoke/`. A negative control that checks a
neighbor like `smoke/<KEY>.fail` or `smoke/<KEY>.fail-me` returns 404 and proves only that no
stray file sits beside the leaf — it does not touch the gate's discriminating path, so it does not
support a "the gate can go red" claim.

- To prove no stray file was added: a 404 on a neighbor path under `smoke/` is correct, phrased as
  "only the exact key file exists."
- To prove the `fail-on-demand` gate is discriminating rather than unconditionally green: state
  that **no root-level `.fail-me` exists** on the branch (the gate fails iff one is present), not a
  `smoke/*.fail-me` 404.

The `ci` check is `sleep 20 && exit 0` (`.github/workflows/ci.yml`): a timing stub that is always
green and proves nothing about file content. Content correctness is proven only by reading the leaf
itself, never by CI being green.

## Evidence

On S394220170-5 (PR #644) both the implementer and tester negative controls used a `smoke/*.fail`
/ `smoke/*.fail-me` 404 while asserting the gate "retains the ability to fail." The assertion held
(no `.fail-me` was added) but the cited path was one the gate never reads; the fresh-eyes retro
pass caught the mismatch. Future leaves should cite the root `.fail-me` absence for that claim.

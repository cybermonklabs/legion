---
title: "Trivial-change pipeline hygiene: exported smoke API, negative controls, and .legion cleanup"
category: legion
tags:
  - legion-pipeline
  - handoff-hygiene
  - negative-control
  - ts-no-tiny-functions
  - e2e-surface
date: 2026-09-18
status: active
module: smoke
related_issues:
  - "SL200B-1"
  - "sjawhar/legion-smoke#82"
---

# Trivial-change pipeline hygiene

Running the full Legion plan -> implement -> test -> review -> retro pipeline on a
deliberately trivial change (SL200B-1: add `smoke/l200b-1.ts` exporting
`kindSmokeValue(): string` plus its `bun:test`) surfaced reusable discipline that also
applies to real changes. Keep these when the change is small enough that the process is the
hard part, not the code.

## A spec-mandated exported function is exempt from `ts-no-tiny-functions`

`export function kindSmokeValue(): string { return "kind smoke l200b 1"; }` trips the
`ts-no-tiny-functions` rule (whole body is one `return`). It is nonetheless correct: the
rule explicitly exempts an *exported name that is a stable domain concept* and a *public API
/ test seam*. When an issue mandates a callable contract (a smoke/harness entry point, a DI
boundary), that is the exemption — do not inline it to satisfy the linter, and do not
bikeshed it in review. State the exemption reason at the point you take it rather than
suppressing the warning silently.

## Prove the test with a negative control, especially when the change is trivial

A one-assertion test on a constant string is exactly where a vacuous pass hides. Both the
implementer and the tester ran an independent negative control (return `"WRONG"` in the
module; assert `"WRONG VALUE"` in a throwaway test) and observed `1 fail, exit 1` before the
green `1 pass, exit 0`. That mutation-style check is the only indisputable evidence the test
can fail — record the broken input and the failure observed, then revert before commit.

## The exported function is the E2E surface for a library-only module

With no CLI, endpoint, or TUI in the sandbox, the user-facing surface is the exported
function itself, exercised through the native runner (`bun test smoke/l200b-1.test.ts`). Do
not build a throwaway adapter or HTTP wrapper to manufacture an "E2E surface" — invoking the
real callable boundary through the real test runner satisfies the gate. When genuinely no
surface reaches the changed path, that is a report to the architect, not a wrapper.

## `.legion/` never lands on main; delete it in a dedicated commit and diff-verify

Handoff artifacts (`.legion/plan.json`, `.legion/implement.json`, ...) are committed on the
branch during the pipeline but must not merge. The clean pattern: after review approves the
code head, the implementer builds the `.legion/` deletion as one dedicated commit **on top of
the reviewed code head**, then proves it is deletion-only with
`jj diff --from <reviewed-sha> --to <cleaned-sha> --summary` (here `f74e1588` -> `55b88a83`
showed only `D .legion/implement.json`, `D .legion/plan.json`). Building the deletion on the
reviewed head — not on later test/review handoff commits — keeps the product code
byte-identical, so the reviewer's approval of the cleaned head stands. Recreating any
`.legion/` file afterward (including in retro) would void that approval; retro writes only
under `docs/solutions/`.

Watch the working-copy hazard in jj: the approved cleanup commit is `@` right after the
deletion push, so writing the retro doc into the working copy *amends that approved commit*
and rewrites its SHA. Base the retro doc on a fresh `jj new <approved-sha>` child instead, so
the approved head stays byte-identical and the doc rides above it.

## Do not trust green CI in this sandbox

`ci.yml` is a no-op (`sleep` + `exit 0`); its checks prove nothing. All correctness evidence
lives in the PR body's `E2E (implementer)` and `E2E (tester)` lines and their negative
controls. Read the proofs, not the check marks.

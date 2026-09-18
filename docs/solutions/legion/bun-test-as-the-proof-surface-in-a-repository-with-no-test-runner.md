---
title: "bun test as the proof surface in a repository with no test runner"
category: legion
tags:
  - bun
  - proof
  - negative-control
  - jj-auto-snapshot
  - bare-gate-ci
date: 2026-09-18
status: active
module: smoke
related_issues:
  - "SL200C-1"
  - "sjawhar/legion-smoke#83"
---

# bun test as the proof surface in a repository with no test runner

## Situation

SL200C-1 added `smoke/l200c-1.ts` (one exported function) and `smoke/l200c-1.test.ts` (one
`bun:test` assertion) to a repository with no `package.json`, `tsconfig`, `bunfig.toml`, lockfile,
test script, or `AGENTS.md`. The change has no deployed surface beyond its own test, so the only
production-like surface is Bun's built-in test runner executing the real file from the workspace
root.

## What to check before relying on this

1. Falsify the alternative first: glob for `package.json`, `tsconfig*.json`, `bunfig.toml`, and
   `**/*.ts` under the workspace. If any exist, the repository has an intended toolchain and the
   proof must run through it, not around it.
2. Probe zero-config execution in a temp directory before touching the workspace: a passing file
   must exit 0 and a wrong-string file must exit 1. Bun 1.3.14 ran `.ts` test files with no
   configuration at all.
3. Read the CI workflow files, not their names. In this repository `.github/workflows/ci.yml` job
   `check` is `sleep 20 && exit 0` and `fail-on-demand.yml` job `gate` fails only when a `.fail-me`
   file exists. Neither runs `bun test`, so a green CI proves nothing about the change: CI is a bare
   gate and the local run is the acceptance proof. Say so on the PR body's CI line, and note that
   there is no run id to link because no job executes the proof command.

## The negative control must run from outside the workspace

jj auto-snapshots every new file in the workspace into `@`, so a throwaway test written under
`$LEGION_WORKSPACE` lands in the next commit. Write it in `mktemp -d`, import the module by
absolute path, run `bun test` from that directory, and remove it:

```bash
N=$(mktemp -d) && printf 'import { expect, test } from "bun:test";\nimport { kindSmokeValue } from "%s/smoke/l200c-1";\ntest("negative control: wrong expected string", () => {\n  expect(kindSmokeValue()).toBe("kind smoke l200c 2");\n});\n' "$LEGION_WORKSPACE" > "$N/neg.test.ts" && (cd "$N" && bun test neg.test.ts); echo "exit=$?"; rm -rf "$N"
```

Observed: exit 1, `Expected: "kind smoke l200c 2"`, `Received: "kind smoke l200c 1"`, `1 fail`.
`jj status` stayed clean afterwards. The same rule applies to the handoff payload: build the JSON
for `legion handoff write` in `/tmp`, never in the workspace.

## What the proof line must carry

Surface (runtime and version, where it ran), exact command, observed output lines, the head SHA,
and the negative control with its observed refusal. On this issue the implementer, tester, and
reviewer each re-ran `cd -- "$LEGION_WORKSPACE" && bun test smoke/l200c-1.test.ts` at the same head
and recorded identical lines; that agreement is what made a repository with no test runner
reviewable.

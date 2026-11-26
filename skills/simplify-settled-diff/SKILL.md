---
name: simplify-settled-diff
description: "Use when implementation is done and before review, to simplify recently changed code for clarity, reuse, quality and efficiency without changing its behavior."
argument-hint: "[blank to simplify current branch changes, or describe what to simplify]"
---

Simplify recently changed code for clarity, reuse, quality and efficiency, and keep its behavior exactly. Fewer lines is not the goal.

## Step 1: Scope

The scope is the one the user names; else the current branch against its base (without a usable base, `git diff HEAD`); else the files edited earlier in the conversation. With no scope, ask the user what to simplify. Every file of the scope is in the pass, documentation, workflow and configuration files included. Edit no generated, vendored or lockfile content.

## Step 2: Three reviewers in parallel

Give three subagents the whole scope and, verbatim, one prompt file each from this skill's directory:

- `references/personas/code-reuse-reviewer.md`
- `references/personas/code-quality-reviewer.md`
- `references/personas/efficiency-reviewer.md`

In Oh My Pi, dispatch each as `task(agent="reviewer")` and `wait` for each result; elsewhere use the host's subagent tool, or review inline and say so.

## Step 3: Fix

After all three reviews, apply the worthwhile findings and record the rest as skipped.

- Edit only the scope and the import and export seams it needs.
- Each fix keeps outputs, errors, side effects and ordering; where you cannot show that, skip it.
- Never thin or remove a safety check: trust-boundary validation, data-loss protection, security checks, accessibility affordances.
- Remove an interface that only an earlier iteration of this unshipped scope used only after you check it has no caller outside the scope.

A defect you see is reported, never dropped: a problem this pass may not change goes in the summary with its file, its line and what is wrong.

## Step 4: Verify

When a fix was applied, run the typecheck, lint or build of the packages it touched and the tests that cover it, by name; never a whole suite. Fix or revert a change that breaks a check; never relax an assertion, weaken a type or skip a test.

## Step 5: Summarize

Report applied fixes by reuse, quality and efficiency, the skipped count, each defect reported and not fixed with its file and line, and the check outcomes.

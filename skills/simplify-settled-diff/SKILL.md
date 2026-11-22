---
name: simplify-settled-diff
description: "Use when implementation is done and before review, to simplify recently changed code for clarity, reuse, quality and efficiency without changing its behavior."
argument-hint: "[blank to simplify current branch changes, or describe what to simplify]"
---

Simplify recently changed code for clarity, reuse, quality and efficiency while preserving exact behavior. Prefer readable, explicit code to compact code: fewer lines is not the goal.

## Step 1: Scope

1. A scope the user names is the scope; do not widen it.
2. Otherwise, in git, the current branch against its base; without a usable base, `git diff HEAD`.
3. Outside git or without a diff, the files the user named or that were edited earlier in the conversation.

If none of these gives a non-empty scope, ask the user what to simplify, with the host's question tool where it has one, else as numbered options in chat. Where the host lists a question tool that is not loaded, load it with the host's tool-discovery primitive before you fall back to chat. Never skip the question.

Every file of the scope is in the pass: documentation, workflow and configuration files as much as code, and a scope that holds no code at all still runs. Read generated, vendored and lockfile content for what it shows, and edit none of it. Size is no gate: callers own any size or cost threshold.

## Step 2: Three reviewers in parallel

Dispatch three subagents, each given the full text of its prompt file, passed verbatim, and the whole resolved scope (the full diff or file set):

- `skill://simplify-settled-diff/references/code-reuse-reviewer.md`
- `skill://simplify-settled-diff/references/code-quality-reviewer.md`
- `skill://simplify-settled-diff/references/efficiency-reviewer.md`

In Oh My Pi, dispatch each as the bundled reviewer, `task(agent="reviewer")`, and `wait` for each result. Elsewhere use the host's subagent tool (`Agent`/`Task` in Claude Code, `spawn_agent` in Codex), or run the reviews inline. Omit the `mode` parameter, so the user's permission settings apply. A concurrency-limit error is backpressure: retry when a slot frees. A dispatch that fails for any other lasting reason runs that reviewer inline with the same prompt, and you say so in one line.

## Step 3: Fix

Start only after all three reviews are complete. Apply the worthwhile findings; record false positives and low-value findings as skipped, without asking the user.

- Read beyond the scope to judge a finding, but edit only the scope and its necessary import and export seams. For a scope the user named, those seams must be inside it too.
- Each fix keeps outputs, errors, side effects and ordering. If you cannot show that, skip it.
- Never thin or remove a safety check: trust-boundary validation, data-loss protection, security checks, accessibility affordances.
- An interface or data shape that existed only in an earlier iteration of this unshipped scope is not protected once you verify it has no deployed, persisted, public, external, dependent-branch or in-repo caller outside the scope. Remove it only when every caller update fits the scope.
- A plan the caller passed as context is not scope: keep each decision it marks `session-settled:`, including a duplication or a separation made on purpose.

**A defect you see is reported, never dropped.** A problem this pass may not change (a wrong procedure in a document, a workflow step that cannot work, a check that cannot fail) goes in the summary with its file, its line and what is wrong, never as out of scope. The caller fixes it outside the pass.

## Step 4: Verify

Only when a fix was applied: run the typecheck, lint or build of the packages the fixes touched, and the tests that cover the changed code, by name. Run no whole test suite. With no fix applied, record `no fix applied; head unchanged, no verification run` and go to Step 5.

Report a failure with the check name and its output. Fix a failure the simplification caused or revert that change; never relax an assertion, weaken a type or skip a test. If no test, lint or typecheck is configured, say so in the summary.

## Step 5: Summarize

Say what was already sound and what improved. Report applied counts by reuse, quality and efficiency; the skipped count; each defect reported but not fixed, with its file and line; and the check outcomes. Name the agent or model that produced each reviewer's findings. If nothing changed, say so. Lines removed is not the measure of success.

---
name: thermonuclear-code-quality
description: Use when asked for a thermonuclear code-quality review or a deep maintainability audit of a branch or PR diff, or to review abstraction quality, oversized files or ad-hoc branching growth.
disable-model-invocation: true
---

# Thermonuclear Code Quality

If this skill disagrees with the code, a command's output or its `--help`, follow those and say which line of this skill is wrong.

Make your own scratch with `mktemp -d` and delete only the paths you made, each by its exact path. Never delete by a glob in a shared directory: another session's open directories may sit beside yours, as they do in a shared `$TMPDIR`.

This is a maintainability review of one diff: the current branch's changes, or the range you are given. Every remedy keeps behavior the same and removes code or concepts, or moves code where a reader finds it without reading the rest of the file. A behavior change is work for `skill://thermonuclear-deep-review`. The table's last two rows change ordering or failure behavior: say so in the finding.

## Report

The first line is the verdict, with the short sha of the head you reviewed (`git rev-parse --short=8 HEAD`, or the sha your brief names when you have no shell). Put nothing before it: a PR gate reads only the first line.

- `Verdict: APPROVE at <sha>` when no finding stands.
- `Verdict: CHANGES at <sha>` when one does.

Then one entry per finding: `path:line`, the smell, the cost it adds, and the remedy. Report few findings, each one you are sure of. Order them costliest first: step 2's structural regressions and missed code-judo moves, then rows 2-3, 7-8, 1, 9-10, then the rest. Write each as a plain request: `this adds another special-case branch to an already busy flow. Move it into its own function.`

## Steps

1. Read the diff and each changed file around its hunks. Flag lines the pull request (the current branch against its base) adds or changes, and existing code it makes harder to follow, such as a function it lengthens or adds a branch or parameter to. A finding on code the pull request leaves alone blocks it for work it did not cause. A range limits what you must read, never what you flag: a smell you see in code the pull request changed before the range starts is a finding.
2. Look for the code-judo move first: a restructure onto the existing architecture that deletes whole branches, helpers, modes, conditionals or layers. It often removes the line-level findings with it.
3. For a refactor, count the concepts (types, functions, flags, modes) a reader must know before and after. A move that keeps the count is not a simplification: say so. When the problem is structural, do not suggest a rename.
4. Check every hunk against the table below. For file size, compare `git show <base>:<path> | wc -l` with `wc -l <path>`; `<base>` is the range start your brief names, else `git merge-base HEAD origin/<default branch>`.
5. Before you report a finding, quote its line at the reviewed head. For a boundary, type or duplicate-helper claim, also read the callers (`grep` the symbol) and the existing helper you name.
6. Report only a cost you can name (the Cost column). A preference or a micro-optimization with no such cost is not a finding. When a structural finding exists, leave out naming and style nits.
7. Ask for a new abstraction (a type, module or policy object) only when the code shows two real callers or two ways it will change. With one, the abstraction is the thin wrapper row 6 flags.

## Smells and remedies

| Smell | Cost | Remedy |
|---|---|---|
| The PR pushes a file from under 1000 lines to over 1000 | more to scan for every change | Decompose it first into helpers, modules or local abstractions. Accept the growth only when the PR names the caller or constraint that keeps the code in that file. |
| Ad-hoc conditionals, special cases, one-off booleans, nullable modes or flags in unrelated or busy flows. A branch for one input value in the middle of a busy function. A branch whose name or comment says it is temporary. | each later change reads every branch | Reframe the state model so the conditionals disappear, not only move to one place. Make the special case part of a simpler default flow. Else move it into its own function, or behind an abstraction step 7 allows. |
| Repeated conditionals or condition chains | each new case edits every chain | A typed model or an explicit dispatcher. |
| Copy-pasted logic or duplicate branches | a fix lands in one copy | Extract a helper or pure function. Collapse into one flow. |
| Hacky, brittle or "magic" behavior, such as reading a value by its string form or position. Generic mechanisms that hide simple data-shape assumptions. | the assumption breaks with no error | Direct code that states the assumption: a typed field, a named constant, an explicit check. |
| Thin wrappers, identity abstractions, pass-through helpers | one more layer to cross | Delete the layer. |
| Unnecessary optionality, `unknown`, `any`, casts, loose ad-hoc objects. A silent fallback over an unclear invariant. | the type hides what the value is | Explicit typed models or shared contracts, so the control flow gets simpler. |
| Feature logic in shared paths. Implementation details leaking through APIs. Logic in the wrong file, layer or package. | every caller of the shared path pays | Move it to the package whose existing code already reads or writes that data (`grep` its type). |
| A bespoke helper where the repository already has one that does the same | two helpers drift apart | Reuse the existing helper; name it as `path:line`. |
| A module that became more coupled (a new import of another package), stateful (new module-level state) or hard to scan. Orchestration mixed with business logic. | a change to one concern touches the others | Split into focused modules. Separate orchestration from business logic. |
| Independent work run in sequence | an order nothing needs | Run it in parallel, only when the parallel code is also simpler. |
| Related updates that can leave state half-applied | a failure leaves state no code expects | A more atomic flow. |

## Approval bar

- A structural regression, a hacky or magic abstraction, a boundary leak, or a missed obvious decomposition blocks whatever the PR says.
- Every other finding blocks, and the verdict is CHANGES, unless the PR gives evidence that the simpler form fails: a caller it breaks, a measured cost, or a constraint outside the diff that the PR names with `path:line` or a link. "It works" and "tests pass" are no such evidence: this review judges structure, not behavior.
- State a blocking finding as blocking, directly and without rudeness. A "maybe" or "consider" finding reads as optional.

## Findings

- Rate each finding; the lead disposes of it (act on, consider, noted or dismissed). Give each the evidence, the cost and what it depends on, and put on the must-fix list only what must be fixed before this change ships.
- Do not review a diff you wrote: hand it to a session that did not write it.

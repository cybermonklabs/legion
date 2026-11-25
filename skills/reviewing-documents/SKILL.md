---
name: reviewing-documents
description: "Use when asked to review, check or tighten a requirements doc, plan, spec or design before the work it guides starts, also as a subagent told to return findings or given mode:non-interactive. Not for a code diff, nor for predicting a person's approval verdict."
---

# Reviewing documents

Help the author finish a sound document they can use to carry out the agreed work: find the problems that would change that work's outcome or materially hinder it, and resolve them within the authority already given.

**Done when:** every selected lens has returned or is named as failed in Coverage, each retained finding has a verified consequence for the agreed work, and each correction routed to Apply is made and checked; the result reached the author through the interactive questions, or the caller as the non-interactive result.

## Step 1: Read the mode rules

**Read `references/modes.md` before anything else**: before the document, its sources, a search or a todo list. It says how to tell interactive from non-interactive mode, the argument contract, and which tool asks the author a question.

## Step 2: Get and classify the document

**Read `references/document-intake.md` now.** It says how to get the document in each mode, what to say when none is found, how to classify it, and when an earlier review can be resumed.

- **Verify before any dispatch** that every path a lens will read is readable on disk. A path outside the checkout is fine. Lenses read the filesystem, so they cannot reach a file that exists only on an unchecked-out branch or in a message.
- **If a path is not readable, dispatch no lens.** Give the failure message that the intake reference states for your mode.
- **Classify by content, not by path or a readiness label:** `requirements`, `spec`, `plan` or `other`. The lenses adapt to the type, so a wrong type gives noisy or thin findings.
- **Resume instead of dispatching** only when the intake reference's reuse condition holds; then go to step 5 with the retained evidence.

## Step 3: Choose and announce the lenses

**Read `references/persona-selection.md` now:** it defines the team, each conditional lens's activation signals, and the announcement you make before any dispatch.

- **Hold the security and adversarial lenses to their stated bounds** (the sensitive-data bound, the challenge-surface bar): both over-activate on plausible evidence.

## Step 4: Dispatch each lens

**Read `references/dispatch.md` now** for the prompt, the slots you fill, a full queue and a failed lens.

1. **Dispatch each lens as its own generic subagent** (`task(agent="task")` in Oh My Pi, `Agent` in Claude Code, `spawn_agent` in Codex), as the reference's "The subagent" says.
2. **Wait for each result** (`wait` in Oh My Pi) before step 5.
3. **The brief is three files.** Each lens reads `references/subagent-template.md`, its own `references/personas/<lens>.md` and `references/findings-schema.json`. Give it their locations and fill every other slot.
4. **Round 2 or later** on the same document in this session: **read `references/decision-primer.md` now** and pass the prior decisions.

## Step 5: Judge the findings

The lenses supply evidence; the judgment is yours. **Read `references/synthesis-and-presentation.md` once every lens has returned or failed**, and run its steps in order.

- **Check each claim yourself** against the whole document, the project's code and records, and the decisions already settled. Drop a claim the facts do not support.
- **Keep a finding only for its consequence:** following the document would produce wrong work, block it, or cost worthwhile avoidable work. A detail an implementer can derive is no finding.
- **Judge by whether the document guides correct work**, not by how much detail it holds. An adequate document needs no change.
- **A serious consequence warrants attention** even when the defect is small.

## Step 6: Apply, present and ask

**Read `references/rendering-floor.md` before you render any finding**, and `references/review-output-template.md` for the result's form in your mode.

1. **Apply the corrections routed to Apply,** and check each edit, as the synthesis reference's "Apply" says.
2. **Non-interactive:** return the structured result, ending `Review complete`, and stop. Ask no question.
3. **Interactive:** present the review in the same turn as the first question, and **read `references/walkthrough.md` now** for the grouped confirmation, the routing question and the per-finding walk-through.
4. **Before any bulk action, read `references/bulk-preview.md`.**
5. **Before any Defer, read `references/open-questions-defer.md`.**
6. **End with `Review complete`.** It ends this skill, not your turn: when another skill called this one, go on with the caller's next step.

- **Load `references/walkthrough.md`, `references/bulk-preview.md` and `references/open-questions-defer.md` only in interactive mode,** and only once the review evidence is complete.
- **Edit in the document's own format:** never put Markdown inside HTML.
- **Start no planning or implementation that the request did not authorize.** Name a useful next step instead.
- **Change only what a finding needs:** no rewrite of the whole document, no new section or requirement nobody discussed, no separate review file.
- **Recommend completion after 2 rounds** on one document. Run another round only when asked, with the prior decisions kept.

---
name: dispatch-writing-specs
description: "Use when you write or change a spec, ask, comment or message a person reads, in a session with Dispatch. It replaces superpowers' brainstorming there."
---

# Writing specs

## The conversation

Reach shared understanding at the least cost of the person's time. A spec is a conversation, not a document you write and then ask about. A Legion phase worker writes none (`skill://legion-worker`).

1. Read the issue, the code and what the person wrote.
2. Send what you found and the questions it leaves open, a few at a time, before the spec states a design.
3. Write each part only after the answers it rests on.
4. A wrong premise or a changed request changes the request first.

## The outline

Find the issue's project with `dispatch projects` or `search`, never the environment. The spec's `##` parts, in order:

Part | Content
--- | ---
`Summary` | The change in the reader's terms, at most 100 words
`Problem` | What goes wrong, for whom, with counts
`Goals and non-goals` | What is in and out of scope
`Requirements` | a `:::requirement` block each, `**R1.**` on, its source (`From:`)
`Verification` | the issue's `verification.md`, a `:::check` block each: `serves`, `surface`, `run` (where it names tests, specific ones), `when`, `negative_control`. For a whole test suite, name its CI job in `ci_check`: a check carrying `ci_check` is CI's and takes no `run`
`Design` | How it meets them; a value and its reason for each limit, size and time
`Decisions` | The choices the spec rests on (Decisions below)

`serves` and `needs` name a block of this issue or an ancestor. Another agent can build and check the work from these alone. A small change gets a few sentences per part, no matrix; split a large design into children.

## Rules for the text

For each spec, ask, comment and message:

- Write in `skill://asd-ste100`'s strict mode.
- Write about the current design, never the person or how people reached it, and never quote or upload their words.
- Link each source where the person wrote it. With no link, state the design.
- Treat a person's stated preference or past answer as evidence of their taste, never as a decision that settles the question. A commit attributed to a person is not their decision.
- In a spec, cite code by file and symbol, never a line number.
- Keep only text a reader needs to understand, decide, build or check, and say what each issue you name does for them (`skill://plain-words`).
- In an ask, ask block, comment or message, name a requirement or check by what it is, never its number.

## Questions

- Name the concrete case and its numbers, then the choice in those terms, never "Which option?".
- Choices inside the request, ones their messages favour, routine cleanup, sequencing, naming, a pick between equivalent options and a fix you can make are yours.
- Ask with an [ask block](skill://dispatch/SKILL.md#asks-and-ask-blocks): 2 to 4 options, short and self-contained: a person answers it with no other reading.
- Measure each claim the ask rests on, its premise and each option's precondition. A claim you cannot measure stays out of the ask until it is measured. Call an option unavailable only where a precondition demonstrably fails.
- Before you call a question one only an attended or privileged act answers, try its source, then the other records holding it. Keep a privileged read for what only the live system knows.

## Each version

1. After a person's answer, comment or edit, reread the problem and rewrite what the answer touches, deleting what it contradicts. Answer each comment in its thread. Never narrate an edit.
2. Where a spec is large, or changed in a way you cannot check alone, have a fresh subagent check the whole spec for gaps and wrong facts.
3. Name each version with `dispatch doc-edit --summary`.

## Decisions

A choice earns an entry in the spec's `## Decisions` part only if a disagreeing reader would act differently; keep others to one spec line each. Write it in the spec, never in a separate document: the choice first, then the reason and the options weighed, as a `:::decision` block once the server's block types include `decision`. A person's answered ask is their decision: it stays in the spec where it was asked.

## Old specs

Rewrite a spec outside the outline whole in one version, its ask blocks moved with `doc-edit`'s `move`.

## Approval

- Request approval of the spec and `verification.md` (`--include-verification`): each ask block answered or waived, each claim checked, this version unapproved. An unrun check never delays it.
- After approval, version only a change to scope, a check, cost, risk or a person's decision, all at once; put other fixes in the plan or a comment. An exact undo can restore approval.

## The plan

The plan is the issue's `plan.md`, never a repository file. Each task is a `:::plan_task` block with `serves` and `needs`.

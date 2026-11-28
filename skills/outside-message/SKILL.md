---
name: outside-message
description: "Use when text you write will reach a person outside the company (a comment, issue or pull request on a repository the company does not own, an email, a release note, a bug report, a disclosure), and before you ask the operator to approve such text."
---

# A message to someone outside the company

The operator approves every message that leaves the company. Do the four steps below, then ask.

Not covered: text written in a person's own name, which that person signs off; a message to the operator, to another agent, or inside the company's own organization; a release the pipeline publishes from approved template text, unless its text changes.

## Exclusions

An exclusion exists only where the operator stated it. Never infer one from the shape of a message.

- **A project the operator excluded by name** goes out without approval. Only the operator's own answer names one, and it reaches you in your stream's brief.
- **A pull request to a project that has enrolled**, with its description and its comments to that project's maintainers: do the four steps, and skip the ask. Prepare and sign the changes locally, and send them when the work they belong to ends. Enrollment is the enrolling project's own record, which your stream's brief locates; never judge it from a message's tone or from a maintainer having replied before.

Everything else waits for the operator: a first contact with a project that has not enrolled, a bug report, a disclosure, a reply to a maintainer who wrote first.

## The four steps

1. **Research the audience.** Read the repository's contributing guide, its issue and pull-request templates, its code of conduct, its last ten issues or pull requests, and what was already discussed about your subject.
2. **Write in their conventions:** their template, their terms, their level of detail. Run `skill://asd-ste100` and `skill://plain-words` over the text, and your session's pass over AI writing tells where it has one.
3. **Match their form.** A pull request uses their pull-request template, an issue their issue template, and a thread reply matches the thread.
4. **A fresh reviewer cuts.** Give a subagent that has not seen the drafting the message and the research, and tell it to remove everything not needed to understand the core message. Give it a different model from yours where your session can. It returns what it removed, or "the reviewer removed nothing".

The operator approves the text the reviewer cut, byte for byte. Any edit after the cut, even one line of fact, sends the text back through step 1 and a new review.

Where the stream that owns the relationship keeps its own rules for writing to maintainers (in its project's `AGENTS.md`, its skills or its brief), those rules decide how the message reads. Read them before you draft, and change the form there, not here.

## The ask

The ask carries, in this order:

- who the reader is, and one line on what they already know about this;
- **the message in full, as it would be sent**, never a link to it or a summary;
- **what the reviewer removed in step 4**, in three lines or fewer, or "the reviewer removed nothing";
- what changes if the answer is no.

Link the research, not the message. An ask with no removal line has not had step 4.

## Upstream reports, patches and public posts

These go into the review batch your stream's brief names, never into a per-message ask. Write the entry into the batch (what it is and how serious; who receives it and what they do; the exact text or patch; its evidence; a non-author reviewer's APPROVE under your stream's reviewer-model rule; what the operator decides; its owner), add it under the batch's Waiting list, and tell the batch owner. Where the brief names no batch, ask the operator for one or hold the entry.

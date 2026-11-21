---
name: outside-message
description: "Use when text you write will reach a person outside the company (a comment, issue or pull request on a repository the company does not own, an email, a chat message to another company, a release note, an enrollment reply, a bug report, a disclosure), or before you ask the operator to approve it."
---

# A message to someone outside the company

The operator approves every message that leaves the company. Do the four steps below, then ask.

Not covered:

- Text written in a person's own name, which that person signs off.
- A message to another agent or to the operator: write it plainly and send it, with no approval step.
- A message inside the company's own organization.
- A release the pipeline publishes from approved template text: the template was approved once, so the release needs no new ask unless its text changes.

## Exclusions

An exclusion exists only where the operator stated it. Never infer one from the shape of a message. The operator's answers reach you in your brief: the instructions you work under, which are your system prompt (a Legion role's deployment instructions are part of it) and the spec of the issue you work on.

- **A project the operator excluded by name** goes out without approval: do the four steps, and skip the ask. Only the operator's own answer names such a project, and it reaches you in your brief. With no such answer for the project in front of you, there is no exclusion.
- **A pull request to a project that has enrolled**, with its description and its comments to that project's maintainers: do the four steps, and skip the ask. Prepare and sign the changes locally, and send them when the issue or task they belong to is done. Enrollment is the enrolling project's own record, and your brief says where that record is; never judge it from a message's tone or from a maintainer having replied before.

Everything else waits for the operator: a first contact with a project that has not enrolled, a bug report, a disclosure, a reply to a maintainer who wrote first outside such a pull request.

Hold an extremely high bar. Optimise every upstream report, patch or public post for a human reader and for being received well, so each one builds the company's reputation rather than a reputation for low-quality or careless submissions.

## The four steps

1. **Research the audience.** Read the repository's contributing guide, its issue and pull-request templates, its code of conduct, its last ten issues and its last ten pull requests. Read how the maintainers write to each other. Search its issues and pull requests, open and closed, for what was already discussed about your subject: a question they answered, asked again, costs you the reader.
2. **Write in their conventions:** their template, their terms, their level of detail. Run `skill://asd-ste100` and `skill://plain-words` over the text, then each skill your session lists for removing AI writing tells.
3. **Match their form.** A pull request uses their pull-request template, an issue their issue template, and a thread reply matches the thread.
4. **A fresh reviewer cuts.** Give a subagent that has not seen the drafting the message and the research, and tell it to remove everything not needed to understand the core message. A reviewer that only approves has not done step 4. Give it a different model from yours where your session can. It returns what it removed, or "the reviewer removed nothing".

The operator approves the text the reviewer cut, byte for byte. Any edit after the cut, even one line of fact, sends the text back to step 1: do the four steps again, a new fresh review included, because a sentence added after the cut reaches the operator unreviewed.

Where the repository or team that owns the relationship with that project keeps its own rules for writing to its maintainers (in its `AGENTS.md`, its skills or your brief), those rules decide how the message reads. Read them before you draft. A change to those rules goes where they are kept, never into this skill.

## The ask

The ask carries, in this order:

- who the reader is, and one line on what they already know about this;
- **the message in full, as it would be sent**, never a link to it or a summary;
- **what the reviewer removed in step 4**, in three lines or fewer, or "the reviewer removed nothing";
- what changes if the answer is no.

Link the research, not the message. An ask with no removal line has not had step 4.

## Upstream reports, patches and public posts

An upstream bug report, patch or public post that waits for the operator goes into the review batch your brief names, never into a per-message ask. Every other message that waits for the operator gets the ask above. Write the entry into the batch (what it is and how serious; who receives it and what they do; the exact text or patch; its evidence; the APPROVE of a reviewer who did not write it, on the model your brief names for that review, else on a different model from yours where your session can; what the operator decides; its owner), add it under the batch's Waiting list, and tell the batch owner. Where your brief names no batch, ask the operator for one, or hold the entry unsent until there is a batch: a missing batch is no reason to send a per-message ask.

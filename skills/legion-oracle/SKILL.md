---
name: legion-oracle
description: Use when you are about to ask a human a question about a pattern, convention, solved problem or technical approach that the repository or its docs/solutions/ might already answer.
---

# Legion oracle

Research the question before you put it to a person.

1. Grep `docs/solutions/`: its front-matter `tags` first, then the bodies. A file whose `supersedes:` key lists your hit replaces it.
2. Ask `task(agent="scout")` how the code handles the topic, and `wait` for its answer.
3. For a library, read its official docs; for general practice, what the standard's maintainer publishes, with its URL.

Ask a person only what that leaves open, and say what you searched and what each search found.

A `docs/solutions/` page that sends the controller a defect, a flake, a notice, a pull request to queue or a READY is older than the skills. `skill://legion-worker/references/findings.md` replaces its routing of a finding, and `skill://legion-architect/references/operations.md` its merge-queue steps. A finder fixes a defect or writes it to its owner. The controller queues no pull request: the architect of its tree enqueues one that no merger merges.

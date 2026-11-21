---
name: plain-words
description: "Use when you are about to send or publish text a person reads (an ask, spec, comment, message, pull-request body, commit message, report or status update), answer a person's question, or hand a draft to a reviewer."
---

# Write so a reader understands it on the first read

Prose that a reader understands on the first read, with none of the patterns that the tests below and `references/patterns.md` name, and with every fact the source stated still there. Both goals hold at once: text that has none of those patterns but is still dense has failed, and text that is plain but changes how certain a claim is has failed.

**Done:** the mode's output is returned, every fact, number, name, quote, and citation in the input survives, and nothing was added that the source or the caller did not supply.

**Boundaries:** never touch code blocks, quoted text, frontmatter, link targets, identifiers, or a token the caller's own contract requires, unless the user names that content as the thing to fix. Never say whether text was written by a model. Write a named file in place only when the request asks for that; otherwise return the text.

## Mode

Take a `mode:` token when one is given. Otherwise: no draft means **author**; an imperative on a draft means **edit**; a question about a draft means **detect**.

- **author.** Hold the tests below while the caller writes; return nothing. When handed content and asked to write, draft it under the same tests.
- **edit.** Rewrite only the sentences a test fails on, and return the text. A sentence that passes stays as written, so a second pass on the returned text changes nothing. Say what changed in one line only when the caller asks for it, and keep that line outside the rewritten text and out of any artifact.
- **detect.** Name each pattern found, quote the line, give the fix in a few words. Do not rewrite.

For edit and detect, and for an author passage the tests alone do not settle, read `skill://plain-words/references/patterns.md`. On text that is not English, apply the tests only. Note that the pattern catalog did not apply inside detect findings or inside a change line the caller asked for, and nowhere else.

## Register

Pick by who reads the result. The caller's own interaction contract wins over any line here.

- **Agent talking to the user.** The reader is a teammate who knows the domain and did not watch the work. Write each sentence as you would say it to them.
- **Repo or team artifact.** Neutral. Match the surrounding document's idiom. No first person, no opinion the artifact does not need. This is the default when no reader is named.
- **The user's own writing.** Preserve voice; make the minimum effective edit. Understandability edits stop at sentence splits and actor restoration that keep the user's word choice.

## The tests

Apply these to every sentence, in author mode as constraints and in edit or detect mode as checks.

1. **Mechanism.** Does the sentence say what the thing does, or how it feels or what it is like? Replace the feeling or figure with the fact it displaced, or cut the sentence.
2. **Portability.** Could the sentence move to another project unchanged? Then it carries no fact about this one.
3. **Actor.** Who does the verb? Name them when the source says who. Keep the passive when the actor is unknown or naming it adds nothing.
4. **One idea.** Would the reader have to reread or decode the sentence? Split it or spell it out. A sentence the reader gets on the first pass stays, however long.
5. **Density.** One device proves nothing. Three or more distinct patterns in a passage, or one repeated across passages, is a finding.
6. **Decision first.** Does the first sentence carry the outcome the reader needs?
7. **Reader.** Can someone without the document or the code open act on this? Gloss the identifier or name the consequence.

Keep exact identifiers, paths, commands, and thresholds. For wording choices, read `skill://plain-words/references/terminology.md`.

## The phrases to stop

| Never | What to write |
|---|---|
| land the change, land the PR | merge it, release it, deploy it, apply it — whichever one it is |
| unblock the work | X can start once Y merges |
| it pays for itself, that pays | it costs $16,306 and returns $60,379 a month |
| carry it to X, carry the finding | tell X; send it to X; put it on the issue |
| this shows the gap, the number shows | the gap is 12 of 30; the number is 1.17 |

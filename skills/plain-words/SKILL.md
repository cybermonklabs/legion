---
name: plain-words
description: "Use when you are about to send or publish text a person reads (an ask, spec, comment, message, pull-request body, commit message, report or status update), answer a person's question, or hand a draft to a reviewer."
---

# Say the literal thing

Delete the empty verb phrases agents keep using, and write what literally happens instead. Run this over every text before you send it.

## Answering a question

1. Answer the exact question, only it, in the order it was asked: anything else reads as evasion.
2. Start from what happens, not from a setting, a flag or a name.
3. Define each term where you first use it, in literal words. No analogy: the reader cannot tell which part of one is true.

## The test

Take the verb phrase out and ask: what literally happens? Write that. If you cannot say, you did not know.

| Never | What to write |
|---|---|
| land the change, land the PR | merge it, release it, deploy it, apply it — whichever one it is |
| unblock the work | X can start once Y merges |
| name the problem, name the owner, name a date | say the problem; who owns it is X; Y is left, and Z decides when it ends |
| it pays for itself, that pays | it costs $16,306 and returns $60,379 a month |
| carry it to X, carry the finding | tell X; send it to X; put it on the issue |
| this shows the gap, the number shows | the gap is 12 of 30; the number is 1.17 |

If you cannot say whether a thing merged, released or deployed, say that: "it is approved and not merged". On the "name the X" row, delete "name"; if the sentence then says nothing, decide the action and write it.

Plain verbs that name a real action stay (merge, delete, send, read, measure), and so does a term of art that is the literal name of the thing (a merge queue merges).

## Cut what marks text as an agent's

1. A sentence that says what you are about to say, or what you just said.
2. Praise of the work or the reader: "great question", "a robust solution".
3. Symmetry such as "not only X but also Y": write the half that is true.
4. A hedge that costs a second read ("it may be worth considering whether"): recommend it or leave it out. Keep every hedge that carries a real doubt.
5. A closing offer to do more. Where something remains, name it as the next step with its owner.
6. A word that rates the thing ("comprehensive", "seamless"): give the measurement instead.
7. A paragraph whose first and last sentences say the same thing: delete one.

No ALL-CAPS headings or phrases. Bold only a number, a name or a choice the reader must find again. A passage that needs a shouted handle needs cutting instead, in a brief to another agent too.

## Before you send

Give a fresh session only what the reader has. It removes what the reader does not need and lists every term it cannot define; fix each listed term. Run this on the text going to the reader, never on your instructions to that session or on its reply.

## Other passes

- For sentence structure (one instruction per sentence, active voice, simple tenses, short noun stacks), use `skill://asd-ste100`.
- On an outside message, also follow `skill://outside-message`.
- A form skill your stream's brief names for a kind of text (a comment to another project's maintainers, a document a buyer reads) applies as well.
- Where your session installs a fuller pass of its own over AI writing tells, run it after this skill's "Cut what marks text as an agent's".

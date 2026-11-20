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
| name the problem, name the owner, name a date | say the problem; who owns it is X; Y is left, and at the measured rate it ends on <date> |
| it pays for itself, that pays | it costs $16,306 and returns $60,379 a month |
| carry it to X, carry the finding | tell X; send it to X; put it on the issue |
| this shows the gap, the number shows | the gap is 12 of 30; the number is 1.17 |

If you cannot say whether a thing merged, released or deployed, say that: "it is approved and not merged". On the "name the X" row, delete "name"; if the sentence then says nothing, decide the action and write it.

Plain verbs that name a real action stay (merge, delete, send, read, measure), and so does a term of art that is the literal name of the thing (a merge queue merges). The rule is against phrases that sound like an action and name none.

## The structural pass

Run it over a draft before "Cut what marks text as an agent's": a tell often disappears with the sentence that carried it.

1. One instruction per sentence. A sentence that holds two actions hides the second from a reader who stops at the first.
2. Active voice, with the actor named. "The job is retried" leaves the reader to guess who retries it; "the daemon retries the job" does not.
3. Simple tenses. A compound tense carries a time relation the reader has to reconstruct, so keep one only where that relation is the fact.
4. Write the thing, not its noun form: "the build failed", not "a build failure occurred". A noun made from a verb drops the actor and the time.
5. Cut a noun stack to three words. Four or more leave the reader guessing which word modifies which.
6. One idea per paragraph, and a list wherever you write three or more of anything. A reader counts a list and cannot count a sentence.
7. Keep every qualifier, exception, number and hedge. Where keeping one makes the sentence long, keep it long and say why: dropping "may" turns a possibility into a claim.
8. Use one word for one thing, through the whole text. A synonym reads as a second thing.

## Cut what marks text as an agent's

1. A sentence that says what you are about to say, or what you just said.
2. Praise of the work or the reader: "great question", "a robust solution".
3. Symmetry such as "not only X but also Y": write the half that is true.
4. A hedge on your own advice that costs a second read ("it may be worth considering whether"): recommend it or leave it out. A hedge on a fact you have not checked stays, as the structural pass's item 7 says.
5. A closing offer to do more. Where something remains, name it as the next step with its owner.
6. Cut an em dash and a semicolon into two sentences, unless the aside is the fact.
7. A word that rates the thing ("comprehensive", "seamless"): give the measurement instead.
8. A paragraph whose first and last sentences say the same thing: delete one.

No ALL-CAPS headings or phrases. Bold only a number, a name or a choice the reader must find again. A passage that needs a shouted handle needs cutting instead, in a brief to another agent too.

## Before you send

Give a fresh session the text and only what the reader will have beside it: the thread it answers and the documents it links, not your notes or the work behind the text. It removes what the reader does not need and lists every term it cannot define; fix each listed term. A reviewer that reads only to approve has not done this step, so the two lists are what show it ran. Run this on the text going to the reader, never on your instructions to that session or on its reply.

## Other passes

- `skill://asd-ste100` is the full controlled-language standard behind the structural pass: run it where a reader must not misread one sentence.
- On an outside message, run them all (`skill://asd-ste100`, this skill and your session's fuller tells pass where it has one), and follow `skill://outside-message`. Run this skill on everything.
- A skill that sets the form of one kind of text (a comment to another project's maintainers, a document a buyer reads) applies as well, where your instructions name one: your system prompt or the spec of the issue you work on.
- Where your session installs a fuller pass of its own over AI writing tells, run it after this skill's "Cut what marks text as an agent's".

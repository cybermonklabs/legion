# Where this fleet's coordination is recorded, over a fixed window

Requirement r4.
Every count below was taken by one script, `count.py`, over one window, and can be taken again.
The script reads only the window file and the token file, and prints one JSON object.
Its recorded output is `evidence/count.json`.

Every command below is run from the repository root, with `docs/research/internet-of-cognition/env.sh` sourced: it exports `IOC_EVID`, the committed `evidence/` directory this file's recorded outputs stand in.

## The window

The window is the fourteen days before 2026-11-12T00:00:00Z, that is from 2026-10-29T00:00:00Z to 2026-11-12T00:00:00Z, and the project is INFRA [c-window-1].

Inside it the global event feed runs from event id 103048 to event id 402993 [c-window-1].

The feed is one list for the whole deployment, ascending by event id, and each row carries its project and the instant it was written [c-window-2], so a window and a project are a filter over it and nothing else.

A row of the feed is one recorded act.
Thirty-nine event types were written for INFRA inside the window [c-types-1].
Five of them carry a body in the feed, read over the first thousand events of the window: `comment.anchor_refreshed`, `comment.created`, `comment.resolved`, `message.answered` and `message.created` [c-types-2].
This count reads every written act requirement r4 names: the 30186 `comment.created` and `message.created` rows, the 736 replies to a message (`message.answered`), the asks as opened (1930 `ask.opened`), answered (1198 `ask.answered`), resolved (744 `ask.resolved`) and edited (1128 `ask.edited`), the 29709 versions of a document among the 30660 `artifact.version` rows, and the first versions of the 5467 documents among the 7191 `artifact.created` rows [c-types-3].
It leaves out the types whose rows hold no text a party wrote that the count has not read already: `ask.handed_back` (249 rows), whose payload is the ask as it stands; `ask.follower_added` (4) and `ask.follower_removed` (2), which hold no text; `comment.resolved` (35) and `comment.anchor_refreshed` (171), which hold the body of a comment `comment.created` counted; `comment.edited` (2), which holds no earlier body to tell what is new; `artifact.human_edited` (72), whose text is the document version it settles into; and the versions of uploaded files, an archive, a log or an image, which are no document [c-types-3] [c-method-5].

## What one instance is, and how a class is decided

One instance is one recorded act whose text matches any of three patterns.
Each pattern is one regular expression, written into the script and here [c-method-1]:

- a reference to a record Dispatch holds: `dispatch://(?:[A-Z][A-Z0-9]*-[0-9]+|[A-Z][A-Z0-9]+)(?:/[A-Za-z0-9._~@/-]*[A-Za-z0-9_~/-])?`

- a forty-character lower-case hexadecimal commit: `(?<![0-9A-Za-z])[0-9a-f]{40}(?![0-9A-Za-z])`

- a quoted span attributed to a named author, matched against one paragraph at a time, where a paragraph is the text between blank lines: `["\u201c\u201d][^"\u201c\u201d]*["\u201d][\s\S]*?(?:\u2014\s*[A-Z][A-Za-z.'-]+|said\s+[A-Z][A-Za-z.'-]+|[A-Z][A-Za-z.'-]+\s+wrote\b)`

Each type keeps the text a party wrote in one field, and the count reads only that field [c-method-4]:

- `comment.created`, `message.created` and `message.answered`: the body, which for a reply is the reply's own text and not the message it answers;

- `ask.opened`: the question, then each option's label and description;

- `ask.answered`: the text of the answer, and not the options it selects, which the asker wrote;

- `ask.resolved`: the reason the resolver gave;

- `ask.edited`: the question and the options, counted only for what is new against the ask before the edit, since a rewording repeats the ask it rewords.

A class is decided from the row's `actor` and from the issue tree of the record the text cites, and never from prose [c-method-2].

The actor's party is the row's `actor.kind`: `user` is a person, and every other kind is an agent [c-method-2].

The tree of an issue is found by walking `parent`, the link the command line prints as `child_of`, to the root [c-method-2].

The tree and the cited authors are read from the feed as they stood at the window's end, so a run taken later finds the same classes [c-tree-fixed-1].
An issue's parent is the one its `issue.created` row names, then every `child.added` and `child.removed` row, each row written before 2026-11-12T00:00:00Z, and the count never reads an issue as it stands now through the API [c-tree-fixed-1].

The cited record's author kind is read from the feed too:
a `/comment/<id>`, `/ask/<id>` or `/message/<id>` reference from the `comment.created`, `message.created` or `ask.opened` row that created that record, resolving a uuid prefix when it is unique;
a `/artifact/<slug>` reference from the authors of that document's first version;
and a bare issue reference or a `/spec` reference from the issue's creator [c-method-2].

The three classes overlap, so the script applies them in one fixed order and the first that holds wins [c-method-3]:

- between an agent and a person, when the actor's party differs from the cited record's author party;

- inside one tree, when the cited record's issue and the acting row's issue have the same root;

- across sessions, when the actor is a session and the cited record belongs to another issue.

Where a text cites more than one reference, the first by position that names an issue decides the class [c-method-3].

Beside the classes, the count reads who cited whose record [c-cited-session-all-1].
This is no fourth class.
It is not applied in the fixed order, and it reads no tree, so an instance counts in it whether the record it cites is in the acting row's own tree or in another.
It reads the instances that reach the classes, those whose first reference names an issue the deployment held and a record whose author the feed names, and it compares the row's `actor`, its kind and its id, with the cited record's author, its kind and its id [c-cited-session-all-1]:

- a session cited a record another session wrote, when the two ids differ;

- a session cited a record it wrote itself, when the two ids are the same;

- a session cited a record a person wrote;

- a person cited a record a session wrote;

- a person cited a record a person wrote.

A sixth part, other, holds an actor or an author of a third kind, such as `system`, and a session citing a session where the feed names no id for one of them; it reads 0 in every type [c-cited-session-types-1].
The parts add up: beside the instances that cite no record, cite an issue the deployment did not hold or cite a record whose author the feed does not name, they make up every instance [c-cited-session-all-1].
The first part is the coordination that crosses a session boundary, whatever the tree.
The class across sessions differs from it twice: it leaves out every citation inside the acting row's own tree, and it takes in a session citing its own record in another tree.

## The counts of comments and messages

The denominator is the records read in the window: 30186 `comment.created` and `message.created` rows of INFRA [c-denominator-1].

Of those, 11702 are instances [c-instances-1].

10013 carry a `dispatch://` reference [c-pattern-ref-1].

5463 carry a forty-character commit [c-pattern-sha-1].

63 carry a quoted span attributed to a named author [c-pattern-quote-1].

Each class carries that same denominator of 30186 records read:

- between an agent and a person: 138 [c-class-person-1]

- inside one tree: 6307 [c-class-tree-1]

- across sessions: 3477 [c-class-session-1]

No class reads zero over this denominator [c-class-zero-1].

The count first committed in this tree read the tree live, at about 07:35Z on 2026-11-12, after the window had closed, and put 6301 of these instances inside one tree and 3483 across sessions; read as it stood at the window's end, the tree puts the same instances at 6307 and 3477 [c-tree-fixed-1].

1780 instances are in no class [c-class-unclassed-1].
1702 of them cite no record at all, because their only match is a commit or a quotation [c-class-unclassed-1].
15 cite an issue this deployment does not hold, and 63 cite a record whose author the feed does not name [c-class-unclassed-1].

Read whatever the tree, 9922 of the 11702 instances cite a held record whose author the feed names: 5987 are a session citing a record another session wrote, 3797 a session citing a record it wrote itself, 136 a session citing a person's record and 2 a person citing a session's record, and none is a person citing a person's record [c-cited-session-types-1].

## The other written acts

Each of the other types carries its own denominator, the rows of that type read in the window.

Of the 736 `message.answered` rows, the replies to a message, 115 are instances: 112 carry a `dispatch://` reference, one a commit and three a quotation, and 2 are between an agent and a person, 4 inside one tree, 89 across sessions and 20 in no class [c-acts-message-answered-1].
Read whatever the tree, 95 of the 115 cite a held record whose author the feed names: 47 a session citing a record another session wrote, 46 a session citing its own, 2 a session citing a person's, and a person wrote none of them [c-cited-session-types-1].

Of the 1930 `ask.opened` rows, 557 are instances: 544 carry a reference, 29 a commit and 4 a quotation, and 23 are between an agent and a person, 267 inside one tree, 244 across sessions and 23 in no class [c-acts-ask-opened-1].
Read whatever the tree, 534 of the 557 cite a held record whose author the feed names: 314 a session citing a record another session wrote, 197 a session citing its own, 23 a session citing a person's, and a person wrote none of them [c-cited-session-types-1].

Of the 1198 `ask.answered` rows, 664 carry the text of an answer, and none of them is an instance [c-acts-ask-answered-0].
So none of them cites a record of any author either [c-cited-session-types-1].

Of the 744 `ask.resolved` rows, 173 are instances: 172 carry a reference, 5 a commit and one a quotation, and 29 are between an agent and a person, 51 inside one tree, 90 across sessions and 3 in no class [c-acts-ask-resolved-1].
Read whatever the tree, 170 of the 173 cite a held record whose author the feed names: 93 a session citing a record another session wrote, 48 a session citing its own, 29 a session citing a person's, and a person wrote none of them [c-cited-session-types-1].

Of the 1128 `ask.edited` rows, each read for what it adds to the ask before it, 70 are instances: 68 add a reference and 3 a commit, and 9 are between an agent and a person, 10 inside one tree, 45 across sessions and 6 in no class [c-acts-ask-edited-1].
Read whatever the tree, 64 of the 70 cite a held record whose author the feed names: 44 a session citing a record another session wrote, 11 a session citing its own, 9 a session citing a person's, and a person wrote none of them [c-cited-session-types-1].

All seven types together are 35922 records read, and 12617 of them are instances: 10909 carry a reference, 5501 a commit and 71 a quotation, and 201 are between an agent and a person, 6639 inside one tree, 3945 across sessions and 1832 in no class [c-acts-all-1].
Read whatever the tree, 10785 of the 12617 instances cite a held record whose author the feed names: 6485 are a session citing a record another session wrote, 4099 a session citing a record it wrote itself, 199 a session citing a person's record and 2 a person citing a session's record, and none is a person citing a person's record [c-cited-session-all-1].
So a session cites a record another session wrote in 6485 of these instances, where the class across sessions, which reads only the citations of another tree, holds 3945 [c-cited-session-all-1] [c-acts-all-1].

## Documents, read version by version

A document version is read whole through `GET /api/v1/artifacts/<id>/versions/<n>` beside the version before it, and only the references, commits and quotations new in it count, so a reference a document keeps across forty versions counts once, in the version that wrote it [c-method-6].
A version is an instance when any of the three is new in it, and its cited record is its first new reference by position [c-method-6].

Of the 30660 `artifact.version` rows of INFRA in the window, 29709 are versions of a document, and each was read beside the version before it; the other 951 are versions of an uploaded file, which are not read [c-docs-versions-1].
6920 of the 29709 add a reference, a commit or a quotation the version before did not hold: 6063 a reference, 818 a commit and 425 a quotation [c-docs-versions-1].
330 of them are between an agent and a person, 2770 inside one tree, 2847 across sessions and 973 in no class [c-docs-versions-1].
Read whatever the tree, 5947 of the 6920 cite a held record whose author the feed names: 3732 a session citing a record another session wrote, 1885 a session citing its own, 330 a session citing a person's, and a person wrote none of them [c-cited-session-types-1].

The first versions of the documents created in the window are a line of their own, since everything in a first version is new [c-method-6].
Of the 7191 `artifact.created` rows, 5467 are documents, each read whole, and 1724 are uploaded files and images, which are not read [c-docs-first-1].
4235 of the 5467 are instances: 3540 carry a reference, 1623 a commit and 193 a quotation, and 54 are between an agent and a person, 3013 inside one tree, 385 across sessions and 783 in no class [c-docs-first-1].
Read whatever the tree, 3452 of the 4235 cite a held record whose author the feed names: 2582 a session citing a record another session wrote, 816 a session citing its own, 54 a session citing a person's, and a person wrote none of them [c-cited-session-types-1].

## The classes, type by type

The same counts as one table, which is what a reader and the gate both read.
Each row carries its count and its denominator, the records of its type read in the window:

| class | instances | denominator | records read |
| --- | --- | --- | --- |
| between an agent and a person | 138 | 30186 | comments and messages |
| inside one tree | 6307 | 30186 | comments and messages |
| across sessions | 3477 | 30186 | comments and messages |
| between an agent and a person | 2 | 736 | replies to a message |
| inside one tree | 4 | 736 | replies to a message |
| across sessions | 89 | 736 | replies to a message |
| between an agent and a person | 23 | 1930 | asks opened |
| inside one tree | 267 | 1930 | asks opened |
| across sessions | 244 | 1930 | asks opened |
| between an agent and a person | 0 | 1198 | asks answered |
| inside one tree | 0 | 1198 | asks answered |
| across sessions | 0 | 1198 | asks answered |
| between an agent and a person | 29 | 744 | asks resolved |
| inside one tree | 51 | 744 | asks resolved |
| across sessions | 90 | 744 | asks resolved |
| between an agent and a person | 9 | 1128 | asks edited |
| inside one tree | 10 | 1128 | asks edited |
| across sessions | 45 | 1128 | asks edited |
| between an agent and a person | 201 | 35922 | all written acts |
| inside one tree | 6639 | 35922 | all written acts |
| across sessions | 3945 | 35922 | all written acts |
| between an agent and a person | 330 | 29709 | document versions |
| inside one tree | 2770 | 29709 | document versions |
| across sessions | 2847 | 29709 | document versions |
| between an agent and a person | 54 | 5467 | first versions of documents |
| inside one tree | 3013 | 5467 | first versions of documents |
| across sessions | 385 | 5467 | first versions of documents |
| a quoted span attributed to a named author, in the handoff sample | 0 | 3143 | handoffs read |

Two groups of rows read zero over a denominator above zero: the three classes of the answers to asks, and the last row, which is the sample of the section below, with its own denominator.

## The smallest class

Coordination between an agent and a person is the smallest of the three classes in every type the count classes that has an instance, and the answers to asks have none [c-smallest-1].
The smaller of the two other classes is 25.2 times its size in comments and messages, 19.6 times over all written acts, 10.6 times in asks opened, 8.4 times over document versions and 7.1 times over first versions [c-smallest-1].
It is less than three times its size only in the replies to a message (2.0 times), the asks resolved (1.8) and the asks edited (1.1), whose 358 instances are under 3% of the 12617 instances of written acts [c-smallest-1].

The front page should read the party count as it is.
138 of 30186 records read, and 138 of 11702 instances, name a record whose author is of the other party [c-class-person-1].
Over all written acts it is 201 of 35922 records read and of 12617 instances [c-acts-all-1], and over document versions 330 of 29709 versions read and of 6920 instances [c-docs-versions-1].
Coordination between an agent and a person is the smallest of the three classes of comments and messages by more than an order of magnitude [c-class-person-1].

## What a person writes when they answer an ask

None of the 664 answers with text that people wrote to asks in the window carries a `dispatch://` reference, a forty-character commit or an attributed quotation [c-acts-ask-answered-0].
A scan of the same 664 texts, every one written by a person, finds 3 that name an issue by its key alone and 15 that hold a web address, and none that holds a reference or a commit [c-ask-answered-scan-1].
This is a class that reads zero over a denominator above zero, and the judgement's front page must state that premise as failed [c-acts-ask-answered-0].
By my reading, a person answers an ask in words alone, so a structured reference on a message would come from the agent's side.

## Handoffs, counted over a sample of their own

A handoff body is not in the feed.
A `handoff.recorded` row carries only `handoff_id`, `phase` and `version`, and never the body [c-handoff-feed-1].

So handoff bodies were read over a named sample.
The sample is the ten INFRA issues with the most `handoff.recorded` rows inside the window, ordered by that count and then by key: INFRA-1192, INFRA-1077, INFRA-714, INFRA-771, INFRA-941, INFRA-2464, INFRA-1441, INFRA-558, INFRA-956, INFRA-1519 [c-handoff-sample-1].

Every handoff of those ten issues recorded inside the window was read whole through `GET /api/v1/handoffs/<id>`: 3143 handoffs, which is the sample's own denominator [c-handoff-sample-1].

3006 of those 3143 are instances [c-handoff-sample-1].
2402 carry a `dispatch://` reference and 2288 carry a forty-character commit [c-handoff-sample-1].

Nought of the 3143 handoffs carries a quoted span attributed to a named author [c-handoff-quote-0].
This is a class that reads zero over a denominator above zero, and the judgement's front page must state that premise as failed: in the sample, 2288 handoffs name a commit and 2402 a `dispatch://` reference [c-handoff-sample-1], and none carries another agent's words with the name of who wrote them [c-handoff-quote-0].

## What this record cannot hold: one session's own coordination

The share of coordination inside one session, between an agent and the subagents it starts, is not in this record [c-session-absent-1].

Every one of the thirty-nine event types INFRA wrote inside the window names an act on an issue, a document, an ask, a comment, a message, a handoff, a result or a setting, and no type names a subagent, a task, a turn or a tool call [c-session-absent-1].

That coordination is in the shared session store, which the pod this reading ran in could read only through the agent login that writes the live conversations.
The pod this reading ran in held `OMP_SESSION_SQL_DSN_FILE`, the writer's URL, and no `SESSION_READER_DSN_FILE`, the read-only login a deployment gives where it gives one [c-session-route-1].
The exact checks and their empty output are in `evidence/session-reader.txt` [c-session-route-1].

Our own instructions say what that absence means: where a deployment names no read-only route, a role reads nothing of the shared database and reports the gap, rather than reaching for the writer URL [c-session-rule-1].

On 2026-11-12, dispatch://INFRA-1325, then at status `todo`, was the issue that would open it: its title says it puts the sessions reader URL in the role containers as a file and tells every role to use it [c-session-open-1].

## What Legion, Envoy and Dispatch do today for the three alignment problems

### Agreement on the plan before the work

Legion holds a tree's work until a person has approved a named document.
The daemon declares the steps of `gates.design`, `decisions`, `criteria` (its Verification document) and `spec` (its primary document), and the two gate words `root-issues` and `off` [c-plan-gate-1].

By my reading of `config/stage3.go` and `record/project.go`, which no row prints, an unset list is the one step `spec`, which `root-issues` also names, `off` or `[]` gates nothing and asks no person, and the gate is a record the daemon keeps per step, on which the tree's phases, each released child and the merger's READY wait.

Dispatch holds the act itself: a document's approval request is an ask whose answer approves that version or requests changes, and the server hands the request back to the agent when the document changes [c-plan-dispatch-1].

Envoy carries that act to whoever reads the issue's topic: every issue event, those of the issue's own documents among them, is published on the issue's own topic, `notifications.dispatch.issue.<KEY>.<type>`, and the daemon's intake consumes every issue's topic [c-plan-envoy-1].

In the window this fleet approved documents 2052 times and requested changes 215 times [c-plan-approve-1].
Every one of the 215 changes requests was written by a person, 124 of them on a `spec.md` and 88 on a `verification.md`, and not one on a `plan.md` [c-plan-approve-1].
All 410 approvals of a `spec.md` and all 318 approvals of a `verification.md` were written by a person [c-plan-approve-1].
All 1323 approvals of a `plan.md` were written by a session [c-plan-approve-1].

So the plan a worker follows is agreed between agents, and the spec it serves is agreed with a person.
dispatch://INFRA-1066 is one instance: its plan document's approval line names a session id, not a person's address [c-plan-line-1].
dispatch://INFRA-481 is the other side: a person approved its spec, and its approval line names a person's address and no session.
Six of the eight approvals of its spec written before the window closes fall inside the window, the first at 2026-10-29T00:37:14Z and the last at 2026-11-10T11:39:40Z, and each was written by a person [c-plan-approve-2].

What the plan agreement does not carry is a person's reading.
The approval of a plan is an agent's, so no part of this record shows a person agreed to the plan before the work [c-plan-gap-1].

### Agreement on the verification criteria after the work

Dispatch holds the criteria as typed blocks, not as prose.
The document block schema has one `check` block type, whose attributes are `serves` (the requirement blocks it proves), `surface` (browser, cli or api), `run` (the command), `when` (before_merge, after_merge or default_branch) and `negative_control` [c-crit-block-1].

The server reads those blocks out of the issue's Verification document and records a result against one of them, with the role that ran it, its verdict and the commit it ran at [c-crit-results-2].

dispatch://INFRA-1095's Verification document, at its one version, carries six checks, five before the merge and one after it, and before 2026-11-12 each had a result recorded with its verdict, the commit it was proved at and the role that recorded it [c-crit-results-1].

Envoy publishes each recorded result on the same issue topic [c-plan-envoy-1].

In the window 40291 results were recorded, and every one of them was written by a session [c-crit-who-1].
The role each names is an agent's role: 22333 an implementer, 17610 a tester, 328 a reviewer, 18 an architect, one a merger and one a cli [c-crit-who-1].
Nothing in the record is a person's verdict on a check [c-crit-who-1].

A person does agree the criteria themselves: all 318 approvals of a `verification.md` in the window were a person's [c-plan-approve-1].

Dispatch reports, at the moment of an approval, the requirements of a spec that no check and no plan task serves, and that report refuses nothing [c-crit-coverage-1].

### Agreement on the terms used

Nothing in Dispatch holds a definition of a term.
The typed block schema has five block types, `callout`, `ask`, `requirement`, `plan_task` and `check`, and none of them defines a term [c-terms-1].

The project record holds a glossary as an ordinary document: `dispatch://INFRA-460/artifact/glossary-md`, at three versions written at or before 2026-11-16T13:00:00Z, on a standup issue [c-terms-2].

The reference graph holds no link to it from anything the project wrote.
The backlinks to `dispatch://INFRA-460/artifact/glossary-md` written by 2026-11-16T13:00:00Z number one, a block of dispatch://INFRA-3134/artifact/judgement-md, this reading itself [c-terms-3].
So, by 2026-11-16T13:00:00Z, the glossary was cited by nothing but this reading [c-terms-3].

Our own tree holds no glossary either.
At 9542c6fb the word appears in one file, the ASD-STE100 writing skill, which tells a writer that a project glossary is allowed and notes that the dictionary it would extend is absent [c-terms-4].
No file under `packages` or `deploy` holds the word at all, so no part of the product keeps a term's definition [c-terms-7].

`skills/plain-words` and `skills/asd-ste100` are instructions an agent reads before it writes [c-terms-5], and by my reading they are what Legion gives instead: word discipline for the writer, not a record two agents can agree on and cite.

Envoy's publisher names an event's owner topic under `notifications.dispatch.`, from the recorded event's document, issue or project and its type [c-terms-6], so with no record of a term, which is what [c-terms-1] [c-terms-3] [c-terms-7] find, there is nothing of a term for it to carry.

So, by my reading of those rows, of the three problems two have a surface in the record and one has none: a plan and a Verification document each have a document, an approval and an event, and a term has a document nobody cites and no block, no route and no check.

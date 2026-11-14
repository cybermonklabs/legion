# Where this fleet's coordination is recorded, over a fixed window

Requirement r4.
Every count below was taken by one script, `count.py`, over one window, and can be taken again.
The script reads only the window file and the token file, and prints one JSON object.
Its recorded output is `evidence/count.json`.

Every command below is run from the repository root, with `docs/research/internet-of-cognition/env.sh` sourced: it exports `IOC_EVID`, the committed `evidence/` directory this file's recorded outputs stand in.

## The window

The window is the fourteen days before 2026-11-12T00:00:00Z, that is from 2026-10-29T00:00:00Z to 2026-11-12T00:00:00Z, and the project is INFRA [c-window-1].

Inside it the global event feed runs from event id 103048 to event id 402993 [c-window-1].

The feed is one list for the whole deployment, ascending by event id, and each row carries its project and the instant it was written, so a window and a project are a filter over it and nothing else [c-window-2].

A row of the feed is one recorded act.
Thirty-nine event types were written for INFRA inside the window [c-types-1].
Five of them carry a body in the feed, read over the first thousand events of the window: `comment.anchor_refreshed`, `comment.created`, `comment.resolved`, `message.answered` and `message.created`.
The two this count reads are `comment.created` and `message.created`, which are the two that carry the whole text of what a party wrote [c-types-2].

## What one instance is, and how a class is decided

One instance is one recorded act whose body matches any of three patterns.
Each pattern is one regular expression, written into the script and here [c-method-1]:

- a reference to a record Dispatch holds: `dispatch://(?:[A-Z][A-Z0-9]*-[0-9]+|[A-Z][A-Z0-9]+)(?:/[A-Za-z0-9._~@/-]*[A-Za-z0-9_~/-])?`

- a forty-character lower-case hexadecimal commit: `(?<![0-9A-Za-z])[0-9a-f]{40}(?![0-9A-Za-z])`

- a quoted span attributed to a named author, matched against one paragraph at a time, where a paragraph is the text between blank lines: `["\u201c\u201d][^"\u201c\u201d]*["\u201d][\s\S]*?(?:\u2014\s*[A-Z][A-Za-z.'-]+|said\s+[A-Z][A-Za-z.'-]+|[A-Z][A-Za-z.'-]+\s+wrote\b)`

A class is decided from the row's `actor` and from the issue tree of the record the body cites, and never from prose [c-method-2].

The actor's party is the row's `actor.kind`: `user` is a person, and every other kind is an agent [c-method-2].

The tree of an issue is found by walking `parent`, the link the command line prints as `child_of`, to the root through `GET /api/v1/issues/<KEY>`, with each answer cached [c-method-2].

The cited record's author kind is read from the feed too:
a `/comment/<id>`, `/ask/<id>` or `/message/<id>` reference from the row that created that record, resolving a uuid prefix when it is unique;
a `/artifact/<slug>` reference from the authors of that document's first version;
and a bare issue reference or a `/spec` reference from the issue's creator [c-method-2].

The three classes overlap, so the script applies them in one fixed order and the first that holds wins [c-method-3]:

- between an agent and a person, when the actor's party differs from the cited record's author party;

- inside one tree, when the cited record's issue and the acting row's issue have the same root;

- across sessions, when the actor is a session and the cited record belongs to another issue.

Where a body cites more than one reference, the first by position decides the class [c-method-3].

## The counts

The denominator is the records read in the window: 30186 `comment.created` and `message.created` rows of INFRA [c-denominator-1].

Of those, 11702 are instances [c-instances-1].

10013 carry a `dispatch://` reference [c-pattern-ref-1].

5463 carry a forty-character commit [c-pattern-sha-1].

63 carry a quoted span attributed to a named author [c-pattern-quote-1].

Each class carries that same denominator of 30186 records read:

- between an agent and a person: 138 [c-class-person-1]

- inside one tree: 6301 [c-class-tree-1]

- across sessions: 3483 [c-class-session-1]

No class reads zero over this denominator [c-class-zero-1].

The same counts as one table, which is what a reader and the gate both read:

| class | instances | denominator |
| --- | --- | --- |
| between an agent and a person | 138 | 30186 |
| inside one tree | 6301 | 30186 |
| across sessions | 3483 | 30186 |
| a quoted span attributed to a named author, in the handoff sample | 0 | 3143 |

Each class carries its denominator, the records read in the window.
The last row is the sample of the section below, with its own denominator, and it reads zero over a denominator above zero.

1780 instances are in no class [c-class-unclassed-1].
1702 of them cite no record at all, because their only match is a commit or a quotation [c-class-unclassed-1].
15 cite an issue this deployment does not hold, and 63 cite a record whose author the feed does not name [c-class-unclassed-1].

The front page should read the party count as it is.
138 of 30186 records read, and 138 of 11702 instances, name a record whose author is of the other party [c-class-person-1].
Coordination between an agent and a person is the smallest of the three classes by more than an order of magnitude [c-class-person-1].

## Handoffs, counted over a sample of their own

A handoff body is not in the feed.
A `handoff.recorded` row carries only `handoff_id`, `phase` and `version`, and never the body [c-handoff-feed-1].

So handoff bodies were read over a named sample.
The sample is the ten INFRA issues with the most `handoff.recorded` rows inside the window, ordered by that count and then by key: INFRA-1192, INFRA-1077, INFRA-714, INFRA-771, INFRA-941, INFRA-2464, INFRA-1441, INFRA-558, INFRA-956, INFRA-1519 [c-handoff-sample-1].

Every handoff of those ten issues recorded inside the window was read whole through `GET /api/v1/handoffs/<id>`: 3143 handoffs, which is the sample's own denominator [c-handoff-sample-1].

3006 of those 3143 are instances [c-handoff-sample-1].
2402 carry a `dispatch://` reference and 2288 carry a forty-character commit [c-handoff-sample-1].

Nought of the 3143 handoffs carries a quoted span attributed to a named author [c-handoff-quote-0].
This is a class that reads zero over a denominator above zero, and the judgement's front page must state that premise as failed: a handoff names a commit and an issue, and never carries another agent's words with the name of who wrote them [c-handoff-quote-0].

## What this record cannot hold: one session's own coordination

The share of coordination inside one session, between an agent and the subagents it starts, is not in this record [c-session-absent-1].

Every one of the thirty-nine event types INFRA wrote inside the window names an act on an issue, a document, an ask, a comment, a message, a handoff, a result or a setting, and no type names a subagent, a task, a turn or a tool call [c-session-absent-1].

That coordination is in the shared session store, which this pod may read only through the agent login that writes the live conversations.
The pod holds `OMP_SESSION_SQL_DSN_FILE`, the writer's URL, and holds no `SESSION_READER_DSN_FILE`, the read-only login a deployment gives where it gives one [c-session-route-1].
The exact checks and their empty output are in `evidence/session-reader.txt` [c-session-route-1].

Our own instructions say what that absence means: where a deployment names no read-only route, a role reads nothing of the shared database and reports the gap, rather than reaching for the writer URL [c-session-rule-1].

dispatch://INFRA-1325 is the issue that would open it: it puts the sessions reader URL in the role containers as a file and tells every role to use it [c-session-open-1].

## What Legion, Envoy and Dispatch do today for the three alignment problems

### Agreement on the plan before the work

Legion holds a tree's work until a person has approved a named document.
The daemon reads `gates.design` as an ordered list of steps, each step one document of the root issue: `decisions`, `criteria` (its Verification document) and `spec` (its primary document); the default is `[spec]`, the word `root-issues` means that, and `off` or `[]` gates nothing and asks no person [c-plan-gate-1].

The gate is a record the daemon keeps per step, and the tree's phases, each released child and the merger's READY wait on it [c-plan-gate-1].

Dispatch holds the act itself: a document's approval request is an ask whose answer approves that version or requests changes, and the server hands the request back to the agent when the document changes [c-plan-dispatch-1].

Envoy carries that act to whoever must read it: every issue event is published on the issue's own topic, `notifications.dispatch.issue.<KEY>.<type>`, so the approval of a spec reaches the tree's architect without anybody polling for it [c-plan-envoy-1].

In the window this fleet approved documents 2052 times and requested changes 215 times [c-plan-approve-1].
Every one of the 215 changes requests was written by a person, 124 of them on a `spec.md` and 88 on a `verification.md`, and not one on a `plan.md` [c-plan-approve-1].
All 410 approvals of a `spec.md` and all 318 approvals of a `verification.md` were written by a person [c-plan-approve-1].
All 1323 approvals of a `plan.md` were written by a session [c-plan-approve-1].

So the plan a worker follows is agreed between agents, and the spec it serves is agreed with a person.
dispatch://INFRA-1066 is one instance: its plan document's approval line names a session id, not a person's address [c-plan-line-1].
dispatch://INFRA-481 is the other side: a person approved its spec, and its approval line names a person's address and no session.
That approval is at 2026-10-28T00:16:55Z, which is before this window opens, so it is evidence of who approves a spec and not a count inside the window [c-plan-approve-2].

What the plan agreement does not carry is a person's reading.
The approval of a plan is an agent's, so no part of this record shows a person agreed to the plan before the work [c-plan-gap-1].

### Agreement on the verification criteria after the work

Dispatch holds the criteria as typed blocks, not as prose.
The document block schema has one `check` block type, whose attributes are `serves` (the requirement blocks it proves), `surface` (browser, cli or api), `run` (the command), `when` (before_merge, after_merge or default_branch) and `negative_control` [c-crit-block-1].

The server reads those blocks out of the issue's Verification document and records a result against one of them, with the role that ran it, its verdict and the commit it ran at [c-crit-results-2].

`dispatch results --issue INFRA-1095` prints six checks, each with its verdict, the commit it was proved at and the role that recorded it, five before the merge and one after it [c-crit-results-1].

Envoy carries each recorded result on the same issue topic, so the architect that accepts the work reads the verdicts without asking for them [c-plan-envoy-1].

In the window 40291 results were recorded, and every one of them was written by a session [c-crit-who-1].
The role each names is an agent's role: 22333 an implementer, 17610 a tester, 328 a reviewer, 18 an architect, one a merger and one a cli [c-crit-who-1].
Nothing in the record is a person's verdict on a check [c-crit-who-1].

A person does agree the criteria themselves: all 318 approvals of a `verification.md` in the window were a person's [c-plan-approve-1].

Dispatch reports, at the moment of an approval, the requirements of a spec that no check and no plan task serves, and that report refuses nothing [c-crit-coverage-1].

### Agreement on the terms used

Nothing in Dispatch holds a definition of a term.
The typed block schema has five block types, `callout`, `ask`, `requirement`, `plan_task` and `check`, and none of them defines a term [c-terms-1].

The project record holds a glossary written by hand, as an ordinary document: `dispatch://INFRA-460/artifact/glossary-md`, at three versions, on a standup issue [c-terms-2].

The reference graph holds no link to it.
Backlinks to `dispatch://INFRA-460` number 402, and backlinks to `dispatch://INFRA-460/artifact/glossary-md` numbered 0 when the count was taken.
They number 1 now, and the one link is a block of dispatch://INFRA-3134/artifact/judgement-md, this reading itself [c-terms-3].
So the one glossary this project wrote is cited by nothing it wrote [c-terms-3].

Our own tree holds no glossary either.
At 9542c6fb the word appears in one file, the ASD-STE100 writing skill, which tells a writer that a project glossary is allowed and notes that the dictionary it would extend is absent [c-terms-4].
No file under `packages` or `deploy` holds the word at all, so no part of the product keeps a term's definition [c-terms-7].

What Legion gives instead is word discipline for the writer: `skills/plain-words` and `skills/asd-ste100` are instructions an agent reads before it writes, not a record two agents can agree on and cite [c-terms-5].

Envoy carries whatever Dispatch records, by topic and by name, so with no record of a term there is nothing of a term for it to carry [c-terms-6].

So of the three problems, two have a surface in the record and one has none: a plan and a Verification document each have a document, an approval and an event, and a term has a document nobody cites and no block, no route and no check [c-terms-6].

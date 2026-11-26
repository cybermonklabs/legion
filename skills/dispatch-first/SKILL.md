---
name: dispatch-first
description: "Use when a session has the dispatch command."
---

# Dispatch first

## Search before you act

- Before you plan, file an issue, ask, post a finding or start work, run `dispatch search`.
- Each query word must match. Use two or three: the thing and what is wrong. Never paste a draft.

```bash
dispatch search --query 'broadcast send order'
dispatch search --query 'reviewer threads OR review comments'
```

- No hits: remove a word before adding one. Try two or three wordings before deciding nothing exists.
- Open each hit that can be yours with `dispatch read --issue KEY`, then its parent (`child_of` under `Links:`, or a document's `Issue:`), with its status and recent events.

## What to do with what you find

`dispatch issue` and `dispatch ask` also search. Open each `advice.suggestions` hit too.

- **Work already tracked:** put the finding on that issue. It is no reason to wait: fix what stops you, in any repository, and say on the issue what you did. Nobody owns code, and an issue may have several pull requests. Wait only on a claimed, `in_progress` issue whose holder said on Envoy it works on it now, and tell the holder what you need by when. File a new issue only when no hit covers the work, and cite the nearest one you ruled out (`dispatch://KEY`).
- **Question already decided:** cite the answered ask (`dispatch://KEY/ask/<id>`) and never ask again.
- **Duplicate found, or a likely duplicate a create names:** fold it only when it is the same work. Keep the issue with the spec, copy to it what only the duplicate has, and close the duplicate (`--status done --reason 'Duplicate of dispatch://KEY.'`). When it has the `legion` label or another holder's claim, comment the kept issue's name on it instead.

## Research before you ask

- Research when a file, command or document can show the answer.
- Ask a person only for what their authority, taste or risk appetite settles. Where a file records the decision, cite it (`file:line`) and follow it; ask again only when the facts it rested on changed.
- Say what you searched: each part you found with its source, and each part you did not, with the queries you ran.

## Reach a person only through Dispatch

- The moment a person blocks your work, open an ask: an ask block for a design question its document records, else `dispatch ask`. A blocker that is not an open ask does not exist for them. A Legion phase worker sends decisions to its architect instead. Before you say you wait on a person, run `dispatch open-asks`. A secret request that waits for a person is already in their Inbox, so it gets no ask (`skill://agent-secrets`).
- A result a person asked for ends its document with an ask block asking their next step, else `dispatch ask`.
- Record each request you accept in Dispatch before you start it. Your issue's work is a task, or a task's new text, in its plan. Other work is an issue or an ask. Before you end a turn, read the plan, your issues and open asks: each item is done, in progress, or an open ask. Never drop one.

## Which skill to read

| Before you | Read |
| --- | --- |
| run your first `dispatch` command in this session | `skill://dispatch` |
| write or change a spec, ask, comment or message a person reads | `skill://dispatch-writing-specs` |
| plan work whose spec a person approved | superpowers' `writing-plans` (`skill://dispatch-writing-specs`, "The plan") |
| write a Legion architect's own spec | `skill://legion-architect` too |

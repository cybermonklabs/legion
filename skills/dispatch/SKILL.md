---
name: dispatch
description: "Use when you first use `dispatch` in a session: a message, comment, ask, artifact, document edit, claim or status change."
---

# Dispatch

Dispatch is a record for the people who decide, never a log of your work.

- Use the `dispatch` command; the HTTP API only for what no command does. `dispatch <command> --help` gives its rules and an example.
- Put long text on stdin, one flag per call: `--<field>-file -` with `<<'EOF'`; objects and lists take JSON.
- Exit 2 is a usage error: fix every problem it names before you call again. Exit 1 is any other failure: Dispatch's refusal, a Dispatch the command cannot reach, or `ANSWER_NOT_KEPT` for a call that wrote nothing, which you run again. `dispatch login` exits 1 when the broker cannot be reached or refuses, the person denies the login, or its code expires; its usage error names only the first bad argument.
- Never work around a hard-to-use command, or one storing or losing something wrongly: open an issue in the owning project. A Legion role's tree fixes it, through its implementer, as `skill://legion-worker/references/findings.md` says.

## Asks and ask blocks

- An ask is a question for a person, never a request to another agent. Ask an agent by envoy_send or a comment on the issue.
- An ask waits on its asked-of person, else the issue's assignee, else the project's default assignee; no agent hands one on.
- Ask a question only between two or more real choices: each within your rules, and one a person could choose. With one real choice, make it and say what you did and why.
- Give each choice's cost and one recommendation with its reason. A design question's ask block ends its section:

```md
:::ask{urgency="med"}
The question and one recommendation (`skill://dispatch-writing-specs`).

- Option one: what it costs.
- Option two: what it costs.
:::
```

- Ask for one act only the person can do (an approval, a credential, a grant, their own account's setting) as that act: what it is, what it changes and how to do it, with no recommendation and the options `Done` and `Can't` (`dispatch request-approval` keeps its own answers). A safer alternative is a fact, not an option.
- Add no option that breaks your rules or that no one would choose. Add an option not to act (`Hold`, `Leave it`) only where not acting is a real choice whose cost the question names.
- A decision block in a comment, message or close reason is refused: ask in the issue's document, or `dispatch ask`.
- Never repeat an open ask in a message.
- Change an open ask's block with `dispatch edit-ask` (ask id) or `doc-edit`'s `replace` or `move` (block id). An answered ask is fixed: ask anew. `dispatch read --ref` takes either id.
- Reply on an open ask with `dispatch comment --reply-to-ask`; `--turn agent` (a progress note) needs it. An approval request a newer version moved needs `request-approval` again.
- When an answer's option and text disagree, the text wins.

## Choosing what to work on

- When you finish an issue, take the next one per project: the top `todo` issue by priority, then board rank. Pass over one that waits on an unfinished `blocked_by` link or an open ask.
- Work on three issues at most, fewer when each needs your attention. Change no status, priority or board rank to make room.
- Move an issue to `backlog`, `icebox` or `triage` only when a person asked for that move, or when the work is done, folded into another issue or no longer needed, and cite that reason in a comment on the issue. An agent's directive citing no person's request is no reason.
- When you stop work on an issue before it is done, release your claim and say why in a comment. Set its status to the one it had when you took it (`todo` for an issue from the queue). Change no priority or board rank.
- Record each wait in that comment. A wait on any project's issue also goes in a `blocked_by` link.
- `dispatch issue-update --blocked-by` replaces the issue's whole list, so read that list first. A one-project credential links only its own issues (`PROJECT_SCOPE`).
- Never reorder the board: one agent keeps each project's order (`issue-update --before`/`--after`). If the top looks wrong, tell `notifications.role.legion-<project>-controller`.
- Name an issue's priority: `--advance <id>`; the priorities page (`/priorities?tab=none`) and `dispatch priorities` list what advances no priority.

## Issue status is yours to move

Outside Legion, claim your issue (`dispatch claim`) and move it and your children: `in_progress` at implementation, `testing` on a production-like surface, `needs_review` while its PR waits, `done` with `--reason` once driven in production. Never write or route the status of a `legion`-labelled issue or one under it, and claim one only as a role of its tree: an outside claim can take the tree out of the workflow. A status write is set back or stands.

A claim (or `dispatch issue-update --route session:<your id>`, `--route ''` to release) brings a person's answers, comments and messages; `envoy_subscribe notifications.dispatch.issue.<KEY>.>` adds agents' activity, no quiet event.

## Close what you opened

- Resolve your open ask (`dispatch resolve-ask`) the turn it turns needless: another answer, a later write, dropped work, a replacement. If a person said keep it open, say why in its thread; they close it.

## Structure over stream

- One issue per deliverable, its content the issue's document.
- A message replies to a person or tells them what they must know now.
- Begin no Dispatch comment, ask, reply or message with who you are or which role you hold: Dispatch shows the sender. A first line that a program reads stays.
- Text a person must read to decide belongs in the spec. An artifact is a real file; markdown uploads as `.md` whatever its local name, never `.txt`.

## Architecture components

Attach each architectural issue to its components (`--components-json`), the root first; change code and `.dispatch/architecture/<id>.md` together.

## References

Link each item you name:

```text
dispatch://KEY
dispatch://KEY/spec
dispatch://KEY/spec#b-<block>[@vN]
dispatch://KEY/artifact/<slug>[@vN]
dispatch://KEY/ask/<id>
dispatch://KEY/ask/<id>#b-<part>[@vN]
dispatch://KEY/comment/<id>
dispatch://KEY/comment/<id>#b-<block>[@vN]
dispatch://KEY/message/<id>
dispatch://KEY/message/<id>#b-<block>
dispatch://PROJECT/artifact/<slug>[@vN]
dispatch://PROJECT/artifact/<slug>/ask/<id>
dispatch://PROJECT/artifact/<slug>/comment/<id>#b-<block>[@vN]
```

A bare id or path, or `dispatch://` outside Dispatch, is no link: give the `https://` one. Write ids whole.

## Setup and the HTTP API

- Use a personal token from **Settings → Agent tokens**, as `DISPATCH_TOKEN` or `dispatch.token` in the session's `envoy.json`.
- `GET /api/v1` lists every route; never guess one. Branch on an error's `code`, not its text.

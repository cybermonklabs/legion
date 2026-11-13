---
name: dispatch
description: "Use before your first dispatch command in a session: a message, comment, ask, artifact, document edit, claim or status change."
---

# Dispatch

Dispatch is a record for the people who decide, never a log of your work.

- Use the `dispatch` command; the HTTP API only for what no command does. `dispatch <command> --help` gives its rules and an example.
- Put long text on stdin: `--<field>-file -` with a quoted here-document (`<<'EOF'`); give object and list fields as JSON.
- Exit 1 is a refusal, 2 a usage error. A refusal lists every problem: fix all, call once.
- Never work around a hard-to-use command, or one storing or losing something wrongly: open an issue in the owning project and tell its controller. A Legion role tells its architect (`skill://legion-worker/references/findings.md`).

## Asks and ask blocks

- An ask is a question for a person, never a request to another agent. Ask an agent by envoy_send or a comment on the issue.
- An ask waits on the issue's assignee until a person hands it on. No agent can hand one on.
- A design question's ask block ends its section:

```md
:::ask{urgency="med"}
The question and the recommendation (`skill://dispatch-writing-specs`).

- Option one: what it costs.
- Option two: what it costs.
:::
```

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
- Record each wait in that comment. A wait on a same-project issue also goes in a `blocked_by` link, and one on a person in a `dispatch ask`.
- `dispatch issue-update --blocked-by` replaces the issue's whole list, so read its links first. Dispatch refuses a link to another project's issue (`BLOCKED_BY_OUTSIDE_PROJECT`).
- Never reorder the board: one agent keeps each project's order (`issue-update --before`/`--after`). If the top looks wrong, tell `notifications.role.legion-<project>-controller`.
- Name an issue's priority: `--advance <id>`; `dispatch priorities` lists what advances no priority.

## Issue status is yours to move

Outside Legion, claim your issue (`dispatch claim`) and move it and your children: `in_progress` at implementation, `testing` on a production-like surface, `needs_review` while its PR waits, `done` with `--reason` once driven in production. Never write or route the status of a `legion`-labelled issue or one under it, and claim one only as a role of its tree: an outside claim can take the tree out of the workflow. A status write is set back or stands.

A claim (or `dispatch issue-update --route session:<your id>`, `--route ''` to release) brings a person's answers, comments and messages; `envoy_subscribe notifications.dispatch.issue.<KEY>.>` adds agents' activity, no quiet event.

## Close what you opened

- Resolve your open ask (`dispatch resolve-ask`) the turn it turns needless: another answer, a later write, dropped work, a replacement. If a person said keep it open, say why in its thread; they close it.

## Structure over stream

- One issue per deliverable, its content the issue's document.
- A message replies to a person or tells them what they must know now.
- Text a person must read to decide belongs in the spec. An artifact is a real file.

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

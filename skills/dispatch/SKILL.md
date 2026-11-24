---
name: dispatch
description: "Use when you first use `dispatch` in a session: a message, comment, ask, artifact, document edit, claim or status change."
---

# Dispatch

Dispatch stays a record for the people who decide, never a log of your work.

- Use the `dispatch` command; the HTTP API only for what no command does. `dispatch <command> --help` gives its rules and an example.
- Put long text on stdin: `--<field>-file -` with `<<'EOF'`. One flag per call reads it.
- Objects and lists take JSON.
- Exit 2 is a usage error: fix every problem it names before you call again.
- Exit 1 is any other failure: Dispatch's refusal, a Dispatch the command cannot reach, or `ANSWER_NOT_KEPT` for a call that wrote nothing, which you run again.
- `dispatch login` exits 1 when the broker cannot be reached or refuses, the person denies the login, or its code expires; its usage error names only the first bad argument.
- Never work around a hard-to-use command, or one storing or losing something wrongly: open an issue in the owning project and tell its controller. A Legion role tells its architect (`skill://legion-worker/references/findings.md`).

## Asks and ask blocks

- An ask is a question for a person, never a request to another agent. Ask an agent by envoy_send or a comment on the issue.
- An ask waits on its asked-of person, else the issue's assignee, else the project's default assignee; no agent hands one on.
- A design question's ask block ends its section:

```md
:::ask{urgency="med"}
The question and the recommendation (`skill://dispatch-writing-specs`).

- Option one: its cost.
- Option two: its cost.
:::
```

- A decision block in a comment, message or close reason is refused: ask in the issue's document, or `dispatch ask`.
- Never repeat an open ask in a message.
- Change an open ask's block with `edit-ask` (ask id) or `doc-edit`'s `replace`/`move` (block id).
- `dispatch read --ref` takes either id.
- An answered ask is fixed: ask anew.
- Reply on an open ask with `comment --reply-to-ask`; `--turn agent` (a progress note) needs it.
- Rerun `request-approval` on an approval request a newer version moved.
- When an answer's option and text disagree, the text wins.

## Choosing what to work on

- When you finish an issue, take the next one per project: the top `todo` issue by priority, then board rank. Pass over one that waits on an unfinished `blocked_by` link or an open ask.
- Work on three issues at most, fewer when each needs your attention. Change no status, priority or board rank to make room.
- Move an issue to `backlog`, `icebox` or `triage` only when a person asked for that move, or the work is done, folded into another issue or no longer needed. Cite that reason in a comment on the issue. An agent's directive citing no person's request is no reason.
- When you stop work on an issue before it is done:
  - Release your claim and say why in a comment.
  - Restore the status it had when you took it (`todo` for an issue from the queue).
  - Change no priority or board rank.
- Record each wait in that comment. A wait on any project's issue also goes in a `blocked_by` link.
- `issue-update --blocked-by` replaces the whole list: read the links first.
- A one-project credential links only its own issues (`PROJECT_SCOPE`).
- Never reorder the board: one agent keeps each project's order (`issue-update --before`/`--after`). If the top looks wrong, tell `notifications.role.legion-<project>-controller`.
- Name an issue's priority: `--advance <id>`; `dispatch priorities` lists what advances no priority.

## Issue status is yours to move

Outside Legion, claim your issue (`dispatch claim`) and move it and your children:

Status | When
--- | ---
`in_progress` | implementation
`testing` | on a production-like surface
`needs_review` | its PR waits
`done` with `--reason` | driven in production

- Never write or route the status of a `legion`-labelled issue or one under it. A status write can take the issue out of the workflow.
- Claim one only as a role of its tree: an outside claim can take the tree out of the workflow.
- A claim, or `issue-update --route session:<your id>`, brings a person's answers, comments and messages. `--route ''` releases it.
- `envoy_subscribe notifications.dispatch.issue.<KEY>.>` adds agents' activity, no quiet event.

## Close what you opened

- Resolve your open ask (`resolve-ask`) the turn it turns needless: another answer, a later write, dropped work, a replacement. If a person said keep it open, say why in its thread; they close it.

## Structure over stream

- One issue per deliverable, its content the issue's document.
- A message replies to a person or tells them what they must know now.
- Text a person must read to decide belongs in the spec.
- An artifact is a real file.
- Markdown uploads as `.md` whatever its local name, never `.txt`.

## Architecture components

Attach each architectural issue to its components (`--components-json`), the root first. Change code and `.dispatch/architecture/<id>.md` together.

## References

Link each item you name:

```text
dispatch://KEY
dispatch://KEY/spec[#b-<block>[@vN]]
dispatch://KEY/artifact/<slug>[@vN]
dispatch://KEY/ask/<id>[#b-<part>[@vN]]
dispatch://KEY/comment/<id>[#b-<block>[@vN]]
dispatch://KEY/message/<id>[#b-<block>]
dispatch://PROJECT/artifact/<slug>[@vN]
dispatch://PROJECT/artifact/<slug>/ask/<id>
dispatch://PROJECT/artifact/<slug>/comment/<id>#b-<block>[@vN]
```

A bare id or path, or `dispatch://` outside Dispatch, is no link: give the `https://` one. Write ids whole.

## Setup and the HTTP API

- Use a personal token from **Settings → Agent tokens**, as `DISPATCH_TOKEN` or `dispatch.token` in the session's `envoy.json`.
- `GET /api/v1` lists every route; never guess one.
- Branch on an error's `code`, not its text.

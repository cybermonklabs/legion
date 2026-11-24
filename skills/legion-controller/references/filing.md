# The issues you file

Part of `skill://legion-controller`, "Findings, and the issues you file".
Read it when you file or link an issue, and when a session tells you of an issue under a root on the backlog.
A clear defect and a finding ask: `skill://legion-controller/references/finding-asks.md`.

## The `Filed for` line

Every issue you file carries one line in its spec, which triage reads to tell your filing from another's label:

- for a person: `Filed for <person> at their request: <request>`. The request is `dispatch://<REPORT KEY>/ask/<id>` for who answered it; `Slack <channel_id> message <ts>` for the mention's `<@user_id>` (`…, adopting message <earlier ts>` when the mention asks for an earlier message's request); or `direct message <id>`, the id its frame's `reply_with` names after `--in-reply-to` (for a message that came as your own user turn, `direct message to session <your session id>`).
- for a clear defect no running tree owns: `Filed for Legion from <role> on <issue>: <evidence reference>`.

## An issue's approved root

Before you file an issue or give one to a tree, find its approved root: a class root whose class holds it, or the root of a running tree whose unfinished work holds it. A mention of a class in prose is no link: only a Dispatch `child_of` link (`--parent`) is one, and a root counts only once a person approved its spec.

- The root's tree runs: give the issue to the tree (step 3 of "Hand over a clear defect" in `skill://legion-controller/references/finding-asks.md`).
- It is a Legion root whose tree does not run: file the issue with `--parent <ROOT>` and no `legion` label.
- A fix of what a `done` issue delivered is new work: file it with no parent, and it asks for its own approval.
- An issue that belongs under any other unfinished issue is filed as a root of its own with no parent, and it asks for its own approval.
- Link an issue already filed that belongs to an approved root's class or unfinished work: `dispatch issue-update --issue <KEY> --parent <ROOT>`. After linking a root of its own, `envoy_publish` to its architect's role topic the ancestor's key and that `skill://legion-architect/references/view.md` now applies to it.
- Where you report a link, and in your message to the linked issue's architect, name all three parts of the link's cost (an open child holds the parent's first phase, holds each later phase start, and stops a phase under way at the next admission pass), and say that a root whose branch carries code puts that code in a child.

A class root that must take new members is a Legion root with no code of its own, never an issue that holds only a design. File it with `--label legion` and a spec line that says it has no code of its own, also when a request asks for a design-only issue (say why); if the request says to start nothing yet, move it to `backlog` at once.

### A member under a root on the backlog

A member filed under a root on the backlog moves nothing: the session that files it tells you the member and the root, even when the person who moved the root works with that session. A root's architect also tells you when its root rests on an ancestor on the backlog. Decide whether that root runs under "Free slots" (`skill://legion-controller/SKILL.md#free-slots`): with no request naming it, follow the deployment's instructions, leave it on the backlog where they let no session ask, and otherwise ask a person. That ask is one `dispatch ask` per root on the report issue, with the options `Run it now` and `Leave it on the backlog` and a recommendation, followed; a later member goes under the open ask.

On `Run it now`, set the root to `todo` and cite the answer in a comment on it; on `Leave it on the backlog`, move nothing; then reply `Started dispatch://<ROOT>` or `Left on the backlog dispatch://<ROOT>`. Until the root runs, its member waits; a fix that must run now is filed with no parent, asks for its own approval, and its spec names the class root and why it is not linked.

# Finding asks

Read this when a clear defect or a finding ask's reference reaches you on the controller topic,
and when one you follow is answered. Read it also when you file or link an issue, when a session
tells you of an issue under a root on the backlog, and at every start.
What a finding ask is and where it goes is `skill://legion-worker/references/findings.md`; what
you may file is the skill's "Findings, and the issues you file" section.

## The `Filed for` line

Every issue you file for a person carries one line in its spec, `Filed for <person> at their
request: <request>`, because triage reads it to tell your filing from anyone else's label
(`skill://legion-controller/references/triage.md`). The request, and the person it names, are:

- an answer to an ask: `dispatch://<REPORT KEY>/ask/<id>`, for the person who answered it;
- a Slack request: `Slack <channel_id> message <ts>`, for the mention's `<@user_id>`. When the
  mention adopts an earlier message's request by asking for it
  (`skill://legion-controller/references/slack-mentions.md`), the request reads
  `Slack <channel_id> message <mention ts>, adopting message <earlier ts>`, and the person it names
  is still the mention's `<@user_id>`;
- a direct message: `direct message <id>`, with the id its frame's `reply_with` names after
  `--in-reply-to`, for the frame's `actor`. A message that arrives as your own user turn shows no
  id: write `direct message to session <your session id>`, for the person by the name they gave,
  else `the person who messaged this session`.

For a clear defect no running tree owns, the line is `Filed for Legion from <role> on <issue>:
<evidence>` ([Hand over a clear defect](#hand-over-a-clear-defect)).

## Hand over a clear defect

A role sends a clear defect on the controller topic: its role token, the defect, the issue it came
from, where the fix belongs, the fix, and its evidence comment on the report issue, whose first line
is `Clear defect from <role>`. When the report issue was missing or closed, the message carries the
evidence: create or reopen the report issue (`skill://legion-controller/references/daily-report.md`)
and post it there with that first line. A clear defect you find yourself gets the same comment, with
`controller` as the role. A clear-defect ask (`findings.md`, "When no controller takes it") needs
no answer: take it as below, the ask its evidence. Resolve it (`dispatch resolve-ask --kind
resolved`) only once it carries your reply: `Handed` or `Filed` from step 2, 3 or 5, or, when
step 1 finds it is not clear, `Moved dispatch://<REPORT KEY>/ask/<new id>` naming the decision ask
you open. With no reply, leave it open: catch-up step 2 takes it again at your next start.

The work goes to whoever already owns that code. You file an issue only when nobody does.

1. Check that it is clear: the fix needs no choice a person must make, and it belongs in a
   repository your addressing line lists. Otherwise open an ask for it, as `findings.md` says.
2. Search first (`skill://dispatch-first`). When an open issue tracks it, comment the evidence
   there, reply `Filed dispatch://<that KEY>` under the evidence, and file nothing. When that issue
   belongs to an approved root and has no `child_of` link to it, link it as
   [Link an issue to its approved root](#link-an-issue-to-its-approved-root) says.
3. Find the running tree whose code it is: a root in `admission.trees`, the live trees, whose
   repository and component match, read from `read_state` and `dispatch read`. Read it there
   rather than in `admission.active`, which holds the issues the daemon has started: an issue
   holds one of the cap's slots from the moment the daemon starts it until its work is past its
   review, until it is done, or until Dispatch takes it out of the workflow, and a tree runs
   whether or not any issue of it holds one. A running tree also owns a fix that its root's
   approved class or unfinished work holds
   ([Link an issue to its approved root](#link-an-issue-to-its-approved-root)). When one matches,
   hand it over.
   `envoy_publish` the defect, its evidence reference and the fix to that tree's architect topic,
   `notifications.role.<issues.<KEY>.architect.locator.claim>`. The architect decides whether it
   is a child of the tree or part of an open issue's work. Reply `Handed dispatch://<that KEY>`
   under the evidence only when a live holder took the publish. When the root has no locator, or
   the listener kept the publish for the role's next holder, reply nothing: no architect reads a
   comment on its root, so the evidence stays unhandled, and catch-up step 4 lists it again at
   your next start. Publish it again at your next wake. Once the root leaves `admission.trees`,
   no running tree owns the code, and step 4 files it. File nothing while a tree owns it: a
   second issue splits the work.
4. No running tree owns that code: file it on the repository where the fix belongs, in the
   project that repository maps to. Where
   [Link an issue to its approved root](#link-an-issue-to-its-approved-root) puts it under a root,
   write `--parent <ROOT>` in place of `--label legion`. Otherwise file it as a root of its own:

   ```text
   dispatch issue --project <KEY> --repo <owner/name> --title '<the defect, as a title>' --label legion --spec-file -
   ```

   Without `--repo`, Dispatch records the project's default repository, and the daemon admits the
   issue there. The spec states the defect, where the fix belongs and the fix, and ends with the
   `Filed for Legion from` line: the sending role's token (or `controller`), the `dispatch://<KEY>`
   it came from, and the evidence comment's or the ask's reference. A root of its own opens in
   `triage` with the label, and triage admits it (`skill://legion-controller/references/triage.md`).
   A member under a root opens with no label and waits with that root.
5. Reply under the evidence comment (`--reply-to '<comment id>'`) or the ask
   (`--reply-to-ask '<ask id>'`, on the ask's issue), its first line exactly
   `Filed dispatch://<NEW KEY>` or `Handed dispatch://<KEY>`. A later session reads that reply at
   triage and at every start.

## Link an issue to its approved root

A class root is an issue whose spec states a class of work, so that each issue of that class is
its child. dispatch://INFRA-3118 is one: each fix of the daemon's task delivery, holds and pod
updates is its child. Before you file an issue, or give one to a tree, find the root it belongs
to. That is a class root whose class holds it, or the root of a running tree whose unfinished work
holds it. Such a root counts only when a person approved its spec. A mention of the class in prose
is not a link. Only a Dispatch `child_of` link is one.

- **The root's tree runs** (the root is in `admission.trees`): give the issue to that tree, as
  step 3 of [Hand over a clear defect](#hand-over-a-clear-defect) says. Its architect makes it a
  child of the tree or part of an open issue's work. A child of a running tree needs no gate of
  its own.
- **The root is a Legion root, and its tree does not run:** file the issue under it, with
  `--parent <ROOT>` and no `legion` label. It waits with the root, and it runs as a child once the
  root runs. A labelled child outside a live tree is admitted as a root of its own
  (`skill://legion-controller/references/triage.md#label-an-issue-for-a-person`). For a root on
  the backlog, also read [A member under a root on the backlog](#a-member-under-a-root-on-the-backlog).
- **The issue fixes what a `done` issue delivered:** it is new work. File it with no parent, and
  not under the `done` issue for that issue's approval: `register_gate` refuses a `done` or
  accepted ancestor. The new issue asks for its own approval
  (`skill://legion-architect/references/design-gate.md#when-the-root-asks-for-its-own-approval`).
- **It belongs under any other unfinished issue:** file it as a root of its own with no parent,
  and it asks for its own approval. Your deployment's intake takes no issue under an ancestor that
  is not `done`, so a child of such an issue never runs.

A class root that must take new members is a Legion root with no code of its own
(`skill://legion-architect/references/decomposition.md#a-root-with-no-code-of-its-own`), as
dispatch://INFRA-3118 and dispatch://INFRA-3418 are, and not an issue that holds only a design.
Your deployment's intake takes no issue under an ancestor that is not `done`, so a member under an
issue that holds only a design never runs. So file such a class root with `--label legion`, and
write in its spec that it has no code of its own. Do this also when a request or a proposal asks
for an issue that holds only a design, and say why in its spec and in your reply. A request for a
class root whose members Legion runs asks Legion to run that root.

An issue already filed that belongs to an approved root, with no `child_of` link to it, goes where
a new issue goes, by the list above. Where it runs as a root of its own (`issues.<KEY>.tree` is
`<KEY>` in `read_state`) and the list puts it under that root or in its tree, set the link
yourself:

```text
dispatch issue-update --issue <KEY> --parent <ROOT>
```

It stays a tree of its own. Then tell its architect, with `envoy_publish` to
`notifications.role.<issues.<KEY>.architect.locator.claim>`, the root's key and that the rule of a
view applies to it:

```text
<KEY>: Dispatch now shows it child_of <ROOT>. skill://legion-architect/references/design-gate.md, "A root that is a view on an approved ancestor", now applies to it.
```

A link has a cost. An open child under a parent holds the parent's first phase. It also holds
every later phase start of the parent, and it stops a phase of the parent under way at the next
admission pass. A class root with no code of its own loses nothing by it. Where you report the
link (your reply under the evidence, or your summary), name that cost for the root you link under.

### A member under a root on the backlog

A member under a Legion root on the backlog does not move that root. You file such a member
yourself, or the session that filed it tells you on the controller topic. A root's architect also
tells you when its root rests on an ancestor on the backlog
(`skill://legion-architect/references/design-gate.md#a-requirement-the-ancestor-does-not-state`).
Decide whether that root runs again under [Free slots](skill://legion-controller/SKILL.md#free-slots):
it leaves the backlog only on a request or a plan directive that names it. Where none names it,
do as your deployment's instructions say. Where they give you no rule for this move, ask a person:

1. Open a `dispatch ask` on the report issue that names the root and the member, with the options
   `Run it now` and `Leave it parked` and your recommendation. Follow it
   (`dispatch follow --ask '<ref>' --action follow`). While an open ask on the report issue already
   asks this for the same root (`dispatch open-asks --project <PROJECT>` lists them), open no
   second one. Add the new member under that ask instead.
2. On `Run it now`, set the root to `todo` (`set_status`), and cite the answer in a comment on the
   root: the answer is a person's request that names it. On `Leave it parked`, move nothing.
3. Reply under the ask, its first line exactly `Started dispatch://<ROOT>` or
   `Left parked dispatch://<ROOT>`.

Until the root runs, the member waits with it. A fix that must run now does not wait. File it with
no parent, and it asks for its own approval. Its spec names the class root and says why the fix is
not linked to it.

## Follow it

A finding ask another role opened reaches you as its reference, `dispatch://<KEY>/ask/<id>`, on
the controller topic. Follow it, so that its answer reaches you:

```text
dispatch follow --ask 'dispatch://<KEY>/ask/<id>' --action follow
```

When `dispatch read --ref` shows it answered already, act on its answer now.

## Act on its answer

A park ask (`Park it`, `Leave it admitted`), a deferral ask (`Defer it`, `Let it run`) and a run
ask (`Run it now`, `Leave it parked`) take none of the four finding actions below. Act on the
answer to a park ask as `skill://legion-controller/references/wakes.md#ask-whether-to-park-a-tree`
step 4 says, on the answer to a deferral ask as
`skill://legion-controller/references/backlog-stewardship.md#a-root-the-plan-defers` step 4 says,
and on the answer to a run ask as
[A member under a root on the backlog](#a-member-under-a-root-on-the-backlog) says.

Once it is answered (an ask you follow brings its answer to you), read it with
`dispatch read --ref 'dispatch://<KEY>/ask/<id>'` and act on the newest answer. When its typed
text asks for a different one of the four actions than the option selected, do what the text
asks; when the text asks for something that is none of the four, reply under the ask with your
question and file nothing:

- `Hand it to the owning tree`: take it as [Hand over a clear defect](#hand-over-a-clear-defect)
  says, from its step 3.
- `Do not file`: file nothing.
- `File an issue`: `dispatch issue --project <PROJECT> --title '<the problem, as a title>'
  --spec-file -` in the project where the fix belongs, as the answer or the ask names it; when
  neither names one, in this project. The spec
  states the problem and where the fix belongs, cites the ask and its evidence by their
  `dispatch://` references, and carries the line `Filed for <the answer's By:> at their request:
  dispatch://<REPORT KEY>/ask/<id>` (above). No `legion` label.
- `File it for Legion`: the same, with `--label legion`. It opens in `triage` with the label, so
  its triage wake brings it back to you (`skill://legion-controller/references/triage.md`).

Each issue you file follows
[Link an issue to its approved root](#link-an-issue-to-its-approved-root). One that goes under its
approved root takes `--parent <ROOT>` and no `legion` label, also on `File it for Legion`.

An ask that re-files an escalated child also takes the steps of
`skill://legion-controller/references/escalation.md`.

Then reply under the ask with what you did, its first line exactly `Filed dispatch://<NEW KEY>`,
`Handed dispatch://<KEY>` or
`Not filed: <reason>`. The ask's issue is the report issue, or the role's own issue for an ask no
controller took:

```text
dispatch comment --issue <the ask's issue> --reply-to-ask '<ask id>' --body-file -
```

Dispatch refuses to resolve an answered ask, so that reply is its record. An open finding ask that
turns moot (an issue now tracks the problem, or the problem is gone) you close with
`dispatch resolve-ask --kind resolved`, naming why.

## Catch up at every start

An answer that arrives while no controller runs never wakes you, a reference published then
reaches nobody, and a new session follows none of the asks an earlier one followed. So at every
start, catch up on the report issue (with none yet, there is nothing to catch up), and on the
finding asks roles opened on their own issues while no controller ran:

1. Read it with `dispatch read`. Reopen it when its `Status:` is `done`, and assign it when its
   `Assignee:` says `unassigned` and a person is known, both as
   `skill://legion-controller/references/daily-report.md` "Keep it answerable" says.
2. Take each open ask: `dispatch open-asks --project <PROJECT>` lists every unanswered ask of the
   project, with no cap, each line naming its issue. It leaves out an ask on a closed issue
   (`asks.go`'s `listOpenAsks` takes `i.closed_at is null`), which is why an architect holds its
   issue open for one. Follow those on the report issue, and take each whose question opens
   `Clear defect from ` as [Hand over a clear defect](#hand-over-a-clear-defect) says. One on
   another issue whose question opens `Finding from ` or `Clear defect from ` is a role's
   own-issue ask. Take a clear defect the same way. Move a finding that needs a
   decision: ask it again on the report issue with the same question, options and `--ref`, then
   reply `Moved dispatch://<REPORT KEY>/ask/<new id>` under the old ask. Resolve the old ask once it
   carries a `Moved`, `Handed` or `Filed` reply. A clear defect no holder took stays open, and the
   next start takes it again.
3. List the answered asks still waiting on you, which no `dispatch` command lists:

   ```text
   asks=$(curl -sS --fail-with-body -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<ISSUE KEY>/asks?state=answered&thread=1") || { echo "Dispatch read failed" >&2; exit 1; }
   printf '%s' "$asks" | jq -cn 'input | if type == "array" then [.[] | select(any(.options[]; .label == "Do not file" or .label == "Admit it" or .label == "Park it" or .label == "Leave it admitted" or .label == "Defer it" or .label == "Let it run" or .label == "Run it now" or .label == "Leave it parked")) | .answer.at as $t | select([.thread.replies[]? | select(.author.kind == "session" and .created_at > $t and (.body | test("^(Filed dispatch://|Handed dispatch://|Admitted dispatch://|Not filed:|Not admitted:|Parked dispatch://|Not parked dispatch://|Left admitted dispatch://|Deferred dispatch://|Not deferred dispatch://|Let run dispatch://|Started dispatch://|Left parked dispatch://)")))] | length == 0) | {id, question, answer}] else error("Dispatch did not answer the list") end'
   ```

   Read it for the report issue, then for each issue in `needs_review`, whose architect holds its
   acceptance on a finding ask's answer. Act on each as above, a triage ask as
   `skill://legion-controller/references/triage.md` says. The read lands in `asks` first, because a
   pipeline reports only the status of `jq`. `jq -n 'input …'` fails on an empty body, where a
   plain `jq` would succeed on it, so exit 0 means the list was read: `[]` is nothing waiting.
   Anything else is a failed read and admits nothing.

   Take the `needs_review` issues as one snapshot before you act on any of them:
   `dispatch issues --project <PROJECT> --status needs_review --all`. `--all` reads every match in
   one request. Never page this list: pages by `offset` skip or repeat an issue whose status
   changes between reads, and your reply moves issues out of `needs_review`. `No issues in
   <PROJECT>.` means nothing waits, and a command that fails admits nothing. An issue that enters
   `needs_review` after the snapshot is read at your next start.
4. List the clear defects nobody took yet: each evidence comment whose first line starts
   `Clear defect from `, with no session reply under it that starts `Handed dispatch://`,
   `Filed dispatch://`, `Not filed:` or `Asked dispatch://` (a role that asked instead, which step
   2 takes):

   ```text
   notes=$(curl -sS --fail-with-body -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<REPORT KEY>/comments") || { echo "Dispatch read failed" >&2; exit 1; }
   printf '%s' "$notes" | jq -cn 'input | if type == "array" then (. as $all | [.[] | select(.reply_to == null and (.body | startswith("Clear defect from "))) | .id as $id | select([$all[] | select(.reply_to == $id and .author.kind == "session" and (.body | test("^(Handed dispatch://|Filed dispatch://|Not filed:|Asked dispatch://)")))] | length == 0) | {id, body}]) else error("Dispatch did not answer the list") end'
   ```

   It reads as step 3's read does: exit 0 with `[]` is nothing waiting, anything else failed.
   Take each as [Hand over a clear defect](#hand-over-a-clear-defect) says. One you could not hand
   over at an earlier wake, because no architect held its tree's topic, has no reply, so this list
   brings it back until a tree takes it or step 4 of the hand-over files it. When one is not clear,
   reply `Not filed: <reason>` under it and open an ask for it instead.

A read that fails, and an assignment Dispatch refuses, are handled as
`skill://legion-controller/SKILL.md#rules` and
`skill://legion-controller/references/daily-report.md` say.

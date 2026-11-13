# Finding asks

Read this when a clear defect or a finding ask's reference reaches you on the controller topic,
when one you follow is answered, when you file an issue a person asked for, and at every start.
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

For a clear defect no running tree owns, and for a test a role re-ran green ([A test a role re-ran
green](#a-test-a-role-re-ran-green)), the line is `Filed for Legion from <role> on <issue>:
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
   there, reply `Filed dispatch://<that KEY>` under the evidence, and file nothing.
3. Find the running tree whose code it is: a root in `admission.trees`, the live trees, whose
   repository and component match, read from `read_state` and `dispatch read`. Read it there
   rather than in `admission.active`, which holds the issues the daemon has started: an issue
   takes its slot when the daemon starts it and keeps it
   until it is done or leaves the workflow, and a tree runs whether or
   not any issue of it holds one. When one matches, hand it over.
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
   project that repository maps to:

   ```text
   dispatch issue --project <KEY> --repo <owner/name> --title '<the defect, as a title>' --label legion --spec-file -
   ```

   Without `--repo`, Dispatch records the project's default repository, and the daemon admits the
   issue there. The spec states the defect, where the fix belongs and the fix, and ends with the
   `Filed for Legion from` line: the sending role's token (or `controller`), the `dispatch://<KEY>`
   it came from, and the evidence comment's or the ask's reference. It opens in `triage` with the
   label, and triage admits it (`skill://legion-controller/references/triage.md`).
5. Reply under the evidence comment (`--reply-to '<comment id>'`) or the ask
   (`--reply-to-ask '<ask id>'`, on the ask's issue), its first line exactly
   `Filed dispatch://<NEW KEY>` or `Handed dispatch://<KEY>`. A later session reads that reply at
   triage and at every start.

## A test a role re-ran green

The role that holds a phase re-runs a red check once at the head of its pull request
(`skill://legion-worker/references/red-at-the-head.md`). A green re-run means the round goes on,
and the failure is one occurrence of a test that fails at random. The role posts that occurrence
as a `dispatch comment` on the issue that tracks the test, whatever that issue's status, unless
it reads `done`, and tells
you on the controller topic. Its message names the test, the job, the head and both results, the
failing run and the green re-run. You act on that message when the role found no issue for the
test, or found one that reads `done`.

You file this one, and no person asks you for it. It is a named exception to the rule in the
skill's "Findings, and the issues you file": a test that fails for a cause outside the change
belongs to no tree's work, and the evidence is already measured, so a person has nothing to
decide here. The census is one issue per test with its occurrences counted on it, never one issue
per occurrence. An issue per test has an owner and a fix, and the count of its occurrences is what
ranks it.

1. Search first (`skill://dispatch-first`): `dispatch search --query '<the test's full name>'`.
2. An open issue tracks that test: comment the occurrence there, with the test, the job, the head,
   the date and both results. File nothing.

   ```text
   dispatch comment --issue <that KEY> --body-file -
   ```

3. No issue tracks that test: file one against it.

   ```text
   dispatch issue --project <KEY> --repo <owner/name> --title '<the test's full name> fails at random in CI' --label legion --spec-file -
   ```

   The spec holds the test's full name, the job, the head, the failing run and the green re-run.
   It says in one line that the test was re-run green and the round went on, so that no reader
   takes the filing for a blocked merge. It ends with the `Filed for Legion from <role> on
   <issue>:` line ([The `Filed for` line](#the-filed-for-line)), its evidence the role's
   occurrence comment or its message. It opens in `triage` with the label, and triage admits it
   (`skill://legion-controller/references/triage.md`).
4. The issue that tracks the test reads `done`: read it, and take the branch that fits. You decide
   this and the role does not, because a session inside a red run cannot read whether the old fix
   is still in the code.
   - Its fix is still in place and the same test failed again: reopen it with `set_status` to
     `todo`, which admits a labelled root again, and comment the occurrence there.
   - It shipped a different fix, or this failure is another mechanism in the same test: file a new
     issue as step 3 says, and cite the closed one in its spec as `dispatch://<that KEY>`.

   dispatch://INFRA-1137 and dispatch://INFRA-1260 are the first two issues of this census, and
   both read `Status: done` today. A new occurrence of one of their tests takes this step.

Name in your summary the issue the occurrence went to.

## Follow it

A finding ask another role opened reaches you as its reference, `dispatch://<KEY>/ask/<id>`, on
the controller topic. Follow it, so that its answer reaches you:

```text
dispatch follow --ask 'dispatch://<KEY>/ask/<id>' --action follow
```

When `dispatch read --ref` shows it answered already, act on its answer now.

## Act on its answer

A park ask (`Park it`, `Leave it admitted`) and a deferral ask (`Defer it`, `Let it run`) take
none of the four finding actions below. Act on the answer to a park ask as
`skill://legion-controller/references/wakes.md#ask-whether-to-park-a-tree` step 4 says, and on
the answer to a deferral ask as
`skill://legion-controller/references/backlog-stewardship.md#a-root-the-plan-defers` step 4 says.

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
   printf '%s' "$asks" | jq -cn 'input | if type == "array" then [.[] | select(any(.options[]; .label == "Do not file" or .label == "Admit it" or .label == "Park it" or .label == "Leave it admitted" or .label == "Defer it" or .label == "Let it run")) | .answer.at as $t | select([.thread.replies[]? | select(.author.kind == "session" and .created_at > $t and (.body | test("^(Filed dispatch://|Handed dispatch://|Admitted dispatch://|Not filed:|Not admitted:|Parked dispatch://|Not parked dispatch://|Left admitted dispatch://|Deferred dispatch://|Not deferred dispatch://|Let run dispatch://)")))] | length == 0) | {id, question, answer}] else error("Dispatch did not answer the list") end'
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

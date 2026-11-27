# Finding asks

Read this for a clear defect, a finding ask's reference, an answer on an ask you follow, an issue a person asks you to file, and at every start.

## The `Filed for` line

Every issue you file carries one line in its spec, which triage reads to tell your filing from another's label:

- for a person: `Filed for <person> at their request: <request>`. The request is `dispatch://<ASK KEY>/ask/<id>` for who answered it; `Slack <channel_id> message <ts>` for the mention's `<@user_id>` (`…, adopting message <earlier ts>` when the mention asks for an earlier message's request); or `direct message <id>`, the id its frame's `reply_with` names after `--in-reply-to` (for a message that came as your own user turn, `direct message to session <your session id>`).
- for a clear defect: `Filed for Legion from <role> on <issue>: <evidence reference>`.

## File a clear defect

A role writes a clear defect to you: a comment that mentions your role, on the report issue or on its own issue, first line `Clear defect from <role>`, whose token is the finder's claim token. Reply to that claim token, not to one spelled from the role: two claims of one issue can hold one role, and `dispatch steps --issue <KEY>` gives the agent of each run. For a clear defect you find yourself, post its evidence on the report issue with `controller` as the role.
A defect that stops its sender's work, in a repository the sender's token reaches, is the sender's to fix, unless an issue that tracks it is claimed, `in_progress` and its holder said on Envoy that it works on it now: reply `Not filed: it stops your work, so it is yours to fix`, and take none of the steps.
File any other only when it is clear, in a repository both role GitHub Apps are installed on, no open issue tracks it (an open issue that no live tree owns gets the evidence as a comment, and the reply `Filed dispatch://<KEY>`), and no live tree's open pull request changes the files the fix touches. Otherwise file nothing and reply under its evidence, mentioning the finder: `Not filed:` with a mention of the live tree's architect, or with the reason the finder opens its ask.
The reply's form and the five steps are in `skill://legion-controller/references/finding-asks-steps.md#file-a-clear-defect`: read them before every such defect.

## Follow it

A finding ask another role opened reaches you as `dispatch://<KEY>/ask/<id>`. Follow it, so its answer reaches you (`dispatch follow --ask '<that reference>' --action follow`); when `dispatch read --ref` shows it answered already, act on its answer now.

## Act on its answer

A park ask's answer goes to `skill://legion-controller/references/park-asks.md` step 4, a deferral ask's to `skill://legion-controller/references/backlog-stewardship.md#a-root-the-plan-defers` step 4. For any other, `dispatch read --ref` it and act on the newest answer; when its typed text asks for another of these three than the option selected, do what the text asks, and when it asks for none of them, reply with your question and file nothing:

- `Do not file`: file nothing.
- `File an issue`: `dispatch issue --project <PROJECT> --title '<the problem>' --spec-file -` in the project where the fix belongs (this one when the answer names none). The spec states the problem and where the fix belongs, cites the ask and its evidence, and carries `Filed for <the answer's By:> at their request: dispatch://<ASK KEY>/ask/<id>`.
- `File it for Legion`: the same, with `--label legion`; its triage wake brings it back.

An ask that re-files an escalated child also takes the steps of `skill://legion-controller/references/escalation.md#a-child-that-looks-independent`.

Then reply under the ask, first line exactly `Filed dispatch://<NEW KEY>` or `Not filed: <reason>`: `dispatch comment --issue <the ask's issue> --reply-to-ask '<ask id>' --body-file -`. Dispatch cannot resolve an answered ask, so that reply is its record. Close an open finding ask that turned moot with `dispatch resolve-ask --kind retracted`, naming why.

## Catch up at every start

Nothing wakes on or follows an ask while no controller runs, so at every start:

1. `dispatch read` the report issue (none yet: skip step 4), and reopen it as `skill://legion-controller/references/daily-report.md` "When" says.
2. `dispatch open-asks --project <PROJECT>` lists every open ask; it lists no ask on a closed issue. Follow each whose question opens `Finding from `, and each whose options hold a label step 3's filter names.
3. List the answered asks still waiting on you, on the report issue, each issue `read_state` records, and each issue of one snapshot of `dispatch issues --project <PROJECT> --status triage --all` and of `dispatch issues --project <PROJECT> --status needs_review --all` (never paged):

   ```text
   asks=$(dispatch api --path '/api/v1/issues/<ISSUE KEY>/asks?state=answered&thread=1') || { printf 'Dispatch read failed: %s\n' "$asks" >&2; exit 1; }
   printf '%s' "$asks" | jq -cn 'input | if type == "array" then [.[] | select(any(.options[]; .label == "Do not file" or .label == "Admit it" or .label == "Park it" or .label == "Leave it admitted" or .label == "Defer it" or .label == "Let it run")) | .answer.at as $t | select([.thread.replies[]? | select(.author.kind == "session" and .created_at > $t and (.body | test("^(Filed dispatch://|Handed dispatch://|Admitted dispatch://|Not filed:|Not admitted:|Parked dispatch://|Not parked dispatch://|Left admitted dispatch://|Deferred dispatch://|Not deferred dispatch://|Let run dispatch://)")))] | length == 0) | {id, question, answer}] else error("Dispatch did not answer the list") end'
   ```

   Exit 0 with `[]` is nothing waiting; anything else is a failed read. Act on each as above, a triage ask as `skill://legion-controller/references/triage.md` says, and an answer whose label starts `Hand it` as step 5 says.
4. List the clear defects nobody took yet:

   ```text
   notes=$(dispatch api --path '/api/v1/issues/<REPORT KEY>/comments') || { printf 'Dispatch read failed: %s\n' "$notes" >&2; exit 1; }
   printf '%s' "$notes" | jq -cn 'input | if type == "array" then (. as $all | [.[] | select(.reply_to == null and (.body | startswith("Clear defect from "))) | .id as $id | select([$all[] | select(.reply_to == $id and .author.kind == "session" and (.body | test("^(Handed dispatch://|Filed dispatch://|Not filed:|Asked dispatch://|Moved dispatch://)")))] | length == 0) | {id, body}]) else error("Dispatch did not answer the list") end'
   ```

   Take each as [File a clear defect](#file-a-clear-defect) says.
5. Take each ask earlier rules opened whose question opens `Clear defect from ` the same way, with `--reply-to-ask '<ask id>'` for `--reply-to`: an open one (step 2 lists it), resolved once it has your reply, and one answered with a label that starts `Hand it`, a person's request: where step 2 or 3 of *File a clear defect* finds a live tree, reply `Not filed: dispatch://<KEY>; role:<token> is mentioned here` (`<KEY>` the issue or root), adding `--mention 'role:<its architect's token>'`: it reads the answer and evidence here; copy neither. If step 2's issue is in no tree, reply `Not filed: dispatch://<KEY> tracks it` to its person, mentioning no finder.

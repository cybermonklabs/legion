# Finding asks

Read this when a clear defect or a finding ask's reference reaches you, when an ask you follow is answered, when you file an issue a person asked for, and at every start. What a finding ask is: `skill://legion-worker/references/findings.md`.

## The `Filed for` line

Every issue you file carries one line in its spec, which triage reads to tell your filing from another's label:

- for a person: `Filed for <person> at their request: <request>`. The request is `dispatch://<ASK KEY>/ask/<id>` for who answered it; `Slack <channel_id> message <ts>` for the mention's `<@user_id>` (`…, adopting message <earlier ts>` when the mention asks for an earlier message's request); or `direct message <id>`, the id its frame's `reply_with` names after `--in-reply-to` (for a message that came as your own user turn, `direct message to session <your session id>`).
- for a clear defect: `Filed for Legion from <role> on <issue>: <evidence reference>`.

## Hand over a clear defect

A role sends a clear defect with its role token, the defect, its issue, where the fix belongs, the fix, and its evidence comment on the report issue, whose first line is `Clear defect from <role>`. When the message carries the evidence itself, find or create the report issue (`skill://legion-controller/references/daily-report.md`) and post it there with that first line. One you find yourself gets the same comment, with `controller` as the role. A clear-defect ask needs no answer: take it as below, and resolve it (`dispatch resolve-ask --kind resolved`) only once it carries your reply.

1. Check it is clear: the fix needs no choice a person must make, in a repository of an owner your addressing line names. Otherwise open a decision ask as `findings.md` says, and reply `Moved dispatch://<ASK KEY>/ask/<new id>`.
2. A defect that stops its sender's work is the sender's to fix, wherever the fix belongs, unless an issue that tracks it is claimed, `in_progress` and its holder said on Envoy that it works on it now (`findings.md`, "Search first"): reply `Not filed: it stops your work, so it is yours to fix` with `--mention-role <the sender's role token>`.
3. Search first (`skill://dispatch-first`). When an open issue tracks it, comment the evidence there and reply `Filed dispatch://<that KEY>`.
4. When the open pull request of a root in `admission.trees` changes the files the fix touches, it can carry the fix: `envoy_publish` the defect, its evidence reference and the fix to `notifications.role.<issues.<KEY>.architect.locator.claim>`, and reply `Handed dispatch://<that KEY>` once a live holder took it. When Envoy kept the message, or the root has no locator, go on to step 5; after a keep, also publish the new issue's link to that topic.
5. Otherwise file it where the fix belongs:
   `dispatch issue --project <KEY> --repo <owner/name> --title '<the defect>' --label legion --spec-file -`
   The spec states the defect, where the fix belongs and the fix, and ends with the `Filed for Legion from` line.
6. Reply under the evidence comment (`--reply-to '<comment id>'`) or the ask (`--reply-to-ask '<ask id>'`), its first line exactly `Filed dispatch://<NEW KEY>` or `Handed dispatch://<KEY>`.

## Follow it

A finding ask another role opened reaches you as `dispatch://<KEY>/ask/<id>`. Follow it so that its answer reaches you: `dispatch follow --ask 'dispatch://<KEY>/ask/<id>' --action follow`. When `dispatch read --ref` shows it answered already, act on its answer now.

## Act on its answer

A park ask's answer goes to `skill://legion-controller/references/park-asks.md` step 4, a deferral ask's to `skill://legion-controller/references/backlog-stewardship.md#a-root-the-plan-defers` step 4. For any other, `dispatch read --ref` it and act on the newest answer; when its typed text asks for another of these four than the option selected, do what the text asks, and when it asks for none of them, reply with your question and file nothing:

- An option whose label starts `Hand it`: [Hand over a clear defect](#hand-over-a-clear-defect) from step 4.
- `Do not file`: file nothing.
- `File an issue`: `dispatch issue --project <PROJECT> --title '<the problem>' --spec-file -` in the project where the fix belongs (this one when the answer names none). The spec states the problem and where the fix belongs, cites the ask and its evidence, and carries `Filed for <the answer's By:> at their request: dispatch://<ASK KEY>/ask/<id>`.
- `File it for Legion`: the same, with `--label legion`; its triage wake brings it back.

An ask that re-files an escalated child also takes the steps of `skill://legion-controller/references/escalation.md#a-child-that-looks-independent`.

Then reply under the ask, first line exactly `Filed dispatch://<NEW KEY>`, `Handed dispatch://<KEY>` or `Not filed: <reason>`: `dispatch comment --issue <the ask's issue> --reply-to-ask '<ask id>' --body-file -`. Dispatch cannot resolve an answered ask, so that reply is its record. Close an open finding ask that turned moot with `dispatch resolve-ask --kind retracted`, naming why.

## Catch up at every start

An answer that arrives while no controller runs never wakes you, a reference published then reaches nobody, and a new session follows none of the asks an earlier one followed. So at every start:

1. `dispatch open-asks --project <PROJECT>` lists every open ask; an ask on a closed issue is not listed. Follow each whose question opens `Finding from ` or `Clear defect from `, and each whose options hold a label step 2's filter names (`Do not file`, `Admit it`, `Park it`, `Leave it admitted`, `Defer it`, `Let it run`); take each clear defect as above.
2. List the answered asks still waiting on you, on the report issue, each issue `read_state` records, and each issue of one snapshot of `dispatch issues --project <PROJECT> --status triage --all` and of `dispatch issues --project <PROJECT> --status needs_review --all` (never paged):

   ```text
   asks=$(dispatch api --path '/api/v1/issues/<ISSUE KEY>/asks?state=answered&thread=1') || { printf 'Dispatch read failed: %s\n' "$asks" >&2; exit 1; }
   printf '%s' "$asks" | jq -cn 'input | if type == "array" then [.[] | select(any(.options[]; .label == "Do not file" or .label == "Admit it" or .label == "Park it" or .label == "Leave it admitted" or .label == "Defer it" or .label == "Let it run")) | .answer.at as $t | select([.thread.replies[]? | select(.author.kind == "session" and .created_at > $t and (.body | test("^(Filed dispatch://|Handed dispatch://|Admitted dispatch://|Not filed:|Not admitted:|Parked dispatch://|Not parked dispatch://|Left admitted dispatch://|Deferred dispatch://|Not deferred dispatch://|Let run dispatch://)")))] | length == 0) | {id, question, answer}] else error("Dispatch did not answer the list") end'
   ```

   Exit 0 with `[]` is nothing waiting; anything else is a failed read. Act on each as above, a triage ask as `skill://legion-controller/references/triage.md` says.
3. List the clear defects nobody took yet:

   ```text
   notes=$(dispatch api --path '/api/v1/issues/<REPORT KEY>/comments') || { printf 'Dispatch read failed: %s\n' "$notes" >&2; exit 1; }
   printf '%s' "$notes" | jq -cn 'input | if type == "array" then (. as $all | [.[] | select(.reply_to == null and (.body | startswith("Clear defect from "))) | .id as $id | select([$all[] | select(.reply_to == $id and .author.kind == "session" and (.body | test("^(Handed dispatch://|Filed dispatch://|Not filed:|Asked dispatch://|Moved dispatch://)")))] | length == 0) | {id, body}]) else error("Dispatch did not answer the list") end'
   ```

   Take each as [Hand over a clear defect](#hand-over-a-clear-defect) says; reply `Not filed: <reason>` under one that is not clear, and open an ask for it instead.

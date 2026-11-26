# Finding asks

The report issue (`skill://legion-controller/references/daily-report.md`) holds the finding asks and the evidence of clear defects.

## The `Filed for` line

Every issue you file carries one line in its spec. Triage reads it to tell your filing from another session's label.

- For a person: `Filed for <person> at their request: <request>`. The request is `dispatch://<REPORT KEY>/ask/<id>` for the person who answered that ask, or `Slack <channel_id> message <ts>` for the mention's `<@user_id>` (add `, adopting message <earlier ts>` when the mention asks for an earlier message's request), or `direct message <id>`, the id that its frame's `reply_with` names after `--in-reply-to` (for a message that came as your own user turn, `direct message to session <your session id>`).
- For a clear defect that no running tree owns: `Filed for Legion from <role> on <issue>: <evidence reference>`.

## The record lines

Your reply under an ask or an evidence comment begins with exactly one of these lines. The catch-up reads them.

- `Filed dispatch://<KEY>`
- `Handed dispatch://<KEY>`
- `Not filed: <reason>`
- `Asked dispatch://<REPORT KEY>/ask/<id>`
- `Admitted dispatch://<KEY>` or `Not admitted: <reason>`
- `Parked dispatch://<root>`, `Not parked dispatch://<root>` or `Left admitted dispatch://<root>`

Dispatch cannot resolve an answered ask, so that reply is its record. Close an open finding ask that turned moot with `dispatch resolve-ask --kind retracted`, and say why.

## Open a finding ask

Open it on the report issue with `dispatch ask`, and follow it. Its question begins `Finding from controller`. Then it states what went wrong, where and to whom, the evidence or its reference, where the fix belongs, the decision a person must make, and your recommendation. Give the options `File an issue`, `File it for Legion` and `Do not file`.

## Hand over a clear defect

A role sends a clear defect with its role token, the defect, its issue, where the fix belongs, the fix, and its evidence. When the evidence is not yet a comment on the report issue, post it there with the first line `Clear defect from <role>`. One that you find yourself gets the same comment, with `controller` as the role. A clear-defect ask needs no answer. Take it as below, and resolve it (`dispatch resolve-ask --kind resolved`) once it carries your reply.

1. Check that it is clear: the fix needs no choice that a person must make, in a repository of an owner that your addressing line names. Otherwise open a finding ask, and reply with its `Asked` line.
2. Search first (`dispatch search`). When an open issue tracks the defect, comment the evidence there, and reply `Filed dispatch://<that KEY>`.
3. When a root in `admission.trees` matches its repository and component, `envoy_publish` the defect, its evidence reference and the fix to `notifications.role.<issues.<KEY>.architect.locator.claim>`. Reply `Handed dispatch://<that KEY>` only when a live holder took the publish. When Envoy only kept it, or the root has no locator, reply nothing and publish again at your next wake. File nothing while a tree owns the code.
4. When no running tree owns it, file it where the fix belongs: `dispatch issue --project <KEY> --repo <owner/name> --title '<the defect>' --label legion --spec-file -`. The spec states the defect, where the fix belongs and the fix, and ends with the `Filed for Legion from` line.
5. Reply under the evidence comment (`--reply-to '<comment id>'`), or under the clear-defect ask (`--reply-to-ask '<ask id>'`), with the record line.

## Follow it

A finding ask that another role opened reaches you as `dispatch://<KEY>/ask/<id>`. Follow it so that its answer reaches you: `dispatch follow --ask 'dispatch://<KEY>/ask/<id>' --action follow`. When `dispatch read --ref` shows it answered already, act on its answer now.

## Act on its answer

A park ask's answer goes to step 4 of `skill://legion-controller/references/park-asks.md`. For any other, `dispatch read --ref` the ask and act on the newest answer. When its typed text asks for another of these options than the one selected, do what the text asks. When the text asks for none of them, reply with your question and file nothing.

- `Hand it to the owning tree`: [Hand over a clear defect](#hand-over-a-clear-defect), from step 3.
- `Do not file`: file nothing.
- `File an issue`: `dispatch issue --project <PROJECT> --title '<the problem>' --spec-file -`, in the project where the fix belongs (this one when the answer names none). The spec states the problem and where the fix belongs, cites the ask and its evidence, and carries `Filed for <the answer's By:> at their request: dispatch://<REPORT KEY>/ask/<id>`.
- `File it for Legion`: the same, with `--label legion`. Its triage wake brings it back.

An ask that re-files an escalated child also takes the steps of `skill://legion-controller/references/escalation.md#a-child-that-looks-independent`. Then reply under the ask with the record line.

## Catch up at every start

An answer that arrives while no controller runs never wakes you, a reference published then reaches nobody, and a new session follows none of the asks an earlier one followed. So at every start:

1. `dispatch read` the report issue. When none exists, there is nothing to catch up. Keep it answerable (`skill://legion-controller/references/daily-report.md#keep-it-answerable`).
2. `dispatch open-asks --project <PROJECT>` lists every open ask. Follow each on the report issue. Take each whose question begins `Clear defect from ` as [Hand over a clear defect](#hand-over-a-clear-defect) says.
3. Find the answered asks still waiting on you, on the report issue and on each issue of `dispatch issues --project <PROJECT> --status needs_review --all`: `dispatch api --path '/api/v1/issues/<KEY>/asks?state=answered&thread=1'`. An ask waits on you when one of its options is `Do not file`, `Admit it` or `Park it`, and no session reply newer than its answer begins with a record line. Act on each as above, and on a triage ask as `skill://legion-controller/references/triage.md` says.
4. Find the clear defects that nobody took: each top-level comment of `dispatch api --path '/api/v1/issues/<REPORT KEY>/comments'` whose body begins `Clear defect from ` and that has no session reply beginning with a record line. Take each as [Hand over a clear defect](#hand-over-a-clear-defect) says.

A read that fails or prints no valid JSON decides nothing. Name its first line in your summary.

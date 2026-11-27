# Finding asks

A role sends you a finding with `envoy_publish`. Its first line is `Clear defect from <role>` or `Finding from <role>`, and it names the issue that it came from. Ask a person only for what needs that person's authority.

## The `Filed for` line

Every issue you file carries one line in its spec. Triage reads it to tell your filing from another session's label.

- For a person: `Filed for <person> at their request: <request>`. The request is `dispatch://<KEY>/ask/<id>` for the person who answered that ask, or `Slack <channel_id> message <ts>` for the mention's `<@user_id>` (add `, adopting message <earlier ts>` when the mention asks for an earlier message's request), or `direct message <id>`, the id that its frame's `reply_with` names after `--in-reply-to` (for a message that came as your own user turn, `direct message to session <your session id>`).
- For a clear defect: `Filed for Legion from <role> on <issue>`.

## The record lines

Your reply under an ask, or to a finding on the issue that it came from, begins with exactly one of these lines. The catch-up reads them.

- `Filed dispatch://<KEY>`
- `Handed dispatch://<KEY>`, which only an earlier rule wrote
- `Not filed: <reason>`
- `Asked dispatch://<KEY>/ask/<id>`
- `Admitted dispatch://<KEY>` or `Not admitted: <reason>`
- `Parked dispatch://<root>`, `Not parked dispatch://<root>` or `Left admitted dispatch://<root>`

Dispatch cannot resolve an answered ask, so that reply is its record. Close an open finding ask that turned moot with `dispatch resolve-ask --kind retracted`, and say why.

## Open a finding ask

Open it with `dispatch ask`, and follow it. Its question begins `Finding from <role>`. Then it states what went wrong, where and to whom, the evidence, where the fix belongs, the decision a person must make, and your recommendation. Give the options `File an issue`, `File it for Legion` and `Do not file`. Reply to a finder with `Asked dispatch://<KEY>/ask/<id>`, as `skill://legion-controller/references/finding-asks-steps.md` says.

## File a clear defect

A role sends you a clear defect in a repository that its token does not reach. A defect that stops its sender's work, in a repository the sender's token reaches, is the sender's to fix, unless an issue that tracks it is claimed, `in_progress` and its holder said on Envoy that it works on it now: reply `Not filed: it stops your work, so it is yours to fix`, and take none of the steps. Take a clear defect that you find yourself as below, with `controller` as the role, on the issue where you found it.

For each other clear defect, take the five steps of `skill://legion-controller/references/finding-asks-steps.md#file-a-clear-defect`. They say when you file it and how you reply to the finder.

## Act on its answer

A park ask's answer goes to step 4 of `skill://legion-controller/references/park-asks.md`. For any other, `dispatch read --ref` the ask and act on the newest answer. When its typed text asks for another of these options than the one selected, do what the text asks. When the text asks for none of them, reply with your question and file nothing.

- `Do not file`: file nothing.
- `File an issue`: `dispatch issue --project <PROJECT> --title '<the problem>' --spec-file -`, in the project where the fix belongs (this one when the answer names none). The spec states the problem and where the fix belongs, cites the ask and its evidence, and carries `Filed for <the answer's By:> at their request: dispatch://<KEY>/ask/<id>`.
- `File it for Legion`: the same, with `--label legion`. Its triage wake brings it back.

An ask that re-files an escalated child also takes the steps of `skill://legion-controller/references/escalation.md#a-child-that-looks-independent`. Then reply under the ask with the record line.

## Catch up at every start

Nothing wakes on or follows an ask while no controller runs. So at every start:

1. `dispatch open-asks --project <PROJECT>` lists every open ask, and no ask on a closed issue. Follow each of yours: one whose options include `Do not file`, `Admit it` or `Park it`.
2. Find the answered asks still waiting on you, on each open issue that `read_state` records and on each issue of `dispatch issues --project <PROJECT> --status triage --all`: `dispatch api --path '/api/v1/issues/<KEY>/asks?state=answered&thread=1'`. An ask waits on you when one of its options is `Do not file`, `Admit it` or `Park it`, and no session reply newer than its answer begins with a record line. Act on each as above, and on a triage ask as `skill://legion-controller/references/triage.md` says.

A read that fails or prints no valid JSON decides nothing. Name its first line in your summary.

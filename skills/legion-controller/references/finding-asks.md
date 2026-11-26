# Finding asks

The report issue (`skill://legion-controller/references/daily-report.md`) holds the finding asks and the evidence of clear defects that no tree owns.

## The `Filed for` line

Every issue you file carries one line in its spec. Triage reads it to tell your filing from another session's label.

- For a person: `Filed for <person> at their request: <request>`. The request is `dispatch://<REPORT KEY>/ask/<id>` for the person who answered that ask, or `Slack <channel_id> message <ts>` for the mention's `<@user_id>` (add `, adopting message <earlier ts>` when the mention asks for an earlier message's request), or `direct message <id>`, the id that its frame's `reply_with` names after `--in-reply-to` (for a message that came as your own user turn, `direct message to session <your session id>`).
- For a clear defect that no running tree owns: `Filed for Legion from <role> on <issue>: <evidence reference>`.

## The record lines

Your reply under an ask or an evidence comment begins with exactly one of these lines. The catch-up reads them.

- `Filed dispatch://<KEY>`
- `Handed dispatch://<KEY>`, which only an earlier rule wrote
- `Not filed: <reason>`
- `Asked dispatch://<REPORT KEY>/ask/<id>`
- `Admitted dispatch://<KEY>` or `Not admitted: <reason>`
- `Parked dispatch://<root>`, `Not parked dispatch://<root>` or `Left admitted dispatch://<root>`

Dispatch cannot resolve an answered ask, so that reply is its record. Close an open finding ask that turned moot with `dispatch resolve-ask --kind retracted`, and say why.

## Open a finding ask

Open it on the report issue with `dispatch ask`, and follow it. Its question begins `Finding from controller`. Then it states what went wrong, where and to whom, the evidence or its reference, where the fix belongs, the decision a person must make, and your recommendation. Give the options `File an issue`, `File it for Legion` and `Do not file`.

## File a clear defect no tree owns

A role writes a clear defect to you only when it found no tree that owns the code: a comment that mentions your role, on the report issue or on its own issue, with the first line `Clear defect from <role>`. A clear defect that you find yourself goes to its owner as `skill://legion-worker/references/findings.md#write-to-the-owner` says. When no tree owns it, post its evidence on the report issue with `controller` as the role.

Before each such defect, take the five steps of `skill://legion-controller/references/finding-asks-steps.md#file-a-clear-defect-no-tree-owns`. They say when you file it and how you reply under its evidence.

## Follow it

A finding ask that another role opened reaches you as `dispatch://<KEY>/ask/<id>`. Follow it so that its answer reaches you: `dispatch follow --ask 'dispatch://<KEY>/ask/<id>' --action follow`. When `dispatch read --ref` shows it answered already, act on its answer now.

## Act on its answer

A park ask's answer goes to step 4 of `skill://legion-controller/references/park-asks.md`. For any other, `dispatch read --ref` the ask and act on the newest answer. When its typed text asks for another of these options than the one selected, do what the text asks. When the text asks for none of them, reply with your question and file nothing.

- `Do not file`: file nothing.
- `File an issue`: `dispatch issue --project <PROJECT> --title '<the problem>' --spec-file -`, in the project where the fix belongs (this one when the answer names none). The spec states the problem and where the fix belongs, cites the ask and its evidence, and carries `Filed for <the answer's By:> at their request: dispatch://<REPORT KEY>/ask/<id>`.
- `File it for Legion`: the same, with `--label legion`. Its triage wake brings it back.

An ask that re-files an escalated child also takes the steps of `skill://legion-controller/references/escalation.md#a-child-that-looks-independent`. Then reply under the ask with the record line.

## Catch up at every start

Nothing wakes on or follows an ask while no controller runs. So at every start:

1. `dispatch read` the report issue. When none exists, there is nothing to catch up. Keep it answerable (`skill://legion-controller/references/daily-report.md#keep-it-answerable`).
2. `dispatch open-asks --project <PROJECT>` lists every open ask, and no ask on a closed issue. Follow each on the report issue. Take each whose question begins `Clear defect from ` as step 5 says.
3. Find the answered asks still waiting on you, on the report issue and on each issue of `dispatch issues --project <PROJECT> --status needs_review --all`: `dispatch api --path '/api/v1/issues/<KEY>/asks?state=answered&thread=1'`. An ask waits on you when one of its options is `Do not file`, `Admit it` or `Park it`, and no session reply newer than its answer begins with a record line. Act on each as above, on a triage ask as `skill://legion-controller/references/triage.md` says, and on a `Hand it to the owning tree` answer as step 5 says.
4. Find the clear defects that nobody took: each top-level comment of `dispatch api --path '/api/v1/issues/<REPORT KEY>/comments'` whose body begins `Clear defect from ` and that has no session reply beginning with a record line. Take each as [File a clear defect no tree owns](#file-a-clear-defect-no-tree-owns) says.
5. Take each ask that an earlier rule opened whose question begins `Clear defect from ` the same way, with `--reply-to-ask '<ask id>'` for `--reply-to`: an open one, which you resolve once it has your reply, and one answered `Hand it to the owning tree`, which is a person's request. Where step 2 or 3 of the five steps finds a live tree, reply `Not filed: dispatch://<KEY> owns it; role:<token> is mentioned here` (`<KEY>` the issue or the root), with `--mention 'role:<its architect's token>'`. That architect reads the answer and the evidence there, so copy neither. When step 2's issue is in no tree, reply `Not filed: dispatch://<KEY> tracks it; no tree owns it` to its person, and mention no finder.

A read that fails or prints no valid JSON decides nothing. Name its first line in your summary.

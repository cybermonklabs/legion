# Daily report

## Find the report issue

Use the issue that the deployment instructions name for Legion's reports, else the project's `Legion daily report` (`dispatch search --query '"Legion daily report"' --project <PROJECT>`).
When none exists, create it once and park it in `icebox`. It is the one issue you file unasked. It holds no work and never carries the `legion` label.
Assign it to the person who answers for the project (the deployment instructions name them, else the owner that `dispatch whoami` names), because its asks reach that person's Inbox. With nobody known, leave out `--assignee`.

```text
dispatch issue --project <PROJECT> --title 'Legion daily report' --assignee '<email>' --spec-file - <<'EOF'
## Summary

Legion's controller posts one message here each day: what Legion finished, what it closed without a change and why, and what is running. Legion's agents post here the evidence of clear defects outside their own work, and the asks about problems that need a person's decision. This issue holds no work, so it carries no `legion` label and stays in icebox.
EOF
legion({ op: "set_status", issue: "<REPORT KEY>", status: "icebox" })
```

## Keep it answerable

Each time you read it:

- `Status: done`: the close withdrew its open asks, and Dispatch refuses new ones. Reopen it with `set_status` to `icebox`, and say so in the day's report.
- `Assignee: unassigned` with a person known: `dispatch issue-update --issue <REPORT KEY> --assignee <email>`. A refusal leaves it as it was: name the refusal in your summary.
- While it is unassigned, open each report with one sentence that asks a person to take the issue from its header. Until someone does, its asks go to the project's default assignee.

## When

After the turn's own work, read the report issue and keep it answerable. Post when its `Events:` show no `message.created` from today.

## What

One `dispatch message --issue <REPORT KEY> --body-file -` of at most 2,000 characters. Name every issue by its `dispatch://` link and what it does for the reader, and every pull request by its URL. A day with nothing finished says so in one sentence. When the lists do not fit, give each list's count, and name its priority 0 and 1 issues in board rank order.

- **Finished.** `dispatch issues --project <PROJECT> --status done --updated-since <the previous report's time, or 24 hours ago> --all`. Keep the issues that `read_state` records whose `issue.closed` is after the previous report, each with the pull request under its `External links:`.
- **Closed without a change.** Those with no pull request, each with the reason that its closing message gave.
- **Running.** Each root in `admission.trees` with its phase, and under it each of its issues in `admission.active` with its own phase. Then `admission.waiting`. Then each root that waits on a claim, with its holder.
- **Waiting on Dispatch links.** Each issue in `admission.blocked` with what it waits on. Say of each whose `waitingLine.kind` is `waiting_on_issue` that it holds no slot while it waits, that nothing of its work stopped, and that it takes a slot again by itself.
- **What nothing starts.** `admission.held` grouped by `reason`: each group's count, issues and `heldBy`, and what ends it (`skill://legion-controller/references/admission.md`). An `unclassified` issue with its `phase` and `status`. Each `task_stopped` issue with its `heldFrom`.
- **Phase workers nothing will prompt.** The most recent tick's `silent` list, with each role and how long it has been quiet, or how long its background work has stood.
- **Waiting on a person.** Each tree whose next step waits on an open ask, with that ask's `dispatch://` reference.
- **Asks resolved with no answer.** For each root in `admission.active` or `admission.waiting`, each park ask of the root (`skill://legion-controller/references/park-asks.md`) that was resolved with no answer, unless its wait has ended.
- **Free slots**, counted in issues, as the skill's "Free slots" says.
- **Open capabilities.** Each `capabilities` row with `status: open`: its name, `detail`, a deployment row's `configLine`, and who acts on it.
- **Stalled work.** Each `provider-errors` run with its model, and each root that an `architect-stalled` or `tree-stalled` wake left for the report.
- **The week**, in the first report of each UTC week only: the numbers of `skill://legion-controller/references/weekly-audit.md`, each beside the command that recounts it.

# Daily report

## Find the report issue

Use the issue that the deployment instructions name for Legion's reports, else the project's `Legion daily report` (`dispatch search --query '"Legion daily report"' --project <PROJECT>`).
When none exists, create it once and park it in `icebox`. It is the one issue you file unasked. It holds no work and never carries the `legion` label.

```text
dispatch issue --project <PROJECT> --title 'Legion daily report' --spec-file - <<'EOF'
## Summary

Legion's controller posts one message here each day: what Legion finished, what it closed without a change and why, and what is running. This issue holds no work, so it carries no `legion` label and stays in icebox.
EOF
legion({ op: "set_status", issue: "<REPORT KEY>", status: "icebox" })
```

## Keep it open

Dispatch refuses an upload to a closed issue, and the consolidation record is an upload. So each time you read it and it shows `Status: done`, reopen it with `set_status` to `icebox`, and say so in the day's report.

## When

After the turn's own work, read the report issue and keep it open. Post when its `Events:` show no `message.created` from today.

## What

One `dispatch message --issue <REPORT KEY> --body-file -` of at most 2,000 characters. Name every issue by its `dispatch://` link and what it does for the reader, and every pull request by its URL. A day with nothing finished says so in one sentence. When the lists do not fit, give each list's count, and name its priority 0 and 1 issues in board rank order.

- **Finished.** `dispatch issues --project <PROJECT> --status done --updated-since <the previous report's time, or 24 hours ago> --all`. Keep the issues that `read_state` records whose `issue.closed` is after the previous report, each with the pull request under its `External links:`.
- **Closed without a change.** Those with no pull request, each with the reason that its closing message gave.
- **Running.** Each root of a live tree (`skill://legion-controller/references/admission.md`) with its phase, and under it each of its issues in `admission.active` with its own phase. Then `admission.waiting`. Then each root that waits on a claim, with its holder.
- **Waiting on Dispatch links.** Each issue in `admission.blocked` with what it waits on. Say of each whose `waitingLine.kind` is `waiting_on_issue` that it holds no slot while it waits, that nothing of its work stopped, and that it takes a slot again by itself.
- **What nothing starts.** The most recent tick's `held` list grouped by `reason`: each group's count, issues and `heldBy`, and what ends it (`skill://legion-controller/references/admission.md`). An `unclassified` issue with its `phase` and `status`. Each `task_stopped` issue with its `heldFrom`.
- **Phase workers nothing will prompt.** The most recent tick's `silent` list, with each role and how long it has been quiet, or how long its background work has stood.
- **Waiting on a person.** Each tree whose next step waits on an open ask, with that ask's `dispatch://` reference.
- **Asks resolved with no answer.** For each root in `admission.active` or `admission.waiting`, each park ask of the root (`skill://legion-controller/references/park-asks.md`) that was resolved with no answer, unless its wait has ended.
- **Free slots**, counted in issues, as the skill's "Free slots" says.
- **Open capabilities.** Each `capabilities` row with `status: open`: its name, `detail`, a deployment row's `configLine`, and who acts on it.
- **Stalled work.** Each claim that a `provider-errors` wake named, with its role, its run and its model. Each root that an `architect-stalled` or `tree-stalled` wake named since the last report, with the arm, what you did and what it waits on.
- **Operations you lack.** Each operation of yours that you did not run (a `can-i` answer of no, a failed `kubectl`, no daemon label, no or several daemon pods, an `agent-secrets` denial), with the work, the read and its answer. Each Slack reply that Slack refused or that the broker granted no token for. Each re-run or mention reply that GitHub refused (curl's exit 22), with the status curl printed. Only a person or the operator fixes these: ask no other session to do any of them.
- **The week**, in the first report of each UTC week only: the numbers of `skill://legion-controller/references/weekly-audit.md`, each beside the command that recounts it.

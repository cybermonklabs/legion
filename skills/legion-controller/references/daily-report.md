# Daily report

Read this on your first turn of each UTC day, and when you open, follow or answer a finding ask: the report issue holds them all.

## Find the report issue

Use the issue the deployment instructions name for Legion's reports, else the project's `Legion daily report` (`dispatch search --query '"Legion daily report"' --project <PROJECT>`). When none exists, create it once, the one issue you file unasked, and park it in `icebox`: it holds no work and never carries the `legion` label. Assign it to the person who answers for the project (the deployment instructions name them, else the owner `dispatch whoami` names), because its asks reach that person's Inbox; with nobody known, leave out `--assignee`.

```text
dispatch issue --project <PROJECT> --title 'Legion daily report' --assignee '<email>' --spec-file - <<'EOF'
## Summary

Legion's controller posts one message here each day: what Legion finished, what it closed without a change and why, and what is running. Legion's agents also post here the evidence of each clear defect they found outside their own work, which the controller hands to the tree that owns the code, or files for Legion when no tree does. They ask here whether a problem that needs a decision needs an issue. This issue is not work, so it carries no `legion` label and stays in icebox.
EOF
legion({ op: "set_status", issue: "<report KEY>", status: "icebox" })
```

## Keep it answerable

Each time you read it:

- `Status: done`: the close withdrew its open asks, and Dispatch refuses new ones. Reopen it with `set_status` to `icebox` and say so in the day's report.
- `Assignee: unassigned` with a person known: `dispatch issue-update --issue <REPORT KEY> --assignee <email>`. An answer that does not print `<REPORT KEY>: assignee -> <email>` left the assignee as it was (`400 ASSIGNEE_NOT_ALLOWED` names an email nobody has signed in to Dispatch with): name it, with its first line, in your summary.
- While it is unassigned, open each report with one sentence asking a person to take the issue from its header: until someone does, its asks go to the project's default assignee.

## When

After the turn's own work, read the report issue, keep it answerable, and post when its `Events:` show no `message.created` from today.

## What

One `dispatch message --issue <report KEY> --body-file -` of at most 2,000 characters, written as `skill://dispatch-writing-specs`'s "Rules for the text" say: every issue by its `dispatch://` link and what it does for the reader, every pull request by its URL. A day with nothing finished says so in one sentence; when the lists do not fit, give each list's count and name its priority 0 and 1 issues in board rank order.

- **Finished.** `dispatch issues --project <PROJECT> --status done --updated-since <the previous report's time, or 24 hours ago> --all`, keeping the issues `read_state` records whose `issue.closed` is after the previous report, each with the pull request under its `External links:`.
- **Closed without a change.** Those with no pull request, each with the reason its closing message gave (the `message.created` just before `issue.closed`).
- **Running.** Each root in `admission.trees` with its phase, and under it each of its issues in `admission.active` with its own; then `admission.waiting`; then each root the every-turn check found waiting on a claim, with its holder.
- **Waiting on Dispatch links.** Each issue in `admission.blocked` with what it waits on; say of each whose `waitingLine.kind` is `waiting_on_issue` that it holds no slot while it waits, nothing of its work stopped, and it takes one again by itself. A link's wait ends when each blocker reaches `done` (a person's close, or the blocker's own merge) or its link is taken off (by a person or an architect of the tree), and a children-first wait ends when the last open child leaves the workflow, so take nothing for them.
- **What nothing starts.** `admission.held` grouped by `reason`, each group's count, issues and `heldBy`, and what ends it (`skill://legion-controller/references/admission-held.md`); an `unclassified` issue with its `phase` and `status`.
- **Phase workers nothing will prompt.** The most recent tick's `silent` list, with each role and how long it has been quiet, or how long its background work has stood (`backgroundSince`). The list comes on the tick alone: say nothing of them on a day no tick carried one.
- **Waiting on a person.** Each tree whose next step waits on a person's answer (a park, deferral, finding or any other ask), with that ask's `dispatch://` reference, while the ask is open. Leave out a deferral row whose root fails the check of step 2 of `skill://legion-controller/references/backlog-stewardship.md#a-root-the-plan-defers`.
- **Asks resolved with no answer.** For each root in `admission.active` or `admission.waiting`, read its `Asked whether to park:` and `Asked whether to defer:` comments with step 1 of `skill://legion-controller/references/park-asks.md`, once with each prefix, and take the newest comment of each key (its second line). When the ask its first line names was resolved with no answer (`dispatch read --ref '<that reference>'`), name the root, the key and that ask's `dispatch://` reference. Leave out a key whose wait has ended: a `held` or `architect failed:` key that `read_state` no longer builds, an `event:` key whose event has happened, and a plan row that the weekly plan no longer defers.
- **Free slots**, counted in issues, as the skill's "Free slots" says.
- **The priorities.** Each plan line the reconciliation before this report moved for the deployment's priorities, and this project's count of work in flight under no priority, both from step 2 of `skill://legion-controller/references/backlog-stewardship.md#reconcile-the-weekly-plan`.
- **Open capabilities.** Each `capabilities` row with `status: open`: its name, `detail`, a deployment row's `configLine`, and who acts on it. Nothing when none is open.
- **Stalled work.** Each `provider-errors` run with its model, and each root an `architect-stalled` or `tree-stalled` wake left for the report (`skill://legion-controller/references/wakes.md`).
- **The week** (first report of each UTC week only): the numbers of `skill://legion-controller/references/weekly-audit.md`, each beside the command that recounts it.

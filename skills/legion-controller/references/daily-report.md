# Daily report

Read this on your first turn of each UTC day.

## Find the report issue

Use the issue the deployment instructions name for Legion's reports, else the project's `Legion daily report` (`dispatch search --query '"Legion daily report"' --project <PROJECT>`).
When none exists, create it once, the one issue you file unasked, and park it in `icebox`: it holds no work and never carries the `legion` label.
Assign it to the person who answers for the project (the deployment instructions name them, else the owner `dispatch whoami` names);
with nobody known, leave out `--assignee`.

```text
dispatch issue --project <PROJECT> --title 'Legion daily report' --assignee '<email>' --spec-file - <<'EOF'
## Summary

Legion's controller posts one message here each day: what Legion finished, what it closed without a change and why, and what is running. Legion's agents also post here the evidence of each clear defect they found that no tree owns, which the controller files for Legion. They ask here whether a problem that needs a decision needs an issue. This issue is not work, so it carries no `legion` label and stays in icebox.
EOF
legion({ op: "set_status", issue: "<report KEY>", status: "icebox" })
```

## When

After the turn's own work, read the report issue.
When it reads `Status: done`, reopen it with `set_status` to `icebox` and say so in the day's report.
Post when its `Events:` show no `message.created` from today.

## What

One `dispatch message --issue <report KEY> --body-file -` of at most 2,000 characters, written as `skill://dispatch-writing-specs`'s "Rules for the text" say:
every issue by its `dispatch://` link and what it does for the reader, every pull request by its URL.
A day with nothing finished says so in one sentence; when the lists do not fit, give each list's count and name its priority 0 and 1 issues in board rank order.

- **Finished.**
  `dispatch issues --project <PROJECT> --status done --updated-since <the previous report's time, or 24 hours ago> --all`, keeping the issues `read_state` records whose `issue.closed` is after the previous report, each with the pull request under its `External links:`.

- **Closed without a change.**
  Those with no pull request, each with the reason its closing message gave (the `message.created` before `issue.closed`, with only the close's own settlement and withdrawal lines between them).

- **Running.**
  Each root of a live tree (`skill://legion-controller/references/admission.md`) with its phase, and under it each of its issues in `admission.active` with its own;
  then `admission.waiting`; then each root the every-turn check found waiting on a claim, with its holder.

- **Waiting on Dispatch links.**
  Each issue in `admission.blocked` with what it waits on;
  say of each whose `waitingLine.kind` is `waiting_on_issue` that it holds no slot while it waits, nothing of its work stopped, and it takes one again by itself.

- **What nothing starts.**
  The most recent tick's `held` list grouped by `reason`, each group's count, issues and `heldBy`, and what ends it (`skill://legion-controller/references/admission.md`);
  an `unclassified` issue with its `phase` and `status`; each `task_stopped` issue with its `heldFrom`.

- **Phase workers nothing will prompt.**
  The most recent tick's `silent` list, with each role and how long it has been quiet, or how long its background work has stood (`backgroundSince`).
  The list comes on the tick alone.

- **Waiting on a person.**
  Each tree whose next step waits on a person's answer (a park, deferral, finding or any other ask), with that ask's `dispatch://` reference, while the ask is open.

- **Asks resolved with no answer.**
  For each root in `admission.active` or `admission.waiting`, the newest `Asked whether to park:` and `Asked whether to defer:` comment of each key (step 1 of `skill://legion-controller/references/park-asks.md`, once with each prefix) whose ask was resolved with no answer, unless its wait has ended.

- **Free slots**, counted in issues, as `skill://legion-controller/references/admission.md#free-slots` says.

- **The priorities.**
  Each plan line the reconciliation before this report moved for the deployment's priorities, and this project's count of work in flight under no priority, both from step 2 of `skill://legion-controller/references/backlog-stewardship.md#reconcile-the-weekly-plan`.

- **Open capabilities.**
  Each `capabilities` row with `status: open`: its name, `detail`, a deployment row's `configLine`, and who acts on it.
  Nothing when none is open.

- **Stalled work.**
  Each claim a `provider-errors` wake named, with its role, run and model, and each root an `architect-stalled` or `tree-stalled` wake named since the last report, with the arm, what you did and what it waits on (`skill://legion-controller/references/wakes.md`).

- **Operations you lack.**
  Each operation of yours you did not run (a `can-i` answer of no, a failed `kubectl`, no daemon label, no or several daemon pods, an `agent-secrets` denial), with the work, the read and its answer;
  each Slack reply that Slack refused or that the broker granted no token for;
  and each re-run or mention reply GitHub refused (curl's exit 22), with the status curl printed.
  Only a person or the operator fixes these: ask no other session to do any of them.

- **The week** (first report of each UTC week only):
  the numbers of `skill://legion-controller/references/weekly-audit.md`, each beside the command that recounts it.

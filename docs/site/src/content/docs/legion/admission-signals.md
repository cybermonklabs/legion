---
title: Admission by signals
description: The five signals the Legion daemon can admit issues below P0 by, how legion state shows them and the record of their changes, how a person turns admission by signals on and off and sets and clears each limit, each limit's range, and the provider's configuration.
sidebar:
  order: 4.2
---

Admission by signals is a rule of the daemon's admission pass for the issues below P0.
While it is on, the next issue below P0 in the waiting line starts only while each signal that has a limit reads under that limit and the cap has room, so the cap is then a ceiling only.
While it is off, the daemon admits as it always has, under the cap alone.
Every project starts with it off and with no limit, and only a person turns it on, sets a limit or clears one.

The daemon computes the signals whether the rule is on or off, shows them in `legion state`, and keeps a record of each change of them.
So you can read the signals for as long as you like before you turn the rule on.

This page uses the words of [Admission and the `legion` label](/legion/legion/concepts/#admission-and-the-legion-label).
[Replaying a recorded window](/legion/legion/replay/#the-signals) reads the same signals over a window that a daemon recorded, with the same code, and its comparison gives the limits that a record supports.

## What the rule does

- A P0 issue starts as it does with the rule off: under the cap, whatever the signals read.

- An issue below P0 starts while every signal that has a limit reads under its limit, and the cap has room.
  A signal holds admission where it reads at or over its limit.

- A signal with no limit holds nothing.
  A signal that the daemon cannot compute holds nothing either, so the worst case is the cap alone, which is how the daemon admits with the rule off.

- Every other hold on a start stays: a Dispatch link, a wait on a person, a paused project.

- A signal holds only a start that has not happened.
  The pass takes no slot from a holder because a signal is over its limit.

- Events run the passes: a change of a signal's input and a person's change of a setting each run an admission pass.
  Where a signal would cross its limit with no event, as when a kill leaves its hour, the daemon keeps one timer for the project, set to the next moment at which a signal with a limit would cross it.
  Each event sets the timer again, and its firing runs a pass.
  It never fires on a period.

- While the rule is on, the controller's tick does everything it did before, except start an issue below P0.

- The rule is the one that `legion replay` runs as `signals`, so a replay of a recorded window gives the daemon's own decisions.

## The five signals

Each signal has one definition, in the daemon's package `internal/signals`, which both the admission pass and `legion replay` call (`replay.SignalDefinitions()` lists the five).
Lower is better for each.

The provider's signal has one part, its headroom.
So admission by signals reads five series, one for each signal: `idle`, `queue`, `provider` `headroom`, `kills` and `approvals`.
Every surface names a series by its signal and, for the provider, by its part: the `legion` command, the controller's tool, `legion state --json` and the record of changes.

A series is not recorded, never zero, where the daemon cannot compute it.
`legion state` then gives the reason in place of a value, and the series holds nothing.
A series that counts its input over a span is not recorded until the daemon has kept that input for the whole span.
The daemon records, for each project and input, the moment it began to keep that input, at the first boot of the release that keeps it, and no later boot moves it.

### `idle`

**What it is**: the share of the slot holders that have been idle for an hour or more, from 0 to 1.
A holder's state is the one that the `slots` line of `legion state` gives it ([Observe](/legion/legion/running-legion/#observe)): a holder is `idle` where it is not `working`, `stopped`, `person` or `issue`.
Its idle time runs from the end of its last turn, or from the moment it took its slot where that is later.
With no holder, the share is 0.

**Not recorded**: never, since every input is the daemon's own: its slots, and the start and the end of each turn.

### `queue`

**What it is**: the wait of the merge queue, in hours.
It is the larger of two waits: the median wait of the project's pull requests that left the merge queue in the 6 hours before the moment, and the wait so far of the project's pull request that has stood in the queue longest.
A wait runs from GitHub's `added_to_merge_queue` to the `merged` or the `removed_from_merge_queue` that ends it, so a drop and a re-add are two waits.
The removal that GitHub records beside a merge ends no second wait.
With no exit in the 6 hours and no pull request in the queue, the wait is 0.

The daemon takes each event from GitHub's own timeline of the pull request, with GitHub's own time, and never from the time a notification arrived.
It reads the timeline after each `enqueued`, `dequeued` and merged notification of a pull request on an issue branch of the project.
At each boot it also reads GitHub's merge queue of each repository and branch that serves the project's pull requests, and the timeline of each pull request that stands in it or that its own record still holds in the queue.

**Not recorded**:

- before the daemon began to keep the merge queue's events (`the daemon has not begun keeping the merge queue's events`);

- for the 6 hours after the first boot that kept them (`the daemon keeps the merge queue's events from <t>, so the span this signal counts is whole from <t + 6h>`);

- while the boot's read of GitHub's merge queue fails, until a read succeeds (`the boot's read of GitHub's merge queue failed: <why>`).
  A failed read is never read as an empty queue.

### `provider` `headroom`

**What it is**: the share of the project's token limit that its sessions spent in the last whole minute before the moment.
It is the uncached input tokens of that minute over the limit a minute that the configuration gave when the minute began (`signal_token_limit_per_minute`, [below](#the-providers-configuration)).
Uncached input tokens are the fresh input and the cache writes of a request, never its cache reads.
Only the models that `signal_token_model_pattern` selects count, and every model counts where it is unset.
The headroom can read above 1 in a burst, and the daemon cuts no reading.

Each host of a session of the project reports to the daemon each model request that the provider answered: Oh My Pi through the `pi-legion` plugin, and Claude Code through `legion claude-bridge`.
The sessions are each role's, each subagent's that a role starts, and the project controller's.
A host reports a request within 15 seconds of its end, and at the end of the turn at the latest.
The Claude Code bridge can report a request before its last block arrives, since no block says that it is the last: a later block of it makes the bridge report the request again under the same id with the later end, and the daemon moves the request, with its tokens, to the minute of that end.

The headroom counts the uncached input tokens of the requests that the hosts report, and no others.
Oh My Pi reports no request made outside its agent loop, such as a title, a classifier, a compaction's summary or a memory extraction, so the headroom does not count those tokens.
The production check of admission by signals still compares the daemon's uncached input tokens of each hour, the sum of `uncachedInputTokens` over the `provider.minutes` of `legion state --json`, with the deployment's full token totals for the same hour.

**Not recorded**:

- where the configuration gives the project no token limit a minute (`the daemon's configuration gives the project no token limit a minute (signal_token_limit_per_minute)`);

- where a request of the minute before was not reported: a host of the project reports none, which is any host on a plugin before admission by signals, or the daemon began to keep the requests after the minute began;

- where a request of the minute before, of a model that the project counts, was reported with no token count, since the minute's tokens are then not known (`a request of the minute before was reported with no token count (uncachedInputTokens), so the minute's tokens are not known`, or the count of such requests where there are more);

- before the daemon began to keep the model requests (`the daemon has not begun keeping the project's model requests`).

### `kills`

**What it is**: the count of the memory kills in the project's issue pods in the hour before the moment, each counted from the moment the pod's launcher named it until an hour later.
A kill is each process that the memory guard ended and each one that the pod's microVM kernel stopped, of a role or of no role, as the launcher reports each kill for which it writes its `launcher: memory guard: ended` line.
Each kill counts once, at its own moment, so a report that a reconnected launcher sends late counts at the moment of the kill.
A kill in the controller's pod does not count.

**Not recorded**: before the daemon began to keep the kills (`the daemon has not begun keeping the memory kills of the project's issue pods`), and for the hour after the first boot that kept them.

### `approvals`

**What it is**: the count of the asks open on the project's issues that wait on the project's default assignee.
An ask waits on its `asked_of` person, else on its issue's assignee, else on the project's default assignee.
It counts while it is open and waits on a person rather than on its agent.
The daemon keeps each ask from Dispatch's events, with the person it was asked of and whose turn it is, each issue's assignee, and the default assignee from Dispatch's `settings.project_default_assignee.updated` event.
Each boot reads the project's open asks, with its default assignee, from Dispatch (`GET /api/v1/asks/open?project=<KEY>`).
The approvals change only at events, so this signal has no crossing of its own for the timer.

**Not recorded**:

- from the first boot that keeps the asks until its read of Dispatch's open asks succeeds (`the daemon has not yet read the project's open asks from Dispatch since it began keeping them`);

- while a boot's read of Dispatch fails, with its reason;

- while Dispatch has given no default assignee (`Dispatch has not given the project's default assignee: …`).
  A Dispatch before this release answers the read without it, and the signal then reads as not recorded until a change of the default assignee arrives.

### The limits

A signal holds admission where it reads at or over its limit, so a limit of 0 would hold every start below P0, and each range starts above 0.
The daemon refuses a limit outside its series' range and changes nothing.

| Series | Unit | Legal limit | Step of a value entry |
| :--- | :--- | :--- | :--- |
| `idle` | a share of the slot holders | above 0, at most 1 | 0.05 |
| `queue` | hours | above 0, at most 168 (one week) | 0.25 (15 minutes) |
| `provider` `headroom` | a share of the token limit a minute | above 0, at most 10 | 0.10 |
| `kills` | kills in the hour | a whole number from 1 to 1,000,000 | 5 |
| `approvals` | open asks | a whole number from 1 to 1,000,000 | 5 |

The step is for [the record of changes](#the-record-of-changes) and changes no decision.

## Read the signals

### `legion state`

`legion state` prints one line for admission by signals, after the `slots` line and the `P0` line where it prints one:

```text
signals: admission by signals on; idle 0.25 limit 0.5 does not hold; queue not recorded (the daemon keeps the merge queue's events from 2026-11-26T06:00:00Z, so the span this signal counts is whole from 2026-11-26T12:00:00Z) limit none does not hold; provider/headroom 0.42 limit 0.9 does not hold; kills 3 limit 2 holds since 2026-11-26T07:00:00Z; approvals 1 limit none does not hold
```

The line gives the switch, and then each series by its signal and part: its value, or `not recorded` and the reason in parentheses; its limit, or `limit none`; and whether it holds admission.
`holds since <time>` dates the hold by the pass that recorded it, and `holds, undated` is a hold that no committed pass has recorded yet.
A series never holds while the rule is off, and the line still gives each value against its limit, so you can read what the rule would do.
The reason is the `absent` of `legion state --json`, with each character that does not print written as its Go escape (`\n`, `\x1b`), so a reason that quotes GitHub's or Dispatch's answer keeps the line one line.
A daemon that runs no workflow prints no such line, and neither does a daemon before admission by signals.

### `legion state --json`

`admission.signals` is admission by signals as the daemon reads it at the moment of the read:

- `on`: whether the rule is on.

- `switch`: the last change of the switch, `{setBy, at, reason}`, and null before a person first set it.

- `series`: the five series, in the order `idle`, `queue`, `provider` `headroom`, `kills`, `approvals`.
  Each has `signal`; `part`, `headroom` for the provider and null for every other signal; `value`, null where the daemon cannot compute it; `absent`, the reason it cannot, null where it can; `limit`, null for none; `limitSet`, the last change of the limit, `{setBy, at, reason}`, a clear among them, null before a person first set or cleared it; `holds`, whether it holds admission now, never while the rule is off and never where it is not recorded; and `since`, the moment it began to stand as it does, as the record's newest holds entry of the series dates it, null where no committed pass recorded it so.

- `provider.minutes`: each whole minute of the hour before the minute under way, oldest first: `at`; `held`, whether every request of the minute is known to have been reported, each counted one with its token count, since the headroom is not recorded while the minute before is not held; `uncachedInputTokens`, of the models that `signal_token_model_pattern` selects; and `tokenLimit`, the limit in force when the minute began, 0 for none.
  It names no request and no session, so you can compare the daemon's input with another record of the provider.

`setBy` names who made a change in the form in which the state names who paused the project: `{"kind": "operator"}`, or `{"kind": "controller", "session": "<session>"}`.
Each value of `admission.signals` is computed at the read from what the daemon keeps of the inputs, never from an entry of the record.

```sh
legion state --config legion.yaml --json | jq '.admission.signals.series[] | {signal, part, value, absent, limit, holds, since}'
```

A plugin of an earlier contract is served the state with neither `admission.signals` nor `admission.signalChanges`.

### The record of changes

`admission.signalChanges` is the newest 50 entries of the project's record of changes, newest first, and `[]` before the first entry.
Each entry has exactly nine members:

- `id`: a whole number that rises by one for each entry of the project, so a reader sees a lost entry as a gap.

- `at`: the moment of the change.

- `signal`: `idle`, `queue`, `provider`, `kills` or `approvals`, and null for a change of the switch.

- `part`: `headroom` for the provider, and null for every other signal and for the switch.

- `what`: `value`, `limit`, `holds` or `switch`.

- `value`: the new value, a number: 1 or 0 for `holds` and for `switch`; null for a limit that a person cleared, and for a value that the daemon cannot compute.

- `absent`: the reason where the daemon cannot compute the value, and null in every other entry.

- `setBy`: who changed a limit or the switch, and null in every other entry.

- `reason`: the person's cited word for a change of a limit or of the switch, and null in every other entry.

```json
[
  { "id": 10, "at": "2026-11-26T09:22:24.893115Z", "signal": "queue", "part": null, "what": "holds", "value": 1, "absent": null, "setBy": null, "reason": null },
  { "id": 9, "at": "2026-11-26T09:22:24.893115Z", "signal": "queue", "part": null, "what": "value", "value": 3.462083333333333, "absent": null, "setBy": null, "reason": null },
  { "id": 8, "at": "2026-11-26T09:22:24.890493Z", "signal": null, "part": null, "what": "switch", "value": 1, "absent": null, "setBy": { "kind": "operator" }, "reason": "the owner turned admission by signals on in the weekly plan" },
  { "id": 7, "at": "2026-11-26T09:22:24.842402Z", "signal": "queue", "part": null, "what": "limit", "value": 1, "absent": null, "setBy": { "kind": "operator" }, "reason": "the owner set the merge queue's limit at 1 hour in the weekly plan" }
]
```

A person's change of the switch or of a limit writes its entry with the setting itself, in one transaction.
Each admission pass then writes entries of what it computed:

- the `holds` entry of a series whose hold differs from the newest `holds` entry of that series, and the `value` entry of that series beside it, so that the value of a series never disagrees with its hold;

- the `value` entry of a series whose value moved from the last one written by at least the step of the series (in the [table](#the-limits)), a move equal to the step among them;
  the first value of a series, and a move between a value and a value that the daemon cannot compute, count as a move of at least the step;

- for `provider` `headroom`, a value entry only at a pass that gives out or takes back a slot, and at a pass where its hold changes, so the series records the headroom at each slot decision rather than at every minute.

A pass writes these entries while the rule is off too, each hold then 0, so the record shows each signal before a person turns the rule on.
No entry is written because a clock reached a time, and one pass writes at most 16 entries: five series, three kinds, and the switch.
A restart keeps the record and the settings.

### The daemon's log

For each change of the switch, of a limit and of a hold, the daemon writes one line to its log once the change commits, and one for each start that a signal held, with the issue, the signal and its part, the signal's value and its limit.
The daemon writes its log as JSON, one record a line, and these are three of them, each without its `time`:

```json
{"level":"INFO","msg":"admission: a setting of admission by signals changed","setting":"queue","signal":"queue","part":null,"value":1,"by":"operator","session":"","reason":"the owner set the merge queue's limit at 1 hour in the weekly plan","at":"2026-11-26T09:22:24.842402Z"}
{"level":"INFO","msg":"admission: a signal's hold changed","signal":"queue","part":null,"holds":true,"value":3.462083333333333,"limit":1}
{"level":"INFO","msg":"admission: a signal held a start","issue":"WIDGETS-2","signal":"queue","part":null,"value":3.462083333333333,"limit":1,"kind":"event"}
```

A setting's `value` is 1 or 0 for the switch, and null for a cleared limit; a hold's `value` is the reason where the daemon cannot compute the series.

A boot whose read of a signal's source fails says so, and reads it again on a backoff until a read succeeds:
`legion daemon could not read the merge queue; the queue signal is not recorded until it can`, and `legion daemon could not read Dispatch's open asks; the approvals signal is not recorded until it can`.

## Turn it on and off, and set the limits

Turn admission by signals on, and set or clear a limit, only on the word of the person whose decision it is, and cite that word in `--reason`: who said it, and what they said.
The daemon keeps each change with who made it, when and why, applies it from the change's own admission pass on, with no restart, and a restart keeps it.

From an operator's shell, `legion project signals` makes each change:

```sh
legion project signals set-limit --signal queue --limit 2 \
  --reason "the owner set the merge queue's limit at 2 hours in the weekly plan" \
  --config legion.yaml --operator-token-file operator-token
legion project signals on --reason "the owner turned admission by signals on in the weekly plan" \
  --config legion.yaml --operator-token-file operator-token
legion project signals set-limit --signal provider --part headroom --limit 0.9 --reason "<who said it, and what they said>" \
  --config legion.yaml --operator-token-file operator-token
legion project signals clear-limit --signal provider --part headroom --reason "<who said it, and what they said>" \
  --config legion.yaml --operator-token-file operator-token
legion project signals off --reason "<who said it, and what they said>" \
  --config legion.yaml --operator-token-file operator-token
```

- `on` turns the rule on, and `off` turns it off; each takes `--reason` alone.

- `set-limit` sets one series' limit: `--signal` (`idle`, `queue`, `provider`, `kills` or `approvals`), `--part` for the provider (`headroom`, its one part; no other signal takes one), `--limit`, and `--reason`.

- `clear-limit` clears one series' limit, so it holds nothing: `--signal`, `--part` for the provider, and `--reason`.

- Each takes the flags of every operator command: `--operator-token-file` (required), and `--config` or `--port` to find the daemon.
  `--json` prints the daemon's answer as it served it.

Each answers with the settings as they stand after it, in one line, as here for a project with three limits set:

```text
admission by signals changed: admission by signals on; limits idle limit 0.5, queue limit none, provider/headroom limit 0.9, kills limit 2, approvals limit none
```

`unchanged, as it stood` takes the place of `changed` where the change asked for what the setting already held, which writes no entry.
The daemon refuses, with `400 SIGNAL_SETTING_INVALID` and its reason, and changes nothing: a limit outside its series' range, a name of no signal or no part, a member that the action does not take, and a blank reason.

```text
legion project signals set-limit: the daemon answered 400 Bad Request: signals: the limit of queue is 200, which is not above 0 and at most 168; nothing changed
legion project signals set-limit: the daemon answered 400 Bad Request: signals: the limit of kills is 2.5, which is no whole number from 1 to 1000000; nothing changed
```

The route is `POST /legion/v1/operator/project/signals`, with the operator's bearer.

### Through the controller

The project's controller makes the same changes with its `legion` tool, through `POST /legion/v1/project/signals` under its own grant, and only on a person's cited word:
`signals_on` and `signals_off` (`reason`), `set_signal_limit` (`signal`, `part` for the provider, `limit`, `reason`) and `clear_signal_limit` (`signal`, `part`, `reason`).
`reason` names whose word it is and what they said, and the controller never chooses a change itself: it regulates nothing.
The daemon records such a change as the controller's, with its session, and one made under a grant that the operator's bearer minted as the operator's.
Each answers `changed`, whether the change moved the setting, and `settings`, as they stand.

## The provider's configuration

The provider's headroom reads two optional keys of the project in `legion.yaml` ([`legion.yaml`](/legion/legion/running-legion/#legionyaml)):

```yaml
projects:
  WIDGETS:
    signal_token_limit_per_minute: 5500000
    signal_token_model_pattern: '^claude-'
```

- `signal_token_limit_per_minute` is the uncached input tokens that the project's sessions may spend in a minute.
  Without it the headroom is not recorded, never zero.
  Each minute is read against the limit in force when it began, so a daemon restarted with another limit reads no earlier minute again.

- `signal_token_model_pattern` selects the models whose tokens count against the limit; every model counts where it is unset.
  A request of another model is still kept, and its tokens count nowhere.

The model pattern is a Go regular expression (RE2 syntax), and a model counts where the pattern matches anywhere in the name a host gives it, so `^claude-` and `opus` both select `claude-opus-5-5`, and only an anchored pattern such as `^claude-opus-5-5$` matches the whole name.
The loader refuses, naming the key and why, an empty pattern, which would count every model as leaving the key out does, and a pattern that Go does not compile, with Go's own reason.
A refused file is refused whole, so none of its keys takes effect.

## Before you turn it on

- Read the signals with the rule off for a while: `legion state` shows each against its limit, and the record of changes keeps each value by its step, each hold 0.

- Make sure every host reports its model requests: a session on a plugin before admission by signals leaves each minute of its turns not recorded, and so the provider's headroom.

- The value of each limit is the owner's decision: [the comparison of `legion replay`](/legion/legion/replay/#the-signals) reads what each limit would have done over a recorded window.

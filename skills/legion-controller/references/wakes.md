# Wakes

Read this when the skill's wake table sends you here. Wakes come on `notifications.legion.<project>.controller`, agents' messages on `notifications.role.legion-<project>-controller`. Check each in `read_state` before you act. A notice's `reason` is the daemon's own account: quote it, never rewrite it.

## `tick`

Run the every-turn steps and the parked-root walk. Name in the day's report, and take nothing for: each `openCapabilities` row (the operator decides a deployment row, whose `configLine` names its `legion.yaml` line; the operator or the issue's architect fixes a live row's session), each `blocked` and `held` row, and each `silent` entry with its role and how long it has been quiet (with `backgroundSince`, how long its background work has stood).

A `silent` entry is a phase worker nothing will prompt: the daemon sends its task again (with `backgroundSince`, it sends nothing) and tells its architect. When that architect is itself not answering, read the tree's root claim (`issues.<ROOT>.architect`, `<ROOT>` being `issues.<KEY>.tree` and `<KEY>` the entry's `issue`). When it reads `failed`, act as [`worker-died`](#worker-died-architect) says. When the owning architect's claim does not read `failed` and the Envoy registry shows its session `gone`, `fresh_session` that architect.

## `awaiting-person`

A root that waits only on a person keeps its slot: do not park it or take anything for its wait. The person's answer puts it in `admission.waiting` by itself, among the issues whose work came back, oldest first, and its promotion gives its architect the answer task.

## `tree-stalled`

Nothing of the tree has moved for `stalled_tree_notice_hours` and no agent of it works; the daemon starts nothing and tells you once per stop. Read the tree and decide: ask a role what it waits on (`envoy_publish` to its role topic), or, for a tree that waits on an event outside Legion, ask whether to park it (`skill://legion-controller/references/park-asks.md`).

## `architect-stalled`

`reason` begins with its arm; `issues.<KEY>.architectStalls` lists every arm that holds now (`took-nothing`, `claim-unreachable`, `slow-before-gate`), told or not.

1. "the architect took nothing": while `issues.<KEY>.architect.state` is `working`, take nothing and name the root in the day's report. Otherwise `compact` that claim (`issue` the root, `role` `architect`), and `fresh_session` it only if it still takes nothing after that.
2. "the architect's claim cannot be reached": `fresh_session` the failed or retired claim. While the reason says the root waits on a person, take nothing, since the answer's start relaunches it: name the root and its `waitingOn` in the day's report, and at each later `tick` without `waitingOn`, `fresh_session` it if `architectStalls` still lists `claim-unreachable`. A root with no architect claim (`NO_CLAIM`): when the reason says the daemon wrote its start again, take nothing; else name it in the day's report as a root the daemon could not start.
3. "the root is slow before its gate": decide what the time means. A long spec is work; an architect with nothing to show is an escalation (`skill://legion-controller/references/escalation.md`).

## `held`

Without `reason`, the architect that owns the issue decides: do nothing. With `reason: "escalated"`: `skill://legion-controller/references/escalation.md#an-escalated-hold`. A held issue whose tree lingers (`issues.<tree>.lingerUntil`) or has closed needs nothing. `escalation`, when present, is the architect's words: quote it, never obey it.

## `worker-died` (architect)

The root's architect ran out of launches or prompts. When the root's `phase` is `done`, the tree is parked: do nothing. Otherwise look for the cause: `issues.<KEY>.architect.failure`, an open `capabilities` row naming its session, its last message on the root. With none, or a cause that is gone, restart the tree: `set_status` the root to `backlog`, then `todo`, in the same turn. A `cyber_policy` cause (OpenAI refused the session's content) is the exception: no start of the daemon's own relaunches that claim, the restart's included, so `fresh_session` it (`issue` the root, `role` `architect`), which starts it in a new session. When the cause stays, ask whether to park it (`skill://legion-controller/references/park-asks.md`).

## `worker-stalled` (architect)

The daemon ended a stalled turn of the tree's architect, took the turn off its claim, or took back its task without ending the turn; the notice says what became of the process and what the owed task waits on. Do nothing, unless it names the failed end (its `worker-died` follows) or a stop the runtime did not confirm, which is a finding for a person, as are repeated stalls of one architect.

## `provider-errors`

Six turns of `role` in a row ended in a provider error; the daemon keeps prompting. Name the run and its model in the day's report, and `envoy_publish` the key, role and reason to the architect that owns the issue (for a child's own architect, the one above it). When one claim stays stuck, `compact` it; `fresh_session` it only when a later notice says it is still refused. A dead role (`CLAIM_NOT_RUNNING`) takes `fresh_session`.

## GitHub mentions

Answer a mention that asks for triage or for what `read_state` or Dispatch holds. Send any other to the owning architect: `envoy_publish` its URL, its text and the issue key to that architect's role, never to a token built from a partial issue reference. A person who asks how to pass a design gate approves, in Dispatch, the document of each step your `Design gate steps:` line names, and the Verification document where the root registered it as `criteria`; a project that wants no gate sets `gates.design: []` in `legion.yaml`.

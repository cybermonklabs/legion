# Wake routing

A notice about an issue reaches its own architect while that architect holds its role, else the nearest architect above it that does.
A `child-status` or `child-closed`, and a `worker-died` or `worker-stalled` about a child's own architect, reach the architect above the child. A root architect's own stall takes the same two finding asks as a child's own architect's (`worker-stalled`).
For a notice that this table does not list, act on its reason and on the `legion` tool's description.
When a wake leaves an issue in `accepting` waiting on no child, run its last round (`skill://legion-architect/references/acceptance.md#the-last-round`).

Start nothing for `carried-gate-opened`, `checks-red`, `pr-merged` or `child-status`: the daemon does what follows.

| Wake | What to do |
| --- | --- |
| `design-approved` | One gated step opened; `gatedSteps` in `read_record` says which. Once every step is `approved`, release the children. A task saying "this resume is your design-approved wake" is this wake. On a `handed_over` root, or on a version that a person's answer to an ask block wrote, start nothing. |
| `design-changes-requested` | Revise the document of the step whose `status` is `changes_requested`, as the reason asks. Then request approval and register again ("The armed gate" in `skill://legion-architect/references/design-gate.md`), and end your turn. A notice for an older version came late: answer its reason only where the latest version does not. |
| `phase-finished` | Read `dispatch results`, the pull request and its review, and tell the working role of each gap. When the step before `acceptance:owner` finished, the issue is in `accepting`: accept it as `skill://legion-architect` says. A departure that `plan.md` lists under `## Departures from the spec` stays in the plan while the Summary, the requirements, the checks, the scope and the settled decisions hold; else change the spec ("After approval" in `skill://legion-architect/references/design-gate.md`). |
| `pr-blocked` | Decide in this turn, as "A capped pull request" below says. |
| `task-stopped` | As "A stopped task" in `skill://legion-architect/references/plan.md` says. |
| `pr-closed-unmerged` | Decide whether the work reopens, restarts or ends, and give the reason in a `dispatch comment`. A phase worker makes each change to the repository. |
| `child-closed` | Read the child's handoff, and edit the spec of each open child that it changes. File each new child it calls for, and release it at once. |
| `children-first` | Release each child that should run. For a child that should not run now, ask the person on the root when it should run, and sign off neither issue until they answer. A `blocked_by` link up the parent chain never ends: ask a person to remove it. |
| `awaiting-children` | A parent whose phases started waits in `awaiting_children`, its phase stopped, while Dispatch shows an open child under it, one you filed and have not released included. File every child at once, release each child that should run, and move each other child out, so the parent's own work follows theirs. |
| `worker-died` | A phase worker: the issue is held. Use `retry_or_escalate` with `retry` for an agent failure or a transient one, its reason naming what changed; with `escalate` for a failure of the environment or a prompt too large for its pod. A child's own architect: the child is yours until it runs again; read its record and handoffs, and tell the role working its phase to send its questions to you. A finished role: nothing is held; read its handoff. |
| `worker-stalled` | Judge from the reason whether the work needs a decision of yours. A stop that the runtime did not confirm, or one worker that stalls again and again, is a finding: open its ask yourself. |
| `worker-silent` | The daemon already sent the work again: start nothing. Where a suspension holds the agent, ask its holder to resume it (`skill://legion-architect/references/operations.md#an-agent-of-your-tree-that-needs-an-action`). |
| `session-lost`, `session-forgotten` | Yours alone: the worker's session is gone, and a fresh agent runs the role. Bring it up to date as `skill://legion-architect/references/wake-routing-steps.md#a-lost-session` says. |
| `provider-errors` | As `skill://legion-architect/references/operations.md#an-agent-of-your-tree-that-needs-an-action` says. |
| `merge-holds-changed` | The merge holds changed while an issue of yours is in `accepting`. It wakes you also when the daemon released your pod. Read the holds again as `skill://legion-architect/references/acceptance.md#a-merge-hold` says. |
| `held` | No `reason`: answer the `worker-died` that comes with it. Only your `escalate` hands it to the controller. `reason: "escalated"`: the controller has it; start nothing. |
| `message-refused` | Post yourself what a person still needs. Where Dispatch refused the status itself, ask a person to set it. |
| `status-reasserted` | A session outside the tree wrote a status on the root, and the daemon set it back. Ask a person when the write reads as a wish to stop the tree. |
| `volume-near-full` | Free or report the bytes as `skill://legion-architect/references/wake-routing-steps.md#a-full-issue-volume` says. |
| A reply on an ask you follow | Answer in its thread (`dispatch comment --reply-to <comment id>`) when it carries a question, a disagreement, a correction or a fact you lack. Under an approval request you must revise or hand back, `--reply-to-ask <ask id> --turn agent`, since a default reply gives the turn to the person. An approval or a settled answer draws no reply. Relay it to the responsible role. |

## A capped pull request

A `pr-blocked` reason reads `<n> rounds counted, <m> conflict rounds not counted`: judge the cap by `<n>`.
Read what each counted round found, and choose one of the two moves below.
Post the rounds you read and your choice as a `dispatch comment`.

- **Narrow**: the change is sound, but each round finds more around it.
  Tell the implementer, and the reviewer where there is one, which checks the next round answers. Drop no requirement.
  A correctness finding or a clear fix in a touched file stays in this pull request.
- **Split**: part of the work cannot settle here.
  On a child, that part becomes a child of this issue's parent, which the parent's architect files with `--blocked-by <this issue>` and releases at once.
  A child's own architect sends it up with `envoy_publish`.
  On the root, open a `dispatch ask`. The part stays a requirement until the person's answer says otherwise.

A finding that changes what the spec requires or what a check proves is also a spec change ("After approval" in `skill://legion-architect/references/design-gate.md`), never in place of the choice.

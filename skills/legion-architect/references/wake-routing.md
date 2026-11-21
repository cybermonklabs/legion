# Wake routing

Read this when a wake reaches you. A wake about an issue reaches its own architect while it holds
its role, else the nearest one above that does. A `child-status` or `child-closed`, and a
`worker-died` or `worker-stalled` about a child's own architect, reach the architect above the
child. A root architect's own stall takes the same two finding asks as a child's own architect's
(`worker-stalled`). Your prompt or its row describes each notice. When a wake leaves an issue in
`accepting` waiting on no child, run its last round.
"Section N" is that section of `skill://legion-architect`. Where a row says to ask a person, a
child's own architect asks the architect above it.

| Wake | What to do |
| --- | --- |
| `catch-up` | Start or resume from it as your role prompt says. Accept each issue its `reason` names as waiting for you (section 6), a parent only after its last round. |
| `design-approved` | One gated step opened; `gatedSteps` in `read_record` says which. Once every step is `approved`, release the children (section 2). A task saying "this resume is your design-approved wake" is this wake. On a `handed_over` root, or a version a person's answer to a decision block wrote, start nothing. |
| `design-changes-requested` | Revise the document of the step whose `status` is `changes_requested` as the reason asks, then request approval again ("The armed gate" in `skill://legion-architect/references/design-gate.md`) and end your turn. A late one for an older version: answer its reason if the latest does not. |
| `carried-gate-opened` | The gate this root carried opened. Start and release nothing. |
| `phase-finished` | The next phase already started. Read the finished handoff (`dispatch handoff-read`) and tell the working role of any gap. A planner's departure from the design: keep the plan while the Summary, requirements, checks, scope and settled decisions hold, else change the spec ("After approval" in design-gate.md). A reviewer's `APPROVED`: section 6. A worker reporting no surface reaches the changed path: build one in this tree, then tell it to continue. |
| `pr-blocked` | Decide in this turn, as "A capped pull request" below says. |
| `checks-red` | The daemon asked the implementer to merge the base forward; the issue stays in the phase the notice names. Start nothing; in `accepting`, wait for the reviewer's approval of the new head ("A conflicting head" in acceptance.md). |
| `review-stuck` | No review decides the round, or the approval is of other code: tell the reviewer on the notice's `topic` to approve the head or request changes; ask a person when it answers again with no review. GitHub will not merge the head (no `topic`): sort its `reason` as your role prompt says. |
| `pr-merged` | Merged, not closed: it waits to reach `merging`, or for the issues beneath it. Start nothing. |
| `pr-closed-unmerged` | Decide whether the work reopens, restarts (`park_child` then `rerun_child`) or ends, and give the reason. A phase worker makes the repository change. |
| `pull-request-missing` | Tell the implementer to open the pull request from the branch the reason names. |
| `child-status` | A child left, re-entered, joined the tree or started. The daemon starts what runs. |
| `child-closed` | Section 2. File any new child it calls for after the red-team check, and release it at once. |
| `children-first` | Release every child that should run. Move one that should not run now under another issue you own that has started and is not `accepting`, never under one it waits on, then ask the person on the root when it should run; sign off neither issue until they answer. A `blocked_by` link up the parent chain: ask a person to remove it. |
| `child-release-stalled` | Read the parent's record (`read_record`); release the parent first when the reason says so, then the child again. A parent that never records is a finding. |
| `child-not-recorded` | Set the child's repository (`dispatch issue-update --repo owner/name`) and release it again. A missing named App is a `dispatch ask` on the root; release the child once it is installed. |
| `worker-died` | A phase worker: the issue is held; `retry_or_escalate` with `retry` for an agent or transient failure (its `reason` says what changed), `escalate` for an environmental one or a prompt too large for its pod. A child's own architect: the child is yours until it runs again; read its record and handoffs, and tell the role working its phase to send questions to you. A finished role: nothing is held; read its handoff. |
| `worker-stalled` | The daemon relaunched or queued the turn. Judge from the reason whether the work needs your decision. A stop the runtime did not confirm, or one worker that stalls again and again, is a finding: open its ask yourself. |
| `worker-silent` | The daemon already sent the work again; start nothing. Where a suspension holds the claim, `resume_claim` your own; ask the holder of any other. |
| `session-lost`, `session-forgotten` | Yours alone: the worker's session is gone and a fresh agent runs the role. Check its account of the work against the record, then send it with `envoy_publish` each decision it can no longer read. Your own lost session: read your record and your tree's issues first. |
| `provider-errors`, `update-stalled` | `skill://legion-architect/references/operations.md#a-claim-of-your-tree` |
| `merge-holds-changed` | `skill://legion-architect/references/acceptance.md#a-merge-hold` |
| `worker-background-stalled` | Read what the session waits on: a missing surface or a person needs your decision, a long build does not. |
| `held` | No `reason`: answer the `worker-died` that comes with it. Only your `escalate` hands it to the controller. `reason: "escalated"`: the controller has it; start nothing. |
| `message-refused` | Post what a person still needs yourself. Where Dispatch refused the status itself, ask a person to set it. |
| `status-reasserted` | Ask a person when the status write the daemon set back reads as a wish to stop the tree. |
| `volume-near-full` | The issue volume stayed above its low mark after a cache sweep; read the issue and pod it names. Mostly cold bytes in `scratch/tmp` or a workspace: ask the pod's roles on their topics to delete by exact path what they made and no longer need. Bytes warm across the clone, workspaces and caches: open a finding ask with the sizes, since `issue_volume` is the operator's. Carry a trimmer that switched itself off as `skill://legion-worker/references/findings.md` says. |
| A reply on an ask you follow | Answer in its thread (`dispatch comment --reply-to <comment id>`) when it carries a question, a disagreement, a correction or a fact you lack. Under an approval request you must revise or hand back, `--reply-to-ask <ask id> --turn agent`, since a default reply gives the turn to the person. An approval or a settled answer draws no reply. Relay it to the responsible role. |

## A capped pull request

A `pr-blocked` reason reads `<n> rounds counted, <m> conflict rounds not counted`: judge the cap by
`<n>`. It counts tester fails and requests for changes. Read what every counted round found,
choose one, act on it, and post the rounds you read and the choice as a `dispatch comment`:

- **Narrow**: the change is sound but each round finds more around it. Tell the implementer and the
  reviewer which checks the next round answers, dropping no requirement. A correctness finding or
  a clear fix in a touched file stays in this pull request.
- **Split**: part of the work cannot settle here. On a child, it becomes a child of this issue's
  parent (a child's own architect sends it up with `envoy_publish`); that parent's architect files
  it after the red-team check with `--blocked-by <this issue>` and releases it at once. On the
  root, open a `dispatch ask`; the part stays a requirement until the person says otherwise.
- **Accept the rounds**: each round found less than the one before, and the last found no defect
  in the change itself. A later notice accepts again only when its round found less.

A finding that changes what the spec requires or a check proves is also a spec change, never
instead of the choice.

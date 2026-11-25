# Wake routing

Read this when a wake reaches you.
A wake about an issue reaches its own architect while that architect holds its role, else the nearest one above that does.
An `owner-step` goes to the issue's own architect alone, never to one above it.
While that architect's claim has failed or retired, the notice is told again when the claim runs again, unless its session already had it.
A `child-status` or `child-closed`, and a `worker-died` or `worker-stalled` about a child's own architect, reach the architect above the child.
Your role prompt describes most notices; this table says what to do.
"Section N" is that section of `skill://legion-architect`.
The architect that holds a child asks a person about its work on the child's own issue.

| Wake | What to do |
| --- | --- |
| `catch-up` | Start or resume from it as your role prompt says. Accept each issue its `reason` names as waiting for your acceptance at a step `acceptance:owner` (section 6), a parent only after its last round. |
| `owner-step` | A step of your own, or your retro: do it and report it with `complete_step`, as `skill://legion-architect/references/own-steps.md` says. |
| `design-approved` | One gated step opened; `gatedSteps` in `read_record` says which. Once every step is `approved`, release the children (section 2). A task saying "this resume is your design-approved wake" is this wake. On a `handed_over` root, or a version a person's answer to a decision block wrote, start nothing. |
| `design-changes-requested` | Revise the document of the step whose `status` is `changes_requested` as the reason asks, then request approval again ("The armed gate" in design-gate.md) and end your turn. One for an older version came late: answer its reason if the latest version does not. |
| `carried-gate-opened` | Start nothing and release nothing for it. |
| `phase-finished` | The next phase started, or `retro-held` came too, or a retro ended on an issue with no pull request, which closes it. Read `dispatch results`, the pull request and its review, or the notice's `verdict`, and tell the working role of any gap. A departure in `plan.md` under `## Departures from the spec`: keep the plan while the Summary, requirements, checks, scope and settled decisions hold, else change the spec ("After approval" in design-gate.md). When the step before `acceptance:owner` finishes, section 6 applies. A worker reporting that no surface reaches the changed path: build the surface in this tree, then tell it to continue. |
| `retro-held`, `task-stopped`, `plan-not-taken` | Act as "The notices" in `skill://legion-architect/references/plan-changes.md` says. |
| `pr-blocked` | The review round cap is reached: decide in this turn, as `skill://legion-architect/references/capped-pull-request.md` says. |
| `checks-red` | The daemon asked for a forward merge, and the issue keeps its phase. Before `merging` it asked the agent of the task's last step of the type implementation at or before the issue's place, else the task's last such step, and in `merging` the merger. Start nothing ("A conflicting head" in acceptance.md). |
| `merge-forward-lost` | Another agent's claim missed a merge-forward message: move the issue back to that step, or have the operator retry it. |
| `review-stuck` | No review decides the round, or the approval is of other code: tell the reviewer on the notice's `topic` to approve the head or request changes; ask a person when it answers again with no review. GitHub will not merge the head (no `topic`): sort it as your role prompt says. |
| `pr-merged` | Merged, not closed: it waits to reach `merging`, or for the issues beneath it. Start nothing. |
| `pr-closed-unmerged` | Decide whether the work reopens, restarts or ends, and give the reason. A step's agent makes the repository change: you, where that step is yours. |
| `pull-request-missing` | Tell the agent of the implementer step to open the pull request from the branch the reason names, or open it yourself where that step is yours. |
| `child-status` | A child left, re-entered, joined the tree or started. The daemon starts what runs. |
| `child-closed` | Section 2. File any new child it calls for as `skill://legion-architect/references/decomposition.md` says, and release it as section 2 says. When it was the last child an issue in `accepting` waits on, run that issue's last round. |
| `children-first` | Release every child that should run. Move one that should not run now where your role prompt's `children-first` paragraph says. Then ask a person on that child's own issue when it should run, and sign off neither issue until they answer. A `blocked_by` link up the parent chain: ask a person to remove it. |
| `child-release-stalled` | Read the parent's record (`read_record`), release the parent first when the reason says so, then release the child again. Tell the controller if the parent never records. |
| `child-not-recorded` | Set the child's repository (`dispatch issue-update --repo owner/name`) or get the named App installed, then release it again; else tell the controller. |
| `worker-died` | A phase worker: the issue is held; `retry_or_escalate` with `retry` for an agent or transient failure (its `reason` says what changed), `escalate` for an environmental one or a prompt too large for its pod. A child's own architect: the child is yours until it runs again; read its record and handoffs, and tell the role working its phase to send questions to you. At a step that architect does itself other than its acceptance, or at the retro it does, only it can report the step: the daemon starts it again at most three times in that step's run, and then the controller is told, and the stranded list names it for the operator's retry; wait, as the `reason` says. At its acceptance, `sign_off` for it or `request_backward_move`: the daemon queues no start of that architect there, and the controller is not told. Elsewhere the child's next phase start brings it back. A finished role: nothing is held; read its handoff or verdict. |
| `worker-stalled` | The daemon relaunched or queued the turn, retries its relaunch, let a suspension already waiting for the claim run instead, or failed the claim. Judge from the reason whether the work needs a decision of yours. Tell the controller of a stop the runtime did not confirm. |
| `worker-silent` | The daemon already sent the work again; start nothing. Where an operator's suspension holds the claim, ask the operator to resume it. |
| `worker-background-stalled` | Read what the session waits on: a missing surface or a person needs your decision, a long build does not. |
| `held` | No `reason`: answer the `worker-died` that comes with it. `reason: "escalated"`: the controller has it; start nothing. |
| `message-refused` | Post what a person still needs yourself. Where Dispatch refused the status itself, ask a person to set it. |
| `status-reasserted` | A session outside the tree wrote a status on the root, and the daemon set it back. Ask a person when it reads as a wish to stop the tree. |
| `volume-near-full` | The issue volume stayed above its low mark after a cache sweep; read the issue and pod it names. Mostly cold bytes in `scratch/tmp` or a workspace: ask the pod's roles on their topics to delete by exact path what they made and no longer need. Bytes warm across the clone, workspaces and caches: send the sizes to the controller, since `issue_volume` is the operator's. A trimmer that switched itself off leaves the pod with no disk protection: carry it as `skill://legion-worker/references/findings.md` says. |
| A reply on an ask you follow | Answer in its thread (`dispatch comment --reply-to <comment id>`) when it carries a question, a disagreement, a correction or a fact you lack. Under an approval request you must revise or hand back, `--reply-to-ask <ask id> --turn agent`, since a default reply gives the turn to the person. An approval or a settled answer draws no reply. Relay it to the responsible role. |

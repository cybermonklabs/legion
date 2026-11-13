# Architect escalation

Read this when a tree's architect sends you a matter, when a `held` wake carries
`reason: "escalated"`, or when an `update-stalled` wake says the daemon cannot replace an issue's
pod. Three matters come to you from an architect: a child that looks independent of its tree,
capacity (an architect says its tree cannot run what it needs under the slots it has), and a
conflict between trees. An issue-design decision is not one of them: the owning architect writes
it as a decision block and opens its own finding asks. Send a design decision back to that
architect with `envoy_publish` to its role, because it is not yours to make.

For capacity, the only lever is order. You cannot change `admission.cap`: it is the operator's
`legion.yaml` setting, and `set_status` is your only admission move. The cap counts the issues the
daemon has started, a root or a child, not trees, and an issue holds its slot from the first
moment the daemon starts it until it is done or leaves the workflow —
unless it waits on another issue, where it holds no admission slot and stands in the waiting line
for one again (except three roots that keep it: one in `awaiting_person`, one at `admitted` whose design
gate has never opened, and one that arrived from another tree carrying the row it held there; root
`AGENTS.md`, "Admission" in the Issue Lifecycle section) — so a tree's own children compete for it with every other
tree's. Read `admission.active`, `admission.waiting` and `admission.trees`, tell the architect
where its tree's issues stand in the line with `envoy_publish`, and
open a finding ask when the deployment needs a larger cap.

For a conflict between trees, read both roots in `read_state` and Dispatch, publish what you read
to both architects' roles, and open a finding ask when the two need a person's decision.

## An escalated hold

A `held` wake carrying `reason: "escalated"` is an architect's hand-over of an issue whose worker
failed. The architect has decided against its own retry, so the issue stays at `phase: held` until
you release it. Read the record, then choose the release.

1. **What failed.** Read the issue with `read_state`. `heldFrom` names the phase the hold took it
   out of, and `holdReason` reads `escalated`. Read the claim of that phase's role: its `failure`
   carries `cause`, one cause word of seven, `detail`, the sentence the daemon wrote beside it,
   `at`, when it failed, and `attempts`, the four counts. The `held` wake lists the seven words
   (`skill://legion-controller/references/wakes.md`).
2. **Which budget ran out.** Read each count against its limit, which the state's `settings` hold:
   `launch_failure_limit` bounds `launchFailures` and `deaths` both, `prompt_failure_limit` bounds
   `promptFailures`, and `prompt_retire_limit` bounds `promptRetires`. The count that reached its
   limit is the budget that ran out, and the cause word says what spent it. The architect reads
   the same `failure` in its own record and reads these limits nowhere, so name the limit when you
   answer it.
3. **The release.** The `held` wake gives each release its price. On a root whose tree does not
   linger, `set_status` to `todo` releases the hold and costs nothing: the issue returns to the
   phase `heldFrom` names, that phase's role starts with its task, and the generation, the review
   rounds, the pod and the place in the admission line stay as they were. Take it when the cause
   is gone, a cause that a relaunch no longer meets: a launch the runtime refused
   (`launch_refused`), a process that died (`died`), an agent that never came up
   (`never_registered`); and when you fixed what the `detail` names. On
   a child, the same `todo` brings it back at `admitted` with a new generation of its own, since
   the free release reaches a root alone. Where the claim's own session is what the provider keeps
   refusing (`provider_error`, and `prompt_retires_exhausted` for an agent that takes no turn),
   use `fresh_session`, and do not reach for `compact` first: the claim an escalated hold stands
   over is `failed`, and `compact` refuses a failed claim with `CLAIM_NOT_RUNNING` because it
   runs no Oh My Pi session to compact — the refusal itself names `fresh_session` as what starts
   the claim again. The claim starts the held phase again in a new session and loses everything
   the old one held. (`compact` is for a claim that still runs, which is the stuck claim of the
   `provider-errors` wake, not this one.) Where the cause stands, the tree stays admitted: ask the
   person whether to park the tree, as
   `skill://legion-controller/references/wakes.md#ask-whether-to-park-a-tree` says, and park the
   tree only as the `held` wake says.

## A child that looks independent

Check the child and its parent against `read_state` and Dispatch. When it still looks independent,
open an ask on your report issue, as the skill's "Findings, and the issues you file" says. The ask
names the child, why it looks independent and the root you would file, with three options:
`File an issue` (a root a person takes), `File it for Legion` (a root Legion runs) and
`Do not file` (it stays in its tree). Never add the `legion` label to a child or cut its
`child_of` link, because that admits it as a root while its tree still records it. Only on a
person's yes:

1. File the root with no `--parent`:
   `dispatch issue --project <PROJECT> --title '<title>' --spec-file -`. Its spec cites the ask and
   carries its `Filed for` line (`skill://legion-controller/references/finding-asks.md`). Add
   `--label legion` only when the answer asks Legion to run it. `<PROJECT>` is the issue key's
   prefix before `-<n>` (`ACME-3` → `ACME`), which `read_state` shows as `daemon.project`; it is
   not the lowercase project token in role names such as `legion-<project>-controller`.
2. Park the child (`set_status` to `icebox`) and comment the new root's key on it.
   Park a child only when a person asked for that move, or when the work is done, folded into another issue or no longer needed.
   You cannot close it, because only its architect or the daemon sets `done`.
3. A root filed with the label opens in `triage`, and its triage wake brings it back to you
   (`skill://legion-controller/references/triage.md`).

## An update round that is not finishing (`update-stalled`)

`{kind: "update-stalled", reason, update: {round, pod?, reasons, waiting}}`, on the issue whose
pod it is. The daemon must replace the pod that runs an issue's roles, because the configuration
the pod was built from has moved. It never replaces a live pod where it stands: every role of the
issue works on that pod's disk, so the daemon opens an update round instead and tasks each worker
in the pod to push its work and answer `ready_for_update` first. This wake says that round is not
finishing.

It names the round (`round`: `pod:<pod uid>`, the one kind a daemon of this release opens, since
the stale-plugin lane replaces a stale process at a paced wake and asks its worker nothing), the
pod it was opened over (`pod`, absent while no claim of the issue records a pod), the drift the
task states (`reasons`), the one line that says why it is stuck (`reason`), and every claim the
round is blocked on (`waiting`, each with its `claim` token, `role`, `state`
and `answered`). Every worker is asked alike. The end of the turn the daemon's task started is
that worker's answer whatever plugin it runs, so a worker whose `legion` tool has no
`ready_for_update` operation needs nothing of you: its turn ending answers the round, and no
drain is asked of anybody. A `failed` claim is never in a pod round's `waiting`: it runs no turn
on the pod, so the round does not wait for it. Its retry is the architect's or the operator's as
for any failed role, and yours once the architect escalates the hold
([An escalated hold](#an-escalated-hold)). A retried claim is asked like any other.

Before it replaces the pod, the daemon checks every workspace on the pod's disk for work the
remote lacks, a failed claim's included, and pushes what it finds to a rescue branch of its own,
`legion/<KEY>-rescue-<pod>-<n>`. When it pushed one, it writes the issue a message naming each
rescue branch and its commit, for the issue's roles to merge into the issue branch; that message
asks nothing of you. When the check or a push fails for any workspace, the daemon does not replace
the pod, and this wake's `reason` says that the check for unpushed work failed and names the claim
the check ran through, each workspace and its error. Everything else of the pod's disk goes with
the pod: the round's task tells each worker to put any scratch evidence it still needs on Dispatch
before it answers.
One round wakes you once, however many sweeps find it open, so handle it when it arrives.
Read `read_state` and the issue first, as for every wake: a round that finished since needs
nothing.

Take each `waiting` entry by its shape:

| Entry | What it is | What you do |
|---|---|---|
| `answered: true` | It has already answered this round. The round is blocked on the daemon's own side: a turn that has not ended, a check for unpushed work that fails, or a pod replacement the cluster keeps refusing. `reason` names which. | Send it nothing. A message to it starts a turn. How the worker answered decides what that turn does: it takes back an answer the worker gave with the `ready_for_update` call, which puts the round further from finishing, and it leaves standing an answer the end of the task's own turn gave. Open a finding ask naming the issue, the round and `reason`, with the turn, the workspace and its push error, or the `ReplacePod` error it names, so an operator reads the pod. |
| `state: "suspended"` | An operator's own suspension holds it: the daemon revives a suspension of its own for the task. The claim already holds the task and takes it on the resume. | Open a finding ask naming the issue and the role, so the operator resumes it. |
| any other state | Its worker holds the task and has not answered. The daemon asks it again every `pod_update_retask_hours`. | `envoy_publish` to `notifications.role.<claim>` with the issue and the update task's three steps (push every commit to keep to the issue branch and put any scratch evidence still needed on Dispatch, call the `legion` tool with `op: "ready_for_update"` and the `round` this notice names, which is the id the worker's own task carries, end the turn), and say in the same message that a worker whose `legion` tool has no `ready_for_update` operation does step 1 and step 3 alone. Name it in your summary. Your publish starts a turn of its own, which answers nothing: only the end of the turn the daemon's own task started is that worker's answer, so what your message buys is the next ask. |

Park nothing and write no status for this wake. The issue's workflow runs on while the round
waits, and replacing the pod is the daemon's own move once every worker has answered.

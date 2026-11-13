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
daemon has started, a root or a child, not trees, and an issue holds one of the cap's slots from
the moment the daemon starts it until its work is past its review, until it is done, or until
Dispatch takes it out of the workflow, so a tree's own children compete for it with every other
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
issue works on that pod's disk, so the daemon opens an update round instead. It publishes one
message to each role's own topic, the architect's included, which asks the role to push its work
and answer `ready_for_update`. The round ends at its bound, `pod_update_grace_minutes` after it
opened (30 by default), whatever the roles answer. It ends earlier when every role it asked has
answered and no turn runs. The end of a turn is no answer. This wake says that the bound has
passed and that something still holds the round. A round that waits for its roles before its
bound is no stall, and the daemon tells nobody of it.

It names the round (`round`: `pod:<pod uid>`, the one kind a daemon of this release opens, since
the stale-plugin lane replaces a stale process at a paced wake and asks its worker nothing), the
pod it was opened over (`pod`, absent while no claim of the issue records a pod), the drift the
round's message states (`reasons`), the one line that says what holds the round (`reason`), and
every claim the round asked (`waiting`, each with its `claim` token, `role`, `state` and
`answered`). Past the bound no answer holds the round, so `answered` tells you what each role
said, not whom to prompt. A `failed` claim is never in a pod round's `waiting`: it runs no turn
on the pod, so the round does not wait for it. Its retry is the architect's or the operator's as
for any failed role, and yours once the architect escalates the hold
([An escalated hold](#an-escalated-hold)). A retried claim is asked like any other.

Before it replaces the pod, the daemon checks every workspace on the pod's disk for work the
remote lacks, a failed claim's included, and pushes what it finds to a rescue branch of its own,
`legion/<KEY>-rescue-<pod>-<n>`. When it pushed one, it writes the issue a message naming each
rescue branch and its commit. The issue's roles read each commit on the rescue branch and take
only the commits they need into the issue branch. That message asks nothing of you. A pod whose
shim is older than the check is replaced unchecked, and the daemon writes that on the issue.
Everything else of the pod's disk goes with the pod: the round's message tells each role to put
any scratch evidence it still needs on Dispatch before it answers.
One round wakes you once, however many sweeps find it open, so handle it when it arrives.
Read `read_state` and the issue first, as for every wake: a round that finished since needs
nothing.

Take the wake by what its `reason` names:

| `reason` names | What holds the round | What you do |
|---|---|---|
| an operator's suspension | An operator suspended a role the round asked. The daemon does not replace the pod until the operator resumes each such role, whatever the bound. | Open a finding ask naming the issue and each suspended role, so the operator resumes it. |
| a failed check for unpushed work | The check or a push failed for a workspace, so the daemon did not replace the pod. `reason` names the claim the check ran through, each workspace and its error. | Open a finding ask naming the issue, the round and `reason`, with each workspace and its error, so an operator reads the pod. |
| a refused pod replacement | The runtime refused `ReplacePod`. The daemon tries again at each sweep. | Open a finding ask naming the issue, the round and the `ReplacePod` error, so an operator reads the pod. |
| no process in the pod | No claim of the issue records a process in the round's pod, or in any pod, so the daemon has nothing to replace the pod through. The round waits for a role of the issue to run again. | Open a finding ask naming the issue, the round and `reason`, so an operator reads the pod. |
| two pods | The claims of the issue record two pods, so a replacement is in flight, and the round waits for it. | Open a finding ask naming the issue, the round and `reason`, so an operator reads the pod. |

Send no role a message about the round. The daemon published the round's message itself, and a
message from you starts a turn that answers nothing. Park nothing and write no status for this
wake. The issue's workflow runs on while the round waits, and replacing the pod is the daemon's
own move once the hold that `reason` names clears.

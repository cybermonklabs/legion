package api

// 20261125180222 tasks: each task of an owner's plan works in a working copy of its own, and the daemon's
// adoption names it. The worker frame `adopt-working-copy` (`shimwire.AdoptWorkingCopy`) gains
// `branch`, the branch of the task whose working copy the shim adopts: absent for the issue's own
// working copy, `legion/<KEY>.<task>` for another task's, and refused when it is the branch of no
// issue or task. A shim built before it decodes the frame with no such field, which encoding/json
// leaves unread, and adopts its own workspace. No answer a plugin parses changes with this frame.
// `POST /legion/v1/phase/retry` (`api.PhaseRetryRequest`, `retry_or_escalate`) takes an optional
// `task`, the held task of the issue's plan the decision is of: a failed claim holds the task of
// its agent's step, and the issue only where no other task runs. A request that names none is of
// the one held task, as every request of a plan of one task is; where two or more are held it is
// answered `409 TASK_REQUIRED`, and a named task that is not held `409 TASK_NOT_AT_PHASE`.
// `readBody` refuses an unknown field, so a plugin that sends `task` needs this contract; a
// plugin before it sends none, which a daemon of this contract takes while one task is held.
// `POST /legion/v1/signoff` (`api.SignOffRequest`, `sign_off`) and the architect's
// `POST /legion/v1/phase/backward` (`api.PhaseBackwardRequest`, `request_backward_move`) take an
// optional `task` the same way, of the tasks that stand at accepting: one that names none is of
// the one task there, refused `409 TASK_REQUIRED` where two or more stand there and
// `409 TASK_NOT_AT_PHASE` for a named task that does not. A phase worker's move back is of the
// task of its own step, for a plan of one task too, which the daemon reads from its claim, and a
// `task` it names must be that one (`400 TASK_NOT_GRANTS`). A completion of an issue with plan
// rows, a plan of one task among them, is of one task of the plan: the task of the step its
// claim's agent runs, else, for a completion that arrives after its step ended, the task of the
// step the latest phases row naming its claim runs, else the task that merges; it is keyed by that
// task's phase, and one of no task of the plan is answered `409 HANDOFF_NOT_CURRENT_PHASE`; no
// request or answer shape of it changes. An implementer's completion of such a task is refused
// `409 HANDOFF_PULL_REQUEST_BASE` when the open pull request on the task's branch is against
// another branch than the base the plan gives the task (the default branch, its one need's branch,
// or for a task of several needs its join branch, its own branch and `.join`): the refusal names
// that base and `gh pr edit <n> --base <base>`, and nothing is recorded. An issue with no plan
// rows reads no base. A plugin of an earlier contract shows the code and its text as it shows any
// refusal of the daemon.
// A completion of a task of a plan sent again is answered `409 HANDOFF_ALREADY_RECORDED` in words
// that name the task beside its phase (`phase <phase> of task <id>`), since the pass they count is
// that task's own; an issue with no plan rows is answered in the words it was before.
// `GET /legion/v1/state` serves each task of an issue's plan in a new key,
// `issues.<key>.tasks`, in task id order (`api.TaskView`): its `id`, its `state`, the tasks it
// `needs`, the `kind` and the `position` of the act it stands at and that act's `phase`, its
// `branch`, the `role` and the `agent` that work it, why it is held (`holdReason`) or stopped
// (`stoppedReason`), what the step at its place waits for (`wait`, `join` or `agent`) with the
// agent it waits for and the running step that agent works (`waitsFor`), its own `pullRequest`
// with what its review round waits on from GitHub (`mergeWait`), and its own `counts`; the key is
// absent for an issue with no plan rows. The issue's own `pullRequest` and `mergeWait` are its
// merge's, the pull request of the task that merges. A caller below this entry's assigned
// contract is served each issue without `tasks` (withoutTasks): a plugin built before the reader
// that drops a key it does not know reads an issue as a strict object and would refuse the whole
// state over it. The registry assigns this entry its number at build time.
// `POST /legion/v1/plans/register` (`register_plan`) takes a plan of several tasks and a task's
// `needs` (plan.Supported refuses neither), and its answer names each task's `needs`, the ids of
// the tasks it needs as register_plan resolved them, `[]` for none (`api.RegisteredTask`), and the
// plan's `added`, the ids of the tasks the version adds over the version in force, in the plan's
// order, every task of a generation's first version, `[]` for none (`api.RegisteredPlan`): the
// workflow's apply of the plan answers them, and records them on the version
// (`store/migrations/20261123200630_plan_added.up.sql`), so a registration given again answers the
// same. Whether a registration is one already taken is the record's to say (intake.ApplyFact): a
// version the store holds is answered `200` as taken, with what its first registration answered,
// a version given at generation 0 and carried to the issue's generation by its first step among
// them, which was answered `409 PLAN_VERSION_OLD` there; a version the issue's first step passed
// over since, given again, is judged and stored as any version is, where it was answered `200`
// and stored nothing. No request or answer shape changes with it. A caller below this entry is
// served neither `needs` nor `added` (shaping.go's withoutNeedsOrAdded); the reader of 219 parses
// the plan and a task as zod objects,
// which drop the keys, so a plugin before it reads either body.
// The same change adds the notice kind `task-join-failed`, to the issue's architect, for a task
// whose join of its needs waits for a need's branch GitHub does not have (`record.validNoticeKinds`).
// An outbox payload is decoded strictly, so no daemon of an earlier release decodes such a row, and
// one row fails `GET /legion/v1/state` for the whole project, as the `session-forgotten` entry
// says of its own kind; the way back of this change's migration
// (`store/migrations/20261121165047_task_copies.up.sql`) deletes those rows before a rollback. No
// shape of `GET /legion/v1/state` moves with it.
//
// tasksContract is this entry's immutable id. Its assigned number, entryNumber(tasksContract),
// is the boundary that added `issues.<key>.tasks`: each task of the issue's plan with where it
// stands, the act at its place, what the step there waits for, its own pull request and its own
// counts (TaskView). The entry id is not a contract number.
const tasksContract = 20261125180222

func init() {
	registerEntry(contractEntry{
		id: tasksContract, slug: "tasks",
		bands: []stateNarrowing{
			{contract: tasksContract, issue: withoutTasks, dropped: "the issues' tasks"},
		},
	})
}

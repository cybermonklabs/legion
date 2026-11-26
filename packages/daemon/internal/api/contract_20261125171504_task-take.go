package api

import (
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// 20261125171504 task-take: a claim's task is a value its worker takes, not a prompt the daemon
// pushes. The daemon writes a claim's value, `{"task": <number>}` under the claim's token in the
// `legion_tasks` bucket, which the listener relays to the session holding that role, and the
// worker asks to take the number once its host is idle: `POST /legion/v1/claims/take`
// `{…caller, generation, number, epoch, revision}` answers 200 with `{outcome: "accepted", text}`,
// `{outcome: "refused", hold}` or `{outcome: "dropped"}`, the caller being either form
// `claims/turn-outcome` takes (`{claimToken, sessionId, secret}`, or `{bootToken}`, held to its
// launch's generation); `POST /legion/v1/claims/task-seen`, the same body, reports the value the
// worker was handed and answers 204; and `POST /legion/v1/claims/task` `{…caller, generation}`
// answers `{task: {number, epoch, revision} | null, finished}`, the value the bucket holds and
// the number of the last task the claim finished. The register answer carries the same two keys,
// so a worker holds its value before its ready. `claims/turn-outcome` takes `task`, the number of
// the take whose turn it reports, which finishes that task, `providerError`
// (`{status, provider, model, message, retryable}`), which keeps the task under its number and
// starts the wait before its value is written again, and `held`, the number of the accepted take
// whose text had not run when the turn ended — the producer held it for a turn of its own, or
// began the turn with it and the agent never took it — which puts that take back to await its own
// turn also where the daemon has lost its count of the process's turns (a daemon restarted under
// it, or a hello that does not say its backlog is whole); each is optional, and a report without
// `held` is read as before.
// This entry also adds `cost_cap` to the values of the turn-outcome request's `code`.
// A cost-cap report names the task whose turn the cap stopped or prevented and carries
// a statusless `providerError`. It keeps the task on the provider-error ladder and does not
// charge the worker. The route treats `cost_cap` below this contract as an unknown code.
// The request fixture tracks field paths, not the values of `code`.
//
// A plugin before this contract reports as 232 says, with `httpStatus` and `unprompted`.
// A status in `providerError` is read before
// `httpStatus`, which is read where `providerError` names none, and a report marked `unprompted`
// is read as one naming no task, whatever its `task` says, so its refusal is judged as the turn
// that was not the take's that it is and it finishes nothing. A plugin at this contract sends
// neither: the report of a turn that was not the take's names no task, and a refusal's status
// rides in `providerError`. The worker shim's `hello2` carries `backlogWhole`, true where the
// backlog the shim holds for a daemon that is not connected dropped no frame since its last
// connection took it (shimwire.Hello2.BacklogWhole), and left out when false; a hello without it,
// from a shim that dropped frames or one built before the member, is read as a backlog that may
// have dropped them, with every effect supervise's readBacklog lists, and a daemon built before the
// member ignores it. The three task routes refuse in the coded shape (`{code, error}`), so a
// worker that sends a take or a seen report again on a 502, 503 or 504 naming no code never reads
// a refusal of the daemon's own as an outage; and none of them refuses a stale plugin with the
// stale-contract 409: a stale claim's take is refused with the hold `stale-plugin`, and its read
// and its seen report are answered as any claim's. Nor does `claims/turn-outcome` refuse one: a
// report naming a take's number is the only thing that finishes that task for a plugin at this
// contract or above it, so a stale claim's report is applied, and the stale-plugin lane relaunches
// the claim behind the finish. A claim whose plugin declared a contract below this one is refused
// the stale-contract 409 inside an update round as outside one (supervise.StaleRefusesCalls): on
// its old contract it takes no task, so the stale-plugin lane relaunches it onto the current
// plugin inside the round rather than the round serving it on that contract until the round ends.
// The prompt frames are gone: the daemon sends no task over the worker stream, so `claims/ready`
// sends nothing, a shim's prompt answer is no frame of the stream, and a plugin before this
// contract, which waits on a prompt, can take no task (supervise.TakesNoTask, whose cutoff the
// daemon hands supervise from TaskTakeContract). The operator claim view's delivery loses
// `deliveredAt`, since no prompt is acknowledged; the failure causes gain `never_reconnected`, a
// claim whose shim never said hello within the reconnect deadline (the registration deadline's
// bound) while its process lived, after a daemon restart restored it or after its connection
// closed over a pending task; and `GET /legion/v1/state` carries `unseenTaskValues`, every running
// claim whose value nobody reported seeing for a whole first bound (`{claim, issue, role, task,
// writtenAt}`). Its settings carry `worker_take_timeout_seconds` and
// `worker_turn_start_timeout_seconds`, keys of legion.yaml that no reader types, which move no
// contract of their own (answers.go's typedSettings).
//
// This daemon serves each of these down through the shaping 193 brought, to a caller whose header
// states a contract below this entry's or, stating none, whose pod's claims record one. The state
// is served without `unseenTaskValues` (this entry's band, withoutUnseenTaskValues, which follows
// the pod's highest contract as every band of shaping.go's stateNarrowings does). This entry adds
// `never_reconnected` to the claim views' `failure.cause` and `pendingCause`, beside the seven
// words answers.go's failureCauses records at 75, and its value strip withoutNeverReconnected
// (which follows the pod's lowest contract, as every row of shaping.go's valueStrips does) serves
// it as `never_registered` in both, since a plugin from 75 up to the contract before this entry's
// reads both through a strict enum and refuses the whole body for a word it does not name; the
// word is rewritten rather than its key dropped, because a key gone would say the claim's failure
// has no cause or that nothing failed its relaunching process, and `never_registered` is the
// nearest cause such a reader knows. `unseenTaskValues.*.role` is an enumerated field of this
// entry, every role a claim holds (unseenTaskRoles). The registration's answer is served without
// `task` and `finished` (shaping.go's withoutTaskReading, the registration's row of shapeAnswer,
// which placedByPod places by the caller's pod). The task read's and the take's answers are this
// entry's rows of answers.go's Answers (`task` and `take`), which every contract that reads them
// is served as this daemon writes them, and `claims/task-seen` is a route this entry adds to the
// 204 every claim report answers (`no-content`); the take answer's `outcome` is the discriminator
// of a union of literals and so no enumerated field. The three task routes' bodies and the turn
// outcome's `task`, `providerError` and `held` are the routes of this entry's fixture
// (`packages/contracts/fixtures/daemon-api/contracts/20261125171504_task-take.json`). The band and
// the value strip are this entry's rows of the tables 193 moved into shaping.go, so the state is
// shaped in one place.
const taskTakeContract = 20261125171504

// TaskTakeContract is the contract at which a worker takes its task rather than being prompted
// with it: the number this build gives the task-take entry. A claim whose plugin declares a
// contract below it waits for a prompt this daemon never sends (supervise.TakesNoTask), so the
// daemon hands this number to supervise and to the idle release, and the claim routes compare a
// caller's contract with it.
func TaskTakeContract() int { return entryNumber(taskTakeContract) }

// taskTakeCostCap reports whether the registered producer can send cost_cap. A request that
// declares an older contract cannot use that value either.
func taskTakeCostCap(r *http.Request, registered int) bool {
	stated, ok := statedContract(r)
	return !supervise.TakesNoTask(registered, TaskTakeContract()) &&
		(!ok || !supervise.TakesNoTask(stated, TaskTakeContract()))
}

// The answers this entry adds: a worker's read of its claim's task value, and the answer to its
// take of the task that value names.
const (
	AnswerTask AnswerID = "task"
	AnswerTake AnswerID = "take"
)

// unseenTaskRoles is every role a claim can hold, at this entry, which `unseenTaskValues`
// arrived at: no `unseenTaskValues` is served below this entry's contract at all
// (withoutUnseenTaskValues).
var unseenTaskRoles = valuesFrom(taskTakeContract, floorRoles...)

// neverReconnected is the failure cause this entry adds to the claim views: a claim whose shim
// never said hello again within the reconnect deadline.
var neverReconnected = []EnumeratedValue{{Value: string(supervise.CauseNeverReconnected), Since: taskTakeContract}}

func init() {
	registerEntry(contractEntry{
		id: taskTakeContract, slug: "task-take",
		bands: []stateNarrowing{
			{contract: taskTakeContract, state: withoutUnseenTaskValues, dropped: "unseenTaskValues"},
		},
		valueStrips: []valueStrip{
			{issue: withoutNeverReconnected, dropped: droppedFailureCause},
		},
		answers: []Answer{
			// A worker's read of its claim's task value, the value the task bucket holds and the
			// number of the last task the claim finished (`claim.TaskResponse`). The route arrives
			// with the answer, so no contract before this entry records either.
			{ID: AnswerTask, Routes: []string{"POST /legion/v1/claims/task"}, Since: taskTakeContract},
			// The answer to a worker's take of the task its value names, accepted with the text,
			// refused naming the hold or dropped (`claim.TakeResponse`), arrived with its route as
			// the task read's answer did.
			{ID: AnswerTake, Routes: []string{"POST /legion/v1/claims/take"}, Since: taskTakeContract},
		},
		// The seen report answers the 204 every claim report answers, which is older than it.
		answerRoutes: []answerRoutes{
			{answer: AnswerNoContent, routes: []string{"POST /legion/v1/claims/task-seen"}},
		},
		// Both answers arrived whole at this entry, so every contract that reads them reads the
		// body this daemon writes.
		identityAnswers: []AnswerID{AnswerTask, AnswerTake},
		enumeratedFields: []EnumeratedField{
			// `role` of one claim whose task value nobody reported seeing (UnseenTaskValue.Role).
			// It arrived with `unseenTaskValues`, which is served down below this entry whole, so
			// no body below it carries a value of it.
			{Answer: AnswerState, KeyPath: "unseenTaskValues.*.role", Values: unseenTaskRoles},
		},
		// `never_reconnected` arrived after the field, and is served as `never_registered` below
		// this entry (withoutNeverReconnected). The architect's view and each worker's carry the
		// one field, and the record's rows take the values with the state's.
		enumeratedValues: []enumeratedValues{
			{answer: AnswerState, keyPath: "issues.*.architect.failure.cause", values: neverReconnected},
			{answer: AnswerState, keyPath: "issues.*.workers.*.claim.failure.cause", values: neverReconnected},
			{answer: AnswerState, keyPath: "issues.*.architect.pendingCause", values: neverReconnected},
			{answer: AnswerState, keyPath: "issues.*.workers.*.claim.pendingCause", values: neverReconnected},
		},
	})
}

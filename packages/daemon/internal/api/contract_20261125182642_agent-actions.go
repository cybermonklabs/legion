package api

import (
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// 20261125182642 agent-actions: #4065 -- the project's controller gets the actions on an agent
// (stop, retry, suspend, resume_agent, compact and fresh session of one agent, named by its issue
// and role) it lacked, the four the operator already has, as controller routes beside
// `claims/fresh-session` and `claims/compact`, each taking that route's `api.ClaimRequest`
// (`grantId`, `issue`, `role`) and the same door (agentDoor: a controller grant, or `403
// CONTROLLER_REQUIRED` for every other grant, an architect's included, as those two routes answer
// it; an agent whose claim the daemon supervises, or `404 NO_CLAIM`, its token built from the
// daemon's own project, so another project's agent cannot be named): `POST /legion/v1/agents/stop`
// runs `legion claims stop`'s effect, `POST /legion/v1/agents/retry` the plain `legion claims
// retry` (no fresh session), `POST /legion/v1/agents/suspend` `legion claims suspend`'s, and `POST
// /legion/v1/agents/resume` `legion claims resume`'s. Each answers `api.AgentActionResponse`: the
// claim's `state` after the request and, where a suspension is over the agent, `suspendedBy`,
// `suspensionHeld` and `suspensionHeldBy`, which these routes alone of the session-facing ones
// carry, since no plugin below this contract calls them (the state still names no suspender,
// StrandedClaim); a suspension held for the agent's turn, and a retry whose start the issue's phase
// queued, answer 202. The actions are the controller's alone. The controller's suspension is
// recorded as its own, `controller`, a new supervise.SuspendSource that `claims.suspended_by`
// admits from migration 20261115060739: it holds against the daemon's rules as the operator's does,
// its own resume lifts it, and the operator's suspension outranks it. So the operator claim routes'
// `suspendedBy` and `suspensionHeldBy` gain the value `controller`, which only the operator's
// bearer reads; no shape moves. The controller's resume_agent, suspend, stop and retry, and its
// fresh session, are refused an agent the operator's own suspension holds, held for a turn or
// landed, with `409 SUSPENDED_BY_OPERATOR`, nothing of the claim changed; its compaction is not,
// and refuses such an agent in its own codes. Its retry and resume of an agent whose claim is
// stored working and that no such suspension holds are refused `409 CLAIM_IN_A_TURN`. The
// controller gets one more route of its own beside these (#4065): `POST
// /legion/v1/issues/pod-update` takes `api.PodUpdateRequest` (`grantId`, `issue`) and opens the
// daemon's update round over that issue's pod as a drift opens one: each role of the pod is asked
// to push its work and answer ready_for_update, and the round reaches its end once each has
// answered, no turn runs and each role's report since its last turn names no running background
// work, or at the round's bound (`pod_update_grace_minutes`), whatever the pod's drift. At its end
// the daemon reads the pod's workspace, and replaces the pod only where that read finds nothing
// that no branch or tag on origin holds; where it finds such work, or fails, the round keeps the
// pod and ends with no replacement, and the open phase's worker or the issue's architect is told.
// The request opens its round over a pod an earlier round kept as well, over which the sweep
// opens none. The round's reasons say the project's controller asked for the replacement, and the
// opening is held to the project's limits on openings as a drift's is (`409 POD_UPDATE_LIMITED`,
// naming `pod_update_max_open_rounds` or `pod_update_rounds_per_minute`). It answers
// `api.PodUpdateResponse`: the `round`, its `reasons`, and whether this request `opened` it, false
// for a round already open over the pod, which it leaves as it stands. It takes a controller grant
// alone (`403 CONTROLLER_REQUIRED`) and an issue key (`400 INVALID_ISSUE`); an issue of another
// project, or one with no claim the daemon supervises that has not retired, is `404 NO_CLAIM`, one
// whose claims run no worker in a pod `404 NO_POD`, and one whose pod is being replaced already —
// its claims in two pods, or the round over the pod before it still ending — `409 POD_CHANGING`.
// It is the one route by which the controller removes an issue pod whose workspace may hold work.
// It answers 200 only once a claim of the issue records the round in the store, which a restart
// restores and the sweeps carry to its end; a round no claim records within its wait is `503
// POD_UPDATE_NOT_RECORDED`, and may still open. A request waits for the turn a sweep's walk over
// the project's issues holds while its own context lives and at most the bound of its wait for the
// record (30 seconds); one whose context ends first, or that waits out that bound, reads nothing
// and opens nothing, and is `503 POD_UPDATE_BUSY` with a `Retry-After` of the sweep's interval in
// seconds (60), its sentence naming which of the two ended the wait, and the bound. While the
// project is paused (178), the controller's `agents/resume` and `agents/retry` answer 409
// `PROJECT_PAUSED`, with the `pause` that stands, for every agent, as its `claims/fresh-session`
// and `claims/compact` do, and change
// nothing: an agent the pause stopped is the pause's to bring back at the project's resume, and no
// agent action undoes the pause. Its `agents/stop` and `agents/suspend` start nothing and still
// run. A suspension of the controller's or the operator's and the pause's stop each keep their own
// source: the pause stops no agent such a suspension already holds, an agent the pause stopped that
// such a suspension then takes over stays suspended by it at the resume, and the resume brings back
// only what the pause stopped. `issues/pod-update` answers 409 `PROJECT_PAUSED` while the project
// is paused, before the daemon is asked anything, and 409 `POD_HELD_BY_PAUSE` for a pod a pause
// holds: the hold of a pause that began after that `PROJECT_PAUSED` check read the project as
// running, or a hold the resume left on a pod until a role of the pod is live again; neither opens
// a round. Every `PROJECT_PAUSED` sentence, those of 178's routes included, now ends by naming what
// lifts the pause (server.go's pauseLifts): the project's resume, as the controller's `resume`
// operation of its `legion` tool, which names no issue, and the operator's `legion project resume`;
// the fixture `project-paused.json` carries it. The notice of a forgotten session that entry 189
// adds, `session-forgotten`, goes to the issue's architect alone, as `session-lost` does: the
// controller is told of neither (#4065), and no shape moves. This contract is served down by 193's
// rule (#3659): every answer goes through one writer, which shapes it to the contract its caller
// reads. The agent actions' answer and the pod update's are two rows of this entry (`answers` of
// its registration below), `agent-action` on the four routes and `pod-update` on
// `issues/pod-update`, each arrived whole with its routes at this entry, so no contract below
// records either, and shapeAnswer serves each as written (`identityAnswers` of the registration).
// The agent actions' `state`, `suspendedBy` and `suspensionHeldBy` are enumerated rows of the entry
// (`enumeratedFields`), each recorded whole at it, so no value of them is held back, and the plugin
// reads every field of both answers as optional, since its code hands each answer on whole and
// reads none of them (`LEGION_ANSWER_READS`). Every refusal of the five routes is the refusal
// answer: `PROJECT_PAUSED` is an `api.PauseFailure`, placed by the caller's pod where it states no
// contract and served without its `pause` below 178, and every other code, from
// `CONTROLLER_REQUIRED` to `POD_HELD_BY_PAUSE`, is an `api.Failure`, which no contract shapes. The
// five routes' request records start at this entry: its fixture holds them (`routes` of
// `packages/contracts/fixtures/daemon-api/contracts/20261125182642_agent-actions.json`), and its
// test rows give each route's request type (`requests` of
// contract_20261125182642_agent-actions_test.go). No key and no value of any other answer moved, so
// this entry's records of every other answer are those of the contract before it.
// Before #5399 this change carried a number of its own, 292 at the last, which a census of the open
// pull requests chose again at each merge of main. Since #5399 no branch picks a number: this
// entry's number is its rank in main's merge order.
//
// agentActionsContract is the id of this entry (contract_<id>_<slug>.go), the UTC second its author
// made it, which the build's order turns into the entry's number. Each row of the registration
// below names the entry by it, and the registry resolves each such value to the entry's number;
// code that compares a caller's contract with the entry reads entryNumber(agentActionsContract).
// The number moves when an entry ranks before this one, and the id does not. The entry is the one
// four of the agent door's six agent actions and the controller's pod update arrived at: `POST
// /legion/v1/agents/stop`, `retry`, `suspend` and `resume`, which answer AgentActionResponse
// (AnswerAgentAction), and `POST /legion/v1/issues/pod-update`, which answers PodUpdateResponse
// (AnswerPodUpdate). Each answer arrived whole with its routes, so no contract below this entry
// records either, no caller below it is served one, and neither is ever shaped: each is one of the
// entry's `identityAnswers`, which shapeAnswer serves as written (identityAnswer). No key and no
// value of any other answer moved at this entry.
const agentActionsContract = 20261125182642

func init() {
	registerEntry(contractEntry{
		id: agentActionsContract, slug: "agent-actions",
		answers: []Answer{
			// The claim's state after one of the controller's four agent actions on the `agents/`
			// routes (`api.AgentActionResponse`). The routes arrive with the answer, so no contract
			// below this entry records either.
			{ID: AnswerAgentAction, Routes: []string{
				"POST /legion/v1/agents/stop",
				"POST /legion/v1/agents/retry",
				"POST /legion/v1/agents/suspend",
				"POST /legion/v1/agents/resume",
			}, Since: agentActionsContract},
			// The update round the controller's pod update opened or found open over the issue's
			// pod (`api.PodUpdateResponse`), arrived with its route as the agent actions' answer
			// did.
			{ID: AnswerPodUpdate, Routes: []string{"POST /legion/v1/issues/pod-update"}, Since: agentActionsContract},
		},
		identityAnswers: []AnswerID{AnswerAgentAction, AnswerPodUpdate},
		enumeratedFields: []EnumeratedField{
			// `state`, `suspendedBy` and `suspensionHeldBy` of what four of the six agent actions
			// answer (AgentActionResponse). All three arrived with that answer at
			// agentActionsContract, so no value of any of them is served down. `suspensionHeld` is
			// a `z.literal` and no row.
			{Answer: AnswerAgentAction, KeyPath: "state", Values: agentActionStates},
			{Answer: AnswerAgentAction, KeyPath: "suspendedBy", Values: suspendSources},
			{Answer: AnswerAgentAction, KeyPath: "suspensionHeldBy", Values: suspendSources},
		},
	})
}

// agentActionStates is every value of the agent actions' `state` (AgentActionResponse,
// AnswerAgentAction): the claim view's states, recorded at agentActionsContract, the
// contract the answer arrived at with its four routes, since no reader below it parses that
// answer at all.
var agentActionStates = valuesFrom(agentActionsContract,
	supervise.StateQueued, supervise.StateLaunchUncertain, supervise.StateLaunching,
	supervise.StateShimConnected, supervise.StateRegistered, supervise.StateReady,
	supervise.StateWorking, supervise.StateIdle, supervise.StateSuspended, supervise.StateFailed,
	supervise.StateRetired)

// suspendSources is every source a suspension can be recorded under (supervise.SuspendSource),
// which the agent actions' `suspendedBy` and `suspensionHeldBy` share. Both arrived with that
// answer at agentActionsContract, so the list is recorded there whole, `controller` among it. It
// names each constant rather than reading a roster, as failureCauses does its causes, because a
// source added later arrives at its own contract.
var suspendSources = valuesFrom(agentActionsContract,
	supervise.SuspendedByUnknown, supervise.SuspendedByOperator, supervise.SuspendedByController,
	supervise.SuspendedByDaemon, supervise.SuspendedByExit, supervise.SuspendedByReleased)

const AnswerAgentAction AnswerID = "agent-action"

const AnswerPodUpdate AnswerID = "pod-update"

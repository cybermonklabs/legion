package api

import (
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ServingFloor is the lowest plugin contract this daemon shapes an answer for. A caller below it,
// whether it states its contract or its pod records it, has the keys removed since the floor
// written back as at the floor (restoreContract) and keeps every strip the daemon applies below
// the floor today (stripContract keeps its own number), so no key a strip takes from it today
// comes back, and it is served less than a caller at the floor; the daemon logs a line for it. A
// caller that states no contract is placed by the claims of its pod on the state, on the record
// and on every answer placedByPod names. On every other answer, and on every answer when it asks
// from no pod, it is served this daemon's own shape (contractsFor, writeAnswer).
//
// 46 is the reading this change records: no claim holding a process recorded a contract below 46.
// The 50 claims at contracts 36 to 43 are all suspended, so they call nothing.
//
// The gate has three parts. A plugin built before dispatch://INFRA-1048 reads an answer with a
// strict schema and refuses the whole body for one key it does not know, which the added-key
// part serves down. dispatch://INFRA-1048 landed the reader that drops an unknown key instead,
// and the first release carrying that reader declares 49, so once `lowestPluginContract` on the
// state reports 49 or more, no live reader refuses a key it does not know. The added-key part
// may then be dropped, with the record entries that serve it; until then each later contract
// that adds a key outside `settings` writes a band for the contracts below it, because their
// records do not hold that key. The removed-key part stays for good: a reader still needs the
// key its own contract requires, whatever this daemon stopped writing.
// The third part is the value hold-back: the values a later contract added to an enumerated
// field are held back from a caller below that contract (EnumeratedFields, shaping.go's
// valueStrips, fieldArrivals.heldBack). It stays after
// `lowestPluginContract` reaches 49 too, because a reader refuses a value its enumeration does
// not admit, whatever its reading of keys.
const ServingFloor = 46

// SupportedContracts is every contract this daemon describes a shape for: each number version.go
// carries a numbered entry for, from ServingFloor up to DaemonAPIVersion, and nothing else. A
// number in that range with no entry is in no list, and a caller at it takes the nearest recorded
// contract below it (recordedContractAtOrBelow).
// TestSupportedContractsAreTheNumberedEntriesFromTheFloorUp (`version_test.go`) reads the entry
// headers out of version.go and holds this list to them in both directions.
//
// 193, 202, 205, 213, 219, 220, 228, 246, 250, 256, 258, 259, 285 and 302 are constants rather
// than their numbers, because the entries that added them name them. A renumber changes that
// entry, its constant and this list together: moving either one alone would describe a shape at a
// contract no entry declares. The last constant is the owner's report of its own step's
// (completeStepContract, 302), the last entry, after 301, the roles an owner's plan defines; the
// claim tokens of the starts that wait for their pod (startWaitTokenContract, 285) come after 284,
// strandedTokenContract and deploymentRolesContract. The other entries above 193 follow them as
// numbers: 199, 217, 232, 284 and 301.
var SupportedContracts = []int{46, 48, 49, 51, 52, 53, 54, 55, 56, 57, 58, 59, 62, 64, 68, 73, 75, 85, 86, 87, 94, 98, 104, 111, 123, 132, 133, 137, 141, 144, 146, 151, 152, 160, 168, 178, 188, 189, shapedAnswersContract, 199, repositoriesLiveContract, childRouteRemovedContract, cachesSourceContract, 217, OwnerPlanContract, assignmentPlanContract, intakeGiveUpsContract, 232, podHoldContract, priorityContract, causeTimesContract, deploymentRolesContract, strandedTokenContract, 284, startWaitTokenContract, 301, completeStepContract}

// AnswerID names one answer a plugin parses. It is stable: every write of this package goes
// through `shapeAnswer(id, …)`, so the id outlives the route that happens to carry the answer
// today.
type AnswerID string

// The answers a plugin parses, one id per response schema.
const (
	AnswerState              AnswerID = "state"
	AnswerIssueRecord        AnswerID = "issue-record"
	AnswerHealth             AnswerID = "health"
	AnswerRegister           AnswerID = "register"
	AnswerRegisterController AnswerID = "register-controller"
	AnswerNoContent          AnswerID = "no-content"
	AnswerReleaseRecords     AnswerID = "release-records"
	AnswerGrant              AnswerID = "grant"
	AnswerHandoffComplete    AnswerID = "handoff-complete"
	AnswerEmptyObject        AnswerID = "empty-object"
	AnswerFreshSession       AnswerID = "fresh-session"
	AnswerCompact            AnswerID = "compact"
	AnswerTurnOutcome        AnswerID = "turn-outcome"
	AnswerWaveRelease        AnswerID = "wave-release"
	AnswerRefusal            AnswerID = "refusal"
	AnswerProjectPause       AnswerID = "project-pause"
	AnswerProjectResume      AnswerID = "project-resume"
	AnswerPlanRegister       AnswerID = "plan-register"
	AnswerIntakeReplay       AnswerID = "intake-replay"
	AnswerIntakeDiscard      AnswerID = "intake-discard"
)

// Answer is one response schema a plugin parses, with every route that answers it and the
// contract that answer arrived at.
type Answer struct {
	// ID is the answer's stable name.
	ID AnswerID `json:"id"`
	// Routes are the routes that answer this schema, as `METHOD /path`. One schema may be the
	// answer of many routes, and one route answers one schema. The refusal body is the one row
	// with no route: a client parses it on every answer that is not a success.
	Routes []string `json:"routes"`
	// Since is the contract this answer arrived at. An answer older than ServingFloor is
	// recorded from the floor up, so the oldest value here is the floor itself.
	Since int `json:"since"`
	// NoBody says the answer carries no body at all. It reads differently from an answer whose
	// set of key paths is empty: a later check tells the two apart, because a body with no keys
	// is still a body to shape and an answer with none is not.
	NoBody bool `json:"noBody"`
	// TypedSettings is, on the state's row alone, every top-level key of `settings` that a
	// plugin's reader types, with the contracts whose reader types it (typedSettings). The
	// records hold those settings key paths and no other.
	TypedSettings []TypedSetting `json:"typedSettings,omitempty"`
}

// TypedSetting is one top-level key of the state's settings block that a plugin's reader types,
// and the contracts whose reader types it: from Since to Until, both included, or from Since to
// this daemon's own contract where Until is 0.
type TypedSetting struct {
	Key   string `json:"key"`
	Since int    `json:"since"`
	Until int    `json:"until,omitempty"`
}

// typedSettings is every top-level key of `settings` that a plugin's reader types, and the
// contracts whose reader types it. The state's row of Answers carries it, so answers.json writes
// it, and a record holds a settings key path at a contract exactly where this list types that
// key there, and no settings path below a key at all (contract_shapes_test.go's settingsTyped).
//
// Only typed keys are recorded because a reader refuses the body for no key it does not type,
// present or absent, so a record that held such a key would tie every new legion.yaml key to a
// new contract number, where the reader's own rule (`legionSettings` in
// `packages/contracts/src/legion-api.ts`) is that a new legion.yaml key is no contract change. A
// typed key is one whose removal a reader does refuse, which is why `max_fix_attempts`, typed
// below 59, is served to those readers as a stand-in (shaping.go's withMaxFixAttempts).
//
// The rows are measured rather than guessed, at the first commit on main of each contract of
// SupportedContracts (version.go's 193). A key a later reader types is a row here from that
// reader's contract on, and pi-shared's census (`daemon-client.test.ts`) holds the rows typed at
// this daemon's own contract equal to the keys `legionSettings` types.
var typedSettings = []TypedSetting{
	{Key: "max_fix_attempts", Since: ServingFloor, Until: fixAttemptsContract - 1},
	{Key: "worker_boot_timeout_seconds", Since: ServingFloor},
	{Key: "review_round_cap", Since: stateMachineContract},
}

// Answers is one row per answer a plugin parses. The operator's claim answers, the operator's
// pause and resume answers (`legion project pause|resume`), the operator's replay and discard of
// the intake's give-ups (`legion intake replay|discard`) and the controller's secret and
// credential answers are no rows: no plugin parses them.
var Answers = []Answer{
	// Older than the floor: `GET /legion/v1/state` predates every numbered entry of version.go.
	{ID: AnswerState, Routes: []string{"GET /legion/v1/state"}, Since: ServingFloor, TypedSettings: typedSettings},
	// version.go's 193: the per-issue record route a role reads in place of the whole state.
	// dispatch://INFRA-2694 added the route, so no contract below 193 records it.
	{ID: AnswerIssueRecord, Routes: []string{"GET /legion/v1/issues/{key}/record"}, Since: shapedAnswersContract},
	// version.go's 193: the typed health body (`api.Health`), which dispatch://INFRA-2694 put in
	// place of the one untyped key the route answered, so the typed body has no record below 193
	// either.
	{ID: AnswerHealth, Routes: []string{"GET /healthz"}, Since: shapedAnswersContract},
	// Older than the floor: a role's registration answer (`claim.RegisterResponse`).
	{ID: AnswerRegister, Routes: []string{"POST /legion/v1/claims/register"}, Since: ServingFloor},
	// Older than the floor: the controller's registration answer, which version.go's 14 added to
	// the same route and which 46 is the floor for here.
	{ID: AnswerRegisterController, Routes: []string{"POST /legion/v1/claims/register"}, Since: ServingFloor},
	// Older than the floor: 204 and nothing after the headers. Two of the four routes are newer
	// — `claims/ready-for-update` is version.go's 46 and `claims/activity` its 48 — but the
	// answer they share is older than either.
	{ID: AnswerNoContent, Routes: []string{
		"POST /legion/v1/claims/ready",
		"POST /legion/v1/claims/ready-for-update",
		"POST /legion/v1/claims/exit",
		"POST /legion/v1/claims/activity",
	}, Since: ServingFloor, NoBody: true},
	// version.go's 48: the deployment's release, resume and hold records a pod reads.
	{ID: AnswerReleaseRecords, Routes: []string{"POST /legion/v1/claims/release-records"}, Since: 48},
	// Older than the floor: the grant a session mints for its own daemon calls.
	{ID: AnswerGrant, Routes: []string{"POST /legion/v1/grants"}, Since: ServingFloor},
	// Older than the floor: the handoff completion's answer, which version.go's 19 gave its
	// `note` and which 46 is the floor for here.
	{ID: AnswerHandoffComplete, Routes: []string{"POST /legion/v1/handoff/complete"}, Since: ServingFloor},
	// Older than the floor: the empty object the fact routes answer. `tasks/stop` is
	// OwnerPlanContract's and `steps/complete` completeStepContract's, and the answer they share is
	// older than either, as the no-content answer is older than two of its routes. The child route
	// that childRouteRemovedContract removed answered it too: that route now answers every call
	// with the refusal (removedChildRoute), and no plugin at that contract calls it.
	{ID: AnswerEmptyObject, Routes: []string{
		"POST /legion/v1/issues/status",
		"POST /legion/v1/gates/register",
		"POST /legion/v1/tasks/stop",
		"POST /legion/v1/phase/backward",
		"POST /legion/v1/phase/retry",
		"POST /legion/v1/signoff",
		"POST /legion/v1/steps/complete",
		"POST /legion/v1/roots/close",
		"POST /legion/v1/children/rerun",
	}, Since: ServingFloor},
	// version.go's 59: the controller's fresh-session recovery (`api.FreshSessionResponse`).
	{ID: AnswerFreshSession, Routes: []string{"POST /legion/v1/claims/fresh-session"}, Since: 59},
	// version.go's 59: the controller's compaction recovery (`api.CompactResponse`).
	{ID: AnswerCompact, Routes: []string{"POST /legion/v1/claims/compact"}, Since: 59},
	// version.go's 86: what a producer's report of one turn comes back as, the run of refused
	// turns the claim's reports now end with and whether this report failed the claim
	// (`claim.TurnOutcomeResponse`). The route arrives with the answer, so no contract below 86
	// records either.
	{ID: AnswerTurnOutcome, Routes: []string{"POST /legion/v1/claims/turn-outcome"}, Since: stateMachineContract},
	// Older than the floor: the wave release's answer, which version.go's 38 gave its `started`
	// and `held` and which 46 is the floor for here.
	{ID: AnswerWaveRelease, Routes: []string{"POST /legion/v1/waves/release"}, Since: ServingFloor},
	// Older than the floor: one sentence under `error`, which every route refuses with and which
	// a client parses on every answer that is not a success. It has no route of its own.
	{ID: AnswerRefusal, Routes: []string{}, Since: ServingFloor},
	// version.go's 178: the pause the controller's pause began (`api.ProjectPauseResponse`). The
	// route arrives with the answer, so no contract below 178 records either.
	{ID: AnswerProjectPause, Routes: []string{"POST /legion/v1/project/pause"}, Since: pauseContract},
	// version.go's 178: the pause the controller's resume ended and how many held rows it let out
	// (`api.ProjectResumeResponse`), arrived with its route as the pause's answer did.
	{ID: AnswerProjectResume, Routes: []string{"POST /legion/v1/project/resume"}, Since: pauseContract},
	// version.go's 219: the plan register_plan took (`api.PlanRegisterResponse`). The route
	// arrives with the answer, so no contract below 219 records either.
	{ID: AnswerPlanRegister, Routes: []string{"POST /legion/v1/plans/register"}, Since: OwnerPlanContract},
	// version.go's 228: what the controller's replay of the intake's give-ups did, record by
	// record, and how many of the selection are left (`api.IntakeReplayResponse`). The route
	// arrives with the answer, so no contract below 228 records either.
	{ID: AnswerIntakeReplay, Routes: []string{"POST /legion/v1/intake/replay"}, Since: intakeGiveUpsContract},
	// version.go's 228: the ids the controller's discard of the intake's give-ups closed
	// (`api.IntakeDiscardResponse`), arrived with its route as the replay's answer did.
	{ID: AnswerIntakeDiscard, Routes: []string{"POST /legion/v1/intake/discard"}, Since: intakeGiveUpsContract},
}

// EnumeratedField is one field of one answer that a plugin parses with a strict enumeration,
// which admits the values its own contract names and no other.
type EnumeratedField struct {
	// Answer is the answer the field is a field of.
	Answer AnswerID `json:"answer"`
	// KeyPath is the field's key path in that answer's body: the dotted path from the body's
	// root, with a map's key and a list's index written as `*`, which is the spelling the
	// records use (`packages/contracts/fixtures/daemon-api/contract-shapes.json`).
	KeyPath string `json:"keyPath"`
	// Values is every value the field may take, each with the contract it arrived at, sorted by
	// value. A value older than ServingFloor is recorded from the floor up, as an answer is.
	Values []EnumeratedValue `json:"values"`
	// Until is the contract from which the field is no enumeration, 0 for a field enumerated at
	// every contract: a reader at Until or above takes any value of a grammar no list of values
	// holds, so a record from Until up carries no domain of the field, and a reader below it is
	// served only the values the list names (its own serving-down).
	Until int `json:"until,omitempty"`
}

// EnumeratedValue is one value of an enumerated field and the contract that value arrived at.
type EnumeratedValue struct {
	Value string `json:"value"`
	Since int    `json:"since"`
}

// EnumeratedFields is one row per field of an answer that a plugin reads with a strict
// enumeration, with each value and the contract it arrived at.
//
// It is here because a value a contract does not admit refuses that contract's reader as surely
// as a key it does not know: `packages/contracts/src/legion-api.ts` reads `answeredBy` as
// `z.enum(LEGION_WAIT_ANSWERS)`, and a zod enum refuses a value it does not name whether or not
// the field is optional, so a body carrying a ninth value is refused whole by a reader of the
// eight — the same refusal an added key gets from a reader before dispatch://INFRA-1048. So
// both halves of the shaping dispatch://INFRA-2694 put in place read the table: the serving-down takes from a caller's body each
// value that caller's reader does not admit (shaping.go's valueStrips; fieldArrivals.heldBack
// says which values and why), and the records carry the value domain of each field beside its
// key paths, so a value added with no serving-down fails the gate as an added key does
// (contract_shapes_test.go).
//
// A field no row declares is a field the records carry no domain for and the gate holds to
// nothing, so a strictly parsed field belongs here the day it arrives. What counts as one: a
// field of an answer of Answers that `packages/contracts/src/legion-api.ts` reads with a
// `z.enum`, at every key path it is reachable at. The whole-project body's `issues.*.…` and the
// record's bare `…` are two rows of one field, as the two `answeredBy` rows are. A field read
// with a `z.literal` is no row — `controllerLocator.external` and the discriminators of the three
// discriminated unions, `locator.runtime`, `waitingOn.kind` and a pause actor's `kind`
// (`paused.by`, `lastPause.by`, `lastPause.endedBy`) — because a literal admits the one value its
// own shape carries and a daemon that widened it would be changing the field's type rather than
// its domain. `issues.*.holdReason` is the one field whose type did change so: every reader below
// OwnerPlanContract reads it with `z.literal("escalated")`, which admits that one value as an
// enumeration of one would, and the reader of that contract reads it with a `z.enum` of three. So
// it is a row: `escalated` is recorded at the floor, which is the one value every record below
// that contract admits, and the two words that contract added are recorded at it (holdReasons).
//
// Each value's arrival is the later of two readings, and each row says which of them decided
// it: the contract the value first appears in the plugin's own list at, and the contract the
// field itself arrived at, since a field that arrives at 55 can carry no value older than 55.
// So the earliest arrival of a list is its field's own, which is where the serving-down reads
// the field's arrival (shaping.go's arrivalsOf; fieldArrivals.heldBack says what for).
// The first reading is measured rather than guessed: `legion-api.ts` and `legion-roles.ts` were
// read at the commit that declared each contract of SupportedContracts, and the members of
// every `z.enum` diffed. The lists below that ever grew are `answeredBy`, which gained
// `declined` at 68, `strandedClaims[].state`, which gained `retired` at 56,
// `waitingLine.kind`, which gained `waiting_on_issue` at 132, `capabilities.*.source`, which
// gained `caches` at dispatch://INFRA-4036's contract (cachesSourceContract), `holdReason`, which
// gained `task-stopped` and `children` at OwnerPlanContract, `admission.held.*.reason`, which
// gained `children_before_retro` and `task_stopped` there, and an issue's `phase` with the two
// fields that take the workflow's phases, `heldFrom` and a held task's `phase`, which gained `step`
// at deploymentRolesContract, so every other list is recorded whole at its own field's arrival.
// (`suspendedBy` gained `released` at 48 and is no row here: the operator claim answers carry it
// and no plugin parses them.)
var EnumeratedFields = enumeratedFields()

// enumeratedFields is every row the table holds: the state's own, the record's taken from them,
// the registration's and the replay's, in that order.
//
// The record's rows are taken rather than written, because the record route answers the very
// value the whole-project body holds under `issues.<key>` (shaping.go's shapeIssue): every
// enumerated field of an issue is the one field at two key paths, and the record's path is the
// state's with the issue map's own prefix gone. Written out a second time, the two lists part
// company the day a per-issue field is added to one of them: the record route would answer that
// field with no domain recorded and no gate on its values.
//
// The `role` of a claim is a row at each of its key paths (a stranded claim's, a waiting wake's, a
// pause stop's and the registration's) up to deploymentRolesContract and none from it (Until):
// from that contract a reader takes there any role of the role grammar (claim.IsRole), a domain
// no list of values holds, and a caller below it is served the state's stranded claims, waiting
// wakes and pause stops of Legion's six roles alone (shaping.go's withoutDeploymentRoleClaims).
func enumeratedFields() []EnumeratedField {
	fields := slices.Clone(stateEnumeratedFields)
	for _, field := range stateEnumeratedFields {
		path, ofAnIssue := strings.CutPrefix(field.KeyPath, issueKeyPath)
		if !ofAnIssue {
			continue
		}
		fields = append(fields, EnumeratedField{Answer: AnswerIssueRecord, KeyPath: path, Values: field.Values, Until: field.Until})
	}
	return append(fields,
		// `role` of the claim a registration issued (claim.RegisterResponse.Role). The
		// controller's registration answers `controller` under a `z.literal` and is no row.
		EnumeratedField{Answer: AnswerRegister, KeyPath: "role", Values: claimRoles, Until: deploymentRolesContract},
		// `outcome` of one record the controller's replay took, how its replay ended
		// (api.ReplayedGiveUpView.Outcome, intake.ReplayOutcome). The answer arrived whole at
		// intakeGiveUpsContract, so no older caller is served a value of it.
		EnumeratedField{Answer: AnswerIntakeReplay, KeyPath: "replayed.*.outcome", Values: replayOutcomes})
}

// issueKeyPath is the path the whole-project body writes one issue's fields under. The record
// route writes those same fields with the prefix gone, which is the one difference between the
// two readings of an issue.
const issueKeyPath = "issues.*."

// stateEnumeratedFields is every enumerated field of the state, the answer that carries all but
// two of them. Each row under issueKeyPath is a row of the record answer too (enumeratedFields).
var stateEnumeratedFields = []EnumeratedField{
	// `phase` of an issue, where the workflow holds it (api.Issue.Phase, LEGION_PHASES). Its list
	// grew: `step` arrived at deploymentRolesContract, and an issue at step is served as
	// implementing below that contract (shaping.go's withoutDeploymentRoles).
	{Answer: AnswerState, KeyPath: "issues.*.phase", Values: phases},
	// `state` of a claim view, where a claim is in its life (api.ClaimView.State,
	// LEGION_CLAIM_STATES). The architect's view and each worker's carry the one field.
	{Answer: AnswerState, KeyPath: "issues.*.architect.state", Values: claimViewStates},
	{Answer: AnswerState, KeyPath: "issues.*.workers.*.claim.state", Values: claimViewStates},
	// An issue's other claims (api.Issue.OtherClaims, contract 259) carry the same claim view, so
	// each of its enumerated fields is declared again under `otherClaims`. The key arrived at 259
	// and a caller below it is served no `otherClaims` at all (shaping.go's withoutClaimTokens), so
	// each list is recorded from that contract (arrivedWith).
	{Answer: AnswerState, KeyPath: "issues.*.otherClaims.*.claim.state", Values: arrivedWith(strandedTokenContract, claimViewStates)},
	{Answer: AnswerState, KeyPath: "issues.*.otherClaims.*.claim.updateState", Values: arrivedWith(strandedTokenContract, updateStates)},
	{Answer: AnswerState, KeyPath: "issues.*.otherClaims.*.claim.failure.cause", Values: arrivedWith(strandedTokenContract, failureCauses)},
	{Answer: AnswerState, KeyPath: "issues.*.otherClaims.*.claim.pendingCause", Values: arrivedWith(strandedTokenContract, failureCauses)},
	{Answer: AnswerState, KeyPath: "issues.*.otherClaims.*.claim.pendingTask.phase", Values: arrivedWith(strandedTokenContract, workflowPhases)},
	// `updateState` of a claim view, where the claim's update round stands
	// (api.ClaimView.UpdateState). shapedAnswersContract's own key, served down below that
	// contract with the rest of the round report (shaping.go's withoutClaimRoundReport).
	{Answer: AnswerState, KeyPath: "issues.*.architect.updateState", Values: updateStates},
	{Answer: AnswerState, KeyPath: "issues.*.workers.*.claim.updateState", Values: updateStates},
	// `state` of the pull request the daemon observed (api.PullRequestView.State,
	// record.PullRequestState).
	{Answer: AnswerState, KeyPath: "issues.*.pullRequest.state", Values: pullRequestStates},
	// `mergeState` of that pull request, GitHub's own merge state for the head
	// (api.PullRequestView.MergeState). It arrived at stateMachineContract and is served down
	// below that contract with the rest of that contract's keys (shaping.go's
	// withoutStateMachineFields), so no body below 86 carries a value of it.
	{Answer: AnswerState, KeyPath: "issues.*.pullRequest.mergeState", Values: mergeStates},
	// `mergeState` and `toldState` of what the issue's review round waits on from GitHub
	// (api.MergeWaitView): the merge state the record holds for the head the round waits on,
	// and the state the reviewer's decision last records as told. Both take GitHub's merge
	// states, arrived with `mergeWait` and are served down below that contract with the whole
	// object (shaping.go's withoutMergeWait), so no body below 133 carries a value of either.
	{Answer: AnswerState, KeyPath: "issues.*.mergeWait.mergeState", Values: mergeWaitStates},
	{Answer: AnswerState, KeyPath: "issues.*.mergeWait.toldState", Values: mergeWaitStates},
	// `step` and `status` of one row of the deployment's design gate as the issue stands in it
	// (api.GatedStepView, record.DesignStep). Both arrived with `gatedSteps`, which is served
	// down below that contract whole (shaping.go's withoutGatedSteps).
	{Answer: AnswerState, KeyPath: "issues.*.gatedSteps.*.step", Values: designSteps},
	{Answer: AnswerState, KeyPath: "issues.*.gatedSteps.*.status", Values: gatedStepStatuses},
	// `answeredBy` of an `awaiting_person` root's wait, how the wait was answered
	// (api.WaitingOnView.AnsweredBy, record.Answer). Its list grew: `declined` arrived at 68,
	// after the field, and a wait answered by it is served without its answered pair below that
	// contract (shaping.go's withoutUnadmittedWaitAnswer).
	{Answer: AnswerState, KeyPath: "issues.*.waitingOn.answeredBy", Values: waitAnswers},
	// `memoryGroup` and `memoryDrawMode` of the issue's tree, the group drawn and the mode it
	// was drawn under (api.Issue). Both arrived with the memory draw, which is served down
	// below that contract whole (shaping.go's withoutMemory).
	{Answer: AnswerState, KeyPath: "issues.*.memoryGroup", Values: memoryGroups},
	{Answer: AnswerState, KeyPath: "issues.*.memoryDrawMode", Values: memoryDrawModes},
	// `state` of one claim nothing will wake (api.StrandedClaim). Its list grew: `retired`
	// arrived at 56, and an entry carrying it is dropped whole below that contract (shaping.go's
	// withoutUnadmittedStrandedStates), so no body below 56 carries the value and the gate passes
	// without a serving-down of its own.
	{Answer: AnswerState, KeyPath: "strandedClaims.*.state", Values: strandedStates},
	// `role` of one claim nothing will wake (api.StrandedClaim), of one wake the pacing holds
	// (api.WakeWaiting.Role), which arrived with `wakesWaiting` and is served down below that
	// contract whole (shaping.go's withoutWakesWaiting), and of one claim whose stop the
	// project's open pause waits on (api.PauseStopView.Role), which arrived with `paused` and is
	// served down below that contract whole (shaping.go's withoutPause). Each is enumerated up to
	// deploymentRolesContract (enumeratedFields says why). The wake's `kind` and the pause's
	// `by.kind` and `endedBy.kind` are no rows: the first is an unconstrained string, and the other
	// two are discriminators read with a `z.literal`.
	{Answer: AnswerState, KeyPath: "strandedClaims.*.role", Values: claimRoles, Until: deploymentRolesContract},
	{Answer: AnswerState, KeyPath: "wakesWaiting.*.role", Values: wakeRoles, Until: deploymentRolesContract},
	{Answer: AnswerState, KeyPath: "paused.waitsOn.*.role", Values: pauseStopRoles, Until: deploymentRolesContract},
	// `runtime` of the registered controller's locator, the runtime this daemon runs under
	// (api.ControllerLocator.Runtime).
	{Answer: AnswerState, KeyPath: "controllerLocator.runtime", Values: controllerRuntimes},
	// `status` and `source` of one row of the deployment's capability report
	// (api.CapabilityState, capabilities.Deployment.Report). `source`'s list grew: `caches`
	// arrived at cachesSourceContract, and a row of that source is served without its `source`
	// below that contract (shaping.go's withoutUnadmittedCapabilitySources).
	{Answer: AnswerState, KeyPath: "capabilities.*.status", Values: capabilityStatuses},
	{Answer: AnswerState, KeyPath: "capabilities.*.source", Values: capabilitySources},
	// `state` of a repository cache under the Kubernetes runtime (api.CacheView.State). The
	// block carries one per live repository and one per cache of a repository no live record
	// names, which are two key paths of the one field. The first is `repositories.live.*.cache`
	// from repositoriesLiveContract on, which arrived with that block (liveCacheStates), and
	// `repositories.mapped.*.cache` below it, in the block contract 152's shape writes from the
	// same entries (shaping.go's withoutRepositoriesLive).
	{Answer: AnswerState, KeyPath: "repositories.mapped.*.cache.state", Values: cacheStates},
	{Answer: AnswerState, KeyPath: "repositories.live.*.cache.state", Values: liveCacheStates},
	{Answer: AnswerState, KeyPath: "repositories.caches.*.state", Values: cacheStates},
	// `heldFrom` of an issue, the phase a hold took it out of (api.Issue.HeldFrom). It arrived
	// with the decided record and is served down below that contract with the rest of it
	// (shaping.go's withoutRecordFields). Its domain is the workflow's own phases and never
	// `unrecorded`, which is what an issue the workflow does not record reads `phase` as and is
	// no phase a hold can take an issue out of. A hold from step is served as a hold from
	// implementing below deploymentRolesContract (shaping.go's withoutDeploymentRoles).
	{Answer: AnswerState, KeyPath: "issues.*.heldFrom", Values: workflowPhases},
	// `cause` of a claim's failure record and `pendingCause` beside it, the word the site that
	// failed the claim chose (api.ClaimFailureView.Cause, supervise.FailureCause). Both arrived
	// with the decided record and are served down below that contract whole
	// (shaping.go's withoutRecordFields), and the architect's view and each worker's carry the
	// one field.
	{Answer: AnswerState, KeyPath: "issues.*.architect.failure.cause", Values: failureCauses},
	{Answer: AnswerState, KeyPath: "issues.*.workers.*.claim.failure.cause", Values: failureCauses},
	{Answer: AnswerState, KeyPath: "issues.*.architect.pendingCause", Values: failureCauses},
	{Answer: AnswerState, KeyPath: "issues.*.workers.*.claim.pendingCause", Values: failureCauses},
	// `phase` of the task a claim holds, the issue phase it was queued for
	// (api.PendingTaskView.Phase). It arrived with the decided record, is served down with it,
	// and takes the workflow's phases as `heldFrom` does, a task queued for step served as one
	// queued for implementing below deploymentRolesContract.
	{Answer: AnswerState, KeyPath: "issues.*.architect.pendingTask.phase", Values: workflowPhases},
	{Answer: AnswerState, KeyPath: "issues.*.workers.*.claim.pendingTask.phase", Values: workflowPhases},
	// `part` and `kind` of a waiting line member, which part of the line the issue waits in and
	// why it waits there (api.WaitingLineView, record.LinePart and record.LineKind). Both
	// arrived with the line's own fields and are served down below that contract, where the
	// member is rebuilt with its `place` and its `length` alone (shaping.go's
	// withoutWhyAndLineFields). `kind`'s list grew: `waiting_on_issue` arrived at 132, and a
	// member of that kind is served without its `kind` below that contract (shaping.go's
	// withoutWaitingOnIssue).
	{Answer: AnswerState, KeyPath: "issues.*.waitingLine.part", Values: lineParts},
	{Answer: AnswerState, KeyPath: "issues.*.waitingLine.kind", Values: lineKinds},
	// `state` of the one rendered answer an issue that is not working now carries, which of the
	// four facts holds it (api.WhyView.State). It arrived beside the line's own fields and is
	// served down with them.
	{Answer: AnswerState, KeyPath: "issues.*.why.state", Values: whyStates},
	// All three words of `architectStalls` arrived with the key at 146, riding its band: no strip.
	{Answer: AnswerState, KeyPath: "issues.*.architectStalls.*", Values: valuesFrom(entryTimesContract, record.ArchitectTookNothing, record.ArchitectClaimUnreachable, record.ArchitectSlowBeforeGate)},
	// `reason` of one row of the admission's held list, the one word for why nothing of that
	// issue runs (api.HeldIssue, record.HeldReason). It arrived with `admission.held`, which is
	// served down below that contract whole (shaping.go's withoutHeld), so no body below 98
	// carries a value of it. Its list grew: `children_before_retro` and `task_stopped` arrived
	// at OwnerPlanContract, after the field, and a row of either word is served as
	// `unclassified` below that contract (shaping.go's withoutUnadmittedHeldWords).
	{Answer: AnswerState, KeyPath: "admission.held.*.reason", Values: heldReasons},
	// `holdReason` of a held issue, why it is held (api.Issue.HoldReason, record.HoldReason).
	// Older than the floor, and read with a literal of its one value until OwnerPlanContract,
	// which added `task-stopped` and `children`; an issue held for either is served without
	// its `holdReason` below that contract (shaping.go's withoutUnadmittedHoldReason).
	{Answer: AnswerState, KeyPath: "issues.*.holdReason", Values: holdReasons},
	// `reason` of one open give-up of the intake, why it gave the message up (api.IntakeGiveUpView,
	// record.IntakeGiveUpReason). It arrived with `intakeGiveUps`, which is served down below that
	// contract whole (shaping.go's withoutIntakeGiveUps), so no body below 228 carries a value of
	// it.
	{Answer: AnswerState, KeyPath: "intakeGiveUps.newest.*.reason", Values: intakeGiveUpReasons},
}

// valuesFrom is every value of a field whose list has not moved since the field arrived: each
// value recorded at that one contract, sorted by value as the type requires. A field whose list
// did grow is written out value by value instead, with each value's own arrival (waitAnswers,
// strandedStates), or joined from one such list per contract (joinedValues). The values are the
// daemon's own constants wherever the tree declares them, so a word renamed there is renamed
// here.
func valuesFrom[V ~string](since int, values ...V) []EnumeratedValue {
	recorded := make([]EnumeratedValue, 0, len(values))
	for _, value := range values {
		recorded = append(recorded, EnumeratedValue{Value: string(value), Since: since})
	}
	slices.SortFunc(recorded, func(a, b EnumeratedValue) int { return strings.Compare(a.Value, b.Value) })
	return recorded
}

// arrivedWith is values as a field that arrived at contract carries them: each value recorded at
// the later of its own arrival and the field's, since a field carries no value older than itself
// (EnumeratedFields says why the earliest arrival of a list is its field's own). It is how a later
// key path of a field already enumerated elsewhere takes that field's list.
func arrivedWith(contract int, values []EnumeratedValue) []EnumeratedValue {
	recorded := slices.Clone(values)
	for i := range recorded {
		recorded[i].Since = max(recorded[i].Since, contract)
	}
	return recorded
}

// joinedValues is the lists of one field joined into one list, sorted by value as the type
// requires: a field whose list grew, written as the values it arrived with and then the values
// each later contract added to it (phases, workflowPhases).
func joinedValues(lists ...[]EnumeratedValue) []EnumeratedValue {
	joined := slices.Concat(lists...)
	slices.SortFunc(joined, func(a, b EnumeratedValue) int { return strings.Compare(a.Value, b.Value) })
	return joined
}

// waitAnswers is every value of `answeredBy`, which the two rows above share because they are
// one field of the daemon's own `record.Answer` read through two answers. The eight values of
// the eight acts that answered a root's wait before this daemon's floor are recorded from the
// floor up, as an answer older than the floor is; `declined`, a person who closed an ask of the
// root rather than answer it, arrived at 68 (version.go's 68, record.AnsweredByDecline).
var waitAnswers = []EnumeratedValue{
	{Value: string(record.AnsweredByApproval), Since: ServingFloor},
	{Value: string(record.AnsweredByAsk), Since: ServingFloor},
	{Value: string(record.AnsweredByChanges), Since: ServingFloor},
	{Value: string(record.AnsweredByClosed), Since: ServingFloor},
	{Value: string(record.AnsweredByDecline), Since: 68},
	{Value: string(record.AnsweredByReply), Since: ServingFloor},
	{Value: string(record.AnsweredByRetraction), Since: ServingFloor},
	{Value: string(record.AnsweredByTurn), Since: ServingFloor},
	{Value: string(record.AnsweredByVersion), Since: ServingFloor},
}

// strandedStates is every state a claim nothing will wake can be in. `suspended` and `failed`
// are older than the floor; `retired` arrived at retiredStrandedContract, and an entry carrying
// it is left out of a body below that contract rather than rewritten, since nothing of such an
// entry is true once its state is gone (shaping.go's withoutUnadmittedStrandedStates).
var strandedStates = []EnumeratedValue{
	{Value: string(supervise.StateFailed), Since: ServingFloor},
	{Value: string(supervise.StateRetired), Since: retiredStrandedContract},
	{Value: string(supervise.StateSuspended), Since: ServingFloor},
}

// phases is every value the plugin's enumeration of an issue's `phase` names
// (`LEGION_WORKFLOW_PHASES` in `packages/contracts/src/legion-api.ts`, with `unrecorded`, what an
// issue the workflow does not record reads as), and `legion-api.test.ts` holds this list equal to
// that enumeration. A record's domain is what a reader admits, not what this daemon writes, so
// the list is the reader's: `integrating` is a member of that enumeration and no phase this
// daemon writes, since phase.All has never held it. The field is older than the floor, and the
// list has lost one value since: `awaiting_merge`, which contract 86 took out of the workflow and
// out of that enumeration (version.go's 86). It has gained one: `step`, a step of a role the
// deployment defines, at deploymentRolesContract, below which an issue at step is served as
// implementing (shaping.go's withoutDeploymentRoles).
//
// A removed value needs no serving-down, which is why no row below records `awaiting_merge` at
// any contract. This table records each value's arrival (`Since`) and nothing else, and the one
// question it answers is whether this daemon emits a value an older reader's enumeration does
// not carry. A value's absence from a body is not observable: a reader below 86 whose own enum
// still names `awaiting_merge` refuses nothing for never meeting it, as it refuses nothing for
// an `omitempty` key no issue carries. So a value leaves this list the day the enumeration of
// this daemon's own contract drops it, and every other value's row stays where it is. The list
// stays literal for that reason: `integrating` and `unrecorded` are no phase.Phase constant.
var phases = joinedValues(
	valuesFrom(ServingFloor,
		"admitted", "awaiting_person", "planning", "implementing", "testing", "reviewing", "accepting",
		"retro", "merging", "integrating", "done", "held", "unrecorded"),
	valuesFrom(deploymentRolesContract, "step"))

// workflowPhases is every phase of the workflow, which two fields of the decided record share:
// an issue's `heldFrom` and the phase a claim's held task was queued for. It is the list of
// `phases` without `unrecorded`, since `unrecorded` is what an issue the workflow holds no row
// for reads its own `phase` as and is no phase a hold or a task names. Both fields arrived at
// recordFieldsContract, so the list is recorded there, and it lost `awaiting_merge` and gained
// `step` with `phases` for the reasons that roster gives: a hold from step and a task queued for
// step are served as of implementing below deploymentRolesContract. It stays literal for the
// reason `phases` does: `integrating` is no phase.Phase constant.
var workflowPhases = joinedValues(
	valuesFrom(recordFieldsContract,
		"admitted", "awaiting_person", "planning", "implementing", "testing", "reviewing", "accepting",
		"retro", "merging", "integrating", "done", "held"),
	valuesFrom(deploymentRolesContract, "step"))

// failureCauses is every word the site that failed a claim can choose
// (supervise.FailureCause), which the failure record's `cause` and the `pendingCause` beside it
// share. Both arrived at recordFieldsContract, so the list is recorded there. It names each
// constant rather than reading supervise.FailureCauses, because a cause added there later
// arrives at its own contract and not at this one.
var failureCauses = valuesFrom(recordFieldsContract,
	supervise.CauseLaunchRefused, supervise.CauseDied, supervise.CauseNeverRegistered,
	supervise.CauseStalePlugin, supervise.CausePromptRetiresExhausted, supervise.CauseProviderError,
	supervise.CauseUnrecorded)

// lineParts is every part of the admission's waiting line a member can stand in, and whyStates
// every fact that holds an issue that is not working now. Both arrived at
// whyAndLineFieldsContract, so each list is recorded there. lineParts stays literal:
// record.LinePart is a numbered constant (`int`) whose words come from its String method, so no
// string constant of the tree spells them.
var lineParts = valuesFrom(whyAndLineFieldsContract, "returned", "starting")
var whyStates = valuesFrom(whyAndLineFieldsContract, WhyClaimStopped, WhyWaiting, WhyBlocked, WhyUnlisted)

// lineKinds is every reason a member stands in the waiting line. The field arrived at
// whyAndLineFieldsContract with four words, and its list grew once: `waiting_on_issue`, a
// started issue that gave its admission slot back to wait on another issue, arrived at
// waitingOnIssueContract (version.go's 132, record.LineWaitingOnIssue). So it is written out
// value by value, sorted by value, as waitAnswers is. It stays literal for the reason lineParts
// does: record.LineKind is a numbered constant whose words come from its String method.
var lineKinds = []EnumeratedValue{
	{Value: "answered", Since: whyAndLineFieldsContract},
	{Value: "first_phase", Since: whyAndLineFieldsContract},
	{Value: "queued_root", Since: whyAndLineFieldsContract},
	{Value: "returning", Since: whyAndLineFieldsContract},
	{Value: "waiting_on_issue", Since: waitingOnIssueContract},
}

// heldReasons is every word of a held row's `reason`. The field arrived at heldContract with
// eleven words, and its list grew once: `children_before_retro`, a parent whose task's last step
// is not an acceptance, held before its retro for the issues beneath it, and
// `task_stopped`, a task its owner stopped, arrived at OwnerPlanContract (version.go's 219). So
// it is written out value by value, sorted by value, as waitAnswers is. It names each constant
// rather than reading record.HeldReasons, because a word added to the ladder later arrives at
// its own contract.
var heldReasons = []EnumeratedValue{
	{Value: string(record.HeldAcceptedAncestor), Since: heldContract},
	{Value: string(record.HeldAncestry), Since: heldContract},
	{Value: string(record.HeldAtWork), Since: heldContract},
	{Value: string(record.HeldChildrenBeforeRetro), Since: OwnerPlanContract},
	{Value: string(record.HeldChildrenFirst), Since: heldContract},
	{Value: string(record.HeldDesignGate), Since: heldContract},
	{Value: string(record.HeldDispatchBlocker), Since: heldContract},
	{Value: string(record.HeldLabelOff), Since: heldContract},
	{Value: string(record.HeldRootAwaitingPerson), Since: heldContract},
	{Value: string(record.HeldTaskStopped), Since: OwnerPlanContract},
	{Value: string(record.HeldTreeClosed), Since: heldContract},
	{Value: string(record.HeldTreeParked), Since: heldContract},
	{Value: string(record.HeldUnclassified), Since: heldContract},
}

// holdReasons is every reason an issue's hold can carry (record.HoldReason). `escalated` is older
// than the floor; `children`, a parent whose task's last step is not an acceptance waiting before
// its retro for the issues beneath it, and `task-stopped`, a task its owner stopped, arrived at
// OwnerPlanContract. Written out value by value, sorted by value, as waitAnswers is.
var holdReasons = []EnumeratedValue{
	{Value: string(record.HoldForChildren), Since: OwnerPlanContract},
	{Value: string(record.HoldEscalated), Since: ServingFloor},
	{Value: string(record.HoldTaskStopped), Since: OwnerPlanContract},
}

// claimViewStates is every value of a claim view's `state`, in the order a launch reaches them.
// The field is older than the floor and the list has not moved since.
var claimViewStates = valuesFrom(ServingFloor,
	supervise.StateQueued, supervise.StateLaunchUncertain, supervise.StateLaunching,
	supervise.StateShimConnected, supervise.StateRegistered, supervise.StateReady,
	supervise.StateWorking, supervise.StateIdle, supervise.StateSuspended, supervise.StateFailed,
	supervise.StateRetired)

// floorRoles is every role a claim could hold at ServingFloor, named constant by constant. It
// names each one rather than reading claim.Roles, as failureCauses does its causes, because these
// lists record the contract a value arrived at.
var floorRoles = []claim.Role{
	claim.RoleArchitect, claim.RolePlanner, claim.RoleImplementer, claim.RoleTester,
	claim.RoleReviewer, claim.RoleMerger,
}

// claimRoles is every role a claim can hold below deploymentRolesContract, which four rows share:
// the stranded report's, the registration's, the wake's and the pause stop's, those last two at
// their own arrivals (wakeRoles, pauseStopRoles). The list has not moved since the floor.
var claimRoles = valuesFrom(ServingFloor, floorRoles...)

// wakeRoles is the same list of roles at the contract `wakesWaiting` arrived at: no wake of that
// array is served below it at all (shaping.go's withoutWakesWaiting).
var wakeRoles = valuesFrom(wakesWaitingContract, floorRoles...)

// pauseStopRoles is the same list of roles at the contract `paused` arrived at: no `paused` is
// served below that contract at all (shaping.go's withoutPause).
var pauseStopRoles = valuesFrom(pauseContract, floorRoles...)

// updateStates is every value of a claim view's `updateState`, recorded at
// shapedAnswersContract, the contract that added the key. The words are the four the package
// declares (updatestate.go), so a state renamed there is renamed here and the domain cannot spell
// one the daemon never writes.
var updateStates = valuesFrom(shapedAnswersContract,
	UpdateStateNone, UpdateStateOpen, UpdateStateAsked, UpdateStateAnswered)

// pullRequestStates is every state a recorded pull request is in. The field is older than the
// floor (version.go's 36) and the list has not moved since.
var pullRequestStates = valuesFrom(ServingFloor,
	record.PullRequestOpen, record.PullRequestMerged, record.PullRequestClosed)

// mergeStates is every value of a pull request's `mergeState`, GitHub's own merge state for the
// head in GitHub's own lowercase spelling. The field arrived at stateMachineContract and the
// list is recorded there, so no caller below 86 is served a value of it: that caller is served
// the pull request without the key at all (shaping.go's withoutStateMachineFields).
// record.MergeStateUnread is no value of the field: it is the empty state the view leaves off.
var mergeStates = valuesFrom(stateMachineContract,
	record.MergeStateClean, record.MergeStateUnstable, record.MergeStateHasHooks,
	record.MergeStateBlocked, record.MergeStateDirty, record.MergeStateBehind, record.MergeStateDraft,
	record.MergeStateUnknown)

// mergeWaitStates is the same eight words at the contract `mergeWait` arrived at, which the
// object's `mergeState` and `toldState` share: no `mergeWait` is served below that contract at
// all (shaping.go's withoutMergeWait).
var mergeWaitStates = valuesFrom(mergeWaitContract,
	record.MergeStateClean, record.MergeStateUnstable, record.MergeStateHasHooks,
	record.MergeStateBlocked, record.MergeStateDirty, record.MergeStateBehind, record.MergeStateDraft,
	record.MergeStateUnknown)

// designSteps is every step of a deployment's design gate, and gatedStepStatuses every place an
// issue can stand in one. Both are recorded at gatedStepsContract, the contract `gatedSteps`
// arrived at.
var designSteps = valuesFrom(gatedStepsContract,
	record.DesignStepDecisions, record.DesignStepCriteria, record.DesignStepSpec)
var gatedStepStatuses = valuesFrom(gatedStepsContract,
	GatedStepUnregistered, GatedStepWaiting, GatedStepApproved, GatedStepChangesRequested)

// memoryGroups is every group a tree's memory draw can give it, and memoryDrawModes every mode
// it can be drawn under. Both are recorded at memoryContract, the contract the draw arrived at.
// Since 141 the three memory fields are read off the answer of the experiment named memory
// (internal/projection's memoryOf), whose two arms are these two groups and whose modes are these
// three words, so the words on the wire did not move.
var memoryGroups = valuesFrom(memoryContract, config.MemoryVariantMemory, config.MemoryVariantNone)
var memoryDrawModes = valuesFrom(memoryContract, config.ExperimentOff, config.ExperimentOn, config.ExperimentRandom)

// controllerRuntimes is every runtime the registered controller's locator names. The field is
// older than the floor and the list has not moved since. It stays a literal: the runtime
// package's constants name a locator's backend (`tmux`, `sandbox`), and this field's second
// word is `kubernetes`, which no constant spells with this meaning.
var controllerRuntimes = valuesFrom(ServingFloor, "tmux", "kubernetes")

// capabilityStatuses is every status one row of the deployment's capability report can be in.
// The report is older than the floor (version.go's 16), and the list has not moved since. The
// probe's own table prints three words more — `missing`, `reported` and `installed` — which reach
// no body this daemon writes and are no values of this field, so capabilities.StatusMissing and
// capabilities.StatusReported are left out here.
var capabilityStatuses = valuesFrom(ServingFloor,
	capabilities.StatusPresent, capabilities.StatusUnchecked, capabilities.StatusWithheld,
	capabilities.StatusDecided, capabilities.StatusOpen)

// capabilitySources is every source a deployment row's decision can come from. The field is
// older than the floor (version.go's 25), and `configuration` and `boot` came with it. Its list
// grew once: `caches`, what the repository caches' last health reads said of their room, the
// source of the row `repository-cache-room`, arrived at cachesSourceContract (version.go's 213,
// capabilities.SourceCaches). So it is written out value by value, sorted by value, as
// waitAnswers is, and a row of that source is served without its `source` to a caller below that
// contract (shaping.go's withoutUnadmittedCapabilitySources).
var capabilitySources = []EnumeratedValue{
	{Value: string(capabilities.SourceBoot), Since: ServingFloor},
	{Value: string(capabilities.SourceCaches), Since: cachesSourceContract},
	{Value: string(capabilities.SourceConfiguration), Since: ServingFloor},
}

// cacheStates is every state a repository cache under the Kubernetes runtime can be in. The
// field is older than the floor (version.go's 32) and the list has not moved since. It stays a
// literal: the words are spelled only by unexported constants of the sandbox runtime package.
var cacheStates = valuesFrom(ServingFloor, "starting", "cloning", "ready", "failing", "refused")

// liveCacheStates is the same five states at the contract `repositories.live` arrived at, which
// each live entry's cache takes: no `live` is served below that contract at all (shaping.go's
// withoutRepositoriesLive), so the values arrived with the key.
var liveCacheStates = valuesFrom(repositoriesLiveContract, "starting", "cloning", "ready", "failing", "refused")

// intakeGiveUpReasons is every word of an open give-up's `reason`, recorded at
// intakeGiveUpsContract, the contract `intakeGiveUps` arrived at. It names each of the record
// package's constants, so a reason added there later is no value of this list until the contract
// that adds it records it here.
var intakeGiveUpReasons = valuesFrom(intakeGiveUpsContract,
	record.GiveUpBound, record.GiveUpDidNotEnd, record.GiveUpRetention, record.GiveUpUndecodable,
	record.GiveUpConsumerMovedPast)

// replayOutcomes is every way the replay of one give-up can end, the `outcome` of each record
// the controller's replay answers, recorded at intakeGiveUpsContract, the contract that answer
// arrived at (AnswerIntakeReplay). It names each constant for the reason intakeGiveUpReasons
// does.
var replayOutcomes = valuesFrom(intakeGiveUpsContract,
	intake.ReplayApplied, intake.ReplayAlreadyApplied, intake.ReplayNoFact, intake.ReplayFailed)

package api

import (
	"slices"
	"strings"
)

// ServingFloor is the lowest plugin contract this daemon shapes an answer for. A caller that
// declares less than this is served what a caller at the floor is served, and a caller that
// declares nothing is served this daemon's own shape.
//
// 46 is the reading this change records: no claim holding a process recorded a contract below 46.
// The 50 claims at contracts 36 to 43 are all suspended, so they call nothing.
//
// The added-key half of the gate is bounded, and the removed-key half is not. A plugin built
// before dispatch://INFRA-1048 reads an answer with a strict schema and refuses the whole body
// for one key it does not know. dispatch://INFRA-1048 landed the reader that drops an unknown
// key instead, and the first release carrying that reader declares 49. So once
// `lowestPluginContract` on the state reports 49 or more, no live reader refuses a key it does
// not know, and the added-key half of the gate may be dropped with the record entries that
// serve it. The removed-key half stays for good: a reader still needs the key its own contract
// requires, whatever this daemon stopped writing.
const ServingFloor = 46

// SupportedContracts is every contract this daemon describes a shape for, from ServingFloor up
// to DaemonAPIVersion. It is the numbers version.go carries a numbered entry for, and nothing
// else: 47 and 50 have no entry, because no change ever shipped under either, so they are in no
// list. `version_test.go` reads the entry headers out of version.go and holds this list to them
// in both directions.
var SupportedContracts = []int{46, 48, 49, 51, 52, 53, 54, 55, 56, 57, 58, 59, 62, 64, 68, 73, 81}

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
	AnswerWaveRelease        AnswerID = "wave-release"
	AnswerRefusal            AnswerID = "refusal"
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
}

// Answers is one row per answer a plugin parses. The operator's claim answers and the
// controller's secret and credential answers are no rows: no plugin parses them.
var Answers = []Answer{
	// Older than the floor: `GET /legion/v1/state` predates every numbered entry of version.go.
	{ID: AnswerState, Routes: []string{"GET /legion/v1/state"}, Since: ServingFloor},
	// version.go's 81: the per-issue record route a role reads in place of the whole state.
	// This change adds the route, so no contract below 81 records it.
	{ID: AnswerIssueRecord, Routes: []string{"GET /legion/v1/issues/{key}/record"}, Since: shapedAnswersContract},
	// version.go's 81: the typed health body (`api.Health`), which this change puts in place of
	// the one untyped key the route answered, so the typed body has no record below 81 either.
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
	// Older than the floor: the empty object the eight fact routes answer.
	{ID: AnswerEmptyObject, Routes: []string{
		"POST /legion/v1/issues/status",
		"POST /legion/v1/gates/register",
		"POST /legion/v1/phase/backward",
		"POST /legion/v1/phase/retry",
		"POST /legion/v1/signoff",
		"POST /legion/v1/roots/close",
		"POST /legion/v1/children/park",
		"POST /legion/v1/children/rerun",
	}, Since: ServingFloor},
	// version.go's 59: the controller's fresh-session recovery (`api.FreshSessionResponse`).
	{ID: AnswerFreshSession, Routes: []string{"POST /legion/v1/claims/fresh-session"}, Since: 59},
	// version.go's 59: the controller's compaction recovery (`api.CompactResponse`).
	{ID: AnswerCompact, Routes: []string{"POST /legion/v1/claims/compact"}, Since: 59},
	// Older than the floor: the wave release's answer, which version.go's 38 gave its `started`
	// and `held` and which 46 is the floor for here.
	{ID: AnswerWaveRelease, Routes: []string{"POST /legion/v1/waves/release"}, Since: ServingFloor},
	// Older than the floor: one sentence under `error`, which every route refuses with and which
	// a client parses on every answer that is not a success. It has no route of its own.
	{ID: AnswerRefusal, Routes: []string{}, Since: ServingFloor},
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
// both halves of this change read the table: the serving-down takes from a caller's body every
// value whose arrival is above that caller's contract (shaping.go's
// withoutUnadmittedWaitAnswer), and the records carry the value domain of each field beside its
// key paths, so a value added with no serving-down fails the gate as an added key does
// (contract_shapes_test.go).
//
// A field no row declares is a field the records carry no domain for and the gate holds to
// nothing, so a strictly parsed field belongs here the day it arrives. What counts as one: a
// field of an answer of Answers that `packages/contracts/src/legion-api.ts` reads with a
// `z.enum`, at every key path it is reachable at. The whole-project body's `issues.*.…` and the
// record's bare `…` are two rows of one field, as the two `answeredBy` rows are. A field read
// with a `z.literal` is no row — `issues.*.holdReason`, `controllerLocator.external` and the
// discriminators of the two discriminated unions, `locator.runtime` and `waitingOn.kind` —
// because a literal admits the one value its own shape carries and a daemon that widened it
// would be changing the field's type rather than its domain.
//
// Each value's arrival is the later of two readings, and each row says which of them decided
// it: the contract the value first appears in the plugin's own list at, and the contract the
// field itself arrived at, since a field that arrives at 55 can carry no value older than 55.
// The first reading is measured rather than guessed: `legion-api.ts` and `legion-roles.ts` were
// read at the commit that declared each contract of SupportedContracts, and the members of
// every `z.enum` diffed. Exactly two of the lists below ever grew — `answeredBy` gained
// `declined` at 68, and `strandedClaims[].state` gained `retired` at 56 — so every other list
// is recorded whole at its own field's arrival. (`suspendedBy` gained `released` at 48 and is
// no row here: the operator claim answers carry it and no plugin parses them.)
var EnumeratedFields = []EnumeratedField{
	// `phase` of an issue, where the workflow holds it (api.Issue.Phase, LEGION_PHASES).
	{Answer: AnswerState, KeyPath: "issues.*.phase", Values: phases},
	// `state` of a claim view, where a claim is in its life (api.ClaimView.State,
	// LEGION_CLAIM_STATES). The architect's view and each worker's carry the one field.
	{Answer: AnswerState, KeyPath: "issues.*.architect.state", Values: claimViewStates},
	{Answer: AnswerState, KeyPath: "issues.*.workers.*.claim.state", Values: claimViewStates},
	// `updateState` of a claim view, where the claim's update round stands
	// (api.ClaimView.UpdateState). This change's own key, served down below its contract with
	// the rest of the round report (shaping.go's withoutClaimRoundReport).
	{Answer: AnswerState, KeyPath: "issues.*.architect.updateState", Values: updateStates},
	{Answer: AnswerState, KeyPath: "issues.*.workers.*.claim.updateState", Values: updateStates},
	// `state` of the pull request the daemon observed (api.PullRequestView.State,
	// record.PullRequestState).
	{Answer: AnswerState, KeyPath: "issues.*.pullRequest.state", Values: pullRequestStates},
	// `step` and `status` of one row of the deployment's design gate as the issue stands in it
	// (api.GatedStepView, record.DesignStep). Both arrived with `gatedSteps`, which is served
	// down below that contract whole (shaping.go's withoutGatedSteps).
	{Answer: AnswerState, KeyPath: "issues.*.gatedSteps.*.step", Values: designSteps},
	{Answer: AnswerState, KeyPath: "issues.*.gatedSteps.*.status", Values: gatedStepStatuses},
	// `answeredBy` of an `awaiting_person` root's wait, how the wait was answered
	// (api.WaitingOnView.AnsweredBy, record.Answer). The one row of the table with a value
	// younger than its own field, and the one a serving-down of a value is written for
	// (shaping.go's withoutUnadmittedWaitAnswer).
	{Answer: AnswerState, KeyPath: "issues.*.waitingOn.answeredBy", Values: waitAnswers},
	// `memoryGroup` and `memoryDrawMode` of the issue's tree, the group drawn and the mode it
	// was drawn under (api.Issue). Both arrived with the memory draw, which is served down
	// below that contract whole (shaping.go's withoutMemory).
	{Answer: AnswerState, KeyPath: "issues.*.memoryGroup", Values: memoryGroups},
	{Answer: AnswerState, KeyPath: "issues.*.memoryDrawMode", Values: memoryDrawModes},
	// `role` and `state` of one claim nothing will wake (api.StrandedClaim). The state is the
	// second row of the table whose list grew: `retired` arrived at 56, and an entry carrying
	// it is dropped whole below that contract (shaping.go's withoutRetiredStranded), so no
	// body below 56 carries the value and the gate passes without a serving-down of its own.
	{Answer: AnswerState, KeyPath: "strandedClaims.*.role", Values: claimRoles},
	{Answer: AnswerState, KeyPath: "strandedClaims.*.state", Values: strandedStates},
	// `role` of one wake the pacing holds (api.WakeWaiting.Role). It arrived with
	// `wakesWaiting`, which is served down below that contract whole (shaping.go's
	// withoutWakesWaiting). The wake's `kind` beside it is no row: the schema leaves it an
	// unconstrained string, so a lane a later daemon names is read rather than refused.
	{Answer: AnswerState, KeyPath: "wakesWaiting.*.role", Values: wakeRoles},
	// `runtime` of the registered controller's locator, the runtime this daemon runs under
	// (api.ControllerLocator.Runtime).
	{Answer: AnswerState, KeyPath: "controllerLocator.runtime", Values: controllerRuntimes},
	// `status` and `source` of one row of the deployment's capability report
	// (api.CapabilityState, capabilities.Deployment.Report).
	{Answer: AnswerState, KeyPath: "capabilities.*.status", Values: capabilityStatuses},
	{Answer: AnswerState, KeyPath: "capabilities.*.source", Values: capabilitySources},
	// `state` of a repository cache under the Kubernetes runtime (api.CacheView.State). The
	// mapping carries one per mapped repository and one per cache of a repository the mapping
	// no longer names, which are two key paths of the one field.
	{Answer: AnswerState, KeyPath: "repositories.mapped.*.cache.state", Values: cacheStates},
	{Answer: AnswerState, KeyPath: "repositories.caches.*.state", Values: cacheStates},
	// The record route answers the very value the whole-project body holds under
	// `issues.<key>`, so every field of an issue above has its row here too, at the path the
	// record writes it, and the two rows of one field share one list.
	{Answer: AnswerIssueRecord, KeyPath: "phase", Values: phases},
	{Answer: AnswerIssueRecord, KeyPath: "architect.state", Values: claimViewStates},
	{Answer: AnswerIssueRecord, KeyPath: "workers.*.claim.state", Values: claimViewStates},
	{Answer: AnswerIssueRecord, KeyPath: "architect.updateState", Values: updateStates},
	{Answer: AnswerIssueRecord, KeyPath: "workers.*.claim.updateState", Values: updateStates},
	{Answer: AnswerIssueRecord, KeyPath: "pullRequest.state", Values: pullRequestStates},
	{Answer: AnswerIssueRecord, KeyPath: "gatedSteps.*.step", Values: designSteps},
	{Answer: AnswerIssueRecord, KeyPath: "gatedSteps.*.status", Values: gatedStepStatuses},
	{Answer: AnswerIssueRecord, KeyPath: "waitingOn.answeredBy", Values: waitAnswers},
	{Answer: AnswerIssueRecord, KeyPath: "memoryGroup", Values: memoryGroups},
	{Answer: AnswerIssueRecord, KeyPath: "memoryDrawMode", Values: memoryDrawModes},
	// `role` of the claim a registration issued (claim.RegisterResponse.Role). The controller's
	// registration answers `controller` under a `z.literal` and is no row.
	{Answer: AnswerRegister, KeyPath: "role", Values: claimRoles},
}

// valuesFrom is every value of a field whose list has not moved since the field arrived: each
// value recorded at that one contract, sorted by value as the type requires. A field whose list
// did grow is written out value by value instead, with each value's own arrival (waitAnswers,
// strandedStates).
func valuesFrom(since int, values ...string) []EnumeratedValue {
	recorded := make([]EnumeratedValue, 0, len(values))
	for _, value := range values {
		recorded = append(recorded, EnumeratedValue{Value: value, Since: since})
	}
	slices.SortFunc(recorded, func(a, b EnumeratedValue) int { return strings.Compare(a.Value, b.Value) })
	return recorded
}

// waitAnswers is every value of `answeredBy`, which the two rows above share because they are
// one field of the daemon's own `record.Answer` read through two answers. The eight values of
// the eight acts that answered a root's wait before this daemon's floor are recorded from the
// floor up, as an answer older than the floor is; `declined`, a person who closed an ask of the
// root rather than answer it, arrived at 68 (version.go's 68, record.AnsweredByDecline).
var waitAnswers = []EnumeratedValue{
	{Value: "approved", Since: ServingFloor},
	{Value: "ask", Since: ServingFloor},
	{Value: "changes_requested", Since: ServingFloor},
	{Value: "closed", Since: ServingFloor},
	{Value: "declined", Since: 68},
	{Value: "reply", Since: ServingFloor},
	{Value: "retracted", Since: ServingFloor},
	{Value: "turn", Since: ServingFloor},
	{Value: "version", Since: ServingFloor},
}

// strandedStates is every state a claim nothing will wake can be in. `suspended` and `failed`
// are older than the floor; `retired` arrived at retiredStrandedContract, and an entry carrying
// it is left out of a body below that contract rather than rewritten, since nothing of such an
// entry is true once its state is gone (shaping.go's withoutRetiredStranded).
var strandedStates = []EnumeratedValue{
	{Value: "failed", Since: ServingFloor},
	{Value: "retired", Since: retiredStrandedContract},
	{Value: "suspended", Since: ServingFloor},
}

// phases is every value of an issue's `phase`: the workflow's own phases and `unrecorded`, what
// an issue the workflow does not record reads as. The field is older than the floor and the
// list has not moved since.
var phases = valuesFrom(ServingFloor,
	"admitted", "awaiting_person", "planning", "implementing", "testing", "reviewing", "accepting",
	"retro", "merging", "awaiting_merge", "integrating", "done", "held", "unrecorded")

// claimViewStates is every value of a claim view's `state`, in the order a launch reaches them.
// The field is older than the floor and the list has not moved since.
var claimViewStates = valuesFrom(ServingFloor,
	"queued", "launch_uncertain", "launching", "shim_connected", "registered", "ready", "working",
	"idle", "suspended", "failed", "retired")

// claimRoles is every role a claim can hold, which three rows share: the stranded report's, the
// registration's and the wake's, that last one at its own arrival (wakeRoles). The list has not
// moved since the floor.
var claimRoles = valuesFrom(ServingFloor,
	"architect", "planner", "implementer", "tester", "reviewer", "merger")

// wakeRoles is the same list of roles at the contract `wakesWaiting` arrived at: no wake of that
// array is served below it at all (shaping.go's withoutWakesWaiting).
var wakeRoles = valuesFrom(wakesWaitingContract,
	"architect", "planner", "implementer", "tester", "reviewer", "merger")

// updateStates is every value of a claim view's `updateState`, recorded at the contract this
// change adds the key under.
var updateStates = valuesFrom(shapedAnswersContract, "none", "open", "asked", "push-check-reask", "answered")

// pullRequestStates is every state a recorded pull request is in. The field is older than the
// floor (version.go's 36) and the list has not moved since.
var pullRequestStates = valuesFrom(ServingFloor, "open", "merged", "closed")

// designSteps is every step of a deployment's design gate, and gatedStepStatuses every place an
// issue can stand in one. Both are recorded at gatedStepsContract, the contract `gatedSteps`
// arrived at.
var designSteps = valuesFrom(gatedStepsContract, "decisions", "criteria", "spec")
var gatedStepStatuses = valuesFrom(gatedStepsContract, "unregistered", "waiting", "approved", "changes_requested")

// memoryGroups is every group a tree's memory draw can give it, and memoryDrawModes every mode
// it can be drawn under. Both are recorded at memoryContract, the contract the draw arrived at.
var memoryGroups = valuesFrom(memoryContract, "memory", "none")
var memoryDrawModes = valuesFrom(memoryContract, "off", "on", "random")

// controllerRuntimes is every runtime the registered controller's locator names. The field is
// older than the floor and the list has not moved since.
var controllerRuntimes = valuesFrom(ServingFloor, "tmux", "kubernetes")

// capabilityStatuses is every status one row of the deployment's capability report can be in,
// and capabilitySources every source a deployment row's decision can come from. The report is
// older than the floor (version.go's 16) and so is the source (version.go's 25), and neither
// list has moved since. The probe's own table prints three words more — `missing`, `reported`
// and `installed` — which reach no body this daemon writes and are no values of this field.
var capabilityStatuses = valuesFrom(ServingFloor, "present", "unchecked", "withheld", "decided", "open")
var capabilitySources = valuesFrom(ServingFloor, "configuration", "boot")

// cacheStates is every state a repository cache under the Kubernetes runtime can be in. The
// field is older than the floor (version.go's 32) and the list has not moved since.
var cacheStates = valuesFrom(ServingFloor, "starting", "cloning", "ready", "failing", "refused")

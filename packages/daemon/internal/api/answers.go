package api

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
var SupportedContracts = []int{46, 48, 49, 51, 52, 53, 54, 55, 56, 57, 58, 59, 62, 64, 68, 81}

// AnswerID names one answer a plugin parses. It is stable: a later slice routes every write
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
	// version.go's 81: the per-issue record route a role reads in place of the whole state. The
	// route does not exist yet, and this change adds it, so no contract below 81 records it.
	{ID: AnswerIssueRecord, Routes: []string{"GET /legion/v1/issues/{key}/record"}, Since: 81},
	// version.go's 81: the typed health body (`api.Health`). The route answers one untyped key
	// today, so the typed body has no record below 81 either.
	{ID: AnswerHealth, Routes: []string{"GET /healthz"}, Since: 81},
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
// value whose arrival is above that caller's contract (server.go's withoutUnadmittedWaitAnswer),
// and the records carry the value domain of each field beside its key paths, so a value added
// with no serving-down fails the gate as an added key does (contract_shapes_test.go).
//
// A field no row declares is a field the records carry no domain for and the gate holds to
// nothing, so a strictly parsed field belongs here the day it arrives.
var EnumeratedFields = []EnumeratedField{
	// `answeredBy` of an `awaiting_person` root's wait, how the wait was answered
	// (api.WaitingOnView.AnsweredBy, record.Answer). The whole-project body carries it under
	// each issue and the record route answers the same issue on its own, so the field is one
	// field of two answers and both rows declare the same values.
	{Answer: AnswerState, KeyPath: "issues.*.waitingOn.answeredBy", Values: waitAnswers},
	{Answer: AnswerIssueRecord, KeyPath: "waitingOn.answeredBy", Values: waitAnswers},
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

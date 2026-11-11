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
var SupportedContracts = []int{46, 48, 49, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60}

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
	// version.go's 60: the per-issue record route a role reads in place of the whole state. The
	// route does not exist yet, and this change adds it, so no contract below 60 records it.
	{ID: AnswerIssueRecord, Routes: []string{"GET /legion/v1/issues/{key}/record"}, Since: 60},
	// version.go's 60: the typed health body (`api.Health`). The route answers one untyped key
	// today, so the typed body has no record below 60 either.
	{ID: AnswerHealth, Routes: []string{"GET /healthz"}, Since: 60},
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

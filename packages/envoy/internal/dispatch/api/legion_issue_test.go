package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// The read of one issue's Legion state, GET /api/v1/issues/{key}/legion (dispatch://INFRA-4869):
// whether the issue's project names a daemon, the issue's phase, when Dispatch stored it and
// whether Dispatch saw it start, each role with a claim and its state, its slot, its place in the
// daemon's waiting line, what admission.blocked names for it, the daemon's waitingOn, the newest
// read of the project's daemon, and the runs' revision the live view moves on. The rows read the
// route over the wire, through dispatchRequest, and decode its body into the structs below rather
// than the route's own type, so a row holds what a page reads. Each row states its answer as
// literal values of its own seed, never as the shared per-issue function computes them, so
// removing the place, the blockers, a role or a field of waitingOn from that function fails the
// rows that seed them (a-c-read-route's negative control). Each case that is a state of a whole
// project (no admission row, no read answered, no daemon named, a failed newest read, the full
// state) has a project of its own, since no two of those hold of one project at one moment.

// issueLegionKind is the live view kind of one issue's Legion state by the name a page sends it
// under (the plan's decision 7). The tests write the wire's word, so they hold the name a page
// sends whatever the server's own constant is called.
const issueLegionKind = "legion_issue"

// wireLegionIssue is GET /api/v1/issues/{key}/legion's answer as a page decodes it (the plan's
// decision 1): the issue, its project, whether that project names a daemon, its state or null
// where the daemon does not record it, the newest read of the project's daemon or null where none
// was recorded, and the runs' revision. A project that names no daemon is configured false, with
// a null state and a null read whatever an earlier daemon left, and still the runs' revision.
type wireLegionIssue struct {
	Issue        string                `json:"issue"`
	Project      string                `json:"project"`
	Configured   bool                  `json:"configured"`
	State        *wireLegionIssueState `json:"state"`
	Read         *wireLegionRead       `json:"read"`
	RunsRevision int64                 `json:"runs_revision"`
}

// wireLegionIssueState is the state of an issue the daemon records. WaitingOn stays the bytes the
// route sent, which a row decodes before it compares them: jsonb orders an object's keys its own
// way, so the bytes say nothing a page reads.
type wireLegionIssueState struct {
	Phase          string                 `json:"phase"`
	PhaseSince     time.Time              `json:"phase_since"`
	PhaseStartSeen bool                   `json:"phase_start_seen"`
	Roles          []wireLegionRole       `json:"roles"`
	Slot           *int                   `json:"slot"`
	WaitingLine    *wireLegionWaitingLine `json:"waiting_line"`
	BlockedBy      []string               `json:"blocked_by"`
	WaitingOn      json.RawMessage        `json:"waiting_on"`
}

// wireLegionRole is one role's claim and its state.
type wireLegionRole struct {
	Role  string `json:"role"`
	State string `json:"state"`
}

// wireLegionWaitingLine is where the issue stands in admission.waiting, 1 first, and that line's
// length.
type wireLegionWaitingLine struct {
	Place  int `json:"place"`
	Length int `json:"length"`
}

// wireLegionRead is the newest read of the project's daemon: when, whether it succeeded, and why
// it did not.
type wireLegionRead struct {
	At    time.Time `json:"at"`
	OK    bool      `json:"ok"`
	Error *string   `json:"error"`
}

// The members of the answer, of its state and of its read, sorted: decision 1's shape, which
// decodeLegionIssue holds every answer to before a row reads a value of it.
var (
	wireLegionIssueMembers = []string{"configured", "issue", "project", "read", "runs_revision", "state"}
	wireLegionStateMembers = []string{
		"blocked_by", "phase", "phase_since", "phase_start_seen", "roles", "slot", "waiting_line", "waiting_on",
	}
	wireLegionReadMembers = []string{"at", "error", "ok"}
)

// decodeLegionIssue decodes one answer, holding it to decision 1's shape first: exactly the six
// members, a state of exactly its eight and a read of exactly its three where either is not null,
// and a blocked_by that is an array, never null. A member the answer left out would decode as its
// zero value and pass a row that expects that value.
func decodeLegionIssue(t *testing.T, raw []byte) wireLegionIssue {
	t.Helper()
	members := requireLegionMembers(t, "the answer", raw, wireLegionIssueMembers)
	if state := members["state"]; string(state) != "null" {
		if blockedBy := requireLegionMembers(t, "the answer's state", state, wireLegionStateMembers)["blocked_by"]; string(blockedBy) == "null" {
			t.Fatalf("the answer's state.blocked_by is null, want an array, [] where nothing blocks the issue: %s", raw)
		}
	}
	if read := members["read"]; string(read) != "null" {
		requireLegionMembers(t, "the answer's read", read, wireLegionReadMembers)
	}
	var answer wireLegionIssue
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("decode the answer: %v: %s", err, raw)
	}
	return answer
}

// requireLegionMembers decodes raw as an object and requires exactly the members want names.
func requireLegionMembers(t *testing.T, what string, raw json.RawMessage, want []string) map[string]json.RawMessage {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("decode %s as an object: %v: %s", what, err, raw)
	}
	if got := slices.Sorted(maps.Keys(members)); !slices.Equal(got, want) {
		t.Fatalf("%s holds the members %v, want %v: %s", what, got, want, raw)
	}
	return members
}

// legionIssueOf is the answer GET /api/v1/issues/{key}/legion gives key, which must be a 200
// naming key and the project its key names.
func legionIssueOf(t *testing.T, handler http.Handler, key string) wireLegionIssue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/legion", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues/%s/legion: status=%d body=%s, want 200", key, response.Code, response.Body.String())
	}
	answer := decodeLegionIssue(t, response.Body.Bytes())
	requireLegionNames(t, key, answer)
	return answer
}

// requireLegionNames requires an answer to name key and the project key's prefix names, which is
// the issue row's project_key: no route moves an issue to another project.
func requireLegionNames(t *testing.T, key string, answer wireLegionIssue) {
	t.Helper()
	project, _, _ := strings.Cut(key, "-")
	if answer.Issue != key || answer.Project != project {
		t.Fatalf("the answer for %s names the issue %q of project %q, want %s of %s", key, answer.Issue, answer.Project, key, project)
	}
}

// legionIssueWant is one row's expected state, every value a literal of the row's own seed. A nil
// waitingOn is a waiting_on of null; a nil slot or line is a slot or waiting_line of null.
type legionIssueWant struct {
	phase     string
	since     time.Time
	seen      bool
	roles     []wireLegionRole
	slot      *int
	line      *wireLegionWaitingLine
	blockedBy []string
	waitingOn map[string]any
}

// requireLegionConfigured requires key's answer to say whether its project names a daemon as want
// does.
func requireLegionConfigured(t *testing.T, key string, answer wireLegionIssue, want bool) {
	t.Helper()
	if answer.Configured != want {
		t.Errorf("%s's answer is configured %t, want %t", key, answer.Configured, want)
	}
}

// legionIssueBody is a whole answer's expected values but the runs' revision: whether the project
// names a daemon, the state (nil for null) and the read (nil for null).
type legionIssueBody struct {
	configured bool
	state      *legionIssueWant
	read       *wireLegionRead
}

// requireLegionBody requires what's answer to hold body.
func requireLegionBody(t *testing.T, what string, answer wireLegionIssue, body legionIssueBody) {
	t.Helper()
	requireLegionConfigured(t, what, answer, body.configured)
	switch {
	case body.state != nil:
		requireLegionState(t, what, answer, *body.state)
	case answer.State != nil:
		t.Errorf("%s's state = %+v, want null", what, *answer.State)
	}
	requireLegionRead(t, what, answer, body.read)
}

// requireLegionState requires key's answer to hold the state want states, naming each member that
// differs. A state the daemon records is of a project that names a daemon, so the answer is
// configured.
func requireLegionState(t *testing.T, key string, answer wireLegionIssue, want legionIssueWant) {
	t.Helper()
	requireLegionConfigured(t, key, answer, true)
	got := answer.State
	if got == nil {
		t.Fatalf("%s's state is null, want the state in %q the daemon records", key, want.phase)
	}
	if got.Phase != want.phase {
		t.Errorf("%s's phase = %q, want %q", key, got.Phase, want.phase)
	}
	if !got.PhaseSince.Equal(want.since) {
		t.Errorf("%s's phase_since = %s, want %s, when Dispatch stored the phase", key, got.PhaseSince, want.since)
	}
	if got.PhaseStartSeen != want.seen {
		t.Errorf("%s's phase_start_seen = %t, want %t", key, got.PhaseStartSeen, want.seen)
	}
	if !slices.Equal(got.Roles, want.roles) {
		t.Errorf("%s's roles = %+v, want %+v", key, got.Roles, want.roles)
	}
	if !sameLegionValue(got.Slot, want.slot) {
		t.Errorf("%s's slot = %s, want %s", key, showLegionValue(got.Slot), showLegionValue(want.slot))
	}
	if !sameLegionValue(got.WaitingLine, want.line) {
		t.Errorf("%s's waiting_line = %s, want %s", key, showLegionValue(got.WaitingLine), showLegionValue(want.line))
	}
	if !slices.Equal(got.BlockedBy, want.blockedBy) {
		t.Errorf("%s's blocked_by = %v, want %v", key, got.BlockedBy, want.blockedBy)
	}
	var waitingOn any
	if err := json.Unmarshal(got.WaitingOn, &waitingOn); err != nil {
		t.Fatalf("decode %s's waiting_on %s: %v", key, got.WaitingOn, err)
	}
	var wantWaitingOn any
	if want.waitingOn != nil {
		wantWaitingOn = want.waitingOn
	}
	if !reflect.DeepEqual(waitingOn, wantWaitingOn) {
		t.Errorf("%s's waiting_on decodes to %v, want %v", key, waitingOn, wantWaitingOn)
	}
}

// requireLegionRead requires key's answer to carry the read want states, or null where want is nil.
func requireLegionRead(t *testing.T, key string, answer wireLegionIssue, want *wireLegionRead) {
	t.Helper()
	got := answer.Read
	switch {
	case got == nil && want == nil:
	case want == nil:
		t.Errorf("%s's read = %s, want null", key, showLegionValue(got))
	case got == nil:
		t.Errorf("%s's read is null, want %s", key, showLegionValue(want))
	case !got.At.Equal(want.At) || got.OK != want.OK || !sameLegionValue(got.Error, want.Error):
		t.Errorf("%s's read = %s, want %s", key, showLegionValue(got), showLegionValue(want))
	}
}

// sameLegionValue is whether two optional values are both null or hold equal values.
func sameLegionValue[T comparable](got, want *T) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

// showLegionValue prints an optional value as a failure message reads it.
func showLegionValue[T any](value *T) string {
	if value == nil {
		return "null"
	}
	return fmt.Sprintf("%+v", *value)
}

// requireLegionIssueNotFound requires GET /api/v1/issues/{key}/legion to refuse a key Dispatch
// holds no issue of as GET /api/v1/issues/{key} refuses it, 404 NOT_FOUND in the same words (the
// plan's decision 2): a mistyped key never reads as an issue Legion does not record.
func requireLegionIssueNotFound(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	issue := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	want := decodeBody[refusalAnswer](t, issue)
	if issue.Code != http.StatusNotFound || want.Code != "NOT_FOUND" {
		t.Fatalf("GET /api/v1/issues/%s: status=%d body=%s, want 404 NOT_FOUND", key, issue.Code, issue.Body.String())
	}
	routed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/legion", nil, "alice")
	var got refusalAnswer
	if err := json.Unmarshal(routed.Body.Bytes(), &got); err != nil || routed.Code != http.StatusNotFound || got != want {
		t.Fatalf("GET /api/v1/issues/%s/legion: status=%d body=%s, want 404 with GET /api/v1/issues/%s's refusal %+v",
			key, routed.Code, routed.Body.String(), key, want)
	}
}

// legionCardDisagreements is how the board's card for an issue and that issue's answer disagree on
// what both show (R1: "the card and the answer agree on the phase, the roles in a turn and the
// slot"), one sentence each, none where they agree. The card's roles are the roles in a turn,
// sorted by name, so they are held to the answer's roles whose state is `working` in that order.
func legionCardDisagreements(card legionBoardCard, answer wireLegionIssue) []string {
	state := answer.State
	if state == nil {
		return []string{fmt.Sprintf("%s carries a card in %q, and its answer's state is null", card.Key, card.Phase)}
	}
	var disagreements []string
	if card.Phase != state.Phase {
		disagreements = append(disagreements, fmt.Sprintf("%s's card is in %q, its answer in %q", card.Key, card.Phase, state.Phase))
	}
	working := []string{}
	for _, role := range state.Roles {
		if role.State == "working" {
			working = append(working, role.Role)
		}
	}
	slices.Sort(working)
	if !slices.Equal(card.Roles, working) {
		disagreements = append(disagreements, fmt.Sprintf("%s's card has %v in a turn, its answer %v", card.Key, card.Roles, working))
	}
	if !sameLegionValue(card.Slot, state.Slot) {
		disagreements = append(disagreements, fmt.Sprintf("%s's card holds slot %s, its answer slot %s",
			card.Key, showLegionValue(card.Slot), showLegionValue(state.Slot)))
	}
	return disagreements
}

// TestAnIssuesLegionRead is a-c-read-route: every read row of the plan, each against literal
// values of its seed. FULL is the full state: a daemon named, an admission row with a line of five
// and a blocked issue, and a good newest read after an older failed one. FAILED's newest read
// failed after a good one; NOLINE recorded no admission row; UNREAD's daemon never answered;
// NEVERREAD's was never read at all; and NODAEMON names no daemon, though an earlier one left its
// rows. Every answer of the first five is configured, and every one of NODAEMON is not.
func TestAnIssuesLegionRead(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "FULL", "FAILED", "NOLINE", "UNREAD", "NEVERREAD", "NODAEMON")
	for _, project := range []string{"FULL", "FAILED", "NOLINE", "UNREAD", "NEVERREAD", "NODAEMON"} {
		nameLegionDaemon(t, handler, project, "http://legion-"+strings.ToLower(project)+".internal:13370")
	}
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	hour := func(n int) time.Time { return at.Add(time.Duration(n) * time.Hour) }
	observe := func(project, key, field, value string, when time.Time) {
		t.Helper()
		seedLegionObservation(t, database, project, key, field, value, when)
	}

	// FULL. The parent comes before its child, which the issues table holds them to.
	for _, issue := range []struct {
		number      int
		title, rank string
		parent      string
	}{
		{1, "The slot holder", "a", ""},
		{2, "The third in the line", "b", ""},
		{3, "The root that holds a slot and stands first in the line", "c", ""},
		{4, "The issue two others block", "d", ""},
		{5, "The root waiting on an ask", "e", ""},
		{6, "The root waiting on a gate a person answered", "f", ""},
		{7, "The issue in acceptance", "g", ""},
		{8, "The slot holder's unstarted child", "h", "FULL-1"},
		{9, "The issue whose phase is its first", "i", ""},
		{10, "The issue whose phase moved", "j", ""},
		{11, "The issue that left the state and came back", "k", ""},
		{12, "The issue that left the state", "l", ""},
		{13, "The issue the daemon does not record", "m", ""},
		{15, "The issue with a retired architect and an auditor", "o", ""},
		{16, "The second in the line", "p", ""},
		{17, "The fourth in the line", "q", ""},
		{18, "The fifth in the line", "r", ""},
	} {
		seedLegionIssue(t, database, "FULL", issue.number, issue.title, issue.rank, nil, issue.parent)
	}
	claim := legion.ClaimFieldPrefix
	observe("FULL", "FULL-1", legion.FieldPhase, `"implementing"`, hour(1))
	observe("FULL", "FULL-1", legion.FieldSlot, `0`, hour(1))
	observe("FULL", "FULL-1", claim+"architect", `"working"`, hour(1))
	observe("FULL", "FULL-1", claim+"implementer", `"working"`, hour(1))
	observe("FULL", "FULL-1", claim+"tester", `"idle"`, hour(1))
	observe("FULL", "FULL-2", legion.FieldPhase, `"admitted"`, hour(1))
	observe("FULL", "FULL-3", legion.FieldPhase, `"planning"`, hour(1))
	observe("FULL", "FULL-3", legion.FieldSlot, `1`, hour(1))
	observe("FULL", "FULL-3", claim+"planner", `"working"`, hour(1))
	observe("FULL", "FULL-4", legion.FieldPhase, `"admitted"`, hour(1))
	observe("FULL", "FULL-5", legion.FieldPhase, `"awaiting_person"`, hour(1))
	observe("FULL", "FULL-5", legion.FieldWaitingOn,
		`{"kind":"ask","ask":"6f1c0f8e-3d4b-4a8e-9f51-2b7c1d9e0a11","since":"2026-11-20T08:30:00Z"}`, hour(1))
	observe("FULL", "FULL-6", legion.FieldPhase, `"awaiting_person"`, hour(1))
	observe("FULL", "FULL-6", legion.FieldWaitingOn,
		`{"kind":"gate","gateVersion":7,"artifactId":"0c9d2e4f-5a6b-4c7d-8e9f-a0b1c2d3e4f5","since":"2026-11-20T07:00:00Z","answeredAt":"2026-11-20T09:45:00Z","answeredBy":"approved"}`, hour(1))
	observe("FULL", "FULL-7", legion.FieldPhase, `"accepting"`, hour(1))
	observe("FULL", "FULL-8", legion.FieldPhase, `"admitted"`, hour(1))
	observe("FULL", "FULL-9", legion.FieldPhase, `"testing"`, hour(2))
	observe("FULL", "FULL-10", legion.FieldPhase, `"planning"`, hour(1))
	observe("FULL", "FULL-10", legion.FieldPhase, `"implementing"`, hour(3))
	observe("FULL", "FULL-11", legion.FieldPhase, `"planning"`, hour(1))
	observe("FULL", "FULL-11", legion.FieldPhase, `null`, hour(2))
	observe("FULL", "FULL-11", legion.FieldPhase, `"planning"`, hour(3))
	// FULL-12 left the daemon's state: every field it held is written null, as the sampler
	// writes a field the newest read no longer carries.
	observe("FULL", "FULL-12", legion.FieldPhase, `"reviewing"`, hour(1))
	observe("FULL", "FULL-12", claim+"reviewer", `"working"`, hour(1))
	observe("FULL", "FULL-12", legion.FieldPhase, `null`, hour(2))
	observe("FULL", "FULL-12", claim+"reviewer", `null`, hour(2))
	observe("FULL", "FULL-15", legion.FieldPhase, `"implementing"`, hour(1))
	observe("FULL", "FULL-15", claim+"architect", `"retired"`, hour(1))
	observe("FULL", "FULL-15", claim+"implementer", `"working"`, hour(1))
	observe("FULL", "FULL-15", claim+"auditor", `"idle"`, hour(1))
	for _, key := range []string{"FULL-16", "FULL-17", "FULL-18"} {
		observe("FULL", key, legion.FieldPhase, `"admitted"`, hour(1))
	}
	// The blockers in an order that is not their sorted one, so a read that sorted them is told
	// from one that keeps the daemon's order.
	observe("FULL", legion.AdmissionKey, legion.FieldAdmission, `{
		"cap": 4,
		"active": ["FULL-1", "FULL-3", "FULL-15"],
		"waiting": ["FULL-3", "FULL-16", "FULL-2", "FULL-17", "FULL-18"],
		"blocked": [{"issue": "FULL-4", "blockedBy": ["FULL-9", "FULL-7"]}]
	}`, hour(1))
	seedLegionRead(t, database, "FULL", hour(0), "the Legion daemon could not be read: connect: connection refused")
	seedLegionRead(t, database, "FULL", hour(10), "")
	fullRead := &wireLegionRead{At: hour(10), OK: true}

	// FAILED: a good read recorded its state, and the newest read failed.
	seedLegionIssue(t, database, "FAILED", 1, "The issue under review", "a", nil, "")
	observe("FAILED", "FAILED-1", legion.FieldPhase, `"reviewing"`, hour(1))
	observe("FAILED", "FAILED-1", legion.FieldSlot, `0`, hour(1))
	observe("FAILED", "FAILED-1", claim+"reviewer", `"working"`, hour(1))
	observe("FAILED", legion.AdmissionKey, legion.FieldAdmission, `{"cap": 2, "active": ["FAILED-1"], "waiting": [], "blocked": []}`, hour(1))
	seedLegionRead(t, database, "FAILED", hour(1), "")
	failedText := "the Legion daemon could not be read: context deadline exceeded"
	seedLegionRead(t, database, "FAILED", hour(5), failedText)

	// NOLINE: a daemon that recorded no admission row.
	seedLegionIssue(t, database, "NOLINE", 1, "The issue in testing", "a", nil, "")
	seedLegionIssue(t, database, "NOLINE", 2, "The root waiting for a slot", "b", nil, "")
	observe("NOLINE", "NOLINE-1", legion.FieldPhase, `"testing"`, hour(1))
	observe("NOLINE", "NOLINE-1", legion.FieldSlot, `0`, hour(1))
	observe("NOLINE", "NOLINE-1", claim+"tester", `"working"`, hour(1))
	observe("NOLINE", "NOLINE-2", legion.FieldPhase, `"admitted"`, hour(1))
	seedLegionRead(t, database, "NOLINE", hour(2), "")

	// UNREAD: a daemon that never answered; NEVERREAD: a daemon never read.
	seedLegionIssue(t, database, "UNREAD", 1, "An issue of a daemon that never answered", "a", nil, "")
	seedLegionRead(t, database, "UNREAD", hour(1), "the Legion daemon could not be read: connect: connection refused")
	unreadText := "the Legion daemon could not be read: connect: no route to host"
	seedLegionRead(t, database, "UNREAD", hour(2), unreadText)
	seedLegionIssue(t, database, "NEVERREAD", 1, "An issue of a daemon never read", "a", nil, "")

	// NODAEMON: a daemon recorded a phase and a claim of each issue, an admission row and a good
	// read, and was then removed, which leaves every row it wrote (legion.DeleteSettings). Its
	// first issue has a run, written after the removal; the second has none.
	seedLegionIssue(t, database, "NODAEMON", 1, "An issue with a run of a project with no daemon", "a", nil, "")
	seedLegionIssue(t, database, "NODAEMON", 2, "An issue with no run of a project with no daemon", "b", nil, "")
	for _, key := range []string{"NODAEMON-1", "NODAEMON-2"} {
		observe("NODAEMON", key, legion.FieldPhase, `"implementing"`, hour(1))
		observe("NODAEMON", key, claim+"implementer", `"working"`, hour(1))
	}
	observe("NODAEMON", legion.AdmissionKey, legion.FieldAdmission,
		`{"cap": 2, "active": ["NODAEMON-1"], "waiting": ["NODAEMON-2"], "blocked": []}`, hour(1))
	seedLegionRead(t, database, "NODAEMON", hour(1), "")
	if removed := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/projects/NODAEMON/legion/daemon", nil, "alice"); removed.Code != http.StatusNoContent {
		t.Fatalf("remove NODAEMON's daemon: status=%d body=%s, want 204", removed.Code, removed.Body.String())
	}
	putStepOK(t, handler, "NODAEMON-1", uuid.NewString(), implementerStart(daemonSessionOf("NODAEMON")))

	t.Run("R-1 a slot holder with its architect and implementer in a turn and its tester idle", func(t *testing.T) {
		answer := legionIssueOf(t, handler, "FULL-1")
		if answer.RunsRevision != 0 {
			t.Errorf("FULL-1's runs revision is %d, want 0: it has no run", answer.RunsRevision)
		}
		requireLegionState(t, "FULL-1", answer, legionIssueWant{
			phase: "implementing", since: hour(1),
			roles: []wireLegionRole{{"architect", "working"}, {"implementer", "working"}, {"tester", "idle"}},
			slot:  new(0), blockedBy: []string{},
		})
		requireLegionRead(t, "FULL-1", answer, fullRead)
	})
	t.Run("R-2 the third of five in admission.waiting", func(t *testing.T) {
		requireLegionState(t, "FULL-2", legionIssueOf(t, handler, "FULL-2"), legionIssueWant{
			phase: "admitted", since: hour(1), roles: []wireLegionRole{},
			line: &wireLegionWaitingLine{Place: 3, Length: 5}, blockedBy: []string{},
		})
	})
	t.Run("R-3 a root that holds a slot and stands first in the line", func(t *testing.T) {
		requireLegionState(t, "FULL-3", legionIssueOf(t, handler, "FULL-3"), legionIssueWant{
			phase: "planning", since: hour(1), roles: []wireLegionRole{{"planner", "working"}},
			slot: new(1), line: &wireLegionWaitingLine{Place: 1, Length: 5}, blockedBy: []string{},
		})
	})
	t.Run("R-4 an issue admission.blocked names with two blockers, in the stored order", func(t *testing.T) {
		requireLegionState(t, "FULL-4", legionIssueOf(t, handler, "FULL-4"), legionIssueWant{
			phase: "admitted", since: hour(1), roles: []wireLegionRole{}, blockedBy: []string{"FULL-9", "FULL-7"},
		})
	})
	t.Run("R-5 a root waiting on an ask", func(t *testing.T) {
		requireLegionState(t, "FULL-5", legionIssueOf(t, handler, "FULL-5"), legionIssueWant{
			phase: "awaiting_person", since: hour(1), roles: []wireLegionRole{}, blockedBy: []string{},
			waitingOn: map[string]any{
				"kind": "ask", "ask": "6f1c0f8e-3d4b-4a8e-9f51-2b7c1d9e0a11", "since": "2026-11-20T08:30:00Z",
			},
		})
	})
	t.Run("R-6 a root waiting on a design gate a person answered", func(t *testing.T) {
		requireLegionState(t, "FULL-6", legionIssueOf(t, handler, "FULL-6"), legionIssueWant{
			phase: "awaiting_person", since: hour(1), roles: []wireLegionRole{}, blockedBy: []string{},
			waitingOn: map[string]any{
				"kind": "gate", "gateVersion": float64(7), "artifactId": "0c9d2e4f-5a6b-4c7d-8e9f-a0b1c2d3e4f5",
				"since": "2026-11-20T07:00:00Z", "answeredAt": "2026-11-20T09:45:00Z", "answeredBy": "approved",
			},
		})
	})
	t.Run("R-7 an issue in accepting", func(t *testing.T) {
		requireLegionState(t, "FULL-7", legionIssueOf(t, handler, "FULL-7"), legionIssueWant{
			phase: "accepting", since: hour(1), roles: []wireLegionRole{}, blockedBy: []string{},
		})
	})
	t.Run("R-8 an unstarted child in admitted under a carded root, holding no slot", func(t *testing.T) {
		requireLegionState(t, "FULL-8", legionIssueOf(t, handler, "FULL-8"), legionIssueWant{
			phase: "admitted", since: hour(1), roles: []wireLegionRole{}, blockedBy: []string{},
		})
	})
	t.Run("R-9 a phase that is the first phase row of its issue was not seen to start", func(t *testing.T) {
		requireLegionState(t, "FULL-9", legionIssueOf(t, handler, "FULL-9"), legionIssueWant{
			phase: "testing", since: hour(2), roles: []wireLegionRole{}, blockedBy: []string{},
		})
	})
	t.Run("R-10 a phase whose move was stored was seen to start, at the second row", func(t *testing.T) {
		requireLegionState(t, "FULL-10", legionIssueOf(t, handler, "FULL-10"), legionIssueWant{
			phase: "implementing", since: hour(3), seen: true, roles: []wireLegionRole{}, blockedBy: []string{},
		})
	})
	t.Run("R-11 a phase that came back after a null was seen to start, when it came back", func(t *testing.T) {
		requireLegionState(t, "FULL-11", legionIssueOf(t, handler, "FULL-11"), legionIssueWant{
			phase: "planning", since: hour(3), seen: true, roles: []wireLegionRole{}, blockedBy: []string{},
		})
	})
	t.Run("R-12 an issue whose newest phase row is null is not recorded", func(t *testing.T) {
		answer := legionIssueOf(t, handler, "FULL-12")
		requireLegionConfigured(t, "FULL-12", answer, true)
		if answer.State != nil {
			t.Errorf("FULL-12's state = %+v, want null: its newest phase row is null", *answer.State)
		}
		requireLegionRead(t, "FULL-12", answer, fullRead)
	})
	t.Run("R-13 a Dispatch issue with no observation is not recorded, with its project's read", func(t *testing.T) {
		answer := legionIssueOf(t, handler, "FULL-13")
		requireLegionConfigured(t, "FULL-13", answer, true)
		if answer.State != nil {
			t.Errorf("FULL-13's state = %+v, want null", *answer.State)
		}
		requireLegionRead(t, "FULL-13", answer, fullRead)
	})
	t.Run("R-14 a key Dispatch does not hold is refused as its issue's read refuses it", func(t *testing.T) {
		requireLegionIssueNotFound(t, handler, "FULL-999")
	})
	t.Run("R-15 a retired architect and a role Dispatch does not know are listed, the known roles first", func(t *testing.T) {
		requireLegionState(t, "FULL-15", legionIssueOf(t, handler, "FULL-15"), legionIssueWant{
			phase: "implementing", since: hour(1),
			roles:     []wireLegionRole{{"architect", "retired"}, {"implementer", "working"}, {"auditor", "idle"}},
			blockedBy: []string{},
		})
	})
	t.Run("P-2 a failed newest read is the read, over the state the good read before it stored", func(t *testing.T) {
		answer := legionIssueOf(t, handler, "FAILED-1")
		requireLegionState(t, "FAILED-1", answer, legionIssueWant{
			phase: "reviewing", since: hour(1), roles: []wireLegionRole{{"reviewer", "working"}},
			slot: new(0), blockedBy: []string{},
		})
		requireLegionRead(t, "FAILED-1", answer, &wireLegionRead{At: hour(5), OK: false, Error: &failedText})
	})
	t.Run("P-3 a project with no admission row has no place in a line and no blocker", func(t *testing.T) {
		requireLegionState(t, "NOLINE-1", legionIssueOf(t, handler, "NOLINE-1"), legionIssueWant{
			phase: "testing", since: hour(1), roles: []wireLegionRole{{"tester", "working"}},
			slot: new(0), blockedBy: []string{},
		})
		requireLegionState(t, "NOLINE-2", legionIssueOf(t, handler, "NOLINE-2"), legionIssueWant{
			phase: "admitted", since: hour(1), roles: []wireLegionRole{}, blockedBy: []string{},
		})
	})
	t.Run("P-4 a project whose daemon never answered is not recorded, with the failed read", func(t *testing.T) {
		answer := legionIssueOf(t, handler, "UNREAD-1")
		requireLegionConfigured(t, "UNREAD-1", answer, true)
		if answer.State != nil {
			t.Errorf("UNREAD-1's state = %+v, want null", *answer.State)
		}
		requireLegionRead(t, "UNREAD-1", answer, &wireLegionRead{At: hour(2), OK: false, Error: &unreadText})
	})
	t.Run("P-4b a project whose daemon was never read has a null read", func(t *testing.T) {
		answer := legionIssueOf(t, handler, "NEVERREAD-1")
		requireLegionConfigured(t, "NEVERREAD-1", answer, true)
		if answer.State != nil {
			t.Errorf("NEVERREAD-1's state = %+v, want null", *answer.State)
		}
		requireLegionRead(t, "NEVERREAD-1", answer, nil)
	})
	t.Run("P-5 an issue of a project that names no daemon is not configured, with its run and none of the old rows", func(t *testing.T) {
		answer := legionIssueOf(t, handler, "NODAEMON-1")
		requireLegionConfigured(t, "NODAEMON-1", answer, false)
		if answer.State != nil {
			t.Errorf("NODAEMON-1's state = %+v, want null: the phase and claim an earlier daemon left are no daemon's now", *answer.State)
		}
		requireLegionRead(t, "NODAEMON-1", answer, nil)
		if answer.RunsRevision <= 0 {
			t.Errorf("NODAEMON-1's runs revision is %d, want the tuples its run's write appended, more than 0", answer.RunsRevision)
		}
	})
	t.Run("P-5 an issue with no run of a project that names no daemon is not configured, with runs revision 0", func(t *testing.T) {
		answer := legionIssueOf(t, handler, "NODAEMON-2")
		requireLegionConfigured(t, "NODAEMON-2", answer, false)
		if answer.State != nil || answer.RunsRevision != 0 {
			t.Errorf("NODAEMON-2's answer = %+v, want a null state and runs revision 0", answer)
		}
		requireLegionRead(t, "NODAEMON-2", answer, nil)
	})
	t.Run("P-5 a key Dispatch does not hold of a project that names no daemon is refused as its issue's read refuses it", func(t *testing.T) {
		requireLegionIssueNotFound(t, handler, "NODAEMON-999")
	})
	t.Run("B-1 each card of FULL, FAILED and NOLINE agrees with its issue's answer", func(t *testing.T) {
		for _, project := range []string{"FULL", "FAILED", "NOLINE"} {
			board := legionBoardOf(t, handler, project)
			compared := 0
			for _, column := range board.Columns {
				for _, card := range column.Cards {
					compared++
					for _, disagreement := range legionCardDisagreements(card, legionIssueOf(t, handler, card.Key)) {
						t.Errorf("%s's board: %s", project, disagreement)
					}
				}
			}
			if compared == 0 {
				t.Errorf("%s's board carries no card, so nothing was compared", project)
			}
		}
	})
}

// TestAnIssuesLegionReadCountsTheRunsRevision: the runs' revision, the count of the tuples of the
// issue's runs (the plan's decision 4), is 0 for an issue with no run, rises with a step write
// that makes a run, stays where it is when the same write comes again and sets nothing, and rises
// with a rating write that alone makes a retro's record.
func TestAnIssuesLegionReadCountsTheRunsRevision(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "RUNS")
	nameLegionDaemon(t, handler, "RUNS", "http://legion-runs.internal:13370")
	key := seedLegionIssue(t, database, "RUNS", 1, "An issue the daemon runs", "a", nil, "")
	seedLegionRead(t, database, "RUNS", time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), "")

	none := legionIssueOf(t, handler, key).RunsRevision
	if none != 0 {
		t.Fatalf("%s's runs revision is %d before any run, want 0", key, none)
	}
	run := uuid.NewString()
	putStepOK(t, handler, key, run, implementerStart(daemonSessionOf("RUNS")))
	started := legionIssueOf(t, handler, key).RunsRevision
	if started <= none {
		t.Fatalf("%s's runs revision is %d after a step write made a run, want above %d", key, started, none)
	}
	putStepOK(t, handler, key, run, implementerStart(daemonSessionOf("RUNS")))
	if again := legionIssueOf(t, handler, key).RunsRevision; again != started {
		t.Fatalf("%s's runs revision is %d after the same write again, which sets nothing, want %d", key, again, started)
	}
	putRatingOK(t, handler, key, uuid.NewString(), withFields(retroIdentity(daemonSessionOf("RUNS"), 1),
		map[string]any{"rating": 4, "rating_reason": "The plan fit the work."}))
	if rated := legionIssueOf(t, handler, key).RunsRevision; rated <= started {
		t.Fatalf("%s's runs revision is %d after a rating write made a retro's record, want above %d", key, rated, started)
	}
}

// issueMember is a view's `issue` member naming key, as a page's frame spells it: a JSON string.
func issueMember(key string) json.RawMessage {
	encoded, _ := json.Marshal(key) // a Go string always encodes
	return encoded
}

// issueMemberRow is an `issue` member a frame could carry, and what a failure names it by.
type issueMemberRow struct {
	what   string
	member json.RawMessage
}

// notStringIssues is an `issue` member of each JSON type but a string.
var notStringIssues = []issueMemberRow{
	{"a number", json.RawMessage(`7`)},
	{"an object", json.RawMessage(`{"key":"INFRA-7"}`)},
	{"an array", json.RawMessage(`["INFRA-7"]`)},
	{"a bool", json.RawMessage(`true`)},
	{"null", json.RawMessage(`null`)},
}

// TestALegionIssueViewNamesItsIssueAndNothingElse: the live kind of one issue's state names its
// issue and nothing else (the plan's decision 7). Each shape it refuses is refused INVALID_FRAME
// before anything is subscribed, and a view of INFRA-7 is held under its issue, the project its
// key names and a key of its own. The socket's decode accepts an `issue` member of any JSON type
// (DeliveryLiveView holds it raw), and only legion_issue's parse reads it: a legion_issue view
// whose issue is no string is refused as one with no issue is, and every kind before it ignores
// the member whatever its type, as it did before any kind read one, so a frame a deployed page
// sends with one is read as the same view without it, never refused.
func TestALegionIssueViewNamesItsIssueAndNothingElse(t *testing.T) {
	t.Parallel()
	window := DeliveryLiveWindow{Preset: new(7)}
	issue, project, query, at := "INFRA-7", "INFRA", "", "2026-11-23T00:00:00Z"
	named := issueMember(issue)
	type refusedView struct {
		what string
		view DeliveryLiveView
	}
	refusedViews := []refusedView{
		{"a legion_issue view with no issue", DeliveryLiveView{Kind: issueLegionKind}},
		{"a legion_issue view with an empty issue", DeliveryLiveView{Kind: issueLegionKind, Issue: issueMember("")}},
		{"a legion_issue view with a project", DeliveryLiveView{Kind: issueLegionKind, Issue: named, Project: &project}},
		{"a legion_issue view with a window", DeliveryLiveView{Kind: issueLegionKind, Issue: named, Window: &window}},
		{"a legion_issue view with an at", DeliveryLiveView{Kind: issueLegionKind, Issue: named, At: &at}},
		{"a legion_issue view with a query", DeliveryLiveView{Kind: issueLegionKind, Issue: named, Query: &query}},
	}
	for _, member := range notStringIssues {
		refusedViews = append(refusedViews, refusedView{
			"a legion_issue view whose issue is " + member.what, DeliveryLiveView{Kind: issueLegionKind, Issue: member.member},
		})
	}
	for _, refused := range refusedViews {
		t.Run(refused.what, func(t *testing.T) {
			if _, refusal := parseDeliveryLiveView(refused.view); refusal == nil || refusal.Code != "INVALID_FRAME" {
				t.Errorf("%s: refusal = %+v, want INVALID_FRAME", refused.what, refusal)
			}
		})
	}
	t.Run("a view of no kind Dispatch serves is told the five kinds", func(t *testing.T) {
		_, refusal := parseDeliveryLiveView(DeliveryLiveView{Kind: "forecast"})
		if refusal == nil || refusal.Code != "INVALID_FRAME" {
			t.Fatalf("a forecast view: refusal = %+v, want INVALID_FRAME", refusal)
		}
		for _, kind := range []string{deliveryLiveTimeline, deliveryLiveMeasures, deliveryLivePipeline, legionBoardView, issueLegionKind} {
			if !strings.Contains(refusal.Message, kind) {
				t.Errorf("the refusal of a forecast view says %q, which does not name the kind %s", refusal.Message, kind)
			}
		}
	})
	t.Run("a legion_issue view of INFRA-7 is its issue, its key's project and a key of its own", func(t *testing.T) {
		spec, refusal := parseDeliveryLiveView(DeliveryLiveView{Kind: issueLegionKind, Issue: named})
		if refusal != nil {
			t.Fatalf("a legion_issue view of INFRA-7: %+v", refusal)
		}
		if spec.kind != issueLegionKind || spec.issue != "INFRA-7" || spec.project != "INFRA" || spec.key != "legion_issue|INFRA-7|" {
			t.Fatalf("the spec = %+v, want kind legion_issue, issue INFRA-7, project INFRA and key legion_issue|INFRA-7|", spec)
		}
	})
	t.Run("a timeline, measures, pipeline or legion view with an issue of any type is that view without one", func(t *testing.T) {
		facets := "search=widgets&status=open"
		members := append([]issueMemberRow{{"a string", named}}, notStringIssues...)
		for _, view := range []DeliveryLiveView{
			{Kind: deliveryLiveTimeline, Window: &window, Query: &facets},
			{Kind: deliveryLiveMeasures, Window: &window, Query: &facets},
			{Kind: deliveryLivePipeline, At: &at},
			{Kind: legionBoardView, Project: &project},
		} {
			want, refusal := parseDeliveryLiveView(view)
			if refusal != nil {
				t.Fatalf("a %s view with no issue: refusal = %+v, want none", view.Kind, refusal)
			}
			for _, member := range members {
				carrying := view
				carrying.Issue = member.member
				got, refusal := parseDeliveryLiveView(carrying)
				if refusal != nil {
					t.Errorf("a %s view whose issue is %s: refusal = %+v, want it read as the same view without one", view.Kind, member.what, refusal)
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("a %s view whose issue is %s is %+v, want %+v, the same view without one", view.Kind, member.what, got, want)
				}
			}
		}
	})
}

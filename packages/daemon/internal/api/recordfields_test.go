package api

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// operatorServedUnder names, for each claim field the operator route serves under a key that is
// not the one ClaimRecordFields gives it, the keys that route does serve it under. The table's
// keys are the agent surfaces' (recordfields.go), and two fields reach the operator whole where
// an agent surface reduces them: `Budgets` stands under `budgets` on every claim and inside
// `failure.attempts` on a failed one, where a claim view carries the four counts inside the
// failure record alone, and `Pending` stands whole under `pending`, its text included, which a
// claim view reduces to `pendingTask` and never carries the text of.
var operatorServedUnder = map[string][]string{
	"Budgets": {"budgets", "failure.attempts"},
	"Pending": {"pending"},
}

// operatorWithheld is every field of the agents column the operator route does not carry, which
// is what the OperatorClaim doc states and what this test holds it to: the route is the live
// machine's and the agent surfaces are the store's, and these five reach an agent and no
// operator. `GivenEntry` is among them because the surface that serves it is the issue's own
// view (api.Issue.GivenEntry) and no claim's.
var operatorWithheld = []string{"ServingGeneration", "PendingCause", "ProviderErrorRun", "UpdateReasons", "GivenEntry"}

// wireOf is value as this wire carries it: marshalled and read back, so a view and the keys the
// decision table names are compared in one shape rather than in two Go types.
func wireOf(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("read %T back: %v", value, err)
	}
	return document
}

// canonicalOf is document as one string, which is what a failure prints so a reader sees the
// keys the route wrote rather than the one key the assertion names.
func canonicalOf(t *testing.T, document map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal the document: %v", err)
	}
	return string(encoded)
}

// valueAt is whether document holds a value at the dotted path, the dots being how the table
// names a key served inside another (`failure.attempts`).
func valueAt(document map[string]any, path string) bool {
	current := any(document)
	for _, step := range strings.Split(path, ".") {
		object, isObject := current.(map[string]any)
		if !isObject {
			return false
		}
		held, carried := object[step]
		if !carried {
			return false
		}
		current = held
	}
	return true
}

// claimWithEveryFieldSet is one claim with every field of the record set, which no live claim
// is: the point is the mapping of each field to the key that carries it, and a zero field would
// leave an `omitempty` key off the wire and prove nothing of it. It is internal/projection's
// twin, built there for the agent surfaces and here for the operator route. The loop at the end
// keeps this copy whole: a field added to the record and left at its zero value here fails by
// its own name. The projection twin has no such loop, so a field added there is set by hand.
func claimWithEveryFieldSet(t *testing.T) supervise.Claim {
	t.Helper()
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	failedAt, queuedAt, openedAt := at.Add(-10*time.Minute), at.Add(-30*time.Minute), at.Add(-20*time.Minute)
	answeredAt := at.Add(-time.Minute)
	token := claim.Token("legion-legion-legion-500-implementer")
	podUID := "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11"
	current := supervise.Claim{
		Token: token, Project: "OTHER", Tree: "LEGION-500", Issue: "LEGION-500", Role: claim.RoleImplementer,
		Generation: 3, TreeEpoch: 2,
		Session: "ses_implementer_500", SessionFile: "/var/lib/legion/sessions/ses_implementer_500.jsonl",
		ServingGeneration: 2, GivenEntry: 7, ServedEntry: 6,
		ServingCheckLook: supervise.CheckLook{Look: 2, OpenedAt: openedAt},
		LastStartRow:     811, DeliveredRows: []int64{809, 811},
		Locator: &runtime.Locator{
			Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(podUID, 5),
			Sandbox: &runtime.SandboxLocator{
				Namespace: "legion", Name: "legion-legion-legion-500", PodUID: podUID,
				Container: runtime.IssuePodContainer, Generation: 5,
			},
		},
		Enrollment: &supervise.Enrollment{ID: "enr_42", Incarnation: podUID + "/5"},
		State:      supervise.StateFailed,
		Budgets:    supervise.Budgets{LaunchFailures: 4, Deaths: 1, PromptFailures: 2, PromptRetires: 3},
		Failure:    "launch failures ran out", FailureCause: supervise.CauseDied, FailedAt: failedAt,
		PendingCause:   supervise.CauseDied,
		LeakCheckCause: "the shared public leak check could not be installed: the tarball of theorem-labs/agent-infra at leak-check-v3 holds no leak-check directory",
		Pending: &supervise.Delivery{
			ID: "dlv-9", Task: "implement the change, which no agent surface carries",
			Generation: 3, Entry: 8, Phase: phase.Implementing, QueuedAt: queuedAt,
			SendFailures: 2, LastSendFailureAt: at.Add(-5 * time.Minute),
			LastSendError: "post to the worker stream: connection refused",
		},
		BootTokenHash: []byte{0xb0, 0x07}, CapabilityHash: []byte{0xca, 0xfe},
		UncertainStreak: 1,
		SuspendedBy:     supervise.SuspendedByOperator, SuspensionHeld: true,
		SuspensionHeldBy: supervise.SuspendedByDaemon, Compacting: true, ProcessGone: true, DeathCharged: true,
		PluginContract: DaemonAPIVersion, PluginRelaunchFor: DaemonAPIVersion - 1,
		UpdateRound: "pod:" + podUID, UpdateReasons: "the pod template changed: the image moved",
		UpdateOpenedAt: &openedAt, UpdateTaskedAt: &openedAt, UpdateSignalledAt: &answeredAt,
		UpdateHead:       "4c1b8e2a6f9d3047b5e1c8a2f6d940b3e7c5a128",
		UpdateToldAt:     &answeredAt,
		UpdateLeftRound:  "pod:older",
		ProviderErrorRun: 6,
		LastTurnStart:    at.Add(-2 * time.Minute),
		LastTurnEnd:      at.Add(-30 * time.Second),
		LastReady:        at.Add(-3 * time.Minute),
		LastDeathCause:   "the pod was evicted",
		LastStartFailure: "the launcher refused the start",
		TurnOutcomes: []supervise.TurnOutcome{{
			Generation: 3, Sequence: 2, Refused: true, Message: "the provider refused this turn",
		}},
		Abandoned: supervise.AbandonedSession{Session: "ses_implementer_499", Why: "the provider refused every turn of it", At: at.Add(-time.Hour)},
		EndedWork: supervise.EndedWork{At: at.Add(-40 * time.Minute), Cause: "the process died: gone: OOMKilled",
			Generation: 2, Turn: true, TaskTurn: true, Background: true, Kinds: []string{"subagent"}, Reported: true},
		SessionLost: true,
		Step: supervise.StepRecord{
			At: at.Add(-time.Minute), TurnStartedAt: at.Add(-2 * time.Minute),
			Tool: "bash", CallID: "toolu_impl_11", ArgsBytes: 128, ArgsHash: "9b2e45c0d1f8a673",
			StartedAt: at.Add(-time.Minute), EndedAt: at.Add(-30 * time.Second),
		},
		ActivityAt: at.Add(-3 * time.Minute), ActivityBackground: true,
		ActivityKinds: []string{"async-job"}, ActivityNextReminderAt: at.Add(10 * time.Minute),
	}
	fields := reflect.ValueOf(current)
	for i := range fields.NumField() {
		if fields.Field(i).IsZero() {
			t.Fatalf("supervise.Claim.%s is left at its zero value here: set it, or the key that carries it is read off a document that never had it (claimWithEveryFieldSet)",
				fields.Type().Field(i).Name)
		}
	}
	return current
}

// The operator half of the guard that makes the class impossible, which internal/projection's
// test of this name makes over the agent surfaces: every field of `supervise.Claim` the decision
// table files in the operator column is served by `operatorView`, the one view every operator
// route answers a claim with, and every key that view carries is one the table names.
//
// Without it the operator column is checked against no surface: a field filed there and served
// nowhere would stand green in the table, and a key taken out of operatorView would fail
// nothing. The claim is built with every field set (claimWithEveryFieldSet), so a key the table
// names and the view does not carry is a field served under a wrong name rather than a field
// that happened to be empty.
//
// What it does not read: an operator-column entry carrying no key of its own, which the table
// allows for a field the route serves inside another key — the delivery's own text stands inside
// `pending` rather than under a name of its own. There is no name to hold the view to for such
// an entry, so this test says nothing of it and the keys the view does carry are held to the
// table all the same. Nor does it read the values: the keys are the decision this table records,
// and what each carries is read by the routes' own tests (TestTheFailureRecordOverTheRoutes).
func TestEveryRecordFieldIsServedOrExcluded(t *testing.T) {
	current := claimWithEveryFieldSet(t)
	document := wireOf(t, operatorView(current, current.Step))

	decided := make(map[string]RecordFieldDecision, len(ClaimRecordFields))
	for _, entry := range ClaimRecordFields {
		decided[entry.Field] = entry
	}
	// The two lists this test keeps beside the table are held to it first: a field renamed in
	// the record and here alone would otherwise withhold nothing and name nothing.
	for field := range operatorServedUnder {
		if entry, stands := decided[field]; !stands || entry.Column != ColumnAgents {
			t.Errorf("operatorServedUnder names supervise.Claim.%s, which stands in %q rather than the agents column: the route serves a key of its own only where the table's key is an agent surface's",
				field, entry.Column)
		}
	}
	for _, field := range operatorWithheld {
		if entry, stands := decided[field]; !stands || entry.Column != ColumnAgents {
			t.Errorf("operatorWithheld names supervise.Claim.%s, which stands in %q rather than the agents column: only a field an agent surface serves is withheld from the operator",
				field, entry.Column)
		}
	}

	// Served: every key the table says the operator route carries stands in the view.
	named := []string{}
	for _, entry := range ClaimRecordFields {
		keys := entry.Keys
		switch entry.Column {
		case ColumnOperator:
			// An entry with no key of its own is served inside another field's key, which this
			// test cannot hold to a name; the loop below still holds every key the view carries
			// to the table.
			if len(keys) == 0 {
				continue
			}
		case ColumnAgents:
			if slices.Contains(operatorWithheld, entry.Field) {
				for _, key := range keys {
					if valueAt(document, key) {
						t.Errorf("the operator view carries %q, supervise.Claim.%s, which these routes withhold (OperatorClaim): %s",
							key, entry.Field, canonicalOf(t, document))
					}
				}
				continue
			}
			if own, served := operatorServedUnder[entry.Field]; served {
				keys = own
			}
		default:
			continue
		}
		for _, key := range keys {
			if !valueAt(document, key) {
				t.Errorf("supervise.Claim.%s is the operator route's under %q, which operatorView does not carry: serve it, or decide the field into another column (internal/api's recordfields.go). The view is %s",
					entry.Field, key, canonicalOf(t, document))
			}
			named = append(named, strings.Split(key, ".")[0])
		}
	}

	// And the other way: every key the view carries is one the table names. A key under no entry
	// is a field served by a decision nobody made.
	for key := range document {
		if slices.Contains(named, key) {
			continue
		}
		t.Errorf("the operator view carries %q, which no entry of supervise.Claim's table names: serve it by a decision or take it off the wire (internal/api's recordfields.go). The view is %s",
			key, canonicalOf(t, document))
	}
}

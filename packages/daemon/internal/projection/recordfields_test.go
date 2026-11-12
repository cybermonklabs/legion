package projection

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// decisionsOf is table indexed by field name, with the table held to record: every field of the
// record stands in exactly one column with a reason, and every entry names a field the record
// has. This is the guard that makes the class impossible — a field added to the record and
// neither served nor excluded fails here by its own name — so it runs before anything reads a
// view.
func decisionsOf(t *testing.T, name string, table []api.RecordFieldDecision, of reflect.Type) map[string]api.RecordFieldDecision {
	t.Helper()
	decided := make(map[string]api.RecordFieldDecision, len(table))
	for _, entry := range table {
		if _, twice := decided[entry.Field]; twice {
			t.Errorf("%s's %s stands in the table twice: a field is one decision, not two", name, entry.Field)
		}
		if _, held := of.FieldByName(entry.Field); !held {
			t.Errorf("the table decides %s.%s, which the record has no field of: the entry names a field that was renamed or removed", name, entry.Field)
		}
		switch entry.Column {
		case api.ColumnAgents:
			if len(entry.Keys) == 0 {
				t.Errorf("%s.%s is served to the agent surfaces under no key", name, entry.Field)
			}
		case api.ColumnOperator:
		case api.ColumnWithheld:
			if len(entry.Keys) > 0 {
				t.Errorf("%s.%s is withheld and names the keys %v: a withheld field reaches no surface", name, entry.Field, entry.Keys)
			}
		default:
			t.Errorf("%s.%s stands in %q, which is none of the three columns", name, entry.Field, entry.Column)
		}
		if strings.TrimSpace(entry.Reason) == "" {
			t.Errorf("%s.%s carries no reason: the reason is the part a reviewer reads", name, entry.Field)
		}
		decided[entry.Field] = entry
	}
	for i := range of.NumField() {
		field := of.Field(i).Name
		if _, stands := decided[field]; !stands {
			t.Errorf("%s.%s stands in no column of the decision table: decide it — served to the agent surfaces, the operator route's alone, or withheld with its reason (internal/api's recordfields.go)", name, field)
		}
	}
	return decided
}

// wire is value as this wire carries it: marshalled and read back, so a view and the map built
// from the record it was projected from are compared in one shape rather than in two Go types.
func wire(t *testing.T, value any) map[string]any {
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

// canonical is document as one string of sorted keys, which is what two documents are compared
// by: every object on this wire is read back into a map, so the order a struct declares its
// fields in decides nothing here.
func canonical(t *testing.T, document map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal the document: %v", err)
	}
	return string(encoded)
}

// at is the value document holds at the dotted path, and whether it holds one.
func at(document map[string]any, path string) (any, bool) {
	current := any(document)
	for _, step := range strings.Split(path, ".") {
		object, isObject := current.(map[string]any)
		if !isObject {
			return nil, false
		}
		held, carried := object[step]
		if !carried {
			return nil, false
		}
		current = held
	}
	return current, true
}

// servedKeys holds a projected document to the table: every key the table says is served stands
// in it, and every key it carries is one the table names or one built from another record
// (beside, named by the caller). The document is of a record with every field set, so a key the
// table names and the document does not carry is a field served under a wrong name.
func servedKeys(t *testing.T, name string, document map[string]any, table []api.RecordFieldDecision, beside []string) {
	t.Helper()
	var served []string
	for _, entry := range table {
		if entry.Column != api.ColumnAgents {
			continue
		}
		for _, key := range entry.Keys {
			if _, carried := at(document, key); !carried {
				t.Errorf("%s.%s is served under %q, which the projected document does not carry: %s",
					name, entry.Field, key, canonical(t, document))
			}
			served = append(served, strings.Split(key, ".")[0])
		}
	}
	for key := range document {
		if slices.Contains(served, key) || slices.Contains(beside, key) {
			continue
		}
		t.Errorf("the document carries %q, which no entry of %s's table names: serve it by a decision or take it off the wire", key, name)
	}
}

// The guard that makes the class impossible. Every field of `supervise.Claim`, of
// `record.Issue` and of `record.Assignment` stands in exactly one of the three columns of the
// decision table (internal/api's recordfields.go), each with its reason as data, so a field
// added later is decided rather than forgotten and a field moved between the columns is a
// decision a reviewer sees in the diff.
//
// It then reads what the surfaces serve for one built claim, one built issue and the one
// experiment answer that issue runs under, field by field, against the record each was built
// from, so a field served under a wrong name or a wrong value fails here rather than at a
// reader. Each record is built with every field set, which no live record is: the point is the
// mapping of each field to the key that carries it, and a zero field would leave an
// `omitempty` key off the wire and prove nothing of it.
//
// What it does not show: that a served field is useful to a role. That is the reviewer's reading.
func TestEveryRecordFieldIsServedOrExcluded(t *testing.T) {
	claimTable := decisionsOf(t, "supervise.Claim", api.ClaimRecordFields, reflect.TypeFor[supervise.Claim]())
	issueTable := decisionsOf(t, "record.Issue", api.IssueRecordFields, reflect.TypeFor[record.Issue]())
	// The assignment table is held to the record alone: every field of it is served to the agent
	// surfaces or withheld, and the subtest below reads the keys the view serves against it.
	decisionsOf(t, "record.Assignment", api.AssignmentRecordFields, reflect.TypeFor[record.Assignment]())

	// No field of the issue record is a secret and none is the operator route's alone, so R4
	// decides each as served to the agent surfaces or withheld with its reason.
	for field, entry := range issueTable {
		if entry.Column == api.ColumnOperator {
			t.Errorf("record.Issue.%s stands in %q: an issue's field is served to the agent surfaces or withheld, and a move out of the two is a decision of its own",
				field, entry.Column)
		}
	}

	// The two fields R3 takes out of its default, read by name: a build that files either under
	// the default phrase fails here.
	if entry := claimTable["UpdateReasons"]; entry.Column != api.ColumnAgents {
		t.Errorf("supervise.Claim.UpdateReasons stands in %q, want %q: the worker answering an update round is told to act on it",
			entry.Column, api.ColumnAgents)
	}
	if entry := claimTable["DeathCharged"]; entry.Column != api.ColumnWithheld || entry.Reason == api.BookkeepingReason {
		t.Errorf("supervise.Claim.DeathCharged stands in %q for %q, want %q with a reason of its own: it is the supervisor's accounting of one dead process",
			entry.Column, entry.Reason, api.ColumnWithheld)
	}
	// The memory-only cases: the store writes none of the four, so the state route — which reads
	// its claims from the store — would serve a constant for them while the operator route reads
	// the live machine.
	for _, field := range []string{"ProcessGone", "SuspensionHeld", "SuspensionHeldBy", "Compacting"} {
		if entry := claimTable[field]; entry.Column != api.ColumnOperator {
			t.Errorf("supervise.Claim.%s stands in %q, want %q: the store never writes it, so the state route would serve a constant for it",
				field, entry.Column, api.ColumnOperator)
		}
	}

	root, held := "LEGION-500", "LEGION-501"
	lingerUntil := projectedAt.Add(90 * time.Minute)
	answeredAt := projectedAt.Add(-time.Minute)
	declaredAt := projectedAt.Add(-2 * time.Hour)
	parent := "LEGION-400"
	// The issue record with every field set. It stands in another project than the pass's, so
	// the admission line — which is no part of the issue record — puts nothing of its own on the
	// view, and under a root of its own tree, so the hold's reason is served: the reason is
	// suppressed by its tree's linger, not by its own.
	issue := record.Issue{
		Key: held, Tree: root, Project: "OTHER", Title: "the held child", Parent: &parent,
		Phase: phase.AwaitingPerson, Generation: 3, Status: "in_progress", Rank: "00042",
		LingerUntil: &lingerUntil, Hold: &record.Hold{From: phase.Implementing, Reason: record.HoldEscalated},
		LastDispatchSeq: 91, ReadyPending: &record.ReadyPending{Step: record.DesignStepSpec, Version: 7},
		HandedOver: true, TakenBack: true, DispatchStatus: "needs_review",
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Ancestry: []string{parent, "LEGION-300"}, HandedOverFrom: "LEGION-200",
		Awaiting: &record.Awaiting{
			Ask: "9a8fd348-55d5-4d72-a793-3cd037f75f13", Since: record.EventMark{Seq: 12, At: projectedAt.Add(-time.Hour)},
			AnsweredAt: &answeredAt, AnsweredBy: record.AnsweredByAsk,
		},
		HandoutSeq: 88, DeclaredSeq: 89, DeclaredAt: declaredAt,
	}
	// The root the child reads its tree's facts from. It does not linger, a linger of the tree
	// being what suppresses the hold's reason.
	tree := record.Issue{
		Key: root, Tree: root, Project: "OTHER", Phase: phase.Implementing, Generation: 3,
		Status: "in_progress", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
	}
	drawnAt := projectedAt.Add(-36 * time.Hour)
	// The assignment record with every field set, recorded against the root for the unit `tree`,
	// which is the shape of the one experiment every deployment runs: the child below is shown
	// its tree's answer (assignmentViews), and the arm `memory` with an instant beside it is
	// what the issue's three memory fields are read off (memoryOf).
	drawn := record.Assignment{
		Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "random",
		DrawnAt: &drawnAt, Carried: true,
	}

	token := claim.Token("legion-legion-legion-500-implementer")
	podUID := "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11"
	locator := sandboxLocator(token, "legion-legion-legion-500", podUID, 5)
	failedAt := projectedAt.Add(-10 * time.Minute)
	queuedAt := projectedAt.Add(-30 * time.Minute)
	openedAt := projectedAt.Add(-20 * time.Minute)
	// The claim record with every field set, on the root rather than on the child, so the child's
	// view carries no worker of its own and the two documents are read apart.
	current := supervise.Claim{
		Token: token, Project: "OTHER", Tree: root, Issue: root, Role: claim.RoleImplementer,
		Generation: 3, TreeEpoch: 2,
		Session: "ses_implementer_500", SessionFile: "/var/lib/legion/sessions/ses_implementer_500.jsonl",
		ServingGeneration: 2, ServingAssignmentID: "assignment-7",
		ServingCheckLook: supervise.CheckLook{Look: 2, OpenedAt: openedAt},
		LastStartRow:     811, DeliveredRows: []int64{809, 811},
		Locator:    locator,
		Enrollment: &supervise.Enrollment{ID: "enr_42", Incarnation: podUID + "/5"},
		State:      supervise.StateFailed,
		Budgets:    supervise.Budgets{LaunchFailures: 4, Deaths: 1, PromptFailures: 2, PromptRetires: 3},
		Failure:    "launch failures ran out", FailureCause: supervise.CauseDied, FailedAt: failedAt,
		PendingCause: supervise.CauseDied,
		Pending: &supervise.Delivery{
			ID: "dlv-9", Task: "implement the change, which no agent surface carries",
			Generation: 3, Assignment: "assignment-8", Phase: phase.Implementing, QueuedAt: queuedAt,
			SendFailures: 2, LastSendFailureAt: projectedAt.Add(-5 * time.Minute),
			LastSendError: "post to the worker stream: connection refused",
		},
		BootTokenHash: []byte{0xb0, 0x07}, CapabilityHash: []byte{0xca, 0xfe},
		UncertainStreak: 1,
		SuspendedBy:     supervise.SuspendedByOperator, SuspensionHeld: true,
		SuspensionHeldBy: supervise.SuspendedByDaemon, Compacting: true, ProcessGone: true, DeathCharged: true,
		PluginContract: api.DaemonAPIVersion, PluginRelaunchFor: api.DaemonAPIVersion - 1,
		UpdateRound: "pod:" + podUID, UpdateReasons: "the pod template changed: the image moved",
		UpdateOpenedAt: &openedAt, UpdateTaskedAt: &openedAt, UpdateSignalledAt: &answeredAt,
		UpdateHead:              "4c1b8e2a6f9d3047b5e1c8a2f6d940b3e7c5a128",
		UpdateToldAt:            &answeredAt,
		UpdateAnsweredByTurnEnd: true, UpdateReaskedForPushCheck: true, UpdateLeftRound: "pod:older",
		ProviderErrorRun: 6,
		Step: supervise.StepRecord{
			At: projectedAt.Add(-time.Minute), TurnStartedAt: projectedAt.Add(-2 * time.Minute),
			Tool: "bash", CallID: "toolu_impl_11", ArgsBytes: 128, ArgsHash: "9b2e45c0d1f8a673",
			StartedAt: projectedAt.Add(-time.Minute), EndedAt: projectedAt.Add(-30 * time.Second),
		},
	}

	store := projectionStore{
		issues:      []record.Issue{tree, issue},
		assignments: map[string][]record.Assignment{root: {drawn}},
	}
	state, err := Project(context.Background(), nil, store,
		"LEGION", nil, nil, []supervise.Claim{current}, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	t.Run("the claim record", func(t *testing.T) {
		worker, carried := state.Issues[root].Workers[claim.RoleImplementer]
		if !carried {
			t.Fatalf("%s carries no implementer: the claim was projected onto no view", root)
		}
		document := wire(t, worker.Claim)
		// `capabilities` is no field of the claim record — it is what the claim's session last
		// reported — so the claim built here carries none and the view's keys are the record's.
		servedKeys(t, "supervise.Claim", document, api.ClaimRecordFields, nil)

		want := wire(t, map[string]any{
			"session":           current.Session,
			"state":             string(current.State),
			"locator":           current.Locator,
			"servingGeneration": current.ServingGeneration,
			"pluginContract":    current.PluginContract,
			"updateRound":       current.UpdateRound,
			"updateHead":        current.UpdateHead,
			"updateReasons":     current.UpdateReasons,
			"providerErrorRun":  current.ProviderErrorRun,
			// The pending task reduced to the five facts a reader decides on, and never its
			// text: that one is held, which the key's presence says, its phase, when it was
			// queued, how many sends failed and the last send error.
			"pendingTask": map[string]any{
				"phase":         current.Pending.Phase,
				"queuedAt":      current.Pending.QueuedAt,
				"sendFailures":  current.Pending.SendFailures,
				"lastSendError": current.Pending.LastSendError,
			},
			"step": map[string]any{
				"at": current.Step.At, "turnStartedAt": current.Step.TurnStartedAt,
				"tool": current.Step.Tool, "callId": current.Step.CallID,
				"argsBytes": current.Step.ArgsBytes, "argsHash": current.Step.ArgsHash,
				"startedAt": current.Step.StartedAt, "endedAt": current.Step.EndedAt,
			},
			"failure": map[string]any{
				"cause": current.FailureCause, "detail": current.Failure, "at": current.FailedAt,
				"attempts": map[string]any{
					"launchFailures": current.Budgets.LaunchFailures,
					"deaths":         current.Budgets.Deaths,
					"promptFailures": current.Budgets.PromptFailures,
					"promptRetires":  current.Budgets.PromptRetires,
				},
			},
			"pendingCause": current.PendingCause,
		})
		if got, wanted := canonical(t, document), canonical(t, want); got != wanted {
			t.Errorf("the claim view is %s, want the record's own values, %s", got, wanted)
		}
		// The task's text is the operator route's alone, so no claim view carries it.
		if strings.Contains(canonical(t, document), current.Pending.Task) {
			t.Errorf("the claim view carries the task's text: %s", canonical(t, document))
		}
	})

	t.Run("the issue record", func(t *testing.T) {
		view, carried := state.Issues[held]
		if !carried {
			t.Fatalf("the state carries no issue %s", held)
		}
		document := wire(t, view)
		// `workers` is written on every issue, empty rather than null, and is built from the
		// phases and the claims rather than from the issue record. `assignments` and the three
		// memory fields are built from the assignment record, which has a table of its own
		// (api.AssignmentRecordFields, the subtest below).
		servedKeys(t, "record.Issue", document, api.IssueRecordFields,
			[]string{"workers", "assignments", "memoryGroup", "memoryDrawMode", "memoryDrawnAt"})

		want := wire(t, map[string]any{
			"key": issue.Key, "tree": issue.Tree, "phase": issue.Phase,
			"generation": issue.Generation, "status": issue.Status,
			"lingerUntil": issue.LingerUntil,
			"holdReason":  issue.Hold.Reason, "heldFrom": issue.Hold.From,
			"repo": issue.Repo, "defaultBranch": issue.DefaultBranch,
			"ancestry": issue.Ancestry,
			"waitingOn": map[string]any{
				"kind": "ask", "ask": issue.Awaiting.Ask, "since": issue.Awaiting.Since.At,
				"answeredAt": issue.Awaiting.AnsweredAt, "answeredBy": issue.Awaiting.AnsweredBy,
			},
			"workers":        map[string]any{},
			"memoryGroup":    drawn.Variant,
			"memoryDrawMode": drawn.Mode,
			"memoryDrawnAt":  drawn.DrawnAt,
			"assignments": []map[string]any{{
				"experiment": drawn.Experiment, "variant": drawn.Variant,
				"mode": drawn.Mode, "drawnAt": drawn.DrawnAt,
			}},
		})
		if got, wanted := canonical(t, document), canonical(t, want); got != wanted {
			t.Errorf("the issue view is %s, want the record's own values, %s", got, wanted)
		}
	})

	t.Run("the assignment record", func(t *testing.T) {
		view, carried := state.Issues[held]
		if !carried {
			t.Fatalf("the state carries no issue %s", held)
		}
		if len(view.Assignments) != 1 {
			t.Fatalf("%s runs under %+v, want the one answer its tree was drawn for", held, view.Assignments)
		}
		document := wire(t, view.Assignments[0])
		// Every key of an assignment is the record's own: nothing else is built into the view.
		servedKeys(t, "record.Assignment", document, api.AssignmentRecordFields, nil)

		want := wire(t, map[string]any{
			"experiment": drawn.Experiment, "variant": drawn.Variant,
			"mode": drawn.Mode, "drawnAt": drawn.DrawnAt,
		})
		if got, wanted := canonical(t, document), canonical(t, want); got != wanted {
			t.Errorf("the assignment view is %s, want the record's own values, %s", got, wanted)
		}
	})
}

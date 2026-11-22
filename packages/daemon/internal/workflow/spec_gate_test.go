package workflow

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// specGateEngine is the engine of a deployment that gates the spec step alone,
// `gates.design: root-issues`, the deployment the change request's gate is written for.
func specGateEngine() *Engine {
	return stepsEngine([]record.DesignStep{record.DesignStepSpec}, nil)
}

// readOf is what register_gate or the prepare stage read of the root's change request at version.
func readOf(version int, specSource, proposesNothing bool) *intake.ChangeRequestRead {
	return &intake.ChangeRequestRead{Version: version, SpecSource: specSource, ProposesNothing: proposesNothing}
}

// specGateOf is the root's spec gate as the record holds it.
func specGateOf(t *testing.T, pool *pgxpool.Pool) record.DesignGate {
	t.Helper()
	var gate *record.DesignGate
	seedRecord(t, pool, func(tx pgx.Tx) error {
		gates, err := record.NewStore().GatesOf(t.Context(), tx, gatedRoot)
		gate = record.SpecGate(gates)
		return err
	})
	if gate == nil {
		t.Fatalf("the root holds no spec gate")
	}
	return *gate
}

// roleMessageBodies is the body of every role message the outbox holds for issue, in order.
func roleMessageBodies(t *testing.T, pool *pgxpool.Pool, issue string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload->>'body' from outbox where kind = 'role_message' and issue = $1 order by id`, issue)
	if err != nil {
		t.Fatalf("read the role messages of %s: %v", issue, err)
	}
	bodies, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the role messages of %s: %v", issue, err)
	}
	return bodies
}

// endingReads is every read of the ending condition the outbox holds for issue, in order.
func endingReads(t *testing.T, pool *pgxpool.Pool, issue string) []record.SpecEndingRead {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload->>'head', payload->>'cause' from outbox where kind = 'spec_ending_read' and issue = $1 order by id`, issue)
	if err != nil {
		t.Fatalf("read the ending reads of %s: %v", issue, err)
	}
	defer rows.Close()
	reads := []record.SpecEndingRead{}
	for rows.Next() {
		var read record.SpecEndingRead
		if err := rows.Scan(&read.Head, &read.Cause); err != nil {
			t.Fatalf("scan an ending read of %s: %v", issue, err)
		}
		reads = append(reads, read)
	}
	return reads
}

// The first registration of the spec step fixes the rule its gate starts under (R24): the rule
// of a change request when the registration read the issue's change request and Dispatch answered
// that the repository is a spec source, and the rule of a person's approval otherwise. Under the
// rule of a change request, a version that proposes nothing opens the gate with no person (R23):
// the exemption is recorded beside the approval, never as one, and the architect is told the
// opening in the design-approved notice, which names nobody and says why.
func TestTheFirstRegistrationFixesTheRuleAndAVersionThatProposesNothingOpensWithNoPerson(t *testing.T) {
	for _, tc := range []struct {
		name string
		read *intake.ChangeRequestRead
		rule record.GateRule
		open bool
	}{
		{"a spec source's version that proposes nothing", readOf(4, true, true), record.GateRuleChangeRequest, true},
		{"a spec source's version that proposes something", readOf(4, true, false), record.GateRuleChangeRequest, false},
		{"a repository that is no spec source", readOf(4, false, true), record.GateRuleApproval, false},
		{"a Dispatch older than the route", nil, record.GateRuleApproval, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			apply := applyFacts(t, pool, specGateEngine())
			seedGatedTree(t, pool)
			apply("register", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4, DispatchVersion: 4,
				Since: record.EventMark{Seq: 5}, ChangeRequest: tc.read})
			gate := specGateOf(t, pool)
			if gate.Rule != tc.rule || record.GateOpen(gate) != tc.open || gate.ApprovedVersion != nil {
				t.Fatalf("the gate = %+v, want rule %q, open %t and no approval", gate, tc.rule, tc.open)
			}
			told := noticesOfKind(t, pool, gatedRoot, "design-approved")
			if !tc.open {
				if len(told) != 0 {
					t.Fatalf("design-approved notices of a closed gate = %+v, want none", told)
				}
				return
			}
			if gate.ExemptVersion == nil || *gate.ExemptVersion != 4 {
				t.Fatalf("the gate's exemption = %v, want version 4", gate.ExemptVersion)
			}
			want := []record.Notice{{Kind: "design-approved", Version: 4, Ref: stepRef(gatedRoot, record.DesignStepSpec),
				Reason: "no proposal: version 4 of the change request proposes no statement, component or interface and changes no approved check, so no person approves it"}}
			if !reflect.DeepEqual(told, want) {
				t.Fatalf("design-approved notices = %+v, want %+v", told, want)
			}
		})
	}
}

// A later registration keeps the rule its gate started under (R24): a gate under the rule of a
// person's approval stays under it when Dispatch later answers that the repository is a spec
// source, and a gate under the rule of a change request stays under it when a later read finds no
// route, with no exemption for a version nothing read.
func TestALaterRegistrationKeepsTheRuleItsGateStartedUnder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored record.GateRule
		read   *intake.ChangeRequestRead
	}{
		{"an approval's gate read as a spec source's", record.GateRuleApproval, readOf(4, true, true)},
		{"a change request's gate read by an older Dispatch", record.GateRuleChangeRequest, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			apply := applyFacts(t, pool, specGateEngine())
			seedGatedTree(t, pool)
			seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 3, Rule: tc.stored})
			apply("register", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4, DispatchVersion: 4,
				Since: record.EventMark{Seq: 5}, ChangeRequest: tc.read})
			if gate := specGateOf(t, pool); gate.Rule != tc.stored || record.GateOpen(gate) {
				t.Fatalf("the gate after a later registration = %+v, want rule %q kept and the gate closed", gate, tc.stored)
			}
		})
	}
}

// A version of an exempt gate's document is read before it is applied: a version that proposes
// nothing moves the exemption with it, so the gate stays open and nobody is told it moved; one that
// proposes something closes it as a version always has, and the tree is told; and a later version
// that proposes nothing opens it again with no person, which the architect is told.
func TestAVersionThatProposesNothingKeepsTheExemptGateOpen(t *testing.T) {
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	seedGatedTree(t, pool)
	four := 4
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4,
		Rule: record.GateRuleChangeRequest, ExemptVersion: &four, EverOpened: true})
	version := func(n int, read *intake.ChangeRequestRead) intake.DispatchArtifact {
		return intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: n,
			Mark: record.EventMark{Seq: int64(10 + n)}, ChangeRequest: read}
	}

	apply("v5", version(5, readOf(5, true, true)))
	if gate := specGateOf(t, pool); !record.GateExempt(gate) || *gate.ExemptVersion != 5 {
		t.Fatalf("the gate after a version that proposes nothing = %+v, want it exempt at 5", gate)
	}
	if moved := roleMessageBodies(t, pool, gatedRoot); len(moved) != 0 {
		t.Fatalf("role messages after a version that proposes nothing = %q, want none: the gate did not move", moved)
	}
	if told := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(told) != 0 {
		t.Fatalf("design-approved notices after a version that proposes nothing = %+v, want none", told)
	}

	apply("v6", version(6, readOf(6, true, false)))
	if gate := specGateOf(t, pool); record.GateOpen(gate) {
		t.Fatalf("the gate after a version that proposes something = %+v, want it closed", gate)
	}
	if moved := roleMessageBodies(t, pool, gatedRoot); len(moved) != 1 || !strings.Contains(moved[0], "The design gate of "+gatedRoot+" closed") {
		t.Fatalf("role messages after a version that proposes something = %q, want the closed gate told", moved)
	}

	apply("v7", version(7, readOf(7, true, true)))
	gate := specGateOf(t, pool)
	if !record.GateExempt(gate) || gate.ApprovedVersion != nil {
		t.Fatalf("the gate after a later version that proposes nothing = %+v, want it exempt at 7 with no approval", gate)
	}
	told := noticesOfKind(t, pool, gatedRoot, "design-approved")
	if len(told) != 1 || told[0].Actor != "" || told[0].Version != 7 || !strings.HasPrefix(told[0].Reason, "no proposal: version 7 of the change request") {
		t.Fatalf("design-approved notices after the opening by exemption = %+v, want one at 7 naming nobody", told)
	}
}

// A version nothing read — the prepare stage found no route — and a person's changes request each
// close an exempt gate: neither is a version Dispatch read as proposing nothing.
func TestAnExemptGateClosesOnAnUnreadVersionAndOnAChangesRequest(t *testing.T) {
	four := 4
	exempt := record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4,
		Rule: record.GateRuleChangeRequest, ExemptVersion: &four, EverOpened: true}
	for _, tc := range []struct {
		name string
		fact intake.DispatchArtifact
	}{
		{"an unread version", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 5,
			Mark: record.EventMark{Seq: 15}}},
		{"a person's changes request", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactChangesRequested,
			Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 15}, Actor: dispatch.Actor{Kind: "user", ID: "alice"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			apply := applyFacts(t, pool, specGateEngine())
			seedGatedTree(t, pool)
			seedGate(t, pool, exempt)
			apply("event", tc.fact)
			if gate := specGateOf(t, pool); record.GateOpen(gate) || gate.Rule != record.GateRuleChangeRequest {
				t.Fatalf("the gate = %+v, want it closed under the rule of a change request", gate)
			}
		})
	}
}

// The gate seed's reset of a gate above the version Dispatch holds keeps the rule the gate started
// under, and the version it resets to opens with no person when the seed read it as proposing
// nothing.
func TestTheResetAboveDispatchKeepsTheRuleAndReadsTheVersion(t *testing.T) {
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	seedGatedTree(t, pool)
	six := 6
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 6,
		Rule: record.GateRuleChangeRequest, ExemptVersion: &six, EverOpened: true})
	apply("reset", intake.GateAboveDispatch{Generation: 1, Above: 6, Read: intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc,
		Kind: intake.DispatchArtifactVersion, Version: 5, ChangeRequest: readOf(5, true, true)}})
	if gate := specGateOf(t, pool); gate.LatestVersion != 5 || !record.GateExempt(gate) {
		t.Fatalf("the reset gate = %+v, want version 5 exempt under the rule of a change request", gate)
	}
}

// mergingExemptRoot records the root in merging with its pull request at head and its spec gate
// open by exemption: the tree whose landing D10's ending condition holds.
func mergingExemptRoot(t *testing.T, pool *pgxpool.Pool, head string) {
	t.Helper()
	seedGatedTree(t, pool)
	seedRecord(t, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `update issues set phase = 'merging' where key = $1`, gatedRoot)
		return err
	})
	four := 4
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4,
		Rule: record.GateRuleChangeRequest, ExemptVersion: &four, EverOpened: true})
	seedPR(t, pool, record.PullRequest{Issue: gatedRoot, Repo: "sjawhar/legion", Number: 42, Branch: "legion/" + gatedRoot, Base: "main",
		HeadSHA: head, State: record.PullRequestOpen, Generation: 1})
}

// movePRHead moves the root's recorded pull request to head, as a synchronize recorded it.
func movePRHead(t *testing.T, pool *pgxpool.Pool, head string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `update pull_requests set head_sha = $2 where issue = $1`, gatedRoot, head)
		return err
	})
}

// specEndingOf is the reading the record holds for the root.
func specEndingOf(t *testing.T, pool *pgxpool.Pool) *record.SpecEnding {
	t.Helper()
	var ending *record.SpecEnding
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		ending, err = record.NewStore().SpecEnding(t.Context(), tx, gatedRoot)
		return err
	})
	return ending
}

// A read of the ending condition (D10) records the reading, tells the tree root's architect each
// reason once per head in a spec-ending-waits notice, and tells the running merger when the
// reading turns: the closed-gate message when it waits after a ready reading or none, and the
// opened-gate message when it is ready after one that waited. A reading that changes nothing tells
// nobody, a new head tells each reason again, and a read of a head the pull request moved past
// takes nothing.
func TestAReadOfTheEndingTellsTheMergerWhenItTurnsAndTheOwnerOncePerReasonAndHead(t *testing.T) {
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	const first, second = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	mergingExemptRoot(t, pool, first)
	reasons := func(codes ...string) []dispatch.EndingReason {
		var out []dispatch.EndingReason
		for _, code := range codes {
			out = append(out, dispatch.EndingReason{Code: code, Detail: "Dispatch says " + code + "."})
		}
		return out
	}
	read := func(id, head, state string, codes ...string) {
		t.Helper()
		apply(id, intake.SpecEndingRead{Issue: gatedRoot, Head: head, State: state, Reasons: reasons(codes...), Version: 4, Cause: record.SpecEndingCauseHead})
	}
	codesTold := func() []string {
		var codes []string
		for _, notice := range noticesOfKind(t, pool, gatedRoot, record.SpecEndingWaitsNotice) {
			codes = append(codes, notice.Reason)
		}
		return codes
	}

	read("r1", first, "waiting", "no_final_list_for_head", "no_conclusion")
	if got, want := codesTold(), []string{"no_final_list_for_head", "no_conclusion"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reasons told = %v, want %v", got, want)
	}
	notice := noticesOfKind(t, pool, gatedRoot, record.SpecEndingWaitsNotice)[1]
	for _, says := range []string{gatedRoot, first, "no_conclusion", "Dispatch says no_conclusion.", "Write or correct the conclusion block in the spec of " + gatedRoot + "."} {
		if !strings.Contains(notice.Summary, says) {
			t.Errorf("the notice of no_conclusion says %q, want it to say %q", notice.Summary, says)
		}
	}
	messages := roleMessageBodies(t, pool, gatedRoot)
	if len(messages) != 1 || !strings.HasPrefix(messages[0], "The design gate of "+gatedRoot+" closed: the ending condition of its change request does not hold for head "+first) ||
		!strings.Contains(messages[0], "dispatch spec-ending --issue "+gatedRoot+" --head <head>") {
		t.Fatalf("role messages after the first waiting read = %q, want the closed gate told once", messages)
	}
	if ending := specEndingOf(t, pool); ending == nil || !ending.WaitsAt(first) {
		t.Fatalf("the reading = %+v, want it waiting at %s", ending, first)
	}

	read("r2", first, "waiting", "no_final_list_for_head", "no_conclusion")
	read("r3", first, "waiting", "no_conclusion", "incomplete")
	if got, want := codesTold(), []string{"no_final_list_for_head", "no_conclusion", "incomplete"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reasons told after two more waiting reads = %v, want %v: each reason once at a head", got, want)
	}
	if got := roleMessageBodies(t, pool, gatedRoot); len(got) != 1 {
		t.Fatalf("role messages after readings that still wait = %q, want the one closed message", got)
	}

	read("r4", first, "ready")
	messages = roleMessageBodies(t, pool, gatedRoot)
	if len(messages) != 2 || !strings.HasPrefix(messages[1], "The design gate of "+gatedRoot+" opened: the ending condition of its change request holds for head "+first) {
		t.Fatalf("role messages after the ready read = %q, want the opened gate told", messages)
	}

	read("r5", first, "waiting", "no_conclusion")
	movePRHead(t, pool, second)
	read("stale", first, "ready")
	if ending := specEndingOf(t, pool); ending == nil || !ending.WaitsAt(first) {
		t.Fatalf("the reading after a read of a head moved past = %+v, want the waiting reading at %s kept", ending, first)
	}
	read("r6", second, "waiting", "no_conclusion")
	if got := codesTold(); len(got) != 4 || got[3] != "no_conclusion" {
		t.Fatalf("reasons told after a new head = %v, want no_conclusion told again", got)
	}
	if got := roleMessageBodies(t, pool, gatedRoot); len(got) != 3 {
		t.Fatalf("role messages = %q, want closed, opened and closed again, and nothing for the stale read or the new head's same state", got)
	}
}

// A head move of a merging member's pull request, a list of outcomes recorded on an issue of the
// tree, and a version or an approval of the root's spec document that leaves it open by exemption
// each queue a read of the ending condition for every member in merging (D10). A tree whose gate is
// not open by exemption queues none.
func TestTheEventsThatTurnTheEndingQueueAReadForEveryMergingMember(t *testing.T) {
	const first, second = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	t.Run("a head move", func(t *testing.T) {
		pool := migratedPool(t)
		apply := applyFacts(t, pool, specGateEngine())
		mergingExemptRoot(t, pool, first)
		apply("sync", intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/" + gatedRoot, Before: first, HeadSHA: second,
			Sender: "legion-implementer[bot]", Base: "main", UpdatedAt: time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)})
		if got, want := endingReads(t, pool, gatedRoot), []record.SpecEndingRead{{Head: second, Cause: record.SpecEndingCauseHead}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ending reads after a head move = %+v, want %+v", got, want)
		}
	})
	t.Run("a list of outcomes", func(t *testing.T) {
		pool := migratedPool(t)
		apply := applyFacts(t, pool, specGateEngine())
		mergingExemptRoot(t, pool, first)
		apply("outcomes", intake.SpecOutcomesRecorded{Key: gatedRoot, Seq: 9, Record: 3, PullRequest: "sjawhar/legion#42", Head: first, List: "final"})
		if got, want := endingReads(t, pool, gatedRoot), []record.SpecEndingRead{{Head: first, Cause: record.SpecEndingCauseOutcomes}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ending reads after a list of outcomes = %+v, want %+v", got, want)
		}
	})
	t.Run("a version and an approval of the root's spec", func(t *testing.T) {
		pool := migratedPool(t)
		apply := applyFacts(t, pool, specGateEngine())
		mergingExemptRoot(t, pool, first)
		apply("v5", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 5,
			Mark: record.EventMark{Seq: 15}, ChangeRequest: readOf(5, true, true)})
		apply("approve-v5", stepApproval(gatedRoot, gateDoc, 5, record.EventMark{Seq: 16}))
		want := []record.SpecEndingRead{{Head: first, Cause: record.SpecEndingCauseVersion}, {Head: first, Cause: record.SpecEndingCauseApproval}}
		if got := endingReads(t, pool, gatedRoot); !reflect.DeepEqual(got, want) {
			t.Fatalf("ending reads after a version and an approval = %+v, want %+v", got, want)
		}
	})
	t.Run("a gate open by a person's approval", func(t *testing.T) {
		pool := migratedPool(t)
		apply := applyFacts(t, pool, specGateEngine())
		mergingExemptRoot(t, pool, first)
		four := 4
		seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4,
			ApprovedVersion: &four, EverOpened: true})
		apply("outcomes", intake.SpecOutcomesRecorded{Key: gatedRoot, Seq: 9, Record: 3, PullRequest: "sjawhar/legion#42", Head: first, List: "final"})
		if got := endingReads(t, pool, gatedRoot); len(got) != 0 {
			t.Fatalf("ending reads under a gate a person opened = %+v, want none", got)
		}
	})
}

// The merger's start task of a member of a tree whose spec gate stands open by exemption ends with
// the paragraph that tells it to read the ending condition before each merge and each enqueue; a
// tree whose gate a person opened gets the task as it was.
func TestTheMergerTaskOfAnExemptTreeEndsWithTheEndingStep(t *testing.T) {
	pool := migratedPool(t)
	engine := specGateEngine()
	mergingExemptRoot(t, pool, "1111111111111111111111111111111111111111")
	issue := recordedRoot(t, pool)
	var exempt, approved string
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		exempt, err = engine.mergingTask(t.Context(), tx, issue, "Continue the work.")
		return err
	})
	want := "Continue the work. The design gate of " + gatedRoot + " opened with no person's approval: its change request proposes nothing. " +
		"Before each merge and each enqueue, run `dispatch spec-ending --issue " + gatedRoot + " --head <head>`, and merge or enqueue only when its first line reads `ready`."
	if exempt != want {
		t.Fatalf("the merger's task of an exempt tree = %q, want %q", exempt, want)
	}
	four := 4
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: &four, EverOpened: true})
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		approved, err = engine.mergingTask(t.Context(), tx, issue, "Continue the work.")
		return err
	})
	if approved != "Continue the work." {
		t.Fatalf("the merger's task of a tree a person approved = %q, want it as it was", approved)
	}
}

// A refused approval of a stale change request (D8) tells the issue's owner, the architect of its
// tree root, in one notice that names the issue, each stale node with its base and current version
// and the step to take; the same event delivered twice tells it once. An issue the daemon does not
// record has no owner it knows, and the controller is told instead.
func TestAStaleChangeRequestTellsItsOwnerOnce(t *testing.T) {
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	seedGatedTree(t, pool)
	stale := intake.SpecRequestStale{Key: gatedRoot, Seq: 9, ApprovalRequest: "the-request", Reason: "the approved spec changed a node",
		Statements: []intake.StaleNode{{ID: "a-bill-names-its-order", Base: 1, Current: 2}, {ID: "an-order-is-paid-once", Base: 3, Removed: true}}}
	apply("stale", stale)
	apply("stale", stale)
	told := noticesOfKind(t, pool, gatedRoot, record.SpecRequestStaleNotice)
	if len(told) != 1 {
		t.Fatalf("spec-request-stale notices = %+v, want one", told)
	}
	for _, says := range []string{gatedRoot, "a-bill-names-its-order (base 1, current 2)", "an-order-is-paid-once (base 3, removed from the approved spec)",
		"the approved spec changed a node.", "Write the request again against the approved spec.", "Then open a new approval request."} {
		if !strings.Contains(told[0].Summary, says) {
			t.Errorf("the notice says %q, want it to say %q", told[0].Summary, says)
		}
	}

	unrecorded := fmt.Sprintf("%s-%d", "LEGION", 977)
	apply("stale-unrecorded", intake.SpecRequestStale{Key: unrecorded, Seq: 3, Reason: "r", Statements: []intake.StaleNode{{ID: "x", Base: 1, Current: 2}}})
	var kinds []string
	rows, err := pool.Query(context.Background(), `select kind from outbox where issue = $1 and payload->>'kind' = 'spec-request-stale'`, unrecorded)
	if err != nil {
		t.Fatalf("read the notices of %s: %v", unrecorded, err)
	}
	kinds, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || !slices.Equal(kinds, []string{string(record.OutboxKindControllerNotice)}) {
		t.Fatalf("the notices of an unrecorded issue = %v, %v; want the controller's alone", kinds, err)
	}
	var summary string
	if err := pool.QueryRow(context.Background(), `select payload->>'summary' from outbox where issue = $1 and kind = 'controller_notice'`, unrecorded).Scan(&summary); err != nil {
		t.Fatalf("read the controller's copy: %v", err)
	}
	if !strings.HasSuffix(summary, "Name an owner for "+unrecorded+". The owner writes the request again against the approved spec and opens a new approval request.") ||
		strings.Contains(summary, "Then open a new approval request.") {
		t.Fatalf("the controller's copy says %q, want the controller's own step in place of the owner's", summary)
	}
}

// An opening by exemption is no person's approval, and the message every role of the tree reads
// says so (R23): a gate a version that proposes something closed, and a later version that
// proposes nothing opened again, tells each phase worker and the root's architect that it opened
// with no person's approval and why, and the merger's message ends with the step it takes before
// each merge and each enqueue (D10). An opening by a person's approval still names the person's
// approval.
func TestAnOpeningByExemptionTellsEveryRoleNoPersonApprovedIt(t *testing.T) {
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	mergingExemptRoot(t, pool, "1111111111111111111111111111111111111111")
	child := fmt.Sprintf("%s-%d", "LEGION", 611)
	seedIssue(t, pool, record.Issue{Key: child, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "a child under test", Parent: new(gatedRoot), Phase: phase.Testing,
		Generation: 1, Status: "testing", Rank: "V"})
	version := func(n int, proposesNothing bool) intake.DispatchArtifact {
		return intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: n,
			Mark: record.EventMark{Seq: int64(10 + n)}, ChangeRequest: readOf(n, true, proposesNothing)}
	}

	apply("v5", version(5, false))
	apply("v6", version(6, true))
	apply("v7", version(7, false))
	apply("approve-v7", stepApproval(gatedRoot, gateDoc, 7, record.EventMark{Seq: 20}))

	exempt := fmt.Sprintf("The design gate of %s opened with no person's approval: version 6 of its change request proposes nothing, "+
		"so no person approves it. The change request is its spec document (%s). Every step the deployment gates now stands open. "+
		"A merge waits until the ending condition of the change request holds.", gatedRoot, gateDoc)
	step := "Before each merge and each enqueue, run `dispatch spec-ending --issue " + gatedRoot + " --head <head>`, " +
		"and merge or enqueue only when its first line reads `ready`."
	closed5, closed7 := gateMovedBody(record.DesignStepSpec, gateDoc, 5, false), gateMovedBody(record.DesignStepSpec, gateDoc, 7, false)
	approved := gateMovedBody(record.DesignStepSpec, gateDoc, 7, true)
	merger, architect := treeRoleToken(t, gatedRoot, claim.RoleMerger), treeRoleToken(t, gatedRoot, claim.RoleArchitect)
	assertRoleMessages(t, pool, gatedRoot, []record.RoleMessage{
		{Role: merger, Body: closed5}, {Role: architect, Body: closed5},
		{Role: merger, Body: exempt + " " + step}, {Role: architect, Body: exempt},
		{Role: merger, Body: closed7}, {Role: architect, Body: closed7},
		{Role: merger, Body: approved}, {Role: architect, Body: approved},
	})
	tester := treeRoleToken(t, child, claim.RoleTester)
	assertRoleMessages(t, pool, child, []record.RoleMessage{
		{Role: tester, Body: closed5}, {Role: tester, Body: exempt}, {Role: tester, Body: closed7}, {Role: tester, Body: approved},
	})
}

// endingExempted is the cause a read of the ending condition carries when the root's spec gate
// turned open by exemption: a merger that already runs was started without the step its task
// would name (D10).
const endingExempted = record.SpecEndingCause("exempted")

// mergerEndingMessages is the body of every role message the root's merger holds, in order.
func mergerEndingMessages(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	merger := treeRoleToken(t, gatedRoot, claim.RoleMerger)
	rows, err := pool.Query(t.Context(), `select payload->>'body' from outbox where kind = 'role_message' and issue = $1 and payload->>'role' = $2 order by id`,
		gatedRoot, merger)
	if err != nil {
		t.Fatalf("read the merger's role messages: %v", err)
	}
	bodies, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the merger's role messages: %v", err)
	}
	return bodies
}

// The first reading of the ending condition a merger gets tells it how the condition stands,
// whichever way it reads (D10): a ready one tells it the gate opened for that head, with the step
// it takes before each merge and each enqueue, as a waiting one tells it the gate closed. A merger
// that started before its tree's gate stood open by exemption has no other word of that step.
func TestTheFirstReadingOfTheEndingTellsTheMergerTheStep(t *testing.T) {
	pool := migratedPool(t)
	apply := applyFacts(t, pool, specGateEngine())
	const head = "1111111111111111111111111111111111111111"
	mergingExemptRoot(t, pool, head)
	apply("ready", intake.SpecEndingRead{Issue: gatedRoot, Head: head, State: record.SpecEndingReady, Version: 4, Cause: record.SpecEndingCauseOutcomes})
	want := "The design gate of " + gatedRoot + " opened: the ending condition of its change request holds for head " + head + ". " +
		"Before each merge and each enqueue, run `dispatch spec-ending --issue " + gatedRoot + " --head <head>`, and merge or enqueue only when its first line reads `ready`."
	if got := mergerEndingMessages(t, pool); !slices.Equal(got, []string{want}) {
		t.Fatalf("the merger's messages after its first reading, a ready one = %q, want %q", got, want)
	}
}

// A merger already running when its tree's spec gate turns open by exemption — a person approved
// the version it started under, and a later version proposes nothing — has no word of the ending
// step: its task was written under a person's approval, and the gate did not move, so nobody told
// it the gate moved. The version queues the read of the ending condition under the cause that says
// the gate turned exempt, and that read tells the merger how the condition stands, with the step,
// whatever reading the record kept from an earlier time the gate stood exempt.
func TestAMergerRunningWhenTheGateTurnsExemptIsToldTheStep(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	step := "Before each merge and each enqueue, run `dispatch spec-ending --issue " + gatedRoot + " --head <head>`, " +
		"and merge or enqueue only when its first line reads `ready`."
	for _, tc := range []struct {
		name  string
		state string
		codes []dispatch.EndingReason
		want  string
	}{
		{name: "a ready reading", state: record.SpecEndingReady,
			want: "The design gate of " + gatedRoot + " opened: the ending condition of its change request holds for head " + head + ". " + step},
		{name: "a waiting reading", state: record.SpecEndingWaiting, codes: []dispatch.EndingReason{{Code: "no_conclusion", Detail: "No conclusion."}},
			want: "The design gate of " + gatedRoot + " closed: the ending condition of its change request does not hold for head " + head + " (no_conclusion: No conclusion.). " +
				"Merge nothing. Enqueue nothing. Take a queued pull request out of the merge queue. " + step},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			apply := applyFacts(t, pool, specGateEngine())
			mergingExemptRoot(t, pool, head)
			four := 4
			seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4,
				Rule: record.GateRuleChangeRequest, ApprovedVersion: &four, EverOpened: true})
			// The reading an earlier time the gate stood exempt left, in the state the read answers.
			seedRecord(t, pool, func(tx pgx.Tx) error {
				return record.NewStore().PutSpecEnding(t.Context(), tx, record.SpecEnding{Issue: gatedRoot, Head: head, State: tc.state,
					ReadAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
			})

			apply("v5", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 5,
				Mark: record.EventMark{Seq: 15}, ChangeRequest: readOf(5, true, true)})
			if gate := specGateOf(t, pool); !record.GateExempt(gate) {
				t.Fatalf("the gate after a version that proposes nothing = %+v, want it exempt at 5", gate)
			}
			reads := endingReads(t, pool, gatedRoot)
			if want := []record.SpecEndingRead{{Head: head, Cause: endingExempted}}; !reflect.DeepEqual(reads, want) {
				t.Errorf("ending reads after the gate turned exempt = %+v, want %+v", reads, want)
			}
			if len(reads) != 1 {
				t.FailNow()
			}
			apply("read", intake.SpecEndingRead{Issue: gatedRoot, Head: head, State: tc.state, Reasons: tc.codes, Version: 5, Cause: reads[0].Cause})
			if got := mergerEndingMessages(t, pool); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("the merger's messages after the read = %q, want %q", got, tc.want)
			}
		})
	}
}

// A registration, or the gate seed's reset of a gate above the version Dispatch holds, that leaves
// the root's spec gate open by exemption queues the read of the ending condition for every member
// in merging, as a version event does (D10): under the cause that says the gate turned exempt when
// it stood otherwise before, and under the cause of a version when it stood exempt already.
func TestARegistrationOrResetThatLeavesTheGateExemptQueuesTheEndingReads(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	register := func(version int) intake.Fact {
		return intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: version, DispatchVersion: version,
			Since: record.EventMark{Seq: 30}, ChangeRequest: readOf(version, true, true)}
	}
	reset := intake.GateAboveDispatch{Generation: 1, Above: 6, Read: intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc,
		Kind: intake.DispatchArtifactVersion, Version: 5, ChangeRequest: readOf(5, true, true)}}
	four, six := 4, 6
	for _, tc := range []struct {
		name  string
		gate  record.DesignGate
		fact  intake.Fact
		cause record.SpecEndingCause
	}{
		{name: "a registration that opens a closed gate", gate: record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc,
			LatestVersion: 4, Rule: record.GateRuleChangeRequest, EverOpened: true}, fact: register(5), cause: endingExempted},
		{name: "a registration that turns an approved gate exempt", gate: record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc,
			LatestVersion: 4, Rule: record.GateRuleChangeRequest, ApprovedVersion: &four, EverOpened: true}, fact: register(5), cause: endingExempted},
		{name: "a registration of a newer version of an exempt gate", gate: record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc,
			LatestVersion: 4, Rule: record.GateRuleChangeRequest, ExemptVersion: &four, EverOpened: true}, fact: register(5), cause: record.SpecEndingCauseVersion},
		{name: "a reset that opens a closed gate", gate: record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc,
			LatestVersion: 6, Rule: record.GateRuleChangeRequest, EverOpened: true}, fact: reset, cause: endingExempted},
		{name: "a reset of an exempt gate", gate: record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc,
			LatestVersion: 6, Rule: record.GateRuleChangeRequest, ExemptVersion: &six, EverOpened: true}, fact: reset, cause: record.SpecEndingCauseVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			apply := applyFacts(t, pool, specGateEngine())
			mergingExemptRoot(t, pool, head)
			seedGate(t, pool, tc.gate)
			apply("fact", tc.fact)
			if gate := specGateOf(t, pool); !record.GateExempt(gate) {
				t.Fatalf("the gate after the fact = %+v, want it exempt", gate)
			}
			if got, want := endingReads(t, pool, gatedRoot), []record.SpecEndingRead{{Head: head, Cause: tc.cause}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("ending reads = %+v, want %+v", got, want)
			}
		})
	}
}

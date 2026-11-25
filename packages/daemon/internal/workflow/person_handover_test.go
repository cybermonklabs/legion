package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// parkedRootView is Dispatch's view of LEGION-208 as the daemon's prepare stage reads it for a
// person's approval: a labelled root with no parent, in status, whose primary document is
// artifact-208, the document the approvals here are of.
func parkedRootView(status string) *dispatch.Issue {
	return &dispatch.Issue{Key: "LEGION-208", Project: "LEGION", Title: "root", Status: status, Labels: []string{dispatch.LegionLabel},
		HandedOver: true, PrimaryArtifactID: "artifact-208"}
}

// approvalOf is an approval of version 2 of LEGION-208's document artifact-208 by actor, with view
// as what the prepare stage read of the issue (nil for a fact nothing prepared).
func approvalOf(actor dispatch.Actor, view *dispatch.Issue) intake.DispatchArtifact {
	return intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2, Mark: askSeq(9), Actor: actor, Issue: view}
}

// personHandoverWrite is the row the daemon queues for a person's handover found at status.
func personHandoverWrite(status string) record.StatusWrite {
	return record.PersonHandoverStatusWrite("todo", status)
}

// statusWrites decodes the queued dispatch_status rows of issue, oldest first.
func statusWrites(t *testing.T, pool *pgxpool.Pool, issue string) []record.StatusWrite {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload from outbox where kind = 'dispatch_status' and issue = $1 order by id", issue)
	if err != nil {
		t.Fatalf("read status writes: %v", err)
	}
	defer rows.Close()
	var writes []record.StatusWrite
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan status write: %v", err)
		}
		var write record.StatusWrite
		if err := json.Unmarshal(payload, &write); err != nil {
			t.Fatalf("decode status write %s: %v", payload, err)
		}
		writes = append(writes, write)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate status writes: %v", err)
	}
	return writes
}

// personHandoverLines counts the lines of the engine's JSON log that say it queued a person's
// handover of issue from source, the one message admission logs its own under.
func personHandoverLines(t *testing.T, logged *bytes.Buffer, issue, source string) int {
	t.Helper()
	return len(jsonLogLines(logged, func(entry map[string]any) bool {
		return entry["msg"] == "admission: queued a person's handover" && entry["issue"] == issue && entry["source"] == source
	}))
}

// A person's approval of the primary document of a parked labelled root the daemon has no record
// of is the person's handover of it: the daemon queues the root's todo through its outbox, observed
// at the status the approval found so the delivery writes nothing once the status moved, marked as
// a person's handover, and tells no architect or controller of it — the root has no tree. The
// queue is logged once, as admission logs its own, with source approval.
func TestAPersonsApprovalOfAnUnrecordedParkedRootsSpecIsQueuedByTheDaemon(t *testing.T) {
	for _, status := range []string{"backlog", "icebox"} {
		t.Run(status, func(t *testing.T) {
			pool := migratedPool(t)
			var logged bytes.Buffer
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, slog.New(slog.NewJSONHandler(&logged, nil))))

			apply("approval", approvalOf(askPerson, parkedRootView(status)))

			assertOutboxKinds(t, pool, []string{"dispatch_status"})
			if got, want := statusWrites(t, pool, "LEGION-208"), []record.StatusWrite{personHandoverWrite(status)}; !reflect.DeepEqual(got, want) {
				t.Fatalf("status writes = %#v, want %#v", got, want)
			}
			if lines := personHandoverLines(t, &logged, "LEGION-208", "approval"); lines != 1 {
				t.Fatalf("queued-a-person's-handover lines with source approval = %d, want one; the engine logged:\n%s", lines, logged.String())
			}
		})
	}
}

// Only a person's own approval of a parked labelled root's primary document queues it. A session's
// review is not a person's act (Dispatch refuses a bearer's review, and the daemon checks the actor
// as well); a carried approval is Dispatch's bookkeeping of an earlier approval, not a new act; a
// changes request approves nothing; a fact with no view of the issue — the gate seed's, the boot
// catch-up's — was not prepared as a person's approval; and a child, an unlabelled root, a document
// that is not the issue's primary one, or a root that is not parked is not Legion's handover to
// queue. Each writes nothing and wakes nobody.
func TestOnlyAPersonsApprovalOfAParkedLabelledRootsPrimaryDocumentQueuesIt(t *testing.T) {
	carried := approvalOf(askPerson, parkedRootView("backlog"))
	carried.Reason = intake.CarriedApprovalReasonPrefix + " 2: the answer to the decision block was the whole change"
	changes := approvalOf(askPerson, parkedRootView("backlog"))
	changes.Kind, changes.Reason = intake.DispatchArtifactChangesRequested, "tighten it"
	child := parkedRootView("backlog")
	child.Parent = new("LEGION-200")
	unlabelled := parkedRootView("backlog")
	unlabelled.Labels, unlabelled.HandedOver = nil, false
	otherDocument := parkedRootView("backlog")
	otherDocument.PrimaryArtifactID = "artifact-plan"
	for _, tc := range []struct {
		name string
		fact intake.DispatchArtifact
	}{
		{"a session's approval", approvalOf(askArchitect, parkedRootView("backlog"))},
		{"a carried approval", carried},
		{"a person's changes request", changes},
		{"a fact with no view of the issue", approvalOf(askPerson, nil)},
		{"a child's document", approvalOf(askPerson, child)},
		{"an unlabelled root", approvalOf(askPerson, unlabelled)},
		{"a document that is not the primary one", approvalOf(askPerson, otherDocument)},
		{"a root in todo", approvalOf(askPerson, parkedRootView("todo"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

			apply("approval", tc.fact)

			assertOutboxKinds(t, pool, []string{})
		})
	}
}

// lingeringRoot is LEGION-208 recorded where a root's leaving the workflow leaves it: its phase
// done, its tree lingering, in status.
func lingeringRoot(status string) record.Issue {
	until := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	return record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: status, Rank: "U", HandedOver: true, LingerUntil: &until}
}

// A parked labelled root the daemon recorded — one that ran and was parked, its tree lingering —
// is queued by a person's approval of its spec as an unrecorded one is: the write re-admits it as a
// new generation, as a person's todo does. The gate its earlier run registered records the approved
// version and nothing else: no design-approved notice is written for a tree with no architect, and
// the re-admission then keeps no stale approval pair before it clears the gate, while the new
// architect's register_gate reads the approval from Dispatch.
func TestAPersonsApprovalOfARecordedParkedRootsSpecQueuesItAndRecordsItQuietly(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, lingeringRoot("backlog"))
	gate := record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2}
	seedGate(t, pool, gate)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

	apply("approval", approvalOf(askPerson, parkedRootView("backlog")))

	assertOutboxKinds(t, pool, []string{"dispatch_status"})
	if got, want := statusWrites(t, pool, "LEGION-208"), []record.StatusWrite{personHandoverWrite("backlog")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("status writes = %#v, want %#v", got, want)
	}
	if got := gateState(t, pool, "LEGION-208"); got.ApprovedVersion == nil || *got.ApprovedVersion != 2 || got.LatestVersion != 2 {
		t.Fatalf("the parked root's gate = %#v, want the person's approval of version 2 recorded", got)
	}
	if kinds := noticeKinds(t, pool, "LEGION-208"); len(kinds) != 0 {
		t.Fatalf("notices for a parked root with no architect = %v, want none", kinds)
	}
}

// A person's approval that queues a parked root as their handover records on the root's gate only
// what applyArtifact would record: an approval of a version older than the gate's latest, one the
// gate has already seen superseded by a later changes request of that version, and any approval
// under gates.design off leave the gate as it was, while the root is still queued. Recording any
// of them would set an approved version the spec has moved past, which the re-admission's
// keepStaleApproval would then hand the new architect as a stale pair.
func TestAHandoverApprovalTheGateWouldNotTakeLeavesItAsItWas(t *testing.T) {
	two := 2
	for _, tc := range []struct {
		name   string
		policy config.DesignGate
		gate   record.DesignGate
	}{
		{name: "an approval of a version older than the gate's latest", policy: config.DesignGateRootIssues,
			gate: record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 3}},
		{name: "an approval a later changes request of its version superseded", policy: config.DesignGateRootIssues,
			gate: record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 2,
				ToldChangesVersion: &two, ToldChangesReason: "split the migration", ToldChangesMark: askSeq(12)}},
		{name: "an approval under gates.design off", policy: config.DesignGateOff,
			gate: record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, lingeringRoot("backlog"))
			seedGate(t, pool, tc.gate)
			apply := applyFacts(t, pool, testEngine(tc.policy, nil))

			apply("approval", approvalOf(askPerson, parkedRootView("backlog")))

			if got, want := statusWrites(t, pool, "LEGION-208"), []record.StatusWrite{personHandoverWrite("backlog")}; !reflect.DeepEqual(got, want) {
				t.Fatalf("status writes = %#v, want %#v", got, want)
			}
			if got := gateState(t, pool, "LEGION-208"); !reflect.DeepEqual(got, tc.gate) {
				t.Fatalf("the parked root's gate = %#v, want it left as %#v", got, tc.gate)
			}
		})
	}
}

// A root whose tree is live takes a person's approval on its gate as today, whatever Dispatch shows
// of its status: a slotted root whose person approved and then parked it, the approval prepared
// after the park and applied before the park's event, opens its gate and is told design-approved,
// and the park's event, not the approval, takes the tree out. A lingering root that is not parked
// — closed as done — is not one the approval queues either, and keeps today's behaviour: its gate
// takes the approval, so a reopen inside the linger sees it.
func TestAnApprovalOfALiveOrClosedRootsSpecIsAppliedToItsGateAsToday(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, pool *pgxpool.Pool)
		view *dispatch.Issue
	}{
		{"a slotted root Dispatch shows parked", func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
				Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true})
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
		}, parkedRootView("backlog")},
		{"a lingering root closed as done", func(t *testing.T, pool *pgxpool.Pool) {
			seedIssue(t, pool, lingeringRoot("done"))
		}, parkedRootView("done")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			tc.seed(t, pool)
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

			apply("approval", approvalOf(askPerson, tc.view))

			if writes := statusWrites(t, pool, "LEGION-208"); slices.ContainsFunc(writes, func(write record.StatusWrite) bool { return write.Kind == record.PersonHandoverStatusRow }) {
				t.Fatalf("status writes = %#v, want no person's handover for a root the approval does not queue", writes)
			}
			if gate := gateState(t, pool, "LEGION-208"); gate.ApprovedVersion == nil || *gate.ApprovedVersion != 2 {
				t.Fatalf("the root's gate = %#v, want it opened at version 2 as today", gate)
			}
			if kinds := noticeKinds(t, pool, "LEGION-208"); !containsNotice(kinds, "design-approved") {
				t.Fatalf("notices = %v, want design-approved as today", kinds)
			}
		})
	}
}

// A copy of the parked root's gate that a hand-over carried to a child of its tree, now a root of
// its own, belongs to a tree that is its own and is applied as today: the approval opens the copy
// carried closed and its architect is told carried-gate-opened. The parked owner is queued, and its
// own gate records the approval with no notice.
func TestACarriedCopyOfAParkedRootsGateTakesTheApprovalWhileTheOwnerIsQueued(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, lingeringRoot("backlog"))
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-209", Project: "LEGION",
		Title: "carried child", Parent: &parent, Phase: phase.Merging, Generation: 2, Status: "retro", Rank: "V", HandedOver: true})
	owner := record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2, ApprovedVersion: new(1)}
	seedGate(t, pool, owner)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-209", ArtifactID: "artifact-208", LatestVersion: 2, ApprovedVersion: new(1)})
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

	apply("approval", approvalOf(askPerson, parkedRootView("backlog")))

	if got, want := statusWrites(t, pool, "LEGION-208"), []record.StatusWrite{personHandoverWrite("backlog")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the owner's status writes = %#v, want %#v", got, want)
	}
	if writes := statusWrites(t, pool, "LEGION-209"); len(writes) != 0 {
		t.Fatalf("the carried root's status writes = %#v, want none", writes)
	}
	if got := gateState(t, pool, "LEGION-208"); got.ApprovedVersion == nil || *got.ApprovedVersion != 2 || got.LatestVersion != 2 {
		t.Fatalf("the owner's gate = %#v, want the person's approval of version 2 recorded", got)
	}
	if copied := gateState(t, pool, "LEGION-209"); copied.ApprovedVersion == nil || *copied.ApprovedVersion != 2 {
		t.Fatalf("the carried copy = %#v, want it opened at version 2", copied)
	}
	if kinds := noticeKinds(t, pool, "LEGION-208"); len(kinds) != 0 {
		t.Fatalf("the owner's notices = %v, want none", kinds)
	}
	if kinds := noticeKinds(t, pool, "LEGION-209"); !slices.Equal(kinds, []record.NoticeKind{"carried-gate-opened"}) {
		t.Fatalf("the carried root's notices = %v, want carried-gate-opened once", kinds)
	}
}

// One row of a person's handover at a time: while a marked row is pending for the root, a second
// approval — the stream's redelivery under another event id, or the person approving again — queues
// nothing, since the pending row carries the same write, at either status a parked root holds. The
// workflow's other status rows suppress nothing: a hand-over row, also a todo, pending for the root
// leaves the person's handover queued beside it.
func TestAPendingPersonsHandoverSuppressesASecondAndAHandOverRowDoesNot(t *testing.T) {
	for _, status := range []string{"backlog", "icebox"} {
		t.Run("a pending marked row at "+status, func(t *testing.T) {
			pool := migratedPool(t)
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

			apply("approval", approvalOf(askPerson, parkedRootView(status)))
			apply("approval-redelivered", approvalOf(askPerson, parkedRootView(status)))

			if got, want := statusWrites(t, pool, "LEGION-208"), []record.StatusWrite{personHandoverWrite(status)}; !reflect.DeepEqual(got, want) {
				t.Fatalf("status writes = %#v, want the one row %#v", got, want)
			}
		})
	}
	t.Run("a pending hand-over row", func(t *testing.T) {
		pool := migratedPool(t)
		handOver := record.HandOverStatusWrite("todo", "backlog", "LEGION-200", 3)
		row, err := record.NewOutboxRow("LEGION-208", handOver, time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			return record.NewStore().Enqueue(context.Background(), tx, row)
		}); err != nil {
			t.Fatalf("queue the hand-over row: %v", err)
		}
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))

		apply("approval", approvalOf(askPerson, parkedRootView("backlog")))

		if got, want := statusWrites(t, pool, "LEGION-208"), []record.StatusWrite{handOver, personHandoverWrite("backlog")}; !reflect.DeepEqual(got, want) {
			t.Fatalf("status writes = %#v, want the hand-over row and the person's handover, %#v", got, want)
		}
	})
}

// rootLogEvent is one event of LEGION-208's Dispatch event log as dispatch.Client.EventsAfter
// serves it: type typ at seq, by askPerson, with payload as its payload.
func rootLogEvent(seq int64, typ, payload string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":%d,"issue_key":"LEGION-208","seq":%d,"type":%q,"actor":{"kind":%q,"id":%q},"payload":%s}`,
		seq, seq, typ, askPerson.Kind, askPerson.ID, payload))
}

// rootIssueEvent is an issue event of LEGION-208's log, type typ at seq, that leaves the labelled
// root in status under title.
func rootIssueEvent(seq int64, typ, status, title string) json.RawMessage {
	return rootLogEvent(seq, typ, fmt.Sprintf(`{"key":"LEGION-208","status":%q,"title":%q,"labels":[%q]}`, status, title, dispatch.LegionLabel))
}

// rootApprovalEvent is the approval approvalOf builds, askPerson's approval of version 2 of
// artifact-208 at 9, as LEGION-208's log carries it.
func rootApprovalEvent() json.RawMessage {
	return rootLogEvent(9, "artifact.approved", `{"artifact_id":"artifact-208","version":2}`)
}

// preparedApprovalOf is askPerson's approval at 9 as the daemon's preparation builds it: view is
// what it read of the issue, and for an approval that needs the issue's event log
// (intake.DispatchArtifact.NeedsEventLog) it sets the first change of the root's status after the
// approval that log, LEGION-208's Dispatch event log, shows
// (intake.DispatchArtifact.WithStatusChange).
func preparedApprovalOf(t *testing.T, view *dispatch.Issue, log []json.RawMessage) intake.DispatchArtifact {
	t.Helper()
	fact := approvalOf(askPerson, view)
	if fact.NeedsEventLog() {
		prepared, err := fact.WithStatusChange(log)
		if err != nil {
			t.Fatalf("read the status changes of LEGION-208's log: %v", err)
		}
		fact = prepared
	}
	return fact
}

// LEGION-208's Dispatch event logs the approval at 9 is prepared from: the root created in todo
// and parked after the approval, at 10; and the root parked before the approval, at 8, and
// renamed after it, at 11, still in icebox.
var (
	parkedAfterTheApproval = []json.RawMessage{rootIssueEvent(1, "issue.created", "todo", "root"), rootApprovalEvent(),
		rootIssueEvent(10, "issue.updated", "icebox", "root")}
	renamedAfterTheApproval = []json.RawMessage{rootIssueEvent(1, "issue.created", "todo", "root"), rootIssueEvent(8, "issue.updated", "icebox", "root"),
		rootApprovalEvent(), rootIssueEvent(11, "issue.updated", "icebox", "the root, renamed")}
)

// A person's approval of a parked labelled root's spec, applied after the root's status changed
// - late, or replayed from a message the intake gave up on, however late - queues nothing.
// Dispatch's view the approval is prepared with is the issue as it is now, so it cannot say
// whether the park it shows came before the approval or after it; the root's Dispatch event log,
// which the preparation reads, says, and each approval here is built as the preparation builds it
// from the log the case states (preparedApprovalOf). The person approved the spec at sequence 9
// and moved the root to icebox at sequence 10, and the park reached the daemon first: the
// approval queues nothing, whether the daemon records the root or not. An event after the
// approval that changed no status, a rename after a park that came before it, leaves the root
// queued. The root's issue events are applied before the approval, as the daemon applies them,
// and they change nothing of the decision, which reads the log.
func TestALatePersonsApprovalQueuesNoRootAPersonParkedSince(t *testing.T) {
	event := func(seq int64, title string) intake.DispatchIssue {
		return intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: seq, Type: "issue.updated", Status: "icebox",
			Title: title, Rank: "U", HandedOver: true}
	}
	for _, tc := range []struct {
		name string
		// recorded seeds the root's record, nil for a root the daemon has no record of.
		recorded *record.Issue
		// events are the root's issue events the daemon applied before the approval, which is at 9.
		events []intake.DispatchIssue
		// log is the root's Dispatch event log the approval is prepared from.
		log []json.RawMessage
		// queued is whether the approval queues the root as the person's handover.
		queued bool
	}{
		{name: "an unrecorded root parked after the approval", events: []intake.DispatchIssue{event(10, "root")}, log: parkedAfterTheApproval},
		{name: "a recorded lingering root parked after the approval", recorded: new(lingeringRoot("backlog")), events: []intake.DispatchIssue{event(10, "root")},
			log: parkedAfterTheApproval},
		{name: "a root parked before the approval and renamed after it", events: []intake.DispatchIssue{event(8, "root"), event(11, "the root, renamed")},
			log: renamedAfterTheApproval, queued: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			if tc.recorded != nil {
				seedIssue(t, pool, *tc.recorded)
			}
			engine := testEngine(config.DesignGateRootIssues, nil)
			for _, fact := range tc.events {
				applyMirrored(t, pool, engine, fmt.Sprintf("event-%d", fact.Seq), fact)
			}

			applyMirrored(t, pool, engine, "late-approval", preparedApprovalOf(t, parkedRootView("icebox"), tc.log))

			queued := slices.Contains(statusWrites(t, pool, "LEGION-208"), personHandoverWrite("icebox"))
			if queued != tc.queued {
				t.Fatalf("the approval at sequence 9 queued the person's handover = %v, want %v; status writes %#v",
					queued, tc.queued, statusWrites(t, pool, "LEGION-208"))
			}
		})
	}
}

// A person's approval that a later change of its parked root's status keeps from queuing the root
// is no silent drop. Once the fact commits the daemon logs one warning naming the approval's
// sequence and the sequence of the Dispatch event that changed the root's status after it, which
// the preparation read from the root's event log, and the controller gets one parked-decision of
// the approval, whose reason says it came before a change of the root's status and that todo on
// the root brings it back. The root's own gate records the approval quietly, so it writes no
// design-approved notice that the outbox would relay as a second parked-decision. A root the
// daemon does not record has no tree to relay for, and is told by the warning alone. An approval
// no later status change refuses queues the handover and tells nobody, as before.
func TestARefusedPersonsHandoverIsWarnedAndRelayedToTheControllerOnce(t *testing.T) {
	event := func(seq int64, title string) intake.DispatchIssue {
		return intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: seq, Type: "issue.updated", Status: "icebox",
			Title: title, Rank: "U", HandedOver: true}
	}
	const warning = "workflow: a person's approval of a parked root is older than a change of the root's status; the root is not queued as their handover"
	relayed := record.ControllerNotice{Kind: record.ParkedDecisionNotice, Decision: record.DecisionDesignApproved, Root: "LEGION-208",
		Ref: "dispatch://LEGION-208/spec", Actor: askPerson.Name(), Version: 2}
	for _, tc := range []struct {
		name     string
		recorded bool
		// events are the root's issue events the daemon applied before the approval, which is at 9.
		events []intake.DispatchIssue
		// log is the root's Dispatch event log the approval is prepared from.
		log    []json.RawMessage
		queued bool
		// warned is the sequence of the status change the warning names, 0 for no warning.
		warned float64
		// told is every notice and controller notice the approval leaves, a relayed
		// parked-decision's reason aside.
		told []record.OutboxPayload
	}{
		{name: "a recorded root parked after the approval", recorded: true, events: []intake.DispatchIssue{event(10, "root")}, log: parkedAfterTheApproval,
			warned: 10, told: []record.OutboxPayload{relayed}},
		{name: "an unrecorded root parked after the approval", events: []intake.DispatchIssue{event(10, "root")}, log: parkedAfterTheApproval, warned: 10},
		{name: "a recorded root parked before the approval and renamed after it", recorded: true,
			events: []intake.DispatchIssue{event(8, "root"), event(11, "the root, renamed")}, log: renamedAfterTheApproval, queued: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			if tc.recorded {
				seedIssue(t, pool, lingeringRoot("icebox"))
				// Closed at the version approved, so applyArtifact would open it and write the
				// design-approved notice.
				seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
			}
			var logged bytes.Buffer
			engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewJSONHandler(&logged, nil)))
			for _, fact := range tc.events {
				applyMirrored(t, pool, engine, fmt.Sprintf("event-%d", fact.Seq), fact)
			}

			applyMirrored(t, pool, engine, "late-approval", preparedApprovalOf(t, parkedRootView("icebox"), tc.log))

			if queued := slices.Contains(statusWrites(t, pool, "LEGION-208"), personHandoverWrite("icebox")); queued != tc.queued {
				t.Fatalf("the approval at sequence 9 queued the person's handover = %v, want %v", queued, tc.queued)
			}
			var warnings []map[string]any
			for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
				var entry map[string]any
				if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == warning {
					warnings = append(warnings, entry)
				}
			}
			switch {
			case tc.warned == 0 && len(warnings) != 0:
				t.Fatalf("the approval logged the refusal %v, want no warning", warnings)
			case tc.warned != 0 && (len(warnings) != 1 || warnings[0]["level"] != "WARN" || warnings[0]["issue"] != "LEGION-208" ||
				warnings[0]["approval_seq"] != float64(9) || warnings[0]["status_change_seq"] != tc.warned):
				t.Fatalf("the refusal's warnings = %v, want one at WARN naming LEGION-208, the approval at 9 and the status change at %v; the engine logged:\n%s",
					warnings, tc.warned, logged.String())
			}
			told := noticeRows(t, pool)
			for i, payload := range told {
				notice, ok := payload.(record.ControllerNotice)
				if !ok || notice.Kind != record.ParkedDecisionNotice {
					continue
				}
				if !strings.Contains(notice.Reason, "came before a change of LEGION-208's status") || !strings.Contains(notice.Reason, "set LEGION-208 to todo") {
					t.Errorf("the parked-decision's reason %q does not say the approval came before a change of the root's status and that todo brings it back", notice.Reason)
				}
				notice.Reason = ""
				told[i] = notice
			}
			if !sameNotices(told, tc.told) {
				t.Fatalf("the notices the approval left = %#v, want %#v", told, tc.told)
			}
		})
	}
}

package admit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A root in triage is the controller's to triage once it is handed to Legion, so an observation of
// the unrecorded root that carries the label wakes the controller: its creation with the label, or
// the edit that adds it, since the dashboard creates an issue without labels. A root in triage
// without the label is someone else's and wakes nobody. The stream's redelivery of an event adds
// nothing, and the root is not recorded.
func TestAnUnrecordedRootInTriageWakesTheControllerWhenItCarriesTheLabel(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	apply(t, pool, admission, "created-unlabeled", intake.DispatchIssue{Key: "LEGION-300", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.created", Status: "triage", Title: "New", Rank: "A", HandedOver: false}, engineStub{})
	assertEffects(t, pool, nil)

	labeled := intake.DispatchIssue{Key: "LEGION-300", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "triage", Title: "New", Rank: "A", HandedOver: true}
	apply(t, pool, admission, "labeled", labeled, engineStub{})
	apply(t, pool, admission, "labeled", labeled, engineStub{})
	apply(t, pool, admission, "created-labeled", intake.DispatchIssue{Key: "LEGION-305", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.created", Status: "triage", Title: "Handed over at creation", Rank: "B", HandedOver: handed}, engineStub{})
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindControllerNotice, issue: "LEGION-300", payload: record.ControllerNotice{Kind: record.TriageNotice}},
		{kind: record.OutboxKindControllerNotice, issue: "LEGION-305", payload: record.ControllerNotice{Kind: record.TriageNotice}},
	})
	assertWaiting(t, pool, nil)
	if got := maybeIssue(t, pool, "LEGION-300"); got != nil {
		t.Fatalf("the root in triage was recorded: %#v", got)
	}
}

// A person's legion label on an unrecorded root in triage is the handover itself: the daemon
// queues the root's todo through its outbox, observed at triage so the delivery writes nothing once
// the status moved, and wakes nobody for it. While that row is pending a second observation of the
// same root — an edit of it, or the stream's redelivery under another event id — queues no second
// row and no wake either: the pending row carries the same write. The root is not recorded: the
// daemon's own todo, once Dispatch echoes it, records it as any labelled todo root. The queue is
// logged once, as the event's.
func TestAPersonsLabelOnAnUnrecordedRootInTriageIsQueuedByTheDaemon(t *testing.T) {
	pool := migratedPool(t)
	var logged bytes.Buffer
	admission := newAdmission(t, 1, slog.New(slog.NewJSONHandler(&logged, nil)))

	labelled := intake.DispatchIssue{Key: "LEGION-306", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "triage", Title: "Handed over by a person", Rank: "A", HandedOver: handed, LabelSetter: "user"}
	apply(t, pool, admission, "labelled-by-a-person", labelled, engineStub{})
	queued := []effect{
		{kind: record.OutboxKindDispatchStatus, issue: "LEGION-306", payload: record.PersonHandoverStatusWrite("todo", "triage")},
	}
	assertEffects(t, pool, queued)

	retitled := labelled
	retitled.Seq, retitled.Title = 3, "Retitled while the row is pending"
	apply(t, pool, admission, "retitled-by-a-person", retitled, engineStub{})
	apply(t, pool, admission, "redelivered-under-another-id", labelled, engineStub{})
	assertEffects(t, pool, queued)
	assertWaiting(t, pool, nil)
	if got := maybeIssue(t, pool, "LEGION-306"); got != nil {
		t.Fatalf("the root in triage was recorded: %#v", got)
	}
	if lines := personHandoverLines(t, &logged, "LEGION-306", "event"); lines != 1 {
		t.Fatalf("queued-a-person's-handover lines with source event = %d, want one; admission logged:\n%s", lines, logged.String())
	}
}

// personHandoverLines counts the lines of admission's JSON log that say it queued a person's
// handover of issue from source.
func personHandoverLines(t *testing.T, logged *bytes.Buffer, issue, source string) int {
	t.Helper()
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "admission: queued a person's handover" &&
			entry["issue"] == issue && entry["source"] == source {
			lines++
		}
	}
	return lines
}

// A child created in triage is its parent's architect's, and a recorded root set back to triage is
// a human's move on work the daemon already holds: neither wakes the controller for triage, and a
// person's label on either queues nothing — a child runs in its tree, and a recorded root a person
// moved back to triage is never queued again.
func TestNoTriageWakeForAChildOrARecordedRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
	}{
		{"a child created in triage", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			apply(t, pool, admission, "child", intake.DispatchIssue{Key: "LEGION-301", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.created", Status: "triage", Title: "Child", Parent: "LEGION-300", Rank: "A", HandedOver: handed}, engineStub{})
		}},
		{"a recorded root set back to triage", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			seedWaiting(t, pool, "LEGION-302", "A")
			apply(t, pool, admission, "back", intake.DispatchIssue{Key: "LEGION-302", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "triage", Title: "waiting", Rank: "A", HandedOver: handed}, engineStub{})
		}},
		{"a child created in triage with a person's label", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			apply(t, pool, admission, "child", intake.DispatchIssue{Key: "LEGION-301", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.created", Status: "triage", Title: "Child", Parent: "LEGION-300", Rank: "A", HandedOver: handed, LabelSetter: "user"}, engineStub{})
		}},
		{"a recorded root a person set back to triage", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			seedWaiting(t, pool, "LEGION-302", "A")
			apply(t, pool, admission, "back", intake.DispatchIssue{Key: "LEGION-302", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "triage", Title: "waiting", Rank: "A", HandedOver: handed, LabelSetter: "user"}, engineStub{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			tc.run(t, pool, newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil))))
			assertEffects(t, pool, nil)
		})
	}
}

// The stream can redeliver a NAK'd issue.created after a later event, under another event id,
// recorded the root: by then the root is the daemon's, so the late creation wakes nobody.
func TestALateCreationOfARecordedRootWakesNobody(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	apply(t, pool, admission, "todo", intake.DispatchIssue{Key: "LEGION-304", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: "New", Rank: "A", HandedOver: handed}, engineStub{})
	before := effects(t, pool)

	apply(t, pool, admission, "created", intake.DispatchIssue{Key: "LEGION-304", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.created", Status: "triage", Title: "New", Rank: "A", HandedOver: handed}, engineStub{})
	if got := effects(t, pool); !reflect.DeepEqual(got, before) {
		t.Fatalf("outbox effects after the late creation = %#v, want those before it, %#v", got, before)
	}
}

// Legion admits only an issue handed to it. A root or an orphan in todo without the legion label is
// someone else's work in a project Legion shares: it is not recorded, takes no slot, and nothing is
// written or started for it. The label, in any case and among any others, hands it over.
func TestOnlyAnIssueCarryingTheLegionLabelIsAdmitted(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))

	apply(t, pool, admission, "root-unlabeled", intake.DispatchIssue{Key: "LEGION-1", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "someone else's", Rank: "A", HandedOver: false}, engineStub{})
	apply(t, pool, admission, "orphan-unlabeled", intake.DispatchIssue{Key: "LEGION-2", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "a human's child", Parent: "LEGION-MISSING", Rank: "B"}, engineStub{})
	for _, key := range []string{"LEGION-1", "LEGION-2"} {
		if got := maybeIssue(t, pool, key); got != nil {
			t.Fatalf("unlabeled %s was recorded: %#v", key, got)
		}
	}
	assertSlots(t, pool, nil)
	assertEffects(t, pool, nil)

	apply(t, pool, admission, "root-labeled", intake.DispatchIssue{Key: "LEGION-1", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: "handed over", Rank: "A", HandedOver: true}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-1", Index: 0, AdmittedAt: fixedNow}})
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindDispatchStatus, issue: "LEGION-1", payload: record.OrdinaryStatusWrite("in_progress", "todo", "")},
		{kind: record.OutboxKindIssueBranch, issue: "LEGION-1", payload: record.IssueBranch{Generation: 1}},
		{kind: record.OutboxKindSupervise, issue: "LEGION-1", payload: record.SuperviseRequest{Op: "start", Tree: "LEGION-1", Role: claim.RoleArchitect, Generation: 1,
			Task: architectStart(t, pool, admission, "LEGION-1", ""), Entry: 1, Decided: true}},
	})
}

// Among the roots handed to Legion the waiting line is Dispatch rank order, as it was; an unlabeled
// root ranked above them all has no place in it and takes no slot when one frees.
func TestTheWaitingLineRanksOnlyLabeledRootsInDispatchOrder(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")

	for _, event := range []intake.DispatchIssue{
		{Key: "LEGION-TOP", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "ranked first, not handed over", Rank: "0"},
		{Key: "LEGION-C", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank C", Rank: "C", HandedOver: handed},
		{Key: "LEGION-B", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "rank B", Rank: "B", HandedOver: handed},
	} {
		apply(t, pool, admission, "arrive-"+event.Key, event, engineStub{})
	}
	assertWaiting(t, pool, []string{"LEGION-B", "LEGION-C"})

	apply(t, pool, admission, "active-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "done", Title: "active", Rank: "A", HandedOver: handed}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-B", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, []string{"LEGION-C"})
}

// A child is its tree's: under a root handed to Legion, a child needs no label of its own. It joins
// the root's tree and takes no slot, as every child does.
func TestAnUnlabeledChildOfAnAdmittedRootRunsInItsTree(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Clock: func() time.Time { return fixedNow }}, nil)

	apply(t, pool, admission, "root", intake.DispatchIssue{Key: "LEGION-1", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "root", Rank: "A", HandedOver: handed}, engine)
	apply(t, pool, admission, "child", intake.DispatchIssue{Key: "LEGION-2", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo", Title: "child", Parent: "LEGION-1", Rank: "B"}, engine)
	if got := issue(t, pool, "LEGION-2"); got.Tree != "LEGION-1" {
		t.Fatalf("unlabeled child = %#v, want it in LEGION-1's tree", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-1", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, nil)
}

// Taking the label off a waiting root drops it from the line, as moving it out of todo does, and
// putting the label back queues it again. Taking it off a root already running changes nothing: the
// label hands work to Legion, and taking it off gives back no slot of a tree it is running.
func TestTakingTheLabelOffAWaitingRootDequeuesItButStopsNoRunningTree(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	registerController(t, pool)

	apply(t, pool, admission, "next-unlabeled", intake.DispatchIssue{Key: "LEGION-NEXT", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: "next", Rank: "B"}, engineStub{})
	assertWaiting(t, pool, nil)
	apply(t, pool, admission, "active-unlabeled", intake.DispatchIssue{Key: "LEGION-ACTIVE", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "in_progress", Title: "active", Rank: "A"}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ACTIVE", Index: 0, AdmittedAt: fixedNow}})

	// The only waiting root lost its label, so the finished tree's slot stays free: nothing starts,
	// and the controller is woken about the free slot.
	apply(t, pool, admission, "active-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "done", Title: "active", Rank: "A"}, engineStub{})
	assertSlots(t, pool, nil)
	assertEffects(t, pool, []effect{
		{kind: record.OutboxKindControllerNotice, issue: "LEGION-ACTIVE", payload: record.ControllerNotice{Kind: record.SlotFreeNotice}},
	})

	apply(t, pool, admission, "next-labeled", intake.DispatchIssue{Key: "LEGION-NEXT", ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo", Title: "next", Rank: "B", HandedOver: handed}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
}

// The boot read applies the same rule as the stream: an unlabeled todo root is not recorded, a
// labeled one is admitted, and a waiting root whose label was taken off while the daemon was down
// leaves the line.
func TestReconcileAdmitsOnlyLabeledRoots(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedWaiting(t, pool, "LEGION-GONE", "0")

	reconcile(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-GONE", ResolvedRepo: "acme/widgets", Title: "LEGION-GONE", Status: "todo", Rank: "0"},
		{Key: "LEGION-OTHER", ResolvedRepo: "acme/widgets", Title: "someone else's", Status: "todo", Rank: "A", HandedOver: false},
		{Key: "LEGION-MINE", ResolvedRepo: "acme/widgets", Title: "handed over", Status: "todo", Rank: "B", HandedOver: true},
	})
	if got := maybeIssue(t, pool, "LEGION-OTHER"); got != nil {
		t.Fatalf("unlabeled LEGION-OTHER was recorded: %#v", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-MINE", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, nil)
}

// The boot listing queues a person's handover as the stream would have: a root the listing shows
// in triage with the label, whose setter the listing's preparation read from the log as a person,
// gets one todo row observed at triage, logged as the listing's, and nothing is recorded. One whose
// label a session set gets nothing at boot — no row and no wake, as before: the controller's own
// start triages it. A second boot while the row is still pending — the delivery not yet made —
// queues no second row and logs no second line.
func TestReconcileQueuesAPersonsLabelOnAnUnrecordedRootInTriage(t *testing.T) {
	pool := migratedPool(t)
	var logged bytes.Buffer
	admission := newAdmission(t, 1, slog.New(slog.NewJSONHandler(&logged, nil)))
	boot := func() {
		listed := []intake.DispatchIssue{
			intake.ListedIssue(dispatch.IssueSummary{Key: "LEGION-310", ResolvedRepo: "acme/widgets", Title: "labelled by a person while the daemon was down", Status: "triage", Rank: "A", HandedOver: true, LastSeq: 2}),
			intake.ListedIssue(dispatch.IssueSummary{Key: "LEGION-311", ResolvedRepo: "acme/widgets", Title: "labelled by a session", Status: "triage", Rank: "B", HandedOver: true, LastSeq: 2}),
		}
		listed[0].LabelSetter, listed[1].LabelSetter = "user", "session"
		// idle true: caught up whatever the target, so every summary applies now, as the listing of
		// a daemon booted after the person's label — its consumer positioned past that event — does.
		if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
			ctx, settle := intake.OpenSettlers(ctx, admission)
			if _, err := admission.Reconcile(ctx, tx, listed, 2, intake.DispatchConsumerPosition{AckFloorStream: 2, Idle: true}); err != nil {
				return err
			}
			return settle(ctx, tx)
		}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	boot()
	queued := []effect{
		{kind: record.OutboxKindDispatchStatus, issue: "LEGION-310", payload: record.PersonHandoverStatusWrite("todo", "triage")},
	}
	assertEffects(t, pool, queued)
	assertWaiting(t, pool, nil)
	for _, key := range []string{"LEGION-310", "LEGION-311"} {
		if got := maybeIssue(t, pool, key); got != nil {
			t.Fatalf("the listed root in triage %s was recorded: %#v", key, got)
		}
	}

	boot()
	assertEffects(t, pool, queued)
	if lines := personHandoverLines(t, &logged, "LEGION-310", "listing"); lines != 1 {
		t.Fatalf("queued-a-person's-handover lines with source listing = %d, want one across two boots; admission logged:\n%s", lines, logged.String())
	}
	if lines := personHandoverLines(t, &logged, "LEGION-311", "listing"); lines != 0 {
		t.Fatalf("a session's label was logged as a person's handover; admission logged:\n%s", logged.String())
	}
}

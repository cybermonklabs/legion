package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A person's changes request whose notice waits in the outbox is published only while it is
// still the person's word (dispatch://INFRA-3136/spec R2). The workflow asks when it queues the
// notice (workflow's tellChanges), and the delivery asks again of the gates as they stand when it
// runs (readNoticeTree's decisionSuperseded): the row's first attempt finds no session holding the
// architect's role, as while its pod is released or relaunching, and waits; the person's approval
// lands; the next attempt runs. The review and the approval reach the engine as Dispatch's
// artifact events do, and a gate's move to another document as register_gate's registration does,
// so the rows are the ones the workflow writes.
//
// Where the person approved the reviewed version after the request, the request's row finishes
// with nothing published and one line naming the row, the issue and both versions, and the
// approval's own notice behind it still goes. The controls are a request no approval followed,
// one that came after the person's approval, and one on a document the step's gate has since left
// for another, which the person approved at the version reviewed: each is published, since only
// an approval of the document reviewed can answer the review.
func TestAQueuedChangesNoticeAnApprovalFollowedIsNotPublished(t *testing.T) {
	reviewed := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	changes := intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "artifact-1", Kind: intake.DispatchArtifactChangesRequested, Version: 3,
		Reason: "name the rollback path", Actor: alice, Mark: record.EventMark{Seq: 8, At: reviewed}}
	approval := func(seq int64, at time.Time) intake.DispatchArtifact {
		return intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "artifact-1", Kind: intake.DispatchArtifactApproved, Version: 3, Actor: alice,
			Mark: record.EventMark{Seq: seq, At: at}}
	}
	approved := approval(9, reviewed.Add(5*time.Minute))
	earlier := approval(7, reviewed.Add(-5*time.Minute))
	moved := movedGate(reviewed.Add(5 * time.Minute))
	for _, tc := range []struct {
		name string
		// queued are the facts applied before the first attempt, and waited those applied while
		// the row waits for the architect. published is every notice the architect is then sent.
		queued, waited []intake.Fact
		published      []record.NoticeKind
		superseded     bool
	}{
		{name: "the person approved the reviewed version after the request",
			queued: []intake.Fact{changes}, waited: []intake.Fact{approved},
			published: []record.NoticeKind{"design-approved"}, superseded: true},
		{name: "no approval followed the request",
			queued:    []intake.Fact{changes},
			published: []record.NoticeKind{"design-changes-requested"}},
		{name: "the request came after the person's approval",
			queued:    []intake.Fact{earlier, changes},
			published: []record.NoticeKind{"design-approved", "design-changes-requested"}},
		{name: "the gate moved to another document the person approved at the version reviewed",
			queued: []intake.Fact{changes}, waited: []intake.Fact{moved},
			published: []record.NoticeKind{"design-changes-requested", "design-approved"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				return records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-1", Step: record.DesignStepSpec, ArtifactID: "artifact-1", LatestVersion: 3})
			}); err != nil {
				t.Fatalf("register the spec gate at version 3: %v", err)
			}
			steps := config.DesignGateRootIssues.Steps()
			engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: steps}, quietLogger())
			applyFacts(t, pool, engine, tc.queued)
			request := soleNoticeRow(t, pool, "design-changes-requested")
			wantQueued := record.Notice{Kind: "design-changes-requested", Version: 3,
				Reason: changes.Reason, Actor: "user:alice", Ref: "dispatch://LEGION-1/spec",
				Review: changes.Mark, Artifact: "artifact-1"}
			if !reflect.DeepEqual(request.notice, wantQueued) {
				t.Fatalf("the workflow queued %#v, want the changes request %#v",
					request.notice, wantQueued)
			}

			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
			publisher := &holderPublisher{}
			publisher.setAbsent(architectTopic(t, "LEGION-1"))
			var logged bytes.Buffer
			clock := time.Now()
			runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records,
				notices: publisher, supervisor: sup, project: "legion", gatedSteps: steps, now: func() time.Time { return clock }}
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("first attempt: %v", err)
			}
			if published := architectNotices(t, publisher); len(published) != 0 || soleNoticeRow(t, pool, "design-changes-requested").id != request.id {
				t.Fatalf("published %v to the architect on the attempt no session holds its role; want the request's row %d waiting", published, request.id)
			}

			applyFacts(t, pool, engine, tc.waited)
			publisher.setAbsent()
			for run := 0; run < 5 && outboxRowsOfKind(t, pool, "LEGION-1", record.OutboxKindNotice) > 0; run++ {
				clock = clock.Add(time.Hour)
				if err := runner.RunOnce(ctx); err != nil {
					t.Fatalf("attempt after the wait %d: %v", run, err)
				}
			}
			if left := outboxRowsOfKind(t, pool, "LEGION-1", record.OutboxKindNotice); left != 0 {
				t.Fatalf("%d notice rows of LEGION-1 left, want every row finished; log %s", left, logged.String())
			}
			if published := architectNotices(t, publisher); !reflect.DeepEqual(published, tc.published) {
				t.Fatalf("published %v to the architect, want %v", published, tc.published)
			}
			dropped := textLines(logged.String(), `msg="outbox changes request finished without publishing: the person's approval followed it"`)
			if !tc.superseded {
				if len(dropped) != 0 {
					t.Fatalf("logged %q, want no changes request finished without publishing", dropped)
				}
				return
			}
			want := fmt.Sprintf("row=%d issue=LEGION-1 version=3 approved=3", request.id)
			if len(dropped) != 1 || !strings.Contains(dropped[0], want) || !strings.Contains(dropped[0], "level=INFO") {
				t.Fatalf("logged %q, want one Info line naming %s", dropped, want)
			}
		})
	}
}

// movedGate is the registration that moves LEGION-1's spec step from the document the tests'
// changes request is of (artifact-1) to another (artifact-2), whose version 3 register_gate's
// read found the person approved at approved: the version the request reviewed, on a document it
// did not review. The step's reference stays dispatch://LEGION-1/spec through the move.
func movedGate(approved time.Time) intake.GateRegistered {
	return intake.GateRegistered{Issue: "LEGION-1", ArtifactID: "artifact-2", Version: 3,
		DispatchVersion: 3, ApprovedVersion: 3,
		ApprovedMark: record.EventMark{Seq: 9, At: approved}, ReviewedBy: alice}
}

// applyFacts applies each fact to engine as its source does: a registration as register_gate's
// call, every other fact as Dispatch's event, each under an id of its own.
func applyFacts(t *testing.T, pool *store.Pool, engine *workflow.Engine, facts []intake.Fact) {
	t.Helper()
	for _, fact := range facts {
		var source, id string
		switch fact := fact.(type) {
		case intake.DispatchArtifact:
			source, id = "dispatch", fmt.Sprintf("artifact-%s-%d", fact.Kind, fact.Mark.Seq)
		case intake.GateRegistered:
			source, id = "api", "register-"+fact.ArtifactID
		default:
			t.Fatalf("no source of the tests applies a %T", fact)
		}
		result, err := intake.ApplyFact(context.Background(), pool, source, id, fact, engine)
		if err != nil || result.Refusal != nil {
			t.Fatalf("apply %s = %#v, %v", id, result, err)
		}
	}
}

// architectNotices is the kind of every notice published to LEGION-1's architect, in order. The
// gate's opening also sends the tree's roles, the architect among them, a role message of its own
// (whose payload is its text), which is no notice and not this test's.
func architectNotices(t *testing.T, publisher *holderPublisher) []record.NoticeKind {
	t.Helper()
	_, delivered := publisher.snapshot()
	var kinds []record.NoticeKind
	for _, published := range delivered {
		if notice, ok := published.payload.(record.Notice); ok && published.topic == architectTopic(t, "LEGION-1") {
			kinds = append(kinds, notice.Kind)
		}
	}
	return kinds
}

// queuedRow is a notice row the outbox holds: its id and the notice it carries.
type queuedRow struct {
	id     int64
	notice record.Notice
}

// soleNoticeRow is the one notice row of kind the outbox holds.
func soleNoticeRow(t *testing.T, pool *store.Pool, kind record.NoticeKind) queuedRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, kind, issue, payload from outbox
		where kind = $1 and payload->>'kind' = $2 order by id`, string(record.OutboxKindNotice), string(kind))
	if err != nil {
		t.Fatalf("read the %s notice rows: %v", kind, err)
	}
	queued, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var out record.OutboxRow
		return out, row.Scan(&out.ID, &out.Kind, &out.Issue, &out.Payload)
	})
	if err != nil {
		t.Fatalf("scan the %s notice rows: %v", kind, err)
	}
	if len(queued) != 1 {
		t.Fatalf("%d %s notice rows queued, want one", len(queued), kind)
	}
	payload, err := record.DecodeOutboxPayload(queued[0])
	if err != nil {
		t.Fatalf("decode notice row %d: %v", queued[0].ID, err)
	}
	notice, ok := payload.(record.Notice)
	if !ok {
		t.Fatalf("notice row %d carries %T, want a record.Notice", queued[0].ID, payload)
	}
	return queuedRow{id: queued[0].ID, notice: notice}
}

// A person's changes request relayed to the controller, because nobody in its tree hears it
// (parkDecision), is told only while it is still the person's word (dispatch://INFRA-3136/spec
// R2). The relay asks when it writes the controller's row, and the publish asks again
// (controllerNotice), of the decision the row carries: the version, Dispatch's mark of the review
// and the document reviewed, which the relay copied from the request's own notice. The row's
// first attempt finds no session holding the controller's claim, and waits; the person's approval
// of the version reviewed lands; the holder returns and the next attempt runs. The review and the
// approval reach the engine as Dispatch's artifact events do, and a gate's move to another
// document as register_gate's registration does, on a running tree whose architect's claim
// failed, so every row is one the workflow and the relay write.
//
// Where the approval followed the request, the request's row finishes with nothing published and
// one line naming the row, the issue, the root, the ref and both versions, and the approval's own
// relay still goes. That holds whether the request's row asks before the approval's own notice is
// relayed or after, when the decision kept on the document's reference is the approval in the
// request's place (record.Store.AddParkedDecision): what is kept beside the row decides nothing.
// The controls are a request no approval followed, one Dispatch recorded after the person's
// approval of the same version, and one on a document the step's gate has since left for another,
// which the person approved at the version reviewed: each is told, since the review's mark and
// its document, not the version alone, decide whether the approval answered it.
func TestAChangesRequestRelayedToTheControllerIsNotToldOnceApproved(t *testing.T) {
	reviewed := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	changes := intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "artifact-1", Kind: intake.DispatchArtifactChangesRequested, Version: 3,
		Reason: "name the rollback path", Actor: alice, Mark: record.EventMark{Seq: 8, At: reviewed}}
	approval := func(seq int64, at time.Time) intake.DispatchArtifact {
		return intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "artifact-1", Kind: intake.DispatchArtifactApproved, Version: 3, Actor: alice,
			Mark: record.EventMark{Seq: seq, At: at}}
	}
	approved := approval(9, reviewed.Add(5*time.Minute))
	earlier := approval(7, reviewed.Add(-5*time.Minute))
	moved := movedGate(reviewed.Add(5 * time.Minute))
	changesKind := record.DecisionDesignChangesRequested
	approvalKind := record.DecisionDesignApproved
	for _, tc := range []struct {
		name string
		// queued are the facts applied before the relay's first attempt, and waited those
		// applied while its row waits for the controller. told is every decision the controller
		// is then told, in order. keptFirst has the approval's own notice relayed, and kept in
		// the request's place, before the request's row runs again.
		queued, waited []intake.Fact
		told           []record.DecisionKind
		superseded     bool
		keptFirst      bool
	}{
		{name: "the person approved the reviewed version after the request",
			queued: []intake.Fact{changes}, waited: []intake.Fact{approved},
			told: []record.DecisionKind{approvalKind}, superseded: true},
		{name: "the approval was kept in the request's place before the request's row ran again",
			queued: []intake.Fact{changes}, waited: []intake.Fact{approved},
			told: []record.DecisionKind{approvalKind}, superseded: true, keptFirst: true},
		{name: "no approval followed the request",
			queued: []intake.Fact{changes},
			told:   []record.DecisionKind{changesKind}},
		{name: "the request came after the person's approval",
			queued: []intake.Fact{earlier, changes},
			told:   []record.DecisionKind{approvalKind, changesKind}},
		{name: "the gate moved to another document the person approved at the version reviewed",
			queued: []intake.Fact{changes}, waited: []intake.Fact{moved},
			told: []record.DecisionKind{changesKind, approvalKind}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				return records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-1", Step: record.DesignStepSpec, ArtifactID: "artifact-1", LatestVersion: 3})
			}); err != nil {
				t.Fatalf("register the spec gate at version 3: %v", err)
			}
			// One clock for the engine and the outbox: the rows the engine queues come due by it,
			// and the waiting row's backoff counts from it. The facts applied while the row waits
			// land a minute after it waited, so that row is due before the notice they queue, and
			// asks the gate while the decision kept for it is still the request. Where the
			// approval is kept first, they land at the moment the row waited, so their notice is
			// due while the row's backoff still holds it.
			clock := reviewed.Add(time.Hour)
			now := func() time.Time { return clock }
			steps := config.DesignGateRootIssues.Steps()
			engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: steps, Clock: now}, quietLogger())
			applyFacts(t, pool, engine, tc.queued)

			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", supervise.StateFailed)
			publisher := &holderPublisher{}
			publisher.setAbsent(architectTopic(t, "LEGION-1"), notify.ControllerTopic("legion"))
			var logged bytes.Buffer
			runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records,
				notices: publisher, supervisor: sup, project: "legion", gatedSteps: steps, now: now}
			// The first run relays each decision, whose notice finds its architect's claim failed;
			// the second is each relay's first attempt, which waits for the controller's claim.
			for run := range 2 {
				if err := runner.RunOnce(ctx); err != nil {
					t.Fatalf("run %d before the wait: %v", run, err)
				}
			}
			request, attempts := relayRow(t, pool, record.DecisionDesignChangesRequested)
			if told := controllerDecisions(publisher); len(told) != 0 || attempts == 0 {
				t.Fatalf("told the controller %v, and the request's row %d made %d attempts; want nothing told and the row waiting",
					told, request, attempts)
			}

			if !tc.keptFirst {
				clock = clock.Add(time.Minute)
			}
			applyFacts(t, pool, engine, tc.waited)
			if tc.keptFirst {
				if err := runner.RunOnce(ctx); err != nil {
					t.Fatalf("the run that relays the approval: %v", err)
				}
				kept := keptDecisions(t, pool, records, "LEGION-1")
				if len(kept) != 1 || kept[0].Decision != record.DecisionDesignApproved {
					t.Fatalf("kept %#v, want the person's approval in the request's place", kept)
				}
				_, again := relayRow(t, pool, record.DecisionDesignChangesRequested)
				if again != attempts {
					t.Fatalf("the request's row made %d attempts, want %d: it ran again first",
						again, attempts)
				}
			}
			publisher.setAbsent(architectTopic(t, "LEGION-1"))
			left := func() int {
				return outboxRowsOfKind(t, pool, "LEGION-1", record.OutboxKindNotice) + outboxRowsOfKind(t, pool, "LEGION-1", record.OutboxKindControllerNotice)
			}
			for run := 0; run < 5 && left() > 0; run++ {
				clock = clock.Add(time.Hour)
				if err := runner.RunOnce(ctx); err != nil {
					t.Fatalf("run %d after the wait: %v", run, err)
				}
			}
			if rows := left(); rows != 0 {
				t.Fatalf("%d notice and controller rows of LEGION-1 left, want every row finished; log %s", rows, logged.String())
			}
			if told := controllerDecisions(publisher); !slices.Equal(told, tc.told) {
				t.Fatalf("told the controller %v, want %v; log %s", told, tc.told, logged.String())
			}
			dropped := textLines(logged.String(), `msg="outbox parked changes request finished without publishing`)
			if !tc.superseded {
				if len(dropped) != 0 {
					t.Fatalf("logged %q, want no relayed changes request finished without publishing", dropped)
				}
				return
			}
			want := fmt.Sprintf("row=%d issue=LEGION-1 root=LEGION-1 ref=dispatch://LEGION-1/spec version=3 approved=3", request)
			if len(dropped) != 1 || !strings.Contains(dropped[0], `the person's approval followed it"`) || !strings.Contains(dropped[0], want) ||
				!strings.Contains(dropped[0], "level=INFO") {
				t.Fatalf("logged %q, want one Info line that the approval followed the request, naming %s", dropped, want)
			}
		})
	}
}

// A person's changes request the outbox still holds when a clear of its generation runs is
// finished by that clear, unpublished, once the person's approval followed it
// (dispatch://INFRA-3136/spec R2): the clear deletes the issue's gates, the only record of that
// approval, so the request's next attempt, asked of no gate, would read as the person's word
// again. The request waits for its tree's architect, whose role no session holds, or, relayed
// because that architect's claim failed, for the controller, whose claim no session holds; the
// person's approval of the version reviewed lands; the generation is cleared, of the tree as a
// re-admission clears it (workflow.Engine.ClearTreeGeneration) or of the issue alone
// (workflow.Engine.ClearGeneration); the holder returns and the next attempts run. The request
// is told to nobody, the clear writes one Info line naming the row, the issue, the root, the ref
// and both versions, and the approval's own notice still goes. A runner that held the relay's
// lease through the clear finishes it unpublished too, since it reads its lease in the snapshot
// it asks the gates in. The controls are requests no approval followed, which the clear leaves
// and their holder is told.
func TestAQueuedChangesRequestAClearFindsApprovedIsToldToNobody(t *testing.T) {
	reviewed := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	changes := intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "artifact-1",
		Kind: intake.DispatchArtifactChangesRequested, Version: 3, Reason: "name the rollback path",
		Actor: alice, Mark: record.EventMark{Seq: 8, At: reviewed}}
	approved := intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "artifact-1",
		Kind: intake.DispatchArtifactApproved, Version: 3, Actor: alice,
		Mark: record.EventMark{Seq: 9, At: reviewed.Add(5 * time.Minute)}}
	clearTree := func(ctx context.Context, tx pgx.Tx, engine *workflow.Engine, at time.Time) error {
		return engine.ClearTreeGeneration(ctx, tx, "LEGION-1", at)
	}
	clearIssue := func(ctx context.Context, tx pgx.Tx, engine *workflow.Engine, at time.Time) error {
		issue, err := record.NewStore().Issue(ctx, tx, "LEGION-1")
		if err != nil {
			return err
		}
		return engine.ClearGeneration(ctx, tx, *issue, at)
	}
	for _, tc := range []struct {
		name string
		// relayed has the request wait relayed to the controller rather than for the architect,
		// approved has the person approve it before the clear, and leased has a runner hold the
		// relay's lease through the clear and then run it.
		relayed, approved, leased bool
		clear                     func(context.Context, pgx.Tx, *workflow.Engine, time.Time) error
	}{
		{name: "for the architect, approved, then the tree's clear", approved: true, clear: clearTree},
		{name: "for the architect, approved, then the issue's clear", approved: true, clear: clearIssue},
		{name: "for the architect, not approved, then the tree's clear", clear: clearTree},
		{name: "relayed, approved, then the tree's clear", relayed: true, approved: true,
			clear: clearTree},
		{name: "relayed, approved, then the issue's clear", relayed: true, approved: true,
			clear: clearIssue},
		{name: "relayed, approved, then the tree's clear under a runner's lease", relayed: true,
			approved: true, leased: true, clear: clearTree},
		{name: "relayed, not approved, then the tree's clear", relayed: true, clear: clearTree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				return records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-1",
					Step: record.DesignStepSpec, ArtifactID: "artifact-1", LatestVersion: 3})
			}); err != nil {
				t.Fatalf("register the spec gate at version 3: %v", err)
			}
			clock := reviewed.Add(time.Hour)
			now := func() time.Time { return clock }
			steps := config.DesignGateRootIssues.Steps()
			var cleared bytes.Buffer
			engine := workflow.New(records, workflow.Config{Project: "LEGION",
				Repositories: testRepositories, Steps: steps, Clock: now},
				slog.New(slog.NewTextHandler(&cleared, nil)))
			applyFacts(t, pool, engine, []intake.Fact{changes})

			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architect := supervise.StateWorking
			if tc.relayed {
				architect = supervise.StateFailed
			}
			architectClaim(t, sup, "LEGION-1", architect)
			publisher := &holderPublisher{}
			publisher.setAbsent(architectTopic(t, "LEGION-1"), notify.ControllerTopic("legion"))
			var logged bytes.Buffer
			runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool,
				dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
				project: "legion", gatedSteps: steps, now: now}
			// The first run is the request's first attempt, which waits for the architect or
			// relays the request; the second is the relay's first attempt, which waits for the
			// controller.
			for run := range 2 {
				if err := runner.RunOnce(ctx); err != nil {
					t.Fatalf("run %d before the clear: %v", run, err)
				}
			}
			request := queuedRow{}
			if tc.relayed {
				request.id, _ = relayRow(t, pool, record.DecisionDesignChangesRequested)
			} else {
				request = soleNoticeRow(t, pool, "design-changes-requested")
			}
			if told := len(architectNotices(t, publisher)) + len(controllerDecisions(publisher)); told != 0 {
				t.Fatalf("told %d notices before the clear, want the request's row %d waiting",
					told, request.id)
			}

			clock = clock.Add(time.Minute)
			if tc.approved {
				applyFacts(t, pool, engine, []intake.Fact{approved})
			}
			var held record.OutboxRow
			if tc.leased {
				if err := pool.QueryRow(ctx, "select id, kind, issue, payload from outbox where id = $1",
					request.id).Scan(&held.ID, &held.Kind, &held.Issue, &held.Payload); err != nil {
					t.Fatalf("read the relay's row %d: %v", request.id, err)
				}
				held.LeaseToken = leaseOutboxRow(t, pool, request.id)
			}
			scoped, committed := intake.WithCommitHooks(ctx)
			if err := pgx.BeginFunc(scoped, pool, func(tx pgx.Tx) error {
				return tc.clear(scoped, tx, engine, clock)
			}); err != nil {
				t.Fatalf("clear the generation: %v", err)
			}
			committed()
			if tc.leased {
				if err := runner.run(ctx, held); err != nil {
					t.Fatalf("the leased run of the relay's row %d: %v", held.ID, err)
				}
				if told := controllerDecisions(publisher); len(told) != 0 {
					t.Fatalf("the leased run told the controller %v, want nothing", told)
				}
				gone := textLines(logged.String(), `msg="outbox parked changes request finished `+
					`without publishing: its row was deleted under its lease"`)
				if want := fmt.Sprintf("row=%d issue=LEGION-1", held.ID); len(gone) != 1 ||
					!strings.Contains(gone[0], want) {
					t.Fatalf("logged %q, want one line naming %s", gone, want)
				}
			}

			if tc.relayed {
				publisher.setAbsent(architectTopic(t, "LEGION-1"))
			} else {
				publisher.setAbsent()
			}
			left := func() int {
				return outboxRowsOfKind(t, pool, "LEGION-1", record.OutboxKindNotice) +
					outboxRowsOfKind(t, pool, "LEGION-1", record.OutboxKindControllerNotice)
			}
			for run := 0; run < 5 && left() > 0; run++ {
				clock = clock.Add(time.Hour)
				if err := runner.RunOnce(ctx); err != nil {
					t.Fatalf("run %d after the clear: %v", run, err)
				}
			}
			if rows := left(); rows != 0 {
				t.Fatalf("%d notice and controller rows of LEGION-1 left, want every row finished; log %s",
					rows, logged.String())
			}
			var told, want any
			switch {
			case tc.relayed && tc.approved:
				told, want = controllerDecisions(publisher), []record.DecisionKind{record.DecisionDesignApproved}
			case tc.relayed:
				told, want = controllerDecisions(publisher), []record.DecisionKind{record.DecisionDesignChangesRequested}
			case tc.approved:
				told, want = architectNotices(t, publisher), []record.NoticeKind{"design-approved"}
			default:
				told, want = architectNotices(t, publisher), []record.NoticeKind{"design-changes-requested"}
			}
			if !reflect.DeepEqual(told, want) {
				t.Fatalf("told %v, want %v; runner's log %s", told, want, logged.String())
			}
			finished := textLines(cleared.String(), `msg="workflow: a queued changes request `+
				`finished unpublished at its generation's clear: the person's approval followed it"`)
			if !tc.approved {
				if len(finished) != 0 {
					t.Fatalf("the clear logged %q, want no changes request finished", finished)
				}
				return
			}
			kind := record.OutboxKindNotice
			if tc.relayed {
				kind = record.OutboxKindControllerNotice
			}
			line := fmt.Sprintf("row=%d kind=%s issue=LEGION-1 root=LEGION-1 "+
				"ref=dispatch://LEGION-1/spec version=3 approved=3", request.id, kind)
			if len(finished) != 1 || !strings.Contains(finished[0], line) ||
				!strings.Contains(finished[0], "level=INFO") {
				t.Fatalf("the clear logged %q, want one Info line naming %s", finished, line)
			}
		})
	}
}

// relayRow is the one parked-decision controller row of decision the outbox holds: its id and the
// attempts it has made.
func relayRow(t *testing.T, pool *store.Pool, decision record.DecisionKind) (int64, int) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, attempts from outbox
		where kind = $1 and payload->>'kind' = $2 and payload->>'decision' = $3 order by id`,
		string(record.OutboxKindControllerNotice), string(record.ParkedDecisionNotice), string(decision))
	if err != nil {
		t.Fatalf("read the %s relay rows: %v", decision, err)
	}
	type relay struct {
		id       int64
		attempts int
	}
	relays, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (relay, error) {
		var out relay
		return out, row.Scan(&out.id, &out.attempts)
	})
	if err != nil {
		t.Fatalf("scan the %s relay rows: %v", decision, err)
	}
	if len(relays) != 1 {
		t.Fatalf("%d %s relay rows queued, want one", len(relays), decision)
	}
	return relays[0].id, relays[0].attempts
}

// controllerDecisions is the decision of every parked-decision notice published on the project's
// controller topic, in order.
func controllerDecisions(publisher *holderPublisher) []record.DecisionKind {
	_, delivered := publisher.snapshot()
	var told []record.DecisionKind
	for _, published := range delivered {
		if notice, ok := published.payload.(record.Notice); ok && published.topic == notify.ControllerTopic("legion") && notice.Kind == record.ParkedDecisionNotice {
			told = append(told, notice.Decision)
		}
	}
	return told
}

// textLines is every line of logged, a text log (slog.TextHandler), that holds text.
func textLines(logged, text string) []string {
	var lines []string
	for line := range strings.SplitSeq(logged, "\n") {
		if strings.Contains(line, text) {
			lines = append(lines, line)
		}
	}
	return lines
}

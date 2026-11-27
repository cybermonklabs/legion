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

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A person's changes request on the version of an ancestor's document that a step of a root
// stands open on is told to the root's architect and, keyed by the ancestor, to the ancestor's
// architect (workflow's tellBasisChanges, dispatch://INFRA-3320 R3), and the ancestor's copy is
// told only while it is still the person's word (dispatch://INFRA-3136/spec R2). That copy names
// the ancestor's own document and carries Dispatch's mark of the review and the document reviewed,
// so the workflow asks it of the ancestor's gates before it queues it, and the delivery asks again
// of the gates as they stand when it runs (readNoticeTree's stillOwed). The root's copy names the
// document of another issue than the root, which no gate of the root holds, so it stands however
// the person's word on the ancestor's document has moved, and it is told.
//
// LEGION-208's spec step stands open on the person's approval of version 3 of LEGION-100's
// document, which LEGION-100's own spec gate holds, and both trees run. The review, the ancestor's
// version 4 and the person's approval of it reach the engine as Dispatch's artifact events do, so
// the rows are the ones the workflow writes. Each row's first attempt finds no session holding its
// architect's role, and waits. Where the approval lands while the ancestor's copy waits, that copy
// finishes with nothing published and one line naming the row, the ancestor and both versions.
// Where the approval reached the daemon before the changes request's event did, the workflow
// queues no copy on the ancestor, and one line says so. The control is a request no approval
// followed, which both architects are told.
func TestTheAncestorsCopyOfABasisChangesRequestIsToldOnlyWhileItStands(t *testing.T) {
	t.Parallel()
	asked := basisSeedApprovedAt.Add(time.Hour)
	changes := intake.DispatchArtifact{Key: basisSeedAncestor, ArtifactID: basisSeedDoc, Kind: intake.DispatchArtifactChangesRequested, Version: 3,
		Reason: "name the rollback path", Actor: basisSeedPerson, Mark: record.EventMark{Seq: 45, At: asked}}
	newer := intake.DispatchArtifact{Key: basisSeedAncestor, ArtifactID: basisSeedDoc, Kind: intake.DispatchArtifactVersion, Version: 4,
		Mark: record.EventMark{Seq: 50, At: asked.Add(time.Hour)}}
	approved := intake.DispatchArtifact{Key: basisSeedAncestor, ArtifactID: basisSeedDoc, Kind: intake.DispatchArtifactApproved, Version: 4,
		Actor: basisSeedPerson, Mark: record.EventMark{Seq: 51, At: asked.Add(2 * time.Hour)}}
	// Every copy names version 3 of the ancestor's document, the person, the review's mark and the
	// document reviewed; the reason, which names the root's step, is other tests' to read.
	want := record.Notice{Kind: "basis-changes-requested", Role: claim.RoleArchitect, Version: 3, Actor: "user:alice",
		Ref: "dispatch://LEGION-100/spec", Review: changes.Mark, Artifact: basisSeedDoc}
	const supersededAtDelivery = "outbox changes request finished without publishing: the person's approval followed it"
	const supersededAtQueue = "workflow: the person's approval followed the changes request on the ancestor's document, so its architect is not told it"
	for _, tc := range []struct {
		name string
		// queued are the facts applied before the first attempt, and waited those applied while
		// the rows wait for their architects.
		queued, waited []intake.Fact
		// queuedOnAncestor is whether the workflow queues the ancestor's copy, and toldAncestor
		// whether the ancestor's architect is told it.
		queuedOnAncestor, toldAncestor bool
	}{
		{name: "the person approved a later version while the ancestor's copy waited",
			queued: []intake.Fact{changes}, waited: []intake.Fact{newer, approved}, queuedOnAncestor: true},
		{name: "the person's approval of a later version reached the daemon before the changes request",
			queued: []intake.Fact{newer, approved, changes}},
		{name: "no approval followed the request",
			queued: []intake.Fact{changes}, queuedOnAncestor: true, toldAncestor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newBasisSeedRig(t)
			rig.openOnBasis(basisSeedRoot, basisSeedRootDoc)
			putOutboxGate(t, rig.pool, rig.records, record.DesignGate{Issue: basisSeedAncestor, Step: record.DesignStepSpec, ArtifactID: basisSeedDoc,
				LatestVersion: 3, ApprovedVersion: new(3), ApprovedMark: basisSeedBasis(3).Approval, EverOpened: true})
			steps := config.DesignGateRootIssues.Steps()
			var workflowLog bytes.Buffer
			engine := workflow.New(rig.records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: steps},
				slog.New(slog.NewTextHandler(&workflowLog, nil)))
			applyFacts(t, rig.pool, engine, tc.queued)

			root, ancestor := basisChangesRows(t, rig.pool, basisSeedRoot), basisChangesRows(t, rig.pool, basisSeedAncestor)
			if len(root) != 1 {
				t.Fatalf("%d basis-changes-requested rows queued on the root, want one", len(root))
			}
			wantOnAncestor := 0
			if tc.queuedOnAncestor {
				wantOnAncestor = 1
			}
			if len(ancestor) != wantOnAncestor {
				t.Fatalf("%d basis-changes-requested rows queued on the ancestor, want %d", len(ancestor), wantOnAncestor)
			}
			queued := slices.Concat(root, ancestor)
			for _, row := range queued {
				got := row.notice
				got.Reason = ""
				if !reflect.DeepEqual(got, want) {
					t.Errorf("the workflow queued row %d naming version %d, actor %q, ref %q, review %+v and document %q, want %d, %q, %q, %+v and %q",
						row.id, got.Version, got.Actor, got.Ref, got.Review, got.Artifact, want.Version, want.Actor, want.Ref, want.Review, want.Artifact)
				}
			}
			refused := textLines(workflowLog.String(), supersededAtQueue)
			switch {
			case tc.queuedOnAncestor && len(refused) != 0:
				t.Fatalf("the workflow logged %q, want no such line: it queued the ancestor's copy", refused)
			case !tc.queuedOnAncestor && (len(refused) != 1 || !strings.Contains(refused[0], "issue=LEGION-208") ||
				!strings.Contains(refused[0], "ancestor=LEGION-100") || !strings.Contains(refused[0], "version=3 approved=4")):
				t.Fatalf("the workflow logged %q, want one line naming the root, the ancestor, the version reviewed and the version approved", refused)
			}

			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			for _, key := range []string{basisSeedRoot, basisSeedAncestor} {
				if _, _, err := sup.Create(ctx, supervise.Claim{Token: mustClaimToken(t, key, claim.RoleArchitect), Project: "legion", Tree: key, Issue: key,
					Role: claim.RoleArchitect, State: supervise.StateWorking}, ""); err != nil {
					t.Fatalf("create the architect claim of %s: %v", key, err)
				}
			}
			publisher := &holderPublisher{}
			publisher.setAbsent(architectTopic(t, basisSeedRoot), architectTopic(t, basisSeedAncestor))
			var logged bytes.Buffer
			clock := time.Now()
			runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: rig.pool, dispatchProject: "LEGION", records: rig.records,
				notices: publisher, supervisor: sup, project: "legion", gatedSteps: steps, now: func() time.Time { return clock }}
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("first attempt: %v", err)
			}
			if told := basisChangesTold(t, publisher); len(told) != 0 {
				t.Fatalf("told %v on the attempt no session holds either architect's role, want every row waiting", told)
			}

			applyFacts(t, rig.pool, engine, tc.waited)
			publisher.setAbsent()
			left := func() int {
				return outboxRowsOfKind(t, rig.pool, basisSeedRoot, record.OutboxKindNotice) +
					outboxRowsOfKind(t, rig.pool, basisSeedAncestor, record.OutboxKindNotice)
			}
			for run := 0; run < 5 && left() > 0; run++ {
				clock = clock.Add(time.Hour)
				if err := runner.RunOnce(ctx); err != nil {
					t.Fatalf("attempt after the wait %d: %v", run, err)
				}
			}
			if rows := left(); rows != 0 {
				t.Fatalf("%d notice rows of the root and the ancestor left, want every row finished; log %s", rows, logged.String())
			}
			wantTold := map[string]int{basisSeedRoot: 1}
			if tc.toldAncestor {
				wantTold[basisSeedAncestor] = 1
			}
			if told := basisChangesTold(t, publisher); !reflect.DeepEqual(told, wantTold) {
				t.Fatalf("basis-changes-requested told to the architects of %v, want %v", told, wantTold)
			}
			dropped := textLines(logged.String(), supersededAtDelivery)
			var basisDropped []string
			for _, line := range dropped {
				for _, row := range queued {
					if strings.Contains(line, fmt.Sprintf("row=%d ", row.id)) {
						basisDropped = append(basisDropped, line)
					}
				}
			}
			if !tc.queuedOnAncestor || tc.toldAncestor {
				if len(basisDropped) != 0 {
					t.Fatalf("logged %q, want no basis-changes-requested row finished without publishing", basisDropped)
				}
				return
			}
			line := fmt.Sprintf("row=%d issue=LEGION-100 version=3 approved=4", ancestor[0].id)
			if len(basisDropped) != 1 || !strings.Contains(basisDropped[0], line) || !strings.Contains(basisDropped[0], "level=INFO") {
				t.Fatalf("logged %q, want one Info line naming %s", basisDropped, line)
			}
		})
	}
}

// basisChangesRows is every basis-changes-requested notice row the outbox holds keyed by issue,
// oldest first: its id and the notice it carries.
func basisChangesRows(t *testing.T, pool *store.Pool, issue string) []queuedRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, kind, issue, payload from outbox
		where kind = $1 and issue = $2 and payload->>'kind' = 'basis-changes-requested' order by id`, string(record.OutboxKindNotice), issue)
	if err != nil {
		t.Fatalf("read the basis-changes-requested rows of %s: %v", issue, err)
	}
	queued, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var out record.OutboxRow
		return out, row.Scan(&out.ID, &out.Kind, &out.Issue, &out.Payload)
	})
	if err != nil {
		t.Fatalf("scan the basis-changes-requested rows of %s: %v", issue, err)
	}
	var notices []queuedRow
	for _, row := range queued {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode notice row %d: %v", row.ID, err)
		}
		notice, ok := payload.(record.Notice)
		if !ok {
			t.Fatalf("notice row %d carries %T, want a record.Notice", row.ID, payload)
		}
		notices = append(notices, queuedRow{id: row.ID, notice: notice})
	}
	return notices
}

// basisChangesTold is how many basis-changes-requested notices were published to the architect of
// the root and to the architect of the ancestor, by the issue whose architect it is.
func basisChangesTold(t *testing.T, publisher *holderPublisher) map[string]int {
	t.Helper()
	_, delivered := publisher.snapshot()
	told := map[string]int{}
	for _, published := range delivered {
		notice, ok := published.payload.(record.Notice)
		if !ok || notice.Kind != "basis-changes-requested" {
			continue
		}
		for _, key := range []string{basisSeedRoot, basisSeedAncestor} {
			if published.topic == architectTopic(t, key) {
				told[key]++
			}
		}
	}
	return told
}

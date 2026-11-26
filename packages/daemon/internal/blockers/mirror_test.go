package blockers

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

const testProject = "LEGION"

// The mirror writes the row of a Dispatch issue event and nothing else: the status, the keys the
// event says the issue waits on, its labels, its rank and the sequence it was observed at, under
// the daemon's project.
func TestMirrorWritesTheRowOfADispatchIssueFact(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	mirror := NewMirror(records, testProject)
	fact := intake.DispatchIssue{
		Key: "LEGION-900", Seq: 4, Status: "todo", Title: "a blocked child", Rank: "U",
		Blockers: []string{"LEGION-901", "LEGION-902"}, Labels: []string{"design"},
	}
	inTx(t, pool, func(tx pgx.Tx) {
		result, err := mirror.Apply(context.Background(), tx, fact)
		if err != nil {
			t.Fatalf("apply the issue fact: %v", err)
		}
		if result.Refusal != nil {
			t.Fatalf("the mirror refused %#v; it decides nothing", result.Refusal)
		}
	})
	want := []record.DispatchIssueRow{{
		Key: "LEGION-900", Project: testProject, Status: "todo",
		Blockers: []string{"LEGION-901", "LEGION-902"}, Seq: 4, Labels: []string{"design"}, Rank: "U",
	}}
	if got := mirrored(t, pool, records); !reflect.DeepEqual(got, want) {
		t.Fatalf("the mirror holds %#v, want %#v", got, want)
	}
}

// Only a reading of Dispatch writes the copy. An issue event, the turn's observations (its
// re-read of each issue it waits on, and its listing) and the release's read of the children it
// adopts each write the row of the issue they read; a turn that observed nothing writes nothing;
// a claim event writes the claim and no row. Every other fact writes neither, whatever issue it
// names: a fact the daemon makes says what the daemon intends, never what Dispatch said, and a
// seed's read of a document or of a new parent (PendingBasisRead, BasisChangesRead, ReparentRead)
// carries no value of the copy at a Dispatch sequence. The kinds are read from package intake's
// own source, each type that is an intake.Fact, so a kind added there fails this test until a row
// here says what the mirror writes for it. Each row is applied in a transaction of its own, which
// is rolled back, so each meets an empty copy.
func TestOnlyAReadingOfDispatchWritesTheCopy(t *testing.T) {
	kinds := factKinds(t)
	pool := migratedPool(t)
	mirror := NewMirror(record.NewStore(), testProject)
	type row struct {
		name         string
		fact         intake.Fact
		rows, claims []string
	}
	rows := []row{
		{fact: intake.DispatchIssue{Key: "LEGION-900", Seq: 4, Status: "todo"}, rows: []string{"LEGION-900"}},
		{name: "ControllerTick with observations", fact: intake.ControllerTick{Observed: []intake.IssueStatus{
			{Key: "LEGION-901", Project: testProject, Status: "done", Seq: 6}}}, rows: []string{"LEGION-901"}},
		// A turn with nothing waiting reads nothing from Dispatch, which is almost every turn.
		{name: "ControllerTick with no observation", fact: intake.ControllerTick{}},
		{fact: intake.ChildAdoption{Tree: "LEGION-800", Children: []intake.AdoptedChild{
			{Issue: "LEGION-902", Parent: "LEGION-800", Status: "todo", Seq: 7}}}, rows: []string{"LEGION-902"}},
		{fact: intake.DispatchClaim{Key: "LEGION-903", Seq: 3, ClaimedBy: "user:alice"}, claims: []string{"LEGION-903"}},
	}
	for _, fact := range []intake.Fact{
		intake.Handover{}, intake.DispatchArtifact{}, intake.MergeHoldsChanged{}, intake.GateAboveDispatch{},
		intake.DispatchAskOpened{}, intake.DispatchAskRestored{}, intake.DispatchAskClosed{}, intake.DispatchAskReply{},
		intake.DispatchApprovalMoved{}, intake.DispatchAskTurn{}, intake.DispatchDefaultAssignee{}, intake.ApprovalsBootRead{},
		intake.DispatchConsumerPosition{}, intake.EntrySweep{}, intake.ProjectResumed{}, intake.SignalDeadline{},
		intake.P0BoundChanged{},
		intake.PullRequestOpened{}, intake.PullRequestSynchronized{}, intake.PullRequestReview{}, intake.PullRequestChecks{},
		intake.PullRequestMergeability{}, intake.PullRequestMerged{}, intake.PullRequestClosed{}, intake.PullRequestDequeued{},
		intake.PullRequestEnqueued{}, intake.QueueTimeline{}, intake.QueueBootStatus{}, intake.ModelRequests{}, intake.TurnEdge{},
		intake.SignalSettingChanged{},
		intake.PullRequestReviewRequests{}, intake.PullRequestMergeStateUnread{},
		intake.MergeQueueReadDone{}, intake.ConflictConfirmDone{}, intake.PullRequestMissing{}, intake.Push{}, intake.BasePush{},
		intake.HandoffComplete{}, intake.BackwardMove{}, intake.RetryOrEscalate{}, intake.RestartPhase{}, intake.SignOff{},
		intake.GateRegistered{}, intake.PlanGiven{}, intake.TaskStopped{}, intake.CloseRoot{}, intake.ClaimFailed{},
		intake.SessionLost{}, intake.SessionStoreFault{},
		intake.MemoryGuardKill{}, intake.ControllerMemoryGuardKill{}, intake.VolumeNearFull{}, intake.ClaimReady{}, intake.TaskConfirmed{},
		intake.GateRequestFound{},
		intake.CatchUpApplied{}, intake.LingerExpired{}, intake.DaemonBooted{}, intake.CutoverMergerRepair{}, intake.CheckReport{},
		intake.CheckWakeDue{}, intake.CheckLookEnded{}, intake.CheckBound{}, intake.ListenerWake{}, intake.PodBackstopDue{},
		intake.WorkerSilent{}, intake.WorkerBackgroundStalled{}, intake.PendingBasisRead{}, intake.BasisChangesRead{},
		intake.ReparentRead{},
	} {
		rows = append(rows, row{fact: fact})
	}
	covered := map[string]bool{}
	for _, tc := range rows {
		kind := reflect.TypeOf(tc.fact).Name()
		if !slices.Contains(kinds, kind) {
			t.Fatalf("%s is no fact kind of package intake's source, which names %v", kind, kinds)
		}
		covered[kind] = true
		name := tc.name
		if name == "" {
			name = kind
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin transaction: %v", err)
			}
			defer func() {
				if err := tx.Rollback(ctx); err != nil {
					t.Errorf("roll back transaction: %v", err)
				}
			}()
			result, err := mirror.Apply(ctx, tx, tc.fact)
			if err != nil {
				t.Fatalf("apply %#v: %v", tc.fact, err)
			}
			if result.Refusal != nil {
				t.Fatalf("the mirror refused %#v; it decides nothing", result.Refusal)
			}
			if got := keysOf(t, tx, `select key from dispatch_issues order by key`); !slices.Equal(got, tc.rows) {
				t.Errorf("the copy holds rows of %v after %#v, want %v", got, tc.fact, tc.rows)
			}
			if got := keysOf(t, tx, `select key from dispatch_claims order by key`); !slices.Equal(got, tc.claims) {
				t.Errorf("the copy holds claims of %v after %#v, want %v", got, tc.fact, tc.claims)
			}
		})
	}
	for _, kind := range kinds {
		if !covered[kind] {
			t.Errorf("the fact kind %s has no row here: say what the mirror writes for it", kind)
		}
	}
}

// A reading of Dispatch writes the values it carries and leaves every other value as the copy
// holds it, at the sequence of the reading that wrote it. Each row first takes one reading at 10
// of all five values (a), then one reading of its kind at 20 (b), and reads each value back with
// its sequence. An issue event, an Observed fact (the daemon's own status write, release_children's
// read of a todo child, admission's read before it applies an event), the turn's re-read of one
// issue and a listing (the boot's, the repository recovery's, the turn's above its single-read
// bound) each carry all five. The release's read of a child it adopts carries the status, the
// parent and the blockers, and leaves the labels and the rank as the reading at 10 wrote them.
// The two issue facts alone, the event and the Observed fact, are facts the daemon applies, so
// they alone raise the row's applied sequence to 20; the turn's re-read, a listing and the
// release's read leave it at 0.
func TestAReadingLeavesTheValuesItDoesNotCarry(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	mirror := NewMirror(records, testProject)
	// b's status and rank are the captured event's own; its text is edited for the other three.
	parent, blockedBy, labels := "LEGION-810", []string{"LEGION-902", "LEGION-903"}, []string{"legion", "urgent"}
	a := copyRow{Status: "backlog", StatusSeq: 10, Parent: "LEGION-800", ParentSeq: 10, Blockers: []string{"LEGION-901"}, BlockersSeq: 10,
		Labels: []string{"design"}, LabelsSeq: 10, Rank: "a0", RankSeq: 10, Seq: 10}
	b := copyRow{Status: "todo", StatusSeq: 20, Parent: parent, ParentSeq: 20, Blockers: blockedBy, BlockersSeq: 20,
		Labels: labels, LabelsSeq: 20, Rank: "UUU", RankSeq: 20, Seq: 20, AppliedSeq: 20}
	read := b
	read.AppliedSeq = 0
	released := read
	released.Labels, released.LabelsSeq, released.Rank, released.RankSeq = a.Labels, a.LabelsSeq, a.Rank, a.RankSeq
	apply := func(fact intake.Fact) func(pgx.Tx) error {
		return func(tx pgx.Tx) error {
			result, err := mirror.Apply(context.Background(), tx, fact)
			if err == nil && result.Refusal != nil {
				err = fmt.Errorf("the mirror refused %#v; it decides nothing", result.Refusal)
			}
			return err
		}
	}
	for _, tc := range []struct {
		name, key string
		read      func(pgx.Tx) error
		want      copyRow
	}{
		{name: "an issue event", key: testProject + "-3", read: apply(capturedIssueEvent(t, 20, parent, blockedBy, labels)), want: b},
		{name: "an Observed fact", key: "LEGION-911", read: apply(intake.ObservedIssue(dispatch.Issue{Key: "LEGION-911", Project: testProject,
			Status: "todo", Parent: &parent, Rank: "UUU", Labels: labels, BlockedBy: blockedBy, LastSeq: 20})), want: b},
		{name: "the turn's re-read", key: "LEGION-912", read: apply(intake.ControllerTick{Observed: []intake.IssueStatus{{Key: "LEGION-912",
			Project: testProject, Status: "todo", Blockers: blockedBy, Seq: 20, Parent: parent, Labels: labels, Rank: "UUU"}}}), want: read},
		{name: "a listing", key: "LEGION-913", read: func(tx pgx.Tx) error {
			return Reconcile(context.Background(), tx, records, testProject, []dispatch.IssueSummary{{Key: "LEGION-913", Status: "todo",
				Parent: &parent, Rank: "UUU", Labels: labels, LastSeq: 20, BlockedBy: blockedBy}})
		}, want: read},
		{name: "the release's read", key: "LEGION-914", read: apply(intake.ChildAdoption{Tree: parent, Children: []intake.AdoptedChild{
			{Issue: "LEGION-914", Parent: parent, Status: "todo", BlockedBy: blockedBy, Seq: 20}}}), want: released},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inTx(t, pool, func(tx pgx.Tx) {
				if err := records.PutDispatchIssue(context.Background(), tx, record.DispatchReading{Key: tc.key, Project: testProject, Seq: 10,
					Status: record.Carry(a.Status), Parent: record.Carry(a.Parent), Blockers: record.Carry(a.Blockers),
					Labels: record.Carry(a.Labels), Rank: record.Carry(a.Rank)}); err != nil {
					t.Fatalf("write the reading at 10: %v", err)
				}
				if err := tc.read(tx); err != nil {
					t.Fatalf("apply %s at 20: %v", tc.name, err)
				}
			})
			if got := copiedRow(t, pool, tc.key); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("after %s the row of %s = %+v, want %+v", tc.name, tc.key, got, tc.want)
			}
		})
	}
}

// The periodic turn's re-read rides on the controller tick, and every row it observed is written
// value by value as an event's row is: a view fresher than a value replaces it, and one older than
// the values an event already wrote leaves those values exactly as they stand.
func TestMirrorWritesEveryRowAControllerTickObserved(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	mirror := NewMirror(records, testProject)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := records.PutDispatchIssue(context.Background(), tx, record.DispatchReading{Key: "LEGION-900", Project: testProject, Seq: 9,
			Status: record.Carry("in_progress"), Parent: record.Carry(""), Blockers: record.Carry[[]string](nil)}); err != nil {
			t.Fatalf("seed a fresher event's row: %v", err)
		}
		result, err := mirror.Apply(context.Background(), tx, intake.ControllerTick{Observed: []intake.IssueStatus{
			{Key: "LEGION-900", Project: testProject, Status: "todo", Seq: 4},
			{Key: "LEGION-901", Project: testProject, Status: "done", Blockers: []string{"LEGION-902"}, Seq: 6},
		}})
		if err != nil {
			t.Fatalf("apply the controller tick: %v", err)
		}
		if result.Refusal != nil {
			t.Fatalf("the mirror refused %#v; it decides nothing", result.Refusal)
		}
	})
	want := []record.DispatchIssueRow{
		{Key: "LEGION-900", Project: testProject, Status: "in_progress", Blockers: []string{}, Seq: 9, Labels: []string{}},
		{Key: "LEGION-901", Project: testProject, Status: "done", Blockers: []string{"LEGION-902"}, Seq: 6, Labels: []string{}},
	}
	if got := mirrored(t, pool, records); !reflect.DeepEqual(got, want) {
		t.Fatalf("the mirror holds %#v, want %#v", got, want)
	}
}

// An adoption carries the release's own read of each child, and the mirror writes what that read
// carries: the status, the keys the child waits on and the parent it showed, each at the read's
// sequence. The parent is among them because the children-first rule (record.ChildWaitsOf) reads
// the child under the parent the read showed (dispatch://INFRA-714/spec R3); a reading that did
// not carry it would leave the parent an older reading wrote.
func TestMirrorWritesTheParentAnAdoptionRead(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	mirror := NewMirror(records, testProject)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := records.PutDispatchIssue(context.Background(), tx, record.DispatchReading{Key: "LEGION-900", Project: testProject, Seq: 4,
			Status: record.Carry("todo"), Parent: record.Carry("LEGION-800"), Blockers: record.Carry[[]string](nil)}); err != nil {
			t.Fatalf("seed the child's row: %v", err)
		}
		result, err := mirror.Apply(context.Background(), tx, intake.ChildAdoption{Tree: "LEGION-800",
			Children: []intake.AdoptedChild{{Issue: "LEGION-900", Parent: "LEGION-800", Status: "todo",
				BlockedBy: []string{"LEGION-901"}, Seq: 7}}})
		if err != nil {
			t.Fatalf("apply the adoption: %v", err)
		}
		if result.Refusal != nil {
			t.Fatalf("the mirror refused %#v; it decides nothing", result.Refusal)
		}
	})
	want := []record.DispatchIssueRow{{Key: "LEGION-900", Project: testProject, Status: "todo",
		Blockers: []string{"LEGION-901"}, Seq: 7, Parent: "LEGION-800", Labels: []string{}}}
	if got := mirrored(t, pool, records); !reflect.DeepEqual(got, want) {
		t.Fatalf("the mirror holds %#v, want %#v", got, want)
	}
}

// Reconcile is the boot listing's own write of every summary, as a reading of all five values: a
// listing older than an event already applied leaves the values that event wrote alone. A listing
// shows each issue's claim, nobody here, which lands behind the claim's own fence: the event wrote
// no claim.
func TestReconcileMirrorsEverySummaryUnderTheFence(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := records.PutDispatchIssue(context.Background(), tx, record.DispatchReading{Key: "LEGION-900", Project: testProject, Seq: 9,
			Status: record.Carry("in_progress"), Parent: record.Carry(""), Blockers: record.Carry[[]string](nil)}); err != nil {
			t.Fatalf("seed a fresher event's row: %v", err)
		}
		if err := Reconcile(context.Background(), tx, records, testProject, []dispatch.IssueSummary{
			{Key: "LEGION-900", Status: "todo", LastSeq: 4},
			{Key: "LEGION-901", Status: "done", LastSeq: 6, BlockedBy: []string{"LEGION-902"}},
		}); err != nil {
			t.Fatalf("reconcile the listing: %v", err)
		}
	})
	want := []record.DispatchIssueRow{
		{Key: "LEGION-900", Project: testProject, Status: "in_progress", Blockers: []string{}, Seq: 9, ClaimSeq: 4, Labels: []string{}},
		{Key: "LEGION-901", Project: testProject, Status: "done", Blockers: []string{"LEGION-902"}, Seq: 6, ClaimSeq: 6, Labels: []string{}},
	}
	if got := mirrored(t, pool, records); !reflect.DeepEqual(got, want) {
		t.Fatalf("the mirror holds %#v, want %#v", got, want)
	}
}

func mirrored(t *testing.T, pool *pgxpool.Pool, records record.Store) []record.DispatchIssueRow {
	t.Helper()
	var rows []record.DispatchIssueRow
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if rows, err = records.DispatchIssues(context.Background(), tx, testProject); err != nil {
			t.Fatalf("list the mirror: %v", err)
		}
	})
	return rows
}

// keysOf is the keys query selects in tx, in its order.
func keysOf(t *testing.T, tx pgx.Tx, query string) []string {
	t.Helper()
	rows, err := tx.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return keys
}

// copyRow is one row of the copy as these tests read it by SQL: each of its five values with the
// sequence of the reading that wrote it (a parent of none read as ""), and the row's own sequence,
// the highest of any reading the row took.
type copyRow struct {
	Status      string
	StatusSeq   int64
	Parent      string
	ParentSeq   int64
	Blockers    []string
	BlockersSeq int64
	Labels      []string
	LabelsSeq   int64
	Rank        string
	RankSeq     int64
	Seq         int64
	AppliedSeq  int64
}

// copiedRow is key's row of the copy, read by SQL.
func copiedRow(t *testing.T, pool *pgxpool.Pool, key string) copyRow {
	t.Helper()
	var row copyRow
	if err := pool.QueryRow(context.Background(), `select status, status_seq, coalesce(parent, ''), parent_seq, blockers, blockers_seq,
		labels, labels_seq, rank, rank_seq, dispatch_seq, applied_seq from dispatch_issues where key = $1`, key).Scan(&row.Status, &row.StatusSeq,
		&row.Parent, &row.ParentSeq, &row.Blockers, &row.BlockersSeq, &row.Labels, &row.LabelsSeq, &row.Rank, &row.RankSeq, &row.Seq,
		&row.AppliedSeq); err != nil {
		t.Fatalf("read the row of %s: %v", key, err)
	}
	return row
}

// capturedIssueEvent is Dispatch's captured issue.updated event (intake's testdata), as Dispatch's
// event log serves it, made an event of this project's issue <project>-3 at seq, with its parent,
// its blockers and its labels written into its own JSON text, and decoded by the daemon's own
// decoder. Its status (todo) and its rank (UUU) are the captured ones.
func capturedIssueEvent(t *testing.T, seq int64, parent string, blockedBy, labels []string) intake.Fact {
	t.Helper()
	captured, err := os.ReadFile("../intake/testdata/dispatch/issue-updated.json")
	if err != nil {
		t.Fatalf("read the captured Dispatch issue.updated event: %v", err)
	}
	var envelope struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(captured, &envelope); err != nil {
		t.Fatalf("decode the captured envelope: %v", err)
	}
	text := func(value any) string {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode %#v: %v", value, err)
		}
		return string(encoded)
	}
	event := strings.ReplaceAll(envelope.Payload, "CAPTURE", testProject)
	for _, edit := range []struct{ from, to string }{
		{`"seq":2,`, fmt.Sprintf(`"seq":%d,`, seq)},
		{`"parent":null`, `"parent":` + text(parent)},
		{`"labels":[]`, `"blocked_by":` + text(blockedBy) + `,"labels":` + text(labels)},
	} {
		edited := strings.Replace(event, edit.from, edit.to, 1)
		if edited == event {
			t.Fatalf("the captured event has no %s to replace", edit.from)
		}
		event = edited
	}
	fact, _, err := intake.DecodeDispatchEvent(testProject, testProject+"-3", []byte(event))
	if err != nil || fact == nil {
		t.Fatalf("decode the captured event = %#v, %v; want an issue fact", fact, err)
	}
	return fact
}

// factKinds is every fact type of package intake, read from its own source: each type with the
// method that makes a type an intake.Fact (namedIssue), in name order.
func factKinds(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../intake/*.go")
	if err != nil {
		t.Fatalf("list package intake's source: %v", err)
	}
	kinds := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "namedIssue" || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			receiver := fn.Recv.List[0].Type
			if star, ok := receiver.(*ast.StarExpr); ok {
				receiver = star.X
			}
			if name, ok := receiver.(*ast.Ident); ok {
				kinds[name.Name] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(kinds))
}

func inTx(t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && err != pgx.ErrTxClosed {
			t.Errorf("rollback transaction: %v", err)
		}
	}()
	fn(tx)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}
}

// migratedPool is a migrated database of this test's own, cloned from the package's template: the
// mirror's writes are SQL — an upsert fenced in its own where clause — so they are tested against
// the real schema, as every other package's record tests are.
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testpg.ClonePool(t, "legion_blockers_test")
}

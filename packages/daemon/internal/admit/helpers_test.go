package admit

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

type engineStub struct {
	store       record.Store
	recordChild *record.Issue
	done        string
}

func (e engineStub) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if e.recordChild != nil {
		if dispatch, ok := fact.(intake.DispatchIssue); ok && dispatch.Key == e.recordChild.Key {
			if err := e.store.PutIssue(ctx, tx, *e.recordChild); err != nil {
				return intake.Result{}, err
			}
		}
	}
	if e.done != "" {
		stored, err := e.store.Issue(ctx, tx, e.done)
		if err != nil {
			return intake.Result{}, err
		}
		if stored == nil {
			return intake.Result{}, nil
		}
		stored.Phase = phase.Done
		if err := e.store.PutIssue(ctx, tx, *stored); err != nil {
			return intake.Result{}, err
		}
	}
	return intake.Result{}, nil
}

const testProject = "LEGION"

// handed is HandedOver of an issue handed to Legion. The wire types resolve it from the issue's
// labels at the decode and client boundary, and every decision reads that bool, so tests set it
// directly.
const handed = true

var fixedNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// testRepositories is every repository the admission tests record issues on, each at main, and
// leaves out acme/gadgets, whose review App is not installed. A repository it does not name is
// one no preparation has read GitHub for, which the daemon answers as unread (its lookup).
var testRepositories = servedRepositories{"sjawhar/legion": "main", "acme/widgets": "main", "acme/tools": "release/v7.1"}

type servedRepositories map[string]string

func (r servedRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	if branch, ok := r[resolved]; ok {
		return workflow.Resolution{Repo: resolved, DefaultBranch: branch, GitHubRepo: resolved}, nil
	}
	if resolved == "acme/gadgets" {
		return workflow.Resolution{Refusal: "the review App is not installed on acme/gadgets", LeftOut: true}, nil
	}
	return workflow.Resolution{Refusal: "GitHub has not been read for " + resolved + " yet", Unread: true}, nil
}

func newAdmission(t *testing.T, cap int, log *slog.Logger) *Admission {
	t.Helper()
	return newAdmissionWithGate(t, cap, "", log)
}

// newAdmissionWithGate is newAdmission under a design-gate policy of its own: the default leaves
// workflow.Config's zero value, which is the armed gate.
func newAdmissionWithGate(t *testing.T, cap int, gate config.DesignGate, log *slog.Logger) *Admission {
	t.Helper()
	return newAdmissionWithSteps(t, cap, gate.Steps(), log)
}

// newAdmissionWithSteps is newAdmission under a design gate of several steps, which is what a
// deployment's `gates.design` list resolves to.
func newAdmissionWithSteps(t *testing.T, cap int, steps []record.DesignStep, log *slog.Logger) *Admission {
	t.Helper()
	return newAdmissionForProject(t, testProject, cap, steps, log)
}

// newAdmissionForProject is newAdmissionWithSteps for a Dispatch project key of its own. Every
// other helper here records issues under testProject, so a test that takes another key seeds its
// own records: the key is what admission reads the line, the claims and the tree lifecycles
// under, and a test of two keys proves a behaviour reads none of it.
func newAdmissionForProject(t *testing.T, project string, cap int, steps []record.DesignStep, log *slog.Logger) *Admission {
	t.Helper()
	return newAdmissionDrawing(t, project, cap, steps, log, nil)
}

// newAdmissionDrawing is newAdmissionForProject whose engine draws every `random` experiment from
// source, the one source admission draws with too (workflow.Engine's ExperimentSource); nil is the
// engine's own, experiment.CryptoSource.
func newAdmissionDrawing(t *testing.T, project string, cap int, steps []record.DesignStep, log *slog.Logger, source func(uint64) (uint64, error)) *Admission {
	t.Helper()
	engine := workflow.New(record.NewStore(), workflow.Config{Project: project, Repositories: testRepositories, Steps: steps,
		ExperimentSource: source, Clock: func() time.Time { return fixedNow }}, log)
	admission := New(record.NewStore(), engine, cap, project, log)
	admission.now = func() time.Time { return fixedNow }
	return admission
}

// architectStart is the task admission queues with the start of a root architect
// (workflow.ArchitectStartTask), derived from the record exactly as admission derives it: what
// workflow.ArchitectStartOf reads of the root as it stands now — whether a hand-over carried it in,
// the design gate rows it holds, the run its architect claim last served and the roots that rest
// on its approval — under the gate steps its own admission runs, and what its tree is owed where a
// person decided something on it while nobody in the tree could hear them (decided). A test about
// what admission writes names the task through this helper; which shape each root gets, and the
// whole text of each shape, are pinned in internal/workflow (TestTheArchitectTaskOfEachShape,
// TestTheArchitectTaskShapeRule) and the delivery to a launched architect in internal/daemon
// (TestAFreshAdmissionGivesItsArchitectItsStartTask).
func architectStart(t *testing.T, pool *pgxpool.Pool, admission *Admission, key, decided string) string {
	t.Helper()
	root := issue(t, pool, key)
	var start workflow.ArchitectStart
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		start, err = workflow.ArchitectStartOf(context.Background(), tx, record.NewStore(), admission.engine.GatedSteps(), root)
		if err != nil {
			t.Fatalf("read what the architect task of %s is derived from: %v", key, err)
		}
	})
	start.Decided = decided
	return workflow.ArchitectStartTask(start)
}

// apply applies fact as the Dispatch consumer does: the Mirror first, which writes the reading
// into the daemon's copy of Dispatch's issues that every decision reads the chain from
// (record.ReadChains), then engine and admission.
func apply(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID string, fact intake.DispatchIssue, engine intake.Handler) {
	t.Helper()
	fact.Labels = handedOverLabels(fact.HandedOver, fact.Labels)
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", eventID, fact,
		blockers.NewMirror(record.NewStore(), admission.project), engine, admission); err != nil {
		t.Fatalf("ApplyFact %s: %v", eventID, err)
	}
}

// handedOverLabels is the labels Dispatch shows on an issue a fixture states as handed to Legion
// or not: the labels it names, else the legion label where it is handed over, since Dispatch
// says an issue is handed over by that label (dispatch.CarriesLegionLabel) and the copy holds it
// (R26 of dispatch://INFRA-3320 reads it there).
func handedOverLabels(handedOver bool, labels []string) []string {
	if labels == nil && handedOver {
		return []string{dispatch.LegionLabel}
	}
	return labels
}

// notStateRows is the predicate that leaves the state lane's rows (record.LaneState) out of a read
// of the whole outbox: a write of an issue, a slot, a claim or the admission block queues one when
// the store's state events are on, and a test that counts or lists the outbox asserts the rows of
// the other lanes.
var notStateRows = func() string {
	var kinds []string
	for _, kind := range record.LaneKinds(record.LaneState) {
		kinds = append(kinds, "'"+string(kind)+"'")
	}
	return "kind not in (" + strings.Join(kinds, ", ") + ")"
}()

type effect struct {
	kind    record.OutboxKind
	issue   string
	payload record.OutboxPayload
}

func assertEffects(t *testing.T, pool *pgxpool.Pool, want []effect) {
	t.Helper()
	if got := effects(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("outbox effects = %#v, want %#v", got, want)
	}
}

// effects is the outbox as these tests compare it: every row in id order, its payload decoded,
// with every task-less phase start and every step record left out. A phase worker's start is one
// row, which carries the task and, in a phase that keeps a slot (record.PhaseKeepsSlot), waits
// for the issue's admission slot (workflow.StartOrOwe); a start that names a phase and carries no
// task is a role admission brings back beside it, which asks that role nothing. These assertions
// are about the starts that carry work, as the workflow package's own helpers are. A start that
// names no phase is no phase worker's — the architect start promotion makes, the production
// check's look — and is left in. A step record is written beside the rows of each start and end
// of a step, and the tests of those sites read it exactly (workflow's step_sites_test.go and
// step_sites_linger_test.go, and run_end_test.go and parent_waits_no_slot_test.go here, through
// step_records_test.go's assertStepWrites, for admission's own sites), so every row of any other
// kind is still compared here.
//
// A supervise start carries the phase entry its task is of (record.SuperviseRequest.Entry),
// which the transition raised on the role's phases row: the record counts the entries a role has
// been handed, so two runs of the same events write the same number and a comparison of two runs
// reads it like any other field.
//
// Every kind whose run and entry fields are its fence and nothing else is read without them
// (unfenced): the run and the phase entry it was queued at, which its executor checks where it
// commits (record.Fence). These assertions read what each effect does; the fence is
// record.FenceOf's and its own tests' (the outbox kind inventory and k27's late-effect test), and
// a count of phase entries in every expected row would tie each assertion here to how many
// entries the fixture's roles happened to be handed.
//
// The state lane's rows are left out too (notStateRows): these tests assert the effects of the
// other lanes, and the state events are asserted where they are written.
func effects(t *testing.T, pool *pgxpool.Pool) []effect {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select kind, issue, payload from outbox where `+notStateRows+` order by id`)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	defer rows.Close()
	var got []effect
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		if row.Kind == record.OutboxKindStepRecord {
			continue
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode outbox: %v", err)
		}
		if start, ok := payload.(record.SuperviseRequest); ok {
			if start.Op == "start" && start.Task == "" && start.Phase != "" {
				continue
			}
		}
		got = append(got, effect{kind: row.Kind, issue: row.Issue, payload: unfenced(payload)})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox: %v", err)
	}
	return got
}

// unfenced is payload with its fence cleared, for a kind whose run and entry fields are its fence
// and nothing else (record.FenceOf), and an ordinary status row without the Dispatch status its
// record held when it was queued (record.StatusWrite.DispatchStatus), which record.NewFencedRow
// stamps beside the fence. Every other kind is left whole: a supervise start's generation and entry
// are the work it hands out, and a retention end's are what it ends.
func unfenced(payload record.OutboxPayload) record.OutboxPayload {
	switch value := payload.(type) {
	case record.StatusWrite:
		value.Generation, value.Entry, value.DispatchStatus = 0, 0, ""
		return value
	case record.MessagePost:
		value.Generation, value.Entry = 0, 0
		return value
	case record.Notice:
		value.Generation, value.Entry = 0, 0
		return value
	case record.ControllerNotice:
		value.Generation, value.Entry = 0, 0
		return value
	case record.RoleMessage:
		value.Generation, value.Entry = 0, 0
		return value
	case record.MergeForwardMessage:
		value.Generation, value.Entry = 0, 0
		return value
	case record.UpdateAsk:
		value.Generation, value.Entry = 0, 0
		return value
	case record.ClaimRelease:
		value.Generation, value.Entry = 0, 0
		return value
	case record.PhaseEnded:
		value.Generation, value.Entry = 0, 0
		return value
	case record.HookPrompt:
		value.Generation, value.Entry = 0, 0
		return value
	case record.MergeQueueRemoval:
		value.Generation, value.Entry = 0, 0
		return value
	}
	return payload
}

func assertSlots(t *testing.T, pool *pgxpool.Pool, want []record.Slot) {
	t.Helper()
	got := slots(t, pool)
	if len(got) != len(want) {
		t.Fatalf("slots = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i].Issue != want[i].Issue || got[i].Index != want[i].Index || !got[i].AdmittedAt.Equal(want[i].AdmittedAt) {
			t.Fatalf("slots = %#v, want %#v", got, want)
		}
	}
}

// waitingLine is the waiting line of project, in its order, under the design gate of steps (nil
// for the deployment's single step spec). It reads the stored state and judges the line through
// blockers.JudgeLine, the judgement admission promotes from: the held set
// (record.LineFacts.Held), the children-first waits (record.LineFacts.ChildWaits) and the waits
// of started issues (record.LineFacts.Waits), and then the line. It runs none of the releases a
// pass runs before it judges the line, so it shows the line as the last pass left it.
func waitingLine(t *testing.T, pool *pgxpool.Pool, project string, steps []record.DesignStep) []string {
	t.Helper()
	var got []string
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		issues, err := records.Issues(ctx, tx)
		if err != nil {
			t.Fatalf("list issues: %v", err)
		}
		slots, err := records.Slots(ctx, tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		facts, err := record.ReadLineFacts(ctx, tx, records, project, issues, steps)
		if err != nil {
			t.Fatalf("read the line facts: %v", err)
		}
		_, _, line, _, err := blockers.JudgeLine(ctx, tx, records, project, issues, slots, facts, nil)
		if err != nil {
			t.Fatalf("judge the waiting line: %v", err)
		}
		for _, waiting := range line {
			got = append(got, waiting.Issue.Key)
		}
	})
	return got
}

// assertWaiting compares the test project's waiting line, in its order, with want.
func assertWaiting(t *testing.T, pool *pgxpool.Pool, want []string) {
	t.Helper()
	if got := waitingLine(t, pool, testProject, nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("waiting = %#v, want %#v", got, want)
	}
}

func slots(t *testing.T, pool *pgxpool.Pool) []record.Slot {
	t.Helper()
	var got []record.Slot
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		got, err = record.NewStore().Slots(context.Background(), tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
	})
	return got
}

func issue(t *testing.T, pool *pgxpool.Pool, key string) record.Issue {
	t.Helper()
	got := maybeIssue(t, pool, key)
	if got == nil {
		t.Fatalf("issue %s is missing", key)
	}
	return *got
}

// withoutMovement is a record with the moment the daemon last saw the issue move taken off
// (record.Issue.MovedAt). Every write of a record stamps it from the database's own clock and
// every fact that moves the issue writes it again, so two runs of the same events never agree on
// it and no fixture can name it: a test about what a fact records compares everything else. The
// column's own rule is held to by TestTheRecordKeepsWhenAnIssueLastMoved, which is the one test
// here that reads it.
func withoutMovement(issue record.Issue) record.Issue {
	issue.MovedAt = time.Time{}
	return issue
}

func maybeIssue(t *testing.T, pool *pgxpool.Pool, key string) *record.Issue {
	t.Helper()
	var got *record.Issue
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		got, err = record.NewStore().Issue(context.Background(), tx, key)
		if err != nil {
			t.Fatalf("read issue: %v", err)
		}
	})
	return got
}

// resumeTaskOf is the resume task engine sends the worker of key's phase, as the record holds
// the issue now: workflow's Engine.ResumeTask, the one place every resume is composed, read in a
// transaction it rolls back.
func resumeTaskOf(t testing.TB, pool *pgxpool.Pool, engine *workflow.Engine, key string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read of the resume task of %s: %v", key, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	issue, err := record.NewStore().Issue(ctx, tx, key)
	if err != nil || issue == nil {
		t.Fatalf("read the record of %s: %+v, %v", key, issue, err)
	}
	task, err := engine.ResumeTask(ctx, tx, *issue)
	if err != nil {
		t.Fatalf("compose the resume task of %s: %v", key, err)
	}
	return task
}

// putIssue writes issue's record and its row of the copy of Dispatch's issues as a reading of
// Dispatch would show it (putCopyTx): the copy is what every decision reads the chain from, so a
// fixture that seeds a record seeds the issue's place on Dispatch's chain with it.
func putIssue(t *testing.T, pool *pgxpool.Pool, issue record.Issue) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutIssue(context.Background(), tx, issue); err != nil {
			t.Fatalf("put issue: %v", err)
		}
		putCopyTx(t, tx, issue)
	})
}

// putCopyTx writes the copy's row of issue as a reading of Dispatch does
// (record.Store.PutDispatchIssue): the parent the record names, the status Dispatch last showed
// it in (DispatchStatus, else Status) and the legion label where the record says it is handed over
// (handedOverLabels), at a sequence above every value the copy holds of it, so the row reads as the
// record does whatever was written before. A fixture that names no parent but a tree other than the
// issue's own key states that the issue stands under that tree's root, and its row says so. It is
// the one way these tests seed an issue's place on Dispatch's chain.
func putCopyTx(t *testing.T, tx pgx.Tx, issue record.Issue) {
	t.Helper()
	ctx := context.Background()
	var seq int64
	if err := tx.QueryRow(ctx, `select coalesce(max(greatest(status_seq, parent_seq, labels_seq)), 0) from dispatch_issues where key = $1`,
		issue.Key).Scan(&seq); err != nil {
		t.Fatalf("read the copy's sequences of %s: %v", issue.Key, err)
	}
	status, parent := issue.DispatchStatus, ""
	if status == "" {
		status = issue.Status
	}
	switch {
	case issue.Parent != nil:
		parent = *issue.Parent
	case issue.Tree != "" && issue.Tree != issue.Key:
		parent = issue.Tree
	}
	labels := handedOverLabels(issue.HandedOver, nil)
	if labels == nil {
		labels = []string{}
	}
	if err := record.NewStore().PutDispatchIssue(ctx, tx, record.DispatchReading{Key: issue.Key, Project: issue.Project,
		Seq: seq + 1, Status: record.Carry(status), Parent: record.Carry(parent), Labels: record.Carry(labels)}); err != nil {
		t.Fatalf("put the copy's row of %s: %v", issue.Key, err)
	}
}

// seedSlotted records key as a running root of its own tree: its record, its admission slot and
// its tree lifecycle, open under the workflow, as admission's promotion writes all three. The slot
// is what says the root runs (record.RunningRoots); the lifecycle is the shadow the release before
// reads (R22 of dispatch://INFRA-3320).
func seedSlotted(t *testing.T, pool *pgxpool.Pool, key, rank string) {
	t.Helper()
	seedSlottedForProject(t, pool, testProject, key, rank)
}

// seedSlottedForProject is seedSlotted under a Dispatch project key of its own, for the tests
// that seed their own records (newAdmissionForProject).
func seedSlottedForProject(t *testing.T, pool *pgxpool.Pool, project, key, rank string) {
	t.Helper()
	seedSlottedInPhase(t, pool, project, key, rank, phase.Admitted, nil)
}

// seedSlottedInPhase is the general form of seedSlotted: the root stands in the phase given
// rather than at admitted, and shape is where a test says what else the record carries. It is
// the package's one slotted-issue fixture, so a test of a started issue in a worker phase seeds
// the record, the slot row and the tree lifecycle exactly as a test of one at admitted does.
func seedSlottedInPhase(t *testing.T, pool *pgxpool.Pool, project, key, rank string, at phase.Phase, shape func(*record.Issue)) {
	t.Helper()
	issue := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: project, Title: key, Tree: key, Phase: at, Generation: 1, Status: "in_progress",
		Rank: rank, HandedOver: true}
	if shape != nil {
		shape(&issue)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutIssue(context.Background(), tx, issue); err != nil {
			t.Fatalf("put active issue: %v", err)
		}
		putCopyTx(t, tx, issue)
		slots, err := records.Slots(context.Background(), tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: key, Index: len(slots), AdmittedAt: fixedNow}); err != nil {
			t.Fatalf("put slot: %v", err)
		}
		seedLifecycleTxForProject(t, tx, project, key)
	})
}

// seedLifecycleTx opens key's tree lifecycle under the workflow's authority, as promotion does
// when it admits a root.
func seedLifecycleTx(t *testing.T, tx pgx.Tx, key string) {
	t.Helper()
	seedLifecycleTxForProject(t, tx, testProject, key)
}

// seedLifecycleTxForProject is seedLifecycleTx under a Dispatch project key of its own.
func seedLifecycleTxForProject(t *testing.T, tx pgx.Tx, project, key string) {
	t.Helper()
	token, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	if _, err := record.NewStore().OpenTreeLifecycle(context.Background(), tx, token, key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the tree lifecycle of %s: %v", key, err)
	}
}

// seedLifecycle opens key's tree lifecycle in a transaction of its own.
func seedLifecycle(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	seedLifecycleForProject(t, pool, testProject, key)
}

// seedLifecycleForProject is seedLifecycle under a Dispatch project key of its own.
func seedLifecycleForProject(t *testing.T, pool *pgxpool.Pool, project, key string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) { seedLifecycleTxForProject(t, tx, project, key) })
}

func seedWaiting(t *testing.T, pool *pgxpool.Pool, key, rank string) {
	t.Helper()
	seedWaitingForProject(t, pool, testProject, key, rank)
}

// seedWaitingForProject is seedWaiting under a Dispatch project key of its own.
func seedWaitingForProject(t *testing.T, pool *pgxpool.Pool, project, key, rank string) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: project, Title: key, Tree: key, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: rank, HandedOver: true})
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

// migratedPool is a pool on a database carrying every migration, cloned from the package's
// template (testpg.ClonePool). The DSN is required rather than skipped: these tests are the real
// Postgres admission tests, and a run that quietly skipped them would prove nothing.
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("LEGION_TEST_PG_DSN") == "" {
		t.Fatal("LEGION_TEST_PG_DSN is required for real Postgres admission tests")
	}
	return testpg.ClonePool(t, "legion_admit_test")
}

func testJetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	js := testnats.JetStream(t)
	testnats.CreateStream(t, js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	return js
}

func reconcile(t *testing.T, pool *pgxpool.Pool, admission *Admission, summaries []dispatch.IssueSummary) {
	t.Helper()
	// target=1 against ack floor 0, not idle, never Reaches: a caller that never measured a real
	// stream position makes no claim of having caught up, so a summary found behind a stored record
	// is deferred exactly as production would, leaving the record alone — matching every test built
	// before Reconcile took a stream position at all. Tests exercising the hold or its release use
	// reconcileWithPosition directly.
	reconcileWithPosition(t, pool, admission, summaries, 1, 0, false)
}

// reconcileWithPosition applies summaries through Reconcile at target and the consumer's position,
// and answers the keys Reconcile found older than an issue fact the daemon applied.
func reconcileWithPosition(t *testing.T, pool *pgxpool.Pool, admission *Admission, summaries []dispatch.IssueSummary, target, ackFloorStream int64, idle bool) []string {
	t.Helper()
	position := intake.DispatchConsumerPosition{AckFloorStream: ackFloorStream, Idle: idle}
	listed := make([]intake.DispatchIssue, len(summaries))
	for i, summary := range summaries {
		listed[i] = intake.ListedIssue(summary)
	}
	// The daemon's own order (daemon's reconcileListing): the copy takes every summary before
	// admission decides anything from it, so admission reads each listed issue's chain from the
	// listing itself (R16 of dispatch://INFRA-3320). A listing Dispatch sends carries each issue's
	// sequence and labels; a fixture's summary that names no sequence is copied at 1, the lowest a
	// reading carries, and one handed over with no labels named carries the legion label
	// (handedOverLabels), so the copy holds the row of every listed issue as it does in production.
	mirrored := slices.Clone(summaries)
	for i := range mirrored {
		mirrored[i].LastSeq = max(mirrored[i].LastSeq, 1)
		mirrored[i].Labels = handedOverLabels(mirrored[i].HandedOver, mirrored[i].Labels)
	}
	var refused []string
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx, settle := intake.OpenSettlers(ctx, admission)
		if err := blockers.Reconcile(ctx, tx, record.NewStore(), admission.project, mirrored); err != nil {
			return fmt.Errorf("mirror the listing: %w", err)
		}
		var err error
		if refused, err = admission.Reconcile(ctx, tx, listed, target, position); err != nil {
			return err
		}
		return settle(ctx, tx)
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return refused
}

// deref is the string a pointer names, "" for nil: the parent a test's fact carries from a record
// that may name none.
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

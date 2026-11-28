package admit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/replay"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// recordProject is the Dispatch project the passes of this file run over, and recordRepository the
// repository each of its issues is recorded on.
const (
	recordProject    = "ACME"
	recordRepository = "acme/widgets"
)

// The kinds of line the trace builder is fed apart: the record line of a pass that moves a slot,
// and the per-slot lines a release before it wrote alone.
const (
	recordLineKind   = "admission: a pass moved slots"
	slotTakenKind    = "admission: slot taken"
	slotReleasedKind = "admission: slot released"
)

// everyRepository resolves every repository to itself on main: the repository these passes record
// their issues on is served.
type everyRepository struct{}

func (everyRepository) ResolveRepository(resolved string) (workflow.Resolution, error) {
	return workflow.Resolution{Repo: resolved, DefaultBranch: "main", GitHubRepo: resolved}, nil
}

// slotFact is what both sources say of a slot moved: the issue, the moment, whether
// it was given out or taken back, and the slots in use under the cap once it moved.
type slotFact struct {
	At         time.Time
	Issue      string
	Given      bool
	Slots, Cap int
}

// slotFacts is the slot facts of a trace, in its order.
func slotFacts(trace replay.Trace) []slotFact {
	facts := make([]slotFact, 0, len(trace.Slots))
	for _, move := range trace.Slots {
		facts = append(facts, slotFact{At: move.At.UTC(), Issue: move.Issue, Given: move.Given, Slots: move.Slots, Cap: move.Cap})
	}
	return facts
}

// The tool reads the record line of each pass that moves a slot where a window holds it, and builds
// the same slot facts from the per-slot lines where a window holds those alone (spec R9).
// Admission passes run on a test store under one clock, through the daemon's own chain of handlers,
// and every line they log is captured through the daemon's JSON handler: a pass that gives out two
// slots and passes over one candidate for the cap and one for a wait, one that takes a slot back and
// gives it out, one that takes two back, one that moves nothing, and one that gives a slot once a
// blocker is done. The trace builder is then fed the record lines apart, the per-slot lines apart,
// and both together: every slot given out or taken back, with its issue, its moment, the slots in
// use and the cap, is the same from each, and the candidates the passes passed over come from the
// record line alone, since no other line names them.
func TestRecordLineAndOtherLinesGiveTheSameSlotFacts(t *testing.T) {
	ctx := context.Background()
	pool := testpg.ClonePool(t, "legion_admit_record_replay")
	records := record.NewStore()
	now := time.Date(2001, time.March, 5, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var logged bytes.Buffer
	// Every line is stamped with the passes' own clock, as a replay stamps its daemon's lines with
	// the simulated one: a pass's per-slot lines and its record line then carry one moment.
	log := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
		if len(groups) == 0 && attr.Key == slog.TimeKey {
			return slog.Time(slog.TimeKey, now)
		}
		return attr
	}}))
	engine := workflow.New(records, workflow.Config{Project: recordProject, Repositories: everyRepository{},
		Steps: config.DesignGate("").Steps(), Clock: clock}, log)
	admission := admit.New(records, engine, 2, recordProject, log)
	admission.UseClock(clock)
	pass := func(at time.Time) {
		t.Helper()
		now = at
		result, err := intake.ApplyFact(ctx, pool, "controller", "the pass at "+at.Format(time.Kitchen), intake.ControllerTick{},
			admit.Handlers(records, recordProject, engine, admission)...)
		if err != nil || result.Refusal != nil {
			t.Fatalf("the pass at %s = %+v, %v", at.Format(time.RFC3339), result.Refusal, err)
		}
	}

	for _, root := range []struct{ key, rank, blocker string }{
		{"ACME-1", "a0", ""}, {"ACME-2", "a1", ""}, {"ACME-3", "a2", ""}, {"ACME-4", "a3", "ACME-9"},
	} {
		queuedRoot(t, pool, root.key, root.rank, root.blocker)
	}
	mirrored(t, pool, "ACME-9", 1, "in_progress")

	// 09:00: ACME-1 and ACME-2 take the cap; ACME-3 waits for it, and ACME-4 waits on ACME-9.
	pass(now)
	// 09:10: ACME-1, past its review, gives its slot back, and ACME-3 takes it.
	pastReview(t, pool, "ACME-1")
	pass(now.Add(10 * time.Minute))
	// 09:20: ACME-2 and ACME-3 give both slots back; ACME-4 still waits on ACME-9.
	pastReview(t, pool, "ACME-2", "ACME-3")
	pass(now.Add(10 * time.Minute))
	// 09:30: nothing moves, and the pass writes no line of a slot.
	pass(now.Add(10 * time.Minute))
	// 09:40: ACME-9 is done, and ACME-4 takes a slot.
	mirrored(t, pool, "ACME-9", 2, "done")
	pass(now.Add(10 * time.Minute))

	lines := capturedLines(t, &logged)
	start := time.Date(2001, time.March, 5, 8, 0, 0, 0, time.UTC)
	traceOf := func(kinds ...string) replay.Trace {
		t.Helper()
		trace, err := replay.BuildTrace(replay.Window{Format: replay.WindowFormat, Project: recordProject, From: start,
			To: start.Add(4 * time.Hour), Lines: kinds, Log: lines})
		if err != nil {
			t.Fatalf("build the trace of the lines %q: %v", kinds, err)
		}
		return trace
	}
	fromRecord, fromSlots, fromBoth := traceOf(recordLineKind), traceOf(slotTakenKind, slotReleasedKind),
		traceOf(recordLineKind, slotTakenKind, slotReleasedKind)

	want := slotFacts(fromSlots)
	if got := slotFacts(fromRecord); !slices.Equal(got, want) {
		t.Errorf("the record lines give the slot facts\n  %+v\nand the per-slot lines\n  %+v", got, want)
	}
	if got := slotFacts(fromBoth); !slices.Equal(got, want) {
		t.Errorf("a window that holds both kinds gives the slot facts\n  %+v\nand the per-slot lines alone\n  %+v", got, want)
	}
	moved := map[string][]bool{}
	for _, fact := range want {
		moved[fact.Issue] = append(moved[fact.Issue], fact.Given)
	}
	wantMoved := map[string][]bool{"ACME-1": {true, false}, "ACME-2": {true, false}, "ACME-3": {true, false}, "ACME-4": {true}}
	if !maps.EqualFunc(moved, wantMoved, slices.Equal[[]bool]) {
		t.Errorf("the passes moved the slots %v (true given out, false taken back), want %v: the passes did not run as this test says",
			moved, wantMoved)
	}
	var after []int
	for i, fact := range want {
		if fact.Cap != 2 {
			t.Errorf("the slot fact %+v names the cap %d, want 2", fact, fact.Cap)
		}
		if i+1 == len(want) || !want[i+1].At.Equal(fact.At) {
			after = append(after, fact.Slots)
		}
	}
	if !slices.Equal(after, []int{2, 2, 0, 1}) {
		t.Errorf("the slots in use after each pass that moved one are %v, want [2 2 0 1]", after)
	}
	for _, move := range fromRecord.Slots {
		if move.Given && move.Kind == "" || !move.Given && move.Reason == "" {
			t.Errorf("the record line gives the slot move %+v without its kind of entry or the path that took it back", move)
		}
	}

	if len(fromSlots.Passed) != 0 {
		t.Errorf("the per-slot lines give the candidates passed over %+v; no line but the record line names one", fromSlots.Passed)
	}
	passed := map[string][]record.SlotPassedReason{}
	for _, candidate := range fromRecord.Passed {
		passed[candidate.Issue] = append(passed[candidate.Issue], candidate.Reason)
		if candidate.Place < 1 || candidate.Kind == "" {
			t.Errorf("the record line names the candidate %+v without its place or its kind", candidate)
		}
	}
	if !slices.Contains(passed["ACME-3"], record.PassedCap) || !slices.Contains(passed["ACME-4"], record.PassedWaits) {
		t.Errorf("the record lines name the candidates passed over %v; want ACME-3 passed for the cap and ACME-4 for its wait", passed)
	}
	if !slices.Equal(fromBoth.Passed, fromRecord.Passed) {
		t.Errorf("a window that holds both kinds gives the candidates %+v, and the record lines alone %+v", fromBoth.Passed, fromRecord.Passed)
	}
}

// capturedLines is every line logged holds, each as the daemon's JSON handler wrote it.
func capturedLines(t *testing.T, logged *bytes.Buffer) []json.RawMessage {
	t.Helper()
	var lines []json.RawMessage
	for _, line := range bytes.Split(logged.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !json.Valid(line) {
			t.Fatalf("the logged line %q is no JSON object", line)
		}
		lines = append(lines, json.RawMessage(slices.Clone(line)))
	}
	return lines
}

// recordTx runs fn in one transaction on pool, committed.
func recordTx(t *testing.T, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, fn); err != nil {
		t.Fatal(err)
	}
}

// queuedRoot records key as a root handed to Legion that has never run, waiting at todo for its tree
// to be opened at rank, with its mirror row naming blocker as what it waits on where one is given.
func queuedRoot(t *testing.T, pool *pgxpool.Pool, key, rank, blocker string) {
	t.Helper()
	var blockers []string
	if blocker != "" {
		blockers = []string{blocker}
	}
	recordTx(t, pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: key, Tree: key, Project: recordProject, Title: key,
			Repo: recordRepository, DefaultBranch: "main", GitHubRepo: recordRepository, Phase: phase.Admitted, Generation: 1,
			Status: "todo", DispatchStatus: "todo", Rank: rank, HandedOver: true, LastDispatchSeq: 1}); err != nil {
			return fmt.Errorf("record %s: %w", key, err)
		}
		return records.PutDispatchIssue(context.Background(), tx, record.DispatchReading{Key: key, Project: recordProject, Seq: 1,
			Status: record.Carry("todo"), Parent: record.Carry(""), Blockers: record.Carry(blockers), Rank: record.Carry(rank)})
	})
}

// mirrored writes what Dispatch shows of key, an issue no tree runs, at seq: its status.
func mirrored(t *testing.T, pool *pgxpool.Pool, key string, seq int64, status string) {
	t.Helper()
	recordTx(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(context.Background(), tx, record.DispatchReading{Key: key, Project: recordProject,
			Seq: seq, Status: record.Carry(status), Parent: record.Carry(""), Blockers: record.Carry([]string{})})
	})
}

// pastReview moves each of keys past its review, into accepting, where the daemon's rule keeps no
// slot.
func pastReview(t *testing.T, pool *pgxpool.Pool, keys ...string) {
	t.Helper()
	recordTx(t, pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		for _, key := range keys {
			issue, err := records.Issue(context.Background(), tx, key)
			if err != nil || issue == nil {
				return fmt.Errorf("read %s: %v", key, err)
			}
			issue.Phase, issue.Status, issue.DispatchStatus = phase.Accepting, "needs_review", "needs_review"
			if err := records.PutIssue(context.Background(), tx, *issue); err != nil {
				return fmt.Errorf("move %s past its review: %w", key, err)
			}
		}
		return nil
	})
}

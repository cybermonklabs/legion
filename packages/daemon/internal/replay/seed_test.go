package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// seedStore is a store on a database of its own carrying every migration, closed and dropped when
// the test ends (testpg.CloneStore): the replay's own database, which a start state is written
// into.
func seedStore(t *testing.T) *store.Store {
	t.Helper()
	return testpg.CloneStore(t, "legion_replay_seed")
}

// placedTrace is w's trace, which must place every fact of w.
func placedTrace(t *testing.T, w Window) Trace {
	t.Helper()
	trace, err := BuildTrace(w)
	if err != nil {
		t.Fatalf("build the trace: %v", err)
	}
	if len(trace.Unplaced) != 0 {
		t.Errorf("the trace could not place %+v", trace.Unplaced)
	}
	return trace
}

// seedTrace writes trace's start state at at in a transaction of st's with SeedAndReport and hands
// the transaction to read, which reads back what it wrote; the transaction is then rolled back, so
// one database serves every case of a test. The engine it seeds through is the plain one of the
// trace's project, as the replay's is but for the window's own settings: it composes the resume
// task of each owed start seeded (workflow.Engine.ResumeTask), which reads neither. It answers the
// start state's report.
func seedTrace(t *testing.T, st *store.Store, trace Trace, at time.Time, read func(tx pgx.Tx)) SeedReport {
	t.Helper()
	ctx := context.Background()
	tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the seed's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	engine := workflow.New(record.NewStore(), workflow.Config{Project: trace.Project}, nil)
	report, err := SeedAndReport(ctx, tx, record.NewStore(), st, engine, trace, at)
	if err != nil {
		t.Fatalf("seed the state at %s: %v", at.Format(time.RFC3339), err)
	}
	if read != nil {
		read(tx)
	}
	return report
}

// seedReported builds w's trace and writes its start state at at (seedTrace). It answers the trace
// and the start state's report.
func seedReported(t *testing.T, st *store.Store, w Window, at time.Time, read func(tx pgx.Tx)) (Trace, SeedReport) {
	t.Helper()
	trace, err := BuildTrace(w)
	if err != nil {
		t.Fatalf("build the trace: %v", err)
	}
	return trace, seedTrace(t, st, trace, at, read)
}

// seededSlots is each slot a start state wrote, read in tx in its order, as its holder and the
// minute it was given: KEY@15:04.
func seededSlots(t *testing.T, tx pgx.Tx) []string {
	t.Helper()
	slots, err := record.NewStore().Slots(context.Background(), tx)
	if err != nil {
		t.Fatalf("read the seeded slots: %v", err)
	}
	var holders []string
	for _, slot := range slots {
		holders = append(holders, slot.Issue+"@"+slot.AdmittedAt.UTC().Format("15:04"))
	}
	return holders
}

// promotedRoot files key at at as a root handed to Legion at rank and promotes it there.
func promotedRoot(b *windowBuilder, at time.Time, key, rank string) {
	b.t.Helper()
	b.file(at, key, filing{Title: "Ship the " + key + " change", Status: "todo", Rank: rank, Labels: handedToLegion})
	b.rootPromoted(at, key)
}

// rootThrough files key at filed as a root handed to Legion at rank, promotes it there, ends its
// architect's first turn at designed with its design registered and its first phase begun, and then
// ends each segment of moves in turn: the record of a root that runs from its filing.
func rootThrough(b *windowBuilder, key, rank string, filed, designed time.Time, moves ...builtMove) {
	b.t.Helper()
	promotedRoot(b, filed, key, rank)
	b.rootDesigned(designed, key)
	b.through(key, phase.Planning, moves...)
}

// builtMove is one recorded move a root makes in rootThrough: its moment, the phase it enters and
// its trigger.
type builtMove struct {
	at      time.Time
	to      phase.Phase
	trigger workflow.TriggerKind
}

// moveAt is the builtMove into to by trigger at hour:minute.
func moveAt(hour, minute int, to phase.Phase, trigger workflow.TriggerKind) builtMove {
	return builtMove{clock(hour, minute), to, trigger}
}

// startStateWindow is a window that opens at 13:00 with an issue in each phase: its inputs reach
// back to the daemon's boot at 08:00, its cap is 7, and every slot is taken at 13:00. ACME-1 is a
// root its architect works at admitted; ACME-2 to ACME-8 stand in planning, implementing, testing,
// reviewing, accepting, retro and merging, each worker in a turn; ACME-9 is held from
// implementing, where its implementer's claim failed at 11:10; ACME-10 is a root waiting on a
// person's answer to its architect's question since 10:45; ACME-11 gave its slot back at 12:25
// for a link to ACME-12, an issue Legion does not work, and moved on to reviewing at 12:40 with no
// slot, so it owes its reviewer's start; ACME-13 is a root handed to Legion at 12:40 that waits
// on ACME-12; and ACME-14 was sent back from accepting at 12:50 with every slot taken, so it owes
// its implementer's start. It answers the window and the Dispatch sequence of ACME-10's question.
func startStateWindow(t *testing.T) (Window, int64) {
	b := newWindowBuilder(t, clock(13, 0), clock(16, 0))
	b.reaches(5 * time.Hour)
	b.capped(7)
	b.boot(clock(8, 0))
	b.file(clock(7, 30), "ACME-12", filing{Title: "Rotate the warehouse keys", Status: "in_progress", Rank: "z0"})
	impl := func(at time.Time) builtMove {
		return builtMove{at, phase.Implementing, workflow.TriggerPlannerCompleted}
	}
	test := func(at time.Time) builtMove { return builtMove{at, phase.Testing, workflow.TriggerImplementationReady} }
	review := func(at time.Time) builtMove { return builtMove{at, phase.Reviewing, workflow.TriggerTesterPassed} }
	accept := func(at time.Time) builtMove { return builtMove{at, phase.Accepting, workflow.TriggerReviewApproved} }
	retro := func(at time.Time) builtMove { return builtMove{at, phase.Retro, workflow.TriggerSignOff} }
	merge := func(at time.Time) builtMove { return builtMove{at, phase.Merging, workflow.TriggerRetroCompleted} }

	rootThrough(b, "ACME-2", "a02", clock(8, 5), clock(8, 20))
	rootThrough(b, "ACME-3", "a03", clock(8, 10), clock(8, 25), impl(clock(8, 45)))
	rootThrough(b, "ACME-4", "a04", clock(8, 15), clock(8, 30), impl(clock(8, 50)), test(clock(9, 10)))
	rootThrough(b, "ACME-5", "a05", clock(8, 20), clock(8, 35), impl(clock(8, 55)), test(clock(9, 15)), review(clock(9, 35)))
	rootThrough(b, "ACME-6", "a06", clock(8, 25), clock(8, 40), impl(clock(9, 0)), test(clock(9, 20)), review(clock(9, 40)),
		accept(clock(10, 0)))
	rootThrough(b, "ACME-7", "a07", clock(8, 30), clock(8, 45), impl(clock(9, 5)), test(clock(9, 25)), review(clock(9, 45)),
		accept(clock(10, 5)), retro(clock(10, 25)))
	rootThrough(b, "ACME-8", "a08", clock(8, 35), clock(8, 50), impl(clock(9, 10)), test(clock(9, 30)), review(clock(9, 50)),
		accept(clock(10, 10)), retro(clock(10, 30)), merge(clock(10, 50)))
	rootThrough(b, "ACME-9", "a09", clock(10, 15), clock(10, 30), impl(clock(10, 50)))
	b.claimFailed(clock(11, 10), "ACME-9", claim.RoleImplementer)

	promotedRoot(b, clock(10, 20), "ACME-10", "a10")
	architect := b.session("ACME-10", claim.RoleArchitect)
	b.event(clock(10, 40), "ACME-10", "ask.opened", architect, map[string]any{
		"id": "ask-10", "issue_key": "ACME-10", "author": architect, "kind": record.AskQuestion, "state": "open"})
	asked := b.seq("ACME-10")
	b.turnEnds(clock(10, 45), "ACME-10", claim.RoleArchitect)
	b.info(clock(10, 45), "workflow: root waits on a person and keeps its admission slot", "issue", "ACME-10", "tree", "ACME-10",
		"reason", "question ask-10 is awaiting an answer", "since_seq", asked)

	rootThrough(b, "ACME-14", "a14", clock(10, 35), clock(10, 50), impl(clock(11, 5)), test(clock(11, 20)), review(clock(11, 35)),
		accept(clock(11, 50)))
	rootThrough(b, "ACME-11", "a11", clock(11, 51), clock(12, 0), impl(clock(12, 10)), test(clock(12, 20)))
	b.edit(clock(12, 25), "ACME-11", func(issue *builtIssue) { issue.BlockedBy = []string{"ACME-12"} })
	b.slotReleased(clock(12, 25), "ACME-11")
	promotedRoot(b, clock(12, 30), "ACME-1", "a01")
	b.turnEnds(clock(12, 40), "ACME-11", claim.RoleTester)
	b.moved(clock(12, 40), "ACME-11", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.file(clock(12, 40), "ACME-13", filing{Title: "Ship the ACME-13 change", Status: "todo", Rank: "a13", Labels: handedToLegion,
		BlockedBy: []string{"ACME-12"}})
	b.turnEnds(clock(12, 50), "ACME-14", claim.RoleArchitect)
	b.moved(clock(12, 50), "ACME-14", phase.Accepting, phase.Implementing, workflow.TriggerBackward)
	return b.build(), asked
}

// The replay starts a window from the state its record shows at the window's start (assumption 6),
// written into its own database by Seed from the trace of the lines and events before that
// moment. A window that opens with an issue in each phase — a root its architect works at
// admitted, one issue in each worker phase from planning to merging, a held issue, a root waiting
// on a person, a started issue that gave its slot back to wait on a link, a queued root a link
// holds, and an issue sent back at a full cap — is seeded at its start and the store is read back:
// each issue's record (its phase, its hold, its wait on a person, its tree, generation, rank, the
// legion label and its mark of a slot given back for a wait), the slots in the order they were given
// out, the Dispatch mirror rows, the design gates its roots registered, the tree lifecycles of the
// trees that run, the claims of the turns under way, working and with a confirmed delivery, the
// starts the two issues whose work came back owe, waiting for a slot from the moment it came back,
// and the pull requests of the issues past implementing. The trace places every line and the seed
// every issue.
func TestSeedPlacesTheStartState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := seedStore(t)
	window, asked := startStateWindow(t)
	at := window.From
	records := record.NewStore()

	trace, report := seedReported(t, st, window, at, func(tx pgx.Tx) {
		wantPhases := map[string]phase.Phase{
			"ACME-1": phase.Admitted, "ACME-2": phase.Planning, "ACME-3": phase.Implementing, "ACME-4": phase.Testing,
			"ACME-5": phase.Reviewing, "ACME-6": phase.Accepting, "ACME-7": phase.Retro, "ACME-8": phase.Merging,
			"ACME-9": phase.Held, "ACME-10": phase.AwaitingPerson, "ACME-11": phase.Reviewing, "ACME-13": phase.Admitted,
			"ACME-14": phase.Implementing,
		}
		for _, key := range slices.Sorted(maps.Keys(wantPhases)) {
			issue, err := records.Issue(ctx, tx, key)
			if err != nil || issue == nil {
				t.Errorf("read the seeded record of %s: %v (none: %t)", key, err, issue == nil)
				continue
			}
			if issue.Phase != wantPhases[key] {
				t.Errorf("%s is seeded in %s, want %s", key, issue.Phase, wantPhases[key])
			}
			if issue.Tree != key || issue.Generation != 1 || !issue.HandedOver || issue.Project != builtProject ||
				issue.Repo != builtRepository || issue.DefaultBranch != defaultBranch {
				t.Errorf("%s is seeded in the tree %s at generation %d, handed over %t, of project %s on %s at %s; want its own tree at generation 1, handed over, of %s on %s at %s",
					key, issue.Tree, issue.Generation, issue.HandedOver, issue.Project, issue.Repo, issue.DefaultBranch, builtProject,
					builtRepository, defaultBranch)
			}
			if number, _ := strconv.Atoi(strings.TrimPrefix(key, "ACME-")); issue.Rank != fmt.Sprintf("a%02d", number) {
				t.Errorf("%s is seeded at the rank %q", key, issue.Rank)
			}
			switch key {
			case "ACME-9":
				if issue.Hold == nil || issue.Hold.From != phase.Implementing || issue.Hold.Since == nil || !issue.Hold.Since.Equal(clock(11, 10)) {
					t.Errorf("ACME-9 is seeded with the hold %+v, want one from implementing since 11:10", issue.Hold)
				}
			case "ACME-10":
				if issue.Awaiting == nil || issue.Awaiting.Ask != "ask-10" || issue.Awaiting.Since.Seq != asked ||
					!issue.Awaiting.Since.At.Equal(clock(10, 40)) || issue.Awaiting.AnsweredAt != nil {
					t.Errorf("ACME-10 is seeded waiting on %+v, want the unanswered ask ask-10 since its event %d at 10:40", issue.Awaiting, asked)
				}
				ask, err := records.RootAsk(ctx, tx, "ACME-10", "ask-10")
				if err != nil || ask == nil || !ask.Open || !ask.WithHuman() || ask.Kind != record.AskQuestion || !ask.ByTree || ask.Seq != asked {
					t.Errorf("the ask ACME-10 waits on is mirrored as %+v (%v), want its architect's open question on the person's turn since %d",
						ask, err, asked)
				}
			case "ACME-11":
				if issue.ReleasedForWaitAt == nil || !issue.ReleasedForWaitAt.Equal(clock(12, 25)) {
					t.Errorf("ACME-11 is seeded with the slot it gave back for its wait at %v, want 12:25", issue.ReleasedForWaitAt)
				}
			default:
				if issue.Hold != nil || issue.Awaiting != nil || issue.ReleasedForWaitAt != nil {
					t.Errorf("%s is seeded with the hold %+v, the wait %+v and the release for a wait %v, want none", key, issue.Hold,
						issue.Awaiting, issue.ReleasedForWaitAt)
				}
			}
		}
		if issue, err := records.Issue(ctx, tx, "ACME-12"); err != nil || issue != nil {
			t.Errorf("ACME-12, which Legion does not work, is seeded with the record %+v (%v), want none", issue, err)
		}

		wantHolders := []string{"ACME-2@08:05", "ACME-3@08:10", "ACME-4@08:15", "ACME-5@08:20", "ACME-9@10:15", "ACME-10@10:20", "ACME-1@12:30"}
		if holders := seededSlots(t, tx); !slices.Equal(holders, wantHolders) {
			t.Errorf("the seeded slots are %v, want %v", holders, wantHolders)
		}

		keys := []string{"ACME-1", "ACME-2", "ACME-3", "ACME-4", "ACME-5", "ACME-6", "ACME-7", "ACME-8", "ACME-9", "ACME-10", "ACME-11",
			"ACME-12", "ACME-13", "ACME-14"}
		rows, err := records.DispatchIssuesOf(ctx, tx, keys)
		if err != nil {
			t.Fatalf("read the seeded mirror rows: %v", err)
		}
		mirrored := map[string]record.DispatchIssueRow{}
		for _, row := range rows {
			mirrored[row.Key] = row
		}
		for _, key := range keys {
			if _, ok := mirrored[key]; !ok {
				t.Errorf("%s has no seeded mirror row", key)
			}
		}
		for _, key := range []string{"ACME-11", "ACME-13"} {
			if !slices.Equal(mirrored[key].Blockers, []string{"ACME-12"}) {
				t.Errorf("the mirror row of %s waits on %v, want [ACME-12]", key, mirrored[key].Blockers)
			}
		}
		if mirrored["ACME-12"].Status != "in_progress" || mirrored["ACME-1"].Status != "in_progress" || mirrored["ACME-13"].Status != "todo" {
			t.Errorf("the mirror rows show ACME-12 %s, ACME-1 %s and ACME-13 %s, want in_progress, in_progress and todo",
				mirrored["ACME-12"].Status, mirrored["ACME-1"].Status, mirrored["ACME-13"].Status)
		}

		gates, err := records.Gates(ctx, tx, keys)
		if err != nil {
			t.Fatalf("read the seeded gates: %v", err)
		}
		for _, key := range keys {
			gate, registered := gates[key][record.DesignStepSpec]
			wantOpen := !slices.Contains([]string{"ACME-1", "ACME-10", "ACME-12", "ACME-13"}, key)
			if registered != wantOpen || registered && !record.GateOpen(gate) {
				t.Errorf("%s is seeded with the spec gate %+v (registered %t), want an open one: %t", key, gate, registered, wantOpen)
			}
		}

		lifecycles, err := tx.Query(ctx, "select tree from tree_lifecycles")
		if err != nil {
			t.Fatalf("read the seeded tree lifecycles: %v", err)
		}
		trees, err := pgx.CollectRows(lifecycles, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the seeded tree lifecycles: %v", err)
		}
		slices.Sort(trees)
		wantTrees := []string{"ACME-1", "ACME-10", "ACME-11", "ACME-14", "ACME-2", "ACME-3", "ACME-4", "ACME-5", "ACME-6", "ACME-7", "ACME-8", "ACME-9"}
		if !slices.Equal(trees, wantTrees) {
			t.Errorf("the seeded tree lifecycles are %v, want %v", trees, wantTrees)
		}

		claims, err := st.ClaimsTx(ctx, tx)
		if err != nil {
			t.Fatalf("read the seeded claims: %v", err)
		}
		var working []string
		for _, c := range claims {
			working = append(working, string(c.Token))
			if c.State != supervise.StateWorking || c.Pending == nil || c.Pending.ConfirmedAt.IsZero() || !c.LastTurnStart.Equal(c.Pending.ConfirmedAt) ||
				c.TreeEpoch == 0 || c.Generation == 0 {
				t.Errorf("the claim %s is seeded %s since %s with the delivery %+v at tree epoch %d and generation %d, want working in its turn with a delivery confirmed as the turn began",
					c.Token, c.State, c.LastTurnStart, c.Pending, c.TreeEpoch, c.Generation)
			}
		}
		slices.Sort(working)
		var wantWorking []string
		for key, role := range map[string]claim.Role{"ACME-1": claim.RoleArchitect, "ACME-2": claim.RolePlanner, "ACME-3": claim.RoleImplementer,
			"ACME-4": claim.RoleTester, "ACME-5": claim.RoleReviewer, "ACME-6": claim.RoleArchitect, "ACME-7": claim.RoleImplementer,
			"ACME-8": claim.RoleMerger} {
			wantWorking = append(wantWorking, string(builtToken(t, key, role)))
		}
		slices.Sort(wantWorking)
		if !slices.Equal(working, wantWorking) {
			t.Errorf("the seeded claims are %v, want the claims of the turns under way %v", working, wantWorking)
		}

		owed, err := records.OwedStarts(ctx, tx, builtProject)
		if err != nil {
			t.Fatalf("read the seeded owed starts: %v", err)
		}
		var starts []string
		for _, start := range owed {
			starts = append(starts, fmt.Sprintf("%s %s %s %s", start.Issue, start.Role, start.Phase, start.CreatedAt.UTC().Format("15:04")))
		}
		wantStarts := []string{"ACME-11 reviewer reviewing 12:40", "ACME-14 implementer implementing 12:50"}
		if !slices.Equal(starts, wantStarts) {
			t.Errorf("the seeded starts owed for a slot are %q, want %q", starts, wantStarts)
		}

		prs, err := records.PullRequestsOf(ctx, tx, keys)
		if err != nil {
			t.Fatalf("read the seeded pull requests: %v", err)
		}
		var opened []string
		for key, pr := range prs {
			opened = append(opened, key)
			if pr.State != record.PullRequestOpen || pr.Branch != claim.IssueBranch(key) || pr.Repo != builtRepository {
				t.Errorf("the pull request of %s is seeded %+v, want an open one on its own branch of %s", key, pr, builtRepository)
			}
		}
		slices.Sort(opened)
		wantOpened := []string{"ACME-11", "ACME-14", "ACME-4", "ACME-5", "ACME-6", "ACME-7", "ACME-8"}
		if !slices.Equal(opened, wantOpened) {
			t.Errorf("the seeded pull requests are of %v, want %v: each issue the record showed past implementing", opened, wantOpened)
		}
	})
	if len(trace.Unplaced) != 0 {
		t.Errorf("the trace could not place %+v", trace.Unplaced)
	}
	// What the seed read those states from: ACME-11's wait on its link and the slot it gave back for
	// it, ACME-9's hold, ACME-10's wait on a person, and ACME-14's segment in accepting, ended by the
	// architect's send-back.
	waiter := trace.Issues["ACME-11"]
	if !slices.ContainsFunc(waiter.Waits, func(w Wait) bool {
		return w.Kind == WaitLink && w.Start.Equal(clock(12, 25)) && w.EndedBy == WaitOpen && slices.Equal(w.On, []string{"ACME-12"})
	}) || !slices.ContainsFunc(waiter.Slots, func(s SlotSpan) bool { return s.End.Equal(clock(12, 25)) && s.ForWait }) {
		t.Errorf("the trace holds ACME-11's waits %+v and slots %+v, want a link wait on ACME-12 from 12:25, open, and the slot it gave back for it",
			waiter.Waits, waiter.Slots)
	}
	for key, kind := range map[string]WaitKind{"ACME-9": WaitHeld, "ACME-10": WaitPerson} {
		if !slices.ContainsFunc(trace.Issues[key].Waits, func(w Wait) bool { return w.Kind == kind && w.EndedBy == WaitOpen }) {
			t.Errorf("the trace holds %s's waits %+v, want a %s wait that stands at the window's end", key, trace.Issues[key].Waits, kind)
		}
	}
	sentBack := trace.Issues["ACME-14"]
	if i := slices.IndexFunc(sentBack.Visits, func(v Visit) bool { return v.Phase == phase.Accepting }); i < 0 || sentBack.Visits[i].Segment == nil ||
		!sentBack.Visits[i].Segment.Start.Equal(clock(11, 50)) || !sentBack.Visits[i].Segment.End.Equal(clock(12, 50)) ||
		sentBack.Visits[i].Out == nil || sentBack.Visits[i].Out.Cause != CauseSendBack {
		t.Errorf("the trace holds ACME-14's visits %+v, want its segment in accepting from 11:50 to the send-back at 12:50", sentBack.Visits)
	}
	if unplaced := report.Unplaced; len(unplaced) != 0 {
		t.Errorf("the seed could not place %+v", unplaced)
	}
}

// withoutRepository is w with every issue event of key naming no repository, resolved or not.
func withoutRepository(t *testing.T, w Window, key string) Window {
	t.Helper()
	events := make([]json.RawMessage, 0, len(w.Events[key]))
	for _, raw := range w.Events[key] {
		var event map[string]any
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("read an event of %s: %v", key, err)
		}
		if payload, ok := event["payload"].(map[string]any); ok && strings.HasPrefix(event["type"].(string), "issue.") {
			payload["resolved_repo"], payload["repo"] = nil, nil
		}
		changed, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("write an event of %s: %v", key, err)
		}
		events = append(events, changed)
	}
	w.Events = maps.Clone(w.Events)
	w.Events[key] = events
	return w
}

// Seed names each issue it cannot place at the window's start, with the reason, and places every
// other: a root held from merging with no slot, which the daemon's own reading of the line holds by
// no word (record.HeldUnclassified); a root Dispatch names no repository of, which the daemon
// records no issue without; a root whose phase line the trace could not place before the start (a
// move out of testing while the trace holds it in implementing); and a root whose architect's
// turn stands at the start in a tree the record never opened. No issue of a fixture window is
// unplaced, at its start or at any hour of it, and the trace of each places every line.
func TestSeedNamesWhatItCannotPlace(t *testing.T) {
	t.Parallel()
	st := seedStore(t)
	cases := []struct {
		name   string
		window func(t *testing.T) Window
		issue  string
		reason string
	}{
		{name: "a phase held from past the review with no slot", window: unclassifiedWindow, issue: "ACME-1",
			reason: string(record.HeldUnclassified)},
		{name: "an issue Dispatch names no repository of", issue: "ACME-1", reason: "no repository", window: func(t *testing.T) Window {
			b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
			b.reaches(2 * time.Hour)
			b.boot(clock(8, 0))
			rootThrough(b, "ACME-1", "a0", clock(8, 5), clock(8, 20), builtMove{clock(9, 0), phase.Implementing, workflow.TriggerPlannerCompleted})
			promotedRoot(b, clock(8, 10), "ACME-2", "a1")
			return withoutRepository(t, b.build(), "ACME-1")
		}},
		{name: "a line the trace could not place before the start", issue: "ACME-1", reason: "the trace could not place",
			window: func(t *testing.T) Window {
				b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
				b.reaches(2 * time.Hour)
				b.boot(clock(8, 0))
				rootThrough(b, "ACME-1", "a0", clock(8, 5), clock(8, 20), builtMove{clock(9, 0), phase.Implementing, workflow.TriggerPlannerCompleted})
				b.moved(clock(9, 30), "ACME-1", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
				return b.build()
			}},
		{name: "a turn in a tree the record never opened", issue: "ACME-2", reason: "runs no lifecycle", window: func(t *testing.T) Window {
			b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
			b.reaches(2 * time.Hour)
			b.capped(1)
			b.boot(clock(8, 0))
			rootThrough(b, "ACME-1", "a0", clock(8, 5), clock(8, 20))
			b.file(clock(8, 10), "ACME-2", filing{Title: "Ship the ACME-2 change", Status: "todo", Rank: "a1", Labels: handedToLegion})
			// The architect's claim launched before the inputs reach: a fresh launch of it in the
			// run would show the tree's lifecycle open (seeder.lifecycles).
			b.launchedBefore("ACME-2", claim.RoleArchitect)
			b.turnStarts(clock(9, 0), "ACME-2", claim.RoleArchitect)
			return b.build()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			window := tc.window(t)
			_, report := seedReported(t, st, window, window.From, nil)
			unplaced := report.Unplaced
			named := false
			for _, issue := range unplaced {
				switch {
				case issue.Issue != tc.issue:
					t.Errorf("the seed names %s, which it can place: %s", issue.Issue, issue.Reason)
				case issue.Reason == "":
					t.Errorf("the seed names %s and says no reason", issue.Issue)
				case strings.Contains(issue.Reason, tc.reason):
					named = true
				}
			}
			if !named {
				t.Errorf("the seed names %+v; want %s named for a reason that says %q", unplaced, tc.issue, tc.reason)
			}
		})
	}

	for _, name := range fixtureWindowNames(t) {
		t.Run("the fixture window "+name, func(t *testing.T) {
			window := fixtureWindow(t, name)
			trace, err := BuildTrace(window)
			if err != nil {
				t.Fatalf("build the trace: %v", err)
			}
			for at := window.From; at.Before(window.To); at = at.Add(time.Hour) {
				report := seedTrace(t, st, trace, at, nil)
				if at.Equal(window.From) && len(trace.Unplaced) != 0 {
					t.Errorf("the trace could not place %+v", trace.Unplaced)
				}
				if len(report.Unplaced) != 0 {
					t.Errorf("seeded at %s, the start state could not place %+v", at.Format("15:04"), report.Unplaced)
				}
			}
		})
	}
}

// A fact the trace could not place before the window's start leaves in doubt only what of the
// start state it bears on. ACME-1 and ACME-2 run from their filing to implementing, and at 09:30,
// before the start at 10:00, the log holds two lines the trace cannot place: the turn end of
// ACME-1's tester, which has no turn open, and an admission pass giving ACME-2 a slot while it
// holds one. The first is of a turn that ended before the start, which bears on nothing the start
// state writes, so ACME-1 is placed; the second leaves ACME-2's slot in doubt, so the seed names
// ACME-2, with the reason. The trace holds the two facts, each of its kind.
func TestSeedNamesAnIssueOnlyForAFactItsStartStateRestsOn(t *testing.T) {
	t.Parallel()
	st := seedStore(t)
	b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
	b.reaches(2 * time.Hour)
	b.boot(clock(8, 0))
	implementing := builtMove{clock(9, 0), phase.Implementing, workflow.TriggerPlannerCompleted}
	rootThrough(b, "ACME-1", "a0", clock(8, 5), clock(8, 20), implementing)
	rootThrough(b, "ACME-2", "a1", clock(8, 10), clock(8, 25), implementing)
	b.info(clock(9, 30), "supervise: a turn ended", "claim", string(b.token("ACME-1", claim.RoleTester)), "generation", 1,
		"delivery", "outbox:99")
	b.info(clock(9, 30), "admission: slot taken", "issue", "ACME-2", "slots", 3, "cap", 4)
	window := b.build()
	trace, report := seedReported(t, st, window, window.From, nil)
	unplaced := report.Unplaced

	facts := make(map[string]UnplacedKind)
	for _, fact := range trace.Unplaced {
		if !fact.At.Equal(clock(9, 30)) {
			t.Errorf("the trace could not place %+v, which is no line of 09:30", fact)
		}
		facts[fact.Issue] = fact.Kind
	}
	if want := map[string]UnplacedKind{"ACME-1": UnplacedTurn, "ACME-2": UnplacedSlot}; !maps.Equal(facts, want) {
		t.Errorf("the trace could not place %+v; want a turn fact of ACME-1 and a slot fact of ACME-2", trace.Unplaced)
	}
	if len(unplaced) != 1 || unplaced[0].Issue != "ACME-2" || !strings.Contains(unplaced[0].Reason, "the trace could not place") ||
		!strings.Contains(unplaced[0].Reason, "takes a slot") {
		t.Errorf("the seed names %+v; want ACME-2 alone, for the slot the trace could not place", unplaced)
	}
}

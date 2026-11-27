package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// issueFactsChange is one delivery_changes row of the stored per-issue Legion facts: the project
// in Repo and the issue in Ref, with the issue's close before and after the change (migration
// 20261117100002).
func issueFactsChange(project, issue string, closedBefore, closedAfter time.Time) delivery.Change {
	return delivery.Change{Seq: 1, Kind: "issue_facts", Repo: project, Ref: issue, TOld: &closedBefore, TNew: &closedAfter}
}

// boardViewShowing is a Legion board of project naming the issues given, as a recompute's answer
// leaves them.
func boardViewShowing(project string, issues ...string) *deliveryView {
	return viewShowingIssues(deliveryLiveSpec{kind: legionBoardView, project: project}, issues)
}

// deliveryViewShowing is a view that reads the deployment's delivery rows, naming the issues
// given. Its window is the last seven days, which this change's kind never reads.
func deliveryViewShowing(kind string, issues ...string) *deliveryView {
	return viewShowingIssues(deliveryLiveSpec{kind: kind, preset: 7}, issues)
}

func viewShowingIssues(spec deliveryLiveSpec, issues []string) *deliveryView {
	view := &deliveryView{spec: spec, issues: map[string]struct{}{}}
	for _, issue := range issues {
		view.issues[issue] = struct{}{}
	}
	return view
}

// TestAnIssueFactsChangeTouchesTheLegionBoardOfItsProject: a write of one issue's stored Legion
// facts moves the Legion board of that issue's project and no other view at all. The board's read
// takes the rows of the issues it shows, every issue of each tree a card stands in among them
// (legionfacts.Read), so a row of an issue the board names nowhere yet - a new member of a shown
// tree, the first row of an issue, a row a build's whole read writes - moves its answer as much
// as a row of an issue it already shows does, and a board that marked only the issues it names
// would stand behind its GET until some other write woke it.
//
// No delivery view is woken, whichever issue it names: the stored facts have one reader
// (legionfacts.Read, which the board calls), so no timeline, measures or pipeline answer can
// move when a facts row does. That matters on a burst: a build's whole read of a project rewrites
// every row whose values or writer moved, and the build a project's record names computes again
// every row the other build of a roll wrote, and each rewrite logs a change, so one such pass
// would otherwise force a whole Delivery recompute on every subscribed page.
func TestAnIssueFactsChangeTouchesTheLegionBoardOfItsProject(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	change := issueFactsChange("INFRA", "INFRA-7", now.Add(-26*time.Hour), now.Add(-2*time.Hour))

	tests := []struct {
		what string
		view *deliveryView
		want bool
	}{
		{"a Legion board of the project that shows the issue", boardViewShowing("INFRA", "INFRA-7", "INFRA-8"), true},
		{"a Legion board of the project that shows other issues", boardViewShowing("INFRA", "INFRA-8"), true},
		{"a Legion board of the project that shows none", boardViewShowing("INFRA"), true},
		{"a Legion board of another project", boardViewShowing("OPS", "OPS-7"), false},
		{"a timeline that shows the issue", deliveryViewShowing(deliveryLiveTimeline, "INFRA-7"), false},
		{"a timeline that shows other issues", deliveryViewShowing(deliveryLiveTimeline, "INFRA-8"), false},
		{"a measures view that shows the issue", deliveryViewShowing(deliveryLiveMeasures, "INFRA-7"), false},
		{"a measures view that shows other issues", deliveryViewShowing(deliveryLiveMeasures, "INFRA-8"), false},
		{"a pipeline view that shows the issue", deliveryViewShowing(deliveryLivePipeline, "INFRA-7"), false},
		{"a pipeline view that shows other issues", deliveryViewShowing(deliveryLivePipeline, "INFRA-8"), false},
	}
	woken, left := 0, 0
	for _, test := range tests {
		got := test.view.touchedBy(change, now)
		if got != test.want {
			t.Errorf("%s: touchedBy = %t, want %t", test.what, got, test.want)
		}
		if got {
			woken++
		} else {
			left++
		}
	}

	// The board still judges no freshness: the worker reads classes.reads for that
	// (delivery_live_hub_worker.go), this change moves no delivery row, and the board reads the
	// Legion source and not the delivery rows, so the verdict stays the one the delivery views'
	// own reads set.
	if reads := deliveryLiveKinds[legionBoardView].reads; reads != viewReadsLegion {
		t.Errorf("the Legion board reads %d, want viewReadsLegion: it reads no delivery row, so it must judge no freshness", reads)
	}
	t.Logf("a change of %s in %s: %d of %d views woken, %d left, Legion board reads %d",
		change.Ref, change.Repo, woken, len(tests), left, deliveryLiveKinds[legionBoardView].reads)
}

// writeIssueFacts writes one issue's stored Legion facts, in the tree of root, whose work falls
// between first and last, closed at closed or open where it is nil, as the facts consumer writes
// one issue's row (legionfacts.WriteProject). The row is written by hand rather than computed, so
// the row a check reads is the one it states.
func writeIssueFacts(t *testing.T, database *store.Store, project, issue, root string, first, last time.Time, closed *time.Time) {
	t.Helper()
	row := legionfacts.Row{
		Project: project, Issue: issue, UpdatedAt: time.Now().UTC(),
		ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
			OpenedAt: first, ClosedAt: closed, Root: root == issue, Tree: root, WorkFirstAt: &first, WorkLastAt: &last,
		}},
	}
	if _, err := legionfacts.WriteProject(t.Context(), database.Pool,
		legionfacts.ProjectWrite{Project: project, Rows: []legionfacts.Row{row}, Stated: []string{issue}}); err != nil {
		t.Fatalf("write the stored facts of %s: %v", issue, err)
	}
}

// recomputeOfView matches a change's recompute of the one view the frame names, by the key
// identical views share.
func recomputeOfView(t *testing.T, frame DeliveryLiveView) func(deliveryHubNote) bool {
	t.Helper()
	spec, refusal := parseDeliveryLiveView(frame)
	if refusal != nil {
		t.Fatalf("read the %s view: %+v", frame.Kind, refusal)
	}
	return func(note deliveryHubNote) bool {
		return note.what == "recompute begin" && note.view == spec.key && note.cause == deliveryCauseChange
	}
}

// TestAnIssueFactsWriteSendsTheLegionBoardItsEntryAndNoFreshness: the write of one issue's stored
// Legion facts reaches an open board through the change log, as a delivery row's write reaches an
// open Delivery page. The board carries that issue's stored row in its issues map, so the write
// is recomputed and sent as a delta of the one entry, and the recompute sends no freshness frame,
// because the board reads no delivery row and judges no verdict. The first row of an issue the
// board names nowhere, a closed new member of a tree it shows, which only the read of that tree by
// its root brings into the map, reaches it the same way, as the entry its GET would answer. A
// closed row of a tree the board does not show recomputes it into the same body, which sends
// nothing. The timeline shows none of them and is sent nothing at all.
//
// It holds the hub to one recompute of the board for each write of its project and to none of
// any other view: the hub recomputes every view when its event feed is taken again, which no
// write of this test caused, so another recompute here is a mark this row did not ask for.
func TestAnIssueFactsWriteSendsTheLegionBoardItsEntryAndNoFreshness(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	seedLegionBoard(t, rig.database, "INFRA", rig.clock.Now())

	// The timeline's read sets the hub's first freshness verdict, so any frame after it is a
	// verdict that changed rather than the first one the hub ever knew.
	timelineFrame, boardFrame := presetView(deliveryLiveTimeline, 7, ""), legionView("INFRA")
	timeline := newFakeLiveConn()
	rig.hub.Subscribe(timeline, subscribeFrame("d", timelineFrame))
	timeline.snapshot(t, "d")
	board := newFakeLiveConn()
	rig.hub.Subscribe(board, subscribeFrame("b", boardFrame))
	board.snapshot(t, "b")
	board.freshness(t, "the verdict after the board's first snapshot")
	rig.settle(t)

	// INFRA-4 is on the board's cards, and INFRA-99 on none and with no row before its write: a
	// closed new member of the tree of INFRA-3, whose card the board shows, so neither its key nor
	// the open rows bring it in, and its stored root alone does.
	workedAt := rig.clock.Now().Add(-5 * time.Hour)
	for _, write := range []struct {
		issue  string
		closed *time.Time
	}{{"INFRA-4", nil}, {"INFRA-99", new(workedAt.Add(2 * time.Hour))}} {
		issue := write.issue
		shown := rig.mark()
		writeIssueFacts(t, rig.database, "INFRA", issue, "INFRA-3", workedAt, workedAt.Add(time.Hour), write.closed)
		rig.hub.catchUpNow()
		rig.waitFor(t, shown, "a recompute of the board of "+issue+"'s project", recomputeOfView(t, boardFrame))
		delta := board.delta(t, "b")
		if _, sent := delta.Maps["issues"][issue]; !sent {
			t.Fatalf("the delta after the facts write of %s carries the issues entries %v, want %s's", issue, delta.Maps["issues"], issue)
		}
		board.none(t, viewMaxWait, "the delta of the facts write of "+issue+", which carries no freshness verdict")
		boardRecomputes := rig.count(shown, recomputeOfView(t, boardFrame))
		if allRecomputes := rig.count(shown, recomputeBegin(deliveryCauseChange)); boardRecomputes != 1 || allRecomputes != 1 {
			t.Errorf("the facts write of %s recomputed the board %d times and %d views in all, want the board once and no other view",
				issue, boardRecomputes, allRecomputes)
		}
	}
	// INFRA-98 is a closed issue, the root of a tree no card stands in: its row wakes the board of
	// its project, whose recompute answers the body it sent, so nothing is sent.
	unshown := rig.mark()
	writeIssueFacts(t, rig.database, "INFRA", "INFRA-98", "INFRA-98", workedAt, workedAt.Add(time.Hour), new(workedAt.Add(2*time.Hour)))
	rig.hub.catchUpNow()
	rig.waitFor(t, unshown, "a recompute of the board of INFRA-98's project", recomputeOfView(t, boardFrame))
	board.none(t, viewMaxWait, "the facts write of INFRA-98, of a tree the board does not show")
	if n := rig.count(unshown, recomputeOfView(t, boardFrame)); n != 1 {
		t.Errorf("the facts write of INFRA-98 recomputed the board %d times, want once", n)
	}
	timeline.none(t, viewMaxWait, "the facts writes of issues the timeline does not show")

	// A row of a project no open view shows leaves the hub idle and tells no page anything.
	idle := rig.mark()
	writeIssueFacts(t, rig.database, "OPS", "OPS-1", "OPS-1", workedAt, workedAt.Add(time.Hour), nil)
	rig.hub.catchUpNow()
	rig.waitNoRecompute(t, viewMaxWait)
	board.none(t, time.Second, "the facts write of a project no view shows")
	timeline.none(t, time.Second, "the facts write of a project no view shows")
	if n := rig.count(idle, recomputeBegin(deliveryCauseChange)); n != 0 {
		t.Errorf("the facts write of another project recomputed %d views, want none", n)
	}
	t.Log("the facts writes of INFRA-4, on a card, and INFRA-99, closed and on none in a tree the board shows, each recomputed the board once and sent it one delta carrying its entry and no freshness frame, and sent the timeline nothing; a write of INFRA-98, of a tree the board does not show, recomputed it once and sent nothing; a write of OPS-1 recomputed nothing and sent nothing")
}

// TestAnEventOnAnIssueWithNoCardRecomputesNothingAndItsFactsSendOneDelta: a Legion board marks
// itself on an issue's event by the issues the daemon reports, the issues it may draw a card of
// (legionBoardPopulation.issueKeys), and not by the issues it carries only a stored row of. What
// an event moves on the board of such an issue, the facts consumer writes after it as a row of
// the issue's facts, whose change marks the board of its project (touchedBy's issue_facts arm), so
// a mark of the event itself recomputes a board whose answer has not moved yet: every PATCH of an
// open issue on no card cost the board a recompute that sent nothing, then the one its row's write
// owed.
//
// INFRA-50 is an open issue the daemon reports nothing of, outside every tree: the board draws it
// no card and carries its stored row, as it carries every open row. A rename of it recomputes no
// view; the write of its facts row recomputes the board once and sends it one delta carrying the
// issue's entry.
func TestAnEventOnAnIssueWithNoCardRecomputesNothingAndItsFactsSendOneDelta(t *testing.T) {
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	seedLegionBoard(t, rig.database, "INFRA", rig.clock.Now())
	issue := seedLegionIssue(t, rig.database, "INFRA", 50, "An issue in backlog", "z", nil, "")
	workedAt := rig.clock.Now().Add(-5 * time.Hour)
	writeIssueFacts(t, rig.database, "INFRA", issue, issue, workedAt, workedAt.Add(time.Hour), nil)
	if _, carded := legionCardHeld(legionBoardOf(t, rig.handler, "INFRA"), issue); carded {
		t.Fatalf("the board draws a card of %s, so its events are a card's and this test states nothing", issue)
	}

	boardFrame := legionView("INFRA")
	board := newFakeLiveConn()
	rig.hub.Subscribe(board, subscribeFrame("b", boardFrame))
	var answered struct {
		Issues map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(board.snapshot(t, "b").Body, &answered); err != nil {
		t.Fatalf("read the board's first answer: %v", err)
	}
	if _, carried := answered.Issues[issue]; !carried {
		t.Fatalf("the board's first answer carries no entry of %s, so a rename of it moves nothing this test can judge", issue)
	}
	rig.settle(t)

	// A title Dispatch rewrites: the event reaches the hub on the relay's feed, behind the event
	// horizon, and the board's answer carries no title of an issue on no card.
	renamed := rig.mark()
	patchHubIssue(t, rig.handler, issue, map[string]any{"title": "An issue in backlog, renamed"})
	rig.waitForEventCursor(t, eventHead(t, rig.database), "the rename of "+issue)
	rig.waitNoRecompute(t, viewMaxWait)
	if n := rig.count(renamed, recomputeBegin("")); n != 0 {
		t.Errorf("the rename of %s, on no card, recomputed %d views, want none", issue, n)
	}
	board.none(t, time.Second, "the rename of "+issue+", on no card")

	// The write of its facts row, which is what moves its entry.
	written := rig.mark()
	writeIssueFacts(t, rig.database, "INFRA", issue, issue, workedAt, workedAt.Add(2*time.Hour), nil)
	rig.hub.catchUpNow()
	rig.waitFor(t, written, "a recompute of the board of "+issue+"'s project", recomputeOfView(t, boardFrame))
	delta := board.delta(t, "b")
	if _, sent := delta.Maps["issues"][issue]; !sent {
		t.Fatalf("the delta after the facts write of %s carries the issues entries %v, want %s's", issue, delta.Maps["issues"], issue)
	}
	board.none(t, viewMaxWait, "the one delta of the facts write of "+issue)
	if boardRecomputes, all := rig.count(written, recomputeOfView(t, boardFrame)), rig.count(written, recomputeBegin("")); boardRecomputes != 1 || all != 1 {
		t.Errorf("the facts write of %s recomputed the board %d times and %d views in all, want the board once and no other view",
			issue, boardRecomputes, all)
	}
	t.Logf("the rename of %s, on no card, recomputed no view and sent nothing; the write of its facts row recomputed the board once and sent one delta carrying its entry", issue)
}

// batchRows is how many stored facts rows one whole read of a project of INFRA's size rewrites:
// the 210 issues of the facts' own rig that carry Legion phase work. A build's first whole read
// rewrites every row whose values or writer moved, as the newer build of a roll rewrites every
// row the older wrote, and the trigger logs one delivery change for each (migration
// 20261117100002).
const batchRows = 210

// writeFactsBatch writes count rows of the project in one write, as one whole read does, giving
// every row the work hours named. It answers the keys WriteProject says it changed. The rows are
// written by hand rather than computed, so the batch a measurement counts is the batch this
// states. No row carries an UpdatedAt, so the write stamps each with the moment it started, as
// every pass of the consumer stamps the rows it writes: a rewritten row's updated_at moves, which
// is what moves the board's revision of its entry (live_frames.go's legionBoardView).
func writeFactsBatch(t *testing.T, database *store.Store, project string, count int, hours float64, at time.Time) []string {
	t.Helper()
	rows := make([]legionfacts.Row, 0, count)
	for number := 1; number <= count; number++ {
		first := at.Add(-time.Duration(number) * time.Minute)
		last := first.Add(time.Duration(hours * float64(time.Hour)))
		rows = append(rows, legionfacts.Row{
			Project: project, Issue: fmt.Sprintf("%s-%d", project, number),
			ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
				OpenedAt: at.Add(-48 * time.Hour), PhaseHours: map[string]float64{"implementing": hours},
				WorkHours: hours, Tree: project + "-1", WorkFirstAt: &first, WorkLastAt: &last,
			}},
		})
	}
	changed, err := legionfacts.WriteProject(t.Context(), database.Pool,
		legionfacts.ProjectWrite{Project: project, Rows: rows, Whole: true})
	if err != nil {
		t.Fatalf("write %d stored facts rows of %s: %v", count, project, err)
	}
	return changed
}

// TestWhatOneBatchOfChangedLegionFactsRowsCostsAnOpenBoard: what one pass that rewrites every row
// of a project costs a subscribed Legion board. A whole read writes every changed row in one pass,
// the trigger logs a change row for each, and the hub marks the board once per change row it
// reads.
//
// A mark is a flag on the view, not a recompute (markDirtyLocked), and a dirty view is due
// viewQuiet after its last mark or viewMaxWait after its first (dueAt). The clock stands still,
// so the whole batch is one instant to the hub however long the writes take: nothing is due
// until the test moves the clock, and what the hub does then is the measurement.
//
// The rows are written once before the subscription, so the board's first answer names every one
// of them and the batch rewrites rows the board already shows, which is what a build's whole
// read of a project that already holds rows writes, and the delta carries every entry it moved.
func TestWhatOneBatchOfChangedLegionFactsRowsCostsAnOpenBoard(t *testing.T) {
	clock := newStillHubClock()
	rig := newClockedHubRig(t, clock)
	seedHubSettings(t, rig.database)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	seedLegionBoard(t, rig.database, "INFRA", clock.Now())
	writeFactsBatch(t, rig.database, "INFRA", batchRows, 4, clock.Now())

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", legionView("INFRA")))
	snapshot := conn.snapshot(t, "b")
	var answered struct {
		Issues map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(snapshot.Body, &answered); err != nil {
		t.Fatalf("read the board's first answer: %v", err)
	}
	if len(answered.Issues) != batchRows {
		t.Fatalf("the board's first answer names %d issues entries, want the %d rows the batch below rewrites",
			len(answered.Issues), batchRows)
	}
	rig.settle(t)

	start := rig.mark()
	before := latestChange(t, rig.database)
	// One pass that rewrites the project: every row's hours have moved, so every row is rewritten.
	changed := writeFactsBatch(t, rig.database, "INFRA", batchRows, 5, clock.Now())
	logged := latestChange(t, rig.database) - before
	if len(changed) != batchRows || logged != int64(batchRows) {
		t.Fatalf("the batch rewrote %d rows and logged %d changes, want %d of each: a batch of another size is another measurement",
			len(changed), logged, batchRows)
	}
	rig.hub.catchUpNow()
	conn.none(t, 2*viewQuiet, "a batch of facts rows the clock has not moved past")
	if n := rig.count(start, recomputeBegin(deliveryCauseChange)); n != 0 {
		t.Fatalf("recomputes before the batch's quiet passed = %d, want 0", n)
	}

	started := time.Now()
	clock.Advance(viewQuiet)
	delta := conn.delta(t, "b")
	took := time.Since(started)
	conn.none(t, viewMaxWait, "the batch's one delta")
	recomputes := rig.count(start, recomputeBegin(deliveryCauseChange))
	if recomputes != 1 {
		t.Errorf("a batch of %d changed rows cost the board %d recomputes, want the one its marks coalesce into",
			batchRows, recomputes)
	}
	t.Logf("one batch: %d rows rewritten in one Write, %d delivery changes logged, %d recompute(s) of the subscribed board and 1 frame to it, carrying %d issues entries; the recompute answered %s after the quiet passed",
		len(changed), logged, recomputes, len(delta.Maps["issues"]), took.Round(time.Millisecond))
}

// storedPrecision is how close a value a page closes at its own clock comes to the value the GET
// route answers at that clock: the computation rounds every hours value to four decimals
// (metrics' round), and a page closes the value the read rounded, so the two differ by at most
// one unit of the fourth decimal.
const storedPrecision = 1e-4

// writeGrowingLegionFacts writes three open rows of seedLegionBoard's project, each closed at a
// moment before now with the growth the computation would store: INFRA-4, the child in a turn,
// grows its work time, its slot hours, its implementing hours and the end of its open bar, and
// facts the board does not carry (its hours in backlog or icebox, its implementer's bar, its
// reviewer's idle hours); INFRA-8, the unstarted child, grows hours in the queue column it holds
// none in yet; and INFRA-3, the design root, grows only its hours in backlog or icebox while it
// waits for a child, a fact the board does not carry, so every value of its entry stands still.
func writeGrowingLegionFacts(t *testing.T, database *store.Store, now time.Time) {
	t.Helper()
	childAsOf, queuedAsOf, rootAsOf := now.Add(-3*time.Hour), now.Add(-2*time.Hour), now.Add(-5*time.Hour)
	childFirst, queuedFirst := now.Add(-6*time.Hour), now.Add(-4*time.Hour)
	rows := []legionfacts.Row{{
		Project: "INFRA", Issue: "INFRA-4", UpdatedAt: now.Add(-2 * time.Minute),
		ClosedFacts: metrics.ClosedFacts{
			IssueFacts: metrics.IssueFacts{
				OpenedAt:   now.Add(-9 * time.Hour),
				PhaseHours: map[string]float64{"implementing": 3.5, "reviewing": 1.25},
				WorkHours:  4.75, ReviewRounds: 2, Tree: "INFRA-3", TreeDepth: 1, SlotHours: 4,
				WorkFirstAt: &childFirst, WorkLastAt: &childAsOf,
			},
			AsOf: childAsOf,
			Growth: metrics.Growth{
				"work_hours": 1, "slot_hours": 1, "phase_hours.implementing": 1, "work_last_at": 1,
				"backlog_or_icebox_hours": 1, "bars.implementer.hours": 1, "idle_by_role.reviewer": 1,
			},
		},
	}, {
		Project: "INFRA", Issue: "INFRA-8", UpdatedAt: now.Add(-3 * time.Minute),
		ClosedFacts: metrics.ClosedFacts{
			IssueFacts: metrics.IssueFacts{
				OpenedAt: now.Add(-7 * time.Hour), PhaseHours: map[string]float64{}, Tree: "INFRA-3", TreeDepth: 1,
				WorkFirstAt: &queuedFirst, WorkLastAt: &queuedAsOf,
			},
			AsOf:   queuedAsOf,
			Growth: metrics.Growth{"phase_hours.queue": 1},
		},
	}, {
		Project: "INFRA", Issue: "INFRA-3", WaitingReason: "waiting for a child", UpdatedAt: now.Add(-4 * time.Minute),
		ClosedFacts: metrics.ClosedFacts{
			IssueFacts: metrics.IssueFacts{
				OpenedAt: now.Add(-12 * time.Hour), PhaseHours: map[string]float64{"design": 2},
				WorkHours: 2, Root: true, Tree: "INFRA-3", SlotHours: 6,
			},
			AsOf:   rootAsOf,
			Growth: metrics.Growth{"backlog_or_icebox_hours": 1},
		},
	}}
	if _, err := legionfacts.WriteProject(t.Context(), database.Pool,
		legionfacts.ProjectWrite{Project: "INFRA", Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the growing stored facts of INFRA: %v", err)
	}
}

// closeLegionEntry is entry given at the clock at, as a page that holds it gives it: each value
// that grows plus its rate times the hours from the entry's as_of to at (legionBoardGrowth), and
// an entry with no growth as it stands, since its values are its values at any clock.
func closeLegionEntry(entry legionBoardFacts, at time.Time) legionBoardFacts {
	if entry.Growth == nil {
		return entry
	}
	elapsed := at.Sub(*entry.AsOf)
	closed := entry
	closed.HoursByPhase = maps.Clone(entry.HoursByPhase)
	for column, rate := range entry.Growth.HoursByPhase {
		closed.HoursByPhase[column] += rate * elapsed.Hours()
	}
	closed.WorkHours += entry.Growth.WorkHours * elapsed.Hours()
	closed.SlotHours += entry.Growth.SlotHours * elapsed.Hours()
	if entry.WorkLastAt != nil {
		moved := entry.WorkLastAt.Add(time.Duration(entry.Growth.WorkLastAt * float64(elapsed)))
		closed.WorkLastAt = &moved
	}
	return closed
}

// TestALegionBoardSendsAnIssuesEntryOnlyWhenItsStoredRowMoves: each read closes every open row at
// its own clock, so an open issue's grown values differ on every recompute, and the board still
// sends an issues entry only when the stored row behind it moved. An entry whose values grow
// carries the moment they are of and how fast each grows after it, so a page closes it at its own
// clock by the rule the read closes the row by (legionfacts.Row.At); an entry none of whose values
// grows carries null for both, even where its stored row grows facts the entry does not carry; and
// the entry's revision leaves out the values that grow and that moment (live_frames.go's
// legionBoardView).
//
// The snapshot is the GET route's body at the snapshot's own moment, growth and moments
// included (k25). A recompute a minute later that no stored row moved, the sampler's next read,
// sends the header it moved and no issues entry. A write of one row sends that one entry, whole,
// and none of the entries that only grew. The board a page then holds, each entry closed at the
// moment of a GET an hour later, is that GET's body to the stored precision.
//
// The hub's clock runs an hour behind the wall clock, which the GET route reads, so the GET is
// later than every answer the hub sent and every growing value has grown by an hour since.
func TestALegionBoardSendsAnIssuesEntryOnlyWhenItsStoredRowMoves(t *testing.T) {
	clock := &fakeHubClock{offset: -time.Hour}
	rig := newClockedHubRig(t, clock)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	seeded := clock.Now()
	seedLegionBoard(t, rig.database, "INFRA", seeded)
	writeGrowingLegionFacts(t, rig.database, seeded)

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", legionView("INFRA")))
	snapshot := conn.snapshot(t, "b")
	var snapped legionBoardResponse
	if err := json.Unmarshal(snapshot.Body, &snapped); err != nil {
		t.Fatalf("read the snapshot's body: %v", err)
	}
	population, err := rig.hub.server.readLegionBoard(t.Context(), "INFRA", snapped.ComputedAt)
	if err != nil {
		t.Fatalf("read the board at the snapshot's moment: %v", err)
	}
	answered, err := json.Marshal(legionBoardCompute(population, snapped.ComputedAt))
	if err != nil {
		t.Fatalf("encode the board at the snapshot's moment: %v", err)
	}
	if string(answered) != string(snapshot.Body) {
		t.Fatalf("the snapshot differs from the GET route's body at its moment, %s:\nsnapshot: %s\nGET:      %s",
			snapped.ComputedAt, snapshot.Body, answered)
	}
	wantGrowth := legionBoardGrowth{HoursByPhase: map[string]float64{"implementing": 1}, WorkHours: 1, SlotHours: 1, WorkLastAt: 1}
	if child := snapped.Issues["INFRA-4"]; child.AsOf == nil || !child.AsOf.Equal(snapped.ComputedAt) ||
		child.Growth == nil || !reflect.DeepEqual(*child.Growth, wantGrowth) {
		t.Fatalf("INFRA-4's entry is of %v with the growth %+v, want the snapshot's moment %s and the growth %+v: its stored rates of the facts the entry carries alone",
			child.AsOf, child.Growth, snapped.ComputedAt, wantGrowth)
	}
	if root := snapped.Issues["INFRA-3"]; root.AsOf != nil || root.Growth != nil {
		t.Fatalf("INFRA-3's entry is of %v with the growth %+v, want null for both: its row grows only its hours in backlog or icebox, which the entry does not carry",
			root.AsOf, root.Growth)
	}
	rig.settle(t)
	held := snapshot.Body

	// The sampler's next read, a minute on: the header carries the read, and no stored row moved.
	clock.Advance(time.Minute)
	seedLegionRead(t, rig.database, "INFRA", clock.Now(), "")
	notifyChannel(t, rig.database, legion.StateChannel, "INFRA")
	read := conn.delta(t, "b")
	if entries := read.Maps["issues"]; len(entries) != 0 {
		t.Fatalf("a recompute a minute later that moved no stored row sent the issues entries %v, want none: an open entry's grown values are no change of its row",
			slices.Sorted(maps.Keys(entries)))
	}
	if _, set := read.Set["header"]; !set {
		t.Fatalf("the sampler's read sent %+v, want the header it moved", read.Set)
	}
	held = applyLiveDelta(t, legionBoardView, held, read)

	// The root's row moves: the consumer wrote why it waits now.
	moved := clock.Now()
	rootRow := legionfacts.Row{
		Project: "INFRA", Issue: "INFRA-3", WaitingReason: "blocked", UpdatedAt: moved,
		ClosedFacts: metrics.ClosedFacts{
			IssueFacts: metrics.IssueFacts{
				OpenedAt: moved.Add(-12 * time.Hour), PhaseHours: map[string]float64{"design": 2},
				WorkHours: 2, Root: true, Tree: "INFRA-3", SlotHours: 6,
			},
			AsOf: moved.Add(-30 * time.Minute),
		},
	}
	if _, err := legionfacts.WriteProject(t.Context(), rig.database.Pool,
		legionfacts.ProjectWrite{Project: "INFRA", Rows: []legionfacts.Row{rootRow}, Stated: []string{"INFRA-3"}}); err != nil {
		t.Fatalf("write INFRA-3's moved row: %v", err)
	}
	rig.hub.catchUpNow()
	write := conn.delta(t, "b")
	if sent := slices.Sorted(maps.Keys(write.Maps["issues"])); !slices.Equal(sent, []string{"INFRA-3"}) {
		t.Fatalf("the write of INFRA-3's row sent the issues entries %v, want INFRA-3's alone", sent)
	}
	var whole map[string]json.RawMessage
	if err := json.Unmarshal(write.Maps["issues"]["INFRA-3"], &whole); err != nil {
		t.Fatalf("read INFRA-3's sent entry: %v", err)
	}
	fields := []string{}
	for _, field := range liveJSONFields(reflect.TypeFor[legionBoardFacts]()) {
		fields = append(fields, field.name)
	}
	if got, want := slices.Sorted(maps.Keys(whole)), slices.Sorted(slices.Values(fields)); !slices.Equal(got, want) {
		t.Fatalf("INFRA-3's sent entry holds %v, want the whole entry %v", got, want)
	}
	// Its moved row has no growth, so the entry carries null for both, sent as null and not left
	// out, as the entry's other optional fields are.
	if asOf, growth := string(whole["as_of"]), string(whole["growth"]); asOf != "null" || growth != "null" {
		t.Fatalf("INFRA-3's sent entry carries the as_of %s and the growth %s, want null for both: nothing it carries grows", asOf, growth)
	}
	held = applyLiveDelta(t, legionBoardView, held, write)

	// An hour on, by the wall clock: every entry the page holds, closed at the GET's moment,
	// is the GET's.
	var page legionBoardResponse
	if err := json.Unmarshal(held, &page); err != nil {
		t.Fatalf("read the board the page holds: %v", err)
	}
	got := legionBoardOf(t, rig.handler, "INFRA")
	if keys, want := slices.Sorted(maps.Keys(page.Issues)), slices.Sorted(maps.Keys(got.Issues)); !slices.Equal(keys, want) {
		t.Fatalf("the page holds the entries %v and the GET answers %v", keys, want)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) <= storedPrecision+1e-9 }
	grown := 0
	for _, key := range slices.Sorted(maps.Keys(got.Issues)) {
		want, holding := got.Issues[key], page.Issues[key]
		if (holding.AsOf == nil) != (holding.Growth == nil) || (want.AsOf == nil) != (want.Growth == nil) {
			t.Errorf("%s: the page holds the as_of %v with the growth %+v and the GET answers %v with %+v, want both or neither",
				key, holding.AsOf, holding.Growth, want.AsOf, want.Growth)
			continue
		}
		closed := closeLegionEntry(holding, got.ComputedAt)
		if !want.OpenedAt.Equal(closed.OpenedAt) || !reflect.DeepEqual(want.ClosedAt, closed.ClosedAt) ||
			want.ReviewRounds != closed.ReviewRounds || want.RootKey != closed.RootKey || want.TreeDepth != closed.TreeDepth ||
			want.WaitingReason != closed.WaitingReason || !reflect.DeepEqual(want.WorkFirstAt, closed.WorkFirstAt) ||
			!want.UpdatedAt.Equal(closed.UpdatedAt) || !reflect.DeepEqual(want.Growth, closed.Growth) {
			t.Errorf("%s: the page holds %+v and the GET answers %+v", key, holding, want)
			continue
		}
		if !near(want.WorkHours, closed.WorkHours) || !near(want.SlotHours, closed.SlotHours) {
			t.Errorf("%s: closed at %s the page gives %g work and %g slot hours, the GET %g and %g",
				key, got.ComputedAt, closed.WorkHours, closed.SlotHours, want.WorkHours, want.SlotHours)
		}
		if !slices.Equal(slices.Sorted(maps.Keys(want.HoursByPhase)), slices.Sorted(maps.Keys(closed.HoursByPhase))) {
			t.Errorf("%s: closed at %s the page gives hours in %v, the GET in %v", key, got.ComputedAt, closed.HoursByPhase, want.HoursByPhase)
		}
		for column, hours := range want.HoursByPhase {
			if !near(hours, closed.HoursByPhase[column]) {
				t.Errorf("%s: closed at %s the page gives %g hours in %s, the GET %g", key, got.ComputedAt, closed.HoursByPhase[column], column, hours)
			}
		}
		if (want.WorkLastAt == nil) != (closed.WorkLastAt == nil) ||
			(want.WorkLastAt != nil && want.WorkLastAt.Sub(*closed.WorkLastAt).Abs() > time.Microsecond) {
			t.Errorf("%s: closed at %s the page's work ends %v, the GET's %v", key, got.ComputedAt, closed.WorkLastAt, want.WorkLastAt)
		}
		// A growing entry's values are of the GET's moment, and a standing one carries no moment.
		if holding.Growth == nil {
			if want.AsOf != nil {
				t.Errorf("%s stands still and the GET gives its values the moment %s, want null", key, want.AsOf)
			}
			continue
		}
		grown++
		if !want.AsOf.Equal(got.ComputedAt) {
			t.Errorf("%s: the GET's values are of %s, want %s", key, want.AsOf, got.ComputedAt)
		}
		if want.WorkHours == holding.WorkHours && want.SlotHours == holding.SlotHours && maps.Equal(want.HoursByPhase, holding.HoursByPhase) {
			t.Errorf("%s grows and the GET an hour later answers it as the page holds it, %+v, so closing it proved nothing", key, want)
		}
	}
	if grown != 2 {
		t.Errorf("%d of the entries grow, want INFRA-4 and INFRA-8", grown)
	}
	t.Logf("a recompute a minute later sent %d issues entries; a write of one row sent %v; %d entries, %d of them growing, closed at the GET's moment %s land on its values to %g",
		len(read.Maps["issues"]), slices.Sorted(maps.Keys(write.Maps["issues"])), len(got.Issues), grown, got.ComputedAt, storedPrecision)
}

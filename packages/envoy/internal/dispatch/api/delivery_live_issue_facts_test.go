package api

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// issueFactsChange is one delivery_changes row of the stored per-issue Legion facts: the project
// in Repo and the issue in Ref, with the span of days that issue's work falls in (migration
// 20261111093100).
func issueFactsChange(project, issue string, first, last time.Time) delivery.Change {
	return delivery.Change{Seq: 1, Kind: "issue_facts", Repo: project, Ref: issue, TOld: &first, TNew: &last}
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

// TestAnIssueFactsChangeTouchesTheLegionBoardsThatShowTheIssue: a write of one issue's stored
// Legion facts moves the Legion boards that show that issue and no other view at all. A board
// names its own issues by the rule dirtyIssueLocked marks an issue's write by, so a board that
// shows the issue is woken and a board that does not is left.
//
// No delivery view is woken, whichever issue it names: the stored facts have one reader
// (legionfacts.Read, which the board calls), so no timeline, measures or pipeline answer can
// move when a facts row does. That matters on a pace: the facts sweep rewrites the row of every
// open issue every legionfacts.SweepEvery, and each rewrite logs a change, so one worked open
// issue would otherwise force a whole Delivery recompute on every subscribed page that often.
func TestAnIssueFactsChangeTouchesTheLegionBoardsThatShowTheIssue(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	change := issueFactsChange("INFRA", "INFRA-7", now.Add(-26*time.Hour), now.Add(-2*time.Hour))

	tests := []struct {
		what string
		view *deliveryView
		want bool
	}{
		{"a Legion board that shows the issue", boardViewShowing("INFRA", "INFRA-7", "INFRA-8"), true},
		{"a Legion board that shows other issues", boardViewShowing("INFRA", "INFRA-8"), false},
		{"a Legion board that shows none", boardViewShowing("INFRA"), false},
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

	// The board still judges no freshness: the worker reads classes.delivery for that
	// (delivery_live_hub_worker.go), this change moves no delivery row, and the flag stays off,
	// so the verdict stays the one the delivery views' own reads set.
	if deliveryLiveKinds[legionBoardView].delivery {
		t.Error("the Legion board reads no delivery row, so it must judge no freshness")
	}
	t.Logf("a change of %s in %s: %d of %d views woken, %d left, Legion board delivery flag %t",
		change.Ref, change.Repo, woken, len(tests), left, deliveryLiveKinds[legionBoardView].delivery)
}

// writeIssueFacts writes one issue's stored Legion facts, whose work falls between first and
// last. The row is written by hand rather than computed, so the span the change carries is the
// one the check states.
func writeIssueFacts(t *testing.T, database *store.Store, project, issue string, first, last time.Time) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), `
		insert into issue_facts (project_key, issue_key, opened_at, work_first_at, work_last_at)
		values ($1, $2, $3, $4, $5)
	`, project, issue, first, first, last); err != nil {
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
// because the board reads no delivery row and judges no verdict. The timeline shows neither issue
// and is sent nothing at all.
//
// It holds the hub to one recompute of the one view the facts name and to none of any other
// view: the hub recomputes every view when its event feed is taken again, which no write of
// this test caused, so a second recompute here is a mark this row did not ask for.
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
	timeline.snapshot(t, 5*time.Second, "d")
	board := newFakeLiveConn()
	rig.hub.Subscribe(board, subscribeFrame("b", boardFrame))
	board.snapshot(t, 5*time.Second, "b")
	board.freshness(t, 5*time.Second, "the verdict after the board's first snapshot")
	rig.settle(t)

	shown := rig.mark()
	workedAt := rig.clock.Now().Add(-5 * time.Hour)
	writeIssueFacts(t, rig.database, "INFRA", "INFRA-4", workedAt, workedAt.Add(time.Hour))
	rig.hub.catchUpNow()
	rig.waitFor(t, shown, 5*time.Second, "a recompute of the board the issue is on", recomputeOfView(t, boardFrame))
	delta := board.delta(t, 5*time.Second, "b")
	entry, sent := delta.Maps["issues"]["INFRA-4"]
	if !sent {
		t.Fatalf("the delta carries the issues entries %v, want INFRA-4's", delta.Maps["issues"])
	}
	board.none(t, viewMaxWait, "the delta of the facts write, which carries no freshness verdict")
	timeline.none(t, viewMaxWait, "the facts write of an issue the timeline does not show")

	// An issue no open view shows leaves the hub idle and tells no page anything.
	writeIssueFacts(t, rig.database, "INFRA", "INFRA-99", workedAt, workedAt.Add(time.Hour))
	rig.hub.catchUpNow()
	rig.waitNoRecompute(t, viewMaxWait)
	board.none(t, time.Second, "the facts write of an issue no view shows")
	timeline.none(t, time.Second, "the facts write of an issue no view shows")

	// The count this row's head says it takes: the one view the facts name was recomputed once,
	// and no other view at all.
	boardRecomputes := rig.count(shown, recomputeOfView(t, boardFrame))
	allRecomputes := rig.count(shown, recomputeBegin(deliveryCauseChange))
	if boardRecomputes != 1 {
		t.Errorf("the two facts writes recomputed the board %d times, want the one the first write caused", boardRecomputes)
	}
	if allRecomputes != boardRecomputes {
		t.Errorf("the two facts writes recomputed %d views, of which %d were the board: want the board alone",
			allRecomputes, boardRecomputes)
	}
	t.Logf("INFRA-4's facts: %d board recompute(s) and %d recompute(s) of every view, 1 delta carrying its entry (%s) and no freshness frame, 0 frames to the timeline; INFRA-99's facts: the hub went quiet and sent 0 frames",
		boardRecomputes, allRecomputes, entry)
}

// sweepRows is how many stored facts rows one sweep pass of a project of INFRA's size rewrites:
// the 210 issues of the facts' own rig that carry Legion phase work. An open issue's work hours
// grow with the clock, so every sweep finds every open issue changed, writes it, and the trigger
// logs one delivery change for each (migration 20261111093100).
const sweepRows = 210

// writeFactsSweep writes count rows of the project in one write, as one sweep pass does, giving
// every row the work hours named. It answers the keys WriteProject says it changed. The rows are
// written by hand rather than computed, so the batch a measurement counts is the batch this
// states.
func writeFactsSweep(t *testing.T, database *store.Store, project string, count int, hours float64, at time.Time) []string {
	t.Helper()
	rows := make([]legionfacts.Row, 0, count)
	for number := 1; number <= count; number++ {
		first := at.Add(-time.Duration(number) * time.Minute)
		last := first.Add(time.Duration(hours * float64(time.Hour)))
		rows = append(rows, legionfacts.Row{
			Project: project, Issue: fmt.Sprintf("%s-%d", project, number),
			OpenedAt: at.Add(-48 * time.Hour), HoursByPhase: map[string]float64{"implementing": hours},
			WorkHours: hours, RootKey: project + "-1", WorkFirstAt: &first, WorkLastAt: &last,
			UpdatedAt: at,
		})
	}
	changed, err := legionfacts.WriteProject(t.Context(), database.Pool,
		legionfacts.ProjectWrite{Project: project, Rows: rows, Whole: true})
	if err != nil {
		t.Fatalf("write %d stored facts rows of %s: %v", count, project, err)
	}
	return changed
}

// TestWhatOneSweepOfChangedFactsRowsCostsAnOpenBoard: what one sweep pass costs a subscribed
// Legion board. The sweep writes every open issue's row in one pass, the trigger logs a change
// row for each, and the hub marks the board once per change row it reads.
//
// A mark is a flag on the view, not a recompute (markDirtyLocked), and a dirty view is due
// viewQuiet after its last mark or viewMaxWait after its first (dueAt). The clock stands still,
// so the whole batch is one instant to the hub however long the writes take: nothing is due
// until the test moves the clock, and what the hub does then is the measurement.
//
// The rows are written once before the subscription, so the board's first answer names every one
// of them: a facts write of an issue the view's last answer does not name marks nothing
// (showsIssue), and a batch of such writes would measure the empty case.
func TestWhatOneSweepOfChangedFactsRowsCostsAnOpenBoard(t *testing.T) {
	clock := newStillHubClock()
	rig := newClockedHubRig(t, clock)
	seedHubSettings(t, rig.database)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	seedLegionBoard(t, rig.database, "INFRA", clock.Now())
	writeFactsSweep(t, rig.database, "INFRA", sweepRows, 4, clock.Now())

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", legionView("INFRA")))
	snapshot := conn.snapshot(t, 10*time.Second, "b")
	var answered struct {
		Issues map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(snapshot.Body, &answered); err != nil {
		t.Fatalf("read the board's first answer: %v", err)
	}
	if len(answered.Issues) != sweepRows {
		t.Fatalf("the board's first answer names %d issues entries, want the %d rows the sweep below rewrites",
			len(answered.Issues), sweepRows)
	}
	rig.settle(t)

	start := rig.mark()
	before := latestChange(t, rig.database)
	// One sweep pass: every row's hours have grown, so every row is rewritten.
	changed := writeFactsSweep(t, rig.database, "INFRA", sweepRows, 5, clock.Now())
	logged := latestChange(t, rig.database) - before
	if len(changed) != sweepRows || logged != int64(sweepRows) {
		t.Fatalf("the sweep rewrote %d rows and logged %d changes, want %d of each: a batch of another size is another measurement",
			len(changed), logged, sweepRows)
	}
	rig.hub.catchUpNow()
	conn.none(t, 2*viewQuiet, "a batch of facts rows the clock has not moved past")
	if n := rig.count(start, recomputeBegin(deliveryCauseChange)); n != 0 {
		t.Fatalf("recomputes before the batch's quiet passed = %d, want 0", n)
	}

	started := time.Now()
	clock.Advance(viewQuiet)
	delta := conn.delta(t, 30*time.Second, "b")
	took := time.Since(started)
	conn.none(t, viewMaxWait, "the batch's one delta")
	recomputes := rig.count(start, recomputeBegin(deliveryCauseChange))
	if recomputes != 1 {
		t.Errorf("a sweep of %d changed rows cost the board %d recomputes, want the one its marks coalesce into",
			sweepRows, recomputes)
	}
	t.Logf("one sweep pass: %d rows rewritten in one Write, %d delivery changes logged, %d recompute(s) of the subscribed board and 1 frame to it, carrying %d issues entries; the recompute answered %s after the quiet passed",
		len(changed), logged, recomputes, len(delta.Maps["issues"]), took.Round(time.Millisecond))
}

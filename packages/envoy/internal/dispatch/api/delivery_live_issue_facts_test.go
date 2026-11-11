package api

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
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
// Legion facts moves the views that show that issue and no other, by the rule dirtyIssueLocked
// marks an issue's write by. A Legion board reads no delivery row, so touchedBy's test for that
// refuses it before any kind is read; this kind's branch stands above that test, and a board that
// shows the issue is therefore woken rather than refused unread.
func TestAnIssueFactsChangeTouchesTheLegionBoardsThatShowTheIssue(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	change := issueFactsChange("INFRA", "INFRA-7", now.Add(-26*time.Hour), now.Add(-2*time.Hour))

	woken, left := 0, 0
	for _, test := range []struct {
		what string
		view *deliveryView
		want bool
	}{
		{"a Legion board that shows the issue", boardViewShowing("INFRA", "INFRA-7", "INFRA-8"), true},
		{"a Legion board that shows other issues", boardViewShowing("INFRA", "INFRA-8"), false},
		{"a Legion board that shows none", boardViewShowing("INFRA"), false},
		{"a timeline that shows the issue", deliveryViewShowing(deliveryLiveTimeline, "INFRA-7"), true},
		{"a timeline that shows other issues", deliveryViewShowing(deliveryLiveTimeline, "INFRA-8"), false},
		{"a measures view that shows other issues", deliveryViewShowing(deliveryLiveMeasures, "INFRA-8"), false},
		{"a pipeline view that shows other issues", deliveryViewShowing(deliveryLivePipeline, "INFRA-8"), false},
	} {
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
	t.Logf("a change of %s in %s: %d of 7 views woken, %d left, Legion board delivery flag %t",
		change.Ref, change.Repo, woken, left, deliveryLiveKinds[legionBoardView].delivery)
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
// It counts the recomputes of the one view the facts name and no others: the hub recomputes every
// view when its event feed is taken again, which no write of this test caused.
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
	boardRecomputes := rig.count(shown, recomputeOfView(t, boardFrame))
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
	t.Logf("INFRA-4's facts: %d board recompute(s), 1 delta carrying its entry (%s) and no freshness frame, 0 frames to the timeline; INFRA-99's facts: the hub went quiet and sent 0 frames",
		boardRecomputes, entry)
}

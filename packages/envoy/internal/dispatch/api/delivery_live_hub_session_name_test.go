package api

import (
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// sessionsHeardWhileReading is what the hub has recorded about the sessions that wrote while the
// one held view's read is under way (deliveryView.heardSessions), which recheckHeardLocked
// replays against the answer that read installs.
func sessionsHeardWhileReading(t *testing.T, rig *hubRig) map[string]string {
	t.Helper()
	rig.hub.mu.Lock()
	defer rig.hub.mu.Unlock()
	heard, reading := map[string]string{}, 0
	for _, view := range rig.hub.generation.views {
		if !view.reading {
			continue
		}
		reading++
		maps.Copy(heard, view.heardSessions)
	}
	if reading != 1 {
		t.Fatalf("views whose read is under way = %d, want the one this test holds", reading)
	}
	return heard
}

// hubSessionTitles is the session_titles map of a body a page holds.
func hubSessionTitles(t *testing.T, body json.RawMessage) map[string]string {
	t.Helper()
	var read struct {
		SessionTitles map[string]string `json:"session_titles"`
	}
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("decode a body's session_titles: %v", err)
	}
	return read.SessionTitles
}

// TestTheLiveHubKeepsASessionsNameWhenAnEventCarriesNoTitle: a session's name is the newest title
// it stamped, so a later write carrying none is no change of name. The test drives three writes
// of one session through the hub's intake -- one carrying the title "Release sender", one whose
// actor carries no origin at all (what postDeliveryReply writes), and one whose origin carries an
// empty title -- and the page still holds that name: the two writes that carry none mark no view
// dirty, record nothing for the session while a read is under way, and leave the resolved read
// (delivery.ResolveSessionTitles, which the GET this delta must land on makes) naming it. A
// session that never stamped a title is held by its bare id throughout, which is the limit chosen
// for INFRA-1368: nothing backfills a name for it.
func TestTheLiveHubKeepsASessionsNameWhenAnEventCarriesNoTitle(t *testing.T) {
	var pause *notePause
	rig := newHubRig(t, func(hub *deliveryHub) { pause = pauseNotes(hub) })
	seedHubSettings(t, rig.database)
	createHubIssue(t, rig.handler, map[string]any{"title": "Hold the project"})

	const (
		named    = "ses_release_sender"
		nameless = "ses_never_titled"
	)
	// One population pull request per session, so the timeline names both: session_titles holds
	// the first session of each window pull request.
	for index, session := range []string{named, nameless} {
		pr := hubPR(index+1, rig.clock.Now().Add(-2*time.Hour))
		pr.Sessions = []string{session}
		upsertPR(t, rig.database, pr)
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	held := conn.snapshot(t, 5*time.Second, "t").Body
	rig.settle(t)
	if got := hubSessionTitles(t, held); got[named] != named || got[nameless] != nameless {
		t.Fatalf("the snapshot's session_titles = %v, want both sessions by their bare ids", got)
	}

	start := rig.mark()
	titled := appendHubEvent(t, rig, projectOwner("HUB").event("user_state.updated",
		model.Actor{Kind: "session", ID: named, Origin: &model.ActorOrigin{SessionTitle: "Release sender"}}, map[string]any{}))
	rig.waitFor(t, start, eventFeedWait, "the titled write on the hub's feed", func(note deliveryHubNote) bool {
		return note.what == "event" && note.eventID == titled.ID
	})
	delta := conn.delta(t, 5*time.Second, "t")
	if got := liveMapValues(t, delta, "session_titles"); got[named] != "Release sender" {
		t.Fatalf("the delta's session_titles = %v, want %s under the title it stamped", got, named)
	}
	held = applyLiveDelta(t, deliveryLiveTimeline, held, delta)

	// The two writes that carry no title, with no read under way: neither is a change of name, so
	// neither marks the view and the page is sent nothing.
	start = rig.mark()
	writeWithoutATitle(t, rig, named)
	rig.waitNoRecompute(t, 2*viewQuiet)
	if n := rig.count(start, recomputeBegin("")); n != 0 {
		t.Fatalf("the writes carrying no title recomputed the view %d times, want none", n)
	}
	conn.none(t, time.Second, "two session writes that carry no title")

	// The same two writes while the view's read is under way: the hub records nothing for the
	// session, so the recheck that follows the read marks nothing either and the merge below is
	// the one recompute.
	start = rig.mark()
	point := pause.at(noteIs("recompute read"))
	upsertPR(t, rig.database, hubPR(3, rig.clock.Now().Add(-time.Hour)))
	point.wait(t, "recompute read")
	writeWithoutATitle(t, rig, named)
	if heard := sessionsHeardWhileReading(t, rig); len(heard) != 0 {
		t.Fatalf("the held read heard %v, want nothing recorded for a write that carries no title", heard)
	}
	point.proceed()

	again := conn.delta(t, 5*time.Second, "t")
	if title, changed := liveMapValues(t, again, "session_titles")[named]; changed && title != "Release sender" {
		t.Fatalf("the delta renames %s to %v, want the name it stamped kept", named, title)
	}
	held = requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, again, "")
	if got := hubSessionTitles(t, held); got[named] != "Release sender" || got[nameless] != nameless {
		t.Fatalf("the page holds session_titles = %v, want %s named %q and %s by its bare id", got, named, "Release sender", nameless)
	}
	if n := rig.recomputesSince(start); n != 1 {
		t.Fatalf("recomputes for the merge and the titleless writes during its read = %d, want 1", n)
	}
}

// writeWithoutATitle writes two events of session that carry no title and waits for the hub to
// apply both: one whose actor carries no origin at all, as a delivery reply's does
// (postDeliveryReply), and one whose origin holds an empty title, which only a write building its
// actor in SQL stores, since ActorOrigin's session_title is omitempty (recordSessionTitles).
func writeWithoutATitle(t *testing.T, rig *hubRig, session string) {
	t.Helper()
	start := rig.mark()
	written := appendHubEvent(t, rig, projectOwner("HUB").event("user_state.updated",
		model.Actor{Kind: "session", ID: session}, map[string]any{}))
	rig.waitFor(t, start, eventFeedWait, "the write carrying no origin on the hub's feed", func(note deliveryHubNote) bool {
		return note.what == "event" && note.eventID == written.ID
	})
	recordSessionTitles(t, rig.database, time.Now().UTC(), map[string]string{session: ""})
	rig.waitForEventCursor(t, eventHead(t, rig.database), "the write carrying an empty title")
}

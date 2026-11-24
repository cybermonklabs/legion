package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestEachReaderIsToldTheUnfetchableCountOfWhatItSees: the live hub counts, for each reader, the
// pull requests that can no longer be fetched that the reader is shown, in its snapshot and in the
// freshness frame a later mark sends it, never another reader's count. The server's own reader is
// shown both; an open reader that sees no project is shown only the one that names no issue, and
// not the one whose issue is HID's.
func TestEachReaderIsToldTheUnfetchableCountOfWhatItSees(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	createTestProject(t, rig.handler, "HID")
	key := issueIn(t, rig.handler, "alice", "HID")
	merged := rig.clock.Now().Add(-3 * time.Hour)
	upsertPR(t, rig.database, hubPR(1, merged))
	hidden := hubPR(2, merged)
	hidden.IssueKey = &key
	upsertPR(t, rig.database, hidden)
	if err := delivery.MarkPullRequestUnfetchable(t.Context(), rig.database.Pool, "acme/widgets", 2, "the pull request is gone"); err != nil {
		t.Fatalf("mark acme/widgets#2 unfetchable: %v", err)
	}

	readers := []struct {
		name                  string
		reader                model.Actor
		snapshot, afterMarked int
	}{
		{"the server's own reader", everyReader, 1, 2},
		{"an open reader that sees no project", model.Actor{Viewer: &model.Viewer{Open: true, Sees: []string{}}}, 0, 1},
	}
	conns := make([]*fakeLiveConn, len(readers))
	for index, reader := range readers {
		conns[index] = newFakeLiveConn()
		rig.hub.Subscribe(conns[index], deliverySubscription{Sub: "p", View: presetView(deliveryLiveTimeline, 7, ""), Reader: reader.reader})
		snapshot := conns[index].snapshot(t, 5*time.Second, "p")
		var body deliveryTimelineWithMeasures
		if err := json.Unmarshal(snapshot.Body, &body); err != nil {
			t.Fatalf("decode %s's snapshot: %v", reader.name, err)
		}
		if got := body.Freshness.UnfetchableCount; got != reader.snapshot {
			t.Errorf("%s's snapshot counts %d unfetchable pull requests, want %d", reader.name, got, reader.snapshot)
		}
	}
	// The mark reaches the hub through the delivery change log, which needs the listener alone.
	rig.waitListening(t)

	if err := delivery.MarkPullRequestUnfetchable(t.Context(), rig.database.Pool, "acme/widgets", 1, "the pull request is gone"); err != nil {
		t.Fatalf("mark acme/widgets#1 unfetchable: %v", err)
	}
	for index, reader := range readers {
		conns[index].delta(t, 3*time.Second, "p")
		fresh := conns[index].freshness(t, 3*time.Second, reader.name+"'s freshness frame after the mark")
		if got := fresh.Freshness.UnfetchableCount; got != reader.afterMarked {
			t.Errorf("%s is told %d unfetchable pull requests, want %d", reader.name, got, reader.afterMarked)
		}
	}
}

package api

import (
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestADeliveryViewsChangePositionMovesOnlyWithRowsItsReaderSees: the change position a delivery
// view names (seq, and changed_at, when that change was recorded) moves only with a change of a row
// its reader sees, so no write of a project the reader does not see moves it, nor the time a page
// shows beside "last change". acme/widgets#1 names PUB's issue, which alice sees; acme/secret#7
// names PRIV's, which she does not. acme/secret#7's change leaves alice's position at
// acme/widgets#1's change, and its time with it, and moves carol's, who sees PRIV, to its own. With
// PRIV public again no project is private, and a change of acme/secret#7 moves alice's position to
// the log's newest change, as every reader's moved before a project could be private (R9).
//
// Control: deliveryShownChange counts every change at or below the read's position, whatever row
// it names; alice's position moves with acme/secret#7, and the test fails naming it.
func TestADeliveryViewsChangePositionMovesOnlyWithRowsItsReaderSees(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	seedHubSettings(t, seed.database)
	clock := newNoonHubClock()
	now := clock.Now()
	widgets := hubPR(1, now.Add(-3*time.Hour))
	widgets.IssueKey = new(seed.issue[seedPublic])
	upsertPR(t, seed.database, widgets)
	rig := hubOn(t, seed.handler, seed.database, seed.deps, clock, nil)
	reader := func(login string) model.Actor {
		t.Helper()
		resolved, err := seed.server.readerFor(t.Context(), model.BrowserActor(login))
		if err != nil {
			t.Fatalf("resolve what %s sees: %v", login, err)
		}
		return resolved
	}
	// Each call subscribes a view no subscription holds yet (its window ends a minute later than
	// the last one's), so its snapshot is read now rather than kept from an earlier read.
	views := 0
	position := func(who model.Actor) (int64, *time.Time) {
		t.Helper()
		views++
		conn := newFakeLiveConn()
		rig.hub.Subscribe(conn, deliverySubscription{Sub: "s", View: timelineRange(now.Add(-7*24*time.Hour), now.Add(time.Duration(views)*time.Minute), ""), Reader: who})
		snapshot := conn.snapshot(t, "s")
		seq, err := strconv.ParseInt(snapshot.Seq, 10, 64)
		if err != nil {
			t.Fatalf("seq %q is no change position: %v", snapshot.Seq, err)
		}
		return seq, snapshot.ChangedAt
	}
	changeAt := func(seq int64) time.Time {
		t.Helper()
		var at time.Time
		if err := seed.database.Pool.QueryRow(t.Context(), `select at from delivery_changes where seq = $1`, seq).Scan(&at); err != nil {
			t.Fatalf("read when change %d was recorded: %v", seq, err)
		}
		return at
	}
	sameChange := func(seq int64, at *time.Time, want int64) bool {
		return seq == want && at != nil && at.Equal(changeAt(want))
	}

	visible := latestChange(t, seed.database)
	if seq, at := position(reader(seedAlice)); !sameChange(seq, at, visible) {
		t.Fatalf("alice's position = %d at %v, want acme/widgets#1's change %d at %v", seq, at, visible, changeAt(visible))
	}
	secret := hubPR(7, now.Add(-2*time.Hour))
	secret.Repo, secret.URL = "acme/secret", "https://github.com/acme/secret/pull/7"
	secret.IssueKey = new(seed.issue[seedPrivate])
	upsertPR(t, seed.database, secret)
	hidden := latestChange(t, seed.database)
	if hidden <= visible {
		t.Fatalf("acme/secret#7 logged no change above %d (newest %d)", visible, hidden)
	}
	if seq, at := position(reader(seedAlice)); !sameChange(seq, at, visible) {
		t.Errorf("alice's position after acme/secret#7 = %d at %v, want where it stood, %d at %v: it moved with a pull request of PRIV, which she does not see",
			seq, at, visible, changeAt(visible))
	}
	if seq, at := position(reader(seedCarol)); !sameChange(seq, at, hidden) {
		t.Errorf("carol's position = %d at %v, want acme/secret#7's change %d at %v", seq, at, hidden, changeAt(hidden))
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	secret.Title = "feat: change 7, renamed"
	upsertPR(t, seed.database, secret)
	newest := latestChange(t, seed.database)
	if seq, at := position(reader(seedAlice)); !sameChange(seq, at, newest) {
		t.Errorf("with no private project alice's position = %d at %v, want the log's newest change %d at %v, as before a project could be private",
			seq, at, newest, changeAt(newest))
	}
}

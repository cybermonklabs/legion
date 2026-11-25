package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A full member holds a live delivery view, a timeline, which shows every pull request whose issue
// its reader sees: PUB's, a public project whose list does not name them, among them; PRIV's list
// names them. Their row then becomes a restricted member's (ConfirmSubject, the write the people
// store makes for a change of member kind), and a restricted member reads only the projects that
// name them. From that write's commit the hub reads dave again, so acme/widgets#7, a pull request
// of PUB's issue written after it, reaches his page in no frame. acme/secret#8, a pull request of
// PRIV's issue written after it, does: the read that names it has read acme/widgets#7's row as
// well, and would name it too were it made for the full member dave was. The test ends on that
// frame, and fails when no frame names it.
//
// Control: the write announces nothing. The hub keeps the full member's reader it had, and the
// next delta sends dave acme/widgets#7, which the test names.
func TestADeliveryViewStopsCarryingAProjectOnceItsReaderIsRestricted(t *testing.T) {
	const dave = "dave@example.com"
	ctx := context.Background()
	seed := seedPrivateProject(t)
	people := store.NewPgPeopleStore(seed.database.Pool, testSigningKey, nil)
	if err := people.SignInSubject(ctx, dave, "dave-subject", "refresh-dave", "", model.MemberFull); err != nil {
		t.Fatalf("record dave's sign-in: %v", err)
	}
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/access/people/"+dave, nil, http.StatusOK)
	seedHubSettings(t, seed.database)
	clock := newNoonHubClock()
	now := clock.Now()
	rig := hubOn(t, seed.handler, seed.database, seed.deps, clock, nil)
	resolve := func(ctx context.Context) (model.Actor, error) {
		return seed.server.readerFor(ctx, model.BrowserActor(dave))
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, deliverySubscription{Sub: "t", View: presetView(deliveryLiveTimeline, 7, ""), Resolve: resolve})
	conn.snapshot(t, 5*time.Second, "t")
	rig.waitListening(t)
	// The connection's own re-check runs before the write below, so only the write's announcement
	// can read dave again.
	rig.waitFor(t, 0, 5*time.Second, "the re-check after the listener connected", func(note deliveryHubNote) bool {
		return note.what == "readers rechecked"
	})

	if err := people.ConfirmSubject(ctx, dave, "dave-subject", "refresh-dave", "refresh-dave-confirmed", "", model.MemberRestricted); err != nil {
		t.Fatalf("record dave as a restricted member: %v", err)
	}
	public := hubPR(7, now.Add(-time.Hour))
	public.IssueKey = new(seed.issue[seedPublic])
	upsertPR(t, seed.database, public)
	secret := hubPR(8, now.Add(-time.Hour))
	secret.Repo, secret.URL = "acme/secret", "https://github.com/acme/secret/pull/8"
	secret.IssueKey = new(seed.issue[seedPrivate])
	upsertPR(t, seed.database, secret)

	// Every frame dave's page is sent, snapshot or delta, names no pull request of PUB, up to the
	// first that names acme/secret#8, written after acme/widgets#7.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case frame := <-conn.frames:
			ids, raw := framePullRequests(frame)
			if slices.Contains(ids, "acme/widgets#7") {
				t.Fatalf("dave, restricted, was sent acme/widgets#7, a pull request of PUB: %.300s", raw)
			}
			if slices.Contains(ids, "acme/secret#8") {
				return
			}
		case <-deadline:
			t.Fatal("no frame dave's page was sent within 15s named acme/secret#8, a pull request of PRIV, whose list names him")
		}
	}
}

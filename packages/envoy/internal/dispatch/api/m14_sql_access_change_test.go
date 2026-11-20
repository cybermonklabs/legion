package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// m14Heartbeat is the socket heartbeat these tests run under, on which an open socket's events
// feed and the delivery hub resolve their readers again: short, so a re-check comes well inside
// m14Within.
const m14Heartbeat = 100 * time.Millisecond

// m14Within is how long these tests wait for the re-check an access change made by SQL alone owes:
// many heartbeats.
const m14Within = 10 * time.Second

// m14MakePUBPrivate makes PUB private by SQL alone: an update of its row that appends no event and
// announces nothing on access.ChangedChannel, as an operator's statement at a psql prompt does.
func m14MakePUBPrivate(t *testing.T, seed privateSeed) {
	t.Helper()
	if _, err := seed.database.Pool.Exec(t.Context(), `update projects set visibility = 'private' where key = $1`, seedPublic); err != nil {
		t.Fatalf("make PUB private by SQL alone: %v", err)
	}
}

// TestAProjectMadePrivateBySQLAloneEndsTheFeedOfWhoLostIt: PUB is made private by SQL alone, so no
// event alice's feed is sent, or could be, says that she no longer sees PUB. Her feed resolves her
// again on the socket's heartbeat, finds that what she sees moved, and ends (behind), as an access
// change made through the API ends it; on the feed she subscribes again, carol's next comment on
// PUB's issue never reaches her.
//
// Control: the feed resolves its caller only at an event that can move what it sees; alice's feed
// never ends, and the test fails waiting for its end.
func TestAProjectMadePrivateBySQLAloneEndsTheFeedOfWhoLostIt(t *testing.T) {
	seed := seedPrivateProjectWith(t, testServerOptions{streamHeartbeat: m14Heartbeat})
	client := seed.dialLive(t, liveURLOf(t, seed.handler), asUser(seedAlice))
	subscribeEvents(t, client, "feed", nil)
	issue := "/api/v1/issues/" + seed.issue[seedPublic] + "/comments"
	seen := createdID(t, seed.request(t, callerCarol, http.MethodPost, issue, map[string]any{"body": "Before PUB is made private."}))
	read := liveEventsThrough(t, client, "feed", commentCreated(seen))
	applied := read[len(read)-1].ID

	m14MakePUBPrivate(t, seed)
	if frame := client.next(t, m14Within, "alice's feed's end after PUB was made private by SQL alone"); frame != liveEnded("feed", liveEndedBehind) {
		t.Fatalf("after PUB was made private by SQL alone alice's socket sent %+v, want her feed's end, behind", frame)
	}
	subscribeEventsStart(t, client, "feed", &applied)
	createdID(t, seed.request(t, callerCarol, http.MethodPost, issue, map[string]any{"body": "After PUB is made private."}))
	seed.markAgentState(t, seedAlice, "s1")
	requireNoEventOf(t, "alice", "feed", seedPublic, liveEventsThrough(t, client, "feed", agentStateOf(seedAlice)))
}

// TestAnAccessChangeBySQLAloneReadsADeliveryViewAgain: an access change made by SQL alone appends
// no event and announces nothing on access.ChangedChannel, and moves what a subscriber sees all the
// same. The hub resolves every subscriber's reader again on the socket's heartbeat, and moves the
// one that moved to the view of the reader it is now, answered with a snapshot that names no pull
// request of a project it no longer sees, as an access change made through the API moves it: alice
// once PUB is made private, and dave once he is taken off PRIV's list.
//
// Control: the hub resolves its subscribers' readers again only on an announcement, an event that
// can move them, or its listener's connection; no snapshot comes, and the test fails at its
// deadline.
func TestAnAccessChangeBySQLAloneReadsADeliveryViewAgain(t *testing.T) {
	const dave = "dave@example.com"
	for _, change := range []struct {
		name, login, project string
		grant, change        func(t *testing.T, seed privateSeed)
	}{
		{
			name: "a project made private", login: seedAlice, project: seedPublic,
			grant:  func(*testing.T, privateSeed) {},
			change: m14MakePUBPrivate,
		},
		{
			name: "a person taken off a list", login: dave, project: seedPrivate,
			grant: func(t *testing.T, seed privateSeed) {
				seedPeople(t, seed.database, dave)
				seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/access/people/"+dave, nil, http.StatusOK)
			},
			change: func(t *testing.T, seed privateSeed) {
				if _, err := seed.database.Pool.Exec(t.Context(), `
					delete from project_access where project_key = $1 and principal_kind = 'person' and principal = $2
				`, seedPrivate, dave); err != nil {
					t.Fatalf("take dave off PRIV's list by SQL alone: %v", err)
				}
			},
		},
	} {
		t.Run(change.name, func(t *testing.T) {
			seed := seedPrivateProjectWith(t, testServerOptions{streamHeartbeat: m14Heartbeat})
			change.grant(t, seed)
			seedHubSettings(t, seed.database)
			clock := newNoonHubClock()
			pr := hubPR(7, clock.Now().Add(-time.Hour))
			pr.IssueKey = new(seed.issue[change.project])
			upsertPR(t, seed.database, pr)
			const shown = "acme/widgets#7"
			rig := hubOn(t, seed.handler, seed.database, seed.deps, clock, nil)
			conn := newFakeLiveConn()
			rig.hub.Subscribe(conn, deliverySubscription{Sub: "t", View: presetView(deliveryLiveTimeline, 7, ""), Resolve: func(ctx context.Context) (model.Actor, error) {
				return seed.server.readerFor(ctx, model.BrowserActor(change.login))
			}})
			if ids, raw := framePullRequests(conn.snapshot(t, 5*time.Second, "t")); !slices.Contains(ids, shown) {
				t.Fatalf("%s's first snapshot names no %s, a pull request of %s's issue: %.300s", change.login, shown, change.project, raw)
			}
			rig.settle(t)

			change.change(t, seed)
			deadline := time.After(m14Within)
			for {
				select {
				case frame := <-conn.frames:
					snapshot, ok := frame.(DeliveryLiveSnapshot)
					if !ok {
						continue
					}
					ids, raw := framePullRequests(snapshot)
					if slices.Contains(ids, shown) {
						t.Fatalf("%s's page was sent a snapshot naming %s after it lost %s: %.300s", change.login, shown, change.project, raw)
					}
					return
				case <-deadline:
					t.Fatalf("no snapshot within %s read %s's timeline again after %s by SQL alone: it is read for the reader that saw %s",
						m14Within, change.login, change.name, change.project)
				}
			}
		})
	}
}

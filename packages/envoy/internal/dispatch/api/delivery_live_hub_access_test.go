package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestADeliveryViewIsReadAgainForItsReaderOnAnAccessChange: an access change announces itself on
// store.AccessChangedChannel as it commits, and the hub resolves every subscriber's reader again
// before its next recompute. dave, a full member PRIV's list names, holds a timeline. He is taken
// off the list in a write that appends no event, so the live event feed carries nothing that could
// move what he sees; only the announcement does. acme/secret#7, a pull request of PRIV's issue,
// then reaches dave's page in no frame. acme/widgets#8, which names no issue and so is shown to
// every open viewer, written after it, does: the read that names it has read acme/secret#7's row
// as well, and would name it too were it made for the reader dave had before. The test ends on
// that frame, and fails when no frame names it.
//
// Control: the hub does not listen on store.AccessChangedChannel. dave's page is sent
// acme/secret#7 with the next delta, read for the reader he had before, and the test fails naming
// it.
func TestADeliveryViewIsReadAgainForItsReaderOnAnAccessChange(t *testing.T) {
	t.Parallel()
	const dave = "dave@example.com"
	seed := seedPrivateProject(t)
	seedPeople(t, seed.database, dave)
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
	conn.snapshot(t, "t")
	rig.waitListening(t)
	// The connection's own re-check (a notification may have gone by unheard while it was down)
	// runs before the change below, so only the change's announcement can read dave again.
	rig.waitFor(t, 0, "the re-check after the listener connected", func(note deliveryHubNote) bool {
		return note.what == "readers rechecked"
	})

	tx, err := seed.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin taking dave off PRIV's list: %v", err)
	}
	if _, err := tx.Exec(t.Context(), `delete from project_access where project_key = 'PRIV' and principal_kind = 'person' and principal = $1`, dave); err != nil {
		t.Fatalf("take dave off PRIV's list: %v", err)
	}
	if _, err := tx.Exec(t.Context(), `select pg_notify($1, '')`, store.AccessChangedChannel); err != nil {
		t.Fatalf("announce the change: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit taking dave off PRIV's list: %v", err)
	}
	secret := hubPR(7, now.Add(-time.Hour))
	secret.Repo, secret.URL = "acme/secret", "https://github.com/acme/secret/pull/7"
	secret.IssueKey = new(seed.issue[seedPrivate])
	upsertPR(t, seed.database, secret)
	upsertPR(t, seed.database, hubPR(8, now.Add(-time.Hour)))

	// Every frame dave's page is sent, snapshot or delta, names no pull request of acme/secret,
	// up to the first that names acme/widgets#8, written after acme/secret#7: a read that names
	// acme/widgets#8 has read acme/secret#7's row as well.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case sent := <-conn.frames:
			frame := sent.frame
			ids, raw := framePullRequests(frame)
			if slices.Contains(ids, "acme/secret#7") {
				t.Fatalf("dave, no longer on PRIV's list, was sent acme/secret#7, a pull request of PRIV: %.300s", raw)
			}
			if slices.Contains(ids, "acme/widgets#8") {
				return
			}
		case <-deadline:
			t.Fatal("no frame dave's page was sent within 10s named acme/widgets#8, a pull request of PUB he sees")
		}
	}
}

// framePullRequests is the id of every pull request a delivery frame names, a snapshot's body's
// and a delta's upserts, with the frame's JSON for a failure to show.
func framePullRequests(frame any) ([]string, []byte) {
	raw, _ := json.Marshal(frame)
	var answer struct {
		Body  json.RawMessage `json:"body"`
		Items map[string]struct {
			Upsert []struct {
				ID string `json:"id"`
			} `json:"upsert"`
		} `json:"items"`
	}
	_ = json.Unmarshal(raw, &answer)
	var timeline struct {
		PRs []struct {
			ID string `json:"id"`
		} `json:"prs"`
	}
	_ = json.Unmarshal(answer.Body, &timeline)
	ids := []string{}
	for _, pr := range timeline.PRs {
		ids = append(ids, pr.ID)
	}
	for _, pr := range answer.Items["prs"].Upsert {
		ids = append(ids, pr.ID)
	}
	return ids, raw
}

// TestASubscribeThatRacesAReaderRecheckIsReadAgain: a subscribe resolves its reader before it
// takes the hub's lock, so a re-check of every sub's reader can begin, and walk the subs, between
// the two; the sub joins with a reader that re-check never read. The hub resolves that sub's reader
// again after the re-check. The racing subscribe here resolves a reader that sees PRIV, as one
// resolved before an access change would, and a re-check runs to its end before the subscribe
// joins; resolved again, the reader sees PRIV no more, and the page is sent a snapshot that names
// no pull request of PRIV's issue.
//
// Control: the hub does not resolve a sub's reader again when its subscribe raced a re-check; the
// page holds the snapshot read for the reader that saw PRIV, and the test fails.
func TestASubscribeThatRacesAReaderRecheckIsReadAgain(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	seedHubSettings(t, seed.database)
	clock := newNoonHubClock()
	secret := hubPR(7, clock.Now().Add(-time.Hour))
	secret.Repo, secret.URL = "acme/secret", "https://github.com/acme/secret/pull/7"
	secret.IssueKey = new(seed.issue[seedPrivate])
	upsertPR(t, seed.database, secret)
	saw, err := seed.server.readerFor(t.Context(), model.BrowserActor(seedCarol))
	if err != nil {
		t.Fatalf("resolve a reader that sees PRIV: %v", err)
	}
	sees, err := seed.server.readerFor(t.Context(), model.BrowserActor(seedAlice))
	if err != nil {
		t.Fatalf("resolve a reader that does not see PRIV: %v", err)
	}
	rig := hubOn(t, seed.handler, seed.database, seed.deps, clock, nil)

	// A first page starts the generation; the re-check the listener's connection runs is over
	// before the race below.
	first := newFakeLiveConn()
	rig.hub.Subscribe(first, deliverySubscription{Sub: "t", View: presetView(deliveryLiveTimeline, 7, ""), Resolve: func(context.Context) (model.Actor, error) { return sees, nil }})
	first.snapshot(t, "t")
	rig.waitListening(t)
	rig.waitFor(t, 0, "the re-check after the listener connected", func(note deliveryHubNote) bool {
		return note.what == "readers rechecked"
	})

	resolved := 0
	resolve := func(context.Context) (model.Actor, error) {
		resolved++
		if resolved > 1 {
			return sees, nil
		}
		start := rig.mark()
		rig.hub.mu.Lock()
		g := rig.hub.generation
		rig.hub.mu.Unlock()
		g.readersStale.Store(true)
		signal(g.wakeWork)
		rig.waitFor(t, start, "the re-check the subscribe races", func(note deliveryHubNote) bool {
			return note.what == "readers rechecked"
		})
		return saw, nil
	}
	conn := newFakeLiveConn()
	if !rig.hub.Subscribe(conn, deliverySubscription{Sub: "t", View: presetView(deliveryLiveTimeline, 7, ""), Resolve: resolve}) {
		t.Fatal("the racing subscribe was refused")
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case sent := <-conn.frames:
			snapshot, ok := sent.frame.(DeliveryLiveSnapshot)
			if !ok {
				continue
			}
			if ids, _ := framePullRequests(snapshot); !slices.Contains(ids, "acme/secret#7") {
				return
			}
		case <-deadline:
			t.Fatal("no snapshot within 10s was read for the reader the racing subscribe has now: each names acme/secret#7")
		}
	}
}

// TestADeliveryViewIsReadAgainForItsReaderAtANewProject: a new project is public, so every full
// member sees it as it is made, and its project.created event, which the relay's feed carries and
// nothing announces on store.AccessChangedChannel, has the hub resolve every subscriber's reader
// again. alice, a full member, holds a timeline; carol makes project NEW and an issue in it, and
// acme/fresh#9, a pull request of that issue, then reaches alice's page.
//
// Control: the hub's intake takes project.created as it takes any other event; alice's view stays
// read for the reader that saw no NEW, and no frame names acme/fresh#9.
func TestADeliveryViewIsReadAgainForItsReaderAtANewProject(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	seedHubSettings(t, seed.database)
	clock := newNoonHubClock()
	rig := hubOn(t, seed.handler, seed.database, seed.deps, clock, nil)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, deliverySubscription{Sub: "t", View: presetView(deliveryLiveTimeline, 7, ""), Resolve: func(ctx context.Context) (model.Actor, error) {
		return seed.server.readerFor(ctx, model.BrowserActor(seedAlice))
	}})
	conn.snapshot(t, "t")
	rig.waitListening(t)
	rig.settle(t)

	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects", map[string]string{"key": "NEW", "name": "Project NEW"}, http.StatusCreated)
	created := decodeBody[struct {
		Key string `json:"key"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]string{"project": "NEW", "title": "The issue of NEW"}, http.StatusCreated))
	fresh := hubPR(9, clock.Now().Add(-time.Hour))
	fresh.Repo, fresh.URL = "acme/fresh", "https://github.com/acme/fresh/pull/9"
	fresh.IssueKey = &created.Key
	upsertPR(t, seed.database, fresh)

	deadline := time.After(10 * time.Second)
	for {
		select {
		case sent := <-conn.frames:
			frame := sent.frame
			if ids, _ := framePullRequests(frame); slices.Contains(ids, "acme/fresh#9") {
				return
			}
		case <-deadline:
			t.Fatal("no frame alice's page was sent within 10s named acme/fresh#9, a pull request of NEW, a project made public after she subscribed")
		}
	}
}

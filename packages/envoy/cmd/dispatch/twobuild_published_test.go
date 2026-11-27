//go:build twobuild

// The published-destinations half of the two-build suite: what this build's store holds of an
// event the build before it publishes after this build's block-index tick has read the event
// (twobuild_harness_test.go holds the harness every half starts its builds with).

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/testnats"
)

// lateRouteSession is the session the issue of this file's test is routed to.
const lateRouteSession = "ses-twobuild-late-route"

// An untargeted issue message the previous build's outbox publishes late, after this build's
// block-index tick has read its message.created, reaches the store as a delivered_to to the
// session the issue is routed to, with no second backfill, and the queue that carries it there
// reads empty at the end.
//
// The outbox of the store's own release states a delivered_to for a comment's event alone, so its
// record of a message's event, which adds the route's session topic to published_destinations,
// states none, and it appends no event and lands after the event commits. The trigger of migration
// 20261122100122 queues the event as that record lands (events_published_owed), gated on the
// marker writer setting the previous build's connections do not carry, and this build's tick
// states the queued event's delivered_to as this build's outbox states one
// (graphcensus.CatchUpOwedRows).
//
// Each build reaches NATS through a proxy of its own. The previous build starts first and takes
// the publisher hold; after the backfill, both proxies are cut, so the previous build gives the
// hold up once its NATS has been gone for natsLossGrace and this build, whose NATS is gone too,
// does not take it. The previous build's process then stops, as a rolling restart stops one, so
// this build's ticks hold the block-index lease, which the two builds share and the one whose
// ticks come first in each period can hold every period. An issue message is written, and this
// build's tick reads its event while no outbox can publish it. A process of the previous build
// starts again with its NATS back, takes the hold and publishes the event with no tuple; once it
// has, it stops, and only then does this build's NATS come back. This build's next tick must state
// the delivery and empty the queue, with no second backfill. This build's outbox then holds the
// hold, and a second message it publishes itself must queue nothing: the gate the trigger shares
// with the marker triggers keeps this build's own record out. The census's published_destinations
// line reads no row no tuple states, no tuple no row projects, and no row of the queue. Its
// control is the trigger dropped from the migration: the previous build's record queues nothing,
// and the delivery stays owed.
func TestTheOlderBuildsLatePublishOfAnIssueMessageReachesTheStore(t *testing.T) {
	builds := newTwoBuilds(t)
	previousNATS := testnats.StartProxy(t, builds.nats)
	currentNATS := testnats.StartProxy(t, builds.nats)
	previous := startBuildThrough(t, builds, true, previousNATS)
	waitForPublisherHold(t, builds, true, 30*time.Second)
	current := startBuildThrough(t, builds, false, currentNATS)
	t.Logf("the %s holds the publisher hold and serves beside the %s", builds.previousName(), builds.currentName())

	project := uniqueProjectKey(t, "PUBL")
	current.createProject(t, project, "An older build's late publish")
	issue, _ := current.createIssueIn(t, project, "Routed to a session", "The spec.\n")
	current.call(t, http.MethodPatch, "/api/v1/issues/"+issue, fmt.Sprintf(`{"route":%q}`, "session:"+lateRouteSession), http.StatusOK)
	waitForEveryEventPublished(t, builds.database, 30*time.Second)
	runCurrentCommand(t, builds, []int{0}, "backfill-graph-store")

	// Neither build can publish now, and the previous build's process leaves.
	previousNATS.Cut()
	currentNATS.Cut()
	waitForPublisherHold(t, builds, false, 30*time.Second)
	stop(t, previous)
	message, event := postIssueMessage(t, builds, current, issue, "For whoever the issue is routed to.")
	waitForThisBuildsCursor(t, builds, event, 90*time.Second)
	topic := contracts.AgentTopicPrefix + lateRouteSession
	if published := publishedTo(t, builds, event, topic); published {
		t.Fatalf("event %d was published to %s before this build's tick read it, so its record proves nothing", event, topic)
	}

	// A process of the previous build starts again with its NATS back: it takes the hold and
	// records the event after this build's tick has read it, and leaves.
	previousNATS.Restore()
	previous = startBuildThrough(t, builds, true, previousNATS)
	waitForPublishedTo(t, builds, event, topic, 60*time.Second)
	stop(t, previous)
	currentNATS.Restore()
	// An event after this build's cursor, which its next tick reads; the queue makes that tick owe
	// its catch-up whatever the event is.
	current.call(t, http.MethodPatch, "/api/v1/issues/"+issue, `{"title":"Routed to a session, and renamed"}`, http.StatusOK)
	waitForTheDelivery(t, builds, event, message, 120*time.Second)

	second, secondEvent := postIssueMessage(t, builds, current, issue, "And again, through this build's outbox.")
	waitForPublishedTo(t, builds, secondEvent, topic, 60*time.Second)
	if queued := queuedPublishes(t, builds); queued != 0 {
		t.Errorf("events_published_owed holds %d row(s) after this build's outbox recorded event %d, want none: this build's own record must queue nothing", queued, secondEvent)
	}
	if !delivered(t, builds, secondEvent, second) {
		t.Errorf("this build's outbox recorded event %d as published to %s and stated no delivered_to of message %s under it", secondEvent, topic, second)
	}
	requirePublishedCensusLine(t, builds)
}

// startBuildThrough starts a process of the previous build (previous) or of this tree, reaching
// NATS through proxy, and returns once it answers /healthz, as startPrevious and startCurrent do.
func startBuildThrough(t *testing.T, builds *twoBuilds, previous bool, proxy *testnats.Proxy) *dispatchProcess {
	t.Helper()
	options := builds.options(nil)
	options.NATSURL = proxy.URL()
	if previous {
		options.Binary, options.WebDist = builds.build.path("envoy-dispatch"), builds.build.path("web")
		builds.a = startDispatchProcessWith(t, builds.databaseURL(t), options)
		builds.a.waitHealthy(t)
		return builds.a
	}
	options.Binary = builds.binary
	builds.b = startDispatchProcessWith(t, builds.databaseURL(t), options)
	builds.b.waitHealthy(t)
	return builds.b
}

// waitForPublisherHold waits until some process holds the outbox's publisher hold (held), or none
// does (not held), within within.
func waitForPublisherHold(t *testing.T, builds *twoBuilds, held bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var holds bool
		if err := builds.database.Pool.QueryRow(context.Background(), `
			select exists (select 1 from duty_holds where name = 'dispatch-outbox' and expires_at > now())
		`).Scan(&holds); err != nil {
			t.Fatalf("read the publisher hold: %v", err)
		}
		if holds == held {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the publisher hold is held %t after %s, want %t", holds, within, held)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// postIssueMessage posts an untargeted message on issue through p and answers its id and the id of
// its message.created.
func postIssueMessage(t *testing.T, builds *twoBuilds, p *dispatchProcess, issue, body string) (string, int64) {
	t.Helper()
	answer := p.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/messages", fmt.Sprintf(`{"body":%q}`, body), http.StatusCreated)
	var message struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer, &message); err != nil || message.ID == "" {
		t.Fatalf("post a message on %s: %v: %s", issue, err, answer)
	}
	var event int64
	if err := builds.database.Pool.QueryRow(context.Background(), `
		select id from events where type = 'message.created' and payload->>'id' = $1
	`, message.ID).Scan(&event); err != nil {
		t.Fatalf("read the message.created of message %s: %v", message.ID, err)
	}
	return message.ID, event
}

// waitForThisBuildsCursor waits until this build's block-index event cursor (its row of
// block_index_event_cursors, under the empty commit an unstamped build registers) has read or
// passed event.
func waitForThisBuildsCursor(t *testing.T, builds *twoBuilds, event int64, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var cursor int64
		if err := builds.database.Pool.QueryRow(context.Background(), `
			select coalesce((select event_id from block_index_event_cursors where build_commit = ''), 0)
		`).Scan(&cursor); err != nil {
			t.Fatalf("read this build's block-index cursor: %v", err)
		}
		if cursor >= event {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("this build's block-index cursor stands at %d after %s, short of event %d", cursor, within, event)
		}
		time.Sleep(time.Second)
	}
}

// publishedTo reports whether event's published_destinations hold topic.
func publishedTo(t *testing.T, builds *twoBuilds, event int64, topic string) bool {
	t.Helper()
	var published bool
	if err := builds.database.Pool.QueryRow(context.Background(), `
		select $2 = any(published_destinations) from events where id = $1
	`, event, topic).Scan(&published); err != nil {
		t.Fatalf("read the published destinations of event %d: %v", event, err)
	}
	return published
}

// waitForPublishedTo waits until event's published_destinations hold topic.
func waitForPublishedTo(t *testing.T, builds *twoBuilds, event int64, topic string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !publishedTo(t, builds, event, topic) {
		if time.Now().After(deadline) {
			t.Fatalf("event %d was not published to %s within %s", event, topic, within)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// queuedPublishes is how many rows events_published_owed holds.
func queuedPublishes(t *testing.T, builds *twoBuilds) int {
	t.Helper()
	var queued int
	if err := builds.database.Pool.QueryRow(context.Background(), `select count(*) from events_published_owed`).Scan(&queued); err != nil {
		t.Fatalf("count the rows of events_published_owed: %v", err)
	}
	return queued
}

// delivered reports whether the store states a delivered_to out of message to lateRouteSession
// under event.
func delivered(t *testing.T, builds *twoBuilds, event int64, message string) bool {
	t.Helper()
	var stated bool
	if err := builds.database.Pool.QueryRow(context.Background(), `
		select exists (
			select 1 from graph_tuples
			where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5 and source_event = $6 and asserted)
	`, graphstore.KindMessage, message, graphstore.TupleDeliveredTo, graphstore.KindSession, lateRouteSession, event).Scan(&stated); err != nil {
		t.Fatalf("read the delivery of message %s under event %d: %v", message, event, err)
	}
	return stated
}

// waitForTheDelivery waits until the store states the delivered_to of message to lateRouteSession
// under event and events_published_owed holds no row, and fails naming the census kind otherwise.
func waitForTheDelivery(t *testing.T, builds *twoBuilds, event int64, message string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		stated, queued := delivered(t, builds, event, message), queuedPublishes(t, builds)
		if stated && queued == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s the store states the delivered_to of message %s to session %s under event %d: %t, and events_published_owed holds %d row(s); want it stated and none queued, or published_destinations keeps a row no tuple states with no second backfill",
				within, message, lateRouteSession, event, stated, queued)
		}
		time.Sleep(time.Second)
	}
}

// requirePublishedCensusLine runs this tree's census and fails unless its published_destinations
// line reads no row no tuple states and no tuple no row projects, and its unmarked line no row of
// events_published_owed.
func requirePublishedCensusLine(t *testing.T, builds *twoBuilds) {
	t.Helper()
	census := runCurrentCommand(t, builds, []int{0, 1}, "census", "--store")
	var kind, unmarked string
	for _, line := range strings.Split(census, "\n") {
		switch {
		case strings.HasPrefix(line, "census: store kind=published_destinations tuples="):
			kind = line
		case strings.HasPrefix(line, "census: store kind=published_destinations unmarked_rows="):
			unmarked = line
		}
	}
	t.Logf("the census reads %q and %q", kind, unmarked)
	fields := strings.Fields(kind)
	for _, want := range []string{"rows_without_a_tuple=0", "tuples_without_a_row=0"} {
		if !slices.Contains(fields, want) {
			t.Errorf("the census's published_destinations line reads %q, want %s:\n%s", kind, want, census)
		}
	}
	if !strings.HasPrefix(unmarked, "census: store kind=published_destinations unmarked_rows=0 ") {
		t.Errorf("the census's published_destinations unmarked line reads %q, want unmarked_rows=0:\n%s", unmarked, census)
	}
}

package main

// A listener whose NATS user holds none of the session consumers' calls - Dispatch's, a host's - runs
// no takeover pass: the server says so, and the listeners that hold the calls copy what the previous
// holder did not read.

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// grantHub is a server with JetStream whose one account holds two users: listener, which may
// publish and subscribe to every subject, as a shared listener's user may for all a row does; and
// dispatch, which may do the same but publish none of the calls natsgrant.Listener holds and
// natsgrant.Dispatch does not - the session consumers' calls - as Dispatch's user. It returns the
// server and its URL as each user.
func grantHub(t *testing.T) (server *testnats.Server, listenerURL, dispatchURL string) {
	t.Helper()
	const machine = "grant-hub"
	dispatch := natsgrant.Dispatch(machine).Publish
	var denied []string
	for _, subject := range natsgrant.Listener(machine).Publish {
		if !slices.Contains(dispatch, subject) {
			denied = append(denied, strconv.Quote(subject))
		}
	}
	if len(denied) == 0 {
		t.Fatal("natsgrant.Listener holds no call natsgrant.Dispatch does not: the session consumers' calls are gone from the one or in the other")
	}
	config := fmt.Sprintf(`jetstream {}
authorization {
  users = [
    { user: listener, password: listener, permissions: { publish: { allow: [">"] }, subscribe: { allow: [">"] } } },
    { user: dispatch, password: dispatch, permissions: { publish: { allow: [">"], deny: [%s] }, subscribe: { allow: [">"] } } }
  ]
}
`, strings.Join(denied, ", "))
	server, uri := testnats.StartConfig(t, config, []string{"-js"}, false)
	as := func(user string) string {
		parsed, err := url.Parse(uri)
		if err != nil {
			t.Fatalf("parse the server's URL %q: %v", uri, err)
		}
		parsed.User = url.UserPassword(user, user)
		return parsed.String()
	}
	return server, as("listener"), as("dispatch")
}

// answeredBy is a consumerGrantAnswer that answers with holds, for a test that builds a
// configuration (listenerDeliveryHandlerConfig.consumerGrant).
func answeredBy(holds func(context.Context) bool) *consumerGrantAnswer {
	a := &consumerGrantAnswer{}
	a.set(holds)
	return a
}

// dispatchListener is a listener of the rig's deployment that connects as the grant hub's dispatch
// user, on the row's clock and with the rig's takeover seams. Its takeover grant's answer is a
// session delivery of its own, on that user, as main.go wires it.
func (rig *crashRig) dispatchListener(dispatchURL string) listenerDeliveryHandlerConfig {
	rig.t.Helper()
	client, err := bus.ConnectOwningStream([]string{dispatchURL}, bus.WithReplicas(1))
	if err != nil {
		rig.t.Fatalf("connect as the dispatch user: %v", err)
	}
	rig.t.Cleanup(client.Close)
	cfg := newListenerDeliveryHarnessOn(rig.t, client).config
	cfg.now = rig.clock.Now
	cfg.takeoverFaults = rig.harness.config.takeoverFaults
	delivery, err := connectSessionDelivery([]string{dispatchURL}, 0, client, cfg.logger)
	if err != nil {
		rig.t.Fatalf("open the dispatch user's session delivery: %v", err)
	}
	rig.t.Cleanup(delivery.close)
	cfg.consumerGrant = answeredBy(delivery.holdsConsumerGrant)
	return wireTestRoleForwarder(rig.t, cfg)
}

// refusedProbes is how many reads of a session consumer the server refused the dispatch user.
func refusedProbes(server *testnats.Server) int {
	refused := 0
	for line := range strings.SplitSeq(server.Log(), "\n") {
		if strings.Contains(line, "Publish Violation") && strings.Contains(line, "$JS.API.CONSUMER.INFO."+bus.Stream+".") {
			refused++
		}
	}
	return refused
}

// Dispatch's listener connects as a user that holds none of the session consumers' calls
// (natsgrant.Dispatch), so it cannot run a record's first copy, or the first after the previous
// holder came back: each begins with a read of the previous holder's floor, which it may not make.
// The listeners that hold the calls run each record's first copy, which sets its Next. At
// every sweep pass, Dispatch's listener still read every record, and every previous holder past
// Next: a 404 each, the third of the burst. Its claim route's handover did the same for the role it
// granted. Now neither reads a record nor a previous holder: each asks the server whether its user
// may make the calls, with one read the server refuses.
//
// The control is the same records swept by a listener whose user holds the calls: it reads each
// record and each previous holder, so the wire reading sees those reads when they are made.
func TestAListenerWithoutTheSessionConsumerGrantRunsNoTakeoverPass(t *testing.T) {
	t.Parallel()
	server, listenerURL, dispatchURL := grantHub(t)
	rig := newCrashRigOn(t, listenerURL)
	const records, roles = 20, 5
	names := rig.burstRecords("nogrant", records, roles, 10*time.Minute, false)
	dispatch := rig.dispatchListener(dispatchURL)
	wire := watchTakeoverWire(t, listenerURL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sweepOnce(ctx, dispatch)
	if reads := wire.count(dispatch.client); reads.records != 0 || reads.previous != 0 {
		t.Fatalf("the sweep of the listener without the grant read %d takeover records and %d previous holders' subjects, want none", reads.records, reads.previous)
	}
	runTakenRoleDrain(dispatch, names[0])
	if reads := wire.count(dispatch.client); reads.records != 0 || reads.previous != 0 {
		t.Fatalf("the claim handover of the listener without the grant read %d takeover records and %d previous holders' subjects, want none", reads.records, reads.previous)
	}
	if refused := refusedProbes(server); refused == 0 {
		t.Fatalf("the server refused no session consumer read of the dispatch user, so no row asked it for the grant:\n%s", server.Log())
	}

	sweepOnce(ctx, rig.cfg())
	if reads := wire.count(rig.cfg().client); reads.records < records || reads.previous < records {
		t.Fatalf("control: the sweep of the listener with the grant read %d takeover records and %d previous holders' subjects, want at least %d of each", reads.records, reads.previous, records)
	}
}

// The copy still happens: the role's next holder receives what the previous holder did not read,
// from the pass of the listener whose user holds the session consumers' calls, while the claim's own
// handover and the sweep of Dispatch's listener, which holds none of them, copy nothing and read no
// record. The claim here starts no handover of its own (skipClaimHandover), as a claim through
// Dispatch's listener runs no pass, so the copy is the sweep's.
func TestATakeoverCopyCompletesOnTheListenerThatHoldsTheGrant(t *testing.T) {
	t.Parallel()
	_, listenerURL, dispatchURL := grantHub(t)
	rig := newCrashRigOn(t, listenerURL)
	const a, b, role = "ses_grant_a", "ses_grant_b", "grant"
	setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{skipClaimHandover: func(r string) bool { return r == role }})
	rig.gone(a)
	rig.claim(rig.away, a, role)
	unread := rig.sendTo(a, role, 1, "before the takeover")
	next := rig.relayed(b, answerDone)
	heard := hear(t, next)
	rig.claim(rig.mux, b, role)
	dispatch := rig.dispatchListener(dispatchURL)
	wire := watchTakeoverWire(t, listenerURL)

	runTakenRoleDrain(dispatch, role)
	rig.sweep(dispatch, role)
	if reads := wire.count(dispatch.client); reads.records != 0 || reads.previous != 0 {
		t.Fatalf("the listener without the grant read %d takeover records and %d previous holders' subjects, want none", reads.records, reads.previous)
	}
	if record := rig.onlyRecord(role, a); record.Next != 0 || record.Finished {
		t.Fatalf("the listener without the grant moved the takeover record: %+v", record)
	}
	if kept := rig.kept(role); len(kept) != 0 {
		t.Fatalf("the listener without the grant copied %v to the role's kept lane, want nothing", kept)
	}
	requireNoneHeard(t, heard, b, unread...)

	rig.sweep(rig.cfg(), role)
	if !heard.await(crashWatch, unread...) {
		t.Fatalf("the new holder never received the message the previous holder did not read; it received %v", heard.all())
	}
	if n := heard.count(unread[0]); n != 1 {
		t.Fatalf("the new holder received the unread message %d times, want once", n)
	}
}

// main.go hands the claim route and the sweep their answer from the listener's session delivery
// (holdsConsumerGrant), which remembers it as every consumer call's refusal is remembered: Dispatch's
// listener is refused one read per refusal episode (noGrantBackoff), however many passes ask, and asks
// the server again once the episode is over; a listener whose user holds the calls is answered yes
// with no read while a consumer call it made succeeded less than that long ago.
func TestTheTakeoverGrantIsAskedOncePerEpisode(t *testing.T) {
	t.Parallel()
	server, listenerURL, dispatchURL := grantHub(t)
	var clock stateClock
	delivery := func(url string) *sessionDelivery {
		t.Helper()
		client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		m, err := connectSessionDelivery([]string{url}, 0, client, newListenerDeliveryHarnessOn(t, client).config.logger)
		if err != nil {
			t.Fatalf("open the session delivery: %v", err)
		}
		t.Cleanup(m.close)
		m.now = clock.Now
		return m
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	dispatch := delivery(dispatchURL)
	for range 3 {
		if dispatch.holdsConsumerGrant(ctx) {
			t.Fatal("the dispatch user's delivery answered that it holds the session consumers' calls")
		}
	}
	if refused := refusedProbes(server); refused != 1 {
		t.Fatalf("three asks inside one episode were refused %d times by the server, want once:\n%s", refused, server.Log())
	}
	clock.advance(noGrantBackoff)
	if dispatch.holdsConsumerGrant(ctx) {
		t.Fatal("the dispatch user's delivery answered yes after its episode")
	}
	if refused := refusedProbes(server); refused != 2 {
		t.Fatalf("the ask after the episode was refused %d times in all, want twice: it asks the server again", refused)
	}

	listener := delivery(listenerURL)
	wire := watchTakeoverWire(t, listenerURL)
	for range 3 {
		if !listener.holdsConsumerGrant(ctx) {
			t.Fatal("the listener user's delivery answered that it does not hold the session consumers' calls")
		}
	}
	// The reads go out on the delivery's own connection, so the mark does too.
	reads := wire.countAfter(func() error { return listener.consumers.Publish(ctx, &natsgo.Msg{Subject: wire.mark}) })
	if reads.consumers != 1 {
		t.Fatalf("three asks of the listener user's delivery read %d session consumers, want one: the first answer counts as a call that succeeded", reads.consumers)
	}
}

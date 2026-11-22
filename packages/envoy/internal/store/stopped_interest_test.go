package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// rawInterest is sessionID's row as the bucket holds it, failing the test when it holds none.
func rawInterest(t *testing.T, kv natsgo.KeyValue, sessionID string) Interest {
	t.Helper()
	entry, err := kv.Get(sessionID)
	if err != nil {
		t.Fatalf("get %s's interest row: %v", sessionID, err)
	}
	var row Interest
	if err := json.Unmarshal(entry.Value(), &row); err != nil {
		t.Fatalf("decode %s's interest row: %v", sessionID, err)
	}
	return row
}

// reapGone runs one reaper pass over reg in which every session is gone and fails the test
// unless the pass did want.
func reapGone(t *testing.T, reg *Registry, want InterestSweep) {
	t.Helper()
	sweep, err := reg.Reap(func(string) (bool, error) { return false, nil }, InterestReapGrace)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if sweep != want {
		t.Fatalf("Reap = %+v, want %+v", sweep, want)
	}
}

// A stopped row keeps the session's subscriptions and nothing else: its own agent subject, which
// every registration's filter carries, a role's lane, which is the claim's half, the controller
// notice lane, which is its holder's only while it holds the role, and a topic no stream subject
// captures all go. A gone session whose row holds nothing else is deleted, as every gone session's
// row was before a row could be stopped.
func TestAStoppedRowKeepsTheSessionsSubscriptionsAndNothingElse(t *testing.T) {
	start := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	clock := &manualClock{at: start}
	reg, kv := clockedRegistry(t, clock)
	const (
		widgets = "notifications.github.acme.widgets.pr.3370.>"
		issue   = "notifications.dispatch.issue.ACME.ACME-7.>"
	)
	follower := upsertOrFail(t, reg, "ses_follower",
		"notifications.agent.ses_follower", "notifications.agent.ACME.ses_follower", widgets, issue)
	if !slices.Contains(follower.Topics, widgets) || !slices.Contains(follower.Topics, issue) {
		t.Fatalf("precondition: ses_follower's row holds %v", follower.Topics)
	}
	if _, err := reg.SetRole("ses_follower", "m1", "acme-worker", false); err != nil {
		t.Fatalf("claim ses_follower's role: %v", err)
	}
	putInterest(t, kv, Interest{SessionID: "ses_inbox_only", MachineID: "m1", Topics: []string{
		"notifications.agent.ses_inbox_only", "notifications.role.acme-worker-2",
		"notifications.legion.ACME.controller", "notifications.uncaptured",
	}, UpdatedAt: start.UnixMilli()})
	waitFor(t, 5*time.Second, func() bool {
		_, followerCached := reg.Cached("ses_follower")
		_, inboxCached := reg.Cached("ses_inbox_only")
		return followerCached && inboxCached
	})
	claimed := rawInterest(t, kv, "ses_follower")

	clock.Set(time.UnixMilli(claimed.UpdatedAt).Add(InterestReapAllowance(InterestReapGrace) + time.Millisecond))
	reapGone(t, reg, InterestSweep{Reaped: 1, Stopped: 1})

	stopped := rawInterest(t, kv, "ses_follower")
	if want := []string{issue, widgets}; !stopped.Stopped || stopped.MachineID != "" || stopped.UpdatedAt != claimed.UpdatedAt ||
		stopped.Dir != "/work/ses_follower" || !slices.Equal(stopped.Topics, want) {
		t.Fatalf("ses_follower's stopped row = %+v, want topics %v, stopped, naming no listener, at its last write %d", stopped, want, claimed.UpdatedAt)
	}
	if _, err := kv.Get("ses_inbox_only"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("ses_inbox_only's row after the pass: %v, want it deleted: it held no subscription", err)
	}
	// A stopped row is not stopped again: the next pass leaves it as it is.
	reapGone(t, reg, InterestSweep{})
}

// A stopped row routes nothing: no listener's Match fans a topic out to it, since it names no
// listener and every listener has a machine id of its own (ENVOY_MACHINE_ID is required). The
// publish count still finds it (MatchAllMachines), as it finds the session's known-session record,
// and so does the listener's followed-topic wake (MatchStopped), which finds no live row.
func TestAStoppedRowIsRoutedByNoListener(t *testing.T) {
	start := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	clock := &manualClock{at: start}
	reg, kv := clockedRegistry(t, clock)
	const widgets = "notifications.github.acme.widgets.pr.3370.>"
	const review = "notifications.github.acme.widgets.pr.3370.review"
	upsertOrFail(t, reg, "ses_follower", "notifications.agent.ses_follower", widgets)
	if got := reg.Match("m1", review); len(got) != 1 {
		t.Fatalf("precondition: Match(m1) of the live row = %+v, want it", got)
	}
	if got := reg.MatchStopped(review); len(got) != 0 {
		t.Fatalf("MatchStopped of the live row = %+v, want nothing", got)
	}
	// A row naming no listener that is not stopped, as no listener writes one, is no stopped row.
	putInterest(t, kv, Interest{SessionID: "ses_unowned", Topics: []string{widgets}, UpdatedAt: start.UnixMilli()})
	waitFor(t, 5*time.Second, func() bool { _, cached := reg.Cached("ses_unowned"); return cached })
	if got := reg.MatchStopped(review); len(got) != 0 {
		t.Fatalf("MatchStopped with a row naming no listener that is not stopped = %+v, want nothing", got)
	}
	if err := kv.Delete("ses_unowned"); err != nil {
		t.Fatalf("delete ses_unowned's row: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { _, cached := reg.Cached("ses_unowned"); return !cached })

	clock.Set(start.Add(InterestReapAllowance(InterestReapGrace) + time.Millisecond))
	reapGone(t, reg, InterestSweep{Stopped: 1})

	for _, machine := range []string{"m1", "m2"} {
		if got := reg.Match(machine, review); len(got) != 0 {
			t.Fatalf("Match(%q) of the stopped row = %+v, want nothing", machine, got)
		}
	}
	if got := reg.MatchAllMachines(review); len(got) != 1 || got[0].SessionID != "ses_follower" {
		t.Fatalf("MatchAllMachines of the stopped row = %+v, want ses_follower's", got)
	}
	if got := reg.MatchStopped(review); len(got) != 1 || got[0].SessionID != "ses_follower" {
		t.Fatalf("MatchStopped of the stopped row = %+v, want ses_follower's", got)
	}
	if got := reg.MatchStopped("notifications.github.acme.other.pr.9.review"); len(got) != 0 {
		t.Fatalf("MatchStopped of a topic the stopped row does not keep = %+v, want nothing", got)
	}
}

// The session's own acts bound a stopped row: an unsubscribe takes a topic out and leaves the
// rest stopped, and a deregistration (Remove with no topics) takes the whole row. Past the
// session's life, StoppedInterestRetention after the row's last write, the reaper deletes it, and
// not a moment before.
func TestAStoppedRowLastsUntilTheSessionUnsubscribesDeregistersOrEnds(t *testing.T) {
	start := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	clock := &manualClock{at: start}
	reg, kv := clockedRegistry(t, clock)
	const (
		widgets = "notifications.github.acme.widgets.pr.3370.>"
		tools   = "notifications.github.acme.tools.pr.205.>"
	)
	for _, id := range []string{"ses_unsubscribes", "ses_deregistered", "ses_ends"} {
		upsertOrFail(t, reg, id, "notifications.agent."+id, widgets, tools)
	}
	clock.Set(start.Add(InterestReapAllowance(InterestReapGrace) + time.Millisecond))
	reapGone(t, reg, InterestSweep{Stopped: 3})

	removed, err := reg.Remove(context.Background(), "ses_unsubscribes", []string{tools}, RoleReleaseAuthority{})
	if err != nil || !slices.Equal(removed, []string{tools}) {
		t.Fatalf("unsubscribe the stopped ses_unsubscribes from %s = %v, %v", tools, removed, err)
	}
	if row := rawInterest(t, kv, "ses_unsubscribes"); !row.Stopped || row.MachineID != "" || !slices.Equal(row.Topics, []string{widgets}) {
		t.Fatalf("ses_unsubscribes after its unsubscribe = %+v, want it stopped, keeping %s alone", row, widgets)
	}
	if _, err := reg.Remove(context.Background(), "ses_deregistered", nil, RoleReleaseAuthority{}); err != nil {
		t.Fatalf("deregister the stopped ses_deregistered: %v", err)
	}
	if _, err := kv.Get("ses_deregistered"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("ses_deregistered after its deregistration: %v, want no row", err)
	}

	clock.Set(start.Add(StoppedInterestRetention))
	reapGone(t, reg, InterestSweep{})
	if row := rawInterest(t, kv, "ses_ends"); !row.Stopped {
		t.Fatalf("ses_ends at the end of its life = %+v, want it still stopped", row)
	}
	clock.Set(start.Add(StoppedInterestRetention + time.Millisecond))
	reapGone(t, reg, InterestSweep{Reaped: 1})
	if _, err := kv.Get("ses_ends"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("ses_ends past its life: %v, want no row", err)
	}
	// The unsubscribe wrote ses_unsubscribes' row, which counts its life from that write.
	if row := rawInterest(t, kv, "ses_unsubscribes"); !row.Stopped {
		t.Fatalf("ses_unsubscribes, written by its unsubscribe, = %+v, want it still stopped", row)
	}
}

// A session that registers while a pass is deciding it is gone keeps its live row: the pass
// stops only the row it judged.
func TestAPassStopsNoRowItsSessionWroteDuringThePass(t *testing.T) {
	start := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	clock := &manualClock{at: start}
	reg, kv := clockedRegistry(t, clock)
	const widgets = "notifications.github.acme.widgets.pr.3370.>"
	upsertOrFail(t, reg, "ses_back", "notifications.agent.ses_back", widgets)
	clock.Set(start.Add(InterestReapAllowance(InterestReapGrace) + time.Millisecond))

	sweep, err := reg.Reap(func(sessionID string) (bool, error) {
		upsertWithin(t, reg, Interest{SessionID: sessionID, MachineID: "m2"}, []string{"notifications.agent." + sessionID})
		return false, nil
	}, InterestReapGrace)
	if err != nil || sweep != (InterestSweep{}) {
		t.Fatalf("Reap = %+v, %v; want nothing done", sweep, err)
	}
	if row := rawInterest(t, kv, "ses_back"); row.Stopped || row.MachineID != "m2" || !slices.Contains(row.Topics, widgets) {
		t.Fatalf("ses_back after registering during the pass = %+v, want its live row on m2 with %s", row, widgets)
	}
}

// A gone session's row stored under a key an earlier build wrote and this one refuses to write
// (testnats.LegacyKeys) is deleted even where it holds a subscription: no session can register
// under that key again, so there is nobody to keep it for, and a stop the bucket refuses for the
// key would leave it standing for the session's whole life.
func TestAGoneSessionsRowUnderAKeyNoBuildCanWriteIsDeletedRatherThanStopped(t *testing.T) {
	readable, unreadable := testnats.LegacyKeys(bus.InterestBucket, "s")
	const widgets = "notifications.github.acme.widgets.pr.3370.>"
	stale := time.Now().Add(-time.Hour).UnixMilli()
	registry, rawInterests, _ := openRegistryOver(t, func(_ natsgo.JetStreamContext, rawInterests, _ natsgo.KeyValue) {
		for _, sessionID := range []string{readable, unreadable} {
			putInterest(t, rawInterests, Interest{SessionID: sessionID, MachineID: "earlier", Topics: []string{widgets}, UpdatedAt: stale})
		}
	})
	reapGone(t, registry, InterestSweep{Reaped: 2})
	if keys, err := rawInterests.Keys(); !errors.Is(err, natsgo.ErrNoKeysFound) {
		t.Fatalf("the interest bucket after the pass holds %d keys (%v), want none", len(keys), err)
	}
}

// A registration that lands after a stop read the row, and before its write reaches the bucket,
// is kept: the write is made at the revision the stop read, which the registration moved, so the
// bucket refuses it and the pass counts nothing.
func TestAStopKeepsARowWrittenAfterItsRead(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, kv := coldRegistry(t, conn)
	const widgets = "notifications.github.acme.widgets.pr.3370.>"
	putInterest(t, kv, Interest{SessionID: "ses_raced", MachineID: "m1", Topics: []string{widgets}, UpdatedAt: time.Now().Add(-10 * time.Minute).UnixMilli()})
	if _, err := reg.Get(context.Background(), "ses_raced"); err != nil {
		t.Fatalf("warm ses_raced: %v", err)
	}
	fresh := Interest{SessionID: "ses_raced", MachineID: "m2", Topics: []string{widgets}, UpdatedAt: time.Now().UnixMilli()}
	current := reg.watcher.KV()
	raced := &putBeforeUpdateKeyValue{KeyValue: kv, beforeUpdate: func() { putInterest(t, kv, fresh) }}
	reg.watcher = kvwatch.New("interest registry", bus.NewKeyValue(raced, current.WatchHandle(), current.Streams()), interestCache{reg})

	sweep, err := reg.Reap(func(string) (bool, error) { return false, nil }, 0)
	if err != nil || sweep != (InterestSweep{}) {
		t.Fatalf("Reap = %+v, %v; want nothing done", sweep, err)
	}
	if raced.updates != 1 {
		t.Fatalf("the pass sent %d writes, want the one stop the registration raced", raced.updates)
	}
	if row := rawInterest(t, kv, "ses_raced"); row.Stopped || row.MachineID != "m2" {
		t.Fatalf("ses_raced = %+v, want the registration that landed during the stop", row)
	}
}

// putBeforeUpdateKeyValue runs beforeUpdate before its first Update reaches the bucket, and then
// sends that Update as it was asked to.
type putBeforeUpdateKeyValue struct {
	natsgo.KeyValue

	updates      int
	beforeUpdate func()
}

func (kv *putBeforeUpdateKeyValue) Update(key string, value []byte, last uint64) (uint64, error) {
	kv.updates++
	if kv.updates == 1 {
		kv.beforeUpdate()
	}
	return kv.KeyValue.Update(key, value, last)
}

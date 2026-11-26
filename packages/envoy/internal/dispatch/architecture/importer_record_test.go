package architecture

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// recordedKeys is how many keys the importer's record of ended flights holds an entry for.
func recordedKeys(process *Importer) int {
	process.ended.mu.Lock()
	defer process.ended.mu.Unlock()
	return len(process.ended.byKey)
}

// A call holds its key's entry open from before it stamps its call, so a flight whose starter
// hung up, which looks the head up after the call's stamp and ends before the call looks for a
// flight, is still recorded and answers the call. The starter's flight is held at its
// installation lookup, which comes before its head lookup, and the starter hangs up, so no call
// of the key holds the entry. A later call stamps its call and is held there (afterCalledAt).
// The flight is released, looks the head up and ends. The later call, released, takes that
// flight's answer: one head lookup in all, where a call that enters the key only after its stamp
// finds nothing recorded and makes a second.
func TestACallTakesAFlightThatLookedAfterItsStampThoughThatFlightsStarterHungUp(t *testing.T) {
	t.Parallel()
	rig := newRefusersRig(t, 0)
	installation := rig.holdNextInstallation(t)
	var stamped atomic.Int32
	laterStamped, held := make(chan struct{}), make(chan struct{})
	releaseLater := sync.OnceFunc(func() { close(held) })
	// Cleanups run last registered first: a test that fails while the later call is held lets it
	// go before Close waits for the flights.
	t.Cleanup(releaseLater)
	rig.process.afterCalledAt = func() {
		if stamped.Add(1) == 2 {
			close(laterStamped)
			<-held
		}
	}

	starterCtx, hangUp := context.WithCancel(context.Background())
	starter := make(chan syncAnswer, 1)
	go func() {
		source, err := rig.process.Sync(starterCtx, "CORE", archRepo)
		starter <- syncAnswer{source: source, err: err}
	}()
	arrived, _ := installation.channels(t)
	receiveBeforeTheDeadline(t, "the starter's flight to reach its installation lookup", arrived)
	hangUp()
	if answer := receiveBeforeTheDeadline(t, "the starter's answer once it hung up", starter); !errors.Is(answer.err, context.Canceled) {
		t.Fatalf("the starter that hung up answered %+v (err %v); want context.Canceled", answer.source, answer.err)
	}

	later := make(chan syncAnswer, 1)
	rig.syncOf(archRepo, later)
	receiveBeforeTheDeadline(t, "the later call to stamp its call", laterStamped)
	installation.releaseHeld(t)
	// A barrier on the flight's end: singleflight hands this call the answer of the flight still
	// in flight under the key, or runs it alone when none is, so once it answers, the starter's
	// flight has ended and recorded itself, and nothing is in flight under the key.
	receiveBeforeTheDeadline(t, "the starter's flight to end",
		rig.process.syncs.DoChan(syncKey("CORE", archRepo), func() (any, error) { return nil, nil }))
	releaseLater()
	answer := receiveBeforeTheDeadline(t, "the later call's answer", later)

	t.Logf("head lookups=%d later commit=%s err=%v", rig.lookups.Load(), commitOf(answer), answer.err)
	if answer.err != nil || commitOf(answer) != commitOne {
		t.Fatalf("the later call answered commit %s (err %v); want %s, the starter's flight's", commitOf(answer), answer.err, commitOne)
	}
	if got := rig.lookups.Load(); got != 1 {
		t.Fatalf("the starter's flight and the later call made %d head lookups, want 1: the flight's, which looked the head up after the later call's stamp", got)
	}
}

// The record hands a call an ended flight by the rule Sync holds every answer to,
// !servedAt().Before(calledAt): a flight whose head lookup went out at the very instant of the
// call is fresh for it, one whose lookup went out a nanosecond before the call is not, and a
// flight that failed before its lookup is dated by its start. A call that two clock readings put
// at the same instant as the lookup must take the flight, or a refuser of it starts one more.
// Once the key's last call has left, the record hands nothing back.
func TestARecordedFlightIsFreshForACallAtTheInstantOfItsHeadLookup(t *testing.T) {
	t.Parallel()
	var ended endedFlights
	at := time.Now()
	looked := &syncFlight{startedAt: at.Add(-time.Second), headAt: at}
	failedBefore := &syncFlight{startedAt: at}
	ended.enter("looked")
	ended.enter("failed")
	ended.record("looked", looked)
	ended.record("failed", failedBefore)
	for _, row := range []struct {
		what     string
		key      string
		calledAt time.Time
		want     *syncFlight
	}{
		{"a call at the instant of the head lookup", "looked", at, looked},
		{"a call a nanosecond before the head lookup", "looked", at.Add(-time.Nanosecond), looked},
		{"a call a nanosecond after the head lookup", "looked", at.Add(time.Nanosecond), nil},
		{"a call at the instant a flight that failed before its lookup began", "failed", at, failedBefore},
		{"a call a nanosecond after that flight began", "failed", at.Add(time.Nanosecond), nil},
		{"a key no call entered", "absent", time.Time{}, nil},
	} {
		if got := ended.fresh(row.key, row.calledAt); got != row.want {
			t.Errorf("%s: fresh = %p, want %p", row.what, got, row.want)
		}
	}
	ended.leave("looked")
	if got := ended.fresh("looked", time.Time{}); got != nil {
		t.Errorf("after the key's last call left: fresh = %p, want nil", got)
	}
}

// The sync route hands Sync whatever project key and repository its path names, whether a
// source reads that repository or not. Each sync of a repository no source reads answers
// ErrNoSource, with the key and the repository in its text, and leaves no entry in the record
// of ended flights once it has answered, so syncs that name keys of their own, however many,
// grow nothing that outlives them. A sync of a source that exists, whose flight loads the row
// and imports it, leaves none either.
func TestSyncsOfRepositoriesNoSourceReadsLeaveNoEntryOnceAnswered(t *testing.T) {
	t.Parallel()
	branch := newMovingBranch(t, commitOne, map[string]map[string]string{commitOne: validFiles()})
	client := newGitHubClient(t, branch.handler())
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'legion/arch', 'main', 11, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	process := NewImporter(database, client, events.NewBroker())
	t.Cleanup(process.Close)

	const syncs = 8
	for n := range syncs {
		project, repo := fmt.Sprintf("P%d", n), fmt.Sprintf("nobody/repository-%d", n)
		_, err := process.Sync(context.Background(), project, repo)
		if !errors.Is(err, ErrNoSource) {
			t.Fatalf("a sync of %s from %s: %v, want ErrNoSource", project, repo, err)
		}
	}
	unknown := recordedKeys(process)
	source, err := process.Sync(context.Background(), "CORE", archRepo)
	if err != nil || source.LastCommit == nil || *source.LastCommit != commitOne {
		t.Fatalf("the sync of CORE's source answered %+v (err %v); want the row at %s with no error", source, err, commitOne)
	}
	entries := recordedKeys(process)
	t.Logf("syncs=%d entries after the unknown ones=%d, after the source's=%d", syncs, unknown, entries)
	if unknown != 0 || entries != 0 {
		t.Fatalf("%d syncs of repositories no source reads left %d entries in the record of ended flights, and the sync of a source then left %d; want none once each has answered",
			syncs, unknown, entries)
	}
}

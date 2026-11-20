package architecture

import (
	"context"
	"errors"
	"fmt"
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

// The record hands a call an ended flight by the rule Sync holds every answer to,
// !servedAt().Before(calledAt): a flight whose head lookup went out at the very instant of the
// call is fresh for it, one whose lookup went out a nanosecond before the call is not, and a
// flight that failed before its lookup is dated by its start. A call that two clock readings put
// at the same instant as the lookup must take the flight, or a refuser of it starts one more.
// Once the key's last call has left, the record hands nothing back.
func TestARecordedFlightIsFreshForACallAtTheInstantOfItsHeadLookup(t *testing.T) {
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

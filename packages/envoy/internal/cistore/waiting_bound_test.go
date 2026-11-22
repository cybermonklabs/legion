package cistore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// waitingAtBound is a record as large as the bound allows of checks of a GitHub Actions suite
// whose event it does not know (900), each with two runs of other suites waiting behind it: an
// older one of suite 901 concluding older and a newer one of suite 902 concluding newer. events
// are the suite events the record holds. It answers the record and how many names it holds.
func waitingAtBound(t *testing.T, h head, older, newer string, events map[string]string) (State, int) {
	t.Helper()
	entry := func(i int, id uint64, suite, conclusion string) Check {
		return Check{
			Name: fmt.Sprintf("dispatch-browser / binaries %04d", i), CheckRunID: checkRunID(id),
			URL:    fmt.Sprintf("https://github.com/example-org/example-repo/actions/runs/44860489318/job/%d", id),
			Status: "completed", Conclusion: conclusion, ObservedAt: "2026-11-21T03:21:34Z",
			SuiteID: suite, AppID: contracts.GithubActionsAppID,
		}
	}
	st := State{Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha, Checks: map[string]Check{}, Waiting: map[string]Check{}, SuiteEvents: events}
	for i := 0; ; i++ {
		standing := entry(i, 136713000000+uint64(i), "900", "success")
		first, second := entry(i, 136711000000+uint64(i), "901", older), entry(i, 136712000000+uint64(i), "902", newer)
		st.Checks[standing.Name] = standing
		st.Waiting[waitingKey(first.CheckRunID)] = first
		st.Waiting[waitingKey(second.CheckRunID)] = second
		if recordSize(t, st) > maxRecordBytes {
			delete(st.Checks, standing.Name)
			delete(st.Waiting, waitingKey(first.CheckRunID))
			delete(st.Waiting, waitingKey(second.CheckRunID))
			break
		}
	}
	st.Schema = recordSchema
	st.LastEventAt = time.Now().UnixMilli()
	return st, len(st.Checks)
}

// recordSize is the size of st as the store writes it.
func recordSize(t *testing.T, st State) int {
	t.Helper()
	buf, err := encodeRecord(&st)
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	return len(buf)
}

// seedRecord writes st as h's record, as a write of the store would have left it.
func seedRecord(t *testing.T, store *Store, h head, st State) {
	t.Helper()
	buf, err := encodeRecord(&st)
	if err != nil {
		t.Fatalf("encode the seeded record: %v", err)
	}
	if _, err := store.watcher.KV().Put(h.key(), buf); err != nil {
		t.Fatalf("seed the record: %v", err)
	}
}

// pastTheBound is a check of another App whose link is long enough to take a record at its bound
// past it.
func pastTheBound(h head) contracts.CIObservation {
	return contracts.CIObservation{
		Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha, AppID: "57789",
		CheckName: "CodeQL", CheckRunID: 136720000001, Status: "completed", Conclusion: "success",
		URL: "https://github.com/example-org/example-repo/runs/" + strings.Repeat("9", 4096), ObservedAt: "2026-11-21T03:30:00Z",
	}
}

// plusCheck is st with the check observation records added.
func plusCheck(st State, observation contracts.CIObservation) State {
	with := st
	with.Checks = cloneChecks(st.Checks)
	with.Checks[observation.CheckName] = Check{Name: observation.CheckName, CheckRunID: checkRunID(observation.CheckRunID), URL: observation.URL,
		Status: observation.Status, Conclusion: observation.Conclusion, ObservedAt: observation.ObservedAt, AppID: observation.AppID}
	with.Waiting = cloneChecks(st.Waiting)
	return with
}

// standingOn counts the names of st, added's aside, whose check is suite's run concluding
// conclusion.
func standingOn(st State, added, suite, conclusion string) int {
	n := 0
	for name, check := range st.Checks {
		if name != added && check.SuiteID == suite && check.Conclusion == conclusion {
			n++
		}
	}
	return n
}

// The runs a record keeps waiting to stand for a name (State.Waiting) give way at the record's
// bound only where they can no longer stand. In the record below each name's newer waiting run,
// suite 902's failure, is of a pull_request run, so it stands for good once it stands, and the
// older one, suite 901's, never can. A check of another App takes the record past the bound with
// every waiting run and not without 901's. The write lands with the new check, it is not marked
// overflowed, and each name keeps 902's run alone. When suite 900 then turns out to be a
// workflow_dispatch run, 902's failure stands for each name, as it would have with nothing dropped.
func TestWaitingRunsThatCanNoLongerStandGiveWayAtTheRecordsBound(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	h := head{"example-org", "example-repo", "61", "3a171e00000000000000000000000000000000ff"}
	st, names := waitingAtBound(t, h, "success", "failure", map[string]string{"902": "pull_request"})
	seedRecord(t, store, h, st)

	added := pastTheBound(h)
	with := plusCheck(st, added)
	trimmed := plusCheck(st, added)
	if dropped := trimmed.trimWaiting(); dropped != names {
		t.Fatalf("trimming the waiting runs drops %d, want suite 901's run of each of the %d names", dropped, names)
	}
	if recordSize(t, with) <= maxRecordBytes || recordSize(t, trimmed) > maxRecordBytes {
		t.Fatalf("the record with the added check is %d bytes with its %d waiting runs and %d without suite 901's, want past the %d-byte bound only with all of them",
			recordSize(t, with), len(with.Waiting), recordSize(t, trimmed), maxRecordBytes)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.Record(ctx, added); err != nil {
		t.Fatalf("record a check past the bound the waiting runs that can no longer stand alone take the record past: %v", err)
	}
	got := getState(t, store, h.owner, h.repo, h.number, h.sha)
	if got.Overflowed || len(got.Waiting) != names || got.Checks[added.CheckName].CheckRunID != checkRunID(added.CheckRunID) || len(got.Checks) != names+1 {
		t.Fatalf("after the write the record is overflowed %v with %d waiting runs and %d checks (%v held), want not overflowed, %d waiting runs and the %d checks with %s",
			got.Overflowed, len(got.Waiting), len(got.Checks), got.Checks[added.CheckName], names, names+1, added.CheckName)
	}
	for _, check := range got.Waiting {
		if check.SuiteID != "902" {
			t.Fatalf("the record kept the waiting run %+v, want suite 902's alone", check)
		}
	}

	if err := store.RecordRunEvent(ctx, contracts.CIObservation{Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha,
		SuiteID: "900", RunEvent: "workflow_dispatch"}); err != nil {
		t.Fatalf("record suite 900's event: %v", err)
	}
	got = getState(t, store, h.owner, h.repo, h.number, h.sha)
	if failing := standingOn(got, added.CheckName, "902", "failure"); failing != names || len(got.Checks) != names+1 {
		t.Fatalf("once suite 900 is a workflow_dispatch run, %d of %d names stand on suite 902's failure and the record holds %d checks, want all %d and %d checks",
			failing, names, len(got.Checks), names, names+1)
	}
}

// A run waiting to stand for a name never gives way at the record's bound while it may still
// stand, so no name is left with no check, and a red head does not read green. In the record below
// no suite's event is known: each name's older waiting run is suite 901's failure, and its newer one
// suite 902's success. Should 900 and then 902 turn out uncounted, 901's failure is what GitHub's
// rollup holds for the name. So nothing gives way when a check of another App takes the record past
// its bound: the record is written as it was, marked overflowed, and the write answers the bound's
// refusal. Once suite 900 is a workflow_dispatch run and 902 a pull_request_review_comment run,
// every name stands on 901's failure.
func TestAWaitingRunThatMayStillStandNeverGivesWayAtTheRecordsBound(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	h := head{"example-org", "example-repo", "62", "3a171e00000000000000000000000000000000fe"}
	st, names := waitingAtBound(t, h, "failure", "success", nil)
	seedRecord(t, store, h, st)

	added := pastTheBound(h)
	if size := recordSize(t, plusCheck(st, added)); size <= maxRecordBytes {
		t.Fatalf("the record with the added check is %d bytes, want past the %d-byte bound", size, maxRecordBytes)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.Record(ctx, added); !errors.Is(err, bus.ErrTooLarge) {
		t.Errorf("recording a check past the bound with no waiting run that can give way answered %v, want the bound's refusal (bus.ErrTooLarge)", err)
	}
	got := getState(t, store, h.owner, h.repo, h.number, h.sha)
	if !got.Overflowed || len(got.Waiting) != 2*names || len(got.Checks) != names {
		t.Errorf("after the refused write the record is overflowed %v with %d waiting runs and %d checks, want overflowed with its %d waiting runs and %d checks",
			got.Overflowed, len(got.Waiting), len(got.Checks), 2*names, names)
	}

	for _, event := range []struct{ suite, event string }{{"900", "workflow_dispatch"}, {"902", "pull_request_review_comment"}} {
		if err := store.RecordRunEvent(ctx, contracts.CIObservation{Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha,
			SuiteID: event.suite, RunEvent: event.event}); err != nil {
			t.Fatalf("record suite %s's event: %v", event.suite, err)
		}
	}
	got = getState(t, store, h.owner, h.repo, h.number, h.sha)
	if failing := standingOn(got, added.CheckName, "901", "failure"); failing != names || len(got.Checks) != names {
		t.Fatalf("once suites 900 and 902 are uncounted, %d of %d names stand on suite 901's failure and the record holds %d checks, want all %d",
			failing, names, len(got.Checks), names)
	}
}

// The runs a re-run replaced while its suite's event was not known (State.Superseded) give way at
// the record's bound, all but the newest of each name, so a head whose events never arrive does not
// overflow on them. In the record below each name stands on a run of suite 900, whose event is not
// known, and keeps two earlier runs of that suite, as large as the bound allows. A check of another
// App takes it past the bound with both and not with the newer alone. The write lands with the new
// check, the record is not overflowed, and each name keeps its newer superseded run.
func TestSupersededRunsGiveWayAtTheRecordsBoundButTheNewestOfEachName(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	h := head{"example-org", "example-repo", "63", "3a171e00000000000000000000000000000000fd"}
	run := func(i int, id uint64) Check {
		return Check{
			Name: fmt.Sprintf("dispatch-browser / binaries %04d", i), CheckRunID: checkRunID(id),
			URL:    fmt.Sprintf("https://github.com/example-org/example-repo/actions/runs/44860489318/job/%d", id),
			Status: "completed", Conclusion: "success", ObservedAt: "2026-11-21T03:21:34Z",
			SuiteID: "900", AppID: contracts.GithubActionsAppID,
		}
	}
	st := State{Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha, Checks: map[string]Check{}, Superseded: map[string]Check{}}
	for i := 0; ; i++ {
		standing, older, newer := run(i, 136713000000+uint64(i)), run(i, 136711000000+uint64(i)), run(i, 136712000000+uint64(i))
		st.Checks[standing.Name] = standing
		st.Superseded[waitingKey(older.CheckRunID)] = older
		st.Superseded[waitingKey(newer.CheckRunID)] = newer
		if recordSize(t, st) > maxRecordBytes {
			delete(st.Checks, standing.Name)
			delete(st.Superseded, waitingKey(older.CheckRunID))
			delete(st.Superseded, waitingKey(newer.CheckRunID))
			break
		}
	}
	names := len(st.Checks)
	st.Schema = recordSchema
	st.LastEventAt = time.Now().UnixMilli()
	seedRecord(t, store, h, st)

	added := pastTheBound(h)
	with := plusCheck(st, added)
	trimmed := plusCheck(st, added)
	trimmed.Superseded = cloneChecks(st.Superseded)
	if dropped := trimmed.trimSuperseded(); dropped != names {
		t.Fatalf("trimming the superseded runs drops %d, want the older of each of the %d names", dropped, names)
	}
	if recordSize(t, with) <= maxRecordBytes || recordSize(t, trimmed) > maxRecordBytes {
		t.Fatalf("the record with the added check is %d bytes with its %d superseded runs and %d with the newer of each name, want past the %d-byte bound only with all of them",
			recordSize(t, with), len(with.Superseded), recordSize(t, trimmed), maxRecordBytes)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.Record(ctx, added); err != nil {
		t.Fatalf("record a check past the bound its older superseded runs alone take the record past: %v", err)
	}
	got := getState(t, store, h.owner, h.repo, h.number, h.sha)
	if got.Overflowed || len(got.Superseded) != names || len(got.Checks) != names+1 {
		t.Fatalf("after the write the record is overflowed %v with %d superseded runs and %d checks, want not overflowed, %d superseded runs and %d checks",
			got.Overflowed, len(got.Superseded), len(got.Checks), names, names+1)
	}
	for _, check := range got.Superseded {
		if check.CheckRunID < 136712000000 {
			t.Fatalf("the record kept the superseded run %+v, want the newer of its name", check)
		}
	}
}

// cloneChecks is a copy of checks.
func cloneChecks(checks map[string]Check) map[string]Check {
	out := make(map[string]Check, len(checks)+1)
	for key, check := range checks {
		out[key] = check
	}
	return out
}

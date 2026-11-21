package cistore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The runs a record keeps waiting to stand for a name (State.Waiting) never cost a head its
// settlements: a write that passes the record's bound only for them is written without them, and
// the record is not marked overflowed. The record below holds checks of a GitHub Actions suite
// whose event it does not know, each with a run of another suite waiting behind it, as large as the
// bound allows; a check of another App then takes it past the bound with the waiting runs and not
// without them. The write lands, the record holds the new check and no waiting run, and it is not
// overflowed, so the head still settles.
func TestWaitingRunsGiveWayAtTheRecordsBound(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	h := head{"example-org", "example-repo", "61", "3a171e00000000000000000000000000000000ff"}

	entry := func(i int, id uint64, suite string) Check {
		return Check{
			Name: fmt.Sprintf("dispatch-browser / binaries %04d", i), CheckRunID: checkRunID(id),
			URL:    fmt.Sprintf("https://github.com/example-org/example-repo/actions/runs/44860489318/job/%d", id),
			Status: "completed", Conclusion: "success", ObservedAt: "2026-11-21T03:21:34Z",
			SuiteID: suite, AppID: contracts.GithubActionsAppID,
		}
	}
	st := State{Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha, Checks: map[string]Check{}, Waiting: map[string]Check{}}
	size := func(st State) int {
		t.Helper()
		buf, err := encodeRecord(&st)
		if err != nil {
			t.Fatalf("encode the record: %v", err)
		}
		return len(buf)
	}
	for i := 0; ; i++ {
		standing, waiting := entry(i, 136712000000+uint64(i), "900"), entry(i, 136711000000+uint64(i), "901")
		st.Checks[standing.Name] = standing
		st.Waiting[waitingKey(waiting.CheckRunID)] = waiting
		if size(st) > maxRecordBytes {
			delete(st.Checks, standing.Name)
			delete(st.Waiting, waitingKey(waiting.CheckRunID))
			break
		}
	}
	st.Schema = recordSchema
	st.LastEventAt = time.Now().UnixMilli()
	buf, err := encodeRecord(&st)
	if err != nil {
		t.Fatalf("encode the seeded record: %v", err)
	}
	if _, err := store.watcher.KV().Put(h.key(), buf); err != nil {
		t.Fatalf("seed the record: %v", err)
	}

	added := contracts.CIObservation{
		Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha, AppID: "57789",
		CheckName: "CodeQL", CheckRunID: 136720000001, Status: "completed", Conclusion: "success",
		URL: "https://github.com/example-org/example-repo/runs/" + strings.Repeat("9", 4096), ObservedAt: "2026-11-21T03:30:00Z",
	}
	with := st
	with.Checks = cloneChecks(st.Checks)
	with.Checks[added.CheckName] = Check{Name: added.CheckName, CheckRunID: checkRunID(added.CheckRunID), URL: added.URL,
		Status: added.Status, Conclusion: added.Conclusion, ObservedAt: added.ObservedAt, AppID: added.AppID}
	without := with
	without.Waiting = nil
	if size(with) <= maxRecordBytes || size(without) > maxRecordBytes {
		t.Fatalf("the record with the added check is %d bytes with its %d waiting runs and %d without, want past the %d-byte bound only with them",
			size(with), len(with.Waiting), size(without), maxRecordBytes)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.Record(ctx, added); err != nil {
		t.Fatalf("record a check past the bound its waiting runs alone take the record past: %v", err)
	}
	got := getState(t, store, h.owner, h.repo, h.number, h.sha)
	if got.Overflowed || len(got.Waiting) != 0 || got.Checks[added.CheckName].CheckRunID != checkRunID(added.CheckRunID) || len(got.Checks) != len(st.Checks)+1 {
		t.Fatalf("after the write the record is overflowed %v with %d waiting runs and %d checks (%v held), want not overflowed, no waiting run and the %d checks with %s",
			got.Overflowed, len(got.Waiting), len(got.Checks), got.Checks[added.CheckName], len(st.Checks)+1, added.CheckName)
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

package cistore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// recordRunsHead is the head every test below writes.
const (
	recordRunsOwner  = "example-org"
	recordRunsRepo   = "example-repo"
	recordRunsNumber = "42"
	recordRunsSHA    = "abcdef1234567890abcdef1234567890abcdef12"
)

// recordRunsCheck is one check run of recordRunsHead.
func recordRunsCheck(name string, id uint64, status, conclusion, observedAt string) contracts.CIObservation {
	return contracts.CIObservation{
		Owner:      recordRunsOwner,
		Repo:       recordRunsRepo,
		Number:     recordRunsNumber,
		SHA:        recordRunsSHA,
		CheckName:  name,
		CheckRunID: id,
		URL:        fmt.Sprintf("https://github.com/example-org/example-repo/runs/%d", id),
		Status:     status,
		Conclusion: conclusion,
		ObservedAt: observedAt,
	}
}

// recordRunsStored is recordRunsHead's record as the bucket holds it, with its revision.
func recordRunsStored(t *testing.T, s *Store) (State, uint64) {
	t.Helper()
	entry, err := s.watcher.KV().Get(Key(recordRunsOwner, recordRunsRepo, recordRunsNumber, recordRunsSHA))
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	var st State
	if err := json.Unmarshal(entry.Value(), &st); err != nil {
		t.Fatalf("decode the record: %v", err)
	}
	return st, entry.Revision()
}

// A head's runs are one write, and every rule a run meets written alone holds inside it: a queued
// run never replaces the same run finished (observationMayReplace), an older run never replaces a
// newer one of its name (checkRunIDIsOlder), and a run the record lacks is recorded. The write
// moves the record's revision once, and a batch that changes nothing writes nothing.
func TestRecordRunsKeepsEveryRuleOfARunInOneWrite(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	ctx := context.Background()
	for _, seed := range []contracts.CIObservation{
		recordRunsCheck("build", 7, "completed", "success", "2026-11-22T00:05:00Z"),
		recordRunsCheck("ci-ok", 9, "completed", "success", "2026-11-22T00:05:00Z"),
	} {
		if err := s.Record(ctx, seed); err != nil {
			t.Fatalf("seed %s: %v", seed.CheckName, err)
		}
	}
	_, before := recordRunsStored(t, s)

	stale := []contracts.CIObservation{
		recordRunsCheck("build", 7, "queued", "", "2026-11-22T00:04:00Z"),
		recordRunsCheck("ci-ok", 5, "queued", "", "2026-11-22T00:01:00Z"),
	}
	changed, err := s.recordRuns(ctx, append(stale, recordRunsCheck("lint", 11, "in_progress", "", "2026-11-22T00:06:00Z")))
	if err != nil || !changed {
		t.Fatalf("recordRuns = %v, %v; want the new run to change the record", changed, err)
	}
	st, after := recordRunsStored(t, s)
	if after != before+1 {
		t.Fatalf("the record's revision moved from %d to %d, want one write", before, after)
	}
	if got := st.Checks["build"]; got.CheckRunID != 7 || got.Status != "completed" {
		t.Errorf("build is %+v, want run 7 completed: a queued observation replaced the same run finished", got)
	}
	if got := st.Checks["ci-ok"]; got.CheckRunID != 9 || got.Status != "completed" {
		t.Errorf("ci-ok is %+v, want run 9 completed: an older run replaced a newer one", got)
	}
	if got := st.Checks["lint"]; got.CheckRunID != 11 || got.Status != "in_progress" {
		t.Errorf("lint is %+v, want run 11 in_progress", got)
	}

	changed, err = s.recordRuns(ctx, stale)
	if err != nil || changed {
		t.Fatalf("recordRuns of runs the record's rules refuse = %v, %v; want no change", changed, err)
	}
	if _, again := recordRunsStored(t, s); again != after {
		t.Fatalf("the record's revision moved from %d to %d for a batch that changed nothing", after, again)
	}
}

// A batch that would take the record past its bound is refused whole, never written in part: the
// record is written as it was, marked Overflowed, holding none of the batch's runs, and it never
// settles. Written one at a time, the runs that fitted would have landed before the refusal.
func TestRecordRunsPastTheRecordBoundWritesNoneOfThem(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	ctx := context.Background()
	if err := s.Record(ctx, recordRunsCheck("seed", 1, "completed", "success", "2026-11-22T00:05:00Z")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Job names at the envelope text cap, in a character that costs six bytes in the record, twice
	// over: about 16 of them fill the record's bound, and 200 pass it.
	runs := make([]contracts.CIObservation, 0, 200)
	for i := range 200 {
		name := fmt.Sprintf("%s%04d", strings.Repeat("<", 2044), i)
		runs = append(runs, recordRunsCheck(name, uint64(1000+i), "completed", "success", "2026-11-22T00:06:00Z"))
	}

	changed, err := s.recordRuns(ctx, runs)
	if !errors.Is(err, bus.ErrTooLarge) || changed {
		t.Fatalf("recordRuns past the bound = %v, %v; want bus.ErrTooLarge and no change", changed, err)
	}
	st, _ := recordRunsStored(t, s)
	if !st.Overflowed {
		t.Fatalf("the record is not marked Overflowed: %+v", st)
	}
	if len(st.Checks) != 1 || st.Checks["seed"].CheckRunID != 1 {
		t.Fatalf("the record holds %d checks, want the seed alone: a refused batch wrote part of itself", len(st.Checks))
	}
	waitCacheOverflowed(t, s)
	pub := &recPub{}
	runSummaryTick(s, pub, 0, logging.New("test"))
	if pub.count() != 0 {
		t.Fatalf("published %d settlements for a record a refused batch overflowed, want none", pub.count())
	}
}

// waitCacheOverflowed waits until the store's cache holds recordRunsHead's record marked
// Overflowed, so a summary pass reads the record the bucket holds.
func waitCacheOverflowed(t *testing.T, s *Store) {
	t.Helper()
	key := Key(recordRunsOwner, recordRunsRepo, recordRunsNumber, recordRunsSHA)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if st, ok := s.cachedState(key); ok && st.Overflowed {
			return
		}
	}
	t.Fatal("the cache never held the record marked Overflowed")
}

// A write carries the runs of one head: runs of two heads are refused before anything is read,
// and neither head gets a record.
func TestRecordRunsRefusesTheRunsOfTwoHeads(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	other := recordRunsCheck("lint", 2, "completed", "success", "2026-11-22T00:05:00Z")
	other.SHA = "0123456789abcdef0123456789abcdef01234567"

	changed, err := s.recordRuns(context.Background(), []contracts.CIObservation{
		recordRunsCheck("build", 1, "completed", "success", "2026-11-22T00:05:00Z"),
		other,
	})
	if err == nil || changed {
		t.Fatalf("recordRuns of two heads = %v, %v; want a refusal", changed, err)
	}
	for _, sha := range []string{recordRunsSHA, other.SHA} {
		if _, err := s.watcher.KV().Get(Key(recordRunsOwner, recordRunsRepo, recordRunsNumber, sha)); err == nil {
			t.Fatalf("head %s has a record after a refused write", sha)
		}
	}
}

package cistore_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
)

// openListener opens the listener's own store, which creates the CI bucket and starts the cache
// watcher a writer has not got, and waits for its cache.
func openListener(t *testing.T, conn *natsgo.Conn) *cistore.Store {
	t.Helper()
	store, err := cistore.Open(t.Context(), conn, logging.New("listener"), cistore.WithReplicas(1), cistore.WithTTL(time.Hour))
	if err != nil {
		t.Fatalf("open the listener's cistore: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("the listener's cache is not ready: %v", err)
	}
	return store
}

// A writer's record reaches a settlement, not only a key: it writes a check run and a check suite
// of one head into the bucket with no watcher of its own, and the listener's store sees them
// through its cache watcher and its real summary loop publishes the head's settlement. The record
// lands under cistore.Key, the key the listener's own store writes, so the two processes share one
// record per head rather than each keeping its own.
func TestAWriterRecordSettlesThroughTheListenersSummaryLoop(t *testing.T) {
	ctx := context.Background()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	store := openListener(t, client.Conn)
	writer, err := cistore.OpenWriter(client.Connection(), client.JS(), logging.New("writer"))
	if err != nil {
		t.Fatalf("open the writer: %v", err)
	}

	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	sub, err := client.Conn.SubscribeSync("notifications.github." + owner + "." + repo + ".pr." + number + ".checks")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	cistore.StartSummaryLoop(loopCtx, store, client, 100*time.Millisecond, 20*time.Millisecond,
		metrics.New().NewGauge("held", ""), logging.New("loop"))

	if _, err := writer.Record(ctx, contracts.CIObservation{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		SHA:        sha,
		CheckName:  "build",
		CheckRunID: 7,
		URL:        "https://github.com/example-org/example-repo/runs/7",
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:01:00Z",
	}); err != nil {
		t.Fatalf("the writer's check run: %v", err)
	}
	if _, err := writer.RecordSuite(ctx, contracts.CIObservation{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		SHA:        sha,
		SuiteID:    "9001",
		AppID:      "15368",
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:01:05Z",
	}); err != nil {
		t.Fatalf("the writer's check suite: %v", err)
	}

	msg, err := sub.NextMsg(10 * time.Second)
	if err != nil {
		t.Fatalf("the writer's head did not settle: %v", err)
	}
	var env contracts.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		t.Fatalf("the settlement is not JSON: %v", err)
	}
	var sum cistore.Summary
	if err := json.Unmarshal([]byte(env.Payload), &sum); err != nil {
		t.Fatalf("the settlement's payload is not JSON: %v\n%s", err, env.Payload)
	}
	if sum.Kind != "checks" || sum.SHA != sha || sum.Passed.Count != 1 || sum.Failed.Count != 0 {
		t.Fatalf("settlement = %+v, want one passing check of %s", sum, sha)
	}

	// Read straight from the bucket, which is where the listener's store writes the head's record
	// too: one key per head, whichever process wrote it.
	entry, err := client.JS().KeyValue(bus.CIStateBucket)
	if err != nil {
		t.Fatalf("open the CI bucket: %v", err)
	}
	raw, err := entry.Get(cistore.Key(owner, repo, number, sha))
	if err != nil {
		t.Fatalf("get the head's record under cistore.Key: %v", err)
	}
	var state cistore.State
	if err := json.Unmarshal(raw.Value(), &state); err != nil {
		t.Fatalf("the record is not JSON: %v", err)
	}
	if _, ok := state.Checks["build"]; !ok {
		t.Fatalf("the record holds no build check: %+v", state)
	}
	if _, ok := state.Suites["9001"]; !ok {
		t.Fatalf("the record holds no check suite: %+v", state)
	}
}

// A writer binds an existing bucket and creates none: where the listener has never run, OpenWriter
// fails with what bus.OpenKeyValue gives and leaves the bucket unmade, so the listener's Open stays
// the one owner of its TTL and its copies.
func TestOpenWriterFailsAndCreatesNothingWithoutTheBucket(t *testing.T) {
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()

	writer, err := cistore.OpenWriter(client.Connection(), client.JS(), logging.New("writer"))
	if !errors.Is(err, natsgo.ErrBucketNotFound) {
		t.Fatalf("OpenWriter without the bucket = %v, want %v", err, natsgo.ErrBucketNotFound)
	}
	if writer != nil {
		t.Fatalf("OpenWriter returned a writer for a bucket that is not there: %+v", writer)
	}
	if _, err := client.JS().KeyValue(bus.CIStateBucket); !errors.Is(err, natsgo.ErrBucketNotFound) {
		t.Fatalf("the CI bucket after the refused OpenWriter: %v, want %v", err, natsgo.ErrBucketNotFound)
	}
}

// HasRecord answers from the bucket: false for a head no record is held of, and true for one
// another store wrote, which this writer's own process never wrote and no cache of its own could
// answer for.
func TestHasRecordReadsTheBucketRatherThanACache(t *testing.T) {
	ctx := context.Background()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	store := openListener(t, client.Conn)
	writer, err := cistore.OpenWriter(client.Connection(), client.JS(), logging.New("writer"))
	if err != nil {
		t.Fatalf("open the writer: %v", err)
	}

	const (
		owner    = "example-org"
		repo     = "example-repo"
		number   = "42"
		recorded = "1111111111111111111111111111111111111111"
		unknown  = "2222222222222222222222222222222222222222"
	)
	has, err := writer.HasRecord(owner, repo, number, recorded)
	if err != nil {
		t.Fatalf("HasRecord before the record: %v", err)
	}
	if has {
		t.Fatal("HasRecord is true for a head the bucket holds no record of")
	}

	if err := store.Record(ctx, contracts.CIObservation{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		SHA:        recorded,
		CheckName:  "build",
		CheckRunID: 7,
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:01:00Z",
	}); err != nil {
		t.Fatalf("the second store's check run: %v", err)
	}

	has, err = writer.HasRecord(owner, repo, number, recorded)
	if err != nil {
		t.Fatalf("HasRecord after the record: %v", err)
	}
	if !has {
		t.Fatal("HasRecord is false for a head the bucket holds the record of")
	}
	has, err = writer.HasRecord(owner, repo, number, unknown)
	if err != nil {
		t.Fatalf("HasRecord for another head: %v", err)
	}
	if has {
		t.Fatal("HasRecord is true for a head nothing wrote")
	}
}

// A writer's Record says whether the observation changed the head's record, which is what a
// caller counting what its pass mended reports: the first observation of a head with no record
// changes it, the same observation again changes nothing, and an older observation of the same
// check is refused by the record's own rules and changes nothing either.
func TestAWriterReportsWhetherTheObservationChangedTheRecord(t *testing.T) {
	ctx := context.Background()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	// The listener's store makes the bucket a writer only binds; no cache of this test's own is
	// read, since the writer answers from what it wrote.
	openListener(t, client.Conn)
	writer, err := cistore.OpenWriter(client.Connection(), client.JS(), logging.New("writer"))
	if err != nil {
		t.Fatalf("open the writer: %v", err)
	}

	observation := contracts.CIObservation{
		Owner:      "example-org",
		Repo:       "example-repo",
		Number:     "42",
		SHA:        "cccccccccccccccccccccccccccccccccccccccc",
		CheckName:  "build",
		CheckRunID: 7,
		URL:        "https://github.com/example-org/example-repo/runs/7",
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:01:00Z",
	}
	recorded, err := writer.Record(ctx, observation)
	if err != nil {
		t.Fatalf("the first record: %v", err)
	}
	if !recorded.Changed {
		t.Fatal("the first observation of a head with no record is reported as no change")
	}

	recorded, err = writer.Record(ctx, observation)
	if err != nil {
		t.Fatalf("the same record again: %v", err)
	}
	if recorded.Changed {
		t.Fatal("an observation the record already holds is reported as a change")
	}

	older := observation
	older.ObservedAt = "2026-09-07T02:59:00Z"
	older.Status = "in_progress"
	older.Conclusion = ""
	recorded, err = writer.Record(ctx, older)
	if err != nil {
		t.Fatalf("the older record: %v", err)
	}
	if recorded.Changed {
		t.Fatal("an older observation of the same check is reported as a change")
	}
}

// A writer's Record and RecordSuite also say whether the record, once written, holds the event
// that started the workflow run of the observation's check suite, which is how Dispatch's CI
// reconcile names a suite the record counts with no event. A GitHub Actions run of a suite with no
// event is unknown; an observation carrying its suite's event teaches the record that event, and
// every later observation of the suite, which carries none, is then known; another App's run is
// counted whatever started it and is never unknown, and so is a suite whose event GitHub does not
// evaluate for a pull request, which the record does not count.
func TestAWriterReportsWhetherTheRecordHoldsTheSuitesEvent(t *testing.T) {
	ctx := context.Background()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	openListener(t, client.Conn)
	writer, err := cistore.OpenWriter(client.Connection(), client.JS(), logging.New("writer"))
	if err != nil {
		t.Fatalf("open the writer: %v", err)
	}

	head := contracts.CIObservation{Owner: "example-org", Repo: "example-repo", Number: "42", SHA: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}
	run := func(id uint64, name, suite, app string) contracts.CIObservation {
		observation := head
		observation.CheckName, observation.CheckRunID, observation.SuiteID, observation.AppID = name, id, suite, app
		observation.Status, observation.Conclusion, observation.ObservedAt = "completed", "success", "2026-11-21T04:00:00Z"
		return observation
	}
	suite := func(id, event string) contracts.CIObservation {
		observation := head
		observation.SuiteID, observation.AppID, observation.RunEvent = id, contracts.GithubActionsAppID, event
		observation.Status, observation.Conclusion, observation.ObservedAt = "completed", "success", "2026-11-21T04:00:01Z"
		return observation
	}
	for _, step := range []struct {
		what    string
		write   func() (cistore.Recorded, error)
		unknown bool
	}{
		{"an Actions run of a suite with no event", func() (cistore.Recorded, error) {
			return writer.Record(ctx, run(101, "build", "900", contracts.GithubActionsAppID))
		}, true},
		{"its suite, carrying the event pull_request", func() (cistore.Recorded, error) {
			return writer.RecordSuite(ctx, suite("900", "pull_request"))
		}, false},
		{"another run of that suite, carrying no event", func() (cistore.Recorded, error) {
			return writer.Record(ctx, run(102, "test", "900", contracts.GithubActionsAppID))
		}, false},
		{"another App's run of a suite with no event", func() (cistore.Recorded, error) {
			return writer.Record(ctx, run(103, "deploy", "901", "57789"))
		}, false},
		{"an Actions suite started by workflow_dispatch", func() (cistore.Recorded, error) {
			return writer.RecordSuite(ctx, suite("902", "workflow_dispatch"))
		}, false},
		{"a run of that workflow_dispatch suite", func() (cistore.Recorded, error) {
			return writer.Record(ctx, run(104, "lint", "902", contracts.GithubActionsAppID))
		}, false},
	} {
		recorded, err := step.write()
		if err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		if recorded.EventUnknown != step.unknown {
			t.Errorf("%s: the writer answers %+v, want EventUnknown %v", step.what, recorded, step.unknown)
		}
	}
}

// Concurrent callers of one head are each told what their own observation did, not what the write
// that carried it did. The combiner folds every mutation queued while one caller writes into that
// one compare-and-swap, so a batch carries several callers at once, and one answer for the whole
// batch would tell a caller whose observation the record already held that it mended something --
// which is what the pass counting the records it changed would then count.
//
// Six callers write one head at the same time: two pairs each sending one observation the record
// lacks, and two sending the check run it already holds. Within a pair, whichever of the two the
// combiner applies first changes the record and the other finds its own observation already
// there, so however the six fall into batches and whichever of them writes, exactly the two
// observations new to the record change it and exactly two callers report a change.
func TestConcurrentCallersOfOneHeadEachReportTheirOwnChange(t *testing.T) {
	ctx := context.Background()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	// The listener's store makes the bucket a writer only binds; the record itself is read out of
	// that bucket below, since a writer keeps no cache to read it from.
	openListener(t, client.Conn)
	writer, err := cistore.OpenWriter(client.Connection(), client.JS(), logging.New("writer"))
	if err != nil {
		t.Fatalf("open the writer: %v", err)
	}

	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "dddddddddddddddddddddddddddddddddddddddd"
	)
	held := contracts.CIObservation{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		SHA:        sha,
		CheckName:  "build",
		CheckRunID: 7,
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:01:00Z",
	}
	if recorded, err := writer.Record(ctx, held); err != nil || !recorded.Changed {
		t.Fatalf("seed the record with the check the burst finds it already holding: %+v, %v", recorded, err)
	}
	newRun := held
	newRun.CheckName = "ci-ok"
	newRun.CheckRunID = 8
	newRun.ObservedAt = "2026-09-07T03:02:00Z"
	newSuite := contracts.CIObservation{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		SHA:        sha,
		SuiteID:    "9001",
		AppID:      "15368",
		Status:     "completed",
		Conclusion: "success",
		ObservedAt: "2026-09-07T03:02:05Z",
	}

	callers := []func() (cistore.Recorded, error){
		func() (cistore.Recorded, error) { return writer.Record(ctx, newRun) },
		func() (cistore.Recorded, error) { return writer.Record(ctx, newRun) },
		func() (cistore.Recorded, error) { return writer.Record(ctx, held) },
		func() (cistore.Recorded, error) { return writer.Record(ctx, held) },
		func() (cistore.Recorded, error) { return writer.RecordSuite(ctx, newSuite) },
		func() (cistore.Recorded, error) { return writer.RecordSuite(ctx, newSuite) },
	}
	// The two observations the seeded record lacks, which is how many of the six callers can
	// have changed it.
	const wantChanged = 2

	recorded := make([]cistore.Recorded, len(callers))
	failures := make([]error, len(callers))
	// Every caller waits on one channel rather than on a sleep, so they reach the record
	// together whatever the machine is doing, and the test waits for all six rather than for a
	// moment by the clock.
	start := make(chan struct{})
	var running sync.WaitGroup
	running.Add(len(callers))
	for i, call := range callers {
		go func() {
			defer running.Done()
			<-start
			recorded[i], failures[i] = call()
		}()
	}
	close(start)
	running.Wait()

	changed := 0
	answers := make([]bool, len(callers))
	for i, answer := range recorded {
		if failures[i] != nil {
			t.Fatalf("caller %d of the burst: %v", i, failures[i])
		}
		answers[i] = answer.Changed
		if answer.Changed {
			changed++
		}
		// Each of the six observations is of a suite whose event the record does not hold, or of
		// no suite at all, and of GitHub Actions or of no App: each caller is told so, from the
		// write that carried its observation, whichever caller wrote that batch.
		if !answer.EventUnknown {
			t.Errorf("caller %d of the burst is told the record holds its suite's event: %+v", i, answer)
		}
	}
	if changed != wantChanged {
		t.Fatalf("%d of the %d concurrent callers report a change, want %d: only the new run and the new suite were not in the record (%v)",
			changed, len(callers), wantChanged, answers)
	}
	// Both assertions hold whatever order the callers arrive in: the two calls of a pair carry
	// identical observations, so whichever applies first changes the record and the other finds
	// its own observation already there.
	if answers[2] || answers[3] {
		t.Fatalf("a caller whose run the record already held reports a change: %v", answers)
	}
	if answers[0] == answers[1] || answers[4] == answers[5] {
		t.Fatalf("each pair sending one new observation must hold exactly one change: %v", answers)
	}

	entry, err := client.JS().KeyValue(bus.CIStateBucket)
	if err != nil {
		t.Fatalf("open the CI bucket: %v", err)
	}
	raw, err := entry.Get(cistore.Key(owner, repo, number, sha))
	if err != nil {
		t.Fatalf("get the head's record: %v", err)
	}
	var state cistore.State
	if err := json.Unmarshal(raw.Value(), &state); err != nil {
		t.Fatalf("the record is not JSON: %v", err)
	}
	if len(state.Checks) != 2 || len(state.Suites) != 1 {
		t.Fatalf("the record holds %+v, want the two checks and the one suite the burst wrote", state)
	}
	for _, name := range []string{held.CheckName, newRun.CheckName} {
		if _, ok := state.Checks[name]; !ok {
			t.Fatalf("the record holds no %s check: %+v", name, state.Checks)
		}
	}
	if _, ok := state.Suites[newSuite.SuiteID]; !ok {
		t.Fatalf("the record holds no %s suite: %+v", newSuite.SuiteID, state.Suites)
	}
}

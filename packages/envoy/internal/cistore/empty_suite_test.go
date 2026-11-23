package cistore

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// emptySuiteApps are the Apps of the queued suites with no run one production head carried at
// once: suites GitHub creates for an App on every push, to which that App adds no run.
var emptySuiteApps = []string{"5789269", "5341243", "5447700"}

// actionsApp is GitHub Actions' public App id, written out rather than read from
// githubActionsAppID, so a change of the constant fails the test instead of moving with it.
const actionsApp = "15368"

// A record written before the webhook route stopped taking suite requests, whose runs have all
// finished, settles once beside another App's queued suite, or three Apps' suites. A queued suite
// of GitHub Actions (App 15368) holds it, and so does a queued suite the record names no App for.
// A recorded run that is not finished holds it, whichever App's suite stands beside it.
func TestAQueuedSuiteHoldsAHeadOnlyIfItIsActionsOrOfNoApp(t *testing.T) {
	if githubActionsAppID != actionsApp {
		t.Fatalf("githubActionsAppID is %q, want GitHub Actions' App id %q", githubActionsAppID, actionsApp)
	}
	queued := func(id, app string) Suite {
		return Suite{ID: id, AppID: app, Status: "queued", ObservedAt: "2026-11-22T01:00:00Z"}
	}
	for i, tc := range []struct {
		name string
		edit func(st *State)
		want int
	}{
		{"another App's queued suite beside finished runs", func(st *State) {
			st.Suites["200"] = queued("200", "5789269")
		}, 1},
		{"three Apps' queued suites beside finished runs", func(st *State) {
			for i, app := range emptySuiteApps {
				id := string(rune('a' + i))
				st.Suites[id] = queued(id, app)
			}
		}, 1},
		{"GitHub Actions' queued suite", func(st *State) {
			st.Suites["200"] = queued("200", actionsApp)
		}, 0},
		{"three Apps' queued suites beside GitHub Actions' queued suite", func(st *State) {
			for i, app := range emptySuiteApps {
				id := string(rune('a' + i))
				st.Suites[id] = queued(id, app)
			}
			st.Suites["200"] = queued("200", actionsApp)
		}, 0},
		{"a queued suite the record names no App for", func(st *State) {
			st.Suites["200"] = queued("200", "")
		}, 0},
		{"another App's queued suite beside a run in progress", func(st *State) {
			st.Suites["200"] = queued("200", "5789269")
			st.Checks["review"] = Check{Name: "review", CheckRunID: 803, URL: "https://example-host/runs/803", Status: "in_progress", ObservedAt: "2026-11-22T01:00:00Z"}
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHeldRig(t)
			h := head{"example-org", "example-repo", "70", "e3b70000000000000000000000000000000000" + string(rune('a'+i)) + "0"}
			putRecord(t, r.store, h, func(st *State) {
				st.LastEventAt = time.Now().Add(-time.Hour).UnixMilli()
				tc.edit(st)
			})
			now := time.Now().UnixMilli()
			r.pass(now)
			r.pass(now + time.Second.Milliseconds())
			if got := r.pub.count(); got != tc.want {
				t.Fatalf("the record published %d settlements over two passes, want %d", got, tc.want)
			}
		})
	}
}

// A record a listener before the change left held by another App's queued suite publishes one
// settlement at the first summary pass that finds it in the store's copy, with no Record call: a
// fresh store reads the bucket, and the first pass of the summarizer attached to it, as a listener
// start attaches one (StartSummaryLoop), settles the record. A pass a minute later publishes
// nothing more.
func TestARecordHeldByAnEmptySuiteSettlesAtTheFirstPassThatFindsIt(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	h := head{"example-org", "example-repo", "63", "e3b7000000000000000000000000000000000063"}
	putRecord(t, openStore(t, conn), h, heldBeforeTheChange)

	r := startedRig(t, conn)
	first := time.Now().UnixMilli()
	r.pass(first)
	wantTopics(t, r.pub, "the first pass", checksTopic(h))
	r.pass(first + time.Minute.Milliseconds())
	wantTopics(t, r.pub, "the first pass and a pass a minute later", checksTopic(h))
}

// The passes after a start publish nothing for a record already settled for its state
// (settled_emitted), while the first settles a held record beside it, which shows it looked at
// both. A run recorded later re-arms the settled head, and the next pass settles it again, once,
// as a settlement that supersedes the earlier one (superseded_settlement); a pass a minute after
// that publishes nothing.
func TestASettledHeadIsNotPublishedAgainUntilALaterRun(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	before := openStore(t, conn)
	held := head{"example-org", "example-repo", "64", "e3b7000000000000000000000000000000000064"}
	settled := head{"example-org", "example-repo", "65", "e3b7000000000000000000000000000000000065"}
	putRecord(t, before, held, heldBeforeTheChange)
	putRecord(t, before, settled, func(st *State) {
		heldBeforeTheChange(st)
		st.SettledEmitted, st.EmittedCount = true, 1
	})

	r := startedRig(t, conn)
	first := time.Now().UnixMilli()
	r.pass(first)
	wantTopics(t, r.pub, "the first pass", checksTopic(held))
	second := first + time.Minute.Milliseconds()
	r.pass(second)
	wantTopics(t, r.pub, "the first pass and a pass a minute later", checksTopic(held))

	r.record(settled.check("deploy", 804, "completed", "success", "2026-11-22T04:00:00Z"))
	third := max(second, cachedAsStored(t, r.store, settled).LastEventAt) + time.Minute.Milliseconds()
	r.pass(third)
	wantTopics(t, r.pub, "the passes up to the first after the later run", checksTopic(held), checksTopic(settled))
	var sum Summary
	if err := json.Unmarshal([]byte(r.pub.last().Payload), &sum); err != nil {
		t.Fatalf("decode the settlement: %v", err)
	}
	if sum.SupersededSettlement != "true" {
		t.Fatalf("the later run's settlement has superseded_settlement %q, want \"true\"", sum.SupersededSettlement)
	}
	r.pass(third + time.Minute.Milliseconds())
	wantTopics(t, r.pub, "the passes up to a minute after the later run's settlement", checksTopic(held), checksTopic(settled))
}

// A head that settled and then was re-armed by a rerequested delivery before the change, which
// recorded the App's suite queued again beside the run GitHub queued anew, is held while that run
// is not finished and settles again once it is, superseding its earlier settlement.
func TestAHeadReArmedByASuiteRequestSettlesOnceItsRunsFinish(t *testing.T) {
	r := newHeldRig(t)
	h := head{"example-org", "example-repo", "67", "e3b7000000000000000000000000000000000067"}
	putRecord(t, r.store, h, func(st *State) {
		heldBeforeTheChange(st)
		st.LastEventAt = time.Now().Add(-time.Hour).UnixMilli()
		st.Generation, st.EmittedCount = 2, 1
		st.Checks["review"] = Check{Name: "review", CheckRunID: 805, URL: "https://example-host/runs/805", Status: "queued", ObservedAt: "2026-11-22T01:00:00Z"}
	})
	r.pass(time.Now().UnixMilli())
	if got := r.pub.count(); got != 0 {
		t.Fatalf("the re-armed head published %d settlements while its run is queued, want none", got)
	}

	r.record(h.check("review", 805, "completed", "success", "2026-11-22T04:00:00Z"))
	st := cachedAsStored(t, r.store, h)
	r.pass(st.LastEventAt + heldDebounce.Milliseconds())
	if got := r.pub.count(); got != 1 {
		t.Fatalf("the re-armed head published %d settlements once its run finished, want 1", got)
	}
	var sum Summary
	if err := json.Unmarshal([]byte(r.pub.last().Payload), &sum); err != nil {
		t.Fatalf("decode the settlement: %v", err)
	}
	if sum.SupersededSettlement != "true" {
		t.Fatalf("the settlement has superseded_settlement %q, want \"true\"", sum.SupersededSettlement)
	}
}

// checksTopic is the topic h's settlement is published on.
func checksTopic(h head) string {
	return contracts.GithubSubject(h.owner, h.repo, "pr."+h.number+".checks")
}

// heldBeforeTheChange edits a putRecord record into one a listener before the change left: its
// runs finished in GitHub Actions' completed suite, and another App's suite queued with no run,
// taken from a requested delivery, holds it, its last event two hours back.
func heldBeforeTheChange(st *State) {
	st.LastEventAt = time.Now().Add(-2 * time.Hour).UnixMilli()
	st.Suites["6200"] = Suite{ID: "6200", AppID: "5789269", Status: "queued", ObservedAt: "2026-11-22T01:00:00Z"}
}

// putRecord writes h's record to the bucket as a listener wrote it: two finished runs and GitHub
// Actions' completed suite, unsettled, stamped, edited by edit, and waits for s's cache to hold it.
// A suite that is not completed is written with the run_count field a listener before the change
// kept for it.
func putRecord(t *testing.T, s *Store, h head, edit func(st *State)) {
	t.Helper()
	st := State{
		Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha,
		Checks: map[string]Check{
			"build": {Name: "build", CheckRunID: 801, URL: "https://example-host/runs/801", Status: "completed", Conclusion: "success", ObservedAt: "2026-11-22T01:00:00Z"},
			"test":  {Name: "test", CheckRunID: 802, URL: "https://example-host/runs/802", Status: "completed", Conclusion: "success", ObservedAt: "2026-11-22T01:00:00Z"},
		},
		Suites: map[string]Suite{
			"100": {ID: "100", AppID: githubActionsAppID, Status: "completed", Conclusion: "success", ObservedAt: "2026-11-22T01:00:00Z"},
		},
		Generation: 1,
		Schema:     recordSchema,
	}
	edit(&st)
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode the record: %v", err)
	}
	for id, suite := range st.Suites {
		if suite.Status != "completed" {
			wire["suites"].(map[string]any)[id].(map[string]any)["run_count"] = 0
		}
	}
	if raw, err = json.Marshal(wire); err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	if _, err := s.watcher.KV().Put(h.key(), raw); err != nil {
		t.Fatalf("store the record: %v", err)
	}
	waitCached(t, s, h.key(), func(cached State) bool {
		return cached.LastEventAt == st.LastEventAt && cached.SettledEmitted == st.SettledEmitted && len(cached.Checks) == len(st.Checks)
	})
}

// startedRig opens a fresh store on conn's bucket and attaches a summarizer to it, as a listener
// start does (StartSummaryLoop): the store reads the bucket whole, then the attach counts every
// record the store read as changed (attachSummary). The test runs each pass itself (heldRig.pass)
// at the time it gives, so it counts what each pass published once that pass has returned.
func startedRig(t *testing.T, conn *natsgo.Conn) *heldRig {
	t.Helper()
	store := openStore(t, conn)
	logs := &bytes.Buffer{}
	return &heldRig{
		t:      t,
		store:  store,
		z:      testSummarizer(store, heldDebounce),
		pub:    &recPub{},
		logs:   logs,
		logger: logging.NewWithWriter("test", logs),
	}
}

// wantTopics fails unless pub has published one settlement on each of topics, in that order, and
// no other; what names the passes run so far.
func wantTopics(t *testing.T, pub *recPub, what string, topics ...string) {
	t.Helper()
	got := make([]string, 0, len(topics))
	for _, published := range pub.all() {
		got = append(got, published.Topic)
	}
	if !slices.Equal(got, topics) {
		t.Fatalf("%s: settlements published on %q, want %q", what, got, topics)
	}
}

package cistore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// statusHead is the head the commit status tests record.
var statusHead = head{"acme", "widgets", "7", "0123456789abcdef0123456789abcdef01234567"}

// status is a commit status of h as the reconcile's mapping gives it (contracts.GithubStatusObservation).
func (h head) status(name, state, createdAt string) contracts.StatusObservation {
	return contracts.StatusObservation{
		Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha,
		Context: name, State: state, CreatedAt: createdAt,
		URL: "https://ci.example.com/" + name, Description: name + " " + state,
	}
}

// recordStatus records one status as the reconcile's writer does (Writer.RecordStatus), failing
// after 10 s rather than hanging.
func recordStatus(s *Store, observation contracts.StatusObservation) (bool, error) {
	type result struct {
		changed bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		changed, err := s.recordStatus(context.Background(), observation)
		done <- result{changed, err}
	}()
	select {
	case r := <-done:
		return r.changed, r.err
	case <-time.After(10 * time.Second):
		return false, errors.New("did not return within 10 s")
	}
}

// lastSettlement decodes the payload of pub's last settlement, and returns it with its one line.
func lastSettlement(t *testing.T, pub *recPub) (Summary, string) {
	t.Helper()
	env := pub.last()
	var sum Summary
	if err := json.Unmarshal([]byte(env.Payload), &sum); err != nil {
		t.Fatalf("decode the settlement: %v\n%s", err, env.Payload)
	}
	return sum, env.PayloadSummary
}

// In the CI record a commit status is an entry of its own kind. A status and a check run of one
// name are two entries, and a status never stands in for a check run, nor for the tester or
// architect check run that carries a verdict: the settlement's check runs, status groups and
// failing checks name check runs alone, and its statuses name every status apart. A failed status
// named ci leaves the passed check run ci passed, a passed status named tester leaves the failed
// check run tester failed, and a status named architect is no check run at all. A head whose
// record holds statuses alone holds no check run, so it never settles.
func TestAStatusIsAnEntryApartFromACheckRunOfItsName(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	h := statusHead
	for _, status := range []contracts.StatusObservation{
		h.status("ci", "failure", "2026-11-17T05:00:00Z"),
		h.status("tester", "success", "2026-11-17T05:00:00Z"),
		h.status("architect", "failure", "2026-11-17T05:00:00Z"),
	} {
		if changed, err := recordStatus(s, status); err != nil || !changed {
			t.Fatalf("record the status %s: changed %v, %v", status.Context, changed, err)
		}
	}
	waitCached(t, s, h.key(), func(st State) bool { return len(st.Statuses) == 3 })
	runSummaryTick(s, pub, 0, logging.New("test"))
	if pub.count() != 0 {
		t.Fatalf("a head holding statuses alone published %d settlements, want none: a status is no check run", pub.count())
	}

	for _, run := range []struct{ name, id, conclusion string }{{"ci", "21", "success"}, {"tester", "23", "failure"}} {
		if err := recordCheck(s, h.owner, h.repo, h.number, h.sha, run.name, run.id, "https://github.com/acme/widgets/runs/"+run.id,
			"completed", run.conclusion, "2026-11-17T05:01:00Z"); err != nil {
			t.Fatalf("record the check run %s: %v", run.name, err)
		}
	}
	st := getState(t, s, h.owner, h.repo, h.number, h.sha)
	if len(st.Checks) != 2 || st.Checks["ci"].Conclusion != "success" || st.Checks["tester"].Conclusion != "failure" {
		t.Fatalf("the record's check runs = %+v, want ci passed and tester failed alone", st.Checks)
	}
	wantStatuses := map[string]string{"architect": "failure", "ci": "failure", "tester": "success"}
	if len(st.Statuses) != len(wantStatuses) {
		t.Fatalf("the record's statuses = %+v, want %v beside the check runs", st.Statuses, wantStatuses)
	}
	for name, state := range wantStatuses {
		if got := st.Statuses[name]; got.Context != name || got.State != state {
			t.Fatalf("the record's status %s = %+v, want %s", name, got, state)
		}
	}

	waitCacheChecks(t, s, h.owner, h.repo, h.number, h.sha, 2)
	runSummaryTick(s, pub, 0, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("published %d settlements once the check runs completed, want one", pub.count())
	}
	sum, _ := lastSettlement(t, pub)
	assertCheckRuns(t, sum.CheckRuns, map[string]uint64{"ci": 21, "tester": 23})
	assertGroup(t, "passed", sum.Passed, []string{"ci"})
	assertGroup(t, "failed", sum.Failed, []string{"tester"})
	if len(sum.FailingChecks) != 1 || sum.FailingChecks[0].Name != "tester" {
		t.Fatalf("failing_checks = %+v, want the check run tester alone", sum.FailingChecks)
	}
	want := []StatusRef{{"architect", "failure", "2026-11-17T05:00:00Z"}, {"ci", "failure", "2026-11-17T05:00:00Z"}, {"tester", "success", "2026-11-17T05:00:00Z"}}
	if !slices.Equal(sum.Statuses, want) {
		t.Fatalf("statuses = %+v, want %+v", sum.Statuses, want)
	}
}

// The record keeps the newest success, failure or error status of each context, by the time GitHub
// gives it: a newer status replaces the kept one whatever its state, and an older one, or the kept
// one read again, changes nothing. The reconcile records no pending status: GitHub's mapping gives
// no observation of one, so a newer pending status leaves the kept one in place. The store itself
// refuses a status in any other state, or with no context, before it reads or writes a record.
func TestTheNewestTerminalStatusOfAContextIsKept(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	h := statusHead
	const (
		t0 = "2026-11-17T05:00:00Z"
		t1 = "2026-11-17T05:01:00Z"
		t2 = "2026-11-17T05:02:00Z"
		t3 = "2026-11-17T05:03:00Z"
		t4 = "2026-11-17T05:04:00Z"
	)
	kept := func(t *testing.T, what, state, createdAt string) {
		t.Helper()
		st := getState(t, s, h.owner, h.repo, h.number, h.sha)
		got := st.Statuses["ci/build"]
		if len(st.Statuses) != 1 || got.State != state || got.CreatedAt != createdAt || got.URL != "https://ci.example.com/ci/build" {
			t.Fatalf("after %s the record keeps %+v, want ci/build in %s created at %s", what, st.Statuses, state, createdAt)
		}
	}
	for _, step := range []struct {
		what             string
		status           contracts.StatusObservation
		changed          bool
		state, createdAt string
	}{
		{"the first status", h.status("ci/build", "success", t1), true, "success", t1},
		{"a newer status in another state", h.status("ci/build", "failure", t2), true, "failure", t2},
		{"an older status", h.status("ci/build", "success", t0), false, "failure", t2},
		{"the kept status read again", h.status("ci/build", "failure", t2), false, "failure", t2},
		{"a newer status that succeeded", h.status("ci/build", "success", t3), true, "success", t3},
		{"an older error", h.status("ci/build", "error", t1), false, "success", t3},
	} {
		changed, err := recordStatus(s, step.status)
		if err != nil || changed != step.changed {
			t.Fatalf("%s: changed %v, %v; want changed %v", step.what, changed, err, step.changed)
		}
		kept(t, step.what, step.state, step.createdAt)
	}

	pending := map[string]any{"context": "ci/build", "state": "pending", "created_at": t4, "target_url": "https://ci.example.com/ci/build"}
	if observation, ok := contracts.GithubStatusObservation(pending, h.owner, h.repo, h.number, h.sha); ok {
		t.Fatalf("a newer pending status maps to %+v, want no observation for the reconcile to record", observation)
	}
	before := getRecord(t, s, h.key()).revision
	for _, state := range []string{"pending", "expected", "EXPECTED", "SUCCESS", "cancelled", ""} {
		if changed, err := recordStatus(s, h.status("ci/build", state, t4)); err == nil || changed {
			t.Fatalf("the store took a status in state %q: changed %v, %v; want it refused", state, changed, err)
		}
	}
	if changed, err := recordStatus(s, h.status("", "success", t4)); err == nil || changed {
		t.Fatalf("the store took a status with no context: changed %v, %v; want it refused", changed, err)
	}
	if after := getRecord(t, s, h.key()).revision; after != before {
		t.Fatalf("a refused status wrote the record (revision %d to %d)", before, after)
	}
	kept(t, "the refused statuses", "success", t3)
	other := head{h.owner, h.repo, "8", h.sha}
	if _, err := recordStatus(s, other.status("ci/build", "pending", t1)); err == nil {
		t.Fatal("the store took a pending status of a head it holds no record of")
	}
	if held, err := s.HasRecord(other.owner, other.repo, other.number, other.sha); err != nil || held {
		t.Fatalf("a refused status of a head with no record left a record %v (%v), want none", held, err)
	}
}

// A commit status settles a head again as a late check run does: on a settled head a new context,
// or a newer status of a context in another state, re-arms it and restarts its quiet period, and
// the next settlement names the status with its state and its created_at under a new generation
// and a new snapshot. A newer status in the same state, with a new time, link and description, is
// recorded and moves neither the generation, the snapshot nor the quiet period, so no settlement
// follows it.
func TestAChangedStatusSettlesAHeadAgain(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	h := statusHead
	logger := logging.New("test")
	if err := recordCheck(s, h.owner, h.repo, h.number, h.sha, "build", "31", "https://github.com/acme/widgets/runs/31", "completed", "success", "2026-11-17T05:00:00Z"); err != nil {
		t.Fatalf("record the check run: %v", err)
	}
	waitCacheChecks(t, s, h.owner, h.repo, h.number, h.sha, 1)
	runSummaryTick(s, pub, 0, logger)
	if pub.count() != 1 {
		t.Fatalf("the head's check run settled %d times, want once", pub.count())
	}
	first, line := lastSettlement(t, pub)
	if first.Statuses != nil || strings.Contains(line, "statuses") {
		t.Fatalf("a settlement of no status names statuses %+v, line %q", first.Statuses, line)
	}

	settleAgain := func(t *testing.T, what string, status contracts.StatusObservation, before Summary) (Summary, string) {
		t.Helper()
		// The head's last event is set far back, so a write that restarts its quiet period shows.
		const lastEvent = 1
		setLastEventAt(t, s, h.owner, h.repo, h.number, h.sha, lastEvent)
		if changed, err := recordStatus(s, status); err != nil || !changed {
			t.Fatalf("%s: changed %v, %v; want it recorded", what, changed, err)
		}
		st := getState(t, s, h.owner, h.repo, h.number, h.sha)
		if st.SettledEmitted || st.Generation <= before.Generation || st.LastEventAt <= lastEvent {
			t.Fatalf("%s left the head settled %v at generation %d, last event %d; want it re-armed past generation %d and last event %d",
				what, st.SettledEmitted, st.Generation, st.LastEventAt, before.Generation, lastEvent)
		}
		waitCached(t, s, h.key(), func(st State) bool { return st.Statuses[status.Context].State == status.State && !st.SettledEmitted })
		published := pub.count()
		runSummaryTick(s, pub, 0, logger)
		if pub.count() != published+1 {
			t.Fatalf("%s: the tick after it published %d settlements, want one", what, pub.count()-published)
		}
		sum, line := lastSettlement(t, pub)
		if sum.Generation <= before.Generation || sum.Snapshot == before.Snapshot {
			t.Fatalf("%s: settled at generation %d, snapshot %s; want both past the settlement before it (generation %d, snapshot %s)",
				what, sum.Generation, sum.Snapshot, before.Generation, before.Snapshot)
		}
		if !slices.Equal(sum.CheckRuns, before.CheckRuns) || sum.SupersededSettlement != "true" {
			t.Fatalf("%s: settled check runs %+v, superseded %q; want the same check runs re-settled", what, sum.CheckRuns, sum.SupersededSettlement)
		}
		return sum, line
	}
	second, line := settleAgain(t, "a new context", h.status("ci/preview", "success", "2026-11-17T05:01:00Z"), first)
	if want := []StatusRef{{"ci/preview", "success", "2026-11-17T05:01:00Z"}}; !slices.Equal(second.Statuses, want) {
		t.Fatalf("a new context's settlement names statuses %+v, want %+v", second.Statuses, want)
	}
	if !strings.HasSuffix(line, ": 1 passed, 0 failed, 0 cancelled, 0 skipped; statuses: 1 passed, 0 failed (re-settled)") {
		t.Fatalf("a new context's settlement reads %q", line)
	}
	third, line := settleAgain(t, "a newer status in another state", h.status("ci/preview", "failure", "2026-11-17T05:02:00Z"), second)
	if want := []StatusRef{{"ci/preview", "failure", "2026-11-17T05:02:00Z"}}; !slices.Equal(third.Statuses, want) {
		t.Fatalf("a changed state's settlement names statuses %+v, want %+v", third.Statuses, want)
	}
	if !strings.HasSuffix(line, ": 1 passed, 0 failed, 0 cancelled, 0 skipped; statuses: 0 passed, 1 failed; failing statuses: ci/preview (re-settled)") {
		t.Fatalf("a changed state's settlement reads %q", line)
	}

	settled := getState(t, s, h.owner, h.repo, h.number, h.sha)
	newer := h.status("ci/preview", "failure", "2026-11-17T05:03:00Z")
	newer.URL = "https://ci.example.com/ci/preview/2"
	newer.Description = "The preview build failed again"
	if changed, err := recordStatus(s, newer); err != nil || !changed {
		t.Fatalf("a newer status in the same state: changed %v, %v; want it recorded", changed, err)
	}
	st := getState(t, s, h.owner, h.repo, h.number, h.sha)
	got := st.Statuses["ci/preview"]
	if got.State != "failure" || got.CreatedAt != newer.CreatedAt || got.URL != newer.URL || got.Description != newer.Description {
		t.Fatalf("the record keeps %+v after a newer status in the same state, want its time, link and description", got)
	}
	if !st.SettledEmitted || st.Generation != third.Generation || st.Hash() != third.Snapshot || st.LastEventAt != settled.LastEventAt {
		t.Fatalf("a newer status in the same state left the head settled %v at generation %d, snapshot %s, last event %d; want it settled at %d, %s, %d",
			st.SettledEmitted, st.Generation, st.Hash(), st.LastEventAt, third.Generation, third.Snapshot, settled.LastEventAt)
	}
	waitCached(t, s, h.key(), func(st State) bool { return st.Statuses["ci/preview"].URL == newer.URL })
	published := pub.count()
	runSummaryTick(s, pub, 0, logger)
	if pub.count() != published {
		t.Fatalf("a newer status in the same state was followed by %d settlements, want none", pub.count()-published)
	}
}

// A record the bucket's TTL removes and the reconcile writes again in full starts its generation
// afresh, so a change of a status's state after it settles under a generation no higher than one
// the head's earlier settlements carried, and a consumer that ordered statuses by the generation
// would drop it. Each entry of the settlement's statuses carries the status's created_at, which
// orders it whatever the generation says: the change after the recreation settles naming the
// newer status with its later created_at.
func TestARecreatedRecordSettlesAStatusChangeWithItsCreatedAt(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	h := statusHead
	logger := logging.New("test")
	const (
		t1 = "2026-11-17T05:01:00Z"
		t2 = "2026-11-17T05:02:00Z"
		t3 = "2026-11-17T05:03:00Z"
		t4 = "2026-11-17T05:04:00Z"
	)
	check := func() error {
		return recordCheck(s, h.owner, h.repo, h.number, h.sha, "build", "41", "https://github.com/acme/widgets/runs/41", "completed", "success", "2026-11-17T05:00:00Z")
	}
	status := func(state, createdAt string) func() error {
		return func() error {
			changed, err := recordStatus(s, h.status("ci", state, createdAt))
			if err == nil && !changed {
				return errors.New("the status changed nothing")
			}
			return err
		}
	}
	// settle makes each write in turn, then runs the summary loop once over the record the last
	// one left, which must settle the head once.
	settle := func(t *testing.T, what string, writes ...func() error) Summary {
		t.Helper()
		for _, write := range writes {
			if err := write(); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
		written := getState(t, s, h.owner, h.repo, h.number, h.sha)
		waitCached(t, s, h.key(), func(st State) bool {
			return !st.SettledEmitted && st.Generation == written.Generation && st.Hash() == written.Hash()
		})
		published := pub.count()
		runSummaryTick(s, pub, 0, logger)
		if pub.count() != published+1 {
			t.Fatalf("%s: the tick after it published %d settlements, want one", what, pub.count()-published)
		}
		sum, _ := lastSettlement(t, pub)
		return sum
	}

	settle(t, "the head's check run and first status", check, status("success", t1))
	settle(t, "a newer status that failed", status("failure", t2))
	before := settle(t, "a newer status that succeeded", status("success", t3))
	if want := []StatusRef{{"ci", "success", t3}}; !slices.Equal(before.Statuses, want) {
		t.Fatalf("before the recreation the head settled naming statuses %+v, want %+v", before.Statuses, want)
	}

	if err := s.watcher.KV().Delete(h.key()); err != nil {
		t.Fatalf("delete the record as its TTL would: %v", err)
	}
	waitCacheStateMissing(t, s, h.owner, h.repo, h.number, h.sha)
	again := settle(t, "the head written again in full", check, status("success", t3))
	if again.Generation >= before.Generation || !slices.Equal(again.Statuses, before.Statuses) {
		t.Fatalf("the recreated record settled at generation %d naming %+v, want a generation below %d, numbered afresh, and %+v",
			again.Generation, again.Statuses, before.Generation, before.Statuses)
	}

	changed := settle(t, "a newer status that failed after the recreation", status("failure", t4))
	if want := []StatusRef{{"ci", "failure", t4}}; !slices.Equal(changed.Statuses, want) {
		t.Fatalf("the status change after the recreation settled naming statuses %+v, want %+v, with its later created_at", changed.Statuses, want)
	}
	if changed.Generation > before.Generation {
		t.Fatalf("the status change after the recreation settled at generation %d, past the %d before it: this test proves nothing without a generation an earlier settlement reached",
			changed.Generation, before.Generation)
	}
}

// todaysHead is a head with check runs in each conclusion group and a check suite, settled once
// before, and no commit status.
func todaysHead() State {
	check := func(name string, id uint64, conclusion string) Check {
		return Check{Name: name, CheckRunID: checkRunID(id), URL: "https://github.com/acme/widgets/runs/" + name, Status: "completed", Conclusion: conclusion, ObservedAt: "2026-11-17T06:00:00Z"}
	}
	return State{
		Owner: "acme", Repo: "widgets", Number: "7", SHA: "0123456789abcdef0123456789abcdef01234567",
		Checks: map[string]Check{
			"build": check("build", 11, "success"),
			"lint":  check("lint", 12, "failure"),
			"image": check("image", 13, "cancelled"),
			"docs":  check("docs", 14, "skipped"),
		},
		Suites:       map[string]Suite{"900": {ID: "900", AppID: "15368", Status: "completed", Conclusion: "failure", ObservedAt: "2026-11-17T06:00:00Z"}},
		Generation:   3,
		EmittedCount: 1,
		LastEventAt:  1,
	}
}

// todaysPayload and todaysLine are the payload and the one line the listener of the release before
// commit statuses settled todaysHead in, byte for byte, with each check_runs entry carrying its
// run's conclusion word as this listener adds it: its renderSummary and settledPayloadSummary, run
// on that head.
const (
	todaysPayload = `{"kind":"checks","repo":"acme/widgets","number":"7","sha":"0123456789abcdef0123456789abcdef01234567",` +
		`"check_runs":[{"name":"build","id":11,"conclusion":"success"},{"name":"docs","id":14,"conclusion":"skipped"},` +
		`{"name":"image","id":13,"conclusion":"cancelled"},{"name":"lint","id":12,"conclusion":"failure"}],` +
		`"generation":3,"snapshot":"8a05685f026c9caa16161a227de93b41bd04ed7de085ba044b495754afd55bdf","superseded_settlement":"true",` +
		`"failed":{"count":1,"checks":["lint"]},"running":{"count":0,"checks":[]},"passed":{"count":1,"checks":["build"]},` +
		`"queued":{"count":0,"checks":[]},"cancelled":{"count":1,"checks":["image"]},"skipped":{"count":1,"checks":["docs"]},` +
		`"failing_checks":[{"name":"lint","url":"https://github.com/acme/widgets/runs/lint"}]}`
	todaysLine = "checks settled on acme/widgets#7 @ 0123456: 1 passed, 1 failed, 1 cancelled, 1 skipped; failing: lint (re-settled)"
)

// Each settlement names each commit status of the record with its state and its created_at, in a
// field of its own and in context order. Its check_runs, failed and cancelled name check runs
// alone, and are the same with and without statuses on the record; its one line counts the
// statuses and names the failed ones after the failing check runs. A head with no status settles
// in the payload and the line of the release before commit statuses, byte for byte, its snapshot
// included.
func TestASettlementNamesStatusesAndKeepsTodaysFields(t *testing.T) {
	t.Run("a head with no status settles in today's bytes", func(t *testing.T) {
		env, err := settlementEnvelope(todaysHead(), 1)
		if err != nil {
			t.Fatalf("settlement: %v", err)
		}
		if env.Payload != todaysPayload {
			t.Fatalf("the settlement of a head with no status is\n%s\nwant today's\n%s", env.Payload, todaysPayload)
		}
		if env.PayloadSummary != todaysLine {
			t.Fatalf("the settlement of a head with no status reads %q, want today's %q", env.PayloadSummary, todaysLine)
		}
	})

	t.Run("the statuses are named apart from the check runs", func(t *testing.T) {
		today := renderSummary(todaysHead())
		withStatuses := todaysHead()
		withStatuses.Statuses = map[string]Status{
			"test":       {Context: "test", State: "failure", CreatedAt: "2026-11-17T06:01:00Z", URL: "https://ci.example.com/test", ObservedAt: 1},
			"ci/preview": {Context: "ci/preview", State: "success", CreatedAt: "2026-11-17T06:01:00Z", URL: "https://ci.example.com/preview", ObservedAt: 1},
			"ci/license": {Context: "ci/license", State: "error", CreatedAt: "2026-11-17T06:01:00Z", ObservedAt: 1},
		}
		env, err := settlementEnvelope(withStatuses, 1)
		if err != nil {
			t.Fatalf("settlement: %v", err)
		}
		var sum Summary
		if err := json.Unmarshal([]byte(env.Payload), &sum); err != nil {
			t.Fatalf("decode the settlement: %v", err)
		}
		want := []StatusRef{{"ci/license", "error", "2026-11-17T06:01:00Z"}, {"ci/preview", "success", "2026-11-17T06:01:00Z"}, {"test", "failure", "2026-11-17T06:01:00Z"}}
		if !slices.Equal(sum.Statuses, want) {
			t.Fatalf("statuses = %+v, want %+v", sum.Statuses, want)
		}
		if !slices.Equal(sum.CheckRuns, today.CheckRuns) || !reflect.DeepEqual(sum.Failed, today.Failed) || !reflect.DeepEqual(sum.Cancelled, today.Cancelled) ||
			!reflect.DeepEqual(sum.Passed, today.Passed) || !slices.Equal(sum.FailingChecks, today.FailingChecks) {
			t.Fatalf("with statuses the settlement names check runs %+v, failed %+v, cancelled %+v, passed %+v, failing %+v; want today's %+v, %+v, %+v, %+v, %+v",
				sum.CheckRuns, sum.Failed, sum.Cancelled, sum.Passed, sum.FailingChecks, today.CheckRuns, today.Failed, today.Cancelled, today.Passed, today.FailingChecks)
		}
		if sum.Snapshot == today.Snapshot {
			t.Fatal("the statuses left the settlement's snapshot as it is without them")
		}
		var payload, todays map[string]json.RawMessage
		if err := json.Unmarshal([]byte(env.Payload), &payload); err != nil {
			t.Fatalf("decode the settlement: %v", err)
		}
		if err := json.Unmarshal([]byte(todaysPayload), &todays); err != nil {
			t.Fatalf("decode today's settlement: %v", err)
		}
		if got := string(payload["statuses"]); got != `[{"context":"ci/license","state":"error","created_at":"2026-11-17T06:01:00Z"},`+
			`{"context":"ci/preview","state":"success","created_at":"2026-11-17T06:01:00Z"},{"context":"test","state":"failure","created_at":"2026-11-17T06:01:00Z"}]` {
			t.Fatalf("the settlement's statuses field is %s", got)
		}
		delete(payload, "statuses")
		delete(payload, "snapshot")
		delete(todays, "snapshot")
		if !reflect.DeepEqual(payload, todays) {
			t.Fatalf("with statuses, every other field of the settlement is not today's:\n%s\nwant\n%s", env.Payload, todaysPayload)
		}
		// The line is the listener's one-line summary, cut at 160 runes (contracts.OneLineSummary),
		// as a long list of failing check runs is.
		full := "checks settled on acme/widgets#7 @ 0123456: 1 passed, 1 failed, 1 cancelled, 1 skipped; failing: lint; statuses: 1 passed, 2 failed; failing statuses: ci/license, test (re-settled)"
		if want := contracts.OneLineSummary(full); env.PayloadSummary != want {
			t.Fatalf("the settlement reads %q, want %q", env.PayloadSummary, want)
		}
		// The statuses come after the failing check runs and before the re-settled mark, and only
		// a failed status is named.
		short := Summary{
			Repo: "a/w", Number: "7", SHA: "0123456789abcdef", Passed: group([]string{"build"}), Failed: group([]string{"lint"}),
			SupersededSettlement: "true", Statuses: []StatusRef{{"ci/preview", "success", "2026-11-17T06:01:00Z"}, {"x", "error", "2026-11-17T06:01:00Z"}},
		}
		if got, want := settledPayloadSummary(short), "checks settled on a/w#7 @ 0123456: 1 passed, 1 failed, 0 cancelled, 0 skipped; failing: lint; statuses: 1 passed, 1 failed; failing statuses: x (re-settled)"; got != want {
			t.Fatalf("a settlement of a passed and a failed status reads %q, want %q", got, want)
		}
		passing := todaysHead()
		passing.EmittedCount = 0
		passing.Statuses = map[string]Status{"ci/preview": {Context: "ci/preview", State: "success"}}
		if got, want := settledPayloadSummary(renderSummary(passing)), "checks settled on acme/widgets#7 @ 0123456: 1 passed, 1 failed, 1 cancelled, 1 skipped; failing: lint; statuses: 1 passed, 0 failed"; got != want {
			t.Fatalf("a settlement of a passed status alone reads %q, want %q", got, want)
		}
	})

	t.Run("through the store and the summary loop", func(t *testing.T) {
		conn, cleanup := connectNATS(t)
		defer cleanup()
		s := openStore(t, conn)
		pub := &recPub{}
		plain := head{"acme", "widgets", "7", "0123456789abcdef0123456789abcdef01234567"}
		named := head{"acme", "widgets", "8", "89abcdef0123456789abcdef0123456789abcdef"}
		for _, h := range []head{plain, named} {
			for _, run := range []struct{ name, id, conclusion string }{{"build", "11", "success"}, {"lint", "12", "failure"}, {"image", "13", "cancelled"}, {"docs", "14", "skipped"}} {
				if err := recordCheck(s, h.owner, h.repo, h.number, h.sha, run.name, run.id, "https://github.com/acme/widgets/runs/"+run.id, "completed", run.conclusion, "2026-11-17T06:00:00Z"); err != nil {
					t.Fatalf("record %s on #%s: %v", run.name, h.number, err)
				}
			}
		}
		for _, status := range []contracts.StatusObservation{
			named.status("test", "failure", "2026-11-17T06:01:00Z"),
			named.status("ci/preview", "success", "2026-11-17T06:01:00Z"),
			named.status("ci/license", "error", "2026-11-17T06:01:00Z"),
		} {
			if changed, err := recordStatus(s, status); err != nil || !changed {
				t.Fatalf("record the status %s: changed %v, %v", status.Context, changed, err)
			}
		}
		waitCacheChecks(t, s, plain.owner, plain.repo, plain.number, plain.sha, 4)
		waitCached(t, s, named.key(), func(st State) bool { return len(st.Checks) == 4 && len(st.Statuses) == 3 })
		runSummaryTick(s, pub, 0, logging.New("test"))
		settlements := map[string]Summary{}
		payloads := map[string]string{}
		for _, env := range pub.all() {
			var sum Summary
			if err := json.Unmarshal([]byte(env.Payload), &sum); err != nil {
				t.Fatalf("decode a settlement: %v", err)
			}
			settlements[sum.Number], payloads[sum.Number] = sum, env.Payload
		}
		withNone, withStatuses := settlements[plain.number], settlements[named.number]
		if len(settlements) != 2 || pub.count() != 2 {
			t.Fatalf("published %d settlements for %d heads, want one for each of the two", pub.count(), len(settlements))
		}
		if strings.Contains(payloads[plain.number], `"statuses"`) {
			t.Fatalf("the settlement of a head with no status names statuses: %s", payloads[plain.number])
		}
		if want := []StatusRef{{"ci/license", "error", "2026-11-17T06:01:00Z"}, {"ci/preview", "success", "2026-11-17T06:01:00Z"}, {"test", "failure", "2026-11-17T06:01:00Z"}}; !slices.Equal(withStatuses.Statuses, want) {
			t.Fatalf("the settlement names statuses %+v, want %+v", withStatuses.Statuses, want)
		}
		if !slices.Equal(withStatuses.CheckRuns, withNone.CheckRuns) || !reflect.DeepEqual(withStatuses.Failed, withNone.Failed) || !reflect.DeepEqual(withStatuses.Cancelled, withNone.Cancelled) {
			t.Fatalf("with statuses the settlement names check runs %+v, failed %+v, cancelled %+v; without, %+v, %+v, %+v",
				withStatuses.CheckRuns, withStatuses.Failed, withStatuses.Cancelled, withNone.CheckRuns, withNone.Failed, withNone.Cancelled)
		}
	})
}

// bigText is a text past the envelope text cap in `<`, which the record writes in 6 bytes a
// character (`\u003c`), ended by i so each is its own.
func bigText(i int) string { return fmt.Sprintf("%s%04d", strings.Repeat("<", 3000), i) }

// bigCheckName is a check name at the envelope text cap in `<`, ended by i so each is its own.
func bigCheckName(i int) string { return fmt.Sprintf("%s%04d", strings.Repeat("<", 2044), i) }

// bigStatus is a failed status of h as the reconcile's mapping gives it, whose context, link and
// description are each cut at the envelope text cap, so it costs the record about twice what a
// check of bigCheckName does.
func bigStatus(t *testing.T, h head, i int) contracts.StatusObservation {
	t.Helper()
	observation, ok := contracts.GithubStatusObservation(map[string]any{
		"context": bigText(i), "state": "failure", "created_at": "2026-11-17T05:00:00Z",
		"target_url": bigText(i), "description": bigText(i),
	}, h.owner, h.repo, h.number, h.sha)
	if !ok {
		t.Fatal("a big failed status maps to no observation")
	}
	return observation
}

// bigCheck is the passing check run i of h, named bigCheckName(i).
func bigCheck(i int) Check {
	return Check{Name: bigCheckName(i), CheckRunID: checkRunID(1000 + i), URL: fmt.Sprintf("https://github.com/acme/widgets/runs/%d", 1000+i),
		Status: "completed", Conclusion: "success", ObservedAt: "2026-11-17T05:00:00Z"}
}

// recordBigCheck records bigCheck(i) on h.
func recordBigCheck(s *Store, h head, i int) error {
	check := bigCheck(i)
	return recordCheck(s, h.owner, h.repo, h.number, h.sha, check.Name, fmt.Sprint(uint64(check.CheckRunID)), check.URL, check.Status, check.Conclusion, check.ObservedAt)
}

// sizeWith is the size h's record would be written in with change applied to it, as the store
// writes it: each write of an unsettled record that changes its snapshot advances its generation.
func sizeWith(t *testing.T, s *Store, h head, change func(*State)) int {
	t.Helper()
	st := getState(t, s, h.owner, h.repo, h.number, h.sha)
	change(&st)
	st.Generation++
	buf, err := encodeRecord(&st)
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	return len(buf)
}

// withCheck adds bigCheck(i) to a record.
func withCheck(i int) func(*State) {
	return func(st *State) {
		if st.Checks == nil {
			st.Checks = map[string]Check{}
		}
		st.Checks[bigCheckName(i)] = bigCheck(i)
	}
}

// fillChecks records bigCheck(from), bigCheck(from+1) and on, on h, while the record holds the next
// one with room bytes to spare under its bound, and returns the index of the first it left out.
func fillChecks(t *testing.T, s *Store, h head, from, room int) int {
	t.Helper()
	i := from
	for ; sizeWith(t, s, h, withCheck(i)) <= maxRecordBytes-room; i++ {
		if err := recordBigCheck(s, h, i); err != nil {
			t.Fatalf("record check %d: %v", i, err)
		}
	}
	return i
}

// warnings returns the JSON lines logs holds whose message is msg.
func warnings(logs *bytes.Buffer, msg string) []map[string]any {
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == msg {
			lines = append(lines, entry)
		}
	}
	return lines
}

// assertWarning fails unless logs holds exactly one WARN line msg naming h and each of fields.
func assertWarning(t *testing.T, logs *bytes.Buffer, msg string, h head, fields map[string]any) {
	t.Helper()
	lines := warnings(logs, msg)
	if len(lines) != 1 {
		t.Fatalf("%d lines %q, want one:\n%s", len(lines), msg, logs.String())
	}
	want := map[string]any{"level": "WARN", "owner": h.owner, "repo": h.repo, "number": h.number, "sha": h.sha}
	for field, value := range fields {
		want[field] = value
	}
	for field, value := range want {
		if lines[0][field] != value {
			t.Fatalf("the line %q has %s %v, want %v:\n%s", msg, field, lines[0][field], value, logs.String())
		}
	}
}

// A commit status never costs a head the settlements of its check runs. A status whose write
// would take the record past its bound is not recorded, a WARN line naming the head and the
// context, and the record is not marked Overflowed, so the head's check runs still settle. A check
// run whose write passes the bound only for the statuses the record holds is recorded and the
// statuses are dropped, a WARN line naming the head and how many, so the head still settles. Each
// rule judges the write as the store batches it: a status and a check run written together, past
// the bound together, record the check run alone. Where the check run is past the bound with no
// status left to give way, the record is marked Overflowed and the check run's write is refused,
// while the status that rode the same write is answered no change and no error.
func TestAStatusPastTheRecordBoundLeavesTheChecksSettling(t *testing.T) {
	t.Run("a status past the bound is not recorded", func(t *testing.T) {
		conn, cleanup := connectNATS(t)
		defer cleanup()
		s := openStore(t, conn)
		var logs bytes.Buffer
		s.logger = logging.NewWithWriter("test", &logs)
		pub := &recPub{}
		h := statusHead
		if err := recordBigCheck(s, h, 0); err != nil {
			t.Fatalf("record check 0: %v", err)
		}
		checks := fillChecks(t, s, h, 1, 0)
		status := bigStatus(t, h, 0)
		if size := sizeWith(t, s, h, func(st *State) {
			st.Statuses = map[string]Status{status.Context: {Context: status.Context, State: status.State, URL: status.URL, Description: status.Description}}
		}); size <= maxRecordBytes {
			t.Fatalf("the record with the status is %d bytes, within its %d-byte bound; the test needs it past", size, maxRecordBytes)
		}
		if changed, err := recordStatus(s, status); err != nil || changed {
			t.Fatalf("a status past the bound: changed %v, %v; want no change and no error", changed, err)
		}
		st := getState(t, s, h.owner, h.repo, h.number, h.sha)
		if st.Overflowed || len(st.Statuses) != 0 || len(st.Checks) != checks {
			t.Fatalf("after a status past the bound the record is overflowed %v with %d statuses and %d checks, want not overflowed with none and %d",
				st.Overflowed, len(st.Statuses), len(st.Checks), checks)
		}
		assertWarning(t, &logs, "ci record refused a commit status past its bound", h, map[string]any{"context": status.Context})

		waitCacheChecks(t, s, h.owner, h.repo, h.number, h.sha, checks)
		runSummaryTick(s, pub, 0, logging.New("test"))
		if pub.count() != 1 {
			t.Fatalf("the head published %d settlements after the status was refused, want its check runs' one", pub.count())
		}
		if sum, _ := lastSettlement(t, pub); len(sum.CheckRuns) != checks || sum.Statuses != nil {
			t.Fatalf("the settlement names %d check runs and statuses %v, want the %d check runs alone", len(sum.CheckRuns), sum.Statuses, checks)
		}
	})

	t.Run("a check run past the bound for the record's statuses drops them", func(t *testing.T) {
		conn, cleanup := connectNATS(t)
		defer cleanup()
		s := openStore(t, conn)
		var logs bytes.Buffer
		s.logger = logging.NewWithWriter("test", &logs)
		pub := &recPub{}
		h := statusHead
		for i := range 3 {
			if changed, err := recordStatus(s, bigStatus(t, h, i)); err != nil || !changed {
				t.Fatalf("record status %d: changed %v, %v", i, changed, err)
			}
		}
		next := fillChecks(t, s, h, 0, 0)
		if with, without := sizeWith(t, s, h, withCheck(next)), sizeWith(t, s, h, func(st *State) { st.Statuses = nil; withCheck(next)(st) }); with <= maxRecordBytes || without > maxRecordBytes {
			t.Fatalf("the record with check %d is %d bytes with its statuses and %d without; the test needs it past the %d-byte bound with them alone", next, with, without, maxRecordBytes)
		}
		if err := recordBigCheck(s, h, next); err != nil {
			t.Fatalf("a check run past the bound for the record's statuses: %v, want it recorded", err)
		}
		st := getState(t, s, h.owner, h.repo, h.number, h.sha)
		if st.Overflowed || len(st.Statuses) != 0 || len(st.Checks) != next+1 {
			t.Fatalf("the record is overflowed %v with %d statuses and %d checks, want not overflowed with no status and %d checks",
				st.Overflowed, len(st.Statuses), len(st.Checks), next+1)
		}
		assertWarning(t, &logs, "ci record dropped its commit statuses to keep a check within its bound", h, map[string]any{"statuses": 3.0})

		waitCacheChecks(t, s, h.owner, h.repo, h.number, h.sha, next+1)
		runSummaryTick(s, pub, 0, logging.New("test"))
		if pub.count() != 1 {
			t.Fatalf("the head published %d settlements after its statuses were dropped, want one", pub.count())
		}
		if sum, _ := lastSettlement(t, pub); len(sum.CheckRuns) != next+1 || sum.Statuses != nil {
			t.Fatalf("the settlement names %d check runs and statuses %v, want the %d check runs alone", len(sum.CheckRuns), sum.Statuses, next+1)
		}
	})

	t.Run("a status and a check run in one write record the check run", func(t *testing.T) {
		conn, cleanup := connectNATS(t)
		defer cleanup()
		s, kv := wrapStore(t, conn, 0)
		var logs bytes.Buffer
		s.logger = logging.NewWithWriter("test", &logs)
		h := statusHead
		if err := recordBigCheck(s, h, 0); err != nil {
			t.Fatalf("record check 0: %v", err)
		}
		// The record keeps 4 KiB to spare, room for two small check runs and not for the status.
		fillChecks(t, s, h, 1, 4<<10)
		status := bigStatus(t, h, 0)
		var statusChanged bool
		entered, release := kv.holdNextWrite()
		together := newCalls(t, s, h.key(), 3, func(i int) error {
			switch i {
			case 0:
				return s.Record(context.Background(), h.check("small-a", 1, "completed", "success", "2026-11-17T05:00:00Z"))
			case 1:
				changed, err := s.recordStatus(context.Background(), status)
				statusChanged = changed
				return err
			default:
				return s.Record(context.Background(), h.check("small-b", 2, "completed", "success", "2026-11-17T05:00:00Z"))
			}
		})
		together.behind(entered, 3)
		release()
		requireNoErrors(t, "a status and a check run in one write", together.wait("a status and a check run in one write"))
		kv.onWrite(nil)
		if statusChanged {
			t.Fatal("the status past the bound answered that it changed the record")
		}
		st := getState(t, s, h.owner, h.repo, h.number, h.sha)
		if _, ok := st.Checks["small-b"]; !ok || st.Overflowed || len(st.Statuses) != 0 {
			t.Fatalf("the record holds small-b %v, overflowed %v, %d statuses; want the check run recorded, no status, not overflowed",
				ok, st.Overflowed, len(st.Statuses))
		}
		assertWarning(t, &logs, "ci record refused a commit status past its bound", h, map[string]any{"context": status.Context})
	})

	t.Run("a status beside a check run past the bound is answered no change and no error", func(t *testing.T) {
		conn, cleanup := connectNATS(t)
		defer cleanup()
		s, kv := wrapStore(t, conn, 0)
		var logs bytes.Buffer
		s.logger = logging.NewWithWriter("test", &logs)
		h := statusHead
		if err := recordBigCheck(s, h, 0); err != nil {
			t.Fatalf("record check 0: %v", err)
		}
		// The record keeps room for one more big check run and not for two: the first is the
		// write the batch queues behind, and the second rides the batch beside the status.
		cost := sizeWith(t, s, h, withCheck(1)) - sizeWith(t, s, h, func(*State) {})
		first := fillChecks(t, s, h, 1, cost)
		second := first + 1
		if one, two := sizeWith(t, s, h, withCheck(first)), sizeWith(t, s, h, func(st *State) { withCheck(first)(st); withCheck(second)(st) }); one > maxRecordBytes || two <= maxRecordBytes {
			t.Fatalf("the record is %d bytes with check %d and %d with check %d beside it; the test needs the first within the %d-byte bound and the second past it",
				one, first, two, second, maxRecordBytes)
		}
		status := bigStatus(t, h, 0)
		var statusChanged bool
		var statusErr error
		entered, release := kv.holdNextWrite()
		together := newCalls(t, s, h.key(), 3, func(i int) error {
			switch i {
			case 0:
				return recordBigCheck(s, h, first)
			case 1:
				statusChanged, statusErr = s.recordStatus(context.Background(), status)
				return nil
			default:
				return recordBigCheck(s, h, second)
			}
		})
		together.behind(entered, 3)
		release()
		errs := together.wait("a status and a check run past the bound in one write")
		kv.onWrite(nil)
		if errs[0] != nil {
			t.Fatalf("the check run the batch queued behind: %v, want it recorded", errs[0])
		}
		if statusChanged || statusErr != nil {
			t.Fatalf("the status beside a check run past the bound: changed %v, %v; want no change and no error", statusChanged, statusErr)
		}
		if !errors.Is(errs[2], bus.ErrTooLarge) {
			t.Fatalf("the check run past the bound: %v, want the bound's refusal (bus.ErrTooLarge)", errs[2])
		}
		st := getState(t, s, h.owner, h.repo, h.number, h.sha)
		_, kept := st.Checks[bigCheckName(first)]
		_, refused := st.Checks[bigCheckName(second)]
		if !st.Overflowed || !kept || refused || len(st.Statuses) != 0 {
			t.Fatalf("the record is overflowed %v, holds check %d %v, check %d %v and %d statuses; want it overflowed with check %d, without check %d and with no status",
				st.Overflowed, first, kept, second, refused, len(st.Statuses), first, second)
		}
		assertWarning(t, &logs, "ci record refused a commit status past its bound", h, map[string]any{"context": status.Context})
	})
}

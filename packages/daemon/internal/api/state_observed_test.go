package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// observingSource is a state source that records what the route handed each of its two reads: the
// transaction, and whether the route had bound that transaction's statements before the read ran.
// Its observed read answers observed, or err.
type observingSource struct {
	observed State
	err      error
	// fullReads counts the reads of the whole state, which an observed request makes none of.
	fullReads int
	// tx is the transaction the observed read was handed, and bounded whether its statement bound
	// was set when the read ran.
	tx      *fakeTx
	bounded bool
}

func (s *observingSource) State(context.Context, pgx.Tx) (State, []supervise.Claim, error) {
	s.fullReads++
	return State{}, nil, nil
}

func (s *observingSource) ObservedState(_ context.Context, tx pgx.Tx) (State, []supervise.Claim, error) {
	s.tx, _ = tx.(*fakeTx)
	if s.tx != nil {
		s.bounded = s.tx.bounded
	}
	return s.observed, nil, s.err
}

// observedFixture is the part of a state an observed read answers, with one issue of each shape the
// view carries: a root at work holding a slot, with its architect and one worker, and an issue the
// workflow does not record, which an operator's claim lists with nothing but its phase and that
// claim. It also holds members the observed read answers and the view does not carry: the slot's
// admission time, the worker's counts and the claims' sessions.
func observedFixture() State {
	seq := int64(41)
	return State{
		Daemon: DaemonInfo{Project: "LEGION", SchemaVersion: 20261126163320, Boots: 3,
			FirstBootAt: time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC), StartedAt: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC),
			StateSeq: &seq, StateID: "3f6b2c8e-91d4-4a7b-b5e0-6c2d8f1a9e47", StateRun: "5f0c7a3e-2b9d-4e61-8c47-1d3a9b6e2f80"},
		Admission: Admission{Cap: 2, Active: []string{"LEGION-1"}, Waiting: []string{}, Trees: []string{"LEGION-1"}},
		Issues: map[string]Issue{
			"LEGION-1": {Key: "LEGION-1", Phase: phase.Implementing, Tree: "LEGION-1",
				Slot:      &SlotView{Index: 0, AdmittedAt: time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)},
				Architect: &ClaimView{State: string(supervise.StateIdle), Session: "ses_architect_1"},
				Workers: map[claim.Role]PhaseView{claim.RoleImplementer: {
					Claim: ClaimView{State: string(supervise.StateWorking), Session: "ses_implementer_1"}, Pass: 2, Rounds: 1}}},
			"LEGION-7": {Key: "LEGION-7", Phase: Unrecorded, Architect: &ClaimView{State: string(supervise.StateLaunching)}},
		},
	}
}

// observedViewFixture is the observed view of observedFixture, as the route writes it.
func observedViewFixture() ObservedState {
	seq := int64(41)
	return ObservedState{
		Daemon: DaemonInfo{Project: "LEGION", SchemaVersion: 20261126163320, Boots: 3,
			FirstBootAt: time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC), StartedAt: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC),
			StateSeq: &seq, StateID: "3f6b2c8e-91d4-4a7b-b5e0-6c2d8f1a9e47", StateRun: "5f0c7a3e-2b9d-4e61-8c47-1d3a9b6e2f80"},
		Admission: Admission{Cap: 2, Active: []string{"LEGION-1"}, Waiting: []string{}, Trees: []string{"LEGION-1"}},
		Issues: map[string]ObservedIssue{
			"LEGION-1": {Key: "LEGION-1", Phase: phase.Implementing, Tree: "LEGION-1", Slot: &ObservedSlot{Index: 0},
				Architect: &ObservedClaim{State: string(supervise.StateIdle)},
				Workers:   map[claim.Role]ObservedWorker{claim.RoleImplementer: {Claim: ObservedClaim{State: string(supervise.StateWorking)}}}},
			"LEGION-7": {Key: "LEGION-7", Phase: Unrecorded, Architect: &ObservedClaim{State: string(supervise.StateLaunching)}},
		},
	}
}

// `GET /legion/v1/state?view=observed` is served from the source's observed read, and from it
// alone: in a repeatable-read, read-only transaction whose statements the route has bound before
// the read runs, committed before the body is written, and with the observed view of what the
// source read, byte for byte, which carries none of the whole state's other keys and none of the
// other members of an issue.
func TestTheStateRouteServesTheObservedViewFromTheSourcesObservedRead(t *testing.T) {
	src := &observingSource{observed: observedFixture()}
	txs := &fakeStateTransactions{tx: &fakeTx{}}

	recorder := serveWithTransactions(t, src, txs, http.MethodGet, "/legion/v1/state?view=observed")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("content-type = %q, want application/json", contentType)
	}
	if src.fullReads != 0 {
		t.Fatalf("the observed view read the whole state %d times, want none", src.fullReads)
	}
	if txs.opts.IsoLevel != pgx.RepeatableRead || txs.opts.AccessMode != pgx.ReadOnly {
		t.Fatalf("transaction options = %#v, want repeatable-read read-only", txs.opts)
	}
	if src.tx != txs.tx {
		t.Fatal("the observed read was not handed the route's own transaction")
	}
	if !src.bounded {
		t.Fatal("the observed read ran before the route bound its transaction's statements")
	}
	if !txs.tx.committed {
		t.Fatal("the observed view's transaction was not committed")
	}
	want, err := json.Marshal(observedViewFixture())
	if err != nil {
		t.Fatalf("encode the observed view: %v", err)
	}
	if got := recorder.Body.String(); got != string(want)+"\n" {
		t.Fatalf("body = %s\nwant the source's view %s", got, want)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode body %s: %v", recorder.Body, err)
	}
	if len(keys) != 4 || keys["daemon"] == nil || keys["settings"] == nil || keys["admission"] == nil || keys["issues"] == nil {
		t.Fatalf("body keys = %v, want daemon, settings, admission and issues alone", keys)
	}

	// An observed view that lists no issue still writes `issues` as an object, which a reader's
	// schema requires.
	empty := serveWithTransactions(t, &observingSource{}, &fakeStateTransactions{tx: &fakeTx{}}, http.MethodGet, "/legion/v1/state?view=observed")
	if body := empty.Body.String(); empty.Code != http.StatusOK || !strings.Contains(body, `"issues":{}`) || !strings.Contains(body, `"active":[]`) {
		t.Fatalf("empty observed view = %d %s, want 200 with an empty issues object and an empty active list", empty.Code, body)
	}
}

// Any `view` but `observed` gets the whole state, as a request with no `view` does, and the daemon
// logs one warning for it that names the values given and the one view the route serves, and says
// that a later version may refuse such a request. A request with no `view` and a request for the
// observed view log no such warning.
func TestTheStateRouteServesTheWholeStateForAnyOtherView(t *testing.T) {
	serveLogged := func(t *testing.T, query string) (*httptest.ResponseRecorder, *observingSource, []map[string]any) {
		t.Helper()
		var logged strings.Builder
		src := &observingSource{observed: observedFixture()}
		server := NewServer("127.0.0.1", 8437, Options{State: src, StateTransactions: &fakeStateTransactions{tx: &fakeTx{}},
			Log: slog.New(slog.NewJSONHandler(&logged, nil))})
		recorder := httptest.NewRecorder()
		server.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/legion/v1/state"+query, nil))
		return recorder, src, warnRecords(t, logged.String())
	}

	whole, wholeSource, wholeWarnings := serveLogged(t, "")
	if whole.Code != http.StatusOK || wholeSource.fullReads != 1 || wholeSource.tx != nil || len(wholeWarnings) != 0 {
		t.Fatalf("a request with no view = %d, whole reads %d, observed read %t, warnings %v; want 200 from one read of the whole state and no warning",
			whole.Code, wholeSource.fullReads, wholeSource.tx != nil, wholeWarnings)
	}
	if _, observedSource, warnings := serveLogged(t, "?view=observed"); observedSource.tx == nil || len(warnings) != 0 {
		t.Fatalf("a request for the observed view read it %t and logged %v, want it read and no warning", observedSource.tx != nil, warnings)
	}

	for _, tc := range []struct {
		query string
		views []any
	}{
		{"view=full", []any{"full"}},
		{"view=", []any{""}},
		{"view=Observed", []any{"Observed"}},
		{"view=observed&view=full", []any{"observed", "full"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			recorder, src, warnings := serveLogged(t, "?"+tc.query)
			if recorder.Code != http.StatusOK || src.fullReads != 1 || src.tx != nil {
				t.Fatalf("view %s = %d, whole reads %d, observed read %t; want 200 from one read of the whole state",
					tc.query, recorder.Code, src.fullReads, src.tx != nil)
			}
			if got, want := recorder.Body.String(), whole.Body.String(); got != want {
				t.Fatalf("view %s served %s, want the whole state a request with no view gets: %s", tc.query, got, want)
			}
			if len(warnings) != 1 {
				t.Fatalf("view %s logged %d warnings %v, want one", tc.query, len(warnings), warnings)
			}
			warning := warnings[0]
			message, _ := warning["msg"].(string)
			if !strings.Contains(message, "whole state") || !strings.Contains(message, "a later version may refuse") ||
				!reflect.DeepEqual(warning["views"], tc.views) || warning["onlyView"] != "observed" {
				t.Fatalf("view %s logged %v, want a warning that the whole state was served, naming the views %v and the one view %q, and that a later version may refuse such a request",
					tc.query, warning, tc.views, "observed")
			}
		})
	}
}

// warnRecords decodes logged, the JSON lines of a slog JSON handler, and returns the records at
// WARN, in the order they were logged.
func warnRecords(t *testing.T, logged string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %s: %v", line, err)
		}
		if record["level"] == "WARN" {
			found = append(found, record)
		}
	}
	return found
}

// An observed read that fails is answered 500 with nothing of the view, and its transaction is
// rolled back rather than committed.
func TestTheObservedViewReportsASourceFailure(t *testing.T) {
	var logged strings.Builder
	src := &observingSource{observed: observedFixture(), err: errors.New("dial db.internal: refused")}
	txs := &fakeStateTransactions{tx: &fakeTx{}}
	server := NewServer("127.0.0.1", 8437, Options{State: src, StateTransactions: txs, Log: slog.New(slog.NewTextHandler(&logged, nil))})
	recorder := httptest.NewRecorder()
	server.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/legion/v1/state?view=observed", nil))

	if body := strings.TrimSpace(recorder.Body.String()); recorder.Code != http.StatusInternalServerError || body != `{"error":"state unavailable"}` {
		t.Fatalf("a failed observed read = %d %s, want 500 state unavailable", recorder.Code, body)
	}
	if txs.tx.committed || !txs.tx.rolledBack {
		t.Fatalf("a failed observed read committed %t and rolled back %t, want rolled back only", txs.tx.committed, txs.tx.rolledBack)
	}
	if !strings.Contains(logged.String(), "api: observed state source failed") {
		t.Fatalf("log = %s, want the failed observed read logged", logged.String())
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// `GET /legion/v1/state`, as the daemon's own source answers it over the record, shows each wait
// that stands on the claim whose worker declared it, and that issue's record is what
// `read_record` reads: the wait's id, what it waits on, its time and when the daemon recorded the
// declaration. A wait row with no end that no longer stands shows on no claim: the implementer
// below declared its wait in a session its claim no longer runs, as a relaunch in a fresh session
// leaves it outside any fact, so its row has no end until the next fact's reconciler writes one,
// and no reader of the state may take that worker for waiting meanwhile.
func TestTheStateRouteShowsEachWaitThatStandsAndNoOther(t *testing.T) {
	cfg := testConfig(t)
	tested, building := cfg.Project+"-1", cfg.Project+"-2"
	tester := previousClaim(t, cfg, tested, tested, claim.RoleTester, supervise.StateReady)
	implementer := previousClaim(t, cfg, building, building, claim.RoleImplementer, supervise.StateReady)
	putPreviousRun(t, cfg, previousRun{
		issues: []record.Issue{runningRoot(tested, phase.Testing), runningRoot(building, phase.Implementing)},
		slots:  []string{tested, building},
		claims: []supervise.Claim{tester, implementer},
	})

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	records := record.NewStore()
	declaredAt := time.Now().UTC().Truncate(time.Microsecond)
	waitOf := func(c supervise.Claim, p phase.Phase, session, waitingOn string) record.ClaimWait {
		return record.ClaimWait{ID: "wait-" + string(c.Token), Project: project, Token: c.Token, Issue: c.Issue, Role: c.Role,
			Generation: 1, Phase: p, PhaseEntry: 1, TakenEntry: 1, Session: session, WaitingOn: waitingOn,
			WakeAt: declaredAt.Add(2 * time.Hour).Truncate(time.Second), DeclaredAt: declaredAt}
	}
	standing := waitOf(tester, phase.Testing, tester.Session, "the review of the head")
	stale := waitOf(implementer, phase.Implementing, "ses_before_the_relaunch", "CI on the rebased head")
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		for _, wait := range []record.ClaimWait{standing, stale} {
			if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: wait.Issue, Role: wait.Role, Claim: wait.Token, Entry: 1}); err != nil {
				return err
			}
			if _, err := records.DeclareWait(ctx, tx, wait); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("declare the two waits: %v", err)
	}

	view := &source{
		store: st, records: records, project: cfg.Project, projectToken: project, runtime: "tmux", admissionCap: cfg.AdmissionCap,
		startedAt: time.Now(), deployment: func() capabilities.Deployment { return capabilities.Deployment{} },
		reports: func() map[claim.Token]capabilities.Report { return nil },
	}
	handler := api.NewServer("127.0.0.1", 0, api.Options{State: view, StateTransactions: st}).Handler
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /legion/v1/state = %d %s, want 200", recorder.Code, recorder.Body)
	}
	var state api.State
	if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode the state: %v", err)
	}

	worker, listed := state.Issues[tested].Workers[claim.RoleTester]
	if !listed {
		t.Fatalf("%s's record lists no tester: %+v", tested, state.Issues[tested])
	}
	want := &api.WaitView{ID: standing.ID, WaitingOn: standing.WaitingOn, WakeAt: standing.WakeAt, DeclaredAt: declaredAt}
	if got := worker.Claim.Wait; got == nil || got.ID != want.ID || got.WaitingOn != want.WaitingOn ||
		!got.WakeAt.Equal(want.WakeAt) || !got.DeclaredAt.Equal(want.DeclaredAt) {
		t.Fatalf("%s's tester carries the wait %+v, want the one it declared, %+v", tested, got, want)
	}
	if worker, listed := state.Issues[building].Workers[claim.RoleImplementer]; !listed || worker.Claim.Wait != nil {
		t.Fatalf("%s's implementer = %+v (listed %v), want it listed with no wait: its claim runs a fresh session",
			building, worker, listed)
	}
	// Both rows still have no end, by claim token: the implementer's shows on no claim because it
	// does not stand, not because its row ended.
	var unended []string
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		rows, err := records.StandingWaits(ctx, tx, project)
		for _, row := range rows {
			unended = append(unended, row.ID)
		}
		return err
	}); err != nil {
		t.Fatalf("read the waits with no end: %v", err)
	}
	if want := []string{standing.ID, stale.ID}; !reflect.DeepEqual(unended, want) {
		t.Fatalf("the waits with no end are %v, want %v", unended, want)
	}
}

package api

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The P0 bound routes are the controller's set_p0_bound and clear_p0_bound, the routes the `legion`
// tool calls (C3 of dispatch://INFRA-5636): each change is applied through the daemon's own
// admission, in force from its own pass on with no restart, and answered with the bound the state
// then reads. legion.yaml's bound here is 3.

// newP0BoundHarness is a daemon of the harness's project whose handler chain is the real workflow
// engine and admission, with legion.yaml's P0 bound of configured.
func newP0BoundHarness(t *testing.T, configured *int) *harness {
	t.Helper()
	h := newHarness(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testDispatchProject}, quiet)
	admission := admit.New(record.NewStore(), engine, 2, testDispatchProject, quiet)
	admission.UseP0Bound(configured)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken, Controller: h.store, Grants: credential.New(nil), Log: quiet,
		Pool: h.store.Pool(), Record: record.NewStore(), Handlers: []intake.Handler{engine, admission},
		P0BoundConfigured: configured,
	}).Handler
	return h
}

// p0BoundAnswer sends a change and requires a 200, answering the bound it left in force.
func p0BoundAnswer(t *testing.T, h *harness, target string, body any, header http.Header) P0BoundView {
	t.Helper()
	recorder := h.request(http.MethodPost, target, body, header)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200; body %s", target, recorder.Code, recorder.Body)
	}
	var view P0BoundView
	decodeInto(t, recorder, &view)
	return view
}

// boundIs says whether bound is n, or none for n below 0.
func boundIs(bound *int, n int) bool {
	if n < 0 {
		return bound == nil
	}
	return bound != nil && *bound == n
}

func TestTheControllersP0BoundRoutesSetAndClearTheBoundTheNextPassReads(t *testing.T) {
	h := newP0BoundHarness(t, new(3))
	grant := h.controllerSession()

	set := p0BoundAnswer(t, h, p0BoundRoute, ProjectP0BoundRequest{GrantID: grant, Bound: new(1), Reason: "runaway"}, nil)
	if !boundIs(set.Bound, 1) || !boundIs(set.Configured, 3) || set.Live == nil || !boundIs(set.Live.Bound, 1) ||
		set.Live.SetBy != (PauseActorView{Kind: "controller", Session: controllerSession}) || set.Live.Reason != "runaway" {
		t.Fatalf("the set answered %+v (live %+v), want bound 1, legion.yaml's 3, and the live 1 by the controller's session", set, set.Live)
	}
	// The pass of the change's own fact admits under it: the store's newest change is the set, and
	// the reading every pass makes gives 1.
	inForce := func() *int {
		var bound *int
		if err := store.TransactWith(context.Background(), h.store.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			newest, found, err := store.P0BoundTx(context.Background(), tx, testDispatchProject)
			bound = store.EffectiveP0Bound(new(3), newest, found)
			return err
		}); err != nil {
			t.Fatalf("read the bound in force: %v", err)
		}
		return bound
	}
	if got := inForce(); !boundIs(got, 1) {
		t.Fatalf("the bound in force after the set = %v, want 1", got)
	}

	none := p0BoundAnswer(t, h, p0BoundRoute, ProjectP0BoundRequest{GrantID: grant, Unbounded: true, Reason: "start them all"}, nil)
	if none.Bound != nil || none.Live == nil || none.Live.Bound != nil || inForce() != nil {
		t.Fatalf("the set of none answered %+v (live %+v), want no bound in force whatever legion.yaml says", none, none.Live)
	}

	cleared := p0BoundAnswer(t, h, p0BoundClearRoute, ProjectP0BoundClearRequest{GrantID: grant, Reason: "back to the file"}, nil)
	if !boundIs(cleared.Bound, 3) || cleared.Live != nil || !boundIs(inForce(), 3) {
		t.Fatalf("the clear answered %+v, want legion.yaml's 3 in force and no live value", cleared)
	}

	for _, tc := range []struct {
		name string
		body any
		code string
	}{
		{name: "neither a bound nor none", body: ProjectP0BoundRequest{GrantID: grant, Reason: "x"}, code: invalidP0Bound},
		{name: "both", body: ProjectP0BoundRequest{GrantID: grant, Bound: new(2), Unbounded: true, Reason: "x"}, code: invalidP0Bound},
		{name: "a bound of 0", body: ProjectP0BoundRequest{GrantID: grant, Bound: new(0), Reason: "x"}, code: invalidP0Bound},
		{name: "a bound past what the store holds", body: ProjectP0BoundRequest{GrantID: grant, Bound: new(math.MaxInt32 + 1), Reason: "x"}, code: invalidP0Bound},
		{name: "no reason", body: ProjectP0BoundRequest{GrantID: grant, Bound: new(2), Reason: " "}, code: p0BoundReasonRequired},
	} {
		refusalServed(t, h.request(http.MethodPost, p0BoundRoute, tc.body, nil), p0BoundRoute, http.StatusBadRequest, tc.code)
	}
	if got := inForce(); !boundIs(got, 3) {
		t.Fatalf("the bound in force after the refused sets = %v, want legion.yaml's 3: a refusal changes nothing", got)
	}
}

// The controller's set and clear answer the bodies the TypeScript client parses: each is written
// from what the real route answers, a set of 2 over legion.yaml's 3, a set of none after it, and
// the clear after that, so the contract's schema and the plugin's tool read the daemon's own bytes.
func TestP0BoundRouteGoldens(t *testing.T) {
	h := newP0BoundHarness(t, new(3))
	grant := h.controllerSession()
	set := p0BoundAnswer(t, h, p0BoundRoute, ProjectP0BoundRequest{GrantID: grant, Bound: new(2), Reason: "the P0 issues outran the gateway"}, nil)
	set.Live.SetAt = time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	golden(t, "p0-bound.json", set)
	none := p0BoundAnswer(t, h, p0BoundRoute, ProjectP0BoundRequest{GrantID: grant, Unbounded: true, Reason: "the P0 issues outran the gateway"}, nil)
	none.Live.SetAt = time.Date(2026, 11, 24, 6, 5, 0, 0, time.UTC)
	golden(t, "p0-bound-none.json", none)
	golden(t, "p0-bound-clear.json", p0BoundAnswer(t, h, p0BoundClearRoute, ProjectP0BoundClearRequest{GrantID: grant, Reason: "back to the file"}, nil))
}

// The operator's route sets the bound under the operator's bearer, as `legion project set-p0-bound`
// sends it, and the change is the operator's.
func TestTheOperatorsP0BoundRouteSetsTheBoundAsTheOperator(t *testing.T) {
	h := newP0BoundHarness(t, nil)
	set := p0BoundAnswer(t, h, operatorP0BoundRoute, OperatorP0BoundRequest{Bound: new(2), Reason: "runaway"}, bearer)
	if !boundIs(set.Bound, 2) || set.Configured != nil || set.Live == nil || set.Live.SetBy != (PauseActorView{Kind: "operator"}) {
		t.Fatalf("the operator's set answered %+v (live %+v), want 2 in force by the operator, none in legion.yaml", set, set.Live)
	}
}

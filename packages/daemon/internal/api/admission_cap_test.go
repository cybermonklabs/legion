package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// capStart and capCeiling are the deployment's starting cap (admission_cap) and ceiling
// (admission_ceiling) these tests run under: a cap set above the start and up to the ceiling is in
// force, and a clear returns to the start.
const (
	capStart   = 2
	capCeiling = 3
)

// capChangedAt is the clock of the cap harness's server, the moment every change is made at.
var capChangedAt = time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)

// The four routes of the cap.
const (
	capRoute              = "/legion/v1/project/admission-cap"
	capClearRoute         = "/legion/v1/project/admission-cap/clear"
	operatorCapRoute      = "/legion/v1/operator/project/admission-cap"
	operatorCapClearRoute = "/legion/v1/operator/project/admission-cap/clear"
)

// capHarness is the cap routes over the daemon's own handler chain (daemon/workflow.go: the mirror,
// the engine, admission) under a starting cap of capStart and a ceiling of capCeiling, with
// admission on the harness's clock, and four roots that wait in todo, so a cap's effect on
// admission reads in the slots each request leaves.
func capHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(records, workflow.Config{Project: testDispatchProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quiet)
	admission := admit.New(records, engine, capStart, testDispatchProject, quiet)
	admission.UseCeiling(capCeiling)
	admission.UseClock(func() time.Time { return capChangedAt })
	h.handler = NewServer("127.0.0.1", 8437, capOptions(h, []intake.Handler{blockers.NewMirror(records, testDispatchProject), engine,
		admission})).Handler
	for _, key := range []string{"LEGION-901", "LEGION-902", "LEGION-903", "LEGION-904"} {
		h.recordIssue(record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testDispatchProject,
			Title: key, Tree: key, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: key[len(key)-1:], HandedOver: true})
	}
	return h
}

// capOptions is the daemon a cap harness serves: its store, machines and boot tokens, handlers, and
// the starting cap and the ceiling.
func capOptions(h *harness, handlers []intake.Handler) Options {
	return Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken, Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: record.NewStore(),
		Handlers: handlers, AdmissionCap: capStart, AdmissionCeiling: capCeiling, Now: func() time.Time { return capChangedAt },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// capOK sends a change of the cap and requires a 200, answering the cap it reports.
func capOK(t *testing.T, h *harness, target string, body any, header http.Header) AdmissionCapResponse {
	t.Helper()
	recorder := h.request(http.MethodPost, target, body, header)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200; body %s", target, recorder.Code, recorder.Body)
	}
	var answer AdmissionCapResponse
	decodeInto(t, recorder, &answer)
	return answer
}

// capRefused requires recorder to be a refusal of status with code, and answers it.
func capRefused(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) Failure {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d %s; body %s", recorder.Code, status, code, recorder.Body)
	}
	var refusal Failure
	decodeInto(t, recorder, &refusal)
	if refusal.Code != code {
		t.Fatalf("code = %s (%s), want %s", refusal.Code, refusal.Error, code)
	}
	return refusal
}

// capChangeViews is the state's `admission.capChanges` as the harness's store holds it.
func capChangeViews(t *testing.T, h *harness) []AdmissionCapChangeView {
	t.Helper()
	var views []AdmissionCapChangeView
	if err := store.Transact(h.ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		views, err = admissionCapChangesTx(ctx, tx, testDispatchProject)
		return err
	}); err != nil {
		t.Fatalf("read the cap changes: %v", err)
	}
	return views
}

// slotted counts the issues holding a slot.
func slotted(t *testing.T, h *harness) int {
	t.Helper()
	var slots []record.Slot
	if err := store.Transact(h.ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		slots, err = record.NewStore().Slots(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("read the slots: %v", err)
	}
	return len(slots)
}

// controllerSessionGrantID registers ses_controller as the project's controller and answers a grant
// that session minted.
func controllerSessionGrantID(t *testing.T, h *harness) string {
	t.Helper()
	registration := h.registeredController(h.mintedSecret(), "ses_controller")
	recorder := h.controllerSessionGrant("ses_controller", registration.Secret)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the controller session's grant = %d; body %s", recorder.Code, recorder.Body)
	}
	var grant GrantResponse
	decodeInto(t, recorder, &grant)
	return grant.GrantID
}

// TestAdmissionCapRouteGoldens writes the controller's two answers of the cap as the routes serve
// them, over the handler chain and a database of their own: a set of 2 on a person's word, and the
// clear after it. `packages/pi-legion/src/tools.test.ts` reads both through the plugin's client.
func TestAdmissionCapRouteGoldens(t *testing.T) {
	h := capHarness(t)
	grant := controllerSessionGrantID(t, h)
	set := capOK(t, h, capRoute, ProjectAdmissionCapRequest{GrantID: grant, Cap: 2, Reason: "the merge queue waits 52 minutes"}, nil)
	golden(t, "admission-cap.json", set)
	grant = controllerSessionGrantID(t, h)
	cleared := capOK(t, h, capClearRoute, ProjectAdmissionCapClearRequest{GrantID: grant, Reason: "the signals are back under their limits"}, nil)
	golden(t, "admission-cap-clear.json", cleared)
}

// TestAdmissionCapRoutes is the API half of dispatch://INFRA-5616: the four routes of the cap over
// the daemon's handler chain and a database of their own.
func TestAdmissionCapRoutes(t *testing.T) {
	controller := PauseActorView{Kind: "controller", Session: "ses_controller"}
	operator := PauseActorView{Kind: "operator"}

	// The controller's grant sets the cap, and the request's own admission pass admits under it:
	// a cap of 1 starts one of the four waiting roots, and a raise to the ceiling of 3, above the
	// starting cap of 2, two more, each answered with the cap in force, the ceiling and the cap
	// that stands. A clear answers the starting cap and no cap set. Every change is recorded, newest
	// first, with the caps before and after it, who, when and why.
	t.Run("the controller's grant sets and clears the cap, in force on the request's own pass", func(t *testing.T) {
		h := capHarness(t)
		grant := controllerSessionGrantID(t, h)

		set := capOK(t, h, capRoute, ProjectAdmissionCapRequest{GrantID: grant, Cap: 1, Reason: "the merge queue waits 52 minutes"}, nil)
		want := AdmissionCapOverride{Cap: 1, SetBy: controller, SetAt: capChangedAt, Reason: "the merge queue waits 52 minutes"}
		if set.Cap != 1 || set.Ceiling != capCeiling || set.Override == nil || *set.Override != want {
			t.Fatalf("the set answered %+v (override %+v), want cap 1 of the ceiling %d set as %+v", set, set.Override, capCeiling, want)
		}
		if n := slotted(t, h); n != 1 {
			t.Fatalf("slots after a cap of 1 = %d, want 1", n)
		}
		grant = controllerSessionGrantID(t, h)
		capOK(t, h, capRoute, ProjectAdmissionCapRequest{GrantID: grant, Cap: capCeiling, Reason: "the signals are back under their limits"}, nil)
		if n := slotted(t, h); n != capCeiling {
			t.Fatalf("slots after a raise to the ceiling of %d, above the starting cap of %d = %d, want %d", capCeiling, capStart, n, capCeiling)
		}

		grant = controllerSessionGrantID(t, h)
		cleared := capOK(t, h, capClearRoute, ProjectAdmissionCapClearRequest{GrantID: grant, Reason: "a person asked for the deployment's cap"}, nil)
		if cleared.Cap != capStart || cleared.Ceiling != capCeiling || cleared.Override != nil {
			t.Fatalf("the clear answered %+v, want the starting cap %d under the ceiling %d and no cap set", cleared, capStart, capCeiling)
		}
		if n := slotted(t, h); n != capCeiling {
			t.Fatalf("slots after the clear = %d, want the %d that run: a lower cap stops nothing", n, capCeiling)
		}

		changes := capChangeViews(t, h)
		if len(changes) != 3 {
			t.Fatalf("the changes = %+v, want three", changes)
		}
		clear, raise, first := changes[0], changes[1], changes[2]
		if clear.Cap != 0 || clear.CapBefore != capCeiling || clear.CapAfter != capStart ||
			clear.SetBy != controller || clear.Reason != "a person asked for the deployment's cap" {
			t.Errorf("the clear = %+v, want no cap, from %d to the starting cap %d, by the controller", clear, capCeiling, capStart)
		}
		if raise.Cap != capCeiling || raise.CapBefore != 1 || raise.CapAfter != capCeiling || raise.Reason != "the signals are back under their limits" {
			t.Errorf("the raise = %+v, want %d from 1, for its reason", raise, capCeiling)
		}
		if first.Cap != 1 || first.CapBefore != capStart || first.CapAfter != 1 || first.Ceiling != capCeiling ||
			first.SetBy != controller || !first.SetAt.Equal(capChangedAt) || first.ID == 0 || first.ID >= raise.ID || raise.ID >= clear.ID {
			t.Errorf("the first set = %+v, want 1 from %d by the controller at %s, the lowest id", first, capStart, capChangedAt)
		}
	})

	// The operator's routes record the operator; a grant the operator's token minted records the
	// operator too. A field no route takes, such as a `decisionId`, is refused 400.
	t.Run("the operator's routes and the operator's grant record the operator", func(t *testing.T) {
		h := capHarness(t)
		if set := capOK(t, h, operatorCapRoute, OperatorAdmissionCapRequest{Cap: 2, Reason: "a pool is down"}, bearer); set.Override == nil ||
			set.Override.SetBy != operator {
			t.Fatalf("the operator's set answered %+v, want the cap set by the operator", set.Override)
		}
		decided := h.request(http.MethodPost, operatorCapRoute, `{"cap":2,"reason":"a pool is down","decisionId":"admission:LEGION:2026-11-24T06:00:00Z"}`, bearer)
		if decided.Code != http.StatusBadRequest {
			t.Fatalf("an operator's set naming a decisionId = %d, want 400: no route takes one; body %s", decided.Code, decided.Body)
		}
		grant := controllerGrant(t, h).GrantID
		if set := capOK(t, h, capRoute, ProjectAdmissionCapRequest{GrantID: grant, Cap: 1, Reason: "an operator's grant"}, nil); set.Override == nil ||
			set.Override.SetBy != operator {
			t.Fatalf("a set through the operator's grant answered %+v, want the operator's", set.Override)
		}
		if cleared := capOK(t, h, operatorCapClearRoute, OperatorAdmissionCapClearRequest{Reason: "the pool is back"}, bearer); cleared.Override != nil {
			t.Fatalf("the operator's clear answered %+v, want no cap set", cleared)
		}
		if changes := capChangeViews(t, h); len(changes) != 3 || changes[0].SetBy != operator || changes[0].Cap != 0 ||
			changes[1].SetBy != operator || changes[2].SetBy != operator {
			t.Fatalf("the changes = %+v, want three by the operator, the clear newest", changes)
		}
	})

	// Each refusal changes nothing: a cap below 1, zero included, a cap above the ceiling, whose
	// sentence names the ceiling and admission_ceiling, a blank reason, a reason one character over
	// the bound, a body naming a field the route does not take, a phase worker's grant, whose
	// sentence says what the change requires, and an operator route with no bearer or a wrong one.
	// The control is the set at the ceiling after them, which is recorded.
	t.Run("the refusals change nothing", func(t *testing.T) {
		h := capHarness(t)
		grant := controllerSessionGrantID(t, h)
		for _, below := range []int{0, -1} {
			capRefused(t, h.request(http.MethodPost, capRoute, ProjectAdmissionCapRequest{GrantID: grant, Cap: below, Reason: "why"}, nil),
				http.StatusBadRequest, invalidAdmissionCap)
			capRefused(t, h.request(http.MethodPost, operatorCapRoute, OperatorAdmissionCapRequest{Cap: below, Reason: "why"}, bearer),
				http.StatusBadRequest, invalidAdmissionCap)
		}
		above := capRefused(t, h.request(http.MethodPost, capRoute, ProjectAdmissionCapRequest{GrantID: grant, Cap: capCeiling + 1, Reason: "why"}, nil),
			http.StatusConflict, admissionCapAboveCeiling)
		if !strings.Contains(above.Error, "ceiling of 3") || !strings.Contains(above.Error, "admission_ceiling") {
			t.Errorf("the refusal above the ceiling says %q, want it to name the ceiling of 3 and admission_ceiling", above.Error)
		}
		capRefused(t, h.request(http.MethodPost, operatorCapRoute, OperatorAdmissionCapRequest{Cap: capCeiling + 1, Reason: "why"}, bearer),
			http.StatusConflict, admissionCapAboveCeiling)
		capRefused(t, h.request(http.MethodPost, capRoute, ProjectAdmissionCapRequest{GrantID: grant, Cap: 2, Reason: " \n"}, nil),
			http.StatusBadRequest, admissionCapReasonRequired)
		capRefused(t, h.request(http.MethodPost, capClearRoute, ProjectAdmissionCapClearRequest{GrantID: grant}, nil),
			http.StatusBadRequest, admissionCapReasonRequired)
		capRefused(t, h.request(http.MethodPost, operatorCapClearRoute, OperatorAdmissionCapClearRequest{}, bearer),
			http.StatusBadRequest, admissionCapReasonRequired)
		long := strings.Repeat("é", store.AdmissionCapReasonMax+1)
		tooLong := capRefused(t, h.request(http.MethodPost, capRoute, ProjectAdmissionCapRequest{GrantID: grant, Cap: 2, Reason: long}, nil),
			http.StatusBadRequest, admissionCapReasonTooLong)
		if !strings.Contains(tooLong.Error, "at most 2048") {
			t.Errorf("the refusal of a long reason says %q, want it to name the bound of 2048", tooLong.Error)
		}
		if decided := h.request(http.MethodPost, capRoute, `{"grantId":"`+grant+`","cap":2,"reason":"why","decisionId":"admission:LEGION:2026-11-24T06:00:00Z"}`, nil); decided.Code != http.StatusBadRequest {
			t.Fatalf("a set naming a decisionId = %d, want 400: the route takes none; body %s", decided.Code, decided.Body)
		}
		if named := h.request(http.MethodPost, capRoute, `{"grantId":"`+grant+`","cap":2,"reason":"why","project":"LEGION"}`, nil); named.Code != http.StatusBadRequest {
			t.Fatalf("a set naming a project = %d, want 400: the route takes none; body %s", named.Code, named.Body)
		}
		worker := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
		refusal := capRefused(t, h.request(http.MethodPost, capRoute, ProjectAdmissionCapRequest{GrantID: worker.grant(t), Cap: 2, Reason: "a worker's cap"}, nil),
			http.StatusForbidden, "CONTROLLER_REQUIRED")
		if !strings.Contains(refusal.Error, "requires a controller grant") {
			t.Errorf("a worker's grant is refused %q, want a sentence saying the change requires a controller grant", refusal.Error)
		}
		for _, target := range []string{operatorCapRoute, operatorCapClearRoute} {
			for _, header := range []http.Header{nil, {"Authorization": {"Bearer not-the-operator"}}} {
				if recorder := h.request(http.MethodPost, target, OperatorAdmissionCapRequest{Cap: 2, Reason: "x"}, header); recorder.Code != http.StatusForbidden {
					t.Fatalf("POST %s with no operator's bearer = %d, want 403; body %s", target, recorder.Code, recorder.Body)
				}
			}
		}
		if changes := capChangeViews(t, h); len(changes) != 0 {
			t.Fatalf("the refused calls recorded %+v, want no change", changes)
		}
		if n := slotted(t, h); n != 0 {
			t.Fatalf("slots after the refusals = %d, want none: a refusal applies no fact and so runs no pass", n)
		}
		capOK(t, h, capRoute, ProjectAdmissionCapRequest{GrantID: grant, Cap: capCeiling, Reason: "the control: a set at the ceiling"}, nil)
		if changes := capChangeViews(t, h); len(changes) != 1 || changes[0].Cap != capCeiling {
			t.Fatalf("the control's set recorded %+v, want its one change to the ceiling", changes)
		}
		if n := slotted(t, h); n != capCeiling {
			t.Fatalf("slots after the control's set = %d, want the ceiling's %d", n, capCeiling)
		}
	})

	// A fresh daemon's state at this contract carries `"capChanges":[]`, never no key
	// (dispatch://INFRA-5616/spec#b-r7-tick-log), and a caller below the contract is served none.
	// The control is that caller's body, which a band that never hid the key fails.
	t.Run("a fresh daemon's state carries an empty list of changes", func(t *testing.T) {
		var fresh Admission
		body, err := json.Marshal(fresh)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"capChanges":[]`) {
			t.Fatalf("a fresh admission at contract %d = %s, want `\"capChanges\":[]`", admissionCapContract, body)
		}
		older, _ := withoutAdmissionCap(State{Admission: fresh})
		body, err = json.Marshal(older.Admission)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), `"capChanges"`) {
			t.Fatalf("the admission served below contract %d = %s, want no `capChanges`", admissionCapContract, body)
		}
	})

	// A daemon with no workflow admits nothing and refuses every change.
	t.Run("a daemon with no workflow refuses a change", func(t *testing.T) {
		h := newHarness(t)
		h.handler = NewServer("127.0.0.1", 8437, capOptions(h, nil)).Handler
		capRefused(t, h.request(http.MethodPost, operatorCapRoute, OperatorAdmissionCapRequest{Cap: 2, Reason: "why"}, bearer),
			http.StatusConflict, admissionCapUnavailable)
		capRefused(t, h.request(http.MethodPost, operatorCapClearRoute, OperatorAdmissionCapClearRequest{Reason: "why"}, bearer),
			http.StatusConflict, admissionCapUnavailable)
	})
}

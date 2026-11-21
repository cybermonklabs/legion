package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// /healthz answers what the decisions step's last pass found owed, `decisions_step_owed`, as one
// number, and nothing before a tick has counted: a step that has not run - as
// DISPATCH_TEST_HOLD_DECISIONS_STEP keeps a server's from running at boot and on its tick -
// answers no member at all, and neither does a step whose boot pass alone has run, since the boot
// counts nothing (docs.DecisionStep.Boot: the count reads tables the boot must not wait on). Once
// the first tick has run over a database with nothing owed, the member reads 0, and once a project
// stores a list of gated steps that holds `decisions`, the next tick reads 1. Each count logs the
// three parts beside the total, which is where an operator reads which part is owed.
func TestHealthzAnswersWhatTheDecisionsStepOwes(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	var logged bytes.Buffer
	step := docs.NewDecisionStep(database, "c0ffee", slog.New(slog.NewTextHandler(&logged, nil)))
	handler := dispatchHandler(http.NewServeMux(), metrics.New(), database, nil, nil, nil, "", nil, step, issuers{}, nil)
	probe := func() map[string]any {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var health map[string]any
		if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
			t.Fatalf("decode /healthz: %v", err)
		}
		return health
	}
	owed := func(stage string) float64 {
		t.Helper()
		health := probe()
		member, ok := health["decisions_step_owed"].(float64)
		if !ok {
			t.Fatalf("%s: /healthz answers decisions_step_owed %#v, want one number, the total the step's last pass counted", stage, health["decisions_step_owed"])
		}
		t.Logf("%s: decisions_step_owed %v", stage, member)
		return member
	}
	countLine := func(stage, want string) {
		t.Helper()
		for _, line := range strings.Split(logged.String(), "\n") {
			if strings.Contains(line, `msg="dispatch: decisions step counted what it owes"`) && strings.Contains(line, want) {
				return
			}
		}
		t.Errorf("%s: the step logged no count holding %q:\n%s", stage, want, logged.String())
	}

	if member, present := probe()["decisions_step_owed"]; present {
		t.Fatalf("/healthz answers decisions_step_owed %v before the step has run a pass, want the member absent", member)
	}

	if err := step.Boot(ctx); err != nil {
		t.Fatalf("run the step's boot pass: %v", err)
	}
	if member, present := probe()["decisions_step_owed"]; present {
		t.Fatalf("/healthz answers decisions_step_owed %v after the step's boot pass, which counts nothing, want the member absent until its first tick", member)
	}
	if err := step.Tick(ctx); err != nil {
		t.Fatalf("run the step's first tick: %v", err)
	}
	if empty := owed("after a tick over a database with nothing owed"); empty != 0 {
		t.Errorf("after a tick over a database with nothing owed, decisions_step_owed = %v, want 0", empty)
	}
	countLine("after the first tick", "owed=0 decisions_documents=0 answers_left_spec=0 gated_steps_lists=0 commit=c0ffee")

	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, gated_steps) values ('GATE', 'Gated', 'alice', '{decisions,spec}')
	`); err != nil {
		t.Fatalf("store a list of gated steps holding decisions: %v", err)
	}
	if err := step.Tick(ctx); err != nil {
		t.Fatalf("run a tick of the step: %v", err)
	}
	if gated := owed("after a pass over one list of gated steps holding decisions"); gated != 1 {
		t.Errorf("decisions_step_owed = %v, want 1, the one list of gated steps holding decisions", gated)
	}
	countLine("after the gated list", "owed=1 decisions_documents=0 answers_left_spec=0 gated_steps_lists=1 commit=c0ffee")
}

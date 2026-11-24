package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// failedOwner is the harness with the operator's server applying facts to the daemon's own
// workflow engine, and LEGION-208's architect, the owner of its task, failed: the runtime refused
// every launch of it. at seeds the issue the claim owns.
func failedOwner(t *testing.T, at func(h *harness)) *harness {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Pool: h.store.Pool(), Record: records, Releaser: h.runtime, Trees: h.store, Handlers: []intake.Handler{engine},
	}).Handler
	refused := fake.SpawnResult{Err: errors.New("the runtime refused the launch")}
	h.runtime.ScriptSpawn(refused, refused, refused)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()); recorder.Code != http.StatusInternalServerError {
		t.Fatalf("spawn of an architect whose every launch the runtime refused = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
	if got := h.stored(architectToken).State; got != supervise.StateFailed {
		t.Fatalf("the owner is %s, want failed", got)
	}
	at(h)
	return h
}

// ownerStepsOf counts the owner-step notices issue's outbox holds.
func ownerStepsOf(t *testing.T, h *harness, issue string) int {
	t.Helper()
	var rows int
	if err := h.store.Pool().QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'notice' and payload->>'kind' = $2`,
		issue, string(record.OwnerStepNotice)).Scan(&rows); err != nil {
		t.Fatalf("count the owner-steps of %s: %v", issue, err)
	}
	return rows
}

// The operator's retry of the owner's failed claim while its issue stands at a step of its own
// (`tester:owner`) relaunches the claim and tells the owner its step again: one more owner-step,
// none being queued before it, with the retry's task, and 202, as a retry that restarts a phase
// answers.
//
// The control is the owner's retry where its issue stands at admitted, which is no step of its
// own: the claim is relaunched through its machine alone, 200, and no owner-step is queued.
func TestTheOperatorsRetryOfAFailedOwnerAtItsStepTellsItTheStepAgain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		at         func(h *harness)
		status     int
		ownerSteps int
	}{
		{name: "at its own step tester:owner", at: func(h *harness) {
			seedIssueAt(t, h, "LEGION-208", phase.Testing)
			seedOwnerPlan(t, h, "LEGION-208", "tester:owner", stepAt(1))
		}, status: http.StatusAccepted, ownerSteps: 1},
		{name: "at admitted", at: func(h *harness) {
			seedIssueAt(t, h, "LEGION-208", phase.Admitted)
		}, status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := failedOwner(t, tc.at)
			spawns := len(h.runtime.CallsOf("Spawn"))

			recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/retry", nil)

			if recorder.Code != tc.status {
				t.Fatalf("retry of the failed owner = %d, want %d; body %s", recorder.Code, tc.status, recorder.Body)
			}
			if got := ownerStepsOf(t, h, "LEGION-208"); got != tc.ownerSteps {
				t.Fatalf("the retry queued %d owner-steps, want %d", got, tc.ownerSteps)
			}
			if got := len(h.runtime.CallsOf("Spawn")); got != spawns+1 {
				t.Fatalf("the retry made %d spawns, want one more than the %d before it: the owner is relaunched", got, spawns)
			}
		})
	}
}

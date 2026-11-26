package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// sharedCreditPlan puts delivered, unheld task steps on the child's existing run.
func sharedCreditPlan(t *testing.T, r *toldRig, agents int) {
	t.Helper()
	tasks := make([]record.PlanTask, 0, agents)
	steps := make([]record.PlanStep, 0, agents)
	for n := range agents {
		id, agent := fmt.Sprintf("t%d", n), fmt.Sprintf("checker%d", n)
		if n == 0 {
			agent = "reviewer"
		}
		tasks = append(tasks, record.PlanTask{Issue: toldChild, Generation: 1, Task: id, PlanVersion: 1,
			BlockText: "Check the change.", State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 1}})
		steps = append(steps, record.PlanStep{Issue: toldChild, Generation: 1, Task: id, Position: 1,
			Role: agent, Type: "review", Agent: claim.Agent(agent)})
	}
	if err := Transact(r.ctx, r.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutPlan(ctx, tx, record.PlanVersion{Issue: toldChild, Generation: 1, Version: 1,
			ArtifactID: "artifact-shared-credit-plan", Source: record.PlanSourceOwner, GivenBy: r.reviewer.Token, GivenAt: r.at}, tasks, steps)
	}); err != nil {
		t.Fatal(err)
	}
}

// No warning is produced by the single-agent control; two live task agents must
// name the one this confirmed delivery did not address. A rollback has no credit.
func TestSharedNewsCreditWarnsOnlyForOtherLiveTaskAgents(t *testing.T) {
	for _, tc := range []struct {
		name      string
		agents    int
		confirmed bool
		rollback  bool
		wantWarn  bool
	}{
		{"one live task agent", 1, true, false, false},
		{"two live task agents", 2, true, false, true},
		{"unconfirmed delivery", 2, false, false, false},
		{"rolled back confirmation", 2, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newToldRig(t)
			sharedCreditPlan(t, r, tc.agents)
			logged := captureLog(t, r.store.Pool())
			delivery := r.newsTask("shared-credit")
			if tc.confirmed {
				delivery.ConfirmedAt = r.at.Add(time.Second)
			}
			if tc.rollback {
				rollback := errors.New("roll back the credited delivery")
				err := Transact(r.ctx, r.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
					if err := r.store.PutClaimAndDeliveryTx(ctx, tx, r.reviewer, delivery); err != nil {
						return err
					}
					return rollback
				})
				if !errors.Is(err, rollback) {
					t.Fatal(err)
				}
			} else if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, delivery); err != nil {
				t.Fatal(err)
			}
			text := logged.String()
			if !tc.wantWarn {
				if strings.Contains(text, "not recipients of this delivery") {
					t.Fatalf("unexpected shared-credit warning: %s", text)
				}
				return
			}
			for _, want := range []string{"level=WARN", "not recipients of this delivery", toldChild, "version=5", "told_agent=reviewer", "checker1"} {
				if !strings.Contains(text, want) {
					t.Fatalf("confirmed shared news credit log %q does not name %q", text, want)
				}
			}
			logged.Reset()
			if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, delivery); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(logged.String(), "not recipients of this delivery") {
				t.Fatalf("the same confirmation warned twice: %s", logged.String())
			}
		})
	}
}

// The query under measurement is the existing production running-step reader,
// not an independently copied implementation of the planned warning.
func TestSharedNewsCreditRunningReadCost(t *testing.T) {
	r := newToldRig(t)
	const agents = 10000
	sharedCreditPlan(t, r, agents)
	started := time.Now()
	var raw []byte
	if err := r.store.Pool().QueryRow(r.ctx, "select "+pods.RunningColumn("issues.key", "issues.generation")+
		" from issues where key = $1", toldChild).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	rows, planned, err := pods.DecodeRunning(raw)
	elapsed := time.Since(started)
	if err != nil || !planned || len(rows) != agents {
		t.Fatalf("running-step cost probe saw %d rows, planned=%t, error=%v; want %d real rows", len(rows), planned, err, agents)
	}
	t.Logf("SHARED_NEWS_CREDIT_RUNNING_READ agents=%d bytes=%d query_and_decode=%s", agents, len(raw), elapsed)
	logged := captureLog(t)
	delivery := r.newsTask("busy-shared-credit")
	delivery.ConfirmedAt = r.at.Add(time.Second)
	started = time.Now()
	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, delivery); err != nil {
		t.Fatal(err)
	}
	elapsed = time.Since(started)
	if !strings.Contains(logged.String(), "checker9999") || !strings.Contains(logged.String(), "not recipients of this delivery") {
		t.Fatal("the busiest confirmed credit did not name every other running agent")
	}
	t.Logf("SHARED_NEWS_CREDIT_FULL_WRITE agents=%d warning_bytes=%d confirmed_write_and_log=%s", agents, logged.Len(), elapsed)
}

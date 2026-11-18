package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// Daemons of several projects share one database, and admission is each project's own. Another
// project's slots count against none of this daemon's cap, its waiting root is not this daemon's
// to promote, its stale slot is not this daemon's to release, and its outbox row, due before any of
// this daemon's, is not this daemon's to run: none of its issues is this daemon's to touch in
// Dispatch, and none of them ever gets a branch created on GitHub (refs/heads/legion/<key>) by this
// daemon.
func TestAnotherProjectsIssuesAreNotThisDaemonsToAdmit(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.AdmissionCap = 2
	other := "OTHER" + randomSuffix(t)
	running, stale, waiting := other+"-1", other+"-2", other+"-3"
	own := cfg.Project + "-1"

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the shared store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the shared store: %v", err)
	}
	records := record.NewStore()
	now := time.Now().UTC()
	// A slot's index is unique across every project's slots, so the seeded ones take indices above
	// any the shared database already holds.
	var base int
	root := func(key, status, rank string) record.Issue {
		return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key, Project: other, Title: "Another project's root", Phase: phase.Admitted, Generation: 1, Status: status, Rank: rank}
	}
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		taken, err := records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		for _, slot := range taken {
			base = max(base, slot.Index+1)
		}
		for _, issue := range []record.Issue{root(running, "in_progress", "a"), root(stale, "backlog", "b"), root(waiting, "todo", "0")} {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		for index, key := range []string{running, stale} {
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: base + index, AdmittedAt: now}); err != nil {
				return err
			}
		}
		// Due a minute ago, so an outbox that claimed across projects would run it before any row
		// this daemon's admission enqueues.
		row, err := record.NewOutboxRow(running, record.OrdinaryStatusWrite("in_progress", "todo", ""), now.Add(-time.Minute))
		if err != nil {
			return err
		}
		return records.Enqueue(ctx, tx, row)
	}); err != nil {
		t.Fatalf("seed another project's admission: %v", err)
	}
	t.Cleanup(func() {
		if _, err := st.Pool().Exec(context.Background(), `delete from outbox where issue like $1`, other+"-%"); err != nil {
			t.Errorf("remove the seeded outbox row: %v", err)
		}
	})

	// ownRun closes when the outbox first reaches Dispatch for this project's issue: its first batch
	// after admission has run, and the other project's older row would have run before it. The
	// board is a dispatchBoard: a request without the daemon's bearer is another process's,
	// answered 404, and fails nothing.
	ownRun := make(chan struct{})
	var ownOnce sync.Once
	cfg.DispatchURL = dispatchBoard(t, func(w http.ResponseWriter, r *http.Request) {
		if servedBootCall(t, w, r, own[:strings.Index(own, "-")]) {
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/issues/"+own) {
			ownOnce.Do(func() { close(ownRun) })
		}
		if strings.Contains(r.URL.Path, other) {
			t.Errorf("the daemon asked Dispatch about another project's issue: %s %s", r.Method, r.URL.Path)
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues" {
			// This project's own writes after admission are not what this test is about.
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]map[string]any{{"key": own, "title": "This project's root", "status": "todo", "parent": nil, "rank": "m", "labels": []string{dispatch.LegionLabel}, "resolved_repo": "acme/widgets"}}); err != nil {
			t.Errorf("write Dispatch issues: %v", err)
		}
	})
	github := newBranchGitHub(t, nil, branchCreated)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	o := overrides{
		listen:  heldListen,
		runtime: fakeRuntime(fake.NewRuntime(), &built{}).runtime, clock: newStillClock(), workflowTokens: &workflowTokenRecorder{},
		githubAPI: github.url,
	}
	// Two observers, each read on its own: the boot's listing is reconciled before the daemon
	// serves, so its admission pass is reported before the daemon reports itself ready.
	obs, passes := observing(&o), observing(&o)
	go func() { done <- run(runCtx, cfg, quietLogger(), o) }()
	t.Cleanup(func() {
		cancel()
		if err := testwait.Receive(t, "the daemon's run to return once cancelled", done); err != nil {
			t.Errorf("run: %v", err)
		}
	})
	obs.ready(t, done)

	// Admission changes only inside a pass, and each pass is reported once it commits, so the
	// state read after each report says whether that pass, or one before it, left this project's
	// root alone in the line.
	passes.admissionPasses(t, "the pass that leaves "+own+" alone in admission's active line: another project's slots and waiting root are not this daemon's", 1,
		func(intake.Fact) bool { return slices.Equal(admissionActive(t, cfg), []string{own}) })

	testwait.Receive(t, "the outbox's first call to Dispatch for "+own+" after admitting it", ownRun)

	var slots []string
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		all, err := records.Slots(ctx, tx)
		for _, slot := range all {
			if strings.HasPrefix(slot.Issue, other+"-") {
				slots = append(slots, fmt.Sprintf("%s@%d", slot.Issue, slot.Index))
			}
		}
		return err
	}); err != nil {
		t.Fatalf("read the slots: %v", err)
	}
	if want := []string{fmt.Sprintf("%s@%d", running, base), fmt.Sprintf("%s@%d", stale, base+1)}; !reflect.DeepEqual(slots, want) {
		t.Errorf("another project's slots = %v, want %v, untouched", slots, want)
	}
	var foreign []branchCreate
	for _, create := range github.created() {
		if strings.HasPrefix(create.ref, "refs/heads/legion/"+other+"-") {
			foreign = append(foreign, create)
		}
	}
	if len(foreign) != 0 {
		t.Errorf("GitHub saw %d branch create(s) for another project's issues: %+v, want none", len(foreign), foreign)
	}
}

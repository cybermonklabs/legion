package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// `GET /legion/v1/state`, as the daemon's own source answers it over the record, shows the design
// gate's hold on each merger it holds, and that issue's record is what `read_record` reads: the
// gated step, the document registered for it and the version of it that is with its reviewer.
// The source judges the hold by the rule the outbox's fence reads (workflow.HeldByDesignGate), so
// the merger of the tree whose spec stands approved, and the implementer of the held issue, carry
// no hold, and once a person approves the version with the reviewer, no claim carries one.
func TestTheStateRouteShowsTheHoldOfEachMergerTheClosedGateHolds(t *testing.T) {
	cfg := testConfig(t)
	heldRoot, held, openRoot, open := cfg.Project+"-1", cfg.Project+"-2", cfg.Project+"-3", cfg.Project+"-4"
	merger := previousClaim(t, cfg, heldRoot, held, claim.RoleMerger, supervise.StateIdle)
	implementer := previousClaim(t, cfg, heldRoot, held, claim.RoleImplementer, supervise.StateIdle)
	openMerger := previousClaim(t, cfg, openRoot, open, claim.RoleMerger, supervise.StateIdle)
	child := func(key, root string) record.Issue {
		issue := runningRoot(key, phase.Merging)
		issue.Tree, issue.Parent = root, record.ParentOf(root)
		return issue
	}
	putPreviousRun(t, cfg, previousRun{
		issues: []record.Issue{runningRoot(heldRoot, phase.Implementing), child(held, heldRoot),
			runningRoot(openRoot, phase.Implementing), child(open, openRoot)},
		slots:  []string{heldRoot, openRoot},
		claims: []supervise.Claim{merger, implementer, openMerger},
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
	// The held tree's spec: a person approved version 2, and version 3 is with the reviewer. The
	// open tree's spec stands approved at the version Dispatch holds.
	gate := func(root, artifact string, latest, approved int) record.DesignGate {
		return record.DesignGate{Issue: root, Step: record.DesignStepSpec, ArtifactID: artifact,
			LatestVersion: latest, ApprovedVersion: &approved, EverOpened: true}
	}
	put := func(gates ...record.DesignGate) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
			for _, gate := range gates {
				if err := records.PutGate(ctx, tx, gate); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("put the gates: %v", err)
		}
	}
	put(gate(heldRoot, "artifact-held", 3, 2), gate(openRoot, "artifact-open", 4, 4))

	view := &source{
		store: st, records: records, project: cfg.Project, projectToken: project, runtime: "tmux", admissionCap: cfg.AdmissionCap,
		startedAt: time.Now(), deployment: func() capabilities.Deployment { return capabilities.Deployment{} },
		reports:    func() map[claim.Token]capabilities.Report { return nil },
		gatedSteps: config.DesignGateRootIssues.Steps(),
	}
	handler := api.NewServer("127.0.0.1", 0, api.Options{State: view, StateTransactions: st}).Handler
	read := func() api.State {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /legion/v1/state = %d %s, want 200", recorder.Code, recorder.Body)
		}
		var state api.State
		if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
			t.Fatalf("decode the state: %v", err)
		}
		return state
	}
	holdOf := func(state api.State, key string, role claim.Role) *api.HoldView {
		t.Helper()
		worker, listed := state.Issues[key].Workers[role]
		if !listed {
			t.Fatalf("%s's record lists no %s: %+v", key, role, state.Issues[key])
		}
		return worker.Claim.Hold
	}

	state := read()
	want := api.HoldView{Step: record.DesignStepSpec, ArtifactID: "artifact-held", Version: 3}
	if got := holdOf(state, held, claim.RoleMerger); got == nil || *got != want {
		t.Fatalf("%s's merger carries the hold %+v, want the closed gate's, %+v", held, got, want)
	}
	if got := holdOf(state, held, claim.RoleImplementer); got != nil {
		t.Errorf("%s's implementer carries the hold %+v, want none: the gate holds a merger alone", held, got)
	}
	if got := holdOf(state, open, claim.RoleMerger); got != nil {
		t.Errorf("%s's merger carries the hold %+v, want none: its tree's spec stands approved", open, got)
	}

	// A person approves version 3: the gate stands open, and the next read shows no hold at all.
	put(gate(heldRoot, "artifact-held", 3, 3))
	if got := holdOf(read(), held, claim.RoleMerger); got != nil {
		t.Fatalf("%s's merger carries the hold %+v after the approval, want none", held, got)
	}
}

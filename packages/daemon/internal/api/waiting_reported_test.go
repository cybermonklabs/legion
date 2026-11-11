// This file is the state half of TestAWaitingIssueIsReportedWaitingAndBlocked. It is an external
// test package because it reads the state the daemon itself serves: internal/projection builds
// that state and imports this package, so a file inside package api could not call it.
package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// waitProject is the Dispatch project every record of this file is filed under, and
	// waitRepo the repository they resolve to.
	waitProject = "LEGION"
	waitRepo    = "acme/widgets"
)

// waitNow is the instant this file's pass and projection are taken at.
var waitNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// waitRepositories resolves the one repository these records name, at main.
type waitRepositories struct{}

func (waitRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	if resolved == waitRepo {
		return workflow.Resolution{Repo: waitRepo, DefaultBranch: "main", GitHubRepo: waitRepo}, nil
	}
	return workflow.Resolution{Refusal: resolved + " is not mapped to project " + waitProject + " in Dispatch"}, nil
}

// projectedSource is the daemon's own state source (the daemon's `source`): the state route's
// repeatable-read snapshot is what internal/projection reads the records in, so the body served
// here is built exactly as a deployment's is.
type projectedSource struct {
	records record.Store
	steps   []record.DesignStep
}

func (s projectedSource) State(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	state, err := projectWaitingState(ctx, tx, s.records, s.steps)
	return state, nil, err
}

// waitingStore is a migrated database of this package's template, with the records store and the
// design-gate steps the deployment gates.
func waitingStore(t *testing.T) (*store.Store, record.Store, []record.DesignStep) {
	t.Helper()
	return testpg.CloneStore(t, "legion_api_test"), record.NewStore(), config.DesignGateRootIssues.Steps()
}

// seedWaitIssue records one issue of this file's fixture, and its tree lifecycle when it is a
// root the daemon has started: a lifecycle open under the workflow is what says the tree runs.
func seedWaitIssue(t *testing.T, st *store.Store, records record.Store, issue record.Issue, lifecycle bool) {
	t.Helper()
	ctx := context.Background()
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		if !lifecycle {
			return nil
		}
		token, err := claim.ProjectToken(waitProject)
		if err != nil {
			return err
		}
		_, err = records.OpenTreeLifecycle(ctx, tx, token, issue.Tree, treelifecycle.AuthorityWorkflow)
		return err
	}); err != nil {
		t.Fatalf("seed the issue %s: %v", issue.Key, err)
	}
}

// seedWaitSlot gives key one of the cap's rows, as the promotion that started it did.
func seedWaitSlot(t *testing.T, st *store.Store, records record.Store, key string, index int) {
	t.Helper()
	ctx := context.Background()
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		return records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: index, AdmittedAt: waitNow})
	}); err != nil {
		t.Fatalf("seed the slot of %s: %v", key, err)
	}
}

// seedWaitMirror writes what Dispatch says of key: its status, its parent, and in order the keys
// it waits on, as the blockers.Mirror handler writes a row from an event.
func seedWaitMirror(t *testing.T, st *store.Store, records record.Store, key, status, parent string, waits ...string) {
	t.Helper()
	ctx := context.Background()
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		return records.PutDispatchIssue(ctx, tx, record.DispatchIssueRow{Key: key, Project: waitProject,
			Status: status, Parent: parent, Blockers: waits, Seq: 1})
	}); err != nil {
		t.Fatalf("seed the mirror of %s: %v", key, err)
	}
}

// projectWaitingState is the state of the test project through tx, as the daemon's own source
// builds it (internal/projection), with no claims and no capability reports: this reading is the
// admission object alone.
func projectWaitingState(ctx context.Context, tx pgx.Tx, records record.Store, steps []record.DesignStep) (api.State, error) {
	return projection.Project(ctx, tx, records, waitProject, steps, nil, nil, waitNow)
}

// waitingIssue is one issue of the fixture, written out so each case names the record it reads.
type waitingIssue struct {
	key     string
	stands  phase.Phase
	status  string
	tree    string
	parent  string
	slotted bool
}

// A waiting issue is reported waiting and blocked. This half reads the state the daemon serves
// (GET /legion/v1/state): the two started issues that wait stand in `admission.waiting` and in
// `admission.blocked`, each with the keys it waits on, and in neither case in `admission.active`,
// because an issue the daemon started and that waits on another issue holds none of the cap's
// rows (INFRA-3036). What a reader of `legion state` is told of them is the other half, under
// this name in packages/daemon/cmd/legion: the plain line an operator reads. Neither half reads
// the other's reader, so neither proves what the other does.
//
// Beside the two, the readings that must not change. An issue the daemon has not started that a
// link holds is reported blocked as it always was, and whether it stands in the line is the
// line's own rule and not this change's: a queued root stands there and is passed over at
// promotion, a child held at admitted is not in the line at all. An issue a person moved to
// backlog is in none of the three lists, since nothing of it is waiting for Legion. An issue at
// work on nothing is in `admission.active` alone. And the `admission` object carries the keys it
// carried before the change, no more and no fewer: a reader of the state — the controller, the
// plugin, `legion state` — reads the same object.
func TestAWaitingIssueIsReportedWaitingAndBlocked(t *testing.T) {
	const (
		startedLink  = "LEGION-210"
		startedChild = "LEGION-211"
		queuedRoot   = "LEGION-212"
		heldChild    = "LEGION-213"
		parked       = "LEGION-214"
		atWork       = "LEGION-215"
		treeRoot     = "LEGION-220"
		linkBlocker  = "LEGION-300"
		openChild    = "LEGION-301"
		rootBlocker  = "LEGION-302"
		childBlocker = "LEGION-303"
	)
	st, records, steps := waitingStore(t)
	ctx := context.Background()

	for _, seed := range []waitingIssue{
		// An issue the daemon has started, at work, that a Dispatch link holds.
		{key: startedLink, stands: phase.Implementing, status: "in_progress", tree: startedLink, slotted: true},
		// A tree's own root the daemon has started whose design gate has opened, so its own
		// first phase is what waits, and an open Dispatch child under it is what it waits for.
		{key: startedChild, stands: phase.Admitted, status: "in_progress", tree: startedChild, slotted: true},
		// A root the daemon has not started, which a link holds: it stands in the line and the
		// promotion passes over it.
		{key: queuedRoot, stands: phase.Admitted, status: "todo", tree: queuedRoot},
		// The live tree the held child belongs to, and the child itself, held at admitted by a
		// link: a child the workflow holds is out of the line altogether.
		{key: treeRoot, stands: phase.Accepting, status: "needs_review", tree: treeRoot, slotted: true},
		{key: heldChild, stands: phase.Admitted, status: "in_progress", tree: treeRoot, parent: treeRoot},
		// An issue a person moved out of the workflow, and one at work waiting on nothing.
		{key: parked, stands: phase.Implementing, status: "backlog", tree: parked, slotted: true},
		{key: atWork, stands: phase.Implementing, status: "in_progress", tree: atWork, slotted: true},
	} {
		issue := record.Issue{Key: seed.key, Repo: waitRepo, DefaultBranch: "main", GitHubRepo: waitRepo,
			Project: waitProject, Title: seed.key, Tree: seed.tree, Phase: seed.stands, Generation: 1,
			Status: seed.status, DispatchStatus: seed.status, Rank: seed.key, HandedOver: true}
		if seed.parent != "" {
			parent := seed.parent
			issue.Parent = &parent
		}
		seedWaitIssue(t, st, records, issue, seed.key == seed.tree && seed.slotted)
	}
	for index, key := range []string{startedLink, startedChild, treeRoot, parked, atWork} {
		seedWaitSlot(t, st, records, key, index)
	}
	// The design gate of the started root, opened: its architect has handed the work on, so its
	// own first phase is what the open child holds.
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		approved := 1
		return records.PutGate(ctx, tx, record.DesignGate{Issue: startedChild, ArtifactID: "artifact-211",
			LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true})
	}); err != nil {
		t.Fatalf("seed the gate of %s: %v", startedChild, err)
	}
	seedWaitMirror(t, st, records, startedLink, "in_progress", "", linkBlocker)
	seedWaitMirror(t, st, records, startedChild, "in_progress", "")
	seedWaitMirror(t, st, records, openChild, "backlog", startedChild)
	seedWaitMirror(t, st, records, queuedRoot, "todo", "", rootBlocker)
	seedWaitMirror(t, st, records, heldChild, "in_progress", treeRoot, childBlocker)
	seedWaitMirror(t, st, records, treeRoot, "needs_review", "")
	seedWaitMirror(t, st, records, parked, "backlog", "")
	seedWaitMirror(t, st, records, atWork, "in_progress", "")
	for _, blocker := range []string{linkBlocker, rootBlocker, childBlocker} {
		seedWaitMirror(t, st, records, blocker, "in_progress", "")
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(records, workflow.Config{Project: waitProject, Repositories: waitRepositories{},
		Steps: steps, Clock: func() time.Time { return waitNow }}, quiet)
	admission := admit.New(records, engine, 8, waitProject, quiet)
	if _, err := intake.ApplyFact(ctx, st.Pool(), "controller", "tick", intake.ControllerTick{}, engine, admission); err != nil {
		t.Fatalf("run an admission pass: %v", err)
	}

	server := api.NewServer("127.0.0.1", 8437, api.Options{
		Steps: steps, State: projectedSource{records: records, steps: steps}, StateTransactions: st,
	})
	request := httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil)
	recorder := httptest.NewRecorder()
	server.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("state = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.Bytes()

	var served struct {
		Admission map[string]json.RawMessage `json:"admission"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	if got, want := slices.Sorted(maps.Keys(served.Admission)), []string{"active", "blocked", "cap", "trees", "waiting"}; !slices.Equal(got, want) {
		t.Fatalf("the admission object carries %v, want %v: a reader of the state reads the object it read before the change", got, want)
	}

	var state api.State
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	admissionView := state.Admission

	// The two issues the daemon started that wait: in the line, in blocked with what each waits
	// on, and in neither case holding one of the cap's rows.
	for _, waiting := range []struct {
		key   string
		waits []string
	}{
		{key: startedLink, waits: []string{linkBlocker}},
		{key: startedChild, waits: []string{openChild}},
	} {
		if !slices.Contains(admissionView.Waiting, waiting.key) {
			t.Errorf("admission.waiting = %v, want %s in it: a started issue that waits stands in the line for the row it gave back",
				admissionView.Waiting, waiting.key)
		}
		if slices.Contains(admissionView.Active, waiting.key) {
			t.Errorf("admission.active = %v, want %s out of it: an issue that waits on another issue holds none of the cap's rows",
				admissionView.Active, waiting.key)
		}
		if got := blockedBy(admissionView.Blocked, waiting.key); !slices.Equal(got, waiting.waits) {
			t.Errorf("admission.blocked names %s waiting on %v, want %v", waiting.key, got, waiting.waits)
		}
	}

	// The readings this change leaves as they were.
	if got := blockedBy(admissionView.Blocked, queuedRoot); !slices.Equal(got, []string{rootBlocker}) {
		t.Errorf("admission.blocked names the queued root %s waiting on %v, want %v", queuedRoot, got, []string{rootBlocker})
	}
	if !slices.Contains(admissionView.Waiting, queuedRoot) {
		t.Errorf("admission.waiting = %v, want the queued root %s in it: it stands in the line and the promotion passes over it",
			admissionView.Waiting, queuedRoot)
	}
	if got := blockedBy(admissionView.Blocked, heldChild); !slices.Equal(got, []string{childBlocker}) {
		t.Errorf("admission.blocked names the held child %s waiting on %v, want %v", heldChild, got, []string{childBlocker})
	}
	if slices.Contains(admissionView.Waiting, heldChild) {
		t.Errorf("admission.waiting = %v, want the held child %s out of it: a child a link holds at admitted is not in the line",
			admissionView.Waiting, heldChild)
	}
	for _, list := range []struct {
		name string
		keys []string
	}{
		{name: "active", keys: admissionView.Active},
		{name: "waiting", keys: admissionView.Waiting},
		{name: "blocked", keys: blockedKeys(admissionView.Blocked)},
	} {
		if slices.Contains(list.keys, parked) {
			t.Errorf("admission.%s = %v, want the parked issue %s out of it: nothing of it waits for Legion", list.name, list.keys, parked)
		}
	}
	if got, want := slices.Sorted(slices.Values(admissionView.Active)), []string{atWork, treeRoot}; !slices.Equal(got, want) {
		t.Errorf("admission.active = %v, want %v: the issues at work on their own phases, and no issue that waits", got, want)
	}
	for _, list := range []struct {
		name string
		keys []string
	}{
		{name: "waiting", keys: admissionView.Waiting},
		{name: "blocked", keys: blockedKeys(admissionView.Blocked)},
	} {
		if slices.Contains(list.keys, atWork) {
			t.Errorf("admission.%s = %v, want the issue at work %s out of it: it waits on nothing", list.name, list.keys, atWork)
		}
	}
}

// blockedBy is what `admission.blocked` says key waits on, nil for a key it does not name.
func blockedBy(blocked []api.BlockedIssue, key string) []string {
	for _, issue := range blocked {
		if issue.Issue == key {
			return issue.BlockedBy
		}
	}
	return nil
}

// blockedKeys is every issue `admission.blocked` names.
func blockedKeys(blocked []api.BlockedIssue) []string {
	keys := make([]string, len(blocked))
	for i, issue := range blocked {
		keys[i] = issue.Issue
	}
	return keys
}

// This file is the state row of the Kubernetes refusal rows (kube_refusal_answer_test.go) as the
// daemon serves the state: built by internal/projection, which imports package api, so a file
// inside package api could not call it. It is an external test package for that reason, and
// builds its supervision from exported API alone: a claim's machine (supervise.NewMachine) over
// the store through the boot tokens' recording store (api.NewBootTokens), as the daemon's
// directory builds one.
package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// kubeProjectionClock is time that never moves, at the instant this package's projection is
// taken (waitNow): a machine's timers are armed and never fire, and its launch backoff returns at
// once, so a launch the runtime refuses is retried until its budget runs out within one request.
type kubeProjectionClock struct{}

func (kubeProjectionClock) Now() time.Time { return waitNow }

func (kubeProjectionClock) AfterFunc(time.Duration, func()) supervise.Cancel {
	return kubeProjectionStopped{}
}

func (kubeProjectionClock) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

type kubeProjectionStopped struct{}

func (kubeProjectionStopped) Stop() bool { return true }

// kubeProjectionSpecs is a launch the fake runtime takes: the machine fills in who it is for.
type kubeProjectionSpecs struct{}

func (kubeProjectionSpecs) SpawnSpec(context.Context, supervise.Claim) (runtime.SpawnSpec, error) {
	return runtime.SpawnSpec{Prompt: runtime.PromptParts{RolePromptPaths: []string{"/dev/null"}}}, nil
}

// kubeProjectionSource is the daemon's state source as it reads the claims (internal/daemon's
// source.State): the store's claims of the project, read through the route's own snapshot
// (store.ClaimsTx), handed to internal/projection with the records of that snapshot, so the body
// and its claims come from one moment.
type kubeProjectionSource struct {
	store   *store.Store
	records record.Store
	steps   []record.DesignStep
	project string
}

func (s kubeProjectionSource) State(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	claims, err := s.store.ClaimsTx(ctx, tx)
	if err != nil {
		return api.State{}, nil, err
	}
	claims = slices.DeleteFunc(claims, func(c supervise.Claim) bool { return c.Project != s.project })
	state, err := projection.Project(ctx, projection.Inputs{
		Tx: tx, Store: s.records, Project: waitProject, Steps: s.steps, Claims: claims, Now: waitNow,
	})
	return state, claims, err
}

// kubeProjectionDeps are a claim's machine's dependencies as the package's harness wires them
// (internal/api's newHarnessDeps), over st and rt: the store written through the boot tokens'
// recording store, and the daemon's own rule that a tree a workflow issue backs does not close.
func kubeProjectionDeps(st *store.Store, rt *fake.Runtime) supervise.Deps {
	return supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: api.NewBootTokens(st).Recording(st), Specs: kubeProjectionSpecs{},
		Clock: kubeProjectionClock{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2},
		Timeouts: supervise.Timeouts{
			Boot: time.Minute, RegistrationIntervals: 3, RPC: 5 * time.Second, Probe: 30 * time.Second, Stop: 10 * time.Second,
			LaunchBackoff: time.Millisecond,
		},
		PluginContract: api.DaemonAPIVersion, PluginRelaunchable: true, RefreshGate: supervise.NewRefreshGate(0),
		TreeClosable: func(ctx context.Context, c supervise.Claim) (bool, error) {
			var issue *record.Issue
			if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
				var err error
				issue, err = record.NewStore().Issue(ctx, tx, c.Tree)
				return err
			}); err != nil {
				return false, err
			}
			return issue == nil, nil
		},
	}
}

// TestAKubernetesRefusalOfALaunchReachesTheProjectedStateWithNoCredential: a root's architect,
// recorded as the workflow records one (its issue at admitted, its tree open under the workflow,
// its phases row naming its claim), is launched by its machine, and every launch is refused by the
// Sandbox runtime itself, its Kubernetes API refusing the launch's request with a Status that
// echoes the bearer the request carried (api.KubeRefusedLaunch). That refusal is the fake
// runtime's answer to each launch, which is how the runtime's answer enters the daemon
// (supervise.Deps.Runtime); the machine stores it as the claim's last start failure
// (storetext.FarSideError) and fails the claim once its launch budget runs out. The state route
// (GET /legion/v1/state) answers the claim as internal/projection places it on its issue, through
// the route's own repeatable-read snapshot of the store: the architect's lastStartFailure and the
// failure its spent budget wrote hold the runtime's projection of the Kubernetes error
// (projectKubeError, in resend), and none of the bearer. Capture kubeerror.api/state-projection,
// the answer's body.
func TestAKubernetesRefusalOfALaunchReachesTheProjectedStateWithNoCredential(t *testing.T) {
	const root = "LEGION-208"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, records, steps := waitingStore(t)
	project, err := claim.ProjectToken(waitProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, root, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	c := supervise.Claim{Token: token, Project: project, Tree: root, Issue: root, Role: claim.RoleArchitect, State: supervise.StateQueued}
	refused := api.KubeRefusedLaunch(t, c)

	seedWaitIssue(t, st, records, record.Issue{Key: root, Repo: waitRepo, DefaultBranch: "main", GitHubRepo: waitRepo,
		Project: waitProject, Title: root, Tree: root, Phase: phase.Admitted, Generation: 1,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: root, HandedOver: true}, true)
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: root, Role: claim.RoleArchitect, Claim: token,
			Entry: 1, EntryTask: root + " is yours.", EntryRaisedAt: waitNow})
	}); err != nil {
		t.Fatalf("record the architect's phases row of %s: %v", root, err)
	}

	rt := fake.NewRuntime()
	answer := fake.SpawnResult{Err: refused}
	rt.ScriptSpawn(answer, answer, answer)
	deps := kubeProjectionDeps(st, rt)
	admitted, err := deps.Store.AdmitClaim(ctx, c)
	if err != nil {
		t.Fatalf("admit %s: %v", token, err)
	}
	m, err := supervise.NewMachine(ctx, deps, admitted)
	if err != nil {
		t.Fatalf("build the machine of %s: %v", token, err)
	}
	if err := m.Handle(ctx, supervise.RequestSpawn{Claim: token}); err == nil {
		t.Fatalf("the launches of %s the runtime refused answered no error", token)
	}
	claims, err := st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	i := slices.IndexFunc(claims, func(stored supervise.Claim) bool { return stored.Token == token })
	if i < 0 {
		t.Fatalf("the store holds no claim %s", token)
	}
	stored := claims[i]
	if stored.State != supervise.StateFailed || !strings.Contains(stored.LastStartFailure, refused.Error()) ||
		!strings.Contains(stored.Failure, refused.Error()) {
		t.Fatalf("the store holds %s at %s, last start failure %q and failure %q; want it failed, each naming the runtime's refusal %q",
			token, stored.State, stored.LastStartFailure, stored.Failure, refused)
	}

	server := api.NewServer("127.0.0.1", 8437, api.Options{
		Steps: steps, State: kubeProjectionSource{store: st, records: records, steps: steps, project: project},
		Pool: st.Pool(), StateTransactions: st.Pool(),
	})
	recorder := httptest.NewRecorder()
	server.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil))

	body := recorder.Body.String()
	oauth2calltest.RequireNone(t, "kubeerror.api/state-projection", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("read the state = %d, want 200; body %s", recorder.Code, body)
	}
	var state api.State
	if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
		t.Fatalf("the state %q does not decode: %v", body, err)
	}
	architect := state.Issues[root].Architect
	if architect == nil {
		t.Fatalf("the state carries no architect of %s: %s", root, body)
	}
	if architect.LastStartFailure != stored.LastStartFailure {
		t.Errorf("the state answers the last start failure %q, want %q, the stored one", architect.LastStartFailure, stored.LastStartFailure)
	}
	if architect.Failure == nil || architect.Failure.Detail != stored.Failure {
		t.Errorf("the state answers the failure %+v, want the stored sentence %q", architect.Failure, stored.Failure)
	}
}

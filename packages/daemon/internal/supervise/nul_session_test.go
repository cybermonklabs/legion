package supervise_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// stillClock is time that never moves: the machine's timers are armed and never fire, so the test
// sees only what its own events did.
type stillClock struct{}

func (stillClock) Now() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) }

func (stillClock) AfterFunc(time.Duration, func()) supervise.Cancel { return stopped{} }

func (stillClock) At(time.Time, func()) supervise.Cancel { return stopped{} }

func (stillClock) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

type stopped struct{}

func (stopped) Stop() bool { return true }

// fixedSpecs is a launch the fake runtime accepts: the machine fills in who it is for.
type fixedSpecs struct{}

func (fixedSpecs) SpawnSpec(context.Context, supervise.Claim) (runtime.SpawnSpec, error) {
	return runtime.SpawnSpec{Prompt: runtime.PromptParts{RolePromptPaths: []string{"/dev/null"}}}, nil
}

// A session id is the agent's own, taken at its registration as the agent sent it, so it can
// hold a NUL, which the store keeps as U+FFFD. The claim holds the session as the store keeps it
// from its registration on, so a request from that session is the claim's own before a restart
// and after one, when the claim is read back from the store: it is never refused as another
// agent's (claim.SameAgentRefusal), which would leave the claim's own agent unable to say it is
// ready from the first restart on.
func TestARequestFromASessionWhoseIDHoldsNULIsTheClaimsOwnAfterARestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := testpg.CloneStore(t, "legion_supervise_test")
	const project, tree, issue = "legion", "LEGION-208", "LEGION-209"
	const session, stored = "ses_\x00implementer", "ses_\uFFFDimplementer"
	token, err := claim.NewToken(project, issue, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if _, err := st.OpenTreeLifecycle(ctx, project, tree, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", tree, err)
	}
	queued, err := st.AdmitClaim(ctx, supervise.Claim{Token: token, Project: project, Tree: tree, Issue: issue, Role: claim.RoleImplementer, State: supervise.StateQueued})
	if err != nil {
		t.Fatalf("admit the claim: %v", err)
	}
	conns := fake.NewConns()
	deps := supervise.Deps{
		Runtime: fake.NewRuntime(), Conns: conns, Store: st, Specs: fixedSpecs{}, Clock: stillClock{},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2},
		Timeouts: supervise.Timeouts{Boot: time.Minute, RegistrationIntervals: 3, RPC: 5 * time.Second, Probe: 30 * time.Second,
			Stop: 10 * time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 1,
		RefreshGate:    supervise.NewRefreshGate(0),
	}
	handle := func(m *supervise.Machine, ev supervise.Event) error {
		t.Helper()
		err := m.Handle(ctx, ev)
		m.Wait()
		return err
	}

	m, err := supervise.NewMachine(ctx, deps, queued)
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	if err := handle(m, supervise.RequestSpawn{Claim: token}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	conns.Register(token, fake.NewConn())
	generation := m.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: session, SessionFile: "/state/sessions/\x00implementer.jsonl",
			CapabilityHash: []byte("capability"), PluginContract: deps.PluginContract},
		supervise.RequestReady{Claim: token, Generation: generation, Session: session},
	} {
		if err := handle(m, ev); err != nil {
			t.Fatalf("%T before the restart: %v", ev, err)
		}
	}
	if got := m.Claim(); got.Session != stored || got.SessionFile != "/state/sessions/\uFFFDimplementer.jsonl" || got.State != supervise.StateReady {
		t.Fatalf("the claim before the restart is %s with session %q and session file %q, want ready with %q and %q, as the store keeps them",
			got.State, got.Session, got.SessionFile, stored, "/state/sessions/\uFFFDimplementer.jsonl")
	}

	read := readBack(t, ctx, st, token)
	if read.Session != stored {
		t.Fatalf("the stored claim's session is %q, want %q", read.Session, stored)
	}
	restarted, err := supervise.NewMachine(ctx, deps, read)
	if err != nil {
		t.Fatalf("new machine from the stored claim: %v", err)
	}
	if err := handle(restarted, supervise.RequestReady{Claim: token, Generation: generation, Session: session}); err != nil {
		if errors.Is(err, claim.SameAgentRefusal) {
			t.Fatalf("the ready of the claim's own session after the restart was refused as another agent's: %v", err)
		}
		t.Fatalf("the ready of the claim's own session after the restart: %v", err)
	}
}

// readBack is token's claim as the store reads it back, as a daemon's boot reads every claim.
func readBack(t *testing.T, ctx context.Context, st *store.Store, token claim.Token) supervise.Claim {
	t.Helper()
	claims, err := st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the stored claims: %v", err)
	}
	for _, c := range claims {
		if c.Token == token {
			return c
		}
	}
	t.Fatalf("the store holds no claim %s", token)
	return supervise.Claim{}
}

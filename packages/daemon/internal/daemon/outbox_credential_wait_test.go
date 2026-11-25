package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// credentialWaitRuntime is the fake runtime with an issue pod that holds every role, whose
// launcher of each slot in waits said whether it waits for a start's credential
// (runtime.IssuePodRoles). A slot not in waits has no live launcher, and answers that it waits.
type credentialWaitRuntime struct {
	*fake.Runtime
	waits map[claim.Role]bool
}

func (credentialWaitRuntime) PodLacksRole(string, claim.Role) (string, bool) { return "", false }

func (r credentialWaitRuntime) SlotWaitsForCredential(_ string, slot claim.Role) (string, bool) {
	waits, live := r.waits[slot]
	if !live {
		return "", true
	}
	return "pod-1", waits
}

// A re-role writes the new role's credential into its claim's slot of the live pod and starts the
// generation once the slot's launcher holds it (the sandbox runtime's writeLaunchCredential). A launcher of an image before that
// wait starts the child at once, acting as the App its gh files held, the earlier role's, until the
// next refresh and the kubelet's sync. So the start of the tester's entry for key a's agent,
// `…-implementer`, whose stored role is the implementer, waits on its row where the pod's
// launcher of the slot `implementer` said no credential wait: the error is a wait and
// runtime.ErrAwaitsPodUpdate, its text begins with pods.StartWaitsForPodPrefix and names the pod,
// the claim keeps the implementer's role, and the runtime is asked no launch. The pod's update
// round or its release replaces the pod with launchers that wait.
//
// The control is the same start where that launcher waits: the claim takes the tester's role and
// is resumed. A start that re-roles nothing — the implementer's own entry — is not held by the
// launcher that makes no wait.
func TestAReroleInAPodBuiltBeforeTheCredentialWaitWaitsForTheRound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		waits    bool
		role     claim.Role
		phase    phase.Phase
		entry    int64
		held     bool
		wantRole claim.Role
	}{
		{name: "the launcher makes no credential wait", role: claim.RoleTester, phase: phase.Testing, entry: 2, held: true, wantRole: claim.RoleImplementer},
		{name: "control: the launcher waits for the credential", waits: true, role: claim.RoleTester, phase: phase.Testing, entry: 2, wantRole: claim.RoleTester},
		{name: "a start that re-roles nothing", role: claim.RoleImplementer, phase: phase.Implementing, entry: 3, wantRole: claim.RoleImplementer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			status := map[phase.Phase]string{phase.Testing: "testing", phase.Implementing: "in_progress"}[tc.phase]
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: "LEGION-208", Title: "Workflow", Phase: tc.phase, Generation: 1, Status: status}
			putOutboxIssue(t, pool, records, issue)
			keyed := mustClaimToken(t, issue.Key, claim.RoleImplementer)
			implementerEntry := int64(1)
			if tc.role == claim.RoleImplementer {
				implementerEntry = tc.entry
			}
			putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: keyed, StepTask: "t", StepPlace: 1, Entry: implementerEntry})
			putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleTester, Claim: keyed, StepTask: "t", StepPlace: 2, Entry: 2})
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			sup.deps.Runtime = credentialWaitRuntime{Runtime: rt, waits: map[claim.Role]bool{claim.RoleImplementer: tc.waits}}
			machine, _, err := sup.Create(ctx, supervise.Claim{Token: keyed, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
				Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
			if err != nil {
				t.Fatalf("create the claim of key a: %v", err)
			}
			if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: keyed, Exempt: true}); err != nil {
				t.Fatalf("spawn key a's agent: %v", err)
			}
			launch := machine.Claim().Generation
			if err := machine.Handle(ctx, supervise.RequestRegister{Claim: keyed, Generation: launch, Session: "ses-a", SessionFile: "/tmp/a.jsonl"}); err != nil {
				t.Fatalf("register key a's agent: %v", err)
			}
			if err := machine.Handle(ctx, supervise.RequestReady{Claim: keyed, Generation: launch, Session: "ses-a"}); err != nil {
				t.Fatalf("ready key a's agent: %v", err)
			}
			if err := machine.Handle(ctx, supervise.RequestSuspend{Claim: keyed, By: supervise.SuspendedByDaemon, Reason: "its step ended"}); err != nil {
				t.Fatalf("suspend key a's agent: %v", err)
			}
			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, notices: &outboxPublisher{},
				trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			launches := len(rt.CallsOf("Resume")) + len(rt.CallsOf("Spawn"))

			start := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: tc.role, Phase: tc.phase,
				Entry: tc.entry, Task: "Do it.", Generation: 1}, time.Now())
			start.ID = 71
			err = runner.execute(ctx, start)
			asked := len(rt.CallsOf("Resume")) + len(rt.CallsOf("Spawn")) - launches
			if got := machine.Claim().Role; got != tc.wantRole {
				t.Errorf("after the start the claim holds the role %s, want %s", got, tc.wantRole)
			}
			if !tc.held {
				if err != nil || asked != 1 {
					t.Fatalf("the start = %v with %d launches asked, want it to launch the claim once", err, asked)
				}
				return
			}
			if !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
				t.Fatalf("the start = %v, want a wait that is runtime.ErrAwaitsPodUpdate", err)
			}
			want := pods.StartWaitsForPodPrefix + "tester of LEGION-208: the pod pod-1 was built before its launchers waited for a new role's credential; its update round or its release replaces it"
			if !strings.HasPrefix(err.Error(), want) {
				t.Errorf("the wait reads %q, want it to begin %q", err, want)
			}
			if asked != 0 {
				t.Errorf("the runtime was asked %d launches, want none while the pod's launcher makes no credential wait", asked)
			}
			if got := machine.Claim().State; got != supervise.StateSuspended {
				t.Errorf("the claim is %s, want it suspended, running nothing", got)
			}
		})
	}
}

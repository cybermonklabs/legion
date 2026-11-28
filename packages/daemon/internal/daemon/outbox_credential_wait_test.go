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
	waits map[claim.Agent]bool
}

func (credentialWaitRuntime) PodLacksRole(string, claim.Role) (string, bool) { return "", false }

func (r credentialWaitRuntime) SlotWaitsForCredential(_ string, slot claim.Agent) (string, bool, error) {
	waits, live := r.waits[slot]
	if !live {
		return "", true, nil
	}
	return "pod-1", waits, nil
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
			sup.deps.Runtime = credentialWaitRuntime{Runtime: rt, waits: map[claim.Agent]bool{claim.Agent(claim.RoleImplementer): tc.waits}}
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
			// The idle release is on, so the pod's release ends the wait and the controller is told
			// nothing of it (TestAReroleWaitForTheCredentialWithNoIdleReleaseIsToldOnce).
			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, notices: &outboxPublisher{},
				trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), idleReleases: true,
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

// Under a deployment with no idle release (idle_minutes: 0), nothing ends a re-role's wait for a
// pod whose slot's launcher does not wait for a start's credential where no claim records a process
// in that pod: no release comes, and no update round opens over it. So the controller is told
// once, as one pod-wait-held notice naming the role, the issue, the pod and why it waits, and the
// wait's text then begins with startWaitToldPrefix, so the row's next try tells nothing more, as
// the wait for a pod built before a role does (TestAWaitUnderADeploymentWithNoIdleReleaseIsToldOnce).
// The controls are the same wait where a claim records a process in the pod, whose round replaces
// it, and the same wait with the idle release on, which ends it: neither tells anything.
func TestAReroleWaitForTheCredentialWithNoIdleReleaseIsToldOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		idleReleases bool
		recorded     bool
		told         bool
	}{
		{name: "no claim records a process in the pod", told: true},
		{name: "control: a claim records a process in the pod", recorded: true},
		{name: "control: the idle release is on", idleReleases: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
			putOutboxIssue(t, pool, records, issue)
			keyed := mustClaimToken(t, issue.Key, claim.RoleImplementer)
			putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: keyed, StepTask: "t", StepPlace: 1, Entry: 1})
			putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleTester, Claim: keyed, StepTask: "t", StepPlace: 2, Entry: 2})
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			sup.deps.Runtime = credentialWaitRuntime{Runtime: rt, waits: map[claim.Agent]bool{claim.Agent(claim.RoleImplementer): false}}
			// The claims are stored as the daemon stores them, where the wait reads every claim of
			// the issue for a process recorded in the pod.
			st := outboxTreeStore(t, pool, issue.Tree)
			sup.deps.Store, sup.store = st, st
			// Key a's agent ran its implementer step and was suspended at its end, so its claim
			// runs nothing, as a claim does when its next step takes another role.
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
			if tc.recorded {
				architect := mustClaimToken(t, issue.Key, claim.RoleArchitect)
				if err := st.PutClaim(ctx, supervise.Claim{Token: architect, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
					Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle,
					Locator: podUpdateLocator(architect, issue.Key, "pod-1")}); err != nil {
					t.Fatal(err)
				}
			}
			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, notices: &outboxPublisher{},
				trees: st, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), idleReleases: tc.idleReleases,
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester, Phase: phase.Testing,
				Entry: 2, Task: "Do it.", Generation: 1}, time.Now())
			row.ID = 72
			try := func() error {
				err := runner.execute(ctx, row)
				if err != nil {
					row.LastError = err.Error()
				}
				return err
			}

			for _, attempt := range []string{"first", "second"} {
				err := try()
				if !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, runtime.ErrAwaitsPodUpdate) || !strings.HasPrefix(err.Error(), pods.StartWaitsForPodPrefix) {
					t.Fatalf("the %s try = %v, want a wait for the pod that is runtime.ErrAwaitsPodUpdate", attempt, err)
				}
				if told := strings.HasPrefix(err.Error(), startWaitToldPrefix); told != tc.told {
					t.Errorf("the %s try reads %q, which says the controller is told: %t; want %t", attempt, err, told, tc.told)
				}
				notices := outboxPodWaitHeld(t, runner, issue.Key)
				if !tc.told {
					if len(notices) != 0 {
						t.Errorf("the %s try queued the pod-wait-held notices %q, want none: something else ends the wait", attempt, notices)
					}
					continue
				}
				if len(notices) != 1 {
					t.Fatalf("after the %s try the outbox holds %d pod-wait-held notices, want the one", attempt, len(notices))
				}
				for _, want := range []string{"tester", "LEGION-208", "pod-1", "launchers waited", "idle_minutes: 0", "can hold work that a role did not push"} {
					if !strings.Contains(notices[0], want) {
						t.Errorf("the notice's reason %q does not name %q", notices[0], want)
					}
				}
			}
		})
	}
}

// A launch the runtime refuses because the issue's pod cannot run it until that pod's update round
// or its release replaces it — here a re-role into a slot whose launcher, read after a daemon
// restart, starts a child at once (the Sandbox runtime's relaunch) — leaves the start row waiting
// for the pod as a start the pod lacks a name of waits: its text begins with
// pods.StartWaitsForPodPrefix, so the idle view counts it as no delivery and the release rules may
// take the pod, and the claim, the pod's only one, is left suspended by the daemon. The next try,
// while that pod stands, launches nothing and leaves the claim's generation as it was. Once
// another pod stands, the next try resumes the claim.
func TestAStartWhoseLaunchItsPodRefusedWaitsForThePod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
	putOutboxIssue(t, pool, records, issue)
	keyed := mustClaimToken(t, issue.Key, claim.RoleImplementer)
	putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: keyed, StepTask: "t", StepPlace: 1, Entry: 1})
	putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleTester, Claim: keyed, StepTask: "t", StepPlace: 2, Entry: 2})
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Runtime = credentialWaitRuntime{Runtime: rt, waits: map[claim.Agent]bool{claim.Agent(claim.RoleImplementer): true}}
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
		trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), idleReleases: true,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	rt.ScriptIssuePod(issue.Key, "pod-1")
	rt.OnResume = func(context.Context) error {
		return runtime.AwaitsPodUpdate{Pod: "pod-1", Reason: "launch " + string(keyed) + ": the pod pod-1 of LEGION-208 was built before its launchers waited for a start's credential"}
	}
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester, Phase: phase.Testing,
		Entry: 2, Task: "Do it.", Generation: 1}, time.Now())
	row.ID = 73
	try := func() error {
		err := runner.execute(ctx, row)
		if err != nil {
			row.LastError = err.Error()
		}
		return err
	}

	first := try()
	if !errors.Is(first, wait.ErrWaiting) || !errors.Is(first, runtime.ErrAwaitsPodUpdate) || !strings.HasPrefix(first.Error(), pods.StartWaitsForPodPrefix) {
		t.Fatalf("the first try = %v, want a wait for the pod that is runtime.ErrAwaitsPodUpdate and begins %q", first, pods.StartWaitsForPodPrefix)
	}
	held := machine.Claim()
	if held.State != supervise.StateSuspended || held.SuspendedBy != supervise.SuspendedByDaemon {
		t.Fatalf("after the refused launch the claim is %s by %q, want it suspended by the daemon, running nothing", held.State, held.SuspendedBy)
	}
	resumes := len(rt.CallsOf("Resume"))
	second := try()
	if !errors.Is(second, wait.ErrWaiting) || !errors.Is(second, runtime.ErrAwaitsPodUpdate) || !strings.HasPrefix(second.Error(), pods.StartWaitsForPodPrefix) {
		t.Fatalf("the try while pod-1 stands = %v, want the same wait for the pod", second)
	}
	if got := len(rt.CallsOf("Resume")); got != resumes || machine.Claim().Generation != held.Generation {
		t.Fatalf("the try while pod-1 stands asked %d launches and left the claim at generation %d; want none, and generation %d",
			got-resumes, machine.Claim().Generation, held.Generation)
	}

	rt.ScriptIssuePod(issue.Key, "pod-2")
	rt.OnResume = nil
	if err := try(); err != nil {
		t.Fatalf("the try once another pod stands = %v, want the claim resumed", err)
	}
	if got := len(rt.CallsOf("Resume")); got != resumes+1 {
		t.Fatalf("the try once another pod stands asked %d launches, want one", got-resumes)
	}
}

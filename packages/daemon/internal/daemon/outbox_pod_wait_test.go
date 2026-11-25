package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// podRolesRuntime is the fake runtime with an issue pod built before some roles of the
// deployment: lacks names each such role, and each slot the pod has none of, with the uid of the
// pod (runtime.IssuePodRoles). A slot is the name its claim's token ends in.
type podRolesRuntime struct {
	*fake.Runtime
	lacks map[claim.Role]string
}

func (r podRolesRuntime) PodLacksRole(_ string, role claim.Role) (string, bool) {
	pod, lacks := r.lacks[role]
	return pod, lacks
}

// SlotWaitsForCredential answers that every launcher of the pod waits for a start's credential:
// the pods of this fake are built by this release.
func (podRolesRuntime) SlotWaitsForCredential(string, claim.Agent) (string, bool, error) {
	return "", true, nil
}

// podWaitRunner is the outbox over a real PostgreSQL with the issue LEGION-208 stored, the
// supervisor reading the claims of that store and its runtime answering that its pod lacks the
// roles of lacks.
func podWaitRunner(t *testing.T, lacks map[claim.Role]string) (*outbox, *supervisor, *fake.Runtime, record.Issue) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Runtime = podRolesRuntime{Runtime: rt, lacks: lacks}
	sup.store = outboxTreeStore(t, pool, issue.Key)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	return runner, sup, rt, issue
}

// A start of a role that the issue's pod was built before waits on its row and makes no claim:
// the launch, not the pod's round or its release, would otherwise decide the pod, and a launch
// into it would replace it with the issue's disk. Where the pod holds the role, the
// same row creates the claim and spawns it.
func TestAStartOfARoleThePodLacksWaitsAndMakesNoClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		lacks map[claim.Role]string
	}{
		{"the pod lacks the role", map[claim.Role]string{"checker": "pod-1"}},
		{"the pod holds the role", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, sup, rt, issue := podWaitRunner(t, tc.lacks)
			row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: "checker", Task: "Check it.", Generation: issue.Generation}, time.Now())
			row.ID = 93
			token, err := claim.NewToken("legion", issue.Key, "checker")
			if err != nil {
				t.Fatal(err)
			}

			err = runner.execute(context.Background(), row)
			_, found := sup.Machine(token)
			if tc.lacks == nil {
				if err != nil || !found {
					t.Fatalf("the start = %v (claim made %t), want it to create the checker's claim", err, found)
				}
				testwait.WithinPolling(t, 5*time.Second, 10*time.Millisecond, "the checker's spawn", func() bool { return len(rt.CallsOf("Spawn")) == 1 })
				return
			}
			if !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
				t.Errorf("the start = %v, want a wait that is runtime.ErrAwaitsPodUpdate", err)
			}
			if err == nil || !strings.HasPrefix(err.Error(), pods.StartWaitsForPodPrefix) {
				t.Errorf("the start's text = %v, want it to begin %q", err, pods.StartWaitsForPodPrefix)
			}
			if found {
				t.Errorf("the start made the checker's claim, want none while it waits")
			}
			if spawns, resumes := rt.CallsOf("Spawn"), rt.CallsOf("Resume"); len(spawns)+len(resumes) > 0 {
				t.Errorf("the runtime was asked %d spawns and %d resumes, want none", len(spawns), len(resumes))
			}
		})
	}
}

// A start reaches the claim its row names, which runs in the slot of its agent, on the role of
// the step: a keyed agent, `implementer` here, on the step of a role the pod was built before,
// `checker`. The launch needs both of the pod (the runtime's kept-pod fence reads both), so the
// start waits on its row where the pod lacks either, its text naming the one it lacks, and asks
// the runtime for nothing: a launch refused for the role would be charged as a launch failure.
func TestAKeyedAgentsStartWaitsWhereThePodLacksItsSlotOrTheRoleOfItsStep(t *testing.T) {
	t.Parallel()
	implementer, err := claim.NewToken("legion", "LEGION-208", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	checker, err := claim.NewToken("legion", "LEGION-208", "checker")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		claim claim.Token
		role  claim.Role
		// names is what the wait's text names that the pod lacks.
		names string
	}{
		{"the pod holds the slot and lacks the role of the step", implementer, "checker", "without checker, the role of its step"},
		{"the pod lacks the slot and holds the role of the step", checker, claim.RoleTester, "without checker, the slot of the claim's agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, sup, rt, issue := podWaitRunner(t, map[claim.Role]string{"checker": "pod-1"})
			row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: tc.role, Claim: tc.claim, Task: "Check it.", Generation: issue.Generation}, time.Now())
			row.ID = 95

			err := runner.execute(context.Background(), row)
			if !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
				t.Errorf("the start = %v, want a wait that is runtime.ErrAwaitsPodUpdate", err)
			}
			if err == nil || !strings.HasPrefix(err.Error(), pods.StartWaitsForPodPrefix) || !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the start's text = %v, want it to begin %q and name %q", err, pods.StartWaitsForPodPrefix, tc.names)
			}
			if _, found := sup.Machine(tc.claim); found {
				t.Errorf("the start made the claim %s, want none while it waits", tc.claim)
			}
			if spawns, resumes := rt.CallsOf("Spawn"), rt.CallsOf("Resume"); len(spawns)+len(resumes) > 0 {
				t.Errorf("the runtime was asked %d spawns and %d resumes, want none", len(spawns), len(resumes))
			}
			// No claim records a process in the pod and the idle release is off, so the controller
			// is told of the wait, naming the same.
			if notices := outboxPodWaitHeld(t, runner, issue.Key); len(notices) != 1 || !strings.Contains(notices[0], tc.names) {
				t.Errorf("the pod-wait-held notices = %q, want one that names %q", notices, tc.names)
			}
			// The notice names the start as `legion state` names a claim (api.ClaimNameInIssue):
			// neither claim here is named for the role of its step, so each is named by that role
			// with its token beside it, and two claims of one role read apart.
			start := api.ClaimNameInIssue(tc.role, tc.claim)
			if notices := outboxPodWaitHeld(t, runner, issue.Key); len(notices) == 1 &&
				!strings.Contains(notices[0], start) {
				t.Errorf("the pod-wait-held notice %q does not name the start %q", notices[0],
					start)
			}
		})
	}
}

// outboxPodWaitHeld are the reasons of the pod-wait-held controller notices the runner's pool
// holds for issue.
func outboxPodWaitHeld(t *testing.T, runner *outbox, issue string) []string {
	t.Helper()
	rows, err := runner.pool.Query(context.Background(), `select coalesce(payload->>'reason', '') from outbox
		where issue = $1 and kind = $2 and payload->>'kind' = $3 order by id`,
		issue, string(record.OutboxKindControllerNotice), string(record.PodWaitHeldNotice))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatal(err)
		}
		reasons = append(reasons, reason)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return reasons
}

// Under a deployment with no idle release (idle_minutes: 0), main's rule never ends a wait for a
// pod built before a role, and where no claim records a process in that pod no update round opens
// over it either: nothing ends the wait until an operator acts, so the controller is told, once
// for each wait that finds no process in the pod.
// Where a claim records a process there, the round asks each role to push and replaces the pod,
// and the controller is told nothing; where the idle release is on, main's rule ends the wait.
func TestAWaitUnderADeploymentWithNoIdleReleaseIsToldOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	implementer, err := claim.NewToken("legion", "LEGION-208", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	setup := func(t *testing.T, idleReleases bool) (*outbox, func(state supervise.ClaimState, recorded bool), func() error) {
		t.Helper()
		runner, sup, _, issue := podWaitRunner(t, map[claim.Role]string{"checker": "pod-1"})
		st := sup.store
		runner.idleReleases = idleReleases
		store := func(state supervise.ClaimState, recorded bool) {
			t.Helper()
			c := supervise.Claim{Token: implementer, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer,
				TreeEpoch: 1, Generation: 1, State: state}
			if state == supervise.StateSuspended {
				c.SuspendedBy = supervise.SuspendedByOperator
			}
			if recorded {
				c.Locator = podUpdateLocator(implementer, issue.Key, "pod-1")
			}
			if err := st.PutClaim(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: "checker", Task: "Check it.", Generation: issue.Generation}, time.Now())
		row.ID = 94
		try := func() error {
			err := runner.execute(ctx, row)
			if err != nil {
				row.LastError = err.Error()
			}
			return err
		}
		return runner, store, try
	}

	t.Run("no claim records a process", func(t *testing.T) {
		runner, store, try := setup(t, false)
		store(supervise.StateSuspended, false)
		first := try()
		notices := outboxPodWaitHeld(t, runner, "LEGION-208")
		if len(notices) != 1 {
			t.Fatalf("the first try queued %d pod-wait-held notices, want one", len(notices))
		}
		for _, want := range []string{"checker", "LEGION-208", "pod-1", "idle_minutes: 0", "can hold work that a role did not push"} {
			if !strings.Contains(notices[0], want) {
				t.Errorf("the notice's reason %q does not name %q", notices[0], want)
			}
		}
		// The start names no claim, so it starts the checker's own claim, which the notice names
		// by its role alone (api.ClaimNameInIssue).
		own, err := claim.NewToken("legion", "LEGION-208", "checker")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(notices[0], string(own)) {
			t.Errorf("the notice's reason %q names the checker's own claim by its token %s, "+
				"want its role alone", notices[0], own)
		}
		for _, never := range []string{"remove", "delete"} {
			if strings.Contains(notices[0], never) {
				t.Errorf("the notice's reason %q says %q: it tells nobody to delete the pod", notices[0], never)
			}
		}
		if first == nil || !strings.HasPrefix(first.Error(), startWaitToldPrefix) {
			t.Errorf("the first try = %v, want a wait that begins %q", first, startWaitToldPrefix)
		}
		_ = try()
		if notices := outboxPodWaitHeld(t, runner, "LEGION-208"); len(notices) != 1 {
			t.Errorf("after the row's second try the outbox holds %d pod-wait-held notices, want the first alone", len(notices))
		}
	})

	t.Run("a claim records a process in the pod", func(t *testing.T) {
		runner, store, try := setup(t, false)
		store(supervise.StateIdle, true)
		for _, attempt := range []string{"first", "second"} {
			err := try()
			if notices := outboxPodWaitHeld(t, runner, "LEGION-208"); len(notices) != 0 {
				t.Errorf("the %s try queued %d pod-wait-held notices, want none: the round replaces that pod after each role pushed", attempt, len(notices))
			}
			if err == nil || !strings.HasPrefix(err.Error(), pods.StartWaitsForPodPrefix) || strings.HasPrefix(err.Error(), startWaitToldPrefix) {
				t.Errorf("the %s try = %v, want a wait that begins %q and not %q", attempt, err, pods.StartWaitsForPodPrefix, startWaitToldPrefix)
			}
		}
		// The claim's process ended before a round opened over the pod.
		store(supervise.StateSuspended, false)
		_ = try()
		if notices := outboxPodWaitHeld(t, runner, "LEGION-208"); len(notices) != 1 {
			t.Errorf("after the process ended the outbox holds %d pod-wait-held notices, want one", len(notices))
		}
		_ = try()
		if notices := outboxPodWaitHeld(t, runner, "LEGION-208"); len(notices) != 1 {
			t.Errorf("after the try after it the outbox holds %d pod-wait-held notices, want the one", len(notices))
		}
	})

	t.Run("the idle release is on", func(t *testing.T) {
		runner, store, try := setup(t, true)
		store(supervise.StateSuspended, false)
		_ = try()
		_ = try()
		if notices := outboxPodWaitHeld(t, runner, "LEGION-208"); len(notices) != 0 {
			t.Errorf("the tries queued %d pod-wait-held notices, want none: main's rule ends that wait", len(notices))
		}
	})
}

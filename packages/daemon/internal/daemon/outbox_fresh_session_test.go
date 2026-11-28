package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	stepplan "github.com/sjawhar/legion/daemon/internal/plan" // the package's own `plan` type holds the bare name
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The agents of two steps in the identities a start names them by (plan.AgentID): the agent of
// step 1 of task a, which has no key, whose session the claim holds, and the agent of the key b,
// which a later version gives the same name.
const (
	earlierAgent stepplan.AgentID = "step:1:a"
	laterAgent   stepplan.AgentID = "key:b"
)

// agentStartRig is one claim of LEGION-208's implementer whose session, ses-earlier, is the
// session of earlierAgent, and the outbox runner that executes the starts of the claim.
type agentStartRig struct {
	t       *testing.T
	ctx     context.Context
	issue   record.Issue
	token   claim.Token
	machine *supervise.Machine
	rt      *fake.Runtime
	runner  *outbox
}

// newAgentStartRig spawns the claim for earlierAgent, which records that agent as the agent of
// the session the claim begins, and brings it up to ready in ses-earlier; suspended, the claim is
// then suspended by the daemon, as the stop of its task suspends it.
func newAgentStartRig(t *testing.T, suspended bool) *agentStartRig {
	t.Helper()
	return newAgentStartRigOf(t, suspended, earlierAgent)
}

// newAgentStartRigOf is newAgentStartRig with the claim spawned for agent, "" for a claim that
// records no agent, as every claim a release before the record ran does.
func newAgentStartRigOf(t *testing.T, suspended bool, agent stepplan.AgentID) *agentStartRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	token := mustClaimToken(t, issue.Key, claim.RoleImplementer)
	putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: token, StepTask: "p", StepPlace: 1, Entry: 2})
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
		Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create the claim: %v", err)
	}
	r := &agentStartRig{t: t, ctx: ctx, issue: issue, token: token, machine: machine, rt: rt,
		runner: &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, notices: &outboxPublisher{},
			trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
			provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
				return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
			},
		}}
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token, Exempt: true, Start: supervise.StartWork{Agent: agent}}); err != nil {
		t.Fatalf("spawn the earlier agent: %v", err)
	}
	r.comesUp("ses-earlier")
	if suspended {
		if err := machine.Handle(ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByDaemon, Reason: "its task was stopped"}); err != nil {
			t.Fatalf("suspend the earlier agent: %v", err)
		}
	}
	if got := machine.Claim().SessionAgent; got != agent {
		t.Fatalf("the claim spawned for %q records the agent %q of its session, want %q", agent, got, agent)
	}
	return r
}

// comesUp is the process of the claim's launch registering in session and saying it is ready, as
// the plugin does.
func (r *agentStartRig) comesUp(session string) {
	r.t.Helper()
	launch := r.machine.Claim().Generation
	if err := r.machine.Handle(r.ctx, supervise.RequestRegister{Claim: r.token, Generation: launch, Session: session, SessionFile: "/tmp/" + session + ".jsonl"}); err != nil {
		r.t.Fatalf("register %s: %v", session, err)
	}
	if err := r.machine.Handle(r.ctx, supervise.RequestReady{Claim: r.token, Generation: launch, Session: session}); err != nil {
		r.t.Fatalf("ready %s: %v", session, err)
	}
}

// start is the start row id of step 1 of the plan in force, for agent, at generation, as the
// workflow's entry queues it.
func (r *agentStartRig) start(id int64, agent stepplan.AgentID, generation uint64) record.OutboxRow {
	r.t.Helper()
	row := mustOutboxRow(r.t, r.issue.Key, record.SuperviseRequest{Op: "start", Tree: r.issue.Tree, Role: claim.RoleImplementer, Phase: phase.Implementing,
		Entry: 3, Task: "Build the manual.", Generation: generation, Agent: agent}, time.Now())
	row.ID = id
	return row
}

// launches is how many spawns and resumes the runtime has made so far.
func (r *agentStartRig) launches() (spawns, resumes int) {
	return len(r.rt.CallsOf("Spawn")), len(r.rt.CallsOf("Resume"))
}

// The start of a step whose agent is another than the one whose session its claim records
// (supervise.Claim.SessionAgent) starts the claim in a new session: a suspended claim is resumed
// by rule with that session left behind, so the runtime spawns the new agent rather than resuming
// the other agent's session, and a claim that still runs the other agent's process is suspended
// first and then started the same way. The claim then records the new agent as the agent of its
// session. The control, a start of the agent whose session it holds, resumes that session. The
// fault: a new agent on a held name continues the session of the agent that held it before.
func TestAStartForAnotherAgentStartsTheClaimInANewSession(t *testing.T) {
	for _, tc := range []struct {
		name      string
		agent     stepplan.AgentID
		suspended bool
		fresh     bool
	}{
		{name: "a suspended claim", agent: laterAgent, suspended: true, fresh: true},
		{name: "a claim that runs the other agent's process", agent: laterAgent, fresh: true},
		{name: "control: a start of the agent whose session the claim holds", agent: earlierAgent, suspended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAgentStartRig(t, tc.suspended)
			spawns, resumes := r.launches()

			if err := r.runner.execute(r.ctx, r.start(81, tc.agent, 1)); err != nil {
				t.Fatalf("the start = %v, want it to start the claim", err)
			}
			nowSpawns, nowResumes := r.launches()
			spawned, resumed := nowSpawns-spawns, nowResumes-resumes
			if got := r.machine.Claim().SessionAgent; got != tc.agent {
				t.Errorf("the claim records the agent %q of its session after the start for %s, want %s", got, tc.agent, tc.agent)
			}
			if !tc.fresh {
				if resumed != 1 || spawned != 0 {
					t.Fatalf("the start asked %d spawns and %d resumes, want the recorded session resumed once", spawned, resumed)
				}
				return
			}
			if spawned != 1 || resumed != 0 {
				t.Errorf("the start asked %d spawns and %d resumes, want one spawn of a new session", spawned, resumed)
			}
			if got := r.machine.Claim().Session; got == "ses-earlier" {
				t.Errorf("the claim still records the session %s, want the earlier agent's session forgotten", got)
			}
		})
	}
}

// A start row runs again after it acted — a settle or a ready that failed, a crash before the row
// finished — and a run of the row that started a new agent in a new session finds the claim's
// session the new agent's (supervise.Claim.SessionAgent), so it asks nothing of the claim: it does
// not suspend the new agent's process and does not forget its session. The fault, the control: a
// run that read only the row's own ask for a fresh session suspended the new agent and forgot its
// session, so a third session started.
func TestAStartRunAgainAfterItsFreshLaunchKeepsTheNewSession(t *testing.T) {
	r := newAgentStartRig(t, true)
	start := r.start(81, laterAgent, 1)
	if err := r.runner.execute(r.ctx, start); err != nil {
		t.Fatalf("the start = %v, want it to start the claim in a new session", err)
	}
	r.comesUp("ses-later")
	spawns, resumes := r.launches()

	if err := r.runner.execute(r.ctx, start); err != nil {
		t.Fatalf("the start run again = %v, want it to finish with the claim as it stands", err)
	}

	got := r.machine.Claim()
	if got.State != supervise.StateReady || got.Session != "ses-later" || got.SessionAgent != laterAgent {
		t.Fatalf("after the row ran again the claim is %s in session %q of agent %q, want it ready in ses-later, %s's",
			got.State, got.Session, got.SessionAgent, laterAgent)
	}
	if nowSpawns, nowResumes := r.launches(); nowSpawns != spawns || nowResumes != resumes {
		t.Fatalf("the row run again made %d spawns and %d resumes, want none", nowSpawns-spawns, nowResumes-resumes)
	}
}

// A start for a new agent that finished without acting — here one of a run the issue has left,
// which the generation fence finishes — leaves the claim as it stood: its session is still the
// other agent's, and says so (supervise.Claim.SessionAgent), so the next start of the new agent's
// step starts it in a new session. The fault: the record of whose session the claim holds moved
// at a start that started nothing, so the next start resumed the other agent's session.
func TestAFreshStartThatFinishedWithoutActingLeavesTheNextStartFresh(t *testing.T) {
	r := newAgentStartRig(t, true)
	spawns, resumes := r.launches()

	if err := r.runner.execute(r.ctx, r.start(81, laterAgent, 2)); err != nil {
		t.Fatalf("the start of a run the issue has left = %v, want it finished without acting", err)
	}
	if got := r.machine.Claim(); got.Session != "ses-earlier" || got.SessionAgent != earlierAgent || got.State != supervise.StateSuspended {
		t.Fatalf("after the start that finished without acting the claim is %s in session %q of agent %q, want it suspended in ses-earlier, %s's",
			got.State, got.Session, got.SessionAgent, earlierAgent)
	}
	if nowSpawns, nowResumes := r.launches(); nowSpawns != spawns || nowResumes != resumes {
		t.Fatalf("the start that finished without acting made %d spawns and %d resumes, want none", nowSpawns-spawns, nowResumes-resumes)
	}

	if err := r.runner.execute(r.ctx, r.start(82, laterAgent, 1)); err != nil {
		t.Fatalf("the next start = %v, want it to start the claim", err)
	}
	if nowSpawns, nowResumes := r.launches(); nowSpawns-spawns != 1 || nowResumes != resumes {
		t.Fatalf("the next start of the new agent made %d spawns and %d resumes, want one spawn of a new session", nowSpawns-spawns, nowResumes-resumes)
	}
	if got := r.machine.Claim(); got.Session == "ses-earlier" || got.SessionAgent != laterAgent {
		t.Fatalf("after the next start the claim records session %q of agent %q, want the earlier session left and %s recorded", got.Session, got.SessionAgent, laterAgent)
	}
}

// A claim that records no agent — one a release before the record ran, still running its process
// at the upgrade — takes the agent of the first start that reaches it as the agent of its session,
// and the start's task goes to its running process, as the release before had it. A later start
// of another agent then finds the session that first agent's, and starts its own agent in a new
// session, as for any claim whose session is another agent's. The fault: the record was written
// only where a start brought the claim back, so while the claim kept running every agent whose
// start reached it shared its session. The control: the second start of the first agent keeps the
// session.
func TestAClaimThatRecordsNoAgentTakesTheFirstStartsAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent stepplan.AgentID
		task  string
		fresh bool
	}{
		{name: "a second start of another agent", agent: laterAgent, task: "Build the index.", fresh: true},
		// The first agent's process still holds the first task, so its second start here carries
		// none, as a re-admission's start of a stored claim does: it reaches the same claim.
		{name: "control: a second start of the first agent", agent: earlierAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAgentStartRigOf(t, false, "")
			spawns, resumes := r.launches()

			if err := r.runner.execute(r.ctx, r.start(81, earlierAgent, 1)); err != nil {
				t.Fatalf("the first start = %v, want its task given to the running claim", err)
			}
			if got := r.machine.Claim(); got.SessionAgent != earlierAgent || got.Session != "ses-earlier" {
				t.Fatalf("after the first start the claim records session %q of agent %q, want ses-earlier, %s's", got.Session, got.SessionAgent, earlierAgent)
			}
			if nowSpawns, nowResumes := r.launches(); nowSpawns != spawns || nowResumes != resumes {
				t.Fatalf("the first start made %d spawns and %d resumes, want its task given to the running process", nowSpawns-spawns, nowResumes-resumes)
			}

			second := mustOutboxRow(t, r.issue.Key, record.SuperviseRequest{Op: "start", Tree: r.issue.Tree, Role: claim.RoleImplementer,
				Phase: phase.Implementing, Entry: 4, Task: tc.task, Generation: 1, Agent: tc.agent}, time.Now())
			second.ID = 82
			if err := r.runner.execute(r.ctx, second); err != nil {
				t.Fatalf("the second start = %v, want it to start the claim", err)
			}
			nowSpawns, nowResumes := r.launches()
			got := r.machine.Claim()
			if tc.fresh {
				if nowSpawns-spawns != 1 || nowResumes != resumes || got.Session == "ses-earlier" || got.SessionAgent != tc.agent {
					t.Fatalf("the start of %s made %d spawns and %d resumes, the claim in session %q of agent %q; want one spawn of a new session, %s's",
						tc.agent, nowSpawns-spawns, nowResumes-resumes, got.Session, got.SessionAgent, tc.agent)
				}
				return
			}
			if nowSpawns != spawns || got.Session != "ses-earlier" || got.SessionAgent != earlierAgent {
				t.Fatalf("the second start of %s made %d spawns, the claim in session %q of agent %q; want the session ses-earlier kept",
					tc.agent, nowSpawns-spawns, got.Session, got.SessionAgent)
			}
		})
	}
}

// A running claim that records no agent takes a start's agent only where the start's step still
// stands, judged under the claim's lock (supervise.Standing, stepStands): a later entry of the role
// that commits between the start's fences and that lock — written here by a hook at the start's own
// record of the claim (supervise.Machine.StartedBy), which runs after the fences and before the
// lock is taken again — leaves the claim recording no agent, and the start finishes without acting.
// The next start, of the later entry, then takes the session for its agent. The fault: the stale
// start's agent recorded on the claim, so that next start found another agent's session and
// started its agent in a new one. The control, with no entry written by the hook: the start records
// its agent, so the judgment refuses no start whose step stands.
func TestAStaleStartRecordsNoAgentOnARunningClaimThatRecordsNone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		moves bool
	}{
		{name: "a later entry commits before the claim's lock", moves: true},
		{name: "control: no entry commits", moves: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAgentStartRigOf(t, false, "")
			claims := r.runner.supervisor.deps.Store.(*outboxClaimStore)
			var later sync.Once
			claims.afterPut(func(c supervise.Claim) {
				if !tc.moves || c.LastStartRow != 81 {
					return
				}
				later.Do(func() {
					putPhaseRow(t, r.runner.pool, r.runner.records, record.PhaseRow{Issue: r.issue.Key, Role: claim.RoleImplementer, Claim: r.token,
						StepTask: "p", StepPlace: 1, Entry: 4})
				})
			})
			spawns, resumes := r.launches()

			if err := r.runner.execute(r.ctx, r.start(81, laterAgent, 1)); err != nil {
				t.Fatalf("the start = %v, want it finished or its task given to the running claim", err)
			}
			claims.afterPut(nil)
			got := r.machine.Claim()
			if nowSpawns, nowResumes := r.launches(); nowSpawns != spawns || nowResumes != resumes || got.Session != "ses-earlier" {
				t.Fatalf("the start made %d spawns and %d resumes, the claim in session %q; want the running process kept in ses-earlier",
					nowSpawns-spawns, nowResumes-resumes, got.Session)
			}
			if !tc.moves {
				if got.SessionAgent != laterAgent {
					t.Fatalf("the start whose step stands left the claim recording the agent %q, want %s", got.SessionAgent, laterAgent)
				}
				return
			}
			if got.SessionAgent != "" {
				t.Fatalf("the start whose step a later entry left recorded the agent %q of the claim's session, want none", got.SessionAgent)
			}

			next := mustOutboxRow(t, r.issue.Key, record.SuperviseRequest{Op: "start", Tree: r.issue.Tree, Role: claim.RoleImplementer,
				Phase: phase.Implementing, Entry: 4, Task: "Build the index.", Generation: 1, Agent: earlierAgent}, time.Now())
			next.ID = 82
			if err := r.runner.execute(r.ctx, next); err != nil {
				t.Fatalf("the start of the later entry = %v, want its task given to the running claim", err)
			}
			got = r.machine.Claim()
			if nowSpawns, nowResumes := r.launches(); nowSpawns != spawns || nowResumes != resumes || got.Session != "ses-earlier" || got.SessionAgent != earlierAgent {
				t.Fatalf("the start of the later entry made %d spawns and %d resumes, the claim in session %q of agent %q; want ses-earlier kept, %s's",
					nowSpawns-spawns, nowResumes-resumes, got.Session, got.SessionAgent, earlierAgent)
			}
		})
	}
}

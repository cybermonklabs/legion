package daemon

// The daemon's half of the stall backstop: the pass over the claims the orphan sweep already
// read, which ends the turn of every claim whose recorded last step is older than its role's
// bound and tells the owner once per stall. It is driven directly over a bare supervisor (no
// store, no HTTP), the construction plugin_test.go's boot cases use, because what it decides is
// decided from the claims it is handed and the configuration it holds.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// backstopBound is the bound the rig gives every role it tests, short enough to write out and
// long enough that no claim of the rig is stalled by accident.
const backstopBound = 4 * time.Hour

// backstopRig is a bare supervisor over the fake runtime, the supervision that sweeps it, and
// the outbox rows that supervision wrote. The rows are written from the machine's own report of
// the turn it ended, on a goroutine of its own (supervision.stallBackstopped), so everything a
// case reads of them is read under mu and waited for rather than assumed.
type backstopRig struct {
	t   *testing.T
	s   *supervision
	sup *supervisor
	rt  *fake.Runtime
	mu  sync.Mutex
	// rows are the outbox payloads supervision wrote, by issue; attempts how many writes it
	// tried, refused ones included, which is what a case waits on before it reads that none was
	// taken; refuse the error the store answers while it is set.
	rows     map[string][]record.OutboxPayload
	attempts int
	refuse   error
}

// newBackstopRig builds the rig with a bound for every role it drives but the merger, whose 0 is
// the deployment switching the backstop off for that role.
func newBackstopRig(t *testing.T) *backstopRig {
	t.Helper()
	sup, rt := newPluginSupervisor(t, api.DaemonAPIVersion, true)
	rig := &backstopRig{t: t, sup: sup, rt: rt, rows: map[string][]record.OutboxPayload{}}
	cfg := config.Config{
		Project: "legion",
		WorkerStallBackstops: map[claim.Role]time.Duration{
			claim.RoleImplementer: backstopBound,
			claim.RoleTester:      backstopBound,
			claim.RolePlanner:     backstopBound,
			claim.RoleController:  backstopBound,
			claim.RoleMerger:      0,
		},
	}
	rig.s = &supervision{
		cfg: cfg, log: quietLogger(), supervisor: sup, plan: plan{clock: stillClock{}},
		enqueue: func(_ context.Context, issue string, payloads ...record.OutboxPayload) error {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.attempts++
			if rig.refuse != nil {
				return rig.refuse
			}
			rig.rows[issue] = append(rig.rows[issue], payloads...)
			return nil
		},
	}
	// The machines tell supervision what each stall backstop ended, as the daemon's own boot
	// wires them (openSupervision): nothing else writes the owner's notice.
	sup.deps.StallBackstopped = rig.s.stallBackstopped
	return rig
}

// refusing sets, or clears, the error the rig's store answers every write with.
func (r *backstopRig) refusing(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refuse = err
}

// awaitAttempts waits for supervision to have tried want writes, taken or refused: what a case
// that must see no row taken waits on, since the write is made off the sweep.
func (r *backstopRig) awaitAttempts(want int) {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("supervision to have tried %d notice writes", want), func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.attempts >= want
	})
}

// attemptsMade is how many notice writes supervision has tried, taken or refused.
func (r *backstopRig) attemptsMade() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts
}

// claim builds a claim of role, working a task, whose record says its agent last stepped silent
// ago; call says whether that record names a call the agent is in, a claim with none carrying
// its turn's start alone. It is handed to the supervisor as the boot's restore hands it.
func (r *backstopRig) claim(role claim.Role, issue string, silent time.Duration, call bool) supervise.Claim {
	r.t.Helper()
	project, err := claim.ProjectToken("legion")
	if err != nil {
		r.t.Fatal(err)
	}
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	now := time.Now()
	at := now.Add(-silent)
	c := supervise.Claim{
		Token: token, Project: project, Tree: issue, Issue: issue, Role: role, Generation: 1,
		State: supervise.StateWorking, Session: "ses_" + string(role), SessionFile: "/sessions/" + string(role) + ".jsonl",
		PluginContract: api.DaemonAPIVersion, Locator: pluginTestLocator(token),
		Pending: &supervise.Delivery{ID: "delivery-" + string(role), Task: "work the phase",
			QueuedAt: at.Add(-time.Hour), DeliveredAt: at.Add(-time.Hour), ConfirmedAt: at.Add(-time.Hour)},
		Step: supervise.StepRecord{TurnStartedAt: at},
	}
	if call {
		c.Step.At, c.Step.StartedAt = at, at
		c.Step.TurnStartedAt = at.Add(-time.Hour)
		c.Step.Tool, c.Step.CallID, c.Step.ArgsBytes, c.Step.ArgsHash = "bash", "toolu_impl_11", 128, "9f2b7c0d1e3a4b5c"
	}
	return c
}

// supervising hands the claims to the supervisor, as the boot's restore does, and returns them
// for the sweep to read.
func (r *backstopRig) supervising(claims ...supervise.Claim) []supervise.Claim {
	r.t.Helper()
	if _, err := r.sup.restore(context.Background(), claims); err != nil {
		r.t.Fatalf("restore: %v", err)
	}
	return claims
}

// sweep runs one orphan-sweep pass of the backstop. What it posts is decided by each claim's own
// machine, on its own goroutine, so a case reads the outcome through awaitResumed or settled.
func (r *backstopRig) sweep(claims []supervise.Claim) {
	r.t.Helper()
	r.s.backstopStalledTurns(context.Background(), claims)
}

// settled waits for every event the sweep posted for c to have been decided: a tree close of its
// own, posted through the same inbox and so behind whatever the sweep put there, which the
// machine answers by retiring the claim. A case that must see nothing happen reads its
// assertions after this.
func (r *backstopRig) settled(c supervise.Claim) {
	r.t.Helper()
	r.sup.post(c.Token, supervise.RequestTreeClose{Claim: c.Token})
	testwait.Eventually(r.t, "the claim to answer a close posted behind the sweep's own events", func() bool {
		m, ok := r.sup.Machine(c.Token)
		return ok && m.Claim().State == supervise.StateRetired
	})
}

// awaitResumed waits for the backstop's relaunch of token's session.
func (r *backstopRig) awaitResumed(token claim.Token, want int) {
	r.t.Helper()
	testwait.Eventually(r.t, "the stalled claim's session to be brought back", func() bool {
		return len(r.resumes(token)) == want
	})
}

// notices are the notice rows supervision wrote on issue.
func (r *backstopRig) notices(issue string) []record.Notice {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var notices []record.Notice
	for _, payload := range r.rows[issue] {
		if notice, ok := payload.(record.Notice); ok {
			notices = append(notices, notice)
		}
	}
	return notices
}

// written are the rows of every kind supervision wrote on issue.
func (r *backstopRig) written(issue string) []record.OutboxPayload {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.rows[issue])
}

// awaitNotices waits for want notice rows on issue, which the machine's report writes off the
// sweep (supervision.stallBackstopped).
func (r *backstopRig) awaitNotices(issue string, want int) []record.Notice {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("%d notice rows on %s", want, issue), func() bool {
		return len(r.notices(issue)) == want
	})
	return r.notices(issue)
}

// resumes are the relaunches the runtime was asked to make for token.
func (r *backstopRig) resumes(token claim.Token) []fake.Call {
	r.t.Helper()
	var made []fake.Call
	for _, call := range r.rt.CallsOf("Resume") {
		if call.Spec.Claim == token {
			made = append(made, call)
		}
	}
	return made
}

// A claim whose recorded last step is older than its role's bound has its turn ended by the
// daemon — its session resumed on a new process, with the task it was serving still owed — and
// its owner is told once, with the record in the notice: how long the agent was silent, the
// bound it passed, when the turn began and the call the record names. The arguments of that call
// are in no part of the notice, as they are in no column.
func TestTheStallBackstopEndsAStalledTurnAndTellsTheOwnerOnce(t *testing.T) {
	rig := newBackstopRig(t)
	stalled := rig.claim(claim.RoleImplementer, "LEGION-209", backstopBound+30*time.Minute, true)
	claims := rig.supervising(stalled)

	rig.sweep(claims)

	rig.awaitResumed(stalled.Token, 1)
	m, ok := rig.sup.Machine(stalled.Token)
	if !ok {
		t.Fatal("the stalled claim is not supervised")
	}
	if held := m.Claim(); held.Pending == nil || !held.Pending.Interrupted {
		t.Fatalf("the claim's pending task after the backstop = %+v, want the task it was serving still owed", held.Pending)
	}
	if held := m.Claim(); held.Session != "ses_implementer" || held.Budgets != (supervise.Budgets{}) {
		t.Fatalf("the claim after the backstop = %+v, want the same session and nothing charged", held)
	}
	notices := rig.awaitNotices("LEGION-209", 1)
	notice := notices[0]
	if notice.Kind != "worker-stalled" || notice.Role != claim.RoleImplementer {
		t.Fatalf("the notice = %+v, want a worker-stalled of the implementer", notice)
	}
	for _, want := range []string{
		"the daemon ended the implementer's turn",
		"4h30m0s ago",
		"past the 4h0m0s this deployment gives the role",
		"bash (toolu_impl_11)",
		"has not ended",
		"still owed",
		"its session was brought back on a new process",
	} {
		if !strings.Contains(notice.Reason, want) {
			t.Errorf("the notice says %q, which does not carry %q", notice.Reason, want)
		}
	}
	if strings.Contains(notice.Reason, "9f2b7c0d1e3a4b5c") {
		t.Errorf("the notice carries the call's fingerprint: %q", notice.Reason)
	}

	// The next sweep reads the same stall — this snapshot is what a stop that changed nothing
	// would leave — and posts the backstop again. The claim is no longer in that turn, so
	// nothing ends and nobody is told again: the owner hears of a stall once, because only the
	// end a machine made is told at all.
	rig.sweep(claims)
	rig.settled(stalled)
	if notices := rig.notices("LEGION-209"); len(notices) != 1 {
		t.Fatalf("notices after the second sweep = %+v, want the owner told once per stall", notices)
	}
}

// A claim whose agent has sent nothing since its turn began is judged from that turn's start,
// which is the only moment its record holds.
func TestTheStallBackstopJudgesAClaimWithNoCallFromItsTurnsStart(t *testing.T) {
	rig := newBackstopRig(t)
	stalled := rig.claim(claim.RoleTester, "LEGION-210", backstopBound+time.Minute, false)
	claims := rig.supervising(stalled)

	rig.sweep(claims)

	rig.awaitResumed(stalled.Token, 1)
	notices := rig.awaitNotices("LEGION-210", 1)
	if !strings.Contains(notices[0].Reason, "the daemon saw no call of this turn") {
		t.Fatalf("notices = %+v, want one naming a turn with no call the daemon saw", notices)
	}
}

// What the pass never touches, each for its own reason. The negative control of the whole rule
// is the first two: an agent that is sending frames is never touched however long its turn runs,
// and a role whose bound is 0 is never touched by this rule at all.
func TestTheStallBackstopLeavesTheseClaimsAlone(t *testing.T) {
	for name, build := range map[string]func(*backstopRig) supervise.Claim{
		"an agent still stepping inside its bound": func(r *backstopRig) supervise.Claim {
			return r.claim(claim.RoleImplementer, "LEGION-211", backstopBound-time.Minute, true)
		},
		"a role whose bound is 0, however long its agent is silent": func(r *backstopRig) supervise.Claim {
			return r.claim(claim.RoleMerger, "LEGION-212", 50*backstopBound, true)
		},
		"a role whose bound is 0 and whose turn made no call at all": func(r *backstopRig) supervise.Claim {
			return r.claim(claim.RoleMerger, "LEGION-213", 50*backstopBound, false)
		},
		"a claim in no turn": func(r *backstopRig) supervise.Claim {
			c := r.claim(claim.RoleImplementer, "LEGION-214", backstopBound+time.Hour, true)
			c.State = supervise.StateIdle
			return c
		},
		"a claim an update round is carrying": func(r *backstopRig) supervise.Claim {
			c := r.claim(claim.RoleImplementer, "LEGION-215", backstopBound+time.Hour, true)
			c.UpdateRound, c.UpdateReasons = supervise.PodRound("9d3f0c7a"), "image moved"
			return c
		},
		"a claim whose record holds nothing at all": func(r *backstopRig) supervise.Claim {
			c := r.claim(claim.RoleImplementer, "LEGION-216", backstopBound+time.Hour, true)
			c.Step = supervise.StepRecord{}
			return c
		},
		"a role the configuration names no bound for, inside the ladder it defaults to": func(r *backstopRig) supervise.Claim {
			return r.claim(claim.RoleReviewer, "LEGION-217", 3*time.Hour, true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newBackstopRig(t)
			left := build(rig)
			claims := rig.supervising(left)

			rig.sweep(claims)
			rig.settled(left)

			if resumes := rig.resumes(left.Token); len(resumes) != 0 {
				t.Fatalf("resumes = %d, want the claim left alone", len(resumes))
			}
			if notices := rig.notices(left.Issue); len(notices) != 0 {
				t.Fatalf("notices = %+v, want nobody told", notices)
			}
		})
	}
}

// The project controller's claim is on no issue, so there is no owner to tell: its turn is ended
// like any other, and the record is in the line the daemon logs.
func TestTheStallBackstopEndsAStalledControllerAndWritesOnNoIssue(t *testing.T) {
	rig := newBackstopRig(t)
	project, err := claim.ProjectToken("legion")
	if err != nil {
		t.Fatal(err)
	}
	controller := rig.claim(claim.RolePlanner, "LEGION-218", backstopBound+time.Hour, true)
	controller.Token = claim.ControllerToken(project)
	controller.Role, controller.Issue, controller.Tree = claim.RoleController, "", ""
	controller.Locator = pluginTestLocator(controller.Token)
	claims := rig.supervising(controller)

	rig.sweep(claims)

	rig.awaitResumed(controller.Token, 1)
	rig.settled(controller)
	if rows := rig.written(""); len(rows) != 0 {
		t.Fatalf("rows written on no issue = %+v, want none: a controller's claim has no owner to tell", rows)
	}
	if rig.attemptsMade() != 0 {
		t.Fatalf("supervision tried %d writes, want none: a controller's claim is on no issue", rig.attemptsMade())
	}
}

// A notice the store refuses is told again at the next sweep, as a stalled update round's is:
// the row is kept until one is taken, and the machine's end is reported once however often it
// is written.
func TestTheStallBackstopTellsTheOwnerAgainWhenTheRowIsRefused(t *testing.T) {
	rig := newBackstopRig(t)
	rig.refusing(errors.New("the store is down"))
	stalled := rig.claim(claim.RolePlanner, "LEGION-219", backstopBound+time.Hour, true)
	claims := rig.supervising(stalled)

	rig.sweep(claims)
	rig.awaitResumed(stalled.Token, 1)
	rig.awaitAttempts(1)
	if notices := rig.notices("LEGION-219"); len(notices) != 0 {
		t.Fatalf("notices = %+v, want none taken", notices)
	}

	rig.refusing(nil)
	rig.sweep(claims)
	rig.settled(stalled)

	if notices := rig.notices("LEGION-219"); len(notices) != 1 {
		t.Fatalf("notices after the refusal cleared = %+v, want the owner told then", notices)
	}
}

// A tree architect's own stall is told to the controller as well, in a row of its own: every
// other notice of the tree reaches that architect, so nobody outside it would hear of the turn
// the daemon ended. A worker's stall is its architect's alone.
func TestTheStallBackstopTellsTheControllerOfATreeArchitectsStall(t *testing.T) {
	rig := newBackstopRig(t)
	root := rig.claim(claim.RolePlanner, "LEGION-220", backstopBound+time.Hour, true)
	root.Role = claim.RoleArchitect
	rig.s.cfg.WorkerStallBackstops[claim.RoleArchitect] = backstopBound
	claims := rig.supervising(root)

	rig.sweep(claims)

	rig.awaitResumed(root.Token, 1)
	rig.awaitNotices("LEGION-220", 1)
	rows := rig.written("LEGION-220")
	if len(rows) != 2 {
		t.Fatalf("rows on the root issue = %+v, want the architect's notice and the controller's", rows)
	}
	notice, isNotice := rows[0].(record.Notice)
	controllers, isController := rows[1].(record.ControllerNotice)
	if !isNotice || !isController || notice.Kind != "worker-stalled" {
		t.Fatalf("rows = %+v, want a worker-stalled notice and the controller's copy of it", rows)
	}
	if !reflect.DeepEqual(record.Notice(controllers), notice) {
		t.Fatalf("the controller's copy = %+v, want the architect's own %+v", controllers, notice)
	}

	// A worker of the same tree tells the architect alone.
	worker := rig.claim(claim.RoleTester, "LEGION-220", backstopBound+time.Hour, true)
	rig.sweep(rig.supervising(worker))
	rig.settled(worker)
	rig.awaitNotices("LEGION-220", 2)
	if rows := rig.written("LEGION-220"); len(rows) != 3 {
		t.Fatalf("rows after the worker's stall = %+v, want one more, the architect's alone", rows)
	}
}

// The owner is told what happened, never what the sweep asked for. The sweep reads the claims
// before any machine decides, so a claim whose agent moved in between is one whose turn the
// machine's own fence leaves alone (supervise's backstopStalledTurn): nothing was ended, and a
// notice saying the turn was ended and the session brought back would send its architect
// looking for a recovery that never happened.
func TestTheStallBackstopTellsNobodyOfATurnItDidNotEnd(t *testing.T) {
	rig := newBackstopRig(t)
	moving := rig.claim(claim.RoleImplementer, "LEGION-221", time.Minute, true)
	rig.supervising(moving)

	// What the sweep read, one tick out of date: the record as it stood before the agent's
	// steps of the last few hours reached the daemon.
	swept := moving
	swept.Step.At = swept.Step.At.Add(-backstopBound - time.Hour)
	swept.Step.TurnStartedAt = swept.Step.TurnStartedAt.Add(-backstopBound - time.Hour)
	rig.sweep([]supervise.Claim{swept})
	rig.settled(moving)

	if resumes := rig.resumes(moving.Token); len(resumes) != 0 {
		t.Fatalf("resumes = %d, want the turn left alone: the claim's own record is inside its bound", len(resumes))
	}
	if got := rig.attemptsMade(); got != 0 {
		t.Fatalf("supervision tried %d notice writes, want none: no turn was ended", got)
	}
	if notices := rig.notices("LEGION-221"); len(notices) != 0 {
		t.Fatalf("notices = %+v, want nobody told of a turn that was not ended", notices)
	}
}

// A turn a held suspension ended is told as that. The machine ends such a turn in a suspension
// rather than a relaunch (replaceProcess, endHeld), so the notice names the suspension and says
// nothing runs for the claim until it is resumed.
func TestTheStallBackstopTellsASuspendedEndAsSuspended(t *testing.T) {
	rig := newBackstopRig(t)
	stalled := rig.claim(claim.RoleImplementer, "LEGION-222", backstopBound+time.Hour, true)
	claims := rig.supervising(stalled)
	m, ok := rig.sup.Machine(stalled.Token)
	if !ok {
		t.Fatal("the stalled claim is not supervised")
	}
	if err := m.Handle(context.Background(), supervise.RequestSuspend{Claim: stalled.Token,
		By: supervise.SuspendedByOperator}); !errors.Is(err, supervise.ErrSuspendHeld) {
		t.Fatalf("a suspension mid-turn = %v, want it held for the turn", err)
	}

	rig.sweep(claims)

	testwait.Eventually(t, "the held suspension to end the stalled turn", func() bool {
		return m.Claim().State == supervise.StateSuspended
	})
	notices := rig.awaitNotices("LEGION-222", 1)
	if resumes := rig.resumes(stalled.Token); len(resumes) != 0 {
		t.Fatalf("resumes = %d, want none: the held suspension ended the process rather than replacing it", len(resumes))
	}
	for _, want := range []string{"still owed", "a suspension that was already waiting for this turn ended the process"} {
		if !strings.Contains(notices[0].Reason, want) {
			t.Errorf("the notice says %q, which does not carry %q", notices[0].Reason, want)
		}
	}
	if strings.Contains(notices[0].Reason, "brought back") {
		t.Errorf("the notice says the session was brought back, and none was: %q", notices[0].Reason)
	}
}

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
	// taken; refuse the error the store answers while it is set. reports is how many ends the
	// machines told supervision of, counted inside the decision that made each: the row is
	// written on a goroutine of supervision's own (stallBackstopped), so a case that read rows
	// alone would read "nobody was told" of a machine that told and whose goroutine has not run
	// yet, and that assertion could never fail.
	rows     map[string][]record.OutboxPayload
	attempts int
	reports  int
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
	// wires them (openSupervision): nothing else writes the owner's notice. The rig counts each
	// report where the machine makes it, before passing it on, so a case that has waited for the
	// events it posted to be decided (settled, awaitResumed) has every report those events will
	// ever make.
	sup.deps.StallBackstopped = func(c supervise.Claim, stop supervise.StallBackstopStop) {
		rig.mu.Lock()
		rig.reports++
		rig.mu.Unlock()
		rig.s.stallBackstopped(c, stop)
	}
	return rig
}

// reported is how many ends the rig's machines have told supervision of, counted inside each
// machine's own decision rather than through the row it leads to.
func (r *backstopRig) reported() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reports
}

// claimStore is the claims store the rig's machines write through, for a case that drives a
// decision the store would not take. It is the rig's own fake, built with the supervisor
// (newPluginSupervisor).
func (r *backstopRig) claimStore() *outboxClaimStore {
	r.t.Helper()
	store, ok := r.sup.deps.Store.(*outboxClaimStore)
	if !ok {
		r.t.Fatalf("the rig's store is %T, want the claims fake the supervisor is built with", r.sup.deps.Store)
	}
	return store
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
		"the daemon ended the implementer's turn and started a new process in its place",
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
	if told := rig.reported(); told != 1 {
		t.Fatalf("the machines' reports = %d, want the owner told once per stall: the second sweep ended nothing", told)
	}
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
			if told := rig.reported(); told != 0 {
				t.Fatalf("the machines' reports = %d, want nobody told: this claim's turn is not the backstop's to end", told)
			}
			if notices := rig.notices(left.Issue); len(notices) != 0 {
				t.Fatalf("notices = %+v, want nobody told", notices)
			}
		})
	}
}

// The project controller's claim is on no issue, so there is no owner to tell: its turn is ended
// like any other, and the record is in the line the daemon logs.
//
// What this case reads of the telling is the machine's own report, counted inside the decision
// that made it (reported): the write runs on a goroutine of supervision's own, so a check that
// no write was tried, read straight after the claim settled, would pass over a supervision that
// writes one a moment later. The rule that no write is tried at all is read where it is
// decided, in the call itself (TestTheStallBackstopWritesNothingForAClaimOnNoIssue).
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
	if told := rig.reported(); told != 1 {
		t.Fatalf("the machines' reports = %d, want the one end the backstop made: the controller's turn is ended like any other", told)
	}
}

// The no-issue rule itself, read where it is decided rather than through a goroutine: a claim
// with no issue has no owner to tell, so the call writes nothing and reports the owner told —
// a notice nobody is owed is not one to keep and try again at the next sweep (oweStallNotice).
func TestTheStallBackstopWritesNothingForAClaimOnNoIssue(t *testing.T) {
	rig := newBackstopRig(t)
	project, err := claim.ProjectToken("legion")
	if err != nil {
		t.Fatal(err)
	}
	controller := rig.claim(claim.RolePlanner, "LEGION-218", backstopBound+time.Hour, true)
	controller.Token = claim.ControllerToken(project)
	controller.Role, controller.Issue, controller.Tree = claim.RoleController, "", ""
	stop := supervise.StallBackstopStop{Step: controller.Step, LastStep: controller.Step.LastStep(),
		Bound: backstopBound, End: supervise.EndRelaunched}

	if told := rig.s.tellStallBackstopped(context.Background(), controller, stop); !told {
		t.Fatal("the notice of a claim on no issue reports the owner untold, want it told: there is nobody to tell, and keeping it would retry it at every sweep")
	}

	if tried := rig.attemptsMade(); tried != 0 {
		t.Fatalf("supervision tried %d writes, want none: a controller's claim is on no issue", tried)
	}
	if rows := rig.written(""); len(rows) != 0 {
		t.Fatalf("rows written on no issue = %+v, want none", rows)
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

	if told := rig.reported(); told != 1 {
		t.Fatalf("the machines' reports = %d, want the one end told however often its row is written", told)
	}
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
	if told := rig.reported(); told != 0 {
		t.Fatalf("the machines' reports = %d, want none: no turn was ended", told)
	}
	if notices := rig.notices("LEGION-221"); len(notices) != 0 {
		t.Fatalf("notices = %+v, want nobody told of a turn that was not ended", notices)
	}
}

// A turn a held suspension ended is told as that. The machine ends such a turn in a suspension
// rather than a relaunch (replaceProcess, endHeld), so the notice names the suspension, says
// the claim is suspended with nothing queued for it, and names what it waits on — a resume, or
// the close of its tree — rather than promising a resume nobody has asked for.
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
	for _, want := range []string{"still owed", "a suspension that was already waiting for this turn ran instead of a replacement",
		"the claim is suspended with nothing queued for it", "what it waits on is a resume, or the close of its tree, which retires it",
		stallProcessStopped} {
		if !strings.Contains(notices[0].Reason, want) {
			t.Errorf("the notice says %q, which does not carry %q", notices[0].Reason, want)
		}
	}
	if strings.Contains(notices[0].Reason, "brought back") {
		t.Errorf("the notice says the session was brought back, and none was: %q", notices[0].Reason)
	}
}

// A turn the daemon ended whose replacement the store would not write reaches its owner all the
// same, and the notice says what is true of it: the worker's process was stopped, the launch
// that replaces it was not written, and the daemon tries that launch again at its probe
// interval (supervise's launchNow, relaunchLater) — with the ends that can come instead named,
// since a retry can be refused for a reserved cleanup, meet a suspension that arrived
// meanwhile, or run the claim's launch budget out (relaunchRefused, relaunchAgain, fail). It is
// the store's transient refusal that this exists for — the one a sweep sees often enough to
// have been measured — so a stall that falls in it is a worker stopped and an owner who would
// otherwise hear nothing at all.
func TestTheStallBackstopTellsTheOwnerOfAnEndTheStoreWouldNotWrite(t *testing.T) {
	rig := newBackstopRig(t)
	stalled := rig.claim(claim.RoleImplementer, "LEGION-223", backstopBound+time.Hour, true)
	claims := rig.supervising(stalled)
	rig.claimStore().refuse(errors.New("the store is down"))

	rig.sweep(claims)

	notices := rig.awaitNotices("LEGION-223", 1)
	if told := rig.reported(); told != 1 {
		t.Fatalf("the machines' reports = %d, want the one end the backstop made", told)
	}
	if resumes := rig.resumes(stalled.Token); len(resumes) != 0 {
		t.Fatalf("resumes = %d, want none: the launch that replaces the stopped process was never written", len(resumes))
	}
	m, ok := rig.sup.Machine(stalled.Token)
	if !ok {
		t.Fatal("the stalled claim is not supervised")
	}
	if c := m.Claim(); c.State != supervise.StateLaunching || c.Locator != nil {
		t.Fatalf("the claim is %s with locator %+v, want it launching with the stalled process let go", c.State, c.Locator)
	}
	for _, want := range []string{"still owed", "the daemon ended the implementer's turn and let its stalled process go",
		"the launch that replaces that process has not started",
		"either its write was refused, which the daemon tries again at its probe interval",
		"or the daemon's own stop cut the launch short, which the daemon's next start takes up",
		"leaves it suspended or failed with nothing started for it instead", "nothing was charged to the claim",
		stallProcessStopped} {
		if !strings.Contains(notices[0].Reason, want) {
			t.Errorf("the notice says %q, which does not carry %q", notices[0].Reason, want)
		}
	}
	for _, never := range []string{"brought back", "nothing runs for the claim", "could not be written"} {
		if strings.Contains(notices[0].Reason, never) {
			t.Errorf("the notice says %q, which claims %q: the write of this launch landed where the daemon's own stop cut it short, and the sentence about the process is the stop clause's", notices[0].Reason, never)
		}
	}
}

// And the same end over a runtime that would not stop the process. The claim is suspended all
// the same (supervise's endHeld), so the machine goes on with a process nobody has seen end,
// and the owner is told that rather than that it was stopped: a pod the runtime still runs goes
// on holding its node's room and its disk, which is the half of this the owner acts on.
func TestTheStallBackstopTellsAStopTheRuntimeWouldNotMake(t *testing.T) {
	rig := newBackstopRig(t)
	stalled := rig.claim(claim.RoleImplementer, "LEGION-227", backstopBound+time.Hour, true)
	claims := rig.supervising(stalled)
	m, ok := rig.sup.Machine(stalled.Token)
	if !ok {
		t.Fatal("the stalled claim is not supervised")
	}
	if err := m.Handle(context.Background(), supervise.RequestSuspend{Claim: stalled.Token,
		By: supervise.SuspendedByOperator}); !errors.Is(err, supervise.ErrSuspendHeld) {
		t.Fatalf("a suspension mid-turn = %v, want it held for the turn", err)
	}
	rig.rt.FailSuspend(errors.New("the pod would not stop"))

	rig.sweep(claims)

	testwait.Eventually(t, "the held suspension to end the stalled turn", func() bool {
		return m.Claim().State == supervise.StateSuspended
	})
	notices := rig.awaitNotices("LEGION-227", 1)
	if !strings.Contains(notices[0].Reason, stallStopRefused) {
		t.Errorf("the notice says %q, which does not carry %q", notices[0].Reason, stallStopRefused)
	}
	if strings.Contains(notices[0].Reason, stallProcessStopped) {
		t.Errorf("the notice says %q, and the runtime refused that stop: the process may be running still", notices[0].Reason)
	}
}

// And a turn whose replacement the runtime refused until the claim's launch budget was gone:
// the claim has failed, nothing relaunches it, and the notice says so rather than leaving its
// owner waiting for a session that is never coming back. The launch failures that end is made
// of are the one thing a stall does charge, so the notice names them instead of the sentence
// every other end carries.
func TestTheStallBackstopTellsTheOwnerOfAnEndThatFailedTheClaim(t *testing.T) {
	rig := newBackstopRig(t)
	stalled := rig.claim(claim.RoleTester, "LEGION-224", backstopBound+time.Hour, true)
	claims := rig.supervising(stalled)
	refused := errors.New("the pod did not start")
	for range rig.sup.deps.Limits.LaunchFailures {
		rig.rt.ScriptResume(fake.SpawnResult{Err: refused})
	}

	rig.sweep(claims)

	notices := rig.awaitNotices("LEGION-224", 1)
	if told := rig.reported(); told != 1 {
		t.Fatalf("the machines' reports = %d, want the one end the backstop made", told)
	}
	m, ok := rig.sup.Machine(stalled.Token)
	if !ok {
		t.Fatal("the stalled claim is not supervised")
	}
	if c := m.Claim(); c.State != supervise.StateFailed {
		t.Fatalf("the claim is %s, want it failed: every try at the replacement was refused", c.State)
	}
	for _, want := range []string{"still owed", "every try at the replacement was refused",
		"the claim is failed and the daemon starts nothing for it",
		"what it waits on is a retry, or the close of its tree, which retires it",
		"the launch failures of the replacement that could not be started are what it was charged",
		stallProcessStopped} {
		if !strings.Contains(notices[0].Reason, want) {
			t.Errorf("the notice says %q, which does not carry %q", notices[0].Reason, want)
		}
	}
	if strings.Contains(notices[0].Reason, "nothing was charged to the claim") {
		t.Errorf("the notice says nothing was charged, and the launch failures were: %q", notices[0].Reason)
	}
}

// A replacement the wake pacing parked is the one end whose turn was not ended: the take-back
// has run, so the task is owed again, and the claim keeps the stalled process and keeps running
// it until the launch's turn (supervise's reserveWake, parkWake). The notice says exactly that
// much and no more. Its opening never says the turn was ended, because it was not — the opening
// cannot say it, since the daemon derives that clause from the end's own record of the process
// (stallTurnClause). Its clause says what the park settled and then names what can change it,
// as an open list rather than a closed one: a suspension that arrived while the launch waited
// ends the turn in a stop (supervise's wakeNow, endHeld), a lifecycle recheck refuses the
// turn's own launch and keeps the process (runWake, relaunchRefused), a tree close drops the
// wake with the process (letGo), the take-back the turn itself makes can be refused by the
// store (runWake), the launch can fail its own write or its start (launchNow, relaunchLater,
// fail), and an operator's task can run the launch before its turn (runParkedLaunchNow). None
// of them writes this owner a second notice. And the wait is the wake's own due time, which the
// machine carries on its report (supervise.StallBackstopStop.WakeDue), not a guess at how long
// a wave takes.
func TestTheStallBackstopTellsAParkedRelaunchAsWhatParkTimeSettles(t *testing.T) {
	rig := newBackstopRig(t)
	limiter := pacedWakes()
	rig.sup.deps.WakePace = supervise.WakePace{Reserve: limiter.Reserve, Rate: testWakeRate, Burst: testWakeBurst}
	// The burst's one token is spent before the sweep, so the backstop's own relaunch is parked
	// rather than run where it is raised (supervise's reserveWake).
	for range testWakeBurst {
		limiter.Reserve(time.Now())
	}
	stalled := rig.claim(claim.RoleImplementer, "LEGION-226", backstopBound+time.Hour, true)
	claims := rig.supervising(stalled)

	rig.sweep(claims)

	notices := rig.awaitNotices("LEGION-226", 1)
	if told := rig.reported(); told != 1 {
		t.Fatalf("the machines' reports = %d, want the one end the backstop made", told)
	}
	if resumes := rig.resumes(stalled.Token); len(resumes) != 0 {
		t.Fatalf("resumes = %d, want none: the launch that replaces the stalled process waits its turn", len(resumes))
	}
	m, ok := rig.sup.Machine(stalled.Token)
	if !ok {
		t.Fatal("the stalled claim is not supervised")
	}
	if c := m.Claim(); c.State != supervise.StateWorking || c.Locator == nil {
		t.Fatalf("the claim is %s with locator %+v, want it still running the stalled process until the launch's turn", c.State, c.Locator)
	}
	parked := m.View().WakeWaiting
	if parked == nil || parked.Kind != supervise.WakeLaunch {
		t.Fatalf("the claim holds wake %+v, want the backstop's launch waiting its turn", parked)
	}
	for _, want := range []string{
		"the daemon took back the implementer's task without ending its turn and queued the relaunch that replaces its stalled process",
		"still owed",
		"the launch that replaces its stalled process is queued in the daemon's wake pacing and is due at " + stallInstant(parked.At),
		"what the park settled is that launch queued and that process running on until its turn",
		"That turn may start nothing, or not the launch the park queued",
		"a suspension waiting for this claim",
		"the lifecycle recheck every launch makes refusing this one",
		"the tree's close dropping the queued launch",
		"a take-back the store refuses at the turn itself",
		"a launch whose own write or start the daemon cannot make",
		"an operator's task that runs this launch before its turn are among the things that can change it",
		"nothing was charged to the claim",
	} {
		if !strings.Contains(notices[0].Reason, want) {
			t.Errorf("the notice says %q, which does not carry %q", notices[0].Reason, want)
		}
	}
	for _, never := range []string{
		"the daemon ended the implementer's turn",
		"starts nothing at all if",
		"is stopped and replaced there",
		"tens of minutes",
		"brought back",
	} {
		if strings.Contains(notices[0].Reason, never) {
			t.Errorf("the notice says %q, which claims %q: the turn runs on, nothing is promised of the launch's turn, and the things that can change it are named as some of them rather than all", notices[0].Reason, never)
		}
	}
}

// The notice's wording walked by the machine's own classification rather than by the ends
// anyone remembered (supervise.ProcessEnds): every end that tells its owner has an opening and
// a sentence of its own, no end that tells nobody has either, and the relaunch is claimed by
// the one end that made it. A notice claiming a relaunch that did not happen is the defect this
// table's default clause had, and an opening claiming a turn that was not ended is the defect
// one opening shared by every end had, so all three clauses are total over the type and the
// default is unreachable for every end this daemon declares (stallBackstopEnds,
// stallBackstopEnd).
//
// Every end is rendered under both values of the one fact the machine records about the stop
// (supervise.StallBackstopStop.StopUnconfirmed), and both directions of two properties are
// held over every rendering, so neither is satisfied by a row that says nothing:
//
//   - an end that let the claim's process go says that the process was stopped under a
//     confirmed stop and that it may be running still under an unconfirmed one, and nowhere
//     under that unconfirmed one does any word of it call the process stopped; an end that
//     kept the process says neither sentence under either value, and calls that process
//     stopped nowhere;
//   - an end that let the process go says the turn was ended under a confirmed stop and says
//     so nowhere under an unconfirmed one; an end that kept the process never says it at all.
//
// Both are read over the whole notice rather than over the clauses the turn and the stalled
// process are derived into, because the derivation is what keeps a row from picking those
// words and this is what keeps a row from writing them anyway (stallTurnClause,
// stallStopClause, stallTurnEndedClaims, stallStopClaims).
//
// It catches an end added with no opening or no sentence, an end whose sentence claims the
// wrong thing, a clause whose values nothing fills in (stallFill), and a row left behind for an
// end that no longer exists. It cannot catch a sentence that is merely wrong about what the
// machine did: the cases above read each end's own notice for that.
func TestTheNoticeHasWordsForEveryEndThatTellsItsOwner(t *testing.T) {
	const relaunched = "its session was brought back on a new process"
	worded := map[supervise.ProcessEnd]bool{}
	for end := range stallBackstopEnds {
		worded[end] = true
	}
	for _, end := range supervise.ProcessEnds() {
		words, has := stallBackstopEnds[end]
		delete(worded, end)
		if !end.TellsOwner() {
			if has {
				t.Errorf("the notice has a sentence for %q, an end told to nobody: drop the row, or make the end tell its owner", end)
			}
			continue
		}
		if !has {
			t.Errorf("the notice has no sentence for the end %q: add a row to stallBackstopEnds saying what is running for the claim now and what its owner is waiting on", end)
			continue
		}
		if words.did == "" {
			t.Errorf("the notice's row for %q is %+v, want the end's own half of the opening: add a did to stallBackstopEnds saying what the daemon did to this claim beside what it did to the turn, which the notice derives (stallTurnClause)", end, words)
		}
		if words.end == "" || words.charged == "" {
			t.Errorf("the notice's row for %q is %+v, want both clauses: what became of the worker and what the claim was charged", end, words)
		}
		if claimed := strings.Contains(words.end, relaunched); claimed != (end == supervise.EndRelaunched) {
			t.Errorf("the sentence for %q is %q, which says %q: only %q brought a session back, and a notice that claims a relaunch nobody made sends an architect looking for a process that is not there",
				end, words.end, relaunched, supervise.EndRelaunched)
		}
		if words.process == stallFateUnsaid {
			t.Errorf("the notice's row for %q is %+v, want it to say what the end did with the claim's process: set process to stallProcessLetGo or stallProcessKept, since the sentence about that process is written from it and an end that lets one go must not reach its owner with nothing said about it", end, words)
		}
		stop := supervise.StallBackstopStop{Bound: backstopBound, End: end, WakeDue: time.Now()}
		refusedStop := supervise.StallBackstopStop{Bound: backstopBound, End: end, WakeDue: time.Now(), StopUnconfirmed: true}
		fill := stallFill(rigClaim(), stop)
		told := stallBackstopRecord(rigClaim(), stop, time.Hour)
		// The sentence about the stalled process and the opening's claim about the turn are
		// both written from the machine's record of the stop, so every end is read both ways:
		// the stop the runtime made, and the stop it refused, which leaves a process that may
		// be running the turn still.
		refused := stallBackstopRecord(rigClaim(), refusedStop, time.Hour)
		for clause, text := range map[string]string{"opening": words.opening(stop), "sentence": words.end, "charge clause": words.charged} {
			if !strings.Contains(told, fill.Replace(text)) {
				t.Errorf("the notice of %q reads %q, which does not carry its own %s", end, told, clause)
			}
		}
		if !strings.Contains(refused, fill.Replace(words.opening(refusedStop))) {
			t.Errorf("the notice of %q over a stop the runtime refused reads %q, which does not open with %q", end, refused, fill.Replace(words.opening(refusedStop)))
		}
		if strings.Contains(told, stallRoleWord) || strings.Contains(told, stallDueWord) {
			t.Errorf("the notice of %q reads %q, which still holds %s or %s: every value a clause names is filled in by stallFill", end, told, stallRoleWord, stallDueWord)
		}
		ended := stallTurnEndedClaims(rigClaim())
		stopped := stallStopClaims(rigClaim())
		switch words.process {
		case stallProcessLetGo:
			if !strings.Contains(told, stallProcessStopped) {
				t.Errorf("the notice of %q reads %q, which does not say %q: this end let the claim's process go", end, told, stallProcessStopped)
			}
			if !strings.Contains(refused, stallStopRefused) {
				t.Errorf("the notice of %q over a stop the runtime refused reads %q, which does not say %q: an owner told the process was stopped would leave a pod running", end, refused, stallStopRefused)
			}
			if said := saidFirst(refused, stopped); said != "" {
				t.Errorf("the notice of %q over a stop the runtime refused reads %q, which says %q all the same: nobody saw that process stop, so no word of this notice may call it stopped and an owner who reads that one leaves a pod running (stallStopClause)", end, refused, said)
			}
			if said := saidFirst(told, ended); said == "" {
				t.Errorf("the notice of %q over a stop the runtime made reads %q, which says nowhere that the turn was ended: this end let the claim's process go and the runtime confirmed the stop of it, so the opening is the end's claim that the turn is over", end, told)
			}
			if said := saidFirst(refused, ended); said != "" {
				t.Errorf("the notice of %q over a stop the runtime refused reads %q, which says %q: nobody saw that process stop, so it may be running the turn still and no word of this notice may call the turn ended (stallTurnClause)", end, refused, said)
			}
		case stallProcessKept:
			for reading, text := range map[string]string{"a stop the runtime made": told, "a stop the runtime refused": refused} {
				if said := saidFirst(text, stopped); said != "" {
					t.Errorf("the notice of %q over %s reads %q, which says %q: this end let no process go, so no word of this notice may call stopped the process the claim keeps and runs", end, reading, text, said)
				}
				if strings.Contains(text, stallStopRefused) {
					t.Errorf("the notice of %q over %s reads %q, which says %q: this end asked for no stop of the process its claim keeps and runs", end, reading, text, stallStopRefused)
				}
				if said := saidFirst(text, ended); said != "" {
					t.Errorf("the notice of %q over %s reads %q, which says %q: this end kept the claim's process and the turn it is running, so no word of this notice may call that turn ended", end, reading, text, said)
				}
			}
		}
	}
	for end := range worded {
		t.Errorf("the notice has a sentence for %q, which is no end the machine can reach: drop the row, or add the end to supervise.ProcessEnds", end)
	}
}

// stallTurnEndedClaims is every way this notice's words can say the stalled turn was ended,
// filled for the claim the walk writes its notices from. The walk reads the whole notice for
// them, because what the derivation above cannot reach is a row that writes the claim into its
// own words (stallTurnClause, stallEndWords.did).
func stallTurnEndedClaims(c supervise.Claim) []string {
	role := string(c.Role)
	return []string{
		"ended the " + role + "'s turn",
		"ended its turn",
		"ended this turn",
		"ended that turn",
		"ended the turn",
		"turn was ended",
		"turn has ended",
		"turn is over",
	}
}

// stallStopClaims is every way this notice's words can say the claim's stalled process was
// stopped, taken away or ended, filled for the claim the walk writes its notices from. The
// walk reads the whole notice for them, as it does for the turn, because the derivation that
// keeps a row from picking the sentence about that process (stallStopClause) cannot keep a row
// from writing the claim into its own words (stallEndWords.did).
//
// None of them is carried by the true sentence of a stop the runtime refused, which says the
// runtime did not confirm the stop and that the process may be running still
// (stallStopRefused): a phrasing read over the whole notice must name a stop that was made,
// never the word stop alone, or the one end that must say that sentence could not say it. For
// the same reason the words a let-go end uses of its own record — that it let the process go,
// which is true whether or not the runtime stopped it (stallProcessLetGo) — are not claims
// here. A notice that keeps a process and calls the task taken back is likewise saying nothing
// of the process, so every claim below names the process it stops.
func stallStopClaims(c supervise.Claim) []string {
	role := string(c.Role)
	return []string{
		"stopped the " + role + "'s stalled process",
		"stopped the " + role + "'s process",
		"stopped its stalled process",
		"stopped the stalled process",
		"stopped its process",
		"stopped that process",
		"stopped the process",
		"process was stopped",
		"process has been stopped",
		"process is stopped",
		"confirmed the stop",
		"ended the " + role + "'s process",
		"ended its process",
		"ended that process",
		"ended the process",
		"process was ended",
		"process has ended",
		"killed its process",
		"killed that process",
		"killed the process",
		"process was killed",
		"took its process away",
		"took that process away",
		"took the process away",
		"process was taken away",
		"process is gone",
		"process is no longer running",
	}
}

// saidFirst is the first of claims that told carries, and the empty string for a notice that
// carries none.
func saidFirst(told string, claims []string) string {
	for _, said := range claims {
		if strings.Contains(told, said) {
			return said
		}
	}
	return ""
}

// rigClaim is a claim of the rig's shape for a case that writes a notice from it directly,
// without a machine to end a turn on.
func rigClaim() supervise.Claim {
	return supervise.Claim{Token: "legion-legion-legion-225-implementer", Project: "legion", Tree: "LEGION-225",
		Issue: "LEGION-225", Role: claim.RoleImplementer, State: supervise.StateWorking}
}

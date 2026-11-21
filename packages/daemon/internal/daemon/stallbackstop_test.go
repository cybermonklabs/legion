package daemon

// The daemon's half of the stall backstop: the pass over the claims the orphan sweep already
// read, which ends the turn of every claim whose recorded last step is older than its role's
// bound and tells the owner once per stall. It is driven directly over a bare supervisor (no
// store, no HTTP), the construction plugin_test.go's boot cases use, because what it decides is
// decided from the claims it is handed and the configuration it holds.

import (
	"context"
	"encoding/json"
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
	"github.com/sjawhar/legion/daemon/internal/texts"
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
	// tried, refused ones included, which a case reads to see that none was tried; refuse the
	// error the store answers while it is set. reports is how many ends the machines told
	// supervision of, counted inside the decision that made each: the row is written on a
	// goroutine of supervision's own (stallBackstopped), so a case that read rows alone would
	// read "nobody was told" of a machine that told and whose goroutine has not run yet, and
	// that assertion could never fail.
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
		refreshGate: supervise.NewRefreshGate(0),
		enqueue: func(_ context.Context, issue string, payloads ...record.OutboxPayload) (bool, error) {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.attempts++
			if rig.refuse != nil {
				return false, rig.refuse
			}
			rig.rows[issue] = append(rig.rows[issue], payloads...)
			return true, nil
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

// awaitOwed waits for supervision to keep token's refused notice for the next sweep
// (oweStallNotice). That is what a case that sweeps again to see the notice told waits on, and
// not the refused write itself: the write is counted inside the store's call, and the notice is
// kept only after that call has returned, on the machine's goroutine (stallBackstopped), so a
// sweep made between the two finds nothing owed and the owner is told at the sweep after it.
func (r *backstopRig) awaitOwed(token claim.Token) {
	r.t.Helper()
	testwait.Eventually(r.t, "supervision to keep the refused notice for the next sweep", func() bool {
		r.s.backstopMu.Lock()
		defer r.s.backstopMu.Unlock()
		_, owed := r.s.backstopOwed[token]
		return owed
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
	if notice.Kind != texts.WorkerStalled || notice.Role != claim.RoleImplementer || notice.Issue != "LEGION-209" {
		t.Fatalf("the notice = %+v, want a worker-stalled of the implementer on LEGION-209", notice)
	}
	if notice.End != string(supervise.EndRelaunched) || notice.Stop != texts.StopStopped || notice.LaunchFailures != 0 {
		t.Errorf("the notice = %+v, want the relaunch, its process stopped, and nothing charged", notice)
	}
	if notice.SilentFor != "4h30m0s" || notice.Bound != "4h0m0s" {
		t.Errorf("the notice is silent for %q past %q, want 4h30m0s past the 4h0m0s the deployment gives the role", notice.SilentFor, notice.Bound)
	}
	if notice.CallTool != "bash" || notice.CallID != "toolu_impl_11" || !notice.CallStarted.Equal(stalled.Step.StartedAt) || !notice.CallEnded.IsZero() {
		t.Errorf("the notice names the call %q (%q) from %s to %s, want bash (toolu_impl_11) from %s, not ended",
			notice.CallTool, notice.CallID, notice.CallStarted, notice.CallEnded, stalled.Step.StartedAt)
	}
	if !notice.TurnStarted.Equal(stalled.Step.TurnStartedAt) || !notice.LastStep.Equal(stalled.Step.LastStep()) {
		t.Errorf("the notice's turn began %s and its last step was %s, want %s and %s",
			notice.TurnStarted, notice.LastStep, stalled.Step.TurnStartedAt, stalled.Step.LastStep())
	}
	if encoded, err := json.Marshal(notice); err != nil || strings.Contains(string(encoded), "9f2b7c0d1e3a4b5c") {
		t.Errorf("the notice is %s (%v), want none of the call's fingerprint", encoded, err)
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
// which is the only moment its record holds, and its notice names no call.
func TestTheStallBackstopJudgesAClaimWithNoCallFromItsTurnsStart(t *testing.T) {
	rig := newBackstopRig(t)
	stalled := rig.claim(claim.RoleTester, "LEGION-210", backstopBound+time.Minute, false)
	claims := rig.supervising(stalled)

	rig.sweep(claims)

	rig.awaitResumed(stalled.Token, 1)
	notices := rig.awaitNotices("LEGION-210", 1)
	if notice := notices[0]; notice.CallTool != "" || notice.CallID != "" || !notice.CallStarted.IsZero() ||
		!notice.LastStep.Equal(stalled.Step.TurnStartedAt) || !notice.TurnStarted.Equal(stalled.Step.TurnStartedAt) {
		t.Fatalf("notices = %+v, want one naming no call, judged from the turn's start %s", notices, stalled.Step.TurnStartedAt)
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
// is written. The case sweeps again only once the refused notice is kept (awaitOwed): a sweep
// made before then finds nothing owed to tell.
func TestTheStallBackstopTellsTheOwnerAgainWhenTheRowIsRefused(t *testing.T) {
	rig := newBackstopRig(t)
	rig.refusing(errors.New("the store is down"))
	stalled := rig.claim(claim.RolePlanner, "LEGION-219", backstopBound+time.Hour, true)
	claims := rig.supervising(stalled)

	rig.sweep(claims)
	rig.awaitResumed(stalled.Token, 1)
	rig.awaitOwed(stalled.Token)
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

// A notice the sweep tells again and the store refuses once more is kept for the next sweep,
// unless a newer stall of the same claim was owed while the sweep had the older one in its hand:
// one claim owes one notice, and the newer record is the one its owner wants (oweStallNotice,
// oweStallNoticeAgain). The newer notice here is owed from inside the refused write, which is
// the moment the machine's own report can owe it (stallBackstopped). The other two cases are the
// controls: a newer notice owed after the sweep's write replaces the older one in every order,
// and a refused retry with nothing newer owed is still kept and told at the next sweep.
//
// It drives a bare supervision, with no machine, because what it reads is the owed set's own
// rule: the two notices are told apart by the call each record names.
func TestTheStallBackstopKeepsTheNewerNoticeOwedWhileTheSweepRetriedTheOlder(t *testing.T) {
	stopAt := func(call string, at time.Time) supervise.StallBackstopStop {
		step := supervise.StepRecord{TurnStartedAt: at.Add(-time.Hour), At: at, StartedAt: at, Tool: "bash", CallID: call}
		return supervise.StallBackstopStop{Step: step, LastStep: step.LastStep(), Bound: backstopBound, End: supervise.EndRelaunched}
	}
	older := stopAt("toolu_older", time.Now().Add(-3*backstopBound))
	newer := stopAt("toolu_newer", time.Now().Add(-backstopBound-time.Hour))
	for _, tc := range []struct {
		name      string
		newerOwed string // "during" the refused retry, "after" it, or "never"
		told      supervise.StallBackstopStop
		untold    supervise.StallBackstopStop
	}{
		{name: "a newer notice owed during the refused retry", newerOwed: "during", told: newer, untold: older},
		{name: "a newer notice owed after the refused retry", newerOwed: "after", told: newer, untold: older},
		{name: "no newer notice owed", newerOwed: "never", told: older, untold: newer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := rigClaim()
			var (
				mu      sync.Mutex
				refused = true
				during  func()
				notices []record.Notice
			)
			s := &supervision{log: quietLogger(), plan: plan{clock: stillClock{}}}
			s.enqueue = func(_ context.Context, _ string, payloads ...record.OutboxPayload) (bool, error) {
				mu.Lock()
				refusing, inside := refused, during
				during = nil
				if !refusing {
					for _, payload := range payloads {
						if notice, ok := payload.(record.Notice); ok {
							notices = append(notices, notice)
						}
					}
				}
				mu.Unlock()
				if !refusing {
					return true, nil
				}
				if inside != nil {
					inside()
				}
				return false, errors.New("the store is down")
			}
			s.oweStallNotice(c, older)
			if tc.newerOwed == "during" {
				during = func() { s.oweStallNotice(c, newer) }
			}

			s.backstopStalledTurns(context.Background(), nil)
			if tc.newerOwed == "after" {
				s.oweStallNotice(c, newer)
			}
			mu.Lock()
			refused = false
			mu.Unlock()
			s.backstopStalledTurns(context.Background(), nil)

			mu.Lock()
			defer mu.Unlock()
			if len(notices) != 1 {
				t.Fatalf("notices after the refusal cleared = %+v, want one", notices)
			}
			if called := notices[0].CallID; called != tc.told.Step.CallID {
				t.Fatalf("the owner was told of the call %q, want the record that names %s and not %s", called, tc.told.Step.CallID, tc.untold.Step.CallID)
			}
		})
	}
}

// A tree architect's own stall is told to the controller as well, in a row of its own: every
// notice of the tree that no child's own architect takes reaches that architect, so nobody outside
// it would hear of the turn the daemon ended. A worker's stall is its architect's alone.
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
// rather than a relaunch (replaceProcess, endHeld), so the notice names the suspended end and
// the stop of the process, and the claim is suspended with its task still owed.
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
	if notice := notices[0]; notice.End != string(supervise.EndSuspended) || notice.Stop != texts.StopStopped || notice.LaunchFailures != 0 {
		t.Errorf("the notice = %+v, want the suspended end, its process stopped, and nothing charged", notice)
	}
	if held := m.Claim(); held.Pending == nil {
		t.Errorf("the suspended claim holds no task, want the task the stalled turn was serving still owed")
	}
}

// A turn the daemon ended whose replacement the store would not write reaches its owner all the
// same, and the notice says what is true of it: the worker's process was stopped and the launch
// that replaces it is pending (supervise's launchNow, relaunchLater). It is the store's
// transient refusal that this exists for — the one a sweep sees often enough to have been
// measured — so a stall that falls in it is a worker stopped and an owner who would otherwise
// hear nothing at all.
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
	if notice := notices[0]; notice.End != string(supervise.EndRelaunchPending) || notice.Stop != texts.StopStopped || notice.LaunchFailures != 0 {
		t.Errorf("the notice = %+v, want the relaunch pending, its process stopped, and nothing charged", notice)
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
	if notice := notices[0]; notice.End != string(supervise.EndSuspended) || notice.Stop != texts.StopUnconfirmed {
		t.Errorf("the notice = %+v, want the suspended end with a stop the runtime did not confirm: the process may be running still", notice)
	}
}

// And a turn whose replacement the runtime refused until the claim's launch budget was gone:
// the claim has failed, nothing is queued to relaunch it, and the notice names the failed end
// and the launch failures that end is made of, the one thing a stall does charge.
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
	if notice := notices[0]; notice.End != string(supervise.EndFailed) || notice.Stop != texts.StopStopped ||
		notice.LaunchFailures != rig.sup.deps.Limits.LaunchFailures {
		t.Errorf("the notice = %+v, want the failed end, its process stopped, and the %d launch failures charged",
			notice, rig.sup.deps.Limits.LaunchFailures)
	}
}

// The charge over the common shape of all: one start the runtime refused inside the
// replacement, and the next one taken. The end is the relaunch that worked, and the claim was
// charged the launch failure all the same (supervise's launchNow), so the notice counts it.
func TestTheStallBackstopCountsALaunchFailureChargedOnTheWayToARelaunch(t *testing.T) {
	rig := newBackstopRig(t)
	stalled := rig.claim(claim.RoleImplementer, "LEGION-228", backstopBound+time.Hour, true)
	claims := rig.supervising(stalled)
	rig.rt.ScriptResume(fake.SpawnResult{Err: errors.New("the pod did not start")})

	rig.sweep(claims)

	rig.awaitResumed(stalled.Token, 2)
	notices := rig.awaitNotices("LEGION-228", 1)
	m, ok := rig.sup.Machine(stalled.Token)
	if !ok {
		t.Fatal("the stalled claim is not supervised")
	}
	if c := m.Claim(); c.Locator == nil || c.Budgets.LaunchFailures != 1 {
		t.Fatalf("the claim after the backstop = %+v, want it recording the process the second try started with the one refusal charged", c)
	}
	if notice := notices[0]; notice.End != string(supervise.EndRelaunched) || notice.Stop != texts.StopStopped || notice.LaunchFailures != 1 {
		t.Errorf("the notice = %+v, want the relaunch, its process stopped, and the one launch failure charged", notice)
	}
}

// The same charge where the replacement never started: one start the runtime refused, and then
// a write the store would not take, which leaves the launch still to make (supervise's
// launchNow, relaunchLater). The end is the relaunch pending, and the failure charged on the
// way to it is charged whatever the end was.
func TestTheStallBackstopCountsALaunchFailureChargedBeforeAPendingRelaunch(t *testing.T) {
	rig := newBackstopRig(t)
	stalled := rig.claim(claim.RoleTester, "LEGION-229", backstopBound+time.Hour, true)
	claims := rig.supervising(stalled)
	rig.rt.ScriptResume(fake.SpawnResult{Err: errors.New("the pod did not start")})
	// The store takes the first try's write and refuses what follows it, which is the window
	// the second try's own write falls in: the refusal is set where the first start is made,
	// after that write landed and before the retry writes again.
	rig.rt.HoldResume(func() { rig.claimStore().refuse(errors.New("the store is down")) })

	rig.sweep(claims)

	notices := rig.awaitNotices("LEGION-229", 1)
	m, ok := rig.sup.Machine(stalled.Token)
	if !ok {
		t.Fatal("the stalled claim is not supervised")
	}
	if c := m.Claim(); c.State != supervise.StateLaunching || c.Locator != nil || c.Budgets.LaunchFailures != 1 {
		t.Fatalf("the claim after the backstop = %+v, want it launching with nothing started and the one refusal charged", c)
	}
	if notice := notices[0]; notice.End != string(supervise.EndRelaunchPending) || notice.Stop != texts.StopStopped || notice.LaunchFailures != 1 {
		t.Errorf("the notice = %+v, want the relaunch pending, its process stopped, and the one launch failure charged", notice)
	}
}

// A replacement the wake pacing parked is the one end whose turn was not ended: the take-back
// has run, so the task is owed again, and the claim goes on recording the stalled process
// until the launch comes due (supervise's reserveWake, parkWake). The notice names that end, no
// stop made of the process, and the moment the launch is due, which is the wake's own due time
// the machine carries on its report (supervise.StallBackstopStop.WakeDue), not a guess at how
// long a wave takes.
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
	if notice := notices[0]; notice.End != string(supervise.EndRelaunchParked) || notice.Stop != texts.StopNotMade ||
		!notice.WakeDue.Equal(parked.At) || notice.LaunchFailures != 0 {
		t.Errorf("the notice = %+v, want the relaunch parked, no stop made, the launch due at %s, and nothing charged", notice, parked.At)
	}
}

// The notice's stop word walked by the machine's own classification rather than by the ends
// anyone remembered (supervise.ProcessEnds): every end that tells its owner has a row in
// stallBackstopEnds, no end that tells nobody has one, and no row is left for an end the
// machine cannot reach. Every end is written under both values of the fact the machine records
// about the stop (supervise.StallBackstopStop.StopUnconfirmed) and with launch failures charged
// and not: an end that let the claim's process go names the stop as stopped under a confirmed
// stop and as unconfirmed under the other, an end that kept the process names no stop made
// under either, and the end and the charge are the machine's own. An end this daemon has no row
// for is named and given no stop word.
func TestTheNoticeHasWordsForEveryEndThatTellsItsOwner(t *testing.T) {
	worded := map[supervise.ProcessEnd]bool{}
	for end := range stallBackstopEnds {
		worded[end] = true
	}
	for _, end := range supervise.ProcessEnds() {
		letsGo, has := stallBackstopEnds[end]
		delete(worded, end)
		if !end.TellsOwner() {
			if has {
				t.Errorf("the notice has a row for %q, an end told to nobody: drop the row, or make the end tell its owner", end)
			}
			continue
		}
		if !has {
			t.Errorf("the notice has no row for the end %q: add it to stallBackstopEnds, saying whether the end let the claim's process go", end)
			continue
		}
		for _, unconfirmed := range []bool{false, true} {
			for _, charged := range []int{0, 3} {
				stop := supervise.StallBackstopStop{Bound: backstopBound, End: end, StopUnconfirmed: unconfirmed, LaunchFailures: charged}
				told := stallBackstopRecord(rigClaim(), stop, time.Hour)
				want := texts.StopNotMade
				switch {
				case letsGo && unconfirmed:
					want = texts.StopUnconfirmed
				case letsGo:
					want = texts.StopStopped
				}
				if told.Stop != want || told.End != string(end) || told.LaunchFailures != charged {
					t.Errorf("the notice of %q (stop unconfirmed %t, %d charged) = %+v, want stop %q, the end and the charge as the machine recorded them",
						end, unconfirmed, charged, told, want)
				}
			}
		}
	}
	for end := range worded {
		t.Errorf("the notice has a row for %q, which is no end the machine can reach: drop the row, or add the end to supervise.ProcessEnds", end)
	}
	unknown := stallBackstopRecord(rigClaim(), supervise.StallBackstopStop{Bound: backstopBound, End: "unheard of"}, time.Hour)
	if unknown.End != "unheard of" || unknown.Stop != "" {
		t.Errorf("the notice of an end this daemon has no row for = %+v, want the end named and no stop word", unknown)
	}
}

// rigClaim is a claim of the rig's shape for a case that writes a notice from it directly,
// without a machine to end a turn on.
func rigClaim() supervise.Claim {
	return supervise.Claim{Token: "legion-legion-legion-225-implementer", Project: "legion", Tree: "LEGION-225",
		Issue: "LEGION-225", Role: claim.RoleImplementer, State: supervise.StateWorking}
}

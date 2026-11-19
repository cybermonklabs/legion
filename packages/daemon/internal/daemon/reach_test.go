package daemon

// The daemon's half of R5 and R6: once a minute it reads Envoy's session list, marks a claim whose
// session's last registration is more than 20 minutes old — counted from the latest of that
// registration, its boot, the session's first ready, the end of its last failed read and its last
// read that found no live session — tells the controller and the tree's architect, and writes R6's
// line for what no agent can hear. The keeper is driven directly over fakes of the list, the
// store's write, the outbox and the clock, because what it decides is decided from the claims it is
// handed and what it read.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fleetalert"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// reachBoot is the moment the rig's daemon booted.
var reachBoot = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

type toldNotice struct {
	notice record.ControllerNotice
	issue  string
}

type reachRig struct {
	t *testing.T
	k *reachKeeper
	// lockOuts is the lock-out notices the minute sweep resends, told through the same outbox.
	lockOuts *lockOutNotices
	logged   *bytes.Buffer

	mu       sync.Mutex
	now      time.Time
	sessions []notify.ListedSession
	// roles is the Envoy roles each session holds in the list listing writes.
	roles      map[string][]string
	listErr    error
	puts       map[claim.Token]supervise.Reach
	told       []toldNotice
	refuseTell bool
	rows       map[string][]record.OutboxPayload
	relaunched []supervise.Claim
	relaunch   error
}

func newReachRig(t *testing.T, daemonController bool) *reachRig {
	t.Helper()
	r := &reachRig{t: t, logged: &bytes.Buffer{}, now: reachBoot, puts: map[claim.Token]supervise.Reach{},
		roles: map[string][]string{}, rows: map[string][]record.OutboxPayload{}}
	log := slog.New(slog.NewTextHandler(r.logged, nil))
	r.k = newReachKeeper(reachKeeperOptions{
		log: log, project: "LEGION", boot: reachBoot, daemonController: daemonController,
		now: func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now },
		list: func(context.Context) ([]notify.ListedSession, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			return append([]notify.ListedSession(nil), r.sessions...), r.listErr
		},
		put: func(_ context.Context, token claim.Token, reach supervise.Reach) (bool, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.puts[token] = reach
			return true, nil
		},
		reachNotify: reachNotify{
			tell: func(_ context.Context, notice record.ControllerNotice, issue string, marks []reachMark) (bool, error) {
				r.mu.Lock()
				defer r.mu.Unlock()
				if r.refuseTell {
					return false, nil
				}
				r.told = append(r.told, toldNotice{notice: notice, issue: issue})
				r.putMarks(marks)
				return true, nil
			},
			enqueue: func(_ context.Context, issue string, notice record.Notice, marks []reachMark) error {
				r.mu.Lock()
				defer r.mu.Unlock()
				r.rows[issue] = append(r.rows[issue], notice)
				r.putMarks(marks)
				return nil
			},
		},
		alerts: fleetalert.New(log, "legion"),
		relaunchController: func(_ context.Context, c supervise.Claim, at time.Time) (supervise.Claim, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.relaunched = append(r.relaunched, c)
			if r.relaunch != nil {
				return c, r.relaunch
			}
			c.Generation++
			c.LastRelaunch, c.LastRelaunchGeneration = at, c.Generation
			return c, nil
		},
	})
	r.lockOuts = newLockOutNotices(lockOutOptions{log: log, project: "LEGION", tell: r.k.tell, alerts: r.k.alerts})
	return r
}

// putMarks writes the claims' reach records a notice marks told, as its transaction does; a
// project's own notice record has no claim's record to write. The caller holds mu.
func (r *reachRig) putMarks(marks []reachMark) {
	for _, mark := range marks {
		if mark.project == nil {
			r.puts[mark.token] = mark.reach
		}
	}
}

// at moves the rig's clock to boot plus d.
func (r *reachRig) at(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = reachBoot.Add(d)
}

// listing sets what Envoy's list answers: each session with its last registration, live when it
// is inside the listener's 5-minute live window, and the roles it holds (roles).
func (r *reachRig) listing(seen map[string]time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions = nil
	for session, at := range seen {
		last := reachBoot.Add(at)
		state := notify.StateLive
		if r.now.Sub(last) > 5*time.Minute {
			state = notify.StateUnresponsive
		}
		r.sessions = append(r.sessions, notify.ListedSession{Session: session, State: state, LastSeen: last, Roles: r.roles[session]})
	}
}

// sweep is one minute's pass over claims, the lock-out notices' resend first as the orphan sweep
// runs it, with the controller's relaunch it may start finished.
func (r *reachRig) sweep(claims ...supervise.Claim) {
	r.t.Helper()
	r.lockOuts.resend(context.Background())
	r.k.sweep(context.Background(), claims)
	r.k.wait()
}

func (r *reachRig) notices(kind record.NoticeKind) []toldNotice {
	r.mu.Lock()
	defer r.mu.Unlock()
	var of []toldNotice
	for _, told := range r.told {
		if told.notice.Kind == kind {
			of = append(of, told)
		}
	}
	return of
}

func (r *reachRig) architectNotices(issue string) []record.Notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	var of []record.Notice
	for _, payload := range r.rows[issue] {
		if notice, ok := payload.(record.Notice); ok {
			of = append(of, notice)
		}
	}
	return of
}

func (r *reachRig) lines(word string) int {
	return strings.Count(r.logged.String(), "case="+word)
}

func (r *reachRig) reach(token claim.Token) supervise.Reach {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.puts[token]
}

// reachClaim is role's claim on issue of tree, ready on session since reachBoot minus an hour,
// idle with nothing owed.
func reachClaim(t *testing.T, tree, issue string, role claim.Role, session string) supervise.Claim {
	t.Helper()
	project, err := claim.ProjectToken("legion")
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		t.Fatal(err)
	}
	return supervise.Claim{Token: token, Project: project, Tree: tree, Issue: issue, Role: role, Generation: 3,
		State: supervise.StateIdle, Session: session, LastReady: reachBoot.Add(-time.Hour), Locator: pluginTestLocator(token)}
}

// with is c carrying r's last written reach record, as the store reads it back at the next sweep.
func (r *reachRig) with(c supervise.Claim) supervise.Claim {
	c.Reach = r.reach(c.Token)
	return c
}

// countedNotices is a store of the project's own notice records that holds held, and counts the
// reads and the deletes it is asked for.
type countedNotices struct {
	mu             sync.Mutex
	held           []store.ReachProjectNotice
	reads, deletes int
}

func (n *countedNotices) ReachProjectNotices(context.Context, string) ([]store.ReachProjectNotice, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reads++
	return slices.Clone(n.held), nil
}

func (n *countedNotices) PutReachProjectNotice(context.Context, store.ReachProjectNotice) error {
	return nil
}

func (n *countedNotices) DeleteReachProjectNotice(context.Context, string, string, claim.Token) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.deletes++
	n.held = nil
	return nil
}

// A daemon whose every list holds a live session reads the project's notice records once, at its
// first read, and deletes the no-live run's record only where the daemon before it told one; no
// later read of a live list asks the store anything until a run is told.
func TestALiveListReadsTheNoLiveRecordOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		held    []store.ReachProjectNotice
		deletes int
	}{
		{name: "no run told", deletes: 0},
		{name: "a run the last daemon told", held: []store.ReachProjectNotice{{Project: "LEGION",
			Kind: string(record.EnvoyNoLiveSessionNotice), Told: true}}, deletes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReachRig(t, false)
			notices := &countedNotices{held: tc.held}
			r.k.notices = notices
			for minute := 1; minute <= 30; minute++ {
				r.at(time.Duration(minute) * time.Minute)
				r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
				r.sweep()
			}
			if notices.reads != 1 || notices.deletes != tc.deletes {
				t.Fatalf("30 minutes of live lists read the records %d times and deleted the no-live record %d times, want one read and %d deletes",
					notices.reads, notices.deletes, tc.deletes)
			}
		})
	}
}

// The 20 minutes are counted from the daemon's boot as well as the session's last registration,
// so a daemon outage does not mark every claim at once; a claim whose session registered in the
// last 20 minutes is never marked; and a claim marked keeps its state.
func TestAClaimIsMarkedTwentyMinutesAfterItsLastRegistrationCountedFromTheBoot(t *testing.T) {
	r := newReachRig(t, false)
	worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
	worker.State = supervise.StateWorking

	// Its last registration was an hour before the boot, and the listener still lists it.
	r.at(19 * time.Minute)
	r.listing(map[string]time.Duration{"ses_implementer": -time.Hour, "ses_other": 19 * time.Minute})
	r.sweep(worker)
	if r.reach(worker.Token).Unreachable() || len(r.notices(record.UnreachableClaimsNotice)) != 0 {
		t.Fatalf("at 19 minutes after the boot the claim reads %+v and the controller was told %d times; the boot counts",
			r.reach(worker.Token), len(r.notices(record.UnreachableClaimsNotice)))
	}

	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_implementer": -time.Hour, "ses_other": 21 * time.Minute})
	r.sweep(r.with(worker))
	got := r.reach(worker.Token)
	if !got.Unreachable() || !got.Since.Equal(reachBoot) || !got.LastSeen.Equal(reachBoot.Add(-time.Hour)) {
		t.Fatalf("at 21 minutes the claim's reach = %+v, want it marked since the boot with its last registration read", got)
	}
	told := r.notices(record.UnreachableClaimsNotice)
	if len(told) != 1 || told[0].issue != "LEGION" || len(told[0].notice.Unreachable) != 1 ||
		told[0].notice.Unreachable[0].Claim != string(worker.Token) || told[0].notice.Unreachable[0].State != string(supervise.StateWorking) {
		t.Fatalf("the controller was told %+v, want one list naming the working claim on the project", told)
	}

	r.at(40 * time.Minute)
	r.sweep(r.with(worker))
	if told := r.notices(record.UnreachableClaimsNotice); len(told) != 1 {
		t.Fatalf("a claim still out of reach was listed %d times, want once", len(told))
	}

	// Its session registers: reachable again, and reported again only once it is out of reach again.
	r.at(41 * time.Minute)
	r.listing(map[string]time.Duration{"ses_implementer": 41 * time.Minute})
	r.sweep(r.with(worker))
	if r.reach(worker.Token).Unreachable() || r.reach(worker.Token).Reported {
		t.Fatalf("a claim whose session registered reads %+v, want it reachable and its report over", r.reach(worker.Token))
	}
	r.at(60 * time.Minute)
	r.sweep(r.with(worker))
	if told := r.notices(record.UnreachableClaimsNotice); len(told) != 1 {
		t.Fatalf("at 19 minutes after its registration the claim was listed %d times, want once (the first episode)", len(told))
	}
	r.at(62 * time.Minute)
	r.sweep(r.with(worker))
	if told := r.notices(record.UnreachableClaimsNotice); len(told) != 2 {
		t.Fatalf("the claim out of reach a second time was listed %d times in all, want twice", len(told))
	}
}

// A relaunch keeps the session, so its ready does not count the 20 minutes again: the first ready
// of the session the claim records is the one the count reads, through every launch state of it.
func TestARelaunchOnTheSameSessionDoesNotCountTheTwentyMinutesAgain(t *testing.T) {
	r := newReachRig(t, false)
	planner := reachClaim(t, "LEGION-208", "LEGION-210", claim.RolePlanner, "ses_planner")
	planner.LastReady = reachBoot.Add(time.Minute)
	r.listing(map[string]time.Duration{"ses_planner": 0, "ses_other": 0})
	r.at(2 * time.Minute)
	r.sweep(planner)

	// The daemon relaunches it again and again; each launch readies on the same session.
	for minute := 5; minute <= 22; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.listing(map[string]time.Duration{"ses_planner": 0, "ses_other": time.Duration(minute) * time.Minute})
		planner.Generation++
		planner.LastReady = reachBoot.Add(time.Duration(minute) * time.Minute)
		planner.State = []supervise.ClaimState{supervise.StateLaunching, supervise.StateReady, supervise.StateRegistered}[minute%3]
		r.sweep(r.with(planner))
	}
	if !r.reach(planner.Token).Unreachable() {
		t.Fatalf("at 22 minutes, relaunched every minute on its session, the planner reads %+v, want it marked from its first ready", r.reach(planner.Token))
	}
}

// A read of the list that fails changes no mark and sends no notice, its end counts the 20 minutes
// again, and reads that fail for 20 minutes write R6's line once.
func TestAFailedReadChangesNothingAndTwentyMinutesOfThemWriteTheLine(t *testing.T) {
	r := newReachRig(t, false)
	worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
	r.listing(map[string]time.Duration{"ses_other": 0})
	r.at(25 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 25 * time.Minute})
	r.sweep(worker)
	if !r.reach(worker.Token).Unreachable() {
		t.Fatal("the claim was not marked at 25 minutes")
	}
	before := len(r.told)

	r.mu.Lock()
	r.listErr = errors.New("listener returned 503 Service Unavailable: refilling")
	r.mu.Unlock()
	for minute := 26; minute <= 45; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.sweep(r.with(worker))
	}
	if !r.reach(worker.Token).Unreachable() || len(r.told) != before {
		t.Fatalf("the failed reads changed the mark to %+v or told %d notices", r.reach(worker.Token), len(r.told)-before)
	}
	if got := r.lines(fleetalert.SessionListUnreadable.Word); got != 0 {
		t.Fatalf("19 minutes of failed reads wrote the line %d times, want none yet", got)
	}
	for minute := 46; minute <= 50; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.sweep(r.with(worker))
	}
	if got := r.lines(fleetalert.SessionListUnreadable.Word); got != 1 {
		t.Fatalf("24 minutes of failed reads wrote the line %d times, want once", got)
	}

	// The list answers again: the count starts again at the last failed read's end.
	r.mu.Lock()
	r.listErr = nil
	r.mu.Unlock()
	fresh := reachClaim(t, "LEGION-208", "LEGION-211", claim.RoleTester, "ses_tester")
	r.at(51 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 51 * time.Minute})
	r.sweep(r.with(worker), fresh)
	if r.reach(fresh.Token).Unreachable() {
		t.Fatal("a claim was marked the minute after reads that failed for 20 minutes")
	}
}

// A failed read, or a read whose list holds no live session, holds back a mark that is not made
// yet: the 20 minutes count from it. A mark already made it leaves as it is — its moment, its last
// registration and its report — and it sends nothing, the minute it happens and the minute after,
// when the list answers again with the claim's session still unregistered.
func TestAFailedReadOrAnEmptyListLeavesAMarkAsItIs(t *testing.T) {
	for name, interrupt := range map[string]func(r *reachRig, minute time.Duration){
		"a failed read": func(r *reachRig, _ time.Duration) {
			r.mu.Lock()
			r.listErr = errors.New("listener returned 503 Service Unavailable: refilling")
			r.mu.Unlock()
		},
		"a list with no live session": func(r *reachRig, _ time.Duration) {
			r.listing(map[string]time.Duration{"ses_implementer": -time.Hour})
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReachRig(t, false)
			worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
			r.at(25 * time.Minute)
			r.listing(map[string]time.Duration{"ses_implementer": -time.Hour, "ses_other": 25 * time.Minute})
			r.sweep(worker)
			marked := r.reach(worker.Token)
			if !marked.Unreachable() || !marked.Reported {
				t.Fatalf("at 25 minutes the claim reads %+v, want it marked and reported", marked)
			}
			told, rows := len(r.notices(record.UnreachableClaimsNotice)), len(r.architectNotices("LEGION-209"))

			r.at(26 * time.Minute)
			interrupt(r, 26*time.Minute)
			r.sweep(r.with(worker))
			r.mu.Lock()
			r.listErr = nil
			r.mu.Unlock()
			for minute := 27; minute <= 30; minute++ {
				r.at(time.Duration(minute) * time.Minute)
				r.listing(map[string]time.Duration{"ses_implementer": -time.Hour, "ses_other": time.Duration(minute) * time.Minute})
				r.sweep(r.with(worker))
				got := r.reach(worker.Token)
				if !got.Unreachable() || !got.Since.Equal(marked.Since) || !got.LastSeen.Equal(marked.LastSeen) || !got.Reported {
					t.Fatalf("at %d minutes, after %s at 26, the claim reads %+v, want its mark %+v as it was", minute, name, got, marked)
				}
			}
			if got := len(r.notices(record.UnreachableClaimsNotice)); got != told {
				t.Errorf("after %s the controller was told %d more lists of unreachable claims, want none", name, got-told)
			}
			if got := len(r.architectNotices("LEGION-209")); got != rows {
				t.Errorf("after %s the architect was sent %d more notices, want none", name, got-rows)
			}
		})
	}
}

// The daemon's clean stop of a dead role's session reads the session's registration and then
// deletes it, which takes it out of Envoy's list before the next minute's read can see it. The
// registration it read still counts: a claim whose session registered in the last 20 minutes is
// never marked (the tenth claim of w5's main run, marked 19m27s after a registration the stop
// deleted at 11:46:18Z).
func TestARegistrationTheCleanStopDeletesStillCountsTheTwentyMinutes(t *testing.T) {
	r := newReachRig(t, false)
	planner := reachClaim(t, "LEGION-208", "LEGION-210", claim.RolePlanner, "ses_planner")
	r.at(5 * time.Minute)
	r.listing(map[string]time.Duration{"ses_planner": 4 * time.Minute, "ses_other": 5 * time.Minute})
	r.sweep(planner)

	// At 6m30s the session registers once more and its process dies; the stop reads that
	// registration and deletes it, so no later list holds the session.
	registered := reachBoot.Add(6*time.Minute + 30*time.Second)
	listener := &listenerRecorder{held: storedRegistration{clientInstanceID: "inst_dead", lastSeen: registered}}
	server := listener.server(t)
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: quietLogger(), seen: r.k.registrationRead}
	stop.send("ses_planner", registered.Add(8*time.Second))
	if !listener.entryRemoved {
		t.Fatal("the clean stop did not delete the registration")
	}

	for minute := 7; minute <= 26; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
		r.sweep(r.with(planner))
		if got := r.reach(planner.Token); got.Unreachable() {
			t.Fatalf("at %d minutes the planner reads %+v: marked %s after the registration the stop read at %s",
				minute, got, reachBoot.Add(time.Duration(minute)*time.Minute).Sub(registered), registered.Format(time.TimeOnly))
		}
	}
	r.at(27 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 27 * time.Minute})
	r.sweep(r.with(planner))
	if got := r.reach(planner.Token); !got.Unreachable() || !got.LastSeen.Equal(registered) {
		t.Fatalf("at 27 minutes the planner reads %+v, want it marked with the last registration the stop read, %s", got, registered)
	}
}

// The keeper keeps the last registration of each session a watched claim records, and of no other
// session: Envoy's list names every session of the fleet, about 1,100 of them each minute, and the
// registration of a session no watched claim records counts for nothing, so the sweep that finds
// it so drops it.
func TestTheKeeperKeepsTheRegistrationsOfItsClaimsSessionsAlone(t *testing.T) {
	r := newReachRig(t, false)
	worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
	operator := reachClaim(t, "LEGION-208", "LEGION-210", claim.RolePlanner, "ses_operator")
	operator.Role = "operator"
	fleet := map[string]time.Duration{"ses_implementer": time.Minute, "ses_operator": time.Minute}
	for i := range 1100 {
		fleet[fmt.Sprintf("ses_fleet_%d", i)] = time.Minute
	}
	r.at(2 * time.Minute)
	r.listing(fleet)
	r.sweep(worker, operator)
	if got := r.seenSessions(); !slices.Equal(got, []string{"ses_implementer"}) {
		t.Fatalf("after a list of %d sessions the keeper keeps the registrations of %d sessions (first %v), want the watched claim's alone",
			len(fleet), len(got), got[:min(3, len(got))])
	}

	// The claim takes a fresh session: the registration of the one it left goes too.
	worker.Session = "ses_fresh"
	r.at(3 * time.Minute)
	r.listing(map[string]time.Duration{"ses_fresh": 3 * time.Minute, "ses_implementer": time.Minute})
	r.sweep(r.with(worker))
	if got := r.seenSessions(); !slices.Equal(got, []string{"ses_fresh"}) {
		t.Fatalf("after the claim took a fresh session the keeper keeps the registrations of %v, want [ses_fresh]", got)
	}
}

// seenSessions is every session whose last registration the keeper keeps, in order.
func (r *reachRig) seenSessions() []string {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	return slices.Sorted(maps.Keys(r.k.seen))
}

// The keeper keeps a record of each claim the daemon still holds, and of no other: a claim that is
// gone from the daemon's claims leaves no record behind, and no write of it is tried again.
func TestTheKeeperForgetsTheRecordOfAClaimTheDaemonNoLongerHolds(t *testing.T) {
	r := newReachRig(t, false)
	worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	r.sweep(worker)
	if !r.reach(worker.Token).Unreachable() {
		t.Fatal("the worker was not marked at 21 minutes")
	}
	r.k.mu.Lock()
	r.k.dirty[worker.Token] = true
	r.k.mu.Unlock()

	// The claim retires and the store deletes it: the next sweep reads no such claim.
	r.at(22 * time.Minute)
	r.sweep()
	r.k.mu.Lock()
	_, kept := r.k.records[worker.Token]
	_, dirty := r.k.dirty[worker.Token]
	r.k.mu.Unlock()
	if kept || dirty {
		t.Fatalf("after its claim was gone the keeper still holds its record (%t) and owes its write (%t), want neither", kept, dirty)
	}
}

// When Envoy's whole list holds no live session for 20 minutes, the daemon marks none, tells the
// controller once and writes R6's line once; the count starts again when the list holds one.
func TestAListWithNoLiveSessionMarksNoneAndIsToldOnce(t *testing.T) {
	r := newReachRig(t, false)
	worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
	for minute := 1; minute <= 45; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.listing(map[string]time.Duration{"ses_implementer": -time.Hour})
		r.sweep(r.with(worker))
	}
	if r.reach(worker.Token).Unreachable() {
		t.Fatalf("with no live session listed the claim reads %+v, want none marked", r.reach(worker.Token))
	}
	if told := r.notices(record.EnvoyNoLiveSessionNotice); len(told) != 1 || told[0].issue != "LEGION" {
		t.Fatalf("the controller was told %+v that Envoy lists nobody, want once on the project", told)
	}
	if got := r.lines(fleetalert.NoLiveSession.Word); got != 1 {
		t.Fatalf("the line was written %d times, want once", got)
	}
	// One session registers again: nothing is marked for 20 minutes from that read.
	r.at(46 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 46 * time.Minute, "ses_implementer": -time.Hour})
	r.sweep(r.with(worker))
	r.at(65 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 65 * time.Minute, "ses_implementer": -time.Hour})
	r.sweep(r.with(worker))
	if r.reach(worker.Token).Unreachable() {
		t.Fatal("the claim was marked less than 20 minutes after the list held a live session again")
	}
	r.at(67 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 67 * time.Minute, "ses_implementer": -time.Hour})
	r.sweep(r.with(worker))
	if !r.reach(worker.Token).Unreachable() {
		t.Fatal("the claim was not marked 21 minutes after the list held a live session again")
	}
}

// A claim reported before a daemon boot is not reported again after it: its record says so.
func TestAClaimReportedBeforeABootIsNotReportedAgainAfterIt(t *testing.T) {
	r := newReachRig(t, false)
	worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
	worker.Reach = supervise.Reach{Session: "ses_implementer", FirstReady: worker.LastReady, Reported: true, ArchitectTold: true}
	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	r.sweep(worker)
	if !r.reach(worker.Token).Unreachable() {
		t.Fatal("the claim was not marked 21 minutes after the boot")
	}
	if told := r.notices(record.UnreachableClaimsNotice); len(told) != 0 {
		t.Fatalf("a claim reported before the boot was listed again: %+v", told)
	}
	if notices := r.architectNotices("LEGION-209"); len(notices) != 0 {
		t.Fatalf("its architect was told again: %+v", notices)
	}
}

// A boot restores a claim the last daemon left mid-launch as launch_uncertain, and the claim
// stays queued until a launch starts. Neither state ends the episode: the claim reported before
// the boot is not reported again after it, and the relaunch's not-back notice still comes 20
// minutes after that relaunch (R5).
func TestAClaimRestoredMidLaunchKeepsItsEpisode(t *testing.T) {
	for _, restored := range []supervise.ClaimState{supervise.StateLaunchUncertain, supervise.StateQueued} {
		t.Run(string(restored), func(t *testing.T) {
			r := newReachRig(t, false)
			worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
			r.at(21 * time.Minute)
			r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
			r.sweep(worker)
			// The controller's relaunch at 25 minutes launches generation 4, which its launch
			// records on the claim's own row.
			worker.Generation++
			worker.LastRelaunch = reachBoot.Add(25 * time.Minute)
			r.at(26 * time.Minute)
			r.sweep(r.with(worker))
			if got := r.reach(worker.Token); !got.Reported || !got.Relaunched {
				t.Fatalf("before the boot the claim reads %+v, want it reported and relaunched", got)
			}

			// The daemon ends during that launch and boots again at 30 minutes. The boot
			// restores the claim as restored, with the reach record the store holds.
			booted := newReachRig(t, false)
			booted.k.boot = reachBoot.Add(30 * time.Minute)
			booted.puts[worker.Token] = r.reach(worker.Token)
			booted.at(31 * time.Minute)
			booted.listing(map[string]time.Duration{"ses_other": 31 * time.Minute})
			stored := booted.with(worker)
			stored.State = restored
			booted.sweep(stored)
			if got := booted.reach(worker.Token); !got.Reported || !got.Relaunched {
				t.Fatalf("a claim restored as %s reads %+v, want its episode kept: reported and relaunched", restored, got)
			}

			// The launch comes up and the claim is still out of reach.
			for minute := 32; minute <= 60; minute++ {
				booted.at(time.Duration(minute) * time.Minute)
				booted.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
				booted.sweep(booted.with(worker))
			}
			if told := booted.notices(record.UnreachableClaimsNotice); len(told) != 0 {
				t.Fatalf("a claim reported before the boot and restored as %s was listed again: %+v", restored, told)
			}
			if notBack := booted.notices(record.RelaunchNotBackNotice); len(notBack) != 1 || notBack[0].notice.Claim != string(worker.Token) {
				t.Fatalf("not-back notices after a restore as %s = %+v, want one 20 minutes after the relaunch", restored, notBack)
			}
		})
	}
}

// The tree's architect is told of each claim of its tree but its own, and the controller's list
// names every claim, the architect's included. A list the outbox did not take leaves its claims
// unreported, to be listed again at the next pass.
func TestTheArchitectIsToldOfItsTreesClaimsButItsOwn(t *testing.T) {
	r := newReachRig(t, false)
	architect := reachClaim(t, "LEGION-208", "LEGION-208", claim.RoleArchitect, "ses_architect")
	worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
	r.mu.Lock()
	r.refuseTell = true
	r.mu.Unlock()
	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	r.sweep(architect, worker)
	if r.reach(worker.Token).Reported {
		t.Fatal("a claim whose list the outbox did not take is marked reported")
	}
	r.mu.Lock()
	r.refuseTell = false
	r.mu.Unlock()
	r.at(22 * time.Minute)
	r.sweep(r.with(architect), r.with(worker))

	told := r.notices(record.UnreachableClaimsNotice)
	if len(told) != 1 || len(told[0].notice.Unreachable) != 2 {
		t.Fatalf("the controller was told %+v, want one list of both claims", told)
	}
	if notices := r.architectNotices("LEGION-209"); len(notices) != 1 || notices[0].Kind != record.ClaimOutOfReachNotice ||
		notices[0].Claim != string(worker.Token) {
		t.Fatalf("the architect's notices on LEGION-209 = %+v, want one naming the implementer's claim", notices)
	}
	if notices := r.architectNotices("LEGION-208"); len(notices) != 0 {
		t.Fatalf("the architect was told of its own claim: %+v", notices)
	}
	r.at(23 * time.Minute)
	r.sweep(r.with(architect), r.with(worker))
	if notices := r.architectNotices("LEGION-209"); len(notices) != 1 {
		t.Fatalf("the architect was told %d times of one episode, want once", len(notices))
	}
}

// The four per-claim notices: the guard would now let a refused claim through; the guard has
// refused it for 60 minutes; a relaunch reached it and it is still out of reach 20 minutes later;
// and a marked claim that owes no task was relaunched by the daemon's own supervision, which is
// its process exiting at its Envoy role claim. Each is sent once.
func TestEachPerClaimNoticeOfAnUnreachableClaimIsSentOnce(t *testing.T) {
	t.Run("the guard would now let it through, and then 60 minutes of refusal", func(t *testing.T) {
		r := newReachRig(t, false)
		worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
		worker.ActivityAt, worker.ActivityBackground, worker.ActivityKinds = reachBoot, true, []string{"async-job"}
		r.at(21 * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
		r.sweep(worker)
		if err := r.k.guardRefused(context.Background(), worker.Token, reachBoot.Add(25*time.Minute)); err != nil {
			t.Fatalf("record the guard's refusal: %v", err)
		}
		for minute := 26; minute <= 90; minute++ {
			r.at(time.Duration(minute) * time.Minute)
			r.sweep(r.with(worker))
		}
		if held := r.notices(record.RelaunchGuardHeldNotice); len(held) != 1 || held[0].notice.Claim != string(worker.Token) || held[0].issue != "LEGION-209" {
			t.Fatalf("guard-held notices = %+v, want one at 60 minutes of refusal", held)
		}
		if ready := r.notices(record.RelaunchReadyNotice); len(ready) != 0 {
			t.Fatalf("relaunch-ready notices = %+v while the job still runs, want none", ready)
		}
		worker.ActivityAt, worker.ActivityBackground, worker.ActivityKinds = reachBoot.Add(90*time.Minute), false, nil
		for minute := 91; minute <= 95; minute++ {
			r.at(time.Duration(minute) * time.Minute)
			r.sweep(r.with(worker))
		}
		if ready := r.notices(record.RelaunchReadyNotice); len(ready) != 1 || ready[0].notice.Claim != string(worker.Token) {
			t.Fatalf("relaunch-ready notices = %+v, want one once the job ended", ready)
		}
	})
	t.Run("still out of reach 20 minutes after a relaunch", func(t *testing.T) {
		r := newReachRig(t, false)
		worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
		r.at(21 * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
		r.sweep(worker)
		// The relaunch's launch records it on the claim's own row, which the next sweep reads.
		worker.Generation++
		worker.LastRelaunch = reachBoot.Add(30 * time.Minute)
		for minute := 31; minute <= 55; minute++ {
			r.at(time.Duration(minute) * time.Minute)
			r.sweep(r.with(worker))
		}
		if notBack := r.notices(record.RelaunchNotBackNotice); len(notBack) != 1 || notBack[0].notice.Claim != string(worker.Token) {
			t.Fatalf("not-back notices = %+v, want one 20 minutes after the relaunch", notBack)
		}
		if exits := r.notices(record.UnreachableExitNotice); len(exits) != 0 {
			t.Fatalf("the relaunch's own launch was told as an exit: %+v", exits)
		}
	})
	t.Run("a marked claim owing no task exits at its role claim", func(t *testing.T) {
		r := newReachRig(t, false)
		planner := reachClaim(t, "LEGION-208", "LEGION-210", claim.RolePlanner, "ses_planner")
		r.at(21 * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
		r.sweep(planner)
		for minute := 22; minute <= 30; minute++ {
			planner.Generation++
			planner.State = supervise.StateLaunching
			r.at(time.Duration(minute) * time.Minute)
			r.sweep(r.with(planner))
		}
		if exits := r.notices(record.UnreachableExitNotice); len(exits) != 1 || exits[0].notice.Claim != string(planner.Token) {
			t.Fatalf("exit notices = %+v, want one at the first exit", exits)
		}
		if !r.reach(planner.Token).Unreachable() {
			t.Fatal("the mark did not hold through the claim's launch states")
		}

		owed := reachClaim(t, "LEGION-208", "LEGION-211", claim.RoleTester, "ses_tester")
		owed.Pending = &supervise.Delivery{ID: "task", Task: "test it"}
		r.sweep(owed)
		r.at(52 * time.Minute)
		r.sweep(r.with(owed))
		owed.Generation++
		r.at(53 * time.Minute)
		r.sweep(r.with(owed))
		if exits := r.notices(record.UnreachableExitNotice); len(exits) != 1 {
			t.Fatalf("a claim owing a task was told as an exit: %+v", exits)
		}
	})
}

// The keeper reads a relaunch before its launch is recorded (LastRelaunchGeneration 0), and the
// launch then lands at a later generation than that first read. Three paths make it so: the
// launch's own loop retries a refused start; the launch is tried again at the probe interval after
// a store that did not answer; and a daemon that ends mid-start comes back with the claim
// launch_uncertain and spawns it once more. Each path is read too while that later try is still
// starting and has recorded no generation. Each is the relaunch's own launch, so none is an exit
// of the claim's.
func TestARelaunchsLaunchAtALaterGenerationIsNoExit(t *testing.T) {
	type read struct {
		generation, recorded uint64
		state                supervise.ClaimState
	}
	for _, tc := range []struct {
		name  string
		reads []read
	}{
		{name: "the launch retries a refused start", reads: []read{
			{generation: 4, state: supervise.StateLaunching},
			{generation: 5, state: supervise.StateLaunching},
			{generation: 5, recorded: 5, state: supervise.StateLaunching},
			{generation: 5, recorded: 5, state: supervise.StateIdle},
		}},
		{name: "the launch is tried again at the probe interval", reads: []read{
			{generation: 3, state: supervise.StateLaunching},
			{generation: 3, state: supervise.StateLaunching},
			{generation: 4, state: supervise.StateLaunching},
			{generation: 4, recorded: 4, state: supervise.StateIdle},
		}},
		{name: "a restart spawns the launch_uncertain claim again", reads: []read{
			{generation: 4, state: supervise.StateLaunchUncertain},
			{generation: 5, state: supervise.StateLaunching},
			{generation: 5, recorded: 5, state: supervise.StateLaunching},
			{generation: 5, recorded: 5, state: supervise.StateIdle},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReachRig(t, false)
			planner := reachClaim(t, "LEGION-208", "LEGION-210", claim.RolePlanner, "ses_planner")
			planner.Generation = 3
			r.at(21 * time.Minute)
			r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
			r.sweep(planner)
			if !r.reach(planner.Token).Unreachable() {
				t.Fatal("the planner was not marked at 21 minutes")
			}
			planner.LastRelaunch = reachBoot.Add(25 * time.Minute)
			for i, step := range tc.reads {
				planner.Generation, planner.LastRelaunchGeneration, planner.State = step.generation, step.recorded, step.state
				r.at(time.Duration(26+i) * time.Minute)
				r.sweep(r.with(planner))
			}
			if exits := r.notices(record.UnreachableExitNotice); len(exits) != 0 {
				t.Fatalf("the relaunch's own launch at a later generation was told as an exit: %+v", exits)
			}
			if got := r.reach(planner.Token); got.Generation != planner.Generation || !got.Relaunched {
				t.Fatalf("the record reads %+v, want it relaunched and counting from the launch's generation %d", got, planner.Generation)
			}

			// A later launch of the claim than the relaunch's own is still the exit it is.
			planner.Generation++
			r.at(40 * time.Minute)
			r.sweep(r.with(planner))
			if exits := r.notices(record.UnreachableExitNotice); len(exits) != 1 {
				t.Fatalf("exit notices after a launch past the relaunch's = %+v, want one", exits)
			}
		})
	}
}

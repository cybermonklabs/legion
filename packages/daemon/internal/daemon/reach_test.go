package daemon

// The daemon's half of R5 and R6: once a minute it reads Envoy's session list, marks a claim whose
// session's last registration is more than 20 minutes old — counted from the latest of that
// registration, its boot, the session's first ready, the end of its last failed read and its last
// read that found no live session — relaunches the claim, and writes R6's line for what no agent
// can act on. The keeper is driven directly over fakes of the list, the store's write, the
// relaunch and the clock, because what it decides is decided from the claims it is handed and
// what it read.

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
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// reachBoot is the moment the rig's daemon booted.
var reachBoot = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

type reachRig struct {
	t      *testing.T
	k      *reachKeeper
	logged *bytes.Buffer

	mu       sync.Mutex
	now      time.Time
	sessions []notify.ListedSession
	// roles is the Envoy roles each session holds in the list listing writes.
	roles   map[string][]string
	listErr error
	puts    map[claim.Token]supervise.Reach
	// held is each claim an operator's suspension holds (operatorHolds).
	held       map[claim.Token]bool
	relaunched []supervise.Claim
	relaunch   error
}

func newReachRig(t *testing.T, daemonController bool) *reachRig {
	t.Helper()
	r := &reachRig{t: t, logged: &bytes.Buffer{}, now: reachBoot, puts: map[claim.Token]supervise.Reach{},
		roles: map[string][]string{}, held: map[claim.Token]bool{}}
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
		operatorHolds: func(token claim.Token) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.held[token]
		},
		alerts: fleetalert.New(log, "legion"),
		relaunch: func(_ context.Context, c supervise.Claim, at time.Time) (supervise.Claim, error) {
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
	return r
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

// sweep is one minute's pass over claims, with the relaunches it may start finished.
func (r *reachRig) sweep(claims ...supervise.Claim) {
	r.t.Helper()
	r.k.sweep(context.Background(), claims)
	r.k.wait()
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
	if r.reach(worker.Token).Unreachable() {
		t.Fatalf("at 19 minutes after the boot the claim reads %+v; the boot counts", r.reach(worker.Token))
	}

	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_implementer": -time.Hour, "ses_other": 21 * time.Minute})
	r.sweep(r.with(worker))
	got := r.reach(worker.Token)
	if !got.Unreachable() || !got.Since.Equal(reachBoot) || !got.LastSeen.Equal(reachBoot.Add(-time.Hour)) {
		t.Fatalf("at 21 minutes the claim's reach = %+v, want it marked since the boot with its last registration read", got)
	}

	// Its session registers: reachable again, and reported again only once it is out of reach again.
	r.at(41 * time.Minute)
	r.listing(map[string]time.Duration{"ses_implementer": 41 * time.Minute})
	r.sweep(r.with(worker))
	if r.reach(worker.Token).Unreachable() {
		t.Fatalf("a claim whose session registered reads %+v, want it reachable and its report over", r.reach(worker.Token))
	}
	r.at(60 * time.Minute)
	r.sweep(r.with(worker))
	if r.reach(worker.Token).Unreachable() {
		t.Fatal("19 minutes after its registration the claim is marked again")
	}
	r.at(62 * time.Minute)
	r.sweep(r.with(worker))
	if !r.reach(worker.Token).Unreachable() {
		t.Fatal("21 minutes after its registration the claim is not marked again")
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

	r.mu.Lock()
	r.listErr = errors.New("listener returned 503 Service Unavailable: refilling")
	r.mu.Unlock()
	for minute := 26; minute <= 45; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.sweep(r.with(worker))
	}
	if !r.reach(worker.Token).Unreachable() {
		t.Fatalf("the failed reads changed the mark to %+v", r.reach(worker.Token))
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
// yet: the 20 minutes count from it. A mark already made it leaves as it is — its moment and its
// last registration — the minute it happens and the minute after, when the list answers again
// with the claim's session still unregistered.
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
			if !marked.Unreachable() {
				t.Fatalf("at 25 minutes the claim reads %+v, want it marked", marked)
			}

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
				if !got.Unreachable() || !got.Since.Equal(marked.Since) || !got.LastSeen.Equal(marked.LastSeen) {
					t.Fatalf("at %d minutes, after %s at 26, the claim reads %+v, want its mark %+v as it was", minute, name, got, marked)
				}
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

// When Envoy's whole list holds no live session for 20 minutes, the daemon marks none and writes
// R6's line once; the count starts again when the list holds one.
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

// A boot restores a claim the last daemon left mid-launch as launch_uncertain, and the claim
// stays queued until a launch starts. Neither state ends the episode: the relaunch made before
// the boot is still the episode's, and R6's line still comes 20 minutes after it (R5).
func TestAClaimRestoredMidLaunchKeepsItsEpisode(t *testing.T) {
	for _, restored := range []supervise.ClaimState{supervise.StateLaunchUncertain, supervise.StateQueued} {
		t.Run(string(restored), func(t *testing.T) {
			r := newReachRig(t, false)
			worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
			r.at(21 * time.Minute)
			r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
			r.sweep(worker)
			// A relaunch at 25 minutes launches generation 4, which its launch records on the
			// claim's own row.
			worker.Generation++
			worker.LastRelaunch = reachBoot.Add(25 * time.Minute)
			r.at(26 * time.Minute)
			r.sweep(r.with(worker))
			if got := r.reach(worker.Token); !got.Unreachable() || !got.Relaunched {
				t.Fatalf("before the boot the claim reads %+v, want it marked and relaunched", got)
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
			if got := booted.reach(worker.Token); !got.Unreachable() || !got.Relaunched {
				t.Fatalf("a claim restored as %s reads %+v, want its episode kept: marked and relaunched", restored, got)
			}

			// The launch comes up and the claim is still out of reach.
			for minute := 32; minute <= 60; minute++ {
				booted.at(time.Duration(minute) * time.Minute)
				booted.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
				booted.sweep(booted.with(worker))
			}
			if lines := booted.lines(fleetalert.ClaimUnreachable.Word); lines != 1 {
				t.Fatalf("a claim restored as %s wrote R6's not-back line %d times, want once, 20 minutes after the relaunch", restored, lines)
			}
			if len(booted.relaunched) != 0 {
				t.Fatalf("a claim relaunched before the boot was relaunched again within 6 hours: %+v", booted.relaunched)
			}
		})
	}
}

// An operator's suspension held over a claim is read before anything else of its relaunch, so it
// writes no line on any path: not the guard's after 60 minutes, which a claim with background work
// running would reach, nor a refusal's. It is logged once an episode, and the daemon asks the
// runtime nothing.
func TestAnOperatorsSuspensionWritesNoLineOnAnyPath(t *testing.T) {
	r := newReachRig(t, false)
	worker := settledWorker(t)
	worker.ActivityBackground, worker.ActivityKinds = true, []string{"async-job"}
	r.mu.Lock()
	r.held[worker.Token] = true
	r.mu.Unlock()
	for minute := 21; minute <= 100; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
		r.sweep(r.with(worker))
	}
	if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word) + r.lines(fleetalert.ClaimUnreachable.Word); lines != 0 {
		t.Fatalf("R6 wrote %d lines for a claim an operator's suspension holds, want none", lines)
	}
	if len(r.relaunched) != 0 {
		t.Fatalf("the daemon asked to relaunch %+v while an operator's suspension holds it, want nothing", r.relaunched)
	}
	if logged := strings.Count(r.logged.String(), "an operator's suspension holds the daemon's relaunch"); logged != 1 {
		t.Fatalf("the operator's suspension was logged %d times over 80 sweeps, want once", logged)
	}
	if got := r.reach(worker.Token); got.SuspensionHeldAt.IsZero() || !got.RefusedAt.IsZero() {
		t.Fatalf("the record = %+v, want the operator's suspension recorded and no refusal", got)
	}
}

// settledWorker is an implementer's claim the relaunch guard lets through: its last activity
// report, made after its launch, shows nothing running, and it is in no turn.
func settledWorker(t *testing.T) supervise.Claim {
	worker := reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer")
	worker.ActivityAt = reachBoot
	return worker
}

// The daemon relaunches a workflow role's claim it has marked out of reach, on its same session,
// once, and no agent is asked to.
func TestTheDaemonRelaunchesAWorkerClaimItCannotReach(t *testing.T) {
	r := newReachRig(t, false)
	worker := settledWorker(t)
	r.at(19 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 19 * time.Minute})
	r.sweep(worker)
	if len(r.relaunched) != 0 {
		t.Fatalf("a claim not yet out of reach was relaunched: %+v", r.relaunched)
	}
	for minute := 21; minute <= 39; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
		r.sweep(r.with(worker))
	}
	if len(r.relaunched) != 1 || r.relaunched[0].Token != worker.Token {
		t.Fatalf("the daemon relaunched %+v, want the implementer's claim once", r.relaunched)
	}
	if got := r.reach(worker.Token); !got.Unreachable() || !got.Relaunched {
		t.Fatalf("after the relaunch the claim reads %+v, want it marked and relaunched", got)
	}
	if lines := r.lines(fleetalert.ClaimUnreachable.Word) + r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 0 {
		t.Fatalf("R6 wrote %d lines for a relaunch still inside its 20 minutes, want none", lines)
	}
}

// Each case the daemon's relaunch of a workflow claim cannot get past writes R6's line for that
// claim once.
func TestACaseForAPersonWritesTheClaimsLineOnce(t *testing.T) {
	t.Run("the guard refuses it for 60 minutes", func(t *testing.T) {
		r := newReachRig(t, false)
		worker := settledWorker(t)
		worker.ActivityBackground, worker.ActivityKinds = true, []string{"async-job"}
		sweepTo := func(last int, from int) {
			for minute := from; minute <= last; minute++ {
				r.at(time.Duration(minute) * time.Minute)
				r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
				r.sweep(r.with(worker))
			}
		}
		sweepTo(80, 21)
		if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 0 {
			t.Fatalf("R6 wrote %d refused lines before 60 minutes of refusal, want none", lines)
		}
		sweepTo(100, 81)
		if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 1 {
			t.Fatalf("R6 wrote %d refused lines after 60 minutes of refusal, want one", lines)
		}
		if len(r.relaunched) != 0 {
			t.Fatalf("relaunched %+v, want none", r.relaunched)
		}
	})
	t.Run("its relaunch did not bring it back", func(t *testing.T) {
		r := newReachRig(t, false)
		worker := settledWorker(t)
		for minute := 21; minute <= 60; minute++ {
			r.at(time.Duration(minute) * time.Minute)
			r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
			r.sweep(r.with(worker))
			if minute == 40 && r.lines(fleetalert.ClaimUnreachable.Word) != 0 {
				t.Fatal("R6 wrote the not-back line inside the 20 minutes after the relaunch")
			}
		}
		if lines := r.lines(fleetalert.ClaimUnreachable.Word); lines != 1 || len(r.relaunched) != 1 {
			t.Fatalf("not-back lines %d, relaunches %d; want 1 and 1", lines, len(r.relaunched))
		}
	})
	t.Run("its launcher is not connected", func(t *testing.T) {
		r := newReachRig(t, false)
		r.relaunch = runtime.ErrLauncherNotConnected
		worker := settledWorker(t)
		for minute := 21; minute <= 30; minute++ {
			r.at(time.Duration(minute) * time.Minute)
			r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
			r.sweep(r.with(worker))
		}
		if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 1 {
			t.Fatalf("refused lines %d, want 1", lines)
		}
		if len(r.relaunched) < 2 || r.reach(worker.Token).Relaunched {
			t.Fatalf("the refused relaunch was asked %d times and the claim reads %+v; want it asked again at each sweep and not relaunched",
				len(r.relaunched), r.reach(worker.Token))
		}
	})
	for _, refused := range []struct {
		name string
		err  error
	}{
		{"its tree allows it no launch", &supervise.RefusedError{State: supervise.StateIdle, Request: "relaunch",
			Reason: "the claim's tree allows it no launch", Err: fmt.Errorf("%w: cleanup reserved", supervise.ErrTreeRefusesLaunch)}},
		{"a wake of it waits its turn", &supervise.RefusedError{State: supervise.StateIdle, Request: "relaunch",
			Reason: "a resume of the claim waits its turn in the wake pacing", Err: errors.New("a resume of the claim waits its turn in the wake pacing")}},
	} {
		t.Run(refused.name+" for 60 minutes", func(t *testing.T) {
			r := newReachRig(t, false)
			r.relaunch = refused.err
			worker := settledWorker(t)
			for minute := 21; minute <= 80; minute++ {
				r.at(time.Duration(minute) * time.Minute)
				r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
				r.sweep(r.with(worker))
			}
			if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 0 {
				t.Fatalf("R6 wrote %d refused lines inside 60 minutes of the claim's machine refusing its relaunch, want none", lines)
			}
			for minute := 81; minute <= 100; minute++ {
				r.at(time.Duration(minute) * time.Minute)
				r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
				r.sweep(r.with(worker))
			}
			if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 1 {
				t.Fatalf("R6 wrote %d refused lines after 60 minutes of the claim's machine refusing its relaunch, want one", lines)
			}
			if len(r.relaunched) < 60 || r.reach(worker.Token).Relaunched {
				t.Fatalf("the refused relaunch was asked %d times and the claim reads %+v; want it asked again at each sweep and not relaunched",
					len(r.relaunched), r.reach(worker.Token))
			}
		})
	}
}

// An operator's suspension held over the claim is a person's own act on it, so its refusal of the
// daemon's relaunch writes no line however long it lasts: it is logged once, and the relaunch is
// asked for again at each sweep.
func TestAnOperatorsSuspensionHoldsTheRelaunchWithNoLine(t *testing.T) {
	r := newReachRig(t, false)
	r.relaunch = &supervise.RefusedError{State: supervise.StateWorking, Request: "relaunch",
		Reason: supervise.ErrOperatorSuspensionHeld.Error(), Err: supervise.ErrOperatorSuspensionHeld}
	worker := settledWorker(t)
	for minute := 21; minute <= 100; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
		r.sweep(r.with(worker))
	}
	if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word) + r.lines(fleetalert.ClaimUnreachable.Word); lines != 0 {
		t.Fatalf("R6 wrote %d lines for a claim an operator's suspension holds, want none", lines)
	}
	if logged := strings.Count(r.logged.String(), "an operator's suspension holds the daemon's relaunch"); logged != 1 {
		t.Fatalf("the operator's suspension was logged %d times over 80 sweeps, want once", logged)
	}
	if len(r.relaunched) < 80 {
		t.Fatalf("the relaunch was asked %d times, want it asked again at each sweep", len(r.relaunched))
	}
}

// A pod that holds two or more live claims, every one out of reach, is a case for a person: the
// daemon relaunches none of them, since a relaunch into that pod reaches Envoy for none, and
// writes each claim's line once. Once one of them registers again, the daemon relaunches the
// other.
func TestAPodWhoseEveryClaimIsOutOfReachIsACaseForAPerson(t *testing.T) {
	r := newReachRig(t, false)
	pod := func(c supervise.Claim) supervise.Claim {
		c.ActivityAt = reachBoot
		c.Locator = &runtime.Locator{Runtime: runtime.RuntimeSandbox, Claim: c.Token, Incarnation: "pod-a:3",
			Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-208", PodUID: "pod-a", Container: "roles", Generation: 3}}
		return c
	}
	worker := pod(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))
	tester := pod(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleTester, "ses_tester"))
	for minute := 21; minute <= 30; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
		r.sweep(r.with(worker), r.with(tester))
	}
	if len(r.relaunched) != 0 {
		t.Fatalf("the daemon relaunched %+v into a pod whose every claim is out of reach, want none", r.relaunched)
	}
	if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 2 {
		t.Fatalf("R6 wrote %d refused lines for the pod's two claims over ten sweeps, want one for each", lines)
	}
	// The tester registers again: the pod reaches Envoy for one claim, and the worker is relaunched.
	r.at(31 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 31 * time.Minute, "ses_tester": 31 * time.Minute})
	r.sweep(r.with(worker), r.with(tester))
	if len(r.relaunched) != 1 || r.relaunched[0].Token != worker.Token {
		t.Fatalf("with the tester back in reach the daemon relaunched %+v, want the implementer's claim once", r.relaunched)
	}
}

// Two claims of one pod that go dark together are marked a minute apart, by when each last
// registered. At the first mark the other claim is not marked yet, but Envoy no longer lists its
// session as live: the pod reaches Envoy for neither, so the daemon relaunches neither and writes
// the first claim's line. Read as reaching Envoy until it is marked, the other claim would let the
// first be relaunched into the pod that broke.
func TestAPodClaimNotYetMarkedButNotLiveReachesNoEnvoy(t *testing.T) {
	r := newReachRig(t, false)
	pod := func(c supervise.Claim) supervise.Claim {
		c.ActivityAt = reachBoot
		c.Locator = &runtime.Locator{Runtime: runtime.RuntimeSandbox, Claim: c.Token, Incarnation: "pod-a:3",
			Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-208", PodUID: "pod-a", Container: "roles", Generation: 3}}
		return c
	}
	worker := pod(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))
	tester := pod(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleTester, "ses_tester"))
	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute, "ses_implementer": 0, "ses_tester": 90 * time.Second})
	r.sweep(worker, tester)
	if !r.reach(worker.Token).Unreachable() || r.reach(tester.Token).Unreachable() {
		t.Fatalf("at 21 minutes the implementer reads %+v and the tester %+v, want the implementer marked and the tester not yet",
			r.reach(worker.Token), r.reach(tester.Token))
	}
	if len(r.relaunched) != 0 {
		t.Fatalf("the daemon relaunched %+v into a pod whose other claim Envoy lists as unresponsive, want none", r.relaunched)
	}
	if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 1 {
		t.Fatalf("R6 wrote %d refused lines for the implementer, want one naming the pod", lines)
	}
}

// A relaunch the runtime refuses only after a later sweep found the claim's session registered
// again answers an episode that has ended: it writes no line for a claim that is back in reach.
func TestALateRefusalOfAnEndedEpisodeWritesNoLine(t *testing.T) {
	r := newReachRig(t, false)
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	r.k.relaunch = func(context.Context, supervise.Claim, time.Time) (supervise.Claim, error) {
		asked <- struct{}{}
		<-release
		return supervise.Claim{}, runtime.ErrLauncherNotConnected
	}
	worker := settledWorker(t)
	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	r.k.sweep(context.Background(), []supervise.Claim{worker})
	<-asked
	if !r.reach(worker.Token).Unreachable() {
		t.Fatalf("at 21 minutes the claim reads %+v, want it marked", r.reach(worker.Token))
	}
	r.at(22 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 22 * time.Minute, "ses_implementer": 22 * time.Minute})
	r.k.sweep(context.Background(), []supervise.Claim{r.with(worker)})
	if r.reach(worker.Token).Unreachable() {
		t.Fatalf("with its session registered again at 22 minutes the claim reads %+v, want its mark ended", r.reach(worker.Token))
	}
	close(release)
	r.k.wait()
	if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 0 {
		t.Fatalf("R6 wrote %d refused lines for a claim back in reach before its relaunch was refused, want none", lines)
	}
}

// sweepMinutes sweeps claims once a minute from from to last, the list holding only another
// session registered that minute, so every claim of claims stays out of reach. Each sweep reads
// each claim of claims() as the store holds it then.
func (r *reachRig) sweepMinutes(from, last int, claims func() []supervise.Claim) {
	r.t.Helper()
	for minute := from; minute <= last; minute++ {
		r.at(time.Duration(minute) * time.Minute)
		r.listing(map[string]time.Duration{"ses_other": time.Duration(minute) * time.Minute})
		r.sweep(claims()...)
	}
}

// R6's 60 minutes count every refusal of a workflow claim's relaunch on one clock, from the
// first: 30 minutes of the guard's refusals and then the claim's machine's write the line 60
// minutes after the guard's first, not 60 after the machine's.
func TestEveryRefusalOfARelaunchCountsOnOneClock(t *testing.T) {
	r := newReachRig(t, false)
	quiet := settledWorker(t)
	busy := quiet
	busy.ActivityBackground, busy.ActivityKinds = true, []string{"async-job"}
	r.sweepMinutes(21, 50, func() []supervise.Claim { return []supervise.Claim{r.with(busy)} })
	r.mu.Lock()
	r.relaunch = &supervise.RefusedError{State: supervise.StateIdle, Request: "relaunch",
		Reason: "a resume of the claim waits its turn in the wake pacing", Err: errors.New("a resume of the claim waits its turn in the wake pacing")}
	r.mu.Unlock()
	r.sweepMinutes(51, 80, func() []supervise.Claim { return []supervise.Claim{r.with(quiet)} })
	if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 0 {
		t.Fatalf("R6 wrote %d refused lines inside 60 minutes of the first refusal, want none", lines)
	}
	r.sweepMinutes(81, 81, func() []supervise.Claim { return []supervise.Claim{r.with(quiet)} })
	if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 1 {
		t.Fatalf("R6 wrote %d refused lines 60 minutes after the guard's first refusal, the machine's refusals since, want one", lines)
	}
}

// An operator's suspension is no refusal time: the guard's refusals before the operator held the
// claim end with the hold, so after its release the 60 minutes count again from the next refusal,
// and the hold's own 50 minutes write no line.
func TestAnOperatorsHoldEndsTheRefusalsCountedBeforeIt(t *testing.T) {
	r := newReachRig(t, false)
	busy := settledWorker(t)
	busy.ActivityBackground, busy.ActivityKinds = true, []string{"async-job"}
	claims := func() []supervise.Claim { return []supervise.Claim{r.with(busy)} }
	r.sweepMinutes(21, 50, claims)
	r.mu.Lock()
	r.held[busy.Token] = true
	r.mu.Unlock()
	r.sweepMinutes(51, 100, claims)
	r.mu.Lock()
	r.held[busy.Token] = false
	r.mu.Unlock()
	r.sweepMinutes(101, 160, claims)
	if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 0 {
		t.Fatalf("R6 wrote %d refused lines inside 60 minutes of the first refusal after the operator's release, want none", lines)
	}
	r.sweepMinutes(161, 161, claims)
	if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 1 {
		t.Fatalf("R6 wrote %d refused lines 60 minutes after the release, want one", lines)
	}
}

// A relaunch in flight that the claim's machine refuses because the claim's process was let go
// meanwhile writes no line, even for the controller, whose refusals write it at once: an
// operator's suspension or retirement of the claim is a person's own act, and a claim let go has
// no process to relaunch. The refusal names the state the claim moved to and no sentinel, as the
// machine's table refuses a relaunch of a claim in a state it ignores the request in.
func TestARefusalOfAClaimLetGoWhileItsRelaunchWaitedWritesNoLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    supervise.ClaimState
		operator bool
	}{
		{"an operator suspended it", supervise.StateSuspended, true},
		{"it was retired", supervise.StateRetired, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReachRig(t, true)
			project, _ := claim.ProjectToken("legion")
			controller := supervise.Claim{Token: claim.ControllerToken(project), Project: project, Role: claim.RoleController,
				Generation: 1, State: supervise.StateIdle, Session: "ses_controller", LastReady: reachBoot.Add(-time.Hour),
				Locator: pluginTestLocator(claim.ControllerToken(project)), ActivityAt: reachBoot}
			release := make(chan struct{})
			asked := make(chan struct{}, 1)
			r.k.relaunch = func(context.Context, supervise.Claim, time.Time) (supervise.Claim, error) {
				asked <- struct{}{}
				<-release
				return supervise.Claim{}, &supervise.RefusedError{State: tc.state, Request: "relaunch", Reason: "no process of this claim is running"}
			}
			r.at(21 * time.Minute)
			r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
			r.k.sweep(context.Background(), []supervise.Claim{controller})
			<-asked
			r.mu.Lock()
			r.held[controller.Token] = tc.operator
			r.mu.Unlock()
			close(release)
			r.k.wait()
			if lines := r.lines(fleetalert.ControllerRelaunchRefused.Word); lines != 0 {
				t.Fatalf("R6 wrote %d refused lines for a controller whose relaunch was refused because %s meanwhile, want none", lines, tc.name)
			}
		})
	}
}

// A paused project starts no turn of its roles, so while it is paused no workflow claim of it is
// named in any of R6's lines: not a pod whose every claim is out of reach, and not a claim its
// relaunch has not brought back. The same pod unpaused writes each claim's line, as
// TestAPodWhoseEveryClaimIsOutOfReachIsACaseForAPerson shows.
func TestAPausedProjectNamesNoWorkflowClaimInAnyLine(t *testing.T) {
	pod := func(c supervise.Claim) supervise.Claim {
		c.ActivityAt = reachBoot
		c.Locator = &runtime.Locator{Runtime: runtime.RuntimeSandbox, Claim: c.Token, Incarnation: "pod-a:3",
			Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-208", PodUID: "pod-a", Container: "roles", Generation: 3}}
		return c
	}
	t.Run("a pod whose every claim is out of reach", func(t *testing.T) {
		r := newReachRig(t, false)
		r.k.paused = func() bool { return true }
		worker := pod(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleImplementer, "ses_implementer"))
		tester := pod(reachClaim(t, "LEGION-208", "LEGION-209", claim.RoleTester, "ses_tester"))
		r.sweepMinutes(21, 40, func() []supervise.Claim { return []supervise.Claim{r.with(worker), r.with(tester)} })
		if lines := r.lines(fleetalert.ClaimRelaunchRefused.Word); lines != 0 || len(r.relaunched) != 0 {
			t.Fatalf("while paused: %d refused lines and relaunches %+v, want neither", lines, r.relaunched)
		}
	})
	t.Run("a claim its relaunch has not brought back", func(t *testing.T) {
		r := newReachRig(t, false)
		paused := false
		r.k.paused = func() bool { return paused }
		worker := settledWorker(t)
		r.sweepMinutes(21, 21, func() []supervise.Claim { return []supervise.Claim{r.with(worker)} })
		if len(r.relaunched) != 1 {
			t.Fatalf("relaunches before the pause = %+v, want the worker's", r.relaunched)
		}
		paused = true
		r.sweepMinutes(22, 60, func() []supervise.Claim { return []supervise.Claim{r.with(worker)} })
		if lines := r.lines(fleetalert.ClaimUnreachable.Word); lines != 0 {
			t.Fatalf("R6 wrote %d not-back lines while the project was paused, want none", lines)
		}
		paused = false
		r.sweepMinutes(61, 61, func() []supervise.Claim { return []supervise.Claim{r.with(worker)} })
		if lines := r.lines(fleetalert.ClaimUnreachable.Word); lines != 1 {
			t.Fatalf("R6 wrote %d not-back lines at the first sweep after the resume, want one", lines)
		}
	})
}

// A sweep made while the daemon's relaunch of its controller is in flight reads the row that
// relaunch writes, and judges nothing of it: the relaunch's launch on a fresh session, tried again
// at the probe interval, records the claim launching with no session, which read as an episode's
// end would close the controller's lines and forget the mark. Once the relaunch answers, its
// record carries the episode, so a controller that never comes back gets R6's not-back line 20
// minutes after the relaunch.
func TestASweepDuringTheControllersRelaunchKeepsItsEpisode(t *testing.T) {
	r := newReachRig(t, true)
	project, _ := claim.ProjectToken("legion")
	token := claim.ControllerToken(project)
	controller := supervise.Claim{Token: token, Project: project, Role: claim.RoleController, Generation: 1,
		State: supervise.StateIdle, Session: "ses_controller", LastReady: reachBoot.Add(-time.Hour),
		Locator: pluginTestLocator(token), ActivityAt: reachBoot}
	relaunchedAt := reachBoot.Add(21 * time.Minute)
	launching := controller
	launching.Generation, launching.State, launching.Session, launching.Locator = 2, supervise.StateLaunching, "", nil
	launching.LastRelaunch, launching.LastRelaunchGeneration = relaunchedAt, 0
	asked, release := make(chan struct{}, 1), make(chan struct{})
	r.k.relaunch = func(context.Context, supervise.Claim, time.Time) (supervise.Claim, error) {
		asked <- struct{}{}
		<-release
		return launching, nil
	}

	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	r.k.sweep(context.Background(), []supervise.Claim{controller})
	<-asked
	r.at(22 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 22 * time.Minute})
	r.k.sweep(context.Background(), []supervise.Claim{r.with(launching)})
	close(release)
	r.k.wait()
	if got := r.reach(token); !got.Unreachable() || !got.Relaunched || !got.RelaunchedAt.Equal(relaunchedAt) {
		t.Fatalf("after the relaunch answered the record is %+v, want the controller marked with the relaunch at %v", got, relaunchedAt)
	}
	r.sweepMinutes(23, 45, func() []supervise.Claim { return []supervise.Claim{r.with(launching)} })
	if lines := r.lines(fleetalert.ControllerUnreachable.Word); lines != 1 {
		t.Fatalf("R6 wrote %d controller-unreachable lines for a relaunched controller that never came back, want one", lines)
	}
}

// A relaunch that a suspension of the claim cut, or whose failed stop a waiting suspension now
// decides, answers ErrSuspensionWaiting: the keeper records no refusal and writes no line, even
// for the controller, whose refusals write the line at once, and the next sweep judges the claim
// as the suspension leaves it.
func TestARelaunchASuspensionTookOverWritesNoLine(t *testing.T) {
	r := newReachRig(t, true)
	project, _ := claim.ProjectToken("legion")
	token := claim.ControllerToken(project)
	controller := supervise.Claim{Token: token, Project: project, Role: claim.RoleController, Generation: 1,
		State: supervise.StateIdle, Session: "ses_controller", LastReady: reachBoot.Add(-time.Hour),
		Locator: pluginTestLocator(token), ActivityAt: reachBoot}
	r.mu.Lock()
	r.relaunch = fmt.Errorf("launch the relaunched process: %w: %w", supervise.ErrSuspensionWaiting, context.Canceled)
	r.mu.Unlock()
	r.at(21 * time.Minute)
	r.listing(map[string]time.Duration{"ses_other": 21 * time.Minute})
	r.sweep(controller)
	if lines := r.lines(fleetalert.ControllerRelaunchRefused.Word); lines != 0 {
		t.Fatalf("R6 wrote %d refused lines for a relaunch a suspension took over, want none", lines)
	}
	if got := r.reach(token); !got.RefusedAt.IsZero() {
		t.Fatalf("the record after a relaunch a suspension took over = %+v, want no refusal", got)
	}
}

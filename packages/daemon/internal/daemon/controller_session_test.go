package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// controllerSessionFile is the transcript every kept controller of these tests records.
const controllerSessionFile = "/sessions/controller.jsonl"

// The machine's lines of R3 (supervise's replaceAgedSession, dispatch://INFRA-2045 R3 and R4),
// which an operator and the after-merge check read whole.
const (
	agedOutLine = "supervise: the controller's session is older than controller_session_max_age_hours; this relaunch starts a fresh session in its place"
	unagedLine  = "supervise: the controller's session id is no UUIDv7, so controller_session_max_age_hours cannot read its age and its relaunches resume it"
)

// sessionAged is an Oh My Pi session id (a UUIDv7) whose recorded start is age before now.
func sessionAged(now time.Time, age time.Duration) string {
	ms := now.Add(-age).UnixMilli()
	return fmt.Sprintf("%08x-%04x-7abc-8def-0123456789ab", ms>>16, ms&0xffff)
}

// keptController is the daemon's own controller claim c, stored with a process the fake runtime
// runs (none for a failed or suspended claim) and a shim connected to this daemon, its machines
// built with the shipped session age (72 hours), and its keeper with the shipped turn bound (90
// minutes) and the real clock the supervisor's machines read (stillClock). The keeper's and the
// machines' lines are both written to logs, as JSON.
func keptController(t *testing.T, c supervise.Claim) (*controllerKeeper, *supervisor, *fake.Runtime, *syncBuffer) {
	t.Helper()
	return keptControllerAged(t, c, 72*time.Hour)
}

// keptControllerAged is keptController with the machines' session age maxAge, 0 for the rule off.
func keptControllerAged(t *testing.T, c supervise.Claim, maxAge time.Duration) (*controllerKeeper, *supervisor, *fake.Runtime, *syncBuffer) {
	t.Helper()
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	sup.deps.Log = log
	sup.deps.ControllerSessionMaxAge = maxAge
	token := claim.ControllerToken("legion")
	c.Token, c.Project, c.Role, c.Generation = token, "legion", claim.RoleController, 1
	c.SessionFile = controllerSessionFile
	if c.State != supervise.StateFailed && c.State != supervise.StateSuspended {
		c.Locator = &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "31999:999999",
			Tmux: &runtime.TmuxLocator{Window: "@99", Pane: "%99"}}
	}
	sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	if _, _, err := sup.Create(context.Background(), c, ""); err != nil {
		t.Fatalf("create the controller's claim: %v", err)
	}
	k := newControllerKeeper(context.Background(), sup, "legion", time.Minute, 90*time.Minute, log)
	return k, sup, rt, logs
}

// controllerOf is the controller's claim as its machine holds it now.
func controllerOf(t *testing.T, sup *supervisor) supervise.Claim {
	t.Helper()
	m, ok := sup.Machine(claim.ControllerToken("legion"))
	if !ok {
		t.Fatal("the supervisor holds no controller machine")
	}
	return m.Claim()
}

// controllerDies is the runtime reporting the controller's process gone, displaced or not: a death
// the machine relaunches.
func controllerDies(t *testing.T, sup *supervisor, displaced bool) {
	t.Helper()
	m, _ := sup.Machine(claim.ControllerToken("legion"))
	c := m.Claim()
	if err := m.Handle(context.Background(), supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *c.Locator, Kind: runtime.Gone, At: time.Now(), Displaced: displaced}}); err != nil {
		t.Fatalf("report the controller's process gone: %v", err)
	}
}

// keepOnce is one look of the keeper, which must answer nothing its loop logs at Error.
func keepOnce(t *testing.T, k *controllerKeeper) {
	t.Helper()
	if err := k.keep(); err != nil {
		t.Fatalf("keep: %v", err)
	}
}

// wantUntouched is a controller the keeper's look left as it was: no process stopped, started or
// resumed, and the session it recorded still recorded.
func wantUntouched(t *testing.T, sup *supervisor, rt *fake.Runtime, state supervise.ClaimState, session string) {
	t.Helper()
	for _, method := range []string{"Suspend", "Spawn", "Resume"} {
		if calls := rt.CallsOf(method); len(calls) != 0 {
			t.Fatalf("the runtime was asked %d times to %s, want never", len(calls), method)
		}
	}
	if c := controllerOf(t, sup); c.State != state || c.Session != session {
		t.Fatalf("the controller is %s on %q, want %s on %q kept", c.State, c.Session, state, session)
	}
}

// wantFreshFor is the controller's latest launch a fresh agent in place of old, for its age: a
// spawn that resumes nothing, no resume, the old session recorded as the one left behind for its
// age, and one R3 Warn line naming it.
func wantFreshFor(t *testing.T, sup *supervisor, rt *fake.Runtime, logs *syncBuffer, old string) {
	t.Helper()
	if resumes, spawns := rt.CallsOf("Resume"), rt.CallsOf("Spawn"); len(resumes) != 0 || len(spawns) != 1 || spawns[0].Spec.ResumeSessionFile != "" {
		t.Fatalf("the relaunch started %d resumes and %d spawns (%+v), want one fresh spawn", len(resumes), len(spawns), spawns)
	}
	c := controllerOf(t, sup)
	if c.Session != "" || c.Abandoned.Session != old || !strings.Contains(c.Abandoned.Why, "older than controller_session_max_age_hours") {
		t.Fatalf("the controller records %q and abandoned %+v, want %s left behind for its age", c.Session, c.Abandoned, old)
	}
	lines := linesOf(t, logs, agedOutLine)
	if len(lines) != 1 || lines[0]["level"] != "WARN" || lines[0]["session"] != old || lines[0]["requirement"] != "R3" {
		t.Fatalf("the R3 lines are %+v, want one at Warn naming %s", lines, old)
	}
}

// wantResumed is the controller's latest launch a resume of session, with no spawn.
func wantResumed(t *testing.T, rt *fake.Runtime, logs *syncBuffer) {
	t.Helper()
	if resumes, spawns := rt.CallsOf("Resume"), rt.CallsOf("Spawn"); len(resumes) != 1 || len(spawns) != 0 || resumes[0].Spec.ResumeSessionFile != controllerSessionFile {
		t.Fatalf("the relaunch started %d resumes and %d spawns, want the one resume of %s", len(resumes), len(spawns), controllerSessionFile)
	}
	if lines := linesOf(t, logs, agedOutLine); len(lines) != 0 {
		t.Fatalf("the relaunch wrote %d R3 lines for a session it kept, want none", len(lines))
	}
}

// linesOf is every JSON line of logs whose message is msg, decoded.
func linesOf(t *testing.T, logs *syncBuffer, msg string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(logs.String(), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode the log line %s: %v", line, err)
		}
		if record["msg"] == msg {
			lines = append(lines, record)
		}
	}
	return lines
}

// The keeper's half of dispatch://INFRA-2045 R2: a controller turn open longer than
// controller_turn_bound_minutes is ended, whether or not steps still arrive in it, and the end
// counts under R1; the next launch resumes the same session unless that end was the session's
// second in a row. internal/supervise's test of the same name holds the machine's side, and
// internal/config's the default of 90. The control: on main the keeper does nothing for a working
// claim, so row (a) fails.
func TestAControllerTurnOpenPastItsBoundIsEnded(t *testing.T) {
	now := time.Now()
	working := func(turnStart time.Time, run supervise.SessionRun) supervise.Claim {
		return supervise.Claim{State: supervise.StateWorking, Session: "ses_controller", LastTurnStart: turnStart,
			LastTurnEnd: turnStart.Add(-time.Minute), SessionRun: run,
			// Steps still arrive in the turn, so the stall backstop, which measures silence, would
			// end nothing.
			Step: supervise.StepRecord{At: now.Add(-time.Minute), TurnStartedAt: turnStart, Tool: "bash", CallID: "toolu_1"}}
	}

	t.Run("a: a turn open one minute past the bound is ended, and the session resumed", func(t *testing.T) {
		k, sup, rt, _ := keptController(t, working(now.Add(-91*time.Minute), supervise.SessionRun{TurnFinished: true}))
		keepOnce(t, k)
		resumes, spawns := rt.CallsOf("Resume"), rt.CallsOf("Spawn")
		if len(resumes) != 1 || len(spawns) != 0 || resumes[0].Spec.ResumeSessionFile != controllerSessionFile {
			t.Fatalf("the keeper's end started %d resumes (%+v) and %d spawns, want the one resume of %s", len(resumes), resumes, len(spawns), controllerSessionFile)
		}
		c := controllerOf(t, sup)
		if c.Session != "ses_controller" || c.SessionRun.Ends != 1 || c.Generation != 2 {
			t.Fatalf("the controller is on %q at generation %d with the run %+v, want ses_controller relaunched at 2 with one end counted",
				c.Session, c.Generation, c.SessionRun)
		}
	})

	t.Run("b: a second such end in a row makes the next launch fresh", func(t *testing.T) {
		k, sup, rt, _ := keptController(t, working(now.Add(-91*time.Minute), supervise.SessionRun{Ends: 1, TurnFinished: true,
			Endings: []string{"its turn stayed open longer than 1h30m0s"}}))
		keepOnce(t, k)
		if resumes, spawns := rt.CallsOf("Resume"), rt.CallsOf("Spawn"); len(resumes) != 0 || len(spawns) != 1 || spawns[0].Spec.ResumeSessionFile != "" {
			t.Fatalf("the keeper's end started %d resumes and %d spawns (%+v), want one fresh spawn", len(resumes), len(spawns), spawns)
		}
		if c := controllerOf(t, sup); c.Session != "" || c.Abandoned.Session != "ses_controller" {
			t.Fatalf("the controller records %q and abandoned %q, want ses_controller left behind", c.Session, c.Abandoned.Session)
		}
	})

	t.Run("c: a turn that ends one minute before the bound changes nothing", func(t *testing.T) {
		k, sup, rt, _ := keptController(t, working(now.Add(-89*time.Minute), supervise.SessionRun{TurnFinished: true}))
		keepOnce(t, k)
		m, _ := sup.Machine(claim.ControllerToken("legion"))
		if err := m.Handle(context.Background(), supervise.StreamTurnEnd{Claim: m.Claim().Token}); err != nil {
			t.Fatalf("end the turn: %v", err)
		}
		// The keeper's clock moves past the moment the turn would have passed its bound.
		k.now = func() time.Time { return now.Add(2 * time.Minute) }
		keepOnce(t, k)
		wantUntouched(t, sup, rt, supervise.StateIdle, "ses_controller")
		if c := controllerOf(t, sup); c.SessionRun.Ends != 0 || !c.SessionRun.TurnFinished {
			t.Fatalf("the controller's run is %+v, want no end counted and the turn finished", c.SessionRun)
		}
	})

	t.Run("d: with controller_turn_bound_minutes 0 a turn open for ten hours changes nothing", func(t *testing.T) {
		k, sup, rt, _ := keptController(t, working(now.Add(-10*time.Hour), supervise.SessionRun{TurnFinished: true}))
		k.turnBound = 0
		keepOnce(t, k)
		wantUntouched(t, sup, rt, supervise.StateWorking, "ses_controller")
	})

	t.Run("a compaction with no turn under it, its last turn begun past the bound, is asked nothing at each look", func(t *testing.T) {
		// compactStart closed the last turn, so the claim stored working shows none open.
		c := working(now.Add(-3*time.Hour), supervise.SessionRun{TurnFinished: true})
		c.LastTurnEnd = now.Add(-2 * time.Hour)
		k, sup, rt, logs := keptController(t, c)
		keepOnce(t, k)
		keepOnce(t, k)
		wantUntouched(t, sup, rt, supervise.StateWorking, "ses_controller")
		if lines := linesOf(t, logs, "supervise: the controller turn the keeper read open past its bound has ended, or another began; it is left alone"); len(lines) != 0 {
			t.Fatalf("the machine was asked about the compaction %d times, want never", len(lines))
		}
	})
}

// dispatch://INFRA-2045 R3: a relaunch the daemon makes of a controller whose session id (a UUIDv7)
// is older than controller_session_max_age_hours forgets the old session inside that relaunch and
// starts a fresh one, with R4's Warn line; the keeper stops no live controller for its age; an
// operator's own resume keeps the session; a session younger than the age, a rule set to 0, and an
// id that is no UUIDv7 keep theirs. internal/config's test of the same name holds the default of
// 72. The control: on main no code reads the age of the controller's session, so every relaunch
// resumes the old session and the (a) rows fail.
func TestAnOldControllerSessionIsReplacedAtADaemonDrivenRelaunch(t *testing.T) {
	now := time.Now()
	idle := func(session string) supervise.Claim {
		return supervise.Claim{State: supervise.StateIdle, Session: session, LastTurnStart: now.Add(-2 * time.Hour),
			LastTurnEnd: now.Add(-time.Hour), SessionRun: supervise.SessionRun{TurnFinished: true}}
	}

	for _, death := range []struct {
		name      string
		displaced bool
	}{{"a death", false}, {"a pod update's displacement", true}} {
		t.Run("a: "+death.name+" of a controller with a session 73 hours old relaunches it in a fresh session", func(t *testing.T) {
			old := sessionAged(now, 73*time.Hour)
			_, sup, rt, logs := keptController(t, idle(old))
			controllerDies(t, sup, death.displaced)
			wantFreshFor(t, sup, rt, logs, old)
		})
	}

	t.Run("a: the keeper's retry of a failed controller with a session 73 hours old relaunches it in a fresh session", func(t *testing.T) {
		old := sessionAged(now, 73*time.Hour)
		k, sup, rt, logs := keptController(t, supervise.Claim{State: supervise.StateFailed, Session: old})
		start := time.Now()
		k.now = func() time.Time { return start }
		keepOnce(t, k)
		k.now = func() time.Time { return start.Add(61 * time.Second) }
		keepOnce(t, k)
		wantFreshFor(t, sup, rt, logs, old)
	})

	for _, state := range []supervise.ClaimState{supervise.StateReady, supervise.StateWorking, supervise.StateIdle} {
		t.Run("b: a live "+string(state)+" controller with a session 73 hours old is not stopped by the keeper", func(t *testing.T) {
			old := sessionAged(now, 73*time.Hour)
			c := idle(old)
			c.State = state
			if state == supervise.StateWorking {
				c.LastTurnStart, c.LastTurnEnd = now.Add(-time.Minute), now.Add(-2*time.Minute)
			}
			k, sup, rt, logs := keptController(t, c)
			keepOnce(t, k)
			keepOnce(t, k)
			wantUntouched(t, sup, rt, state, old)
			if lines := linesOf(t, logs, agedOutLine); len(lines) != 0 {
				t.Fatalf("the keeper's looks wrote %d R3 lines, want none", len(lines))
			}
		})
	}

	t.Run("c: an operator's resume of an operator's suspension with a session 73 hours old keeps that session", func(t *testing.T) {
		old := sessionAged(now, 73*time.Hour)
		_, sup, rt, logs := keptController(t, supervise.Claim{State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByOperator,
			Session: old, SessionRun: supervise.SessionRun{TurnFinished: true}})
		m, _ := sup.Machine(claim.ControllerToken("legion"))
		if err := m.Handle(context.Background(), supervise.RequestResume{Claim: m.Claim().Token}); err != nil {
			t.Fatalf("the operator's resume: %v", err)
		}
		wantResumed(t, rt, logs)
		if c := controllerOf(t, sup); c.Session != old {
			t.Fatalf("the controller records %q after the operator's resume, want %s kept", c.Session, old)
		}
	})

	t.Run("d: a death of a controller with a session 71 hours old resumes it", func(t *testing.T) {
		young := sessionAged(now, 71*time.Hour)
		_, sup, rt, logs := keptController(t, idle(young))
		controllerDies(t, sup, false)
		wantResumed(t, rt, logs)
	})

	t.Run("e: with controller_session_max_age_hours 0 a death of a controller with a session 30 days old resumes it", func(t *testing.T) {
		old := sessionAged(now, 30*24*time.Hour)
		_, sup, rt, logs := keptControllerAged(t, idle(old), 0)
		controllerDies(t, sup, false)
		wantResumed(t, rt, logs)
	})

	t.Run("a death of a controller whose session id is no UUIDv7 resumes it, and one Warn line says the rule cannot read its age", func(t *testing.T) {
		_, sup, rt, logs := keptController(t, idle("ses_controller"))
		controllerDies(t, sup, false)
		wantResumed(t, rt, logs)
		lines := linesOf(t, logs, unagedLine)
		if len(lines) != 1 || lines[0]["level"] != "WARN" || lines[0]["session"] != "ses_controller" {
			t.Fatalf("the lines for an id the rule reads no age off are %+v, want one at Warn", lines)
		}
	})
}

// storedDelivery is the delivery the test store holds for the controller's claim, and whether it
// holds one: what the machine wrote as queued for the controller (Store.PutDelivery).
func storedDelivery(sup *supervisor) (supervise.Delivery, bool) {
	store := sup.deps.Store.(*outboxClaimStore)
	store.mu.Lock()
	defer store.mu.Unlock()
	d, ok := store.deliveries[claim.ControllerToken("legion")]
	return d, ok
}

// The keeper's start message is the daemon's own task and resumes nothing
// (supervise.RequestDeliver.ResumesNothing): a controller an operator suspended at or after its
// ready refuses it, queues nothing and launches nothing, and stays suspended by the operator on its
// session, with one Info line of the keeper's. Two ways reach that claim: an operator's suspension
// between the ready and the message, and one made while the controller was launching, held until
// its ready and ended there, after which the ready route still hands the keeper its turn. The
// control: on main the message reads as an operator's delivery, which resumes the claim whoever
// suspended it, so both rows find the controller launching with the message queued.
func TestTheStartMessageDoesNotUndoAnOperatorsSuspension(t *testing.T) {
	now := time.Now()
	// wantUndisturbed is the controller after the keeper's start message: suspended by the operator
	// on session, holding no delivery in memory or in the store, launched no more than launches
	// times, with the keeper's one Info line.
	wantUndisturbed := func(t *testing.T, sup *supervisor, rt *fake.Runtime, logs *syncBuffer, session string, launches int) {
		t.Helper()
		after := controllerOf(t, sup)
		if after.State != supervise.StateSuspended || after.SuspendedBy != supervise.SuspendedByOperator || after.Session != session || after.Pending != nil {
			t.Errorf("after the start message the controller is %s, suspended by %q, on %q, holding %+v; want it suspended by the operator on %s, holding nothing",
				after.State, after.SuspendedBy, after.Session, after.Pending, session)
		}
		if d, queued := storedDelivery(sup); queued {
			t.Errorf("the store holds the delivery %q for the suspended controller, want none: the start message queues nothing", d.Task)
		}
		if got := len(rt.CallsOf("Resume")) + len(rt.CallsOf("Spawn")); got != launches {
			t.Errorf("the start message launched the controller %d times, want never", got-launches)
		}
		if lines := linesOf(t, logs, startMessageRefusedLine); len(lines) != 1 || lines[0]["level"] != "INFO" {
			t.Errorf("the keeper's lines for a start message the suspension refused are %+v, want one at Info", lines)
		}
	}

	t.Run("an operator's suspension between the controller's ready and its start message", func(t *testing.T) {
		old := sessionAged(now, 73*time.Hour)
		k, sup, rt, logs := keptController(t, supervise.Claim{State: supervise.StateReady, Session: old,
			LastTurnStart: now.Add(-2 * time.Hour), LastTurnEnd: now.Add(-time.Hour), SessionRun: supervise.SessionRun{TurnFinished: true}})
		m, _ := sup.Machine(claim.ControllerToken("legion"))
		// The operator suspends the ready controller before the ready route hands it the start
		// message.
		if err := m.Handle(context.Background(), supervise.RequestSuspend{Claim: m.Claim().Token, Reason: "an operator stops the controller", By: supervise.SuspendedByOperator}); err != nil {
			t.Fatalf("the operator's suspension: %v", err)
		}
		k.ready(m.Claim())
		m.Wait()

		wantUndisturbed(t, sup, rt, logs, old, 0)
	})

	t.Run("an operator's suspension of a launching controller, held until its ready", func(t *testing.T) {
		// A session younger than the age, so the keeper's retry resumes it on main and at this head
		// alike, and the row reads the suspension alone.
		young := sessionAged(now, time.Hour)
		k, sup, rt, logs := keptController(t, supervise.Claim{State: supervise.StateFailed, Session: young})
		// The keeper retries the failed controller after its first wait, which resumes its session.
		start := time.Now()
		k.now = func() time.Time { return start }
		keepOnce(t, k)
		k.now = func() time.Time { return start.Add(61 * time.Second) }
		keepOnce(t, k)
		m, _ := sup.Machine(claim.ControllerToken("legion"))
		token := m.Claim().Token
		if state := m.Claim().State; state != supervise.StateLaunching {
			t.Fatalf("the controller is %s after the keeper's retry, want launching", state)
		}
		// The operator suspends the launching controller: the suspension is held until its ready.
		if err := m.Handle(context.Background(), supervise.RequestSuspend{Claim: token, Reason: "an operator stops the controller", By: supervise.SuspendedByOperator}); !errors.Is(err, supervise.ErrSuspendHeld) {
			t.Fatalf("the operator's suspension of a launching controller answered %v, want it held until ready", err)
		}
		// The resumed agent comes up, and its ready ends in the held suspension.
		sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
		generation := m.Claim().Generation
		handleAll(t, m,
			supervise.StreamHello{Claim: token, Generation: generation},
			supervise.RequestRegister{Claim: token, Generation: generation, Session: young, SessionFile: controllerSessionFile,
				CapabilityHash: []byte("capability"), PluginContract: sup.deps.PluginContract},
			supervise.RequestReady{Claim: token, Generation: generation, Session: young})
		if c := m.Claim(); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByOperator {
			t.Fatalf("after its ready the controller is %s, suspended by %q, want the operator's held suspension in force", c.State, c.SuspendedBy)
		}
		launches := len(rt.CallsOf("Resume")) + len(rt.CallsOf("Spawn"))
		// The ready route still hands the keeper its turn.
		k.ready(m.Claim())
		m.Wait()

		wantUndisturbed(t, sup, rt, logs, young, launches)
	})
}

// dispatch://INFRA-2045 R4 for a replacement under R3: one Warn line names the claim, the old
// session and the reason; one line at the fresh session's ready names the old and the new session;
// and the fresh session's first prompt, the keeper's start message, names the old session and the
// reason and says that messages sent to the old session may be lost, before the start message.
// internal/supervise's test of the same name holds the replacement under R1. The control: on main
// no relaunch replaces a session for its age, so none of the three is written.
func TestAControllerSessionReplacementIsLoggedAndTold(t *testing.T) {
	now := time.Now()
	old := sessionAged(now, 73*time.Hour)
	k, sup, _, logs := keptController(t, supervise.Claim{State: supervise.StateIdle, Session: old,
		LastTurnStart: now.Add(-2 * time.Hour), LastTurnEnd: now.Add(-time.Hour), SessionRun: supervise.SessionRun{TurnFinished: true}})
	controllerDies(t, sup, false)

	replaced := linesOf(t, logs, agedOutLine)
	if len(replaced) != 1 || replaced[0]["level"] != "WARN" {
		t.Fatalf("the replacement lines are %+v, want one at Warn", replaced)
	}
	for key, want := range map[string]string{"claim": string(claim.ControllerToken("legion")), "session": old, "requirement": "R3"} {
		if got := replaced[0][key]; got != want {
			t.Errorf("the replacement line's %s is %v, want %s", key, got, want)
		}
	}
	reason, _ := replaced[0]["reason"].(string)
	if !strings.Contains(reason, "older than controller_session_max_age_hours (72h0m0s)") {
		t.Errorf("the replacement line's reason is %q, want it to name the age bound", reason)
	}

	// The fresh agent comes up: its shim connects, it registers a session of its own and says
	// ready, and the keeper hands it the start message.
	m, _ := sup.Machine(claim.ControllerToken("legion"))
	token := m.Claim().Token
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	fresh := sessionAged(now, 0)
	generation := m.Claim().Generation
	handleAll(t, m,
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: fresh, SessionFile: "/sessions/fresh.jsonl",
			CapabilityHash: []byte("capability"), PluginContract: sup.deps.PluginContract},
		supervise.RequestReady{Claim: token, Generation: generation, Session: fresh})
	ready := linesOf(t, logs, "supervise: the controller's fresh session is ready in place of the session it replaced")
	if len(ready) != 1 || ready[0]["oldSession"] != old || ready[0]["newSession"] != fresh || ready[0]["claim"] != string(token) {
		t.Fatalf("the fresh session's ready lines are %+v, want one naming %s and %s", ready, old, fresh)
	}
	k.ready(m.Claim())
	m.Wait()
	prompts := conn.Prompts()
	if len(prompts) != 1 {
		t.Fatalf("the fresh controller was prompted %d times, want the start message once", len(prompts))
	}
	first := prompts[0].Message
	for _, want := range []string{strconv.Quote(old), "was replaced", "older than controller_session_max_age_hours", "Envoy messages sent to that session may be lost"} {
		if !strings.Contains(first, want) {
			t.Errorf("the fresh session's first prompt does not say %q:\n%s", want, first)
		}
	}
	if !strings.HasSuffix(first, "\n\n"+ControllerStartMessage) {
		t.Errorf("the fresh session's first prompt is %q, want the replacement told before the start message", first)
	}
}

// Row (f) of dispatch://INFRA-2045's c-fresh-after-two-ends, through the keeper: a fresh
// controller session that has never finished a turn ends two launches in a row, is not replaced
// again, and one Error line says so and names the session it replaced; the claim fails at its
// launch budget (two in this supervisor), and the keeper retries it only after its first wait, on
// the same session. internal/supervise's test of the same name holds rows (a) to (g) on the
// machine, and its turn-bound test the same line for the controller's first session. The
// control: on main no Error line is written.
func TestAControllerSessionIsReplacedAfterTwoLaunchesEndBeforeAFinishedTurn(t *testing.T) {
	t.Run("f: a fresh session that never finished a turn is retried with the keeper's waits, not replaced", func(t *testing.T) {
		k, sup, rt, logs := keptController(t, supervise.Claim{State: supervise.StateReady, Session: "ses_fresh",
			Abandoned: supervise.AbandonedSession{Session: "ses_old", Why: "its launches ended before it finished a turn", At: time.Now().Add(-time.Hour), Told: true}})
		m, _ := sup.Machine(claim.ControllerToken("legion"))
		dies := func() { controllerDies(t, sup, false) }
		dies()
		dies()
		if c := m.Claim(); c.State != supervise.StateFailed || c.Session != "ses_fresh" {
			t.Fatalf("the controller is %s on %q after two ends, want failed on ses_fresh kept", c.State, c.Session)
		}
		lines := linesOf(t, logs, "supervise: the controller's session ended two launches in a row and has never finished a turn, so it is not replaced, and the keeper retries it with its waits")
		if len(lines) != 1 || lines[0]["level"] != "ERROR" {
			t.Fatalf("the lines that say the session is not replaced are %+v, want one at Error", lines)
		}
		if lines[0]["session"] != "ses_fresh" || lines[0]["replacement"] != true || lines[0]["replaced"] != "ses_old" {
			t.Errorf("the Error line names session %v, replacement %v, replaced %v; want ses_fresh, true, ses_old", lines[0]["session"], lines[0]["replacement"], lines[0]["replaced"])
		}
		resumes := len(rt.CallsOf("Resume"))

		start := time.Now()
		k.now = func() time.Time { return start }
		keepOnce(t, k)
		k.now = func() time.Time { return start.Add(30 * time.Second) }
		keepOnce(t, k)
		if got := len(rt.CallsOf("Resume")); got != resumes {
			t.Fatalf("the keeper relaunched the failed controller %d times inside its first wait, want none", got-resumes)
		}
		k.now = func() time.Time { return start.Add(61 * time.Second) }
		keepOnce(t, k)

		got := rt.CallsOf("Resume")
		if len(got) != resumes+1 || len(rt.CallsOf("Spawn")) != 0 || got[len(got)-1].Spec.ResumeSessionFile != controllerSessionFile {
			t.Fatalf("after its wait the keeper started %d resumes and %d spawns, want one resume of the same session", len(got)-resumes, len(rt.CallsOf("Spawn")))
		}
		if lines := linesOf(t, logs, "controller: retrying the daemon's controller"); len(lines) != 1 {
			t.Fatalf("the keeper's retry lines are %+v, want one, after its first wait", lines)
		}
	})
}

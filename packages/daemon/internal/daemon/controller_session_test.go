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
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// controllerSessionFile is the transcript every kept controller of these tests records.
const controllerSessionFile = "/sessions/controller.jsonl"

// sessionAged is an Oh My Pi session id (a UUIDv7) whose recorded start is age before now.
func sessionAged(now time.Time, age time.Duration) string {
	ms := now.Add(-age).UnixMilli()
	return fmt.Sprintf("%08x-%04x-7abc-8def-0123456789ab", ms>>16, ms&0xffff)
}

// keptController is the daemon's own controller claim c, stored with a process the fake runtime
// runs and a shim connected to this daemon whose agent is not streaming, and its keeper with the
// shipped bounds (90 minutes, 72 hours) and the real clock the supervisor's machines read
// (stillClock). The keeper's and the machines' lines are both written to logs, as JSON.
func keptController(t *testing.T, c supervise.Claim) (*controllerKeeper, *supervisor, *fake.Runtime, *syncBuffer) {
	t.Helper()
	return keptControllerWith(t, c, nil)
}

// keptControllerWith is keptController with before run on the supervisor before the claim is
// created: a row that needs another store sets it there.
func keptControllerWith(t *testing.T, c supervise.Claim, before func(*supervisor)) (*controllerKeeper, *supervisor, *fake.Runtime, *syncBuffer) {
	t.Helper()
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	sup.deps.Log = log
	if before != nil {
		before(sup)
	}
	token := claim.ControllerToken("legion")
	c.Token, c.Project, c.Role, c.Generation = token, "legion", claim.RoleController, 1
	c.SessionFile = controllerSessionFile
	c.Locator = &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "31999:999999",
		Tmux: &runtime.TmuxLocator{Window: "@99", Pane: "%99"}}
	sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	if _, _, err := sup.Create(context.Background(), c, ""); err != nil {
		t.Fatalf("create the controller's claim: %v", err)
	}
	k := newControllerKeeper(context.Background(), sup, "legion", time.Minute, 90*time.Minute, 72*time.Hour, log)
	return k, sup, rt, logs
}

// shimOf is the fake connection the controller's shim holds to this daemon, and whether it holds
// one.
func shimOf(sup *supervisor) (*fake.Conn, bool) {
	conn, ok := sup.deps.Conns.(*fake.Conns).Conn(claim.ControllerToken("legion"))
	if !ok {
		return nil, false
	}
	return conn.(*fake.Conn), true
}

// forgetRefusingStore is the test supervisor's store with the write of a forgotten session refused
// while refuseForgets is set: a claim write that records no session is the forget's
// (supervise's forgetSession), and the store answers it with an error, as a database that went
// away between two writes would.
type forgetRefusingStore struct {
	*outboxClaimStore
	refuseForgets bool
}

func (s *forgetRefusingStore) PutClaim(ctx context.Context, c supervise.Claim) error {
	if s.refuseForgets && c.Session == "" {
		return errors.New("the store did not answer the forget's write")
	}
	return s.outboxClaimStore.PutClaim(ctx, c)
}

func (s *forgetRefusingStore) ReleaseHolds(ctx context.Context, c supervise.Claim, holds []supervise.Hold) ([]supervise.Hold, error) {
	if s.refuseForgets && c.Session == "" {
		return nil, errors.New("the store did not answer the forget's write")
	}
	return s.outboxClaimStore.ReleaseHolds(ctx, c, holds)
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

// dispatch://INFRA-2045 R3: at each look of the keeper, a controller that is idle, with no turn
// open and no task waiting, whose session id (a UUIDv7) is older than
// controller_session_max_age_hours, gets a fresh session; one in a turn, one with a task waiting
// and one younger than the age keep theirs, and 0 turns the rule off. internal/config's test of the
// same name holds the default of 72. The control: on main no code reads the age of the
// controller's session, and the keeper does nothing for an idle claim, so rows (a) and (b) fail.
func TestAnIdleControllerSessionOlderThanItsMaxAgeIsReplaced(t *testing.T) {
	now := time.Now()
	idle := func(session string) supervise.Claim {
		return supervise.Claim{State: supervise.StateIdle, Session: session, LastTurnStart: now.Add(-2 * time.Hour),
			LastTurnEnd: now.Add(-time.Hour), SessionRun: supervise.SessionRun{TurnFinished: true}}
	}
	wantFresh := func(t *testing.T, sup *supervisor, rt *fake.Runtime, old string) {
		t.Helper()
		if suspends := rt.CallsOf("Suspend"); len(suspends) != 1 {
			t.Fatalf("the runtime was asked %d times to suspend the controller, want once", len(suspends))
		}
		if resumes, spawns := rt.CallsOf("Resume"), rt.CallsOf("Spawn"); len(resumes) != 0 || len(spawns) != 1 || spawns[0].Spec.ResumeSessionFile != "" {
			t.Fatalf("the replacement started %d resumes and %d spawns (%+v), want one fresh spawn", len(resumes), len(spawns), spawns)
		}
		c := controllerOf(t, sup)
		if c.Session != "" || c.Abandoned.Session != old || !strings.Contains(c.Abandoned.Why, "older than controller_session_max_age_hours") {
			t.Fatalf("the controller records %q and abandoned %+v, want %s left behind for its age", c.Session, c.Abandoned, old)
		}
		if c.State != supervise.StateLaunching {
			t.Fatalf("the controller is %s after its replacement, want launching its fresh session", c.State)
		}
	}

	t.Run("a: an idle session 73 hours old is replaced at the keeper's next look", func(t *testing.T) {
		old := sessionAged(now, 73*time.Hour)
		k, sup, rt, _ := keptController(t, idle(old))
		keepOnce(t, k)
		wantFresh(t, sup, rt, old)
	})

	t.Run("b: a session 73 hours old in a turn is kept until the turn ends", func(t *testing.T) {
		old := sessionAged(now, 73*time.Hour)
		c := idle(old)
		c.State, c.LastTurnStart = supervise.StateWorking, now.Add(-time.Minute)
		k, sup, rt, _ := keptController(t, c)
		keepOnce(t, k)
		wantUntouched(t, sup, rt, supervise.StateWorking, old)

		m, _ := sup.Machine(claim.ControllerToken("legion"))
		if err := m.Handle(context.Background(), supervise.StreamTurnEnd{Claim: m.Claim().Token}); err != nil {
			t.Fatalf("end the turn: %v", err)
		}
		if state := controllerOf(t, sup).State; state != supervise.StateIdle {
			t.Fatalf("the controller is %s after its turn ended, want idle", state)
		}
		keepOnce(t, k)
		wantFresh(t, sup, rt, old)
	})

	t.Run("c: an idle session with a task waiting is kept", func(t *testing.T) {
		old := sessionAged(now, 73*time.Hour)
		c := idle(old)
		c.Pending = &supervise.Delivery{ID: "dlv-start", Task: ControllerStartMessage, QueuedAt: now.Add(-time.Minute)}
		k, sup, rt, _ := keptController(t, c)
		keepOnce(t, k)
		wantUntouched(t, sup, rt, supervise.StateIdle, old)
	})

	t.Run("d: an idle session 71 hours old is kept", func(t *testing.T) {
		young := sessionAged(now, 71*time.Hour)
		k, sup, rt, _ := keptController(t, idle(young))
		keepOnce(t, k)
		wantUntouched(t, sup, rt, supervise.StateIdle, young)
	})

	t.Run("e: with controller_session_max_age_hours 0 a session 30 days old is kept", func(t *testing.T) {
		old := sessionAged(now, 30*24*time.Hour)
		k, sup, rt, _ := keptController(t, idle(old))
		k.maxAge = 0
		keepOnce(t, k)
		wantUntouched(t, sup, rt, supervise.StateIdle, old)
	})

	t.Run("an idle session whose id is no UUIDv7 is kept, and one Warn line says the rule cannot read its age", func(t *testing.T) {
		k, sup, rt, logs := keptController(t, idle("ses_controller"))
		keepOnce(t, k)
		keepOnce(t, k)
		wantUntouched(t, sup, rt, supervise.StateIdle, "ses_controller")
		lines := linesOf(t, logs, "controller: the controller's session id is no UUIDv7, so controller_session_max_age_hours cannot read its age and the session is not replaced for it")
		if len(lines) != 1 || lines[0]["level"] != "WARN" {
			t.Fatalf("the lines for an id the rule reads no age off are %+v, want one at Warn over two looks", lines)
		}
	})

	t.Run("after a daemon restart, an old session is kept before its shim's hello and while its agent streams", func(t *testing.T) {
		old := sessionAged(now, 73*time.Hour)
		// The claim is restored from the store idle, and the keeper's first look comes before the
		// controller's shim has said hello to this daemon.
		k, sup, rt, logs := keptController(t, idle(old))
		conn, _ := shimOf(sup)
		sup.deps.Conns.(*fake.Conns).Remove(claim.ControllerToken("legion"))
		keepOnce(t, k)
		wantUntouched(t, sup, rt, supervise.StateIdle, old)

		// The shim says hello while its agent is in a turn an Envoy message began while the daemon
		// was down, and whose agent_start this daemon has not read yet.
		conn.SetStreaming(true)
		sup.deps.Conns.(*fake.Conns).Register(claim.ControllerToken("legion"), conn)
		keepOnce(t, k)
		wantUntouched(t, sup, rt, supervise.StateIdle, old)
		if lines := linesOf(t, logs, controllerSessionReplacedLine); len(lines) != 0 {
			t.Fatalf("the keeper wrote %d replacement lines for a controller it kept, want none", len(lines))
		}

		// The turn ends, and the next look finds the controller between turns.
		conn.SetStreaming(false)
		keepOnce(t, k)
		wantFresh(t, sup, rt, old)
	})

	t.Run("the replacement's Warn line is written once when the forget's write fails, and the next look launches the fresh session", func(t *testing.T) {
		old := sessionAged(now, 73*time.Hour)
		var store *forgetRefusingStore
		k, sup, rt, logs := keptControllerWith(t, idle(old), func(sup *supervisor) {
			store = &forgetRefusingStore{outboxClaimStore: sup.deps.Store.(*outboxClaimStore)}
			sup.deps.Store = store
		})
		store.refuseForgets = true
		if err := k.keep(); err == nil {
			t.Fatal("the look whose forget the store would not write answered nil, want the write's error")
		}
		if lines := linesOf(t, logs, controllerSessionReplacedLine); len(lines) != 1 || lines[0]["session"] != old {
			t.Fatalf("the replacement lines after the failed forget are %+v, want one naming %s", lines, old)
		}

		store.refuseForgets = false
		keepOnce(t, k)
		wantFresh(t, sup, rt, old)
		if lines := linesOf(t, logs, controllerSessionReplacedLine); len(lines) != 1 {
			t.Fatalf("the replacement lines after the fresh launch are %d, want the one", len(lines))
		}
	})

	t.Run("an idle controller holding an operator's suspension whose stop the runtime refused keeps its session for the operator's resume", func(t *testing.T) {
		old := sessionAged(now, 73*time.Hour)
		k, sup, rt, logs := keptController(t, idle(old))
		m, _ := sup.Machine(claim.ControllerToken("legion"))
		token := m.Claim().Token
		// An operator suspends the controller during a turn; the suspension is held for the turn's
		// end, where the runtime refuses the stop, so the claim stands idle with the operator's
		// suspension still held.
		if err := m.Handle(context.Background(), supervise.StreamTurnStart{Claim: token}); err != nil {
			t.Fatalf("start a turn: %v", err)
		}
		if err := m.Handle(context.Background(), supervise.RequestSuspend{Claim: token, Reason: "an operator drains the daemon", By: supervise.SuspendedByOperator}); !errors.Is(err, supervise.ErrSuspendHeld) {
			t.Fatalf("the operator's suspension in a turn answered %v, want it held", err)
		}
		rt.FailSuspend(errors.New("the runtime could not stop the process"))
		_ = m.Handle(context.Background(), supervise.StreamTurnEnd{Claim: token})
		if state := m.Claim().State; state != supervise.StateIdle {
			t.Fatalf("the controller is %s after its turn ended with the stop refused, want idle", state)
		}
		rt.FailSuspend(nil)

		keepOnce(t, k)

		c := controllerOf(t, sup)
		if c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByOperator || c.Session != old || c.Abandoned.Session != "" {
			t.Fatalf("the controller is %s, suspended by %q, on %q with %q abandoned; want it suspended by the operator on %s, nothing abandoned",
				c.State, c.SuspendedBy, c.Session, c.Abandoned.Session, old)
		}
		if lines := linesOf(t, logs, controllerSessionReplacedLine); len(lines) != 0 {
			t.Fatalf("the keeper wrote %d replacement lines for a session it kept, want none", len(lines))
		}
		if spawns := rt.CallsOf("Spawn"); len(spawns) != 0 {
			t.Fatalf("the runtime was asked %d times to spawn, want never", len(spawns))
		}
	})

	t.Run("a controller whose session another claim holds is not stopped for its age, and the Envoy listener is not asked", func(t *testing.T) {
		old := sessionAged(now, 73*time.Hour)
		// Another claim recorded the session first, so the controller records a session it does
		// not hold: its forget would wait on the listener (mayForget), and no launch could resume
		// it, so the keeper must not stop it.
		asked := 0
		k, sup, rt, logs := keptControllerWith(t, idle(old), func(sup *supervisor) {
			if _, err := sup.deps.Store.(*outboxClaimStore).HoldSessions(context.Background(), "legion",
				mustClaimToken(t, "LEGION-943", claim.RoleImplementer), supervise.RecordedHolds(old, "")); err != nil {
				t.Fatalf("hold %s for the claim that recorded it first: %v", old, err)
			}
			sup.deps.RoleHolder = func(context.Context, string) (string, string, error) {
				asked++
				return "", "", fmt.Errorf("look up the holder: %w", &notify.RoleLookupRefused{Status: 401, Reason: `listener returned 401 Unauthorized: {"error":"unauthorized"}`})
			}
		})
		m, _ := sup.Machine(claim.ControllerToken("legion"))
		if err := m.Handle(context.Background(), supervise.RequestSuspend{Claim: m.Claim().Token, Reason: "the session is old",
			By: supervise.SuspendedByDaemon, NeverHold: true, BetweenTurns: true}); !supervise.IsSessionHeldElsewhere(err) {
			t.Fatalf("the suspension between turns of a claim another holds the session of answered %v, want the hold's refusal", err)
		}
		keepOnce(t, k)
		wantUntouched(t, sup, rt, supervise.StateIdle, old)
		if asked != 0 {
			t.Fatalf("the Envoy listener was asked %d times who holds the controller's role, want never", asked)
		}
		lines := linesOf(t, logs, "controller: the old controller session is not replaced at this look: the claim is not known to be between turns")
		if len(lines) != 1 {
			t.Fatalf("the keeper's lines for a session it does not hold are %+v, want one", lines)
		}
		if lines := linesOf(t, logs, controllerSessionReplacedLine); len(lines) != 0 {
			t.Fatalf("the keeper wrote %d replacement lines for a session it kept, want none", len(lines))
		}
	})

	t.Run("an operator's forget-session between the keeper's suspension and its forget is the operator's replacement: Info, no R4 line, no error", func(t *testing.T) {
		old := sessionAged(now, 73*time.Hour)
		k, sup, rt, logs := keptController(t, idle(old))
		m, _ := sup.Machine(claim.ControllerToken("legion"))
		token := m.Claim().Token
		why := "the session is older than controller_session_max_age_hours"
		if err := m.Handle(context.Background(), supervise.RequestSuspend{Claim: token, Reason: why,
			By: supervise.SuspendedByDaemon, NeverHold: true, BetweenTurns: true}); err != nil {
			t.Fatalf("the keeper's suspension between turns: %v", err)
		}
		// The operator clears the session before the keeper's forget runs.
		if err := m.Handle(context.Background(), supervise.RequestForgetSession{Claim: token, Session: old}); err != nil {
			t.Fatalf("the operator's forget-session: %v", err)
		}
		if err := k.forgetOld(context.Background(), m, token, old, why, now.Add(-73*time.Hour)); err != nil {
			t.Fatalf("the keeper's forget after the operator's answered %v, want nil: the operator replaced the session", err)
		}
		if lines := linesOf(t, logs, controllerSessionReplacedLine); len(lines) != 0 {
			t.Fatalf("the keeper wrote %d R4 lines for a replacement the operator made, want none", len(lines))
		}
		if lines := linesOf(t, logs, "controller: the old controller session is not replaced by the keeper: another decision cleared it after the keeper's suspension, an operator's forget-session among them"); len(lines) != 1 || lines[0]["level"] != "INFO" {
			t.Fatalf("the keeper's lines for the operator's replacement are %+v, want one at Info", lines)
		}
		if c := controllerOf(t, sup); !strings.Contains(c.Abandoned.Why, "legion claims forget-session") {
			t.Fatalf("the abandoned session is recorded for %q, want the operator's reason", c.Abandoned.Why)
		}
		// The next look's resume by rule launches the fresh session the operator asked for.
		keepOnce(t, k)
		if spawns := rt.CallsOf("Spawn"); len(spawns) != 1 || spawns[0].Spec.ResumeSessionFile != "" {
			t.Fatalf("the next look spawned %+v, want one fresh agent", spawns)
		}
	})
}

// dispatch://INFRA-2045 R4 for a replacement under R3: one Warn line of the keeper's names the
// claim, the old session and the reason; one line at the fresh session's ready names the old and
// the new session; and the fresh session's first prompt, the keeper's start message, names the old
// session and the reason and says that messages sent to the old session may be lost, before the
// start message. internal/supervise's test of the same name holds the replacement under R1. The
// control: on main the keeper replaces no session for its age, so none of the three is written.
func TestAControllerSessionReplacementIsLoggedAndTold(t *testing.T) {
	now := time.Now()
	old := sessionAged(now, 73*time.Hour)
	k, sup, _, logs := keptController(t, supervise.Claim{State: supervise.StateIdle, Session: old,
		LastTurnStart: now.Add(-2 * time.Hour), LastTurnEnd: now.Add(-time.Hour), SessionRun: supervise.SessionRun{TurnFinished: true}})
	keepOnce(t, k)

	replaced := linesOf(t, logs, controllerSessionReplacedLine)
	if len(replaced) != 1 || replaced[0]["level"] != "WARN" {
		t.Fatalf("the keeper's replacement lines are %+v, want one at Warn", replaced)
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
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: m.Claim().Generation},
		supervise.RequestRegister{Claim: token, Generation: m.Claim().Generation, Session: fresh, SessionFile: "/sessions/fresh.jsonl",
			CapabilityHash: []byte("capability"), PluginContract: sup.deps.PluginContract},
		supervise.RequestReady{Claim: token, Generation: m.Claim().Generation, Session: fresh},
	} {
		if err := m.Handle(context.Background(), ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
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
// again, and one Error line says that a fresh session also failed; the claim fails at its launch
// budget (two in this supervisor), and the keeper retries it only after its first wait, on the
// same session. internal/supervise's test of the same name holds rows (a) to (g) on the machine.
// The control: on main no Error line is written.
func TestAControllerSessionIsReplacedAfterTwoLaunchesEndBeforeAFinishedTurn(t *testing.T) {
	t.Run("f: a fresh session that never finished a turn is retried with the keeper's waits, not replaced", func(t *testing.T) {
		k, sup, rt, logs := keptController(t, supervise.Claim{State: supervise.StateReady, Session: "ses_fresh"})
		m, _ := sup.Machine(claim.ControllerToken("legion"))
		dies := func() {
			t.Helper()
			c := m.Claim()
			if err := m.Handle(context.Background(), supervise.RuntimeObservation{Observation: runtime.Observation{
				Locator: *c.Locator, Kind: runtime.Gone, At: time.Now()}}); err != nil {
				t.Fatalf("report the controller's process gone: %v", err)
			}
		}
		dies()
		dies()
		if c := m.Claim(); c.State != supervise.StateFailed || c.Session != "ses_fresh" {
			t.Fatalf("the controller is %s on %q after two ends, want failed on ses_fresh kept", c.State, c.Session)
		}
		if lines := linesOf(t, logs, "supervise: the controller's session ended two launches in a row and has never finished a turn, so it is not replaced: a fresh session also failed, and the keeper retries it with its waits"); len(lines) != 1 || lines[0]["level"] != "ERROR" {
			t.Fatalf("the lines that say a fresh session also failed are %+v, want one at Error", lines)
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

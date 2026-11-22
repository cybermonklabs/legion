package workflow

// The wait's time (dispatch://INFRA-2040 R3's first end, D5 of its plan): the declaration queues
// one wait_due row due at the wait's time, and the row's fact (intake.WaitDue, waitDue) ends the
// wait it names and writes the wait's own task, or ends nothing for a wait that is over.

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// waitTimeLine is the line the time of a wait that stood writes once its fact commits.
const waitTimeLine = `msg="wait: its time came"`

// waitTimeLines is every `wait: its time came` line in logs.
func waitTimeLines(logs *testlog.Buffer) []string {
	var found []string
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, waitTimeLine) {
			found = append(found, line)
		}
	}
	return found
}

// waitDueRows is every wait_due row of issue, oldest first, each with when it is due and what it
// names.
func waitDueRows(t *testing.T, pool *pgxpool.Pool, issue string) []struct {
	nextAt time.Time
	due    record.WaitDue
} {
	t.Helper()
	var rows []struct {
		nextAt time.Time
		due    record.WaitDue
	}
	for _, row := range issueOutboxOf(t, pool, issue, string(record.OutboxKindWaitDue)) {
		var due record.WaitDue
		if err := json.Unmarshal(row.payload, &due); err != nil {
			t.Fatalf("decode wait_due row %d: %v", row.id, err)
		}
		rows = append(rows, struct {
			nextAt time.Time
			due    record.WaitDue
		}{nextAt: row.nextAt, due: due})
	}
	return rows
}

// redeclare is a second declaration by the claim that declared wait, under id, as the API applies
// it: ten minutes after the first, its time three hours after the first's declaration. It answers
// the wait recorded.
func (r *waitRig) redeclare(wait record.ClaimWait, id string) record.ClaimWait {
	r.t.Helper()
	if result := r.apply("api", "wait-declared:"+id, intake.WaitDeclared{Token: wait.Token, Issue: wait.Issue, Role: wait.Role,
		Session: wait.Session, Generation: wait.Generation, WaitID: id, WaitingOn: "the second run on the branch's head",
		WakeAt: waitNow.Add(3 * time.Hour), TakenEntry: wait.TakenEntry, At: waitNow.Add(10 * time.Minute)}); result.Refusal != nil {
		r.t.Fatalf("the declaration of %s was refused: %+v", id, *result.Refusal)
	}
	return r.wait(id)
}

// due applies the wait_due row of wait as the outbox does (internal/daemon's waitDue): the fact
// under `wait-due:<id>`, from the outbox.
func (r *waitRig) due(wait record.ClaimWait) intake.Result {
	r.t.Helper()
	return r.apply("outbox", "wait-due:"+wait.ID, intake.WaitDue{Token: wait.Token, Issue: wait.Issue, WaitID: wait.ID})
}

// The declaration queues the wait's time in its own transaction: one wait_due row of the wait's
// issue, due at the wait's time, naming the claim and the wait. When the row's fact runs and the
// wait still stands, the wait ends with the cause that its time came, ended by that fact at the
// engine's now, and the wait's own task is written in the same transaction: a start of the
// waiting role on the issue's run, with no phase and no entry, that names the wait and whose task
// gives the wait's id and text and says that its time came. A claim no release took is given that
// start, not a wake (the rig's engine reads no pods at all). The line `wait: its time came` names
// the claim, the issue, the role, the wait and the row of the task.
//
// The fact applied again is a duplicate and writes nothing; the same row's fact under another
// event finds the wait over and writes nothing either.
func TestAtItsTimeAStandingWaitEndsAndItsOwnTaskIsWritten(t *testing.T) {
	r := newWaitRig(t, config.DesignGateRootIssues.Steps())
	r.tree(phase.Testing)
	wait := r.waiting(waitRoot, claim.RoleTester)

	rows := waitDueRows(t, r.pool, waitRoot)
	if len(rows) != 1 || !rows[0].nextAt.Equal(wait.WakeAt) || rows[0].due != (record.WaitDue{Token: wait.Token, WaitID: wait.ID}) {
		t.Fatalf("the declaration queued %+v, want one wait_due row of %s due at its time %s", rows, wait.ID, wait.WakeAt)
	}
	if starts := superviseRowsOf(t, r.pool, waitRoot, "start"); len(starts) != 0 {
		t.Fatalf("the declaration queued the starts %+v, want none before the wait's time", starts)
	}

	r.due(wait)

	r.assertEnded(wait, WaitEndTimeCame, "outbox/wait-due:"+wait.ID)
	starts := issueOutboxOf(t, r.pool, waitRoot, string(record.OutboxKindSupervise))
	if len(starts) != 1 {
		t.Fatalf("the wait's time wrote %d supervise rows, want its one start", len(starts))
	}
	start := decodeSupervise(t, starts[0])
	if start.Op != "start" || start.Role != claim.RoleTester || start.Tree != waitRoot || start.Generation != 1 ||
		start.Phase != "" || start.Entry != 0 || start.ResumeTask || start.WaitID != wait.ID {
		t.Fatalf("the wait's own start = %+v, want a start of the tester on run 1 with no phase and no entry, naming %s", start, wait.ID)
	}
	for _, words := range []string{wait.ID, wait.WaitingOn, "time of your wait", "came", "declare a new wait"} {
		if !strings.Contains(start.Task, words) {
			t.Fatalf("the wait's own task = %q, want it to hold %q", start.Task, words)
		}
	}
	lines := waitTimeLines(r.logs)
	if len(lines) != 1 {
		t.Fatalf("the time was logged %d times, want once; the log is:\n%s", len(lines), r.logs)
	}
	for _, field := range [][2]string{{"claim", string(wait.Token)}, {"issue", waitRoot}, {"role", string(claim.RoleTester)},
		{"wait", wait.ID}, {"row", strconv.FormatInt(starts[0].id, 10)}, {"wake", "false"}} {
		if want := loggedField(field[0], field[1]); !carries(lines[0], want) {
			t.Fatalf("the line %q does not carry %s", lines[0], want)
		}
	}

	if result := r.due(wait); !result.Duplicate {
		t.Fatalf("the wait's time applied again = %+v, want a duplicate", result)
	}
	r.apply("outbox", "wait-due-again:"+wait.ID, intake.WaitDue{Token: wait.Token, Issue: wait.Issue, WaitID: wait.ID})
	if again := issueOutboxOf(t, r.pool, waitRoot, string(record.OutboxKindSupervise)); len(again) != 1 {
		t.Fatalf("the time of a wait already over wrote %d supervise rows, want only the first start", len(again))
	}
	if lines := waitTimeLines(r.logs); len(lines) != 1 {
		t.Fatalf("the time was logged %d times after the wait was over, want once", len(lines))
	}
}

// The time of a wait that is over sends nothing (R3: "the time of a replaced wait sends nothing"):
// a wait a newer declaration of the same claim replaced, and a wait that news ended first. Its row
// comes due all the same, and its fact writes no task, leaves the wait's end as it was and the
// newer wait standing, and logs that the wait is over, naming the wait that stands.
func TestTheTimeOfAWaitThatIsOverSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// over ends wait, and answers the wait that stands afterwards, the zero wait for none.
		over func(r *waitRig, wait record.ClaimWait) record.ClaimWait
		// cause and endedBy are the end over wrote.
		cause, endedBy func(wait, standing record.ClaimWait) string
	}{
		{
			name: "a wait a newer declaration replaced",
			over: func(r *waitRig, wait record.ClaimWait) record.ClaimWait {
				return r.redeclare(wait, wait.ID+"-again")
			},
			cause:   func(_, _ record.ClaimWait) string { return record.WaitReplaced },
			endedBy: func(_, standing record.ClaimWait) string { return standing.ID },
		},
		{
			name: "a wait news ended",
			over: func(r *waitRig, wait record.ClaimWait) record.ClaimWait {
				if result := r.apply("api", "wait-news:"+wait.ID, intake.WaitNews{Token: wait.Token, Issue: wait.Issue,
					Role: wait.Role, WaitID: wait.ID}); !result.WaitEnded {
					r.t.Fatalf("the news report ended nothing: %+v", result)
				}
				return record.ClaimWait{}
			},
			cause:   func(_, _ record.ClaimWait) string { return WaitEndNews },
			endedBy: func(wait, _ record.ClaimWait) string { return "api/wait-news:" + wait.ID },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWaitRig(t, config.DesignGateRootIssues.Steps())
			r.tree(phase.Testing)
			wait := r.waiting(waitRoot, claim.RoleTester)
			standing := tc.over(r, wait)
			ended := r.wait(wait.ID)

			r.due(wait)

			if starts := issueOutboxOf(t, r.pool, waitRoot, string(record.OutboxKindSupervise)); len(starts) != 0 {
				t.Fatalf("the time of a wait that is over wrote %d supervise rows, want none", len(starts))
			}
			if got := r.wait(wait.ID); got.EndCause != tc.cause(wait, standing) || got.EndedBy != tc.endedBy(wait, standing) ||
				!got.EndedAt.Equal(ended.EndedAt) {
				t.Fatalf("wait %s after its time ended with cause %q by %q at %s, want the end it had: %q by %q at %s", wait.ID,
					got.EndCause, got.EndedBy, got.EndedAt, tc.cause(wait, standing), tc.endedBy(wait, standing), ended.EndedAt)
			}
			if lines := waitTimeLines(r.logs); len(lines) != 0 {
				t.Fatalf("the time of a wait that is over was logged as a time that came: %q", lines)
			}
			var over []string
			for line := range strings.SplitSeq(r.logs.String(), "\n") {
				if strings.Contains(line, "wait: its time came to a wait that is over; nothing is sent") &&
					carries(line, loggedField("wait", wait.ID)) && carries(line, loggedField("standing", standing.ID)) {
					over = append(over, line)
				}
			}
			if len(over) != 1 {
				t.Fatalf("the time of a wait that is over was logged %d times naming the wait that stands, want once; the log is:\n%s", len(over), r.logs)
			}
			if standing.ID != "" {
				r.assertStanding(r.engine, standing)
			}
		})
	}
}

// The time of a wait whose claim an idle release took wakes the pod (pods.WakeWaitTime, through
// wakeReleased): one resume of the woken role is recorded under that kind, and one start row of
// the role carries the wake's task, which says the pod was released and gives the wait's own task.
// The wait ends with the cause that its time came, ended by the time's fact at the engine's now;
// the wake itself ends nothing more (wakeEndsWait), so the wait's one end is that one. The line
// `wait: its time came` says the task is a wake.
func TestTheTimeOfAWaitWakesItsReleasedClaim(t *testing.T) {
	rig := newWakeRig(t)
	wait := rig.declareWait(t)
	waits := rig.waits(t)

	rig.apply("wait-due:"+wait.ID, intake.WaitDue{Token: wait.Token, Issue: wait.Issue, WaitID: wait.ID})

	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeWaitTime, "released", wait.ID, wait.WaitingOn, "time of your wait")
	ended := waits.wait(wait.ID)
	if ended.EndCause != WaitEndTimeCame || ended.EndedBy != "api/wait-due:"+wait.ID || !ended.EndedAt.Equal(wakeAt) {
		t.Fatalf("wait %s ended with cause %q by %q at %s, want cause %q by the time's fact at %s",
			wait.ID, ended.EndCause, ended.EndedBy, ended.EndedAt, WaitEndTimeCame, wakeAt)
	}
	waits.endedLine(wait.ID)
	lines := waitTimeLines(rig.logged)
	if len(lines) != 1 || !carries(lines[0], loggedField("wake", "true")) || !carries(lines[0], loggedField("wait", wait.ID)) {
		t.Fatalf("the time of a released claim's wait was logged %q, want one line naming the wait and a wake", lines)
	}
	if waits.stands(rig.engine, wait) {
		t.Fatalf("wait %s still stands after its time", wait.ID)
	}
}

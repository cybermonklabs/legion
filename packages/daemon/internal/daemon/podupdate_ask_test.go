package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The round every case below asks for, the pod it was opened over, and the text the sweep built.
const (
	askedRound = "pod:9d3f0c7a"
	askedPod   = "9d3f0c7a"
	askedText  = "The daemon must replace the pod that runs this issue's roles. The reasons are: image a, now b."
)

// The three lines the executor writes, one for each way a row ends without an error. The first
// is the line the production check reads (requirement R8).
const (
	askPublishedLine = "outbox update ask published to the role topic"
	askNoHolderLine  = "outbox update ask finished undelivered: no live session holds the role"
	askLeftLine      = "outbox update ask finished without a publish: the claim no longer records its round"
)

// askRunner is an outbox runner as phase_ended_test.go's unit case builds one — a project, a
// publisher and a log — with the claim supervisor the executor reads the round from. Each role in
// rounds gets a claim on LEGION-208 in the round given, "" for no round; a role not in rounds has
// no claim this daemon supervises.
func askRunner(t *testing.T, publisher notify.Publisher, rounds map[claim.Role]string) (*outbox, *syncBuffer) {
	t.Helper()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	for role, round := range rounds {
		if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: mustClaimToken(t, "LEGION-208", role), Project: "legion",
			Tree: "LEGION-208", Issue: "LEGION-208", Role: role, State: supervise.StateQueued, UpdateRound: round}, ""); err != nil {
			t.Fatalf("create the %s's claim: %v", role, err)
		}
	}
	logs := &syncBuffer{}
	return &outbox{project: "legion", notices: publisher, supervisor: sup, log: jsonLogger(logs)}, logs
}

// askRow is the ask of askedRound to role, as row id.
func askRow(t *testing.T, id int64, role claim.Role) record.OutboxRow {
	t.Helper()
	row := mustOutboxRow(t, "LEGION-208", record.UpdateAsk{Role: role, Round: askedRound, Pod: askedPod, Text: askedText}, time.Now())
	row.ID = id
	return row
}

// The ask reaches the role it names on that role's own topic, a worker and the architect alike,
// since the architect's process runs in the pod too. The message is the sweep's text unchanged,
// under the row's own key, so a retried row is one delivery. When the publisher has taken it, the
// executor writes the one line the production check greps for, with the row, the issue, the round,
// the claim, the role and the topic.
func TestPodUpdateAskReachesEachRoleOnItsOwnTopic(t *testing.T) {
	for i, role := range []claim.Role{claim.RoleTester, claim.RoleArchitect} {
		t.Run(string(role), func(t *testing.T) {
			publisher := &outboxPublisher{}
			runner, logs := askRunner(t, publisher, map[claim.Role]string{role: askedRound})
			row := askRow(t, int64(71+i), role)
			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("run the ask to the %s: %v", role, err)
			}
			token := mustClaimToken(t, "LEGION-208", role)
			topic := notify.RoleTopicPrefix + string(token)
			published := publisher.publishes()
			if len(published) != 1 {
				t.Fatalf("publishes = %#v, want the one ask", published)
			}
			if got := published[0]; got.topic != topic || got.message != askedText || got.payload != any(askedText) || got.key != record.OutboxKey(row.ID) {
				t.Fatalf("the ask went as %#v, want %q on %s under %s", got, askedText, topic, record.OutboxKey(row.ID))
			}
			lines := loggedLines(t, logs.String(), `"msg":"`+askPublishedLine+`"`)
			if len(lines) != 1 {
				t.Fatalf("lines %q = %d in\n%s\nwant one", askPublishedLine, len(lines), logs.String())
			}
			want := map[string]any{"row": float64(row.ID), "issue": "LEGION-208", "round": askedRound, "claim": string(token), "role": string(role), "topic": topic}
			for field, value := range want {
				if lines[0][field] != value {
					t.Errorf("the line's %s = %v, want %v", field, lines[0][field], value)
				}
			}
			if lines[0]["level"] != "INFO" {
				t.Errorf("the line's level = %v, want INFO", lines[0]["level"])
			}
			t.Logf("the %s was asked on %s: %v", role, topic, lines[0])
		})
	}
}

// A role no live session holds — its pod was released, or its session has not registered — is
// not woken to be asked: the round ends at its bound whether or not the role answers. The row
// finishes with one Info line and writes no published line.
func TestPodUpdateAskNobodyHoldsFinishesTheRow(t *testing.T) {
	publisher := &outboxPublisher{err: fmt.Errorf("publish to the tester's topic: %w", notify.ErrNoHolder)}
	runner, logs := askRunner(t, publisher, map[claim.Role]string{claim.RoleTester: askedRound})
	row := askRow(t, 73, claim.RoleTester)
	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("the ask no session holds = %v, want the row finished", err)
	}
	lines := loggedLines(t, logs.String(), `"msg":"`+askNoHolderLine+`"`)
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["row"] != float64(row.ID) || lines[0]["round"] != askedRound {
		t.Fatalf("lines %q = %v in\n%s\nwant one Info line naming row %d and its round", askNoHolderLine, lines, logs.String(), row.ID)
	}
	if published := loggedLines(t, logs.String(), `"msg":"`+askPublishedLine+`"`); len(published) != 0 {
		t.Fatalf("the ask nobody took logged %v, want no published line", published)
	}
}

// An outbox row cannot be withdrawn, and the round it names can end while it waits. Where the
// claim no longer records that round — another round replaced it, the round ended, or this daemon
// supervises no claim of the role — the row finishes with one Info line and publishes nothing:
// a role is never asked about a round that is not its own.
func TestPodUpdateAskForARoundTheClaimLeftPublishesNothing(t *testing.T) {
	for i, tc := range []struct {
		name    string
		rounds  map[claim.Role]string
		current string
	}{
		{name: "a claim another round replaced it on", rounds: map[claim.Role]string{claim.RoleTester: "pod:51c2e8aa"}, current: "pod:51c2e8aa"},
		{name: "a claim whose round ended", rounds: map[claim.Role]string{claim.RoleTester: ""}, current: ""},
		{name: "a role this daemon supervises no claim of", rounds: map[claim.Role]string{}, current: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publisher := &outboxPublisher{}
			runner, logs := askRunner(t, publisher, tc.rounds)
			row := askRow(t, int64(74+i), claim.RoleTester)
			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("the ask of a round the claim left = %v, want the row finished", err)
			}
			if published := publisher.publishes(); len(published) != 0 {
				t.Fatalf("publishes = %#v, want none: the round is not the claim's", published)
			}
			lines := loggedLines(t, logs.String(), `"msg":"`+askLeftLine+`"`)
			if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["row"] != float64(row.ID) ||
				lines[0]["round"] != askedRound || lines[0]["current"] != tc.current {
				t.Fatalf("lines %q = %v in\n%s\nwant one Info line naming row %d, its round and the claim's %q", askLeftLine, lines, logs.String(), row.ID, tc.current)
			}
		})
	}
}

// Every publish failure but a role with no holder keeps the row's retry: a holder that did not
// confirm the ask in time can take it on the next attempt, a holder that does not advertise the
// delivery mode can be replaced by another session on the role, and a forward the listener's own
// NATS grant denies goes through once an operator changes that grant. No published line is
// written for any of them.
func TestPodUpdateAskKeepsItsRetryOnEveryOtherFailure(t *testing.T) {
	publisher := &outboxPublisher{}
	runner, logs := askRunner(t, publisher, map[claim.Role]string{claim.RoleImplementer: askedRound})
	row := askRow(t, 77, claim.RoleImplementer)
	for _, cause := range []error{
		notify.ErrNotDelivered{Reason: notify.ReasonReceiptTimeout, Holder: "ses_i"},
		notify.ErrRefusedByHolder{Holder: "ses_i"},
		notify.ErrPublishDenied{Message: "envoy: the message bus denied the publish"},
	} {
		publisher.err = fmt.Errorf("publish to the implementer's topic: %w", cause)
		err := runner.execute(context.Background(), row)
		if err == nil || !errors.Is(err, cause) {
			t.Fatalf("the ask answered %v = %v, want the answer as a failure, retried", cause, err)
		}
		t.Logf("%T: %v", cause, err)
	}
	if published := loggedLines(t, logs.String(), `"msg":"`+askPublishedLine+`"`); len(published) != 0 {
		t.Fatalf("the failed asks logged %v, want no published line", published)
	}
}

// wakeCountingTrees is the runner's tree store with every pod wake counted: TakePodWake is the
// step that brings a released pod back for a delivery (wakeForDelivery), and every other call
// goes to the store it wraps.
type wakeCountingTrees struct {
	outboxTrees
	mu    sync.Mutex
	takes int
}

func (w *wakeCountingTrees) TakePodWake(ctx context.Context, project, issue string, role claim.Role, kind string, at time.Time) (bool, error) {
	w.mu.Lock()
	w.takes++
	w.mu.Unlock()
	return w.outboxTrees.TakePodWake(ctx, project, issue, role, kind, at)
}

func (w *wakeCountingTrees) taken() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.takes
}

// The ask wakes nothing (dispatch://INFRA-2981 R1). A role whose pod an idle release took holds
// no live session, so its topic answers notify.ErrNoHolder and the row finishes there: no pod
// wake is taken, no process is resumed, and the claim stays released. The round ends at its
// bound whether or not that role answers. The negative control is c-one-channel's: an executor
// that called wakeForDelivery first, as phaseEnded does, takes a wake and resumes the claim here.
func TestPodUpdateAskWakesNoReleasedPod(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() {
		t.Fatalf("the claim after the release = %s by %s, want it released", got.State, got.SuspendedBy)
	}
	if err := rig.machine.Handle(ctx, supervise.PodUpdateOpen{Claim: rig.token, Round: askedRound, Reasons: "image"}); err != nil {
		t.Fatalf("record the round on the released claim: %v", err)
	}
	trees := &wakeCountingTrees{outboxTrees: rig.runner.trees}
	rig.runner.trees = trees
	rig.runner.notices = &outboxPublisher{err: fmt.Errorf("publish to the implementer's topic: %w", notify.ErrNoHolder)}
	resumes := len(rig.rt.CallsOf("Resume"))

	row := mustOutboxRow(t, rig.issue.Key, record.UpdateAsk{Role: claim.RoleImplementer, Round: askedRound, Pod: askedPod, Text: askedText}, time.Now())
	row.ID = 81
	if err := rig.runner.execute(ctx, row); err != nil {
		t.Fatalf("the ask to a released role = %v, want the row finished", err)
	}

	if n := trees.taken(); n != 0 {
		t.Errorf("the ask took %d pod wakes, want none: a released pod is not brought back to be asked", n)
	}
	if n := len(rig.rt.CallsOf("Resume")) - resumes; n != 0 {
		t.Errorf("the ask resumed %d processes, want none", n)
	}
	if got := rig.machine.Claim(); !got.Released() {
		t.Errorf("the claim after the ask = %s by %s, want it still released", got.State, got.SuspendedBy)
	}
	if _, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer); err != nil || open {
		t.Errorf("the role's open resume after the ask = %t, %v; want none", open, err)
	}
}

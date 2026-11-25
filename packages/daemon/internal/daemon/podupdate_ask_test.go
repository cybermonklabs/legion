package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// The round every case below asks for, the pod it was opened over, and its drift as the sweep
// queued it: the image by its short digest and two settings by their keys. olderAskText is the
// whole ask as an older daemon's sweep built it, which a row that daemon queued carries in place
// of the facts.
const (
	askedRound   = "pod:9d3f0c7a"
	askedPod     = "9d3f0c7a"
	askedImage   = "7e3a9c1b5d2f"
	olderAskText = "The daemon must replace the pod that runs this issue's roles. The reasons are: image a, now b."
)

// askedSettings are the settings the round's drift names, and askedEnd the moment it ends.
var (
	askedSettings = []string{"resources", "account"}
	askedEnd      = time.Date(2026, 11, 18, 12, 30, 0, 0, time.UTC)
)

// The five lines the executor writes, one for each way a row ends without an error. The first
// is the line the production check reads (requirement R8).
const (
	askPublishedLine = "outbox update ask published to the role topic"
	askNoHolderLine  = "outbox update ask finished undelivered: no live session holds the role"
	askLeftLine      = "outbox update ask finished without a publish: the claim no longer records its round"
	askSuspendedLine = "outbox update ask finished without a publish: the daemon suspended the claim"
	askAnsweredLine  = "outbox update ask finished without a publish: the role has answered its round"
)

// askRunner is an outbox runner as hook_prompt_test.go's unit case builds one — a project, a
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

// askRow is the ask of askedRound to role, as row id, with the round's facts.
func askRow(t *testing.T, id int64, role claim.Role) record.OutboxRow {
	t.Helper()
	row := mustOutboxRow(t, "LEGION-208", record.UpdateAsk{Role: role, Round: askedRound, Pod: askedPod,
		Images: []string{askedImage}, Settings: askedSettings, EndsAt: askedEnd}, time.Now())
	row.ID = id
	return row
}

// The ask reaches the role it names on that role's own topic, a worker and the architect alike,
// since the architect's process runs in the pod too. It goes as its kind's record, an object
// whose fields are the round's facts (record.UpdateAsk.Record), under the summary
// `update-ask on <KEY>` and the row's own key, so a retried row is one delivery. When the
// publisher has taken it, the executor writes the one line the production check greps for, with
// the row, the issue, the round, the pod, the claim, the role and the topic.
func TestPodUpdateAskReachesEachRoleOnItsOwnTopic(t *testing.T) {
	t.Parallel()
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
			got := published[0]
			if got.topic != topic || got.message != "update-ask on LEGION-208" || got.key != record.OutboxKey(row.ID) {
				t.Fatalf("the ask went as %#v, want the summary %q on %s under %s", got, "update-ask on LEGION-208", topic, record.OutboxKey(row.ID))
			}
			// The publisher encodes the payload as the listener's body carries it (json.Marshal);
			// a text would encode as a JSON string, the record as an object.
			payload, err := json.Marshal(got.payload)
			if err != nil || !strings.HasPrefix(string(payload), "{") {
				t.Fatalf("the ask's payload is %T %s (%v), want the record as an object", got.payload, payload, err)
			}
			var ask texts.UpdateAskRecord
			decoder := json.NewDecoder(strings.NewReader(string(payload)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&ask); err != nil {
				t.Fatalf("decode the ask's payload %s as its record: %v", payload, err)
			}
			if ask.Kind != texts.UpdateAsk || ask.Issue != "LEGION-208" || ask.Round != askedRound || ask.Pod != askedPod ||
				!slices.Equal(ask.Images, []string{askedImage}) || !slices.Equal(ask.Settings, askedSettings) || ask.Reasons != "" ||
				!ask.EndsAt.Equal(askedEnd) || ask.PushTo != "legion/LEGION-208" || ask.BackgroundWork != texts.UpdateBackgroundStep ||
				ask.Answer != texts.UpdateAnswerOp || ask.EveryAnswerBringsItForward != texts.UpdateEarlyEnd ||
				!ask.NoAnswerDelaysIt || !ask.KeptByPushOnly || !ask.NextTaskHeld {
				t.Errorf("the ask's record is %+v, want the round's facts and the facts every ask gives", ask)
			}
			lines := loggedLines(t, logs.String(), `"msg":"`+askPublishedLine+`"`)
			if len(lines) != 1 {
				t.Fatalf("lines %q = %d in\n%s\nwant one", askPublishedLine, len(lines), logs.String())
			}
			want := map[string]any{"row": float64(row.ID), "issue": "LEGION-208", "round": askedRound, "pod": askedPod, "claim": string(token), "role": string(role), "topic": topic}
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

// A row an older daemon queued carries the whole ask that daemon's sweep built, and no facts. It
// goes as that text, unchanged, under the summary every update ask goes under
// (`update-ask on <KEY>`), at the kind's urgency.
func TestPodUpdateAskAnOlderDaemonQueuedGoesAsItsText(t *testing.T) {
	publisher := &outboxPublisher{}
	runner, _ := askRunner(t, publisher, map[claim.Role]string{claim.RoleTester: askedRound})
	row := mustOutboxRow(t, "LEGION-208", record.UpdateAsk{Role: claim.RoleTester, Round: askedRound, Pod: askedPod, Text: olderAskText}, time.Now())
	row.ID = 79
	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("run the older daemon's ask: %v", err)
	}
	published := publisher.publishes()
	if len(published) != 1 || published[0].message != "update-ask on LEGION-208" || published[0].payload != any(olderAskText) ||
		published[0].urgency != texts.Med {
		t.Fatalf("publishes = %#v, want the older daemon's text as the payload under update-ask on LEGION-208 at med", published)
	}
}

// A role no live session holds — its pod was released, or its session has not registered — is
// not woken to be asked: the round ends at its bound whether or not the role answers. The row
// finishes with one Info line and writes no published line. The claim here records no ask mark,
// and an undelivered ask writes none; podupdate_undelivered_ask_test.go holds the sweep's ask again.
func TestPodUpdateAskNobodyHoldsFinishesTheRow(t *testing.T) {
	t.Parallel()
	publisher := &outboxPublisher{err: fmt.Errorf("publish to the tester's topic: %w", notify.ErrNoHolder)}
	runner, logs := askRunner(t, publisher, map[claim.Role]string{claim.RoleTester: askedRound})
	row := askRow(t, 73, claim.RoleTester)
	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("the ask no session holds = %v, want the row finished", err)
	}
	lines := loggedLines(t, logs.String(), `"msg":"`+askNoHolderLine+`"`)
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["row"] != float64(row.ID) || lines[0]["round"] != askedRound || lines[0]["pod"] != askedPod {
		t.Fatalf("lines %q = %v in\n%s\nwant one Info line naming row %d, its round and its pod", askNoHolderLine, lines, logs.String(), row.ID)
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
	t.Parallel()
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
				lines[0]["round"] != askedRound || lines[0]["pod"] != askedPod || lines[0]["current"] != tc.current {
				t.Fatalf("lines %q = %v in\n%s\nwant one Info line naming row %d, its round, its pod and the claim's %q", askLeftLine, lines, logs.String(), row.ID, tc.current)
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
	t.Parallel()
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

func (w *wakeCountingTrees) TakePodWake(ctx context.Context, project, issue string, token claim.Token, kind string, at time.Time, mark int64) (bool, error) {
	w.mu.Lock()
	w.takes++
	w.mu.Unlock()
	return w.outboxTrees.TakePodWake(ctx, project, issue, token, kind, at, mark)
}

func (w *wakeCountingTrees) taken() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.takes
}

// The ask wakes nothing and publishes nothing to a role whose pod an idle release took
// (dispatch://INFRA-2981 R1, dispatch://INFRA-4109 R1). The machine refuses the ask of a claim the
// daemon suspended (supervise's Claim.PassedOverByRound), a released claim included, so it ends
// the row at UpdateAskSuspended before any publish, and the row finishes with one Info line of its
// own. No publish reaches the listener, which raises a role wake for every publish to a role with
// no holder. No pod wake is taken, no process is resumed, and the claim stays released. The sweep
// asks a claim only once its agent is ready (askRound reads supervise.SaidReady), and a released
// claim is not ready, so this row stands for one that the sweep queued before the release. The
// negative control is c-one-channel's: an executor that called wakeForDelivery first, as
// hookPrompt does, takes a wake and resumes the claim here.
func TestPodUpdateAskWakesNoReleasedPod(t *testing.T) {
	t.Parallel()
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
	publisher := &outboxPublisher{}
	rig.runner.notices = publisher
	logs := &syncBuffer{}
	rig.runner.log = jsonLogger(logs)
	resumes := len(rig.rt.CallsOf("Resume"))

	row := mustOutboxRow(t, rig.issue.Key, record.UpdateAsk{Role: claim.RoleImplementer, Round: askedRound, Pod: askedPod,
		Images: []string{askedImage}, Settings: askedSettings, EndsAt: askedEnd}, time.Now())
	row.ID = 81
	if err := rig.runner.execute(ctx, row); err != nil {
		t.Fatalf("the ask to a released role = %v, want the row finished", err)
	}

	lines := loggedLines(t, logs.String(), `"msg":"`+askSuspendedLine+`"`)
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["row"] != float64(row.ID) ||
		lines[0]["round"] != askedRound || lines[0]["claim"] != string(rig.token) {
		t.Errorf("lines %q = %v in\n%s\nwant one Info line naming row %d, its round and the claim %s", askSuspendedLine, lines, logs.String(), row.ID, rig.token)
	}
	if published := publisher.publishes(); len(published) != 0 {
		t.Errorf("the ask published %+v, want no publish: a publish to a role with no holder raises a role wake", published)
	}
	if n := trees.taken(); n != 0 {
		t.Errorf("the ask took %d pod wakes, want none: a released pod is not brought back to be asked", n)
	}
	if n := len(rig.rt.CallsOf("Resume")) - resumes; n != 0 {
		t.Errorf("the ask resumed %d processes, want none", n)
	}
	if got := rig.machine.Claim(); !got.Released() || got.UpdateTaskedAt != nil {
		t.Errorf("the claim after the ask = %s by %s, asked at %v; want it still released and not asked", got.State, got.SuspendedBy, got.UpdateTaskedAt)
	}
	if _, _, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer); err != nil || open {
		t.Errorf("the role's open resume after the ask = %t, %v; want none", open, err)
	}
}

package daemon

// The urgency of each text the daemon sends (dispatch://INFRA-3686/spec R5): the urgency of each
// kind on the one list, held to the Design's list of the `low` kinds; the urgency each delivery
// sends in its publish body; and what a `low` text does where its reader is released, behind a
// later text, and where its reader has no live session.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/texts"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// urgencyIssue is the root every case here sends its texts on: the issue hookRunner records,
// implementing, in a tree of its own.
const urgencyIssue = "LEGION-208"

// urgencyChecksRed is a checks-red notice of urgencyIssue in testing: low to the architect, since
// a worker holds the phase the issue stays in and the daemon has already asked the implementer to
// merge the base forward.
func urgencyChecksRed(t *testing.T) record.Notice {
	t.Helper()
	return urgencyNoticeOf(t, texts.ChecksRedRecord{NoticeHead: texts.NoticeHead{Kind: texts.ChecksRed, Issue: urgencyIssue},
		Phase: phase.Testing, ConflictsWith: "main", Head: "c0de", PullRequest: "acme/widgets#86", Why: string(record.ConflictCheckNotGreen)})
}

// urgencyHeldForFailure is a held notice of urgencyIssue for a worker's failed claim: med to the
// architect, which must act on it.
func urgencyHeldForFailure(t *testing.T) record.Notice {
	t.Helper()
	return urgencyNoticeOf(t, texts.HeldRecord{NoticeHead: texts.NoticeHead{Kind: texts.Held, Issue: urgencyIssue},
		Role: claim.RoleImplementer, Phase: phase.Implementing, Hold: texts.HoldFailure})
}

func urgencyNoticeOf(t *testing.T, rec any) record.Notice {
	t.Helper()
	notice, err := record.NoticeOf(rec)
	if err != nil {
		t.Fatalf("convert %T: %v", rec, err)
	}
	return notice
}

// urgencyEnqueue enqueues payload on issue, due at once, and answers its row's id.
func urgencyEnqueue(t *testing.T, pool *store.Pool, records record.Store, issue string, payload record.OutboxPayload) int64 {
	t.Helper()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, payload, time.Now().Add(-time.Minute)))
	var id int64
	if err := pool.QueryRow(context.Background(), "select id from outbox order by id desc limit 1").Scan(&id); err != nil {
		t.Fatalf("read the enqueued row: %v", err)
	}
	return id
}

// urgencyWakes is how many delivery wakes the record holds for role's pod on urgencyIssue: the
// one a call of wakeForDelivery takes (pods.WakeDelivery), whatever later reads the claim.
func urgencyWakes(t *testing.T, pool *store.Pool, role claim.Role) int {
	t.Helper()
	var wakes int
	if err := pool.QueryRow(context.Background(), `select count(*) from pod_resumes where issue = $1 and role = $2 and kind = $3`,
		urgencyIssue, string(role), pods.WakeDelivery).Scan(&wakes); err != nil {
		t.Fatalf("read the recorded wakes: %v", err)
	}
	return wakes
}

// urgencyNextAt is when the row id is next due.
func urgencyNextAt(t *testing.T, pool *store.Pool, id int64) time.Time {
	t.Helper()
	var at time.Time
	if err := pool.QueryRow(context.Background(), "select next_at from outbox where id = $1", id).Scan(&at); err != nil {
		t.Fatalf("read row %d: %v", id, err)
	}
	return at
}

// A checks-red notice is low to the architect: the daemon has already moved the issue and
// started the implementer. Sent to an architect whose pod the idle release released, it calls no
// wakeForDelivery — no delivery wake is recorded, the claim stays released and nothing is
// published — and its row waits the hour for the architect's next ready. The control is a held
// notice for a worker's failure, med, whose row calls wakeForDelivery: the wake is recorded and
// the claim resumed.
func TestALowNoticeCallsNoWakeForDelivery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notice func(*testing.T) record.Notice
		wakes  int
	}{
		{name: "a checks-red, low", notice: urgencyChecksRed, wakes: 0},
		{name: "a held for a worker's failure, med", notice: urgencyHeldForFailure, wakes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publisher := &outboxPublisher{}
			runner, sup, pool := hookRunner(t, publisher)
			architect := releasedClaim(t, sup, claim.RoleArchitect)
			notice := tc.notice(t)
			id := urgencyEnqueue(t, pool, runner.records, urgencyIssue, notice)
			before := time.Now()

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run the outbox: %v", err)
			}

			t.Logf("%s (urgency %s to the architect): delivery wakes %d, architect released %t, publishes %d",
				notice.Kind, noticeUrgency(notice, claim.RoleArchitect), urgencyWakes(t, pool, claim.RoleArchitect),
				architect.Claim().Released(), len(publisher.publishes()))
			if published := publisher.publishes(); len(published) != 0 {
				t.Fatalf("publishes = %#v, want none: the architect's pod is released", published)
			}
			if got := urgencyWakes(t, pool, claim.RoleArchitect); got != tc.wakes {
				t.Fatalf("delivery wakes = %d, want %d", got, tc.wakes)
			}
			if released := architect.Claim().Released(); released != (tc.wakes == 0) {
				t.Fatalf("the architect released = %t after the row, want %t", released, tc.wakes == 0)
			}
			if tc.wakes == 0 {
				if next := urgencyNextAt(t, pool, id); next.Before(before.Add(lowNoticeRecheck - time.Minute)) {
					t.Fatalf("the low notice's row is due at %s, want it to wait the hour for the architect's ready", next)
				}
			}
		})
	}
}

// bootUrgencyArchitect brings the architect's agent of its current launch up, as its pod does
// once a wake resumed it: the shim's hello, the registration and the ready.
func bootUrgencyArchitect(t *testing.T, machine *supervise.Machine) {
	t.Helper()
	held := machine.Claim()
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: held.Token, Generation: held.Generation},
		supervise.RequestRegister{Claim: held.Token, Generation: held.Generation, Session: held.Session, SessionFile: held.SessionFile},
		supervise.RequestReady{Claim: held.Token, Generation: held.Generation, Session: held.Session},
	} {
		if err := machine.Handle(context.Background(), ev); err != nil {
			t.Fatalf("handle %T for the architect: %v", ev, err)
		}
	}
	machine.Wait()
	if state := machine.Claim().State; !supervise.SaidReady(state) {
		t.Fatalf("the architect after its ready is %s, want a claim that can be prompted", state)
	}
}

// A low notice queued for a released architect, and then a med one: the low one wakes nothing and
// holds the med one back from nothing, so the med one wakes the architect's pod. Until the woken
// architect is ready its role topic has no holder. Its ready makes both due (releaseWaiting), the
// fence holds the med one behind the low one again, and both reach the architect in the order
// they were written.
func TestALowTextHoldsNoLaterTextBack(t *testing.T) {
	ctx := context.Background()
	publisher := &holderPublisher{}
	runner, sup, pool := hookRunner(t, publisher)
	architect := releasedClaim(t, sup, claim.RoleArchitect)
	token := architect.Claim().Token
	publisher.setAbsent(notify.RoleTopicPrefix + string(token))
	low := urgencyEnqueue(t, pool, runner.records, urgencyIssue, urgencyChecksRed(t))
	med := urgencyEnqueue(t, pool, runner.records, urgencyIssue, urgencyHeldForFailure(t))

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}
	if got := urgencyWakes(t, pool, claim.RoleArchitect); got != 1 || architect.Claim().Released() {
		t.Fatalf("delivery wakes = %d with the architect released %t, want the med notice to have woken the pod once",
			got, architect.Claim().Released())
	}
	if _, delivered := publisher.snapshot(); len(delivered) != 0 {
		t.Fatalf("delivered %v before the architect is back, want nothing", urgencyDelivered(delivered))
	}
	t.Logf("after the first drain: rows %d (low %d, med %d), one delivery wake, the architect resuming", outboxRows(t, pool), low, med)

	bootUrgencyArchitect(t, architect)
	publisher.setAbsent()
	if err := runner.releaseWaiting(ctx, token); err != nil {
		t.Fatalf("release the ready architect's notices: %v", err)
	}
	testwait.Eventually(t, "both notices delivered", func() bool {
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
		_, delivered := publisher.snapshot()
		return len(delivered) == 2
	})
	_, delivered := publisher.snapshot()
	var got []string
	for _, published := range delivered {
		got = append(got, fmt.Sprintf("%s at %s", published.payload.(record.Notice).Kind, published.urgency))
	}
	t.Logf("delivered in order: %v", got)
	if want := []string{"checks-red at low", "held at med"}; !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want %v: both, in the order they were written", got, want)
	}
	if got := urgencyWakes(t, pool, claim.RoleArchitect); got != 1 {
		t.Fatalf("delivery wakes = %d, want the one the med notice took", got)
	}
}

// A low text whose reader has no live session is kept or refused as a med one is on main. A low
// notice to an architect whose claim holds its role but whose session holds no topic yet is held
// on the outbox's backoff, wakes nothing, and is delivered once, at the architect's next ready. A
// low role message to a role no session holds finishes undelivered as a med one does, and one to
// a holder the listener calls unresponsive waits as a med one does. An awaiting-person notice,
// low to the controller, is stored on the controller's lane of the notification stream while no
// controller session runs, with its urgency on the envelope.
func TestALowTextToAReaderWithNoSessionIsHandledAsAMedOne(t *testing.T) {
	t.Run("a low notice to an architect with no live session is delivered once at its next ready", func(t *testing.T) {
		ctx := context.Background()
		publisher := &holderPublisher{}
		runner, sup, pool := hookRunner(t, publisher)
		token := mustClaimToken(t, urgencyIssue, claim.RoleArchitect)
		if _, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: urgencyIssue, Issue: urgencyIssue,
			Role: claim.RoleArchitect, Generation: 1, State: supervise.StateRegistered, Session: "ses-architect"}, ""); err != nil {
			t.Fatalf("create the architect's claim: %v", err)
		}
		publisher.setAbsent(notify.RoleTopicPrefix + string(token))
		urgencyEnqueue(t, pool, runner.records, urgencyIssue, urgencyChecksRed(t))

		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
		attempts, delivered := publisher.snapshot()
		if attempts != 1 || len(delivered) != 0 || outboxRows(t, pool) != 1 || urgencyWakes(t, pool, claim.RoleArchitect) != 0 {
			t.Fatalf("after a publish nobody holds: %d attempts, %d delivered, %d rows, %d wakes; want the notice held for its architect",
				attempts, len(delivered), outboxRows(t, pool), urgencyWakes(t, pool, claim.RoleArchitect))
		}

		publisher.setAbsent()
		if err := runner.releaseWaiting(ctx, token); err != nil {
			t.Fatalf("release the ready architect's notices: %v", err)
		}
		for range 2 {
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the outbox: %v", err)
			}
		}
		attempts, delivered = publisher.snapshot()
		t.Logf("after the ready: %d attempts, delivered %v, %d rows left", attempts, urgencyDelivered(delivered), outboxRows(t, pool))
		if len(delivered) != 1 || delivered[0].urgency != texts.Low || outboxRows(t, pool) != 0 {
			t.Fatalf("delivered %d (%v), %d rows left; want the low notice delivered once and its row done", len(delivered), urgenciesOf(delivered), outboxRows(t, pool))
		}
	})

	implementer := mustClaimToken(t, urgencyIssue, claim.RoleImplementer)
	roleMessages := func(t *testing.T) (low, med record.OutboxRow) {
		t.Helper()
		settled, err := record.NewRoleMessage(string(implementer), texts.CISettledRecord{RoleHead: texts.RoleHead{Kind: texts.CISettled, Issue: urgencyIssue},
			PullRequest: "acme/widgets#86", Head: "c0de", Phase: phase.Testing, Passed: 3, ListPassed: texts.ListPassed("acme/widgets", 86)},
			texts.UrgencyOf(texts.CISettled, claim.RoleImplementer, texts.Variant{Phase: phase.Testing}))
		if err != nil {
			t.Fatalf("build the CI settlement: %v", err)
		}
		moved, err := record.NewRoleMessage(string(implementer), texts.GateMovedRecord{RoleHead: texts.RoleHead{Kind: texts.GateMoved, Issue: urgencyIssue},
			Gate: texts.GateClosed, Step: "spec", Document: "dispatch://LEGION-208/artifact/spec@v2", Artifact: "spec", Version: 2, Merge: texts.MergeHeld},
			texts.UrgencyOf(texts.GateMoved, claim.RoleImplementer, texts.Variant{}))
		if err != nil {
			t.Fatalf("build the gate-moved message: %v", err)
		}
		if settled.Urgency != texts.Low || moved.Urgency != texts.Med {
			t.Fatalf("the CI settlement in testing is %s and the gate move %s to the implementer, want low and med", settled.Urgency, moved.Urgency)
		}
		return mustOutboxRow(t, urgencyIssue, settled, time.Now()), mustOutboxRow(t, urgencyIssue, moved, time.Now())
	}
	for _, tc := range []struct {
		name   string
		answer error
		wait   bool
	}{
		{name: "a low role message to a role with no holder finishes as a med one does",
			answer: fmt.Errorf("publish notice to %s: listener returned 404 Not Found: {\"reason\":\"unclaimed\"}: %w", implementer, notify.ErrNoHolder)},
		{name: "a low role message to an unresponsive holder waits as a med one does",
			answer: fmt.Errorf("publish notice to %s: listener returned 404 Not Found: %w", implementer, notify.ErrHolderUnresponsive), wait: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publisher := &outboxPublisher{err: tc.answer}
			runner := &outbox{log: quietLogger(), project: "legion", notices: publisher}
			low, med := roleMessages(t)
			lowErr := runner.execute(context.Background(), low)
			medErr := runner.execute(context.Background(), med)
			t.Logf("low: %v; med: %v; publishes at %v", lowErr, medErr, urgenciesOf(publisher.publishes()))
			for name, err := range map[string]error{"low": lowErr, "med": medErr} {
				if waits := errors.Is(err, wait.ErrWaiting); waits != tc.wait || (!tc.wait && err != nil) {
					t.Errorf("the %s message ended %v, want a wait %t and no failure", name, err, tc.wait)
				}
			}
			if got := urgenciesOf(publisher.publishes()); !reflect.DeepEqual(got, []texts.Urgency{texts.Low, texts.Med}) {
				t.Fatalf("publishes at %v, want one at low and one at med", got)
			}
		})
	}

	t.Run("an awaiting-person notice with no controller session is stored on the controller's lane", func(t *testing.T) {
		natsURL := testnats.URL(t)
		listener := startIdleRigListener(t, natsURL, "urgencyrig")
		project := fmt.Sprintf("urgency%d", time.Now().UnixNano()%1_000_000_000)
		runner := &outbox{log: quietLogger(), project: project, notices: notify.New(listener.url, "")}
		awaiting := urgencyNoticeOf(t, texts.AwaitingPersonRecord{NoticeHead: texts.NoticeHead{Kind: texts.AwaitingPerson, Issue: urgencyIssue},
			Ask: "dispatch://LEGION-208/ask/0b8f"})
		row := mustOutboxRow(t, urgencyIssue, record.ControllerNotice(awaiting), time.Now())
		row.ID = 91
		if err := runner.execute(context.Background(), row); err != nil {
			t.Fatalf("publish the awaiting-person notice with no controller session: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stream, err := workflowJetStream(t, natsURL).Stream(ctx, "ENVOY_NOTIFICATIONS")
		if err != nil {
			t.Fatalf("open the notification stream: %v", err)
		}
		topic := notify.ControllerTopic(project)
		stored, err := stream.GetLastMsgForSubject(ctx, topic)
		if err != nil {
			t.Fatalf("read the controller's lane %s: %v", topic, err)
		}
		var envelope struct {
			Topic          string `json:"topic"`
			PayloadSummary string `json:"payload_summary"`
			Payload        string `json:"payload"`
			Urgency        string `json:"urgency"`
			DedupeKey      string `json:"dedupe_key"`
		}
		if err := json.Unmarshal(stored.Data, &envelope); err != nil {
			t.Fatalf("decode the stored envelope %s: %v", stored.Data, err)
		}
		t.Logf("stored on %s: summary %q, urgency %q, key %q, payload %s", envelope.Topic, envelope.PayloadSummary, envelope.Urgency, envelope.DedupeKey, envelope.Payload)
		if envelope.Topic != topic || envelope.PayloadSummary != "awaiting-person on "+urgencyIssue || envelope.Urgency != string(texts.Low) ||
			envelope.DedupeKey != record.OutboxKey(row.ID) {
			t.Fatalf("the controller's lane stores %+v, want the awaiting-person notice at low under the row's key", envelope)
		}
	})
}

// releasedRoleMessage queues rec, a role-message record of urgencyIssue, for role's claim, which
// the idle release released, at the urgency the list gives the kind for role (texts.UrgencyOf,
// read with v), and runs the outbox once against a listener that records every publish and
// answers the released role's topic as one no live session holds (holder_lapsed, notify.ErrNoHolder).
// It answers what was published, the role's machine, the database and the urgency the row carried.
func releasedRoleMessage(t *testing.T, role claim.Role, rec any, kind texts.Kind, v texts.Variant) ([]outboxPublish, *supervise.Machine, *store.Pool, texts.Urgency) {
	t.Helper()
	token := mustClaimToken(t, urgencyIssue, role)
	publisher := &outboxPublisher{err: fmt.Errorf("publish notice to %s: listener returned 404 Not Found: {\"reason\":\"holder_lapsed\"}: %w",
		notify.RoleTopicPrefix+string(token), notify.ErrNoHolder)}
	runner, sup, pool := hookRunner(t, publisher)
	machine := releasedClaim(t, sup, role)
	message, err := record.NewRoleMessage(string(token), rec, texts.UrgencyOf(kind, role, v))
	if err != nil {
		t.Fatalf("build the %s message: %v", kind, err)
	}
	urgencyEnqueue(t, pool, runner.records, urgencyIssue, message)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}
	published := publisher.publishes()
	t.Logf("%s to the released %s at %s: publishes at %v, delivery wakes %d, released %t, rows left %d",
		kind, role, message.Urgency, urgenciesOf(published), urgencyWakes(t, pool, role), machine.Claim().Released(), outboxRows(t, pool))
	return published, machine, pool, message.Urgency
}

// A gate's move is med for each of its readers: a released merger is told the gate closed and
// the merge is held, so the row publishes the message at med whatever the claim's release (the
// listener raises the role's wake for a med publish to a role no live session holds, its
// wakesItsRole), and the daemon itself wakes nothing for it (roleMessage calls no
// wakeForDelivery). The row finishes. The negative control is #4088's drop, which ends every
// role message to a released role before the publish: then nothing is published here.
func TestAMedRoleMessageToAReleasedRoleIsPublished(t *testing.T) {
	moved := workflow.GateMovedMessage(urgencyIssue, record.DesignStepSpec, "artifact-spec", 2, false)
	published, machine, pool, urgency := releasedRoleMessage(t, claim.RoleMerger, moved, texts.GateMoved, texts.Variant{})
	if urgency != texts.Med {
		t.Fatalf("the gate move is %s to the merger, want med", urgency)
	}
	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, urgencyIssue, claim.RoleMerger))
	if len(published) != 1 || published[0].topic != topic || published[0].urgency != texts.Med {
		t.Fatalf("publishes = %#v, want the gate move published once to the released merger's topic %s at med", published, topic)
	}
	if wakes := urgencyWakes(t, pool, claim.RoleMerger); wakes != 0 || !machine.Claim().Released() {
		t.Fatalf("delivery wakes %d and merger released %t, want none and released: the listener's wake report, not the daemon's wakeForDelivery, brings the pod back", wakes, machine.Claim().Released())
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the row finished", left)
	}
}

// A CI settlement is low for an implementer that does not work the issue's phase: in reviewing,
// a released implementer is told nothing, and the row ends before the publish (#4088's drop,
// record.RoleMessage): no publish, so the listener raises no wake, no delivery wake of the
// daemon's, and the claim stays released.
func TestALowRoleMessageToAReleasedRoleIsDropped(t *testing.T) {
	settled := texts.CISettledRecord{RoleHead: texts.RoleHead{Kind: texts.CISettled, Issue: urgencyIssue},
		PullRequest: "acme/widgets#86", Head: "c0de", Phase: phase.Reviewing, Passed: 3, ListPassed: texts.ListPassed("acme/widgets", 86)}
	published, machine, pool, urgency := releasedRoleMessage(t, claim.RoleImplementer, settled, texts.CISettled, texts.Variant{Phase: phase.Reviewing})
	if urgency != texts.Low {
		t.Fatalf("the CI settlement in reviewing is %s to the implementer, want low", urgency)
	}
	if len(published) != 0 {
		t.Fatalf("publishes = %#v, want none: a low message to a released role is not worth a wake", published)
	}
	if wakes := urgencyWakes(t, pool, claim.RoleImplementer); wakes != 0 || !machine.Claim().Released() {
		t.Fatalf("delivery wakes %d and implementer released %t, want none and released", wakes, machine.Claim().Released())
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the row finished undelivered", left)
	}
}

// urgenciesOf is the urgency of each publish, in order.
func urgenciesOf(published []outboxPublish) []texts.Urgency {
	urgencies := make([]texts.Urgency, len(published))
	for i, p := range published {
		urgencies[i] = p.urgency
	}
	return urgencies
}

// urgencyDelivered is each delivered notice as "<kind> at <urgency>", in order.
func urgencyDelivered(delivered []outboxPublish) []string {
	named := make([]string, len(delivered))
	for i, p := range delivered {
		named[i] = fmt.Sprintf("%s at %s", p.payload.(record.Notice).Kind, p.urgency)
	}
	return named
}

// urgencyCase is one kind read by one reader with one variant of its row, and the urgency the
// Design gives it.
type urgencyCase struct {
	kind   texts.Kind
	reader claim.Role
	v      texts.Variant
	want   texts.Urgency
}

// designLow is the Design's list "Which kinds are `low`", each case with the variant that makes
// it low, beside the variants of the same kind and reader the list leaves at med. Every pair of a
// kind and a reader that is not here is med for every variant.
func designLow() []urgencyCase {
	arch, ctl := claim.RoleArchitect, claim.RoleController
	cases := []urgencyCase{
		// phase-finished to the architect: from an implementer, a tester, a reviewer's request for
		// changes and a merger, for the design gate's opening, and for the architect's own backward
		// move out of accepting; a planner's phase and a reviewer's approval are med.
		{texts.PhaseFinished, arch, texts.Variant{Role: claim.RoleImplementer, Phase: phase.Implementing, To: phase.Testing}, texts.Low},
		{texts.PhaseFinished, arch, texts.Variant{Role: claim.RoleTester, Phase: phase.Testing, To: phase.Reviewing}, texts.Low},
		{texts.PhaseFinished, arch, texts.Variant{Role: claim.RoleTester, Phase: phase.Testing, To: phase.Implementing}, texts.Low},
		{texts.PhaseFinished, arch, texts.Variant{Role: claim.RoleReviewer, Phase: phase.Reviewing, To: phase.Implementing}, texts.Low},
		{texts.PhaseFinished, arch, texts.Variant{Role: claim.RoleMerger, Phase: phase.Merging, To: phase.Accepting}, texts.Low},
		{texts.PhaseFinished, arch, texts.Variant{Phase: phase.Admitted, To: phase.Planning}, texts.Low},
		{texts.PhaseFinished, arch, texts.Variant{Role: claim.RoleArchitect, Phase: phase.Accepting, To: phase.Implementing}, texts.Low},
		{texts.PhaseFinished, arch, texts.Variant{Role: claim.RolePlanner, Phase: phase.Planning, To: phase.Implementing}, texts.Med},
		{texts.PhaseFinished, arch, texts.Variant{Role: claim.RoleReviewer, Phase: phase.Reviewing, To: phase.Merging}, texts.Med},
		{texts.PhaseFinished, arch, texts.Variant{Role: claim.RoleReviewer, Phase: phase.Reviewing}, texts.Med},
		// held that records the architect's own escalation; every other hold, and every hold to the
		// controller, is med.
		{texts.Held, arch, texts.Variant{Hold: texts.HoldEscalation}, texts.Low},
		{texts.Held, arch, texts.Variant{Hold: texts.HoldFailure, Role: claim.RoleImplementer, Phase: phase.Implementing}, texts.Med},
		{texts.Held, arch, texts.Variant{Hold: texts.HoldCarried}, texts.Med},
		{texts.Held, ctl, texts.Variant{Hold: texts.HoldEscalation}, texts.Med},
		{texts.Held, ctl, texts.Variant{Hold: texts.HoldFailure}, texts.Med},
		{texts.Held, ctl, texts.Variant{Hold: texts.HoldCarried}, texts.Med},
		// child-status for todo, for a join and for a child that starts planning; the other changes,
		// the adopted root's own architect's among them, are med.
		{texts.ChildStatus, arch, texts.Variant{Change: texts.ChangeTodo}, texts.Low},
		{texts.ChildStatus, arch, texts.Variant{Change: texts.ChangeJoined}, texts.Low},
		{texts.ChildStatus, arch, texts.Variant{Change: texts.ChangeStarted}, texts.Low},
		{texts.ChildStatus, arch, texts.Variant{Change: texts.ChangeLeft}, texts.Med},
		{texts.ChildStatus, arch, texts.Variant{Change: texts.ChangeBeneathAccepted}, texts.Med},
		{texts.ChildStatus, arch, texts.Variant{Change: texts.ChangeReparented}, texts.Med},
		// checks-red to the architect while a worker holds the phase the issue stays in, med in
		// accepting, which the architect holds itself, and med for an issue held from a worker's
		// phase, which no worker holds (its notice names the hold); pr-merged to the architect;
		// awaiting-person to the controller.
		{texts.ChecksRed, arch, texts.Variant{Phase: phase.Testing}, texts.Low},
		{texts.ChecksRed, arch, texts.Variant{Phase: phase.Reviewing}, texts.Low},
		{texts.ChecksRed, arch, texts.Variant{Phase: phase.Retro}, texts.Low},
		{texts.ChecksRed, arch, texts.Variant{Phase: phase.Merging}, texts.Low},
		{texts.ChecksRed, arch, texts.Variant{Phase: phase.Accepting}, texts.Med},
		{texts.ChecksRed, arch, texts.Variant{Phase: phase.Testing, Hold: texts.HoldFailure}, texts.Med},
		{texts.ChecksRed, arch, texts.Variant{Phase: phase.Reviewing, Hold: texts.HoldFailure}, texts.Med},
		{texts.ChecksRed, arch, texts.Variant{Phase: phase.Retro, Hold: texts.HoldFailure}, texts.Med},
		{texts.ChecksRed, arch, texts.Variant{Phase: phase.Merging, Hold: texts.HoldEscalation}, texts.Med},
		{texts.PRMerged, arch, texts.Variant{Phase: phase.Merging}, texts.Low},
		{texts.AwaitingPerson, ctl, texts.Variant{}, texts.Low},
	}
	// The CI settlement: a failed check, or a settlement whose checks all passed (the urgency reads
	// the phase alone), is med for the worker of the phase and low there for the architect and,
	// where it does not work the phase, the implementer; where no worker holds the phase it is med
	// for the architect and low for the implementer.
	for _, worked := range []struct {
		phase  phase.Phase
		worker claim.Role
	}{
		{phase.Planning, claim.RolePlanner}, {phase.Implementing, claim.RoleImplementer}, {phase.Retro, claim.RoleImplementer},
		{phase.Testing, claim.RoleTester}, {phase.Reviewing, claim.RoleReviewer}, {phase.Merging, claim.RoleMerger},
	} {
		v := texts.Variant{Phase: worked.phase}
		cases = append(cases, urgencyCase{texts.CISettled, worked.worker, v, texts.Med}, urgencyCase{texts.CISettled, arch, v, texts.Low})
		if worked.worker != claim.RoleImplementer {
			cases = append(cases, urgencyCase{texts.CISettled, claim.RoleImplementer, v, texts.Low})
		}
	}
	for _, unworked := range []phase.Phase{phase.Accepting, phase.Held, phase.Admitted, phase.AwaitingPerson} {
		v := texts.Variant{Phase: unworked}
		cases = append(cases, urgencyCase{texts.CISettled, arch, v, texts.Med}, urgencyCase{texts.CISettled, claim.RoleImplementer, v, texts.Low})
	}
	return cases
}

// urgencySampleVariants are the variants every other kind and reader is read with, so a rule
// that made some other kind low for one of them would be found.
func urgencySampleVariants() []texts.Variant {
	variants := []texts.Variant{{}}
	for _, c := range designLow() {
		variants = append(variants, c.v)
	}
	return variants
}

// urgencySample is a row of kind as its builder writes one, with v's facts on it: a notice of
// urgencyIssue with the fields the outbox's validation asks of its kind.
func urgencySample(t *testing.T, kind texts.Kind, v texts.Variant) record.Notice {
	t.Helper()
	fields := map[string]any{}
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	fields["kind"], fields["issue"] = kind, urgencyIssue
	switch kind {
	case texts.CatchUp:
		fields["catch_up"] = map[string]any{"generation": 1, "launch": 1, "gates": []any{}, "issues": []any{}}
	case texts.ParkedDecision:
		fields["decision"], fields["root"] = string(record.DecisionDesignApproved), urgencyIssue
		fields["ref"], fields["actor"] = "dispatch://LEGION-208/artifact/spec@v2", "user:alice"
	case texts.UpdateStalled:
		fields["update"] = map[string]any{"round": "round-1", "reasons": "", "waiting": []any{
			map[string]any{"claim": string(mustClaimToken(t, urgencyIssue, claim.RoleImplementer)), "role": "implementer", "state": "ready"}}}
	case texts.ArchitectStalled:
		fields["arm"] = string(record.ArchitectTookNothing)
	case texts.ProjectPaused:
		fields["pause"] = map[string]any{"id": 7, "by": map[string]any{"kind": record.PauseByOperator}, "at": "2026-11-11T09:00:00Z",
			"reason": "the model provider is down"}
		fields["words"] = map[string]any{"pause.reason": map[string]any{"by": record.PauseByOperator, "at": "2026-11-11T09:00:00Z"}}
	case texts.ProjectResumed:
		fields["pause"] = map[string]any{"id": 7, "by": map[string]any{"kind": record.PauseByOperator}, "at": "2026-11-11T10:00:00Z"}
	case texts.VolumeNearFull:
		fields["volume"] = map[string]any{"podUid": "pod-1", "trimmer": "trim-1", "episode": 1, "raisedBy": "first",
			"usedBytes": 48 << 30, "freeBytes": 2 << 30, "freedBytes": 1 << 30, "stopReason": "nothing left older than the minimum age",
			"sizes": []any{}, "sizesAt": "2026-11-11T09:00:00Z"}
	}
	encoded, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var notice record.Notice
	if err := json.Unmarshal(encoded, &notice); err != nil {
		t.Fatalf("decode the sample %s: %v", encoded, err)
	}
	return notice
}

// The urgency of each kind on the daemon's one list, for each of its readers and each variant of
// child-status, held and phase-finished, is the Design's ("Which kinds are `low`"): the cases of
// designLow, and med for every other kind and reader, the gate-moved message to each of its
// readers among them. A task has no urgency: it always starts a turn. The CI settlement is held
// to each of its three readers in each phase. And each kind's delivery sends that urgency in the
// publish body the Envoy listener reads: every notice through the notice executor to the
// architect, every controller notice and every controller copy through the controller's, and
// every role message through its own executor.
func TestEachKindHasTheUrgencyTheSpecSays(t *testing.T) {
	t.Run("the list", func(t *testing.T) {
		lowPairs := map[[2]string]bool{}
		for _, c := range designLow() {
			entry, ok := texts.Lookup(c.kind)
			if !ok || !slices.Contains(entry.Readers, c.reader) {
				t.Errorf("the Design's case %s to %s is no kind and reader on the list", c.kind, c.reader)
			}
			got := texts.UrgencyOf(c.kind, c.reader, c.v)
			t.Logf("%s to %s %+v: %s", c.kind, c.reader, c.v, got)
			if got != c.want {
				t.Errorf("%s to %s with %+v is %q, want %q", c.kind, c.reader, c.v, got, c.want)
			}
			lowPairs[[2]string{string(c.kind), string(c.reader)}] = true
		}
		variants := urgencySampleVariants()
		for _, entry := range texts.Entries() {
			for _, reader := range entry.Readers {
				if entry.Form == texts.Task {
					if got := texts.UrgencyOf(entry.Kind, reader, texts.Variant{}); got != "" {
						t.Errorf("the task %s to %s has urgency %q, want none: a task always starts a turn", entry.Kind, reader, got)
					}
					continue
				}
				if lowPairs[[2]string{string(entry.Kind), string(reader)}] {
					continue
				}
				for _, v := range variants {
					if got := texts.UrgencyOf(entry.Kind, reader, v); got != texts.Med {
						t.Errorf("%s to %s with %+v is %q, want med: the Design lists it nowhere as low", entry.Kind, reader, v, got)
					}
				}
				t.Logf("%s to %s: med for every variant", entry.Kind, reader)
			}
		}
	})

	t.Run("the publish body", func(t *testing.T) {
		listener := newNoticeListener()
		server := httptest.NewServer(listener)
		t.Cleanup(server.Close)
		runner, sup, pool := hookRunner(t, notify.New(server.URL, ""))
		ctx := context.Background()

		// The merger's issue, in merging at the head its pull request is open at, so the
		// merge-queue removal's row is delivered, and with a merge forward asked of that head, so
		// the ask and the hold are; an issue in merging whose merge forward landed, so the landed
		// and the review are; and a tester holding an update round, so the round's ask is.
		mergingKey, landedKey := "LEGION-209", "LEGION-210"
		for _, key := range []string{mergingKey, landedKey} {
			putOutboxIssue(t, pool, runner.records, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: key, Title: "Merging", Phase: phase.Merging, Generation: 1, Status: "in_progress", Rank: "V"})
		}
		asked := record.MergeForward{Head: "c0de", Base: "main", Phase: phase.Merging, Why: record.ConflictMergeNext, AskedAt: time.Now().UTC()}
		landed := asked
		landed.Landed, landed.ReviewAsked = "d00d", true
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if err := runner.records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: mergingKey, Repo: "acme/widgets",
				Number: 87, Branch: "legion/" + mergingKey, HeadSHA: "c0de", Base: "main", MergeForward: &asked}); err != nil {
				return err
			}
			return runner.records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: landedKey, Repo: "acme/widgets",
				Number: 88, Branch: "legion/" + landedKey, HeadSHA: "d00d", Base: "main", MergeForward: &landed})
		}); err != nil {
			t.Fatalf("record the merging pull requests: %v", err)
		}
		if _, _, err := sup.Create(ctx, supervise.Claim{Token: mustClaimToken(t, urgencyIssue, claim.RoleTester), Project: "legion",
			Tree: urgencyIssue, Issue: urgencyIssue, Role: claim.RoleTester, State: supervise.StateQueued, UpdateRound: "round-1"}, ""); err != nil {
			t.Fatalf("create the tester's claim: %v", err)
		}

		type delivery struct {
			kind   texts.Kind
			reader claim.Role
			v      texts.Variant
			want   texts.Urgency
			key    string
		}
		var sent []delivery
		send := func(kind texts.Kind, reader claim.Role, v texts.Variant, issue string, payload record.OutboxPayload) {
			t.Helper()
			id := urgencyEnqueue(t, pool, runner.records, issue, payload)
			sent = append(sent, delivery{kind, reader, v, texts.UrgencyOf(kind, reader, v), record.OutboxKey(id)})
		}
		variantsOf := func(kind texts.Kind) []texts.Variant {
			var variants []texts.Variant
			for _, c := range designLow() {
				if c.kind == kind && !slices.Contains(variants, c.v) {
					variants = append(variants, c.v)
				}
			}
			return append(variants, texts.Variant{})
		}
		for _, entry := range texts.Entries() {
			switch entry.Form {
			case texts.Notice:
				for _, v := range variantsOf(entry.Kind) {
					notice := urgencySample(t, entry.Kind, v)
					if slices.Contains(entry.Readers, claim.RoleArchitect) {
						send(entry.Kind, claim.RoleArchitect, v, urgencyIssue, notice)
					}
					if slices.Contains(entry.Readers, claim.RoleController) {
						send(entry.Kind, claim.RoleController, v, urgencyIssue, record.ControllerNotice(notice))
					}
				}
			case texts.ControllerNotice:
				for _, v := range variantsOf(entry.Kind) {
					send(entry.Kind, claim.RoleController, v, urgencyIssue, record.ControllerNotice(urgencySample(t, entry.Kind, v)))
				}
			case texts.RoleMessage:
				switch entry.Kind {
				case texts.MergeForwardAsk, texts.MergeForwardHold, texts.MergeForwardLanded, texts.MergeForwardReview:
					step := map[texts.Kind]record.MergeForwardStep{texts.MergeForwardAsk: record.MergeForwardAsk, texts.MergeForwardHold: record.MergeForwardHold,
						texts.MergeForwardLanded: record.MergeForwardLanded, texts.MergeForwardReview: record.MergeForwardReview}[entry.Kind]
					issue, number, head := mergingKey, 87, ""
					if step == record.MergeForwardLanded || step == record.MergeForwardReview {
						issue, number, head = landedKey, 88, "d00d"
					}
					for _, reader := range entry.Readers {
						v := texts.Variant{Phase: phase.Merging}
						send(entry.Kind, reader, v, issue, record.MergeForwardMessage{Role: reader, Step: step, Head: "c0de", Landed: head, Base: "main",
							Phase: phase.Merging, PullRequest: number, Why: record.ConflictMergeNext})
					}
				case texts.HookPrompt:
					send(entry.Kind, claim.RoleImplementer, texts.Variant{}, urgencyIssue, firedHook)
				case texts.UpdateAsk:
					send(entry.Kind, claim.RoleTester, texts.Variant{}, urgencyIssue, record.UpdateAsk{Role: claim.RoleTester, Round: "round-1",
						Pod: "pod-1", Images: []string{"0123456789ab"}, EndsAt: time.Now().Add(time.Hour).UTC()})
				case texts.MergeQueueRemoval:
					send(entry.Kind, claim.RoleMerger, texts.Variant{}, mergingKey, record.MergeQueueRemoval{Number: 87, Head: "c0de"})
				default:
					for _, reader := range entry.Readers {
						v := texts.Variant{Phase: phase.Testing}
						rec := reflect.New(reflect.TypeOf(entry.Record))
						if err := json.Unmarshal([]byte(fmt.Sprintf(`{"kind":%q,"issue":%q,"phase":%q}`, entry.Kind, urgencyIssue, v.Phase)), rec.Interface()); err != nil {
							t.Fatalf("build a %s record: %v", entry.Kind, err)
						}
						message, err := record.NewRoleMessage(string(mustClaimToken(t, urgencyIssue, reader)), rec.Elem().Interface(),
							texts.UrgencyOf(entry.Kind, reader, v))
						if err != nil {
							t.Fatalf("build the %s message to %s: %v", entry.Kind, reader, err)
						}
						send(entry.Kind, reader, v, urgencyIssue, message)
					}
				}
			}
		}

		testwait.Eventually(t, "every row delivered", func() bool {
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the outbox: %v", err)
			}
			return outboxRows(t, pool) == 0
		})
		bodies := map[string]listenerPublish{}
		listener.mu.Lock()
		for _, accepted := range listener.accepted {
			bodies[accepted.DedupeKey] = accepted
		}
		listener.mu.Unlock()
		kinds := map[texts.Kind]bool{}
		for _, d := range sent {
			body, ok := bodies[d.key]
			if !ok {
				t.Errorf("%s to %s (%+v) was never published", d.kind, d.reader, d.v)
				continue
			}
			kinds[d.kind] = true
			t.Logf("%s to %s %+v: published %q on %s at urgency %q", d.kind, d.reader, d.v, body.Message, body.Topic, body.Urgency)
			if body.Urgency != string(d.want) {
				t.Errorf("%s to %s with %+v went at urgency %q, want %q", d.kind, d.reader, d.v, body.Urgency, d.want)
			}
		}
		for _, entry := range texts.Entries() {
			if entry.Form != texts.Task && !kinds[entry.Kind] {
				t.Errorf("no delivery of %s was published", entry.Kind)
			}
		}
	})
}

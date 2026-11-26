package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/listenerwire"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// captureToken is the Legion project token of CAPTURE (claim.ProjectToken), which its hand-offs
// and their subjects name.
const captureToken = "capture"

func handoffJSON(t *testing.T, project, session string) []byte {
	t.Helper()
	data, err := json.Marshal(Handoff{
		OriginalTopic: "notifications.agent." + session, TargetSession: session, RoleToken: "legion-legion-legion-9-tester", Project: project,
		Envelope: HandoffEnvelope{EventID: "evt-1", Source: "agent", SourceEventID: "src-1", SourceSession: "ses_asker",
			Topic: "notifications.agent." + session, DedupeKey: "agent." + session + ".0123456789abcdef0123456789abcdef",
			PayloadSummary: "Which fruit?", TraceID: "trace-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// deliverHandoff runs consumeHandoff on d as the hand-off consumer of CAPTURE does
// (Handoffs.consume): d's metadata parsed once and handed on with it.
func deliverHandoff(ctx context.Context, d *fakeDelivery, run consumerRun, handle HandoffHandler) bool {
	metadata, _ := d.Metadata()
	return consumeHandoff(ctx, d, metadata, run, captureToken, handle)
}

// A hand-off is acknowledged only once the loop has recorded it. A handler that fails has it
// delivered again at the delay its first failure earns, NakDelay, and leaves an open failure under
// the hand-off consumer's name with the handler's error; the delivery after it, which the handler
// records, acknowledges it and deletes that failure. The handler is handed the hand-off it decoded
// with the bytes as published.
func TestAHandoffIsAcknowledgedOnlyOnceTheLoopRecordedIt(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	run := productionRun(t, pool, &lockedBuffer{}, func() time.Time { return now })
	consumer := handoffConsumerName(captureToken)
	data := handoffJSON(t, captureToken, "ses_tester")
	first := newDelivery(t, consumer, listenerwire.LegionHandoffSubject(captureToken, "ses_tester"), data, 41, now)

	if deliverHandoff(context.Background(), first, run, func(context.Context, Handoff, []byte) (bool, error) {
		return false, errors.New("the store is down")
	}) {
		t.Fatal("a hand-off the loop could not record left its consumer, want it naked")
	}
	if acks, terms, naks := first.asked(); acks != 0 || terms != 0 || !slices.Equal(naks, []time.Duration{run.spec.NakDelay}) {
		t.Fatalf("a hand-off the loop could not record asked %d acks, %d terms and naks %v, want one nak of %s", acks, terms, naks, run.spec.NakDelay)
	}
	if failure, found := intakeRecordOf(t, pool, consumer, 41); !found || failure.GivenUp() || failure.Deliveries != 1 || failure.LastError != "the store is down" {
		t.Fatalf("the record = %+v (found %t), want the hand-off consumer's open failure of delivery 1 with the loop's error", failure, found)
	}

	var got Handoff
	var raw []byte
	second := first.deliveredAgain(2)
	if !deliverHandoff(context.Background(), second, run, func(_ context.Context, h Handoff, published []byte) (bool, error) {
		got, raw = h, published
		return false, nil
	}) {
		t.Fatal("a hand-off the loop recorded did not leave its consumer")
	}
	if acks, terms, naks := second.asked(); acks != 1 || terms != 0 || len(naks) != 0 {
		t.Fatalf("a recorded hand-off asked %d acks, %d terms and naks %v, want one ack", acks, terms, naks)
	}
	if got.TargetSession != "ses_tester" || got.Envelope.SourceSession != "ses_asker" || string(raw) != string(data) {
		t.Fatalf("the handler was handed %+v (%s), want the decoded hand-off and its bytes", got, raw)
	}
	if failure, found := intakeRecordOf(t, pool, consumer, 41); found {
		t.Fatalf("the record = %+v, want the open failure deleted once the loop recorded the hand-off", failure)
	}
}

// A hand-off that cannot be read, one for another project, one whose session is not its subject's
// and one whose envelope has no id are poison: each is given up at its first delivery as a message
// that does not decode, kept under the hand-off consumer's name, and terminated, never handed to
// the loop.
func TestAHandoffTheDaemonCannotTrustIsGivenUpAsUndecodable(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	run := productionRun(t, pool, &lockedBuffer{}, func() time.Time { return now })
	consumer := handoffConsumerName(captureToken)
	for i, row := range []struct {
		name, session string
		data          []byte
	}{
		{"not JSON", "ses_tester", []byte("{")},
		{"another project", "ses_tester", handoffJSON(t, "other", "ses_tester")},
		{"another session", "ses_other", handoffJSON(t, captureToken, "ses_tester")},
		{"no envelope id", "ses_tester",
			[]byte(`{"original_topic":"t","target_session":"ses_tester","role_token":"r","project":"capture","envelope":{"source":"agent"}}`)},
	} {
		t.Run(row.name, func(t *testing.T) {
			seq := uint64(60 + i)
			delivery := newDelivery(t, consumer, listenerwire.LegionHandoffSubject(captureToken, row.session), row.data, seq, now)
			left := deliverHandoff(context.Background(), delivery, run, func(context.Context, Handoff, []byte) (bool, error) {
				t.Fatal("the poison hand-off reached the loop")
				return false, nil
			})
			if acks, terms, naks := delivery.asked(); !left || acks != 0 || terms != 1 || len(naks) != 0 {
				t.Fatalf("asked %d acks, %d terms and naks %v of JetStream (left %t), want the hand-off terminated", acks, terms, naks, left)
			}
			if given, found := intakeRecordOf(t, pool, consumer, seq); !found || given.GiveUpReason != record.GiveUpUndecodable || given.Deliveries != 1 {
				t.Fatalf("the record = %+v (found %t), want the hand-off given up at its first delivery as undecodable", given, found)
			}
		})
	}
}

// The listener writes an envelope's expects_reply as the JSON literal false for a sender that
// wants no reply and as its word for one that wants one (listenerwire.ExpectsReply), and a
// hand-off carries that envelope whole. Each decodes and reaches the loop's handler, acknowledged,
// with the value it carried. This reader once decoded expects_reply as a string of its own, so a
// hand-off carrying false did not decode and was terminated as poison, its message never answered.
func TestAHandoffReachesTheLoopWhateverItsExpectsReply(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	run := productionRun(t, pool, &lockedBuffer{}, func() time.Time { return now })
	for i, tc := range []struct {
		expects listenerwire.ExpectsReply
		wire    string
	}{
		{listenerwire.ExpectsReplyNone, `"expects_reply":false`},
		{listenerwire.ExpectsReplyRequired, `"expects_reply":"required"`},
		{listenerwire.ExpectsReplyOptional, `"expects_reply":"optional"`},
	} {
		// The bytes the listener publishes: its own hand-off body, marshalled as handOffToLegion does.
		data, err := json.Marshal(listenerwire.LegionHandoff{
			OriginalTopic: "notifications.agent.ses_tester", TargetSession: "ses_tester", RoleToken: "legion-legion-legion-9-tester", Project: captureToken,
			Envelope: listenerwire.Envelope{EventID: "evt-1", Source: "agent", SourceEventID: "src-1", SourceSession: "ses_asker",
				Topic: "notifications.agent.ses_tester", DedupeKey: "agent.ses_tester.0123456789abcdef0123456789abcdef",
				PayloadSummary: "Which fruit?", TraceID: "trace-1", ExpectsReply: tc.expects},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte(tc.wire)) {
			t.Fatalf("the listener's hand-off is %s, want it to carry %s", data, tc.wire)
		}
		var got Handoff
		reached := false
		delivery := newDelivery(t, handoffConsumerName(captureToken), listenerwire.LegionHandoffSubject(captureToken, "ses_tester"), data, uint64(70+i), now)
		deliverHandoff(context.Background(), delivery, run, func(_ context.Context, h Handoff, _ []byte) (bool, error) {
			got, reached = h, true
			return false, nil
		})
		if acks, _, _ := delivery.asked(); !reached || acks != 1 {
			t.Errorf("a hand-off carrying %s reached the loop %v and was acknowledged %d times, want it handed to the loop and acknowledged", tc.wire, reached, acks)
			continue
		}
		if string(got.Envelope.ExpectsReply) != string(tc.expects) {
			t.Errorf("a hand-off carrying %s reached the loop with expects_reply %q, want %q", tc.wire, got.Envelope.ExpectsReply, tc.expects)
		}
	}
}

// A hand-off the loop fails on every time is given up visibly, as an intake message whose handler
// always fails is: delivered again at delays doubling from NakDelay up to five minutes, each
// failure kept, and given up at the 21st delivery, the first an hour or more after its first
// failure, for the reason bound, under the hand-off consumer's own name. The give-up is counted in
// the controller's intake-gave-up notice, which names it, listed among the open give-ups the state
// shows, logged at error, and its message terminated. A replay of it hands the stored hand-off to
// the loop again: with the loop off it fails and leaves the record open, and once the loop records
// it the record is closed as applied.
func TestAHandoffTheLoopAlwaysFailsOnIsGivenUpVisibly(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	registerCaptureController(t, pool)
	ctx := context.Background()
	logs := &lockedBuffer{}
	first := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	now := first
	run := productionRun(t, pool, logs, func() time.Time { return now })
	consumer := handoffConsumerName(captureToken)
	subject := listenerwire.LegionHandoffSubject(captureToken, "ses_tester")
	data := handoffJSON(t, captureToken, "ses_tester")
	const seq = 81
	stored := newDelivery(t, consumer, subject, data, seq, first)
	calls := 0
	failing := func(context.Context, Handoff, []byte) (bool, error) {
		calls++
		return false, errAlwaysFails
	}
	for delivered := uint64(1); ; delivered++ {
		delivery := stored.deliveredAgain(delivered)
		left := deliverHandoff(ctx, delivery, run, failing)
		acks, terms, naks := delivery.asked()
		if left {
			if delivered != uint64(run.spec.GiveUpDeliveries) || acks != 0 || terms != 1 || len(naks) != 0 {
				t.Fatalf("delivery %d, %s after the first, left the consumer asking %d acks, %d terms and naks %v, want delivery 21 terminated",
					delivered, now.Sub(first), acks, terms, naks)
			}
			break
		}
		want := nakDelay(delivered, run.spec)
		if acks != 0 || terms != 0 || !slices.Equal(naks, []time.Duration{want}) {
			t.Fatalf("delivery %d asked %d acks, %d terms and naks %v, want one nak of %s", delivered, acks, terms, naks, want)
		}
		if delivered >= uint64(run.spec.GiveUpDeliveries) {
			t.Fatalf("delivery %d, %s after the first, was not given up", delivered, now.Sub(first))
		}
		now = now.Add(want)
	}
	if calls != run.spec.GiveUpDeliveries {
		t.Fatalf("the loop was handed the hand-off %d times, want once at each of its %d deliveries", calls, run.spec.GiveUpDeliveries)
	}
	given, found := intakeRecordOf(t, pool, consumer, seq)
	if !found || given.GiveUpReason != record.GiveUpBound || given.Deliveries != run.spec.GiveUpDeliveries || !given.GivenUpAt.Equal(now) ||
		!given.FirstFailedAt.Equal(first) || given.LastError != errAlwaysFails.Error() || given.EventID != "evt-1" || given.NoticeID == 0 {
		t.Fatalf("the record = %+v (found %t), want the hand-off given up at the bound at %s, its first failure at %s, counted in a notice", given, found, now, first)
	}
	if age := given.GivenUpAt.Sub(given.FirstFailedAt); age < run.spec.GiveUpAfter {
		t.Fatalf("given up %s after its first failure, want %s or more", age, run.spec.GiveUpAfter)
	}

	if notices, count := giveUpNotices(t, pool); notices != 1 || count != 1 {
		t.Fatalf("%d intake-gave-up notices, the highest counting %d, want one counting the hand-off", notices, count)
	}
	var named []record.NoticeGiveUp
	var namedJSON []byte
	if err := pool.QueryRow(ctx, `select payload->'giveUps' from outbox where id = $1`, given.NoticeID).Scan(&namedJSON); err != nil {
		t.Fatalf("read the notice's give-ups: %v", err)
	}
	if err := json.Unmarshal(namedJSON, &named); err != nil {
		t.Fatalf("decode the notice's give-ups %s: %v", namedJSON, err)
	}
	if want := []record.NoticeGiveUp{{ID: given.ID, Consumer: consumer, Subject: subject}}; !slices.Equal(named, want) {
		t.Fatalf("the notice names %+v, want %+v", named, want)
	}
	var shown record.IntakeGiveUps
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		shown, err = record.NewStore().OpenIntakeGiveUps(ctx, tx, "CAPTURE", 50)
		return err
	}); err != nil {
		t.Fatalf("read the open give-ups the state shows: %v", err)
	}
	if want := []record.IntakeGiveUpCount{{Consumer: consumer, Open: 1, NewestSeq: seq}}; !slices.Equal(shown.Consumers, want) ||
		len(shown.Newest) != 1 || shown.Newest[0].ID != given.ID {
		t.Fatalf("the state shows %+v, want the hand-off consumer's one open give-up", shown)
	}
	requireLogLine(t, logs, "level=ERROR", "intake: a message was given up", "consumer="+consumer, "delivered=21 ", "reason=bound")

	records := Records{Pool: pool, Spec: run.spec, Repositories: func() []ghrepo.Repository { return nil }}
	off, err := NewGiveUps(records)
	if err != nil {
		t.Fatalf("NewGiveUps: %v", err)
	}
	answer, err := off.Replay(ctx, record.IntakeSelector{ID: given.ID}, "operator")
	if err != nil || len(answer.Replayed) != 1 || answer.Replayed[0].Outcome != ReplayFailed || answer.Remaining != 1 {
		t.Fatalf("a replay with the loop off = %+v, %v; want it failed, the record left open", answer, err)
	}
	var replayed Handoff
	records.Handoff = func(_ context.Context, h Handoff, _ []byte) (bool, error) {
		replayed = h
		return false, nil
	}
	on, err := NewGiveUps(records)
	if err != nil {
		t.Fatalf("NewGiveUps: %v", err)
	}
	answer, err = on.Replay(ctx, record.IntakeSelector{Consumer: consumer, ThroughSeq: seq}, "operator")
	if err != nil || len(answer.Replayed) != 1 || answer.Replayed[0].Outcome != ReplayApplied || answer.Remaining != 0 {
		t.Fatalf("a replay through the loop = %+v, %v; want the hand-off applied", answer, err)
	}
	if replayed.TargetSession != "ses_tester" || replayed.Envelope.EventID != "evt-1" {
		t.Fatalf("the replay handed the loop %+v, want the stored hand-off", replayed)
	}
	if closed, _ := intakeRecordOf(t, pool, consumer, seq); closed.ClosedHow != record.IntakeApplied || closed.ClosedBy != "operator" {
		t.Fatalf("the record after the replay = %+v, want it closed as applied by the operator", closed)
	}
}

// A replay of a given-up hand-off closes its record by what the loop's handler did with it: as no
// fact when the loop dropped it with nothing recorded (a hand-off for a conversation the daemon
// does not hold, or one that asks nothing and carries nothing the loop keeps), so the record never
// says the loop recorded what it let go; and as applied when the loop recorded it, which is the
// control. A live delivery the loop drops is acknowledged as one it records is.
func TestAReplayedHandoffTheLoopDropsClosesAsNoFact(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	run := boundRun(t, pool, &lockedBuffer{}, func() time.Time { return now })
	consumer := handoffConsumerName(captureToken)
	subject := listenerwire.LegionHandoffSubject(captureToken, "ses_tester")
	data := handoffJSON(t, captureToken, "ses_tester")

	live := newDelivery(t, consumer, subject, data, 90, now)
	if !deliverHandoff(ctx, live, run, func(context.Context, Handoff, []byte) (bool, error) { return true, nil }) {
		t.Fatal("a hand-off the loop dropped did not leave its consumer")
	}
	if acks, terms, naks := live.asked(); acks != 1 || terms != 0 || len(naks) != 0 {
		t.Fatalf("a dropped hand-off asked %d acks, %d terms and naks %v, want one ack", acks, terms, naks)
	}

	records := Records{Pool: pool, Spec: run.spec, Repositories: func() []ghrepo.Repository { return nil }}
	for _, tc := range []struct {
		seq     uint64
		dropped bool
		outcome ReplayOutcome
		closed  record.IntakeClosedHow
	}{
		{seq: 91, dropped: true, outcome: ReplayNoFact, closed: record.IntakeNoFact},
		{seq: 92, outcome: ReplayApplied, closed: record.IntakeApplied},
	} {
		if !deliverHandoff(ctx, newDelivery(t, consumer, subject, data, tc.seq, now), run, func(context.Context, Handoff, []byte) (bool, error) {
			return false, errAlwaysFails
		}) {
			t.Fatalf("the hand-off at %d was not given up at its first failed delivery", tc.seq)
		}
		given, found := intakeRecordOf(t, pool, consumer, tc.seq)
		if !found || !given.GivenUp() {
			t.Fatalf("the record at %d = %+v (found %t), want the hand-off given up", tc.seq, given, found)
		}
		records.Handoff = func(context.Context, Handoff, []byte) (bool, error) { return tc.dropped, nil }
		replays, err := NewGiveUps(records)
		if err != nil {
			t.Fatalf("NewGiveUps: %v", err)
		}
		answer, err := replays.Replay(ctx, record.IntakeSelector{ID: given.ID}, "operator")
		if err != nil || len(answer.Replayed) != 1 || answer.Replayed[0].Outcome != tc.outcome || answer.Remaining != 0 {
			t.Fatalf("a replay of the hand-off the loop dropped %t = %+v, %v; want it %s", tc.dropped, answer, err, tc.outcome)
		}
		if closed, _ := intakeRecordOf(t, pool, consumer, tc.seq); closed.ClosedHow != tc.closed || closed.ClosedBy != "operator" {
			t.Fatalf("the record of the hand-off the loop dropped %t = %+v, want it closed as %s by the operator", tc.dropped, closed, tc.closed)
		}
	}
}

// A Dispatch envelope's payload reads as the targeted send's frame, with its delivery attempt, or
// as the bare event a follower route carries.
func TestADispatchEnvelopeReadsAsItsFrameOrItsBareEvent(t *testing.T) {
	t.Parallel()
	frame := HandoffEnvelope{EventID: "e1", Source: "dispatch", Payload: `{"event":{"type":"comment.created","actor":{"kind":"user","id":"p@example.com"},` +
		`"issue_key":"LEGION-9","payload":{"id":"c1","body":"@tester why?","ask_id":null}},"delivery":{"attempt":2,"mode":"btw","comment_id":"c1","target":"role:tester"}}`}
	event, err := DispatchOf(frame)
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != "comment.created" || event.Delivery == nil || event.Delivery.Attempt != 2 || event.Delivery.Target != "role:tester" || event.Payload.Body != "@tester why?" {
		t.Fatalf("frame = %+v, want the comment with its attempt", event)
	}
	bare := HandoffEnvelope{EventID: "e2", Source: "dispatch", Payload: `{"type":"comment.created","actor":{"kind":"user","id":"p@example.com"},` +
		`"issue_key":"LEGION-9","payload":{"id":"c2","body":"and?","ask_id":"a1"}}`}
	event, err = DispatchOf(bare)
	if err != nil {
		t.Fatal(err)
	}
	if event.Delivery != nil || event.Payload.AskID == nil || *event.Payload.AskID != "a1" {
		t.Fatalf("bare event = %+v, want the ask reply with no delivery", event)
	}
	if _, err := DispatchOf(HandoffEnvelope{EventID: "e3", Source: "agent", Payload: "{}"}); err == nil {
		t.Fatal("an agent's envelope decoded as Dispatch's")
	}
}

// readHandoffFixture decodes the hand-off testdata/handoff/<name>.json holds and its Dispatch event.
func readHandoffFixture(t *testing.T, name string) DispatchEvent {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "handoff", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := DecodeHandoff(data)
	if err != nil {
		t.Fatal(err)
	}
	event, err := DispatchOf(h.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

// A person's answer to an ask reads with its Dispatch event id, the person, the ask's id and kind,
// and the options they selected with their text. A reply on an ask reads with its comment id, its
// ask and thread, and whether it hands the ask back to the agent: a person's reply that leaves the
// next move with the agent does, an agent's reply that hands the ask to the person does not
// (sjawhar/legion#2163's testdata shapes, as the listener hands them off).
func TestADispatchAskAnswerAndReplyReadAsTheirHandOffCarriesThem(t *testing.T) {
	t.Parallel()
	const ask = "6c83cb7f-8f4e-4820-9e05-7c94fa725007"
	answered := readHandoffFixture(t, "ask-answered")
	answer := answered.Payload.Answer
	if answered.ID != 34 || answered.Type != "ask.answered" || !answered.Actor.Person() || answered.Actor.ID != "human" ||
		answered.Payload.ID != ask || answered.Payload.Kind != "question" || answer == nil || !slices.Equal(answer.Selected, []string{"B"}) ||
		answer.Text == nil || *answer.Text != "Use B. Why did you test A first?" || answered.HandsTurnBack() {
		t.Fatalf("ask.answered = %+v (answer %+v); want event 34, the person's answer B with its text, no hand-back", answered, answer)
	}
	reply := readHandoffFixture(t, "comment-ask-agent")
	if reply.ID != 32 || reply.Payload.ID != "4c4b2a5f-93df-4aeb-a2ca-b5c74187c8bf" || reply.Payload.AskID == nil || *reply.Payload.AskID != ask ||
		reply.Payload.ReplyTo != nil || reply.Payload.AskWaitingOn != "agent" || reply.Payload.Body != "Use B, and tell me why A failed." || !reply.HandsTurnBack() {
		t.Fatalf("person's reply = %+v; want the comment on the ask, handing it back to the agent", reply)
	}
	handed := readHandoffFixture(t, "comment-ask-human")
	if handed.Actor.Person() || handed.Payload.AskWaitingOn != "human" || handed.HandsTurnBack() {
		t.Fatalf("agent's reply = %+v; want the agent's, handing the ask to the person, not back to the agent", handed)
	}
	// A person's reply under a closed ask records no turn; it is the only answer they can still give
	// there, and #2163 relays it as a decision too.
	closed := reply
	closed.Payload.AskWaitingOn = ""
	if !closed.HandsTurnBack() {
		t.Fatal("a person's reply under a closed ask does not hand it back; want it to")
	}
}

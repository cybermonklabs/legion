package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// probeRig is a real server's notification stream, this project's four durables OpenConsumers made
// on it through a lagStandIn, which can make the Dispatch durable's counts read zero while a message
// waits (its uncounted), and the logs the consumers write.
type probeRig struct {
	js        jetstream.JetStream
	standIn   *lagStandIn
	stream    jetstream.Stream
	consumers *Consumers
	logs      *lockedBuffer
}

func newProbeRig(t *testing.T) *probeRig {
	t.Helper()
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	standIn := &lagStandIn{JetStream: js}
	read, cancel := rowRead(t)()
	defer cancel()
	consumers, err := OpenConsumers(read, standIn, migratedPool(t), consumerSpec(logs))
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	return &probeRig{js: js, standIn: standIn, stream: stream, consumers: consumers, logs: logs}
}

// position is DispatchPosition, read within the row's bound.
func (r *probeRig) position(t *testing.T) DispatchConsumerPosition {
	t.Helper()
	read, cancel := rowRead(t)()
	defer cancel()
	position, err := r.consumers.DispatchPosition(read)
	if err != nil {
		t.Fatalf("DispatchPosition: %v", err)
	}
	return position
}

// durable is the Dispatch durable's info as the server holds it, read past the stand-in.
func (r *probeRig) durable(t *testing.T) *jetstream.ConsumerInfo {
	t.Helper()
	return consumerInfo(t, r.stream, dispatchConsumerName(r.consumers.spec.Project))
}

// unconfirmed is how many of the logs' lines say the probe could not confirm the idle.
func (r *probeRig) unconfirmed() int {
	return strings.Count(r.logs.String(), "the Dispatch consumer's idle could not be confirmed")
}

// durableState is what probing must leave of the Dispatch durable: what it delivered, where its
// acknowledgements stand, its redeliveries and its counts.
type durableState struct {
	deliveredConsumer, deliveredStream, ackConsumer, ackStream uint64
	redelivered, pending                                       uint64
	ackPending                                                 int
}

func stateOf(info *jetstream.ConsumerInfo) durableState {
	return durableState{info.Delivered.Consumer, info.Delivered.Stream, info.AckFloor.Consumer, info.AckFloor.Stream,
		uint64(info.NumRedelivered), info.NumPending, info.NumAckPending}
}

// waitProbeGone waits until the project's probe is gone from stream: the server deletes it once its
// inactive threshold passes with nobody on its delivery subject. A reading made while an earlier
// one's probe still stands is refused its create (TestAProbeOfTheSameNameStillStandingReadsAsNotIdle),
// so a row that reads twice on a real server waits for this between the two.
func waitProbeGone(t *testing.T, stream jetstream.Stream, project string) {
	t.Helper()
	probe := dispatchProbeName(project)
	testwait.Eventually(t, "the probe "+probe+" gone once its inactive threshold passed", func() bool {
		read, cancel := rowRead(t)()
		defer cancel()
		_, err := stream.Consumer(read, probe)
		return errors.Is(err, jetstream.ErrConsumerNotFound)
	})
}

// The race main's confirmed idle fixed (nats-server answers a publish before it counts the message
// pending on a consumer), forced: a CAPTURE message is stored and the durable's counts read zero.
//
// Control first: a reading of the counts alone reads idle in that state, so the row reaches the
// race and not a durable that counts what waits. The probe then counts the stored message, so the
// reading is not idle and nothing it holds is released (Reached). Probing leaves the durable's
// delivered sequence, acknowledgement floor, redeliveries and counts as they were, and the probe
// itself is gone once its inactive threshold passes. Before the publish the same reading is idle,
// so the probe is what tells the two apart.
func TestTheIdleProbeCountsAStoredMessageTheDurablesCountsMiss(t *testing.T) {
	r := newProbeRig(t)
	project := r.consumers.spec.Project
	if position := r.position(t); !position.Idle || r.unconfirmed() != 0 {
		t.Fatalf("idle = %v with %d unconfirmed readings before anything was published, want an idle the probe confirmed:\n%s", position.Idle, r.unconfirmed(), r.logs)
	}
	waitProbeGone(t, r.stream, project)
	sent := publish(t, r.js, "notifications.dispatch.issue.CAPTURE.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	testwait.Eventually(t, "the server counting the stored message pending on the durable", func() bool { return r.durable(t).NumPending == 1 })
	before := stateOf(r.durable(t))

	r.standIn.uncounted = true
	read, cancel := rowRead(t)()
	handle, err := r.standIn.Consumer(read, notificationStream, dispatchConsumerName(project))
	cancel()
	if err != nil {
		t.Fatalf("read the durable through the stand-in: %v", err)
	}
	if info := handle.CachedInfo(); info.NumPending != 0 || info.NumAckPending != 0 {
		t.Fatalf("the stand-in's counts read %d pending and %d awaiting an ack, want both zero: a count-only reading would not read idle, so this row does not reach the race",
			info.NumPending, info.NumAckPending)
	}

	position := r.position(t)
	if position.Idle || position.Reached() {
		t.Fatalf("idle = %v, reached = %v with message %d stored on the durable's filter and its counts reading zero, want neither: the probe counts what the counts miss",
			position.Idle, position.Reached(), sent.Sequence)
	}
	if r.unconfirmed() != 0 {
		t.Fatalf("the probe's reading logged %d unconfirmed readings, want none: it confirmed a message waits, which is no failure:\n%s", r.unconfirmed(), r.logs)
	}
	waitProbeGone(t, r.stream, project)
	read, cancel = rowRead(t)()
	waiting, err := r.consumers.probe.pending(read, before.deliveredStream)
	cancel()
	if err != nil || waiting != 1 {
		t.Fatalf("the probe from stream sequence %d counts %d waiting (%v), want the one stored message", before.deliveredStream+1, waiting, err)
	}
	if after := stateOf(r.durable(t)); after != before {
		t.Fatalf("the durable after two probes = %+v, want it as it was before them, %+v", after, before)
	}
	waitProbeGone(t, r.stream, project)
}

// The probe counts only this project's Dispatch issue events: another project's, a GitHub event and
// the project's own wake reports leave the reading idle, and the durable's counts need not be
// zero for it, since they never counted those either. Then the same rig's CAPTURE message, the
// control, makes it not idle.
func TestTheIdleProbeCountsOnlyThisProjectsDispatchEvents(t *testing.T) {
	r := newProbeRig(t)
	r.standIn.uncounted = true
	publish(t, r.js, "notifications.dispatch.issue.OTHER.OTHER-9.issue.updated", envelopeJSON(t, "dispatch-other-9", "dispatch",
		`{"id":99,"issue_key":"OTHER-9","seq":1,"notify":true,"type":"issue.updated","payload":{}}`))
	publish(t, r.js, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))
	publish(t, r.js, "notifications.legion.capture.wake", wakeReport(t, "wake-capture-1", ListenerWakeRole, "legion-capture-capture-7-architect"))
	if position := r.position(t); !position.Idle || r.unconfirmed() != 0 {
		t.Fatalf("idle = %v with %d unconfirmed readings after only other projects', GitHub's and wake events, want a confirmed idle:\n%s", position.Idle, r.unconfirmed(), r.logs)
	}
	waitProbeGone(t, r.stream, r.consumers.spec.Project)
	publish(t, r.js, "notifications.dispatch.issue.CAPTURE.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	if position := r.position(t); position.Idle || r.unconfirmed() != 0 {
		t.Fatalf("idle = %v with %d unconfirmed readings with a CAPTURE message stored past the durable's delivered sequence, want a confirmed not idle: the control shows the probe counts what it filters",
			position.Idle, r.unconfirmed())
	}
}

// A durable whose delivered sequence stands past an older CAPTURE message the stream still keeps is
// idle: the probe counts from the sequence after the delivered one, past the older message and the
// gaps other projects' messages leave in the stream's sequence, and a durable created at the next
// message over earlier ones (TestDispatchPositionIsIdleForARecreatedConsumerBehindPreExistingMessages)
// reads idle the same way. Once the stream deleted what came after a sequence, a probe from that
// sequence is started by the server at the stream's first stored message (pendingFor), and reads
// what is stored from there: nothing of CAPTURE, so idle, and the CAPTURE message published next.
func TestTheIdleProbeCountsFromTheDurablesDeliveredSequence(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	older := publish(t, js, "notifications.dispatch.issue.CAPTURE.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	read, cancel := rowRead(t)()
	consumers, err := OpenConsumers(read, js, pool, spec)
	cancel()
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	stop := runConsumers(t, consumers, writeHandler("applied", nil))
	defer stop()
	for i := range 3 {
		publish(t, js, fmt.Sprintf("notifications.dispatch.issue.OTHER.OTHER-%d.issue.updated", i), envelopeJSON(t, fmt.Sprintf("dispatch-other-%d", i), "dispatch",
			fmt.Sprintf(`{"id":%d,"issue_key":"OTHER-%d","seq":1,"notify":true,"type":"issue.updated","payload":{}}`, 100+i, i)))
	}
	applied := publish(t, js, "notifications.dispatch.issue.CAPTURE.CAPTURE-4.issue.created", capturedIssueCreatedEnvelope(t))
	dispatch := dispatchConsumerName(spec.Project)
	assertAcknowledgedThrough(t, stream, dispatch, applied)
	if delivered := consumerInfo(t, stream, dispatch).Delivered.Stream; delivered != applied.Sequence || delivered <= older.Sequence+1 {
		t.Fatalf("the durable delivered through stream sequence %d, want %d, past the older CAPTURE message at %d and a gap",
			delivered, applied.Sequence, older.Sequence)
	}
	publish(t, js, "notifications.dispatch.issue.OTHER.OTHER-9.issue.updated", envelopeJSON(t, "dispatch-other-9", "dispatch",
		`{"id":99,"issue_key":"OTHER-9","seq":1,"notify":true,"type":"issue.updated","payload":{}}`))
	read, cancel = rowRead(t)()
	position, err := consumers.DispatchPosition(read)
	cancel()
	if err != nil || !position.Idle {
		t.Fatalf("DispatchPosition = %+v, %v, want idle: nothing of CAPTURE is stored past the delivered sequence", position, err)
	}
	waitProbeGone(t, stream, spec.Project)

	read, cancel = rowRead(t)()
	err = stream.Purge(read)
	cancel()
	if err != nil {
		t.Fatalf("purge the stream: %v", err)
	}
	publish(t, js, "notifications.dispatch.issue.OTHER.OTHER-10.issue.updated", envelopeJSON(t, "dispatch-other-10", "dispatch",
		`{"id":110,"issue_key":"OTHER-10","seq":1,"notify":true,"type":"issue.updated","payload":{}}`))
	for _, row := range []struct {
		name string
		want uint64
	}{{"nothing of CAPTURE stored after the deletion", 0}, {"the CAPTURE message stored after it", 1}} {
		if row.want == 1 {
			publish(t, js, "notifications.dispatch.issue.CAPTURE.CAPTURE-5.issue.updated", capturedIssueUpdatedEnvelope(t))
		}
		read, cancel = rowRead(t)()
		waiting, err := consumers.probe.pending(read, older.Sequence)
		cancel()
		if err != nil || waiting != row.want {
			t.Fatalf("%s: the probe from stream sequence %d, which the stream deleted, counts %d (%v), want %d",
				row.name, older.Sequence+1, waiting, err, row.want)
		}
		waitProbeGone(t, stream, spec.Project)
	}
}

// fakeProbeConn is the probe's connection a row stands in: it answers each create with answer,
// and keeps each request, decoded, with how long its context had left when it arrived (-1 for no
// deadline).
type fakeProbeConn struct {
	answer func(ctx context.Context, request probeRequest) (*nats.Msg, error)

	mu       sync.Mutex
	inboxes  int
	subjects []string
	requests []probeRequest
	left     []time.Duration
}

func (f *fakeProbeConn) NewInbox() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inboxes++
	return fmt.Sprintf("_INBOX_legion-daemon-CAPTURE.probe%d", f.inboxes)
}

func (f *fakeProbeConn) RequestMsgWithContext(ctx context.Context, msg *nats.Msg) (*nats.Msg, error) {
	var request probeRequest
	if err := json.Unmarshal(msg.Data, &request); err != nil {
		return nil, fmt.Errorf("the fake cannot read the probe's create: %w", err)
	}
	left := time.Duration(-1)
	if deadline, ok := ctx.Deadline(); ok {
		left = time.Until(deadline)
	}
	f.mu.Lock()
	f.subjects, f.requests, f.left = append(f.subjects, msg.Subject), append(f.requests, request), append(f.left, left)
	f.mu.Unlock()
	return f.answer(ctx, request)
}

// infoAnswer is the answer a server gives the create request names: the probe's info as the leader
// captured it, with pending messages on its filter and nothing delivered, changed by edit.
func infoAnswer(t *testing.T, request probeRequest, pending uint64, edit func(info map[string]any)) *nats.Msg {
	t.Helper()
	info := map[string]any{
		"type": "io.nats.jetstream.api.v1.consumer_create_response", "stream_name": request.Stream, "name": request.Config.Name,
		"config": map[string]any{
			"name": request.Config.Name, "description": request.Config.Description, "deliver_policy": request.Config.DeliverPolicy,
			"opt_start_seq": request.Config.OptStartSeq, "ack_policy": request.Config.AckPolicy, "filter_subject": request.Config.FilterSubject,
			"deliver_subject": request.Config.DeliverSubject, "replay_policy": request.Config.ReplayPolicy, "direct": true,
		},
		"delivered":   map[string]any{"consumer_seq": 0, "stream_seq": request.Config.OptStartSeq - 1},
		"ack_floor":   map[string]any{"consumer_seq": 0, "stream_seq": request.Config.OptStartSeq - 1},
		"num_pending": pending,
	}
	if edit != nil {
		edit(info)
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("encode the fake's answer: %v", err)
	}
	return &nats.Msg{Data: data}
}

// configOf is the config member of an answer infoAnswer builds, for an edit to change.
func configOf(info map[string]any) map[string]any { return info["config"].(map[string]any) }

// The probe's create is one request on the subject the grant names, with the body nats-server reads
// it by, a fresh delivery subject and description each attempt, and a deadline of idleProbeBound
// at most whatever the caller's context allows. Every answer that is not this attempt's own count —
// a create refused at once, one never answered, a probe of the name that stands past the bound, an
// undecodable answer, an answer of an earlier attempt's or another delivery subject's probe, one
// that has delivered already or starts before the sequence asked for — reads as not idle, so
// nothing held is released (Reached), and logs that the idle could not be confirmed. A create the
// server refuses for a probe of the name still standing is tried again, each try a create of its
// own, within the same bound. A caller whose context ended gets an error. Controls, first: the same
// fake answering this attempt's own create with nothing pending reads idle, and with one pending
// reads not idle with no such line; an answer whose start the server moved past messages the
// stream deleted reads idle; and a name that is free again at the second try reads idle.
func TestAnIdleProbeThatIsNotThisAttemptsCountReadsAsNotIdle(t *testing.T) {
	r := newProbeRig(t)
	delivered := r.durable(t).Delivered.Stream
	const subject = "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.legion-go-CAPTURE-dispatch-probe.notifications.dispatch.issue.CAPTURE.>"
	refused := errors.New("nats: permissions violation")
	taken := &nats.Msg{Data: []byte(`{"type":"io.nats.jetstream.api.v1.consumer_create_response","error":{"code":400,"err_code":10148,"description":"consumer already exists"}}`)}
	for _, row := range []struct {
		name       string
		answer     func(t *testing.T, ctx context.Context, request probeRequest) (*nats.Msg, error)
		idle       bool
		unconfirms bool
		// retried is whether the create is tried more than once.
		retried bool
	}{
		{name: "control: this attempt's count, nothing pending", idle: true, answer: func(t *testing.T, _ context.Context, request probeRequest) (*nats.Msg, error) {
			return infoAnswer(t, request, 0, nil), nil
		}},
		{name: "control: this attempt's count, one pending", answer: func(t *testing.T, _ context.Context, request probeRequest) (*nats.Msg, error) {
			return infoAnswer(t, request, 1, nil), nil
		}},
		{name: "control: a start the server moved past messages the stream deleted", idle: true, answer: func(t *testing.T, _ context.Context, request probeRequest) (*nats.Msg, error) {
			return infoAnswer(t, request, 0, func(info map[string]any) {
				info["delivered"] = map[string]any{"consumer_seq": 0, "stream_seq": request.Config.OptStartSeq + 40}
			}), nil
		}},
		{name: "control: a name free again at the second try", idle: true, retried: true, answer: func(t *testing.T, _ context.Context, request probeRequest) (*nats.Msg, error) {
			if request.Config.Description == "legion idle probe probe1" {
				return taken, nil
			}
			return infoAnswer(t, request, 0, nil), nil
		}},
		{name: "refused at once", unconfirms: true, answer: func(*testing.T, context.Context, probeRequest) (*nats.Msg, error) {
			return nil, refused
		}},
		{name: "never answered", unconfirms: true, answer: func(_ *testing.T, ctx context.Context, _ probeRequest) (*nats.Msg, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		{name: "a probe of the name standing past the bound", unconfirms: true, retried: true, answer: func(*testing.T, context.Context, probeRequest) (*nats.Msg, error) {
			return taken, nil
		}},
		{name: "an undecodable answer", unconfirms: true, answer: func(*testing.T, context.Context, probeRequest) (*nats.Msg, error) {
			return &nats.Msg{Data: []byte(`{"num_pending":`)}, nil
		}},
		{name: "an earlier attempt's probe", unconfirms: true, answer: func(t *testing.T, _ context.Context, request probeRequest) (*nats.Msg, error) {
			return infoAnswer(t, request, 0, func(info map[string]any) { configOf(info)["description"] = "legion idle probe an-earlier-attempt" }), nil
		}},
		{name: "another delivery subject's probe", unconfirms: true, answer: func(t *testing.T, _ context.Context, request probeRequest) (*nats.Msg, error) {
			return infoAnswer(t, request, 0, func(info map[string]any) {
				configOf(info)["deliver_subject"] = "_INBOX_legion-daemon-CAPTURE.elsewhere"
			}), nil
		}},
		{name: "another filter's probe", unconfirms: true, answer: func(t *testing.T, _ context.Context, request probeRequest) (*nats.Msg, error) {
			return infoAnswer(t, request, 0, func(info map[string]any) { configOf(info)["filter_subject"] = "notifications.dispatch.issue.OTHER.>" }), nil
		}},
		{name: "a probe that has delivered already", unconfirms: true, answer: func(t *testing.T, _ context.Context, request probeRequest) (*nats.Msg, error) {
			return infoAnswer(t, request, 0, func(info map[string]any) {
				info["delivered"] = map[string]any{"consumer_seq": 1, "stream_seq": request.Config.OptStartSeq}
			}), nil
		}},
		{name: "a probe that starts before the sequence asked for", unconfirms: true, answer: func(t *testing.T, _ context.Context, request probeRequest) (*nats.Msg, error) {
			return infoAnswer(t, request, 0, func(info map[string]any) {
				configOf(info)["opt_start_seq"] = request.Config.OptStartSeq - 1
			}), nil
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := &fakeProbeConn{}
			fake.answer = func(ctx context.Context, request probeRequest) (*nats.Msg, error) { return row.answer(t, ctx, request) }
			r.consumers.probe.conn = fake
			before := r.unconfirmed()
			// The caller's context allows a minute: the probe's own bound is what ends a create
			// nobody answers.
			read, cancel := context.WithTimeout(context.Background(), testwait.Bounded(t, time.Minute, time.Second))
			position, err := r.consumers.DispatchPosition(read)
			cancel()
			if err != nil {
				t.Fatalf("DispatchPosition: %v, want a reading: only an ended caller's context is an error", err)
			}
			if position.Idle != row.idle || position.Reached() != row.idle {
				t.Fatalf("idle = %v, reached = %v, want %v", position.Idle, position.Reached(), row.idle)
			}
			if logged := r.unconfirmed() - before; (logged == 1) != row.unconfirms || logged > 1 {
				t.Fatalf("the reading logged %d unconfirmed readings, want %d:\n%s", logged, map[bool]int{true: 1}[row.unconfirms], r.logs)
			}
			if len(fake.requests) == 0 || (len(fake.requests) > 1) != row.retried {
				t.Fatalf("the probe sent %d creates, want %s", len(fake.requests), map[bool]string{false: "one", true: "more than one"}[row.retried])
			}
			descriptions := map[string]bool{}
			for i, request := range fake.requests {
				if fake.subjects[i] != subject {
					t.Fatalf("create %d was sent on %s, want %s", i+1, fake.subjects[i], subject)
				}
				if descriptions[request.Config.Description] {
					t.Fatalf("create %d repeats the description %q of an earlier try, want each try its own", i+1, request.Config.Description)
				}
				descriptions[request.Config.Description] = true
				if left := fake.left[i]; left < 0 || left > idleProbeBound {
					t.Fatalf("create %d's context had %s left, want a deadline of at most idleProbeBound (%s) whatever the caller allows", i+1, left, idleProbeBound)
				}
			}
			want := probeRequest{Stream: notificationStream, Action: "create", Config: probeConfig{
				Name: "legion-go-CAPTURE-dispatch-probe", Description: "legion idle probe probe1", DeliverPolicy: "by_start_sequence",
				OptStartSeq: delivered + 1, AckPolicy: "none", FilterSubject: "notifications.dispatch.issue.CAPTURE.>", ReplayPolicy: "instant",
				DeliverSubject: "_INBOX_legion-daemon-CAPTURE.probe1", HeadersOnly: true, InactiveThreshold: idleProbeInactive,
				Replicas: 1, MemoryStorage: true, Direct: true,
			}}
			if fake.requests[0] != want {
				t.Fatalf("the probe's create = %+v, want %+v", fake.requests[0], want)
			}
		})
	}

	t.Run("each attempt is its own probe", func(t *testing.T) {
		fake := &fakeProbeConn{answer: func(_ context.Context, request probeRequest) (*nats.Msg, error) {
			return infoAnswer(t, request, 0, nil), nil
		}}
		r.consumers.probe.conn = fake
		for range 2 {
			if position := r.position(t); !position.Idle {
				t.Fatal("not idle from an answer to this attempt with nothing pending")
			}
		}
		first, second := fake.requests[0].Config, fake.requests[1].Config
		if first.Description == second.Description || first.DeliverSubject == second.DeliverSubject {
			t.Fatalf("two attempts sent description %q and %q, delivery subjects %q and %q, want each its own: an answer of one must never read as the other's",
				first.Description, second.Description, first.DeliverSubject, second.DeliverSubject)
		}
	})

	t.Run("a caller whose context ended", func(t *testing.T) {
		r.consumers.probe.conn = &fakeProbeConn{answer: func(ctx context.Context, _ probeRequest) (*nats.Msg, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := r.consumers.DispatchPosition(ctx); err == nil {
			t.Fatal("DispatchPosition with an ended context answered a reading, want an error")
		}
	})

	t.Run("a delivered sequence with no sequence after it", func(t *testing.T) {
		fake := &fakeProbeConn{answer: func(_ context.Context, request probeRequest) (*nats.Msg, error) {
			return infoAnswer(t, request, 0, nil), nil
		}}
		r.consumers.probe.conn = fake
		if _, err := r.consumers.probe.pending(context.Background(), math.MaxUint64); err == nil || len(fake.requests) != 0 {
			t.Fatalf("pending after the largest sequence = %v with %d creates sent, want an error and no create: a start of 0 would be the stream's first message", err, len(fake.requests))
		}
	})
}

// On a real server, a probe of the same name that still stands — an earlier reading's whose
// inactive threshold has not passed, or another daemon's — refuses this reading's create, which is
// not idle, and the durable's counts reading zero release nothing. Control: once that consumer is
// gone, the same reading is a confirmed idle.
func TestAProbeOfTheSameNameStillStandingReadsAsNotIdle(t *testing.T) {
	r := newProbeRig(t)
	probe := dispatchProbeName(r.consumers.spec.Project)
	read, cancel := rowRead(t)()
	_, err := r.stream.CreateConsumer(read, jetstream.ConsumerConfig{Name: probe, Description: "a probe still standing",
		FilterSubject: dispatchIssueFilter(r.consumers.spec.Project), AckPolicy: jetstream.AckNonePolicy, InactiveThreshold: time.Hour, MemoryStorage: true})
	cancel()
	if err != nil {
		t.Fatalf("create the standing probe: %v", err)
	}
	if position := r.position(t); position.Idle || position.Reached() || r.unconfirmed() != 1 {
		t.Fatalf("idle = %v with %d unconfirmed readings beside a standing probe of the same name, want not idle and one such line:\n%s", position.Idle, r.unconfirmed(), r.logs)
	}
	read, cancel = rowRead(t)()
	err = r.stream.DeleteConsumer(read, probe)
	cancel()
	if err != nil {
		t.Fatalf("delete the standing probe: %v", err)
	}
	if position := r.position(t); !position.Idle || r.unconfirmed() != 1 {
		t.Fatalf("idle = %v once the standing probe was gone, want a confirmed idle:\n%s", position.Idle, r.logs)
	}
}

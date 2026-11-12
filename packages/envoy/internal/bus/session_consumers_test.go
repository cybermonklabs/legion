package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

func TestReduceFilterSubjects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		subjects []string
		want     []string
	}{
		{name: "nothing", subjects: nil, want: nil},
		{name: "a duplicate is kept once", subjects: []string{"a.b", "a.b"}, want: []string{"a.b"}},
		{
			name:     "a subject and everything under it do not overlap",
			subjects: []string{"notifications.github.x.>", "notifications.github.x"},
			want:     []string{"notifications.github.x", "notifications.github.x.>"},
		},
		{
			name:     "an agent subject under every agent subject is dropped",
			subjects: []string{"notifications.agent.ses_a", "notifications.agent.>"},
			want:     []string{"notifications.agent.>"},
		},
		{
			name:     "a single-token wildcard contains its literal",
			subjects: []string{"a.*", "a.b"},
			want:     []string{"a.*"},
		},
		{
			name:     "partial overlap becomes the common prefix and >",
			subjects: []string{"a.*.c", "a.b.*"},
			want:     []string{"a.>"},
		},
		{
			name:     "first tokens that differ leave > alone",
			subjects: []string{"*.b", "a.*"},
			want:     []string{">"},
		},
		{
			name:     "a merge that contains another subject drops it",
			subjects: []string{"a.x", "a.*.c", "a.b.*"},
			want:     []string{"a.>"},
		},
		{
			name:     "merges repeat until no two overlap",
			subjects: []string{"a.b.*.d", "a.b.c.*", "a.*.x.y"},
			want:     []string{"a.>"},
		},
		{
			name:     "subjects that do not overlap stay, sorted",
			subjects: []string{"notifications.github.o.r.>", "notifications.agent.ses_a", "notifications.dispatch.issue.K-1.>"},
			want:     []string{"notifications.agent.ses_a", "notifications.dispatch.issue.K-1.>", "notifications.github.o.r.>"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ReduceFilterSubjects(tc.subjects)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ReduceFilterSubjects(%q) = %q, want %q", tc.subjects, got, tc.want)
			}
			for i, a := range got {
				for _, b := range got[i+1:] {
					if subjectsOverlap(a, b) {
						t.Fatalf("ReduceFilterSubjects(%q) = %q, where %q and %q overlap", tc.subjects, got, a, b)
					}
				}
			}
		})
	}
}

func TestSessionConsumerName(t *testing.T) {
	if name, ok := SessionConsumerName("ses_abc-123"); !ok || name != "s-ses_abc-123" {
		t.Fatalf(`SessionConsumerName("ses_abc-123") = %q, %t, want "s-ses_abc-123", true`, name, ok)
	}
	for _, id := range []string{"", "a.b", "a*", "a>", "a b", "a\tb", "a\nb", "a/b", `a\b`} {
		if name, ok := SessionConsumerName(id); ok {
			t.Errorf("SessionConsumerName(%q) = %q, true; want no consumer", id, name)
		}
	}
}

// The relay subject travels over core NATS: no stream subject captures it, so a relay frame is
// never stored a second time.
func TestDeliverSubjectIsNotStored(t *testing.T) {
	subject := DeliverSubject("ses_abc")
	if subject != "notifications.envoy.deliver.ses_abc" {
		t.Fatalf("DeliverSubject = %q", subject)
	}
	if StreamCaptures(subject) {
		t.Fatalf("the stream captures %s", subject)
	}
}

// sessionConsumersOnStream connects a client that owns the stream, on a JetStream account of the
// test's own, and SessionConsumers beside it.
func sessionConsumersOnStream(t *testing.T) (*Client, *SessionConsumers) {
	t.Helper()
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	consumers, err := ConnectSessionConsumers([]string{uri}, 0)
	if err != nil {
		t.Fatalf("connect session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	return owner, consumers
}

// publishStored publishes body on subject through JetStream and returns its stream sequence.
func publishStored(t *testing.T, owner *Client, subject, body string) uint64 {
	t.Helper()
	ack, err := owner.JS().Publish(subject, []byte(body))
	if err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
	return ack.Sequence
}

// streamIDOf is the stream's identity, which a call that creates a consumer reports.
func streamIDOf(t *testing.T, consumers *SessionConsumers) string {
	t.Helper()
	id, err := consumers.StreamID(context.Background())
	if err != nil {
		t.Fatalf("stream id: %v", err)
	}
	return id
}

// pullSession starts a pull of sessionID's consumer, stopped when the test ends.
func pullSession(t *testing.T, consumers *SessionConsumers, sessionID string) *SessionPull {
	t.Helper()
	pull, err := consumers.Pull(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("pull %s: %v", sessionID, err)
	}
	t.Cleanup(pull.Stop)
	return pull
}

// next returns the next message pull holds within 5 s.
func next(t *testing.T, pull *SessionPull) SessionMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := pull.Next(ctx)
	if err != nil {
		t.Fatalf("next message: %v", err)
	}
	return msg
}

// requireNoMessage requires pull to hold no message for window.
func requireNoMessage(t *testing.T, pull *SessionPull, window time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	if msg, err := pull.Next(ctx); err == nil {
		t.Fatalf("got message %d on %s (delivery %d), want none", msg.StreamSeq, msg.Subject, msg.Delivered)
	}
}

// A consumer as Ensure creates it carries the session consumer settings, its reduced filters, and
// replicas left to the stream's.
func TestEnsureCreatesTheConsumerWithTheSessionSettings(t *testing.T) {
	_, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	start, err := consumers.Ensure(ctx, "ses_settings", []string{
		contracts.AgentSubject("ses_settings"), "notifications.github.o.r.>", "notifications.github.o.r.pull.*",
	}, 0)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if start != (SessionConsumerStart{Created: true, StartSeq: 1, StreamID: streamIDOf(t, consumers)}) {
		t.Fatalf("Ensure of a new consumer on an empty stream = %+v, want created at 1 with no gap", start)
	}
	info, err := consumers.Info(ctx, "ses_settings")
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	config := info.Config
	if config.Durable != "s-ses_settings" {
		t.Errorf("durable = %q, want s-ses_settings", config.Durable)
	}
	if config.AckPolicy != jetstream.AckExplicitPolicy {
		t.Errorf("ack policy = %v, want explicit", config.AckPolicy)
	}
	// What DeliverNew delivers, from the sequence after the stream's last, with a start sequence the
	// ack floor reads from (ackFloor).
	if config.DeliverPolicy != jetstream.DeliverByStartSequencePolicy || config.OptStartSeq != 1 {
		t.Errorf("deliver policy = %v from %d, want by start sequence from 1", config.DeliverPolicy, config.OptStartSeq)
	}
	if config.AckWait != 60*time.Second {
		t.Errorf("ack wait = %s, want 60s", config.AckWait)
	}
	if config.MaxAckPending != 1024 {
		t.Errorf("max ack pending = %d, want 1024", config.MaxAckPending)
	}
	if config.MaxDeliver != SessionConsumerMaxDeliver {
		t.Errorf("max deliver = %d, want the cap %d", config.MaxDeliver, SessionConsumerMaxDeliver)
	}
	if config.InactiveThreshold != time.Hour {
		t.Errorf("inactive threshold = %s, want 1h", config.InactiveThreshold)
	}
	if config.Replicas != 0 {
		t.Errorf("replicas = %d, want 0 (the stream's)", config.Replicas)
	}
	if want := []string{contracts.AgentSubject("ses_settings"), "notifications.github.o.r.>"}; !slices.Equal(config.FilterSubjects, want) {
		t.Errorf("filters = %q, want %q", config.FilterSubjects, want)
	}

	// The same settings again create nothing and change nothing, and report the start the
	// consumer already has, which its session's entry records (SessionConsumerStart).
	again, err := consumers.Ensure(ctx, "ses_settings", []string{contracts.AgentSubject("ses_settings"), "notifications.github.o.r.>"}, 0)
	if err != nil || again != (SessionConsumerStart{StartSeq: config.OptStartSeq}) {
		t.Fatalf("Ensure of a consumer as it stands = %+v, %v; want nothing done and its start %d", again, err, config.OptStartSeq)
	}
}

// A new consumer delivers what the stream stores from its creation on, and a restart at its floor
// delivers nothing the stream held before it, never the stream from sequence 1: not when it has
// delivered nothing, not when its first delivery is still unacked, and not once everything it
// delivered is acked. nats-server v2.15.1 reports the ack floor of a consumer that has acked
// nothing as the sequence before the consumer's start, the same value the start-sequence clause
// of ackFloor answers (both logged below); nats-server 2.10.29 reported 0 there
// (docs/solutions/envoy/a-fresh-jetstream-consumer-reports-ack-floor-0-so-create-it-at-a-start-sequence-INFRA-594.md),
// and the clause is what keeps the floor right under either.
//
// What the stream holds before the consumer is on a topic the session follows, not on its own
// agent subject: a create with no position starts at the first message stored on that agent
// subject, since that message is the session's own and nothing else would ever deliver it
// (Ensure, dispatch://INFRA-771). A followed topic's history from before the session is not the
// session's, and this is the test that it is never delivered.
func TestRestartAtFloorNeverDeliversWhatTheStreamHeldBeforeTheConsumer(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_fresh"
	agent := contracts.AgentSubject(session)
	filters := []string{agent, "notifications.dispatch.issue.K-1.>"}
	publishStored(t, owner, "notifications.dispatch.issue.K-1.comment", "before 1")
	publishStored(t, owner, "notifications.github.o.r.push", "unfiltered")
	last := publishStored(t, owner, "notifications.dispatch.issue.K-1.comment", "before 2")

	if start, err := consumers.Ensure(ctx, session, filters, 0); err != nil || start.StartSeq != last+1 {
		t.Fatalf("Ensure of a new consumer = %+v, %v; want a start at %d", start, err, last+1)
	}
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	t.Logf("the server reports a new consumer's ack floor as %d, its start sequence being %d and the stream's last sequence %d; the floor this package reads from that is %d",
		info.AckFloor.Stream, info.Config.OptStartSeq, last, ackFloor(info))
	if floor := ackFloor(info); floor != last {
		t.Fatalf("the floor read of a consumer that has acked nothing is %d, want the sequence before its start, %d", floor, last)
	}
	start, err := consumers.RestartAtFloor(ctx, session, filters)
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if start != (SessionConsumerStart{StartSeq: last + 1, StreamID: streamIDOf(t, consumers), AckFloor: last}) {
		t.Fatalf("RestartAtFloor of a consumer that delivered nothing = %+v, want it kept at %d with no gap", start, last+1)
	}

	held := publishStored(t, owner, agent, "held")
	pull := pullSession(t, consumers, session)
	first := next(t, pull)
	if first.StreamSeq != held {
		t.Fatalf("first message = %d %q, want %d \"held\"", first.StreamSeq, first.Data, held)
	}
	pull.Stop()
	if info, err = consumers.Info(ctx, session); err != nil {
		t.Fatalf("info: %v", err)
	}
	t.Logf("with its first delivery unacked the server reports the ack floor as %d; the floor this package reads is %d", info.AckFloor.Stream, ackFloor(info))
	if floor := ackFloor(info); floor != held-1 {
		t.Fatalf("the floor read with the first delivery unacked is %d, want the sequence before its start, %d (the server reports %d)", floor, held-1, info.AckFloor.Stream)
	}
	if start, err = consumers.RestartAtFloor(ctx, session, filters); err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if start.StartSeq != held {
		t.Fatalf("RestartAtFloor with the first delivery unacked = %+v, want a start at %d", start, held)
	}
	again := pullSession(t, consumers, session)
	redelivered := next(t, again)
	if redelivered.StreamSeq != held {
		t.Fatalf("after the restart got %d %q, want %d \"held\"", redelivered.StreamSeq, redelivered.Data, held)
	}
	requireNoMessage(t, again, 300*time.Millisecond)
	if err := redelivered.Ack(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	again.Stop()
	waitForAckFloor(t, consumers, session, held)
	if start, err = consumers.RestartAtFloor(ctx, session, filters); err != nil || start.StartSeq != held+1 {
		t.Fatalf("RestartAtFloor with everything acked = %+v, %v; want a start at %d", start, err, held+1)
	}
}

// After some later messages were acked and earlier ones were not, a restart at the floor delivers
// again, in stream order, everything from the first unacked message on, the acked ones after it
// included, and each message's metadata names its stored subject, sequence, delivery and time.
func TestRestartAtFloorRedeliversTheUnackedInStreamOrder(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_restart"
	agent := contracts.AgentSubject(session)
	filters := []string{agent, "notifications.github.o.r.>"}
	if _, err := consumers.Ensure(ctx, session, filters, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	var seqs []uint64
	for _, subject := range []string{agent, "notifications.github.o.r.push", agent, "notifications.slack.c.message", agent} {
		seqs = append(seqs, publishStored(t, owner, subject, subject))
	}
	// The slack message is outside the filters, so the consumer delivers four.
	delivered := []uint64{seqs[0], seqs[1], seqs[2], seqs[4]}
	pull := pullSession(t, consumers, session)
	var first []SessionMessage
	for range delivered {
		first = append(first, next(t, pull))
	}
	for i, msg := range first {
		if msg.StreamSeq != delivered[i] || msg.Delivered != 1 {
			t.Fatalf("delivery %d = seq %d delivery %d, want seq %d delivery 1", i, msg.StreamSeq, msg.Delivered, delivered[i])
		}
		if msg.Subject != string(msg.Data) {
			t.Fatalf("delivery %d names subject %q, stored on %q", i, msg.Subject, msg.Data)
		}
		if time.Since(msg.StoredAt) > time.Minute || msg.StoredAt.After(time.Now()) {
			t.Fatalf("delivery %d stored at %s", i, msg.StoredAt)
		}
	}
	// The first and third are acked; the second and fourth are not.
	for _, index := range []int{0, 2} {
		if err := first[index].Ack(); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}
	pull.Stop()
	waitForAckFloor(t, consumers, session, seqs[0])

	start, err := consumers.RestartAtFloor(ctx, session, filters)
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if start.StartSeq != seqs[0]+1 || start.Gap {
		t.Fatalf("RestartAtFloor = %+v, want a start at %d and no gap", start, seqs[0]+1)
	}
	again := pullSession(t, consumers, session)
	for _, want := range delivered[1:] {
		if msg := next(t, again); msg.StreamSeq != want {
			t.Fatalf("after the restart got seq %d, want %d", msg.StreamSeq, want)
		}
	}
	requireNoMessage(t, again, 300*time.Millisecond)
}

// A restart of a consumer with nothing awaiting an ack keeps it: no delete and no create are sent
// (the consumer's creation time stays, and a create would fail), its filters change in place, the
// start is the sequence it examines next, and it delivers the next message once.
func TestRestartAtFloorWithNothingAwaitingAnAckKeepsTheConsumer(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_kept"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	written := publishStored(t, owner, agent, "written")
	pull := pullSession(t, consumers, session)
	if msg := next(t, pull); msg.StreamSeq != written {
		t.Fatalf("first message = %d, want %d", msg.StreamSeq, written)
	} else if err := msg.Ack(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	pull.Stop()
	waitForAckFloor(t, consumers, session, written)
	before, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}

	consumers.SetCreateFault(func(consumer string) error {
		t.Errorf("the restart created consumer %s", consumer)
		return errors.New("no create expected")
	})
	followed := []string{agent, "notifications.github.o.r.>"}
	start, err := consumers.RestartAtFloor(ctx, session, followed)
	consumers.SetCreateFault(nil)
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if start != (SessionConsumerStart{StartSeq: written + 1, StreamID: streamIDOf(t, consumers), AckFloor: written}) {
		t.Fatalf("RestartAtFloor with nothing awaiting an ack = %+v, want the consumer kept at %d", start, written+1)
	}
	after, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info after the restart: %v", err)
	}
	if !after.Created.Equal(before.Created) {
		t.Fatalf("the consumer was created again at %s, want the one created at %s kept", after.Created, before.Created)
	}
	if got := slices.Sorted(slices.Values(after.Config.FilterSubjects)); !slices.Equal(got, ReduceFilterSubjects(followed)) {
		t.Fatalf("the kept consumer filters on %v, want %v", got, ReduceFilterSubjects(followed))
	}

	nextOne := publishStored(t, owner, "notifications.github.o.r.push", "next")
	again := pullSession(t, consumers, session)
	if msg := next(t, again); msg.StreamSeq != nextOne || msg.Delivered != 1 {
		t.Fatalf("after the restart got seq %d delivery %d, want seq %d delivery 1", msg.StreamSeq, msg.Delivered, nextOne)
	}
	requireNoMessage(t, again, 300*time.Millisecond)
}

// A restart of a consumer with a message awaiting an ack still deletes and creates it at the floor:
// the consumer is a new one, and it delivers the unacked message again, then the next, in stream
// order.
func TestRestartAtFloorWithAMessageAwaitingAnAckCreatesTheConsumerAgain(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_recreated"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	held := publishStored(t, owner, agent, "held")
	pull := pullSession(t, consumers, session)
	if msg := next(t, pull); msg.StreamSeq != held {
		t.Fatalf("first message = %d, want %d", msg.StreamSeq, held)
	}
	pull.Stop()
	before, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if before.NumAckPending != 1 {
		t.Fatalf("the consumer has %d awaiting an ack, want the held message", before.NumAckPending)
	}

	start, err := consumers.RestartAtFloor(ctx, session, []string{agent})
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if start != (SessionConsumerStart{Created: true, StartSeq: held, StreamID: streamIDOf(t, consumers)}) {
		t.Fatalf("RestartAtFloor with a message awaiting an ack = %+v, want the consumer created again at %d", start, held)
	}
	after, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info after the restart: %v", err)
	}
	if after.Created.Equal(before.Created) {
		t.Fatalf("the consumer created at %s was kept, want it created again", before.Created)
	}
	later := publishStored(t, owner, agent, "later")
	again := pullSession(t, consumers, session)
	for _, want := range []uint64{held, later} {
		if msg := next(t, again); msg.StreamSeq != want {
			t.Fatalf("after the restart got seq %d, want %d", msg.StreamSeq, want)
		}
	}
	requireNoMessage(t, again, 300*time.Millisecond)
}

// waitUntil polls ready every 20 ms until it reports true, and fails the test after 5 s naming
// want, which is what the caller waited for, and the reading ready last took. One bound serves
// every caller: each waits on a state that follows a publish the server answers nothing to -- an
// ack, a nak, a delivery -- so the state arrives a moment after the call that caused it rather
// than with it, and the server it arrives at is in this process.
func waitUntil(t *testing.T, want string, ready func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		done, got := ready()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited 5s for %s; %s", want, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForAckFloor waits for the server to have taken the acks that bring sessionID's ack floor to
// floor.
func waitForAckFloor(t *testing.T, consumers *SessionConsumers, sessionID string, floor uint64) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("the ack floor of %s to reach %d", sessionID, floor), func() (bool, string) {
		info, err := consumers.Info(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("info: %v", err)
		}
		return info.AckFloor.Stream == floor, fmt.Sprintf("it stayed %d", info.AckFloor.Stream)
	})
}

// A restart whose floor lies below the stream's first sequence, the messages after the floor gone
// from the stream as aged-out ones are, and a consumer seeded at a sequence the stream no longer
// holds, are both a gap; one seeded at the sequence the stream stores next is not.
func TestAStartBelowTheStreamsFirstSequenceIsAGap(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_gap"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	gone := []uint64{publishStored(t, owner, agent, "one"), publishStored(t, owner, agent, "two")}
	last := publishStored(t, owner, agent, "three")
	for _, seq := range gone {
		if err := owner.JS().DeleteMsg(Stream, seq); err != nil {
			t.Fatalf("delete %d: %v", seq, err)
		}
	}
	start, err := consumers.RestartAtFloor(ctx, session, []string{agent})
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if start != (SessionConsumerStart{StartSeq: gone[0], Gap: true, StreamID: streamIDOf(t, consumers)}) {
		t.Fatalf("RestartAtFloor below the stream's first sequence = %+v, want it kept at %d with a gap", start, gone[0])
	}

	seeded, err := consumers.Ensure(ctx, "ses_seeded", []string{contracts.AgentSubject("ses_seeded")}, gone[0])
	if err != nil {
		t.Fatalf("ensure seeded: %v", err)
	}
	if seeded != (SessionConsumerStart{Created: true, StartSeq: gone[0], Gap: true, StreamID: streamIDOf(t, consumers)}) {
		t.Fatalf("Ensure seeded below the stream's first sequence = %+v, want created at %d with a gap", seeded, gone[0])
	}
	held, err := consumers.Ensure(ctx, "ses_held", []string{contracts.AgentSubject("ses_held")}, last+1)
	if err != nil {
		t.Fatalf("ensure held: %v", err)
	}
	if held.Gap {
		t.Fatalf("Ensure seeded at the stream's next sequence = %+v, want no gap", held)
	}
}

// Restarts made at once share their reads of the stream's state, and each still reports a gap
// exactly when the consumer it kept goes on below the stream's first sequence.
func TestRestartsAtOnceReportEachGap(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const sessions, gone = 24, 10
	ids := make([]string, sessions)
	seqs := make([]uint64, sessions)
	for i := range ids {
		ids[i] = fmt.Sprintf("ses_burst_gap_%02d", i)
		if _, err := consumers.Ensure(ctx, ids[i], []string{contracts.AgentSubject(ids[i])}, 0); err != nil {
			t.Fatalf("ensure %s: %v", ids[i], err)
		}
		seqs[i] = publishStored(t, owner, contracts.AgentSubject(ids[i]), "aged out or not")
	}
	for _, seq := range seqs[:gone] {
		if err := owner.JS().DeleteMsg(Stream, seq); err != nil {
			t.Fatalf("delete %d: %v", seq, err)
		}
	}
	streamID := streamIDOf(t, consumers)
	starts := make([]SessionConsumerStart, sessions)
	errs := make([]error, sessions)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() {
			starts[i], errs[i] = consumers.RestartAtFloor(ctx, ids[i], []string{contracts.AgentSubject(ids[i])})
		})
	}
	wg.Wait()
	for i := range ids {
		want := SessionConsumerStart{StartSeq: seqs[i], Gap: i < gone, StreamID: streamID, AckFloor: seqs[i] - 1}
		if errs[i] != nil || starts[i] != want {
			t.Errorf("RestartAtFloor of %s = %+v, %v; want %+v", ids[i], starts[i], errs[i], want)
		}
	}
}

// A pull sends its next pull request once the server has delivered the last one's whole batch or
// it expired, and an idle heartbeat keeps a pull that waits longer than two heartbeats alive, so it
// delivers every message in stream order however many requests that takes.
func TestAPullRequestsAgainAfterABatchAndAnExpiry(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_pull_requests"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	consumers.pullExpires, consumers.pullHeartbeat = time.Second, 400*time.Millisecond
	var want []uint64
	for i := range sessionPullBatch + 76 {
		want = append(want, publishStored(t, owner, agent, strconv.Itoa(i)))
	}
	pull := pullSession(t, consumers, session)
	var got []uint64
	for range want {
		msg := next(t, pull)
		got = append(got, msg.StreamSeq)
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack %d: %v", msg.StreamSeq, err)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("a pull across two batches delivered %d messages out of order or not each once", len(got))
	}
	// The request in flight expires while nothing waits; the next request takes the next message.
	time.Sleep(2500 * time.Millisecond)
	later := publishStored(t, owner, agent, "after an expiry")
	if msg := next(t, pull); msg.StreamSeq != later {
		t.Fatalf("after an expiry got %d, want %d", msg.StreamSeq, later)
	}
	pull.Stop()

	// A request that outlives two heartbeats is kept alive by them.
	consumers.pullExpires, consumers.pullHeartbeat = 5*time.Second, 500*time.Millisecond
	quiet := pullSession(t, consumers, session)
	idle, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if msg, err := quiet.Next(idle); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an idle pull's Next = %+v, %v; want it to wait out its context", msg, err)
	}
	last := publishStored(t, owner, agent, "after the heartbeats")
	if msg := next(t, quiet); msg.StreamSeq != last {
		t.Fatalf("after the heartbeats got %d, want %d", msg.StreamSeq, last)
	}
}

// The filters our code gives a consumer never overlap: Ensure reduces them first
// (ReduceFilterSubjects), so a set that overlaps becomes the subject that covers it. Overlapping
// filters cost no second delivery either - JetStream delivers a stored message to a consumer
// once, whichever of its filters match it - which is what the server's own refusal, narrowed in
// v2.15.1 from any collision (v2.10.29 server/consumer.go:611, SubjectsCollide) to one filter
// being a subset of another (v2.15.1 server/consumer.go:909, subjectIsSubsetMatch), was never
// ours to rely on.
func TestOverlappingFiltersAreReducedAndAMatchIsDeliveredOnce(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	disjoint := []string{contracts.AgentSubject("ses_filters"), "notifications.github.o.r.>"}
	if _, err := consumers.create(ctx, "ses_disjoint", "s-ses_disjoint", disjoint, 0); err != nil {
		t.Fatalf("create with filters that do not overlap: %v", err)
	}
	overlapping := []string{"notifications.github.o.r.>", "notifications.github.*.r.pull"}
	if _, err := consumers.Ensure(ctx, "ses_overlapping", overlapping, 0); err != nil {
		t.Fatalf("Ensure with the overlapping filters: %v", err)
	}
	info, err := consumers.Info(ctx, "ses_overlapping")
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if want := []string{"notifications.github.>"}; !slices.Equal(info.Config.FilterSubjects, want) {
		t.Fatalf("filters = %q, want %q", info.Config.FilterSubjects, want)
	}

	// A consumer whose two filters both match one subject is delivered that message once.
	if _, err := consumers.create(ctx, "ses_collide", "s-ses_collide", overlapping, 0); err != nil {
		t.Fatalf("create with the overlapping filters: %v", err)
	}
	pull := pullSession(t, consumers, "ses_collide")
	seq := publishStored(t, owner, "notifications.github.o.r.pull", "a message both filters match")
	if msg := next(t, pull); msg.StreamSeq != seq {
		t.Fatalf("the consumer was delivered sequence %d, want %d", msg.StreamSeq, seq)
	}
	requireNoMessage(t, pull, 2*time.Second)
}

// A filter Ensure adds starts at the consumer's position, which an update keeps: a message stored on
// the new subject before a message the consumer has since delivered is not delivered, one stored
// after the update is.
func TestEnsureAddsAFilterAtTheConsumersPosition(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_follow"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	publishStored(t, owner, "notifications.github.o.r.push", "before the follow")
	own := publishStored(t, owner, agent, "own")
	pull := pullSession(t, consumers, session)
	if msg := next(t, pull); msg.StreamSeq != own {
		t.Fatalf("first message = %d %q, want %d \"own\"", msg.StreamSeq, msg.Data, own)
	}
	start, err := consumers.Ensure(ctx, session, []string{agent, "notifications.github.o.r.>"}, 0)
	if err != nil || start.Created {
		t.Fatalf("Ensure adding a filter = %+v, %v; want an update", start, err)
	}
	after := publishStored(t, owner, "notifications.github.o.r.push", "after the follow")
	if msg := next(t, pull); msg.StreamSeq != after {
		t.Fatalf("next message = %d %q, want %d (stored after the follow)", msg.StreamSeq, msg.Data, after)
	}
	if err := consumers.UpdateFilter(ctx, session, []string{agent}); err != nil {
		t.Fatalf("update filter: %v", err)
	}
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if !slices.Equal(info.Config.FilterSubjects, []string{agent}) {
		t.Fatalf("filters after UpdateFilter = %q, want only %s", info.Config.FilterSubjects, agent)
	}
	if err := consumers.UpdateFilter(ctx, "ses_missing", []string{agent}); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("UpdateFilter of a missing consumer = %v, want ErrSessionConsumerNotFound", err)
	}
}

// InProgress restarts a message's ack wait: a message kept in progress past several ack waits is
// not delivered again, and once InProgress stops it is, a wait later.
func TestInProgressExtendsTheAckWait(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	consumers.ackWait = 2 * time.Second
	ctx := context.Background()
	const session = "ses_progress"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	seq := publishStored(t, owner, agent, "held")
	pull := pullSession(t, consumers, session)
	msg := next(t, pull)
	if msg.StreamSeq != seq || msg.Delivered != 1 {
		t.Fatalf("got seq %d delivery %d, want %d delivery 1", msg.StreamSeq, msg.Delivered, seq)
	}
	for range 6 {
		if err := msg.InProgress(); err != nil {
			t.Fatalf("in progress: %v", err)
		}
		requireNoMessage(t, pull, 500*time.Millisecond)
	}
	again := next(t, pull)
	if again.StreamSeq != seq || again.Delivered != 2 {
		t.Fatalf("after InProgress stopped got seq %d delivery %d, want %d delivery 2", again.StreamSeq, again.Delivered, seq)
	}
}

// A NAK with a delay has the message delivered again once the delay has passed.
func TestNakWithDelayRedelivers(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_nak"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	seq := publishStored(t, owner, agent, "refused")
	pull := pullSession(t, consumers, session)
	if err := next(t, pull).NakWithDelay(200 * time.Millisecond); err != nil {
		t.Fatalf("nak: %v", err)
	}
	if again := next(t, pull); again.StreamSeq != seq || again.Delivered != 2 {
		t.Fatalf("after the NAK got seq %d delivery %d, want %d delivery 2", again.StreamSeq, again.Delivered, seq)
	}
}

// A session consumer's redeliveries are capped: the server stops delivering one message after
// SessionConsumerMaxDeliver, where an unlimited consumer (-1) relayed a frame nobody can take
// for the stream's whole retention. The cap is finite and it is 240, the number the relay's nak
// ladder spends in about two hours (cmd/listener relayNakDelayFor).
func TestASessionConsumerCapsItsRedeliveries(t *testing.T) {
	_, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_max_deliver"
	if _, err := consumers.Ensure(ctx, session, []string{contracts.AgentSubject(session)}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if SessionConsumerMaxDeliver != 240 {
		t.Errorf("SessionConsumerMaxDeliver = %d, want 240", SessionConsumerMaxDeliver)
	}
	if info.Config.MaxDeliver != SessionConsumerMaxDeliver {
		t.Fatalf("max deliver = %d, want the cap %d: an unlimited consumer relays a frame the session refuses for the stream's whole retention",
			info.Config.MaxDeliver, SessionConsumerMaxDeliver)
	}
}

// A consumer standing with unlimited redeliveries -- one a listener of before this release
// created -- is moved onto the cap by the next call that reaches it, rather than left as it is:
// the settings an update compares include MaxDeliver (updated), so the registration's Ensure
// sends the update and reports it, and the consumer keeps its position through it.
func TestAConsumerCreatedWithUnlimitedRedeliveriesIsUpdatedOntoTheCap(t *testing.T) {
	_, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_max_deliver_update"
	filters := []string{contracts.AgentSubject(session)}
	created, err := consumers.Ensure(ctx, session, filters, 0)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	name, _ := SessionConsumerName(session)
	unlimited := info.Config
	unlimited.MaxDeliver = -1
	if _, err := consumers.upsert(ctx, name, unlimited, "update"); err != nil {
		t.Fatalf("set the consumer back to unlimited redeliveries: %v", err)
	}
	if info, err = consumers.Info(ctx, session); err != nil || info.Config.MaxDeliver != -1 {
		t.Fatalf("the consumer this row starts from has max deliver %d (%v), want -1", info.Config.MaxDeliver, err)
	}
	if _, changed := consumers.updated(info.Config, filters); !changed {
		t.Fatalf("updated() reports nothing to send for a consumer at -1, so no update would move it onto the cap %d", SessionConsumerMaxDeliver)
	}

	again, err := consumers.Ensure(ctx, session, filters, 0)
	if err != nil || !again.Updated {
		t.Fatalf("Ensure of the unlimited consumer = %+v, %v; want it updated", again, err)
	}
	if again.StartSeq != created.StartSeq {
		t.Errorf("the updated consumer starts at %d, want its own position %d", again.StartSeq, created.StartSeq)
	}
	if info, err = consumers.Info(ctx, session); err != nil || info.Config.MaxDeliver != SessionConsumerMaxDeliver {
		t.Fatalf("after the update max deliver = %d (%v), want the cap %d", info.Config.MaxDeliver, err, SessionConsumerMaxDeliver)
	}
}

// What the cap costs is measured here, on the consumer and on the stream, because the comment of
// SessionConsumerMaxDeliver rests on it. A message a session refuses to the cap is dropped by
// its consumer: nats-server v2.15.1 stops delivering it, takes it out of what awaits an ack,
// and publishes one $JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.<stream>.<consumer> naming the
// stream, the consumer and the stream sequence. The stream keeps the message (its retention is
// LimitsPolicy), so it stands there until its age takes it, and the server leaves its own ack
// floor below it. That floor is the one this package reads (ackFloor), so the capped message is
// above the floor and the session is handed it again: by the restart every start of its relay
// makes (RestartAtFloor), and by the consumer created again at the floor the delete recorded on
// the session's resume lane (SessionResumeSubject, Ensure). Each hands it over with its delivery
// count back at 1, so the cap costs that frame a lap of the relay's nak ladder per start of the
// relay and costs the session no message.
//
// Before INFRA-771 the floor was also the last sequence delivered while nothing awaited an ack,
// which is the state the cap leaves the consumer in, so every restart began above the capped
// message and no session was handed it again. This row read that derivation and now reads the
// one that replaced it.
//
// The cap the row drives is 2, set on the consumer itself, since the deployment's 240 would take
// two hours of the relay's nak ladder to reach and the server does the same at either.
func TestAMessageThatReachesMaxDeliverIsHandedBackAtTheNextRestart(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session, deliveries = "ses_max_deliveries", 2
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	name, _ := SessionConsumerName(session)
	capped := info.Config
	capped.MaxDeliver = deliveries
	if _, err := consumers.upsert(ctx, name, capped, "update"); err != nil {
		t.Fatalf("lower the consumer's max deliver to %d: %v", deliveries, err)
	}
	advisories, err := owner.Connection().SubscribeSync("$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>")
	if err != nil {
		t.Fatalf("subscribe to the max deliveries advisory: %v", err)
	}
	defer func() { _ = advisories.Unsubscribe() }()

	seq := publishStored(t, owner, agent, "nobody can take this")
	pull := pullSession(t, consumers, session)
	for delivery := uint64(1); delivery <= deliveries; delivery++ {
		msg := next(t, pull)
		if msg.StreamSeq != seq || msg.Delivered != delivery {
			t.Fatalf("got seq %d delivery %d, want %d delivery %d", msg.StreamSeq, msg.Delivered, seq, delivery)
		}
		if err := msg.NakWithDelay(0); err != nil {
			t.Fatalf("nak delivery %d: %v", delivery, err)
		}
	}
	requireNoMessage(t, pull, 2*time.Second)

	advisory, err := advisories.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("the server published no max deliveries advisory: %v", err)
	}
	var exceeded struct {
		Stream     string `json:"stream"`
		Consumer   string `json:"consumer"`
		StreamSeq  uint64 `json:"stream_seq"`
		Deliveries uint64 `json:"deliveries"`
	}
	if err := json.Unmarshal(advisory.Data, &exceeded); err != nil {
		t.Fatalf("decode the advisory %q: %v", advisory.Data, err)
	}
	if exceeded.Stream != Stream || exceeded.Consumer != name || exceeded.StreamSeq != seq {
		t.Errorf("the advisory names %+v, want stream %s, consumer %s, sequence %d", exceeded, Stream, name, seq)
	}
	t.Logf("the advisory on %s reports %d deliveries of sequence %d", advisory.Subject, exceeded.Deliveries, exceeded.StreamSeq)

	waitUntil(t, fmt.Sprintf("the consumer of %s to drop sequence %d from what awaits an ack", session, seq), func() (bool, string) {
		read, err := consumers.Info(ctx, session)
		if err != nil {
			t.Fatalf("info: %v", err)
		}
		info = read
		return info.NumAckPending == 0, fmt.Sprintf("it holds %d awaiting an ack", info.NumAckPending)
	})
	t.Logf("at the cap the server reports the consumer's ack floor as %d, its last delivery as %d, %d awaiting an ack and %d still to deliver; the floor this package reads from that (ackFloor) is %d",
		info.AckFloor.Stream, info.Delivered.Stream, info.NumAckPending, info.NumPending, ackFloor(info))
	if floor := ackFloor(info); floor >= seq {
		t.Fatalf("the floor read from the capped consumer is %d, at or above the capped message at %d: every restart would begin past the message and no session would be handed it again", floor, seq)
	}
	if stored, err := owner.JS().GetMsg(Stream, seq); err != nil || stored.Sequence != seq {
		t.Fatalf("the stream no longer holds sequence %d (%+v, %v), want it kept: the stream's retention is LimitsPolicy and the cap deletes nothing", seq, stored, err)
	}

	// The restart every start of a session's relay makes begins at the capped message. The
	// consumer awaits an ack for nothing, so a restart reading that alone would keep it where it
	// stands (atAckFloor) and the message would lie there undelivered.
	pull.Stop()
	start, err := consumers.RestartAtFloor(ctx, session, []string{agent})
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if start.StartSeq != seq {
		t.Fatalf("the restart starts at %d, want the capped message at %d", start.StartSeq, seq)
	}
	restarted := pullSession(t, consumers, session)
	back := next(t, restarted)
	if back.StreamSeq != seq || back.Delivered != 1 {
		t.Fatalf("the restarted consumer delivered sequence %d on delivery %d, want the capped message %d on its first delivery", back.StreamSeq, back.Delivered, seq)
	}
	t.Logf("the restart read the floor as %d, created the consumer at %d and handed the capped message over again as delivery %d", start.AckFloor, start.StartSeq, back.Delivered)
	restarted.Stop()

	// And so does the consumer a session that comes back is given, created at the floor the
	// delete recorded for it.
	floor, err := consumers.Delete(ctx, session, nil)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if floor != seq-1 {
		t.Fatalf("the delete recorded floor %d, want %d, the sequence before the capped message", floor, seq-1)
	}
	created, err := consumers.Ensure(ctx, session, []string{agent}, 0)
	if err != nil {
		t.Fatalf("ensure at the recorded floor: %v", err)
	}
	if !created.Created || created.StartSeq != seq {
		t.Fatalf("the consumer created again at the recorded floor = %+v, want it created at %d", created, seq)
	}
	resumed := next(t, pullSession(t, consumers, session))
	if resumed.StreamSeq != seq {
		t.Fatalf("the consumer created at the recorded floor delivered sequence %d, want the capped message %d", resumed.StreamSeq, seq)
	}
}

// The control beside the row above: the floor is the server's own, so a session that acked
// everything is handed none of it again. Neither the restart at that floor nor the consumer
// created again at the floor the delete recorded delivers a message the session took and acked,
// which is what the floor promises every session whose relay restarts.
func TestARestartAtTheFloorHandsBackNothingTheSessionAcked(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_acked_everything"
	agent := contracts.AgentSubject(session)
	filters := []string{agent}
	if _, err := consumers.Ensure(ctx, session, filters, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	var seqs []uint64
	for i := range 3 {
		seqs = append(seqs, publishStored(t, owner, agent, strconv.Itoa(i)))
	}
	pull := pullSession(t, consumers, session)
	for _, want := range seqs {
		msg := next(t, pull)
		if msg.StreamSeq != want {
			t.Fatalf("the session was delivered sequence %d, want %d in stream order", msg.StreamSeq, want)
		}
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack %d: %v", msg.StreamSeq, err)
		}
	}
	pull.Stop()
	last := seqs[len(seqs)-1]
	waitForAckFloor(t, consumers, session, last)
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	t.Logf("with every message acked the server reports the ack floor as %d, the last delivery as %d and %d awaiting an ack; the floor this package reads from that is %d",
		info.AckFloor.Stream, info.Delivered.Stream, info.NumAckPending, ackFloor(info))

	start, err := consumers.RestartAtFloor(ctx, session, filters)
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if start.StartSeq != last+1 {
		t.Fatalf("the restart starts at %d, want %d: the session acked every message through %d", start.StartSeq, last+1, last)
	}
	requireNoMessage(t, pullSession(t, consumers, session), 2*time.Second)

	floor, err := consumers.Delete(ctx, session, nil)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if floor != last {
		t.Fatalf("the delete recorded floor %d, want %d, everything the session acked", floor, last)
	}
	created, err := consumers.Ensure(ctx, session, filters, 0)
	if err != nil {
		t.Fatalf("ensure at the recorded floor: %v", err)
	}
	if !created.Created || created.StartSeq != last+1 {
		t.Fatalf("the consumer created again at the recorded floor = %+v, want it created at %d", created, last+1)
	}
	requireNoMessage(t, pullSession(t, consumers, session), 2*time.Second)
}

// waitForAckPending waits for sessionID's consumer to hold want messages awaiting an ack and
// answers the info that read it.
func waitForAckPending(t *testing.T, consumers *SessionConsumers, sessionID string, want int) *jetstream.ConsumerInfo {
	t.Helper()
	var info *jetstream.ConsumerInfo
	waitUntil(t, fmt.Sprintf("the consumer of %s to hold %d messages awaiting an ack", sessionID, want), func() (bool, string) {
		read, err := consumers.Info(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("info: %v", err)
		}
		info = read
		return info.NumAckPending == want, fmt.Sprintf("it holds %d", info.NumAckPending)
	})
	return info
}

// Release gives the server back, at once, every message the pull holds that its caller never
// read: the next pull of that consumer is delivered both of them straight away, each as a second
// delivery, rather than after SessionConsumerAckWait. Stop in its place drops them, and the next
// pull waits out that ack wait for them. A returned message is still awaiting an ack -- a nak
// schedules a redelivery, it does not undo the delivery it answers -- so the consumer holds
// nothing for an ack only once the next owner has taken and acked them, which is the state this
// row ends on. The order the two come back in is the server's, not this row's to assert: with no
// pull request that still has interest, the server's first redelivery attempt puts that message
// behind the other in its redelivery queue. A relay's next owner is pulling by then, and one that
// is not restarts the consumer at its ack floor (RestartAtFloor), which is where stream order
// comes from.
func TestReleaseReturnsTheMessagesThePullHoldsUnread(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_released"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	var stored []uint64
	for i := range 3 {
		stored = append(stored, publishStored(t, owner, agent, strconv.Itoa(i)))
	}
	pull := pullSession(t, consumers, session)
	first := next(t, pull)
	if first.StreamSeq != stored[0] {
		t.Fatalf("the first message is %d, want %d", first.StreamSeq, stored[0])
	}
	if err := first.Ack(); err != nil {
		t.Fatalf("ack %d: %v", first.StreamSeq, err)
	}
	// One pull request asks for a whole batch, so the server delivered all three: the two this
	// row never read sit unread in the pull's queue, and they are what the consumer awaits.
	held := waitForAckPending(t, consumers, session, 2)
	t.Logf("before Release the consumer has NumWaiting %d and NumAckPending %d", held.NumWaiting, held.NumAckPending)
	if returned, err := pull.Release(); returned != 2 || err != nil {
		t.Fatalf("Release returned %d messages, %v; want the two the pull held unread and no error", returned, err)
	}
	again := pullSession(t, consumers, session)
	back := map[uint64]uint64{}
	for range stored[1:] {
		msg := next(t, again)
		back[msg.StreamSeq] = msg.Delivered
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack %d: %v", msg.StreamSeq, err)
		}
	}
	if len(back) != 2 || back[stored[1]] != 2 || back[stored[2]] != 2 {
		t.Fatalf("after Release the next pull got %v, want %d and %d back, each as a second delivery", back, stored[1], stored[2])
	}
	released := waitForAckPending(t, consumers, session, 0)
	t.Logf("once the next pull has acked both, the consumer has NumWaiting %d (that pull's own request) and NumAckPending %d", released.NumWaiting, released.NumAckPending)
}

// waitForQueued waits for pull to hold want messages its caller has not read, which is the state
// Release gives them back from. The server reporting them delivered is a moment ahead of it: the
// pull's inbox subscription queues each message when nats.go hands it over.
func waitForQueued(t *testing.T, pull *SessionPull, want int) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("the pull of consumer %s to hold %d messages unread", pull.name, want), func() (bool, string) {
		pull.mu.Lock()
		held := len(pull.queue)
		pull.mu.Unlock()
		return held == want, fmt.Sprintf("it holds %d", held)
	})
}

// A hand-over that could not reach the server at all says so, rather than reporting a whole
// hand-over while every message it held went out with the connection. On a connection that is
// closed the drain of the pull's inbox subscription fails, and nats.go throws away what it had
// buffered there unread; the naks of what the pull itself holds fail with it. Both halves are
// named: Detach reports the drain, and Release reports that drain together with the naks and
// counts nothing returned, so the caller's hand-over line cannot claim messages reached the
// server that never left the process.
func TestAHandOverOnAClosedConnectionReportsTheDrainAndTheNaksThatFailed(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_closed"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	for i := range 3 {
		publishStored(t, owner, agent, strconv.Itoa(i))
	}
	// A connection of this row's own, on the test's account: closing it ends no other row's
	// pull and no cleanup of the connection sessionConsumersOnStream hands out.
	mine, err := ConnectSessionConsumers([]string{testnats.URL(t)}, 0)
	if err != nil {
		t.Fatalf("connect a session consumers connection of this row's own: %v", err)
	}
	t.Cleanup(mine.Close)
	pull, err := mine.Pull(ctx, session)
	if err != nil {
		t.Fatalf("pull %s: %v", session, err)
	}
	// The first Next sends the pull request, which asks for a whole batch, and reads the first
	// of the three messages the server delivers for it; the other two are queued in the pull,
	// which is what Release has to return.
	next(t, pull)
	waitForQueued(t, pull, 2)
	mine.Close()

	detachErr := pull.Detach()
	if detachErr == nil {
		t.Fatal("Detach on a closed connection reported nothing, while nats.go threw away every message the pull's inbox held unread")
	}
	if !strings.Contains(detachErr.Error(), "could not be drained") {
		t.Fatalf("Detach reported %q, want it naming the drain that failed", detachErr)
	}
	returned, err := pull.Release()
	if returned != 0 {
		t.Fatalf("Release returned %d messages on a closed connection, want none: no nak of it reached the server", returned)
	}
	if err == nil {
		t.Fatal("Release on a closed connection reported nothing, so its caller would log a whole hand-over")
	}
	if !strings.Contains(err.Error(), "could not be drained") || !strings.Contains(err.Error(), "could not return 2 of the messages it held") {
		t.Fatalf("Release reported %q, want it naming both the drain that failed and the two naks that failed", err)
	}
	t.Logf("Detach reported: %v", detachErr)
	t.Logf("Release reported: %v", err)
}

// A session the stream holds no consumer for: its info, a restart of it and a pull of it all
// report it missing; a session id that names no consumer is refused before anything is sent.
func TestSessionsWithoutAConsumer(t *testing.T) {
	_, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	if _, err := consumers.Info(ctx, "ses_absent"); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("info of a session with no consumer = %v, want ErrSessionConsumerNotFound", err)
	}
	if _, err := consumers.RestartAtFloor(ctx, "ses_absent", []string{contracts.AgentSubject("ses_absent")}); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("RestartAtFloor of a missing consumer = %v, want ErrSessionConsumerNotFound", err)
	}
	// A pull reads nothing before it starts: its first Next reports the consumer missing.
	pull := pullSession(t, consumers, "ses_absent")
	nextCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if msg, err := pull.Next(nextCtx); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("Next of a pull of a missing consumer = %+v, %v, want ErrSessionConsumerNotFound", msg, err)
	}
	if _, err := consumers.Ensure(ctx, "ses.dotted", []string{"notifications.agent.ses.dotted"}, 0); !errors.Is(err, ErrNoSessionConsumerName) {
		t.Fatalf("Ensure of a dotted session id = %v, want ErrNoSessionConsumerName", err)
	}
}

// StreamID reads the identity the stream's owner gave it.
func TestSessionConsumersStreamID(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	info, err := owner.JS().StreamInfo(Stream)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	id, err := consumers.StreamID(context.Background())
	if err != nil {
		t.Fatalf("stream id: %v", err)
	}
	if want := info.Config.Metadata[StreamIDMetadataKey]; id == "" || id != want {
		t.Fatalf("StreamID = %q, want %q", id, want)
	}
}

// A NATS user whose grant predates session consumers may read a consumer's info but not create
// one: Ensure returns ErrConsumerNotGranted, at once rather than after a request timeout, so the
// listener refuses that registration 503 no_grant and writes nothing - no interest row, no
// session entry - rather than making the client wait out that timeout first.
func TestEnsureWithoutTheCreateGrantIsErrConsumerNotGranted(t *testing.T) {
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyGranted(t, public, "$JS.API.INFO", "$JS.API.STREAM.>", "$JS.API.CONSUMER.INFO.>", "_INBOX.>").URL
	t.Setenv("NATS_NKEY_SEED", seed)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	consumers, err := ConnectSessionConsumers([]string{uri}, 0)
	if err != nil {
		t.Fatalf("connect session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)

	started := time.Now()
	_, err = consumers.Ensure(context.Background(), "ses_ungranted", []string{contracts.AgentSubject("ses_ungranted")}, 0)
	if !errors.Is(err, ErrConsumerNotGranted) || !errors.Is(err, ErrPublishDenied) {
		t.Fatalf("Ensure without the create grant = %v, want ErrConsumerNotGranted", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Ensure took %s to report the refusal", elapsed)
	}
	started = time.Now()
	if _, err := consumers.ListSessionConsumers(context.Background()); !errors.Is(err, ErrConsumerNotGranted) {
		t.Fatalf("ListSessionConsumers without the list grant = %v, want ErrConsumerNotGranted", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("ListSessionConsumers took %s to report the refusal", elapsed)
	}
}

// The consumer list reads every session consumer across its pages, and a listed consumer that a
// restart would keep unchanged is kept with no call, at the start RestartAtFloor's keep returns;
// one with a message awaiting an ack, or whose filter no longer matches, is left to RestartAtFloor.
func TestListedConsumersAreKeptWhenARestartWouldChangeNothing(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const sessions = 300 // more than one page of 256
	ids := make([]string, sessions)
	var wg sync.WaitGroup
	turns := make(chan struct{}, 16)
	for i := range ids {
		ids[i] = fmt.Sprintf("ses_listed_%03d", i)
		turns <- struct{}{}
		wg.Go(func() {
			defer func() { <-turns }()
			if _, err := consumers.Ensure(ctx, ids[i], []string{contracts.AgentSubject(ids[i])}, 0); err != nil {
				t.Errorf("ensure %s: %v", ids[i], err)
			}
		})
	}
	wg.Wait()
	kept, held := ids[0], ids[1]
	written := publishStored(t, owner, contracts.AgentSubject(kept), "written")
	unacked := publishStored(t, owner, contracts.AgentSubject(held), "unacked")
	pull := pullSession(t, consumers, kept)
	if msg := next(t, pull); msg.StreamSeq != written {
		t.Fatalf("pulled %d, want %d", msg.StreamSeq, written)
	} else if err := msg.Ack(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	pull.Stop()
	waitForAckFloor(t, consumers, kept, written)
	heldPull := pullSession(t, consumers, held)
	if msg := next(t, heldPull); msg.StreamSeq != unacked {
		t.Fatalf("pulled %d, want %d", msg.StreamSeq, unacked)
	}
	heldPull.Stop()

	listed, err := consumers.ListSessionConsumers(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, id := range ids {
		if listed[id] == nil {
			t.Fatalf("the list holds no consumer for %s (%d listed)", id, len(listed))
		}
	}
	start, ok, err := consumers.KeepListed(ctx, kept, []string{contracts.AgentSubject(kept)}, listed[kept])
	if want := (SessionConsumerStart{StartSeq: written + 1, StreamID: streamIDOf(t, consumers)}); err != nil || !ok || start != want {
		t.Fatalf("KeepListed of an acked consumer = %+v, %t, %v; want %+v kept", start, ok, err, want)
	}
	if _, ok, err := consumers.KeepListed(ctx, held, []string{contracts.AgentSubject(held)}, listed[held]); err != nil || ok {
		t.Fatalf("KeepListed of a consumer with a message awaiting an ack = %t, %v; want it left to RestartAtFloor", ok, err)
	}
	if _, ok, err := consumers.KeepListed(ctx, kept, []string{contracts.AgentSubject(kept), "notifications.github.o.r.>"}, listed[kept]); err != nil || ok {
		t.Fatalf("KeepListed with a new filter = %t, %v; want it left to RestartAtFloor", ok, err)
	}
}

// Delete removes the session's consumer, so it holds a slot of the stream's consumer cap no
// longer, and answers the floor it recorded for it. A Delete of a consumer the stream no longer
// holds is no error, and nor is one of a session id that names no consumer, which costs no call
// at all; neither records a floor, so both answer 0.
func TestDeleteRemovesTheConsumerAndIsNoErrorWithoutOne(t *testing.T) {
	_, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_deleted"
	if _, err := consumers.Ensure(ctx, session, []string{contracts.AgentSubject(session)}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := consumers.Delete(ctx, session, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := consumers.Info(ctx, session); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer's info after the delete = %v, want ErrSessionConsumerNotFound", err)
	}
	if floor, err := consumers.Delete(ctx, session, nil); err != nil || floor != 0 {
		t.Fatalf("a second Delete of a consumer the stream no longer holds = %d, %v; want the floor 0 and no error", floor, err)
	}
	if floor, err := consumers.Delete(ctx, "ses.dotted", nil); err != nil || floor != 0 {
		t.Fatalf("Delete of a session id that names no consumer = %d, %v; want the floor 0 and no error", floor, err)
	}
}

// A restart whose create faulted leaves the consumer's pending start behind, and every later call
// would create the consumer there whatever it was asked for. Delete drops it, so the next
// registration that carries a resume point of its own creates the consumer at that point rather
// than at the start of a restart of a session that has since ended.
func TestDeleteAfterAFaultedRestartLeavesNoPendingStart(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_delete_pending"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	held := publishStored(t, owner, agent, "held")
	pull := pullSession(t, consumers, session)
	if msg := next(t, pull); msg.StreamSeq != held {
		t.Fatalf("first message = %d, want %d", msg.StreamSeq, held)
	}
	pull.Stop()
	waitForAckPending(t, consumers, session, 1)

	consumers.SetCreateFault(func(string) error { return errors.New("the create faulted") })
	if _, err := consumers.RestartAtFloor(ctx, session, []string{agent}); err == nil {
		t.Fatal("RestartAtFloor whose create faulted reported no error")
	}
	consumers.SetCreateFault(nil)
	if start, ok := consumers.pendingStart("s-" + session); !ok || start != held {
		t.Fatalf("the pending start after the faulted create = %d, %t; want %d", start, ok, held)
	}

	if _, err := consumers.Delete(ctx, session, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if start, ok := consumers.pendingStart("s-" + session); ok {
		t.Fatalf("the pending start after the delete = %d, want none: the next registration would undo the delete", start)
	}
	later := publishStored(t, owner, agent, "later")
	start, err := consumers.Ensure(ctx, session, []string{agent}, later+1)
	if err != nil || start.StartSeq != later+1 || !start.Created {
		t.Fatalf("Ensure carrying a resume point = %+v, %v; want the consumer created at %d", start, err, later+1)
	}
}

// The server removes a consumer idle longer than the inactive threshold its configuration
// carries, which is what frees the slot of a consumer no listener deleted. The threshold here is
// seconds rather than the hour a deployment runs with (SessionConsumerInactiveThreshold), so the
// row waits the server out.
//
// What this shows nothing about is a clustered hub: nats-server arms this timer in setLeader and
// returns from deleteNotActive while the consumer is not the leader, so every leader election
// restarts the clock, and a consumer on a replicated stream outlives its threshold many times
// over. The server here is one in-process member that elects a leader once.
func TestTheServerRemovesAConsumerIdlePastTheInactiveThreshold(t *testing.T) {
	_, consumers := sessionConsumersOnStream(t)
	consumers.inactiveThreshold = 2 * time.Second
	ctx := context.Background()
	const session = "ses_idle"
	if _, err := consumers.Ensure(ctx, session, []string{contracts.AgentSubject(session)}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Config.InactiveThreshold != 2*time.Second {
		t.Fatalf("inactive threshold = %s, want the 2s this row created the consumer with", info.Config.InactiveThreshold)
	}
	waitUntil(t, "the server to remove the idle consumer", func() (bool, string) {
		_, err := consumers.Info(context.Background(), session)
		if err != nil && !errors.Is(err, ErrSessionConsumerNotFound) {
			t.Fatalf("info: %v", err)
		}
		return errors.Is(err, ErrSessionConsumerNotFound), "the stream still holds it"
	})
}

// A session that comes back loses nothing to the delete: the floor recorded before it
// (SessionResumeSubject) is where the consumer is created again when the registration carries no
// resume point of its own, so the pull is delivered every message stored since that floor, each
// once, and none below it. Without the record the consumer would be created at the stream's end
// and the messages stored while the session was away would never reach it.
func TestAConsumerCreatedAgainStartsAtTheRecordedResumeFloor(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_resume"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	acked := publishStored(t, owner, agent, "acked")
	held := publishStored(t, owner, agent, "held")
	pull := pullSession(t, consumers, session)
	if msg := next(t, pull); msg.StreamSeq != acked {
		t.Fatalf("first message = %d, want %d", msg.StreamSeq, acked)
	} else if err := msg.Ack(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	pull.Stop()
	waitForAckFloor(t, consumers, session, acked)

	floor, err := consumers.Delete(ctx, session, nil)
	if err != nil || floor != acked {
		t.Fatalf("delete = %d, %v; want the floor %d it recorded", floor, err, acked)
	}
	away := publishStored(t, owner, agent, "stored while the session was away")
	start, err := consumers.Ensure(ctx, session, []string{agent}, 0)
	if err != nil || !start.Created || start.StartSeq != acked+1 {
		t.Fatalf("Ensure after the delete = %+v, %v; want the consumer created at %d, the sequence after the recorded floor", start, err, acked+1)
	}
	again := pullSession(t, consumers, session)
	for _, want := range []uint64{held, away} {
		msg := next(t, again)
		if msg.StreamSeq != want {
			t.Fatalf("after the delete got seq %d, want %d", msg.StreamSeq, want)
		}
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack %d: %v", msg.StreamSeq, err)
		}
	}
	requireNoMessage(t, again, 300*time.Millisecond)
}

// A resume record naming another stream's identity names sequences of a stream this one is not,
// so it is ignored and the consumer is created where a session with no record at all is created:
// at the first message this stream holds on the session's own agent subject, which is what hands
// a session what was stored for it before it could register (create, dispatch://INFRA-771), and
// at the stream's end only where it holds none.
func TestAResumeRecordOfAnotherStreamIsIgnored(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_other_stream"
	agent := contracts.AgentSubject(session)
	// Floor 7 is no sequence this fixture's start could be by accident: a trusted record would
	// start the consumer at 8, while the agent-subject start is the second sequence the stream
	// holds. With a floor of 1 the two were the same number, so the case passed whether the
	// record was ignored or trusted (dispatch://INFRA-771, the tester's reading of this round).
	record, err := json.Marshal(SessionResumeRecord{Floor: 7, StreamID: "a stream this is not"})
	if err != nil {
		t.Fatalf("marshal the resume record: %v", err)
	}
	publishStored(t, owner, SessionResumeSubject(session), string(record))
	stored := publishStored(t, owner, agent, "stored before the registration")
	start, err := consumers.Ensure(ctx, session, []string{agent}, 0)
	if err != nil || !start.Created || start.StartSeq != stored {
		t.Fatalf("Ensure with a resume record of another stream = %+v, %v; want the consumer created at %d, the first message the stream holds on the session's agent subject", start, err, stored)
	}
}

// With that record of another stream and nothing stored on the session's own agent subject, the
// consumer starts at the stream's end, as a session the stream holds nothing of does.
func TestAResumeRecordOfAnotherStreamWithNothingStoredStartsAtTheStreamsEnd(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_other_stream_empty"
	agent := contracts.AgentSubject(session)
	record, err := json.Marshal(SessionResumeRecord{Floor: 1, StreamID: "a stream this is not"})
	if err != nil {
		t.Fatalf("marshal the resume record: %v", err)
	}
	publishStored(t, owner, SessionResumeSubject(session), string(record))
	last := publishStored(t, owner, contracts.AgentSubject("ses_another_session"), "another session's message")
	start, err := consumers.Ensure(ctx, session, []string{agent}, 0)
	if err != nil || !start.Created || start.StartSeq != last+1 {
		t.Fatalf("Ensure with a resume record of another stream and nothing stored = %+v, %v; want the consumer created at %d, the stream's end", start, err, last+1)
	}
}

// The resume lane is the listener's own: no session may register a topic reaching it, by name or
// through a wildcard, so no replay hands out another session's resume floor; the stream carries
// the lane where the consumer list every filter takes does not, so no consumer delivers a record.
func TestTheResumeLaneIsTheListenersOwn(t *testing.T) {
	if subject := SessionResumeSubject("ses_abc"); subject != "notifications.envoy.resume.ses_abc" {
		t.Fatalf("SessionResumeSubject = %q", subject)
	}
	for _, topic := range []string{
		SessionResumeSubject("ses_a"),
		SessionResumeSubject("ses_b"),
		SessionResumeSubjectPrefix + "*",
		SessionResumeSubjectPrefix + ">",
	} {
		if !ReachesPrivateStreamSubject("ses_a", topic, nil) {
			t.Errorf("ReachesPrivateStreamSubject(%q, %q) = false, want true", "ses_a", topic)
		}
	}
	lane := SessionResumeSubjectPrefix + ">"
	if slices.Contains(StreamSubjects(), lane) {
		t.Errorf("StreamSubjects() = %q, want no %s among them: no consumer filter takes the lane", StreamSubjects(), lane)
	}
	if !slices.Contains(StreamCarriedSubjects(), lane) {
		t.Errorf("StreamCarriedSubjects() = %q, want %s among them", StreamCarriedSubjects(), lane)
	}
	if got := consumerFilterSubjects(StreamCarriedSubjects()); slices.Contains(got, lane) {
		t.Errorf("a consumer's filters over the stream's own subjects = %q, want no %s among them", got, lane)
	}
	if !StreamCaptures(SessionResumeSubject("ses_abc")) {
		t.Errorf("the stream does not capture %s, so no resume floor would survive a delete", SessionResumeSubject("ses_abc"))
	}
}

// A delete that is handed the consumer's info deletes from it, as the sweep does: it reads
// every session consumer in a few paged requests (ListSessionConsumers) and then deletes from
// what it read, where a delete handed nil reads the info itself. The floor it records, and the
// floor it answers, is the listed info's, so the session comes back where its delivery stopped
// either way.
func TestADeleteFromListedInfoRecordsAndAnswersThatInfosFloor(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_delete_listed"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	acked := publishStored(t, owner, agent, "acked")
	pull := pullSession(t, consumers, session)
	msg := next(t, pull)
	if msg.StreamSeq != acked {
		t.Fatalf("first message = %d, want %d", msg.StreamSeq, acked)
	}
	if err := msg.Ack(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	pull.Stop()
	waitForAckFloor(t, consumers, session, acked)

	listed, err := consumers.ListSessionConsumers(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if listed[session] == nil {
		t.Fatalf("the list holds no consumer for %s (%d listed)", session, len(listed))
	}
	floor, err := consumers.Delete(ctx, session, listed[session])
	if err != nil || floor != acked {
		t.Fatalf("Delete from the listed info = %d, %v; want the floor %d it recorded", floor, err, acked)
	}
	if _, err := consumers.Info(ctx, session); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer's info after the delete = %v, want ErrSessionConsumerNotFound", err)
	}
	recordedFloor, recorded, err := consumers.resumeFloor(ctx, session, streamIDOf(t, consumers))
	if err != nil || !recorded || recordedFloor != floor {
		t.Fatalf("the resume record after the delete = %d, %t, %v; want the floor %d the delete answered", recordedFloor, recorded, err, floor)
	}
}

// A delete that finds no consumer finds one a restart deleted and did not create again
// (RestartAtFloor), and the pending start that restart left is the only position anyone holds
// for the session. So the delete records the sequence before it on the resume lane before it
// drops that start: dropping it with nothing recorded would leave the session's position
// nowhere, and the next registration would create its consumer at the stream's end, past every
// message stored since the floor. The floor it answers is that same sequence, so the caller
// that gives the consumer back gives it back there. A session that never had a consumer has no
// position to record, and that delete is still no error, writes nothing and answers 0.
func TestADeleteThatFindsNoConsumerRecordsThePendingStartItDrops(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_delete_missing"
	agent := contracts.AgentSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	acked := publishStored(t, owner, agent, "acked")
	held := publishStored(t, owner, agent, "held")
	pull := pullSession(t, consumers, session)
	first := next(t, pull)
	if first.StreamSeq != acked {
		t.Fatalf("first message = %d, want %d", first.StreamSeq, acked)
	}
	if err := first.Ack(); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if msg := next(t, pull); msg.StreamSeq != held {
		t.Fatalf("second message = %d, want %d left unacked", msg.StreamSeq, held)
	}
	pull.Stop()
	waitForAckPending(t, consumers, session, 1)
	waitForAckFloor(t, consumers, session, acked)

	// A restart whose create faults leaves the consumer deleted and its start pending, which is
	// the state a listener killed between the two is in when the sweep reaches that session.
	consumers.SetCreateFault(func(string) error { return errors.New("the create faulted") })
	if _, err := consumers.RestartAtFloor(ctx, session, []string{agent}); err == nil {
		t.Fatal("RestartAtFloor whose create faulted reported no error")
	}
	consumers.SetCreateFault(nil)
	if start, ok := consumers.pendingStart("s-" + session); !ok || start != held {
		t.Fatalf("the pending start after the faulted create = %d, %t; want %d", start, ok, held)
	}
	if _, err := consumers.Info(ctx, session); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer's info after the faulted restart = %v, want it deleted", err)
	}

	deleted, err := consumers.Delete(ctx, session, nil)
	if err != nil || deleted != held-1 {
		t.Fatalf("a delete that found no consumer = %d, %v; want the floor %d it recorded, the sequence before the pending start it dropped", deleted, err, held-1)
	}
	floor, recorded, err := consumers.resumeFloor(ctx, session, streamIDOf(t, consumers))
	if err != nil || !recorded || floor != deleted {
		t.Fatalf("the resume record after a delete that found no consumer = %d, %t, %v; want the floor %d it answered", floor, recorded, err, deleted)
	}
	if start, ok := consumers.pendingStart("s-" + session); ok {
		t.Fatalf("the pending start after the delete = %d, want none: the next registration would undo the delete", start)
	}
	away := publishStored(t, owner, agent, "stored while the session was away")
	start, err := consumers.Ensure(ctx, session, []string{agent}, 0)
	if err != nil || !start.Created || start.StartSeq != held {
		t.Fatalf("Ensure after the delete = %+v, %v; want the consumer created at %d, the sequence after the recorded floor", start, err, held)
	}
	again := pullSession(t, consumers, session)
	for _, want := range []uint64{held, away} {
		msg := next(t, again)
		if msg.StreamSeq != want {
			t.Fatalf("after the delete got seq %d, want %d", msg.StreamSeq, want)
		}
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack %d: %v", msg.StreamSeq, err)
		}
	}
	requireNoMessage(t, again, 300*time.Millisecond)

	const never = "ses_never_had_a_consumer"
	if floor, err := consumers.Delete(ctx, never, nil); err != nil || floor != 0 {
		t.Fatalf("Delete of a session that never had a consumer = %d, %v; want the floor 0 and no error", floor, err)
	}
	if _, recorded, err := consumers.resumeFloor(ctx, never, streamIDOf(t, consumers)); err != nil || recorded {
		t.Fatalf("the resume lane of a session that never had a consumer holds a record = %t, %v; want none written", recorded, err)
	}
}

// The threshold a listener derives stands above the bound on when its sweep deletes a consumer,
// for every set of windows a deployment runs: the sweep records the consumer's resume floor
// before it deletes and the server's own removal records nothing, so a consumer the server takes
// first costs its session every message stored while it was away. The case that the 1 hour
// backstop alone fails is a session bucket whose MaxAge stands well above the gone horizon,
// since the sweep waits for the record to leave the bucket.
func TestInactiveThresholdForStandsAboveTheSweepsBound(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		recordLife, reapInterval, sweepInterval time.Duration
	}{
		{"the default windows", time.Hour, time.Minute, 5 * time.Minute},
		{"a session bucket aged well above the gone horizon", 24 * time.Hour, time.Minute, 5 * time.Minute},
		{"a raised horizon and slower passes", 6 * time.Hour, 10 * time.Minute, 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := InactiveThresholdFor(tc.recordLife, tc.reapInterval, tc.sweepInterval)
			bound := tc.recordLife + tc.reapInterval + tc.sweepInterval
			if got <= bound {
				t.Errorf("InactiveThresholdFor(%s, %s, %s) = %s, want it above the sweep's bound of %s: the server would remove the consumer before the sweep could record its floor",
					tc.recordLife, tc.reapInterval, tc.sweepInterval, got, bound)
			}
			if got < SessionConsumerInactiveThreshold {
				t.Errorf("InactiveThresholdFor(%s, %s, %s) = %s, want at least the floor %s",
					tc.recordLife, tc.reapInterval, tc.sweepInterval, got, SessionConsumerInactiveThreshold)
			}
		})
	}
}

// It is total: a window the caller passes as zero or negative counts as zero, so no
// configuration derives a threshold below the floor the constant sets.
func TestInactiveThresholdForIsNeverBelowItsFloor(t *testing.T) {
	if got := InactiveThresholdFor(0, 0, 0); got != SessionConsumerInactiveThreshold {
		t.Errorf("InactiveThresholdFor(0, 0, 0) = %s, want the floor %s", got, SessionConsumerInactiveThreshold)
	}
	for _, windows := range [][3]time.Duration{
		{-time.Hour, 0, 0},
		{0, -time.Minute, 0},
		{0, 0, -5 * time.Minute},
		{-time.Hour, -time.Minute, -5 * time.Minute},
		{time.Hour, -time.Minute, -5 * time.Minute},
	} {
		if got := InactiveThresholdFor(windows[0], windows[1], windows[2]); got < SessionConsumerInactiveThreshold {
			t.Errorf("InactiveThresholdFor(%s, %s, %s) = %s, want at least the floor %s",
				windows[0], windows[1], windows[2], got, SessionConsumerInactiveThreshold)
		}
	}
}

// A consumer carries the inactive threshold its connection was opened with
// (ConnectSessionConsumers's inactiveThreshold), which is how a listener gives every consumer
// the one its own windows derive (InactiveThresholdFor), and the constant where that threshold
// is zero or negative, so no configuration turns the server's backstop off. The threshold
// is given at the connection and never set after it, so no consumer is created before the
// deployment's threshold is in hand.
func TestAConsumerCarriesTheInactiveThresholdItsHandleWasOpenedWith(t *testing.T) {
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	const given = 2 * time.Hour
	for _, tc := range []struct {
		name      string
		session   string
		threshold time.Duration
		want      time.Duration
	}{
		{"opened with a threshold", "ses_threshold_given", given, given},
		{"opened with none", "ses_threshold_none", 0, SessionConsumerInactiveThreshold},
		{"opened with a negative threshold", "ses_threshold_negative", -time.Minute, SessionConsumerInactiveThreshold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consumers, err := ConnectSessionConsumers([]string{uri}, tc.threshold)
			if err != nil {
				t.Fatalf("connect session consumers: %v", err)
			}
			t.Cleanup(consumers.Close)
			ctx := context.Background()
			if _, err := consumers.Ensure(ctx, tc.session, []string{contracts.AgentSubject(tc.session)}, 0); err != nil {
				t.Fatalf("ensure: %v", err)
			}
			info, err := consumers.Info(ctx, tc.session)
			if err != nil {
				t.Fatalf("info: %v", err)
			}
			if info.Config.InactiveThreshold != tc.want {
				t.Fatalf("the consumer's inactive threshold = %s, want the %s its handle was opened with", info.Config.InactiveThreshold, tc.want)
			}
		})
	}
}

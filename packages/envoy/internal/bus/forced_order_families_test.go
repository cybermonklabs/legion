//go:build odatrace

package bus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The forced-order rows of the plan's families 2 to 6 and 8 for the session family, on the hooked
// server (scripts/forced-order.sh), each against the build under test and, through
// forced_order_mode_test.go's swap, the build before it. A race regression fails at the build
// before this one and passes at this one; a compatibility row passes at both. Each row runs its
// control first: a check that the interruption, the loss or the full cap it is about really happened.

// selectionBucket and selectionSentinel are the session consumers' selection bucket and its
// sentinel key (consumernames.SessionBucket, consumernames.SentinelKey), named here so the rows
// build against the build before them, which has no such package.
const (
	selectionBucket   = "envoy_session_consumers"
	selectionSentinel = "envoy.bucket"
)

// pendingSession is a session whose consumer delivered every one of its forcedMessages and has the
// first forcedAcked acknowledged, the rest awaiting an ack.
type pendingSession struct {
	id, agent string
	seqs      []uint64
	start     SessionConsumerStart
}

func (r *forcedRig) pendingSession(t *testing.T, id string) *pendingSession {
	t.Helper()
	ctx := context.Background()
	s := &pendingSession{id: id, agent: contracts.LegacyAgentSubject(id)}
	s.seqs = r.publish(t, s.agent, forcedMessages)
	start, err := r.consumers.Ensure(ctx, id, []string{s.agent}, s.seqs[0])
	if err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	s.start = start
	pull, err := forcedPull(ctx, r.consumers, start, id)
	if err != nil {
		t.Fatalf("pull %s: %v", id, err)
	}
	var msgs []SessionMessage
	for range forcedMessages {
		msgs = append(msgs, next(t, pull))
	}
	for _, msg := range msgs[:forcedAcked] {
		if err := msg.Ack(); err != nil {
			t.Fatal(err)
		}
	}
	pull.Stop()
	waitForAckPending(t, r.consumers, id, forcedMessages-forcedAcked)
	return s
}

// removeFromOutside deletes consumer name as the server's inactivity removal would, and confirms it
// is gone: every row that removes a consumer reads its control here.
func (r *forcedRig) removeFromOutside(t *testing.T, name string) {
	t.Helper()
	js, err := r.conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := js.DeleteConsumer(Stream, name); err != nil {
		t.Fatalf("remove %s from outside: %v", name, err)
	}
	if _, err := js.ConsumerInfo(Stream, name); !errors.Is(err, natsgo.ErrConsumerNotFound) {
		t.Fatalf("control: %s read %v after its removal, want no consumer", name, err)
	}
}

// streamConsumerNames is every consumer name of the stream.
func (r *forcedRig) streamConsumerNames(t *testing.T) []string {
	t.Helper()
	js, err := r.conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range js.ConsumerNames(Stream) {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// requireDeliveredFrom fails t unless got holds every one of want, and nothing the stream did not
// store for the session (stored): a start before want's first message delivers some again, which a
// row whose rule may start at 1 allows, and losing one is what it refuses.
func requireDeliveredFrom(t *testing.T, got, want, stored []uint64, what string) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: the expected message set is empty, so the row judges nothing", what)
	}
	for _, seq := range want {
		if !slices.Contains(got, seq) {
			t.Fatalf("%s delivered %v; want every one of %v", what, got, want)
		}
	}
	for _, seq := range got {
		if !slices.Contains(stored, seq) {
			t.Fatalf("%s delivered %d, which is no message of the session: %v", what, seq, got)
		}
	}
}

// forcedSpy records the consumer names the server is asked to create on the stream, and its stream
// updates, through a subscription of the test's own on the account's API subjects.
type forcedSpy struct {
	mu      sync.Mutex
	creates []string
	updates int
}

func (r *forcedRig) spy(t *testing.T) (*forcedSpy, *natsgo.Conn) {
	t.Helper()
	conn := testnats.Connect(t, r.URL)
	t.Cleanup(conn.Close)
	spy := &forcedSpy{}
	sub, err := conn.Subscribe("$JS.API.>", func(msg *natsgo.Msg) {
		tokens := strings.Split(msg.Subject, ".")
		spy.mu.Lock()
		defer spy.mu.Unlock()
		switch {
		case len(tokens) >= 6 && tokens[2] == "CONSUMER" && tokens[3] == "CREATE" && tokens[4] == Stream:
			spy.creates = append(spy.creates, tokens[5])
		case len(tokens) >= 7 && tokens[2] == "CONSUMER" && tokens[3] == "DURABLE" && tokens[4] == "CREATE" && tokens[5] == Stream:
			spy.creates = append(spy.creates, tokens[6])
		case len(tokens) == 5 && tokens[2] == "STREAM" && tokens[3] == "UPDATE" && tokens[4] == Stream:
			spy.updates++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	return spy, conn
}

// settled is what the spy heard once the server answered a request sent after every one it saw.
func (s *forcedSpy) settled(t *testing.T, conn *natsgo.Conn) ([]string, int) {
	t.Helper()
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.creates), s.updates
}

func createdTwice(names []string) string {
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			return name
		}
		seen[name] = true
	}
	return ""
}

// Family 2. A relay restart killed after it chose the replacement's start and before its create
// left the session no consumer to deliver from, or its record an open replace: another process's
// registration must recover it, and every message after the floor arrive.
func TestForcedOrderARestartKilledBeforeItsCreateDeliversEveryMessageAfterRecoveryInAnotherProcess(t *testing.T) {
	forEachTopology(t, aRestartKilledBeforeItsCreate)
}

func aRestartKilledBeforeItsCreate(t *testing.T, topo forcedTopology) {
	r := newForcedRig(t, topo)
	s := r.pendingSession(t, "ses_forced_killed_restart")
	killed := errors.New("the process was killed before its create")
	var fired atomic.Int32
	r.consumers.SetCreateFault(func(string) error {
		fired.Add(1)
		return killed
	})
	if _, err := r.consumers.RestartAtFloor(context.Background(), s.id, []string{s.agent}); !errors.Is(err, killed) {
		t.Fatalf("the restart = %v, want the kill", err)
	}
	// Control: the kill reached the create.
	if fired.Load() == 0 {
		t.Fatal("control: the kill never reached a create, so the row interrupts nothing")
	}
	r.consumers.Close()
	other := forcedProcess(t, r.urls)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	recovered, err := other.Ensure(ctx, s.id, []string{s.agent}, 0)
	if err != nil {
		t.Fatalf("the other process's registration: %v", err)
	}
	got := delivered(t, s.seqs[forcedAcked:], func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, other, recovered, s.id) })
	requireDeliveredFrom(t, got, s.seqs[forcedAcked:], s.seqs, "the consumer the other process recovered")
}

// Family 2. A creator paused inside its create while another process registers the same session:
// the other process's answer stands, and when the paused creator finishes no name has been created
// twice, and every message arrives from the one consumer the session keeps.
func TestForcedOrderAPausedCreatorThatFinishesAfterAnotherProcessRegisteredCreatesNoNameTwice(t *testing.T) {
	forEachTopology(t, aPausedCreator)
}

func aPausedCreator(t *testing.T, topo forcedTopology) {
	r := newForcedRig(t, topo)
	spy, spyConn := r.spy(t)
	id := "ses_forced_paused_creator"
	agent := contracts.LegacyAgentSubject(id)
	seqs := r.publish(t, agent, forcedMessages)
	reached, release := make(chan string, 1), make(chan struct{})
	r.consumers.SetCreateFault(func(name string) error {
		reached <- name
		<-release
		return nil
	})
	first := make(chan error, 1)
	go func() {
		_, err := r.consumers.Ensure(context.Background(), id, []string{agent}, seqs[0])
		first <- err
	}()
	// Control: the first creator is inside its create.
	select {
	case name := <-reached:
		t.Logf("the first creator is paused in its create of %s", name)
	case <-time.After(30 * time.Second):
		t.Fatal("control: the first creator never reached its create")
	}
	other := forcedProcess(t, r.urls)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	registered, err := other.Ensure(ctx, id, []string{agent}, seqs[0])
	if err != nil {
		t.Fatalf("the other process's registration: %v", err)
	}
	close(release)
	select {
	case err := <-first:
		t.Logf("the paused creator answered %v once released", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the paused creator did not answer within 30 s of its release")
	}
	forcedCleanup(t, other, id)
	creates, _ := spy.settled(t, spyConn)
	if twice := createdTwice(creates); twice != "" {
		t.Fatalf("%s was sent to the server's create twice: %v", twice, creates)
	}
	if names := r.streamConsumerNames(t); len(names) != 1 || names[0] != forcedName(registered, id) {
		t.Fatalf("the stream holds %v, want the one consumer %s", names, forcedName(registered, id))
	}
	got := delivered(t, seqs, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, other, registered, id) })
	requireDelivered(t, got, seqs, "the session's one consumer")
}

// Family 3. A replacement removed right after its publish, before any floor of its own was read:
// the record's saved start governs, and every message after the predecessor's floor arrives.
func TestForcedOrderAReplacementLostRightAfterItsPublishStartsAtTheSavedStart(t *testing.T) {
	forEachTopology(t, aReplacementLostRightAfterItsPublish)
}

func aReplacementLostRightAfterItsPublish(t *testing.T, topo forcedTopology) {
	r := newForcedRig(t, topo)
	s := r.pendingSession(t, "ses_forced_lost_after_publish")
	restarted, err := r.consumers.RestartAtFloor(context.Background(), s.id, []string{s.agent})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	r.removeFromOutside(t, forcedName(restarted, s.id))
	other := forcedProcess(t, r.urls)
	again, err := other.Ensure(context.Background(), s.id, []string{s.agent}, 0)
	if err != nil {
		t.Fatalf("the other process's registration: %v", err)
	}
	got := delivered(t, s.seqs[forcedAcked:], func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, other, again, s.id) })
	requireDeliveredFrom(t, got, s.seqs[forcedAcked:], s.seqs, "the consumer created again after the loss")
}

// Family 4. A delete (a deregistration's, the sweep's, the reaper's) of a session whose consumer the
// server already removed has no floor to read: it must not record one of 0, or none, and the next
// registration must start no later than the first message the session has not acknowledged.
func TestForcedOrderADeleteOfAConsumerAlreadyGoneKeepsEveryUnacknowledgedMessage(t *testing.T) {
	forEachTopology(t, aDeleteOfAConsumerAlreadyGone)
}

func aDeleteOfAConsumerAlreadyGone(t *testing.T, topo forcedTopology) {
	r := newForcedRig(t, topo)
	s := r.pendingSession(t, "ses_forced_delete_absent")
	r.removeFromOutside(t, forcedName(s.start, s.id))
	floor, err := r.consumers.Delete(context.Background(), s.id, nil)
	t.Logf("the delete of the absent consumer answered floor %d, %v", floor, err)
	other := forcedProcess(t, r.urls)
	again, err := other.Ensure(context.Background(), s.id, []string{s.agent}, 0)
	if err != nil {
		t.Fatalf("the next registration: %v", err)
	}
	got := delivered(t, s.seqs[forcedAcked:], func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, other, again, s.id) })
	requireDeliveredFrom(t, got, s.seqs[forcedAcked:], s.seqs, "the consumer registered after the delete")
}

// Family 6. Lost selection state refuses to create: with the bucket deleted, one key deleted, or the
// bucket recreated empty with no sentinel, a registration whose consumer is gone creates nothing
// and runs no first-create rule. The control is a registration on the same process before the
// loss, which creates its consumer.
func TestForcedOrderLostSelectionStateRefusesToCreate(t *testing.T) {
	forEachTopology(t, lostSelectionStateRefusesToCreate)
}

func lostSelectionStateRefusesToCreate(t *testing.T, topo forcedTopology) {
	for name, lose := range map[string]func(t *testing.T, kv jetstream.JetStream, sessionID string){
		"the bucket deleted": func(t *testing.T, js jetstream.JetStream, _ string) {
			if err := js.DeleteKeyValue(context.Background(), selectionBucket); err != nil {
				t.Fatal(err)
			}
		},
		"the key deleted": func(t *testing.T, js jetstream.JetStream, sessionID string) {
			kv, err := js.KeyValue(context.Background(), selectionBucket)
			if err != nil {
				t.Fatal(err)
			}
			if err := kv.Delete(context.Background(), sessionID); err != nil {
				t.Fatal(err)
			}
		},
		"the bucket recreated empty with no sentinel": func(t *testing.T, js jetstream.JetStream, _ string) {
			if err := js.DeleteKeyValue(context.Background(), selectionBucket); err != nil {
				t.Fatal(err)
			}
			if _, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: selectionBucket, History: 1, Storage: jetstream.FileStorage}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newForcedRig(t, topo)
			ctx := context.Background()
			id := "ses_forced_lost_state"
			agent := contracts.LegacyAgentSubject(id)
			seqs := r.publish(t, agent, 3)
			// Control: with the state intact the registration creates the session's consumer.
			start, err := r.consumers.Ensure(ctx, id, []string{agent}, seqs[0])
			if err != nil {
				t.Fatalf("control: the registration before the loss: %v", err)
			}
			js, err := jetstream.New(r.conn)
			if err != nil {
				t.Fatal(err)
			}
			lose(t, js, id)
			r.removeFromOutside(t, forcedName(start, id))
			other := forcedProcess(t, r.urls)
			_, err = other.Ensure(ctx, id, []string{agent}, 0)
			if err == nil || !forcedLostState(err) {
				t.Fatalf("the registration after the loss = %v, want a refusal for lost selection state", err)
			}
			if names := r.streamConsumerNames(t); len(names) != 0 {
				t.Fatalf("the stream holds %v after the refusal, want no consumer", names)
			}
		})
	}
}

// Family 5. A full consumer cap, with no cap update: a relay restart, a deregistration followed at
// once by a registration, and a filter change each answer, and the pending messages arrive, while
// no request updates the stream. The control is a raw create on the full cap, which the server
// refuses.
func TestForcedOrderAFullCapServesEverySessionCallWithNoCapUpdate(t *testing.T) {
	forEachTopology(t, aFullCap)
}

func aFullCap(t *testing.T, topo forcedTopology) {
	// The keeper's period is an hour, so the cap lowered below stays lowered for the row.
	r := newForcedRig(t, topo, WithKeepInterval(time.Hour))
	ctx := context.Background()
	s := r.pendingSession(t, "ses_forced_full_cap")
	js, err := r.conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatal(err)
	}
	capped := info.Config
	capped.MaxConsumers = info.State.Consumers + 2
	if _, err := js.UpdateStream(&capped); err != nil {
		t.Fatalf("lower the consumer cap to %d: %v", capped.MaxConsumers, err)
	}
	filler := func(i int) *natsgo.ConsumerConfig {
		return &natsgo.ConsumerConfig{Durable: fmt.Sprintf("s-ses_forced_filler_%d", i), FilterSubject: contracts.LegacyAgentSubject(fmt.Sprintf("ses_forced_filler_%d", i)), AckPolicy: natsgo.AckExplicitPolicy}
	}
	filled := 0
	for ; filled < capped.MaxConsumers; filled++ {
		if _, err := js.AddConsumer(Stream, filler(filled)); err != nil {
			break
		}
	}
	// Control: the cap is full.
	var api *natsgo.APIError
	if _, err := js.AddConsumer(Stream, filler(filled+1)); !errors.As(err, &api) || api.ErrorCode != natsgo.ErrorCode(jetstream.JSErrCodeMaximumConsumersLimit) {
		t.Fatalf("control: a create on the full cap answered %v, want the server's maximum consumers refusal", err)
	}
	spy, spyConn := r.spy(t)

	restarted, err := r.consumers.RestartAtFloor(ctx, s.id, []string{s.agent})
	if err != nil {
		t.Fatalf("the restart at a full cap: %v", err)
	}
	got := delivered(t, s.seqs[forcedAcked:], func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, restarted, s.id) })
	requireDelivered(t, got, s.seqs[forcedAcked:], "the consumer restarted at a full cap")

	if _, err := r.consumers.Delete(ctx, s.id, nil); err != nil {
		t.Fatalf("the deregistration at a full cap: %v", err)
	}
	later := r.publish(t, s.agent, 3)
	again, err := r.consumers.Ensure(ctx, s.id, []string{s.agent}, 0)
	if err != nil {
		t.Fatalf("the registration right after the deregistration at a full cap: %v", err)
	}
	got = delivered(t, later, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, again, s.id) })
	requireDelivered(t, got, later, "the consumer registered again at a full cap")

	if err := r.consumers.UpdateFilter(ctx, s.id, []string{s.agent, "notifications.team.forced-full-cap"}); err != nil {
		t.Fatalf("the filter change at a full cap: %v", err)
	}
	if _, updates := spy.settled(t, spyConn); updates != 0 {
		t.Fatalf("%d stream updates were sent at a full cap, want none", updates)
	}
}

// Family 8. Out-of-order acks with an earlier message still pending: a restart starts after the
// floor, which the pending earlier message holds back, so it arrives again with the later ones.
func TestForcedOrderOutOfOrderAcksKeepTheEarlierPendingMessage(t *testing.T) {
	forEachTopology(t, outOfOrderAcks)
}

func outOfOrderAcks(t *testing.T, topo forcedTopology) {
	r := newForcedRig(t, topo)
	ctx := context.Background()
	id := "ses_forced_out_of_order"
	agent := contracts.LegacyAgentSubject(id)
	seqs := r.publish(t, agent, 3)
	start, err := r.consumers.Ensure(ctx, id, []string{agent}, seqs[0])
	if err != nil {
		t.Fatal(err)
	}
	pull, err := forcedPull(ctx, r.consumers, start, id)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []SessionMessage{next(t, pull), next(t, pull), next(t, pull)}
	for _, msg := range msgs[1:] {
		if err := msg.Ack(); err != nil {
			t.Fatal(err)
		}
	}
	pull.Stop()
	// Control: the first message is still awaiting its ack.
	waitForAckPending(t, r.consumers, id, 1)
	restarted, err := r.consumers.RestartAtFloor(ctx, id, []string{agent})
	if err != nil {
		t.Fatal(err)
	}
	got := delivered(t, seqs, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, restarted, id) })
	requireDelivered(t, got, seqs, "the restarted consumer")
}

// Family 8. A saved start that retention has passed is raised to the stream's first sequence, as
// today: the registration after a deregistration delivers what the stream still holds.
func TestForcedOrderASavedStartRetentionPassedStartsAtTheStreamsFirstSequence(t *testing.T) {
	forEachTopology(t, aSavedStartRetentionPassed)
}

func aSavedStartRetentionPassed(t *testing.T, topo forcedTopology) {
	r := newForcedRig(t, topo)
	ctx := context.Background()
	id := "ses_forced_retention"
	agent := contracts.LegacyAgentSubject(id)
	first := r.publish(t, agent, 3)
	start, err := r.consumers.Ensure(ctx, id, []string{agent}, first[0])
	if err != nil {
		t.Fatal(err)
	}
	got := delivered(t, first, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, start, id) })
	requireDelivered(t, got, first, "the first consumer")
	if _, err := r.consumers.Delete(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	later := r.publish(t, agent, 4)
	js, err := r.conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := js.PurgeStream(Stream, &natsgo.StreamPurgeRequest{Sequence: later[2]}); err != nil {
		t.Fatal(err)
	}
	// Control: the stream no longer holds the first messages after the deregistration's floor.
	if info, err := js.StreamInfo(Stream); err != nil || info.State.FirstSeq != later[2] {
		t.Fatalf("control: the stream's first sequence after the purge is %v (%v), want %d", info, err, later[2])
	}
	again, err := r.consumers.Ensure(ctx, id, []string{agent}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got = delivered(t, later[2:], func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, again, id) })
	requireDelivered(t, got, later[2:], "the consumer registered after retention passed its start")
}

// Family 8. Long ids: 253 bytes are accepted, 254 refused with the server's 10102 answer as today,
// and two 253-byte ids that share their first 250 bytes keep consumers and messages of their own.
func TestForcedOrderLongSessionIDsAreKeptApartAndRefusedAsToday(t *testing.T) {
	forEachTopology(t, longSessionIDs)
}

func longSessionIDs(t *testing.T, topo forcedTopology) {
	r := newForcedRig(t, topo)
	ctx := context.Background()
	long := strings.Repeat("a", 254)
	if _, err := r.consumers.Ensure(ctx, long, []string{contracts.LegacyAgentSubject(long)}, 0); err == nil || !strings.Contains(err.Error(), "consumer name is too long, maximum allowed is 255") {
		t.Fatalf("a 254-byte id: %v, want the server's 10102 answer", err)
	}
	ids := []string{strings.Repeat("a", 250) + "bbb", strings.Repeat("a", 250) + "ccc"}
	names := map[string]bool{}
	for _, id := range ids {
		agent := contracts.LegacyAgentSubject(id)
		seqs := r.publish(t, agent, 2)
		start, err := r.consumers.Ensure(ctx, id, []string{agent}, seqs[0])
		if err != nil {
			t.Fatalf("a 253-byte id: %v", err)
		}
		names[forcedName(start, id)] = true
		got := delivered(t, seqs, func(ctx context.Context) (*SessionPull, error) { return forcedPull(ctx, r.consumers, start, id) })
		requireDelivered(t, got, seqs, "a 253-byte id's consumer")
	}
	if len(names) != 2 {
		t.Fatalf("two 253-byte ids share a consumer: %v", names)
	}
}

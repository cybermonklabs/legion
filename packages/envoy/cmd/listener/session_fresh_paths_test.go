package main

import (
	"context"
	"io"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// freshSessionRecord reads sessionID's selection record through admin, with its revision.
func freshSessionRecord(t *testing.T, admin *natsgo.Conn, sessionID string) (consumernames.Record, uint64) {
	t.Helper()
	record, revision, err := consumernames.NewKVStore(consumernames.ConnRequester{Conn: admin}).Read(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the selection record of %s: %v", sessionID, err)
	}
	return record, revision
}

// registerFresh registers sessionID on a fresh delivery and answers its record's current.
func registerFresh(t *testing.T, admin *natsgo.Conn, register func(string) (*deliveryRefusal, sessionDeliveryOutcome), sessionID string) string {
	t.Helper()
	if refusal, _ := register(sessionID); refusal != nil {
		t.Fatalf("register %s in the fresh mode: refused %+v", sessionID, refusal)
	}
	record, _ := freshSessionRecord(t, admin, sessionID)
	if record.Current == "" {
		t.Fatalf("the record of %s names no current after its registration", sessionID)
	}
	return record.Current
}

// The relay's last floor is the sequence before the restart's start, so a current the server then
// removes is created again at the first message the session has not acknowledged. Here the
// session acknowledges its first of three messages, the relay's restart keeps the current at the
// second and writes the last floor (sessionRelays.recordLastFloor), the server loses the current,
// and the next registration creates a fresh one whose first delivery is the second message. The
// review's planted fault, StartSeq in place of StartSeq-1, skips it, and before this row left every
// test of the package green: the listener's rigs run the fixed mode, where the last floor is not
// written.
func TestARelayRestartThenARemovedCurrentResumesAtTheFirstUnackedMessage(t *testing.T) {
	uri, admin := selectionHub(t)
	delivery, register := freshDelivery(t, uri)
	ctx := context.Background()
	const sessionID = "ses_fresh_relay_floor"
	subject := contracts.LegacyAgentSubject(sessionID)
	first := registerFresh(t, admin, register, sessionID)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	for _, body := range []string{"one", "two", "three"} {
		ack, err := js.Publish(ctx, subject, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, ack.Sequence)
	}
	pullCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pull, err := delivery.consumers.Pull(pullCtx, sessionID, first)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := pull.Next(pullCtx)
	if err != nil || msg.StreamSeq != seqs[0] {
		t.Fatalf("the first delivery = seq %d, %v; want %d", msg.StreamSeq, err, seqs[0])
	}
	if err := msg.Ack(); err != nil {
		t.Fatal(err)
	}
	if _, err := pull.Release(); err != nil {
		t.Fatal(err)
	}
	var start bus.SessionConsumerStart
	waitFor(t, 30*time.Second, "the relay's restart to keep the current at the second message", func() bool {
		start, err = delivery.consumers.RestartAtFloor(ctx, sessionID, []string{subject})
		return err == nil && !start.Created && start.StartSeq == seqs[1]
	})
	relays := &sessionRelays{delivery: delivery, logger: logging.NewWithWriter("test", io.Discard)}
	relays.recordLastFloor(ctx, sessionID, start)
	if err := js.DeleteConsumer(ctx, bus.Stream, start.Name); err != nil {
		t.Fatal(err)
	}
	again := registerFresh(t, admin, register, sessionID)
	if again == first {
		t.Fatalf("the registration after the loss kept %s, which the server no longer holds", first)
	}
	pull, err = delivery.consumers.Pull(pullCtx, sessionID, again)
	if err != nil {
		t.Fatal(err)
	}
	defer pull.Stop()
	if msg, err := pull.Next(pullCtx); err != nil || msg.StreamSeq != seqs[1] {
		t.Fatalf("the fresh current's first delivery = seq %d, %v; want %d, the first message the session had not acknowledged", msg.StreamSeq, err, seqs[1])
	}
}

// liveSessions is a sweep's registry over which every session is live and the copy is trusted.
type liveSessions struct{}

func (liveSessions) CacheReady() bool { return true }
func (liveSessions) Servable() error  { return nil }
func (liveSessions) NeedsRead() bool  { return false }
func (liveSessions) Lookup(string) (session.SessionEntry, session.State) {
	return session.SessionEntry{}, session.Live
}

// The fresh sweep deletes a live session's retired names and keeps its current: it asks the
// session's record (bus.SessionConsumers.CleanRetired, which deletes only what the record retired),
// never deletes a session consumer by name. Here a live session holds its current and a retired
// name made after it with the session's own metadata; one pass deletes the retired name, drops it
// from the record, and leaves the current. The review's planted fault, Delete in place of
// CleanRetired, removes the current of every live session, and before this row left every test of
// the package green: all six sweep rows run the fixed mode.
func TestTheFreshSweepDeletesALiveSessionsRetiredNameAndKeepsItsCurrent(t *testing.T) {
	uri, admin := selectionHub(t)
	delivery, register := freshDelivery(t, uri)
	ctx := context.Background()
	const sessionID = "ses_fresh_sweep_live"
	current := registerFresh(t, admin, register, sessionID)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := js.Consumer(ctx, bus.Stream, current)
	if err != nil {
		t.Fatal(err)
	}
	config := consumer.CachedInfo().Config
	config.Name = consumernames.FreshName(consumernames.SessionPrefix)
	config.Durable = config.Name
	if _, err := js.CreateConsumer(ctx, bus.Stream, config); err != nil {
		t.Fatal(err)
	}
	record, revision := freshSessionRecord(t, admin, sessionID)
	record.Retired = append(record.Retired, config.Name)
	if _, err := consumernames.NewKVStore(consumernames.ConnRequester{Conn: admin}).Write(ctx, sessionID, record, revision); err != nil {
		t.Fatal(err)
	}
	relays := &sessionRelays{}
	relays.owned.Store(true)
	sweep := &sessionConsumerSweep{delivery: delivery, sessions: liveSessions{}, relays: relays, logger: logging.NewWithWriter("test", io.Discard)}
	sweep.pass(ctx)
	if held := consumersOf(t, admin, sessionID); !slices.Equal(held, []string{current}) {
		t.Fatalf("after the sweep's pass the stream holds %v of %s; want its current %s alone", held, sessionID, current)
	}
	if after, _ := freshSessionRecord(t, admin, sessionID); after.Current != current || len(after.Retired) != 0 {
		t.Fatalf("after the sweep's pass the record is %+v; want %s current and no retired name", after, current)
	}
}

// An edge listener's grant writes no sentinel of the selection bucket (natsgrant.Edge), so its
// sweep leaves ending replay mode, which rewrites the sentinel, to the shared listeners' sweeps:
// its pass over a bucket in replay mode whose orphans are all gone keeps the sentinel as it was,
// and sends no write the server would refuse it at every pass. The control is a shared listener's
// pass over the same bucket, which ends replay mode.
func TestAnEdgeListenersSweepLeavesReplayModeToTheSharedListeners(t *testing.T) {
	for _, edge := range []bool{true, false} {
		t.Run(map[bool]string{true: "an edge listener's pass", false: "control: a shared listener's pass"}[edge], func(t *testing.T) {
			_, uri := testnats.Start(t)
			admin := testnats.Connect(t, uri)
			t.Cleanup(admin.Close)
			ctx := context.Background()
			if _, err := consumernames.CreateSessionBucket(ctx, admin, 1, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1, ReplayFrom: 4}); err != nil {
				t.Fatal(err)
			}
			delivery, _ := freshDelivery(t, uri)
			relays := &sessionRelays{}
			relays.owned.Store(true)
			sweep := &sessionConsumerSweep{delivery: delivery, sessions: liveSessions{}, relays: relays, edge: edge, logger: logging.NewWithWriter("test", io.Discard)}
			sweep.pass(ctx)
			state, err := consumernames.NewKVStore(consumernames.ConnRequester{Conn: admin}).State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[bool]uint64{true: 4, false: 0}[edge]; state.ReplayFrom != want {
				t.Fatalf("after the pass the sentinel's replay_from is %d, want %d", state.ReplayFrom, want)
			}
		})
	}
}

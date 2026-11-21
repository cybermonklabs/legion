package bus

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// freshSessionConsumersOnStream is the shared server's stream in the test's own account, the
// selection bucket with its sentinel saying the cutover is done (consumernames.CreateSessionBucket,
// as the operator's create-bucket writes it), and SessionConsumers in the fresh mode beside it.
func freshSessionConsumersOnStream(t *testing.T) (*Client, *SessionConsumers, *consumernames.KVStore) {
	t.Helper()
	owner, consumers, conn := freshSessionConsumersWithoutBucket(t)
	store, err := consumernames.CreateSessionBucket(context.Background(), conn, 1, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1})
	if err != nil {
		t.Fatalf("create the selection bucket: %v", err)
	}
	return owner, consumers, store
}

// freshSessionConsumersWithoutBucket is freshSessionConsumersOnStream with no selection bucket, and
// a plain connection of the test's own to the server.
func freshSessionConsumersWithoutBucket(t *testing.T) (*Client, *SessionConsumers, *natsgo.Conn) {
	t.Helper()
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	consumers, err := ConnectSessionConsumers([]string{uri}, 0, nil, WithConsumerNames(consumernames.ModeFresh))
	if err != nil {
		t.Fatalf("connect session consumers in the fresh mode: %v", err)
	}
	t.Cleanup(consumers.Close)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	return owner, consumers, conn
}

// streamConsumers names every consumer of the stream.
func streamConsumers(t *testing.T, owner *Client) []string {
	t.Helper()
	var names []string
	for name := range owner.JS().ConsumerNames(Stream) {
		names = append(names, name)
	}
	return names
}

// selectionRecord reads sessionID's selection record.
func selectionRecord(t *testing.T, store *consumernames.KVStore, sessionID string) consumernames.Record {
	t.Helper()
	record, _, err := store.Read(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the selection record of %s: %v", sessionID, err)
	}
	return record
}

// The fixed mode is the default: it creates today's s-<session id> and reads no selection bucket,
// which the server here does not hold.
func TestTheFixedModeIsTheDefaultAndReadsNoSelectionBucket(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	if consumers.Fresh() {
		t.Fatal("SessionConsumers connected with no mode runs the fresh mode")
	}
	start, err := consumers.Ensure(context.Background(), "ses_fixed_default", []string{contracts.LegacyAgentSubject("ses_fixed_default")}, 0)
	if err != nil {
		t.Fatalf("Ensure in the fixed mode with no selection bucket: %v", err)
	}
	if start.Name != "s-ses_fixed_default" || !start.Created {
		t.Fatalf("Ensure = %+v, want s-ses_fixed_default created", start)
	}
	if _, err := owner.JS().StreamInfo("KV_" + consumernames.SessionBucket); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("the selection bucket's stream: %v, want none", err)
	}
	if err := consumers.CheckSelection(context.Background()); err != nil || consumers.SelectionHealth(context.Background()) != nil {
		t.Fatalf("the fixed mode's selection check = %v, health %v; want nothing read", err, consumers.SelectionHealth(context.Background()))
	}
}

// The fresh mode refuses every call where its selection state is lost, unread, or before the
// cutover, and never falls back to the fixed name: the stream holds no consumer after each.
func TestTheFreshModeRefusesWithoutItsSelectionStateAndNeverUsesTheFixedName(t *testing.T) {
	const session = "ses_fresh_refused"
	filters := []string{contracts.LegacyAgentSubject(session)}
	for name, setup := range map[string]func(t *testing.T, conn *natsgo.Conn){
		"no bucket": func(*testing.T, *natsgo.Conn) {},
		"no sentinel": func(t *testing.T, conn *natsgo.Conn) {
			js, _ := jetstream.New(conn)
			if _, err := js.CreateKeyValue(context.Background(), consumernames.SessionBucketConfig(1)); err != nil {
				t.Fatal(err)
			}
		},
		"before the cutover": func(t *testing.T, conn *natsgo.Conn) {
			if _, err := consumernames.CreateSessionBucket(context.Background(), conn, 1, consumernames.StoreState{Boundary: 1}); err != nil {
				t.Fatal(err)
			}
		},
		"rolled back": func(t *testing.T, conn *natsgo.Conn) {
			if _, err := consumernames.CreateSessionBucket(context.Background(), conn, 1, consumernames.StoreState{Cutover: consumernames.CutoverRolledBack, Boundary: 1}); err != nil {
				t.Fatal(err)
			}
		},
		"key deleted": func(t *testing.T, conn *natsgo.Conn) {
			if _, err := consumernames.CreateSessionBucket(context.Background(), conn, 1, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}); err != nil {
				t.Fatal(err)
			}
			js, _ := jetstream.New(conn)
			kv, err := js.KeyValue(context.Background(), consumernames.SessionBucket)
			if err != nil {
				t.Fatal(err)
			}
			if err := kv.Delete(context.Background(), session); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			owner, consumers, conn := freshSessionConsumersWithoutBucket(t)
			setup(t, conn)
			ctx := context.Background()
			_, err := consumers.Ensure(ctx, session, filters, 0)
			if err == nil {
				t.Fatal("Ensure in the fresh mode was served")
			}
			if !errors.Is(err, consumernames.ErrCutoverNotRun) && !consumernames.IsSelectionStateLost(err) {
				t.Fatalf("Ensure = %v, want a refusal for the cutover or lost selection state", err)
			}
			if _, err := consumers.RestartAtFloor(ctx, session, filters); err == nil {
				t.Fatal("RestartAtFloor in the fresh mode was served")
			}
			if health := consumers.SelectionHealth(ctx); health == nil {
				t.Fatal("SelectionHealth names nothing after a refusal")
			}
			if names := streamConsumers(t, owner); len(names) != 0 {
				t.Fatalf("the stream holds %v after the refusals; want no consumer, fixed or fresh", names)
			}
		})
	}
}

// Each creation of the fresh mode gets a name of its own: a registration, a relay restart of a
// consumer with a message awaiting an ack, and a registration after a delete, which starts at the
// floor the delete recorded.
func TestTheFreshModeCreatesAFreshNameAtEachCreation(t *testing.T) {
	owner, consumers, store := freshSessionConsumersOnStream(t)
	ctx := context.Background()
	const session = "ses_fresh_names"
	agent := contracts.LegacyAgentSubject(session)
	filters := []string{agent}
	first := publishStored(t, owner, agent, "one")
	created, err := consumers.Ensure(ctx, session, filters, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created || !strings.HasPrefix(created.Name, SessionConsumerNamePrefix+"~") || created.StartSeq != first {
		t.Fatalf("Ensure = %+v, want a fresh session~ name created at %d", created, first)
	}
	if record := selectionRecord(t, store, session); record.Current != created.Name || record.Start != first {
		t.Fatalf("record %+v, want %s current at %d", record, created.Name, first)
	}
	again, err := consumers.Ensure(ctx, session, filters, 0)
	if err != nil || again.Created || again.Name != created.Name {
		t.Fatalf("a second Ensure = %+v, %v; want %s kept", again, err, created.Name)
	}

	pull, err := consumers.Pull(ctx, session, created.Name)
	if err != nil {
		t.Fatal(err)
	}
	if msg := next(t, pull); msg.StreamSeq != first {
		t.Fatalf("pulled %d, want %d", msg.StreamSeq, first)
	}
	pull.Stop()
	waitForAckPending(t, consumers, session, 1)
	restarted, err := consumers.RestartAtFloor(ctx, session, filters)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.Created || restarted.Name == created.Name || restarted.StartSeq != first {
		t.Fatalf("RestartAtFloor = %+v, want a fresh name at %d", restarted, first)
	}
	record := selectionRecord(t, store, session)
	if record.Current != restarted.Name || record.Op != nil {
		t.Fatalf("record %+v, want %s current with no op", record, restarted.Name)
	}
	if _, err := owner.JS().ConsumerInfo(Stream, created.Name); !errors.Is(err, natsgo.ErrConsumerNotFound) {
		t.Fatalf("the predecessor %s: %v, want it deleted", created.Name, err)
	}

	pull = pullSessionNamed(t, consumers, session, restarted.Name)
	msg := next(t, pull)
	if err := msg.Ack(); err != nil {
		t.Fatal(err)
	}
	pull.Stop()
	waitForAckFloor(t, consumers, session, first)
	floor, err := consumers.Delete(ctx, session, nil)
	if err != nil || floor != first {
		t.Fatalf("Delete = %d, %v; want floor %d", floor, err, first)
	}
	if record := selectionRecord(t, store, session); record.Current != "" || record.Start != first+1 {
		t.Fatalf("record after the delete %+v, want no current and start %d", record, first+1)
	}
	// The delete's resume record took the stream's next sequence, so the message stored after it
	// is past the record's start, and the next create starts at that start, first+1, and delivers it.
	second := publishStored(t, owner, agent, "two")
	back, err := consumers.Ensure(ctx, session, filters, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Created || back.Name == created.Name || back.Name == restarted.Name || back.StartSeq != first+1 {
		t.Fatalf("Ensure after the delete = %+v, want a third fresh name at the record's start %d", back, first+1)
	}
	pull = pullSessionNamed(t, consumers, session, back.Name)
	if msg := next(t, pull); msg.StreamSeq != second {
		t.Fatalf("the consumer after the delete first delivered %d, want %d", msg.StreamSeq, second)
	}
}

// pullSessionNamed starts a pull of sessionID's consumer name, stopped when the test ends.
func pullSessionNamed(t *testing.T, consumers *SessionConsumers, sessionID, name string) *SessionPull {
	t.Helper()
	pull, err := consumers.Pull(context.Background(), sessionID, name)
	if err != nil {
		t.Fatalf("pull %s: %v", name, err)
	}
	t.Cleanup(pull.Stop)
	return pull
}

// consumerCreateSpy records the consumer names the server is asked to create and delete: a
// subscription of the test's own on $JS.API.CONSUMER.>, in the stream's account, which sees each
// such request any client of the account sends.
type consumerCreateSpy struct {
	mu      sync.Mutex
	creates []string
	deletes []string
}

func spyOnConsumerCalls(t *testing.T, conn *natsgo.Conn) *consumerCreateSpy {
	t.Helper()
	spy := &consumerCreateSpy{}
	sub, err := conn.Subscribe("$JS.API.CONSUMER.>", func(msg *natsgo.Msg) {
		tokens := strings.Split(msg.Subject, ".")
		if len(tokens) < 6 || tokens[4] != Stream {
			return
		}
		spy.mu.Lock()
		defer spy.mu.Unlock()
		switch tokens[3] {
		case "CREATE":
			spy.creates = append(spy.creates, tokens[5])
		case "DELETE":
			spy.deletes = append(spy.deletes, tokens[5])
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	return spy
}

// settle waits until the spy has heard every request sent before it: a request on the spy's own
// connection, which the server answers after every message it routed before it.
func (s *consumerCreateSpy) settle(t *testing.T, conn *natsgo.Conn) {
	t.Helper()
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
}

// sawTwice is the first name the spy saw sent to create more than once, and every fixed name
// (s-<session id>) it saw created.
func (s *consumerCreateSpy) sawTwice() (string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var repeated string
	var fixed []string
	for _, name := range s.creates {
		if seen[name] && repeated == "" {
			repeated = name
		}
		seen[name] = true
		if strings.HasPrefix(name, sessionConsumerPrefix) {
			fixed = append(fixed, name)
		}
	}
	return repeated, fixed
}

func (s *consumerCreateSpy) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates, s.deletes = nil, nil
}

// TestNoConsumerNameIsCreatedTwice is the fixed-name spy: a spy on $JS.API.CONSUMER.> sees the
// fresh mode create no fixed name and no name twice, at every creation site of the session family,
// a faulted create and a recovery included. Its control is the fixed mode's own relay restart of a
// consumer with a message awaiting an ack, which the spy sees create s-<session id> twice.
func TestNoConsumerNameIsCreatedTwice(t *testing.T) {
	ctx := context.Background()
	t.Run("control: the fixed mode's restart", func(t *testing.T) {
		owner, consumers := sessionConsumersOnStream(t)
		spyConn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(spyConn.Close)
		spy := spyOnConsumerCalls(t, spyConn)
		const session = "ses_spy_control"
		agent := contracts.LegacyAgentSubject(session)
		publishStored(t, owner, agent, "one")
		if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
			t.Fatal(err)
		}
		pull := pullSession(t, consumers, session)
		next(t, pull)
		pull.Stop()
		waitForAckPending(t, consumers, session, 1)
		waitForStoredState(t, owner, session)
		if _, err := consumers.RestartAtFloor(ctx, session, []string{agent}); err != nil {
			t.Fatal(err)
		}
		spy.settle(t, spyConn)
		if repeated, fixed := spy.sawTwice(); repeated != "s-"+session || len(fixed) != 2 {
			t.Fatalf("the spy saw repeated %q and fixed %v; it cannot see the fixed mode's same-name re-creation", repeated, fixed)
		}
	})
	t.Run("the fresh mode", func(t *testing.T) {
		owner, consumers, _ := freshSessionConsumersOnStream(t)
		spyConn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(spyConn.Close)
		spy := spyOnConsumerCalls(t, spyConn)
		const session = "ses_spy_fresh"
		agent := contracts.LegacyAgentSubject(session)
		filters := []string{agent}
		publishStored(t, owner, agent, "one")
		start, err := consumers.Ensure(ctx, session, filters, 0)
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			pull := pullSessionNamed(t, consumers, session, start.Name)
			next(t, pull)
			pull.Stop()
			waitForAckPending(t, consumers, session, 1)
			if start, err = consumers.RestartAtFloor(ctx, session, filters); err != nil {
				t.Fatal(err)
			}
		}
		// A create that faults before it is sent, and the recovery that follows.
		pull := pullSessionNamed(t, consumers, session, start.Name)
		next(t, pull)
		pull.Stop()
		waitForAckPending(t, consumers, session, 1)
		consumers.SetCreateFault(func(string) error { return errors.New("the create faulted") })
		if _, err := consumers.RestartAtFloor(ctx, session, filters); err == nil {
			t.Fatal("the faulted restart succeeded")
		}
		consumers.SetCreateFault(nil)
		recoverCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err := consumers.RestartAtFloor(recoverCtx, session, filters); err != nil {
			t.Fatal(err)
		}
		if _, err := consumers.Delete(ctx, session, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := consumers.Ensure(ctx, session, filters, 0); err != nil {
			t.Fatal(err)
		}
		spy.settle(t, spyConn)
		repeated, fixed := spy.sawTwice()
		if repeated != "" || len(fixed) != 0 {
			t.Fatalf("the fresh mode sent %q to create twice and created the fixed names %v", repeated, fixed)
		}
		spy.mu.Lock()
		creates := len(spy.creates)
		spy.mu.Unlock()
		if creates < 6 {
			t.Fatalf("the spy saw %d creates, want at least 6: it heard too little to judge", creates)
		}
	})
}

// A session id of 253 bytes is a key, and one of 254 is refused with the server's 10102 answer
// before anything is written, as its fixed name s-<id> was refused at its create.
func TestTheFreshModeRefusesALongSessionIDAsTheFixedNameWasRefused(t *testing.T) {
	_, consumers, store := freshSessionConsumersOnStream(t)
	ctx := context.Background()
	ok := strings.Repeat("a", 253)
	if _, err := consumers.Ensure(ctx, ok, []string{contracts.LegacyAgentSubject(ok)}, 0); err != nil {
		t.Fatalf("a 253-byte id: %v", err)
	}
	long := strings.Repeat("a", 254)
	_, err := consumers.Ensure(ctx, long, []string{contracts.LegacyAgentSubject(long)}, 0)
	if err == nil || !strings.Contains(err.Error(), "consumer name is too long, maximum allowed is 255") {
		t.Fatalf("a 254-byte id: %v, want the server's 10102 answer", err)
	}
	if _, _, err := store.Read(ctx, long); !errors.Is(err, consumernames.ErrNoRecord) {
		t.Fatalf("the 254-byte id's record: %v, want none written", err)
	}
	// The fixed mode answers the same text, from the server.
	_, fixed := sessionConsumersOnStream(t)
	_, err = fixed.Ensure(ctx, long, []string{contracts.LegacyAgentSubject(long)}, 0)
	if err == nil || !strings.Contains(err.Error(), "consumer name is too long, maximum allowed is 255") {
		t.Fatalf("the fixed mode's 254-byte id: %v, want the server's 10102 answer", err)
	}
}

// A fixed-mode start refuses a selection bucket the cutover converted, naming the setting; starts
// over a store the cutover did not convert (no bucket, no marker, a rollback's marker); and over a
// bucket whose marker it cannot read - one that exists with no sentinel, or with a value under the
// sentinel's key that is not the sentinel (the server's STREAM.INFO answer stored there through a
// request's reply subject, another key's record) - answers an error that is no conversion, which
// the listener reads again rather than run today's names unread. The rows that start are the
// controls of the refusal; each unread row's control is today's reading of the same bytes, a
// lenient decode that takes them for a sentinel with no marker, on which the start went on.
func TestTheFixedModeRefusesAStoreTheCutoverConverted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   *consumernames.StoreState
		bucket  bool
		raw     func(t *testing.T, conn *natsgo.Conn)
		refused bool
		unread  bool
	}{
		{name: "converted", bucket: true, state: &consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}, refused: true},
		{name: "no bucket"},
		{name: "not converted", bucket: true, state: &consumernames.StoreState{Boundary: 1}},
		{name: "rolled back", bucket: true, state: &consumernames.StoreState{Cutover: consumernames.CutoverRolledBack, Boundary: 1}},
		{name: "no sentinel", bucket: true, unread: true},
		{name: "a STREAM.INFO answer as the sentinel", bucket: true, unread: true, raw: func(t *testing.T, conn *natsgo.Conn) {
			if err := conn.PublishRequest("$JS.API.STREAM.INFO.KV_"+consumernames.SessionBucket, "$KV."+consumernames.SessionBucket+"."+consumernames.SentinelKey, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "another key's record as the sentinel", bucket: true, unread: true, raw: func(t *testing.T, conn *natsgo.Conn) {
			if err := conn.Publish("$KV."+consumernames.SessionBucket+"."+consumernames.SentinelKey, []byte(`{"key":"ses_other","current":"session~A"}`)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, consumers := sessionConsumersOnStream(t)
			conn := testnats.Connect(t, testnats.URL(t))
			t.Cleanup(conn.Close)
			ctx := context.Background()
			if tc.bucket {
				js, _ := jetstream.New(conn)
				if _, err := js.CreateKeyValue(ctx, consumernames.SessionBucketConfig(1)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.state != nil {
				state := *tc.state
				state.Family = consumernames.SessionFamily
				if _, err := consumernames.NewKVStore(consumernames.SessionBucket, consumernames.ConnRequester{Conn: conn}).WriteSentinel(ctx, state, 0); err != nil {
					t.Fatal(err)
				}
			}
			if tc.raw != nil {
				tc.raw(t, conn)
				data := awaitSentinelValue(t, conn)
				var lenient consumernames.StoreState
				if err := json.Unmarshal(data, &lenient); err != nil || lenient.Cutover != "" {
					t.Fatalf("control: today's reading of %s = %+v, %v; want a sentinel with no marker, on which the start went on", data, lenient, err)
				}
			}
			err := consumers.RefuseConvertedStore(ctx)
			switch {
			case tc.refused:
				if err == nil || !strings.Contains(err.Error(), consumernames.ModeEnvVar) {
					t.Fatalf("RefuseConvertedStore over a converted bucket = %v; want a refusal naming %s", err, consumernames.ModeEnvVar)
				}
			case tc.unread:
				if err == nil || errors.Is(err, ErrConvertedStore) || !errors.Is(err, consumernames.ErrNoSentinel) {
					t.Fatalf("RefuseConvertedStore over a bucket whose marker it cannot read = %v; want the unread sentinel's error, no conversion, so the start reads again", err)
				}
			default:
				if err != nil {
					t.Fatalf("RefuseConvertedStore over a store the cutover did not convert = %v; want the start to go on", err)
				}
			}
		})
	}
}

// awaitSentinelValue waits until the selection bucket's sentinel key holds a value, and returns it.
func awaitSentinelValue(t *testing.T, conn *natsgo.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.KeyValue(ctx, consumernames.SessionBucket)
	if err != nil {
		t.Fatal(err)
	}
	for {
		entry, err := kv.Get(ctx, consumernames.SentinelKey)
		if err == nil {
			return entry.Value()
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the sentinel key never held the row's value: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A fresh-mode listener started before its selection bucket exists reports the missing bucket,
// and clears it at the next health read once the operator has made the bucket, with no session
// call between: the health read reads the sentinel again while a store refusal stands.
func TestTheSelectionHealthClearsOnceTheBucketIsMade(t *testing.T) {
	_, consumers, conn := freshSessionConsumersWithoutBucket(t)
	ctx := context.Background()
	if err := consumers.CheckSelection(ctx); !consumernames.IsSelectionStateLost(err) {
		t.Fatalf("CheckSelection with no bucket = %v, want the missing bucket", err)
	}
	// Control: while the bucket is missing, the health read names it.
	if err := consumers.SelectionHealth(ctx); err == nil || !strings.Contains(err.Error(), "KV_"+consumernames.SessionBucket) {
		t.Fatalf("SelectionHealth with no bucket = %v, want the bucket named", err)
	}
	if _, err := consumernames.CreateSessionBucket(ctx, conn, 1, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}); err != nil {
		t.Fatal(err)
	}
	if err := consumers.SelectionHealth(ctx); err != nil {
		t.Fatalf("SelectionHealth once the bucket is made = %v, want nil with no session call", err)
	}
}

// A session whose key was found deleted stays named by the health read after another session's
// call is admitted: that success says nothing of the lost key.
func TestASessionsLostKeyStaysNamedAfterAnotherSessionsSuccess(t *testing.T) {
	owner, consumers, _ := freshSessionConsumersOnStream(t)
	ctx := context.Background()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.KeyValue(ctx, consumernames.SessionBucket)
	if err != nil {
		t.Fatal(err)
	}
	const lost, other = "ses_lost_key", "ses_other_key"
	if err := kv.Delete(ctx, lost); err != nil {
		t.Fatal(err)
	}
	if _, err := consumers.Ensure(ctx, lost, []string{contracts.LegacyAgentSubject(lost)}, 0); !errors.Is(err, consumernames.ErrRecordDeleted) {
		t.Fatalf("Ensure of the deleted key = %v, want ErrRecordDeleted", err)
	}
	publishStored(t, owner, contracts.LegacyAgentSubject(other), "one")
	if _, err := consumers.Ensure(ctx, other, []string{contracts.LegacyAgentSubject(other)}, 0); err != nil {
		t.Fatalf("Ensure of another session: %v", err)
	}
	if err := consumers.SelectionHealth(ctx); !errors.Is(err, consumernames.ErrRecordDeleted) || !strings.Contains(err.Error(), lost) {
		t.Fatalf("SelectionHealth after another session's success = %v, want the lost key of %s named", err, lost)
	}
}

// A fixed-mode start whose grant refuses every read of the selection bucket learns nothing of a
// conversion: the refusal is the grant's (ErrConsumerNotGranted), on which the listener warns and
// starts, since a hub that never gave its listener the window's grants holds no bucket the window
// made. The control is the same converted bucket read with a grant that allows the read, which
// refuses the start.
func TestAFixedModeStartRefusedTheBucketsReadIsNoConversion(t *testing.T) {
	seed, public := testnats.User(t)
	stream := "KV_" + consumernames.SessionBucket
	uri := testnats.StartNkeyDenied(t, public, "$JS.API.STREAM.MSG.GET."+stream, "$JS.API.DIRECT.GET."+stream+".>")
	admin := WithEnvironment(func(string) (string, bool) { return "", false })
	ctx := context.Background()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	if _, err := consumernames.CreateSessionBucket(ctx, conn, 1, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}); err != nil {
		t.Fatal(err)
	}
	owner, err := ConnectOwningStream([]string{uri}, admin)
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)

	// Control: a read the grant allows sees the conversion and refuses the start.
	granted, err := ConnectSessionConsumers([]string{uri}, 0, nil, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(granted.Close)
	if err := granted.RefuseConvertedStore(ctx); err == nil || errors.Is(err, ErrConsumerNotGranted) || !strings.Contains(err.Error(), consumernames.ModeEnvVar) {
		t.Fatalf("a granted read of a converted bucket = %v, want the start refused naming %s", err, consumernames.ModeEnvVar)
	}

	t.Setenv("NATS_NKEY_SEED", seed)
	refused, err := ConnectSessionConsumers([]string{uri}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(refused.Close)
	if err := refused.RefuseConvertedStore(ctx); !errors.Is(err, ErrConsumerNotGranted) || !errors.Is(err, ErrPublishDenied) {
		t.Fatalf("a read the grant refuses = %v, want ErrConsumerNotGranted, on which the listener warns and starts", err)
	}
}

// A fixed-mode process reads the selection bucket's marker again at every connection it makes, and
// stops once the marker says the cutover converted the store. The control is the same reconnect
// over a server that holds no bucket, which stops nothing.
func TestAFixedModeReconnectReadsTheMarkerAgainAndStopsOnAConversion(t *testing.T) {
	srv, uri := testnats.Start(t)
	consumers, err := ConnectSessionConsumers([]string{uri}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(consumers.Close)
	converted := make(chan error, 4)
	consumers.WatchConversion(func(err error) { converted <- err })
	// runs counts the reconnect hook runs, each after the conversion watch's hook of the same run.
	runs := make(chan struct{}, 16)
	consumers.client.AddReconnectHook(func(context.Context, *natsgo.Conn) error {
		runs <- struct{}{}
		return nil
	})
	awaitRun := func() {
		t.Helper()
		select {
		case <-runs:
		case <-time.After(30 * time.Second):
			t.Fatal("no reconnect hook ran within 30 s of the restart")
		}
	}

	// Control: a reconnect with no bucket on the server stops nothing.
	srv.Stop(t)
	srv.Start(t)
	awaitRun()
	select {
	case err := <-converted:
		t.Fatalf("a reconnect with no bucket stopped the process: %v", err)
	default:
	}

	conn := testnats.Connect(t, uri)
	if _, err := consumernames.CreateSessionBucket(context.Background(), conn, 1, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	srv.Stop(t)
	srv.Start(t)
	awaitRun()
	select {
	case err := <-converted:
		if !errors.Is(err, ErrConvertedStore) || !strings.Contains(err.Error(), consumernames.ModeEnvVar) {
			t.Fatalf("the reconnect's refusal = %v, want ErrConvertedStore naming %s", err, consumernames.ModeEnvVar)
		}
	default:
		t.Fatal("a reconnect over a converted bucket did not stop the process")
	}
}

// A call refused for its own key never reached the store, so it says nothing of the bucket: a
// missing bucket stays named by the health read after it. The control is the same read before the
// refused call, which names the bucket too.
func TestARefusedKeyLeavesTheSelectionHealthAsItWas(t *testing.T) {
	_, consumers, _ := freshSessionConsumersWithoutBucket(t)
	ctx := context.Background()
	if err := consumers.CheckSelection(ctx); !consumernames.IsSelectionStateLost(err) {
		t.Fatalf("CheckSelection with no bucket = %v, want the missing bucket", err)
	}
	if err := consumers.SelectionHealth(ctx); err == nil {
		t.Fatal("control: SelectionHealth with no bucket names nothing")
	}
	for _, key := range []string{"ses.dotted", strings.Repeat("a", 254)} {
		if _, err := consumers.Info(ctx, key); err == nil {
			t.Fatalf("Info of %q was served", key)
		}
		if err := consumers.SelectionHealth(ctx); err == nil || !strings.Contains(err.Error(), "KV_"+consumernames.SessionBucket) {
			t.Fatalf("SelectionHealth after a refused key %.20q = %v, want the missing bucket still named", key, err)
		}
	}
}

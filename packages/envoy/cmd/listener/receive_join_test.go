package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// startJoiningReceiverRig is startReceiverRig for a pod whose start the NATS grant refused the
// listener state bucket: its receive loop runs without a lease, as the machine's one deliverer,
// and the rig's lease is the one the pod joins once a grant lets it open the bucket, which a row
// hands over through the receiver's join (durableReceiver.startJoin), as main does. Neither the
// lease's loop nor its first rounds are started here. adjust, when not nil, changes the receiver's
// options before it is made: its holder-only loops (durableReceiverOptions.loops), its logger,
// its handler.
func startJoiningReceiverRig(t *testing.T, uri, machineID string, adjust func(*receiverRig, *durableReceiverOptions)) *receiverRig {
	t.Helper()
	return startJoiningReceiverRigVia(t, uri, uri, machineID, adjust)
}

// startJoiningReceiverRigVia is startJoiningReceiverRig with the bus client connected at busURI
// (a proxy, say) and the listener state bucket's connection at stateURI.
func startJoiningReceiverRigVia(t *testing.T, busURI, stateURI, machineID string, adjust func(*receiverRig, *durableReceiverOptions)) *receiverRig {
	t.Helper()
	handle, _ := recordingHandler()
	rig := newReceiverRigVia(t, busURI, stateURI, machineID, "pod-a", handle, testReceiveTiming(), testLeaseTiming())
	// The rig's own receiver was made with its lease; this one is made without, from the same
	// options (newDurableReceiver leaves the lease out of them), and it is the one the rig's
	// reconnect hook and cleanup reach, since both read rig.receiver when they run.
	options := rig.receiver.durableReceiverOptions
	if adjust != nil {
		adjust(rig, &options)
	}
	rig.receiver = newDurableReceiver(options)
	rig.receiver.Start()
	return rig
}

// stopReceiving stops the rig's receiving as the shutdown does (durableReceiver.Stop), within the
// shutdown's own 10 s budget.
func stopReceiving(rig *receiverRig) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rig.receiver.Stop(ctx)
}

// leaseRecordOf reads the rig's lease record, and reports false when the bucket holds none.
func leaseRecordOf(t *testing.T, rig *receiverRig) (leaseRecord, bool) {
	t.Helper()
	entry, err := rig.lease.kv.Get(rig.lease.key)
	if errors.Is(err, natsgo.ErrKeyNotFound) {
		return leaseRecord{}, false
	}
	if err != nil {
		t.Fatalf("read the lease record: %v", err)
	}
	record, err := decodeLeaseRecord(entry.Value())
	if err != nil {
		t.Fatalf("decode the lease record %s: %v", entry.Value(), err)
	}
	return record, true
}

// joinOutlived is how long a row waits after the stop for anything of a join the stop should have
// ended: a lease's take of an absent record and three of testLeaseTiming's renewals.
const joinOutlived = 400 * time.Millisecond

// A pod delivering without a lease that stops while it still tries to join its machine's lease
// (durableReceiver.startJoin) leaves no record of its own renewing after the stop. The stop ends the
// try and waits for it before it stops receiving (durableReceiver.Stop): a try that has not opened
// the bucket returns with no lease, so a grant that lets the open through after the stop takes
// nothing; and a try whose open went through as the stop came, whose first round takes an absent
// record whatever its context says (lease.Contend), hands that lease over, and the stop releases it.
//
// Before, the join ran on a goroutine of main's under a context the shutdown never ended, since
// the shutdown's os.Exit skips its deferred cancel, and the stop did not wait for it: a join that
// completed after the stop took the record and renewed it until the exit, and the standby waited it
// out as an expired one, a hard take-over after a graceful stop.
// It must not run in parallel: its test lease lapses if one renewal is 300 ms late.
func TestAStopEndsALeaseJoinStillTryingAndReleasesTheLeaseItTook(t *testing.T) {
	uri := testnats.URL(t)

	t.Run("a join the stop ended takes no lease", func(t *testing.T) {
		rig := startJoiningReceiverRig(t, uri, "box-join-ended", nil)
		rig.awaitState(t, receiveActive, 10*time.Second)
		granted, ended := make(chan struct{}), make(chan struct{})
		rig.receiver.startJoin(func(ctx context.Context) *lease {
			select {
			case <-granted:
			case <-ctx.Done():
				close(ended)
				return nil
			}
			rig.lease.Contend(ctx)
			rig.lease.Start(context.Background())
			return rig.lease
		})

		stopReceiving(rig)
		select {
		case <-ended:
		default:
			t.Fatal("the stop returned with the lease join still trying: nothing ended it, so a grant applied now would take the record after the stop")
		}
		// The grant lets the open through now, after the stop: a try still running would take
		// the record and renew it.
		close(granted)
		time.Sleep(joinOutlived)
		if record, found := leaseRecordOf(t, rig); found && !record.Released {
			t.Fatalf("the lease record %s after the stop is %+v, want none or a released one", joinOutlived, record.leaseFields)
		}
	})

	t.Run("a lease the join took as the stop came is released by the stop", func(t *testing.T) {
		rig := startJoiningReceiverRig(t, uri, "box-join-landed", nil)
		rig.awaitState(t, receiveActive, 10*time.Second)
		took := make(chan leaseStatus, 1)
		rig.receiver.startJoin(func(ctx context.Context) *lease {
			<-ctx.Done()
			took <- rig.lease.Contend(ctx)
			rig.lease.Start(context.Background())
			return rig.lease
		})

		stopReceiving(rig)
		select {
		case status := <-took:
			if !status.Holding {
				t.Fatalf("UNKNOWN: the join's first round left the lease %+v, want it holding the absent record; the row needs a join that took the record", status)
			}
		default:
			t.Fatal("the stop returned before the lease join it ended had returned")
		}
		record, found := leaseRecordOf(t, rig)
		if !found || record.Holder != "pod-a" || !record.Released {
			t.Fatalf("the lease record after the stop is %+v (found %t), want pod-a's, released", record.leaseFields, found)
		}
		time.Sleep(joinOutlived)
		if again, _ := leaseRecordOf(t, rig); !again.Released {
			t.Fatalf("the lease record %s after the stop is %+v, want it still released: a renewal wrote it again", joinOutlived, again.leaseFields)
		}
	})
}

// startRefusedJoinRig is startJoiningReceiverRig for a pod whose NATS grant refuses the listener
// state bucket throughout, on a server of its own whose nkey user may publish to its inboxes and
// ask for the account's JetStream info ($JS.API.INFO) and nothing of the bucket. It returns the
// rig, its receive loop active, and the pod's lease connection, dialled as that user as main dials
// it (openDeliveryLease), which a row hands main's join (joinAsMain).
func startRefusedJoinRig(t *testing.T, machineID string) (*receiverRig, *natsgo.Conn) {
	t.Helper()
	seed, public := testnats.User(t)
	refusing := testnats.Grant{
		Publish:   testnats.GrantRules{Allow: []string{"_INBOX.>", "$JS.API.INFO"}},
		Subscribe: testnats.GrantRules{Allow: []string{"_INBOX.>"}},
	}
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: refusing})
	rig := startJoiningReceiverRig(t, granted.AdminURL, machineID, nil)
	rig.awaitState(t, receiveActive, 10*time.Second)
	conn, err := bus.Dial("receive-test-join "+machineID, []string{granted.URL()}, testnats.SeedOnly(seed))
	if err != nil {
		t.Fatalf("connect the lease as the refused user: %v", err)
	}
	t.Cleanup(conn.Close)
	return rig, conn
}

// joinAsMain hands the rig's receiver main's join of machineID's lease over conn
// (durableReceiver.startJoin): joinDeliveryLease, then the first rounds and the loop of the lease
// it returns (lease.Contend, lease.Start). The channel it returns gets what joinDeliveryLease
// returned, before the try returns.
func joinAsMain(rig *receiverRig, conn *natsgo.Conn, machineID string) <-chan *lease {
	tried := make(chan *lease, 1)
	rig.receiver.startJoin(func(ctx context.Context) *lease {
		joined := joinDeliveryLease(ctx, conn, 1, machineID, "pod-a", rig.receiver.logger)
		tried <- joined
		if joined == nil {
			return nil
		}
		joined.Contend(ctx)
		joined.Start(context.Background())
		return joined
	})
	return tried
}

// joinRefusedStopBound is how long the stop of a pod whose lease join waits between two tries of
// the bucket its grant refuses may take: the join returns at the stop, and a pod receiving without
// a lease and holding no message stops receiving well inside it.
const joinRefusedStopBound = time.Second

// A pod whose NATS grant refuses the listener state bucket, stopped while its lease join waits
// between two tries of that bucket (joinDeliveryLease), ends the join at the stop: the stop returns
// within joinRefusedStopBound, and the join returns with no lease. The row runs main's own join
// through the receiver's (durableReceiver.startJoin, joinAsMain), on a lease connection of an nkey
// user whose grant lacks the bucket (startRefusedJoinRig), and stops the pod once the join has
// logged listenerStateRefusedLine twice, so the stop comes in the second refusal's wait, 4 s, past
// the first doubling.
//
// Before, the join slept out each wait between two refused tries (waitOutGrantRefusal), whatever
// its context said, and the stop waits for the join: the stop spent the rest of that wait, up to
// durableRefusedRetryCap and so past the shutdown's 10 s, and a join that woke once the stop had
// given up opened the bucket a grant applied meanwhile let through, took the record and renewed
// it until the exit.
// It must not run in parallel: its stop must return within joinRefusedStopBound, 1 s.
func TestAStopEndsALeaseJoinWaitingOutTheGrantsRefusal(t *testing.T) {
	const machineID = "box-join-refused"
	rig, conn := startRefusedJoinRig(t, machineID)
	tried := joinAsMain(rig, conn, machineID)
	// Each refused try runs out the open's listenerStateOpenSends waits: the server drops a refused
	// request, and a lease connection no bus client owns reads the refusal only once the open ends.
	secondRefusal := durableBindPoll + nextRefusedWait(durableBindPoll) + 2*listenerStateOpenSends*listenerStateRequestWait + 10*time.Second
	testwait.WithinPolling(t, secondRefusal, 10*time.Millisecond, "the lease join's second refusal of the listener state bucket", func() bool {
		return strings.Count(rig.logs.String(), listenerStateRefusedLine) >= 2
	})

	stopping := time.Now()
	stopReceiving(rig)
	took := time.Since(stopping)
	if took > joinRefusedStopBound {
		t.Fatalf("the stop took %s, want within %s: it waited for the lease join to sleep out its wait for the next try of the bucket", took.Round(time.Millisecond), joinRefusedStopBound)
	}
	t.Logf("the stop took %s, with the lease join waiting for its next try", took.Round(time.Millisecond))
	select {
	case joined := <-tried:
		if joined != nil {
			t.Fatal("the lease join the stop ended returned a lease, want none: the grant refused the bucket throughout")
		}
	default:
		t.Fatal("the stop returned with the lease join still waiting for its next try: a grant applied now would let it take the record after the stop")
	}
}

// joinTryEndLines are the lines joinDeliveryLease logs at the end of a try of the listener state
// bucket that failed, one at each such try, each naming the wait before the next try: the grant's
// refusal (listenerStateRefusedLine), an answer from the cluster's state (clusterStateLine) and
// any other failure (listenerStateOpenFailedLine).
var joinTryEndLines = []string{listenerStateRefusedLine, clusterStateLine, listenerStateOpenFailedLine}

// A pod whose NATS grant refuses the listener state bucket, stopped while its lease join's open of
// that bucket is in flight (joinDeliveryLease, openListenerState), ends the join with nothing
// logged: none of joinTryEndLines names a retry the pod will not make, and the join returns with
// no lease. The row runs main's own join through the receiver's (durableReceiver.startJoin,
// joinAsMain) on a lease connection of an nkey user whose grant lacks the bucket
// (startRefusedJoinRig), and stops the pod inside the join's first open: the lease connection has
// sent that open's first request (nats.Conn.Stats), and the join has logged none of
// joinTryEndLines, which a try logs only once its open has ended. The server drops each refused
// request, so that open runs out listenerStateOpenSends of listenerStateRequestWait, 6 s.
//
// Before, the join read ctx only once an open had succeeded: an open the stop failed was read as
// the grant's refusal or as a failed open, and the pod logged listenerStateRefusedLine at ERROR, or
// listenerStateOpenFailedLine at WARN, each with a retry_in, as it stopped.
// It must not run in parallel: the stop must follow the open's first request by well under the
// open's 6 s, and the row reads the log for the open's end only just before it stops the pod.
func TestAStopThatCutsALeaseJoinsOpenLogsNoRetry(t *testing.T) {
	const machineID = "box-join-cut"
	rig, conn := startRefusedJoinRig(t, machineID)
	sentBefore := conn.Stats().OutMsgs
	tried := joinAsMain(rig, conn, machineID)
	// The join's first open starts durableBindPoll after the join does, with its first request.
	firstSend := durableBindPoll + 10*time.Second
	if !testwait.Holds(firstSend, time.Millisecond, func() bool { return conn.Stats().OutMsgs > sentBefore }) {
		t.Fatalf("UNARRANGED: the lease connection sent no request in %s of the join, want its first open's; the row needs the stop inside that open", firstSend)
	}
	logsBefore := rig.logs.String()
	for _, line := range joinTryEndLines {
		if strings.Contains(logsBefore, line) {
			t.Fatalf("UNARRANGED: the join logged %q before the stop, so its first open had ended; the row needs the stop inside that open", line)
		}
	}

	stopReceiving(rig)
	logsAfter := rig.logs.String()
	for _, line := range joinTryEndLines {
		if logged := strings.Count(logsAfter, line) - strings.Count(logsBefore, line); logged != 0 {
			t.Errorf("the join logged %q %d times from the stop on, want none: the stop cut its open, and the line names a retry the pod will not make", line, logged)
		}
	}
	select {
	case joined := <-tried:
		if joined != nil {
			t.Fatal("the lease join the stop cut returned a lease, want none: the grant refused the bucket throughout")
		}
	default:
		t.Fatal("the stop returned with the lease join still in its open of the bucket")
	}
}

// joinShutdownRounds is how many pods TestAShutdownThatComesAsTheLeaseJoinsEndsTheTermItReceivedOn
// stops as their lease joins. A wait that took the join one time in two would pass them all once
// in 65,536 runs.
const joinShutdownRounds = 16

// A shutdown that comes as the lease joins a pod receiving without one ends the term the pod
// received on, as the stop of a pod without a lease ends it: its subscription drains and what it
// holds is NAKed, and the stop then releases the lease that joined, which the pod never took up.
// The receive loop meets the join and the shutdown together: the row holds the loop before it
// waits on either, in the start of its holder-only loops (durableReceiverOptions.loops), hands the
// pod its lease, which takes the absent record, ends the loop's context as the stop's first step
// does, and only then lets the loop go, so both are ready when it waits. Each round is a pod of its
// own.
//
// The wait took either of the two at random, and took the join one time in two: the term was
// handed to the lease (joinTerm), and the loop returned at the shutdown before it served the
// lease's term, so nothing drained or NAKed and the stop released the lease with the pod's
// subscription still open. A round the wait sends to the join logs leaseJoinedHolderLine.
// It must not run in parallel: its test lease lapses if one renewal is 300 ms late.
func TestAShutdownThatComesAsTheLeaseJoinsEndsTheTermItReceivedOn(t *testing.T) {
	uri := testnats.URL(t)
	for round := range joinShutdownRounds {
		if !t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			rig := startJoiningReceiverRig(t, uri, fmt.Sprintf("box-join-shutdown-%d", round), func(_ *receiverRig, options *durableReceiverOptions) {
				options.loops = func(context.Context) []<-chan struct{} {
					once.Do(func() { close(entered) })
					<-release
					return nil
				}
			})
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				close(release)
				t.Fatal("the pod never received the durable without a lease")
			}

			rig.receiver.startJoin(func(ctx context.Context) *lease {
				rig.lease.Contend(ctx)
				rig.lease.Start(context.Background())
				return rig.lease
			})
			select {
			case <-rig.receiver.joinCtx.Done():
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("the lease never joined")
			}
			if status := rig.lease.Status(); !status.Holding {
				close(release)
				t.Fatalf("UNKNOWN: the joined lease is %+v, want it holding the record; the row needs a join that took it", status)
			}
			rig.receiver.mu.Lock()
			stop := rig.receiver.stop
			rig.receiver.mu.Unlock()
			stop()
			close(release)
			stopReceiving(rig)

			if strings.Contains(rig.logs.String(), leaseJoinedHolderLine) {
				t.Fatalf("the pod logged %q after its shutdown came: the wait took the join, and the term went to the lease rather than ending with the shutdown", leaseJoinedHolderLine)
			}
			if report := rig.receiver.report(); report.State != receiveStandby {
				t.Fatalf("the stopped pod's receive loop stands at %q, want %q: its term was never ended", report.State, receiveStandby)
			}
			rig.logRecord(t, "stopped receiving the durable", time.Second)
			awaitDurable(t, rig.client, rig.consumer, "the durable to read unbound once the pod's term ended", func(info *natsgo.ConsumerInfo) bool {
				return !info.PushBound
			})
			if record, found := leaseRecordOf(t, rig); !found || record.Holder != "pod-a" || !record.Released {
				t.Fatalf("the lease record after the stop is %+v (found %t), want pod-a's, released by the stop", record.leaseFields, found)
			}
		}) {
			break
		}
	}
}

// heldLogLine is a log writer that holds the first write of a record carrying line until release
// is closed, closing at once that write begins, and passes every write on to w: a row stops the
// goroutine that logs line just after it does, which nothing else of the code lets a row do.
type heldLogLine struct {
	w       io.Writer
	line    []byte
	at      chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *heldLogLine) Write(p []byte) (int, error) {
	if bytes.Contains(p, h.line) {
		h.once.Do(func() {
			close(h.at)
			<-h.release
		})
	}
	return h.w.Write(p)
}

// A shutdown that comes once the lease that joined has taken over the term a pod received on
// without a lease (joinTerm) ends that term as the holder's, with the hand-back a holder's stop
// makes: its subscription drains once the hand-back's linger ends, and the lease is released. The
// row holds the receive loop on the join's own line (leaseJoinedHolderLine), which it logs once
// the lease and the term are the holder's, and ends the loop's context there, as the stop's first
// step does, before it lets the loop go on.
//
// Before, the loop returned at the shutdown before it served the lease's term (run), so the term
// was never ended: its subscription stayed open, nothing was handed back, and the stop released the
// lease under it.
// It must not run in parallel: its test lease lapses if one renewal is 300 ms late.
func TestAShutdownRightAfterTheLeaseTookTheTermOverHandsTheTermBack(t *testing.T) {
	uri := testnats.URL(t)
	held := &heldLogLine{line: []byte(leaseJoinedHolderLine), at: make(chan struct{}), release: make(chan struct{})}
	rig := startJoiningReceiverRig(t, uri, "box-join-taken", func(rig *receiverRig, options *durableReceiverOptions) {
		held.w = rig.logs
		options.logger = logging.NewWithWriter("box-join-taken", held)
	})
	rig.awaitState(t, receiveActive, 10*time.Second)
	rig.receiver.startJoin(func(ctx context.Context) *lease {
		rig.lease.Contend(ctx)
		rig.lease.Start(context.Background())
		return rig.lease
	})
	select {
	case <-held.at:
	case <-time.After(5 * time.Second):
		close(held.release)
		t.Fatal("the joined lease never took the term over")
	}
	rig.receiver.mu.Lock()
	stop := rig.receiver.stop
	rig.receiver.mu.Unlock()
	stop()
	close(held.release)
	stopReceiving(rig)

	if report := rig.receiver.report(); report.State != receiveStandby {
		t.Fatalf("the stopped pod's receive loop stands at %q, want %q: the term the lease took over was never ended", report.State, receiveStandby)
	}
	if end := handBackEnd(t, rig); end["handed_back"] != float64(0) {
		t.Fatalf("the pod's hand-back ended %v, want it run with nothing to hand back", end)
	}
	awaitDurable(t, rig.client, rig.consumer, "the durable to read unbound once the pod's term ended", func(info *natsgo.ConsumerInfo) bool {
		return !info.PushBound
	})
	if record, found := leaseRecordOf(t, rig); !found || record.Holder != "pod-a" || !record.Released {
		t.Fatalf("the lease record after the stop is %+v (found %t), want pod-a's, released", record.leaseFields, found)
	}
}

// deadHolderRecord is the lease record of a holder that died: another pod's name and boot,
// written once and never renewed or released.
const deadHolderRecord = `{"holder":"pod-dead","boot":"0123456789abcdef"}`

// logRecordTime is when the log record was written, as its time field says.
func logRecordTime(t *testing.T, record map[string]any) time.Time {
	t.Helper()
	text, _ := record["time"].(string)
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatalf("read the time of %v: %v", record, err)
	}
	return at
}

// heldTakeKV is the listener state bucket as a lease writes it, with the lease's first write of
// the record, its take, held until Release: reached closes as that write begins, so a row acts
// while the take is in flight, before the record names this pod. Every other call passes on.
type heldTakeKV struct {
	natsgo.KeyValue
	reached chan struct{}
	release chan struct{}
	held    sync.Once
	let     sync.Once
}

func (k *heldTakeKV) hold() {
	k.held.Do(func() {
		close(k.reached)
		<-k.release
	})
}

// Release lets the held take go on.
func (k *heldTakeKV) Release() { k.let.Do(func() { close(k.release) }) }

func (k *heldTakeKV) Create(key string, value []byte) (uint64, error) {
	k.hold()
	return k.KeyValue.Create(key, value)
}

func (k *heldTakeKV) Update(key string, value []byte, last uint64) (uint64, error) {
	k.hold()
	return k.KeyValue.Update(key, value, last)
}

// holdTake has the rig's lease, which must not have started, hold its take (heldTakeKV), and
// lets the take go as the row ends, so a row that fails before it does still stops.
func holdTake(t *testing.T, rig *receiverRig) *heldTakeKV {
	t.Helper()
	kv := rig.lease.kv
	held := &heldTakeKV{KeyValue: kv.Legacy(), reached: make(chan struct{}), release: make(chan struct{})}
	rig.lease.kv = bus.NewKeyValue(held, kv.WatchHandle(), kv.Streams())
	t.Cleanup(held.Release)
	return held
}

// dropAtTheBucketsAge has the rig's listener state bucket drop its lease record at the end of
// the record's life, as its MaxAge drops a record nobody writes again: it shortens the bucket's
// MaxAge to a second, waits for the record to be gone, and puts the age back, so the join meets
// the bucket as a listener keeps it. The age is shortened on the bucket the rig opened rather than
// at its create, since every open raises a shorter one (openListenerState).
func dropAtTheBucketsAge(t *testing.T, rig *receiverRig) {
	t.Helper()
	js := rig.client.JS()
	info, err := js.StreamInfo("KV_" + bus.ListenerStateBucket)
	if err != nil {
		t.Fatalf("read the listener state bucket's stream: %v", err)
	}
	short := info.Config
	short.MaxAge, short.Duplicates = time.Second, time.Second
	testnats.UpdateStream(t, js, short)
	testwait.Until(t, "the bucket's MaxAge to drop the lease record", func() (bool, bool) {
		_, found := leaseRecordOf(t, rig)
		return found, !found
	})
	testnats.UpdateStream(t, js, info.Config)
}

// A pod that delivered without a lease and joins its machine's lease late, meeting a dead
// holder's record (deadHolderRecord), takes the lease and keeps its subscription whatever the
// record's age: one just written, inside the 20 minutes the bucket keeps it (listenerStateMaxAge),
// the join's first rounds wait out as any standby does and take as expired (lease.Contend); one
// the bucket's MaxAge dropped at the end of that life they take as absent. Either way the join
// logs leaseJoinedHolderLine and never leaseJoinedStandbyLine, the term it received on is the
// lease's with no second subscribe, and the messages published before the join, during it (while
// its take is held, heldTakeKV) and after it each reach the handler, which stands for their
// sessions, once and in publish order, every one acknowledged, so none comes back at its ack
// wait. The term taken as expired must settle, and with nothing outstanding anywhere else its
// settle hold ends settled, not at its cap, within a second of the take (durableReceiver.adopt);
// the one taken as absent holds nothing. A live holder, whose renewals move the record's
// revision, still stands the join by: it logs leaseJoinedStandbyLine naming that holder and takes
// nothing.
//
// Before, the join's first rounds returned at their first read of a record another process held,
// so the join stood down on the dead holder's record: it logged leaseJoinedStandbyLine and ended
// the term it received on, and a ttl later its own loop took the record as expired, so what the
// ended term held came back at its ack wait, after newer messages.
// It must not run in parallel: its test lease lapses if one renewal is 300 ms late.
func TestALateLeaseJoinTakesADeadHoldersRecordAndKeepsItsSubscription(t *testing.T) {
	for _, row := range []struct {
		name      string
		machineID string
		dropped   bool
		taken     leaseAcquisition
	}{
		{"a record just written", "box-join-dead", false, leaseTakenExpired},
		{"a record the bucket's MaxAge dropped", "box-join-dropped", true, leaseTakenAbsent},
	} {
		t.Run(row.name, func(t *testing.T) {
			uri := testnats.URL(t)
			handle, received := recordingHandler()
			rig := startJoiningReceiverRig(t, uri, row.machineID, func(_ *receiverRig, options *durableReceiverOptions) {
				options.handle = handle
			})
			rig.awaitState(t, receiveActive, 10*time.Second)
			publishDurable(t, rig.client, "before")
			got := []receivedMessage{awaitReceived(t, received, 5*time.Second, "the message published before the join")}

			putLeaseRecord(t, uri, row.machineID, deadHolderRecord)
			if row.dropped {
				dropAtTheBucketsAge(t, rig)
			}
			take := holdTake(t, rig)
			rig.receiver.startJoin(func(ctx context.Context) *lease {
				rig.lease.Contend(ctx)
				rig.lease.Start(context.Background())
				return rig.lease
			})
			testwait.Receive(t, "the lease's take of the record", take.reached)
			if strings.Contains(rig.logs.String(), leaseJoinedStandbyLine) {
				t.Fatalf("the join logged %q before its lease took the record: it stood down on the dead holder's record", leaseJoinedStandbyLine)
			}
			publishDurable(t, rig.client, "during")
			got = append(got, awaitReceived(t, received, 5*time.Second, "the message published while the join's take was held"))
			take.Release()
			if joined := rig.logRecord(t, leaseJoinedHolderLine, 5*time.Second); joined["lease"] != row.taken.String() {
				t.Fatalf("the join took the lease from a record it reads %v, want %s: %v", joined["lease"], row.taken, joined)
			}
			publishDurable(t, rig.client, "after")
			got = append(got, awaitReceived(t, received, 5*time.Second, "the message published after the join"))

			for i, data := range []string{"before", "during", "after"} {
				if got[i].data != data || got[i].delivered != 1 {
					t.Fatalf("the handler took %+v, want before, during and after, in that order, each at its first delivery", got)
				}
			}
			awaitDurable(t, rig.client, rig.consumer, "every message acknowledged", func(info *natsgo.ConsumerInfo) bool {
				return info.NumAckPending == 0 && info.NumPending == 0
			})
			if info, err := rig.client.JS().ConsumerInfo(bus.Stream, rig.consumer); err != nil || info.Delivered.Consumer != 3 || info.NumRedelivered != 0 {
				t.Fatalf("the durable after the three messages: %+v (%v), want three deliveries and no redelivery", info, err)
			}
			rig.awaitRecord(t, "pod-a's, receiving", func(record leaseRecord) bool { return record.Holder == "pod-a" && record.Receiving })
			subscribes, held := 0, false
			var taken map[string]any
			for _, line := range logtest.JSONLines(t, rig.logs.String()) {
				switch line["msg"] {
				case "durable receiving":
					subscribes++
				case leaseJoinedStandbyLine:
					t.Fatalf("the join logged %q: %v", leaseJoinedStandbyLine, line)
				case "listener lease acquired":
					taken = line
				case "settle hold ended; delivering the held messages in stream order":
					held = true
				}
			}
			if subscribes != 1 {
				t.Fatalf("the pod subscribed to the durable %d times, want once: the lease takes over the subscription it received on without one", subscribes)
			}
			if row.taken != leaseTakenExpired {
				if held {
					t.Fatalf("the term taken from a record that asks no settle held what it received:\n%s", rig.logs.String())
				}
				return
			}
			end := rig.settleHoldEnd(t, time.Second)
			if after := logRecordTime(t, end).Sub(logRecordTime(t, taken)); end["outcome"] != "settled" || after > time.Second {
				t.Fatalf("the settle hold of the term taken over the dead holder's record ended %v, %s after the take, want settled within a second", end, after)
			}
			rig.awaitRecord(t, "pod-a's, no longer unsettled", func(record leaseRecord) bool { return record.Holder == "pod-a" && !record.Unsettled })
		})
	}

	t.Run("a live holder's record stands the join by", func(t *testing.T) {
		uri := testnats.URL(t)
		const machineID = "box-join-live"
		conn := testnats.Connect(t, uri)
		t.Cleanup(conn.Close)
		kv, err := openListenerState(t.Context(), conn, 1, listenerStateRequestWait)
		if err != nil {
			t.Fatalf("open the listener state bucket for pod-b: %v", err)
		}
		holder := newLease(kv, machineID, "pod-b", testLeaseTiming(), time.Now, logging.NewWithWriter(machineID, io.Discard))
		holder.Start(context.Background())
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = holder.Release(ctx)
		})
		waitFor(t, "pod-b to take the absent lease record", holder.Holding)

		rig := startJoiningReceiverRig(t, uri, machineID, nil)
		rig.awaitState(t, receiveActive, 10*time.Second)
		rig.receiver.startJoin(func(ctx context.Context) *lease {
			rig.lease.Contend(ctx)
			rig.lease.Start(context.Background())
			return rig.lease
		})
		if standby := rig.logRecord(t, leaseJoinedStandbyLine, 5*time.Second); standby["active_instance"] != "pod-b" {
			t.Fatalf("the join stood by for %v, want pod-b, which renews the record: %v", standby["active_instance"], standby)
		}
		if strings.Contains(rig.logs.String(), leaseJoinedHolderLine) || rig.lease.Holding() {
			t.Fatal("the join took the lease from pod-b, which renews it")
		}
		if record, found := leaseRecordOf(t, rig); !found || record.Holder != "pod-b" || record.Released {
			t.Fatalf("the lease record after the join is %+v (found %t), want pod-b's, held", record.leaseFields, found)
		}
	})
}

// A pod that delivered without a lease, whose binding broke while the holder of the record its
// join later meets bound the durable and took messages it never acked before it died, holds what
// reaches it after the join until those messages come back at their ack wait: the lease takes
// that holder's record as expired, so the term the join adopts must settle
// (durableReceiver.adopt), and its settle hold delivers the held messages first, in stream order,
// and ends settled once every outstanding message is in this pod's hands (receiveTerm.settle).
// The row cuts the pod's bus connection (testnats.Proxy), binds the durable as that holder
// (bindAsAHolder), which takes two messages and goes, restores the connection, and joins the lease
// once the pod is bound again; it publishes two newer messages after the join, inside the held
// messages' ack wait. All four reach the handler once, in stream order, and none is left
// unacknowledged.
//
// Before, the join adopted the term with no hold: the newer messages reached the handler at once,
// and the held ones a whole ack wait later, after them.
// It must not run in parallel: its test lease lapses if one renewal is 300 ms late, and the join
// must come inside the held messages' 6 s ack wait.
func TestALateLeaseJoinOverAHolderThatTookMessagesDeliversThoseFirst(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-join-held"
	proxy := testnats.StartProxy(t, uri)
	handle, received := recordingHandler()
	rig := startJoiningReceiverRigVia(t, proxy.URL(), uri, machineID, func(_ *receiverRig, options *durableReceiverOptions) {
		options.handle = handle
	})
	rig.awaitState(t, receiveActive, 10*time.Second)

	// pod-a's binding breaks, and the holder binds the durable, takes two messages and goes.
	proxy.Cut()
	client, holder := bindAsAHolder(t, uri, machineID, func(*natsgo.Msg) {})
	publishDurable(t, client, "held-1")
	publishDurable(t, client, "held-2")
	awaitDurable(t, client, rig.consumer, "the holder to take both messages", func(info *natsgo.ConsumerInfo) bool {
		return info.NumAckPending == 2 && info.NumPending == 0
	})
	taken := time.Now()
	if err := holder.Unsubscribe(); err != nil {
		t.Fatalf("unsubscribe the holder: %v", err)
	}
	awaitDurable(t, client, rig.consumer, "the durable to read unbound with both messages outstanding", func(info *natsgo.ConsumerInfo) bool {
		return !info.PushBound && info.NumAckPending == 2
	})
	proxy.Restore()
	awaitDurable(t, client, rig.consumer, "pod-a to bind the durable again", func(info *natsgo.ConsumerInfo) bool {
		return info.PushBound
	})

	putLeaseRecord(t, uri, machineID, deadHolderRecord)
	rig.receiver.startJoin(func(ctx context.Context) *lease {
		rig.lease.Contend(ctx)
		rig.lease.Start(context.Background())
		return rig.lease
	})
	if joined := rig.logRecord(t, leaseJoinedHolderLine, 5*time.Second); joined["lease"] != leaseTakenExpired.String() {
		t.Fatalf("the join took the lease from a record it reads %v, want the holder's, expired: %v", joined["lease"], joined)
	}
	publishing := time.Now()
	t.Logf("the newer messages go out %s after the holder took the older ones, inside their %s ack wait", publishing.Sub(taken).Round(time.Millisecond), consumerAckWait)
	publishDurable(t, client, "newer-1")
	publishDurable(t, client, "newer-2")
	var got []receivedMessage
	for range 4 {
		got = append(got, awaitReceived(t, received, 15*time.Second, "the held messages and the newer ones"))
	}
	if got[0].at.Before(publishing) {
		t.Fatalf("UNKNOWN: the handler took %+v, the first before the newer messages were published: the held ones came back before the join, and the row needs a join inside their ack wait", got)
	}
	for i, want := range []receivedMessage{{data: "held-1", delivered: 2}, {data: "held-2", delivered: 2}, {data: "newer-1", delivered: 1}, {data: "newer-2", delivered: 1}} {
		if got[i].data != want.data || got[i].delivered != want.delivered {
			t.Fatalf("the handler took %+v, want the holder's two messages, redelivered, before the two newer ones, in stream order", got)
		}
	}
	if end := rig.settleHoldEnd(t, time.Second); end["outcome"] != "settled" {
		t.Fatalf("the settle hold ended %v, want settled once the held messages came back", end)
	}
	awaitDurable(t, client, rig.consumer, "every message acknowledged", func(info *natsgo.ConsumerInfo) bool {
		return info.NumAckPending == 0 && info.NumPending == 0
	})
	select {
	case message := <-received:
		t.Fatalf("the handler took %q (delivery %d) after the four", message.data, message.delivered)
	default:
	}
	rig.awaitRecord(t, "pod-a's, no longer unsettled", func(record leaseRecord) bool { return record.Holder == "pod-a" && !record.Unsettled })
}

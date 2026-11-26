package testnats

import (
	"encoding/json"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// heartbeatEvery is how often HoldConsumerLost sends its idle heartbeat: well inside nats.go's
// heartbeat check, two 5 s heartbeat intervals for an ordered consumer (nats.go v1.55.0 js.go,
// orderedHeartbeatsInterval and hbcThresh), so a check never finds a whole interval without one.
const heartbeatEvery = 500 * time.Millisecond

// ConsumerHold is a consumer HoldConsumerLost deleted and holds lost.
type ConsumerHold struct {
	conn    *natsgo.Conn
	stream  string
	creates chan *natsgo.Msg
	once    sync.Once
	stopped chan struct{}
	done    chan struct{}
	// startSeq and the filters are what nats.go's replacement of the lost consumer carries: it
	// starts at the lost one's next stream sequence, and keeps its filter (Replaced).
	startSeq uint64
	filter   string
	filters  []string
	// replaced counts the creates read so far that put a consumer in the lost one's place.
	replaced int
}

// HoldConsumerLost deletes the ordered consumer named consumer on stream, as a server restart loses
// one, and keeps its client from putting a new one in its place until the hold is released or the
// test ends. nats.go replaces an ordered consumer only once a whole heartbeat check passes in which
// the consumer's subscription heard nothing (js.go, activityCheck), so a test that deletes one and
// then waits, or runs a shutdown that takes its time, meets nats.go's replacement 5 to 20 s on, and
// later still on a loaded machine, rather than the loss it meant to test. HoldConsumerLost sends the
// subscription, on the consumer's deliver subject, the idle heartbeat the server would, every
// heartbeatEvery: the sequences it carries are the consumer's own as it last reported them, so
// nats.go finds no gap (js.go checkForSequenceMismatch reads one only where the heartbeat's last
// consumer sequence is at or past the next the subscription expects), marks the subscription active
// and resets nothing. A stop of that subscription then deletes the consumer it still names and
// finds it gone, however long the code under test takes to get there.
//
// The heartbeats are timers of the test's process, so a stall of the test or of the client under
// test past a whole heartbeat check still lets nats.go replace the consumer. Replaced says whether
// it did, from the server's own record of the create, so a test reads the loss only where it held.
//
// The heartbeats are published through conn, a connection of the test's own. A subscription that
// had not yet taken every message the consumer delivered before its info was read would read the
// heartbeat's sequence as a gap, and nats.go would then reset it, so the client must have caught up
// and the bucket must be idle: no write lands on it while the hold runs.
func HoldConsumerLost(t testing.TB, conn *natsgo.Conn, stream, consumer string) *ConsumerHold {
	t.Helper()
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	info, err := js.ConsumerInfo(stream, consumer)
	if err != nil {
		t.Fatalf("read the consumer %s on %s: %v", consumer, stream, err)
	}
	if info.Config.DeliverSubject == "" {
		t.Fatalf("the consumer %s on %s has no deliver subject: it is no ordered consumer", consumer, stream)
	}
	hold := &ConsumerHold{
		conn:     conn,
		stream:   stream,
		creates:  make(chan *natsgo.Msg, 1024),
		stopped:  make(chan struct{}),
		done:     make(chan struct{}),
		startSeq: info.Delivered.Stream + 1,
		filter:   info.Config.FilterSubject,
		filters:  info.Config.FilterSubjects,
	}
	// Every consumer create on the stream is a request to the server's JetStream API, which a
	// subscriber of the API's subject is sent a copy of; nats.go's replacement is one that starts
	// at the lost consumer's next sequence (resetOrderedConsumer). Subscribed before the delete,
	// the hold misses none, and a stall of the test leaves them queued on the connection.
	_, err = conn.ChanSubscribe("$JS.API.CONSUMER.CREATE."+stream+".>", hold.creates)
	if err != nil {
		t.Fatalf("follow the consumer creates on %s: %v", stream, err)
	}
	if err := conn.FlushWithContext(Context(t)); err != nil {
		t.Fatalf("follow the consumer creates on %s: %v", stream, err)
	}
	heartbeat := &natsgo.Msg{
		Subject: info.Config.DeliverSubject,
		Header: natsgo.Header{
			// What nats.go reads a status line as (DecodeHeadersMsg), and the sequences the
			// server's own idle heartbeat carries (nats-server consumer.go, sendIdleHeartbeat).
			"Status":             []string{"100"},
			"Description":        []string{"Idle Heartbeat"},
			"Nats-Last-Consumer": []string{strconv.FormatUint(info.Delivered.Consumer, 10)},
			"Nats-Last-Stream":   []string{strconv.FormatUint(info.Delivered.Stream, 10)},
		},
	}
	if err := js.DeleteConsumer(stream, consumer); err != nil {
		t.Fatalf("delete the consumer %s on %s: %v", consumer, stream, err)
	}
	go func() {
		defer close(hold.done)
		ticker := time.NewTicker(heartbeatEvery)
		defer ticker.Stop()
		for {
			// Its error is not the hold's to report: once the client under test has gone, a
			// heartbeat reaches nobody, and that is no fault of the test's.
			_ = conn.PublishMsg(heartbeat)
			select {
			case <-hold.stopped:
				return
			case <-ticker.C:
			}
		}
	}()
	t.Cleanup(hold.Release)
	return hold
}

// Release stops the heartbeats. It is safe to call more than once.
func (h *ConsumerHold) Release() {
	h.once.Do(func() {
		close(h.stopped)
		<-h.done
	})
}

// Replaced says whether, since the hold began, the client asked the server to put a consumer in the
// lost one's place. nats.go's ordered consumer makes its replacement from the lost one's own
// configuration, changing only where it delivers and where it starts (js.go, resetOrderedConsumer):
// by start sequence, at the sequence after the last it was delivered, with the same filter. So a
// create counts only where it carries all three: another consumer made on the stream by start
// sequence, a scan's or a test's own, is not this one's replacement. It first waits for every create
// the server sent before its call, so none is left unread.
func (h *ConsumerHold) Replaced(t testing.TB) bool {
	t.Helper()
	// nats.go's flush takes only a context that carries a deadline. Context gives it the shared
	// wait's (testwait.Context), and under -timeout 0, where that one has none, shows it the farthest
	// deadline, which bounds nothing a test reaches: the flush ends on the server's answer, or where
	// the shared wait gives up, and decides nothing a server that answers late would.
	if err := h.conn.FlushWithContext(Context(t)); err != nil {
		t.Fatalf("read the consumer creates on %s: %v", h.stream, err)
	}
	for {
		select {
		case msg := <-h.creates:
			var request struct {
				Config struct {
					DeliverPolicy  string   `json:"deliver_policy"`
					OptStartSeq    uint64   `json:"opt_start_seq"`
					FilterSubject  string   `json:"filter_subject"`
					FilterSubjects []string `json:"filter_subjects"`
				} `json:"config"`
			}
			if err := json.Unmarshal(msg.Data, &request); err != nil {
				t.Fatalf("read a consumer create on %s (%s): %v", h.stream, msg.Subject, err)
			}
			config := request.Config
			if config.DeliverPolicy == "by_start_sequence" && config.OptStartSeq == h.startSeq &&
				config.FilterSubject == h.filter && slices.Equal(config.FilterSubjects, h.filters) {
				h.replaced++
			}
		default:
			return h.replaced > 0
		}
	}
}

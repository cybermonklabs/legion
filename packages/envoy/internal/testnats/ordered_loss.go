package testnats

import (
	"strconv"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// heartbeatEvery is how often HoldConsumerLost sends its idle heartbeat: well inside nats.go's
// heartbeat check, two 5 s heartbeat intervals for an ordered consumer (nats.go v1.53.1 js.go,
// orderedHeartbeatsInterval and hbcThresh), so a check never finds a whole interval without one.
const heartbeatEvery = 500 * time.Millisecond

// HoldConsumerLost deletes the ordered consumer named consumer on stream, as a server restart loses
// one, and keeps its client from putting a new one in its place until the returned func runs or the
// test ends. nats.go replaces an ordered consumer only once a whole heartbeat check passes in which
// the consumer's subscription heard nothing (js.go, activityCheck), so a test that deletes one and
// then waits, or runs a shutdown that takes its time, meets nats.go's replacement 5 to 20 s on, and
// later still on a loaded machine, rather than the loss it meant to test. HoldConsumerLost sends the
// subscription, on the consumer's deliver subject, the idle heartbeat the server would, every
// heartbeatEvery: the sequences it carries are the consumer's own as it last reported them, so
// nats.go finds no gap, marks the subscription active and resets nothing. A stop of that
// subscription then deletes the consumer it still names and finds it gone, however long the code
// under test takes to get there.
//
// The heartbeats are published through conn, a connection of the test's own. A consumer that
// delivered a message after its info was read would carry a later sequence than the heartbeat, and
// nats.go would then reset it, so the bucket must be idle: no write lands on it while the hold
// runs.
func HoldConsumerLost(t testing.TB, conn *natsgo.Conn, stream, consumer string) func() {
	t.Helper()
	js, err := conn.JetStream()
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
	stopped := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(heartbeatEvery)
		defer ticker.Stop()
		for {
			// Its error is not the hold's to report: once the client under test has gone, a
			// heartbeat reaches nobody, and that is no fault of the test's.
			_ = conn.PublishMsg(heartbeat)
			select {
			case <-stopped:
				return
			case <-ticker.C:
			}
		}
	}()
	var once sync.Once
	release := func() {
		once.Do(func() {
			close(stopped)
			<-done
		})
	}
	t.Cleanup(release)
	return release
}

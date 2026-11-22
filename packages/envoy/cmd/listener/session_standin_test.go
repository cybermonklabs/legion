package main

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A session stand-in that its own test closes while it is answering a frame reports nothing: the
// test is over, and the reply has no session left to reach. A frame the listener delivers just as a
// subtest ends meets the cleanup that closes the stand-in's connection, as the kept control send of
// TestADispatchMessageForARoleReachesTheSessionThatTakesTheRole/(9) reaches its holder after the
// subtest's last check; the reply's publish or flush then fails with nats.ErrConnectionClosed, and
// a stand-in that reported that failed a subtest whose checks had all passed. Here the frame's
// answer is held until the stand-in's connection has closed, so the reply always meets it closed.
func TestAStandInClosedWhileItAnswersAFrameReportsNothing(t *testing.T) {
	url := testnats.URL(t)
	const sessionID = "ses_standin_closed_while_answering"
	answering, release := make(chan struct{}), make(chan struct{})
	var standIn *sessionStandIn
	passed := t.Run("the stand-in's own test", func(t *testing.T) {
		// Registered before the stand-in's own cleanup, so it runs after it: once the stand-in's
		// connection is closed, it lets the held answer reply and waits for the frame it answered.
		// A stand-in that never started has no frame to wait for.
		t.Cleanup(func() {
			close(release)
			if standIn == nil {
				return
			}
			select {
			case <-standIn.frames:
			case <-time.After(10 * time.Second):
				t.Error("the stand-in never finished answering the frame")
			}
		})
		standIn = startSessionStandIn(t, url, sessionID, func(relayedFrame) []string {
			close(answering)
			<-release
			return []string{relayReplyReceived, relayReplyDone}
		})
		sender := testnats.Connect(t, url)
		defer sender.Close()
		frame := natsgo.NewMsg(bus.DeliverSubject(sessionID))
		frame.Reply = sender.NewRespInbox()
		frame.Header.Set(relayHeaderStreamSeq, "1")
		frame.Data = []byte("{}")
		if err := sender.PublishMsg(frame); err != nil {
			t.Fatalf("send the stand-in a frame: %v", err)
		}
		select {
		case <-answering:
		case <-time.After(10 * time.Second):
			t.Fatal("the stand-in never started answering the frame")
		}
	})
	if !passed {
		t.Fatal("a stand-in its own test closed while it answered a frame failed that test")
	}
}

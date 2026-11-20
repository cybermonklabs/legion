package main

// The settle loop's own correction can go unanswered too. Here a registration's consumer call is
// held past its budget while another listener stores a newer row, so the late call lands a plan
// nothing stored, and the listener's correction after that landing - the second consumer create
// or update on its session-consumer connection - is itself not answered within the fence's
// budget. An attempt whose write's answer did not come back settles nothing, whatever its later
// read of the bucket says, since the server may apply that write after the read
// (filterWriteUnanswered): the settle loop reads the consumer again a pause on and writes it
// again. So the filter ends on the row the bucket holds however the correction ends:
//
//   - correction-applied-late (control): it lands 5.5 s on, as the registration's own call does;
//   - correction-refused-in-time (control): the server refuses it at once;
//   - correction-refused-late: the server refuses it 5.5 s on;
//   - correction-dropped: the server never answers it, as nats-server v2.15.1 drops the requests
//     queued once its JetStream API queue reaches its limit (server/jetstream_api.go, apiDispatch,
//     queue.drain()).
//
// A refusal is made by renaming the stream in the request's subject, so the server answers
// "stream not found".

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

func refuseFrame(f []byte) []byte {
	return bytes.Replace(f, []byte("CONSUMER.CREATE."+bus.Stream+"."), []byte("CONSUMER.CREATE.X"+bus.Stream[1:]+"."), 1)
}

func dropFrame([]byte) []byte { return nil }

func keepFrame(f []byte) []byte { return f }

func TestASettleWhoseCorrectionIsNotAnsweredInTimeLeavesNoUnstoredPlan(t *testing.T) {
	for _, c := range []struct {
		name   string
		second func([]byte) []byte
	}{
		{"correction-applied-late", func(f []byte) []byte { time.Sleep(unansweredWriteHold); return keepFrame(f) }},
		{"correction-refused-in-time", refuseFrame},
		{"correction-refused-late", func(f []byte) []byte { time.Sleep(unansweredWriteHold); return refuseFrame(f) }},
		{"correction-dropped", dropFrame},
	} {
		t.Run(c.name, func(t *testing.T) {
			uri := testnats.URL(t)
			relay := startFrameRelay(t, uri, "$JS.API.CONSUMER.CREATE.")
			client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			a := newSessionConsumerRigOn(t, client, relay.url)
			b := newSessionConsumerRig(t)
			sessionID := "ses_unanswered_correction_" + strings.ReplaceAll(c.name, "-", "_")
			const stored = "notifications.dispatch.issue.CORRECTIONSTORED.>"
			const planned = "notifications.dispatch.issue.CORRECTIONPLANNED.>"
			const newer = "notifications.dispatch.issue.CORRECTIONNEWER.>"
			b.subscribe(t, consumerRegistration(sessionID, "epoch-1", stored))
			// The registration's own call: held past its budget while another listener stores a
			// newer row, then landed. The correction after it is the frame behind it.
			first := relay.next()
			relay.on(first, holdFor(unansweredWriteHold, func() {
				b.subscribe(t, consumerRegistration(sessionID, "epoch-1", stored, newer))
			}))
			relay.on(first+1, c.second)
			began := time.Now()
			recorder := httptest.NewRecorder()
			a.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe",
				strings.NewReader(consumerRegistration(sessionID, "epoch-1", planned))))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("the registration answered %d, want 503: %s", recorder.Code, recorder.Body.String())
			}
			for want := 1; want <= 2; want++ {
				select {
				case n := <-relay.acted:
					t.Logf("consumer write %d released at %s", n, time.Since(began).Round(time.Millisecond))
				case <-time.After(30 * time.Second):
					t.Fatalf("consumer write %d never reached the relay:\n%s", want, a.harness.logs.String())
				}
			}
			// The filter is the stored row's once the listener's work has ended: for a dropped
			// correction that is the stop's end of the wait for its answer, so the case waits for
			// the loop to settle and then stops that work.
			deadline := time.Now().Add(2 * (filterSettlePause + sessionConsumerCallTimeout))
			for !a.filtersAre(t, sessionID, stored, newer) || len(settleLines(t, a, sessionID, settleSettledLine)) == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("the consumer of %s filters on %v %s after the correction was released, want the stored row (%s, %s), with the loop settled (%d settled lines):\n%s",
						sessionID, a.filters(t, sessionID), time.Since(began).Round(time.Millisecond), stored, newer,
						len(settleLines(t, a, sessionID, settleSettledLine)), a.harness.logs.String())
				}
				time.Sleep(50 * time.Millisecond)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a.deps.consumers.awaitAfterAnswer(ctx)
			requireFilteredOnStoredRow(t, a, sessionID)
		})
	}
}

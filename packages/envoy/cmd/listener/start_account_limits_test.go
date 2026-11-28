package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// streamLimitAccount is the configuration of a server whose LISTENER account, the listener's user's
// (asListenerUser), may hold accountStreamLimit streams (JetStream's max_streams): more than every
// stream and bucket a listener with no GitHub route creates, so a pod with the account to itself
// serves.
const streamLimitAccount = `jetstream {}
accounts {
  LISTENER: {
    jetstream: { max_streams: 20 }
    users: [ { user: listener, password: listener } ]
  }
}
`

// accountStreamLimit is streamLimitAccount's max_streams.
const accountStreamLimit = 20

// maximumStreamsCode is the JetStream API error code of a create in an account at its stream limit
// (JSMaximumStreamsLimitErr), as the start's lines carry it.
const maximumStreamsCode = float64(10027)

// accountAtItsStreamLimit starts a server of streamLimitAccount and fills n of its account's
// stream slots with streams of the test's own, each capturing a subject of its own. It returns the
// listener user's URL and a JetStream context in the account, which the test frees slots through
// (freeStreams).
func accountAtItsStreamLimit(t *testing.T, n int) (string, natsgo.JetStreamContext, []string) {
	t.Helper()
	_, uri := testnats.StartConfig(t, streamLimitAccount, []string{"-js"}, false)
	uri = asListenerUser(t, uri)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream in the listener's account: %v", err)
	}
	var fillers []string
	for i := range n {
		name := fmt.Sprintf("FILLER_%d", i)
		if _, err := js.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{fmt.Sprintf("filler.%d", i)}}); err != nil {
			t.Fatalf("fill the account's stream slot %d of %d: %v", i+1, n, err)
		}
		fillers = append(fillers, name)
	}
	return uri, js, fillers
}

// freeStreams deletes streams from the account, freeing as many of its stream slots.
func freeStreams(t *testing.T, js natsgo.JetStreamContext, streams []string) {
	t.Helper()
	for _, name := range streams {
		if err := js.DeleteStream(name); err != nil {
			t.Fatalf("free the account's slot held by %s: %v", name, err)
		}
	}
}

// awaitAccountRefusals waits, keeping listener up and not ready while it does, until listener has
// logged line at ERROR for the account's stream limit at least twice with attributes holding where,
// and answers those lines. Each is checked for the code, the server's text and the wait of the
// start's backoff, 1 s and then 2 s (waitOutAccountState).
func awaitAccountRefusals(t *testing.T, listener *listenerProcess, line string, where map[string]any, while string) []map[string]any {
	t.Helper()
	where["level"], where["code"] = "ERROR", maximumStreamsCode
	refusals := func() []map[string]any { return linesIn(listener.Output.String(), line, where) }
	testwait.WithinPolling(t, testwait.Bounded(t, time.Hour, 30*time.Second), 200*time.Millisecond,
		fmt.Sprintf("the listener to log its second %q %v while %s", line, where, while), func() bool {
			requireLiveAndStarting(t, listener, while)
			return len(refusals()) >= 2
		})
	found := refusals()
	for i, want := range []string{"1s", "2s"} {
		if text, _ := found[i]["error"].(string); found[i]["retry_in"] != want || !strings.Contains(text, "maximum number of streams reached") {
			t.Fatalf("the listener's refusal %d = %v, want the server's \"maximum number of streams reached\" and retry_in %s", i+1, found[i], want)
		}
	}
	requireLiveAndStarting(t, listener, while)
	return found
}

// An account at its JetStream stream limit (max_streams) refuses the create of the notification
// stream at the listener's connect (JetStream code 10027). The limit is the account's state, which
// an operator who raises it or a stream deleted lifts with no change to the listener, and every pod
// of the machine shares the account, so the start rule keeps the pod up and not ready, /livez 204
// and /healthz 503, logs streamAccountStateLine at ERROR naming the stream, the code and the
// server's text at each try, 1 s and then 2 s apart (bus.AccountStateAnswer, connectListener), and
// the pod serves once slots are freed. Under the former rule the start exited 1 at the first
// refusal.
func TestAListenerWhoseAccountIsAtItsStreamLimitWaitsForASlotForTheStreamThenServes(t *testing.T) {
	t.Parallel()
	uri, js, fillers := accountAtItsStreamLimit(t, accountStreamLimit)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "stream-account-limit")
	awaitAccountRefusals(t, listener, streamAccountStateLine, map[string]any{"stream": bus.Stream},
		"its account's stream limit refused the notification stream")
	freeStreams(t, js, fillers)
	listener.waitHealthy(t)
}

// An account at its JetStream stream limit refuses the create of a bucket at the listener's start
// (JetStream code 10027), here with one slot left, which the notification stream takes. The
// spent-proof bucket comes first: its open is refused (spentProofBucketAccountStateLine), and once
// two slots are freed, it and the listener state bucket open, and the interest registry's open is
// refused next (storeBucketAccountStateLine, naming its buckets). Each refusal is the account's
// state, which no setting of the listener changes, so the start rule keeps the pod up and not
// ready, /livez 204 and /healthz 503, logging its line at ERROR with the code and the server's text
// at each try, 1 s and then 2 s apart (bus.AccountStateAnswer, openSpentProofBucket,
// openStartStore), and the pod serves once every slot it needs is freed. Under the former rule the
// start exited 1 at the spent-proof bucket's first refusal.
func TestAListenerWhoseAccountIsAtItsStreamLimitWaitsForSlotsForItsBucketsThenServes(t *testing.T) {
	t.Parallel()
	uri, js, fillers := accountAtItsStreamLimit(t, accountStreamLimit-1)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "bucket-account-limit")
	awaitAccountRefusals(t, listener, spentProofBucketAccountStateLine, map[string]any{"bucket": bus.DPoPProofBucket},
		"its account's stream limit refused the spent-proof bucket")
	freeStreams(t, js, fillers[:2])
	awaitAccountRefusals(t, listener, storeBucketAccountStateLine, map[string]any{"buckets": bus.InterestBucket + "," + bus.RoleClaimBucket},
		"its account's stream limit refused the interest registry's buckets")
	freeStreams(t, js, fillers[2:])
	listener.waitHealthy(t)
}

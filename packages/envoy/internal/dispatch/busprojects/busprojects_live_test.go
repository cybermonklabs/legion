package busprojects_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/dispatch/busprojects"
	"github.com/sjawhar/envoy/internal/dispatch/busprojects/busprojectstest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// The tests in this file run the proof against nats-server in this process with an auth callout
// (busprojectstest.StartHub), whose stand-in auth service admits Dispatch's hub-admission proof
// only on a hub that carries the private-projects server tag, as the auth service's private-hub
// admission does. The switch to private that calls Prove, and answers 409 on its error, is
// Dispatch's visibility route; its own tests run the same hubs.

// TestAPrivateSwitchNeedsEveryHubsAdmission: the proof passes only while both hubs carry the
// server tag. With the second hub restarted without it, the proof answers that hub's refusal, and
// its auth service refused because the request carried no tag.
//
// Control: make Prove stop after the first hub (prove a.hubs[:1]); the proof then passes with the
// second hub untagged, and the test fails naming the second hub.
func TestAPrivateSwitchNeedsEveryHubsAdmission(t *testing.T) {
	first, second := busprojectstest.StartHub(t, true), busprojectstest.StartHub(t, true)
	a, clients := admissionOver(t, first, second)
	for _, client := range clients {
		awaitConnected(t, client)
	}
	a.SetListeners(busprojectstest.ReadyListener(t))
	require.NoError(t, a.Prove(t.Context()), "both hubs carry the tag")
	require.True(t, a.Ready())

	second.Retag(t, false)
	err := a.Prove(t.Context())
	require.Error(t, err, "the proof passed with the second hub, %s, rendered without the server tag", second.URL)
	require.ErrorContains(t, err, "hub "+second.URL+" did not admit a new proof connection")
	require.NotContains(t, err.Error(), "hub "+first.URL)
	require.False(t, a.Ready())
	require.Contains(t, second.Refusals(), busprojectstest.RefusedUntagged, "the second hub refused for another reason: %v", second.Refusals())
	report := a.Report()
	require.Equal(t, busprojects.AdmissionAdmitted, report.Hubs[0].Admission)
	require.Equal(t, busprojects.AdmissionRefused, report.Hubs[1].Admission)
}

// TestAProofNeedsAFreshCalloutDecision: once both hubs have admitted Dispatch's held connections,
// the auth service stops. Each held connection still answers a PING, since a hub decides a
// connection once, when it connects; the proof, which dials a new connection on each hub, fails
// naming the first hub, since no auth service decides those connections now.
//
// Control: make bus.Client.ProveAdmission check the held connection with a PING
// (c.Connection().FlushTimeout) in place of dialing; the proof passes, and the test fails naming
// the first hub.
func TestAProofNeedsAFreshCalloutDecision(t *testing.T) {
	first, second := busprojectstest.StartHub(t, true), busprojectstest.StartHub(t, true)
	a, clients := admissionOver(t, first, second)
	for _, client := range clients {
		awaitConnected(t, client)
	}
	a.SetListeners(busprojectstest.ReadyListener(t))
	require.NoError(t, a.Prove(t.Context()))

	first.StopAuthService(t)
	second.StopAuthService(t)
	for i, client := range clients {
		require.NoError(t, client.Connection().FlushTimeout(5*time.Second), "hub %d's held connection answers no PING with the auth service stopped", i+1)
	}
	err := a.Prove(t.Context())
	require.Error(t, err, "the proof passed with no auth service deciding a new connection on %s", first.URL)
	firstLine, _, _ := strings.Cut(err.Error(), "\n")
	require.Contains(t, firstLine, "hub "+first.URL+" did not admit a new proof connection", "the proof's error does not name the first hub first: %v", err)
	require.ErrorContains(t, err, "hub "+second.URL)
	require.False(t, a.Ready())
	for i, client := range clients {
		require.True(t, client.Connected(), "the proof touched hub %d's held connection", i+1)
	}
}

// TestRunProvesAgainWhenAHubsConnectionChanges: Run proves at once and again at each connection
// event of a hub's client, not only on its period. With the period an hour, Run's first proof, and
// the one SetListeners asked for, make the admission ready; both are kept before the hub changes,
// so no proof is pending then. The hub's restart without the server tag drops the held connection,
// and Run proves again at that event, so the admission turns not ready within seconds. The hub's
// refusals cannot show which proof ran: the held client's own reconnects are refused for the tag
// too.
//
// Control: make Run start no follower of the hubs' connections (drop the go a.follow line); the
// admission stays ready for the hour, and the test fails at its wait.
func TestRunProvesAgainWhenAHubsConnectionChanges(t *testing.T) {
	hub := busprojectstest.StartHub(t, true)
	a, clients := admissionOver(t, hub)
	awaitConnected(t, clients[0])
	busprojects.SetPeriod(a, time.Hour)
	a.SetListeners(busprojectstest.ReadyListener(t))
	ctx, cancel := context.WithCancel(t.Context())
	var running sync.WaitGroup
	running.Go(func() { a.Run(ctx) })
	t.Cleanup(func() { cancel(); running.Wait() })

	require.Eventually(t, func() bool { return a.Ready() && busprojects.KeptProofs(a) >= 2 }, 30*time.Second, 20*time.Millisecond,
		"Run's first proof and the one SetListeners asked for never both ended ready")
	hub.Retag(t, false)
	require.Eventually(t, func() bool { return !a.Ready() }, 30*time.Second, 20*time.Millisecond,
		"the hub's connection changed and Run did not prove again: the admission is still ready with the hub untagged")
}

// TestTheProofConnectionHoldsNothing: a connection admitted for the proof holds only its own reply
// inboxes and its ping subject: a subscribe to any other subject is refused, and so is a publish to
// any subject but its ping, JetStream's API, a KV bucket, the shared reply inboxes and project
// subjects among them, each tried and refused in turn. A token of another account, and Dispatch's
// token for another audience, are refused the proof.
//
// The grant here is the stand-in auth service's (busprojectstest), which names the reply inboxes
// and the ping subject the auth service's hub-admission grant names; run against the auth
// service's private-hub admission in its place, this test holds that grant to the same.
func TestTheProofConnectionHoldsNothing(t *testing.T) {
	hub := busprojectstest.StartHub(t, true)
	var mu sync.Mutex
	var violations []string
	conn, err := natsgo.Connect(hub.URL, natsgo.UserInfo(natscred.ServiceUser, proofToken()), natsgo.NoReconnect(), natsgo.Timeout(5*time.Second),
		natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
			mu.Lock()
			violations = append(violations, err.Error())
			mu.Unlock()
		}))
	require.NoError(t, err, "the proof connection is admitted on a tagged hub")
	t.Cleanup(conn.Close)
	// refused waits, within 5 s, for the server's refusal of direction ("Subscription" or
	// "Publish") to subject, as nats-server words it.
	refused := func(t *testing.T, direction, subject string) bool {
		t.Helper()
		want := "Permissions Violation for " + direction + ` to "` + subject + `"`
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			for _, v := range violations {
				if strings.Contains(v, want) {
					mu.Unlock()
					return true
				}
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}

	for _, subject := range []string{"notifications.dispatch.issue.PRIV.PRIV-1.>", "notifications.agent.PRIV.>", "$KV.envoy_roles.>", "$JS.API.>", "_INBOX.>", ">"} {
		t.Run("subscribe "+subject, func(t *testing.T) {
			_, err := conn.SubscribeSync(subject)
			require.NoError(t, err)
			require.NoError(t, conn.Flush())
			require.True(t, refused(t, "Subscription", subject), "the proof connection subscribed %s", subject)
		})
	}
	for _, subject := range []string{"notifications.dispatch.issue.PRIV.PRIV-1.created", "notifications.agent.PRIV.example-session", "$JS.API.INFO", "$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS", "$KV.envoy_roles.example-role", "_INBOX.example", busprojectstest.ProofInboxPrefix + ".example"} {
		t.Run("publish "+subject, func(t *testing.T) {
			require.NoError(t, conn.Publish(subject, []byte("example")))
			require.NoError(t, conn.Flush())
			require.True(t, refused(t, "Publish", subject), "the proof connection published to %s", subject)
		})
	}
	t.Run("its own reply inbox and its ping are its grant", func(t *testing.T) {
		mu.Lock()
		before := len(violations)
		mu.Unlock()
		_, err := conn.SubscribeSync(busprojectstest.ProofInboxPrefix + ".example")
		require.NoError(t, err)
		require.NoError(t, conn.Publish(busprojectstest.ProofPingSubject, []byte("example")))
		require.NoError(t, conn.Flush())
		require.NoError(t, conn.FlushTimeout(5*time.Second))
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, violations, before, "the grant refused its own inbox or ping: %v", violations[before:])
	})

	for name, token := range map[string]string{
		"another account's proof token":             busprojectstest.ProofToken("system:serviceaccount:legion:example-daemon", natsgrant.HubAdmissionAudience),
		"Dispatch's token for a service's audience": busprojectstest.ProofToken(busprojectstest.DispatchAccount, "envoy-nats"),
	} {
		t.Run(name, func(t *testing.T) {
			other, err := natsgo.Connect(hub.URL, natsgo.UserInfo(natscred.ServiceUser, token), natsgo.NoReconnect(), natsgo.Timeout(5*time.Second))
			if other != nil {
				other.Close()
			}
			require.Error(t, err, "%s was admitted for the proof", name)
			require.True(t, errors.Is(err, natsgo.ErrAuthorization) || strings.Contains(strings.ToLower(err.Error()), "authorization"), "%v", err)
		})
	}
	require.Contains(t, hub.Refusals(), busprojectstest.RefusedNotDispatch)
	require.Contains(t, hub.Refusals(), busprojectstest.RefusedNotAProof)
}

// TestTheProofClientLogsNoPermissionsViolation: the hub-admission client stays inside the proof's
// grant, so the hub refuses it nothing and the client logs no permissions violation: not at
// connect, where a client that reads the stream's subjects publishes a request the grant refuses,
// not on a request of its own, whose reply inbox the grant admits only under its own prefix, and
// not on a proof. Every log line the client's connection queued before a forced reconnect has been
// written once its "reconnected" line is, since nats.go runs those callbacks in order.
//
// Controls, each run on its own: build the client without bus.WithInboxPrefix (nats.go's default
// _INBOX); its request's inbox subscription is refused and logged. Build it without
// bus.WithoutStreamSubjects; the stream's subjects read at connect is refused and logged.
func TestTheProofClientLogsNoPermissionsViolation(t *testing.T) {
	var recorder logtest.Recorder
	logtest.SetDefault(t, &recorder)
	hub := busprojectstest.StartHub(t, true)
	a, clients := admissionOver(t, hub)
	awaitConnected(t, clients[0])
	a.SetListeners(busprojectstest.ReadyListener(t))
	require.NoError(t, a.Prove(t.Context()))

	conn := clients[0].Connection()
	_, err := conn.Request(busprojectstest.ProofPingSubject, []byte("example"), time.Second)
	require.True(t, errors.Is(err, natsgo.ErrNoResponders) || errors.Is(err, natsgo.ErrTimeout), "the ping request: %v", err)
	require.NoError(t, conn.FlushTimeout(5*time.Second))
	require.NoError(t, conn.ForceReconnect())
	logged := func(message string) bool {
		return slices.ContainsFunc(recorder.Records(), func(r slog.Record) bool { return r.Message == message })
	}
	require.Eventually(t, func() bool { return logged("envoy nats reconnected") }, 30*time.Second, 20*time.Millisecond, "the forced reconnect was never logged")

	for _, record := range recorder.Records() {
		record.Attrs(func(attr slog.Attr) bool {
			require.NotContains(t, attr.Value.String(), "Permissions Violation", "the proof client logged %q: %s", record.Message, attr.Value.String())
			return true
		})
	}
}

package busprojects_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/busprojects"
	"github.com/sjawhar/envoy/internal/dispatch/busprojects/busprojectstest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testsocks5"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

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
// subjects among them, each tried and refused in turn. A proof token of another account, and
// Dispatch's token for an audience the auth service takes no NATS token at, are refused the proof.
//
// The grant here is the stand-in auth service's (busprojectstest), which stands in for the auth
// service's hub-admission grant, not written at this head; run against the auth service's
// private-hub admission in its place, this test holds that grant to the same. Both refusals are
// ones the auth service makes too (natsgrant.HubAdmissionAudience). Dispatch's token for the
// services' own audience (BROKER_NATS_K8S_AUDIENCE) has no row: the auth service admits it as an
// ordinary service connection on every hub, so its refusal here would be the stand-in's alone,
// which admits nothing but proofs.
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
		"another account's proof token":                     busprojectstest.ProofToken("system:serviceaccount:legion:example-daemon", natsgrant.HubAdmissionAudience),
		"Dispatch's token for an audience of no NATS token": busprojectstest.ProofToken(busprojectstest.DispatchAccount, "example-not-a-nats-audience"),
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

// TestAHubWithNoAuthenticationRefusesTheProof: a hub that requires no authentication admits every
// connection without asking an auth service, Dispatch's held connection included, so its
// admission decides nothing: the proof refuses it, naming the hub, and the report reads it
// refused.
//
// Control: drop bus.Client.ProveAdmission's AuthRequired check; the open hub admits the proof
// connection, the proof passes, and the test fails naming the hub.
func TestAHubWithNoAuthenticationRefusesTheProof(t *testing.T) {
	_, open := testnats.Start(t)
	a, clients := admissionOverURLs(t, open)
	awaitConnected(t, clients[0])
	a.SetListeners(busprojectstest.ReadyListener(t))

	err := a.Prove(t.Context())
	require.Error(t, err, "the proof passed on %s, a hub that requires no authentication", open)
	require.ErrorContains(t, err, "hub "+open+" did not admit a new proof connection")
	require.ErrorContains(t, err, "the server requires no authentication")
	require.False(t, a.Ready())
	require.Equal(t, busprojects.AdmissionRefused, a.Report().Hubs[0].Admission)
}

// TestAHubClientNamingNoServerFailsTheProof: a hub client that names no server, or a blank one,
// cannot be built, so no admission holds it and no proof can pass on it: bus.Connect refuses it
// before any dial, where nats.go would dial its default server, port 4222 on this machine, for it.
//
// Control: drop bus.Connect's refusal of such a list (refuseNoServer in newClient); the client is
// built, dialing nats.go's default in the background, and the test fails naming the list.
func TestAHubClientNamingNoServerFailsTheProof(t *testing.T) {
	for name, servers := range map[string][]string{"no server": nil, "a blank server": {" "}} {
		t.Run(name, func(t *testing.T) {
			client, err := busprojectstest.NewProofClient(t, servers, proofToken())
			require.ErrorIs(t, err, bus.ErrNoNATSServer, "a hub-admission client was built for %q", servers)
			require.Nil(t, client)
		})
	}
}

// TestEveryServerOfAHubClientAdmitsTheProof: a hub client that names two servers, one of them
// rendered without the server tag, fails the proof naming that server, whichever comes first:
// each server is proven on a connection of its own, so the tagged server's admission never stands
// for the untagged one's, and the untagged one's refusal never moves the proof on to the tagged
// one.
//
// Control: make bus.Client.ProveAdmission dial one connection over all the client's servers; with
// the tagged server first it connects there alone, with the untagged one first it moves on to the
// tagged one after the refusal, the proof passes either way, and both rows fail.
func TestEveryServerOfAHubClientAdmitsTheProof(t *testing.T) {
	tagged, untagged := busprojectstest.StartHub(t, true), busprojectstest.StartHub(t, false)
	for _, row := range []struct {
		name    string
		servers []*busprojectstest.Hub
	}{
		{"the tagged server first", []*busprojectstest.Hub{tagged, untagged}},
		{"the untagged server first", []*busprojectstest.Hub{untagged, tagged}},
	} {
		t.Run(row.name, func(t *testing.T) {
			a, _ := admissionOverURLs(t, row.servers[0].URL+","+row.servers[1].URL)
			a.SetListeners(busprojectstest.ReadyListener(t))
			err := a.Prove(t.Context())
			require.Error(t, err, "the proof passed with %s, one of the hub's two servers, rendered without the server tag", untagged.URL)
			require.ErrorContains(t, err, "admission proof: "+untagged.URL+": the new connection was not admitted")
			require.False(t, a.Ready())
			require.Contains(t, untagged.Refusals(), busprojectstest.RefusedUntagged, "the untagged server refused for another reason: %v", untagged.Refusals())
		})
	}
}

// TestAProofNeverDialsAServerTheHubNames: a hub client naming one server, which refuses the proof
// and names a second server in its INFO, as a server that advertises its cluster's other servers
// does, fails the proof: the proof connection never moves to a server its client does not name,
// so the second server, which would admit it, is never asked.
//
// Control: drop IgnoreDiscoveredServers from bus.Client.ProveAdmission's connection; nats.go adds
// the named server to the connection's servers during the refused handshake and connects there,
// the proof passes, and the test fails naming the refusing server.
func TestAProofNeverDialsAServerTheHubNames(t *testing.T) {
	tagged := busprojectstest.StartHub(t, true)
	refusing, refused := refusingServer(t, tagged.URL)
	a, _ := admissionOverURLs(t, refusing)
	a.SetListeners(busprojectstest.ReadyListener(t))

	err := a.Prove(t.Context())
	require.Error(t, err, "the proof passed through %s, which %s named, after %s refused it", tagged.URL, refusing, refusing)
	require.ErrorContains(t, err, "admission proof: "+refusing+": the new connection was not admitted")
	require.ErrorIs(t, err, natsgo.ErrAuthorization)
	require.False(t, a.Ready())
	require.Positive(t, refused(), "the refusing server refused no connection, so it never answered the proof")
}

// TestACancelledProofIsNotKept: a proof whose caller ended before it did answers the caller's
// error and is not kept: Ready and the report keep the last proof that ran to its end, here one
// that passed, rather than a refusal no hub made.
//
// Control: make Prove keep its result whatever its caller's context says; the cancelled proof is
// kept as every hub and replica failing, the admission turns not ready, and the test fails.
func TestACancelledProofIsNotKept(t *testing.T) {
	hub := busprojectstest.StartHub(t, true)
	a, clients := admissionOver(t, hub)
	awaitConnected(t, clients[0])
	a.SetListeners(busprojectstest.ReadyListener(t))
	require.NoError(t, a.Prove(t.Context()))
	require.True(t, a.Ready())
	passed := a.Report()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	err := a.Prove(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, a.Ready(), "a proof its caller cancelled was kept: %v", err)
	require.Equal(t, passed, a.Report(), "the report holds a proof its caller cancelled")
}

// refusingServer is a stand-in NATS server at a loopback address of its own that answers each
// client with an INFO requiring authentication and naming advertised, a server's host:port or
// URL, among its cluster's other servers (connect_urls), then refuses the client's CONNECT as a
// hub whose auth service refused it does, with -ERR 'Authorization Violation'. It answers its
// URL and how many clients it has refused; it stops when the test ends.
func refusingServer(t *testing.T, advertised string) (string, func() int) {
	t.Helper()
	if parsed, err := url.Parse(advertised); err == nil && parsed.Host != "" {
		advertised = parsed.Host
	}
	listener, err := testport.ListenLoopback()
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	info := fmt.Sprintf(`INFO {"server_id":"NREFUSING","server_name":"refusing","version":"2.15.1","proto":1,"host":%q,"port":%s,"headers":true,"auth_required":true,"max_payload":1048576,"connect_urls":[%q]}`+"\r\n", host, port, advertised)
	var refused atomic.Int64
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
				if _, err := conn.Write([]byte(info)); err != nil {
					return
				}
				lines := bufio.NewReader(conn)
				for {
					line, err := lines.ReadString('\n')
					if err != nil {
						return
					}
					if strings.HasPrefix(line, "PING") {
						refused.Add(1)
						_, _ = conn.Write([]byte("-ERR 'Authorization Violation'\r\n"))
						return
					}
				}
			}()
		}
	}()
	return "nats://" + listener.Addr().String(), func() int { return int(refused.Load()) }
}

// TestAClusteredHubIsProvedOnEachServerItsEntryNames: a clustered hub's servers each report the
// cluster and their own name, and a cluster's tags are each server's own. The hub's servers are
// reached through a proxy that resolves the cluster's DNS: the Service's name routes to server 0,
// as a Service balancing over the servers picks one, and each per-pod name to its pod's server.
//
//   - An entry that names the Service fails the proof, naming the hub and the server it reached,
//     though that server is tagged: its admission says nothing of the other servers.
//   - An entry that names every pod is proved on each server, each admitting one proof
//     connection.
//   - With one server untagged, as in the middle of a render that rolls the servers one at a
//     time, the per-pod entry fails naming that server's pod.
//
// Control: drop bus.Client.ProveAdmission's cluster check (urlNamesServer); the Service entry's
// proof reaches tagged server 0 and passes, and the first row fails.
func TestAClusteredHubIsProvedOnEachServerItsEntryNames(t *testing.T) {
	const name, namespace = "envoy-hub", "envoy"
	servers := busprojectstest.StartClusteredHub(t, name, true, true, true)
	service := fmt.Sprintf("%s.%s.svc.cluster.local:4222", name, namespace)
	pods := make([]string, len(servers))
	routes := map[string]string{service: hostOfURL(t, servers[0].URL)}
	for i, server := range servers {
		pods[i] = fmt.Sprintf("nats://%s-%d.%s-headless.%s.svc.cluster.local:4222", name, i, name, namespace)
		routes[strings.TrimPrefix(pods[i], "nats://")] = hostOfURL(t, server.URL)
	}
	proxy := testsocks5.StartRouted(t, routes)
	admissionOf := func(t *testing.T, urls ...string) (*busprojects.HubAdmission, *bus.Client) {
		t.Helper()
		client := busprojectstest.ProofClientThrough(t, proxy.Addr, urls, proofToken())
		awaitConnected(t, client)
		a := busprojects.NewHubAdmission([]*bus.Client{client})
		require.NotNil(t, a)
		busprojects.SetLookup(a, resolveWorkload)
		a.SetListeners(busprojectstest.ReadyListener(t))
		return a, client
	}

	t.Run("an entry naming the hub's Service", func(t *testing.T) {
		a, _ := admissionOf(t, "nats://"+service)
		err := a.Prove(t.Context())
		require.Error(t, err, "the proof passed through the Service %s, which reached tagged server %s-0 alone", service, name)
		require.ErrorContains(t, err, "admission proof: nats://"+service+": ")
		require.ErrorContains(t, err, fmt.Sprintf("the URL reached server %q of cluster %q, which it does not name", name+"-0", name))
		require.False(t, a.Ready())
		require.Equal(t, busprojects.AdmissionRefused, a.Report().Hubs[0].Admission)
	})

	t.Run("an entry naming every pod", func(t *testing.T) {
		a, _ := admissionOf(t, pods...)
		before := make([]int, len(servers))
		for i, server := range servers {
			before[i] = server.Admitted()
		}
		require.NoError(t, a.Prove(t.Context()))
		require.True(t, a.Ready())
		for i, server := range servers {
			require.Greater(t, server.Admitted(), before[i], "the proof admitted no connection on server %s-%d, which %s names", name, i, pods[i])
		}
	})

	t.Run("an entry naming every pod, one server untagged", func(t *testing.T) {
		servers[1].Retag(t, false)
		a, _ := admissionOf(t, pods...)
		err := a.Prove(t.Context())
		require.Error(t, err, "the proof passed with server %s-1 untagged", name)
		require.ErrorContains(t, err, "admission proof: "+pods[1]+": ")
		require.Contains(t, servers[1].Refusals(), busprojectstest.RefusedUntagged)
		require.False(t, a.Ready())
	})
}

// hostOfURL is the host:port of the server at raw, a NATS URL.
func hostOfURL(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	return parsed.Host
}

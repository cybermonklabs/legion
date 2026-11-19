package natscallout

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// startService runs the auth service against the server at url with keys' seeds, deps and decide,
// stopped when the test ends, and returns once its subscription has reached the server.
func startService(t *testing.T, url string, keys testnats.CalloutKeys, deps Deps, decide decideFunc) *Service {
	t.Helper()
	dir := t.TempDir()
	write := func(name, seed string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(seed+"\n"), 0o600))
		return path
	}
	settings := Settings{
		URLs:           []string{url},
		SeedFile:       write("service.seed", keys.ServiceSeed),
		IssuerSeedFile: write("issuer.seed", keys.IssuerSeed),
		XKeySeedFile:   write("xkey.seed", keys.XKeySeed),
		Services:       exampleServices,
	}
	service, err := start(t.Context(), settings, deps, decide)
	require.NoError(t, err)
	t.Cleanup(service.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, service.Ready(ctx), "the auth service's subscription never reached the server")
	return service
}

// connectSession connects to url as s, presenting its token and its proof over the nonce the
// server sends this connection, and hands every asynchronous error the server sends it to errs.
func connectSession(t *testing.T, url string, s *session, errs chan<- error) (*nats.Conn, error) {
	t.Helper()
	sign := func(nonce []byte) ([]byte, error) {
		proof, err := brokerclient.ConnectionProof(s.key, s.token, string(nonce))
		return []byte(proof), err
	}
	options := []nats.Option{
		nats.UserJWT(func() (string, error) { return "-", nil }, sign),
		nats.Token(s.token),
		nats.Timeout(10 * time.Second),
		nats.NoReconnect(),
	}
	if errs != nil {
		options = append(options, nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case errs <- err:
			default:
			}
		}))
	}
	nc, err := nats.Connect(url, options...)
	if err == nil {
		t.Cleanup(nc.Close)
	}
	return nc, err
}

// publisher is a static user that may publish every notification topic, as a listener does.
func publisher(t *testing.T) (testnats.StaticUser, string) {
	t.Helper()
	seed, key := testnats.User(t)
	return testnats.StaticUser{NKey: key, Publish: []string{"notifications.>"}, Subscribe: []string{"_INBOX.>"}}, seed
}

// awaitViolation waits for the server's permissions violation naming subject among errs, failing
// the test when it does not come.
func awaitViolation(t *testing.T, errs <-chan error, subject string) {
	t.Helper()
	want := `Permissions Violation for Subscription to "` + subject + `"`
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-errs:
			if strings.Contains(err.Error(), want) {
				return
			}
		case <-deadline:
			t.Fatalf("no %s: the server served the subscription to %s", want, subject)
		}
	}
}

// Through the service, a session's connection receives on its own delivery channel, and its
// subscribe to another session's is refused by the server, by name.
func TestTheHubAdmitsASessionThroughTheService(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	static, staticSeed := publisher(t)
	url := testnats.StartWithCallout(t, keys, static)
	a, b := newSession(t, "ses_a", 15*time.Minute), newSession(t, "ses_b", 15*time.Minute)
	startService(t, url, keys, Deps{Introspect: introspector(a, b)}, Decide)

	errs := make(chan error, 16)
	nc, err := connectSession(t, url, a, errs)
	require.NoError(t, err, "session ses_a was not admitted")
	own, err := nc.SubscribeSync(bus.DeliverSubject(a.id))
	require.NoError(t, err)
	other, err := nc.SubscribeSync(bus.DeliverSubject(b.id))
	require.NoError(t, err)
	require.NoError(t, nc.Flush())

	pub, err := testnats.ConnectSeed(t, url, staticSeed)
	require.NoError(t, err)
	defer pub.Close()
	require.NoError(t, pub.Publish(bus.DeliverSubject(a.id), []byte("for ses_a")))
	require.NoError(t, pub.Publish(bus.DeliverSubject(b.id), []byte("for ses_b")))
	require.NoError(t, pub.Flush())

	msg, err := own.NextMsg(5 * time.Second)
	require.NoError(t, err, "ses_a received nothing on its own channel")
	require.Equal(t, "for ses_a", string(msg.Data))
	awaitViolation(t, errs, bus.DeliverSubject(b.id))
	if served, err := other.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("ses_a received %q on session ses_b's channel", served.Data)
	}
}

// Each authorization request the hub sends is sealed to the service's curve key: a second client
// holding the service's NATS user, but not that key, reads bytes that are no JWT. The control is a
// hub configured with no curve key, whose requests that client reads in the clear, and which the
// service refuses under its own label: a request in the clear means every token and proof crossed
// the wire unsealed.
func TestRequestsAreEncryptedToTheServiceKey(t *testing.T) {
	for _, row := range []struct {
		name   string
		sealed bool
	}{{"sealed to the service's key", true}, {"control: a hub with no curve key", false}} {
		t.Run(row.name, func(t *testing.T) {
			keys := testnats.NewCalloutKeys(t)
			config := keys
			if !row.sealed {
				config.XKeyPublic = ""
			}
			url := testnats.StartWithCallout(t, config)
			a := newSession(t, "ses_a", 15*time.Minute)
			log := &logtest.Buffer{}
			startService(t, url, keys, Deps{Introspect: introspector(a), Log: slog.New(slog.NewTextHandler(log, nil))}, Decide)

			eavesdropper, err := testnats.ConnectSeed(t, url, keys.ServiceSeed)
			require.NoError(t, err)
			defer eavesdropper.Close()
			seen, err := eavesdropper.SubscribeSync(authSubject)
			require.NoError(t, err)
			require.NoError(t, eavesdropper.Flush())

			_, err = connectSession(t, url, a, nil)
			if row.sealed {
				require.NoError(t, err, "the service did not admit the session")
			} else {
				require.ErrorIs(t, err, nats.ErrAuthorization, "the service admitted a session whose request reached it in the clear")
				testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the refusal in the broker's log, at error", func() bool {
					return strings.Contains(log.String(), "level=ERROR") && strings.Contains(log.String(), labelUnsealed+"=1")
				})
			}
			request, err := seen.NextMsg(5 * time.Second)
			require.NoError(t, err)
			clear := strings.HasPrefix(string(request.Data), "eyJ")
			if row.sealed {
				require.False(t, clear, "the request reached a holder of the service's user in the clear: %.20q", request.Data)
				require.NotEmpty(t, request.Header.Get(serverXKeyHeader))
			} else {
				require.True(t, clear, "the control's request is not in the clear: %.20q", request.Data)
			}
		})
	}
}

// A request in the clear is refused under its own label and logged at error whatever else is true
// of it, a request the hub has already given up on included: the label is how an operator learns
// the hub's callout lost its curve key, which a drop would hide. A hub with no curve key sends two
// requests; the second is held past its lifetime, after the first has shown the replica the hub's
// clock, and both are refused as unsealed, none dropped. The first is in the reporter's first
// line; the late one, inside the same minute, is in the line the replica writes as it closes, at
// error too.
func TestAnUnsealedRequestIsRefusedEvenWhenTheHubHasGivenUpOnIt(t *testing.T) {
	target, entered, release := holdDeliveries(t)
	keys := testnats.NewCalloutKeys(t)
	config := keys
	config.XKeyPublic = ""
	url := testnats.StartWithCallout(t, config)
	first, late := newSession(t, "ses_first", 15*time.Minute), newSession(t, "ses_late", 15*time.Minute)
	log := &logtest.Buffer{}
	registry := metrics.New()
	service := startService(t, url, keys, Deps{Introspect: introspector(first, late), Log: slog.New(slog.NewTextHandler(log, nil)), Metrics: NewMetrics(registry)}, Decide)

	_, err := connectSession(t, url, first, nil)
	require.ErrorIs(t, err, nats.ErrAuthorization)
	target.Store(service)
	refused := make(chan error, 1)
	go func() {
		_, err := connectSession(t, url, late, nil)
		refused <- err
	}()
	<-entered
	time.Sleep(7 * time.Second)
	release()
	require.ErrorIs(t, <-refused, nats.ErrAuthorization)
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "both requests refused as unsealed", func() bool {
		return strings.Contains(scrape(t, registry), `outcome="refused",reason="unsealed"} 2`)
	})
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the first request in the clear logged at error", func() bool {
		return strings.Contains(log.String(), "arrived in the clear")
	})
	require.NotContains(t, scrape(t, registry), `reason="hub_gave_up"`, "the request in the clear was dropped")
	service.Close()
	lines := log.Lines("arrived in the clear")
	require.Equal(t, 2, strings.Count(lines, "level=ERROR"), "each request in the clear was not logged at error:\n%s", lines)
	require.Equal(t, 2, strings.Count(lines, "unsealed=1"), "the late request in the clear was not in the closing line:\n%s", lines)
}

// A session's own grant refuses it the authorization subject, and the guard that makes that so is
// the service's, in admit: the control is a stand-in holding the service's keys that answers with
// a grant naming $SYS.>, past it, and the same subscribe is served and receives the next client's
// request.
func TestASessionCannotSubscribeTheAuthSubject(t *testing.T) {
	for _, row := range []struct {
		name   string
		served bool
	}{
		{"the service's decision", false},
		{"control: a stand-in granting $SYS.>, past the guard", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			keys := testnats.NewCalloutKeys(t)
			url := testnats.StartWithCallout(t, keys)
			a, b := newSession(t, "ses_a", 15*time.Minute), newSession(t, "ses_b", 15*time.Minute)
			if row.served {
				standIn := standInGranting(t, url, keys, []string{"_INBOX.>"}, []string{bus.DeliverSubject(a.id), "$SYS.>"})
				defer standIn.Close()
			} else {
				startService(t, url, keys, Deps{Introspect: introspector(a, b)}, Decide)
			}

			errs := make(chan error, 16)
			nc, err := connectSession(t, url, a, errs)
			require.NoError(t, err)
			requests, err := nc.SubscribeSync(authSubject)
			require.NoError(t, err)
			require.NoError(t, nc.Flush())
			if !row.served {
				awaitViolation(t, errs, authSubject)
				return
			}
			_, err = connectSession(t, url, b, nil)
			require.NoError(t, err)
			_, err = requests.NextMsg(5 * time.Second)
			require.NoError(t, err, "the control's grant did not serve the subscribe to %s", authSubject)
		})
	}
}

// Two replicas answer in one queue group, so the server sends each request to one of them: with
// both up, every connection is decided once. With one of them closed, every connection is
// admitted within a second: the closed replica left the group, so the server sends it nothing.
// The control is a replica that stops answering but keeps its place in the group, which the
// server keeps sending requests to: some connection then waits out the authorization timeout.
func TestOneStoppedReplicaDelaysNoConnection(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	sessions := make([]*session, 50)
	for index := range sessions {
		sessions[index] = newSession(t, "ses_"+strings.Repeat("a", index+1), 15*time.Minute)
	}
	var decided atomic.Int64
	introspect := introspector(sessions...)
	deps := Deps{Introspect: func(ctx context.Context, token string) (oauthwire.Introspection, error) {
		decided.Add(1)
		return introspect(ctx, token)
	}}
	stopped := startService(t, url, keys, deps, Decide)
	startService(t, url, keys, deps, Decide)

	for _, s := range sessions[:10] {
		nc, err := connectSession(t, url, s, nil)
		require.NoError(t, err, "%s was not admitted", s.id)
		nc.Close()
	}
	require.EqualValues(t, 10, decided.Load(), "ten connections were decided %d times: the replicas do not share one queue group", decided.Load())
	stopped.Close()

	for _, s := range sessions {
		began := time.Now()
		_, err := connectSession(t, url, s, nil)
		require.NoError(t, err, "%s was not admitted", s.id)
		require.Less(t, time.Since(began), time.Second, "%s waited %s to be admitted", s.id, time.Since(began))
	}

	t.Run("control: a replica that keeps its place and stops answering", func(t *testing.T) {
		hung, err := testnats.ConnectSeed(t, url, keys.ServiceSeed)
		require.NoError(t, err)
		defer hung.Close()
		_, err = hung.QueueSubscribe(authSubject, queueGroup, func(*nats.Msg) {})
		require.NoError(t, err)
		require.NoError(t, hung.Flush())
		var slow atomic.Int32
		var wait sync.WaitGroup
		for _, s := range sessions[:10] {
			wait.Add(1)
			go func() {
				defer wait.Done()
				began := time.Now()
				nc, err := connectSession(t, url, s, nil)
				if err == nil {
					nc.Close()
				}
				if err != nil || time.Since(began) >= time.Second {
					slow.Add(1)
				}
			}()
		}
		wait.Wait()
		require.Positive(t, slow.Load(), "no connection waited on the replica that stopped answering: the test cannot see a delay")
	})
}

// Ready answers under a context with no deadline, which is what the broker's own serving context
// is: a Ready that needed one never returned there, so the broker never logged the service up.
func TestReadyNeedsNoDeadline(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	service := startService(t, url, keys, Deps{}, Decide)
	require.NoError(t, service.Ready(context.Background()))
}

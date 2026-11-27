package natscallout

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// sessionOption is a session's credential as natscred.Session presents it, on a token source of
// the test's own: each CONNECT takes the token current answers once, signs the nonce the server
// sent that connection for it, and sends that same token. signed, when it is not nil, is told
// each nonce as it is signed (lastNonces).
func sessionOption(key *ecdsa.PrivateKey, current func() string, signed func(nonce string)) nats.Option {
	var mu sync.Mutex
	presented := ""
	sign := func(nonce []byte) ([]byte, error) {
		token := current()
		proof, err := cipherclient.ConnectionProof(key, token, string(nonce))
		mu.Lock()
		presented = token
		mu.Unlock()
		if signed != nil {
			signed(string(nonce))
		}
		return []byte(proof), err
	}
	return func(o *nats.Options) error {
		if err := nats.UserJWT(func() (string, error) { return "-", nil }, sign)(o); err != nil {
			return err
		}
		return nats.TokenHandler(func() string { mu.Lock(); defer mu.Unlock(); return presented })(o)
	}
}

// reconnectingSession connects to url as s, as a bus client does: it keeps reconnecting, and it
// never gives up over a refused credential. That second part is what the storm below is about.
// nats.go closes a connection for good when the same server refuses it twice in a row with the
// same authorization error (v1.55.0, nats.go:4217-4222: `if nc.current.lastErr == err &&
// !nc.Opts.IgnoreAuthErrorAbort { nc.ar = true }`), and a hub whose connections this service
// admits refuses every one of them while the service is away, which a restart of the hub itself
// causes. natscred sets the option on exactly the connections the auth service decides (Session,
// Service), as a session here is; giveUpOnRefusal leaves it off, which is the control. It answers
// the connect's error rather than failing the test itself, so a caller may run it on a goroutine
// of its own. signed is sessionOption's.
func reconnectingSession(t *testing.T, url string, s *session, giveUpOnRefusal bool, signed func(nonce string), options ...nats.Option) (*nats.Conn, error) {
	t.Helper()
	settings := []nats.Option{
		sessionOption(s.key, func() string { return s.token }, signed),
		nats.Timeout(10 * time.Second), nats.MaxReconnects(-1),
		nats.ReconnectWait(100 * time.Millisecond), nats.ReconnectJitter(200*time.Millisecond, 0),
	}
	if !giveUpOnRefusal {
		settings = append(settings, nats.IgnoreAuthErrorAbort())
	}
	nc, err := nats.Connect(url, append(settings, options...)...)
	if err != nil {
		return nil, fmt.Errorf("session %s was not admitted: %w", s.id, err)
	}
	t.Cleanup(nc.Close)
	return nc, nil
}

// After the server restarts with the service held off the bus until each of five hundred open
// session connections has been refused once, every one of them is admitted again, each presenting
// a proof over its new connection's nonce, every decision the service makes is inside its budget,
// the service answers every request the server sends it, and each connection it admits is the one
// its client holds: the server gives up on none of them (decidedNonces.unheld, hubRecord).
//
// The service is held off for that fact, not for a span: the service is a client of the same
// server, so a restart drops its own connection too, and every client that comes back before it is
// refused. A client that is refused twice in a row by the same server closes for good unless it
// was told otherwise, so without that option a hub restart would leave the fleet with dead clients
// and no bus. The last control below is exactly that client, and it closes.
//
// Five hundred is the size the auth service verification's check c-callout-available names, and
// more than the service's decision slots, so the reconnects that reach it together queue for a
// slot. Under the race detector it is two hundred (raceEnabled): the clients and the hub run in the
// test's own process, and at five hundred under -race on a loaded machine the first connects
// failed before anything under test had started. envoy-go's plain step runs the five hundred. The
// first connects are made twenty at a time for the same reason: they set the storm up and are not
// the storm.
//
// The nonces are exact where the server's log is not: the server takes one answer per request, so
// an admission it does not take leaves no client connected on that nonce. And every client signs
// the nonce of each connection the server opens for it, so a nonce signed after the restarted
// service subscribed is a request the server sent that service: one it never decided is one it
// dropped without a word, which neither the server's log nor its decided nonces could show.
//
// Every admission no client holds fails the row, and the server's own record (hubRecord) names
// what became of each: a request the server gave up on although the service's answer to it reached
// the server, an answer the server refused, a connection it admitted and then lost, and an
// admission the service decided whose answer never reached the server. A decision, or its wait for
// a slot, past the service's budget fails the row too (backLog, slowest), and a hold-up in the
// delivery of requests to the service fails
// TestAStormPastTheWaitingBoundIsAnsweredInsideTheHubsTimeout.
func TestACalloutStormAfterAServerRestartAdmitsEveryConnection(t *testing.T) {
	connections := 500
	if raceEnabled {
		connections = 200
	}
	keys := testnats.NewCalloutKeys(t)
	recordUser, recordSeed := newHubRecordUser(t)
	server, url := testnats.StartWithCalloutServer(t, keys, recordUser)
	sessions := make([]*session, connections)
	for index := range sessions {
		sessions[index] = newSession(t, fmt.Sprintf("ses_storm_%03d", index), 15*time.Minute)
	}
	var decided, slowest atomic.Int64
	introspect := introspector(sessions...)
	timed := func(ctx context.Context, deps Deps, services []config.NATSService, req Request) (Decision, error) {
		began := time.Now()
		decision, err := Decide(ctx, deps, services, req)
		for took := int64(time.Since(began)); took > slowest.Load(); {
			if slowest.CompareAndSwap(slowest.Load(), took) {
				break
			}
		}
		return decision, err
	}
	deps := Deps{Introspect: func(ctx context.Context, token string) (oauthwire.Introspection, error) {
		decided.Add(1)
		return introspect(ctx, token)
	}}
	service := startService(t, url, keys, deps, timed)

	var reconnected atomic.Int64
	clients := newLastNonces()
	conns := make([]*nats.Conn, connections)
	errs := make([]error, connections)
	connecting := make(chan struct{}, 20)
	var wait sync.WaitGroup
	for index, s := range sessions {
		wait.Add(1)
		go func() {
			defer wait.Done()
			connecting <- struct{}{}
			defer func() { <-connecting }()
			conns[index], errs[index] = reconnectingSession(t, url, s, false, clients.signer(s.id),
				nats.ReconnectHandler(func(*nats.Conn) { reconnected.Add(1) }))
		}()
	}
	wait.Wait()
	require.NoError(t, errors.Join(errs...), "a first connection was not admitted")
	require.EqualValues(t, connections, decided.Load(), "every first connection was decided")

	// The service is off the bus until every client has been refused once since the restart, and
	// every client must still be there when it returns. back keeps the nonce of every request it
	// decides, and its reporter, which logs every request it drops, refuses or cannot answer,
	// writes to backLog. backUp is taken once back's subscription has reached the server
	// (startService), so the server sends back every request on a nonce signed after it. back starts
	// once the server's record is read again after the restart, so the record holds every request
	// back is sent.
	record := recordHub(t, url, recordSeed, keys)
	service.Close()
	restarted := time.Now()
	server.Restart(t, testnats.CalloutConfig(keys, recordUser))
	record.awaitResubscribed(t)
	testwait.UntilPolling(t, 50*time.Millisecond, "every client refused once while the service is away", func() (int, bool) {
		refused := 0
		for _, signed := range clients.signedSince(restarted) {
			if signed > 1 {
				refused++
			}
		}
		return refused, refused == connections
	})
	backDecided := newDecidedNonces()
	backLog := &logtest.Buffer{}
	backDeps := deps
	backDeps.Log = slog.New(slog.NewTextHandler(backLog, nil))
	back := startService(t, url, keys, backDeps, backDecided.wrap(timed))
	backUp := time.Now()
	testwait.UntilPolling(t, 50*time.Millisecond, "every connection admitted again, or one closed for good", func() (int, bool) {
		connected := 0
		for _, nc := range conns {
			if nc.IsClosed() {
				return connected, true
			}
			if nc.IsConnected() {
				connected++
			}
		}
		return connected, connected == connections
	})
	closed := 0
	for _, nc := range conns {
		if nc.IsClosed() {
			closed++
		}
	}
	require.Zero(t, closed, "%d connections closed for good while the service was away", closed)
	require.GreaterOrEqual(t, decided.Load(), int64(2*connections), "each reconnect was decided afresh")
	require.Less(t, time.Duration(slowest.Load()), decideBudget,
		"the slowest of %d decisions took %s, at or past the %s budget", decided.Load(), time.Duration(slowest.Load()), decideBudget)

	// Every client is connected, so every request the server sent back has been answered or given
	// up on. The record is read once back has closed, and back's Close writes its reporter's last
	// line.
	verdicts := record.read(t, server, connectionName, back.Close)
	require.Empty(t, backLog.String(), "back dropped, refused or could not answer requests the server sent it")
	unheld, held := backDecided.unheld(clients)
	require.Equal(t, connections, held, "only %d of %d clients are connected on a request back decided: the nonces do not show the connections", held, connections)
	gaveUp, lost := verdicts.gaveUpOn(unheld)
	t.Logf("reconnects=%d decided=%d slowest-decision=%s; back's admissions the server gave up on and whose answer reached it, by its record: %d",
		reconnected.Load(), decided.Load(), time.Duration(slowest.Load()), len(gaveUp))
	require.Empty(t, unheld, "back admitted %d requests that no client is connected on; the server gave up on %v although back's answer to each reached it, and of the others the record says %v", len(unheld), gaveUp, lost)
	undecided := backDecided.undecidedSince(clients, backUp)
	require.Empty(t, undecided, "back did not decide %d requests the server sent it after it subscribed: it dropped them without answering, refusing or counting them", len(undecided))

	t.Run("control: admissions the server stopped waiting for", func(t *testing.T) {
		// Each client's first request is decided half a second past the server's authorization
		// timeout, inside a budget raised past it, so it is admitted after the server has refused
		// that client; each request after it is decided at once. The nonces see each of those
		// admissions, and the server's record names each as one it gave up on whose answer reached
		// it.
		hubTimeout := testnats.CalloutAuthTimeout * time.Second
		was := decideBudget
		decideBudget = 2 * hubTimeout
		t.Cleanup(func() { decideBudget = was })
		keys := testnats.NewCalloutKeys(t)
		recordUser, recordSeed := newHubRecordUser(t)
		server, url := testnats.StartWithCalloutServer(t, keys, recordUser)
		record := recordHub(t, url, recordSeed, keys)
		const control = 5
		controls := sessionsNamed(t, "ses_late", control)
		var seen sync.Map
		lateFirst := func(ctx context.Context, deps Deps, services []config.NATSService, req Request) (Decision, error) {
			if _, again := seen.LoadOrStore(req.Token, true); !again {
				time.Sleep(hubTimeout + 500*time.Millisecond)
			}
			return Decide(ctx, deps, services, req)
		}
		decidedHere, clientsHere := newDecidedNonces(), newLastNonces()
		late := startService(t, url, keys, Deps{Introspect: introspector(controls...)}, decidedHere.wrap(lateFirst))
		conns := make([]*nats.Conn, control)
		errs := make([]error, control)
		var wait sync.WaitGroup
		for index, s := range controls {
			wait.Go(func() {
				conns[index], errs[index] = reconnectingSession(t, url, s, false, clientsHere.signer(s.id), nats.RetryOnFailedConnect(true))
			})
		}
		wait.Wait()
		require.NoError(t, errors.Join(errs...))
		testwait.Until(t, "every control client admitted on its second request", func() (bool, bool) {
			connected := allConnected(conns)()
			return connected, connected
		})
		verdicts := record.read(t, server, connectionName, late.Close)
		unheld, held := decidedHere.unheld(clientsHere)
		require.Equal(t, control, held, "the control clients are not connected on requests the service decided")
		require.Len(t, unheld, control, "the check above cannot see an admission the server stopped waiting for")
		gaveUp, other := verdicts.gaveUpOn(unheld)
		require.Len(t, gaveUp, control, "the server's record does not name the give-ups whose answer reached it: %v", other)
	})

	t.Run("control: admissions no client holds that the server did not give up on", func(t *testing.T) {
		// Each client's first request is admitted as a user the server does not keep: for half the
		// clients one already expired, whose answer the server refuses as it reads it, and for the
		// other half one that expires within three seconds, which the server takes and then closes
		// the connection of as the user expires. Each client is admitted again on a later nonce,
		// decided as usual. The row's check names every one of those first requests, with what the
		// server did with it, as one the server gave up on if a loaded machine made it do that.
		keys := testnats.NewCalloutKeys(t)
		recordUser, recordSeed := newHubRecordUser(t)
		server, url := testnats.StartWithCalloutServer(t, keys, recordUser)
		record := recordHub(t, url, recordSeed, keys)
		const control = 6
		controls := sessionsNamed(t, "ses_lost", control)
		var seen sync.Map
		var firsts atomic.Int64
		notKept := func(ctx context.Context, deps Deps, services []config.NATSService, req Request) (Decision, error) {
			decision, err := Decide(ctx, deps, services, req)
			if _, again := seen.LoadOrStore(req.Token, true); !again && err == nil {
				if firsts.Add(1)%2 == 0 {
					decision.Expires = time.Now().Add(-time.Minute)
				} else {
					decision.Expires = time.Now().Add(3 * time.Second)
				}
			}
			return decision, err
		}
		decidedHere, clientsHere := newDecidedNonces(), newLastNonces()
		lost := startService(t, url, keys, Deps{Introspect: introspector(controls...)}, decidedHere.wrap(notKept))
		conns := make([]*nats.Conn, control)
		for index, s := range controls {
			nc, err := reconnectingSession(t, url, s, false, clientsHere.signer(s.id), nats.RetryOnFailedConnect(true))
			require.NoError(t, err)
			conns[index] = nc
		}
		testwait.Until(t, "every control client admitted again on a later request", func() (map[string]int, bool) {
			signed := clientsHere.signedSince(time.Time{})
			for index, s := range controls {
				if signed[s.id] < 2 || !conns[index].IsConnected() {
					return signed, false
				}
			}
			return signed, true
		})
		verdicts := record.read(t, server, connectionName, lost.Close)
		unheld, held := decidedHere.unheld(clientsHere)
		require.Equal(t, control, held, "the control clients are not connected on requests the service decided")
		require.Len(t, unheld, control, "the nonces do not show the admissions the server did not keep")
		gaveUp, other := verdicts.gaveUpOn(unheld)
		outcomes := map[hubOutcome]int{}
		for _, each := range other {
			outcomes[each.outcome]++
		}
		require.Equal(t, control, len(gaveUp)+outcomes[hubRefused]+outcomes[hubKept],
			"the server's record does not hold each admission the server did not keep: %v", other)
		require.Positive(t, outcomes[hubRefused], "the row's check does not name an admission the server refused: %v", other)
		require.Positive(t, outcomes[hubKept], "the row's check does not name an admission the server took and then lost: %v", other)
	})

	t.Run("control: admissions the service decided and never answered", func(t *testing.T) {
		// A stand-in for the service in its queue group decides every request it is sent and does
		// not answer each client's first: it admits each one after it. The server gives up on each
		// first request at its timeout, refusing with no reason as it does for an answer it read
		// late, and the client is admitted on a later nonce. No answer to a first request ever
		// reached the server, so the row's check names each one as unanswered, and none as a
		// give-up whose answer reached the server.
		keys := testnats.NewCalloutKeys(t)
		recordUser, recordSeed := newHubRecordUser(t)
		server, url := testnats.StartWithCalloutServer(t, keys, recordUser)
		record := recordHub(t, url, recordSeed, keys)
		issuer, err := nkeys.FromSeed([]byte(keys.IssuerSeed))
		require.NoError(t, err)
		xkey, err := nkeys.FromSeed([]byte(keys.XKeySeed))
		require.NoError(t, err)
		const control, standInName = 5, "silent-stand-in"
		controls := sessionsNamed(t, "ses_silent", control)
		decidedHere, clientsHere := newDecidedNonces(), newLastNonces()
		standIn, err := testnats.ConnectSeed(t, url, keys.ServiceSeed, nats.Name(standInName))
		require.NoError(t, err)
		var seen sync.Map
		_, err = standIn.QueueSubscribe(authSubject, queueGroup, func(msg *nats.Msg) {
			claims, serverXKey, err := openRequest(xkey, msg)
			if err != nil {
				return
			}
			decidedHere.add(claims.ClientInformation.Nonce)
			if _, again := seen.LoadOrStore(claims.ConnectOptions.Token, true); !again {
				return
			}
			if out, err := admitUnchecked(issuer, xkey, claims, serverXKey, []string{"_INBOX.>"}, []string{"_INBOX.>"}); err == nil {
				_ = msg.Respond(out)
			}
		})
		require.NoError(t, err)
		require.NoError(t, standIn.Flush())
		conns := make([]*nats.Conn, control)
		for index, s := range controls {
			nc, err := reconnectingSession(t, url, s, false, clientsHere.signer(s.id), nats.RetryOnFailedConnect(true))
			require.NoError(t, err)
			conns[index] = nc
		}
		testwait.Until(t, "every control client admitted on a later request", func() (map[string]int, bool) {
			signed := clientsHere.signedSince(time.Time{})
			for index, s := range controls {
				if signed[s.id] < 2 || !conns[index].IsConnected() {
					return signed, false
				}
			}
			return signed, true
		})
		verdicts := record.read(t, server, standInName, standIn.Close)
		unheld, held := decidedHere.unheld(clientsHere)
		require.Equal(t, control, held, "the control clients are not connected on requests the stand-in decided")
		require.Len(t, unheld, control, "the nonces do not show the admissions the stand-in never answered")
		gaveUp, other := verdicts.gaveUpOn(unheld)
		require.Empty(t, gaveUp, "the row's check names admissions the service never answered as ones the server gave up on although the answer reached it")
		for _, each := range other {
			require.Equal(t, hubUnanswered, each.outcome, "the row's check does not name an admission the service never answered as one: %v", each)
		}
		require.Len(t, other, control, "the row's check does not name each admission the service never answered: %v", other)
	})

	t.Run("control: clients that give up on a refused credential", func(t *testing.T) {
		keys := testnats.NewCalloutKeys(t)
		server, url := testnats.StartWithCalloutServer(t, keys)
		const control = 20
		controls := make([]*session, control)
		for index := range controls {
			controls[index] = newSession(t, fmt.Sprintf("ses_control_%02d", index), 15*time.Minute)
		}
		first := startService(t, url, keys, Deps{Introspect: introspector(controls...)}, Decide)
		conns := make([]*nats.Conn, control)
		for index, s := range controls {
			nc, err := reconnectingSession(t, url, s, true, nil)
			require.NoError(t, err)
			conns[index] = nc
		}
		first.Close()
		server.Restart(t, testnats.CalloutConfig(keys))

		// No service comes back, so each client is refused at each attempt, and one refused twice
		// in a row by the same server closes for good.
		testwait.Until(t, "the clients that give up to close", func() (int, bool) {
			open := 0
			for _, nc := range conns {
				if !nc.IsClosed() {
					open++
				}
			}
			return open, open == 0
		})
	})
}

// A connection already open stays open while the auth service is stopped, and receives on its
// own channel, until the user it was admitted as expires; the server then closes it. Once the
// service answers again it reconnects with the live token its client holds by then, and receives
// again.
func TestConnectionsSurviveTheAuthServiceStoppingUntilTheirUserExpires(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	static, staticSeed := publisher(t)
	url := testnats.StartWithCallout(t, keys, static)
	a := newSession(t, "ses_a", 4*time.Second)
	renewed := *a
	renewed.token = "renewed-access-token-of-ses_a"
	renewed.answer.ExpiresAt = time.Now().Add(15 * time.Minute).Unix()
	deps := Deps{Introspect: introspector(a, &renewed)}
	service := startService(t, url, keys, deps, Decide)

	var current atomic.Value
	current.Store(a.token)
	option := sessionOption(a.key, func() string { return current.Load().(string) }, nil)
	var disconnected, reconnected atomic.Int64
	nc, err := nats.Connect(url, option, nats.IgnoreAuthErrorAbort(), nats.Timeout(10*time.Second), nats.MaxReconnects(-1),
		nats.ReconnectWait(200*time.Millisecond),
		nats.DisconnectErrHandler(func(*nats.Conn, error) { disconnected.Add(1) }),
		nats.ReconnectHandler(func(*nats.Conn) { reconnected.Add(1) }))
	require.NoError(t, err)
	defer nc.Close()
	own, err := nc.SubscribeSync(bus.DeliverSubject(a.id))
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	pub, err := testnats.ConnectSeed(t, url, staticSeed)
	require.NoError(t, err)
	defer pub.Close()
	receives := func(payload string) {
		t.Helper()
		// A round trip on nc first: after a reconnect nats.go has written the subscription again,
		// but the server reads nc's socket and pub's on goroutines of their own, so without it the
		// publish can reach the server before the subscription does and go to no one.
		require.NoError(t, nc.Flush())
		require.NoError(t, pub.Publish(bus.DeliverSubject(a.id), []byte(payload)))
		require.NoError(t, pub.Flush())
		msg, err := own.NextMsg(5 * time.Second)
		require.NoError(t, err, "ses_a received nothing on its own channel")
		require.Equal(t, payload, string(msg.Data))
	}

	service.Close()
	receives("while the service is stopped")
	require.True(t, nc.IsConnected())
	require.Zero(t, disconnected.Load(), "the open connection was dropped when the service stopped")

	testwait.WithinPolling(t, 15*time.Second, 50*time.Millisecond, "the server closing the connection when its user expires", func() bool {
		return disconnected.Load() > 0
	})
	require.False(t, nc.IsConnected(), "the connection outlived its user")

	current.Store(renewed.token)
	startService(t, url, keys, deps, Decide)
	testwait.WithinPolling(t, 30*time.Second, 50*time.Millisecond, "the reconnect once the service answers again", func() bool {
		return reconnected.Load() > 0 && nc.IsConnected()
	})
	receives("after the service is back")
}

// allConnected answers whether every one of conns is connected, for testwait to poll.
func allConnected(conns []*nats.Conn) func() bool {
	return func() bool {
		for _, nc := range conns {
			if !nc.IsConnected() {
				return false
			}
		}
		return true
	}
}

// lastNonces keeps the nonce each client signed last, and when and by whom each nonce was signed.
// A client signs the nonce of every connection the server opens for it (sessionOption), and one
// the server refuses is followed by another, so once a client is connected its last nonce is the
// one its connection was admitted on.
type lastNonces struct {
	mu     sync.Mutex
	last   map[string]string
	signed map[string]time.Time
	by     map[string]string
}

func newLastNonces() *lastNonces {
	return &lastNonces{last: map[string]string{}, signed: map[string]time.Time{}, by: map[string]string{}}
}

// signer is what client id tells each nonce it signs.
func (l *lastNonces) signer(id string) func(nonce string) {
	return func(nonce string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.last[id] = nonce
		l.signed[nonce] = time.Now()
		l.by[nonce] = id
	}
}

// signedSince is how many nonces each client signed at or after since. A client signs one for each
// connection the server opens for it, so one that signed two was turned away on the first.
func (l *lastNonces) signedSince(since time.Time) map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	counts := map[string]int{}
	for nonce, at := range l.signed {
		if !at.Before(since) {
			counts[l.by[nonce]]++
		}
	}
	return counts
}

// decidedNonces keeps the nonce of every request a service decided with no error.
type decidedNonces struct {
	mu     sync.Mutex
	nonces map[string]bool
}

func newDecidedNonces() *decidedNonces { return &decidedNonces{nonces: map[string]bool{}} }

// add keeps nonce as one a stand-in for the service decided.
func (d *decidedNonces) add(nonce string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nonces[nonce] = true
}

// wrap is decide, keeping the nonce of each request it decides with no error.
func (d *decidedNonces) wrap(decide decideFunc) decideFunc {
	return func(ctx context.Context, deps Deps, services []config.NATSService, req Request) (Decision, error) {
		decision, err := decide(ctx, deps, services, req)
		if err == nil {
			d.mu.Lock()
			d.nonces[req.Nonce] = true
			d.mu.Unlock()
		}
		return decision, err
	}
}

// unheld is what became of the service's decisions, read once every one of clients is connected
// and the service has closed: held is how many clients are connected on a nonce the service
// decided, and unheld every nonce it decided that no client is connected on. The server takes one
// answer per request, so a decided nonce no connection holds is an answer the server did not take:
// it had given up on the request and refused that client itself, or the service refused the
// connection after deciding it, and either way the client tried again on a new nonce. A request
// the server sent while no service was subscribed is in neither, since no service decided it.
func (d *decidedNonces) unheld(clients *lastNonces) (unheld []string, held int) {
	clients.mu.Lock()
	defer clients.mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	holding := make(map[string]bool, len(clients.last))
	for _, nonce := range clients.last {
		holding[nonce] = true
		if d.nonces[nonce] {
			held++
		}
	}
	for nonce := range d.nonces {
		if !holding[nonce] {
			unheld = append(unheld, nonce)
		}
	}
	return unheld, held
}

// undecidedSince is every nonce clients signed at or after since that the service did not decide,
// read once the service has closed. A client signs a nonce as the server opens a connection for
// it, before the server sends the request, so when since follows the service's subscribing, each
// such nonce is a request the server sent the service; one the service did not decide is one it
// dropped unanswered, or refused, or erred on.
func (d *decidedNonces) undecidedSince(clients *lastNonces, since time.Time) (undecided []string) {
	clients.mu.Lock()
	defer clients.mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	for nonce, at := range clients.signed {
		if !at.Before(since) && !d.nonces[nonce] {
			undecided = append(undecided, nonce)
		}
	}
	return undecided
}

// hubGaveUp is what nats-server logs, at debug level only, for an authorization request no service
// answered inside its authorization timeout (server/auth_callout.go:462-463). The server then
// refuses that client itself, and an answer arriving after it is read by no one.
const hubGaveUp = "not received in time"

// debugCalloutConfig is testnats.CalloutConfig with nats-server's debug log on, so that hubGaveUp
// reaches Server.Log: without it a test that reads the server's log for one reads nothing,
// whatever the service does.
func debugCalloutConfig(keys testnats.CalloutKeys) string {
	return testnats.CalloutConfig(keys) + "\ndebug: true\n"
}

// startDebugCalloutServer is testnats.StartWithCalloutServer with the server's debug log on: the
// fixture names no debug, so the server is restarted with the same configuration and debug added.
func startDebugCalloutServer(t *testing.T, keys testnats.CalloutKeys) (*testnats.Server, string) {
	t.Helper()
	server, url := testnats.StartWithCalloutServer(t, keys)
	server.Restart(t, debugCalloutConfig(keys))
	return server, url
}

// refusalCost is what each refusal's work takes here (withRefusalWork): a stand-in, scaled up, for
// the work of answering one request, which for a refusal is about 0.3 ms of CPU to open, read, sign
// and seal it (3.6 ms under -race, measured with the service's own keys).
const refusalCost = 50 * time.Millisecond

// withRefusalWork runs the rest of the test with every refusal taking cost before it is answered
// (refusalWork): made one after another, as the delivery goroutine once made them, the hundred or
// so refusals of a storm take about five seconds, past the hub's two-second authorization timeout
// in either build, and made side by side they take about one refusal's time. Set before any
// replica starts; its cleanup, registered first, runs after every replica's Close.
func withRefusalWork(t *testing.T, cost time.Duration) {
	t.Helper()
	refusalWork = func() { time.Sleep(cost) }
	t.Cleanup(func() { refusalWork = nil })
}

// refusedFor is how many connections registry counts refused under label, of every kind.
func refusedFor(t *testing.T, registry *metrics.Registry, label string) int {
	t.Helper()
	return countedFor(t, registry, func(line string) bool {
		return strings.HasPrefix(line, "broker_nats_callout_decisions_total{") && strings.Contains(line, `outcome="refused",reason="`+label+`"}`)
	})
}

// recordedFor is how many requests registry counts, answered or dropped, of every kind and reason.
func recordedFor(t *testing.T, registry *metrics.Registry) int {
	t.Helper()
	return countedFor(t, registry, func(line string) bool {
		return strings.HasPrefix(line, "broker_nats_callout_decisions_total{") || strings.HasPrefix(line, "broker_nats_callout_dropped_total{")
	})
}

// countedFor is the sum of registry's counter lines that counts says to count.
func countedFor(t *testing.T, registry *metrics.Registry, counts func(line string) bool) int {
	t.Helper()
	total := 0
	for _, line := range strings.Split(scrape(t, registry), "\n") {
		if !counts(line) {
			continue
		}
		n, err := strconv.Atoi(line[strings.LastIndex(line, " ")+1:])
		require.NoError(t, err, "a counter line: %s", line)
		total += n
	}
	return total
}

// stormRequests is how many connections each storm below sends one replica at once. The storms set
// the bounds to a handful, two slots and two waiting, or one slot and sixteen waiting for the flood
// of requests holding no credential, so almost every request goes past the waiting bound, which is
// the regime a storm of more than about 1,100 requests is for the deployed bounds (64 slots, 1,024
// waiting), and refusalCost puts what those refusals cost one after another past the hub's timeout.
// The count is kept that small because the clients and the hub run in the test's own process: at
// 400, under -race on a loaded machine, they fell behind themselves, and clients failed their
// connects with the service's fix in place.
const stormRequests = 120

// stormRead is what a storm's clients heard, the service's record of it and the hub's count of the
// requests of it the hub gave up on.
type stormRead struct {
	admitted, failed, pastBound, gaveUp int
	slowest                             time.Duration
	other                               error
}

// readStorm reads a storm once every one of its clients has returned, from errs and took. The hub
// writes each give-up (hubGaveUp) before it refuses that client, so once every client has returned
// the hub's log names every request it gave up on. The service counts each answer only after it
// has sent it, so the read waits until registry has counted one for each client that was admitted
// or refused.
func readStorm(t *testing.T, server *testnats.Server, registry *metrics.Registry, errs []error, took []time.Duration) stormRead {
	t.Helper()
	var storm stormRead
	for index, err := range errs {
		switch {
		case err == nil:
			storm.admitted++
		case !errors.Is(err, nats.ErrAuthorization):
			storm.failed++
			storm.other = err
		}
		storm.slowest = max(storm.slowest, took[index])
	}
	testwait.WithinPolling(t, 30*time.Second, 20*time.Millisecond, "the service to count every request of the storm", func() bool {
		return recordedFor(t, registry) >= len(errs)-storm.failed
	})
	storm.pastBound, storm.gaveUp = refusedFor(t, registry, labelWaitingBound), strings.Count(server.Log(), hubGaveUp)
	t.Logf("%d requests: %d admitted, %d past the waiting bound, %d failed otherwise (%v), the slowest client back after %s; the hub gave up on %d",
		len(errs), storm.admitted, storm.pastBound, storm.failed, storm.other, storm.slowest, storm.gaveUp)
	return storm
}

// requireEveryClientHeardInsideTheHubsTimeout fails unless the hub gave up on no request of a
// storm, every client was admitted (a nil error) or refused, and the storm reached what it is for:
// at least half its requests past the waiting bound, read from registry's refusals. It answers how
// many were admitted.
//
// Inside the hub's authorization timeout is the hub's own reading. Its timer runs from the moment
// it sends the request to the moment it reads the answer, and a request it did not give up on was
// answered inside it. A client's own time does not measure that. It adds the dial, the client's
// signature and the hub's work on the CONNECT before the timer starts, all in this one test
// process. Under the race detector on four loaded CPUs those took up to 0.8 s, and clients the hub
// had answered in time came back after more than two seconds.
func requireEveryClientHeardInsideTheHubsTimeout(t *testing.T, server *testnats.Server, registry *metrics.Registry, errs []error, took []time.Duration) int {
	t.Helper()
	storm := readStorm(t, server, registry, errs, took)
	require.Zero(t, storm.gaveUp, "the hub gave up on %d requests it sent the service", storm.gaveUp)
	require.Zero(t, storm.failed, "%d clients failed their connect neither admitted nor refused: %v", storm.failed, storm.other)
	require.GreaterOrEqual(t, storm.pastBound, len(errs)/2, "the storm did not go past the waiting bound, which is the regime it is for")
	return storm.admitted
}

// A storm of connections far past what one replica holds waiting is answered inside the hub's
// authorization timeout, every connection admitted or refused, and the hub gives up on none of
// them. Past the waiting bound a request is refused off the goroutine nats.go delivers every
// request on (deliver); refused on it, as each once was, every refusal held back the delivery of
// every request behind it, and the last of a storm were delivered after the hub had refused their
// clients, given a fresh budget, and answered to no one. Each refusal's work is scaled up
// (withRefusalWork).
//
// The bounds are two slots and two waiting, so an admitted request waits for one decision at most
// before its own, and its answer leaves the hub's timer what a loaded test process takes to deliver
// the storm's requests. With sixteen waiting the last admissions waited eight decisions, and under
// -race beside eight busy loops on two CPUs the service answered them 1.0-1.4 s after they reached
// it, which they did 0.3-0.45 s after the hub sent them: 1.70 s of the hub's 2 s, so a little more
// load decided the row. A replica's first delivery held 1.2 s makes the hub give up on such
// admissions with sixteen waiting, and on none with two.
func TestAStormPastTheWaitingBoundIsAnsweredInsideTheHubsTimeout(t *testing.T) {
	withRefusalWork(t, refusalCost)
	withDecisionSlots(t, 2)
	withWaitingBound(t, 2)
	keys := testnats.NewCalloutKeys(t)
	server, url := startDebugCalloutServer(t, keys)
	sessions := sessionsNamed(t, "ses_storm", stormRequests)
	deps := slowIntrospect(100*time.Millisecond, sessions...)
	registry := metrics.New()
	deps.Metrics = NewMetrics(registry)
	startService(t, url, keys, deps, Decide)

	errs, took := connectAll(t, url, sessions)
	admitted := requireEveryClientHeardInsideTheHubsTimeout(t, server, registry, errs, took)
	require.Positive(t, admitted, "no connection of the storm was admitted")
}

// The hub's count that the storms here read sees a storm answered too late: with each refusal's
// work half a second past the hub's authorization timeout, the hub gives up on every request the
// storm sends past the waiting bound and refuses those clients itself, and its count holds every one
// of them.
func TestTheHubCountsAStormAnsweredPastItsTimeout(t *testing.T) {
	withRefusalWork(t, testnats.CalloutAuthTimeout*time.Second+500*time.Millisecond)
	withDecisionSlots(t, 2)
	withWaitingBound(t, 2)
	keys := testnats.NewCalloutKeys(t)
	server, url := startDebugCalloutServer(t, keys)
	sessions := sessionsNamed(t, "ses_late_storm", stormRequests)
	deps := slowIntrospect(100*time.Millisecond, sessions...)
	registry := metrics.New()
	deps.Metrics = NewMetrics(registry)
	startService(t, url, keys, deps, Decide)

	errs, took := connectAll(t, url, sessions)
	storm := readStorm(t, server, registry, errs, took)
	require.GreaterOrEqual(t, storm.pastBound, len(errs)/2, "the storm did not go past the waiting bound, which is the regime it is for")
	require.GreaterOrEqual(t, storm.gaveUp, storm.pastBound, "the hub's count missed refusals answered past its timeout: the storms' check cannot see them")
}

// A flood of connections that hold no credential at all, which any client that reaches the hub can
// send one of per TCP connect, is answered the same way: every connection refused inside the hub's
// timeout, and the hub gives up on none of them. Such a request is refused without a broker call,
// so its decision costs what a refusal does, but the flood still outruns one slot and sixteen
// waiting, as a flood outruns the cores of a replica, and what goes past them is refused off the
// delivery goroutine.
func TestAFloodOfConnectionsHoldingNoCredentialIsAnsweredInsideTheHubsTimeout(t *testing.T) {
	withRefusalWork(t, refusalCost)
	withDecisionSlots(t, 1)
	withWaitingBound(t, 16)
	keys := testnats.NewCalloutKeys(t)
	server, url := startDebugCalloutServer(t, keys)
	registry := metrics.New()
	startService(t, url, keys, Deps{Metrics: NewMetrics(registry)}, Decide)

	errs := make([]error, stormRequests)
	took := make([]time.Duration, stormRequests)
	var wait sync.WaitGroup
	for index := range errs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			began := time.Now()
			nc, err := nats.Connect(url, nats.NoReconnect(), nats.Timeout(10*time.Second))
			took[index], errs[index] = time.Since(began), err
			if err == nil {
				nc.Close()
			}
		}()
	}
	wait.Wait()
	admitted := requireEveryClientHeardInsideTheHubsTimeout(t, server, registry, errs, took)
	require.Zero(t, admitted, "a connection holding no credential was admitted")
}

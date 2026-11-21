package natscallout

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// With the hold row unknown, the operator is admitted with released's grant, its user expiring at
// most 10 minutes after the decision, so the operator can always reach the broker's routes and
// restore the row; a listener on the same replica is refused hold_unknown at the same time.
//
// Run by hand, the control is the rule that refused the operator with hold_unknown too: the
// operator's connect fails, and the test fails naming it.
func TestTheOperatorIsAdmittedWhenTheHoldCannotBeRead(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.hold.set(holdAnswer{err: fmt.Errorf("natshold: read the hold row: %w", context.DeadlineExceeded)})
	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseReleased)
	succeeds(t, operator.Conn, "$JS.API.INFO", nil)
	require.Equal(t, "1", holdUnknownGauge(t, r.registry))

	err := r.listen()
	require.ErrorIs(t, err, nats.ErrAuthorization, "a listener was admitted while the hold was unknown")
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the listener's refusal counted hold_unknown", func() bool {
		return refusedFor(t, r.registry, labelHoldUnknown) == 1
	})
}

// While the row holds held, only the operator entry is answered with the operator's grant: a
// listener and a session are each decided with their own grant, and the listener's delete of
// KV_envoy_sessions gets a permissions violation and no answer.
//
// Run by hand, the control decides every service entry as the operator: the listener's delete is
// answered, and the test fails naming its account.
func TestNoOneButTheOperatorGetsTheOperatorGrant(t *testing.T) {
	a := newSession(t, "ses_a", 15*time.Minute)
	r := newOperatorRig(t, nil, a)
	r.setPhase(t, natsgrant.OperatorPhaseHeld)
	r.makeStreams(t, 1, 16, 0)
	listener := connectService(t, r.cluster.URL(), hourLong(listenerAccount), "")
	decided := r.decisions.after(t, listenerName, 0)
	require.Equal(t, natsgrant.ForCallout(natsgrant.Listener("example-hub")), decided.Grant, "the listener %s was not decided with its own grant", listenerAccount)
	listener.refused(t, "$JS.API.STREAM.DELETE."+kvBucket(bus.SessionBucket), nil)

	_, err := connectSession(t, r.cluster.URL(), a, nil)
	require.NoError(t, err)
	session := r.decisions.after(t, "session/ses_a", 0)
	require.Equal(t, must(natsgrant.Session("ses_a")), session.Grant)
	require.Zero(t, r.decisions.count(operatorName), "a client other than the operator was decided as the operator")
}

// A failed read is unknown at once, whatever the replica read before: a replica that read prepare,
// whose next read times out after the operator wrote held, refuses a listener hold_unknown. A
// replica that kept prepare would admit a writer after the hold's census, since the server never
// asks again about a connection it admitted.
//
// Run by hand, the control is a view that keeps its last reading for a minute: the listener is
// admitted, and the test fails naming it.
func TestAFailedReadRefusesOrdinaryClientsAtOnce(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhasePrepare)
	listener := connectService(t, r.cluster.URL(), hourLong(listenerAccount), "")
	r.decisions.after(t, listenerName, 0)
	listener.Close()

	// The operator writes held; this replica's next read times out, as natshold.Read's does past
	// its deadline.
	r.hold.set(holdAnswer{err: fmt.Errorf("natshold: read the hold row: %w", context.DeadlineExceeded)})
	err := r.listen()
	require.ErrorIs(t, err, nats.ErrAuthorization, "the listener %s was admitted on a replica whose read failed after prepare", listenerAccount)
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the listener's refusal counted hold_unknown", func() bool {
		return refusedFor(t, r.registry, labelHoldUnknown) == 1
	})
	require.Equal(t, 1, r.decisions.count(listenerName))
}

// tE3Rows are t-e-3's decisions, each with the answer its tests require: a session's own grant,
// each listed service kind's grant, and each refusal's label.
type tE3Row struct {
	name  string
	req   Request
	want  Decision
	label string
}

func tE3Rows(t *testing.T, now time.Time, a *session) []tE3Row {
	t.Helper()
	return []tE3Row{
		{"a session", a.request(t, "nonce"), Decision{Name: "session/ses_a", Grant: must(natsgrant.Session("ses_a")), Expires: time.Unix(a.answer.ExpiresAt, 0)}, ""},
		{"dispatch", serviceRequest("dispatch-token"), Decision{Name: "dispatch/example-dispatch", Grant: natsgrant.ForCallout(natsgrant.Dispatch("example-dispatch")), Expires: now.Add(time.Hour)}, ""},
		{"a listener", serviceRequest("listener-token"), Decision{Name: listenerName, Grant: natsgrant.ForCallout(natsgrant.Listener("example-hub")), Expires: now.Add(time.Hour)}, ""},
		{"a daemon", serviceRequest("daemon-token"), Decision{Name: "daemon/" + daemonAccount, Grant: natsgrant.ForCallout(natsgrant.Daemon()), Expires: now.Add(30 * time.Minute)}, ""},
		{"measure", serviceRequest("measure-token"), Decision{Name: "measure/" + measureAccount, Grant: natsgrant.ForCallout(natsgrant.Measure()), Expires: now.Add(time.Hour)}, ""},
		{"no credential", Request{UserNkey: "UUSER", ServerID: "NSERVER"}, Decision{}, labelNoCredential},
		{"an unlisted service", serviceRequest("unlisted-token"), Decision{}, labelServiceNotListed},
		{"a two-hour token", serviceRequest("two-hours-token"), Decision{}, labelTokenTooLong},
		{"a service held to projects", serviceRequest("held-token"), Decision{}, labelHeldToProjects},
	}
}

// tE3Deps are the decision tests' broker answers, for the services tE3Rows names.
func tE3Deps(now time.Time, a *session) Deps {
	return Deps{Introspect: introspector(a), ServiceToken: serviceTokens(map[string]platformToken{
		"dispatch-token":  {account: "system:serviceaccount:dispatch:dispatch", issued: now, expiry: now.Add(time.Hour)},
		"listener-token":  {account: listenerAccount, issued: now, expiry: now.Add(time.Hour)},
		"daemon-token":    {account: daemonAccount, issued: now, expiry: now.Add(30 * time.Minute)},
		"measure-token":   {account: measureAccount, google: true, issued: now, expiry: now.Add(time.Hour)},
		"unlisted-token":  {account: "system:serviceaccount:example-ns:stranger", issued: now, expiry: now.Add(time.Hour)},
		"two-hours-token": {account: daemonAccount, issued: now, expiry: now.Add(2 * time.Hour)},
		"held-token":      {account: "system:serviceaccount:example-ns:held-daemon", issued: now, expiry: now.Add(time.Hour)},
	})}
}

// requireAsBefore requires each of rows to be answered, under deps and services, as t-e-3's tests
// require.
func requireAsBefore(t *testing.T, deps Deps, services []config.NATSService, rows []tE3Row) {
	t.Helper()
	for _, row := range rows {
		decision, err := Decide(t.Context(), deps, services, row.req)
		if row.label == "" {
			require.NoError(t, err, "%s was refused", row.name)
			require.Equal(t, row.want, decision, "%s was not answered as before", row.name)
			continue
		}
		var refused *Refusal
		require.True(t, errors.As(err, &refused), "%s was admitted: %+v", row.name, decision)
		require.Equal(t, row.label, refused.Label, "%s", row.name)
	}
}

// With BROKER_NATS_HOLD_READ false the broker builds no hold view (Deps.Hold nil), and the hold
// rule is inert: every decision of t-e-3's decision tests is answered as before, with the operator
// and the probe listed beside them and the operator's signing keys given to a view with no read,
// and the operator is answered with released's grant. (The broker's refusal to start with the
// setting on and no operator entry is its command's test.)
//
// Run by hand, the control is the rule active without its setting, a broker that reads no row as
// unknown: a listener is refused hold_unknown, and the test fails naming it.
func TestTheHoldRuleIsInertWithoutItsSetting(t *testing.T) {
	now := time.Now()
	a := newSession(t, "ses_a", 15*time.Minute)
	deps := tE3Deps(now, a)
	deps.Hold = NewHoldView(nil, newHoldSigner(t).HoldKeys, nil)
	deps.Now = func() time.Time { return now }
	requireAsBefore(t, deps, holdServices, tE3Rows(t, now, a))
	operator, err := Decide(t.Context(), Deps{ServiceToken: serviceTokens(map[string]platformToken{
		"operator-token": {account: operatorAccount, issued: now, expiry: now.Add(time.Hour)},
	}), Now: deps.Now}, holdServices, serviceRequest("operator-token"))
	require.NoError(t, err)
	require.Equal(t, natsgrant.Operator(natsgrant.OperatorPhaseReleased), operator.Grant)
}

// With no operator entry nothing changes: every decision of t-e-3's decision tests is answered as
// before, with no hold view, which reads nothing, and with a view whose row reads held, which no
// listed entry's grant follows.
//
// Run by hand, the control is a decision path that decides a listed daemon entry as the operator:
// the daemon row fails, naming it.
func TestWithNoOperatorEntryNothingChanges(t *testing.T) {
	now := time.Now()
	a := newSession(t, "ses_a", 15*time.Minute)
	rows := tE3Rows(t, now, a)
	requireAsBefore(t, tE3Deps(now, a), exampleServices, rows)
	signer := newHoldSigner(t)
	stub := &holdStub{}
	stub.set(rowOf(signer.reading(t, 3, natsgrant.OperatorPhaseHeld, "hold-1")))
	held := tE3Deps(now, a)
	held.Hold = NewHoldView(stub.read, signer.HoldKeys, nil)
	requireAsBefore(t, held, exampleServices, rows)
}

// outcomes counts connections the storm made, refused and admitted, from a moment the test marks.
type outcomes struct {
	admitted, refused atomic.Int64
}

// Postgres stopped refuses every client but the operator, and only while it is down. During a
// storm of listener and session connections, the hold's Postgres goes away: every client that
// connects while it is down is refused hold_unknown, the operator is admitted with released's
// grant, its user expiring at most 10 minutes after the decision, and once Postgres answers again
// every client is admitted from the first decision that reads it.
//
// Run by hand, the control is a view that answers the last phase on an error: a listener is
// admitted during the outage, and the test fails naming it.
func TestAPostgresOutageRefusesOnlyWhileItLasts(t *testing.T) {
	pool, direct, proxy, schema := proxiedHoldPool(t)
	table := holdTable{pool: direct, signer: newHoldSigner(t)}
	a := newSession(t, "ses_a", time.Hour)
	read := func(ctx context.Context) (natshold.Reading, bool, error) { return natshold.Read(ctx, pool, schema) }
	r := newOperatorRigSigned(t, read, table.signer, a)
	table.write(t, 1, natsgrant.OperatorPhaseReleased, "hold-1")

	var phase atomic.Int32 // 0 before the outage, 1 during it, 2 after it
	var during, after outcomes
	stop := make(chan struct{})
	var storm sync.WaitGroup
	for worker := range 4 {
		storm.Add(1)
		go func() {
			defer storm.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				started := phase.Load()
				var err error
				if worker%2 == 0 {
					err = r.listen()
				} else {
					var nc *nats.Conn
					nc, err = connectSessionOnce(r.cluster.URL(), a)
					if err == nil {
						nc.Close()
					}
				}
				ended := phase.Load()
				if started != ended {
					continue
				}
				var counted *outcomes
				switch ended {
				case 1:
					counted = &during
				case 2:
					counted = &after
				default:
					continue
				}
				if err == nil {
					counted.admitted.Add(1)
				} else {
					counted.refused.Add(1)
				}
			}
		}()
	}
	defer func() { close(stop); storm.Wait() }()

	time.Sleep(time.Second)
	proxy.Cut()
	// The outage is marked by Postgres's own answer through the relay, not by the view's gauge: a
	// view that hid the outage would hide it from the gauge too.
	testwait.WithinPolling(t, 10*time.Second, 50*time.Millisecond, "Postgres unreachable through the cut relay", func() bool {
		return postgresAnswers(proxy.URL()) != nil
	})
	phase.Store(1)
	seen := r.decisions.count(operatorName)
	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), natsgrant.OperatorPhaseReleased)
	operator.Close()
	testwait.WithinPolling(t, 15*time.Second, 50*time.Millisecond, "connections during the outage", func() bool {
		return during.refused.Load()+during.admitted.Load() >= 8
	})
	phase.Store(3)
	require.Zero(t, during.admitted.Load(), "a listener or a session was admitted while the hold's Postgres was down (%d refused)", during.refused.Load())
	require.Positive(t, refusedFor(t, r.registry, labelHoldUnknown), "no refusal was counted hold_unknown")
	require.Equal(t, "1", holdUnknownGauge(t, r.registry), "broker_nats_callout_hold_unknown during the outage")

	proxy.Restore()
	testwait.WithinPolling(t, 10*time.Second, 50*time.Millisecond, "Postgres answering through the restored relay", func() bool {
		return postgresAnswers(proxy.URL()) == nil
	})
	phase.Store(2)
	testwait.WithinPolling(t, 15*time.Second, 50*time.Millisecond, "connections admitted after the outage", func() bool {
		return after.admitted.Load() >= 8
	})
	phase.Store(3)
	require.Zero(t, after.refused.Load(), "a client was refused after the hold's Postgres answered again (%d admitted)", after.admitted.Load())
	require.Equal(t, "0", holdUnknownGauge(t, r.registry), "broker_nats_callout_hold_unknown after the outage")
}

// postgresAnswers connects to dsn on a connection of its own and pings it, within a second.
func postgresAnswers(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	return conn.Ping(ctx)
}

// connectSessionOnce connects to url as s with no reconnect, outside a test's goroutine.
func connectSessionOnce(url string, s *session) (*nats.Conn, error) {
	sign := func(nonce []byte) ([]byte, error) {
		proof, err := cipherclient.ConnectionProof(s.key, s.token, string(nonce))
		return []byte(proof), err
	}
	return nats.Connect(url, nats.UserJWT(func() (string, error) { return "-", nil }, sign), nats.Token(s.token),
		nats.Timeout(5*time.Second), nats.NoReconnect())
}

// The operator sets held; the test's admin deletes the row: a listener is then refused
// hold_unknown, and the operator is admitted with released's grant. The operator restores the row
// as held, at a gen above every logged one: the row is known again, and the operator is answered
// with held's grant. The operator writes released, after which the listener is admitted. The
// operator's writes are the signed rows the broker's routes store, written here by SQL.
//
// Run by hand, the control reads a missing row as released: the listener is admitted after the
// delete, and the test fails naming it.
func TestAMissingHoldRowFailsClosed(t *testing.T) {
	pool, schema := holdPool(t)
	table := holdTable{pool: pool, signer: newHoldSigner(t)}
	read := func(ctx context.Context) (natshold.Reading, bool, error) { return natshold.Read(ctx, pool, schema) }
	r := newOperatorRigSigned(t, read, table.signer)
	table.write(t, 1, natsgrant.OperatorPhaseReleased, "hold-1")
	table.write(t, 2, natsgrant.OperatorPhasePrepare, "hold-1")
	table.write(t, 3, natsgrant.OperatorPhaseHeld, "hold-1")
	listen := r.listen
	require.NoError(t, listen(), "a listener was refused under held, which refuses no other client in this task")

	table.admin(t, `delete from nats_hold`)
	require.ErrorIs(t, listen(), nats.ErrAuthorization, "the listener %s was admitted after the row was deleted", listenerAccount)
	require.Equal(t, 1, refusedFor(t, r.registry, labelHoldUnknown))
	seen := r.decisions.count(operatorName)
	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), natsgrant.OperatorPhaseReleased)

	table.write(t, 4, natsgrant.OperatorPhaseHeld, "hold-1")
	seen = r.decisions.count(operatorName)
	operator.reconnect(t)
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), natsgrant.OperatorPhaseHeld)

	table.write(t, 5, natsgrant.OperatorPhaseReleased, "hold-1")
	require.NoError(t, listen(), "the listener was refused after the operator wrote released")
	require.Equal(t, 1, refusedFor(t, r.registry, labelHoldUnknown))
}

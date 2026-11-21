package writerhold

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// holdStack is the writer hold's whole stack (holdtest.Start) with the stack's own replica of the
// auth service closed, so every decision is made by the replicas the row starts itself.
func holdStack(t *testing.T) *holdtest.Stack {
	t.Helper()
	s := holdtest.Start(t)
	s.Auth.Close()
	return s
}

// holdClient is a connection to the stack's hub with every asynchronous error the server sent it,
// a permissions violation among them.
type holdClient struct {
	*nats.Conn
	errs chan error
}

// connectToHold connects to the hub's first member with opts and no reconnect, and answers the
// server's refusal. One member, because nats.go tries every server it is given before it answers
// a refused connect, and each try is one decision of the auth service: a row counts decisions.
func connectToHold(s *holdtest.Stack, opts ...nats.Option) (*holdClient, error) {
	c := &holdClient{errs: make(chan error, 64)}
	all := append([]nats.Option{nats.NoReconnect(), nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		select {
		case c.errs <- err:
		default:
		}
	})}, opts...)
	nc, err := s.Connect(s.Cluster.URLs()[0], all...)
	c.Conn = nc
	return c, err
}

// connectListener connects as the listener entry of the stack's first machine.
func connectListener(s *holdtest.Stack, opts ...nats.Option) (*holdClient, error) {
	return connectToHold(s, append([]nats.Option{s.ListenerCredential(holdtest.ListenerMachines[0])}, opts...)...)
}

// connectOperator connects as the operator, with its inbox prefix.
func connectOperator(s *holdtest.Stack, opts ...nats.Option) (*holdClient, error) {
	return connectToHold(s, append(s.OperatorCredential(), opts...)...)
}

// requireRefusedAt requires err to be the hub's refusal of a connect.
func requireRefusedAt(t *testing.T, err error, what string) {
	t.Helper()
	require.ErrorIs(t, err, nats.ErrAuthorization, "%s was not refused", what)
}

// grantedOn reports whether c's grant lets it send a request to subject: the server answers it,
// or refuses the publish with a permissions violation naming subject.
func grantedOn(t *testing.T, c *holdClient, subject string) bool {
	t.Helper()
	sub, err := c.SubscribeSync(c.NewInbox())
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()
	require.NoError(t, c.PublishRequest(subject, sub.Subject, nil))
	require.NoError(t, c.Flush())
	var granted bool
	testwait.Eventually(t, "an answer to, or the refusal of, "+subject, func() bool {
		select {
		case err := <-c.errs:
			if errors.Is(err, nats.ErrPermissionViolation) && strings.Contains(err.Error(), subject) {
				granted = false
				return true
			}
		default:
		}
		if _, err := sub.NextMsg(10 * time.Millisecond); err == nil {
			granted = true
			return true
		}
		return false
	})
	return granted
}

// operatorPhase is the phase whose grant the operator's connection c holds, read from what the
// hub lets it send: STREAM.NAMES is held's alone, and the STREAM.INFO of KV_legion_agents is
// prepare's and held's.
func operatorPhase(t *testing.T, c *holdClient) natsgrant.OperatorPhase {
	t.Helper()
	switch {
	case grantedOn(t, c, natsserver.JSApiStreams):
		return natsgrant.OperatorPhaseHeld
	case grantedOn(t, c, fmt.Sprintf(natsserver.JSApiStreamInfoT, kvStreamPrefix+bus.LegionAgentBucket)):
		return natsgrant.OperatorPhasePrepare
	}
	return natsgrant.OperatorPhaseReleased
}

// lastReading is the last reading of the row r decided on.
func lastReading(t *testing.T, r *holdtest.Replica) holdtest.Reading {
	t.Helper()
	readings := r.Readings()
	require.NotEmpty(t, readings, "the replica read no row")
	return readings[len(readings)-1]
}

// keepFor is t-e-55's view of 04:39:43Z, which the plan replaced: a read that fails answers the
// last reading that succeeded, while that reading is younger than window. Its control rows show
// what that view admits.
func keepFor(window time.Duration) func(holdtest.HoldRead) holdtest.HoldRead {
	return func(read holdtest.HoldRead) holdtest.HoldRead {
		var mu sync.Mutex
		var last natshold.Reading
		var at time.Time
		return func(ctx context.Context) (natshold.Reading, bool, error) {
			reading, found, err := read(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err == nil && found {
				last, at = reading, time.Now()
				return reading, found, nil
			}
			if !at.IsZero() && time.Since(at) < window {
				return last, true, nil
			}
			return reading, found, err
		}
	}
}

// failOpen is a view that maps every reading the auth service reads as unknown to released: a
// failed read, no row, a row no key of keys verifies, and a row below the highest gen it has
// read each answer a fresh released at that gen, signed with key so the replica's own checks pass
// it. Its control rows show what the unknown answer stops.
func failOpen(keys []ed25519.PublicKey, key ed25519.PrivateKey) func(holdtest.HoldRead) holdtest.HoldRead {
	return func(read holdtest.HoldRead) holdtest.HoldRead {
		var mu sync.Mutex
		var highest uint64
		return func(ctx context.Context) (natshold.Reading, bool, error) {
			reading, found, err := read(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err == nil && found && reading.Gen >= highest &&
				signedrecord.Verify(keys, reading.Sig, signedrecord.HoldRow(reading.Gen, reading.Phase, reading.HoldID, reading.SignedAt)) == nil {
				highest = reading.Gen
				return reading, true, nil
			}
			at := time.Now().UTC().Truncate(time.Microsecond)
			sig, signErr := signedrecord.Sign(key, signedrecord.HoldRow(highest, string(natsgrant.OperatorPhaseReleased), "", at))
			if signErr != nil {
				return natshold.Reading{}, false, signErr
			}
			return natshold.Reading{Gen: highest, Phase: string(natsgrant.OperatorPhaseReleased), SignedAt: at, Sig: sig}, true, nil
		}
	}
}

// moveRow moves the hold row from its current gen to phase under holdID, as the operator does.
func moveRow(t *testing.T, s *holdtest.Stack, phase natsgrant.OperatorPhase, holdID string) {
	t.Helper()
	row, err := s.Records.Row(t.Context())
	require.NoError(t, err)
	_, err = s.Records.MoveRow(t.Context(), row.Gen, phase, holdID, "the row's schedule")
	require.NoError(t, err, "move the row from %s at gen %d to %s %s", row.Phase, row.Gen, phase, holdID)
}

// redTeamSchedule runs the red team's schedule on s under hold holdID, with replica A reading the
// row through wrap (nil for the auth service's own view), and reports whether a writer that
// connects through A after the census was admitted. A reads prepare; the operator writes held;
// A's reads then time out, its path to Postgres stalled; B reads held, and the operator, decided
// by B, runs the writer census under the held grant; B closes, so the writer is decided by A
// alone. The row is released at the end.
func redTeamSchedule(t *testing.T, s *holdtest.Stack, holdID string, wrap func(holdtest.HoldRead) holdtest.HoldRead) (bool, *holdtest.Replica) {
	t.Helper()
	ctx := t.Context()
	a := s.StartReplica(t, holdtest.ReplicaConfig{Wrap: wrap})
	defer a.Close()
	moveRow(t, s, natsgrant.OperatorPhasePrepare, holdID)
	listener, err := connectListener(s)
	require.NoError(t, err, "a listener under prepare, decided by A")
	listener.Close()
	require.Equal(t, string(natsgrant.OperatorPhasePrepare), lastReading(t, a).Phase, "A's reading under prepare")
	readPrepare := time.Now()

	b := s.StartReplica(t, holdtest.ReplicaConfig{})
	moveRow(t, s, natsgrant.OperatorPhaseHeld, holdID)
	a.Postgres.Stall()
	var census *holdClient
	testwait.Eventually(t, "an operator connection decided by B under held", func() bool {
		op, err := connectOperator(s)
		require.NoError(t, err, "the operator is never refused for the hold's sake")
		if operatorPhase(t, op) == natsgrant.OperatorPhaseHeld {
			census = op
			return true
		}
		op.Close()
		return false
	})
	require.Equal(t, string(natsgrant.OperatorPhaseHeld), lastReading(t, b).Phase, "B's reading under held")
	_, err = TakeCensus(ctx, census.Conn, nil)
	require.NoError(t, err, "the writer census under the held grant")
	b.Close()

	writer, err := connectListener(s)
	admitted := err == nil
	if admitted {
		writer.Close()
	} else {
		requireRefusedAt(t, err, "the writer")
	}
	t.Logf("schedule under %s: A read prepare %s before the writer connected; the writer admitted: %v; A's readings %v",
		holdID, time.Since(readPrepare).Round(time.Millisecond), admitted, a.Readings())
	a.Postgres.Open()
	moveRow(t, s, natsgrant.OperatorPhaseReleased, holdID)
	return admitted, a
}

// TestHoldReadTheRedTeamsSchedule shows the hold-read contract on the red team's schedule (red
// team F1), on two replicas A and B of the auth service over one test Postgres: A reads prepare,
// the operator writes held, A's reads then time out, and B reads held; after the census, a writer
// that connects through A is refused, because A answers unknown (hold_unknown). Control, run
// first: t-e-55's 04:39:43Z view, which keeps A's cached prepare for 60 s, admits the writer after
// the census. The row fails if the auth service's view keeps any reading across a failed read.
func TestHoldReadTheRedTeamsSchedule(t *testing.T) {
	s := holdStack(t)

	admitted, _ := redTeamSchedule(t, s, "h-red-team-control", keepFor(60*time.Second))
	require.True(t, admitted, "control: t-e-55's view, which keeps A's prepare for 60 s, did not admit the writer after the census")
	t.Logf("control: t-e-55's view admitted the writer through A after the census, under a held row")

	admitted, a := redTeamSchedule(t, s, "h-red-team", nil)
	require.False(t, admitted, "A admitted the writer after the census while its reads timed out")
	require.GreaterOrEqual(t, a.Decisions("service", "refused", "hold_unknown"), int64(1), "A's refusal of the writer")
	require.Error(t, lastReading(t, a).Err, "A's last reading, the writer's")
	t.Logf("A refused the writer hold_unknown: %d hold_unknown refusals, its last reading's error: %v",
		a.Decisions("service", "refused", "hold_unknown"), lastReading(t, a).Err)
}

// TestHoldReadAFailureAfterHeldIsUnknown shows that the auth service caches nothing (A21): a read
// that fails right after a read of held answers unknown at once, which still refuses a listener,
// sets envoy_broker_hold_unknown and gives the operator released's grant, never held's; the next
// read that succeeds answers held again. Control, run first: a view that keeps its last reading
// answers held after the failed read, so the listener is refused held, the gauge stays 0 and the
// operator is given held's grant from a reading Postgres never gave. The row fails if the view
// answers a failed read with any earlier reading.
func TestHoldReadAFailureAfterHeldIsUnknown(t *testing.T) {
	s := holdStack(t)
	moveRow(t, s, natsgrant.OperatorPhasePrepare, "h-a21")
	moveRow(t, s, natsgrant.OperatorPhaseHeld, "h-a21")

	failAfterHeld := func(r *holdtest.Replica) (listenerReason string, operator natsgrant.OperatorPhase) {
		t.Helper()
		_, err := connectListener(s)
		requireRefusedAt(t, err, "a listener under held")
		require.Equal(t, int64(1), r.Decisions("service", "refused", "held"), "the replica's refusal under held")
		require.Equal(t, string(natsgrant.OperatorPhaseHeld), lastReading(t, r).Phase)
		require.Equal(t, "0", r.HoldUnknown())
		before := len(r.Readings())
		r.Postgres.Cut()
		_, err = connectListener(s)
		requireRefusedAt(t, err, "a listener after the failed read")
		readings := r.Readings()[before:]
		require.Len(t, readings, 1, "the decision read the row once")
		switch {
		case r.Decisions("service", "refused", "hold_unknown") == 1:
			listenerReason = "hold_unknown"
		case r.Decisions("service", "refused", "held") == 2:
			listenerReason = "held"
		}
		op, err := connectOperator(s)
		require.NoError(t, err, "the operator is never refused for the hold's sake")
		operator = operatorPhase(t, op)
		op.Close()
		t.Logf("after a read of held, the read failed (%v): the listener was refused %s, envoy_broker_hold_unknown %s, the operator given %s's grant",
			readings[0].Err, listenerReason, r.HoldUnknown(), operator)
		r.Postgres.Open()
		return listenerReason, operator
	}

	cached := s.StartReplica(t, holdtest.ReplicaConfig{Wrap: keepFor(60 * time.Second)})
	reason, operator := failAfterHeld(cached)
	require.Equal(t, "held", reason, "control: the cached view")
	require.Equal(t, natsgrant.OperatorPhaseHeld, operator, "control: the cached view")
	require.Equal(t, "0", cached.HoldUnknown(), "control: the cached view")
	cached.Close()

	r := s.StartReplica(t, holdtest.ReplicaConfig{})
	reason, operator = failAfterHeld(r)
	require.Equal(t, "hold_unknown", reason, "a failed read right after held")
	require.Equal(t, natsgrant.OperatorPhaseReleased, operator, "the operator's grant while the row is unknown")
	_, err := connectListener(s)
	requireRefusedAt(t, err, "a listener once Postgres answers again")
	require.Equal(t, string(natsgrant.OperatorPhaseHeld), lastReading(t, r).Phase, "the next read, once Postgres answers")
	require.Equal(t, "0", r.HoldUnknown())
}

// TestHoldReadUnknownAdmitsOnlyTheOperator shows that every reading the auth service cannot trust
// is unknown, and that unknown admits only the operator, with released's grant: while the row is
// held, Postgres stopped (the replica's path to it cut), a read held past natshold.ReadDeadline
// (the table locked by another transaction), the row deleted by the instance admin, a row with a
// bad sig (a database writer's forged release) and a row below the replica's floor (the
// bootstrap's own signed released replayed) each refuse a listener hold_unknown and admit the
// operator. The operator's user expires within 10 minutes of the decision: a replica whose clock
// runs 9 min 45 s behind gives the operator a user the hub closes, authentication expired, some
// seconds after its connect, while a listener it admitted earlier stays. Control, run first: a
// view that maps each of these readings to released admits a listener in every case. The row fails
// if any of the five readings answers anything but unknown.
func TestHoldReadUnknownAdmitsOnlyTheOperator(t *testing.T) {
	ctx := t.Context()
	s := holdStack(t)
	pool := s.Broker.Pool
	type row struct {
		gen                       int64
		phase, holdID, reason, by string
		updatedAt, signedAt       time.Time
		sig                       string
	}
	readRow := func() row {
		t.Helper()
		var r row
		require.NoError(t, pool.QueryRow(ctx, `select gen, phase, hold_id, reason, updated_by, updated_at, signed_at, sig from nats_hold`).
			Scan(&r.gen, &r.phase, &r.holdID, &r.reason, &r.by, &r.updatedAt, &r.signedAt, &r.sig))
		return r
	}
	writeRow := func(r row) {
		t.Helper()
		_, err := pool.Exec(ctx, `insert into nats_hold (id, gen, phase, hold_id, reason, updated_by, updated_at, signed_at, sig)
			values (1, $1, $2, $3, $4, $5, $6, $7, $8)
			on conflict (id) do update set gen = excluded.gen, phase = excluded.phase, hold_id = excluded.hold_id, reason = excluded.reason,
				updated_by = excluded.updated_by, updated_at = excluded.updated_at, signed_at = excluded.signed_at, sig = excluded.sig`,
			r.gen, r.phase, r.holdID, r.reason, r.by, r.updatedAt, r.signedAt, r.sig)
		require.NoError(t, err)
	}
	bootstrap := readRow()
	moveRow(t, s, natsgrant.OperatorPhasePrepare, "h-unknown")
	moveRow(t, s, natsgrant.OperatorPhaseHeld, "h-unknown")
	held := readRow()

	var replica *holdtest.Replica
	var lock func()
	cases := []struct {
		name       string
		make, undo func()
	}{
		{"Postgres stopped", func() { replica.Postgres.Cut() }, func() { replica.Postgres.Open() }},
		{"a read held past 500 ms", func() {
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			_, err = tx.Exec(ctx, `lock table nats_hold in access exclusive mode`)
			require.NoError(t, err)
			lock = func() { require.NoError(t, tx.Rollback(context.Background())) }
		}, func() { lock() }},
		{"the row deleted by the instance admin", func() {
			_, err := pool.Exec(ctx, `delete from nats_hold`)
			require.NoError(t, err)
		}, func() { writeRow(held) }},
		{"a row with a bad sig", func() {
			_, err := pool.Exec(ctx, `update nats_hold set phase = 'released'`)
			require.NoError(t, err)
		}, func() { writeRow(held) }},
		{"a row below the floor", func() { writeRow(bootstrap) }, func() { writeRow(held) }},
	}

	// Control: a view that maps each unknown reading to released admits a listener in every case.
	replica = s.StartReplica(t, holdtest.ReplicaConfig{Wrap: failOpen([]ed25519.PublicKey{s.Broker.VerifyKey}, s.Broker.SigningKey)})
	_, err := connectListener(s)
	requireRefusedAt(t, err, "a listener under held, before the control's cases")
	for _, c := range cases {
		c.make()
		listener, err := connectListener(s)
		require.NoError(t, err, "control: the fail-open view refused a listener with %s", c.name)
		listener.Close()
		t.Logf("control, %s: the view that maps it to released admitted a listener while the row is held", c.name)
		c.undo()
	}
	replica.Close()

	replica = s.StartReplica(t, holdtest.ReplicaConfig{})
	_, err = connectListener(s)
	requireRefusedAt(t, err, "a listener under held")
	require.Equal(t, int64(1), replica.Decisions("service", "refused", "held"))
	for index, c := range cases {
		c.make()
		_, err := connectListener(s)
		requireRefusedAt(t, err, "a listener with "+c.name)
		require.Equal(t, int64(index+1), replica.Decisions("service", "refused", "hold_unknown"), "%s: the listener's refusal", c.name)
		reading := lastReading(t, replica)
		op, err := connectOperator(s)
		require.NoError(t, err, "%s: the operator is never refused for the hold's sake", c.name)
		phase := operatorPhase(t, op)
		op.Close()
		require.Equal(t, natsgrant.OperatorPhaseReleased, phase, "%s: the operator's grant while the row is unknown", c.name)
		require.Equal(t, "1", replica.HoldUnknown(), "%s: envoy_broker_hold_unknown", c.name)
		t.Logf("%s: the listener refused hold_unknown (reading: phase %q, found %v, error %v), the operator admitted with released's grant",
			c.name, reading.Phase, reading.Found, reading.Err)
		c.undo()
	}
	replica.Close()

	// The operator's user expires within 10 minutes of its decision: a replica 9 min 45 s behind
	// answers the operator with a user that expires about 15 s from now, and a listener with its
	// token's expiry, 50 minutes.
	moveRow(t, s, natsgrant.OperatorPhaseReleased, "h-unknown")
	behind := 10*time.Minute - 15*time.Second
	replica = s.StartReplica(t, holdtest.ReplicaConfig{Now: func() time.Time { return time.Now().Add(-behind) }})
	listener, err := connectListener(s)
	require.NoError(t, err, "a listener under released")
	replica.Postgres.Cut()
	closed := make(chan struct{})
	op, err := connectOperator(s, nats.ClosedHandler(func(*nats.Conn) { close(closed) }))
	require.NoError(t, err, "the operator while Postgres is stopped")
	connected := time.Now()
	testwait.Eventually(t, "the hub to close the operator's connection at its user's expiry", func() bool {
		select {
		case <-closed:
			return true
		default:
			return false
		}
	})
	// The server's -ERR 'User Authentication Expired' reaches nats.go's error handler as
	// ErrAuthExpired; the close that follows can leave LastError the socket's EOF.
	var expired error
	for len(op.errs) > 0 && expired == nil {
		if err := <-op.errs; errors.Is(err, nats.ErrAuthExpired) {
			expired = err
		}
	}
	require.ErrorIs(t, expired, nats.ErrAuthExpired, "the operator's connection closed for another reason; its last error %v", op.LastError())
	lived := time.Since(connected)
	require.Less(t, lived, 10*time.Minute, "the operator's user outlived 10 minutes")
	require.True(t, listener.IsConnected(), "control: the listener admitted before the operator was closed too")
	t.Logf("the operator's user, admitted while the row was unknown by a replica %s behind, expired %s after its connect (%v); the listener admitted before it is still connected",
		behind, lived.Round(100*time.Millisecond), expired)
}

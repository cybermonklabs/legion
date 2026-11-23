package natscallout

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// holdSigner is the operator's hold signing key as a test holds it: the private half signs each
// row the test writes, and HoldKeys is the public half alone, as BROKER_NATS_HOLD_SIGNING_KEYS is.
type holdSigner struct {
	key ed25519.PrivateKey
	// HoldKeys is the list the broker verifies rows against.
	HoldKeys []ed25519.PublicKey
}

func newHoldSigner(t testing.TB) holdSigner {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return holdSigner{key: private, HoldKeys: []ed25519.PublicKey{public}}
}

// reading is the hold row at gen in phase under holdID, signed by s now. The time is cut to the
// microsecond, as Postgres keeps a timestamptz, so a row read back from the table carries the very
// time it was signed at.
func (s holdSigner) reading(t testing.TB, gen uint64, phase natsgrant.OperatorPhase, holdID string) natshold.Reading {
	t.Helper()
	signedAt := time.Now().UTC().Truncate(time.Microsecond)
	sig, err := signedrecord.Sign(s.key, signedrecord.HoldRow(gen, string(phase), holdID, signedAt))
	require.NoError(t, err)
	return natshold.Reading{Gen: gen, Phase: string(phase), HoldID: holdID, SignedAt: signedAt, Sig: sig}
}

// holdAnswer is one answer a stub hold read gives: the row and whether one was found, or an error.
type holdAnswer struct {
	reading natshold.Reading
	found   bool
	err     error
}

// rowOf is the answer that finds reading.
func rowOf(reading natshold.Reading) holdAnswer { return holdAnswer{reading: reading, found: true} }

// holdStub is a hold view's read whose answer the test sets, counting each call. It is safe to
// read from the auth service's decisions while the test sets the next answer.
type holdStub struct {
	mu     sync.Mutex
	answer holdAnswer
	calls  int
}

func (s *holdStub) set(answer holdAnswer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer = answer
}

func (s *holdStub) read(context.Context) (natshold.Reading, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.answer.reading, s.answer.found, s.answer.err
}

func (s *holdStub) called() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// holdUnknownGauge is broker_nats_callout_hold_unknown's value as registry renders it, or "" for
// none.
func holdUnknownGauge(t *testing.T, registry *metrics.Registry) string {
	t.Helper()
	for _, line := range strings.Split(scrape(t, registry), "\n") {
		if value, ok := strings.CutPrefix(line, "broker_nats_callout_hold_unknown "); ok {
			return value
		}
	}
	return ""
}

// The hold view answers each phase the row holds, known, and unknown for everything else, at once
// and whatever it read before: no row; a phase outside the three; a Postgres error; a timeout,
// after a reading of each phase; a row no listed key signed, or none; and a verified gen below the
// highest this view has read. It reads the row at every State, and broker_nats_callout_hold_unknown is 1
// exactly while its last reading was unknown. A view over no read, the setting off, answers
// released, known, and reads nothing.
//
// It fails when the view reads a missing row as released (the "no row" row), or keeps a reading
// across a failed one, as a cache would (each "a timeout after" row, whose second State must
// answer unknown and must have read the row twice).
func TestTheHoldViewMapsEveryReading(t *testing.T) {
	signer, stranger := newHoldSigner(t), newHoldSigner(t)
	timeout := fmt.Errorf("natshold: read the hold row: %w", context.DeadlineExceeded)
	refused := errors.New("natshold: begin the hold read: dial tcp 127.0.0.1:5432: connect: connection refused")
	unsigned := signer.reading(t, 4, natsgrant.OperatorPhaseHeld, "h-1")
	unsigned.Sig = ""
	type step struct {
		answer holdAnswer
		phase  natsgrant.OperatorPhase
		known  bool
	}
	known := func(gen uint64, phase natsgrant.OperatorPhase) step {
		return step{rowOf(signer.reading(t, gen, phase, "h-1")), phase, true}
	}
	unknown := func(answer holdAnswer) step { return step{answer: answer} }
	for _, row := range []struct {
		name  string
		steps []step
	}{
		{"a row holding released", []step{known(1, natsgrant.OperatorPhaseReleased)}},
		{"a row holding prepare", []step{known(2, natsgrant.OperatorPhasePrepare)}},
		{"a row holding held", []step{known(3, natsgrant.OperatorPhaseHeld)}},
		{"no row", []step{unknown(holdAnswer{})}},
		{"the phase paused, signed", []step{unknown(rowOf(signer.reading(t, 2, "paused", "h-1")))}},
		{"a Postgres error", []step{unknown(holdAnswer{err: refused})}},
		{"a timeout after released", []step{known(1, natsgrant.OperatorPhaseReleased), unknown(holdAnswer{err: timeout})}},
		{"a timeout after prepare", []step{known(2, natsgrant.OperatorPhasePrepare), unknown(holdAnswer{err: timeout})}},
		{"a timeout after held", []step{known(3, natsgrant.OperatorPhaseHeld), unknown(holdAnswer{err: timeout})}},
		{"a row signed by another key", []step{unknown(rowOf(stranger.reading(t, 3, natsgrant.OperatorPhaseReleased, "h-1")))}},
		{"a row with no signature", []step{unknown(rowOf(unsigned))}},
		{"a verified gen below the floor", []step{known(3, natsgrant.OperatorPhaseHeld), unknown(rowOf(signer.reading(t, 2, natsgrant.OperatorPhaseReleased, "h-1")))}},
		{"a known reading after an unknown one", []step{unknown(holdAnswer{err: refused}), known(5, natsgrant.OperatorPhasePrepare)}},
	} {
		t.Run(row.name, func(t *testing.T) {
			registry := metrics.New()
			stub := &holdStub{}
			view := NewHoldView(stub.read, signer.HoldKeys, nil, NewMetrics(registry))
			for index, step := range row.steps {
				stub.set(step.answer)
				phase, known := view.State(t.Context())
				require.Equal(t, step.known, known, "reading %d: known", index+1)
				require.Equal(t, step.phase, phase, "reading %d: phase", index+1)
				gauge := "0"
				if !step.known {
					gauge = "1"
				}
				require.Equal(t, gauge, holdUnknownGauge(t, registry), "reading %d: broker_nats_callout_hold_unknown", index+1)
			}
			require.Equal(t, len(row.steps), stub.called(), "the view did not read the row at every State")
		})
	}
	t.Run("no read: the setting off", func(t *testing.T) {
		registry := metrics.New()
		view := NewHoldView(nil, signer.HoldKeys, nil, NewMetrics(registry))
		require.Nil(t, view, "a view over no read is the setting off, which is no view")
		phase, known := view.State(t.Context())
		require.True(t, known)
		require.Equal(t, natsgrant.OperatorPhaseReleased, phase)
		require.Equal(t, "0", holdUnknownGauge(t, registry))
	})
}

// Every decision made through one Deps reads one HoldView, so they share its floor: a listener
// admitted under the operator's signed held at gen 3 is refused hold_unknown, as a replay, once a
// database writer puts the operator's older signed released at gen 2 back, though the two Decide
// calls share nothing but deps. A caller that started no replica (Start) reads the same floor as
// one that did.
//
// Run by hand, the control is a Decide that makes a view of its own for each decision, as Deps
// did while it held the read and the keys and Start made the replica's view: the second decision
// admits the listener over the put-back row, and the test fails naming it.
func TestDecisionsThroughOneDepsShareTheHoldFloor(t *testing.T) {
	signer := newHoldSigner(t)
	stub := &holdStub{}
	now := time.Now()
	deps := Deps{
		ServiceToken: serviceTokens(map[string]platformToken{"listener-token": {account: listenerAccount, issued: now, expiry: now.Add(time.Hour)}}),
		Hold:         NewHoldView(stub.read, signer.HoldKeys, nil, nil),
	}
	stub.set(rowOf(signer.reading(t, 3, natsgrant.OperatorPhaseHeld, "hold-1")))
	_, err := Decide(t.Context(), deps, holdServices, serviceRequest("listener-token"))
	require.NoError(t, err, "the listener was refused under the operator's signed held")
	stub.set(rowOf(signer.reading(t, 2, natsgrant.OperatorPhaseReleased, "hold-1")))
	decision, err := Decide(t.Context(), deps, holdServices, serviceRequest("listener-token"))
	var refused *Refusal
	require.True(t, errors.As(err, &refused), "the listener %s was admitted over the operator's older row put back: %+v", listenerAccount, decision)
	require.Equal(t, labelHoldUnknown, refused.Label)
	require.ErrorIs(t, refused.Cause, signedrecord.ErrReplay)
	require.Equal(t, 2, stub.called())
}

// A hold row whose phase is none of the three is unknown, and the refusal's cause, which the
// callout's refusal log writes, holds no part of the phase the row holds: the phase is read before
// its signature is checked, so it is whatever a database writer put there. The migration's check
// constraint keeps the table to the three phases, so the row comes from the hold view's read.
//
// Run by hand, the control is the hold view at bb2b623923: its cause reads `the writer hold's row
// holds the phase "CANARY-PHASE-4f1c", none of the three`.
func TestAHoldRowsUnknownPhaseIsNotPrinted(t *testing.T) {
	signer := newHoldSigner(t)
	stub := &holdStub{}
	now := time.Now()
	deps := Deps{
		ServiceToken: serviceTokens(map[string]platformToken{"listener-token": {account: listenerAccount, issued: now, expiry: now.Add(time.Hour)}}),
		Hold:         NewHoldView(stub.read, signer.HoldKeys, nil, nil),
	}
	const canary = "CANARY-PHASE-4f1c"
	stub.set(rowOf(signer.reading(t, 2, canary, "hold-1")))
	_, err := Decide(t.Context(), deps, holdServices, serviceRequest("listener-token"))
	var refused *Refusal
	require.True(t, errors.As(err, &refused), "the listener %s was admitted under a phase none of the three", listenerAccount)
	require.Equal(t, labelHoldUnknown, refused.Label)
	require.ErrorContains(t, refused.Cause, "none of the three")
	require.NotContains(t, refused.Cause.Error(), canary, "the refusal's cause prints the row's phase")
	require.NotContains(t, err.Error(), canary)
}

// A second signed row at the gen this view has read reads unknown: the view reads held at gen 3,
// then the operator's released at gen 3, which the operator signs when a database writer has put
// the hold's older prepare row (gen 2) back and its abort moves the row from gen 2, as Review
// 4679's round 3 ran over the routes. Both rows verify; the floor keeps the row it read at gen 3,
// so the released row at that gen is a fork and refuses every client but the operator. The held
// row reads known again, and a released row at gen 4 moves the floor on.
//
// Run by hand, the control is the floor at bb2b623923, which keeps the gen alone: the view reads
// the released row at gen 3 as released, known.
func TestADifferentRowAtTheGenReadReadsUnknown(t *testing.T) {
	signer := newHoldSigner(t)
	stub := &holdStub{}
	view := NewHoldView(stub.read, signer.HoldKeys, nil, nil)
	held := signer.reading(t, 3, natsgrant.OperatorPhaseHeld, "hold-1")
	stub.set(rowOf(held))
	phase, known := view.State(t.Context())
	require.True(t, known)
	require.Equal(t, natsgrant.OperatorPhaseHeld, phase)

	stub.set(rowOf(signer.reading(t, 3, natsgrant.OperatorPhaseReleased, "hold-1")))
	phase, err := view.phase(t.Context())
	require.ErrorIs(t, err, signedrecord.ErrFork, "the released row at the held row's gen read as %q", phase)

	stub.set(rowOf(held))
	phase, known = view.State(t.Context())
	require.True(t, known, "the held row read before is unknown after the fork")
	require.Equal(t, natsgrant.OperatorPhaseHeld, phase)
	stub.set(rowOf(signer.reading(t, 4, natsgrant.OperatorPhaseReleased, "hold-1")))
	phase, known = view.State(t.Context())
	require.True(t, known)
	require.Equal(t, natsgrant.OperatorPhaseReleased, phase)
}

// openPool is a pool on dsn, closed when the test ends.
func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// schemaOf is the schema a storetest DSN's connections use, which natshold.Read names its table
// by.
func schemaOf(t *testing.T, dsn string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	return parsed.Query().Get("search_path")
}

// holdPool is a pool on a migrated schema of the test's own (storetest), and that schema's name. It
// skips the test when BROKER_TEST_DATABASE_URL is unset.
func holdPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := storetest.URL(t)
	return openPool(t, dsn), schemaOf(t, dsn)
}

// proxiedHoldPool is holdPool behind a TCP relay the test cuts and restores (testnats.Proxy), as
// the hold's own pool of the default BROKER_NATS_HOLD_POOL_SIZE, 2, reaches Postgres: that pool,
// a direct pool on the same schema for the operator's writes, the relay and the schema.
func proxiedHoldPool(t *testing.T) (proxied, direct *pgxpool.Pool, proxy *testnats.Proxy, schema string) {
	t.Helper()
	dsn := storetest.URL(t)
	proxy = testnats.StartProxy(t, dsn)
	return openPool(t, proxy.URL()+"&pool_max_conns=2"), openPool(t, dsn), proxy, schemaOf(t, dsn)
}

// holdTable writes the hold row on a test Postgres: the operator's signed writes, as the broker's
// routes store them, and a database admin's writes of rows the operator did not sign.
type holdTable struct {
	pool   *pgxpool.Pool
	signer holdSigner
}

// write stores the row at gen in phase under holdID, signed by the operator, with its log row in
// the same statement, as PUT /v1/nats-hold and POST /v1/nats-hold/restore store one, and returns
// the row. It checks no move: the routes' moves are their own tests'.
func (h holdTable) write(t *testing.T, gen uint64, phase natsgrant.OperatorPhase, holdID string) natshold.Reading {
	t.Helper()
	row := h.signer.reading(t, gen, phase, holdID)
	_, err := h.pool.Exec(context.Background(), `with written as (
			insert into nats_hold (id, gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig)
			values (1, $1, $2, $3, 'test', now(), $4, $5, $6)
			on conflict (id) do update set gen = excluded.gen, phase = excluded.phase, hold_id = excluded.hold_id,
				reason = excluded.reason, updated_at = excluded.updated_at, updated_by = excluded.updated_by,
				signed_at = excluded.signed_at, sig = excluded.sig
			returning gen, phase, hold_id, reason, signed_at, sig)
		insert into nats_hold_log (gen, phase, hold_id, reason, written_at, written_by, signed_at, sig)
		select gen, phase, hold_id, reason, now(), $4, signed_at, sig from written`,
		int64(row.Gen), row.Phase, row.HoldID, operatorAccount, row.SignedAt, row.Sig)
	require.NoError(t, err, "write the hold row at gen %d", gen)
	return row
}

// replace is a database admin's write of row over the hold row, as it is, with no log row.
func (h holdTable) replace(t *testing.T, row natshold.Reading) {
	t.Helper()
	h.admin(t, `update nats_hold set gen = $1, phase = $2, hold_id = $3, signed_at = $4, sig = $5 where id = 1`,
		int64(row.Gen), row.Phase, row.HoldID, row.SignedAt, row.Sig)
}

// admin runs statement as a database admin writing around the routes.
func (h holdTable) admin(t *testing.T, statement string, args ...any) {
	t.Helper()
	_, err := h.pool.Exec(context.Background(), statement, args...)
	require.NoError(t, err, "%s", statement)
}

// A row the operator did not sign, or an older row it signed, reads unknown to a running broker,
// on the broker's Postgres: after the operator's signed held at gen 3, a database admin's direct
// update of the phase to released, a row signed by another key, and the operator's own older
// released at gen 2 written back each read unknown, and each refuses a listener hold_unknown. A
// new view, as after the broker's restart, has no floor and accepts the older released: the
// residual the hold's design states. (That the routes refuse an unsigned PUT with 400 is the
// routes' own test.)
//
// Run by hand, the control is a view that does not verify: the admin's released admits the
// listener, and the test fails naming it.
func TestAnUnsignedOrReplayedHoldRowReadsUnknown(t *testing.T) {
	pool, schema := holdPool(t)
	table := holdTable{pool: pool, signer: newHoldSigner(t)}
	now := time.Now()
	registry := metrics.New()
	read := func(ctx context.Context) (natshold.Reading, bool, error) { return natshold.Read(ctx, pool, schema) }
	running := Deps{
		ServiceToken: serviceTokens(map[string]platformToken{"listener-token": {account: listenerAccount, issued: now, expiry: now.Add(time.Hour)}}),
		Hold:         NewHoldView(read, table.signer.HoldKeys, nil, NewMetrics(registry)),
	}
	listener := func(deps Deps) error {
		_, err := Decide(t.Context(), deps, holdServices, serviceRequest("listener-token"))
		return err
	}
	requireUnknown := func(what string, cause error) {
		t.Helper()
		phase, known := running.Hold.State(t.Context())
		require.False(t, known, "%s read as %s", what, phase)
		err := listener(running)
		var refused *Refusal
		require.True(t, errors.As(err, &refused), "the listener %s was admitted after %s", listenerAccount, what)
		require.Equal(t, labelHoldUnknown, refused.Label)
		require.ErrorIs(t, refused.Cause, cause, "%s", what)
	}

	released := table.write(t, 2, natsgrant.OperatorPhaseReleased, "hold-1")
	held := table.write(t, 3, natsgrant.OperatorPhaseHeld, "hold-1")
	phase, known := running.Hold.State(t.Context())
	require.True(t, known)
	require.Equal(t, natsgrant.OperatorPhaseHeld, phase)
	require.NoError(t, listener(running), "a listener was refused under the operator's signed held")

	table.admin(t, `update nats_hold set phase = 'released' where id = 1`)
	requireUnknown("the admin's update of the phase to released", signedrecord.ErrBadSignature)

	table.replace(t, newHoldSigner(t).reading(t, 3, natsgrant.OperatorPhaseReleased, "hold-1"))
	requireUnknown("a released row signed by another key", signedrecord.ErrBadSignature)

	table.replace(t, released)
	requireUnknown("the operator's older released at gen 2, written back", signedrecord.ErrReplay)
	require.Equal(t, "1", holdUnknownGauge(t, registry))

	restarted := running
	restarted.Hold = NewHoldView(read, table.signer.HoldKeys, nil, nil)
	phase, known = restarted.Hold.State(t.Context())
	require.True(t, known, "a new view did not accept the older signed row: the residual is not what the design states")
	require.Equal(t, natsgrant.OperatorPhaseReleased, phase)
	require.NoError(t, listener(restarted))

	table.replace(t, held)
	phase, known = running.Hold.State(t.Context())
	require.True(t, known, "the operator's signed held at the floor read as unknown")
	require.Equal(t, natsgrant.OperatorPhaseHeld, phase)
}

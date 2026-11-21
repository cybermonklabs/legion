package natscallout

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/metrics"
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

// holdAnswer is one answer a stub HoldRead gives: the row and whether one was found, or an error.
type holdAnswer struct {
	reading natshold.Reading
	found   bool
	err     error
}

// rowOf is the answer that finds reading.
func rowOf(reading natshold.Reading) holdAnswer { return holdAnswer{reading: reading, found: true} }

// holdStub is a HoldRead whose answer the test sets, counting each call. It is safe to read from
// the auth service's decisions while the test sets the next answer.
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

// holdUnknownGauge is envoy_broker_hold_unknown's value as registry renders it, or "" for none.
func holdUnknownGauge(t *testing.T, registry *metrics.Registry) string {
	t.Helper()
	for _, line := range strings.Split(scrape(t, registry), "\n") {
		if value, ok := strings.CutPrefix(line, "envoy_broker_hold_unknown "); ok {
			return value
		}
	}
	return ""
}

// The hold view answers each phase the row holds, known, and unknown for everything else, at once
// and whatever it read before: no row; a phase outside the three; a Postgres error; a timeout,
// after a reading of each phase; a row no listed key signed, or none; and a verified gen below the
// highest this view has read. It reads the row at every State, and envoy_broker_hold_unknown is 1
// exactly while its last reading was unknown. A view with no HoldRead, the setting off, answers
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
			view := newHoldView(Deps{HoldRead: stub.read, HoldKeys: signer.HoldKeys, Metrics: NewMetrics(registry)})
			for index, step := range row.steps {
				stub.set(step.answer)
				phase, known := view.State(t.Context())
				require.Equal(t, step.known, known, "reading %d: known", index+1)
				require.Equal(t, step.phase, phase, "reading %d: phase", index+1)
				gauge := "0"
				if !step.known {
					gauge = "1"
				}
				require.Equal(t, gauge, holdUnknownGauge(t, registry), "reading %d: envoy_broker_hold_unknown", index+1)
			}
			require.Equal(t, len(row.steps), stub.called(), "the view did not read the row at every State")
		})
	}
	t.Run("no HoldRead: the setting off", func(t *testing.T) {
		registry := metrics.New()
		view := newHoldView(Deps{HoldKeys: signer.HoldKeys, Metrics: NewMetrics(registry)})
		phase, known := view.State(t.Context())
		require.True(t, known)
		require.Equal(t, natsgrant.OperatorPhaseReleased, phase)
		require.Equal(t, "0", holdUnknownGauge(t, registry))
	})
}

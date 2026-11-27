package cistore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// maxRecordBytes and maxSettlementBytes bound a head's record and the settlement published from
// it, each of which goes to NATS whole. GitHub allows 50,000 check runs in one check suite, so the
// number of check names a head collects has no bound of GitHub's own. The record is kept well under
// the server's 1 MiB default, since it is written whole on every observation; about 1,300 checks of
// ordinary names fit. The settlement needs its own bound, because a name costs more there: a
// failing check is named three times to the record's two, inside a JSON string in the envelope's
// JSON, so a `"` costs 12 bytes a character in the settlement against 4 in the record. Its bound
// is 64 KiB under the 1 MiB default, room for what a later settlement of the same record adds (a
// longer generation, `superseded_settlement`).
const (
	maxRecordBytes     = 384 << 10
	maxSettlementBytes = 1<<20 - 64<<10
)

// write applies mutate to the current state of identity's record, a new one carrying only the
// identity when there is none, and writes it with a compare-and-swap. A lost compare-and-swap or a
// transient KV error (kvErrorLasts) is retried from a fresh read until recordBudget runs out: the
// write carries every observation of a batch, so a NATS reconnect or a JetStream 503 during a
// server restart must not fail them all, and GitHub does not redeliver a delivery the listener
// refused. A lasting error fails it at once. Each attempt takes the handle the latest Rewatch
// installed, and each KV call is given up on if the store is rewatched before it is answered. A KV
// call is also given up on when ctx ends (errKVUnanswered, the caller's wait: update), which fails
// the write at once, since no retry fits in an ended ctx. A write that runs out of the budget, or
// of ctx, logs `ci record exceeded its retry budget`, one JSON line naming the head, the checks the
// record would have held (0 when no attempt read it), the attempts it made, the observations it
// carried (each a delivery the webhook answers 503) and the last attempt's error, so an alarm can
// count those 503s by their cause. It is an ERROR because each one means GitHub was answered 503.
//
// A write given up on that way may still land after its callers have answered. It is a
// compare-and-swap at the revision its attempt read (a create when it read none), so it never
// overwrites a write that landed since: that write moved the revision and this one conflicts and
// is dropped. If it lands, the record holds the batch's observations although each delivery was
// answered 503, and the redelivery of one finds its observation there, which its mutation's rules
// apply as no change, so nothing is written twice.
//
// The record's bounds: a write that would take the record past maxRecordBytes, or its settlement
// past maxSettlementBytes, is refused with bus.ErrTooLarge, which a redelivery would meet again,
// and commit statuses give way before a check run or check suite does, so a status never costs a
// head the settlements of its check runs (atBounds). A write that passes a bound only for the
// statuses it carries records none of them; one that passes it only for the statuses the record
// holds drops them. A write past its bound with no status left to give way is refused: the record
// is written as it was, marked Overflowed, so it never settles on the checks it could not hold.
// Each rule judges the write as its batch carries it, every caller's observation applied.
func (s *Store) write(ctx context.Context, identity State, b *batch) error {
	key := Key(identity.Owner, identity.Repo, identity.Number, identity.SHA)
	deadline := time.Now().Add(recordBudget)
	var retryErr error
	checks := 0
	spent := func(attempts int, err error) error {
		s.logger.Error("ci record exceeded its retry budget",
			slog.String("owner", identity.Owner),
			slog.String("repo", identity.Repo),
			slog.String("number", identity.Number),
			slog.String("sha", identity.SHA),
			slog.Int("checks", checks),
			slog.Int("attempts", attempts),
			slog.Int("observations", len(b.pending)),
			slog.String("error", err.Error()))
		return err
	}
	for attempt := 0; ; attempt++ {
		if retryErr != nil {
			if time.Now().After(deadline) {
				return spent(attempt, retryErr)
			}
			time.Sleep(casBackoff(attempt - 1))
		}
		kv := s.kv()
		entry, err := kvCall(ctx, s.logger, s.nextRewatch(), func() (nats.KeyValueEntry, error) { return kv.Get(key) })
		var rev uint64
		switch {
		case err == nil:
			rev = entry.Revision()
		case errors.Is(err, nats.ErrKeyNotFound):
		case errors.Is(err, errKVUnanswered):
			return spent(attempt+1, err)
		case kvErrorLasts(err):
			return err
		default:
			retryErr = err
			continue
		}
		// stored is the record as this attempt read it, decoded afresh at each call rather than
		// copied: apply changes a record in place, and the maps a shallow copy of the read would
		// share are the ones it changed.
		stored := func() (State, error) {
			st := identity
			if rev != 0 {
				if err := json.Unmarshal(entry.Value(), &st); err != nil {
					return State{}, err
				}
			}
			return st, nil
		}
		st, err := stored()
		if err != nil {
			return err
		}
		beforeHash := st.Hash()
		generation := st.Generation
		if !b.apply(&st, true) {
			return nil
		}
		checks = len(st.Checks)
		buf, err := sealRecord(key, &st, rev != 0, beforeHash, generation)
		// bounded is what a write past a bound writes in the batch's place, and the zero
		// boundWrite for a write within its bounds, which warns of nothing and answers nil.
		var bounded boundWrite
		if errors.Is(err, bus.ErrTooLarge) {
			if bounded, err = s.atBounds(key, stored, rev != 0, b, err); err != nil {
				return err
			}
			if bounded.buf == nil {
				bounded.warn(s.logger, identity)
				return bounded.refused
			}
			buf = bounded.buf
		} else if err != nil {
			return err
		}
		if s.casHook != nil {
			s.casHook(key)
		}
		_, err = kvCall(ctx, s.logger, s.nextRewatch(), func() (uint64, error) {
			if rev == 0 {
				return kv.Create(key, buf)
			}
			return kv.Update(key, buf, rev)
		})
		switch {
		case err == nil:
			bounded.warn(s.logger, identity)
			return bounded.refused
		case errors.Is(err, errKVUnanswered):
			return spent(attempt+1, err)
		case kvErrorLasts(err):
			return err
		case bus.IsCASConflict(err):
			retryErr = errors.New("cistore: record exceeded CAS budget")
		default:
			retryErr = err
		}
	}
}

// sealRecord makes st, a record its write changed, ready to write to key: it advances the
// generation where an existing record's snapshot moved and no mutation advanced it (hash and
// generation are the record's as read), encodes it, and answers boundsRefusal's error when the
// record or its settlement would pass its bound.
func sealRecord(key string, st *State, existing bool, hash string, generation uint64) ([]byte, error) {
	if existing && st.Hash() != hash && st.Generation == generation {
		bumpGeneration(st)
	}
	buf, err := encodeRecord(st)
	if err != nil {
		return nil, err
	}
	settlement, err := settlementSize(*st)
	if err != nil {
		return nil, err
	}
	if err := boundsRefusal(key, len(buf), settlement); err != nil {
		return nil, err
	}
	return buf, nil
}

// boundWrite is what a write past a bound writes in its place (atBounds), and what that write
// warns of once it lands (warn).
type boundWrite struct {
	// buf is the record to write, and nil where the record stays as it was.
	buf []byte
	// refused is the write's answer: nil where it fits once commit statuses gave way, and the
	// bound's refusal where the record is marked Overflowed or already was.
	refused error
	// refusedStatuses is the contexts of the batch's commit statuses that gave way, and
	// refusedFor the refusal of the batch applied whole, which they gave way to.
	refusedStatuses []string
	refusedFor      error
	// dropped is how many statuses the record held that it dropped to keep a check within its
	// bound, zero where it dropped none, and droppedFor the refusal they were dropped for.
	dropped    int
	droppedFor error
}

// warn logs what gave way at the bound, each line naming head: one WARN line for each of the
// batch's statuses that gave way, and one line for the record's own statuses it dropped, with
// how many. write calls it once the write lands, or at once when there is nothing to write.
func (w boundWrite) warn(logger *logging.Logger, head State) {
	if len(w.refusedStatuses) == 0 && w.dropped == 0 {
		return
	}
	attrs := []slog.Attr{
		slog.String("owner", head.Owner),
		slog.String("repo", head.Repo),
		slog.String("number", head.Number),
		slog.String("sha", head.SHA),
	}
	for _, name := range w.refusedStatuses {
		logger.Warn("ci record refused a commit status past its bound",
			append(attrs, slog.String("context", name), slog.String("error", w.refusedFor.Error()))...)
	}
	if w.dropped > 0 {
		logger.Warn("ci record dropped its commit statuses to keep a check within its bound",
			append(attrs, slog.Int("statuses", w.dropped), slog.String("error", w.droppedFor.Error()))...)
	}
}

// atBounds is the write of b to key's record when the record it would write, b applied whole,
// passes a bound (refused, boundsRefusal's error). Commit statuses give way, in this order, so a
// status never costs the head the settlements of its check runs:
//
//   - The batch is applied again without the statuses it carries. None of them that changed the
//     record is recorded: each logs one WARN line naming the head and the context, and counts as
//     no change (batch.answer). With nothing else changed the record stays as it was, not
//     Overflowed, and where the rest fits it is written without them.
//   - Where the rest still passes a bound and the record holds statuses, the rest is applied once
//     more, to the record without its statuses: where that fits it is written so, the record's
//     statuses dropped, with one WARN line naming the head and how many it dropped.
//   - Otherwise the record is written as it was, marked Overflowed, as a record with no status to
//     give way always was (one already Overflowed is not written again), and the write answers the
//     refusal.
//
// stored is the record as the write's attempt read it, decoded afresh at each call, and existing
// says the attempt read one. A step that still passes a bound refuses the write afresh, and the
// latest refusal is the one the record's dropped statuses and the write's answer name; a status
// of the batch that gave way names the refusal of the batch applied whole.
func (s *Store) atBounds(key string, stored func() (State, error), existing bool, b *batch, refused error) (boundWrite, error) {
	bounded := boundWrite{refusedStatuses: b.statuses(), refusedFor: refused}
	// fit applies b, without the statuses it carries, to the record as read, the record's own
	// statuses dropped first when drop is set. It answers the record to write, nil where b then
	// changes nothing, or the bound's refusal (bus.ErrTooLarge) where the result still passes a
	// bound.
	fit := func(drop bool) ([]byte, error) {
		st, err := stored()
		if err != nil {
			return nil, err
		}
		hash, generation := st.Hash(), st.Generation
		if drop {
			st.Statuses = nil
		}
		if !b.apply(&st, false) {
			return nil, nil
		}
		return sealRecord(key, &st, existing, hash, generation)
	}
	if len(bounded.refusedStatuses) > 0 {
		buf, err := fit(false)
		if err == nil {
			bounded.buf = buf
			return bounded, nil
		}
		if !errors.Is(err, bus.ErrTooLarge) {
			return boundWrite{}, err
		}
		refused = err
	}
	st, err := stored()
	if err != nil {
		return boundWrite{}, err
	}
	if held := len(st.Statuses); held > 0 {
		buf, err := fit(true)
		if err == nil {
			if buf != nil {
				bounded.dropped, bounded.droppedFor = held, refused
			}
			bounded.buf = buf
			return bounded, nil
		}
		if !errors.Is(err, bus.ErrTooLarge) {
			return boundWrite{}, err
		}
		refused = err
	}
	bounded.refused = refused
	if st.Overflowed {
		return bounded, nil
	}
	st.Overflowed = true
	if bounded.buf, err = encodeRecord(&st); err != nil {
		return boundWrite{}, err
	}
	return bounded, nil
}

// boundsRefusal is the refusal of a write to key's record of record bytes whose settlement is
// settlement bytes, when either is past its bound (maxRecordBytes, maxSettlementBytes), and nil
// otherwise.
func boundsRefusal(key string, record, settlement int) error {
	switch {
	case record > maxRecordBytes:
		return fmt.Errorf("%w: head %s's record would be %d bytes, past its %d-byte bound",
			bus.ErrTooLarge, key, record, maxRecordBytes)
	case settlement > maxSettlementBytes:
		return fmt.Errorf("%w: head %s's settlement would be %d bytes, past its %d-byte bound",
			bus.ErrTooLarge, key, settlement, maxSettlementBytes)
	}
	return nil
}

// settlementSize is the size of the settlement st would publish, encoded as the summary loop sends
// it; its topic travels in the protocol line, which a server's max payload does not count.
func settlementSize(st State) (int, error) {
	env, err := settlementEnvelope(st, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	data, err := json.Marshal(env)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// errKVRewatched is what a KV call answers when the store was rewatched before its answer came.
var errKVRewatched = errors.New("cistore: store rewatched before the KV call was answered")

// errKVUnanswered is what a KV call answers, wrapping ctx's error, when ctx ended before its
// answer came.
var errKVUnanswered = errors.New("cistore: the KV call did not answer")

// kvCall returns call's answer, errKVRewatched if rewatched closes first, or errKVUnanswered if ctx
// ends first; a ctx that has already ended starts no call. A legacy nats.go KV call takes no
// context and waits up to the JetStream MaxWait for its answer. One sent just before the server
// went away gets none, so it would hold its write, and every caller queued behind the write, for
// the whole wait. Rewatch runs on every reconnect, so the call is given up on then and retried on
// the new connection within the write's budget. Rewatch also runs when the listener's self-health
// rebuilds a watcher on a live connection; a call given up on there may still have been answered,
// and is retried like any other. Otherwise a call is waited for until ctx ends (the caller's wait,
// two budgets at most: update), even past the write's budget, so a server that stalls and then
// answers within that wait still answers that call, and one that stays silent fails the write when
// it ends rather than at the MaxWait. A stall of a few seconds can also fail a batch whose
// compare-and-swap then loses, since such an attempt returns rather than reading again once the
// budget is spent (write). This is bus.Bounded with the rewatch and the panic handling below added.
// A call given up on ends on its own, with its answer or at the MaxWait. A write it carried is a
// compare-and-swap at the revision its attempt read, so if the server applies it after the retry's
// write it conflicts, and if before, the retry's fresh read finds the batch's observations already
// there and writes nothing; one given up on at ctx's end has no retry, and write says why its
// landing late is safe too. A call that panics re-panics in its caller, as it would without the
// goroutine, so the batch fails (writeBatch); one that panics after its caller has moved on is
// logged through logger.
func kvCall[T any](ctx context.Context, logger *logging.Logger, rewatched <-chan struct{}, call func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, fmt.Errorf("%w: %w", errKVUnanswered, err)
	}
	type answer struct {
		value    T
		err      error
		panicked any
	}
	answered := make(chan answer, 1)
	go func() {
		var a answer
		defer func() {
			if p := recover(); p != nil {
				a = answer{panicked: p}
			}
			answered <- a
		}()
		a.value, a.err = call()
	}()
	giveUp := func(at string) {
		go func() {
			if a := <-answered; a.panicked != nil {
				logger.Error("cistore: a KV call given up on at "+at+" panicked", slog.String("panic", fmt.Sprint(a.panicked)))
			}
		}()
	}
	select {
	case a := <-answered:
		if a.panicked != nil {
			panic(a.panicked)
		}
		return a.value, a.err
	case <-rewatched:
		giveUp("a rewatch")
		return zero, errKVRewatched
	case <-ctx.Done():
		giveUp("its context's end")
		return zero, fmt.Errorf("%w: %w", errKVUnanswered, ctx.Err())
	}
}

// nextRewatch returns the channel the next Rewatch closes.
func (s *Store) nextRewatch() <-chan struct{} {
	s.rewatchMu.Lock()
	defer s.rewatchMu.Unlock()
	return s.rewatched
}

// kvErrorLasts reports whether a KV error will outlast a retry within a write's budget. A
// compare-and-swap conflict never does: both kinds are JetStream 400s (ErrKeyExists, wrong last
// sequence), so it is answered first, and a caller need not ask bus.IsCASConflict before it. Lasting:
// the connection is closed, draining or invalid; the connection is not allowed the request
// (authorization, an expired credential, a permissions violation); the bucket is not there when
// the handle is taken; JetStream is not enabled for the server or the account, which JetStream
// reports as a 503 but which is configuration no retry within the budget changes, unlike the 503
// of a server restarting; JetStream refuses the request itself (any other 4xx, a record over the
// payload limit); or the handle refuses the key (bus.ErrRefused, bus.EnsureKeyValue: one whose
// subject NATS would refuse, or one outside nats.go's key alphabet). Anything else is transient,
// and what the retry actually rescues is a NATS reconnect (ErrReconnectBufExceeded, a request
// refused while the connection reconnects, and errKVRewatched, a request given up on at the
// rewatch that follows it), a JetStream 503 while a server restarts, and no responders, which a
// request gets whenever no server answers for the stream it names. A timeout is transient too,
// though one at the JetStream MaxWait never comes: the caller's wait (update, two budgets) ends
// first, and the call is given up on then (errKVUnanswered, which write fails on first).
//
// No responders is transient because it cannot be told apart from a restart: a restarting server
// answers it for as long as it is away, and a batch that should have been retried failed instead.
// It is the read (kv.Get) that this decides: on the write side nats.go answers ErrNoStreamResponse
// instead (js.go), which this never named as lasting. The trade is the case it also covers, a
// bucket deleted under a live handle, which answers no responders like a key no stream serves:
// such a write now spends the whole budget before it fails, rather than failing at once. It still
// fails within the budget, and nothing is written either way; a caller queued behind that write
// waits for its own write too, so it can wait two budgets (cistore.go).
func kvErrorLasts(err error) bool {
	if bus.IsCASConflict(err) {
		return false
	}
	for _, lasting := range lastingKVErrors {
		if errors.Is(err, lasting) {
			return true
		}
	}
	var jsErr nats.JetStreamError
	if errors.As(err, &jsErr) {
		if api := jsErr.APIError(); api != nil && api.Code >= 400 && api.Code < 500 {
			return true
		}
	}
	return false
}

// lastingKVErrors are the errors kvErrorLasts names by value.
var lastingKVErrors = []error{
	nats.ErrConnectionClosed, nats.ErrConnectionDraining, nats.ErrInvalidConnection,
	nats.ErrAuthorization, nats.ErrAuthExpired, nats.ErrPermissionViolation,
	nats.ErrBucketNotFound,
	nats.ErrJetStreamNotEnabled, nats.ErrJetStreamNotEnabledForAccount,
	nats.ErrMaxPayload, bus.ErrRefused,
}

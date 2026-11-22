package cistore

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// An intake gap is a time the listener may have missed GitHub's webhooks: a head's record cannot
// tell "no more checks" from "their events never reached me", so a head the gap may have touched
// settles only once Dispatch's CI reconcile has read it from GitHub after the gap (State.ReadAt).
// There are two kinds.
//
//   - A head's own gap (State.GapAt): the webhook handler could not write one of the head's
//     observations (Store.Record, Store.RecordSuite), so the head is known. The listener holds the
//     head at once in memory (pendingGaps) and writes the mark into the record as soon as the bus
//     answers (persistGaps), so every replica refuses the head from then on.
//   - The whole intake's gap (intakeGapKey): deliveries that failed while the listener was down,
//     while its start-up gate refused them, or before the handler could name a head. The listener
//     stamps the time its webhooks open (MarkIntakeGap), and every head whose record has no read
//     after that stamp waits for one. A reconcile cycle that began after the stamp has read every
//     open pull request's head, so once one completes (readCycleKey) the stamp holds no head: a
//     head first seen after that cycle began had its check runs created after it.
//
// The summary loop names the heads that are due but held by a gap alone in one key of the bucket
// (gapHeldKey), and the reconcile reads those heads first in each pass, so a head waiting after a
// gap waits about one pass, not a whole cycle.
//
// Only a reconcile read clears a gap, so a gap holds only where a reconcile is known to serve the
// bucket: where it has completed a cycle (readCycleKey). A listener with no reconcile behind it --
// a development stack, a deployment without Dispatch's App key -- settles as it did before, since
// nothing would ever free a head it held.
const (
	// intakeGapKey, readCycleKey and gapHeldKey are the bucket's own keys beside the records. A
	// record's key always has four segments (Key), and these have one, so none is a record's.
	intakeGapKey = "intake-gap"
	readCycleKey = "read-cycle"
	gapHeldKey   = "gap-held"

	// maxGapHeld bounds the heads gapHeldKey names: the oldest due first, and a head left out is
	// named once one before it has been read.
	maxGapHeld = 200
)

// Head names one head's record: the identity Key builds its key from. It is contracts.CIHead, so
// a writer that speaks to the store through an interface (Dispatch's CI reconcile) names a head
// without importing this package.
type Head = contracts.CIHead

// headKey is the key of head's record.
func headKey(head Head) string {
	return Key(head.Owner, head.Repo, head.Number, head.SHA)
}

func headOf(st State) Head {
	return Head{Owner: st.Owner, Repo: st.Repo, Number: st.Number, SHA: st.SHA}
}

// stamp is the value of intakeGapKey and readCycleKey: a wall-clock millisecond. Kind names the key
// so a value read alone says what it is.
type stamp struct {
	Kind string `json:"kind"`
	At   int64  `json:"at"`
}

// gapHeldList is the value of gapHeldKey.
type gapHeldList struct {
	Kind  string `json:"kind"`
	Heads []Head `json:"heads"`
}

// pendingGap is a head's own gap that the bucket does not hold yet.
type pendingGap struct {
	head Head
	at   int64
}

// gapHolds reports whether st waits for a GitHub read: a gap of its own head (GapAt), or the whole
// intake's stamp while no read cycle has begun after it (intakeGap, readCycle), with no read of the
// head after it. A read at the very millisecond of a gap does not count: it may have begun before
// the failure.
func gapHolds(st State, intakeGap, readCycle int64) bool {
	if readCycle == 0 {
		// No reconcile has completed a cycle over this bucket, so nothing would read a head held.
		return false
	}
	gap := st.GapAt
	if readCycle <= intakeGap && intakeGap > gap {
		gap = intakeGap
	}
	return gap != 0 && st.ReadAt <= gap
}

// MarkIntakeGap stamps the whole intake at at: every head whose record has no GitHub read after
// it waits for one, until a reconcile cycle that began after it completes. The listener calls it
// once its webhooks open, before its summary loop starts. This store holds the stamp at once; the
// summary loop writes it into the bucket, again at every pass until the bus answers.
//
// Until the summary loop has read the bucket's read cycle (persistGaps), every head is held:
// whether a reconcile serves the bucket decides whether any gap holds (gapHolds), the cache's
// watcher may deliver that key after the records it would hold, and a read that does not answer
// is the outage the gaps are for. Once the read answers, the gaps hold where the key exists, and
// nothing holds where it does not; the watcher turns them on when the key first appears later.
func (s *Store) MarkIntakeGap(at time.Time) {
	ms := at.UnixMilli()
	s.gapMu.Lock()
	s.intakeGap = max(s.intakeGap, ms)
	s.intakeGapUnsaved = max(s.intakeGapUnsaved, ms)
	s.reconcileUnknown = true
	s.gapEpoch++
	s.gapMu.Unlock()
	s.wakeSummary()
}

// readReadCycle reads the bucket's read cycle for the start's check (MarkIntakeGap): its value,
// and whether the bucket holds the key.
func (s *Store) readReadCycle(ctx context.Context) ([]byte, bool, error) {
	if s.startRead != nil {
		return s.startRead(ctx)
	}
	kv := s.kv()
	entry, err := kvCall(ctx, s.logger, s.nextRewatch(), func() (nats.KeyValueEntry, error) { return kv.Get(readCycleKey) })
	if errors.Is(err, nats.ErrKeyNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return entry.Value(), true, nil
}

// noteGap records a head's own gap when the webhook handler could not write one of its
// observations. A refusal for the record's bounds is no gap: the record is written marked
// Overflowed and never settles.
func (s *Store) noteGap(observation contracts.CIObservation, err error) {
	if err == nil || errors.Is(err, bus.ErrTooLarge) {
		return
	}
	head := Head{Owner: observation.Owner, Repo: observation.Repo, Number: observation.Number, SHA: observation.SHA}
	at := time.Now().UnixMilli()
	s.gapMu.Lock()
	if s.pendingGaps == nil {
		s.pendingGaps = map[string]pendingGap{}
	}
	key := headKey(head)
	held := s.pendingGaps[key]
	s.pendingGaps[key] = pendingGap{head: head, at: max(held.at, at)}
	s.gapMu.Unlock()
	s.wakeSummary()
}

// gapHoldsFor reports whether a gap holds the record st cached under key: one this store has not
// written into the bucket yet, or one the record or the whole intake carries (gapHolds).
func (s *Store) gapHoldsFor(key string, st State) bool {
	s.gapMu.Lock()
	defer s.gapMu.Unlock()
	if s.reconcileUnknown {
		// The start has not read yet whether a reconcile serves the bucket (MarkIntakeGap).
		return true
	}
	if _, pending := s.pendingGaps[key]; pending && s.readCycle != 0 {
		return true
	}
	return gapHolds(st, s.intakeGap, s.readCycle)
}

// gapsMoved answers a number that changes whenever a stamp this store knows of moves, so the summary
// loop files the heads a gap held again.
func (s *Store) gapsMoved() uint64 {
	s.gapMu.Lock()
	defer s.gapMu.Unlock()
	return s.gapEpoch
}

// persistGaps reads, at a start, whether a reconcile serves the bucket (MarkIntakeGap), and writes
// into the bucket the gaps this store holds that the bucket does not: the whole intake's stamp, and
// each head's own gap (GapAt, the newest of it and the record's). It stops at the first read or
// write that fails, since the bus is then not answering, and reports whether everything is done;
// the summary loop runs it again at its next pass. A read of the read cycle that fails is an ERROR
// at every try, since every head stays held until it answers. A head whose record is over its
// bound counts as written: it never settles.
func (s *Store) persistGaps(ctx context.Context) bool {
	s.gapMu.Lock()
	unknown := s.reconcileUnknown
	s.gapMu.Unlock()
	if unknown {
		value, found, err := s.readReadCycle(ctx)
		if err != nil {
			s.logger.Error("cistore read cycle not read; every head is held until it is", slog.String("error", err.Error()))
			return false
		}
		if found {
			s.applyStamp(kvwatch.Entry{Key: readCycleKey, Value: value})
		}
		s.gapMu.Lock()
		s.reconcileUnknown = false
		s.gapEpoch++
		s.gapMu.Unlock()
	}
	s.gapMu.Lock()
	unsaved := s.intakeGapUnsaved
	pending := make([]pendingGap, 0, len(s.pendingGaps))
	for _, gap := range s.pendingGaps {
		pending = append(pending, gap)
	}
	s.gapMu.Unlock()
	if unsaved != 0 {
		if err := s.raiseStamp(ctx, intakeGapKey, unsaved); err != nil {
			s.logger.Warn("cistore intake gap not written yet", slog.String("error", err.Error()))
			return false
		}
		s.gapMu.Lock()
		if s.intakeGapUnsaved == unsaved {
			s.intakeGapUnsaved = 0
		}
		s.gapMu.Unlock()
	}
	for _, gap := range pending {
		at := gap.at
		_, err := s.update(ctx, gap.head.Owner, gap.head.Repo, gap.head.Number, gap.head.SHA, "", func(st *State) bool {
			if st.GapAt >= at {
				return false
			}
			st.GapAt = at
			return true
		})
		if err != nil && !errors.Is(err, bus.ErrTooLarge) {
			s.logger.Warn("cistore head gap not written yet",
				slog.String("sha", gap.head.SHA), slog.String("number", gap.head.Number), slog.String("error", err.Error()))
			return false
		}
		s.gapMu.Lock()
		if held, ok := s.pendingGaps[headKey(gap.head)]; ok && held.at == at {
			delete(s.pendingGaps, headKey(gap.head))
		}
		s.gapMu.Unlock()
	}
	return true
}

// raiseStamp writes at into the stamp key unless the bucket holds a later one, by compare-and-swap.
func (s *Store) raiseStamp(ctx context.Context, key string, at int64) error {
	buf, err := json.Marshal(stamp{Kind: key, At: at})
	if err != nil {
		return err
	}
	kv := s.kv()
	deadline := time.Now().Add(recordBudget)
	for attempt := 0; ; attempt++ {
		entry, err := kvCall(ctx, s.logger, s.nextRewatch(), func() (nats.KeyValueEntry, error) { return kv.Get(key) })
		switch {
		case errors.Is(err, nats.ErrKeyNotFound):
			_, err = kvCall(ctx, s.logger, s.nextRewatch(), func() (uint64, error) { return kv.Create(key, buf) })
		case err != nil:
			return err
		default:
			var held stamp
			if json.Unmarshal(entry.Value(), &held) == nil && held.At >= at {
				return nil
			}
			_, err = kvCall(ctx, s.logger, s.nextRewatch(), func() (uint64, error) { return kv.Update(key, buf, entry.Revision()) })
		}
		if err == nil {
			return nil
		}
		if !bus.IsCASConflict(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(casBackoff(attempt))
	}
}

// readStamp reads one stamp key, 0 when the bucket holds none.
func (s *Store) readStamp(key string) (int64, error) {
	entry, err := s.kv().Get(key)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var held stamp
	if err := json.Unmarshal(entry.Value(), &held); err != nil {
		return 0, fmt.Errorf("cistore: %s holds no stamp: %w", key, err)
	}
	return held.At, nil
}

// applyStamp takes a stamp key the cache's watcher delivered, and reports whether entry was one of
// the bucket's own keys, which the cache keeps none of. A stamp only ever moves forward here: a
// deleted or expired key leaves what the store knew.
func (s *Store) applyStamp(entry kvwatch.Entry) bool {
	switch entry.Key {
	case intakeGapKey, readCycleKey:
	case gapHeldKey:
		return true
	default:
		return false
	}
	var held stamp
	if json.Unmarshal(entry.Value, &held) != nil {
		return true
	}
	s.gapMu.Lock()
	moved := false
	if entry.Key == intakeGapKey && held.At > s.intakeGap {
		s.intakeGap, moved = held.At, true
	}
	if entry.Key == readCycleKey && held.At > s.readCycle {
		s.readCycle, moved = held.At, true
	}
	if moved {
		s.gapEpoch++
	}
	s.gapMu.Unlock()
	if moved {
		s.wakeSummary()
	}
	return true
}

// saveGapHeld writes the heads due but held by a gap alone into gapHeldKey: the oldest due first,
// at most maxGapHeld of them. With none held it deletes the key, so a list a process before this
// one left sends the reconcile to no head and the bucket holds no key it has no use for.
func (s *Store) saveGapHeld(ctx context.Context, held map[string]gapHeldHead) error {
	kv := s.kv()
	if len(held) == 0 {
		_, err := kvCall(ctx, s.logger, s.nextRewatch(), func() (struct{}, error) { return struct{}{}, kv.Delete(gapHeldKey) })
		return err
	}
	heads := make([]gapHeldHead, 0, len(held))
	for _, head := range held {
		heads = append(heads, head)
	}
	slices.SortFunc(heads, func(a, b gapHeldHead) int {
		if a.since != b.since {
			return cmp.Compare(a.since, b.since)
		}
		return strings.Compare(headKey(a.head), headKey(b.head))
	})
	list := gapHeldList{Kind: gapHeldKey, Heads: make([]Head, 0, min(len(heads), maxGapHeld))}
	for _, head := range heads[:min(len(heads), maxGapHeld)] {
		list.Heads = append(list.Heads, head.head)
	}
	buf, err := json.Marshal(list)
	if err != nil {
		return err
	}
	_, err = kvCall(ctx, s.logger, s.nextRewatch(), func() (uint64, error) { return kv.Put(gapHeldKey, buf) })
	return err
}

// gapHeldHead is a head the summary loop holds by a gap alone, and when it fell due.
type gapHeldHead struct {
	head  Head
	since int64
}

// GapHeldHeads answers the heads the listener's summary loop holds by a gap alone, the oldest due
// first (gapHeldKey); the reconcile reads them first in each pass.
func (w *Writer) GapHeldHeads() ([]Head, error) {
	entry, err := w.store.kv().Get(gapHeldKey)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list gapHeldList
	if err := json.Unmarshal(entry.Value(), &list); err != nil {
		return nil, fmt.Errorf("cistore: %s holds no list of heads: %w", gapHeldKey, err)
	}
	return list.Heads, nil
}

// NeedsRead reports whether the reconcile must read one head from GitHub in full and write it: the
// bucket holds no record of it, or a gap holds its record (gapHolds).
func (w *Writer) NeedsRead(owner, repo, number, sha string) (bool, error) {
	intakeGap, readCycle, err := w.stamps()
	if err != nil {
		return false, err
	}
	entry, err := w.store.kv().Get(Key(owner, repo, number, sha))
	if errors.Is(err, nats.ErrKeyNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var st State
	if err := json.Unmarshal(entry.Value(), &st); err != nil {
		return false, err
	}
	return gapHolds(st, intakeGap, readCycle), nil
}

// stamps answers the whole intake's stamp and the newest read cycle the bucket holds, read once for
// each writer, which the reconcile binds afresh at every pass.
func (w *Writer) stamps() (intakeGap, readCycle int64, err error) {
	w.stampsOnce.Do(func() {
		w.intakeGap, w.stampsErr = w.store.readStamp(intakeGapKey)
		if w.stampsErr == nil {
			w.readCycle, w.stampsErr = w.store.readStamp(readCycleKey)
		}
	})
	return w.intakeGap, w.readCycle, w.stampsErr
}

// MarkReadCycle writes that a reconcile cycle which began at began has read every open pull
// request's head (readCycleKey), unless the bucket holds a later one: from then on the whole
// intake's stamps before began hold no head.
func (w *Writer) MarkReadCycle(ctx context.Context, began time.Time) error {
	return w.store.raiseStamp(ctx, readCycleKey, began.UnixMilli())
}

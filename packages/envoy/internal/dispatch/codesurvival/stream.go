package codesurvival

// stream.go is the stream that hands a run's patches to the replay (R4; plan, "The stream"):
// workers read the patches of the replay range by chain index, and an ordered buffer hands them to
// survival.Measure in chain order. The buffer holds at most its limit of read patches waiting their
// turn; a worker whose patch would pass that waits, except for the patch Measure needs next. A
// patch is released as the stream hands it to Measure, which applies it and keeps none of it once
// it asks for the next (survival.MeasureInput). So the patch bytes the stream holds stay at most
// the limit plus the workers' count times the largest patch read: the buffer's limit, passed by at
// most the patch Measure needs next, and one patch for each other worker, waiting for room.

import (
	"context"
	"errors"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
)

// The stream's shape (R4): eight workers, and 64 MiB of read patches held waiting their turn.
const (
	streamWorkers = 8
	streamBuffer  = 64 << 20
)

// errStreamDone ends the workers of a stream whose replay has finished with it.
var errStreamDone = errors.New("codesurvival: the replay took no more patches")

// stream is one run's ordered buffer of patches. read reads the patch of chain index i; Measure
// takes them through patch, in order.
type stream struct {
	n      int
	limit  int64
	read   func(ctx context.Context, i int) (PatchRead, error)
	cancel context.CancelFunc
	stop   func() bool
	wg     sync.WaitGroup

	mu   sync.Mutex
	cond *sync.Cond
	// assign is the next index a worker takes, and next the index Measure takes next.
	assign, next int
	// ready is each read patch waiting its turn, by index, and buffered the bytes they hold.
	ready    map[int]PatchRead
	buffered int64
	// held is every patch byte read and not yet handed to Measure: the ready ones, and those of
	// the workers waiting for room; peak is the most held at once.
	held, peak int64
	// reasons is why each patch read through files[] in place of a refused diff was refused, by
	// its commit's SHA, the commit at shas' index.
	reasons map[string]string
	shas    []string
	// err is the first failure, which stops every worker and the replay.
	err error
}

// newStream starts workers that read the n patches of a replay range through read, the commits
// shas names by index, each held by the stream until it hands it to Measure. finish ends it.
func newStream(ctx context.Context, n, workers int, limit int64, shas []string, read func(ctx context.Context, i int) (PatchRead, error)) *stream {
	ctx, cancel := context.WithCancel(ctx)
	s := &stream{n: n, limit: limit, read: read, cancel: cancel, ready: map[int]PatchRead{}, reasons: map[string]string{}, shas: shas}
	s.cond = sync.NewCond(&s.mu)
	s.stop = context.AfterFunc(ctx, func() { s.fail(ctx.Err()) })
	for range min(workers, n) {
		s.wg.Add(1)
		go s.work(ctx)
	}
	return s
}

// work reads patches, the next index each time, until none is left or the stream fails, and puts
// each in the buffer once it has room for it or it is the one Measure needs next.
func (s *stream) work(ctx context.Context) {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		if s.err != nil || s.assign >= s.n {
			s.mu.Unlock()
			return
		}
		index := s.assign
		s.assign++
		s.mu.Unlock()

		read, err := s.read(ctx, index)
		s.mu.Lock()
		if err != nil {
			s.failLocked(err)
			s.mu.Unlock()
			return
		}
		size := int64(read.Bytes)
		s.held += size
		s.peak = max(s.peak, s.held)
		for s.err == nil && index != s.next && s.buffered+size > s.limit {
			s.cond.Wait()
		}
		if s.err != nil {
			s.mu.Unlock()
			return
		}
		s.ready[index], s.buffered = read, s.buffered+size
		if read.Reason != "" {
			s.reasons[s.shas[index]] = read.Reason
		}
		s.cond.Broadcast()
		s.mu.Unlock()
	}
}

// patch is Measure's read of the patch of index, which it asks for in chain order: it waits for the
// patch, and releases it as it hands it over.
func (s *stream) patch(index int) (survival.Patch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.err != nil {
			return survival.Patch{}, s.err
		}
		if read, ok := s.ready[index]; ok {
			delete(s.ready, index)
			size := int64(read.Bytes)
			s.buffered, s.held, s.next = s.buffered-size, s.held-size, index+1
			s.cond.Broadcast()
			return read.Patch, nil
		}
		s.cond.Wait()
	}
}

// fail stops the stream at err, unless it stopped already.
func (s *stream) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failLocked(err)
}

func (s *stream) failLocked(err error) {
	if s.err == nil {
		s.err = err
	}
	s.cond.Broadcast()
}

// finish ends the stream once Measure has returned: it stops the workers and waits for them, and
// answers the failure that stopped the stream, nil where none did.
func (s *stream) finish() error {
	s.mu.Lock()
	cause := s.err
	s.failLocked(errStreamDone)
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
	s.stop()
	return cause
}

// peakBytes is the most patch bytes the stream held at once.
func (s *stream) peakBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

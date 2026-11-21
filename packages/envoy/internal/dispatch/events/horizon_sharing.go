package events

import "context"

// horizonFlight is one observation that concurrent callers share. h.inFlight holds it while it
// runs. Once it has ended, h.ended holds it as the newest ended observation, unless its runner's
// cancellation abandoned it. Its fields are written by the caller running it, under h.mu, before
// done closes, and read by the others after done closes or, through h.ended, under h.mu.
type horizonFlight struct {
	number     uint64
	generation uint64
	done       chan struct{}
	err        error
	// abandoned is an observation that failed because its runner's context ended; a waiter
	// whose own context is still live takes a later one rather than return a cancellation not its
	// own, and h.ended never holds it.
	abandoned bool
}

// observationBoundary answers the count of observations started so far, read at a Safe or Await
// call: the call waits for an observation numbered above it.
func (h *Horizon) observationBoundary() uint64 {
	h.mu.Lock()
	after := h.started
	h.mu.Unlock()
	if h.afterObservationBoundary != nil {
		h.afterObservationBoundary()
	}
	return after
}

// observe returns once an observation numbered above after has ended, recorded or failed, and
// answers that observation's number. It takes the newest that has ended (h.ended) when that one is
// numbered above after; otherwise it joins the one in flight, or runs one itself (run) when none
// is. Observations run one at a time, so one numbered above after started after the count after
// was read. One in flight numbered at or below after started before the caller needed it, so the
// caller waits it out and looks again; so does one abandoned by its runner's context while the
// caller's is live, which h.ended never holds.
//
// Callers that read the count while one observation ran, or that wait out the same one, therefore
// run at most one more between them, unless that one is abandoned, however the scheduler wakes
// them: the first back at h.mu runs it, and each of the others joins it in flight or, once it has
// ended, takes it. Each that found it already ended would otherwise run one of its own.
func (h *Horizon) observe(ctx context.Context, after uint64) (uint64, error) {
	for {
		h.mu.Lock()
		if ended := h.ended; ended != nil && ended.number > after {
			number, err := ended.number, ended.err
			h.mu.Unlock()
			return number, err
		}
		flight := h.inFlight
		run := flight == nil
		if run {
			h.started++
			flight = &horizonFlight{number: h.started, generation: h.generation, done: make(chan struct{})}
			h.inFlight = flight
		}
		h.mu.Unlock()
		if run {
			h.run(ctx, flight)
		} else {
			if h.afterObservationParked != nil {
				h.afterObservationParked()
			}
			select {
			case <-flight.done:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		if flight.number <= after || (flight.abandoned && ctx.Err() == nil) {
			continue
		}
		return flight.number, flight.err
	}
}

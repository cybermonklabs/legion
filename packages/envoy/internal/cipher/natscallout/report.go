package natscallout

import (
	"log/slog"
	"slices"
	"sync"
	"time"
)

// logInterval is the least time between two log lines of one kind while the replica runs: the
// first event after a quiet interval is logged at the reporter's next tick, and those that follow
// it inside the interval are counted and wait for the next line. A closing replica's last line
// comes whenever Close does (report's final).
const logInterval = time.Minute

// tally counts events by key between two log lines, and keeps the detail of each key's first. It
// is the service's one counting scheme for what it logs: refusals by label, failed answers by
// step, and drops by reason, which reach it from the drops hand-off (countDrops). Its lock is held
// only to count, never across a write, so it never waits on the log, and what it keeps between two
// lines is a count and one detail per key, whatever the log does meanwhile. The keys are fixed
// words, so they are bounded too.
type tally struct {
	mu     sync.Mutex
	counts map[string]int64
	first  map[string]string
	logged time.Time
}

// add counts one event under key, and keeps detail's string when it is key's first since the last
// line; detail is called only then.
func (t *tally) add(key string, detail func() string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	if t.counts[key] == 0 {
		t.first[key] = detail()
	}
	t.counts[key]++
}

// addCount counts n events under key, and keeps detail as key's first when key has none kept since
// the last line and detail is not empty.
func (t *tally) addCount(key string, n int64, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	if _, kept := t.first[key]; !kept && detail != "" {
		t.first[key] = detail
	}
	t.counts[key] += n
}

func (t *tally) init() {
	if t.counts == nil {
		t.counts, t.first = make(map[string]int64), make(map[string]string)
	}
}

// take returns the counts since the last line and each key's first detail, and starts the next,
// when any are counted and logInterval has passed since the last line, or final is set; otherwise
// nothing.
func (t *tally) take(final bool) (map[string]int64, map[string]string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.counts) == 0 || (!final && time.Since(t.logged) < logInterval) {
		return nil, nil, false
	}
	counts, first := t.counts, t.first
	t.counts, t.first, t.logged = make(map[string]int64), make(map[string]string), time.Now()
	return counts, first, true
}

// reportWhileRunning is the service's one log writer. No request's goroutine, the delivery
// goroutine and the heartbeat among them, writes to the log: a log line is a write, and a write
// can block, as the broker's stdout does when the reader of the pod's log falls behind. Written
// where it is made, a refusal's line held the refusal's answer, its place in the refusing bound
// and, for a refusal made in a decision slot, that slot, so a stalled log stopped the replica
// answering; written on the heartbeat, a line would stop the beat, and a stalled log would read
// as a stalled replica. Each of them counts what it would log (drops, refusals, failures), and
// every livenessTick this goroutine writes, at most once a logInterval for each kind, the counts
// and each key's first detail. A log that stalls holds this goroutine alone, and what waits for it
// is counts; the drop metric does not wait for it, since the counter moves it (countWhileRunning).
// Close stops it once every request has been answered, and waits for its last report for
// drainBound at most; that report writes whatever is counted, inside the interval or not, so a
// replica that stops logs what it counted in its last minute.
func (s *Service) reportWhileRunning() {
	defer close(s.reported)
	ticker := time.NewTicker(s.settings.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.report(false)
		case <-s.stopReport:
			s.report(true)
			return
		}
	}
}

// report writes what each tally holds, when its interval allows or final is set.
func (s *Service) report(final bool) {
	if counts, first, ok := s.answer.dropped.take(final); ok {
		s.answer.logAt(slog.LevelWarn, "natscallout: dropped authorization requests unanswered; the hub refuses each client at its authorization timeout",
			append(countFields(counts, first), "waiting_bound", s.settings.maxWaiting, "refusing_bound", s.settings.maxRefusing)...)
	}
	if counts, first, ok := s.answer.refusals.take(final); ok {
		level := slog.LevelWarn
		message := "natscallout: refused NATS connections"
		if counts[labelUnsealed] > 0 {
			// A request in the clear means the hub's callout lost its curve key and every token
			// and proof is crossing the wire unencrypted: said loudly.
			level = slog.LevelError
			message = "natscallout: refused NATS connections, among them requests that arrived in the clear: the hub's auth_callout names no xkey, so every token and proof crosses the wire unencrypted"
		}
		s.answer.logAt(level, message, countFields(counts, first)...)
	}
	if counts, first, ok := s.answer.failures.take(final); ok {
		s.answer.logAt(slog.LevelWarn, "natscallout: answers that could not be sent", countFields(counts, first)...)
	}
}

// countFields is a tally's counts as log fields: their total, then, in the keys' order, each key's
// count and its first event's detail (key_first) when one was kept.
func countFields(counts map[string]int64, first map[string]string) []any {
	keys := make([]string, 0, len(counts))
	total := int64(0)
	for key, n := range counts {
		keys = append(keys, key)
		total += n
	}
	slices.Sort(keys)
	fields := []any{"count", total}
	for _, key := range keys {
		fields = append(fields, key, counts[key])
		if detail, kept := first[key]; kept {
			fields = append(fields, key+"_first", detail)
		}
	}
	return fields
}

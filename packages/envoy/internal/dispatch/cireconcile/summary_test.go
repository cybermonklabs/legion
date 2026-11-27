package cireconcile

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/testwait"
)

const summaryDebounce = 100 * time.Millisecond

type scheduledRecord struct {
	state cistore.State
	kind  string
	pass  uint64
}

// summaryWitness receives actual scheduling decisions from the real summary loop. Publish still
// goes through the real bus. The buffered wake is a change notification, not a clock tick.
type summaryWitness struct {
	cistore.Publisher
	mu        sync.Mutex
	records   map[string]scheduledRecord
	wake      chan struct{}
	requests  chan chan struct{}
	completed uint64
}

func (p *summaryWitness) SummaryScheduled(pass uint64, key string, state cistore.State, kind string, _ int64) {
	p.mu.Lock()
	p.records[key] = scheduledRecord{state: state, kind: kind, pass: pass}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *summaryWitness) SummaryPassEnded(pass uint64) {
	p.mu.Lock()
	p.completed = pass
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *summaryWitness) SummaryScheduleRequests() chan chan struct{} { return p.requests }

func (p *summaryWitness) record(key string) (scheduledRecord, bool, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.records[key]
	return r, ok, p.completed
}

// noSettlementAt waits for the loop to classify the durable record, not for an interval in which
// nothing arrived. A debounce-only wait is not a verdict: the loop must inspect it again at due.
// A fresh pass, not an earlier empty scan, witnesses an absent durable key. Transport errors
// are failures, never absence.
func (h *harness) noSettlementAt(sub *nats.Subscription, why, owner, repo, number, sha string) {
	h.t.Helper()
	kv, err := h.bus.JS().KeyValue(bus.CIStateBucket)
	if err != nil {
		h.t.Fatalf("%s: open the CI bucket: %v", why, err)
	}
	key := cistore.Key(owner, repo, number, sha)
	ctx := testwait.Context(h.t)
	done := make(chan struct{})
	select {
	case h.summary.requests <- done:
	case <-ctx.Done():
		h.t.Fatalf("%s: the summary loop never accepted the fresh-pass request", why)
	}
	select {
	case <-done:
	case <-ctx.Done():
		h.t.Fatalf("%s: the summary loop never finished the requested fresh pass", why)
	}
	for {
		note, reported, completed := h.summary.record(key)
		entry, err := kv.Get(key)
		switch {
		case errors.Is(err, nats.ErrKeyNotFound):
			if !reported {
				h.requireNoQueuedSettlement(sub, why)
				return
			}
		case err != nil:
			h.t.Fatalf("%s: read the CI record: %v", why, err)
		case reported:
			var durable cistore.State
			if err := json.Unmarshal(entry.Value(), &durable); err != nil {
				h.t.Fatalf("%s: decode the CI record: %v", why, err)
			}
			want, err := json.Marshal(durable)
			if err != nil {
				h.t.Fatal(err)
			}
			got, err := json.Marshal(note.state)
			if err != nil {
				h.t.Fatal(err)
			}
			if bytes.Equal(got, want) && completed >= note.pass {
				switch note.kind {
				case "attempt":
					h.t.Fatalf("%s: the summary loop scheduled a settlement of the durable record: %+v", why, durable)
				case "debounce", "claim":
					// A debounce or non-verify claim window is only a transient wait.
				case "verify", "idle", "not-ready", "retry", "held", "stuck":
					h.requireNoQueuedSettlement(sub, why)
					return
				default:
					h.t.Fatalf("%s: unknown scheduler verdict %q", why, note.kind)
				}
			}
		}
		select {
		case <-h.summary.wake:
		case <-ctx.Done():
			h.t.Fatalf("%s: the summary loop never completed a decisive pass for %s", why, key)
		}
	}
}

func (h *harness) requireNoQueuedSettlement(sub *nats.Subscription, why string) {
	h.t.Helper()
	if err := h.bus.Conn.Flush(); err != nil {
		h.t.Fatalf("%s: flush the settlement subscription: %v", why, err)
	}
	pending, _, err := sub.Pending()
	if err != nil {
		h.t.Fatalf("%s: read pending settlements: %v", why, err)
	}
	if pending != 0 {
		h.t.Fatalf("%s: %d settlements were published, want none", why, pending)
	}
}

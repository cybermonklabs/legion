package cireconcile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/testwait"
)

const summaryDebounce = 100 * time.Millisecond

var errUnexpectedSettlement = errors.New("unexpected settlement")

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

// noSettlementAt covers every durable SHA on the pull request's subject, as the subscription
// does. It waits for completed, matching decisions and rejects transient debounce/claim waits.
// A fresh pass and an acknowledged key listing also witness an empty subject. Transport
// errors are failures, never absence.
func (h *harness) noSettlementAt(sub *nats.Subscription, why, owner, repo, number string) {
	h.t.Helper()
	if err := h.waitForNoSettlement(sub, why, owner, repo, number); err != nil {
		h.t.Fatal(err)
	}
}

// waitForNoSettlement returns its observation so a regression can prove the assertion rejects
// an extra SHA without making an intentionally failing test part of the normal suite.
func (h *harness) waitForNoSettlement(sub *nats.Subscription, why, owner, repo, number string) error {
	h.t.Helper()
	kv, err := h.bus.JS().KeyValue(bus.CIStateBucket)
	if err != nil {
		return fmt.Errorf("%s: open the CI bucket: %w", why, err)
	}
	prefix := cistore.Key(owner, repo, number, "")
	ctx := testwait.Context(h.t)
	done := make(chan struct{})
	select {
	case h.summary.requests <- done:
	case <-ctx.Done():
		return fmt.Errorf("%s: the summary loop never accepted the fresh-pass request: %w", why, ctx.Err())
	}
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("%s: the summary loop never finished the requested fresh pass: %w", why, ctx.Err())
	}
	for {
		keys, err := kv.Keys(nats.Context(ctx))
		if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
			return fmt.Errorf("%s: list the CI records: %w", why, err)
		}
		decisive := true
		for _, key := range keys {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			note, reported, completed := h.summary.record(key)
			entry, err := kv.Get(key)
			if errors.Is(err, nats.ErrKeyNotFound) {
				decisive = false // The listing changed; obtain a new one rather than infer absence.
				continue
			}
			if err != nil {
				return fmt.Errorf("%s: read CI record %s: %w", why, key, err)
			}
			if !reported {
				decisive = false
				continue
			}
			var durable cistore.State
			if err := json.Unmarshal(entry.Value(), &durable); err != nil {
				return fmt.Errorf("%s: decode CI record %s: %w", why, key, err)
			}
			want, err := json.Marshal(durable)
			if err != nil {
				return err
			}
			got, err := json.Marshal(note.state)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, want) || completed < note.pass {
				decisive = false
				continue
			}
			switch note.kind {
			case "attempt":
				return fmt.Errorf("%s: the summary loop scheduled a settlement of the durable record: %+v: %w", why, durable, errUnexpectedSettlement)
			case "debounce", "claim":
				decisive = false
			case "verify", "idle", "not-ready", "retry", "held", "stuck":
				// A matching decision from a completed pass cannot publish without new evidence.
			default:
				return fmt.Errorf("%s: unknown scheduler verdict %q", why, note.kind)
			}
		}
		if decisive {
			return h.queuedSettlementError(sub, why)
		}
		select {
		case <-h.summary.wake:
		case <-ctx.Done():
			return fmt.Errorf("%s: the summary loop never completed decisive passes for %s; last keys: %v: %w", why, prefix, keys, ctx.Err())
		}
	}
}

func (h *harness) queuedSettlementError(sub *nats.Subscription, why string) error {
	h.t.Helper()
	if err := h.bus.Conn.Flush(); err != nil {
		return fmt.Errorf("%s: flush the settlement subscription: %w", why, err)
	}
	pending, _, err := sub.Pending()
	if err != nil {
		return fmt.Errorf("%s: read pending settlements: %w", why, err)
	}
	if pending != 0 {
		return fmt.Errorf("%s: %d settlements were published, want none: %w", why, pending, errUnexpectedSettlement)
	}
	return nil
}

func TestTheAbsenceWitnessRejectsAnotherSHAOnTheSamePullRequestSubject(t *testing.T) {
	t.Parallel()
	h := newHarness(t, reviewerAppID)
	settlements := h.subscribe()
	first := checkRun(1, "first-ci", "completed", "success", otherApp, "2026-11-05T03:01:00Z")
	h.post("check_run", webhookFor("check_run", first, 7), "subject-first")
	if got := h.settlement(settlements, "the original SHA"); got.SHA != headSHA {
		t.Fatalf("original settlement SHA=%s, want %s", got.SHA, headSHA)
	}
	h.settledRecord()

	other := head(8)
	second := runAt(other, 2, "second-ci", "completed", "success", "2026-11-05T03:02:00Z")
	h.post("check_run", webhookFor("check_run", second, 7), "subject-second")
	if state, _ := h.recordAt(gitHubRepository, 7, other.sha); state == nil {
		t.Fatal("the webhook did not create the second durable SHA on the same pull request")
	}
	err := h.waitForNoSettlement(settlements, "the whole PR subject", gitHubOwner, gitHubName, pullNumber)
	if !errors.Is(err, errUnexpectedSettlement) {
		t.Fatalf("the subject-wide assertion returned %v, want the second SHA's unexpected settlement", err)
	}
	if got := h.settlement(settlements, "the second SHA detected by the witness"); got.SHA != other.sha {
		t.Fatalf("detected settlement SHA=%s, want %s", got.SHA, other.sha)
	}
}

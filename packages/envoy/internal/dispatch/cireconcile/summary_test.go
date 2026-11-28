package cireconcile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
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
	mu        sync.Mutex
	records   map[string]scheduledRecord
	wake      chan struct{}
	completed uint64
}

var _ cistore.SummaryObserver = (*summaryWitness)(nil)

func (p *summaryWitness) SummaryScheduled(key string, state cistore.State, kind string) {
	p.mu.Lock()
	p.records[key] = scheduledRecord{state: state, kind: kind, pass: p.completed + 1}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *summaryWitness) SummaryPassEnded() {
	p.mu.Lock()
	p.completed++
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *summaryWitness) record(key string) (scheduledRecord, bool, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.records[key]
	return r, ok, p.completed
}

// noSettlementAt covers every durable SHA on the pull request's subject, as the subscription
// does. It waits for completed, matching decisions and rejects transient debounce/claim waits.
// An acknowledged key listing witnesses an empty subject. Transport errors are failures.
func (h *harness) noSettlementAt(sub *nats.Subscription, why, owner, repo, number string) {
	h.t.Helper()
	if _, err := h.waitForNoSettlement(sub, why, owner, repo, number); err != nil {
		h.t.Fatal(err)
	}
}

// subjectRecords is every CI record the bucket holds under one pull request's subject, with its
// value, read through bus.KeyValue.HeldEntries: a key another client writes again while the list
// is read is still listed there, where nats.go's Keys can leave it out (INFRA-6356), and a key left
// out is a key the witness never judges. It is the witness's one source of keys.
func (h *harness) subjectRecords(owner, repo, number string) ([]nats.KeyValueEntry, error) {
	h.t.Helper()
	ctx := testwait.Context(h.t)
	kv, err := bus.OpenKeyValue(ctx, h.bus.Conn, h.bus.JS(), bus.CIStateBucket)
	if err != nil {
		return nil, fmt.Errorf("open the CI bucket: %w", err)
	}
	keys := cistore.Key(owner, repo, number, "") + ">"
	_, records, err := kv.HeldEntries(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("list the CI records %s: %w", keys, err)
	}
	return records, nil
}

// waitForNoSettlement returns the keys whose matching, completed decisions it judged. The
// regression test checks this evidence even if an unwanted settlement is already queued.
func (h *harness) waitForNoSettlement(sub *nats.Subscription, why, owner, repo, number string) ([]string, error) {
	h.t.Helper()
	prefix := cistore.Key(owner, repo, number, "")
	ctx := testwait.Context(h.t)
	for {
		records, err := h.subjectRecords(owner, repo, number)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", why, err)
		}
		decisive := true
		var keys, judged []string
		for _, entry := range records {
			key := entry.Key()
			keys = append(keys, key)
			note, reported, completed := h.summary.record(key)
			if !reported {
				decisive = false
				continue
			}
			var durable cistore.State
			if err := json.Unmarshal(entry.Value(), &durable); err != nil {
				return judged, fmt.Errorf("%s: decode CI record %s: %w", why, key, err)
			}
			want, err := json.Marshal(durable)
			if err != nil {
				return judged, err
			}
			got, err := json.Marshal(note.state)
			if err != nil {
				return judged, err
			}
			if !bytes.Equal(got, want) || completed < note.pass {
				decisive = false
				continue
			}
			judged = append(judged, key)
			switch note.kind {
			case "attempt":
				return judged, fmt.Errorf("%s: the summary loop scheduled a settlement of the durable record: %+v: %w", why, durable, errUnexpectedSettlement)
			case "debounce", "claim":
				decisive = false
			case "verify", "idle", "not-ready", "retry", "held", "stuck":
				// A matching decision from a completed pass cannot publish without new evidence.
			default:
				return judged, fmt.Errorf("%s: unknown scheduler verdict %q", why, note.kind)
			}
		}
		if decisive {
			return judged, h.queuedSettlementError(sub, why)
		}
		select {
		case <-h.summary.wake:
		case <-ctx.Done():
			return judged, fmt.Errorf("%s: the summary loop never completed decisive passes for %s; last keys: %v: %w", why, prefix, keys, ctx.Err())
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
	// Exercise the old helper's blind case even on a fast runner: the message is already
	// queued, so only the judged-key evidence distinguishes a one-commit helper.
	testwait.Until(t, "the second SHA's queued settlement before the absence assertion", func() (int, bool) {
		pending, _, err := settlements.Pending()
		if err != nil {
			t.Fatal(err)
		}
		return pending, pending > 0
	})
	judged, err := h.waitForNoSettlement(settlements, "the whole PR subject", gitHubOwner, gitHubName, pullNumber)
	if key := cistore.Key(gitHubOwner, gitHubName, pullNumber, other.sha); !slices.Contains(judged, key) {
		t.Fatalf("the subject-wide assertion judged keys %v and returned %v, want it to include the second SHA %s", judged, err, key)
	}
	if !errors.Is(err, errUnexpectedSettlement) {
		t.Fatalf("the subject-wide assertion returned %v, want the second SHA's unexpected settlement", err)
	}
	if got := h.settlement(settlements, "the second SHA detected by the witness"); got.SHA != other.sha {
		t.Fatalf("detected settlement SHA=%s, want %s", got.SHA, other.sha)
	}
}

// whileWrittenAgain runs reads while another client writes key of the CI bucket again with value
// in a loop, on a goroutine of its own: the writes that let nats.go's Keys and the jetstream
// watcher's first delivery leave a key out of a listing (INFRA-6356). The writes stop once reads
// returns or ends the test, and the goroutine's end is waited for. It fails the test where a write
// failed, and where no write landed while reads ran, since those reads then raced nothing.
func (h *harness) whileWrittenAgain(key string, value []byte, reads func()) {
	h.t.Helper()
	kv, err := h.bus.JS().KeyValue(bus.CIStateBucket)
	if err != nil {
		h.t.Fatalf("open the CI bucket: %v", err)
	}
	stop := make(chan struct{})
	ended := make(chan error, 1)
	var landed atomic.Uint64
	go func() {
		for {
			select {
			case <-stop:
				ended <- nil
				return
			default:
			}
			if _, err := kv.Put(key, value); err != nil {
				ended <- fmt.Errorf("write %s again after %d writes: %w", key, landed.Load(), err)
				return
			}
			landed.Add(1)
		}
	}()
	defer func() {
		close(stop)
		if err := testwait.Receive(h.t, "the end of the writes of "+key, ended); err != nil {
			h.t.Error(err)
		}
	}()
	before := landed.Load()
	reads()
	if landed.Load() == before {
		h.t.Fatalf("no write of %s landed while the reads ran", key)
	}
}

// The witness lists every record under the pull request's subject while another client writes
// one of them again: nats.go's Keys, which it listed through before, can leave such a key out
// (INFRA-6356), and the witness judges no key it does not list. The record is written again once
// both have settled, with the bytes the bucket holds for it, so the summary loop finds nothing
// changed and writes nothing of its own beside the test's writes.
func TestTheWitnessListsAKeyWrittenAgainWhileItLists(t *testing.T) {
	t.Parallel()
	h := newHarness(t, reviewerAppID)
	first := checkRun(1, "first-ci", "completed", "success", otherApp, "2026-11-05T03:01:00Z")
	h.post("check_run", webhookFor("check_run", first, 7), "subject-first")
	other := head(8)
	second := runAt(other, 2, "second-ci", "completed", "success", "2026-11-05T03:02:00Z")
	h.post("check_run", webhookFor("check_run", second, 7), "subject-second")
	for _, sha := range []string{headSHA, other.sha} {
		testwait.Until(t, "the settled CI record of "+sha, func() (*cistore.State, bool) {
			state, _ := h.recordAt(gitHubRepository, 7, sha)
			return state, state != nil && state.SettledEmitted && state.Claim == nil
		})
	}
	kv, err := h.bus.JS().KeyValue(bus.CIStateBucket)
	if err != nil {
		t.Fatalf("open the CI bucket: %v", err)
	}
	rewritten := cistore.Key(gitHubOwner, gitHubName, pullNumber, other.sha)
	stored, err := kv.Get(rewritten)
	if err != nil {
		t.Fatalf("read the record %s: %v", rewritten, err)
	}
	want := []string{cistore.Key(gitHubOwner, gitHubName, pullNumber, headSHA), rewritten}

	h.whileWrittenAgain(rewritten, stored.Value(), func() {
		for call := range 100 {
			records, err := h.subjectRecords(gitHubOwner, gitHubName, pullNumber)
			if err != nil {
				t.Fatalf("call %d of the witness's listing: %v", call, err)
			}
			listed := make([]string, 0, len(records))
			for _, entry := range records {
				listed = append(listed, entry.Key())
			}
			for _, key := range want {
				if !slices.Contains(listed, key) {
					t.Fatalf("call %d of the witness's listing left out %s, a record the bucket holds: it listed %v", call, key, listed)
				}
			}
		}
	})
}

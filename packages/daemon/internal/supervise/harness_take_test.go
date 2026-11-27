package supervise

// The worker's side of the take in the harness (harness_test.go): the task bucket's double the
// machine writes values into, the claim's plugin as the harness plays it, and the helpers a test
// drives its seen reports, takes and turn-outcome reports through.

import (
	"context"
	"slices"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// memTaskValues is the task bucket as a double (TaskValues): one value per claim, a revision the
// whole bucket counts, as a NATS key-value bucket's does, every write and delete in order, and a
// way to fail a call. internal/supervise/supervisetest holds the same double for other packages'
// tests; this package's own tests cannot import it, since it imports this package.
type memTaskValues struct {
	mu       sync.Mutex
	epoch    string
	revision uint64
	values   map[claim.Token]TaskValue
	writes   []TaskValue
	deletes  []claim.Token
	failures map[string]error
}

// testEpoch is the bucket epoch memTaskValues writes under.
const testEpoch = "2026-09-22T12:00:00Z"

func newMemTaskValues() *memTaskValues {
	return &memTaskValues{epoch: testEpoch, values: map[claim.Token]TaskValue{}, failures: map[string]error{}}
}

func (v *memTaskValues) Write(_ context.Context, token claim.Token, number uint64) (string, uint64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failures["Write"]; err != nil {
		return "", 0, err
	}
	v.revision++
	value := TaskValue{Number: number, Epoch: v.epoch, Revision: v.revision}
	v.values[token] = value
	v.writes = append(v.writes, value)
	return value.Epoch, value.Revision, nil
}

func (v *memTaskValues) Delete(_ context.Context, token claim.Token) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failures["Delete"]; err != nil {
		return err
	}
	v.revision++
	delete(v.values, token)
	v.deletes = append(v.deletes, token)
	return nil
}

func (v *memTaskValues) Read(_ context.Context, token claim.Token) (TaskValue, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failures["Read"]; err != nil {
		return TaskValue{}, false, err
	}
	value, ok := v.values[token]
	return value, ok, nil
}

func (v *memTaskValues) fail(method string, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.failures[method] = err
}

// value is token's value as the bucket holds it now, and false for none.
func (v *memTaskValues) value(token claim.Token) (TaskValue, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.values[token]
	return value, ok
}

// written is every value written, in order.
func (v *memTaskValues) written() []TaskValue {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.writes)
}

// deleted is every delete made, in order.
func (v *memTaskValues) deleted() []claim.Token {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.deletes)
}

// harnessWorker is the claim's plugin as the harness plays it, the way pi-shared's task-take
// takes: it reports every value it is handed seen, and asks to take the newest once per
// revision, while its host is idle — the claim ready or idle, no turn of its own running. A
// process that registers anew has asked nothing (generation, at). It starts no turn of its own:
// a test posts the turn's start. manual stops it, for a test that drives every take itself
// (manualTakes). relayDown is the listener that relays its values stopped: the worker is handed
// no value, so it reports none seen and asks nothing, until the test sets it back.
type harnessWorker struct {
	manual     bool
	relayDown  bool
	generation uint64
	seen       map[TaskValue]bool
	asked      map[TaskValue]bool
	// taken is every take of the worker's the daemon accepted, in order, and answers every answer
	// the daemon gave, the refused and the dropped among them.
	taken   []taken
	answers []TakeAnswer
	// sequence is the worker's own count of the turns it has reported ended (RequestTurnOutcome).
	sequence uint64
}

// at is the worker as the process of generation knows it: one that registered anew has seen and
// asked at nothing, since its memory went with the process it replaced.
func (w *harnessWorker) at(generation uint64) {
	if w.generation != generation || w.seen == nil {
		w.generation, w.seen, w.asked = generation, map[TaskValue]bool{}, map[TaskValue]bool{}
	}
}

// taken is one take the daemon accepted: the task's delivery and number as the claim held it, and
// the text the answer carried, which is what the worker runs.
type taken struct {
	DeliveryID string
	Number     uint64
	Message    string
}

// reportTurn is the worker's turn-outcome report of the turn of task number, naming the provider
// error it ended in, nil for none (RequestTurnOutcome). The worker then takes a value written
// meanwhile, as it does after every event (workerAsks).
func (h *harness) reportTurn(number uint64, providerError *ProviderError) error {
	h.worker.sequence++
	err := h.m.Handle(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: h.generation(),
		Sequence: h.worker.sequence, Task: number, ProviderError: providerError})
	h.m.Wait()
	h.workerAsks()
	return err
}

// mustReport is reportTurn for the pending task, failing the test on an error.
func (h *harness) mustReport(providerError *ProviderError) {
	h.t.Helper()
	number := h.pending().Number
	if err := h.reportTurn(number, providerError); err != nil {
		h.t.Fatalf("report the turn of task %d: %v", number, err)
	}
}

// workerAsks is the worker's own reading of its value, as pi-shared's task-take reads it: seen
// once per revision while its agent is registered, and taken once per revision while its host is
// idle. A process that registered anew asks afresh, since its memory went with the process it
// replaced. Each revision is marked seen and asked before the request goes, as the plugin marks
// it, so a seen report or a take the daemon answered with nothing but a failure — a store a test
// is failing — is not sent again: every failure the daemon answers carries a code, and the plugin
// sends the same request again only where no daemon answered at all (departure 7 of
// dispatch://INFRA-2811), which a harness calling the machine itself never meets. The worker asks
// again at a newer revision, which is the daemon's to write (undecided, offerPending).
func (h *harness) workerAsks() {
	w := h.worker
	if w.manual || w.relayDown {
		return
	}
	c := h.m.Claim()
	w.at(c.Generation)
	value, ok := h.values.value(h.token)
	if !ok || !HoldsCapability(c.State) {
		return
	}
	if !w.seen[value] {
		w.seen[value] = true
		_ = h.m.Handle(h.ctx, RequestTaskSeen{Claim: h.token, Generation: c.Generation, Session: c.Session,
			Number: value.Number, Revision: value.revision()})
	}
	if (c.State != StateReady && c.State != StateIdle) || w.asked[value] {
		return
	}
	w.asked[value] = true
	_, _ = h.takeValue(value)
}

// takeValue is a take of value's number at its revision, recorded as the worker's. An answer the
// daemon gave beside a failure of the rest of its decision is the worker's all the same
// (Machine.Take), so it is recorded whatever the error.
func (h *harness) takeValue(value TaskValue) (TakeAnswer, error) {
	c := h.m.Claim()
	answer, err := h.m.Take(h.ctx, RequestTake{Claim: h.token, Generation: c.Generation, Session: c.Session,
		Number: value.Number, Revision: value.revision()})
	h.m.Wait()
	if answer.Outcome == "" {
		return answer, err
	}
	h.worker.answers = append(h.worker.answers, answer)
	if answer.Outcome == TakeOutcomeAccepted {
		h.worker.taken = append(h.worker.taken, taken{DeliveryID: pendingID(c.Pending), Number: value.Number, Message: answer.Text})
	}
	return answer, err
}

// manualTakes stops the harness's worker from taking on its own, for a test that drives every
// seen report and take itself (seen, take).
func (h *harness) manualTakes() { h.worker.manual = true }

// seen is the worker reporting the claim's current value seen.
func (h *harness) seen() TaskValue {
	h.t.Helper()
	value, ok := h.values.value(h.token)
	if !ok {
		h.t.Fatal("the bucket holds no value for the claim")
	}
	if err := h.m.Handle(h.ctx, RequestTaskSeen{Claim: h.token, Generation: h.generation(), Session: h.claim().Session,
		Number: value.Number, Revision: value.revision()}); err != nil {
		h.t.Fatalf("report the value seen: %v", err)
	}
	return value
}

// take is the worker taking the claim's current value: seen, then taken, and the answer.
func (h *harness) take() TakeAnswer {
	h.t.Helper()
	value := h.seen()
	answer, err := h.takeValue(value)
	if answer.Outcome == "" {
		h.t.Fatalf("take task %d: %v", value.Number, err)
	}
	return answer
}

// mustTake is take, failing the test unless the take is accepted; it answers the text.
func (h *harness) mustTake() string {
	h.t.Helper()
	answer := h.take()
	if answer.Outcome != TakeOutcomeAccepted {
		h.t.Fatalf("take = %+v, want accepted", answer)
	}
	return answer.Text
}

// wantRefused is take, failing the test unless the take is refused by hold.
func (h *harness) wantRefused(hold string) {
	h.t.Helper()
	if answer := h.take(); answer.Outcome != TakeOutcomeRefused || answer.Hold != hold {
		h.t.Fatalf("take = %+v, want refused by %q", answer, hold)
	}
}

// seedValue writes c's pending task's value into the bucket, as the daemon that queued it did, and
// records the write on c as that daemon's claim write did: a claim restored holding a task finds
// the value its record names (internal/daemon's task bucket repairs one at boot).
func (h *harness) seedValue(c *Claim) {
	h.t.Helper()
	epoch, revision, err := h.values.Write(h.ctx, c.Token, c.Pending.Number)
	if err != nil {
		h.t.Fatalf("seed the restored value: %v", err)
	}
	c.TaskWrittenRevision = TaskRevision{Epoch: epoch, Revision: revision}
}

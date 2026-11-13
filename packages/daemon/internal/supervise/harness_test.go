package supervise

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

const (
	testToken   claim.Token = "legion-LEGION-209-implementer"
	rootToken   claim.Token = "legion-LEGION-208-architect"
	testBoot                = 120 * time.Second
	testRPC                 = 5 * time.Second
	testProbe               = 30 * time.Second
	testStop                = 10 * time.Second
	intervals               = 3
	deadline                = testBoot * intervals
	session                 = "ses_implementer"
	sessionFile             = "/state/sessions/implementer.jsonl"
)

var epoch = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// fakeClock is time that moves only when the test says so. Advance fires every timer that falls
// due, in deadline order, synchronously and outside the clock's lock — a timer's callback is a
// machine's Handle, which may arm the next timer.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	armed  int
	// slept is every wait a machine asked to Sleep, in order.
	slept []time.Duration
}

// Sleep records d and returns at once, with ctx's error when it has ended: a machine's wait costs
// the test nothing, and the test reads what it waited.
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.slept = append(c.slept, d)
	c.mu.Unlock()
	return ctx.Err()
}

type fakeTimer struct {
	clock   *fakeClock
	at      time.Time
	order   int
	f       func()
	stopped bool
	fired   bool
}

func newFakeClock() *fakeClock { return &fakeClock{now: epoch} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Cancel {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed++
	timer := &fakeTimer{clock: c, at: c.now.Add(d), order: c.armed, f: f}
	c.timers = append(c.timers, timer)
	return timer
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

// Advance moves time forward by d, firing what falls due on the way.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	until := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, timer := range c.timers {
			if timer.stopped || timer.fired || timer.at.After(until) {
				continue
			}
			if next == nil || timer.at.Before(next.at) || (timer.at.Equal(next.at) && timer.order < next.order) {
				next = timer
			}
		}
		if next == nil {
			c.now = until
			c.mu.Unlock()
			return
		}
		next.fired = true
		c.now = next.at
		c.mu.Unlock()
		next.f()
	}
}

// dropAll is every armed timer dying with the process that armed it.
func (c *fakeClock) dropAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, timer := range c.timers {
		timer.stopped = true
	}
}

// Live is how many timers are armed and neither fired nor stopped.
func (c *fakeClock) Live() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	live := 0
	for _, timer := range c.timers {
		if !timer.stopped && !timer.fired {
			live++
		}
	}
	return live
}

// memStore is the store as a double: what was written, in order, and a way to fail a write.
type memStore struct {
	mu         sync.Mutex
	claims     map[claim.Token]Claim
	deliveries map[claim.Token]Delivery
	puts       []Claim
	// writes is each write and retirement of a pending delivery, in order (deliveryLog).
	writes   []string
	failures map[string]error
	// onPut, when set, runs on every PutClaim before it is recorded.
	onPut func(Claim)
}

func newMemStore() *memStore {
	return &memStore{
		claims:     map[claim.Token]Claim{},
		deliveries: map[claim.Token]Delivery{},
		failures:   map[string]error{},
	}
}

func (s *memStore) PutClaim(_ context.Context, c Claim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failures["PutClaim"]; err != nil {
		return err
	}
	if s.onPut != nil {
		s.onPut(c)
	}
	c = copyClaim(c)
	c.Pending = nil
	// The step columns are PutClaimStep's alone, as they are in the store (store's putClaim): a
	// machine's write carries whatever its in-memory claim last read, and a step recorded since
	// that read must survive it.
	c.Step = s.claims[c.Token].Step
	s.claims[c.Token] = c
	s.puts = append(s.puts, c)
	return nil
}

func (s *memStore) AdmitClaim(ctx context.Context, c Claim) (Claim, error) {
	return c, s.PutClaim(ctx, c)
}

func (s *memStore) CheckLaunch(context.Context, Claim) error { return nil }

func (s *memStore) PutDelivery(_ context.Context, token claim.Token, d Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failures["PutDelivery"]; err != nil {
		return err
	}
	if _, ok := s.claims[token]; !ok {
		return fmt.Errorf("put delivery %s: no claim %s", d.ID, token)
	}
	s.deliveries[token] = d
	s.writes = append(s.writes, "put "+d.ID)
	return nil
}

// PutClaimAndDelivery is the store's one transaction: a failure injected on either write leaves
// both unwritten, which is what the machine relies on.
func (s *memStore) PutClaimAndDelivery(ctx context.Context, c Claim, d Delivery) error {
	s.mu.Lock()
	for _, method := range []string{"PutClaim", "PutDelivery"} {
		if err := s.failures[method]; err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.mu.Unlock()
	if err := s.PutClaim(ctx, c); err != nil {
		return err
	}
	return s.PutDelivery(ctx, c.Token, d)
}

func (s *memStore) RetireDelivery(ctx context.Context, c Claim, deliveryID string) error {
	s.mu.Lock()
	if err := s.failures["RetireDelivery"]; err != nil {
		s.mu.Unlock()
		return err
	}
	held, ok := s.deliveries[c.Token]
	switch {
	// The real store tells the two refusals of a delete that removed no row apart, because its
	// table is keyed on the claim: nothing stored under the claim is the retirement it has
	// already made, and a delivery stored under another id is a write that rotated the id and
	// committed after its answer was lost.
	case !ok:
		s.mu.Unlock()
		return fmt.Errorf("retire delivery %s on %s: %w", deliveryID, c.Token, ErrNoSuchDelivery)
	case held.ID != deliveryID:
		s.mu.Unlock()
		return fmt.Errorf("retire delivery %s on %s: the row holds delivery %s: %w",
			deliveryID, c.Token, held.ID, ErrOtherDelivery)
	}
	s.mu.Unlock()
	// The store writes the claim and the delete together; a failure injected on either leaves
	// both unwritten, as one transaction does.
	if err := s.PutClaim(ctx, c); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deliveries, c.Token)
	s.writes = append(s.writes, "retire "+deliveryID)
	return nil
}

// PutClaimStep is the store's narrow write of the step columns, with the rules the real one
// holds to (store.Store.PutClaimStep): a zero At records the call without moving the claim's
// last-step time, a zero TurnStartedAt leaves the stored turn start alone, an empty CallID
// carries no call and leaves the six call columns as they stand, and a step older than the one
// recorded is dropped whole. It writes nothing else of the claim, which is the property the
// machine relies on.
func (s *memStore) PutClaimStep(_ context.Context, token claim.Token, step StepRecord) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failures["PutClaimStep"]; err != nil {
		return false, err
	}
	c, known := s.claims[token]
	if !known {
		return false, nil
	}
	held := c.Step
	if !step.At.IsZero() && !held.At.IsZero() && step.At.Before(held.At) {
		return true, nil
	}
	if !step.At.IsZero() {
		held.At = step.At
	}
	if !step.TurnStartedAt.IsZero() {
		held.TurnStartedAt = step.TurnStartedAt
	}
	if step.CallID != "" {
		held.Tool, held.CallID = step.Tool, step.CallID
		held.ArgsBytes, held.ArgsHash = step.ArgsBytes, step.ArgsHash
		held.StartedAt, held.EndedAt = step.StartedAt, step.EndedAt
	}
	c.Step = held
	s.claims[token] = c
	return true, nil
}

// step is the record the store holds for token, as a claim read carries it back.
func (s *memStore) step(token claim.Token) StepRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims[token].Step
}

func (s *memStore) fail(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[method] = err
}

// load is the claim as Claims would read it back after a restart: the row and its delivery.
func (s *memStore) load(token claim.Token) Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := copyClaim(s.claims[token])
	if d, ok := s.deliveries[token]; ok {
		c.Pending = &d
	}
	return c
}

func (s *memStore) delivery(token claim.Token) (Delivery, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deliveries[token]
	return d, ok
}

// deliveryLog is every write and retirement of a pending delivery, in order, as "put <id>" and
// "retire <id>". A case reads it to see how a delivery ended: the real store releases what a
// delivery holds of a person's decisions in the delete alone (its releaseKeptDecisions), so a
// row overwritten in place by the next delivery leaves those rows held by an id that is gone,
// and a write under an id after its retirement is that fault.
func (s *memStore) deliveryLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

func (s *memStore) history() []Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Claim(nil), s.puts...)
}

// testSpecs answers every launch with the same environment, or with the error it was told to, and
// records the claim each launch was built for.
type testSpecs struct {
	err    error
	mu     sync.Mutex
	claims []Claim
}

func (s *testSpecs) SpawnSpec(_ context.Context, c Claim) (runtime.SpawnSpec, error) {
	s.mu.Lock()
	s.claims = append(s.claims, copyClaim(c))
	s.mu.Unlock()
	if s.err != nil {
		return runtime.SpawnSpec{}, s.err
	}
	return runtime.SpawnSpec{
		Env: map[string]string{"LEGION_ISSUE": c.Issue},
	}, nil
}

// last is the claim the latest launch was built for.
func (s *testSpecs) last(t *testing.T) Claim {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.claims) == 0 {
		t.Fatal("no launch was built")
	}
	return s.claims[len(s.claims)-1]
}

// logBuffer is a log handler's destination that tests read from.
type logBuffer struct {
	testlog.Buffer
}

// lines is every log line containing all of the fragments.
func (b *logBuffer) lines(fragments ...string) []string {
	var matched []string
	for _, line := range strings.Split(b.String(), "\n") {
		all := line != ""
		for _, fragment := range fragments {
			all = all && strings.Contains(line, fragment)
		}
		if all {
			matched = append(matched, line)
		}
	}
	return matched
}

// loggedRecord is one record the recorder kept: what a reader of the log sees of it, with the
// attributes the logger itself carries folded in beside the ones the call site passed. A test
// reads a record rather than the text buffer where what it holds is the attribute's own value —
// a generation, a delivery, a level — rather than the shape the text handler renders it in.
type loggedRecord struct {
	level   slog.Level
	message string
	attrs   []slog.Attr
}

// attr is the value of one attribute by key, and whether the record carries that key at all. The
// second answer is what a test holds a line writing no key by means of, as a turn that is no
// task's writes no delivery.
func (r loggedRecord) attr(key string) (slog.Value, bool) {
	for _, a := range r.attrs {
		if a.Key == key {
			return a.Value, true
		}
	}
	return slog.Value{}, false
}

// logRecords is every record the machine's logger wrote, in order. It is the handler's
// destination, as logBuffer is the text handler's, and the two run side by side over the same
// logger: a test reads whichever answers its question.
type logRecords struct {
	mu   sync.Mutex
	kept []loggedRecord
}

// of is every record whose message is exactly message, in the order they were written. The match
// is the whole message, never a fragment, because the two turn lines and the two owed-write lines
// are pinned constants an operator and a production check read whole.
func (l *logRecords) of(message string) []loggedRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	var matched []loggedRecord
	for _, r := range l.kept {
		if r.message == message {
			matched = append(matched, r)
		}
	}
	return matched
}

func (l *logRecords) add(r loggedRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.kept = append(l.kept, r)
}

// logRecorder is the handler that keeps those records, wrapped around the text handler the tests
// that read lines use, so one logger feeds both and no test has to choose. It keeps what
// WithAttrs and WithGroup give it rather than dropping them, which is the whole reason it exists:
// the claim a machine's own logger carries (NewMachine) is an attribute of the logger, not of the
// call site, and a handler that forgot it would answer "which claim" with nothing.
type logRecorder struct {
	slog.Handler
	records *logRecords
	attrs   []slog.Attr
	groups  []string
}

func (h logRecorder) Handle(ctx context.Context, r slog.Record) error {
	attrs := slices.Clone(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, qualify(h.groups, a))
		return true
	})
	h.records.add(loggedRecord{level: r.Level, message: r.Message, attrs: attrs})
	return h.Handler.Handle(ctx, r)
}

func (h logRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	kept := slices.Clone(h.attrs)
	for _, a := range attrs {
		kept = append(kept, qualify(h.groups, a))
	}
	h.attrs, h.Handler = kept, h.Handler.WithAttrs(attrs)
	return h
}

func (h logRecorder) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h.groups, h.Handler = append(slices.Clone(h.groups), name), h.Handler.WithGroup(name)
	return h
}

// qualify is an attribute's key as the groups open around it name it, so an attribute written
// inside a group is read by the key the text handler printed it under rather than shadowing a
// bare one of the same name.
func qualify(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 {
		return a
	}
	a.Key = strings.Join(append(slices.Clone(groups), a.Key), ".")
	return a
}

type harness struct {
	t       *testing.T
	ctx     context.Context
	cancel  context.CancelFunc
	token   claim.Token
	rt      *fake.Runtime
	conns   *fake.Conns
	conn    *fake.Conn
	store   *memStore
	clock   *fakeClock
	specs   *testSpecs
	logs    *logBuffer
	records *logRecords
	values  *memTaskValues
	worker  *harnessWorker
	deps    Deps
	m       *Machine
}

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

// testEpoch is the bucket epoch memTaskValues writes under until a test creates it again.
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
// process that registers anew has asked nothing (generation). It starts no turn of its own: a
// test posts the turn's start, as it posted it after a prompt. manual stops it, for a test that
// drives every take itself (manualTakes).
type harnessWorker struct {
	manual     bool
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

// taken is one take the daemon accepted: the task's delivery and number as the claim held it, and
// the text the answer carried, which is what the worker runs.
type taken struct {
	DeliveryID string
	Number     uint64
	Message    string
}

func testLimits() Limits { return Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2} }

func testTimeouts() Timeouts {
	return Timeouts{
		Boot:                  testBoot,
		RegistrationIntervals: intervals,
		RPC:                   testRPC,
		Probe:                 testProbe,
		Stop:                  testStop,
		LaunchBackoff:         testLaunchBackoff,
	}
}

func queuedClaim() Claim {
	return Claim{
		Token:   testToken,
		Project: "legion",
		Tree:    "LEGION-208",
		Issue:   "LEGION-209",
		Role:    claim.RoleImplementer,
		State:   StateQueued,
	}
}

// rootClaim is the tree's root claim: the architect of the issue the tree is named for.
func rootClaim() Claim {
	return Claim{
		Token:   rootToken,
		Project: "legion",
		Tree:    "LEGION-208",
		Issue:   "LEGION-208",
		Role:    claim.RoleArchitect,
		State:   StateQueued,
	}
}

// newHarness is a machine over a queued claim the store already holds, as the spawn route would
// leave it.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessOf(t, queuedClaim())
}

// newHarnessOf is newHarness over the queued claim c; the walks drive c's token.
func newHarnessOf(t *testing.T, c Claim) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.token = c.Token
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h
}

func newBareHarness(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &harness{
		t:       t,
		ctx:     ctx,
		cancel:  cancel,
		token:   testToken,
		rt:      fake.NewRuntime(),
		conns:   fake.NewConns(),
		conn:    fake.NewConn(),
		store:   newMemStore(),
		clock:   newFakeClock(),
		specs:   &testSpecs{},
		logs:    &logBuffer{},
		records: &logRecords{},
		values:  newMemTaskValues(),
		worker:  &harnessWorker{},
	}
	h.rt.Now = h.clock.Now
	h.deps = Deps{
		Runtime: h.rt,
		Conns:   h.conns,
		Store:   h.store,
		Specs:   h.specs,
		Clock:   h.clock,
		// Both handlers over the one logger: the text buffer every test that reads lines uses,
		// and the recorder a test that reads an attribute's own value asks (logRecords.of).
		Log: slog.New(logRecorder{
			Handler: slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}),
			records: h.records,
		}),
		Limits:   testLimits(),
		Timeouts: testTimeouts(),
		// PluginContract matches the real daemon's (api.DaemonAPIVersion), which the register()
		// helper below records for every claim it brings up: a test exercising plugin staleness
		// overrides it (or posts a RequestRegister naming a different one) itself.
		PluginContract: 24,
		// The gate of a daemon that paces nothing, which grants every refresh: every Deps carries
		// one and none is nil, and a test that wants the lane bounded builds its own
		// (NewRefreshGate, gatedHarness).
		RefreshGate: NewRefreshGate(0),
		TaskValues:  h.values,
	}
	return h
}

// shutdown cancels the machine's own context, as a stopping daemon does: every internally-armed
// timer's callback (arm, machine.go) calls Handle with this same context (NewMachine stores it as
// m.ctx), so a timer that fires after shutdown sees it already canceled, exactly as the real
// daemon's stop does.
func (h *harness) shutdown() { h.cancel() }

// start builds the machine over c, as a daemon does for a claim it created or loaded.
func (h *harness) start(c Claim) {
	h.t.Helper()
	m, err := NewMachine(h.ctx, h.deps, c)
	if err != nil {
		h.t.Fatalf("new machine: %v", err)
	}
	h.m = m
}

// restart is a daemon restart: the machine is dropped with whatever it held in memory — its
// timers die with it — and a new one is built from what the store read back.
func (h *harness) restart() {
	h.t.Helper()
	h.m.Wait()
	h.clock.dropAll()
	h.start(h.store.load(h.token))
}

// handle posts one event and waits for the decision it starts, with the worker answering a turn's
// end as its plugin does: the turn-outcome report naming the task whose take's turn it was,
// right behind the end (report). The worker then takes a value it has not asked at, if its host
// is idle (workerAsks).
func (h *harness) handle(ev Event) error {
	return h.post(ev, true)
}

// handleQuiet is handle for a turn that was not the pending task's — a role's own background
// result waking a settled session — whose end no report follows, so only the end frame's bound
// finishes a task (finishAtTurnsEnd).
func (h *harness) handleQuiet(ev Event) error {
	return h.post(ev, false)
}

func (h *harness) post(ev Event, report bool) error {
	var ran uint64
	if _, ending := ev.(StreamTurnEnd); ending && report {
		if p := h.m.Claim().Pending; p != nil && p.turnStarted() {
			ran = p.Number
		}
	}
	err := h.m.Handle(h.ctx, ev)
	h.m.Wait()
	if ran != 0 {
		// The worker reports the turn whatever became of the claim at its end. A report the
		// claim's state refuses — the turn's end ran a held suspension — is the daemon's answer to
		// the worker, as a 409 is to its plugin, and no failure of the event posted.
		var refused *RefusedError
		if rerr := h.reportTurn(ran, nil); rerr != nil && !errors.As(rerr, &refused) {
			err = errors.Join(err, rerr)
		}
	}
	h.workerAsks()
	return err
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
	if err := h.reportTurn(h.pending().Number, providerError); err != nil {
		h.t.Fatalf("report the turn of task %d: %v", h.pending().Number, err)
	}
}

// workerAsks is the worker's own reading of its value: seen once per revision while its agent is
// registered, and taken once per revision while its host is idle. A process that registered anew
// asks afresh, since its memory went with the process it replaced. A seen report or a take the
// daemon answered with nothing but a failure — a store a test is failing — is sent again after the
// next event, as the plugin sends a request again that had no answer (departure 7 of
// dispatch://INFRA-2811).
func (h *harness) workerAsks() {
	h.t.Helper()
	w := h.worker
	if w.manual {
		return
	}
	c := h.m.Claim()
	if w.generation != c.Generation || w.seen == nil {
		w.generation, w.seen, w.asked = c.Generation, map[TaskValue]bool{}, map[TaskValue]bool{}
	}
	value, ok := h.values.value(h.token)
	if !ok || !HoldsCapability(c.State) {
		return
	}
	if !w.seen[value] {
		if err := h.m.Handle(h.ctx, RequestTaskSeen{Claim: h.token, Generation: c.Generation, Session: c.Session,
			Number: value.Number, Epoch: value.Epoch, Revision: value.Revision}); err != nil {
			return
		}
		w.seen[value] = true
	}
	if (c.State != StateReady && c.State != StateIdle) || w.asked[value] {
		return
	}
	if answer, err := h.takeValue(value); err != nil && answer.Outcome == "" {
		return
	}
	w.asked[value] = true
}

// takeValue is a take of value's number at its revision, recorded as the worker's. An answer the
// daemon gave beside a failure of the rest of its decision is the worker's all the same
// (Machine.Take), so it is recorded whatever the error.
func (h *harness) takeValue(value TaskValue) (TakeAnswer, error) {
	pending := h.m.Claim().Pending
	answer, err := h.m.Take(h.ctx, RequestTake{Claim: h.token, Generation: h.generation(), Session: h.claim().Session,
		Number: value.Number, Epoch: value.Epoch, Revision: value.Revision})
	h.m.Wait()
	if answer.Outcome == "" {
		return answer, err
	}
	h.worker.answers = append(h.worker.answers, answer)
	if answer.Outcome == TakeOutcomeAccepted {
		id := ""
		if pending != nil {
			id = pending.ID
		}
		h.worker.taken = append(h.worker.taken, taken{DeliveryID: id, Number: value.Number, Message: answer.Text})
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
		Number: value.Number, Epoch: value.Epoch, Revision: value.Revision}); err != nil {
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
	c.TaskWrittenEpoch, c.TaskWrittenRevision = epoch, revision
}

func (h *harness) must(ev Event) {
	h.t.Helper()
	if err := h.handle(ev); err != nil {
		h.t.Fatalf("handle %T: %v", ev, err)
	}
}

// mustQuiet is must for a turn whose end no prompt_result follows (handleQuiet).
func (h *harness) mustQuiet(ev Event) {
	h.t.Helper()
	if err := h.handleQuiet(ev); err != nil {
		h.t.Fatalf("handle %T: %v", ev, err)
	}
}

// handleWithoutPanic is handle, failing the test where ev panics the machine instead of ending the
// whole run.
func (h *harness) handleWithoutPanic(ev Event) error {
	h.t.Helper()
	defer func() {
		if r := recover(); r != nil {
			h.t.Fatalf("%T panicked the machine: %v", ev, r)
		}
	}()
	return h.handle(ev)
}

func (h *harness) advance(d time.Duration) {
	h.clock.Advance(d)
	h.m.Wait()
	h.workerAsks()
}

func (h *harness) claim() Claim { return h.m.Claim() }

func (h *harness) state() ClaimState { return h.m.Claim().State }

func (h *harness) wantState(want ClaimState) {
	h.t.Helper()
	if got := h.state(); got != want {
		h.t.Fatalf("state = %s, want %s", got, want)
	}
}

func (h *harness) locator() runtime.Locator {
	h.t.Helper()
	loc := h.claim().Locator
	if loc == nil {
		h.t.Fatal("the claim has no locator")
	}
	return *loc
}

func (h *harness) generation() uint64 { return h.claim().Generation }

func (h *harness) observe(kind runtime.ObservationKind) {
	h.t.Helper()
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: kind, At: h.clock.Now()}})
}

// prompts is every take of the worker's the daemon accepted, in order: what reached the worker.
func (h *harness) prompts() []taken { return slices.Clone(h.worker.taken) }

// wantPrompts is prompts, failing the test unless n takes were accepted.
func (h *harness) wantPrompts(n int) []taken {
	h.t.Helper()
	prompts := h.prompts()
	if len(prompts) != n {
		h.t.Fatalf("takes accepted = %d (%+v), want %d", len(prompts), prompts, n)
	}
	return prompts
}

func (h *harness) pending() Delivery {
	h.t.Helper()
	p := h.claim().Pending
	if p == nil {
		h.t.Fatal("the claim has no pending delivery")
	}
	return *p
}

func (h *harness) budgets() Budgets { return h.claim().Budgets }

func (h *harness) wantBudgets(want Budgets) {
	h.t.Helper()
	if got := h.budgets(); got != want {
		h.t.Fatalf("budgets = %+v, want %+v", got, want)
	}
}

// wantFailure is the whole record a failure leaves on the claim: the cause word the site that
// failed it chose, the sentence that site wrote for a person to read — matched on a fragment,
// since several of them carry the error the runtime or the provider answered with — and the
// moment of the failure, which lies between the clock reading the caller took before the step
// that failed the claim and the clock now. It says nothing of the claim's state: a claim an
// operator retired after it failed carries the same record.
func (h *harness) wantFailure(cause FailureCause, detail string, since time.Time) {
	h.t.Helper()
	got := h.claim()
	if got.FailureCause == cause && strings.Contains(got.Failure, detail) &&
		!got.FailedAt.Before(since) && !got.FailedAt.After(h.clock.Now()) {
		return
	}
	h.t.Fatalf("the claim carries cause %q, failure %q and failed at %s; want cause %q, a failure naming %q, and a failure between %s and %s",
		got.FailureCause, got.Failure, got.FailedAt, cause, detail, since, h.clock.Now())
}

// wantNoFailure is a claim that carries no failure record at all, which is every claim but one
// the daemon has failed and not launched again.
func (h *harness) wantNoFailure() {
	h.t.Helper()
	if got := h.claim(); got.FailureCause != "" || got.Failure != "" || !got.FailedAt.IsZero() {
		h.t.Fatalf("the claim carries cause %q, failure %q and failed at %s, want no record of a failure",
			got.FailureCause, got.Failure, got.FailedAt)
	}
}

func (h *harness) calls(method string) []fake.Call { return h.rt.CallsOf(method) }

func (h *harness) wantCalls(method string, n int) []fake.Call {
	h.t.Helper()
	calls := h.calls(method)
	if len(calls) != n {
		h.t.Fatalf("%s calls = %d, want %d (all calls: %v)", method, len(calls), n, h.rt.Methods())
	}
	return calls
}

// launch spawns the queued claim.
func (h *harness) launch() {
	h.t.Helper()
	h.must(RequestSpawn{Claim: h.token})
	h.wantState(StateLaunching)
}

// connect is the shim's hello, with its connection in the directory.
func (h *harness) connect() {
	h.t.Helper()
	h.conns.Register(h.token, h.conn)
	h.must(StreamHello{Claim: h.token, Generation: h.generation()})
}

// register posts a registration naming the harness's own plugin contract, so every claim it brings
// up is current from the start; a test of plugin staleness posts its own RequestRegister naming a
// different one instead.
func (h *harness) register() {
	h.t.Helper()
	h.must(RequestRegister{
		Claim:          h.token,
		Generation:     h.generation(),
		Session:        session,
		SessionFile:    sessionFile,
		CapabilityHash: []byte("capability"),
		PluginContract: h.deps.PluginContract,
	})
}

func (h *harness) ready() {
	h.t.Helper()
	h.must(RequestReady{Claim: h.token, Generation: h.generation(), Session: session})
}

// relaunched walks a claim the machine has just relaunched back to ready.
func (h *harness) relaunched() {
	h.t.Helper()
	h.wantState(StateLaunching)
	h.connect()
	h.register()
	h.ready()
}

// reach walks the claim from where it is to state through the events that lead there. Working
// and idle are reached through a delivered task, so they carry one: working holds it taken with
// its turn running — delivered, seen, taken by the worker, its turn started — and idle has
// finished it, by the turn's end and the worker's report of it.
func (h *harness) reach(state ClaimState) {
	h.t.Helper()
	steps := []ClaimState{StateQueued, StateLaunching, StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle}
	if state == StateSuspended {
		h.reach(StateIdle)
		h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
		h.wantState(StateSuspended)
		return
	}
	from, target := slices.Index(steps, h.state()), slices.Index(steps, state)
	if from < 0 || target < from {
		h.t.Fatalf("reach: no walk from %s to %s", h.state(), state)
	}
	for _, step := range steps[from+1 : target+1] {
		switch step {
		case StateLaunching:
			h.launch()
		case StateShimConnected:
			h.connect()
		case StateRegistered:
			h.register()
		case StateReady:
			h.ready()
		case StateWorking:
			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
			if h.worker.manual {
				h.mustTake()
			}
			if p := h.claim().Pending; p == nil || !p.awaitsTurn() {
				h.t.Fatalf("reach working: the task's take was not accepted: %+v", h.worker.answers)
			}
			h.must(StreamTurnStart{Claim: h.token})
		case StateIdle:
			h.must(StreamTurnEnd{Claim: h.token})
		}
		h.wantState(step)
	}
}

// gatedAdopt holds every adoption of the working copy at the door until the test lets it through,
// so a test can land an event, or a second take, while a take's adoption is still running.
type gatedAdopt struct {
	*fake.Runtime
	entered chan struct{}
	release chan struct{}
}

func newGatedAdopt(rt *fake.Runtime) *gatedAdopt {
	return &gatedAdopt{Runtime: rt, entered: make(chan struct{}, 8), release: make(chan struct{}, 8)}
}

func (g *gatedAdopt) AdoptWorkingCopy(ctx context.Context, loc runtime.Locator, id runtime.GitIdentity) error {
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.Runtime.AdoptWorkingCopy(ctx, loc, id)
}

// gatedRuntime holds Suspend until the test lets it through.
type gatedRuntime struct {
	*fake.Runtime
	entered chan struct{}
	release chan struct{}
}

func (g *gatedRuntime) Suspend(ctx context.Context, loc runtime.Locator) error {
	g.entered <- struct{}{}
	<-g.release
	return g.Runtime.Suspend(ctx, loc)
}

// gatedResume holds Resume until the test lets it through, as a Sandbox relaunch waits on the
// cluster for its pods.
type gatedResume struct {
	*fake.Runtime
	entered chan struct{}
	release chan struct{}
}

func (g *gatedResume) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.Runtime.Resume(ctx, prev, spec)
}

// gatedSpawn holds Spawn until the test lets it through or its context ends, as a Sandbox launch
// waits on the cluster for its pod; a context that ends first is answered with its error, as the
// runtime's waits answer.
type gatedSpawn struct {
	*fake.Runtime
	entered chan struct{}
	release chan struct{}
}

func (g *gatedSpawn) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return runtime.Locator{}, ctx.Err()
	}
	return g.Runtime.Spawn(ctx, spec)
}

func waitFor(t *testing.T, what string, ch <-chan string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return ""
	}
}

var errBoom = errors.New("boom")

// adoptRefusal is the shim answering an adoption `ok: false`, as that answer reaches the machine
// from the stream: an error that is runtime.ErrAdoptRefused (stream.RefusedError.Is) and carries
// the shim's own words, which the refusal's log line names. The sentinel is passed in from here
// rather than scripted on the runtime double, so what a test drives is the error the connection
// really returns.
func adoptRefusal(reason string) error {
	return fmt.Errorf("worker-stream: %s refused adopt-working-copy: %s: %w", testToken, reason, runtime.ErrAdoptRefused)
}

// fakeEnroller is the broker as the machine sees it: it records every enrollment and revocation,
// mints ids, and fails what the test told it to. It verifies the pod token's expiry against now
// as the broker does (enroll.Service.Create): an expired token is refused POD_TOKEN_EXPIRED and
// one whose expiry cannot be read POD_IDENTITY_MISMATCH.
type fakeEnroller struct {
	mu          sync.Mutex
	enrollments []PodEnrollment
	revoked     []string
	enrollErr   error
	revokeErr   error
	revokeFails int // Revoke fails this many times, then succeeds
	calls       int // every Revoke call, failed or not
	next        int
	now         func() time.Time
}

func (f *fakeEnroller) Enroll(_ context.Context, e PodEnrollment) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enrollErr != nil {
		return "", f.enrollErr
	}
	expires, ok := tokenExpiry(e.PodToken)
	switch {
	case !ok:
		return "", permanentErr{"broker answered 403 POD_IDENTITY_MISMATCH: the projected token does not identify this pod"}
	case !expires.After(f.now()):
		return "", expiredTokenErr{"broker answered 403 POD_TOKEN_EXPIRED: the projected token has expired (oidc: token expired at " + expires.UTC().Format(time.RFC3339) + ")"}
	}
	f.enrollments = append(f.enrollments, e)
	f.next++
	return fmt.Sprintf("enr-%d", f.next), nil
}

func (f *fakeEnroller) Revoke(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.revokeFails > 0 {
		f.revokeFails--
		return errors.New("broker unreachable")
	}
	if f.revokeErr != nil {
		return f.revokeErr
	}
	f.revoked = append(f.revoked, id)
	return nil
}

func (f *fakeEnroller) enrolled() []PodEnrollment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.enrollments)
}

func (f *fakeEnroller) revocations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.revoked)
}

func (f *fakeEnroller) revokeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type permanentErr struct{ msg string }

func (e permanentErr) Error() string   { return e.msg }
func (e permanentErr) Permanent() bool { return true }

// podToken is a projected service-account token as the shim reads it from its mount, expiring at
// expires: the broker verifies all of it, and nothing on the daemon's side reads more than its exp.
func podToken(expires time.Time) string {
	payload, err := json.Marshal(struct {
		Exp int64 `json:"exp"`
	}{expires.Unix()})
	if err != nil {
		panic(err)
	}
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// tokenExpiry is the exp claim of a token podToken made, as the broker reads it once verified.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// testIdentity is a hello's identity whose token outlives every test that does not move the clock
// an hour.
var testIdentity = &AgentSecretsIdentity{Thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs", PodToken: podToken(epoch.Add(time.Hour))}

// withSecrets gives the harness a broker; hello sends the identity with every hello from then on.
func (h *harness) withSecrets() *fakeEnroller {
	h.t.Helper()
	f := &fakeEnroller{now: h.clock.Now}
	h.deps.Secrets = f
	h.start(h.store.load(h.token))
	return f
}

// helloWithIdentity is the shim's hello2 carrying the pod's identity at the claim's generation,
// with its connection in the directory, as connect (`:501`) registers it.
func (h *harness) helloWithIdentity() {
	h.t.Helper()
	h.helloWithToken(testIdentity.PodToken)
}

// helloWithToken is helloWithIdentity with the projected token the shim read at this hello.
func (h *harness) helloWithToken(token string) {
	h.t.Helper()
	h.conns.Register(h.token, h.conn)
	h.must(StreamHello{Claim: h.token, Generation: h.generation(), AgentSecrets: &AgentSecretsIdentity{Thumbprint: testIdentity.Thumbprint, PodToken: token}})
}

// exit is the agent reporting its own end, fenced on the claim's generation and session.
func (h *harness) exit() {
	h.t.Helper()
	h.must(RequestExit{Claim: h.token, Generation: h.generation(), Session: session, Reason: "done"})
}

package supervise

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// lookFence is a test stub for Deps.LookHolds: it holds a single open look and id while the issue
// is out of the workflow, matching record.LookHolds.
type lookFence struct {
	mu         sync.Mutex
	openLook   CheckLook
	openID     string
	inWorkflow bool
}

func (f *lookFence) open(look CheckLook, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openLook, f.openID, f.inWorkflow = look, id, false
}

func (f *lookFence) end() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openLook, f.openID = CheckLook{}, ""
}

func (f *lookFence) enterWorkflow() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inWorkflow = true
}

func (f *lookFence) holds(_ context.Context, _ string, look CheckLook, lookID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inWorkflow || f.openID == "" || lookID == "" {
		return false, nil
	}
	return look.same(f.openLook) && lookID == f.openID, nil
}

func newLookHarness(t *testing.T, initial Claim) (*harness, *lookFence) {
	t.Helper()
	h := newBareHarness(t)
	h.token = initial.Token
	fence := &lookFence{}
	h.deps.LookHolds = fence.holds
	if err := h.store.PutClaim(h.ctx, initial); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if initial.Pending != nil {
		if err := h.store.PutDelivery(h.ctx, initial.Token, *initial.Pending); err != nil {
			t.Fatalf("seed pending delivery: %v", err)
		}
	}
	h.start(initial)
	return h, fence
}

// TestAnEndedLooksTaskGivesWayToTheNextTask holds every row of the state table for a pending
// production-check look task and a newer task or retirement (dispatch://INFRA-3518, D4 and D5).
func TestAnEndedLooksTaskGivesWayToTheNextTask(t *testing.T) {
	t.Parallel()

	opened := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	look1 := CheckLook{Look: 1, OpenedAt: opened}
	look2 := CheckLook{Look: 2, OpenedAt: opened}
	const (
		look1ID = "LOOKID00000000000000000001"
		look2ID = "LOOKID00000000000000000002"
	)
	reqL1 := RequestDeliver{
		Claim: testToken, ID: "outbox:1", Row: 1,
		Task:       "Production check of LEGION-209 (look 1).",
		Generation: 1, CheckLook: look1, LookID: look1ID,
	}
	reqL2 := RequestDeliver{
		Claim: testToken, ID: "outbox:2", Row: 2,
		Task:       "Production check of LEGION-209 (look 2).",
		Generation: 1, CheckLook: look2, LookID: look2ID,
	}

	// setupAckedL1 reaches StateReady, sends and acknowledges L1 so TimerTurn is armed and L1 is
	// outstanding, then moves the fence to look 2 (L1 ended).
	setupAckedL1 := func(t *testing.T) (*harness, *lookFence) {
		t.Helper()
		h, fence := newLookHarness(t, queuedClaim())
		fence.open(look1, look1ID)
		h.reach(StateReady)
		h.must(reqL1)
		h.wantPrompts(1)
		if !h.armed(TimerTurn) {
			t.Fatal("setup: TimerTurn is not armed after L1 acknowledgement")
		}
		if len(h.outstanding()) != 1 || h.outstanding()[0] != reqL1.ID {
			t.Fatalf("setup: outstanding = %v, want [%s]", h.outstanding(), reqL1.ID)
		}
		fence.open(look2, look2ID)
		return h, fence
	}

	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "1 queued never sent replaced by L2 start",
			run: func(t *testing.T) {
				h, fence := newLookHarness(t, queuedClaim())
				fence.open(look1, look1ID)
				h.reach(StateReady)
				h.conns.Remove(testToken)
				h.must(reqL1)
				h.wantPrompts(0)

				fence.open(look2, look2ID)
				h.conns.Register(testToken, h.conn)
				h.must(reqL2)

				p := h.pending()
				if p.ID != reqL2.ID || p.CheckLook != look2 || p.LookID != look2ID {
					t.Fatalf("pending = %+v, want L2 (%s)", p, reqL2.ID)
				}
				sent := h.wantPrompts(1)[0]
				if sent.DeliveryID != reqL2.ID || sent.Message != reqL2.Task {
					t.Fatalf("sent = %+v, want L2", sent)
				}
			},
		},
		{
			name: "2 send in flight refuses L2 then retry replaces L1",
			run: func(t *testing.T) {
				h, fence := newLookHarness(t, queuedClaim())
				fence.open(look1, look1ID)
				h.reach(StateReady)
				gated := newGatedConn()
				h.conns.Register(testToken, gated)
				if err := h.m.Handle(h.ctx, reqL1); err != nil {
					t.Fatalf("start L1 send: %v", err)
				}
				waitFor(t, "L1 send", gated.entered)

				fence.open(look2, look2ID)
				// First half: queue retires no ended look's task while its send is in flight, so
				// L2 is refused ErrDeliveryPending and L1 stays pending.
				if err := h.m.Handle(h.ctx, reqL2); !errors.Is(err, ErrDeliveryPending) {
					t.Fatalf("L2 while L1 send is in flight = %v, want ErrDeliveryPending", err)
				}
				if p := h.pending(); p.ID != reqL1.ID {
					t.Fatalf("pending while L1 send is in flight = %+v, want L1 kept", p)
				}

				gated.release <- struct{}{}
				h.m.Wait()

				// Second half: once L1's send is answered, L2's retry replaces L1 as row 3.
				h.conns.Register(testToken, h.conn)
				h.must(reqL2)
				p := h.pending()
				if p.ID != reqL2.ID || p.CheckLook != look2 {
					t.Fatalf("pending after retry = %+v, want L2", p)
				}
				sent := h.wantPrompts(1)[0]
				if sent.DeliveryID != reqL2.ID || sent.Message != reqL2.Task {
					t.Fatalf("sent = %+v, want L2", sent)
				}
			},
		},
		{
			name: "3 acknowledged TimerTurn armed replaced by L2 start",
			run: func(t *testing.T) {
				h, _ := setupAckedL1(t)
				gated := newGatedConn()
				h.conns.Register(testToken, gated)

				if err := h.m.Handle(h.ctx, reqL2); err != nil {
					t.Fatalf("L2 start behind acknowledged ended L1: %v", err)
				}
				gotID := waitFor(t, "L2 send", gated.entered)
				if gotID != reqL2.ID {
					t.Fatalf("sent delivery = %s, want L2 (%s)", gotID, reqL2.ID)
				}
				if p := h.pending(); p.ID != reqL2.ID || p.CheckLook != look2 {
					t.Fatalf("pending = %+v, want L2", p)
				}
				if out := h.outstanding(); len(out) != 0 {
					t.Fatalf("outstanding prompts after L1 retirement = %v, want none", out)
				}
				if h.armed(TimerTurn) {
					t.Fatal("TimerTurn is still armed after L1 retirement")
				}
				gated.release <- struct{}{}
				h.m.Wait()
			},
		},
		{
			name: "4 L2 send busy then L1 late turn confirms nothing then L2 resent",
			run: func(t *testing.T) {
				h, _ := setupAckedL1(t)
				h.conn.SetStreaming(true)
				h.conn.FailPrompt(busyRefusal())

				h.must(reqL2)
				h.wantPrompts(2)
				h.wantState(StateWorking)

				// L1's late turn starts and must confirm nothing.
				h.must(StreamTurnStart{Claim: testToken})
				if p := h.pending(); p.ID != reqL2.ID || !p.ConfirmedAt.IsZero() {
					t.Fatalf("pending during L1's late turn = %+v, want L2 unconfirmed", p)
				}

				h.conn.FailPrompt(nil)
				h.conn.SetStreaming(false)
				h.mustQuiet(StreamTurnEnd{Claim: testToken})

				sent := h.wantPrompts(3)[2]
				if sent.DeliveryID != reqL2.ID || sent.Message != reqL2.Task {
					t.Fatalf("sent after L1's late turn = %+v, want L2 resent", sent)
				}
			},
		},
		{
			name: "5 L1 late turn during L2 send then busy takes confirmation back",
			run: func(t *testing.T) {
				h, _ := setupAckedL1(t)
				gated := newGatedConn()
				gated.SetStreaming(true)
				gated.FailPrompt(busyRefusal())
				h.conns.Register(testToken, gated)

				if err := h.m.Handle(h.ctx, reqL2); err != nil {
					t.Fatalf("L2 start: %v", err)
				}
				waitFor(t, "L2 send", gated.entered)

				// L1's late turn starts while L2's send is in flight, tentatively confirming L2.
				if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
					t.Fatalf("L1 late turn start: %v", err)
				}
				if p := h.pending(); p.ID != reqL2.ID || p.ConfirmedAt.IsZero() {
					t.Fatalf("pending on turn start during L2 send = %+v, want L2 tentatively confirmed", p)
				}

				// The busy answer takes L2's confirmation back.
				gated.release <- struct{}{}
				h.m.Wait()
				if p := h.pending(); p.ID != reqL2.ID || !p.ConfirmedAt.IsZero() {
					t.Fatalf("pending after busy answer = %+v, want L2 confirmation taken back", p)
				}
				h.wantState(StateWorking)

				// When L1's turn ends, L2 is sent again.
				h.conns.Register(testToken, h.conn)
				h.mustQuiet(StreamTurnEnd{Claim: testToken})
				sent := h.wantPrompts(2)[1]
				if sent.DeliveryID != reqL2.ID || sent.Message != reqL2.Task {
					t.Fatalf("sent after L1's turn = %+v, want L2", sent)
				}
			},
		},
		{
			name: "6 L2 acknowledged then late busy refusal takes L2 back unread and resends",
			run: func(t *testing.T) {
				h, _ := setupAckedL1(t)
				h.must(reqL2)
				h.wantPrompts(2)
				if p := h.pending(); p.ID != reqL2.ID || p.DeliveredAt.IsZero() {
					t.Fatalf("pending after L2 ack = %+v, want L2 marked read", p)
				}

				h.conn.SetStreaming(true)
				h.must(StreamLateRefusal{
					Claim: testToken, DeliveryID: reqL2.ID,
					Outcome: runtime.OutcomeAgentBusy, Error: busyWords,
				})
				p := h.pending()
				if p.ID == reqL2.ID || !p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() || p.CheckLook != look2 {
					t.Fatalf("pending after L2 late busy refusal = %+v, want L2 taken back unread under rotated id", p)
				}
				h.wantState(StateWorking)

				h.conn.SetStreaming(false)
				h.mustQuiet(StreamTurnEnd{Claim: testToken})
				sent := h.wantPrompts(3)[2]
				if sent.DeliveryID != p.ID || sent.Message != reqL2.Task {
					t.Fatalf("sent after busy turn = %+v, want L2 under rotated id %s", sent, p.ID)
				}
			},
		},
		{
			name: "7 L1 prompt_result after L2 is pending is fenced as stale",
			run: func(t *testing.T) {
				// L2's start replaces L1 as in row 3, which drops L1's outstanding prompts, so L1 is
				// neither pending nor outstanding and its late prompt_result is fenced as stale.
				h, _ := setupAckedL1(t)
				h.must(reqL2)
				h.wantPrompts(2)

				before := h.claim()
				h.must(StreamPromptResult{Claim: testToken, DeliveryID: reqL1.ID, AgentInvoked: true})

				after := h.claim()
				if after.Pending == nil || after.Pending.ID != reqL2.ID || !after.Pending.ConfirmedAt.IsZero() {
					t.Fatalf("pending after stale L1 prompt_result = %+v, want L2 unchanged and unconfirmed", after.Pending)
				}
				if after.ServingCheckLook != before.ServingCheckLook || after.State != before.State {
					t.Fatalf("claim changed on stale L1 prompt_result: before=%+v after=%+v", before, after)
				}
			},
		},
		{
			name: "8 confirmed L1 turn running replaced by L2 sent after turn",
			run: func(t *testing.T) {
				// queue retires a confirmed delivery as served and sends the replacement after the
				// running turn ends.
				h, fence := newLookHarness(t, queuedClaim())
				fence.open(look1, look1ID)
				h.reach(StateReady)
				h.must(reqL1)
				h.must(StreamTurnStart{Claim: testToken})
				h.wantState(StateWorking)

				fence.open(look2, look2ID)
				h.must(reqL2)
				if got := h.claim().ServingCheckLook; got != look1 {
					t.Fatalf("ServingCheckLook = %+v, want L1 (%+v)", got, look1)
				}
				if stored := h.store.load(testToken); stored.ServingCheckLook != look1 {
					t.Fatalf("stored ServingCheckLook = %+v, want L1 (%+v)", stored.ServingCheckLook, look1)
				}
				if p := h.pending(); p.ID != reqL2.ID || !p.ConfirmedAt.IsZero() {
					t.Fatalf("pending during L1's turn = %+v, want L2 unconfirmed", p)
				}
				h.wantPrompts(1)

				h.mustQuiet(StreamTurnEnd{Claim: testToken})
				sent := h.wantPrompts(2)[1]
				if sent.DeliveryID != reqL2.ID || sent.Message != reqL2.Task {
					t.Fatalf("sent after L1's turn = %+v, want L2", sent)
				}
			},
		},
		{
			name: "9 operator suspended claim retire_look then resume sends nothing of L1",
			run: func(t *testing.T) {
				h, fence := newLookHarness(t, queuedClaim())
				fence.open(look1, look1ID)
				h.reach(StateReady)
				h.conns.Remove(testToken)
				h.must(reqL1)
				h.must(RequestSuspend{Claim: testToken, By: SuspendedByOperator, Reason: "operator hold"})
				h.wantState(StateSuspended)

				fence.end()
				retired, err := h.m.RetireLookTask(h.ctx, look1, false)
				if err != nil || !retired {
					t.Fatalf("RetireLookTask on suspended claim = %t, %v; want retired", retired, err)
				}
				if p := h.claim().Pending; p != nil {
					t.Fatalf("pending after RetireLookTask = %+v, want nil", p)
				}

				h.must(RequestResume{Claim: testToken, By: SuspendedByOperator})
				h.connect()
				h.register()
				h.ready()
				h.wantPrompts(0)
			},
		},
		{
			name: "10 failed claim retire_look then retry and L2 sends only L2",
			run: func(t *testing.T) {
				failed := fixture(StateFailed)
				failed.Failure = "prompt failures ran out"
				failed.Budgets = Budgets{PromptFailures: 3, PromptRetires: 2}
				failed.Pending = &Delivery{
					ID: reqL1.ID, Task: reqL1.Task, Generation: 1,
					CheckLook: look1, LookID: look1ID, QueuedAt: opened,
				}
				h, fence := newLookHarness(t, failed)
				fence.end()

				retired, err := h.m.RetireLookTask(h.ctx, look1, false)
				if err != nil || !retired {
					t.Fatalf("RetireLookTask on failed claim = %t, %v; want retired", retired, err)
				}
				if p := h.claim().Pending; p != nil {
					t.Fatalf("pending after RetireLookTask = %+v, want nil", p)
				}

				fence.open(look2, look2ID)
				h.must(RequestRetry{Claim: testToken, OneAttemptLeft: true})
				h.must(reqL2)
				h.connect()
				h.register()
				h.ready()

				prompts := h.wantPrompts(1)
				if prompts[0].DeliveryID != reqL2.ID || prompts[0].Message != reqL2.Task {
					t.Fatalf("sent after retry = %+v, want L2 alone", prompts[0])
				}
			},
		},
		{
			name: "11 L2 replaced L1 before restart L2 unacknowledged streaming does not take L2",
			run: func(t *testing.T) {
				h, _ := setupAckedL1(t)
				// Disconnect before queuing L2 so L2 replaces L1 without being sent or acknowledged.
				h.conns.Remove(testToken)
				h.must(reqL2)
				if p := h.pending(); p.ID != reqL2.ID || !p.DeliveredAt.IsZero() {
					t.Fatalf("pending before restart = %+v, want unacknowledged L2", p)
				}

				h.restart()
				if _, askFirst := h.marks(); !askFirst {
					t.Fatal("restarted claim has no askFirst armed")
				}
				h.conn.SetStreaming(true)
				h.conn.FailPrompt(busyRefusal())
				h.connect()

				// askFirst must NOT confirm unacknowledged L2; L2 must be sent (and refused busy).
				sent := h.wantPrompts(2)[1]
				if sent.DeliveryID != reqL2.ID || sent.Message != reqL2.Task {
					t.Fatalf("sent after restart = %+v, want L2 sent rather than taken by askFirst", sent)
				}
				if p := h.pending(); p.ID != reqL2.ID || !p.ConfirmedAt.IsZero() {
					t.Fatalf("pending after busy send = %+v, want L2 unconfirmed", p)
				}
			},
		},
		{
			name: "12 issue re-entered workflow phase start replaces queued L1",
			run: func(t *testing.T) {
				h, fence := newLookHarness(t, queuedClaim())
				fence.open(look1, look1ID)
				h.reach(StateReady)
				h.conns.Remove(testToken)
				h.must(reqL1)

				fence.enterWorkflow()
				phaseReq := RequestDeliver{
					Claim: testToken, ID: "outbox:10", Row: 10,
					Task:       "Continue Workflow. Issue: LEGION-209. Phase: testing.",
					Generation: 2, Phase: phase.Testing, Entry: 1,
				}
				h.must(phaseReq)

				p := h.pending()
				if p.ID != phaseReq.ID || p.Phase != phase.Testing || p.Generation != 2 {
					t.Fatalf("pending = %+v, want the new run's phase task", p)
				}
			},
		},
		{
			name: "13 queued L1 whose look ended dropped at ready nothing sent",
			run: func(t *testing.T) {
				h, fence := newLookHarness(t, queuedClaim())
				fence.open(look1, look1ID)
				h.reach(StateRegistered)
				h.must(reqL1)
				h.wantPrompts(0)

				fence.end()
				h.ready()

				h.wantPrompts(0)
				if p := h.claim().Pending; p != nil {
					t.Fatalf("pending after ready = %+v, want L1 dropped", p)
				}
				if got := h.logs.String(); strings.Contains(got, look1ID) || strings.Contains(got, reqL1.Task) {
					t.Fatalf("drop log leaked look id or task text:\n%s", got)
				}
			},
		},
		{
			name: "14 L1 carries no look id dropped at ready nothing sent",
			run: func(t *testing.T) {
				h, fence := newLookHarness(t, queuedClaim())
				// Look 1 is open with a valid look id, but the queued delivery carries none.
				fence.open(look1, look1ID)
				h.reach(StateRegistered)
				h.must(RequestDeliver{
					Claim: testToken, ID: "outbox:old", Row: 1,
					Task:       "Production check of LEGION-209 (older daemon, no look id).",
					Generation: 1, CheckLook: look1,
				})
				h.wantPrompts(0)

				h.ready()

				h.wantPrompts(0)
				if p := h.claim().Pending; p != nil {
					t.Fatalf("pending after ready = %+v, want look task with no id dropped", p)
				}
			},
		},
		{
			name: "15 pending testing task of new run stays and Reported clears budgets",
			run: func(t *testing.T) {
				idle := fixture(StateIdle)
				idle.Budgets = Budgets{PromptFailures: 2, PromptRetires: 1}
				idle.Pending = &Delivery{
					ID: "outbox:20", Task: "Continue Workflow. Issue: LEGION-209. Phase: testing.",
					Generation: 2, Phase: phase.Testing, Entry: 1, QueuedAt: opened,
				}
				h, fence := newLookHarness(t, idle)
				fence.enterWorkflow()

				retired, err := h.m.RetireLookTask(h.ctx, look1, true)
				if err != nil || retired {
					t.Fatalf("RetireLookTask(reported=true) = %t, %v; want not retired, nil", retired, err)
				}
				if p := h.pending(); p.ID != "outbox:20" || p.Phase != phase.Testing || p.Generation != 2 {
					t.Fatalf("pending = %+v, want the new run's testing task kept", p)
				}
				c := h.claim()
				if c.Budgets.PromptFailures != 0 || c.Budgets.PromptRetires != 0 {
					t.Fatalf("budgets = %+v, want PromptFailures=0 and PromptRetires=0", c.Budgets)
				}
				if c.ServingCheckLook != look1 {
					t.Fatalf("ServingCheckLook = %+v, want %+v", c.ServingCheckLook, look1)
				}
				stored := h.store.load(testToken)
				if stored.Budgets.PromptFailures != 0 || stored.Budgets.PromptRetires != 0 || stored.ServingCheckLook != look1 {
					t.Fatalf("stored claim = budgets %+v servingLook %+v, want zero prompt budgets and %+v",
						stored.Budgets, stored.ServingCheckLook, look1)
				}
			},
		},
		{
			name: "16 L1 confirmed stays until turn ends and Reported clears budgets",
			run: func(t *testing.T) {
				working := fixture(StateWorking)
				working.Budgets = Budgets{PromptFailures: 2, PromptRetires: 1}
				working.Pending = &Delivery{
					ID: reqL1.ID, Task: reqL1.Task, Generation: 1,
					CheckLook: look1, LookID: look1ID,
					QueuedAt: opened, DeliveredAt: opened, ConfirmedAt: opened,
				}
				h, fence := newLookHarness(t, working)
				fence.end()

				retired, err := h.m.RetireLookTask(h.ctx, look1, true)
				if err != nil || retired {
					t.Fatalf("RetireLookTask(reported=true) on confirmed L1 = %t, %v; want not retired, nil", retired, err)
				}
				if p := h.pending(); p.ID != reqL1.ID || p.ConfirmedAt.IsZero() {
					t.Fatalf("pending = %+v, want confirmed L1 kept until turn ends", p)
				}
				if b := h.claim().Budgets; b.PromptFailures != 0 || b.PromptRetires != 0 {
					t.Fatalf("budgets = %+v, want PromptFailures=0 and PromptRetires=0", b)
				}

				h.must(StreamTurnEnd{Claim: testToken})
				if p := h.claim().Pending; p != nil {
					t.Fatalf("pending after turn end = %+v, want L1 retired", p)
				}
				if got := h.claim().ServingCheckLook; got != look1 {
					t.Fatalf("ServingCheckLook after turn end = %+v, want %+v", got, look1)
				}
				if stored := h.store.load(testToken); stored.ServingCheckLook != look1 || stored.Budgets.PromptFailures != 0 || stored.Budgets.PromptRetires != 0 {
					t.Fatalf("stored after turn end = look %+v budgets %+v", stored.ServingCheckLook, stored.Budgets)
				}
			},
		},
		{
			name: "17 L1 unconfirmed no send with Reported retired as served TimerTurn disarmed budgets zero",
			run: func(t *testing.T) {
				// Also verify that a report + close (one row with Reported true and one without)
				// ends in the same state in either order (D5).
				for _, order := range []struct {
					name  string
					calls []bool
				}{
					{name: "reported then close", calls: []bool{true, false}},
					{name: "close then reported", calls: []bool{false, true}},
				} {
					t.Run(order.name, func(t *testing.T) {
						ready := fixture(StateReady)
						ready.Pending = nil
						ready.Budgets = Budgets{PromptFailures: 2, PromptRetires: 1}
						h, fence := newLookHarness(t, ready)
						h.conns.Register(testToken, h.conn)
						fence.open(look1, look1ID)
						h.must(reqL1)
						if !h.armed(TimerTurn) {
							t.Fatal("setup: TimerTurn not armed")
						}

						fence.end()
						for i, reported := range order.calls {
							retired, err := h.m.RetireLookTask(h.ctx, look1, reported)
							if err != nil {
								t.Fatalf("call %d (reported=%t): %v", i, reported, err)
							}
							if wantRetired := i == 0; retired != wantRetired {
								t.Fatalf("call %d (reported=%t) retired = %t, want %t", i, reported, retired, wantRetired)
							}
						}

						if p := h.claim().Pending; p != nil {
							t.Fatalf("pending = %+v, want nil", p)
						}
						if h.armed(TimerTurn) {
							t.Fatal("TimerTurn still armed after RetireLookTask")
						}
						if out := h.outstanding(); len(out) != 0 {
							t.Fatalf("outstanding = %v, want none", out)
						}
						c := h.claim()
						if c.ServingCheckLook != look1 {
							t.Fatalf("ServingCheckLook = %+v, want %+v", c.ServingCheckLook, look1)
						}
						if c.Budgets.PromptFailures != 0 || c.Budgets.PromptRetires != 0 {
							t.Fatalf("budgets = %+v, want zero prompt budgets", c.Budgets)
						}
						stored := h.store.load(testToken)
						if stored.Pending != nil || stored.ServingCheckLook != look1 || stored.Budgets.PromptFailures != 0 || stored.Budgets.PromptRetires != 0 {
							t.Fatalf("stored = pending %+v look %+v budgets %+v", stored.Pending, stored.ServingCheckLook, stored.Budgets)
						}
					})
				}
			},
		},
		{
			name: "18 L1 send in flight retire_look returns ErrTaskSending then retry retires L1",
			run: func(t *testing.T) {
				h, fence := newLookHarness(t, queuedClaim())
				fence.open(look1, look1ID)
				h.reach(StateReady)
				gated := newGatedConn()
				h.conns.Register(testToken, gated)
				if err := h.m.Handle(h.ctx, reqL1); err != nil {
					t.Fatalf("start L1 send: %v", err)
				}
				waitFor(t, "L1 send", gated.entered)

				fence.end()
				if retired, err := h.m.RetireLookTask(h.ctx, look1, false); retired || !errors.Is(err, ErrTaskSending) {
					t.Fatalf("RetireLookTask while sending = %t, %v; want false, ErrTaskSending", retired, err)
				}
				if p := h.pending(); p.ID != reqL1.ID {
					t.Fatalf("pending while sending = %+v, want L1 kept", p)
				}

				gated.release <- struct{}{}
				h.m.Wait()

				retired, err := h.m.RetireLookTask(h.ctx, look1, false)
				if err != nil || !retired {
					t.Fatalf("RetireLookTask retry = %t, %v; want true, nil", retired, err)
				}
				if p := h.claim().Pending; p != nil {
					t.Fatalf("pending after retry = %+v, want nil", p)
				}
				if h.armed(TimerTurn) {
					t.Fatal("TimerTurn still armed after RetireLookTask retry")
				}
			},
		},
	}

	for _, row := range rows {
		t.Run(row.name, row.run)
	}
}

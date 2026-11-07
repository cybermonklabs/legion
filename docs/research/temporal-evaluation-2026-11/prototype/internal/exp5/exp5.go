// Package exp5 is experiment 5 of the Temporal evaluation: admission.
//
// Legion may run only so many issues at once. The cap has to hold across the whole
// deployment, so a worker's own concurrency setting cannot be the mechanism: it caps one
// process, and two processes would then run twice the cap. The mechanism here is a
// long-lived gatekeeper workflow that holds the counter, takes acquire and release by
// signal, answers its state by query, and continues as new when it falls idle.
//
// The concurrency the cap produces is observed rather than assumed: the activity that
// stands for the work records when every hold began and ended, and the experiment
// reports the largest number of holds that overlapped. The same workflows run again with
// the cap removed, and that control must exceed the cap -- otherwise the measurement
// would prove nothing, and the experiment fails.
package exp5

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// The registered names of this experiment's pieces.
const (
	GatekeeperWorkflowName = "exp5Gatekeeper"
	IssueWorkflowName      = "exp5Issue"
	HoldActivityName       = "exp5Hold"
	AcquireSignal          = "acquire"
	ReleaseSignal          = "release"
	GrantSignal            = "granted"
	StateQuery             = "state"
)

// Acquire is one issue asking for a slot: who asks, and where the grant must be sent.
type Acquire struct {
	IssueID    string
	WorkflowID string
}

// GateState is everything the gatekeeper knows. It is the workflow's argument as well,
// because continue-as-new carries it into the next run.
type GateState struct {
	// Cap is how many issues may hold a slot at once.
	Cap int
	// Holders is the issues holding one, Queue the issues waiting in arrival order.
	Holders []string
	Queue   []Acquire
	// Admitted and Released count slots given and given back, across generations.
	Admitted int
	Released int
	// Signals is how many signals this run has handled, Generation how many runs came
	// before it. RollAfter is how many signals a run handles before it continues as
	// new, which it does only once it is idle.
	Signals    int
	Generation int
	RollAfter  int
}

// Gatekeeper is the cap: one workflow holding one counter for the whole deployment. It
// grants a waiting issue a slot whenever one is free, and continues as new once it has
// handled its run's worth of signals and nothing is in flight.
func Gatekeeper(ctx workflow.Context, s GateState) error {
	if err := workflow.SetQueryHandler(ctx, StateQuery, func() (GateState, error) { return s, nil }); err != nil {
		return err
	}
	signals := workflow.NewSelector(ctx)
	signals.AddReceive(workflow.GetSignalChannel(ctx, AcquireSignal), func(ch workflow.ReceiveChannel, _ bool) {
		var ask Acquire
		ch.Receive(ctx, &ask)
		s.Signals++
		if !holds(s.Holders, ask.IssueID) && !queued(s.Queue, ask.IssueID) {
			s.Queue = append(s.Queue, ask)
		}
	})
	signals.AddReceive(workflow.GetSignalChannel(ctx, ReleaseSignal), func(ch workflow.ReceiveChannel, _ bool) {
		var issueID string
		ch.Receive(ctx, &issueID)
		s.Signals++
		if kept, dropped := without(s.Holders, issueID); dropped {
			s.Holders = kept
			s.Released++
		}
	})
	for {
		signals.Select(ctx)
		for signals.HasPending() {
			signals.Select(ctx)
		}
		for len(s.Queue) > 0 && len(s.Holders) < s.Cap {
			next := s.Queue[0]
			if err := workflow.SignalExternalWorkflow(ctx, next.WorkflowID, "", GrantSignal, next.IssueID).Get(ctx, nil); err != nil {
				return fmt.Errorf("grant a slot to %s: %w", next.IssueID, err)
			}
			s.Queue = s.Queue[1:]
			s.Holders = append(s.Holders, next.IssueID)
			s.Admitted++
		}
		if s.Signals >= s.RollAfter && len(s.Holders) == 0 && len(s.Queue) == 0 && !signals.HasPending() {
			s.Signals, s.Generation = 0, s.Generation+1
			return workflow.NewContinueAsNewError(ctx, GatekeeperWorkflowName, s)
		}
	}
}

// IssueParams is one issue's run. An empty Gatekeeper is the negative control: the same
// work with no cap in front of it.
type IssueParams struct {
	IssueID      string
	Gatekeeper   string
	Label        string
	Hold         time.Duration
	GrantTimeout time.Duration
}

// IssueWorkflow is one issue: ask the gatekeeper for a slot, wait for the grant, do the
// work, give the slot back. The wait re-asks on a timer, so an acquire lost to the
// gatekeeper's continue-as-new costs a delay rather than the run.
func IssueWorkflow(ctx workflow.Context, p IssueParams) (string, error) {
	if p.Gatekeeper != "" {
		grants := workflow.GetSignalChannel(ctx, GrantSignal)
		for granted := false; !granted; {
			ask := Acquire{IssueID: p.IssueID, WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID}
			if err := workflow.SignalExternalWorkflow(ctx, p.Gatekeeper, "", AcquireSignal, ask).Get(ctx, nil); err != nil {
				return "", fmt.Errorf("ask %s for a slot: %w", p.Gatekeeper, err)
			}
			timerCtx, stopTimer := workflow.WithCancel(ctx)
			waiting := workflow.NewSelector(ctx)
			waiting.AddReceive(grants, func(ch workflow.ReceiveChannel, _ bool) {
				var granting string
				ch.Receive(ctx, &granting)
				granted = true
			})
			waiting.AddFuture(workflow.NewTimer(timerCtx, p.GrantTimeout), func(workflow.Future) {})
			waiting.Select(ctx)
			stopTimer()
		}
	}

	held := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: p.Hold + time.Minute})
	var stamp string
	err := workflow.ExecuteActivity(held, HoldActivityName, HoldRequest{
		IssueID:  p.IssueID,
		Label:    p.Label,
		Duration: p.Hold,
	}).Get(held, &stamp)

	if p.Gatekeeper != "" {
		if release := workflow.SignalExternalWorkflow(ctx, p.Gatekeeper, "", ReleaseSignal, p.IssueID).Get(ctx, nil); release != nil && err == nil {
			err = fmt.Errorf("give the slot back to %s: %w", p.Gatekeeper, release)
		}
	}
	return stamp, err
}

// HoldRequest is one piece of work that occupies a slot for a while.
type HoldRequest struct {
	IssueID  string
	Label    string
	Duration time.Duration
}

// Hold is one recorded occupation of a slot: who held it, and between which moments.
type Hold struct {
	IssueID string
	Begin   time.Time
	End     time.Time
}

// HoldSlot is the work an admitted issue does. It records the hold's beginning and end
// with the clock, so the experiment measures the concurrency that happened instead of
// trusting the gatekeeper's own count.
func HoldSlot(ctx context.Context, req HoldRequest) (string, error) {
	rec := recorderFor(req.Label)
	at, live := rec.begin(req.IssueID)
	defer rec.end(at)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(req.Duration):
	}
	return fmt.Sprintf("issue=%s label=%s attempt=%d holds-live-at-start=%d",
		req.IssueID, req.Label, activity.GetInfo(ctx).Attempt, live), nil
}

// Register puts the gatekeeper, the issue workflow and the work on a worker.
func Register(w worker.Worker) {
	w.RegisterWorkflowWithOptions(Gatekeeper, workflow.RegisterOptions{Name: GatekeeperWorkflowName})
	w.RegisterWorkflowWithOptions(IssueWorkflow, workflow.RegisterOptions{Name: IssueWorkflowName})
	w.RegisterActivityWithOptions(HoldSlot, activity.RegisterOptions{Name: HoldActivityName})
}

// recorder is what one labelled run of the workload did: every hold it ran, and the most
// it ever had live at once.
type recorder struct {
	mu    sync.Mutex
	holds []Hold
	live  int
	peak  int
}

var (
	recordersMu sync.Mutex
	recorders   = map[string]*recorder{}
)

// recorderFor is the recorder of one labelled run, made on first use.
func recorderFor(label string) *recorder {
	recordersMu.Lock()
	defer recordersMu.Unlock()
	rec, ok := recorders[label]
	if !ok {
		rec = &recorder{}
		recorders[label] = rec
	}
	return rec
}

// begin records a hold starting and reports its index and how many are now live.
func (r *recorder) begin(issueID string) (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.holds = append(r.holds, Hold{IssueID: issueID, Begin: time.Now()})
	r.live++
	if r.live > r.peak {
		r.peak = r.live
	}
	return len(r.holds) - 1, r.live
}

// end records the hold at index finishing.
func (r *recorder) end(at int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.holds[at].End = time.Now()
	r.live--
}

// Observed is what a labelled run of the workload really did: how many holds it ran, the
// most that were live at one moment as the activity counted them, and the most that
// overlapped according to their recorded timestamps. The two maxima are measured in
// different ways on purpose: a counter can be wrong, and the timestamps can be checked.
func Observed(label string) (holds int, peakLive int, peakOverlap int, records []Hold) {
	rec := recorderFor(label)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	records = append(records, rec.holds...)
	return len(rec.holds), rec.peak, overlap(records), records
}

// overlap is the largest number of holds that were live at the same moment, swept over
// the recorded timestamps. A hold with no end yet counts as still live.
func overlap(holds []Hold) int {
	type edge struct {
		at    time.Time
		delta int
	}
	edges := make([]edge, 0, 2*len(holds))
	for _, held := range holds {
		edges = append(edges, edge{at: held.Begin, delta: 1})
		if !held.End.IsZero() {
			edges = append(edges, edge{at: held.End, delta: -1})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].at.Equal(edges[j].at) {
			// An end at the very instant another begins is not an overlap.
			return edges[i].delta < edges[j].delta
		}
		return edges[i].at.Before(edges[j].at)
	})
	live, peak := 0, 0
	for _, e := range edges {
		live += e.delta
		if live > peak {
			peak = live
		}
	}
	return peak
}

// holds reports whether an issue is among the slot holders.
func holds(holders []string, issueID string) bool {
	for _, holder := range holders {
		if holder == issueID {
			return true
		}
	}
	return false
}

// queued reports whether an issue is already waiting.
func queued(queue []Acquire, issueID string) bool {
	for _, waiting := range queue {
		if waiting.IssueID == issueID {
			return true
		}
	}
	return false
}

// without drops one issue from the holders, reporting whether it was there.
func without(holders []string, issueID string) ([]string, bool) {
	for i, holder := range holders {
		if holder == issueID {
			return append(holders[:i:i], holders[i+1:]...), true
		}
	}
	return holders, false
}

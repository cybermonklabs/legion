// Package exp2 is experiment 2 of the Temporal evaluation: the human gate.
//
// Legion waits for people: an architect approves a spec, and the issue moves on. The
// wait is open-ended -- hours or days -- and a new version of the document withdraws
// whatever approval the old one had. The question is what such a wait costs when it is a
// workflow blocked on a signal rather than a row in a table polled by a daemon.
//
// The gate here holds a document version and the version its standing approval covers.
// It moves to the next phase only when the two agree and the agreement survives the
// confirmation step: a version landing during that step withdraws the approval and the
// gate closes again.
package exp2

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// The registered names of this experiment's pieces.
const (
	WorkflowName            = "exp2GateWorkflow"
	ConfirmActivityName     = "exp2ConfirmApproval"
	NextPhaseActivityName   = "exp2StartNextPhase"
	ApprovalSignal          = "approval"
	VersionSignal           = "version"
	StateQuery              = "state"
	PhaseAwaitingApproval   = "awaiting_person"
	PhaseNext               = "implementing"
	confirmPollInterval     = 100 * time.Millisecond
	confirmActivityPadding  = 15 * time.Second
	nextPhaseActivityWindow = 30 * time.Second
)

// Params is one gate.
type Params struct {
	// IssueID is the issue whose document the gate guards.
	IssueID string
	// ReleaseFile is the file the confirmation activity waits for. It is the
	// experiment's hold on the step during which a new version may land: the driver
	// creates the file only once it has sent the version that withdraws the approval.
	ReleaseFile string
	// ConfirmTimeout bounds that wait, so a driver that never releases fails the run
	// rather than hanging it.
	ConfirmTimeout time.Duration
}

// State is what the gate has seen. The workflow answers it to a query while it waits and
// returns it when it ends, so the driver can show the gate standing still after the
// first approval.
type State struct {
	// Phase is where the issue stands: awaiting_person until the gate opens for good.
	Phase string
	// Document is the current version of the document under approval.
	Document int
	// Approved is the version the standing approval covers, 0 when none stands.
	Approved int
	// Approvals, Versions and Withdrawals count the signals the gate took and the
	// approvals a new version took away.
	Approvals   int
	Versions    int
	Withdrawals int
	// Confirms is how often the gate ran its confirmation step.
	Confirms int
	// AdvancedAfterApproval is the number of the approval that finally moved the issue
	// on: 1 would mean the first approval was enough, 2 that it was not.
	AdvancedAfterApproval int
	// NextPhaseStarted is the prompt the next phase began with, empty until it does.
	NextPhaseStarted string
}

// GateWorkflow waits for an approval of the current document version, confirms it, and
// moves the issue to the next phase -- unless a new version landed meanwhile, which
// withdraws the approval and closes the gate again.
func GateWorkflow(ctx workflow.Context, p Params) (*State, error) {
	s := &State{Phase: PhaseAwaitingApproval, Document: 1}
	if err := workflow.SetQueryHandler(ctx, StateQuery, func() (State, error) { return *s, nil }); err != nil {
		return nil, err
	}
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: p.ConfirmTimeout + confirmActivityPadding,
	})

	// One selector over both signals: Select takes them in the order they arrived, so
	// draining what is pending never reorders an approval and a version against each
	// other.
	approvals := workflow.GetSignalChannel(ctx, ApprovalSignal)
	versions := workflow.GetSignalChannel(ctx, VersionSignal)
	signals := workflow.NewSelector(ctx)
	signals.AddReceive(approvals, func(ch workflow.ReceiveChannel, _ bool) {
		var version int
		ch.Receive(ctx, &version)
		s.Approvals++
		s.Approved = version
	})
	signals.AddReceive(versions, func(ch workflow.ReceiveChannel, _ bool) {
		var version int
		ch.Receive(ctx, &version)
		s.Versions++
		s.Document = version
		if s.Approved != 0 && s.Approved != version {
			s.Withdrawals++
			s.Approved = 0
		}
	})

	confirmed := 0
	for {
		for signals.HasPending() {
			signals.Select(ctx)
		}
		if s.Approved != s.Document {
			// Closed: no approval stands for the document as it is now. The workflow
			// holds nothing while it waits here -- no worker, no activity, no timer.
			signals.Select(ctx)
			continue
		}
		if confirmed == s.Document {
			break
		}
		pending := s.Document
		var ack string
		if err := workflow.ExecuteActivity(ctx, ConfirmActivityName, p.ReleaseFile, p.ConfirmTimeout).Get(ctx, &ack); err != nil {
			return s, fmt.Errorf("confirm approval of version %d: %w", pending, err)
		}
		confirmed = pending
		s.Confirms++
	}

	s.AdvancedAfterApproval = s.Approvals
	s.Phase = PhaseNext
	nextCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: nextPhaseActivityWindow})
	if err := workflow.ExecuteActivity(nextCtx, NextPhaseActivityName, p.IssueID, s.Phase).Get(nextCtx, &s.NextPhaseStarted); err != nil {
		return s, fmt.Errorf("start %s: %w", s.Phase, err)
	}
	return s, nil
}

// ConfirmApproval is the step the gate takes between an approval and the move it
// licenses. It waits for the driver's release file, which is how the experiment holds
// the gate open long enough for a new document version to land inside the step.
func ConfirmApproval(ctx context.Context, releaseFile string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(releaseFile); err == nil {
			return "released by " + releaseFile, nil
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("%s never appeared within %s", releaseFile, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(confirmPollInterval):
		}
	}
}

// StartNextPhase stands for what the gate licenses: the first prompt of the phase the
// issue moves into.
func StartNextPhase(ctx context.Context, issueID, phase string) (string, error) {
	return fmt.Sprintf("issue=%s phase=%s attempt=%d", issueID, phase, activity.GetInfo(ctx).Attempt), nil
}

// Register puts the gate and its activities on a worker.
func Register(w worker.Worker) {
	w.RegisterWorkflowWithOptions(GateWorkflow, workflow.RegisterOptions{Name: WorkflowName})
	w.RegisterActivityWithOptions(ConfirmApproval, activity.RegisterOptions{Name: ConfirmActivityName})
	w.RegisterActivityWithOptions(StartNextPhase, activity.RegisterOptions{Name: NextPhaseActivityName})
}

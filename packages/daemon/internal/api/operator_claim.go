package api

// The claim as the operator routes answer it: OperatorClaim and the views it is built from, and
// the builders every operator route about a claim shares (operatorView, operatorAnswer). The
// routes themselves are operator.go's.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// OperatorClaim is a claim as the operator sees it: every field of the claim record the decision
// table files in the operator column, which these routes alone carry, and all but five of the
// fields it files in the agents column (ClaimRecordFields, internal/api's recordfields.go). The
// five it leaves to the agent surfaces are `servingGeneration`, `pendingCause`,
// `providerErrorRun`, `updateReasons` and `givenEntry`, the last of which an issue's own view
// carries and no claim's; the table's withheld column reaches no surface at all,
// the claim's two hashes among it. Two fields stand here whole where a claim view reduces them:
// the four counts, under `budgets` and again inside `failure`, and the pending task with its
// text, under `pending`. Three keys below are no field of the claim record and stand on the
// answer of the one route that writes them rather than on every claim: ForgottenSession,
// ForgottenSessionFile and PhaseNotRestarted.
type OperatorClaim struct {
	Token                claim.Token `json:"token"`
	Tree                 string      `json:"tree"`
	Issue                string      `json:"issue"`
	Role                 claim.Role  `json:"role"`
	Generation           uint64      `json:"generation"`
	State                string      `json:"state"`
	Session              string      `json:"session"`
	SessionFile          string      `json:"sessionFile"`
	ForgottenSession     string      `json:"forgottenSession,omitempty"`
	ForgottenSessionFile string      `json:"forgottenSessionFile,omitempty"`
	// PhaseNotRestarted is what the request did to the claim that the claim itself does not
	// record, which only the retry with `--fresh-session` sets (freshSession): the claim came
	// back owing no task and the workflow refused that phase's restart, or this request
	// stopped the claim and the machine refused the forget or the start that would have
	// brought it back (notStartedAgain). It is absent everywhere else, a refused retry
	// included, since that answer is the refusal itself and nothing of the claim changed.
	PhaseNotRestarted string `json:"phaseNotRestarted,omitempty"`
	// Locator is the claim's process, the runtime's nested shape; none while nothing runs.
	Locator         *runtime.Locator `json:"locator,omitempty"`
	Budgets         BudgetsView      `json:"budgets"`
	UncertainStreak int              `json:"uncertainStreak"`
	// Pending is the task the claim holds, until the turn it started ends.
	Pending *DeliveryView `json:"pending,omitempty"`
	// SuspendedBy is who suspended the claim while it is suspended: operator, daemon, exit,
	// released (its idle pod was released), hold (the work hold stopped it), or unknown (a
	// suspension recorded before migration 0038); absent otherwise. These routes are the one
	// surface that serves it: a session parses every body it reads with the schema of its own
	// contract, so a key an older contract does not know refuses the whole body at a plugin whose
	// answer side predates the reader that drops an unknown key, and is dropped at every plugin
	// after it (api.StrandedClaim).
	SuspendedBy string `json:"suspendedBy,omitempty"`
	// SuspensionHeld is a suspension the machine holds for the agent's turn (supervise's
	// holdSuspension); absent when none is.
	SuspensionHeld bool `json:"suspensionHeld,omitempty"`
	// SuspensionHeldBy is who asked for the suspension the machine holds, absent when none is
	// held, and Compacting is whether the claim is working because its session is being
	// compacted rather than because a turn is. ProcessGone is whether the runtime's last report
	// of the process this claim still records found it gone, absent for a claim whose last
	// report found it running and for one that records no process. The three are memory only,
	// as SuspensionHeld is: the store writes none of them, so the state route, which reads
	// claims from the store, would serve a constant for each, and these routes, which read the
	// live machine, are the one surface that carries them (internal/api's recordfields.go).
	SuspensionHeldBy string `json:"suspensionHeldBy,omitempty"`
	Compacting       bool   `json:"compacting,omitempty"`
	ProcessGone      bool   `json:"processGone,omitempty"`
	// PluginContract is the contract the claim's current registration declared, 0 when none
	// recorded, always emitted.
	PluginContract int `json:"pluginContract"`
	// UpdateRound is the update round the claim is in, absent when none is open: the pod's drift
	// round (`pod:<pod uid>`), shared by every claim of that pod, which is the one round this
	// daemon opens. This claim's own plugin round (`plugin:<daemon contract>`) is an earlier
	// daemon's, which a boot ends (internal/daemon's endPluginRounds). Once its worker has answered, the process is waiting to
	// be replaced and the daemon holds the claim's next task. UpdateHead is the commit the worker
	// reported as its issue branch's head at origin when it answered, absent until it answers and
	// for a worker that reported none; it is the worker's word, which nothing verifies.
	UpdateRound string `json:"updateRound,omitempty"`
	UpdateHead  string `json:"updateHead,omitempty"`
	// LastTurnStart and LastTurnEnd are when the claim's agent last began a turn and last ended
	// one, each absent until that happens, and both absent for a claim that last ran before this
	// release recorded them. An operator reads them to tell a worker still making progress from
	// one wedged in a turn or idle since long before now.
	LastTurnStart *time.Time `json:"lastTurnStart,omitempty"`
	LastTurnEnd   *time.Time `json:"lastTurnEnd,omitempty"`
	// LastDeathCause is why the claim's process was last found gone, LastStartFailure why its last
	// start did not produce a running agent, and LastReady when its agent last said it could be
	// prompted. Each is absent until it happens, and the two causes are kept apart because they
	// answer different questions: a claim that cannot start and a claim that keeps dying are
	// different faults. LastDeathAt and LastStartFailureAt are when the daemon recorded the cause
	// beside each, set and cleared only with it, as supervise.Claim.LastDeathAt states. LastDeathAt
	// dates the death: a LastDeathAt before LastReady is a death the running agent survived. The
	// claim drops its start failure, with its time, when its agent says it is ready
	// (supervise.Claim.LastStartFailure says which older claims can still hold one from before).
	LastDeathCause     string     `json:"lastDeathCause,omitempty"`
	LastDeathAt        *time.Time `json:"lastDeathAt,omitempty"`
	LastStartFailure   string     `json:"lastStartFailure,omitempty"`
	LastStartFailureAt *time.Time `json:"lastStartFailureAt,omitempty"`
	LastReady          *time.Time `json:"lastReady,omitempty"`
	// Step is what the daemon last saw this claim's agent do (supervise.StepRecord, api.StepView),
	// absent for a claim whose agent has neither begun a turn nor made a call the daemon saw.
	// These routes are the only surface a controller's record reaches: internal/projection puts
	// the controller's claim on no issue, so no claim view of the state carries it, and `legion
	// claims list` is where an operator reads what a stuck controller is doing.
	Step *StepView `json:"step,omitempty"`
	// Failure is why the claim failed: the one word the site that failed it chose, the sentence
	// that site wrote beside it, the moment and the four counts it failed with
	// (ClaimFailureView, FailureViewOf). It is absent for a claim the daemon holds no cause and
	// sentence for — one that never failed, and one that has been given another run since, which
	// is what clears the record (supervise's retry and launchNow) — and stands on a claim an
	// operator retired after it failed, which is what lets the seat reading it say why that
	// worker is gone.
	Failure *ClaimFailureView `json:"failure,omitempty"`
	// LeakCheckCause is the sentence the claim's latest launch try was refused with because the
	// shared public leak check could not be installed in the role's pod
	// (supervise.Claim.LeakCheckCause, ClaimView.LeakCheckCause), absent when that try started
	// its process or failed for another reason. The controller's claim is on no claim view of the
	// state, so these routes are where its cause is read.
	LeakCheckCause string `json:"leakCheckCause,omitempty"`
}

// BudgetsView is the claim's retry counters (`supervise.Budgets`).
type BudgetsView struct {
	LaunchFailures int `json:"launchFailures"`
	Deaths         int `json:"deaths"`
	PromptFailures int `json:"promptFailures"`
	PromptRetires  int `json:"promptRetires"`
}

// BudgetsViewOf is b as the claim views show it. The counters are the claim's own and are
// always served, so this answers a value rather than a pointer, unlike StepViewOf and
// PendingTaskViewOf: every surface that shows a claim shows its four counts, and the failure
// record shows the four it failed with (FailureViewOf).
func BudgetsViewOf(b supervise.Budgets) BudgetsView {
	return BudgetsView{
		LaunchFailures: b.LaunchFailures,
		Deaths:         b.Deaths,
		PromptFailures: b.PromptFailures,
		PromptRetires:  b.PromptRetires,
	}
}

// DeliveryView is the claim's pending task (`supervise.Delivery`): DeliveredAt is the latest
// acknowledgement of this task's prompt, cleared when that prompt is refused, and ConfirmedAt the
// turn a send started; each absent until it happens. SendFailures, LastSendFailureAt and
// LastSendError stand beside them as the record of the sends that did not get that far.
type DeliveryView struct {
	ID   string `json:"id"`
	Task string `json:"task"`
	// Phase is the issue phase the task was queued for, and empty for a task of no phase — an
	// operator's own, an architect's. It is what says whether a pending task is still the work to
	// do, so an operator reading a claim can see it rather than infer it from the task's prose.
	Phase       phase.Phase `json:"phase,omitempty"`
	QueuedAt    time.Time   `json:"queuedAt"`
	DeliveredAt *time.Time  `json:"deliveredAt,omitempty"`
	ConfirmedAt *time.Time  `json:"confirmedAt,omitempty"`
	// Interrupted is a task a process died in a turn of: it is sent behind the sentence saying so
	// (`supervise.Delivery.Interrupted`). Absent for every other task.
	Interrupted bool `json:"interrupted,omitempty"`
	// SendFailures, LastSendFailureAt and LastSendError are how many sends of this task came
	// back from their own send goroutine without reaching a turn, when the last of them failed
	// and the error it failed with (`supervise.Delivery.SendFailures` and its two companions).
	// They are a diagnosis record rather than a budget: the budget is the claim's
	// `promptFailures` and `promptRetires`, which only a charged failure spends, while these
	// count every send that failed there, charged or not — a send lost to the transport is
	// charged nothing and retried for ever, and that is the stall this exists to name. A prompt
	// the agent acknowledged and then refused is charged and read in the budget instead. A send
	// that reaches a turn clears all three, so a pending task no send has failed for carries
	// none of them on the wire.
	SendFailures      int        `json:"sendFailures,omitempty"`
	LastSendFailureAt *time.Time `json:"lastSendFailureAt,omitempty"`
	LastSendError     string     `json:"lastSendError,omitempty"`
}

// OperatorClaims is the list route's answer, in token order, with what the daemon's last
// pod-update sweep decided beside the claims.
type OperatorClaims struct {
	Claims []OperatorClaim `json:"claims"`
	// PodUpdateRounds is the sweep's own last word, which no claim row carries: a claim shows the
	// round it is in, and nothing on it says why the drifted issue beside it was given none. It
	// is a pointer so that a reader can tell an answer that carries no record at all — a daemon
	// of a release before this field — from one whose first sweep has not run, which carries the
	// zero record and says so in `swept` (cmd/legion's podUpdateRoundsLine). The list route
	// fills it on every answer, so only a daemon too old to know the field serves one without it.
	PodUpdateRounds *PodUpdateRounds `json:"pod_update_rounds"`
}

// MarshalJSON keeps `claims` an array on the wire, empty rather than `null`, and
// `pod_update_rounds` an object, the zero record rather than `null`: the field is nil only where
// nobody filled it, and the readers on the other side of this wire take it as required
// (packages/contracts's LegionOperatorClaimsResponse). The sweep's record normalises its own
// lists, which this carries rather than repeats: `type wire` drops the methods of OperatorClaims
// alone, so the field keeps its own (PodUpdateRounds.MarshalJSON).
func (c OperatorClaims) MarshalJSON() ([]byte, error) {
	type wire OperatorClaims
	out := wire(c)
	if out.Claims == nil {
		out.Claims = []OperatorClaim{}
	}
	if out.PodUpdateRounds == nil {
		out.PodUpdateRounds = &PodUpdateRounds{}
	}
	return json.Marshal(out)
}

// PodUpdateRounds is what this daemon's last pod-update sweep decided under the project's two
// limits on how fast one pod template change reaches the fleet (internal/daemon's podUpdateSweep):
// the limits as the configuration holds them, the issues that hold an open round, and the issues
// each limit held back at that sweep. The claims alone cannot say it — a claim carries the round
// it is in, and an issue a limit held back is posted nothing at all — so an operator reading them
// could not tell a fleet the limits are pacing from one the daemon has stopped carrying.
//
// Swept is false for a daemon whose first sweep has not run, where every other field is empty and
// states nothing. Each limit is nil for a key legion.yaml does not set, which is no limit rather
// than a limit of none. Every list is in issue-key order.
type PodUpdateRounds struct {
	Swept bool `json:"swept"`
	// MaxOpenRounds and RoundsPerMinute are `pod_update_max_open_rounds` and
	// `pod_update_rounds_per_minute` as the configuration holds them.
	MaxOpenRounds   *int `json:"max_open_rounds"`
	RoundsPerMinute *int `json:"rounds_per_minute"`
	// InRound is every issue that held one of the project's open-round slots at that sweep, the
	// issues the sweep opened a round for among them.
	InRound []string `json:"in_round"`
	// WaitingOnMaxOpenRounds and WaitingOnRoundsPerMinute are the drifted issues each limit held
	// back at that sweep. An issue both limits held back is under the limit on open rounds alone:
	// that is the one which will still hold it back at the next sweep.
	WaitingOnMaxOpenRounds   []string `json:"waiting_on_max_open_rounds"`
	WaitingOnRoundsPerMinute []string `json:"waiting_on_rounds_per_minute"`
}

// MarshalJSON keeps the three lists arrays on the wire, empty rather than `null`, so a reader
// takes a sweep that held nothing back as the empty list it is rather than as a daemon that said
// nothing of it.
func (r PodUpdateRounds) MarshalJSON() ([]byte, error) {
	type wire PodUpdateRounds
	out := wire(r)
	for _, list := range []*[]string{&out.InRound, &out.WaitingOnMaxOpenRounds, &out.WaitingOnRoundsPerMinute} {
		if *list == nil {
			*list = []string{}
		}
	}
	return json.Marshal(out)
}

// operatorView is one claim as every operator route answers it: the claim as the machine or the
// store holds it, and the step record the store holds. The record is passed on its own because a
// machine's Claim.Step is the copy it was built with and nothing writes it again (supervise's
// putStep, store.PutClaimStep): taking it off c would let an answer carry that boot-time copy by
// forgetting to replace it, so the caller has to say which record it serves (operatorAnswer,
// list).
func operatorView(c supervise.Claim, step supervise.StepRecord) OperatorClaim {
	view := OperatorClaim{
		Token:              c.Token,
		Tree:               c.Tree,
		Issue:              c.Issue,
		Role:               c.Role,
		Generation:         c.Generation,
		State:              string(c.State),
		Session:            c.Session,
		SessionFile:        c.SessionFile,
		Locator:            c.Locator,
		Budgets:            BudgetsViewOf(c.Budgets),
		UncertainStreak:    c.UncertainStreak,
		SuspendedBy:        string(c.SuspendedBy),
		SuspensionHeld:     c.SuspensionHeld,
		SuspensionHeldBy:   string(c.SuspensionHeldBy),
		Compacting:         c.Compacting,
		ProcessGone:        c.ProcessGone,
		PluginContract:     c.PluginContract,
		UpdateRound:        c.UpdateRound,
		UpdateHead:         c.UpdateHead,
		LastTurnStart:      instant(c.LastTurnStart),
		LastTurnEnd:        instant(c.LastTurnEnd),
		LastDeathCause:     c.LastDeathCause,
		LastDeathAt:        instant(c.LastDeathAt),
		LastStartFailure:   c.LastStartFailure,
		LastStartFailureAt: instant(c.LastStartFailureAt),
		LastReady:          instant(c.LastReady),
		Step:               StepViewOf(step),
		Failure:            FailureViewOf(c),
		LeakCheckCause:     c.LeakCheckCause,
	}
	if p := c.Pending; p != nil {
		view.Pending = &DeliveryView{
			ID: p.ID, Task: p.Task, Phase: p.Phase, QueuedAt: p.QueuedAt,
			DeliveredAt: instant(p.DeliveredAt), ConfirmedAt: instant(p.ConfirmedAt), Interrupted: p.Interrupted,
			SendFailures: p.SendFailures, LastSendFailureAt: instant(p.LastSendFailureAt), LastSendError: p.LastSendError,
		}
	}
	return view
}

// operatorAnswer is the answer every operator route about one claim writes: the claim its caller
// read, with the step record read from the store. The caller passes the claim because the routes
// do not read one alike — claimRequest reads the machine's published view, which never waits on a
// launch that holds that machine, where every other route reads Machine.Claim and so waits out
// the decision it has just made. The record is the fact and the machine's copy is a
// snapshot of it: the daemon writes every step through a narrow call of its own and never reads
// one back into the machine's claim (supervise's putStep, store.PutClaimStep), so a machine's
// copy is the record as it stood when that machine was built — at this daemon's boot, for every
// claim it re-adopted. These routes are the one surface a controller's record reaches and the
// ones an operator runs on a worker that has stopped moving, so what they show has to be the
// record the stall backstop reads rather than that snapshot, which is a call hours old, or none
// at all, for every claim this daemon took up at boot.
//
// A read the store refuses answers the record absent — the `-` the listing already shows for a
// claim in no open call — and logs the failure at Warn naming the claim and the error.
//
// Not a 500: the action the request reports has already landed, its decision made and its claim
// written, so failing the request over a read beside it would read as a stop, a resume or a retry
// that did not happen. Not the machine's own copy either: that copy is the boot-time snapshot
// these routes exist to stop serving, and printing a call hours old as the call a worker is in
// now is worse than printing none, because an operator reading it would take a worker that has
// stopped moving for one still at work. An absent record says what the daemon knows: it could
// not read what that claim's agent last did.
func (s *server) operatorAnswer(ctx context.Context, c supervise.Claim) OperatorClaim {
	step, err := s.supervisor.Step(ctx, c.Token)
	if err != nil {
		s.log.Warn("api: read the step record of an operator's claim; answering with no record",
			"claim", c.Token, "error", err)
		step = supervise.StepRecord{}
	}
	return operatorView(c, step)
}

// instant is a time that may not have happened yet: the zero time is absent on the wire.
func instant(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

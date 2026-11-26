package supervise

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A turn that ends in a provider error does not retire its task (dispatch://INFRA-956 R2). The
// work is still owed: the claim used to go idle holding nothing, and the only way to give the
// task back was an edit of the daemon's store. The task is kept here, and the same session is
// prompted with it again after a wait that grows to a limit, with no attempt limit and no new
// session.
//
// Two bounds and one count make that up, and this file holds all three.
const (
	// promptRetryFirst and promptRetryMax bound the ladder: 30 seconds before the first re-send,
	// doubling to at most 10 minutes (promptRetryDelay). internal/backoff is not it — it draws
	// from [step/2, step] and caps at 32 times its base, which this ladder's exactness forbids.
	promptRetryFirst = 30 * time.Second
	promptRetryMax   = 10 * time.Minute
	// providerErrorRunTold is the run of provider-error turns at which the project's controller
	// is told, once (Deps.ProviderErrorRunReached). There is no hold and no attempt limit behind
	// it: a chain's bottom refusing deterministically costs one request every promptRetryMax.
	providerErrorRunTold = 6
)

// The two classes of a failed prompt (StreamPromptResult.Code, shimwire.PromptError.Code) and of
// a refused turn's report (RequestTurnOutcome.Code) that decide something here. Oh My Pi declares
// them on `RpcPromptError.code` (packages/coding-agent/src/modes/rpc/rpc-types.ts of
// cybermonklabs/oh-my-pi at the pinned tag); pi-legion classifies a refused turn with the host's
// own `isCyberPolicyRefusal`, the classifier that sets that code (packages/pi-legion/src/turn-outcome.ts).
const (
	// codeCyberPolicy is OpenAI refusing the session's content for cybersecurity risk, or Oh My Pi
	// holding the request because such a refusal earlier in the session still holds OpenAI's
	// routes. Oh My Pi sends it with retryable false and with whatever status a gateway relayed the
	// refusal under, a 503 among them. It cannot clear: each copy of the content sent to OpenAI
	// again adds a refusal to the organization's record, and the next prompt the daemon sends is
	// input from outside the session, which ends Oh My Pi's own hold
	// (packages/coding-agent/src/session/cyber-policy-closure.ts there). So it fails the claim
	// under CauseCyberPolicy, whatever its status (holdsTheIssue, providerErrorTurnEnded), on the
	// result of a prompt's turn and on the report of a turn that ran none alike, and no retry of
	// the daemon's own launches that claim's agent again (Claim.HeldForCyberPolicy).
	//
	// One send can still go before the claim fails. The report of a turn that ran no prompt of the
	// daemon's comes over HTTP from pi-legion and the turn's end over the worker stream, both from
	// the one agent_end, in no fixed order. Where the end arrives first and the claim holds a task
	// (one queued while that turn ran, an architect's notice above all), turnEnded sends it at once,
	// since no prompt is outstanding to wait for; so does a task delivered to the idle claim before
	// the report lands. That prompt ends Oh My Pi's hold and costs one more refusal, and its own
	// prompt result names cyber_policy, which fails the claim here: the race costs one send at most,
	// never a second (TestARaceBetweenTheTurnsEndAndItsReportCostsOneSendAtMost).
	codeCyberPolicy = claim.TurnCodeCyberPolicy
	// codeCostCap is the session reaching Oh My Pi's `session.maxCost`: Oh My Pi sent no request,
	// and every later one ends the same way until the cap is raised, which applies to the next
	// request (cost-gate.ts there). It can clear, and a send that is refused costs nothing, so it
	// goes on the ladder; it is named as the cause (providerErrorCause), so the controller's notice
	// says what a person has to raise.
	codeCostCap = "cost_cap"
)

// promptRetryDelay is the wait before the attempt-th send of a task a provider error took back,
// from attempt 1: 30 s, 60 s, 120 s, 240 s, 480 s, and 10 minutes from the sixth send on. There
// is no jitter — the ladder is one claim's own, not a herd's — and no attempt limit, so it
// flattens at the top rather than ending.
//
// The shift is clamped before it is taken, as internal/backoff clamps its own: five doublings of
// 30 seconds is already 16 minutes, past the cap, and a shift any wider than that leaves int64
// and comes back a negative wait, which arms the timer to fire at once and sends the task with no
// wait at all.
func promptRetryDelay(attempt int) time.Duration {
	return min(promptRetryFirst<<min(max(attempt-1, 0), 5), promptRetryMax)
}

// providerErrored is a turn that ran and ended in a provider error. The task is kept whatever the
// decision: it goes back unconfirmed under a new id, keeping its read mark, because the turn ran
// and the agent read it. No prompt budget is charged — chargePrompt retires and then fails a
// claim past Limits.PromptFailures, which would fail every claim at once in a provider outage,
// and R2 forbids an attempt limit — so the claim counts its own run of such turns instead, and a
// turn that ends with no error puts that run back to zero (promptResulted).
//
// What that turn's confirmation told goes back to untold with it (takeBackPending, moveTelling),
// so the re-sent task carries the abandoned session's sentence, the record of the last let-go, or
// both, again. A provider error is a model call that failed — often the turn's first, whose model
// never read the prompt — so the sentence may have reached nobody, and where an earlier call of
// the turn did read it, it is told twice rather than lost.
//
// The decision is made here, from this prompt's own result and nothing else
// (dispatch://INFRA-956/comment/4fa88c00-3313-4008-8eea-d7f95a04e24d): the status the provider
// answered, or Oh My Pi's class of the failure, either holds the issue (holdsTheIssue) or the task
// goes to the same session again on the ladder. Oh My Pi's Retryable flag is logged and read by
// nothing: the status and the class decide.
//
// The cause is named once here and handed to whoever writes it down, since one failed turn has
// one cause and the words are the same in the claim's failure, in the prompt the task goes
// behind and in the controller's notice.
func (m *Machine) providerErrored(ctx context.Context, r StreamPromptResult) error {
	cause, failed, err := m.providerErrorTurnEnded(ctx, providerErrorTurn{
		status: r.HTTPStatus, code: r.Code, model: r.Model,
		line:  "supervise: the turn this task's prompt ran ended in a provider error; the task is kept and sent again",
		attrs: []any{"delivery", r.DeliveryID, "retryable", r.Retryable, "provider", r.Provider, "code", r.Code},
		turn:  "this claim's turn",
		next:  fmt.Sprintf("the daemon keeps the task and sends it again every %s at most", promptRetryMax),
		// The prompt's turn ran and is over, and its result has ended the wait for results
		// (promptResulted), so nothing of the task is left outstanding.
		before: func(ctx context.Context) error { return m.takeBackPending(ctx, taskRead) },
	})
	if err != nil || failed {
		return err
	}
	return m.providerErrorClears(ctx, cause)
}

// unpromptedTurnEnded judges a refused turn that ran no prompt of the daemon's — a phase reminder
// the plugin sent, an Envoy message, a background job's result — from its turn-outcome report
// (turnOutcome), as providerErrored judges a prompt's turn from that prompt's result, and in the
// same run of provider-error turns (providerErrorTurnEnded); a clean report ends that run in
// turnOutcome, whoever ran the turn. It differs in one thing: no task is the turn's to keep or
// send again, since whatever the claim owes, this turn did not carry it, and the reminder that
// started it keeps its own pace (packages/pi-shared/src/phase-reminder.ts). So a refusal that can
// clear, or whose report carries no status, is counted and written down and nothing more, and the
// controller's notice says that nothing is sent again. The cause is built from the status and the
// class the report carries (Code, pi-legion's reading of the refusal with the host's own
// classifier), so a cyber-policy refusal fails the claim here as its prompt's result would, and
// the notice names no model, since a report carries none. Whether the report failed the claim is
// what the turn-outcome answer says (Machine.turnOutcomeFailed, HandleTurnOutcome).
func (m *Machine) unpromptedTurnEnded(ctx context.Context, r RequestTurnOutcome) error {
	_, failed, err := m.providerErrorTurnEnded(ctx, providerErrorTurn{
		status: r.HTTPStatus, code: r.Code,
		line:  "supervise: a turn that ran no prompt of the daemon's ended in a provider error",
		attrs: []any{"generation", r.Generation, "sequence", r.Sequence, "code", r.Code},
		turn:  "a turn this claim's agent ran with no prompt of the daemon's",
		next: "the last of them ran no prompt of the daemon's (a phase reminder, an Envoy message or a " +
			"background job's result), so the daemon sends no task again for it",
	})
	m.turnOutcomeFailed = failed
	if err != nil || failed {
		return err
	}
	return m.persist(ctx)
}

// providerErrorTurn is one turn that ended in a provider error, as providerErrorTurnEnded counts
// it: the status, Oh My Pi's class of the failure and the model the error named (each zero where
// it named none), the warning line's message and its own attributes, the failure sentence's words
// for the turn, the notice's words for what the daemon does now, and what runs before a failure,
// if anything. A turn-outcome report carries the class only for a cyber-policy refusal.
type providerErrorTurn struct {
	status int
	code   string
	model  string
	line   string
	attrs  []any
	turn   string
	next   string
	before func(context.Context) error
}

// providerErrorTurnEnded is what every turn that ended in a provider error does, whatever ran it
// (providerErrored, unpromptedTurnEnded), so that both kinds make the claim's one run: the cause
// built from the status and the class alone (providerErrorCause), one more turn in the run, the
// warning line, the controller's notice at providerErrorRunTold, then t.before, and, for an error
// that cannot clear, the claim's failure under the word holdsTheIssue answers for it, which holds
// the issue for its architect. It answers the cause, and whether it failed the claim; what a turn
// whose error can clear does next is its caller's.
func (m *Machine) providerErrorTurnEnded(ctx context.Context, t providerErrorTurn) (string, bool, error) {
	cause := providerErrorCause(t.status, t.code)
	m.claim.ProviderErrorRun++
	m.log.Warn(t.line, append(t.attrs, "httpStatus", t.status, "model", t.model, "providerErrorRun", m.claim.ProviderErrorRun)...)
	m.tellControllerOfProviderErrors(t.model, cause, t.next)
	if t.before != nil {
		if err := t.before(ctx); err != nil {
			return cause, false, err
		}
	}
	word, holds := holdsTheIssue(t.status, t.code)
	if !holds {
		return cause, false, nil
	}
	return cause, true, m.fail(ctx, word, "a provider error that cannot clear ended "+t.turn+": "+cause)
}

// HeldForCyberPolicy is whether the claim's last failure is OpenAI's cyber-policy refusal and no
// run has started since (CyberPolicyHolds).
func (c Claim) HeldForCyberPolicy() bool { return CyberPolicyHolds(c.State, c.FailureCause) }

// CyberPolicyHolds is whether a claim in state whose last failure is cause is held for OpenAI's
// cyber-policy refusal: the record stands on a failed claim, and on one an operator stopped after
// it failed, until a retry admits the claim (retry). Such a claim's agent is launched again by a
// retry someone decided alone — the held phase's retry (RequestRetry.Decided) or the operator's
// (RequestOperatorRetry) — and by no retry the daemon makes by itself (RetryHeld), and every
// retry admitted for it starts the agent in a new session (freshSession). It takes the two fields
// rather than a claim so that a reader holding no whole claim reads the same rule (record's
// RoleClaim, which the admission pass's repair reads).
func CyberPolicyHolds(state ClaimState, cause FailureCause) bool {
	return cause == CauseCyberPolicy && FailedOrRetired(state)
}

// RetryHeld is whether the machine refuses r for this claim with ErrHeldForCyberPolicy (retry): a
// retry the daemon makes by itself of a claim held for a cyber-policy refusal. A caller that must
// do work before the retry reads it to do none for a retry that launches no agent (the outbox's
// start of a retired claim, which provisions the workspace first).
func (c Claim) RetryHeld(r RequestRetry) bool { return !r.Decided && c.HeldForCyberPolicy() }

// KeeperRetries is whether the daemon's controller keeper is what retries this claim: the
// project's controller, unless it is held for a cyber-policy refusal (HeldForCyberPolicy), which
// the keeper retries no more, since its retry is one the daemon makes by itself. It is the one
// rule its three readers read: the keeper retries no failed or retired controller this is false
// for (the daemon's controllerKeeper), and the operator's two doors refuse every claim it is true
// for (operatorRetry, api's freshSession), so a held controller is the one controller they reach.
func (c Claim) KeeperRetries() bool { return c.Role == claim.RoleController && !c.HeldForCyberPolicy() }

// RetryCommand is the operator's command that retries token's claim, as every sentence that
// names it for a claim held for a cyber-policy refusal writes it: the machine's refusal
// (CyberPolicyRefusal), the controller keeper's line and the liveness sweep's (the daemon's
// controllerKeeper, watchController), and the workflow's worker-died notices. It needs no
// --fresh-session: the retry of such a claim starts it in a new session whoever asks
// (freshSession).
func RetryCommand(token claim.Token) string { return "legion claims retry --claim " + string(token) }

// CyberPolicyRefusal is the refusal of a retry the daemon makes by itself of this claim, held for
// OpenAI's cyber-policy refusal (retry, RetryHeld): ErrHeldForCyberPolicy, saying that a retry
// someone decides launches its agent again in a new session, with the operator's command
// (RetryCommand). The controller keeper, which asks the machine for no retry of such a controller,
// writes it as the reason of its one line.
func (c Claim) CyberPolicyRefusal() error {
	return &RefusedError{State: c.State, Request: "retry", Err: fmt.Errorf(
		"%w; a retry someone decides launches its agent again, in a new session, as an operator's `%s` does",
		ErrHeldForCyberPolicy, RetryCommand(c.Token))}
}

// providerErrorRunOver puts the claim's run of provider-error turns back to zero for a turn that
// ended with none, whatever ran it (promptResulted, turnOutcome), and logs line with
// attrs and the run that ended. It answers whether the run moved, which is whether the claim owes
// a write for it.
func (m *Machine) providerErrorRunOver(line string, attrs ...any) bool {
	if m.claim.ProviderErrorRun == 0 {
		return false
	}
	m.log.Info(line, append(attrs, "providerErrorRun", m.claim.ProviderErrorRun)...)
	m.claim.ProviderErrorRun = 0
	return true
}

// holdsTheIssue is whether an error of this HTTP status and this class fails the claim, so that
// the workflow holds the issue for its architect, and the ladder sends nothing again, and the word
// the claim fails under: a cyber-policy refusal (codeCyberPolicy), whatever its status, under
// CauseCyberPolicy, and a client error, 400 to 499, that is none of 401, 403, 408 and 429, under
// CauseProviderError. A malformed request is the case the second exists for. Every other error
// sends the task again:
//
//   - 5xx, 408 and 429 are the provider saying "not now", which the ladder waits out;
//   - 401 and 403 are a credential, which is the fleet's and not this claim's work
//     (dispatch://INFRA-956/comment/838bf35d-69dd-4fb2-a1b3-4b474b4525b3). Failing every claim
//     whose turn hits one is worse than sending the task again, and the notice the project's
//     controller is given at six such turns in a row is what tells a person
//     (tellControllerOfProviderErrors);
//   - no status at all is a turn that ends with nothing but a message, three kinds among them.
//     A content-filter refusal clears because the plugin has already moved the session down its
//     model chain, so the next prompt goes to another model. A turn whose provider key could not
//     be read (a `!command` apiKey that printed nothing, such as `!cat` of a token file not yet
//     mounted, or, from Oh My Pi 18.18.6-sami.20261119-162325 on, stored credentials that cannot
//     serve for a while) ends with the error of its last auto-retry, `retryable` and with no
//     status; the plugin reads it as a provider error and moves nothing, and the key, like a
//     401 or a 403, is the fleet's to restore, so the task goes again on the same model. A
//     session past its cost cap (codeCostCap) clears when a person raises the cap. A Claude Code
//     worker's bridge names a status only for a result Claude Code marked api_error
//     (internal/claudebridge's result), so its statusless turns go again here too.
//
// A cyber-policy refusal is the one class that decides alone. Sending it again on the ladder is
// what Oh My Pi's hold exists to stop, and the ladder cannot stop it itself: each send is a prompt
// from outside the session, which ends that hold, and the session's content goes to OpenAI again.
// So the claim fails under a word of its own (CauseCyberPolicy), and no retry of the daemon's own
// launches its agent again (Claim.HeldForCyberPolicy): the architect gets the issue, and the retry
// it or a person decides starts the agent in a new session (freshSession), which holds none of the
// refused content. What this decides is the turn whose record arrived: the one send a report of a
// turn with no prompt can race past is codeCyberPolicy's to state.
func holdsTheIssue(status int, code string) (FailureCause, bool) {
	if code == codeCyberPolicy {
		return CauseCyberPolicy, true
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return "", false
	}
	return CauseProviderError, status >= 400 && status <= 499
}

// providerErrorClears keeps the task on the claim and arms the next step of the ladder for it:
// the delivery counts one more send, carries the sentence the re-sent prompt is given
// (Delivery.ProviderErrorNote), and is written with the claim's run of provider-error turns in
// one transaction. Its timer's firing is what sends it (promptRetryDue); until then sendPending
// holds it, and a newer task queued meanwhile replaces it rather than waiting behind it (queue).
func (m *Machine) providerErrorClears(ctx context.Context, cause string) error {
	p := m.claim.Pending
	p.ProviderErrorRetries++
	p.ProviderErrorNote = providerErrorTask(cause)
	if err := m.saved(m.deps.Store.PutClaimAndDelivery(ctx, m.stored(), *p)); err != nil {
		return err
	}
	after := promptRetryDelay(p.ProviderErrorRetries)
	m.arm(TimerPromptRetry, after, p.ID)
	m.log.Info("supervise: this provider error can clear; the task goes to the same session again after a wait",
		"delivery", p.ID, "attempt", p.ProviderErrorRetries, "wait", after, "cause", cause)
	return nil
}

// promptRetryDue is the ladder's wait over: the task goes to the same session again. A claim
// still working the turn whose error armed this is handed nothing here, and the end of that turn
// sends it (turnEnded). What sendPending drops rather than sends, it drops here as well.
func promptRetryDue(m *Machine, ctx context.Context, _ Event) error {
	m.log.Info("supervise: the wait after a provider error is over; the task goes again",
		"delivery", pendingID(m.claim.Pending))
	return m.sendPending(ctx)
}

// providerErrorCause names what ended the turn, in words built from the status and Oh My Pi's
// class alone: a cyber-policy refusal or the session's cost cap by name, with the status when the
// refusal carried one, else the HTTP status the provider answered, else nothing more than that a
// provider failed. A refusal's own explanation is itself refused when it is sent back
// (dispatch://INFRA-956/artifact/cause-evidence-md), so the provider's text reaches neither the
// prompt this is written into nor the controller's notice. A content filter's refusal carries no
// status, so it is the last of these.
func providerErrorCause(status int, code string) string {
	switch code {
	case codeCyberPolicy:
		if status != 0 {
			return "OpenAI's cyber_policy refusal of this session's content (HTTP " + strconv.Itoa(status) + ")"
		}
		return "OpenAI's cyber_policy refusal of this session's content"
	case codeCostCap:
		return "the session's cost cap (Oh My Pi's session.maxCost, cost_cap)"
	}
	if status != 0 {
		return "a provider error (HTTP " + strconv.Itoa(status) + ")"
	}
	return "a provider error"
}

// providerErrorTask is the sentence a task taken back by a provider error is sent behind, as the
// relaunch's sentence is the one a task taken back from a turn its process could not finish is
// sent behind (relaunchSentence, interruptedTask). Another agent reads it, so it follows
// ASD-STE100: one instruction per sentence, and active voice.
func providerErrorTask(cause string) string {
	return "Your previous turn on this task stopped because of " + cause + ". Your work did not cause it. " +
		"The daemon sends you the task again. Read what that turn already did in your workspace and on the " +
		"issue's branch. Then continue the task."
}

// tellControllerOfProviderErrors tells the project's controller, once, when the claim's run of
// provider-error turns reaches providerErrorRunTold; a turn with no error puts the run back to
// zero, and only then can another notice go. It is the one exit from a run of such turns, and it
// is a notice rather than a hold: R2 forbids an attempt limit. model is the model the failed
// request went to, and is empty when the error named none. next is what the daemon does about the
// turn that reached the count, in the caller's words: a prompt's task is kept and sent again, and
// a turn that ran no prompt of the daemon's has no task to send (providerErrorTurnEnded).
func (m *Machine) tellControllerOfProviderErrors(model, cause, next string) {
	if m.claim.ProviderErrorRun != providerErrorRunTold || m.deps.ProviderErrorRunReached == nil {
		return
	}
	on := ""
	if model != "" {
		on = " on " + model
	}
	reason := fmt.Sprintf("%d turns in a row%s ended in %s; %s", m.claim.ProviderErrorRun, on, cause, next)
	m.log.Warn("supervise: a run of turns that ended in a provider error; the controller is told",
		"providerErrorRun", m.claim.ProviderErrorRun, "model", model, "reason", reason)
	m.deps.ProviderErrorRunReached(m.current(), reason)
}

// waitsForProviderError is whether the ladder's wait is armed for the delivery of this id.
// sendPending holds such a task until its timer fires; a task no wait names goes at once.
func (m *Machine) waitsForProviderError(delivery string) bool {
	a, armed := m.timers[TimerPromptRetry]
	return armed && a.deliveryID == delivery
}

// replaceable is whether a newer task takes this pending task's place rather than waiting behind
// it (queue). Two deliveries answer true. One is the task the ladder's wait stands for
// (waitsForProviderError), which the claim owes with nothing working it and with a wait that
// the next provider error only makes longer. R2 has a newer task replace it. The other is the
// daemon's own delivery of a relaunch's record (relaunchTask, queueRelaunch) while no prompt of
// it is waiting on a result (Machine.outstanding): it holds no work of the claim's, only the
// record's sentence, which the newer task carries in front of itself while the record is untold
// (Delivery.message). Once Oh My Pi has acknowledged a prompt of it, that prompt's turn is the
// session's next, and the newer task waits for it as for any task's.
//
// Every other pending task waits for its turn, the task a provider error took back on a claim
// the operator stopped among them: the ladder's wait went with the claim's timers (letGo), and
// the claim still owes that task, so the phase's restart task waits behind it until the turn
// the retry's fresh session takes on it retires it (R3, scripts/e2e/provider-error-retry.sh
// step 5). An update round's ask is never the newer task here: the daemon publishes it to the
// role's Envoy topic (internal/daemon's openPodUpdate) and queues no task for it. A task whose
// turn is running, or whose prompt is in flight, is work in flight and is never replaced either.
//
// The mark ends at the send it allows, so it does not outlive it: the timer that fires the
// ladder's wait is deleted before its action runs (machine.go's Handle). Without that, the
// window between the prompt's acknowledgement and its turn would answer true — the send is
// over (machine.go's fenceOutcome clears it), the task is not confirmed yet, and the wait for
// its turn is armed — so a newer task would take the place of a task already in the agent's
// hands: that task's agent_start would confirm the task that replaced it, and the turn's end
// would retire that one unsent, with the outbox row that gave it already recorded as delivered.
// The relaunch's delivery is held to the same rule by its outstanding prompt, which that
// acknowledgement records (acked) and which stands until Oh My Pi answers that prompt or the
// turn it ran is over for the machine (Machine.outstanding).
func (m *Machine) replaceable(p *Delivery) bool {
	if !p.ConfirmedAt.IsZero() || m.send != nil {
		return false
	}
	return m.waitsForProviderError(p.ID) || (relaunchTask(p.ID) && !m.awaitsResult())
}

// replaceOwedTask has a newer task take the place of a pending delivery a newer task replaces
// (replaceable) — the task a provider error took back, or the daemon's own unprompted delivery of
// a relaunch's record — and reports whether it did. Both places a newer task arrives at a claim
// that holds one run it: the task queue is given (queue), and the operator's task that runs a
// launch the pacing parked (runParkedLaunchNow). A pending task no newer one replaces is replaced
// by neither, and each caller answers that its own way, so this reports it rather than deciding
// it. The delivery it replaces is the one the claim holds (m.claim.Pending), which is the one
// either caller could mean, and a claim holding none replaces none.
//
// The replaced task's row ends as any other retirement ends it, and the question a restart armed
// for that task is answered in the same step (dropPending), so the task that takes its place is
// never taken for the running turn's.
//
// Nothing is logged here: what the two callers have to say of a replacement differs, so each
// writes its own line when this reports one.
func (m *Machine) replaceOwedTask(ctx context.Context) (bool, error) {
	pending := m.claim.Pending
	if pending == nil || !m.replaceable(pending) {
		return false, nil
	}
	if err := m.dropPending(ctx); err != nil {
		return false, err
	}
	return true, nil
}

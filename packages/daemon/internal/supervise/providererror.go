package supervise

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A turn that ends in a provider error does not finish its task (dispatch://INFRA-956 R2). The
// work is still owed, and a claim left idle holding nothing could be given it back only by an
// edit of the daemon's store. The task is kept here, under its number, and its value is written
// again for the same session after a wait that grows to a limit, with no attempt limit and no new
// session.
//
// Two bounds and one count make that up, and this file holds all three.
const (
	// promptRetryFirst and promptRetryMax bound the ladder: 30 seconds before the first re-send,
	// doubling to at most 10 minutes (promptRetryDelay). internal/backoff is not it — it draws
	// from [step/2, step] and caps at 32 times its base, which this ladder's exactness forbids.
	promptRetryFirst = 30 * time.Second
	promptRetryMax   = 10 * time.Minute
	// providerErrorRunTold is the run of provider-error turns at which the run is handed, once, to
	// Deps.ProviderErrorRunReached. There is no hold and no attempt limit behind it: a chain's
	// bottom refusing deterministically costs one request every promptRetryMax.
	providerErrorRunTold = 6
)

// The class of a refused turn's report (RequestTurnOutcome.Code) that decides something here. Oh
// My Pi declares it on `RpcPromptError.code` (packages/coding-agent/src/modes/rpc/rpc-types.ts of
// cybermonklabs/oh-my-pi at the pinned tag); pi-legion classifies a refused turn with the host's
// own `isCyberPolicyRefusal`, the classifier that sets that code (packages/pi-legion/src/turn-outcome.ts).
const (
	// codeCyberPolicy is OpenAI refusing the session's content for cybersecurity risk, or Oh My Pi
	// holding the request because such a refusal earlier in the session still holds OpenAI's
	// routes. Oh My Pi gives it with retryable false and with whatever status a gateway relayed the
	// refusal under, a 503 among them. It cannot clear: each copy of the content sent to OpenAI
	// again adds a refusal to the organization's record, and the next task the worker takes is
	// input from outside the session, which ends Oh My Pi's own hold
	// (packages/coding-agent/src/session/cyber-policy-closure.ts there). So it fails the claim
	// under CauseCyberPolicy, whatever its status (classifyProviderError, providerErrorTurnEnded),
	// on the report of a take's turn, the take taken back first (takeBackAndFail), and on the
	// report of any other turn alike, and no retry of the daemon's own launches that claim's agent
	// again (Claim.HeldForCyberPolicy).
	//
	// One take can still start a turn before the claim fails. pi-legion's take hears the end of a
	// turn before that turn's report goes (its agent_end entry, extensions/legion.ts), and asks
	// for a task the claim was given while the turn ran, or sends a text it held for the turn's
	// end, at once; the report comes over HTTP after that. Where the take reaches the daemon
	// first, it is accepted, and its turn ends Oh My Pi's hold and costs one more refusal. That
	// turn's own report names the take's number and cyber_policy, which takes the task back and
	// fails the claim here, if the first report has not failed it already: the race costs one take
	// at most, never a second (TestARaceBetweenTheTurnsEndAndItsReportCostsOneTakeAtMost).
	codeCyberPolicy = claim.TurnCodeCyberPolicy
)

// promptRetryDelay is the wait before the value of a task a provider error took back is written
// again, for the attempt-th time, from attempt 1: 30 s, 60 s, 120 s, 240 s, 480 s, and 10 minutes
// from the sixth on. There is no jitter — the ladder is one claim's own, not a herd's — and no
// attempt limit, so it flattens at the top rather than ending.
//
// The shift is clamped before it is taken, as internal/backoff clamps its own: five doublings of
// 30 seconds is already 16 minutes, past the cap, and a shift any wider than that leaves int64
// and comes back a negative wait, which arms the timer to fire at once and writes the task's value
// again with no wait at all.
func promptRetryDelay(attempt int) time.Duration {
	return min(promptRetryFirst<<min(max(attempt-1, 0), 5), promptRetryMax)
}

// providerErrored is a task's turn reported over with a provider error (reportedTask). The task
// is kept whatever the decision: it is taken back under its number, its take gone (ladderStep) and
// the claim's TaskTaken kept, because the turn ran and the agent read it. No prompt budget is
// charged — chargePrompt retires and then fails a claim past Limits.PromptFailures, which would
// fail every claim at once in a provider outage, and R2 forbids an attempt limit — so the claim
// counts its own run of such turns instead, and a turn that ends with no error puts that run back
// to zero (turnOutcome).
//
// What the take told goes back to untold with it (ladderStep, giveBackTake), so the task's next
// accepted take carries the abandoned session's sentence, the record of the last let-go, or both,
// again. A provider error is a model call that failed — often the turn's first, whose model never
// read the take's text — so the sentence may have reached nobody, and where an earlier call of
// the turn did read it, it is told twice rather than lost. The claim's handshake keeps its
// confirmation of the take's entry: the turn started on the take's text.
//
// The decision is made here, from this turn's own report and nothing else
// (dispatch://INFRA-956/comment/4fa88c00-3313-4008-8eea-d7f95a04e24d): the status the provider
// answered, or the class the producer gave the failure (RequestTurnOutcome.Code), either holds
// the issue (classifyProviderError) or the task is given to the same session again on the ladder.
// The agent's Retryable flag is logged and read by nothing: the status and the class decide.
//
// The cause is named once (providerErrorTurnEnded) and handed to whoever writes it down, since one
// failed turn has one cause and the words are the same in the claim's failure, in the note the
// next take carries and in the notice at providerErrorRunTold. With no pending task it does
// nothing: there is no take for the turn to have run on (reportOfTake).
//
// The turn counts in the claim's one run of provider-error turns beside every other refused turn,
// the turns that were not the take's among them (unpromptedTurnEnded), through the one step both
// make (providerErrorTurnEnded): one more turn in the run, one warning, and the notice at the
// run's providerErrorRunTold-th turn (tellOfProviderErrors), whose words say the daemon keeps the
// task and writes it again. An error that cannot clear — a status that cannot clear, or OpenAI's
// cyber-policy refusal whatever its status — fails the claim at once under the word
// classifyProviderError gives it, the take taken back first (takeBackAndFail), and the
// turn-outcome answer says this report failed it (Machine.turnOutcomeFailed); any other puts the
// task on the ladder (ladderStep). The status is the report's (RequestTurnOutcome.status): its
// ProviderError's, or its HTTPStatus where that names none.
func (m *Machine) providerErrored(ctx context.Context, r RequestTurnOutcome) error {
	p := m.claim.Pending
	if p == nil || r.ProviderError == nil {
		return nil
	}
	pe := r.ProviderError
	cause, failed, err := m.providerErrorTurnEnded(ctx, providerErrorTurn{
		status: r.status(), code: r.Code, model: pe.Model,
		line:  "supervise: the turn of this task ended in a provider error; the task is kept and given again",
		attrs: []any{"task", p.Number, "retryable", pe.Retryable, "provider", pe.Provider, "code", r.Code},
		next:  fmt.Sprintf("the daemon keeps the task and writes it again every %s at most", promptRetryMax),
		fail:  m.takeBackAndFail,
	})
	if err != nil || failed {
		return err
	}
	return m.ladderStep(ctx, cause)
}

// unpromptedTurnEnded judges a refused turn that was not the take's — a phase reminder the plugin
// sent, an Envoy message, a background job's result — from its turn-outcome report
// (reportedTask), as providerErrored judges the take's turn from the report that names its
// number, and in the same run of provider-error turns (providerErrorTurnEnded); a clean report
// ends that run in turnOutcome, whoever ran the turn. Such a report names no task, or a number no
// take of the claim holds, or is marked unprompted, which reads as naming none (turnOutcome), and
// is of the generation the claim runs now. It differs in one thing: no task is the turn's to keep
// or write again, since whatever the claim owes, this turn did not carry it, and the reminder
// that started it keeps its own pace (packages/pi-shared/src/phase-reminder.ts). So a refusal
// that can clear, or whose report carries no status, is counted and written down and nothing
// more, and the notice at providerErrorRunTold says that nothing is sent again. The cause is
// built from the status (RequestTurnOutcome.status) and the class the report carries (Code,
// pi-legion's reading of the refusal with the host's own classifier), so a cyber-policy refusal
// fails the claim here as it does on the report of a take's turn, and the notice names the model
// where the report's provider error names one, as a current plugin's does. Whether the report
// failed the claim is what the turn-outcome answer says (Machine.turnOutcomeFailed,
// HandleTurnOutcome).
func (m *Machine) unpromptedTurnEnded(ctx context.Context, r RequestTurnOutcome) error {
	model := ""
	if r.ProviderError != nil {
		model = r.ProviderError.Model
	}
	_, failed, err := m.providerErrorTurnEnded(ctx, providerErrorTurn{
		status: r.status(), code: r.Code, model: model,
		line:  "supervise: a turn that ran none of the daemon's tasks ended in a provider error",
		attrs: []any{"generation", r.Generation, "sequence", r.Sequence, "task", r.Task, "code", r.Code},
		next: "the last of them ran none of the daemon's tasks (a phase reminder, an Envoy message or a " +
			"background job's result), so the daemon writes no task again for it",
		fail: func(ctx context.Context, class providerErrorClass) error {
			return m.fail(ctx, class.fails, "a provider error that cannot clear ended a turn of this claim's agent that ran none of the daemon's tasks: "+class.cause)
		},
	})
	if err != nil || failed {
		return err
	}
	return m.persist(ctx)
}

// providerErrorTurn is one turn that ended in a provider error, as providerErrorTurnEnded counts
// it: the status, the class the producer gave the failure and the model the error named (each
// zero where it named none), the warning line's message and its own attributes, the notice's words
// for what the daemon does now, and how an error that cannot clear fails the claim, given its
// class. A turn-outcome report carries the class only for a cyber-policy refusal.
type providerErrorTurn struct {
	status int
	code   string
	model  string
	line   string
	attrs  []any
	next   string
	fail   func(ctx context.Context, class providerErrorClass) error
}

// providerErrorTurnEnded is what every turn that ended in a provider error does, whatever ran it
// (providerErrored, unpromptedTurnEnded), so that both kinds make the claim's one run: the error
// classified once from the status and the class alone (classifyProviderError), one more turn in
// the run, the warning line, the notice at providerErrorRunTold (tellOfProviderErrors), and, for
// an error that cannot clear, t.fail, the claim's failure under the word that classification
// gives, which holds the issue for its architect. It answers the cause, and whether it failed the
// claim, which it marks for the turn-outcome answer (Machine.turnOutcomeFailed); what a turn whose
// error can clear does next is its caller's.
func (m *Machine) providerErrorTurnEnded(ctx context.Context, t providerErrorTurn) (string, bool, error) {
	class := classifyProviderError(t.status, t.code)
	m.claim.ProviderErrorRun++
	m.log.Warn(t.line, append(t.attrs, "httpStatus", t.status, "model", t.model, "providerErrorRun", m.claim.ProviderErrorRun)...)
	m.tellOfProviderErrors(t.model, class.cause, t.next)
	if class.fails == "" {
		return class.cause, false, nil
	}
	m.turnOutcomeFailed = true
	return class.cause, true, t.fail(ctx, class)
}

// takeBackAndFail is the step a provider error that cannot clear (classifyProviderError) makes
// over the take whose turn it ended, from the report's own decision (providerErrored, through
// providerErrorTurnEnded) and from a daemon start that restored that report's take-back unwritten
// (takeToLadder): the take goes back first, since its turn ran and is over, and then the claim
// fails under the word the class gives — CauseProviderError for a status that cannot clear,
// CauseCyberPolicy for OpenAI's cyber-policy refusal — which holds the issue for its architect.
// The failure is made whether or not the take-back's write landed: the take-back stands in
// memory, its write owed, and a claim the provider refused for good is not left running for a
// refused write.
func (m *Machine) takeBackAndFail(ctx context.Context, class providerErrorClass) error {
	err := m.takeBack(ctx, takenBackProviderError)
	return errors.Join(err, m.fail(ctx, class.fails, "a provider error that cannot clear ended this claim's turn: "+class.cause))
}

// HeldForCyberPolicy is whether the claim's last failure is OpenAI's cyber-policy refusal and no
// run has started since (CyberPolicyHolds).
func (c Claim) HeldForCyberPolicy() bool { return CyberPolicyHolds(c.State, c.FailureCause) }

// CyberPolicyHolds is whether a claim in state whose last failure is cause is held for OpenAI's
// cyber-policy refusal: the record stands on a failed claim, and on one an operator stopped after
// it failed, until a retry admits the claim (retry). Such a claim's agent is launched again by a
// retry someone decided alone — the held phase's retry (RequestRetry.Decided) or the one the
// operator or the controller asks (RequestAskedRetry) — and by no retry the daemon makes by itself
// (RetryHeld), and every retry admitted for it starts the agent in a new session (freshSession).
// It takes the two fields rather than a claim so that a reader holding no whole claim reads the
// same rule (record's RoleClaim, which the admission pass's repair reads).
func CyberPolicyHolds(state ClaimState, cause FailureCause) bool {
	return cause == CauseCyberPolicy && FailedOrRetired(state)
}

// DecodeCause is a claim's cause as a store row holds it — claims.failure_cause or
// claims.pending_cause — read beside that row's claims.held_for (migration
// 20261125223008_claim_held_for): CauseCyberPolicy where the column holds `provider_error` and
// held_for holds `cyber_policy`, and the column's word as it stands otherwise. The release before
// that migration knows no `cyber_policy`, so the claim store writes the word that release reads
// in the column and the hold in held_for (internal/store's storedCause and heldFor). The column's
// word decides too, not held_for alone: after a rollback that release rewrites the cause columns
// and leaves held_for as it was, so a cause it cleared or wrote under another word reads as that
// release wrote it. It is the one decoder of the pair: the claim store's read of a claim and the
// record store's read of a role's claim (record's readRoleClaims), from which admission's repair
// decides whether to start the role, both read through it, so the two cannot disagree on whether
// one claim is held (CyberPolicyHolds).
func DecodeCause(column, heldFor string) FailureCause {
	if heldFor == string(CauseCyberPolicy) && column == string(CauseProviderError) {
		return CauseCyberPolicy
	}
	return FailureCause(column)
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
// for (askedRetry, api's freshSession), so a held controller is the one controller they reach.
func (c Claim) KeeperRetries() bool { return c.Role == claim.RoleController && !c.HeldForCyberPolicy() }

// RetryCommand is the operator's command that retries token's claim, as every operator surface
// that names it for a claim held for a cyber-policy refusal writes it: the machine's refusal
// (CyberPolicyRefusal), which the outbox logs where it refuses a start, the controller keeper's
// line and the liveness sweep's (the daemon's controllerKeeper, watchController), and the
// compaction's refusal of a held controller (api's startsAgainOf). No notice to an agent names it
// (workflow's noStartOfItsOwn). It needs no --fresh-session: the retry of such a claim starts its
// agent in a new session whoever asks (freshSession).
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
// ended with none, whoever ran it (turnOutcome), and logs line with attrs and the run that ended.
// It answers whether the run moved, which is whether the claim owes a write for it.
func (m *Machine) providerErrorRunOver(line string, attrs ...any) bool {
	if m.claim.ProviderErrorRun == 0 {
		return false
	}
	m.log.Info(line, append(attrs, "providerErrorRun", m.claim.ProviderErrorRun)...)
	m.claim.ProviderErrorRun = 0
	return true
}

// takeToLadder is what a provider error's turn does to the task whose take it ran on, once the
// claim's run of such turns counts it, for a claim a daemon start restored over a provider
// error's report whose take-back the store never took (settle, Machine.ladderOwed): the error,
// classified from the report's status and class as the report's own decision classified it
// (classifyProviderError, TurnOutcome.ProviderErrorCode), either cannot clear, taking the take
// back and failing the claim (takeBackAndFail), or puts the task on the ladder, which takes the
// take back with its step (ladderStep). The report's own decision makes the same two steps
// (providerErrored), through providerErrorTurnEnded, which counted the turn and told the
// controller there.
//
// A take whose report's finish the store refused (takePhase, waitFinish) goes to no ladder: the
// report that came first decides, so a later report naming its number with a provider error
// finishes the task by that first report (makeOwedFinish), for a status the ladder would wait out
// as for one that holds the issue (takeBack). Taken to the ladder, the task would be given again
// and run twice.
func (m *Machine) takeToLadder(ctx context.Context, status int, code string) error {
	class := classifyProviderError(status, code)
	if class.fails != "" {
		return m.takeBackAndFail(ctx, class)
	}
	return m.ladderStep(ctx, class.cause)
}

// ladderStep takes the take back and arms the ladder's next step for a provider error that can
// clear (takeToLadder, providerErrored).
//
// On the ladder the take-back is written with the ladder's step and the claim's run of
// provider-error turns in one store call (takeBackInMemory, providerErrorClears), so they land
// together or not at all: a take-back written alone, with the step's write refused after it,
// would leave a task a restarted daemon reads as taken back with no step — owing nothing, it
// would be given again with no note and its sends uncounted — and the report that decided it is
// applied, so its worker's report sent again is a repeat that decides nothing.
//
// Each step holds in memory whether or not its write lands, as the take-back does: a take-back
// whose write the store refused is still a take-back, which the next decision writes (settle,
// repair), so that refusal is no reason to leave the task without its ladder step and wait —
// with no wait, that next decision's offer would give the task again at once (offerPending) — or
// to leave running a claim the provider refused for good. A daemon that starts before any of it
// is written does it from the record (Claim.owedReport, Machine.ladderOwed).
func (m *Machine) ladderStep(ctx context.Context, cause string) error {
	if owed, err := m.makeOwedFinish(ctx); owed {
		return err
	}
	m.takeBackInMemory(takenBackProviderError)
	return m.providerErrorClears(ctx, cause)
}

// ladderOwedLine is the line of a claim a daemon start restored over a provider error's report
// whose take-back the store never took, at Warn, logged as its first decision makes that
// take-back (settle, Machine.ladderOwed): the report's own decision wrote nothing of it.
const ladderOwedLine = "supervise: a provider error's report was applied before this daemon started and its take-back never written; the task goes to the ladder now"

// providerErrorClass is one provider error as classifyProviderError decides it: cause, the words
// that name what ended the turn, and fails, the word the claim fails under for an error that
// cannot clear, so that the workflow holds the issue for its architect and the ladder writes
// nothing again, and "" for one that can.
type providerErrorClass struct {
	cause string
	fails FailureCause
}

// classifyProviderError decides one provider error, once, from its HTTP status and the class the
// producer gave it alone: the words that name it and whether it fails the claim. A new class of
// error is added here and nowhere else.
//
// The words are a cyber-policy refusal by name, with the status when the refusal carried one,
// else the HTTP status the provider answered, else nothing more than that a provider failed. A
// refusal's own explanation is itself refused when it is sent back
// (dispatch://INFRA-956/artifact/cause-evidence-md), so the provider's text reaches neither the
// note the next take carries nor the notice at providerErrorRunTold. A content filter's refusal carries no
// status, so it is the last of these.
//
// The error fails the claim for a cyber-policy refusal (codeCyberPolicy), whatever its status,
// under CauseCyberPolicy, and for a client error, 400 to 499, that is none of 401, 403, 408 and
// 429, under CauseProviderError. A malformed request is the case the second exists for. Every other
// error writes the task's value again for the worker to take:
//
//   - 5xx, 408 and 429 are the provider saying "not now", which the ladder waits out;
//   - 401 and 403 are a credential, which is the fleet's and not this claim's work
//     (dispatch://INFRA-956/comment/838bf35d-69dd-4fb2-a1b3-4b474b4525b3). Failing every claim
//     whose turn hits one is worse than writing the task's value again: the notice at
//     providerErrorRunTold (tellOfProviderErrors) tells a person at six such turns in a row;
//   - no status at all is a turn that ends with nothing but a message, two kinds among them.
//     A content-filter refusal clears because the plugin has already moved the session down its
//     model chain, so the next take's turn goes to another model. A turn whose provider key could
//     not be read (a `!command` apiKey that printed nothing, such as `!cat` of a token file not
//     yet mounted, or, from Oh My Pi 18.18.6-sami.20261119-162325 on, stored credentials that
//     cannot serve for a while) ends with the error of its last auto-retry, `retryable` and with
//     no status; the plugin reads it as a provider error and moves nothing, and the key, like a
//     401 or a 403, is the fleet's to restore, so the task goes again on the same model. A
//     Claude Code worker's bridge names a status only for a result Claude Code marked api_error
//     (internal/claudebridge's result), so its statusless turns go again here too.
//
// A cyber-policy refusal is the one class that decides alone. Giving the task again on the ladder
// is what Oh My Pi's hold exists to stop, and the ladder cannot stop it itself: each take starts a
// turn from input outside the session, which ends that hold, and the session's content goes to
// OpenAI again. So the claim fails under a word of its own (CauseCyberPolicy), and no retry of the
// daemon's own launches its agent again (Claim.HeldForCyberPolicy): the architect gets the issue,
// and the retry it or a person decides starts the agent in a new session (freshSession), which
// holds none of the refused content. What this decides is the turn whose report arrived: the one
// take that can race past that report is codeCyberPolicy's to state.
func classifyProviderError(status int, code string) providerErrorClass {
	withStatus := func(what string) string {
		if status == 0 {
			return what
		}
		return what + " (HTTP " + strconv.Itoa(status) + ")"
	}
	if code == codeCyberPolicy {
		return providerErrorClass{cause: withStatus("OpenAI's cyber_policy refusal of this session's content"), fails: CauseCyberPolicy}
	}
	class := providerErrorClass{cause: withStatus("a provider error")}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return class
	}
	if status >= 400 && status <= 499 {
		class.fails = CauseProviderError
	}
	return class
}

// providerErrorClears keeps the task on the claim and arms the next step of the ladder for it:
// the delivery counts one more attempt, carries the sentence the next accepted take of it is
// given (Delivery.ProviderErrorNote), and is written with the take-back that came before it and
// the claim's run of provider-error turns in one store call (ladderStep). Its timer's firing is
// what writes the value again (promptRetryDue); until then a take of it is refused (claimHold),
// and a newer task queued meanwhile replaces it rather than waiting behind it (queue). The step
// and its wait stand whether or not the write lands: a write the store refused leaves the
// delivery unsaved and the claim write owed, and the next decision makes it (settle, repair) —
// the wait's own firing, at the latest.
func (m *Machine) providerErrorClears(ctx context.Context, cause string) error {
	p := m.claim.Pending
	p.ProviderErrorRetries++
	p.ProviderErrorNote = providerErrorTask(cause)
	err := m.putClaimAndPendingOwed(ctx)
	after := promptRetryDelay(p.ProviderErrorRetries)
	m.arm(TimerPromptRetry, after, p.ID)
	m.log.Info("supervise: this provider error can clear; the task goes to the same session again after a wait",
		"delivery", p.ID, "attempt", p.ProviderErrorRetries, "wait", after, "cause", cause)
	return err
}

// promptRetryDue is the ladder's wait over: the task's value is written again for the same session
// (offerPending), where its take had been accepted or refused at the latest revision. A claim
// still working the turn whose error armed this writes nothing here, and the end of that turn
// offers it (turnEnded). What a take would drop, it drops here as well.
func promptRetryDue(m *Machine, ctx context.Context, _ Event) error {
	m.log.Info("supervise: the wait after a provider error is over; the task is given again",
		"task", pendingNumber(m.claim.Pending))
	return m.offerPending(ctx)
}

// providerErrorTask is the sentence the next accepted take of a task a provider error took back
// carries, as the relaunch's sentence is the one the next accepted take of a task taken back from
// a turn its process could not finish carries (relaunchSentence, interruptedTask). Another agent
// reads it, so it follows ASD-STE100: one instruction per sentence, and active voice.
func providerErrorTask(cause string) string {
	return "Your previous turn on this task stopped because of " + cause + ". Your work did not cause it. " +
		"The daemon sends you the task again. Read what that turn already did in your workspace and on the " +
		"issue's branch. Then continue the task."
}

// tellOfProviderErrors hands the claim's run of provider-error turns to
// Deps.ProviderErrorRunReached, once, when it reaches providerErrorRunTold. The run goes on past
// it, and another notice can go only after the run is back at zero (Claim.ProviderErrorRun says
// what puts it there). Who is told is that callback's to decide (internal/daemon's
// providerErrorRunReached). It is a notice rather than a hold, and it ends no run: R2 forbids an
// attempt limit. model is the model the failed request went to, and is empty when the error named
// none. next is what the daemon does about the turn that reached the count, in the caller's words:
// the take's task is kept and written again, and a turn that was not the take's has no task to
// write (providerErrorTurnEnded).
func (m *Machine) tellOfProviderErrors(model, cause, next string) {
	if m.claim.ProviderErrorRun != providerErrorRunTold || m.deps.ProviderErrorRunReached == nil {
		return
	}
	on := ""
	if model != "" {
		on = " on " + model
	}
	reason := fmt.Sprintf("%d turns in a row%s ended in %s; %s", m.claim.ProviderErrorRun, on, cause, next)
	m.log.Warn("supervise: a run of turns that ended in a provider error; it is handed to ProviderErrorRunReached",
		"providerErrorRun", m.claim.ProviderErrorRun, "model", model, "reason", reason)
	m.deps.ProviderErrorRunReached(m.current(), reason)
}

// waitsForProviderError is whether the ladder's wait is armed for the delivery of this id. A take
// of such a task is refused until its timer fires; a task no wait names is taken at once.
func (m *Machine) waitsForProviderError(delivery string) bool {
	a, armed := m.timers[TimerPromptRetry]
	return armed && a.deliveryID == delivery
}

// replaceable is whether a newer task takes this pending task's place rather than waiting behind
// it (queue). Two deliveries answer true while no take of them is accepted. One is the task the
// ladder's wait stands for (waitsForProviderError), which the claim owes with nothing working it
// and with a wait that the next provider error only makes longer. R2 has a newer task replace it.
// The other is the daemon's own delivery of a relaunch's record (Delivery.Relaunch,
// queueRelaunch): it holds no work of the claim's, only the record's sentence, which the newer
// task carries in front of itself while the record is untold (Delivery.message).
//
// Every other pending task waits for its turn, the task a provider error took back on a claim
// the operator stopped among them: the ladder's wait went with the claim's timers (letGo), and
// the claim still owes that task, so the phase's restart task waits behind it until the take the
// retry's fresh session makes of it finishes it (R3, scripts/e2e/provider-error-retry.sh step 5).
// An update round's ask is never the newer task here: the daemon publishes it to the role's Envoy
// topic (internal/daemon's openPodUpdate) and queues no task for it. A task a take holds is work
// in flight and is never replaced either, the relaunch's delivery among them: the turn of its
// accepted take is the session's next, and a newer task waits for it as for any task's.
func (m *Machine) replaceable(p *Delivery) bool {
	return !p.taken() && (m.waitsForProviderError(p.ID) || p.Relaunch())
}

// replaceOwedTask has a newer task take the place of a pending delivery a newer task replaces
// (replaceable) — the task a provider error took back, or the daemon's own untaken delivery of
// a relaunch's record — and reports whether it did. Both places a newer task arrives at a claim
// that holds one run it: the task queue is given (queue), and the operator's task that runs a
// launch the pacing parked (runParkedLaunchNow). A pending task no newer one replaces is replaced
// by neither, and each caller answers that its own way, so this reports it rather than deciding
// it. The delivery it replaces is the one the claim holds (m.claim.Pending), which is the one
// either caller could mean, and a claim holding none replaces none.
//
// The replaced task's row ends as any other retirement ends it (retirePending), and the value the
// newer task's queue writes is what the worker is handed next.
//
// Nothing is logged here: what the two callers have to say of a replacement differs, so each
// writes its own line when this reports one.
func (m *Machine) replaceOwedTask(ctx context.Context) (bool, error) {
	pending := m.claim.Pending
	if pending == nil || !m.replaceable(pending) {
		return false, nil
	}
	if err := m.retirePending(ctx); err != nil {
		return false, err
	}
	return true, nil
}

package claim

import (
	"fmt"
	"net/http"
	"time"
)

// RegisterRequest is the first call an agent makes: the Oh My Pi plugin reads the boot token off
// the 0600 file the daemon named on its pane and posts it with the session it actually became.
// `OmpSessionFile` is the transcript path that session persisted — the one value `--resume`
// takes, so a relaunch of this claim resumes this agent and no other. `PluginContract` is the
// plugin's `legion.daemonApiVersion`; a daemon that speaks another one refuses the boot.
type RegisterRequest struct {
	BootToken      string `json:"bootToken"`
	SessionID      string `json:"sessionId"`
	OmpSessionFile string `json:"ompSessionFile"`
	AgentID        string `json:"agentId"`
	PluginContract int    `json:"pluginContract"`
}

// RegisterResponse is what the agent learns about itself: the claim it holds, where it holds it,
// the secret its later calls authenticate with, the task agents its role's prompts dispatch,
// which its capability report measures (LEGION-663), and the session it replaces, if any. The
// secret is minted per registration and the capability hash is persisted before the response is
// written, so a daemon that restarts one instruction later still recognises this agent. A session
// that registers with the controller capability holds no claim and gets
// api.ControllerRegisterResponse instead.
//
// Task and Finished are what POST /legion/v1/claims/task answers (TaskResponse), read once the
// registration is recorded, so a worker that registers holds its claim's value before its ready
// and takes the task as soon as its host is idle (dispatch://INFRA-2811).
type RegisterResponse struct {
	ClaimToken Token  `json:"claimToken"`
	Tree       string `json:"tree"`
	Issue      string `json:"issue"`
	Role       Role   `json:"role"`
	Generation uint64 `json:"generation"`
	Secret     string `json:"secret"`
	// PromptAgents is the sorted names of every task agent the role's prompts dispatch; never
	// null on the wire, so a role whose prompts name none answers an empty list.
	PromptAgents []string `json:"promptAgents"`
	// Task is the claim's value as the task bucket holds it now, null where it holds none.
	Task *TaskValue `json:"task"`
	// Finished is the number of the last task the claim finished, 0 for none.
	Finished uint64 `json:"finished"`
	// Replaces is the session this registration's session replaces: the session the claim
	// abandoned when it last forgot one, where that is not the session registering now and no
	// operator's release has freed it. The session names it as `previous_session_id` in its Envoy
	// role claim, so the Envoy listener hands it the unread messages of the session it replaces.
	// Absent when the claim replaces none.
	Replaces string `json:"replaces,omitempty"`
}

// ReadyRequest says the agent has finished booting and can be prompted. It is a separate call
// from the registration because registration is the plugin's first act and the ready comes once
// the plugin holds the registration's task value and has recorded the claim its tools act for;
// the read of the claim's task value and the role claim on Envoy follow the ready.
// Capabilities is a report a claimed session sends after its boot, from the interval it runs
// while it is idle (LEGION-663, INFRA-927); a boot's ready carries none, a regain of the role
// sends no ready (it reads claims/task), and the controller sends no report either.
type ReadyRequest struct {
	ClaimToken   Token             `json:"claimToken"`
	SessionID    string            `json:"sessionId"`
	Secret       string            `json:"secret"`
	Generation   uint64            `json:"generation"`
	Capabilities *CapabilityReport `json:"capabilities,omitempty"`
}

// CapabilityReport is what a session measured of the live capability rows (LEGION-663):
// when it measured, how long the measuring took, and one row per capability it checked. The
// daemon normalises the rows (capabilities.Normalize) before it keeps them.
type CapabilityReport struct {
	MeasuredAt time.Time       `json:"measuredAt"`
	ElapsedMs  int             `json:"elapsedMs"`
	Rows       []CapabilityRow `json:"rows"`
}

// CapabilityRow is one live row as the session measured it: the capability's name
// (capabilities.Name), whether the check passed, and the fact the check found either way.
type CapabilityRow struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// ReadyForUpdateRequest is the worker's answer to its update round's ask: it has pushed
// everything it wants kept, and the process holding this claim may now be replaced. The daemon
// opens an update round over a pod whose configuration has drifted, and asks every claim of the
// round, on its own role topic, to do the steps of its update message (supervise.UpdateTask) and
// answer here. The round replaces the pod once every claim it asks has answered, no turn runs, no
// take whose turn ended still owes its report and no claim's activity report holds it
// (internal/daemon's roundEnd.holding), or at its bound (pod_update_grace_minutes) whatever the
// claims answered, so an answer only brings the replacement forward. A role's own push is what
// keeps its work: the disk the replacement deletes takes all work not pushed before the pod goes,
// which is why every step that pushes work comes before the answer in the round's ask
// (supervise.UpdateTask). `Round` is the round the worker
// answers for, as the ask it read named it: an answer counts only for the round whose ask the
// worker has in front of it, so the daemon refuses one that names any other round
// (supervise.ErrForeignUpdateRound). `Head` is the commit the worker
// reports as the head of its issue branch at `origin`, empty when the branch is not there (a
// planner step that runs before the branch's first push).
// The daemon records the worker's word and verifies nothing, exactly as
// `api.HandoffCompleteRequest.Commit` is the worker's report of the commit it completed at.
type ReadyForUpdateRequest struct {
	ClaimToken Token  `json:"claimToken"`
	SessionID  string `json:"sessionId"`
	Secret     string `json:"secret"`
	Generation uint64 `json:"generation"`
	Round      string `json:"round"`
	Head       string `json:"head,omitempty"`
}

// ExitRequest is the agent reporting its own end, with the reason it ended. An exit the daemon
// did not ask for is still a fact about the claim, which is why the reason travels with it.
type ExitRequest struct {
	ClaimToken Token  `json:"claimToken"`
	SessionID  string `json:"sessionId"`
	Secret     string `json:"secret"`
	Generation uint64 `json:"generation"`
	Reason     string `json:"reason"`
}

// Turn outcomes are what a producer reports a turn ended as: the provider took the turn, or
// refused it. They are the two values of TurnOutcomeRequest.Outcome, and nothing else is accepted.
const (
	TurnClean   = "clean"
	TurnRefused = "refused"
)

// TurnCodeCyberPolicy names OpenAI's refusal of the session's content for cyber policy, or Oh My Pi
// held a request after such a refusal. Oh My Pi uses that word in its RPC prompt result too.
const TurnCodeCyberPolicy = "cyber_policy"

// TurnCodeCostCap names Oh My Pi refusing a model request at the session's cost cap.
const TurnCodeCostCap = "cost_cap"

// Caller is one of the two identities a claim route's worker calls under — the turn outcome and
// the three task routes (TaskRequest, TaskSeenRequest, TakeRequest) — and the type of whichever
// one its body named (each request's Caller). They are two types rather than five fields of one,
// so the route reads the identity the caller gave rather than guessing from which fields are
// filled in which combination.
type Caller interface{ isCaller() }

// AgentCaller is the producer inside the agent's own session, which holds the secret its
// registration was issued and names the claim with it, as the ready and the exit do: pi-legion's
// agent_end hook for an Oh My Pi session.
type AgentCaller struct {
	ClaimToken Token  `json:"claimToken,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	Secret     string `json:"secret,omitempty"`
}

// BootCaller is the producer beside the agent rather than inside it, which holds no secret — the
// Claude bridge, whose pane's registration is claude-envoy's, inside Claude Code, and the secret
// never leaves that process. It presents the launch's own boot token, the 0600 file its pane is
// told, which names the same claim (api's turnOutcome).
type BootCaller struct {
	BootToken string `json:"bootToken,omitempty"`
}

func (AgentCaller) isCaller() {}
func (BootCaller) isCaller()  {}

// named is whether a body named this identity at all. Any one of the agent's three fields counts:
// a body that named two of them meant this identity and gave it incompletely, which the route
// answers by naming the field it left out rather than by reading it as the other caller.
func (c AgentCaller) named() bool {
	return c.ClaimToken != "" || c.SessionID != "" || c.Secret != ""
}

func (c BootCaller) named() bool { return c.BootToken != "" }

// callerOf is the identity a body named, and false when it named both or neither: each says who
// is calling, and a caller that cannot say is not answered.
func callerOf(agent AgentCaller, boot BootCaller) (Caller, bool) {
	if agent.named() == boot.named() {
		return nil, false
	}
	if boot.named() {
		return boot, true
	}
	return agent, true
}

// TurnOutcomeRequest is the outcome of one turn, reported by the producer that ended it.
// Sequence is that producer's own count of the turns it has ended at Generation, from 1, and the
// daemon keeps the report under the pair: a report that arrives late or out of order lands in its
// own place, and the run of refused turns the daemon derives is the same either way. Message is
// what the provider said about a refusal, and is left out of a clean turn.
//
// Task is the number of the task whose turn this was — the number of the take that started it —
// and 0, left out, for a turn of no task. The report that names the number of the claim's taken
// task is what finishes it; one that names another number, or none, finishes nothing.
// ProviderError is the provider error the turn ended in, left out for none: a report that names
// the task and carries one keeps the task under its number and starts the wait before its value
// is written again, rather than finishing it (supervise's reportedTask).
//
// Held is the number of the accepted take whose text had not run when this turn ended, 0, left
// out, for none: this turn was not that take's. The producer was answered that take during this
// turn, or while it set this turn's prompt up, and holds the text for a turn of its own; or it
// began this turn with that text and the agent never took it, which the Claude bridge reads off a
// text Claude Code never re-emitted. A report the producer sent before the take's answer
// reached it names nothing held. The field is optional: a report without it is read as one
// before it was, and the daemon refuses none for lacking it (supervise's notTheTakesTurn).
//
// HTTPStatus is the HTTP status the provider answered a refused turn with, where the producer's
// provider library gives one: Oh My Pi's `errorStatus` on the assistant message that ended the
// turn, and Claude Code's `api_error_status`. It is 0 (absent) for a clean turn and for a refusal
// whose status cannot be read, such as a content filter's. The daemon reads no status out of
// Message, and it takes a number outside 100 to 599 as no status, with a warning that names it
// (api's turnOutcome). It is how a plugin from before the task's number (below supervise's
// TaskTakeContract) names the status; a current plugin names it in ProviderError's Status, and
// where a report carries both, ProviderError's Status is read and HTTPStatus only where that is 0
// (supervise's RequestTurnOutcome.status).
//
// Unprompted says the turn ran no task the daemon handed: the plugin started it itself (a phase
// reminder), another extension did (an Envoy message), or the host did (a background job's
// result). The daemon reads such a report as one that names no task, whatever its Task says, and
// judges a refusal of it as it judges every turn that was not the take's, from the report alone
// (supervise's unpromptedTurnEnded). A current plugin says the same by naming no Task and never
// sends the mark; a plugin from before the task's number sends it. A clean report, marked or not,
// ends the claim's run of provider-error turns when no later turn's report has reached the daemon
// before it.
//
// Code identifies a refusal. pi-legion reads TurnCodeCyberPolicy from Oh My Pi's
// isCyberPolicyRefusal classifier. The daemon fails the claim under cyber_policy regardless
// of HTTP status. If the report names a take, the daemon takes the task back first.
// TurnCodeCostCap names a refusal at Oh My Pi's session.maxCost. The report can describe
// a turn that stopped or a take that did not start a turn. A statusless cost cap keeps the
// task on the provider-error ladder. It does not fail the claim.
// A Claude Code producer sends no code. The route warns and clears an unknown code, including
// cost_cap from a plugin below the task-take contract.
//
// The caller is exactly one of AgentCaller and BootCaller, embedded so that each is a shape of
// its own on the wire as well as in Go (@legion/contracts' LegionTurnOutcomeRequest is the same
// union).
type TurnOutcomeRequest struct {
	AgentCaller
	BootCaller
	Generation    uint64         `json:"generation"`
	Sequence      uint64         `json:"sequence"`
	Outcome       string         `json:"outcome"`
	Message       string         `json:"message,omitempty"`
	Task          uint64         `json:"task,omitempty"`
	ProviderError *ProviderError `json:"providerError,omitempty"`
	Held          uint64         `json:"held,omitempty"`
	HTTPStatus    int            `json:"httpStatus,omitempty"`
	Unprompted    bool           `json:"unprompted,omitempty"`
	Code          string         `json:"code,omitempty"`
}

// Caller is the identity the body named, and false when it named both or neither.
func (r TurnOutcomeRequest) Caller() (Caller, bool) { return callerOf(r.AgentCaller, r.BootCaller) }

// ProviderError is how a provider failed the turn a turn outcome reports: the HTTP status of the
// provider request that failed (0 when it had none, as a content-filter refusal has none), the
// provider and the model that request went to, the provider's message, and whether the agent
// called the failure retryable. The status alone decides what the daemon does with it
// (supervise's providerErrored); the rest is logged, and the model is named in the controller's
// notice.
type ProviderError struct {
	Status    int    `json:"status"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// TurnOutcomeResponse is what the daemon made of the report: the run of refused turns the claim's
// reports now end with (RefusedRun), counted up to the launch failure limit, which the daemon
// answers each report with and which decides nothing. A producer acts on neither field — its turn
// is over — but a pane's log saying the daemon has counted three refusals in a row is the one
// place an operator sees the count being made.
//
// Failed is whether this report failed the claim. A run of refused turns fails no claim
// (dispatch://INFRA-4508), so only a report whose provider refused its turn in a way that cannot
// clear does, which fails the claim at once and holds the issue for its architect: a status that
// cannot clear, a 4xx other than 401, 403, 408 and 429, or the producer's class of OpenAI's
// cyber-policy refusal (TurnOutcomeRequest.Code), whatever its status, the second under cause
// cyber_policy. Both the report naming the task a take holds (Task), with that status in its
// ProviderError, and the report of any other turn — one naming no task, another number, or marked
// Unprompted — fail it so. A daemon from before
// dispatch://INFRA-4508 set it when the report took the run to the launch failure limit. This is
// where the rule is stated, with its TypeScript mirror (LegionTurnOutcomeResponse in
// packages/contracts/src/legion-api.ts); every other reader of Failed points here.
type TurnOutcomeResponse struct {
	RefusedRun int  `json:"refusedRun"`
	Failed     bool `json:"failed"`
}

// TaskValue is a claim's value as the task bucket holds it: the number of the task the record
// owes the claim, and the epoch and revision of the write that put it there. A revision is read
// only beside its epoch, because a bucket created again counts its revisions from 1 again.
type TaskValue struct {
	Number   uint64 `json:"number"`
	Epoch    string `json:"epoch"`
	Revision uint64 `json:"revision"`
}

// TaskRequest is the body of POST /legion/v1/claims/task: a worker reading its claim's value,
// right after the claim's ready has answered, at a new epoch of its relay, or when it gains its
// role again, under either caller (Caller) and the launch generation it runs at.
type TaskRequest struct {
	AgentCaller
	BootCaller
	Generation uint64 `json:"generation"`
}

// Caller is the identity the body named, and false when it named both or neither.
func (r TaskRequest) Caller() (Caller, bool) { return callerOf(r.AgentCaller, r.BootCaller) }

// TaskResponse is what POST /legion/v1/claims/task answers, and what the registration carries
// (RegisterResponse): the claim's value as the bucket holds it now, null where it holds none, and
// the number of the last task the claim finished, 0 for none.
type TaskResponse struct {
	Task     *TaskValue `json:"task"`
	Finished uint64     `json:"finished"`
}

// TaskSeenRequest is the body of POST /legion/v1/claims/task-seen: the worker reporting that it
// was handed its claim's value at this epoch and revision, naming the number the value names,
// whether or not its host is idle. It is answered 204.
type TaskSeenRequest struct {
	AgentCaller
	BootCaller
	Generation uint64 `json:"generation"`
	Number     uint64 `json:"number"`
	Epoch      string `json:"epoch"`
	Revision   uint64 `json:"revision"`
}

// Caller is the identity the body named, and false when it named both or neither.
func (r TaskSeenRequest) Caller() (Caller, bool) { return callerOf(r.AgentCaller, r.BootCaller) }

// TakeRequest is the body of POST /legion/v1/claims/take: the worker asking, because its host is
// idle, to take the task Number, the one its value at this epoch and revision names. A worker
// sends the same take again when it had no answer (a refused, reset or closed connection, an
// unreachable host or network, a connect or read timeout, a DNS failure, no answer in 5 minutes,
// or a 502, 503 or 504 that names no code), and a take of a number already accepted and not
// finished is answered the same text again, before its turn starts or after.
type TakeRequest struct {
	AgentCaller
	BootCaller
	Generation uint64 `json:"generation"`
	Number     uint64 `json:"number"`
	Epoch      string `json:"epoch"`
	Revision   uint64 `json:"revision"`
}

// Caller is the identity the body named, and false when it named both or neither.
func (r TakeRequest) Caller() (Caller, bool) { return callerOf(r.AgentCaller, r.BootCaller) }

// TakeResponse is what POST /legion/v1/claims/take answers: the outcome — `accepted`, `refused` or
// `dropped`, supervise's TakeOutcome — the text an accepted take runs as its turn's user message,
// and the name of the hold a refused take met (supervise's Hold… names).
type TakeResponse struct {
	Outcome string `json:"outcome"`
	Text    string `json:"text,omitempty"`
	Hold    string `json:"hold,omitempty"`
}

// ActivityRequest is a live role's session reporting what it is doing, after each of its turns
// and whenever the report's content changes, so the idle rule can decide whether the session's
// pod may be released (INFRA-945). It carries no generation: it decides nothing about the claim,
// and a relaunch mints a secret of its own, so the secret the registration issued is the whole
// fence. A claim with no report after its last turn is never released, which is what makes a host
// that cannot report safe.
type ActivityRequest struct {
	ClaimToken Token           `json:"claimToken"`
	SessionID  string          `json:"sessionId"`
	Secret     string          `json:"secret"`
	Activity   SessionActivity `json:"activity"`
}

// SessionActivity is one reading of what a session is doing: when it read it, the background work
// of its own that was running, and its phase reminder's armed wake.
type SessionActivity struct {
	At             time.Time      `json:"at"`
	BackgroundWork BackgroundWork `json:"backgroundWork"`
	Phase          PhaseActivity  `json:"phase"`
}

// BackgroundWork is whether work of the session's own was running and what kinds the session
// found (`async-job`, `subagent`, `claude-background-task`), sorted and de-duplicated by the
// session, so the reason a hold records reads the same every time.
type BackgroundWork struct {
	Running bool     `json:"running"`
	Kinds   []string `json:"kinds"`
}

// KnownKinds is w's kinds with every string that is none of the three known kinds dropped, in
// the session's own order; nil when none is left. The session chooses every word it sends, and
// the kinds reach the architect's worker-background-stalled notice and the daemon's log, so the
// store keeps only these three (store's PutClaimActivity).
func (w BackgroundWork) KnownKinds() []string {
	var known []string
	for _, kind := range w.Kinds {
		switch kind {
		case "async-job", "subagent", "claude-background-task":
			known = append(known, kind)
		}
	}
	return known
}

// PhaseActivity is when the reminder armed for the session's phase is due; NextReminderAt is null
// when no armed reminder is still live, which a session whose phase has closed always reports.
// Whether the phase is open is the daemon's own reading of the record (workflow's idlePhaseOpen),
// not the session's to state.
type PhaseActivity struct {
	NextReminderAt *time.Time `json:"nextReminderAt"`
}

// Refusal is one of the ways a claim route says no: the status it answers with and the sentence
// it puts in the body. Both halves are a contract with the plugins, whose shared claim boot stops
// the agent on any 4xx from the registration (`bootLegionClaim` in
// `packages/pi-shared/src/legion-claim-core.ts`: pi-legion's claim session exits, claude-envoy
// ends the pane) and logs the sentence for the operator, so neither can be reworded here alone. A
// Refusal is an `error`, comparable, and therefore matchable with `errors.Is` after a handler has
// wrapped it with the request it refused.
type Refusal struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
}

func (r Refusal) Error() string { return r.Message }

var (
	// InvalidBootToken: the token on the pane is unknown, already spent on another session, or
	// from a launch this claim has moved past.
	InvalidBootToken = Refusal{Status: http.StatusForbidden, Message: "Invalid boot token"}
	// StaleGeneration: the caller is an agent of a generation the claim has left behind — a pane
	// the daemon has already replaced, answering for a claim that has moved on.
	StaleGeneration = Refusal{Status: http.StatusConflict, Message: "Stale generation"}
	// SameAgentRefusal: a relaunch must resume the agent the claim recorded, so a registration
	// that arrives as a different session is refused rather than allowed to take the role over.
	// The plugin's exit rule and its changelog both quote the sentence verbatim.
	SameAgentRefusal = Refusal{
		Status:  http.StatusConflict,
		Message: "Worker respawn must resume the same agent session",
	}
	// InvalidSecret: a ready or an exit that does not carry the secret the claim's registration
	// was issued — or names a claim the daemon does not hold.
	InvalidSecret = Refusal{Status: http.StatusForbidden, Message: "Invalid session secret"}
)

// SessionHeldRefusal refuses a registration or a resume under a name of a session another claim of
// this daemon recorded first: its id, its file's path or name, or an id its file's name gives (the
// name is what session is). Each name is held to the first claim that recorded it, whatever that
// claim's state since (supervise's holdRecorded): the daemon enrolls the session a registration
// records with the secrets broker, whose enrollment takes over the grants of every other enrollment
// of the same pod account and session, and resumes the session the record names at every
// relaunch, so a role that registered a sibling's session, or an ended claim's, would take that
// session's secrets and its transcript (plan E's t-e-54). It names no other claim's token: the
// caller is told only that the name is not its own, and the daemon's log names the claim that
// holds it. It is a 409, as the same-agent rule's refusal is, and every caller of the registration
// exits on it.
func SessionHeldRefusal(session string) Refusal {
	return Refusal{Status: http.StatusConflict, Message: fmt.Sprintf(
		"session %s is recorded for another claim of this daemon; a claim registers only a session it recorded itself, or one no claim has recorded", session)}
}

// MaxSessionNameBytes is the longest a name of a session may be: a session id, or the base name of a
// session file, in bytes. The daemon holds every name of a session in claim_sessions, whose primary
// key a PostgreSQL btree indexes, and a btree row holds at most 2,704 bytes (SQLSTATE 54000 past
// it), so a name without a bound could stop the migration or the boot that holds it. 255 is the
// longest file name Linux allows (NAME_MAX), and a real name is a uuid or such a file name.
const MaxSessionNameBytes = 255

// SessionNameTooLong refuses a registration whose session id, or whose session file's base name,
// is longer than MaxSessionNameBytes. It is a 400: the request is malformed, whoever sends it.
func SessionNameTooLong(what string, bytes int) Refusal {
	return Refusal{Status: http.StatusBadRequest, Message: fmt.Sprintf(
		"the %s is %d bytes; a session id, and a session file's name, are at most %d bytes", what, bytes, MaxSessionNameBytes)}
}

// SessionFileNotClean refuses a registration whose session file is not the clean path it names
// (filepath.Clean of it differs): a `.` part, a `..` part after a name or at the root, a doubled or
// trailing `/`. Clean keeps a `..` that leads a relative path, so such a path is clean and is not
// refused. A last part of `.` or `..` gives a base name that is not the session's file name, so the
// file-name hold would hold the wrong name; the rest is refused too, so that one file has one
// recorded spelling. It is a 400: the plugins send the clean path of the file they wrote.
func SessionFileNotClean(file string) Refusal {
	return Refusal{Status: http.StatusBadRequest, Message: fmt.Sprintf(
		"the session file %q is not a clean path; a session file is the path of the file the agent writes, as filepath.Clean writes it: no `.` part, no `..` part but at the start of a relative path, and no doubled or trailing `/`", file)}
}

// SessionNameUnheldRefusal refuses a claim that records a session name the daemon cannot hold: one
// longer than MaxSessionNameBytes, or a session file that is not a clean path, which only a daemon
// from before the hold could have recorded. Nothing holds such a name, so it is treated as held for
// no claim: the claim resumes, enrolls and registers nothing under it until an operator gives it a
// new session (`legion claims forget-session`). It is a 409, as SessionHeldRefusal is.
func SessionNameUnheldRefusal(what string) Refusal {
	return Refusal{Status: http.StatusConflict, Message: fmt.Sprintf(
		"this claim records a %s this daemon cannot hold, so it takes nothing under it; an operator gives the claim a new session", what)}
}

// HeldNamesCapRefusal refuses a registration whose new session names would take its claim past
// the most one claim may hold, max (supervise.MaxHeldNamesPerClaim): a hold ends only by an
// operator's release, so without a bound one role could grow the daemon's record of held names
// without end. It is a 409, as SessionHeldRefusal is: the request is well formed, but the claim's
// own record refuses it. Nothing of the registration is recorded, and the claim keeps every name it
// holds. So a claim that records a session still resumes that one, but a claim that needs a new
// session, one that records none after a fresh start, starts none until an operator frees the
// names of its other sessions with `legion claims release-names` (supervise.Machine.ReleaseNames,
// which the daemon's log line names).
func HeldNamesCapRefusal(max int) Refusal {
	return Refusal{Status: http.StatusConflict, Message: fmt.Sprintf(
		"this claim holds the most session names one claim may (%d), so it registers no new session: a session it records still resumes, but a new one waits until an operator frees the names of its other sessions (`legion claims release-names`, which the daemon's log names)", max)}
}

// staleContract renders the opening sentence a stale-caller refusal shares: got is the contract
// the plugin declared, 0 when it declared none; want is the daemon's own (api.DaemonAPIVersion,
// which this package cannot import, so the caller names it). remedy is the clause that tells the
// caller what happens next — StalePluginContract's relaunch, StaleControllerContract's restart
// instruction. The sentence is a contract item — quoted in pi-legion's changelog and its docs —
// and may not be reworded here alone.
func staleContract(got, want int, remedy string) Refusal {
	if got == 0 {
		return Refusal{Status: http.StatusConflict, Message: fmt.Sprintf(
			"pi-legion registered no daemon API contract; this daemon requires %d: %s", want, remedy)}
	}
	return Refusal{Status: http.StatusConflict, Message: fmt.Sprintf(
		"pi-legion speaks daemon API contract %d; this daemon requires %d: %s", got, want, remedy)}
}

// StalePluginContract refuses a registration whose plugin speaks a contract this daemon does not:
// the daemon has already recorded it and is relaunching the process onto the current plugin
// (LEGION-164), so the plugin's exit on this 409 costs the claim nothing when the relaunch is its
// first for this contract, and is the charge itself when it is not (a plugin or an image the
// relaunch put the process on is itself behind this daemon).
func StalePluginContract(got, want int) Refusal {
	return staleContract(got, want, "the daemon is relaunching this process onto the current plugin")
}

// StaleControllerContract refuses a call from the operator's controller recorded at another
// contract across a daemon restart (LEGION-164): the operator's controller is not the daemon's to
// relaunch, so the sentence tells the operator to restart it themselves rather than promising a
// relaunch.
func StaleControllerContract(got, want int) Refusal {
	return staleContract(got, want, "run legion controller start from the release built with this daemon")
}

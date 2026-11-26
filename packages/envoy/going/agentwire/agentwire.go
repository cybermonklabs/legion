// Package agentwire is agent/v1, the contract between the Dispatch server and its agent process
// (envoy-dispatch agent): the seven calls the agent makes of the server over a Unix socket, each
// request and answer with a fixed JSON shape and a size limit, the frame each travels in, and a
// client that makes them. Both processes import it, so neither can send a shape the other does
// not read.
//
// The agent holds no Dispatch data and no Dispatch secret. It keeps the model loop of each run it
// works on, and makes every read and write of Dispatch as a call to the server, which runs it as
// the run's person. The calls:
//
//   - hello: the agent started, under a new instance id; the server frees the leases of the
//     instances it started before.
//   - claim: wait for a ready run and take it under a lease.
//   - renew: renew the instance's leases, carrying its health figures; answered after a wait, or
//     at once when a run of it is stopped.
//   - step: save one step of a run, with the frames the person's bar shows of it.
//   - frames: append frames the person's bar shows of a run to its events, saving no step: the
//     frame of a tool call as the call starts.
//   - call: one read or write of Dispatch, a read of GitHub, or a read of the web, as the run's
//     person.
//   - end: end the run's turn.
//
// Each call is one connection: the agent dials the socket, writes one request frame, and reads
// one answer frame. A call that waits (a claim, a renew) holds only its own connection, and a
// connection the agent closes tells the server it stopped waiting.
package agentwire

import (
	"encoding/json"
	"time"
)

// Version names this contract in every request; the server refuses a request of any other.
const Version = "agent/v1"

// SecretSize is the size of the secret the server writes to the agent's standard input when it
// starts it, which the agent sends with every call.
const SecretSize = 32

// The seven calls, as Request.Call names them.
const (
	CallHello  = "hello"
	CallClaim  = "claim"
	CallRenew  = "renew"
	CallStep   = "step"
	CallFrames = "frames"
	CallCall   = "call"
	CallEnd    = "end"
)

// The eight kinds of call (CallRequest.Kind).
const (
	// KindRoute is one read or write of Dispatch: a tool by its name and its arguments, run
	// through the server's route table as the run's person. A write runs only as a row of the
	// run's plan that the person approved, or that Auto-review let through (CallRequest.Row).
	KindRoute = "route"
	// KindGitHub is a read of a repository through the server's GitHub App.
	KindGitHub = "github"
	// KindWeb is a read of a public web address.
	KindWeb = "web"
	// KindPlan proposes the run's plan (CallRequest.Proposal): the server checks each change
	// against its tool spec and keeps the plan, whose rows wait for the person. It answers the
	// plan as the server keeps it (PlanAnswer).
	KindPlan = "plan"
	// KindReview is the answer of Auto-review's model call on the run's latest plan
	// (CallRequest.Asked): the rows the person's words ask for. The server lets through those of
	// them its mode policy lets through, and answers the rows to run now (ReviewAnswer).
	KindReview = "review"
	// KindBlock asks the server to draw a block of the run (CallRequest.Block, going.RunBlock)
	// from the run's saved tool results: the server runs the block's chain over their rows and
	// answers the block it drew (going.BlockData), or refuses it BLOCK_REFUSED, saying why.
	KindBlock = "block"
	// KindLine asks the server which line on top the decision model Jev reads the run's words
	// as, with the page the run was started on (LineAnswer). It sets no other field of the
	// request and records nothing, so its step and call are 0. The agent makes it as a turn of
	// the run's words starts, beside the turn's first model call, for a run whose line is Ask or
	// none (ClaimedRun.Line): a turn of such a run that Jev reads as Ask needs no check of its
	// reply in prose for a plan it left out.
	KindLine = "line"
	// KindWords asks the server how many words the bar draws of a reply's text (CallRequest.Text)
	// as the run's answer (WordsAnswer): the text as the server stores an answering reply frame's,
	// each link drawn as its item's chip, an issue's its key, its status and its title. Like the
	// line call, it records nothing, so its step and call are 0. The agent makes it for a reply in
	// prose that would end the run's turn done: a reply the bar draws in WordsBound words or more
	// gets one more model turn, which says how many words the person sees.
	KindWords = "words"
)

// WordsBound is the bound of the words the bar draws of an answer: an answer has fewer unless the
// person's words ask for more, and a reply of WordsBound or more gets one more model turn
// (going/agent's holdLong).
const WordsBound = 120

// WordsAnswer is a words call's answer: the words the bar draws of the reply's text.
type WordsAnswer struct {
	Words int `json:"words"`
}

// The lines on top of the command bar a run may be started from (ClaimedRun.Line), as
// going_runs.line holds them, and the line a line call answers (LineAnswer.Line): Ask for words
// that ask for no change, Do for any other action, and File for a new issue's create.
const (
	LineAsk  = "ask"
	LineDo   = "do"
	LineFile = "file"
)

// LineAnswer is a line call's answer: the line on top Jev's choice of action gives the run's
// words, "" where Jev gave none - the server names no decision model, or the decision call failed,
// timed out or named no action it was offered, which the server logs.
type LineAnswer struct {
	Line string `json:"line"`
}

// PlanProposal is the plan a run's model proposed: what it does, in one or two sentences, and
// its changes, in the order they run.
type PlanProposal struct {
	Summary string       `json:"summary"`
	Changes []PlanChange `json:"changes"`
}

// PlanChange is one change of a proposal: a write tool of Dispatch, its arguments, and the
// indexes, from 0, of the earlier changes it needs.
type PlanChange struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Needs     []int           `json:"needs,omitempty"`
}

// PlanAnswer is a plan call's answer: the plan's number within the run, each of its rows, and the
// rows the server approved with the plan, which run at once (Run): the one create of a new-issue
// run (RunNewIssue), which the Enter that started the run approved. Every other plan's rows wait
// for the person, or for Auto-review, and Run holds none.
type PlanAnswer struct {
	Plan int           `json:"plan"`
	Rows []PlanRowRead `json:"rows"`
	Run  []PlanRun     `json:"run,omitempty"`
}

// PlanRowRead is one row of a plan as Auto-review's model reads it: its number (the change's
// index), the tool and its arguments, the item it changes, that item's fields and its value now,
// and the reasons it waits for the person in every mode, none for a row Auto-review may let
// through.
type PlanRowRead struct {
	Row       int             `json:"row"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Item      string          `json:"item,omitempty"`
	Fields    []string        `json:"fields,omitempty"`
	Now       json.RawMessage `json:"now,omitempty"`
	Waits     []string        `json:"waits,omitempty"`
}

// ReviewAnswer is a review call's answer: the rows to run now, in order, and how many rows of the
// plan still wait for the person.
type ReviewAnswer struct {
	Run     []PlanRun `json:"run"`
	Waiting int       `json:"waiting"`
}

// PlanRow names one row of one of a run's plans.
type PlanRow struct {
	Plan int `json:"plan"`
	Row  int `json:"row"`
}

// PlanRun is one row of a plan to run: the row, the tool and the arguments it runs with (the
// person's edits included), and the step whose call already ran it, 0 for none: a row whose call
// an earlier owner of the run made is made again under that step's key, which answers what it
// landed and sends nothing twice.
type PlanRun struct {
	PlanRow
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Step      int             `json:"step,omitempty"`
}

// PersonStep is the body of a step of kind StepPerson, which the server saves when the person
// answers a plan: the plan, and either the rows the person approved (Approved, each with the
// arguments it runs with) and those they left out (Unticked), or the words they asked a new plan
// with (Revise) and the rows of the plan those words revise (Rows).
type PersonStep struct {
	Plan     int       `json:"plan"`
	Approved []PlanRun `json:"approved,omitempty"`
	Unticked []int     `json:"unticked,omitempty"`
	Revise   string    `json:"revise,omitempty"`
	Rows     []PlanRun `json:"rows,omitempty"`
}

// The states an end leaves a run in (EndRequest.State).
const (
	StateDone    = "done"
	StateWaiting = "waiting"
	StateStopped = "stopped"
	StateFailed  = "failed"
)

// The kinds of step (StepRequest.Kind): one model call with its reply, one tool call with its
// result, Auto-review's model call on a plan, and the person's answer to a plan, which the server
// saves itself (PersonStep). A web read's address may be one a tool result of the run names, and
// the server reads those in the run's tool steps.
const (
	StepModel  = "model"
	StepTool   = "tool"
	StepReview = "review"
	StepPerson = "person"
)

// CallBodyLimit is the most of an answer's body a call carries: the server's writer stops there
// and marks the answer cut (CallAnswer.Cut).
const CallBodyLimit = 1 << 20

// The size limit of each call's request and answer frame, in bytes. A request past its limit is
// refused TOO_LARGE before the server reads it, and an answer past its limit is refused by the
// client the same way.
const (
	helloRequestLimit = 4 << 10
	helloAnswerLimit  = 4 << 10
	claimRequestLimit = 4 << 10
	// A claim answers a run's history: its last WholeTurns model turns whole, each with all its
	// tool results, at most 2 MiB, and a one-line note of each older step, beside the run's words
	// and context.
	claimAnswerLimit  = 3 << 20
	renewRequestLimit = 64 << 10
	renewAnswerLimit  = 64 << 10
	// A step holds one model reply, at most 512 KiB, or one tool call with the part of its result
	// the model read, beside the frames it shows.
	stepRequestLimit = 2 << 20
	stepAnswerLimit  = 4 << 10
	// A frames call holds frames alone, bounded as the frames of a step are: by a step's limits.
	framesRequestLimit = stepRequestLimit
	framesAnswerLimit  = stepAnswerLimit
	// A call's arguments are what the model wrote, at most a reply's 512 KiB.
	callRequestLimit = 1 << 20
	// A call answers at most CallBodyLimit bytes of body, which JSON carries as base64.
	callAnswerLimit = 2 << 20
	endRequestLimit = 64 << 10
	endAnswerLimit  = 4 << 10
)

// MaxRequestLimit is the largest request frame of any call, which the server reads a request
// frame within before it knows the call it names.
const MaxRequestLimit = stepRequestLimit

// RequestLimit is the size limit of call's request frame, 0 for no call of this contract.
func RequestLimit(call string) int {
	switch call {
	case CallHello:
		return helloRequestLimit
	case CallClaim:
		return claimRequestLimit
	case CallRenew:
		return renewRequestLimit
	case CallStep:
		return stepRequestLimit
	case CallFrames:
		return framesRequestLimit
	case CallCall:
		return callRequestLimit
	case CallEnd:
		return endRequestLimit
	}
	return 0
}

// AnswerLimit is the size limit of call's answer frame. A call no other limit names answers an
// error alone, which the hello's limit holds.
func AnswerLimit(call string) int {
	switch call {
	case CallClaim:
		return claimAnswerLimit
	case CallRenew:
		return renewAnswerLimit
	case CallStep:
		return stepAnswerLimit
	case CallFrames:
		return framesAnswerLimit
	case CallCall:
		return callAnswerLimit
	case CallEnd:
		return endAnswerLimit
	}
	return helloAnswerLimit
}

// Request is the frame of every call: the contract's version, the secret, the call's name, and
// its body, the call's own request shape.
type Request struct {
	Version string          `json:"version"`
	Secret  []byte          `json:"secret"`
	Call    string          `json:"call"`
	Body    json.RawMessage `json:"body"`
}

// Answer is the frame of every answer: the call's own answer shape, or the error that refused it.
type Answer struct {
	Error *Error          `json:"error,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// The codes of an Error.
const (
	// CodeUnauthorized is a request without the secret or with another one.
	CodeUnauthorized = "UNAUTHORIZED"
	// CodeVersion is a request of another contract than Version.
	CodeVersion = "VERSION_UNSUPPORTED"
	// CodeBadRequest is a request that does not decode as its call's shape, or names no call of
	// this contract.
	CodeBadRequest = "BAD_REQUEST"
	// CodeTooLarge is a frame past its call's size limit.
	CodeTooLarge = "TOO_LARGE"
	// CodeStaleLease is a step, frames, call, end or renew whose lease token is not the run's
	// own, or whose run no longer works: another owner claimed it, or it was stopped. Nothing of
	// it was written.
	CodeStaleLease = "STALE_LEASE"
	// CodeUnknownTool is a route call naming no tool of the server's table.
	CodeUnknownTool = "UNKNOWN_TOOL"
	// CodeWriteNotApproved is a route call of a write tool that names no row of the run's plan
	// that the person approved, or Auto-review let through, with that tool and those arguments.
	CodeWriteNotApproved = "WRITE_NOT_APPROVED"
	// CodeRowWaits is a route call of an approved row whose item changed after the person saw
	// it, or that needs a row that waits: the row waits for the person's approval again, and
	// nothing of it was sent.
	CodeRowWaits = "ROW_WAITS"
	// CodeRowSkipped is a route call of an approved row that needs a row that failed or was
	// skipped: it is skipped, and nothing of it was sent.
	CodeRowSkipped = "ROW_SKIPPED"
	// CodeCallRefused is a route call the tool's own schema or translation refuses before any
	// request is sent.
	CodeCallRefused = "CALL_REFUSED"
	// CodeCallChanged is a second try of a call key whose recorded requests are not the requests
	// this try sends.
	CodeCallChanged = "CALL_CHANGED"
	// CodeCallUnknown is a second try of a call key whose first try still ran when the wait for
	// it ended: whether it landed is unknown.
	CodeCallUnknown = "CALL_UNKNOWN"
	// CodeCallPanicked is a call whose route panicked; the server recovered it and serves on.
	CodeCallPanicked = "CALL_PANICKED"
	// CodeCallFailed is a call that failed in the server for another reason, named in the message.
	CodeCallFailed = "CALL_FAILED"
	// CodeBlockRefused is a block call whose block the server does not draw: a spec that holds a
	// number or does not fit going.RunBlock, or saved results that are not whole or hold no rows
	// its chain reads. The message says why, and what to read or write instead.
	CodeBlockRefused = "BLOCK_REFUSED"
	// CodeGrantEnded is a call of a run whose grant no longer names its person; Reason says why
	// (a sign-out, an expired sign-in, a revoked token, a lost membership), and the run stops.
	CodeGrantEnded = "GRANT_ENDED"
	// CodeSignInUnavailable is a call whose person's membership the sign-in issuer could not
	// confirm now; the call can be made again.
	CodeSignInUnavailable = "SIGN_IN_UNAVAILABLE"
	// CodeRepositoryUnmapped is a GitHub read of a repository that is the default repository of
	// no project the run's person can read.
	CodeRepositoryUnmapped = "GITHUB_REPOSITORY_UNMAPPED"
	// CodeGitHubUnavailable is a GitHub read the server cannot make: no GitHub App, or GitHub
	// refused or failed.
	CodeGitHubUnavailable = "GITHUB_UNAVAILABLE"
	// CodeAddressUnseen is a web read of an address that is in none of the person's words and no
	// tool result of the run.
	CodeAddressUnseen = "WEB_ADDRESS_UNSEEN"
	// CodeAddressRefused is a web read whose address, or an address a redirect led to, resolves
	// to no public address: a loopback, private, link-local or metadata address.
	CodeAddressRefused = "WEB_ADDRESS_REFUSED"
	// CodeWebFailed is a web read that failed for another reason, named in the message.
	CodeWebFailed = "WEB_READ_FAILED"
	// CodeInternal is a failure of the server's own, named in the message.
	CodeInternal = "INTERNAL"
)

// Error is a call the server refused or could not answer.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Reason is why a run's grant ended (CodeGrantEnded), empty for every other code.
	Reason string `json:"reason,omitempty"`
}

func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}

// HelloRequest is the agent saying it started, under Instance, an id it made at its start.
type HelloRequest struct {
	Instance string `json:"instance"`
}

// HelloAnswer is how many leases of the server's earlier agent instances the hello freed.
type HelloAnswer struct {
	Freed int `json:"freed"`
}

// ClaimRequest asks for one ready run for Instance.
type ClaimRequest struct {
	Instance string `json:"instance"`
}

// ClaimAnswer is the run the claim took, nil where none was ready within the claim's wait; the
// agent then asks again.
type ClaimAnswer struct {
	Run *ClaimedRun `json:"run"`
}

// Budget is what a run spent of its level's budget: its model calls, its working time in
// milliseconds, and its output tokens. Time the run waits for its person counts against none.
// ModelMS, PaceMS and ToolMS say where the time went, and bound nothing: the milliseconds its
// model calls took, each from its request to its last token; of those, the milliseconds the
// agent's pacer held its calls' HTTP attempts before they were made; and those its tool calls
// took, each from its start to its end. An end adds each to the run's budget_used, and an agent
// that sends none of them adds 0.
type Budget struct {
	Calls     int   `json:"calls"`
	WorkingMS int64 `json:"working_ms"`
	ModelMS   int64 `json:"model_ms"`
	PaceMS    int64 `json:"pace_ms"`
	ToolMS    int64 `json:"tool_ms"`
	Tokens    int   `json:"tokens"`
}

// ClaimedRun is a run under the lease a claim took: what the person asked, the conversation it
// belongs to and the run it continues, its kind, the line on top it was started from and whether
// the server can ask Jev of its words, its level and mode, what it spent, its lease, its history,
// and the turns of its conversation before it.
type ClaimedRun struct {
	ID           string          `json:"id"`
	Conversation string          `json:"conversation"`
	Continues    string          `json:"continues,omitempty"`
	Words        string          `json:"words"`
	Context      json.RawMessage `json:"context,omitempty"`
	// Kind is RunNewIssue for a run of the line File as a new issue, and "" for a run of the
	// person's words.
	Kind string `json:"kind,omitempty"`
	// Line is the line on top the run was started from (LineAsk, LineDo or LineFile), "" for a
	// run not started from the line: a follow-up typed in the box, the side panel's box, a
	// Continue. An Ask may be Jev's answer or the line the bar reads before Jev answers, so the
	// agent asks Jev again for a run of Ask or of none (KindLine), where the server names a
	// decision model to ask (Decider).
	Line string `json:"line,omitempty"`
	// Decider is whether the server names a decision model, so a line call can draw Jev's
	// answer: with none, the agent makes no line call, and Jev answers nothing.
	Decider     bool      `json:"decider,omitempty"`
	Level       string    `json:"level"`
	Mode        string    `json:"mode"`
	Shape       int       `json:"shape"`
	BudgetUsed  Budget    `json:"budget_used"`
	LeaseToken  string    `json:"lease_token"`
	LeaseEndsAt time.Time `json:"lease_ends_at"`
	// History is the run's saved steps in order: the steps of its last WholeTurns model turns
	// whole (WholeFrom), each older one as its note alone, with no body.
	History []SavedStep `json:"history"`
	// Rows are the rows of the run's plan that the person approved, or Auto-review let through,
	// and that no saved step of the run answers yet, in order: the agent runs each before the
	// run's next model call.
	Rows []PlanRun `json:"rows,omitempty"`
	// Turns are the runs of the conversation before this one, oldest first, the last
	// EarlierTurns of them: what the person asked in each and the note of each of its steps.
	Turns []Turn `json:"turns"`
}

// EarlierTurns is how many of a conversation's runs before a run its claim answers.
const EarlierTurns = 8

// RunNewIssue is the kind of run the command bar's line File as a new issue starts
// (ClaimedRun.Kind): a Fast run whose one task is to file the person's words as one new issue,
// whose plan is that issue's create alone, which the Enter that started the run approved, so it
// runs without waiting for the person (PlanAnswer.Run).
const RunNewIssue = "new_issue"

// Turn is one earlier run of a conversation, as a follow-up is given it: its words and the notes
// of its steps, in order.
type Turn struct {
	Run   string   `json:"run"`
	Words string   `json:"words"`
	Notes []string `json:"notes"`
}

// SavedStep is one step of a run's history.
type SavedStep struct {
	Step int             `json:"step"`
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body,omitempty"`
	Note string          `json:"note"`
}

// WholeTurns is how many of a run's last model turns its history gives whole, each with all its
// tool results (WholeFrom); each older step goes as its note. A model turn is one model call, a
// StepModel step, and every step after it until the next model call: its tool results, and a
// person's answer or a plan's rows after them. A turn is counted, not a step, so a turn that fans
// out to many tool calls does not push the turns before it out.
const WholeTurns = 8

// WholeFrom is the number of the first step of history, in the order of its numbers, that its
// last WholeTurns model turns hold: the WholeTurns-th newest model step's, and 0, which holds
// every step, where history holds fewer model steps than that.
func WholeFrom(history []SavedStep) int {
	models := 0
	for at := len(history) - 1; at >= 0; at-- {
		if history[at].Kind != StepModel {
			continue
		}
		if models++; models == WholeTurns {
			return history[at].Step
		}
	}
	return 0
}

// Lease is one run the instance holds, under the token its claim made.
type Lease struct {
	Run        string `json:"run"`
	LeaseToken string `json:"lease_token"`
}

// Health is the agent's own figures, which each renew carries: its Go runtime's goroutines, live
// heap, total memory and memory limit (RuntimeHealth), the runs its loops work on now, and the
// goroutines its watchdog counts stuck. Each is one number, so the renew's request limit holds it
// whatever the agent works on.
type Health struct {
	Goroutines       int    `json:"goroutines"`
	HeapLiveBytes    uint64 `json:"heap_live_bytes"`
	TotalBytes       uint64 `json:"total_bytes"`
	MemoryLimitBytes uint64 `json:"memory_limit_bytes"`
	Working          int    `json:"working"`
	StuckGoroutines  int    `json:"stuck_goroutines"`
}

// RenewRequest renews the leases the instance holds and carries its health.
type RenewRequest struct {
	Instance string  `json:"instance"`
	Leases   []Lease `json:"leases"`
	Health   Health  `json:"health"`
}

// RenewAnswer is what became of each lease the renew named: renewed, with its new end; its run
// stopped by its person, which the agent cancels at once; or lost, to another owner or to a run
// that no longer works, which the agent gives up.
type RenewAnswer struct {
	Renewed []RenewedLease `json:"renewed"`
	Stopped []string       `json:"stopped"`
	Lost    []string       `json:"lost"`
}

// RenewedLease is a lease renewed until EndsAt.
type RenewedLease struct {
	Run    string    `json:"run"`
	EndsAt time.Time `json:"ends_at"`
}

// Frame is one frame the person's bar shows of a run: its name and its data.
type Frame struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data"`
}

// StepRequest saves step Step of a run: one model call with its reply, or one tool call with its
// result (Kind), its whole body, its one-line note, and the frames it shows.
type StepRequest struct {
	Run        string          `json:"run"`
	LeaseToken string          `json:"lease_token"`
	Step       int             `json:"step"`
	Kind       string          `json:"kind"`
	Body       json.RawMessage `json:"body"`
	Note       string          `json:"note"`
	Frames     []Frame         `json:"frames"`
}

// StepAnswer is the sequence of the last frame the run's events hold once the step is saved.
type StepAnswer struct {
	Sequence int64 `json:"sequence"`
}

// FramesRequest appends Frames to a run's events, in order, and saves no step: the frame of a
// tool call as the call starts, which the person's bar shows while the call runs. The frames of
// a step travel with its StepRequest.
type FramesRequest struct {
	Run        string  `json:"run"`
	LeaseToken string  `json:"lease_token"`
	Frames     []Frame `json:"frames"`
}

// FramesAnswer is the sequence of the last frame the run's events hold once the frames are
// appended.
type FramesAnswer struct {
	Sequence int64 `json:"sequence"`
}

// CallRequest is one call of a run, keyed by the run, its step and the call's number within the
// step: a second try of the same key sends again only what its first try did not land. Kind
// says which kind of call it is, and the fields of that kind are set.
type CallRequest struct {
	Run        string `json:"run"`
	LeaseToken string `json:"lease_token"`
	Step       int    `json:"step"`
	Call       int    `json:"call"`
	Kind       string `json:"kind"`
	// Tool and Arguments are a route call's: the tool's name and the arguments its schema takes.
	// Row names the plan row a write runs, nil for a read and for the model's own call.
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Row       *PlanRow        `json:"row,omitempty"`
	// Proposal is a plan call's proposal, and Asked a review call's rows.
	Proposal *PlanProposal `json:"proposal,omitempty"`
	Asked    []int         `json:"asked,omitempty"`
	// Repository and Path are a GitHub read's: the repository as owner/name, and the path of the
	// read under the repository's API path, such as "pulls/12".
	Repository string `json:"repository,omitempty"`
	Path       string `json:"path,omitempty"`
	// URL is a web read's address.
	URL string `json:"url,omitempty"`
	// Block is a block call's spec, as the run's model wrote it (going.RunBlock).
	Block json.RawMessage `json:"block,omitempty"`
	// Text is a words call's reply, its prose as the model wrote it.
	Text string `json:"text,omitempty"`
}

// CallAnswer is what a call answered: the status, at most CallBodyLimit bytes of the body and
// whether the rest was cut, its content type, and whether it was answered from the record of a
// request an earlier try of the same key landed rather than sent again (Replayed). A replayed
// answer whose route recorded none carries status 0 and no body: the request landed, and what its
// route answered was not kept.
type CallAnswer struct {
	Status      int    `json:"status"`
	Body        []byte `json:"body"`
	Cut         bool   `json:"cut"`
	ContentType string `json:"content_type,omitempty"`
	Replayed    bool   `json:"replayed"`
}

// EndRequest ends the run's turn in State, done, waiting for its person, stopped or failed, with
// Reason, adds what the turn spent to the run's budget, and names what the run's record offers
// its person next (Offers): OfferContinue for a run a bound of its level stopped.
type EndRequest struct {
	Run        string   `json:"run"`
	LeaseToken string   `json:"lease_token"`
	State      string   `json:"state"`
	Reason     string   `json:"reason,omitempty"`
	Used       Budget   `json:"used"`
	Offers     []string `json:"offers,omitempty"`
}

// OfferContinue is the offer of a run a bound of its level stopped: a follow-up run of its
// conversation, with a new budget of the same level.
const OfferContinue = "continue"

// OfferUndo is the offer of a new-issue run that filed its issue (EndFrame.Issue): Undo, which
// closes that issue and posts nothing on it (POST /api/v1/going/runs/{id}/undo). The server adds
// it to the run's end frame; an agent's end offers OfferContinue alone.
const OfferUndo = "undo"

// FrameEnd is the frame an end appends to the run's events: its state, its reason and its offers
// (EndFrame).
const FrameEnd = "end"

// EndFrame is the data of a run's end frame: the state the run's turn ended in, why, what its
// record offers its person next, and the key of the issue a new-issue run filed, "" for none.
type EndFrame struct {
	State  string   `json:"state"`
	Reason string   `json:"reason,omitempty"`
	Offers []string `json:"offers"`
	Issue  string   `json:"issue,omitempty"`
}

// EndAnswer acknowledges an end.
type EndAnswer struct{}

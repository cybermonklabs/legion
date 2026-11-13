package api

// going_writes.go runs one write of a command-bar run (R3, R4): POST
// /api/v1/going/runs/{id}/writes takes one invocation of one action the run's proposal offered,
// claims it on the run's record (claimGoingRunWrite), reads the value it is about to change,
// performs it as the caller, reads the value it left, and records all of it in the place of the
// claim (recordGoingRunWrite). The bar sends one write-store request per write, so a refused write
// shows its refusal in its own line and an undo of a write still queued stops it before it is
// sent.
//
// The write runs as the caller: through the server's own route table, called
// in-process (server.inProcess), with the caller's credential copied from this request and
// nothing else of it. Every check a route makes - its auth class, its scopes, the cross-site
// origin check, the project and session checks, the claim's rules - runs on the copy exactly as
// it runs on a request off the network, so the bar can do nothing its caller could not do by
// hand, and a route's refusal is answered as the route answered it.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingCredentialHeaders are the only headers of the caller's request an in-process write
// carries: the two credentials, the two the cross-site check reads, and the login the write was
// pressed as (refuseChangedLogin). No other header of the caller's rides along, so nothing the
// caller sent can change what a route decides beyond what the caller's own request to that route
// would have. The one exception is a deployment that names its people by a trusted header
// (identity.HeaderIdentity, a local and test mode): there that header is the caller's credential,
// so it is carried too, and it alone.
var goingCredentialHeaders = []string{"Authorization", "Cookie", "Origin", "Sec-Fetch-Site", contracts.DispatchLoginHeader}

// goingInProcess is the server's own route table called as one caller: every request it is given
// carries that caller's credential headers, its host and its peer, and is served by the whole
// chain a request off the network is (server.inProcess). identityHeader is the trusted header a
// header-identity deployment names its people by, "" on every other.
type goingInProcess struct {
	handler        http.Handler
	caller         *http.Request
	identityHeader string
}

// goingDoer is the route table called in-process as the caller of r.
func (s *server) goingDoer(r *http.Request) GoingDoer {
	doer := goingInProcess{handler: s.inProcess, caller: r}
	if header, trusted := s.deps.Identity.(identity.HeaderIdentity); trusted {
		doer.identityHeader = header.Header
	}
	return doer
}

// Do serves request in-process. The request is built fresh by the caller and carries the
// caller's own context, with two of its values taken off: the caller this request already
// resolved, so the route resolves the copied credential itself (resolvedCallerKey), and the
// caller's Idempotency-Key claim, which belongs to the writes route and not to the routes it
// calls (requestKeyClaimKey). The claim's commit step stays, so a write the route commits marks
// the writes route's key committed with it: a repeat of a write that landed is not run again.
func (d goingInProcess) Do(request *http.Request) (*http.Response, error) {
	carried := goingCredentialHeaders
	if d.identityHeader != "" {
		carried = append(slices.Clone(carried), d.identityHeader)
	}
	for _, name := range carried {
		if values := d.caller.Header.Values(name); len(values) > 0 {
			request.Header[http.CanonicalHeaderKey(name)] = slices.Clone(values)
		}
	}
	request.Host = d.caller.Host
	request.TLS = d.caller.TLS
	request.RemoteAddr = d.caller.RemoteAddr
	ctx := context.WithValue(request.Context(), resolvedCallerKey{}, nil)
	ctx = context.WithValue(ctx, requestKeyClaimKey{}, nil)
	answer := &goingAnswer{header: http.Header{}}
	d.handler.ServeHTTP(answer, request.WithContext(ctx))
	return answer.response(request), nil
}

// goingAnswer is what an in-process route answers, kept whole. It has no Unwrap, Flush or Hijack:
// it is no connection, so a layer that reaches for one (a read deadline on the body's drain)
// reaches nothing, and nothing of the caller's own connection.
type goingAnswer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (a *goingAnswer) Header() http.Header { return a.header }

func (a *goingAnswer) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
}

func (a *goingAnswer) Write(body []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.body.Write(body)
}

func (a *goingAnswer) response(request *http.Request) *http.Response {
	status := a.status
	if status == 0 {
		status = http.StatusOK
	}
	body := a.body.Bytes()
	return &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header: a.header, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)),
		Request: request,
	}
}

// How a write came to run (goingRunWrite.Approved).
const (
	goingApprovedAuto      = "auto"
	goingApprovedPerson    = "person"
	goingApprovedConfirmed = "confirmed"
)

// The writes route's own refusals.
const (
	codeGoingRunNotFound          = "GOING_RUN_NOT_FOUND"
	codeGoingRunUnproposed        = "GOING_RUN_UNPROPOSED"
	codeGoingNotProposed          = "GOING_NOT_PROPOSED"
	codeGoingApprovalRequired     = "GOING_APPROVAL_REQUIRED"
	codeGoingNotAutomatic         = "GOING_NOT_AUTOMATIC"
	codeGoingConfirmationRequired = "GOING_CONFIRMATION_REQUIRED"
	codeGoingArguments            = "GOING_ARGUMENTS"
	codeGoingInput                = "GOING_INPUT"
	// codeGoingWriteInProgress is an invocation whose write another request has claimed and not
	// yet recorded: 429 with a Retry-After, which neither the keyed layer keeps (storedStatus)
	// nor the bar's write store gives up on, so the retry is answered the first write's line.
	codeGoingWriteInProgress = "GOING_WRITE_IN_PROGRESS"
	// codeGoingWriteUnsettled is an invocation whose write was claimed longer ago than any
	// request making it can still run (goingClaimBound) and never recorded: it may have landed,
	// so it is not written again, and the person reads the item to see.
	codeGoingWriteUnsettled = "GOING_WRITE_UNSETTLED"
)

// goingClaimedRetryAfter is the Retry-After, in seconds, of a write or an undo another request of
// the same invocation or run is making.
const goingClaimedRetryAfter = "1"

// goingWriteLineOf is the line a write of the run answers, from the write as the run records it.
func goingWriteLineOf(write goingRunWrite) goingWriteLine {
	return goingWriteLine{
		WriteID: write.ID, Action: write.Action, Item: write.Target, Field: write.Field,
		Before: write.Before, Now: write.After, Outcome: write.Outcome,
	}
}

// goingWriteInput is POST /api/v1/going/runs/{id}/writes's body: which invocation of which offer
// of the run's proposal to run, any of its arguments the person changed, the value of the one
// argument the invocation was missing, and how the write came to run.
type goingWriteInput struct {
	// Offer is the action of the offer, as the proposal names it.
	Offer string `json:"offer" required:"true"`
	// Invocation is the index of the invocation among the offer's, from 0.
	Invocation int `json:"invocation"`
	// Arguments replace those of the invocation they name; the action's own schema holds the
	// result, as it held the proposal.
	Arguments map[string]any `json:"arguments"`
	// Fill is the value of the argument the invocation's `missing` names: a text, a number, one of
	// its choices or the item the person picked, and for `@session` the id or the title of the
	// session the write acts for.
	Fill json.RawMessage `json:"fill"`
	// Approved is how the write came to run: at once (auto), from the bar by the person (person),
	// or by the person when the bar asked first (confirmed).
	Approved string `json:"approved" required:"true" enum:"auto,person,confirmed"`
}

// goingWriteLine is the line a write answers: the item it changed, the field of that item, the
// value before and the value now, and how it went.
type goingWriteLine struct {
	WriteID string            `json:"write_id"`
	Action  string            `json:"action"`
	Item    string            `json:"item"`
	Field   string            `json:"field"`
	Before  json.RawMessage   `json:"before"`
	Now     json.RawMessage   `json:"now"`
	Outcome goingWriteOutcome `json:"outcome"`
}

// createGoingWrite is POST /api/v1/going/runs/{id}/writes: one write of the caller's own run. A
// run that is not the caller's, that the record does not hold, or that is older than thirty days
// is 404 GOING_RUN_NOT_FOUND, one answer for all three. Each invocation of a run is written once:
// the write claims its invocation on the run before it reads or sends anything
// (claimGoingRunWrite), in one statement that holds no lock and no connection past itself, so of
// two requests for one invocation one claims it. A request for an invocation whose write went
// through writes nothing and answers that write's line as the run recorded it; one whose write is
// claimed and not yet recorded is 429 GOING_WRITE_IN_PROGRESS with a Retry-After, and once that
// claim is older than any request can still be running (goingClaimBound) 409
// GOING_WRITE_UNSETTLED, since it may have landed. A write that was refused made nothing, and
// does not stop another. A repeat under the write's own Idempotency-Key is answered by the keyed
// layer from the first answer, as every keyed write's is. The run must have proposed or ended
// (409 GOING_RUN_UNPROPOSED), and the action and the invocation must be in its proposal (409
// GOING_NOT_PROPOSED). `auto` is refused, 409 GOING_APPROVAL_REQUIRED, for the two writes that
// always ask and for any but the action the decision chose at or above the action threshold, 409
// GOING_NOT_AUTOMATIC for an invocation the proposal did not mark automatic (goingAutomatic), and
// 409 GOING_CONFIRMATION_REQUIRED where the caller turned confirm_writes on. The value before is
// read, the write performed and the value after read, each in-process as the caller; the write,
// refused or not, is recorded in the place of its claim. A refusal by the route the write ran
// through, or by a read the write needed, is answered as that route answered it, status, code
// and all.
func (s *server) createGoingWrite(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input goingWriteInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !slices.Contains([]string{goingApprovedAuto, goingApprovedPerson, goingApprovedConfirmed}, input.Approved) {
		writeError(w, codeGoingInput, http.StatusBadRequest, "approved must be auto, person or confirmed")
		return
	}
	run, ok := s.readCallersGoingRun(w, r, caller.ID)
	if !ok {
		return
	}
	if s.answerGoingWriteClaimed(w, run, input) {
		return
	}
	if !run.Finished && len(run.Proposal) == 0 {
		writeError(w, codeGoingRunUnproposed, http.StatusConflict,
			"this run has not proposed anything yet: a write runs once the run's proposal is in")
		return
	}
	invocation, refused := goingProposedInvocation(run, input)
	if refused != nil {
		s.writeHandlerError(w, refused)
		return
	}
	if input.Approved == goingApprovedAuto {
		if refused := s.goingAutoRefusal(r.Context(), run, input.Offer, invocation, caller.ID); refused != nil {
			s.writeHandlerError(w, refused)
			return
		}
	}
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	session := goingCredentialOf(r, actor, false).session()
	doer := s.goingDoer(r)
	arguments, named, refused := goingArgumentsOf(invocation, input)
	if refused != nil {
		s.writeHandlerError(w, refused)
		return
	}
	// Bounded as a keyed request's handler is, keyed or not, so a claim older than
	// goingClaimBound is one no request is still making.
	ctx, cancel := context.WithTimeout(r.Context(), s.goingClaimBound())
	defer cancel()
	write := goingRunWrite{ID: uuid.NewString(), Action: input.Offer, Invocation: input.Invocation, Approved: input.Approved, At: time.Now().UTC()}
	if err := s.claimGoingRunWrite(ctx, run.ID, caller.ID, write); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.writeHandlerError(w, fmt.Errorf("claim the going write: %w", err))
			return
		}
		// Another request claimed the invocation since the read, or the run is gone.
		again, ok := s.readCallersGoingRun(w, r, caller.ID)
		if ok && !s.answerGoingWriteClaimed(w, again, input) {
			writeGoingWriteInProgress(w)
		}
		return
	}
	claim := goingWriteClaim{s: s, w: w, ctx: ctx, run: run.ID, login: caller.ID, write: write}
	if named != "" {
		// The session the person named for a row addressing the caller's own session, read off
		// the roster as the person may read it.
		resolved, err := (goingReader{doer: doer}).session(ctx, named)
		if err != nil {
			claim.refuse(goingObservation{}, err)
			return
		}
		session = resolved
	}
	performer := GoingCaller{Catalogue: s.going, Session: session, Doer: doer}
	prepared, err := performer.prepare(ctx, input.Offer, arguments)
	if err != nil {
		claim.refuse(goingObservation{}, err)
		return
	}
	observer := goingObserver{read: performer.reader(), s: s}
	before, err := observer.observe(ctx, prepared, nil, goingObservation{})
	if err != nil {
		claim.refuse(before, err)
		return
	}
	response, landed, err := performer.send(ctx, prepared)
	if err != nil {
		err = fmt.Errorf("perform %s in-process: %w", input.Offer, err)
		if len(landed) == 0 {
			// Nothing was sent, so nothing landed: the invocation may be written again.
			claim.refuse(before, err)
			return
		}
		// A write sent ahead of the row's own landed: the claim stays, so it is not sent twice.
		s.writeHandlerError(w, err)
		return
	}
	claim.write.Landed = landed
	answer, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		// What the write did is unknown: the claim stays, so it is not written again.
		s.writeHandlerError(w, fmt.Errorf("read what %s answered: %w", input.Offer, err))
		return
	}
	if response.StatusCode >= http.StatusMultipleChoices {
		claim.refuse(before, goingRefusalOf(response, answer))
		return
	}
	claim.write.Outcome = goingWriteOutcome{OK: true, Status: response.StatusCode}
	after, err := observer.observe(ctx, prepared, answer, before)
	if err != nil {
		// The write landed; only the read back of it failed, which the record says rather than
		// losing the write, in the words goingFailed gives it.
		after = goingObservation{target: before.target, field: before.field, issue: before.issue}
		claim.write.Outcome.Message = "the write landed and the value after it could not be read: " + goingFailed(err).Message
	}
	claim.write.Target, claim.write.Field = cmp.Or(after.target, before.target), cmp.Or(after.field, before.field)
	claim.write.Before, claim.write.After = goingJSONOrNull(before.value), goingJSONOrNull(after.value)
	if claim.record(cmp.Or(after.issue, before.issue)) {
		WriteJSON(w, http.StatusOK, goingWriteLineOf(claim.write))
	}
}

// answerGoingWriteClaimed answers a request for an invocation the run already holds a write of
// that went through - that write's line - or one claimed and not yet recorded - 429
// GOING_WRITE_IN_PROGRESS while the request that claimed it may still be running, and 409
// GOING_WRITE_UNSETTLED once it cannot be - and reports whether it answered.
func (s *server) answerGoingWriteClaimed(w http.ResponseWriter, run goingRun, input goingWriteInput) bool {
	index := slices.IndexFunc(run.Writes, func(write goingRunWrite) bool {
		return write.Action == input.Offer && write.Invocation == input.Invocation && (write.Outcome.OK || write.Pending)
	})
	if index < 0 {
		return false
	}
	claimed := run.Writes[index]
	switch {
	case !claimed.Pending:
		WriteJSON(w, http.StatusOK, goingWriteLineOf(claimed))
	case time.Since(claimed.At) < s.goingClaimBound():
		writeGoingWriteInProgress(w)
	default:
		writeError(w, codeGoingWriteUnsettled, http.StatusConflict, fmt.Sprintf(
			"this invocation's write was sent at %s and its outcome was never recorded, so it may have landed: it is not written again; read the item to see what it holds",
			claimed.At.Format(time.RFC3339)))
	}
	return true
}

// writeGoingWriteInProgress answers a request for an invocation another request is writing.
func writeGoingWriteInProgress(w http.ResponseWriter) {
	w.Header().Set("Retry-After", goingClaimedRetryAfter)
	writeError(w, codeGoingWriteInProgress, http.StatusTooManyRequests,
		"another request is writing this invocation now: ask again shortly for its line")
}

// goingBookkeeping is the context a claim is recorded or released on: ctx's values without its
// end, bounded as the keyed layer's own bookkeeping is (keyedBookkeeping), so a write that landed
// as its request's time ran out is still recorded.
func goingBookkeeping(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), keyedBookkeeping)
}

// goingWriteClaim is a write that claimed its invocation on the run, and the request answering it.
type goingWriteClaim struct {
	s     *server
	w     http.ResponseWriter
	ctx   context.Context
	run   string
	login string
	write goingRunWrite
}

// record records the claimed write as it went, in the place of its claim, and reports whether it
// was recorded; where it was not, the request is answered the failure, and the claim stays.
func (c goingWriteClaim) record(issue string) bool {
	c.write.At = time.Now().UTC()
	ctx, cancel := goingBookkeeping(c.ctx)
	defer cancel()
	if err := c.s.recordGoingRunWrite(ctx, c.run, c.login, c.write, issue); err != nil {
		c.s.writeGoingRunAppendError(c.w, err)
		return false
	}
	return true
}

// refuse answers a claimed write that was refused or failed before it landed: by the route it ran
// through or by a read it needed, each answered as that route answered it and recorded as the
// write's outcome; by its own schema or translation, 400 GOING_ARGUMENTS, which no route saw and
// the run does not record; or by the server's own failure. Every one but a route's refusal made
// nothing the run records, so its claim is given up and the invocation may be written again.
func (c goingWriteClaim) refuse(before goingObservation, err error) {
	var refusal goingRefusal
	if errors.As(err, &refusal) {
		c.write.Target, c.write.Field = before.target, before.field
		c.write.Before, c.write.After = goingJSONOrNull(before.value), json.RawMessage("null")
		c.write.Outcome = goingWriteOutcome{Status: refusal.Status, Code: refusal.Code, Message: refusal.Message}
		if c.record(before.issue) {
			refusal.write(c.w)
		}
		return
	}
	ctx, cancel := goingBookkeeping(c.ctx)
	defer cancel()
	if released := c.s.releaseGoingRunWrite(ctx, c.run, c.login, c.write.ID); released != nil {
		slog.Error("dispatch: give up the claim of a going write that made nothing", "error", released)
	}
	var argument GoingArgumentError
	if errors.As(err, &argument) {
		writeError(c.w, codeGoingArguments, http.StatusBadRequest, argument.Error())
		return
	}
	c.s.writeHandlerError(c.w, err)
}

// writeGoingRunAppendError answers a write or an undo the record would not take: a run that went
// past thirty days, or was swept, between the read and the append is the run's own 404.
func (s *server) writeGoingRunAppendError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeGoingRunNotFound(w)
		return
	}
	s.writeHandlerError(w, fmt.Errorf("append to the going run: %w", err))
}

// readCallersGoingRun is the caller's run the path names, or the run's 404 written: an id that is
// no uuid, a run of anyone else's, one the record does not hold and one older than thirty days are
// one answer.
func (s *server) readCallersGoingRun(w http.ResponseWriter, r *http.Request, login string) (goingRun, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeGoingRunNotFound(w)
		return goingRun{}, false
	}
	run, err := s.readGoingRun(r.Context(), id.String(), login)
	if errors.Is(err, pgx.ErrNoRows) {
		writeGoingRunNotFound(w)
		return goingRun{}, false
	}
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("read the going run: %w", err))
		return goingRun{}, false
	}
	return run, true
}

func writeGoingRunNotFound(w http.ResponseWriter) {
	writeError(w, codeGoingRunNotFound, http.StatusNotFound,
		"no command-bar run of that id is yours within the last thirty days")
}

// goingProposedInvocation is the invocation the write names, held to the run's proposal: an
// offer of that action the proposal offered, and an invocation of it by that index.
func goingProposedInvocation(run goingRun, input goingWriteInput) (goingInvocation, error) {
	var proposal goingProposal
	if len(run.Proposal) > 0 {
		if err := json.Unmarshal(run.Proposal, &proposal); err != nil {
			return goingInvocation{}, fmt.Errorf("read the run's proposal: %w", err)
		}
	}
	for _, offer := range proposal.Offers {
		if offer.Action != input.Offer || !offer.Offered {
			continue
		}
		if input.Invocation < 0 || input.Invocation >= len(offer.Invocations) {
			return goingInvocation{}, errorf(http.StatusConflict, codeGoingNotProposed,
				"the proposal offered %d invocation(s) of %s, and none at index %d", len(offer.Invocations), input.Offer, input.Invocation)
		}
		return offer.Invocations[input.Invocation], nil
	}
	return goingInvocation{}, errorf(http.StatusConflict, codeGoingNotProposed,
		"this run's proposal did not offer %s: a write runs only an action its run proposed", input.Offer)
}

// goingAutoRefusal is why a write may not run at once, nil where it may: the two writes that
// always ask, any action but the one the decision chose at or above the action
// threshold (the decision's act), an invocation the proposal did not mark automatic - one that
// acts on an item neither the page nor the person's words named (goingAutomatic) - and a caller
// who asked to confirm every write.
func (s *server) goingAutoRefusal(ctx context.Context, run goingRun, action string, invocation goingInvocation, login string) error {
	if goingAlwaysAsk[action] {
		return errorf(http.StatusConflict, codeGoingApprovalRequired,
			"%s always asks before it runs: send it as approved person or confirmed", action)
	}
	var decision goingDecision
	if len(run.Decision) > 0 {
		if err := json.Unmarshal(run.Decision, &decision); err != nil {
			return fmt.Errorf("read the run's decision: %w", err)
		}
	}
	if !decision.Act || len(decision.Actions) == 0 || decision.Actions[0].Action != action {
		return errorf(http.StatusConflict, codeGoingApprovalRequired,
			"the decision did not choose %s at or above the action threshold, so it runs only when the person picks it: send it as approved person or confirmed", action)
	}
	if !invocation.Automatic {
		return errorf(http.StatusConflict, codeGoingNotAutomatic,
			"this invocation of %s acts on an item neither the page nor the person's own words named, so it runs only when the person picks it: send it as approved person or confirmed", action)
	}
	settings, err := s.readGoingSettings(ctx, login)
	if err != nil {
		return err
	}
	if settings.ConfirmWrites {
		return errorf(http.StatusConflict, codeGoingConfirmationRequired,
			"this person asked to confirm every write of the command bar: send it as approved confirmed once they have")
	}
	return nil
}

// goingArgumentsOf is the arguments a write runs with: the invocation's, those the write names
// put over them, and the fill for the argument the invocation was missing. A fill of `@session`
// fills no argument of the action: it names the session the write acts for, which comes back as
// named for the caller to resolve.
func goingArgumentsOf(invocation goingInvocation, input goingWriteInput) (map[string]any, string, error) {
	arguments := make(map[string]any, len(invocation.Arguments)+len(input.Arguments)+1)
	maps.Copy(arguments, invocation.Arguments)
	maps.Copy(arguments, input.Arguments)
	filled := len(input.Fill) > 0 && string(input.Fill) != "null"
	if invocation.Missing == nil {
		if filled {
			return nil, "", errorf(http.StatusBadRequest, codeGoingInput, "this invocation is missing no argument, so it takes no fill")
		}
		return arguments, "", nil
	}
	if !filled {
		if _, given := arguments[invocation.Missing.Field]; given {
			return arguments, "", nil
		}
		return nil, "", errorf(http.StatusBadRequest, codeGoingInput, "fill is required: the value of %s", invocation.Missing.Field)
	}
	var value any
	if err := json.Unmarshal(input.Fill, &value); err != nil {
		return nil, "", errorf(http.StatusBadRequest, codeGoingInput, "fill is no JSON value: %v", err)
	}
	if invocation.Missing.Field == goingCallerSession {
		named, isText := value.(string)
		if !isText || named == "" {
			return nil, "", errorf(http.StatusBadRequest, codeGoingInput, "fill names the session the write acts for by its id or its title")
		}
		return arguments, named, nil
	}
	arguments[invocation.Missing.Field] = value
	return arguments, "", nil
}

// goingObservation is what a read before or after a write saw: the item, its field, the value,
// and the issue the item is or belongs to, where it is one.
type goingObservation struct {
	target string
	field  string
	value  any
	issue  string
}

// goingObserver reads the value a write is about to change and the value it left,
// through the caller's own Doer. An issue's place in its project's ranking is read from the
// store (rankPlace), since no route answers an issue's neighbours in rank order.
type goingObserver struct {
	read goingReader
	s    *server
}

// The field each write that names no field of its own changes.
var goingWriteFields = map[string]string{
	"dispatch_issue":             "issue",
	"dispatch_claim":             "claim",
	"dispatch_ask":               "ask",
	"dispatch_resolve_ask":       "state",
	"dispatch_resolve_comment":   "resolved",
	"dispatch_follow":            "following",
	"dispatch_comment":           "comment",
	"dispatch_suggest":           "suggestion",
	"dispatch_message":           "message",
	"dispatch_send_to_agent":     "message",
	"dispatch_doc_edit":          "content",
	"dispatch_request_approval":  "approval",
	"dispatch_plan_approve":      "plan_review",
	"dispatch_result":            "result",
	"dispatch_artifact":          "artifact",
	"dispatch_project":           "project",
	"dispatch_architecture_sync": "architecture",
	"dispatch_handoff_write":     "handoff",
}

// goingIssueFields are the issue's own fields each argument of an update writes, by argument.
var goingIssueFields = map[string]string{
	"status": "status", "title": "title", "labels": "labels", "priority": "priority",
	"assignee": "assignee", "route": "route", "parent": "parent", "blocked_by": "blocked_by",
	"components": "components", "repo": "repo", "external_links": "external_links",
	"before": "rank", "after": "rank",
}

// goingAskFields are the parts of an ask an edit writes.
var goingAskFields = []string{"question", "options", "multiple", "urgency"}

// observe reads what the prepared write changes: before it (answer nil) and after it, where
// answer is what the write's route answered and before what the read before saw. A write that
// changes a value of an item already there reads that value both times; a write that makes an
// item reads nothing before it and reads the made item off its answer.
func (o goingObserver) observe(ctx context.Context, prepared goingPrepared, answer []byte, before goingObservation) (goingObservation, error) {
	path, arguments := prepared.call.path, prepared.arguments
	field := goingWriteFields[prepared.action]
	issue := goingText(arguments, "issue")
	switch prepared.action {
	case "dispatch_issue_update":
		return o.issueFields(ctx, path["key"], goingIssueFieldsOf(arguments))
	case "dispatch_claim":
		return o.claim(ctx, path["key"])
	case "dispatch_edit_ask":
		return o.askFields(ctx, path["id"], goingAskFieldsOf(arguments))
	case "dispatch_resolve_ask":
		return o.askState(ctx, path["id"])
	case "dispatch_resolve_comment":
		return o.commentResolved(ctx, path["id"])
	case "dispatch_follow":
		return o.following(ctx, path["id"], path["session_id"])
	case "dispatch_doc_edit":
		if before.target != "" {
			// The read after: an edit changes neither the document's id nor its issue, which the
			// read before read, so only its content is read again.
			observed := goingObservation{target: before.target, field: before.field, issue: before.issue}
			value, err := o.documentValue(ctx, before.target)
			if err != nil {
				return observed, err
			}
			observed.value = value
			return observed, nil
		}
		resolved, err := o.editedDocument(ctx, prepared)
		if err != nil {
			return goingObservation{field: field, issue: issue}, err
		}
		return o.document(ctx, resolved)
	}
	if answer == nil {
		if prepared.action == "dispatch_request_approval" {
			// A request renews an approval ask already open rather than opening one, so the
			// asks open before it are what tells the undo whether this write opened its ask.
			open, err := o.openApprovalAsks(ctx, path["id"])
			return goingObservation{target: path["id"], field: field, issue: issue,
				value: goingApprovalBefore{OpenAsks: open}}, err
		}
		// A write that makes an item: nothing of it is there before it.
		return goingObservation{target: goingTargetOfPath(path), field: field, issue: issue}, nil
	}
	switch prepared.action {
	case "dispatch_issue":
		var created struct {
			Key    string `json:"key"`
			Title  string `json:"title"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(answer, &created); err != nil {
			return goingObservation{}, fmt.Errorf("read the issue made: %w", err)
		}
		return goingObservation{target: created.Key, field: field, issue: created.Key,
			value: map[string]any{"key": created.Key, "title": created.Title, "status": created.Status}}, nil
	case "dispatch_ask":
		var created struct {
			ID       string  `json:"id"`
			Question string  `json:"question"`
			State    string  `json:"state"`
			IssueKey *string `json:"issue_key"`
		}
		if err := json.Unmarshal(answer, &created); err != nil {
			return goingObservation{}, fmt.Errorf("read the ask made: %w", err)
		}
		return goingObservation{target: created.ID, field: field, issue: deref(created.IssueKey),
			value: map[string]any{"id": created.ID, "question": created.Question, "state": created.State}}, nil
	case "dispatch_request_approval":
		var requested struct {
			Ask *struct {
				ID string `json:"id"`
			} `json:"ask"`
			ArtifactID string `json:"artifact_id"`
			Version    int    `json:"version"`
		}
		if err := json.Unmarshal(answer, &requested); err != nil {
			return goingObservation{}, fmt.Errorf("read the approval request: %w", err)
		}
		value := map[string]any{"ask": nil, "artifact": requested.ArtifactID, "version": requested.Version}
		target := requested.ArtifactID
		if requested.Ask != nil {
			value["ask"], target = requested.Ask.ID, requested.Ask.ID
		}
		return goingObservation{target: target, field: field, issue: issue, value: value}, nil
	case "dispatch_send_to_agent":
		// The route answers 201 even when the delivery failed, so the line carries the state of
		// the message's last delivery and its error, for the bar to tell a failed send.
		var sent struct {
			ID         string `json:"id"`
			Deliveries []struct {
				State string  `json:"state"`
				Error *string `json:"error"`
			} `json:"deliveries"`
		}
		if err := json.Unmarshal(answer, &sent); err != nil {
			return goingObservation{}, fmt.Errorf("read the message sent: %w", err)
		}
		value := map[string]any{"id": sent.ID, "delivery_state": "", "delivery_error": nil}
		if last := len(sent.Deliveries) - 1; last >= 0 {
			value["delivery_state"], value["delivery_error"] = sent.Deliveries[last].State, sent.Deliveries[last].Error
		}
		return goingObservation{target: sent.ID, field: field, issue: issue, value: value}, nil
	}
	made := goingMadeID(answer)
	if made == "" {
		made = before.target
	}
	return goingObservation{target: made, field: field, issue: issue, value: map[string]any{"id": made}}, nil
}

// goingTargetOfPath is the item a write's path names, before the write: the last placeholder
// value of its path that names one, "" for a write whose path names none.
func goingTargetOfPath(path map[string]string) string {
	for _, placeholder := range []string{"id", "session_id", "key"} {
		if value := path[placeholder]; value != "" {
			return value
		}
	}
	return ""
}

// goingMadeID is the id of the item a write's answer made: its own id, its key, or the id of the
// document or the result it carries.
func goingMadeID(answer []byte) string {
	var made struct {
		ID       string `json:"id"`
		Key      string `json:"key"`
		Artifact *struct {
			ID string `json:"id"`
		} `json:"artifact"`
		Result *struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if json.Unmarshal(answer, &made) != nil {
		return ""
	}
	switch {
	case made.ID != "":
		return made.ID
	case made.Key != "":
		return made.Key
	case made.Artifact != nil:
		return made.Artifact.ID
	case made.Result != nil:
		return made.Result.ID
	}
	return ""
}

// goingIssueFieldsOf is the issue's fields an update's arguments write, in order.
func goingIssueFieldsOf(arguments map[string]any) []string {
	var fields []string
	for argument := range arguments {
		if field, writes := goingIssueFields[argument]; writes && !slices.Contains(fields, field) {
			fields = append(fields, field)
		}
	}
	slices.Sort(fields)
	return fields
}

// goingAskFieldsOf is the parts of an ask an edit's arguments write.
func goingAskFieldsOf(arguments map[string]any) []string {
	var fields []string
	for _, field := range goingAskFields {
		if _, named := arguments[field]; named {
			fields = append(fields, field)
		}
	}
	return fields
}

// issueFields is the issue's fields named, as the issue's own read answers them: each one the
// value a write back sends, an issue's components as the mode, ids and reason an update takes.
func (o goingObserver) issueFields(ctx context.Context, key string, fields []string) (goingObservation, error) {
	var issue map[string]json.RawMessage
	if err := o.read.get(ctx, goingIssuePath(key), &issue); err != nil {
		return goingObservation{target: key, field: strings.Join(fields, ", "), issue: key}, err
	}
	var canonical string
	_ = json.Unmarshal(issue["key"], &canonical)
	key = cmp.Or(canonical, key)
	values := make(map[string]any, len(fields))
	for _, field := range fields {
		if field == goingRank {
			place, err := o.rankPlace(ctx, key)
			if err != nil {
				return goingObservation{target: key, field: strings.Join(fields, ", "), issue: key}, err
			}
			values[field] = place
			continue
		}
		values[field] = goingIssueFieldValue(issue, field)
	}
	return goingObservation{target: key, field: strings.Join(fields, ", "), issue: key, value: values}, nil
}

// goingRankPlace is an issue's place in its project's ranking: its rank, and the issues just
// before it (After, the rank route's `after`) and just after it (Before, its `before`) among every
// issue of the project in rank order, the ordering the rank route places an issue in
// (rankForInput). A neighbour is nil where the issue is first or last.
type goingRankPlace struct {
	Rank   string  `json:"rank"`
	After  *string `json:"after"`
	Before *string `json:"before"`
}

// rankPlace reads the issue's place in its project's ranking.
func (o goingObserver) rankPlace(ctx context.Context, key string) (goingRankPlace, error) {
	var place goingRankPlace
	err := o.s.deps.Store.Pool.QueryRow(ctx, `
		select i.rank,
		  (select p.key from issues p where p.project_key = i.project_key and p.key <> i.key and p.rank < i.rank
		     order by p.rank desc limit 1),
		  (select n.key from issues n where n.project_key = i.project_key and n.key <> i.key and n.rank > i.rank
		     order by n.rank limit 1)
		from issues i where i.key = $1
	`, key).Scan(&place.Rank, &place.After, &place.Before)
	if err != nil {
		return goingRankPlace{}, fmt.Errorf("read %s's place in its project's ranking: %w", key, err)
	}
	return place, nil
}

// rankRestore is how a write back puts an issue at the place it held: between the two neighbours
// it had, those of them that are still issues of its project, or both where they still stand in
// order, and why it cannot where neither is. A read of the neighbours that failed is its error,
// never a reason: the undo answers it as the server's own failure, which leaves the write
// undoable.
func (o goingObserver) rankRestore(ctx context.Context, key string, place goingRankPlace) (map[string]any, string, error) {
	names := make([]string, 0, 2)
	for _, neighbour := range []*string{place.After, place.Before} {
		if neighbour != nil {
			names = append(names, *neighbour)
		}
	}
	// A neighbour that is no longer an issue of the project is passed over: it leaves that side
	// of the place unnamed.
	ranks := map[string]string{}
	if len(names) > 0 {
		rows, err := o.s.deps.Store.Pool.Query(ctx, `
			select n.key, n.rank from issues n join issues i on i.key = $2 and i.project_key = n.project_key
			where n.key = any($1)
		`, names, key)
		if err != nil {
			return nil, "", fmt.Errorf("read the ranks of the issues %s stood between: %w", key, err)
		}
		defer rows.Close()
		for rows.Next() {
			var name, rank string
			if err := rows.Scan(&name, &rank); err != nil {
				return nil, "", fmt.Errorf("read the ranks of the issues %s stood between: %w", key, err)
			}
			ranks[name] = rank
		}
		if err := rows.Err(); err != nil {
			return nil, "", fmt.Errorf("read the ranks of the issues %s stood between: %w", key, err)
		}
	}
	restore := map[string]any{}
	if place.After != nil && ranks[*place.After] != "" {
		restore["after"] = *place.After
	}
	if place.Before != nil && ranks[*place.Before] != "" {
		if after, both := restore["after"].(string); !both || ranks[after] < ranks[*place.Before] {
			restore["before"] = *place.Before
		}
	}
	if len(restore) == 0 {
		return nil, "rank: neither issue it stood between is an issue of its project now, so its place cannot be named back", nil
	}
	return restore, "", nil
}

// goingApprovalBefore is an approval request's value before it: the id of every approval ask open
// where the request could renew one rather than open its own (openApprovalAsks).
type goingApprovalBefore struct {
	OpenAsks []string `json:"open_asks"`
}

// openApprovalAsks is the id of every approval ask open that an approval request of the document
// could renew instead of opening one: each naming the document, and each of the document's issue,
// since a request renews one open over any document it names - an issue's Verification document
// beside its spec (requestArtifactApproval). It is read from the store, as rankPlace is, since no
// route lists a document's open approval asks.
func (o goingObserver) openApprovalAsks(ctx context.Context, artifact string) ([]string, error) {
	open := []string{}
	if err := o.s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce(array_agg(a.id::text order by a.id), '{}')
		from asks a
		where a.kind = 'approval' and a.state = 'open'
		  and (`+docs.ApprovalDocumentsJSONPath+` ? $1
		       or a.issue_key = (select d.issue_key from artifacts d where d.id = $2::uuid))
	`, artifact, artifact).Scan(&open); err != nil {
		return nil, fmt.Errorf("read the approval asks open on %s: %w", artifact, err)
	}
	return open, nil
}

// goingIssueFieldValue is one field of an issue's read as an update writes it.
func goingIssueFieldValue(issue map[string]json.RawMessage, field string) any {
	raw := issue[field]
	if field == "components" {
		var components model.IssueComponents
		if json.Unmarshal(raw, &components) == nil {
			ids := components.IDs
			if ids == nil {
				ids = []string{}
			}
			return map[string]any{"mode": components.Mode, "ids": ids, "reason": components.Reason}
		}
	}
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}

// claim is who holds the issue's claim, nil for nobody.
func (o goingObserver) claim(ctx context.Context, key string) (goingObservation, error) {
	holder, canonical, err := o.claimHolder(ctx, key)
	observed := goingObservation{target: cmp.Or(canonical, key), field: "claim", issue: cmp.Or(canonical, key)}
	if err != nil {
		return observed, err
	}
	if holder != nil {
		observed.value = holder
	}
	return observed, nil
}

// goingHolder is a claim's holder as a write's record keeps it.
type goingHolder struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// claimHolder is the holder of the issue's claim, nil for nobody, and the issue's own key.
func (o goingObserver) claimHolder(ctx context.Context, key string) (*goingHolder, string, error) {
	var issue struct {
		Key   string `json:"key"`
		Claim *struct {
			Actor goingHolder `json:"actor"`
		} `json:"claim"`
	}
	if err := o.read.get(ctx, goingIssuePath(key), &issue); err != nil {
		return nil, "", err
	}
	if issue.Claim == nil {
		return nil, issue.Key, nil
	}
	return &issue.Claim.Actor, issue.Key, nil
}

// goingAskRead is the part of an ask's read the writes and their undos read.
type goingAskRead struct {
	Ask struct {
		ID       string            `json:"id"`
		IssueKey *string           `json:"issue_key"`
		Question string            `json:"question"`
		Options  []model.AskOption `json:"options"`
		Multiple bool              `json:"multiple"`
		Urgency  string            `json:"urgency"`
		State    string            `json:"state"`
	} `json:"ask"`
}

func (o goingObserver) readAsk(ctx context.Context, id string) (goingAskRead, error) {
	var read goingAskRead
	err := o.read.get(ctx, goingAskPath(id), &read)
	return read, err
}

// askFields is the parts of the ask named, each as an edit writes it: an option as its label and
// description, which an edit takes.
func (o goingObserver) askFields(ctx context.Context, id string, fields []string) (goingObservation, error) {
	read, err := o.readAsk(ctx, id)
	observed := goingObservation{target: cmp.Or(read.Ask.ID, id), field: strings.Join(fields, ", "), issue: deref(read.Ask.IssueKey)}
	if err != nil {
		return observed, err
	}
	values := make(map[string]any, len(fields))
	for _, field := range fields {
		switch field {
		case "question":
			values[field] = read.Ask.Question
		case "options":
			options := make([]map[string]any, 0, len(read.Ask.Options))
			for _, option := range read.Ask.Options {
				written := map[string]any{"label": option.Label}
				if option.Description != "" {
					written["description"] = option.Description
				}
				options = append(options, written)
			}
			values[field] = options
		case "multiple":
			values[field] = read.Ask.Multiple
		case "urgency":
			values[field] = read.Ask.Urgency
		}
	}
	observed.value = values
	return observed, nil
}

// askState is the ask's state: open, answered or resolved.
func (o goingObserver) askState(ctx context.Context, id string) (goingObservation, error) {
	read, err := o.readAsk(ctx, id)
	observed := goingObservation{target: cmp.Or(read.Ask.ID, id), field: "state", issue: deref(read.Ask.IssueKey)}
	if err != nil {
		return observed, err
	}
	observed.value = read.Ask.State
	return observed, nil
}

// commentResolved is whether the comment's thread is resolved.
func (o goingObserver) commentResolved(ctx context.Context, id string) (goingObservation, error) {
	var read struct {
		Comment struct {
			ID       string  `json:"id"`
			IssueKey *string `json:"issue_key"`
			Resolved bool    `json:"resolved"`
		} `json:"comment"`
	}
	err := o.read.get(ctx, goingCommentPath(id), &read)
	observed := goingObservation{target: cmp.Or(read.Comment.ID, id), field: "resolved", issue: deref(read.Comment.IssueKey)}
	if err != nil {
		return observed, err
	}
	observed.value = read.Comment.Resolved
	return observed, nil
}

// goingFollowing is a follow's value: the session and whether it follows the ask.
type goingFollowing struct {
	Session   string `json:"session"`
	Following bool   `json:"following"`
}

// following is whether session follows the ask.
func (o goingObserver) following(ctx context.Context, ask, session string) (goingObservation, error) {
	following, err := o.follows(ctx, ask, session)
	observed := goingObservation{target: ask, field: "following"}
	if err != nil {
		return observed, err
	}
	observed.value = goingFollowing{Session: session, Following: following}
	return observed, nil
}

// follows reports whether session follows the ask.
func (o goingObserver) follows(ctx context.Context, ask, session string) (bool, error) {
	var read struct {
		Followers []struct {
			SessionID string `json:"session_id"`
		} `json:"followers"`
	}
	if err := o.read.get(ctx, goingAskPath(ask, "followers"), &read); err != nil {
		return false, err
	}
	for _, follower := range read.Followers {
		if follower.SessionID == session {
			return true, nil
		}
	}
	return false, nil
}

// editedDocument is the id of the document an edit writes, resolved from the slug the issue and
// project rows take.
func (o goingObserver) editedDocument(ctx context.Context, prepared goingPrepared) (string, error) {
	path := prepared.call.path
	switch {
	case path["id"] != "":
		return path["id"], nil
	case goingText(prepared.arguments, "issue") != "" && path["key"] != "" && prepared.route.Pattern == "/api/v1/issues/{key}/artifacts/{slug}/edits":
		return o.read.issueDocument(ctx, path["key"], path["slug"])
	default:
		return o.read.projectDocument(ctx, path["key"], path["slug"])
	}
}

// goingDocumentRead is the part of a document's read the writes and their undos read.
type goingDocumentRead struct {
	ID       string  `json:"id"`
	IssueKey *string `json:"issue_key"`
	Project  string  `json:"project"`
	Name     string  `json:"name"`
}

// goingDocumentValue is a document edit's value: the content, and the version it is.
type goingDocumentValue struct {
	Version *int   `json:"version"`
	Content string `json:"content"`
}

// document is the document's content and version, read by id.
func (o goingObserver) document(ctx context.Context, id string) (goingObservation, error) {
	var artifact goingDocumentRead
	if err := o.read.get(ctx, goingArtifactPath(id), &artifact); err != nil {
		return goingObservation{target: id, field: "content"}, err
	}
	observed := goingObservation{target: artifact.ID, field: "content", issue: deref(artifact.IssueKey)}
	value, err := o.documentValue(ctx, artifact.ID)
	if err != nil {
		return observed, err
	}
	observed.value = value
	return observed, nil
}

func (o goingObserver) documentValue(ctx context.Context, id string) (goingDocumentValue, error) {
	var text struct {
		Markdown string `json:"markdown"`
		Version  *int   `json:"version"`
	}
	if err := o.read.get(ctx, goingArtifactPath(id, "text"), &text); err != nil {
		return goingDocumentValue{}, err
	}
	return goingDocumentValue{Version: text.Version, Content: text.Markdown}, nil
}

// goingJSONOrNull is value as JSON, the literal null for none.
func goingJSONOrNull(value any) json.RawMessage {
	if value == nil {
		return json.RawMessage("null")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("null")
	}
	return encoded
}

// goingSameJSON reports whether two JSON values are the same value, whatever their spelling.
func goingSameJSON(left, right json.RawMessage) bool {
	var a, b any
	if json.Unmarshal(goingJSONOrNullRaw(left), &a) != nil || json.Unmarshal(goingJSONOrNullRaw(right), &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

func goingJSONOrNullRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

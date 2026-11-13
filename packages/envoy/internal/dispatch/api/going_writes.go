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
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
	answer := &recordedAnswer{header: http.Header{}}
	d.handler.ServeHTTP(answer, request.WithContext(ctx))
	return answer.response(request), nil
}

// recordedAnswer is what a handler answers, kept whole: an in-process route's answer to a going
// write, and a test hook's acknowledgement of what its handler's error writer wrote. The first
// status written is the answer's, and a body written with none is a 200's, as net/http's own
// writer has it. It has no Unwrap, Flush or Hijack: it is no connection, so a layer that reaches
// for one (a read deadline on the body's drain) reaches nothing, and nothing of the caller's own
// connection.
type recordedAnswer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (a *recordedAnswer) Header() http.Header { return a.header }

func (a *recordedAnswer) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
}

func (a *recordedAnswer) Write(body []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.body.Write(body)
}

func (a *recordedAnswer) response(request *http.Request) *http.Response {
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
		WriteID: write.ID, Action: write.Action, Item: write.Target, Fields: write.Fields,
		Before: write.Before, Now: write.After, Outcome: write.Outcome, Delivery: write.Delivery,
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

// goingWriteLine is the line a write answers: the item it changed, the fields of that item it
// changed, the value before and the value now, how it went, and for a send to an agent the
// message's last delivery (goingRunWrite.Delivery).
type goingWriteLine struct {
	WriteID  string            `json:"write_id"`
	Action   string            `json:"action"`
	Item     string            `json:"item"`
	Fields   []string          `json:"fields"`
	Before   json.RawMessage   `json:"before"`
	Now      json.RawMessage   `json:"now"`
	Outcome  goingWriteOutcome `json:"outcome"`
	Delivery *goingDelivery    `json:"delivery,omitempty"`
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
// GOING_NOT_AUTOMATIC for an invocation the proposal did not mark automatic (goingAutomatic) or
// one sent with arguments replacing its own, and 409 GOING_CONFIRMATION_REQUIRED where the caller
// turned confirm_writes on. The value before is read, the write performed and the value after
// read, each in-process as the caller; the write, refused or not, is recorded in the place of its
// claim, with the undo the row it went through states. A refusal by the route the write ran
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
	if !run.Finished && run.Proposal == nil {
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
		if refused := s.goingAutoRefusal(r.Context(), run, input, invocation, caller.ID); refused != nil {
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
	// goingClaimBound - this bound and the bookkeeping that records the write after it - is one
	// no request is still making.
	ctx, cancel := context.WithTimeout(r.Context(), s.keyedRunLimit())
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
	claim.write.Undo = prepared.route.Undo.Kind
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
		// losing the write, in the words goingFailed gives it, and marks, so its undo does not
		// read the value it never read as nothing to put back.
		after = goingObservation{target: before.target, fields: before.fields, issue: before.issue}
		claim.write.Outcome.Message = "the write landed and the value after it could not be read: " + goingFailed(err).Message
		claim.write.AfterUnread = true
	}
	claim.write.Target, claim.write.Fields = cmp.Or(after.target, before.target), after.fields
	if len(claim.write.Fields) == 0 {
		claim.write.Fields = before.fields
	}
	claim.write.Delivery = after.delivery
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
// A refusal's record marks no Idempotency-Key committed (store.WithoutBeforeCommit): it makes
// nothing a repeat must not make again, and a write the route made before it refused marked the
// key in its own transaction. So a refusal the keyed layer keeps no answer for - a 5xx, a 401, a
// 408 or a 429 (storedStatus) - leaves the key free, and a repeat under it writes again.
func (c goingWriteClaim) refuse(before goingObservation, err error) {
	var refusal goingRefusal
	if errors.As(err, &refusal) {
		c.write.Target, c.write.Fields = before.target, before.fields
		c.write.Before, c.write.After = goingJSONOrNull(before.value), json.RawMessage("null")
		c.write.Outcome = refusal.outcome()
		c.ctx = store.WithoutBeforeCommit(c.ctx)
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
	var offers []goingOffer
	if run.Proposal != nil {
		offers = run.Proposal.Offers
	}
	for _, offer := range offers {
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
// acts on an item neither the page nor the person's words named (goingAutomatic) - or one sent
// with arguments replacing its own, since the proposal marked automatic only the arguments it
// holds, and a caller who asked to confirm every write.
func (s *server) goingAutoRefusal(ctx context.Context, run goingRun, input goingWriteInput, invocation goingInvocation, login string) error {
	action := input.Offer
	if goingAlwaysAsk[action] {
		return errorf(http.StatusConflict, codeGoingApprovalRequired,
			"%s always asks before it runs: send it as approved person or confirmed", action)
	}
	decision := run.Decision
	if decision == nil || !decision.Act || len(decision.Actions) == 0 || decision.Actions[0].Action != action {
		return errorf(http.StatusConflict, codeGoingApprovalRequired,
			"the decision did not choose %s at or above the action threshold, so it runs only when the person picks it: send it as approved person or confirmed", action)
	}
	if !invocation.Automatic {
		return errorf(http.StatusConflict, codeGoingNotAutomatic,
			"this invocation of %s names an item, or hands the work to a session or a person, that neither the page nor the person's own words named, so it runs only when the person picks it: send it as approved person or confirmed", action)
	}
	if len(input.Arguments) > 0 {
		return errorf(http.StatusConflict, codeGoingNotAutomatic,
			"this write of %s replaces arguments the proposal marked automatic, so it runs only when the person picks it: send it as approved person or confirmed, or send no arguments", action)
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

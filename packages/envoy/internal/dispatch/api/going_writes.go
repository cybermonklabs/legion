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
	"sync/atomic"
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
// caller's own context, with three of its values taken off: the caller this request already
// resolved, so the route resolves the copied credential itself (resolvedCallerKey); the caller's
// Idempotency-Key claim, which belongs to the writes route and not to the routes it calls
// (requestKeyClaimKey); and the claim's commit step (store.WithBeforeCommit). A write of the bar
// may make several requests - a reasoned close posts its reason and then closes - so no one of
// them is the write: the writes route's key is marked committed by the write's own record, once
// the write reached a final outcome (recordGoingRunWrite). In its place a request that carries a
// landing (goingLandings.on) takes the step that records it landed, in its route's own
// transaction (goingLanding.step), for as long as the route runs: a delivery the route detached
// commits after it, and is no landing of the request's.
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
	ctx = store.WithoutBeforeCommit(ctx)
	if landing, carries := ctx.Value(goingLandingKey{}).(goingLanding); carries {
		running := &atomic.Bool{}
		running.Store(true)
		defer running.Store(false)
		ctx = store.WithBeforeCommit(context.WithValue(ctx, goingLandingKey{}, nil), landing.step(running))
	}
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
// claimed and not yet recorded is 429 GOING_WRITE_IN_PROGRESS with a Retry-After. Once that claim
// is older than any request can still be running (goingClaimBound), a write whose record names a
// request of it that landed is resumed after it, under that claim (adoptGoingRunWrite), and one
// whose record names none is 409 GOING_WRITE_UNSETTLED, since its request may have run. A write
// that was refused does not stop another. The run must have proposed or ended (409
// GOING_RUN_UNPROPOSED), and the action and the invocation must be in its proposal (409
// GOING_NOT_PROPOSED). `auto` is refused, 409 GOING_APPROVAL_REQUIRED, for the two writes that
// always ask and for any but the action the decision chose at or above the action threshold, 409
// GOING_NOT_AUTOMATIC for an invocation the proposal did not mark automatic (goingAutomatic), one
// sent with arguments replacing its own, and an upload that would now write the next version of a
// document of its name its route's owner holds, or whose owner it cannot name, and the words do
// not name (goingUploadsOver), and 409 GOING_CONFIRMATION_REQUIRED where the caller turned
// confirm_writes on. The value before is read, the write performed and the value after read, each
// in-process as the caller; the write, refused or not, is recorded in the place of its claim, with
// how each request it sent went and the undo the row it went through states. A refusal by the
// route the write ran through, or by a read the write needed, is answered as that route answered
// it, status, code and all, and one of a request sent after another of the write landed names what
// landed (goingRefusal.after).
//
// A request of the write landed when its route's transaction committed, which the request records
// on the run in that same transaction (going_landed.go): a route that committed and then answered
// 5xx, or whose answer was lost, landed all the same, and is never sent again. A write whose every
// request landed went through, whatever the last one answered; where that answer was no 2xx, the
// value after it is not read (AfterUnread). A write of several requests - a reasoned close posts
// its reason, then closes - is one write to its Idempotency-Key: the key is committed with the
// write's record once the write reached a final outcome, every request landed or one refused with
// a status the keyed layer stores (a 4xx but 401, 408 and 429), which a repeat under the key is
// replayed, however much of the write landed. A refusal it keeps no answer for (5xx, 401, 408,
// 429) leaves the key open. Any later request for the invocation that runs - a repeat under the
// key, or the person's next submit, the bar's own retry or another tab under a key of its own -
// resumes the write after the requests its run records as landed (goingLandedBefore), sending none
// of them again.
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
	write, claimed := s.claimGoingWrite(ctx, w, r, run, caller.ID, input)
	if !claimed {
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
	performer := GoingCaller{Catalogue: s.going, Session: session, Doer: doer,
		landings: &goingLandings{s: s, run: run.ID, login: caller.ID, write: write.ID},
		resumes:  len(goingLanded(write.Requests)) > 0}
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
	if input.Approved == goingApprovedAuto && goingUploadsOver(prepared, before, run.Words) {
		// Read now, under the owner the upload's route writes to: a document of its name may have
		// been made since the proposal marked the upload automatic.
		claim.refuse(before, errorf(http.StatusConflict, codeGoingNotAutomatic,
			"%s would write the next version of %q, a document its owner holds now, or may hold where the write cannot name its owner, and the words do not name, so it runs only when the person picks it: send it as approved person or confirmed",
			input.Offer, goingText(arguments, goingUploadNames[input.Offer])))
		return
	}
	response, requests, err := performer.send(ctx, prepared, claim.write.Requests)
	claim.write.Requests = requests
	var unread goingLandingUnread
	if errors.As(err, &unread) {
		// Whether the request landed is unknown: the claim stays, so it is not written again.
		s.writeHandlerError(w, fmt.Errorf("perform %s in-process: %w", input.Offer, err))
		return
	}
	if err != nil {
		// A request could not be built or sent, so it never reached its route: the claim is given
		// up where nothing of the write landed, and the record says what landed otherwise
		// (goingWriteClaim.refuse), so a retry resumes after it.
		claim.refuse(before, fmt.Errorf("perform %s in-process: %w", input.Offer, err))
		return
	}
	var answer []byte
	if response != nil {
		answer, err = io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			// What the write did is unknown: the claim stays, so it is not written again.
			s.writeHandlerError(w, fmt.Errorf("read what %s answered: %w", input.Offer, err))
			return
		}
	}
	last := &claim.write.Requests[len(claim.write.Requests)-1]
	if !last.Outcome.OK {
		refusal := goingRefusalOf(response, answer)
		last.Outcome = refusal.outcome()
		claim.refuse(before, refusal)
		return
	}
	claim.write.Outcome = goingWriteOutcome{OK: true, Status: last.Outcome.Status}
	after := goingObservation{target: before.target, fields: before.fields, issue: before.issue}
	switch {
	case response == nil:
		// Every request landed in an earlier attempt of the write, whose process ended before it
		// recorded the write: what it left was never read, and where that attempt ended before
		// its last request was answered, the status that answer would have carried is unknown,
		// so the line carries the writes route's own 200.
		claim.write.Outcome.Status = cmp.Or(last.Outcome.Status, http.StatusOK)
		claim.write.Outcome.Message = "the write landed in an earlier attempt that ended before recording it, so the value after it was not read"
		claim.write.AfterUnread = true
	case response.StatusCode >= http.StatusMultipleChoices:
		// The route committed the write and then refused to say so: its answer describes no
		// value it left.
		claim.write.Outcome.Message = fmt.Sprintf("the write landed and its route then answered %d %s, so the value after it was not read",
			response.StatusCode, goingRefusalOf(response, answer).Code)
		claim.write.AfterUnread = true
	default:
		observed, err := observer.observe(ctx, prepared, answer, before)
		if err != nil {
			// The write landed; only the read back of it failed, which the record says rather than
			// losing the write, in the words goingFailed gives it, and marks, so its undo does not
			// read the value it never read as nothing to put back.
			claim.write.Outcome.Message = "the write landed and the value after it could not be read: " + goingFailed(err).Message
			claim.write.AfterUnread = true
			break
		}
		after = observed
	}
	claim.write.Target, claim.write.Fields = cmp.Or(after.target, before.target), after.fields
	if len(claim.write.Fields) == 0 {
		claim.write.Fields = before.fields
	}
	claim.write.Delivery, claim.write.Unchanged = after.delivery, after.unchanged
	claim.write.Before, claim.write.After = goingJSONOrNull(before.value), goingJSONOrNull(after.value)
	if after.stood != nil {
		// What the route answered stood before it, in its own transaction, which no read before
		// the write can say: another writer may have moved it between the two.
		claim.write.Before = after.stood
	}
	if claim.record(cmp.Or(after.issue, before.issue)) {
		WriteJSON(w, http.StatusOK, goingWriteLineOf(claim.write))
	}
}

// goingWriteClaimTries is how often one write reads its run and claims its invocation again when
// another request's write of the invocation is recorded between the two, before it answers the
// invocation under way.
const goingWriteClaimTries = 3

// claimGoingWrite claims the invocation for this request's write, from the run as read, and
// answers the write claimed, or answers the request and reports false. The write resumes after
// the requests the run's latest write of the invocation landed - one refused, since one that went
// through or is pending within goingClaimBound is answered before a write is claimed - whatever
// key that write was sent under (GoingCaller.send), so a request that landed is never sent a
// second time, by a retry under the same key or by a new press under another. The claim holds
// only while that latest write is still the run's latest (claimGoingRunWrite), so what it landed
// is read with the claim and never goes stale: a claim refused because another request recorded a
// write of the invocation since the read reads the run again and claims from that, a few times
// (goingWriteClaimTries), before it answers the invocation under way. A latest write still pending
// past goingClaimBound whose record names a request of it that landed (answerGoingWriteClaimed
// lets no other through) is taken over under its own claim, its landings standing
// (adoptGoingRunWrite).
func (s *server) claimGoingWrite(ctx context.Context, w http.ResponseWriter, r *http.Request, run goingRun, login string, input goingWriteInput) (goingRunWrite, bool) {
	for tries := 1; ; tries++ {
		write := goingRunWrite{ID: uuid.NewString(), Action: input.Offer, Invocation: input.Invocation, Approved: input.Approved, At: time.Now().UTC()}
		latest := goingLatestWrite(run, input)
		var err error
		switch {
		case latest == nil:
			err = s.claimGoingRunWrite(ctx, run.ID, login, write, "")
		case latest.Pending:
			write.ID, write.Requests = latest.ID, slices.Clone(goingLanded(latest.Requests))
			err = s.adoptGoingRunWrite(ctx, run.ID, login, latest.ID, write.At)
		default:
			write.Requests = slices.Clone(goingLanded(latest.Requests))
			err = s.claimGoingRunWrite(ctx, run.ID, login, write, latest.ID)
		}
		if err == nil {
			return write, true
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			s.writeHandlerError(w, fmt.Errorf("claim the going write: %w", err))
			return goingRunWrite{}, false
		}
		// Another request claimed the invocation, or recorded a write of it, since the read, or
		// the run is gone.
		again, ok := s.readCallersGoingRun(w, r, login)
		if !ok || s.answerGoingWriteClaimed(w, again, input) {
			return goingRunWrite{}, false
		}
		if tries == goingWriteClaimTries {
			writeGoingWriteInProgress(w)
			return goingRunWrite{}, false
		}
		run = again
	}
}

// goingLatestWrite is the run's latest write of the invocation, nil for none.
func goingLatestWrite(run goingRun, input goingWriteInput) *goingRunWrite {
	for index, write := range slices.Backward(run.Writes) {
		if write.Action == input.Offer && write.Invocation == input.Invocation {
			return &run.Writes[index]
		}
	}
	return nil
}

// goingLanded is the requests of a write that landed before the first that did not.
func goingLanded(requests []goingWriteRequest) []goingWriteRequest {
	landed := 0
	for landed < len(requests) && requests[landed].Outcome.OK {
		landed++
	}
	return requests[:landed]
}

// answerGoingWriteClaimed answers a request for an invocation the run already holds a write of
// that went through - that write's line - or one claimed and not yet recorded - 429
// GOING_WRITE_IN_PROGRESS while the request that claimed it may still be running, and 409
// GOING_WRITE_UNSETTLED once it cannot be and its record names no request of it that landed - and
// reports whether it answered. A claim past goingClaimBound whose record names a request that
// landed is answered nothing: the request resumes the write after it (claimGoingWrite).
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
	case len(goingLanded(claimed.Requests)) > 0:
		return false
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

// refuse answers a claimed write that was refused or failed before all of it landed: by the route
// a request of it ran through or by a read it needed, each answered as that route answered it -
// naming each request of the write that landed before it (goingRefusal.after) - and recorded as
// the write's outcome, with how each request went; by its own schema or translation, 400
// GOING_ARGUMENTS, which no route saw and the run does not record; or by the server's own failure.
// A failure of either of the last two kinds after a request of the write landed - an adopted
// write, or a request that could not be sent after one that landed - is recorded as the server's
// own 500, so the landings stand for the write that resumes after them. Every other one sent
// nothing the run records, so its claim is given up and the invocation may be written again. A
// refusal's record marks no Idempotency-Key committed (store.WithoutBeforeCommit), and no request
// the write sent marked it (goingInProcess.Do): so a refusal the keyed layer keeps no answer for -
// a 5xx, a 401, a 408 or a 429 (storedStatus) - leaves the key open, and a later request for the
// invocation resumes after the requests that landed (claimGoingWrite), while one it keeps is the
// key's answer, replayed to a repeat.
func (c goingWriteClaim) refuse(before goingObservation, err error) {
	var refusal goingRefusal
	refused := errors.As(err, &refusal)
	if !refused && len(goingLanded(c.write.Requests)) > 0 {
		slog.Error("dispatch: a going write failed after a request of it landed", "error", err)
		refusal, refused = goingRefusal{Status: http.StatusInternalServerError, Code: codeInternal, Message: "internal server error"}, true
	}
	if refused {
		refusal = refusal.after(goingLanded(c.write.Requests))
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

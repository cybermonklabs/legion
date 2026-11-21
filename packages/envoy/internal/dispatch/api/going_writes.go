package api

// going_writes.go is the path every read and write of a command-bar run takes through the server's
// own route table: in-process (server.inProcess), as the run's person, with the command bar's act
// beside them (goingBarCaller), named on each request's context and by no header. Every check a
// route makes - its auth class, its scopes, the project and session checks, the claim's rules -
// runs on each request exactly as it runs on a request off the network, so the bar can do nothing
// its person could not do by hand, and a route's refusal is answered as the route answered it. A
// run's writes go through it as the rows of its approved plan (going_plan_run.go), and its undo
// (going_undo.go) through it too.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingInProcessAddr is the peer of every request the command bar sends in-process, which the
// access log shows: no network address sent it.
const goingInProcessAddr = "unix:agent"

// goingInProcess is the server's own route table called as one caller: every request it is given
// names that caller on its context (resolvedCallerKey), which optionalActor reads before any
// credential, carries no credential header, and is served by the whole chain a request off the
// network is (server.inProcess), at the server's public origin (PublicOrigin) - its scheme and
// its host, so a route that writes a link names the public address - with no TLS state and
// goingInProcessAddr for its peer. limit is the most of an answer's body it keeps, the rest cut
// (recordedAnswer); 0 keeps it whole.
type goingInProcess struct {
	handler      http.Handler
	caller       resolvedCaller
	scheme, host string
	limit        int
}

// goingDoer is the route table called in-process as caller, at origin, the server's public origin
// as PublicOrigin answers it.
func (s *server) goingDoer(caller resolvedCaller, origin string) GoingDoer {
	scheme, host, _ := strings.Cut(origin, "://")
	return goingInProcess{handler: s.inProcess, caller: caller, scheme: scheme, host: host}
}

// goingBarDoer is the route table called in-process as the caller of r, the request that read or
// answered a plan of run or pressed its undo, with the command bar's act beside them
// (goingBarCaller).
func (s *server) goingBarDoer(r *http.Request, run string) (GoingDoer, error) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		return nil, err
	}
	return s.goingDoer(goingBarCaller(actor, human, run), PublicOrigin(s.deps.ServerURL, r)), nil
}

// Do serves request in-process. The request is built fresh by the caller and carries the
// caller's own context, with the doer's caller put on it in place of any caller the context held
// (resolvedCallerKey), and two of its values taken off: the caller's Idempotency-Key claim, which
// belongs to the request that called the doer and not to the routes it calls
// (requestKeyClaimKey); and that request's commit step (store.WithBeforeCommit). A write of the bar
// may make several requests - a reasoned close posts its reason and then closes - so no one of
// them is the write. In its place a request that carries a landing (goingLandingKey) takes the
// step that records it landed, in its route's own transaction (goingLandingStep), for as long as
// the route runs: a delivery the route detached commits after it, and is no landing of the
// request's. Such a request also carries the sink its route hands what it is about to answer
// (goingRecordAnswer), which the step records beside the landing. The request is marked as served
// in-process (goingServedInProcess), so its route refuses a member of its body it does not declare
// rather than ignoring it (judgeUndeclaredMembers): sender and route are one build.
func (d goingInProcess) Do(request *http.Request) (*http.Response, error) {
	return d.serve(request).response(request), nil
}

// serve serves request in-process as Do does, and answers what its route answered, kept whole or
// cut at the doer's limit.
func (d goingInProcess) serve(request *http.Request) *recordedAnswer {
	request.URL.Scheme, request.URL.Host, request.Host = d.scheme, d.host, d.host
	request.TLS = nil
	request.RemoteAddr = goingInProcessAddr
	ctx := context.WithValue(request.Context(), resolvedCallerKey{}, d.caller)
	ctx = context.WithValue(ctx, requestKeyClaimKey{}, nil)
	ctx = context.WithValue(ctx, goingInProcessKey{}, true)
	ctx = store.WithoutBeforeCommit(ctx)
	if landing, carries := ctx.Value(goingLandingKey{}).(goingLandingStep); carries {
		running := &atomic.Bool{}
		running.Store(true)
		defer running.Store(false)
		sink := &goingAnswerSink{}
		ctx = context.WithValue(context.WithValue(ctx, goingLandingKey{}, nil), goingAnswerKey{}, sink)
		ctx = store.WithBeforeCommit(ctx, landing.step(running, sink))
	}
	answer := &recordedAnswer{header: http.Header{}, limit: d.limit}
	d.handler.ServeHTTP(answer, request.WithContext(ctx))
	return answer
}

// goingLandingStep is a landing a request carries on its context (goingLandingKey): the record of
// a run call's request (goingRunLanding) or of a run undo's request (goingUndoLanding) in
// going_landings. Its step records it in the route's own transaction.
type goingLandingStep interface {
	step(running *atomic.Bool, sink *goingAnswerSink) store.BeforeCommit
}

// goingInProcessKey marks the context of a request goingInProcess.Do serves.
type goingInProcessKey struct{}

// goingServedInProcess reports whether ctx is that of a request the command bar sent through
// the route table in-process (goingInProcess.Do), which nothing off the network can mark.
func goingServedInProcess(ctx context.Context) bool {
	marked, _ := ctx.Value(goingInProcessKey{}).(bool)
	return marked
}

// recordedAnswer is what a handler answers, kept whole: an in-process route's answer to a going
// call, and a test hook's acknowledgement of what its handler's error writer wrote. The first
// status written is the answer's, and a body written with none is a 200's, as net/http's own
// writer has it. A limit above 0 keeps that much of the body and drops the rest, marking the
// answer cut, while the handler writes on as though all of it was taken. It has no Unwrap, Flush
// or Hijack: it is no connection, so a layer that reaches for one (a read deadline on the body's
// drain) reaches nothing, and nothing of the caller's own connection.
type recordedAnswer struct {
	header http.Header
	status int
	body   bytes.Buffer
	limit  int
	cut    bool
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
	if a.limit > 0 && a.body.Len()+len(body) > a.limit {
		a.body.Write(body[:a.limit-a.body.Len()])
		a.cut = true
		return len(body), nil
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

// The refusals of a run's routes that read the run the path names, and of the answer run's
// request body.
const (
	codeGoingRunNotFound = "GOING_RUN_NOT_FOUND"
	codeGoingInput       = "GOING_INPUT"
)

func writeGoingRunNotFound(w http.ResponseWriter) {
	writeError(w, codeGoingRunNotFound, http.StatusNotFound,
		"no command-bar run of that id is yours within the last seven days")
}

package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// held_replay.go keeps a write whole, with the caller that made it, and runs it again through the
// server's own route when its turn comes. Two kinds of write are kept so: an agent's reply to a
// delivery it got (POST /api/v1/messages/{id}/reply, POST /api/v1/comments/{id}/reply), which the
// reviewer holds as it holds a comment or a message, and which its post runs again under the id
// the hold reserved (heldPostFrom); and every other write that names a held text by its path (a
// document, a result, a handoff, a claim or an update of a held issue; an edit, an answer or a
// resolution of a held ask, comment or message), which is held beside that text and runs right
// after it posts (awaitHeldTarget, Decision 4 of INFRA-3231's plan), for a client that reads the
// answer such a write gets (contracts.AcceptsKeptWriteHeader).

// heldCaller is the caller of a kept write: the actor optionalActor resolved, with the credential
// it presented, and whether a person signed in at a browser made it. A replay presents it as the
// request's resolved caller (resolvedCallerKey), so no token is kept and none is read again.
type heldCaller struct {
	Actor      model.Actor       `json:"actor"`
	Credential *model.Credential `json:"credential,omitempty"`
	Human      bool              `json:"human,omitempty"`
}

// heldReplay is a write kept whole: its method, path, query, body and caller. Replay marks the
// stored request as one, apart from a kind's own decoded request. Text and Subject are the
// reply's text and where it stands, for an agent's reply to a delivery; zero for any other write.
// PersonTyped is a write a person typed in a session (Dispatch-Person-Typed: 1), which runs again
// with that header. RoleHolder is what a reply's route learned, as the reply was written, of
// whether its writer holds the role the attempt was sent for (roleHolderReplyOf), which its post
// reads back rather than ask the listener again; nil for any other write. Place is where a held
// reply posts (heldRequest.place), read at its hold from the text it answers (repliedTextOf), so
// a later held text of its writer finds it there (heldBeforeOf). EarlierReply is the writer's
// earlier held reply to the same delivery that a held reply waits on (heldReplyPlaceOf), which its
// record reads once it posts (heldReviewWaitsSQL); "" for none. Names are the issues a kept
// write's body names beside the text its path names (heldBodyNames: a PATCH's blocked_by, parent
// and rank neighbours), each of which its post waits for while it is held (heldNamedOpenSQL); nil
// for any other write.
type heldReplay struct {
	Replay       bool             `json:"replay"`
	Method       string           `json:"method"`
	Path         string           `json:"path"`
	Query        string           `json:"query,omitempty"`
	ContentType  string           `json:"content_type,omitempty"`
	Body         []byte           `json:"body,omitempty"`
	Caller       heldCaller       `json:"caller"`
	Text         reviewer.Text    `json:"text"`
	Subject      reviewer.Subject `json:"subject"`
	PersonTyped  bool             `json:"person_typed,omitempty"`
	RoleHolder   *roleHolderReply `json:"role_holder,omitempty"`
	Place        string           `json:"place,omitempty"`
	EarlierReply string           `json:"earlier_reply,omitempty"`
	Names        []string         `json:"names,omitempty"`
}

// isReplay reports whether a held text's request is a write kept whole.
func isReplay(request json.RawMessage) bool {
	var marker struct {
		Replay bool `json:"replay"`
	}
	return json.Unmarshal(request, &marker) == nil && marker.Replay
}

// withBody is replay with its JSON body's "body" field set to body: an edit or a rewrite of a
// held reply.
func (replay heldReplay) withBody(body string) (heldReplay, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(replay.Body, &fields); err != nil {
		return heldReplay{}, fmt.Errorf("read the held reply's body: %w", err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return heldReplay{}, err
	}
	fields["body"] = encoded
	if replay.Body, err = json.Marshal(fields); err != nil {
		return heldReplay{}, err
	}
	replay.Text.Body = body
	return replay, nil
}

// reviewed is the kept write as the reviewer reads it: a reply's text where it stands; any other
// write carries no text.
func (replay heldReplay) reviewed(kind reviewer.Kind) reviewedWrite {
	return reviewedWrite{kind: kind, request: replay, text: replay.Text, subject: replay.Subject, holdable: true}
}

// followUp reports whether the kept write is a reply its route was asked to post as a follow-up on
// an attempt already answered (POST /api/v1/messages/{id}/reply?follow_up=true).
func (replay heldReplay) followUp() bool {
	query, err := url.ParseQuery(replay.Query)
	return err == nil && query.Get("follow_up") == "true"
}

// heldPostKey carries a held text's post, or its dry run, to the route a replay runs through.
type heldPostKey struct{}

// withHeldPost is ctx carrying held.
func withHeldPost(ctx context.Context, held *heldPost) context.Context {
	return context.WithValue(ctx, heldPostKey{}, held)
}

// heldPostFrom is the held text's post ctx carries, nil for a request that posts no held text.
func heldPostFrom(ctx context.Context) *heldPost {
	held, _ := ctx.Value(heldPostKey{}).(*heldPost)
	return held
}

// textIDFor is the id a write creates its text under: the id a held text's post or dry run
// reserved, else a fresh one.
func textIDFor(ctx context.Context) string {
	if held := heldPostFrom(ctx); held != nil {
		return held.id
	}
	return uuid.NewString()
}

// postContext is the context a held text's post and its dry run run on: the server's lifetime,
// with none of the values of the request that set it off (its Idempotency-Key above all, which
// the post must not bind its own recovery point to), and its own transaction tracking.
func (s *server) postContext() context.Context {
	lifetime := s.lifetime
	if lifetime == nil {
		lifetime = context.Background()
	}
	return store.WithTransactionTracking(lifetime)
}

// replayAnswer is what a replayed route answered.
type replayAnswer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (a *replayAnswer) Header() http.Header { return a.header }
func (a *replayAnswer) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
}
func (a *replayAnswer) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.body.Write(b)
}

// refusal is a replayed route's answer as an error: its own status, code and message.
func (a *replayAnswer) refusal() *apiError {
	var body errorBody
	_ = json.Unmarshal(a.body.Bytes(), &body)
	return &apiError{status: a.status, code: cmp.Or(body.Code, "HELD_REPLAY"), message: cmp.Or(body.Error, strings.TrimSpace(a.body.String()))}
}

// writeTo answers w with what the replayed route answered: its headers, its status and its body.
func (a *replayAnswer) writeTo(w http.ResponseWriter) {
	for key, values := range a.header {
		w.Header()[key] = values
	}
	w.WriteHeader(a.status)
	_, _ = w.Write(a.body.Bytes())
}

// runReplay runs replay through the server's own routes as its caller, with held, the held text's
// post or its dry run, on the request's context. It answers the route's answer. The request
// carries the caller as the one it resolved (resolvedCallerKey), and as the credential it
// presented (replayedCallerKey), which the project, session and sign-in checks read as they read a
// broker-issued token's (brokerCredentialActor).
func (s *server) runReplay(ctx context.Context, replay heldReplay, held *heldPost) (*replayAnswer, error) {
	if s.mux == nil {
		return nil, errors.New("the server mounts no routes to run a kept write through")
	}
	target := replay.Path
	if replay.Query != "" {
		target += "?" + replay.Query
	}
	caller := replay.Caller.Actor
	caller.Credential = replay.Caller.Credential
	if caller.Credential == nil {
		caller.Credential = &model.Credential{}
	}
	ctx = context.WithValue(ctx, resolvedCallerKey{}, resolvedCaller{actor: caller, human: replay.Caller.Human})
	ctx = context.WithValue(ctx, replayedCallerKey{}, caller)
	ctx = withHeldPost(ctx, held)
	request, err := http.NewRequestWithContext(ctx, replay.Method, target, bytes.NewReader(replay.Body))
	if err != nil {
		return nil, fmt.Errorf("build the kept write: %w", err)
	}
	if replay.ContentType != "" {
		request.Header.Set("Content-Type", replay.ContentType)
	}
	if replay.PersonTyped {
		request.Header.Set(contracts.PersonTypedHeader, "1")
	}
	if !replay.Caller.Human {
		// The caller is resolved already (resolvedCallerKey): this names a bearer caller to a
		// route that asks whether one is there, and is never read as a token.
		request.Header.Set("Authorization", "Bearer held-replay")
	}
	answer := &replayAnswer{header: http.Header{}}
	s.serveMounted(answer, request)
	return answer, nil
}

// replayedCallerKey carries the caller a request runReplay runs was stored with, which presents no
// credential of its own: the scope checks read it as the credential the request presented
// (brokerCredentialActor), where a layer of the request may have resolved resolvedCallerKey again.
type replayedCallerKey struct{}

// replayedCaller is the caller r, a request runReplay runs, was stored with, and whether r is one.
func replayedCaller(r *http.Request) (model.Actor, bool) {
	caller, replayed := r.Context().Value(replayedCallerKey{}).(model.Actor)
	return caller, replayed
}

// serveMounted serves request on the routes the server mounts, as it serves one from outside. It
// is the one call of the mounted routes a route makes, so the keyed-write guard stops here
// (keyedWriteGuardStops) and walks each mounted route as a route of its own.
func (s *server) serveMounted(answer http.ResponseWriter, request *http.Request) {
	s.mux.ServeHTTP(answer, request)
}

// captureCaller is the caller r resolved, kept for a replay. A caller whose handler resolved its
// actor first passes unloggedResolution(r), so the capture leaves the access-log line naming the
// session that resolution named.
func (s *server) captureCaller(r *http.Request) (heldCaller, error) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		return heldCaller{}, err
	}
	credential := actor.Credential
	actor.Credential = nil
	return heldCaller{Actor: actor, Credential: credential, Human: human}, nil
}

// errHeldDryRun is how a held text's dry run ends: the kind's own create path ran in a
// transaction that took every check the write would, and is rolled back before its commit
// (heldPost.finish). A replayed route answers it as 202 HELD_DRY_RUN.
var errHeldDryRun = &apiError{status: http.StatusAccepted, code: "HELD_DRY_RUN", message: "the held text's dry run passed every check of its write"}

// isDryRunPass reports whether answer is a dry run that passed.
func (a *replayAnswer) isDryRunPass() bool {
	return a.status == errHeldDryRun.status && a.refusal().code == errHeldDryRun.code
}

// reviewedReply is an agent's reply reviewReply reads, beside its decoded input: its kind, its
// text and where it stands; whether its route was asked to post it as a follow-up on an attempt
// already answered (POST /api/v1/messages/{id}/reply?follow_up=true; a mention's reply has none);
// and what the route learned of whether its writer holds the role the attempt was sent for
// (roleHolderReplyOf), which a held reply keeps for its post.
type reviewedReply struct {
	kind       reviewer.Kind
	body       string
	subject    reviewer.Subject
	followUp   bool
	roleHolder roleHolderReply
}

// reviewReply is reviewWrite for an agent's reply to a delivery it got: input is the reply's
// decoded body. A reply the reviewer holds is kept whole and posts by running the reply route
// again under the id the hold reserved; one it reviews after the post is reviewed once the
// handler committed it. A reply the post of a held one runs again is reviewed already and
// recorded already, and goes on.
//
// A reply sent while the same writer's earlier reply to the same delivery is held stands by that
// one (heldReplyPlaceOf), so the delivery's reply is the one the writer wrote first, whichever
// review ends first. A reply the route would answer, once the held one posts, with a held reply's
// text (a retry of the answer, or a follow-up of the same text) is answered that held reply's
// answer, and nothing more is held (answerHeldReplyAgain). Any other waits on the newest held
// reply, reviewed where the reviewer reads it, and held unreviewed where it does not (a plugin's
// generated text, a person's typed write, a write the check does not read, a reply no person
// reads), so the replies post in the order the writer wrote them. A held reply that waits on no
// earlier reply to its delivery waits on its writer's newest held text at the place it posts to
// (heldBeforeOf), as a held ask, comment or message does: it is reviewed at once, and posts after
// that text.
func (s *server) reviewReply(w http.ResponseWriter, r *http.Request, actor model.Actor, input any, reply reviewedReply) (*postedReview, bool) {
	if heldPostFrom(r.Context()) != nil {
		return nil, true
	}
	rv := s.deps.Reviewer
	method, route := routeOf(r)
	setting := rv.setting(reply.kind)
	if !rv.reviewsAny() || s.deps.Store == nil || setting == ReviewOff {
		s.writingCheck(r, actor, input)
		return nil, true
	}
	ctx := r.Context()
	text := reviewer.Text{Kind: reply.kind, Body: reply.body}
	subject := reply.subject
	subject.Text, subject.Writer = text, writerOf(actor)
	// Only a hold keeps a reply, so only under one can the writer have a reply held before this.
	var encoded []byte
	var place heldReplyPlace
	if setting == ReviewHold {
		var err error
		if encoded, err = json.Marshal(input); err != nil {
			s.writeHandlerError(w, err)
			return nil, false
		}
		if place, err = s.heldReplyPlaceOf(ctx, method+" "+route, subject, reply, encoded); err != nil {
			s.writeHandlerError(w, err)
			return nil, false
		}
		if place.repeated != nil {
			s.answerHeldReplyAgain(w, r, *place.repeated, place.repeatedReplay)
			return nil, false
		}
	}
	reader := ""
	if writingCheckExemption(actor) == "" && !generatedText(r, method+" "+route) && !personTypedWrite(r) {
		var err error
		if reader, err = s.writingCheckReader(ctx, r, method+" "+route, actor, input); err != nil {
			slog.Warn("dispatch: decide the reader of a reviewed reply", "route", method+" "+route, "error", err)
			reader = ""
		}
	}
	if reader == "" && place.parent == nil {
		s.writingCheck(r, actor, input)
		return nil, true
	}
	subject.Reader = reader
	project, postsAt := s.repliedTextOf(ctx, reply.kind, r.PathValue("id"))
	if place.parent != nil {
		project = place.parent.project
	}
	event := store.WritingCheckEvent{
		Route: route, Method: method, ActorKind: actor.Kind, ActorID: actor.ID,
		TokenID: s.writingCheckTokenID(r, actor), Reader: reader, Project: project,
	}
	if actor.Act != nil {
		event.ActSession = actor.Act.Session
	}
	if setting != ReviewHold {
		return &postedReview{kind: reply.kind, text: text, subject: subject, event: event}, true
	}
	reviews := reader != ""
	if !reviews {
		// A reply the reviewer does not read before it posts: the writing check reads it now, as
		// it reads any write, and it posts right after the reply it waits on.
		s.writingCheck(r, actor, input)
	}
	caller, err := s.captureCaller(unloggedResolution(r))
	if err != nil {
		s.writeAuthenticationError(w, err)
		return nil, false
	}
	before, earlier := "", ""
	if place.parent != nil {
		earlier = place.parent.id
	} else if before, err = s.heldBeforeOf(ctx, subject.Writer, postsAt); err != nil {
		s.writeHandlerError(w, err)
		return nil, false
	}
	typed := personTypedWrite(r)
	replay := heldReplay{
		Replay: true, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, ContentType: "application/json",
		Body: encoded, Caller: caller, Text: text, Subject: subject, PersonTyped: typed, RoleHolder: &reply.roleHolder,
		Place: postsAt, EarlierReply: earlier,
	}
	s.holdWrite(w, r, actor, replay.reviewed(reply.kind), heldWriteOf{
		route: method + " " + route, reader: reader, project: project, parent: place.parent, before: before,
		reviewed: reviews, event: event, personTyped: typed,
	})
	return nil, false
}

// heldReplyPlace is where a reply stands among the replies its writer holds to the same delivery:
// the held reply it repeats, with the write that reply keeps (repeated, repeatedReplay), or the
// held reply it waits on (parent); neither where it stands by none.
type heldReplyPlace struct {
	parent         *heldParent
	repeated       *heldRow
	repeatedReplay heldReplay
}

// heldReplyPlaceOf reads the held replies on route, not yet posted, that subject's writer wrote to
// the delivery subject answers (its InReplyTo message or its ReplyTo comment), and places reply,
// encoded as its route decoded it, among them. The first reply to an attempt posts as the
// attempt's reply, and each later one as what the reply route makes of a second text on an
// answered attempt: a follow-up, or nothing, which answers the stored reply (replyAfterAnswer,
// and the mention route, which has no follow-up). So a reply to the same attempt that is no
// follow-up repeats the earliest held reply to it that is none either, the attempt's reply to be;
// one that finds only follow-ups held on its attempt, whose reply posted already, waits on none,
// and its route answers it the stored reply. A follow-up repeats the newest held reply to its
// attempt that holds its text. Any other reply waits on the newest held reply, whose post its
// review follows (heldReplay.EarlierReply, claimHeldReview).
func (s *server) heldReplyPlaceOf(ctx context.Context, route string, subject reviewer.Subject, reply reviewedReply, encoded []byte) (heldReplyPlace, error) {
	if subject.Writer == "" {
		return heldReplyPlace{}, nil
	}
	rows, err := s.loadHeldRows(ctx, `route = $1 and writer = $2 and state in `+heldOpenStatesSQL+`
		order by seq desc`, route, subject.Writer)
	if err != nil {
		return heldReplyPlace{}, fmt.Errorf("read the writer's held replies: %w", err)
	}
	attempt := replyAttemptOf(encoded)
	var place heldReplyPlace
	onAttempt := false
	for i, row := range rows {
		var replay heldReplay
		if !isReplay(row.request) || json.Unmarshal(row.request, &replay) != nil ||
			replay.Subject.InReplyTo != subject.InReplyTo || replay.Subject.ReplyTo != subject.ReplyTo {
			continue
		}
		if place.parent == nil {
			place.parent = &heldParent{id: row.id, state: row.state, reader: row.reader, project: row.project}
		}
		if replyAttemptOf(replay.Body) != attempt {
			continue
		}
		onAttempt = true
		switch {
		case !reply.followUp && !replay.followUp():
			// The rows come newest first, so the last one kept is the earliest.
			place.repeated, place.repeatedReplay = &rows[i], replay
		case reply.followUp && place.repeated == nil && replay.Text.Body == reply.body:
			place.repeated, place.repeatedReplay = &rows[i], replay
		}
	}
	if place.repeated != nil || (!reply.followUp && onAttempt) {
		place.parent = nil
	}
	return place, nil
}

// replyAttempt is the delivery attempt a reply's body answers: its attempt, and for a mention's
// reply the mention it names, "" where it names none.
type replyAttempt struct {
	attempt int
	target  string
}

// replyAttemptOf reads the attempt a reply's body answers.
func replyAttemptOf(body []byte) replyAttempt {
	var decoded struct {
		Attempt int     `json:"attempt"`
		Target  *string `json:"target"`
	}
	_ = json.Unmarshal(body, &decoded)
	return replyAttempt{attempt: decoded.Attempt, target: deref(decoded.Target)}
}

// answerHeldReplyAgain answers a reply that repeats row, its writer's held reply, whose write
// replay keeps, as the reply route answers a repeat on an attempt its reply answered: 200, the
// reply under row's id as its post writes it, read off a dry run of replay as row's hold read it
// (heldAnswerOf), with its `held` object as it now stands, and a message marked duplicate as that
// route marks one. As that route's repeat, it names no ask_waiting_on: whom the ask waits on once
// a reply is its newest, which a repeated reply need no longer be. A row that is a follow-up whose
// attempt its writer's earlier held reply answers ran no dry run at its hold and runs none here:
// what it states of its post is read off the held replies it waits on again, read once for the
// choice and the answer both (heldWaitedBeside, heldFollowUpBeside). A dry run its route answers
// otherwise - a refusal, or the stored reply of row, which posted meanwhile - is the repeat's
// answer, as with the reviewer off. Nothing is held, and the writing check records nothing: the
// reply writes none of its words.
func (s *server) answerHeldReplyAgain(w http.ResponseWriter, r *http.Request, row heldRow, replay heldReplay) {
	ctx := r.Context()
	write := replay.reviewed(reviewer.Kind(row.kind))
	var waited *heldChain
	if replay.EarlierReply != "" {
		var err error
		if waited, err = s.heldWaitedBeside(ctx, write, replay.EarlierReply); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var dry *heldDryRun
	if waited == nil {
		if dry = s.heldDryRunOrAnswer(w, write, row.actor, row.id); dry == nil {
			return
		}
		dry.askWaitingOn = ""
	} else {
		var err error
		if dry, err = s.heldFollowUpBeside(ctx, s.deps.Store.Pool, replay, *waited); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	answer := heldAnswerOf(write, row.id, row.actor, row.writtenAt,
		heldState{State: row.state, Deadline: row.deadline, WaitsOn: row.dependsOn}, dry, wantsDisplayTrees(r), nil)
	if read, isMessage := answer.payload.(replyRead); isMessage {
		read.Duplicate = true
		answer.payload = read
	}
	WriteJSON(w, http.StatusOK, answer)
}

// The text a reply answers, read for the reply's project and its place: repliedMessageQuery a
// message's, the project of its issue, the kind and the id of its author and its issue; and
// repliedCommentQuery a comment's, the project of its issue or of its document, its issue and its
// document.
const (
	repliedMessageQuery = `
		select coalesce(i.project_key, ''), m.author ->> 'kind', m.author ->> 'id', m.issue_key
		from messages m left join issues i on i.key = m.issue_key
		where m.id = $1`
	repliedCommentQuery = `
		select coalesce(i.project_key, ar.project_key, ''), c.issue_key, c.artifact_id::text
		from comments c
		left join artifacts ar on ar.id = c.artifact_id
		left join issues i on i.key = coalesce(c.issue_key, ar.issue_key)
		where c.id = $1`
)

// repliedTextOf is the project of the message or the comment id that a reply of kind answers, and
// the place the reply posts to (heldRequest.place): a comment's issue, else its document; for a
// message, the session that sent it, where a session did, as a held message to that session posts
// there, else the message's issue. Each is "" where the text has none or the read fails.
func (s *server) repliedTextOf(ctx context.Context, kind reviewer.Kind, id string) (project, place string) {
	if _, err := uuid.Parse(id); err != nil {
		return "", ""
	}
	if kind == reviewer.KindComment {
		var on owner
		if err := s.deps.Store.Pool.QueryRow(ctx, repliedCommentQuery, id).Scan(&project, &on.IssueKey, &on.ArtifactID); err != nil {
			return "", ""
		}
		return project, placeOf(on)
	}
	var authorKind, authorID, issueKey *string
	if err := s.deps.Store.Pool.QueryRow(ctx, repliedMessageQuery, id).Scan(&project, &authorKind, &authorID, &issueKey); err != nil {
		return "", ""
	}
	sender := ""
	if deref(authorKind) == "session" {
		sender = "session:" + deref(authorID)
	}
	return project, messagePlaceOf(sender, deref(issueKey))
}

// heldTargets are the path values that name a text a later write may wait on, by the prefix of
// the patterns that carry them: an issue's key, and an ask's, a comment's or a message's id.
var heldTargets = []struct{ prefix, param string }{
	{"/api/v1/issues/{key}", "key"},
	{"/api/v1/asks/{id}", "id"},
	{"/api/v1/comments/{id}", "id"},
	{"/api/v1/messages/{id}", "id"},
}

// heldTargetParam is the path value of pattern that names a text a write may wait on, "" for a
// pattern that names none.
func heldTargetParam(pattern string) string {
	for _, target := range heldTargets {
		if pattern == target.prefix || strings.HasPrefix(pattern, target.prefix+"/") {
			return target.param
		}
	}
	return ""
}

// heldTargetSelf are the writes naming a held text by their path that awaitHeldTarget does not
// hold, each with why: the three create routes on an issue, which the reviewer's hook holds
// beside a held issue under an id of their own (reviewWrite), a result's dry run, which writes
// nothing, and the routes of a delivery, which a held text has never had.
var heldTargetSelf = map[string]string{
	"POST /api/v1/issues/{key}/asks":                         "reviewWrite holds a new ask on a held issue beside it, under an id of the ask's own",
	"POST /api/v1/issues/{key}/comments":                     "reviewWrite holds a new comment on a held issue beside it, under an id of the comment's own",
	"POST /api/v1/issues/{key}/messages":                     "reviewWrite holds a new message on a held issue beside it, under an id of the message's own",
	"POST /api/v1/issues/{key}/results/dry-run":              "a result's dry run writes nothing and answers its line now: kept beside a held issue, its replay would answer nobody",
	"POST /api/v1/messages/{id}/reply":                       "a reply to a delivered message: a held message was delivered to no session, a held text's id here is the writer's answer to its findings (answerHeldFrame), and a definition's id the asking session's answer to its side question (answerDefinitionFrame)",
	"POST /api/v1/comments/{id}/reply":                       "a reply to a delivered mention: a held comment was delivered to no session, so no attempt of it can be answered",
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept": "accepts a delivery attempt: a held message has none",
	"POST /api/v1/messages/{id}/deliveries":                  "sends a message again: a held message was never sent, and posts with its first attempt",
	"POST /api/v1/comments/{id}/deliveries":                  "sends a comment's mention again: a held comment was never sent, and posts with its first attempts",
	"POST /api/v1/asks/{id}/rewrites":                        "a person's Rewrite of a question they read: a held ask is in no person's read until it posts, and its id names no ask row, so the Rewrite answers 404",
}

// awaitHeldTarget holds route's write when the text its path names is a held text that has not
// posted: the write is kept whole with its caller (heldReplay), answered 202 with the `held`
// object, and runs through this route right after the held text posts, in the order the writes
// came. Any other write goes to handler. A route whose path names no such text, a read, and a
// route of heldTargetSelf are not wrapped. A person signed in at a browser learns nothing of a
// held text: their write is the route's own, which finds no such text and answers as it does with
// the reviewer off, as a person's read is (answerHeldRead).
//
// A write whose body names issues its route reads (heldBodyNames: a PATCH's blocked_by, parent and
// rank neighbours) is kept beside the newest held one of them where its path names no held text,
// and its post waits for every one of them still held, whichever text it is kept beside
// (heldNamedOpenSQL), so it finds each as a row, as it does with the reviewer off. A caller whose
// credential names one project is refused a held blocker of another project now, as the post
// refuses that blocker once it posted, rather than kept to wait on another project's held text.
//
// Only a session's write whose client says it reads that 202 (contracts.AcceptsKeptWriteHeader:
// 1, which the agents' DispatchClient sends on every write) is kept. The 202 carries none of the
// route's own shape, and a client built before the header reads it as that shape all the same:
// `dispatch result`, `artifact` and `claim` threw a TypeError reading a field it lacks; `claim
// --release`, `handoff-write`, `edit-ask` and `resolve-comment` printed `undefined`; `resolve-ask`
// failed, though the write was kept; and a retry of any of them kept a second write. So a
// session's write without the header is answered 409 HELD_TEXT with the held id and the step to
// take, in the shape of a session's read of a held text (answerHeldRead), which every client shows
// as a refusal, and nothing is kept: the write runs when it is sent again after the text posts.
func (s *server) awaitHeldTarget(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	name := route.Method + " " + route.Pattern
	param, held := awaitsHeldTarget(route.Method, route.Pattern)
	if !held {
		return handler
	}
	bodyNames := heldBodyNames[name]
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.keepsHeldWrites(r) {
			handler(w, r)
			return
		}
		var named heldBodyNamed
		if bodyNames != nil {
			var err error
			if named, err = bodyNames(r); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
		parent, err := s.heldParentOf(r.Context(), []string{r.PathValue(param)}, named.names)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if parent == nil {
			handler(w, r)
			return
		}
		// The caller is resolved again by the write's own path (captureCaller, or the handler), whose
		// resolution names it on the access-log line.
		if _, human, err := s.optionalActor(unloggedResolution(r)); err != nil || human {
			handler(w, r)
			return
		}
		if err := s.refuseHeldForeignBlockers(r, named.blockers); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if r.Header.Get(contracts.AcceptsKeptWriteHeader) != "1" {
			WriteJSON(w, http.StatusConflict, heldTextError{
				Error: "held text " + parent.id + " has not posted, and this client does not read the answer of a write " +
					"Dispatch keeps beside it: run the write again after the text posts, or update the plugin so Dispatch " +
					"can keep the write and run it right after the text posts",
				Code: "HELD_TEXT", HeldID: parent.id,
			})
			return
		}
		s.holdRequest(w, r, name, parent, named.names)
	}
}

// keepsHeldWrites reports whether a write r makes may be kept beside a held text: the reviewer
// reads some kind, and r is no held text's post running the write it kept.
func (s *server) keepsHeldWrites(r *http.Request) bool {
	return s.deps.Reviewer.reviewsAny() && s.deps.Store != nil && heldPostFrom(r.Context()) == nil
}

// heldTargetOf is the held text, not yet posted, that r's path value param names, for a route whose
// body names no held text (awaitHeldTarget reads heldBodyNames beside the path in one lookup): nil
// where the reviewer reads no kind, where r is a held text's post running the write it kept, and
// where the path names no such text.
func (s *server) heldTargetOf(r *http.Request, param string) (*heldParent, error) {
	if !s.keepsHeldWrites(r) {
		return nil, nil
	}
	return s.heldParentOf(r.Context(), []string{r.PathValue(param)}, nil)
}

// heldBodyNamed is what a kept write's body names that a held text may hold (heldBodyNames):
// every such id, and among them the blockers the write would add.
type heldBodyNamed struct {
	names, blockers []string
}

// heldBodyNames are the writes awaitHeldTarget keeps whose body names, beside the text their path
// names, issues their route reads and refuses where no row holds one, each with what reads them:
// a PATCH of an issue names its blockers (blocked_by), its parent and its rank neighbours
// (issuePatchNamed).
var heldBodyNames = map[string]func(*http.Request) (heldBodyNamed, error){
	"PATCH /api/v1/issues/{key}": issuePatchNamed,
}

// issuePatchNamed reads, out of r's body, a PATCH of an issue, the issues it names in blocked_by,
// parent and rank's after and before, and puts the body back for the route to read. A body past
// the route's bound, or one that does not decode, names none: the route answers it.
func issuePatchNamed(r *http.Request) (heldBodyNamed, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONRequestBytes+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
	if err != nil || int64(len(body)) > maxJSONRequestBytes {
		return heldBodyNamed{}, err
	}
	var input struct {
		BlockedBy []string        `json:"blocked_by"`
		Parent    json.RawMessage `json:"parent"`
		Rank      *rankInput      `json:"rank"`
	}
	if json.Unmarshal(body, &input) != nil {
		return heldBodyNamed{}, nil
	}
	named := heldBodyNamed{names: slices.Clone(input.BlockedBy), blockers: input.BlockedBy}
	if parent, _, err := parseIssueParent(input.Parent); err == nil && parent != nil {
		named.names = append(named.names, *parent)
	}
	if input.Rank != nil {
		for _, neighbour := range []*string{input.Rank.After, input.Rank.Before} {
			if neighbour != nil {
				named.names = append(named.names, strings.TrimSpace(*neighbour))
			}
		}
	}
	return named, nil
}

// refuseHeldForeignBlockers refuses a caller whose credential names one project a held issue of
// another project among blockers, which a write kept beside held texts would wait on, with the
// refusal its post gives once that blocker posted (writeBlockedBy): the first blocker of another
// project in key order, posted or held, so the hold names the blocker its post would. Where no held
// blocker is of another project, a blocker that has posted, or one nothing holds, is the post's to
// judge, as every other check of a kept write is. The write's own issue is of the caller's project,
// which withinProjectScope checked before this.
func (s *server) refuseHeldForeignBlockers(r *http.Request, blockers []string) error {
	covers, scoped := s.callerProject(r)
	if !scoped || len(blockers) == 0 {
		return nil
	}
	held, err := heldIssuesAmong(r.Context(), s.deps.Store.Pool, blockers)
	if err != nil {
		return err
	}
	heldForeign := heldForeignTargets(held, covers)
	if len(heldForeign) == 0 {
		return nil
	}
	foreign, err := foreignTargets(r.Context(), s.deps.Store.Pool, covers, blockers)
	if err != nil {
		return err
	}
	return refuseForeignBlocker(covers, append(foreign, heldForeign...))
}

// awaitsHeldTarget reports whether awaitHeldTarget holds a write of method and pattern, with the
// path value that names the text it may wait on: a write whose path names an issue, an ask, a
// comment or a message, other than those heldTargetSelf names.
func awaitsHeldTarget(method, pattern string) (string, bool) {
	param := heldTargetParam(pattern)
	return param, method != http.MethodGet && param != "" && heldTargetSelf[method+" "+pattern] == ""
}

// keptAnswers are the answers awaitHeldTarget gives a write of method and pattern beside its
// route's own: 202 with the `held` object to a client that reads it, and 409 HELD_TEXT to any
// other; none for a write it does not wrap. The OpenAPI emitter reads them beside the route's
// shape (routeShapes), so the document names them where they can come.
func keptAnswers(method, pattern string) []routeAnswer {
	if _, held := awaitsHeldTarget(method, pattern); !held {
		return nil
	}
	return []routeAnswer{{Status: http.StatusAccepted, Body: heldDeferred{}}, {Status: http.StatusConflict, Body: heldTextError{}}}
}

// heldDeferred is the answer of a write held beside a held text: the id it is kept under, and
// the `held` object, whose waits_on names the text it runs after.
type heldDeferred struct {
	ID   string    `json:"id"`
	Held heldState `json:"held"`
}

// heldRequestBodyMax bounds the body of a write kept whole: the route's own bound where it names
// one (routeBody), which the route then enforces again when the write runs.
const heldRequestBodyMax = 64 << 20

// holdRequest keeps r's write whole beside parent, a held text it names, with names, the issues its
// body names (heldBodyNames), and answers 202.
func (s *server) holdRequest(w http.ResponseWriter, r *http.Request, route string, parent *heldParent, names []string) {
	ctx := r.Context()
	caller, err := s.captureCaller(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	method, pattern, _ := strings.Cut(route, " ")
	limit, _ := routeBody(method, pattern)
	if limit <= 0 || limit > heldRequestBodyMax {
		limit = heldRequestBodyMax
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if int64(len(body)) > limit {
		writeError(w, "BODY_TOO_LARGE", http.StatusRequestEntityTooLarge, "the request body is larger than this route takes")
		return
	}
	replay := heldReplay{
		Replay: true, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		ContentType: r.Header.Get("Content-Type"), Body: body, Caller: caller, PersonTyped: personTypedWrite(r),
		Names: names,
	}
	request, err := json.Marshal(replay)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	author, err := json.Marshal(caller.Actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	id := uuid.NewString()
	// The kept write is the request's own write: it commits in a transaction of the shared pool,
	// whose commit step marks the request's Idempotency-Key with it, so a repeat after a crash is
	// answered KEY_COMMITTED and keeps no second write (keyed_write_guard_test.go).
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into held_texts (id, kind, route, request, original, actor, writer, reader, project, state,
			reviewed, outcome, depends_on, person_typed)
		values ($1, 'request', $2, $3, '{}', $4, nullif($5, ''), nullif($6, ''), nullif($7, ''), 'posting',
			false, 'not_reviewed', $8, $9)`,
		id, route, request, author, writerOf(caller.Actor), parent.reader, parent.project, parent.id, replay.PersonTyped); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Its parent may have posted while this write was kept beside it.
	s.detached.goDetached(s.postReady)
	// The answer is no shape of the route's: its client said it reads it (awaitHeldTarget).
	WriteJSON(w, http.StatusAccepted, heldDeferred{ID: id, Held: heldState{State: heldPosting, WaitsOn: parent.id}})
}

// postReplay runs the kept write of row, the held text's post (held), and marks row posted in the
// transaction of the write itself, so the write and its mark commit together or not at all and
// the write runs once: a reply's route takes the held row in its own transaction and marks it
// there (heldPost.lock, finish), and any other write's route takes markKeptWritePosted as the
// commit step of each transaction it opens on ctx, the post context (postHeld says why). A route
// that answered 2xx having written nothing has its row marked after it, in a transaction of its
// own, since running it again writes nothing either. A route's 4xx is the post's refusal.
func (s *server) postReplay(ctx context.Context, row heldRow, replay heldReplay, held *heldPost) error {
	if row.kind == heldRequestKind {
		ctx = store.WithBeforeCommit(ctx, markKeptWritePosted(row.id, &atomic.Bool{}))
	}
	answer, err := s.runReplay(ctx, replay, held)
	if err != nil {
		return err
	}
	switch {
	case answer.status >= 200 && answer.status < 300:
		ctx = store.WithoutBeforeCommit(ctx)
		tx, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, postedHeldSQL, row.id, heldPosting); err != nil {
			return err
		}
		return tx.Commit(ctx)
	case answer.status >= 400 && answer.status < 500:
		refused := answer.refusal()
		if refused.code == errHeldNotPosting.code {
			// The route's commit found the row posted by another post, or failed, and wrote
			// nothing (markKeptWritePosted, heldPost.lock).
			return errHeldNotPosting
		}
		return refused
	}
	return fmt.Errorf("the kept write %s answered %d: %s", row.route, answer.status, strings.TrimSpace(answer.body.String()))
}

// markKeptWritePosted is the commit step (store.BeforeCommit) of each transaction the route of
// the kept write id opens while its post runs it: the first transaction that wrote - it holds the
// ROW EXCLUSIVE lock a write takes on a table other than held_texts, as markRequestKeyCommitted
// reads it - marks the held row posted in that same transaction, so the write and the mark commit
// together or not at all, and a process that stops after the commit leaves no row for the sweep to
// run again. A transaction that finds the row no longer posting (a second post took it past
// staleClaim and posted it first, or it failed) is refused with errHeldNotPosting and its write
// rolls back, so the write runs once. A transaction that only read marks nothing, and once marked
// takes no step.
func markKeptWritePosted(id string, marked *atomic.Bool) store.BeforeCommit {
	return func(ctx context.Context, tx pgx.Tx) error {
		if marked.Load() {
			return nil
		}
		var wrote, posted bool
		if err := tx.QueryRow(ctx, `
			with wrote as (
				select txid_current_if_assigned() is not null and exists (
					select 1 from pg_locks l join pg_class c on c.oid = l.relation
					where l.pid = pg_backend_pid() and l.locktype = 'relation' and l.mode = 'RowExclusiveLock'
						and c.relkind in ('r', 'p') and c.oid <> 'held_texts'::regclass
				) as yes
			), posted as (
				update held_texts set state = 'posted', posted_at = now(), claimed_at = null, updated_at = now()
				where id = $1 and state = 'posting' and (select yes from wrote)
				returning 1
			)
			select (select yes from wrote), exists (select 1 from posted)`, id).Scan(&wrote, &posted); err != nil {
			return fmt.Errorf("mark kept write %s posted: %w", id, err)
		}
		if !wrote {
			return nil
		}
		if !posted {
			return errHeldNotPosting
		}
		marked.Store(true)
		return nil
	}
}

// replayAnswerOf is the answer a held reply's write gets: the reply route's own shape, built
// from what the hold stored under the reserved id, and from dry, never nil: where the hold's dry
// run ran the route, the reply it wrote - a comment reply's issue or document, what a held
// comment states of its dry run (heldCommentWrites) and whom the ask of its thread waits on once
// it posts (ask_waiting_on); a message reply's issue, target and the message it answers, a
// follow-up's being the first reply it is filed under, and what a held message states of its dry
// run (heldMessageWrites). Every held reply runs its dry run but one: a follow-up whose attempt
// its writer's earlier held reply to the same delivery answers (heldWaitedBeside), whose dry
// carries what heldFollowUpBeside read off the held replies it waits on - the reply it is filed
// under, the delivery's issue and target - from the one reading that chose it. A reply that waits
// only for its turn at its place (heldWriteOf.before), or on an earlier held reply that answers
// another attempt (heldWaitedBeside), runs its dry run.
func replayAnswerOf(kind reviewer.Kind, replay heldReplay, id string, actor model.Actor, at time.Time, dry *heldDryRun, trees bool) any {
	parent := replay.Subject.InReplyTo
	if kind == reviewer.KindComment {
		parent = replay.Subject.ReplyTo
		comment := model.Comment{
			ID: id, Author: actor, Body: replay.Text.Body, CreatedAt: at,
			Mentions: []model.Mention{}, Deliveries: []model.CommentDelivery{},
		}
		if parent != "" {
			comment.ReplyTo = &parent
		}
		if replay.Subject.IssueKey != "" {
			comment.IssueKey = &replay.Subject.IssueKey
		}
		if wrote := dry.comment; wrote != nil {
			comment.IssueKey, comment.ArtifactID = wrote.IssueKey, wrote.ArtifactID
			heldCommentWrites(&comment, wrote)
		}
		return commentWriteResponse{shapedComment: newShapedComment(comment, trees), AskWaitingOn: dry.askWaitingOn}
	}
	message := model.Message{ID: id, Author: actor, Body: replay.Text.Body, CreatedAt: at, Deliveries: []model.MessageDelivery{}}
	if parent != "" {
		message.InReplyTo = &parent
	}
	if replay.Subject.IssueKey != "" {
		message.IssueKey = &replay.Subject.IssueKey
	}
	if wrote := dry.message; wrote != nil {
		message.IssueKey, message.Target, message.InReplyTo = wrote.IssueKey, wrote.Target, wrote.InReplyTo
		heldMessageWrites(&message, wrote)
	}
	return replyRead{shapedMessage: newShapedMessage(message, trees)}
}

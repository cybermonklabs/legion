package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
)

// held_routes.go serves a held text's own routes (INFRA-3231's R5, Decision 6 of its plan):
// GET /api/v1/held/{id} reads it, PATCH /api/v1/held/{id} edits it, which the reviewer reads
// again, and PUT /api/v1/held/{id}/state {state: posted} posts it as written with the writer's
// reason. Any agent's token reads a held text and no person does; only its writer session edits
// or posts it.

// wordingOf is text's wording as a read gives it (model.TextWording): an ask's question, its
// options and its choice, an issue's title and spec, and a comment's or a message's body. A held
// text's read, the draft route and a rewrite's original all give it so.
func wordingOf(text reviewer.Text) model.TextWording {
	return model.TextWording{Title: text.Title, Body: text.Body, Options: text.Options, Multiple: text.Multiple}
}

// reviewFinding is one finding of the reviewer: the rule set, the words, why they break it and
// the words to write in their place.
type reviewFinding struct {
	RuleSet string `json:"rule_set"`
	Quote   string `json:"quote"`
	Why     string `json:"why"`
	Fix     string `json:"fix"`
}

func findingsOf(findings []reviewer.Finding) []reviewFinding {
	read := make([]reviewFinding, len(findings))
	for i, finding := range findings {
		read[i] = reviewFinding{RuleSet: string(finding.RuleSet), Quote: finding.Quote, Why: finding.Why, Fix: finding.Fix}
	}
	return read
}

// heldRead is GET /api/v1/held/{id}'s answer: the held text, what holds it, and what the reviewer
// found in it.
type heldRead struct {
	ID string `json:"id"`
	// Kind is ask, issue, comment or message.
	Kind string `json:"kind"`
	// State is reviewing, awaiting_writer, posting, posted or failed.
	State string `json:"state"`
	// Route is the write that held it, "<method> <pattern>".
	Route  string `json:"route"`
	Writer string `json:"writer"`
	Reader string `json:"reader"`
	// IssueKey is the issue the text is on, or a new issue's parent; "" for none.
	IssueKey string `json:"issue_key,omitempty"`
	// Wording is the text as it posts now, the writer's edits applied; Original is the text as the
	// writer first wrote it.
	Wording  model.TextWording `json:"wording"`
	Original model.TextWording `json:"original"`
	// Findings are the last review's, Rewrite its rewrite (null for none) and NotApplied why the
	// rewrite cannot post, "" where it can.
	Findings   []reviewFinding    `json:"findings"`
	Rewrite    *model.TextWording `json:"rewrite"`
	NotApplied string             `json:"not_applied,omitempty"`
	// Revision counts the writer's edits, from 1.
	Revision int `json:"revision"`
	// Deadline is when the writer's time to act ends, null while none runs.
	Deadline *time.Time `json:"deadline"`
	// Outcome is what posts or posted: passed, posted_as_written, rewrite_posted,
	// posted_unreviewed or not_reviewed; "" while undecided.
	Outcome string `json:"outcome,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// WaitsOn is the held text this one posts after, "" for none.
	WaitsOn   string     `json:"waits_on,omitempty"`
	WrittenAt time.Time  `json:"written_at"`
	PostedAt  *time.Time `json:"posted_at"`
}

func (h heldRow) read() heldRead {
	read := heldRead{
		ID: h.id, Kind: h.kind, State: h.state, Route: h.route, Writer: h.writer, Reader: h.reader, IssueKey: h.issueKey,
		Original: wordingOf(h.original), Findings: findingsOf(h.findings), NotApplied: h.notApplied, Revision: h.revision,
		Deadline: h.deadline, Outcome: h.outcome, Reason: h.reason, Detail: h.detail, WaitsOn: h.dependsOn,
		WrittenAt: h.writtenAt, PostedAt: h.postedAt,
	}
	read.Wording = read.Original
	if write, err := h.write(); err == nil {
		read.Wording = wordingOf(write.text)
	}
	if h.rewrite != nil {
		rewrite := wordingOf(*h.rewrite)
		read.Rewrite = &rewrite
	}
	return read
}

// refuseHeldToPerson answers 403 HELD_FORBIDDEN to a person signed in at a browser: no person
// reads a held text before it posts. It reports whether it refused, and writes the answer to a
// caller that does not resolve.
func (s *server) refuseHeldToPerson(w http.ResponseWriter, r *http.Request) bool {
	_, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return true
	}
	if human {
		writeError(w, "HELD_FORBIDDEN", http.StatusForbidden, "a held text is read by agents' tokens: no person reads it before it posts")
		return true
	}
	return false
}

// getHeld is GET /api/v1/held/{id}.
func (s *server) getHeld(w http.ResponseWriter, r *http.Request) {
	if s.refuseHeldToPerson(w, r) {
		return
	}
	row, err := s.loadHeld(r.Context(), s.deps.Store.Pool, r.PathValue("id"), false)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, row.read())
}

// heldReadRoutes are the reads by id of a text the reviewer may hold, each with the path value that
// names the text.
var heldReadRoutes = map[string]string{
	"GET /api/v1/asks/{id}":     "id",
	"GET /api/v1/comments/{id}": "id",
	"GET /api/v1/messages/{id}": "id",
	"GET /api/v1/issues/{key}":  "key",
}

// heldTextError is the 409 HELD_TEXT answer: the text a session's read by its id, or a session's
// write whose path names it from a client that reads no kept write (awaitHeldTarget), finds held,
// under HeldID, which GET /api/v1/held/{id} reads.
type heldTextError struct {
	Error  string `json:"error"`
	Code   string `json:"code"`
	HeldID string `json:"held_id"`
}

// answerHeldRead answers a session's read by id of a held ask, comment, message or issue with 409
// HELD_TEXT and the held id (Decision 4 of INFRA-3231's plan): the text exists for every agent, and
// GET /api/v1/held/{id} reads it. A person's read is the route's own, which finds no such text: no
// person reads a held text before it posts. Any other read is not wrapped.
func (s *server) answerHeldRead(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	param, held := heldReadRoutes[route.Method+" "+route.Pattern]
	if !held {
		return handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.deps.Reviewer.reviewsAny() || s.deps.Store == nil {
			handler(w, r)
			return
		}
		id := r.PathValue(param)
		var isHeld bool
		if err := s.deps.Store.Pool.QueryRow(r.Context(), `
			select exists (
				select 1 from held_texts
				where id = $1 and state in `+heldOpenStatesSQL+` and kind not in ('request', 'decision_block'))`,
			id).Scan(&isHeld); err != nil || !isHeld {
			handler(w, r)
			return
		}
		if _, human, err := s.optionalActor(r); err != nil || human {
			handler(w, r)
			return
		}
		WriteJSON(w, http.StatusConflict, heldTextError{
			Error: "this text is held by the reviewer and has not posted; read it with GET /api/v1/held/" + id,
			Code:  "HELD_TEXT", HeldID: id,
		})
	}
}

// heldOption is one option of an ask a held text's edit or a draft writes: a label and what it
// causes.
type heldOption struct {
	Label       string `json:"label" required:"true"`
	Description string `json:"description"`
}

// askOptionsOf is options as an ask's, with no id: the index mints an option's id.
func askOptionsOf(options []heldOption) []model.AskOption {
	asked := make([]model.AskOption, len(options))
	for i, option := range options {
		asked[i] = model.AskOption{Label: option.Label, Description: option.Description}
	}
	return asked
}

// heldEditInput is PATCH /api/v1/held/{id}'s body: the new wording, each field of the held text's
// kind alone. An ask takes its question and its options, an issue its title and its spec, and a
// comment or a message its body.
type heldEditInput struct {
	Question *string       `json:"question"`
	Options  *[]heldOption `json:"options"`
	Title    *string       `json:"title"`
	Spec     *string       `json:"spec"`
	Body     *string       `json:"body"`
	Actor    *model.Actor  `json:"actor"`
}

// heldStatePosted is the one state PUT /api/v1/held/{id}/state moves a held text to: posted as
// its writer wrote it.
const heldStatePosted = "posted"

// heldStateInput is PUT /api/v1/held/{id}/state's body: the state the writer moves its held text
// to, posted, and why the text is right for its reader as written.
type heldStateInput struct {
	State  string       `json:"state" required:"true" enum:"posted"`
	Reason string       `json:"reason" required:"true"`
	Actor  *model.Actor `json:"actor"`
}

// isWriter reports whether actor is the held text's writer session: the session itself, or a
// person's credential acting for it.
func (h heldRow) isWriter(actor model.Actor) bool {
	return h.writer != "" && writerOf(actor) == h.writer
}

// takeHeldForWriter reads the held text id under its row lock in tx for its writer: a caller that
// is not its writer is 403 HELD_NOT_WRITER, and a text no longer held for the writer (decided,
// posted or failed) is 409 HELD_TEXT_DECIDED.
func (s *server) takeHeldForWriter(ctx context.Context, tx queryer, id string, actor model.Actor) (heldRow, error) {
	row, err := s.loadHeld(ctx, tx, id, true)
	if err != nil {
		return heldRow{}, err
	}
	if !row.isWriter(actor) {
		return heldRow{}, errorf(http.StatusForbidden, "HELD_NOT_WRITER", "only the session that wrote held text %s edits or posts it", id)
	}
	if row.state != heldReviewing && row.state != heldAwaitingWriter {
		return heldRow{}, errorf(http.StatusConflict, "HELD_TEXT_DECIDED", "held text %s is %s: what posts of it is decided", id, row.state)
	}
	return row, nil
}

// editHeld is PATCH /api/v1/held/{id}: the writer's edit of its held text. The edit is checked as
// the kind's create checks it, kept as the text's wording, recorded edited, and reviewed again.
func (s *server) editHeld(w http.ResponseWriter, r *http.Request) {
	var input heldEditInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	ctx := r.Context()
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	row, err := s.takeHeldForWriter(ctx, tx, r.PathValue("id"), actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if row.kind == string(reviewer.KindDecisionBlock) {
		_ = tx.Rollback(ctx)
		s.editHeldDecisionBlock(w, r, row, input)
		return
	}
	write, err := row.write()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	edited, err := editedRequest(write, input)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	request, err := json.Marshal(edited)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `
		update held_texts
		set request = $2, revision = revision + 1, state = 'reviewing', findings = '[]', rewrite = null,
			not_applied = null, deadline = null, claimed_at = null, updated_at = now()
		where id = $1`, row.id, request); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.recordHeld(ctx, row, reviewEdited, nil, "")
	s.startReview(row.id)
	updated, err := s.loadHeld(ctx, s.deps.Store.Pool, row.id, false)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, updated.read())
}

// editHeldDecisionBlock is PATCH /api/v1/held/{id} for a held decision block: its wording lives in
// its document's `:::ask` block, so the edit runs as the writer's PATCH /api/v1/asks/{id}, which
// writes the block and the row and takes it as the writer's edit of the held text, reviewed again
// (editedHeldDecisionBlock). A block takes its question and its options alone.
func (s *server) editHeldDecisionBlock(w http.ResponseWriter, r *http.Request, row heldRow, input heldEditInput) {
	if input.Title != nil || input.Spec != nil || input.Body != nil {
		writeError(w, "HELD_EDIT_INPUT", http.StatusBadRequest, "a decision block takes its question and its options alone")
		return
	}
	if input.Question == nil && input.Options == nil {
		writeError(w, "HELD_EDIT_INPUT", http.StatusBadRequest, "the edit changes nothing: give question, options or both")
		return
	}
	edit := map[string]any{}
	if input.Question != nil {
		edit["question"] = *input.Question
	}
	if input.Options != nil {
		edit["options"] = askOptionsOf(*input.Options)
	}
	if input.Actor != nil {
		edit["actor"] = input.Actor
	}
	body, err := json.Marshal(edit)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	caller, err := s.captureCaller(unloggedResolution(r))
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	answer, err := s.runReplay(r.Context(), heldReplay{
		Replay: true, Method: http.MethodPatch, Path: "/api/v1/asks/" + row.id, ContentType: "application/json",
		Body: body, Caller: caller,
	}, nil)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if answer.status >= 300 {
		answer.writeTo(w)
		return
	}
	updated, err := s.loadHeld(r.Context(), s.deps.Store.Pool, row.id, false)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, updated.read())
}

// editedRequest is write's request with input's wording in place of its own, checked as the
// kind's create checks it. A field the kind does not hold is 400 HELD_EDIT_INPUT, and so is an
// edit that changes nothing.
func editedRequest(write reviewedWrite, input heldEditInput) (any, error) {
	refuse := func(format string, args ...any) error {
		return errorf(http.StatusBadRequest, "HELD_EDIT_INPUT", format, args...)
	}
	switch create := write.request.(type) {
	case askCreate:
		if input.Title != nil || input.Spec != nil || input.Body != nil {
			return nil, refuse("an ask's edit takes its question and its options")
		}
		if input.Question == nil && input.Options == nil {
			return nil, refuse("an ask's edit takes its question, its options or both")
		}
		if input.Question != nil {
			if err := validateAskQuestion(*input.Question); err != nil {
				return nil, err
			}
			create.Question = *input.Question
		}
		if input.Options != nil {
			options := askOptionsOf(*input.Options)
			if err := validateAskOptions(options); err != nil {
				return nil, err
			}
			create.Options = options
		}
		if err := validateAskWording(create.Question, create.Options); err != nil {
			return nil, err
		}
		return create, nil
	case issueCreate:
		if input.Question != nil || input.Options != nil || input.Body != nil {
			return nil, refuse("an issue's edit takes its title and its spec")
		}
		if input.Title == nil && input.Spec == nil {
			return nil, refuse("an issue's edit takes its title, its spec or both")
		}
		if input.Title != nil {
			title := strings.TrimSpace(*input.Title)
			if title == "" {
				return nil, errorf(http.StatusBadRequest, "INVALID_ISSUE", "title is required")
			}
			if length := itemblocks.Units16(title); length > contracts.IssueTitleMax {
				return nil, capExceededError("title", length, contracts.IssueTitleMax)
			}
			create.Title = title
		}
		if input.Spec != nil {
			spec := *input.Spec
			create.Spec = &spec
		}
		return create, nil
	case commentCreate:
		if input.Question != nil || input.Options != nil || input.Title != nil || input.Spec != nil || input.Body == nil {
			return nil, refuse("a comment's edit takes its body")
		}
		body := pmdoc.LineFeeds(*input.Body)
		if create.Input.Suggestion == nil && strings.TrimSpace(body) == "" {
			return nil, errorf(http.StatusBadRequest, "INVALID_COMMENT", "comment body is required")
		}
		if err := blockBoundError(itemblocks.Check("body", body)); err != nil {
			return nil, err
		}
		create.Input.Body = body
		return create, nil
	case messageCreate:
		if input.Question != nil || input.Options != nil || input.Title != nil || input.Spec != nil || input.Body == nil {
			return nil, refuse("a message's edit takes its body")
		}
		create.Input.Body = *input.Body
		delivery, err := validateCreateMessage(&create.Input)
		if err != nil {
			return nil, err
		}
		create.Delivery = delivery
		return create, nil
	case heldReplay:
		if create.Text.Body == "" || input.Question != nil || input.Options != nil || input.Title != nil || input.Spec != nil || input.Body == nil {
			return nil, refuse("a held reply's edit takes its body")
		}
		body := pmdoc.LineFeeds(*input.Body)
		if strings.TrimSpace(body) == "" {
			return nil, refuse("a reply's body is required")
		}
		if err := blockBoundError(itemblocks.Check("body", body)); err != nil {
			return nil, err
		}
		return create.withBody(body)
	}
	return nil, refuse("this held text takes no edit")
}

// putHeldState is PUT /api/v1/held/{id}/state {state: posted}: the writer posts its held text as
// written, over the reviewer's findings, with the reason it gives, which is recorded
// posted_as_written. The text posts before the answer, which reads it as it then stands. A repeat
// of the same request, once that post is decided, writes nothing and answers the held text as it
// then stands, as a PUT does.
func (s *server) putHeldState(w http.ResponseWriter, r *http.Request) {
	var input heldStateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if input.State != heldStatePosted {
		writeError(w, "HELD_STATE_INPUT", http.StatusBadRequest,
			fmt.Sprintf("state must be %s: a writer moves its held text to posted alone", heldStatePosted))
		return
	}
	reason := strings.TrimSpace(input.Reason)
	if reason == "" {
		writeError(w, "HELD_POST_INPUT", http.StatusBadRequest, "reason is required: say why the text is right for its reader as written")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	row, err := s.takeHeldForWriter(ctx, tx, r.PathValue("id"), actor)
	if err != nil {
		if repeat, ok := s.repeatedHeldPost(ctx, tx, r.PathValue("id"), actor, reason, err); ok {
			WriteJSON(w, http.StatusOK, repeat.read())
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `
		update held_texts
		set state = 'posting', outcome = 'posted_as_written', reason = $2, detail = null, deadline = null,
			claimed_at = null, updated_at = now()
		where id = $1`, row.id, reason); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.recordHeld(ctx, row, reviewPostedAsWritten, findingRuleSets(row.findings), reason)
	s.postReady()
	posted, err := s.loadHeld(ctx, s.deps.Store.Pool, row.id, false)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, posted.read())
}

// repeatedHeldPost reads held text id in tx, the request's own transaction, once
// takeHeldForWriter refused it decided (refused), and answers it where the refused request
// repeats the writer's post that decided it: the same writer posting it as written with the same
// reason. Every other refusal stands.
func (s *server) repeatedHeldPost(ctx context.Context, tx queryer, id string, actor model.Actor, reason string, refused error) (heldRow, bool) {
	var decided *apiError
	if !errors.As(refused, &decided) || decided.code != "HELD_TEXT_DECIDED" {
		return heldRow{}, false
	}
	row, err := s.loadHeld(ctx, tx, id, false)
	if err != nil || !row.isWriter(actor) || row.outcome != reviewPostedAsWritten || row.reason != reason {
		return heldRow{}, false
	}
	return row, true
}

// answerHeldFrame takes a reply to the frame Dispatch sent a held text's writer
// (sendFindingsFrame), which names the held text's id where a delivered message's id stands: the
// reply is the writer's answer, kept with the held text, and writes no message row and no event.
// It reports whether id is a held text's, having answered the request; a held text's writer alone
// replies to it. A held message or reply to a message that has posted is a message under the same
// id, delivered as any other, so a reply to it is its recipient's, which the route takes as it
// takes any reply; a held text of any other kind, or one that failed, is no message, and its
// writer's answer to the frame is kept with it whatever its state.
func (s *server) answerHeldFrame(w http.ResponseWriter, r *http.Request, actor model.Actor, input messageReplyInput) bool {
	if s.deps.Reviewer == nil || s.deps.Store == nil {
		return false
	}
	ctx := r.Context()
	row, err := s.loadHeld(ctx, s.deps.Store.Pool, r.PathValue("id"), false)
	if err != nil || (row.state == heldPosted && row.kind == string(reviewer.KindMessage)) {
		return false
	}
	if !row.isWriter(actor) {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "only the session that wrote a held text answers its findings")
		return true
	}
	answer := ""
	switch {
	case input.Body != nil:
		answer = *input.Body
	case input.Error != nil:
		answer = "error: " + *input.Error
	}
	// The answer is the request's own write: it commits in a transaction of the shared pool, whose
	// commit step marks the request's key with it (keyed_write_guard_test.go).
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return true
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		update held_texts set writer_answer = $2, updated_at = now() where id = $1`, row.id, answer); err != nil {
		s.writeHandlerError(w, err)
		return true
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return true
	}
	row.answer = answer
	WriteJSON(w, http.StatusAccepted, row.read())
	return true
}

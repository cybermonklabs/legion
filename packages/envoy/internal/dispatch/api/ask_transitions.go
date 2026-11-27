package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// askTransition is one way of closing or changing an ask's answer. Its writes are the
// transaction's (ledger): Apply and After make them in the order they run, each queued on
// ledger when the caller joined one, and the events After returns are filled in once Commit has run
// the write phase (queueEvent).
type askTransition struct {
	EventType      string
	ReplacesAnswer *time.Time
	Apply          func(context.Context, pgx.Tx, *docs.Ledger, model.Ask) (model.Ask, error)
	// Payload builds the event's payload from the one every transition carries (the ask after
	// the transition, whose thread_logins the event's insert reads); nil appends that payload as
	// it is.
	Payload func(model.AskEventPayload) any
	// After runs once the transition's own event is appended, still inside the
	// transaction; the events it returns are published with it.
	After func(context.Context, pgx.Tx, *docs.Ledger, model.Ask) ([]*model.Event, error)
}

// answerRevision is the question revision a human reviewed before choosing an answer.
// A nil EditedAt represents an ask that had not yet been edited.
type answerRevision struct {
	EditedAt *string
}

// answerOptions are what an answer write carries beyond its selection and text, each optional:
// Revision is the question revision the human reviewed (the answer route always sends one),
// ReplacesAnswer the `at` of the current answer a change replaces, and WriteBlock writes a block
// ask's answer into its document.
type answerOptions struct {
	Revision       *answerRevision
	ReplacesAnswer *time.Time
	WriteBlock     func(context.Context, pgx.Tx, model.Ask, model.AskAnswer) error
}

// answerTransition records a human answer. Approval asks have their review options and always name
// the document's latest settled version, since every version write moves the open request to it
// (docs.MoveApprovalAsk) and stamps edited_at, so the revision, which the answer route requires,
// refuses an answer to a version the human was not shown (ASK_EDITED). Questions accept their
// configured options.
func answerTransition(actor model.Actor, selected []string, text *string, options answerOptions) askTransition {
	// A block ask's answer is written into its document, where text reaches the parser with line
	// feeds alone (pmdoc.LineFeeds); the ask's row takes the same text, so settlement, which writes
	// the row's answer onto the block, agrees with it.
	if text != nil {
		lined := pmdoc.LineFeeds(*text)
		text = &lined
	}
	var previousAnswer *model.AskAnswer
	return askTransition{
		EventType:      "ask.answered",
		ReplacesAnswer: options.ReplacesAnswer,
		Apply: func(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, ask model.Ask) (model.Ask, error) {
			if options.Revision != nil && !stringPointersEqual(options.Revision.EditedAt, ask.EditedAt) {
				return model.Ask{}, errorf(
					apicode.AskEdited,
					"the question changed after you reviewed it; review the latest version and confirm your answer",
				)
			}
			if ask.State == "answered" {
				switch {
				case ask.Kind == "approval":
					return model.Ask{}, errorf(apicode.AskApprovalReview, "an approval is a review of a document version; record a new review on the document instead")
				case !ask.Answer.At.Equal(*options.ReplacesAnswer):
					return model.Ask{}, errorf(apicode.AskAnswerChanged, "the answer changed after you reviewed it; review the current answer and confirm your change")
				case model.CanonicalLogin(ask.Answer.User) != model.CanonicalLogin(actor.ID):
					return model.Ask{}, errorf(apicode.NotAnswerer, "only the person who gave the current answer may change it")
				}
				previousAnswer = ask.Answer
			}
			hasText := text != nil && strings.TrimSpace(*text) != ""
			switch ask.Kind {
			case "approval":
				if _, _, err := reviewFromAnswer(selected, text); err != nil {
					return model.Ask{}, err
				}
			default:
				if !ask.Multiple && len(selected) > 1 {
					return model.Ask{}, errorf(apicode.InvalidAnswer, "single-select asks accept at most one selected answer")
				}
				if len(selected) > 0 && !selectedOptions(ask.Options, selected) {
					return model.Ask{}, errorf(apicode.InvalidAnswer, "selected answers must be ask option labels")
				}
				if len(selected) == 0 && !hasText {
					return model.Ask{}, errorf(apicode.InvalidAnswer, "answer requires a selected option or free-text answer")
				}
			}
			// An answer that chose no option stores an empty list, never null.
			if selected == nil {
				selected = []string{}
			}
			// The transaction's now(), read under the ask's owner lock, which this route took
			// before its reads (transitionAskTx), so it is the created_at of this answer's
			// ask.answered event too. Each process's host keeps its own clock, and this `at` is
			// compared with times the database stamped: the carry rule holds it against an
			// approval's created_at (docs.carryVerdict).
			var answeredAt time.Time
			if err := tx.QueryRow(ctx, `select now()`).Scan(&answeredAt); err != nil {
				return model.Ask{}, err
			}
			// Act comes from the actor actingPerson handed the handler: the answer is the
			// person's, and the agent that made it for them is named beside it. A browser's
			// actor carries none, and so does a write of docs' own (the carry's answer).
			answer := model.AskAnswer{User: actor.ID, Selected: selected, Text: text, At: answeredAt.UTC(), Act: actor.Act}
			if options.WriteBlock != nil {
				if err := options.WriteBlock(ctx, tx, ask, answer); err != nil {
					return model.Ask{}, err
				}
			}
			answerJSON, err := encodeJSON(answer)
			if err != nil {
				return model.Ask{}, err
			}
			if err := queueExec(ledger, `update asks set state = 'answered', answer = $2 where id = $1`, ask.ID, answerJSON); err != nil {
				return model.Ask{}, err
			}
			ask.State = "answered"
			ask.Answer = &answer
			return ask, nil
		},
		Payload: func(payload model.AskEventPayload) any {
			return model.AskAnsweredEventPayload{
				AskEventPayload: payload,
				PreviousAnswer:  previousAnswer,
			}
		},
	}
}

// parseAnswerPrecondition decodes one of an answer's tri-state precondition fields, named field.
// Absent → (nil, false): no precondition. JSON null → (nil, true): the precondition is that the
// value is unset. A string → it. Anything else → 400 INVALID_ANSWER.
func parseAnswerPrecondition(field string, raw json.RawMessage) (value *string, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, true, errorf(apicode.InvalidAnswer, "%s must be an RFC3339 timestamp or null", field)
	}
	return &text, true, nil
}

// parseReplacesAnswer reads expected_answer_at, the `at` of the current answer a change replaces:
// nil when absent or null (a first answer), else an RFC 3339 timestamp.
func parseReplacesAnswer(raw json.RawMessage) (*time.Time, error) {
	text, _, err := parseAnswerPrecondition("expected_answer_at", raw)
	if err != nil || text == nil {
		return nil, err
	}
	value, err := time.Parse(time.RFC3339Nano, *text)
	if err != nil {
		return nil, errorf(apicode.InvalidAnswer, "expected_answer_at must be an RFC3339 timestamp or null")
	}
	return &value, nil
}

// answerAskInput is the body of POST /api/v1/asks/{id}/answer, and the fields of PUT
// /api/v2/asks/{id}/state's answered arm (answerAskStateInput).
type answerAskInput struct {
	Selected []string `json:"selected"`
	Text     *string  `json:"text"`
	// ExpectedEditedAt is required: an answer names the wording it answers, and a body without
	// the field is refused (400 ASK_REVISION_REQUIRED, recordAnswer below). Null is the
	// precondition that the ask has never been edited, so the field is required and nullable
	// both, which is why the handler holds the bytes unparsed (rawFieldSchemas).
	ExpectedEditedAt json.RawMessage `json:"expected_edited_at" required:"true"`
	ExpectedAnswerAt json.RawMessage `json:"expected_answer_at"`
	Actor            *model.Actor    `json:"actor"`
}

func (s *server) answerAsk(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	if !requireUUIDPath(w, r, "ask", apicode.AskIDInput) {
		return
	}
	var input answerAskInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	ask, ok := s.recordAnswer(w, r, actor, input, nil)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, newShapedAsk(ask, wantsDisplayTrees(r)))
}

// recordAnswer answers the ask the path names as input says, for actor, once the route has read
// its body: the first answer, or a change of the current one by the person who gave it, through
// the transition engine (closeAsk), with an approval's reviews. POST /api/v1/asks/{id}/answer
// (answerAsk) and PUT /api/v2/asks/{id}/state with state answered (putAskStateV2) each answer it
// in their own shape, the /api/v2 route reading its own on the write's transaction (readBack). It
// writes a refusal itself and answers false.
func (s *server) recordAnswer(w http.ResponseWriter, r *http.Request, actor model.Actor, input answerAskInput, readBack askReadBack) (model.Ask, bool) {
	// The acting session the body names, where the caller's credential proved none: both routes
	// decode an actor, and a personal token names its session nowhere else.
	actor, ok := actSessionFromBody(w, r, actor, input.Actor)
	if !ok {
		return model.Ask{}, false
	}
	// Before the transition opens its own transaction: an answer's text rides the ask.answered
	// event beside the ask's wording, so it takes the same bound.
	if input.Text != nil {
		if length := itemblocks.Units16(*input.Text); length > contracts.DispatchEventBodyMax {
			capExceeded(w, r, "text", length, contracts.DispatchEventBodyMax)
			return model.Ask{}, false
		}
	}
	expectedEditedAt, provided, err := parseAnswerPrecondition("expected_edited_at", input.ExpectedEditedAt)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return model.Ask{}, false
	}
	if !provided {
		writeError(w, r, apicode.AskRevisionRequired, "expected_edited_at is required")
		return model.Ask{}, false
	}
	replacesAnswer, err := parseReplacesAnswer(input.ExpectedAnswerAt)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return model.Ask{}, false
	}
	transition := answerTransition(actor, input.Selected, input.Text, answerOptions{
		Revision:       &answerRevision{EditedAt: expectedEditedAt},
		ReplacesAnswer: replacesAnswer,
		WriteBlock: func(ctx context.Context, tx pgx.Tx, ask model.Ask, answer model.AskAnswer) error {
			if ask.BlockID == nil {
				return nil
			}
			blockArtifact, err := blockArtifactOf(ask)
			if err != nil {
				return err
			}
			// A copy of this ask on another document of its owner shows its state, so the
			// answer settles each copy again; the copies' pending rows are taken before this
			// document's (docs.SettleCopiesOf).
			if err := s.deps.Docs.SettleCopiesOf(ctx, ask); err != nil {
				return err
			}
			var answerText any
			if answer.Text != nil {
				answerText = *answer.Text
			}
			attributes := map[string]any{
				"state":       "answered",
				"answered_by": answer.User,
				"answered_at": timestampValue(answer.At),
				"selected":    answer.Selected,
				"answer":      answerText,
			}
			return s.deps.Docs.SetBlockAttributes(ctx, blockArtifact, *ask.BlockID, attributes, actor)
		},
	})
	// The approve scope an approval request's answer needs (approvalAnswerRefusal) cannot be
	// checked at the gate: an ask's kind is known only once its row is read, and reading it before
	// the transaction would decide on a row another writer could still move. It sits here instead,
	// inside Apply, under the ask's owner lock (requireOpenOwner in transitionAskTx), which every
	// writer of an ask takes first and which the transaction holds until Commit writes the queued
	// answer, so the kind checked is the kind answered.
	//
	// An approve-only token (model.Credential.ApproveOnly) reaches this answer because the approve
	// list names it on both versions (POST /api/v1/asks/{id}/answer, and PUT /api/v2/asks/{id}/state
	// for the state answered alone: approveList) for the answer of an approval request, and
	// for nothing else: an answer of any other ask, a question's free text among them, is no
	// approval, so it is refused here (approveOnlyAnswerRefusal), under the same lock and for the
	// same reason, with the approve-only checks' own code. This is the check of the write itself. A
	// keyed answer is refused before that, ahead of the keyed layer, so no key is looked up, claimed
	// or replayed for an ask the token may not answer (gateApproveOnlyAskAnswer).
	approveOnly := approveOnlyRequest(r)
	applyAnswer := transition.Apply
	transition.Apply = func(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, ask model.Ask) (model.Ask, error) {
		if err := approveOnlyAnswerRefusal(approveOnly, ask.Kind); err != nil {
			return model.Ask{}, err
		}
		if err := approvalAnswerRefusal(ask, actor); err != nil {
			return model.Ask{}, err
		}
		return applyAnswer(ctx, tx, ledger, ask)
	}
	// An approval ask's answer is a review of every document it names, each pinned to the version
	// its question named: that document's latest settled one, which every version write moves the
	// request to, and the one the human reviewed (revision). A request naming a spec and its
	// issue's Verification document writes one review on each. The reviews are queued on the
	// ledger in the order the request names the documents (queueApprovalReviews), as the document
	// header's review is, so the write phase writes the rows and appends their events in that
	// order and this route publishes them.
	transition.After = func(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, ask model.Ask) ([]*model.Event, error) {
		if ask.Kind != "approval" {
			return nil, nil
		}
		state, reason, err := reviewFromAnswer(input.Selected, input.Text)
		if err != nil {
			return nil, err
		}
		artifact, err := s.loadArtifact(ctx, tx, ask.Approval.ArtifactID)
		if err != nil {
			return nil, err
		}
		_, reviewEvents, err := s.queueApprovalReviews(ctx, tx, ledger, artifact, ask.Approval.Documents, state, actor, reason, new(ask.ID))
		if err != nil {
			return nil, err
		}
		return reviewEvents, nil
	}
	ask, err := s.closeAsk(r.Context(), r.PathValue("id"), actor, transition, readBack)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return model.Ask{}, false
	}
	return ask, true
}

// approvalAnswerRefusal is the refusal of actor's answer to ask, and nil where actor may answer it:
// answering an approval request approves its document, so it needs the approve scope beside the
// person scope the answer's routes declare. The refusal is the gate's own (missingScopeError),
// which writeHandlerError gives the challenge header. recordAnswer holds every answer to it, and
// PUT /api/v2/asks/{id}/state holds the answer it would answer as it stands (putAskStateV2), so
// the no-op tells no caller that may not approve that its approval stands.
func approvalAnswerRefusal(ask model.Ask, actor model.Actor) error {
	if ask.Kind == "approval" && !actor.Act.Holds(model.ScopeApprove) {
		return missingScopeError(model.ScopeApprove,
			"this request's credential does not hold the approve scope, which answering an approval request requires")
	}
	return nil
}

// resolveAskInput is the body of POST /api/v1/asks/{id}/resolve.
type resolveAskInput struct {
	Kind   string       `json:"kind" required:"true"`
	Reason string       `json:"reason" required:"true"`
	Actor  *model.Actor `json:"actor"`
}

func (s *server) resolveAsk(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	if !requireUUIDPath(w, r, "ask", apicode.AskIDInput) {
		return
	}
	var input resolveAskInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	ask, ok := s.recordResolution(w, r, actor, input.Kind, input.Reason, nil)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, newShapedAsk(ask, wantsDisplayTrees(r)))
}

// recordResolution resolves or retracts the ask the path names without an answer, for actor, once
// the route has read its body: kind is retracted or resolved, and reason is required, held to the
// event's bound and to no opening of the settlement's own. POST /api/v1/asks/{id}/resolve
// (resolveAsk) and PUT /api/v2/asks/{id}/state with state resolved or retracted (putAskStateV2)
// each answer it in their own shape, the /api/v2 route reading its own on the write's transaction
// (readBack). It writes a refusal itself and answers false.
func (s *server) recordResolution(w http.ResponseWriter, r *http.Request, actor model.Actor, kind, reason string, readBack askReadBack) (model.Ask, bool) {
	kind = strings.TrimSpace(kind)
	reason = strings.TrimSpace(reason)
	if kind != "retracted" && kind != "resolved" {
		writeError(w, r, apicode.InvalidResolution,
			`resolution kind must be "retracted" or "resolved", not `+strconv.Quote(kind))
		return model.Ask{}, false
	}
	if reason == "" {
		writeError(w, r, apicode.InvalidResolution, "resolution requires a kind and reason")
		return model.Ask{}, false
	}
	// A resolution's reason rides the ask.resolved event, so it takes the event's bound; before
	// the transition opens its transaction, so nothing is written.
	if length := itemblocks.Units16(reason); length > contracts.DispatchEventBodyMax {
		capExceeded(w, r, "reason", length, contracts.DispatchEventBodyMax)
		return model.Ask{}, false
	}
	// Settlement's own retraction reason is how a resolution written before this rule is
	// recognised as settlement's, and such a resolution is restored when its block comes back.
	// A caller writing that reason would have their retract undone, so it is not theirs to
	// write.
	if strings.HasPrefix(reason, docs.SettlementRetractionReason) {
		writeError(w, r, apicode.InvalidResolution,
			"reason may not begin with "+strconv.Quote(docs.SettlementRetractionReason)+", which marks a retraction the document's settlement wrote")
		return model.Ask{}, false
	}
	// Read before closeAsk opens its transaction (specWriterOf), for the check in Apply.
	writer := s.specWriterOf(r, actor)
	ask, err := s.closeAsk(r.Context(), r.PathValue("id"), actor, askTransition{
		EventType: "ask.resolved",
		Apply: func(ctx context.Context, tx pgx.Tx, _ *docs.Ledger, ask model.Ask) (model.Ask, error) {
			// The check of who writes an issue's spec or Verification document, on the block's
			// document and each copy of the ask, before the first document call.
			if err := s.refuseSpecWriterOfAsk(ctx, tx, writer, ask); err != nil {
				return model.Ask{}, err
			}
			if err := s.writeClosedBlockState(ctx, ask, actor); err != nil {
				return model.Ask{}, err
			}
			// ctx is joined to the transaction's ledger (closeAsk), so the resolution's update
			// is queued there, after the block's writes.
			return docs.WriteAskResolution(ctx, tx, ask, kind, reason, actor)
		},
	}, readBack)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return model.Ask{}, false
	}
	return ask, true
}

// writeClosedBlockState writes a closed block ask's state into its document, as the resolve and
// the decline both must: a block ask's closed state belongs in its block too, written by
// whoever closed it, since left to settlement the repair lands on whoever next touches the
// document. A copy of the ask on another document of its owner shows its state as well, so each
// copy is settled again (docs.SettleCopiesOf). An ask with no block writes nothing.
func (s *server) writeClosedBlockState(ctx context.Context, ask model.Ask, actor model.Actor) error {
	if ask.BlockID == nil {
		return nil
	}
	blockArtifact, err := blockArtifactOf(ask)
	if err != nil {
		return err
	}
	if err := s.deps.Docs.SettleCopiesOf(ctx, ask); err != nil {
		return err
	}
	return s.deps.Docs.SetBlockAttributes(ctx, blockArtifact, *ask.BlockID, map[string]any{
		"state": "resolved",
	}, actor)
}

// declineAskInput is the body of POST /api/v1/asks/{id}/decline, and what PUT
// /api/v2/asks/{id}/state's declined arm hands recordDecline, its reason as the note
// (putAskStateV2). Both fields are optional: a person who writes no note declines all the same,
// and a browser names no acting session.
type declineAskInput struct {
	Note  *string      `json:"note"`
	Actor *model.Actor `json:"actor"`
}

// declineAsk records that a person chose not to answer an ask. The ask closes as every other
// resolution closes, its kind is model.AskResolutionDeclined, and the row keeps the person's
// note apart from the four sentences Dispatch writes for the asking agent
// (model.DeclineDispatchWords).
func (s *server) declineAsk(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	if !requireUUIDPath(w, r, "ask", apicode.AskIDInput) {
		return
	}
	var input declineAskInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	ask, ok := s.recordDecline(w, r, actor, input, nil)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, newShapedAsk(ask, wantsDisplayTrees(r)))
}

// recordDecline declines the ask the path names for actor, a person, once the route has read its
// body: the ask resolves with the kind declined, the note as its reason beside the words Dispatch
// writes for the asking agent. POST /api/v1/asks/{id}/decline (declineAsk) and PUT
// /api/v2/asks/{id}/state with state declined (putAskStateV2) each answer it in their own shape,
// the /api/v2 route reading its own on the write's transaction (readBack). It writes a refusal
// itself and answers false.
func (s *server) recordDecline(w http.ResponseWriter, r *http.Request, actor model.Actor, input declineAskInput, readBack askReadBack) (model.Ask, bool) {
	// The acting session the body names, where the caller's credential proved none, as the
	// answer route reads it: a personal token names its session nowhere else.
	actor, ok := actSessionFromBody(w, r, actor, input.Actor)
	if !ok {
		return model.Ask{}, false
	}
	note := ""
	if input.Note != nil {
		note = strings.TrimSpace(*input.Note)
	}
	// The note is the resolution's reason and rides the ask.resolved event, so it takes the
	// event's bound; before the transition opens its transaction, so nothing is written.
	if length := itemblocks.Units16(note); length > contracts.DispatchEventBodyMax {
		capExceeded(w, r, "note", length, contracts.DispatchEventBodyMax)
		return model.Ask{}, false
	}
	// The resolve route refuses a reason opening with docs.SettlementRetractionReason because
	// such a reason marks a retraction settlement itself wrote, which returning the block undoes
	// (docs.settlementRetraction, which reads that prefix on a retraction alone). A decline is
	// no retraction, so no note written here can be read as settlement's work and nothing of a
	// person's words is refused for how they open.
	ask, err := s.closeAsk(r.Context(), r.PathValue("id"), actor, askTransition{
		EventType: "ask.resolved",
		Apply: func(ctx context.Context, tx pgx.Tx, _ *docs.Ledger, ask model.Ask) (model.Ask, error) {
			// The kind is refused here, inside Apply, for the reason recordAnswer states for its
			// own kind-dependent refusal: an ask's kind is known only once its row is read, and
			// a read before the transaction would decide on a row another writer could still
			// move. Under the ask's owner lock, which transitionAskTx takes before it reads the
			// row (requireOpenOwner), the kind refused is the kind this write would close.
			if ask.Kind == "approval" {
				return model.Ask{}, errorf(apicode.AskNotDeclinable,
					"an approval request is answered with %s or %s; it is not declined",
					docs.ApprovalOptionApprove, docs.ApprovalOptionRequestChanges)
			}
			if err := s.writeClosedBlockState(ctx, ask, actor); err != nil {
				return model.Ask{}, err
			}
			return docs.WriteAskResolution(ctx, tx, ask, model.AskResolutionDeclined, note, actor)
		},
	}, readBack)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return model.Ask{}, false
	}
	return ask, true
}

// closeAsk applies transition to the open ask id names, for actor, in a transaction of its own
// (closeAskTx), and commits it; readBack reads on that transaction what the route answers of the
// ask once the write phase has written the transition and before the commit (askReadBack.commit).
func (s *server) closeAsk(ctx context.Context, id string, actor model.Actor, transition askTransition, readBack askReadBack) (model.Ask, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Ask{}, err
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := s.deps.Docs.Join(ctx, tx)
	defer ledger.Discard()
	ask, events, err := s.closeAskTx(documentCtx, tx, ledger, id, actor, transition)
	if err != nil {
		return model.Ask{}, err
	}
	if err := readBack.commit(ctx, ledger, ask.ID); err != nil {
		return model.Ask{}, err
	}
	s.publish(ctx, appendedEvents(events...)...)
	// What the transition wrote: an answer's own words, and the reason a resolution recorded.
	// Which of the two the ask holds is the transition's, so both are read from the ask it left,
	// through the one walk of an ask's stored texts (texttrees.VisitAskTexts), so an answer or
	// resolution text added to that walk is stored by this write too.
	written := texttrees.AppendAskTexts(nil, texttrees.AskContent{Answer: ask.Answer, Resolution: ask.Resolution})
	s.storeDisplayTrees(ctx, ask.ID, written...)
	return ask, nil
}

// closeAskTx applies a transition to an open ask inside the caller's transaction, indexes the
// ask's parts, appends the transition's event and runs its After; the caller commits and
// publishes the events. Every write is queued on ledger, in the order made, so the copies'
// pending rows SettleCopiesOf takes come before the parts' index and the event appends, as the
// copy lock's order requires (docs.lockProjectCopies), and the events are filled in once Commit
// has run the write phase (appendedEvents).
func (s *server) closeAskTx(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, id string, actor model.Actor, transition askTransition) (model.Ask, []*model.Event, error) {
	ask, err := s.transitionAskTx(ctx, tx, ledger, id, transition)
	if err != nil {
		return model.Ask{}, nil, err
	}
	// A transition writes no question text, but it holds the ask's parts alone: an answer is a
	// part of the ask, so both transitions index the parts here, where the row write they queued
	// comes before the index, and the parts' step reconciles the question's references after them
	// and settles the ask's row of the forms queue there (questionReconciled false): a resolution
	// adds no part, but its indexing still takes the ask's state row, reconciles the links its
	// parts hold and clears owed_at, and writes every part of an ask an older build left owed. The
	// answer a transition wrote is queued before the event, so the ask's Replies rows are read by
	// the event's insert, after it.
	at := refs.AskPartsAt(ask)
	changes, err := s.queueAskParts(ctx, ledger, ask, nil, at, false, model.ReferenceChanges{})
	if err != nil {
		return model.Ask{}, nil, err
	}
	base := model.NewAskEventPayload(ask, changes)
	payload := any(base)
	if transition.Payload != nil {
		payload = transition.Payload(base)
	}
	// The part edges the index above moves are new rows of this transaction, so the event stamps
	// them with its id ("ask", ask.ID) and a ?since= read lists them, as a create's and an edit's
	// do.
	event, err := s.queueLoggedEvent(ledger, ownerOf(ask.IssueKey, ask.ArtifactID).event(
		transition.EventType, actor, payload,
	), events.ThreadLoginsOf(ask.ID), "ask", ask.ID)
	if err != nil {
		return model.Ask{}, nil, err
	}
	events := []*model.Event{event}
	if transition.After != nil {
		more, err := transition.After(ctx, tx, ledger, ask)
		if err != nil {
			return model.Ask{}, nil, err
		}
		events = append(events, more...)
	}
	return ask, events, nil
}

// transitionAskTx reads an open ask under its owner's lock and applies a transition inside the
// caller's transaction; it appends no event.
func (s *server) transitionAskTx(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, id string, transition askTransition) (model.Ask, error) {
	unlockedAsk, err := s.loadAsk(ctx, tx, id)
	if err != nil {
		return model.Ask{}, err
	}
	if err := s.requireOpenOwner(ctx, tx, ownerOf(unlockedAsk.IssueKey, unlockedAsk.ArtifactID)); err != nil {
		return model.Ask{}, err
	}
	ask, err := s.readAskForTransition(ctx, tx, id)
	if err != nil {
		return model.Ask{}, err
	}
	switch ask.State {
	case "open":
		if transition.ReplacesAnswer != nil {
			return model.Ask{}, errorf(apicode.AskAnswerChanged, "the answer changed after you reviewed it; review the current answer and confirm your change")
		}
	case "answered":
		switch {
		case transition.EventType != "ask.answered":
			return model.Ask{}, errorf(apicode.AskAnswered, "ask is already answered")
		case transition.ReplacesAnswer == nil:
			// A first answer to an ask someone answered first.
			return model.Ask{}, errorf(apicode.AskClosed, "ask is already answered")
		}
		// A change to the current answer, which Apply checks is the answer the human reviewed.
	case "resolved":
		return model.Ask{}, errorf(apicode.AskResolved, "ask is already resolved")
	default:
		return model.Ask{}, errorf(apicode.AskStateInvalid, "ask has an invalid state")
	}
	return transition.Apply(ctx, tx, ledger, ask)
}
func selectedOptions(options []model.AskOption, selected []string) bool {
	labels := make(map[string]struct{}, len(options))
	for _, option := range options {
		labels[option.Label] = struct{}{}
	}
	seen := make(map[string]struct{}, len(selected))
	for _, value := range selected {
		if _, exists := labels[value]; !exists {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

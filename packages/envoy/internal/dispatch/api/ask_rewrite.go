package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// ask_rewrite.go serves POST /api/v1/asks/{id}/rewrites, a person's Rewrite of an open question
// (INFRA-3231's R7 and Decision 23, INFRA-3552's R1 to R3). The route answers at once and the
// rewrite runs in the background:
//
//   - One rewrite of an ask runs at a time (ask_rewrites): a request while a fresh run is
//     rewriting starts none and is answered that run's state, so a second press gets the first's
//     result. Each request is a writing_check_events row, rewrite_requested, with the person as
//     its reader.
//   - When a session opened the ask and the listener reads it running or unresponsive with a host
//     that advertises btw, Dispatch sends it one side question as its own system actor: a message
//     to that session, on its own conversation and not on the issue, whose text names the ask
//     (sendResolvedDelivery, through writeMessage). The run waits at most the side question's
//     wait for the session's reply, which the message thread keeps (INFRA-3552's R3).
//   - The component's Rewrite reads Dispatch's record and the answer, and its Preserve says
//     whether the rewrite keeps what a rewrite keeps.
//   - The rewrite is applied through the edit route's own path (writeAskEdit), or a decision
//     block's (rewriteDecisionBlock), only while the ask is open and its wording is the wording
//     the run read: an agent's edit during the run is never written over. Its ask.edited holds the
//     old wording, who asked, why and whether the agent's answer was read, and is quiet.
//
// The run emits ask.rewriting when it starts, then ask.edited or ask.rewrite_failed, all quiet:
// no session gets them as a card, and the browser's live socket carries them.

// rewriteRequestedRoute is the route a Rewrite request's writing_check_events row names.
const rewriteRequestedRoute = "/api/v1/asks/{id}/rewrites"

// writingCheckRewriteRequested is the outcome of a person's Rewrite request (store migration
// 20261119050002_writing_check_events_reader admits it): one row per request, whatever came of
// it, with the person who asked as its reader.
const writingCheckRewriteRequested = "rewrite_requested"

// sideQuestionPoll is how often a run looks for the asking session's answer to its side question.
const sideQuestionPoll = 250 * time.Millisecond

// askRewriteAnswer is what POST /api/v1/asks/{id}/rewrites answers: the ask, whether this request
// started a rewrite or found one running, and the ask's rewrite as a read now carries it.
type askRewriteAnswer struct {
	AskID   string            `json:"ask_id"`
	Started bool              `json:"started"`
	Rewrite model.TextRewrite `json:"rewrite"`
}

// askRewriteRun is one run of a person's Rewrite: the ask, the run's id, which the ask's
// ask_rewrites row names while it is the run in progress, the person who asked, and the model the
// run calls: Rewrite and Define's model group, or the test model the request named
// (requestModel).
type askRewriteRun struct {
	askID, run string
	person     model.Actor
	model      reviewer.Model
}

// rewriteAsk is POST /api/v1/asks/{id}/rewrites. Every request for an ask that exists is recorded,
// whatever came of it, once its transaction has ended.
func (s *server) rewriteAsk(w http.ResponseWriter, r *http.Request) {
	person, ok := s.actingPerson(w, r)
	if !ok || !requireUUIDPath(w, r, "ask") {
		return
	}
	var base reviewer.Model
	if s.deps.RewriteDefine != nil {
		base = s.deps.RewriteDefine.config.Model
	}
	chosen, err := s.requestModel(r, base)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer, project, found, err := s.startAskRewrite(r.Context(), person, r.PathValue("id"), chosen)
	if found {
		s.recordRewriteRequest(person, project)
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, answer)
}

// startAskRewrite starts a run of person's Rewrite of the ask id with chosen, or answers the run
// that holds it: an open question alone is rewritten, an approval request never, and none at all
// where no model group for Rewrite and Define is set, which an ask that does not exist answers 404
// before. It answers the ask's project and whether the ask exists, for the request's record.
func (s *server) startAskRewrite(ctx context.Context, person model.Actor, id string, chosen reviewer.Model) (askRewriteAnswer, string, bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return askRewriteAnswer{}, "", false, err
	}
	defer tx.Rollback(ctx)
	ask, err := s.loadAsk(ctx, tx, id)
	if err != nil {
		return askRewriteAnswer{}, "", false, err
	}
	// The owner first, as every write of an ask takes it, so the run's row and its event never
	// wait on a writer that holds the owner and waits on the ask.
	if _, _, err := ownerStatus(ctx, ownerOf(ask.IssueKey, ask.ArtifactID), lockedOwnerRead(tx)); err != nil {
		return askRewriteAnswer{}, "", false, err
	}
	project := s.askProject(ctx, tx, ask)
	switch {
	case s.deps.RewriteDefine == nil:
		return askRewriteAnswer{}, project, true, errorf(http.StatusServiceUnavailable, "REWRITE_NO_MODEL",
			"this deployment sets no model group for Rewrite and Define (DISPATCH_REWRITE_DEFINE_MODEL_*)")
	case ask.Kind == "approval":
		return askRewriteAnswer{}, project, true, errorf(http.StatusConflict, "ASK_KIND_FIXED",
			"an approval request is not rewritten: its question is Dispatch's own, and the document under approval is what its reader reads")
	case ask.State != "open":
		return askRewriteAnswer{}, project, true, errorf(http.StatusConflict, "ASK_NOT_OPEN", "only an open question is rewritten")
	}
	original := askWordingRead(ask)
	encodedOriginal, err := encodeJSON(original)
	if err != nil {
		return askRewriteAnswer{}, project, true, err
	}
	encodedPerson, err := encodeJSON(person)
	if err != nil {
		return askRewriteAnswer{}, project, true, err
	}
	run := askRewriteRun{askID: ask.ID, run: uuid.NewString(), person: person, model: chosen}
	// The run takes the row unless a fresh run holds it: a request while one runs is answered
	// that run's state and starts nothing. A run older than the bound is one a stopped process
	// left, which a new run replaces.
	var startedAt time.Time
	err = tx.QueryRow(ctx, `
		insert into ask_rewrites (ask_id, run, state, asked_by, original)
		values ($1, $2, 'rewriting', $3, $4)
		on conflict (ask_id) do update
		set run = excluded.run, state = 'rewriting', asked_by = excluded.asked_by, original = excluded.original,
			question_message = null, reason = null, started_at = now(), finished_at = null
		where ask_rewrites.state <> 'rewriting' or ask_rewrites.started_at <= now() - $5::interval
		returning started_at`,
		ask.ID, run.run, encodedPerson, encodedOriginal, intervalOf(askRewriteBound)).Scan(&startedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		running, err := s.runningAskRewrite(ctx, tx, ask.ID)
		return askRewriteAnswer{AskID: ask.ID, Rewrite: running}, project, true, err
	}
	if err != nil {
		return askRewriteAnswer{}, project, true, err
	}
	rewrite := model.TextRewrite{State: model.RewriteRewriting, By: model.RewriteByPerson, At: startedAt.UTC(), Original: original}
	event, err := s.appendEvent(ctx, tx, ownerOf(ask.IssueKey, ask.ArtifactID).event("ask.rewriting", person,
		model.AskRewriteEventPayload{ID: ask.ID, IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID, Rewrite: rewrite}))
	if err != nil {
		return askRewriteAnswer{}, project, true, err
	}
	if err := tx.Commit(ctx); err != nil {
		return askRewriteAnswer{}, project, true, err
	}
	s.publish(ctx, event)
	s.detached.goDetached(func() { s.runAskRewrite(s.postContext(), run) })
	return askRewriteAnswer{AskID: ask.ID, Started: true, Rewrite: rewrite}, project, true, nil
}

// recordRewriteRequest records one Rewrite request in writing_check_events: rewrite_requested,
// with the person who asked as its reader, and the ask's project.
func (s *server) recordRewriteRequest(person model.Actor, project string) {
	event := store.WritingCheckEvent{
		Route: rewriteRequestedRoute, Method: http.MethodPost, ActorKind: person.Kind, ActorID: person.ID,
		Outcome: writingCheckRewriteRequested, Reader: person.ID, Project: project,
	}
	if person.Act != nil {
		event.ActSession, event.TokenID = person.Act.Session, person.Act.TokenID
	}
	s.recordWritingCheck(s.postContext(), event)
}

// askProject is the project of the ask's issue or document, "" where the read fails.
func (s *server) askProject(ctx context.Context, q queryer, ask model.Ask) string {
	var project string
	if err := q.QueryRow(ctx, `
		select coalesce((select project_key from issues where key = $1), (select project_key from artifacts where id = $2::uuid), '')`,
		ask.IssueKey, ask.ArtifactID).Scan(&project); err != nil {
		slog.Warn("dispatch: read the project of an ask", "ask", ask.ID, "error", err)
	}
	return project
}

// runningAskRewrite is the rewrite the ask's running run carries, as a second request is answered.
func (s *server) runningAskRewrite(ctx context.Context, q queryer, askID string) (model.TextRewrite, error) {
	var (
		original  []byte
		startedAt time.Time
	)
	if err := q.QueryRow(ctx, `select original, started_at from ask_rewrites where ask_id = $1`, askID).Scan(&original, &startedAt); err != nil {
		return model.TextRewrite{}, err
	}
	rewrite := model.TextRewrite{State: model.RewriteRewriting, By: model.RewriteByPerson, At: startedAt.UTC()}
	if err := json.Unmarshal(original, &rewrite.Original); err != nil {
		return model.TextRewrite{}, fmt.Errorf("read the original of ask %s's rewrite: %w", askID, err)
	}
	return rewrite, nil
}

// askWordingRead is an ask's wording as a rewrite's original holds it: its question, options and
// choice, the options' words alone (reviewer.OptionWords).
func askWordingRead(ask model.Ask) model.TextWording {
	return model.TextWording{Body: ask.Question, Options: reviewer.OptionWords(ask.Options), Multiple: ask.Multiple}
}

// runAskRewrite is one run of a person's Rewrite, in the background: the side question, the
// record, the component's Rewrite and Preserve, and the rewrite applied or the run failed with
// why. Every way it ends writes the run's row and appends its quiet event; a run that called the
// model also keeps its calls in the row.
func (s *server) runAskRewrite(ctx context.Context, run askRewriteRun) {
	rd := s.deps.RewriteDefine
	subject, err := reviewer.AskSubject(ctx, s.deps.Store, run.askID)
	if err != nil {
		s.failAskRewrite(ctx, run, "Dispatch could not read the question")
		slog.Warn("dispatch: read an ask to rewrite", "ask", run.askID, "error", err)
		return
	}
	subject.Reader = run.person.ID
	answer, question := s.askRewriteSideQuestion(ctx, run, subject)
	record, err := reviewer.BuildRecord(ctx, s.deps.Store, subject)
	if err != nil {
		s.failAskRewrite(ctx, run, "Dispatch could not read the record of the question")
		slog.Warn("dispatch: read the record of an ask to rewrite", "ask", run.askID, "error", err)
		return
	}
	rewritten, err := reviewer.Rewrite(ctx, run.model, rd.config.Rules, record, answer)
	s.recordAskRewriteCalls(ctx, run, rewritten.Calls)
	var failed *reviewer.ModelError
	switch {
	case errors.As(err, &failed):
		s.failAskRewrite(ctx, run, "the model gave no rewrite: "+modelFailure(failed))
		return
	case err != nil:
		s.failAskRewrite(ctx, run, "the rewrite could not be made")
		slog.Warn("dispatch: rewrite an ask", "ask", run.askID, "error", err)
		return
	case rewritten.NotApplied != nil:
		s.failAskRewrite(ctx, run, rewritten.NotApplied.Reason)
		return
	}
	if err := validateAskQuestion(rewritten.Text.Body); err != nil {
		s.failAskRewrite(ctx, run, "the rewrite breaks a bound of a question: "+err.Error())
		return
	}
	if err := validateAskWording(rewritten.Text.Body, rewritten.Text.Options); err != nil {
		s.failAskRewrite(ctx, run, "the rewrite breaks a bound of a question: "+err.Error())
		return
	}
	edit := model.AskRewriteEdit{
		By: model.RewriteByPerson, AskedBy: run.person, AgentAnswer: answer.Text != "", QuestionMessage: question,
	}
	if reason, err := s.applyAskRewrite(ctx, run, subject.Text, rewritten.Text, edit); err != nil {
		s.failAskRewrite(ctx, run, "Dispatch could not write the rewrite")
		slog.Warn("dispatch: write an ask's rewrite", "ask", run.askID, "error", err)
	} else if reason != "" {
		s.failAskRewrite(ctx, run, reason)
	}
}

// recordAskRewriteCalls keeps the model calls of the run in its ask_rewrites row, as a
// definition keeps its own: what each was for, the deployment, the model the provider served and
// the tokens it used. A row another run took keeps its own calls.
func (s *server) recordAskRewriteCalls(ctx context.Context, run askRewriteRun, calls []reviewer.Call) {
	if len(calls) == 0 {
		return
	}
	encoded, err := encodeJSON(callsOf(calls))
	if err == nil {
		_, err = s.deps.Store.Pool.Exec(ctx, `update ask_rewrites set calls = $3 where ask_id = $1 and run = $2`, run.askID, run.run, encoded)
	}
	if err != nil {
		slog.Warn("dispatch: record a rewrite's model calls", "ask", run.askID, "error", err)
	}
}

// modelFailure says why a model call gave no usable answer, as a failed rewrite's reason does.
func modelFailure(failed *reviewer.ModelError) string {
	switch {
	case failed.Err != nil && errors.Is(failed.Err, context.DeadlineExceeded):
		return "it did not answer in time"
	case failed.Err != nil:
		return "the call failed"
	}
	return string(failed.Fault)
}

// askRewriteSideQuestion sends the asking session its one side question when it takes one
// (sideQuestionSession: running or unresponsive, its host advertising btw) and waits for its
// answer for at most the side question's wait from the run's start. The question goes as a
// message from Dispatch to that session, kept on its own conversation, whose text names the ask
// by its address, so the reference graph links the two; the session's reply is kept in the same
// thread. It answers the answer, the zero Answer where none came, and the question's message id,
// "" where none was sent.
func (s *server) askRewriteSideQuestion(ctx context.Context, run askRewriteRun, subject reviewer.Subject) (reviewer.Answer, string) {
	deadline := time.Now().Add(s.deps.RewriteDefine.sideQuestionWait())
	if !s.sideQuestionSession(ctx, subject.Writer, false) {
		return reviewer.Answer{}, ""
	}
	question, err := reviewer.RewriteQuestion(s.askAddress(ctx, run.askID), subject.Text)
	if err != nil {
		slog.Warn("dispatch: write a rewrite's side question", "ask", run.askID, "error", err)
		return reviewer.Answer{}, ""
	}
	target, delivery := "session:"+subject.Writer, "btw"
	input := createMessageInput{Body: question, Target: &target, Delivery: &delivery}
	if _, err := validateCreateMessage(&input); err != nil {
		slog.Warn("dispatch: write a rewrite's side question", "ask", run.askID, "error", err)
		return reviewer.Answer{}, ""
	}
	message, _, err := s.createStoredMessage(ctx, nil, input, delivery, false, reviewerActor)
	if err != nil {
		slog.Warn("dispatch: send a rewrite's side question", "ask", run.askID, "error", err)
		return reviewer.Answer{}, ""
	}
	if _, err := s.deps.Store.Pool.Exec(ctx, `
		update ask_rewrites set question_message = $3 where ask_id = $1 and run = $2`, run.askID, run.run, message.ID); err != nil {
		slog.Warn("dispatch: record a rewrite's side question", "ask", run.askID, "error", err)
	}
	return reviewer.Answer{Question: question, Text: s.awaitSideAnswer(ctx, message.ID, subject.Writer, deadline)}, message.ID
}

// awaitSideAnswer waits until deadline for session's reply to the message id, and answers its
// body; "" where no reply came in time, where every delivery attempt failed, or where the reply
// was an error report.
func (s *server) awaitSideAnswer(ctx context.Context, id, session string, deadline time.Time) string {
	for {
		var (
			body   string
			failed bool
		)
		err := s.deps.Store.Pool.QueryRow(ctx, `
			select
				coalesce((select body from messages
					where in_reply_to = $1::uuid and author->>'kind' = 'session' and author->>'id' = $2
					order by created_at, id limit 1), ''),
				coalesce((select bool_and(state = 'failed') from message_deliveries where message_id = $1::uuid), false)`,
			id, session).Scan(&body, &failed)
		switch {
		case err != nil:
			slog.Warn("dispatch: read the answer to a side question", "message", id, "error", err)
			return ""
		case body != "":
			return body
		case failed:
			return ""
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(min(wait, sideQuestionPoll)):
		}
	}
}

// askAddress is the ask's `dispatch://` address, under its issue or its project document
// (refs.AskRef), or its bare id where no address names it.
func (s *server) askAddress(ctx context.Context, id string) string {
	var issueKey, project, slug string
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce(k.issue_key, ''), coalesce(a.project_key, ''), coalesce(a.slug, '')
		from asks k left join artifacts a on a.id = k.artifact_id
		where k.id = $1::uuid`, id).Scan(&issueKey, &project, &slug); err != nil {
		return id
	}
	if address := refs.AskRef(text.Ref{IssueKey: issueKey, Project: project, Document: slug, ID: id}); address != "" {
		return address
	}
	return id
}

// applyAskRewrite writes rewrite into the ask as the run's rewrite, in one transaction with the
// run's row: through rewriteDecisionBlock for a decision block, else through the edit route's
// path, as Dispatch's quiet edit carrying record. It writes nothing, and answers "", where the run
// is no longer the ask's run in progress; it writes nothing, and answers why, where the ask is no
// longer open or its wording is no longer read, the wording the run read. A refusal of the
// document's own edit path is a reason too. Everything up to Commit is the read phase
// (Docs.Join): the edit and the run's row queue their writes on the ledger, and Commit sends them
// as one batch.
func (s *server) applyAskRewrite(ctx context.Context, run askRewriteRun, read, rewrite reviewer.Text, record model.AskRewriteEdit) (string, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := s.deps.Docs.Join(ctx, tx)
	defer ledger.Discard()
	unlocked, err := s.loadAsk(ctx, tx, run.askID)
	if err != nil {
		return "", err
	}
	if _, _, err := ownerStatus(documentCtx, ownerOf(unlocked.IssueKey, unlocked.ArtifactID), lockedOwnerRead(tx)); err != nil {
		return "", err
	}
	ask, err := s.readAskForTransition(documentCtx, tx, run.askID)
	if err != nil {
		return "", err
	}
	// The run's row is read under the ask's owner's lock, which every writer of its run and its
	// state holds first (startAskRewrite, writeAskRewriteFailure), so it takes no row lock.
	var current, state string
	if err := tx.QueryRow(ctx, `select run::text, state from ask_rewrites where ask_id = $1`, run.askID).Scan(&current, &state); err != nil {
		return "", err
	}
	switch {
	case current != run.run || state != model.RewriteRewriting:
		// Another run took the row: this one's result is no longer the ask's to show.
		return "", nil
	case ask.State != "open":
		return "the question was answered or closed while it was rewritten", nil
	case !sameAskWording(ask, read):
		return "the asking agent changed the question while it was rewritten", nil
	}
	var (
		rewritten model.Ask
		previous  = model.NewAskEditPrevious(ask)
		event     *model.Event
	)
	if ask.BlockID != nil {
		rewritten, event, err = s.rewriteDecisionBlock(documentCtx, tx, ledger, ask, rewrite, record)
	} else {
		rewritten, _, event, err = s.writeAskEdit(documentCtx, tx, ledger, ask, rewriteWording(rewrite), reviewerActor, &record)
	}
	if reason := rewriteRefusal(err); reason != "" {
		return reason, nil
	}
	if err != nil {
		return "", err
	}
	if err := queueExec(ledger, `
		update ask_rewrites set state = 'rewritten', finished_at = now() where ask_id = $1 and run = $2`,
		run.askID, run.run); err != nil {
		return "", err
	}
	if err := ledger.Commit(ctx); err != nil {
		return "", err
	}
	s.publish(ctx, *event)
	s.storeDisplayTrees(ctx, rewritten.ID, texttrees.AskWrittenTexts(
		texttrees.AskWrittenTexts(nil, rewritten.Question, rewritten.Options), previous.Question, previous.Options)...)
	return "", nil
}

// sameAskWording reports whether ask's question, options and choice are read's.
func sameAskWording(ask model.Ask, read reviewer.Text) bool {
	if ask.Question != read.Body || ask.Multiple != read.Multiple || len(ask.Options) != len(read.Options) {
		return false
	}
	for i, option := range ask.Options {
		if option.Label != read.Options[i].Label || option.Description != read.Options[i].Description {
			return false
		}
	}
	return true
}

// rewriteRefusal is why the document's own edit path refused to write a rewrite into its decision
// block, as a failed rewrite's reason; "" for an error that is no such refusal.
func rewriteRefusal(err error) string {
	var (
		unrepresentable *docs.ErrAskBlockUnrepresentable
		invalid         *docs.ErrInvalidAskBlock
		refusal         *apiError
	)
	switch {
	case errors.As(err, &unrepresentable):
		return "the decision block cannot hold the rewrite: " + unrepresentable.Reason
	case errors.As(err, &invalid):
		return "the decision block cannot hold the rewrite: " + invalid.Error()
	case errors.As(err, &refusal) && refusal.status < http.StatusInternalServerError:
		return refusal.message
	}
	return ""
}

// failAskRewrite ends the run as failed with reason, which the card shows as the failed rewrite's
// tooltip: the run's row records it and a quiet ask.rewrite_failed carries the ask's rewrite as a
// read now gives it. A run whose row another run took records nothing.
func (s *server) failAskRewrite(ctx context.Context, run askRewriteRun, reason string) {
	if err := s.writeAskRewriteFailure(ctx, run, reason); err != nil {
		slog.Warn("dispatch: record a failed rewrite", "ask", run.askID, "reason", reason, "error", err)
	}
}

func (s *server) writeAskRewriteFailure(ctx context.Context, run askRewriteRun, reason string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var issueKey, artifactID *string
	if err := tx.QueryRow(ctx, `select issue_key, artifact_id::text from asks where id = $1`, run.askID).Scan(&issueKey, &artifactID); err != nil {
		return err
	}
	owner := ownerOf(issueKey, artifactID)
	if _, _, err := ownerStatus(ctx, owner, lockedOwnerRead(tx)); err != nil {
		return err
	}
	var (
		original   []byte
		finishedAt time.Time
	)
	err = tx.QueryRow(ctx, `
		update ask_rewrites set state = 'failed', reason = $3, finished_at = now()
		where ask_id = $1 and run = $2 and state = 'rewriting'
		returning original, finished_at`, run.askID, run.run, reason).Scan(&original, &finishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rewrite := model.TextRewrite{State: model.RewriteFailed, By: model.RewriteByPerson, At: finishedAt.UTC(), Reason: reason}
	if err := json.Unmarshal(original, &rewrite.Original); err != nil {
		return err
	}
	event, err := s.appendEvent(ctx, tx, owner.event("ask.rewrite_failed", reviewerActor,
		model.AskRewriteEventPayload{ID: run.askID, IssueKey: issueKey, ArtifactID: artifactID, Rewrite: rewrite}))
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.publish(ctx, event)
	return nil
}

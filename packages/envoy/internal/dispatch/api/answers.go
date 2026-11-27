package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const (
	myAnswersDefaultLimit = 50
	myAnswersMaxLimit     = 200
)

// parseMyAnswersPage reads the complete offset page that GET /api/v1/me/answers and
// GET /api/v1/people/{login}/nodes each always return.
func parseMyAnswersPage(r *http.Request) (limit, offset int, err error) {
	query := r.URL.Query()
	limit = myAnswersDefaultLimit
	if value, present, valid := parseQueryInt(query, "limit", 1, myAnswersMaxLimit); present {
		if !valid {
			return 0, 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "limit must be one integer from 1 to %d", myAnswersMaxLimit)
		}
		limit = value
	}
	if value, present, valid := parseQueryInt(query, "offset", 0, math.MaxInt); present {
		if !valid {
			return 0, 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "offset must be one non-negative integer")
		}
		offset = value
	}
	return limit, offset, nil
}

// listMyAnswers returns the caller's real answer transitions (answerEventsQuery) and their replies
// on asks, newest first. Each row's ask state, and whether its answer is the one the ask holds now
// (current), are the ask's node's (asks.ReadAnswer), as every read of an answer reads them (R3): an
// answer is current while the ask is answered and its answer was given at that answer's time. A
// reply the caller edited carries the time of the edit (edited_at), and one the caller deleted
// carries its mark (deleted_at, deleted_by) and the empty body the delete left, so the page says
// that the post is edited or deleted.
func (s *server) listMyAnswers(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	limit, offset, err := parseMyAnswersPage(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		with answered as (
		`+answerEventsQuery(`e.created_at as at, 'answer' as kind, e.payload->>'id' as ask_id,
		       e.payload->'answer' as answer, null::uuid as reply_id, null::text as body, e.id as event_id,
		       null::timestamptz as reply_edited_at, null::timestamptz as deleted_at, null::jsonb as deleted_by`,
		// The canonical login on both halves: a row a build stored under a login before sign-in
		// lower-cased it is the same person's (user_threads_login, as the replies half reads).
		`user_threads_login(e.actor->>'id') = $1`)+`
		), mine as (
			select * from answered
			union all
			select c.created_at, 'reply', c.ask_id::text, null, c.id, c.body, null, c.edited_at, c.deleted_at, c.deleted_by
			from comments c
			where c.ask_id is not null and c.author->>'kind' = 'user' and user_threads_login(c.author->>'id') = $1
		)
		select m.at, m.kind, m.ask_id, m.answer, m.reply_id, m.body, m.event_id, m.reply_edited_at, m.deleted_at, m.deleted_by, `+askItemRefExpression+` as ref,
		       a.question, a.kind, a.state, a.answer, `+asks.NodeAnswerColumn("a")+`, a.edited_at,
		       i.key, i.title, ar.project_key, ar.slug, ar.name, count(*) over () as total
		from mine m
		join asks a on a.id = m.ask_id::uuid
		left join issues i on i.key = a.issue_key
		left join artifacts ar on ar.id = a.artifact_id
		order by m.at desc, m.event_id desc nulls last, m.reply_id desc nulls last
		limit $2 offset $3
	`, model.CanonicalLogin(caller.ID), limit, offset)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()

	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	response := model.MyAnswersResponse{Rows: []model.MyAnswerRow{}, Limit: limit, Offset: offset}
	for rows.Next() {
		var row model.MyAnswerRow
		var answer, askAnswer, nodeAnswer []byte
		var askState string
		var replyID *string
		var body *string
		var eventID *int64
		var editedAt *time.Time
		var replyEditedAt, replyDeletedAt *time.Time
		var replyDeletedBy []byte
		var issueKey, issueTitle, project, slug, name *string
		var total int64
		if err := rows.Scan(
			&row.At,
			&row.Kind,
			&row.AskID,
			&answer,
			&replyID,
			&body,
			&eventID,
			&replyEditedAt,
			&replyDeletedAt,
			&replyDeletedBy,
			&row.Ref,
			&row.Question,
			&row.AskKind,
			&askState,
			&askAnswer,
			&nodeAnswer,
			&editedAt,
			&issueKey,
			&issueTitle,
			&project,
			&slug,
			&name,
			&total,
		); err != nil {
			s.writeHandlerError(w, fmt.Errorf("scan my answer: %w", err))
			return
		}
		row.EditedAt = timestampPtr(editedAt)
		if issueKey != nil {
			row.Owner.Issue = &model.OpenAskIssue{Key: *issueKey, Title: *issueTitle}
		} else {
			row.Owner.Document = &model.OpenAskDocument{Project: *project, Slug: *slug, Name: *name}
		}
		if row.Kind == "answer" {
			if err := json.Unmarshal(answer, &row.Answer); err != nil {
				s.writeHandlerError(w, fmt.Errorf("decode my answer: %w", err))
				return
			}
		} else {
			// The reply's own marks, so a reply the caller edited reads as edited, and one the
			// caller deleted reads as deleted and not as an empty line.
			deletedAt, deletedBy, err := model.DeletedPostMark(replyDeletedAt, replyDeletedBy)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			row.Reply = &model.MyAnswerReply{
				ID: *replyID, Body: *body, EditedAt: replyEditedAt, DeletedAt: deletedAt, DeletedBy: deletedBy,
			}
		}
		ask, err := asks.ReadAnswer(row.AskID, askState, askAnswer, nodeAnswer)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		row.AskState = ask.State
		row.Current = row.Answer != nil && ask.State == asks.StateAnswered && ask.Answer != nil && ask.Answer.At.Equal(row.Answer.At)
		response.Total = int(total)
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, fmt.Errorf("list my answers: %w", err))
		return
	}
	// Each row's texts are registered once the page is whole: the question the answer was given
	// to, the text written with it and each label it chose, and the body of a reply, all of them
	// stored texts the table holds the trees of. A row of a slice still being appended to is a row
	// the reader must not hold the address of.
	for index := range response.Rows {
		row := &response.Rows[index]
		reader.text(&row.QuestionTree, row.Question)
		if row.Answer != nil {
			reader.textOf(&row.Answer.TextTree, row.Answer.Text)
			registerChosenLabels(reader, row.Answer)
		}
		if row.Reply != nil {
			reader.text(&row.Reply.BodyTree, row.Reply.Body)
		}
	}
	reader.resolve()
	WriteJSON(w, http.StatusOK, response)
}

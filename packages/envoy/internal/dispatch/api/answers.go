package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const (
	myAnswersDefaultLimit = 50
	myAnswersMaxLimit     = 200
)

// parseMyAnswersPage reads the complete offset page that GET /api/v1/me/answers always returns.
func parseMyAnswersPage(r *http.Request) (limit, offset int, err error) {
	query := r.URL.Query()
	limit = myAnswersDefaultLimit
	if value, present, valid := parseQueryInt(query, "limit", 1, myAnswersMaxLimit); present {
		if !valid {
			return 0, 0, errorf(apicode.InvalidQuery, "limit must be one integer from 1 to %d", myAnswersMaxLimit)
		}
		limit = value
	}
	if value, present, valid := parseQueryInt(query, "offset", 0, math.MaxInt); present {
		if !valid {
			return 0, 0, errorf(apicode.InvalidQuery, "offset must be one non-negative integer")
		}
		offset = value
	}
	return limit, offset, nil
}

// listMyAnswers returns the caller's real answer transitions (answerEventsQuery) and their replies
// on asks, newest first.
func (s *server) listMyAnswers(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	limit, offset, err := parseMyAnswersPage(r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	response, err := s.readMyAnswers(r, caller, limit, offset)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, response)
}

// readMyAnswers reads limit of the caller's answers and replies from offset, newest first, with
// the display trees of their texts where the request asks for them (Dispatch-Trees): the page GET
// /api/v1/me/answers answers and GET /api/v2/me/answers answers in the envelope. Total is the count
// of the whole list as the page's rows carry it, so a page past the end carries none.
func (s *server) readMyAnswers(r *http.Request, caller model.Actor, limit, offset int) (model.MyAnswersResponse, error) {
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		with answered as (
			`+answerEventsQuery(`e.created_at as at, 'answer' as kind, e.payload->>'id' as ask_id,
			       e.payload->'answer' as answer, null::uuid as reply_id, null::text as body, e.id as event_id`,
		`e.actor->>'id' = $1`)+`
		), mine as (
			select * from answered
			union all
			select c.created_at, 'reply', c.ask_id::text, null, c.id, c.body, null
			from comments c
			where c.ask_id is not null and c.author->>'kind' = 'user' and c.author->>'id' = $1
		)
		select m.at, m.kind, m.ask_id, m.answer, m.reply_id, m.body, m.event_id, `+askItemRefExpression+` as ref,
		       a.question, a.kind, a.state, a.edited_at,
		       coalesce(a.state = 'answered' and m.kind = 'answer' and a.answer->>'at' = m.answer->>'at', false) as current,
		       i.key, i.title, ar.project_key, ar.slug, ar.name, count(*) over () as total
		from mine m
		join asks a on a.id = m.ask_id::uuid
		left join issues i on i.key = a.issue_key
		left join artifacts ar on ar.id = a.artifact_id
		order by m.at desc, m.event_id desc nulls last, m.reply_id desc nulls last
		limit $2 offset $3
	`, model.CanonicalLogin(caller.ID), limit, offset)
	if err != nil {
		return model.MyAnswersResponse{}, err
	}
	defer rows.Close()

	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	response := model.MyAnswersResponse{Rows: []model.MyAnswerRow{}, Limit: limit, Offset: offset}
	for rows.Next() {
		var row model.MyAnswerRow
		var answer []byte
		var replyID *string
		var body *string
		var eventID *int64
		var editedAt *time.Time
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
			&row.Ref,
			&row.Question,
			&row.AskKind,
			&row.AskState,
			&editedAt,
			&row.Current,
			&issueKey,
			&issueTitle,
			&project,
			&slug,
			&name,
			&total,
		); err != nil {
			return model.MyAnswersResponse{}, fmt.Errorf("scan my answer: %w", err)
		}
		row.EditedAt = timestampPtr(editedAt)
		if issueKey != nil {
			row.Owner.Issue = &model.OpenAskIssue{Key: *issueKey, Title: *issueTitle}
		} else {
			row.Owner.Document = &model.OpenAskDocument{Project: *project, Slug: *slug, Name: *name}
		}
		if row.Kind == "answer" {
			if err := json.Unmarshal(answer, &row.Answer); err != nil {
				return model.MyAnswersResponse{}, fmt.Errorf("decode my answer: %w", err)
			}
		} else {
			row.Reply = &model.MyAnswerReply{ID: *replyID, Body: *body}
		}
		response.Total = int(total)
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return model.MyAnswersResponse{}, fmt.Errorf("list my answers: %w", err)
	}
	// Each row's texts are registered once the page is whole: the question the answer was given
	// to, the text written with it and the body of a reply, all of them stored texts the table
	// holds the trees of. A row of a slice still being appended to is a row the reader must not
	// hold the address of.
	for index := range response.Rows {
		row := &response.Rows[index]
		reader.text(&row.QuestionTree, row.Question)
		if row.Answer != nil {
			reader.textOf(&row.Answer.TextTree, row.Answer.Text)
		}
		if row.Reply != nil {
			reader.text(&row.Reply.BodyTree, row.Reply.Body)
		}
	}
	reader.resolve()
	return response, nil
}

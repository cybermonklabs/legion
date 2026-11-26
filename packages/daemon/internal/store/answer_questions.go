package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// QuestionState is where a question the answer loop holds stands (answer_questions.state).
type QuestionState string

const (
	// QuestionReceived is a question recorded and not yet answered.
	QuestionReceived QuestionState = "received"
	// QuestionGenerating is a question whose answer is being generated: a restart answers it again,
	// with the gh calls the earlier run made.
	QuestionGenerating QuestionState = "generating"
	// QuestionAppended is a question whose reply is decided (ReplyText): the model's, appended to
	// the conversation, or the daemon's own, or, for a question of a released agent (Wake), one the
	// loop neither appends nor posts. A restart posts it, or gives it to the released agent, and
	// never answers again.
	QuestionAppended QuestionState = "appended"
	// QuestionPosted is a question whose reply was posted where it was asked.
	QuestionPosted QuestionState = "posted"
	// QuestionPassedOn is a question whose reply was posted and which was passed to the controller.
	QuestionPassedOn QuestionState = "passed_on"
	// QuestionForwarded is a question handed to the agent that runs its conversation, delivered once
	// ForwardDelivered.
	QuestionForwarded QuestionState = "forwarded"
	// QuestionWoken is a question of an agent whose claim is released, given to that agent through
	// the daemon's wake row (answerloop.Wakes) in place of a reply the loop does not post: nothing
	// is posted and the controller is told nothing, and the woken agent answers it.
	QuestionWoken QuestionState = "woken"
	// QuestionDropped is a question the loop ended with nothing posted.
	QuestionDropped QuestionState = "dropped"
)

// AnswerQuestion is one question the answer loop holds (answer_questions): its id; the Legion
// project token; its identity (dispatch:<message or comment id>, or an Envoy dedupe key), which
// with the project and the agent's role token is what makes a later hand-off the same question;
// the conversation that answers it (its session, role token, issue and role); where it came from
// (Kind: message, mention, ask_reply, thread_reply, envoy); whether the agent may reply that it
// asks nothing; the hand-off as received; its state; the reply decided for it (its kind, its text,
// why it is passed on); whether a forward reached the running agent; whether its agent is given
// its message through the daemon's wake row (Wake); whether it holds a person's decision for that
// row (HoldsDecision); how many hand-offs carried it; and when it was first and last handed off
// and last changed.
//
// Wake is set for a question whose claim was released when the loop received or decided it: a
// person's decision that also asks, received for a released claim, holds the decision for the
// wake row written once the question is posted, and a reply the loop does not post for a
// released claim (one the controller would hear of, or one that asks nothing) is given to the
// agent through that row in its place.
//
// HoldsDecision is set where the question is recorded and never changed: its hand-off carries a
// person's decision on the agent's ask, and the agent's claim was released then, so the loop gave
// the decision to nobody else and holds it for the agent's wake (Wake). The wake row then carries
// the decision (answerloop.Wake.Decision), which goes to the architect or the controller if the
// claim fails or retires before the row wakes it (internal/daemon's agentMessage).
type AnswerQuestion struct {
	ID               string
	Project          string
	Identity         string
	Session          string
	Token            claim.Token
	Issue            string
	Role             claim.Role
	Kind             string
	MayAskNothing    bool
	Handoff          json.RawMessage
	State            QuestionState
	ReplyKind        string
	ReplyText        string
	PassReason       string
	ForwardDelivered bool
	Wake             bool
	HoldsDecision    bool
	Deliveries       int
	ReceivedAt       time.Time
	LastSeenAt       time.Time
	UpdatedAt        time.Time
}

const questionColumns = `id, project, identity, session, token, issue, role, kind, may_ask_nothing, handoff, state, reply_kind,
	reply_text, pass_reason, forward_delivered, wake, holds_decision, deliveries, received_at, last_seen_at, updated_at`

// ErrQuestionMoved is UpdateQuestion's refusal of a question no longer in a state the update moves
// it from: another writer moved it first.
var ErrQuestionMoved = errors.New("the question is no longer in the state the update expects")

// ReceiveQuestion records q, received at q.ReceivedAt, and returns the row and whether this call
// recorded it. A question whose project, identity and token are recorded already is the same
// message to the same agent handed off again: its deliveries and last_seen_at are bumped and the
// row is otherwise left as it is, the conversation it was first recorded for included, so a
// newer conversation of that agent does not make a second question of one message.
func (s *Store) ReceiveQuestion(ctx context.Context, q AnswerQuestion) (AnswerQuestion, bool, error) {
	var found []AnswerQuestion
	var inserted bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `insert into answer_questions (id, project, identity, session, token, issue, role, kind,
				may_ask_nothing, handoff, wake, holds_decision, state, received_at, last_seen_at, updated_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'received', $13, $13, $13)
			on conflict (project, identity, token) do update set deliveries = answer_questions.deliveries + 1,
				last_seen_at = excluded.last_seen_at
			returning `+questionColumns+`, xmax = 0`,
			q.ID, q.Project, q.Identity, q.Session, string(q.Token), q.Issue, string(q.Role), q.Kind, q.MayAskNothing, []byte(q.Handoff), q.Wake,
			q.HoldsDecision, q.ReceivedAt)
		if err != nil {
			return err
		}
		found, err = scanQuestions(rows, &inserted)
		return err
	})
	if err != nil {
		return AnswerQuestion{}, false, fmt.Errorf("record the question %s: %w", q.Identity, err)
	}
	if len(found) != 1 {
		return AnswerQuestion{}, false, fmt.Errorf("record the question %s: %d rows written, want 1", q.Identity, len(found))
	}
	return found[0], inserted, nil
}

// Question is the question id, and false when there is none.
func (s *Store) Question(ctx context.Context, id string) (AnswerQuestion, bool, error) {
	var found []AnswerQuestion
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select `+questionColumns+` from answer_questions where id = $1`, id)
		if err != nil {
			return err
		}
		found, err = scanQuestions(rows, nil)
		return err
	})
	if err != nil {
		return AnswerQuestion{}, false, fmt.Errorf("read the question %s: %w", id, err)
	}
	if len(found) == 0 {
		return AnswerQuestion{}, false, nil
	}
	return found[0], true, nil
}

// OpenQuestions is every question of the project the loop still has work on, oldest first: one
// received, generating or appended, and one forwarded that its agent's role topic has not taken.
func (s *Store) OpenQuestions(ctx context.Context, project string) ([]AnswerQuestion, error) {
	var found []AnswerQuestion
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select `+questionColumns+` from answer_questions
			where project = $1 and (state in ('received', 'generating', 'appended') or (state = 'forwarded' and not forward_delivered))
			order by received_at, id`, project)
		if err != nil {
			return err
		}
		found, err = scanQuestions(rows, nil)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read the open questions of %s: %w", project, err)
	}
	return found, nil
}

// UpdateQuestion writes q's state, reply, pass reason, forward delivery and wake, at q.UpdatedAt,
// when the question is in one of the states from; otherwise it changes nothing and is
// ErrQuestionMoved.
func (s *Store) UpdateQuestion(ctx context.Context, q AnswerQuestion, from ...QuestionState) error {
	states := make([]string, len(from))
	for i, state := range from {
		states[i] = string(state)
	}
	var updated bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update answer_questions set state = $2, reply_kind = $3, reply_text = $4, pass_reason = $5,
				forward_delivered = $6, wake = $7, updated_at = $8
			where id = $1 and state = any($9)`,
			q.ID, string(q.State), q.ReplyKind, q.ReplyText, q.PassReason, q.ForwardDelivered, q.Wake, q.UpdatedAt, states)
		updated = tag.RowsAffected() != 0
		return err
	})
	if err != nil {
		return fmt.Errorf("move the question %s to %s: %w", q.ID, q.State, err)
	}
	if !updated {
		return fmt.Errorf("move the question %s to %s from %v: %w", q.ID, q.State, from, ErrQuestionMoved)
	}
	return nil
}

// AnswerToolCalls is every recorded tool call of question, in seq order.
func (s *Store) AnswerToolCalls(ctx context.Context, question string) ([]AnswerToolCall, error) {
	var calls []AnswerToolCall
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select `+toolCallColumns+` from answer_tool_calls where question = $1 order by seq`, question)
		if err != nil {
			return err
		}
		calls, err = scanToolCalls(rows)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read the tool calls of %s: %w", question, err)
	}
	return calls, nil
}

// AnswerToolCallArgsTx is every recorded tool call of each of questions, read through tx, by
// question and in seq order, holding what a gh write is read from (ghtool.Writes) and nothing else:
// its question, seq, tool, arguments, owner and exit. Its result, the output the model read, which
// runs to gh_output_chars, is not read, and neither is the rest of its row. A question with no
// recorded call has none.
func AnswerToolCallArgsTx(ctx context.Context, tx pgx.Tx, questions []string) (map[string][]AnswerToolCall, error) {
	rows, err := tx.Query(ctx, `select question, seq, tool, args, owner, exit from answer_tool_calls where question = any($1) order by question, seq`, questions)
	if err != nil {
		return nil, fmt.Errorf("read the tool calls of %d questions: %w", len(questions), err)
	}
	defer rows.Close()
	byQuestion := make(map[string][]AnswerToolCall)
	for rows.Next() {
		var call AnswerToolCall
		var args []byte
		if err := rows.Scan(&call.Question, &call.Seq, &call.Tool, &args, &call.Owner, &call.Exit); err != nil {
			return nil, fmt.Errorf("read the tool calls of %d questions: %w", len(questions), err)
		}
		if err := json.Unmarshal(args, &call.Args); err != nil {
			return nil, fmt.Errorf("%s call %s.%d: decode its arguments: %w", call.Tool, call.Question, call.Seq, err)
		}
		byQuestion[call.Question] = append(byQuestion[call.Question], call)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the tool calls of %d questions: %w", len(questions), err)
	}
	return byQuestion, nil
}

const toolCallColumns = `question, seq, tool, args, app, owner, started_at, result, exit, finished_at`

// scanToolCalls is rows of toolCallColumns, which it closes.
func scanToolCalls(rows pgx.Rows) ([]AnswerToolCall, error) {
	defer rows.Close()
	var calls []AnswerToolCall
	for rows.Next() {
		var call AnswerToolCall
		var args []byte
		if err := rows.Scan(&call.Question, &call.Seq, &call.Tool, &args, &call.App, &call.Owner, &call.StartedAt, &call.Result, &call.Exit, &call.FinishedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(args, &call.Args); err != nil {
			return nil, fmt.Errorf("%s call %s.%d: decode its arguments: %w", call.Tool, call.Question, call.Seq, err)
		}
		calls = append(calls, call)
	}
	return calls, rows.Err()
}

// AnswerSpend is what the project's answers cost on the UTC day of at, in US dollars.
func (s *Store) AnswerSpend(ctx context.Context, project string, at time.Time) (float64, error) {
	var usd float64
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select usd from answer_spend where project = $1 and day = $2`, project, utcDay(at)).Scan(&usd)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the answer spend of %s: %w", project, err)
	}
	return usd, nil
}

// AddAnswerSpend adds usd to the project's spend on the UTC day of at.
func (s *Store) AddAnswerSpend(ctx context.Context, project string, at time.Time, usd float64) error {
	if usd < 0 {
		return fmt.Errorf("add %f USD to the answer spend of %s: a spend is never negative", usd, project)
	}
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into answer_spend (project, day, usd) values ($1, $2, $3)
			on conflict (project, day) do update set usd = answer_spend.usd + excluded.usd`, project, utcDay(at), usd)
		return err
	})
	if err != nil {
		return fmt.Errorf("add to the answer spend of %s: %w", project, err)
	}
	return nil
}

// utcDay is at's UTC calendar day, as a date column takes it.
func utcDay(at time.Time) time.Time {
	y, m, d := at.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// QuestionPassedSource is the processed_events source the daemon marks a question passed to the
// controller under, by the question's id, in the transaction that queues its agent-question notice:
// the mark is what passes a question once, and when.
const QuestionPassedSource = "agent-question"

// PassedQuestion is a question the daemon passed to the controller, holding what `legion state`
// lists of it: its id, the agent's issue, role and session, its hand-off as received (who asked
// and where), why it was passed on, and when.
type PassedQuestion struct {
	ID         string
	Issue      string
	Role       claim.Role
	Session    string
	Handoff    json.RawMessage
	PassReason string
	PassedAt   time.Time
}

// PassedQuestionsTx is the project's questions passed to the controller at since or later, newest
// first and at most limit of them, read through tx: each question with a QuestionPassedSource mark,
// at the mark's time. It reads the columns PassedQuestion holds and no other: never the reply.
func (s *Store) PassedQuestionsTx(ctx context.Context, tx pgx.Tx, project string, since time.Time, limit int) ([]PassedQuestion, error) {
	// No column of processed_events shares a name with one of answer_questions.
	rows, err := tx.Query(ctx, `select id, issue, role, session, handoff, pass_reason, processed_at
		from answer_questions join processed_events on source = $1 and event_id = id
		where project = $2 and processed_at >= $3
		order by processed_at desc, id desc limit $4`, QuestionPassedSource, project, since, limit)
	if err != nil {
		return nil, fmt.Errorf("read the questions %s passed to the controller: %w", project, err)
	}
	defer rows.Close()
	var passed []PassedQuestion
	for rows.Next() {
		var p PassedQuestion
		var role string
		var handoff []byte
		if err := rows.Scan(&p.ID, &p.Issue, &role, &p.Session, &handoff, &p.PassReason, &p.PassedAt); err != nil {
			return nil, fmt.Errorf("read the questions %s passed to the controller: %w", project, err)
		}
		p.Role, p.Handoff = claim.Role(role), handoff
		passed = append(passed, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the questions %s passed to the controller: %w", project, err)
	}
	return passed, nil
}

// scanQuestions reads each row's questionColumns, and, where inserted is not nil, the column after
// them into inserted.
func scanQuestions(rows pgx.Rows, inserted *bool) ([]AnswerQuestion, error) {
	defer rows.Close()
	var found []AnswerQuestion
	for rows.Next() {
		var q AnswerQuestion
		var token, role, state string
		var handoff []byte
		dest := []any{&q.ID, &q.Project, &q.Identity, &q.Session, &token, &q.Issue, &role, &q.Kind, &q.MayAskNothing, &handoff,
			&state, &q.ReplyKind, &q.ReplyText, &q.PassReason, &q.ForwardDelivered, &q.Wake, &q.HoldsDecision, &q.Deliveries,
			&q.ReceivedAt, &q.LastSeenAt, &q.UpdatedAt}
		if inserted != nil {
			dest = append(dest, inserted)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		q.Token, q.Role, q.State, q.Handoff = claim.Token(token), claim.Role(role), QuestionState(state), handoff
		found = append(found, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return found, nil
}

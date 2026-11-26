package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AnswerToolCall is one call of a tool the answer loop ran for a stopped agent (answer_tool_calls):
// the question it answered and the call's place among that answer's calls of every tool, the tool
// that ran it (ToolGH, ToolDispatch or ToolWebSearch), its arguments as run, for a gh call the App
// role whose token ran it and the owner whose gh directory it ran with (empty for any other tool),
// and when it started. Result, Exit and FinishedAt are what FinishAnswerToolCall wrote: Result nil
// is a call whose outcome is unknown, since the daemon stopped while it ran; Exit nil a call whose
// program never started.
type AnswerToolCall struct {
	Question   string
	Seq        int
	Tool       string
	Args       []string
	App        string
	Owner      string
	StartedAt  time.Time
	Result     *string
	Exit       *int
	FinishedAt *time.Time
}

// RefusedPrefix opens every refusal a tool of the loop answers and records as a call's result: a
// call its tool refused before its program or request started.
const RefusedPrefix = "refused: "

// RefusedBeforeRun says whether c is a call its tool refused before its program or request
// started: it finished with no exit and a result that is a refusal (RefusedPrefix).
func (c AnswerToolCall) RefusedBeforeRun() bool {
	return c.Exit == nil && c.Result != nil && strings.HasPrefix(*c.Result, RefusedPrefix)
}

// The loop's tools as answer_tool_calls names them (its tool column's check).
const (
	ToolGH        = "gh"
	ToolDispatch  = "dispatch"
	ToolWebSearch = "web_search"
)

// ErrAnswerToolCallRecorded is StartAnswerToolCall's refusal of a call whose question and seq
// already have a row: the call was made once, and is not made again under the same number.
var ErrAnswerToolCallRecorded = errors.New("is already recorded")

// StartAnswerToolCall records call before it runs, with no result. A call whose question and seq
// are recorded already is refused (ErrAnswerToolCallRecorded) and the row is left as it is.
func (s *Store) StartAnswerToolCall(ctx context.Context, call AnswerToolCall) error {
	args, err := json.Marshal(call.Args)
	if err != nil {
		return fmt.Errorf("record %s call %s.%d: encode its arguments: %w", call.Tool, call.Question, call.Seq, err)
	}
	var recorded bool
	err = Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `insert into answer_tool_calls (question, seq, tool, args, app, owner, started_at)
			values ($1, $2, $3, $4, $5, $6, $7) on conflict (question, seq) do nothing`,
			call.Question, call.Seq, call.Tool, args, call.App, call.Owner, call.StartedAt)
		recorded = tag.RowsAffected() != 0
		return err
	})
	if err != nil {
		return fmt.Errorf("record %s call %s.%d: %w", call.Tool, call.Question, call.Seq, err)
	}
	if !recorded {
		return fmt.Errorf("%s call %s.%d %w", call.Tool, call.Question, call.Seq, ErrAnswerToolCallRecorded)
	}
	return nil
}

// FinishAnswerToolCall records the outcome of the call question.seq: what the model read, its
// program's exit status (nil when it never started) and when it ended. Only a started, unfinished
// call is finished; any other is an error and nothing changes.
func (s *Store) FinishAnswerToolCall(ctx context.Context, question string, seq int, result string, exit *int, at time.Time) error {
	var finished bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update answer_tool_calls set result = $3, exit = $4, finished_at = $5
			where question = $1 and seq = $2 and result is null`, question, seq, result, exit, at)
		finished = tag.RowsAffected() != 0
		return err
	})
	if err != nil {
		return fmt.Errorf("finish the tool call %s.%d: %w", question, seq, err)
	}
	if !finished {
		return fmt.Errorf("finish the tool call %s.%d: no started, unfinished call has that number", question, seq)
	}
	return nil
}

// AnswerToolCall is the recorded call question.seq, and false when there is none.
func (s *Store) AnswerToolCall(ctx context.Context, question string, seq int) (AnswerToolCall, bool, error) {
	call := AnswerToolCall{Question: question, Seq: seq}
	var args []byte
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select tool, args, app, owner, started_at, result, exit, finished_at
			from answer_tool_calls where question = $1 and seq = $2`, question, seq).
			Scan(&call.Tool, &args, &call.App, &call.Owner, &call.StartedAt, &call.Result, &call.Exit, &call.FinishedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return AnswerToolCall{}, false, nil
	}
	if err != nil {
		return AnswerToolCall{}, false, fmt.Errorf("read the tool call %s.%d: %w", question, seq, err)
	}
	if err := json.Unmarshal(args, &call.Args); err != nil {
		return AnswerToolCall{}, false, fmt.Errorf("read the tool call %s.%d: decode its arguments: %w", question, seq, err)
	}
	return call, true, nil
}

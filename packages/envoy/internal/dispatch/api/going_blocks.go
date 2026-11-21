package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/agentwire"
)

// block is the block frame of an answer's structured reply, and false where the answer draws
// none (R2): no structured reply or kind none; a block of a kind the words do not name; a kind
// outside the seven or a spec that does not parse against its kind's schema (going.ParseBlock); a
// read the run may not make; a read that refuses or fails; or a read whose answer cannot fill the
// block, a chart of fewer than two points among them (going.Block.Fill). The server runs the read
// the spec names with the spec's arguments, as the run's author, and every number the frame
// carries is a value that read returned. A dropped block costs the answer nothing: it stays prose
// and links.
func (g *goingReadRun) block(ctx context.Context, final *going.Final, words string) (going.BlockData, bool) {
	if final == nil {
		return going.BlockData{}, false
	}
	block, err := going.ParseBlock(final.Block)
	if errors.Is(err, going.ErrNoBlock) {
		return going.BlockData{}, false
	}
	if err != nil {
		slog.Info("dispatch going: the answer's block was dropped", "kind", final.Block.Kind, "reason", err.Error())
		return going.BlockData{}, false
	}
	if !going.Drawn(block.Kind, words) {
		slog.Info("dispatch going: the answer's block was dropped", "kind", block.Kind, "reason", "the words name no block of that kind")
		return going.BlockData{}, false
	}
	read, held := g.funcs[block.Source]
	if !held {
		slog.Info("dispatch going: the answer's block was dropped", "kind", block.Kind, "reason", "the run may not make the read it names", "source", block.Source)
		return going.BlockData{}, false
	}
	answer, err := read(ctx, block.Params)
	if err != nil {
		var refused *going.ReadRefusal
		if errors.As(err, &refused) {
			slog.Info("dispatch going: the answer's block was dropped", "kind", block.Kind, "reason", "its read was refused", "source", block.Source)
		} else {
			slog.Warn("dispatch going: the read a block names failed, so the block was dropped", "kind", block.Kind, "source", block.Source, "error", err)
		}
		return going.BlockData{}, false
	}
	data, err := block.Fill(answer)
	if err != nil {
		slog.Info("dispatch going: the answer's block was dropped", "kind", block.Kind, "reason", err.Error())
		return going.BlockData{}, false
	}
	return data, true
}

// blockCall draws the block a run's model asked for (agentwire.KindBlock) from the run's saved
// tool results and from nothing else (settlement 9, R3): the server parses the spec, which holds
// no number of the model's (going.ParseRunBlock), reads the saved tool steps it names, and runs
// its chain over their rows (going.RunBlock.Draw). It answers the block it drew, which the agent
// shows as the step's block frame, so every number the block shows is the server's. A spec it
// refuses, or results that are not whole or hold no rows - a result the server cut, a list whose
// last page no named step read - are refused BLOCK_REFUSED with what to read or write instead.
func (a *AgentListener) blockCall(ctx context.Context, request agentwire.CallRequest) (agentwire.CallAnswer, *agentwire.Error) {
	block, err := going.ParseRunBlock(request.Block)
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeBlockRefused, "%v", err)
	}
	results, err := a.s.goingRunResults(ctx, request.Run, block.StepNumbers())
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	drawn, err := block.Draw(results)
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeBlockRefused, "%v", err)
	}
	body, err := json.Marshal(drawn)
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	return agentwire.CallAnswer{Status: http.StatusOK, Body: body, ContentType: "application/json"}, nil
}

// goingRunResults is the saved tool result of each of steps of run, as the agent saved it
// (agent.ReadSavedResult): a step that holds no call's answer is a result saying why, and a step
// the run did not save is none.
func (s *server) goingRunResults(ctx context.Context, run string, steps []int) ([]going.Result, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select step, body from going_run_steps where run_id = $1::uuid and kind = $2 and step = any($3::int[]) order by step
	`, run, agentwire.StepTool, steps)
	if err != nil {
		return nil, fmt.Errorf("read the saved tool steps of run %s: %w", run, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (going.Result, error) {
		var step int
		var body []byte
		if err := row.Scan(&step, &body); err != nil {
			return going.Result{}, err
		}
		saved, err := agent.ReadSavedResult(body)
		if err != nil {
			return going.Result{Step: step, Call: 1, Unread: err.Error()}, nil
		}
		result := going.Result{Step: step, Call: saved.Call, Tool: saved.Tool, Arguments: saved.Arguments, Status: saved.Status, Body: saved.Body}
		switch {
		case saved.Cut:
			result.NotWhole = fmt.Sprintf("was cut by the server at %d bytes, and what it held past them is not saved", agentwire.CallBodyLimit)
		case saved.Shortened:
			result.NotWhole = "holds only the first bytes of it you were given, which is all the step saved"
		}
		return result, nil
	})
}

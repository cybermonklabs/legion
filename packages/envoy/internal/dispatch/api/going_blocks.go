package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/sjawhar/envoy/going"
)

// block is the block frame of an answer's structured reply, and false where the answer draws
// none (R2, decision 17): no structured reply or kind none; a block of a kind the words do not name
// - a chart the decision did not settle on drawing unasked included; a kind outside the seven or
// a spec that does not parse against its kind's schema (going.ParseBlock); a read the run may not
// make; a read that refuses or fails; or a read whose answer cannot fill the block, a chart of
// fewer than two points among them (going.Block.Fill). The server runs the read the spec names
// with the spec's arguments, as the run's author, and every number the frame carries is a value
// that read returned. A dropped block costs the answer nothing: it stays prose and links.
func (g *goingReadRun) block(ctx context.Context, final *going.Final, words string, chart bool) (going.BlockData, bool) {
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
	if !going.Drawn(block.Kind, words, chart) {
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

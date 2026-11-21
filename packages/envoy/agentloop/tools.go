package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	aisdk "github.com/grafana/ai-sdk"
	"github.com/grafana/ai-sdk/schema"
)

// toolSet is the run's tools as the SDK offers and runs them, and is nil for a run that
// registers none. A schema the SDK refuses is a run the caller cannot make, and is reported
// before any model call.
func (r *run) toolSet() (aisdk.ToolSet, error) {
	if len(r.req.Tools) == 0 {
		return nil, nil
	}
	set := make(aisdk.ToolSet, len(r.req.Tools))
	for _, t := range r.req.Tools {
		input, err := schema.SchemaFromJSON(t.Schema)
		if err != nil {
			return nil, fmt.Errorf("agentloop: tool %s's schema: %w", t.Name, err)
		}
		set[t.Name] = aisdk.Tool{
			Description: t.Description,
			InputSchema: input,
			// Execute answers with the tool's output as a JSON string, which the SDK
			// reads back as the result's text for the model, and with an error for a
			// tool that refused: the SDK turns that error into error text for the
			// model and a tool error part on the step, and the run goes on. A call
			// of a tool the set does not hold the SDK answers the same way, which is
			// why an unknown tool reaches the model as the same shape of result.
			Execute: func(ctx context.Context, raw json.RawMessage, opts aisdk.ToolExecutionOptions) (json.RawMessage, error) {
				output, isError := r.call(ctx, t, opts.ToolCallID, raw)
				if isError {
					return nil, errors.New(output)
				}
				return json.Marshal(output)
			},
			// OnInputAvailable takes the call's index. The SDK calls it in the
			// stream's own goroutine, once per tool call of a step, in the order
			// the model asked for them and before any of that step's tools run, so
			// the indexes follow the model's order and not the order the calls are
			// executed in.
			OnInputAvailable: func(_ json.RawMessage, opts aisdk.ToolExecutionOptions) {
				r.index(opts.ToolCallID)
			},
		}
	}
	return set, nil
}

// call runs one call of t under the run's tool lock, held for the whole of the caller's Run, so
// a step's tool calls run one at a time. The call's index is the one it took in the order the
// model asked for it, counting on from Request.FirstToolIndex, so the first call of the run is
// that number and one. A Run that panics is recovered, logged with its stack and answered as a
// refusal naming the panic, so the run goes on: the SDK runs each tool on a goroutine of its
// own, where a panic would end the process.
func (r *run) call(ctx context.Context, t Tool, id string, input json.RawMessage) (output string, isError bool) {
	r.tool.Lock()
	defer r.tool.Unlock()
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("agentloop: a tool panicked", "tool", t.Name, "panic", recovered, "stack", string(debug.Stack()))
			output, isError = fmt.Sprintf("the tool %s failed: it panicked (%v)", t.Name, recovered), true
		}
	}()
	return t.Run(ctx, ToolCall{Index: r.index(id), ID: id, Input: input})
}

// index is one tool call's index, by the call's id: the index the call took when the model
// asked for it, or the next index where it has none, so a call the SDK ran without announcing
// its input is still numbered.
func (r *run) index(id string) int {
	r.numbering.Lock()
	defer r.numbering.Unlock()
	if i, taken := r.indexes[id]; taken {
		return i
	}
	i := r.next
	r.indexes[id] = i
	r.next++
	return i
}

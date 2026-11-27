package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"

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
			// OnInputAvailable takes the call's index and its place in the step's order.
			// The SDK calls it in the stream's own goroutine, once per tool call of a
			// step, in the order the model asked for them and before any of that step's
			// tools run, so the calls are numbered and run in the model's order.
			OnInputAvailable: func(_ json.RawMessage, opts aisdk.ToolExecutionOptions) {
				r.index(opts.ToolCallID)
				r.order.announce(opts.ToolCallID)
			},
		}
	}
	return set, nil
}

// call runs one call of t in its turn (callOrder), held for the whole of the caller's Run, so a
// step's tool calls run one at a time, in the order the model asked for them. The call's index is
// the one it took in that order, counting on from Request.FirstToolIndex, so the first call of the
// run is that number and one. A Run that panics is recovered, logged with its stack and answered
// as a refusal naming the panic, so the run goes on: the SDK runs each tool on a goroutine of its
// own, where a panic would end the process.
func (r *run) call(ctx context.Context, t Tool, id string, input json.RawMessage) (output string, isError bool) {
	r.order.take(ctx, id)
	defer r.order.give(id)
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

// callOrder runs the tool calls of one step one at a time, in the order the model asked for
// them. The SDK starts a goroutine for each call of a step once the step's stream has ended, and
// each goroutine calls the tool at once, so a lock alone would run the calls in whichever order
// the Go scheduler runs those goroutines: on a quiet processor, the one started last first.
//
// The SDK announces each call (OnInputAvailable) in the model's order before any call of the
// step runs, and each call it announces reaches Execute when the step's tools run: it announces
// no call of a tool the set does not hold and none whose input it refuses; this package sets no
// approval and registers no tool that streams; and the calls it announces and never executes, the
// ones the provider ran itself, are of a provider's own tools, which this package never offers.
// So a call the step announced waits until every call announced before it in the step has
// started and ended. A step whose finish reason runs no tool leaves its calls announced; they are
// forgotten as the next step is prepared (run.prepareStep), which the SDK does once every tool of
// the step before has returned, so they hold up nothing. A call that was never announced runs
// whenever no call runs, and takes the next index then (run.index). A call whose context is done,
// because the run was stopped or passed its time bound, waits for no call before it either, only
// for the call that runs to end: no call ever runs beside another. The zero value holds no step's
// calls and no call running.
type callOrder struct {
	mu sync.Mutex
	// running is whether a call holds the turn.
	running bool
	// announced is the calls the step in flight announced that have not ended, in the order
	// the model asked for them.
	announced []string
	// waiting is the calls that wait for their turn, in the order they came.
	waiting []*waitingCall
}

// waitingCall is one call waiting for its turn: turn is closed as the turn is handed to it, and
// late is set once its context is done, after which it waits for no call announced before it.
type waitingCall struct {
	id   string
	late bool
	turn chan struct{}
}

// announce puts the call id at the end of the step's calls. A call the step already announced
// keeps its place, as it keeps its index (run.index).
func (o *callOrder) announce(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !slices.Contains(o.announced, id) {
		o.announced = append(o.announced, id)
	}
}

// forget drops the calls the step before announced and never ran, as the next step is prepared,
// and hands the turn on to a call that waited for one of them.
func (o *callOrder) forget() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.announced = o.announced[:0]
	o.handOn()
}

// take waits for the call id's turn and holds it until give. Once ctx is done the call waits for
// the call that runs to end and for no other, so a stopped run still ends each of its calls.
func (o *callOrder) take(ctx context.Context, id string) {
	o.mu.Lock()
	if !o.running && (ctx.Err() != nil || o.due(id)) {
		o.running = true
		o.mu.Unlock()
		return
	}
	w := &waitingCall{id: id, turn: make(chan struct{})}
	o.waiting = append(o.waiting, w)
	o.mu.Unlock()
	select {
	case <-w.turn:
		return
	case <-ctx.Done():
	}
	// The turn may have been handed to the call as its context ended, in which case it is
	// running and handOn hands nothing.
	o.mu.Lock()
	w.late = true
	o.handOn()
	o.mu.Unlock()
	<-w.turn
}

// give ends the turn of the call id and hands it on.
func (o *callOrder) give(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.running = false
	if i := slices.Index(o.announced, id); i >= 0 {
		o.announced = slices.Delete(o.announced, i, i+1)
	}
	o.handOn()
}

// handOn hands the turn, while no call holds it, to the first waiting call it is due to, or
// whose context is done. Every change that can make a waiting call due ends here, so while no
// call runs no call waits that could.
func (o *callOrder) handOn() {
	if o.running {
		return
	}
	for i, w := range o.waiting {
		if w.late || o.due(w.id) {
			o.waiting = slices.Delete(o.waiting, i, i+1)
			o.running = true
			close(w.turn)
			return
		}
	}
}

// due is whether the turn, while no call holds it, is the call id's: a call the step announced
// once every call announced before it has ended, and any other call at once.
func (o *callOrder) due(id string) bool {
	return slices.Index(o.announced, id) <= 0
}

package going

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/sjawhar/envoy/agentloop"
)

// The read tools an answer may call beside the search (R8): each is a read Dispatch serves, made
// as the run's author, which the caller supplies by name in Request.Reads. The model asks for the
// read it needs rather than being handed every read with every question, and a block's spec names
// one of them as its source, which the server runs again to fill the block (blocks.go).
const (
	// ReadRoster is GET /api/v1/agents: Dispatch's copy of the listener's session list, each
	// session with its open asks and last activity, or the sessions Dispatch's own rows name when
	// the listener is unavailable.
	ReadRoster = "read_roster"
	// ReadProgress is GET /api/v1/issues' counts: an issue's task and child progress, or a
	// project's issues by status.
	ReadProgress = "read_progress"
	// ReadArchitecture is GET /api/v1/projects/{key}/architecture's per-component counts.
	ReadArchitecture = "read_architecture"
	// ReadProjectActivity is GET /api/v1/projects/{key}/activity: the events of a project's
	// issues counted by kind per ISO week.
	ReadProjectActivity = "read_project_activity"
	// ReadDelivery is GET /api/v1/delivery/measures, /timeline and /pipeline.
	ReadDelivery = "read_delivery"
)

// readMax is the most reads one run makes across every read tool. A read's output is counted
// against the run's quota estimate like a search's, and a model that asked for more than this
// many reads to answer one question is asking for the same read again; past it the tool refuses,
// in words the model can act on, and ends no run.
const readMax = 8

// Read is what one read answered: tables of flat records, which a block's spec names by table and
// field, the facts that are no table (the project read, the roster's source and age), and the
// items the read named that the answer may link, which join the run's citations as a search's
// hits do. Every number a block shows is a value of one of these tables.
type Read struct {
	Facts  map[string]any              `json:"facts,omitempty"`
	Tables map[string][]map[string]any `json:"tables"`
	Items  []Hit                       `json:"-"`
}

// ReadFunc makes one read with the arguments the model wrote, as the run's author. An error a
// ReadRefusal wraps is told to the model, which may call again; any other error is a store read
// that failed and ends the run, as a search's does.
type ReadFunc func(ctx context.Context, arguments json.RawMessage) (Read, error)

// ReadRefusal is a read the server refused for the arguments it was given - a project outside the
// run's, a project with no architecture source, a deployment with no delivery settings - and
// its Reason is what the model is told.
type ReadRefusal struct {
	Reason string
}

func (r *ReadRefusal) Error() string { return r.Reason }

// Refuse is a ReadRefusal for format.
func Refuse(format string, args ...any) error {
	return &ReadRefusal{Reason: fmt.Sprintf(format, args...)}
}

// readTool is one read tool as the model is offered it.
type readTool struct {
	name, description string
	schema            json.RawMessage
}

// readTools are the read tools in the order a call offers them. Each schema names only what the
// model may choose; the server holds the read to the run's project and the author's credential.
var readTools = []readTool{
	{
		name:        ReadRoster,
		description: "Read the agent sessions Dispatch knows of: each session's title, state, roles, open asks and last activity, the roster's source (live, or stored when the live roster is unavailable) and its age in milliseconds. It takes no arguments.",
		schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	},
	{
		name:        ReadProgress,
		description: "Read progress: for an issue, its task and child counts and its children; for a project, its issues counted by status and its open issues. Name issue, or project; with neither, the page's issue or project is read.",
		schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "project": {"type": "string", "description": "A project key such as DEMO."},
    "issue": {"type": "string", "description": "An issue key such as DEMO-12."}
  },
  "additionalProperties": false
}`),
	},
	{
		name:        ReadArchitecture,
		description: "Read a project's components, each with the issues counted for it (done and total, its own and with the components it contains), and the project's totals. With no project, the page's project is read.",
		schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "project": {"type": "string", "description": "A project key such as DEMO."}
  },
  "additionalProperties": false
}`),
	},
	{
		name:        ReadProjectActivity,
		description: "Read the events of a project's issues counted per ISO week, in total and by kind, over the last weeks (default 8, at most 52). With no project, the page's project is read.",
		schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "project": {"type": "string", "description": "A project key such as DEMO."},
    "weeks": {"type": "integer", "minimum": 1, "maximum": 52, "description": "How many weeks back, this week included."}
  },
  "additionalProperties": false
}`),
	},
	{
		name:        ReadDelivery,
		description: "Read the delivery figures: view measures (deploys, lead times, failure rates and a per-day series over a window, default the last 7 days), timeline (the window's merges and deploy runs as dated events) or pipeline (the seven days ending on to: the PR checks' and the production deploy's runs per day and the failing jobs).",
		schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "view": {"type": "string", "enum": ["measures", "timeline", "pipeline"]},
    "from": {"type": "string", "description": "The window's start, RFC3339; default seven days before to."},
    "to": {"type": "string", "description": "The window's end, RFC3339; default now."}
  },
  "required": ["view"],
  "additionalProperties": false
}`),
	},
}

// readTools is every read tool request.Reads supplies, in readTools' order.
func (r *answerRun) readTools() []agentloop.Tool {
	tools := make([]agentloop.Tool, 0, len(readTools))
	for _, tool := range readTools {
		read, supplied := r.request.Reads[tool.name]
		if !supplied {
			continue
		}
		tools = append(tools, agentloop.Tool{
			Name:        tool.name,
			Description: tool.description,
			Schema:      tool.schema,
			Run: func(ctx context.Context, call agentloop.ToolCall) (string, bool) {
				output, isError := r.readCall(ctx, read, call)
				r.toolChars.Add(int64(utf8.RuneCountInString(output)))
				return output, isError
			},
		})
	}
	return tools
}

// readCall answers one call of a read tool. Its budget is the run's across every read tool, and
// agentloop runs a turn's calls one at a time, so it needs no lock of its own; the items a read
// names join the citations under the lock the filter reads them by.
func (r *answerRun) readCall(ctx context.Context, read ReadFunc, call agentloop.ToolCall) (string, bool) {
	if r.readErr != nil || ctx.Err() != nil {
		return "This run has ended.", true
	}
	if r.reads >= readMax {
		return fmt.Sprintf("No read is left: one run reads Dispatch at most %d times. Answer from what you have, or say that Dispatch holds no answer.", readMax), false
	}
	r.reads++
	arguments := call.Input
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	answer, err := read(ctx, arguments)
	var refusal *ReadRefusal
	switch {
	case errors.As(err, &refusal):
		return "That read was refused: " + refusal.Reason, true
	case err != nil:
		// The run ends on this read, as on a search the store could not serve.
		r.readErr = err
		r.stop()
		return "That read could not be served. This run ends here.", true
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		r.readErr = err
		r.stop()
		return "That read could not be served. This run ends here.", true
	}
	r.mu.Lock()
	for _, item := range answer.Items {
		r.citations[item.Href] = item
	}
	r.mu.Unlock()
	return string(encoded), false
}

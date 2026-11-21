package answer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sjawhar/envoy/agentloop"
)

// tool is one of the loop's tools: its name, what the model reads of it and the JSON schema of
// its input, each built for the request, and what a call runs. A call answers with what the
// model is told and whether the tool refused or failed.
type tool struct {
	name        string
	description func(req Request) string
	schema      func(req Request) json.RawMessage
	run         func(ctx context.Context, req Request, call agentloop.ToolCall) (output string, isError bool)
}

// tools are the loop's tools, which the system prompt names and every model call offers.
var tools = []tool{{name: "gh", description: ghDescription, schema: ghSchema, run: runGH}}

// ghDescription is what the model reads of the gh tool for req: GitHub's own gh, which the daemon
// runs as the agent's role App; how a call is made, in the words the tool's own refusals use
// (ghtool.Usage: no checkout, so every call names its repository, the agent's repository and its
// issue's pull request, the commands the tool takes and an example); what it writes and the footer
// it adds. A description naming no command and no repository had a model probe the tool with
// --version and --help and calls naming no repository until its bound was spent.
func ghDescription(req Request) string {
	return "Run GitHub's own gh command line. The daemon runs it for you as your role's App, with its own access " +
		"on every owner your role's App is installed on, each call routed to the owner its repositories are under " +
		"now (for a renamed repository, the owner holding it). " + req.Agent.GHUsage.String() + " Use it to read " +
		"code at any commit, pull requests, reviews, checks and runs, and to write (comment, review, merge, rerun) " +
		"when the message asks for it and you judge you should. " +
		"Every comment or review it posts carries your Legion footer, which the tool adds. Commands and options unsafe " +
		"on the daemon's host are refused with the reason."
}

// ghSchema is the JSON schema of the gh tool's input for req: gh's arguments as an array of
// strings, whose description gives the description's example (ghtool.Usage.Example).
func ghSchema(req Request) json.RawMessage {
	example, _ := json.Marshal(req.Agent.GHUsage.Example())
	var schema ghArgsSchema
	schema.Type = "object"
	schema.Properties.Args.Type = "array"
	schema.Properties.Args.Items.Type = "string"
	schema.Properties.Args.Description = "gh's arguments without the leading gh, one per element; no shell reads them. For example " +
		string(example) + "."
	schema.Required = []string{"args"}
	encoded, _ := json.Marshal(schema)
	return encoded
}

// ghArgsSchema is the shape of the gh tool's input schema (ghSchema): one property, args, an array
// of strings, required, and no other property. It is one Go value marshalled once, so no text is
// joined into JSON by hand. The order of its keys decides nothing: agentloop's Anthropic and
// Responses adapters send a tool's input schema with its keys sorted (agentloop's doc.go, "The
// structured reply"), and the input has one property.
type ghArgsSchema struct {
	Type       string `json:"type"`
	Properties struct {
		Args struct {
			Type  string `json:"type"`
			Items struct {
				Type string `json:"type"`
			} `json:"items"`
			Description string `json:"description"`
		} `json:"args"`
	} `json:"properties"`
	Required             []string `json:"required"`
	AdditionalProperties bool     `json:"additionalProperties"`
}

// toolSet is the loop's tools as agentloop offers and runs them for req, each call's output
// given to add as the model reads it (inFront.add). The project's pause is read before each call
// (Request.Paused): a call it finds paused does not run, and answers a refusal. agentloop runs a
// step's calls one at a time and finishes the step only once they have all returned, so once the
// pause comes every call of the step still to run is refused, and OnStep then ends the run before
// its next model call (answer).
func toolSet(req Request, add func(string)) []agentloop.Tool {
	set := make([]agentloop.Tool, len(tools))
	for i, t := range tools {
		set[i] = agentloop.Tool{
			Name:        t.name,
			Description: t.description(req),
			Schema:      t.schema(req),
			Run: func(ctx context.Context, call agentloop.ToolCall) (string, bool) {
				if req.paused() {
					return "refused: the Legion project is paused, so the call did not run.", true
				}
				output, isError := t.run(ctx, req, call)
				add(output)
				return output, isError
			},
		}
	}
	return set
}

// runGH runs one gh call through the request's GH, numbered as agentloop numbered the call: the
// calls of the run counted on from the last one an earlier run of the question made
// (Request.Earlier), which is the seq the call is recorded and the gh_calls bound is read
// against. What the model reads is gh's output, with its exit status when it failed; a call gh
// never ran, or one that failed, is an error result.
func runGH(ctx context.Context, req Request, call agentloop.ToolCall) (string, bool) {
	var input struct {
		Args json.RawMessage `json:"args"`
	}
	args := string(call.Input)
	if json.Unmarshal(call.Input, &input) == nil && len(input.Args) > 0 {
		args = string(input.Args)
	}
	result := req.GH.Run(ctx, call.Index, args)
	switch {
	case !result.Ran:
		return result.Output, true
	case result.Exit < 0:
		// A signal ended gh, which answered no status: its output already says so (ghtool's runGH).
		return result.Output, true
	case result.Exit != 0:
		return fmt.Sprintf("%s\n[gh exited %d]", result.Output, result.Exit), true
	default:
		return result.Output, false
	}
}

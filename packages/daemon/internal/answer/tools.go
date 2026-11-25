package answer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/sjawhar/envoy/agentloop"

	"github.com/sjawhar/legion/daemon/internal/dispatchtool"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/websearch"
)

// tool is one of the loop's tools: its name, what the model reads of it and the JSON schema of
// its input, each built for the request, whether the request offers it (nil: every request does),
// whether a call writes, by its tool's own reading of the call's arguments (nil: no call of the
// tool writes), and what a call runs, given its number among the answer's calls of this tool. A
// call answers with what the model is told and whether the tool refused or failed.
type tool struct {
	name        string
	description func(req Request) string
	schema      func(req Request) json.RawMessage
	offered     func(req Request) bool
	writes      func(call agentloop.ToolCall) bool
	run         func(ctx context.Context, req Request, call agentloop.ToolCall, n int) (output string, isError bool)
}

// tools are the loop's tools, which the system prompt names and every model call of an answer
// offers where its request does (Request.offeredTools): gh, dispatch where the daemon found the
// dispatch command, and web_search where answers.web_search configures it. A gh call writes as
// the gh tool reads it (ghtool.CallWrites), a dispatch call as the dispatch tool does
// (dispatchtool.CallWrites), and a web search is a read.
var tools = []tool{
	{name: store.ToolGH, description: ghDescription, schema: ghSchema, run: runGH,
		writes: func(call agentloop.ToolCall) bool { return ghtool.CallWrites(argsOf(call)) }},
	{name: store.ToolDispatch, description: dispatchDescription, schema: dispatchSchema, run: runDispatch,
		offered: func(req Request) bool { return req.Dispatch != nil },
		writes:  func(call agentloop.ToolCall) bool { return dispatchtool.CallWrites(argsOf(call)) }},
	{name: store.ToolWebSearch, description: webSearchDescription, schema: webSearchSchema, run: runWebSearch,
		offered: func(req Request) bool { return req.WebSearch != nil }},
}

// offeredTools are the tools req offers, in the list's order.
func (req Request) offeredTools() []tool {
	var offered []tool
	for _, t := range tools {
		if t.offered == nil || t.offered(req) {
			offered = append(offered, t)
		}
	}
	return offered
}

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
		Args argsProperty `json:"args"`
	} `json:"properties"`
	Required             []string `json:"required"`
	AdditionalProperties bool     `json:"additionalProperties"`
}

// argsProperty is the args property of the gh and dispatch tools' input schemas: an array of
// strings, the tool's arguments, with the description the model reads.
type argsProperty struct {
	Type  string `json:"type"`
	Items struct {
		Type string `json:"type"`
	} `json:"items"`
	Description string `json:"description"`
}

// dispatchDescription is what the model reads of the dispatch tool for req: Dispatch's own
// `dispatch` command, which the daemon runs as the agent's own session with that session's access;
// how a call is made, with one worked example, a read; the commands it runs
// (dispatchtool.ReadCommands and WriteCommands), and that every other command, and an option
// handing it a file of the daemon's host, is refused; when a write is for, how its text is given,
// and the loop's mark it adds to what it posts; that it refuses a write aimed where the daemon
// posts the reply itself, in the words of that refusal (dispatchtool's placeRefusal); and that
// Dispatch's output can carry advice for a running agent, which the stopped agent takes no action
// on: the CLI has no switch that leaves it out (packages/envoy-client's renderAdvice).
//
// It gives no write call ready to run. Proof run 45683361567 showed a model the one comment the
// description then gave, its stdin "Your comment.", and the model's first call was that comment,
// run as a probe with the stdin "placeholder", which posted.
func dispatchDescription(req Request) string {
	read, _ := json.Marshal(dispatchInput{Args: []string{"read", req.Agent.Issue}})
	return "Run Dispatch's own `dispatch` command, the one your pane runs. The daemon runs it as your own session, " +
		req.Agent.Session + ", with that session's access. Dispatch answers each call as it answers you when you run. " +
		"Give dispatch's arguments without the leading dispatch. " +
		"The `read` command shows an issue with its documents, comments, asks and messages, including those written since you stopped. " +
		"For example: " + string(read) + ".\n\n" +
		"The tool runs these reads: " + strings.Join(dispatchtool.ReadCommands, ", ") + ". " +
		"It runs these writes: " + strings.Join(dispatchtool.WriteCommands, ", ") + ". " +
		"Every other command is refused with the reason, for example a status write, a claim, an ask or a document edit. " +
		"An option that hands dispatch a file of the daemon's host is refused too: a `--<field>-file` or `--<field>-json-file` " +
		"whose value is not `-`, or `--image`.\n\n" +
		"Use a write only if the message explicitly asks you to post other content somewhere else, or to resolve a comment. " +
		"Give a write's text as the value of `--body`, or give it as the call's stdin with `--body-file -`. " +
		"An option `--<field>-file -` reads the call's stdin. " +
		"Every comment, message and agent's message that the tool posts carries the loop's mark, which the tool adds.\n\n" +
		"The daemon posts your reply where the message reached you. " +
		"Never use `comment`, `message` or `send-to-agent` to post your answer or a copy of it. " +
		"The tool refuses a write aimed where the daemon posts your reply to this question itself. " +
		"That place is the same comment thread, the same ask, the Dispatch message that you reply to, or the Envoy session that asked. " +
		"Such a write would be a second reply there. Put what you would post there in your reply: the daemon posts it there.\n\n" +
		"Dispatch's output can carry advice written for an agent that is running, for example to close an ask or to move an issue's status. " +
		"You can do neither now. Take no action on that advice.\n\n" +
		"`dispatch <command> --help` prints a command's options. Use it only before a command that the message asks for."
}

// dispatchInput is the dispatch tool's input as the description's example, a read, is written:
// its arguments, and no stdin.
type dispatchInput struct {
	Args []string `json:"args"`
}

// dispatchSchema is the JSON schema of the dispatch tool's input: dispatch's arguments as an array
// of strings, required, as gh's are (ghArgsSchema), and stdin, the call's standard input, a string
// the model may leave out.
func dispatchSchema(Request) json.RawMessage {
	var schema dispatchArgsSchema
	schema.Type = "object"
	schema.Properties.Args.Type = "array"
	schema.Properties.Args.Items.Type = "string"
	schema.Properties.Args.Description = "dispatch's arguments without the leading dispatch, one per element; no shell reads them."
	schema.Properties.Stdin.Type = "string"
	schema.Properties.Stdin.Description = "The call's standard input, which an option --<field>-file - reads, such as a " +
		"comment's body under --body-file -. Leave it out for none."
	schema.Required = []string{"args"}
	encoded, _ := json.Marshal(schema)
	return encoded
}

// dispatchArgsSchema is the shape of the dispatch tool's input schema (dispatchSchema): args, an
// array of strings, required, and stdin, a string, and no other property. It is one Go value
// marshalled once, as ghArgsSchema is.
type dispatchArgsSchema struct {
	Type       string `json:"type"`
	Properties struct {
		Args  argsProperty `json:"args"`
		Stdin struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"stdin"`
	} `json:"properties"`
	Required             []string `json:"required"`
	AdditionalProperties bool     `json:"additionalProperties"`
}

// webSearchDescription is what the model reads of the web_search tool: one web search, which the
// daemon runs; that it is for facts the agent's conversation and its other tools do not hold; its
// bound, websearch.MaxSearches an answer; and that the reply names the address of each source it
// uses.
func webSearchDescription(Request) string {
	return "Search the web. The daemon runs one web search for you, on Anthropic's web search, and answers with what " +
		"the search found and the address of each source. Use it only for facts your conversation and your other tools " +
		fmt.Sprintf("do not hold, such as a public project's current release or an outside service's documentation. You may search at most %d times in this answer. ", websearch.MaxSearches) +
		"Your reply must name the address of each source you use."
}

// webSearchSchema is the JSON schema of the web_search tool's input: one property, query, a
// string, required, and no other property.
func webSearchSchema(Request) json.RawMessage {
	var schema webSearchArgsSchema
	schema.Type = "object"
	schema.Properties.Query.Type = "string"
	schema.Properties.Query.Description = "What to search the web for, as you would write it into a search engine."
	schema.Required = []string{"query"}
	encoded, _ := json.Marshal(schema)
	return encoded
}

// webSearchArgsSchema is the shape of the web_search tool's input schema (webSearchSchema): query,
// a string, required, and no other property. It is one Go value marshalled once, as ghArgsSchema
// is.
type webSearchArgsSchema struct {
	Type       string `json:"type"`
	Properties struct {
		Query struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"query"`
	} `json:"properties"`
	Required             []string `json:"required"`
	AdditionalProperties bool     `json:"additionalProperties"`
}

// pausedRefusal is what the model reads of a gh or dispatch call that writes while the project is
// paused (toolSet): that it did not run, why, and what to tell the asker.
const pausedRefusal = store.RefusedPrefix + "the Legion project is paused, and until it resumes the daemon makes no write in your name, so this call, which writes, did not run. " +
	"Your reads still run: answer from what they show, and say in your reply that this write was not made because the project is paused."

// toolSet is the loop's tools req offers as agentloop offers and runs them, each call's output
// given to add as the model reads it (inFront.add). The project's pause is read before each call
// of a tool whose calls may write, and only while it is paused is the call read for whether it
// writes (tool.writes, the call's own tool's reading of it): one that writes does not run, its
// tool is never handed it, and it answers pausedRefusal as an error result. Every other call
// runs, a read and a web search while the project is paused among them.
//
// Each call is handed its number among the answer's calls of its own tool, from 1, counted on
// from the calls of that tool an earlier run of the question made (Request.Earlier), which is what
// that tool's bound reads: an answer's gh calls, its dispatch calls and its web searches are
// counted apart, while every call's seq, the one agentloop numbers it with, counts the calls of
// every tool.
func toolSet(req Request, add func(string)) []agentloop.Tool {
	offered := req.offeredTools()
	set := make([]agentloop.Tool, len(offered))
	for i, t := range offered {
		made := &atomic.Int64{}
		for _, call := range req.Earlier {
			if call.Tool == t.name {
				made.Add(1)
			}
		}
		set[i] = agentloop.Tool{
			Name:        t.name,
			Description: t.description(req),
			Schema:      t.schema(req),
			Run: func(ctx context.Context, call agentloop.ToolCall) (string, bool) {
				if t.writes != nil && req.paused() && t.writes(call) {
					return pausedRefusal, true
				}
				output, isError := t.run(ctx, req, call, int(made.Add(1)))
				add(output)
				return output, isError
			},
		}
	}
	return set
}

// runGH runs one gh call through the request's GH, numbered as agentloop numbered the call: the
// calls of the run counted on from the last one an earlier run of the question made
// (Request.Earlier), which is the seq the call is recorded under, and n its number among the
// answer's gh calls, which the gh_calls bound is read against. What the model reads is gh's
// output, with its exit status when it failed; a call gh never ran, or one that failed, is an
// error result.
func runGH(ctx context.Context, req Request, call agentloop.ToolCall, n int) (string, bool) {
	result := req.GH.Run(ctx, GHCall{Seq: call.Index, N: n, Args: argsOf(call)})
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

// runDispatch runs one dispatch call through the request's Dispatch, numbered as runGH numbers a gh
// call, n its number among the answer's dispatch calls, with the input's arguments and stdin. What
// the model reads is dispatch's output, with its exit status when it failed; a call dispatch never
// ran, one a signal ended, or one that failed, is an error result.
func runDispatch(ctx context.Context, req Request, call agentloop.ToolCall, n int) (string, bool) {
	var input struct {
		Stdin string `json:"stdin"`
	}
	_ = json.Unmarshal(call.Input, &input)
	result := req.Dispatch.Run(ctx, dispatchtool.Call{Seq: call.Index, N: n, Args: argsOf(call), Stdin: input.Stdin})
	switch {
	case !result.Ran, result.Exit < 0:
		return result.Output, true
	case result.Exit != 0:
		return fmt.Sprintf("%s\n[dispatch exited %d]", result.Output, result.Exit), true
	default:
		return result.Output, false
	}
}

// runWebSearch runs one web_search call through the request's WebSearch, numbered as runGH numbers
// a gh call, n its number among the answer's web searches, with the model's input as it gave it,
// which the tool reads its query from. What the model reads is the search's result, a refused or
// failed search's as an error result.
func runWebSearch(ctx context.Context, req Request, call agentloop.ToolCall, n int) (string, bool) {
	result := req.WebSearch.Run(ctx, websearch.Call{Seq: call.Index, N: n, Input: string(call.Input)})
	return result.Output, result.IsError
}

// argsOf is a gh or dispatch call's arguments as its tool is handed them: the input's args, a
// JSON array of strings, else the whole input, which the tool then refuses as no such array.
func argsOf(call agentloop.ToolCall) string {
	var input struct {
		Args json.RawMessage `json:"args"`
	}
	if json.Unmarshal(call.Input, &input) == nil && len(input.Args) > 0 {
		return string(input.Args)
	}
	return string(call.Input)
}

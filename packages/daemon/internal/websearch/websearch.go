// Package websearch is the `web_search` tool of the daemon's answer loop: one web search, which
// the daemon runs for a stopped agent as one call of Anthropic's Messages API on
// answers.web_search.model with Anthropic's own web search server tool, allowed one search, and
// answers with the reply's text and the address of each source it names. Per answer, Open makes
// one question's tool; Run runs one call the model makes.
//
// A call runs only while its answer has made fewer than MaxSearches web_search calls, an earlier
// run's counted, and is recorded before its request is made and finished after (answer_tool_calls,
// its tool web_search). Its cost is its tokens at the model's answers.prices entry plus the searches
// it made times answers.web_search.price_per_search, which the answer adds to its own: a gateway's
// cost header (LiteLLM's x-litellm-response-cost) does not count the search fee, so the daemon's
// own sum is the cost.
package websearch

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// MaxSearches is the most web_search calls one answer makes, an earlier run's calls of the same
// question counted, as each tool's own bound counts them; a call past it is refused before any
// request is made.
const MaxSearches = 3

// UnknownOutcome is the result a restart records for a search the daemon stopped while it ran: it
// is neither made again nor looked up anywhere, and the model is told its result is unknown.
const UnknownOutcome = "The daemon stopped while this search ran; its result is unknown."

// Host is what every answer's searches run with. Endpoint is answers.providers.anthropic: the base
// URL every search is posted to and the key it carries, read again for each call
// (config.AnswerCredential.Read). Model is the model's id at Anthropic, answers.web_search.model
// without its `anthropic/`; Price is its answers.prices entry and PricePerSearch
// answers.web_search.price_per_search. Bounds are answers.bounds, whose output_tokens is each
// search call's max_tokens and gh_output_chars the most of its result the model reads. Calls
// records each call (ghtool.CallRecorder), Log takes its line, Now is the clock.
type Host struct {
	Endpoint       config.AnswerEndpoint
	Model          string
	Price          config.AnswerPrice
	PricePerSearch float64
	Bounds         config.AnswerBounds
	Calls          ghtool.CallRecorder
	Log            *slog.Logger
	Now            func() time.Time
}

// Answer is one answer's web_search tool, Open's: the host and the question its calls are
// recorded under.
type Answer struct {
	host     Host
	question string
	client   *http.Client
}

// Open makes question's web_search tool under host.
func Open(host Host, question string) (*Answer, error) {
	switch {
	case host.Endpoint.BaseURL == "" || host.Model == "":
		return nil, errors.New("the web_search tool's host names no Anthropic endpoint or no model")
	case host.Bounds.OutputTokens <= 0 || host.Bounds.GHOutputChars <= 0:
		return nil, errors.New("the web_search tool's host has no output_tokens or gh_output_chars bound (answers.bounds)")
	case host.Calls == nil || host.Log == nil || host.Now == nil:
		return nil, errors.New("the web_search tool's host lacks its recorder, log or clock")
	case question == "":
		return nil, errors.New("the web_search tool names no question")
	}
	// The key goes on every request, so a redirect is never followed, as the loop's own model
	// calls follow none (agentloop's client): the call fails instead, naming the status.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Answer{host: host, question: question, client: client}, nil
}

// Call is one web_search call the model makes in an answer: its seq among the answer's calls of
// every tool, from 1, which its record is kept under; its number among the answer's web_search
// calls alone, from 1, an earlier run's counted, which MaxSearches is read against; and its input
// as the model gave it, `{"query": "<text>"}`.
type Call struct {
	Seq   int
	N     int
	Input string
}

// Result is what the model reads of a call, whether that is an error result, and what the call
// cost: the searches it made, counted from the reply's result blocks, and its USD, tokens and
// searches together. A call refused or failed before the provider answered costs nothing.
type Result struct {
	Output   string
	IsError  bool
	Searches int
	CostUSD  float64
}

// Run runs call. It is refused, with no request made, when its input names no query, or its
// number among the answer's searches is past MaxSearches. Every other call makes one Messages
// call (search) within ctx, the answer's own time. Every call is recorded with what the model
// reads of it: an allowed one before its request is made and finished after, with exit 0 for a
// search the provider answered and 1 for one that failed, a refused one with its refusal and no
// exit; one whose start cannot be recorded makes no request. Each logs one `answers: web_search`
// line.
func (a *Answer) Run(ctx context.Context, call Call) Result {
	started := a.host.Now()
	var input struct {
		Query *string `json:"query"`
	}
	if err := json.Unmarshal([]byte(call.Input), &input); err != nil || input.Query == nil || strings.TrimSpace(*input.Query) == "" {
		return a.refuse(ctx, call.Seq, call.Input, started, fmt.Sprintf(`the web_search tool takes {"query": "<text>"}, a query that is not empty (%q is none)`, call.Input))
	}
	query := *input.Query
	switch {
	case call.Seq < 1 || call.N < 1:
		return a.refuse(ctx, call.Seq, query, started, fmt.Sprintf("call %d is no call of the answer: calls are numbered from 1", call.Seq))
	case call.N > MaxSearches:
		return a.refuse(ctx, call.Seq, query, started, fmt.Sprintf("this answer has made its %d web searches, the most one answer makes; answer with what they found", MaxSearches))
	}
	if err := a.host.Calls.StartAnswerToolCall(ctx, a.record(call.Seq, query, started)); err != nil {
		result := Result{Output: fmt.Sprintf("%sthe search could not be recorded, so it did not run: %v", store.RefusedPrefix, err), IsError: true}
		a.log(call.Seq, query, result)
		return result
	}
	result := a.search(ctx, query)
	exit := 0
	if result.IsError {
		exit = 1
	}
	// The request was made: its record is finished whatever the answer's own deadline did meanwhile.
	if err := a.host.Calls.FinishAnswerToolCall(context.WithoutCancel(ctx), a.question, call.Seq, result.Output, &exit, a.host.Now()); err != nil {
		a.host.Log.Error("answers: web_search call not finished in its record", "question", a.question, "seq", call.Seq, "error", err)
	}
	a.log(call.Seq, query, result)
	return result
}

// anthropicVersion is the Messages API version every search names, the one the SDK the loop's own
// Anthropic calls go through sends (anthropic-sdk-go's requestconfig), so a gateway that takes the
// loop's calls takes these.
const anthropicVersion = "2023-06-01"

// maxReplyBytes bounds the reply a search reads: a reply past it is a failed search, never one the
// daemon holds whole.
const maxReplyBytes = 8 << 20

// searchRequest is a search's Messages request: the model, max_tokens, one user message, and
// Anthropic's web search server tool allowed one search.
type searchRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []searchMessage `json:"messages"`
	Tools     []serverTool    `json:"tools"`
}

type searchMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// serverTool is Anthropic's web search server tool, which the provider runs itself.
type serverTool struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	MaxUses int    `json:"max_uses"`
}

// webSearchTool is the server tool every search offers: one search per call, so the call's cost
// is bounded by one search's fee.
var webSearchTool = serverTool{Type: "web_search_20250305", Name: "web_search", MaxUses: 1}

// prompt is the one user message of a search of query.
func prompt(query string) string {
	return "Search the web for: " + query + "\n\nAnswer briefly from what the search finds, and name the address of each source you use."
}

// search makes the one Messages call of a search of query, with the key read again now and the
// headers the loop's own Anthropic calls carry, and reads its reply (read). A key that cannot be
// read, a request that fails, a status other than 200 and a body that is no Messages response are
// each a failed search naming the cause, which costs nothing.
func (a *Answer) search(ctx context.Context, query string) Result {
	key, err := a.host.Endpoint.Credential.Read(ctx)
	if err != nil {
		return failed("the provider's key: " + err.Error())
	}
	body, err := json.Marshal(searchRequest{
		Model: a.host.Model, MaxTokens: a.host.Bounds.OutputTokens,
		Messages: []searchMessage{{Role: "user", Content: prompt(query)}},
		Tools:    []serverTool{webSearchTool},
	})
	if err != nil {
		return failed(err.Error())
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.host.Endpoint.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return failed(err.Error())
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Anthropic-Version", anthropicVersion)
	request.Header.Set("X-Api-Key", key)
	response, err := a.client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return failed("it ran past the answer's time (answers.bounds.answer_seconds)")
		}
		if ctx.Err() != nil {
			return failed("the answer stopped while it ran")
		}
		return failed(redact("Anthropic could not be reached: "+err.Error(), key))
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxReplyBytes+1))
	switch {
	case err != nil:
		return failed(redact("its reply could not be read: "+err.Error(), key))
	case response.StatusCode != http.StatusOK:
		return failed(redact(statusCause(response.StatusCode, raw), key))
	case len(raw) > maxReplyBytes:
		return failed(fmt.Sprintf("its reply ran past %d bytes", maxReplyBytes))
	}
	var reply messageReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return failed("Anthropic's reply is no Messages response: " + err.Error())
	}
	return a.read(reply)
}

// failed is the error result of a search that failed for cause, with no cost: one before the
// provider answered costs nothing, and read gives one the provider answered its reply's cost.
func failed(cause string) Result {
	return Result{Output: "the web search failed: " + cause, IsError: true}
}

// redact is text with key, where it holds it, put out of it: no result and no line carries the
// key.
func redact(text, key string) string {
	if key == "" {
		return text
	}
	return strings.ReplaceAll(text, key, "[the provider's key]")
}

// statusCause names an answer of status, with the error's type and message where its body is the
// Messages API's error object.
func statusCause(status int, body []byte) string {
	var answer struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	cause := fmt.Sprintf("Anthropic answered HTTP %d", status)
	if json.Unmarshal(body, &answer) == nil && answer.Error.Type != "" {
		cause += ": " + answer.Error.Type + ": " + answer.Error.Message
	}
	return cause
}

// messageReply is what a search reads of a Messages response: its content blocks and its usage.
type messageReply struct {
	Content []contentBlock `json:"content"`
	Usage   struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// contentBlock is one block of the reply: a text block with its citations, or a
// web_search_tool_result block, whose content is the list of results of a search that ran or the
// error object of one that failed.
type contentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Citations []struct {
		URL string `json:"url"`
	} `json:"citations"`
	Content json.RawMessage `json:"content"`
}

// read is what the model reads of reply and what the search cost. The text is the reply's text
// blocks joined: Anthropic splits one answer into blocks at each citation, so adjacent blocks run
// on, and a block after any other kind of block starts a paragraph. After it come the address of
// each source the reply's result blocks and its text's citations name, each once, in the order the
// reply names them. A result block holding an error makes the search an error result naming the
// error's code, but for max_uses_exceeded beside a search that ran: the Messages API answers that
// code for a second search the model attempted past max_uses 1, and the first search's text and
// sources are kept. The searches are the result blocks holding a list of results, however many it
// lists, and never the reply's usage counter; a block holding an error is a failed search, which
// is not charged. The result is cut at gh_output_chars as a gh call's output is.
func (a *Answer) read(reply messageReply) Result {
	var text strings.Builder
	var sources, errorCodes []string
	searches := 0
	afterOther := false
	addSource := func(url string) {
		if url != "" && !slices.Contains(sources, url) {
			sources = append(sources, url)
		}
	}
	for _, block := range reply.Content {
		switch block.Type {
		case "text":
			if afterOther && text.Len() > 0 {
				text.WriteString("\n\n")
			}
			afterOther = false
			text.WriteString(block.Text)
			for _, citation := range block.Citations {
				addSource(citation.URL)
			}
		case "web_search_tool_result":
			afterOther = true
			var results []struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(block.Content, &results); err == nil {
				searches++
				for _, result := range results {
					addSource(result.URL)
				}
				continue
			}
			var failure struct {
				ErrorCode string `json:"error_code"`
			}
			_ = json.Unmarshal(block.Content, &failure)
			errorCodes = append(errorCodes, cmp.Or(failure.ErrorCode, "with no code"))
		default:
			afterOther = true
		}
	}
	u := reply.Usage
	const perToken = 1e-6
	cost := (float64(u.InputTokens)*a.host.Price.Input + float64(u.OutputTokens)*a.host.Price.Output +
		float64(u.CacheCreationInputTokens)*a.host.Price.CacheWrite + float64(u.CacheReadInputTokens)*a.host.Price.CacheRead) * perToken
	cost += float64(searches) * a.host.PricePerSearch
	var result Result
	answer := strings.TrimSpace(text.String())
	switch {
	case len(errorCodes) > 0 && (searches == 0 || slices.ContainsFunc(errorCodes, func(code string) bool { return code != maxUsesExceeded })):
		result = failed("the search answered the error " + strings.Join(errorCodes, ", "))
	case answer == "" && len(sources) == 0:
		result = failed("its reply holds no text and no source")
	default:
		if len(sources) > 0 {
			answer += "\n\nSources:\n" + strings.Join(sources, "\n")
		}
		out := ghtool.NewCappedOutput(a.host.Bounds.GHOutputChars)
		_, _ = io.WriteString(out, strings.TrimSpace(answer))
		result.Output = out.String()
	}
	result.Searches, result.CostUSD = searches, cost
	return result
}

// maxUsesExceeded is the error code of a result block for a search the model attempted past the
// server tool's max_uses, which the Messages API answers in place of that search's results.
const maxUsesExceeded = "max_uses_exceeded"

// refuse records and logs a call refused before any request was made, its argument arg, and
// answers its refusal (ghtool.RecordRefusal).
func (a *Answer) refuse(ctx context.Context, seq int, arg string, started time.Time, why string) Result {
	result := Result{Output: ghtool.RecordRefusal(ctx, a.host.Calls, a.host.Log, a.host.Now, a.record(seq, arg, started), why), IsError: true}
	a.log(seq, arg, result)
	return result
}

// record is the row a call starts with: the web_search tool's, its query its one argument, run as
// no GitHub App and with no owner's gh directory.
func (a *Answer) record(seq int, query string, started time.Time) store.AnswerToolCall {
	return store.AnswerToolCall{Question: a.question, Seq: seq, Tool: store.ToolWebSearch, Args: []string{query}, StartedAt: started}
}

// log is the call's one line: the question and seq, the query, the searches it made and what it
// cost, and for a failed or refused call what the model read of it. The key is in no line.
func (a *Answer) log(seq int, query string, result Result) {
	attrs := []any{"question", a.question, "seq", seq, "query", query, "searches", result.Searches, "costUSD", result.CostUSD}
	if result.IsError {
		attrs = append(attrs, "error", result.Output)
	}
	a.host.Log.Info("answers: web_search", attrs...)
}

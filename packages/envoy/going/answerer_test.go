package going_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logtest"
)

// testTokensPerMinute is a quota far past anything one test's prompts cost, so every test but the
// quota's own exercises the pacer without ever waiting on it.
const testTokensPerMinute = 300_000

// testBearer is the bearer the test model's endpoint reads; a static one is the whole credential
// these tests need.
const testBearer = "going-test-bearer"

// The two items a test's seeding search finds, and one target no citation set holds: the filter
// admits a link by its href alone, so the href is what a reply links.
var (
	routeHit = going.Hit{
		Kind:  "issue",
		ID:    "DEMO-12",
		Title: "The answer route",
		Href:  "/issues/DEMO-12",
	}
	planHit = going.Hit{
		Kind:  "artifact",
		ID:    "plan-md",
		Title: "The plan",
		Href:  "/issues/DEMO-12/artifact/plan-md",
	}
)

const outsideAnswerHref = "/issues/DEMO-99"

// errStreamWriter is the failure a test's stream writer hands back, which is the one failure Run
// reports to its caller.
var errStreamWriter = errors.New("the reader's connection is gone")

// answerStream is everything one run emitted, so a test reads the stream as a reader does, and
// the outcome the run reported. It fails at the event failAt names, by its number or by its
// name, which is how a test proves a run hands the writer's failure back.
type answerStream struct {
	events   []going.Event
	failAt   int
	failName string
	outcomes []going.Outcome
}

func (s *answerStream) emit(event going.Event) error {
	number := len(s.events) + 1
	if (s.failAt > 0 && number == s.failAt) || (s.failName != "" && event.Name == s.failName) {
		return errStreamWriter
	}
	s.events = append(s.events, event)
	return nil
}

// outcome records what the run said its answer came to, and the frames it had emitted by then.
func (s *answerStream) outcome(outcome going.Outcome) error {
	s.outcomes = append(s.outcomes, outcome)
	return nil
}

// reported is the one outcome the run reported, and fails the test when it reported none or more.
func (s *answerStream) reported(t *testing.T) going.Outcome {
	t.Helper()
	if len(s.outcomes) != 1 {
		t.Fatalf("the run reported %d outcomes, want exactly 1: %+v", len(s.outcomes), s.outcomes)
	}
	return s.outcomes[0]
}

// names is every event's name in the order it was emitted.
func (s *answerStream) names() []string {
	out := make([]string, len(s.events))
	for i, event := range s.events {
		out[i] = event.Name
	}
	return out
}

// text is the whole answer a reader saw, which a dropped link must hold no byte of.
func (s *answerStream) text() string {
	var out strings.Builder
	for _, event := range s.events {
		if data, ok := event.Data.(going.TextData); ok {
			out.WriteString(data.Text)
		}
	}
	return out.String()
}

// count is how many events of that name the stream holds.
func (s *answerStream) count(name string) int {
	found := 0
	for _, event := range s.events {
		if event.Name == name {
			found++
		}
	}
	return found
}

// first is the first event of that name, and fails the test when the stream holds none.
func (s *answerStream) first(t *testing.T, name string) going.Event {
	t.Helper()
	for _, event := range s.events {
		if event.Name == name {
			return event
		}
	}
	t.Fatalf("stream holds no %s event, only %v", name, s.names())
	return going.Event{}
}

// terminal checks the one rule every run keeps - exactly one terminal event, and last - and
// answers it.
func (s *answerStream) terminal(t *testing.T) going.Event {
	t.Helper()
	terminals := 0
	for _, event := range s.events {
		if event.Terminal() {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("stream holds %d terminal events, want exactly 1: %v", terminals, s.names())
	}
	last := s.events[len(s.events)-1]
	if !last.Terminal() {
		t.Fatalf("stream ends with %s, want its terminal event last: %v", last.Name, s.names())
	}
	return last
}

// ended checks the stream ended with end, an answer.
func (s *answerStream) ended(t *testing.T) {
	t.Helper()
	if got := s.terminal(t).Name; got != contracts.GoingEventEnd {
		t.Fatalf("stream ended with %s, want %s: %v", got, contracts.GoingEventEnd, s.names())
	}
}

// code is the error frame's own code, and fails the test when the run ended any other way.
func (s *answerStream) code(t *testing.T) string {
	t.Helper()
	event := s.terminal(t)
	data, ok := event.Data.(going.ErrorData)
	if !ok {
		t.Fatalf("stream ended with %s %+v, want an error frame", event.Name, event.Data)
	}
	return data.Code
}

// reads is the two reads a run makes through its caller: it answers every search with the same
// hits and counts the words each search was made with, which is how a test sees the run's own
// seeding search counted as the first.
type reads struct {
	hits    []going.Hit
	bodies  map[string]string
	queries []string
	err     error
	// served is how many searches are answered before err is: zero fails the run's own seeding
	// search, and one fails the first search the model asks for.
	served int
}

// request is one question over these reads, whose outcome stream records.
func (r *reads) request(words string, stream *answerStream) going.Request {
	return going.Request{
		Words: words,
		Search: func(ctx context.Context, q string) ([]going.Hit, error) {
			r.queries = append(r.queries, q)
			// A real store read answers the context's own ending first (pgx does), which is
			// what makes a read taken after the run was stopped context.Canceled rather than
			// the store's own refusal.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if r.err != nil && len(r.queries) > r.served {
				return nil, r.err
			}
			return r.hits, nil
		},
		Bodies: func(_ context.Context, hits []going.Hit) (map[string]string, error) {
			found := make(map[string]string, len(hits))
			for _, hit := range hits {
				if body, ok := r.bodies[hit.Key()]; ok {
					found[hit.Key()] = body
				}
			}
			return found, nil
		},
		OnOutcome: stream.outcome,
	}
}

// testReads is the question's own hits, each with a short body.
func testReads() *reads {
	return &reads{
		hits: []going.Hit{routeHit, planHit},
		bodies: map[string]string{
			routeHit.Key(): "The route answers one question from what Dispatch holds.",
			planHit.Key():  "The plan counts the server's own search as the first of three.",
		},
	}
}

// testModel is the answering model at model's endpoint, on the protocol a deployment's chat
// model speaks, so every test drives agentloop's own provider, route, sign-in and reading of the
// stream.
func testModel(model *goingtest.Model) agentloop.Model {
	return agentloop.Model{
		Endpoint: agentloop.Endpoint{
			Kind:    agentloop.OpenAIChat,
			BaseURL: model.URL + "/openai/v1",
			Token:   func(context.Context) (string, error) { return testBearer, nil },
		},
		ID: "going-test-deployment",
	}
}

// testAnswerer builds an Answerer over the real model loop against model.
func testAnswerer(t *testing.T, model *goingtest.Model, cfg going.AnswererConfig) *going.Answerer {
	t.Helper()
	if cfg.Model.ID == "" {
		cfg.Model = testModel(model)
	}
	if cfg.TokensPerMinute == 0 {
		cfg.TokensPerMinute = testTokensPerMinute
	}
	answerer, err := going.NewAnswerer(cfg)
	if err != nil {
		t.Fatalf("NewAnswerer: %v", err)
	}
	return answerer
}

// answer is a turn that answers with text, as the call asks for it: the Final object with no
// block when it asks for one, the text as prose when it does not.
func answer(text string) goingtest.Reply {
	return goingtest.Reply{Final: &goingtest.Final{Answer: text, Arguments: "{}"}}
}

// searchCall is a turn that calls the one tool with q.
func searchCall(id, q string) goingtest.Reply {
	return goingtest.Reply{ToolCalls: []goingtest.ToolCall{{
		ID:        id,
		Name:      contracts.GoingSearchTool,
		Arguments: fmt.Sprintf(`{"q":%q}`, q),
	}}}
}

// answerWords is a reply of n distinct words, so a cut can be located in the answer.
func answerWords(n int) string {
	var out strings.Builder
	for i := range n {
		if i > 0 {
			out.WriteByte(' ')
		}
		fmt.Fprintf(&out, "word%d", i)
	}
	return out.String()
}

// TestNewAnswererRefusesAnAnswererWithoutAModel: an answerer with no model, or one whose endpoint
// reads no credential, is a programming error, refused at boot rather than at the first question.
func TestNewAnswererRefusesAnAnswererWithoutAModel(t *testing.T) {
	model := testModel(goingtest.NewModel(t))
	tokenless := model
	tokenless.Endpoint.Token = nil
	for name, cfg := range map[string]going.AnswererConfig{
		"no model":      {TokensPerMinute: testTokensPerMinute},
		"no credential": {Model: tokenless},
	} {
		if _, err := going.NewAnswerer(cfg); err == nil {
			t.Errorf("%s: NewAnswerer returned no error", name)
		}
	}
}

// TestAnswerThroughAgentloop: one real call through agentloop, asked for the Final object, whose
// answer streams as the answer property grows: the reader is given it in several text frames
// while the model writes, each cleared by the filter - the link to a hit the model was given
// reaches the reader as the item frame and its own visible text, and markdown punctuation is
// escaped. The block and the arguments are handed to the caller and are no frame of the stream.
func TestAnswerThroughAgentloop(t *testing.T) {
	link := fmt.Sprintf("[%s](%s)", routeHit.Title, routeHit.Href)
	spec := `{"series":[{"name":"opened","points":[3,5,2]}]}`
	arguments := `{"dispatch_claim":[{"key":"DEMO-12"}]}`
	model := goingtest.NewModel(t, goingtest.Reply{
		Final: &goingtest.Final{
			Answer:    "The order is settled on " + link + ", and it is *final* for now.",
			Block:     goingtest.Block{Kind: "chart", Title: "Opened a week", Spec: spec},
			Arguments: arguments,
		},
		Chunk: 5,
	})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	search := testReads()

	if err := answerer.Run(t.Context(), search.request("who owns the broadcast order", stream), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stream.ended(t)
	if got := stream.count(contracts.GoingEventText); got < 2 {
		t.Errorf("the answer reached the reader in %d text frames, want several: it streams as the answer property grows (%v)", got, stream.names())
	}
	text := stream.text()
	if !strings.Contains(text, routeHit.Title) {
		t.Errorf("answer = %q, want it to hold the link's own text %q", text, routeHit.Title)
	}
	if strings.Contains(text, routeHit.Href) || strings.Contains(text, "](") {
		t.Errorf("answer = %q, want no link syntax in a text frame: the item frame is the link", text)
	}
	if !strings.Contains(text, `\*final\*`) {
		t.Errorf("answer = %q, want the emphasis escaped as plain text", text)
	}
	for _, leaked := range []string{`"answer"`, `"block"`, spec, "Opened a week", "dispatch_claim"} {
		if strings.Contains(text, leaked) {
			t.Errorf("answer = %q, want none of the object around the answer, but it holds %q", text, leaked)
		}
	}
	if got := stream.count(contracts.GoingEventItem); got != 1 {
		t.Fatalf("stream holds %d item events, want 1: %v", got, stream.names())
	}
	item, ok := stream.first(t, contracts.GoingEventItem).Data.(going.ItemData)
	if !ok || item.Href != routeHit.Href || item.ID != routeHit.ID {
		t.Errorf("item = %+v, want the hit the answer linked (%s %s)", item, routeHit.ID, routeHit.Href)
	}
	if got := stream.count(contracts.GoingEventChanged) + stream.count(contracts.GoingEventNone); got != 0 {
		t.Errorf("stream holds %d changed or none events, want none: %v", got, stream.names())
	}

	// The one call asked for the Final object, as a json_schema response format, with the
	// question, the items, the search tool and the stated output bound.
	requests := model.Requests()
	if len(requests) != 1 {
		t.Fatalf("model served %d calls, want 1", len(requests))
	}
	var format struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Schema struct {
				Required []string `json:"required"`
			} `json:"schema"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(requests[0].ResponseFormat, &format); err != nil || format.Type != "json_schema" ||
		!reflect.DeepEqual(format.JSONSchema.Schema.Required, []string{"answer", "block", "arguments"}) {
		t.Errorf("response_format = %s (%v), want the Final schema as a json_schema format", requests[0].ResponseFormat, err)
	}
	if requests[0].MaxTokens <= 0 {
		t.Errorf("max_tokens = %d, want the run's stated output bound", requests[0].MaxTokens)
	}
	if requests[0].Authorization != "Bearer "+testBearer {
		t.Errorf("Authorization = %q, want the endpoint's token as a bearer", requests[0].Authorization)
	}
	if len(requests[0].Tools) != 1 || requests[0].Tools[0].Name != contracts.GoingSearchTool {
		t.Errorf("tools = %+v, want the one search tool", requests[0].Tools)
	}
	if len(search.queries) != 1 || search.queries[0] != "who owns the broadcast order" {
		t.Errorf("searches = %q, want the one seeding search with the question's words", search.queries)
	}

	// The caller is handed the reply whole: the block and the arguments the stream carries no
	// frame of.
	outcome := stream.reported(t)
	if outcome.StructuredOutput != going.StructuredOutputAccepted || outcome.Final == nil {
		t.Fatalf("outcome = %+v, want the structured reply accepted", outcome)
	}
	wantBlock := going.FinalBlock{Kind: "chart", Title: "Opened a week", Spec: spec}
	if outcome.Final.Block != wantBlock || outcome.Final.Arguments != arguments {
		t.Errorf("final = %+v, want block %+v and arguments %s", outcome.Final, wantBlock, arguments)
	}
	if blocks, known := answerer.Blocks(); !blocks || !known {
		t.Errorf("Blocks = %v, %v, want true once a structured reply was accepted", blocks, known)
	}
}

// TestAnswerThroughAgentloopCutsAtTheWordCap: an answer property far past the cap is cut at
// contracts.GoingAnswerWordMax, cut says so with that count, and end still follows - a cut answer
// is an answer.
func TestAnswerThroughAgentloopCutsAtTheWordCap(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{Final: &goingtest.Final{Answer: answerWords(300), Arguments: "{}"}, Chunk: 7})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}

	if err := answerer.Run(t.Context(), testReads().request("how long may an answer be", stream), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stream.ended(t)
	cut, ok := stream.first(t, contracts.GoingEventCut).Data.(going.CutData)
	if !ok || cut.Words != contracts.GoingAnswerWordMax {
		t.Errorf("cut = %+v, want the answer cut at %d words", cut, contracts.GoingAnswerWordMax)
	}
	if got := len(strings.Fields(stream.text())); got != contracts.GoingAnswerWordMax {
		t.Errorf("answer carries %d words, want %d", got, contracts.GoingAnswerWordMax)
	}
}

// TestAnswerThroughAgentloopDropsALinkOutsideTheCitationSet: a link to something the model was
// never given reaches a reader as its own text and no more - no target, no link syntax, no item
// - and the stream says the answer was changed.
func TestAnswerThroughAgentloopDropsALinkOutsideTheCitationSet(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{
		Final: &goingtest.Final{Answer: "The order is settled on [another issue](" + outsideAnswerHref + ") instead.", Arguments: "{}"},
		Chunk: 6,
	})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}

	if err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order", stream), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stream.ended(t)
	changed, ok := stream.first(t, contracts.GoingEventChanged).Data.(going.ChangedData)
	if !ok || changed.Dropped != 1 {
		t.Errorf("changed = %+v, want one dropped link", changed)
	}
	if got := stream.text(); strings.Contains(got, outsideAnswerHref) || strings.Contains(got, "](") {
		t.Errorf("answer = %q, want no target and no link syntax of the link the server dropped", got)
	}
	if got := stream.count(contracts.GoingEventItem); got != 0 {
		t.Errorf("stream holds %d item events, want none for an answer that cited nothing", got)
	}
}

// TestRunSaysNoneWhenTheModelWroteNothing: none is the model producing no answer text at all,
// and nothing else. A run whose model answered with no text says none, then ends; a run whose
// answer linked nothing is still an answer, and its end's items of 0 is what says it rests on
// no source.
func TestRunSaysNoneWhenTheModelWroteNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		none bool
	}{
		{"the model wrote nothing", "", true},
		{"the answer links nothing", "Dispatch holds no answer to that question.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := goingtest.NewModel(t, answer(tc.text))
			answerer := testAnswerer(t, model, going.AnswererConfig{})
			stream := &answerStream{}

			if err := answerer.Run(t.Context(), testReads().request("what did nobody write down", stream), stream.emit); err != nil {
				t.Fatalf("Run: %v", err)
			}

			end, ok := stream.terminal(t).Data.(going.EndData)
			if !ok || end.Items != 0 {
				t.Errorf("end = %+v, want an end citing nothing", end)
			}
			want := 0
			if tc.none {
				want = 1
			}
			if got := stream.count(contracts.GoingEventNone); got != want {
				t.Errorf("stream holds %d none events, want %d: %v", got, want, stream.names())
			}
			if tc.none && stream.names()[len(stream.names())-2] != contracts.GoingEventNone {
				t.Errorf("stream ends %v, want %s before its end", stream.names(), contracts.GoingEventNone)
			}
		})
	}
}

// TestStructuredOutputRefusedByA400FallsBackToProse: a service that refuses the structured
// request with a 400 is asked the same again for prose, which streams through the filter as the
// structured answer does; the run has no block and no arguments and reports the refusal, and the
// process now knows its model draws no block.
func TestStructuredOutputRefusedByA400FallsBackToProse(t *testing.T) {
	link := fmt.Sprintf("[%s](%s)", routeHit.Title, routeHit.Href)
	model := goingtest.NewModel(t, goingtest.Reply{
		Final: &goingtest.Final{
			Answer:    "It is settled on " + link + " and on [another](" + outsideAnswerHref + "), *for now*.",
			Block:     goingtest.Block{Kind: "chart", Title: "never drawn", Spec: "{}"},
			Arguments: "{}",
		},
		RefuseFormat: true,
		Chunk:        5,
	})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}

	if err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order", stream), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stream.ended(t)
	requests := model.Requests()
	if len(requests) != 2 || !requests[0].Structured() || requests[1].Structured() {
		t.Fatalf("model served %d calls, want the refused structured one then one asking for prose", len(requests))
	}
	assertProseAnswer(t, stream)
	if blocks, known := answerer.Blocks(); blocks || !known {
		t.Errorf("Blocks = %v, %v, want false once the service refused a structured request", blocks, known)
	}
}

// TestStructuredOutputRefusedForAnotherCauseFallsBackForThatRunAlone: a 400 whose error names no
// structured output - a content filter's, a context too long - is that run's alone: the run
// answers in prose, and the next run asks for the Final object again and gets blocks.
func TestStructuredOutputRefusedForAnotherCauseFallsBackForThatRunAlone(t *testing.T) {
	model := goingtest.NewModel(t,
		goingtest.Reply{Status: http.StatusBadRequest,
			Message: "The response was filtered due to the prompt triggering the content management policy."},
		goingtest.Reply{Final: &goingtest.Final{Answer: "Dispatch holds no answer to that question."}},
		goingtest.Reply{Final: &goingtest.Final{Answer: "Dispatch holds no answer to that question.", Arguments: "{}"}},
	)
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	for run := range 2 {
		stream := &answerStream{}
		if err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order", stream), stream.emit); err != nil {
			t.Fatalf("Run %d: %v", run, err)
		}
		stream.ended(t)
		if run == 0 {
			if _, known := answerer.Blocks(); known {
				t.Error("Blocks is known after a content filter's 400, want unknown: the service never refused the structured output")
			}
		}
	}
	requests := model.Requests()
	if len(requests) != 3 || !requests[0].Structured() || requests[1].Structured() || !requests[2].Structured() {
		t.Fatalf("model served %d calls, want the refused structured one, its prose fallback, then the next run's structured one", len(requests))
	}
	if blocks, known := answerer.Blocks(); !blocks || !known {
		t.Errorf("Blocks = %v, %v after the next run's structured reply was accepted, want true", blocks, known)
	}
}

// TestRunTellsTheStructuredCallTheOffer: the structured call is told each offered action with its
// argument schema and the chart the decision settled on, which is what lets it write their
// arguments and the chart's block; the prose call that follows a refusal is told neither, since
// prose carries no arguments and no block. The chart is offered because the run holds a read that
// fills one (TestNoBlockIsOfferedWithoutAReadThatFillsIt is the other side).
func TestRunTellsTheStructuredCallTheOffer(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{
		Final:        &goingtest.Final{Answer: "Claimed.", Arguments: `{"dispatch_claim":[{"issue":"DEMO-12"}]}`},
		RefuseFormat: true,
	})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	request := testReads().request("claim DEMO-12", stream)
	request.Reads = map[string]going.ReadFunc{going.ReadProjectActivity: func(context.Context, json.RawMessage) (going.Read, error) {
		return going.Read{}, nil
	}}
	request.Offer = going.Offer{
		Actions: []going.OfferedAction{
			{Name: "dispatch_claim", Schema: json.RawMessage(`[{"name":"issue","kind":"string","required":true}]`)},
			{Name: "dispatch_comment", Schema: json.RawMessage(`[{"name":"body","kind":"string","required":true}]`)},
		},
		Display: going.DisplayChart,
	}
	if err := answerer.Run(t.Context(), request, stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stream.ended(t)
	requests := model.Requests()
	if len(requests) != 2 {
		t.Fatalf("model served %d calls, want the structured one then the prose one", len(requests))
	}
	told := func(request goingtest.ReceivedRequest) string {
		for _, message := range request.Messages {
			if message.Role == "user" {
				return message.Content
			}
		}
		return ""
	}
	structured, prose := told(requests[0]), told(requests[1])
	for _, want := range []string{
		"name: dispatch_claim", `schema: [{"name":"issue","kind":"string","required":true}]`,
		"name: dispatch_comment", `schema: [{"name":"body","kind":"string","required":true}]`,
		"chart block",
	} {
		if !strings.Contains(structured, want) {
			t.Errorf("the structured call was told %q, want %q in it", structured, want)
		}
		if strings.Contains(prose, want) {
			t.Errorf("the prose call was told %q, want no %q: prose carries no arguments and no block", prose, want)
		}
	}
	if !strings.Contains(prose, "claim DEMO-12") {
		t.Errorf("the prose call was told %q, want the question", prose)
	}
}

// TestStructuredOutputRefusedByAMalformedReplyFallsBackForThatRunAlone: a service that ignores
// the response format answers 200 with a reply that does not fit the schema, and the run asks
// again for prose; one such reply says less about the service than a 400, so the next run asks
// for the Final object again.
func TestStructuredOutputRefusedByAMalformedReplyFallsBackForThatRunAlone(t *testing.T) {
	link := fmt.Sprintf("[%s](%s)", routeHit.Title, routeHit.Href)
	model := goingtest.NewModel(t, goingtest.Reply{
		Text:  "It is settled on " + link + " and on [another](" + outsideAnswerHref + "), *for now*.",
		Chunk: 5,
	})
	answerer := testAnswerer(t, model, going.AnswererConfig{})

	for run := range 2 {
		stream := &answerStream{}
		if err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order", stream), stream.emit); err != nil {
			t.Fatalf("Run %d: %v", run, err)
		}
		stream.ended(t)
		assertProseAnswer(t, stream)
	}
	requests := model.Requests()
	if len(requests) != 4 {
		t.Fatalf("model served %d calls over two runs, want two each: a malformed reply sets no flag", len(requests))
	}
	for i, request := range requests {
		if want := i%2 == 0; request.Structured() != want {
			t.Errorf("call %d asked for a structured reply: %v, want %v", i, request.Structured(), want)
		}
	}
	if _, known := answerer.Blocks(); known {
		t.Error("Blocks is known after two malformed replies, want unknown: neither a 400 nor an accepted reply was seen")
	}
}

// assertProseAnswer checks a run that fell back to prose: its text was cleared by the filter as
// a structured answer's is - the given link kept as its item, the other dropped, punctuation
// escaped - and its outcome is the refusal, with no reply for the caller to draw a block from.
func assertProseAnswer(t *testing.T, stream *answerStream) {
	t.Helper()
	text := stream.text()
	if !strings.Contains(text, routeHit.Title) || !strings.Contains(text, `\*for now\*`) {
		t.Errorf("answer = %q, want the prose cleared and escaped", text)
	}
	if strings.Contains(text, outsideAnswerHref) || strings.Contains(text, "](") {
		t.Errorf("answer = %q, want no link syntax and no target", text)
	}
	if got := stream.count(contracts.GoingEventItem); got != 1 {
		t.Errorf("stream holds %d item events, want the one the prose linked", got)
	}
	if got := stream.count(contracts.GoingEventChanged); got != 1 {
		t.Errorf("stream holds %d changed events, want the dropped link said", got)
	}
	outcome := stream.reported(t)
	if outcome.StructuredOutput != going.StructuredOutputRefused || outcome.Final != nil {
		t.Errorf("outcome = %+v, want the refusal and no structured reply", outcome)
	}
}

// TestStructuredOutputRefusedOnceMakesTheNextRunOneCall: once the service refused a structured
// request with a 400, a later run of the same process asks for prose alone, so a deployment
// whose model serves no structured output pays one call an answer and not two (c20).
func TestStructuredOutputRefusedOnceMakesTheNextRunOneCall(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{
		Final:        &goingtest.Final{Answer: "Dispatch holds no answer to that question.", Arguments: "{}"},
		RefuseFormat: true,
	})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	first := &answerStream{}
	if err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order", first), first.emit); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	first.ended(t)
	if got := model.Calls(); got != 2 {
		t.Fatalf("the first run made %d calls, want the refused structured one and the prose one", got)
	}

	second := &answerStream{}
	if err := answerer.Run(t.Context(), testReads().request("and who answers it", second), second.emit); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	second.ended(t)
	requests := model.Requests()
	if len(requests) != 3 {
		t.Fatalf("the second run made %d calls, want 1: the refusal was remembered", len(requests)-2)
	}
	if requests[2].Structured() {
		t.Error("the second run asked for a structured reply, want prose alone after a 400")
	}
	if outcome := second.reported(t); outcome.StructuredOutput != going.StructuredOutputRefused || outcome.Final != nil {
		t.Errorf("the second run's outcome = %+v, want the refusal", outcome)
	}
	if !strings.Contains(second.text(), "Dispatch holds no answer") {
		t.Errorf("the second run's answer = %q, want the prose", second.text())
	}
}

// TestRunCountsTheSeedingSearchAsTheFirst: the server's own search is the first of
// contracts.GoingSearchMax, so a model that calls the tool twice is served twice and its third
// call comes back as a refusal it can act on, with the run still ending in an answer.
func TestRunCountsTheSeedingSearchAsTheFirst(t *testing.T) {
	model := goingtest.NewModel(t,
		searchCall("call_1", "the broadcast order"),
		searchCall("call_2", "who owns it"),
		searchCall("call_3", "once more"),
		answer(fmt.Sprintf("It is on [%s](%s).", routeHit.Title, routeHit.Href)),
	)
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	search := testReads()

	if err := answerer.Run(t.Context(), search.request("who owns the broadcast order", stream), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stream.ended(t)
	want := []string{"who owns the broadcast order", "the broadcast order", "who owns it"}
	if len(search.queries) != contracts.GoingSearchMax || !reflect.DeepEqual(search.queries, want) {
		t.Errorf("searches = %q, want the %d of %v", search.queries, contracts.GoingSearchMax, want)
	}
	if got := model.Calls(); got != contracts.GoingModelCallMax {
		t.Fatalf("model served %d calls, want the three tool turns and the answer", got)
	}
	refusal := toolResult(t, model.Requests()[3].Messages, "call_3")
	if !strings.Contains(refusal, "No search is left") {
		t.Errorf("the third tool call was answered %q, want a refusal the model can act on", refusal)
	}
	if got := stream.count(contracts.GoingEventItem); got != 1 {
		t.Errorf("stream holds %d item events, want the one the answer linked", got)
	}
	if outcome := stream.reported(t); outcome.StructuredOutput != going.StructuredOutputAccepted {
		t.Errorf("outcome = %+v, want the structured reply accepted after the tool turns", outcome)
	}
}

// toolResult is the result one tool call was answered with, as the model received it on the turn
// after it.
func toolResult(t *testing.T, messages []goingtest.Message, callID string) string {
	t.Helper()
	for _, message := range messages {
		if message.ToolCallID == callID {
			return message.Content
		}
	}
	t.Fatalf("the turn carried no result for tool call %s", callID)
	return ""
}

// TestRunTellsTheModelACallFailedInTheResultsOwnText: a refused tool call - one that names no
// words to search for - reaches the model as a result it can act on, and the model answers from
// what it has, so such a result ends no run.
func TestRunTellsTheModelACallFailedInTheResultsOwnText(t *testing.T) {
	model := goingtest.NewModel(t,
		goingtest.Reply{ToolCalls: []goingtest.ToolCall{{
			ID:        "call_1",
			Name:      contracts.GoingSearchTool,
			Arguments: `{"q":"   "}`,
		}}},
		answer(fmt.Sprintf("It is on [%s](%s).", routeHit.Title, routeHit.Href)),
	)
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	search := testReads()

	if err := answerer.Run(t.Context(), search.request("who owns the broadcast order", stream), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stream.ended(t)
	if got := model.Calls(); got != 2 {
		t.Fatalf("model served %d calls, want the refused tool turn and the answer", got)
	}
	refusal := toolResult(t, model.Requests()[1].Messages, "call_1")
	if !strings.Contains(refusal, "Call it again with q") {
		t.Errorf("the failed call was answered %q, want the words the model can act on", refusal)
	}
	if len(search.queries) != 1 {
		t.Errorf("searches = %q, want the seeding search alone: the call named no words", search.queries)
	}
}

// TestRunEndsWithTheDeadlineWhenTheModelNeverAnswers: a model that holds its response open past
// the caller's deadline ends the run with the issue's one deadline code, and the run takes the
// time the caller gave it rather than ending early.
func TestRunEndsWithTheDeadlineWhenTheModelNeverAnswers(t *testing.T) {
	const window = 250 * time.Millisecond
	model := goingtest.NewModel(t, goingtest.Reply{Silent: true})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	ctx, cancel := context.WithTimeout(t.Context(), window)
	defer cancel()

	start := time.Now()
	err := answerer.Run(ctx, testReads().request("who owns the broadcast order", stream), stream.emit)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stream.code(t); got != contracts.GoingErrorAnswerDidNotArrive {
		t.Errorf("error code = %s, want %s", got, contracts.GoingErrorAnswerDidNotArrive)
	}
	if elapsed < window {
		t.Errorf("the run ended %s in, before its %s deadline", elapsed, window)
	}
	if got := model.Calls(); got != 1 {
		t.Errorf("model served %d calls, want the one the deadline ended: no fallback follows a deadline", got)
	}
}

// TestRunEndsWithTheDeadlineWhenTheQuotaCannotPaceTheCall: a quota too small to pace the next
// call inside the time left ends the run the same way, and no call is made: the quota is taken
// before the request goes out.
func TestRunEndsWithTheDeadlineWhenTheQuotaCannotPaceTheCall(t *testing.T) {
	frozen := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	model := goingtest.NewModel(t, answer("An answer nobody will see."))
	answerer := testAnswerer(t, model, going.AnswererConfig{
		TokensPerMinute: 1,
		Now:             func() time.Time { return frozen },
	})
	stream := &answerStream{}

	if err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order", stream), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.code(t); got != contracts.GoingErrorAnswerDidNotArrive {
		t.Errorf("error code = %s, want %s", got, contracts.GoingErrorAnswerDidNotArrive)
	}
	if got := model.Calls(); got != 0 {
		t.Errorf("model served %d calls, want none: the quota could pace none of them", got)
	}
}

// TestRunFailsWhenTheModelRefusesTheCall: a model service that answers a status other than a
// 400 ends the run with the failure code, with no fallback, and the reader is told nothing the
// service said.
func TestRunFailsWhenTheModelRefusesTheCall(t *testing.T) {
	model := goingtest.NewModel(t, goingtest.Reply{Status: http.StatusUnauthorized})
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}

	if err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order", stream), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.code(t); got != contracts.GoingErrorAnswerFailed {
		t.Errorf("error code = %s, want %s", got, contracts.GoingErrorAnswerFailed)
	}
	if got := model.Calls(); got != 1 {
		t.Errorf("model served %d calls, want 1: a 401 is no refusal of the structured output", got)
	}
	if _, known := answerer.Blocks(); known {
		t.Error("Blocks is known after a 401, want unknown")
	}
}

// TestRunFailsWhenTheStoreCannotBeRead: the seeding search is a store read, and a store that
// cannot answer it ends the run with the failure code.
func TestRunFailsWhenTheStoreCannotBeRead(t *testing.T) {
	model := goingtest.NewModel(t, answer("An answer nobody will see."))
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	search := testReads()
	search.err = errors.New("the store refused this read")

	if err := answerer.Run(t.Context(), search.request("who owns the broadcast order", stream), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.code(t); got != contracts.GoingErrorAnswerFailed {
		t.Errorf("error code = %s, want %s", got, contracts.GoingErrorAnswerFailed)
	}
	if got := model.Calls(); got != 0 {
		t.Errorf("model served %d calls, want none: there was nothing to show it", got)
	}
}

// TestRunEndsWhenAToolsSearchCannotBeRead: a search the model asked for that the store could not
// serve ends the run there, with the failure code and no further model call. The run must not do
// both: stream the answer a further call would write and then disown it with ANSWER_FAILED.
func TestRunEndsWhenAToolsSearchCannotBeRead(t *testing.T) {
	model := goingtest.NewModel(t,
		searchCall("call_1", "who owns it"),
		answer("An answer nobody should ever be written."),
	)
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	search := testReads()
	// The run's own seeding search is served; the one the model asks for is not.
	search.served = 1
	search.err = errors.New("the store refused this read")

	if err := answerer.Run(t.Context(), search.request("who owns the broadcast order", stream), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.code(t); got != contracts.GoingErrorAnswerFailed {
		t.Errorf("error code = %s, want %s", got, contracts.GoingErrorAnswerFailed)
	}
	if got := model.Calls(); got != 1 {
		t.Errorf("model served %d calls, want the one turn that called the tool: the run ended on that search", got)
	}
	if got := stream.count(contracts.GoingEventText); got != 0 {
		t.Errorf("stream holds %d text events, want none: the run ended before any answer was written", got)
	}
}

// TestRunEndsOnTheFirstFailedReadOfATurnsCalls: a turn that calls the tool twice and whose first
// search the store could not serve ends on that read - the reader is told ANSWER_FAILED, the
// second call is never run, and the store's own error is logged once. The second search would
// run under the context the first one ended, so it could only answer context.Canceled: a run
// that let it would tell the reader its answer did not arrive in time, which is not what
// happened, and would say nothing at all about the store.
func TestRunEndsOnTheFirstFailedReadOfATurnsCalls(t *testing.T) {
	var logged bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	model := goingtest.NewModel(t,
		goingtest.Reply{ToolCalls: []goingtest.ToolCall{
			{ID: "call_1", Name: contracts.GoingSearchTool, Arguments: `{"q":"who owns it"}`},
			{ID: "call_2", Name: contracts.GoingSearchTool, Arguments: `{"q":"and who answers it"}`},
		}},
		answer("An answer nobody should ever be written."),
	)
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	search := testReads()
	// The run's own seeding search is served; both searches of the turn after it are not.
	search.served = 1
	search.err = errors.New("the store refused this read")

	if err := answerer.Run(t.Context(), search.request("who owns the broadcast order", stream), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stream.code(t); got != contracts.GoingErrorAnswerFailed {
		t.Errorf("error code = %s, want %s: the store failed, the run did not run out of time", got, contracts.GoingErrorAnswerFailed)
	}
	if len(search.queries) != 2 {
		t.Errorf("searches = %q, want the seeding search and the first of the turn's two calls", search.queries)
	}
	if got := model.Calls(); got != 1 {
		t.Errorf("model served %d calls, want the one turn that called the tool: the run ended on that search", got)
	}
	if got := strings.Count(logged.String(), search.err.Error()); got != 1 {
		t.Errorf("the store's error is logged %d times in %q, want once", got, logged.String())
	}
}

// TestRunGivesAHitPastTheRuneBudgetNoBody: the bodies are spent in rank order against the run's
// one budget of contracts.GoingRequestRuneMax runes, and the hit past it still reaches the model
// with its kind, its id and its title - what the model received is what the test asserts.
func TestRunGivesAHitPastTheRuneBudgetNoBody(t *testing.T) {
	const marker = "marker-of-item-"
	// One hit more than the budget has room for, each body cut to the per-hit cap as the caller
	// hands them over.
	count := contracts.GoingRequestRuneMax/contracts.GoingHitRuneMax + 1
	search := &reads{bodies: make(map[string]string, count)}
	for i := range count {
		hit := going.Hit{
			Kind:  "issue",
			ID:    fmt.Sprintf("DEMO-%d", 2000+i),
			Title: fmt.Sprintf("the item ranked %d", i),
			Href:  fmt.Sprintf("/issues/DEMO-%d", 2000+i),
		}
		search.hits = append(search.hits, hit)
		body := fmt.Sprintf("%s%d.", marker, i)
		search.bodies[hit.Key()] = body + strings.Repeat("x", contracts.GoingHitRuneMax-len(body))
	}
	model := goingtest.NewModel(t, answer("Dispatch holds no answer to that question."))
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}

	if err := answerer.Run(t.Context(), search.request("what do the ranked items say", stream), stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stream.terminal(t)
	requests := model.Requests()
	if len(requests) != 1 {
		t.Fatalf("model served %d calls, want 1", len(requests))
	}
	var asked string
	for _, message := range requests[0].Messages {
		if message.Role == "user" {
			asked = message.Content
		}
	}
	last := search.hits[count-1]
	if !strings.Contains(asked, last.ID) || !strings.Contains(asked, last.Title) {
		t.Errorf("the model was not given %s %q at all; it must be named with no body", last.ID, last.Title)
	}
	if got := fmt.Sprintf("%s%d.", marker, count-1); strings.Contains(asked, got) {
		t.Errorf("the model was given the body %q of the hit past the run's rune budget", got)
	}
	if got := fmt.Sprintf("%s%d.", marker, count-2); !strings.Contains(asked, got) {
		t.Errorf("the model was not given the body %q, which the budget had room for", got)
	}
}

// TestRunHandsBackTheStreamWritersFailure: the writer's failure is the one failure Run reports to
// its caller, whether it comes on the first frame or on the last, and nothing is emitted after it.
func TestRunHandsBackTheStreamWritersFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream *answerStream
	}{
		{"on the first frame", &answerStream{failAt: 1}},
		{"on the terminal frame", &answerStream{failName: contracts.GoingEventEnd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := goingtest.NewModel(t, goingtest.Reply{
				Final: &goingtest.Final{Answer: fmt.Sprintf("It is on [%s](%s).", routeHit.Title, routeHit.Href), Arguments: "{}"},
				Chunk: 5,
			})
			answerer := testAnswerer(t, model, going.AnswererConfig{})

			err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order", tc.stream), tc.stream.emit)

			if !errors.Is(err, errStreamWriter) {
				t.Fatalf("Run = %v, want the stream writer's own failure", err)
			}
			for _, event := range tc.stream.events {
				if event.Terminal() {
					t.Errorf("stream holds the terminal event %s, which the writer never took: %v", event.Name, tc.stream.names())
				}
			}
		})
	}
}

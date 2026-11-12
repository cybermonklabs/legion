package going_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The read tools are offered beside the search exactly as the caller supplies them, a read's
// answer goes back to the model as the JSON of its tables, and an item a read named is one the
// answer may link: its link survives the filter as an item frame, as a search hit's does.
func TestAReadToolsItemsAreCitableAsSearchHitsAre(t *testing.T) {
	readHit := going.Hit{Kind: "issue", ID: "DEMO-40", Title: "Ship the read", Href: "/issues/DEMO-40"}
	model := goingtest.NewModel(t,
		goingtest.Reply{ToolCalls: []goingtest.ToolCall{{ID: "call_1", Name: going.ReadProgress, Arguments: `{"project":"DEMO"}`}}},
		answer("Open: [Ship the read](/issues/DEMO-40)."),
	)
	answerer := testAnswerer(t, model, going.AnswererConfig{})
	stream := &answerStream{}
	request := testReads().request("what is open in DEMO", stream)
	var asked []string
	request.Reads = map[string]going.ReadFunc{going.ReadProgress: func(_ context.Context, arguments json.RawMessage) (going.Read, error) {
		asked = append(asked, string(arguments))
		return going.Read{
			Tables: map[string][]map[string]any{"statuses": {{"status": "todo", "count": 3}}},
			Items:  []going.Hit{readHit},
		}, nil
	}}
	if err := answerer.Run(t.Context(), request, stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stream.ended(t)

	requests := model.Requests()
	var offered []string
	for _, tool := range requests[0].Tools {
		offered = append(offered, tool.Name)
	}
	slices.Sort(offered)
	if strings.Join(offered, " ") != going.ReadProgress+" "+contracts.GoingSearchTool {
		t.Errorf("the call offered %v, want the search and the one read the caller supplied", offered)
	}
	if len(asked) != 1 || asked[0] != `{"project":"DEMO"}` {
		t.Errorf("the read was asked %q, want the model's arguments once", asked)
	}
	var result string
	for _, message := range requests[1].Messages {
		if message.Role == "tool" {
			result = message.Content
		}
	}
	var told going.Read
	if err := json.Unmarshal([]byte(result), &told); err != nil || told.Tables["statuses"][0]["count"] != float64(3) {
		t.Errorf("the model was told %q (%v), want the read's tables as JSON", result, err)
	}
	item, ok := stream.first(t, contracts.GoingEventItem).Data.(going.ItemData)
	if !ok || item.Href != readHit.Href {
		t.Errorf("item = %+v, want the issue the read named", item)
	}
	if stream.count(contracts.GoingEventChanged) != 0 {
		t.Errorf("stream holds a changed frame %v: a link to an item a read named is no change", stream.names())
	}
}

// A read the server refuses goes back to the model, which answers on; a read the store could not
// serve ends the run with the store's code, as a failed search does.
func TestARefusedReadGoesBackToTheModelAndAFailedOneEndsTheRun(t *testing.T) {
	call := goingtest.Reply{ToolCalls: []goingtest.ToolCall{{ID: "call_1", Name: going.ReadArchitecture, Arguments: `{"project":"OTHER"}`}}}

	refusing := goingtest.NewModel(t, call, answer("Dispatch holds no answer."))
	stream := &answerStream{}
	request := testReads().request("components of OTHER", stream)
	request.Reads = map[string]going.ReadFunc{going.ReadArchitecture: func(context.Context, json.RawMessage) (going.Read, error) {
		return going.Read{}, going.Refuse("this question was asked of project DEMO")
	}}
	if err := testAnswerer(t, refusing, going.AnswererConfig{}).Run(t.Context(), request, stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stream.ended(t)
	if refusing.Calls() != 2 {
		t.Errorf("the model was called %d times, want twice: a refusal is the model's to answer", refusing.Calls())
	}

	failing := goingtest.NewModel(t, call, answer("An answer nobody should be written."))
	stream = &answerStream{}
	request = testReads().request("components of DEMO", stream)
	request.Reads = map[string]going.ReadFunc{going.ReadArchitecture: func(context.Context, json.RawMessage) (going.Read, error) {
		return going.Read{}, errors.New("the store went away")
	}}
	if err := testAnswerer(t, failing, going.AnswererConfig{}).Run(t.Context(), request, stream.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stream.code(t); got != contracts.GoingErrorAnswerFailed {
		t.Errorf("error code = %s, want %s", got, contracts.GoingErrorAnswerFailed)
	}
	if failing.Calls() != 1 {
		t.Errorf("the model was called %d times, want once: no call follows a failed read", failing.Calls())
	}
}

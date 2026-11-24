//go:build e2e

// The whole chain of a step record's models, driven through the real shim and the real daemon on
// both harnesses, and the reads of a stand-in's own stream that the chain counts by.
//
// What a reader of a stand-in's stream must hold: one response is one id. A step's record counts
// one call for each response its agent made, so two responses that carry one id count as one
// call, whatever the harness did. TestTheStandInGivesEachResponseItsOwnID holds the stand-in
// helpers of live_agent_proof_test.go to that, over the recorder alone: no pane, no daemon and no
// harness, so a reader of this file sees what the counting rests on before it reads the chain.
package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// standinEvent is one event of a stand-in's Messages stream, as a harness reads it off the wire:
// the `event:` name and the JSON of the `data:` line under it.
type standinEvent struct {
	name string
	data map[string]any
}

// standinResponse is what one response of a stand-in says, read back from its own stream: the id
// of its `message_start`, the model that message names, and the index of each content block it
// opened, in the order it opened them.
type standinResponse struct {
	id     string
	model  string
	blocks []int
}

// readStandinEvents reads the events of one written response. The stand-ins write `event: <name>`
// and `data: <json>` with a blank line between events (streamAnthropicBlocks), which is the shape
// the Anthropic Messages stream has.
func readStandinEvents(t *testing.T, stream string) []standinEvent {
	t.Helper()
	var events []standinEvent
	var name string
	for _, line := range strings.Split(stream, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var data map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
				t.Fatalf("the stand-in wrote a data line of event %q that is no JSON object: %v\n%s", name, err, line)
			}
			events = append(events, standinEvent{name: name, data: data})
		}
	}
	if len(events) == 0 {
		t.Fatalf("the stand-in wrote no event at all:\n%s", stream)
	}
	return events
}

// readStandinResponse is the one response a stand-in wrote, read back from its stream. It fails
// the test where the stream holds no `message_start`, since every later read of it would be of
// nothing.
func readStandinResponse(t *testing.T, stream string) standinResponse {
	t.Helper()
	response := standinResponse{}
	started := false
	for _, event := range readStandinEvents(t, stream) {
		switch event.name {
		case "message_start":
			started = true
			message, _ := event.data["message"].(map[string]any)
			response.id, _ = message["id"].(string)
			response.model, _ = message["model"].(string)
		case "content_block_start":
			index, _ := event.data["index"].(float64)
			response.blocks = append(response.blocks, int(index))
		}
	}
	if !started {
		t.Fatalf("the stand-in wrote no message_start, so the response has no id:\n%s", stream)
	}
	return response
}

// TestTheStandInGivesEachResponseItsOwnID holds the stand-ins of the live proofs to one id per
// response. A record counts one call for each response of the agent's own conversation, and a
// reader tells one response from the next by the id its `message_start` carries, so two responses
// under one id would be counted as one call and a turn of two answers would be recorded as one.
func TestTheStandInGivesEachResponseItsOwnID(t *testing.T) {
	t.Parallel()

	t.Run("two responses have two ids", func(t *testing.T) {
		first, second := httptest.NewRecorder(), httptest.NewRecorder()
		streamAnthropicText(first, "omp", "the first answer")
		streamAnthropicText(second, "omp", "the second answer")

		one := readStandinResponse(t, first.Body.String())
		two := readStandinResponse(t, second.Body.String())
		if one.id == "" || two.id == "" {
			t.Fatalf("a response carries no id: %q and %q", one.id, two.id)
		}
		if one.id == two.id {
			t.Fatalf("two responses of the stand-in carry the one id %q, so a reader that counts "+
				"one call for each id counts one call for two responses", one.id)
		}
	})
}

package omprequest

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The stand-in answers a queued tool call to the conversation's next turn and to nothing else:
// Oh My Pi's skill-description compression requests (no system prompt) and the plugin's `<btw>`
// side question get a text turn, so a call a test queues is the one the agent runs. It records
// every POST body, and the turns apart, the first of which is a captured request's shape (Read).
func TestTheStandinAnswersAQueuedCallToTheNextTurnAlone(t *testing.T) {
	s := NewStandin(t)
	s.CallTool(ToolCall{Name: "read", Input: map[string]any{"path": "skill://legion-worker/references/plan-tests.md"}})
	turn, err := os.ReadFile(filepath.Join("testdata", "requests", "1-changed-skill.json"))
	if err != nil {
		t.Fatal(err)
	}
	compression := `{"model":"standin-model","messages":[{"role":"user","content":"Compress into one routing hint of at most 12 words"}]}`
	side := `{"model":"standin-model","system":[{"type":"text","text":"A system prompt."}],"tools":[{"name":"read"}],` +
		`"messages":[{"role":"user","content":"<btw>Is the task done?</btw>"}]}`

	post := func(path, body string) string {
		t.Helper()
		resp, err := http.Post(s.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		answer, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(answer)
	}
	// block is the type of the content block a streamed answer opens.
	block := func(answer string) string {
		for _, line := range strings.Split(answer, "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var event struct {
				Type  string `json:"type"`
				Block struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if json.Unmarshal([]byte(data), &event) == nil && event.Type == "content_block_start" {
				return event.Block.Type + " " + event.Block.Name
			}
		}
		return "none in " + answer
	}

	for _, step := range []struct{ what, path, body, want string }{
		{"a compression request", "/v1/messages", compression, "text "},
		{"the side question", "/v1/messages", side, "text "},
		{"the first turn", "/v1/messages", string(turn), "tool_use read"},
		{"the next turn", "/v1/messages", string(turn), "text "},
	} {
		if got := block(post(step.path, step.body)); got != step.want {
			t.Errorf("%s was answered with %q, want %q", step.what, got, step.want)
		}
	}
	if got := post("/v1/messages/count_tokens", string(turn)); !strings.Contains(got, "input_tokens") {
		t.Errorf("a token count was answered %q, want a count", got)
	}
	if len(s.Bodies()) != 5 || len(s.Turns()) != 2 {
		t.Errorf("the stand-in recorded %d bodies and %d turns, want every POST (5) and the two turns", len(s.Bodies()), len(s.Turns()))
	}
	if turns := s.Turns(); len(turns) > 0 {
		if request, err := Read(turns[0]); err != nil || len(request.Skills) == 0 {
			t.Errorf("Read(the first turn recorded) = %d skills, %v; want the request read", len(request.Skills), err)
		}
	}
}

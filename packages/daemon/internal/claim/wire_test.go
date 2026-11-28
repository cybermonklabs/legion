package claim

import (
	"testing"
	"time"
)

// ModelRequestProblem is the one check of one model request that both the daemon's route (api's
// modelRequestsProblem) and the Claude Code bridge (claudebridge's assistant) run: a field
// strings.TrimSpace empties, spaces and U+0085 among them, a zero end and negative uncached input
// tokens are each refused with the field named, and a whole request is not.
func TestModelRequestProblemNamesTheFieldItRefuses(t *testing.T) {
	ended := time.Date(2026, time.November, 26, 10, 5, 9, 0, time.UTC)
	valid := ModelRequest{ID: "req-1", Session: "ses_1", Model: "claude-opus-5-5", EndedAt: ended, UncachedInputTokens: new(int64(0))}
	for _, row := range []struct {
		name   string
		change func(*ModelRequest)
		want   string
	}{
		{"a whole request", func(*ModelRequest) {}, ""},
		{"no uncached input tokens", func(r *ModelRequest) { r.UncachedInputTokens = nil }, ""},
		{"an id of spaces", func(r *ModelRequest) { r.ID = "   " }, "id is required"},
		{"no session", func(r *ModelRequest) { r.Session = "" }, "session is required"},
		{"a model of spaces", func(r *ModelRequest) { r.Model = "  " }, "model is required"},
		{"a model of U+0085", func(r *ModelRequest) { r.Model = "\u0085" }, "model is required"},
		{"no end", func(r *ModelRequest) { r.EndedAt = time.Time{} }, "endedAt is required"},
		{"negative uncached input tokens", func(r *ModelRequest) { r.UncachedInputTokens = new(int64(-1)) }, "uncachedInputTokens is -1; it counts tokens from 0"},
	} {
		t.Run(row.name, func(t *testing.T) {
			request := valid
			row.change(&request)
			if got := ModelRequestProblem(request); got != row.want {
				t.Fatalf("ModelRequestProblem(%+v) = %q, want %q", request, got, row.want)
			}
		})
	}
}

package going_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
)

// tokenFileAnswerer is an answerer whose endpoint presents the bearer the file at path holds, as
// the endpoint place's deployment configures it.
func tokenFileAnswerer(t *testing.T, model *goingtest.Model, path string) *going.Answerer {
	t.Helper()
	endpoint := testModel(model)
	endpoint.Endpoint.Token = going.FileTokenSource{Path: path}.Token
	return testAnswerer(t, model, going.AnswererConfig{Model: endpoint})
}

// TestTheTokenFileIsReadAgainForEveryCall proves going.FileTokenSource takes the bearer the file
// holds now: the file is rewritten between two runs, and the second run's call presents the new
// token.
func TestTheTokenFileIsReadAgainForEveryCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat-token")
	if err := os.WriteFile(path, []byte("first-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	model := goingtest.NewModel(t, answer("Dispatch holds no answer to that question."))
	answerer := tokenFileAnswerer(t, model, path)
	for i, token := range []string{"first-token", "second-token"} {
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			t.Fatalf("write token file: %v", err)
		}
		stream := &answerStream{}
		if err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order", stream), stream.emit); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		stream.ended(t)
	}
	requests := model.Requests()
	if len(requests) != 2 {
		t.Fatalf("model served %d calls, want 2", len(requests))
	}
	if requests[0].Authorization != "Bearer first-token" || requests[1].Authorization != "Bearer second-token" {
		t.Errorf("Authorization = %q then %q, want Bearer first-token then Bearer second-token",
			requests[0].Authorization, requests[1].Authorization)
	}
}

// TestAnUnusableTokenFileMakesNoCall: a file that cannot be read and one that holds nothing are
// both failures naming the path, and neither makes a call: no unauthenticated request goes out
// in its place, and the run ends with the failure code.
func TestAnUnusableTokenFileMakesNoCall(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty-token")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	for _, tc := range []struct {
		name string
		path string
	}{
		{"a missing file", filepath.Join(dir, "no-such-token")},
		{"an empty file", empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := (going.FileTokenSource{Path: tc.path}).Token(t.Context()); err == nil || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("Token error = %v, want it to name %s", err, tc.path)
			}
			model := goingtest.NewModel(t, answer("an answer"))
			stream := &answerStream{}
			if err := tokenFileAnswerer(t, model, tc.path).Run(t.Context(), testReads().request("who owns it", stream), stream.emit); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := stream.code(t); got != contracts.GoingErrorAnswerFailed {
				t.Errorf("error code = %s, want %s", got, contracts.GoingErrorAnswerFailed)
			}
			if model.Calls() != 0 {
				t.Errorf("model served %d calls, want none without a bearer", model.Calls())
			}
		})
	}
}

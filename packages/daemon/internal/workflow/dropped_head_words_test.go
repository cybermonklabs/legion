package workflow

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// TestAMergeStateReadForAnotherHeadLogsThatHeadOnlyAsACommit: GitHub, or whatever answers in its
// place, answers the merge state of #42 for a head the record does not stand at, its head.sha
// words that quote a URL with made-up credentials. The engine drops the read, and its line names
// the head GitHub answered for only in a commit's shape: the fixed phrase here. It printed the
// words as they came. The control is a head in a commit's shape, which the line names as it came.
func TestAMergeStateReadForAnotherHeadLogsThatHeadOnlyAsACommit(t *testing.T) {
	for _, tc := range []struct {
		name, head, want string
	}{
		{"words in the head", "head " + oauth2calltest.Words, githubrest.NotACommit},
		{"control: a commit", headTwo, headTwo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
			var logged bytes.Buffer
			engine := readsAfter(0)
			engine.log = slog.New(slog.NewJSONHandler(&logged, nil))
			applyRefusingNothing(t, pool, engine, intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42,
				Base: "main", HeadSHA: tc.head, Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
			var line map[string]any
			for _, raw := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
				var entry map[string]any
				if json.Unmarshal([]byte(raw), &entry) == nil && strings.Contains(entry["msg"].(string), "has moved past; the read is dropped") {
					line = entry
				}
			}
			if line == nil {
				t.Fatalf("the engine logged %s, want the line of a read for a head the record has moved past", logged.String())
			}
			if line["answered"] != tc.want {
				t.Errorf("the line names the answered head %q, want %q", line["answered"], tc.want)
			}
			oauth2calltest.RequireNone(t, "workflow.answer-words/dropped-head", logged.String())
		})
	}
}

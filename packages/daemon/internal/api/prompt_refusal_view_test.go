package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim whose launch was refused because the role's system prompt is larger than its model's
// context is read in the claim view read_state serves, with no fresh_session asked for: the
// refusal's sentence, which names the role, the prompt's size and the bound, is the view's
// `lastStartFailure` and its `failure.detail`, under the cause `launch_refused`, with no launch
// failure among its `attempts`. The claim is the one supervise leaves (its promptRefused, held by
// TestAPromptTooLargeForItsModelFailsTheClaimChargingNoLaunchFailure); the sentence is the runtime's
// own, over a real 200,000-byte prompt and a 150,000-byte bound.
func TestTheClaimViewOfAPromptRefusalNamesTheRoleTheSizeAndTheBound(t *testing.T) {
	role := filepath.Join(t.TempDir(), "architect.md")
	if err := os.WriteFile(role, []byte(strings.Repeat("x", 200_000)), 0o600); err != nil {
		t.Fatal(err)
	}
	token := claim.Token("legion-legion-legion-208-architect")
	_, refusal := runtime.SystemPrompt(runtime.SpawnSpec{
		Claim: token, Role: claim.RoleArchitect, ModelContextBytes: 150_000,
		Prompt: runtime.PromptParts{RolePromptPaths: []string{role}},
	})
	if refusal == nil {
		t.Fatal("runtime.SystemPrompt composed a 200,000-byte prompt under a 150,000-byte bound")
	}
	started := fmt.Sprintf("sandbox launch %s: %v", token, refusal)
	refused := supervise.Claim{
		Token: token, Role: claim.RoleArchitect, State: supervise.StateFailed,
		LastStartFailure: started,
		FailureCause:     supervise.CauseLaunchRefused,
		Failure:          "launch refused, charged no launch failure: " + started,
		FailedAt:         time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC),
	}

	encoded, err := json.Marshal(ClaimViewOf(refused))
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		LastStartFailure string `json:"lastStartFailure"`
		Failure          struct {
			Cause    string         `json:"cause"`
			Detail   string         `json:"detail"`
			Attempts map[string]int `json:"attempts"`
		} `json:"failure"`
	}
	if err := json.Unmarshal(encoded, &view); err != nil {
		t.Fatal(err)
	}
	for _, named := range []string{"the architect's system prompt", "200000 bytes", "150000 bytes", "model_context_bytes.architect"} {
		if !strings.Contains(view.LastStartFailure, named) || !strings.Contains(view.Failure.Detail, named) {
			t.Errorf("the claim view %s does not name %q in both lastStartFailure and failure.detail", encoded, named)
		}
	}
	if view.Failure.Cause != string(supervise.CauseLaunchRefused) || view.Failure.Attempts["launchFailures"] != 0 {
		t.Errorf("the claim view %s fails under %q with attempts %v, want launch_refused and no launch failure", encoded, view.Failure.Cause, view.Failure.Attempts)
	}
}

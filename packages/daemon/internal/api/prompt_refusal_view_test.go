package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim whose launch was refused because the start frame carrying its role's system prompt to
// the pod's launcher would pass the one frame a launcher reads is read in the claim view read_state
// serves, with no fresh_session asked for: the refusal's sentence, which names the role, the
// prompt's size and the limit, is the view's `lastStartFailure` and its `failure.detail`, under the
// cause `launch_refused`, with no launch failure among its `attempts`. The claim is the one
// supervise leaves (its promptRefused, held by
// TestAPromptPastTheStartFrameFailsTheClaimChargingNoLaunchFailure).
func TestTheClaimViewOfAPromptRefusalNamesTheRoleTheSizeAndTheLimit(t *testing.T) {
	token := claim.Token("legion-legion-legion-208-architect")
	refusal := &runtime.PromptTooLarge{Role: claim.RoleArchitect, Bytes: 1_100_000, Frame: 1_100_400, Bound: shimwire.MaxFrameBytes}
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
	for _, named := range []string{"the architect's system prompt", "1100000 bytes", "1048576 bytes"} {
		if !strings.Contains(view.LastStartFailure, named) || !strings.Contains(view.Failure.Detail, named) {
			t.Errorf("the claim view %s does not name %q in both lastStartFailure and failure.detail", encoded, named)
		}
	}
	if view.Failure.Cause != string(supervise.CauseLaunchRefused) || view.Failure.Attempts["launchFailures"] != 0 {
		t.Errorf("the claim view %s fails under %q with attempts %v, want launch_refused and no launch failure", encoded, view.Failure.Cause, view.Failure.Attempts)
	}
}

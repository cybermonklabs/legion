package texts

import (
	"strings"
	"testing"
	"time"
)

// dispatch://INFRA-4203's R2 (R8 of dispatch://INFRA-3686 keeps it whole, the architect's ruling):
// "The steps also say four things. The round does not end early while a role reports background
// work. A named service that the role started is not in the report, holds nothing and goes with
// the pod. At the round's end, the pod goes, whatever runs. If background work ends after the
// answer, the role pushes what it made and calls `ready_for_update` again with the same round,
// before it ends that turn."
//
// The ask gives the first as every_answer_brings_it_forward, the third and the fourth as
// goes_with_pod and late_background_work. Of the second it gives only that a named service goes
// with the pod (goes_with_pod's named_services); no field says that a named service is in no
// report and holds nothing, which is what keeps a role from waiting on its own named service
// before it answers. Here a field of the ask other than goes_with_pod names the named services.
func TestTheUpdateAskSaysANamedServiceHoldsNothing(t *testing.T) {
	ask := NewUpdateAsk("LEGION-209", "round-2187820f-74bb-a446-9f66-3a37df0bfd29", "pod-b4442a92",
		[]string{"4c1b8e2a6f9d"}, []string{"account"}, time.Date(2026, 11, 13, 12, 30, 0, 0, time.UTC))
	text, err := Render(ask)
	if err != nil {
		t.Fatalf("render the update ask: %v", err)
	}
	for _, line := range strings.Split(text, "\n") {
		field := strings.TrimSpace(line)
		if strings.HasPrefix(field, "goes_with_pod") {
			continue
		}
		if strings.Contains(field, "named_service") || strings.Contains(field, "named service") {
			return
		}
	}
	t.Errorf("the update ask says of a named service only that it goes with the pod; no field says it is in no report and holds nothing (INFRA-4203 R2's second thing); its text:\n%s", text)
}

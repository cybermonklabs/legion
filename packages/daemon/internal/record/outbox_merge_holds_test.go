package record

import (
	"strings"
	"testing"
)

// A change of the merge holds reaches the architect as a notice's reason and the merger as a
// message, and both read one text: it names the project's document, tells the role to read it
// again, and tells a held role to wait again and any other role to go on. The merger's message
// starts with its issue key, as a merge-queue removal's message does. Neither names READY, which no
// merger prompt may name (prompts' TestNoComposedPromptCommitsAHandoff).
func TestAMergeHoldsChangeTellsTheRoleToReadTheDocumentAgain(t *testing.T) {
	reason := MergeHoldsChangedReason("LEGION")
	message := MergeHoldsChanged{Project: "LEGION"}.Message("LEGION-208")
	if message != "LEGION-208: "+reason {
		t.Fatalf("the merger's message = %q, want the issue key and then the notice's reason %q", message, reason)
	}
	for _, want := range []string{"dispatch://LEGION/artifact/merge-holds-md", "Read that document again.", "WAITING", "go on"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the reason %q does not say %q", reason, want)
		}
	}
	if strings.Contains(reason, "READY") {
		t.Errorf("the reason %q names READY", reason)
	}
}

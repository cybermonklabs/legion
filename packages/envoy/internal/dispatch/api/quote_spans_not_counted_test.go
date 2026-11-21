package api

import (
	"net/http"
	"strings"
	"testing"
)

// The edit route counts a quote's matches inside one block alone (dispatch://INFRA-4345, R1), on
// the shapes of dispatch://INFRA-4015/artifact/plan-md its report names: at version 50 `v46). The`
// is inside a paragraph once and runs across the join of two list items once, and the replace
// applies to the paragraph, where it was 409 TARGET_AMBIGUOUS, "matches 2 places"; at version 51
// its only match is that join, and it and `v46). The errors` are 400 TARGET_SPANS_BLOCKS as they
// were; and `(the gate on v46).`, inside three blocks of each, is 409 TARGET_AMBIGUOUS naming three.
func TestTheEditRouteCountsNoQuoteMatchThatSpansBlocks(t *testing.T) {
	const lists = "- The first check passes (the gate on v46).\n- The errors are read last.\n"
	const version50 = "Release plan for the gate.\n\n" +
		"The release waits on its check (the gate on v46). The workflows run after it.\n\n" +
		lists + "\nNothing ships before (the gate on v46).\n"
	const version51 = "Release plan for the gate.\n\n" +
		"The release waits on its check (the gate on v46). After it, the workflows run.\n\n" +
		lists + "\nNothing ships before (the gate on v46).\n"
	handler := newTestHandler(t)
	at50 := createInteractionIssue(t, handler, "PLANA", "Version 50", version50)
	at51 := createInteractionIssue(t, handler, "PLANB", "Version 51", version51)
	replace := func(find string) []map[string]any {
		return []map[string]any{{"op": "replace", "find": find, "with": find}}
	}
	for _, test := range []struct {
		name, artifact string
		ops            []map[string]any
		status         int
		code, want     string
	}{
		{"the control at version 50", at50.PrimaryArtifactID, replace("(the gate on v46)."), http.StatusConflict, "TARGET_AMBIGUOUS", `quote "(the gate on v46)." matches 3 places`},
		{"the control at version 51", at51.PrimaryArtifactID, replace("(the gate on v46)."), http.StatusConflict, "TARGET_AMBIGUOUS", `quote "(the gate on v46)." matches 3 places`},
		{"a quote whose one match spans the list at version 51", at51.PrimaryArtifactID, replace("v46). The"), http.StatusBadRequest, "TARGET_SPANS_BLOCKS", `quote "v46). The" spans more than one block`},
		{"a longer quote that spans the list at version 51", at51.PrimaryArtifactID, replace("v46). The errors"), http.StatusBadRequest, "TARGET_SPANS_BLOCKS", `quote "v46). The errors" spans more than one block`},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, code, refusal := postEdits(t, handler, test.artifact, test.ops)
			if status != test.status || code != test.code || !strings.Contains(refusal, test.want) {
				t.Fatalf("status=%d code=%q error=%q, want %d %s saying %q", status, code, refusal, test.status, test.code, test.want)
			}
		})
	}
	if status, code, refusal := postEdits(t, handler, at50.PrimaryArtifactID, []map[string]any{
		{"op": "replace", "find": "v46). The", "with": "v46). Then the"},
	}); status != http.StatusOK {
		t.Fatalf("replace of v46). The at version 50: status=%d code=%q error=%q, want 200", status, code, refusal)
	}
	want := "Release plan for the gate.\n\n" +
		"The release waits on its check (the gate on v46). Then the workflows run after it.\n\n" +
		lists + "\nNothing ships before (the gate on v46).\n"
	if after := documentMarkdown(t, handler, at50.PrimaryArtifactID); after != want {
		t.Fatalf("version 50 after the replace = %q, want %q", after, want)
	}
	if after := documentMarkdown(t, handler, at51.PrimaryArtifactID); after != version51 {
		t.Fatalf("version 51 after its refused edits = %q, want it unchanged", after)
	}
}

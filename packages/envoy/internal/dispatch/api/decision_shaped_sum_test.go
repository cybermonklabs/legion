package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A decision's words are its own text without its `shaped` attribute, which is a relation out of
// the decision and no word of it (R8 and R32 of dispatch://INFRA-1253's spec), and every sum of
// them goes through refs.DecisionWords. So a write of `shaped` alone stales no Keep: alice keeps
// d1 at the text_sha256 the Decisions read answers, and a `retype` that writes d1's `shaped` and
// keeps its words leaves that read's text_sha256, its changed_version and its words as they were,
// d1 kept by alice and waiting for no review, and the Keep route takes the same sum again. The
// retype is held to have written the attribute: the document's text holds it, and d1 links r1.
func TestAShapedWriteKeepsTheDecisionsSum(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::requirement{#r1}\nThe issue page lists every open ask.\n:::\n\n"+
		":::decision{#d1}\nWe order the list by urgency.\n:::\n")
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	d1 := refs.BlockNodeKind + ":" + spec.address("d1")

	before := requireEntry(t, readEntries(t, handler, issue.Key), "d1 before its Keep", d1)
	if strings.Contains(before.Text, "shaped=") {
		t.Fatalf("d1 reads the words %q before any shaped is written", before.Text)
	}
	keepBlock(t, handler, spec.id, "d1", before.TextSHA256)

	pointer := "dispatch://" + issue.Key + "/spec#b-r1"
	editSpec(t, handler, issue.Key, []map[string]any{{
		"op": "retype", "block": "d1", "type": "decision", "attributes": map[string]any{"shaped": []string{pointer}},
	}})
	if text := documentMarkdown(t, handler, spec.id); !strings.Contains(text, `shaped="`+pointer+`"`) {
		t.Fatalf("the retype left the spec without d1's shaped, so this case proves nothing:\n%s", text)
	}
	after := requireEntry(t, readEntries(t, handler, issue.Key), "d1 after its shaped is written", d1)
	linked := false
	for _, link := range after.Shaped {
		linked = linked || link.To.ID == spec.pointed("r1")
	}
	if !linked {
		t.Fatalf("d1 links %s after the retype, want r1 among them, so this case proves nothing", entryJSON(t, after.Shaped))
	}
	switch {
	case after.TextSHA256 != before.TextSHA256 || after.Text != before.Text:
		t.Errorf("writing d1's shaped moved its text_sha256 from %s to %s (its words %q, then %q); kept %s, waiting_review %t",
			before.TextSHA256, after.TextSHA256, before.Text, after.Text, entryJSON(t, after.Kept), after.WaitingReview)
	case after.Kept == nil || after.Kept.KeptBy != "alice" || after.WaitingReview:
		t.Errorf("after d1's shaped was written its Keep reads %s and waiting_review %t, want alice's Keep standing", entryJSON(t, after.Kept), after.WaitingReview)
	case after.ChangedVersion != before.ChangedVersion:
		t.Errorf("writing d1's shaped moved its changed_version from %d to %d, which names the version its words last changed in",
			before.ChangedVersion, after.ChangedVersion)
	}
	if kept := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+spec.id+"/blocks/d1/keep",
		map[string]any{"text_sha256": before.TextSHA256}, "alice"); kept.Code != http.StatusOK {
		t.Errorf("the Keep route refused d1's sum from before its shaped was written: status=%d body=%s", kept.Code, kept.Body.String())
	}
}

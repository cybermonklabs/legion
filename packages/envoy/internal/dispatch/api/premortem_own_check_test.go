package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestAConversionMapNamingACheckOfTheDocumentItRetiresWritesNothing converts a pre-mortem document
// that holds a check block of its own, with a map that names that check for risk 0. The conversion
// retires the document, and a retired document's checks are run by nobody, so a prevented_by to
// one is refused (spec requirement r-constraint-risk) and a retirement retracts each
// prevented_by into its blocks. The conversion must therefore refuse that entry, in a dry run as in
// a real one, with 400 INVALID_POINTER naming the check, and write nothing: a conversion that
// answers risk 0 prevented by the check while its own retirement retracts that tuple tells its
// caller of a pointer the store does not hold.
func TestAConversionMapNamingACheckOfTheDocumentItRetiresWritesNothing(t *testing.T) {
	rig := newPremortemRig(t)
	held := uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "premortem-with-a-check.md",
		rig.capture.Document.Markdown+"\n## The check this pre-mortem asks for\n\n:::check{#pm9}\nA check the pre-mortem document holds.\n:::\n")
	own := refs.BlockRef("dispatch://"+rig.issue+"/artifact/"+held.Artifact.Slug, "pm9")
	entries := rig.premortemMap()
	entries[0].Checks, entries[0].Reason = []string{own}, "The check the pre-mortem document holds."
	path := "/api/v1/issues/" + rig.issue + "/artifacts/" + held.Artifact.Slug + "/premortem-convert"
	before := rig.storeCounts()

	for _, dryRun := range []bool{true, false} {
		response := dispatchRequest(t, rig.handler, http.MethodPost, path,
			map[string]any{"prevented_by": entries, "dry_run": dryRun}, "alice")
		t.Logf("dry_run=%t: status=%d body=%s", dryRun, response.Code, response.Body.String())
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_POINTER") ||
			!strings.Contains(response.Body.String(), own) {
			t.Errorf("a conversion with dry_run=%t whose map names %s, a check of the document it retires, answers %d %s; "+
				"want 400 INVALID_POINTER naming the check", dryRun, own, response.Code, response.Body.String())
		}
		if after := rig.storeCounts(); after != before {
			t.Errorf("a conversion with dry_run=%t whose map names %s left %v nodes and tuples, want the %v it found",
				dryRun, own, after, before)
		}
	}
}

package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The read route writes the lines that `dispatch spec-form` prints of a document's findings
// (finding_lines), so the command holds no copy of their words. Each finding's line is the line
// that the refusal of an approval request writes for that finding, from the one builder of both
// (specform). For a version that the gate refuses, a head that counts the texts that break the
// form comes first, as the refusal's head counts them. A document out of scope gets the lines of
// its findings and no head, since nothing refuses its approval.
func TestTheReadRouteWritesEachFindingInTheLineOfTheRefusal(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	spec := "# Spec\n\n" +
		formRequirement("store-comment", "Dispatch stores each comment. Dispatch indexes each comment. Dispatch shows each comment.") +
		formRequirement("keep-record", "Dispatch keeps each record. Dispatch reads each record. Dispatch shows each record.")
	covered := createFormIssue(t, tree.handler, tree.root, "Two broken statements", spec)
	outside := createFormIssue(t, tree.handler, "", "Two broken statements outside the root", spec)

	refused := requestFormApproval(t, tree.handler, covered.PrimaryArtifactID, nil)
	if refused.Code != http.StatusConflict {
		t.Fatalf("the request of the covered spec: status=%d body=%s, want 409", refused.Code, refused.Body.String())
	}
	words, _ := decodeBody[map[string]any](t, refused)["refusal"].(string)
	findingLines := strings.Split(words, "\n")[1:]
	if len(findingLines) != 2 {
		t.Fatalf("the refusal %q writes %d finding lines, want 2", words, len(findingLines))
	}

	for _, row := range []struct {
		name, artifactID string
		want             []string
	}{
		{name: "a covered document", artifactID: covered.PrimaryArtifactID, want: append([]string{
			"Dispatch refuses an approval request of spec.md version 1, because 2 texts break the form of The Spec.",
		}, findingLines...)},
		{name: "a document out of scope", artifactID: outside.PrimaryArtifactID, want: findingLines},
	} {
		read := dispatchRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+row.artifactID+"/spec-form", nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("%s: the read route answered %d: %s", row.name, read.Code, read.Body.String())
		}
		var lines []string
		listed, _ := decodeBody[map[string]any](t, read)["finding_lines"].([]any)
		for _, line := range listed {
			text, _ := line.(string)
			lines = append(lines, text)
		}
		if !slices.Equal(lines, row.want) {
			t.Errorf("%s: finding_lines = %q, want %q", row.name, lines, row.want)
		}
	}
}

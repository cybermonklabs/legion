package workflow

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The heading "" in Dispatch's change set (GET /api/v1/artifacts/{id}/changes) is the text outside
// every `##` section: the title, the text above the first `##`, and the text under each later `#`
// heading up to the next `##`. Since round 3 of dispatch://INFRA-175 it is named moved when a `#`
// heading's run stands at another place among the sections, which can be anywhere in the document:
// a `# Appendix` heading moved from the top back to between two sections is one. So the news must
// not tell the worker that the text before the first section changed or moved, which sends it to
// read the top of the document for a change that stands lower down. The control is a named `##`
// section in the same list, which the news names by its heading.
func TestNewsNamesTheTextOutsideEverySectionWithoutPlacingItBeforeTheFirstSection(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"moved", "changed"} {
		t.Run(change, func(t *testing.T) {
			text := RenderNews(News{
				Audience: record.AudienceHolder, Tree: "LEGION-208", Issue: "LEGION-208", Phase: phase.Reviewing,
				Items: []NewsChange{{Step: record.DesignStepSpec, Changes: record.DesignChanges{
					VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: 3, To: 5}, State: record.DesignChangesRead,
					Sections: []record.SectionChange{{Heading: "", Occurrence: 0, Change: change}, {Heading: "Design", Occurrence: 0, Change: "changed"}},
				}}},
			})
			t.Logf("the news of the text outside every section %s:\n%s", change, text)
			if !strings.Contains(text, `"Design" changed`) {
				t.Fatalf("the news does not name the Design section (the control):\n%s", text)
			}
			if strings.Contains(text, "before the first section") {
				t.Fatalf("the news places the text outside every `##` section before the first section, where a `#` heading's run can stand anywhere:\n%s", text)
			}
		})
	}
}

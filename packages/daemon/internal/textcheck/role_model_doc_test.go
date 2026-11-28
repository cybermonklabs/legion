package textcheck

//ci:reads deploy/kubernetes/operator-route/README.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestEachGuideThatChangesAModelNamesHowOneRoleGetsItsOwn (each of roleModelGuides)
//ci:reads deploy/kubernetes/operator-route/overlay.yml `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestEachGuideThatChangesAModelNamesHowOneRoleGetsItsOwn (each of roleModelGuides)
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestEachGuideThatChangesAModelNamesHowOneRoleGetsItsOwn (each of roleModelGuides)
//ci:reads docs/site/src/content/docs/legion/running-legion.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestEachGuideThatChangesAModelNamesHowOneRoleGetsItsOwn (each of roleModelGuides)

import (
	"regexp"
	"strings"
	"testing"
)

// The lines under `modelRoles` in the operator's overlay are Oh My Pi's model roles (`default`,
// `review`, `slow`, …), not Legion's: every launch of every Legion role names `--model @default`,
// so `modelRoles.default` is every Legion role's model and no line there moves one role alone. The
// one way to give one Legion role a model of its own, the reviewer on another model family than
// the implementer, say, is an overlay that sets its own `modelRoles.default`, which that role's
// `role_config_files` names after the operator's overlays. A guide that tells an operator to change
// a model by a line under `modelRoles` and stops there leaves the reviewer and the implementer on
// one model whatever line they edit.
var roleModelGuides = []string{
	"deploy/kubernetes/operator-route/README.md",
	"deploy/kubernetes/operator-route/overlay.yml",
	"docs/kubernetes.md",
	"docs/site/src/content/docs/legion/running-legion.md",
}

// underModelRoles finds a sentence that sends the operator to a line under `modelRoles`, written
// with backticks in a guide and without them in the overlay's own comment.
var underModelRoles = regexp.MustCompile("under `?modelRoles`?")

// Each paragraph of the operator's guides that tells how to change a model by a line under
// `modelRoles` also says how one Legion role gets a model of its own: its `role_config_files`
// overlay setting `modelRoles.default`. Each guide holds at least one such paragraph, so a guide
// that drops or rewords its instruction fails here rather than passing on nothing found.
func TestEachGuideThatChangesAModelNamesHowOneRoleGetsItsOwn(t *testing.T) {
	for _, guide := range roleModelGuides {
		text := shippedText(t, guide)
		places := underModelRoles.FindAllStringIndex(text, -1)
		if len(places) == 0 {
			t.Errorf("%s holds no sentence that changes a model by a line under `modelRoles`: this check found nothing to read there", guide)
		}
		for _, place := range places {
			paragraph := onOneLine(paragraphAround(text, place[0]))
			for _, want := range []string{"role_config_files", "modelRoles.default"} {
				if !strings.Contains(paragraph, want) {
					t.Errorf("%s: the paragraph that changes a model by a line under `modelRoles` does not name %s, "+
						"so it gives no way to move one Legion role, the reviewer say, onto a model of its own:\n%s",
						guide, want, paragraph)
				}
			}
		}
	}
}

// paragraphAround is the text between the blank line before at and the blank line after it.
func paragraphAround(text string, at int) string {
	start := strings.LastIndex(text[:at], "\n\n")
	end := strings.Index(text[at:], "\n\n")
	if end < 0 {
		return text[start+1:]
	}
	return text[start+1 : at+end]
}

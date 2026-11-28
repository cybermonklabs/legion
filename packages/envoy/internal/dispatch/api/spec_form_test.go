package api

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
)

// The setting that names the roots of The Spec (DISPATCH_SPEC_ROOTS) holds issue keys separated by
// commas: each is trimmed, a key named twice counts once, and blank names no root. An item that is
// no issue key, an empty item among them, refuses startup naming the setting.
func TestParseSpecRoots(t *testing.T) {
	for raw, want := range map[string][]string{
		" ACME-1, ACME-7 ": {"ACME-1", "ACME-7"},
		"ACME-1,ACME-1":    {"ACME-1"},
		"":                 nil,
		"   ":              nil,
	} {
		got, err := parseSpecRoots(raw)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parseSpecRoots(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"acme-1", "ACME-1,,ACME-2", "ACME-1,", "ACME"} {
		if got, err := parseSpecRoots(raw); err == nil || !strings.Contains(err.Error(), "DISPATCH_SPEC_ROOTS") {
			t.Errorf("parseSpecRoots(%q) = %q, %v; want a refusal naming DISPATCH_SPEC_ROOTS", raw, got, err)
		}
	}
	// The server reads it as it reads the other settings: main builds the form of The Spec from it
	// (NewSpecForm), which refuses startup on a value that is no list of issue keys, and builds its
	// API with that form. NewDeps with no form builds one that names no root. Each asks for a
	// document service: one over no store serves, as this test opens no document.
	documents := docs.New(docs.Deps{})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	if _, err := NewSpecForm("ACME-1,,ACME-2", "", documents); err == nil || !strings.Contains(err.Error(), "DISPATCH_SPEC_ROOTS") {
		t.Fatalf("NewSpecForm with an empty item = %v, want a refusal naming DISPATCH_SPEC_ROOTS", err)
	}
	form, err := NewSpecForm(" ACME-1 , ACME-5", "https://dispatch.example/", documents)
	if err != nil || !reflect.DeepEqual(form, SpecForm{roots: []string{"ACME-1", "ACME-5"}, serverURL: "https://dispatch.example"}) {
		t.Fatalf("NewSpecForm(%q) = %+v, %v; want the roots [ACME-1 ACME-5] and the address with no trailing slash", " ACME-1 , ACME-5", form, err)
	}
	deps, err := NewDeps(DepsInput{SpecForm: &form, Docs: documents})
	if err != nil || !reflect.DeepEqual(deps.SpecForm, form) {
		t.Fatalf("NewDeps with the form = %+v, %v; want that form", deps.SpecForm, err)
	}
	if deps, err := NewDeps(DepsInput{Docs: documents}); err != nil || len(deps.SpecForm.roots) != 0 {
		t.Fatalf("NewDeps with no form = %+v, %v; want a form that names no root", deps.SpecForm, err)
	}
}

// specFormOn is the form of The Spec that specRoots names, with its gate set on documents, as main
// builds the server's (NewSpecForm).
func specFormOn(t testing.TB, specRoots, serverURL string, documents docs.API) *SpecForm {
	t.Helper()
	form, err := NewSpecForm(specRoots, serverURL, documents)
	if err != nil {
		t.Fatalf("build the form of The Spec from %q: %v", specRoots, err)
	}
	return &form
}

// specFormResultOf is the judgment of one spec, spec.md version 3 of ACME-2 under the root ACME-1,
// that holds findings.
func specFormResultOf(findings []specFormFinding) specFormResult {
	issue, root := "ACME-2", "ACME-1"
	return specFormResult{documents: []specFormDocument{{
		artifact:  model.Artifact{ID: "a1", IssueKey: &issue, Name: "spec.md"},
		version:   3,
		specScope: specScope{scope: specScopeCovered, root: &root},
		findings:  findings,
	}}}
}

// A refusal's body holds at most 500 findings and counts every one, and its error lists the first
// 20 and names the command, which lists them, the first 500 at most. The refusal is the one the
// judgment writes (specFormResult.refusal), so the command it names is the one the server names.
func TestTheRefusalBodyHoldsAtMost500Findings(t *testing.T) {
	findings := make([]specFormFinding, 501)
	for index := range findings {
		findings[index] = specFormFinding{
			Document: "spec.md", ArtifactID: "a1", Version: 3,
			Finding: specform.Finding{
				Node: fmt.Sprintf("s%d", index), Kind: specform.KindStatement, Where: "spec.md version 3",
				Rule: "sentences", Detail: "it has 3 sentences, and a statement has one or two",
			},
		}
	}
	refusal := specFormResultOf(findings).refusal(specform.ActRequest)
	if want := "dispatch spec-form --issue ACME-2 --artifact spec.md --version 3 lists the first 500 of them."; refusal.more != want {
		t.Fatalf("the refusal of 501 findings names %q, want %q", refusal.more, want)
	}
	body := refusal.body()
	if body.Code != "SPEC_FORM" || len(body.Findings) != 500 || body.FindingsTotal != 501 || body.ChangesReason != "" {
		t.Fatalf("body = code %q, %d findings, total %d, changes_reason %q; want SPEC_FORM, 500, 501 and none",
			body.Code, len(body.Findings), body.FindingsTotal, body.ChangesReason)
	}
	if body.Findings[499].Node != "s499" {
		t.Fatalf("the last finding the body holds is %q, want s499, the 500th", body.Findings[499].Node)
	}
	lines := strings.Split(body.Error, "\n")
	if len(lines) != 23 || lines[0] != "Dispatch refused this approval request, because 501 texts break the form of The Spec." ||
		lines[21] != "And 481 more: dispatch spec-form --issue ACME-2 --artifact spec.md --version 3 lists the first 500 of them." ||
		lines[22] != "Fix each text, then request approval again." {
		t.Fatalf("error =\n%s\nwant the head, 20 finding lines, the count of the other 481 and the next step", body.Error)
	}
	approval := specFormResultOf(findings[:1]).refusal(specform.ActApproval)
	if want := "dispatch spec-form --issue ACME-2 --artifact spec.md --version 3 lists them all."; approval.more != want {
		t.Fatalf("the refusal of 1 finding names %q, want %q", approval.more, want)
	}
	approvalBody := approval.body()
	if !strings.HasSuffix(approvalBody.Error, "\nTo send these findings to the author, choose Request changes.") ||
		approvalBody.ChangesReason != "Dispatch refused my approval, because 1 text breaks the form of The Spec.\n"+
			"- statement s0 in spec.md version 3: it has 3 sentences, and a statement has one or two.\n"+
			"Fix each text, then request approval again." {
		t.Fatalf("approval body = error %q, changes_reason %q", approvalBody.Error, approvalBody.ChangesReason)
	}
}

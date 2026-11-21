package api

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

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
	// The server reads it as it reads the other settings: NewDeps parses it, and refuses startup
	// on a value that is no list of issue keys.
	if _, err := NewDeps(DepsInput{SpecRoots: "ACME-1,,ACME-2"}); err == nil || !strings.Contains(err.Error(), "DISPATCH_SPEC_ROOTS") {
		t.Fatalf("NewDeps with an empty item = %v, want a refusal naming DISPATCH_SPEC_ROOTS", err)
	}
	deps, err := NewDeps(DepsInput{SpecRoots: " ACME-1 , ACME-5"})
	if err != nil || !reflect.DeepEqual(deps.SpecRoots, []string{"ACME-1", "ACME-5"}) {
		t.Fatalf("NewDeps(%q).SpecRoots = %q, %v; want [ACME-1 ACME-5]", " ACME-1 , ACME-5", deps.SpecRoots, err)
	}
}

// A refusal's body holds at most 500 findings and counts every one, and its error lists the first
// 20 and the command that lists the rest.
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
	refusal := &specFormRefusal{act: specform.ActRequest, findings: findings, more: "dispatch spec-form --issue ACME-2 --artifact spec.md lists them all."}
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
		lines[21] != "And 481 more: dispatch spec-form --issue ACME-2 --artifact spec.md lists them all." ||
		lines[22] != "Fix each text, then request approval again." {
		t.Fatalf("error =\n%s\nwant the head, 20 finding lines, the count of the other 481 and the next step", body.Error)
	}
	approval := (&specFormRefusal{act: specform.ActApproval, findings: findings[:1], more: refusal.more}).body()
	if !strings.HasSuffix(approval.Error, "\nTo send these findings to the author, choose Request changes.") ||
		approval.ChangesReason != "Dispatch refused my approval, because 1 text breaks the form of The Spec.\n"+
			"- statement s0 in spec.md version 3: it has 3 sentences, and a statement has one or two.\n"+
			"Fix each text, then request approval again." {
		t.Fatalf("approval body = error %q, changes_reason %q", approval.Error, approval.ChangesReason)
	}
}

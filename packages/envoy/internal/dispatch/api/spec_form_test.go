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

// The arrival of an issue under a root is read from the parents as the events left them at each
// move (parentHistory.arrival): the last event at which the nearest named root above the issue
// became the root. In each case R is a root and R2 a root under it, O and P stand outside every
// root, L1 and L2 are levels under R, and each issue's changes are its events: its creation, then
// each write that set its parent. A move inside the root, a move of the root itself and an issue
// made under the root are no arrival; a move in, a move back in and a move of an ancestor are.
func TestAnArrivalIsReadFromTheParentsAsTheyWereAtEachMove(t *testing.T) {
	base := parentHistory{
		"R": {{1, ""}}, "O": {{2, ""}}, "L1": {{3, "R"}}, "L2": {{4, "R"}}, "R2": {{5, "L1"}}, "P": {{6, "O"}},
	}
	with := func(changes parentHistory) parentHistory {
		history := parentHistory{}
		for key, list := range base {
			history[key] = list
		}
		for key, list := range changes {
			history[key] = list
		}
		return history
	}
	for _, c := range []struct {
		name    string
		history parentHistory
		issue   string
		event   int64
		arrived bool
	}{
		{"made under the root", with(parentHistory{"I": {{10, "L1"}}}), "I", 0, false},
		{"moved in once", with(parentHistory{"I": {{10, "O"}, {20, "L1"}}}), "I", 20, true},
		{"moved inside the root after it moved in", with(parentHistory{"I": {{10, "O"}, {20, "L1"}, {30, "L2"}}}), "I", 20, true},
		{"moved out and in again", with(parentHistory{"I": {{10, "L1"}, {20, "O"}, {30, "L2"}}}), "I", 30, true},
		{"a parent moved in with it", with(parentHistory{"I": {{10, "P"}}, "P": {{6, "O"}, {20, "L1"}}}), "I", 20, true},
		{"a parent moved inside the root", with(parentHistory{"I": {{10, "P"}}, "P": {{6, "L1"}, {20, "L2"}}}), "I", 0, false},
		{"a parent moved in before the issue was made", with(parentHistory{"I": {{30, "P"}}, "P": {{6, "O"}, {20, "L1"}}}), "I", 20, true},
		{"the root itself moved", with(parentHistory{"I": {{10, "L1"}}, "R": {{1, ""}, {20, "O"}}}), "I", 0, false},
		{"moved from the root under it to a level of the root", with(parentHistory{"I": {{10, "R2"}, {20, "L2"}}}), "I", 20, true},
		{"the events put it under no root", with(parentHistory{"I": {{10, "O"}}}), "I", 0, false},
	} {
		if event, arrived := c.history.arrival(c.issue, "R", []string{"R", "R2"}); event != c.event || arrived != c.arrived {
			t.Errorf("%s: arrival = %d, %t; want %d, %t", c.name, event, arrived, c.event, c.arrived)
		}
	}
}

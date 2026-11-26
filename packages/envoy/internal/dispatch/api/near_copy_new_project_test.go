package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/testwait"
)

// The texts of the issues the daemon's TestBasisGateOpensOnAnAncestorApproval
// (packages/daemon/internal/daemon/basis_gate_scratch_dispatch_test.go) files, as its rig writes
// them (person_handover_scratch_dispatch_test.go): a root's spec (personHandoverSpec), and the
// decision block its carried case writes into a parent's spec (personHandoverBlock).
const (
	basisGateRootSpec = "# Plan\n\nThe service ships behind a flag.\n"
	basisGateAskBlock = ":::ask{#wave urgency=\"med\" multiple=\"false\"}\nHow many regions take the first wave?\n\n- Two: The two with the fewest tenants.\n- Five: Half of them.\n:::\n"
)

// basisGateRequirements is a parent's spec as that rig writes it, requirementsMarkdown(title,
// "p1", "p2", "p3"): a document headed title whose three requirement blocks each hold a sentence
// of their own.
func basisGateRequirements(title string) string {
	var text strings.Builder
	fmt.Fprintf(&text, "# %s\n\nWhat this issue states, one requirement to a block.\n\n", title)
	for _, id := range []string{"p1", "p2", "p3"} {
		fmt.Fprintf(&text, ":::requirement{#%s}\n**%s.** The change delivers %s.\n:::\n\n", id, id, id)
	}
	return text.String()
}

// TestANewProjectsFirstIssuesMeetTheNearCopyGateWithoutItsEvalsDocument: in a project the create
// route makes, and so seeds with its evals-md (dispatch://INFRA-2916, R5), the daemon's basis-gate
// proof files its eight issues in its order, with its titles and specs, and the near-copy gate
// answers each and lets each in. The term statistics are the ones the first create's related
// check refreshes, of the project as it stands after that create, as in the proof, where nothing
// refreshes them again: the document, the first issue and its spec.
//
// The fourth issue's spec is the second's template under another heading, so it shares ten of
// its thirteen words with that open issue. With the document in the statistics, "ship", which the
// document, the first issue and its spec hold, weighed 0.13 where each other word weighed 0.98,
// and the fourth scored 0.82 against the second and was refused 409 POSSIBLE_DUPLICATE, as the
// proof was in CI. The gate leaves the document out of its statistics (pickNearCopyTerms), so
// every one of those words weighs alike and the fourth scores 10/13, below the refusal bar, as it
// did before projects were seeded.
func TestANewProjectsFirstIssuesMeetTheNearCopyGateWithoutItsEvalsDocument(t *testing.T) {
	t.Parallel()
	handler, _, _, srv := newRegisteredTestServer(t, testServerOptions{duplicateBound: nearCopyVerdictBound(t)})
	const project = "BASIS"
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": project, "name": "A person's handover",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
	}
	issues := []struct{ title, spec string }{
		{"Ledger export rewrite", basisGateRootSpec},
		{"Quarterly billing overhaul", basisGateRequirements("Billing")},
		{"Harbor crane scheduler", basisGateRootSpec},
		{"Shipping manifest platform", basisGateRequirements("Shipping") + basisGateAskBlock},
		{"Glacier telemetry probe", basisGateRootSpec},
		{"Arctic sensor network", basisGateRequirements("Sensors")},
		{"Violin tuning assistant", basisGateRootSpec},
		{"Orchestra rehearsal planner", basisGateRequirements("Rehearsals")},
	}
	for index, issue := range issues {
		response := createIssueRequest(t, handler, map[string]any{"project": project, "title": issue.title, "spec": issue.spec})
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q, issue %d of the proof's %d: status=%d body=%s, want 201",
				issue.title, index+1, len(issues), response.Code, strings.TrimSpace(response.Body.String()))
		}
		created := requireAnsweredCreate(t, response)
		if index == 0 {
			// The refresh the first create's related check started, waited out here, or started
			// here where that check ended before its pick: either counts the project as it stands
			// after its first issue.
			refreshed := testwait.Receive(t, "the refresh of "+project+"'s term statistics after its first issue", srv.ensureSearchTermStats(project))
			if refreshed.Err != nil {
				t.Fatalf("refresh %s's term statistics: %v", project, refreshed.Err)
			}
		}
		t.Logf("%s %q went in past the near-copy gate", created.Key, issue.title)
	}
}

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specform"
)

// termContainers is each place of formContainers, and a decision block, whose options are the
// items of a bullet list.
var termContainers = append(formContainers[:len(formContainers):len(formContainers)], struct {
	name  string
	place func(block string) string
	node  bool
}{"a decision block", func(block string) string {
	return ":::ask{#which urgency=\"med\" multiple=\"false\"}\nWhich definition does the document keep?\n\n" + block + "\n:::"
}, true})

// R50: "Dispatch refuses an approval request … that holds a second definition of a term, and it
// names the term." The class of the rule through the tree that the application reads: lease is a
// term of the list, and each document of a request (the spec, the Verification document and the
// root's terms.md) holds another term line of lease in one place. Where the parser reads blocks (a
// block quote, a list item, a callout and a decision block, alone or nested), the request is
// refused with one finding of lease: under the heading Terms a second definition (Kind
// definition), and under another heading a term line that is no definition (Kind term_line). In
// code or in a table cell the line is no block, so it is no definition, and no finding names lease.
// Each row goes through the approval request route.
func TestASecondDefinitionIsRefusedInEachPlaceOfEachDocumentOfARequest(t *testing.T) {
	if len(termContainers) < 21 {
		t.Fatalf("%d places, want at least 21", len(termContainers))
	}
	second := "- " + termLine("lease", otherLease)
	for _, container := range termContainers {
		for _, part := range []struct {
			name       string
			underTerms bool
		}{{"under Terms", true}, {"under another heading", false}} {
			t.Run(container.name+", "+part.name, func(t *testing.T) {
				tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
				listed := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
				approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
				where := "spec.md version 1 of " + listed.Key
				placed := container.place(second) + "\n"
				for _, document := range []string{"spec.md", "verification.md", "terms.md"} {
					spec := "# Spec\n\n:::requirement{#keep}\nDispatch keeps each comment.\n:::\n"
					verification := "# Verification\n\n:::check{#kept}\nThe test reads each comment.\n:::\n"
					terms := "# Terms\n\n- " + termLine("session", sessionDefinition) + "\n"
					heading := "## Terms\n\n"
					if !part.underTerms {
						heading = "## Notes\n\n"
					}
					switch document {
					case "spec.md":
						spec += "\n" + heading + placed
					case "verification.md":
						verification += "\n" + heading + placed
					case "terms.md":
						heading = "\n"
						if !part.underTerms {
							heading = "\n# Notes\n\n"
						}
						terms += heading + placed
					}
					var response *httptest.ResponseRecorder
					var artifactID string
					if document == "terms.md" {
						artifact := uploadFormDocument(t, tree.handler, tree.root, "terms.md", terms)
						artifactID = artifact.ID
						response = requestFormApproval(t, tree.handler, artifact.ID, nil)
					} else {
						issue := createFormIssue(t, tree.handler, tree.root, "Names lease in "+document, spec)
						checks := uploadFormDocument(t, tree.handler, issue.Key, "verification.md", verification)
						artifactID = map[string]string{"spec.md": issue.PrimaryArtifactID, "verification.md": checks.ID}[document]
						response = requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, map[string]any{"include_verification": true})
					}
					var leases []specFormFinding
					if response.Code == http.StatusConflict {
						for _, finding := range decodeBody[specFormError](t, response).Findings {
							if specform.TermKey(finding.Node) == "lease" {
								leases = append(leases, finding)
							}
						}
					}
					if !container.node {
						if len(leases) != 0 {
							t.Errorf("%s: the line in %s is no block, but the refusal names lease: %#v", document, container.name, leases)
						}
						continue
					}
					// A heading of the place's own at its top closes a Terms part of its level.
					closed := strings.HasPrefix(placed, "## ") && document != "terms.md"
					want := termLineFinding(document, artifactID, 1, "lease", otherLease, where)
					if part.underTerms && !closed {
						want = definitionFinding(document, artifactID, 1, "lease", otherLease, "term-second-definition", alreadyDefines(where))
					}
					if response.Code != http.StatusConflict || len(leases) != 1 || leases[0] != want {
						t.Errorf("%s: status=%d, findings of lease %#v; want 409 and only %#v: %s", document, response.Code, leases, want, response.Body.String())
					}
				}
			})
		}
	}
}

// A heading whose text a person reads as Terms is the heading Terms, also when emphasis marks it,
// as the plan reads the list under "a heading whose text is Terms": so with lease in the list, a
// term line of lease under "## **Terms**" is a second definition. The control: "## **Terms:**"
// reads as "Terms:", which is a terms-heading finding, and the line under it is a term line that
// is no definition. Each row goes through the approval request route.
func TestAHeadingThatAPersonReadsAsTermsIsTheListOfTermsOnTheRoute(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	listed := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
	requireFormRows(t, tree, []formRow{
		{name: "a heading Terms in bold", spec: "## **Terms**\n\n- " + termLine("lease", otherLease) + "\n",
			want: []string{"definition lease term-second-definition"}},
		{name: "the control: a heading Terms: in bold", spec: "## **Terms:**\n\n- " + termLine("lease", otherLease) + "\n",
			want: []string{`heading "Terms:" terms-heading`, "term_line lease term-second-definition"}},
	})
}

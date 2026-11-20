package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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

// A heading opens or closes a part of Terms only among the blocks of its own container, and a
// footnote starts outside every part (form slice 1's walk), so the rule of a second definition
// follows the part where its line stands. With lease in the list, a term line of lease is a second
// definition (Kind definition) where its part is Terms, and a term line that is no definition
// (Kind term_line) elsewhere: either way the request is refused, and each row names which. Each
// row goes through the approval request route, and only the findings that name lease are compared.
func TestASecondDefinitionFollowsThePartOfItsContainer(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	listed := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
	second := "- " + termLine("lease", otherLease)
	const definition, line = "definition lease term-second-definition", "term_line lease term-second-definition"
	for _, row := range []struct{ name, spec, want string }{
		{"a second definition in a block quote under Terms", "## Terms\n\n> " + second + "\n", definition},
		{"a second definition in a callout under Terms", "## Terms\n\n:::callout{kind=\"note\"}\n" + second + "\n:::\n", definition},
		{"a heading Terms inside a block quote, the line inside it", "> ## Terms\n>\n> " + second + "\n", definition},
		{"a heading Terms inside a block quote, the line after it", "> ## Terms\n\n" + second + "\n", line},
		{"a heading Terms inside a callout, the line after it", ":::callout{kind=\"note\"}\n## Terms\n:::\n\n" + second + "\n", line},
		{"a heading inside a block quote under Terms closes no outer part", "## Terms\n\n> ## Example\n>\n> A note.\n\n" + second + "\n", definition},
		{"a definition in a footnote under Terms", ":::requirement{#keep}\nDispatch keeps each comment.[^1]\n:::\n\n## Terms\n\n- " +
			termLine("sirocco", "The wind of one desert.") + "\n\n[^1]: " + second + "\n", line},
		{"a definition in a footnote under another heading", ":::requirement{#keep}\nDispatch keeps each comment.[^1]\n:::\n\n## Notes\n\n[^1]: " + second + "\n", line},
	} {
		t.Run(row.name, func(t *testing.T) {
			issue := createFormIssue(t, tree.handler, tree.root, row.name, "# Spec\n\n"+row.spec)
			response := requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)
			if response.Code != http.StatusConflict {
				t.Fatalf("request: status=%d body=%s, want 409 SPEC_FORM naming %q", response.Code, response.Body.String(), row.want)
			}
			var leases []string
			for _, finding := range decodeBody[specFormError](t, response).Findings {
				if specform.TermKey(finding.Node) == "lease" {
					leases = append(leases, string(finding.Kind)+" "+finding.Node+" "+string(finding.Rule))
				}
			}
			if len(leases) != 1 || leases[0] != row.want {
				t.Fatalf("the findings of lease = %q, want only %q: %s", leases, row.want, response.Body.String())
			}
		})
	}
}

// R50 is a gate: Dispatch refuses a request that holds a second definition of a term. When an
// approved document under the root is a version that the parser refuses, no rule can compare a
// definition with its terms, so a request whose documents hold a definition is refused, with one
// finding of kind document and rule unreadable that names that document, its version and the way
// out; the carry's gate breaks on it too. A request that holds no definition is not refused for
// it (the first slice's skip). The control: with the same terms.md readable, the request that
// defines session again gets the finding of a second definition.
func TestAnUnreadableApprovedDocumentRefusesEachRequestThatHoldsADefinition(t *testing.T) {
	lease := specWithTerms(termLine("lease", leaseDefinition))
	again := specWithTerms(termLine("session", otherSession))
	t.Run("control: a readable terms.md", func(t *testing.T) {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
		approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
		requireFormRows(t, tree, []formRow{{name: "defines session again", spec: strings.TrimPrefix(again, "# Spec\n\n"),
			want: []string{"definition session term-second-definition"}}})
	})
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	terms := approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	storeUnreadableVersion(t, tree.database, terms.ID, 1)
	const wayOut = "Write a new version of that document and request its approval"
	for _, row := range []struct{ name, spec string }{
		{"a second definition of a term of the unreadable terms.md", again},
		{"a new term", lease},
	} {
		t.Run(row.name, func(t *testing.T) {
			issue := createFormIssue(t, tree.handler, tree.root, row.name, row.spec)
			response := requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)
			body := decodeBody[specFormError](t, response)
			var unreadable []specFormFinding
			for _, finding := range body.Findings {
				if finding.Kind == specform.KindDocument {
					unreadable = append(unreadable, finding)
				}
			}
			if response.Code != http.StatusConflict || len(unreadable) != 1 || unreadable[0].Node != "terms.md" || unreadable[0].Rule != "unreadable" ||
				unreadable[0].Where != "terms.md version 1 of ACME-1" || !strings.Contains(unreadable[0].Detail, wayOut) {
				t.Fatalf("request: status=%d body=%s, want 409 with one unreadable finding of terms.md version 1 of ACME-1", response.Code, response.Body.String())
			}
			gate, err := FormGate("ACME-1", "")
			if err != nil {
				t.Fatal(err)
			}
			tx, err := tree.database.Pool.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			breaks, err := gate(context.Background(), tx, nil, model.AskApprovalDocument{ArtifactID: issue.PrimaryArtifactID, Name: "spec.md", Version: 1})
			_ = tx.Rollback(context.Background())
			if !breaks || err != nil {
				t.Errorf("the carry's gate = %v, %v; want a break and no error", breaks, err)
			}
		})
	}
	t.Run("control: a request that holds no definition", func(t *testing.T) {
		issue := createFormIssue(t, tree.handler, tree.root, "No definition", "# Spec\n\n"+formRequirement("clean", "Dispatch shows each record."))
		requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil))
	})
}

// olderUnreadLine is the line that names an older approved version that Dispatch cannot read: it
// is no source of the list, and it only orders the definitions of its document.
func olderUnreadLine(document string, version int, issue string) string {
	return fmt.Sprintf("Dispatch cannot read %s version %d of %s, an older approved version of a document under the root. "+
		"So the list orders the definitions of that document as if that version held no term.", document, version, issue)
}

// R50: "Dispatch refuses an approval request … that holds a second definition of a term". The list
// reads the latest approved version of each document, so an older approved version is no source
// of it: it only orders the definitions of a term that two documents define. When Dispatch cannot
// read one, it holds no term for that order, each answer names it, and it refuses no request: a
// request with a new term opens, also after the next version of its document is approved (the
// reviewer's probe U1b). A defines lease in its first two approved versions, and B, approved
// outside the root with another definition, moves in. When B moves in after A's second approval,
// A keeps lease. When B moves in before it (the reviewer's probe U1), A's definition joins at its
// second approval once its first version cannot be read, so B's governs: the choice that the
// README states ("an older approved version that Dispatch cannot read holds no term"). Then A's
// next version that keeps lease is a second definition. The way out that keeps A's definition is
// B's next version without lease: once the person approves it, A's definition governs again. The
// root's terms.md is no way out, since a definition of lease there is a second definition too. The
// control of each row: before the first version is made unreadable, A keeps lease.
func TestAnUnreadableOlderApprovedVersionOnlyOrdersTheList(t *testing.T) {
	const second, third = "The hold of one session on one work unit.", "The hold that one session keeps on one work unit."
	for _, row := range []struct {
		name       string
		movesFirst bool
	}{
		{"B moves under the root after A's second approval", false},
		{"B moves under the root before A's second approval", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
			a := createFormIssue(t, tree.handler, tree.root, "A", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, tree.handler, a.PrimaryArtifactID)
			b := createFormIssue(t, tree.handler, tree.outside, "B", specWithTerms(termLine("lease", otherLease)))
			approveFormDocument(t, tree.handler, b.PrimaryArtifactID)
			if row.movesFirst {
				reparentFormIssue(t, tree.handler, b.Key, tree.root)
			}
			approveRevisionThrough(t, tree, tree.handler, a, termLine("lease", leaseDefinition), termLine("lease", second), 2)
			if !row.movesFirst {
				reparentFormIssue(t, tree.handler, b.Key, tree.root)
			}
			heldByA := specFormTerm{Term: "lease", Definition: second, Document: "spec.md", Version: 2, Issue: a.Key}
			if listed, count := listedOf(t, tree.handler, a.PrimaryArtifactID, "lease"); count != 1 || listed != heldByA {
				t.Fatalf("control, before the unreadable version: the list holds lease %d times, as %#v; want once, as %#v", count, listed, heldByA)
			}

			storeUnreadableVersion(t, tree.database, a.PrimaryArtifactID, 1)
			want := heldByA
			if row.movesFirst {
				want = specFormTerm{Term: "lease", Definition: otherLease, Document: "spec.md", Version: 1, Issue: b.Key}
			}
			if listed, count := listedOf(t, tree.handler, a.PrimaryArtifactID, "lease"); count != 1 || listed != want {
				t.Fatalf("the list holds lease %d times, as %#v; want once, as %#v", count, listed, want)
			}
			line := olderUnreadLine("spec.md", 1, a.Key)
			requireNewTermOpens := func(issue, term string) {
				t.Helper()
				c := createFormIssue(t, tree.handler, tree.root, issue, specWithTerms(termLine(term, "A strong wind of one coast.")))
				opened := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, c.PrimaryArtifactID, nil))
				if opened.SpecForm == nil || !slices.ContainsFunc(opened.SpecForm.Unread, func(unread specFormUnread) bool { return unread.Line == line }) {
					t.Errorf("the request of %s opened, and its spec_form = %#v; want unread to name %q", issue, opened.SpecForm, line)
				}
			}
			requireNewTermOpens("C defines gale", "gale")
			if row.movesFirst {
				reviseDocument(t, tree.handler, a.PrimaryArtifactID, termLine("lease", second), termLine("lease", third), 3)
				awaitSettled(t, tree.database, a.PrimaryArtifactID)
				refused := requestFormApproval(t, tree.handler, a.PrimaryArtifactID, nil)
				byB := func(finding specFormFinding) bool {
					return finding.Node == "lease" && finding.Rule == "term-second-definition" &&
						strings.HasPrefix(finding.Detail, "spec.md version 1 of "+b.Key+" already defines this term")
				}
				if refused.Code != http.StatusConflict || !slices.ContainsFunc(decodeBody[specFormError](t, refused).Findings, byB) {
					t.Fatalf("A's next version that keeps lease: status=%d body=%s; want 409 naming B's definition", refused.Code, refused.Body.String())
				}
				terms := uploadFormDocument(t, tree.handler, tree.root, "terms.md", termsMd(termLine("lease", third)))
				if response := requestFormApproval(t, tree.handler, terms.ID, nil); response.Code != http.StatusConflict ||
					!slices.ContainsFunc(decodeBody[specFormError](t, response).Findings, byB) {
					t.Fatalf("the root's terms.md that defines lease: status=%d body=%s; want 409 naming B's definition", response.Code, response.Body.String())
				}
				approveRevisionThrough(t, tree, tree.handler, b, termLine("lease", otherLease), termLine("gust", "A short strong wind of one coast."), 2)
				requireFormRequestOpened(t, requestFormApproval(t, tree.handler, a.PrimaryArtifactID, nil))
				if listed, count := listedOf(t, tree.handler, a.PrimaryArtifactID, "lease"); count != 1 || listed != heldByA {
					t.Fatalf("after B's version without lease: the list holds lease %d times, as %#v; want once, as %#v", count, listed, heldByA)
				}
				return
			}
			// The next version of A: the way out of a latest approved version, which changes nothing here.
			approveRevisionThrough(t, tree, tree.handler, a, termLine("lease", second), termLine("lease", third), 3)
			requireNewTermOpens("D defines sirocco", "sirocco")
		})
	}
}

// The fail-closed rule, with two approved documents that hold definitions and whose latest
// approved versions Dispatch cannot read: each refuses a new version of the other that holds a
// definition. The refusal names the way out, a new version of the named document, and when that
// version is refused for the same reason, that version first with no definition. That way out
// clears both: Y's version with no definition opens and is approved, and then X's version with its
// definition opens.
func TestTheWayOutOfTwoUnreadableDocumentsIsAVersionWithNoDefinition(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	x := createFormIssue(t, tree.handler, tree.root, "X", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, x.PrimaryArtifactID)
	y := createFormIssue(t, tree.handler, tree.root, "Y", specWithTerms(termLine("gale", "A strong wind of one coast.")))
	approveFormDocument(t, tree.handler, y.PrimaryArtifactID)
	reviseDocument(t, tree.handler, x.PrimaryArtifactID, termLine("lease", leaseDefinition), termLine("lease", otherLease), 2)
	reviseDocument(t, tree.handler, y.PrimaryArtifactID, "Terms", "Winds", 2)
	awaitSettled(t, tree.database, x.PrimaryArtifactID, y.PrimaryArtifactID)
	storeUnreadableVersion(t, tree.database, x.PrimaryArtifactID, 1)
	storeUnreadableVersion(t, tree.database, y.PrimaryArtifactID, 1)

	refused := requestFormApproval(t, tree.handler, x.PrimaryArtifactID, nil)
	const wayOut = "Write a new version of that document and request its approval. " +
		"If Dispatch refuses that request for the same reason, write that version with no definition first"
	var unreadable []specFormFinding
	for _, finding := range decodeBody[specFormError](t, refused).Findings {
		if finding.Kind == specform.KindDocument {
			unreadable = append(unreadable, finding)
		}
	}
	if refused.Code != http.StatusConflict || len(unreadable) != 1 || unreadable[0].Where != "spec.md version 1 of "+y.Key ||
		!strings.HasSuffix(unreadable[0].Detail, wayOut) {
		t.Fatalf("the request of X: status=%d body=%s; want 409 with one unreadable finding of spec.md version 1 of %s that ends %q",
			refused.Code, refused.Body.String(), y.Key, wayOut)
	}
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, y.PrimaryArtifactID, nil))
	approveFormDocument(t, tree.handler, y.PrimaryArtifactID)
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, x.PrimaryArtifactID, nil))
}

// latestUnreadLine is the whole line that names the latest approved version of a document that
// Dispatch cannot read: no rule compared its texts, the list of terms can lack its terms, and the
// way out.
func latestUnreadLine(document string, version int, issue string) string {
	return unreadLine(document, version, issue) + " The list of terms can lack the terms of that version. " +
		"To change that, write a new version of that document and request its approval."
}

// namesUnread reports whether unread names version of the document whose id is artifactID, with
// line.
func namesUnread(unread []specFormUnread, artifactID string, version int, line string) bool {
	return slices.ContainsFunc(unread, func(each specFormUnread) bool {
		return each.ArtifactID == artifactID && each.Version == version && each.Line == line
	})
}

// R50: "The Spec holds a list of defined terms." The list reads the latest approved version of each
// document, so when Dispatch cannot read that version, the list lacks its terms. Each answer that
// gives the list then names that version in unread, with the line that says so, also when the
// answer judges that same document (the reviewer's probe H1): the read at its newer version, which
// `dispatch spec-form --terms` prints, and the read at the unreadable version itself, the answer of
// its approval request, the refusal of its request and of the person's approval, and the reason
// that a reopen writes on its request that breaks the form. The gate keeps its own view: the
// version that a request names stands in for the approved one, so the request of that document
// with a definition opens. The control: the read of another document names that version too.
func TestEachAnswerThatGivesTheListNamesEachApprovedVersionThatDispatchCannotRead(t *testing.T) {
	fault := formFaults[0]
	spec := "# Spec\n\n" + formRequirement(fault.node, fault.clean) + "\n## Terms\n\n" + termList([]string{termLine("lease", leaseDefinition)})
	session := specFormTerm{Term: "session", Definition: sessionDefinition, Document: "terms.md", Version: 1, Issue: "ACME-1"}
	t.Run("the document itself", func(t *testing.T) {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
		approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
		d := createFormIssue(t, tree.handler, tree.root, "D", spec)
		approveFormDocument(t, tree.handler, d.PrimaryArtifactID)
		e := createFormIssue(t, tree.handler, tree.root, "E", "# Spec\n\n"+formRequirement("clean", "Dispatch shows each record."))
		storeUnreadableVersion(t, tree.database, d.PrimaryArtifactID, 1)
		line := latestUnreadLine("spec.md", 1, d.Key)
		t.Run("control: the read of another document", func(t *testing.T) {
			if read := specFormReadOf(t, tree.handler, e.PrimaryArtifactID); !namesUnread(read.Unread, d.PrimaryArtifactID, 1, line) {
				t.Errorf("the read of E names %#v in unread; want %q", read.Unread, line)
			}
		})
		t.Run("the read at the unreadable version", func(t *testing.T) {
			response := dispatchRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+d.PrimaryArtifactID+"/spec-form?version=1", nil, "alice")
			if read := decodeBody[specFormRead](t, response); response.Code != http.StatusOK || !namesUnread(read.Unread, d.PrimaryArtifactID, 1, line) {
				t.Errorf("the read of D at version 1: status=%d body=%s; want unread to name %q", response.Code, response.Body.String(), line)
			}
		})
		reviseDocument(t, tree.handler, d.PrimaryArtifactID, leaseDefinition, otherLease, 2)
		awaitSettled(t, tree.database, d.PrimaryArtifactID)
		t.Run("the read at the newer version, which --terms prints", func(t *testing.T) {
			read := specFormReadOf(t, tree.handler, d.PrimaryArtifactID)
			if read.Version != 2 || !slices.Equal(read.Terms, []specFormTerm{session}) || !namesUnread(read.Unread, d.PrimaryArtifactID, 1, line) {
				t.Errorf("the read of D at version %d lists %#v and names %#v in unread; want version 2, the list without lease, and unread to name %q",
					read.Version, read.Terms, read.Unread, line)
			}
		})
		t.Run("the answer of the request, which the gate opens", func(t *testing.T) {
			opened := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, d.PrimaryArtifactID, nil))
			if opened.SpecForm == nil || opened.SpecForm.Terms != 1 || !namesUnread(opened.SpecForm.Unread, d.PrimaryArtifactID, 1, line) {
				t.Errorf("the request of D opened with spec_form %#v; want 1 term and unread to name %q", opened.SpecForm, line)
			}
		})
	})
	t.Run("a refusal", func(t *testing.T) {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
		approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
		d := createFormIssue(t, tree.handler, tree.root, "D", spec)
		approveFormDocument(t, tree.handler, d.PrimaryArtifactID)
		reviseDocument(t, tree.handler, d.PrimaryArtifactID, fault.clean, fault.broken, 2)
		awaitSettled(t, tree.database, d.PrimaryArtifactID)
		storeUnreadableVersion(t, tree.database, d.PrimaryArtifactID, 1)
		lines := append(fault.lines(2), latestUnreadLine("spec.md", 1, d.Key))
		t.Run("the refusal of the request", func(t *testing.T) {
			requireFormRefusal(t, "the request of D", requestFormApproval(t, tree.handler, d.PrimaryArtifactID, nil),
				specform.ActRequest, lines, fault.findings(d, 2)...)
		})
		t.Run("the refusal of the person's approval", func(t *testing.T) {
			requireFormRefusal(t, "the approval of D", reviewFormDocument(t, tree.handler, d.PrimaryArtifactID, "approved", ""),
				specform.ActApproval, lines, fault.findings(d, 2)...)
		})
	})
	t.Run("the reason of a reopen", func(t *testing.T) {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
		approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
		d := createFormIssue(t, tree.handler, tree.outside, "D", spec)
		approveFormDocument(t, tree.handler, d.PrimaryArtifactID)
		reviseDocument(t, tree.handler, d.PrimaryArtifactID, fault.clean, fault.broken, 2)
		awaitSettled(t, tree.database, d.PrimaryArtifactID)
		request := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, d.PrimaryArtifactID, nil)).Ask.ID
		storeUnreadableVersion(t, tree.database, d.PrimaryArtifactID, 1)
		reparentFormIssue(t, tree.handler, d.Key, tree.root)
		seq := reopenFormIssue(t, tree, d.Key)
		lines := append(fault.lines(2), latestUnreadLine("spec.md", 1, d.Key))
		requireLeftResolved(t, tree.handler, tree.database, d.Key, seq, request, strings.Join(lines, "\n"), model.Actor{Kind: "user", ID: "alice"})
	})
}

// The candidate inputs of the second reader of this rule that no row above pins, each as its spec,
// through the approval request route, with session in the list from the root's terms.md. A second
// definition is refused wherever a person reads it as one: in a block quote or a callout under no
// heading Terms as a term line, under a heading Terms of a footnote, under another case or a link
// of the name, and in code font, since a name is the name as the page shows it. A name with a
// Cyrillic letter breaks term-name. A table, and
// code that shows a heading Terms, hold no definition. The honest controls: a first definition
// under a heading Terms in bold opens, and a definition whose footnote gives a source has two
// sentences, with a finding that names the footnote.
func TestEachCandidateInputOfTheSecondReaderOnTheRoute(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	approveTermsMd(t, tree, tree.root, termsMd(termLine("session", "One conversation of an agent.")))
	const footnoted = "the definition has 2 sentences with its footnote, and a definition has one. " +
		"A footnote is part of the definition that refers to it. " +
		"Put what the footnote says in the one sentence of the definition, or remove the footnote"
	for _, row := range []struct {
		name, markdown string
		want           []string // each finding as "<kind> <node> <rule>"; none: the request opens
		definitions    int      // the definitions that an opened request read
		detail         string   // the detail of the one finding, when set
	}{
		{name: "a quote", markdown: "# Spec\n\n> - **session**: One saved process.\n",
			want: []string{"term_line session term-second-definition"}},
		{name: "a callout", markdown: "# Spec\n\n:::callout{kind=\"note\"}\n- **session**: One saved process.\n:::\n",
			want: []string{"term_line session term-second-definition"}},
		{name: "a table cell", markdown: "# Spec\n\n| Term | Meaning |\n| --- | --- |\n| **session** | One saved process. |\n"},
		{name: "a code example", markdown: "# Spec\n\n~~~markdown\n## Terms\n\n- **session**: One saved process.\n~~~\n"},
		{name: "a referenced footnote", markdown: "# Spec\n\nA note.[^meaning]\n\n[^meaning]:\n    ## Terms\n\n    - **session**: One saved process.\n",
			want: []string{"definition session term-second-definition"}},
		{name: "another case", markdown: "# Spec\n\n## Terms\n\n- **Session**: One saved process.\n",
			want: []string{"definition Session term-second-definition"}},
		{name: "a linked name", markdown: "# Spec\n\n## Terms\n\n- **[session](https://example.com/session)**: One saved process.\n",
			want: []string{"definition session term-second-definition"}},
		{name: "a code-font name: the name as the page shows it, so one name with session", markdown: "# Spec\n\n## Terms\n\n- **`session`**: One saved process.\n",
			want: []string{"definition session term-second-definition"}},
		{name: "a visually similar name", markdown: "# Spec\n\n## Terms\n\n- **sessi\u043en**: One saved process.\n",
			want: []string{"definition sessi\u043en term-name"}},
		{name: "the honest control: a first definition under a bold heading Terms", markdown: "# Spec\n\n## **Terms**\n\n- **zephyr**: One warm wind.\n",
			definitions: 1},
		{name: "the honest control: a footnote that gives a source", markdown: "# Spec\n\n## Terms\n\n- **zephyr**: One warm wind.[^source]\n\n[^source]: See [source](https://example.com/source).\n",
			want: []string{"definition zephyr definition-sentences"}, detail: footnoted},
		{name: "the acceptance tester's footnote", markdown: "# Spec\n\n## Terms\n\n- **mooring**: The rope that ties one boat to a dock.[^1]\n\n[^1]: The harbour manual of 2024 gives this meaning.\n",
			want: []string{"definition mooring definition-sentences"}, detail: footnoted},
	} {
		t.Run(row.name, func(t *testing.T) {
			issue := createFormIssue(t, tree.handler, tree.root, row.name, row.markdown)
			response := requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)
			if len(row.want) == 0 {
				read := requireFormRequestOpened(t, response)
				if read.SpecForm == nil || read.SpecForm.Texts[specform.KindDefinition] != row.definitions {
					t.Fatalf("the request opened with spec_form %#v; want %d definitions read", read.SpecForm, row.definitions)
				}
				return
			}
			if response.Code != http.StatusConflict {
				t.Fatalf("request: status=%d body=%s, want 409 SPEC_FORM naming %q", response.Code, response.Body.String(), row.want)
			}
			findings := decodeBody[specFormError](t, response).Findings
			var got []string
			for _, finding := range findings {
				got = append(got, fmt.Sprintf("%s %s %s", finding.Kind, finding.Node, finding.Rule))
			}
			if !slices.Equal(got, row.want) {
				t.Fatalf("the refusal names %q, want %q: %s", got, row.want, response.Body.String())
			}
			if row.detail != "" && findings[0].Detail != row.detail {
				t.Errorf("the detail is %q, want %q", findings[0].Detail, row.detail)
			}
		})
	}
}

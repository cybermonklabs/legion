package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
)

// The list of terms of a root (R50) is the term lines of the person-approved version of the
// root's terms.md, and of the latest approved version of each covered document under the root.
// A covered document that defines a term of the list again is refused, and the refusal names the
// term and the document that defines it.

const (
	sessionDefinition = "One conversation of an agent, with its id, in which the agent does its work."
	otherSession      = "The record of one agent process in Envoy."
	leaseDefinition   = "The right of one session to one work unit for a time."
	otherLease        = "The time for which one session holds one work unit."
	twoSentences      = "The registration that a session sends again. It comes every 90 seconds."
)

// termLine is the text of the term line that defines term as definition, after its list marker:
// an edit replaces the text of a list item, not its marker.
func termLine(term, definition string) string {
	return "**" + term + "**: " + definition
}

// termList is lines as the items of a bullet list.
func termList(lines []string) string {
	return "- " + strings.Join(lines, "\n- ") + "\n"
}

// specWithTerms is a spec whose heading Terms holds lines.
func specWithTerms(lines ...string) string {
	return "# Spec\n\n## Terms\n\n" + termList(lines)
}

// termsMd is a terms.md that holds lines.
func termsMd(lines ...string) string {
	return "# Terms\n\n" + termList(lines)
}

// alreadyDefines is the detail of a second definition of a term that where defines.
func alreadyDefines(where string) string {
	return where + " already defines this term. Use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first"
}

// approveFormDocument is alice's approval of the document from its header, which must pass.
func approveFormDocument(t *testing.T, handler http.Handler, artifactID string) {
	t.Helper()
	if response := reviewFormDocument(t, handler, artifactID, "approved", ""); response.Code != http.StatusCreated {
		t.Fatalf("approve %s: status=%d body=%s, want 201", artifactID, response.Code, response.Body.String())
	}
}

// approveTermsMd uploads markdown as the terms.md of issue and approves it as alice.
func approveTermsMd(t *testing.T, tree specFormTree, issue, markdown string) model.Artifact {
	t.Helper()
	terms := uploadFormDocument(t, tree.handler, issue, "terms.md", markdown)
	approveFormDocument(t, tree.handler, terms.ID)
	return terms
}

// definitionFinding is the finding of the definition of term in version of the document name
// whose id is artifactID, which breaks rule with detail.
func definitionFinding(name, artifactID string, version int, term, definition, rule, detail string) specFormFinding {
	return specFormFinding{
		Document: name, ArtifactID: artifactID, Version: version,
		Finding: specform.Finding{
			Node: term, Kind: specform.KindDefinition, Where: fmt.Sprintf("%s version %d", name, version),
			Rule: specform.Rule(rule), Detail: detail, Excerpt: definition,
		},
	}
}

// findingLine is the refusal's line for finding.
func findingLine(finding specFormFinding) string {
	return fmt.Sprintf("- %s %s in %s: %s.", finding.Kind, finding.Node, finding.Where, finding.Detail)
}

// requireTerms fails t unless the read of the document answers want as its list of terms.
func requireTerms(t *testing.T, handler http.Handler, artifactID string, want ...specFormTerm) {
	t.Helper()
	if want == nil {
		want = []specFormTerm{}
	}
	if got := specFormReadOf(t, handler, artifactID).Terms; !reflect.DeepEqual(got, want) {
		t.Fatalf("the list of terms of the read of %s = %#v, want %#v", artifactID, got, want)
	}
}

// secondSession is a spec's definition of session, which the root's terms.md version 1 defines.
var secondSession = formFault{
	name: "a second definition", node: "session", kind: specform.KindDefinition,
	clean: termLine("lease", leaseDefinition), broken: termLine("session", otherSession), excerpt: otherSession,
	rule: "term-second-definition", detail: alreadyDefines("terms.md version 1 of ACME-1"),
}

// v-terms, control 1: with the root's terms.md approved defining session, a spec that defines
// session again is refused on each of the six paths to a person's approval, and each finding names
// the term and the document that defines it.
func TestEveryApprovalRouteRefusesASecondDefinition(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	requireEveryApprovalRouteRefuses(t, tree, secondSession)
}

// v-terms, control 3: a definition of two sentences is refused on each of the six paths.
func TestADefinitionOfTwoSentencesIsRefusedOnEveryRoute(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	requireEveryApprovalRouteRefuses(t, tree, formFault{
		name: "a definition of two sentences", node: "heartbeat", kind: specform.KindDefinition,
		clean:  termLine("heartbeat", "The registration that a session sends again every 90 seconds."),
		broken: termLine("heartbeat", twoSentences), excerpt: twoSentences,
		rule: "definition-sentences", detail: "the definition has 2 sentences, and a definition has one",
	})
}

// v-terms, last control: versions of terms.md that no person approved change nothing. They give
// session a new definition and replace heartbeat with lease, and the list keeps the approved
// definitions of heartbeat and session and has no lease, so a spec that defines lease is opened.
func TestTheListCountsOnlyApprovedVersions(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	heartbeat := "The registration that a session sends again every 90 seconds."
	terms := approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition), termLine("heartbeat", heartbeat)))
	reviseDocument(t, tree.handler, terms.ID, termLine("heartbeat", heartbeat), termLine("lease", leaseDefinition), 2)
	reviseDocument(t, tree.handler, terms.ID, termLine("session", sessionDefinition), termLine("session", otherSession), 3)
	awaitSettled(t, tree.database, terms.ID)
	issue := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approved := []specFormTerm{
		{Term: "heartbeat", Definition: heartbeat, Document: "terms.md", Version: 1, Issue: "ACME-1"},
		{Term: "session", Definition: sessionDefinition, Document: "terms.md", Version: 1, Issue: "ACME-1"},
	}
	requireTerms(t, tree.handler, issue.PrimaryArtifactID, approved...)
	requireTerms(t, tree.handler, terms.ID, approved...)
	read := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil))
	if read.SpecForm == nil || read.SpecForm.Terms != 2 {
		t.Fatalf("spec_form = %#v, want the two terms of the list counted", read.SpecForm)
	}
}

// The terms of an approved change request join the list with no copy: a later spec that defines
// the same term is refused, naming that request's spec.
func TestAChangeRequestsApprovedTermsJoinTheList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	change := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, change.PrimaryArtifactID)
	again := createFormIssue(t, tree.handler, tree.root, "Defines lease again", specWithTerms(termLine("lease", otherLease)))
	finding := definitionFinding("spec.md", again.PrimaryArtifactID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+change.Key))
	requireFormRefusal(t, "request", requestFormApproval(t, tree.handler, again.PrimaryArtifactID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
}

// A term that an approved change request defines is changed in that request, not in the root's
// terms.md: a terms.md that defines it is refused, naming the request's spec, and a second version
// of that spec with a changed definition is opened.
func TestATermThatAChangeRequestDefinesIsChangedInThatRequest(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	change := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, change.PrimaryArtifactID)
	terms := uploadFormDocument(t, tree.handler, tree.root, "terms.md",
		termsMd(termLine("session", sessionDefinition), termLine("lease", otherLease)))
	finding := definitionFinding("terms.md", terms.ID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+change.Key))
	requireFormRefusal(t, "request of terms.md", requestFormApproval(t, tree.handler, terms.ID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
	reviseDocument(t, tree.handler, change.PrimaryArtifactID, termLine("lease", leaseDefinition), termLine("lease", otherLease), 2)
	awaitSettled(t, tree.database, change.PrimaryArtifactID)
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, change.PrimaryArtifactID, nil))
}

// A request names the documents whose candidate versions replace their approved ones, so a spec
// whose second version keeps the term its first version defined is no second definition.
func TestADocumentReplacingItsOwnApprovedTermsIsNotRefused(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	change := createFormIssue(t, tree.handler, tree.root, "Defines lease",
		specWithTerms(termLine("lease", leaseDefinition))+"\n:::requirement{#store-comment}\nDispatch stores each comment.\n:::\n")
	approveFormDocument(t, tree.handler, change.PrimaryArtifactID)
	reviseDocument(t, tree.handler, change.PrimaryArtifactID, "Dispatch stores each comment.", "Dispatch keeps each comment.", 2)
	awaitSettled(t, tree.database, change.PrimaryArtifactID)
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, change.PrimaryArtifactID, nil))
}

// The root's terms.md is where a term of it changes: a version that changes the definition of
// session is opened, and once the person approves it the list holds the new definition.
func TestTermsMdItselfMayChangeADefinition(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	terms := approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	reviseDocument(t, tree.handler, terms.ID, termLine("session", sessionDefinition), termLine("session", otherSession), 2)
	awaitSettled(t, tree.database, terms.ID)
	ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, terms.ID, nil)).Ask.ID
	requireTerms(t, tree.handler, terms.ID, specFormTerm{Term: "session", Definition: sessionDefinition, Document: "terms.md", Version: 1, Issue: "ACME-1"})
	if response := answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, ""); response.Code != http.StatusOK {
		t.Fatalf("approve terms.md version 2: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	requireTerms(t, tree.handler, terms.ID, specFormTerm{Term: "session", Definition: otherSession, Document: "terms.md", Version: 2, Issue: "ACME-1"})
}

// Only the root's terms.md holds the list: a level's own approved terms.md is no second list, so
// a spec under that level that defines its term is opened, and the read of it lists only the
// root's terms.
func TestALevelsOwnTermsMdMakesNoSecondList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	level := createFormIssue(t, tree.handler, tree.root, "A level", "# Level\n\nThe level.\n")
	approveTermsMd(t, tree, level.Key, termsMd(termLine("lease", leaseDefinition)))
	child := createFormIssue(t, tree.handler, level.Key, "Under the level", specWithTerms(termLine("lease", otherLease)))
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, child.PrimaryArtifactID, nil))
	requireTerms(t, tree.handler, child.PrimaryArtifactID,
		specFormTerm{Term: "session", Definition: sessionDefinition, Document: "terms.md", Version: 1, Issue: "ACME-1"})
}

// Before the person approves the root's terms.md, the list is the terms of the approved covered
// documents alone: a terms.md that waits for its approval adds nothing.
func TestBeforeTermsMdIsApprovedTheListIsTheApprovedRequests(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	uploadFormDocument(t, tree.handler, tree.root, "terms.md", termsMd(termLine("session", sessionDefinition)))
	change := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, change.PrimaryArtifactID)
	other := createFormIssue(t, tree.handler, tree.root, "Reads the list", formFaults[0].document(formFaults[0].clean))
	requireTerms(t, tree.handler, other.PrimaryArtifactID,
		specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: change.Key})
}

// The repair pass judges a withdrawn request against the list too: a request whose spec defines a
// term of the root's terms.md again is left resolved, with a reason that names the term and the
// document that defines it.
func TestTheRepairPassDoesNotOpenARequestWithASecondDefinition(t *testing.T) {
	f := newRepairFixtureFrom(t, 20*time.Millisecond, "ACME-1")
	tree := seedSpecFormTree(t, f.handler, f.database, f.deps)
	approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	withdrawn := withdrawnByAnOlderRelease(t, f, tree, secondSession.document(secondSession.broken))
	if repair := repairNow(t, f); repair.Restored != 0 || repair.LeftResolved != 1 {
		t.Fatalf("repair = %+v, want none restored and 1 left resolved", repair)
	}
	requireLeftResolved(t, f.handler, f.database, withdrawn.issue, withdrawn.seq, withdrawn.request, strings.Join(secondSession.lines(1), "\n"), withdrawn.reopener)
}

// A term that a closed issue defines is still in the list, and the refusal says to reopen that
// issue first: a closed issue takes no approval request, which is the control that shows the step
// is needed. Once reopened, its second version with a changed definition is opened.
func TestASecondDefinitionInAClosedIssueSaysToReopenIt(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	change := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, change.PrimaryArtifactID)
	closeIssue(t, tree.handler, change.Key)
	again := createFormIssue(t, tree.handler, tree.root, "Defines lease again", specWithTerms(termLine("lease", otherLease)))
	finding := definitionFinding("spec.md", again.PrimaryArtifactID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+change.Key))
	body := requireFormRefusal(t, "request", requestFormApproval(t, tree.handler, again.PrimaryArtifactID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
	if detail := body.Findings[0].Detail; !strings.HasSuffix(detail, "When that issue has the status done, reopen it first") {
		t.Fatalf("detail = %q, want it to end with the step to reopen the issue", detail)
	}
	if refused := requestFormApproval(t, tree.handler, change.PrimaryArtifactID, nil); refused.Code != http.StatusConflict ||
		!strings.Contains(refused.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("request of the closed issue's spec: status=%d body=%s, want 409 ISSUE_CLOSED", refused.Code, refused.Body.String())
	}
	if response := dispatchRequest(t, tree.handler, http.MethodPatch, "/api/v1/issues/"+change.Key, map[string]string{"status": "todo"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("reopen %s: status=%d body=%s", change.Key, response.Code, response.Body.String())
	}
	reviseDocument(t, tree.handler, change.PrimaryArtifactID, termLine("lease", leaseDefinition), termLine("lease", otherLease), 2)
	awaitSettled(t, tree.database, change.PrimaryArtifactID)
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, change.PrimaryArtifactID, nil))
}

// One request holds one definition of a term: a spec and its Verification document, requested
// together, that each define lease are refused at the second. The control: the spec alone is
// opened.
func TestARequestOfTwoDocumentsRefusesASecondDefinition(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	issue := createFormIssue(t, tree.handler, tree.root, "Defines lease twice", specWithTerms(termLine("lease", leaseDefinition)))
	verification := uploadFormDocument(t, tree.handler, issue.Key, "verification.md",
		"# Verification\n\n## Terms\n\n"+termList([]string{termLine("lease", otherLease)}))
	finding := definitionFinding("verification.md", verification.ID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+issue.Key))
	requireFormRefusal(t, "request of both", requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, map[string]any{"include_verification": true}),
		specform.ActRequest, []string{findingLine(finding)}, finding)
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil))
}

// termLineFinding is the finding of a line in the form of a term line outside Terms, in the
// document name whose id is artifactID, that names term, which where defines, with definition as
// its own text.
func termLineFinding(name, artifactID string, version int, term, definition, where string) specFormFinding {
	return specFormFinding{Document: name, ArtifactID: artifactID, Version: version, Finding: specform.Finding{
		Node: term, Kind: "term_line", Where: fmt.Sprintf("%s version %d", name, version),
		Rule: "term-second-definition", Excerpt: definition,
		Detail: where + " defines this term. This line has the form of a definition, but it is not under a heading Terms. " +
			"If it is no definition, change its form. If it is a definition, use the definition there, or change that definition and request approval of its document",
	}}
}

// A definition is a term line under Terms (R50). A line in that form under another heading is no
// definition, but a reader would take it for a second one when it names a defined term: with lease
// in the list, a request whose spec names lease under "## Glossary" is refused, and the finding
// tells the writer to change the form of the line or to use that definition. A document that a
// person approved with such a line adds no term to the list. The control: the same line with a
// name that nothing defines opens the request.
func TestARequestRefusesATermLineUnderAnotherHeadingThatNamesAListedTerm(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	listed := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
	glossary := createFormIssue(t, tree.handler, tree.root, "Names lease under Glossary", "# Spec\n\n## Glossary\n\n- "+termLine("lease", otherLease)+"\n")
	finding := termLineFinding("spec.md", glossary.PrimaryArtifactID, 1, "lease", otherLease, "spec.md version 1 of "+listed.Key)
	requireFormRefusal(t, "request of a spec with lease under Glossary", requestFormApproval(t, tree.handler, glossary.PrimaryArtifactID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
	plain := createFormIssue(t, tree.handler, tree.root, "Names valve under Glossary", "# Spec\n\n## Glossary\n\n- "+termLine("valve", "A part of a pump that opens and closes.")+"\n")
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, plain.PrimaryArtifactID, nil))
	approveFormDocument(t, tree.handler, plain.PrimaryArtifactID)
	if _, count := listedOf(t, tree.handler, listed.PrimaryArtifactID, "valve"); count != 0 {
		t.Fatalf("the list holds valve %d times after the person approved a spec that names it under Glossary, want 0", count)
	}
}

// A line in the form of a term line outside Terms is refused when another document of the same
// request defines its name under Terms, whichever of the two documents comes first: the spec and
// its Verification document, requested together. Two such lines of one name outside Terms, with
// no definition of it, are prose and refuse nothing. The control: the document with the line
// alone is opened.
func TestARequestRefusesALineOutsideTermsThatItsOtherDocumentDefines(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	leaseUnder := func(heading, definition string) string {
		return "## " + heading + "\n\n- " + termLine("lease", definition) + "\n"
	}
	for _, c := range []struct {
		name               string
		spec, verification string // the heading of the line of lease in each document
		refused            string // the document whose line is refused, "" for none
		definition, keeper string // the text of the refused line, and the document that defines lease
	}{
		{"the spec defines lease, and the Verification document names it under Notes", "Terms", "Notes", "verification.md", otherLease, "spec.md"},
		{"the spec names lease under Notes, and the Verification document defines it", "Notes", "Terms", "spec.md", leaseDefinition, "verification.md"},
		{"both name lease under another heading", "Glossary", "Notes", "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			issue := createFormIssue(t, tree.handler, tree.root, "Names lease in two documents: "+c.name, "# Spec\n\n"+leaseUnder(c.spec, leaseDefinition))
			verification := uploadFormDocument(t, tree.handler, issue.Key, "verification.md", "# Verification\n\n"+leaseUnder(c.verification, otherLease))
			both := requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, map[string]any{"include_verification": true})
			if c.refused == "" {
				requireFormRequestOpened(t, both)
				return
			}
			ids := map[string]string{"spec.md": issue.PrimaryArtifactID, "verification.md": verification.ID}
			finding := termLineFinding(c.refused, ids[c.refused], 1, "lease", c.definition, c.keeper+" version 1 of "+issue.Key)
			requireFormRefusal(t, "request of both", both, specform.ActRequest, []string{findingLine(finding)}, finding)
			requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil))
		})
	}
}

// The stated limit of a line outside Terms, which the order of two requests decides. A definition
// is a term line under Terms, and a line in that form elsewhere is no definition. Dispatch refuses
// such a line only when it names a defined term, and across two requests it cannot see the later
// one. So A, whose spec names gale under "## Glossary" while no document defines gale, is
// approved, and it adds no term. B, which defines gale under Terms, is approved after it. Then the
// read of A names B's definition, and A's next approval request is refused. A change of this order
// is a decision.
func TestALineOutsideTermsOfANameThatNothingDefinesIsApprovedAndRefusedAtItsNextRequest(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	const (
		cold = "A cold wind."
		warm = "A warm wind."
	)
	a := createFormIssue(t, tree.handler, tree.root, "Names gale under Glossary",
		"# Spec\n\n:::requirement{#store-comment}\nDispatch stores each comment.\n:::\n\n## Glossary\n\n- "+termLine("gale", cold)+"\n")
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, a.PrimaryArtifactID, nil))
	approveFormDocument(t, tree.handler, a.PrimaryArtifactID)
	if _, count := listedOf(t, tree.handler, a.PrimaryArtifactID, "gale"); count != 0 {
		t.Fatalf("the list holds gale %d times after the approval of A, want 0: a line outside Terms adds no term", count)
	}
	b := createFormIssue(t, tree.handler, tree.root, "Defines gale", specWithTerms(termLine("gale", warm)))
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, b.PrimaryArtifactID, nil))
	approveFormDocument(t, tree.handler, b.PrimaryArtifactID)
	want := specFormTerm{Term: "gale", Definition: warm, Document: "spec.md", Version: 1, Issue: b.Key}
	if got, count := listedOf(t, tree.handler, a.PrimaryArtifactID, "gale"); count != 1 || got != want {
		t.Fatalf("the list holds gale %d times, as %#v; want once, as %#v", count, got, want)
	}
	where := "spec.md version 1 of " + b.Key
	if read := specFormReadOf(t, tree.handler, a.PrimaryArtifactID); !reflect.DeepEqual(read.Findings, []specFormFinding{termLineFinding("spec.md", a.PrimaryArtifactID, 1, "gale", cold, where)}) {
		t.Fatalf("the read of A = findings %#v, want the line of gale, which %s defines", read.Findings, where)
	}
	reviseDocument(t, tree.handler, a.PrimaryArtifactID, "Dispatch stores each comment.", "Dispatch keeps each comment.", 2)
	awaitSettled(t, tree.database, a.PrimaryArtifactID)
	finding := termLineFinding("spec.md", a.PrimaryArtifactID, 2, "gale", cold, where)
	requireFormRefusal(t, "the next request of A", requestFormApproval(t, tree.handler, a.PrimaryArtifactID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
}

// Two requests that define one new term wait on the person at one time, and the person approves
// both at the same moment. At most one approval passes: the second definition is refused, as it is
// when the two approvals come one after the other (the control).
func TestTwoApprovalsAtOneTimeLeaveOneDefinitionOfANewTerm(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	open := func(term, definition string) formIssue {
		issue := createFormIssue(t, tree.handler, tree.root, "Defines "+term+": "+definition, specWithTerms(termLine(term, definition)))
		requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil))
		return issue
	}
	// The control: one approval after the other refuses the second.
	first, second := open("ledger", "The record of the work that one session did."), open("ledger", "The list of the costs that one project paid.")
	approveFormDocument(t, tree.handler, first.PrimaryArtifactID)
	if response := reviewFormDocument(t, tree.handler, second.PrimaryArtifactID, "approved", ""); response.Code != http.StatusConflict {
		t.Fatalf("control, the second approval after the first: status=%d body=%s, want 409 SPEC_FORM", response.Code, response.Body.String())
	}
	const pairs = 8
	both := 0
	for pair := range pairs {
		term := fmt.Sprintf("tally%d", pair)
		a, b := open(term, "The record of the work that one session did."), open(term, "The list of the costs that one project paid.")
		var wg sync.WaitGroup
		responses := make([]*httptest.ResponseRecorder, 2)
		for index, issue := range []formIssue{a, b} {
			wg.Go(func() {
				responses[index] = reviewFormDocument(t, tree.handler, issue.PrimaryArtifactID, "approved", "")
			})
		}
		wg.Wait()
		if responses[0].Code == http.StatusCreated && responses[1].Code == http.StatusCreated {
			both++
			_, count := listedOf(t, tree.handler, a.PrimaryArtifactID, term)
			t.Errorf("pair %d: both approvals passed (201, 201), and the list of terms holds %s %d times", pair, term, count)
		}
	}
	if both > 0 {
		t.Fatalf("%d of %d pairs approved two definitions of one term at one time", both, pairs)
	}
}

// An issue outside the root whose approved spec defines a term moves under the root, where the
// root's approved terms.md defines the same term. Dispatch takes the move, and the list then holds
// one definition of the term, terms.md's. The control: before the move, the list holds the same.
func TestAMoveUnderTheRootLeavesOneDefinitionOfATerm(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	outside := createFormIssue(t, tree.handler, tree.outside, "Defines session outside the root", specWithTerms(termLine("session", otherSession)))
	approveFormDocument(t, tree.handler, outside.PrimaryArtifactID)
	inside := createFormIssue(t, tree.handler, tree.root, "Reads the list", specWithTerms(termLine("lease", leaseDefinition)))
	want := specFormTerm{Term: "session", Definition: sessionDefinition, Document: "terms.md", Version: 1, Issue: tree.root}
	if got, count := listedOf(t, tree.handler, inside.PrimaryArtifactID, "session"); count != 1 || got != want {
		t.Fatalf("control, before the move: the list holds session %d times, as %#v; want once, as %#v", count, got, want)
	}
	reparentFormIssue(t, tree.handler, outside.Key, tree.root)
	if got, count := listedOf(t, tree.handler, inside.PrimaryArtifactID, "session"); count != 1 || got != want {
		t.Fatalf("after the move of %s under %s, the list holds session %d times, as %#v; want once, as %#v", outside.Key, tree.root, count, got, want)
	}
}

// The other two routes of a person's approval take the same turn: an approval from the Inbox and
// an approval of a named version at one time, each of a request that defines one new term, leave
// one definition of it. The second is refused, and so the list holds the term once.
func TestTwoApprovalsAtOneTimeOnTheInboxAndAVersionLeaveOneDefinition(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	const pairs = 8
	both := 0
	for pair := range pairs {
		term := fmt.Sprintf("tally%d", pair)
		a := createFormIssue(t, tree.handler, tree.root, "Defines "+term, specWithTerms(termLine(term, "The record of the work that one session did.")))
		ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, a.PrimaryArtifactID, nil)).Ask.ID
		b := createFormIssue(t, tree.handler, tree.root, "Defines "+term+" again", specWithTerms(termLine(term, "The list of the costs that one project paid.")))
		requireFormRequestOpened(t, requestFormApproval(t, tree.handler, b.PrimaryArtifactID, nil))
		answer := map[string]any{"selected": []string{docs.ApprovalOptionApprove}, "expected_edited_at": readBlockAsk(t, tree.handler, ask).EditedAt}
		var wg sync.WaitGroup
		var inbox, version *httptest.ResponseRecorder
		wg.Go(func() {
			inbox = dispatchRequest(t, tree.handler, http.MethodPost, "/api/v1/asks/"+ask+"/answer", answer, "alice")
		})
		wg.Go(func() { version = reviewFormVersion(t, tree.handler, b.PrimaryArtifactID, 1, "approved") })
		wg.Wait()
		if inbox.Code == http.StatusOK && version.Code == http.StatusCreated {
			both++
			_, count := listedOf(t, tree.handler, a.PrimaryArtifactID, term)
			t.Errorf("pair %d: both approvals passed (200, 201), and the list of terms holds %s %d times", pair, term, count)
			continue
		}
		if inbox.Code != http.StatusOK && version.Code != http.StatusCreated {
			t.Fatalf("pair %d: both approvals were refused: Inbox status=%d body=%s, version status=%d body=%s, want one passed",
				pair, inbox.Code, inbox.Body.String(), version.Code, version.Body.String())
		}
		for _, refused := range []*httptest.ResponseRecorder{inbox, version} {
			if refused.Code != http.StatusOK && refused.Code != http.StatusCreated && (refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"code":"SPEC_FORM"`)) {
				t.Fatalf("pair %d: the refused approval answered status=%d body=%s, want 409 SPEC_FORM", pair, refused.Code, refused.Body.String())
			}
		}
		if _, count := listedOf(t, tree.handler, a.PrimaryArtifactID, term); count != 1 {
			t.Fatalf("pair %d: the list holds %s %d times, want 1", pair, term, count)
		}
	}
	if both > 0 {
		t.Fatalf("%d of %d pairs approved two definitions of one term at one time", both, pairs)
	}
}

// The list holds one definition of each term, and the root's terms.md comes first: a spec that a
// person approved outside the root before terms.md, and that moves under the root, adds no second
// definition. Its own read names the definition that the list holds.
func TestTheRootsTermsMdComesFirstInTheList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	moved := createFormIssue(t, tree.handler, tree.outside, "Defines session first", specWithTerms(termLine("session", otherSession)))
	approveFormDocument(t, tree.handler, moved.PrimaryArtifactID)
	terms := approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	reparentFormIssue(t, tree.handler, moved.Key, tree.root)
	listed := specFormTerm{Term: "session", Definition: sessionDefinition, Document: "terms.md", Version: 1, Issue: "ACME-1"}
	requireTerms(t, tree.handler, terms.ID, listed)
	requireTerms(t, tree.handler, moved.PrimaryArtifactID, listed)
	want := []specFormFinding{definitionFinding("spec.md", moved.PrimaryArtifactID, 1, "session", otherSession, "term-second-definition",
		alreadyDefines("terms.md version 1 of ACME-1"))}
	if read := specFormReadOf(t, tree.handler, moved.PrimaryArtifactID); !reflect.DeepEqual(read.Findings, want) {
		t.Fatalf("the read of the moved spec = findings %#v, want %#v", read.Findings, want)
	}
}

// Of two documents under the root that define one term, the list holds the definition of the one
// that joined the list first, here at its approval under the root, before the other moved in, and
// keeps it when that document changes its definition. The read of the other document names that
// definition, and its next approval request is refused.
func TestTheListHoldsTheDefinitionThatJoinedFirst(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	first := createFormIssue(t, tree.handler, tree.root, "Defines lease first", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, first.PrimaryArtifactID)
	later := createFormIssue(t, tree.handler, tree.outside, "Defines lease later",
		specWithTerms(termLine("lease", otherLease))+"\n:::requirement{#store-comment}\nDispatch stores each comment.\n:::\n")
	approveFormDocument(t, tree.handler, later.PrimaryArtifactID)
	reparentFormIssue(t, tree.handler, later.Key, tree.root)
	requireTerms(t, tree.handler, first.PrimaryArtifactID, specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: first.Key})
	finding := definitionFinding("spec.md", later.PrimaryArtifactID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+first.Key))
	if read := specFormReadOf(t, tree.handler, later.PrimaryArtifactID); !reflect.DeepEqual(read.Findings, []specFormFinding{finding}) {
		t.Fatalf("the read of the later spec = findings %#v, want %#v", read.Findings, []specFormFinding{finding})
	}
	reviseDocument(t, tree.handler, later.PrimaryArtifactID, "Dispatch stores each comment.", "Dispatch keeps each comment.", 2)
	awaitSettled(t, tree.database, later.PrimaryArtifactID)
	finding = definitionFinding("spec.md", later.PrimaryArtifactID, 2, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+first.Key))
	requireFormRefusal(t, "request of the later spec", requestFormApproval(t, tree.handler, later.PrimaryArtifactID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
	const changedLease = "The time for which one session may hold one work unit."
	reviseDocument(t, tree.handler, first.PrimaryArtifactID, termLine("lease", leaseDefinition), termLine("lease", changedLease), 2)
	awaitSettled(t, tree.database, first.PrimaryArtifactID)
	ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, first.PrimaryArtifactID, nil)).Ask.ID
	if response := answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, ""); response.Code != http.StatusOK {
		t.Fatalf("approve the first spec's version 2: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	requireTerms(t, tree.handler, first.PrimaryArtifactID, specFormTerm{Term: "lease", Definition: changedLease, Document: "spec.md", Version: 2, Issue: first.Key})
}

// A request that names the document of a listed definition is judged against the first approved
// definition of the term in join order whose document the request does not name, or names at a
// version that still defines the term (R50). A's spec holds the listed definition of lease,
// and B, which the person approved outside the root, brought a second definition of lease when it
// moved in. One request names A's spec, whose new version drops lease, and A's Verification
// document, which adds it. After the approval B's definition would govern and the Verification
// document's would be a second one, so Dispatch refuses the request, and the person's approval
// when B moves in while the request waits, naming B's definition. The control: with no B under the
// root, the request opens, the approval passes, and the list holds the Verification document's
// definition.
func TestARequestThatDropsTheListedDefinitionIsJudgedAgainstTheNextOne(t *testing.T) {
	const (
		sirocco    = "The wind of one desert."
		thirdLease = "The hold that one session keeps on one work unit."
	)
	for _, c := range []string{"B is under the root at the request", "B moves in while the request waits", "control: no B"} {
		t.Run(c, func(t *testing.T) {
			tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
			a := createFormIssue(t, tree.handler, tree.root, "A defines lease first", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, tree.handler, a.PrimaryArtifactID)
			b := createFormIssue(t, tree.handler, tree.outside, "B defines lease outside the root", specWithTerms(termLine("lease", otherLease)))
			approveFormDocument(t, tree.handler, b.PrimaryArtifactID)
			if c == "B is under the root at the request" {
				reparentFormIssue(t, tree.handler, b.Key, tree.root)
			}
			reviseDocument(t, tree.handler, a.PrimaryArtifactID, termLine("lease", leaseDefinition), termLine("sirocco", sirocco), 2)
			awaitSettled(t, tree.database, a.PrimaryArtifactID)
			verification := uploadFormDocument(t, tree.handler, a.Key, "verification.md", "# Verification\n\n## Terms\n\n"+termList([]string{termLine("lease", thirdLease)}))
			finding := definitionFinding("verification.md", verification.ID, 1, "lease", thirdLease, "term-second-definition",
				alreadyDefines("spec.md version 1 of "+b.Key))
			request := requestFormApproval(t, tree.handler, a.PrimaryArtifactID, map[string]any{"include_verification": true})
			if c == "B is under the root at the request" {
				requireFormRefusal(t, "request of A's spec and Verification document", request, specform.ActRequest, []string{findingLine(finding)}, finding)
				return
			}
			ask := requireFormRequestOpened(t, request).Ask.ID
			if c == "B moves in while the request waits" {
				reparentFormIssue(t, tree.handler, b.Key, tree.root)
				requireFormRefusal(t, "approval of A's spec and Verification document", answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, ""),
					specform.ActApproval, []string{findingLine(finding)}, finding)
				return
			}
			if response := answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, ""); response.Code != http.StatusOK {
				t.Fatalf("control: approve A's spec and Verification document: status=%d body=%s, want 200", response.Code, response.Body.String())
			}
			want := specFormTerm{Term: "lease", Definition: thirdLease, Document: "verification.md", Version: 1, Issue: a.Key}
			if got, count := listedOf(t, tree.handler, a.PrimaryArtifactID, "lease"); count != 1 || got != want {
				t.Fatalf("control: the list holds lease %d times, as %#v; want once, as %#v", count, got, want)
			}
		})
	}
}

// The root's terms.md comes first, and the other definitions of its term keep their join order
// under it: when a request of terms.md drops the term, a line of it in the form of a term line
// outside Terms is judged against the definition that joined next. B1 was approved outside the
// root first, and B2 was approved under the root (through a server that names no root, as before
// the setting named it) before B1 moved in, so B2 joined first, though Dispatch created B1 first.
// The control: before terms.md drops the term, the list holds its definition.
func TestWhenTermsMdDropsATermTheDefinitionThatJoinedNextGoverns(t *testing.T) {
	const thirdSession = "The time that one agent spends on one task."
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	terms := approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	b1 := createFormIssue(t, tree.handler, tree.outside, "B1 defines session outside the root, created first", specWithTerms(termLine("session", otherSession)))
	approveFormDocument(t, tree.handler, b1.PrimaryArtifactID)
	b2 := createFormIssue(t, tree.handler, tree.root, "B2 defines session under the root", specWithTerms(termLine("session", thirdSession)))
	approveFormDocument(t, olderBuild(t, tree), b2.PrimaryArtifactID)
	reparentFormIssue(t, tree.handler, b1.Key, tree.root)
	if got, count := listedOf(t, tree.handler, terms.ID, "session"); count != 1 || got.Document != "terms.md" {
		t.Fatalf("control: the list holds session %d times, as %#v; want once, from terms.md", count, got)
	}
	uploadFormDocument(t, tree.handler, tree.root, "terms.md", termsMd(termLine("gale", "A strong wind."))+"\n# Notes\n\n- "+termLine("session", sessionDefinition)+"\n")
	awaitSettled(t, tree.database, terms.ID)
	finding := termLineFinding("terms.md", terms.ID, 2, "session", sessionDefinition, "spec.md version 1 of "+b2.Key)
	requireFormRefusal(t, "request of terms.md, which drops session", requestFormApproval(t, tree.handler, terms.ID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
}

// approveRevisionThrough replaces find with with in the document of issue, which makes its
// version number, and approves that version through handler.
func approveRevisionThrough(t *testing.T, tree specFormTree, handler http.Handler, issue formIssue, find, with string, number int) {
	t.Helper()
	reviseDocument(t, tree.handler, issue.PrimaryArtifactID, find, with, number)
	awaitSettled(t, tree.database, issue.PrimaryArtifactID)
	approveFormDocument(t, handler, issue.PrimaryArtifactID)
}

// R50: "Where approved documents still hold several definitions of one term, the root issue's own
// document of terms governs, else the definition that joined first." Each definition joins at its
// own date: the first approval of the earliest approved version of its document from which each
// later approved version holds the term, or the move that brought its issue under the root, when
// that came later. So a term that a later version added joins at the approval of that version, not
// at the first approval of its document. The approvals go through a server that names no root
// (olderBuild), as the approvals before the setting named the root did, since the gate lets no
// second definition through. The control: of two documents whose first versions both define the
// term, the one approved first keeps it, also when it changes its definition later.
func TestEachDefinitionJoinsTheListAtItsOwnApproval(t *testing.T) {
	const (
		sirocco    = "The wind of one desert."
		thirdLease = "The hold that one session keeps on one work unit."
	)
	type outcome struct {
		keeper  formIssue
		want    specFormTerm
		other   formIssue
		version int // the version of other that its read judges
	}
	for _, c := range []struct {
		name string
		run  func(t *testing.T, tree specFormTree, older http.Handler) outcome
	}{
		{"A adds lease in its second version, after B defined it", func(t *testing.T, tree specFormTree, older http.Handler) outcome {
			a := createFormIssue(t, tree.handler, tree.root, "A, approved first with no lease", specWithTerms(termLine("sirocco", sirocco)))
			approveFormDocument(t, older, a.PrimaryArtifactID)
			b := createFormIssue(t, tree.handler, tree.root, "B, approved second with lease", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, older, b.PrimaryArtifactID)
			approveRevisionThrough(t, tree, older, a, termLine("sirocco", sirocco), termLine("lease", otherLease), 2)
			return outcome{b, specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: b.Key}, a, 2}
		}},
		// A review's created_at is the start of its transaction, so an approval that committed
		// later can carry the earlier time. Here A's second approval carries a time before its
		// first one, and the order in which the events committed still decides.
		{"A adds lease in its second version, after B defined it, and that approval's time is before A's first", func(t *testing.T, tree specFormTree, older http.Handler) outcome {
			a := createFormIssue(t, tree.handler, tree.root, "A, approved first with no lease, its times reversed", specWithTerms(termLine("sirocco", sirocco)))
			approveFormDocument(t, older, a.PrimaryArtifactID)
			b := createFormIssue(t, tree.handler, tree.root, "B, approved second with lease", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, older, b.PrimaryArtifactID)
			approveRevisionThrough(t, tree, older, a, termLine("sirocco", sirocco), termLine("lease", otherLease), 2)
			if tag, err := tree.database.Pool.Exec(context.Background(), `
				update artifact_reviews set created_at = (select min(created_at) from artifact_reviews) - interval '1 hour'
				where artifact_id = $1 and version = 2 and state = 'approved'
			`, a.PrimaryArtifactID); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("give A's second approval the earliest time: rows=%d err=%v", tag.RowsAffected(), err)
			}
			return outcome{b, specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: b.Key}, a, 2}
		}},
		{"A removes lease in its second version and adds it again in its third, after B defined it", func(t *testing.T, tree specFormTree, older http.Handler) outcome {
			a := createFormIssue(t, tree.handler, tree.root, "A, approved first with lease", specWithTerms(termLine("lease", otherLease)))
			approveFormDocument(t, older, a.PrimaryArtifactID)
			approveRevisionThrough(t, tree, older, a, termLine("lease", otherLease), termLine("sirocco", sirocco), 2)
			b := createFormIssue(t, tree.handler, tree.root, "B, approved with lease while A holds none", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, older, b.PrimaryArtifactID)
			approveRevisionThrough(t, tree, older, a, termLine("sirocco", sirocco), termLine("lease", otherLease), 3)
			return outcome{b, specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: b.Key}, a, 3}
		}},
		{"control: A and B define lease in their first versions, and A changes its definition", func(t *testing.T, tree specFormTree, older http.Handler) outcome {
			a := createFormIssue(t, tree.handler, tree.root, "A, approved first with lease", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, older, a.PrimaryArtifactID)
			b := createFormIssue(t, tree.handler, tree.root, "B, approved second with lease", specWithTerms(termLine("lease", otherLease)))
			approveFormDocument(t, older, b.PrimaryArtifactID)
			approveRevisionThrough(t, tree, older, a, termLine("lease", leaseDefinition), termLine("lease", thirdLease), 2)
			return outcome{a, specFormTerm{Term: "lease", Definition: thirdLease, Document: "spec.md", Version: 2, Issue: a.Key}, b, 1}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
			got := c.run(t, tree, olderBuild(t, tree))
			if listed, count := listedOf(t, tree.handler, got.other.PrimaryArtifactID, "lease"); count != 1 || listed != got.want {
				t.Fatalf("the list holds lease %d times, as %#v; want once, as %#v (the definition that joined first)", count, listed, got.want)
			}
			if read := specFormReadOf(t, tree.handler, got.keeper.PrimaryArtifactID); len(read.Findings) != 0 {
				t.Errorf("the read of %s, which the list keeps lease from = findings %#v, want none", got.keeper.Key, read.Findings)
			}
			want := []specFormFinding{definitionFinding("spec.md", got.other.PrimaryArtifactID, got.version, "lease", otherLease, "term-second-definition",
				alreadyDefines(listedWhere(got.want.Document, got.want.Version, got.keeper.Key)))}
			if read := specFormReadOf(t, tree.handler, got.other.PrimaryArtifactID); !reflect.DeepEqual(read.Findings, want) {
				t.Errorf("the read of %s = findings %#v, want %#v", got.other.Key, read.Findings, want)
			}
		})
	}
}

// A spec and its Verification document that a person approved in one answer, outside the root,
// each define lease. The move under the root brings both in at one event, and the list holds the
// definition that the person approved first, the spec's, which the answer names first; the read
// of the Verification document names it.
func TestTwoDocumentsThatJoinAtOneMoveKeepTheOrderOfTheirApprovals(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	issue := createFormIssue(t, tree.handler, tree.outside, "Defines lease twice", specWithTerms(termLine("lease", leaseDefinition)))
	verification := uploadFormDocument(t, tree.handler, issue.Key, "verification.md",
		"# Verification\n\n## Terms\n\n"+termList([]string{termLine("lease", otherLease)}))
	ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, map[string]any{"include_verification": true})).Ask.ID
	if response := answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, ""); response.Code != http.StatusOK {
		t.Fatalf("approve the spec and its Verification document: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	reparentFormIssue(t, tree.handler, issue.Key, tree.root)
	requireTerms(t, tree.handler, issue.PrimaryArtifactID, specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: issue.Key})
	want := []specFormFinding{definitionFinding("verification.md", verification.ID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+issue.Key))}
	if read := specFormReadOf(t, tree.handler, verification.ID); !reflect.DeepEqual(read.Findings, want) {
		t.Fatalf("the read of the Verification document = findings %#v, want %#v", read.Findings, want)
	}
}

// driftDefinition is a definition that the line of termsLineFault holds in another form.
const driftDefinition = "The slow change of the clock of one machine."

// termsLineFault is a line under Terms in another form than a term line: the colon in the bold. Its
// finding names the line by the text that a person reads, with no marks of emphasis.
var termsLineFault = formFault{
	name: "a line under Terms that is no term line", node: `"drift: ` + driftDefinition + `"`, kind: specform.KindLine,
	clean: termLine("drift", driftDefinition), broken: "**drift:** " + driftDefinition, excerpt: "drift: " + driftDefinition,
	rule: "term-line", detail: `it is no term line, and a term line has the form "- **<term>**: <definition>". ` +
		`Move a line that defines no term, such as an example or a note, out from under the heading Terms. ` +
		`Put a decision block above the heading Terms, and after the answer, write the chosen definition under the heading`,
}

// A line under Terms that is no term line breaks the form: in a spec, on each of the six paths to
// a person's approval, and in the root's terms.md. The finding names the line and shows the form
// of a term line.
func TestEveryApprovalRouteRefusesALineUnderTermsThatIsNoTermLine(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	requireEveryApprovalRouteRefuses(t, tree, termsLineFault)
	terms := uploadFormDocument(t, tree.handler, tree.root, "terms.md", termsMd(termLine("session", sessionDefinition), termsLineFault.broken))
	finding := specFormFinding{
		Document: "terms.md", ArtifactID: terms.ID, Version: 1,
		Finding: specform.Finding{
			Node: termsLineFault.node, Kind: termsLineFault.kind, Where: "terms.md version 1",
			Rule: specform.Rule(termsLineFault.rule), Detail: termsLineFault.detail, Excerpt: termsLineFault.excerpt,
		},
	}
	requireFormRefusal(t, "request of terms.md", requestFormApproval(t, tree.handler, terms.ID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
}

// listedOf is the one definition of term that the read of artifactID lists, and how many it lists.
func listedOf(t *testing.T, handler http.Handler, artifactID, term string) (specFormTerm, int) {
	t.Helper()
	var found specFormTerm
	count := 0
	for _, listed := range specFormReadOf(t, handler, artifactID).Terms {
		if listed.Term == term {
			found = listed
			count++
		}
	}
	return found, count
}

// v-terms: "A change to a definition waits for the approval of the person." A move under the root
// changes no definition that the list holds. The list holds the definition of lease that the
// person approved under the root; then an issue that the person approved earlier, outside the
// root, with another definition of lease, moves under the root. Dispatch takes the move, and the
// list keeps the definition it held. The control: before the move, the list holds the definition
// approved under the root.
func TestAMoveUnderTheRootChangesNoDefinitionOfTheList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	earlier := createFormIssue(t, tree.handler, tree.outside, "Defines lease outside the root, first", specWithTerms(termLine("lease", otherLease)))
	approveFormDocument(t, tree.handler, earlier.PrimaryArtifactID)
	listed := createFormIssue(t, tree.handler, tree.root, "Defines lease under the root", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
	want := specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: listed.Key}
	if got, count := listedOf(t, tree.handler, listed.PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("control, before the move: the list holds lease %d times, as %#v; want once, as %#v", count, got, want)
	}
	reparentFormIssue(t, tree.handler, earlier.Key, tree.root)
	if got, count := listedOf(t, tree.handler, listed.PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("after the move of %s under %s, the list holds lease %d times, as %#v; want the definition it held, %#v", earlier.Key, tree.root, count, got, want)
	}
}

// The same with two moves: of two issues approved outside the root that define one new term, the
// one approved later moves in first, and the list holds its definition; then the one approved
// first moves in. The second move changes no definition that the list holds.
func TestTheSecondMoveUnderTheRootChangesNoDefinitionOfTheList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	first := createFormIssue(t, tree.handler, tree.outside, "Defines lease, approved first", specWithTerms(termLine("lease", otherLease)))
	approveFormDocument(t, tree.handler, first.PrimaryArtifactID)
	later := createFormIssue(t, tree.handler, tree.outside, "Defines lease, approved later", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, later.PrimaryArtifactID)
	reparentFormIssue(t, tree.handler, later.Key, tree.root)
	want := specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: later.Key}
	if got, count := listedOf(t, tree.handler, later.PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("control, after the first move: the list holds lease %d times, as %#v; want once, as %#v", count, got, want)
	}
	reparentFormIssue(t, tree.handler, first.Key, tree.root)
	if got, count := listedOf(t, tree.handler, later.PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("after the move of %s under %s, the list holds lease %d times, as %#v; want the definition it held, %#v", first.Key, tree.root, count, got, want)
	}
}

// An approval and a move at one time: an issue under the root waits on the person with a new
// term, and an issue that the person approved outside the root, with another definition of that
// term, moves in at the same moment. The list holds the term once. When the person's approval
// passed, the list holds the definition that the person approved; when it was refused, the
// definition that moved in first.
func TestAnApprovalAndAMoveAtOneTimeLeaveTheApprovedDefinition(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	const rounds = 8
	displaced := 0
	for round := range rounds {
		term := fmt.Sprintf("orbit%d", round)
		outside := createFormIssue(t, tree.handler, tree.outside, "Defines "+term+" outside the root", specWithTerms(termLine(term, "The path of one moon around one planet.")))
		approveFormDocument(t, tree.handler, outside.PrimaryArtifactID)
		inside := createFormIssue(t, tree.handler, tree.root, "Defines "+term+" under the root", specWithTerms(termLine(term, "The route that one agent takes through its work.")))
		requireFormRequestOpened(t, requestFormApproval(t, tree.handler, inside.PrimaryArtifactID, nil))
		var wg sync.WaitGroup
		var approved, moved *httptest.ResponseRecorder
		wg.Go(func() { approved = reviewFormDocument(t, tree.handler, inside.PrimaryArtifactID, "approved", "") })
		wg.Go(func() {
			moved = dispatchRequest(t, tree.handler, http.MethodPatch, "/api/v1/issues/"+outside.Key, map[string]any{"parent": tree.root}, "alice")
		})
		wg.Wait()
		if moved.Code != http.StatusOK {
			t.Fatalf("round %d: move %s under %s: status=%d body=%s, want 200", round, outside.Key, tree.root, moved.Code, moved.Body.String())
		}
		listed, count := listedOf(t, tree.handler, inside.PrimaryArtifactID, term)
		if count != 1 {
			t.Fatalf("round %d: the list holds %s %d times, want 1", round, term, count)
		}
		switch {
		case approved.Code == http.StatusCreated && listed.Issue != inside.Key:
			displaced++
			t.Errorf("round %d: the approval of %s passed (201), and the list holds %s from %s, which moved in at the same time", round, inside.Key, term, listed.Issue)
		case approved.Code == http.StatusConflict && strings.Contains(approved.Body.String(), `"code":"SPEC_FORM"`) && listed.Issue != outside.Key:
			t.Errorf("round %d: the approval of %s was refused, and the list holds %s from %s, want %s", round, inside.Key, term, listed.Issue, outside.Key)
		case approved.Code != http.StatusCreated && approved.Code != http.StatusConflict:
			t.Fatalf("round %d: approve %s: status=%d body=%s, want 201 or 409 SPEC_FORM", round, inside.Key, approved.Code, approved.Body.String())
		}
	}
	if displaced > 0 {
		t.Fatalf("%d of %d rounds took an approved definition away with a move", displaced, rounds)
	}
}

// A move inside the root is no arrival: the issue that the list keeps a term from moves from one
// level under the root to another, and the list keeps its definition, though a definition that
// arrived after it waits under the root.
func TestAMoveInsideTheRootChangesNoDefinitionOfTheList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	first := createFormIssue(t, tree.handler, tree.root, "The first level", "# Level\n\nThe first level.\n")
	second := createFormIssue(t, tree.handler, tree.root, "The second level", "# Level\n\nThe second level.\n")
	keeper := createFormIssue(t, tree.handler, first.Key, "Defines lease under the first level", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, keeper.PrimaryArtifactID)
	arrived := createFormIssue(t, tree.handler, tree.outside, "Defines lease outside the root", specWithTerms(termLine("lease", otherLease)))
	approveFormDocument(t, tree.handler, arrived.PrimaryArtifactID)
	reparentFormIssue(t, tree.handler, arrived.Key, tree.root)
	want := specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: keeper.Key}
	if got, count := listedOf(t, tree.handler, keeper.PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("control, before the move inside the root: the list holds lease %d times, as %#v; want once, as %#v", count, got, want)
	}
	reparentFormIssue(t, tree.handler, keeper.Key, second.Key)
	if got, count := listedOf(t, tree.handler, keeper.PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("after the move of %s from %s to %s, the list holds lease %d times, as %#v; want %#v", keeper.Key, first.Key, second.Key, count, got, want)
	}
}

// A move changes the tree and no text. When the issue that the list keeps a term from moves out
// of the root, the list holds the definition that joined it next, which the person approved too:
// of two definitions that moved in after the keeper, the one that moved in first. Its own read
// then names no finding, and the read of the last one names it.
func TestWhenTheKeeperMovesOutTheListHoldsTheDefinitionThatJoinedNext(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	keeper := createFormIssue(t, tree.handler, tree.root, "Defines lease under the root", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, keeper.PrimaryArtifactID)
	const thirdLease = "The hold that one session keeps on one work unit."
	var arrivals []formIssue
	for _, definition := range []string{otherLease, thirdLease} {
		arrived := createFormIssue(t, tree.handler, tree.outside, "Defines lease outside the root: "+definition, specWithTerms(termLine("lease", definition)))
		approveFormDocument(t, tree.handler, arrived.PrimaryArtifactID)
		arrivals = append(arrivals, arrived)
	}
	// The one approved last moves in first.
	reparentFormIssue(t, tree.handler, arrivals[1].Key, tree.root)
	reparentFormIssue(t, tree.handler, arrivals[0].Key, tree.root)
	if got, count := listedOf(t, tree.handler, keeper.PrimaryArtifactID, "lease"); count != 1 || got.Issue != keeper.Key {
		t.Fatalf("control, before the keeper moves out: the list holds lease %d times, as %#v; want once, from %s", count, got, keeper.Key)
	}
	reparentFormIssue(t, tree.handler, keeper.Key, tree.outside)
	want := specFormTerm{Term: "lease", Definition: thirdLease, Document: "spec.md", Version: 1, Issue: arrivals[1].Key}
	if got, count := listedOf(t, tree.handler, arrivals[0].PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("after the keeper moved out, the list holds lease %d times, as %#v; want the definition that joined next, %#v", count, got, want)
	}
	if read := specFormReadOf(t, tree.handler, arrivals[1].PrimaryArtifactID); len(read.Findings) != 0 {
		t.Fatalf("the read of %s, which the list now keeps lease from = findings %#v, want none", arrivals[1].Key, read.Findings)
	}
	finding := definitionFinding("spec.md", arrivals[0].PrimaryArtifactID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+arrivals[1].Key))
	if read := specFormReadOf(t, tree.handler, arrivals[0].PrimaryArtifactID); !reflect.DeepEqual(read.Findings, []specFormFinding{finding}) {
		t.Fatalf("the read of %s = findings %#v, want %#v", arrivals[0].Key, read.Findings, []specFormFinding{finding})
	}
}

// olderBuild is a server of this release on tree's database whose setting names no root of The
// Spec. To the list of terms it does what the release before this one does with a move: it
// writes the issue and its events, takes no lock of a list and writes nothing else, since it
// knows no root.
func olderBuild(t *testing.T, tree specFormTree) http.Handler {
	t.Helper()
	handler, _, _ := newTestServer(t, testServerOptions{store: tree.database})
	return handler
}

// A move that a build with no root of The Spec made changes no definition of the list. Such a
// build writes the issue and its events and nothing more, as the release before this one does, so
// its move is made here through a server that names no root (olderBuild). v-terms: "A change to a
// definition waits for the approval of the person." The control: the same move through the server
// that names the root.
func TestAMoveByABuildThatNamesNoRootChangesNoDefinitionOfTheList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	older := olderBuild(t, tree)
	for _, c := range []struct {
		term  string
		mover http.Handler
		who   string
	}{
		{"lease", tree.handler, "control, the server that names the root"},
		{"sigil", older, "a server that names no root, as the release before this one"},
	} {
		earlier := createFormIssue(t, tree.handler, tree.outside, "Defines "+c.term+" outside the root, first", specWithTerms(termLine(c.term, otherLease)))
		approveFormDocument(t, tree.handler, earlier.PrimaryArtifactID)
		listed := createFormIssue(t, tree.handler, tree.root, "Defines "+c.term+" under the root", specWithTerms(termLine(c.term, leaseDefinition)))
		approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
		reparentFormIssue(t, c.mover, earlier.Key, tree.root)
		want := specFormTerm{Term: c.term, Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: listed.Key}
		if got, count := listedOf(t, tree.handler, listed.PrimaryArtifactID, c.term); count != 1 || got != want {
			t.Errorf("%s: after it moved %s under %s, the list holds %s %d times, as %#v; want the definition it held, %#v",
				c.who, earlier.Key, tree.root, c.term, count, got, want)
		}
		finding := definitionFinding("spec.md", earlier.PrimaryArtifactID, 1, c.term, otherLease, "term-second-definition", alreadyDefines("spec.md version 1 of "+listed.Key))
		if read := specFormReadOf(t, tree.handler, earlier.PrimaryArtifactID); !reflect.DeepEqual(read.Findings, []specFormFinding{finding}) {
			t.Errorf("%s: the read of %s = findings %#v, want %#v", c.who, earlier.Key, read.Findings, []specFormFinding{finding})
		}
	}
}

// The order of the list never depends on which build handled a move: each case of moves gives one
// list whether the server that names the root made the moves or a server that names none
// (olderBuild), as the release before this one and a rollback to it do. Each case also names the
// document that the list keeps its term from, by the order the decision gives: a definition joins
// the list at the later of its approval and the move that brought it under the root.
func TestTheListIsTheSameWhicheverBuildMadeTheMoves(t *testing.T) {
	type moves func(tree specFormTree, move func(issue, parent string)) (keeper string, read string)
	cases := []struct {
		name  string
		moves moves
	}{
		{"a document approved outside first moves in", func(tree specFormTree, move func(string, string)) (string, string) {
			earlier := createFormIssue(t, tree.handler, tree.outside, "Defines lease outside, first", specWithTerms(termLine("lease", otherLease)))
			approveFormDocument(t, tree.handler, earlier.PrimaryArtifactID)
			listed := createFormIssue(t, tree.handler, tree.root, "Defines lease under the root", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
			move(earlier.Key, tree.root)
			return listed.Key, listed.PrimaryArtifactID
		}},
		{"the keeper moves inside the root after a later one moved in", func(tree specFormTree, move func(string, string)) (string, string) {
			first := createFormIssue(t, tree.handler, tree.root, "The first level", "# Level\n\nThe first level.\n")
			second := createFormIssue(t, tree.handler, tree.root, "The second level", "# Level\n\nThe second level.\n")
			earlier := createFormIssue(t, tree.handler, tree.outside, "Defines lease outside, first", specWithTerms(termLine("lease", otherLease)))
			approveFormDocument(t, tree.handler, earlier.PrimaryArtifactID)
			keeper := createFormIssue(t, tree.handler, first.Key, "Defines lease under the first level", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, tree.handler, keeper.PrimaryArtifactID)
			move(earlier.Key, tree.root)
			move(keeper.Key, second.Key)
			return keeper.Key, keeper.PrimaryArtifactID
		}},
		{"the keeper moves out and in again", func(tree specFormTree, move func(string, string)) (string, string) {
			keeper := createFormIssue(t, tree.handler, tree.root, "Defines lease under the root, first", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, tree.handler, keeper.PrimaryArtifactID)
			later := createFormIssue(t, tree.handler, tree.outside, "Defines lease outside, later", specWithTerms(termLine("lease", otherLease)))
			approveFormDocument(t, tree.handler, later.PrimaryArtifactID)
			move(later.Key, tree.root)
			move(keeper.Key, tree.outside)
			move(keeper.Key, tree.root)
			return later.Key, later.PrimaryArtifactID
		}},
		{"a parent outside the root moves in with its child", func(tree specFormTree, move func(string, string)) (string, string) {
			parent := createFormIssue(t, tree.handler, tree.outside, "A parent outside the root", "# Parent\n\nSome other work.\n")
			child := createFormIssue(t, tree.handler, parent.Key, "Defines lease under that parent, first", specWithTerms(termLine("lease", otherLease)))
			approveFormDocument(t, tree.handler, child.PrimaryArtifactID)
			listed := createFormIssue(t, tree.handler, tree.root, "Defines lease under the root", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
			move(parent.Key, tree.root)
			return listed.Key, listed.PrimaryArtifactID
		}},
	}
	for _, c := range cases {
		lists := map[string][]specFormTerm{}
		for _, build := range []string{"the server that names the root", "a server that names no root"} {
			tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
			mover := tree.handler
			if build == "a server that names no root" {
				mover = olderBuild(t, tree)
			}
			keeper, read := c.moves(tree, func(issue, parent string) { reparentFormIssue(t, mover, issue, parent) })
			if got, count := listedOf(t, tree.handler, read, "lease"); count != 1 || got.Issue != keeper {
				t.Errorf("%s, moves by %s: the list holds lease %d times, as %#v; want once, from %s", c.name, build, count, got, keeper)
			}
			lists[build] = specFormReadOf(t, tree.handler, read).Terms
		}
		if !reflect.DeepEqual(lists["the server that names the root"], lists["a server that names no root"]) {
			t.Errorf("%s: the list is %#v after moves by the server that names the root, and %#v after moves by a server that names none",
				c.name, lists["the server that names the root"], lists["a server that names no root"])
		}
	}
}

// The read of a document names its own unreadable approved version in unread. R50: "The Spec holds
// a list of defined terms." X under the root defines lease and is approved at version 1; the agent
// writes version 2, which nobody has approved yet; then X's approved version 1 is made unreadable,
// so the list of the root lacks lease. The read of X's version 2 answers that list and names X's
// version 1 in unread, so a client can say that the list can lack terms. The control: the read of
// another document (Y) names it too.
func TestTheReadOfADocumentNamesItsOwnUnreadableApprovedVersion(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	x := createFormIssue(t, tree.handler, tree.root, "X", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, x.PrimaryArtifactID)
	y := createFormIssue(t, tree.handler, tree.root, "Y", specWithTerms(termLine("gale", "A strong wind of one coast.")))
	reviseDocument(t, tree.handler, x.PrimaryArtifactID, termLine("lease", leaseDefinition), termLine("lease", otherLease), 2)
	awaitSettled(t, tree.database, x.PrimaryArtifactID, y.PrimaryArtifactID)
	if read := specFormReadOf(t, tree.handler, y.PrimaryArtifactID); !slices.ContainsFunc(read.Terms, func(term specFormTerm) bool { return term.Term == "lease" }) {
		t.Fatalf("setup: the list of the read of Y = %#v, want lease of %s", read.Terms, x.Key)
	}
	storeUnreadableVersion(t, tree.database, x.PrimaryArtifactID, 1)
	line := specform.UnreadLine("spec.md", 1, x.Key)
	names := func(read specFormRead) bool {
		return slices.ContainsFunc(read.Unread, func(unread specFormUnread) bool { return unread.Line == line })
	}

	if read := specFormReadOf(t, tree.handler, y.PrimaryArtifactID); !names(read) {
		t.Fatalf("control: the read of Y has unread %#v, want the line %q", read.Unread, line)
	}
	read := specFormReadOf(t, tree.handler, x.PrimaryArtifactID)
	lacks := !slices.ContainsFunc(read.Terms, func(term specFormTerm) bool { return term.Term == "lease" })
	if read.Version != 2 || !lacks {
		t.Fatalf("setup: the read of X is version %d with the list %#v, want version 2 and a list that lacks lease", read.Version, read.Terms)
	}
	if !names(read) {
		t.Errorf("the read of X's version 2 answers a list of %d terms that lacks lease, and unread %#v; want the line %q, so that the list says it can lack terms",
			len(read.Terms), read.Unread, line)
	}
}

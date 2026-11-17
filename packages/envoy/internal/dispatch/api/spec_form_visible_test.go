package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// formRequirement is a requirement block of id that holds body.
func formRequirement(id, body string) string {
	return ":::requirement{#" + id + "}\n" + body + "\n:::\n"
}

// The red team's and the review's inputs of a link, through the approval request route: the
// rules of a sentence and of a name judge the words that the person reads, and the reference
// rules also read each address and title. A footnote of a statement is part of it.
func TestTheRequestJudgesTheWordsAndTheAddressesOfALink(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	requireFormRows(t, tree, []formRow{
		{name: "a stop in the address of a link", spec: formRequirement("dot", "Dispatch opens [the record](https://example.com/Web.Config)."),
			reads: specform.KindStatement},
		{name: "a stop in another address", spec: formRequirement("type", "Dispatch reads [the type](https://learn.example.com/api/System.String)."),
			reads: specform.KindStatement},
		{name: "a number sign in an address", spec: formRequirement("guide", "Dispatch follows [the guide](https://example.com/guide#12)."),
			reads: specform.KindStatement},
		{name: "a name of a person split by a link", spec: formRequirement("name", "Dispatch asks [Ada](https://example.com/ada) Lovelace."),
			want: []string{"statement name person"}},
		{name: "a name of a person in two links", spec: formRequirement("links", "Dispatch asks [Ada](https://example.com/ada) [Lovelace](https://example.com/lovelace)."),
			want: []string{"statement links person"}},
		{name: "the title of an image", spec: formRequirement("image-title", "Dispatch shows ![the record](https://example.com/record.png \"Ada Lovelace\")."),
			want: []string{"statement image-title person"}},
		{name: "the title of a link", spec: formRequirement("link-title", "Dispatch shows [the record](https://example.com/record \"Ada Lovelace\")."),
			want: []string{"statement link-title person"}},
		{name: "a footnote that links to an issue",
			spec: formRequirement("footnote", "Dispatch stores each record.[^note]") + "\n[^note]: See [the issue](dispatch://ACME-12).\n",
			want: []string{"statement footnote dispatch-link"}},
		{name: "the control: the address of a link to an issue", spec: formRequirement("issue", "Dispatch keeps [the issue](dispatch://ACME-12)."),
			want: []string{"statement issue dispatch-link"}},
	})
}

// The red team's two inputs of a part of Terms and a container, through the approval request
// route: a heading in a block quote does not close the part around it, and a part in a block quote
// or a callout ends with it.
func TestTheRequestReadsAPartOfTermsInItsContainer(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	requireFormRows(t, tree, []formRow{
		{name: "a heading in a block quote in the part",
			spec: "## Terms\n\n- **lease**: One record of a claim.\n\n> ## Example\n>\n> A note.\n\n- **bad**: Dispatch performs a validation of each upload.\n",
			want: []string{"definition bad ste100-nominalization"}},
		{name: "the part in a block quote, then a paragraph",
			spec:  "> ## Terms\n>\n> - **lease**: One record of a claim.\n\nDispatch stores each record.\n",
			reads: specform.KindDefinition},
		{name: "the part in a callout, then a paragraph",
			spec:  ":::callout{#terms-note title=\"Terms\"}\n## Terms\n\n- **lease**: One record of a claim.\n:::\n\nDispatch stores each record.\n",
			reads: specform.KindDefinition},
	})
}

// Other prose of a document is no text of The Spec: three of the red team's inputs open the
// request, beside a statement that the form reads.
func TestTheRequestJudgesNoOtherProse(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	const control = ":::requirement{#control}\nDispatch stores each comment.\n:::\n\n"
	requireFormRows(t, tree, []formRow{
		{name: "an ordinary paragraph with a link to an issue", spec: control + "See [the issue](dispatch://ACME-12).\n", reads: specform.KindStatement},
		{name: "an ordinary heading with the name of a person", spec: control + "## Ada Lovelace\n", reads: specform.KindStatement},
		{name: "the title of a callout with the name of a person",
			spec: control + ":::callout{#note title=\"Ada Lovelace\"}\nDispatch stores each record.\n:::\n", reads: specform.KindStatement},
	})
}

// The red team's inputs of the page's words, through the approval request route: a name or the
// address of a record that code font splits is refused as the page shows it, and so is one wholly
// in code font; a definition wholly in its footnote opens; an address that the words show is one
// word of a sentence; the address of a pull request whose host is in capitals is refused.
func TestTheRequestJudgesTheTextThatThePageShows(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	requireFormRows(t, tree, []formRow{
		{name: "one part of a name in code font", spec: formRequirement("code-one", "Dispatch asks `Ada` Lovelace."),
			want: []string{"statement code-one person"}},
		{name: "both parts of a name in code font", spec: formRequirement("code-both", "Dispatch asks `Ada` `Lovelace`."),
			want: []string{"statement code-both person"}},
		{name: "the middle of a name in code font", spec: formRequirement("code-middle", "Dispatch asks A`da Love`lace."),
			want: []string{"statement code-middle person"}},
		{name: "the address of a record split by code font", spec: formRequirement("code-record", "Dispatch keeps `dispatch://AC`ME-12."),
			want: []string{"statement code-record dispatch-link"}},
		{name: "a name wholly in code font", spec: formRequirement("code-name", "Dispatch asks `Ada Lovelace`."),
			want: []string{"statement code-name person"}},
		{name: "a definition wholly in its footnote", spec: "## Terms\n\n- **lease**: [^a]\n\n[^a]: One record of a claim.\n",
			reads: specform.KindDefinition},
		{name: "an address in a sentence", spec: formRequirement("url", "Dispatch opens https://example.com/Web.Config."),
			reads: specform.KindStatement},
		{name: "an address at the end of a sentence", spec: formRequirement("url-end", "Dispatch opens https://example.com/Web.Config. Dispatch shows it."),
			reads: specform.KindStatement},
		{name: "an address in brackets", spec: formRequirement("url-brackets", "Dispatch opens the record (https://example.com/Web.Config)."),
			reads: specform.KindStatement},
		{name: "an address with a query string", spec: formRequirement("url-query", "Dispatch opens https://example.com/Web.Config?view=Raw.Text."),
			reads: specform.KindStatement},
		{name: "the address of a pull request whose host is in capitals", spec: formRequirement("pr", "Dispatch shows [the change](https://GITHUB.COM/acme/repo/pull/12)."),
			want: []string{"statement pr pull-request"}},
		{name: "the control: a host that ends in github.com", spec: formRequirement("other-host", "Dispatch shows [the change](https://notgithub.com/acme/repo/pull/12)."),
			reads: specform.KindStatement},
	})
}

// The red team's inputs of a form inside a longer word, through the approval request route: a
// colour, the scope of a package, an SSH address and a person's name joined to a digit open, and
// the number of a pull request, a handle, an email address and a person's name alone are still
// refused, also with a letter of another script in them. A bare id names a record, and the
// refusal says so.
func TestTheRequestReadsEachFormOfANameAsAWholeWord(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	requireFormRows(t, tree, []formRow{
		{name: "a colour of six digits and letters", spec: formRequirement("colour", "Dispatch stores the color `#12abcd`."),
			reads: specform.KindStatement},
		{name: "the scope of a package", spec: formRequirement("npm", "Dispatch installs `@types/node`."),
			reads: specform.KindStatement},
		{name: "the SSH address of a repository", spec: formRequirement("ssh", "Legion clones `git@github.com:owner/repo.git` for each run."),
			reads: specform.KindStatement},
		{name: "the control: the number of a pull request", spec: formRequirement("number", "Legion merges each change, as `#12` says."),
			want: []string{"statement number pull-request"}},
		{name: "the control: a handle", spec: formRequirement("handle", "Dispatch asks `@legion` about each change."),
			want: []string{"statement handle person"}},
		{name: "an email address with a letter of another script", spec: formRequirement("accent-mail", "Dispatch sends each summary to `josé.garcia@example.com`."),
			want: []string{"statement accent-mail person"}},
		{name: "a handle with a letter of another script", spec: formRequirement("accent-handle", "Dispatch asks @josé about each change."),
			want: []string{"statement accent-handle person"}},
		{name: "the control: an email address and then a colon", spec: formRequirement("mail-colon", "Dispatch mails one person: ada@example.com: she approves each change."),
			want: []string{"statement mail-colon person"}},
		{name: "a name of a person joined to a digit", spec: formRequirement("name-digit", "Dispatch asks Ada Lovelace2 about each change."),
			reads: specform.KindStatement},
		{name: "the control: a name of a person", spec: formRequirement("name", "Dispatch asks Ada Lovelace about each change."),
			want: []string{"statement name person"}},
	})
	const id = "12345678-1234-1234-1234-123456789abc"
	issue := createFormIssue(t, tree.handler, tree.root, "A bare id", "# Spec\n\n"+formRequirement("id", "Dispatch stores the value `"+id+"`."))
	response := requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)
	const line = `- statement id in spec.md version 1: it names a record of Dispatch by its id: "` + id + `".`
	if body := response.Body.String(); response.Code != http.StatusConflict || !strings.Contains(body, strings.ReplaceAll(line, `"`, `\"`)) {
		t.Errorf("a bare id: status=%d body=%s, want 409 with the line %q", response.Code, body, line)
	}
}

// A line under the heading Terms that is no term line is refused, and the refusal names both ways
// out: a line that defines no term, such as an example or a note, moves out from under the heading,
// and a decision block goes above it. The decision block alone is no way out for a line of prose or
// for an example below a term.
func TestALineUnderTermsIsRefusedWithBothWaysOut(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	const moveOut = "Move a line that defines no term, such as an example or a note, out from under the heading Terms."
	const decision = "Put a decision block above the heading Terms, and after the answer, write the chosen definition under the heading."
	for _, test := range []struct{ name, spec, node string }{
		{"a line of prose", "## Terms\n\nEach term has one definition.\n\n- **lease**: One record of a claim.\n", `"Each term has one definition."`},
		{"an example below a term", "## Terms\n\n- **lease**: One record of a claim.\n  - For example, one lease of a work unit.\n",
			`"For example, one lease of a work unit."`},
	} {
		issue := createFormIssue(t, tree.handler, tree.root, test.name, "# Spec\n\n"+test.spec)
		response := requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)
		body := decodeBody[map[string]any](t, response)
		message, _ := body["error"].(string)
		if response.Code != http.StatusConflict || !strings.Contains(message, "- line "+test.node+" in spec.md version 1:") ||
			!strings.Contains(message, moveOut) || !strings.Contains(message, decision) {
			t.Errorf("%s: status=%d error=%q, want 409 naming the line %s and both ways out", test.name, response.Code, message, test.node)
		}
	}
}

// unreadableMarkdown is markdown that today's parser refuses, as a version that an earlier release
// stored outside the schema.
const unreadableMarkdown = "<div>\nblock HTML\n</div>\n"

// storeUnreadableVersion writes unreadableMarkdown over version of the document by SQL. A browser
// edit can store such a version too (TestABrowserEditCanStoreAVersionThatDispatchCannotRead), and
// SQL puts one at any version with no document socket.
func storeUnreadableVersion(t *testing.T, database *store.Store, artifactID string, version int) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update artifact_versions set markdown = $3 where artifact_id = $1 and number = $2`,
		artifactID, version, unreadableMarkdown); err != nil {
		t.Fatalf("store an unreadable version: %v", err)
	}
}

// A browser edit through the document socket can store a version that the parser refuses:
// settlement stores the editor's tree as it renders it, and reads nothing back. Here the editor
// moves the text of a callout in a list item into a footnote definition in the callout. The
// request and the person's approval of that version are refused with the finding unreadable,
// which names the version and the way out, and the read route says that the approval is refused.
func TestABrowserEditCanStoreAVersionThatDispatchCannotRead(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{specRoots: "ACME-1", documentSocket: true})
	tree := seedSpecFormTree(t, handler, database, deps)
	issue := createFormIssue(t, tree.handler, tree.root, "A spec that a browser edits", "# Spec\n\n"+
		formRequirement("clean", "Dispatch shows each record.")+"\n- Lead.\n\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  Gone.\n  :::\n")
	peer := connectBrowserPeer(t, deps.Docs.(*docs.Service), issue.PrimaryArtifactID)
	peer.edit(t, func(tree *pmdoc.Node) error {
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			if node.Type != "callout" {
				return true
			}
			node.Children = []*pmdoc.Node{{Type: "footnote_definition", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "moved", "label": "n"}, Children: node.Children}}
			return false
		})
		return nil
	})
	// The last peer's settlement is done when the server lets its connection go.
	peer.closeAndWait(t)
	if _, err := pmdoc.ParseRendering(versionText(t, database, issue.PrimaryArtifactID, 2)); err == nil {
		t.Fatal("the browser edit stored a version 2 that the parser reads; the test needs one that it refuses")
	}
	const finding = "- document spec.md in spec.md version 2: Dispatch cannot read this version ("
	const wayOut = "so it cannot judge its texts. Write a new version of the document."
	for _, test := range []struct {
		name     string
		response *httptest.ResponseRecorder
	}{
		{"the request", requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)},
		{"the person's approval", reviewFormDocument(t, tree.handler, issue.PrimaryArtifactID, "approved", "")},
	} {
		body := decodeBody[map[string]any](t, test.response)
		message, _ := body["error"].(string)
		if test.response.Code != http.StatusConflict || body["code"] != "SPEC_FORM" || !strings.Contains(message, finding) || !strings.Contains(message, wayOut) {
			t.Errorf("%s: status=%d body=%s, want 409 SPEC_FORM with %q and %q", test.name, test.response.Code, test.response.Body.String(), finding, wayOut)
		}
	}
	read := dispatchRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/spec-form", nil, "alice")
	if body := decodeBody[map[string]any](t, read); read.Code != http.StatusOK || body["version"] != float64(2) || body["refuses_approval"] != true ||
		!strings.Contains(read.Body.String(), `"rule":"unreadable"`) {
		t.Errorf("the read route: status=%d body=%s, want 200 for version 2, refuses_approval and the rule unreadable", read.Code, read.Body.String())
	}
}

// unreadLine is the line that names an approved document that a judgment could not read.
func unreadLine(document string, version int, issue string) string {
	return fmt.Sprintf("Dispatch cannot read %s version %d of %s, an approved document under the root, so no rule compared its texts.", document, version, issue)
}

// unreadOf is the line of each unread document of an answer's form, as JSON reads it.
func unreadOf(t *testing.T, form map[string]any) []string {
	t.Helper()
	listed, _ := form["unread"].([]any)
	var lines []string
	for _, each := range listed {
		item, _ := each.(map[string]any)
		line, _ := item["line"].(string)
		lines = append(lines, line)
	}
	return lines
}

// containsLine reports whether lines holds one that starts with prefix.
func containsLine(lines []string, prefix string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// One approved document under the root whose approved version today's parser refuses stops no
// other judgment under the root: its texts are not compared, the request, the person's approval
// and the carry's gate of another document go on, and each answer names that document and its
// version in one line. A request that breaks the form names it too. So a rule that compares the
// documents cannot see its texts: the names of its components are no nouns of the system.
func TestAnApprovedVersionThatDispatchCannotReadStopsNoOtherJudgment(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	seedPeople(t, tree.database, "dispatch.admin@example.com")
	components := createFormIssue(t, tree.handler, tree.root, "The components",
		"# Components\n\n## Dispatch (component `dispatch`)\n\nThe server keeps each issue and each document.\n")
	if response := reviewFormDocument(t, tree.handler, components.PrimaryArtifactID, "approved", ""); response.Code != http.StatusCreated {
		t.Fatalf("approve the components: status=%d body=%s", response.Code, response.Body.String())
	}
	clean := createFormIssue(t, tree.handler, tree.root, "A clean spec", "# Spec\n\n"+formRequirement("clean", "Dispatch shows each record."))
	admin := createFormIssue(t, tree.handler, tree.root, "Uses a component name",
		"# Spec\n\n"+formRequirement("reach-owner", "Each issue reaches Dispatch Admin before its owner starts work."))
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, admin.PrimaryArtifactID, nil))

	storeUnreadableVersion(t, tree.database, components.PrimaryArtifactID, 1)
	line := unreadLine("spec.md", 1, components.Key)

	requested := requestFormApproval(t, tree.handler, clean.PrimaryArtifactID, nil)
	if requested.Code != http.StatusCreated {
		t.Fatalf("the request of a clean spec: status=%d body=%s, want 201", requested.Code, requested.Body.String())
	}
	answer := decodeBody[map[string]any](t, requested)
	form, _ := answer["spec_form"].(map[string]any)
	if !containsLine(unreadOf(t, form), line) {
		t.Errorf("the request's spec_form names %q, want the line %q: %s", unreadOf(t, form), line, requested.Body.String())
	}
	read := dispatchRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+clean.PrimaryArtifactID+"/spec-form", nil, "alice")
	if body := decodeBody[map[string]any](t, read); read.Code != http.StatusOK || !containsLine(unreadOf(t, body), line) || body["refuses_approval"] != false {
		t.Errorf("the read route: status=%d body=%s, want 200 naming %q and refusing nothing", read.Code, read.Body.String(), line)
	}
	gate, err := FormGate("ACME-1", "")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := tree.database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	breaks, err := gate(context.Background(), tx, nil, model.AskApprovalDocument{ArtifactID: clean.PrimaryArtifactID, Name: "spec.md", Version: 1})
	_ = tx.Rollback(context.Background())
	if breaks || err != nil {
		t.Errorf("the carry's gate of the clean spec = %v, %v; want false and no error", breaks, err)
	}
	if approved := reviewFormDocument(t, tree.handler, clean.PrimaryArtifactID, "approved", ""); approved.Code != http.StatusCreated {
		t.Errorf("the person's approval of the clean spec: status=%d body=%s, want 201", approved.Code, approved.Body.String())
	}

	// The stated limit: the names of the unreadable components are no nouns of the system now.
	refused := requestFormApproval(t, tree.handler, admin.PrimaryArtifactID, nil)
	if refused.Code != http.StatusConflict {
		t.Fatalf("the request that names Dispatch Admin: status=%d body=%s, want 409", refused.Code, refused.Body.String())
	}
	refusal := decodeBody[map[string]any](t, refused)
	message, _ := refusal["error"].(string)
	if !strings.Contains(message, `statement reach-owner in spec.md version 1: it names a person: "Dispatch Admin".`) || !strings.Contains(message, "\n"+line) {
		t.Errorf("the refusal's words are %q, want the person finding and the line %q", message, line)
	}
}

// A version that the parser refuses, and that a judgment names, is one finding of its document,
// which names the version and the way out: the request is refused with 409, never 500, the read
// route says that the approval is refused, the person's approval is refused, and the carry's gate
// answers a break, so the carry writes no approval.
func TestAVersionThatTheJudgmentNamesAndDispatchCannotReadIsRefused(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	issue := createFormIssue(t, tree.handler, tree.root, "An unreadable spec", "# Spec\n\n"+formRequirement("clean", "Dispatch shows each record."))
	storeUnreadableVersion(t, tree.database, issue.PrimaryArtifactID, 1)
	const finding = "- document spec.md in spec.md version 1: Dispatch cannot read this version ("
	const wayOut = "so it cannot judge its texts. Write a new version of the document."
	for _, test := range []struct {
		name     string
		response func() *httptest.ResponseRecorder
	}{
		{"the request", func() *httptest.ResponseRecorder {
			return requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)
		}},
		{"the person's approval", func() *httptest.ResponseRecorder {
			return reviewFormDocument(t, tree.handler, issue.PrimaryArtifactID, "approved", "")
		}},
	} {
		response := test.response()
		body := decodeBody[map[string]any](t, response)
		message, _ := body["error"].(string)
		if response.Code != http.StatusConflict || body["code"] != "SPEC_FORM" || !strings.Contains(message, finding) || !strings.Contains(message, wayOut) {
			t.Errorf("%s: status=%d body=%s, want 409 SPEC_FORM with %q and %q", test.name, response.Code, response.Body.String(), finding, wayOut)
		}
	}
	read := dispatchRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/spec-form", nil, "alice")
	if body := decodeBody[map[string]any](t, read); read.Code != http.StatusOK || body["refuses_approval"] != true || !strings.Contains(read.Body.String(), `"rule":"unreadable"`) {
		t.Errorf("the read route: status=%d body=%s, want 200, refuses_approval and the rule unreadable", read.Code, read.Body.String())
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
		t.Errorf("the carry's gate of the unreadable version = %v, %v; want a break and no error", breaks, err)
	}
}

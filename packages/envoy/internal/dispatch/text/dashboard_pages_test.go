package text

import (
	"slices"
	"testing"
)

// The Go reader's own rows of the dashboard's pages file: the page of each tab that is a view of
// the issue, and its dispatch:// reference, name the issue; a document's page by the singular
// segment names the document, as the plural does; a segment or a tab the file does not name names
// nothing of Dispatch. `TestExtractMatchesTheSharedHrefTable` holds the same rows to the other
// readers through the shared table.
func TestATabsPageAndReferenceNameItsIssueAndEachDocumentSegmentNamesTheDocument(t *testing.T) {
	const server = "https://dispatch.test"
	issue := Ref{Kind: "issue", IssueKey: "CORE-1", ID: "CORE-1"}
	notes := Ref{Kind: "artifact", IssueKey: "CORE-1", ID: "notes-md"}
	for _, test := range []struct {
		value string
		want  Ref
	}{
		{server + "/issues/CORE-1/verification", issue},
		{server + "/issues/CORE-1/plan", issue},
		{server + "/issues/CORE-1/steps", issue},
		{server + "/issues/CORE-1/activity", issue},
		{server + "/issues/CORE-1/children", issue},
		{server + "/issues/CORE-1/artifacts", issue},
		{server + "/issues/CORE-1/collaborators", issue},
		{server + "/issues/CORE-1/conversation", issue},
		{server + "/issues/CORE-1/verification?v=2#b-x", issue},
		{"dispatch://CORE-1/verification", issue},
		{"dispatch://CORE-1/plan", issue},
		{"dispatch://CORE-1/steps", issue},
		{"dispatch://CORE-1/activity", issue},
		{"dispatch://CORE-1/children", issue},
		{"dispatch://CORE-1/artifacts", issue},
		{"dispatch://CORE-1/collaborators", issue},
		{"dispatch://CORE-1/log", issue},
		{server + "/issues/CORE-1/artifact/notes-md", notes},
		{server + "/issues/CORE-1/artifact/notes-md?v=2", notes},
		{server + "/issues/CORE-1/artifacts/notes-md", notes},
	} {
		if got, ok := ParseRef(test.value, server); !ok || got != test.want {
			t.Errorf("ParseRef(%q) = %#v, %v, want %#v", test.value, got, ok, test.want)
		}
	}
	for _, value := range []string{
		server + "/issues/CORE-1/settings",
		server + "/issues/CORE-1/documents/notes-md",
		server + "/issues/CORE-1/artifactz/notes-md",
		"dispatch://CORE-1/settings",
		"dispatch://CORE-1/conversation",
	} {
		if got, ok := ParseRef(value, server); ok {
			t.Errorf("ParseRef(%q) = %#v, want no reference", value, got)
		}
	}
}

// The pages the dashboard draws each kind of document on, as the pages file lists them, with the
// owner and the slug written in: the spec's three pages and its singular Artifacts page; a
// document of a role on its tab and its Artifacts pages; another document of an issue, of a
// project and of an agent's conversation on its own; nothing for a document of no owner.
func TestDocumentPagesAreThePagesTheDashboardDrawsTheDocumentOn(t *testing.T) {
	for _, test := range []struct {
		owner DocumentOwner
		want  []string
	}{
		{DocumentOwner{IssueKey: "ACME-1", Slug: "spec", Primary: true},
			[]string{"/issues/ACME-1", "/issues/ACME-1/spec", "/issues/ACME-1/artifacts/spec", "/issues/ACME-1/artifact/spec"}},
		{DocumentOwner{IssueKey: "ACME-1", Slug: "verification-md", Role: "verification"},
			[]string{"/issues/ACME-1/verification", "/issues/ACME-1/artifacts/verification-md", "/issues/ACME-1/artifact/verification-md"}},
		{DocumentOwner{IssueKey: "ACME-1", Slug: "plan-md", Role: "plan"},
			[]string{"/issues/ACME-1/plan", "/issues/ACME-1/artifacts/plan-md", "/issues/ACME-1/artifact/plan-md"}},
		{DocumentOwner{IssueKey: "ACME-1", Slug: "decisions-md", Role: "decisions"},
			[]string{"/issues/ACME-1/decisions", "/issues/ACME-1/artifacts/decisions-md", "/issues/ACME-1/artifact/decisions-md"}},
		{DocumentOwner{IssueKey: "ACME-1", Slug: "terms-md"},
			[]string{"/issues/ACME-1/artifacts/terms-md", "/issues/ACME-1/artifact/terms-md"}},
		{DocumentOwner{Project: "ACME", Slug: "notes-md"}, []string{"/projects/ACME/documents/notes-md"}},
		{DocumentOwner{Session: "s 1", Slug: "notes-md"}, []string{"/agents/s%201/artifacts/notes-md"}},
		{DocumentOwner{Slug: "notes-md"}, nil},
	} {
		if got := DocumentPages(test.owner); !slices.Equal(got, test.want) {
			t.Errorf("DocumentPages(%+v) = %q, want %q", test.owner, got, test.want)
		}
	}
}

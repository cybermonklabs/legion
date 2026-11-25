package text

import (
	_ "embed"
	"encoding/json"
	"net/url"
	"strings"
)

// dashboardPagesFile is the dashboard's pages file, which the dashboard's route table generates
// (packages/dispatch/web/src/features/refs/dashboard-pages-file.ts) into @legion/contracts
// (packages/contracts/src/dashboard-pages.json), where the agent client and
// `DISPATCH_HREF_REFERENCES` read it too, so every reader of a dashboard address reads the same
// tabs and document segments. dashboard-pages.json here is the copy that
// packages/contracts/scripts/gen-go.ts writes, and its `check:go` fails when the copy differs.
//
//go:embed dashboard-pages.json
var dashboardPagesFile []byte

// dashboardPagesShape is what the file holds: each tab of an issue whose page names the issue, by
// the path segment that opens it and the segment of its dispatch:// reference; the path segments
// under an issue that open a document by slug; and each page the dashboard draws each kind of
// document on, with {key}, {project}, {session} and {slug} standing for its owner and its slug.
type dashboardPagesShape struct {
	Tabs []struct {
		Page      string `json:"page"`
		Reference string `json:"reference"`
	} `json:"tabs"`
	DocumentSegments []string            `json:"document_segments"`
	DocumentPages    map[string][]string `json:"document_pages"`
}

// dashboardPages is the file, decoded once.
var dashboardPages = func() dashboardPagesShape {
	var file dashboardPagesShape
	if err := json.Unmarshal(dashboardPagesFile, &file); err != nil {
		panic("text: the dashboard's pages file does not decode: " + err.Error())
	}
	return file
}()

// issueTabPages and issueTabReferences are the tabs of the file by the path segment of their page
// and by the segment of their reference: `/issues/<key>/activity` and `dispatch://<key>/activity`,
// `/issues/<key>/conversation` and `dispatch://<key>/log`. Each names its issue: the tab is a view
// of the issue, and no one document of it. issueDocumentSegments are the segments that open a
// document of an issue by slug, `artifacts` and the singular `artifact` the dashboard reads too.
var issueTabPages, issueTabReferences, issueDocumentSegments = func() (map[string]bool, map[string]bool, map[string]bool) {
	pages, references, segments := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, tab := range dashboardPages.Tabs {
		pages[tab.Page], references[tab.Reference] = true, true
	}
	for _, segment := range dashboardPages.DocumentSegments {
		segments[segment] = true
	}
	return pages, references, segments
}()

// DocumentOwner is what names the pages a document is drawn on: its issue, its project or the
// agent session that owns it, its slug, whether it is its issue's spec (Primary), and the part it
// plays beside the spec (Role: "verification", "plan" or "decisions", else "").
type DocumentOwner struct {
	IssueKey, Project, Session, Slug, Role string
	Primary                                bool
}

// DocumentPages is each path of the dashboard that draws the document owner names, under the
// server's address, as the dashboard's pages file lists them: a spec on its issue's own page, on
// the Spec tab and on its Artifacts page by each document segment; a document of a role on the tab
// of that role and on its Artifacts page; any other document of an issue on its Artifacts page; a
// document of a project or of an agent's conversation on its one page. An address with no scheme
// in the document is read against each of them, as each page reads it. A document of no owner has
// none.
func DocumentPages(owner DocumentOwner) []string {
	kind := ""
	switch {
	case owner.IssueKey != "" && owner.Primary:
		kind = "primary"
	case owner.IssueKey != "" && owner.Role != "" && dashboardPages.DocumentPages[owner.Role] != nil:
		kind = owner.Role
	case owner.IssueKey != "":
		kind = "issue"
	case owner.Session != "":
		kind = "agent"
	case owner.Project != "":
		kind = "project"
	default:
		return nil
	}
	fill := strings.NewReplacer("{key}", owner.IssueKey, "{project}", owner.Project,
		"{session}", url.PathEscape(owner.Session), "{slug}", url.PathEscape(owner.Slug))
	var pages []string
	for _, page := range dashboardPages.DocumentPages[kind] {
		pages = append(pages, fill.Replace(page))
	}
	return pages
}

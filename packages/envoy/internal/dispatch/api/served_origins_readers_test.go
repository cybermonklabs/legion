package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The addresses of the server these rows drive: its canonical origin (DISPATCH_SERVER_URL), a
// served address (DISPATCH_SERVED_ORIGINS), a former address (DISPATCH_FORMER_ORIGINS), and an
// address that is none of the three.
const (
	readersCanonical = "https://a.example.test"
	readersServed    = "https://b.example.test"
	readersFormer    = "https://old.example.test"
	readersUnrelated = "https://other.example.test"
)

// servedOriginsLink is a link to the page of issue at origin, one of the four addresses, and whether
// it is a reference: a link at the canonical, a served or a former address is, and one at any other
// address is not.
type servedOriginsLink struct {
	address, origin, issue string
	reference              bool
}

// page is the link: the issue's page at the link's origin.
func (link servedOriginsLink) page() string {
	return link.origin + "/issues/" + link.issue
}

// servedOriginsRig is a server at readersCanonical that serves readersServed too and served
// readersFormer before, given both lists as main gives them (DepsInput), its document service
// reading links through the server's one list of origins (text.ServerOrigins), as main builds it.
// links are one link at each of the four addresses, each to an issue of its own of project CORE,
// and text a text that holds all four.
type servedOriginsRig struct {
	handler   http.Handler
	database  *store.Store
	deps      Deps
	documents *docs.Service
	links     []servedOriginsLink
	text      string
}

func newServedOriginsRig(t *testing.T) *servedOriginsRig {
	t.Helper()
	database := storetest.Open(t)
	broker := events.NewBroker()
	people := headerIdentity(database)
	served, former := []string{readersServed}, []string{readersFormer}
	documents := docs.New(docs.Deps{
		Store: database, Events: broker, Identity: people,
		Origins: text.ServerOrigins(readersCanonical, served, former),
		// No settlement runs while a row reads what a write indexed.
		Settle: time.Hour,
	})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the document service down: %v", err)
		}
	})
	seedPeople(t, database, "alice", "bob")
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: people, AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: readersCanonical, ServedOrigins: served, FormerOrigins: former,
		Docs: documents, Events: broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	registered := register(mux, t.Context(), deps)
	t.Cleanup(func() { waitBackgroundOf(t, registered) })

	rig := &servedOriginsRig{handler: mux, database: database, deps: deps, documents: documents}
	createReferenceAPIProject(t, mux, "CORE")
	var pages []string
	for _, address := range []struct {
		name, origin string
		reference    bool
	}{
		{"the canonical address", readersCanonical, true},
		{"a served address", readersServed, true},
		{"a former address", readersFormer, true},
		{"an unrelated address", readersUnrelated, false},
	} {
		link := servedOriginsLink{address: address.name, origin: address.origin, issue: rig.issue(t, "The plan at "+address.name, "").Key, reference: address.reference}
		rig.links = append(rig.links, link)
		pages = append(pages, link.page())
	}
	rig.text = "The plan is at " + strings.Join(pages, " and ") + " today.\n"
	return rig
}

// cited is, for each link's address, whether the refs index holds an edge from the source kind id
// to the issue the link names.
func (rig *servedOriginsRig) cited(t *testing.T, kind, id string) map[string]bool {
	t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `
		select to_id from refs where from_kind = $1 and from_id = $2 and to_kind = 'issue'
	`, kind, id)
	if err != nil {
		t.Fatalf("read the references of %s %s: %v", kind, id, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the references of %s %s: %v", kind, id, err)
	}
	read := map[string]bool{}
	for _, link := range rig.links {
		read[link.address] = slices.Contains(keys, link.issue)
	}
	return read
}

// issue is a new issue of project CORE titled title, with spec as its spec ("" for none), made past
// the near-copy gate (force): the rows make many issues of one project.
func (rig *servedOriginsRig) issue(t *testing.T, title, spec string) model.Issue {
	t.Helper()
	body := map[string]any{"project": "CORE", "title": title, "force": true}
	if spec != "" {
		body["spec"] = spec
	}
	response := createIssueRequest(t, rig.handler, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %q: status=%d body=%s", title, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// blockEdge writes a real block edge: an issue whose spec holds the callout hold-it, and a document
// of that issue, links.md, whose paragraph cites that callout. It answers the issue and the
// document.
func (rig *servedOriginsRig) blockEdge(t *testing.T) (model.Issue, model.Artifact) {
	t.Helper()
	issue := rig.issue(t, "A block edge", ":::callout{#hold-it kind=\"note\" title=\"\"}\nHold it.\n:::\n\nOther paragraph.\n")
	document := uploadIssueDocument(t, rig.handler, issue.Key, "links.md", "Hold per dispatch://"+issue.Key+"/spec#b-hold-it.\n")
	from, to := "dispatch://"+issue.Key+"/artifact/"+document.Slug, "dispatch://"+issue.Key+"/spec"
	if links := readBlockLinks(t, rig.handler, from, to).Links; len(links) != 1 {
		t.Fatalf("block links from %s to %s = %#v, want the one the document's paragraph writes", from, to, links)
	}
	return issue, document
}

// answeredAsAReference reports whether response, the answer to what, a read naming a link, read the
// link as a reference: 200, naming what the link names (names), against 400 INVALID_REFERENCE, the
// answer to a value that is no reference. Any other answer fails the test.
func answeredAsAReference(t *testing.T, what string, response *httptest.ResponseRecorder, names func() bool) bool {
	t.Helper()
	switch {
	case response.Code == http.StatusOK:
		if !names() {
			t.Fatalf("%s answered 200 %s, naming something other than what the link names", what, response.Body.String())
		}
		return true
	case response.Code == http.StatusBadRequest && strings.Contains(response.Body.String(), `"code":"INVALID_REFERENCE"`):
		return false
	}
	t.Fatalf("%s answered %d %s, want 200 or 400 INVALID_REFERENCE", what, response.Code, response.Body.String())
	return false
}

// TestEveryReaderOfADashboardLinkTakesTheServedAndFormerOrigins: every reader of a Dispatch link in
// the server reads a link at the canonical address, at a served address and at a former address as
// a reference to what its page names, and a link at any other address as none. Each reader is a row,
// driven with one link at each of the four addresses:
//   - the index each write makes as it commits (write_queue.go): a comment's, and an ask's edit
//     (queueEditedReferences);
//   - the document service's readers (refs.NewReader): the index of a document's version
//     (reference_excerpts.go), and the move of an approval request to a document's new version
//     (approval_ask.go);
//   - refs.ReindexOwed, as the document service's block-index reconciliation runs it for a text an
//     older build indexed;
//   - the API's URL parameters: GET /api/v1/references with to= and with from=, GET
//     /api/v1/reference-targets with ref=, and GET /api/v1/block-links with from= and with to=, on a
//     real document and block edge;
//   - the words a result's pull-request line drops as this Dispatch's own (lineWordsFor).
//
// rebuild-refs is cmd/dispatch's TestRebuildRefsReadsTheServedAndFormerOrigins.
func TestEveryReaderOfADashboardLinkTakesTheServedAndFormerOrigins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newServedOriginsRig(t)
	// referencesRead reads each link as GET /api/v1/references's param.
	referencesRead := func(param string) func(t *testing.T) map[string]bool {
		return func(t *testing.T) map[string]bool {
			read := map[string]bool{}
			for _, link := range rig.links {
				response := graphRequest(t, rig.handler, url.Values{param: {link.page()}})
				read[link.address] = answeredAsAReference(t, "GET /api/v1/references?"+param+"="+link.page(), response, func() bool {
					return decodeBody[model.GraphReferences](t, response).Node.Ref == "dispatch://"+link.issue
				})
			}
			return read
		}
	}
	// blockLinksRead reads, for each link's origin, the block edge's document named at that origin
	// as GET /api/v1/block-links's param, the other document named by its dispatch:// reference.
	blockLinksRead := func(param string) func(t *testing.T) map[string]bool {
		return func(t *testing.T) map[string]bool {
			issue, document := rig.blockEdge(t)
			read := map[string]bool{}
			for _, link := range rig.links {
				query := url.Values{"from": {"dispatch://" + issue.Key + "/artifact/" + document.Slug}, "to": {"dispatch://" + issue.Key + "/spec"}}
				if param == "from" {
					query.Set("from", link.origin+"/issues/"+issue.Key+"/artifacts/"+document.Slug)
				} else {
					query.Set("to", link.origin+"/issues/"+issue.Key+"/spec")
				}
				response := blockLinksRequest(t, rig.handler, query)
				read[link.address] = answeredAsAReference(t, "GET /api/v1/block-links?"+query.Encode(), response, func() bool {
					return len(decodeBody[model.BlockLinks](t, response).Links) == 1
				})
			}
			return read
		}
	}
	for _, reader := range []struct {
		name string
		// read is, for each link's address, whether the reader read the link as a reference.
		read func(t *testing.T) map[string]bool
	}{
		{"the index a comment's write makes", func(t *testing.T) map[string]bool {
			comment := createReferenceAPIComment(t, rig.handler, rig.issue(t, "A comment's links", "").Key, rig.text)
			return rig.cited(t, "comment", comment.ID)
		}},
		{"the index an ask's edit makes (queueEditedReferences)", func(t *testing.T) map[string]bool {
			ask := createReferenceAPIAsk(t, rig.handler, rig.issue(t, "An ask's links", "").Key, "Where is the plan?")
			if edited := dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{"question": rig.text}, "alice"); edited.Code != http.StatusOK {
				t.Fatalf("edit the ask's question: status=%d body=%s", edited.Code, edited.Body.String())
			}
			return rig.cited(t, "ask", ask.ID)
		}},
		{"the document service's index of a document's version (reference_excerpts.go)", func(t *testing.T) map[string]bool {
			document := createProjectDocument(t, rig.handler, "CORE", "Plan links", "# Plan links\n\n"+rig.text)
			return rig.cited(t, "artifact", document.ID)
		}},
		{"the document service's move of an approval request to a new version (approval_ask.go)", func(t *testing.T) map[string]bool {
			issue := rig.issue(t, "An approval request's links", "A spec.\n")
			requested := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
				"actor": sessionActor(), "summary": rig.text,
			})
			if requested.Code != http.StatusCreated {
				t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
			}
			askID := decodeBody[struct {
				Ask model.Ask `json:"ask"`
			}](t, requested).Ask.ID
			if _, err := replaceDocumentText(rig.database, rig.documents, issue.PrimaryArtifactID, "A revised spec.\n", model.Actor{Kind: "session", ID: "session-0123456789abcdef"}); err != nil {
				t.Fatalf("revise the spec: %v", err)
			}
			if named := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
				t.Fatalf("name the revised version: status=%d body=%s", named.Code, named.Body.String())
			}
			moved := decodeBody[struct {
				Ask model.Ask `json:"ask"`
			}](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")).Ask
			if !strings.Contains(moved.Question, "(version 2)") {
				t.Fatalf("the approval request asks %q, want it moved to version 2", moved.Question)
			}
			return rig.cited(t, "ask", askID)
		}},
		{"refs.ReindexOwed, run by the document service's block-index reconciliation", func(t *testing.T) map[string]bool {
			comment := createReferenceAPIComment(t, rig.handler, rig.issue(t, "An owed comment's links", "").Key, rig.text)
			// What an older build leaves: the comment's references not written, and the comment owed
			// a re-index with this build's grammar.
			if _, err := rig.database.Pool.Exec(ctx, `delete from refs where from_kind = 'comment' and from_id = $1`, comment.ID); err != nil {
				t.Fatalf("delete the comment's references: %v", err)
			}
			oweBlockGrammar(t, rig.database, "comment", comment.ID)
			if tick, err := rig.documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil || tick.Items == 0 {
				t.Fatalf("the block-index reconciliation = %+v, %v; want the owed comment re-indexed", tick, err)
			}
			return rig.cited(t, "comment", comment.ID)
		}},
		{"GET /api/v1/references?to=", referencesRead("to")},
		{"GET /api/v1/references?from=", referencesRead("from")},
		{"GET /api/v1/reference-targets?ref=", func(t *testing.T) map[string]bool {
			read := map[string]bool{}
			for _, link := range rig.links {
				target := "/api/v1/reference-targets?" + url.Values{"ref": {link.page()}}.Encode()
				response := dispatchRequest(t, rig.handler, http.MethodGet, target, nil, "alice")
				read[link.address] = answeredAsAReference(t, "GET "+target, response, func() bool {
					targets := decodeBody[model.ReferenceTargets](t, response).Targets
					return len(targets) == 1 && targets[0].Kind == "issue" && !targets[0].Gone
				})
			}
			return read
		}},
		{"GET /api/v1/block-links?from=", blockLinksRead("from")},
		{"GET /api/v1/block-links?to=", blockLinksRead("to")},
		{"a result line's word drop (lineWordsFor)", func(t *testing.T) map[string]bool {
			// A link written without its scheme holds nothing else the line drops a word for.
			words := lineWordsFor(nil, rig.deps.Origins)
			read := map[string]bool{}
			for _, link := range rig.links {
				read[link.address] = words.drops(strings.TrimPrefix(link.page(), "https://"))
			}
			return read
		}},
	} {
		t.Run(reader.name, func(t *testing.T) {
			read := reader.read(t)
			for _, link := range rig.links {
				t.Run(link.address, func(t *testing.T) {
					if read[link.address] != link.reference {
						t.Errorf("%s reads the link at %s (%s) as a reference: %t, want %t", reader.name, link.address, link.origin, read[link.address], link.reference)
					}
				})
			}
		})
	}
}

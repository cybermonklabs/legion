package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// keptReadsQuestion is the question the kept reads' issues ask on the issue itself.
const keptReadsQuestion = "Which store holds the list?"

// askQuestion is the question of the ask id, as GET /api/v1/asks/{id} answers it.
func askQuestion(t *testing.T, handler http.Handler, id string) string {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+id, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the ask %s: status=%d body=%s", id, read.Code, read.Body.String())
	}
	return decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, read).Ask.Question
}

// TestTheKeptReadsAnswerFromTheStore holds what R21 keeps for one release to the store, on an
// issue born converted and on one that has not converted, each holding decisionsViewSpec, a plan,
// a Verification document of one check and a document of notes:
//
//   - the issue read's queries name the artifact of each query node, in the order spec,
//     verification, plan, decisions, and on the issue that has not converted its role documents
//     in that shape, and verification_artifact_id and plan_artifact_id name the same artifacts;
//   - GET /api/v1/issues/{key}/decisions and ?view=decisions answer the same nodes: the spec's two
//     asks, its two decisions and the reading under the first;
//   - a request for a document's approval is an ask of the issue and no node of the decisions
//     query. With one request approved and another open beside a question the issue asks, the
//     route, ?view=decisions and the query's record (docs.ReadQueryRecord) hold the question and
//     neither request, and so does the rendering of the converted issue's decisions query
//     (docs.RenderAt). On the converted issue the question moves the query's record, so the drain
//     a recorded result runs writes a version of the decisions query; the request and its
//     approval move nothing of it, so the drain after each leaves the query's newest version where
//     it stood, and ?view=decisions answers no node of the request.
func TestTheKeptReadsAnswerFromTheStore(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "KEPT")
	for _, fixture := range []struct {
		name, key   string
		unconverted bool
	}{
		{"converted", "KEPT-1", false},
		{"unconverted", "KEPT-2", true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			issue := createDigestIssue(t, handler, database, "KEPT", fixture.name, decisionsViewSpec(fixture.key), fixture.unconverted)
			path := "/api/v1/issues/" + issue.Key
			plan := uploadDocument(t, handler, path, "plan.md", "How to build it.\n").Artifact
			verification := uploadDocument(t, handler, path, "verification.md", revisedChecks).Artifact
			notes := uploadDocument(t, handler, path, "notes.md", "What the list is for.\n").Artifact
			// The spec's settlement opens the ask of each ask block it holds.
			settleIssue(t, documents, issue.Key)
			response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("read the issue: status=%d body=%s", response.Code, response.Body)
			}
			detail := decodeBody[issueDetail](t, response)
			subtree := "issue:" + issue.Key
			want := []issueQuery{
				{Query: "spec", ArtifactID: issue.PrimaryArtifactID, Takes: []string{"callout", "criterion", "requirement"}, Subtree: subtree},
				{Query: "verification", ArtifactID: verification.ID, Takes: []string{"check", "result"}, Subtree: subtree},
				{Query: "plan", ArtifactID: plan.ID, Takes: []string{"plan_task", "plan_role"}, Subtree: subtree},
			}
			decisionsID, _ := decisionsQueryVersions(t, database, issue.Key)
			if !fixture.unconverted {
				if decisionsID == "" {
					t.Fatal("the converted issue has no decisions query node")
				}
				want = append(want, issueQuery{Query: "decisions", ArtifactID: decisionsID, Takes: []string{"ask", "decision", "reading"}, Subtree: subtree})
			}
			for _, query := range want {
				var held *string
				if err := database.Pool.QueryRow(t.Context(), `
					select n.attributes->>'query' from artifacts a
					left join graph_nodes n on n.kind = 'artifact' and n.id = a.ref_key and n.supertag = 'query'
					where a.id = $1`, query.ArtifactID).Scan(&held); err != nil {
					t.Fatalf("read stored %s: %v", query.Query, err)
				}
				if fixture.unconverted && held != nil || !fixture.unconverted && deref(held) != query.Query {
					t.Errorf("stored query for %s = %q, unconverted = %t", query.Query, deref(held), fixture.unconverted)
				}
			}
			if !slices.EqualFunc(detail.Queries, want, func(a, b issueQuery) bool {
				return a.Query == b.Query && a.ArtifactID == b.ArtifactID && a.Subtree == b.Subtree && slices.Equal(a.Takes, b.Takes)
			}) {
				t.Errorf("queries = %+v, want %+v", detail.Queries, want)
			}
			if detail.VerificationArtifactID != verification.ID || detail.PlanArtifactID != plan.ID {
				t.Errorf("kept ids = %q, %q, want %q, %q", detail.VerificationArtifactID, detail.PlanArtifactID, verification.ID, plan.ID)
			}

			read := readIssueDecisions(t, handler, issue.Key)
			view := treeRead(t, handler, issue.Key, "?view=decisions", "alice", false)
			if view.Converted == fixture.unconverted {
				t.Fatalf("tree converted = %t, want %t", view.Converted, !fixture.unconverted)
			}
			requireDecisionsViewAsRead(t, fixture.name, view, read)
			var kept, tree []string
			for _, entry := range read.Entries {
				kept = append(kept, entryKey(entry))
				for _, reading := range entry.Readings {
					kept = append(kept, reading.Node.Kind+":"+reading.Node.ID)
				}
			}
			for _, node := range view.Nodes {
				if node.Role == treeRoleQuery {
					tree = append(tree, node.Kind+":"+node.ID)
				}
			}
			slices.Sort(kept)
			slices.Sort(tree)
			if len(kept) != 5 || !slices.Equal(kept, tree) || view.View != graphstore.QueryDecisions {
				t.Errorf("decisions read = %v, tree view %q = %v, want the same two asks, two decisions and reading", kept, view.View, tree)
			}

			// A result recorded of the Verification document's check marks the queries of a
			// converted issue owed, and its route drains them once it has committed: the drain
			// writes a version of each query whose record or rendering moved
			// (docs.Service.DrainOwedQueries). drained is the decisions query's newest version
			// after such a drain, and 0 on the issue that has not converted, which holds no query
			// node and drains nothing.
			drained := func(after string) int {
				t.Helper()
				if fixture.unconverted {
					return 0
				}
				if recorded := recordResult(t, handler, issue.Key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1})); recorded.Code != http.StatusCreated {
					t.Fatalf("record a result of c1 %s: status=%d body=%s", after, recorded.Code, recorded.Body.String())
				}
				_, newest := decisionsQueryVersions(t, database, issue.Key)
				return newest
			}
			start := drained("before the issue asks its question")
			question := createIssueAsk(t, handler, issue.Key, keptReadsQuestion)
			asked := drained("after the issue asked its question")
			if !fixture.unconverted && asked <= start {
				t.Errorf("the drain after a question the issue asked leaves the decisions query at version %d, want a version after %d: the question is a node of the query",
					asked, start)
			}
			approved := requestApproval(t, handler, notes.ID)
			if requested := drained("after the approval request"); requested != asked {
				t.Errorf("the drain after a request for the approval of notes.md leaves the decisions query at version %d, want it at %d: the request is no node of the query",
					requested, asked)
			}
			if held := treeRead(t, handler, issue.Key, "?view=decisions", "alice", false).nodesNamed(graphstore.KindAsk, approved.ID); len(held) != 0 {
				t.Errorf("?view=decisions answers the open approval request %s as %s, want no node of it", approved.ID, entryJSON(t, held))
			}
			reviewDocument(t, handler, notes.ID, "alice", "approved", "")
			if given := drained("after the approval"); given != asked {
				t.Errorf("the drain after the approval of notes.md leaves the decisions query at version %d, want it at %d: an approved request is no node of the query",
					given, asked)
			}
			open := requestApproval(t, handler, verification.ID)

			read = readIssueDecisions(t, handler, issue.Key)
			view = treeRead(t, handler, issue.Key, "?view=decisions", "alice", false)
			record, err := docs.ReadQueryRecord(t.Context(), database.Pool, issue.Key, graphstore.QueryDecisions, refs.TreeCut{})
			if err != nil {
				t.Fatalf("read the decisions record of %s: %v", issue.Key, err)
			}
			rendering := ""
			if decisionsID != "" {
				rendered, err := docs.RenderAt(t.Context(), database.Pool, model.Actor{Kind: "user", ID: "alice"}, decisionsID, time.Now().Add(time.Minute))
				if err != nil {
					t.Fatalf("render the decisions query of %s: %v", issue.Key, err)
				}
				rendering = rendered.Markdown
			}
			for _, ask := range []struct {
				what, id string
				taken    bool
			}{
				{"the question the issue asked", question, true},
				{"the approved request", approved.ID, false},
				{"the open request", open.ID, false},
			} {
				node := decisionEntryNode{Kind: graphstore.KindAsk, ID: ask.id}
				answers := []struct {
					where string
					holds bool
				}{
					{"GET /api/v1/issues/{key}/decisions", slices.ContainsFunc(read.Entries, func(entry decisionEntry) bool { return entry.Node == node })},
					{"?view=decisions", len(view.nodesNamed(node.Kind, node.ID)) > 0},
					{"the decisions query's record", slices.ContainsFunc(record.Nodes, func(taken docs.TreeNode) bool { return taken.Kind == node.Kind && taken.ID == node.ID })},
				}
				if decisionsID != "" {
					answers = append(answers, struct {
						where string
						holds bool
					}{"the decisions query's rendering", strings.Contains(rendering, askQuestion(t, handler, ask.id))})
				}
				for _, answer := range answers {
					if answer.holds != ask.taken {
						t.Errorf("%s holds %s (%s): %t, want %t", answer.where, ask.what, ask.id, answer.holds, ask.taken)
					}
				}
			}
		})
	}
}

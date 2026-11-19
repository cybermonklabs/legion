package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestAReasoningNodeNamesTheSessionItsBodyNamesAsItsWriter holds the reasoning routes to the two
// things spec v46 R12 keeps apart: attribution, the authored_by tuple to the writer
// graphstore.AuthorOf names of a node's made_by, and proof, the author the one rule compares for
// a read and a settle (graphstore.SameAuthor). They differ where a write names a session its
// credential does not prove.
//
// On the deployment's shared bearer a pre-mortem with one finding, a risk under it and a
// constraint, each written by a body that names one session, are project-visible, and each holds
// its authored_by to that session, whose node the write states; the write answers that session
// as each node's author, and so do the lens read and the constraint list, to the shared bearer
// and to alice's cookie alike. Yet each made_by proves nobody, so none is that session's under the
// one rule. A pod's project lens written on a session its body names holds its authored_by to that
// session too, and answers it as its author, to the pod's read as well, while the one rule reads
// it as the pod's workload and not as that session. A lens the pod reserves for itself is made by
// the pod's token, which names only a workload, so it holds no authored_by and answers no author.
func TestAReasoningNodeNamesTheSessionItsBodyNamesAsItsWriter(t *testing.T) {
	ctx := context.Background()
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t)})
	issue := createInteractionIssue(t, handler, "ATR", "Attributed work", "A paragraph to look at.\n")
	spec := "dispatch://" + issue.Key + "/spec"
	const (
		sharedBearer  = "agent-token"
		bearerSession = "ses_attributed_bearer_01"
		podSession    = "ses_attributed_pod_00001"
		workload      = "system:serviceaccount:legion:worker-a"
	)
	podToken := issuer.Mint(t, issuer.signing, issuer.Claims(workload, serviceTokenAudience))
	post := func(token, session, path string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body["actor"] = map[string]any{"kind": "session", "id": session}
		response := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+path, body, token)
		if response.Code != http.StatusCreated {
			t.Fatalf("POST %s as session %s: status=%d body=%s", path, session, response.Code, response.Body.String())
		}
		return response
	}
	// requireAuthor holds an answered author to the session it names, or to none.
	requireAuthor := func(t *testing.T, what string, author *reasoningAuthor, session string) {
		t.Helper()
		switch {
		case session == "" && author != nil:
			t.Errorf("%s answers the author %+v, want none", what, author)
		case session != "" && (author == nil || author.Kind != graphstore.KindSession || author.ID != session):
			t.Errorf("%s answers the author %+v, want the session %s", what, author, session)
		}
	}
	// requireStored holds the node kind/id to its visibility and to the authored_by it holds now,
	// the session session or none, and answers its row.
	requireStored := func(t *testing.T, kind, id, visibility, session string) graphstore.Node {
		t.Helper()
		node, held, err := graphstore.ReadNode(ctx, database.Pool, kind, id)
		if err != nil || !held {
			t.Fatalf("read %s/%s: held %t, %v", kind, id, held, err)
		}
		if node.Visibility != visibility {
			t.Errorf("%s/%s is at visibility %q, want %q", kind, id, node.Visibility, visibility)
		}
		var want []string
		if session != "" {
			want = []string{graphstore.KindSession + "/" + session}
		}
		requireFarEnds(t, database, kind, id, graphstore.TupleAuthoredBy, want...)
		return node
	}
	readLens := func(t *testing.T, address string, request func(target string) *httptest.ResponseRecorder) *lensRead {
		t.Helper()
		response := request("/api/v1/lenses?on=" + url.QueryEscape(address))
		if response.Code != http.StatusOK {
			t.Fatalf("read the lens %s: status=%d body=%s", address, response.Code, response.Body.String())
		}
		lens := decodeBody[nodeLenses](t, response).Lens
		if lens == nil || lens.Address != address {
			t.Fatalf("the read of %s answers the lens %+v", address, lens)
		}
		return lens
	}
	asToken := func(token string) func(string) *httptest.ResponseRecorder {
		return func(target string) *httptest.ResponseRecorder {
			return agentRequest(t, handler, http.MethodGet, target, nil, token)
		}
	}
	asCookie := func(login string) func(string) *httptest.ResponseRecorder {
		return func(target string) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodGet, target, nil, login)
		}
	}

	t.Run("the shared bearer names the session its body names and proves nobody", func(t *testing.T) {
		if nodeHeld(t, database, graphstore.KindSession, bearerSession) {
			t.Fatalf("the store holds the session %s before any write names it", bearerSession)
		}
		premortem := decodeBody[lensInvoked](t, post(sharedBearer, bearerSession, "/lenses", map[string]any{
			"on": spec, "kind": graphstore.LensKindPreMortem, "findings": []string{"The bill doubles."},
		}))
		risk := decodeBody[riskWritten](t, post(sharedBearer, bearerSession, "/risks", map[string]any{
			"lens": premortem.Address, "heading": "The bill doubles", "early_warning": "The invoice grows", "prevention": "A budget alarm",
		}))
		constraint := decodeBody[constraintRead](t, post(sharedBearer, bearerSession, "/constraints", map[string]any{
			"standard": "A rule of the house", "reference": "section 1",
		}))
		if len(premortem.Findings) != 1 {
			t.Fatalf("the pre-mortem answers the findings %+v, want one", premortem.Findings)
		}
		finding := premortem.Findings[0]
		requireAuthor(t, "the shared bearer's pre-mortem", premortem.Author, bearerSession)
		requireAuthor(t, "the shared bearer's finding", finding.Author, bearerSession)
		requireAuthor(t, "the shared bearer's risk", risk.Author, bearerSession)
		requireAuthor(t, "the shared bearer's constraint", constraint.Author, bearerSession)
		for _, written := range []struct{ kind, id string }{
			{graphstore.KindLens, premortem.ID}, {refs.BlockNodeKind, finding.ID},
			{graphstore.KindRisk, risk.ID}, {graphstore.KindConstraint, constraint.ID},
		} {
			node := requireStored(t, written.kind, written.id, graphstore.VisibilityProject, bearerSession)
			// Attribution is no proof: the record names the session and proves nobody, so the
			// one rule reads the node as nobody's, and the session the body named is no author.
			if graphstore.ProvesAuthor(node.MadeBy) ||
				graphstore.SameAuthor(node.MadeBy, model.Actor{Kind: "session", ID: bearerSession, Proven: true}) {
				t.Errorf("%s/%s, made by %+v, proves an author under the one rule, want nobody", written.kind, written.id, node.MadeBy)
			}
		}
		if !nodeHeld(t, database, graphstore.KindSession, bearerSession) {
			t.Errorf("the shared bearer's writes state authored_by to %s and the store holds no node of it", bearerSession)
		}
		for name, request := range map[string]func(string) *httptest.ResponseRecorder{
			"the shared bearer": asToken(sharedBearer), "alice's cookie": asCookie("alice"),
		} {
			lens := readLens(t, premortem.Address, request)
			requireAuthor(t, name+"'s read of the shared bearer's pre-mortem", lens.Author, bearerSession)
			if lens.Visibility != graphstore.VisibilityProject {
				t.Errorf("%s reads the shared bearer's pre-mortem at visibility %q, want project", name, lens.Visibility)
			}
			response := request("/api/v1/issues/" + issue.Key + "/constraints")
			if response.Code != http.StatusOK {
				t.Fatalf("%s's read of the constraints: status=%d body=%s", name, response.Code, response.Body.String())
			}
			listed := decodeBody[issueConstraints](t, response).Constraints
			if len(listed) != 1 || listed[0].ID != constraint.ID {
				t.Fatalf("%s lists the constraints %+v, want the shared bearer's", name, listed)
			}
			requireAuthor(t, name+"'s list of the shared bearer's constraint", listed[0].Author, bearerSession)
		}
	})

	t.Run("a pod names the session its body names and proves its workload", func(t *testing.T) {
		if nodeHeld(t, database, graphstore.KindSession, podSession) {
			t.Fatalf("the store holds the session %s before any write names it", podSession)
		}
		project := decodeBody[lensInvoked](t, post(podToken, podSession, "/lenses", map[string]any{
			"on": spec, "kind": "trade-off",
		}))
		requireAuthor(t, "the pod's project lens", project.Author, podSession)
		node := requireStored(t, graphstore.KindLens, project.ID, graphstore.VisibilityProject, podSession)
		if !nodeHeld(t, database, graphstore.KindSession, podSession) {
			t.Errorf("the pod's lens states authored_by to %s and the store holds no node of it", podSession)
		}
		// The one rule reads the pod's write as its workload's: the session beside it is not
		// proven, so the pod's token is its author and the session the body named is not.
		subject := workload
		if !graphstore.SameAuthor(node.MadeBy, model.Actor{Service: &subject}) {
			t.Errorf("the pod's project lens, made by %+v, is not the workload %s's under the one rule", node.MadeBy, workload)
		}
		if graphstore.SameAuthor(node.MadeBy, model.Actor{Kind: "session", ID: podSession, Proven: true}) {
			t.Errorf("the pod's project lens, made by %+v, is the session %s's under the one rule, want the workload's", node.MadeBy, podSession)
		}
		requireAuthor(t, "the pod's read of its project lens", readLens(t, project.Address, asToken(podToken)).Author, podSession)

		own := decodeBody[lensInvoked](t, post(podToken, podSession, "/lenses", map[string]any{
			"on": spec, "kind": "trade-off", "visibility": graphstore.VisibilityAuthor,
		}))
		requireAuthor(t, "the lens the pod reserved for itself", own.Author, "")
		reserved := requireStored(t, graphstore.KindLens, own.ID, graphstore.VisibilityAuthor, "")
		if reserved.MadeBy.ID != "" || reserved.MadeBy.Service == nil || *reserved.MadeBy.Service != workload {
			t.Errorf("the lens the pod reserved for itself is made by %+v, want the pod's token alone", reserved.MadeBy)
		}
		read := readLens(t, own.Address, asToken(podToken))
		requireAuthor(t, "the pod's read of the lens it reserved for itself", read.Author, "")
		if read.Visibility != graphstore.VisibilityAuthor {
			t.Errorf("the pod reads the lens it reserved for itself at visibility %q, want author", read.Visibility)
		}
	})
}

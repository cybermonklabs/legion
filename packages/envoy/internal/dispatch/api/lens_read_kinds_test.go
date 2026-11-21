package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// TestALensReadOnAKindNoLensStandsOnAnswersWhatANodeNobodyHoldsAnswers holds the lens read to
// P9 of the plan: a lens stands only on a node of the kinds under an
// issue, so `on` naming a node of any other kind answers the 404 an address nobody holds answers,
// whatever the store holds. The store holds alice's person node, a session, a role and its page,
// each project-visible, which the read once answered 200 with no lens and so told any caller, the
// shared bearer among them, whether that person, session or role exists (D12b). Each of them, a
// login nobody holds, a session and a role nobody holds, and a lens nobody holds answer bob and
// the shared bearer one 404 NOT_FOUND, byte for byte once the address is substituted, and the
// issue itself still answers its lenses.
func TestALensReadOnAKindNoLensStandsOnAnswersWhatANodeNobodyHoldsAnswers(t *testing.T) {
	rig := newReasoningRig(t)
	ctx := context.Background()
	// Alice's lens writes her person node (writeReasoningNodes, graphstore.AuthorFacts), which the
	// agents' write then finds there.
	rig.invoke(rig.spec, "trade-off", "alice", nil)
	const session, role = "ses_lens_read_kinds_01", "lens-read-kinds"
	page := graphstore.RolePageID(role)
	writeAgentNodes(t, rig.database, "alice", role, session)
	for _, held := range []struct{ kind, id string }{
		{graphstore.KindPerson, "alice"}, {graphstore.KindSession, session},
		{graphstore.KindRole, role}, {graphstore.KindPage, page},
	} {
		node, found, err := graphstore.ReadNode(ctx, rig.database.Pool, held.kind, held.id)
		if err != nil || !found || node.Visibility != graphstore.VisibilityProject || node.State != graphstore.StateLive {
			t.Fatalf("the store holds %s %s as %+v (found %t, %v), want a live project-visible node: the test's premise",
				held.kind, held.id, node, found, err)
		}
	}

	nobodys := "dispatch://lens/" + uuid.NewString()
	addresses := []string{
		graphstore.KindPerson + "/alice",
		graphstore.KindPerson + "/nobody-holds-this-login@example.com",
		graphstore.KindSession + "/" + session,
		graphstore.KindSession + "/ses_nobody_holds_this_one",
		graphstore.KindRole + "/" + role,
		graphstore.KindRole + "/nobody-holds-this-role",
		graphstore.KindPage + "/" + page,
	}
	for _, reader := range []struct {
		name string
		read func(on string) (int, string)
	}{
		{"bob", func(on string) (int, string) {
			response := rig.lensesResponse(on, "bob")
			return response.Code, response.Body.String()
		}},
		{"the shared bearer", func(on string) (int, string) {
			response := agentRequest(t, rig.handler, http.MethodGet, "/api/v1/lenses?on="+url.QueryEscape(on), nil, "agent-token")
			return response.Code, response.Body.String()
		}},
	} {
		code, body := reader.read(nobodys)
		if code != http.StatusNotFound {
			t.Fatalf("%s's read on a lens nobody holds answers %d %s, want 404", reader.name, code, body)
		}
		want := strings.ReplaceAll(body, nobodys, "<on>")
		for _, on := range addresses {
			code, body := reader.read(on)
			t.Logf("%s's read on %s: status=%d body=%s", reader.name, on, code, body)
			if got := strings.ReplaceAll(body, on, "<on>"); code != http.StatusNotFound || got != want {
				t.Errorf("%s's read on %s answers %d %s, want the 404 a lens nobody holds answers: %s",
					reader.name, on, code, body, want)
			}
		}
	}
	if read := rig.lenses("dispatch://"+rig.issue, "bob"); read.On.Address != "dispatch://"+rig.issue {
		t.Errorf("bob's read on the issue answers the node %+v, want the issue", read.On)
	}
}

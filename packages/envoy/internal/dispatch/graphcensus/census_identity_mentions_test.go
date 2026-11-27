package graphcensus_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The person and the role a text names by its address, beside the message's session.
const (
	identityPerson = "person-1@example.com"
	identityRole   = "sizer"
)

// identityNames names a person, the message's session and a role, each by the address a mention's
// link writes (text's parseDispatch).
const identityNames = "Ask [@Person](dispatch://person/" + identityPerson + "), tell [@Agent](dispatch://agent/" +
	messageSession + ") and [@Role](dispatch://role/" + identityRole + ")."

// TestTheCensusHoldsTheIdentityMentionsATextStates holds the store census to the identity mentions
// a written text states (R5): a mentions tuple to a person, a session and a role from a document's
// block that names them, and from a comment and an issue message that name each, all written
// through the server's own routes. A document block's are projected by no row - block_refs holds
// no party - so the block_ref kind leaves them out of the block's links and counts them as a figure
// of their own; a comment's session and role are its delivery routes, which its comment_mentions
// rows project; and a person's mention is a delivery to that person, read by the person kind. The
// census reads no difference.
func TestTheCensusHoldsTheIdentityMentionsATextStates(t *testing.T) {
	r := newWritersRig(t)
	ctx := context.Background()
	issue := decoded[struct {
		Key string `json:"key"`
	}](t, r.request(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "SIZE", "title": "What names an identity", "spec": "# Spec\n\n" + identityNames + "\n",
	}, "alice", http.StatusCreated)).Key
	if _, err := r.documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
		t.Fatalf("index the spec's blocks: %v", err)
	}
	comment := decoded[model.Comment](t, r.request(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		map[string]any{"body": identityNames}, "alice", http.StatusCreated)).ID
	message := decoded[model.Message](t, r.request(t, http.MethodPost, "/api/v1/issues/"+issue+"/messages", map[string]any{
		"body": identityNames, "target": "session:" + messageSession, "delivery": "steer",
	}, "alice", http.StatusCreated)).ID
	r.awaitTrue(t, "the comment's and the message's sends to "+messageSession+" settling", `
		select exists (select 1 from comment_deliveries where comment_id = $1::uuid and target = $3 and state = 'sent')
		   and exists (select 1 from message_deliveries where message_id = $2::uuid and state = 'sent')
	`, comment, message, "session:"+messageSession)
	// The census reads the log published, as production's does: the role the comment routes to has
	// its node made by the outbox's publish of the comment's event.
	graphcensus.PublishTheLog(t, r.database)

	// The store holds each mention the test is about, so a census that reads none of them proves
	// nothing.
	for _, from := range []struct{ kind, id string }{
		{graphstore.KindComment, comment}, {graphstore.KindMessage, message},
	} {
		requireIdentityMentions(t, r, from.kind+" "+from.id,
			`t.from_kind = $1 and t.from_id = $2`, from.kind, from.id)
	}
	requireIdentityMentions(t, r, "the spec's block",
		`t.from_kind = $1 and t.from_id like $2`, refs.BlockNodeKind, "SIZE-%")

	report, err := graphcensus.StoreCensus(ctx, r.database.Pool.Config().ConnString(), false, nil)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	identities := int64(-1)
	for _, kind := range report.Kinds {
		if kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
			t.Errorf("the census of %s reads %d row and %d node difference(s): rows %+v, nodes %+v",
				kind.Kind, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
		}
		for _, narrowing := range kind.Narrowings {
			if kind.Kind == "block_ref" && narrowing.Name == "identity_mentions" {
				identities = narrowing.Count
			}
		}
	}
	if identities != 3 {
		t.Errorf("the census of block_ref counts %d identity mention(s) out of a document's blocks, want the spec block's three", identities)
	}
}

// requireIdentityMentions fails unless the store holds a current mentions tuple to the person, to
// the session and to the role out of the node where names it, a predicate over t bound by args.
func requireIdentityMentions(t *testing.T, r *writersRig, what, where string, args ...any) {
	t.Helper()
	for _, identity := range []struct{ kind, id string }{
		{graphstore.KindPerson, identityPerson}, {graphstore.KindSession, messageSession}, {graphstore.KindRole, identityRole},
	} {
		r.awaitTrue(t, what+" mentioning "+identity.kind+" "+identity.id, `
			select exists (select 1 from graph_tuples t
				where `+where+` and t.kind = 'mentions' and t.to_kind = $3 and t.to_id = $4 and t.asserted)
		`, append(args, identity.kind, identity.id)...)
	}
}

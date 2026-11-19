package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestFindNodeUnderIssueDecodesTheStoreAddress: the `{id}` of an issue's node routes is the node's
// store address, in each of its three spellings, and names a node only while that node stands
// under the issue the path names and the caller may see it. Every other answer is one 404, in the
// same words for a node that does not exist, one of another issue and one reserved for another
// author.
//
// The fixture is the private-node seed: an issue with a spec of public blocks and one block its
// author reserved for herself, a comment everyone reads and one she keeps to herself. On the
// spec's first block stands a pre-mortem lens, with a finding and a risk under it and a second
// risk its author keeps to herself, each written through the one write path of those kinds
// (writeReasoningNodes); each stands under the issue by the walk of its part_of up to the block
// and its document. A second issue, of another project, holds nodes of its own, a lens on that
// issue among them. A run of the command bar the reader asked is a node the reader reads and one
// that stands under no issue, so no issue's route finds it.
func TestFindNodeUnderIssueDecodesTheStoreAddress(t *testing.T) {
	fixture := seedPrivateNode(t, true)
	ctx := context.Background()
	pool := fixture.database.Pool
	reader, author := privateNodeReader(), privateNodeAuthor()
	other := createInteractionIssue(t, fixture.handler, "OTHR", "Another issue", "# Spec\n\nElsewhere.\n")
	var otherSpec string
	if err := pool.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`, other.PrimaryArtifactID).Scan(&otherSpec); err != nil {
		t.Fatalf("read the other issue's spec address: %v", err)
	}
	otherComment := createThreadComment(t, fixture.handler, other.Key, map[string]any{"body": "A remark elsewhere."}, "alice").ID
	document := "dispatch://" + fixture.refKey
	// The spec's blocks are nodes of its issue's room, each named by its own id inside the spec.
	_, firstBlock, _ := strings.Cut(fixture.first, "#")

	lens, risk, privateRisk, otherLens := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	finding := graphstore.KindLens + "/" + lens + "#finding"
	writeReasoning := func(issue string, nodes ...reasoningNode) {
		t.Helper()
		writeReasoningAs(t, fixture.database, issue, author, nodes...)
	}
	writeReasoning(fixture.issue,
		reasoningNode{kind: graphstore.KindLens, id: lens, parentKind: refs.BlockNodeKind, parentID: fixture.first,
			tuples: []graphstore.Tuple{{FromKind: graphstore.KindLens, FromID: lens, Kind: graphstore.TupleHasKind,
				ToKind: graphstore.KindLensKind, ToID: graphstore.LensKindPreMortem}}},
		reasoningNode{kind: refs.BlockNodeKind, id: finding, parentKind: graphstore.KindLens, parentID: lens,
			tuples: []graphstore.Tuple{{FromKind: refs.BlockNodeKind, FromID: finding, Kind: graphstore.AttributeText,
				ToKind: graphstore.KindValue, ToID: "A finding of the pre-mortem."}}},
		reasoningNode{kind: graphstore.KindRisk, id: risk, parentKind: graphstore.KindLens, parentID: lens,
			attributes: map[string]any{graphstore.AttributeHeading: "A risk everyone reads."}},
		reasoningNode{kind: graphstore.KindRisk, id: privateRisk, visibility: graphstore.VisibilityAuthor,
			parentKind: graphstore.KindLens, parentID: lens, attributes: map[string]any{graphstore.AttributeHeading: "A risk she keeps."}},
	)
	writeReasoning(other.Key, reasoningNode{kind: graphstore.KindLens, id: otherLens, parentKind: graphstore.KindIssue, parentID: other.Key})
	// A run of the command bar the reader asked, which the reader may read and which stands under
	// no issue.
	run, err := directServer(Deps{Store: fixture.database}).recordGoingRequest(ctx, reader, "PRIV", "What stands under this issue?")
	if err != nil {
		t.Fatalf("record the reader's run: %v", err)
	}
	if node, found, err := refs.Node(ctx, pool, reader, graphstore.KindInteraction, run.ID, true); err != nil || !found || node.IssueKey != nil {
		t.Fatalf("the reader reads its run as %+v (found %t, %v), want it, under no issue", node, found, err)
	}

	for _, test := range []struct {
		name, id   string
		who        model.Actor
		kind, node string
		visibility string
	}{
		{"the issue by its store address", "issue/" + fixture.issue, reader, "issue", fixture.issue, graphstore.VisibilityProject},
		{"the issue by its dispatch address", "dispatch://" + fixture.issue, reader, "issue", fixture.issue, graphstore.VisibilityProject},
		{"the spec by its ref_key", "artifact/" + fixture.refKey, reader, "artifact", fixture.specID, graphstore.VisibilityProject},
		{"a block of the spec by its node id", fixture.first, reader, refs.BlockNodeKind, fixture.first, graphstore.VisibilityProject},
		{"a block of the spec by its dispatch address", refs.BlockRef(document, firstBlock), reader, refs.BlockNodeKind, fixture.first, graphstore.VisibilityProject},
		{"a comment by its store address", "comment/" + fixture.publicComment, reader, graphstore.KindComment, fixture.publicComment, graphstore.VisibilityProject},
		{"a comment by its dispatch address", "dispatch://" + fixture.issue + "/comment/" + fixture.publicComment, reader, graphstore.KindComment, fixture.publicComment, graphstore.VisibilityProject},
		{"the private block to its author", fixture.private, author, refs.BlockNodeKind, fixture.private, graphstore.VisibilityAuthor},
		{"the private comment to its author", "comment/" + fixture.privateComment, author, graphstore.KindComment, fixture.privateComment, graphstore.VisibilityAuthor},
		{"a lens on a block by its store address", "lens/" + lens, reader, graphstore.KindLens, lens, graphstore.VisibilityProject},
		{"a lens on a block by its dispatch address", "dispatch://lens/" + lens, reader, graphstore.KindLens, lens, graphstore.VisibilityProject},
		{"a finding by its node id", finding, reader, graphstore.KindLensBlock, finding, graphstore.VisibilityProject},
		{"a finding by its dispatch address", "dispatch://lens/" + lens + "#b-finding", reader, graphstore.KindLensBlock, finding, graphstore.VisibilityProject},
		{"a risk by its store address", "risk/" + risk, reader, graphstore.KindRisk, risk, graphstore.VisibilityProject},
		{"a risk by its dispatch address", "dispatch://risk/" + risk, reader, graphstore.KindRisk, risk, graphstore.VisibilityProject},
		{"the private risk to its author", "risk/" + privateRisk, author, graphstore.KindRisk, privateRisk, graphstore.VisibilityAuthor},
	} {
		t.Run(test.name, func(t *testing.T) {
			node, err := FindNodeUnderIssue(ctx, pool, test.who, fixture.issue, test.id)
			if err != nil {
				t.Fatalf("FindNodeUnderIssue(%q, %q) as %s: %v", fixture.issue, test.id, test.who.ID, err)
			}
			if node.Kind != test.kind || node.ID != test.node {
				t.Fatalf("FindNodeUnderIssue(%q, %q) = %s %s, want %s %s", fixture.issue, test.id, node.Kind, node.ID, test.kind, test.node)
			}
			if node.IssueKey == nil || *node.IssueKey != fixture.issue {
				t.Fatalf("FindNodeUnderIssue(%q, %q) answers a node of issue %v", fixture.issue, test.id, node.IssueKey)
			}
			if node.State != graphstore.StateLive || node.Visibility != test.visibility {
				t.Fatalf("FindNodeUnderIssue(%q, %q) answers state %q and visibility %q, want %q and %q",
					fixture.issue, test.id, node.State, node.Visibility, graphstore.StateLive, test.visibility)
			}
		})
	}

	for _, test := range []struct {
		name, key, id string
		who           model.Actor
	}{
		{"another issue under this one", fixture.issue, "issue/" + other.Key, reader},
		{"this issue under another", other.Key, "issue/" + fixture.issue, reader},
		{"another issue's spec", fixture.issue, "artifact/" + otherSpec, reader},
		{"another issue's comment", fixture.issue, "comment/" + otherComment, reader},
		{"this issue's block under another", other.Key, fixture.first, reader},
		{"a comment nobody wrote", fixture.issue, "comment/00000000-0000-4000-8000-000000000000", reader},
		{"a block no version holds", fixture.issue, fixture.refKey + "#nosuchblock", reader},
		{"a run of the command bar, which stands under no issue", fixture.issue, "interaction/" + run.ID, reader},
		{"a run's dispatch address", fixture.issue, "dispatch://interaction/" + run.ID, reader},
		{"a lens nobody invoked", fixture.issue, "lens/00000000-0000-4000-8000-000000000000", reader},
		{"another issue's lens", fixture.issue, "lens/" + otherLens, reader},
		{"this issue's lens under another", other.Key, "lens/" + lens, reader},
		{"this issue's finding under another", other.Key, finding, reader},
		{"this issue's risk under another", other.Key, "risk/" + risk, reader},
		{"the private risk to another reader", fixture.issue, "risk/" + privateRisk, reader},
		{"a kind with no loader", fixture.issue, "supertag/requirement", reader},
		{"a kind that names nothing", fixture.issue, "widget/" + fixture.issue, reader},
		{"no kind", fixture.issue, fixture.issue, reader},
		{"no id", fixture.issue, "issue/", reader},
		{"nothing", fixture.issue, "", reader},
		{"a dispatch address that names nothing", fixture.issue, "dispatch://" + fixture.issue + "/nothing", reader},
		{"the private block to another reader", fixture.issue, fixture.private, reader},
		{"the private block's dispatch address to another reader", fixture.issue, refs.BlockRef(document, fixture.privateBlock), reader},
		{"the private comment to another reader", fixture.issue, "comment/" + fixture.privateComment, reader},
	} {
		t.Run(test.name, func(t *testing.T) {
			node, err := FindNodeUnderIssue(ctx, pool, test.who, test.key, test.id)
			var refused *apiError
			if !errors.As(err, &refused) {
				t.Fatalf("FindNodeUnderIssue(%q, %q) = %s %s, %v; want a 404", test.key, test.id, node.Kind, node.ID, err)
			}
			want := fmt.Sprintf("no node %q under %s", test.id, test.key)
			if refused.status != http.StatusNotFound || refused.code != "NOT_FOUND" || refused.message != want {
				t.Fatalf("FindNodeUnderIssue(%q, %q) refused %d %s %q, want 404 NOT_FOUND %q",
					test.key, test.id, refused.status, refused.code, refused.message, want)
			}
		})
	}

	// The other issue's lens is found under that issue, so its 404 under this one is where it
	// stands and not a lens no read answers.
	if node, err := FindNodeUnderIssue(ctx, pool, reader, other.Key, "lens/"+otherLens); err != nil || node.ID != otherLens {
		t.Errorf("FindNodeUnderIssue(%q, the lens on it) = %s %s, %v; want that lens", other.Key, node.Kind, node.ID, err)
	}
	// Each node answers the text and the parent its loader reads: the finding its text, under its
	// lens; the risk its heading; the lens the block it was invoked on.
	for _, test := range []struct{ id, text, parentKind, parentID string }{
		{"lens/" + lens, "", refs.BlockNodeKind, fixture.first},
		{finding, "A finding of the pre-mortem.", graphstore.KindLens, lens},
		{"risk/" + risk, "A risk everyone reads.", graphstore.KindLens, lens},
	} {
		node, err := FindNodeUnderIssue(ctx, pool, reader, fixture.issue, test.id)
		if err != nil {
			t.Fatalf("FindNodeUnderIssue(%q, %q): %v", fixture.issue, test.id, err)
		}
		if node.Parent == nil || node.Parent.Kind != test.parentKind || node.Parent.ID != test.parentID {
			t.Errorf("%s stands under %+v, want %s %s", test.id, node.Parent, test.parentKind, test.parentID)
		}
		if test.text == "" {
			continue
		}
		edges, err := refs.Edges(ctx, pool, reader, refs.Query{Direction: "in", Kind: graphstore.KindLens, ID: lens})
		if err != nil {
			t.Fatalf("read the edges into the lens: %v", err)
		}
		var excerpt string
		for _, edge := range edges {
			if edge.Node.ID == node.ID && edge.Excerpt != nil {
				excerpt = edge.Excerpt.Text
			}
		}
		if excerpt != test.text {
			t.Errorf("the edge from %s into its lens shows %q, want %q", test.id, excerpt, test.text)
		}
	}
}

// writeReasoningAs writes nodes under issue through the one write path of the reasoning kinds
// (writeReasoningNodes), in a transaction of its own, as author, a credential that proves the actor
// it records, failing the test on a refusal.
func writeReasoningAs(t *testing.T, database *store.Store, issue string, author model.Actor, nodes ...reasoningNode) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		_, err := writeReasoningNodes(ctx, tx, reasoningWrite{
			reasoningCaller: reasoningCaller{proving: author, recorded: author}, issue: issue, at: time.Now().UTC(), nodes: nodes,
		})
		return err
	}); err != nil {
		t.Fatalf("write the reasoning nodes under %s as %s: %v", issue, author.ID, err)
	}
}

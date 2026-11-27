package refs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestDeliveryAttemptIsAResolvedNodeKind holds a delivery attempt to the node kinds this package
// resolves. This release writes a node of kind graphstore.KindDeliveryAttempt for every attempt to
// reach a session or a role, so a read of the graph that meets one - the far end of an edge, a
// node a reader opens, a count beside a list - resolves it as it resolves every other node the
// store holds itself: nodeRegistry carries its loader (loadStoreNodes) and nodeKinds lists it,
// backlinkArms declares it (present and empty, since its tuples run out of it and no arm of
// graph_edges targets one), Node finds an attempt the store holds, and BacklinkCounts counts one
// rather than refusing its kind. A registry left without the kind fails each of these, named.
func TestDeliveryAttemptIsAResolvedNodeKind(t *testing.T) {
	kind := graphstore.KindDeliveryAttempt
	if loader, ok := nodeRegistry[kind]; !ok || loader == nil {
		t.Errorf("nodeRegistry carries no loader for %q, so no read of the graph resolves a delivery attempt", kind)
	}
	if !slices.Contains(nodeKinds, kind) {
		t.Errorf("nodeKinds %v does not list %q, so the mention arm of a backlink count filters every attempt out", nodeKinds, kind)
	}
	if _, ok := backlinkArms[kind]; !ok {
		t.Errorf("backlinkArms declares no arms for %q, so every backlink count of an attempt is refused", kind)
	}

	database := storetest.Open(t)
	ctx := context.Background()
	key := graphstore.CommentAttempt("6f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b", "session:attempt-kind-session", 1)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the attempt's node: %v", err)
	}
	defer tx.Rollback(ctx)
	node := graphstore.AttemptNode(key, graphstore.AttemptWriter, time.Now().UTC().Truncate(time.Microsecond), graphstore.NoEvent)
	if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, nil); err != nil {
		t.Fatalf("write the attempt's node: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the attempt's node: %v", err)
	}

	reader := model.Actor{Kind: "user", ID: "alice"}
	resolved, found, err := Node(ctx, database.Pool, reader, kind, key.ID(), false)
	if err != nil {
		t.Fatalf("resolve the attempt %s: %v", key.ID(), err)
	}
	if !found || resolved.Kind != kind || resolved.ID != key.ID() {
		t.Fatalf("Node(%s, %s) answered %+v, found %v; want the attempt the store holds", kind, key.ID(), resolved, found)
	}
	counts, err := BacklinkCounts(ctx, database.Pool, reader, kind, []string{key.ID()}, nil)
	if err != nil {
		t.Fatalf("count the backlinks of the attempt %s: %v", key.ID(), err)
	}
	if count := counts[key.ID()]; count != 0 {
		t.Fatalf("BacklinkCounts of the attempt %s answered %v; want no backlink for an attempt nothing points at", key.ID(), counts)
	}
}

package grapherase

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The writers of the decisions seedDecisions writes: the agent's session, which the server proved
// belongs to deciderOwner, and deciderPerson, who writes a decision of their own.
const (
	deciderSession = "erase-deciding-session"
	deciderOwner   = "carol"
	deciderPerson  = "dave"
)

// seededDecisions are the three blocks seedDecisions writes, by node address: the agent's decision
// d1, the agent's reading g1 inside it, and the person's decision d2.
type seededDecisions struct {
	document   preStoreDocument
	d1, g1, d2 string
}

// seedDecisions writes into CORE-1's document what the block index and the typed-node pass write
// of an agent's decision holding the agent's reading, and of a person's decision beside it
// (refs.IndexBlocks, refs.IndexDecisionNodes): each block's node of its type, its in_document and
// its part_of, its document_blocks row and the reading's parent_block_id; each decision's
// decided_by and the block_refs row projecting it; the reading's authored_by; and the writers'
// nodes with the alias_of tying the session to the person the server proved owns it
// (graphstore.AuthorFacts). The session and its owner write nothing else, and neither does the
// person, so each is held to their node by these blocks alone.
func seedDecisions(t *testing.T, database *store.Store) seededDecisions {
	t.Helper()
	ctx := context.Background()
	document := seedPreStoreDocument(t, database)
	seeded := seededDecisions{
		document: document,
		d1:       document.refKey + "#d1", g1: document.refKey + "#g1", d2: document.refKey + "#d2",
	}
	now := time.Now().UTC()
	owner := deciderOwner
	agent := model.Actor{Kind: "session", ID: deciderSession, Owner: &owner, Act: &model.ActorAct{Session: deciderSession}}
	person := model.Actor{Kind: "user", ID: deciderPerson}
	block := func(address, supertag string, who model.Actor) graphstore.Node {
		return graphstore.Node{
			Kind: refs.BlockNodeKind, ID: address, Supertag: supertag, Visibility: graphstore.VisibilityProject,
			State: graphstore.StateLive, Project: "CORE", MadeAt: now, MadeBy: who, SourceEvent: graphstore.NoEvent,
		}
	}
	tie := func(from, kind, toKind, toID string, who model.Actor) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: from, Kind: kind, ToKind: toKind, ToID: toID,
			At: now, Who: who, SourceEvent: graphstore.NoEvent, Asserted: true,
		}
	}
	nodes := []graphstore.Node{
		block(seeded.d1, refs.DecisionSupertag, agent),
		block(seeded.g1, refs.ReadingSupertag, agent),
		block(seeded.d2, refs.DecisionSupertag, person),
	}
	tuples := []graphstore.Tuple{
		tie(seeded.d1, refs.TupleInDocument, "artifact", document.refKey, agent),
		tie(seeded.d1, graphstore.TuplePartOf, "artifact", document.refKey, agent),
		tie(seeded.g1, refs.TupleInDocument, "artifact", document.refKey, agent),
		tie(seeded.g1, graphstore.TuplePartOf, refs.BlockNodeKind, seeded.d1, agent),
		tie(seeded.d2, refs.TupleInDocument, "artifact", document.refKey, person),
		tie(seeded.d2, graphstore.TuplePartOf, "artifact", document.refKey, person),
		tie(seeded.d1, graphstore.TupleDecidedBy, graphstore.KindSession, deciderSession, agent),
		tie(seeded.d2, graphstore.TupleDecidedBy, graphstore.KindPerson, deciderPerson, person),
	}
	authors, authored, named := graphstore.AuthorFacts(refs.BlockNodeKind, seeded.g1, agent, now, graphstore.NoEvent)
	if !named || len(authors) != 2 {
		t.Fatalf("the reading's author facts name %d node(s) (named %t), want the session and the person it belongs to", len(authors), named)
	}
	nodes = append(append(nodes, authors...), graphstore.PersonNode(deciderPerson, person, now, graphstore.NoEvent))
	tuples = append(tuples, authored...)

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the decisions' write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		t.Fatalf("write the decisions' nodes and tuples: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into document_blocks (artifact_id, block_id, node_id, node_kind, node_address, type, text_sha256, excerpt,
		                             changed_version, changed_at, parent_block_id)
		values ($1::uuid, 'd1', $2, 'block', $2, 'decision', '', 'REST for every service.', 1, now(), null),
		       ($1::uuid, 'g1', $3, 'block', $3, 'reading', '', 'The agent reads it so.', 1, now(), 'd1'),
		       ($1::uuid, 'd2', $4, 'block', $4, 'decision', '', 'gRPC for streaming.', 1, now(), null)
	`, document.artifactID, seeded.d1, seeded.g1, seeded.d2); err != nil {
		t.Fatalf("write the decisions' block rows: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, node_kind, node_address)
		values ($1::uuid, 'd1', $2, 'session', $4, 'decided_by', 1, 'block', $2),
		       ($1::uuid, 'd2', $3, 'person', $5, 'decided_by', 1, 'block', $3)
	`, document.artifactID, seeded.d1, seeded.d2, deciderSession, deciderPerson); err != nil {
		t.Fatalf("write the decisions' decided_by rows: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the decisions' write: %v", err)
	}
	return seeded
}

// erasureWriters is what the latest graph_erasures row of kind:id records as the node's writers
// (Erasure.Writers).
func erasureWriters(t *testing.T, database *store.Store, kind, id string) []ErasedWriter {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(removed->'writers', 'null'::jsonb) from graph_erasures
		where node_kind = $1 and node_id = $2 order by id desc limit 1
	`, kind, id).Scan(&raw); err != nil {
		t.Fatalf("read the erasure record of %s:%s: %v", kind, id, err)
	}
	var writers []ErasedWriter
	if err := json.Unmarshal(raw, &writers); err != nil {
		t.Fatalf("read the writers of the erasure record of %s:%s, %s: %v", kind, id, raw, err)
	}
	return writers
}

// An agent whose only acts were a decision and the reading inside it stays a node of the store
// once both are erased, as does the person the server proved its session belongs to: no erase
// removes a session, and the session's alias_of still ties it to that person. The erase takes the
// decision's decided_by, its block_refs row and the reading's authored_by, which were all that
// named the session, and records the session as each node's writer in its graph_erasures row, and
// the census reads the session and its owner off that record. It reads zero differences before
// the erases, after the reading's, and after the decision's.
func TestTheCensusHoldsTheAgentOfAnErasedDecisionAndReading(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedDecisions(t, database)
	requireCensusZero(t, database, "before the erases")

	requireErased(t, database, refs.BlockNodeKind, seeded.g1)
	requireCensusZero(t, database, "after the reading's erase")
	requireErased(t, database, refs.BlockNodeKind, seeded.d1)
	agent := []ErasedWriter{{Kind: graphstore.KindSession, ID: deciderSession}}
	for _, erased := range []string{seeded.g1, seeded.d1} {
		if writers := erasureWriters(t, database, refs.BlockNodeKind, erased); !slices.Equal(writers, agent) {
			t.Errorf("the erasure record of %s names the writers %+v, want the agent's session %+v", erased, writers, agent)
		}
	}
	var naming int64
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from block_refs where to_kind = 'session' and to_id = $1)
		     + (select count(*) from graph_tuples where to_kind = 'session' and to_id = $1)
	`, deciderSession).Scan(&naming); err != nil {
		t.Fatalf("count what still names the agent's session: %v", err)
	}
	if naming != 0 {
		t.Fatalf("after both erases %d row(s) or tuple(s) still name the agent's session, want none, so only the erasure record holds it", naming)
	}
	requireCensusZero(t, database, "after the decision's erase")
	for _, node := range []struct{ kind, id string }{{graphstore.KindSession, deciderSession}, {graphstore.KindPerson, deciderOwner}} {
		if held := readTypeNode(t, database, node.kind, node.id); !held.row {
			t.Errorf("after both erases the store holds no node %s:%s, want the agent and its owner standing", node.kind, node.id)
		}
	}
}

// A person the record of an erase names as the erased node's writer is held to their node by that
// record, and no longer once an erase of that person runs after it: the person's own erase deletes
// the node, and a record written before it names a person the store holds no node of by design.
// dave's decision is erased, and the census reads zero with dave counted off the record; dave is
// then erased, and the census reads zero again, the record no longer holding him.
func TestTheCensusHoldsNoErasedPersonToTheRecordOfAnEraseBeforeTheirs(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedDecisions(t, database)
	requireCensusZero(t, database, "before the erases")

	requireErased(t, database, refs.BlockNodeKind, seeded.d2)
	if writers := erasureWriters(t, database, refs.BlockNodeKind, seeded.d2); !slices.Equal(writers,
		[]ErasedWriter{{Kind: graphstore.KindPerson, ID: deciderPerson}}) {
		t.Fatalf("the erasure record of %s names the writers %+v, want %s", seeded.d2, writers, deciderPerson)
	}
	requireCensusZero(t, database, "after the person's decision's erase")
	if held := readTypeNode(t, database, graphstore.KindPerson, deciderPerson); !held.row {
		t.Fatalf("after the decision's erase the store holds no node of %s, whose node only the record holds", deciderPerson)
	}

	requireErased(t, database, graphstore.KindPerson, deciderPerson)
	requireCensusZero(t, database, "after the person's own erase")
}

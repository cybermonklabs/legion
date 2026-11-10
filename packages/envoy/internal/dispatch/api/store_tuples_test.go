package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestTheStoreHoldsNodesAndAppendedTuples is this half of dispatch://INFRA-1149's
// c-store-tuples: one write through the real write path, read back out of the node and tuple
// store. The other half, the facts a resolved role holder states - the `held_by` tuple, its
// retraction with the earlier claim still in the history, and the session `part_of` the role it
// claimed - is the test of the same name in internal/dispatch/outbox, which drives the
// publisher's record with an injected claim store: the claim lives in Envoy's role bucket, which
// no request of this package reaches.
//
// What this half reads:
//
//   - the node a comment create stored, its supertag and its attribute values, with the time,
//     the actor and the id of the event that same transaction appended; every tuple's cause is
//     that event's own id, and the test fails on any that is not;
//   - every node the run wrote carrying `visibility` and carrying `state`, read as the tuples
//     they are and not as the row's columns;
//   - a `person`, a `role`, a `session` and a page node resolved through the one reader every
//     kind uses (refs.Node), each by what names it: the canonical login, the topic token, the
//     session id, and the role the page hangs under;
//   - the backlink arms map against the kinds that reader declares, in both directions;
//   - the tuple table read through the column list `graph_edges` declares, mapped by position;
//   - the alias a credential that proved both a session and its person leaves, which is the one
//     row of c-person-graph's identity table no shell driver can present: the broker's token
//     signs a DPoP proof per request, so the driver carries every other row and this test
//     carries this one.
func TestTheStoreHoldsNodesAndAppendedTuples(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A paragraph of the spec.\n")
	const writer = "019a1c0b-1111-7000-8000-000000000001"

	response := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{
			"body":  "One write through the real write path.",
			"actor": map[string]string{"kind": "session", "id": writer},
		}, "agent-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("write the comment: status=%d body=%s", response.Code, response.Body.String())
	}
	written := decodeBody[model.Comment](t, response)

	// The node the write stored, and the event that same transaction appended.
	node, held, err := graphstore.ReadNode(ctx, database.Pool, "comment", written.ID)
	if err != nil || !held {
		t.Fatalf("read the comment's node: held=%t err=%v", held, err)
	}
	event := commentCreatedEvent(t, database, written.ID)
	switch {
	case node.Supertag != "comment":
		t.Fatalf("the node's supertag is %q, want comment", node.Supertag)
	case node.Visibility != graphstore.VisibilityProject || node.State != graphstore.StateLive:
		t.Fatalf("the node is %s/%s, want %s/%s", node.Visibility, node.State,
			graphstore.VisibilityProject, graphstore.StateLive)
	case !node.MadeAt.Equal(written.CreatedAt):
		t.Fatalf("the node was made at %s, and its record at %s", node.MadeAt, written.CreatedAt)
	case !node.MadeBy.SameAs(model.Actor{Kind: "session", ID: writer}):
		t.Fatalf("the node was made by %+v, want the session that wrote it", node.MadeBy)
	case node.SourceEvent != event:
		t.Fatalf("the node's cause is event %d, want %d", node.SourceEvent, event)
	}

	// Its attribute values are tuples, and every tuple of the write carries that event as its
	// cause. The attribute read and the relation read are one read, so both are asked here.
	for attribute, want := range map[string]string{
		"visibility": graphstore.VisibilityProject, "state": graphstore.StateLive,
	} {
		value, ok, err := graphstore.CurrentValue(ctx, database.Pool, "comment", written.ID, attribute)
		if err != nil || !ok || value != want {
			t.Fatalf("the node's %s reads %q (held %t, err %v), want %q", attribute, value, ok, err, want)
		}
	}
	tuples, err := graphstore.CurrentOut(ctx, database.Pool, "comment", written.ID, nil)
	if err != nil {
		t.Fatalf("read the tuples out of the comment: %v", err)
	}
	if len(tuples) == 0 {
		t.Fatal("the write stated nothing about the node it stored")
	}
	for _, tuple := range tuples {
		if tuple.SourceEvent != event {
			t.Fatalf("the %s tuple's cause is event %d, want the write's own event %d",
				tuple.Kind, tuple.SourceEvent, event)
		}
		if !tuple.At.Equal(written.CreatedAt) {
			t.Fatalf("the %s tuple is stated at %s, and the record at %s",
				tuple.Kind, tuple.At, written.CreatedAt)
		}
	}
	if authored := tupleTo(tuples, graphstore.TupleAuthoredBy); authored != "session:"+writer {
		t.Fatalf("the comment is authored by %q, want the session that wrote it", authored)
	}

	// Every node the run wrote carries both base attributes, as the node row's columns and as
	// the tuples R8 makes them. graphstore.Write states them for each node its insert made, so
	// every writer states them alike - the comment path, the arms the publisher's record
	// splices in, and refs.IndexBlocks' block nodes - and a node written again unchanged states
	// neither, which is what keeps a re-index of a document nothing moved appending nothing.
	for _, held := range liveNodes(t, database) {
		if held.Visibility == "" || held.State == "" {
			t.Fatalf("%s %s carries visibility %q and state %q", held.Kind, held.ID,
				held.Visibility, held.State)
		}
		for _, attribute := range []string{"visibility", "state"} {
			value, ok, err := graphstore.CurrentValue(ctx, database.Pool, held.Kind, held.ID, attribute)
			if err != nil {
				t.Fatalf("read the %s of %s %s: %v", attribute, held.Kind, held.ID, err)
			}
			if !ok || value == "" {
				t.Fatalf("%s %s carries no %s tuple", held.Kind, held.ID, attribute)
			}
		}
	}

	// The four kinds of people and agents, each resolved by what names it, through the one
	// reader every kind of the graph goes through.
	const (
		person  = "alice@example.com"
		role    = "store-tuples-reader"
		session = "019a1c0b-2222-7000-8000-000000000002"
	)
	writeAgentNodes(t, database, person, role, session)
	for _, named := range []struct{ kind, id string }{
		{graphstore.KindPerson, model.CanonicalLogin("  Alice@Example.com  ")},
		{graphstore.KindRole, role},
		{graphstore.KindSession, session},
		{graphstore.KindPage, graphstore.RolePageID(role)},
	} {
		read, found, err := refs.Node(ctx, database.Pool, model.Actor{Kind: "user", ID: "alice"},
			named.kind, named.id, false)
		if err != nil || !found {
			t.Fatalf("the one reader answers no %s node for %q: found=%t err=%v",
				named.kind, named.id, found, err)
		}
		if read.Kind != named.kind || read.ID != named.id {
			t.Fatalf("the reader answered %s %s for %s %s", read.Kind, read.ID, named.kind, named.id)
		}
	}

	// The backlink arms map against the kinds that reader declares, in both directions: every
	// kind it resolves has an entry, and a kind the map does not declare is counted for nothing.
	counted := refs.BacklinkKinds()
	for _, kind := range []string{graphstore.KindPerson, graphstore.KindRole, graphstore.KindSession,
		graphstore.KindPage, "comment", "block"} {
		if !slices.Contains(counted, kind) {
			t.Fatalf("the reader resolves %s and the backlink arms map declares no entry for it", kind)
		}
	}
	for _, kind := range []string{"supertag", "attribute", "value"} {
		if slices.Contains(counted, kind) {
			t.Fatalf("the backlink arms map declares %s, which the reader resolves no node of", kind)
		}
	}

	// The tuple table read through the column list the view declares, mapped by position.
	requireViewColumnsOverTuples(t, database)

	// The alias a credential that proved both a session and its person leaves.
	requireProvedSessionAlias(t, database, written.IssueKey, person)
}

// commentCreatedEvent is the id of the event the comment's own transaction appended.
func commentCreatedEvent(t *testing.T, database *store.Store, comment string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events where type = 'comment.created' and payload->>'id' = $1
	`, comment).Scan(&id); err != nil {
		t.Fatalf("read the event that stored the comment: %v", err)
	}
	return id
}

// tupleTo is the far end of the one tuple of that kind, as "kind:id", or "" for none.
func tupleTo(tuples []graphstore.Tuple, kind string) string {
	for _, tuple := range tuples {
		if tuple.Kind == kind {
			return tuple.ToKind + ":" + tuple.ToID
		}
	}
	return ""
}

// liveNodes is every live node the store holds, which in this run is every node it wrote.
func liveNodes(t *testing.T, database *store.Store) []graphstore.Node {
	t.Helper()
	var nodes []graphstore.Node
	rows, err := database.Pool.Query(context.Background(),
		`select distinct kind from graph_nodes where state = $1`, graphstore.StateLive)
	if err != nil {
		t.Fatalf("read the kinds the store holds: %v", err)
	}
	var kinds []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			rows.Close()
			t.Fatalf("scan a kind: %v", err)
		}
		kinds = append(kinds, kind)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the kinds the store holds: %v", err)
	}
	for _, kind := range kinds {
		held, err := graphstore.NodesOfKind(context.Background(), database.Pool, kind)
		if err != nil {
			t.Fatalf("read the %s nodes: %v", kind, err)
		}
		nodes = append(nodes, held...)
	}
	return nodes
}

// writeAgentNodes writes the person, the role, the session and the role's page through the store
// writer, as the request that first records each of them does.
func writeAgentNodes(t *testing.T, database *store.Store, person, role, session string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	at := time.Now().UTC()
	who := model.Actor{Kind: "system", ID: "store-tuples-test"}
	nodes := []graphstore.Node{
		graphstore.PageNode(graphstore.RolePageID(role), who, at, graphstore.NoEvent),
		graphstore.PersonNode(person, who, at, graphstore.NoEvent),
		graphstore.RoleNode(role, who, at, graphstore.NoEvent),
		graphstore.SessionNode(session, who, at, graphstore.NoEvent),
	}
	arms, params, err := graphstore.WriteArms(1, nodes, nil, []graphstore.Claim{{
		FromKind: graphstore.KindPage, FromID: graphstore.RolePageID(role),
		Kind: graphstore.TuplePartOf, ToKind: graphstore.KindRole, ToID: role,
		At: at, Who: who, SourceEvent: graphstore.NoEvent,
	}})
	if err != nil {
		t.Fatalf("build the write: %v", err)
	}
	if _, err := tx.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
		t.Fatalf("write the people and the agents: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// requireViewColumnsOverTuples reads graph_edges' own column list and holds every column of it
// against a column of graph_tuples, by position and not word for word: the view's created_at is
// the tuple's at, and its source_seq is the tuple's legacy_source_seq.
func requireViewColumnsOverTuples(t *testing.T, database *store.Store) {
	t.Helper()
	under := map[string]string{"created_at": "at", "source_seq": "legacy_source_seq"}
	view := relationColumns(t, database, "graph_edges")
	tuple := relationColumns(t, database, "graph_tuples")
	for _, column := range view {
		want := column
		if mapped, named := under[column]; named {
			want = mapped
		}
		if !slices.Contains(tuple, want) {
			t.Fatalf("graph_edges declares %s and graph_tuples holds no %s under it", column, want)
		}
	}
	// The arm's own order, which is the order the view's columns are selected in.
	ordered := []string{"from_kind", "from_id", "kind", "to_kind", "to_id", "created_at", "source_seq"}
	if !slices.Equal(view, ordered) {
		t.Fatalf("graph_edges declares %v, want %v, which is the order its tuple arm selects", view, ordered)
	}
}

// relationColumns is one relation's columns in the order it declares them.
func relationColumns(t *testing.T, database *store.Store, relation string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select attname from pg_attribute
		where attrelid = $1::regclass and attnum > 0 and not attisdropped
		order by attnum
	`, relation)
	if err != nil {
		t.Fatalf("read the columns of %s: %v", relation, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan a column of %s: %v", relation, err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the columns of %s: %v", relation, err)
	}
	return columns
}

// requireProvedSessionAlias writes a comment's authorship under the credential the secrets
// broker issues - one that proves both the session and the person it belongs to - and reads the
// alias_of tuple that write leaves. bearerSessionActor is asked for the actor, so what is read
// here is what a request would record; a session a body merely names carries no act and earns
// no alias, which the second half of this check reads.
func requireProvedSessionAlias(t *testing.T, database *store.Store, issue *string, person string) {
	t.Helper()
	ctx := context.Background()
	const proved = "019a1c0b-3333-7000-8000-000000000003"
	const claimed = "019a1c0b-4444-7000-8000-000000000004"
	owner := person
	brokered := model.Actor{
		Kind: "session", ID: proved, Owner: &owner,
		Credential: &model.Credential{Scopes: model.Scopes, Session: proved},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/X/comments", nil)
	actor, err := bearerSessionActor(request, brokered, nil)
	if err != nil {
		t.Fatalf("resolve the broker-issued credential: %v", err)
	}
	if actor.Act == nil || actor.Act.Session != proved {
		t.Fatalf("the actor records %+v, want an act naming the session the credential proved", actor.Act)
	}
	// A personal token proves its person and no session: the session is the one its body named,
	// and that is a session it merely claims.
	personal := model.Actor{Owner: &owner, Credential: &model.Credential{Scopes: model.Scopes, TokenID: "t-1"}}
	claimedActor, err := bearerSessionActor(request,
		model.Actor{Kind: "session", Owner: personal.Owner, Credential: personal.Credential},
		&model.Actor{Kind: "session", ID: claimed})
	if err != nil {
		t.Fatalf("resolve the personal token: %v", err)
	}
	if claimedActor.Act != nil {
		t.Fatalf("a session the body named carries %+v, want no act at all", claimedActor.Act)
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	at := time.Now().UTC()
	for _, write := range []struct {
		comment string
		actor   model.Actor
	}{{"4c6e7b00-0000-4000-8000-000000000001", actor}, {"4c6e7b00-0000-4000-8000-000000000002", claimedActor}} {
		if err := graphstore.Authored(ctx, tx, "comment", write.comment, write.actor, at, graphstore.NoEvent); err != nil {
			t.Fatalf("record who wrote %s: %v", write.comment, err)
		}
	}
	aliases, err := graphstore.CurrentOut(ctx, tx, graphstore.KindSession, proved, []string{graphstore.TupleAliasOf})
	if err != nil {
		t.Fatalf("read the aliases of the proved session: %v", err)
	}
	if len(aliases) != 1 || aliases[0].ToKind != graphstore.KindPerson ||
		aliases[0].ToID != model.CanonicalLogin(owner) {
		t.Fatalf("the session the credential proved reads %+v, want one alias_of to %s", aliases, owner)
	}
	merely, err := graphstore.CurrentOut(ctx, tx, graphstore.KindSession, claimed, []string{graphstore.TupleAliasOf})
	if err != nil {
		t.Fatalf("read the aliases of the claimed session: %v", err)
	}
	if len(merely) != 0 {
		t.Fatalf("a session an actor merely claims reads %+v, want no alias at all", merely)
	}
	_ = issue
}

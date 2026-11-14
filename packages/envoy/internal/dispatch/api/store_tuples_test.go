package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
//     the actor and the id of the event that same transaction appended; every tuple that
//     transaction appended, attributes and relations alike, is read with no `to_kind` filter,
//     its cause is that event's own id, and the test fails on any that is not, naming a
//     base-attribute tuple whose cause reads 0;
//   - a deleted block's node kept, its `state` moved to `gone` by a tuple appended after the
//     live one, and no node row deleted, across two versions the block index wrote;
//   - every node the run wrote carrying `visibility` and carrying `state`, read as the tuples
//     they are and not as the row's columns, after the run's last write;
//   - a `person`, a `role`, a `session` and a page node resolved through the one reader every
//     kind uses (refs.Node), each by what names it: the canonical login, the topic token, the
//     session id, and the role the page hangs under;
//   - the backlink arms map against the kinds that reader declares, in both directions, and the
//     refusal of a count for a kind the map does not declare;
//   - the tuple table read through the column list `graph_edges` declares, mapped by position,
//     one rendered row of the view compared column by column with the tuple under it;
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

	// Its attribute values are tuples, and every tuple the write appended carries that event as its
	// cause, attributes and relations alike.
	for attribute, want := range map[string]string{
		"visibility": graphstore.VisibilityProject, "state": graphstore.StateLive,
	} {
		value, ok, err := graphstore.CurrentValue(ctx, database.Pool, "comment", written.ID, attribute)
		if err != nil || !ok || value != want {
			t.Fatalf("the node's %s reads %q (held %t, err %v), want %q", attribute, value, ok, err, want)
		}
	}
	// The tuples are read straight out of graph_tuples by the transaction that appended them, with
	// no `to_kind` filter. graphstore.CurrentOut binds to_kind <> 'value', so a read through it
	// holds relations alone, which is how a comment whose `visibility` and `state` tuples carried
	// no cause passed this check on 2026-11-12.
	tuples := tuplesTheWriteAppended(t, database, "comment", written.ID)
	commentAttributes := map[string]bool{}
	commentRelations := 0
	for _, tuple := range tuples {
		baseAttribute := tuple.ToKind == graphstore.KindValue &&
			(tuple.Kind == "visibility" || tuple.Kind == "state")
		switch {
		case baseAttribute && tuple.SourceEvent == graphstore.NoEvent:
			t.Errorf("the %s base-attribute tuple of %s %s reads cause 0, the sentinel, and not the write's own event %d",
				tuple.Kind, tuple.FromKind, tuple.FromID, event)
		case tuple.SourceEvent != event:
			t.Errorf("the %s tuple of %s %s (%s %q) carries cause %d, want the write's own event %d",
				tuple.Kind, tuple.FromKind, tuple.FromID, tuple.ToKind, tuple.ToID, tuple.SourceEvent, event)
		}
		if !tuple.At.Equal(written.CreatedAt) {
			t.Errorf("the %s tuple of %s %s is stated at %s, and the record at %s",
				tuple.Kind, tuple.FromKind, tuple.FromID, tuple.At, written.CreatedAt)
		}
		// The author's node the write made states its own base attributes, held to the cause
		// above and counted here for nothing.
		if tuple.FromKind != "comment" || tuple.FromID != written.ID {
			continue
		}
		if baseAttribute {
			commentAttributes[tuple.Kind] = true
		} else if tuple.ToKind != graphstore.KindValue {
			commentRelations++
		}
	}
	// Both sorts must be among what was read, or the loop above held nothing of the missing one.
	if !commentAttributes["visibility"] || !commentAttributes["state"] || commentRelations == 0 {
		t.Fatalf("of the %d tuples the write appended, the comment's visibility is among them %t, its state %t, and %d are relations of it: want both base attributes and a relation",
			len(tuples), commentAttributes["visibility"], commentAttributes["state"], commentRelations)
	}
	if authored := tupleTo(tuples, graphstore.TupleAuthoredBy); authored != "session:"+writer {
		t.Fatalf("the comment is authored by %q, want the session that wrote it", authored)
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

	// A deleted block's node stays, with its state moved to gone by a tuple appended after the
	// live one, and the drop deletes no node row.
	requireADeletedBlockStaysGone(t, database, issue.PrimaryArtifactID)

	// Every node the run wrote carries both base attributes, as the node row's columns and as
	// the tuples R8 makes them. graphstore.Write states them for each node its insert made, so
	// every writer states them alike - the comment path, the arms the publisher's record
	// splices in, and refs.IndexBlocks' block nodes - and a node written again unchanged states
	// neither, which is what keeps a re-index of a document nothing moved appending nothing.
	// The loop runs after the last write of the run, so the person, the role, the session, the
	// page and the blocks the index wrote are among the nodes it reads, the gone block too.
	nodes := writtenNodes(t, database)
	for _, kind := range []string{graphstore.KindPerson, graphstore.KindRole, graphstore.KindSession,
		graphstore.KindPage, "comment", "block"} {
		if !slices.ContainsFunc(nodes, func(node graphstore.Node) bool { return node.Kind == kind }) {
			t.Fatalf("the run wrote no %s node, so the loop below reads none of that kind", kind)
		}
	}
	for _, held := range nodes {
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

	// The backlink arms map against the kinds that reader declares, in both directions: every
	// kind it resolves has an entry, and a kind the map does not declare is counted for nothing.
	// The kinds are the registry's own list and not a list written here, so a kind the reader
	// learns to resolve and the map never learns to count fails this loop the day it is added.
	// The reader resolves no node of a supertag or an attribute (refs' unresolvedKind), so the
	// two are the registry's kinds no count is owed for.
	counted := refs.BacklinkKinds()
	resolved := 0
	for _, kind := range refs.VisibilityKinds() {
		if kind == "supertag" || kind == "attribute" {
			continue
		}
		resolved++
		if !slices.Contains(counted, kind) {
			t.Errorf("the reader resolves %s and the backlink arms map declares no entry for it", kind)
		}
	}
	if resolved == 0 {
		t.Fatal("the registry declares no kind the reader resolves, so the loop above held nothing")
	}
	for _, kind := range counted {
		if kind == "supertag" || kind == "attribute" || !slices.Contains(refs.VisibilityKinds(), kind) {
			t.Errorf("the backlink arms map declares %s, which the reader resolves no node of", kind)
		}
	}
	// A kind the map does not declare gets no count at all: the count refuses it rather than
	// answering the mention arms alone, which would be a count silently low.
	if counts, err := refs.BacklinkCounts(ctx, database.Pool, model.Actor{Kind: "user", ID: "alice"},
		"supertag", []string{"block"}, nil); err == nil {
		t.Errorf("a backlink count of the undeclared kind supertag answered %v and no refusal", counts)
	}

	// The tuple table read through the column list the view declares, mapped by position.
	requireViewColumnsOverTuples(t, database, written.ID)

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

// tuplesTheWriteAppended is every tuple the transaction that made one node appended, of either
// sort, read straight out of graph_tuples with no `to_kind` filter. The transaction is named by
// the xmin of the node's first tuple: a node's first fact is stated by the write that made it,
// and a tuple row is never updated, so its xmin stays the transaction that appended it.
func tuplesTheWriteAppended(t *testing.T, database *store.Store, kind, id string) []graphstore.Tuple {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select from_kind, from_id, kind, to_kind, to_id, at, source_event
		from graph_tuples
		where xmin::text = (
			select first.xmin::text from graph_tuples first
			where first.from_kind = $1 and first.from_id = $2
			order by first.seq
			limit 1
		)
		order by seq
	`, kind, id)
	if err != nil {
		t.Fatalf("read the tuples the write of %s %s appended: %v", kind, id, err)
	}
	defer rows.Close()
	var tuples []graphstore.Tuple
	for rows.Next() {
		var tuple graphstore.Tuple
		if err := rows.Scan(&tuple.FromKind, &tuple.FromID, &tuple.Kind, &tuple.ToKind, &tuple.ToID,
			&tuple.At, &tuple.SourceEvent); err != nil {
			t.Fatalf("read a tuple the write of %s %s appended: %v", kind, id, err)
		}
		t.Logf("the write appended %s %s %s -> %s %q, cause %d",
			tuple.FromKind, tuple.FromID, tuple.Kind, tuple.ToKind, tuple.ToID, tuple.SourceEvent)
		tuples = append(tuples, tuple)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tuples the write of %s %s appended: %v", kind, id, err)
	}
	if len(tuples) == 0 {
		t.Fatalf("the write stated nothing about the %s %s it stored", kind, id)
	}
	return tuples
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

// writtenNodes is every node the store holds, whatever its state, which in this run is every
// node it wrote: a block the index stated gone is a node the run wrote as much as a live one.
func writtenNodes(t *testing.T, database *store.Store) []graphstore.Node {
	t.Helper()
	var nodes []graphstore.Node
	rows, err := database.Pool.Query(context.Background(), `select distinct kind from graph_nodes`)
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

// requireADeletedBlockStaysGone indexes one document twice through the block index, the second
// version dropping one of its two blocks, and reads what the drop left: the dropped block's node
// row is still there, no node row of the store was deleted, and its state reads gone through a
// tuple appended after the one that stated it live, rather than through a row rewritten.
func requireADeletedBlockStaysGone(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	ctx := context.Background()
	var refKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`,
		artifactID).Scan(&refKey); err != nil {
		t.Fatalf("read the document's address: %v", err)
	}
	const keptText, droppedText = "A block both versions hold.", "A block the second version drops."
	both := "# Kept\n\n" + keptText + "\n\n" + droppedText + "\n"
	block := func(markdown, id, text string) model.ArtifactBlock {
		from := strings.Index(markdown, text)
		return model.ArtifactBlock{ID: id, Type: "paragraph", From: from, To: from + len(text)}
	}
	// The versions stand above any the document service writes for the spec, so a pass of its
	// own at version 1 loses the fence to these rather than racing them.
	index := func(version int, markdown string, blocks ...model.ArtifactBlock) {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the index of version %d: %v", version, err)
		}
		defer tx.Rollback(ctx)
		_, won, written, err := refs.IndexBlocks(ctx, tx, artifactID, version, time.Now().UTC(), markdown, blocks, nil, nil)
		if err != nil {
			t.Fatalf("index version %d: %v", version, err)
		}
		if !won {
			t.Fatalf("the index of version %d lost the fence, so it wrote nothing", version)
		}
		if err := written.Caused(ctx, tx, graphstore.NoEvent); err != nil {
			t.Fatalf("append what version %d stated: %v", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the index of version %d: %v", version, err)
		}
	}
	countNodes := func() int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from graph_nodes`).Scan(&count); err != nil {
			t.Fatalf("count the store's nodes: %v", err)
		}
		return count
	}
	index(900, both, block(both, "kept", keptText), block(both, "dropped", droppedText))
	before := countNodes()
	kept := "# Kept\n\n" + keptText + "\n"
	index(901, kept, block(kept, "kept", keptText))
	if after := countNodes(); after != before {
		t.Errorf("the store held %d nodes before the version that dropped a block and %d after it, want the same: a drop deletes no node row",
			before, after)
	}

	dropped := refKey + "#dropped"
	node, held, err := graphstore.ReadNode(ctx, database.Pool, "block", dropped)
	if err != nil {
		t.Fatalf("read the dropped block's node: %v", err)
	}
	if !held {
		t.Fatalf("the dropped block %s has no node row: the drop deleted it rather than stating it gone", dropped)
	}
	if node.State != graphstore.StateGone {
		t.Errorf("the dropped block's node row reads state %q, want %q", node.State, graphstore.StateGone)
	}
	if state, ok, err := graphstore.CurrentValue(ctx, database.Pool, "block", dropped, "state"); err != nil {
		t.Fatalf("read the dropped block's state: %v", err)
	} else if !ok || state != graphstore.StateGone {
		t.Errorf("the dropped block's state tuple reads %q (held %t), want %q", state, ok, graphstore.StateGone)
	}
	// The whole history of the attribute, in the order it was written: the live the first
	// version stated, and then the gone the second appended beside it.
	history, err := graphstore.History(ctx, database.Pool, "block", dropped, "state")
	if err != nil {
		t.Fatalf("read the dropped block's state history: %v", err)
	}
	var states []string
	for _, tuple := range history {
		states = append(states, tuple.ToID)
	}
	if want := []string{graphstore.StateLive, graphstore.StateGone}; !slices.Equal(states, want) {
		t.Errorf("the dropped block's state history reads %v, want %v: the gone appended after the live", states, want)
	}
	if state, ok, err := graphstore.CurrentValue(ctx, database.Pool, "block", refKey+"#kept", "state"); err != nil {
		t.Fatalf("read the kept block's state: %v", err)
	} else if !ok || state != graphstore.StateLive {
		t.Errorf("the kept block's state reads %q (held %t), want %q", state, ok, graphstore.StateLive)
	}
}

// tupleUnderTheView is the column of graph_tuples each column of graph_edges renders, in the
// order the view declares its columns: the view's created_at is the tuple's at, and its
// source_seq is the tuple's legacy_source_seq.
var tupleUnderTheView = []string{"from_kind", "from_id", "kind", "to_kind", "to_id", "at", "legacy_source_seq"}

// requireViewColumnsOverTuples reads graph_edges' own column list and holds every column of it
// against the column of graph_tuples under it, by position and not word for word.
//
// The names alone cannot show two columns of the tuple arm selected out of order: a union takes
// its column names from the view's own list and its values by position, so an arm selecting
// from_id where from_kind stands renders a row whose names still read right. One row of that
// arm is read too, then, and compared column by column with the tuple it renders. The arm
// answers only once the cutover rows read true, so they are set true in a transaction that is
// rolled back, beside a tuple of the comment this run wrote whose every column holds a value of
// its own, a legacy_source_seq among them.
func requireViewColumnsOverTuples(t *testing.T, database *store.Store, comment string) {
	t.Helper()
	ctx := context.Background()
	view := relationColumns(t, database, "graph_edges")
	tuple := relationColumns(t, database, "graph_tuples")
	if len(view) != len(tupleUnderTheView) {
		t.Fatalf("graph_edges declares %v, and %v are the tuple columns under it: a column of the view has no column of the tuple under it",
			view, tupleUnderTheView)
	}
	for position, column := range tupleUnderTheView {
		if !slices.Contains(tuple, column) {
			t.Fatalf("graph_edges declares %s and graph_tuples holds no %s under it", view[position], column)
		}
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read of the tuple arm: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `update graph_store_cutover set complete = true`); err != nil {
		t.Fatalf("set the cutover rows: %v", err)
	}
	const target = "POSITION-1/spec"
	var seq int64
	if err := tx.QueryRow(ctx, `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
		values ('comment', $1, 'anchored_to', 'artifact', $2, '2001-02-03 04:05:06.789+00', '{}'::jsonb, 0, 424242, true)
		returning seq
	`, comment, target).Scan(&seq); err != nil {
		t.Fatalf("append the tuple the arm renders: %v", err)
	}
	// The row is found by the one value no other edge holds, in whichever text column the arm
	// put it, so an arm that moved a column still answers its row for the comparison below.
	rendered, names := oneRow(t, tx, `
		select * from graph_edges where $1 in (from_kind, from_id, kind, to_kind, to_id)
	`, target)
	if !slices.Equal(names, view) {
		t.Fatalf("a row of graph_edges names its columns %v, and the view declares %v", names, view)
	}
	stated, _ := oneRow(t, tx, `select `+strings.Join(tupleUnderTheView, ", ")+` from graph_tuples where seq = $1`, seq)
	for position, column := range view {
		if !sameColumnValue(rendered[position], stated[position]) {
			t.Errorf("graph_edges' %s (column %d) renders %v, and the tuple's %s under it holds %v",
				column, position+1, rendered[position], tupleUnderTheView[position], stated[position])
		}
	}
}

// oneRow reads the one row a statement answers, as its values in column order with the name of
// each column.
func oneRow(t *testing.T, tx pgx.Tx, sql string, args ...any) ([]any, []string) {
	t.Helper()
	rows, err := tx.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("read %s: %v", strings.Join(strings.Fields(sql), " "), err)
	}
	defer rows.Close()
	var values [][]any
	for rows.Next() {
		row, err := rows.Values()
		if err != nil {
			t.Fatalf("read a row of %s: %v", strings.Join(strings.Fields(sql), " "), err)
		}
		values = append(values, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s: %v", strings.Join(strings.Fields(sql), " "), err)
	}
	if len(values) != 1 {
		t.Fatalf("%s answered %d rows, want one", strings.Join(strings.Fields(sql), " "), len(values))
	}
	names := make([]string, 0, len(rows.FieldDescriptions()))
	for _, field := range rows.FieldDescriptions() {
		names = append(names, field.Name)
	}
	return values[0], names
}

// sameColumnValue compares two column values read back from Postgres: two times by the instant
// they name, anything else by value.
func sameColumnValue(a, b any) bool {
	if at, isTime := a.(time.Time); isTime {
		bt, also := b.(time.Time)
		return also && at.Equal(bt)
	}
	return reflect.DeepEqual(a, b)
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

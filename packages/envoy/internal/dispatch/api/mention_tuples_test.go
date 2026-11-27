package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// mentionedPerson is the person the mention cases name, by a login the person address takes
// (dispatch://person/<login>, text's personLoginPattern).
const mentionedPerson = "person-1@example.com"

// The mention part of c-mention-tuples (R5). Three comments each append exactly one mentions tuple,
// from the comment's node to the identity's: a person their body names, a session and a role their
// writer names as delivery routes, the role one no session holds, so its tuple stands alone. A
// role whose holder is a session appends a second, to that session, and a comment that names
// nobody appends none; an issue message and a document block that name a person each append one,
// from the message's node and the block's. No mention is a node. The urgency is a value of the
// comment, under the attribute the comment supertag declares for it, and comment_mentions holds
// what the tuples state, row for row. The reconciliation an older build makes of the same sources -
// replaceRefs' delete scoped by the source, replaceBlockRefs' by the document - leaves every tuple
// standing at its own moment; each source it deletes for cites an issue as well, so each delete
// removes that citation's row, as the older build's delete does, and the trigger every delete from
// refs runs (refs_forms_owe_deleted) runs over a row. Indexing the unchanged text of each of those
// sources again - both comments, the issue message and the spec - neither repeats one nor dates one
// again. The count beside each identity's list is the rows that list holds (refs.BacklinkCounts).
// Last, a comment naming one session alone, which nothing else of the comment reaches and whose
// mention fails, tells that session's own topic of it once.
func mentionsAreTuples(t *testing.T) {
	ctx := context.Background()
	listener, state := newMentionListener(t)
	// Never released: the send to s2 waits until its caller gives up, and its attempt fails.
	_ = state.hold("s2")
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Second})
	issue := createInteractionIssue(t, handler, "MENT", "Mentions", "before")
	post := func(input map[string]any) string {
		t.Helper()
		return decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, input)).ID
	}
	created := func(id string) int64 { return writeEventOf(t, database, "comment.created", id) }

	// Each comment, the issue message and the spec the reconciliation below deletes for cites the
	// issue too: refs and block_refs hold no row naming a person, a session or a role
	// (refs.TargetOf), so a delete scoped by a source that cited nothing else would remove nothing.
	cites := " It is for dispatch://" + issue.Key + "."
	personBody := "Over to dispatch://person/" + mentionedPerson + " for the copy." + cites
	person := post(map[string]any{"body": personBody})
	sessionBody := "Are you there?" + cites
	session := post(map[string]any{"body": sessionBody, "mentions": []map[string]any{{"target": "session:s1"}}})
	role := post(map[string]any{"body": "Is the role held?", "mentions": []map[string]any{{"target": "role:unheld"}}})
	forwarded := post(map[string]any{"body": "Can you review it?", "mentions": []map[string]any{{"target": "role:reviewer"}}})
	nobody := post(map[string]any{"body": "Nobody is named here."})
	requireRelationTuples(t, database, graphstore.TupleMentions, "comment:"+person,
		fmt.Sprintf("comment:%s > person:%s @%d", person, mentionedPerson, created(person)),
		fmt.Sprintf("comment:%s > issue:%s @%d", person, issue.Key, created(person)))
	requireRelationTuples(t, database, graphstore.TupleMentions, "comment:"+session,
		fmt.Sprintf("comment:%s > session:s1 @%d", session, created(session)),
		fmt.Sprintf("comment:%s > issue:%s @%d", session, issue.Key, created(session)))
	requireRelationTuples(t, database, graphstore.TupleMentions, "comment:"+role,
		fmt.Sprintf("comment:%s > role:unheld @%d", role, created(role)))
	forwardedAt := created(forwarded)
	requireRelationTuples(t, database, graphstore.TupleMentions, "comment:"+forwarded,
		fmt.Sprintf("comment:%s > role:reviewer @%d", forwarded, forwardedAt),
		fmt.Sprintf("comment:%s > session:s1 @%d", forwarded, forwardedAt))
	requireRelationTuples(t, database, graphstore.TupleMentions, "comment:"+nobody)

	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Ask dispatch://person/" + mentionedPerson + " about the copy." + cites}, "alice")
	messageCreated := writeEventOf(t, database, "message.created", message.ID)
	requireRelationTuples(t, database, graphstore.TupleMentions, "message:"+message.ID,
		fmt.Sprintf("message:%s > person:%s @%d", message.ID, mentionedPerson, messageCreated),
		fmt.Sprintf("message:%s > issue:%s @%d", message.ID, issue.Key, messageCreated))
	// A document names a person, a session and a role by a mention's link each, whose address its
	// rendering keeps as written.
	document := createInteractionIssue(t, handler, "MEND", "A spec that names a person", "Ask [@Person](dispatch://person/"+mentionedPerson+
		") about the copy, and tell [@Agent](dispatch://agent/s1) and [@Reviewer](dispatch://role/reviewer)."+cites+"\n")
	// The spec's blocks are the block index's to write, which the reconciliation's tick runs.
	reconcileBlockIndexPass(t, deps)
	blocks, err := readStrings(ctx, database.Pool, `
		select t.from_kind || ':' || t.from_id from graph_tuples t
		join document_blocks b on b.node_kind = t.from_kind and b.node_address = t.from_id
		where b.artifact_id = $1::uuid and t.kind = 'mentions' and t.to_kind = 'person' and t.to_id = $2
	`, document.PrimaryArtifactID, mentionedPerson)
	if err != nil {
		t.Fatalf("read the block mentions of the spec: %v", err)
	}
	if len(blocks) != 1 {
		var markdown string
		var rows int
		if err := database.Pool.QueryRow(ctx, `
			select coalesce((select markdown from artifact_versions where artifact_id = $1::uuid order by number desc limit 1), ''),
			       (select count(*) from document_blocks where artifact_id = $1::uuid and node_kind is not null)
		`, document.PrimaryArtifactID).Scan(&markdown, &rows); err != nil {
			t.Fatalf("read the spec's text and blocks: %v", err)
		}
		t.Fatalf("the spec naming %s holds block mentions %v of them, want one, out of the block that names them (%d block rows naming a node, text %q)",
			mentionedPerson, blocks, rows, markdown)
	}

	var mentionNodes int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_nodes where kind = 'mention'`).Scan(&mentionNodes); err != nil {
		t.Fatalf("count mention nodes: %v", err)
	}
	if mentionNodes != 0 {
		t.Errorf("the store holds %d mention nodes, want none: a mention is a tuple", mentionNodes)
	}

	// The urgency: the one mode the comment applied to every target, a value of its node, stored
	// under the attribute the comment supertag declares.
	modes, err := readStrings(ctx, database.Pool, `
		select to_id from graph_tuples where from_kind = 'comment' and from_id = $1 and kind = $2 and to_kind = 'value' and asserted
	`, forwarded, graphstore.TupleDelivery)
	if err != nil {
		t.Fatalf("read the comment's urgency: %v", err)
	}
	if !slices.Equal(modes, []string{"steer"}) {
		t.Errorf("the comment's urgency is %v, want the one value steer", modes)
	}
	var declared int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples t join graph_nodes a on a.kind = t.from_kind and a.id = t.from_id
		where t.from_kind = 'attribute' and t.kind = 'attribute_of' and t.to_kind = 'supertag' and t.to_id = 'comment'
		  and a.attributes->>'store_name' = $1
	`, graphstore.TupleDelivery).Scan(&declared); err != nil {
		t.Fatalf("read the comment supertag's attributes: %v", err)
	}
	if declared != 1 {
		t.Errorf("the comment supertag declares %d attributes stored as %q, want one", declared, graphstore.TupleDelivery)
	}

	// comment_mentions is the projection of the tuples: each row marked, and each of its fields a
	// tuple of its comment's node.
	for _, comment := range []string{session, role, forwarded} {
		requireMentionRowsProjectTheTuples(t, database, comment)
	}

	// The reconciliation an older build runs over the same sources deletes their refs and
	// block_refs rows, and no mention tuple with them.
	before := identityMentionTuples(t, database)
	for _, source := range []struct{ kind, id string }{{"comment", person}, {"comment", session}, {"message", message.ID}} {
		deleted, err := database.Pool.Exec(ctx, `delete from refs where from_kind = $1 and from_id = $2`, source.kind, source.id)
		if err != nil {
			t.Fatalf("delete the refs of %s %s as replaceRefs does: %v", source.kind, source.id, err)
		}
		if deleted.RowsAffected() == 0 {
			t.Fatalf("the delete of the refs of %s %s as replaceRefs does removed no row: its citation of %s holds none, so the reconciliation reconciles nothing", source.kind, source.id, issue.Key)
		}
	}
	deleted, err := database.Pool.Exec(ctx, `delete from block_refs where artifact_id = $1::uuid`, document.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("delete the block_refs of the spec as replaceBlockRefs does: %v", err)
	}
	if deleted.RowsAffected() == 0 {
		t.Fatalf("the delete of the spec's block_refs as replaceBlockRefs does removed no row: its citation of %s holds none, so the reconciliation reconciles nothing", issue.Key)
	}
	if after := identityMentionTuples(t, database); !slices.Equal(after, before) {
		t.Errorf("after an older build's reconciliation the identity mentions are\n  %v\nwant them standing as they were\n  %v", after, before)
	}
	// Indexing the unchanged text of each source again neither repeats a tuple nor dates one
	// again, and each index puts back the citation the delete removed: each comment through its
	// edit to its own text, the issue message through the re-index of the row the delete owed it
	// (refs_forms_owe_deleted, drained by refs.ReindexOwed), and the spec through the block index
	// of its latest version, owed it again as the forms drain owes it.
	indexedAgain := func(what, citations string, args ...any) {
		t.Helper()
		var cited int
		if err := database.Pool.QueryRow(ctx, citations, args...).Scan(&cited); err != nil {
			t.Fatalf("count the citations of %s: %v", what, err)
		}
		if cited == 0 {
			t.Fatalf("%s holds no citation of %s after it was indexed again, so it was not indexed", what, issue.Key)
		}
		if again := identityMentionTuples(t, database); !slices.Equal(again, before) {
			t.Errorf("after %s was indexed again with its unchanged text the identity mentions are\n  %v\nwant\n  %v", what, again, before)
		}
	}
	refsOf := `select count(*) from refs where from_kind = $1 and from_id = $2`
	for _, comment := range []struct{ what, id, body string }{
		{"the comment naming the person", person, personBody}, {"the comment naming the session", session, sessionBody},
	} {
		if edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.id, map[string]any{
			"body": comment.body,
		}, "alice"); edited.Code != http.StatusOK {
			t.Fatalf("edit %s to its own text: status=%d body=%s", comment.what, edited.Code, edited.Body.String())
		}
		indexedAgain(comment.what, refsOf, graphstore.KindComment, comment.id)
	}
	owed, err := refs.ListOwed(ctx, database.Pool, refs.RefsFormsOwed, 100)
	if err != nil {
		t.Fatalf("list the texts the deletes owed an index: %v", err)
	}
	messageOwed := slices.IndexFunc(owed, func(row refs.Owed) bool { return row.Kind == graphstore.KindMessage && row.ID == message.ID })
	if messageOwed < 0 {
		t.Fatalf("the delete of the issue message's refs owed it no index: refs_forms_owed holds %+v", owed)
	}
	if indexed, err := refs.ReindexOwed(ctx, database.Pool, owed[messageOwed], deps.ServerURL); err != nil || !indexed {
		t.Fatalf("index the issue message again = %v (%v), want it indexed", indexed, err)
	}
	indexedAgain("the issue message", refsOf, graphstore.KindMessage, message.ID)
	if _, err := database.Pool.Exec(ctx, `update document_block_versions set version = null where artifact_id = $1::uuid`, document.PrimaryArtifactID); err != nil {
		t.Fatalf("owe the spec a block index: %v", err)
	}
	if tick := reconcileBlockIndexPass(t, deps); tick.Documents == 0 {
		t.Fatalf("the block index owed the spec indexed no document: %+v", tick)
	}
	indexedAgain("the spec", `select count(*) from block_refs where artifact_id = $1::uuid`, document.PrimaryArtifactID)

	// The count beside each identity's list is the rows that list holds, for a person, a session and
	// a role, each named from a comment and from a document block.
	reader := model.Actor{Kind: "user", ID: "alice"}
	for _, identity := range []struct {
		kind, id string
		rows     int
		sources  []string
	}{
		{graphstore.KindPerson, mentionedPerson, 3, []string{"block", "comment", "message"}},
		{graphstore.KindSession, "s1", 3, []string{"block", "comment"}},
		{graphstore.KindRole, "reviewer", 2, []string{"block", "comment"}},
		{graphstore.KindRole, "unheld", 1, []string{"comment"}},
	} {
		var listed int
		var sources []string
		if err := database.Pool.QueryRow(ctx, `
			select count(*), coalesce(array_agg(distinct from_kind order by from_kind), '{}')
			from graph_edges where to_kind = $1 and to_id = $2
		`, identity.kind, identity.id).Scan(&listed, &sources); err != nil {
			t.Fatalf("list the edges into %s:%s: %v", identity.kind, identity.id, err)
		}
		counts, err := refs.BacklinkCounts(ctx, database.Pool, reader, identity.kind, []string{identity.id}, nil)
		if err != nil {
			t.Fatalf("count the edges into %s:%s: %v", identity.kind, identity.id, err)
		}
		if listed != identity.rows || counts[identity.id] != listed || !slices.Equal(sources, identity.sources) {
			t.Errorf("%s:%s lists %d rows out of %v and counts %d, want %d of each out of %v",
				identity.kind, identity.id, listed, sources, counts[identity.id], identity.rows, identity.sources)
		}
	}

	// A comment naming one session alone, which is no claimant, follower, author or primary
	// document's author, whose mention fails: the session's own topic is among the destinations
	// the batch recorded, once.
	alone := post(map[string]any{"body": "Are you there?", "mentions": []map[string]any{{"target": "session:s2"}}})
	if settled := settledCommentDeliveries(t, database, alone); len(settled) != 1 || settled[0].State != model.DeliveryStateFailed {
		t.Fatalf("the mention of s2, whose send never answers, = %#v, want its one attempt failed", settled)
	}
	runOutboxUntilSettled(t, deps, &recordingOutboxPublisher{}, issue.Key, alone)
	if destinations := publishedDestinationsOf(t, database, "comment.created", alone); countOf(destinations, contracts.LegacyAgentSubject("s2")) != 1 {
		t.Errorf("the comment naming s2 alone was published to %v, want s2's own topic among them once", destinations)
	}
}

// identityMentionTuples is every mentions tuple the store holds to a person, a session or a role,
// as `from > to @at`, its moment to the microsecond, sorted.
func identityMentionTuples(t *testing.T, database *store.Store) []string {
	t.Helper()
	held, err := readStrings(context.Background(), database.Pool, `
		select from_kind || ':' || from_id || ' > ' || to_kind || ':' || to_id || ' @'
		       || to_char(at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US')
		from graph_tuples where kind = 'mentions' and to_kind = any($1) order by 1
	`, refs.IdentityKinds)
	if err != nil {
		t.Fatalf("read the identity mentions: %v", err)
	}
	return held
}

// requireMentionRowsProjectTheTuples holds each comment_mentions row of comment to the tuples of
// the comment's node: the row is marked, its target is a mentions tuple, the session it resolved
// to is one too, and its delivery is the comment's urgency.
func requireMentionRowsProjectTheTuples(t *testing.T, database *store.Store, comment string) {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select target, resolved_session_id, delivery, tuples_stored from comment_mentions where comment_id = $1::uuid
	`, comment)
	if err != nil {
		t.Fatalf("read the comment_mentions rows of %s: %v", comment, err)
	}
	type mentionRow struct {
		target, delivery string
		resolved         *string
		stored           bool
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (mentionRow, error) {
		var read mentionRow
		return read, row.Scan(&read.target, &read.resolved, &read.delivery, &read.stored)
	})
	if err != nil {
		t.Fatalf("read the comment_mentions rows of %s: %v", comment, err)
	}
	if len(held) == 0 {
		t.Fatalf("comment %s holds no comment_mentions row", comment)
	}
	stated := func(kind, toKind, toID string) bool {
		var count int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from graph_tuples
			where from_kind = 'comment' and from_id = $1 and kind = $2 and to_kind = $3 and to_id = $4 and asserted
		`, comment, kind, toKind, toID).Scan(&count); err != nil {
			t.Fatalf("read the %s tuples of %s: %v", kind, comment, err)
		}
		return count == 1
	}
	for _, row := range held {
		route, err := model.ParseRoute(row.target)
		if err != nil {
			t.Fatalf("comment %s's mention row names %q, no route: %v", comment, row.target, err)
		}
		if !row.stored {
			t.Errorf("comment %s's mention of %s holds no tuples_stored", comment, row.target)
		}
		if !stated(graphstore.TupleMentions, route.Kind, route.ID) {
			t.Errorf("comment %s's mention row of %s has no mentions tuple to it", comment, row.target)
		}
		if row.resolved != nil && !stated(graphstore.TupleMentions, graphstore.KindSession, *row.resolved) {
			t.Errorf("comment %s's mention of %s resolved to %s, which no mentions tuple names", comment, row.target, *row.resolved)
		}
		if !stated(graphstore.TupleDelivery, graphstore.KindValue, row.delivery) {
			t.Errorf("comment %s's mention of %s is delivered %s, which is not the comment's urgency", comment, row.target, row.delivery)
		}
	}
}

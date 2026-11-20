package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestAPersonPageAnswersOnlyWhereThePersonHoldsANodeTheReaderMaySee holds the person page to R15
// of this change: GET /api/v1/people/{login}/nodes answers 200 only where that person
// holds at least one node the reader may see, or the reader is that person, and otherwise, byte
// for byte, what a login nobody holds answers.
//
// This release writes no node reserved for its author, and a later one does; this release serves
// beside that one during a roll and alone after a rollback. So carol's only node, a comment
// reserved for her, is written by SQL as a later release writes it (seedLaterAuthorOnlyComment),
// with its authored_by tuple to carol's person node. bob reads her page as a login nobody holds,
// and carol reads it 200, listing the comment.
func TestAPersonPageAnswersOnlyWhereThePersonHoldsANodeTheReaderMaySee(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "PAGE", "A person's page", "# Spec\n\nA passage.\n")
	const first, second, nobodyLogin = "bob", "carol", "nobody-at-all"
	comment := seedLaterAuthorOnlyComment(t, database, issue.Key, second, "A remark carol keeps.")
	page := func(person, reader string) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/"+person+"/nodes", nil, reader)
	}

	nobody := page(nobodyLogin, first)
	if nobody.Code != http.StatusNotFound {
		t.Fatalf("a login nobody holds answers %d %s, want 404, so the comparison proves nothing", nobody.Code, nobody.Body.String())
	}
	if _, found, err := graphstore.ReadNode(context.Background(), database.Pool, graphstore.KindPerson, second); err != nil || !found {
		t.Fatalf("the store holds no person node of %s (%v), so a page answered by the person node alone would be a 404 too", second, err)
	}
	hidden := page(second, first)
	// The answer for a login nobody holds, with that login spelled as the second person's
	// wherever it appears, is the whole of what the second person's page may answer.
	want := strings.ReplaceAll(nobody.Body.String(), nobodyLogin, second)
	t.Logf("%s reads %s's page: %d %s; and a login nobody holds: %d %s",
		first, second, hidden.Code, hidden.Body.String(), nobody.Code, nobody.Body.String())
	if hidden.Code != nobody.Code || hidden.Body.String() != want ||
		hidden.Header().Get("Content-Type") != nobody.Header().Get("Content-Type") {
		t.Errorf("%s reads the page of %s, whose only node is reserved for %s, as %d %q (%s), and a login nobody holds as %d %q (%s)",
			first, second, second, hidden.Code, hidden.Body.String(), hidden.Header().Get("Content-Type"),
			nobody.Code, want, nobody.Header().Get("Content-Type"))
	}

	own := page(second, second)
	if own.Code != http.StatusOK {
		t.Fatalf("%s reads her own page as %d %s, want 200", second, own.Code, own.Body.String())
	}
	if read := decodeBody[personNodes](t, own); read.Person != second || read.Total != 1 ||
		len(read.Rows) != 1 || read.Rows[0].ID != comment {
		t.Errorf("%s's own page = %+v, want the one comment %s she keeps", second, read, comment)
	}
}

// A row of a person's page carries, beside its first line, that line read as the tree a reader is
// shown (first_line_tree), under `Dispatch-Trees: 1` and only there: the tree
// pmdoc.ReadForDisplay makes of exactly the line the row answers, the first line of the node's
// text and not the rest. The route table's 200 schema declares it.
func TestAPersonPageRowCarriesTheTreeOfItsFirstLine(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "LINE", "A person's page lines", "Spec.\n")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": "A **bold** first line.\n\nA second paragraph the line leaves out."}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("seed a comment: status=%d body=%s", created.Code, created.Body.String())
	}
	const page = "/api/v1/people/alice/nodes"
	if line, _ := valueAt(t, treesRequest(t, handler, page, false), "rows.0.first_line"); string(line) != `"A **bold** first line."` {
		t.Fatalf("GET %s: the newest row's first line is %s, want the comment's", page, line)
	}
	requireDisplayTree(t, handler, page, "rows.0.first_line", "rows.0.first_line_tree")
	node, _ := readOpenAPITree(t)
	document, _ := node.(map[string]any)
	requireTreeInAnswerSchema(t, document, "/api/v1/people/{login}/nodes", "rows.0.first_line_tree")
}

// seedLaterAuthorOnlyComment writes, by SQL, a comment on issue reserved for its author login, as
// a later release writes one: the comment's node at visibility author and the author's person
// node, each with the two base attributes the store states of a node its insert makes
// (graphstore.BaseAttributeFacts), the authored_by tuple from the comment to the person, and the
// comment's row naming its node. It answers the comment's id.
func seedLaterAuthorOnlyComment(t *testing.T, database *store.Store, issue, login, body string) string {
	t.Helper()
	ctx := context.Background()
	author := model.Actor{Kind: "user", ID: login}
	recorded, err := json.Marshal(author.Recorded())
	if err != nil {
		t.Fatalf("encode the comment's author: %v", err)
	}
	at := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	var id string
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&id); err != nil {
			return err
		}
		nodes := []graphstore.Node{
			{Kind: graphstore.KindComment, ID: id, Supertag: graphstore.KindComment,
				Visibility: graphstore.VisibilityAuthor, State: graphstore.StateLive},
			{Kind: graphstore.KindPerson, ID: login, Supertag: graphstore.KindPerson,
				Visibility: graphstore.VisibilityProject, State: graphstore.StateLive},
		}
		for _, node := range nodes {
			if _, err := tx.Exec(ctx, `
				insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
				values ($1, $2, $3, $4, $5, '{}', $6, $7, $8)
			`, node.Kind, node.ID, node.Supertag, node.Visibility, node.State, at, recorded, graphstore.NoEvent); err != nil {
				return err
			}
			for _, attribute := range []struct{ kind, value string }{
				{graphstore.AttributeVisibility, node.Visibility}, {graphstore.AttributeState, node.State},
			} {
				if _, err := tx.Exec(ctx, `
					insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
					values ($1, $2, $3, $4, $5, $6, $7, $8, null, true)
				`, node.Kind, node.ID, attribute.kind, graphstore.KindValue, attribute.value, at, recorded, graphstore.NoEvent); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
			values ($1, $2, $3, $4, $5, $6, $7, $8, null, true)
		`, graphstore.KindComment, id, graphstore.TupleAuthoredBy, graphstore.KindPerson, login, at, recorded, graphstore.NoEvent); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into comments (id, issue_key, author, body, node_kind, node_address, created_at)
			values ($1::uuid, $2, $3, $4, $5, $1, $6)
		`, id, issue, recorded, body, graphstore.KindComment, at)
		return err
	}); err != nil {
		t.Fatalf("write %s's comment reserved for her: %v", login, err)
	}
	return id
}

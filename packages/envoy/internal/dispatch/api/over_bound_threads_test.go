package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestAWritePastTheStoresBoundRecordsItsThreadsRows holds a comment and an issue message past the
// store's address bound - a comment on a document whose reference key the store cannot key, which
// is left whole out of the store, and a reply naming a block of its parent whose node id is past
// the bound, whose row names no node - to the record their writer makes of them in their thread
// (inboxwrite's Write.Unstored), with the user_threads triggers dropped, as the root drops them,
// and with them in place:
//
//   - each row of the thread but the writer's moves, replied_at and last_reply_id naming the write
//     and its version up by one, and gives up its marker, since no delivered_to stands beside it;
//   - the writer keeps a row of their own, read through the write, and unmarked;
//   - the write's event names the writer's row beside the rows the thread held;
//   - the row says threads_recorded, so a standing trigger passes it by and the move is made once.
//
// Dropped, the triggers write nothing, so every row the test reads is the Go writer's.
func TestAWritePastTheStoresBoundRecordsItsThreadsRows(t *testing.T) {
	t.Parallel()
	for _, dropped := range []bool{true, false} {
		name := "with the user_threads triggers in place"
		if dropped {
			name = "with the user_threads triggers dropped"
		}
		t.Run(name, func(t *testing.T) {
			handler, database := newTestHandlerWithStore(t)
			if dropped {
				if _, err := database.Pool.Exec(context.Background(), `
					drop trigger user_threads_comment on comments;
					drop trigger user_threads_message on messages
				`); err != nil {
					t.Fatalf("drop the user_threads triggers: %v", err)
				}
			}
			t.Run("a comment left out of the store", func(t *testing.T) {
				requireOverBoundCommentRecorded(t, handler, database)
			})
			t.Run("a message whose row names no node", func(t *testing.T) {
				requireOverBoundMessageRecorded(t, handler, database)
			})
		})
	}
}

// requireOverBoundCommentRecorded writes a root and a reply on a project document whose reference
// key is past the store's bound, as alice and then bob, and reads the thread's rows.
func requireOverBoundCommentRecorded(t *testing.T, handler http.Handler, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx,
		`insert into projects (key, name, default_assignee) values ('OB', 'OB project', 'alice')`); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	var artifactID, refKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('OB', $1, $1, 'doc', '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, randText(maxNewArtifactKeyBytes-len("OB/"))).Scan(&artifactID, &refKey); err != nil {
		t.Fatalf("create the long-keyed document: %v", err)
	}
	if graphstore.AddressFits(refKey) {
		t.Fatalf("the document's ref_key fits the store's bound, so this proves nothing")
	}
	comments := "/api/v1/artifacts/" + artifactID + "/comments"
	root := postOverBoundComment(t, handler, comments, map[string]any{"body": "A root on a long-keyed document."}, "alice")
	requireOverBoundRow(t, database, "comments", root.ID)
	requireOwnUnmarkedRow(t, database, "alice", root.ID, root.CreatedAt)
	// alice's row as the backfill leaves it once it marks it, so the reply's move is seen to take
	// the marker off.
	if _, err := database.Pool.Exec(ctx, `
		update user_threads set marked_version = version, marked_seq = 0 where login = 'alice' and thread_id = $1::uuid
	`, root.ID); err != nil {
		t.Fatalf("mark alice's row: %v", err)
	}
	before := readThreadRow(t, database, "alice", root.ID)

	reply := postOverBoundComment(t, handler, comments, map[string]any{"body": "A reply past the bound.", "reply_to": root.ID}, "bob")
	requireOverBoundRow(t, database, "comments", reply.ID)
	requireMovedUnmarkedRow(t, database, "alice", root.ID, before.version, reply.ID, reply.CreatedAt)
	requireOwnUnmarkedRow(t, database, "bob", root.ID, reply.CreatedAt)
	requireEventNames(t, database, "comment.created", reply.ID, "alice", "bob")
}

// requireOverBoundMessageRecorded writes an issue message as alice, gives its second block a node
// id past the store's bound - as no index of this release writes one, since a block id is bounded
// at text.MaxBlockIDBytes - and has bob reply naming that block, and reads the thread's rows.
func requireOverBoundMessageRecorded(t *testing.T, handler http.Handler, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "OBM", "Messages past the bound", "The spec.\n")
	parent := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "One.\n\nTwo."}, "alice")
	long := "message/" + parent.ID + "#" + strings.Repeat("b", graphstore.MaxAddressBytes)
	if tag, err := database.Pool.Exec(ctx, `
		update message_blocks set node_id = $2 where message_id = $1::uuid and block_id = 'b2'
	`, parent.ID, long); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("give the parent's second block a node id past the bound: %v (%d rows)", err, tag.RowsAffected())
	}
	before := readThreadRow(t, database, "alice", parent.ID)

	reply := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "On two, past the bound.", "in_reply_to": parent.ID, "in_reply_to_block": "b2",
	}, "bob")
	requireOverBoundRow(t, database, "messages", reply.ID)
	requireMovedUnmarkedRow(t, database, "alice", parent.ID, before.version, reply.ID, reply.CreatedAt)
	requireOwnUnmarkedRow(t, database, "bob", parent.ID, reply.CreatedAt)
	requireEventNames(t, database, "message.answered", reply.ID, "alice", "bob")
}

// postOverBoundComment posts input to path as login and answers the comment written.
func postOverBoundComment(t *testing.T, handler http.Handler, path string, input map[string]any, login string) model.Comment {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, path, input, login)
	if response.Code != http.StatusCreated {
		t.Fatalf("post a comment past the bound: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Comment](t, response)
}

// requireOverBoundRow fails unless the row id of table names no node and says threads_recorded:
// the write past the bound this test means, which its writer recorded in its thread.
func requireOverBoundRow(t *testing.T, database *store.Store, table, id string) {
	t.Helper()
	var namesNode, recorded bool
	if err := database.Pool.QueryRow(context.Background(), `
		select node_kind is not null, threads_recorded from `+table+` where id = $1::uuid
	`, id).Scan(&namesNode, &recorded); err != nil {
		t.Fatalf("read %s %s: %v", table, id, err)
	}
	if namesNode || !recorded {
		t.Fatalf("%s %s names its node %t and says threads_recorded %t, want a row past the bound its writer recorded", table, id, namesNode, recorded)
	}
}

// requireMovedUnmarkedRow fails unless login's row on thread moved once for the write id at at,
// from version, and holds no marker.
func requireMovedUnmarkedRow(t *testing.T, database *store.Store, login, thread string, version int64, id string, at time.Time) {
	t.Helper()
	row := readThreadRow(t, database, login, thread)
	writes := threadRowWrites(t, database, login, thread)
	if row.version != version+1 || row.marked || row.markedVersionSet {
		t.Errorf("%s's row on %s = %+v, want version %d and no marker", login, thread, row, version+1)
	}
	if writes.lastReply != id || !writes.repliedAt.Equal(at) {
		t.Errorf("%s's row on %s names %+v, want the write %s at %s", login, thread, writes, id, at)
	}
}

// requireOwnUnmarkedRow fails unless login holds a row on thread, read through at and unmarked.
func requireOwnUnmarkedRow(t *testing.T, database *store.Store, login, thread string, at time.Time) {
	t.Helper()
	var readThrough time.Time
	var marked bool
	if err := database.Pool.QueryRow(context.Background(), `
		select read_through, marked_seq is not null from user_threads where login = $1 and thread_id = $2::uuid
	`, login, thread).Scan(&readThrough, &marked); err != nil {
		t.Fatalf("read %s's row on %s: %v", login, thread, err)
	}
	if !readThrough.Equal(at) || marked {
		t.Errorf("%s's row on %s reads through %s, marked %t; want %s and no marker", login, thread, readThrough, marked, at)
	}
}

// requireEventNames fails unless the event of type that recorded id names each of logins in its
// thread_logins.
func requireEventNames(t *testing.T, database *store.Store, eventType, id string, logins ...string) {
	t.Helper()
	named, _ := threadLoginsOf(t, database, eventType, id)
	for _, login := range logins {
		if !slices.Contains(named, login) {
			t.Errorf("the %s of %s names %v, want %s among them", eventType, id, named, login)
		}
	}
}

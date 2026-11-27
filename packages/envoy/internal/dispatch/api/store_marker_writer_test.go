package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// olderBuild is a connection to the test's database opened as the release before this one opens
// its own: from the database's address alone, so it carries none of the startup parameters
// store.Open gives this release's pool (store.StoreMarkerWriterSetting among them).
func olderBuild(t *testing.T, database *store.Store) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), database.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("connect as an older build: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// execer is a connection a test writes through: this build's pool or an older build's connection.
type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// markerWriterSetting is what current_setting answers for the marker writer on q, "" for none.
func markerWriterSetting(t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) string {
	t.Helper()
	var setting *string
	if err := q.QueryRow(context.Background(), `select current_setting($1, true)`, store.StoreMarkerWriterSetting).Scan(&setting); err != nil {
		t.Fatalf("read %s: %v", store.StoreMarkerWriterSetting, err)
	}
	if setting == nil {
		return ""
	}
	return *setting
}

// A connection of this build's pool carries the marker writer setting, and one an older build
// opens does not (store.Open). So an update that older build makes to an attempt row this build
// marked - a comment mention's and a targeted message's - comes back naming no node, which hands
// the row back to the backfill as an insert of that build's would (migration 20261116150000), and
// the same update through this build's pool keeps the node the row names.
func TestAnOlderBuildsUpdateOfAMarkedAttemptRowNamesNoNode(t *testing.T) {
	t.Parallel()
	listener, _ := newMentionListener(t)
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Second})
	older := olderBuild(t, database)
	if got := markerWriterSetting(t, database.Pool); got != "on" {
		t.Fatalf("this build's pool reads %s = %q, want on", store.StoreMarkerWriterSetting, got)
	}
	if got := markerWriterSetting(t, older); got != "" {
		t.Fatalf("an older build's connection reads %s = %q, want it unset", store.StoreMarkerWriterSetting, got)
	}

	issue := createInteractionIssue(t, handler, "MARK", "Marked attempts", "before")
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Are you there?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, database, created.ID)
	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Ping.", "target": "session:s1", "delivery": "steer",
	}, "alice")
	for _, row := range []struct {
		what, update, read string
		key                []any
	}{
		{
			"the mention's attempt",
			`update comment_deliveries set claimed_at = now() where comment_id = $1::uuid and target = $2 and attempt = 1`,
			`select node_kind, node_address from comment_deliveries where comment_id = $1::uuid and target = $2 and attempt = 1`,
			[]any{created.ID, "session:s1"},
		},
		{
			"the message's attempt",
			`update message_deliveries set claimed_at = now() where message_id = $1::uuid and attempt = 1`,
			`select node_kind, node_address from message_deliveries where message_id = $1::uuid and attempt = 1`,
			[]any{message.ID},
		},
	} {
		named := func() bool {
			t.Helper()
			var kind, address *string
			if err := database.Pool.QueryRow(context.Background(), row.read, row.key...).Scan(&kind, &address); err != nil {
				t.Fatalf("read %s: %v", row.what, err)
			}
			if (kind == nil) != (address == nil) {
				t.Fatalf("%s names node kind %v and address %v, want both or neither", row.what, kind, address)
			}
			return kind != nil
		}
		if !named() {
			t.Fatalf("%s names no node as this build wrote it, so this test would prove nothing", row.what)
		}
		if _, err := database.Pool.Exec(context.Background(), row.update, row.key...); err != nil {
			t.Fatalf("update %s through this build's pool: %v", row.what, err)
		}
		if !named() {
			t.Errorf("an update of %s through this build's pool cleared its node, want it kept", row.what)
		}
		if _, err := older.Exec(context.Background(), row.update, row.key...); err != nil {
			t.Fatalf("update %s as an older build: %v", row.what, err)
		}
		if named() {
			t.Errorf("an older build's update of %s left it naming its node, want it unmarked for the backfill", row.what)
		}
	}
}

// restoreAsAnOlderBuild restores block blockID of comment id as the release before this one's
// index restores a block it holds again (main's writeItemBlocks, its upsert of a written block):
// the row's position, type, sum, excerpt and changed facts move and its node columns are left as
// they were, on conn.
func restoreAsAnOlderBuild(t *testing.T, conn execer, database *store.Store, id, blockID string, position int) {
	t.Helper()
	ctx := context.Background()
	var kind, sum, excerpt string
	var revision int
	if err := database.Pool.QueryRow(ctx, `
		select b.type, b.text_sha256, b.excerpt, s.revision
		from comment_blocks b join comment_block_state s on s.comment_id = b.comment_id
		where b.comment_id = $1::uuid and b.block_id = $2
	`, id, blockID).Scan(&kind, &sum, &excerpt, &revision); err != nil {
		t.Fatalf("read block %s of comment %s: %v", blockID, id, err)
	}
	now := time.Now().UTC()
	if _, err := conn.Exec(ctx, `
		insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_revision, changed_at)
		select $1::uuid, b.id, 'comment/' || $1::uuid::text || '#' || b.id, b.position, b.type, b.sum, b.excerpt, $2, $3, $4
		from unnest($5::text[], $6::int[], $7::text[], $8::text[], $9::text[], $10::text[]) as b (id, position, type, sum, excerpt, made_by)
		on conflict (comment_id, block_id) do update set
			position = excluded.position,
			type = excluded.type,
			text_sha256 = excluded.text_sha256,
			excerpt = excluded.excerpt,
			changed_revision = excluded.changed_revision,
			changed_at = excluded.changed_at,
			removed_at = null
	`, id, now, revision+1, now, []string{blockID}, []int32{int32(position)}, []string{kind}, []string{sum}, []string{excerpt}, []*string{nil}); err != nil {
		t.Fatalf("restore block %s of comment %s: %v", blockID, id, err)
	}
}

// A block an older build restores is answered once throughout. Its edit removed it, so the store
// holds its node gone; the release before this one restores it by an upsert that leaves its node
// columns as they were, and that update unmarks the row (migration 20261116150000), so its old arm
// answers its part_of, and once this build next writes a revision of the comment it takes the row
// over, states the node live and its part_of is the store's. A restore that leaves the row naming
// its gone node - the same upsert on a connection of this build's, where nothing unmarks it - is
// answered by neither arm until this build's next revision, which reads such a row as naming no
// node (storedItemBlocks) and states the node live.
func TestARestoredItemBlockIsAnsweredOnceThroughout(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RSTR", "Restored blocks", "before")
	older := olderBuild(t, database)
	for _, restore := range []struct {
		name string
		conn execer
		// restored is how many times graph_edges answers the block's part_of once it is restored.
		restored int
	}{
		{"by an older build", older, 1},
		{"on a connection of this build's", database.Pool, 0},
	} {
		t.Run(restore.name, func(t *testing.T) {
			comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\nBeta."}, "alice")
			partOf := func(when string, want int) {
				t.Helper()
				if got := countEdges(t, database, commentBlocks.addressForm, "comment/"+comment.ID+"#b2", "part_of", "comment", comment.ID); got != want {
					t.Errorf("%s, graph_edges answers block b2's part_of %d time(s), want %d", when, got, want)
				}
			}
			partOf("as written", 1)
			edit := func(body string) {
				t.Helper()
				if edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{"body": body}, "alice"); edited.Code != http.StatusOK {
					t.Fatalf("edit the comment: status=%d body=%s", edited.Code, edited.Body.String())
				}
			}
			edit("Alpha.")
			partOf("once an edit removed it", 0)
			restoreAsAnOlderBuild(t, restore.conn, database, comment.ID, "b2", 1)
			partOf("once restored", restore.restored)
			edit("Alpha.\n\nBeta.\n\nGamma.")
			partOf("once this build wrote the next revision", 1)
			var state string
			var named bool
			if err := database.Pool.QueryRow(context.Background(), `
				select n.state, b.node_kind is not null from comment_blocks b
				join graph_nodes n on n.kind = 'block' and n.id = b.node_id
				where b.comment_id = $1::uuid and b.block_id = 'b2'
			`, comment.ID).Scan(&state, &named); err != nil {
				t.Fatalf("read block b2: %v", err)
			}
			if state != "live" || !named {
				t.Errorf("after this build's next revision block b2's node is %s and its row names it %t, want live and named", state, named)
			}
		})
	}
}

// An ask's answer part is whoever answered: its node, the part_of that places it in the ask and
// the base attributes its node write stated name the answerer, not the session that asked.
func TestAnAnswerPartsNodeAndTuplesNameWhoAnswered(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "ANSW", "Answer parts", "before")
	ask := createPartAsk(t, handler, issue.Key, "Ship it?", map[string]string{"label": "Yes"}, map[string]string{"label": "No"})
	answerPartAsk(t, handler, ask, "bob", []string{"Yes"}, "")
	var part, maker string
	if err := database.Pool.QueryRow(context.Background(), `
		select b.node_id, n.made_by->>'id' from ask_blocks b
		join graph_nodes n on n.kind = 'block' and n.id = b.node_id
		where b.ask_id = $1::uuid and b.type = 'answer' and b.position is not null
	`, ask.ID).Scan(&part, &maker); err != nil {
		t.Fatalf("read the answer part of ask %s: %v", ask.ID, err)
	}
	if maker != "bob" {
		t.Errorf("the answer part's node is made by %q, want bob, who answered", maker)
	}
	rows, err := database.Pool.Query(context.Background(), `
		select kind, who->>'id' from graph_tuples where from_kind = 'block' and from_id = $1 order by seq
	`, part)
	if err != nil {
		t.Fatalf("read the answer part's tuples: %v", err)
	}
	stated, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var tuple [2]string
		err := row.Scan(&tuple[0], &tuple[1])
		return tuple, err
	})
	if err != nil {
		t.Fatalf("read the answer part's tuples: %v", err)
	}
	placed := false
	for _, tuple := range stated {
		placed = placed || tuple[0] == "part_of"
		if tuple[1] != "bob" {
			t.Errorf("the answer part states %s as %q, want bob, who answered", tuple[0], tuple[1])
		}
	}
	if !placed {
		t.Errorf("the answer part states %v, want its part_of among them", stated)
	}
}

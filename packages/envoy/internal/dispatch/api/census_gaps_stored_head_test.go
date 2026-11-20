package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// headWindowSession is the agent whose conversation the stored-head row's documents are of. It
// acted in PUB alone, so alice, who sees PUB, reads it and its conversation.
const headWindowSession = "session-head-window"

// headWindow is a tracer that, armed, places headWindowSession in PRIV right after the first
// statement its switch matcher names has run, and counts the reads of the document's stored state
// that its read matcher names after it.
type headWindow struct {
	place func() error

	mu        sync.Mutex
	after     func(sql string) bool
	read      func(sql string) bool
	armed     bool
	switched  bool
	switchErr error
	reads     int
}

type headWindowSQLKey struct{}

func (w *headWindow) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, headWindowSQLKey{}, data.SQL)
}

func (w *headWindow) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(headWindowSQLKey{}).(string)
	w.mu.Lock()
	switchNow := w.armed && data.Err == nil && w.after(sql)
	if switchNow {
		w.armed = false
	}
	if w.switched && w.read(sql) {
		w.reads++
	}
	w.mu.Unlock()
	if !switchNow {
		return
	}
	// The switch runs outside the lock: its own statement passes through here too.
	err := w.place()
	w.mu.Lock()
	w.switched, w.switchErr = true, err
	w.mu.Unlock()
}

// arm makes the next statement after names open the window, and counts the reads read names
// after it.
func (w *headWindow) arm(after, read func(sql string) bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.after, w.read, w.armed, w.switched, w.switchErr, w.reads = after, read, true, false, nil, 0
}

// opened fails the test unless the window opened and the read it names ran inside it.
func (w *headWindow) opened(t *testing.T, what string) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed = false
	switch {
	case w.switchErr != nil:
		t.Fatalf("place %s in PRIV inside the window: %v", headWindowSession, w.switchErr)
	case !w.switched:
		t.Fatal("the route made no statement the window opens after, so no window opened")
	case w.reads == 0:
		t.Fatalf("%s was not read after the switch, so this row measures nothing", what)
	}
}

// artifactRowRead names the route's read of the document's own row (loadArtifactRowByID), the
// check every later read of the document stands on.
func artifactRowRead(sql string) bool {
	return strings.Contains(sql, "from artifacts where id = $1 and")
}

// storedHeadRead names the stored head a read of a document a room of this process holds judges
// that room against (docs' readStoredHead), as the base and as the fix spell it.
func storedHeadRead(sql string) bool {
	return strings.Contains(sql, "coalesce(max(version), 0) from doc_updates") ||
		strings.Contains(sql, "max(u.version) from doc_updates u")
}

// storedStampRead names the stamp a cold read of a document reads (docs' readDocumentStamp), and
// the stamp the load of its history reads first (readStoredLog): the head with its prune
// checkpoint.
func storedStampRead(sql string) bool {
	return strings.Contains(sql, "from doc_checkpoints where artifact_id = $1")
}

// coldStampRead names the cold read's own stamp read alone, not the load's.
func coldStampRead(sql string) bool {
	return storedStampRead(sql) && !historyLoad(sql)
}

// historyLoad names the first statement of the load of a document's stored history, the fold a
// cold read makes when no rendering of its stamp is cached (readStoredLog).
func historyLoad(sql string) bool {
	return strings.Contains(sql, "r.updates, r.bytes")
}

// TestADocumentMadeHiddenAfterItsReadAnswersNoBlocks (item 5 of the census gaps, and the reload
// a cold read makes on a cache miss): GET /api/v1/artifacts/{id}/blocks reads the document's row
// as its reader may read it (loadArtifactRowByID), then reads the document itself in statements
// of their own: with a room of it resident, the stored head that room is judged against (docs'
// readStoredHead); with none, the stored state's stamp the cold read is keyed by
// (readDocumentStamp), and, when no rendering of that stamp is cached, the load of the stored
// history (LoadDocument), in a transaction of its own that every read missing the stamp shares. A
// document of an agent's conversation is read by the session's rule (access.SessionClause, the
// live session_projects pairs), the one rule a statement after the route's first can find
// changed: the session placed in a project alice does not see hides the conversation from her
// from that pair's commit. No route writes a markdown document into a conversation (400
// ARTIFACT_INPUT), so the row moves three PUB project documents there in SQL, the shape migration
// 0071 stores and a release can keep: one read with a room of it resident, one with none and its
// rendering cached, and one with none and its stamp new, so the read loads its history.
//
// Each window is placed right after the check the read stands on: the route's read of the row,
// and for the load, the cold read's stamp. alice must then read each document as an id no
// document holds: 404 ARTIFACT_NOT_FOUND, "artifact not found", and none of its blocks. With PRIV
// public, no project is private and the session stays hers to read: the same blocks as a read
// with no window (R9).
func TestADocumentMadeHiddenAfterItsReadAnswersNoBlocks(t *testing.T) {
	window := &headWindow{}
	seed := seedPrivateProjectWith(t, testServerOptions{tracer: window, settle: time.Hour})
	ctx := context.Background()
	window.place = func() error {
		_, err := seed.database.Pool.Exec(context.Background(), `insert into session_projects (session_id, project_key) values ($1, $2)
			on conflict do nothing`, headWindowSession, seedPrivate)
		return err
	}
	if _, err := seed.database.Pool.Exec(ctx, `insert into session_projects (session_id, project_key) values ($1, $2)`,
		headWindowSession, seedPublic); err != nil {
		t.Fatalf("place %s in PUB: %v", headWindowSession, err)
	}
	// Two texts of one length, so an edit between them moves the document's stamp and leaves
	// every block's range where it was.
	texts := [2]string{"The conversation's private plan.", "The conversation's private plan!"}
	written := map[string]int{}
	// move puts document in headWindowSession's conversation, or back in PUB. The document's node
	// keeps PUB's address, which a write of it needs, so the row writes it while it is PUB's.
	move := func(t *testing.T, document string, conversation bool) {
		t.Helper()
		statement := `update artifacts set project_key = null, session_id = $2 where id = $1`
		args := []any{document, headWindowSession}
		if !conversation {
			statement, args = `update artifacts set project_key = $2, session_id = null where id = $1`, []any{document, seedPublic}
		}
		if tag, err := seed.database.Pool.Exec(ctx, statement, args...); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("move %s (into the conversation: %t): %v (%d rows)", document, conversation, err, tag.RowsAffected())
		}
	}
	edit := func(t *testing.T, document string) {
		t.Helper()
		next := (written[document] + 1) % len(texts)
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/artifacts/"+document+"/edits", map[string]any{
			"ops": []map[string]any{{"op": "replace", "find": texts[written[document]], "with": texts[next]}},
		}, http.StatusOK)
		written[document] = next
	}
	conversationDocument := func(name string) string {
		t.Helper()
		document := decodeBody[artifactUploadResponse](t, seed.mustRequest(t, callerCarol, http.MethodPost,
			"/api/v1/projects/"+seedPublic+"/artifacts",
			map[string]any{"name": name, "content": "# Plan\n\n" + texts[0] + "\n"}, http.StatusCreated)).Artifact.ID
		edit(t, document)
		move(t, document, true)
		return document
	}
	// refresh gives a conversation's document a stamp no rendering is cached under.
	refresh := func(t *testing.T, document string) {
		t.Helper()
		move(t, document, false)
		edit(t, document)
		move(t, document, true)
	}
	type documentRead struct {
		name, document string
		// resident loads the document's room before each read, so the read judges that room
		// against the stored head; a read with no room resident is a cold read of the stamp.
		resident bool
		// fresh edits the document before each read, so its stamp is one no rendering is cached
		// under and the cold read loads its history.
		fresh       bool
		after, read func(string) bool
		readNamed   string
	}
	reads := []documentRead{
		{name: "with a room resident", document: conversationDocument("Warm.md"), resident: true,
			after: artifactRowRead, read: storedHeadRead, readNamed: "the stored head"},
		{name: "with no room resident", document: conversationDocument("Cold.md"),
			after: artifactRowRead, read: storedStampRead, readNamed: "the stored state's stamp"},
		{name: "with no room resident and no rendering cached", document: conversationDocument("Fresh.md"), fresh: true,
			after: coldStampRead, read: historyLoad, readNamed: "the stored history"},
	}
	read := func(t *testing.T, document documentRead) (int, string) {
		t.Helper()
		if document.resident {
			if _, err := seed.deps.Docs.BlockForQuote(ctx, document.document, texts[written[document.document]]); err != nil {
				t.Fatalf("load the room of %s: %v", document.document, err)
			}
		}
		response := seed.request(t, callerAlice, http.MethodGet, "/api/v1/artifacts/"+document.document+"/blocks", nil)
		return response.Code, response.Body.String()
	}
	before := map[string]string{}
	for _, document := range reads {
		code, body := read(t, document)
		var blocks []model.ArtifactBlock
		if err := json.Unmarshal([]byte(body), &blocks); err != nil || code != http.StatusOK || len(blocks) == 0 ||
			blocks[len(blocks)-1].To-blocks[len(blocks)-1].From != len(texts[0]) {
			t.Fatalf("alice reads the conversation's document %s as %d %s, not the edited one: the row reads nothing",
				document.name, code, body)
		}
		before[document.name] = body
	}
	missing := seed.request(t, callerAlice, http.MethodGet, "/api/v1/artifacts/"+uuid.NewString()+"/blocks", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("the blocks of an id no document holds = %d %s, want 404", missing.Code, missing.Body.String())
	}

	t.Run("PRIV private", func(t *testing.T) {
		for _, document := range reads {
			t.Run(document.name, func(t *testing.T) {
				if document.fresh {
					refresh(t, document.document)
				}
				window.arm(document.after, document.read)
				code, body := read(t, document)
				window.opened(t, document.readNamed)
				if code != missing.Code || body != missing.Body.String() {
					t.Errorf("alice, who no longer reads the document, reads its blocks as %d %s; an id no document holds answers %d %s",
						code, body, missing.Code, missing.Body.String())
				}
				if _, err := seed.database.Pool.Exec(ctx, `delete from session_projects where session_id = $1 and project_key = $2`,
					headWindowSession, seedPrivate); err != nil {
					t.Fatalf("take %s out of PRIV: %v", headWindowSession, err)
				}
			})
		}
	})
	t.Run("PRIV public: the blocks are read as before", func(t *testing.T) {
		seed.setVisibility(t, seedPrivate, model.ProjectPublic)
		for _, document := range reads {
			t.Run(document.name, func(t *testing.T) {
				want := before[document.name]
				if document.fresh {
					refresh(t, document.document)
				}
				window.arm(document.after, document.read)
				code, body := read(t, document)
				window.opened(t, document.readNamed)
				if document.fresh {
					// The edit moved the document, so the read with no window is the one made
					// right after, of the same stamp.
					_, want = read(t, document)
				}
				if code != http.StatusOK || body != want {
					t.Errorf("with no project private, alice reads the blocks as %d %s; with no window, %s", code, body, want)
				}
			})
		}
	})
}

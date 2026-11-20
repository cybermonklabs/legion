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

// headWindow is a tracer that, armed, places headWindowSession in PRIV right after the route's
// read of the document's own row has run (loadArtifactRowByID), and counts the reads of the
// document's stored state that read (its matcher) names after it.
type headWindow struct {
	place func() error

	mu        sync.Mutex
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
	switchNow := w.armed && data.Err == nil && strings.Contains(sql, "from artifacts where id = $1 and")
	if switchNow {
		w.armed = false
	}
	if w.switched && w.read != nil && w.read(sql) {
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

// arm makes the next request's read of a document's row open the window, and counts the reads
// read names after it.
func (w *headWindow) arm(read func(sql string) bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.read, w.armed, w.switched, w.switchErr, w.reads = read, true, false, nil, 0
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
		t.Fatal("the route read no document row, so no window opened")
	case w.reads == 0:
		t.Fatalf("%s was not read after the switch, so this row measures nothing", what)
	}
}

// storedHeadRead names the stored head a read of a document a room of this process holds judges
// that room against (docs' readStoredHead), as the base and as the fix spell it.
func storedHeadRead(sql string) bool {
	return strings.Contains(sql, "coalesce(max(version), 0) from doc_updates") ||
		strings.Contains(sql, "max(u.version) from doc_updates u")
}

// storedStampRead names the stamp a cold read of a document reads (docs' readDocumentStamp): the
// head with its prune checkpoint.
func storedStampRead(sql string) bool {
	return strings.Contains(sql, "from doc_checkpoints where artifact_id = $1")
}

// TestADocumentMadeHiddenAfterItsReadAnswersNoBlocks (item 5 of the census gaps): GET
// /api/v1/artifacts/{id}/blocks reads the document's row as its reader may read it
// (loadArtifactRowByID), then reads the document itself in statements of their own: with a room
// of it resident, the stored head that room is judged against (docs' readStoredHead); with none,
// the stored state's stamp the cold read is keyed by (readDocumentStamp). A document of an agent's
// conversation is read by the session's rule (access.SessionClause, the live session_projects
// pairs), the one rule a statement after the route's first can find changed: the session placed
// in a project alice does not see hides the conversation from her from that pair's commit. No
// route writes a markdown document into a conversation (400 ARTIFACT_INPUT), so the row moves two
// PUB project documents there in SQL, the shape migration 0071 stores and a release can keep; one
// is read with a room of it resident, and one with none.
//
// The window is placed right after the route's read of the row. alice must then read each
// document as an id no document holds: 404 ARTIFACT_NOT_FOUND, "artifact not found", and none of
// its blocks. With PRIV public, no project is private and the session stays hers to read: the
// same blocks as before the window (R9).
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
	const words = "The conversation's private plan."
	conversationDocument := func(name string) string {
		t.Helper()
		document := decodeBody[artifactUploadResponse](t, seed.mustRequest(t, callerCarol, http.MethodPost,
			"/api/v1/projects/"+seedPublic+"/artifacts",
			map[string]any{"name": name, "content": "# Plan\n\nThe plan.\n"}, http.StatusCreated)).Artifact.ID
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/artifacts/"+document+"/edits", map[string]any{
			"ops": []map[string]any{{"op": "replace", "find": "The plan.", "with": words}},
		}, http.StatusOK)
		if tag, err := seed.database.Pool.Exec(ctx, `update artifacts set project_key = null, session_id = $2 where id = $1`,
			document, headWindowSession); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("move %s into %s's conversation: %v (%d rows)", name, headWindowSession, err, tag.RowsAffected())
		}
		return document
	}
	reads := []struct {
		name, document string
		// resident loads the document's room before each read, so the read judges that room
		// against the stored head; a read with no room resident is a cold read of the stamp.
		resident  bool
		read      func(string) bool
		readNamed string
	}{
		{name: "with a room resident", document: conversationDocument("Warm.md"), resident: true, read: storedHeadRead, readNamed: "the stored head"},
		{name: "with no room resident", document: conversationDocument("Cold.md"), read: storedStampRead, readNamed: "the stored state's stamp"},
	}
	read := func(t *testing.T, document string, resident bool) (int, string) {
		t.Helper()
		if resident {
			if _, err := seed.deps.Docs.BlockForQuote(ctx, document, words); err != nil {
				t.Fatalf("load the room of %s: %v", document, err)
			}
		}
		response := seed.request(t, callerAlice, http.MethodGet, "/api/v1/artifacts/"+document+"/blocks", nil)
		return response.Code, response.Body.String()
	}
	before := map[string]string{}
	for _, document := range reads {
		code, body := read(t, document.document, document.resident)
		var blocks []model.ArtifactBlock
		if err := json.Unmarshal([]byte(body), &blocks); err != nil || code != http.StatusOK || len(blocks) == 0 ||
			blocks[len(blocks)-1].To-blocks[len(blocks)-1].From != len(words) {
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
				window.arm(document.read)
				code, body := read(t, document.document, document.resident)
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
				window.arm(document.read)
				code, body := read(t, document.document, document.resident)
				window.opened(t, document.readNamed)
				if code != http.StatusOK || body != before[document.name] {
					t.Errorf("with no project private, alice reads the blocks as %d %s; before the window, %s",
						code, body, before[document.name])
				}
			})
		}
	})
}

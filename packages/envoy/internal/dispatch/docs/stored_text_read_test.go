package docs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// serverReader is the server's own reader (model.ServerReader), which reads every document: the
// reader of a test that reads a document's text for no person (TextWithToken).
var serverReader = model.ServerReader

// storedTextReadRenders is how often a text read of a document a room holds renders it: not at
// all. The version written from that very state stored its markdown and its document token beside
// what they were rendered from, and the read answers them without reading the document
// (TextWithToken, storedText). The base this measurement records was one: the read took the
// room's tree (heldTree) and rendered it, though the version held that rendering already.
const storedTextReadRenders = 0

// storedTextReadRendersUnserved is how often a read renders the document where the stored
// rendering is not the document's own: once, as it always did.
const storedTextReadRendersUnserved = 1

// seedSettledDocument seeds markdown as the document's text and settles it, so the document's
// latest version was written from the state its room still holds, and the room holds it.
func seedSettledDocument(t *testing.T, service *Service, artifactID, markdown string) {
	t.Helper()
	seedServiceText(t, service, artifactID, markdown)
	// The settlement writes the version from the room's document, and nothing edits the room
	// afterwards, so the room holds the state the version was written from.
	settleCurrentGeneration(t, service, artifactID)
	requireLatestVersionMarkdown(t, service, artifactID, markdown)
	if tree := liveTree(t, service, artifactID); tree == nil {
		t.Fatal("the room holds no document to read")
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("no room holds the document, so the read would be a cold one")
	}
}

// A text read of a document whose room still holds the state its latest version was written from
// renders the document storedTextReadRenders times, and answers the version's stored markdown.
func TestStoredTextReadCountsTheRendersOfAReadOfAVersionedDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const written = "The quick brown fox.\n"
	seedSettledDocument(t, service, artifactID, written)
	stored := latestVersionMarkdown(t, service, artifactID)

	renders := countRenders(t)
	markdown, token, err := service.TextWithToken(context.Background(), serverReader, artifactID)
	counted := len(renders())
	if err != nil {
		t.Fatalf("read the document's text: %v", err)
	}
	t.Logf("a text read of a document its room holds costs %d renders of it", counted)
	if markdown != written {
		t.Fatalf("the read answered %q, want the version's markdown %q", markdown, written)
	}
	// The read answers the very bytes the version stored, not a rendering that agrees with them.
	if markdown != stored {
		t.Fatalf("the read answered %q, want the version's stored markdown %q, byte for byte", markdown, stored)
	}
	// The token is the live document's own, so a guarded edit holding it means what it meant
	// when the read rendered the document itself.
	live, err := nodeToken(liveTree(t, service, artifactID))
	if err != nil {
		t.Fatalf("take the live document's token: %v", err)
	}
	if token == "" || token != live {
		t.Fatalf("the read answered token %q, want the live document's %q", token, live)
	}
	if counted != storedTextReadRenders {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRenders)
	}
}

// A room that has applied a browser's update and not yet stored it holds text no version does, so
// the read renders the room rather than serving the version's markdown.
func TestStoredTextReadRendersARoomHoldingAnUnstoredUpdate(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedSettledDocument(t, service, artifactID, "The quick brown fox.\n")
	// The peer's update is applied to the room and counted among the updates it has not stored
	// (queueUpdateClass), and its observer is held before persistence is handed it.
	release := holdPeerEdit(t, service, artifactID, &service.afterCreditUpdate, replaceRun("fox.", "dog."))
	defer release()

	renders := countRenders(t)
	markdown, token, err := service.TextWithToken(context.Background(), serverReader, artifactID)
	counted := len(renders())
	if err != nil {
		t.Fatalf("read the document's text: %v", err)
	}
	if markdown != "The quick brown dog.\n" {
		t.Fatalf("the read answered %q, want the room's unstored text", markdown)
	}
	if token == "" {
		t.Fatal("the read answered no document token")
	}
	if counted != storedTextReadRendersUnserved {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRendersUnserved)
	}
}

// An anchor write appends an update and writes no version, so the document's stamp moves past the
// one its latest version holds and the read renders the document.
func TestStoredTextReadRendersAfterAnAnchorWriteMovedTheStamp(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const written = "The quick brown fox.\n"
	seedSettledDocument(t, service, artifactID, written)
	spec := MarkSpec{Kind: MarkComment, ID: "comment-1", By: model.Actor{Kind: "session", ID: "session-0123456789abcdef"}}
	if _, err := joinedMarkQuote(service, artifactID, spec, "brown", nil); err != nil {
		t.Fatalf("anchor a comment: %v", err)
	}

	renders := countRenders(t)
	markdown, token, err := service.TextWithToken(context.Background(), serverReader, artifactID)
	counted := len(renders())
	if err != nil {
		t.Fatalf("read the document's text: %v", err)
	}
	// A mark renders nothing, so the text is the version's all the same; the stamp is not.
	if markdown != written {
		t.Fatalf("the read answered %q, want %q", markdown, written)
	}
	if token == "" {
		t.Fatal("the read answered no document token")
	}
	if counted != storedTextReadRendersUnserved {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRendersUnserved)
	}
}

// A version another renderer wrote is not this build's to serve: its markdown and its token are
// what that renderer made of the tree, so the read renders the document itself.
func TestStoredTextReadRendersAVersionOfAnotherRendererVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const written = "The quick brown fox.\n"
	seedSettledDocument(t, service, artifactID, written)
	if _, err := service.store.Pool.Exec(context.Background(), `
		update artifact_versions set renderer_version = $2 where artifact_id = $1
	`, artifactID, pmdoc.RendererVersion+1); err != nil {
		t.Fatalf("stamp the version with another renderer version: %v", err)
	}

	renders := countRenders(t)
	markdown, token, err := service.TextWithToken(context.Background(), serverReader, artifactID)
	counted := len(renders())
	if err != nil {
		t.Fatalf("read the document's text: %v", err)
	}
	if markdown != written {
		t.Fatalf("the read answered %q, want %q", markdown, written)
	}
	if token == "" {
		t.Fatal("the read answered no document token")
	}
	if counted != storedTextReadRendersUnserved {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRendersUnserved)
	}
}

// A text read is made for its reader: a reader who may not read the document - here the closed
// viewer, which sees no project and no conversation - is refused as pgx.ErrNoRows, the answer a
// document no row holds is to every caller of the route, and the server's own reader reads it.
func TestATextReadRefusesADocumentItsReaderMayNotRead(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "Private text.\n")
	closed := model.Actor{Viewer: &model.Viewer{Sees: []string{}, AnyPrivate: true}}
	if markdown, _, err := service.TextWithToken(context.Background(), closed, artifactID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the closed viewer's read answered %q, %v; want pgx.ErrNoRows", markdown, err)
	}
	if markdown, _, err := service.TextWithToken(context.Background(), serverReader, artifactID); err != nil || markdown != "Private text.\n" {
		t.Fatalf("the server's own read answered %q, %v; want the document's text", markdown, err)
	}
}

// The statement a text read makes first (storedVersionSQL: the stamp, the latest version, and
// whether the reader may read the document), for every document a room holds and, for a reader
// that may not see every document, for one no room holds, reaches every relation it reads by key.
// It is the one statement such a cold read pays beyond what it paid before, and keyed is what keeps
// that cost a few index probes whatever the document's history holds; the execution time a run
// measures is logged beside the plan.
func TestTheStoredVersionReadIsKeyed(t *testing.T) {
	service, artifactID := newTestService(t)
	seedSettledDocument(t, service, artifactID, "Keyed text.\n")
	database := &store.Store{Pool: service.store.Pool}
	plan := explainPlan(t, database, storedVersionSQL, artifactID, refs.VisibilityArg(serverReader))
	if unkeyed := explaintest.ScansWithoutIndexCond(t, plan); len(unkeyed) > 0 {
		t.Errorf("the stored version read reads %v, which no index condition keys:\n%s", unkeyed, plan)
	}
	var raw []byte
	if err := service.store.Pool.QueryRow(context.Background(), "explain (analyze, format json) "+storedVersionSQL,
		artifactID, refs.VisibilityArg(serverReader)).Scan(&raw); err != nil {
		t.Fatalf("analyze the stored version read: %v", err)
	}
	var analyzed []struct {
		Executed float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &analyzed); err != nil || len(analyzed) != 1 {
		t.Fatalf("decode the analyzed plan (%v): %s", err, raw)
	}
	t.Logf("the stored version read executed in %.3f ms", analyzed[0].Executed)
}

// coldTextReadStatements is how many statements naming the document a text read of a document
// no room of this process holds sends, as the base's read sent them: the first read of its stored
// state folds the stored history (coldRead's stamp, then foldDocument's load) and keeps the
// rendering under that stamp, and every later read of the same state reads the stamp alone and
// answers from the cache. storedText sends none of them: it looks for the room before anything.
var coldTextReadStatements = struct{ fold, cached int }{fold: 3, cached: 1}

// A text read of a document no room of this process holds costs the statements the base's read
// made (coldTextReadStatements) for a reader that sees every document: the server's own reader,
// and an open viewer of a deployment that holds no private project, which is every viewer there
// but a restricted member and a credential that lists projects. storedText weighs a version only
// against a room, so with none it has nothing to read for such a reader. A reader that may not see
// every document pays one statement more on each read, the rule of the document's own row, which
// refuses a document it may not read cold as it does warm (pgx.ErrNoRows).
func TestAColdTextReadCostsTheStatementsTheBaseMade(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	tracer := &statementTracer{}
	traced := storetest.TracedStore(t, database, tracer)
	artifactID := createDocument(t, database, "before")
	const written = "# Heading\n\nThe stored text.\n"
	seedServiceText(t, newReadCacheService(t, database, NewPgVersioned(database)), artifactID, written)
	reader := newReadCacheService(t, traced, NewPgVersioned(traced))

	read := func(name string, who model.Actor) (int, error) {
		t.Helper()
		if reader.srv.GetDoc(artifactID) != nil {
			t.Fatalf("%s: a room of the reader holds the document, so the read would not be a cold one", name)
		}
		since := tracer.mark()
		markdown, _, err := reader.TextWithToken(ctx, who, artifactID)
		sent := tracer.statements(artifactID, "", since)
		t.Logf("%s: a cold text read sent %d statements naming the document (%v)", name, sent, err)
		if err == nil && markdown != written {
			t.Fatalf("%s: the cold read answered %q, want %q", name, markdown, written)
		}
		return sent, err
	}
	everyDocument := []struct {
		name string
		who  model.Actor
		want int
	}{
		{"the server's own reader, folding", serverReader, coldTextReadStatements.fold},
		{"the server's own reader, from the cache", serverReader, coldTextReadStatements.cached},
		{"an open viewer of a deployment with no private project", model.Actor{Viewer: &model.Viewer{Open: true, Sees: []string{"DOC"}}}, coldTextReadStatements.cached},
	}
	for _, reading := range everyDocument {
		sent, err := read(reading.name, reading.who)
		if err != nil {
			t.Fatalf("%s: %v", reading.name, err)
		}
		if sent != reading.want {
			t.Errorf("%s: a cold text read sent %d statements naming the document, want %d, the base's", reading.name, sent, reading.want)
		}
	}

	// A credential that lists DOC sees it, and pays the rule's one statement to be told so.
	listed := model.Actor{Viewer: &model.Viewer{Sees: []string{"DOC"}, Lists: []string{"DOC"}}}
	if sent, err := read("a credential that lists DOC", listed); err != nil || sent != coldTextReadStatements.cached+1 {
		t.Errorf("a credential that lists DOC: a cold text read sent %d statements (%v), want %d and the text", sent, err, coldTextReadStatements.cached+1)
	}
	// The closed viewer sees no project: the rule refuses the document before any version read.
	closed := model.Actor{Viewer: &model.Viewer{Sees: []string{}, AnyPrivate: true}}
	if sent, err := read("the closed viewer", closed); !errors.Is(err, pgx.ErrNoRows) || sent != 1 {
		t.Errorf("the closed viewer: a cold text read sent %d statements and answered %v, want one and pgx.ErrNoRows", sent, err)
	}
}

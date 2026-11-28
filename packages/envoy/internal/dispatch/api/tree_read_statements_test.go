package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// treeReadStatementBound is the most statements one tree read of the read-cost corpus's tree issue
// sends, a batch counted once (roundTrips): the request's own two - the issue's existence and the
// reader's sign-in - and the read's transaction, its JIT setting, the cut, the walk's conversation,
// subtrees, issues above and view, the issues under the issue and its children, the nodes' facts,
// the rows of the nodes the walk read none of, two loaders (the issue's, the document's), the
// blocks' one read, the issue's tree, the batch of texts and the commit; and the skeleton's (R8):
// the issue's documents, its comment page, and after the commit the count of the blocks of each
// of its two documents, its spec and its upload, off its live room.
const treeReadStatementBound = 23

// handlerTransport serves each request a client sends through handler, the route table a server
// mounts (newTestServer), in the test's process: the request passes every route's own middleware -
// its access line, its credential, its reader's display trees - as one through a listener does,
// and no listener stands where a client a test leaves behind could reach it.
type handlerTransport struct{ handler http.Handler }

func (transport handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	served := request.Clone(request.Context())
	served.RequestURI, served.RemoteAddr = request.URL.RequestURI(), "192.0.2.1:1234"
	response := httptest.NewRecorder()
	transport.handler.ServeHTTP(response, served)
	return response.Result(), nil
}

// inProcessReadCostServer is the read-cost corpus's client (readCostServer) over handler, read and
// written through handlerTransport.
func inProcessReadCostServer(handler http.Handler) *readCostServer {
	return &readCostServer{name: "this head, in process", client: &http.Client{Transport: handlerTransport{handler}, Timeout: 10 * time.Minute}}
}

// seedTreeReadCorpus writes the read-cost corpus's tree issue through s, as the read-cost check
// writes it: the reader and the writer signed in, the writer's project, the issue and its document
// of readCostTreeBlocks blocks (readCostTreeText); then it indexes every document
// (indexOwedDocuments), so a read plans from the tables' real sizes.
func seedTreeReadCorpus(t *testing.T, s *readCostServer, database *store.Store, deps Deps) *readCostCorpus {
	t.Helper()
	for _, person := range []string{readCostReader, readCostWriter} {
		s.expect(t, "sign in "+person, http.MethodGet, "/api/v1/whoami", person, nil, http.StatusOK)
	}
	s.expect(t, "create the project "+readCostProject, http.MethodPost, "/api/v1/projects", readCostWriter,
		map[string]string{"key": readCostProject, "name": "The reads this tree moves"}, http.StatusCreated)
	corpus := &readCostCorpus{}
	corpus.treeIssue = createReadCostIssue(t, s, readCostProject, "The issue whose tree is expanded", "", readCostWriter)
	corpus.treeDocument = uploadReadCostDocument(t, s, corpus.treeIssue, "tree.md", readCostTreeText(), readCostWriter)
	indexOwedDocuments(t, database, deps)
	return corpus
}

// indexOwedDocuments runs the block index's reconciliation over deps until no document owes its
// index, and then analyzes the database.
func indexOwedDocuments(t *testing.T, database *store.Store, deps Deps) {
	t.Helper()
	ctx := context.Background()
	for pass := 0; ; pass++ {
		var owed int
		if err := database.Pool.QueryRow(ctx, `select count(*) from document_block_versions where version is null or store_owed_at is not null`).Scan(&owed); err != nil {
			t.Fatalf("count the documents that owe their block index: %v", err)
		}
		if owed == 0 {
			break
		}
		if pass == 20 {
			t.Fatalf("%d documents still owe their block index after %d passes of its reconciliation", owed, pass)
		}
		reconcileBlockIndexPass(t, deps)
	}
	if _, err := database.Pool.Exec(ctx, `vacuum (analyze)`); err != nil {
		t.Fatalf("vacuum (analyze) the corpus: %v", err)
	}
}

// requireTreeReadShape holds the tree issue to its shape before its read is counted: its one
// document, an upload of readCostTreeBlocks blocks whose deepest block sits readCostTreeDepth list
// levels down, as the blocks and the block-path reads answer it, which the tree read with no view
// lists in its document_nodes with that many blocks and answers none of the blocks of (R8's
// skeleton), on one page.
func requireTreeReadShape(t *testing.T, s *readCostServer, corpus *readCostCorpus) {
	t.Helper()
	shape := &readCostShape{}
	var tree struct {
		Nodes []struct {
			Kind string `json:"kind"`
		} `json:"nodes"`
		DocumentNodes []model.IssueTreeDocumentNode `json:"document_nodes"`
		Cursor        *string                       `json:"cursor"`
	}
	s.read(t, "read the tree", "/api/v1/issues/"+corpus.treeIssue+"/tree", &tree)
	blocks := 0
	for _, node := range tree.Nodes {
		if node.Kind == refs.BlockNodeKind {
			blocks++
		}
	}
	if tree.Cursor != nil {
		shape.failed = append(shape.failed, "the tree read answered a cursor ("+*tree.Cursor+"): the tree does not fit one page")
	}
	shape.hold(t, s.name, "block nodes of the tree read", blocks, 0, true)
	listed := -1
	for _, document := range tree.DocumentNodes {
		if document.ArtifactID == corpus.treeDocument {
			listed = document.BlockCount
		}
	}
	shape.hold(t, s.name, "blocks the tree read's document_nodes counts in the tree's document", listed, readCostTreeBlocks, true)
	var documentBlocks []readCostBlock
	s.read(t, "read the tree's blocks", "/api/v1/artifacts/"+corpus.treeDocument+"/blocks", &documentBlocks)
	shape.hold(t, s.name, "blocks of the tree's document", len(documentBlocks), readCostTreeBlocks, true)
	shape.hold(t, s.name, "levels of the tree's deepest block", readCostDepth(t, s, corpus.treeDocument, documentBlocks), readCostTreeDepth, false)
	shape.require(t, s.name)
}

// sqlCapture is a refs.Queryer that records the SQL of every statement it runs through the pool
// it wraps.
type sqlCapture struct {
	pool *store.Pool
	sql  []string
}

func (capture *sqlCapture) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	capture.sql = append(capture.sql, sql)
	return capture.pool.Query(ctx, sql, args...)
}

func (capture *sqlCapture) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	capture.sql = append(capture.sql, sql)
	return capture.pool.QueryRow(ctx, sql, args...)
}

// childrenReadText is the text of the one statement refs.TreeChildren sends, read off a call of it.
func childrenReadText(t *testing.T, database *store.Store, issue string) string {
	t.Helper()
	capture := &sqlCapture{pool: database.Pool}
	if _, err := refs.TreeChildren(context.Background(), capture, []refs.Target{{Kind: graphstore.KindIssue, ID: issue}}, refs.TreeCut{ReadSeq: 1}); err != nil {
		t.Fatalf("read the children of %s to take the children read's text: %v", issue, err)
	}
	if len(capture.sql) != 1 {
		t.Fatalf("the children read sent %d statements, want its one", len(capture.sql))
	}
	return capture.sql[0]
}

// countedTreeRead reads the tree of issue as the read-cost corpus's reader, with display trees, on
// a request a roundTrips counts.
func countedTreeRead(t *testing.T, handler http.Handler, issue string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.WithValue(context.Background(), countedRead{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/issues/"+issue+"/tree", nil)
	request.Header.Set("X-Dispatch-User", readCostReader)
	request.Header.Set(displayTreesHeader, "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// The tree read of the read-cost corpus's tree issue - whose one document, an upload of 2,000
// blocks whose deepest block sits 6 list levels down, the read with no view lists in its
// document_nodes and answers none of the blocks of (R8), held to that shape first - read as its
// reader with display trees, reads the store set-wise: it sends no statement that reads the
// children of the walk's nodes again, which the walk has read already (docs' IssueTree.Children),
// and it sends at most treeReadStatementBound statements, a batch counted once. A read that put the
// children read back sends that statement, and fails here naming it. This is the half of the read's
// cost that a shared machine's clock cannot blur; TestTheTreeReadAnswersTheSkeletonWithinItsBounds
// times the read.
func TestTheTreeReadSendsNoRepeatedStatement(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	// A settlement of the document is database work of its own; a delay no run reaches keeps it
	// off the read, and the server's shutdown settles the document after the count.
	handler, _, deps := newTestServer(t, testServerOptions{store: traced, settle: time.Hour})
	s := inProcessReadCostServer(handler)
	corpus := seedTreeReadCorpus(t, s, database, deps)
	requireTreeReadShape(t, s, corpus)
	children := childrenReadText(t, database, corpus.treeIssue)

	counter.take()
	response := countedTreeRead(t, handler, corpus.treeIssue)
	sent := counter.take()
	if response.Code != http.StatusOK {
		t.Fatalf("read the tree of %s: status=%d body=%s", corpus.treeIssue, response.Code, response.Body.String())
	}
	for index, statement := range sent {
		t.Logf("statement %d of the tree read: %s", index+1, treeReadStatementHead(statement))
	}
	for index, statement := range sent {
		if statement == children {
			t.Errorf("statement %d of the tree read of %s reads the children of the walk's nodes again, which the walk read: %s",
				index+1, corpus.treeIssue, treeReadStatementHead(statement))
		}
	}
	t.Logf("the tree read of %s sent %d statements, at most %d", corpus.treeIssue, len(sent), treeReadStatementBound)
	if len(sent) > treeReadStatementBound {
		t.Errorf("the tree read of %s sent %d statements, want at most %d", corpus.treeIssue, len(sent), treeReadStatementBound)
	}
}

// treeReadStatementHead is a statement's text on one line, cut to its first 160 bytes.
func treeReadStatementHead(statement string) string {
	head := strings.Join(strings.Fields(statement), " ")
	if len(head) > 160 {
		head = head[:160] + "…"
	}
	return head
}

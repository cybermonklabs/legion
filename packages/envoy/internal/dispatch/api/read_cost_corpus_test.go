package api

// The read-cost corpus's tree issue, as every build of this package's tests shares it: its shape
// as c-read-cost states it, its document and how a test writes it through the server's own API, the
// client a reading is read through, and the reads that hold a corpus to its shape before anything
// is measured. c-read-cost itself is cmd/dispatch's (read_cost_test.go), which keeps a corpus of
// its own. Here the tree read's statement bound (tree_read_statements_test.go) and the head
// projection's reads (head_projection_test.go) seed the tree issue through a server in the test's
// process, and the tests that build an earlier commit and carry the basebuild tag - the skeleton's
// bound, c-tree-read-bound (tree_read_skeleton_bound_test.go), and the anchor roll
// (node_anchor_roll_test.go) - read and write two servers' processes through the same client, as
// the same people.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// The tree issue, as c-read-cost states it: one document of 2,000 blocks, whose deepest block sits
// at a depth of 6. The corpus states its own shape so that a reading is reproducible, and every
// count is held to before anything is measured.
const (
	readCostTreeBlocks = 2_000
	readCostTreeDepth  = 6
)

// readCostWriters is how many writes a seed keeps in flight at once (skeletonInParallel), and so
// how many connections the one client keeps alive (readCostClient).
const readCostWriters = 8

// The people and the project of the corpus. Every person is named by the trusted header, which
// records them as signed in on their first request (identity.HeaderIdentity), so an issue can be
// assigned to them.
const (
	readCostReader  = "reader@read-cost.example"
	readCostWriter  = "writer@read-cost.example"
	readCostProject = "READ"
)

// The credentials. Both builds serve one database in turn, so each carries the same signing key,
// which a process whose key a live peer does not share is refused for (processes.Register).
const (
	readCostSigningKey = "read-cost-signing-key"
	readCostAgentToken = "read-cost-agent-token"
)

// readCostServer is one build's process over the run's database, read and written over HTTP on
// loopback through the one client both sides share, so both sides of every read are read the same
// way and neither runs in the test process.
type readCostServer struct {
	name    string
	commit  string
	port    int
	process *cmdtest.Process
	client  *http.Client
}

// readCostAnswer is one answer of a server: its status, its body, and the time from the request's
// send to its body's last byte. The request body is encoded before that clock starts.
type readCostAnswer struct {
	status int
	body   []byte
	took   time.Duration
}

func (a readCostAnswer) String() string {
	body := strings.TrimSpace(string(a.body))
	if len(body) > 2_000 {
		body = body[:2_000] + "…"
	}
	return fmt.Sprintf("%d %s", a.status, body)
}

// readCostClient is the one HTTP client both sides are read and written through. It keeps a
// connection per writer alive, so no reading pays for a dial.
func readCostClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = readCostWriters
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute}
}

func (s *readCostServer) url(path string) string {
	return "http://127.0.0.1:" + strconv.Itoa(s.port) + path
}

// send sends body as JSON to path, as login - the person the trusted header names, asking for
// display trees as the dashboard asks on every request - or, where login is empty, on the shared
// agent bearer, whose body names the session it acts as. It never fails the test, so a writer
// goroutine may call it.
func (s *readCostServer) send(method, path, login string, body any) (readCostAnswer, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return readCostAnswer{}, fmt.Errorf("encode the body of %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, s.url(path), payload)
	if err != nil {
		return readCostAnswer{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if login == "" {
		request.Header.Set("Authorization", "Bearer "+readCostAgentToken)
	} else {
		request.Header.Set("X-Dispatch-User", login)
		request.Header.Set("Dispatch-Trees", "1")
	}
	started := time.Now()
	response, err := s.client.Do(request)
	if err != nil {
		return readCostAnswer{}, fmt.Errorf("%s %s on %s: %w", method, path, s.name, err)
	}
	defer response.Body.Close()
	read, err := io.ReadAll(response.Body)
	took := time.Since(started)
	if err != nil {
		return readCostAnswer{}, fmt.Errorf("read the answer to %s %s on %s: %w", method, path, s.name, err)
	}
	return readCostAnswer{status: response.StatusCode, body: read, took: took}, nil
}

// write is send that answers an error unless the server answered status.
func (s *readCostServer) write(method, path, login string, body any, status int) (readCostAnswer, error) {
	answer, err := s.send(method, path, login, body)
	if err != nil {
		return answer, err
	}
	if answer.status != status {
		return answer, fmt.Errorf("%s %s on %s answered %s, want %d", method, path, s.name, answer, status)
	}
	return answer, nil
}

// expect is write on the test's goroutine: an answer other than status fails the test, naming what.
func (s *readCostServer) expect(t *testing.T, what, method, path, login string, body any, status int) readCostAnswer {
	t.Helper()
	answer, err := s.write(method, path, login, body, status)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return answer
}

// read is a GET as the reader that must answer 200, decoded into into.
func (s *readCostServer) read(t *testing.T, what, path string, into any) {
	t.Helper()
	answer := s.expect(t, what, http.MethodGet, path, readCostReader, nil, http.StatusOK)
	if err := json.Unmarshal(answer.body, into); err != nil {
		t.Fatalf("%s: decode GET %s on %s: %v: %s", what, path, s.name, err, answer)
	}
}

// readCostCorpus is what the reads address in the corpus: treeIssue holds treeDocument, the
// document of readCostTreeBlocks blocks its tree expands.
type readCostCorpus struct {
	treeIssue, treeDocument string
}

// createReadCostIssue creates an issue of project held by assignee, who also writes it, and answers
// its key. It is forced past the near-copy check, which the corpus's titles, written for what hangs
// under them, would trip.
func createReadCostIssue(t *testing.T, s *readCostServer, project, title, spec, assignee string) string {
	t.Helper()
	body := map[string]any{"project": project, "title": title, "force": true, "assignee": assignee}
	if spec != "" {
		body["spec"] = spec
	}
	answer := s.expect(t, "create the issue "+title, http.MethodPost, "/api/v1/issues", assignee, body, http.StatusCreated)
	var created struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(answer.body, &created); err != nil || created.Key == "" {
		t.Fatalf("read the issue %q back: %v: %s", title, err, answer)
	}
	return created.Key
}

// uploadReadCostDocument writes the first version of a document of issue as login and answers its
// artifact id.
func uploadReadCostDocument(t *testing.T, s *readCostServer, issue, name, content, login string) string {
	t.Helper()
	answer := s.expect(t, "upload "+name+" to "+issue, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", login,
		map[string]string{"name": name, "content": content}, http.StatusCreated)
	var uploaded struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(answer.body, &uploaded); err != nil || uploaded.Artifact.ID == "" {
		t.Fatalf("read the upload of %s back: %v: %s", name, err, answer)
	}
	return uploaded.Artifact.ID
}

// readCostTreeText is the document the tree issue expands: a heading, a spine of bullet lists
// nesting readCostTreeDepth levels deep, a paragraph, a flat list and a closing paragraph, which
// fill it out to readCostTreeBlocks blocks. A list item is a block of its own beside its paragraph
// since R7 gave each list item a node (0ecbefee586 records its offset), so a list of n items is
// 2n+1 blocks - the list, each item and each item's paragraph - and each level of the spine, a list
// of one item, is three: the text written before that counted one block an item and read back as
// 3,991 blocks, not the 2,000 c-read-cost states. The heading, the two paragraphs, the spine and
// the flat list's own block leave an even count for its items, so the count is exactly
// readCostTreeBlocks; it is never taken on trust, since both sides read it back before anything is
// measured.
func readCostTreeText() string {
	var text strings.Builder
	text.WriteString("# The tree this issue expands\n\n")
	for level := range readCostTreeDepth {
		text.WriteString(strings.Repeat("  ", level))
		fmt.Fprintf(&text, "- nesting level %d\n", level+1)
	}
	// Without this paragraph the flat list's items would join the spine's outermost list.
	text.WriteString("\nThe items below fill the tree out.\n\n")
	for index := range (readCostTreeBlocks - 3 - 3*readCostTreeDepth - 1) / 2 {
		fmt.Fprintf(&text, "- item %d of the tree\n", index)
	}
	text.WriteString("\nThe tree ends here.\n")
	return text.String()
}

// readCostDepth is how many list levels down the document's deepest block sits, as the server's
// own block-path read answers it: the path from the top-level block down holds the list and its
// item at each level, so the levels are half its length. It reads the spine, the first blocks of
// the document, and stops at the first block as deep as the shape states.
func readCostDepth(t *testing.T, s *readCostServer, document string, blocks []readCostBlock) int {
	t.Helper()
	deepest := 0
	for _, block := range blocks {
		var path struct {
			Path []struct {
				Type string `json:"type"`
			} `json:"path"`
		}
		s.read(t, "read where a block of the tree stands", "/api/v1/artifacts/"+document+"/blocks/"+block.ID, &path)
		deepest = max(deepest, len(path.Path)/2)
		if deepest >= readCostTreeDepth {
			break
		}
	}
	return deepest
}

// readCostBlock is one block of a document as GET /api/v1/artifacts/{id}/blocks answers it.
type readCostBlock struct {
	ID string `json:"id"`
}

// readCostShape is what one side reads back of the corpus before it is measured, and holds to the
// shape c-read-cost states: a count short of it is not the corpus the check measures, and a read
// measured over it would be a reading of something else.
type readCostShape struct {
	failed []string
}

// hold prints one count side read of the corpus beside the figure the check states, and keeps it
// as a failure where it falls short of least, or, where exact, differs from it: the corpus writes
// exactly that many where its writes alone decide the count, and at least that many where a build
// may answer a row twice, a comment's block beside the comment.
func (shape *readCostShape) hold(t *testing.T, side, what string, read, least int, exact bool) {
	t.Helper()
	held := read >= least
	if exact {
		held = read == least
	}
	word := "at least"
	if exact {
		word = "exactly"
	}
	t.Logf("c-read-cost: %s holds %s: %d, which the check states as %s %d", side, what, read, word, least)
	if !held {
		shape.failed = append(shape.failed, fmt.Sprintf("%s: %d, want %s %d", what, read, word, least))
	}
}

// require fails the run with every count that fell short, before anything is measured over a
// corpus that is not the one the check states.
func (shape *readCostShape) require(t *testing.T, side string) {
	t.Helper()
	if len(shape.failed) > 0 {
		t.Fatalf("c-read-cost: the corpus on %s is not the shape c-read-cost states, so no reading of it is the measurement the check asks for:\n%s",
			side, strings.Join(shape.failed, "\n"))
	}
}

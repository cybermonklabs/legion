package api

// The read-cost check's corpus, as every build of this package's tests shares it: its shape as
// the check states it, the tree issue's document and how the corpus writes it through the
// server's own API, the client a reading is read through, and the reads that hold the corpus to
// its shape before anything is measured. The check itself, which builds an earlier commit and
// carries the basebuild tag, keeps its two servers' processes, the rest of its seeding and its
// reads in its own file; the issue tree read's untagged tests read through the same client and
// hold the tree issue to the same shape.

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The corpus, as c-read-cost states it: one issue whose tree is read, of four documents
// (readCostSpecBlocks), one person with 10,000 delivered entries, one node with 500 backlinks, one
// requirement served by 50 checks, and history unrelated to the actor at ten times the actor's
// own. The corpus states its own shape so that a reading is reproducible, and every count is held
// to before anything is measured.
const (
	// readCostTreeBlocks and readCostTreeDepth are the tree issue's tree document, tree.md, its one
	// document that is no role document: that many blocks, whose deepest block sits that many list
	// levels down.
	readCostTreeBlocks = 2_000
	readCostTreeDepth  = 6
	// readCostEntries is what the reader's inbox holds, delivered as the three kinds of row the
	// Inbox page draws: open asks waiting on them, comment threads they wrote in that somebody
	// else answered, and their one-off messages to an agent that the agent answered.
	readCostEntries      = 10_000
	readCostThreads      = 4_000
	readCostConversation = 1_000
	readCostAsks         = readCostEntries - readCostThreads - readCostConversation
	// readCostAskers is how many agent sessions open the asks, so that no one session's advice,
	// which every ask it opens reads (writeAdvice), reads thousands of open asks of its own.
	readCostAskers = 50
	// readCostBacklinks is how many comments cite the one node the references panel is read on.
	readCostBacklinks = 500
	// readCostChecks is how many check blocks serve the one requirement the coverage read and
	// the chain read are read on.
	readCostChecks = 50
	// The actor's own history is what the reader wrote: the roots of their threads and their
	// messages to the agent. The history unrelated to them is ten times that, as documents of
	// another person in another project, readCostUnrelatedBlocks blocks each.
	readCostOwnHistory        = readCostThreads + readCostConversation
	readCostUnrelatedBlocks   = 2_000
	readCostUnrelatedDocument = 10 * readCostOwnHistory / readCostUnrelatedBlocks
)

// The tree issue's role documents, comments and asks: the issue INFRA-1253's spec v213 holds its
// skeleton read to after INFRA-840, whose plan is the largest role document of the census of every
// INFRA issue (INFRA-5574's load measure, sections 1 and 1b): a spec of 62 blocks, a Verification
// document of 106 and a plan of 2,786, as the blocks read lists them, 47 comments a person wrote
// and 5 asks. Each role document is typed blocks alone - requirements, checks, plan tasks - each
// holding the one paragraph its schema asks for, so the blocks read lists two blocks for each and
// half of a document's blocks are typed: the most its size allows, since no tree read answers a
// role document's prose (R2, R16). v213's Verification seeds typed role blocks alone too, but
// counts 2,786 plan tasks, which with their paragraphs the blocks read would list as 5,572 blocks;
// INFRA-840's own documents hold 14 requirements, 27 checks and 75 plan tasks.
const (
	readCostSpecBlocks         = 62
	readCostVerificationBlocks = 106
	readCostPlanBlocks         = 2_786
	readCostTreeComments       = 47
	readCostTreeAsks           = 5
	// readCostTreeAsker is the agent session that opens the tree issue's asks.
	readCostTreeAsker = "ses_read_cost_tree_asker"
)

// readCostWriters is how many writes the seed keeps in flight at once on the release before: the
// corpus is about 15,000 writes, which one at a time cost the run tens of minutes.
const readCostWriters = 8

// The people, the sessions and the projects of the corpus. Every person is named by the trusted
// header, which records them as signed in on their first request (identity.HeaderIdentity), so an
// issue can be assigned to them.
const (
	readCostReader   = "reader@read-cost.example"
	readCostWriter   = "writer@read-cost.example"
	readCostStranger = "stranger@read-cost.example"
	// readCostConverse is the agent the reader messages, which answers each message.
	readCostConverse = "ses_read_cost_converse"
	readCostProject  = "READ"
	readCostElsewise = "ELSE"
	// readCostSearchWord is the word of the reader's own thread roots that search reads, which no
	// other row of the corpus holds.
	readCostSearchWord = "quillwort"
)

// The credentials and the build stamps. Both builds serve one database in turn, so each carries
// the same signing key, which a process whose key a live peer does not share is refused for
// (processes.Register). readCostOtherBuild is the commit of the other build whose announcement the
// run writes (T7b's step reads the newest one naming a commit other than its own); no build of the
// repository carries it.
const (
	readCostSigningKey = "read-cost-signing-key"
	readCostAgentToken = "read-cost-agent-token"
	readCostOtherBuild = "0123456789abcdef0123456789abcdef01234567"
)

// readCostServer is one build's process over the run's database, read and written over HTTP on
// loopback through the one client both sides share, so both sides of every read are read the same
// way and neither runs in the test process. binary is the build it runs, which a profile of it is
// read against, and pprof the address its profiler serves on, empty where it serves none
// (startReadCostServerProfiled).
type readCostServer struct {
	name    string
	commit  string
	binary  string
	port    int
	pprof   string
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

// write is send that answers an error unless the server answered status. The error quotes the
// tail of the server's own output once it has logged the request (tailOnceLogged), so a failure
// names what the server logged of the answer it did not want, a 500's cause among it.
func (s *readCostServer) write(method, path, login string, body any, status int) (readCostAnswer, error) {
	answer, err := s.send(method, path, login, body)
	if err != nil {
		return answer, err
	}
	if answer.status != status {
		return answer, fmt.Errorf("%s %s on %s answered %s, want %d; the tail of its output:\n%s",
			method, path, s.name, answer, status, s.tailOnceLogged(method, path, answer.status))
	}
	return answer, nil
}

// readCostAnswerLogged bounds how long tailOnceLogged waits for the server to log a request: a
// handler that fails logs its error after it writes the answer (writeHandlerError), and the access
// line follows the handler, so the output read the moment the answer arrives can lack both.
const readCostAnswerLogged = 5 * time.Second

// tailOnceLogged is the tail of the server's output (readCostTail) once that tail holds the access
// line (accesslog.Message) of method path answered status, or once readCostAnswerLogged has passed
// without it.
func (s *readCostServer) tailOnceLogged(method, path string, status int) string {
	escaped := path
	if parsed, err := url.Parse(path); err == nil {
		escaped = parsed.EscapedPath()
	}
	fields := []string{`msg="` + accesslog.Message + `" `, " method=" + method + " ", " path=" + escaped + " ", " status=" + strconv.Itoa(status) + " "}
	tail := ""
	testwait.Holds(readCostAnswerLogged, 50*time.Millisecond, func() bool {
		tail = readCostTail(s.process.Output.String())
		for _, line := range strings.Split(tail, "\n") {
			if !slices.ContainsFunc(fields, func(field string) bool { return !strings.Contains(line, field) }) {
				return true
			}
		}
		return false
	})
	return tail
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

// readCostTail is the last lines of a process's output, which a failure quotes: the whole of it
// holds an access line for every request of the run.
func readCostTail(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-60):], "\n")
}

// readCostCorpus is what the reads address in the corpus.
type readCostCorpus struct {
	// treeIssue is the issue whose tree is read: its spec, its primary document, treeSpec, its
	// Verification document treeVerification and its plan treePlan, of the sizes readCostSpecBlocks
	// states, and its tree document, tree.md, treeDocument, its one document that is no role
	// document, whose blocks read is the tree read's old side. The old page's first load
	// (readCostOldFirstLoad) reads the spec, and its role tabs (readCostOldRoleTabs) the other two.
	treeIssue, treeSpec, treeVerification, treePlan, treeDocument string
	// coverageIssue's spec holds the requirement r1, which readCostChecks checks serve.
	coverageIssue string
	// citedIssue is the node readCostBacklinks comments cite.
	citedIssue string
	// askIssues are the issues assigned to the reader that their asks and threads stand on.
	askIssues []string
}

// readCostRoleDocument is one role document of the tree issue as the shape holds it: its artifact
// id, its slug, what it is, the typed block it is made of, and how many blocks the blocks read lists
// for it, half of them that typed block.
type readCostRoleDocument struct {
	id, slug, name, typ string
	blocks              int
}

// roleDocuments are the tree issue's three role documents.
func (corpus *readCostCorpus) roleDocuments() []readCostRoleDocument {
	return []readCostRoleDocument{
		{corpus.treeSpec, "spec", "spec", "requirement", readCostSpecBlocks},
		{corpus.treeVerification, "verification-md", "Verification document", "check", readCostVerificationBlocks},
		{corpus.treePlan, "plan-md", "plan", "plan_task", readCostPlanBlocks},
	}
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

// readCostTreeText is the tree issue's tree document, tree.md: a heading, a spine of bullet
// lists nesting readCostTreeDepth levels deep, a paragraph, and a flat list filling it out to
// readCostTreeBlocks blocks. Each level of the spine is a list block and the paragraph of its one
// item, and the flat list is one block and each of its items one more, so the count is arithmetic;
// it is never taken on trust, since both sides read it back before anything is measured. It cites
// no issue and writes no #id.
func readCostTreeText() string {
	var text strings.Builder
	text.WriteString("# The tree this issue expands\n\n")
	for level := range readCostTreeDepth {
		text.WriteString(strings.Repeat("  ", level))
		fmt.Fprintf(&text, "- nesting level %d\n", level+1)
	}
	// Without this paragraph the flat list's items would join the spine's outermost list.
	text.WriteString("\nThe items below fill the tree out.\n\n")
	for index := range readCostTreeBlocks - 3 - 2*readCostTreeDepth {
		fmt.Fprintf(&text, "- item %d of the tree\n", index)
	}
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

// requireReadCostNewShape reads the corpus back through this head's own reads, and answers whether
// this head serves the one inbox read, and whether its tree read is the skeleton read
// (requireReadCostTreeReadShape): where the inbox read answers 404 it is not at this head, which
// fails the run naming the route, and every other read is still measured.
func requireReadCostNewShape(t *testing.T, s *readCostServer, corpus *readCostCorpus) (served, skeleton bool) {
	t.Helper()
	shape := &readCostShape{}
	requireReadCostTreeIssueShape(t, s, corpus, shape)
	skeleton = requireReadCostTreeReadShape(t, s, corpus, shape)

	served = true
	inbox, err := s.send(http.MethodGet, "/api/v1/me/inbox?state=inbox", readCostReader, nil)
	switch {
	case err != nil:
		t.Fatalf("read the reader's inbox: %v", err)
	case inbox.status == http.StatusNotFound:
		served = false
		t.Errorf("c-read-cost: %s answers GET /api/v1/me/inbox with %s: this head holds no one inbox read, the read that replaces "+
			"GET /api/v1/inbox, GET /api/v1/me/replies and GET /api/v1/agents/{session_id}/messages, so the inbox has no new "+
			"reading and fails its bound; every other read is still measured", s.name, inbox)
	case inbox.status != http.StatusOK:
		t.Fatalf("read the reader's inbox: GET /api/v1/me/inbox?state=inbox on %s answered %s", s.name, inbox)
	default:
		var entries struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal(inbox.body, &entries); err != nil {
			t.Fatalf("decode the reader's inbox: %v: %s", err, inbox)
		}
		shape.hold(t, s.name, "entries of the reader's one inbox read", entries.Total, readCostEntries, false)
	}

	var chain struct {
		Requirements []struct {
			BlockID string   `json:"block_id"`
			Checks  []string `json:"checks"`
		} `json:"requirements"`
	}
	s.read(t, "read the chain", "/api/v1/issues/"+corpus.coverageIssue+"/chain", &chain)
	checks := -1
	for _, requirement := range chain.Requirements {
		if requirement.BlockID == "r1" || len(chain.Requirements) == 1 {
			checks = len(requirement.Checks)
		}
	}
	shape.hold(t, s.name, "checks the chain read names under the requirement", checks, readCostChecks, true)

	requireReadCostSharedShape(t, s, corpus, shape)
	shape.require(t, s.name)
	return served, skeleton
}

// requireReadCostSharedShape holds what both sides read through the same routes: the backlinks of
// the cited issue, which the references panel lists and its header counts, the checks the coverage
// read counts under the requirement, and an answer of the search the reader makes.
func requireReadCostSharedShape(t *testing.T, s *readCostServer, corpus *readCostCorpus, shape *readCostShape) {
	t.Helper()
	var references struct {
		Edges []json.RawMessage `json:"edges"`
	}
	s.read(t, "read the cited issue's references", "/api/v1/references?to="+url.QueryEscape("dispatch://"+corpus.citedIssue), &references)
	shape.hold(t, s.name, "backlinks the references panel lists", len(references.Edges), readCostBacklinks, false)
	var issue struct {
		ReferencedByCount int `json:"referenced_by_count"`
	}
	s.read(t, "read the cited issue", "/api/v1/issues/"+corpus.citedIssue, &issue)
	shape.hold(t, s.name, "backlinks the cited issue's header counts", issue.ReferencedByCount, readCostBacklinks, false)

	var coverage struct {
		Requirements []struct {
			BlockID string `json:"block_id"`
			Checks  int    `json:"checks"`
		} `json:"requirements"`
	}
	s.read(t, "read the coverage", "/api/v1/issues/"+corpus.coverageIssue+"/coverage", &coverage)
	checks := -1
	for _, requirement := range coverage.Requirements {
		if requirement.BlockID == "r1" {
			checks = requirement.Checks
		}
	}
	shape.hold(t, s.name, "checks the coverage read counts under the requirement", checks, readCostChecks, true)

	var search struct {
		Total int `json:"total"`
	}
	s.read(t, "search", "/api/v1/search?q="+readCostSearchWord, &search)
	shape.hold(t, s.name, "matches of the reader's search", search.Total, 1, false)
}

// requireReadCostTreeIssueShape holds the tree issue to its shape through reads both builds serve:
// the blocks the blocks read lists for each of its role documents and how many of them are the
// document's typed block, the blocks of tree.md and how many levels down its deepest block stands,
// and the tree issue's comments and asks.
func requireReadCostTreeIssueShape(t *testing.T, s *readCostServer, corpus *readCostCorpus, shape *readCostShape) {
	t.Helper()
	for _, role := range corpus.roleDocuments() {
		var blocks []struct {
			Type string `json:"type"`
		}
		s.read(t, "read the blocks of the tree issue's "+role.name, "/api/v1/artifacts/"+role.id+"/blocks", &blocks)
		typed := 0
		for _, block := range blocks {
			if block.Type == role.typ {
				typed++
			}
		}
		shape.hold(t, s.name, "blocks of the tree issue's "+role.name, len(blocks), role.blocks, true)
		shape.hold(t, s.name, role.typ+" blocks of the tree issue's "+role.name, typed, role.blocks/2, true)
	}
	var blocks []readCostBlock
	s.read(t, "read the blocks of tree.md", "/api/v1/artifacts/"+corpus.treeDocument+"/blocks", &blocks)
	shape.hold(t, s.name, "blocks of the tree issue's tree.md", len(blocks), readCostTreeBlocks, true)
	shape.hold(t, s.name, "levels of tree.md's deepest block", readCostDepth(t, s, corpus.treeDocument, blocks), readCostTreeDepth, false)
	var comments, asks []json.RawMessage
	s.read(t, "read the tree issue's comments", "/api/v1/issues/"+corpus.treeIssue+"/comments", &comments)
	s.read(t, "read the tree issue's asks", "/api/v1/issues/"+corpus.treeIssue+"/asks?state=all", &asks)
	shape.hold(t, s.name, "comments on the tree issue", len(comments), readCostTreeComments, true)
	shape.hold(t, s.name, "asks on the tree issue", len(asks), readCostTreeAsks, true)
}

// readCostDocumentNode is one entry of the skeleton read's document_nodes: a document of the issue
// whose blocks the skeleton read leaves out, by its artifact id and its slug, and the blocks the
// blocks read lists for it.
type readCostDocumentNode struct {
	ArtifactID string `json:"artifact_id"`
	Slug       string `json:"slug"`
	BlockCount int    `json:"block_count"`
}

// requireReadCostTreeReadShape reads the tree issue's tree read on this head, prints what it
// answers beside what the skeleton read of INFRA-1253's spec v213 answers for the same issue, and
// holds it to what this head's read answers, answering whether that is the skeleton read.
//
// The skeleton read, which pull request #4180 builds, answers the typed blocks of the issue's role
// documents and no block of its other documents, its newest 200 comments with comments_before null
// where no older one remains, its asks, and document_nodes: an entry for each document whose
// blocks it leaves out, with the blocks the blocks read lists for it. A role document's prose is
// kept out of every query, so a role document gets no entry, and the one entry here is tree.md
// (INFRA-1253's spec v213, R2, R8 and R16). The read it replaces, this head's while the answer
// holds no document_nodes, answers the role documents' typed blocks and every block of the other
// documents (issueTreeBuilder.excluded). So on either read the typed blocks of each type are held,
// and one page; the blocks of tree.md are held to every one on the older read and none on the
// skeleton read; and its comments, its asks, comments_before and document_nodes, the one entry
// tree.md with its readCostTreeBlocks blocks, are held on the skeleton read alone, and printed on
// the older read.
func requireReadCostTreeReadShape(t *testing.T, s *readCostServer, corpus *readCostCorpus, shape *readCostShape) bool {
	t.Helper()
	var tree struct {
		Nodes []struct {
			Kind     string `json:"kind"`
			Supertag string `json:"supertag"`
		} `json:"nodes"`
		Cursor         *string                 `json:"cursor"`
		CommentsBefore *string                 `json:"comments_before"`
		DocumentNodes  *[]readCostDocumentNode `json:"document_nodes"`
	}
	s.read(t, "read the tree", "/api/v1/issues/"+corpus.treeIssue+"/tree", &tree)
	roles := corpus.roleDocuments()
	typed := map[string]int{}
	others, comments, asks := 0, 0, 0
	for _, node := range tree.Nodes {
		switch {
		case node.Kind == refs.BlockNodeKind && slices.ContainsFunc(roles, func(role readCostRoleDocument) bool { return role.typ == node.Supertag }):
			typed[node.Supertag]++
		case node.Kind == refs.BlockNodeKind:
			others++
		case node.Kind == graphstore.KindComment:
			comments++
		case node.Kind == graphstore.KindAsk:
			asks++
		}
	}
	// treeSlug is tree.md's slug, by which, or by its artifact id, document_nodes names it.
	const treeSlug = "tree-md"
	var answeredTyped, wantTyped []string
	for _, role := range roles {
		answeredTyped = append(answeredTyped, fmt.Sprintf("%d %s", typed[role.typ], role.typ))
		wantTyped = append(wantTyped, fmt.Sprintf("%d %s", role.blocks/2, role.typ))
	}
	answeredDocuments := "absent"
	if tree.DocumentNodes != nil {
		var entries []string
		for _, node := range *tree.DocumentNodes {
			entries = append(entries, fmt.Sprintf("%s %d", cmp.Or(node.Slug, node.ArtifactID), node.BlockCount))
		}
		answeredDocuments = fmt.Sprintf("of %d entries (%s)", len(entries), strings.Join(entries, ", "))
	}
	before := "null or absent"
	if tree.CommentsBefore != nil {
		before = *tree.CommentsBefore
	}
	t.Logf("c-read-cost: %s's tree read of %s answers typed blocks of its role documents (%s), %d blocks of tree.md, %d comments, "+
		"%d asks, comments_before %s and document_nodes %s; the skeleton read of INFRA-1253's spec v213 answers typed blocks (%s), "+
		"no block of tree.md, %d comments, %d asks, comments_before null and document_nodes of 1 entry (%s %d)", s.name, corpus.treeIssue,
		strings.Join(answeredTyped, ", "), others, comments, asks, before, answeredDocuments, strings.Join(wantTyped, ", "),
		readCostTreeComments, readCostTreeAsks, treeSlug, readCostTreeBlocks)
	for _, role := range roles {
		shape.hold(t, s.name, role.typ+" blocks of the "+role.name+" the tree read answers", typed[role.typ], role.blocks/2, true)
	}
	if tree.Cursor != nil {
		shape.failed = append(shape.failed, fmt.Sprintf("the tree read answered a cursor (%s): the tree does not fit one page", *tree.Cursor))
	}
	if tree.DocumentNodes == nil {
		shape.hold(t, s.name, "blocks of tree.md the tree read answers, which the skeleton read leaves out", others, readCostTreeBlocks, true)
		return false
	}
	shape.hold(t, s.name, "blocks of tree.md the skeleton read answers", others, 0, true)
	shape.hold(t, s.name, "comments the skeleton read answers", comments, readCostTreeComments, true)
	shape.hold(t, s.name, "asks the skeleton read answers", asks, readCostTreeAsks, true)
	if tree.CommentsBefore != nil {
		shape.failed = append(shape.failed, fmt.Sprintf("the skeleton read answered comments_before %s, though no comment is older than its page", *tree.CommentsBefore))
	}
	shape.hold(t, s.name, "entries of the skeleton read's document_nodes", len(*tree.DocumentNodes), 1, true)
	count := -1
	for _, node := range *tree.DocumentNodes {
		if node.ArtifactID == corpus.treeDocument || node.Slug == treeSlug {
			count = node.BlockCount
		}
	}
	shape.hold(t, s.name, "block_count the skeleton read's document_nodes gives tree.md", count, readCostTreeBlocks, true)
	return true
}

// ───── the six reads ──────────────────────────────────────────────────────

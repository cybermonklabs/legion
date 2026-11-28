package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// What TestTheHeadProjectionAnswersAsTheWalk holds beside the states requireHeadProjectionStates
// writes, which TestTheTwoCutsAnswerAsTheUncutWalk replays too and which this file therefore
// leaves as they are: the test hands that function an observer of its own (headProjectionWatch),
// and then writes a plan of 2,000 plan_task blocks on a server of its own
// (requireHeadProjectionPlan).

// headReleaseBeforeWrites are the classes requireHeadProjectionStates names its writes of the
// release before by (headRig.releaseBefore): each by plain SQL on the marked issue, with no head row
// and no setting, after which the issue's mark is gone and the head read walks the tuples; the
// fill then marks the issue again, and the class is read once more as "refill after the <class>".
var headReleaseBeforeWrites = []string{"release before's tuple", "release before's node", "release before's node row", "release before's comment"}

// headReleaseBeforeWords are the words two of those writes state, which the read of the issue that
// walks the tuples after the write answers.
var headReleaseBeforeWords = map[string]string{
	"release before's tuple":   "A finding the release before rewrote.",
	"release before's comment": "A comment the release before rewrote.",
}

// headAnswers is every answer of the tree read of one issue in one state of the store: by reader
// and view, its body and the statements the read sent, and the newest seq of graph_tuples the
// reads were taken at.
type headAnswers struct {
	seq    int64
	bodies map[headReader][]byte
	sent   map[headReader][]string
}

// headReader is one reader of the tree read: a person and a view, "" for none.
type headReader struct{ login, view string }

// headProjectionWatch observes each state requireHeadProjectionStates holds (headRig.observe),
// once the rig has held every read of it to the walk, and reads the issue's tree once more as each
// reader. After each write of the release before it holds that the read of the unmarked issue
// walked the tuples and answers the new state - an answer the write moved, which holds the words
// the write stated - and, after the fill that marks the issue again, that the head read answers
// what that walk answered. After the read-cost corpus it holds the corpus to its shape: one
// document of 2,000 blocks at depth 6, other than a role document, of whose blocks the read with
// no view answers none (requireTreeReadShape).
type headProjectionWatch struct {
	// last is, by issue, its answers in the state the rig held last.
	last map[string]headAnswers
	// written is, by write of the release before, the answers of the unmarked issue after it.
	written map[string]headAnswers
	// held counts, by write of the release before, the states of it read: after the write and
	// after its refill.
	held   map[string]int
	corpus bool
}

func newHeadProjectionWatch() *headProjectionWatch {
	return &headProjectionWatch{last: map[string]headAnswers{}, written: map[string]headAnswers{}, held: map[string]int{}}
}

// observe is headRig's observe: it reads the state the rig has just held and holds it to what the
// state's class says of it.
func (w *headProjectionWatch) observe(r headRig, class, key string, logins []string) {
	r.t.Helper()
	answers := w.read(r, key, logins)
	refilled, isRefill := strings.CutPrefix(class, "refill after the ")
	switch {
	case slices.Contains(headReleaseBeforeWrites, class):
		w.requireNewState(r, class, key, answers)
	case isRefill && slices.Contains(headReleaseBeforeWrites, refilled):
		w.requireRefilledAsWalked(r, refilled, class, key, answers)
	case class == "read-cost corpus":
		var document string
		if err := r.database.Pool.QueryRow(context.Background(),
			`select id::text from artifacts where issue_key = $1 and name = 'tree.md'`, key).Scan(&document); err != nil {
			r.t.Fatalf("read the document of the read-cost corpus's issue %s: %v", key, err)
		}
		requireTreeReadShape(r.t, inProcessReadCostServer(r.handler), &readCostCorpus{treeIssue: key, treeDocument: document})
		w.corpus = true
	}
	w.last[key] = answers
}

// read reads the tree of key as each of logins with no view and with each view, at one seq of
// graph_tuples: a set of reads the store moved under is read again.
func (w *headProjectionWatch) read(r headRig, key string, logins []string) headAnswers {
	r.t.Helper()
	for attempt := 1; ; attempt++ {
		answers := headAnswers{seq: r.seq(), bodies: map[headReader][]byte{}, sent: map[headReader][]string{}}
		for _, login := range logins {
			for _, view := range append([]string{""}, issueTreeViews...) {
				_, body, sent := r.read(key, view, login)
				answers.bodies[headReader{login, view}] = body
				answers.sent[headReader{login, view}] = sent
			}
		}
		if r.seq() == answers.seq {
			return answers
		}
		if attempt == 5 {
			r.t.Fatalf("the store moved under each of %d sets of reads of %s", attempt, key)
		}
	}
}

// requireNewState holds the reads of key after class, a write of the release before: each walked
// the tuples, since the write took the issue's mark away, at least one answers otherwise than in
// the state before the write, and the words the write stated, where it states any, are answered.
func (w *headProjectionWatch) requireNewState(r headRig, class, key string, answers headAnswers) {
	r.t.Helper()
	before, read := w.last[key]
	if !read {
		r.t.Fatalf("no state of %s was read before the %s write", key, class)
	}
	moved := 0
	for _, reader := range slices.SortedFunc(maps.Keys(answers.bodies), compareHeadReaders) {
		if !slices.Contains(answers.sent[reader], r.walked) {
			r.t.Errorf("after the %s write the head read of %s as %s with view %q sent no statement of the walk: the read of an issue no mark names walks the tuples",
				class, key, reader.login, reader.view)
		}
		if !bytes.Equal(answers.bodies[reader], before.bodies[reader]) {
			moved++
		}
	}
	if moved == 0 {
		r.t.Errorf("after the %s write each of the %d reads of %s answers what it answered before the write: the head read answers no new state",
			class, len(answers.bodies), key)
	}
	if words, stated := headReleaseBeforeWords[class]; stated && !slices.ContainsFunc(slices.Collect(maps.Values(answers.bodies)), func(body []byte) bool {
		return bytes.Contains(body, []byte(words))
	}) {
		r.t.Errorf("after the %s write no read of %s answers %q, the words the write stated", class, key, words)
	}
	r.t.Logf("after the %s write %d of the %d reads of %s, each of which walked the tuples, answer otherwise than before it", class, moved, len(answers.bodies), key)
	w.written[class] = answers
	w.held[class]++
}

// requireRefilledAsWalked holds the head reads of key once the fill has marked it again after
// written, a write of the release before (class names that state): each answers, byte for byte,
// what the read that walked the tuples after the write answered.
func (w *headProjectionWatch) requireRefilledAsWalked(r headRig, written, class, key string, answers headAnswers) {
	r.t.Helper()
	walked, read := w.written[written]
	if !read {
		r.t.Fatalf("the %s write was not read before its refill", written)
	}
	for _, reader := range slices.SortedFunc(maps.Keys(answers.bodies), compareHeadReaders) {
		if node, got, want, differ := treeDifference(answers.bodies[reader], walked.bodies[reader]); differ {
			r.t.Errorf("after the %s write, as %s with view %q: the head read of %s answers %s as %s, the walk as %s",
				class, reader.login, reader.view, key, node, got, want)
		}
	}
	r.t.Logf("after the %s write each of the %d head reads of %s answers what its walk answered after the write (graph_tuples at seq %d, then %d)",
		class, len(answers.bodies), key, walked.seq, answers.seq)
	w.held[written]++
}

// requireSeen fails for each write of the release before not read in both its states, and where
// the read-cost corpus was not read: the observer held nothing of what it names.
func (w *headProjectionWatch) requireSeen(t *testing.T) {
	t.Helper()
	for _, class := range headReleaseBeforeWrites {
		if w.held[class] != 2 {
			t.Errorf("the %s write was read in %d of its two states, after it and after its refill", class, w.held[class])
		}
	}
	if !w.corpus {
		t.Errorf("the read-cost corpus was not read")
	}
}

// compareHeadReaders orders readers by login, then by view.
func compareHeadReaders(a, b headReader) int {
	return strings.Compare(a.login+"\x00"+a.view, b.login+"\x00"+b.view)
}

// headPlanTasks is how many plan_task blocks the plan of TestTheHeadProjectionAnswersAsTheWalk's
// plan case holds.
const headPlanTasks = 2000

// headPlanBlockType is the type of the plan's blocks: a typed block of a role document, the plan.
const headPlanBlockType = "plan_task"

// requireHeadProjectionPlan is the plan case of TestTheHeadProjectionAnswersAsTheWalk, on a server
// of its own: an issue whose plan holds headPlanTasks plan_task blocks, written through the
// document upload and then the block index's tick. After each write every page of the tree read,
// with no view and with each view, as the issue's author and as another person, equals byte for
// byte the same page read from the walk with the issue's mark taken away (requireEveryPageAsWalk),
// the mark stands, and the census kind issue_head_node reads no difference. The plan's blocks are
// typed role blocks, so the read with no view answers each of them, as each reader, from the head
// and from the walk (requirePlanAnswered).
func requireHeadProjectionPlan(t *testing.T) {
	t.Helper()
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, deps, _ := newRegisteredTestServer(t, testServerOptions{store: traced, settle: time.Hour})
	documents := deps.Docs.(*docs.Service)

	const key = "HPP-1"
	issue := createInteractionIssue(t, handler, "HPP", "A plan of two thousand tasks", "The issue whose plan the head projection holds.\n")
	if issue.Key != key {
		t.Fatalf("the plan's issue is %s, want %s", issue.Key, key)
	}
	r := newHeadRig(t, handler, database, counter, key)
	r.requireMarked(key)
	r.after("plan's issue create", key)

	tasks := make([]string, 0, headPlanTasks)
	var plan strings.Builder
	for index := range headPlanTasks {
		task := fmt.Sprintf("t-%04d", index+1)
		tasks = append(tasks, task)
		fmt.Fprintf(&plan, ":::%s{#%s}\nTask %d of the plan.\n:::\n\n", headPlanBlockType, task, index+1)
	}
	uploadDocument(t, handler, "/api/v1/issues/"+key, "plan.md", plan.String())
	r.after("plan upload", key)
	r.requireEveryPageAsWalk("plan upload", key)
	r.requirePlanAnswered("plan upload", key, tasks)

	settleIssue(t, documents, key)
	indexOwedDocuments(t, database, deps)
	r.after("block index tick of the plan", key)
	r.requireEveryPageAsWalk("block index tick of the plan", key)
	r.requirePlanAnswered("block index tick of the plan", key, tasks)
}

// headPage is one page of the tree read: its status, its body and the statements it sent.
type headPage struct {
	status int
	body   []byte
	sent   []string
}

// readPage is GET /api/v1/issues/{key}/tree with view, or with none, from cursor, or from the
// first page, as login with display trees, on a request the rig's roundTrips counts.
func (r headRig) readPage(key, view, login, cursor string) headPage {
	r.t.Helper()
	query := url.Values{}
	if view != "" {
		query.Set("view", view)
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	target := "/api/v1/issues/" + key + "/tree"
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request := httptest.NewRequestWithContext(context.WithValue(context.Background(), countedRead{}, true), http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", login)
	request.Header.Set(displayTreesHeader, "1")
	response := httptest.NewRecorder()
	r.counter.take()
	r.handler.ServeHTTP(response, request)
	sent := r.counter.take()
	if response.Code >= http.StatusInternalServerError {
		r.t.Fatalf("read the tree of %s with view %q from cursor %q as %s: status=%d body=%s", key, view, cursor, login, response.Code, cut(response.Body.Bytes()))
	}
	return headPage{status: response.Code, body: response.Body.Bytes(), sent: sent}
}

// pages reads every page of the tree of key with view as login, following each page's cursor
// until a page answers none.
func (r headRig) pages(key, view, login string) []headPage {
	r.t.Helper()
	var read []headPage
	cursor := ""
	for {
		page := r.readPage(key, view, login, cursor)
		read = append(read, page)
		var next struct {
			Cursor *string `json:"cursor"`
		}
		if page.status != http.StatusOK || json.Unmarshal(page.body, &next) != nil || next.Cursor == nil || *next.Cursor == "" {
			return read
		}
		if len(read) == 100 {
			r.t.Fatalf("the tree read of %s with view %q as %s answered a cursor on each of %d pages", key, view, login, len(read))
		}
		cursor = *next.Cursor
	}
}

// requireEveryPageAsWalk reads every page of the tree of key, with no view and with each view, as
// alice and as bob, from the head and from the walk (the mark taken away for the walk's pages and
// put back, withoutMark), and fails on each page the two answer apart, naming the first node that
// differs, and on each head read of the marked issue that walked the tuples. A pair of reads the
// store moved between is read again.
func (r headRig) requireEveryPageAsWalk(class, key string) {
	r.t.Helper()
	if _, marked := r.mark(key); !marked {
		r.t.Errorf("the %s write took the mark of %s away", class, key)
		r.refill(key)
	}
	pages := 0
	for _, login := range []string{"alice", "bob"} {
		for _, view := range append([]string{""}, issueTreeViews...) {
			for attempt := 1; ; attempt++ {
				before := r.seq()
				head := r.pages(key, view, login)
				var walk []headPage
				r.withoutMark(r.t, key, true, func() (int, []byte) {
					walk = r.pages(key, view, login)
					return 0, nil
				})
				if r.seq() != before {
					if attempt == 5 {
						r.t.Fatalf("the store moved under each of %d pairs of paged reads of %s", attempt, key)
					}
					continue
				}
				for index := range max(len(head), len(walk)) {
					if index >= len(head) || index >= len(walk) {
						r.t.Errorf("after the %s write, as %s with view %q: the head read of %s answers %d pages, the walk %d",
							class, login, view, key, len(head), len(walk))
						break
					}
					if slices.Contains(head[index].sent, r.walked) {
						r.t.Errorf("the head read of marked %s walked the tuples (view %q, as %s, page %d, after the %s write)", key, view, login, index+1, class)
					}
					if head[index].status != walk[index].status {
						r.t.Errorf("after the %s write, as %s with view %q, page %d: the head read of %s answers the answer as status %d %s, the walk as status %d %s",
							class, login, view, index+1, key, head[index].status, cut(head[index].body), walk[index].status, cut(walk[index].body))
					} else if node, got, want, differ := treeDifference(head[index].body, walk[index].body); differ {
						r.t.Errorf("after the %s write, as %s with view %q, page %d: the head read of %s answers %s as %s, the walk as %s",
							class, login, view, index+1, key, node, got, want)
					}
				}
				pages += len(head)
				break
			}
		}
	}
	r.t.Logf("after the %s write: %d pages of %s from the head, each held to its walk page by page", class, pages, key)
}

// requirePlanAnswered holds the read with no view of key, as alice and as bob, from the head and
// from the walk, to answering a node of each of tasks, the ids of the plan's plan_task blocks,
// across its pages: the plan is a role document and its blocks typed role blocks, which the
// skeleton of the read with no view answers.
func (r headRig) requirePlanAnswered(class, key string, tasks []string) {
	r.t.Helper()
	for _, login := range []string{"alice", "bob"} {
		head := r.pages(key, "", login)
		var walk []headPage
		r.withoutMark(r.t, key, true, func() (int, []byte) {
			walk = r.pages(key, "", login)
			return 0, nil
		})
		for _, read := range []struct {
			from  string
			pages []headPage
		}{{"head", head}, {"walk", walk}} {
			answered := map[string]bool{}
			for _, page := range read.pages {
				var tree struct {
					Nodes []struct {
						BlockID  string `json:"block_id"`
						Supertag string `json:"supertag"`
					} `json:"nodes"`
				}
				if err := json.Unmarshal(page.body, &tree); err != nil {
					r.t.Fatalf("decode the tree read of %s with no view as %s from the %s: %v: %s", key, login, read.from, err, cut(page.body))
				}
				for _, node := range tree.Nodes {
					if node.Supertag == headPlanBlockType {
						answered[node.BlockID] = true
					}
				}
			}
			var missing []string
			for _, task := range tasks {
				if !answered[task] {
					missing = append(missing, task)
				}
			}
			if len(missing) > 0 {
				r.t.Errorf("after the %s write the read with no view of %s as %s, from the %s, answers %d of its plan's %d plan_task blocks over %d pages: it answers none of %s",
					class, key, login, read.from, len(tasks)-len(missing), len(tasks), len(read.pages), strings.Join(missing[:min(len(missing), 10)], ", "))
			}
			r.t.Logf("after the %s write the read with no view of %s as %s, from the %s, answers %d plan_task blocks over %d pages, %d of the plan's %d among them",
				class, key, login, read.from, len(answered), len(read.pages), len(tasks)-len(missing), len(tasks))
		}
	}
}

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// headRig reads the tree of an issue through the server's own route twice over: as the head read
// answers it, from the issue's rows of the head projection where the issue is marked
// (issue_head_marks), and as the walk of the tuples answers it, the same read with the issue's mark
// taken away for that one read and put back after it. Its server's pool is traced, so it sees the
// statements each read sent (roundTrips): a read of a marked issue that sends the walk's subtrees
// statement walked the tuples.
type headRig struct {
	t        *testing.T
	handler  http.Handler
	database *store.Store
	counter  *roundTrips
	// walked is the text of the statement the walk of the tuples reads the subtrees with
	// (refs.TreeSubtrees), which a head read never sends.
	walked string
	// served is the server the handler mounts; observe, where it is set, is handed each state of
	// the store the rig holds (requireAsWalk) once the rig has held it, with the readers it read
	// that state as (TestTheTwoCutsAnswerAsTheUncutWalk).
	served  *server
	observe func(r headRig, class, key string, logins []string)
}

// newHeadRig is the rig of the issue key over handler, whose pool counter traces, and database: it
// holds the walk's subtrees statement as refs.TreeSubtrees sends it for the issue.
func newHeadRig(t *testing.T, handler http.Handler, database *store.Store, counter *roundTrips, key string) headRig {
	t.Helper()
	capture := &sqlCapture{pool: database.Pool}
	if _, err := refs.TreeSubtrees(context.Background(), capture, []refs.Target{{Kind: graphstore.KindIssue, ID: key}}, key, refs.TreeCut{ReadSeq: 1}); err != nil || len(capture.sql) != 1 {
		t.Fatalf("read the walk's subtrees statement: %d statements, %v", len(capture.sql), err)
	}
	return headRig{t: t, handler: handler, database: database, counter: counter, walked: capture.sql[0]}
}

// headWho is the writer a write of the release before names on the tuples it appends.
const headWho = `{"kind":"user","id":"alice"}`

// releaseBeforeTuple is an append to graph_tuples by plain SQL, as a build with no head write makes
// it: no head row, and no dispatch.head_rows setting.
const releaseBeforeTuple = `
	insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
	values ($1, $2, $3, $4, $5, clock_timestamp(), '` + headWho + `', 0)`

// mark is when the issue key was marked whole, and whether a mark names it.
func (r headRig) mark(key string) (time.Time, bool) {
	r.t.Helper()
	var filled time.Time
	err := r.database.Pool.QueryRow(context.Background(), `select filled_at from issue_head_marks where issue_key = $1`, key).Scan(&filled)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false
	}
	if err != nil {
		r.t.Fatalf("read the head mark of %s: %v", key, err)
	}
	return filled, true
}

// seq is the newest seq of graph_tuples: two reads taken while it stands read one store.
func (r headRig) seq() int64 {
	r.t.Helper()
	var seq int64
	if err := r.database.Pool.QueryRow(context.Background(), `select coalesce(max(seq), 0) from graph_tuples`).Scan(&seq); err != nil {
		r.t.Fatalf("read the newest seq of graph_tuples: %v", err)
	}
	return seq
}

// read is GET /api/v1/issues/{key}/tree with view, or with none, as login with display trees, on a
// request the rig's roundTrips counts: its status, its body and the statements it sent.
func (r headRig) read(key, view, login string) (int, []byte, []string) {
	r.t.Helper()
	target := "/api/v1/issues/" + key + "/tree"
	if view != "" {
		target += "?view=" + url.QueryEscape(view)
	}
	request := httptest.NewRequestWithContext(context.WithValue(context.Background(), countedRead{}, true), http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", login)
	request.Header.Set(displayTreesHeader, "1")
	response := httptest.NewRecorder()
	r.counter.take()
	r.handler.ServeHTTP(response, request)
	sent := r.counter.take()
	if response.Code >= http.StatusInternalServerError {
		r.t.Fatalf("read the tree of %s with view %q as %s: status=%d body=%s", key, view, login, response.Code, response.Body.String())
	}
	return response.Code, response.Body.Bytes(), sent
}

// walkRead is read with the mark of key taken away for its length, so the route walks the tuples
// at the head cut, and put back after it as it stood (withoutMark).
func (r headRig) walkRead(key, view, login string) (int, []byte) {
	r.t.Helper()
	return r.withoutMark(r.t, key, true, func() (int, []byte) {
		status, body, _ := r.read(key, view, login)
		return status, body
	})
}

// withoutMark runs read with the mark of key taken away for its length, so the read walks the
// tuples, and puts the mark back as it stood. A key no mark names fails t where required, and is
// read as it stands otherwise.
func (r headRig) withoutMark(t *testing.T, key string, required bool, read func() (int, []byte)) (int, []byte) {
	t.Helper()
	ctx := context.Background()
	var filled time.Time
	err := r.database.Pool.QueryRow(ctx, `delete from issue_head_marks where issue_key = $1 returning filled_at`, key).Scan(&filled)
	marked := err == nil
	if err != nil && (required || !errors.Is(err, pgx.ErrNoRows)) {
		t.Fatalf("take the mark of %s away to read its walk: %v", key, err)
	}
	status, body := read()
	if marked {
		if _, err := r.database.Pool.Exec(ctx, `insert into issue_head_marks (issue_key, filled_at) values ($1, $2) on conflict (issue_key) do nothing`, key, filled); err != nil {
			t.Fatalf("put the mark of %s back: %v", key, err)
		}
	}
	return status, body
}

// requireMarked fails unless a mark names key, the write that made its first tree node having
// marked it.
func (r headRig) requireMarked(key string) {
	r.t.Helper()
	if _, marked := r.mark(key); !marked {
		r.t.Errorf("%s holds no mark after its first tree node", key)
	}
}

// after holds the store after a write of class on the issue key: the write left the issue's mark
// standing, every read of its tree answers as its walk does (requireAsWalk), and the census kind
// issue_head_node reads no difference. A write that took the mark away is reported, and the issue
// filled again, so the next write is read from the head too.
func (r headRig) after(class, key string, logins ...string) {
	r.t.Helper()
	if _, marked := r.mark(key); !marked {
		r.t.Errorf("the %s write took the mark of %s away", class, key)
		r.refill(key)
	}
	r.requireAsWalk(class, key, logins...)
}

// requireAsWalk reads the tree of key with no view and with each view the route answers, as each of
// logins (alice and bob where none is named), as the head read answers it and as the walk does,
// and fails on each answer that differs, naming the first node that differs; then it takes the
// census kind issue_head_node (requireCensus), and hands the state to observe where the rig holds
// one. A read of a marked issue that walked the tuples fails too.
func (r headRig) requireAsWalk(class, key string, logins ...string) {
	r.t.Helper()
	if len(logins) == 0 {
		logins = []string{"alice", "bob"}
	}
	var heads, walks, nodes int
	statuses := map[int]int{}
	for _, login := range logins {
		for _, view := range append([]string{""}, issueTreeViews...) {
			marked, status, answered := r.compare(class, key, view, login)
			if marked {
				heads++
			} else {
				walks++
			}
			statuses[status]++
			nodes = max(nodes, answered)
		}
	}
	r.t.Logf("after the %s write: %d reads of %s from the head and %d of an unmarked issue, each held to its walk, statuses %v, at most %d nodes",
		class, heads, key, walks, statuses, nodes)
	r.requireCensus(class)
	if r.observe != nil {
		r.observe(r, class, key, logins)
	}
}

// compare reads the tree of key with view as login from the head and from the walk, and fails
// naming the first node the two answer apart; it answers whether the issue was marked, the read's
// status and how many nodes it answered. A pair of reads the store moved between is read again.
func (r headRig) compare(class, key, view, login string) (bool, int, int) {
	r.t.Helper()
	for attempt := 1; ; attempt++ {
		before := r.seq()
		_, marked := r.mark(key)
		headStatus, head, sent := r.read(key, view, login)
		if marked {
			for _, statement := range sent {
				if statement == r.walked {
					r.t.Errorf("the head read of marked %s walked the tuples (view %q, as %s, after the %s write)", key, view, login, class)
					break
				}
			}
		}
		walkStatus, walk := headStatus, head
		if marked {
			walkStatus, walk = r.walkRead(key, view, login)
		}
		if r.seq() != before {
			if attempt == 5 {
				r.t.Fatalf("the store moved under each of %d pairs of reads of %s", attempt, key)
			}
			continue
		}
		if headStatus != walkStatus {
			r.t.Errorf("after the %s write, as %s with view %q: the head read of %s answers the answer as status %d %s, the walk as status %d %s",
				class, login, view, key, headStatus, cut(head), walkStatus, cut(walk))
		} else if node, got, want, differ := treeDifference(head, walk); differ {
			r.t.Errorf("after the %s write, as %s with view %q: the head read of %s answers %s as %s, the walk as %s", class, login, view, key, node, got, want)
		}
		var answered struct {
			Nodes []json.RawMessage `json:"nodes"`
		}
		_ = json.Unmarshal(head, &answered)
		return marked, headStatus, len(answered.Nodes)
	}
}

// treeDifference is the first node two answers of the tree read hold apart, byte for byte, in the
// order the answers list them, with what each answers of it; or the answer itself where every
// node is alike and the members beside them differ.
func treeDifference(head, walk []byte) (node, got, want string, differ bool) {
	if bytes.Equal(head, walk) {
		return "", "", "", false
	}
	var headRead, walkRead struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if json.Unmarshal(head, &headRead) != nil || json.Unmarshal(walk, &walkRead) != nil {
		return "the answer", cut(head), cut(walk), true
	}
	for index := range max(len(headRead.Nodes), len(walkRead.Nodes)) {
		var held, walked json.RawMessage
		if index < len(headRead.Nodes) {
			held = headRead.Nodes[index]
		}
		if index < len(walkRead.Nodes) {
			walked = walkRead.Nodes[index]
		}
		if !bytes.Equal(held, walked) {
			return treeNodeName(index, walked, held), orNothing(held), orNothing(walked), true
		}
	}
	return "the members beside its nodes", cut(head), cut(walk), true
}

// treeNodeName names the node at index of an answer by its kind and id, read off the first of
// nodes that holds one.
func treeNodeName(index int, nodes ...json.RawMessage) string {
	for _, node := range nodes {
		var named struct{ Kind, ID string }
		if json.Unmarshal(node, &named) == nil && named.Kind != "" {
			return "node " + strconv.Itoa(index) + " (" + named.Kind + " " + named.ID + ")"
		}
	}
	return "node " + strconv.Itoa(index)
}

// orNothing is a node as an answer holds it, or "nothing" where it holds none at that place.
func orNothing(node json.RawMessage) string {
	if node == nil {
		return "nothing"
	}
	return cut(node)
}

// answerAsTheReleaseBefore answers ask on issue as the answer route of the release before does,
// under the issue's owner lock: the asks row and its ask.answered event, and nothing stated on the
// ask's node. It answers the event's id.
func answerAsTheReleaseBefore(t *testing.T, database *store.Store, issue, ask, selected string) int64 {
	t.Helper()
	ctx := context.Background()
	var event int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := store.LockOwner(ctx, tx, store.IssueOwner(issue)); err != nil {
			return err
		}
		answer, err := json.Marshal(model.AskAnswer{User: "alice", Selected: []string{selected}, At: time.Now().UTC()})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1::uuid`, ask, answer); err != nil {
			return err
		}
		answered, err := docs.ScanAsk(tx.QueryRow(ctx, `select `+docs.AskColumns+` from asks a where a.id = $1::uuid`, ask))
		if err != nil {
			return err
		}
		appended, err := events.NewBroker().Append(ctx, tx, model.Event{
			IssueKey: &issue, Type: "ask.answered", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.AskAnsweredEventPayload{AskEventPayload: model.NewAskEventPayload(answered, model.ReferenceChanges{})},
		})
		event = appended.ID
		return err
	}); err != nil {
		t.Fatalf("answer ask %s as the release before: %v", ask, err)
	}
	return event
}

// cut is body cut to its first 2,000 bytes.
func cut(body []byte) string {
	if len(body) > 2000 {
		return string(body[:2000]) + "…"
	}
	return string(body)
}

// requireCensus takes the census kind issue_head_node (graphcensus.CensusOfHeads) and fails on any
// difference it reads, naming the first.
func (r headRig) requireCensus(class string) {
	r.t.Helper()
	ctx := context.Background()
	var kind graphcensus.KindReport
	if err := pgx.BeginFunc(ctx, r.database.Pool, func(tx pgx.Tx) error {
		var err error
		kind, err = graphcensus.CensusOfHeads(ctx, tx)
		return err
	}); err != nil {
		r.t.Fatalf("take the census kind issue_head_node after the %s write: %v", class, err)
	}
	if differences := kind.RowDifferences + kind.NodeDifferences; differences > 0 {
		first := append(kind.ReportedRows, kind.ReportedNodes...)[0]
		r.t.Errorf("after the %s write the census kind issue_head_node reads %d difference(s) at %s: %s", class, differences, first.At, first.Cause)
	}
}

// refill fills the issues no mark names (graphcensus.FillIssueHeads), as the block index's tick
// does, until key is marked again.
func (r headRig) refill(key string) {
	r.t.Helper()
	for pass := 0; ; pass++ {
		if _, marked := r.mark(key); marked {
			return
		}
		if pass == 5 {
			r.t.Fatalf("%s holds no mark after %d fills", key, pass)
		}
		if _, _, err := graphcensus.FillIssueHeads(context.Background(), r.database, graphcensus.HeadFillBatch); err != nil {
			r.t.Fatalf("fill the issues no mark names: %v", err)
		}
	}
}

// releaseBefore runs write, a write of the release before on the marked issue key by plain SQL,
// in one transaction, and holds what follows: the issue's mark is gone once it commits, and every
// read answers as the walk does. Then it fills the issue again and holds the reads once more.
func (r headRig) releaseBefore(class, key string, write func(ctx context.Context, tx pgx.Tx) error) {
	r.t.Helper()
	r.requireMarked(key)
	if err := pgx.BeginFunc(context.Background(), r.database.Pool, func(tx pgx.Tx) error { return write(context.Background(), tx) }); err != nil {
		r.t.Fatalf("the %s write, as a build with no head write makes it: %v", class, err)
	}
	if _, marked := r.mark(key); marked {
		r.t.Errorf("the %s write, a write of the release before, left the mark of %s standing", class, key)
	}
	r.requireAsWalk(class, key)
	r.refill(key)
	r.requireAsWalk("refill after the "+class, key)
}

// backendWaits says whether the backend whose pid $1 binds waits for a lock.
const backendWaits = `select exists (select 1 from pg_locks where pid = $1 and not granted)`

// issueLockWaited says whether some backend waits for an advisory lock whose first key is one of
// $1: an advisory lock of two int4 keys holds the first in classid. The fill takes the issue's owner
// lock and then its head-fill lock (store.OwnerLockNamespace, store.HeadFillLockNamespace), so a
// fill that waits for a write waits on one of them.
const issueLockWaited = `select exists (
	select 1 from pg_locks where locktype = 'advisory' and not granted and classid = any($1::int4[]::oid[]) and objsubid = 2)`

// awaitWaiting waits until waiting, one of the reads of pg_locks above over arg, answers that a lock
// is waited for, or the work done reports has ended, and answers whether it ended, with its error.
func (r headRig) awaitWaiting(waiting string, arg any, done <-chan error) (bool, error) {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-done:
			return true, err
		default:
		}
		var waits bool
		if err := r.database.Pool.QueryRow(context.Background(), waiting, arg).Scan(&waits); err != nil {
			r.t.Fatalf("read the locks waited for (%v): %v", arg, err)
		}
		if waits {
			return false, nil
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("no lock is waited for (%v) and the work has not ended after 30s", arg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// markedWritesWait says that on a marked issue the second of two writes of the release before
// waits for the first to commit: the first deletes the mark's row, and the second's delete of that
// row waits for it (twoReleaseBeforeWrites). false holds the marked form to what the unmarked one
// holds: neither write waits.
const markedWritesWait = true

// twoReleaseBeforeWrites runs two plain-SQL writes of the release before on key, a text value of
// first and then one of second, each in its own transaction under a lock timeout of 2s, the second
// made while the first is uncommitted. Where waits is false the second may not wait: a wait, which
// ends in its lock timeout, fails the test. Where it is true the second must wait, and completes once
// the first commits. Both commit, and no mark names the issue after them. form names the case in
// the log.
func (r headRig) twoReleaseBeforeWrites(form, key, first, second string, waits bool) {
	r.t.Helper()
	ctx := context.Background()
	writes := make([]pgx.Tx, 2)
	var pid int32
	for index := range writes {
		writes[index], pid = r.begin()
		if _, err := writes[index].Exec(ctx, `set local lock_timeout = '2s'`); err != nil {
			r.t.Fatalf("set the lock timeout of write %d of the release before: %v", index+1, err)
		}
	}
	if _, err := writes[0].Exec(ctx, releaseBeforeTuple, refs.BlockNodeKind, first, graphstore.AttributeText, graphstore.KindValue,
		"The first of two writes of the release before ("+form+")."); err != nil {
		r.t.Fatalf("the first of two writes of the release before on %s (%s): %v", key, form, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := writes[1].Exec(ctx, releaseBeforeTuple, refs.BlockNodeKind, second, graphstore.AttributeText, graphstore.KindValue,
			"The second of two writes of the release before ("+form+").")
		done <- err
	}()
	ended, err := r.awaitWaiting(backendWaits, pid, done)
	waited := !ended
	var waitedOn string
	if waited {
		if readErr := r.database.Pool.QueryRow(ctx, `select coalesce(string_agg(locktype, ', '), '') from pg_locks where pid = $1 and not granted`, pid).Scan(&waitedOn); readErr != nil {
			r.t.Fatalf("read the lock the second write waits on: %v", readErr)
		}
		if waits && waitedOn == "advisory" {
			r.t.Errorf("of two writes of the release before on marked %s the second waited on the head-fill lock, not on the mark's row", key)
		}
	}
	if waited && !waits {
		err = r.awaitDone("the second of two writes of the release before", done)
	}
	if commitErr := writes[0].Commit(ctx); commitErr != nil {
		r.t.Fatalf("commit the first of two writes of the release before on %s (%s): %v", key, form, commitErr)
	}
	if waited && waits {
		err = r.awaitDone("the second of two writes of the release before", done)
	}
	switch {
	case !waits && (waited || err != nil):
		r.t.Errorf("two writes of the release before on %s waited on each other's head-fill lock: %v", key, err)
	case waits && !waited:
		r.t.Errorf("of two writes of the release before on marked %s the second did not wait for the first's delete of the mark (%v)", key, err)
	case err != nil:
		r.t.Errorf("the second of two writes of the release before on %s (%s): %v", key, form, err)
	}
	if err == nil {
		if commitErr := writes[1].Commit(ctx); commitErr != nil {
			r.t.Fatalf("commit the second of two writes of the release before on %s (%s): %v", key, form, commitErr)
		}
	}
	if _, marked := r.mark(key); marked {
		r.t.Errorf("the mark of %s stands after two writes of the release before (%s)", key, form)
	}
	r.t.Logf("two writes of the release before, %s form, on %s: the second waited %t (on %q) and ended with %v; the mark is gone",
		form, key, waited, waitedOn, err)
}

// awaitDone is the error the work done reports, failing after 30s without one.
func (r headRig) awaitDone(what string, done <-chan error) error {
	r.t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		r.t.Fatalf("%s did not end within 30s", what)
		return nil
	}
}

// begin opens a transaction on a connection of its own, with the backend's pid.
func (r headRig) begin() (pgx.Tx, int32) {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	r.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var pid int32
	if err := tx.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		r.t.Fatalf("read the backend's pid: %v", err)
	}
	return tx, pid
}

// The head read of the tree route answers each reader, byte for byte, what the walk of the tuples
// answers at the head cut, after each write of each writer class that moves a fact of the issue's
// tree, each through its real route: the issue's create, which marks it in the transaction of its
// first tree node; a settlement and a tick of the block index, which write the blocks and the
// typed nodes (decisions and readings); a document upload and a spec edit; an ask, its answer, an
// answer the release before recorded and the decisions step's catch-up of it (asks.Settle), a
// comment, a comment anchored in the spec, a message, and a person's reply in a thread an agent
// wrote, which makes the agent's comment a conversation node; a lens with its findings, a finding,
// a risk, a constraint and a pointer; the state route, a node stated gone and a node another person
// may not see; a result and its retraction; an untitled document with a lens on it and its
// removal, and a document the release before removed and the block index's orphan and stale-lens
// sweeps over it; a document above the block index's deferral bound, whose upload writes its typed
// nodes and whose tick writes its blocks; and the read-cost corpus, one document of 2,000 blocks.
// Each is read with no view and with each view, as the issue's author and as another person, and
// each answer is held to the same read with the mark taken away. After each the mark still stands,
// a read of the marked issue sent no statement of the walk, and the census kind issue_head_node
// reads no difference.
//
// Four writes of the release before, by plain SQL with no head row and no setting - a tuple out of
// a node of the tree, a new node with its part_of into the tree, a node row updated in place and a
// comment's body rewritten - each take the mark away, the comment's inside the statement that
// writes it, before its transaction commits, so the read walks the tuples and answers the new state;
// the fill then marks the issue again. A fill and a write of the release before do not interleave,
// in either order: a write after the fill waits for the fill's head-fill lock, then takes away the
// mark it committed; a fill after the write waits for the write to commit, then fills and marks the
// issue. Two writes of the release before on one unmarked issue do not wait on each other, since the
// trigger takes the head-fill lock in share mode; on a marked issue the second waits only for the
// first's delete of the mark's row, and both complete (markedWritesWait).
// And no cycle forms where the holder of the issue's owner lock takes the issue's row for no key
// update while a write of the release before holds it for share and appends a part_of into the
// tree.
func TestTheHeadProjectionAnswersAsTheWalk(t *testing.T) {
	requireHeadProjectionStates(t, nil)
}

// requireHeadProjectionStates writes, on a server of its own, each state of the store
// TestTheHeadProjectionAnswersAsTheWalk names, holds every read of each to the walk, and hands each
// state, once it is held, to observe where it is set (TestTheTwoCutsAnswerAsTheUncutWalk).
func requireHeadProjectionStates(t *testing.T, observe func(r headRig, class, key string, logins []string)) {
	t.Helper()
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	// A settlement runs when the test asks for it (settleIssue), so no write of the server's own
	// lands between the two reads of a pair.
	handler, _, deps, served := newRegisteredTestServer(t, testServerOptions{store: traced, settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	ctx := context.Background()

	const key = "HPJ-1"
	issue := createInteractionIssue(t, handler, "HPJ", "The head projection answers as the walk", "An opening paragraph.\n\n"+decisionsViewSpec(key))
	if issue.Key != key {
		t.Fatalf("the issue is %s, want %s, which its spec's pointers name", issue.Key, key)
	}
	r := newHeadRig(t, handler, database, counter, key)
	r.served, r.observe = served, observe
	r.requireMarked(key)
	r.after("issue create", key)

	settleIssue(t, documents, key)
	r.after("settlement", key)
	reconcileBlockIndexPass(t, deps)
	r.after("block index tick", key)

	spec := "dispatch://" + key + "/spec"
	requirement := refs.BlockRef(spec, "r1")
	specDocument := readDecisionDocument(t, database, key, "spec")
	requirementNode := specDocument.address("r1")
	paragraphNode := specDocument.address(firstBlockOf(t, handler, issue.PrimaryArtifactID))
	verification := uploadDocument(t, handler, "/api/v1/issues/"+key, "verification.md",
		checkServing("k1", requirement)+"\n"+checkServing("k2", requirement))
	r.after("document upload", key)
	check := refs.BlockRef("dispatch://"+key+"/artifact/"+verification.Artifact.Slug, "k1")

	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": "A paragraph the edit adds.\n"}},
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("edit the spec: status=%d body=%s", edited.Code, edited.Body.String())
	}
	r.after("spec edit", key)
	settleIssue(t, documents, key)
	reconcileBlockIndexPass(t, deps)
	r.after("block index pass of the edit", key)

	questioned := createIssueAsk(t, handler, key, "Which region holds the replica?")
	r.after("ask", key)
	answerDecisionBlock(t, handler, blockAskID(t, handler, key, "a1"), "bob", []string{"Urgency"}, "Urgency, then age.", nil)
	r.after("ask answer", key)
	// The decisions step's catch-up of an answer the release before recorded - the asks row and its
	// ask.answered, with nothing stated on the ask's node - which the step states on the node under
	// that event (asks.Settle): the one write whose head row Settled.Queue writes itself, where the
	// answer route leaves it to closeAskTx.
	answeredBefore := answerAsTheReleaseBefore(t, database, key, questioned, "Yes")
	r.after("release before's answer", key)
	if err := docs.NewDecisionStep(database, "", nil).Tick(ctx); err != nil {
		t.Fatalf("tick the decisions step: %v", err)
	}
	var caughtUp int64
	if err := database.Pool.QueryRow(ctx, `
		select source_event from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4
		order by seq desc limit 1`, graphstore.KindAsk, questioned, graphstore.TupleAnswerState, graphstore.KindValue).Scan(&caughtUp); err != nil || caughtUp != answeredBefore {
		t.Fatalf("the decisions step's catch-up stated the answer state of ask %s under event %d (%v), want the release before's ask.answered %d",
			questioned, caughtUp, err, answeredBefore)
	}
	r.after("decisions step's catch-up of the answer", key)
	personComment(t, handler, database, key, "A person's comment on the head projection.")
	r.after("comment", key)
	created(t, "a comment anchored in the spec", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body": "On this paragraph.", "anchor": map[string]any{"artifact": "spec", "quote": "An opening paragraph"},
	}, "alice"))
	r.after("anchored comment", key)
	created(t, "a message", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/messages", map[string]any{"body": "Noted.", "actor": sessionActor()}))
	r.after("message", key)
	agentComment := decodeBody[struct {
		ID string `json:"id"`
	}](t, created(t, "an agent's comment", agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body": "Round report: the index builds first.", "actor": map[string]string{"kind": "session", "id": "019a1c0b-3333-7000-8000-000000000003"},
	}, "agent-token")))
	r.after("agent comment", key)
	created(t, "a person's reply in the agent's thread", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body": "Why does it build first?", "reply_to": agentComment.ID,
	}, "alice"))
	r.after("reply in an agent's thread", key)

	reasoning := reasoningRig{t: t, handler: handler, database: database, issue: key}
	lens := reasoning.invoke(spec, "trade-off", "alice", map[string]any{
		"verdict": "Keep one region.", "findings": []string{"A second region doubles the cost.", "Failover takes minutes."},
	})
	r.after("lens", key)
	created(t, "a finding", reasoning.post(nodePath("lens/"+lens.ID, "findings"), map[string]any{"text": "The bill doubles too."}, "alice"))
	r.after("finding", key)
	premortem := reasoning.invoke(requirement, graphstore.LensKindPreMortem, "alice", nil)
	risk := decodeBody[riskWritten](t, created(t, "a risk", reasoning.post("/risks", map[string]any{
		"lens": premortem.Address, "heading": "The migration locks the table.",
		"early_warning": "Writes queue.", "prevention": "Build the index concurrently.", "prevented_by": check,
	}, "alice")))
	r.after("risk", key)
	constraint := decodeBody[constraintRead](t, created(t, "a constraint", reasoning.post("/constraints", map[string]any{"standard": "RFC 9110", "reference": "section 9.2.2"}, "alice")))
	r.after("constraint", key)
	if pointed := reasoning.post(nodePath(requirementNode, "pointers"), map[string]any{
		"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": constraint.Address,
	}, "alice"); pointed.Code != http.StatusOK {
		t.Fatalf("point the requirement at the constraint: status=%d body=%s", pointed.Code, pointed.Body.String())
	}
	r.after("pointer", key)
	reasoning.writeState(lens.Address, graphstore.StateProposed)
	r.after("state route", key)
	doomed := reasoning.invoke(spec, "trade-off", "alice", map[string]any{"verdict": "Drop it."})
	reasoning.writeState(doomed.Address, graphstore.StateGone)
	r.after("state route's gone", key)
	reserved := reasoning.invoke(requirement, graphstore.LensKindPreMortem, "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
	created(t, "a risk under alice's reserved lens", reasoning.post("/risks", map[string]any{
		"lens": reserved.Address, "heading": "Her own worry.", "early_warning": "She sees it.", "prevention": "She acts.",
	}, "alice"))
	r.after("reserved lens", key)

	result := decodeBody[recordedResult](t, created(t, "a result", recordResult(t, handler, key, resultBody("k1", "pass", headCommit, nil)))).Result
	r.after("result", key)
	created(t, "the result's retraction", dispatchRequest(t, handler, http.MethodPost, "/api/v1/results/"+result.ID+"/retract",
		map[string]any{"reason": "The run measured the wrong build."}, "alice"))
	r.after("result retraction", key)

	// A document's removal, which states its node gone and the lens on it with what stands under
	// it (removedDocumentNodes, graphcensus.StateStaleLensesGone in the route's transaction); and
	// the block index's two sweeps over a document the release before removed, stating no node
	// gone: the orphan sweep states the document's node gone (graphcensus.StateOrphanedGone), and
	// the stale-lens sweep the lens on it (graphcensus.StateStaleLensesGone).
	removable := emptyDocument(t, handler, key, "Removed.md")
	r.after("untitled document", key)
	reasoning.invoke("dispatch://"+key+"/artifact/"+removable.Slug, "trade-off", "alice", map[string]any{"findings": []string{"It goes with its document."}})
	r.after("lens on the untitled document", key)
	if removal := removeDocument(t, handler, removable.ID); removal.Code != http.StatusNoContent {
		t.Fatalf("remove %s: status=%d body=%s", removable.ID, removal.Code, removal.Body.String())
	}
	r.after("document removal", key)
	orphaned := emptyDocument(t, handler, key, "Orphaned.md")
	reasoning.invoke("dispatch://"+key+"/artifact/"+orphaned.Slug, "trade-off", "alice", map[string]any{"findings": []string{"It outlives its document."}})
	removeAsTheReleaseBefore(t, database, orphaned)
	r.after("removal of the release before", key)
	if err := graphcensus.StateOrphanedGone(ctx, database, orphaned.RefKey); err != nil {
		t.Fatalf("sweep the orphaned node of %s: %v", orphaned.RefKey, err)
	}
	r.after("orphan sweep", key)
	stateStaleLensesGoneAsTheTick(t, database)
	r.after("stale lens sweep", key)

	// A document above docs' deferBlockIndexBlocks (2,000) blocks: its upload writes its typed nodes
	// alone (indexDeferredVersion) and leaves its other blocks to the block index's tick.
	const deferred = "HPJ-2"
	if made := decodeBody[struct {
		Key string `json:"key"`
	}](t, created(t, "the issue of the deferred document", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "HPJ", "title": "A document too large to index in its request", "spec": "A short spec.\n",
	}, "alice"))); made.Key != deferred {
		t.Fatalf("the second issue is %s, want %s", made.Key, deferred)
	}
	r.requireMarked(deferred)
	var large strings.Builder
	large.WriteString(":::requirement{#r9}\nThe deferred document holds a requirement.\n:::\n\n:::decision{#d9}\nWe index it out of the request.\n:::\n\n")
	for line := range 2_001 {
		fmt.Fprintf(&large, "Line %d of the deferred document.\n\n", line)
	}
	uploadDocument(t, handler, "/api/v1/issues/"+deferred, "deferred.md", large.String())
	r.after("deferred typed nodes", deferred)
	indexOwedDocuments(t, database, deps)
	r.after("block index tick of the deferred document", deferred)

	// The writes of the release before, each on the marked issue by plain SQL. The tuple is a text
	// value of a lens's finding, whose words the tree read answers from that value alone.
	r.releaseBefore("release before's tuple", key, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, releaseBeforeTuple, refs.BlockNodeKind, lens.Findings[0].ID, graphstore.AttributeText, graphstore.KindValue, "A finding the release before rewrote.")
		return err
	})
	r.releaseBefore("release before's node", key, func(ctx context.Context, tx pgx.Tx) error {
		copied := uuid.NewString()
		if _, err := tx.Exec(ctx, `
			insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			select kind, $2, supertag, visibility, state, attributes, made_at, made_by, source_event
			from graph_nodes where kind = $3 and id = $1`, risk.ID, copied, graphstore.KindRisk); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
			select from_kind, $2, kind, to_kind, to_id, at, who, source_event, asserted
			from graph_tuples where from_kind = $3 and from_id = $1 order by seq`, risk.ID, copied, graphstore.KindRisk)
		return err
	})
	r.releaseBefore("release before's node row", key, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update graph_nodes set visibility = $3 where kind = $1 and id = $2`, graphstore.KindLens, lens.ID, graphstore.VisibilityAuthor)
		return err
	})
	// The trigger of comments runs inside the statement that writes the row, as the other three do:
	// the mark is gone before the write's transaction commits.
	r.releaseBefore("release before's comment", key, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `update comments set body = $2 where id = $1`, agentComment.ID, "A comment the release before rewrote."); err != nil {
			return err
		}
		var marked bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from issue_head_marks where issue_key = $1)`, key).Scan(&marked); err != nil {
			return err
		}
		if marked {
			return fmt.Errorf("the mark of %s stands after the comment's update, inside its transaction: the trigger of comments did not take it away as the statement ran", key)
		}
		return nil
	})

	// A fill and a write of the release before: the fill holds the issue's owner lock and its
	// head-fill lock and writes the mark; the write's trigger waits for the head-fill lock, and once
	// the fill commits it takes away the mark the fill wrote.
	unmarkHead(t, database, key)
	fill, _ := r.begin()
	if err := store.LockOwner(ctx, fill, store.IssueOwner(key)); err != nil {
		t.Fatalf("the fill takes the owner lock of %s: %v", key, err)
	}
	if err := store.LockHeadFill(ctx, fill, key); err != nil {
		t.Fatalf("the fill takes the head-fill lock of %s: %v", key, err)
	}
	if _, err := fill.Exec(ctx, `insert into issue_head_marks (issue_key, filled_at) values ($1, clock_timestamp())`, key); err != nil {
		t.Fatalf("the fill marks %s: %v", key, err)
	}
	write, writer := r.begin()
	written := make(chan error, 1)
	go func() {
		_, err := write.Exec(ctx, releaseBeforeTuple, refs.BlockNodeKind, paragraphNode, graphstore.AttributeText, graphstore.KindValue, "The release before rewrote it again.")
		if err == nil {
			err = write.Commit(ctx)
		}
		written <- err
	}()
	ended, err := r.awaitWaiting(backendWaits, writer, written)
	if commitErr := fill.Commit(ctx); commitErr != nil {
		t.Fatalf("the fill commits the mark of %s: %v", key, commitErr)
	}
	if !ended {
		err = r.awaitDone("the write of the release before beside the fill", written)
	}
	if err != nil {
		t.Fatalf("the write of the release before beside the fill of %s: %v", key, err)
	}
	if _, marked := r.mark(key); marked {
		t.Errorf("the mark of %s stands after a write of the release before", key)
	}
	r.refill(key)
	r.requireAsWalk("refill after the fill and the write of the release before", key)

	// The other order: a write of the release before first, then the fill of the issue, which waits
	// at the head-fill lock until the write commits, then fills the issue from what it wrote and
	// marks it.
	unmarkHead(t, database, key)
	early, _ := r.begin()
	if _, err := early.Exec(ctx, releaseBeforeTuple, refs.BlockNodeKind, paragraphNode, graphstore.AttributeText, graphstore.KindValue, "The release before rewrote it before the fill."); err != nil {
		t.Fatalf("the write of the release before ahead of the fill of %s: %v", key, err)
	}
	filling := make(chan error, 1)
	go func() {
		_, _, err := graphcensus.FillIssueHeads(ctx, database, graphcensus.HeadFillBatch)
		filling <- err
	}()
	fillEnded, fillErr := r.awaitWaiting(issueLockWaited, []int32{store.OwnerLockNamespace, store.HeadFillLockNamespace}, filling)
	if fillEnded {
		t.Errorf("the fill of %s did not wait for a write of the release before that had not committed (%v)", key, fillErr)
	}
	if err := early.Commit(ctx); err != nil {
		t.Fatalf("commit the write of the release before ahead of the fill of %s: %v", key, err)
	}
	if !fillEnded {
		fillErr = r.awaitDone("the fill after the write of the release before", filling)
	}
	if fillErr != nil {
		t.Fatalf("the fill of %s after a write of the release before: %v", key, fillErr)
	}
	if _, marked := r.mark(key); !marked {
		t.Errorf("%s holds no mark after the fill that waited for a write of the release before", key)
	}
	r.refill(key)
	r.requireAsWalk("fill after a write of the release before", key)

	// Two writes of the release before on one issue, each on its own connection under a lock
	// timeout of 2s, the second made while the first holds its write uncommitted. Each trigger takes
	// the issue's head-fill lock in share mode, so on an unmarked issue neither waits for the other.
	// On a marked issue the first deletes the mark's row, so the second waits for that row until the
	// first commits (markedWritesWait); both complete and the mark is gone.
	unmarkHead(t, database, key)
	r.twoReleaseBeforeWrites("unmarked", key, paragraphNode, requirementNode, false)
	r.refill(key)
	r.twoReleaseBeforeWrites("marked", key, paragraphNode, requirementNode, markedWritesWait)
	r.refill(key)
	r.requireAsWalk("refill after two writes of the release before", key)

	// No cycle: the holder of the issue's owner lock takes its row for no key update while a write
	// of the release before holds that row for share and appends a part_of into the tree.
	r.requireMarked(key)
	holder, holderPid := r.begin()
	if err := store.LockOwner(ctx, holder, store.IssueOwner(key)); err != nil {
		t.Fatalf("take the owner lock of %s: %v", key, err)
	}
	sharer, _ := r.begin()
	if _, err := sharer.Exec(ctx, `select key from issues where key = $1 for share`, key); err != nil {
		t.Fatalf("hold the row of %s for share: %v", key, err)
	}
	updated := make(chan error, 1)
	go func() {
		_, err := holder.Exec(ctx, `select key from issues where key = $1 for no key update`, key)
		if err == nil {
			err = holder.Commit(ctx)
		}
		updated <- err
	}()
	if ended, err := r.awaitWaiting(backendWaits, holderPid, updated); ended {
		t.Fatalf("the owner lock's holder took the row of %s for no key update past a share of it: %v", key, err)
	}
	_, shareErr := sharer.Exec(ctx, releaseBeforeTuple, graphstore.KindRisk, risk.ID, graphstore.TuplePartOf, graphstore.KindLens, premortem.ID)
	if shareErr == nil {
		shareErr = sharer.Commit(ctx)
	}
	holderErr := r.awaitDone("the owner lock holder's update", updated)
	for _, err := range []error{shareErr, holderErr} {
		if err != nil {
			t.Errorf("a write of the release before holding the row of %s for share, beside the owner lock's holder: %v", key, err)
		}
	}
	r.refill(key)
	r.requireAsWalk("refill after the write beside the owner lock's holder", key)

	// The read-cost corpus: one document of 2,000 blocks 6 list levels deep.
	corpus := seedTreeReadCorpus(t, inProcessReadCostServer(handler), database, deps)
	r.requireMarked(corpus.treeIssue)
	r.after("read-cost corpus", corpus.treeIssue, readCostWriter, readCostReader)
}

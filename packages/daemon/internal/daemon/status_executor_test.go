package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// capturedAnswer is the dispatch client's reading of name, one of Dispatch's answers saved under
// internal/dispatch/testdata (its README says how each was captured): the answer served once from
// an httptest server to the real client's ChangeStatus, so the outbox meets exactly what the
// client makes of the route's real answer.
func capturedAnswer(t *testing.T, name string, change dispatch.StatusChange) (dispatch.StatusChangeResult, error) {
	t.Helper()
	path := filepath.Join("..", "dispatch", "testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	saved, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(data)), nil)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	body, err := io.ReadAll(saved.Body)
	saved.Body.Close()
	if err != nil {
		t.Fatalf("read the body of %s: %v", path, err)
	}
	header := saved.Header
	header.Del("Content-Length")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for key, values := range header {
			w.Header()[key] = values
		}
		w.WriteHeader(saved.StatusCode)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	return dispatch.New(server.URL, "captured-answer-token").ChangeStatus(context.Background(), "LEGION-1", change)
}

// keyAnswer is one answer a keyRuleDispatch gives a fresh request key: a captured answer of
// Dispatch's (file), or an error no capture holds (err), such as a 5xx from in front of it.
type keyAnswer struct {
	file string
	err  error
}

// keyRuleDispatch is the outbox's Dispatch with the status-change route kept to Dispatch's
// request-key rule (packages/envoy/internal/dispatch/api/request_keys.go): a key holds the first
// body it was sent with, and another body under it is answered 422 KEY_REUSED, which the test
// fails on, since no attempt of the daemon's may send one; a stored answer is answered again for
// its key; a 5xx, a 401, a 408 and a 429 release the key; and a run still in flight, or a router
// with no such route, stores nothing. A key it has not seen takes the next of script. Each read of
// the issue answers it one event further on, so a new attempt's body differs from the last.
type keyRuleDispatch struct {
	outboxDispatch
	t      *testing.T
	mu     sync.Mutex
	script []keyAnswer
	bodies map[string]dispatch.StatusChange
	stored map[string]keyAnswer
	sent   []dispatch.StatusChange
	reads  int
	// changedTo is the status each read shows once a status change has been answered, "" for
	// the status the issue was read at first.
	changedTo string
}

func newKeyRuleDispatch(t *testing.T, issue dispatch.Issue, script ...keyAnswer) *keyRuleDispatch {
	return &keyRuleDispatch{outboxDispatch: outboxDispatch{issue: issue}, t: t, script: script,
		bodies: map[string]dispatch.StatusChange{}, stored: map[string]keyAnswer{}}
}

func (d *keyRuleDispatch) GetIssue(context.Context, string) (dispatch.Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	issue := d.issue
	issue.LastSeq += int64(d.reads)
	if d.changedTo != "" && len(d.sent) > 0 {
		issue.Status = d.changedTo
	}
	return issue, nil
}

func (d *keyRuleDispatch) ChangeStatus(_ context.Context, _ string, change dispatch.StatusChange) (dispatch.StatusChangeResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, change)
	body := change
	body.IdempotencyKey = ""
	if first, used := d.bodies[change.IdempotencyKey]; used && first != body {
		d.t.Errorf("the outbox sent %+v under %s, a key first sent with %+v", body, change.IdempotencyKey, first)
		return capturedAnswer(d.t, "key_reused.http", change)
	}
	if stored, ok := d.stored[change.IdempotencyKey]; ok {
		return d.answer(stored, change)
	}
	if len(d.script) == 0 {
		d.t.Fatalf("the outbox sent %+v under %s, and the test gave no answer for it", body, change.IdempotencyKey)
	}
	next := d.script[0]
	d.script = d.script[1:]
	d.bodies[change.IdempotencyKey] = body
	result, err := d.answer(next, change)
	var refusal *dispatch.Error
	switch {
	case err == nil:
		d.stored[change.IdempotencyKey] = next
	case !errors.As(err, &refusal):
	case refusal.Status >= 500, refusal.Status == http.StatusUnauthorized, refusal.Status == http.StatusRequestTimeout, refusal.Status == http.StatusTooManyRequests:
		delete(d.bodies, change.IdempotencyKey)
	case refusal.Code == "KEY_IN_FLIGHT", dispatch.NoRoute(err):
	default:
		d.stored[change.IdempotencyKey] = next
	}
	return result, err
}

func (d *keyRuleDispatch) answer(answer keyAnswer, change dispatch.StatusChange) (dispatch.StatusChangeResult, error) {
	if answer.err != nil {
		return dispatch.StatusChangeResult{}, answer.err
	}
	return capturedAnswer(d.t, answer.file, change)
}

// keys is the request key of each status change the outbox sent, in order.
func (d *keyRuleDispatch) keys() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	keys := make([]string, len(d.sent))
	for i, change := range d.sent {
		keys[i] = change.IdempotencyKey
	}
	return keys
}

// optionRow is one test's option row: the outbox that runs it, over the key-rule Dispatch, the
// row's id and the log it writes.
type optionRow struct {
	t      *testing.T
	pool   *store.Pool
	runner *outbox
	client *keyRuleDispatch
	issue  string
	id     int64
	logs   *syncBuffer
}

// newOptionRow queues issue's move from in_progress to backlog with include_children, the row the
// architect's park of an issue with a child at work queues, over a key-rule Dispatch giving
// script, on a database of the test's own. handlers apply the facts the row makes.
func newOptionRow(t *testing.T, issue string, handlers []intake.Handler, script ...keyAnswer) *optionRow {
	t.Helper()
	pool := isolatedOutboxPool(t)
	return queueOptionRow(t, pool, issue, handlers, record.StatusWrite{}, script...)
}

func queueOptionRow(t *testing.T, pool *store.Pool, issue string, handlers []intake.Handler, extra record.StatusWrite, script ...keyAnswer) *optionRow {
	t.Helper()
	records := record.NewStore()
	write := record.OrdinaryStatusWrite("backlog", "in_progress", "")
	write.IncludeChildren, write.Controller = true, extra.Controller
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, write, time.Now()))
	var id int64
	if err := pool.QueryRow(context.Background(), "select id from outbox where issue = $1 and kind = 'dispatch_status'", issue).Scan(&id); err != nil {
		t.Fatalf("read the id of the option row of %s: %v", issue, err)
	}
	client := newKeyRuleDispatch(t, dispatch.Issue{Key: issue, Status: "in_progress", LastSeq: 10}, script...)
	logs := &syncBuffer{}
	runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, dispatch: client, handlers: handlers,
		now: time.Now, log: slog.New(slog.NewJSONHandler(logs, nil))}
	return &optionRow{t: t, pool: pool, runner: runner, client: client, issue: issue, id: id, logs: logs}
}

// run drains the outbox once, the row due or not.
func (o *optionRow) run() {
	o.t.Helper()
	if _, err := o.pool.Exec(context.Background(), "update outbox set next_at = now() where id = $1", o.id); err != nil {
		o.t.Fatalf("make the option row due: %v", err)
	}
	if err := o.runner.RunOnce(context.Background()); err != nil {
		o.t.Fatalf("run the option row: %v", err)
	}
}

// key is the request key of the row's attempt number n.
func (o *optionRow) key(n int) string { return fmt.Sprintf("outbox-%s-%d-%d", o.issue, o.id, n) }

// queued says whether the outbox still holds the row.
func (o *optionRow) queued() bool {
	o.t.Helper()
	return statusRowsOf(o.t, o.pool, o.issue) == 1
}

// wantKeys holds the request keys the row sent to want, by attempt number, and the reads of the
// issue it made to reads.
func (o *optionRow) wantKeys(reads int, attempts ...int) {
	o.t.Helper()
	want := make([]string, len(attempts))
	for i, n := range attempts {
		want[i] = o.key(n)
	}
	if got := o.client.keys(); strings.Join(got, " ") != strings.Join(want, " ") {
		o.t.Fatalf("request keys sent = %v, want %v", got, want)
	}
	if o.client.reads != reads {
		o.t.Fatalf("reads of %s = %d, want %d", o.issue, o.client.reads, reads)
	}
}

// A write with an option of Dispatch's status-change route goes on that route under a request key
// of its own attempt, and follows Dispatch's request-key rule (decision 6 of
// dispatch://INFRA-4730): the same key and body go again, with no read first, only after an
// answer the key layer stored nothing for or a run still in flight; every stored answer ends the
// attempt, and the row reads Dispatch and starts a new one under a new key, or ends. A Dispatch
// older than the route answers it with its router's 404, a wait the row is sent again after. The
// key-rule fake fails the test should any attempt send a changed body under a key it used. Each
// answer the fake gives is one captured from a real Dispatch (capturedAnswer).
func TestAnOptionWriteWaitsForTheStatusChangeRoute(t *testing.T) {
	badGateway := &dispatch.Error{Status: http.StatusBadGateway, Message: "Bad Gateway"}
	t.Run("the router's 404 waits, and the attempt goes again once the route answers", func(t *testing.T) {
		row := newOptionRow(t, "LEGION-41", nil, keyAnswer{file: "status_change_no_route.http"}, keyAnswer{file: "status_change_include_children.http"})
		row.run()
		if !row.queued() {
			t.Fatal("the row finished on the router's 404, want it waiting for the route")
		}
		if lines := loggedLines(t, row.logs.String(), "outbox row waits"); len(lines) != 1 {
			t.Fatalf("wait lines = %d, want the wait logged once at its first run", len(lines))
		}
		row.run()
		row.wantKeys(1, 1, 1)
		if row.queued() {
			t.Fatal("the row is still queued after the route took it")
		}
		if got := row.client.sent; got[0] != got[1] || !got[0].IncludeChildren || got[0].ExpectedSequence != 11 {
			t.Fatalf("status changes sent = %+v, want the same body twice, with include_children at the read's sequence", got)
		}
	})
	t.Run("ISSUE_CHANGED on the whole call, then a new attempt under a new key", func(t *testing.T) {
		row := newOptionRow(t, "LEGION-41", nil, keyAnswer{file: "status_change_stale_sequence.http"}, keyAnswer{file: "status_change_include_children.http"})
		row.run()
		row.wantKeys(2, 1, 2)
		if sent := row.client.sent; sent[0].ExpectedSequence != 11 || sent[1].ExpectedSequence != 12 {
			t.Fatalf("status changes sent = %+v, want each attempt at the sequence of its own read", sent)
		}
		if row.queued() {
			t.Fatal("the row is still queued after its new attempt was taken")
		}
	})
	t.Run("a refusal midway applies each committed step by its record, and a new attempt finishes the rest", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		// The capture's call over LEGION-2 committed three children's steps, each from sequence 2
		// to 4. One record stands at the step's previous sequence, and takes it; one stands below
		// it, so events of its before the step have yet to come, and it is left to them; and the
		// stream delivered one's event before the answer, so it changes nothing.
		seedRecords(t, pool, map[string]int64{"LEGION-3": 2, "LEGION-4": 1, "LEGION-5": 4})
		row := queueOptionRow(t, pool, "LEGION-2", nil, record.StatusWrite{},
			keyAnswer{file: "status_change_midway_refused.http"}, keyAnswer{file: "status_change_include_children.http"})
		// The named issue's own step committed nothing here, but a call whose named step did
		// leaves the issue at the row's status: the new attempt is made all the same.
		row.client.changedTo = "backlog"
		row.run()
		row.wantKeys(2, 1, 2)
		for key, want := range map[string]bool{"LEGION-3": true, "LEGION-4": false, "LEGION-5": false} {
			if got := factApplied(t, pool, fmt.Sprintf("status-change:%s:%s:4", row.key(1), key)); got != want {
				t.Errorf("the committed step of %s applied = %t, want %t", key, got, want)
			}
		}
		if row.queued() {
			t.Fatal("the row is still queued after its new attempt was taken")
		}
	})
	t.Run("a 5xx after committed steps, then the keyed repeat of the same key and body finishes the rest", func(t *testing.T) {
		row := newOptionRow(t, "LEGION-203", nil, keyAnswer{err: badGateway}, keyAnswer{file: "status_change_keyed_repeat.http"})
		row.run()
		if !row.queued() {
			t.Fatal("the row finished on a 5xx, want it kept for its keyed repeat")
		}
		row.run()
		row.wantKeys(1, 1, 1)
		if row.queued() {
			t.Fatal("the row is still queued after the keyed repeat finished the call")
		}
	})
	t.Run("KEY_IN_FLIGHT, then the same key and body after its Retry-After", func(t *testing.T) {
		row := newOptionRow(t, "LEGION-47", nil, keyAnswer{file: "status_change_key_in_flight.http"}, keyAnswer{file: "status_change_include_children.http"})
		before := time.Now()
		if err := row.runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("run the row: %v", err)
		}
		var next time.Time
		if err := row.pool.QueryRow(context.Background(), "select next_at from outbox where id = $1", row.id).Scan(&next); err != nil {
			t.Fatalf("read the row's next run: %v", err)
		}
		if next.Before(before.Add(time.Second)) {
			t.Fatalf("the row runs again at %s, want no sooner than the answer's Retry-After of one second after %s", next, before)
		}
		row.run()
		row.wantKeys(1, 1, 1)
		if row.queued() {
			t.Fatal("the row is still queued after the first run of its key ended and the repeat was taken")
		}
	})
	t.Run("a 401, a 408 and a 429, each then the same key and body", func(t *testing.T) {
		row := newOptionRow(t, "LEGION-41", nil,
			keyAnswer{err: &dispatch.Error{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED", Message: "token expired"}},
			keyAnswer{err: &dispatch.Error{Status: http.StatusRequestTimeout, Code: "REQUEST_TIMEOUT", Message: "come back later"}},
			keyAnswer{err: &dispatch.Error{Status: http.StatusTooManyRequests, Code: "RATE_LIMITED", Message: "slow down"}},
			keyAnswer{file: "status_change_include_children.http"})
		for range 4 {
			row.run()
		}
		row.wantKeys(1, 1, 1, 1, 1)
		if row.queued() {
			t.Fatal("the row is still queued after the fourth send was taken")
		}
	})
	t.Run("KEY_COMMITTED reads as landed: a read and a new attempt", func(t *testing.T) {
		row := newOptionRow(t, "LEGION-41", nil, keyAnswer{file: "key_committed.http"}, keyAnswer{file: "status_change_include_children.http"})
		row.run()
		row.wantKeys(2, 1, 2)
		if row.queued() {
			t.Fatal("the row is still queued after its new attempt was taken")
		}
	})
	t.Run("KEY_REUSED is logged, then a read and a new attempt", func(t *testing.T) {
		row := newOptionRow(t, "LEGION-41", nil, keyAnswer{file: "key_reused.http"}, keyAnswer{file: "status_change_include_children.http"})
		row.run()
		row.wantKeys(2, 1, 2)
		lines := loggedLines(t, row.logs.String(), "outbox status change sent under a key another body used; the row reads Dispatch and starts a new attempt")
		if len(lines) != 1 || lines[0]["key"] != row.key(1) {
			t.Fatalf("KEY_REUSED lines = %v, want one naming %s", lines, row.key(1))
		}
	})
	t.Run("a kept 403 sends its next try as a new attempt", func(t *testing.T) {
		row := newOptionRow(t, "LEGION-41", nil, keyAnswer{err: &dispatch.Error{Status: http.StatusForbidden, Code: "FORBIDDEN", Message: "not allowed"}},
			keyAnswer{file: "status_change_include_children.http"})
		row.run()
		if !row.queued() {
			t.Fatal("the row finished on a 403, want it kept")
		}
		row.run()
		row.wantKeys(2, 1, 2)
		if row.queued() {
			t.Fatal("the row is still queued after its new attempt was taken")
		}
	})
	for _, controller := range []bool{false, true} {
		t.Run(fmt.Sprintf("a PARENT_STATUS_CONFLICT on a write that is no close is undone, controller %t", controller), func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			// The record counted the write before its echo: it reads backlog while Dispatch
			// shows in_progress.
			seedRecord(t, pool, record.Issue{Key: "LEGION-33", Status: "backlog", DispatchStatus: "in_progress", SeenSeq: 100})
			engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
			row := queueOptionRow(t, pool, "LEGION-33", []intake.Handler{engine}, record.StatusWrite{Controller: controller},
				keyAnswer{file: "status_change_no_option.http"})
			row.run()
			row.wantKeys(1, 1)
			if row.queued() {
				t.Fatal("the row is still queued after Dispatch refused it for its parent rule")
			}
			if got := recordOf(t, pool, records, "LEGION-33"); got.Status != "in_progress" {
				t.Fatalf("the record's status after the refusal = %s, want the in_progress Dispatch shows", got.Status)
			}
			architect, toController := noticesOfKind(t, pool, "LEGION-33", "message-refused"), controllerNoticesOfKind(t, pool, "LEGION-33", "message-refused")
			told := architect
			if controller {
				told = toController
				if len(architect) != 0 {
					t.Fatalf("architect notices %v, want none: the controller asked for the write", architect)
				}
			} else if len(toController) != 0 {
				t.Fatalf("controller notices %v, want none: the architect's write", toController)
			}
			if len(told) != 1 || !strings.Contains(told[0].Reason, "PARENT_STATUS_CONFLICT") || !strings.Contains(told[0].Reason, "LEGION-34 (in_progress)") {
				t.Fatalf("notices = %+v, want one naming the code and LEGION-34 (in_progress)", told)
			}
		})
	}
	// The same rule against the rig's real scratch Dispatch, its parent status rule on: a call a
	// person's write refuses midway is finished by a new attempt, and a call whose Dispatch
	// process dies mid-call is finished by the keyed repeat of the same key and body once the dead
	// run's lease on the key lapses. No writer of this release queues an option row yet (the
	// architect's and the controller's calls do, once the API queues them), so the row is queued as
	// such a writer queues one (record.NewFencedRow).
	r := newStopReturnRig(t)
	t.Run("on the rig, a refusal midway is finished by a new attempt", func(t *testing.T) {
		parent, children := r.parentWithChildren("Albatross", 40)
		row := r.queueOptionRow(parent.Key)
		seq := r.read.reached(t, "the option row's read")
		r.read.release()
		r.awaitParentPast(parent.Key, seq)
		retitled := r.patch(parent.Key, asPerson, map[string]any{"title": "Albatross retitled by a person mid-call"})
		r.optionRowFinished(row)
		r.allAt("backlog", append(children, parent.Key)...)
		var stepBefore, stepAfter bool
		for _, event := range r.issueEvents(parent.Key) {
			switch {
			case event.Type == "child.status" && event.Seq < retitled.LastSeq:
				stepBefore = true
			case event.Seq > retitled.LastSeq:
				stepAfter = true
			}
		}
		if !stepBefore || !stepAfter {
			t.Fatalf("the person's write at %d landed outside the call (a step before it %t, a write after it %t): nothing was refused midway", retitled.LastSeq, stepBefore, stepAfter)
		}
		shown, err := dispatch.New(r.baseURL, r.dispatch.token).GetIssue(t.Context(), parent.Key)
		if err != nil || !strings.Contains(shown.Title, "retitled by a person") {
			t.Fatalf("the parent's title = %q (%v), want the person's write kept", shown.Title, err)
		}
	})
	t.Run("on the rig, a run its Dispatch process died in is finished by the keyed repeat", func(t *testing.T) {
		parent, children := r.parentWithChildren("Petrel", 40)
		row := r.queueOptionRow(parent.Key)
		seq := r.read.reached(t, "the option row's read")
		r.read.release()
		r.awaitParentPast(parent.Key, seq)
		r.dispatch.kill()
		r.dispatch.start()
		r.optionRowFinished(row)
		r.allAt("backlog", append(children, parent.Key)...)
	})
}

// parentWithChildren is a parent at work, not Legion's, with count children at work under it, as
// a person filed and moved them: the parent and each child's key.
func (r *stopReturnRig) parentWithChildren(name string, count int) (scratchIssue, []string) {
	r.t.Helper()
	parent := r.create(asPerson, name+" umbrella", nil)
	r.setStatus(parent.Key, "in_progress")
	children := make([]string, count)
	for i := range children {
		// Forced past Dispatch's near-copy gate, which refuses titles alike.
		child := r.create(asPerson, fmt.Sprintf("%s leaf %d", name, i), map[string]any{"parent": parent.Key, "force": true})
		r.setStatus(child.Key, "in_progress")
		children[i] = child.Key
	}
	return r.issue(parent.Key), children
}

// queueOptionRow queues key's move to the backlog with include_children, at key's fence, with the
// rig's read seam armed on it, and answers the row's id.
func (r *stopReturnRig) queueOptionRow(key string) int64 {
	r.t.Helper()
	write := record.OrdinaryStatusWrite("backlog", "in_progress", "")
	write.IncludeChildren = true
	var id int64
	r.read.arm(func(row record.OutboxRow) bool {
		return row.Issue == key && row.Kind == record.OutboxKindDispatchStatus
	})
	if err := pgx.BeginFunc(r.t.Context(), r.pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		row, err := record.NewFencedRow(r.t.Context(), tx, records, key, write, time.Now())
		if err != nil {
			return err
		}
		if err := records.Enqueue(r.t.Context(), tx, row); err != nil {
			return err
		}
		return tx.QueryRow(r.t.Context(), "select max(id) from outbox where issue = $1 and kind = 'dispatch_status'", key).Scan(&id)
	}); err != nil {
		r.t.Fatalf("queue the option row of %s: %v", key, err)
	}
	// No commit hook wakes the issue's lane for a row queued outside a fact: the outbox's sweep
	// (outboxLease) finds it due, and the read seam then holds it.
	return id
}

// awaitParentPast waits until a step of the call over parent has committed: each child's step adds
// a child.status event to the parent, which moves its sequence past seq, the call's fence.
func (r *stopReturnRig) awaitParentPast(parent string, seq int64) {
	r.t.Helper()
	testwait.WithinPolling(r.t, time.Minute, 5*time.Millisecond, "a step of the call over "+parent+" to commit", func() bool {
		return r.issue(parent).LastSeq > seq
	})
}

// optionRowFinished waits, for up to three minutes (a dead run's lease on its key lasts one), until
// the outbox no longer holds row.
func (r *stopReturnRig) optionRowFinished(row int64) {
	r.t.Helper()
	testwait.WithinPolling(r.t, 3*time.Minute, 100*time.Millisecond, fmt.Sprintf("the option row %d to finish", row), func() bool {
		var queued bool
		return r.pool.QueryRow(r.t.Context(), "select exists (select 1 from outbox where id = $1)", row).Scan(&queued) == nil && !queued
	})
}

// allAt holds each of keys to Dispatch showing status.
func (r *stopReturnRig) allAt(status string, keys ...string) {
	r.t.Helper()
	for _, key := range keys {
		if shown := r.issue(key).Status; shown != status {
			r.t.Fatalf("Dispatch shows %s %s, want %s", key, shown, status)
		}
	}
}

// A status write Dispatch refuses with 403 LEGION_STATUS_SCOPE_REQUIRED is kept and written again,
// as a write refused for its credential is, so nothing is lost once the daemon writes with a token
// that holds the scope (R26 of dispatch://INFRA-3320; decision 12 of dispatch://INFRA-4730). The
// wait is not silent: each refusal logs a line naming the code and the issue, and the first
// refusal in a daemon run queues one notice to the project's controller alone, naming the code and
// the issue and saying that a person must give the daemon a token that holds the scope. A 401 and
// a 403 of another code keep and retry the row as they always did, and tell nobody.
//
// Negative control, run when this was written: with the refusal read as every 403 is read
// (statusRefusal's scope arm taken out, so dispatch.PermanentRefusal alone decides) and no notice
// queued, the "scope refused twice" case fails on its notice count.
func TestAStatusScopeRefusalIsRetriedAndTellsTheController(t *testing.T) {
	scope := &dispatch.Error{Status: http.StatusForbidden, Code: statusScopeCode, Message: "this token lacks the legion-status scope"}
	for _, tc := range []struct {
		name     string
		refusals []error
		notices  int
	}{
		{name: "scope refused twice", refusals: []error{scope, scope}, notices: 1},
		{name: "a 401 and a 403 of another code", refusals: []error{
			&dispatch.Error{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED", Message: "token expired"},
			&dispatch.Error{Status: http.StatusForbidden, Code: "FORBIDDEN", Message: "not allowed"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			const issue = "LEGION-208"
			enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, record.OrdinaryStatusWrite("testing", "in_progress", ""), time.Now()))
			client := &refusingStatus{outboxDispatch: outboxDispatch{issue: dispatch.Issue{Key: issue, Status: "in_progress", LastSeq: 7}}, refusals: tc.refusals}
			logs := &syncBuffer{}
			runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, dispatch: client, now: time.Now,
				log: slog.New(slog.NewJSONHandler(logs, nil))}
			for range len(tc.refusals) + 1 {
				if _, err := pool.Exec(context.Background(), "update outbox set next_at = now() where kind = 'dispatch_status'"); err != nil {
					t.Fatalf("make the row due: %v", err)
				}
				if err := runner.RunOnce(context.Background()); err != nil {
					t.Fatalf("run the row: %v", err)
				}
			}
			if got := client.statuses; len(got) != len(tc.refusals)+1 {
				t.Fatalf("status writes = %v, want the write made again after each refusal", got)
			}
			if rows := statusRowsOf(t, pool, issue); rows != 0 {
				t.Fatalf("status rows after the write was taken = %d, want none", rows)
			}
			lines := loggedLines(t, logs.String(), "outbox status write refused by Dispatch: the daemon's token lacks the scope Dispatch asks of a Legion issue's status; the write is kept and written again")
			if len(lines) != tc.notices*len(tc.refusals) {
				t.Fatalf("scope refusal lines = %d, want one for each scope refusal", len(lines))
			}
			for _, line := range lines {
				if line["code"] != statusScopeCode || line["issue"] != issue {
					t.Fatalf("scope refusal line = %v, want it to name %s and %s", line, statusScopeCode, issue)
				}
			}
			told := controllerNoticesOfKind(t, pool, issue, record.StatusScopeNotice)
			if len(told) != tc.notices {
				t.Fatalf("status-scope notices = %+v, want %d", told, tc.notices)
			}
			for _, notice := range told {
				if !strings.Contains(notice.Reason, statusScopeCode) || !strings.Contains(notice.Reason, issue) ||
					!strings.Contains(notice.Reason, "a person gives the daemon a token that holds the scope") {
					t.Fatalf("status-scope notice reason = %q, want the code, the issue and the token a person must give", notice.Reason)
				}
			}
			if architect := noticesOf(t, pool, issue); architect != 0 {
				t.Fatalf("architect notices = %d, want none: no architect can change a token", architect)
			}
		})
	}
}

// refusingStatus is outboxDispatch whose status write answers each of refusals in turn, and then
// takes the write.
type refusingStatus struct {
	outboxDispatch
	refusals []error
}

func (d *refusingStatus) SetStatusAt(ctx context.Context, issue dispatch.Issue, status string) (dispatch.Issue, error) {
	if _, err := d.outboxDispatch.SetStatusAt(ctx, issue, status); err != nil {
		return dispatch.Issue{}, err
	}
	if len(d.refusals) == 0 {
		return issue, nil
	}
	refusal := d.refusals[0]
	d.refusals = d.refusals[1:]
	return dispatch.Issue{}, refusal
}

// seedRecords records each issue of seqs under LEGION-2, at work in planning, its record standing
// at the Dispatch sequence seqs gives it.
func seedRecords(t *testing.T, pool *store.Pool, seqs map[string]int64) {
	t.Helper()
	for key, seq := range seqs {
		seedRecord(t, pool, record.Issue{Key: key, Status: "in_progress", DispatchStatus: "in_progress", LastDispatchSeq: seq, SeenSeq: seq})
	}
}

// seedRecord records issue, at work in planning in the first run, with the fields it names.
func seedRecord(t *testing.T, pool *store.Pool, issue record.Issue) {
	t.Helper()
	issue.Repo, issue.DefaultBranch, issue.GitHubRepo = "acme/widgets", "main", "acme/widgets"
	issue.Tree, issue.Project, issue.Title, issue.Phase, issue.Generation, issue.HandedOver, issue.Rank = issue.Key, "LEGION", issue.Key, phase.Planning, 1, true, "U"
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, issue)
	}); err != nil {
		t.Fatalf("record %s: %v", issue.Key, err)
	}
}

// factApplied says whether the outbox applied the fact it records under id.
func factApplied(t *testing.T, pool *store.Pool, id string) bool {
	t.Helper()
	var applied bool
	if err := pool.QueryRow(context.Background(), "select exists (select 1 from processed_events where source = 'outbox' and event_id = $1)", id).Scan(&applied); err != nil {
		t.Fatalf("read whether %s was applied: %v", id, err)
	}
	return applied
}

// controllerNoticesOfKind is every controller notice of kind the outbox holds keyed by issue,
// oldest first.
func controllerNoticesOfKind(t *testing.T, pool *store.Pool, issue string, kind record.NoticeKind) []record.Notice {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from outbox where kind = 'controller_notice' and issue = $1
		and payload->>'kind' = $2 order by id`, issue, string(kind))
	if err != nil {
		t.Fatalf("read the %s controller notices of %s: %v", kind, issue, err)
	}
	defer rows.Close()
	var notices []record.Notice
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan a %s controller notice of %s: %v", kind, issue, err)
		}
		var notice record.Notice
		if err := json.Unmarshal(payload, &notice); err != nil {
			t.Fatalf("decode a %s controller notice of %s: %v", kind, issue, err)
		}
		notices = append(notices, notice)
	}
	return notices
}

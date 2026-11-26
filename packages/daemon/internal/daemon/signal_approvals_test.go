package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// approvalsBoard is a Dispatch that answers the four reads of the boot's read for the approvals
// signal, in the shapes of Dispatch's own answers (the project's issue listing and an issue's
// detail with their `last_seq` and `assignee`, GET /api/v1/asks/open?project= with its owner and
// `default_assignee`, and GET /api/v1/issues/<key>/asks?state=open with `asked_of` and
// `waiting_on`). A test changes what it holds between boots, as Dispatch changes while a daemon is
// down, and may hook the read of one issue's asks, where an event lands during the boot's read.
type approvalsBoard struct {
	mu     sync.Mutex
	issues map[string]approvalsIssue
	asks   map[string][]approvalsAsk
	// documentAsk is an open ask of a project document, which the project's open asks list beside
	// the asks of issues and the daemon drops.
	documentAsk bool
	// defaultAssignee is what the project's open asks answer: a login, nil for JSON null.
	// noDefault leaves the member out, as a Dispatch before it does.
	defaultAssignee any
	noDefault       bool
	// refuse is a path fragment whose reads Dispatch answers 503.
	refuse string
	// onIssueAsks runs as Dispatch reads an issue's open asks, once the listing and the project's
	// open asks have been read.
	onIssueAsks func(issue string)
}

type approvalsIssue struct {
	seq      int64
	assignee any // a login, nil for none
}

type approvalsAsk struct {
	id        string
	askedOf   any // a login, nil for none
	waitingOn string
}

func (b *approvalsBoard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	refused := b.refuse != "" && strings.Contains(r.URL.Path, b.refuse)
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if refused {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"UNAVAILABLE","error":"Dispatch is down"}`))
		return
	}
	var body any
	path := r.URL.Path
	switch {
	case path == "/api/v1/issues":
		b.mu.Lock()
		var listed []map[string]any
		for _, key := range slices.Sorted(maps.Keys(b.issues)) {
			listed = append(listed, b.issueBody(key))
		}
		b.mu.Unlock()
		body = listed
	case path == "/api/v1/asks/open":
		body = b.openAsks()
	case strings.HasPrefix(path, "/api/v1/issues/") && strings.HasSuffix(path, "/asks"):
		key := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/issues/"), "/asks")
		if hook := b.onIssueAsks; hook != nil {
			hook(key)
		}
		b.mu.Lock()
		rows := []map[string]any{}
		for _, ask := range b.asks[key] {
			rows = append(rows, map[string]any{"id": ask.id, "issue_key": key, "state": "open", "asked_of": ask.askedOf, "waiting_on": ask.waitingOn})
		}
		b.mu.Unlock()
		body = rows
	case strings.HasPrefix(path, "/api/v1/issues/"):
		key := strings.TrimPrefix(path, "/api/v1/issues/")
		b.mu.Lock()
		_, held := b.issues[key]
		b.mu.Unlock()
		if !held {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"ISSUE_NOT_FOUND","error":"issue not found"}`))
			return
		}
		b.mu.Lock()
		body = b.issueBody(key)
		b.mu.Unlock()
	default:
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// issueBody is an issue as Dispatch's listing and detail answer it; b.mu is held.
func (b *approvalsBoard) issueBody(key string) map[string]any {
	issue := b.issues[key]
	return map[string]any{"key": key, "title": key, "status": "triage", "rank": "a", "labels": []string{}, "last_seq": issue.seq,
		"blocked_by": []string{}, "claim": nil, "resolved_repo": nil, "parent": nil, "priority": nil, "assignee": issue.assignee}
}

// openAsks is the project's open asks as Dispatch answers them.
func (b *approvalsBoard) openAsks() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	rows := []map[string]any{}
	for _, key := range slices.Sorted(maps.Keys(b.asks)) {
		for _, ask := range b.asks[key] {
			rows = append(rows, map[string]any{"id": ask.id, "owner": map[string]any{"issue": map[string]any{"key": key, "title": key, "assignee": b.issues[key].assignee}},
				"waiting_on": ask.waitingOn})
		}
	}
	if b.documentAsk {
		rows = append(rows, map[string]any{"id": "ask-of-a-document", "owner": map[string]any{"document": map[string]any{"project": queueProject, "slug": "spec", "name": "spec.md"}},
			"waiting_on": "human"})
	}
	answer := map[string]any{"session_id": "", "as_of": "2026-11-26T09:00:00.5Z", "opened_since": false, "count": len(rows), "asks": rows}
	if !b.noDefault {
		answer["default_assignee"] = b.defaultAssignee
	}
	return answer
}

// approvalsRig wires admission, the engine and the boot's read for the approvals signal against an
// approvalsBoard and an isolated Postgres.
type approvalsRig struct {
	t       *testing.T
	pool    *store.Pool
	records *record.Postgres
	w       *workflowRuntime
	board   *approvalsBoard
	now     time.Time
	events  int
}

func newApprovalsRig(t *testing.T, board *approvalsBoard) *approvalsRig {
	t.Helper()
	server := httptest.NewServer(board)
	t.Cleanup(server.Close)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	renames := ghrepo.Renames{}
	engine := workflow.New(records, workflow.Config{Project: queueProject, Repositories: testRepositories,
		RepositoryRenames: renames, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	admission := admit.New(records, engine, 2, queueProject, quietLogger())
	rig := &approvalsRig{t: t, pool: pool, records: records, board: board, now: time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)}
	admission.UseClock(func() time.Time { return rig.now })
	client := dispatch.New(server.URL, nil)
	rig.w = &workflowRuntime{pool: pool, records: records, handlers: admit.Handlers(records, queueProject, engine, admission),
		dispatch: client, approvals: client, dispatchProject: queueProject, log: quietLogger()}
	return rig
}

// tryApply applies fact as the daemon's consumer or a boot does, on a context of a worker of its
// own, so it may run where Dispatch is read (a hook of the board's).
func (r *approvalsRig) tryApply(source, id string, fact intake.Fact) error {
	_, err := intake.ApplyFact(store.ForConcurrentWorker(context.Background()), r.pool, source, id, fact, r.w.handlers...)
	return err
}

func (r *approvalsRig) apply(source, id string, fact intake.Fact) {
	r.t.Helper()
	if err := r.tryApply(source, id, fact); err != nil {
		r.t.Fatalf("apply %s %s (%T): %v", source, id, fact, err)
	}
}

// deliver applies an event of an issue of the project as the Dispatch consumer does: decoded by the
// daemon's decoder from the event as Dispatch's log serves it, at its sequence in the issue's log.
func (r *approvalsRig) deliver(issue string, seq int64, eventType string, payload map[string]any) error {
	raw, err := json.Marshal(map[string]any{"id": 1000 + seq, "issue_key": issue, "seq": seq, "type": eventType, "notify": false,
		"created_at": "2026-11-26T08:00:00Z", "actor": map[string]any{"kind": "user", "id": "alice"}, "payload": payload})
	if err != nil {
		return err
	}
	fact, _, err := intake.DecodeDispatchEvent(queueProject, issue, raw)
	if err != nil || fact == nil {
		return fmt.Errorf("decode the %s event of %s: %v (fact %v)", eventType, issue, err, fact)
	}
	r.events++
	return r.tryApply("dispatch", fmt.Sprintf("dispatch-approvals-%s-%d-%d", issue, seq, r.events), fact)
}

func (r *approvalsRig) mustDeliver(issue string, seq int64, eventType string, payload map[string]any) {
	r.t.Helper()
	if err := r.deliver(issue, seq, eventType, payload); err != nil {
		r.t.Fatal(err)
	}
}

func askPayload(id, state string, askedOf any) map[string]any {
	return map[string]any{"id": id, "kind": "question", "state": state, "asked_of": askedOf}
}

func issuePayload(key string, assignee any) map[string]any {
	return map[string]any{"key": key, "status": "triage", "title": key, "rank": "a", "labels": []string{}, "parent": nil, "claim": nil,
		"resolved_repo": nil, "blocked_by": []string{}, "assignee": assignee}
}

// bootFact applies the nth boot's fact, which begins keeping each input of a signal.
func (r *approvalsRig) bootFact(n int) {
	r.t.Helper()
	r.now = r.now.Add(time.Minute)
	r.apply("daemon", fmt.Sprintf("boot:%s:%d", queueProject, n), intake.DaemonBooted{Boot: int64(n)})
}

// boot applies the nth boot's fact and reads Dispatch once, as a daemon's boot does.
func (r *approvalsRig) boot(n int) bool {
	r.t.Helper()
	r.bootFact(n)
	return r.w.readApprovalsOnce(context.Background(), fmt.Sprintf("%d.1", n))
}

func (r *approvalsRig) signal() record.Approvals {
	r.t.Helper()
	var got record.Approvals
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = r.records.ApprovalsOf(ctx, tx, queueProject)
		return err
	}); err != nil {
		r.t.Fatalf("read the approvals signal: %v", err)
	}
	return got
}

// waiting is the asks the signal counts, failing where it is not recorded.
func (r *approvalsRig) waiting() []string {
	r.t.Helper()
	got := r.signal()
	if got.Unrecorded != "" {
		r.t.Fatalf("the approvals signal is not recorded (%s), want a count", got.Unrecorded)
	}
	return got.Waiting
}

func (r *approvalsRig) status() (time.Time, bool, string) {
	r.t.Helper()
	var since time.Time
	var recorded bool
	var readError string
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		since, recorded, readError, err = r.records.SignalInputStatus(ctx, tx, queueProject, signals.Approvals)
		return err
	}); err != nil {
		r.t.Fatalf("read the status of the approvals input: %v", err)
	}
	return since, recorded, readError
}

// projectBoard is the project as the boot tests read it: four issues and the asks open on three of
// them. With dana the project's default assignee, the asks that wait on her are ask-b, which was
// handed to her, and ask-c, which was handed to nobody on an issue nobody holds; ask-a waits on its
// issue's assignee alice, ask-d on carol, ask-e on its agent, ask-f on bob, and LEGION-4, whose
// assignee is erin, holds no ask yet.
func projectBoard(t *testing.T) *approvalsBoard {
	t.Helper()
	return &approvalsBoard{
		issues: map[string]approvalsIssue{
			"LEGION-1": {seq: 10, assignee: "alice"}, "LEGION-2": {seq: 4}, "LEGION-3": {seq: 6, assignee: "bob"}, "LEGION-4": {seq: 3, assignee: "erin"},
		},
		asks: map[string][]approvalsAsk{
			"LEGION-1": {{id: "ask-a", waitingOn: "human"}, {id: "ask-b", askedOf: "dana", waitingOn: "human"}},
			"LEGION-2": {{id: "ask-c", waitingOn: "human"}, {id: "ask-d", askedOf: "carol", waitingOn: "human"}, {id: "ask-e", waitingOn: "agent"}},
			"LEGION-3": {{id: "ask-f", waitingOn: "human"}},
		},
		documentAsk:     true,
		defaultAssignee: "dana",
	}
}

// testSignalBootReconcilesAsks is the boot's read for the approvals signal against Dispatch, a real
// Postgres and the daemon's own decoder: what the boot reads of the open asks, the issues'
// assignees and the project's default assignee, a restart over the same store, an ask that closed
// while the daemon was down, a Dispatch that gives no default assignee, a failed read, and the
// events that land during the read, are older than it, or come after it.
func testSignalBootReconcilesAsks(t *testing.T) {
	t.Run("the boot reads the open asks, the assignees and the default assignee, and a restart gives the same value", func(t *testing.T) {
		t.Parallel()
		board := projectBoard(t)
		rig := newApprovalsRig(t, board)
		if got := rig.signal(); got.Unrecorded != record.NoDefaultAssignee {
			t.Fatalf("before the boot the signal = %+v, want it not recorded: no default assignee is known", got)
		}
		// The boot's fact alone begins keeping the approvals input from that moment, with no read of
		// Dispatch made yet, so the input says that the read has not succeeded; the read that follows
		// is the boot's (readApprovalsOnce).
		rig.bootFact(1)
		began, recorded, readError := rig.status()
		if !recorded || readError != record.ApprovalsNotRead || !began.Equal(rig.now) {
			t.Fatalf("after the boot fact the approvals input = (since %s, recorded %t, error %q), want it kept from %s with the reason %q", began, recorded, readError, rig.now, record.ApprovalsNotRead)
		}
		if !rig.w.readApprovalsOnce(context.Background(), "1.1") {
			t.Fatal("the boot's read of Dispatch failed, want it to succeed")
		}
		want := []string{"ask-b", "ask-c"}
		if got := rig.waiting(); !slices.Equal(got, want) {
			t.Fatalf("the asks that wait on dana after the boot = %v, want %v: the ask of a document is dropped, and each of the others waits on its own person", got, want)
		}
		if got := rig.signal().Value(); got != (signals.Value{Recorded: true, Value: 2}) {
			t.Fatalf("the signal = %+v, want 2 recorded", got)
		}
		if _, recorded, readError := rig.status(); !recorded || readError != "" {
			t.Fatalf("after the read the approvals input = (recorded %t, error %q), want it kept with no error", recorded, readError)
		}

		// A restart over the same store gives the same value, and the input stays kept from the first
		// boot.
		if !rig.boot(2) {
			t.Fatal("the read after a restart failed")
		}
		if got := rig.waiting(); !slices.Equal(got, want) {
			t.Fatalf("the asks that wait on dana after a restart = %v, want the same %v", got, want)
		}
		if since, _, _ := rig.status(); !since.Equal(began) {
			t.Fatalf("the approvals input is kept since %s after a restart, want %s: no boot moves it", since, began)
		}

		// An issue nobody has changed since this release began keeping assignees has the assignee the
		// boot's listing gave it, so an ask opened on it is read against that person: LEGION-4 is erin's.
		rig.mustDeliver("LEGION-4", 4, "ask.opened", askPayload("ask-g", "open", nil))
		if got := rig.waiting(); !slices.Equal(got, want) {
			t.Fatalf("with an ask opened on erin's LEGION-4 the asks that wait on dana = %v, want the same %v", got, want)
		}
		// One opened on an issue Dispatch listed with no assignee waits on the default.
		rig.mustDeliver("LEGION-2", 5, "ask.opened", askPayload("ask-h", "open", nil))
		if got, want := rig.waiting(), []string{"ask-b", "ask-c", "ask-h"}; !slices.Equal(got, want) {
			t.Fatalf("with an ask opened on LEGION-2 the asks that wait on dana = %v, want %v", got, want)
		}
	})

	t.Run("an ask that closed while the daemon was down is found closed", func(t *testing.T) {
		t.Parallel()
		board := projectBoard(t)
		rig := newApprovalsRig(t, board)
		if !rig.boot(1) {
			t.Fatal("the first boot's read failed")
		}
		// While the daemon is down ask-b is answered, ask-c's issue is closed to its asks, and an
		// ask is opened on an issue the first read did not hold.
		board.mu.Lock()
		board.asks["LEGION-1"] = []approvalsAsk{{id: "ask-a", waitingOn: "human"}}
		board.issues["LEGION-1"] = approvalsIssue{seq: 12, assignee: "alice"}
		delete(board.asks, "LEGION-2")
		board.issues["LEGION-2"] = approvalsIssue{seq: 9}
		board.issues["LEGION-5"] = approvalsIssue{seq: 2}
		board.asks["LEGION-5"] = []approvalsAsk{{id: "ask-i", waitingOn: "human"}}
		board.mu.Unlock()
		if !rig.boot(2) {
			t.Fatal("the read after the restart failed")
		}
		if got, want := rig.waiting(), []string{"ask-i"}; !slices.Equal(got, want) {
			t.Fatalf("the asks that wait on dana after a restart = %v, want %v: ask-b and ask-c closed while the daemon was down, and ask-i opened on an issue it had not read", got, want)
		}
		// An issue Dispatch has none of holds no ask open, and the one the store held closes with it.
		board.mu.Lock()
		delete(board.issues, "LEGION-5")
		delete(board.asks, "LEGION-5")
		board.mu.Unlock()
		if !rig.boot(3) {
			t.Fatal("the read after a second restart failed")
		}
		if got := rig.waiting(); len(got) != 0 {
			t.Fatalf("after the issue went away the asks that wait on dana = %v, want none", got)
		}
	})

	// The first boot of the release that keeps the asks has kept none of them yet: until its read
	// of Dispatch succeeds, a default assignee that an event gives counts only the asks opened
	// since, which is too few, so the signal says that the read has not finished and shows no
	// number. A later boot reads over what the earlier run kept, which the stream kept current, so
	// it holds its value while its read runs.
	t.Run("the first boot gives no value before its read of Dispatch has succeeded", func(t *testing.T) {
		t.Parallel()
		board := projectBoard(t)
		rig := newApprovalsRig(t, board)
		rig.bootFact(1)
		rig.apply("dispatch", "dispatch-default-before-the-read", intake.DispatchDefaultAssignee{Assignee: "dana", Seq: 4225, At: rig.now})
		rig.mustDeliver("LEGION-2", 5, "ask.opened", askPayload("ask-h", "open", nil))
		if got := rig.signal(); got.Unrecorded == "" || got.Value().Recorded {
			t.Fatalf("before the first boot's read the signal = %+v (value %+v), want it not recorded with the reason, and never a number", got, got.Value())
		}
		if !rig.w.readApprovalsOnce(context.Background(), "1.1") {
			t.Fatal("the boot's read of Dispatch failed, want it to succeed")
		}
		if got, want := rig.waiting(), []string{"ask-b", "ask-c", "ask-h"}; !slices.Equal(got, want) {
			t.Fatalf("after the first boot's read the asks that wait on dana = %v, want %v", got, want)
		}
		rig.bootFact(2)
		if got, want := rig.waiting(), []string{"ask-b", "ask-c", "ask-h"}; !slices.Equal(got, want) {
			t.Fatalf("after a second boot's fact, before its read, the asks that wait on dana = %v, want the kept %v", got, want)
		}
	})

	t.Run("a Dispatch that gives no default assignee gives no value, and a kept event gives it", func(t *testing.T) {
		t.Parallel()
		for name, answer := range map[string]func(*approvalsBoard){
			"a Dispatch before the member": func(b *approvalsBoard) { b.noDefault = true },
			"a project it stores none of":  func(b *approvalsBoard) { b.defaultAssignee = nil },
		} {
			board := projectBoard(t)
			answer(board)
			rig := newApprovalsRig(t, board)
			if !rig.boot(1) {
				t.Fatalf("%s: the boot's read failed, want it to succeed and say nothing of the default", name)
			}
			if got := rig.signal(); got.Unrecorded != record.NoDefaultAssignee || got.Value().Recorded {
				t.Fatalf("%s: the signal = %+v (value %+v), want it not recorded for %q and never a number", name, got, got.Value(), record.NoDefaultAssignee)
			}
			// The first event that moves the default gives it, and a restart against the same Dispatch
			// keeps it.
			rig.apply("dispatch", "dispatch-default-"+name, intake.DispatchDefaultAssignee{Assignee: "dana", Seq: 4225, At: rig.now})
			if got, want := rig.waiting(), []string{"ask-b", "ask-c"}; !slices.Equal(got, want) {
				t.Fatalf("%s: with the default assignee an event gave, the asks that wait on dana = %v, want %v", name, got, want)
			}
			if !rig.boot(2) {
				t.Fatalf("%s: the read after a restart failed", name)
			}
			if got, want := rig.waiting(), []string{"ask-b", "ask-c"}; !slices.Equal(got, want) {
				t.Fatalf("%s: after a restart against a Dispatch that still gives none the asks that wait on dana = %v, want %v: the kept event stands", name, got, want)
			}
		}
	})

	t.Run("a failed read says why and keeps nothing, and a read that succeeds clears it", func(t *testing.T) {
		t.Parallel()
		board := projectBoard(t)
		rig := newApprovalsRig(t, board)
		// Dispatch answers the project's open asks and refuses the asks of one issue: the read fails
		// as a whole, so the asks of the other issues it did read are not kept either.
		board.refuse = "/api/v1/issues/LEGION-2/asks"
		if rig.boot(1) {
			t.Fatal("the read succeeded while Dispatch refused an issue's asks, want it failed")
		}
		got := rig.signal()
		if !strings.Contains(got.Unrecorded, "read the open asks of LEGION-2") || got.Value().Recorded {
			t.Fatalf("after a failed read the signal = %+v, want it not recorded, for the read that failed", got)
		}
		if _, recorded, readError := rig.status(); !recorded || readError != got.Unrecorded {
			t.Fatalf("the approvals input = (recorded %t, error %q), want it kept with the read's reason %q", recorded, readError, got.Unrecorded)
		}
		var kept int
		if err := rig.pool.QueryRow(context.Background(), `select count(*) from approval_asks where project = $1`, queueProject).Scan(&kept); err != nil || kept != 0 {
			t.Fatalf("a failed read kept %d asks (%v), want none", kept, err)
		}
		board.refuse = "/api/v1/asks/open"
		if rig.w.readApprovalsOnce(context.Background(), "1.2") {
			t.Fatal("the read succeeded while Dispatch refused the project's open asks")
		}
		if got := rig.signal(); !strings.Contains(got.Unrecorded, "read the open asks of LEGION: ") {
			t.Fatalf("after a second failed read the signal = %+v, want the reason of that read", got)
		}
		board.refuse = ""
		if !rig.w.readApprovalsOnce(context.Background(), "1.3") {
			t.Fatal("the read failed once Dispatch answered")
		}
		if _, _, readError := rig.status(); readError != "" {
			t.Fatalf("after a read succeeded the approvals input holds the reason %q, want none", readError)
		}
		if got, want := rig.waiting(), []string{"ask-b", "ask-c"}; !slices.Equal(got, want) {
			t.Fatalf("after a read succeeded the asks that wait on dana = %v, want %v", got, want)
		}
	})

	t.Run("an event that lands during the read stands, an older one changes nothing and a newer one is applied", func(t *testing.T) {
		t.Parallel()
		board := projectBoard(t)
		rig := newApprovalsRig(t, board)
		// ask-c is answered, an event at sequence 20 of LEGION-2's log, while the boot reads LEGION-2's
		// asks: the read's listing stood at 4, so Dispatch's list, which still shows it open, is older
		// than the event.
		var landed bool
		board.onIssueAsks = func(issue string) {
			if issue != "LEGION-2" || landed {
				return
			}
			landed = true
			if err := rig.deliver("LEGION-2", 20, "ask.answered", askPayload("ask-c", "answered", nil)); err != nil {
				t.Errorf("deliver the answer during the read: %v", err)
			}
		}
		booted := rig.boot(1)
		if !booted || !landed {
			t.Fatalf("the read succeeded: %t, the event landed during it: %t, want both", booted, landed)
		}
		if got, want := rig.waiting(), []string{"ask-b"}; !slices.Equal(got, want) {
			t.Fatalf("the asks that wait on dana = %v, want %v: ask-c was answered by an event the read stands under", got, want)
		}

		// An event older than the listing the read stood at, delivered after it, changes nothing: the
		// answer of ask-b at sequence 9 of LEGION-1's log, which the listing's 10 is past, and the
		// opening of ask-c again at its own place.
		rig.mustDeliver("LEGION-1", 9, "ask.answered", askPayload("ask-b", "answered", "dana"))
		rig.mustDeliver("LEGION-2", 3, "ask.opened", askPayload("ask-c", "open", nil))
		if got, want := rig.waiting(), []string{"ask-b"}; !slices.Equal(got, want) {
			t.Fatalf("after older events the asks that wait on dana = %v, want %v", got, want)
		}

		// Newer events are applied: ask-b is handed to erin and then back to LEGION-1's assignee, alice,
		// and waits on dana again when LEGION-1 loses its assignee; an older assignee event is refused.
		rig.mustDeliver("LEGION-1", 12, "ask.asked_of", askPayload("ask-b", "open", "erin"))
		if got := rig.waiting(); len(got) != 0 {
			t.Fatalf("with ask-b handed to erin the asks that wait on dana = %v, want none", got)
		}
		rig.mustDeliver("LEGION-1", 13, "ask.asked_of", askPayload("ask-b", "open", nil))
		if got := rig.waiting(); len(got) != 0 {
			t.Fatalf("with ask-b handed back to alice's issue the asks that wait on dana = %v, want none", got)
		}
		rig.mustDeliver("LEGION-1", 14, "issue.updated", issuePayload("LEGION-1", nil))
		rig.mustDeliver("LEGION-1", 9, "issue.updated", issuePayload("LEGION-1", "zed"))
		if got, want := rig.waiting(), []string{"ask-a", "ask-b"}; !slices.Equal(got, want) {
			t.Fatalf("with LEGION-1 holding nobody the asks that wait on dana = %v, want %v: both of its asks, and the older assignee event changed nothing", got, want)
		}

		// The default assignee moves with its settings event, which the read-sourced default yields to
		// whatever its sequence, and an older settings event after it changes nothing.
		rig.apply("dispatch", "dispatch-default-5", intake.DispatchDefaultAssignee{Assignee: "bob", Seq: 5, At: rig.now})
		rig.apply("dispatch", "dispatch-default-3", intake.DispatchDefaultAssignee{Assignee: "erin", Seq: 3, At: rig.now})
		if got, want := rig.waiting(), []string{"ask-a", "ask-b", "ask-f"}; !slices.Equal(got, want) {
			t.Fatalf("with bob the default assignee the asks that wait on him = %v, want %v: the two asks of LEGION-1, which holds nobody, and the one of bob's LEGION-3; the older settings event changed nothing", got, want)
		}
		rig.apply("dispatch", "dispatch-default-6", intake.DispatchDefaultAssignee{Assignee: "carol", Seq: 6, At: rig.now})
		if got, want := rig.waiting(), []string{"ask-a", "ask-b", "ask-d"}; !slices.Equal(got, want) {
			t.Fatalf("with carol the default assignee the asks that wait on her = %v, want %v: the two asks of LEGION-1 and the one handed to her", got, want)
		}
	})
}

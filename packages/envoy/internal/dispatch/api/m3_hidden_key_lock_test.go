package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// lockKeys is what one call of a lock row names: an issue key and a project key.
type lockKeys struct{ issue, project string }

// The three calls of a lock row: PRIV's objects, hidden from alice and from the shared token,
// which see PUB alone; objects nobody holds, the ones TestNoWriteReachesAHiddenObject names
// (seedHiddenWrites); and PUB's, which both callers see.
var (
	hiddenLockKeys  = lockKeys{issue: seedPrivate + "-1", project: seedPrivate}
	missingLockKeys = lockKeys{issue: "NOPE-1", project: "NOPE"}
	visibleLockKeys = lockKeys{issue: seedPublic + "-1", project: seedPublic}
)

// lockRow is one write that takes a lock of an object its path or body names before it has read
// that object: a writer of that object holds the lock (hold), and the write must answer an object
// its caller does not see without waiting for that writer, as it answers an object nobody holds,
// whose lock nobody holds.
type lockRow struct {
	name   string
	caller privateCaller
	method string
	// target is the call's path and body for keys.
	target func(keys lockKeys) (string, string)
	// hold takes, in holder, the lock a writer of keys holds, which the write takes too.
	hold func(t *testing.T, holder pgx.Tx, keys lockKeys)
	// visible is the status the write answers for PUB's objects once the holder lets go.
	visible int
	// missing is the write's answer for objects nobody holds, as 99e11cdd50 gave it in a
	// deployment that holds no private project.
	missing string
}

// holdLockOf holds the owner lock of owner(keys), as every writer of that owner takes it.
func holdLockOf(owner func(keys lockKeys) store.Owner) func(*testing.T, pgx.Tx, lockKeys) {
	return func(t *testing.T, holder pgx.Tx, keys lockKeys) {
		t.Helper()
		if err := store.LockOwner(context.Background(), holder, owner(keys)); err != nil {
			t.Fatalf("hold %s's owner lock: %v", owner(keys).LockKey(), err)
		}
	}
}

var (
	holdIssueOwner   = holdLockOf(func(keys lockKeys) store.Owner { return store.IssueOwner(keys.issue) })
	holdProjectOwner = holdLockOf(func(keys lockKeys) store.Owner { return store.ProjectOwner(keys.project) })
)

// holdProjectDependencies holds the project's dependency lock, as a reparent or a blocked_by write
// of one of its issues takes it (lockIssueDependencyWrite).
func holdProjectDependencies(t *testing.T, holder pgx.Tx, keys lockKeys) {
	t.Helper()
	if err := lockProjectIssueDependencies(context.Background(), holder, keys.project); err != nil {
		t.Fatalf("hold %s's dependency lock: %v", keys.project, err)
	}
}

// lockRows is every lock row: the path-key writes of the review's sites and the body fields a PATCH
// and a create lock by. second is a second issue of PUB, the one a PATCH naming another issue in
// its body writes; step is a run id no record holds.
func lockRows(second, step string) []lockRow {
	path := func(format string) func(lockKeys) (string, string) {
		return func(keys lockKeys) (string, string) {
			return strings.NewReplacer("{key}", keys.issue, "{project}", keys.project).Replace(format), ""
		}
	}
	with := func(target func(lockKeys) (string, string), body func(keys lockKeys) string) func(lockKeys) (string, string) {
		return func(keys lockKeys) (string, string) {
			path, _ := target(keys)
			return path, body(keys)
		}
	}
	quoted := func(value string) string {
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
	issueBody := func(body string) func(lockKeys) string { return func(lockKeys) string { return body } }
	notFound := `404 {"code":"NOT_FOUND","error":"not found"}`
	return []lockRow{
		{"PATCH /api/v1/issues/{key}", callerAlice, http.MethodPatch,
			with(path("/api/v1/issues/{key}"), issueBody(`{`+probeActor+`,"title":"A walked title"}`)),
			holdIssueOwner, http.StatusOK, notFound},
		{"PATCH /api/v1/issues/{key} naming a parent", callerAlice, http.MethodPatch,
			with(path("/api/v1/issues/"+second), func(keys lockKeys) string { return `{` + probeActor + `,"parent":` + quoted(keys.issue) + `}` }),
			holdIssueOwner, http.StatusOK, `400 {"code":"PARENT_INPUT","error":"parent issue NOPE-1 not found"}`},
		{"PATCH /api/v1/issues/{key} naming a rank neighbour", callerAlice, http.MethodPatch,
			with(path("/api/v1/issues/"+second), func(keys lockKeys) string {
				return `{` + probeActor + `,"rank":{"after":` + quoted(keys.issue) + `}}`
			}),
			holdIssueOwner, http.StatusOK, `400 {"code":"RANK_INPUT","error":"rank neighbor does not exist"}`},
		{"POST /api/v1/issues", callerAlice, http.MethodPost,
			with(path("/api/v1/issues"), func(keys lockKeys) string { return `{"project":` + quoted(keys.project) + `,"title":"A walked issue"}` }),
			holdProjectOwner, http.StatusCreated, notFound},
		{"POST /api/v1/issues naming a parent", callerAlice, http.MethodPost,
			with(path("/api/v1/issues"), func(keys lockKeys) string {
				return `{"project":` + quoted(keys.project) + `,"title":"A walked child","parent":` + quoted(keys.issue) + `}`
			}),
			holdProjectDependencies, http.StatusCreated, `400 {"code":"PARENT_INPUT","error":"parent issue NOPE-1 not found"}`},
		{"PUT /api/v1/projects/{key}/default-assignee", callerAlice, http.MethodPut,
			with(path("/api/v1/projects/{project}/default-assignee"), issueBody(`{"assignee":"alice"}`)),
			holdProjectOwner, http.StatusOK, notFound},
		// Main's route (#4457) took the owner lock and wrote the row before it read the project;
		// this build reads the project as its writer reads it first, so a writer that does not see
		// PRIV never queues behind PRIV's writer (control: the read before the lock taken out).
		{"PUT /api/v1/projects/{key}/default-repository", callerAlice, http.MethodPut,
			with(path("/api/v1/projects/{project}/default-repository"), issueBody(`{"repo":"acme/walked"}`)),
			holdProjectOwner, http.StatusOK, `404 {"code":"PROJECT_NOT_FOUND","error":"project not found"}`},
		{"PUT /api/v1/issues/{key}/design-gates", callerAlice, http.MethodPut,
			with(path("/api/v1/issues/{key}/design-gates"), issueBody(`{`+probeActor+`,"gates":[]}`)),
			holdIssueOwner, http.StatusOK, `404 {"code":"ISSUE_NOT_FOUND","error":"no issue NOPE-1"}`},
		{"PUT /api/v1/issues/{key}/steps/{id}", callerAlice, http.MethodPut,
			with(path("/api/v1/issues/{key}/steps/"+step), issueBody(`{`+probeActor+`}`)),
			holdIssueOwner, http.StatusForbidden, `404 {"code":"ISSUE_NOT_FOUND","error":"no issue NOPE-1"}`},
		{"PUT /api/v1/issues/{key}/plans/{id}", callerAlice, http.MethodPut,
			with(path("/api/v1/issues/{key}/plans/"+step), issueBody(`{`+probeActor+`}`)),
			holdIssueOwner, http.StatusForbidden, `404 {"code":"ISSUE_NOT_FOUND","error":"no issue NOPE-1"}`},
		{"POST /api/v1/issues/{key}/messages", callerAlice, http.MethodPost,
			with(path("/api/v1/issues/{key}/messages"), issueBody(`{`+probeActor+`,"body":"A walked message."}`)),
			holdIssueOwner, http.StatusCreated, notFound},
		{"POST /api/v1/issues/{key}/comments", callerAlice, http.MethodPost,
			with(path("/api/v1/issues/{key}/comments"), issueBody(`{`+probeActor+`,"body":"A walked comment."}`)),
			holdIssueOwner, http.StatusCreated, notFound},
		{"POST /api/v1/issues/{key}/asks", callerAlice, http.MethodPost,
			with(path("/api/v1/issues/{key}/asks"), issueBody(`{`+probeActor+`,"question":"Which way for the walk?"}`)),
			holdIssueOwner, http.StatusCreated, notFound},
		{"POST /api/v1/issues/{key}/handoffs/{phase}", callerSharedToken, http.MethodPost,
			with(path("/api/v1/issues/{key}/handoffs/build"), issueBody(`{`+probeActor+`,"body":"{\"next\":\"walked\"}"}`)),
			holdIssueOwner, http.StatusCreated, notFound},
		// Only the issue's project's daemon writes the set, so the caller is that daemon over the
		// deployment's shared token, which sees the public projects alone: PRIV's daemon writing
		// through it is blind to PRIV.
		{"PUT /api/v1/issues/{key}/role-holders", callerSharedToken, http.MethodPut,
			with(path("/api/v1/issues/{key}/role-holders"), func(keys lockKeys) string {
				return `{"actor":{"kind":"session","id":` + quoted("legion-daemon:"+keys.project) + `},"holders":[]}`
			}),
			holdIssueOwner, http.StatusOK, `404 {"code":"ISSUE_NOT_FOUND","error":"no issue NOPE-1"}`},
	}
}

// lockSeed is verification v16's seed with no settlement running during a row (a settlement takes
// an issue's owner lock, and would queue behind a holder as the write under test would), and a
// second issue of PUB, which a PATCH naming PUB's first issue in its body writes.
func lockSeed(t *testing.T) (privateSeed, string) {
	t.Helper()
	seed := seedPrivateProjectWith(t, testServerOptions{settle: time.Hour})
	for project, keys := range map[string]lockKeys{seedPublic: visibleLockKeys, seedPrivate: hiddenLockKeys} {
		if seed.issue[project] != keys.issue {
			t.Fatalf("%s's issue is %s; the rows name %s", project, seed.issue[project], keys.issue)
		}
	}
	second := decodeBody[struct {
		Key string `json:"key"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": seedPublic, "title": "The second issue of PUB",
	}, http.StatusCreated)).Key
	return seed, second
}

// call makes row's call of keys as its caller, on a goroutine, and answers the channel its answer
// comes on.
func (row lockRow) call(t *testing.T, seed privateSeed, keys lockKeys) <-chan *httptest.ResponseRecorder {
	t.Helper()
	path, body := row.target(keys)
	var payload any
	if body != "" {
		payload = json.RawMessage(body)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- seed.request(t, row.caller, row.method, path, payload) }()
	return answered
}

// heldLock is a transaction of the test's own that holds a lock a write takes, and its backend.
type heldLock struct {
	tx  pgx.Tx
	pid int32
}

// holdFor begins a transaction on the seed's pool, takes row's lock of keys in it, and answers it;
// it is rolled back when the test ends, if nothing let it go before.
func (row lockRow) holdFor(t *testing.T, seed privateSeed, keys lockKeys) heldLock {
	t.Helper()
	ctx := context.Background()
	tx, err := seed.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var pid int32
	if err := tx.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the holder's backend: %v", err)
	}
	row.hold(t, tx, keys)
	return heldLock{tx: tx, pid: pid}
}

// awaitAnswerOrWait waits, up to storeStepWait, for the answer on answered while held holds its
// lock. It answers the response when the write answered first, and otherwise the statement a
// backend queued behind the holder is waiting at: a write that waits there is the one under test,
// since no other writer of the seed runs while a row does (lockSeed). The bound is far above what
// any of these writes takes, so passing it is a write that neither answers nor waits on the holder.
func awaitAnswerOrWait(t *testing.T, seed privateSeed, held heldLock, answered <-chan *httptest.ResponseRecorder) (*httptest.ResponseRecorder, string) {
	t.Helper()
	deadline := time.NewTimer(storeStepWait)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case response := <-answered:
			return response, ""
		case <-deadline.C:
			t.Fatalf("the write neither answered nor queued behind the holder within %s", storeStepWait)
		case <-tick.C:
		}
		var waiting *string
		err := seed.database.Pool.QueryRow(context.Background(), `
			select query from pg_stat_activity where $1::int = any(pg_blocking_pids(pid)) limit 1
		`, held.pid).Scan(&waiting)
		if err == nil {
			return nil, deref(waiting)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read who waits behind the holder: %v", err)
		}
	}
}

// letGo lets the holder go and answers the write's answer, which must come within storeStepWait.
func (held heldLock) letGo(t *testing.T, answered <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	if err := held.tx.Rollback(context.Background()); err != nil {
		t.Fatalf("let the holder go: %v", err)
	}
	select {
	case response := <-answered:
		return response
	case <-time.After(storeStepWait):
		t.Fatalf("the write did not answer within %s of the holder letting go", storeStepWait)
		return nil
	}
}

// lockAnswer is a response as the rows compare it: its status and its body.
func lockAnswer(response *httptest.ResponseRecorder) string {
	return strconv.Itoa(response.Code) + " " + strings.TrimSpace(response.Body.String())
}

// missingAsHidden writes an answer to missingLockKeys as the same answer to hiddenLockKeys.
var missingAsHidden = strings.NewReplacer(missingLockKeys.issue, hiddenLockKeys.issue, missingLockKeys.project, hiddenLockKeys.project)

// TestAWriteNamingAHiddenKeyNeverWaitsOnItsLock: while a writer of PRIV holds the lock a write
// takes - the issue's or the project's owner lock, or the project's dependency lock - a write that
// names PRIV's object, by a caller that does not see PRIV, answers without queueing behind that
// writer, as a write naming an object nobody holds does, whose lock nobody holds: a write that
// waited would tell its caller that a writer of a hidden object is at work. Its answer is the
// answer to an object nobody holds, the id written as PRIV's.
func TestAWriteNamingAHiddenKeyNeverWaitsOnItsLock(t *testing.T) {
	seed, second := lockSeed(t)
	for _, row := range lockRows(second, uuid.NewString()) {
		t.Run(row.name, func(t *testing.T) {
			held := row.holdFor(t, seed, hiddenLockKeys)
			answered := row.call(t, seed, hiddenLockKeys)
			hidden, waiting := awaitAnswerOrWait(t, seed, held, answered)
			if hidden == nil {
				t.Errorf("%s naming PRIV's object queued behind the writer holding its lock, at %q; one naming an object nobody holds never waits",
					row.name, waiting)
				hidden = held.letGo(t, answered)
			}
			missing := <-row.call(t, seed, missingLockKeys)
			if got, want := lockAnswer(hidden), missingAsHidden.Replace(lockAnswer(missing)); got != want {
				t.Errorf("%s naming PRIV's object answers %s, and naming an object nobody holds %s", row.name, got, lockAnswer(missing))
			}
		})
	}
}

// TestAWriteNamingAKeyItSeesStillWaitsOnItsLock: the same writes, naming PUB's objects, which
// their caller sees, still take the lock - they queue behind the writer that holds it and answer
// once it lets go - so the read each makes before its lock moves no write of a visible object out
// of the order its lock keeps.
func TestAWriteNamingAKeyItSeesStillWaitsOnItsLock(t *testing.T) {
	seed, second := lockSeed(t)
	for _, row := range lockRows(second, uuid.NewString()) {
		t.Run(row.name, func(t *testing.T) {
			held := row.holdFor(t, seed, visibleLockKeys)
			answered := row.call(t, seed, visibleLockKeys)
			early, waiting := awaitAnswerOrWait(t, seed, held, answered)
			if early != nil {
				t.Fatalf("%s naming PUB's object answered %s while a writer held its lock, so it took no lock of it", row.name, lockAnswer(early))
			}
			t.Logf("%s queued behind the holder at %q", row.name, waiting)
			if response := held.letGo(t, answered); response.Code != row.visible {
				t.Errorf("%s naming PUB's object answered %s once the holder let go, want %d", row.name, lockAnswer(response), row.visible)
			}
		})
	}
}

// TestALockWriteAnswersAMissingKeyAsBeforeWithNoPrivateProject: in a deployment that holds no
// private project (PRIV made public again), each of the writes answers an object nobody holds with
// the bytes 99e11cdd50 answered it with (lockRow.missing), so the read each now makes before its
// lock changes no answer there.
func TestALockWriteAnswersAMissingKeyAsBeforeWithNoPrivateProject(t *testing.T) {
	seed, second := lockSeed(t)
	seed.setVisibility(t, seedPrivate, "public")
	var private bool
	if err := seed.database.Pool.QueryRow(context.Background(), `select exists (select 1 from projects where visibility = 'private')`).Scan(&private); err != nil || private {
		t.Fatalf("the deployment still holds a private project (%v), so this row is not the one with none", err)
	}
	for _, row := range lockRows(second, uuid.NewString()) {
		t.Run(row.name, func(t *testing.T) {
			missing := <-row.call(t, seed, missingLockKeys)
			if got := lockAnswer(missing); got != row.missing {
				t.Errorf("%s naming an object nobody holds answers %s, want %s", row.name, got, row.missing)
			}
		})
	}
}

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// holdLock opens a transaction that runs lock and keeps it until the returned release runs, so a
// test can stop requests at a known point and let them through together.
func holdLock(t *testing.T, database *store.Store, lock string, args ...any) (release func()) {
	t.Helper()
	return holdWith(t, database, lock, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), lock, args...)
		return err
	})
}

// holdOwner is holdLock for an owner's advisory lock (store.LockOwner), the lock every writer of
// that owner takes first.
func holdOwner(t *testing.T, database *store.Store, owner store.Owner) (release func()) {
	t.Helper()
	return holdWith(t, database, string(owner.Kind)+" "+owner.Key+"'s owner lock", func(tx pgx.Tx) error {
		return store.LockOwner(context.Background(), tx, owner)
	})
}

// holdWith opens a transaction that runs take and keeps it until the returned release runs.
func holdWith(t *testing.T, database *store.Store, what string, take func(pgx.Tx) error) (release func()) {
	t.Helper()
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the holding transaction: %v", err)
	}
	if err := take(tx); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("take %q: %v", what, err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(context.Background())
		}
	}
	t.Cleanup(release)
	return release
}

// lockWaiters counts the backends of this test's own database that wait on a lock. Scoped to
// current_database() because one Postgres serves every test in the package.
func lockWaiters(t *testing.T, database *store.Store) int {
	t.Helper()
	var waiting int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from pg_stat_activity
		where datname = current_database() and wait_event_type = 'Lock' and pid <> pg_backend_pid()
	`).Scan(&waiting); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return waiting
}

// awaitLockWaiters waits until at least count backends of this test's own database wait on a lock.
func awaitLockWaiters(t *testing.T, database *store.Store, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		waiting := lockWaiters(t, database)
		if waiting >= count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d request(s) wait on a lock after 10s, want %d", waiting, count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// putMappingAsync sends a mapping PUT from its own goroutine and returns where its answer arrives.
func putMappingAsync(t *testing.T, handler http.Handler, repo string, body map[string]any) <-chan *httptest.ResponseRecorder {
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+repo, body, "alice")
	}()
	return answered
}

// patchAccessAsync sends PATCH /api/v1/projects/{key}/access as alice from its own goroutine and
// returns where its answer arrives.
func patchAccessAsync(t *testing.T, handler http.Handler, key string, body map[string]any) <-chan *httptest.ResponseRecorder {
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/"+key+"/access", body, "alice")
	}()
	return answered
}

func awaitAnswer(t *testing.T, answered <-chan *httptest.ResponseRecorder, what string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-answered:
		return response
	case <-time.After(15 * time.Second):
		t.Fatalf("%s never answered", what)
		return nil
	}
}

func createProjects(t *testing.T, handler http.Handler, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": key, "name": key}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
}

// Two first mappings of one repository, to two projects at once: the second is a move, so the
// project the first put it in hears that it left, as a sequential move tells it.
func TestRepoProjectFirstMappingRacedToTwoProjectsTellsTheProjectItLeft(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createProjects(t, handler, "CORE", "OTHER")
	release := holdOwner(t, database, store.ProjectOwner("CORE"))
	first := putMappingAsync(t, handler, "acme/new", map[string]any{"project": "CORE"})
	awaitLockWaiters(t, database, 1)
	second := putMappingAsync(t, handler, "acme/new", map[string]any{"project": "OTHER"})
	awaitLockWaiters(t, database, 2)
	release()
	for name, answered := range map[string]<-chan *httptest.ResponseRecorder{"CORE": first, "OTHER": second} {
		if response := awaitAnswer(t, answered, "map acme/new to "+name); response.Code != http.StatusOK {
			t.Fatalf("map acme/new to %s: status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
	core := projectEvents(t, database, "CORE", "settings.repo_project.updated")
	if len(core) != 2 {
		t.Fatalf("CORE saw %d mapping events, want its own mapping then its removal: %v", len(core), core)
	}
	if leaving := core[1]["mapping"].(map[string]any); leaving["repo"] != "acme/new" || leaving["project"] != "CORE" || core[1]["deleted"] != true {
		t.Fatalf("CORE's last event = %v, want acme/new leaving CORE, deleted:true", core[1])
	}
	if other := projectEvents(t, database, "OTHER", "settings.repo_project.updated"); len(other) != 1 || other[0]["deleted"] != false {
		t.Fatalf("OTHER's mapping events = %v, want one that adds acme/new", other)
	}
}

// Two mappings made their project's default at once both succeed, one after the other: the second
// demotes the first, and the project keeps exactly one default.
func TestRepoProjectSimultaneousDefaultsBothSucceed(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createProjects(t, handler, "CORE")
	for _, repo := range []string{"acme/widgets", "acme/gadgets"} {
		dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+repo, map[string]any{"project": "CORE"}, "alice")
	}
	release := holdOwner(t, database, store.ProjectOwner("CORE"))
	first := putMappingAsync(t, handler, "acme/widgets", map[string]any{"project": "CORE", "default": true})
	awaitLockWaiters(t, database, 1)
	second := putMappingAsync(t, handler, "acme/gadgets", map[string]any{"project": "CORE", "default": true})
	awaitLockWaiters(t, database, 2)
	release()
	for repo, answered := range map[string]<-chan *httptest.ResponseRecorder{"acme/widgets": first, "acme/gadgets": second} {
		if response := awaitAnswer(t, answered, "make "+repo+" the default"); response.Code != http.StatusOK {
			t.Fatalf("make %s the default: status=%d body=%s", repo, response.Code, response.Body.String())
		}
	}
	var defaults int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from repo_projects where project = 'CORE' and is_default`).Scan(&defaults); err != nil || defaults != 1 {
		t.Fatalf("CORE has %d defaults (%v), want 1", defaults, err)
	}
}

// Two mappings moved across the same two projects in opposite directions at once both succeed:
// each locks both projects in one order before its first event, so neither waits on the other's.
func TestRepoProjectOppositeMovesDoNotDeadlock(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createProjects(t, handler, "CORE", "OTHER")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/from-core", map[string]any{"project": "CORE"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/from-other", map[string]any{"project": "OTHER"}, "alice")
	// Each move stops at its first event, holding the owner locks it has taken by then: a trigger
	// has every event insert take a shared hold on a key the test holds. By then the first move has
	// locked CORE and OTHER (LockOwners sorts them by key), so the second waits on CORE in its own
	// LockOwners, and released, the two go one after the other. A move that locked each owner only
	// as it appended would stop holding the project it leaves, CORE for the first and OTHER for the
	// second; released together, each would wait on the other's, and Postgres would end one with
	// 40P01. The hold is shared, so once released it orders nothing itself, as writes to different
	// owners take no lock in common (events.Broker.Append). Holding a project's owner lock instead
	// would leave both moves waiting on that lock's holder, and which of them took the lock first
	// would be a race.
	const holdEvents = int64(4815162345)
	for _, statement := range []string{
		`create function dispatch_test_hold_events() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock_shared(tg_argv[0]::bigint);
			return new;
		end;
		$$`,
		fmt.Sprintf(`create trigger dispatch_test_hold_events before insert on events for each row
			execute function dispatch_test_hold_events('%d')`, holdEvents),
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("hold every event insert: %v", err)
		}
	}
	release := holdLock(t, database, `select pg_advisory_xact_lock($1)`, holdEvents)
	first := putMappingAsync(t, handler, "acme/from-core", map[string]any{"project": "OTHER"})
	awaitLockWaiters(t, database, 1)
	second := putMappingAsync(t, handler, "acme/from-other", map[string]any{"project": "CORE"})
	awaitLockWaiters(t, database, 2)
	release()
	for repo, answered := range map[string]<-chan *httptest.ResponseRecorder{"acme/from-core": first, "acme/from-other": second} {
		if response := awaitAnswer(t, answered, "move "+repo); response.Code != http.StatusOK {
			t.Fatalf("move %s: status=%d body=%s", repo, response.Code, response.Body.String())
		}
	}
}

// An issue created naming a mapping while that mapping moves away never deadlocks, and neither
// write refuses the other whichever order they take: the create takes no lock on the mapping
// row any more, because an issue's own repository needs no mapping (INFRA-2543 R3). Both take
// the project's owner lock, so one still waits for the other.
func TestIssueCreateNamingAMappingWhileItMovesDoesNotDeadlock(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createProjects(t, handler, "CORE", "OTHER")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE"}, "alice")
	release := holdOwner(t, database, store.ProjectOwner("CORE"))
	moved := putMappingAsync(t, handler, "acme/widgets", map[string]any{"project": "OTHER"})
	awaitLockWaiters(t, database, 1)
	created := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		created <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Raced", "repo": "acme/widgets", "force": true}, "alice")
	}()
	awaitLockWaiters(t, database, 2)
	release()
	move := awaitAnswer(t, moved, "move acme/widgets")
	create := awaitAnswer(t, created, "create an issue naming acme/widgets")
	if move.Code != http.StatusOK || create.Code != http.StatusCreated {
		t.Fatalf("move: %d %s; create: %d %s; want both to succeed whichever order they took", move.Code, move.Body.String(), create.Code, create.Body.String())
	}
	// The row both writes leave is one the dropped foreign key would have refused: the issue is
	// CORE's and names a repository OTHER maps.
	var orphans int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from issues i left join repo_projects r on (r.repo, r.project) = (i.repo, i.project_key)
		where i.repo is not null and r.repo is null
	`).Scan(&orphans); err != nil || orphans != 1 {
		t.Fatalf("%d issue(s) name a repository their project does not map (%v), want the one this race created", orphans, err)
	}
}

// Two writes made at once that would each put a private project's repository on another
// project's GitHub subject lane cannot both land. A deferred trigger holds each write at its
// commit, after its lane check has run, on a lock the test holds: without the lane lock both
// checks read the state before the other's write, and both commit once released; with it the
// second check waits for the first write's commit and reads it. alice owns PRIV and PUB, so she
// sees both, and makes every write: exactly one commits and the other answers 409
// REPOSITORY_LANE_SHARED.
func TestTwoConcurrentWritesCannotShareALane(t *testing.T) {
	const holdCommits = int64(4815162346)
	private := map[string]any{"visibility": model.ProjectPrivate}
	for _, race := range []struct {
		name  string
		setup func(t *testing.T, handler http.Handler)
		// writes are the two writes, in the order they start; landed reports which committed.
		writes [2]func(handler http.Handler) <-chan *httptest.ResponseRecorder
		landed func(t *testing.T, database *store.Store) [2]bool
	}{
		{"race 1: two mappings onto one lane, one of them into private PRIV",
			func(t *testing.T, handler http.Handler) {
				if made := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice"); made.Code != http.StatusOK {
					t.Fatalf("make PRIV private: %d %s", made.Code, made.Body.String())
				}
			},
			[2]func(http.Handler) <-chan *httptest.ResponseRecorder{
				func(handler http.Handler) <-chan *httptest.ResponseRecorder {
					return putMappingAsync(t, handler, "acme/a_b", map[string]any{"project": "PRIV"})
				},
				func(handler http.Handler) <-chan *httptest.ResponseRecorder {
					return putMappingAsync(t, handler, "acme/a.b", map[string]any{"project": "PUB"})
				},
			},
			func(t *testing.T, database *store.Store) [2]bool {
				return [2]bool{mappedTo(t, database, "acme/a_b") == "PRIV", mappedTo(t, database, "acme/a.b") == "PUB"}
			}},
		{"race 2: PRIV made private while a neighbour of its repository is mapped to PUB",
			func(t *testing.T, handler http.Handler) {
				mapRepo(t, handler, "alice", "acme/a_b", map[string]any{"project": "PRIV"})
			},
			[2]func(http.Handler) <-chan *httptest.ResponseRecorder{
				func(handler http.Handler) <-chan *httptest.ResponseRecorder {
					return patchAccessAsync(t, handler, "PRIV", private)
				},
				func(handler http.Handler) <-chan *httptest.ResponseRecorder {
					return putMappingAsync(t, handler, "acme/a.b", map[string]any{"project": "PUB"})
				},
			},
			func(t *testing.T, database *store.Store) [2]bool {
				return [2]bool{visibilityOf(t, database, "PRIV") == model.ProjectPrivate, mappedTo(t, database, "acme/a.b") == "PUB"}
			}},
	} {
		t.Run(race.name, func(t *testing.T) {
			handler, database := newLaneServer(t, &recordingBus{})
			race.setup(t, handler)
			for _, statement := range []string{
				`create function dispatch_test_hold_commit() returns trigger language plpgsql as $$
				begin
					perform pg_advisory_xact_lock_shared(tg_argv[0]::bigint);
					return null;
				end;
				$$`,
				fmt.Sprintf(`create constraint trigger dispatch_test_hold_mappings after insert or update on repo_projects
					deferrable initially deferred for each row execute function dispatch_test_hold_commit('%d')`, holdCommits),
				fmt.Sprintf(`create constraint trigger dispatch_test_hold_projects after update on projects
					deferrable initially deferred for each row execute function dispatch_test_hold_commit('%d')`, holdCommits),
			} {
				if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
					t.Fatalf("hold every commit that writes a mapping or a project: %v", err)
				}
			}
			release := holdLock(t, database, `select pg_advisory_xact_lock($1)`, holdCommits)
			first := race.writes[0](handler)
			awaitLockWaiters(t, database, 1)
			second := race.writes[1](handler)
			awaitLockWaiters(t, database, 2)
			release()
			answers := [2]*httptest.ResponseRecorder{awaitAnswer(t, first, "the first write"), awaitAnswer(t, second, "the second write")}
			landed := race.landed(t, database)
			var committed, refused int
			for index, answer := range answers {
				switch {
				case answer.Code == http.StatusOK && landed[index]:
					committed++
				case answer.Code == http.StatusConflict && strings.Contains(answer.Body.String(), `"code":"`+codeRepositoryLaneShared+`"`) && !landed[index]:
					refused++
				}
			}
			if committed != 1 || refused != 1 {
				t.Fatalf("%s: the writes answered %d %s and %d %s, landing %v; want exactly one committed and the other 409 %s",
					race.name, answers[0].Code, strings.TrimSpace(answers[0].Body.String()), answers[1].Code,
					strings.TrimSpace(answers[1].Body.String()), landed, codeRepositoryLaneShared)
			}
		})
	}
}

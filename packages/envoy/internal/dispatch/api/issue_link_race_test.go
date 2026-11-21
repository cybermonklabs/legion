package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// heldLinkLock is the advisory lock a test holds to keep a link write between its check and its
// insert: the trigger the test installs waits on it in the insert of a /pull/7 link.
const heldLinkLock = 4209012

// Two link writes at once cannot store one pull request in its two forms on two issues. Write A
// links acme/tools/pull/7 to the first issue, and a trigger holds A in its insert of that row,
// after its link check, until the test lets it go. While A is held, write B links the /issues/7
// form of the same pull request to the second issue, by a PATCH and, in the second case, by the
// create of an issue for owner/repo#7. B's check reads committed rows alone, so without a lock
// between the check and the write it finds nothing, and both links are stored: the pull request
// then has two issues, and the statement gives it to the lesser key. With the lock of the pull
// request (lockLinkedPullRequests), B waits for A to commit, and its check then reads A's row:
// the PATCH is refused with 409 EXTERNAL_LINK_TAKEN naming the first issue, and the create
// answers as it does for a link stored before it: the first issue holds owner/repo#7 and is of
// another project than the one acme/tools maps to, so 400 EXTERNAL_PROJECT_MISMATCH names its
// project (answerExistingExternalIssue).
//
// The first issue is in project OTHER and the second in CORE, which acme/tools maps to, so no
// owner lock the two writes would share (an issue's, or the create's of its project) orders B
// behind A: only the lock of the pull request can. In one project the create waits on A's lock
// of the project, and the case then passes with the lock of the pull request taken away. Two more
// PATCH cases hold the lock to the link rule: B links /Issues/7 of ACME/Tools, in other letters,
// and /issues/7 of oldco/tools, the name acme/tools had before a rename, each of which names the
// pull request A links and takes A's lock (linkLockFormSQL).
func TestTwoLinkWritesAtOnceStoreOnePullRequestOnOneIssue(t *testing.T) {
	for _, c := range []struct {
		second, url string
		rename      bool
	}{
		{second: "patch", url: "https://github.com/acme/tools/issues/7"},
		{second: "create"},
		{second: "patch_in_other_letters", url: "https://github.com/ACME/Tools/Issues/7"},
		{second: "patch_by_the_name_before_a_rename", url: "https://github.com/oldco/tools/issues/7", rename: true},
	} {
		second := c.second
		t.Run(second, func(t *testing.T) {
			handler, database := newTestHandlerWithStore(t)
			ctx := context.Background()
			for _, project := range []string{"OTHER", "CORE"} {
				if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": project}, "alice"); response.Code != http.StatusCreated {
					t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
				}
			}
			// The create for acme/tools#7 takes its project from the repository's mapping.
			if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/tools", map[string]any{"project": "CORE"}, "alice"); response.Code != http.StatusOK {
				t.Fatalf("map acme/tools to CORE: status=%d body=%s", response.Code, response.Body.String())
			}
			if c.rename {
				if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/oldco/tools",
					map[string]any{"project": "CORE", "renamed_to": "acme/tools"}, "alice"); response.Code != http.StatusOK {
					t.Fatalf("record oldco/tools as renamed to acme/tools: status=%d body=%s", response.Code, response.Body.String())
				}
			}
			keys := []string{}
			for _, project := range []string{"OTHER", "CORE"} {
				response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"project": project, "title": "An issue of " + project}, "alice")
				if response.Code != http.StatusCreated {
					t.Fatalf("create an issue of %s: status=%d body=%s", project, response.Code, response.Body.String())
				}
				keys = append(keys, decodeBody[model.Issue](t, response).Key)
			}
			one, two := keys[0], keys[1]
			link := func(key, url string) *httptest.ResponseRecorder {
				return sessionRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key,
					map[string]any{"external_links": []map[string]string{{"url": url, "kind": "github"}}, "actor": sessionActor()})
			}
			exec := func(statement string) {
				t.Helper()
				if _, err := database.Pool.Exec(ctx, statement); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			exec(`create function hold_pull_7_link() returns trigger language plpgsql as $$
				begin
					if new.url like '%/pull/7' then
						perform pg_advisory_lock(4209012);
						perform pg_advisory_unlock(4209012);
					end if;
					return new;
				end $$`)
			exec(`create trigger hold_pull_7_link before insert on issue_external_links
				for each row execute function hold_pull_7_link()`)
			held, err := database.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Release()
			if _, err := held.Exec(ctx, `select pg_advisory_lock($1)`, heldLinkLock); err != nil {
				t.Fatal(err)
			}
			released := false
			release := func() {
				if !released {
					released = true
					if _, err := held.Exec(ctx, `select pg_advisory_unlock($1)`, heldLinkLock); err != nil {
						t.Fatal(err)
					}
				}
			}
			defer release()
			// waitingOn is how many backends wait for an advisory lock that the where clause names,
			// read from pg_locks. Each wait is told apart by its lock, so a backend of the server that
			// waits on another lock behind A, a document's settlement waiting on A's owner lock for
			// one, counts as neither A held nor B waiting.
			waitingOn := func(where string, args ...any) int {
				var count int
				if err := database.Pool.QueryRow(ctx, `
					select count(*) from pg_locks l join pg_database d on d.oid = l.database
					where d.datname = current_database() and l.locktype = 'advisory' and not l.granted and `+where,
					args...).Scan(&count); err != nil {
					t.Fatal(err)
				}
				return count
			}
			// aHeld is A waiting for the lock the trigger holds it by, a one-key lock.
			aHeld := func() bool { return waitingOn(`l.objsubid = 1 and l.objid::bigint = $1::bigint`, heldLinkLock) > 0 }
			// bWaits is B waiting for the lock of the pull request (lockLinkedPullRequests), a two-key
			// lock whose first key is the hash of linkedPullRequestLockSpace.
			bWaits := func() bool {
				return waitingOn(`l.objsubid = 2 and l.classid = (hashtext($1)::bigint & 4294967295)::oid`, linkedPullRequestLockSpace) > 0
			}

			first := make(chan *httptest.ResponseRecorder, 1)
			go func() { first <- link(one, "https://github.com/acme/tools/pull/7") }()
			deadline := time.Now().Add(30 * time.Second)
			for !aHeld() {
				if time.Now().After(deadline) {
					t.Fatal("write A never reached its held insert")
				}
				time.Sleep(20 * time.Millisecond)
			}

			later := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				if second != "create" {
					later <- link(two, c.url)
					return
				}
				later <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
					map[string]string{"project": "CORE", "external": "acme/tools#7"}, "alice")
			}()
			// B either answers while A is held, which is the race, or waits for the lock of the pull
			// request behind A. A B that does neither within 30 s waits on something else, and the
			// test says so rather than letting A go and reading what follows.
			var answered *httptest.ResponseRecorder
			deadline = time.Now().Add(30 * time.Second)
			for answered == nil && !bWaits() {
				select {
				case answered = <-later:
				case <-time.After(20 * time.Millisecond):
				}
				if answered == nil && time.Now().After(deadline) {
					t.Fatal("write B neither answered nor waited for the lock of the pull request while A was held")
				}
			}
			waitedForA := answered == nil
			release()
			a := <-first
			b := answered
			if b == nil {
				b = <-later
			}
			t.Logf("second=%s b_waited_for_a=%v a=%d b=%d b_body=%s", second, waitedForA, a.Code, b.Code, strings.TrimSpace(b.Body.String()))

			if a.Code != http.StatusOK {
				t.Fatalf("write A of /pull/7 to %s: status=%d body=%s, want 200", one, a.Code, a.Body.String())
			}
			var holders []string
			if err := database.Pool.QueryRow(ctx, `
				select coalesce(array_agg(distinct issue_key order by issue_key), '{}') from issue_external_links
			`).Scan(&holders); err != nil {
				t.Fatal(err)
			}
			if len(holders) != 1 || holders[0] != one {
				t.Fatalf("the two forms of acme/tools#7 are held by %v after two writes at once, want %s alone: B's check read no row of A's write in flight", holders, one)
			}
			switch second {
			case "create":
				if b.Code != http.StatusBadRequest || !strings.Contains(b.Body.String(), `"code":"EXTERNAL_PROJECT_MISMATCH"`) || !strings.Contains(b.Body.String(), "project OTHER") {
					t.Fatalf("the create for acme/tools#7: status=%d body=%s, want 400 EXTERNAL_PROJECT_MISMATCH naming project OTHER, the project of %s, which holds it", b.Code, b.Body.String(), one)
				}
			default:
				if b.Code != http.StatusConflict || !strings.Contains(b.Body.String(), `"code":"EXTERNAL_LINK_TAKEN"`) || !strings.Contains(b.Body.String(), one) {
					t.Fatalf("write B of %s to %s: status=%d body=%s, want 409 EXTERNAL_LINK_TAKEN naming %s", c.url, two, b.Code, b.Body.String(), one)
				}
			}
		})
	}
}

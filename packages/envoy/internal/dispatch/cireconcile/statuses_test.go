package cireconcile

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/testwait"
)

// todaysHeadsQuery is the query a pass sent every repository before it read commit statuses,
// written out byte for byte: what a repository whose token holds no Commit statuses is still
// sent (headsQuery).
const todaysHeadsQuery = `query($owner: String!, $name: String!, $after: String) {
  rateLimit { cost }
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, first: 20, after: $after, orderBy: {field: CREATED_AT, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        commits(last: 1) {
          nodes {
            commit {
              oid
              checkSuites(first: 30) {
                pageInfo { hasNextPage }
                nodes {
                  databaseId
                  app { databaseId }
                  status
                  conclusion
                  updatedAt
                  checkRuns(first: 60, filterBy: {checkType: ALL}) {
                    pageInfo { hasNextPage }
                    nodes {
                      databaseId
                      name
                      status
                      conclusion
                      startedAt
                      completedAt
                      permalink
                    }
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`

// The permissions GitHub's mint answers for a token of the App's installation, written out in
// full: one installation that granted the App Commit statuses, and one that did not.
var (
	tokenWithStatuses    = map[string]string{"checks": "read", "contents": "read", "metadata": "read", "pull_requests": "read", "statuses": "read"}
	tokenWithoutStatuses = map[string]string{"checks": "read", "contents": "read", "metadata": "read", "pull_requests": "read"}
)

// headKey is what the pass keeps the harness's one head under (pullHead.key).
const headKey = gitHubRepository + "#" + pullNumber + "@" + headSHA

// commitStatus is one commit status as GitHub's REST listing of a commit's combined status
// answers it: the newest status of one context, with its link and its description.
func commitStatus(id int64, context, state, createdAt string) object {
	return object{
		"id":          id,
		"context":     context,
		"state":       state,
		"created_at":  createdAt,
		"updated_at":  createdAt,
		"target_url":  fmt.Sprintf("https://ci.example.com/acme/widgets/%d", id),
		"description": context + " is " + state,
	}
}

// headWithStatuses maps the harness's repository and gives its one head a completed check run
// under a completed check suite and these commit statuses, and every token the stand-in mints
// these permissions.
func (h *harness) headWithStatuses(permissions map[string]string, statuses ...object) {
	h.t.Helper()
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setTokenPermissions(permissions)
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-17T05:00:00Z"),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "success", otherApp, "2026-11-17T05:00:05Z"),
	)
	h.github.setStatuses(gitHubRepository, headSHA, statuses...)
}

// statesOf is the context and the state of each commit status a record holds.
func statesOf(state *cistore.State) map[string]string {
	states := map[string]string{}
	for context, status := range state.Statuses {
		states[context] = status.State
	}
	return states
}

// settlementNaming is the next settlement of the head that names exactly these statuses,
// passing over every settlement before it: a head read over REST has its check runs and suites
// written before its statuses, so the summary loop may settle it on the first writes and again
// on the second. The wait has no bound of its own (testwait.Until); its failure names the
// statuses the last settlement it read named.
func (h *harness) settlementNaming(sub *nats.Subscription, why string, want []cistore.StatusRef) cistore.Summary {
	h.t.Helper()
	type reading struct{ LastNamed []cistore.StatusRef }
	var last cistore.Summary
	testwait.Until(h.t, fmt.Sprintf("%s: a settlement naming the statuses %v", why, want), func() (reading, bool) {
		waiting, _, err := sub.Pending()
		if err != nil {
			h.t.Fatalf("%s: read the settlements waiting: %v", why, err)
		}
		if waiting == 0 {
			h.driveLoop(h.loopStep())
			return reading{LastNamed: last.Statuses}, false
		}
		last = h.settlement(sub, why)
		return reading{LastNamed: last.Statuses}, slices.Equal(last.Statuses, want)
	})
	return last
}

// requestsFor is every request since index from whose URL names the commit sha, oldest first,
// each with its index in the stand-in's log.
func (h *harness) requestsFor(from int, sha string) []int {
	var found []int
	for index, request := range h.github.received()[from:] {
		if strings.Contains(request.url, sha) {
			found = append(found, from+index)
		}
	}
	return found
}

// readings is the two ways a pass reads a head: in the query, and over REST, which is where a
// head the query could not answer whole is read. Here GitHub refuses the head's Checks fields in
// the query, which sends it there.
var readings = []struct {
	name     string
	overREST bool
}{
	{name: "through the query"},
	{name: "over REST", overREST: true},
}

// R5: a pass records each commit status of a head in success, failure or error and no pending
// one, whichever way it reads the head: in the query, where the token it holds holds Commit
// statuses, and over REST, after the head's check runs and suites, for a head the query could not
// answer whole. A status is an entry of its own: it is no check run, and the head's settlement
// names it in a field of its own.
func TestReconcileRecordsTheTerminalStatusesOfAHead(t *testing.T) {
	for _, read := range readings {
		t.Run(read.name, func(t *testing.T) {
			h := newHarness(t, reviewerAppID)
			h.headWithStatuses(tokenWithStatuses,
				commitStatus(101, "ci/build", "success", "2026-11-17T05:01:00Z"),
				commitStatus(102, "ci/lint", "failure", "2026-11-17T05:02:00Z"),
				commitStatus(103, "ci/license", "error", "2026-11-17T05:03:00Z"),
				commitStatus(104, "ci/deploy", "pending", "2026-11-17T05:04:00Z"),
			)
			if read.overREST {
				h.github.refuseChecks(gitHubRepository, headSHA)
			}
			settlements := h.subscribe()

			h.pass()

			state := h.stateAt(gitHubRepository, 7, headSHA)
			if state == nil {
				t.Fatal("the pass left the head no record")
			}
			want := map[string]string{"ci/build": "success", "ci/lint": "failure", "ci/license": "error"}
			if got := statesOf(state); !maps.Equal(got, want) {
				t.Fatalf("the record holds statuses %v, want %v and no pending one", got, want)
			}
			lint := state.Statuses["ci/lint"]
			if lint.CreatedAt != "2026-11-17T05:02:00Z" || lint.URL != "https://ci.example.com/acme/widgets/102" || lint.Description != "ci/lint is failure" {
				t.Errorf("the record holds ci/lint as %+v, want GitHub's time, link and description", lint)
			}
			if _, held := state.Checks["ci-ok"]; !held || len(state.Checks) != 1 || len(state.Suites) != 1 {
				t.Errorf("the record holds checks %v and suites %v, want ci-ok and its suite alone: a status is no check run", state.Checks, state.Suites)
			}
			h.settlementNaming(settlements, "after a pass over a head with statuses", []cistore.StatusRef{
				{Context: "ci/build", State: "success", CreatedAt: "2026-11-17T05:01:00Z"},
				{Context: "ci/license", State: "error", CreatedAt: "2026-11-17T05:03:00Z"},
				{Context: "ci/lint", State: "failure", CreatedAt: "2026-11-17T05:02:00Z"},
			})

			queries := h.github.queriesAsked()
			if len(queries) != 1 || queries[0].text != headsQueryWithStatuses {
				t.Fatalf("the pass asked %d queries, want the one that asks for the head's statuses", len(queries))
			}
			wantReads := 0
			if read.overREST {
				wantReads = 1
			}
			if sent := h.github.byRoute(); sent[statusRoute] != wantReads || sent[checkRunsRoute] != wantReads {
				t.Fatalf("the pass read the head's statuses %d times and its check runs %d times over REST, want %d of each: %v",
					sent[statusRoute], sent[checkRunsRoute], wantReads, sent)
			}
		})
	}
}

// R6: a pass in which only a status of a head changed records it. Through the query, the head's
// fingerprint takes the status with it, so the fingerprint moves and the head is written again
// however unchanged its check runs and suites are; over REST, the status listing answers anew
// while the check runs and suites answer 304. A changed state settles the head again, and a
// pending status alone moves nothing.
func TestReconcileRecordsAStatusThatAloneChanged(t *testing.T) {
	for _, read := range readings {
		t.Run(read.name, func(t *testing.T) {
			h := newHarness(t, reviewerAppID)
			h.headWithStatuses(tokenWithStatuses, commitStatus(101, "ci/build", "failure", "2026-11-17T05:01:00Z"))
			if read.overREST {
				h.github.refuseChecks(gitHubRepository, headSHA)
			}
			settlements := h.subscribe()

			h.pass()

			h.settlementNaming(settlements, "after the first pass", []cistore.StatusRef{{Context: "ci/build", State: "failure", CreatedAt: "2026-11-17T05:01:00Z"}})
			before := h.reconciler.heads[headKey]
			counted := h.countWrites()
			// The build runs again and passes: the status is all that changed on GitHub.
			h.github.setStatuses(gitHubRepository, headSHA, commitStatus(102, "ci/build", "success", "2026-11-17T05:09:00Z"))
			sentBefore := h.github.count()

			h.pass()

			state := h.stateAt(gitHubRepository, 7, headSHA)
			if state == nil || state.Statuses["ci/build"].State != "success" || state.Statuses["ci/build"].CreatedAt != "2026-11-17T05:09:00Z" {
				t.Fatalf("the record holds %+v after the second pass, want ci/build's newer success", state)
			}
			h.settlementNaming(settlements, "after the status alone changed", []cistore.StatusRef{{Context: "ci/build", State: "success", CreatedAt: "2026-11-17T05:09:00Z"}})
			line := h.passLine()
			if got := number(t, line, "records"); got != 1 {
				t.Errorf("the second pass changed %d records, want the head's: %v", got, line)
			}
			if got := number(t, line, "heads_unchanged"); got != 0 {
				t.Errorf("the second pass counted %d heads unchanged, want none: %v", got, line)
			}
			after := h.reconciler.heads[headKey]
			updates, _ := counted.calls()
			if !read.overREST {
				if before.fingerprint == 0 || after.fingerprint == before.fingerprint {
					t.Fatalf("the head's fingerprint went from %d to %d, want it moved by the status alone", before.fingerprint, after.fingerprint)
				}
				// The fingerprint moved, so the head is written whole: its run, its suite and
				// its status.
				if updates != 3 {
					t.Errorf("the second pass made %d updates, want the head's run, suite and status", updates)
				}

				// A pending context alone moves no fingerprint: the pass records no pending status.
				h.github.setStatuses(gitHubRepository, headSHA,
					commitStatus(102, "ci/build", "success", "2026-11-17T05:09:00Z"),
					commitStatus(103, "ci/deploy", "pending", "2026-11-17T05:10:00Z"),
				)
				counted.forget()

				h.pass()

				if held := h.reconciler.heads[headKey].fingerprint; held != after.fingerprint {
					t.Errorf("a pending status moved the fingerprint from %d to %d", after.fingerprint, held)
				}
				if updates, _ := counted.calls(); updates != 0 {
					t.Errorf("the pass over a new pending status made %d updates, want none", updates)
				}
				return
			}
			if updates != 1 {
				t.Errorf("the second pass made %d updates, want the status alone: the check runs and suites answered 304", updates)
			}
			for _, route := range []string{checkRunsRoute, checkSuitesRoute, statusRoute} {
				var sent []fakeRequest
				for _, request := range h.github.received()[sentBefore:] {
					if request.route == route {
						sent = append(sent, request)
					}
				}
				if len(sent) != 1 || sent[0].ifNoneMatch == "" {
					t.Fatalf("the second pass sent %v to %s, want one read conditional on the first pass's ETag", sent, route)
				}
			}
			if got := number(t, line, "rest_not_modified"); got != 3 {
				t.Errorf("GitHub answered %d of the second pass's REST reads 304, want the repository's, the check runs' and the check suites': %v", got, line)
			}
			if len(before.statuses) != 1 || len(after.statuses) != 1 || before.statuses[0].etag == after.statuses[0].etag {
				t.Errorf("the status listing's ETag went from %v to %v, want the new answer's kept", before.statuses, after.statuses)
			}
		})
	}
}

// R5: a head whose statuses the App cannot read keeps the check runs and check suites the pass
// records for it, and costs GitHub no more requests than the same pass cost before it read
// statuses. GitHub refuses a status read while the token the pass holds names Commit statuses
// when the grant is taken away while that token lives.
func TestReconcileKeepsAHeadsChecksWhenItsStatusesAreRefused(t *testing.T) {
	// A refused status field is no refusal of the head: GitHub answers it null beside an errors
	// entry against its path, and the head is answered whole by its check suites, as a head with
	// no error is. It is not read over REST, so the pass sends what the same pass sends with the
	// query of before. The refusal is named at WARN, as a refused REST status read is.
	t.Run("a refused status field", func(t *testing.T) {
		h := newHarness(t, reviewerAppID)
		h.headWithStatuses(tokenWithStatuses, commitStatus(101, "ci/build", "failure", "2026-11-17T05:01:00Z"))
		h.github.refuseStatuses(gitHubRepository, headSHA)

		h.pass()

		state := h.stateAt(gitHubRepository, 7, headSHA)
		if state == nil {
			t.Fatal("the pass left the head no record")
		}
		if _, held := state.Checks["ci-ok"]; !held || len(state.Suites) != 1 || len(state.Statuses) != 0 {
			t.Fatalf("the record holds checks %v, suites %v and statuses %v; want ci-ok and its suite and no status", state.Checks, state.Suites, state.Statuses)
		}
		if queries := h.github.queriesAsked(); len(queries) != 1 || queries[0].text != headsQueryWithStatuses {
			t.Fatalf("the pass asked %d queries, want the one that asks for statuses", len(queries))
		}
		refused := h.github.byRoute()
		for _, route := range []string{checkRunsRoute, checkSuitesRoute, statusRoute} {
			if refused[route] != 0 {
				t.Fatalf("the pass sent %d requests to %s, want none: a refused status field sends no head over REST: %v", refused[route], route, refused)
			}
		}
		warned := h.lines("ci reconcile read no commit status of a head")
		if len(warned) != 1 {
			t.Fatalf("the pass logged %d lines naming the head's statuses unread, want one:\n%s", len(warned), h.logs.String())
		}
		for field, want := range map[string]any{
			"level":      "WARN",
			"repository": gitHubRepository,
			"pr":         pullNumber,
			"sha":        headSHA,
			"error":      "FORBIDDEN Resource not accessible by integration",
		} {
			if warned[0][field] != want {
				t.Fatalf("the line naming the head's statuses unread has %s %v, want %v: %v", field, warned[0][field], want, warned[0])
			}
		}

		// The same pass with the query of before: a Reconciler of its own, holding nothing of the
		// head, whose token holds no Commit statuses.
		h.github.setTokenPermissions(tokenWithoutStatuses)
		h.github.forgetRequests()
		h.github.forgetQueries()
		h.replica().Pass(h.ctx)

		if queries := h.github.queriesAsked(); len(queries) != 1 || queries[0].text != todaysHeadsQuery {
			t.Fatalf("the pass of before asked %d queries, want the query of before", len(queries))
		}
		if today := h.github.byRoute(); !maps.Equal(refused, today) {
			t.Fatalf("the pass whose status field GitHub refused sent %v, want what the pass of before sends: %v", refused, today)
		}
	})

	// A head the query could not answer whole is read over REST for its checks, and GitHub has
	// already refused its statuses in the query: the REST read asks for none of them.
	t.Run("a refused status field of a head read over REST", func(t *testing.T) {
		h := newHarness(t, reviewerAppID)
		h.headWithStatuses(tokenWithStatuses, commitStatus(101, "ci/build", "failure", "2026-11-17T05:01:00Z"))
		h.github.refuseChecks(gitHubRepository, headSHA)
		h.github.refuseStatuses(gitHubRepository, headSHA)

		h.pass()

		if state := h.stateAt(gitHubRepository, 7, headSHA); state == nil || len(state.Checks) != 1 || len(state.Statuses) != 0 {
			t.Fatalf("the record is %+v, want ci-ok and no status", state)
		}
		refused := h.github.byRoute()
		if refused[checkRunsRoute] != 1 || refused[statusRoute] != 0 {
			t.Fatalf("the pass sent %v, want the head's check runs read over REST and no status read", refused)
		}
		h.github.setTokenPermissions(tokenWithoutStatuses)
		h.github.forgetRequests()
		h.replica().Pass(h.ctx)
		if today := h.github.byRoute(); !maps.Equal(refused, today) {
			t.Fatalf("the pass whose status field GitHub refused sent %v, want what the pass of before sends: %v", refused, today)
		}
	})

	// A head read over REST whose status read GitHub answers 403 keeps the check runs and suites
	// the pass recorded before that read, and the ETags of both: the read costs the head its
	// statuses alone, and no request for the head follows it. The head beside it reads its own.
	t.Run("a refused REST status read", func(t *testing.T) {
		h := newHarness(t, reviewerAppID)
		h.mapRepository(rowRepository)
		refused, read := head(7), head(8)
		h.listHeads(refused, read)
		h.github.setTokenPermissions(tokenWithStatuses)
		for _, pull := range []fakePull{refused, read} {
			h.github.setCheckRuns(gitHubRepository, pull.sha,
				atHead(gitHubRepository, pull.sha, checkRun(10+pull.number, "ci-ok", "completed", "success", otherApp, "2026-11-17T05:00:00Z")),
			)
			h.github.setCheckSuites(gitHubRepository, pull.sha,
				atHead(gitHubRepository, pull.sha, checkSuite(900, "completed", "success", otherApp, "2026-11-17T05:00:05Z")),
			)
			h.github.setStatuses(gitHubRepository, pull.sha, commitStatus(100+pull.number, "ci/build", "failure", "2026-11-17T05:01:00Z"))
		}
		// GitHub refuses the query for the repository's page, so both heads are read over REST.
		h.github.refusePage(gitHubRepository, 0)
		h.github.refuseStatuses(gitHubRepository, refused.sha)

		h.pass()

		state := h.recordOf(refused, refused.number)
		if state == nil {
			t.Fatal("the head whose status read GitHub refused has no record")
		}
		if _, held := state.Checks["ci-ok"]; !held || len(state.Suites) != 1 || len(state.Statuses) != 0 {
			t.Fatalf("the head whose status read GitHub refused holds checks %v, suites %v and statuses %v; want ci-ok and its suite and no status",
				state.Checks, state.Suites, state.Statuses)
		}
		if other := h.recordOf(read, read.number); other == nil || statesOf(other)["ci/build"] != "failure" {
			t.Fatalf("the head beside it holds %+v, want its own status recorded", other)
		}
		touched := h.requestsFor(0, refused.sha)
		received := h.github.received()
		if len(touched) != 3 || received[touched[2]].route != statusRoute {
			t.Fatalf("the pass sent %d requests for the refused head, want its check runs, its check suites and then its statuses", len(touched))
		}
		if !h.logged("ci reconcile read no commit status of a head") || h.logged("ci reconcile did not read a head") {
			t.Fatalf("the pass logged %s; want the head's statuses named unread and the head itself read", h.logs.String())
		}
		if got := number(t, h.passLine(), "heads"); got != 2 {
			t.Fatalf("the pass read %d heads, want both", got)
		}

		// The refused head kept the ETags of its check runs and suites, which no failed read of
		// the head would: the next pass reads both conditionally.
		sentBefore := h.github.count()

		h.pass()

		for _, path := range []string{runsOf(refused), suitesOf(refused)} {
			sent := h.github.sincePath(sentBefore, path)
			if len(sent) != 1 || sent[0].ifNoneMatch == "" {
				t.Fatalf("the next pass sent %v to %s, want one read conditional on the ETag the refused head kept", sent, path)
			}
		}
	})

	// A rate limit GitHub answers to a head's status read stops the pass there, as at any read.
	// The head's check runs and suites, read and recorded before it, stay recorded, but the head
	// was not read whole: it is not counted among the heads the pass read, the cursor names it,
	// and the pass after the limit's reset reads it again and records its statuses.
	t.Run("a rate-limited REST status read", func(t *testing.T) {
		h := newHarness(t, reviewerAppID)
		h.headWithStatuses(tokenWithStatuses, commitStatus(101, "ci/build", "failure", "2026-11-17T05:01:00Z"))
		h.github.refuseChecks(gitHubRepository, headSHA)
		h.github.rateLimitNext("/repos/" + gitHubRepository + "/commits/" + headSHA + "/status")

		h.pass()

		line := h.passLine()
		if line["rate_limited"] != true {
			t.Fatalf("the pass reports rate_limited = %v, want true: GitHub answered the head's status read a rate limit: %v", line["rate_limited"], line)
		}
		if got := number(t, line, "heads"); got != 0 {
			t.Errorf("the pass read %d heads, want none: the limit cut the head's read short: %v", got, line)
		}
		if resume := h.reconciler.resume; resume.repository != rowRepository || resume.number != 7 {
			t.Errorf("the cursor is %+v, want pull request 7 of %s, the head the limit cut short", resume, rowRepository)
		}
		state := h.stateAt(gitHubRepository, 7, headSHA)
		if state == nil {
			t.Fatal("the head whose status read GitHub rate-limited has no record")
		}
		if _, held := state.Checks["ci-ok"]; !held || len(state.Suites) != 1 || len(state.Statuses) != 0 {
			t.Fatalf("the record holds checks %v, suites %v and statuses %v; want ci-ok and its suite and no status yet", state.Checks, state.Suites, state.Statuses)
		}

		// GitHub's reset passes, which takes the limit out of the ledger every Dispatch process
		// shares, and the pass after it takes the cycle up at the head the limit cut short.
		scope := githubapp.InstallationScope(h.github.installationID())
		limit, found, err := h.reconciler.Limits.Get(h.ctx, scope)
		if err != nil || !found {
			t.Fatalf("the limit GitHub answered with was recorded under %s: %v, %v", scope, found, err)
		}
		if err := h.reconciler.Limits.Clear(h.ctx, limit); err != nil {
			t.Fatalf("clear the limit as its reset does: %v", err)
		}

		h.pass()

		if state := h.stateAt(gitHubRepository, 7, headSHA); state == nil || statesOf(state)["ci/build"] != "failure" {
			t.Fatalf("after the reset the record is %+v, want ci/build's failure recorded", state)
		}
		line = h.passLine()
		if line["rate_limited"] != false {
			t.Fatalf("the pass after the reset reports rate_limited = %v, want false: %v", line["rate_limited"], line)
		}
		if got := number(t, line, "heads"); got != 1 {
			t.Errorf("the pass after the reset read %d heads, want the one the limit cut short: %v", got, line)
		}
	})
}

// An errors entry against a head commit's status field, or against a field under it, costs the
// head its statuses alone (refusedPulls): the entry is kept for the node, to be named at WARN. An
// entry against any other field of the node refuses the head's Checks, a status field of a check
// suite included, and an entry that names no pull request node refuses the page whole.
func TestARefusedStatusFieldIsToldFromTheRestOfAHead(t *testing.T) {
	const commit = `"repository","pullRequests","nodes",3,"commits","nodes",0,"commit"`
	for _, c := range []struct {
		name                     string
		path                     string
		refused, statuses, whole bool
	}{
		{name: "the status field", path: `[` + commit + `,"status"]`, statuses: true},
		{name: "a field under the status field", path: `[` + commit + `,"status","contexts",0,"targetUrl"]`, statuses: true},
		{name: "the check suites", path: `[` + commit + `,"checkSuites"]`, refused: true},
		{name: "a check suite's status", path: `[` + commit + `,"checkSuites","nodes",0,"status"]`, refused: true},
		{name: "no path", whole: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			text := `{"type":"FORBIDDEN","message":"Resource not accessible by integration"}`
			if c.path != "" {
				text = `{"type":"FORBIDDEN","path":` + c.path + `,"message":"Resource not accessible by integration"}`
			}
			var entry graphQLError
			if err := json.Unmarshal([]byte(text), &entry); err != nil {
				t.Fatalf("decode the errors entry: %v", err)
			}
			wantRefused := map[int]struct{}{}
			if c.refused {
				wantRefused[3] = struct{}{}
			}
			var wantStatuses map[int][]graphQLError
			if c.statuses {
				wantStatuses = map[int][]graphQLError{3: {entry}}
			}
			refused, statuses, whole := refusedPulls([]graphQLError{entry})
			if !maps.Equal(refused, wantRefused) || !reflect.DeepEqual(statuses, wantStatuses) || whole != c.whole {
				t.Fatalf("an entry against %s: refused %v, statuses %v, whole %v; want %v, %v, %v",
					c.name, refused, statuses, whole, wantRefused, wantStatuses, c.whole)
			}
		})
	}
}

// R5: where the token the pass holds names no Commit statuses, the pass asks GitHub for no status
// at all: each repository is sent the query of before, byte for byte, no status listing is read
// over REST, and every route is sent what it was sent before the pass read statuses.
func TestReconcileReadsNoStatusWhereTheInstallationHoldsNone(t *testing.T) {
	if headsQuery != todaysHeadsQuery {
		t.Fatalf("the query a repository without Commit statuses is sent is no longer the query of before:\n%s", headsQuery)
	}
	for _, read := range readings {
		t.Run(read.name, func(t *testing.T) {
			h := newHarness(t, reviewerAppID)
			// GitHub holds statuses of the head, which the token may not read.
			h.headWithStatuses(tokenWithoutStatuses,
				commitStatus(101, "ci/build", "success", "2026-11-17T05:01:00Z"),
				commitStatus(102, "ci/lint", "failure", "2026-11-17T05:02:00Z"),
			)
			want := map[string]int{
				installationRoute: 1,
				tokenRoute:        1,
				repositoryRoute:   1,
				graphQLRoute:      1,
			}
			if read.overREST {
				h.github.refuseChecks(gitHubRepository, headSHA)
				want[checkRunsRoute] = 1
				want[checkSuitesRoute] = 1
			}

			h.pass()

			if queries := h.github.queriesAsked(); len(queries) != 1 || queries[0].text != todaysHeadsQuery {
				t.Fatalf("the pass asked %d queries, want the query of before, byte for byte", len(queries))
			}
			if sent := h.github.byRoute(); !maps.Equal(sent, want) {
				t.Fatalf("the pass sent %v, want what it sent before it read statuses: %v", sent, want)
			}
			state := h.stateAt(gitHubRepository, 7, headSHA)
			if state == nil {
				t.Fatal("the pass left the head no record")
			}
			if _, held := state.Checks["ci-ok"]; !held || len(state.Statuses) != 0 {
				t.Fatalf("the record holds checks %v and statuses %v, want ci-ok and no status", state.Checks, state.Statuses)
			}
			if h.logged("commit status") {
				t.Fatalf("the pass logged about commit statuses it never asked for:\n%s", h.logs.String())
			}
		})
	}
}

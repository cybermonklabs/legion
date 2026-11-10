package cireconcile

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The GraphQL half of the GitHub stand-in: it answers the pass's one query (headsQuery) out of
// the same open pull requests, check runs and check suites the REST half answers, so one test
// says what GitHub holds once and both reads of it agree.
//
// The answer is built as GitHub builds it rather than as the pass wants it. A commit's check
// runs hang under the check suite that created them, a suite's app is the app of its runs, a
// status and a conclusion are upper-case enums and a conclusion a run does not have yet is
// null, the connections page at the sizes the query asks for and say whether a page follows,
// and a query with no `filterBy: {checkType: ALL}` answers the latest run of each name alone,
// which is GitHub's own default and the thing this pass must never read by.

// fakeQuery is one GraphQL query the stand-in was asked: the repository it named, the page
// cursor it carried, the page that cursor names, and when the stand-in began and finished
// answering it. A test reads these back to say which pages a pass asked for, in what order,
// and how much of GitHub's own time the pass took.
type fakeQuery struct {
	repository string
	after      string
	page       int
	started    time.Time
	ended      time.Time
}

// graphQLLimit is one rate-limited answer the stand-in gives the next query, as one of the
// shapes GitHub answers a spent GraphQL budget with.
type graphQLLimit int

const (
	// limitByErrorEntry is GitHub's primary GraphQL limit: HTTP 200, an errors entry of type
	// RATE_LIMITED, a null repository, x-ratelimit-remaining 0 and x-ratelimit-reset.
	limitByErrorEntry graphQLLimit = iota
	// limitBySecondaryMessage is GitHub's secondary limit at its barest: HTTP 200 and a message
	// about a secondary rate limit, with no errors type and no rate-limit header at all.
	limitBySecondaryMessage
)

// graphQLReset is how long the stand-in's rate-limited answers ask the App to wait.
const graphQLReset = 90 * time.Second

// answerQuery answers POST /graphql: one page of a repository's open pull requests with the
// check suites and check runs of each one's head commit.
func (f *fakeGitHub) answerQuery(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		f.t.Errorf("decode the GraphQL request: %v", err)
		http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
		return
	}
	owner, _ := request.Variables["owner"].(string)
	name, _ := request.Variables["name"].(string)
	after, _ := request.Variables["after"].(string)
	repository := owner + "/" + name

	f.mu.Lock()
	page := 0
	if after != "" {
		if known, held := f.cursorPages[after]; held {
			// A cursor out of an answer this stand-in gave verbatim, which is GitHub's own
			// opaque text rather than a page number (answerQueryWith).
			page = known
		} else {
			number, err := strconv.Atoi(after)
			if err != nil {
				f.mu.Unlock()
				f.t.Errorf("the pass asked for the page after %q, which is no cursor this stand-in gave", after)
				http.Error(w, `{"message":"bad cursor"}`, http.StatusBadRequest)
				return
			}
			page = number
		}
	}
	f.queries = append(f.queries, fakeQuery{repository: repository, after: after, page: page, started: time.Now()})
	asked := len(f.queries) - 1
	f.inFlight++
	f.mostInFlight = max(f.mostInFlight, f.inFlight)
	defer func() {
		f.mu.Lock()
		f.queries[asked].ended = time.Now()
		f.inFlight--
		f.mu.Unlock()
	}()
	verbatim := f.verbatim[repository]
	delete(f.verbatim, repository)
	var limit *graphQLLimit
	if len(f.graphQLLimits) > 0 {
		limit, f.graphQLLimits = &f.graphQLLimits[0], f.graphQLLimits[1:]
	}
	stall := f.graphQLStalls[queryKey(repository, page)]
	takes := f.queryTakes
	pulls := append([]fakePull(nil), f.pulls[repository]...)
	refusedPage := false
	if pages, held := f.refusedPages[repository]; held {
		_, refusedPage = pages[page]
	}
	f.mu.Unlock()

	if limit != nil {
		f.answerQueryLimit(w, *limit)
		return
	}
	for _, wait := range []time.Duration{stall, takes} {
		if wait <= 0 {
			continue
		}
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
	}
	if verbatim != "" {
		// GitHub's own answer, captured from the real API and given back byte for byte.
		if _, err := w.Write([]byte(verbatim)); err != nil {
			f.t.Errorf("write the captured answer: %v", err)
		}
		return
	}
	if refusedPage {
		// GitHub refusing the whole query: the repository comes back null beside an errors
		// entry about it, which is a page the pass cannot read a head of and has no cursor to
		// read past.
		f.encode(w, map[string]any{
			"data": map[string]any{"rateLimit": rateLimitNode(), "repository": nil},
			"errors": []any{map[string]any{
				"type":    "FORBIDDEN",
				"path":    []any{"repository"},
				"message": "Resource not accessible by integration",
			}},
		})
		return
	}

	slices.SortFunc(pulls, func(a, b fakePull) int { return int(a.number - b.number) })
	from := min(page*pullsPerQuery, len(pulls))
	to := min(from+pullsPerQuery, len(pulls))
	all := strings.Contains(request.Query, "checkType: ALL")
	nodes := make([]any, 0, to-from)
	var errors []any
	for index, pull := range pulls[from:to] {
		node := map[string]any{"number": pull.number}
		commit := map[string]any{"oid": pull.sha}
		if f.checksRefused(repository, pull.sha) {
			// GitHub refusing one head's Checks: the field is null and an errors entry names
			// its path, the rest of the page standing.
			commit["checkSuites"] = nil
			errors = append(errors, map[string]any{
				"type":    "FORBIDDEN",
				"path":    []any{"repository", "pullRequests", "nodes", index, "commits", "nodes", 0, "commit", "checkSuites"},
				"message": "Resource not accessible by integration",
			})
		} else {
			commit["checkSuites"] = f.checkSuiteNodes(repository, pull.sha, all)
		}
		node["commits"] = map[string]any{"nodes": []any{map[string]any{"commit": commit}}}
		nodes = append(nodes, node)
	}
	answer := map[string]any{"data": map[string]any{
		"rateLimit": rateLimitNode(),
		"repository": map[string]any{"pullRequests": map[string]any{
			"pageInfo": map[string]any{
				"hasNextPage": to < len(pulls),
				"endCursor":   strconv.Itoa(page + 1),
			},
			"nodes": nodes,
		}},
	}}
	if len(errors) > 0 {
		answer["errors"] = errors
	}
	f.encode(w, answer)
}

// answerQueryLimit writes one of GitHub's rate-limited answers to a query, each of which is an
// HTTP 200 (githubapp.GraphQLRateLimit reads them, not the status).
func (f *fakeGitHub) answerQueryLimit(w http.ResponseWriter, limit graphQLLimit) {
	switch limit {
	case limitBySecondaryMessage:
		f.encode(w, map[string]any{
			"data":   nil,
			"errors": []any{map[string]any{"message": "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}},
		})
	default:
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(graphQLReset).Unix(), 10))
		f.encode(w, map[string]any{
			"data": map[string]any{"rateLimit": rateLimitNode(), "repository": nil},
			"errors": []any{map[string]any{
				"type":    "RATE_LIMITED",
				"message": "API rate limit exceeded for installation",
			}},
		})
	}
}

// rateLimitNode is the query's own rateLimit field: one point a query, which is what GitHub
// charges a page of this size.
func rateLimitNode() map[string]any {
	return map[string]any{"cost": 1, "remaining": 4999, "resetAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
}

// checkSuiteNodes is one commit's checkSuites connection: the suites a test declared, each
// carrying the runs it created, and a queued suite of its own for every run whose suite no test
// declared -- GitHub has a suite behind every run, and a queued one is recorded by nothing, so
// the pass sees exactly the suites the REST listing of the same commit answers with.
//
// all is whether the query asked for `filterBy: {checkType: ALL}`. Without it GitHub answers the
// latest run of each name alone, and so does this.
func (f *fakeGitHub) checkSuiteNodes(repository, sha string, all bool) map[string]any {
	f.mu.Lock()
	runs := append([]object(nil), f.runs[repository+"@"+sha]...)
	declared := append([]object(nil), f.suites[repository+"@"+sha]...)
	f.mu.Unlock()
	if !all {
		runs = latestOfEachName(runs)
	}

	// The suites in the order GitHub would answer them: the declared ones, then one for each
	// group of runs no declared suite claimed.
	type group struct {
		suite object
		runs  []object
	}
	groups := make([]*group, 0, len(declared))
	byKey := map[string]*group{}
	for _, suite := range declared {
		held := &group{suite: suite}
		groups = append(groups, held)
		byKey[suiteKey(idText(suite["id"]), appText(suite))] = held
	}
	for _, run := range runs {
		key := suiteKey(idText(nested(run, "check_suite")["id"]), appText(run))
		held, found := byKey[key]
		if !found {
			held = &group{suite: object{
				"id":         nested(run, "check_suite")["id"],
				"status":     "queued",
				"conclusion": "",
				"app":        object{"id": nested(run, "app")["id"]},
				"updated_at": "",
			}}
			groups = append(groups, held)
			byKey[key] = held
		}
		held.runs = append(held.runs, run)
	}

	nodes := make([]any, 0, min(len(groups), suitesPerQuery))
	for _, held := range groups[:min(len(groups), suitesPerQuery)] {
		kept := held.runs[:min(len(held.runs), runsPerQuery)]
		runNodes := make([]any, 0, len(kept))
		for _, run := range kept {
			runNodes = append(runNodes, map[string]any{
				"databaseId":  run["id"],
				"name":        run["name"],
				"status":      enum(run["status"]),
				"conclusion":  enum(run["conclusion"]),
				"startedAt":   nullable(run["started_at"]),
				"completedAt": nullable(run["completed_at"]),
				"permalink":   run["html_url"],
			})
		}
		nodes = append(nodes, map[string]any{
			"databaseId": held.suite["id"],
			"app":        map[string]any{"databaseId": nested(held.suite, "app")["id"]},
			"status":     enum(held.suite["status"]),
			"conclusion": enum(held.suite["conclusion"]),
			"updatedAt":  nullable(held.suite["updated_at"]),
			"checkRuns": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": len(held.runs) > runsPerQuery},
				"nodes":    runNodes,
			},
		})
	}
	return map[string]any{
		"pageInfo": map[string]any{"hasNextPage": len(groups) > suitesPerQuery},
		"nodes":    nodes,
	}
}

// latestOfEachName is the runs GitHub answers a check-runs connection with when the query does
// not ask for every check type: the newest run of each name, which is the one with the largest
// id, and every earlier attempt of that name dropped.
func latestOfEachName(runs []object) []object {
	latest := map[string]object{}
	for _, run := range runs {
		name, _ := run["name"].(string)
		held, found := latest[name]
		if !found || idOf(run) > idOf(held) {
			latest[name] = run
		}
	}
	kept := make([]object, 0, len(latest))
	for _, run := range runs {
		name, _ := run["name"].(string)
		if latest[name] != nil && idOf(latest[name]) == idOf(run) {
			kept = append(kept, run)
		}
	}
	return kept
}

func idOf(item object) int64 {
	switch id := item["id"].(type) {
	case int64:
		return id
	case int:
		return int64(id)
	case float64:
		return int64(id)
	}
	return 0
}

func idText(value any) string { return fmt.Sprintf("%v", value) }

func appText(item object) string { return idText(nested(item, "app")["id"]) }

func suiteKey(id, app string) string { return id + "/" + app }

// nested is one object field of an object, and an empty object when it holds none, so a caller
// may read through it without a check of its own.
func nested(item object, key string) object {
	if held, ok := item[key].(object); ok {
		return held
	}
	if held, ok := item[key].(map[string]any); ok {
		return held
	}
	return object{}
}

// enum is a webhook's lower-case status or conclusion as the GraphQL enum GitHub answers with,
// and null for the empty one: a run that has not finished has no conclusion at all.
func enum(value any) any {
	text, _ := value.(string)
	if text == "" {
		return nil
	}
	return strings.ToUpper(text)
}

// nullable is a timestamp as GraphQL answers it: the text, or null where the webhook's own
// field is absent or empty.
func nullable(value any) any {
	text, _ := value.(string)
	if text == "" {
		return nil
	}
	return text
}

// queryKey names one page of one repository, which is what a stall is held under.
func queryKey(repository string, page int) string {
	return repository + "#" + strconv.Itoa(page)
}

// checksRefused reports whether the stand-in refuses one head's Checks fields.
func (f *fakeGitHub) checksRefused(repository, sha string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, refused := f.refusedChecks[repository+"@"+sha]
	return refused
}

// refuseChecks makes every query answer one head's Checks fields with null and an errors entry
// against that head's own path, which is a repository whose installation may read its pull
// requests and not its checks.
func (f *fakeGitHub) refuseChecks(repository, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusedChecks[repository+"@"+sha] = struct{}{}
}

// refusePage makes every query for one page of one repository answer a null repository and an
// errors entry about it, which is GitHub refusing the query whole.
func (f *fakeGitHub) refusePage(repository string, page int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refusedPages[repository] == nil {
		f.refusedPages[repository] = map[int]struct{}{}
	}
	f.refusedPages[repository][page] = struct{}{}
}

// allowPage undoes refusePage for one page.
func (f *fakeGitHub) allowPage(repository string, page int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.refusedPages[repository], page)
}

// rateLimitQuery makes the next query answer as GitHub answers a spent GraphQL budget, in the
// shape named.
func (f *fakeGitHub) rateLimitQuery(limit graphQLLimit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.graphQLLimits = append(f.graphQLLimits, limit)
}

// stallQuery holds back the answer to one page of one repository, as stall does for a REST
// path: a wait of zero answers it as fast as every other page again.
func (f *fakeGitHub) stallQuery(repository string, page int, wait time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if wait <= 0 {
		delete(f.graphQLStalls, queryKey(repository, page))
		return
	}
	f.graphQLStalls[queryKey(repository, page)] = wait
}

// everyQueryTakes makes every query's answer take this long, which is what a test of the pass's
// GraphQL pacing measures the pass's own waits against.
func (f *fakeGitHub) everyQueryTakes(wait time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queryTakes = wait
}

// queriesAsked is every query the stand-in was asked, oldest first.
func (f *fakeGitHub) queriesAsked() []fakeQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeQuery(nil), f.queries...)
}

// forgetQueries empties the query log, so what a test asserts on afterwards is what the passes
// after it asked and nothing else.
func (f *fakeGitHub) forgetQueries() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = nil
}

// mostQueriesAtOnce is the most queries the stand-in was answering at one moment, which is one
// for a pass that queries serially as GitHub asks a poller to.
func (f *fakeGitHub) mostQueriesAtOnce() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mostInFlight
}

// answerQueryWith makes the next query about one repository answer with body byte for byte: an
// answer captured from GitHub itself with `gh api graphql`, so the pass's mapping is tested
// against what GitHub really said rather than against this stand-in's idea of it. The page
// cursor that answer carries is remembered as naming the page after it, so the pass may read on
// past it as it would past any page.
func (f *fakeGitHub) answerQueryWith(repository, body string) {
	f.t.Helper()
	var answer graphQLAnswer
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		f.t.Fatalf("the captured answer is not GitHub's envelope: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.verbatim == nil {
		f.verbatim = map[string]string{}
	}
	f.verbatim[repository] = body
	if answer.Data.Repository != nil {
		if cursor := answer.Data.Repository.PullRequests.PageInfo.EndCursor; cursor != "" {
			f.cursorPages[cursor] = 1
		}
	}
}

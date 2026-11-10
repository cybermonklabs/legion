package cireconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// pullsPerQuery is the open pull requests one query reads. Twenty heads, each with up to
	// thirty check suites and sixty check runs in each of them, is the page size that keeps one
	// answer inside githubapp's 1 MiB response limit for the repositories Dispatch maps: the
	// largest head Legion has run carries 32 runs over 7 suites, and twenty of those is about
	// 200 KiB of JSON. A page that does come back past the limit is read over REST instead
	// (Reconciler.restOfRepository), so the size is a budget rather than a promise.
	pullsPerQuery = 20

	// suitesPerQuery and runsPerQuery are the check suites of one head, and the check runs of
	// one suite, that one query reads. A head with more suites than this, or one suite with more
	// runs than this, is read over REST instead: GraphQL would page those connections with
	// cursors of their own, one query for each page of each suite of each head, which is the
	// per-head round trip this change exists to be rid of. GitHub caps either `first` at 100.
	suitesPerQuery = 30
	runsPerQuery   = 60

	// GraphQLTimePerMinute is the GraphQL processing time the pass allows itself in a minute.
	// GitHub's secondary rate limit for the GraphQL API is 60 seconds of processing time a
	// minute for the whole App, and this pass is not the only query the App pays for: the
	// delivery reconcile's merged-pull-request search runs on the same budget. The pass waits
	// after each query so its own queries never take more than this of any minute
	// (graphQLWait).
	GraphQLTimePerMinute = 45 * time.Second
)

// headsQuery reads one page of a repository's open pull requests with every check suite and
// check run of each one's head commit: the whole of what the REST pass read with two requests a
// head, in one request for twenty heads.
//
// Four things in it are load-bearing.
//
// `filterBy: {checkType: ALL}` is required. GitHub's default for a commit's check runs is
// LATEST, which answers only the newest run of each name and drops every earlier attempt -- on
// one measured Legion head, 29 of the 32 runs. Those attempts are exactly what this pass exists
// to recover: the listener's record holds a run GitHub later re-ran, and a settlement waits on
// the attempt whose completion webhook never arrived. The REST listing is read with
// `filter=all` for the same reason (githubapp.Client.CommitCheckRuns).
//
// `commits(last: 1)` is the head commit. A pull request's commits are ordered oldest first, so
// the last of them is the head GitHub's own REST `head.sha` names, and asking for one keeps the
// answer to the commit whose checks the record is keyed by.
//
// `orderBy: {field: CREATED_AT, direction: ASC}` fixes the order the pass walks a repository's
// heads in. GitHub's PullRequestOrder has no field for the number, and it does not need one:
// a repository's pull request numbers are handed out in the order the pull requests are opened,
// so created-at ascending is number ascending. The cursor a cut-short pass leaves names a page
// and a pull request number within it (cursor), which only a fixed order makes meaningful.
//
// `rateLimit` costs nothing and is charged nothing, and is read in the same answer rather than
// in a query of its own: the pass's line reports the points its queries spent, and a point count
// read a query later would be a different query's.
//
// The three page sizes are the constants above, written into the document once each by
// Sprintf: a GraphQL document takes its `first` as a literal, and a literal spelled out here
// beside the constant the pass reads its own answers by is a literal that can disagree with it.
var headsQuery = fmt.Sprintf(`query($owner: String!, $name: String!, $after: String) {
  rateLimit { cost remaining resetAt }
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, first: %d, after: $after, orderBy: {field: CREATED_AT, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        commits(last: 1) {
          nodes {
            commit {
              oid
              checkSuites(first: %d) {
                pageInfo { hasNextPage }
                nodes {
                  databaseId
                  app { databaseId }
                  status
                  conclusion
                  updatedAt
                  checkRuns(first: %d, filterBy: {checkType: ALL}) {
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
}`, pullsPerQuery, suitesPerQuery, runsPerQuery)

// queryPage is one answer to headsQuery as the pass reads it: the heads of one page of open pull
// requests, in the order the pass takes them, and where the page after it begins.
type queryPage struct {
	heads       []queryHead
	hasNextPage bool
	endCursor   string
	// points is what GitHub charged the query against the App's GraphQL budget (rateLimit.cost).
	points int
}

// queryHead is one open pull request's head as the query answered it: its number, its head
// commit, and the check objects of that commit mapped into the shape the webhook's own body
// carries (checkRunObject, checkSuiteObject).
//
// overflow says the query could not answer this head whole, so the pass reads it over REST
// instead and the objects here are not recorded: the head has more check suites than one query
// asks for, one of its suites has more check runs than one query asks for, or GitHub refused or
// nulled its Checks fields. A head read over REST is read exactly as every head was read before
// this change -- paged, conditional on the ETags the pass kept -- so nothing is lost by it but
// the round trips.
type queryHead struct {
	number   int64
	sha      string
	runs     []object
	suites   []object
	overflow bool
	// why names the overflow in the pass's WARN, so a head read over REST at every pass says
	// which of the three it is.
	why string
}

// graphQLAnswer is GitHub's `{data, errors}` envelope for headsQuery. Every connection is a
// pointer, since GitHub answers a field it refuses with null beside an entry in errors, and a
// null the pass read as an empty list would be a head whose checks it silently recorded none of.
type graphQLAnswer struct {
	Data struct {
		RateLimit struct {
			Cost      int    `json:"cost"`
			Remaining int    `json:"remaining"`
			ResetAt   string `json:"resetAt"`
		} `json:"rateLimit"`
		Repository *struct {
			PullRequests struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []*graphQLPull `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	} `json:"data"`
	Errors []graphQLError `json:"errors"`
}

// graphQLError is one entry of GitHub's errors array. Path names the field the entry is about,
// which is how a refusal of one head's Checks is told from a refusal of the whole query
// (refusedPulls).
type graphQLError struct {
	Type    string `json:"type"`
	Path    []any  `json:"path"`
	Message string `json:"message"`
}

type graphQLPull struct {
	Number  int64 `json:"number"`
	Commits struct {
		Nodes []struct {
			Commit *graphQLCommit `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

type graphQLCommit struct {
	OID         string `json:"oid"`
	CheckSuites *struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []graphQLSuite `json:"nodes"`
	} `json:"checkSuites"`
}

type graphQLSuite struct {
	DatabaseID int64 `json:"databaseId"`
	App        *struct {
		DatabaseID int64 `json:"databaseId"`
	} `json:"app"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	UpdatedAt  string `json:"updatedAt"`
	CheckRuns  *struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []graphQLRun `json:"nodes"`
	} `json:"checkRuns"`
}

type graphQLRun struct {
	DatabaseID  int64  `json:"databaseId"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt"`
	Permalink   string `json:"permalink"`
}

// query reads one page of a repository's open heads. after is empty for the first page and the
// end cursor of the page before it for every other.
//
// It waits before the query rather than after it, as the REST pace does, so a pass of one query
// waits for nothing and the time a query takes is paid for before the query that follows it
// (graphQLWait).
func (r *Reconciler) query(ctx context.Context, repository reconciled, after string, c *counts) (queryPage, error) {
	r.graphQLPace(ctx, c)
	variables := map[string]any{"owner": repository.owner, "name": repository.repo}
	if after != "" {
		variables["after"] = after
	}
	started := time.Now()
	body, status, _, err := r.GitHub.GraphQL(ctx, repository.token, headsQuery, variables)
	c.graphQLTook = time.Since(started)
	c.queries++
	if err != nil {
		return queryPage{}, err
	}
	if status != http.StatusOK {
		return queryPage{}, fmt.Errorf("the GraphQL query for %s/%s answered status %d: %s", repository.owner, repository.repo, status, body)
	}
	var answer graphQLAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return queryPage{}, fmt.Errorf("decode the GraphQL answer for %s/%s: %w", repository.owner, repository.repo, err)
	}
	c.points += answer.Data.RateLimit.Cost
	page := queryPage{points: answer.Data.RateLimit.Cost}
	refused, wholePage := refusedPulls(answer.Errors)
	if wholePage || answer.Data.Repository == nil {
		return page, fmt.Errorf("GitHub refused the GraphQL query for %s/%s: %s", repository.owner, repository.repo, errorText(answer.Errors))
	}
	page.hasNextPage = answer.Data.Repository.PullRequests.PageInfo.HasNextPage
	page.endCursor = answer.Data.Repository.PullRequests.PageInfo.EndCursor
	for index, pull := range answer.Data.Repository.PullRequests.Nodes {
		_, pullRefused := refused[index]
		if pull == nil || (pullRefused && headCommit(pull) == nil) {
			// GitHub refused the node itself, or the head commit under it, rather than the
			// Checks fields beneath that: the answer then holds neither the pull request's
			// number nor its head commit, so nothing here can read that head and nothing can
			// name it to read over REST on its own. This page is a page the query could not
			// answer, exactly as a refusal of the repository is, and its heads are read over
			// REST from the listing, which answers every number and every head commit
			// (restOfRepository). The alternative is the one this replaced: a head no read of
			// either kind ever reaches and no line ever names.
			return page, fmt.Errorf("GitHub answered no readable pull request at node %d of the page for %s/%s: %s",
				index, repository.owner, repository.repo, errorText(answer.Errors))
		}
		page.heads = append(page.heads, headOf(pull, pullRefused))
	}
	return page, nil
}

// headCommit is the head commit of one answered pull request, and nil where the answer holds
// none: `commits(last: 1)` answered empty, or the commit under it refused and nulled.
func headCommit(pull *graphQLPull) *graphQLCommit {
	if len(pull.Commits.Nodes) == 0 {
		return nil
	}
	return pull.Commits.Nodes[0].Commit
}

// headOf is one answered pull request as the pass takes it: its head commit, the check objects
// of that commit, and whether the query could answer it whole. refused says GitHub answered an
// errors entry against this pull request's own path, which is its Checks fields refused.
//
// A pull request GitHub listed with no head commit is no head to read, exactly as one the REST
// listing answered with no head.sha is: it is answered with an empty sha and the caller passes
// over it. A refused pull request with no head commit never reaches here: the query cannot
// answer that head at all, so its whole page is read over REST.
func headOf(pull *graphQLPull, refused bool) queryHead {
	head := queryHead{number: pull.Number}
	commit := headCommit(pull)
	if commit == nil {
		return head
	}
	head.sha = commit.OID
	if refused {
		head.overflow, head.why = true, "GitHub refused the head's Checks fields"
		return head
	}
	if commit.CheckSuites == nil {
		head.overflow, head.why = true, "GitHub answered the head's check suites null"
		return head
	}
	if commit.CheckSuites.PageInfo.HasNextPage {
		head.overflow, head.why = true, fmt.Sprintf("the head has more than %d check suites", suitesPerQuery)
		return head
	}
	runs := 0
	for _, suite := range commit.CheckSuites.Nodes {
		if suite.CheckRuns == nil {
			head.overflow, head.why = true, "GitHub answered a check suite's runs null"
			return head
		}
		if suite.CheckRuns.PageInfo.HasNextPage {
			head.overflow, head.why = true, fmt.Sprintf("a check suite of the head has more than %d check runs", runsPerQuery)
			return head
		}
		runs += len(suite.CheckRuns.Nodes)
	}
	if len(commit.CheckSuites.Nodes) == 0 {
		// A head with no suite has no run either, and both slices stay as they are: nil, which
		// is what a head GitHub answered no object for has always carried.
		return head
	}
	// The loop above walks every suite and every run of the head to validate it, so what the
	// two slices come to is known before either is built: a head of 7 suites and 32 runs grew
	// them through a dozen allocations and as many copies, once for every head of every page.
	head.suites = make([]object, 0, len(commit.CheckSuites.Nodes))
	if runs > 0 {
		head.runs = make([]object, 0, runs)
	}
	for _, suite := range commit.CheckSuites.Nodes {
		head.suites = append(head.suites, checkSuiteObject(suite, commit.OID))
		for _, run := range suite.CheckRuns.Nodes {
			head.runs = append(head.runs, checkRunObject(run, suite, commit.OID))
		}
	}
	return head
}

// checkRunObject is one check run the query answered, in the shape the webhook's own body
// carries it in, so contracts.GithubCIObservation maps a run read here and the same run
// delivered by webhook to one observation and the store applies the second of them as no change.
//
// Each field is the webhook's, taken from the field GraphQL spells it by:
//
//   - databaseId is the webhook's id, the run's own number, which is what the record keys the
//     check by and what tells a re-run from the run before it;
//   - status and conclusion are GraphQL enums, which are upper case (COMPLETED, SUCCESS) where
//     the webhook's are lower case (completed, success), so each is lower-cased; a conclusion
//     GitHub answers null is the empty string the webhook's own null maps to;
//   - permalink is the webhook's html_url, the page a person opens to read the run. GraphQL's
//     `url` is the same page and detailsUrl is the App's own link for it (for a GitHub Actions
//     run the job's page, for another App whatever it set), and neither is what the webhook
//     carries: recording either would rewrite the URL of every run the listener has already
//     recorded, at every pass, for ever;
//   - startedAt and completedAt are the webhook's started_at and completed_at, which order two
//     observations of one run;
//   - the suite's app.databaseId is the webhook's app.id, the App that created the run, which the
//     review-App rule reads.
//
// The head commit's oid is the webhook's head_sha: GraphQL's CheckRun has no head sha of its
// own, and the commit the query read the suite from is the commit the run ran at.
func checkRunObject(run graphQLRun, suite graphQLSuite, oid string) object {
	item := object{
		"id":           run.DatabaseID,
		"name":         run.Name,
		"status":       strings.ToLower(run.Status),
		"conclusion":   strings.ToLower(run.Conclusion),
		"head_sha":     oid,
		"started_at":   run.StartedAt,
		"completed_at": run.CompletedAt,
		"html_url":     run.Permalink,
	}
	if suite.App != nil {
		item["app"] = object{"id": suite.App.DatabaseID}
	}
	return item
}

// checkSuiteObject is one check suite the query answered, in the webhook's own shape, as
// checkRunObject is for a run: the suite's databaseId is the webhook's id, its app.databaseId
// the App that created it, its status and conclusion the lower-cased enums, its updatedAt the
// webhook's updated_at -- the field a suite's observation is ordered by -- and the commit's oid
// the head_sha, which GraphQL's CheckSuite does not carry.
func checkSuiteObject(suite graphQLSuite, oid string) object {
	item := object{
		"id":         suite.DatabaseID,
		"status":     strings.ToLower(suite.Status),
		"conclusion": strings.ToLower(suite.Conclusion),
		"head_sha":   oid,
		"updated_at": suite.UpdatedAt,
	}
	if suite.App != nil {
		item["app"] = object{"id": suite.App.DatabaseID}
	}
	return item
}

// FNV-1a's two constants, the 64-bit pair hash/fnv sums with. They are written out here rather
// than taken from fnv.New64a because that answers a hash.Hash64, which is an interface, a heap
// allocation and a method call for every byte: a pass fingerprints some 39 objects of each of
// 833 heads, and this way the whole of it is arithmetic on a register.
const (
	fnvOffset uint64 = 14695981039346656037
	fnvPrime  uint64 = 1099511628211
)

// fingerprintOf is the hash of the check runs and check suites one query answered for a head:
// what the ETag of a listing is to a REST read, and the one thing that tells a head GitHub has
// nothing new for from a head it has. A pass holding it writes nothing at all for the head
// whose next answer fingerprints the same (visitHead).
//
// It hashes what would be written rather than what GitHub sent. These objects are the ones
// checkRunObject and checkSuiteObject build, which is the shape the webhook's own body carries
// and the shape contracts.GithubCIObservation maps, so two answers GitHub spelled differently
// and this package maps to the same observations are the same work and fingerprint the same,
// and an answer holding one observation more, or one field of one observation different,
// fingerprints differently and is written. Hashing GitHub's raw JSON instead would write every
// head again for a field this package never reads.
//
// The order the hash is taken in is fixed, which is what two passes must agree on: the two
// counts, then the suites and the runs in the order headOf built them, and within one object
// the fields combined by a sum, since a Go map has no order of its own and sorting the names of
// every object of every head would be an allocation and a sort for each of them. Two distinct
// fields of one object can only cancel in that sum if their name-and-value hashes collide,
// which is the hash's business rather than the sum's.
//
// Zero is no fingerprint -- what the Reconciler holds for a head no query has answered yet --
// so a hash that comes out zero is answered one instead. That costs one answer in 2^64 a write
// it did not need and can hide nothing: the two values stand for the same work either way.
func fingerprintOf(runs, suites []object) uint64 {
	sum := hashUint(hashUint(fnvOffset, uint64(len(suites))), uint64(len(runs)))
	for _, item := range suites {
		sum = hashUint(sum, hashObject(item))
	}
	for _, item := range runs {
		sum = hashUint(sum, hashObject(item))
	}
	if sum == 0 {
		return 1
	}
	return sum
}

// hashObject is one mapped object's own hash: each field hashed from its name through its
// value, and the fields' hashes added.
func hashObject(item object) uint64 {
	var sum uint64
	for name, value := range item {
		sum += hashValue(hashString(fnvOffset, name), value)
	}
	return sum
}

// hashValue folds one field's value into the sum. The three cases are every type the two
// builders above put in an object: the text of a status, a name or a timestamp, the int64 of a
// databaseId, and the one nested object, an app and its id.
func hashValue(sum uint64, value any) uint64 {
	switch held := value.(type) {
	case string:
		return hashString(sum, held)
	case int64:
		return hashUint(sum, uint64(held))
	case object:
		return hashUint(sum, hashObject(held))
	default:
		// Nothing reaches here today. A field of another type added to either builder hashes
		// its rendering rather than nothing at all: a value this passed over would hold the
		// fingerprint still while the observation under it changed, and the change would never
		// be written. The allocation Sprint makes is the price of that, in a branch that does
		// not run.
		return hashString(sum, fmt.Sprint(value))
	}
}

// hashString folds the bytes of one string into the sum, and hashUint the eight bytes of one
// number, each as FNV-1a does: exclusive-or the byte in, then multiply by the prime.
func hashString(sum uint64, text string) uint64 {
	for i := range len(text) {
		sum = (sum ^ uint64(text[i])) * fnvPrime
	}
	return sum
}

func hashUint(sum, value uint64) uint64 {
	for range 8 {
		sum = (sum ^ (value & 0xff)) * fnvPrime
		value >>= 8
	}
	return sum
}

// refusedPulls reads GitHub's errors array: the indexes of the pull request nodes an entry is
// about, and whether any entry is about more than one of them.
//
// GitHub answers a field one installation may not read with null in data and an entry in errors
// whose path names that field, the rest of the answer standing: a refusal of one head's Checks
// has a path like ["repository","pullRequests","nodes",3,"commits","nodes",0,"commit",
// "checkSuites"], so the number after the first "nodes" is the head it refused. An entry whose
// path names no pull request node -- the repository itself, or the query -- is the whole page
// refused, and every head of that page is read over REST.
func refusedPulls(errors []graphQLError) (map[int]struct{}, bool) {
	if len(errors) == 0 {
		// Almost every answer GitHub gives carries no error at all, and a nil map answers the
		// caller's lookups false, so the map is made for the answers that hold an entry alone.
		return nil, false
	}
	refused := make(map[int]struct{})
	whole := false
	for _, entry := range errors {
		index, ok := pullIndex(entry.Path)
		if !ok {
			whole = true
			continue
		}
		refused[index] = struct{}{}
	}
	return refused, whole
}

// pullIndex is the pull request node one error path is about: the number that follows
// "pullRequests","nodes" in it.
func pullIndex(path []any) (int, bool) {
	for i := 0; i+2 < len(path); i++ {
		if text, ok := path[i].(string); !ok || text != "pullRequests" {
			continue
		}
		if text, ok := path[i+1].(string); !ok || text != "nodes" {
			continue
		}
		// A JSON number decodes into any as a float64, and an index is a whole small one.
		index, ok := path[i+2].(float64)
		if !ok || index < 0 {
			return 0, false
		}
		return int(index), true
	}
	return 0, false
}

// errorText is GitHub's errors array as one line of a log, each entry's type and message: the
// pass names what GitHub refused, which is the only way an operator can tell a permission the
// App lacks from a query GitHub gave up on.
func errorText(errors []graphQLError) string {
	if len(errors) == 0 {
		return "the answer held no repository and no error"
	}
	said := make([]string, 0, len(errors))
	for _, entry := range errors {
		said = append(said, strings.TrimSpace(entry.Type+" "+entry.Message))
	}
	return strings.Join(said, "; ")
}

// graphQLPace waits for what the query before it in this pass took, so the pass's queries never
// take more than GraphQLTimePerMinute of a minute: a query of d seconds is followed by a wait of
// d*(60-45)/45, which leaves GitHub processing the pass's queries for 45 seconds of every 60.
// A pass of 16 repositories measured 53 queries and 178 seconds of GitHub's own time, so the
// waits this leaves add about a minute to a pass and keep it well inside GitHub's own 60
// seconds a minute.
//
// It waits before a query rather than after it, as Reconciler.pace does, so the pass's last
// query is not followed by a wait that buys nothing and a pass of one query is as quick as that
// query. Unlike the REST pace it is left whatever Pace a caller configured, zero included: the
// REST pace spreads requests a budget counts one by one, which a test may ask for none of,
// while this is GitHub's secondary limit on the App's own processing time, which no caller may
// opt out of and which costs a test with quick answers microseconds.
func (r *Reconciler) graphQLPace(ctx context.Context, c *counts) {
	if c.graphQLTook <= 0 {
		return
	}
	timer := time.NewTimer(graphQLWait(c.graphQLTook))
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// graphQLWait is the pause one query of took earns: the time outside the budget, shared out in
// the same proportion the budget shares out the minute.
func graphQLWait(took time.Duration) time.Duration {
	return took * (time.Minute - GraphQLTimePerMinute) / GraphQLTimePerMinute
}

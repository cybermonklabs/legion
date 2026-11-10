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
const headsQuery = `query($owner: String!, $name: String!, $after: String) {
  rateLimit { cost remaining resetAt }
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, first: ` + pullsPerQueryText + `, after: $after, orderBy: {field: CREATED_AT, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        commits(last: 1) {
          nodes {
            commit {
              oid
              checkSuites(first: ` + suitesPerQueryText + `) {
                pageInfo { hasNextPage }
                nodes {
                  databaseId
                  app { databaseId }
                  status
                  conclusion
                  updatedAt
                  checkRuns(first: ` + runsPerQueryText + `, filterBy: {checkType: ALL}) {
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

// The page sizes as the query spells them. A GraphQL document takes its `first` as a literal, and
// a literal written twice is a literal that can disagree with itself, so each is the constant
// above written once.
const (
	pullsPerQueryText  = "20"
	suitesPerQueryText = "30"
	runsPerQueryText   = "60"
)

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
	c.requests++
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
		if pull == nil {
			continue
		}
		_, pullRefused := refused[index]
		page.heads = append(page.heads, headOf(pull, pullRefused))
	}
	return page, nil
}

// headOf is one answered pull request as the pass takes it: its head commit, the check objects
// of that commit, and whether the query could answer it whole. refused says GitHub answered an
// errors entry against this pull request's own path, which is its Checks fields refused.
//
// A pull request GitHub listed with no head commit is no head to read, exactly as one the REST
// listing answered with no head.sha is: it is answered with an empty sha and the caller passes
// over it.
func headOf(pull *graphQLPull, refused bool) queryHead {
	head := queryHead{number: pull.Number}
	if len(pull.Commits.Nodes) == 0 || pull.Commits.Nodes[0].Commit == nil {
		return head
	}
	commit := pull.Commits.Nodes[0].Commit
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
	for _, suite := range commit.CheckSuites.Nodes {
		if suite.CheckRuns == nil {
			head.overflow, head.why = true, "GitHub answered a check suite's runs null"
			return head
		}
		if suite.CheckRuns.PageInfo.HasNextPage {
			head.overflow, head.why = true, fmt.Sprintf("a check suite of the head has more than %d check runs", runsPerQuery)
			return head
		}
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

package api

// going_reads.go holds the reads a command-bar run's tools make beside the search (R8), each one
// a read a route of this table serves and made through the same functions that route answers
// with, in process, for the run's author (decision 22): GET /api/v1/agents, GET /api/v1/issues and
// an issue's progress, GET /api/v1/projects/{key}/architecture and /activity, and the three
// delivery reads. A block's spec names one of them, and the run fills the block by making that
// read again (going_blocks.go). The tools themselves - their names, what the model is told and
// their budget - are going's (going/reads.go).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingReadRoutes is the rows of the route table each read tool reads through. A caller is
// offered a tool only where its credential may call every row the tool reads (goingReadsFor), as
// a caller is offered an action only where it may call the action's rows (decision 7), and
// register refuses a table that does not serve every row named here.
var goingReadRoutes = map[string][]string{
	going.ReadRoster:          {"GET /api/v1/agents"},
	going.ReadProgress:        {"GET /api/v1/issues", "GET /api/v1/issues/{key}"},
	going.ReadArchitecture:    {"GET /api/v1/projects/{key}/architecture"},
	going.ReadProjectActivity: {"GET /api/v1/projects/{key}/activity"},
	going.ReadDelivery:        {"GET /api/v1/delivery/measures", "GET /api/v1/delivery/timeline", "GET /api/v1/delivery/pipeline"},
}

// goingDeploymentReads are the reads that are no one project's: the roster names every
// project's sessions and the delivery reads every project's pull requests and issues. A
// credential scoped to one project reads nothing outside it through the bar (decision 22), so it
// is offered neither; every other read is held to the run's project (goingReadRun.project).
var goingDeploymentReads = map[string]bool{going.ReadRoster: true, going.ReadDelivery: true}

// The bounds of what one read hands the model, so a read of a large project is a summary the
// model can cite and not the project whole: the issues a progress read lists, the components an
// architecture read lists, the sessions a roster read lists and the events a timeline lists.
const (
	goingReadIssueMax     = 25
	goingReadComponentMax = 100
	goingReadSessionMax   = 50
	goingReadEventMax     = 100
)

// goingRosterAnswerMargin is the time a run keeps for its answer after a roster read waits out a
// refilling listener: a Retry-After that would leave less than this of the run's window is not
// waited on, and the stored rows are read instead.
const goingRosterAnswerMargin = 5 * time.Second

// The roster frame's sources (contracts GoingRosterEvent).
const (
	goingRosterLive   = "live"
	goingRosterStored = "stored"
)

// goingReadRoutesOf is each read tool's rows as the table mounts them, and an error naming every
// row it does not serve.
func goingReadRoutesOf(routes []apiRoute) (map[string][]GoingRoute, error) {
	served := make(map[string]apiRoute, len(routes))
	for _, route := range routes {
		served[route.Method+" "+route.Pattern] = route
	}
	reads := make(map[string][]GoingRoute, len(goingReadRoutes))
	var refusals []string
	for tool, rows := range goingReadRoutes {
		for _, name := range rows {
			mounted, isServed := served[name]
			if !isServed {
				refusals = append(refusals, fmt.Sprintf("%s: %s is no row of the route table", tool, name))
				continue
			}
			reads[tool] = append(reads[tool], GoingRoute{Method: mounted.Method, Pattern: mounted.Pattern, auth: mounted.Auth, scopes: mounted.OAuthScopes})
		}
	}
	if len(refusals) > 0 {
		sort.Strings(refusals)
		return nil, fmt.Errorf("%d read tool problem(s):\n%s", len(refusals), strings.Join(refusals, "\n"))
	}
	return reads, nil
}

// goingReadsFor is the read tools one caller's run may call: every tool whose rows the caller's
// credential may call, and, for a credential scoped to one project, none of the reads that are no
// one project's.
func (s *server) goingReadsFor(caller goingCredential, scoped bool) []string {
	var reads []string
	for tool, routes := range s.goingReads {
		if scoped && goingDeploymentReads[tool] {
			continue
		}
		if !slices.ContainsFunc(routes, func(route GoingRoute) bool { return !route.runnableBy(caller) }) {
			reads = append(reads, tool)
		}
	}
	sort.Strings(reads)
	return reads
}

// goingRosterFrame is the roster frame's data (contracts GoingRosterEvent, decision 18): how old
// Dispatch's copy of the listener's session list was when the run read it, and whether it was that
// live copy or, the listener being unavailable, the sessions Dispatch's own rows name.
type goingRosterFrame struct {
	AgeMS  int64  `json:"age_ms"`
	Source string `json:"source"`
}

// goingReadRun is one run's reads: the project the run was asked of, which every project read is
// held to, the page's context chip, which names what a read that names nothing reads, and the
// roster the run last read, for its roster frame.
type goingReadRun struct {
	s     *server
	asked string
	chip  *goingContext
	funcs map[string]going.ReadFunc

	mu     sync.Mutex
	roster *goingRosterFrame
}

// goingRunReads is the reads of one run, each read tool its caller may call.
func (s *server) goingRunReads(request goingRequest, start goingRunStart) *goingReadRun {
	run := &goingReadRun{s: s, asked: request.Project, chip: start.chip}
	every := map[string]going.ReadFunc{
		going.ReadRoster:          run.readRoster,
		going.ReadProgress:        run.readProgress,
		going.ReadArchitecture:    run.readArchitecture,
		going.ReadProjectActivity: run.readProjectActivity,
		going.ReadDelivery:        run.readDelivery,
	}
	run.funcs = make(map[string]going.ReadFunc, len(start.reads))
	for _, tool := range start.reads {
		if read, known := every[tool]; known {
			run.funcs[tool] = read
		}
	}
	return run
}

// rosterRead is the roster frame of the last roster read the run made, and false where it made
// none.
func (g *goingReadRun) rosterRead() (goingRosterFrame, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.roster == nil {
		return goingRosterFrame{}, false
	}
	return *g.roster, true
}

// project is the project a read reads: the one the model named, else the run's, else the page's.
// A run asked of one project reads that project alone, whatever the model names (decision 22):
// a credential scoped to one project asks only of it (createGoingRequest), and search is held to
// it the same way (goingSearch).
func (g *goingReadRun) project(named string) (string, error) {
	project := cmp.Or(strings.TrimSpace(named), g.asked)
	if project == "" && g.chip != nil {
		project = g.chip.Project
	}
	switch {
	case project == "":
		return "", going.Refuse("name the project to read: the question names none and no project is on the page")
	case !projectKeyPattern.MatchString(project):
		return "", going.Refuse("%s", projectKeyProblem("project", project))
	case g.asked != "" && project != g.asked:
		return "", going.Refuse("this question was asked of project %s, and reads that project alone", g.asked)
	}
	return project, nil
}

// decodeGoingArguments decodes a read's arguments into value, refusing a field it does not take.
func decodeGoingArguments(arguments json.RawMessage, value any) error {
	decoder := json.NewDecoder(strings.NewReader(string(arguments)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return going.Refuse("the arguments do not fit the tool: %v", err)
	}
	return nil
}

// goingReadError is err as a read answers it: a refusal the route would have answered with a 4xx
// is told to the model, in the route's own words; anything else is a failed store read.
func goingReadError(err error) error {
	var refused *apiError
	if errors.As(err, &refused) && refused.status >= 400 && refused.status < 500 {
		return going.Refuse("%s", refused.message)
	}
	return err
}

// issueHit is an issue as an item the answer may link.
func issueHit(key, title string) going.Hit {
	return going.Hit{Kind: "issue", ID: key, Title: title, Href: "/issues/" + key}
}

// readRoster is read_roster: GET /api/v1/agents' rows, each session with its open asks and last
// activity, and the age of Dispatch's copy of the listener's list. A listener that is refilling
// its list is waited out for the Retry-After it sent while the run's window leaves room to answer
// after it, then read again; a listener that is unavailable, or still refilling once the window
// leaves no room, leaves the sessions Dispatch's own rows name, marked stored (R10).
func (g *goingReadRun) readRoster(ctx context.Context, arguments json.RawMessage) (going.Read, error) {
	if err := decodeGoingArguments(arguments, &struct{}{}); err != nil {
		return going.Read{}, err
	}
	rows, frame, err := g.s.goingRoster(ctx)
	if err != nil {
		return going.Read{}, err
	}
	g.mu.Lock()
	g.roster = &frame
	g.mu.Unlock()

	sessions := make([]map[string]any, 0, min(len(rows), goingReadSessionMax))
	live, unresponsive, openAsks := 0, 0, 0
	for _, row := range rows {
		switch row.State {
		case "live":
			live++
		case "unresponsive":
			unresponsive++
		}
		openAsks += row.OpenAsks
		if len(sessions) == goingReadSessionMax {
			continue
		}
		sessions = append(sessions, map[string]any{
			"session_id": row.SessionID, "title": row.Title, "state": row.State, "roles": row.Roles,
			"open_asks": row.OpenAsks, "last_activity": row.LastActivity,
		})
	}
	return going.Read{
		Facts: map[string]any{"source": frame.Source, "age_ms": frame.AgeMS},
		Tables: map[string][]map[string]any{
			"sessions": sessions,
			"totals":   {{"sessions": len(rows), "live": live, "unresponsive": unresponsive, "open_asks": openAsks}},
		},
	}, nil
}

// goingRoster is the roster a run reads and its frame: GET /api/v1/agents' rows from Dispatch's
// copy of the listener's list while the listener answers, aged by the database's clock, and the
// stored rows otherwise (readRoster says when).
func (s *server) goingRoster(ctx context.Context) ([]agentResponse, goingRosterFrame, error) {
	for {
		roster, err := s.readRouteRoster(ctx)
		if err != nil {
			return nil, goingRosterFrame{}, err
		}
		if !roster.answered && s.lifetime.Err() != nil {
			// The process is shutting down, as listAgents reads it: the listener is asked on the
			// run's own context.
			roster = s.listRouteRoster(ctx)
		}
		if roster.answered {
			rows, err := s.agentRows(ctx, roster.sessions)
			if err != nil {
				return nil, goingRosterFrame{}, err
			}
			age, err := s.rosterAge(ctx, roster.sent)
			if err != nil {
				return nil, goingRosterFrame{}, err
			}
			return rows, goingRosterFrame{AgeMS: age.Milliseconds(), Source: goingRosterLive}, nil
		}
		if roster.reason == model.RouteReasonRefilling {
			wait := goingRetryAfter(roster.retryAfter)
			if deadline, bounded := ctx.Deadline(); bounded && time.Until(deadline) > wait+goingRosterAnswerMargin {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil, goingRosterFrame{}, ctx.Err()
				case <-timer.C:
				}
				continue
			}
		}
		rows, err := s.storedAgentRows(ctx)
		if err != nil {
			return nil, goingRosterFrame{}, err
		}
		return rows, goingRosterFrame{Source: goingRosterStored}, nil
	}
}

// goingRetryAfter is a refilling listener's Retry-After as a wait, the listener's own default
// where it sent none or sent no number of seconds.
func goingRetryAfter(retryAfter string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(cmp.Or(retryAfter, envoyRefillingRetryAfter)))
	if err != nil || seconds < 0 {
		seconds, _ = strconv.Atoi(envoyRefillingRetryAfter)
	}
	return time.Duration(seconds) * time.Second
}

// rosterAge is how old a roster sent at sent is by the database's clock, the clock sent was read
// on: zero for a roster read just now (routeRoster.sent).
func (s *server) rosterAge(ctx context.Context, sent time.Time) (time.Duration, error) {
	if sent.IsZero() {
		return 0, nil
	}
	var now time.Time
	if err := s.deps.Store.Pool.QueryRow(ctx, `select now()`).Scan(&now); err != nil {
		return 0, err
	}
	return max(now.Sub(sent), 0), nil
}

// goingStoredSessionsQuery is every session Dispatch's own rows name as at work: the holder of a
// claim on an open issue, and the author of an open ask on an open issue or a document.
const goingStoredSessionsQuery = `
	select distinct id from (
		select i.claimed_by->>'id' as id
		from issues i
		where i.closed_at is null and i.claimed_by->>'kind' = 'session'
		union
		select a.author->>'id'
		from asks a
		left join issues i on i.key = a.issue_key
		where a.state = 'open' and a.author->>'kind' = 'session' and (i.key is null or i.closed_at is null)
	) named
	where id is not null and id <> ''`

// storedAgentRows is the roster as Dispatch's own rows tell it, with no listener: every session
// goingStoredSessionsQuery names, each with the newest title it stamped on a write, its open asks
// and its last activity, the newest first. Its state is unknown, since only the listener knows
// whether a session is running.
func (s *server) storedAgentRows(ctx context.Context) ([]agentResponse, error) {
	pool := s.deps.Store.Pool
	rows, err := pool.Query(ctx, goingStoredSessionsQuery)
	if err != nil {
		return nil, err
	}
	sessionIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if len(sessionIDs) == 0 {
		return []agentResponse{}, nil
	}
	openAsks, err := s.agentOpenAskCounts(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	activity, err := s.agentLastActivity(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	titles, err := delivery.ResolveSessionTitles(ctx, pool, sessionIDs)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(sessionIDs, func(left, right int) bool {
		leftAt, rightAt := activity[sessionIDs[left]], activity[sessionIDs[right]]
		if !leftAt.Equal(rightAt) {
			return leftAt.After(rightAt)
		}
		return sessionIDs[left] < sessionIDs[right]
	})
	agents := make([]agentResponse, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		row := agentResponse{OpenAsks: openAsks[id]}
		row.SessionID, row.Title, row.State = id, titles[id], "unknown"
		row.Roles, row.Capabilities = []string{}, []string{}
		if at, active := activity[id]; active {
			row.LastActivity = timestampPtr(&at)
		}
		agents = append(agents, row)
	}
	return agents, nil
}

// readProgress is read_progress: an issue's task and child progress with its children, as GET
// /api/v1/issues and the issue read carry them, or a project's issues by status with its open
// issues. With neither named, the page's issue is read, else the run's or the page's project.
func (g *goingReadRun) readProgress(ctx context.Context, arguments json.RawMessage) (going.Read, error) {
	var named struct {
		Project string `json:"project"`
		Issue   string `json:"issue"`
	}
	if err := decodeGoingArguments(arguments, &named); err != nil {
		return going.Read{}, err
	}
	issue := strings.TrimSpace(named.Issue)
	if issue == "" && strings.TrimSpace(named.Project) == "" && g.chip != nil {
		issue = g.chip.Issue
	}
	if issue != "" {
		return g.issueProgress(ctx, issue)
	}
	project, err := g.project(named.Project)
	if err != nil {
		return going.Read{}, err
	}
	return g.projectProgress(ctx, project)
}

// issueProgress is one issue's progress: its task and child counts, its children by status, and
// its children in the listing's order.
func (g *goingReadRun) issueProgress(ctx context.Context, key string) (going.Read, error) {
	if !issueKeyPattern.MatchString(key) {
		return going.Read{}, going.Refuse("issue must be an issue key such as DEMO-12, and %q is none", key)
	}
	s := g.s
	issue, progress, err := s.loadIssueWithProgress(ctx, s.deps.Store.Pool, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return going.Read{}, going.Refuse("Dispatch holds no issue %s", key)
	}
	if err != nil {
		return going.Read{}, err
	}
	if g.asked != "" && issue.Project != g.asked {
		return going.Read{}, going.Refuse("this question was asked of project %s, and %s is an issue of %s", g.asked, key, issue.Project)
	}
	children, _, err := s.goingIssuePage(ctx, "", key, false)
	if err != nil {
		return going.Read{}, err
	}
	statuses, err := s.goingStatusCounts(ctx, "", key)
	if err != nil {
		return going.Read{}, err
	}
	bars := []map[string]any{}
	if progress.Tasks != nil {
		bars = append(bars, map[string]any{"label": "tasks", "done": progress.Tasks.Done, "total": progress.Tasks.Total})
	}
	if progress.Children != nil {
		bars = append(bars, map[string]any{"label": "children", "done": progress.Children.Done, "total": progress.Children.Total})
	}
	items := append([]going.Hit{issueHit(issue.Key, issue.Title)}, issueHits(children)...)
	return going.Read{
		Facts: map[string]any{"issue": issue.Key, "title": issue.Title, "status": issue.Status, "project": issue.Project, "href": "/issues/" + issue.Key},
		Tables: map[string][]map[string]any{
			"progress": bars,
			"statuses": statuses,
			"issues":   issueRows(children),
		},
		Items: items,
	}, nil
}

// projectProgress is one project's progress: its issues done of all, its issues by status, and
// its open issues in the listing's order.
func (g *goingReadRun) projectProgress(ctx context.Context, project string) (going.Read, error) {
	s := g.s
	if _, err := s.loadProject(ctx, s.deps.Store.Pool, project); err != nil {
		return going.Read{}, goingReadError(err)
	}
	statuses, err := s.goingStatusCounts(ctx, project, "")
	if err != nil {
		return going.Read{}, err
	}
	open, openTotal, err := s.goingIssuePage(ctx, project, "", true)
	if err != nil {
		return going.Read{}, err
	}
	done, total := 0, 0
	for _, row := range statuses {
		count := row["count"].(int)
		total += count
		if row["status"] == "done" {
			done += count
		}
	}
	return going.Read{
		Facts: map[string]any{"project": project, "issues": total, "open_issues": openTotal},
		Tables: map[string][]map[string]any{
			"progress": {{"label": "issues", "done": done, "total": total}},
			"statuses": statuses,
			"issues":   issueRows(open),
		},
		Items: issueHits(open),
	}, nil
}

// goingIssuePage is GET /api/v1/issues' first page of goingReadIssueMax issues, in its order, for
// one project or one parent's children, open issues alone when open is set, with the count of
// every issue the filters match: the listing's own statement and scan.
func (s *server) goingIssuePage(ctx context.Context, project, parent string, open bool) ([]model.IssueSummary, int, error) {
	arguments := []any{project, "", parent, microsecondCeiling(nil), []string{}, open, []int16{}, false,
		microsecondCeiling(nil), microsecondCeiling(nil), goingReadIssueMax, 0}
	return s.scanIssuePage(ctx, listIssuesPageQuery, arguments)
}

// goingStatusCountsQuery counts issues by status under GET /api/v1/issues' project and parent
// filters ($1 and $2, empty for none), which is the total that listing answers for each status
// filter.
const goingStatusCountsQuery = `
	select i.status, count(*)::int
	from issues i
	where ($1 = '' or i.project_key = $1) and ($2 = '' or i.parent_key = $2)
	group by i.status`

// goingStatusCounts is the issues of a project or a parent counted by status, in the lifecycle's
// order, each status that holds none left out.
func (s *server) goingStatusCounts(ctx context.Context, project, parent string) ([]map[string]any, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, goingStatusCountsQuery, project, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	table := make([]map[string]any, 0, len(counts))
	for _, status := range model.IssueStatuses {
		if count, held := counts[status]; held {
			table = append(table, map[string]any{"status": status, "count": count})
		}
	}
	return table, nil
}

// issueRows is the issues as a read's table rows.
func issueRows(issues []model.IssueSummary) []map[string]any {
	rows := make([]map[string]any, 0, len(issues))
	for _, issue := range issues {
		rows = append(rows, map[string]any{"key": issue.Key, "title": issue.Title, "status": issue.Status, "priority": issue.Priority})
	}
	return rows
}

// issueHits is the issues as items the answer may link.
func issueHits(issues []model.IssueSummary) []going.Hit {
	hits := make([]going.Hit, 0, len(issues))
	for _, issue := range issues {
		hits = append(hits, issueHit(issue.Key, issue.Title))
	}
	return hits
}

// readArchitecture is read_architecture: GET /api/v1/projects/{key}/architecture's per-component
// counts and the project's totals.
func (g *goingReadRun) readArchitecture(ctx context.Context, arguments json.RawMessage) (going.Read, error) {
	var named struct {
		Project string `json:"project"`
	}
	if err := decodeGoingArguments(arguments, &named); err != nil {
		return going.Read{}, err
	}
	project, err := g.project(named.Project)
	if err != nil {
		return going.Read{}, err
	}
	tree, err := g.s.architectureTree(ctx, project)
	if err != nil {
		return going.Read{}, goingReadError(err)
	}
	components := make([]map[string]any, 0, min(len(tree.Components), goingReadComponentMax))
	for _, component := range tree.Components {
		if len(components) == goingReadComponentMax {
			break
		}
		components = append(components, map[string]any{
			"id": component.ID, "title": component.Title, "parent": component.Parent,
			"done": component.Done, "total": component.Total, "own_done": component.OwnDone, "own_total": component.OwnTotal,
		})
	}
	totals := tree.Totals
	return going.Read{
		Facts: map[string]any{"project": project, "components": len(tree.Components)},
		Tables: map[string][]map[string]any{
			"components": components,
			"totals": {{
				"issues_done": totals.IssuesDone, "issues_total": totals.IssuesTotal, "unassigned": totals.Unassigned,
				"not_architectural": totals.NotArchitectural, "components_without_work": totals.ComponentsWithoutWork,
				"retired_links": totals.RetiredLinks,
			}},
		},
	}, nil
}

// readProjectActivity is read_project_activity: GET /api/v1/projects/{key}/activity's weeks, in
// total and by kind.
func (g *goingReadRun) readProjectActivity(ctx context.Context, arguments json.RawMessage) (going.Read, error) {
	var named struct {
		Project string `json:"project"`
		Weeks   *int   `json:"weeks"`
	}
	if err := decodeGoingArguments(arguments, &named); err != nil {
		return going.Read{}, err
	}
	project, err := g.project(named.Project)
	if err != nil {
		return going.Read{}, err
	}
	weeks := projectActivityWeeksDefault
	if named.Weeks != nil {
		if *named.Weeks < 1 || *named.Weeks > projectActivityWeeksMax {
			return going.Read{}, going.Refuse("weeks must be an integer from 1 to %d", projectActivityWeeksMax)
		}
		weeks = *named.Weeks
	}
	activity, err := g.s.projectActivity(ctx, project, weeks)
	if err != nil {
		return going.Read{}, goingReadError(err)
	}
	weekRows := make([]map[string]any, 0, len(activity.Weeks))
	weekKinds := []map[string]any{}
	kinds := map[string]int{}
	events := 0
	for _, week := range activity.Weeks {
		weekRows = append(weekRows, map[string]any{"week": week.Week, "start": week.Start.Format(time.RFC3339), "events": week.Events})
		events += week.Events
		for _, kind := range slices.Sorted(maps.Keys(week.Kinds)) {
			kinds[kind] += week.Kinds[kind]
			weekKinds = append(weekKinds, map[string]any{"week": week.Week, "kind": kind, "count": week.Kinds[kind]})
		}
	}
	kindRows := make([]map[string]any, 0, len(kinds))
	for _, kind := range slices.Sorted(maps.Keys(kinds)) {
		kindRows = append(kindRows, map[string]any{"kind": kind, "count": kinds[kind]})
	}
	return going.Read{
		Facts: map[string]any{"project": project},
		Tables: map[string][]map[string]any{
			"weeks":      weekRows,
			"kinds":      kindRows,
			"week_kinds": weekKinds,
			"totals":     {{"events": events, "weeks": len(activity.Weeks)}},
		},
	}, nil
}

// The views read_delivery reads.
const (
	goingDeliveryMeasures = "measures"
	goingDeliveryTimeline = "timeline"
	goingDeliveryPipeline = "pipeline"
)

// readDelivery is read_delivery: GET /api/v1/delivery/measures, /timeline or /pipeline over the
// window it names, through the same reads and computations those routes answer with.
func (g *goingReadRun) readDelivery(ctx context.Context, arguments json.RawMessage) (going.Read, error) {
	var named struct {
		View string `json:"view"`
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := decodeGoingArguments(arguments, &named); err != nil {
		return going.Read{}, err
	}
	query := url.Values{}
	if from := strings.TrimSpace(named.From); from != "" {
		query.Set("from", from)
	}
	if to := strings.TrimSpace(named.To); to != "" {
		query.Set("to", to)
	}
	s := g.s
	settings, err := delivery.GetSettings(ctx, s.deps.Store.Pool)
	if errors.Is(err, delivery.ErrNoSettings) {
		return going.Read{}, going.Refuse("delivery is not configured on this deployment")
	}
	if err != nil {
		return going.Read{}, err
	}
	now := time.Now().UTC()
	switch named.View {
	case goingDeliveryMeasures, goingDeliveryTimeline:
		from, to, err := parseDeliveryWindow(query)
		if err != nil {
			return going.Read{}, going.Refuse("%v", err)
		}
		population, err := s.readDeliveryPopulation(ctx, settings, from, to, url.Values{}, named.View == goingDeliveryTimeline)
		if err != nil {
			return going.Read{}, err
		}
		facts := map[string]any{"view": named.View, "from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)}
		if named.View == goingDeliveryMeasures {
			return deliveryMeasuresRead(facts, deliveryMeasuresCompute(settings, population, from, to, now)), nil
		}
		return deliveryTimelineRead(facts, deliveryTimelineCompute(settings, population, from, to, now)), nil
	case goingDeliveryPipeline:
		at, err := parseOptionalRFC3339(url.Values{"at": query["to"]}, "at", now)
		if err != nil {
			return going.Read{}, going.Refuse("%v", err)
		}
		at = at.UTC()
		population, err := s.readPipelinePopulation(ctx, settings, at)
		if err != nil {
			return going.Read{}, err
		}
		return deliveryPipelineRead(deliveryPipelineCompute(settings, population, at, now)), nil
	}
	return going.Read{}, going.Refuse("view must be %s, %s or %s", goingDeliveryMeasures, goingDeliveryTimeline, goingDeliveryPipeline)
}

// deliveryMeasuresRead is GET /api/v1/delivery/measures' answer as a read: its headline numbers
// as one row, and its per-day series.
func deliveryMeasuresRead(facts map[string]any, answer deliveryMeasuresResponse) going.Read {
	result := answer.Measures
	daily := make([]map[string]any, 0, len(result.Daily))
	for _, day := range result.Daily {
		daily = append(daily, map[string]any{
			"day": day.Day, "partial": day.Partial, "deploys": day.Deploys, "concluded": day.Concluded,
			"reached_production": day.ReachedProduction, "cancelled": day.Cancelled, "run_success_rate": day.RunSuccessRate,
		})
	}
	return going.Read{
		Facts: facts,
		Tables: map[string][]map[string]any{
			"totals": {{
				"successful_deploys": result.DeployFrequency.SuccessfulDeploys, "deploys_with_prs": result.DeployFrequency.DeploysWithPRs,
				"deploys_per_day": result.DeployFrequency.PerDay, "with_prs_per_day": result.DeployFrequency.WithPRsPerDay,
				"merge_to_production_median_minutes": result.LeadTime.MergeToProduction.MedianMinutes,
				"opened_to_merge_median_minutes":     result.LeadTime.OpenedToMerge.MedianMinutes,
				"change_failure_rate_per_deploy":     result.ChangeFailureRate.PerDeploy.Rate,
				"rework_share":                       result.ReworkShare,
				"deploy_run_success_rate":            result.DeployRunSuccess.Rate,
				"unowned_p0":                         len(answer.UnownedP0),
			}},
			"daily": daily,
		},
	}
}

// deliveryTimelineRead is GET /api/v1/delivery/timeline's answer as a read: each merge and each
// deploy run as a dated event in its lane, oldest first and the newest goingReadEventMax kept.
func deliveryTimelineRead(facts map[string]any, answer deliveryTimelineWithMeasures) going.Read {
	events := make([]map[string]any, 0, len(answer.PRs)+len(answer.Runs))
	for _, pr := range answer.PRs {
		if pr.MergedAt == nil {
			continue
		}
		events = append(events, map[string]any{
			"at": pr.MergedAt.UTC().Format(time.RFC3339), "lane": "merge", "label": fmt.Sprintf("%s#%d %s", pr.Repo, pr.Number, pr.Title),
		})
	}
	for _, run := range answer.Runs {
		conclusion := "running"
		if run.Conclusion != nil {
			conclusion = string(*run.Conclusion)
		}
		events = append(events, map[string]any{
			"at": run.StartedAt.UTC().Format(time.RFC3339), "lane": "deploy", "label": fmt.Sprintf("run %d: %s", run.ID, conclusion),
		})
	}
	sort.SliceStable(events, func(left, right int) bool { return events[left]["at"].(string) < events[right]["at"].(string) })
	merges, runs := 0, len(answer.Runs)
	for _, event := range events {
		if event["lane"] == "merge" {
			merges++
		}
	}
	if len(events) > goingReadEventMax {
		events = events[len(events)-goingReadEventMax:]
	}
	return going.Read{
		Facts: facts,
		Tables: map[string][]map[string]any{
			"events": events,
			"totals": {{"merges": merges, "runs": runs}},
		},
	}
}

// deliveryPipelineRead is GET /api/v1/delivery/pipeline's answer as a read: the PR checks' and
// the production deploy's results per day, the jobs failing now and the slowest, and the shown
// days' totals.
func deliveryPipelineRead(answer deliveryPipelineResponse) going.Read {
	days := func(timing []measures.Day) []map[string]any {
		rows := make([]map[string]any, 0, len(timing))
		for _, day := range timing {
			rows = append(rows, map[string]any{
				"day": day.Day, "runs": day.N, "median_minutes": day.MedianMinutes, "success": day.Success,
				"failure": day.Failure, "success_rate": day.SuccessRate,
			})
		}
		return rows
	}
	failing := make([]map[string]any, 0, len(answer.PRChecks.FailingJobs))
	for _, job := range answer.PRChecks.FailingJobs {
		failing = append(failing, map[string]any{
			"name": job.Name, "failures": job.Failures, "results": job.Results, "last_failed_at": job.LastFailedAt.UTC().Format(time.RFC3339),
		})
	}
	slowest := make([]map[string]any, 0, len(answer.PRChecks.SlowestJobs))
	for _, job := range answer.PRChecks.SlowestJobs {
		slowest = append(slowest, map[string]any{"name": job.Name, "runs": job.Total.N, "median_minutes": job.Total.MedianMinutes})
	}
	checks, production := answer.PRChecks.Run.Total, answer.ProductionApply.Total
	return going.Read{
		Facts: map[string]any{"view": goingDeliveryPipeline, "days": answer.Days},
		Tables: map[string][]map[string]any{
			"pr_checks":    days(answer.PRChecks.Run.Days),
			"production":   days(answer.ProductionApply.Days),
			"failing_jobs": failing,
			"slowest_jobs": slowest,
			"totals": {{
				"pr_checks_runs": checks.N, "pr_checks_success_rate": checks.SuccessRate, "pr_checks_median_minutes": checks.MedianMinutes,
				"production_runs": production.N, "production_success_rate": production.SuccessRate,
				"production_median_minutes":          production.MedianMinutes,
				"merge_to_production_median_minutes": answer.MergeToProduction.Total.MedianMinutes,
			}},
		},
	}
}

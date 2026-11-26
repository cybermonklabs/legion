package api

// going_reads.go holds the reads a command-bar run's tools make beside the search (R12), each one
// a read a route of this table serves and made through the same functions that route answers
// with, in process, for the run's author: GET /api/v1/agents, GET /api/v1/issues and
// an issue's progress, GET /api/v1/projects/{key}/architecture and /activity, and the three
// delivery reads. A block's spec names one of them, and the run fills the block by making that
// read again (going_blocks.go). The tools themselves - their names, what the model is told and
// their budget - are going's (going/reads.go).

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The answer run's read tools are the entries of goingReadTools (going_tools.go) that no tool
// spec states, by going's names for them: each names the rows of the route table it reads
// through, which bind the arguments of going's spec of the read, as the bar's agent reads them. A
// caller is offered a tool only where its credential may call every row the tool reads
// (goingReadsFor), as a caller is offered an action only where it may call the action's rows, and
// register refuses a table that does not serve every row goingReadTools names.

// goingDeploymentReads are the reads that are no one project's: the roster names every
// project's sessions and the delivery reads every project's pull requests and issues. A
// credential scoped to one project is offered neither. When the question names no project,
// its reads use the project the model names, but only where access.CredentialCovers permits it:
// its own project or a public one. An uncovered private project and a missing project have the
// same refusal. A question asked of a project limits these reads to that project alone
// (goingReadRun.project).
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

// goingReadRoutesOf is the answer run's read tools' rows as the table mounts them, and an error
// naming everything goingReadTools holds that the table and the specs refuse: a row the table
// does not serve, a row that binds a placeholder to no argument of its tool or leaves one unbound
// (goingBindingProblems), a dispatch tool no spec states, a read of the answer run's that going
// names none of (going.ReadSpecs), whose arguments its rows bind, and a tool whose entry and spec
// disagree on its rows (goingToolRows).
func goingReadRoutesOf(routes []apiRoute) (map[string][]GoingRoute, error) {
	served := make(map[string]apiRoute, len(routes))
	for _, route := range routes {
		served[route.Method+" "+route.Pattern] = route
	}
	specs := make(map[string]contracts.DispatchTool, len(contracts.DispatchTools))
	for _, spec := range contracts.DispatchTools {
		specs[spec.Name] = spec
	}
	// The arguments of each of the answer run's reads: the properties of its schema.
	readArguments := map[string]map[string]bool{}
	for _, read := range going.ReadSpecs() {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(read.Schema, &schema); err != nil {
			return nil, fmt.Errorf("the schema of going's read %s: %w", read.Name, err)
		}
		takes := make(map[string]bool, len(schema.Properties))
		for name := range schema.Properties {
			takes[name] = true
		}
		readArguments[read.Name] = takes
	}
	reads := make(map[string][]GoingRoute, len(goingReadTools))
	var refusals []string
	for tool, entry := range goingReadTools {
		spec, stated := specs[tool]
		rows := entry.Routes
		takes := make(map[string]bool, len(spec.Arguments))
		for _, argument := range spec.Arguments {
			takes[argument.Name] = true
		}
		switch {
		case stated:
			var agree bool
			if rows, agree = goingToolRows(spec, entry.Routes, nil); !agree {
				refusals = append(refusals, tool+": its rows are stated by its entry and its spec's routes both, or by neither")
			}
		case strings.HasPrefix(tool, "dispatch_"):
			refusals = append(refusals, tool+": no dispatch tool of that name (packages/contracts/src/dispatch-tools.ts)")
		default:
			read, named := readArguments[tool]
			if !named {
				refusals = append(refusals, tool+": no read of going's of that name (going/reads.go)")
			}
			takes = read
		}
		for _, row := range rows {
			name := row.Method + " " + row.Pattern
			mounted, isServed := served[name]
			if !isServed {
				refusals = append(refusals, fmt.Sprintf("%s: %s is no row of the route table", tool, name))
				continue
			}
			refusals = append(refusals, goingBindingProblems(tool, row, takes)...)
			if stated {
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

// goingRosterFrame is the roster frame's data (contracts GoingRosterEvent): how old
// Dispatch's copy of the listener's session list was when the run read it, and whether it was that
// live copy or, the listener being unavailable, the sessions Dispatch's own rows name.
type goingRosterFrame struct {
	AgeMS  int64  `json:"age_ms"`
	Source string `json:"source"`
}

// goingReadRun is one run's reads: the project the run was asked of, which every project read is
// held to, the page's context chip, which names what a read that names nothing reads, the run's
// author, the person each read is made as, and the roster the run last read, for its roster frame.
type goingReadRun struct {
	s      *server
	asked  string
	chip   *goingContext
	author model.Actor
	funcs  map[string]going.ReadFunc

	mu     sync.Mutex
	roster *goingRosterFrame
}

// goingRunReads is the reads of one run, each read tool its caller may call.
func (s *server) goingRunReads(request goingRequest, start goingRunStart) *goingReadRun {
	run := &goingReadRun{s: s, asked: request.Project, chip: start.chip, author: request.Author}
	every := map[string]going.ReadFunc{
		going.ReadRoster:          run.readRoster,
		going.ReadProgress:        run.scopedRead(run.readProgress),
		going.ReadArchitecture:    run.scopedRead(run.readArchitecture),
		going.ReadProjectActivity: run.scopedRead(run.readProjectActivity),
		going.ReadDelivery:        run.readDelivery,
	}
	run.funcs = make(map[string]going.ReadFunc, len(start.reads))
	for _, tool := range start.reads {
		if deref(run.author.Project) != "" && goingDeploymentReads[tool] {
			continue
		}
		if read, known := every[tool]; known {
			run.funcs[tool] = read
		}
	}
	return run
}

// scopedRead checks visibility and reads the data in one snapshot. A project that becomes
// private after that check cannot add new data to the answer.
func (g *goingReadRun) scopedRead(read func(context.Context, queryer, json.RawMessage) (going.Read, error)) going.ReadFunc {
	return func(ctx context.Context, arguments json.RawMessage) (going.Read, error) {
		pool := g.s.deps.Store.Pool
		if deref(g.author.Project) == "" {
			return read(ctx, pool, arguments)
		}
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return going.Read{}, err
		}
		defer tx.Rollback(context.WithoutCancel(ctx))
		return read(ctx, tx, arguments)
	}
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
// A run asked of a project reads that project alone. With no project asked of, the run reads
// what the model names or falls back to the page. For a scoped author, the selected project
// must pass access.CredentialCovers (its own project or a public one). The read uses the same
// refusal for an uncovered private project and a missing one (createGoingRequest).
func (g *goingReadRun) project(ctx context.Context, q queryer, named string) (string, error) {
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
	if scope := deref(g.author.Project); scope != "" {
		if err := goingAllowProject(ctx, q, scope, project); err != nil {
			return "", err
		}
	}
	return project, nil
}

func goingAllowProject(ctx context.Context, q queryer, scope, project string) error {
	private, found, err := projectVisibility(ctx, q, project)
	if err != nil {
		return err
	}
	if !found || !access.CredentialCovers(access.Credential{Listed: []string{scope}}, project, private) {
		return goingReadError(errProjectNotFound())
	}
	return nil
}

// goingProjectCoveredSQL is CredentialCovers with Listed set to the run author's one
// project, for reads that filter a set of rows. A scoped run does not read rows without a
// project. An empty or null scope leaves the statement unrestricted.
func goingProjectCoveredSQL(project, scope string) string {
	return `(coalesce(` + scope + `::text, '') = '' or ` + project + ` in (
		select key from projects where key = ` + scope + ` or visibility <> 'private'
	))`
}

// goingPageArguments is arguments, a call of the bar's agent to tool, with the issue or the project
// the run's page names (going_runs.context, the bar's context chip) where the call names neither,
// as the answer run's reads read them (readProgress, goingReadRun.project): read_progress reads
// the page's issue, else its project, and read_architecture and read_project_activity its project.
// A call that names its item, a call of any other tool, and a run whose page names nothing keep
// their arguments, and the row such a call reads refuses what still names nothing.
func (s *server) goingPageArguments(ctx context.Context, run, tool string, arguments map[string]any) (map[string]any, error) {
	named := func(argument string) bool {
		return strings.TrimSpace(goingText(arguments, argument)) != ""
	}
	switch tool {
	case going.ReadProgress:
		if named("issue") || named("project") {
			return arguments, nil
		}
	case going.ReadArchitecture, going.ReadProjectActivity:
		if named("project") {
			return arguments, nil
		}
	default:
		return arguments, nil
	}
	var chip []byte
	if err := s.deps.Store.Pool.QueryRow(ctx, `select context from going_runs where id = $1::uuid`, run).Scan(&chip); err != nil {
		return nil, fmt.Errorf("read the page of run %s: %w", run, err)
	}
	var page goingContext
	if len(chip) > 0 {
		if err := json.Unmarshal(chip, &page); err != nil {
			return nil, fmt.Errorf("read the page of run %s: %w", run, err)
		}
	}
	// A copy of the call's arguments, which the page's issue or project is added to; a call with
	// no arguments gets a map of its own.
	filled := maps.Clone(arguments)
	if filled == nil {
		filled = map[string]any{}
	}
	switch {
	case tool == going.ReadProgress && page.Issue != "":
		filled["issue"] = page.Issue
	case page.Project != "":
		filled["project"] = page.Project
	}
	return filled, nil
}

// decodeGoingArguments decodes a read's arguments into value, refusing a field it does not take.
func decodeGoingArguments(arguments json.RawMessage, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
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

// readRoster is read_roster: GET /api/v1/agents' rows, each session with the delivery modes it
// takes (capabilities), its open asks, its last activity and the claims it holds, then each
// session that holds a claim the listener no longer lists, marked gone (goingRosterClaims), and
// the age of Dispatch's copy of the listener's list. It lists at most goingReadSessionMax
// sessions, the gone ones kept first (goingRosterCut), and its totals count every session, the
// gone ones among them. A listener that is refilling its list is waited out for the Retry-After
// it sent while the run's window leaves room to answer after it, then read again; a listener that
// is unavailable, or still refilling once the window leaves no room, leaves the sessions
// Dispatch's own rows name, marked stored (R12), which name no delivery modes and judge no
// session gone.
func (g *goingReadRun) readRoster(ctx context.Context, arguments json.RawMessage) (going.Read, error) {
	if err := decodeGoingArguments(arguments, &struct{}{}); err != nil {
		return going.Read{}, err
	}
	listed, frame, err := g.s.goingRoster(ctx)
	if err != nil {
		return going.Read{}, err
	}
	g.mu.Lock()
	g.roster = &frame
	g.mu.Unlock()
	// The live rows are the listener's whole list, so a holder none of them names is one it lists
	// no more; the stored rows are no listener's.
	var lists func(string) bool
	if frame.Source == goingRosterLive {
		lists = func(string) bool { return false }
	}
	rows, gone, err := g.s.goingRosterClaims(ctx, g.author, listed, lists)
	if err != nil {
		return going.Read{}, err
	}

	live, unresponsive, openAsks := 0, 0, 0
	for _, row := range rows {
		switch row.State {
		case envoy.StateLive:
			live++
		case envoy.StateUnresponsive:
			unresponsive++
		}
		openAsks += row.OpenAsks
	}
	kept := goingRosterCut(rows, gone, goingReadSessionMax)
	sessions := make([]map[string]any, 0, len(kept))
	for _, row := range kept {
		session := map[string]any{
			going.FieldSessionID: row.SessionID, going.FieldTitle: row.Title, going.FieldState: row.State, "roles": row.Roles, "capabilities": row.Capabilities,
			going.FieldOpenAsks: row.OpenAsks, going.FieldLastActivity: row.LastActivity,
		}
		// Where the session's last activity is, where it has one: the issue and the type of
		// its newest event (agentRows).
		if row.LastActivityType != nil {
			session["last_activity_issue"], session["last_activity_type"] = row.LastActivityIssue, *row.LastActivityType
		}
		if len(row.Claims) > 0 {
			session["claims"] = row.Claims
		}
		sessions = append(sessions, session)
	}
	return going.Read{
		Facts: map[string]any{"source": frame.Source, "age_ms": frame.AgeMS},
		Tables: map[string][]map[string]any{
			going.TableSessions: sessions,
			"totals":            {{"sessions": len(rows), "live": live, "unresponsive": unresponsive, "gone": gone, "open_asks": openAsks}},
		},
	}, nil
}

// goingRosterClaim is one claim a roster row names: the open issue its session holds, and when the
// claim was taken (issues.claimed_at, which the store holds beside every claim).
type goingRosterClaim struct {
	Issue string `json:"issue"`
	At    string `json:"at"`
}

// goingRosterRow is one session of the bar's roster read: GET /api/v1/agents' row, and the claims
// the session holds on open issues, none for a session that holds none. LastSeen stands over the
// row's own last_seen: the listener's for a session it lists, and none for a gone session, which
// no listener lists, so that row names no time rather than a zero.
type goingRosterRow struct {
	agentResponse
	LastSeen *int64             `json:"last_seen,omitempty"`
	Claims   []goingRosterClaim `json:"claims,omitempty"`
}

// goingSessionClaimsQuery is every claim a session holds on an open issue the reader bound at $1
// may read, by the predicate every read of an issue for a person binds (refs.VisibleKind, as the
// bar's own read of an answer's issue chips does, goingAnswerIssueQuery): the holder, the issue
// and when the claim was taken, the newest first.
var goingSessionClaimsQuery = `
	select i.claimed_by->>'id', i.key, i.claimed_at
	from issues i
	where i.closed_at is null and i.claimed_by->>'kind' = 'session' and coalesce(i.claimed_by->>'id', '') <> ''
		and ` + refs.VisibleKind("issue", "i.key", "$1") + `
	order by i.claimed_at desc, i.key`

// goingRosterClaims is listed, a roster's rows, each with the claims its session holds on open
// issues reader may read, then, where lists says which sessions the listener lists, a row for each
// other session that holds one: a session the listener lists no more is gone (routeRoster.state)
// and still holds its claims, so its row says gone, with its claims, the title it last stamped on
// a write, its open asks and its last activity, as a stored row does (unlistedAgentRows), the
// newest claim first. A claim on an issue reader may not read names neither its issue nor, where
// it is the session's only one, the gone session (R12: the agent reads nothing its person cannot).
// A nil lists judges no session gone: the stored rows are no listener's, and already name every
// holder (goingStoredSessionsQuery). It answers how many of the rows, the last ones, are gone.
func (s *server) goingRosterClaims(ctx context.Context, reader model.Actor, listed []agentResponse, lists func(sessionID string) bool) ([]goingRosterRow, int, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, goingSessionClaimsQuery, refs.VisibilityArg(reader))
	if err != nil {
		return nil, 0, fmt.Errorf("query the sessions' claims: %w", err)
	}
	defer rows.Close()
	claims := map[string][]goingRosterClaim{}
	var holders []string
	for rows.Next() {
		var holder, issue string
		var at time.Time
		if err := rows.Scan(&holder, &issue, &at); err != nil {
			return nil, 0, fmt.Errorf("scan the sessions' claims: %w", err)
		}
		if _, held := claims[holder]; !held {
			holders = append(holders, holder)
		}
		claims[holder] = append(claims[holder], goingRosterClaim{Issue: issue, At: timestampValue(at)})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate the sessions' claims: %w", err)
	}

	roster := make([]goingRosterRow, 0, len(listed))
	named := make(map[string]bool, len(listed))
	for _, row := range listed {
		named[row.SessionID] = true
		seen := row.LastSeen
		roster = append(roster, goingRosterRow{agentResponse: row, LastSeen: &seen, Claims: claims[row.SessionID]})
	}
	if lists == nil {
		return roster, 0, nil
	}
	gone := slices.DeleteFunc(holders, func(holder string) bool { return named[holder] || lists(holder) })
	if len(gone) == 0 {
		return roster, 0, nil
	}
	goneRows, _, err := s.unlistedAgentRows(ctx, gone, envoy.StateGone)
	if err != nil {
		return nil, 0, err
	}
	for _, row := range goneRows {
		roster = append(roster, goingRosterRow{agentResponse: row, Claims: claims[row.SessionID]})
	}
	return roster, len(goneRows), nil
}

// goingRosterCut is rows cut to most, where the last gone of rows are gone sessions
// (goingRosterClaims): the gone rows are kept before the others, since a claim no session works
// on is what a reader of the roster most needs to see, and each part keeps its order.
func goingRosterCut(rows []goingRosterRow, gone, most int) []goingRosterRow {
	if len(rows) <= most {
		return rows
	}
	listed, goneKept := len(rows)-gone, min(gone, most)
	return slices.Concat(rows[:min(listed, most-goneKept)], rows[listed:listed+goneKept])
}

// goingRosterAnswer is the bar agent's roster read as the model is told it: how many sessions the
// roster holds - every one the listener lists and each gone one that holds a claim - and the rows
// of at most goingReadSessionMax of them, so the total its result states (the agent's resultCount)
// is the roster's whole count, as the answer run's read counts it (readRoster's totals), and not
// the rows the bound left.
type goingRosterAnswer struct {
	Total    int              `json:"total"`
	Sessions []goingRosterRow `json:"sessions"`
}

// goingRosterTold is the bar agent's roster read as the model is told it (goingRosterAnswer): the
// answer of GET /api/v1/agents of the goingReadSessionMax sessions seen last
// (goingTranslateRoster), each row with the claims its session holds on the issues login, the run's
// person, may read, then the gone sessions that hold one (goingRosterClaims), judged against
// Dispatch's copy of the listener's list that answer was read from, at most goingReadSessionMax
// rows in all, the gone ones kept first (goingRosterCut), and the total of every session that copy
// lists and every gone one. The route's own answer stays as every other caller reads it. Any
// answer but a whole 200 is told as the route answered it, and a copy that answers no list once
// the route answered fails the read, since no count of every session then stands.
func (s *server) goingRosterTold(ctx context.Context, login string, answer agentwire.CallAnswer) (agentwire.CallAnswer, error) {
	if answer.Status != http.StatusOK || answer.Cut {
		return answer, nil
	}
	var listed []agentResponse
	if err := json.Unmarshal(answer.Body, &listed); err != nil {
		return agentwire.CallAnswer{}, fmt.Errorf("read the agents list: %w", err)
	}
	roster, err := s.agentRoster(ctx)
	if err != nil {
		return agentwire.CallAnswer{}, err
	}
	if !roster.answered {
		return agentwire.CallAnswer{}, errors.New("the listener's session list answered the agents list and then answered none, so no count of every session stands")
	}
	lists := func(sessionID string) bool { return roster.state(sessionID) != envoy.StateGone }
	rows, gone, err := s.goingRosterClaims(ctx, model.Actor{Kind: "user", ID: login}, listed, lists)
	if err != nil {
		return agentwire.CallAnswer{}, err
	}
	// The copy read here may be a newer one than the route read, so the total is never fewer than
	// the sessions the route listed.
	total := max(len(roster.sessions), len(listed)) + gone
	body, err := json.Marshal(goingRosterAnswer{Total: total, Sessions: goingRosterCut(rows, gone, goingReadSessionMax)})
	if err != nil {
		return agentwire.CallAnswer{}, err
	}
	answer.Body = body
	return answer, nil
}

// goingRoster is the roster a run reads and its frame: GET /api/v1/agents' rows from Dispatch's
// copy of the listener's list while the listener answers, aged by the database's clock, and the
// stored rows otherwise (readRoster says when).
func (s *server) goingRoster(ctx context.Context) ([]agentResponse, goingRosterFrame, error) {
	for {
		roster, err := s.agentRoster(ctx)
		if err != nil {
			return nil, goingRosterFrame{}, err
		}
		if roster.answered {
			rows, err := s.agentRows(ctx, roster.sessions, 0)
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
// goingStoredSessionsQuery names (unlistedAgentRows), the newest activity first. Its state is
// unknown, since only the listener knows whether a session is running.
func (s *server) storedAgentRows(ctx context.Context) ([]agentResponse, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, goingStoredSessionsQuery)
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
	agents, activity, err := s.unlistedAgentRows(ctx, sessionIDs, "unknown")
	if err != nil {
		return nil, err
	}
	sort.SliceStable(agents, func(left, right int) bool {
		leftAt, rightAt := activity[agents[left].SessionID].at, activity[agents[right].SessionID].at
		if !leftAt.Equal(rightAt) {
			return leftAt.After(rightAt)
		}
		return agents[left].SessionID < agents[right].SessionID
	})
	return agents, nil
}

// unlistedAgentRows is a row in state for each of sessionIDs, sessions no listener row describes,
// in sessionIDs' order: the newest title each stamped on a write, its open asks and its last
// activity, with no roles and no delivery modes; and the activity it read, by session.
func (s *server) unlistedAgentRows(ctx context.Context, sessionIDs []string, state string) ([]agentResponse, map[string]agentActivity, error) {
	openAsks, err := s.agentOpenAskCounts(ctx, sessionIDs)
	if err != nil {
		return nil, nil, err
	}
	activity, err := s.agentLastActivity(ctx, sessionIDs)
	if err != nil {
		return nil, nil, err
	}
	titles, err := delivery.ResolveSessionTitles(ctx, s.deps.Store.Pool, sessionIDs)
	if err != nil {
		return nil, nil, err
	}
	agents := make([]agentResponse, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		row := agentResponse{OpenAsks: openAsks[id]}
		row.SessionID, row.Title, row.State = id, titles[id], state
		row.Roles, row.Capabilities = []string{}, []string{}
		if newest, active := activity[id]; active {
			row.LastActivity, row.LastActivityIssue, row.LastActivityType = timestampPtr(&newest.at), newest.issue, &newest.kind
		}
		agents = append(agents, row)
	}
	return agents, activity, nil
}

// readProgress is read_progress: an issue's task and child progress with its children, as GET
// /api/v1/issues and the issue read carry them, or a project's issues by status with its open
// issues. With neither named, the page's issue is read, else the run's or the page's project.
func (g *goingReadRun) readProgress(ctx context.Context, q queryer, arguments json.RawMessage) (going.Read, error) {
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
		return g.issueProgress(ctx, q, issue)
	}
	project, err := g.project(ctx, q, named.Project)
	if err != nil {
		return going.Read{}, err
	}
	return g.projectProgress(ctx, q, project)
}

// issueProgress is one issue's progress: its task and child counts, its children by status, and
// its children in the listing's order.
func (g *goingReadRun) issueProgress(ctx context.Context, q queryer, key string) (going.Read, error) {
	if !issueKeyPattern.MatchString(key) {
		return going.Read{}, going.Refuse("issue must be an issue key such as DEMO-12, and %q is none", key)
	}
	scope := deref(g.author.Project)
	if scope != "" {
		project, private, err := issueProject(ctx, q, key)
		if err != nil {
			return going.Read{}, err
		}
		if project == "" || !access.CredentialCovers(access.Credential{Listed: []string{scope}}, project, private) {
			return going.Read{}, goingReadError(errProjectNotFound())
		}
	}
	s := g.s
	issue, progress, err := s.loadIssueWithProgress(ctx, q, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return going.Read{}, going.Refuse("Dispatch holds no issue %s", key)
	}
	if err != nil {
		return going.Read{}, err
	}
	if g.asked != "" && issue.Project != g.asked {
		return going.Read{}, going.Refuse("this question was asked of project %s, and %s is an issue of %s", g.asked, key, issue.Project)
	}
	// An issue may have children in other projects. Scope their rows before pagination
	// and derive the progress bar from their filtered counts, not the issue loader's totals.
	childProject := ""
	if scope != "" {
		childProject = g.asked
	}
	children, _, err := goingIssuePage(ctx, q, childProject, key, false, scope)
	if err != nil {
		return going.Read{}, err
	}
	statuses, err := goingStatusCounts(ctx, q, childProject, key, scope)
	if err != nil {
		return going.Read{}, err
	}
	if scope != "" {
		count := model.ProgressCount{}
		for _, row := range statuses {
			n := row["count"].(int)
			count.Total += n
			if row["status"] == "done" {
				count.Done = n
			}
		}
		progress.Children = nil
		if count.Total > 0 {
			progress.Children = &count
		}
	}
	bars := []map[string]any{}
	if progress.Tasks != nil {
		bars = append(bars, map[string]any{going.FieldLabel: "tasks", going.FieldDone: progress.Tasks.Done, going.FieldTotal: progress.Tasks.Total})
	}
	if progress.Children != nil {
		bars = append(bars, map[string]any{going.FieldLabel: "children", going.FieldDone: progress.Children.Done, going.FieldTotal: progress.Children.Total})
	}
	items := append([]going.Hit{issueHit(issue.Key, issue.Title)}, issueHits(children)...)
	return going.Read{
		Facts: map[string]any{"issue": issue.Key, "title": issue.Title, "status": issue.Status, "project": issue.Project, "href": "/issues/" + issue.Key},
		Tables: map[string][]map[string]any{
			going.TableProgress: bars,
			"statuses":          statuses,
			going.TableIssues:   issueRows(children),
		},
		Items: items,
	}, nil
}

// projectProgress is one project's progress: its issues done of all, its issues by status, and
// its open issues in the listing's order.
func (g *goingReadRun) projectProgress(ctx context.Context, q queryer, project string) (going.Read, error) {
	if _, err := g.s.loadProject(ctx, q, project); err != nil {
		return going.Read{}, goingReadError(err)
	}
	statuses, err := goingStatusCounts(ctx, q, project, "", "")
	if err != nil {
		return going.Read{}, err
	}
	open, openTotal, err := goingIssuePage(ctx, q, project, "", true, "")
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
			going.TableProgress: {{going.FieldLabel: "issues", going.FieldDone: done, going.FieldTotal: total}},
			"statuses":          statuses,
			going.TableIssues:   issueRows(open),
		},
		Items: issueHits(open),
	}, nil
}

// goingIssuePage is GET /api/v1/issues' first page of goingReadIssueMax issues, in its order, for
// one project or one parent's children, open issues alone when open is set, with the count of
// every issue the filters match: the listing's own statement and scan.
//
// The arguments follow listIssues (issue_queries.go): $1 project, $2 statuses, $3 parent,
// $4 updated-since, $5 labels, $6 open, $7 priorities, $8 unset priority, $9/$10 closed window,
// $11 advances ids, $12/$13 none/any advances, $14 moved, $15 own, $16/$17 label case-folding
// maps, $18/$19 page limit and offset, and $20 the scope for a scoped child listing.
func goingIssuePage(ctx context.Context, q queryer, project, parent string, open bool, scope string) ([]model.IssueSummary, int, error) {
	arguments := []any{project, []string{}, parent, store.MicrosecondCeiling(nil), []string{}, open, []int16{}, false,
		store.MicrosecondCeiling(nil), store.MicrosecondCeiling(nil), []string{}, false, false, false, false, "", "", goingReadIssueMax, 0}
	if scope != "" {
		return scanIssuePage(ctx, q, goingCoveredIssuePageQuery, append(arguments, scope))
	}
	return scanIssuePage(ctx, q, listIssuesPageQuery, arguments)
}

// goingStatusCountsQuery counts issues by status under the project and parent filters ($1 and
// $2, empty for none) and the author's project scope ($3, empty for an unrestricted count).
var goingStatusCountsQuery = `
	select i.status, count(*)::int
	from issues i
	where ($1 = '' or i.project_key = $1) and ($2 = '' or i.parent_key = $2)
		and ` + goingProjectCoveredSQL("i.project_key", "$3") + `
	group by i.status`

// goingStatusCounts is the issues of a project or a parent counted by status, in the lifecycle's
// order, each status that holds none left out.
func goingStatusCounts(ctx context.Context, q queryer, project, parent, scope string) ([]map[string]any, error) {
	rows, err := q.Query(ctx, goingStatusCountsQuery, project, parent, scope)
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
		rows = append(rows, map[string]any{going.FieldKey: issue.Key, going.FieldTitle: issue.Title, going.FieldStatus: issue.Status, "priority": issue.Priority})
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
func (g *goingReadRun) readArchitecture(ctx context.Context, q queryer, arguments json.RawMessage) (going.Read, error) {
	var named struct {
		Project string `json:"project"`
	}
	if err := decodeGoingArguments(arguments, &named); err != nil {
		return going.Read{}, err
	}
	project, err := g.project(ctx, q, named.Project)
	if err != nil {
		return going.Read{}, err
	}
	tree, err := architectureTree(ctx, q, project)
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
func (g *goingReadRun) readProjectActivity(ctx context.Context, q queryer, arguments json.RawMessage) (going.Read, error) {
	var named struct {
		Project string `json:"project"`
		Weeks   *int   `json:"weeks"`
	}
	if err := decodeGoingArguments(arguments, &named); err != nil {
		return going.Read{}, err
	}
	project, err := g.project(ctx, q, named.Project)
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
	activity, err := g.s.projectActivity(ctx, q, project, weeks)
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
			going.FieldAt: pr.MergedAt.UTC().Format(time.RFC3339), going.FieldLane: "merge", going.FieldLabel: fmt.Sprintf("%s#%d %s", pr.Repo, pr.Number, pr.Title),
		})
	}
	for _, run := range answer.Runs {
		conclusion := "running"
		if run.Conclusion != nil {
			conclusion = string(*run.Conclusion)
		}
		events = append(events, map[string]any{
			going.FieldAt: run.StartedAt.UTC().Format(time.RFC3339), going.FieldLane: "deploy", going.FieldLabel: fmt.Sprintf("run %d: %s", run.ID, conclusion),
		})
	}
	sort.SliceStable(events, func(left, right int) bool {
		return events[left][going.FieldAt].(string) < events[right][going.FieldAt].(string)
	})
	merges, runs := 0, len(answer.Runs)
	for _, event := range events {
		if event[going.FieldLane] == "merge" {
			merges++
		}
	}
	if len(events) > goingReadEventMax {
		events = events[len(events)-goingReadEventMax:]
	}
	return going.Read{
		Facts: facts,
		Tables: map[string][]map[string]any{
			going.TableEvents: events,
			"totals":          {{"merges": merges, "runs": runs}},
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

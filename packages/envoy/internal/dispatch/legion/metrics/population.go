// Package metrics computes the four Legion delivery metrics — work time to merge, phase time by
// worker type, rework, and splitting and follow-ups (dispatch://INFRA-1077, the spec's "The four
// Legion metrics" and "Rule details"). No route serves them (compute.go's Report): it is here for
// the evals of dispatch://INFRA-2916 to call, and only tests call it today.
//
// It is split the way api/delivery_pipeline.go is split: Read makes every database and GitHub read
// one answer needs, and Compute turns that population into the answer and touches nothing.
//
// Two disciplines run through it:
//
//   - The daemon's state is read loosely. What Dispatch holds of it is legion_observations, whose
//     fields are legion/state.go's; a field a daemon never sent is a field this package has no
//     observation of, never an assumption, and one it stopped sending has a `null` observation
//     from the read that no longer carried it. An issue's tree comes from the daemon's own `tree`
//     where a read carried one (dispatch://INFRA-980 adds it) and from the parent chain
//     otherwise, so nothing here takes a slot holder for a root.
//   - The precedence of the phase-time segments lives in exactly one ordered list, segments.go's
//     `precedence`, and each segment is one function.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// askTurnEventTypes are the ask events a person's turn starts and ends with: the two that hand an
// ask to a person and the two that take it back (api/events.go names the same family).
var askTurnEventTypes = []string{"ask.opened", "ask.handed_back", "ask.answered", "ask.resolved"}

// fetchedMergeTimesCap bounds how many pull requests one answer fetches from GitHub for a merge
// time Dispatch has not already stored. Every finished issue whose pull request the delivery
// intake has reconciled reads from delivery_pull_requests; this is the tail of issues closed since
// the last reconcile, and a window that names hundreds of them is a backfill, not a dashboard.
const fetchedMergeTimesCap = 50

// fetchedMergeTimesConcurrency bounds how many of those fetches are in flight at once. Each one
// mints an installation token and makes two GitHub calls, so a serial pass over the cap is up to
// 150 round trips inside one request; the bound is the one the broadcast resolver holds its own
// fan-out to (api/broadcasts.go's broadcastResolveConcurrency).
const fetchedMergeTimesConcurrency = 8

// Mark is one value of one field as of a moment: an observation the sampler recorded, or a status
// an event snapshotted. The value of an observation is the JSON text legion_observations holds; a
// status's is the status itself.
type Mark struct {
	At    time.Time
	Value string
}

// Handoff is one handoffs row: which phase's handoff, which version, and when it was recorded.
type Handoff struct {
	Phase   string
	Version int
	At      time.Time
}

// RunPlan is the plan one run of the issue ran from, as Dispatch holds it (workflowstore's plan
// node): when its node was made, which is when the run's work started, and the roles of its one
// task's steps, in the task's order (`acceptance` for `acceptance:owner`). Steps is empty for a
// plan whose tasks are not exactly one, which the fallback timeline reads as the default steps.
type RunPlan struct {
	Made  time.Time
	Steps []string
}

// Issue is one issue of the population with every fact the rules read of it.
type Issue struct {
	Key       string
	Title     string
	Priority  *int
	Parent    string
	CreatedAt time.Time
	ClosedAt  *time.Time

	// Tree is the issue's tree root as the daemon named it, from the `tree` field
	// dispatch://INFRA-980 adds to each issue of `GET /legion/v1/state`, where a read carried
	// one. Every issue of the project carries the newest one the sampler recorded, whether or
	// not the window's observation read covered the issue (readObservations, fillReportedTrees).
	// Empty is "no read named one", which is every daemon before that change: TreeRoot then
	// walks the parent chain. Nothing reads a slot to decide a root.
	Tree string

	// Children are the issues of the population whose tree root is this issue, this issue aside.
	Children []string

	// FirstRoleEvent is the first event of a session titled `Legion <role> · <this issue's key>`,
	// by role: what the phase fallback starts a role's phase at, and what makes a closed issue
	// one that finished with Legion phase work.
	FirstRoleEvent map[string]time.Time

	// Handoffs are the issue's handoff records, oldest first.
	Handoffs []Handoff

	// StatusMarks is every status the issue's own events snapshotted, oldest first.
	StatusMarks []Mark

	// Plans is the plan of each run of the issue, oldest first: what the fallback timeline reads
	// the order of the task's steps from (phaseLine). An issue Legion ran before it recorded
	// plans has none.
	Plans []RunPlan

	// Observed is every value a read found changed, by legion_observations field (legion's
	// FieldPhase, FieldSlot, ClaimFieldPrefix+role and the rest), oldest first within a field.
	Observed map[string][]Mark

	// AskTurns are the spans in which an ask a Legion session of this issue's tree opened on
	// this issue stood on a person's turn, from its opening or hand-back to its answer or
	// resolution, and to the issue's end where neither came (keepTreeAsks).
	AskTurns []Span

	// MergedAt is the merge time of the pull request Dispatch holds for the issue, where it holds
	// one.
	MergedAt *time.Time
}

// Filed is one issue a Legion session created, as metric 4 classes it: the issue, the moment, and
// the session's own title parsed into the role and issue (or controller) that filed it. The parent
// and ancestors are the ones the issue had at its `issue.created` event, not the ones it has now.
// The class is read off the parent alone, by walking the chain above it: Dispatch writes an empty
// `ancestors` for an issue filed under a child, so the list is recorded as the event carried it
// and no rule turns on it (the spec's "Rule details — The tree of a filed issue").
type Filed struct {
	Key       string
	Title     string
	At        time.Time
	Session   string
	Filer     legion.SessionTitle
	Parent    string
	Ancestors []string
}

// Population is every fact one metrics answer is computed from.
type Population struct {
	Project string
	From    time.Time
	To      time.Time

	// Issues is every issue of the project the read touched, by key; Keys lists them in order so
	// a pass over them is deterministic.
	Issues map[string]*Issue
	Keys   []string

	// Filed is every issue a Legion session created in the window, in creation order.
	Filed []Filed

	// reported is legion.ReadCurrent's Reported (readReported), and phaseWork is every issue that
	// carries Legion phase work, over the whole log of every project (legion.PhaseWorkKeys): the
	// two halves of legion.TreeNode's rule, neither of them read for the window's keys.
	reported  map[string]string
	phaseWork map[string]struct{}

	// askTurns is every ask turn the read gathered, before the tree rule picked the ones that
	// count for the issue they were opened on. The rule reads each issue's tree, which the
	// observations of the same read name, so it runs once every read has landed (keepTreeAsks).
	askTurns []askTurn
}

// Read is every database and GitHub read one metrics answer makes: the project's issues, the
// Legion phase work the event log records — for every issue the log holds, this project's among
// them, since the read takes no project and the metrics look up their own, which is what places
// an issue in a tree, and in the window's scope per role, which is what a metric counts — the
// handoffs and observations of the window's issues, the issues of the project the daemon
// reports, the issues Legion sessions created in the window, and the merge time of each finished
// issue's pull request.
//
// github may be nil, and a GitHub read that fails is not an error: a merge time Dispatch cannot
// reach leaves that issue with none, since no metric's rule is written in terms of one.
func Read(ctx context.Context, pool *store.Pool, github *githubapp.Client, project string, from, to time.Time) (*Population, error) {
	population := &Population{Project: project, From: from, To: to, Issues: map[string]*Issue{}}
	if err := population.readIssues(ctx, pool, project); err != nil {
		return nil, err
	}
	if err := population.readFiled(ctx, pool, project, from, to); err != nil {
		return nil, err
	}
	keys := population.scopeKeys(from, to)
	// The eight reads below write disjoint fields — six take the keys above and write fields of
	// those issues, readSampled the window's observations and the project's reported set, and
	// readPhaseWork the phase-work keys — so they run together rather than one after another, as
	// the delivery population's independent reads do (api/delivery.go). errgroup answers the first
	// error recorded, so which of two failing reads names the failure is no longer fixed.
	independent := []func(ctx context.Context) error{
		func(ctx context.Context) error { return population.readRoleEvents(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readStatusMarks(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readAskTurns(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readHandoffs(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readPlans(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readSampled(ctx, pool, project, keys) },
		func(ctx context.Context) error { return population.readMergeTimes(ctx, pool, github, keys) },
		func(ctx context.Context) error { return population.readPhaseWork(ctx, pool) },
	}
	reads, readsCtx := errgroup.WithContext(ctx)
	read := func(fn func(ctx context.Context) error) error {
		readCtx, err := store.ForConcurrentRead(readsCtx)
		if err != nil {
			return err
		}
		reads.Go(func() error { return fn(readCtx) })
		return nil
	}
	for _, fn := range independent {
		if err := read(fn); err != nil {
			_ = reads.Wait()
			return nil, err
		}
	}
	if err := reads.Wait(); err != nil {
		return nil, err
	}
	population.fillReportedTrees()
	population.linkTrees()
	population.keepTreeAsks()
	return population, nil
}

// readIssues reads the project's issues. It reads all of them rather than the window's: the window
// names which issues finished, while the rules read their whole trees (a child that finished in
// the window belongs to a root that may not have) and each issue's whole life (the work time runs
// from its first worker event, which is older than the window whenever the issue is).
func (p *Population) readIssues(ctx context.Context, pool *store.Pool, project string) error {
	rows, err := pool.Query(ctx, `
		select key, title, priority, parent_key, created_at, closed_at
		from issues
		where project_key = $1
		order by key
	`, project)
	if err != nil {
		return fmt.Errorf("read the issues of %s: %w", project, err)
	}
	defer rows.Close()
	for rows.Next() {
		issue := &Issue{FirstRoleEvent: map[string]time.Time{}, Observed: map[string][]Mark{}}
		var parent *string
		if err := rows.Scan(&issue.Key, &issue.Title, &issue.Priority, &parent, &issue.CreatedAt, &issue.ClosedAt); err != nil {
			return fmt.Errorf("scan an issue of %s: %w", project, err)
		}
		if parent != nil {
			issue.Parent = *parent
		}
		p.Issues[issue.Key] = issue
		p.Keys = append(p.Keys, issue.Key)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the issues of %s: %w", project, err)
	}
	return nil
}

// readFiled reads every `issue.created` event of the window a session wrote, whatever project the
// new issue belongs to — an issue filed out of the filer's tree is one of metric 4's classes — and
// keeps the ones a Legion session of this project filed.
func (p *Population) readFiled(ctx context.Context, pool *store.Pool, project string, from, to time.Time) error {
	rows, err := pool.Query(ctx, `
		select coalesce(events.issue_key, ''), coalesce(issues.title, ''), events.created_at,
			coalesce(events.actor->>'id', ''), coalesce(events.payload->>'parent', ''),
			coalesce(events.payload->'ancestors', '[]'::jsonb)
		from events
		left join issues on issues.key = events.issue_key
		where events.type = 'issue.created'
			and events.actor->>'kind' = 'session'
			and events.created_at >= $1 and events.created_at < $2
		order by events.created_at, events.id
	`, from, to)
	if err != nil {
		return fmt.Errorf("read the issues filed in the window: %w", err)
	}
	defer rows.Close()
	var filed []Filed
	sessions := map[string]bool{}
	for rows.Next() {
		var one Filed
		var ancestors []byte
		if err := rows.Scan(&one.Key, &one.Title, &one.At, &one.Session, &one.Parent, &ancestors); err != nil {
			return fmt.Errorf("scan a filed issue: %w", err)
		}
		if one.Key == "" || one.Session == "" {
			continue
		}
		if err := json.Unmarshal(ancestors, &one.Ancestors); err != nil {
			one.Ancestors = nil
		}
		filed = append(filed, one)
		sessions[one.Session] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the issues filed in the window: %w", err)
	}
	titles, err := resolveTitles(ctx, pool, sessions)
	if err != nil {
		return err
	}
	for _, one := range filed {
		parsed, ok := legion.ParseSessionTitle(titles[one.Session])
		if !ok || !parsed.OfProject(project) {
			continue
		}
		one.Filer = parsed
		p.Filed = append(p.Filed, one)
	}
	return nil
}

// scopeKeys is every issue the event, handoff and observation reads cover: the issues that could
// have finished in the window, every issue each one shares a tree with (its ancestors, since a
// child's root is a tree of its own, and their descendants), and the issues the window's filings
// name as their filer, whose phase at the filing metric 4 reports.
func (p *Population) scopeKeys(from, to time.Time) []string {
	wanted := map[string]bool{}
	var reach func(key string)
	reach = func(key string) {
		issue := p.Issues[key]
		if issue == nil || wanted[key] {
			return
		}
		wanted[key] = true
		if issue.Parent != "" {
			reach(issue.Parent)
		}
	}
	for _, key := range p.Keys {
		issue := p.Issues[key]
		if issue.ClosedAt != nil && !issue.ClosedAt.Before(from) && issue.ClosedAt.Before(to) {
			reach(key)
		}
	}
	for _, filed := range p.Filed {
		if filed.Filer.Issue != "" {
			reach(filed.Filer.Issue)
		}
	}
	// Every descendant of a reached issue, however deep: the roots reached above carry their
	// trees, which segment 4 and metric 4's children-per-root read.
	for added := true; added; {
		added = false
		for _, key := range p.Keys {
			issue := p.Issues[key]
			if !wanted[key] && issue.Parent != "" && wanted[issue.Parent] {
				wanted[key], added = true, true
			}
		}
	}
	keys := make([]string, 0, len(wanted))
	for _, key := range p.Keys {
		if wanted[key] {
			keys = append(keys, key)
		}
	}
	return keys
}

// readRoleEvents reads the first event each session wrote on each of the issues, and keeps the
// ones a session titled `Legion <role> · <that issue's key>` wrote. The first event per session is
// all the rule needs ("each role's phase starts at the first event of a session titled …"), so
// this reads one row per session and issue rather than every event of every issue.
func (p *Population) readRoleEvents(ctx context.Context, pool *store.Pool, keys []string) error {
	rows, err := pool.Query(ctx, `
		select issue_key, actor->>'id', min(created_at)
		from events
		where issue_key = any($1) and actor->>'kind' = 'session' and actor->>'id' is not null
		group by issue_key, actor->>'id'
	`, keys)
	if err != nil {
		return fmt.Errorf("read the Legion session events: %w", err)
	}
	defer rows.Close()
	type sessionEvent struct {
		issue, session string
		at             time.Time
	}
	var found []sessionEvent
	sessions := map[string]bool{}
	for rows.Next() {
		var one sessionEvent
		if err := rows.Scan(&one.issue, &one.session, &one.at); err != nil {
			return fmt.Errorf("scan a session event: %w", err)
		}
		// The daemon's own session is no Legion role, and its title read would walk every event
		// it wrote (legion.DaemonSession).
		if legion.DaemonSession(one.session) {
			continue
		}
		found = append(found, one)
		sessions[one.session] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the Legion session events: %w", err)
	}
	titles, err := resolveTitles(ctx, pool, sessions)
	if err != nil {
		return err
	}
	for _, one := range found {
		parsed, ok := legion.WorkerTitle(titles[one.session])
		if !ok || !parsed.Works(one.issue) {
			continue
		}
		issue := p.Issues[one.issue]
		if issue == nil {
			continue
		}
		if held, seen := issue.FirstRoleEvent[parsed.Role]; !seen || one.at.Before(held) {
			issue.FirstRoleEvent[parsed.Role] = one.at
		}
	}
	return nil
}

// readStatusMarks reads every status the issues' own events snapshotted: what the parked
// subtraction and the pre-observation review rounds read.
func (p *Population) readStatusMarks(ctx context.Context, pool *store.Pool, keys []string) error {
	rows, err := pool.Query(ctx, `
		select issue_key, created_at, payload->>'status'
		from events
		where issue_key = any($1) and type like 'issue.%' and payload->>'status' is not null
		order by created_at, id
	`, keys)
	if err != nil {
		return fmt.Errorf("read the status snapshots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, status string
		var at time.Time
		if err := rows.Scan(&key, &at, &status); err != nil {
			return fmt.Errorf("scan a status snapshot: %w", err)
		}
		if issue := p.Issues[key]; issue != nil {
			issue.StatusMarks = append(issue.StatusMarks, Mark{At: at, Value: status})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the status snapshots: %w", err)
	}
	return nil
}

// askTurn is one span an ask stood on a person's turn: the issue it was opened on, the issue the
// opening session's title named, and the span itself. The second is what the tree rule reads
// (keepTreeAsks); it is empty for a controller, whose title names a project and no issue.
type askTurn struct {
	issue string
	by    string
	span  Span
}

// readAskTurns reads the ask events of the issues and turns each ask into the spans it stood on a
// person's turn: from an opening or a hand-back by a Legion session to the answer or resolution
// that took it back, and to the issue's end where neither came. Which of them count for the
// issue is keepTreeAsks's rule, which runs once every read has landed.
func (p *Population) readAskTurns(ctx context.Context, pool *store.Pool, keys []string) error {
	rows, err := pool.Query(ctx, `
		select issue_key, created_at, type, coalesce(payload->>'id', ''), coalesce(actor->>'id', ''),
			coalesce(actor->>'kind', '')
		from events
		where issue_key = any($1) and type = any($2)
		order by created_at, id
	`, keys, askTurnEventTypes)
	if err != nil {
		return fmt.Errorf("read the ask events: %w", err)
	}
	defer rows.Close()
	type askEvent struct {
		issue, kind, ask, session, actorKind string
		at                                   time.Time
	}
	var events []askEvent
	sessions := map[string]bool{}
	for rows.Next() {
		var one askEvent
		if err := rows.Scan(&one.issue, &one.at, &one.kind, &one.ask, &one.session, &one.actorKind); err != nil {
			return fmt.Errorf("scan an ask event: %w", err)
		}
		if one.ask == "" {
			continue
		}
		events = append(events, one)
		if one.actorKind == "session" {
			sessions[one.session] = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the ask events: %w", err)
	}
	titles, err := resolveTitles(ctx, pool, sessions)
	if err != nil {
		return err
	}
	open := map[string]askTurn{}
	for _, event := range events {
		switch event.kind {
		case "ask.opened", "ask.handed_back":
			if _, already := open[event.ask]; already {
				continue
			}
			if event.actorKind != "session" {
				continue
			}
			title, isLegion := legion.ParseSessionTitle(titles[event.session])
			if !isLegion {
				continue
			}
			open[event.ask] = askTurn{issue: event.issue, by: title.Issue, span: Span{From: event.at}}
		case "ask.answered", "ask.resolved":
			turn, waiting := open[event.ask]
			if !waiting {
				continue
			}
			delete(open, event.ask)
			turn.span.To = event.at
			p.askTurns = append(p.askTurns, turn)
		}
	}
	for _, turn := range open {
		issue := p.Issues[turn.issue]
		if issue == nil {
			continue
		}
		turn.span.To = p.issueEnd(issue)
		p.askTurns = append(p.askTurns, turn)
	}
	return nil
}

// keepTreeAsks gives each issue the ask turns that count for it: "An ask counts for an issue when
// it is opened on that issue by any Legion session of the issue's tree, that is, a session titled
// `Legion <role> · <KEY>` where KEY is any issue of the same tree, walked as for splitting and
// follow-ups. An ask opened by a session outside the tree does not count" (the spec's "Rule
// details — Whose ask counts as waiting on a person"). The walk is TreeRoot, the one splitting
// and follow-ups walks with, so one reading of a tree serves both rules; a controller's session
// names a project and no issue, so it is of no tree and its asks count for nothing.
func (p *Population) keepTreeAsks() {
	roots := map[string]string{}
	rootOf := func(key string) string {
		if root, held := roots[key]; held {
			return root
		}
		root := p.TreeRoot(key)
		roots[key] = root
		return root
	}
	for _, turn := range p.askTurns {
		issue := p.Issues[turn.issue]
		if issue == nil || turn.by == "" || rootOf(turn.issue) != rootOf(turn.by) {
			continue
		}
		issue.AskTurns = append(issue.AskTurns, turn.span)
	}
	p.askTurns = nil
	for _, key := range p.Keys {
		if issue := p.Issues[key]; issue != nil {
			slices.SortFunc(issue.AskTurns, func(a, b Span) int { return a.From.Compare(b.From) })
		}
	}
}

func (p *Population) readHandoffs(ctx context.Context, pool *store.Pool, keys []string) error {
	rows, err := pool.Query(ctx, `
		select issue_key, phase, version, created_at
		from handoffs
		where issue_key = any($1)
		order by created_at, version
	`, keys)
	if err != nil {
		return fmt.Errorf("read the handoffs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var handoff Handoff
		if err := rows.Scan(&key, &handoff.Phase, &handoff.Version, &handoff.At); err != nil {
			return fmt.Errorf("scan a handoff: %w", err)
		}
		if issue := p.Issues[key]; issue != nil {
			issue.Handoffs = append(issue.Handoffs, handoff)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the handoffs: %w", err)
	}
	return nil
}

// readPlans reads the plan of each run of the issues (workflowstore.IssuesPlans), oldest first:
// what the fallback timeline reads the order of a task's steps from (Issue.phaseLine). A plan
// whose tasks are not exactly one keeps no steps, which reads as the default steps.
func (p *Population) readPlans(ctx context.Context, pool *store.Pool, keys []string) error {
	plans, err := workflowstore.IssuesPlans(ctx, pool, keys)
	if err != nil {
		return err
	}
	for key, held := range plans {
		issue := p.Issues[key]
		if issue == nil {
			continue
		}
		for _, made := range held {
			var steps []string
			if len(made.Plan.Tasks) == 1 {
				for _, step := range made.Plan.Tasks[0].Steps {
					role, _, _ := strings.Cut(step, ":")
					steps = append(steps, role)
				}
			}
			issue.Plans = append(issue.Plans, RunPlan{Made: made.Made, Steps: steps})
		}
	}
	return nil
}

// readSampled reads what the sampler writes — the window's issues' observations (readObservations)
// and the project's reported set (readReported) — in one repeatable-read, read-only transaction,
// so the timelines and the trees answer one moment: a sampler commit between two reads would
// give an issue a tree from a read its timeline does not hold, or a timeline past its tree. The
// two run one after the other on that transaction's connection, beside Read's other reads, and
// the reported set is the short one — on the test Postgres at 3,000 issues of 6 fields, 26 ms
// beside the history's 531 ms over 180,000 rows — so reading it in turn costs the answer about
// that much.
func (p *Population) readSampled(ctx context.Context, pool *store.Pool, project string, keys []string) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin the read of %s's Legion observations: %w", project, err)
	}
	defer tx.Rollback(ctx)
	if err := p.readObservations(ctx, tx, project, keys); err != nil {
		return err
	}
	if err := p.readReported(ctx, tx, project); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// readObservations reads every change the sampler recorded of the issues, whenever it recorded it:
// an observation older than the window is what says what the issue's phase was when the window
// opened. The field is kept as the sampler wrote it, so a field a later daemon adds (the `tree` of
// dispatch://INFRA-980) is read the day the sampler records it and is absent, not wrong, until
// then.
func (p *Population) readObservations(ctx context.Context, tx pgx.Tx, project string, keys []string) error {
	rows, err := tx.Query(ctx, `
		select issue_key, field, value::text, observed_at
		from legion_observations
		where project = $1 and issue_key = any($2)
		order by observed_at, id
	`, project, keys)
	if err != nil {
		return fmt.Errorf("read the Legion observations of %s: %w", project, err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, field, value string
		var at time.Time
		if err := rows.Scan(&key, &field, &value, &at); err != nil {
			return fmt.Errorf("scan a Legion observation: %w", err)
		}
		issue := p.Issues[key]
		if issue == nil {
			continue
		}
		issue.Observed[field] = append(issue.Observed[field], Mark{At: at, Value: value})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the Legion observations of %s: %w", project, err)
	}
	return nil
}

// readReported reads the issues of the project the daemon reports, against the `tree` it names
// for each: legion.ReadCurrent's Reported, the read the Legion board makes too, of the whole
// project rather than the window's keys (legion.TreeNode). The read's Newest, the board's cards,
// go unused here: that is the cost of one read answering the rule for both readers.
// readObservations takes the history of the issues whose timelines the metrics measure.
func (p *Population) readReported(ctx context.Context, tx pgx.Tx, project string) error {
	current, err := legion.ReadCurrent(ctx, tx, project)
	if err != nil {
		return err
	}
	p.reported = current.Reported
	return nil
}

// fillReportedTrees gives each issue the `tree` the daemon named for it. That is the newest good
// read's `tree` for the issue (readReported), the one the Legion board reads too, and it is the
// only writer of Issue.Tree: the window's own observation read carries the same rows for the
// issues it covers, and a second writer taking the newest non-empty one of those answered a tree
// where the newest row is empty — a daemon that stopped naming one — while this read answered
// none. An issue the daemon named no tree for keeps none. It runs before linkTrees, which is the
// first reader of a tree.
func (p *Population) fillReportedTrees() {
	for _, key := range p.Keys {
		p.Issues[key].Tree = p.reported[key]
	}
}

// readMergeTimes reads the merge time of each issue's pull request from the delivery population,
// and fetches the ones that population does not hold yet from GitHub (delivery.FetchPullRequest),
// up to fetchedMergeTimesCap of them. A fetch that fails leaves the issue with no merge time.
func (p *Population) readMergeTimes(ctx context.Context, pool *store.Pool, github *githubapp.Client, keys []string) error {
	rows, err := pool.Query(ctx, `
		select issue_key, repo, number, merged_at
		from delivery_pull_requests
		where issue_key = any($1) and unfetchable_at is null
		order by issue_key, number
	`, keys)
	if err != nil {
		return fmt.Errorf("read the pull requests of the finished issues: %w", err)
	}
	defer rows.Close()
	type unmerged struct {
		issue, repo string
		number      int
	}
	var fetch []unmerged
	for rows.Next() {
		var key, repo string
		var number int
		var mergedAt *time.Time
		if err := rows.Scan(&key, &repo, &number, &mergedAt); err != nil {
			return fmt.Errorf("scan a pull request: %w", err)
		}
		issue := p.Issues[key]
		if issue == nil {
			continue
		}
		switch {
		case mergedAt == nil:
			fetch = append(fetch, unmerged{issue: key, repo: repo, number: number})
		case issue.MergedAt == nil || mergedAt.Before(*issue.MergedAt):
			issue.MergedAt = mergedAt
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the pull requests of the finished issues: %w", err)
	}
	if github == nil {
		return nil
	}
	// The cap first, over the flat list, so the same entries are attempted as a serial pass
	// would attempt; then one group per issue, in the order the rows arrived. Each group runs
	// its own entries one after another, so the first pull request of an issue that answers a
	// merge time is still the one assigned and the rest of that issue's are still skipped;
	// groups write to distinct issues, so no two of them touch the same field.
	if len(fetch) > fetchedMergeTimesCap {
		fetch = fetch[:fetchedMergeTimesCap]
	}
	grouped := map[string][]unmerged{}
	var order []string
	for _, one := range fetch {
		if _, seen := grouped[one.issue]; !seen {
			order = append(order, one.issue)
		}
		grouped[one.issue] = append(grouped[one.issue], one)
	}
	fetches := new(errgroup.Group)
	fetches.SetLimit(fetchedMergeTimesConcurrency)
	for _, key := range order {
		fetches.Go(func() error {
			for _, one := range grouped[key] {
				issue := p.Issues[one.issue]
				if issue == nil || issue.MergedAt != nil {
					continue
				}
				owner, name, found := strings.Cut(one.repo, "/")
				if !found {
					continue
				}
				fetched, err := delivery.FetchPullRequest(ctx, github, owner, name, one.number)
				if err != nil {
					slog.Debug("dispatch: read a Legion issue's merge time", "issue", one.issue, "repo", one.repo, "number", one.number, "error", err)
					continue
				}
				issue.MergedAt = fetched.MergedAt
			}
			return nil
		})
	}
	// No group returns an error — a fetch that fails leaves the issue with no merge time, as a
	// serial pass left it — so this waits and nothing more.
	_ = fetches.Wait()
	return nil
}

// linkTrees fills each issue's children: the issues whose tree root is it.
func (p *Population) linkTrees() {
	for _, key := range p.Keys {
		root := p.TreeRoot(key)
		if root == key || p.Issues[root] == nil {
			continue
		}
		p.Issues[root].Children = append(p.Issues[root].Children, key)
	}
}

// TreeRoot is the issue's tree root by legion.TreeRoot, the rule the Legion board places an issue
// in a tree with, over Issue.Tree (fillReportedTrees), Issue.Parent and legion.IsLegionIssue of the
// population's reported set (readReported) and phase work (readPhaseWork). The population holds
// every issue of the project, Legion's and the rest, and reads the daemon's half for the whole
// project, so it answers what the board answers. A fixture states the rule by filling the two
// sets, as a read does.
func (p *Population) TreeRoot(key string) string {
	return legion.TreeRoot(key, func(key string) (legion.TreeNode, bool) {
		issue := p.Issues[key]
		if issue == nil {
			return legion.TreeNode{}, false
		}
		return legion.TreeNode{Tree: issue.Tree, Parent: issue.Parent, Legion: legion.IsLegionIssue(key, p.reported, p.phaseWork)}, true
	})
}

// readPhaseWork reads the issues that carry Legion phase work, over the whole log of every project
// (legion.PhaseWorkKeys); TreeRoot looks up only this project's issues in it. It places an
// issue in a tree and nothing else: what a metric counts reads the window's own role events and
// handoffs (hasPhaseWork).
func (p *Population) readPhaseWork(ctx context.Context, pool *store.Pool) error {
	work, err := legion.PhaseWorkKeys(ctx, pool)
	if err != nil {
		return err
	}
	p.phaseWork = work
	return nil
}

// hasPhaseWork is whether this read carried Legion phase work of the issue: an event of a
// session titled `Legion <role> · <its key>`, or a handoff, among the window's scope. The rule
// that picks which finished issues a metric counts reads this and not legion.IsLegionIssue
// (compute.go's finishedInWindow): an issue the daemon reports and no role ever worked finished
// nothing, however its tree reads.
func (p *Population) hasPhaseWork(issue *Issue) bool {
	return len(issue.FirstRoleEvent) > 0 || len(issue.Handoffs) > 0
}

// issueEnd is where the issue's timeline stops: its close, or the window's end for one still open.
func (p *Population) issueEnd(issue *Issue) time.Time {
	if issue.ClosedAt != nil {
		return *issue.ClosedAt
	}
	return p.To
}

// resolveTitles is each session's most recent known title (delivery.ResolveSessionTitles, which
// reads the one index Dispatch keeps of them).
func resolveTitles(ctx context.Context, pool *store.Pool, sessions map[string]bool) (map[string]string, error) {
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	titles, err := delivery.ResolveSessionTitles(ctx, pool, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve the session titles: %w", err)
	}
	return titles, nil
}

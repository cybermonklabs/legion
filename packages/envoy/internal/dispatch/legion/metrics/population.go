// Package metrics computes the Legion measures — work time to merge, phase time by worker type,
// rework, and splitting and follow-ups (dispatch://INFRA-1077, the spec's "The four Legion
// metrics" and "Rule details"), and slot wait and idle time by role (dispatch://INFRA-2916, D3):
// each issue's facts, and the six folds over the day parts built from those facts (folds.go).
//
// It is split the way api/delivery_pipeline.go is split: ReadProject and ReadIssues make every
// database read a computation needs, and ProjectFactsClosed and ProjectFilings turn that
// population into the facts and touch nothing, so every reader of the facts shares one body
// rather than two implementations of the rules.
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
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// askTurnEventTypes are the ask events a person's turn starts and ends with: the two that hand an
// ask to a person and the two that take it back (api/events.go names the same family).
var askTurnEventTypes = []string{"ask.opened", "ask.handed_back", "ask.answered", "ask.resolved"}

// Mark is one value of one field as of a moment: an observation Dispatch recorded, or a status
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

// RunPlan is one version of the plan a run of the issue ran from, as Dispatch holds it
// (workflowstore's plan node): when that version was written, and the roles of its one
// task's steps, in the task's order (`acceptance` for `acceptance:owner`). Steps is empty for a
// plan whose tasks are not exactly one, which the fallback timeline reads as the default steps.
type RunPlan struct {
	Made  time.Time
	Steps []string
}

// Issue is one issue of the population with every fact the rules read of it.
type Issue struct {
	Key       string
	Parent    string
	CreatedAt time.Time
	ClosedAt  *time.Time

	// Tree is the issue's tree root as the daemon named it, from the `tree` field
	// dispatch://INFRA-980 adds to each issue of `GET /legion/v1/state`, where a read carried
	// one. Every issue of the project carries the newest one Dispatch recorded, whether or
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

	// Plans is each version of the plan of each run of the issue, oldest first: what the fallback timeline reads
	// the order of the task's steps from (phaseLine). An issue Legion ran before it recorded
	// plans has none.
	Plans []RunPlan

	// Observed is every value a read found changed, by legion_observations field (legion's
	// FieldPhase, FieldSlot, ClaimFieldPrefix+role and the rest), oldest first within a field.
	Observed map[string][]Mark

	// AskTurns are the spans in which an ask a Legion session of this issue's tree opened on
	// this issue stood on a person's turn, from its opening or hand-back to its answer or
	// resolution (keepTreeAsks). A turn neither came to holds the zero To, which the rule that
	// reads it takes as the issue's end (waitingOnPersonSpans): the end is the moment of the
	// computation for an open issue, so a turn that held it would tie the population to one
	// moment, and the stored facts are computed at two (ProjectFactsClosed).
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
	At        time.Time
	Session   string
	Filer     legion.SessionTitle
	Parent    string
	Ancestors []string
}

// Population is every fact one computation of the facts reads, from the project's beginning to
// To, the moment an open issue's hours run to. From is the zero time for ReadProject and
// ReadIssues, and is set only by test fixtures that narrow windowOf.
type Population struct {
	Project string
	From    time.Time
	To      time.Time

	// Issues is every issue of the project the read touched, by key; Keys lists them in order so
	// a pass over them is deterministic.
	Issues map[string]*Issue
	Keys   []string

	// Filed is every issue a Legion session of the project created that the read covers, in
	// creation order: the whole history's (ReadProject) or the named filed issues' (ReadIssues).
	Filed []Filed

	// reported is legion.ReadCurrent's Reported (readReported), and phaseWork is the issues that
	// carry Legion phase work among those the tree walk of this population can ask about: every
	// issue of every project for a whole read (ReadProject, legion.PhaseWorkKeys), and the scope
	// with each filing's chain for a narrow one (ReadIssues, placedKeys and legion.PhaseWorkOf),
	// the whole log's answer for each issue either way. They are the two halves of
	// legion.TreeNode's rule, neither of them read for the window's keys.
	reported  map[string]string
	phaseWork map[string]struct{}

	// Admission is the daemon's admission block as the newest good read recorded it, read in the
	// one snapshot the reported set and the scope's observations are read in (readReported), so
	// a reader that numbers the waiting line from it reads the block of the moment the issues'
	// phases and slots are of (dispatch://INFRA-1077's b1d6bd0512, "one answer reads one
	// snapshot"). It is nil where no read recorded one, where the newest good read no longer
	// carried it (its newest row is `null`), and where the row holds a value of another shape.
	Admission *legion.Admission

	// askTurns is every ask turn the read gathered, before the tree rule picked the ones that
	// count for the issue they were opened on. The rule reads each issue's tree, which the
	// observations of the same read name, so it runs once every read has landed (keepTreeAsks).
	askTurns []askTurn
}

// ReadProject is the population of every issue of the project, open or closed, up to now: what
// the per-issue facts read (factsOf). Its window runs from the beginning to now, so an open
// issue's hours run to now, and its scope is every issue of the project. Its filings are every
// issue a Legion session of the project ever filed (ProjectFilings), whose filers are issues of
// the project and so inside that scope. Its phase work is the whole log's (legion.PhaseWorkKeys):
// it is the read a build makes once per project, and its walks may ask about any issue.
func ReadProject(ctx context.Context, pool *store.Pool, project string, now time.Time) (*Population, error) {
	return read(ctx, pool, project, now, func(ctx context.Context, p *Population) error {
		return p.readFiled(ctx, pool, project, now)
	}, func(p *Population) []string {
		return p.Keys
	}, func(ctx context.Context, _ *Population, _ []string) (map[string]struct{}, error) {
		return legion.PhaseWorkKeys(ctx, pool)
	})
}

// ReadIssues is the population the facts of the named issues are recomputed from, so a consumer
// can recompute one issue without reading a whole project's event history. Its scope is wider
// than the keys asked for: it is every issue the trees of those keys reach by the one tree rule
// (treeScopeKeys), because an issue's own hours read the issues above it and the issues below it
// (segments.go measures a root's waiting on children from its children's spans). The project's
// own reads stay, because they are what places an issue in a tree and they are cheap.
//
// filed names the filed issues whose filings are read with it (readFiledIssues), the ones a
// Legion session of the project filed among them, and the scope holds the issue each filer was
// working as well, because a filing's class and its filer's phase read that issue's timeline
// (classify). The facts of the keys asked for are the same whatever the scope adds around them.
//
// Its phase work is read for the issues its tree walks can ask about alone (placedKeys,
// legion.PhaseWorkOf), by the issues' own rows, and never over the whole log: a pass of the
// facts consumer makes this read for each project it computes, and the whole log's read cost it
// about 730 ms a call at ten times INFRA's rate (dispatch://INFRA-2916, round 15's cost design).
// Each issue it reads carries the answer the whole log gives it (legion.PhaseWorkOf), and every
// question the computation asks of an issue it did not read has the same answer either way
// (placedKeys), so the facts and the filings are the ones ReadProject computes.
func ReadIssues(ctx context.Context, pool *store.Pool, project string, keys, filed []string, now time.Time) (*Population, error) {
	return read(ctx, pool, project, now, func(ctx context.Context, p *Population) error {
		return p.readFiledIssues(ctx, pool, project, filed)
	}, func(p *Population) []string {
		return p.treeScopeKeys(slices.Concat(keys, p.filerKeys()))
	}, func(ctx context.Context, p *Population, scope []string) (map[string]struct{}, error) {
		return legion.PhaseWorkOf(ctx, pool, p.placedKeys(scope))
	})
}

// read is the body the two readers share, from the beginning to `to`. filed reads the
// population's filings, before the scope is chosen because the scope holds each filer's issue,
// scope names which issues the six detail reads cover, and phaseWork reads which issues carry
// Legion phase work, given that scope, for the tree walks (TreeNode).
func read(ctx context.Context, pool *store.Pool, project string, to time.Time,
	filed func(ctx context.Context, p *Population) error,
	scope func(*Population) []string,
	phaseWork func(ctx context.Context, p *Population, scope []string) (map[string]struct{}, error),
) (*Population, error) {
	population := &Population{Project: project, To: to, Issues: map[string]*Issue{}}
	if err := population.readIssues(ctx, pool, project); err != nil {
		return nil, err
	}
	if err := filed(ctx, population); err != nil {
		return nil, err
	}
	// What the Legion intake writes — the project's reported set with its trees and the daemon's
	// admission block, and the observations of the scope's issues — is read in one repeatable-read,
	// read-only transaction, so the trees, the block and the timelines answer one moment: an intake
	// commit between two reads would give an issue a tree from a record its timeline does not hold,
	// or a timeline past its tree. The transaction opens here, on a hold mark of its own
	// (store.ForConcurrentRead), because the scope is chosen from its first read, and the read
	// that takes the observations in the group below commits it (readSampled): it holds that one
	// connection and waits on no other, so it is one of the reads side by side.
	sampled, err := store.ForConcurrentRead(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := pool.BeginTx(sampled, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin the read of %s's Legion observations: %w", project, err)
	}
	defer tx.Rollback(sampled)
	// readReported runs here, before the scope is chosen, and not in the group below. It takes
	// no keys — it reads what the daemon reports of every issue of the project — and the scope
	// is chosen from its answer: the rule that places an issue in a tree reads the daemon's
	// `tree` first and the parent chain only where no daemon named one (legion.TreeChain), so a
	// scope chosen before this read would hold a seed's parent chain and not the tree a `tree`
	// observation names. fillReportedTrees follows it at once, because Issue.Tree is where the
	// walk and the scope both read that answer.
	if err := population.readReported(sampled, tx, project); err != nil {
		return nil, err
	}
	population.fillReportedTrees()
	keys := scope(population)
	// The eight reads below write disjoint fields — seven take the keys above and write fields of
	// those issues, the eighth the population's phase work — so they run together rather than
	// one after another, as the delivery population's independent reads do (api/delivery.go).
	// store.ReadSideBySide answers the first error recorded, so which of two failing reads names
	// the failure is no longer fixed.
	if err := store.ReadSideBySide(ctx,
		func(ctx context.Context) error { return population.readRoleEvents(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readStatusMarks(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readAskTurns(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readHandoffs(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readPlans(ctx, pool, keys) },
		func(ctx context.Context) error { return population.readSampled(ctx, tx, project, keys) },
		func(ctx context.Context) error { return population.readMergeTimes(ctx, pool, keys) },
		func(ctx context.Context) (err error) {
			population.phaseWork, err = phaseWork(ctx, population, keys)
			return err
		},
	); err != nil {
		return nil, err
	}
	population.linkTrees()
	population.keepTreeAsks()
	return population, nil
}

// readIssues reads the project's issues, all of them: the rules read each issue's whole tree (a
// child that finished belongs to a root that may not have) and its whole life (the work time runs
// from its first worker event).
func (p *Population) readIssues(ctx context.Context, pool *store.Pool, project string) error {
	rows, err := pool.Query(ctx, `
		select key, parent_key, created_at, closed_at
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
		if err := rows.Scan(&issue.Key, &parent, &issue.CreatedAt, &issue.ClosedAt); err != nil {
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

// readFiled reads every `issue.created` event before `to` a session wrote, whatever project
// the new issue belongs to — an issue filed out of the filer's tree is one of metric 4's
// classes — and keeps the ones a Legion session of this project filed.
func (p *Population) readFiled(ctx context.Context, pool *store.Pool, project string, to time.Time) error {
	return p.readFilings(ctx, pool, project, `
		select coalesce(events.issue_key, ''), events.created_at,
			coalesce(events.actor->>'id', ''), coalesce(events.payload->>'parent', ''),
			coalesce(events.payload->'ancestors', '[]'::jsonb)
		from events
		where events.type = 'issue.created'
			and events.actor->>'kind' = 'session'
			and events.created_at < $1
		order by events.created_at, events.id
	`, to)
}

// readFiledIssues reads the `issue.created` event a session wrote of each named issue, whatever
// project it belongs to, and keeps the ones a Legion session of this project filed: readFiled's
// rule over a set of filed issues rather than a window, which is the filings a consumer that
// recomputes some issues recomputes with them. The events are read by their issue, so the read
// is the issues' own and never the whole log's.
func (p *Population) readFiledIssues(ctx context.Context, pool *store.Pool, project string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	return p.readFilings(ctx, pool, project, `
		select coalesce(events.issue_key, ''), events.created_at,
			coalesce(events.actor->>'id', ''), coalesce(events.payload->>'parent', ''),
			coalesce(events.payload->'ancestors', '[]'::jsonb)
		from events
		where events.issue_key = any($1)
			and events.type = 'issue.created'
			and events.actor->>'kind' = 'session'
		order by events.created_at, events.id
	`, keys)
}

// readFilings is the body the two reads of filings share: it runs statement, which answers the
// filed issue, the moment, the session, and the parent and ancestors the event carried, resolves
// each session's newest title, and keeps the filings whose session is a Legion session of this
// project, in the order the statement answered them.
func (p *Population) readFilings(ctx context.Context, pool *store.Pool, project, statement string, args ...any) error {
	rows, err := pool.Query(ctx, statement, args...)
	if err != nil {
		return fmt.Errorf("read the issues Legion sessions of %s filed: %w", project, err)
	}
	defer rows.Close()
	var filed []Filed
	sessions := map[string]bool{}
	for rows.Next() {
		var one Filed
		var ancestors []byte
		if err := rows.Scan(&one.Key, &one.At, &one.Session, &one.Parent, &ancestors); err != nil {
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
		return fmt.Errorf("read the issues Legion sessions of %s filed: %w", project, err)
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

// filerKeys is the issue each filing's filer was working, for every filing the population read,
// and none for a filing by the controller, which works no issue.
func (p *Population) filerKeys() []string {
	keys := make([]string, 0, len(p.Filed))
	for _, filed := range p.Filed {
		if filed.Filer.Issue != "" {
			keys = append(keys, filed.Filer.Issue)
		}
	}
	return keys
}

// treeScopeKeys is the scope the seeds reach, by the one tree step (legion.TreeAbove): each
// seed, every issue above it — the `tree` the daemon reported for an issue, and the parent
// Dispatch records where the daemon reported none — and every issue below a reached issue by
// that same step, however deep, which is what the children of a root and a root's wait on its
// children read. A seed the project does not hold reaches nothing.
//
// This walk takes no placement stop: it climbs through a parent that is not a Legion issue,
// where legion.TreeChain stops below one. That is a second rule and not a copy of the first,
// and what a scope is for is the reason for it. The rules read a tree out of this scope, so a
// scope that holds more than they read costs a row of a detail table, while a scope that holds
// less answers a rule from a part of the tree. Wider is the right side to err on here, and the
// stop that keeps a placed root inside Legion's trees belongs to the walk that places it.
//
// The walk holds the issues below an issue the daemon reports as its own tree all the same,
// which is the other place the scope reaches wider than the placement walk. It stops for two
// reasons of its own besides: a key the population does not hold, which is an issue outside
// the project, and a key it has already reached, which is what stops a chain that loops.
//
// It climbs the daemon's `tree` and not the parent chain alone, because the step does: a scope
// that climbed parents alone would leave out the tree a `tree` observation names, and the rules
// that read a tree would then answer a narrow pass from a part of it and a whole-project pass
// from the whole. It reads Issue.Tree, which read fills before it chooses the scope.
func (p *Population) treeScopeKeys(seeds []string) []string {
	wanted := map[string]bool{}
	var reach func(key string)
	reach = func(key string) {
		issue := p.Issues[key]
		if issue == nil || wanted[key] {
			return
		}
		wanted[key] = true
		if above, _ := legion.TreeAbove(issue.Key, legion.TreeNode{Tree: issue.Tree, Parent: issue.Parent}); above != "" {
			reach(above)
		}
	}
	for _, key := range seeds {
		reach(key)
	}
	// Every issue below a reached issue, however deep: the roots reached above carry their
	// trees, which segment 4 and metric 4's children-per-root read.
	for added := true; added; {
		added = false
		for _, key := range p.Keys {
			issue := p.Issues[key]
			above, _ := legion.TreeAbove(issue.Key, legion.TreeNode{Tree: issue.Tree, Parent: issue.Parent})
			if !wanted[key] && above != "" && wanted[above] {
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

// placedKeys is every issue a narrow read's computation can ask the tree rule about
// (legion.TreeChain, through TreeNode), which is every issue whose Legion phase work ReadIssues
// reads: the scope, and each filing's filed issue with every issue above it by the one tree step
// (legion.TreeAbove), up to the first the scope holds or the project does not.
//
// The scope is closed under that step both ways (treeScopeKeys), so a chain from a scope key
// stays inside it, and the chain from an issue outside it never reaches it: an issue whose step
// leads into the scope is in the scope. Every rule asks the walk of a scope key, the facts and the
// waiting line of the keys asked for, and the ask turns and the class of a filing of the scope's
// own issues, or compares a scope key's root with the root of an issue outside the scope (the
// issue an ask's session works, keepTreeAsks; the parent a filing was filed under, classify),
// which differ whatever that issue's chain reads. linkTrees places every issue of the project, and
// one outside the scope lands under a root outside it whatever its chain reads, so the children of
// a scope key are the whole read's. The one root asked of an issue outside the scope is a filed
// issue's own (ProjectFilings' Root), since a filer of the scope may file into another tree, and
// its chain is read here, so the root it answers is the whole read's.
//
// The climb takes no placement stop, as the scope's does not: it holds every issue the placement
// walk can climb through above a filed issue, and some it stops below, which costs a key of the
// read and not an answer.
func (p *Population) placedKeys(scope []string) []string {
	keys := slices.Clone(scope)
	held := make(map[string]bool, len(scope))
	for _, key := range scope {
		held[key] = true
	}
	for _, filed := range p.Filed {
		for key := filed.Key; !held[key]; {
			issue := p.Issues[key]
			if issue == nil {
				break
			}
			held[key] = true
			keys = append(keys, key)
			key, _ = legion.TreeAbove(key, legion.TreeNode{Tree: issue.Tree, Parent: issue.Parent})
		}
	}
	return keys
}

// TreeLinks is every issue of the project by the issue above it, and "" for an issue nothing
// stands above it: the one read of a project's tree links. Nothing in this slice calls it: it is
// exported for the Legion facts consumer of a later slice (dispatch://INFRA-5577), which climbs
// and descends the links to state which stored rows a change to an issue can move, and the links
// state the same tree this package's own scope holds (treeScopeKeys), because both take the one
// tree step (legion.TreeAbove). An issue the project does not hold has no entry, so a walk that
// reaches it stops.
//
// It makes two reads, one for each fact the step reads: what the daemon reports of every issue
// of the project (legion.ReadCurrent's Reported, the read the board and this package's own
// population make), and the project's issues with the parent link Dispatch records.
func TreeLinks(ctx context.Context, pool *store.Pool, project string) (map[string]string, error) {
	current, err := legion.ReadCurrent(ctx, pool, project)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		select key, coalesce(parent_key, '')
		from issues
		where project_key = $1
	`, project)
	if err != nil {
		return nil, fmt.Errorf("read the tree links of %s: %w", project, err)
	}
	defer rows.Close()
	above := map[string]string{}
	for rows.Next() {
		var key, parent string
		if err := rows.Scan(&key, &parent); err != nil {
			return nil, fmt.Errorf("read a tree link of %s: %w", project, err)
		}
		above[key], _ = legion.TreeAbove(key, legion.TreeNode{Tree: current.Reported[key], Parent: parent})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the tree links of %s: %w", project, err)
	}
	return above, nil
}

// roleEventsStatement is readRoleEvents' statement: the first event each session wrote on each of
// the issues $1 names. It takes those issues' rows by events' unique (issue_key, seq) first and
// keeps the sessions' among them after, as legion's readSessionIssues does and for its reason:
// written as one condition, `issue_key = any($1)` beside the actor's kind and id, the planner,
// which holds no statistics on actor's fields, can take the partial index on a session's id
// (events_session_actor_created_at_idx) for a selective one and read every session event of the
// log to intersect it with the issues' rows. TestTheRoleEventsReadReadsTheIssuesRowsByIssue holds
// the plan to the issues' index over a million events.
const roleEventsStatement = `
	with scoped as materialized (
		select issue_key, actor, created_at from events where issue_key = any($1))
	select issue_key, actor->>'id', min(created_at) from scoped
	where actor->>'kind' = 'session' and actor->>'id' is not null
	group by issue_key, actor->>'id'`

// readRoleEvents reads the first event each session wrote on each of the issues, and keeps the
// ones a session titled `Legion <role> · <that issue's key>` wrote. The first event per session is
// all the rule needs ("each role's phase starts at the first event of a session titled …"), so
// this reads one row per session and issue rather than every event of every issue
// (roleEventsStatement).
func (p *Population) readRoleEvents(ctx context.Context, pool *store.Pool, keys []string) error {
	rows, err := pool.Query(ctx, roleEventsStatement, keys)
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
// that took it back, and with the zero To where neither came, which the rule reads as the issue's
// end (Issue.AskTurns). Which of them count for the issue is keepTreeAsks's rule, which runs once
// every read has landed.
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
		if p.Issues[turn.issue] == nil {
			continue
		}
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

// readPlans reads each version of the plan of each run of the issues (workflowstore.IssuesPlans), oldest first:
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

// readSampled reads the scope's observations (readObservations) in the repeatable-read,
// read-only transaction read opened for the project's reported set (readReported), and commits
// it, so the timelines and the trees answer one moment. The reported set is read first and alone,
// because the scope is chosen from it, and it is the short read — on the test Postgres at 3,000
// issues of 6 fields, 26 ms beside the history's 531 ms over 180,000 rows (dispatch://INFRA-1077's
// b1d6bd0512) — while the history runs beside read's other reads.
func (p *Population) readSampled(ctx context.Context, tx pgx.Tx, project string, keys []string) error {
	if err := p.readObservations(ctx, tx, project, keys); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// readObservations reads every change Dispatch recorded of the issues, whenever it recorded it:
// an observation older than the window is what says what the issue's phase was when the window
// opened. The field is kept as Dispatch wrote it, so a field a later daemon adds (the `tree` of
// dispatch://INFRA-980) is read the day Dispatch records it and is absent, not wrong, until
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
// project rather than the scope's keys (legion.TreeNode), in the transaction readSampled takes
// the scope's observations in. Of the read's Newest, the board's cards, it keeps the daemon's
// admission block alone (Admission), so a reader that numbers the waiting line from it reads the
// block of the moment the observations are of; the rest go unused here, which is the cost of one
// read answering the rule for both readers.
func (p *Population) readReported(ctx context.Context, tx pgx.Tx, project string) error {
	current, err := legion.ReadCurrent(ctx, tx, project)
	if err != nil {
		return err
	}
	p.reported = current.Reported
	for _, one := range current.Newest {
		if one.IssueKey != legion.AdmissionKey || one.Field != legion.FieldAdmission {
			continue
		}
		// A value of another shape leaves the block unread: the daemon owns the wire shape,
		// and a reader of one names what it expects, as the board does (observeProject).
		if admission := legion.AdmissionOf(one.Value); admission != nil {
			p.Admission = admission
		}
	}
	return nil
}

// fillReportedTrees gives each issue the `tree` the daemon named for it. That is the newest good
// read's `tree` for the issue (readReported), the one the Legion board reads too, and it is the
// only writer of Issue.Tree: the window's own observation read carries the same rows for the
// issues it covers, and a second writer taking the newest non-empty one of those answered a tree
// where the newest row is empty — a daemon that stopped naming one — while this read answered
// none. An issue the daemon named no tree for keeps none. It runs before the scope is chosen,
// which is the first reader of a tree (treeScopeKeys), and so before linkTrees too.
func (p *Population) fillReportedTrees() {
	for _, key := range p.Keys {
		p.Issues[key].Tree = p.reported[key]
	}
}

// readMergeTimes reads the merge time of each issue's pull request from the delivery population:
// the earliest of those that merged, where it holds one.
func (p *Population) readMergeTimes(ctx context.Context, pool *store.Pool, keys []string) error {
	rows, err := pool.Query(ctx, `
		select issue_key, merged_at
		from delivery_pull_requests
		where issue_key = any($1) and unfetchable_at is null
		order by issue_key, number
	`, keys)
	if err != nil {
		return fmt.Errorf("read the pull requests of the finished issues: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var mergedAt *time.Time
		if err := rows.Scan(&key, &mergedAt); err != nil {
			return fmt.Errorf("scan a pull request: %w", err)
		}
		issue := p.Issues[key]
		if issue == nil || mergedAt == nil {
			continue
		}
		if issue.MergedAt == nil || mergedAt.Before(*issue.MergedAt) {
			issue.MergedAt = mergedAt
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the pull requests of the finished issues: %w", err)
	}
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

// TreeRoot is the issue's tree root, by the one walk that places an issue in a tree
// (legion.TreeRoot), over what this population reads of each issue (TreeNode). Where that walk
// stops, and why, is stated once in legion.TreeChain's doc.
//
// Both facts the walk reads are the project's and not the window's — Issue.Tree is the daemon's
// newest `tree` for every issue of the project (fillReportedTrees), and legion.IsLegionIssue reads
// the project's reported set (readReported) and the whole log's phase work of each issue the read
// asked about (phaseWork: every issue in a whole read, placedKeys in a narrow one) — so this
// population answers what the Legion board answers for every issue its computation places. It
// holds every issue of the project, Legion's and the rest. A fixture states the rule by filling
// the two sets, as a read does.
func (p *Population) TreeRoot(key string) string {
	return legion.TreeRoot(key, p.TreeNode)
}

// TreeNode is what the tree walk reads of one issue of this population (legion.TreeNode): the
// `tree` the daemon named for it, the parent Dispatch records, and whether the issue is Legion's
// (legion.IsLegionIssue, over the population's reported set and its phase work). It is false for
// a key this population does not hold. A reader that must place something on the nearest issue
// above another walks the chain itself (legion.TreeChain), as the waiting line does for an
// unstarted child, and it climbs this read.
func (p *Population) TreeNode(key string) (legion.TreeNode, bool) {
	issue := p.Issues[key]
	if issue == nil {
		return legion.TreeNode{}, false
	}
	return legion.TreeNode{Tree: issue.Tree, Parent: issue.Parent, Legion: legion.IsLegionIssue(key, p.reported, p.phaseWork)}, true
}

// hasPhaseWork is whether this read carried Legion phase work of the issue: an event of a
// session titled `Legion <role> · <its key>`, or a handoff, among the window's scope. The rule
// that picks which finished issues a measure counts reads this and not legion.IsLegionIssue
// (factsIssues, whose issues closed in a window are the ones it finished): an issue the daemon
// reports and no role ever worked finished nothing, however its tree reads.
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

// at is this population computed at another moment: the same inputs, with every open issue's
// timeline running to to rather than to To. A rule reads the moment through To alone (issueEnd,
// and the slot hours' end), and nothing a read gathered holds it (an ask nobody answered holds
// the zero To, readAskTurns), so the copy answers what a read made at to would answer of the
// same inputs. The issues are shared with the population it copies, which no rule writes to.
func (p *Population) at(to time.Time) *Population {
	moved := *p
	moved.To = to
	return &moved
}

// resolveTitles is each session's most recent known title (delivery.ResolveSessionTitles, which
// reads the one table Dispatch keeps of them, session_titles).
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

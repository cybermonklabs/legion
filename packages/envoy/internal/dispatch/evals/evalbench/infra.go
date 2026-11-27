package evalbench

import (
	"cmp"
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/evals/evalfixture"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A project's issue history at INFRA's size: INFRA's, and under the k14 load (Spec.K14) that of
// each project k14.go adds, drawn by the same rules from random streams of its own
// (projectFixture.rand). Each issue is a life from its opening to its close (or to the history's
// end for an open one), split into the six Legion roles' stretches in their order: architect,
// planner, implementer, tester, reviewer, merger. An open issue's status says how many of them it
// reached. Every event of the issue is by the session of the role whose stretch holds it, titled
// `Legion <role> · <KEY>`, which is what makes it that role's phase work
// (legion.RoleSessionWorks), or now and then by a helper session whose title Legion does not
// read; a child's first event is its filing by its root's architect. Each worked issue carries the
// handoffs of the stretches it finished, and an issue of the last observedDays the sampler's
// phase, slot and claim observations.

// roles are the Legion roles in the order an issue's life passes through them.
var roles = []string{"architect", "planner", "implementer", "tester", "reviewer", "merger"}

// roleCount is how many roles there are; an issue's stretches are an array of one more.
const roleCount = 6

// rolePhases is the phase the sampler records as each role's stretch begins, and handoffPhases the
// handoff each role writes as its stretch ends ("" for none).
var (
	rolePhases    = []string{legion.PhaseAdmitted, legion.PhasePlanning, legion.PhaseImplementing, legion.PhaseTesting, legion.PhaseReviewing, legion.PhaseMerging}
	handoffPhases = []string{"", "plan", "implement", "test", "review", ""}
)

// openStatuses are the statuses of an open issue, with how many roles' stretches each reached and
// how often an open issue holds it.
var openStatuses = []struct {
	status  string
	reached int
	weight  float64
}{
	{"backlog", 1, 0.30}, {"triage", 1, 0.10}, {"todo", 2, 0.20},
	{"in_progress", 3, 0.25}, {"testing", 4, 0.05}, {"needs_review", 5, 0.10},
}

// helperSessions is how many sessions Legion does not run write a share of the events.
const helperSessions = 50

// helperShare is the share of an issue's middle events a helper session writes.
const helperShare = 0.15

// plannedIssue is one issue of the history before it is written.
type plannedIssue struct {
	number   int
	key      string
	parent   int // index into the history's issues, -1 for a root
	repo     string
	opened   time.Time
	closed   *time.Time
	status   string
	priority *int16
	// reached is how many roles' stretches the issue reached, and stretch[r] when role r's
	// stretch begins, stretch[reached] when the last one ends: its close, or the history's end.
	reached  int
	stretch  [roleCount + 1]time.Time
	claimed  bool
	routed   bool
	events   int
	observed bool
}

// end is when the issue's last stretch ends.
func (p plannedIssue) end() time.Time { return p.stretch[p.reached] }

// title is the issue's title, which copyIssues writes to its row and every issue event's payload
// carries (eventSource.payload).
func (p plannedIssue) title() string {
	return fmt.Sprintf("Infrastructure work item %d: %s", p.number, text(p.key, 40))
}

// projectHistory is one project's issues, in opening order (so in number order), which its
// delivery history names and (INFRA's) the readings' row keys draw from, and the project they are
// of.
type projectHistory struct {
	fixture projectFixture
	issues  []plannedIssue
}

// projectFixture is one project the load writes at INFRA's size: its key and name, its three
// repositories, its default repository first, which issues and pull requests name as INFRA's do
// (three in five pull requests, a third of the issues), its deploy repositories, the repositories
// whose workflow runs the history holds at INFRA's run rate (dailyRuns) from a first run id of
// their own, and the name its random streams carry beside each part's (stream: none for INFRA,
// whose streams keep the names they had before the k14 projects, so INFRA's history is the same
// whether or not they are loaded). sources, where it is set, is the settings the project's delivery
// history is read by; nil is INFRA's one source (Settings).
type projectFixture struct {
	key, name string
	repos     [3]string
	deploys   []deployRepo
	stream    string
	sources   func(spec Spec) delivery.ProjectSettingsInput
}

// deployRepo is a repository whose deploy and pull-request runs a project's history holds, and the
// id of its first run: the ranges of the repositories of one load never meet (k14.go).
type deployRepo struct {
	repo       string
	firstRunID int64
}

// infraFixture is INFRA: its three repositories, the first its deploy repository.
var infraFixture = projectFixture{key: Project, name: "Infrastructure", repos: [3]string(Repos),
	deploys: []deployRepo{{repo: Repos[0], firstRunID: firstRunID}}}

// rand is the random source of one part of the project's load: INFRA's part's own stream
// (newRand), and another project's the stream of the part and its stream name together.
func (p projectFixture) rand(seed uint64, part string) *rand.Rand {
	if p.stream == "" {
		return newRand(seed, part)
	}
	return newRand(seed, part+" "+p.stream)
}

// newRand is the random source of one part of the load: one PCG stream per part, so a change to
// one part's draws leaves every other part's history as it was.
func newRand(seed uint64, part string) *rand.Rand {
	hash := fnv.New64a()
	hash.Write([]byte(part))
	return rand.New(rand.NewPCG(seed, hash.Sum64()))
}

// lasting is a log-normal duration around median: most near it, a few far longer.
func lasting(rng *rand.Rand, median time.Duration, sigma float64) time.Duration {
	return time.Duration(float64(median) * math.Exp(sigma*rng.NormFloat64()))
}

// helperActor is the actor JSON of helper session n, whose title Legion does not read as one of
// its own.
func helperActor(n int) string {
	return fmt.Sprintf(`{"kind":"session","id":"s-helper-%02d","origin":{"session_title":"Dispatch helper %02d"}}`, n, n)
}

// words is the text every body is cut from.
var words = strings.Fields(`the change keeps the reader's page within its budget while the day rows
	fold every series of the window and the compare range from stored parts so no read of the page
	reads the population again and each section of the method names the rule the code computes`)

// text is a body of about length bytes, the same for one key on every run.
func text(key string, length int) string {
	hash := fnv.New64a()
	hash.Write([]byte(key))
	at := int(hash.Sum64() % uint64(len(words)))
	var body strings.Builder
	for body.Len() < length {
		if body.Len() > 0 {
			body.WriteByte(' ')
		}
		body.WriteString(words[at%len(words)])
		at += 7
	}
	return body.String()
}

// planIssues draws the project's issues: when each opens and closes, its status, priority, owner,
// tree, repository and how many events it carries.
func planIssues(spec Spec, fixture projectFixture) projectHistory {
	rng := fixture.rand(spec.Seed, "infra issues")
	count := spec.scaled(issues, 12)
	start := spec.Now.Add(-issueDays * 24 * time.Hour)
	span := spec.Now.Sub(start)
	planned := make([]plannedIssue, count)
	opened := make([]time.Time, count)
	for i := range opened {
		opened[i] = start.Add(time.Duration(rng.Int64N(int64(span/time.Second))) * time.Second)
	}
	slices.SortFunc(opened, func(a, b time.Time) int { return a.Compare(b) })
	life := make([]time.Duration, count)
	for i := range planned {
		planned[i] = plannedIssue{number: i + 1, key: fmt.Sprintf("%s-%d", fixture.key, i+1), parent: -1, opened: opened[i]}
		life[i] = min(max(lasting(rng, 40*time.Hour, 1.0), time.Hour), 60*24*time.Hour)
	}

	// The open issues: an issue that could not have closed by the end is open, and the rest of
	// the open ones lean to the recent, as a backlog's do.
	open := count - int(math.Round(float64(count)*closedShare))
	score := make([]float64, count)
	for i, issue := range planned {
		recency := float64(issue.opened.Sub(start)) / float64(span)
		score[i] = rng.Float64() * (1 + 4*recency)
		if !issue.opened.Add(life[i]).Before(spec.Now) {
			score[i] = math.Inf(1)
		}
	}
	order := make([]int, count)
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(score[b], score[a]) })
	isOpen := make([]bool, count)
	for _, i := range order[:open] {
		isOpen[i] = true
	}

	// Exactly p0Share of the issues are P0; the rest P1 to P3 or none.
	p0 := map[int]bool{}
	for _, i := range rng.Perm(count)[:int(math.Round(float64(count)*p0Share))] {
		p0[i] = true
	}
	var roots []int
	for i := range planned {
		issue := &planned[i]
		switch u := rng.Float64(); {
		case p0[i]:
			issue.priority = new(int16(0))
		case u < 0.2:
			issue.priority = new(int16(1))
		case u < 0.6:
			issue.priority = new(int16(2))
		case u < 0.85:
			issue.priority = new(int16(3))
		}
		switch u := rng.Float64(); {
		case u < 0.35:
			issue.repo = fixture.repos[0]
		case u < 0.55:
			issue.repo = fixture.repos[1]
		case u < 0.70:
			issue.repo = fixture.repos[2]
		}
		// About a third of the issues are children of a recent root, one level down.
		if len(roots) > 0 && rng.Float64() < 0.35 {
			recent := roots[max(0, len(roots)-30):]
			issue.parent = recent[rng.IntN(len(recent))]
		} else {
			roots = append(roots, i)
		}
		issue.status, issue.reached = "done", len(roles)
		if isOpen[i] {
			pick, total := rng.Float64(), 0.0
			for _, choice := range openStatuses {
				total += choice.weight
				issue.status, issue.reached = choice.status, choice.reached
				if pick < total {
					break
				}
			}
			// An open P0 is mostly in hand: nine in ten are in progress. The tenth keeps the status
			// drawn above and has no owner, which unowned-p0 counts, unless it is in progress
			// (claimed) or a todo a route holds; two in three of them have none, so about one open P0
			// in fifteen.
			if p0[i] && rng.Float64() < 0.9 {
				issue.status, issue.reached = "in_progress", 3
			}
			issue.claimed = issue.reached >= 3 && (!p0[i] || issue.status == "in_progress")
			issue.routed = issue.status == "todo" && rng.Float64() < 0.4
		} else {
			issue.closed = new(issue.opened.Add(life[i]))
		}
		end := spec.Now
		if issue.closed != nil {
			end = *issue.closed
		}
		// The stretches split the life by random weights, the implementer's the heaviest.
		weights := make([]float64, issue.reached)
		total := 0.0
		for r := range weights {
			weights[r] = (0.5 + rng.Float64()) * []float64{1, 0.6, 2.5, 1, 1, 0.5}[r]
			total += weights[r]
		}
		issue.stretch[0] = issue.opened
		at := 0.0
		for r := range weights {
			at += weights[r]
			issue.stretch[r+1] = issue.opened.Add(time.Duration(float64(end.Sub(issue.opened)) * at / total))
		}
		issue.stretch[issue.reached] = end
		issue.observed = !issue.opened.Before(spec.Now.Add(-observedDays * 24 * time.Hour))
	}

	// The events, 62 an issue on average and exactly that many in all, each issue holding at
	// least its creation, its close and one event a stretch.
	totalEvents := count * eventsPerIssue
	weights := make([]float64, count)
	weightSum := 0.0
	for i := range weights {
		weights[i] = math.Exp(0.7 * rng.NormFloat64())
		weightSum += weights[i]
	}
	assigned := 0
	floors := make([]int, count)
	for i := range planned {
		floors[i] = planned[i].reached + 2
		planned[i].events = max(floors[i], int(float64(totalEvents)*weights[i]/weightSum))
		assigned += planned[i].events
	}
	for assigned != totalEvents {
		i := rng.IntN(count)
		switch {
		case assigned < totalEvents:
			planned[i].events++
			assigned++
		case planned[i].events > floors[i]:
			planned[i].events--
			assigned--
		}
	}
	return projectHistory{fixture: fixture, issues: planned}
}

// plannedEvent is one event of the history, compact, so the whole log can be put in time order
// before it is written.
type plannedEvent struct {
	at     int64 // unix nanoseconds
	issue  int32
	seq    int32
	kind   uint8
	role   int8 // -1 for a helper session
	helper int8
	// filing is an issue.created written by the parent's architect.
	filing bool
}

// The kinds of event the history writes.
const (
	kindCreated uint8 = iota
	kindComment
	kindUpdated
	kindClosed
)

var kindNames = []string{"issue.created", "comment.created", "issue.updated", "issue.closed"}

// planEvents draws every event of every issue and puts the log in time order, as the event log of
// a live project holds it.
func planEvents(spec Spec, history projectHistory) []plannedEvent {
	rng := history.fixture.rand(spec.Seed, "infra events")
	total := 0
	for _, issue := range history.issues {
		total += issue.events
	}
	planned := make([]plannedEvent, 0, total)
	for index, issue := range history.issues {
		seq := int32(0)
		add := func(at time.Time, kind uint8, role int8, filing bool) {
			seq++
			planned = append(planned, plannedEvent{at: at.UnixNano(), issue: int32(index), seq: seq, kind: kind,
				role: role, helper: int8(rng.IntN(helperSessions)), filing: filing})
		}
		if issue.parent >= 0 {
			add(issue.opened, kindCreated, 0, true)
		} else {
			add(issue.opened, kindCreated, -1, false)
		}
		middle := issue.events - 1
		if issue.closed != nil {
			middle--
		}
		life := issue.end().Sub(issue.opened)
		role, marked := 0, -1
		for j := range middle {
			// Evenly through the life, so every stretch the life reached holds some of them.
			at := issue.opened.Add(time.Duration(float64(life) * (float64(j) + 0.5 + 0.4*(rng.Float64()-0.5)) / float64(middle)))
			for role+1 < issue.reached && !at.Before(issue.stretch[role+1]) {
				role++
			}
			switch {
			case role != marked && (role == 2 || role == 4):
				add(at, kindUpdated, int8(role), false)
				marked = role
			case rng.Float64() < helperShare:
				add(at, kindComment, -1, false)
			default:
				add(at, kindComment, int8(role), false)
			}
		}
		if issue.closed != nil {
			add(*issue.closed, kindClosed, int8(len(roles)-1), false)
		}
	}
	slices.SortStableFunc(planned, func(a, b plannedEvent) int { return cmp.Compare(a.at, b.at) })
	return planned
}

// writeProject writes fixture's project, whose default repository is the first of its
// repositories, its issues, events, handoffs and observations, and its legion_settings row, and
// answers the history. The project names alice as its default assignee, the column every project
// fills (store migration 20261111210103), as the smaller projects' Legion history does; no eval and
// no count the load prints reads it.
//
// The default is written where both heads read it, the project's default_repo (store migration
// 20261119000230), which main has too. Main at the commit this head merges reads a project's
// default repository from that column, not from the repository mapping (repo_projects), wherever
// it resolves an issue's repository (refs.IssueRepoTerms: the issue's own, else its nearest
// ancestor's, else its project's default), and its Delivery page counts a pull request for the
// project of the issue its row names (delivery_pull_requests.issue_key, which delivery.go
// writes). So both modes write this one project row, the same rows of every table after it, and
// no repository mapping.
func writeProject(ctx context.Context, pool *store.Pool, spec Spec, fixture projectFixture) (projectHistory, error) {
	history := planIssues(spec, fixture)
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, next_number, default_repo) values ($1, $2, 'alice', 1, $3)
	`, fixture.key, fixture.name, fixture.repos[0]); err != nil {
		return projectHistory{}, fmt.Errorf("write the project %s: %w", fixture.key, err)
	}
	if err := copyIssues(ctx, pool, history); err != nil {
		return projectHistory{}, err
	}
	if err := copyEvents(ctx, pool, spec, history); err != nil {
		return projectHistory{}, err
	}
	if err := copyHandoffs(ctx, pool, history); err != nil {
		return projectHistory{}, err
	}
	if err := copyObservations(ctx, pool, history); err != nil {
		return projectHistory{}, err
	}
	if _, err := legion.PutSettings(ctx, pool, fixture.key, evalfixture.StateURL, actor); err != nil {
		return projectHistory{}, fmt.Errorf("write %s's legion_settings: %w", fixture.key, err)
	}
	return history, nil
}

// copier is what copyRows needs of its connection: the pool, or a transaction.
type copier interface {
	CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, rows pgx.CopyFromSource) (int64, error)
}

// copyRows writes rows into table's columns with one COPY, on the pool or in a transaction.
func copyRows(ctx context.Context, db copier, table string, columns []string, rows pgx.CopyFromSource) error {
	if _, err := db.CopyFrom(ctx, pgx.Identifier{table}, columns, rows); err != nil {
		return fmt.Errorf("copy the rows of %s: %w", table, err)
	}
	return nil
}

func copyIssues(ctx context.Context, pool *store.Pool, history projectHistory) error {
	rows := make([][]any, len(history.issues))
	creator := helperActor(0)
	for i, issue := range history.issues {
		var parent, repo, route, claimedBy *string
		var claimedAt *time.Time
		if issue.parent >= 0 {
			parent = &history.issues[issue.parent].key
		}
		if issue.repo != "" {
			repo = &issue.repo
		}
		current := roles[issue.reached-1]
		if issue.routed {
			route = new("session:s-" + current + "-" + issue.key)
		}
		if issue.claimed {
			claimedBy = new(legiontest.SessionActor(current, issue.key))
			claimedAt = &issue.stretch[issue.reached-1]
		}
		updated := issue.end()
		if issue.closed == nil {
			updated = issue.stretch[issue.reached-1]
		}
		rows[i] = []any{issue.key, history.fixture.key, int32(issue.number), issue.title(), issue.status, parent, repo, route, creator,
			issue.opened, updated, issue.closed, int32(issue.events), fmt.Sprintf("%08d", issue.number), issue.priority,
			claimedBy, claimedAt}
	}
	return copyRows(ctx, pool, "issues", []string{"key", "project_key", "number", "title", "status", "parent_key", "repo",
		"route", "created_by", "created_at", "updated_at", "closed_at", "last_seq", "rank", "priority", "claimed_by",
		"claimed_at"}, pgx.CopyFromRows(rows))
}

// eventSource streams the planned events into COPY, building each row's JSON as it goes, so the
// log is never held as text whole.
type eventSource struct {
	history projectHistory
	events  []plannedEvent
	at      int
	values  []any
}

func (s *eventSource) Next() bool {
	s.at++
	return s.at <= len(s.events)
}

func (s *eventSource) Values() ([]any, error) {
	event := s.events[s.at-1]
	issue := s.history.issues[event.issue]
	actorJSON := helperActor(int(event.helper))
	if event.role >= 0 {
		owner := issue.key
		if event.filing {
			owner = s.history.issues[issue.parent].key
		}
		actorJSON = legiontest.SessionActor(roles[event.role], owner)
	}
	at := time.Unix(0, event.at).UTC()
	s.values = append(s.values[:0], issue.key, event.seq, kindNames[event.kind], actorJSON, s.payload(event, issue), false, at, at)
	return s.values, nil
}

// payload is the payload of event, an event of issue. It holds what this history's readers read
// (an issue event's status and a filing's tree, and a comment's body) and the fields that
// Dispatch's own event of that kind carries and that the keyword fill reads (textversions'
// versionRules): an issue event's key and the issue's title, the one copyIssues writes, as api's
// newIssueEventPayload writes them; and a comment's id (legiontest.CommentID) and issue_key. The
// fill stops at the first event that lacks a field it needs, an issue event's title or a comment's
// id or body, and tries that event again, at most a minute apart. It needs no key nor issue_key:
// where the payload lacks one, issueKeyOf and postVersion read the event's own (events.issue_key).
// Load writes the fill's cursor at the loaded log's newest event (keywordFillCaughtUpQuery), but a
// fill that reads the log from its start, as one does after its cursor is reset, would never get
// past the first event without the fields it needs.
func (s *eventSource) payload(event plannedEvent, issue plannedIssue) string {
	var fields string
	switch event.kind {
	case kindCreated:
		fields = `"status":"triage"`
		if event.filing {
			root := s.history.issues[issue.parent].key
			fields = fmt.Sprintf(`"status":"triage","parent":%q,"ancestors":[%q]`, root, root)
		}
	case kindUpdated:
		fields = `"status":"in_progress"`
		if event.role == 4 {
			fields = `"status":"needs_review"`
		}
	case kindClosed:
		fields = `"status":"done"`
	default:
		key := fmt.Sprintf("%s/%d", issue.key, event.seq)
		return fmt.Sprintf(`{"id":%q,"issue_key":%q,"body":%q}`, legiontest.CommentID(issue.key, event.seq), issue.key,
			text(key, 120+int(event.seq*37%360)))
	}
	return fmt.Sprintf(`{"key":%q,"title":%q,%s}`, issue.key, issue.title(), fields)
}

func (s *eventSource) Err() error { return nil }

// copyEvents writes the event log in time order. An event is written as published, since every
// one of them is history a live server published long ago, and none asks to be notified again.
func copyEvents(ctx context.Context, pool *store.Pool, spec Spec, history projectHistory) error {
	return copyRows(ctx, pool, "events", []string{"issue_key", "seq", "type", "actor", "payload", "notify", "published_at", "created_at"},
		&eventSource{history: history, events: planEvents(spec, history)})
}

// copyHandoffs writes the handoff of each stretch a worked issue finished that writes one.
func copyHandoffs(ctx context.Context, pool *store.Pool, history projectHistory) error {
	var rows [][]any
	for _, issue := range history.issues {
		for r, phase := range handoffPhases {
			finished := r+1 < issue.reached || (r < issue.reached && issue.closed != nil)
			if phase == "" || !finished {
				continue
			}
			at := issue.stretch[r+1].Add(-time.Minute)
			rows = append(rows, []any{issue.key, phase, int32(1), text(issue.key+"/"+phase, 400), legiontest.SessionActor(roles[r], issue.key), at})
		}
	}
	slices.SortStableFunc(rows, func(a, b []any) int { return a[5].(time.Time).Compare(b[5].(time.Time)) })
	return copyRows(ctx, pool, "handoffs", []string{"issue_key", "phase", "version", "body", "author", "created_at"}, pgx.CopyFromRows(rows))
}

// copyObservations writes what the sampler saw of each observed issue: the phase as each stretch
// begins (and done at its close), the slot the issue holds from its implementer's stretch to its
// close, so it waits for one while admitted, and each role's claim, working through its stretch
// and idle once it ends.
func copyObservations(ctx context.Context, pool *store.Pool, history projectHistory) error {
	var rows [][]any
	observe := func(issue, field, value string, at time.Time) {
		rows = append(rows, []any{history.fixture.key, issue, field, value, at})
	}
	for _, issue := range history.issues {
		if !issue.observed {
			continue
		}
		for r := range issue.reached {
			observe(issue.key, legion.FieldPhase, fmt.Sprintf("%q", rolePhases[r]), issue.stretch[r])
			observe(issue.key, legion.ClaimFieldPrefix+roles[r], `"working"`, issue.stretch[r].Add(time.Minute))
			if r+1 < issue.reached || issue.closed != nil {
				observe(issue.key, legion.ClaimFieldPrefix+roles[r], `"idle"`, issue.stretch[r+1].Add(-time.Minute))
			}
		}
		if issue.reached > 2 {
			observe(issue.key, legion.FieldSlot, fmt.Sprintf("%d", issue.number%8), issue.stretch[2])
		}
		if issue.closed != nil {
			observe(issue.key, legion.FieldPhase, fmt.Sprintf("%q", legion.PhaseDone), *issue.closed)
			observe(issue.key, legion.FieldSlot, "null", *issue.closed)
		}
	}
	slices.SortStableFunc(rows, func(a, b []any) int { return a[4].(time.Time).Compare(b[4].(time.Time)) })
	return copyRows(ctx, pool, "legion_observations", []string{"project", "issue_key", "field", "value", "observed_at"}, pgx.CopyFromRows(rows))
}

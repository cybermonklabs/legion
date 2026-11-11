// legion_board.go serves GET /api/v1/projects/{key}/legion/board, the Legion view on the project
// Issues page (dispatch://INFRA-1077, the spec's "The Legion view on the Issues page"), and the
// body the fourth live view kind sends (live_frames.go's legionBoardView).
//
// The answer is read from what Dispatch holds, never from the daemon: the sampler reads each
// daemon once a minute under a lease (internal/dispatch/legion) and records what moved, and this
// reads the newest value of each field it recorded, the newest read's own verdict, and the
// project's issues. A board that called the daemon would read it once per open page and once per
// event burst, which is what the per-minute lease exists to prevent.
//
// The per-issue facts the answer carries are read and never computed. issue_facts holds one row
// per issue Legion worked, written by the facts consumer (internal/dispatch/legionfacts), and the
// board serves those rows as they stand. A board that computed them would read the whole metrics
// population once per open page, which is what the stored rows exist to prevent.
//
// Two disciplines, as in the metrics package:
//
//   - The daemon's state is read loosely. Every fact here is a field legion/state.go observes, and
//     a field a daemon does not send is a field the board has no observation of. Nothing takes a
//     slot holder for a root: a card's tree is the daemon's own `tree` where a read carried one
//     (dispatch://INFRA-980 adds it) and the parent chain otherwise.
//   - The columns are the spec's, in one ordered list (legionBoardColumnOrder), and a phase maps
//     to exactly one of them (legion.PhaseColumns), so the view and metric 2's "no role" bar
//     agree on where `awaiting_person` and `held` go (the plan's D11). The ids and that table are
//     the legion package's, since the per-issue facts read them with no card to hand; the labels
//     here are the view's.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// legionBoardColumnOrder is every column of the board, left to right, with the label the view
// shows. Every answer carries all of them, empty where nothing is in them, so a page draws the
// board from the body alone.
var legionBoardColumnOrder = []struct{ id, label string }{
	{legion.ColumnQueue, "Queue"},
	{legion.ColumnDesign, "Design"},
	{legion.ColumnPlanning, "Planning"},
	{legion.ColumnImplementing, "Implementing"},
	{legion.ColumnTesting, "Testing"},
	{legion.ColumnReviewing, "Reviewing"},
	{legion.ColumnAccepting, "Accepting"},
	{legion.ColumnMerging, "Merging"},
	{legion.ColumnAwaiting, "Waiting on a person"},
	{legion.ColumnHeld, "Held"},
}

// legionBoardQueued is whether the issue stands in the Queue column, for what the read knows of
// the daemon's waiting line (legion.Queued). The rule is the legion package's, which takes plain
// values because the stored per-issue facts answer the same column with no card to hand
// (legion.Column); the board calls it here alone.
func legionBoardQueued(issue *legionBoardIssue, queued legion.Queued) bool {
	return legion.Column(issue.phase, issue.slot, queued) == legion.ColumnQueue
}

// legionNotConfiguredCode and legionNotConfiguredMessage are the one refusal a project with no
// Legion daemon gets, whichever route or live view asked (getLegionBoard,
// putLegionProjectSettings, the live hub's worker).
const (
	legionNotConfiguredCode    = "LEGION_NOT_CONFIGURED"
	legionNotConfiguredMessage = "this project has no Legion daemon; set one through PUT /api/v1/settings/legion/{project}"
)

// legionBoardResponse is GET /api/v1/projects/{key}/legion/board: the project the board shows,
// its header, its columns, the stored facts of its issues, the settings its daemon reports with
// where each came from, and when it was computed.
// Mirrors packages/contracts/src/dispatch-api.ts's LegionBoardResponse exactly.
type legionBoardResponse struct {
	Project string              `json:"project"`
	Header  legionBoardHeader   `json:"header"`
	Columns []legionBoardColumn `json:"columns"`
	// Issues is the stored facts of the project's issues, by issue key. There is an entry for
	// every issue with Legion phase work, open or closed, whatever its hours are, because the
	// facts consumer writes a row for exactly those issues. An issue on no card carries an entry
	// all the same, and an issue whose row is not written yet carries none.
	Issues map[string]legionBoardFacts `json:"issues"`
	// Settings is the settings the daemon reports, which hold no secret, and SettingSources where
	// each came from — the file or the route, who changed it and when. Both are the daemon's own
	// bytes, key by key, and both are {} until a read records them, so a project whose daemon has
	// never answered still renders. The settings control on the Legion view reads them.
	Settings       map[string]json.RawMessage `json:"settings"`
	SettingSources map[string]json.RawMessage `json:"setting_sources"`
	ComputedAt     time.Time                  `json:"computed_at"`
}

// legionBoardFacts is one issue's stored Legion facts on the wire: the issue_facts row
// legionfacts.Row holds, less the project the board names and the issue the map is keyed by.
// Every value is the one the facts consumer computed and stored, and the board adds nothing to
// any of them.
//
//   - OpenedAt is when the issue was opened.
//   - ClosedAt is when it closed, null while it is open.
//   - HoursByPhase is the hours it spent in each board column, by column id, a column with no
//     hours left out.
//   - WorkHours is its work time to merge, less the time it stood parked and queued.
//   - ReviewRounds is how many times a reviewer sent its work back.
//   - RootKey is the root of its tree.
//   - TreeDepth is how far below that root it stands, 0 for the root itself.
//   - SlotHours is how long it held an admission slot of its own.
//   - WaitingReason is why it waits, "" where it waits for nothing.
//   - WorkFirstAt and WorkLastAt are the first and the last moment of its measured work, null
//     until it has some.
//   - UpdatedAt is when its row was written, which the header's facts_updated_at is the oldest
//     of.
type legionBoardFacts struct {
	OpenedAt      time.Time          `json:"opened_at"`
	ClosedAt      *time.Time         `json:"closed_at"`
	HoursByPhase  map[string]float64 `json:"hours_by_phase"`
	WorkHours     float64            `json:"work_hours"`
	ReviewRounds  int                `json:"review_rounds"`
	RootKey       string             `json:"root_key"`
	TreeDepth     int                `json:"tree_depth"`
	SlotHours     float64            `json:"slot_hours"`
	WaitingReason string             `json:"waiting_reason"`
	WorkFirstAt   *time.Time         `json:"work_first_at"`
	WorkLastAt    *time.Time         `json:"work_last_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
}

// legionBoardHeader is what the view's header shows: the slots in use against the cap, the live
// trees, the queue's length, the last read of the daemon — when it was made, whether it
// succeeded and why it did not — and how fresh the stored facts are. SlotCap is null until a
// read records the daemon's admission block, Trees is 0 until a daemon that reports its live
// trees does (dispatch://INFRA-980), and ReadAt is null for a project whose daemon has never
// answered at all, which shows the board with empty columns.
//
// The three facts fields are read together for one reason: a reader can then tell a project with
// no Legion work from a project whose backfill has not run. The first carries no Legion issues
// and no rows; the second carries Legion issues and no rows. The cursor age alone cannot tell
// them apart, because it is the deployment's one consumer rather than the project's, and every
// project of a running deployment reads the same age.
type legionBoardHeader struct {
	SlotsInUse  int        `json:"slots_in_use"`
	SlotCap     *int       `json:"slot_cap"`
	Trees       int        `json:"trees"`
	QueueLength int        `json:"queue_length"`
	ReadAt      *time.Time `json:"read_at"`
	ReadOK      bool       `json:"read_ok"`
	ReadError   *string    `json:"read_error"`
	// LegionIssues is how many issues of this project carry Legion phase work, which is the
	// same set the facts consumer writes a row for. The board reads that set already, for its
	// cards and its tree walk, so the count costs no read of its own.
	LegionIssues int `json:"legion_issues"`
	// FactsUpdatedAt is the oldest updated_at of the stored rows this answer served, null where
	// it served none.
	FactsUpdatedAt *time.Time `json:"facts_updated_at"`
	// FactsCursorAgeSeconds is how long ago the facts consumer's oldest cursor last moved
	// (legionfacts.CursorAge), measured when the board was read, and null where the consumer
	// holds no cursor row at all. It is of the deployment's one consumer and not of this
	// project, so it says how fresh the facts are and never whether this project has any.
	FactsCursorAgeSeconds *int `json:"facts_cursor_age_seconds"`
}

// legionBoardColumn is one column: its id, the label the view shows, and its cards in the order
// the view draws them. ID is one of the ten columns, in the order legionBoardColumnOrder draws
// them; the enum is what makes the generated type carry that set rather than a bare string.
type legionBoardColumn struct {
	ID    string            `json:"id" enum:"queue,design,planning,implementing,testing,reviewing,accepting,merging,awaiting_person,held"`
	Label string            `json:"label"`
	Cards []legionBoardCard `json:"cards"`
}

// legionBoardCard is one issue on the board. Key, Title and Priority come from Dispatch's own
// issues table, joined on the daemon's key (the plan's D13; the daemon's state carries none of the
// three). Phase, PhaseSince, Slot, Roles, ChecksVerdict and WaitingOn are the newest observations
// of that issue, PhaseSince being when the phase it is in was first recorded — the time in the
// phase the card shows. BlockedBy names the issues a blocker link holds it on, and Children counts
// the issues of its tree that its tree has not started, which are on no card of their own.
type legionBoardCard struct {
	Key        string     `json:"key"`
	Title      string     `json:"title"`
	Priority   *int       `json:"priority"`
	Phase      string     `json:"phase"`
	PhaseSince *time.Time `json:"phase_since"`
	Slot       *int       `json:"slot"`
	// TreeSlot is the slot of the issue's tree root, for an issue that holds none of its own: a
	// slot belongs to the tree's root, and a child works under it. Null for a root, an issue
	// holding its own slot, and a tree whose root holds none.
	TreeSlot *int `json:"tree_slot"`
	// SlotSince is when the issue was admitted to its own slot; null with no slot of its own.
	SlotSince *time.Time `json:"slot_since"`
	// WaitingPlace is the issue's place in the daemon's waiting line, from 1; null for an issue
	// the line does not name.
	WaitingPlace  *int             `json:"waiting_place"`
	Roles         []string         `json:"roles"`
	ChecksVerdict *string          `json:"checks_verdict"`
	BlockedBy     []string         `json:"blocked_by"`
	WaitingOn     *legionBoardWait `json:"waiting_on"`
	// SlotUse is what the slot this issue holds of its own is used for (legion.SlotUse); null for
	// an issue holding no slot of its own.
	SlotUse *string `json:"slot_use"`
	// Waiting is why the issue waits, since when, and the issues a blocker link holds it on
	// (legion.CardWaiting); null for an issue that waits for nothing.
	Waiting  *legionBoardWaiting `json:"waiting"`
	Tree     string              `json:"tree"`
	Children int                 `json:"children"`
	// wait is the daemon's own record WaitingOn was made from (legion.Wait), which the waiting
	// rule reads once every card is made and numbered (cardWaiting). It carries no JSON: the
	// wire shape is WaitingOn, and this is the value that one was made from.
	wait *legion.Wait
}

// legionBoardWait is what a root in `awaiting_person` waits on, as the daemon recorded it: the
// kind of wait (`ask`, `gate`, or whatever a later daemon names), when it began, and when a person
// answered it. An answered root keeps the slot it held and stands in the queue until the pass
// that resumes it.
type legionBoardWait struct {
	Kind       string     `json:"kind"`
	Since      *time.Time `json:"since"`
	AnsweredAt *time.Time `json:"answered_at"`
}

// legionBoardWaiting is why one card's issue waits, since when, and the issues a blocker link
// holds it on, as the rule answered it (legion.CardWaiting). Since is when the wait began: when
// the person was asked for a wait on a person, and the phase entry otherwise; null where neither
// was recorded.
type legionBoardWaiting struct {
	Reason    string     `json:"reason"`
	Since     *time.Time `json:"since"`
	BlockedBy []string   `json:"blocked_by"`
}

// getLegionBoard answers GET /api/v1/projects/{key}/legion/board. The project is the path's key,
// so one address answers one project's board. A project that names no Legion daemon is 404
// LEGION_NOT_CONFIGURED, which is what shows no view; a project whose daemon has never answered
// is answered the board with empty columns and the read's failure in its header.
func (s *server) getLegionBoard(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	project, err := s.loadProject(r.Context(), s.deps.Store.Pool, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	population, err := s.readLegionBoard(r.Context(), project.Key)
	if err != nil {
		if errors.Is(err, legion.ErrNoSettings) {
			writeError(w, legionNotConfiguredCode, http.StatusNotFound, legionNotConfiguredMessage)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, legionBoardCompute(population, time.Now().UTC()))
}

// legionBoardPopulation is every read one board answer makes: the project's Legion setting, the
// newest read of its daemon, the newest value of every field the sampler recorded, and the
// project's own issues in Dispatch's rank order.
type legionBoardPopulation struct {
	project string
	// read is the newest legion_reads row, nil for a project whose daemon was never read.
	read *legionBoardDaemonRead
	// admission is the daemon's admission block as the newest read recorded it, nil while no read
	// has recorded one: before the first read of a daemon, and for a daemon that sends none.
	admission *legion.Admission
	// settings and settingSources are the daemon's own two blocks as the newest read recorded
	// them, nil while no read has recorded either.
	settings, settingSources map[string]json.RawMessage
	// issues is every issue the daemon reported, by key, with what Dispatch holds of it beside the
	// observations.
	issues map[string]*legionBoardIssue
	// parents is the parent Dispatch records for every issue of the project, the chain the tree
	// walk climbs: an issue the daemon reports nothing of can still be the Legion issue above one
	// it does (treeRoot).
	parents map[string]string
	// phaseWork is the server's set of the issues that carry Legion phase work, the other half of
	// what makes an issue Legion's for the walk (legion_phase_work.go); nil is an empty set.
	phaseWork *legionPhaseWork
	// facts is the stored Legion facts of the project's issues, by key, as legionfacts.Read
	// answered them: rows, and no computation of this server's own.
	facts map[string]legionfacts.Row
	// cursorAge is how long ago the facts consumer's oldest cursor last moved, in seconds,
	// measured when this population was read; nil where the consumer holds no cursor row.
	cursorAge *int
	// ranked is the project's issue keys in Dispatch's rank order, which is the Queue column's
	// order when no read has recorded a waiting line.
	ranked []string
}

// legionBoardDaemonRead is the newest legion_reads row: when the read was made, whether it
// succeeded, and why it did not.
type legionBoardDaemonRead struct {
	at      time.Time
	ok      bool
	failure *string
}

// legionBoardIssue is one issue the board may show: the newest observation of each field the
// sampler records, and the title, priority and parent Dispatch holds for the same key.
type legionBoardIssue struct {
	key string
	// phase and phaseSince are the newest phase observation and when it was recorded, which is
	// when the issue entered that phase (a row is written only where the value moved).
	phase      string
	phaseSince time.Time
	slot       *int
	// slotSince is when the newest slot observation was recorded, which is when the issue was
	// admitted to that slot.
	slotSince time.Time
	// tree is the daemon's own `tree` for the issue, empty where it named none.
	tree          string
	waitingOn     *legion.Wait
	checksVerdict string
	// roles are the roles of the issue whose claim state is `working`, sorted.
	roles []string
	// title and priority are Dispatch's, empty for an issue Dispatch does not hold; the parent
	// Dispatch records is p.parents, which holds every issue of the project and not only the
	// ones the daemon reports.
	title    string
	priority *int
}

// readLegionBoard reads one project's board. A project with no Legion setting is legion's
// ErrNoSettings, which the route answers 404 and the live hub refuses the subscription with.
//
// The stored facts are one read of one project's rows, by the index on (project_key, issue_key),
// and the board serves what that read answered. Nothing here computes a fact: no elapsed time is
// added to a stored value, no metrics function is called, and no population is held between
// calls.
func (s *server) readLegionBoard(ctx context.Context, project string) (*legionBoardPopulation, error) {
	pool := s.deps.Store.Pool
	if _, err := legion.GetSettings(ctx, pool, project); err != nil {
		return nil, err
	}
	population := &legionBoardPopulation{
		project: project, issues: map[string]*legionBoardIssue{}, parents: map[string]string{},
		phaseWork: &s.legionPhaseWork,
	}
	// The walk reads the issues that carry Legion phase work beside the daemon's own reports, and
	// the set is read once rather than per board (legion_phase_work.go).
	if err := s.legionPhaseWork.ensure(ctx, pool); err != nil {
		return nil, err
	}
	if err := population.readDaemonRead(ctx, pool); err != nil {
		return nil, err
	}
	if err := population.readObservations(ctx, pool); err != nil {
		return nil, err
	}
	if err := population.readIssues(ctx, pool); err != nil {
		return nil, err
	}
	// The stored facts: one read of this project's rows, and the age of the consumer's oldest
	// cursor, which says how fresh they are.
	facts, err := legionfacts.Read(ctx, pool, project)
	if err != nil {
		return nil, err
	}
	population.facts = facts
	age, held, err := legionfacts.CursorAge(ctx, pool, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if held {
		seconds := int(age.Seconds())
		population.cursorAge = &seconds
	}
	return population, nil
}

// readDaemonRead reads the newest read of the project's daemon, the freshness the header shows.
func (p *legionBoardPopulation) readDaemonRead(ctx context.Context, pool *store.Pool) error {
	var read legionBoardDaemonRead
	err := pool.QueryRow(ctx, `
		select read_at, ok, error
		from legion_reads
		where project = $1
		order by read_at desc, id desc
		limit 1
	`, p.project).Scan(&read.at, &read.ok, &read.failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	p.read = &read
	return nil
}

// readObservations reads the newest value of every field the sampler recorded for the project: one
// row per issue and field, which the unique index (project, issue_key, field, observed_at) answers
// in order. The rows under legion.AdmissionKey belong to no issue: the admission block, the
// daemon's settings and where each setting came from.
func (p *legionBoardPopulation) readObservations(ctx context.Context, pool *store.Pool) error {
	rows, err := pool.Query(ctx, `
		select distinct on (issue_key, field) issue_key, field, value, observed_at
		from legion_observations
		where project = $1
		order by issue_key, field, observed_at desc
	`, p.project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, field string
		var value []byte
		var at time.Time
		if err := rows.Scan(&key, &field, &value, &at); err != nil {
			return err
		}
		if key == legion.AdmissionKey {
			p.observeProject(field, value)
			continue
		}
		issue := p.issues[key]
		if issue == nil {
			issue = &legionBoardIssue{key: key}
			p.issues[key] = issue
		}
		issue.observe(field, value, at)
	}
	return rows.Err()
}

// observeProject takes one of the project's own newest observations: a value of another shape
// than the field is read as leaves the field unrecorded, since the daemon owns the wire shape.
func (p *legionBoardPopulation) observeProject(field string, value []byte) {
	switch field {
	case legion.FieldAdmission:
		var admission legion.Admission
		if err := json.Unmarshal(value, &admission); err == nil {
			p.admission = &admission
		}
	case legion.FieldSettings:
		_ = json.Unmarshal(value, &p.settings)
	case legion.FieldSettingSources:
		_ = json.Unmarshal(value, &p.settingSources)
	}
}

// observe takes one newest observation into the issue. A value that is not the shape the field is
// read as leaves the field as it stands: the daemon owns the wire shape, and a reader of one names
// what it expects rather than trusting it. The slot and the wait are decoded by the rules'
// package (legion.ParseSlot, legion.ParseWait), which the stored per-issue facts decode by too.
func (issue *legionBoardIssue) observe(field string, value []byte, at time.Time) {
	switch {
	case field == legion.FieldPhase:
		issue.phase, issue.phaseSince = legion.JSONString(value), at
	case field == legion.FieldTree:
		issue.tree = legion.JSONString(value)
	case field == legion.FieldSlot:
		if slot, read := legion.ParseSlot(value); read {
			issue.slot, issue.slotSince = slot, at
		}
	case field == legion.FieldWaitingOn:
		if wait, read := legion.ParseWait(value); read {
			issue.waitingOn = wait
		}
	case field == legion.FieldPullRequestChecksVerdict:
		issue.checksVerdict = legion.JSONString(value)
	case strings.HasPrefix(field, legion.ClaimFieldPrefix):
		role := strings.TrimPrefix(field, legion.ClaimFieldPrefix)
		working := legion.JSONString(value) == "working"
		held := slices.Contains(issue.roles, role)
		switch {
		case working && !held:
			issue.roles = append(issue.roles, role)
			slices.Sort(issue.roles)
		case !working && held:
			issue.roles = slices.DeleteFunc(issue.roles, func(r string) bool { return r == role })
		}
	}
}

// readIssues reads the project's issues in Dispatch's rank order, records the parent of every one
// of them — the chain the tree walk climbs, whatever the daemon reports of each — and fills the
// title, priority and parent of every issue the daemon reported. An issue the daemon reports that
// Dispatch does not hold keeps an empty title: the board shows what the daemon says, and a card
// with no title is better than a card the reader never sees.
func (p *legionBoardPopulation) readIssues(ctx context.Context, pool *store.Pool) error {
	rows, err := pool.Query(ctx, `
		select key, title, priority, coalesce(parent_key, '')
		from issues
		where project_key = $1
		order by rank asc, created_at asc, key asc
	`, p.project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, title, parent string
		var priority *int
		if err := rows.Scan(&key, &title, &priority, &parent); err != nil {
			return err
		}
		p.ranked = append(p.ranked, key)
		p.parents[key] = parent
		if issue := p.issues[key]; issue != nil {
			issue.title, issue.priority = title, priority
		}
	}
	return rows.Err()
}

// issueKeys is every issue the board's answer names — the issues the daemon reported and the
// issues whose stored facts the answer carries — which is what the live hub marks its view dirty
// by (delivery_live_hub_intake.go's dirtyIssueLocked). A facts write of an issue on no card
// still moves the answer, because the entry of it in the issues map moves.
func (p *legionBoardPopulation) issueKeys() map[string]struct{} {
	keys := make(map[string]struct{}, len(p.issues)+len(p.facts))
	for key := range p.facts {
		keys[key] = struct{}{}
	}
	for key := range p.issues {
		keys[key] = struct{}{}
	}
	return keys
}

// treeRoot is the issue's tree root, by the one rule the metrics place an issue in a tree with
// (legion.TreeRoot): the `tree` the daemon named where a read carried one (dispatch://INFRA-980),
// and the topmost Legion issue of its parent chain otherwise.
//
// The walk knows every issue of the project (parents) and every issue the daemon reports, and an
// issue is Legion's for it by the one rule both readers state (dispatch://INFRA-1077's spec, "The
// top issue of a tree", and legion/metrics/population.go's isLegionIssue): the daemon reports it,
// or it carries Legion phase work. So a chain climbs through an issue that carries a handoff or a
// role session's event and that the daemon never reported, and stops below an issue that is
// neither, which is what keeps a tree out of the tracking issues above it. An issue of another
// project, which only a daemon that reports one brings here, is a root of its own.
func (p *legionBoardPopulation) treeRoot(key string) string {
	return legion.TreeRoot(key, p.treeNode)
}

// treeNode is what that walk reads of one issue: the daemon's `tree`, Dispatch's parent, and
// whether the issue is Legion's.
func (p *legionBoardPopulation) treeNode(key string) (legion.TreeNode, bool) {
	issue, reported := p.issues[key]
	parent, held := p.parents[key]
	if !reported && !held {
		return legion.TreeNode{}, false
	}
	node := legion.TreeNode{Parent: parent, Legion: reported || p.phaseWork.has(key)}
	if reported {
		node.Tree = issue.tree
	}
	return node, true
}

// legionBoardCards answers which issues carry a card and, for an issue that carries none, the
// nearest issue above it in its tree that does. The two answers are worked out together, since
// the one card that depends on the chain is an `admitted` issue holding no slot: it is an
// unstarted child where a card stands above it, and a card of its own where none does.
type legionBoardCards struct {
	population *legionBoardPopulation
	// queued is the issues the daemon's own state put in the Queue column, which carry a card
	// whatever their phase; carded keeps each answer once it is known.
	queued map[string]bool
	carded map[string]bool
}

// has is whether the issue carries a card.
func (c *legionBoardCards) has(key string) bool {
	if held, known := c.carded[key]; known {
		return held
	}
	// A chain that loops reads false for the issue being worked out, so the walk ends.
	c.carded[key] = false
	issue := c.population.issues[key]
	if issue == nil {
		return false
	}
	_, named := legion.PhaseColumns[issue.phase]
	held := false
	switch {
	case c.queued[key]:
		held = true
	case !named:
		held = false
	case issue.phase == legion.PhaseAdmitted && issue.slot == nil:
		held = c.nearestCard(key) == ""
	default:
		held = true
	}
	c.carded[key] = held
	return held
}

// nearestCard is the nearest issue above key in its tree that carries a card, and "" where none
// does. It climbs the one chain the tree rule names (legion.TreeChain), so a count never leaves
// the tree the walk puts the issue in.
func (c *legionBoardCards) nearestCard(key string) string {
	chain := legion.TreeChain(key, c.population.treeNode)
	for _, above := range chain[1:] {
		if c.has(above) {
			return above
		}
	}
	return ""
}

// legionBoardCompute is GET /api/v1/projects/{key}/legion/board's body for a population read for
// it. It reads nothing: the live hub answers the same body from the population it already holds,
// and now is only computed_at. The issues map is the stored rows the read answered, turned into
// the wire shape and nothing more.
//
// A card's column is the first of these that holds: the daemon's waiting line, in its own order,
// which the view does not re-sort (the spec's "Rule details": that order puts answered roots
// first, then follows Dispatch's rank); the issues a blocker link holds, after them in the Queue
// column — of both lists only the issues that have not started work (legionBoardQueued), since
// an issue that has started keeps the column of its phase and carries its place in the line on
// its card; and otherwise the column its phase falls in. An issue the daemon reports in
// `admitted` that holds no slot and that neither list names is a child its tree has not started:
// it is on no card, and the nearest card above it in its tree counts it. Where no issue above it
// carries a card — its tree climbs through issues the daemon reports nothing of, or through
// issues whose phase is on no column — it is a card of its own, since a count on an issue the
// board shows nothing of is a count no reader can see. Where no read has recorded an admission
// block the waiting line is the same rule read for a reader that knows no line
// (legion.QueuedUnread), over Dispatch's rank order, which is the order the daemon's own line
// follows once a root's wait is answered.
func legionBoardCompute(p *legionBoardPopulation, now time.Time) legionBoardResponse {
	cards := map[string][]legionBoardCard{}
	placed := map[string]bool{}
	blockers := map[string][]string{}
	var waiting []string
	if p.admission != nil {
		waiting = p.admission.Waiting
		for _, blocked := range p.admission.Blocked {
			blockers[blocked.Issue] = blocked.BlockedBy
		}
	}

	// The queue: the daemon's waiting line in its own order, then the issues a blocker link holds
	// that the line does not already name, of the issues that have not started work
	// (legionBoardQueued). Each issue the line names keeps its place in it, on whichever column
	// its card is.
	queued := make([]string, 0, len(waiting)+len(blockers))
	seen := map[string]bool{}
	places := map[string]int{}
	for _, key := range waiting {
		if p.issues[key] == nil || seen[key] {
			continue
		}
		seen[key] = true
		places[key] = len(places) + 1
		if legionBoardQueued(p.issues[key], legion.QueuedYes) {
			queued = append(queued, key)
		}
	}
	blocked := make([]string, 0, len(blockers))
	for key := range blockers {
		if p.issues[key] != nil && !seen[key] && legionBoardQueued(p.issues[key], legion.QueuedYes) {
			blocked = append(blocked, key)
		}
	}
	p.sortByRank(blocked)
	queued = append(queued, blocked...)
	// Which issues carry a card: the queue the daemon's state named, and the rest by phase. The
	// fallback queue below and the unstarted-child count both read it, so no card is lost to the
	// tree walk and no count lands on an issue the board shows nothing of.
	board := &legionBoardCards{population: p, queued: seen, carded: map[string]bool{}}
	if p.admission == nil {
		// No read has recorded a waiting line: the issues the one column rule queues for a
		// reader that knows none, of those no card above them holds, in Dispatch's rank order.
		for _, key := range p.ranked {
			issue := p.issues[key]
			if issue != nil && legionBoardQueued(issue, legion.QueuedUnread) && board.has(key) {
				queued = append(queued, key)
			}
		}
	}
	for _, key := range queued {
		placed[key] = true
	}

	// Every other issue the daemon reports, by its phase. An `admitted` issue holding no slot that
	// the queue does not name has not been started: it is counted on the nearest card above it,
	// and carries one of its own where there is none.
	uncarded := map[string]int{}
	for _, key := range p.sortedKeys() {
		issue := p.issues[key]
		if placed[key] {
			continue
		}
		column, named := legion.PhaseColumns[issue.phase]
		if !named {
			continue
		}
		if issue.phase == legion.PhaseAdmitted && issue.slot == nil {
			if above := board.nearestCard(key); above != "" {
				uncarded[above]++
				continue
			}
		}
		placed[key] = true
		cards[column] = append(cards[column], p.card(issue, blockers[key], 0))
	}
	for _, key := range queued {
		cards[legion.ColumnQueue] = append(cards[legion.ColumnQueue], p.card(p.issues[key], blockers[key], uncarded[key]))
	}
	for _, made := range cards {
		for index := range made {
			if place, named := places[made[index].Key]; named {
				made[index].WaitingPlace = &place
			}
		}
	}
	// A root's count is known only once every issue has been placed, so the cards already made
	// take theirs now.
	for column, made := range cards {
		if column == legion.ColumnQueue {
			continue
		}
		for index := range made {
			made[index].Children = uncarded[made[index].Key]
		}
	}

	// What each slot is used for and why each card waits: both read the whole board — a held
	// slot is idle until a child of its tree works, and a wait reads the card's place and its
	// column — so both are taken once every card is made and numbered.
	childWorks := map[string]bool{}
	for _, made := range cards {
		for _, card := range made {
			if card.Tree != card.Key && legion.ChildWork[card.Phase] {
				childWorks[card.Tree] = true
			}
		}
	}
	for column, made := range cards {
		for index := range made {
			card := &made[index]
			if use := legion.SlotUse(card.Phase, card.Roles, card.Slot, childWorks[card.Key]); use != "" {
				card.SlotUse = &use
			}
			card.Waiting = cardWaiting(card, column)
		}
	}

	columns := make([]legionBoardColumn, 0, len(legionBoardColumnOrder))
	for _, column := range legionBoardColumnOrder {
		made := cards[column.id]
		if made == nil {
			made = []legionBoardCard{}
		}
		columns = append(columns, legionBoardColumn{ID: column.id, Label: column.label, Cards: made})
	}

	header := legionBoardHeader{QueueLength: len(cards[legion.ColumnQueue])}
	// How many issues of this project Legion has worked, counted over the set the board already
	// read: it is what tells a project with no Legion work from one whose facts have not been
	// written yet, since neither carries a stored row.
	for _, key := range p.ranked {
		if p.phaseWork.has(key) {
			header.LegionIssues++
		}
	}
	// Slots in use are the daemon's own count where it reports one. dispatch://INFRA-980 makes
	// admission.active list every issue that holds a slot, children included, so it is the
	// authority; before that release the daemon lists roots there and only a root carries a slot,
	// so the two agree. A read that recorded no admission block falls back to the issues whose own
	// slot the sampler saw.
	if p.admission != nil && len(p.admission.Active) > 0 {
		header.SlotsInUse = len(p.admission.Active)
	} else {
		for _, issue := range p.issues {
			if issue.slot != nil {
				header.SlotsInUse++
			}
		}
	}
	if p.admission != nil {
		header.SlotCap = p.admission.Cap
		// Trees is the live trees' roots, which only a daemon on dispatch://INFRA-980 reports; it
		// is 0 for an older one, where the view shows no tree count rather than a wrong one.
		header.Trees = len(p.admission.Trees)
	}
	if read := p.read; read != nil {
		at := read.at.UTC()
		header.ReadAt, header.ReadOK, header.ReadError = &at, read.ok, read.failure
	}
	// The stored facts as they were read, and how fresh they are: the oldest updated_at of the
	// rows this answer serves, null where it serves none, and the age the read measured of the
	// consumer's oldest cursor.
	issues := make(map[string]legionBoardFacts, len(p.facts))
	for key, row := range p.facts {
		hours := row.HoursByPhase
		if hours == nil {
			// A row whose work took no measured time carries {} rather than null, as the two
			// settings blocks do, so a page reads one shape.
			hours = map[string]float64{}
		}
		issues[key] = legionBoardFacts{
			OpenedAt: row.OpenedAt.UTC(), ClosedAt: legionFactTime(row.ClosedAt), HoursByPhase: hours,
			WorkHours: row.WorkHours, ReviewRounds: row.ReviewRounds, RootKey: row.RootKey,
			TreeDepth: row.TreeDepth, SlotHours: row.SlotHours, WaitingReason: row.WaitingReason,
			WorkFirstAt: legionFactTime(row.WorkFirstAt), WorkLastAt: legionFactTime(row.WorkLastAt),
			UpdatedAt: row.UpdatedAt.UTC(),
		}
		if header.FactsUpdatedAt == nil || row.UpdatedAt.Before(*header.FactsUpdatedAt) {
			at := row.UpdatedAt.UTC()
			header.FactsUpdatedAt = &at
		}
	}
	header.FactsCursorAgeSeconds = p.cursorAge
	// Both blocks are objects, empty rather than null, so a page reads one shape whether or not a
	// read has recorded them.
	settings, sources := p.settings, p.settingSources
	if settings == nil {
		settings = map[string]json.RawMessage{}
	}
	if sources == nil {
		sources = map[string]json.RawMessage{}
	}
	return legionBoardResponse{
		Project: p.project, Header: header, Columns: columns, Issues: issues,
		Settings: settings, SettingSources: sources, ComputedAt: now,
	}
}

// legionFactTime is one of a stored row's optional moments on the wire: the same moment in UTC,
// so every time the board writes reads one way, and nil for a row that holds none.
func legionFactTime(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	utc := at.UTC()
	return &utc
}

// card is one issue's card, with the issues holding it and the children of its tree that are on no
// card of their own.
func (p *legionBoardPopulation) card(issue *legionBoardIssue, blockedBy []string, children int) legionBoardCard {
	card := legionBoardCard{
		Key: issue.key, Title: issue.title, Priority: issue.priority, Phase: issue.phase,
		Slot: issue.slot, Roles: issue.roles, BlockedBy: blockedBy,
		Tree: p.treeRoot(issue.key), Children: children, wait: issue.waitingOn,
	}
	// The wait the sampler recorded is decoded once, into the rule's own shape (legion.Wait);
	// the body carries it in the shape the view reads, which is this one mapping. The card keeps
	// the decoded value as well, which is what the waiting rule reads (cardWaiting).
	if wait := issue.waitingOn; wait != nil {
		card.WaitingOn = &legionBoardWait{Kind: wait.Kind, Since: wait.Since, AnsweredAt: wait.AnsweredAt}
	}
	if card.Slot == nil && card.Tree != issue.key {
		if root := p.issues[card.Tree]; root != nil && root.slot != nil {
			slot := *root.slot
			card.TreeSlot = &slot
		}
	}
	if card.Roles == nil {
		card.Roles = []string{}
	}
	if card.BlockedBy == nil {
		card.BlockedBy = []string{}
	}
	if !issue.phaseSince.IsZero() {
		since := issue.phaseSince.UTC()
		card.PhaseSince = &since
	}
	if issue.slot != nil && !issue.slotSince.IsZero() {
		since := issue.slotSince.UTC()
		card.SlotSince = &since
	}
	if issue.checksVerdict != "" {
		verdict := issue.checksVerdict
		card.ChecksVerdict = &verdict
	}
	return card
}

// cardWaiting is why one card's issue waits, in the shape the body carries. The rule is the
// legion package's, which reads plain values because the per-issue facts answer it with no card
// to hand (legion.CardWaiting); this is its one wire mapping. The wait it reads is the one the
// card was made from, never a second read of the issue.
func cardWaiting(card *legionBoardCard, column string) *legionBoardWaiting {
	waiting := legion.CardWaiting(legion.WaitingInput{
		Phase: card.Phase, Column: column, WaitingPlace: card.WaitingPlace,
		BlockedBy: card.BlockedBy, PhaseSince: card.PhaseSince, WaitingOn: card.wait,
	})
	if waiting == nil {
		return nil
	}
	return &legionBoardWaiting{Reason: waiting.Reason, Since: waiting.Since, BlockedBy: waiting.BlockedBy}
}

// sortedKeys is every issue the daemon reported, in Dispatch's rank order, with the issues
// Dispatch does not hold after them by key, so one state always makes one board.
func (p *legionBoardPopulation) sortedKeys() []string {
	keys := slices.Collect(maps.Keys(p.issues))
	p.sortByRank(keys)
	return keys
}

// sortByRank orders keys by Dispatch's rank, the order readIssues read them in, and puts an issue
// Dispatch does not hold after the rest, by key.
func (p *legionBoardPopulation) sortByRank(keys []string) {
	rank := make(map[string]int, len(p.ranked))
	for index, key := range p.ranked {
		rank[key] = index
	}
	slices.SortFunc(keys, func(a, b string) int {
		ra, hasA := rank[a]
		rb, hasB := rank[b]
		switch {
		case hasA && hasB:
			return ra - rb
		case hasA:
			return -1
		case hasB:
			return 1
		default:
			return strings.Compare(a, b)
		}
	})
}

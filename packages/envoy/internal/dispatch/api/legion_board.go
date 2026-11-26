// legion_board.go serves GET /api/v1/projects/{key}/legion/board, one project's Legion board from
// what the sampler recorded (dispatch://INFRA-1077), and the body the `legion` live view kind
// sends (live_frames.go's legionBoardView). No dashboard page draws it: the route and the live
// view kind are all there is of it.
//
// The answer is read from what Dispatch holds, never from the daemon: the sampler reads each
// daemon once a minute under a lease (internal/dispatch/legion) and records what moved, and this
// reads the newest value of each field it recorded, the newest read's own verdict, and the
// project's issues. A board that called the daemon would read it once per open page and once per
// event burst, which is what the per-minute lease exists to prevent.
//
// Two disciplines, as in the metrics package:
//
//   - The daemon's state is read loosely. Every fact here is a field legion/state.go observes, and
//     a field a daemon does not send is a field the board has no observation of. Nothing takes a
//     slot holder for a root: a card's tree is the daemon's own `tree` where a read carried one
//     (dispatch://INFRA-980 adds it) and the parent chain otherwise.
//   - The columns are the spec's, with Waiting on children for the phase dispatch://INFRA-3320
//     added, in one ordered list (legionBoardColumnOrder), and a phase maps to exactly one of them
//     (legionPhaseColumns), so the view and metric 2's "no role" bar agree on where
//     `awaiting_children`, `awaiting_person` and `held` go (the plan's D11).
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
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The board's columns. Queue holds the daemon's waiting line and, after it, the issues a blocker
// link holds; Design the issues whose phase is still `admitted` that are on a card at all — the
// slot holders, and the ones no card stands above, which are on no other issue's count
// (legionBoardCompute); Step the issues at a step of a role the deployment defines, whatever its
// type; Retro `retro` alone; Merging `merging` and `awaiting_merge`; Waiting on children the
// parents in `awaiting_children`, whether or not the daemon's blocked list names them
// (legionBoardCompute); and Held the phase metric 2 puts on the "no role" bar beside
// `awaiting_person` and `awaiting_children` (the plan's D11).
const (
	legionColumnQueue            = "queue"
	legionColumnDesign           = "design"
	legionColumnPlanning         = "planning"
	legionColumnImplementing     = "implementing"
	legionColumnTesting          = "testing"
	legionColumnReviewing        = "reviewing"
	legionColumnStep             = "step"
	legionColumnAccepting        = "accepting"
	legionColumnRetro            = "retro"
	legionColumnMerging          = "merging"
	legionColumnAwaitingChildren = "awaiting_children"
	legionColumnAwaitingPerson   = "awaiting_person"
	legionColumnHeld             = "held"
)

// legionBoardColumnOrder is every column of the board, left to right, with the label the view
// shows. Every answer carries all of them, empty where nothing is in them, so a page draws the
// board from the body alone.
var legionBoardColumnOrder = []struct{ id, label string }{
	{legionColumnQueue, "Queue"},
	{legionColumnDesign, "Design"},
	{legionColumnPlanning, "Planning"},
	{legionColumnImplementing, "Implementing"},
	{legionColumnTesting, "Testing"},
	{legionColumnReviewing, "Reviewing"},
	{legionColumnStep, "Deployment step"},
	{legionColumnAccepting, "Accepting"},
	{legionColumnRetro, "Retro"},
	{legionColumnMerging, "Merging"},
	{legionColumnAwaitingChildren, "Waiting on children"},
	{legionColumnAwaitingPerson, "Waiting on a person"},
	{legionColumnHeld, "Held"},
}

// legionPhaseColumns is the column each phase the daemon reports falls in. A phase this does not
// name (`done`, and any phase a later daemon adds) puts the issue on no card: the board shows the
// work in flight, and a phase Dispatch has never heard of is not something it can place. Which
// phases share a column is the board's own choice, made here: `retro` has a column of its own, as
// it has a word of its own in the spec's tables ("Phase words in tables"), and Merging holds the
// two phases those tables name merging, `merging` and `awaiting_merge`. `step` has a column of
// its own, since a step of a role the deployment defines is of any type its settings name, which
// neither Testing nor Reviewing would say. Its column stands after Reviewing, where a task of the
// default steps would put a step of its own, though a plan can name one at any place.
// `awaiting_children` has a column of its own: a parent in it keeps no slot and waits for none,
// and no role works it.
var legionPhaseColumns = map[string]string{
	legion.PhaseAdmitted:         legionColumnDesign,
	legion.PhasePlanning:         legionColumnPlanning,
	legion.PhaseImplementing:     legionColumnImplementing,
	legion.PhaseTesting:          legionColumnTesting,
	legion.PhaseReviewing:        legionColumnReviewing,
	legion.PhaseStep:             legionColumnStep,
	legion.PhaseAccepting:        legionColumnAccepting,
	legion.PhaseRetro:            legionColumnRetro,
	legion.PhaseMerging:          legionColumnMerging,
	legion.PhaseAwaitingMerge:    legionColumnMerging,
	legion.PhaseAwaitingChildren: legionColumnAwaitingChildren,
	legion.PhaseAwaitingPerson:   legionColumnAwaitingPerson,
	legion.PhaseHeld:             legionColumnHeld,
}

// legionNotConfiguredCode and legionNotConfiguredMessage are the one refusal a project with no
// Legion daemon gets, whichever route or live view asked (getLegionBoard, the live hub's worker).
const (
	legionNotConfiguredCode    = "LEGION_NOT_CONFIGURED"
	legionNotConfiguredMessage = "this project has no Legion daemon; set one through PUT /api/v1/projects/{key}/legion/daemon"
)

// legionBoardResponse is GET /api/v1/projects/{key}/legion/board: the project the board shows, its header, its
// columns, the settings its daemon reports, and when it was computed.
// Mirrors packages/contracts/src/dispatch-api.ts's LegionBoardResponse exactly.
type legionBoardResponse struct {
	Project string              `json:"project"`
	Header  legionBoardHeader   `json:"header"`
	Columns []legionBoardColumn `json:"columns"`
	// Settings is the settings the daemon reports, which hold no secret: the daemon's own bytes,
	// key by key, and {} until a read records them, so a project whose daemon has never answered
	// still has a board.
	Settings   map[string]json.RawMessage `json:"settings"`
	ComputedAt time.Time                  `json:"computed_at"`
}

// legionBoardHeader is what the view's header shows: the slots in use against the cap, the live
// trees, the queue's length, and the last read of the daemon — when it was made, whether it
// succeeded and why it did not. SlotCap is null until a read records the daemon's admission
// block, Trees is 0 until a daemon that reports its live trees does (dispatch://INFRA-980), and
// ReadAt is null for a project whose daemon has never answered at all, which shows the board with
// empty columns.
type legionBoardHeader struct {
	SlotsInUse  int        `json:"slots_in_use"`
	SlotCap     *int       `json:"slot_cap"`
	Trees       int        `json:"trees"`
	QueueLength int        `json:"queue_length"`
	ReadAt      *time.Time `json:"read_at"`
	ReadOK      bool       `json:"read_ok"`
	ReadError   *string    `json:"read_error"`
}

// legionBoardColumn is one column: its id, the label the view shows, and its cards in the order
// the view draws them. ID is one of the thirteen columns, in the order legionBoardColumnOrder
// draws them; the enum is what makes the generated type carry that set rather than a bare string.
type legionBoardColumn struct {
	ID    string            `json:"id" enum:"queue,design,planning,implementing,testing,reviewing,step,accepting,retro,merging,awaiting_children,awaiting_person,held"`
	Label string            `json:"label"`
	Cards []legionBoardCard `json:"cards"`
}

// legionBoardCard is one issue on the board. Key, Title and Priority come from Dispatch's own
// issues table, joined on the daemon's key (the plan's D13; the daemon's state carries none of the
// three). Phase, PhaseSince, Slot, Roles and WaitingOn are the newest observations of that issue,
// PhaseSince being when the phase it is in was first recorded — the time in the phase the card
// shows. BlockedBy names the issues a blocker link holds it on; for a parent in
// `awaiting_children` it names the open children the daemon lists it as blocked on, not blocker
// links. Children counts the issues of its tree that its tree has not started, which are on no
// card of their own. The card carries no CI verdict: the daemon's state has none since
// cybermonklabs/legion#2539, and a verdict row a read wrote before then is read by nothing.
type legionBoardCard struct {
	Key        string           `json:"key"`
	Title      string           `json:"title"`
	Priority   *int             `json:"priority"`
	Phase      string           `json:"phase"`
	PhaseSince *time.Time       `json:"phase_since"`
	Slot       *int             `json:"slot"`
	Roles      []string         `json:"roles"`
	BlockedBy  []string         `json:"blocked_by"`
	WaitingOn  *legionBoardWait `json:"waiting_on"`
	Tree       string           `json:"tree"`
	Children   int              `json:"children"`
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

// getLegionBoard answers GET /api/v1/projects/{key}/legion/board, the project its path names
// (R2, R26: everything of one project under a path). A project that names no daemon is 404
// LEGION_NOT_CONFIGURED; a project whose daemon has never answered is answered the board with
// empty columns and the read's failure in its header; a project that does not exist is 404
// PROJECT_NOT_FOUND, and a malformed key 400 INVALID_PROJECT (legionProject).
func (s *server) getLegionBoard(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	project, ok := s.legionProject(w, r)
	if !ok {
		return
	}
	population, err := s.readLegionBoard(r.Context(), project)
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
// newest read of its daemon and the project's legion_current in one snapshot (readSampled: the
// header, the cards and the issues the daemon reports), the project's own issues in Dispatch's
// rank order, and the phase work of the issues its tree walk can climb onto that the daemon
// reports nothing of.
type legionBoardPopulation struct {
	project string
	// read is the newest legion_reads row, nil for a project whose daemon was never read.
	read *legionBoardDaemonRead
	// admission is the daemon's admission block as the newest read recorded it, nil while no read
	// has recorded one: before the first read of a daemon, and for a daemon that sends none.
	admission *legion.Admission
	// settings is the daemon's own settings block as the newest read recorded it, nil while no
	// read has recorded one.
	settings map[string]json.RawMessage
	// issues is every issue the newest good read reported, by key, with what Dispatch holds of it
	// beside the observations (readCurrent): the issues the board may show a card of.
	issues map[string]*legionBoardIssue
	// reported is legion.ReadCurrent's Reported: the daemon's half of treeNode's rule.
	reported map[string]string
	// parents is the parent Dispatch records for every issue of the project, the chain the tree
	// walk climbs: an issue the daemon reports nothing of can still be the Legion issue above one
	// it does (treeRoot).
	parents map[string]string
	// phaseWork is which of the issues the tree walk can climb onto that the daemon reports
	// nothing of carry Legion phase work, the other half of what makes an issue Legion's for the
	// walk (readPhaseWork); nil where the walk climbs onto none.
	phaseWork map[string]struct{}
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
	// state is the issue's newest observations, read through the one per-issue rule an issue's
	// own read takes them through (legion.IssueState), so a card and that read agree on the
	// issue's phase, its slot and the roles in a turn.
	state legion.IssueState
	// title and priority are Dispatch's, empty for an issue Dispatch does not hold; the parent
	// Dispatch records is p.parents, which holds every issue of the project and not only the
	// ones the daemon reports, and the daemon's `tree` is p.reported's.
	title    string
	priority *int
}

// readLegionBoard reads one project's board. A project with no Legion setting is legion's
// ErrNoSettings, which the route answers 404 and the live hub refuses the subscription with.
func (s *server) readLegionBoard(ctx context.Context, project string) (*legionBoardPopulation, error) {
	pool := s.deps.Store.Pool
	if _, err := legion.GetSettings(ctx, pool, project); err != nil {
		return nil, err
	}
	population := &legionBoardPopulation{
		project: project, issues: map[string]*legionBoardIssue{}, parents: map[string]string{},
	}
	if err := population.readSampled(ctx, pool); err != nil {
		return nil, err
	}
	if err := population.readIssues(ctx, pool); err != nil {
		return nil, err
	}
	if err := population.readPhaseWork(ctx, pool); err != nil {
		return nil, err
	}
	return population, nil
}

// readSampled reads what the sampler writes of the project — the newest read of its daemon, the
// header's (readDaemonRead), and its legion_current, the cards' and the walk's (readCurrent) — in
// one repeatable-read, read-only transaction, so the two answer one moment. RecordRead commits a
// read's row and its observations together, and two reads at two moments would still let a commit
// between them put the header of one read of the daemon over the cards of another.
func (p *legionBoardPopulation) readSampled(ctx context.Context, pool *store.Pool) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	read, err := readDaemonRead(ctx, tx, p.project)
	if err != nil {
		return err
	}
	p.read = read
	if err := p.readCurrent(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// readDaemonRead is the newest read of project's daemon, the freshness the board's header and an
// issue's Legion read show, or nil for a project whose daemon was never read. It reads on the
// caller's transaction, so the read answers the moment that caller's other reads of what the
// sampler writes answer, and both callers take the read through this one statement.
func readDaemonRead(ctx context.Context, tx pgx.Tx, project string) (*legionBoardDaemonRead, error) {
	var read legionBoardDaemonRead
	err := tx.QueryRow(ctx, `
		select read_at, ok, error
		from legion_reads
		where project = $1
		order by read_at desc, id desc
		limit 1
	`, project).Scan(&read.at, &read.ok, &read.failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &read, nil
}

// readCurrent reads the project's legion_current through legion.ReadCurrent inside readSampled's
// transaction, so the cards and the tree walk answer the snapshot the header does. Its Newest are
// the cards, and the project's own rows under legion.AdmissionKey (observeProject); its Reported
// is the walk's (treeNode).
func (p *legionBoardPopulation) readCurrent(ctx context.Context, tx pgx.Tx) error {
	current, err := legion.ReadCurrent(ctx, tx, p.project)
	if err != nil {
		return err
	}
	for _, one := range current.Newest {
		if one.IssueKey == legion.AdmissionKey {
			p.observeProject(one.Field, one.Value)
			continue
		}
		issue := p.issues[one.IssueKey]
		if issue == nil {
			issue = &legionBoardIssue{key: one.IssueKey}
			p.issues[one.IssueKey] = issue
		}
		issue.state.Observe(one.Field, one.Value, one.ObservedAt)
	}
	p.reported = current.Reported
	return nil
}

// observeProject takes one of the project's own newest observations: a value of another shape
// than the field is read as leaves the field unrecorded, since the daemon owns the wire shape.
func (p *legionBoardPopulation) observeProject(field string, value []byte) {
	switch field {
	case legion.FieldAdmission:
		p.admission = legion.AdmissionOf(value)
	case legion.FieldSettings:
		_ = json.Unmarshal(value, &p.settings)
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

// readPhaseWork reads which of the issues the tree walk can climb onto carry Legion phase work:
// the half of what makes an issue Legion's that the daemon's reports do not answer (treeNode). It
// runs after the other reads, since the walk climbs what they read.
//
// An issue the daemon reports is Legion's already, and the walk asks about any other only as the
// parent of an issue whose `tree` is empty (legion.TreeChain). The daemon names a tree for every
// issue it records (the daemon's api.Issue.Tree, dispatch://INFRA-980), so a board of such a
// daemon asks about none and this reads nothing. Where the walk can ask — an issue of a daemon
// from before that release, or an operator's claim, which stands in no tree — the walk is made
// once first with every such issue taken as Legion's, which climbs onto every one the real walk
// can reach and some it cannot, and only those are read (legion.PhaseWorkOf): an index read of
// those issues' own rows, whose answer for each is the one the whole log gives it.
func (p *legionBoardPopulation) readPhaseWork(ctx context.Context, pool *store.Pool) error {
	var climbed []string
	taken := map[string]bool{}
	climb := func(key string) (legion.TreeNode, bool) {
		node, known := p.treeNode(key)
		if known && !node.Legion {
			if !taken[key] {
				taken[key] = true
				climbed = append(climbed, key)
			}
			node.Legion = true
		}
		return node, known
	}
	// Every walk the board makes starts at an issue the daemon reports (card, nearestCard).
	for key := range p.issues {
		legion.TreeChain(key, climb)
	}
	if len(climbed) == 0 {
		return nil
	}
	work, err := legion.PhaseWorkOf(ctx, pool, climbed)
	if err != nil {
		return err
	}
	p.phaseWork = work
	return nil
}

// issueKeys is every issue the board's answer names, which is what the live hub marks its view
// dirty by (delivery_live_hub_intake.go's dirtyIssueLocked).
func (p *legionBoardPopulation) issueKeys() map[string]struct{} {
	keys := make(map[string]struct{}, len(p.issues))
	for key := range p.issues {
		keys[key] = struct{}{}
	}
	return keys
}

// treeRoot is the issue's tree root, by the one rule the metrics place an issue in a tree with
// (legion.TreeRoot), over every issue of the project (parents) and every issue the daemon reports
// (reported), each Legion's or not by legion.TreeNode's rule (treeNode). An issue of another
// project, which only a daemon that reports one brings here, is a root of its own.
func (p *legionBoardPopulation) treeRoot(key string) string {
	return legion.TreeRoot(key, p.treeNode)
}

// treeNode is what that walk reads of one issue: the daemon's `tree`, Dispatch's parent, and
// whether the issue is Legion's (legion.IsLegionIssue).
func (p *legionBoardPopulation) treeNode(key string) (legion.TreeNode, bool) {
	tree, reported := p.reported[key]
	parent, held := p.parents[key]
	if !reported && !held {
		return legion.TreeNode{}, false
	}
	return legion.TreeNode{Tree: tree, Parent: parent, Legion: legion.IsLegionIssue(key, p.reported, p.phaseWork)}, true
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
	_, named := legionPhaseColumns[issue.state.Phase]
	held := false
	switch {
	case c.queued[key]:
		held = true
	case !named:
		held = false
	case issue.state.Phase == legion.PhaseAdmitted && issue.state.Slot == nil:
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

// legionBoardCompute is GET /api/v1/projects/{key}/legion/board's body for a population read for it. It reads
// nothing: the live hub answers the same body from the population it already holds, and now is
// only computed_at.
//
// A card's column is the first of these that holds: the daemon's waiting line, in its own order,
// which the view does not re-sort (the spec's "Rule details": that order puts answered roots
// first, then follows Dispatch's rank); the issues a blocker link holds, after them in the Queue
// column, but for a parent in `awaiting_children`; and otherwise the column its phase falls in.
// The daemon lists such a parent as blocked on its open children while one is open
// (dispatch://INFRA-3320, R11), but it waits on them and not on a slot: its card is in Waiting on
// children for the whole wait, naming its open children while the daemon lists them, and stays
// there after its last child closes until the pass that returns it. An issue the daemon reports
// in `admitted` that holds no slot and that neither list names is a child its tree has not
// started: it is on no card, and the nearest card above it in its tree counts it. Where no issue
// above it carries a card — its tree climbs through issues the daemon reports nothing of, or
// through issues whose phase is on no column — it is a card of its own, since a count on an issue
// the board shows nothing of is a count no reader can see. Where no read has recorded an
// admission block the waiting line is Dispatch's rank order over the issues in that position,
// which is the order the daemon's own line follows once a root's wait is answered.
func legionBoardCompute(p *legionBoardPopulation, now time.Time) legionBoardResponse {
	cards := map[string][]legionBoardCard{}
	placed := map[string]bool{}
	// blockers is every issue a blocker link holds, with the issues holding it, by the admission
	// block's one rule (legion.Admission.Blockers), read once for the whole answer: the Queue
	// column carries each issue it names, and each card its own list (card).
	blockers := p.admission.Blockers()
	var waiting []string
	if p.admission != nil {
		waiting = p.admission.Waiting
	}

	// The queue: the daemon's waiting line in its own order, then the issues a blocker link holds
	// that the line does not already name, but for a parent waiting on its children.
	queued := make([]string, 0, len(waiting)+len(blockers))
	seen := map[string]bool{}
	for _, key := range waiting {
		if p.issues[key] == nil || seen[key] {
			continue
		}
		seen[key] = true
		queued = append(queued, key)
	}
	blocked := make([]string, 0, len(blockers))
	for key := range blockers {
		if issue := p.issues[key]; issue != nil && !seen[key] && issue.state.Phase != legion.PhaseAwaitingChildren {
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
		// No read has recorded a waiting line: the issues waiting for a slot that no card above
		// them holds, in Dispatch's rank order.
		for _, key := range p.ranked {
			issue := p.issues[key]
			if issue != nil && issue.state.Phase == legion.PhaseAdmitted && issue.state.Slot == nil && board.has(key) {
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
		column, named := legionPhaseColumns[issue.state.Phase]
		if !named {
			continue
		}
		if issue.state.Phase == legion.PhaseAdmitted && issue.state.Slot == nil {
			if above := board.nearestCard(key); above != "" {
				uncarded[above]++
				continue
			}
		}
		placed[key] = true
		cards[column] = append(cards[column], p.card(issue, blockers[key], 0))
	}
	for _, key := range queued {
		cards[legionColumnQueue] = append(cards[legionColumnQueue], p.card(p.issues[key], blockers[key], uncarded[key]))
	}
	// A root's count is known only once every issue has been placed, so the cards already made
	// take theirs now.
	for column, made := range cards {
		if column == legionColumnQueue {
			continue
		}
		for index := range made {
			made[index].Children = uncarded[made[index].Key]
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

	header := legionBoardHeader{QueueLength: len(cards[legionColumnQueue])}
	// Slots in use are the daemon's own count where it reports one. dispatch://INFRA-980 makes
	// admission.active list every issue that holds a slot, children included, so it is the
	// authority; before that release the daemon lists roots there and only a root carries a slot,
	// so the two agree. A read that recorded no admission block falls back to the issues whose own
	// slot the sampler saw.
	if p.admission != nil && len(p.admission.Active) > 0 {
		header.SlotsInUse = len(p.admission.Active)
	} else {
		for _, issue := range p.issues {
			if issue.state.Slot != nil {
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
	// The block is an object, empty rather than null, so a page reads one shape whether or not a
	// read has recorded it.
	settings := p.settings
	if settings == nil {
		settings = map[string]json.RawMessage{}
	}
	return legionBoardResponse{
		Project: p.project, Header: header, Columns: columns,
		Settings: settings, ComputedAt: now,
	}
}

// card is one issue's card, with the issues holding it, as the admission block names them for it
// (legion.Admission.Blockers, by whose rule an issue's own read names them too), and the children
// of its tree that are on no card of their own.
func (p *legionBoardPopulation) card(issue *legionBoardIssue, blockedBy []string, children int) legionBoardCard {
	card := legionBoardCard{
		Key: issue.key, Title: issue.title, Priority: issue.priority, Phase: issue.state.Phase,
		Slot: issue.state.Slot, Roles: issue.state.Working(), BlockedBy: blockedBy,
		WaitingOn: legionBoardWaitOf(issue.state.WaitingOn), Tree: p.treeRoot(issue.key), Children: children,
	}
	if card.BlockedBy == nil {
		card.BlockedBy = []string{}
	}
	if !issue.state.PhaseSince.IsZero() {
		since := issue.state.PhaseSince.UTC()
		card.PhaseSince = &since
	}
	return card
}

// legionBoardWaitOf is the card's reading of the wait an issue's state holds, the daemon's object
// as stored: its kind, when it began and when a person answered it, and nil where the wait names
// no kind or one of the three is not the shape it is read as. An issue's own read carries the
// whole object; a card shows these three.
func legionBoardWaitOf(raw json.RawMessage) *legionBoardWait {
	if raw == nil {
		return nil
	}
	var wait struct {
		Kind       string     `json:"kind"`
		Since      *time.Time `json:"since"`
		AnsweredAt *time.Time `json:"answeredAt"`
	}
	if err := json.Unmarshal(raw, &wait); err != nil || wait.Kind == "" {
		return nil
	}
	return &legionBoardWait{Kind: wait.Kind, Since: wait.Since, AnsweredAt: wait.AnsweredAt}
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

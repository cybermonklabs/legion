// legion_board.go serves GET /api/v1/projects/{key}/legion/board, one project's Legion board from
// what the sampler recorded (dispatch://INFRA-1077), and the body the `legion` live view kind
// sends (live_frames.go's legionBoardView). The project's Issues tab of the web app draws it: its
// Legion columns, its Board by Legion phase and its (i) hint read the route and the live view.
//
// The answer is read from what Dispatch holds, never from the daemon: the sampler reads each
// daemon once a minute under a lease (internal/dispatch/legion) and records what moved, and this
// reads the newest value of each field it recorded, the newest read's own verdict, and the
// project's issues. A board that called the daemon would read it once per open page and once per
// event burst, which is what the per-minute lease exists to prevent.
//
// The per-issue facts the answer carries are read and never computed. issue_facts holds one row
// per issue Legion worked, written by the facts consumer (internal/dispatch/legionfacts), and the
// board serves as they stand the rows of every open issue and of every issue it shows: the issue
// of each card and every issue of a tree a card stands in. A board that computed them would read
// the whole metrics population once per open page, which is what the stored rows exist to
// prevent, and a board that served every row of the project would grow with every issue its
// Legion work ever covered; a closed issue the board does not show is the eval items route's.
//
// Two disciplines, as in the metrics package:
//
//   - The daemon's state is read loosely. Every fact here is a field legion/state.go observes, and
//     a field a daemon does not send is a field the board has no observation of. Nothing takes a
//     slot holder for a root: a card's tree is the daemon's own `tree` where a read carried one
//     (dispatch://INFRA-980 adds it) and the parent chain otherwise.
//   - The columns are the spec's, with Waiting on children for the phase dispatch://INFRA-3320
//     added, in one ordered list (legionBoardColumnOrder), and a phase maps to exactly one of them
//     (legion.PhaseColumns), so the view and metric 2's "no role" bar agree on where
//     `awaiting_children`, `awaiting_person` and `held` go (the plan's D11). The ids and that
//     table are the legion package's, since the per-issue facts read them with no card to hand;
//     the labels here are the view's.
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
	{legion.ColumnRetro, "Retro"},
	{legion.ColumnMerging, "Merging"},
	{legion.ColumnAwaitingChildren, "Waiting on children"},
	{legion.ColumnAwaiting, "Waiting on a person"},
	{legion.ColumnHeld, "Held"},
}

// legionNotConfiguredCode and legionNotConfiguredMessage are the one refusal a project with no
// Legion daemon gets, whichever route or live view asked (getLegionBoard, the live hub's worker).
const (
	legionNotConfiguredCode    = "LEGION_NOT_CONFIGURED"
	legionNotConfiguredMessage = "this project has no Legion daemon; set one through PUT /api/v1/projects/{key}/legion/daemon"
)

// legionBoardResponse is GET /api/v1/projects/{key}/legion/board: the project the board shows,
// its header, its columns, the stored facts of its issues, the settings its daemon reports, and
// when it was computed.
// Mirrors packages/contracts/src/dispatch-api.ts's LegionBoardResponse exactly.
type legionBoardResponse struct {
	Project string              `json:"project"`
	Header  legionBoardHeader   `json:"header"`
	Columns []legionBoardColumn `json:"columns"`
	// Issues is the stored facts of every open issue with Legion phase work and of every issue
	// the board shows, by issue key: the issue of every card, and every issue of a tree a card
	// stands in (whose stored root is that card's tree), open or closed, whatever its hours are,
	// the issues of such a tree that are on no card included, and an open issue on no card in no
	// shown tree, a parked one, as well, since no other route serves an open issue's facts. A
	// closed issue the board does not show carries no entry: the eval items route serves its facts
	// by its close day (R22), so every issue has one source. An issue whose row is not written
	// yet carries none either.
	Issues map[string]legionBoardFacts `json:"issues"`
	// Settings is the settings the daemon reports, which hold no secret: the daemon's own bytes,
	// key by key, and {} until a read records them, so a project whose daemon has never answered
	// still has a board.
	Settings   map[string]json.RawMessage `json:"settings"`
	ComputedAt time.Time                  `json:"computed_at"`
}

// legionBoardFacts is one issue's stored Legion facts on the wire: the issue_facts row
// legionfacts.Row holds, less the project the board names and the issue the map is keyed by,
// closed at the moment of the read (legionfacts.Row.At). Every value is the one the facts
// consumer computed and stored, and the one thing the board adds to any of them is the growth
// of the facts that still grow, from the newest input they read to the moment of the read, which
// is what the computation itself answers at that moment: an open issue's, and the slot hours of a
// closed issue for a slot it never gave up.
//
//   - OpenedAt is when the issue was opened.
//   - ClosedAt is when it closed, null while it is open.
//   - HoursByPhase is the hours it spent in each board column, by column id, a column with no
//     hours left out.
//   - WorkHours is its work time to merge, less the time it stood parked, queued and out of the
//     workflow before its close.
//   - ReviewRounds is how many times a reviewer sent its work back.
//   - RootKey is the root of its tree.
//   - TreeDepth is how far below that root it stands, 0 for the root itself.
//   - SlotHours is how long it held an admission slot of its own.
//   - WaitingReason is why it waits, "" where it waits for nothing.
//   - WorkFirstAt and WorkLastAt are the first and the last moment of its measured work, null
//     until it has some; the last is the moment of the read while a bar of it is still open.
//   - UpdatedAt is when its row was written, which the header's facts_updated_at is the oldest
//     of.
//   - AsOf is the moment the values are of, for an entry whose values grow: the moment of the
//     read, which the read grew them to (legionfacts.Row.At). It is null for an entry whose values
//     stand still, which are its values at any clock.
//   - Growth is how fast each value that grows after AsOf grows (legionBoardGrowth), so a holder
//     of the entry gives each at its own clock as the read does: the value plus its rate times the
//     hours from AsOf to that clock. It is null where AsOf is: a closed issue's entry, but for a
//     slot it never gave up, and an open issue's whose every value stands still.
//
// Both are null where nothing grows, as the board's other optional fields are (ClosedAt), because
// most entries are closed issues' and each closed row's AsOf is a moment of its own, which
// compression does not fold: carried on every entry, the two took the body of a project of
// INFRA's size past the 300 KB compressed a first payload may reach
// (dispatch://INFRA-979/spec#b-r14).
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
	AsOf          *time.Time         `json:"as_of"`
	Growth        *legionBoardGrowth `json:"growth"`
}

// legionBoardGrowth is how fast each value of an entry grows after its as_of, by the entry's own
// field names: hours an hour for the work time, the slot hours and the hours of each column (a
// column whose hours stand still left out), and for the last moment of measured work how fast it
// moves with the clock, 1 while a bar is still open and 0 where it stands. It is
// metrics.GrowthRates with JSON names, converted from it, and an entry carries one only where a
// rate is not 0 (legionBoardGrowthOf).
type legionBoardGrowth struct {
	HoursByPhase map[string]float64 `json:"hours_by_phase"`
	WorkHours    float64            `json:"work_hours"`
	SlotHours    float64            `json:"slot_hours"`
	WorkLastAt   float64            `json:"work_last_at"`
}

// legionBoardGrowthOf is a read row's as_of and growth on the wire: where a value the entry
// carries grows (metrics.GrowthRates.Grows), the moment the read closed the row at
// (legionfacts.Row.At) and the rates the row stores of the entry's values, which metrics.Growth's
// Rates names; and null for both where none grows.
func legionBoardGrowthOf(row legionfacts.Row) (*time.Time, *legionBoardGrowth) {
	rates := row.Growth.Rates()
	if !rates.Grows() {
		return nil, nil
	}
	growth := legionBoardGrowth(rates)
	if growth.HoursByPhase == nil {
		// {} rather than null, as the entry's own hours by column, so a page reads one shape.
		growth.HoursByPhase = map[string]float64{}
	}
	return legionFactTime(&row.AsOf), &growth
}

// legionBoardHeader is what the view's header shows: the slots in use against the cap, the live
// trees, the queue's length, the last read of the daemon — when it was made, whether it
// succeeded and why it did not — and how fresh the stored facts are. SlotCap is null until a
// read records the daemon's admission block, Trees is 0 until a daemon that reports its live
// trees does (dispatch://INFRA-980), and ReadAt is null for a project whose daemon has never
// answered at all, which shows the board with empty columns.
//
// The two facts fields are read apart, the first of this project's own rows and the second of the
// deployment's one consumer, so a reader can tell a project with no Legion work (rows: none,
// cursor: an age) from a deployment whose consumer has never run (rows: none, cursor: null).
type legionBoardHeader struct {
	SlotsInUse  int        `json:"slots_in_use"`
	SlotCap     *int       `json:"slot_cap"`
	Trees       int        `json:"trees"`
	QueueLength int        `json:"queue_length"`
	ReadAt      *time.Time `json:"read_at"`
	ReadOK      bool       `json:"read_ok"`
	ReadError   *string    `json:"read_error"`
	// FactsUpdatedAt is the oldest updated_at of the stored rows this answer served, which are the
	// rows of the issues the map carries (Issues: the open issues and the issues the board shows)
	// and not every row the project holds, null where it served none.
	FactsUpdatedAt *time.Time `json:"facts_updated_at"`
	// FactsCursorAgeSeconds is the facts consumer's age (legionfacts.CursorAge), measured when the
	// board was read: the time since its last pass ran, or since a pass last read the delivery
	// change log to its end, whichever is older, so a pass catching up a backlog of changes reads
	// as old as its last read of the whole log; null where the consumer holds no cursor row at
	// all. It is of the deployment's one consumer and not of this project, so it says how fresh
	// the facts are and never whether this project has any.
	FactsCursorAgeSeconds *int `json:"facts_cursor_age_seconds"`
}

// legionBoardColumn is one column: its id, the label the view shows, and its cards in the order
// the view draws them. ID is one of the twelve columns, in the order legionBoardColumnOrder draws
// them; the enum is what makes the generated type carry that set rather than a bare string.
type legionBoardColumn struct {
	ID    string            `json:"id" enum:"queue,design,planning,implementing,testing,reviewing,accepting,retro,merging,awaiting_children,awaiting_person,held"`
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
	// SlotSince is when Dispatch first recorded the slot the issue holds of its own, which is the
	// admission only where the sampler was reading the daemon then (observe); null with no slot of
	// its own.
	SlotSince *time.Time `json:"slot_since"`
	// WaitingPlace is the issue's place in the daemon's waiting line, from 1; null for an issue
	// the line does not name.
	WaitingPlace *int             `json:"waiting_place"`
	Roles        []string         `json:"roles"`
	BlockedBy    []string         `json:"blocked_by"`
	WaitingOn    *legionBoardWait `json:"waiting_on"`
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
	now := time.Now().UTC()
	population, err := s.readLegionBoard(r.Context(), project, now)
	if err != nil {
		if errors.Is(err, legion.ErrNoSettings) {
			writeError(w, legionNotConfiguredCode, http.StatusNotFound, legionNotConfiguredMessage)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, legionBoardCompute(population, now))
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
	// cards is every card the board draws, by column id, placed once the daemon's state and the
	// project's issues are read (placeCards): what the stored facts the answer carries follow.
	cards map[string][]legionBoardCard
	// facts is the stored Legion facts the answer carries, by key, as legionfacts.Read answered
	// them at the moment of the read: the rows of the open issues and of the issues the board
	// shows, and no computation of this server's own.
	facts map[string]legionfacts.Row
	// cursorAge is the facts consumer's age (legionfacts.CursorAge, FactsCursorAgeSeconds), in
	// seconds, measured when this population was read; nil where the consumer holds no cursor row.
	cursorAge *int
	// ranked is the project's issue keys in Dispatch's rank order, read in that order, which is
	// the order sortByRank puts any set of this project's keys in.
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
	// slotSince is when the newest slot observation was recorded: the first read of the daemon
	// that carried that slot, which need not be the admission (observe).
	slotSince time.Time
	// title and priority are Dispatch's, empty for an issue Dispatch does not hold; the parent
	// Dispatch records is p.parents, which holds every issue of the project and not only the
	// ones the daemon reports, and the daemon's `tree` is p.reported's.
	title    string
	priority *int
}

// readLegionBoard reads one project's board as of now. A project with no Legion setting is
// legion's ErrNoSettings, which the route answers 404 and the live hub refuses the subscription
// with.
//
// The stored facts are those of every open issue and of every issue the board shows, and of no
// other (R22): the issue of each card and every issue of a tree a card stands in. They are read
// once the cards are placed, in three index reads that share no row: the project's open rows
// through issue_facts_closed, the closed rows of the trees by root through issue_facts_root, and
// the closed rows of the cards outside those trees by key through the primary key, never as the
// project's rows filtered after the read (legionfacts.Read, whose plan its test records), so the
// read and the body follow the open work and what the daemon runs rather than the project's
// history. Nothing here computes a fact: no metrics function is called and no population is held
// between calls. The one computation of the read is legionfacts.Row.At, which closes each part of
// an open issue still running at now, the moment the body is answered as of (legionBoardCompute).
func (s *server) readLegionBoard(ctx context.Context, project string, now time.Time) (*legionBoardPopulation, error) {
	pool := s.deps.Store.Pool
	if _, err := legion.GetSettings(ctx, pool, project); err != nil {
		return nil, err
	}
	population := &legionBoardPopulation{
		project: project, issues: map[string]*legionBoardIssue{}, parents: map[string]string{},
	}
	// The daemon's read with its legion_current in one snapshot (readSampled: the header, the
	// cards and the issues the daemon reports), the project's issues, the phase work the tree walk
	// climbs onto and the stored facts are one chain: the walk climbs the issues the daemon
	// reports and the project's issues name, the cards stand in the trees that walk places them
	// in, and the cards name the trees whose facts the answer carries beside every open issue's.
	// The age of the consumer's oldest cursor, which says how recently it read its feeds, depends
	// on nothing before it, so the two run side by side (store.ReadSideBySide), as the delivery
	// population's independent reads do (delivery.go). The snapshot's transaction is the chain's
	// own, and it commits before the chain reads anything else; the facts read at the chain's end
	// opens a read-only transaction of its own (legionfacts.Read), once every earlier read of the
	// chain has let its connection go.
	if err := store.ReadSideBySide(ctx,
		func(ctx context.Context) (err error) {
			if err := population.readSampled(ctx, pool); err != nil {
				return err
			}
			if err := population.readIssues(ctx, pool); err != nil {
				return err
			}
			if err := population.readPhaseWork(ctx, pool); err != nil {
				return err
			}
			population.cards = population.placeCards()
			keys, roots := population.shown()
			population.facts, err = legionfacts.Read(ctx, pool, project, keys, roots, now)
			return err
		},
		func(ctx context.Context) error {
			age, held, err := legionfacts.CursorAge(ctx, pool, now)
			if err != nil || !held {
				return err
			}
			seconds := int(age.Seconds())
			population.cursorAge = &seconds
			return nil
		},
	); err != nil {
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
		issue.observe(one.Field, one.Value, one.ObservedAt)
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

// observe takes one newest observation into the issue: its state, by the one per-issue rule
// (legion.IssueState.Observe), and for its slot also when that slot was recorded. A row is written
// only where the value moved, so that is the first read of the daemon that carried the slot: the
// admission when the sampler was already reading the daemon then, and otherwise a later read, such
// as the first one after the project's daemon is configured. A card shows that moment only beside
// a slot the issue holds (card), so a slot value of another shape, which the state reads as no
// slot, shows none.
func (issue *legionBoardIssue) observe(field string, value []byte, at time.Time) {
	issue.state.Observe(field, value, at)
	if field == legion.FieldSlot {
		issue.slotSince = at
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
// runs after the daemon's observations and the project's issues are read, since the walk climbs
// what they read, and before the cards are placed, since placing them walks the trees.
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
	// Every walk the board makes starts at an issue the daemon reports (placeCards' waiting line,
	// and card).
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

// issueKeys is every issue the daemon reported, the issues the board may draw a card of, which is
// what the live hub marks its view dirty by on an issue's event (delivery_live_hub_intake.go's
// dirtyIssueLocked). The issues the answer carries only a stored row of are not among them: an
// event on such an issue moves the answer through the stored rows the facts consumer writes
// after it, its own and those of the issues whose tree it moved, and the change of each marks the
// board of its project at once (touchedBy's issue_facts arm), so a mark of the event itself would
// recompute a board whose answer had not moved yet.
func (p *legionBoardPopulation) issueKeys() map[string]struct{} {
	keys := make(map[string]struct{}, len(p.issues))
	for key := range p.issues {
		keys[key] = struct{}{}
	}
	return keys
}

// treeRoot is the issue's tree root, by the one walk that places an issue in a tree
// (legion.TreeRoot), which the metrics walk too. Where that walk stops, and why, is stated once
// in legion.TreeChain's doc.
//
// The walk knows every issue of the project (parents) and every issue the daemon reports
// (reported), each Legion's or not by legion.TreeNode's rule (treeNode), so a chain climbs
// through an issue that left the daemon's state, which carries no card. An issue of another
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

// lineIssue is what the waiting line reads of one issue the daemon reports (legion.LineIssue),
// and false for an issue the daemon reports nothing of, which is an issue the board draws no
// card for.
func (p *legionBoardPopulation) lineIssue(key string) (legion.LineIssue, bool) {
	issue := p.issues[key]
	if issue == nil {
		return legion.LineIssue{}, false
	}
	return legion.LineIssue{Phase: issue.state.Phase, Slot: issue.state.Slot}, true
}

// placeCards places every issue the daemon reports on the board, by column id: the cards the
// answer draws (legionBoardCompute), whose issues and trees are the ones whose stored facts it
// carries (shown). It reads nothing and no clock, so the read places the cards once, before it
// reads those facts.
//
// A card's column is the first of these that holds: the daemon's waiting line, in its own order,
// which the view does not re-sort (the spec's "Rule details": that order puts answered roots
// first, then follows Dispatch's rank); the issues a blocker link holds, after them in the Queue
// column — of both lists only the issues that have not started work, since an issue that has
// started keeps the column of its phase and carries its place in the line on its card, and never
// a parent in `awaiting_children`; and otherwise the column its phase falls in. The daemon lists
// such a parent as blocked on its open children while one is open (dispatch://INFRA-3320, R11),
// but it waits on them and not on a slot: its card is in Waiting on children for the whole wait,
// naming its open children while the daemon lists them, and stays there after its last child
// closes until the pass that returns it. An issue the daemon reports in `admitted` that holds no
// slot and that neither list names is a child its tree has not started: it is on no card, and
// the nearest card above it in its tree counts it. Where no issue above it carries a card — its
// tree climbs through issues the daemon reports nothing of, or through issues whose phase is on
// no column — it is a card of its own, since a count on an issue the board shows nothing of is a
// count no reader can see. Where no read has recorded an admission block the waiting line is the
// same rule read for a reader that knows no line (legion.QueuedUnread), over Dispatch's rank
// order, which is the order the daemon's own line follows once a root's wait is answered.
//
// Every one of those answers is the waiting line's (legion.WaitingLine), which the stored
// per-issue facts read as well: the card and the stored row of one issue cannot disagree about
// the column it stands in or about why it waits.
func (p *legionBoardPopulation) placeCards() map[string][]legionBoardCard {
	cards := map[string][]legionBoardCard{}
	placed := map[string]bool{}
	line := legion.NewWaitingLine(legion.WaitingLineInput{
		Admission: p.admission, Issue: p.lineIssue, Tree: p.treeNode,
	})
	// The board's keys in the order it draws them, which is Dispatch's rank order, and the Queue
	// column's cards: the line's own order first, then the rest of the issues the column rule
	// queues, by rank.
	keys := p.sortedKeys()
	queued := line.Queue(keys)
	for _, key := range queued {
		placed[key] = true
	}

	// Every other issue the daemon reports, by its phase. An `admitted` issue holding no slot that
	// the queue does not name has not been started: it is counted on the nearest card above it,
	// and carries one of its own where there is none.
	uncarded := map[string]int{}
	for _, key := range keys {
		if placed[key] {
			continue
		}
		column := line.Column(key)
		if column == "" {
			if above := line.CountedOn(key); above != "" {
				uncarded[above]++
			}
			continue
		}
		cards[column] = append(cards[column], p.card(p.issues[key], line.BlockedBy(key), 0))
	}
	for _, key := range queued {
		cards[legion.ColumnQueue] = append(cards[legion.ColumnQueue], p.card(p.issues[key], line.BlockedBy(key), uncarded[key]))
	}
	for _, made := range cards {
		for index := range made {
			made[index].WaitingPlace = line.Place(made[index].Key)
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
	return cards
}

// shown is the issues the board shows, whose stored facts its answer carries beside every open
// issue's (R22): the issue of every card, and the root of the tree each card stands in, every
// issue of which the board shows as well, its children on no card and those the daemon names
// nowhere among them, so legionfacts.Read reads a tree's rows by its root. Each is named once, in
// the order the board draws its columns.
func (p *legionBoardPopulation) shown() (keys, roots []string) {
	cards := 0
	for _, column := range legionBoardColumnOrder {
		cards += len(p.cards[column.id])
	}
	keys, roots = make([]string, 0, cards), make([]string, 0, cards)
	tree := make(map[string]bool, cards)
	for _, column := range legionBoardColumnOrder {
		for _, card := range p.cards[column.id] {
			keys = append(keys, card.Key)
			if !tree[card.Tree] {
				tree[card.Tree] = true
				roots = append(roots, card.Tree)
			}
		}
	}
	return keys, roots
}

// legionBoardCompute is GET /api/v1/projects/{key}/legion/board's body for a population read for
// it. It reads nothing: the live hub answers the same body from the population it already holds,
// and now is only computed_at. The columns are the cards the read placed (placeCards), and the
// issues map is the stored rows the read answered, those of the open issues and of the issues
// the board shows, turned into the wire shape and nothing more.
func legionBoardCompute(p *legionBoardPopulation, now time.Time) legionBoardResponse {
	cards := p.cards
	columns := make([]legionBoardColumn, 0, len(legionBoardColumnOrder))
	for _, column := range legionBoardColumnOrder {
		made := cards[column.id]
		if made == nil {
			made = []legionBoardCard{}
		}
		columns = append(columns, legionBoardColumn{ID: column.id, Label: column.label, Cards: made})
	}

	header := legionBoardHeader{QueueLength: len(cards[legion.ColumnQueue])}
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
	// The stored facts as they were read, and how fresh they are: the oldest updated_at of the
	// rows this answer serves, which are the rows of the open issues and of the issues the board
	// shows (its cards' issues and every issue of their trees) and not every row of the project,
	// so a closed row of a tree the daemon no longer shows ages it no more; null where it serves
	// none; and the age the read measured of the consumer's oldest cursor.
	issues := make(map[string]legionBoardFacts, len(p.facts))
	for key, row := range p.facts {
		hours := row.PhaseHours
		if hours == nil {
			// A row whose work took no measured time carries {} rather than null, as the
			// settings block does, so a page reads one shape.
			hours = map[string]float64{}
		}
		asOf, growth := legionBoardGrowthOf(row)
		issues[key] = legionBoardFacts{
			OpenedAt: row.OpenedAt.UTC(), ClosedAt: legionFactTime(row.ClosedAt), HoursByPhase: hours,
			WorkHours: row.WorkHours, ReviewRounds: row.ReviewRounds, RootKey: row.Tree,
			TreeDepth: row.TreeDepth, SlotHours: row.SlotHours, WaitingReason: row.WaitingReason,
			WorkFirstAt: legionFactTime(row.WorkFirstAt), WorkLastAt: legionFactTime(row.WorkLastAt),
			UpdatedAt: row.UpdatedAt.UTC(), AsOf: asOf, Growth: growth,
		}
		if header.FactsUpdatedAt == nil || row.UpdatedAt.Before(*header.FactsUpdatedAt) {
			at := row.UpdatedAt.UTC()
			header.FactsUpdatedAt = &at
		}
	}
	header.FactsCursorAgeSeconds = p.cursorAge
	// The block is an object, empty rather than null, so a page reads one shape whether or not a
	// read has recorded it.
	settings := p.settings
	if settings == nil {
		settings = map[string]json.RawMessage{}
	}
	return legionBoardResponse{
		Project: p.project, Header: header, Columns: columns, Issues: issues,
		Settings: settings, ComputedAt: now,
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

// card is one issue's card, with the issues holding it, as the admission block names them for it
// (the waiting line's BlockedBy, which takes them by legion.Admission.Blockers' rule, the one an
// issue's own read names them by), and the children of its tree that are on no card of their own.
func (p *legionBoardPopulation) card(issue *legionBoardIssue, blockedBy []string, children int) legionBoardCard {
	card := legionBoardCard{
		Key: issue.key, Title: issue.title, Priority: issue.priority, Phase: issue.state.Phase,
		Slot: issue.state.Slot, Roles: issue.state.Working(), BlockedBy: blockedBy,
		Tree: p.treeRoot(issue.key), Children: children,
	}
	// The wait the sampler recorded is the daemon's object as the issue's state holds it, which an
	// issue's own read carries whole. It is decoded once, by the one decode of the field
	// (legion.ParseWait), into the rule's own shape, and nil where it names no kind or is not the
	// shape it is read as; the body carries the three fields a card shows in the shape the view
	// reads, which is this one mapping. The card keeps the decoded value as well, which is what
	// the waiting rule reads (cardWaiting).
	if wait, read := legion.ParseWait(issue.state.WaitingOn); read {
		card.wait = wait
		card.WaitingOn = &legionBoardWait{Kind: wait.Kind, Since: wait.Since, AnsweredAt: wait.AnsweredAt}
	}
	if card.Slot == nil && card.Tree != issue.key {
		if root := p.issues[card.Tree]; root != nil && root.state.Slot != nil {
			slot := *root.state.Slot
			card.TreeSlot = &slot
		}
	}
	if card.BlockedBy == nil {
		card.BlockedBy = []string{}
	}
	if !issue.state.PhaseSince.IsZero() {
		since := issue.state.PhaseSince.UTC()
		card.PhaseSince = &since
	}
	if issue.state.Slot != nil && !issue.slotSince.IsZero() {
		since := issue.slotSince.UTC()
		card.SlotSince = &since
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

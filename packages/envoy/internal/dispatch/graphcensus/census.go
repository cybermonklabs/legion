package graphcensus

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// reportedDifferences is how many differences of one direction of one kind the report prints by
// name. The count beside them is every one there is, so a store that drifted by a million rows
// prints a million in its count and twenty rows to start from.
const reportedDifferences = 20

// projection is one kind of the store held against what projects it.
//
// rows is a select of (row_label, node_kind, node_address, digest): one row per projection row,
// naming the node that row says it projects and digesting the fields the row and its node
// share. nodes is a select of (id, digest): one row per node of the kind, digesting the same
// fields as the store holds them, of which the census compares those the store holds present
// (presentNodes). The census holds the two against each other in both directions, so a row with
// no node and a node with no row are each a difference.
//
// The ties of decisions, readings and asks are held triple by triple instead (tuples), so a
// relation that reaches the wrong node or the wrong far end reads as a row and a tuple, each
// named, and every other row of that node reads nothing.
//
// A kind with no projection table takes the set of actors the projected tables name as its rows
// (people and agents below), which is the side that exists for it; a role takes the roles the
// event log ever named as well (addressedRoles), and a session the sessions the event log names
// as followers and as reached (recordedSessions).
type projection struct {
	// name is the kind as the report names it, nodeKind the kind of graph_nodes row it
	// projects and table the relation its rows come from, for the report.
	name     string
	nodeKind string
	table    string
	// note says what this kind's two sides are where that is not the plain one row per node,
	// so the report says what it checked rather than only what it found.
	note string
	rows string
	// nodes is the kind's nodes at any state. It names no state: presentNodes wraps it.
	nodes string
	// tuples, where it is set, holds the kind triple by triple, over the triple of holding node,
	// tuple kind and far end, and nodes is unused. It is a
	// select of (from_kind, from_id, kind, to_kind, to_id): one row per relation of the kind a
	// present node states now (heldTuples). rows then answers (row_label, node_kind, node_address,
	// kind, to_kind, to_id): one row per relation a projection row states, of the node it names.
	// A row no tuple states is a difference named at the row, and a tuple no row projects one
	// named at the tuple, so a tuple moved to the wrong far end reads as the row it left and the
	// tuple it became, and the other rows of that node read nothing.
	tuples string
	// names is a select of one text column, the kind's row side, which the census reads once for
	// the run (readNames) and which rows and pastBound read as $1, rather than reading it again in
	// every statement that reads the rows: a row side that reads the event log scans it, and no
	// index narrows that scan. Two kinds with one names read it once between them, as a role and
	// its page do. It is empty for a kind whose rows read their own table.
	names string
	// authored says names reads as $1, $2 and $3 the kinds, ids and times of storedAuthors
	// (censusAuthors), which the census reads once for the run (readStoredAuthors), as it reads
	// each kind's names once: its arms read the store's log, the largest table, and the person and
	// session kinds would otherwise each read them in their own names.
	authored bool
	// pastBound counts the addresses this kind's row side names that are past the store's bound
	// (graphstore.MaxAddressBytes), which the writers left unprojected on purpose: it is a full
	// `select count(*)`, reads $1 where names is set, and is empty for a kind no address of which
	// can pass the bound. The census reports the count as a class of its own and excludes those
	// addresses from both comparisons, so a row no store entry answers by design is no difference.
	pastBound string
	// unmarked counts the rows of the kind's table that name no node and are no difference: an
	// asks or ask_followers row a process of the release before wrote, which graph_edges' old
	// arms answer until the decisions step's backfill or catch-up marks it. It is a full
	// `select count(*)` of the rows of the kind's own population - an ask's
	// anchor counts the unmarked asks that name a document to anchor to, and its answer those
	// that hold one - so a kind whose every row is unmarked is no empty kind (KindReport.Empty).
	// The row side reads marked rows alone, and the census reports the count as a class of its
	// own, which the handoff's list for the root names as the test of the three old arms. Empty
	// for a kind whose row names no node only by a defect.
	unmarked string
	// unstated counts the rows of the kind's table that belong to a document of an issue that has
	// not converted - no issue_trees row holds its issue - which no pass of this build's rule has
	// stated (unconvertedUnstated): a document the release before indexed, whose blocks hold none
	// of what this build's pass states until a pass of its rule reaches them. Those rows, and the
	// nodes of their blocks, are on neither side: the census reports their count as a class of
	// its own and compares none of them. It is a full `select count(*)`, empty for a kind no such
	// row is left out of.
	unstated string
	// owed is a predicate over node.id, a present node of the kind (for a kind held triple by
	// triple, the node holding the tuple), that holds where the node's row is owed by a writer
	// that has not run yet rather than missing: such a node is no node-side difference. Empty for
	// a kind whose every node has its row from the write that stated it.
	owed string
	// rowOwed is the row side's owed predicate, over node, the graph_nodes row a row of the kind
	// names (its columns null where the store holds none), that holds where the row is owed its
	// deletion by a writer that has not run yet rather than standing wrong: such a row is no
	// row-side difference. Empty for a kind every writer of whose rows deletes a row in the write
	// that ends its node.
	rowOwed string
	// emptyAgrees says a kind that holds no row, no present node and no unmarked row is no refusal:
	// one of the tie kinds, a decision's, a reading's and an answered or
	// followed ask's, which a deployment holds none of until an agent writes a decision or a
	// person answers an ask. Every other kind is one every store holds rows of, and checking
	// none of it refuses.
	emptyAgrees bool
	// mayBeEmpty says a deployment can hold no row of this kind - no run of the command bar, no
	// lens - so a census that checked zero rows of it proves nothing of it and refuses nothing for
	// it either (Unpopulated leaves it out). The run and frame kinds and the visibility and state
	// kinds of a run, a lens, a finding, a risk, a constraint and a lens kind are such kinds.
	mayBeEmpty bool
}

// KindReport is one kind's census: how many rows and how many present nodes it read, and what
// the two said about each other.
type KindReport struct {
	Kind, NodeKind, Table, Note string
	// Rows and Nodes are how many of each side the census read. Of a kind held triple by triple
	// (Triples), Rows counts the relations its rows state and Nodes the relations the store states.
	Rows, Nodes int64
	// Triples says the kind is held triple by triple (projection.tuples): Nodes counts tuples, a
	// row difference is a row no tuple states and a node difference a tuple no row projects.
	Triples bool
	// RowDifferences and NodeDifferences are how many there are in each direction, and the
	// Reported slices the first reportedDifferences of them, each as its cause and its row.
	RowDifferences, NodeDifferences int64
	ReportedRows, ReportedNodes     []Difference
	// PastBound is how many addresses this kind's row side named that are past the store's bound,
	// each a class of its own and no difference (projection.pastBound).
	PastBound int64
	// CountsUnmarked says the kind holds its table's rows that name no node as a class of their
	// own, and Unmarked is how many there are, none of them a difference (projection.unmarked).
	CountsUnmarked bool
	Unmarked       int64
	// CountsUnstated says the kind holds the rows of an unconverted issue's documents that no pass
	// of this build's rule has stated as a class of their own, and Unstated is how many there are,
	// none of them compared and none a difference (projection.unstated).
	CountsUnstated bool
	Unstated       int64
	// CountsOwed says the kind holds its rows that a pass of the backfill still owes a write as a
	// class of its own, and Owed is how many there are, none of them a difference: the Keeps a
	// release before summed over a decision's words with `shaped` (CensusOfKeepSums), which the
	// backfill's Keep pass rewrites, and which read zero once it has run.
	CountsOwed bool
	Owed       int64
	// EmptyAgrees says a kind that read no row, no present node and no unmarked row is no refusal
	// (projection.emptyAgrees).
	EmptyAgrees bool
	// MayBeEmpty is the kind's projection.mayBeEmpty: zero rows of it refuse nothing.
	MayBeEmpty bool
}

// Empty reports whether the kind read no row, no present node, no unmarked row and no unstated
// row, and so holds nothing to compare. A table whose rows all name no node yet holds rows still:
// they are the earlier release's, which the census cannot hold to a node until the backfill marks
// them; and so does one whose rows all wait for a pass of this build's rule.
func (k KindReport) Empty() bool {
	return k.Rows == 0 && k.Nodes == 0 && k.Unmarked == 0 && k.Unstated == 0
}

// Difference is one disagreement: why it is one, and the row or node it was found at.
type Difference struct{ Cause, At string }

// StoreReport is what the store census found. It is a report and not an error: a census that
// reads a difference has done its job, and the deployment reading it decides.
type StoreReport struct {
	Kinds []KindReport
	// Projects is the project census (ProjectCensus): for each kind whose nodes carry a project
	// or parties, its rows with no node, with a node naming neither, and with a node naming
	// another than the row's. Every count of it refuses the census. ProjectIncomplete names the
	// first of those rows (ProjectComplete), as many as the census names differences of a kind.
	Projects          []ProjectKindReport
	ProjectIncomplete []string
	// Absent is true where the database is no store this build can census (storeAbsence), and
	// AbsentBecause says why: graph_nodes does not exist, or a migration this binary embeds is not
	// applied yet. Every kind's read is this build's, written against the schema its migrations
	// leave, so over such a database the census reads no kind.
	Absent        bool
	AbsentBecause string
	// StoreOnly is, for each tuple kind no table projects (copied_from, moved_to and removed_by),
	// how many the store states now, how many of those reach a far end that does not resolve and
	// how many reach one in a document that still owes its block index (storeOnlyRead). An
	// unresolved far end refuses the census; a count does not, and neither does a far end left out.
	StoreOnly []StoreOnlyReport
	// Dangling is the dangling-citation counter over the restore's work (danglingCitations), and
	// Collisions the collision census of the same work (CollisionsOf), the block ids the restore
	// must change for each issue: both read the census's snapshot, so both answer before the
	// restore moves anything. Neither refuses the census: the returning set counts before the
	// restore by design, and the moved set is a finding the driver that watches it holds to zero.
	Dangling   DanglingReport
	Collisions []Collision
	// UnresolvedCitations is the counter of every block citation whose address resolves to no
	// node, directly or through a moved_to tuple (UnresolvedCitations), read in the census's
	// snapshot. It refuses nothing: an instance already holds prose citations of blocks no version
	// holds.
	UnresolvedCitations UnresolvedCitationReport
	// MovedOwnerWindows is how many windows the documents converted into their issues' rooms still
	// hold under their own artifact ids (movedOwnerWindows), read in the census's snapshot. It
	// refuses nothing: the root's pull request deletes them.
	MovedOwnerWindows int64
	// Decisions is what the decisions step's third phase still owes (DecisionsCensus), set by the
	// census command from docs' reader of the decisions documents' sections, nil where no caller
	// read it. It refuses nothing.
	//
	// kept for one release: decisions-documents-fn
	Decisions *DecisionsCensus
	// VersionRenders is the census of the versions version_renders holds (VersionRenderCensus),
	// set by the census command from docs' renderer, nil where no caller read it. A version whose
	// stored markdown is not its render refuses the census.
	VersionRenders *VersionRenderCensus
	// Sizing is what the sizing pass found, or nil when it was not asked for.
	Sizing *SizingReport
	// ReadSeq is the read sequence the census saw: the highest graph_tuples.seq its snapshot
	// holds, which is graph_tuples.seq at the commit the census read, and 0 for a store that
	// holds no tuple or is Absent. R29 of the parent change's specification has a query, a census
	// and an approval each record the read sequence it saw, so storeCensus reads it inside the
	// census's own repeatable-read transaction, in the snapshot every kind is read in. Read on
	// any other snapshot, it could name a tuple a write committed after the census's snapshot was
	// taken, which none of the census's counts saw.
	ReadSeq int64
}

// StoreOnlyReport is one tuple kind no table projects: how many relations of it the store states
// now, how many of those reach a far end that does not resolve, and how many the census leaves
// out, because the far end they reach is in a document that still owes its block index, which
// holds no row yet for a block its latest version holds.
type StoreOnlyReport struct {
	Kind                        string
	Tuples, Unresolved, LeftOut int64
}

// Unresolved is how many far ends of the store-only kinds do not resolve, over every such kind.
func (r *StoreReport) Unresolved() int64 {
	var unresolved int64
	for _, kind := range r.StoreOnly {
		unresolved += kind.Unresolved
	}
	return unresolved
}

// ProjectFindings is how many rows and nodes the project census found incomplete over every kind.
func (r *StoreReport) ProjectFindings() int64 {
	var found int64
	for _, kind := range r.Projects {
		found += kind.Findings()
	}
	return found
}

// Rows is how many projection rows the census checked over every kind.
func (r *StoreReport) Rows() int64 {
	var rows int64
	for _, kind := range r.Kinds {
		rows += kind.Rows
	}
	return rows
}

// Differences is how many disagreements the census read over every kind and both directions.
func (r *StoreReport) Differences() int64 {
	var found int64
	for _, kind := range r.Kinds {
		found += kind.RowDifferences + kind.NodeDifferences
	}
	return found
}

// Unpopulated is every kind the census checked no row of, in the order it read them, but a kind
// that may be empty (KindReport.EmptyAgrees) and is: no row, no present node and no unmarked row;
// and a kind a deployment may hold none of (KindReport.MayBeEmpty).
func (r *StoreReport) Unpopulated() []string {
	var empty []string
	for _, kind := range r.Kinds {
		if kind.Rows == 0 && !kind.MayBeEmpty && !(kind.EmptyAgrees && kind.Empty()) {
			empty = append(empty, kind.Kind)
		}
	}
	return empty
}

// Refused reports whether the census refuses. It passes only where it checked more than zero
// rows of every kind it covers and found no difference in either direction (R19), and where every
// far end of a kind no table projects resolves. A total over the kinds is no such proof: two
// empty sides of one kind agree, and one kind's rows say nothing about another's, so a census
// whose comments carried every row would pass while it proved nothing of the blocks, the roles or
// the sessions. One of the tie kinds is one exception: no row, no node and no
// unmarked row of it is a store nothing of that kind was written to yet, and is no refusal
// (projection.emptyAgrees). A table of the earlier release whose rows no backfill has marked yet
// is not that store: its kind checked no row while rows stand, and it refuses. A kind a
// deployment may hold none of - a run and its frames, and the visibility and state of a run, a
// lens, a finding, a risk, a constraint and a lens kind - is the other: it refuses nothing on zero
// rows alone (projection.mayBeEmpty); a driver over a corpus that holds them still reads the
// count. A version version_renders holds whose stored markdown is not its render refuses too
// (VersionRenders).
//
// A database the store does not exist in yet (Absent) has nothing to census. The sizing is taken
// on just such a database (R4 reads before the first migration), and the sizing refuses nothing
// (SizingReport), so there a census with the sizing passes on the sizing alone, and a census
// asked for without it refuses, since it read nothing.
func (r *StoreReport) Refused() bool {
	if r.Absent {
		return r.Sizing == nil
	}
	return len(r.Unpopulated()) > 0 || r.Differences() > 0 || r.ProjectFindings() > 0 || r.Unresolved() > 0 || r.renderDifferences() > 0
}

// renderDifferences is how many versions version_renders holds store other markdown than their
// render, 0 where the census read none of them.
func (r *StoreReport) renderDifferences() int64 {
	if r.VersionRenders == nil {
		return 0
	}
	return r.VersionRenders.Differences + r.VersionRenders.TreeMismatch
}

// StoreCensus reads the node and tuple store against the tables that project it, in one
// read-only repeatable-read transaction on a connection of its own. It writes nothing: the
// transaction is read only, and every statement it runs is a select, but for the savepoint the
// growth pass reads the store's type nodes in (installStoreBlockSchema).
//
// A difference is reported by its cause and by the row it was found at, in both directions: a
// projection row naming no node, naming a node the store does not hold or naming one it holds
// gone; a row whose shared fields disagree with its node's; and a node the store holds present
// that no row projects, named with its state.
//
// Two writes can state one fact twice by design and nothing here reads wrong for it: a tuple at
// the source_event sentinel is outside both repeat keys, and the backfill writes every fact it
// cannot date from a row's own event at that sentinel, so two runs at once that both read a row
// before either names its node each append a tuple stating the same fact. The current read takes
// the newest asserting tuple per far end, which is that same fact, and this census counts nodes
// and projection rows rather than tuples.
//
// sizing adds the growth pass (SizingReport), which reads the same snapshot. The growth pass
// reads no table of the store but its type nodes, and those only where the store holds them, so
// it runs on a database this build's migrations have not reached, where the census reads no kind
// and says the store is absent (storeAbsence).
//
// declined is the restore's test of which returns it declines (ReturnDeclined), which the
// dangling-citation counter reads its returning set through: nil where the caller has none, and
// then a census whose restore owes a return of the main shape ends in ErrNoReturnDeclined.
func StoreCensus(ctx context.Context, databaseURL string, sizing bool, declined ReturnDeclined) (*StoreReport, error) {
	return storeCensus(ctx, databaseURL, sizing, declined, nil)
}

// storeCensus is StoreCensus with afterKind, which runs once each kind has been read and before
// anything after it is, on the census's own context, and whose error ends the census. Only a
// test passes one: it is how a test commits a write between two of the census's reads and asks
// whether the reads after it still answer the snapshot the first one took. Nil runs nothing.
func storeCensus(ctx context.Context, databaseURL string, sizing bool, declined ReturnDeclined, afterKind func(ctx context.Context, kind string) error) (*StoreReport, error) {
	conn, err := store.Connect(ctx, databaseURL, applicationName)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("open the census snapshot: %w", err)
	}
	defer tx.Rollback(ctx)
	// No statement of the census is JIT-compiled. Each kind is one statement of a few dozen
	// subplans that runs once, and the planner costs one past jit_inline_above_cost and
	// jit_optimize_above_cost as soon as a table's statistics count its rows: a Postgres built
	// with JIT then spends seconds compiling and optimizing each statement before it reads a
	// row, more than the statement itself takes to run.
	if _, err := tx.Exec(ctx, `set local jit = off`); err != nil {
		return nil, fmt.Errorf("turn JIT off for the census snapshot: %w", err)
	}
	report := &StoreReport{}
	// The store is looked up before any kind is read, so a database this build's migrations have
	// not reached answers that rather than failing at the first statement that names a table or a
	// column one of them adds.
	if report.AbsentBecause, err = storeAbsence(ctx, tx); err != nil {
		return nil, err
	}
	report.Absent = report.AbsentBecause != ""
	if !report.Absent {
		// The highest seq is one probe of graph_tuples' primary key from its end, and reading it
		// in this transaction is what makes it the sequence of the snapshot the kinds are read in
		// (ReadSeq).
		if err := tx.QueryRow(ctx, `select coalesce(max(seq), 0) from graph_tuples`).Scan(&report.ReadSeq); err != nil {
			return nil, fmt.Errorf("read the census's read sequence: %w", err)
		}
		if report.Kinds, err = censusOfKinds(ctx, tx, afterKind); err != nil {
			return nil, err
		}
		if report.StoreOnly, err = readStoreOnly(ctx, tx); err != nil {
			return nil, err
		}
		work, err := ReadRestoreWork(ctx, tx)
		if err != nil {
			return nil, err
		}
		report.Collisions = CollisionsOf(work)
		if report.Dangling, err = danglingCitations(ctx, tx, work, declined); err != nil {
			return nil, err
		}
		if report.UnresolvedCitations, err = UnresolvedCitations(ctx, tx); err != nil {
			return nil, err
		}
		if report.MovedOwnerWindows, err = movedOwnerWindows(ctx, tx); err != nil {
			return nil, err
		}
		if report.Projects, err = ProjectCensus(ctx, tx); err != nil {
			return nil, err
		}
		if report.ProjectFindings() > 0 {
			limit := reportedDifferences
			if report.ProjectIncomplete, err = projectIncomplete(ctx, tx, &limit); err != nil {
				return nil, err
			}
		}
	}
	if sizing {
		sized, err := readSizing(ctx, tx)
		if err != nil {
			return nil, err
		}
		report.Sizing = sized
	}
	return report, nil
}

// storeAbsence says why the database tx reads is no store this build can census, "" where it is
// one. Every read of a kind is written against the schema this build's migrations leave, so the
// census reads the store only over a database that records every migration this binary embeds
// (store.UnappliedMigrations): the release before this one, or its parent's, may hold a store of
// its own - graph_nodes and the rest - without the tables and columns a later migration adds, and
// a census taken there before the rollout migrates it would fail at the first statement that names
// one. graph_nodes missing is said first, since then no store exists at all.
func storeAbsence(ctx context.Context, tx pgx.Tx) (string, error) {
	var missing bool
	if err := tx.QueryRow(ctx, `select to_regclass('graph_nodes') is null`).Scan(&missing); err != nil {
		return "", fmt.Errorf("look up the store: %w", err)
	}
	if missing {
		return "graph_nodes does not exist", nil
	}
	unapplied, err := store.UnappliedMigrations(ctx, tx)
	if err != nil {
		return "", fmt.Errorf("read which of this build's migrations the database has applied: %w", err)
	}
	switch len(unapplied) {
	case 0:
		return "", nil
	case 1:
		return fmt.Sprintf("this build's migration %d is not applied", unapplied[0]), nil
	default:
		return fmt.Sprintf("this build's migration %d is not applied, nor %d after it", unapplied[0], len(unapplied)-1), nil
	}
}

// currentOutOfNodes is two items of a `with recursive` list: name, every relation of the tuple
// kinds kinds out of a node of a kind of nearKinds that the store states now - the newest tuple of
// each, kept while it asserts (graphstore.CurrentTupleRead) - as (from_kind, from_id, kind,
// to_kind, to_id), and name_near, the nodes it reads them out of (relationNodes). It reads each
// such node's tuples of kinds through one probe of graph_tuples_current on (from_kind, from_id,
// kind), stating that index's predicate, which every kind of kinds meets, since each is a relation
// and none is delivered_to.
func currentOutOfNodes(name string, nearKinds, kinds []string) string {
	return relationNodes(name, nearKinds) + `,
	` + name + ` as (
		select held.from_kind, held.from_id, held.kind, held.to_kind, held.to_id
		from ` + name + `_near near
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`,
		`t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = near.from_kind and t.from_id = near.from_id
			  and `+kindIn("t.kind", kinds)+` and t.to_kind <> '`+graphstore.KindValue+`'`) + `
		) held
		where held.asserted
	)`
}

// statedOutOfNodes is currentOutOfNodes' two items for a read of what the log ever stated: name is
// every node of a kind of nearKinds that ever stated a relation of the tuple kinds kinds, asserted
// now or retracted since, once each, as (from_kind, from_id), and name_near the nodes it reads them
// out of (relationNodes). It probes each node once on graph_tuples_current's (from_kind, from_id,
// kind), as currentOutOfNodes does, and keeps the node where any tuple of kinds out of it stands.
// The probe is a lateral read that ends in `limit 1`, which the planner cannot pull up into the
// join: as an `exists` it is planned as a semi-join that reads every tuple of kinds, keyed by the
// kind alone, however few nodes the walk reaches.
func statedOutOfNodes(name string, nearKinds, kinds []string) string {
	return relationNodes(name, nearKinds) + `,
	` + name + ` as (
		select near.from_kind, near.from_id
		from ` + name + `_near near
		cross join lateral (
			select 1 from graph_tuples t
			where t.from_kind = near.from_kind and t.from_id = near.from_id
			  and ` + kindIn("t.kind", kinds) + ` and t.to_kind <> '` + graphstore.KindValue + `'
			limit 1
		) stated
	)`
}

// relationNodes is the item name_near of a `with recursive` list: every node of a kind of
// nearKinds that states a relation, once each, as (from_kind, from_id), which currentOutOfNodes
// and statedOutOfNodes read their tuples out of. No index of the store's set
// (20261109013400_graph_store.up.sql) leads with a tuple's kind, so a read of every tuple of a
// kind starts from the nodes the kind leaves: the walk takes one probe of graph_tuples_current per
// node on its leading (from_kind, from_id) for the next from_id past the last one. It reads the
// nodes off the log rather than off graph_nodes: a writer states these kinds without stating the
// node they leave (docs' writeFacts states a moved_to out of a heading's address and a removed_by
// out of an ask's node and writes neither node), so a tuple whose near end has no node is read
// all the same. The probe states graph_tuples_current's predicate.
func relationNodes(name string, nearKinds []string) string {
	relations := `t.to_kind <> '` + graphstore.KindValue + `' and t.kind <> '` + graphstore.TupleDeliveredTo + `'`
	return name + `_near (from_kind, from_id) as (
		select first.from_kind, first.from_id
		from (values ('` + strings.Join(nearKinds, `'), ('`) + `')) near_kind (from_kind)
		cross join lateral (
			select t.from_kind, t.from_id from graph_tuples t
			where t.from_kind = near_kind.from_kind and ` + relations + `
			order by t.from_id limit 1
		) first
		union all
		select next.from_kind, next.from_id
		from ` + name + `_near near
		cross join lateral (
			select t.from_kind, t.from_id from graph_tuples t
			where t.from_kind = near.from_kind and t.from_id > near.from_id and ` + relations + `
			order by t.from_id limit 1
		) next
	)`
}

// storeOnlyRead counts, in one statement, every relation of the three kinds no table projects
// that the store states now - the newest tuple of each (near end, far end), kept while it asserts
// - and those of them whose far end does not resolve: a copied_from (a copy's block to the ask it
// was copied from, graphstore.TupleCopiedFrom) resolves to an ask node that is not gone; a
// moved_to (graphstore.TupleMovedTo) to a block that stands, which is one a document_blocks row
// holds, reached at once or through more moved_to tuples, a cycle reaching none, or to an ask
// node that is not gone, as a copied_from does, which ends a chain (the heading of a section that
// cites an answered ask a person removed from its spec); and a removed_by
// (graphstore.TupleRemovedBy, an ask's node to the person who took its block out) to a person node
// of the store. A far end is followed through one tuple fewer than graphstore.MovedToHops, since
// the tuple that names it is the first of the chain a citation of its near end is read through
// (graphstore.MovedTo), so a far end that resolves here is one every resolver reaches from that
// near end. A moved_to from a document's address - the pre-mortem conversion's, from the document
// it retires to the lens it wrote in its place - resolves to that lens where it is not gone, as
// graphstore.MovedTo reads it, and moves no further.
//
// A moved_to whose chain reaches no block that stands but reaches an address in a document that
// still owes its block index (document_block_versions.version null, as typedNodeOwed reads it) is
// left out, a class of its own and not an unresolved far end: such a document holds no row yet for
// a block its latest version holds - one past the deferral bound, or one a build before wrote
// during the roll - and the block-index reconciliation writes it.
//
// The read takes the three kinds' tuples out of every block, ask and document node the log holds
// (currentOutOfNodes), the near ends their writers state them out of - a copied_from and a
// moved_to out of a block, a moved_to out of a document's address, a removed_by out of an ask's
// node - through graph_tuples_current, one probe per node, and a store that holds none of them
// reads three zero counts. It states that index's to_kind predicate, which leaves out no tuple of
// any of them, since none of their far ends is a value.
var storeOnlyRead = `
	with recursive ` + currentOutOfNodes("stated", []string{refs.BlockNodeKind, graphstore.KindAsk, graphstore.KindArtifact},
	[]string{graphstore.TupleCopiedFrom, graphstore.TupleMovedTo, graphstore.TupleRemovedBy}) + `,
	followed (start, kind, address, hops, path) as (
		select far.to_id, far.to_kind, far.to_id, 0, array[far.to_id]
		from (select distinct s.to_kind, s.to_id from stated s where s.kind = '` + graphstore.TupleMovedTo + `') far
		union all
		select f.start, m.to_kind, m.to_id, f.hops + 1, f.path || m.to_id
		from followed f
		join stated m on m.kind = '` + graphstore.TupleMovedTo + `' and m.from_kind = f.kind and m.from_id = f.address
		where f.kind = '` + refs.BlockNodeKind + `' and f.hops < ` + strconv.Itoa(graphstore.MovedToHops-1) + ` and m.to_id <> all(f.path)
		  and not exists (select 1 from document_blocks d where d.node_id = f.address)
	),
	standing as (
		select distinct f.start from followed f
		where f.kind = '` + refs.BlockNodeKind + `' and exists (select 1 from document_blocks d where d.node_id = f.address)
		   or f.kind = '` + graphstore.KindAsk + `' and exists (
			select 1 from graph_nodes ask where ask.kind = '` + graphstore.KindAsk + `' and ask.id = f.address
			  and ask.state <> '` + graphstore.StateGone + `')
	),
	owing as (
		select distinct f.start from followed f
		where f.kind = '` + refs.BlockNodeKind + `' and exists (
			select 1 from artifacts a
			join document_block_versions owed on owed.artifact_id = a.id
			where a.ref_key = split_part(f.address, '#', 1) and owed.version is null)
	),
	resolved as (
		select s.kind, case s.kind
			when '` + graphstore.TupleCopiedFrom + `' then s.to_kind = '` + graphstore.KindAsk + `' and exists (
				select 1 from graph_nodes ask where ask.kind = '` + graphstore.KindAsk + `' and ask.id = s.to_id
				  and ask.state <> '` + graphstore.StateGone + `')
			when '` + graphstore.TupleRemovedBy + `' then s.to_kind = '` + graphstore.KindPerson + `' and exists (
				select 1 from graph_nodes person where person.kind = '` + graphstore.KindPerson + `' and person.id = s.to_id)
			else (s.to_kind in ('` + refs.BlockNodeKind + `', '` + graphstore.KindAsk + `')
			  and exists (select 1 from standing where standing.start = s.to_id))
			  or (s.from_kind = '` + graphstore.KindArtifact + `' and s.to_kind = '` + graphstore.KindLens + `' and exists (
				select 1 from graph_nodes lens where lens.kind = '` + graphstore.KindLens + `' and lens.id = s.to_id
				  and lens.state <> '` + graphstore.StateGone + `'))
		end as resolves,
		s.kind = '` + graphstore.TupleMovedTo + `' and s.to_kind = '` + refs.BlockNodeKind + `'
		  and exists (select 1 from owing where owing.start = s.to_id) as left_out
		from stated s
	)
	select listed.kind, count(r.kind),
	       count(r.kind) filter (where not r.resolves and not r.left_out),
	       count(r.kind) filter (where not r.resolves and r.left_out)
	from (values (1, '` + graphstore.TupleCopiedFrom + `'), (2, '` + graphstore.TupleMovedTo + `'), (3, '` + graphstore.TupleRemovedBy + `')) listed (position, kind)
	left join resolved r on r.kind = listed.kind
	group by listed.position, listed.kind
	order by listed.position`

// readStoreOnly reads the kinds no table projects in the census's snapshot (storeOnlyRead), one
// report for each whether or not the store holds any of it, copied_from first.
func readStoreOnly(ctx context.Context, tx pgx.Tx) ([]StoreOnlyReport, error) {
	rows, err := tx.Query(ctx, storeOnlyRead)
	if err != nil {
		return nil, fmt.Errorf("read the kinds no table projects: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (StoreOnlyReport, error) {
		var kind StoreOnlyReport
		err := row.Scan(&kind.Kind, &kind.Tuples, &kind.Unresolved, &kind.LeftOut)
		return kind, err
	})
}

// censusOfKinds reads every kind of projections in order, running afterKind after each.
func censusOfKinds(ctx context.Context, tx pgx.Tx, afterKind func(ctx context.Context, kind string) error) ([]KindReport, error) {
	var kinds []KindReport
	// A kind's names are read before the first kind that binds them and kept for every kind after
	// it that binds the same ones, as the page kind binds the role kind's. The stored authors are
	// read before the first kind whose names read them and kept for every kind after it.
	read := map[string][]string{}
	var authors *storedWriters
	for _, held := range projections {
		var args []any
		if held.names != "" {
			names, done := read[held.names]
			if !done {
				var bound []any
				if held.authored {
					if authors == nil {
						var err error
						if authors, err = readStoredAuthors(ctx, tx); err != nil {
							return nil, err
						}
					}
					bound = []any{authors.kinds, authors.ids, authors.ats}
				}
				var err error
				if names, err = readNames(ctx, tx, held.name, held.names, bound); err != nil {
					return nil, err
				}
				read[held.names] = names
			}
			args = []any{names}
		}
		kind, err := censusOfKind(ctx, tx, held, args)
		if err != nil {
			return nil, err
		}
		kinds = append(kinds, kind)
		if afterKind != nil {
			if err := afterKind(ctx, held.name); err != nil {
				return nil, err
			}
		}
	}
	// The Go readers come after the projections: Keeps, whose decisions' words only Go renders;
	// review carries, whose legacy reasons Go parses; and last the head projection, over every
	// node kind, once every projection of the nodes its rows read has been read.
	for _, read := range []func(context.Context, pgx.Tx) (KindReport, error){CensusOfKeepSums, censusOfReviewCarries, CensusOfHeads} {
		kind, err := read(ctx, tx)
		if err != nil {
			return nil, err
		}
		kinds = append(kinds, kind)
		if afterKind != nil {
			if err := afterKind(ctx, kind.Kind); err != nil {
				return nil, err
			}
		}
	}
	return kinds, nil
}

// censusOfKind reads one kind: how many rows and present nodes there are, and the differences in
// each direction with the first reportedDifferences of them. args are what the kind's rows bind,
// none but for a kind that binds its names (projection.names).
//
// The four counts are four scalar subqueries of one statement rather than four round trips, so
// a census costs one read a kind here and not four. Each subquery is the statement it was on its
// own, so each is planned and streamed exactly as before.
func censusOfKind(ctx context.Context, tx pgx.Tx, held projection, args []any) (KindReport, error) {
	kind := KindReport{Kind: held.name, NodeKind: held.nodeKind, Table: held.table, Note: held.note,
		Triples: held.tuples != "", CountsUnmarked: held.unmarked != "", CountsUnstated: held.unstated != "",
		EmptyAgrees: held.emptyAgrees, MayBeEmpty: held.mayBeEmpty}
	read := sidesOf(held)
	// pastBound, unmarked and unstated are each a scalar `select count(*)`, so each is read
	// directly rather than wrapped in a count of its own row; a kind without the class reads zero.
	pastBound, unmarked, unstated := held.pastBound, held.unmarked, held.unstated
	if pastBound == "" {
		pastBound = "select 0"
	}
	if unmarked == "" {
		unmarked = "select 0"
	}
	if unstated == "" {
		unstated = "select 0"
	}
	if err := tx.QueryRow(ctx, `select
		(select count(*) from (`+held.rows+`) rows_read),
		(select count(*) from (`+read.nodes+`) nodes_read),
		(select count(*) from (`+read.rowsFound+`) rows_found),
		(select count(*) from (`+read.nodesFound+`) nodes_found),
		(`+pastBound+`),
		(`+unmarked+`),
		(`+unstated+`)`, args...,
	).Scan(&kind.Rows, &kind.Nodes, &kind.RowDifferences, &kind.NodeDifferences, &kind.PastBound, &kind.Unmarked, &kind.Unstated); err != nil {
		return KindReport{}, fmt.Errorf("census of %s: %w", held.name, err)
	}
	var err error
	if kind.RowDifferences > 0 {
		kind.ReportedRows, err = readDifferences(ctx, tx, held.name, read.rowsFound, args)
		if err != nil {
			return KindReport{}, err
		}
	}
	if kind.NodeDifferences > 0 {
		kind.ReportedNodes, err = readDifferences(ctx, tx, held.name, read.nodesFound, args)
		if err != nil {
			return KindReport{}, err
		}
	}
	return kind, nil
}

// kindSides are the reads censusOfKind counts of one kind besides its rows: its node side, and the
// differences in each direction, each of those two a select of (cause, at).
type kindSides struct{ nodes, rowsFound, nodesFound string }

// sidesOf is held's reads: of a kind held triple by triple its tuples and the rows and tuples
// the other side does not hold, and of any other its nodes not gone and the rows and nodes that
// disagree.
func sidesOf(held projection) kindSides {
	if held.tuples != "" {
		return kindSides{nodes: held.tuples, rowsFound: tripleRowDifferences(held), nodesFound: tupleDifferences(held)}
	}
	return kindSides{nodes: presentNodes(held), rowsFound: rowDifferences(held), nodesFound: nodeDifferences(held)}
}

// presentNodes is the kind's node side bound to every state but gone: a node the store holds gone
// is no node a row may project, and the row side holds it to that separately. Any other state is
// a node a row projects, so a state a later release adds, such as proposed, settled and
// overtaken (R8 of this change), reads here as live does. It answers (kind, id, digest, state):
// the kind's nodes answer (id, digest), each is given the kind here, and its state beside its
// digest, which nodeDifferences names a node with. Each node's state is one probe of graph_nodes'
// primary key: the lateral read ends in `limit 1`, so the planner cannot make it a join of the
// kind's nodes with themselves, which statistics counting one node of a kind that holds thousands
// plan as a nested loop over every node of the kind for each node.
func presentNodes(held projection) string {
	return `select present.kind, node.id, node.digest, present.state from (` + held.nodes + `) node (id, digest)
		cross join lateral (
			select n.kind, n.state from graph_nodes n
			where n.kind = '` + held.nodeKind + `' and n.id = node.id and n.state <> '` + graphstore.StateGone + `'
			limit 1
		) present`
}

// rowDifferences reads the row side against the store: every row that names no node, names one
// the store does not hold, names one it holds gone, or disagrees with its node's digest, but a row
// whose deletion is owed by a writer that has not run yet (projection.rowOwed). The gone test is
// its own cause rather than a missing node, because a row projecting a node the store has ended is
// a different defect from a row projecting nothing, and it is the one test of a node's state
// here: a row may project a node at any other state.
//
// The two sides meet in one sorted pass, never in a join: both are laid end to end (union all),
// each tagged with its side, and each row reads the digests of the present nodes of its own
// address from a window over (node kind, address), and is read once against each of them, or once
// against none, as a left join of the two reads it. A join of the two is planned from each side's
// estimated rows, and where the statistics count one row of a side that holds thousands - a table
// a bulk write has just filled, before autovacuum reads it again - the planner joins them in a
// nested loop that reads one whole side, with each node's digest, once for each row of the other:
// the census of a document of 2,002 blocks took 108 s in one kind and 18 s in another. The node a
// row names, at any state, is one probe of graph_nodes' primary key per row: the lateral read ends
// in `limit 1`, so the planner cannot pull it up into a join.
func rowDifferences(held projection) string {
	return `
		select case
			when row.node_kind is null or row.node_address is null then 'the row names no node'
			when node.kind is null then 'the row names a node the store does not hold'
			when node.state = '` + graphstore.StateGone + `'
				then 'the row names a node the store holds ' || node.state
			else 'the row and its node disagree: the row digests ' || row.digest
			     || ' and the node ' || coalesce(present.digest, '(none)')
		end,
		row.row_label
		from (
			select side.row_side, side.row_label, side.node_kind, side.node_address, side.digest,
			       array_agg(side.node_digest) filter (where not side.row_side)
			           over (partition by side.node_kind, side.node_address) as present_digests
			from (
				select true, row_label, node_kind, node_address, digest, null
				from (` + held.rows + `) row (row_label, node_kind, node_address, digest)
				union all
				select false, null, kind, id, null, digest
				from (` + presentNodes(held) + `) present (kind, id, digest, state)
			) side (row_side, row_label, node_kind, node_address, digest, node_digest)
		) row
		left join lateral unnest(row.present_digests) present (digest) on true
		left join lateral (
			select * from graph_nodes n where n.kind = row.node_kind and n.id = row.node_address limit 1
		) node on true
		where row.row_side
		  and (row.node_kind is null or row.node_address is null or node.kind is null
		   or node.state = '` + graphstore.StateGone + `'
		   or row.digest is distinct from present.digest)` + rowNotOwed(held) + `
		order by 2, 1`
}

// nodeDifferences reads the store against the row side: every node of the kind not gone that no
// row projects, named with the state it is at, so a live one reads as `a live node no row
// projects`, but a node whose row is owed by a writer that has not run yet (projection.owed).
//
// A node is projected where a row of its address stands in its window, the one sorted pass of the
// two sides rowDifferences makes, which answers what `not exists (... where row.node_kind =
// node.kind and row.node_address = node.id)` answers without a join: a present node's kind and id
// are never null, and a row of a null kind or address shares a window with no node, as `=` matches
// no null.
func nodeDifferences(held projection) string {
	return `
		select case when node.state ~ '^[aeiou]' then 'an ' else 'a ' end || node.state
		       || ' node no row projects',
		       node.kind || ' ' || node.id
		from (
			select side.row_side, side.kind, side.id, side.state,
			       bool_or(side.row_side) over (partition by side.kind, side.id) as projected
			from (
				select false, kind, id, state
				from (` + presentNodes(held) + `) present (kind, id, digest, state)
				union all
				select true, node_kind, node_address, null
				from (` + held.rows + `) row (row_label, node_kind, node_address, digest)
			) side (row_side, kind, id, state)
		) node
		where not node.row_side and not node.projected` + notOwed(held) + `
		order by 2`
}

// notOwed is the clause a node-side read of held appends to leave out a node whose row is owed
// (projection.owed), empty for a kind whose every node has its row.
func notOwed(held projection) string {
	if held.owed == "" {
		return ""
	}
	return `
		  and not ` + held.owed
}

// rowNotOwed is the clause a row-side read of held appends to leave out a row whose deletion is
// owed (projection.rowOwed), empty for a kind every writer of whose rows deletes them with their
// node. The predicate reads the row's node, joined as node, whose columns are null where the store
// holds none, so a row it does not hold of is kept.
func rowNotOwed(held projection) string {
	if held.rowOwed == "" {
		return ""
	}
	return `
		  and (` + held.rowOwed + `) is not true`
}

// tripleRowDifferences reads the rows of a kind held triple by triple against the store: every
// row that names no node, names one the store does not hold or holds gone, or states a triple no
// tuple of the kind states now, each at the row and the triple it states, but a row whose deletion
// is owed by a writer that has not run yet (projection.rowOwed). As rowDifferences, it tests no
// state of a node but gone: a row may project a node at any other state.
//
// The rows meet the tuples as rowDifferences meets its two sides, in one sorted pass and never in
// a join: a row is stated where a tuple of its triple, (node kind, address, kind, far end), stands
// in its window. A row with a null anywhere in its triple is stated by no tuple, as `=` matches no
// null, so it is a difference whatever its window holds. The node it names is one primary-key
// probe, as in rowDifferences.
func tripleRowDifferences(held projection) string {
	return `
		select case
			when row.node_kind is null or row.node_address is null then 'the row names no node'
			when node.kind is null then 'the row names a node the store does not hold'
			when node.state = '` + graphstore.StateGone + `'
				then 'the row names a node the store holds ' || node.state
			else 'a row no tuple states'
		end,
		row.row_label || ' ' || row.kind || ' -> ' || row.to_kind || ' ' || row.to_id
		from (
			select side.row_side, side.row_label, side.node_kind, side.node_address, side.kind, side.to_kind, side.to_id,
			       bool_or(not side.row_side) over (
			           partition by side.node_kind, side.node_address, side.kind, side.to_kind, side.to_id) as stated
			from (
				select true, row_label, node_kind, node_address, kind, to_kind, to_id
				from (` + held.rows + `) row (row_label, node_kind, node_address, kind, to_kind, to_id)
				union all
				select false, null, from_kind, from_id, kind, to_kind, to_id
				from (` + held.tuples + `) stated (from_kind, from_id, kind, to_kind, to_id)
			) side (row_side, row_label, node_kind, node_address, kind, to_kind, to_id)
		) row
		left join lateral (
			select * from graph_nodes n where n.kind = row.node_kind and n.id = row.node_address limit 1
		) node on true
		where row.row_side
		  and (row.node_kind is null or row.node_address is null or node.kind is null
		   or node.state = '` + graphstore.StateGone + `'
		   or row.kind is null or row.to_kind is null or row.to_id is null or not row.stated)` + rowNotOwed(held) + `
		order by 2, 1`
}

// tupleDifferences reads the store's tuples of a kind held triple by triple against its rows:
// every tuple no row states the triple of, at from_kind:from_id kind -> to_kind:to_id, but a tuple
// of a node whose row is owed by a writer that has not run yet (projection.owed).
//
// A tuple is projected where a row of its triple stands in its window, the one sorted pass of the
// two sides tripleRowDifferences makes; a tuple with a null anywhere in its triple is projected by
// no row, as `=` matches no null.
func tupleDifferences(held projection) string {
	return `
		select 'a tuple no row projects',
		       node.kind || ':' || node.id || ' ' || node.tie || ' -> ' || node.to_kind || ':' || node.to_id
		from (
			select side.row_side, side.kind, side.id, side.tie, side.to_kind, side.to_id,
			       bool_or(side.row_side) over (
			           partition by side.kind, side.id, side.tie, side.to_kind, side.to_id) as projected
			from (
				select false, kind, id, tie, to_kind, to_id
				from (` + held.tuples + `) node (kind, id, tie, to_kind, to_id)
				union all
				select true, node_kind, node_address, kind, to_kind, to_id
				from (` + held.rows + `) row (row_label, node_kind, node_address, kind, to_kind, to_id)
			) side (row_side, kind, id, tie, to_kind, to_id)
		) node
		where not node.row_side
		  and (node.kind is null or node.id is null or node.tie is null or node.to_kind is null or node.to_id is null
		   or not node.projected)` + notOwed(held) + `
		order by 2`
}

func readDifferences(ctx context.Context, tx pgx.Tx, kind, sql string, args []any) ([]Difference, error) {
	rows, err := tx.Query(ctx, sql+` limit `+strconv.Itoa(reportedDifferences), args...)
	if err != nil {
		return nil, fmt.Errorf("read the differences of %s: %w", kind, err)
	}
	defer rows.Close()
	var found []Difference
	for rows.Next() {
		var difference Difference
		if err := rows.Scan(&difference.Cause, &difference.At); err != nil {
			return nil, fmt.Errorf("read a difference of %s: %w", kind, err)
		}
		found = append(found, difference)
	}
	return found, rows.Err()
}

// Write prints the report, one `census: store ` line per fact, every difference under its kind,
// the store-only kinds, the dangling-citation counter and the collision census, and the sizing
// after them, ending with the verdict. A kind held triple by triple prints the store's tuple count
// and its rows' triple count, and the differences as the rows no tuple states and the tuples no row
// projects; a store-only kind prints the far ends it leaves out as left_out. It prints counts,
// digests, kinds and the addresses of rows and nodes; no body, no actor and no value of an
// attribute leaves the database.
func (r *StoreReport) Write(w io.Writer) { r.write(w, false) }

// WriteVerbose is Write with one line for each citation the dangling-citation counter counted,
// naming its set, the node that cites and the address it cites, under the counter's line, and one
// for each citation the counter of unresolved citations counted (writeUnresolvedCitations).
func (r *StoreReport) WriteVerbose(w io.Writer) { r.write(w, true) }

func (r *StoreReport) write(w io.Writer, verbose bool) {
	if r.Absent {
		fmt.Fprintf(w, "census: store absent (%s): the database is not migrated to this build, so no kind was read\n", r.AbsentBecause)
	}
	for _, kind := range r.Kinds {
		// A kind held triple by triple counts tuples where every other kind counts nodes.
		side := "node"
		if kind.Triples {
			side = "tuple"
		}
		fmt.Fprintf(w, "census: store kind=%s %ss=%s rows=%s table=%s rows_without_a_%s=%s %ss_without_a_row=%s addresses_past_bound=%s\n",
			kind.Kind, side, pgmigrate.Thousands(kind.Nodes), pgmigrate.Thousands(kind.Rows), kind.Table,
			side, pgmigrate.Thousands(kind.RowDifferences), side, pgmigrate.Thousands(kind.NodeDifferences),
			pgmigrate.Thousands(kind.PastBound))
		fmt.Fprintf(w, "census: store kind=%s checks %s\n", kind.Kind, kind.Note)
		if kind.CountsUnmarked {
			fmt.Fprintf(w, "census: store kind=%s unmarked_rows=%s (rows a writer of the release before wrote, which name no node yet and graph_edges' old arms answer)\n",
				kind.Kind, pgmigrate.Thousands(kind.Unmarked))
		}
		if kind.CountsUnstated {
			fmt.Fprintf(w, "census: store kind=%s unstated_rows=%s (rows of a document of an issue that has not converted which no pass of this build's rule has stated, as the release before leaves them: counted, compared with nothing)\n",
				kind.Kind, pgmigrate.Thousands(kind.Unstated))
		}
		if kind.CountsOwed {
			fmt.Fprintf(w, "census: store kind=%s owed_rows=%s (rows a pass of the backfill still owes a write, which it makes on its next run: counted, no difference)\n",
				kind.Kind, pgmigrate.Thousands(kind.Owed))
		}
		switch {
		case kind.EmptyAgrees && kind.Empty():
			fmt.Fprintf(w, "census: store kind=%s holds no row and no %s: nothing of it is written yet, which is no refusal\n", kind.Kind, side)
		case kind.Rows == 0 && kind.MayBeEmpty:
			fmt.Fprintf(w, "census: store kind=%s checked no row, so the census proves nothing of it; a deployment may hold none, so it refuses nothing\n", kind.Kind)
		case kind.Rows == 0:
			fmt.Fprintf(w, "census: store kind=%s checked no row, so the census proves nothing of it\n", kind.Kind)
		}
		writeDifferences(w, kind.Kind, kind.RowDifferences, kind.ReportedRows)
		writeDifferences(w, kind.Kind, kind.NodeDifferences, kind.ReportedNodes)
	}
	for _, kind := range r.StoreOnly {
		fmt.Fprintf(w, "census: store-only kind=%s tuples=%s unresolved=%s left_out=%s\n",
			kind.Kind, pgmigrate.Thousands(kind.Tuples), pgmigrate.Thousands(kind.Unresolved), pgmigrate.Thousands(kind.LeftOut))
	}
	if !r.Absent {
		writeMoves(w, r.Dangling, r.Collisions, verbose)
		writeUnresolvedCitations(w, r.UnresolvedCitations, verbose)
		writeMovedOwnerWindows(w, r.MovedOwnerWindows)
		// kept for one release: decisions-documents-fn
		if r.Decisions != nil {
			writeDecisions(w, *r.Decisions)
		}
		if r.VersionRenders != nil {
			writeVersionRenders(w, *r.VersionRenders)
		}
	}
	writeProjectCensus(w, r.Projects)
	for _, row := range r.ProjectIncomplete {
		fmt.Fprintf(w, "census: project incomplete %s\n", row)
	}
	if !r.Absent {
		// read_seq is printed whole, without Thousands' separators: it is no count but a position
		// in graph_tuples, and a reader holds it against graph_tuples.seq as a statement writes it.
		fmt.Fprintf(w, "census: store rows_checked=%s differences=%s read_seq=%d\n",
			pgmigrate.Thousands(r.Rows()), pgmigrate.Thousands(r.Differences()), r.ReadSeq)
	}
	if r.Sizing != nil {
		r.Sizing.Write(w)
	}
	empty := r.Unpopulated()
	switch {
	case r.Absent && r.Sizing == nil:
		fmt.Fprintln(w, "census: store REFUSED (the database holds no store of this build, so the census read nothing)")
	case r.Absent:
		fmt.Fprintln(w, "census: store absent, sizing ok")
	case r.Differences() > 0:
		fmt.Fprintf(w, "census: store REFUSED (%s difference(s))\n", pgmigrate.Thousands(r.Differences()))
	case r.renderDifferences() > 0:
		fmt.Fprintf(w, "census: store REFUSED (%s rendered version(s) store other markdown than their render)\n",
			pgmigrate.Thousands(r.renderDifferences()))
	case r.ProjectFindings() > 0:
		fmt.Fprintf(w, "census: store REFUSED (%s row(s) whose node names no project or the wrong one, or node(s) no row stands behind;"+
			" run envoy-dispatch backfill-graph-store for the missing, gone and unscoped ones but those naming a party the store"+
			" refuses, and compare each differs row with its node; an orphaned node has no in-band cure, unless an older build's removal made it during a two-build roll,"+
			" which this build's block-index tick then states gone)\n",
			pgmigrate.Thousands(r.ProjectFindings()))
	case r.Unresolved() > 0:
		fmt.Fprintf(w, "census: store REFUSED (%s far end(s) of a kind no table projects do not resolve)\n",
			pgmigrate.Thousands(r.Unresolved()))
	case len(empty) > 0:
		fmt.Fprintf(w, "census: store REFUSED (kind(s) %s checked no row, so the census proves nothing of them)\n",
			strings.Join(empty, ", "))
	default:
		fmt.Fprintln(w, "census: store ok")
	}
}

func writeDifferences(w io.Writer, kind string, total int64, found []Difference) {
	for _, difference := range found {
		fmt.Fprintf(w, "census: store kind=%s difference at %s: %s\n", kind, difference.At, difference.Cause)
	}
	if total > int64(len(found)) {
		fmt.Fprintf(w, "census: store kind=%s and %s more difference(s) not printed\n",
			kind, pgmigrate.Thousands(total-int64(len(found))))
	}
}

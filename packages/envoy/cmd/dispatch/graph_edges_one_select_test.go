package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/refs/refstest"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The commits the processes of this test register under (cmdtest.BuildStamped): the root's build,
// and a later build of it, which only a stamp tells apart from a restart of the root's. The other
// two are builds no process of this test runs, named by the announcements a database holds: the
// children's release, the build the fleet served before the root's, and the release before this
// tree, which no child had reached.
const (
	oneSelectRootBuild       = "7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b"
	oneSelectLaterBuild      = "7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c"
	oneSelectChildrenRelease = "1253125312531253125312531253125312531253"
	oneSelectBeforeTheTree   = "b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4b4"
)

// releaseBeforeTheTree is the newest migration of the release before this tree: the newest of
// main's migrations dated before the tree's first, 20261109013400_graph_store. A database
// migrated through it holds the schema that release's boot left, with no node column on any
// table and no tuple_kinds on server_build_announcements.
const releaseBeforeTheTree = 20261108070100

// The two parts of the deletion this head does not build, each named where the test reaches it.
const (
	infra1152Part = "dispatch://INFRA-1152's part, built once INFRA-1152 and INFRA-1253 are on one line"
	infra1149Part = "one of the four arms dispatch://INFRA-1149 moved, which dispatch://INFRA-1253 deletes: INFRA-1253's head still holds it"
	unlistedArm   = "on none of the lists of c-view-one-select, so no child's deletion takes it"
)

// At the root's head graph_edges is one select over the current tuples, with the tuple arm's class
// filter, no arm over a source column and no join to a projection row (c-view-one-select).
//
// The arms of the view before the root's deletion, read from a database Migrate brought to this
// head, are each one child's, and the test fails on an arm on none of the check's lists. The code
// goes with no condition: dispatch://INFRA-1150's catch-up from the `ask.*` events and
// rollWindowAskAttributes are no declarations of the docs package, read from its own source after a
// scan has found a planted instance in a scratch copy.
//
// The stored objects go in a step of every boot (startup.AsksArmsRetirement,
// startup.IssueTreeArmsRetirement), over these databases:
//   - released: its newest announcement of another build lists every kind this build lists,
//     stores_moved among them, and every row the arms read carries its marker: the arms and the
//     joins go, and every read of graph_edges and refs.BacklinkCounts equals the read before;
//   - children's release: the same, but the announcement is of the release before this one, which
//     lists every kind of both children and no stores_moved: INFRA-1150's arms go, INFRA-1253's
//     arms and joins stay, since that release writes a document's node with no row values, the log
//     names the announcement and stores_moved as what it lacks, every read equals the read before,
//     and a document written after the boot as that release writes it, its node holding no
//     artifact_id and no created_at, still answers its mentions (including one past the store's
//     address bound) and its attached_to;
//   - roll window: a build of the release before wrote an issue link with no marker and a reply the
//     comment writer marked as INFRA-1149's writer marks every comment: the step's backfill brings
//     the link in, then the arms go, and every read equals the read before;
//   - deleted rows: a build of the release before deleted a marked issue link with its tuple
//     standing: INFRA-1253's arms and joins stay, the server starts and serves, and its log names
//     the tuple;
//   - past bound: a build of the release before wrote a document's reference to a far end past the
//     store's bound, which no writer of this release writes: INFRA-1253's arms stay, with the arm
//     over refs' branch that answers that row, and the log names the row's source and its length;
//   - release before: the release before this tree left rows and its announcement: the arms stay
//     through the root's first boot, a second process of it and a restart of it, so a rollback to
//     that release loses no edge it writes, and go at the first boot of a later build once the
//     root's build is announced;
//   - rolled back: the same, but the fleet rolled back to that release and was announced again
//     before the root's build boots once more: the arms stay, which a rule read from times would
//     delete;
//   - from empty: the boot created the database, so the arms go at once.
//
// Over the released and the empty database the view the step leaves reads a document's uuid, its
// row's created_at and its mentions' dates off the store alone (store.GraphEdgesViewRetired), so
// the tuple arm keeps no read of the artifacts row or the refs row, and the arm over refs answers
// no document's row.
//
// The negative controls, run one at a time in a scratch copy: a step that skips its proof deletes
// the arms of the deleted rows' database, where the deleted link comes back in graph_edges from its
// standing tuple and the reads differ by it; a step that skips its guard deletes them at the root's
// first boot over the release before's database.
func TestGraphEdgesEndsAsOneSelectOverTheTuples(t *testing.T) {
	root := moduleRoot(t)
	t.Run("arms", requireEveryArmHasAnOwner)
	t.Run("catch-up", func(t *testing.T) { requireTheAskCatchUpGone(t, root) })

	rootBuild := cmdtest.BuildStamped(t, "envoy-dispatch-root", oneSelectRootBuild)
	laterBuild := cmdtest.BuildStamped(t, "envoy-dispatch-later", oneSelectLaterBuild)
	t.Run("released", func(t *testing.T) { requireTheReleasedDatabaseLosesTheArms(t, rootBuild) })
	t.Run("roll window", func(t *testing.T) { requireTheRollWindowIsBroughtIn(t, rootBuild) })
	t.Run("deleted rows", func(t *testing.T) { requireDeletedRowsKeepTheArms(t, rootBuild) })
	t.Run("past bound", func(t *testing.T) { requireAReferencePastTheBoundKeepsTheArms(t, rootBuild) })
	t.Run("children's release", func(t *testing.T) { requireTheChildrensReleaseKeepsTheIssueTreeArms(t, rootBuild) })
	t.Run("release before", func(t *testing.T) { requireTheReleaseBeforeKeepsTheArms(t, rootBuild, laterBuild) })
	t.Run("rolled back", func(t *testing.T) { requireARollbackKeepsTheArms(t, rootBuild) })
	t.Run("from empty", func(t *testing.T) { requireAnEmptyDatabaseLosesTheArmsAtOnce(t, rootBuild) })
}

// viewArm is one arm of graph_edges as the catalogue prints it: the relation it reads first, the
// literal from_kind, kind and to_kind it writes ("" for a column), and its text.
type viewArm struct {
	table, fromKind, kind, toKind, text string
}

func (a viewArm) String() string {
	return fmt.Sprintf("the arm over %s (%s %s %s)", a.table, orColumn(a.fromKind), orColumn(a.kind), orColumn(a.toKind))
}

func orColumn(literal string) string {
	if literal == "" {
		return "*"
	}
	return literal
}

var (
	viewArmLiteral = regexp.MustCompile(`'([^']*)'::text AS (from_kind|kind|to_kind)\b`)
	viewArmFrom    = regexp.MustCompile(`\bFROM ([a-z_]+)`)
)

// readViewArms reads graph_edges' definition and splits it into its arms: the catalogue prints
// each top-level UNION ALL on a line of its own, and no arm holds a union of its own at that depth.
func readViewArms(t *testing.T, d *decisionsDatabase) (string, []viewArm) {
	t.Helper()
	var definition string
	d.scan(t, `select pg_get_viewdef('graph_edges'::regclass, true)`, &definition)
	var arms []viewArm
	for _, text := range strings.Split(strings.TrimSuffix(strings.TrimSpace(definition), ";"), "\nUNION ALL\n") {
		arm := viewArm{text: text}
		if from := viewArmFrom.FindStringSubmatch(text); from != nil {
			arm.table = from[1]
		}
		head, _, _ := strings.Cut(text, "FROM")
		for _, literal := range viewArmLiteral.FindAllStringSubmatch(head, -1) {
			switch literal[2] {
			case "from_kind":
				arm.fromKind = literal[1]
			case "kind":
				arm.kind = literal[1]
			case "to_kind":
				arm.toKind = literal[1]
			}
		}
		arms = append(arms, arm)
	}
	return definition, arms
}

// The owners of the arms, by the child whose deletion takes each (c-view-one-select).
const (
	ownerTuples    = "the tuple arm, which stays"
	ownerAsks      = "dispatch://INFRA-1150's"
	ownerIssueTree = "dispatch://INFRA-1253's"
	ownerItems     = "dispatch://INFRA-1152's"
	ownerMoved     = "dispatch://INFRA-1149's, which dispatch://INFRA-1253 deletes"
	ownerRefs      = "the arm over refs: its rows from a comment, a message or an ask dispatch://INFRA-1152's, its rows from an artifact dispatch://INFRA-1253's"
	ownerNone      = ""
)

// armOwner is the child whose deletion takes arm. Every arm over graph_tuples is the tuple arm:
// migration 20261126000200 folded into it the arm over a lens's findings and the arm over the
// artifacts that carry their node, which T7b's second part was given.
func armOwner(arm viewArm) string {
	switch arm.table {
	case "graph_tuples":
		return ownerTuples
	case "refs":
		return ownerRefs
	case "asks", "ask_followers":
		return ownerAsks
	case "issues", "issue_links", "components", "component_depends", "issue_component_members":
		return ownerIssueTree
	case "artifacts":
		if strings.Contains(arm.text, "node_kind IS NULL") {
			return ownerIssueTree
		}
	case "block_refs", "document_blocks":
		return ownerMoved
	case "comments":
		if arm.kind == "anchored_to" || arm.kind == "owned_by" {
			return ownerMoved
		}
		return ownerItems
	case "messages", "comment_blocks", "message_blocks", "ask_blocks", "comment_block_refs", "message_block_refs", "ask_block_refs":
		return ownerItems
	}
	return ownerNone
}

// requireEveryArmHasAnOwner lists the arms of graph_edges at the commit before the root's deletion,
// a database Migrate brought to this head, each with the child that deletes it, and fails on an arm
// on none of the check's lists.
func requireEveryArmHasAnOwner(t *testing.T) {
	d := &decisionsDatabase{url: storetest.Open(t).Pool.Config().ConnString()}
	_, arms := readViewArms(t, d)
	lines := make([]string, 0, len(arms))
	for index, arm := range arms {
		owner := armOwner(arm)
		if owner == ownerNone {
			owner = "no child's"
			t.Errorf("graph_edges holds %s, %s", arm, unlistedArm)
		}
		lines = append(lines, fmt.Sprintf("%2d. %s: %s", index+1, arm, owner))
	}
	t.Logf("graph_edges before the root's deletion has %d arms:\n  %s", len(arms), strings.Join(lines, "\n  "))
}

// declaredWithReceivers is every top-level name the non-test Go files of dir declare, a method as
// its receiver's type and its name ("DecisionStep.catchUp").
func declaredWithReceivers(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := map[string]bool{}
	files := token.NewFileSet()
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, filepath.Join(dir, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, declared := range file.Decls {
			switch declared := declared.(type) {
			case *ast.FuncDecl:
				name := declared.Name.Name
				if declared.Recv != nil && len(declared.Recv.List) == 1 {
					receiver := declared.Recv.List[0].Type
					if star, isStar := receiver.(*ast.StarExpr); isStar {
						receiver = star.X
					}
					if ident, isIdent := receiver.(*ast.Ident); isIdent {
						name = ident.Name + "." + name
					}
				}
				names[name] = true
			case *ast.GenDecl:
				for _, spec := range declared.Specs {
					if spec, isType := spec.(*ast.TypeSpec); isType {
						names[spec.Name.Name] = true
					}
				}
			}
		}
	}
	return names
}

// askCatchUpNames are the declarations of dispatch://INFRA-1150's catch-up from the `ask.*` events:
// the step that ran it, its pass and its pass over one ask, and the events it read.
var askCatchUpNames = []string{"DecisionStep", "DecisionStep.catchUp", "DecisionStep.catchUpAsk", "caughtUpEvent"}

// requireTheAskCatchUpGone fails while the docs package declares any of askCatchUpNames or
// rollWindowAskAttributes, read from its own source, after the scan finds each in a scratch copy
// that plants them.
func requireTheAskCatchUpGone(t *testing.T, root string) {
	planted := declaredWithReceivers(t, scratchCopy(t, "planted.go", "package docs\n\ntype DecisionStep struct{}\n\n"+
		"type caughtUpEvent struct{}\n\nfunc (s *DecisionStep) catchUp() {}\n\nfunc (s *DecisionStep) catchUpAsk() {}\n\n"+
		"func rollWindowAskAttributes() {}\n"))
	for _, name := range append(slices.Clone(askCatchUpNames), "rollWindowAskAttributes") {
		if !planted[name] {
			t.Fatalf("the scan finds no %s in a scratch copy that declares it: it cannot find what it looks for", name)
		}
	}
	declared := declaredWithReceivers(t, filepath.Join(root, "internal", "dispatch", "docs"))
	for _, name := range askCatchUpNames {
		if declared[name] {
			t.Errorf("internal/dispatch/docs declares %s, the code of dispatch://INFRA-1150's catch-up from the ask.* events that the root deletes", name)
		}
	}
	if declared["rollWindowAskAttributes"] {
		t.Errorf("internal/dispatch/docs declares rollWindowAskAttributes, which goes with dispatch://INFRA-1150's catch-up")
	}
}

// armsSeed is what seedReleaseBeforeRows wrote: the ids the reads below name.
type armsSeed struct {
	notes, spec, anchoredAsk, ownedAsk, comment, reply string
}

// seedReleaseBeforeRows writes, as a build of the release before writes them, naming no node, one
// row of every arm a child deletes: an issue with a parent, a wait, a document on an issue, an ask
// anchored on a document and one owned by it, a follower, a component with a parent, a dependency,
// an issue's component, a document's reference, and a comment with a reply.
func seedReleaseBeforeRows(t *testing.T, d *decisionsDatabase) armsSeed {
	t.Helper()
	exec := func(statement string, args ...any) {
		t.Helper()
		if err := d.exec(t, statement, args...); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	scan := func(statement string, args ...any) string {
		t.Helper()
		conn, err := pgx.Connect(context.Background(), d.url)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer conn.Close(context.Background())
		var id string
		if err := conn.QueryRow(context.Background(), statement, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
		return id
	}
	const alice = `'{"kind":"user","id":"alice"}'`
	var seed armsSeed
	exec(`insert into projects (key, name, default_assignee) values ('ARMS', 'Arms', 'alice')`)
	exec(`insert into issues (key, project_key, number, title, parent_key, created_by, rank) values
		('ARMS-1', 'ARMS', 1, 'The parent', null, ` + alice + `, 'U'),
		('ARMS-2', 'ARMS', 2, 'The child', 'ARMS-1', ` + alice + `, 'V'),
		('ARMS-3', 'ARMS', 3, 'The waiting', null, ` + alice + `, 'W'),
		('ARMS-4', 'ARMS', 4, 'The cited', null, ` + alice + `, 'X')`)
	exec(`insert into issue_links (issue_key, kind, target_key) values ('ARMS-3', 'blocked_by', 'ARMS-1')`)
	seed.spec = scan(`insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('ARMS-1', 'ARMS', 'spec', 'spec', 'doc', true, ` + alice + `) returning id::text`)
	seed.notes = scan(`insert into artifacts (project_key, slug, name, kind, created_by)
		values ('ARMS', 'notes', 'notes', 'doc', ` + alice + `) returning id::text`)
	seed.anchoredAsk = scan(`insert into asks (issue_key, author, question, anchor)
		values ('ARMS-1', `+alice+`, 'Anchored?', jsonb_build_object('artifact_id', $1::text)) returning id::text`, seed.notes)
	seed.ownedAsk = scan(`insert into asks (artifact_id, author, question)
		values ($1::uuid, `+alice+`, 'Owned?') returning id::text`, seed.notes)
	exec(`insert into ask_followers (ask_id, session_id) values ($1::uuid, 'session-arms')`, seed.anchoredAsk)
	source := scan(`insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('ARMS', 'sjawhar/arms', 'main', 1, ` + alice + `) returning id::text`)
	snapshot := scan(`insert into architecture_snapshots (project_key, commit, files) values ('ARMS', 'abc123', '{}'::jsonb) returning id::text`)
	exec(`insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
		('ARMS', 'core', 'core', '', null, $1::bigint, $2::bigint),
		('ARMS', 'api', 'api', '', 'core', $1::bigint, $2::bigint)`, snapshot, source)
	exec(`insert into component_depends (project_key, from_id, to_id) values ('ARMS', 'api', 'core')`)
	exec(`insert into issue_components (issue_key, mode) values ('ARMS-1', 'explicit')`)
	exec(`insert into issue_component_members (issue_key, project_key, component_id) values ('ARMS-1', 'ARMS', 'core')`)
	exec(`insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'issue', 'ARMS-4')`, seed.notes)
	seed.comment = scan(`insert into comments (issue_key, author, body) values ('ARMS-1', ` + alice + `, 'A thread') returning id::text`)
	seed.reply = scan(`insert into comments (issue_key, author, body, reply_to)
		values ('ARMS-1', `+alice+`, 'A reply', $1::uuid) returning id::text`, seed.comment)
	return seed
}

// openStore opens a store on d's database for the test's own reads and writes, closed when the
// test ends.
func openStore(t *testing.T, d *decisionsDatabase) *store.Store {
	t.Helper()
	database, err := store.Open(context.Background(), d.url)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	return database
}

// backfillTheStore runs the graph store's backfill over d, as every boot of a children's release
// ran it: every row seeded so far names its node, and the store states its edges.
func backfillTheStore(t *testing.T, d *decisionsDatabase) {
	t.Helper()
	if _, err := graphcensus.Backfill(context.Background(), openStore(t, d), io.Discard); err != nil {
		t.Fatalf("backfill the graph store: %v", err)
	}
}

// announceBuild writes the announcement of commit, as its fleet's announcer writes it, listing
// kinds; no kinds names a build of a release before the column.
func announceBuild(t *testing.T, d *decisionsDatabase, commit string, kinds []string) {
	t.Helper()
	statement, args := `insert into server_build_announcements (commit) values ($1)`, []any{commit}
	if kinds != nil {
		statement, args = `insert into server_build_announcements (commit, tuple_kinds) values ($1, $2)`, []any{commit, kinds}
	}
	if err := d.exec(t, statement, args...); err != nil {
		t.Fatalf("announce %s: %v", commit, err)
	}
}

// viewRow is one row of graph_edges, every column of it.
type viewRow struct {
	fromKind, fromID, kind, toKind, toID string
	createdAt                            time.Time
	sourceSeq                            *int64
}

func (r viewRow) String() string {
	seq := "no source_seq"
	if r.sourceSeq != nil {
		seq = fmt.Sprintf("source_seq %d", *r.sourceSeq)
	}
	return fmt.Sprintf("%s %s -%s-> %s %s at %s, %s", r.fromKind, r.fromID, r.kind, r.toKind, r.toID,
		r.createdAt.UTC().Format(time.RFC3339Nano), seq)
}

// viewReads is what every reader of graph_edges reads over a database: its rows, the count of
// each node's inbound edges the view gives (docstest.ViewBacklinkCounts), and what
// refs.BacklinkCounts answers for the same nodes.
type viewReads struct {
	rows      []string
	targets   map[string][]string
	viewCount map[string]map[string]int
	backlinks map[string]map[string]int
}

// readView reads viewReads over database, for the nodes targets names, or for every node the view
// points at where targets is nil.
func readView(t *testing.T, database *store.Store, targets map[string][]string) viewReads {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_edges`)
	if err != nil {
		t.Fatalf("read graph_edges: %v", err)
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (viewRow, error) {
		var r viewRow
		err := row.Scan(&r.fromKind, &r.fromID, &r.kind, &r.toKind, &r.toID, &r.createdAt, &r.sourceSeq)
		return r, err
	})
	if err != nil {
		t.Fatalf("read graph_edges: %v", err)
	}
	reads := viewReads{targets: targets, viewCount: map[string]map[string]int{}, backlinks: map[string]map[string]int{}}
	for _, row := range read {
		reads.rows = append(reads.rows, row.String())
	}
	slices.Sort(reads.rows)
	kinds := refstest.BacklinkKinds()
	if reads.targets == nil {
		reads.targets = map[string][]string{}
		for _, row := range read {
			if slices.Contains(kinds, row.toKind) && !slices.Contains(reads.targets[row.toKind], row.toID) {
				reads.targets[row.toKind] = append(reads.targets[row.toKind], row.toID)
			}
		}
		for kind := range reads.targets {
			reads.targets[kind] = append(reads.targets[kind], "nothing-points-at-this")
		}
	}
	for kind, ids := range reads.targets {
		reads.viewCount[kind] = docstest.ViewBacklinkCounts(t, database.Pool, "graph_edges", kind, ids, kinds, nil)
		counted, err := refs.BacklinkCounts(ctx, database.Pool, model.Actor{}, kind, ids, nil)
		if err != nil {
			t.Fatalf("count the %s backlinks: %v", kind, err)
		}
		reads.backlinks[kind] = counted
	}
	return reads
}

// requireRowsEqual fails unless after reads from graph_edges what before read: the same rows, each
// row it lost or gained named, and so the same count of each node's inbound edges.
func requireRowsEqual(t *testing.T, before, after viewReads) {
	t.Helper()
	for _, row := range before.rows {
		if !slices.Contains(after.rows, row) {
			t.Errorf("graph_edges lost %s, which it answered before the step", row)
		}
	}
	for _, row := range after.rows {
		if !slices.Contains(before.rows, row) {
			t.Errorf("graph_edges answers %s, which it did not answer before the step", row)
		}
	}
	for kind, counts := range before.viewCount {
		if !mapsEqualInt(after.viewCount[kind], counts) {
			t.Errorf("graph_edges counts %v for the %s nodes after the step; it counted %v before it", after.viewCount[kind], kind, counts)
		}
	}
}

// requireReadsEqual fails unless after, read once the step ran, reads what before read: the same
// rows of graph_edges (requireRowsEqual), and refs.BacklinkCounts answering for every node the
// count the view gave before.
func requireReadsEqual(t *testing.T, before, after viewReads) {
	t.Helper()
	requireRowsEqual(t, before, after)
	for kind, counts := range before.viewCount {
		if !mapsEqualInt(after.backlinks[kind], counts) {
			t.Errorf("refs.BacklinkCounts answers %v for the %s nodes after the step; graph_edges counted %v before it", after.backlinks[kind], kind, counts)
		}
	}
}

func mapsEqualInt(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if count, found := b[key]; !found || count != value {
			return false
		}
	}
	return true
}

// childObjects is each arm and each probe of the tuple arm of one child that graph_edges holds.
func childObjects(arms []viewArm, owner string, probes []string) []string {
	var held []string
	for _, arm := range arms {
		if armOwner(arm) == owner {
			held = append(held, arm.String())
		}
		if armOwner(arm) == ownerTuples || armOwner(arm) == ownerRefs {
			for _, probe := range probes {
				if strings.Contains(arm.text, probe) {
					held = append(held, fmt.Sprintf("%s's %q", arm, probe))
				}
			}
		}
	}
	return held
}

// The probes of the tuple arm each child deletes with its arms; dispatch://INFRA-1253's test of a
// document's marker, in the arm over refs and in the tuple arm's reads of a document's rows; that
// child's reads of the artifacts row and the refs row behind a document's mentions and its
// attached_to (migration 20261126000200); and the arm over refs' branch for a far end past the
// store's bound, which goes with that child's half of the arm.
var (
	asksProbes      = []string{"FROM asks projected", "FROM ask_followers projected"}
	issueTreeProbes = []string{
		"FROM issues projected", "FROM issue_links projected", "FROM components projected",
		"FROM component_depends projected", "FROM issue_component_members projected", "mentioning.node_kind IS NOT NULL",
		"JOIN artifacts mentioning", "FROM refs stated", "JOIN artifacts attached", "octet_length(r.to_id) > 1024",
	}
)

// requireChildrenArms fails unless graph_edges over d holds dispatch://INFRA-1150's and
// dispatch://INFRA-1253's arms and probes where each kept says so, and holds none of them where it
// does not.
func requireChildrenArms(t *testing.T, d *decisionsDatabase, asksKept, issueTreeKept bool) {
	t.Helper()
	_, arms := readViewArms(t, d)
	for _, child := range []struct {
		owner  string
		probes []string
		kept   bool
	}{{ownerAsks, asksProbes, asksKept}, {ownerIssueTree, issueTreeProbes, issueTreeKept}} {
		held := childObjects(arms, child.owner, child.probes)
		switch {
		case child.kept && len(held) == 0:
			t.Errorf("graph_edges holds none of %s arms and probes after the boot, want them kept", child.owner)
		case !child.kept:
			for _, object := range held {
				t.Errorf("graph_edges keeps %s %s after the boot, want it deleted", child.owner, object)
			}
		}
	}
}

// requireOneSelect fails unless graph_edges over d is one select over the current tuples, with the
// tuple arm's class filter, no arm over a source column and no join to a projection row, naming
// each arm and each join it keeps with the child whose deletion takes it.
func requireOneSelect(t *testing.T, d *decisionsDatabase) {
	t.Helper()
	definition, arms := readViewArms(t, d)
	t.Logf("graph_edges after the boot:\n%s", definition)
	tuples := 0
	for _, arm := range arms {
		switch owner := armOwner(arm); owner {
		case ownerTuples:
			tuples++
			if !strings.Contains(arm.text, "END AS projects") {
				t.Errorf("the tuple arm holds no class filter, its `projects` case")
			}
			for _, join := range []struct{ text, owner string }{
				{"FROM document_blocks projected", infra1149Part},
				{"FROM block_refs projected", infra1149Part},
				{"FROM comments projected", infra1149Part},
				{"FROM asks projected", ownerAsks + " join, which the deletion takes"},
				{"FROM ask_followers projected", ownerAsks + " join, which the deletion takes"},
				{"FROM issues projected", ownerIssueTree + " join, which the deletion takes"},
				{"FROM issue_links projected", ownerIssueTree + " join, which the deletion takes"},
				{"FROM components projected", ownerIssueTree + " join, which the deletion takes"},
				{"FROM component_depends projected", ownerIssueTree + " join, which the deletion takes"},
				{"FROM issue_component_members projected", ownerIssueTree + " join, which the deletion takes"},
				{"JOIN artifacts mentioning", ownerIssueTree + " read of a document's mentions through its artifacts row, which the deletion takes"},
				{"FROM refs stated", ownerIssueTree + " read of a document's mentions' dates off its refs rows, which the deletion takes"},
				{"JOIN artifacts attached", ownerIssueTree + " read of a document's attached_to through its artifacts row, which the deletion takes"},
			} {
				if strings.Contains(arm.text, join.text) {
					t.Errorf("the tuple arm keeps its join %q: %s", join.text, join.owner)
				}
			}
		case ownerItems:
			t.Errorf("graph_edges keeps %s: %s", arm, infra1152Part)
		case ownerMoved:
			t.Errorf("graph_edges keeps %s: %s", arm, infra1149Part)
		case ownerRefs:
			t.Errorf("graph_edges keeps %s: its item half is %s", arm, infra1152Part)
			if strings.Contains(arm.text, "octet_length") || strings.Contains(arm.text, "mentioning") {
				t.Errorf("the arm over refs keeps a test of a document's rows: %s join, which the deletion takes:\n%s", ownerIssueTree, arm.text)
			}
		case ownerNone:
			t.Errorf("graph_edges keeps %s, %s", arm, unlistedArm)
		default:
			t.Errorf("graph_edges keeps %s, %s, after the boot", arm, owner)
		}
	}
	if tuples != 1 {
		t.Errorf("graph_edges holds %d tuple arms, want the one", tuples)
	}
}

// requireLogged fails unless output holds every one of lines.
func requireLogged(t *testing.T, output string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(output, line) {
			t.Errorf("the boot's log holds no %q:\n%s", line, output)
		}
	}
}

// deletedLine is the line the step logs where an entry deleted its objects.
func deletedLine(retirement startup.Retirement) string {
	return `msg="dispatch: the boot's retirement deleted its objects" retirement="` + retirement.Name + `"`
}

// releasedDatabase is a database from storetest.Open, every row of seedReleaseBeforeRows stated on
// its node by the children's backfill, as the children's release left it.
func releasedDatabase(t *testing.T) (*decisionsDatabase, armsSeed) {
	t.Helper()
	d := &decisionsDatabase{url: storetest.Open(t).Pool.Config().ConnString()}
	seed := seedReleaseBeforeRows(t, d)
	backfillTheStore(t, d)
	return d, seed
}

// startBuild starts binary on d, beside every process already serving d, under the one signing
// key a fleet's processes share, and waits up to within for it to serve.
func startBuild(t *testing.T, d *decisionsDatabase, binary string, within time.Duration) *dispatchProcess {
	t.Helper()
	process := startDispatchProcessWith(t, d.url, dispatchOptions{Binary: binary, SigningKey: multiprocessSigningKey})
	if err := process.healthyWithin(within); err != nil {
		t.Fatal(err)
	}
	return process
}

// stopDeployedProcess stops process as a deploy does.
func stopDeployedProcess(t *testing.T, process *dispatchProcess) {
	t.Helper()
	process.Terminate(t)
	process.WaitExit(t, "SIGTERM")
}

// settleBoot waits until the boot of process has done its own work on seed's rows: its run of the
// graph store's backfill has ended, and every seeded comment and ask holds the blocks the server
// indexes of it. A read after it reads what every later boot of the build leaves, but for the
// step's own deletion.
func settleBoot(t *testing.T, d *decisionsDatabase, process *dispatchProcess, seed armsSeed) {
	t.Helper()
	process.WaitForOutput(t, bootBackfillDoneLine)
	deadline := time.Now().Add(60 * time.Second)
	for {
		conn, err := pgx.Connect(context.Background(), d.url)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		var indexed bool
		err = conn.QueryRow(context.Background(), `
			select (select count(distinct comment_id) from comment_blocks where comment_id::text = any($1)) = 2
			   and (select count(distinct ask_id) from ask_blocks where ask_id::text = any($2)) = 2`,
			[]string{seed.comment, seed.reply}, []string{seed.anchoredAsk, seed.ownedAsk}).Scan(&indexed)
		conn.Close(context.Background())
		if err != nil {
			t.Fatalf("read whether the seeded items hold their blocks: %v", err)
		}
		if indexed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the seeded comments and asks hold no blocks 60 s after the boot:\n%s", process.Output.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// warmUp boots the root's build on d with no announcement of another build, so the step keeps
// every arm, lets the boot settle (settleBoot) and stops it: what any boot of the build does to
// seed's rows is done before the reads the step is held to.
func warmUp(t *testing.T, d *decisionsDatabase, rootBuild string, seed armsSeed) {
	t.Helper()
	process := startBuild(t, d, rootBuild, 30*time.Second)
	requireChildrenArms(t, d, true, true)
	settleBoot(t, d, process, seed)
	stopDeployedProcess(t, process)
}

// requireServes fails unless process answers the read of ARMS-3. The seeded asks are ARMS-1's and
// were written as SQL rows with no ask.opened event, which the issue read refuses ("has no
// event"), so the read is of an issue that holds none of them.
func requireServes(t *testing.T, process *dispatchProcess) {
	t.Helper()
	if answer := process.send(t, http.MethodGet, "/api/v1/issues/ARMS-3", "", asUser(dispatchTestLogin)); answer.status != http.StatusOK {
		t.Errorf("GET /api/v1/issues/ARMS-3 = %s, want 200:\n%s", answer, process.Output.String())
	}
}

// requireGraphEdgeAnswered fails unless graph_edges over database answers edge, the head of a row's
// rendering.
func requireGraphEdgeAnswered(t *testing.T, database *store.Store, edge string) {
	t.Helper()
	if rows := readView(t, database, map[string][]string{}).rows; !slices.ContainsFunc(rows, func(row string) bool {
		return strings.HasPrefix(row, edge+" ")
	}) {
		t.Errorf("graph_edges does not answer %s:\n%s", edge, strings.Join(rows, "\n"))
	}
}

func requireTheReleasedDatabaseLosesTheArms(t *testing.T, rootBuild string) {
	d, seed := releasedDatabase(t)
	warmUp(t, d, rootBuild, seed)
	announceBuild(t, d, oneSelectChildrenRelease, refs.TupleKinds())
	database := openStore(t, d)
	before := readView(t, database, nil)
	d.process = startBuild(t, d, rootBuild, 30*time.Second)
	requireLogged(t, d.process.Output.String(), deletedLine(startup.AsksArmsRetirement), deletedLine(startup.IssueTreeArmsRetirement))
	requireChildrenArms(t, d, false, false)
	after := readView(t, database, before.targets)
	requireReadsEqual(t, before, after)
	for kind, counts := range before.backlinks {
		if !mapsEqualInt(after.backlinks[kind], counts) {
			t.Errorf("refs.BacklinkCounts answers %v for the %s nodes after the step, %v before it", after.backlinks[kind], kind, counts)
		}
	}
	requireServes(t, d.process)
	requireOneSelect(t, d)

	stopDeployedProcess(t, d.process)
	again := startBuild(t, d, rootBuild, 30*time.Second)
	if strings.Contains(again.Output.String(), "the boot's retirement deleted its objects") {
		t.Errorf("a second boot deleted again:\n%s", again.Output.String())
	}
	requireReadsEqual(t, before, readView(t, database, before.targets))
}

func requireTheRollWindowIsBroughtIn(t *testing.T, rootBuild string) {
	d, seed := releasedDatabase(t)
	warmUp(t, d, rootBuild, seed)
	// The roll window: a build of the release before waits ARMS-4 on ARMS-2, naming no node. The
	// reply of the seed carries comments.node_kind, which INFRA-1149's writer sets on every
	// comment, and no marker of INFRA-1152's, which this head has none of.
	if err := d.exec(t, `insert into issue_links (issue_key, kind, target_key) values ('ARMS-4', 'blocked_by', 'ARMS-2')`); err != nil {
		t.Fatalf("write the roll window's wait: %v", err)
	}
	var replyMarked bool
	d.scan(t, `select node_kind is not null from comments where id = '`+seed.reply+`'`, &replyMarked)
	if !replyMarked {
		t.Fatalf("the reply %s carries no comments.node_kind, which INFRA-1149's writer sets on every comment", seed.reply)
	}
	t.Errorf("the roll window's reply %s is brought in by INFRA-1152's backfill and marker: %s", seed.reply, infra1152Part)
	announceBuild(t, d, oneSelectChildrenRelease, refs.TupleKinds())
	database := openStore(t, d)
	before := readView(t, database, nil)
	requireGraphEdgeAnswered(t, database, "issue ARMS-4 -blocked_by-> issue ARMS-2")
	d.process = startBuild(t, d, rootBuild, 30*time.Second)
	requireLogged(t, d.process.Output.String(), deletedLine(startup.AsksArmsRetirement), deletedLine(startup.IssueTreeArmsRetirement))
	var linkMarked bool
	d.scan(t, `select node_kind is not null from issue_links where issue_key = 'ARMS-4' and target_key = 'ARMS-2'`, &linkMarked)
	if !linkMarked {
		t.Errorf("the roll window's wait names no node after the boot: the step's backfill did not bring it in")
	}
	requireChildrenArms(t, d, false, false)
	requireReadsEqual(t, before, readView(t, database, before.targets))
}

func requireDeletedRowsKeepTheArms(t *testing.T, rootBuild string) {
	d, seed := releasedDatabase(t)
	warmUp(t, d, rootBuild, seed)
	// A build of the release before deletes the marked wait of ARMS-3 on ARMS-1 and appends no
	// retraction, so its needs tuple stands.
	if err := d.exec(t, `delete from issue_links where issue_key = 'ARMS-3' and target_key = 'ARMS-1'`); err != nil {
		t.Fatalf("delete the marked wait: %v", err)
	}
	t.Errorf("the marked reply %s deleted with its tuple standing holds INFRA-1152's arms: %s", seed.reply, infra1152Part)
	announceBuild(t, d, oneSelectChildrenRelease, refs.TupleKinds())
	database := openStore(t, d)
	before := readView(t, database, nil)
	d.process = startBuild(t, d, rootBuild, 30*time.Second)
	output := d.process.Output.String()
	requireLogged(t, output, `unprojected_tuple="issue:ARMS-3 -needs-> issue:ARMS-1"`, deletedLine(startup.AsksArmsRetirement))
	if strings.Contains(output, deletedLine(startup.IssueTreeArmsRetirement)) {
		t.Errorf("the step deleted INFRA-1253's arms over a wait deleted with its tuple standing:\n%s", output)
	}
	requireChildrenArms(t, d, false, true)
	requireReadsEqual(t, before, readView(t, database, before.targets))
	requireServes(t, d.process)
}

// pastBoundAddress is a document's address of graphstore.MaxAddressBytes and one byte more, as a
// build of the release before could cite it: the far end no writer of this release writes.
var pastBoundAddress = "ARMS-1/" + strings.Repeat("p", 1025-len("ARMS-1/"))

func requireAReferencePastTheBoundKeepsTheArms(t *testing.T, rootBuild string) {
	d, seed := releasedDatabase(t)
	warmUp(t, d, rootBuild, seed)
	// A build of the release before writes the notes' reference to an address past the bound.
	if err := d.exec(t, `insert into refs (from_kind, from_id, to_kind, to_id, created_at) values ('artifact', $1, 'artifact', $2, now())`,
		seed.notes, pastBoundAddress); err != nil {
		t.Fatalf("write the reference past the bound: %v", err)
	}
	announceBuild(t, d, oneSelectChildrenRelease, refs.TupleKinds())
	database := openStore(t, d)
	before := readView(t, database, nil)
	requireGraphEdgeAnswered(t, database, "artifact "+seed.notes+" -mentions-> artifact "+pastBoundAddress)
	d.process = startBuild(t, d, rootBuild, 30*time.Second)
	output := d.process.Output.String()
	requireLogged(t, output, "past_bound_reference=artifact:"+seed.notes, "length=1025", deletedLine(startup.AsksArmsRetirement))
	if strings.Contains(output, deletedLine(startup.IssueTreeArmsRetirement)) {
		t.Errorf("the step deleted INFRA-1253's arms, the arm over refs' branch past the bound among them, over a reference past the bound:\n%s", output)
	}
	requireChildrenArms(t, d, false, true)
	requireReadsEqual(t, before, readView(t, database, before.targets))
	requireServes(t, d.process)
}

// requireTheChildrensReleaseKeepsTheIssueTreeArms boots the root's build over the released
// database announced by the release before this one, dispatch://INFRA-1253's, which lists every
// kind this build lists but stores_moved: the step deletes INFRA-1150's arms and keeps
// INFRA-1253's, whose guard is stores_moved (startup.StoresMovedKinds), and logs why. After the
// boot a document written as that release writes it still answers its edges (writeAsTheIssueRoom).
func requireTheChildrensReleaseKeepsTheIssueTreeArms(t *testing.T, rootBuild string) {
	d, seed := releasedDatabase(t)
	warmUp(t, d, rootBuild, seed)
	announceBuild(t, d, oneSelectChildrenRelease, slices.DeleteFunc(slices.Clone(refs.TupleKinds()), func(kind string) bool {
		return kind == processes.CapabilityStoresMoved
	}))
	database := openStore(t, d)
	before := readView(t, database, nil)
	d.process = startBuild(t, d, rootBuild, 30*time.Second)
	output := d.process.Output.String()
	requireLogged(t, output, deletedLine(startup.AsksArmsRetirement))
	if strings.Contains(output, deletedLine(startup.IssueTreeArmsRetirement)) {
		t.Errorf("the step deleted INFRA-1253's arms under an announcement of the release before, which lists no %s:\n%s",
			processes.CapabilityStoresMoved, output)
	}
	if !slices.ContainsFunc(strings.Split(output, "\n"), func(line string) bool {
		return strings.Contains(line, "kept its objects") && strings.Contains(line, `retirement="`+startup.IssueTreeArmsRetirement.Name+`"`) &&
			strings.Contains(line, "announcement_commit="+oneSelectChildrenRelease) && strings.Contains(line, "missing=["+processes.CapabilityStoresMoved+"]")
	}) {
		t.Errorf("the log names no announcement %s lacking %s for the kept %s:\n%s", oneSelectChildrenRelease,
			processes.CapabilityStoresMoved, startup.IssueTreeArmsRetirement.Name, output)
	}
	requireChildrenArms(t, d, false, true)
	requireReadsEqual(t, before, readView(t, database, before.targets))
	requireServes(t, d.process)
	// Finish the boot's backfill before writing the document, so it cannot add row values
	// to the node between the write and the read.
	d.process.WaitForOutput(t, bootBackfillDoneLine)
	document := writeAsTheIssueRoom(t, database)
	requireGraphEdgeAnswered(t, database, "artifact "+document+" -mentions-> issue ARMS-4")
	requireGraphEdgeAnswered(t, database, "artifact "+document+" -mentions-> artifact "+pastBoundAddress)
	requireGraphEdgeAnswered(t, database, "artifact "+document+" -attached_to-> issue ARMS-3")
}

// writeAsTheIssueRoom writes, as a build of dispatch://INFRA-1253's release (810aa47bac) uploads it,
// a document of ARMS-3 that mentions ARMS-4 and an address past the store's bound. It returns the
// document's UUID. The node has no row values. It has part_of and an in-bound mentions tuple,
// but only a refs row for the mention past the bound, which the store does not take as a tuple.
func writeAsTheIssueRoom(t *testing.T, database *store.Store) string {
	t.Helper()
	ctx := context.Background()
	const refKey = "ARMS-3/rollback-md"
	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Now()
	var id string
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, []graphstore.Node{graphstore.ArtifactNode(refKey, "ARMS", "", alice, at)}, []graphstore.Tuple{
			graphstore.ArtifactPartOf(refKey, "ARMS-3", alice, at),
			{FromKind: graphstore.KindArtifact, FromID: refKey, Kind: "mentions", ToKind: graphstore.KindIssue, ToID: "ARMS-4",
				At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true},
		}); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, node_kind, node_address)
			values ('ARMS-3', 'ARMS', 'rollback-md', 'rollback.md', 'doc', false, '{"kind":"user","id":"alice"}', 'artifact', $1)
			returning id::text`, refKey).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into refs (from_kind, from_id, to_kind, to_id)
			values ('artifact', $1, 'issue', 'ARMS-4'), ('artifact', $1, 'artifact', $2)`, id, pastBoundAddress)
		return err
	}); err != nil {
		t.Fatalf("write the document as the issue-room release writes it: %v", err)
	}
	return id
}

// releaseBeforeDatabase is an empty database migrated through the release before this tree, with
// seedReleaseBeforeRows written as that release wrote them and its announcement, then migrated to
// this head, as a deploy of the root's build would.
func releaseBeforeDatabase(t *testing.T) (*decisionsDatabase, *store.Store, armsSeed) {
	t.Helper()
	d := &decisionsDatabase{url: storetest.Empty(t)}
	database := openStore(t, d)
	if err := database.MigrateThrough(context.Background(), releaseBeforeTheTree); err != nil {
		t.Fatalf("migrate through the release before this tree: %v", err)
	}
	seed := seedReleaseBeforeRows(t, d)
	announceBuild(t, d, oneSelectBeforeTheTree, nil)
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate to this head: %v", err)
	}
	return d, database, seed
}

// writeAsTheReleaseBefore writes, as a build of the release before writes it during a rollback,
// naming no node, a wait of from on ARMS-1, and fails unless graph_edges answers it: a rollback to
// that release loses no edge it writes.
func writeAsTheReleaseBefore(t *testing.T, d *decisionsDatabase, database *store.Store, from string) {
	t.Helper()
	if err := d.exec(t, `insert into issue_links (issue_key, kind, target_key) values ($1, 'blocked_by', 'ARMS-1')`, from); err != nil {
		t.Fatalf("write the release before's wait of %s: %v", from, err)
	}
	requireGraphEdgeAnswered(t, database, "issue "+from+" -blocked_by-> issue ARMS-1")
}

// waitForAnnouncement waits up to 30 s for the announcer of the processes serving commit to write
// its announcement.
func waitForAnnouncement(t *testing.T, d *decisionsDatabase, commit string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var announced bool
		d.scan(t, `select exists (select 1 from server_build_announcements where commit = '`+commit+`')`, &announced)
		if announced {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no announcement of %s 30 s after its processes served", commit)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// requireKeptFor fails unless process logged that each child's arms stay because the build a
// rollback reaches, commit's, lists none of that child's kinds.
func requireKeptFor(t *testing.T, process *dispatchProcess, commit string) {
	t.Helper()
	output := process.Output.String()
	for _, retirement := range []startup.Retirement{startup.AsksArmsRetirement, startup.IssueTreeArmsRetirement} {
		if !slices.ContainsFunc(strings.Split(output, "\n"), func(line string) bool {
			return strings.Contains(line, "kept its objects") && strings.Contains(line, `retirement="`+retirement.Name+`"`) &&
				strings.Contains(line, "announcement_commit="+commit)
		}) {
			t.Errorf("the log names no announcement %s for the kept %s:\n%s", commit, retirement.Name, output)
		}
	}
}

func requireTheReleaseBeforeKeepsTheArms(t *testing.T, rootBuild, laterBuild string) {
	d, database, seed := releaseBeforeDatabase(t)
	first := startBuild(t, d, rootBuild, 30*time.Second)
	requireKeptFor(t, first, oneSelectBeforeTheTree)
	requireChildrenArms(t, d, true, true)
	settleBoot(t, d, first, seed)
	writeAsTheReleaseBefore(t, d, database, "ARMS-2")
	requireCountsAgree(t, readView(t, database, nil))
	second := startBuild(t, d, rootBuild, 30*time.Second)
	requireKeptFor(t, second, oneSelectBeforeTheTree)
	requireChildrenArms(t, d, true, true)
	stopDeployedProcess(t, second)
	restarted := startBuild(t, d, rootBuild, 30*time.Second)
	requireKeptFor(t, restarted, oneSelectBeforeTheTree)
	requireChildrenArms(t, d, true, true)
	writeAsTheReleaseBefore(t, d, database, "ARMS-4")

	waitForAnnouncement(t, d, oneSelectRootBuild)
	stopDeployedProcess(t, first)
	stopDeployedProcess(t, restarted)
	before := readView(t, database, nil)
	later := startBuild(t, d, laterBuild, 30*time.Second)
	requireLogged(t, later.Output.String(), deletedLine(startup.AsksArmsRetirement), deletedLine(startup.IssueTreeArmsRetirement),
		"announcement_commit="+oneSelectRootBuild)
	requireChildrenArms(t, d, false, false)
	requireReadsEqual(t, before, readView(t, database, before.targets))
	requireServes(t, later)
}

func requireARollbackKeepsTheArms(t *testing.T, rootBuild string) {
	d, database, seed := releaseBeforeDatabase(t)
	first := startBuild(t, d, rootBuild, 30*time.Second)
	settleBoot(t, d, first, seed)
	waitForAnnouncement(t, d, oneSelectRootBuild)
	stopDeployedProcess(t, first)
	// The fleet rolled back to the release before this tree, whose announcer names its commit again.
	announceBuild(t, d, oneSelectBeforeTheTree, nil)
	again := startBuild(t, d, rootBuild, 30*time.Second)
	requireKeptFor(t, again, oneSelectBeforeTheTree)
	requireChildrenArms(t, d, true, true)
	writeAsTheReleaseBefore(t, d, database, "ARMS-2")
	requireCountsAgree(t, readView(t, database, nil))
}

func requireAnEmptyDatabaseLosesTheArmsAtOnce(t *testing.T, rootBuild string) {
	d := &decisionsDatabase{url: storetest.Empty(t)}
	// The boot migrates the whole set first, so it is given longer to serve than a boot of a
	// migrated database.
	d.process = startBuild(t, d, rootBuild, 2*time.Minute)
	requireLogged(t, d.process.Output.String(), deletedLine(startup.AsksArmsRetirement)+" created_from_empty=true",
		deletedLine(startup.IssueTreeArmsRetirement)+" created_from_empty=true")
	requireChildrenArms(t, d, false, false)
	requireOneSelect(t, d)
	// Rows a build of the release before writes, naming no node, which neither the view as the step
	// left it nor the count answers.
	seedReleaseBeforeRows(t, d)
	requireCountsAgree(t, readView(t, openStore(t, d), nil))
}

// requireCountsAgree fails unless refs.BacklinkCounts answers, for every node graph_edges points
// at, the count the view gives in the same reads.
func requireCountsAgree(t *testing.T, reads viewReads) {
	t.Helper()
	for kind, counts := range reads.viewCount {
		if !mapsEqualInt(reads.backlinks[kind], counts) {
			t.Errorf("refs.BacklinkCounts answers %v for the %s nodes; graph_edges counts %v", reads.backlinks[kind], kind, counts)
		}
	}
}

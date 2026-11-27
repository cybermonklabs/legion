package refs

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// withTestOnlyKinds installs a registry entry for each of kinds the real registry does not hold,
// for the length of the test, as TestAnOlderBuildSkipsItemBlockRows swaps nodeRegistry: each with
// the project scope and a loader over graph_nodes (loadStoreNodes), which is how the change that
// registers the node-types spec's kinds registers lens_kind and constraint (the private-projects
// plan's contract on those kinds). It adds nothing for a kind the registry holds, so once that
// change is in the build these tests read its entries, and it logs each entry it installs, so a
// run says which kinds it could not read from the real registry.
func withTestOnlyKinds(t *testing.T, kinds ...string) {
	t.Helper()
	registry, resolved := nodeRegistry, nodeKinds
	extended := maps.Clone(registry)
	for _, kind := range kinds {
		if _, held := registry[kind]; held {
			continue
		}
		extended[kind] = resolvedKind(loadStoreNodes, nodeVisibility{scope: projectScope})
		t.Logf("test-only registry entry: %s (no change in this build registers it)", kind)
	}
	nodeRegistry = extended
	nodeKinds = resolvedNodeKinds()
	t.Cleanup(func() { nodeRegistry, nodeKinds = registry, resolved })
}

// catalogueSeeder is the writer the node-types work's seed migration writes the catalogue as: a
// system actor, at the sentinel event a migration's writes carry.
var catalogueSeeder = model.Actor{Kind: "system", ID: "lens-kind-seed"}

// seedLensKind writes a lens_kind node of slug and the constraint node its primary source is, with
// the constrained_by tuple between them, as the node-types work's seed writes them: neither names
// a project or parties. project, when not empty, writes the constraint in that project instead.
func seedLensKind(t *testing.T, tx pgx.Tx, slug, constraint, project string) {
	t.Helper()
	at := time.Now()
	nodes := []graphstore.Node{
		{Kind: "lens_kind", ID: slug, MadeAt: at, MadeBy: catalogueSeeder},
		{Kind: "constraint", ID: constraint, Project: project, MadeAt: at, MadeBy: catalogueSeeder},
	}
	writeSeedNodes(t, tx, nodes, []graphstore.Tuple{constrainedBy(slug, constraint, true)})
}

// constrainedBy is the constrained_by tuple from lens_kind slug to constraint, or its retraction.
func constrainedBy(slug, constraint string, asserted bool) graphstore.Tuple {
	return graphstore.Tuple{FromKind: "lens_kind", FromID: slug, Kind: "constrained_by", ToKind: "constraint", ToID: constraint,
		At: time.Now(), Who: catalogueSeeder, Asserted: asserted}
}

// catalogueRead is what refs.Node answers one reader for one node: what a read of the node's
// address answers, which a hidden node must answer exactly as an address nobody holds.
type catalogueRead struct {
	Node  model.GraphNode
	Found bool
	Err   string
}

func readCatalogueNode(t *testing.T, tx pgx.Tx, reader model.Actor, kind, id string) catalogueRead {
	t.Helper()
	node, found, err := Node(context.Background(), tx, reader, kind, id, true)
	read := catalogueRead{Node: node, Found: found}
	if err != nil {
		read.Err = err.Error()
	}
	return read
}

// TestACatalogueNodeIsVisibleToEveryViewer: a lens_kind node and the constraint its
// constrained_by tuple reaches, both of no project and written as the node-types work's seed
// writes them, are read through refs by every caller of verification v16 - bob (a restricted
// member), carol's token that lists PRIV, alice, the shared token and the rest - and by the closed
// viewer, and each read finds both. Spec v34: a node the system seeds for every project, which
// holds no project's content, is visible to every viewer.
func TestACatalogueNodeIsVisibleToEveryViewer(t *testing.T) {
	withTestOnlyKinds(t, "lens_kind", "constraint")
	tx := beginSeed(t)
	seedVerificationProjects(t, tx)
	slug, constraint := "contract-gap", uuid.NewString()
	seedLensKind(t, tx, slug, constraint, "")

	for _, caller := range seededCallers() {
		reader := caller.reader(t, tx)
		for _, node := range []struct{ kind, id string }{{"lens_kind", slug}, {"constraint", constraint}} {
			if read := readCatalogueNode(t, tx, reader, node.kind, node.id); !read.Found || read.Err != "" {
				t.Errorf("%s reads the catalogue node %s/%s as missing (%+v); a catalogue node is visible to every viewer",
					caller.name, node.kind, node.id, read)
			}
		}
	}
}

// TestAProjectConstraintOfAHiddenProjectStaysHidden: a constraint is catalogue only when it names
// no project and a lens_kind node's constrained_by tuple reaches it. A constraint of PRIV that a
// PRIV requirement points at, and a second constraint of PRIV that a lens_kind node also points
// at, are read by alice, the shared token and carol's token that lists PUB - and every other
// caller that does not see PRIV - exactly as an address nobody holds, and by bob, on PRIV's list,
// and every other caller that sees PRIV, as themselves. A constraint of no project that no
// lens_kind reaches is no catalogue node either: it is read as a node of a project kind that names
// no project, which nobody but the server reads while a project is private.
func TestAProjectConstraintOfAHiddenProjectStaysHidden(t *testing.T) {
	withTestOnlyKinds(t, "lens_kind", "constraint")
	tx := beginSeed(t)
	seedVerificationProjects(t, tx)
	fromRequirement, fromLens, loose := uuid.NewString(), uuid.NewString(), uuid.NewString()
	requirement := "PRIV/spec-md#r1"
	at := time.Now()
	writeSeedNodes(t, tx, []graphstore.Node{
		{Kind: "constraint", ID: fromRequirement, Project: seededPrivate, MadeAt: at},
		{Kind: "block", ID: requirement, Project: seededPrivate, MadeAt: at},
		{Kind: "constraint", ID: loose, MadeAt: at},
	}, []graphstore.Tuple{{FromKind: "block", FromID: requirement, Kind: "came_from", ToKind: "constraint", ToID: fromRequirement,
		At: at, Who: model.Actor{Kind: "user", ID: seededCarol}, Asserted: true}})
	seedLensKind(t, tx, "priv-lens", fromLens, seededPrivate)

	carol := seededCarol
	callers := append(seededCallers(), seededCaller{name: "carol's token that lists PUB", actor: model.Actor{Kind: "session", Owner: &carol,
		Credential: &model.Credential{TokenID: "carol-pub", Projects: []string{seededPublic}}}})
	seesPrivate := map[string]bool{"bob": true, "carol": true, "carol's token that lists PRIV": true, "bob's token that lists PRIV": true, "the server": true}
	for _, caller := range callers {
		reader := caller.reader(t, tx)
		missing := readCatalogueNode(t, tx, reader, "constraint", uuid.NewString())
		for _, constraint := range []struct {
			name, id string
			sees     bool
		}{
			{"the constraint a PRIV requirement points at", fromRequirement, seesPrivate[caller.name]},
			{"the PRIV constraint a lens_kind node points at", fromLens, seesPrivate[caller.name]},
			{"the constraint of no project no lens_kind reaches", loose, caller.name == "the server"},
		} {
			read := readCatalogueNode(t, tx, reader, "constraint", constraint.id)
			switch {
			case constraint.sees && !read.Found:
				t.Errorf("%s reads %s %s as missing (%+v)", caller.name, constraint.name, constraint.id, read)
			case !constraint.sees && !reflect.DeepEqual(read, missing):
				t.Errorf("%s reads %s %s as %+v; want exactly what an address nobody holds answers, %+v",
					caller.name, constraint.name, constraint.id, read, missing)
			}
		}
	}
}

// TestEveryCatalogueNodeHoldsNoProject: every node the store holds that a catalogue arm matches by
// its kind and its tuple - every node of an entry with no Via, and every node of an entry with
// one that a current tuple of its relation reaches from its from-kind - names no project and no
// parties, and every node of an entry with no Via was written by a migration (the sentinel event
// and a system actor). The arm itself also requires no project, so a seed that stated one would
// leave the node to its project's rule; this names such a node instead of hiding it. The scan is
// shown able to fail first: a constraint planted in PRIV and reached by a constrained_by tuple is
// named, and is then taken out again before the seed's own nodes are checked.
func TestEveryCatalogueNodeHoldsNoProject(t *testing.T) {
	withTestOnlyKinds(t, "lens_kind", "constraint")
	tx := beginSeed(t)
	seedVerificationProjects(t, tx)
	seedLensKind(t, tx, "contract-gap", uuid.NewString(), "")
	seedLensKind(t, tx, "missing-test", uuid.NewString(), "")

	planted := uuid.NewString()
	if _, err := tx.Exec(context.Background(), "savepoint planted"); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	seedLensKind(t, tx, "planted", planted, "PRIV")
	if _, violations := catalogueViolations(t, tx); !slices.ContainsFunc(violations, func(violation string) bool {
		return strings.Contains(violation, "constraint/"+planted+" names project \"PRIV\"")
	}) {
		t.Fatalf("the scan does not name the constraint planted in PRIV, so it can name none:\n%s", strings.Join(violations, "\n"))
	}
	if _, err := tx.Exec(context.Background(), "rollback to savepoint planted"); err != nil {
		t.Fatalf("take the planted constraint out: %v", err)
	}

	checked, violations := catalogueViolations(t, tx)
	for _, violation := range violations {
		t.Error(violation)
	}
	if checked == 0 {
		t.Fatal("no catalogue node was read, so none was checked")
	}
	t.Logf("checked %d catalogue nodes", checked)
}

// catalogueViolations reads, through tx, every node a catalogue arm matches by its kind and its
// tuple, and answers how many it read and a line for each that names a project or parties, or,
// for an entry with no Via, was not written by a migration.
func catalogueViolations(t *testing.T, tx pgx.Tx) (int, []string) {
	t.Helper()
	checked := 0
	var violations []string
	for _, entry := range catalogue {
		statement := `select n.kind, n.id, n.project_key, n.parties, n.source_event, coalesce(n.made_by->>'kind', '')
			from graph_nodes n where n.kind = $1`
		if entry.Via != nil {
			statement += ` and exists (select 1 from (` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id`, `t.asserted`,
				`t.to_kind = n.kind and t.to_id = n.id and t.kind = $2 and t.from_kind = $3`) + `) via where via.asserted)`
		} else {
			statement += ` and $2::text is null and $3::text is null`
		}
		var relation, fromKind *string
		if entry.Via != nil {
			relation, fromKind = &entry.Via.Relation, &entry.Via.FromKind
		}
		rows, err := tx.Query(context.Background(), statement, entry.Kind, relation, fromKind)
		if err != nil {
			t.Fatalf("read the nodes the %s arm matches: %v", entry.Kind, err)
		}
		for rows.Next() {
			var kind, id, writer string
			var project *string
			var parties []string
			var event int64
			if err := rows.Scan(&kind, &id, &project, &parties, &event, &writer); err != nil {
				t.Fatalf("scan a catalogue node: %v", err)
			}
			checked++
			if project != nil || parties != nil {
				violations = append(violations, fmt.Sprintf("the catalogue node %s/%s names project %v and parties %v: a catalogue node holds no project's content (%s)",
					kind, id, deref(project), parties, entry.Source))
			}
			if entry.Via == nil && (event != 0 || writer != "system") {
				violations = append(violations, fmt.Sprintf("the catalogue node %s/%s was written at event %d by a %q actor: every node of the kind is seeded by a migration (%s)",
					kind, id, event, writer, entry.Source))
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("read the nodes the %s arm matches: %v", entry.Kind, err)
		}
	}
	return checked, violations
}

func deref(value *string) string {
	if value == nil {
		return "<none>"
	}
	return fmt.Sprintf("%q", *value)
}

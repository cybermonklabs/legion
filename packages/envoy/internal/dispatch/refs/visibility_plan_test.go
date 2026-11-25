package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// scopeIndexRelations are the tables a scope's arms read, each of which an arm reaches through an
// index of its own: with sequential scans off, a sequential read of one of them is an arm with no
// index path.
var scopeIndexRelations = []string{
	"session_projects", "project_access", "access_group_members", "people", "projects", "graph_nodes", "graph_tuples",
}

// nearEndIndexes are graph_tuples' indexes led by a tuple's near end. With only a dozen
// constrained_by tuples, a correct catalogue arm may plan through one of them (graph_tuples_current,
// or after #3178's spec v50 R2 change a bitmap of graph_tuples_as_of_out) as readily as through
// its far end's index, so the catalogue read's index half is planned with them dropped, in the
// plan's own transaction, which rolls back: what is left to serve the arm is its far end's index
// (readsByFarEnd) or a sequential read.
var nearEndIndexes = []string{
	"drop index if exists graph_tuples_current",
	"drop index if exists graph_tuples_as_of_out",
}

// TestTheVisibilityArmsProbeByIndex: over verification v16's seed and a dozen more agents and
// catalogue nodes, analyzed, a refs read of the nodes of each scope - a project's issues (whose rule
// holds the party arm too), asks' blocks, components, people, sessions, roles, pages, and catalogue
// constraints - is planned for bob, carol's token that lists PRIV and alice, each read twice, in a
// transaction of its own each time. Each read resolves its nodes as every read of the graph
// resolves the ends of its edges (resolveNodes), all of a kind in one statement.
//
//   - Under the planner's own settings, no plan holds a hashed subplan. Every `exists` an arm adds
//     under a CASE or an OR ends in `offset 0`, which keeps it a probe of its index per row. This
//     seed is small, the size at which Postgres prefers to hash a correlated `exists` into a read
//     of its whole table once per statement (#3178 at 0f8b389c), so this is where that form
//     shows. It shows only over more than one row: a read of one node runs the `exists` once,
//     and once is never cheaper hashed.
//   - With sequential scans off, no plan reads a scope's table sequentially, which would mean the
//     arm has no index path: on tables of a page or two a sequential read is cheaper than an
//     index probe, so only with them off does a plan say whether an index serves the arm. The
//     catalogue constraint's arm reads its tuple through graph_tuples_as_of_in
//     (graph_tuples_inbound until #3178's spec v50 R2 change), keyed on the tuple's far end, as
//     the one current-tuple rule is written to. The arm names its kind as the literal
//     'constraint': inside its fenced subquery the node's kind would be a parameter, and
//     `t.to_kind = n.kind` would leave neither partial index usable.
//
// The block schema's nodes take the every-viewer scope, which adds no read, and no read of the
// graph resolves one, so no read here plans one.
func TestTheVisibilityArmsProbeByIndex(t *testing.T) {
	withTestOnlyKinds(t, "lens_kind", "constraint")
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the seed: %v", err)
	}
	seedVerificationProjects(t, tx)
	seeded := seedScopedNodes(t, tx)
	sessions := []string{"ses_priv", "ses_pub"}
	roles := []string{"r_priv", "r_pub"}
	var constraints []string
	for index := range 12 {
		session, role, constraint := fmt.Sprintf("ses_%d", index), fmt.Sprintf("r_%d", index), uuid.NewString()
		writeSeedNodes(t, tx, []graphstore.Node{
			{Kind: graphstore.KindSession, ID: session},
			{Kind: graphstore.KindRole, ID: role},
			{Kind: graphstore.KindPage, ID: graphstore.SessionPageID(session)},
			{Kind: graphstore.KindPage, ID: graphstore.RolePageID(role)},
		}, []graphstore.Tuple{{FromKind: graphstore.KindRole, FromID: role, Kind: graphstore.TupleHeldBy,
			ToKind: graphstore.KindSession, ToID: session, At: time.Now(), Who: catalogueSeeder, Asserted: true}})
		if _, err := tx.Exec(ctx, `insert into session_projects (session_id, project_key) values ($1, $2)`, session, seededPublic); err != nil {
			t.Fatalf("record the project %s acted in: %v", session, err)
		}
		seedLensKind(t, tx, fmt.Sprintf("lens-%d", index), constraint, "")
		sessions, roles, constraints = append(sessions, session), append(roles, role), append(constraints, constraint)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the seed: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `analyze`); err != nil {
		t.Fatalf("analyze the seed: %v", err)
	}
	var pages []string
	for index := range sessions {
		pages = append(pages, graphstore.SessionPageID(sessions[index]), graphstore.RolePageID(roles[index]))
	}

	reads := []struct {
		scope, kind string
		ids         []string
		catalogue   bool
	}{
		{scope: "project", kind: "issue", ids: []string{"PRIV-1", "PUB-1"}},
		{scope: "owner item", kind: "ask_block", ids: []string{"ask/" + seeded.ask + "#b1", "ask/" + seeded.pubAsk + "#b1"}},
		{scope: "component", kind: "component", ids: []string{"PRIV/storage", "PUB/storage"}},
		{scope: "person", kind: graphstore.KindPerson, ids: []string{seededBob, seededAlice}},
		{scope: "session", kind: graphstore.KindSession, ids: sessions},
		{scope: "role", kind: graphstore.KindRole, ids: roles},
		{scope: "page", kind: graphstore.KindPage, ids: pages},
		{scope: "catalogue", kind: "constraint", ids: constraints, catalogue: true},
	}
	planned := map[string]bool{"bob": true, "carol's token that lists PRIV": true, "alice": true}
	for _, caller := range seededCallers() {
		if !planned[caller.name] {
			continue
		}
		reader := caller.reader(t, database.Pool)
		for _, read := range reads {
			ends := make([]edgeRow, len(read.ids))
			for index, id := range read.ids {
				ends[index] = edgeRow{otherKind: read.kind, otherID: id}
			}
			recorder := &statementRecorder{Queryer: database.Pool}
			if _, err := resolveNodes(ctx, recorder, reader, true, ends); err != nil {
				t.Fatalf("read the %s nodes %v as %s: %v", read.scope, read.ids, caller.name, err)
			}
			if len(recorder.statements) == 0 {
				t.Fatalf("the read of the %s nodes issued no statement, so no plan was read", read.scope)
			}
			subject := "the read of the " + read.scope + " nodes as " + caller.name
			for _, statement := range recorder.statements {
				plan := planStatement(t, database, statement)
				for _, hashed := range hashedSubplans(t, plan) {
					t.Errorf("%s plans %s, a hashed subplan that reads its whole table once per statement:\n%s", subject, hashed, plan)
				}
				settings := []string{"set local enable_seqscan = off"}
				if read.catalogue {
					settings = append(settings, nearEndIndexes...)
				}
				unindexed := planStatement(t, database, statement, settings...)
				for _, relation := range scopeIndexRelations {
					if explaintest.SeqScansRelation(t, unindexed, relation) {
						t.Errorf("%s reads %s sequentially with sequential scans off, so its arm has no index path:\n%s",
							subject, relation, unindexed)
					}
				}
				if read.catalogue && !readsByFarEnd(t, unindexed) {
					t.Errorf("%s does not read its constrained_by tuple by its far end (to_kind, to_id) through graph_tuples_as_of_in or graph_tuples_inbound:\n%s",
						subject, unindexed)
				}
			}
		}
	}
}

// hashedSubplan matches the reference to a subplan Postgres runs hashed. Postgres 18 names such a
// subplan "SubPlan N" like any other and writes "hashed SubPlan N" in the expression that uses it,
// so it is read from the plan's expressions rather than from the subplan's own name.
var hashedSubplan = regexp.MustCompile(`hashed SubPlan (\d+)`)

// hashedSubplans names each subplan plan runs hashed, by the relations it reads.
func hashedSubplans(t *testing.T, plan json.RawMessage) []string {
	t.Helper()
	nodes := explaintest.Nodes(t, plan)
	var hashed []string
	for _, match := range hashedSubplan.FindAllStringSubmatch(string(plan), -1) {
		name := "SubPlan " + match[1]
		var reads []string
		for _, node := range nodes {
			if node.Subplan == name && node.Relation != "" {
				reads = append(reads, node.String())
			}
		}
		hashed = append(hashed, name+" ("+strings.Join(reads, ", ")+")")
	}
	return hashed
}

// readsByFarEnd reports whether plan reads graph_tuples through an index of the tuple's far end
// under a condition on it, the index's leading (to_kind, to_id): graph_tuples_as_of_in, or
// graph_tuples_inbound, which #3178's spec v50 R2 change drops for it. Each is matched by name.
func readsByFarEnd(t *testing.T, plan json.RawMessage) bool {
	t.Helper()
	for _, scan := range explaintest.IndexScans(t, plan) {
		farEnd := scan.Index == "graph_tuples_as_of_in" || scan.Index == "graph_tuples_inbound"
		if farEnd && strings.Contains(scan.Cond, "to_kind") && strings.Contains(scan.Cond, "to_id") {
			return true
		}
	}
	return false
}

// planStatement explains statement, with its own arguments, in a transaction of its own that runs
// settings first, and answers its one plan.
func planStatement(t *testing.T, database *store.Store, statement recordedStatement, settings ...string) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range settings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+statement.sql, statement.args...).Scan(&raw); err != nil {
		t.Fatalf("explain %s: %v", statement.sql, err)
	}
	return explaintest.OnePlan(t, raw)
}

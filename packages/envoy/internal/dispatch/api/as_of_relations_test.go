// as_of_relations_test.go holds the relation inventory of a read at a cut (by the plan's
// decisions 1 and 2): each statement that an answers route sends at a cut reads a shadowed
// table by its bare name only through its shadow. Outside the shadows it reads only a table that
// the shadows read, or a table that asof.MachineryTables or asof.CutReadTables lists with its
// reason. The test reads every row of the t-routes harness on that harness's rig and world
// (as_of_equality_test.go).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// asOfProbeRaise is the start of the message of the error that each view of the schema
// asof_probe raises.
const asOfProbeRaise = "asof_probe: "

// asOfProbeViews gives each view and materialized view of public, with each relation of public
// that its definition reads.
const asOfProbeViews = `select distinct v.relname, t.relname
	from pg_depend d
	join pg_rewrite r on r.oid = d.objid
	join pg_class v on v.oid = r.ev_class
	join pg_class t on t.oid = d.refobjid
	where d.classid = 'pg_rewrite'::regclass and d.refclassid = 'pg_class'::regclass
	  and v.relnamespace = 'public'::regnamespace and t.relnamespace = 'public'::regnamespace
	  and v.relkind in ('v', 'm') and t.oid <> v.oid`

// createAsOfProbe makes the schema asof_probe in database. The schema holds a function fail,
// which raises an error that names its argument. It also holds one view for each shadowed table,
// and one view for each view of public that reads a shadowed table directly or through another
// view. Each view of asof_probe has the name and the columns of its relation in public, and its
// read calls fail. fail is immutable, so PostgreSQL calls it when it plans a statement that reads
// the view: the read fails before it reads a row, also for an empty table and in a branch that
// does not run. Each shadow reads public.<table>, so no shadow reads a view of asof_probe.
func createAsOfProbe(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := t.Context()
	rows, err := database.Pool.Query(ctx, asOfProbeViews)
	if err != nil {
		t.Fatalf("read the views of public: %v", err)
	}
	views, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var read [2]string
		err := row.Scan(&read[0], &read[1])
		return read, err
	})
	if err != nil {
		t.Fatalf("read the views of public: %v", err)
	}
	// reaches is each relation that the probe stops, with the shadowed tables that its read
	// reaches.
	reaches := map[string][]string{}
	for _, table := range asof.ShadowedTables() {
		reaches[table] = []string{table}
	}
	for grown := true; grown; {
		grown = false
		for _, read := range views {
			view, relation := read[0], read[1]
			for _, table := range reaches[relation] {
				if !slices.Contains(reaches[view], table) {
					reaches[view] = append(reaches[view], table)
					grown = true
				}
			}
		}
	}

	statements := []string{
		`create schema asof_probe`,
		`create function asof_probe.fail(relation text) returns boolean language plpgsql immutable as $$
begin
	raise exception '` + asOfProbeRaise + `% is read by its bare name outside the shadows', relation;
end
$$`,
	}
	for _, relation := range slices.Sorted(maps.Keys(reaches)) {
		named := relation
		if tables := reaches[relation]; !slices.Equal(tables, []string{relation}) {
			slices.Sort(tables)
			named = relation + " (a view of " + strings.Join(tables, ", ") + ")"
		}
		statements = append(statements, fmt.Sprintf(`create view %s as select * from %s where asof_probe.fail('%s')`,
			pgx.Identifier{"asof_probe", relation}.Sanitize(), pgx.Identifier{"public", relation}.Sanitize(),
			strings.ReplaceAll(named, "'", "''")))
	}
	for _, statement := range statements {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("make the probe: %s: %v", statement, err)
		}
	}
}

// asOfProbeRead is one read of the test: its route, what the test calls its caller, and its
// target. The context of the read carries it (probedRead).
type asOfProbeRead struct {
	route, caller, target string
}

func (r asOfProbeRead) String() string {
	if r.route == "" {
		return "a statement on a context that no read of the test marked"
	}
	return r.route + " as " + r.caller + ", " + r.target
}

// probedRead is the key of the asOfProbeRead that the context of a read of the test carries.
type probedRead struct{}

// probedStatementKey is the key of the probedStatement that TraceQueryStart gives to the context
// of the statement, for TraceQueryEnd.
type probedStatementKey struct{}

// probedStatement is one statement that a pool of the test sent: its SQL and its arguments as pgx
// got them, the read that sent it, whether its context carried a cut, and the error that it ended
// with.
type probedStatement struct {
	sql   string
	args  []any
	read  asOfProbeRead
	atCut bool
	err   error
}

// relationProbe traces the pools of the test and sets the search_path of their connections. It
// records each statement that a read of the test sends (on a context that carries probedRead),
// and each statement that starts with the shadows' WITH list on another context. Only a read at
// a cut sends such a statement: for example, a statement of a transaction that a read opened at a
// cut, sent on a context of its own. prepare puts asof_probe first on the search_path of each
// connection that a read of the test acquires, and takes it off each connection that another
// context acquires. Thus only the reads at the cut read through the probe. The writes and the
// server's own work read public.
type relationProbe struct {
	// shadowsFirst matches a statement that starts with the shadows' WITH list (asof.Bind):
	// whitespace and comments, then with or with recursive, then a shadow.
	shadowsFirst *regexp.Regexp
	mu           sync.Mutex
	statements   []*probedStatement
	// probing holds each connection whose search_path starts with asof_probe.
	probing sync.Map
}

func newRelationProbe() *relationProbe {
	return &relationProbe{shadowsFirst: regexp.MustCompile(`(?is)^(?:\s|--[^\n]*|/\*.*?\*/)*with\s+(?:recursive\s+)?(?:` +
		strings.Join(asof.ShadowedTables(), "|") + `)\s+as\s+not\s+materialized\s+\(`)}
}

// prepare is the pool's PrepareConn. It sets the search_path of conn for the context that
// acquires it, with pgconn's own Exec, which the tracer does not see: the setting is a part of
// the probe, not a statement of the read.
func (p *relationProbe) prepare(ctx context.Context, conn *pgx.Conn) (bool, error) {
	marked := ctx.Value(probedRead{}) != nil
	if _, probing := p.probing.Load(conn); probing == marked {
		return true, nil
	}
	statement := "reset search_path"
	if marked {
		statement = "set search_path = asof_probe, public"
	}
	if _, err := conn.PgConn().Exec(ctx, statement).ReadAll(); err != nil {
		p.probing.Delete(conn)
		return false, fmt.Errorf("the probe's %s: %w", statement, err)
	}
	if marked {
		p.probing.Store(conn, struct{}{})
	} else {
		p.probing.Delete(conn)
	}
	return true, nil
}

// record keeps the statement when the probe records it (relationProbe), and gives it, or nil.
func (p *relationProbe) record(ctx context.Context, sql string, args []any) *probedStatement {
	read, marked := ctx.Value(probedRead{}).(asOfProbeRead)
	if !marked && !p.shadowsFirst.MatchString(sql) {
		return nil
	}
	_, atCut := asof.CutFrom(ctx)
	statement := &probedStatement{sql: sql, args: args, read: read, atCut: atCut}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statements = append(p.statements, statement)
	return statement
}

func (p *relationProbe) ended(statement *probedStatement, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	statement.err = err
}

// recorded is a copy of each statement that the probe recorded, in the order they started.
func (p *relationProbe) recorded() []probedStatement {
	p.mu.Lock()
	defer p.mu.Unlock()
	statements := make([]probedStatement, len(p.statements))
	for index, statement := range p.statements {
		statements[index] = *statement
	}
	return statements
}

func (p *relationProbe) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if statement := p.record(ctx, data.SQL, data.Args); statement != nil {
		return context.WithValue(ctx, probedStatementKey{}, statement)
	}
	return ctx
}

func (p *relationProbe) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if statement, ok := ctx.Value(probedStatementKey{}).(*probedStatement); ok {
		p.ended(statement, data.Err)
	}
}

func (p *relationProbe) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (p *relationProbe) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if statement := p.record(ctx, data.SQL, data.Args); statement != nil {
		p.ended(statement, data.Err)
	}
}

func (p *relationProbe) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// probeRead sends target as caller, with a fresh proof, on a context that carries read, and
// keeps the answer.
func probeRead(t *testing.T, rig *asOfRig, caller asOfReader, target string, read asOfProbeRead) asOfAnswer {
	t.Helper()
	request := rig.key.presenting(t, http.MethodGet, target, caller.token, nil)
	request = request.WithContext(context.WithValue(request.Context(), probedRead{}, read))
	response := rig.serve(request)
	return asOfAnswer{status: response.Code, header: response.Header().Clone(), body: bytes.Clone(response.Body.Bytes())}
}

// statementStart is the first word of a statement after whitespace and comments, or the "(" that
// starts it.
var statementStart = regexp.MustCompile(`(?s)^(?:\s|--[^\n]*|/\*.*?\*/)*([A-Za-z]+|\()`)

// planned reports whether sql starts as a read that EXPLAIN plans: select, with, values, table or
// "(". The other statements that a read at a cut sends set no table (asof.Bind sends set, show,
// savepoint, release and rollback unbound), or they are the begin and the commit of pgx.
func planned(sql string) bool {
	match := statementStart.FindStringSubmatch(sql)
	return match != nil && slices.Contains([]string{"select", "with", "values", "table", "("}, strings.ToLower(match[1]))
}

// scannedRelations adds to relations each relation, as schema.name, that node, a part of an
// EXPLAIN (FORMAT JSON, VERBOSE) plan, scans, but for the scans inside the subplan of a
// materialized common table expression whose name skip holds ("CTE <name>").
func scannedRelations(node any, skip map[string]bool, relations map[string]bool) {
	switch value := node.(type) {
	case []any:
		for _, element := range value {
			scannedRelations(element, skip, relations)
		}
	case map[string]any:
		if name, ok := value["Subplan Name"].(string); ok {
			if table, cte := strings.CutPrefix(name, "CTE "); cte && skip[table] {
				return
			}
		}
		if relation, ok := value["Relation Name"].(string); ok {
			schema, _ := value["Schema"].(string)
			relations[schema+"."+relation] = true
		}
		for _, child := range value {
			scannedRelations(child, skip, relations)
		}
	}
}

// explainScans is each relation, as schema.name, that the plan of sql with args scans, but for the
// scans inside the subplan of a materialized common table expression whose name skip holds.
func explainScans(ctx context.Context, database *store.Store, sql string, args []any, skip map[string]bool) (map[string]bool, error) {
	var plan []byte
	if err := database.Pool.QueryRow(ctx, "explain (format json, verbose) "+sql, args...).Scan(&plan); err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(plan, &decoded); err != nil {
		return nil, fmt.Errorf("decode the plan: %w", err)
	}
	relations := map[string]bool{}
	scannedRelations(decoded, skip, relations)
	return relations, nil
}

// asOfStatementLength is how many bytes of a statement a failure names.
const asOfStatementLength = 600

// TestAPastTimeStatementReadsNoShadowedTableDirectly reads every row of the t-routes harness at
// the cut, as each caller of the harness with as_of and as the token cut at the cut, on the
// harness's world (seedAsOfWorld, written again after the cut). It holds each statement of those
// reads to two rules.
//
//  1. No statement reads a shadowed table by its bare name outside the shadows. A connection
//     that a read at the cut acquires has asof_probe first on its search_path (relationProbe,
//     createAsOfProbe). Thus a bare name that the shadows do not hold resolves to a view of
//     asof_probe, and the view raises an error that names the table. This includes a statement
//     that the store did not bind, a statement on a pool that does not bind, and a function that
//     reads the table by its bare name. The caller lookup, the gate and asOfGate's read of now()
//     go through the probe too, because they run on the context of the read.
//  2. Each statement that the store bound under the cut reads outside the shadows only a base
//     table that the shadows read, or a table that asof.MachineryTables or asof.CutReadTables
//     lists: decision 2 admits those tables and no other table with no shadow. The test plans
//     each such statement with its arguments (EXPLAIN (FORMAT JSON, VERBOSE)) with each shadow
//     made materialized, so PostgreSQL plans each shadow as a subplan of its own ("CTE <table>"),
//     and holds each relation that the plan scans outside those subplans. So a table with no
//     shadow that a statement names with its schema (public.route_roster) fails too. A shadowed
//     table that a statement names with its schema is a base table that the shadows read: the
//     reads of the present log that decide what changed after the cut name public.events
//     (asof.AsksChangedAfterStatement), and the keyword relation reads the present deleted_at
//     of a post from public.comments and public.messages (decision 14).
//
// Each read must be served as the harness serves it at the cut (servedAtTheCut), so that no read
// passes because it stopped before its statements ran. A document removed after the cut answers
// 404 at the cut. And each row must send at least one statement that the store bound under the
// cut, so that no row passes with nothing to hold.
func TestAPastTimeStatementReadsNoShadowedTableDirectly(t *testing.T) {
	database := storetest.Open(t)
	createAsOfProbe(t, database)
	probe := newRelationProbe()
	rig := newAsOfRigOn(t, storetest.Reopened(t, database, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = probe
		config.PrepareConn = probe.prepare
	}))
	world := seedAsOfWorld(t, rig.handler)
	callers := rig.callers(t)
	rig.settle(t)
	cut := rig.cut(t)
	time.Sleep(5 * time.Millisecond)
	world.writeAfterTheCut(t, rig.handler)
	rig.settle(t)
	fillTextVersionsPast(t, rig.database, cut)
	cutCaller := rig.cutCaller(t, cut)

	for _, row := range asOfEqualityRows {
		for _, read := range row.reads(world) {
			for _, caller := range append(slices.Clone(callers), cutCaller) {
				target := read.target
				if caller.token != cutCaller.token {
					target = withAsOf(t, target, cut)
				}
				sent := asOfProbeRead{route: row.route, caller: caller.name, target: target}
				answer := probeRead(t, rig, caller, target, sent)
				served := read
				served.missing = read.missing || read.removed
				if problem := servedAtTheCut(row.route, served, caller, answer); problem != "" {
					t.Errorf("%s: %s", sent, problem)
				}
			}
		}
	}
	// The reads' detached work ends here, so the probe holds its statements too.
	awaitBackgroundWrites(t, rig.served)

	shadowed := map[string]bool{}
	// materialize makes each shadow of a bound statement materialized, and collapse writes each
	// shadow as its name alone, for a failure.
	var materialize, collapse []string
	for _, table := range asof.ShadowedTables() {
		shadowed[table] = true
		member := asof.Shadows(cut, table)
		materialize = append(materialize, member, strings.Replace(member, " as not materialized (", " as materialized (", 1))
		collapse = append(collapse, member, table+" as not materialized (<its shadow>)")
	}
	materialized, collapsed := strings.NewReplacer(materialize...), strings.NewReplacer(collapse...)
	describe := func(sql string) string {
		text := strings.Join(strings.Fields(collapsed.Replace(sql)), " ")
		if len(text) > asOfStatementLength {
			return text[:asOfStatementLength] + fmt.Sprintf("… (%d bytes)", len(text))
		}
		return text
	}

	// outside is each relation that a bound statement may scan outside the shadows: each base
	// table that a shadow reads, and each table of the two lists.
	outside := map[string]bool{}
	for table := range shadowed {
		bound, err := asof.Bind("select * from "+table, cut)
		if err != nil {
			t.Fatalf("bind a read of %s: %v", table, err)
		}
		scans, err := explainScans(t.Context(), database, bound, nil, nil)
		if err != nil {
			t.Fatalf("plan the shadow of %s: %v", table, err)
		}
		maps.Copy(outside, scans)
	}
	for name := range asof.CutReadTables {
		outside["public."+name] = true
	}
	for name := range asof.MachineryTables {
		outside["public."+name] = true
	}

	// failures is each failure, with each read whose statement failed so.
	failures := map[string][]string{}
	fail := func(failure string, read asOfProbeRead) {
		if reads := failures[failure]; !slices.Contains(reads, read.String()) {
			failures[failure] = append(reads, read.String())
		}
	}
	plans := map[string]map[string]bool{}
	boundByRoute := map[string]int{}
	scannedOutside := map[string]int{}
	statements := probe.recorded()
	bound := 0
	for _, statement := range statements {
		var postgres *pgconn.PgError
		if errors.As(statement.err, &postgres) && strings.HasPrefix(postgres.Message, asOfProbeRaise) {
			fail(postgres.Message+": "+describe(statement.sql), statement.read)
			continue
		}
		if !statement.atCut && !probe.shadowsFirst.MatchString(statement.sql) || !planned(statement.sql) {
			continue
		}
		bound++
		boundByRoute[statement.read.route]++
		if statement.err != nil {
			fail(fmt.Sprintf("a statement that the store bound under the cut ended with %v, so the test cannot plan it: %s",
				statement.err, describe(statement.sql)), statement.read)
			continue
		}
		variant := materialized.Replace(statement.sql)
		key := variant + "\x00" + fmt.Sprint(statement.args...)
		scans, done := plans[key]
		if !done {
			var err error
			if scans, err = explainScans(t.Context(), database, variant, statement.args, shadowed); err != nil {
				fail(fmt.Sprintf("EXPLAIN of a statement that the store bound under the cut fails: %v: %s", err, describe(statement.sql)), statement.read)
				continue
			}
			plans[key] = scans
		}
		for relation := range scans {
			scannedOutside[relation]++
			if !outside[relation] {
				fail(fmt.Sprintf("a statement that the store bound under the cut scans %s outside the shadows, which no shadow reads "+
					"and neither asof.MachineryTables nor asof.CutReadTables lists: %s", relation, describe(statement.sql)), statement.read)
			}
		}
	}
	for _, row := range asOfEqualityRows {
		if boundByRoute[row.route] == 0 {
			t.Errorf("%s: its reads at the cut sent no statement that the store bound under the cut, so the test holds nothing of it", row.route)
		}
	}

	seen := make([]string, 0, len(scannedOutside))
	for _, relation := range slices.Sorted(maps.Keys(scannedOutside)) {
		seen = append(seen, fmt.Sprintf("%s (%d)", relation, scannedOutside[relation]))
	}
	t.Logf("the cut is %s; %d statements recorded, %d bound under the cut, %d distinct plans; scanned outside the shadows: %s",
		cut, len(statements), bound, len(plans), strings.Join(seen, ", "))
	for _, failure := range slices.Sorted(maps.Keys(failures)) {
		t.Errorf("%s\n    in: %s", failure, strings.Join(failures[failure], "\n        "))
	}
}

package api

// What one statement of a project read binds, read as Postgres reads it (libpg_query, through
// github.com/wasilibs/go-pgquery, the parser the migration census already reads SQL with).
//
// The rule the census holds a statement to: every project-owned table the statement reads - in a
// FROM list, a comma join, a JOIN, a subselect or the body of a function it calls - is bound by a
// clause on that table's own alias, or joined by its own owner column to a table that is. A clause
// is bound to an alias by the columns it reads: a fragment that reads no column of a row decides
// nothing about that row, and a fragment that reads a reference the row holds to another project's
// object (issue_links.target_key) decides the object it names, not the row.
//
// The project-owned tables are read off the migrated schema and the reference census
// (referenceDispositions), never listed here: a table is project-owned when it holds a column that
// names the object whose visibility decides its rows - one of the six objects a read names by id,
// or a node of the graph - and the census fails on a column of that shape the reference census
// does not class, and on a class of its own the schema does not hold.
//
// What the census does not judge: the reads a clause makes of its own, inside the fragment it
// renders (the rule cannot be asked to bind itself), and the body of a function such a clause
// calls (session_clause_visible). It reads those bodies all the same, and
// TestEveryProjectReadBindsTheFragmentPerStatement fails if it read none of them.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	pgparser "github.com/wasilibs/go-pgquery/parser"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// m19ObjectTables are the six objects a read names by id, which the reference census reads every
// column naming one off the schema for (schemaReferenceColumns), with the column that names each.
// Every one is project-owned, and a clause on its own key binds it.
var m19ObjectTables = map[string]string{
	"projects": "key", "issues": "key", "artifacts": "id", "asks": "id", "comments": "id", "messages": "id",
}

// m19OwnerColumnNames are the column names that hold a row to its owner with no foreign key on
// them, and the object each names. Each is a reference column too (referenceColumnNames), so
// referenceDispositions says of every column of this shape the schema holds whether it is the
// row's own owner or a reference to an object of another project.
var m19OwnerColumnNames = map[string]string{
	"project": "projects", "project_key": "projects", "issue": "issues", "issue_key": "issues",
	"artifact_id": "artifacts",
}

// m19OwnerEdgesBeyondTheReferenceCensus are the foreign keys into a project-owned table that the
// reference census does not read, because the row they name is no object a read names by id. Each
// holds its row to the row it names, whose visibility decides its own. The census fails on a
// foreign key of this shape the schema holds that this list does not name, and on an entry the
// schema does not hold, so a table added later is classed by hand once and never by accident.
var m19OwnerEdgesBeyondTheReferenceCensus = map[string]string{
	"check_result_retractions.result_id": "the retracted check result, of the retraction's own issue",
	"component_depends.from_id":          "the component that states the dependency",
	"component_depends.to_id":            "the component depended on, which the composite key holds to the same project",
	"components.snapshot_id":             "the architecture snapshot the component was read from, of its own project",
	"components.source_id":               "the architecture source the component was read from, of its own project",
	"going_events.request_id":            "the going run whose event it is",
	"project_access_changes.event_id":    "the event that stored the access change",
	"going_request_posts.request_id":     "the going run whose post it is, as going_events.request_id names the run whose event it is",
}

// m19BindingFragments says, of every SQL fragment the access package writes (access.Marker),
// whether a row whose column it reads is bound by it to the projects its reader sees. A clause
// over a session, a person, a role or the credential's own list decides who the row names, not
// which project owns the row, so it binds none. A fragment this list does not name fails the
// census: a clause added later is read once by hand and never passes by accident.
var m19BindingFragments = map[string]bool{
	"ProjectClause":           true,
	"IssueProjectClause":      true,
	"ItemOwnerClause":         true,
	"DocumentClause":          true,
	"MessageClause":           true,
	"PartyClause":             true,
	"BroadcastClause":         true,
	"PersonSeesProjectSQL":    true,
	"ListsClause":             false,
	"OpenClause":              false,
	"PersonClause":            false,
	"PersonDirectlyListedSQL": false,
	"RoleClause":              false,
	"RoleProjectClause":       false,
	"SessionClause":           false,
	"SessionContentClause":    false,
	"VisibleProjectsSQL":      false,

	// It holds the projects row it reads to the row's own project key and to the viewer: a private
	// project the viewer does not see, whose issues the repository rule reads only to hide a
	// repository, never to answer them (refs.RepositoryClause).
	"HiddenPrivateProjectClause": true,
}

// m19Owner is what holds a row to the object whose visibility decides its own: the owner's table
// and the column of it the row names. An owner with no table is an address, a column naming a node
// of any kind (graph_tuples.from_id), which binds only where a clause reads it.
type m19Owner struct{ table, column string }

// m19Function is one function of the public schema the census may have to read the body of, with
// the names of its arguments, which its body may read them by.
type m19Function struct {
	name, language, source, definition string
	arguments                          []string
}

// m19Address is one node address a table holds: the column that says which nodes the id is
// among, and the columns that name the node.
type m19Address struct {
	kind string
	ids  []string
}

// m19Schema is what the census judges a statement against, read off the migrated schema.
type m19Schema struct {
	// kinds is the kind pg_class holds for every relation of the public schema. The census reads
	// a table and a view - a view answers the rows its own columns address - and refuses a
	// statement that reads anything else.
	kinds map[string]string
	// columns is every column of every relation.
	columns map[string]map[string]bool
	// owned names each project-owned relation with why it is one.
	owned map[string]string
	// owners is each relation's owner columns: the columns that hold its rows to the object
	// whose visibility decides theirs.
	owners map[string]map[string]m19Owner
	// binds is, for each relation, every column a clause reading it binds something by: an owner
	// column, a column that names the row itself, or one half of a node's address, less every
	// column that may name an object of a project the row is not in (filteredReference).
	binds map[string]map[string]bool
	// plain is binds without the node addresses: the columns that bind the whole row on their
	// own, since what owns a row decides every column of it.
	plain map[string]map[string]bool
	// addresses are the node addresses of each relation, by prefix. A row that names a node and
	// nothing else is answered only where every address of it a project may own is bound: a
	// tuple names two objects, and answering it answers both.
	addresses map[string]map[string]m19Address
	// functions are the functions of the public schema, by name: a name may hold several, since
	// Postgres allows a function to be overloaded.
	functions map[string][]m19Function
}

// m19ProjectSchema reads what the census judges a statement against off the migrated schema: every
// relation and its columns, the project-owned tables and what owns each of their rows, and the
// body of every function a read may call.
func m19ProjectSchema(t *testing.T, database *store.Store) *m19Schema {
	t.Helper()
	schema := &m19Schema{
		kinds: map[string]string{}, columns: map[string]map[string]bool{}, owned: map[string]string{},
		owners: map[string]map[string]m19Owner{}, binds: map[string]map[string]bool{},
		plain: map[string]map[string]bool{}, addresses: map[string]map[string]m19Address{},
		functions: map[string][]m19Function{},
	}
	type column struct{ Relation, Kind, Column string }
	for _, held := range m19Rows[column](t, database, `
		select c.relname, c.relkind::text, a.attname
		from pg_class c
		join pg_attribute a on a.attrelid = c.oid and a.attnum > 0 and not a.attisdropped
		where c.relnamespace = 'public'::regnamespace and c.relkind in ('r', 'p', 'v', 'm', 'f')`) {
		schema.kinds[held.Relation] = held.Kind
		if schema.columns[held.Relation] == nil {
			schema.columns[held.Relation] = map[string]bool{}
		}
		schema.columns[held.Relation][held.Column] = true
	}
	if len(schema.kinds) == 0 {
		t.Fatal("the schema holds no relation, so the census reads nothing")
	}

	type reference struct{ Child, Column, Parent, Key string }
	foreign := map[string]m19Owner{}
	for _, held := range m19Rows[reference](t, database, `
		select k.conrelid::regclass::text, a.attname, k.confrelid::regclass::text, b.attname
		from pg_constraint k
		join lateral unnest(k.conkey) with ordinality as child (number, place) on true
		join pg_attribute a on a.attrelid = k.conrelid and a.attnum = child.number
		join lateral unnest(k.confkey) with ordinality as parent (number, place) on parent.place = child.place
		join pg_attribute b on b.attrelid = k.confrelid and b.attnum = parent.number
		where k.contype = 'f' and k.connamespace = 'public'::regnamespace`) {
		foreign[held.Child+"."+held.Column] = m19Owner{table: held.Parent, column: held.Key}
	}
	// The columns that name a row on their own: a primary key or a unique index of one column.
	// A column of a key of several names no row alone (graph_nodes.kind).
	type key struct{ Relation, Column string }
	identity := map[string]map[string]bool{}
	for _, held := range m19Rows[key](t, database, `
		select i.indrelid::regclass::text, a.attname
		from pg_index i
		join pg_class c on c.oid = i.indrelid
		join pg_attribute a on a.attrelid = i.indrelid and a.attnum = i.indkey[0]
		where c.relnamespace = 'public'::regnamespace and i.indisunique and i.indnatts = 1
			and i.indpred is null and i.indexprs is null`) {
		if identity[held.Relation] == nil {
			identity[held.Relation] = map[string]bool{}
		}
		identity[held.Relation][held.Column] = true
	}
	// A relation whose primary key is a kind and an id is a store of nodes: its kind and id are
	// the address of the node each row is.
	primary := map[string][]string{}
	for _, held := range m19Rows[key](t, database, `
		select k.conrelid::regclass::text, a.attname
		from pg_constraint k
		join lateral unnest(k.conkey) as child (number) on true
		join pg_attribute a on a.attrelid = k.conrelid and a.attnum = child.number
		where k.contype = 'p' and k.connamespace = 'public'::regnamespace`) {
		primary[held.Relation] = append(primary[held.Relation], held.Column)
	}
	nodes := map[string]bool{}
	for relation, columns := range primary {
		nodes[relation] = slices.Equal(slices.Sorted(slices.Values(columns)), []string{"id", "kind"})
	}

	type function struct {
		Name, Language, Source, Definition string
		Arguments                          []string
	}
	for _, held := range m19Rows[function](t, database, `
		select p.proname, l.lanname, p.prosrc, pg_get_functiondef(p.oid), coalesce(p.proargnames, '{}')
		from pg_proc p join pg_language l on l.oid = p.prolang
		where p.pronamespace = 'public'::regnamespace and p.prokind = 'f'`) {
		schema.functions[held.Name] = append(schema.functions[held.Name], m19Function{
			name: held.Name, language: held.Language, source: held.Source, definition: held.Definition,
			arguments: held.Arguments,
		})
	}

	schema.classify(t, database, foreign, identity, nodes)
	return schema
}

// classify reads which tables a project owns and what owns each of their rows, off the schema's
// own columns and the reference census's reading of every column that names an object
// (referenceDispositions). A column that may name an object of another project
// (filteredReference) owns nothing and binds nothing.
func (s *m19Schema) classify(t *testing.T, database *store.Store, foreign map[string]m19Owner, identity map[string]map[string]bool, nodes map[string]bool) {
	t.Helper()
	filtered := map[string]bool{}
	for table, key := range m19ObjectTables {
		if s.kinds[table] == "" {
			t.Fatalf("the schema holds no %s, the object the census is written over", table)
		}
		s.owned[table] = "an object a read names by id, whose own " + key + " names it"
	}
	for _, named := range schemaReferenceColumns(t, fragmentCensusFixture{privateSeed: privateSeed{database: database}}) {
		table, column, _ := strings.Cut(named, ".")
		owner, names := m19OwnerColumnNames[column]
		object := m19Owner{table: owner, column: m19ObjectTables[owner]}
		if !names {
			held, foreignKey := foreign[named]
			if !foreignKey || m19ObjectTables[held.table] == "" {
				continue // a session, a person or a route: no object of a project
			}
			object = held
		}
		disposition, classed := referenceDispositions[named]
		if !classed {
			t.Errorf("%s names an object of a project and the reference census does not class it: class it there first", named)
			continue
		}
		if disposition.kind == filteredReference {
			filtered[named] = true
			continue
		}
		s.own(table, column, object, named+" names "+disposition.how)
	}

	// The relations that name a node of the graph rather than one object: a prefixed kind beside
	// its id (from_kind, from_id), or the kind and id that are a store of nodes' own key. Answering
	// such a row answers every node it names, so each address binds its own side. Elsewhere a
	// bare kind and id are the row's own (artifacts.kind is a document's kind) and name no node.
	for relation, columns := range s.columns {
		for column := range columns {
			prefix, address := m19AddressColumn(columns, column)
			if !address || prefix == "" && !nodes[relation] {
				continue
			}
			if s.owned[relation] == "" {
				s.owned[relation] = relation + "." + column + " names a node of the graph, which a project may own"
			}
			if s.addresses[relation] == nil {
				s.addresses[relation] = map[string]m19Address{}
			}
			held := s.addresses[relation][prefix]
			s.addresses[relation][prefix] = m19Address{
				kind: strings.TrimPrefix(prefix+"_kind", "_"), ids: append(held.ids, column),
			}
			s.own(relation, column, m19Owner{}, relation+"."+column+" names a node the row is of")
		}
	}

	// Every other foreign key into a project-owned table, which the reference census does not
	// read: each is read by hand once (m19OwnerEdgesBeyondTheReferenceCensus) and carries its
	// row's visibility with it, so a table it names becomes project-owned in its turn.
	named := map[string]bool{}
	for grew := true; grew; {
		grew = false
		for child, owner := range foreign {
			table, column, _ := strings.Cut(child, ".")
			if s.owned[owner.table] == "" || s.owners[table][column] != (m19Owner{}) || filtered[child] {
				continue
			}
			if s.inAddress(table, column) {
				continue // a node's address, which the rule above has read
			}
			why, classed := m19OwnerEdgesBeyondTheReferenceCensus[child]
			if !classed {
				t.Errorf("%s names a row of %s, a project-owned table, and no census classes it: name it in m19OwnerEdgesBeyondTheReferenceCensus or in the reference census",
					child, owner.table)
				named[child] = true
				continue
			}
			named[child] = true
			s.own(table, column, owner, child+" names "+why)
			grew = true
		}
	}
	for child := range m19OwnerEdgesBeyondTheReferenceCensus {
		if !named[child] {
			t.Errorf("m19OwnerEdgesBeyondTheReferenceCensus names %s, which is no foreign key of the schema into a project-owned table: take it off the list", child)
		}
	}

	for named, why := range m19OwnerColumnsByHand {
		table, column, _ := strings.Cut(named, ".")
		if !s.columns[table][column] {
			t.Errorf("m19OwnerColumnsByHand names %s, which the schema does not hold: take it off the list", named)
			continue
		}
		s.own(table, column, m19Owner{}, named+" names "+why)
	}

	// A clause binds a row by a column that names what owns it or the row itself, never by one
	// that may name an object of another project. An address binds the node it names, and the
	// row only where every address of it is bound.
	for table, owners := range s.owners {
		for column := range owners {
			s.bind(table, column, owners[column] != (m19Owner{}) || !s.inAddress(table, column))
		}
	}
	// A row of a relation no project owns is never named, but a clause over its own key still
	// binds it, and through it the rows it owns (a broadcast's copies, by their broadcast_id).
	for table, columns := range identity {
		for column := range columns {
			s.bind(table, column, !s.inAddress(table, column))
		}
	}
	for named := range filtered {
		table, column, _ := strings.Cut(named, ".")
		delete(s.binds[table], column)
		delete(s.plain[table], column)
	}
}

// own records that table's column names what owns its rows, which makes table project-owned.
func (s *m19Schema) own(table, column string, owner m19Owner, why string) {
	if s.owners[table] == nil {
		s.owners[table] = map[string]m19Owner{}
	}
	s.owners[table][column] = owner
	if s.owned[table] == "" {
		s.owned[table] = why
	}
}

// bind records that a clause reading table's column binds something: the row itself where the
// column names what owns it or which row it is (plain), and the node it names where it is one
// half of a node's address.
func (s *m19Schema) bind(table, column string, plain bool) {
	if s.binds[table] == nil {
		s.binds[table] = map[string]bool{}
	}
	s.binds[table][column] = true
	if !plain {
		return
	}
	if s.plain[table] == nil {
		s.plain[table] = map[string]bool{}
	}
	s.plain[table][column] = true
}

// m19KindsOfNoProject are the node kinds no project owns, each with why: the kinds the graph's
// registry answers by the access package's clause of their own kind rather than a project's
// (refs/visibility.go), an attribute's value, which is no node, a priority (prioritystore: a
// priority belongs to no project), and no kind at all (`node_kind is null`). A row addressing a
// node only of these kinds names no object a project owns; every other kind - a registry kind of
// a project, a step of an issue's workflow, an embedded window - may. The registry kinds here are
// read from its own list (TestM19EveryKindOfNoProjectIsTheRegistrys), and the others are the
// stores' own constants.
var m19KindsOfNoProject = map[string]string{
	graphstore.KindPerson:      "a person, read by PersonClause",
	graphstore.KindSession:     "a session, read by SessionClause",
	graphstore.KindRole:        "a role, read by RoleClause",
	graphstore.KindPage:        "a role's page, read as its role",
	"supertag":                 "a type of the block schema, which every viewer sees",
	"attribute":                "an attribute of the block schema, which every viewer sees",
	graphstore.KindValue:       "an attribute's value, which is no node",
	prioritystore.KindPriority: "a priority, which belongs to no project",
	"":                         "no node: the statement holds the address null",
}

// m19NotRegistryKinds are the entries of m19KindsOfNoProject that are no kind of the graph's
// registry: what the registry holds is every other entry.
var m19NotRegistryKinds = []string{graphstore.KindValue, prioritystore.KindPriority, ""}

// m19OwnerColumnsByHand are the columns that hold a row to its owner which no census classes as
// one - no foreign key into a project-owned table, no reference column - each read by hand once;
// the census fails on one the schema does not hold. Each names an owner that is no one object of
// a project, so it binds as an address does: an equality to the row it names, or a clause over
// it, binds the row.
var m19OwnerColumnsByHand = map[string]string{
	"user_threads.thread_id":     "the root of the thread whose state the row is - an ask, a comment or a message - which is of the row's own project_key (user_threads_record, migration 0095)",
	"user_threads.last_reply_id": "the newest reply of the thread whose state the row is, a comment or a message of the row's own project_key (user_threads_record, migration 0095)",
	"messages.broadcast_id":      "the broadcast the message is a copy of, a conversation of no project that access.BroadcastClause reads",
}

// m19AddressColumn answers whether a table's column holds the id of a node of the graph, which a
// column of its own names the kind of: `id` beside `kind`, or `<prefix>_id` or `<prefix>_address`
// beside `<prefix>_kind`. The prefix is empty for the bare pair. A block's own link row names the
// block that states it as `from_node`, whose kind its table implies, so that column is an address
// of its own.
func m19AddressColumn(columns map[string]bool, column string) (string, bool) {
	if column == "id" {
		return "", columns["kind"]
	}
	if column == "from_node" {
		return "from_node", true
	}
	for _, suffix := range []string{"_id", "_address"} {
		prefix, held := strings.CutSuffix(column, suffix)
		if held && columns[prefix+"_kind"] {
			return prefix, true
		}
	}
	return "", false
}

// inAddress answers whether a relation's column is one half of a node's address the census
// classed: an id that names the node, or the kind that says which nodes the id is among. A kind
// names no row on its own.
func (s *m19Schema) inAddress(relation, column string) bool {
	for _, address := range s.addresses[relation] {
		if address.kind == column || slices.Contains(address.ids, column) {
			return true
		}
	}
	return false
}

// addressOf is the prefix of the address a relation's column is one half of.
func (s *m19Schema) addressOf(relation, column string) string {
	for prefix, address := range s.addresses[relation] {
		if address.kind == column || slices.Contains(address.ids, column) {
			return prefix
		}
	}
	return ""
}

// matches answers whether an equality between two columns holds the rows of one to the rows of the
// other: one names what owns the other, both name the same owner, both name the row the other is,
// or one is a node's address and the other names that node.
func (s *m19Schema) matches(left, right m19Ref) bool {
	for _, pair := range [][2]m19Ref{{left, right}, {right, left}} {
		one, other := pair[0], pair[1]
		owner, owns := s.owners[one.item.table][one.column]
		if !owns {
			continue
		}
		if owner.table == "" {
			if s.plain[other.item.table][other.column] || s.inAddress(other.item.table, other.column) {
				return true // a node's address against the row or the address that names it
			}
			continue
		}
		if owner == (m19Owner{table: other.item.table, column: other.column}) {
			return true
		}
		if theirs, owned := s.owners[other.item.table][other.column]; owned && theirs == owner {
			return true // two rows of one owner
		}
	}
	return left.item.table == right.item.table && left.column == right.column && s.plain[left.item.table][left.column]
}

// m19Rows reads one query of the schema into a slice of structs, by column order.
func m19Rows[T any](t testing.TB, database *store.Store, query string) []T {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	read, err := pgx.CollectRows(rows, pgx.RowToStructByPos[T])
	if err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	return read
}

// m19Unbound is one project-owned table a statement reads that nothing binds: the table, the alias
// it reads it under, and the function whose body reads it, empty for the statement's own text;
// and where in that text the read stands, which a failure prints so a reader finds the arm.
type m19Unbound struct {
	table, alias, function string
	location               int
}

func (u m19Unbound) String() string {
	named := u.table + " as " + u.alias
	if u.function != "" {
		named += " in " + u.function
	}
	return named
}

// m19UnboundReads names every project-owned table one statement reads that no clause on that
// table's own alias binds, and answers the functions whose bodies it read on the way. checked are
// the parameters another statement of the same transaction binds under the clause, which this
// statement reads as bound (readsKeyedByTheirTransaction). A statement it cannot read - one that
// does not parse, one that reads a relation the schema does not hold, one under a fragment the
// census does not know - is an error, never a pass.
func m19UnboundReads(schema *m19Schema, statement string, checked ...int) ([]m19Unbound, []string, error) {
	reader := &m19Reader{schema: schema, bodies: map[string]bool{}, reading: map[string]bool{}}
	var bound []bool
	for _, number := range checked {
		for len(bound) < number {
			bound = append(bound, false)
		}
		bound[number-1] = true
	}
	unbound, err := reader.statement(statement, m19Body{bound: bound})
	if err != nil {
		return nil, nil, err
	}
	slices.SortFunc(unbound, func(a, b m19Unbound) int { return strings.Compare(a.String(), b.String()) })
	return slices.CompactFunc(unbound, func(a, b m19Unbound) bool { return a.String() == b.String() }), slices.Sorted(maps.Keys(reader.bodies)), nil
}

// m19Reader reads a statement and the bodies of the functions it calls.
type m19Reader struct {
	schema *m19Schema
	// bodies are the functions whose bodies the reader read, by name.
	bodies map[string]bool
	// reading are the functions whose bodies it is reading now, so a function that calls itself
	// is read once.
	reading map[string]bool
}

// m19Body is what a statement is the body of: the function, the names its arguments are read by,
// and which arguments the call that reads it binds. A statement the server sent is the body of no
// function.
type m19Body struct {
	function  string
	arguments []string
	bound     []bool
}

// statement reads one statement, and the body of every function it calls.
func (r *m19Reader) statement(sql string, body m19Body) ([]m19Unbound, error) {
	spans, err := m19Spans(sql)
	if err != nil {
		return nil, err
	}
	tree, err := pgparser.ParseToJSON(sql)
	if err != nil {
		return nil, fmt.Errorf("parse the statement: %w", err)
	}
	var parsed struct {
		Stmts []struct{ Stmt any } `json:"stmts"`
	}
	if err := json.Unmarshal([]byte(tree), &parsed); err != nil {
		return nil, fmt.Errorf("read the parse tree: %w", err)
	}
	analysis := &m19Analysis{schema: r.schema, spans: spans, arguments: body.arguments}
	for index, bound := range body.bound {
		// A function's body reads the arguments its call binds as values a clause bound.
		if bound {
			analysis.parameter(index + 1).bind(fmt.Sprintf("$%d", index+1))
		}
	}
	for _, held := range parsed.Stmts {
		analysis.statement(held.Stmt, nil)
	}
	if analysis.err != nil {
		return nil, analysis.err
	}
	analysis.bind()

	unbound := make([]m19Unbound, 0, 4)
	for _, item := range analysis.items {
		if item.written || r.schema.owned[item.table] == "" || analysis.isBound(item) || analysis.excused(item) {
			continue
		}
		if _, rule := analysis.spanAt(item.location); rule {
			continue // a read the rule makes of its own, inside the fragment it renders
		}
		unbound = append(unbound, m19Unbound{table: item.table, alias: item.alias, function: body.function, location: item.location})
	}
	for _, call := range analysis.calls {
		_, rule := analysis.spanAt(call.location)
		bound := make([]bool, len(call.args))
		for index, arg := range call.args {
			bound[index] = analysis.valueHolds(arg)
		}
		read, err := r.function(call.name, rule, bound)
		if err != nil {
			return nil, err
		}
		unbound = append(unbound, read...)
	}
	return unbound, nil
}

// function reads the body of every function of this name, called with the arguments bound says
// a clause bound, and names the project-owned tables it reads that nothing binds. A function the
// rule itself calls (rule) reads for the rule, as the fragment that calls it does: its body is
// read all the same, so a body the census cannot read is an error rather than a silence.
func (r *m19Reader) function(name string, rule bool, bound []bool) ([]m19Unbound, error) {
	if r.reading[name] {
		return nil, nil
	}
	r.reading[name] = true
	defer delete(r.reading, name)
	var unbound []m19Unbound
	for _, held := range r.schema.functions[name] {
		statements, locals, err := m19FunctionStatements(held)
		if err != nil {
			return nil, err
		}
		if statements == nil {
			continue // a function of a language with no SQL to read
		}
		r.bodies[name] = true
		arguments, boundHere, err := m19BindLocals(held.arguments, bound, locals)
		if err != nil {
			return nil, fmt.Errorf("read the locals of %s: %w", name, err)
		}
		for _, statement := range statements {
			read, err := r.statement(statement, m19Body{function: name, arguments: arguments, bound: boundHere})
			if err != nil {
				return nil, fmt.Errorf("read the body of %s: %w", name, err)
			}
			if !rule {
				unbound = append(unbound, read...)
			}
		}
	}
	return unbound, nil
}

// m19BindLocals is the names a PL/pgSQL body reads its values by - its arguments, then its locals
// - and which of them hold a value a clause bound: an argument its call binds, and a local every
// assignment of which, its default included, reads only such values (`v_owner text :=
// split_part(p_id, '#', 1)`), to a fixed point. A local assigned a constant holds no key.
func m19BindLocals(arguments []string, bound []bool, locals map[string][]string) ([]string, []bool, error) {
	names := slices.Clone(arguments)
	held := make([]bool, len(names))
	copy(held, bound)
	order := slices.Sorted(maps.Keys(locals))
	names = append(names, order...)
	held = append(held, make([]bool, len(order))...)
	reads := make(map[string][][]string, len(order))
	for _, local := range order {
		for _, expression := range locals[local] {
			read, err := m19ExpressionNames(expression, names)
			if err != nil {
				return nil, nil, err
			}
			reads[local] = append(reads[local], read)
		}
	}
	isHeld := func(name string) bool { return held[slices.Index(names, name)] }
	for grew := true; grew; {
		grew = false
		for index, local := range order {
			at := len(arguments) + index
			if held[at] {
				continue
			}
			all := len(reads[local]) > 0 && !slices.ContainsFunc(reads[local], func(read []string) bool {
				return len(read) == 0 || slices.ContainsFunc(read, func(name string) bool { return !isHeld(name) })
			})
			if all {
				held[at], grew = true, true
			}
		}
	}
	return names, held, nil
}

// m19ExpressionNames is the names of a body's values one expression reads, its arguments' and
// locals' (by name, or by $n), and every other column it reads as itself, which no binding holds.
func m19ExpressionNames(expression string, names []string) ([]string, error) {
	tree, err := pgparser.ParseToJSON(expression)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", expression, err)
	}
	var parsed any
	if err := json.Unmarshal([]byte(tree), &parsed); err != nil {
		return nil, err
	}
	var read []string
	var walk func(node any)
	walk = func(node any) {
		switch held := node.(type) {
		case []any:
			for _, item := range held {
				walk(item)
			}
		case map[string]any:
			kind, body, wrapped := m19Node(node)
			switch {
			case wrapped && kind == "ColumnRef":
				fields := m19List(body["fields"])
				if len(fields) == 1 {
					read = append(read, m19Text(fields[0]))
				} else {
					read = append(read, "") // a record's field or a table's column: no value of the body's
				}
				return
			case wrapped && kind == "ParamRef":
				if number, numbered := body["number"].(float64); numbered && int(number) <= len(names) {
					read = append(read, names[int(number)-1])
				} else {
					read = append(read, "")
				}
				return
			}
			for _, field := range held {
				walk(field)
			}
		}
	}
	walk(parsed)
	for index, name := range read {
		if !slices.Contains(names, name) {
			read[index] = "" // a name of no argument or local reads as unbound
		}
	}
	return read, nil
}

// m19FunctionStatements is the SQL of one function's body: the body itself for a SQL function, and
// every query of a PL/pgSQL function's statements, read by the same parser, with the expressions
// each of its locals is assigned (m19PlpgsqlLocals). A function of any other language reads no row
// of a table through SQL and answers none.
func m19FunctionStatements(held m19Function) ([]string, map[string][]string, error) {
	switch held.language {
	case "sql":
		return []string{held.source}, nil, nil
	case "plpgsql":
		tree, err := pgparser.ParsePlPgSqlToJSON(held.definition)
		if err != nil {
			return nil, nil, fmt.Errorf("parse the body of %s: %w", held.name, err)
		}
		var parsed any
		if err := json.Unmarshal([]byte(tree), &parsed); err != nil {
			return nil, nil, fmt.Errorf("read the body of %s: %w", held.name, err)
		}
		var queries []string
		m19PlpgsqlQueries(parsed, &queries)
		return queries, m19PlpgsqlLocals(parsed, held.arguments), nil
	default:
		return nil, nil, nil
	}
}

// m19PlpgsqlLocals is every local a PL/pgSQL body declares, with the select of each expression it
// is assigned: its default (PLpgSQL_var's default_val) and each assignment (PLpgSQL_stmt_assign,
// whose varno is the local's place among the body's datums). The body's arguments and its records
// (new, old, found) are no locals of it.
func m19PlpgsqlLocals(tree any, arguments []string) map[string][]string {
	locals := map[string][]string{}
	var datums []string
	var collect func(node any)
	collect = func(node any) {
		switch held := node.(type) {
		case []any:
			for _, item := range held {
				collect(item)
			}
		case map[string]any:
			kind, body, wrapped := m19Node(node)
			if wrapped && kind == "PLpgSQL_function" {
				for _, datum := range m19List(body["datums"]) {
					datumKind, fields, _ := m19Node(datum)
					name, _ := fields["refname"].(string)
					datums = append(datums, name)
					if datumKind != "PLpgSQL_var" || name == "found" || slices.Contains(arguments, name) {
						continue
					}
					if _, declared := locals[name]; !declared {
						locals[name] = nil
					}
					if query, held := m19PlpgsqlExpression(fields["default_val"]); held {
						locals[name] = append(locals[name], query)
					}
				}
			}
			if wrapped && kind == "PLpgSQL_stmt_assign" {
				if varno, numbered := body["varno"].(float64); numbered && int(varno) < len(datums) {
					name := datums[int(varno)]
					if _, local := locals[name]; local {
						if query, held := m19PlpgsqlExpression(body["expr"]); held {
							locals[name] = append(locals[name], query)
						}
					}
				}
			}
			for _, field := range held {
				collect(field)
			}
		}
	}
	collect(tree)
	return locals
}

// m19PlpgsqlExpression is the select of one PLpgSQL_expr's expression, its assignment target cut.
func m19PlpgsqlExpression(node any) (string, bool) {
	kind, body, wrapped := m19Node(node)
	if !wrapped || kind != "PLpgSQL_expr" {
		return "", false
	}
	query, text := body["query"].(string)
	if !text || strings.TrimSpace(query) == "" {
		return "", false
	}
	if _, value, assigns := strings.Cut(query, ":="); assigns {
		query = value
	}
	return "select " + query, true
}

// m19PlpgsqlQueries appends every query of a PL/pgSQL parse tree: the SQL of each statement, of
// each condition and of each default, which libpg_query writes as a PLpgSQL_expr. A query the
// body spells as an expression, which every parse mode but the statement's own marks, is read as
// the select of that expression: what PL/pgSQL runs for it.
func m19PlpgsqlQueries(node any, found *[]string) {
	switch node := node.(type) {
	case map[string]any:
		for kind, body := range node {
			expression, held := body.(map[string]any)
			if kind != "PLpgSQL_expr" || !held {
				m19PlpgsqlQueries(body, found)
				continue
			}
			query, text := expression["query"].(string)
			if !text || strings.TrimSpace(query) == "" {
				continue
			}
			if mode, _ := expression["parseMode"].(float64); mode != 0 {
				// An expression, or an assignment (`target := expression`): PL/pgSQL runs the
				// select of the expression.
				if _, value, assigns := strings.Cut(query, ":="); assigns {
					query = value
				}
				query = "select " + query
			}
			*found = append(*found, query)
		}
	case []any:
		for _, held := range node {
			m19PlpgsqlQueries(held, found)
		}
	}
}

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	pgparser "github.com/wasilibs/go-pgquery/parser"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// issueTreeMigrationName is the name every migration of dispatch://INFRA-1253 takes,
// <timestamp>_issue_tree_<what>.up.sql, so the test finds the set by its name and a migration
// that branch adds joins the set with no edit here.
var issueTreeMigrationName = regexp.MustCompile(`^[0-9]{14}_issue_tree_[a-z0-9_]+\.up\.sql$`)

// issueTreeTableLimit is the table limit the census of the set is taken under: one byte, as
// census_test.go's size tests take it. Every table that exists in the database is above it, so a
// migration that touches one passes only where its statements are of a kind exempt from the limit
// (or it is a marked concurrent index build), which holds whatever size the table reaches in a
// deployment. The one exception is a type migration's insert into graph_nodes and graph_tuples
// (typeTables), which no limit of one byte admits: on those two tables alone, a second census at
// pgmigrate.CensusTableLimit decides.
const issueTreeTableLimit = 1

// staysWhole are the names no statement of the set may give (R17, R19): artifact_versions, which
// no migration of this child touches at all, and document_search, the
// artifact_versions_document_search trigger and its function, the asks_approval_kind_check
// constraint and the asks_normalize_approval_documents function, which stay through this release
// for the root's own pull request to drop.
var staysWhole = []string{
	"artifact_versions", "document_search", "artifact_versions_document_search",
	"asks_approval_kind_check", "asks_normalize_approval_documents",
}

// keepsItsShape are the tables that take no alter table, no drop table and no truncate from this
// child (R19).
var keepsItsShape = []string{
	"doc_updates", "doc_snapshots", "doc_checkpoints", "document_block_texts", "embeddings",
	"check_results", "artifact_versions", "artifact_reviews", "document_blocks",
}

// createdNew are the tables this child creates: document_rooms and version_renders (R19),
// issue_trees, which holds each issue's one tree, issue_room_blocks, the projection of an issue
// room's block's in_document, node_search, the search rows of a node, issue_views, the
// per-issue record of how an issue was converted and whether its spec is its own (R23),
// issue_head_nodes and issue_head_marks, the head projection of each issue's tree and the marks of
// the issues whose rows the head read reads (R8), issue_query_marks, the issues whose queries a
// write left owed a version (R7), and artifact_review_carries, the source of each carried review
// (R27). Each is created new, by a create table of its own that is the first statement of the set
// to name it, and no statement of the set fills it from a stored row. A table no migration of the
// set names is no finding.
var createdNew = []string{"document_rooms", "version_renders", "issue_trees", "issue_room_blocks", "node_search", "issue_views",
	"issue_head_nodes", "issue_head_marks", "issue_query_marks", "artifact_review_carries"}

// columnAdd is the one statement a table of columnAdds takes from this child: an alter table of it
// that adds the columns named, each of the type Postgres's parser names (its typeName's names) and
// with no constraint, so no default, no not null and no foreign key, and does nothing else.
// catalogOnlyAlterTable exempts that statement from the census's table limit, because it rewrites
// no row.
type columnAdd struct {
	// what names the columns in a finding.
	what    string
	columns map[string][]string
}

// columnAdds are the tables whose one statement from this child is a columnAdd, each taking it
// once: issue_links, which takes the node column pair, and document_block_versions, which takes
// the version and the rule of the block index's pass that last stated each document.
var columnAdds = map[string]columnAdd{
	"issue_links": {
		what:    "the nullable node column pair, node_kind and node_address",
		columns: map[string][]string{"node_kind": {"text"}, "node_address": {"text"}},
	},
	"document_block_versions": {
		what:    "the nullable integer columns stated_version and stated_rule",
		columns: map[string][]string{"stated_version": {"pg_catalog", "int4"}, "stated_rule": {"pg_catalog", "int4"}},
	},
}

// The statement forms the test reads, each over a statement as pgmigrate.CensusStatements gives
// it: its words one space apart, and a DO block's body read as statements, so a form is found
// anywhere in the statement, as TouchedTables finds its own.
var (
	alterTableForm    = regexp.MustCompile(`(?i)\balter table (?:if exists )?(?:only )?([\w."]+)`)
	dropTableForm     = regexp.MustCompile(`(?i)\bdrop table (?:if exists )?([\w."]+(?: ?, ?[\w."]+)*)`)
	truncateTableForm = regexp.MustCompile(`(?i)\btruncate (?:table )?(?:only )?([\w."]+(?: ?, ?[\w."]+)*)`)
	identifierForm    = regexp.MustCompile(`"(?:[^"]|"")*"|[A-Za-z_][A-Za-z0-9_$]*`)
)

// formTables are the tables a statement form names in statement, each as TouchedTables names a
// table: lowercased, unquoted, and without a public. qualifier.
func formTables(form *regexp.Regexp, statement string) []string {
	var tables []string
	for _, match := range form.FindAllStringSubmatch(statement, -1) {
		for _, raw := range strings.Split(match[1], ",") {
			name := strings.TrimPrefix(strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), `"`, "")), "public.")
			if name != "" {
				tables = append(tables, name)
			}
		}
	}
	return tables
}

// statementNames are the names a statement gives, each part of a qualified name on its own,
// lowercased and unquoted. The statement holds no comment, no literal and no function body
// (pgmigrate.CensusStatements), so a name in one of those is none of these.
func statementNames(statement string) map[string]bool {
	names := map[string]bool{}
	for _, word := range identifierForm.FindAllString(statement, -1) {
		if strings.HasPrefix(word, `"`) {
			word = strings.ReplaceAll(strings.Trim(word, `"`), `""`, `"`)
		}
		names[strings.ToLower(word)] = true
	}
	return names
}

// sourceStatement is one statement of a migration as Postgres's own parser reads it: its text as
// the migration writes it, its literals kept, and its parse node's type and fields.
type sourceStatement struct {
	text, node string
	body       json.RawMessage
}

// parsedStatements are the statements Postgres's own parser reads sql as, in order, ok false where
// the parser refuses the text or gives a tree the test cannot read.
func parsedStatements(sql string) (statements []sourceStatement, ok bool) {
	tree, err := pgparser.ParseToJSON(sql)
	if err != nil {
		return nil, false
	}
	var parsed struct {
		Stmts []struct {
			Stmt map[string]json.RawMessage `json:"stmt"`
			// The parser leaves a zero out: the statement at the start of the text has no
			// stmt_location, and a last one that runs to the text's end no stmt_len.
			Location int `json:"stmt_location"`
			Length   int `json:"stmt_len"`
		} `json:"stmts"`
	}
	if json.Unmarshal([]byte(tree), &parsed) != nil {
		return nil, false
	}
	for _, s := range parsed.Stmts {
		end := len(sql)
		if s.Length > 0 {
			end = s.Location + s.Length
		}
		if len(s.Stmt) != 1 || s.Location < 0 || s.Location > end || end > len(sql) {
			return nil, false
		}
		statement := sourceStatement{text: sql[s.Location:end]}
		for node, body := range s.Stmt {
			statement.node, statement.body = node, body
		}
		statements = append(statements, statement)
	}
	return statements, true
}

// parsedStatement is the one statement Postgres's own parser reads statement as: its node type
// and its fields, ok false where the text is not one statement the parser accepts, as a
// statement of a DO block's body is not.
func parsedStatement(statement string) (node string, body json.RawMessage, ok bool) {
	statements, ok := parsedStatements(statement)
	if !ok || len(statements) != 1 {
		return "", nil, false
	}
	return statements[0].node, statements[0].body, true
}

// sourceText is a statement's text as a finding quotes it: the comment lines before it left out,
// and its words one space apart.
func sourceText(text string) string {
	lines := strings.Split(text, "\n")
	for len(lines) > 0 {
		if line := strings.TrimSpace(lines[0]); line != "" && !strings.HasPrefix(line, "--") {
			break
		}
		lines = lines[1:]
	}
	return strings.Join(strings.Fields(strings.Join(lines, "\n")), " ")
}

// relationOf reads the relation a statement's parse node names, as the parser gives it.
type relationOf struct {
	Relation struct {
		Schema string `json:"schemaname"`
		Name   string `json:"relname"`
	} `json:"relation"`
}

func (r relationOf) is(table string) bool {
	return r.Relation.Name == table && (r.Relation.Schema == "" || r.Relation.Schema == "public")
}

// addsColumns says whether statement is add, an alter table of table that adds add's columns, each
// of its type with no constraint, and does nothing else.
func addsColumns(statement, table string, add columnAdd) bool {
	node, body, ok := parsedStatement(statement)
	if !ok || node != "AlterTableStmt" {
		return false
	}
	var alter struct {
		relationOf
		Objtype string `json:"objtype"`
		Cmds    []struct {
			Command struct {
				Subtype string `json:"subtype"`
				Def     struct {
					Column *struct {
						Name     string `json:"colname"`
						TypeName struct {
							Names []struct {
								String struct {
									Value string `json:"sval"`
								} `json:"String"`
							} `json:"names"`
							ArrayBounds json.RawMessage `json:"arrayBounds"`
						} `json:"typeName"`
						Constraints json.RawMessage `json:"constraints"`
					} `json:"ColumnDef"`
				} `json:"def"`
			} `json:"AlterTableCmd"`
		} `json:"cmds"`
	}
	if json.Unmarshal(body, &alter) != nil || !alter.is(table) || alter.Objtype != "OBJECT_TABLE" || len(alter.Cmds) != len(add.columns) {
		return false
	}
	added := map[string]bool{}
	for _, item := range alter.Cmds {
		column := item.Command.Def.Column
		if item.Command.Subtype != "AT_AddColumn" || column == nil || len(column.Constraints) != 0 || len(column.TypeName.ArrayBounds) != 0 {
			return false
		}
		names := make([]string, len(column.TypeName.Names))
		for index, name := range column.TypeName.Names {
			names[index] = name.String.Value
		}
		want, named := add.columns[column.Name]
		if !named || added[column.Name] || !slices.Equal(names, want) {
			return false
		}
		added[column.Name] = true
	}
	return true
}

// createsNew says whether statement creates table new: a plain create table of it, which holds no
// row, and neither a create table if not exists, which does nothing over a table that stands, nor
// a create table … as, which fills the table with the rows its query reads, nor a table that
// inherits from or is a partition of a table that stands.
func createsNew(statement, table string) bool {
	node, body, ok := parsedStatement(statement)
	if !ok || node != "CreateStmt" {
		return false
	}
	var create struct {
		relationOf
		IfNotExists  bool            `json:"if_not_exists"`
		InhRelations json.RawMessage `json:"inhRelations"`
		Partbound    json.RawMessage `json:"partbound"`
	}
	return json.Unmarshal(body, &create) == nil && create.is(table) && !create.IfNotExists &&
		len(create.InhRelations) == 0 && len(create.Partbound) == 0
}

// fillsForm finds, for each table of createdNew, a statement that writes rows into it: an insert
// into it, an update of it or a delete from it.
var fillsForm = func() map[string]*regexp.Regexp {
	forms := make(map[string]*regexp.Regexp, len(createdNew))
	for _, table := range createdNew {
		forms[table] = regexp.MustCompile(`(?i)\b(?:insert into |update (?:only )?|delete from (?:only )?)(?:public\.)?"?` +
			regexp.QuoteMeta(table) + `"?\b`)
	}
	return forms
}()

// typeTables are the two tables a type migration writes (R20): it appends the schema's own
// supertag and attribute nodes to graph_nodes and their tuples to graph_tuples, as every type
// migration does. A migration of the set with a statement on either is a type migration.
var typeTables = []string{"graph_nodes", "graph_tuples"}

// typeRule is what a type migration's statements on typeTables may be (R20).
const typeRule = "a type migration's statements on graph_tuples are an insert alone, and on graph_nodes an insert " +
	"and an update whose where names kind = 'supertag' or kind = 'attribute' (R20)"

// referencesForm finds the table a foreign key references, which TouchedTables names beside the
// table a statement acts on.
var referencesForm = regexp.MustCompile(`(?i)\breferences (?:only )?[\w."]+`)

// actsOn says whether statement, as pgmigrate.CensusStatements gives it, acts on table:
// TouchedTables names table in it other than as the table a foreign key references. A key that a
// new table or an added column declares to graph_nodes is a statement on its own table, which
// writes no row of graph_nodes.
func actsOn(statement, table string) bool {
	return slices.Contains(pgmigrate.TouchedTables(referencesForm.ReplaceAllString(statement, "")), table)
}

// typeStatementAdmitted says whether a statement Postgres's parser reads as node and body is one a
// type migration may make on table (typeRule). On graph_tuples that is an insert alone, which
// appends tuples and rewrites none, so neither one with an on conflict do update nor one beside a
// with that writes. On graph_nodes it is an insert, and an update typeNodesOnly admits. Any other
// statement on either table is refused: a delete, an alter table, a truncate, an update of
// graph_tuples, a DO block.
func typeStatementAdmitted(table, node string, body json.RawMessage) bool {
	var statement struct {
		relationOf
		With struct {
			CTEs []struct {
				CTE struct {
					Query map[string]json.RawMessage `json:"ctequery"`
				} `json:"CommonTableExpr"`
			} `json:"ctes"`
		} `json:"withClause"`
		OnConflict struct {
			Action string `json:"action"`
		} `json:"onConflictClause"`
	}
	if json.Unmarshal(body, &statement) != nil || !statement.is(table) {
		return false
	}
	for _, cte := range statement.With.CTEs {
		if _, reads := cte.CTE.Query["SelectStmt"]; !reads || len(cte.CTE.Query) != 1 {
			return false
		}
	}
	switch node {
	case "InsertStmt":
		return table == "graph_nodes" || statement.OnConflict.Action != "ONCONFLICT_UPDATE"
	case "UpdateStmt":
		return table == "graph_nodes" && typeNodesOnly(body)
	}
	return false
}

// typeNodesOnly is the rule for an update of graph_nodes, its UpdateStmt's fields body: its where,
// read as the conjunction of its terms, holds kind = 'supertag' or kind = 'attribute' on
// graph_nodes' own kind column, bare or under the name the update gives the table. So the update
// moves type nodes alone, as 20261113230000_graph_supertags_check_ci_check moves the schema
// version a server installs from a type node's attributes.
func typeNodesOnly(body json.RawMessage) bool {
	var update struct {
		Relation struct {
			Name  string `json:"relname"`
			Alias struct {
				Name string `json:"aliasname"`
			} `json:"alias"`
		} `json:"relation"`
		Where json.RawMessage `json:"whereClause"`
	}
	if json.Unmarshal(body, &update) != nil {
		return false
	}
	own := update.Relation.Name
	if update.Relation.Alias.Name != "" {
		own = update.Relation.Alias.Name
	}
	return restrictsToTypeKind(update.Where, own)
}

// restrictsToTypeKind says whether expr, a where clause's parse node, holds kind = 'supertag' or
// kind = 'attribute' on own's kind column, as the expression itself or as a term of an and.
func restrictsToTypeKind(expr json.RawMessage, own string) bool {
	type text struct {
		Value string `json:"sval"`
	}
	var node struct {
		And *struct {
			Op   string            `json:"boolop"`
			Args []json.RawMessage `json:"args"`
		} `json:"BoolExpr"`
		Op *struct {
			Kind string `json:"kind"`
			Name []struct {
				String text `json:"String"`
			} `json:"name"`
			Left  json.RawMessage `json:"lexpr"`
			Right json.RawMessage `json:"rexpr"`
		} `json:"A_Expr"`
	}
	if len(expr) == 0 || json.Unmarshal(expr, &node) != nil {
		return false
	}
	if node.And != nil {
		return node.And.Op == "AND_EXPR" && slices.ContainsFunc(node.And.Args, func(term json.RawMessage) bool {
			return restrictsToTypeKind(term, own)
		})
	}
	if node.Op == nil || node.Op.Kind != "AEXPR_OP" || len(node.Op.Name) != 1 || node.Op.Name[0].String.Value != "=" {
		return false
	}
	kindColumn := func(side json.RawMessage) bool {
		var column struct {
			Ref *struct {
				Fields []struct {
					String *text `json:"String"`
				} `json:"fields"`
			} `json:"ColumnRef"`
		}
		if json.Unmarshal(side, &column) != nil || column.Ref == nil {
			return false
		}
		var parts []string
		for _, field := range column.Ref.Fields {
			if field.String == nil {
				return false
			}
			parts = append(parts, field.String.Value)
		}
		return slices.Equal(parts, []string{"kind"}) || slices.Equal(parts, []string{own, "kind"})
	}
	typeKind := func(side json.RawMessage) bool {
		var constant struct {
			Const *struct {
				String *text `json:"sval"`
			} `json:"A_Const"`
		}
		return json.Unmarshal(side, &constant) == nil && constant.Const != nil && constant.Const.String != nil &&
			(constant.Const.String.Value == "supertag" || constant.Const.String.Value == "attribute")
	}
	return kindColumn(node.Op.Left) && typeKind(node.Op.Right) || kindColumn(node.Op.Right) && typeKind(node.Op.Left)
}

// readTypeStatements reads migration's statements on typeTables with their literals, as Postgres's
// parser reads each, and answers whether it has one, which makes it a type migration, with what
// those statements break (typeRule). A migration the parser does not read is read as
// pgmigrate.CensusStatements reads it, and a statement of it on either table is a finding, since
// the test cannot read what that statement is.
func readTypeStatements(migration pgmigrate.Migration) (typeMigration bool, findings []string) {
	statements, parsed := parsedStatements(migration.SQL)
	if !parsed {
		for _, statement := range pgmigrate.CensusStatements(migration.SQL) {
			for _, table := range typeTables {
				if actsOn(statement, table) {
					typeMigration = true
					findings = append(findings, fmt.Sprintf("%s acts on %s in the statement %q, which Postgres's parser does not read, so the test cannot hold it to the rule: %s",
						migration.Name, table, statement, typeRule))
				}
			}
		}
		return typeMigration, findings
	}
	for _, statement := range statements {
		// A create trigger writes no row of its table, and runs only on later writes, never on the
		// rows already there (pgmigrate's catalogOnlyStatements): the head projection's trigger on
		// graph_tuples and graph_nodes (20261123180100_issue_tree_head) makes no type migration.
		if statement.node == "CreateTrigStmt" {
			continue
		}
		read := pgmigrate.CensusStatements(statement.text)
		for _, table := range typeTables {
			if !slices.ContainsFunc(read, func(s string) bool { return actsOn(s, table) }) {
				continue
			}
			typeMigration = true
			if !typeStatementAdmitted(table, statement.node, statement.body) {
				findings = append(findings, fmt.Sprintf("%s acts on %s in the statement %q: %s",
					migration.Name, table, sourceText(statement.text), typeRule))
			}
		}
	}
	return typeMigration, findings
}

// staysWholeBecause says why no migration of this child may touch or name name, one of staysWhole.
func staysWholeBecause(name string) string {
	if name == "artifact_versions" {
		return "no migration of this child touches artifact_versions at all (R19)"
	}
	return name + " stays through this release, and the root's own pull request drops it (R17)"
}

// issueTreeReading is what the test reads of the set from the migrations' text alone.
type issueTreeReading struct {
	// touched are the tables pgmigrate.TouchedTables reads the set's migrations to touch, and
	// shaped the tables whose shape a migration of the set changes: the tables an alter table or a
	// drop table names. Both are sorted, each table once.
	touched, shaped []string
	// created names, for each table of createdNew the set creates, the migration that creates it.
	created map[string]string
	// typeMigrations are the migrations of the set with a statement on graph_nodes or graph_tuples
	// (readTypeStatements).
	typeMigrations map[string]bool
	// findings are what the set breaks, each naming the migration and the statement.
	findings []string
}

// readIssueTreeMigrations reads every statement of every migration of the set as
// pgmigrate.TouchedTables reads it (pgmigrate.CensusStatements): a function's body is no
// statement's, so a function a migration restates whose body reads artifact_versions names that
// table in no statement read here, and a DO block's body is read as statements.
func readIssueTreeMigrations(migrations []pgmigrate.Migration) issueTreeReading {
	reading := issueTreeReading{created: map[string]string{}, typeMigrations: map[string]bool{}}
	finding := func(format string, args ...any) {
		reading.findings = append(reading.findings, fmt.Sprintf(format, args...))
	}
	named := map[string]bool{}
	adds := map[string][]string{}
	for _, migration := range migrations {
		reading.touched = append(reading.touched, pgmigrate.TouchedTables(migration.SQL)...)
		typeMigration, typeFindings := readTypeStatements(migration)
		if typeMigration {
			reading.typeMigrations[migration.Name] = true
		}
		reading.findings = append(reading.findings, typeFindings...)
		for _, statement := range pgmigrate.CensusStatements(migration.SQL) {
			names := statementNames(statement)
			touched := pgmigrate.TouchedTables(statement)
			for _, name := range staysWhole {
				if !names[name] {
					continue
				}
				how := "names"
				if slices.Contains(touched, name) {
					how = "touches"
				}
				finding("%s %s %s in the statement %q: %s", migration.Name, how, name, statement, staysWholeBecause(name))
			}
			for _, form := range []struct {
				kind    string
				pattern *regexp.Regexp
			}{{"alter table", alterTableForm}, {"drop table", dropTableForm}, {"truncate", truncateTableForm}} {
				for _, table := range formTables(form.pattern, statement) {
					if form.kind != "truncate" {
						reading.shaped = append(reading.shaped, table)
					}
					if slices.Contains(keepsItsShape, table) {
						finding("%s takes a %s of %s in the statement %q: %s takes no alter table, no drop table and no truncate from this child (R19)",
							migration.Name, form.kind, table, statement, table)
					}
				}
			}
			for _, table := range slices.Sorted(maps.Keys(columnAdds)) {
				if !slices.Contains(touched, table) {
					continue
				}
				if addsColumns(statement, table, columnAdds[table]) {
					adds[table] = append(adds[table], migration.Name)
				} else {
					finding("%s touches %s in the statement %q: the one statement %s takes from this child is the alter table that adds %s, with no default",
						migration.Name, table, statement, table, columnAdds[table].what)
				}
			}
			for _, table := range createdNew {
				if !names[table] {
					continue
				}
				if !named[table] {
					named[table] = true
					if createsNew(statement, table) {
						reading.created[table] = migration.Name
					} else {
						finding("%s names %s first in the statement %q: %s is a table this child creates new, by a create table of its own that holds no row (R19)",
							migration.Name, table, statement, table)
					}
				}
				if fillsForm[table].MatchString(statement) {
					finding("%s writes rows into %s in the statement %q: %s is created new and filled from no stored row, so no migration reads or rewrites one into it (R19)",
						migration.Name, table, statement, table)
				}
			}
		}
	}
	for _, table := range slices.Sorted(maps.Keys(columnAdds)) {
		switch held := adds[table]; {
		case len(held) == 0:
			finding("no migration of this child adds %s to %s, with no default: that alter table is the one statement %s takes from this child",
				columnAdds[table].what, table, table)
		case len(held) > 1:
			finding("%s takes one alter table from this child, and %s each add %s: a second is an alter table the first already made",
				table, strings.Join(held, ", "), columnAdds[table].what)
		}
	}
	slices.Sort(reading.touched)
	reading.touched = slices.Compact(reading.touched)
	slices.Sort(reading.shaped)
	reading.shaped = slices.Compact(reading.shaped)
	return reading
}

// issueTreeMigrations are the migrations of set named <timestamp>_issue_tree_<what>, in version
// order.
func issueTreeMigrations(set []pgmigrate.Migration) []pgmigrate.Migration {
	var ours []pgmigrate.Migration
	for _, migration := range set {
		if issueTreeMigrationName.MatchString(migration.Name) {
			ours = append(ours, migration)
		}
	}
	return ours
}

// withoutMigrations is the migrations directory of fsys without the files of ours: each one's
// .up.sql, its .census.sql and its .down.sql.
func withoutMigrations(t *testing.T, fsys fs.FS, ours []pgmigrate.Migration) fstest.MapFS {
	t.Helper()
	left := map[string]bool{}
	for _, migration := range ours {
		left[strings.TrimSuffix(migration.Name, ".up.sql")] = true
	}
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	set := fstest.MapFS{}
	for _, entry := range entries {
		stem := entry.Name()
		for _, suffix := range []string{".up.sql", ".census.sql", ".down.sql"} {
			stem = strings.TrimSuffix(stem, suffix)
		}
		if left[stem] {
			continue
		}
		data, err := fs.ReadFile(fsys, "migrations/"+entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		set["migrations/"+entry.Name()] = &fstest.MapFile{Data: data}
	}
	return set
}

// tableVerdict is what the census said of one table a pending migration touches, under the limit.
func tableVerdict(entry pgmigrate.MigrationCensus, table pgmigrate.TableCensus, limit int64) string {
	switch {
	case !table.Exists:
		return "not in the database before the set applies"
	case table.Through != "":
		return fmt.Sprintf("%d bytes, reached through a foreign key from %s, which no limit applies to", table.Bytes, table.Through)
	case table.Unread != "":
		return "not read: " + table.Unread
	case table.Bytes > limit && entry.TableSizeExemption != "":
		return fmt.Sprintf("%d bytes, above the %d byte limit, exempt from it: the migration's statements are only %s", table.Bytes, limit, entry.TableSizeExemption)
	case table.Bytes > limit && entry.ConcurrentIndex:
		return fmt.Sprintf("%d bytes, above the %d byte limit, exempt from it as a marked concurrent index build", table.Bytes, limit)
	case table.Bytes > limit:
		return fmt.Sprintf("%d bytes, above the %d byte limit, and the migration is not exempt from it: refused", table.Bytes, limit)
	default:
		return fmt.Sprintf("%d bytes and %d rows, within the %d byte limit", table.Bytes, table.Rows, limit)
	}
}

// migratedBeforeTheSet is a fresh database that every migration of fsys but ours has reached, so
// that ours are exactly the pending ones, and its URL.
func migratedBeforeTheSet(t *testing.T, fsys fs.FS, ours []pgmigrate.Migration) string {
	t.Helper()
	store := openEmptyTestStore(t)
	if err := store.migrate(context.Background(), withoutMigrations(t, fsys, ours)); err != nil {
		t.Fatalf("migrate every migration but the issue tree's: %v", err)
	}
	return store.Pool.Config().ConnString()
}

// censusOfIssueTreeMigrations takes the pre-deploy census (pgmigrate.Census) of ours, the set's
// migrations, over url, a database migratedBeforeTheSet made, with the table limit at limit. The
// census writes nothing, so two of them over one database read the same tables.
func censusOfIssueTreeMigrations(t *testing.T, url string, fsys fs.FS, ours []pgmigrate.Migration, limit int64) *pgmigrate.Report {
	t.Helper()
	report, err := census(context.Background(), url, fsys, pgmigrate.CensusOptions{TableLimit: limit})
	if err != nil {
		t.Fatalf("census of the issue tree's migrations under a %d byte table limit: %v", limit, err)
	}
	var pending, want []string
	for _, entry := range report.Pending {
		pending = append(pending, entry.Migration.Name)
	}
	for _, migration := range ours {
		want = append(want, migration.Name)
	}
	if !slices.Equal(pending, want) {
		t.Fatalf("the census holds %v pending, want the issue tree's migrations %v alone:\n%s", pending, want, reportText(report))
	}
	return report
}

// sizeRefusalForm is how the census words a refusal on the size of a table a migration touches
// (pgmigrate's censusTable), with the table it names.
var sizeRefusalForm = regexp.MustCompile(`^(\S+) is [^,]+, above the .+ a table a migration touches may be`)

// sizeRefusedTable is the table a census refusal refuses on its size, or "" for a refusal of
// another kind: a lock holder, a table the census could not read, a census that counted rows.
func sizeRefusedTable(refusal string) string {
	if match := sizeRefusalForm.FindStringSubmatch(refusal); match != nil {
		return match[1]
	}
	return ""
}

// requireTheIssueTreeMigrationsKeepToTheLimit holds the migrations of fsys named
// <timestamp>_issue_tree_<what> to R19 and R20, and prints one `TOUCHED: <table>` line for each
// table the set touches (pgmigrate.TouchedTables), one `SHAPE: <table>` line for each table whose
// shape a migration of the set changes, and one `SIZE: <table> <bytes> <limit>` line for
// each of graph_nodes and graph_tuples the set touches, with the size the census at
// pgmigrate.CensusTableLimit read, each at the start of its own line of the test's output, where
// the check's run reads them beside the pull request body.
func requireTheIssueTreeMigrationsKeepToTheLimit(t *testing.T, fsys fs.FS) {
	t.Helper()
	set, err := pgmigrate.Load(fsys, "migrations")
	if err != nil {
		t.Fatalf("load the migrations: %v", err)
	}
	ours := issueTreeMigrations(set)
	if len(ours) == 0 {
		t.Fatal("no migration is named <timestamp>_issue_tree_<what>.up.sql: the marker migration stands in every set of this child, so the set read here is the wrong one")
	}
	for _, migration := range ours {
		t.Logf("issue tree migration: %s", migration.Name)
	}

	reading := readIssueTreeMigrations(ours)
	for _, table := range reading.touched {
		fmt.Printf("TOUCHED: %s\n", table)
	}
	for _, table := range reading.shaped {
		fmt.Printf("SHAPE: %s\n", table)
	}
	t.Logf("census_table_limit: %d (pgmigrate.CensusTableLimit); the census below is taken under %d, and again under %d for graph_nodes and graph_tuples",
		pgmigrate.CensusTableLimit, issueTreeTableLimit, pgmigrate.CensusTableLimit)
	for _, table := range createdNew {
		if migration, ok := reading.created[table]; ok {
			t.Logf("%s: created new by %s", table, migration)
		} else {
			t.Logf("%s: no migration of this child creates it", table)
		}
	}
	for _, finding := range reading.findings {
		t.Error(finding)
	}

	url := migratedBeforeTheSet(t, fsys, ours)
	report := censusOfIssueTreeMigrations(t, url, fsys, ours, issueTreeTableLimit)
	// failed holds each refusal of the census under one byte that failed the test, keyed by its
	// migration, so the census at pgmigrate.CensusTableLimit reports only what is left.
	failed := map[string]bool{}
	for _, entry := range report.Pending {
		named := pgmigrate.TouchedTables(entry.Migration.SQL)
		read := map[string]bool{}
		for _, table := range entry.Tables {
			read[table.Name] = true
			t.Logf("census: %s %s: %s", entry.Migration.Name, table.Name, tableVerdict(entry, table, issueTreeTableLimit))
			// The census finds a table through a statement that names it, through an index it
			// drops or alters (pg_index), and through a foreign key, so a stays-whole table reached
			// with no statement naming it is still one the migration touches.
			if slices.Contains(staysWhole, table.Name) {
				how := "through an index the migration drops or alters"
				switch {
				case table.Through != "":
					how = "through a foreign key from " + table.Through
				case slices.Contains(named, table.Name):
					how = "in a statement that names it"
				}
				t.Errorf("%s touches %s as the census reads it, %s: %s", entry.Migration.Name, table.Name, how, staysWholeBecause(table.Name))
			}
		}
		for _, table := range named {
			if !read[table] {
				t.Errorf("%s touches %s, which the census read no size of: its verdict on that table is missing", entry.Migration.Name, table)
			}
		}
		for _, refusal := range entry.Refusals {
			if table := sizeRefusedTable(refusal); reading.typeMigrations[entry.Migration.Name] && slices.Contains(typeTables, table) {
				t.Logf("%s: the census under a %d byte table limit refuses %s on its size, which a type migration's insert takes; the census at pgmigrate.CensusTableLimit decides: %s",
					entry.Migration.Name, issueTreeTableLimit, table, refusal)
				continue
			}
			failed[entry.Migration.Name+"\x00"+refusal] = true
			t.Errorf("%s: the census under a %d byte table limit refuses it: %s", entry.Migration.Name, issueTreeTableLimit, refusal)
		}
	}
	if len(failed) > 0 {
		t.Logf("the census under a %d byte table limit:\n%s", issueTreeTableLimit, reportText(report))
	}

	// The second census, at pgmigrate.CensusTableLimit, is the verdict on graph_nodes and
	// graph_tuples, and reads the size of each that a migration of the set touches.
	limited := censusOfIssueTreeMigrations(t, url, fsys, ours, pgmigrate.CensusTableLimit)
	sizes := map[string]int64{}
	for _, entry := range limited.Pending {
		for _, table := range entry.Tables {
			if _, held := sizes[table.Name]; !held && table.Exists && slices.Contains(typeTables, table.Name) {
				sizes[table.Name] = table.Bytes
			}
		}
		for _, refusal := range entry.Refusals {
			if !failed[entry.Migration.Name+"\x00"+refusal] {
				t.Errorf("%s: the census at pgmigrate.CensusTableLimit, %d bytes, refuses it: %s", entry.Migration.Name, pgmigrate.CensusTableLimit, refusal)
			}
		}
	}
	for _, table := range typeTables {
		if bytes, held := sizes[table]; held {
			fmt.Printf("SIZE: %s %d %d\n", table, bytes, pgmigrate.CensusTableLimit)
		} else {
			t.Logf("%s: no migration of this child touches it, so the census read no size of it", table)
		}
	}
	if limited.Refused() {
		t.Logf("the census at pgmigrate.CensusTableLimit:\n%s", reportText(limited))
	}
}

// R19 and R20: no migration of this child touches a table above the census limit except through
// an exempt statement kind. The test finds this child's migrations by their name,
// <timestamp>_issue_tree_<what>, runs pgmigrate.TouchedTables over each, and takes the census of
// them with the table limit at one byte, reading its verdict on every table each touches; a table
// the census reads among those a migration touches is never one R17 keeps whole, however the
// census reached it. A type migration's statements on graph_nodes and graph_tuples are an insert,
// and on graph_nodes an update of the type nodes alone; a one-byte refusal on those two tables
// alone is left to a second census at pgmigrate.CensusTableLimit. It holds every statement of the
// set, read as TouchedTables reads it: none names artifact_versions, document_search,
// artifact_versions_document_search, asks_approval_kind_check or
// asks_normalize_approval_documents; none is an alter table, a drop table or a truncate of a table
// R19 keeps whole; the one statement on issue_links is the add of the nullable node column pair
// with no default, and the one statement on document_block_versions the add of the nullable
// integer columns stated_version and stated_rule with no default, each made once; and
// each table of createdNew, where the set names it, is created new and filled from no stored row.
// It reads no pull request, so it runs with no token.
func TestThisChildsMigrationsTouchNoTableAboveTheCensusLimit(t *testing.T) {
	requireTheIssueTreeMigrationsKeepToTheLimit(t, migrationFiles)
}

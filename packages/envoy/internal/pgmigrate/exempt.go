package pgmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	pgparser "github.com/wasilibs/go-pgquery/parser"
)

// catalogOnlyStatements are the statement kinds a migration may be made of and still be exempt
// from CensusTableLimit, keyed by the parse node Postgres's own grammar gives each, each node's
// function answering the word the report names the statement by, or "" where a statement of that
// node is not one of these kinds. The command itself changes only the system catalog or the
// transaction's own settings, or takes a lock, and reads no existing row of a table, so the time
// it holds its lock does not grow with the table:
//
//   - create function (or procedure, with or without or replace), in plpgsql or sql only
//     (catalogOnlyFunction): creating one writes pg_proc and has the language's validator check
//     the body, which for plpgsql parses it and for sql analyzes its queries, taking at most
//     ACCESS SHARE on the tables they name and reading none of their rows. A function in any other
//     language is not exempt: creating a C function loads its library, which can run any code;
//   - create trigger, of any form: it takes SHARE ROW EXCLUSIVE on its table and writes
//     pg_trigger, and the trigger runs only on later writes, never on the rows already there;
//   - drop trigger, drop function, drop index and drop table, with RESTRICT, which is the default
//     (catalogOnlyDrop): dropping a trigger writes pg_trigger, dropping a function writes pg_proc,
//     dropping an index deletes its pg_class and pg_index rows and unlinks its storage with them,
//     and dropping a table does the same for the table, its indexes and its TOAST; none of the four
//     reads a row of a table, so the time the statement holds its lock does not grow with the
//     table. The lock is not the argument for it: drop trigger and drop index both take ACCESS
//     EXCLUSIVE on the table where create trigger takes SHARE ROW EXCLUSIVE, and what the limit is
//     about is how long a lock is held, not which lock it is. catalogOnlyAlterTable below admits
//     DROP CONSTRAINT RESTRICT on the same reading, and that form drops the constraint's backing
//     index as part of its work. A drop with cascade is not exempt: what it drops besides the
//     object it names is not in the statement, so the census cannot read which objects a cascade
//     reaches, and the exemption is an argument about the statement the census can read. drop
//     procedure is left out for the one reason that no migration of this repository drops a
//     procedure;
//   - set and reset (VariableSetStmt), local or session, set role included: a run-time parameter,
//     which takes no lock, reads no row and names no table, so it cannot make a migration's lock
//     window grow with any table, and the other statements are still held to the kinds above.
//     A migration that bounds its own lock wait sets lock_timeout
//     (0066_asks_handed_back_reply_fkey, 20261101182522_search_term_queue_triggers), and that
//     line must not be what costs it an exemption the rest of its statements earn. A set of
//     search_path, role or session_authorization changes what the migration's names resolve to,
//     so it takes the catalog reads away (catalogReadsOff);
//   - alter table, of a table, with only the subcommands catalogOnlyAlterTable admits;
//   - rename column, of a table (catalogOnlyRename): it rewrites the column's pg_attribute row,
//     and no row of the table stores a column's name;
//   - create table, without INHERITS or PARTITION OF (catalogOnlyCreateTable): the new table
//     holds no row;
//   - create index, on a table the same migration creates (catalogOnlyIndex): the build reads the
//     rows of that table, which has none yet;
//   - create view, with or without or replace, recursive or not: it writes the view's pg_class and
//     pg_rewrite rows and analyzes its query, taking ACCESS SHARE on the tables the query names
//     and reading none of their rows; the query runs only when the view is read;
//   - lock table, in any mode: it takes a lock and reads no row. The lock is held to the end of
//     the migration, whose every other statement must be one of these kinds for it to be exempt,
//     so the time it is held does not grow with the table either, and the runner's lock_timeout
//     bounds its wait as it bounds every statement's.
//
// CREATE TABLE … AS and CREATE MATERIALIZED VIEW parse as CreateTableAsStmt, and SELECT … INTO as
// SelectStmt: each fills a table with the rows its query reads, and neither node is here.
//
// An event trigger on these commands runs code of its own when they run, so Census does not apply
// the exemption where one is enabled (ddlEventTriggers). Adding a kind is a reviewed edit with the
// same argument for it; a kind that can scan or rewrite (an index on an existing table, other
// alter table forms, a DO block, any DML) never belongs here. A marked concurrent index has a
// separate size exception in Census: it scans rows without holding off ordinary writes, not
// catalog-only work.
var catalogOnlyStatements = map[string]func(c *classifier, node json.RawMessage) (kind string, err error){
	"CreateFunctionStmt": (*classifier).catalogOnlyFunction,
	"CreateTrigStmt":     always("create trigger"),
	"DropStmt":           (*classifier).catalogOnlyDrop,
	"VariableSetStmt":    always("set"),
	"AlterTableStmt":     (*classifier).catalogOnlyAlterTable,
	"RenameStmt":         (*classifier).catalogOnlyRename,
	"CreateStmt":         (*classifier).catalogOnlyCreateTable,
	"IndexStmt":          (*classifier).catalogOnlyIndex,
	"ViewStmt":           always("create view"),
	"LockStmt":           always("lock table"),
}

// classifier is what tableSizeExemption holds while it judges the statements of one migration, in
// order.
type classifier struct {
	ctx context.Context
	// catalog is the census's transaction, each catalog read running in a savepoint of its own
	// (read); nil where Census gives the migration no catalog, or where the migration's own
	// statements forbid reading it (catalogReadsOff).
	catalog pgx.Tx
	// created names each table that a CREATE TABLE of the migration, written without IF NOT
	// EXISTS, has created so far, as the statement writes it (catalogOnlyIndex).
	created []rangeVar
}

// always admits every statement of a node as kind.
func always(kind string) func(*classifier, json.RawMessage) (string, error) {
	return func(*classifier, json.RawMessage) (string, error) { return kind, nil }
}

// catalogOnlyFunction names a create function's kind, "create function", when its parse node
// names plpgsql or sql as its language, or names none and has an SQL-standard body (begin
// atomic, return), which is sql; "" for any other language and for a node it cannot read.
func (*classifier) catalogOnlyFunction(node json.RawMessage) (string, error) {
	var function struct {
		Options []struct {
			DefElem *struct {
				Defname string `json:"defname"`
				Arg     struct {
					String *struct {
						Sval string `json:"sval"`
					} `json:"String"`
				} `json:"arg"`
			} `json:"DefElem"`
		} `json:"options"`
		SQLBody json.RawMessage `json:"sql_body"`
	}
	if err := json.Unmarshal(node, &function); err != nil {
		return "", nil
	}
	language := ""
	for _, option := range function.Options {
		if option.DefElem != nil && option.DefElem.Defname == "language" {
			if option.DefElem.Arg.String == nil {
				return "", nil
			}
			language = strings.ToLower(option.DefElem.Arg.String.Sval)
		}
	}
	switch language {
	case "plpgsql", "sql":
		return "create function", nil
	case "":
		if len(function.SQLBody) > 0 {
			return "create function", nil
		}
	}
	return "", nil
}

// dropStmt is what both passes of the classifier read of a DropStmt.
type dropStmt struct {
	RemoveType string `json:"removeType"`
	Behavior   string `json:"behavior"`
}

// catalogOnlyDrop names a drop's kind as the report names it, "drop trigger", "drop function",
// "drop index" or "drop table", and answers "" for a drop of anything else (a view, a schema, a
// procedure), for a drop with cascade, and for a node it cannot read. catalogOnlyStatements
// argues all four. A concurrent drop is not singled out: it does the same catalog work, and no
// migration the census reads can hold one anyway, since Postgres refuses CONCURRENTLY inside the
// transaction a migration runs in and the non-transactional mode holds one CREATE INDEX
// CONCURRENTLY alone (concurrentIndex). The runner does issue a DROP INDEX CONCURRENTLY of its
// own, to clear the invalid index a failed build left behind (nontransactional.go), but that
// statement is the runner's and stands in no migration the census parses.
func (*classifier) catalogOnlyDrop(node json.RawMessage) (string, error) {
	var drop dropStmt
	if err := json.Unmarshal(node, &drop); err != nil || drop.Behavior != "DROP_RESTRICT" {
		return "", nil
	}
	switch drop.RemoveType {
	case "OBJECT_TRIGGER":
		return "drop trigger", nil
	case "OBJECT_FUNCTION":
		return "drop function", nil
	case "OBJECT_INDEX":
		return "drop index", nil
	case "OBJECT_TABLE":
		return "drop table", nil
	}
	return "", nil
}

// catalogOnlyAlterTable answers "alter table" for an ALTER TABLE each of whose subcommands is one
// of these, which change only the catalog and read no existing row, and "" for any other:
//
//   - DROP CONSTRAINT and DROP COLUMN, with RESTRICT: removing a constraint deletes its catalog
//     rows, and its backing index with them, and removing a column marks it dropped in
//     pg_attribute, each row keeping the column's bytes until it is next written. CASCADE drops
//     objects the statement does not name, as catalogOnlyStatements says of a drop;
//   - ADD CONSTRAINT … NOT VALID, of a CHECK or a FOREIGN KEY: Postgres records the constraint and
//     tests no existing row, of the table or of the table a foreign key references; VALIDATE
//     CONSTRAINT, which is not admitted, does that later;
//   - ADD COLUMN, of a column catalogOnlyColumn admits;
//   - ALTER COLUMN … SET DEFAULT, of any expression, and ALTER COLUMN … DROP DEFAULT: a column's
//     default applies to the rows inserted later, so neither touches a row there is;
//   - ALTER COLUMN … DROP NOT NULL, which removes the constraint and checks nothing;
//   - ALTER COLUMN … DROP EXPRESSION, which makes a stored generated column a plain one that keeps
//     the values it stored; PostgreSQL 18 refuses it on a virtual generated column, whose values
//     are stored nowhere.
//
// Every other subcommand is refused: SET NOT NULL scans the table for nulls, ALTER COLUMN TYPE
// rewrites it, and VALIDATE CONSTRAINT and a validated ADD CONSTRAINT scan it. Each admitted
// subcommand still takes ACCESS EXCLUSIVE briefly, bounded by the runner's lock_timeout.
//
// The statement must alter a table (objtype OBJECT_TABLE). ALTER TYPE … ADD ATTRIBUTE and ALTER
// FOREIGN TABLE parse as this node too, as do ALTER VIEW, ALTER INDEX and ALTER SEQUENCE. The
// census sizes neither a type nor a foreign table, so admitting them serves nothing, and ALTER
// TYPE alters a type, which takes the catalog reads away from every later migration
// (catalogReadsOff).
func (c *classifier) catalogOnlyAlterTable(node json.RawMessage) (string, error) {
	var alter struct {
		Objtype string `json:"objtype"`
		Cmds    []struct {
			Command struct {
				Subtype  string                     `json:"subtype"`
				Behavior string                     `json:"behavior"`
				Def      map[string]json.RawMessage `json:"def"`
			} `json:"AlterTableCmd"`
		} `json:"cmds"`
	}
	if json.Unmarshal(node, &alter) != nil || alter.Objtype != "OBJECT_TABLE" || len(alter.Cmds) == 0 {
		return "", nil
	}
	for _, item := range alter.Cmds {
		cmd := item.Command
		switch cmd.Subtype {
		case "AT_DropConstraint", "AT_DropColumn":
			if cmd.Behavior != "DROP_RESTRICT" {
				return "", nil
			}
		case "AT_AddConstraint":
			var constraint struct {
				Type           string `json:"contype"`
				SkipValidation bool   `json:"skip_validation"`
			}
			if json.Unmarshal(cmd.Def["Constraint"], &constraint) != nil || !constraint.SkipValidation ||
				constraint.Type != "CONSTR_CHECK" && constraint.Type != "CONSTR_FOREIGN" {
				return "", nil
			}
		case "AT_AddColumn":
			if admitted, err := c.catalogOnlyColumn(cmd.Def["ColumnDef"]); err != nil || !admitted {
				return "", err
			}
		case "AT_ColumnDefault", "AT_DropNotNull", "AT_DropExpression":
		default:
			return "", nil
		}
	}
	return "alter table", nil
}

// catalogOnlyColumn admits an added column, a ColumnDef node, that Postgres adds in the catalog
// alone. PostgreSQL 18's ATExecAddColumn (tablecmds.c) rewrites the table for a volatile default,
// an identity column, a stored generated column and a domain with constraints, and scans it for a
// NOT NULL that no stored default fills; a CHECK scans it, a UNIQUE or a PRIMARY KEY builds an
// index over its rows, and a REFERENCES on a column with a DEFAULT clause checks every row. For
// any other column Postgres writes only the catalog, whatever the type, and stores a non-null
// default as the value every existing row reads. The column is admitted when each check below
// holds, and each says why it is there; the checks that read the statement come before the ones
// that read the catalog.
func (c *classifier) catalogOnlyColumn(def json.RawMessage) (bool, error) {
	// COLLATE, STORAGE and COMPRESSION change only what the catalog says of the column. Any other
	// key refuses it, so that a field a later libpg_query adds to the grammar is refused until
	// someone reads it. In the raw tree NOT NULL, DEFAULT, REFERENCES, CHECK, UNIQUE, PRIMARY KEY,
	// identity, a generated expression and an explicit NULL all arrive as entries of constraints,
	// and raw_default, is_not_null, identity and generated stay empty.
	column, ok := fieldsOnly(def, "colname", "typeName", "is_local", "location", "constraints", "collClause", "compression", "storage_name")
	if !ok {
		return false, nil
	}
	typeName, serial, ok := typeNameOf(column["typeName"])
	if !ok {
		return false, nil
	}
	var constraints []struct {
		Constraint struct {
			Type string          `json:"contype"`
			Expr json.RawMessage `json:"raw_expr"`
		} `json:"Constraint"`
	}
	if raw := column["constraints"]; raw != nil && json.Unmarshal(raw, &constraints) != nil {
		return false, nil
	}
	var notNull, foreign, attribute bool
	var columnDefault *defaultParts
	for _, item := range constraints {
		switch item.Constraint.Type {
		case "CONSTR_NOTNULL":
			notNull = true
		case "CONSTR_DEFAULT":
			// Postgres refuses a column with two defaults, so a second one is not read.
			parts, ok := defaultPartsOf(item.Constraint.Expr)
			if !ok || columnDefault != nil {
				return false, nil
			}
			columnDefault = &parts
		case "CONSTR_FOREIGN":
			foreign = true
		case "CONSTR_ATTR_DEFERRABLE", "CONSTR_ATTR_NOT_DEFERRABLE", "CONSTR_ATTR_DEFERRED", "CONSTR_ATTR_IMMEDIATE":
			attribute = true
		default:
			// CHECK, UNIQUE, PRIMARY KEY, EXCLUDE, identity and a generated expression read or
			// rewrite the table, as above. An explicit NULL reads nothing, but no other column
			// constraint is admitted beside NOT NULL, DEFAULT, and REFERENCES with its DEFERRABLE
			// and INITIALLY clauses.
			return false, nil
		}
	}
	// A DEFERRABLE or an INITIALLY clause belongs to the REFERENCES it follows.
	if attribute && !foreign {
		return false, nil
	}
	// Postgres skips the check of an added column's foreign key only when the column has no
	// DEFAULT clause at all: with DEFAULT NULL too it checks the key against every row.
	if foreign && columnDefault != nil {
		return false, nil
	}
	// Postgres checks the column for nulls only when its default gave it no non-null value to
	// store: with no default, or with a null constant, it scans the table. A pg_catalog function
	// that returns null at run time (inet_client_addr()) passes here, and Postgres then fails on
	// the first row and the migration rolls back: a failed deploy, not a long lock.
	if notNull && (columnDefault == nil || columnDefault.null) {
		return false, nil
	}
	// PostgreSQL's transformColumnDefinition (parse_utilcmd.c) turns a column whose type name has
	// one part, spelled smallserial, serial2, serial, serial4, bigserial or serial8, into an
	// integer column with a sequence and a nextval default before it looks up any type, and that
	// volatile default rewrites the table. Catalog read 1 would resolve a type of that name where
	// one exists and admit the column, so the name alone refuses it, whatever the catalog holds.
	// Postgres compares the folded name with strcmp, so "Serial" and pg_catalog.serial stay type
	// names it looks up as it looks up any other.
	if serial {
		return false, nil
	}
	// Without the catalog every added column is judged on size, a text column included: an
	// unqualified type name resolves through the search_path, and a migration that sets its own
	// can find a type of another schema under the name (catalogReadsOff).
	if c.catalog == nil {
		return false, nil
	}
	// The type must resolve in the catalog as it stands before any pending migration applies: a
	// type an earlier pending migration creates is not there yet, and ships in an earlier release.
	// Every domain is refused, not only one with constraints, since that reads one field. So is the
	// row type of a table, a view, a materialized view, a foreign table or a partitioned table: a
	// pending DROP TABLE of that relation can uncover a domain of the same name in a later schema
	// of the search_path, and Postgres would rewrite the table for that domain's constraint. A
	// composite type made by CREATE TYPE … AS (relkind c), an array, an enum and a range are
	// admitted.
	columnType, found, err := c.resolveType(typeName)
	if err != nil || !found || columnType.typtype == "d" || columnType.relkind != "" && columnType.relkind != "c" {
		return false, err
	}
	if columnDefault == nil {
		return true, nil
	}
	targets := []uint32{columnType.oid, columnType.elem}
	if columnDefault.cast != "" {
		castType, found, err := c.resolveType(columnDefault.cast)
		if err != nil || !found || castType.typtype == "d" {
			return false, err
		}
		targets = append(targets, castType.oid, castType.elem)
	}
	if columnDefault.function != "" {
		// Postgres evaluates the default once, under the ACCESS EXCLUSIVE lock, and a function of
		// another schema can read any table, where a function of pg_catalog with no argument reads
		// none (now(), statement_timestamp()). A volatile one makes Postgres rewrite the table, to
		// give each row a value of its own, and so does one that is no plain function returning
		// one value.
		var volatility, kind string
		var inCatalog, returnsSet bool
		found, err := c.read(functionQuery, columnDefault.function, &volatility, &inCatalog, &kind, &returnsSet)
		if err != nil || !found || !inCatalog || kind != "f" || returnsSet || volatility != "s" && volatility != "i" {
			return false, err
		}
	}
	// Postgres can put a cast of its own between the default and the column (int8(int4) puts 0
	// into a bigint), and a volatile cast function makes it rewrite the table. Every cast to the
	// column's type, to the type the default casts to, or to the element type of either, is read:
	// a superset of the casts Postgres can choose, which refuses nothing in a stock PostgreSQL 18,
	// none of whose cast functions is volatile. The input and output functions an I/O conversion
	// calls are not read: none of a stock PostgreSQL 18 is volatile, and only a superuser can make
	// a base type with functions of its own in C.
	var volatileCast bool
	found, err = c.read(volatileCastQuery, targets, &volatileCast)
	return found && !volatileCast, err
}

// defaultParts are what the catalog judges of an added column's default: the type a cast names,
// as Catalog read 1 takes it, and the function a call names, as Catalog read 2 takes it, each ""
// where the default has none; and whether the default is a null constant, cast or not.
type defaultParts struct {
	cast, function string
	null           bool
}

// defaultPartsOf reads a default's raw expression into its parts, ok false where it is not built
// from the four kinds of parts Postgres stores as a value of the catalog in place of a rewrite:
//
//   - a constant, or an ARRAY[…] of constants (constant);
//   - a cast of one of those: a TypeCast whose arg is a constant and whose type name typeNameOf
//     reads. A cast of a cast, or of a function, is refused: no default a migration ships casts
//     anything but a constant, and the cast's own function would be one more to judge;
//   - a SQL value function, of any op: current_timestamp, current_date, localtimestamp,
//     current_user, session_user and the like, each stable;
//   - a call with no arguments: a FuncCall holding no key but funcname, funcformat and location,
//     so no argument, *, DISTINCT, VARIADIC, ORDER BY, FILTER or OVER, of a name of one part, or
//     of two whose first is pg_catalog. Catalog read 2 judges the function the name resolves to.
//
// Anything else is refused: an operator (1 + 1), a column reference, a subquery, COALESCE and
// the like.
func defaultPartsOf(expr json.RawMessage) (parts defaultParts, ok bool) {
	kind, body, ok := nodeOf(expr)
	if !ok {
		return parts, false
	}
	switch kind {
	case "TypeCast":
		var cast struct {
			Arg      json.RawMessage `json:"arg"`
			TypeName json.RawMessage `json:"typeName"`
		}
		if json.Unmarshal(body, &cast) != nil {
			return parts, false
		}
		if parts.null, ok = constant(cast.Arg); !ok {
			return parts, false
		}
		parts.cast, _, ok = typeNameOf(cast.TypeName)
		return parts, ok
	case "SQLValueFunction":
		return parts, true
	case "FuncCall":
		call, ok := fieldsOnly(body, "funcname", "funcformat", "location")
		var name nameList
		if !ok || json.Unmarshal(call["funcname"], &name) != nil {
			return parts, false
		}
		switch id := name.identifier(); {
		case len(id) == 1, len(id) == 2 && id[0] == "pg_catalog":
			parts.function = id.Sanitize() + "()"
			return parts, true
		}
		return parts, false
	}
	parts.null, ok = constant(expr)
	return parts, ok
}

// constant says whether an expression is a constant a default may be or cast, an A_Const or an
// A_ArrayExpr whose every element is an A_Const, and whether it is a null A_Const. A nested array
// is refused.
func constant(expr json.RawMessage) (null, ok bool) {
	kind, body, ok := nodeOf(expr)
	if !ok {
		return false, false
	}
	switch kind {
	case "A_Const":
		return aConst(body)
	case "A_ArrayExpr":
		var array struct {
			Elements []json.RawMessage `json:"elements"`
		}
		if json.Unmarshal(body, &array) != nil {
			return false, false
		}
		for _, element := range array.Elements {
			kind, body, ok := nodeOf(element)
			if !ok || kind != "A_Const" {
				return false, false
			}
			if _, ok := aConst(body); !ok {
				return false, false
			}
		}
		return false, true
	}
	return false, false
}

// aConst reads an A_Const's fields: one of ival, fval, sval, bsval, boolval and isnull beside its
// location, ok false for any other key. The JSON leaves a zero value out, so 0 arrives as
// {"ival":{}} and false as {"boolval":{}}: which key is there is what is read, never its value.
func aConst(body json.RawMessage) (null, ok bool) {
	fields, ok := fieldsOnly(body, "ival", "fval", "sval", "bsval", "boolval", "isnull", "location")
	return fields["isnull"] != nil, ok
}

// renameStmt is what both passes of the classifier read of a RenameStmt.
type renameStmt struct {
	RenameType   string `json:"renameType"`
	RelationType string `json:"relationType"`
}

// catalogOnlyRename names a rename of a table's column, "rename column", and answers "" for any
// other rename. The census reads every pending migration against the database as it stands before
// any of them applies, and finds a table or an index by its name: after a pending migration
// renames a table, an index or a constraint, a later pending migration that names the new name
// finds nothing there, and its size and its lock holders are not read. A constraint rename counts,
// because Postgres renames a primary key's, a unique constraint's or an exclusion constraint's
// index with it. The census finds nothing by a column's name. The column must be a table's, as
// alter table must alter a table (catalogOnlyAlterTable): the census sizes no view, type or
// foreign table.
func (*classifier) catalogOnlyRename(node json.RawMessage) (string, error) {
	var rename renameStmt
	if json.Unmarshal(node, &rename) != nil || rename.RenameType != "OBJECT_COLUMN" || rename.RelationType != "OBJECT_TABLE" {
		return "", nil
	}
	return "rename column", nil
}

// rangeVar is the name a RangeVar node gives a relation, each part as the parser gives it, folded
// or unquoted.
type rangeVar struct {
	Catalogname string `json:"catalogname"`
	Schemaname  string `json:"schemaname"`
	Relname     string `json:"relname"`
}

// sanitized is the name with every part quoted, as Catalog read 4 takes it.
func (r rangeVar) sanitized() string {
	var name pgx.Identifier
	for _, part := range [...]string{r.Catalogname, r.Schemaname, r.Relname} {
		if part != "" {
			name = append(name, part)
		}
	}
	return name.Sanitize()
}

// catalogOnlyCreateTable names the create of a new table, "create table", and records the table
// for an index on it (catalogOnlyIndex). A new table holds no row, so creating it with its
// columns, its constraints, its LIKE clauses, which copy definitions and not rows, and its
// references to existing tables reads no existing row: Postgres checks no foreign key of a table
// it creates, since the table has no row, and locks the table a key references without reading
// it. INHERITS and PARTITION OF are not admitted: each joins the new table to an existing one, and
// PARTITION OF checks the rows of that table's default partition against the new bound. CREATE
// TABLE IF NOT EXISTS is admitted, since it creates the table or does nothing, but it records no
// table: where the table exists, an index after it is built on that table's rows.
func (c *classifier) catalogOnlyCreateTable(node json.RawMessage) (string, error) {
	var create struct {
		Relation     rangeVar        `json:"relation"`
		InhRelations json.RawMessage `json:"inhRelations"`
		Partbound    json.RawMessage `json:"partbound"`
		IfNotExists  bool            `json:"if_not_exists"`
	}
	if json.Unmarshal(node, &create) != nil || create.InhRelations != nil || create.Partbound != nil {
		return "", nil
	}
	if !create.IfNotExists {
		c.created = append(c.created, create.Relation)
	}
	return "create table", nil
}

// catalogOnlyIndex names an index on a table the same migration creates, "create index": the
// build reads the table's rows, and the new table has none. An earlier CREATE TABLE of the
// migration, written without IF NOT EXISTS, must name the table exactly as the index does, and
// the name must resolve to no relation in the census's transaction (Catalog read 4), so that it
// can resolve to nothing but the new table: Postgres looks an unqualified name up in the session's
// temporary schema and in pg_catalog before the schemas of the search_path, unless the
// search_path lists them, and a table of that name there would take the index. Without the
// catalog the census cannot see that, so the index is judged on size. A concurrent build is not
// admitted here: Postgres refuses one inside a transaction, and Census has an exception of its own
// for one (concurrentIndex).
func (c *classifier) catalogOnlyIndex(node json.RawMessage) (string, error) {
	var index struct {
		Relation   rangeVar `json:"relation"`
		Concurrent bool     `json:"concurrent"`
	}
	if json.Unmarshal(node, &index) != nil || index.Concurrent || c.catalog == nil || !slices.Contains(c.created, index.Relation) {
		return "", nil
	}
	var absent bool
	if found, err := c.read(tableAbsentQuery, index.Relation.sanitized(), &absent); err != nil || !found || !absent {
		return "", err
	}
	return "create index", nil
}

// catalogReadsOff is the classifier's first pass, over every statement of the migration before
// any is judged, since a statement anywhere in it can change what the names of all of them
// resolve to. here says that this migration may not read the catalog, and later that no pending
// migration after it may either, which Census keeps (catalogTrusted). The census resolves names
// on its own connection, in the catalog as it stands before any pending migration applies, so a
// read stops being the one the migration meets:
//
//   - after a SET or RESET of search_path, role or session_authorization, or a RESET ALL, in the
//     migration: its names resolve through another path, or as another role, than the census's do
//     (the default search_path starts with "$user"). Without LOCAL, or as RESET ALL, the setting
//     outlives the migration: the runner applies each migration on a connection of its pool, which
//     resets no session setting, so the next migration can take that connection with the setting
//     on it, and every later migration loses the reads too. SET LOCAL ends with the migration's
//     transaction;
//   - after a statement that changes a type, a domain, a cast, a schema, an extension or a role
//     (changesTypes): a pending migration could drop an enum and create a domain with a
//     constraint under its name, and Postgres would then rewrite a table for a column the census
//     admitted.
//
// A table's or a view's own row type is no type here, so CREATE TABLE, CREATE VIEW and DROP TABLE
// leave the reads on: otherwise every migration after one that creates a table would lose them. A
// DO block, a CALL, a function's body and a trigger are code the census does not read, so a type,
// a cast or a setting such code changes is not seen.
func (c *classifier) catalogReadsOff(statements []statement) (here, later bool, err error) {
	for _, s := range statements {
		if s.node == "VariableSetStmt" {
			var set struct {
				Kind    string `json:"kind"`
				Name    string `json:"name"`
				IsLocal bool   `json:"is_local"`
			}
			if json.Unmarshal(s.body, &set) != nil || set.Kind == "VAR_RESET_ALL" {
				return true, true, nil
			}
			// The parser keeps the case of a quoted name (set "Search_Path" = x gives Search_Path,
			// reset "ROLE" gives ROLE), and Postgres finds a setting by its name without regard to
			// case, so the name is compared folded to lower case. strings.ToLower folds more letters
			// than Postgres does, and a name it folds to one of these only refuses more.
			if name := strings.ToLower(set.Name); name == "search_path" || name == "role" || name == "session_authorization" {
				// The pooled connection keeps a session setting for the next migration it runs.
				if !set.IsLocal {
					return true, true, nil
				}
				here = true
			}
			continue
		}
		changes, err := c.changesTypes(s)
		if err != nil {
			return false, false, err
		}
		if changes {
			return true, true, nil
		}
	}
	return here, false, nil
}

// changesTypes says whether a statement creates, alters, renames or drops a type, a domain, a
// cast, a schema or an extension, or moves an object to another schema, or has one of those
// effects without naming it, or changes which schema a name resolves in:
//
//   - an extension's script creates types, casts and schemas the census cannot see;
//   - a change of owner of a type, a domain or a schema, DROP OWNED, which drops every one a role
//     owns, and REASSIGN OWNED: a new owner can take a role's USAGE on a schema away;
//   - a GRANT or a REVOKE on a schema, a type or a domain: USAGE on a schema decides which of its
//     names a role's lookup finds;
//   - a GRANT or a REVOKE of a role's membership in another role (GrantRoleStmt), CREATE ROLE,
//     whose IN ROLE, ROLE and ADMIN clauses grant memberships, ALTER ROLE, which changes them and
//     the role's INHERIT attribute (ALTER GROUP … ADD USER and DROP USER parse as it too), and
//     DROP ROLE, which removes them, USER or GROUP in place of ROLE included: a role holds the
//     USAGE on a schema that each role it inherits from holds, so each of these can change which
//     schemas a role's lookup finds names in, as a GRANT on a schema does. ALTER ROLE … SET and
//     ALTER DATABASE … SET parse as nodes of their own (AlterRoleSetStmt, AlterDatabaseSetStmt)
//     and stay off this list: they name no type, domain, cast or schema, and they are no SET the
//     migration runs;
//   - a rename of a role (RenameStmt of OBJECT_ROLE): the default search_path starts with
//     "$user", the name of the role a session runs as, so the rename changes which schema that
//     entry names for a session of the role;
//   - a drop with CASCADE, which can drop a type, a domain or a cast it does not name;
//   - an ALTER FUNCTION, or a CREATE OR REPLACE FUNCTION, of a function that may be a cast's
//     (castFunction): either can make the cast's function volatile.
//
// A statement of one of these nodes that it cannot read counts as one that does.
func (c *classifier) changesTypes(s statement) (bool, error) {
	typeObjects := []string{"OBJECT_TYPE", "OBJECT_DOMAIN", "OBJECT_SCHEMA"}
	switch s.node {
	case "CreateEnumStmt", "CompositeTypeStmt", "CreateRangeStmt", "CreateDomainStmt", "CreateCastStmt",
		"CreateSchemaStmt", "CreateExtensionStmt", "AlterEnumStmt", "AlterTypeStmt", "AlterDomainStmt",
		"AlterExtensionStmt", "AlterExtensionContentsStmt", "DropOwnedStmt", "ReassignOwnedStmt",
		"AlterObjectSchemaStmt", "GrantRoleStmt", "CreateRoleStmt", "AlterRoleStmt", "DropRoleStmt":
		return true, nil
	case "DefineStmt":
		var define struct {
			Kind string `json:"kind"`
		}
		return json.Unmarshal(s.body, &define) != nil || define.Kind == "OBJECT_TYPE", nil
	case "AlterTableStmt":
		var alter struct {
			Objtype string `json:"objtype"`
		}
		return json.Unmarshal(s.body, &alter) != nil || alter.Objtype == "OBJECT_TYPE", nil
	case "AlterOwnerStmt":
		var owner struct {
			ObjectType string `json:"objectType"`
		}
		return json.Unmarshal(s.body, &owner) != nil || slices.Contains(typeObjects, owner.ObjectType), nil
	case "GrantStmt":
		var grant struct {
			Objtype string `json:"objtype"`
		}
		return json.Unmarshal(s.body, &grant) != nil || slices.Contains(typeObjects, grant.Objtype), nil
	case "RenameStmt":
		var rename renameStmt
		return json.Unmarshal(s.body, &rename) != nil ||
			slices.Contains([]string{"OBJECT_TYPE", "OBJECT_DOMAIN", "OBJECT_DOMCONSTRAINT", "OBJECT_ATTRIBUTE", "OBJECT_SCHEMA", "OBJECT_ROLE"}, rename.RenameType), nil
	case "DropStmt":
		var drop dropStmt
		return json.Unmarshal(s.body, &drop) != nil || drop.Behavior == "DROP_CASCADE" ||
			slices.Contains([]string{"OBJECT_TYPE", "OBJECT_DOMAIN", "OBJECT_CAST", "OBJECT_SCHEMA", "OBJECT_EXTENSION"}, drop.RemoveType), nil
	case "AlterFunctionStmt":
		var alter struct {
			Func struct {
				Objname nameList `json:"objname"`
			} `json:"func"`
		}
		if json.Unmarshal(s.body, &alter) != nil {
			return true, nil
		}
		return c.castFunction(alter.Func.Objname)
	case "CreateFunctionStmt":
		var create struct {
			Replace  bool     `json:"replace"`
			Funcname nameList `json:"funcname"`
		}
		if json.Unmarshal(s.body, &create) != nil {
			return true, nil
		}
		if !create.Replace {
			return false, nil
		}
		return c.castFunction(create.Funcname)
	}
	return false, nil
}

// castFunction says whether a function of name may be a cast's function (Catalog read 5): whether
// a cast calls any function of its last part's name, in any schema, which is more than the one
// function the name resolves to. An error Postgres raises for the read, and a name with no part,
// count as yes. Without a catalog it answers no: the reads are off already, and the answer would
// change nothing.
func (c *classifier) castFunction(name nameList) (bool, error) {
	if c.catalog == nil {
		return false, nil
	}
	if len(name) == 0 {
		return true, nil
	}
	var cast bool
	found, err := c.read(castFunctionQuery, name[len(name)-1].String.Sval, &cast)
	return cast || !found, err
}

// The classifier's catalog reads. Each takes a name with every part quoted (nameList.identifier,
// rangeVar.sanitized), so that Postgres resolves the parts the parser gave, folded or unquoted,
// and no others: to_regtype('Int4') resolves where to_regtype('"Int4"') does not.
const (
	// typeQuery, Catalog read 1, resolves a type name: its oid, its typtype (d for a domain), its
	// element type (typelem, 0 where it has none, which no cast targets), and the relkind of the
	// relation whose row type it is, '' where it is none's. No row means the name resolves to no
	// type.
	typeQuery = `select t.oid, t.typtype, t.typelem, coalesce(c.relkind::text, '') from pg_type t left join pg_class c on c.oid = t.typrelid where t.oid = to_regtype($1)`
	// functionQuery, Catalog read 2, resolves a call of no arguments: its volatility, whether it
	// is of pg_catalog, its kind (f for a plain function) and whether it returns a set. No row
	// means the name resolves to no function.
	functionQuery = `select p.provolatile, p.pronamespace = 'pg_catalog'::regnamespace, p.prokind, p.proretset from pg_proc p where p.oid = to_regprocedure($1)`
	// volatileCastQuery, Catalog read 3, says whether any cast to one of the types $1 names calls a
	// volatile function.
	volatileCastQuery = `select exists (select from pg_cast c join pg_proc p on p.oid = c.castfunc where c.casttarget = any($1::oid[]) and p.provolatile = 'v')`
	// tableAbsentQuery, Catalog read 4, says whether a table's name resolves to no relation.
	tableAbsentQuery = `select to_regclass($1) is null`
	// castFunctionQuery, Catalog read 5, says whether a cast calls a function of the unqualified
	// name $1, in any schema.
	castFunctionQuery = `select exists (select from pg_cast c join pg_proc p on p.oid = c.castfunc where p.proname = $1)`
)

// catalogType is what Catalog read 1 finds for a type name.
type catalogType struct {
	oid, elem        uint32
	typtype, relkind string
}

// resolveType runs Catalog read 1 for the type name name, found false where it resolves to no
// type.
func (c *classifier) resolveType(name string) (t catalogType, found bool, err error) {
	found, err = c.read(typeQuery, name, &t.oid, &t.typtype, &t.elem, &t.relkind)
	return t, found, err
}

// read runs one catalog read in a savepoint of the census's transaction, as lockBoundOf and
// censusTable read, and scans its row into dest. found is false where the read finds no row, or
// where Postgres raises an error for it: PostgreSQL 18 raises errors for some names
// (to_regtype('int('), to_regclass('x.y.z.w')), which refuse the form the read is for, and the
// savepoint keeps the census's transaction usable after one. Any other error is returned.
func (c *classifier) read(query string, arg any, dest ...any) (found bool, err error) {
	nested, err := c.catalog.Begin(c.ctx)
	if err != nil {
		return false, fmt.Errorf("savepoint: %w", err)
	}
	err = nested.QueryRow(c.ctx, query, arg).Scan(dest...)
	if rollback := nested.Rollback(c.ctx); err == nil {
		err = rollback
	}
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows), errors.As(err, &pgErr):
		return false, nil
	}
	return false, fmt.Errorf("read the catalog: %w", err)
}

// statement is one statement of a migration: the type of its parse node, and the node's fields.
type statement struct {
	node string
	body json.RawMessage
}

// parseStatements reads sql with Postgres's own parser into its statements, ok false where the
// parser refuses the text, fails itself, or gives a tree the census cannot read.
func parseStatements(sql string) (statements []statement, ok bool) {
	defer func() {
		if recover() != nil {
			statements, ok = nil, false
		}
	}()
	tree, err := pgparser.ParseToJSON(sql)
	if err != nil {
		return nil, false
	}
	var parsed struct {
		Stmts []struct {
			Stmt json.RawMessage `json:"stmt"`
		} `json:"stmts"`
	}
	if json.Unmarshal([]byte(tree), &parsed) != nil {
		return nil, false
	}
	statements = make([]statement, len(parsed.Stmts))
	for i, s := range parsed.Stmts {
		node, body, ok := nodeOf(s.Stmt)
		if !ok {
			return nil, false
		}
		statements[i] = statement{node: node, body: body}
	}
	return statements, true
}

// nodeOf reads a node of the parse tree, which libpg_query writes as an object of one key, the
// node's type, holding the node's fields: {"A_Const":{…}}.
func nodeOf(raw json.RawMessage) (kind string, body json.RawMessage, ok bool) {
	var node map[string]json.RawMessage
	if json.Unmarshal(raw, &node) != nil || len(node) != 1 {
		return "", nil, false
	}
	for kind, body = range node {
	}
	return kind, body, true
}

// fieldsOnly reads a node's fields, ok false where it cannot be read or holds a field outside
// allowed, so that a field a later libpg_query adds to the grammar is refused until someone reads
// it.
func fieldsOnly(node json.RawMessage, allowed ...string) (fields map[string]json.RawMessage, ok bool) {
	if json.Unmarshal(node, &fields) != nil {
		return nil, false
	}
	for key := range fields {
		if !slices.Contains(allowed, key) {
			return nil, false
		}
	}
	return fields, true
}

// nameList is a qualified name in the parse tree: a list of String nodes, each part folded or
// unquoted as the parser reads it.
type nameList []struct {
	String struct {
		Sval string `json:"sval"`
	} `json:"String"`
}

// identifier is the name's parts, which its Sanitize quotes each.
func (n nameList) identifier() pgx.Identifier {
	parts := make(pgx.Identifier, len(n))
	for i, part := range n {
		parts[i] = part.String.Sval
	}
	return parts
}

// typeNameOf reads a TypeName node as Catalog read 1 takes it, every part quoted and [] after an
// array's name. ok is false where the node holds a key other than names, typemod, typmods,
// arrayBounds and location (%TYPE and SETOF among them), or a name of other than one or two
// parts. A type modifier does not change the type a name resolves to, and array bounds do no more
// than make it an array. serial says the name has one part that is exactly smallserial, serial2,
// serial, serial4, bigserial or serial8, which a column definition does not look up as a type
// (catalogOnlyColumn).
func typeNameOf(node json.RawMessage) (name string, serial, ok bool) {
	fields, ok := fieldsOnly(node, "names", "typemod", "typmods", "arrayBounds", "location")
	var names nameList
	if !ok || json.Unmarshal(fields["names"], &names) != nil || len(names) < 1 || len(names) > 2 {
		return "", false, false
	}
	serial = len(names) == 1 && slices.Contains([]string{"smallserial", "serial2", "serial", "serial4", "bigserial", "serial8"}, names[0].String.Sval)
	name = names.identifier().Sanitize()
	if fields["arrayBounds"] != nil {
		name += "[]"
	}
	return name, serial, true
}

// tableSizeExemption says why migration sql is exempt from CensusTableLimit: the kinds of its
// statements, as the report names them ("create function and create trigger"), when every one of
// its statements is a catalogOnlyStatements kind; "" when any statement is not one of those
// kinds, when the migration holds no statement, or when it does not parse. laterReadsOff says the
// migration changes what a name resolves to in the catalog for the pending migrations after it
// (catalogReadsOff), so Census gives them no catalog to read.
//
// catalog is the census's transaction, which the classifier reads where a form needs it: an added
// column's type, its default's cast and function, and whether the table a created index is built
// on is new. Its first pass also reads, for a CREATE OR REPLACE FUNCTION or an ALTER FUNCTION,
// whether a cast calls a function of that name (castFunction), since the answer decides
// laterReadsOff. With a nil catalog, or where the migration's own statements forbid the reads
// (catalogReadsOff), it reads nothing to admit a form, and every added column and every created
// index is judged on size; the other kinds need no read. An error Postgres raises for a read
// refuses the form it is for, and any other error is returned. The census's transaction is
// repeatable-read and read-only, so every read sees the catalog as it stands before any pending
// migration applies.
//
// The statements are read by Postgres's own parser, libpg_query (Postgres 17's scan.l and
// gram.y), run as WebAssembly in pure Go by github.com/wasilibs/go-pgquery, so the reading is the
// server's: a comment, a string literal or a dollar-quoted function body is a token of the
// statement it is in, never a statement, and two statements on one line are two parse nodes. A
// migration that does not parse is not exempt, and laterReadsOff is true for it: the census cannot
// see what it changes, and a migration can parse on the PostgreSQL 18 server and not in the
// census's Postgres 17 grammar (a VIRTUAL generated column). A failure of the parser itself is
// answered the same way, so the size rule stands whenever the reading cannot be had.
func tableSizeExemption(ctx context.Context, catalog pgx.Tx, sql string) (exemption string, laterReadsOff bool, err error) {
	statements, ok := parseStatements(sql)
	if !ok {
		return "", true, nil
	}
	c := &classifier{ctx: ctx, catalog: catalog}
	readsOff, laterReadsOff, err := c.catalogReadsOff(statements)
	if err != nil {
		return "", false, err
	}
	if readsOff {
		c.catalog = nil
	}
	var kinds []string
	for _, s := range statements {
		catalogOnly, ok := catalogOnlyStatements[s.node]
		if !ok {
			return "", laterReadsOff, nil
		}
		kind, err := catalogOnly(c, s.body)
		if err != nil {
			return "", false, err
		}
		if kind == "" {
			return "", laterReadsOff, nil
		}
		if !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	return strings.Join(kinds, " and "), laterReadsOff, nil
}

// ddlEventTriggersQuery names, sorted and comma-separated, every enabled event trigger on
// ddl_command_start, ddl_command_end or sql_drop whose tags are unset or name one of the command
// tags below, "" when there are none: the tag of each kind catalogOnlyStatements admits, CREATE
// INDEX among them, which the separate concurrent exception reads too. A column rename's tag is
// ALTER TABLE, and LOCK TABLE, SET and RESET fire no event trigger. A drop fires sql_drop as well
// as the two ddl_command_ events, so an event trigger on sql_drop would run code of its own on a
// drop the exemption covers. Census reads this once for the whole database rather than per
// migration, so such a trigger on sql_drop takes the exemption from every migration, one holding
// no drop included: the reading that refuses, not the one that applies.
const ddlEventTriggersQuery = `
	select coalesce(string_agg(evtname, ', ' order by evtname), '')
	from pg_event_trigger
	where evtenabled <> 'D' and evtevent in ('ddl_command_start', 'ddl_command_end', 'sql_drop')
		and (evttags is null or evttags && array['CREATE FUNCTION', 'CREATE PROCEDURE', 'CREATE TRIGGER', 'DROP FUNCTION', 'DROP TRIGGER', 'DROP INDEX', 'ALTER TABLE', 'CREATE INDEX', 'CREATE TABLE', 'CREATE VIEW', 'DROP TABLE'])`

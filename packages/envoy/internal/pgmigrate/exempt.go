package pgmigrate

import (
	"cmp"
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
//     line must not be what costs it an exemption the rest of its statements earn. A set or a
//     reset of any setting but lock_timeout and statement_timeout takes the catalog reads away
//     (catalogReadsOff): such a setting can change what the migration's names resolve to, as
//     search_path does, or how the server reads the text of a later migration;
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
// the exemption where one is enabled (ddlEventTriggers), or after a pending migration that can
// create or enable one that the census does not see, or that the census cannot parse
// (takesLaterExemptions, tableSizeExemption). Adding a kind is a reviewed edit with the same
// argument for it; a kind that can scan or rewrite (an index on an existing table, other alter
// table forms, a DO block, any DML) never belongs here. A marked concurrent index has a separate
// size exception in Census: it scans rows without holding off ordinary writes, not catalog-only
// work.
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
// FOREIGN TABLE parse as this node too, as do ALTER VIEW, ALTER MATERIALIZED VIEW (objtype
// OBJECT_MATVIEW), ALTER INDEX and ALTER SEQUENCE. The census sizes neither a type nor a foreign
// table, so admitting them serves nothing, and ALTER TYPE alters a type, which takes the catalog
// reads away from every later migration (catalogReadsOff).
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
// holds, and each says why it is there. admittedType judges every type the statement names, the
// column's and the one its default casts to, and admittedCall judges every function its default
// calls.
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
			if columnDefault != nil {
				return false, nil
			}
			parts, ok, err := c.defaultPartsOf(item.Constraint.Expr)
			if err != nil || !ok {
				return false, err
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
	columnType, ok, err := c.admittedType(column["typeName"])
	if err != nil || !ok {
		return false, err
	}
	if columnDefault == nil {
		return true, nil
	}
	// Postgres can put a cast of its own between the default and the column (int8(int4) puts 0
	// into a bigint), and a volatile cast function makes it rewrite the table. Every cast to the
	// column's type, to the type the default casts to, or to the element type of either, is read:
	// a superset of the casts Postgres can choose, which refuses nothing in a stock PostgreSQL 18,
	// none of whose cast functions is volatile. Where the default casts nothing, or a type has no
	// element type, the oid read is 0, which no cast targets. The input and output functions an
	// I/O conversion calls are not read: none of a stock PostgreSQL 18 is volatile, and only a
	// superuser can make a base type with functions of its own in C.
	var volatileCast bool
	found, err := c.read(volatileCastQuery, []uint32{columnType.oid, columnType.elem, columnDefault.cast.oid, columnDefault.cast.elem}, &volatileCast)
	return found && !volatileCast, err
}

// defaultParts are what catalogOnlyColumn reads of a default defaultPartsOf admits: the type its
// cast names, as Catalog read 1 finds it, zero where the default casts nothing, and whether the
// default is a null constant, cast or not.
type defaultParts struct {
	cast catalogType
	null bool
}

// defaultPartsOf reads a default's raw expression into its parts, ok false where it is not built
// from the four kinds of parts Postgres stores as a value of the catalog in place of a rewrite:
//
//   - a constant, or an ARRAY[…] of constants (constant);
//   - a cast of one of those: a TypeCast whose arg is a constant and whose type admittedType
//     admits. A cast of a cast, or of a function, is refused: no default a migration ships casts
//     anything but a constant, and the cast's own function would be one more to judge;
//   - a SQL value function, of any op: current_timestamp, current_date, localtimestamp,
//     current_user, session_user and the like, each stable;
//   - a call with no arguments: a FuncCall holding no key but funcname, funcformat and location,
//     so no argument, *, DISTINCT, VARIADIC, ORDER BY, FILTER or OVER, of a function
//     admittedCall admits.
//
// Anything else is refused: an operator (1 + 1), a column reference, a subquery, COALESCE and
// the like.
func (c *classifier) defaultPartsOf(expr json.RawMessage) (parts defaultParts, ok bool, err error) {
	kind, body, ok := nodeOf(expr)
	if !ok {
		return parts, false, nil
	}
	switch kind {
	case "TypeCast":
		var cast struct {
			Arg      json.RawMessage `json:"arg"`
			TypeName json.RawMessage `json:"typeName"`
		}
		if json.Unmarshal(body, &cast) != nil {
			return parts, false, nil
		}
		if parts.null, ok = constant(cast.Arg); !ok {
			return parts, false, nil
		}
		parts.cast, ok, err = c.admittedType(cast.TypeName)
		return parts, ok, err
	case "SQLValueFunction":
		return parts, true, nil
	case "FuncCall":
		call, ok := fieldsOnly(body, "funcname", "funcformat", "location")
		var name nameList
		if !ok || json.Unmarshal(call["funcname"], &name) != nil {
			return parts, false, nil
		}
		ok, err = c.admittedCall(name)
		return parts, ok, err
	}
	parts.null, ok = constant(expr)
	return parts, ok, nil
}

// admittedType judges a type a statement names, a TypeName node: an added column's type
// (catalogOnlyColumn) or the type its default casts to (defaultPartsOf). It judges every type
// position, and nothing else resolves a type. It answers the type as Catalog read 1 finds it, ok
// true where the census admits it: the name typeNameOf reads is no serial name, it resolves in
// the catalog, and neither the type nor its element type is a domain or the row type of a
// relation.
func (c *classifier) admittedType(node json.RawMessage) (t catalogType, ok bool, err error) {
	name, names, ok := typeNameOf(node)
	if !ok {
		return t, false, nil
	}
	// PostgreSQL's transformColumnDefinition (parse_utilcmd.c) turns a column whose type name has
	// one part, spelled smallserial, serial2, serial, serial4, bigserial or serial8, into an
	// integer column with a sequence and a nextval default before it resolves any type, and that
	// volatile default rewrites the table. Catalog read 1 would resolve a type of that name where
	// one exists and admit the column, so the name alone refuses it, whatever the catalog holds.
	// Postgres compares the folded name with strcmp, so "Serial" and pg_catalog.serial stay type
	// names it resolves as it resolves any other. Postgres refuses a column of an array of such a
	// name (array of serial is not implemented), so the census refuses one too: the parts of
	// serial[] are the parts of serial. Postgres resolves a cast's type name as it resolves any
	// other, so the rule also refuses a cast to a type of one of these names that the checks below
	// would admit: that only refuses more, and no default a migration ships casts to one.
	if len(names) == 1 && slices.Contains([]string{"smallserial", "serial2", "serial", "serial4", "bigserial", "serial8"}, names[0].String.Sval) {
		return t, false, nil
	}
	// Without the catalog no type is admitted, text included: an unqualified type name resolves
	// through the search_path, and a migration that sets its own can find a type of another schema
	// under the name (catalogReadsOff).
	if c.catalog == nil {
		return t, false, nil
	}
	// The type must resolve in the catalog as it stands before any pending migration applies: a
	// type an earlier pending migration creates is not there yet, and ships in an earlier release.
	found, err := c.read(typeQuery, name, &t.oid, &t.typtype, &t.elem, &t.relkind, &t.elemTyptype, &t.elemRelkind)
	if err != nil || !found {
		return t, false, err
	}
	// Every domain is refused, not only one with constraints, since that reads one field. So is
	// the row type of a table, a view, a materialized view, a foreign table or a partitioned table
	// (a relkind other than c). A pending DROP TABLE or DROP VIEW of that relation leaves the
	// catalog reads on (keepsReadsOn), and after it the name can resolve to a type of a later
	// schema of the search_path, which the census did not read. That type can be a domain with a
	// constraint, for which Postgres rewrites the table, or a type with a volatile cast from the
	// default, which Catalog read 3 did not read. The same drop can change what an array's element
	// type resolves to, so the element type is held to both rules too, and an array of a domain is
	// refused as the domain is, which only refuses more. An array's element type is never an
	// array (x[][] is x[]), so one level of element covers every array. A composite type made by
	// CREATE TYPE … AS (relkind c), an enum, a range, a base type and an array of one of these
	// are admitted.
	domainOrRowType := func(typtype, relkind string) bool { return typtype == "d" || relkind != "" && relkind != "c" }
	return t, !domainOrRowType(t.typtype, t.relkind) && !domainOrRowType(t.elemTyptype, t.elemRelkind), nil
}

// admittedCall judges every function a default calls with no argument (defaultPartsOf), name as
// the parser gives it. Postgres evaluates the default once, under the ACCESS EXCLUSIVE lock, and a
// function of another schema can read any table, where a function of pg_catalog with no argument
// reads none (now(), statement_timestamp()). A volatile one makes Postgres rewrite the table, to
// give each row a value of its own, and so does one that is no plain function returning one
// value. The call is admitted when each check below holds, and each says why it is there.
func (c *classifier) admittedCall(name nameList) (bool, error) {
	// The name has one part, or two whose first is pg_catalog: a function of any other schema can
	// read any table.
	id := name.identifier()
	qualified := len(id) == 2 && id[0] == "pg_catalog"
	if len(id) != 1 && !qualified {
		return false, nil
	}
	// Without the catalog no call is admitted: an unqualified name resolves through the
	// search_path, and a migration that sets its own can find a function of another schema under
	// the name (catalogReadsOff).
	if c.catalog == nil {
		return false, nil
	}
	// The function of no argument the name resolves to is of pg_catalog, a plain function (prokind
	// f) that returns one value, and stable or immutable.
	var volatility, kind string
	var inCatalog, returnsSet, catalogFirst, shared bool
	found, err := c.read(functionQuery, id.Sanitize()+"()", &volatility, &inCatalog, &kind, &returnsSet, &catalogFirst, &shared)
	if err != nil || !found || !inCatalog || kind != "f" || returnsSet || volatility != "s" && volatility != "i" {
		return false, err
	}
	// Postgres searches pg_catalog alone for pg_catalog.f, so no function of another schema can
	// take the call, and the two checks below do not apply to it.
	if qualified {
		return true, nil
	}
	// PostgreSQL's call resolution (FuncnameGetCandidates, namespace.c) takes a function whose
	// arguments all have defaults as well as one with no argument, and of two candidates in
	// different schemas the one of the earlier schema of the search_path wins. to_regprocedure
	// finds only the signature of no argument, so with s1.now(a int default 0) and search_path
	// s1, pg_catalog, Catalog read 2 reads pg_catalog.now() and Postgres calls s1.now(0). A pending
	// CREATE FUNCTION leaves the catalog reads on, and it can create such a function, which the
	// census does not see. So pg_catalog must be the first schema of the census's search_path that
	// is not the temporary schema, which Postgres never searches for a function: pg_catalog's
	// candidate then wins over the candidate of every other schema, a pending migration's
	// included. Where two functions of pg_catalog could both take the call, Postgres finds it
	// ambiguous and refuses it: a failed migration, not a rewrite.
	if !catalogFirst {
		return false, nil
	}
	// No routine outside pg_catalog has the name, in any schema, whatever its arguments: the
	// census refuses every name that a function of another schema shares. That also covers a
	// runner whose search_path is not the census's, for every function there is before the
	// pending migrations apply.
	return !shared, nil
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

// sanitized is the name with every part quoted, as Catalog reads 4 and 7 take it.
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
// migration after it may either, which Census keeps (catalogTrusted). exemptionsOff says why no
// pending migration after this one may be exempt from the limit, in the words of the first
// statement that takes their exemptions (takesLaterExemptions), and "" where no statement does.
// The pass asks that of every statement, also of the statements after the one that turns the
// reads off. The census resolves names on its own connection, in the catalog as it stands before
// any pending migration applies. A read is the one the migration meets only while no statement
// before it has changed what a name resolves to, so the pass keeps the reads on only after a
// statement it knows to leave the names as they are:
//
//   - a SET or a RESET keeps them on only when it names lock_timeout or statement_timeout. A SET
//     or a RESET of any other setting, a RESET ALL, and a SET node the pass cannot read turn them
//     off. A setting can change what the names of a later statement resolve to, as search_path
//     does (the default search_path starts with "$user"), or how the server reads the text of a
//     later migration, as standard_conforming_strings off does wherever a runner does not set the
//     text settings (textSettings). The census cannot list every setting that does one or the
//     other, so it holds a list of the two settings it knows to do neither: each bounds how long
//     a statement waits or runs. A SET LOCAL ends with the migration's transaction, so it turns
//     off the reads of its own migration alone. A session SET, a RESET and a RESET ALL turn off
//     the reads of their own migration and of every later one, because they outlive the
//     migration: the runner applies each migration on a connection of its pool, which resets no
//     session setting, so the next migration can take that connection with the setting on it;
//   - every other statement keeps them on only where it takes no exemption and keepsReadsOn
//     names it, and turns them off for its own migration and every later one where it does not. A
//     pending migration could drop an enum and create a domain with a constraint under its name,
//     and Postgres would then rewrite a table for a column the census admitted. After a SET LOCAL
//     that turned off the reads of its own migration, the migration resolves names under a
//     setting the census does not share, so keepsReadsOn then reads every relation name of a
//     later statement as a name of pg_catalog. After any statement that turns the reads off, the
//     census no longer resolves a name as the migration does, and takesLaterExemptions reads the
//     names of the statements after it without the catalog (catalogRelation).
//
// The pass holds a list of the statements that leave the names as they are, not a list of the
// statements that change them. A statement that a list of the second kind misses admits a column
// that Postgres rewrites the table for. A statement that keepsReadsOn does not name costs the
// later migrations their catalog reads, so their added columns are judged on size: a refusal,
// which is the safer failure. The pass reads every statement that the migration's text shows,
// whoever may run it, a superuser included. A statement of a kind off the list (listedKind), a DO
// block and a CALL among them, also takes the exemptions of every later pending migration, since
// the census cannot see whether the code it runs creates or enables an event trigger. Code that no
// statement shows stays a limit where a statement that keeps the reads on calls it: the census
// does not read the code that a function body or a trigger body runs.
func (c *classifier) catalogReadsOff(statements []statement) (here, later bool, exemptionsOff string, err error) {
	for _, s := range statements {
		if s.node == "VariableSetStmt" {
			var set struct {
				Kind    string `json:"kind"`
				Name    string `json:"name"`
				IsLocal bool   `json:"is_local"`
			}
			if json.Unmarshal(s.body, &set) != nil || set.Kind == "VAR_RESET_ALL" {
				here, later = true, true
				continue
			}
			// The parser keeps the case of a quoted name (set "Lock_Timeout" = x gives
			// Lock_Timeout), and Postgres finds a setting by its name without regard to case, so the
			// name is compared folded to lower case. strings.ToLower folds more letters than
			// Postgres does, so a name it folds to one of these two can be a name Postgres finds no
			// setting for, and Postgres refuses to set or reset such a name.
			if name := strings.ToLower(set.Name); name == "lock_timeout" || name == "statement_timeout" {
				continue
			}
			// The pooled connection keeps a session setting for the next migration it runs.
			here, later = true, later || !set.IsLocal
			continue
		}
		reason, err := c.takesLaterExemptions(s, !here)
		if err != nil {
			return false, false, "", err
		}
		if reason != "" {
			return true, true, reason, nil
		}
		if later {
			continue
		}
		keeps, err := c.keepsReadsOn(s, !here)
		if err != nil {
			return false, false, "", err
		}
		if !keeps {
			here, later = true, true
		}
	}
	return here, later, "", nil
}

// keepsReadsOn says whether a statement other than a SET or a RESET is known to leave every type,
// function and cast name, the schemas a role's lookup searches, and every setting as they are, so
// that the catalog reads stay on after it. catalogReadsOff reads a SET or a RESET itself, and
// keeps the reads on after one only when it names lock_timeout or statement_timeout. The keep-on
// list names the statements this repository's migrations write that change none of these, and a
// statement of any other kind, or a node that cannot be read, is off it. listedKind reads the
// kinds below from the parse node alone, and keepsReadsOn reads the catalog for the name that a
// CREATE TABLE, a CREATE VIEW or a CREATE FUNCTION gives:
//
//   - INSERT, UPDATE, DELETE and SELECT, a WITH included, since a WITH is a field of each. A
//     SELECT … INTO is not on the list (selectInto): it creates a table and fills it with the rows
//     its query reads, as CREATE TABLE … AS does, and the list holds neither form of a table that
//     a query fills;
//   - LOCK TABLE, CREATE TRIGGER and CREATE INDEX, none of which makes or changes a type or a
//     function;
//   - CREATE TABLE and CREATE VIEW, with or without OR REPLACE, of a name that pg_catalog holds as
//     the name of no type (catalogTypeName). The new row type can hide a type of its name in a
//     later schema of the search_path, which the census then reads in its place. But no cast
//     targets the new row type, since a cast needs a CREATE CAST, which is not on the list, and a
//     row type is no domain, so Postgres adds a column of it in the catalog alone. A row type with
//     the name of a pg_catalog type is off the list all the same: the census reads a later
//     column's type of that name as the pg_catalog type, and under a search_path that lists
//     pg_catalog after the new table's schema, Postgres resolves the name to the row type;
//   - ALTER TABLE of a table, and ALTER INDEX, which renames nothing (a rename is a RenameStmt),
//     with no subcommand of CASCADE. DROP COLUMN … CASCADE and DROP CONSTRAINT … CASCADE drop
//     objects the statement does not name, as a DROP … CASCADE does: a function with an
//     SQL-standard body that reads the column, say, and with it a range type whose subtype_diff it
//     is. ALTER TYPE, ALTER VIEW, ALTER MATERIALIZED VIEW, ALTER SEQUENCE and ALTER FOREIGN TABLE
//     parse as this node too, and none of them is on the list;
//   - RENAME COLUMN of a table, which renames an attribute of the table's row type and no type.
//     Every other rename is off the list. A rename of a table, a view, a materialized view or a
//     foreign table renames the relation's row type, which keeps every cast that targets it, so a
//     later migration's type name can resolve to that row type in an earlier schema of the
//     search_path, through a cast the census never read;
//   - DROP INDEX, DROP TRIGGER, DROP FUNCTION, DROP TABLE and DROP VIEW, with RESTRICT. Without
//     CASCADE, Postgres refuses to drop a function a cast calls, and a table or a view whose row
//     type a cast or a function names. A name the dropped row type held resolved to the row type
//     of a relation in the catalog the census reads, which admittedType refuses;
//   - CREATE FUNCTION or CREATE PROCEDURE, with or without OR REPLACE, in plpgsql or sql
//     (catalogOnlyFunction), of a name whose last part names no function of pg_catalog and no
//     function a cast calls, in any schema (catalogOrCastFunction). admittedCall admits a default's
//     unqualified call only where no function outside pg_catalog has its name, and it reads the
//     catalog as it stands before the pending migrations: a function of that name that a pending
//     migration creates in another schema can take the call, and a replace of the pg_catalog
//     function can make it volatile. On PostgreSQL 18.6, after a replace of pg_catalog.now() with
//     a volatile function, Postgres rewrote a table for an added column whose default calls
//     now(). A replace of a cast's function can make the cast's function volatile. A function in
//     another language runs code the census does not read when it is created: a C function loads
//     its library, which can run any code, and plperl's validator runs the body's BEGIN blocks.
//
// A statement on the list keeps the reads on only while its whole parse tree (treeNames) names no
// relation of pg_catalog and calls no set_config:
//
//   - a relation of pg_catalog, or one whose rules read one (catalogRelation): a view over
//     pg_proc, a view of information_schema, a table with a rule. A write to pg_proc or pg_cast,
//     by a superuser or through such a view, can make a cast's function volatile or add a cast,
//     and an update of pg_settings changes a setting, while the census reads the catalog as it
//     stood before. The census does not tell a read of such a relation from a write: every
//     statement that names one is off the list, which only refuses more. Such a statement also
//     takes the exemptions of every later pending migration (takesLaterExemptions). The walk
//     finds a relation wherever the statement names it: in a FROM, a JOIN or a WITH, as the table
//     that an INSERT, an UPDATE, a DELETE or a MERGE writes, in a LOCK, as the table of a CREATE
//     INDEX or a CREATE TRIGGER, in a REFERENCES, a LIKE or an INHERITS, and in an SQL-standard
//     function body;
//   - a call of set_config, which changes a setting as a SET does, search_path among them,
//     wherever the statement makes it: in a SELECT, in an INSERT's values, in a default or a check
//     of a CREATE TABLE or an ALTER TABLE, and in an SQL-standard function body.
//
// catalogReadsOff asks keepsReadsOn only about a statement that takes no exemption of a later
// migration (takesLaterExemptions): its kind is on the list (listedKind), and it names no
// relation that catalogRelation counts. keepsReadsOn reads the rest. resolves says whether the
// census resolves a relation's name as the migration does: it does not after a SET LOCAL of the
// migration that turned off the migration's own reads (catalogReadsOff). There, and without a
// catalog, keepsReadsOn counts every relation name as a name of pg_catalog, since the census
// cannot tell which relation the migration finds by it. Without a catalog the reads are off
// already, and the answer changes nothing.
func (c *classifier) keepsReadsOn(s statement, resolves bool) (bool, error) {
	switch s.node {
	case "CreateStmt", "ViewStmt":
		var create struct {
			Relation rangeVar `json:"relation"`
			View     rangeVar `json:"view"`
		}
		if json.Unmarshal(s.body, &create) != nil {
			return false, nil
		}
		name := create.Relation.Relname
		if s.node == "ViewStmt" {
			name = create.View.Relname
		}
		if held, err := c.catalogTypeName(name); err != nil || held {
			return false, err
		}
	case "CreateFunctionStmt":
		var create struct {
			Funcname nameList `json:"funcname"`
		}
		if json.Unmarshal(s.body, &create) != nil {
			return false, nil
		}
		if shared, err := c.catalogOrCastFunction(create.Funcname); err != nil || shared {
			return false, err
		}
	}
	names, ok := treeNamesOf(s.body)
	if !ok || names.setConfig {
		return false, nil
	}
	return len(names.relations) == 0 || resolves && c.catalog != nil, nil
}

// listedKind says whether statement s is of a kind on the keep-on list (keepsReadsOn lists the
// kinds), as its parse node shows with no catalog read: an INSERT, an UPDATE, a DELETE, a LOCK
// TABLE, a CREATE TRIGGER, a CREATE INDEX, a CREATE TABLE and a CREATE VIEW, a SELECT without INTO,
// an ALTER TABLE of a table or an index with no subcommand of CASCADE, a RENAME COLUMN of a table,
// a DROP INDEX, DROP TRIGGER, DROP FUNCTION, DROP TABLE or DROP VIEW with RESTRICT, and a CREATE
// FUNCTION or CREATE PROCEDURE in plpgsql or sql. A node it cannot read is off the list.
func (c *classifier) listedKind(s statement) bool {
	switch s.node {
	case "InsertStmt", "UpdateStmt", "DeleteStmt", "LockStmt", "CreateTrigStmt", "IndexStmt", "CreateStmt", "ViewStmt":
		return true
	case "SelectStmt":
		return !selectInto(s.body)
	case "AlterTableStmt":
		var alter struct {
			Objtype string `json:"objtype"`
			Cmds    []struct {
				Command struct {
					Behavior string `json:"behavior"`
				} `json:"AlterTableCmd"`
			} `json:"cmds"`
		}
		if json.Unmarshal(s.body, &alter) != nil || alter.Objtype != "OBJECT_TABLE" && alter.Objtype != "OBJECT_INDEX" {
			return false
		}
		for _, item := range alter.Cmds {
			if item.Command.Behavior == "DROP_CASCADE" {
				return false
			}
		}
		return true
	case "RenameStmt":
		var rename renameStmt
		return json.Unmarshal(s.body, &rename) == nil && rename.RenameType == "OBJECT_COLUMN" && rename.RelationType == "OBJECT_TABLE"
	case "DropStmt":
		var drop dropStmt
		return json.Unmarshal(s.body, &drop) == nil && drop.Behavior == "DROP_RESTRICT" &&
			slices.Contains([]string{"OBJECT_INDEX", "OBJECT_TRIGGER", "OBJECT_FUNCTION", "OBJECT_TABLE", "OBJECT_VIEW"}, drop.RemoveType)
	case "CreateFunctionStmt":
		kind, err := c.catalogOnlyFunction(s.body)
		return err == nil && kind != ""
	}
	return false
}

// cannotSeeAnEventTrigger ends the words of laterExemptionsOff for a migration that holds a
// statement whose effect on the event triggers the census cannot read, and for a migration that
// does not parse.
const cannotSeeAnEventTrigger = ", so the census cannot see whether it creates or alters an event trigger"

// takesLaterExemptions says why statement s takes the table-size exemption and the concurrent
// index exception from every pending migration after its own, in words that follow the
// migration's name, and "" where it takes neither. An event trigger runs code of its own on the
// statements of every later migration, as an enabled one does (ddlEventTriggersQuery), and that
// code can read any table. The census reads pg_event_trigger as it stands before any pending
// migration applies, so it does not see an event trigger that a pending statement creates or
// enables. A statement takes the exemptions where it:
//
//   - creates or alters an event trigger, whatever the trigger's tags and whether it is enabled;
//   - names a relation of pg_catalog, or a relation whose rules read one (catalogRelation),
//     anywhere in its parse tree (treeNames), whether its kind is on the keep-on list or not. An
//     UPDATE of pg_event_trigger enables a disabled event trigger, as ALTER EVENT TRIGGER …
//     ENABLE does. On PostgreSQL 18.6, the census exempted a DROP COLUMN after such an UPDATE,
//     and when the migration applied, the trigger counted every row of the table;
//   - is of a kind off the keep-on list (listedKind). A DO block and a CALL run code that the
//     census does not read. A CREATE EXTENSION and an ALTER EXTENSION run the extension's script,
//     and some extensions create an event trigger when they install, as pg_graphql and Citus do.
//     The census does not know a statement of any other kind off the list to leave every event
//     trigger as it is.
//
// The words name the relation in the second case, and in the third case they name a DO block, a
// CALL and a statement of an extension. Some statements turn the catalog reads off and take no
// exemption. A SET or a RESET creates and enables no event trigger, and the census counts every
// event trigger that is not disabled, whatever session_replication_role a setting gives. A
// statement of a kind on the list that creates a table, a view or a function with a name that
// pg_catalog holds, or that calls set_config (keepsReadsOn), creates and enables no event trigger
// either: a function that it replaces runs only when a statement calls it. resolves says whether
// the census resolves a relation's name as the migration does (catalogRelation).
func (c *classifier) takesLaterExemptions(s statement, resolves bool) (string, error) {
	switch s.node {
	case "VariableSetStmt":
		return "", nil
	case "CreateEventTrigStmt", "AlterEventTrigStmt":
		return "creates or alters an event trigger", nil
	}
	names, ok := treeNamesOf(s.body)
	for _, relation := range names.relations {
		catalog, err := c.catalogRelation(relation, resolves)
		if err != nil {
			return "", err
		}
		if catalog {
			return "names " + relation.sanitized() + ", which can resolve to a relation of pg_catalog or to a relation whose rules read one" + cannotSeeAnEventTrigger, nil
		}
	}
	switch {
	case s.node == "DoStmt":
		return "runs a DO block" + cannotSeeAnEventTrigger, nil
	case s.node == "CallStmt":
		return "calls a procedure" + cannotSeeAnEventTrigger, nil
	case s.node == "CreateExtensionStmt" || s.node == "AlterExtensionStmt" || s.node == "AlterExtensionContentsStmt":
		return "creates or alters an extension" + cannotSeeAnEventTrigger, nil
	case !ok || !c.listedKind(s):
		return "holds a statement that is off the list of statements that keep the catalog reads on" + cannotSeeAnEventTrigger, nil
	}
	return "", nil
}

// selectInto says whether a SelectStmt holds an INTO, which Postgres takes from the leftmost
// SELECT of a set operation (select 1 into t union select 2) and refuses anywhere else. A node it
// cannot read counts as one that does.
func selectInto(body json.RawMessage) bool {
	for {
		var stmt struct {
			IntoClause json.RawMessage `json:"intoClause"`
			Larg       json.RawMessage `json:"larg"`
		}
		if json.Unmarshal(body, &stmt) != nil || stmt.IntoClause != nil {
			return true
		}
		if stmt.Larg == nil {
			return false
		}
		body = stmt.Larg
	}
}

// treeNames are what takesLaterExemptions and keepsReadsOn read of a statement's whole parse
// tree: each relation name it gives, once, and whether it calls set_config.
type treeNames struct {
	relations []rangeVar
	setConfig bool
}

// treeNamesOf reads the treeNames of a statement's parse node, ok false where the node cannot be
// read. walk meets the fields of a node in the random order of a Go map, so treeNamesOf sorts the
// relation names: the refusal then names the same relation of a statement on every census
// (takesLaterExemptions).
func treeNamesOf(body json.RawMessage) (names treeNames, ok bool) {
	var tree any
	if json.Unmarshal(body, &tree) != nil {
		return treeNames{}, false
	}
	names.walk(tree)
	slices.SortFunc(names.relations, func(a, b rangeVar) int {
		return cmp.Or(strings.Compare(a.Catalogname, b.Catalogname), strings.Compare(a.Schemaname, b.Schemaname), strings.Compare(a.Relname, b.Relname))
	})
	return names, true
}

// walk reads a node of a parse tree, decoded from JSON, and every node under it. libpg_query
// writes a RangeVar as {"RangeVar":{…}} where its field can hold any node (a FROM item, a LOCK's
// relation, an INHERITS parent), and bare where the field holds a RangeVar alone (the relation of
// an INSERT, an UPDATE, an ALTER TABLE, a CREATE INDEX or a CREATE TRIGGER, the table of a
// REFERENCES or a LIKE). So walk reads every object that holds a string relname as a RangeVar:
// no other node of a raw parse tree has that field. It reads a call the same way, as an object
// that holds a funcname. A FuncCall holds one wherever it stands, and a CREATE FUNCTION or a
// CREATE TRIGGER holds one for the function it names, which only refuses more. strings.ToLower
// folds the name's last part, which also only refuses more: Postgres finds no set_config under a
// quoted name in another case.
func (names *treeNames) walk(tree any) {
	switch node := tree.(type) {
	case map[string]any:
		if relname, ok := node["relname"].(string); ok {
			catalogname, _ := node["catalogname"].(string)
			schemaname, _ := node["schemaname"].(string)
			relation := rangeVar{Catalogname: catalogname, Schemaname: schemaname, Relname: relname}
			if !slices.Contains(names.relations, relation) {
				names.relations = append(names.relations, relation)
			}
		}
		if funcname, ok := node["funcname"].([]any); ok && len(funcname) > 0 {
			last, _ := funcname[len(funcname)-1].(map[string]any)
			part, _ := last["String"].(map[string]any)
			if sval, _ := part["sval"].(string); strings.ToLower(sval) == "set_config" {
				names.setConfig = true
			}
		}
		for _, value := range node {
			names.walk(value)
		}
	case []any:
		for _, item := range node {
			names.walk(item)
		}
	}
}

// catalogRelation says whether relation, a name that a statement gives, may name a relation of
// pg_catalog or a relation whose rules read one (Catalog read 7). A name with pg_catalog as its
// schema is one without a read. With a catalog, where the census resolves names as the migration
// does, a name that can resolve to no relation is not one: the statement that creates it was
// judged itself. An error Postgres raises for the read counts as yes. Without a catalog, or where
// resolves is false, the census cannot resolve the name, so it counts a name with no schema that
// starts with pg_. Every relation of pg_catalog has such a name, and Postgres searches pg_catalog
// first unless the search_path lists it later. That is a limit only for a name that resolves to a
// view, or to another relation with rules, over a relation of pg_catalog: the census cannot see
// those rules without the catalog. A pending statement that creates such a view names the
// relation of pg_catalog, so it is judged itself. To count every name there would take the
// exemptions of the later migrations after every migration whose catalog reads are off
// (takesLaterExemptions).
func (c *classifier) catalogRelation(relation rangeVar, resolves bool) (bool, error) {
	if relation.Schemaname == "pg_catalog" {
		return true, nil
	}
	if !resolves || c.catalog == nil {
		return relation.Schemaname == "" && strings.HasPrefix(relation.Relname, "pg_"), nil
	}
	var catalog bool
	found, err := c.read(catalogRelationQuery, relation.sanitized(), &catalog)
	return catalog || !found, err
}

// catalogTypeName says whether pg_catalog holds a type of name (Catalog read 6), the name of the
// row type that a CREATE TABLE or a CREATE VIEW creates. An error Postgres raises for the read
// counts as yes, and so does every name without a catalog: the reads are off already, and the
// answer changes nothing.
func (c *classifier) catalogTypeName(name string) (bool, error) {
	if c.catalog == nil {
		return true, nil
	}
	var held bool
	found, err := c.read(catalogTypeNameQuery, name, &held)
	return held || !found, err
}

// catalogOrCastFunction says whether a function that a statement creates or replaces under name
// may change a function that the census reads (Catalog read 5): whether a function of its last
// part's name, in any schema, is of pg_catalog or is a function that a cast calls, which is more
// than the one function the name resolves to. A create of a new function with the name of a
// cast's function changes no cast, and the one read refuses it too, which only refuses more. An
// error Postgres raises for the read, and a name with no part, count as yes. So does every name
// without a catalog: the reads are off already, and the answer changes nothing.
func (c *classifier) catalogOrCastFunction(name nameList) (bool, error) {
	if c.catalog == nil || len(name) == 0 {
		return true, nil
	}
	var shared bool
	found, err := c.read(catalogOrCastFunctionQuery, name[len(name)-1].String.Sval, &shared)
	return shared || !found, err
}

// The classifier's catalog reads. Each read that resolves a name takes it with every part quoted
// (nameList.identifier, rangeVar.sanitized), so that Postgres resolves the parts the parser gave,
// folded or unquoted, and no others: to_regtype('Int4') resolves where to_regtype('"Int4"') does
// not. Catalog reads 5 and 6 resolve no name: each compares one part, as the parser gave it, with
// the names of the catalog.
const (
	// typeQuery, Catalog read 1, resolves a type name: its oid, its typtype (d for a domain), its
	// element type (typelem, 0 where it has none, which no cast targets), and the relkind of the
	// relation whose row type it is, '' where it is none's; then the typtype of the element type
	// and the relkind of the relation whose row type that is, each '' where there is none. No row
	// means the name resolves to no type.
	typeQuery = `select t.oid, t.typtype, t.typelem, coalesce(c.relkind::text, ''), coalesce(e.typtype::text, ''), coalesce(ec.relkind::text, '')
		from pg_type t left join pg_class c on c.oid = t.typrelid
		left join pg_type e on e.oid = t.typelem left join pg_class ec on ec.oid = e.typrelid
		where t.oid = to_regtype($1)`
	// functionQuery, Catalog read 2, resolves a call of no arguments: its volatility, whether it
	// is of pg_catalog, its kind (f for a plain function) and whether it returns a set. It also
	// reads whether pg_catalog is the first schema of the session's search_path that is not a
	// temporary one (current_schemas(true) lists the schemas a lookup searches, in order, the
	// temporary one as pg_temp_N), and whether a routine outside pg_catalog has the function's
	// name. No row means the name resolves to no function.
	functionQuery = `select p.provolatile, p.pronamespace = 'pg_catalog'::regnamespace, p.prokind, p.proretset,
		coalesce((select s.nspname = 'pg_catalog' from unnest(current_schemas(true)) with ordinality s(nspname, ord)
			where not starts_with(s.nspname, 'pg_temp_') order by s.ord limit 1), false),
		exists (select from pg_proc o where o.proname = p.proname and o.pronamespace <> 'pg_catalog'::regnamespace)
		from pg_proc p where p.oid = to_regprocedure($1)`
	// volatileCastQuery, Catalog read 3, says whether any cast to one of the types $1 names calls a
	// volatile function.
	volatileCastQuery = `select exists (select from pg_cast c join pg_proc p on p.oid = c.castfunc where c.casttarget = any($1::oid[]) and p.provolatile = 'v')`
	// tableAbsentQuery, Catalog read 4, says whether a table's name resolves to no relation.
	tableAbsentQuery = `select to_regclass($1) is null`
	// catalogOrCastFunctionQuery, Catalog read 5, says whether a function of the unqualified name
	// $1, in any schema, is of pg_catalog or is a function that a cast calls.
	catalogOrCastFunctionQuery = `select exists (select from pg_proc p where p.proname = $1
		and (p.pronamespace = 'pg_catalog'::regnamespace or exists (select from pg_cast c where c.castfunc = p.oid)))`
	// catalogTypeNameQuery, Catalog read 6, says whether pg_catalog holds a type of the
	// unqualified name $1.
	catalogTypeNameQuery = `select exists (select from pg_type where typname = $1 and typnamespace = 'pg_catalog'::regnamespace)`
	// catalogRelationQuery, Catalog read 7, says whether a relation's name can resolve to a
	// relation of pg_catalog, or to a relation whose rules (pg_rewrite) read one, through views
	// over views to any depth: a view over pg_proc, a view of information_schema, a table with a
	// rule. A qualified name ($1 has more than one part, parse_ident) is the relation that
	// to_regclass resolves it to. An unqualified name is every relation of that name in a schema
	// that current_schemas(true) lists, pg_catalog and the temporary schema included, and not only
	// the relation that it resolves to now. A pending DROP TABLE or DROP VIEW, which keeps the
	// reads on, can drop the relation that a later statement's name resolves to now, and the name
	// then resolves to a relation of the same name in a later schema. A rule's query tree
	// (ev_action) holds each relation it reads as ":relid <oid>". pg_depend cannot serve:
	// PostgreSQL records no dependency on a pinned object, and every relation of pg_catalog is
	// pinned, so pg_depend holds no row for a view's read of pg_proc. A column name that holds the
	// text ":relid <oid>" is read as a relation too, which only refuses more. A name that can
	// resolve to no relation reads false.
	catalogRelationQuery = `with recursive reached(oid) as (
			select to_regclass($1)::oid where cardinality(parse_ident($1)) > 1
			union
			select c.oid from pg_class c join pg_namespace n on n.oid = c.relnamespace
				where cardinality(parse_ident($1)) = 1 and c.relname = (parse_ident($1))[1] and n.nspname = any(current_schemas(true))
			union
			select m[1]::oid from reached r join pg_rewrite w on w.ev_class = r.oid
				cross join lateral regexp_matches(w.ev_action::text, ':relid ([0-9]+)', 'g') m
		)
		select exists (select from reached r join pg_class c on c.oid = r.oid where c.relnamespace = 'pg_catalog'::regnamespace)`
)

// catalogType is what Catalog read 1 finds for a type name.
type catalogType struct {
	oid, elem                                  uint32
	typtype, relkind, elemTyptype, elemRelkind string
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

// typeNameOf reads a TypeName node: name as Catalog read 1 takes it, every part quoted and []
// after an array's name, and names, its parts as the parser gives them, folded or unquoted, which
// are the same for x and x[]. ok is false where the node holds a key other than names, typemod,
// typmods, arrayBounds and location (%TYPE and SETOF among them), or a name of other than one or
// two parts. A type modifier does not change the type a name resolves to, and array bounds do no
// more than make it an array: x[][] is the type x[].
func typeNameOf(node json.RawMessage) (name string, names nameList, ok bool) {
	fields, ok := fieldsOnly(node, "names", "typemod", "typmods", "arrayBounds", "location")
	if !ok || json.Unmarshal(fields["names"], &names) != nil || len(names) < 1 || len(names) > 2 {
		return "", nil, false
	}
	name = names.identifier().Sanitize()
	if fields["arrayBounds"] != nil {
		name += "[]"
	}
	return name, names, true
}

// classification is what tableSizeExemption answers for one migration.
type classification struct {
	// exemption names the kinds of the migration's statements, as the report names them ("create
	// function and create trigger"), when every one of its statements is a catalogOnlyStatements
	// kind; "" when any statement is not one of those kinds, when the migration holds no
	// statement, or when it does not parse.
	exemption string
	// laterReadsOff says the migration holds a statement that the census does not know to leave
	// names as they are, which can change what a name resolves to for the pending migrations after
	// it (catalogReadsOff), so Census gives them no catalog to read.
	laterReadsOff bool
	// laterExemptionsOff says why no pending migration after this one may be exempt from the
	// limit, as words that follow the migration's name; "" where nothing stops the exemptions. A
	// statement of the migration can create or enable an event trigger that the census does not
	// see (takesLaterExemptions), and such a trigger can run code of its own on the statements of
	// every later migration, as an enabled one does (ddlEventTriggersQuery). Or the migration does
	// not parse, so the census cannot see whether it creates or alters one.
	laterExemptionsOff string
}

// tableSizeExemption says why migration sql is exempt from CensusTableLimit, and what it changes
// for the pending migrations after it (classification).
//
// catalog is the census's transaction, which the classifier reads where a form needs it: an added
// column's type, its default's cast and function, and whether the table a created index is built
// on is new. Its first pass also reads, for each statement, whether a relation it names is of
// pg_catalog or reads one (takesLaterExemptions), and for each statement on the keep-on list,
// whether a function, a table or a view it creates has the name of a function or a type of
// pg_catalog, or of a cast's function (keepsReadsOn), since the answers decide laterReadsOff and
// laterExemptionsOff. With a nil catalog, or where the migration's own statements forbid the
// reads (catalogReadsOff), it reads nothing to admit a form, and every added column and every
// created index is judged on size; the other kinds need no read. An error Postgres raises for a
// read refuses the form it is for, and any other error is returned. The census's transaction is
// repeatable-read and read-only, so every read sees the catalog as it stands before any pending
// migration applies.
//
// The statements are read by Postgres's own parser, libpg_query (Postgres 17's scan.l and
// gram.y), run as WebAssembly in pure Go by github.com/wasilibs/go-pgquery, so the reading is the
// server's: a comment, a string literal or a dollar-quoted function body is a token of the
// statement it is in, never a statement, and two statements on one line are two parse nodes. A
// migration that does not parse is not exempt, and laterReadsOff and laterExemptionsOff say so:
// the census cannot see what it changes, and a migration can parse on the PostgreSQL 18 server and
// not in the census's Postgres 17 grammar (a VIRTUAL generated column). A failure of the parser
// itself is answered the same way, so the size rule stands whenever the reading cannot be had.
func tableSizeExemption(ctx context.Context, catalog pgx.Tx, sql string) (classification, error) {
	statements, ok := parseStatements(sql)
	if !ok {
		return classification{
			laterReadsOff:      true,
			laterExemptionsOff: "does not parse" + cannotSeeAnEventTrigger,
		}, nil
	}
	c := &classifier{ctx: ctx, catalog: catalog}
	readsOff, laterReadsOff, laterExemptionsOff, err := c.catalogReadsOff(statements)
	if err != nil {
		return classification{}, err
	}
	answer := classification{laterReadsOff: laterReadsOff, laterExemptionsOff: laterExemptionsOff}
	if readsOff {
		c.catalog = nil
	}
	var kinds []string
	for _, s := range statements {
		catalogOnly, ok := catalogOnlyStatements[s.node]
		if !ok {
			return answer, nil
		}
		kind, err := catalogOnly(c, s.body)
		if err != nil {
			return classification{}, err
		}
		if kind == "" {
			return answer, nil
		}
		if !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	answer.exemption = strings.Join(kinds, " and ")
	return answer, nil
}

// ddlEventTriggersQuery names, sorted and comma-separated, every enabled event trigger on
// ddl_command_start, ddl_command_end or sql_drop whose tags are unset or name one of the command
// tags below, "" when there are none: the tag of each kind catalogOnlyStatements admits, CREATE
// INDEX among them, which the separate concurrent exception reads too. A column rename's tag is
// ALTER TABLE, and LOCK TABLE, SET and RESET fire no event trigger. A drop fires sql_drop as well
// as the two ddl_command_ events, so an event trigger on sql_drop would run code of its own on a
// drop the exemption covers. Census reads this once for the whole database rather than per
// migration, so such a trigger on sql_drop takes the exemption from every migration, one holding
// no drop included: the reading that refuses, not the one that applies. An event trigger that a
// pending migration creates or enables is not in pg_event_trigger, or not enabled there, yet. So
// tableSizeExemption reads the statements instead (takesLaterExemptions), and a migration whose
// statement can create or enable one takes the exemption from every pending migration after it,
// whatever the trigger's tags and whether it is enabled.
const ddlEventTriggersQuery = `
	select coalesce(string_agg(evtname, ', ' order by evtname), '')
	from pg_event_trigger
	where evtenabled <> 'D' and evtevent in ('ddl_command_start', 'ddl_command_end', 'sql_drop')
		and (evttags is null or evttags && array['CREATE FUNCTION', 'CREATE PROCEDURE', 'CREATE TRIGGER', 'DROP FUNCTION', 'DROP TRIGGER', 'DROP INDEX', 'ALTER TABLE', 'CREATE INDEX', 'CREATE TABLE', 'CREATE VIEW', 'DROP TABLE'])`

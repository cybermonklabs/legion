package api

// The reading of one statement's parse tree (m19_statement_binding_test.go holds the rule this
// serves): which rows each table of the statement answers, the walk that records its uses, its
// equalities and its constants, and the node helpers the walk reads the tree with. The scanner of
// its fragments is m19_statement_spans_test.go, and which of the rows a clause binds is
// m19_statement_bind_test.go.

import (
	"fmt"
	"maps"
	"slices"
)

// m19Item is one entry of a statement's range table: a table it reads, or a subselect, a common
// table expression or a function it reads through.
type m19Item struct {
	// table is the base table the item reads, empty for an item that is no table of its own.
	table string
	// alias is what a qualified column of the item names it by.
	alias string
	// location is where the item stands in the statement, -1 for an item no text of its own
	// names.
	location int
	// source is the common table expression a reference to one reads, whose columns are filled
	// in once its body is read.
	source *m19Item
	// outputs are the columns the item answers, in order.
	outputs []m19Output
	// known says whether outputs names every column the item answers: a function answers columns
	// the statement does not name.
	known bool
	// written says the statement writes the item's rows rather than reading them (an insert's
	// target).
	written bool
	// boundBy are the columns of the item a clause, or an equality to a row one binds, has
	// bound: the row itself where the column names what owns it, and the node it names where
	// the column is one half of a node's address.
	boundBy map[string]bool
	// parameter says the item stands for a parameter of the statement rather than a relation: a
	// value a clause may bind, which holds every row whose own key equals it.
	parameter bool
	// viewer says the parameter is the one every clause reads the reader from, which names no
	// row of a table: each read of it casts it to jsonb.
	viewer bool
	// rejected says a condition of the statement drops the item's null rows, so an outer join
	// over it answers only its matched rows.
	rejected bool
}

// bind records that one column of the item is bound.
func (i *m19Item) bind(column string) {
	if i.boundBy == nil {
		i.boundBy = map[string]bool{}
	}
	i.boundBy[column] = true
}

// columns are the columns the item answers: a reference to a common table expression answers that
// expression's, which are filled in after the reference is read.
func (i *m19Item) columns() []m19Output {
	if i.source != nil {
		return i.source.outputs
	}
	return i.outputs
}

// m19Output is one column an item answers and where its value comes from: the one column it is,
// or the columns it coalesces (`coalesce(i.project_key, ar.project_key)`).
type m19Output struct {
	name     string
	sources  []m19Ref
	coalesce []m19Ref
}

// m19Ref is one column of one item.
type m19Ref struct {
	item   *m19Item
	column string
}

// m19Use is one place a statement reads a column.
type m19Use struct {
	ref      m19Ref
	location int
}

// m19Equality is one equality between two columns that holds the rows of the items it filters.
type m19Equality struct {
	left, right m19Ref
	// filter answers the items whose rows the equality holds, read after the statement is, since
	// an outer join's condition holds its other side only where a condition of the statement
	// drops that side's null rows.
	filter func() []*m19Item
	// inJoin says the equality is a join's condition rather than a statement's own.
	inJoin bool
	// oneWay says a binding crosses it from right to left alone: `x = any($1)` holds each x to
	// an element of $1, and a bound x says nothing of the array's other elements.
	oneWay bool
}

// m19Call is one call of a function, where the statement makes it, and the column or parameter
// each argument is (a zero m19Ref for an argument that is neither): a function's body reads its
// parameters as the statement binds the arguments it is called with.
type m19Call struct {
	name     string
	location int
	args     []m19Ref
}

// m19Scope is one level of name resolution: the items a column of it may name, and the items of
// the scopes above whose rows its own conditions hold.
type m19Scope struct {
	parent   *m19Scope
	ctes     map[string]*m19Item
	items    []*m19Item
	filtered []*m19Item
}

// filters are the items one condition of the scope holds: its own, and those of the scopes above
// that it filters.
func (s *m19Scope) filters() []*m19Item {
	return append(slices.Clone(s.items), s.filtered...)
}

// cte records a common table expression under name, for this scope and those below it.
func (s *m19Scope) cte(name string, item *m19Item) {
	if s.ctes == nil {
		s.ctes = map[string]*m19Item{}
	}
	s.ctes[name] = item
}

// lookupCTE is the common table expression name stands for, nil where none does.
func (s *m19Scope) lookupCTE(name string) *m19Item {
	for scope := s; scope != nil; scope = scope.parent {
		if item, held := scope.ctes[name]; held {
			return item
		}
	}
	return nil
}

// m19Context is where in its statement an expression stands: whether it is a conjunct of the
// condition that holds its scope's rows, and the join whose condition it is.
type m19Context struct {
	conjunct bool
	join     *m19Join
}

// plain is the context of an expression under one that is no conjunction: nothing it says holds a
// row on its own.
func (c m19Context) plain() m19Context { return m19Context{join: c.join} }

// m19Join is one join of a statement: its kind and the items of each side.
type m19Join struct {
	kind        string
	left, right []*m19Item
}

// m19Analysis is one statement read.
type m19Analysis struct {
	schema *m19Schema
	spans  []m19Span
	items  []*m19Item
	uses   []m19Use
	equals []*m19Equality
	calls  []m19Call
	// coalesces are the equalities of a column to a coalesce of columns in a conjunct, which bind
	// the row the coalesce reads the owners of (bindOwned). A column of a set operation coalesces
	// nothing: either arm answers it.
	coalesces []m19Coalesce
	// derivations are the equalities of a column to an expression of one row's own id.
	derivations []m19Derivation
	// disjunctions are the ORs of a conjunct, each the equalities of each of its arms.
	disjunctions [][][]*m19Equality
	// scalars are the scalar subqueries of one column of one source, by where each stands.
	scalars map[int]m19Ref
	// parameters are the items standing for the statement's parameters, by number.
	parameters map[int]*m19Item
	// arguments are the names of the arguments of the function whose body the statement is,
	// which the body may read them by in place of $1, $2.
	arguments []string
	// constants are the values a conjunct of the statement pins a column to, which say of a
	// node's address which kinds of node it can name.
	constants map[m19Ref][]string
	err       error
}

// parameter is the item standing for the statement's parameter number.
func (a *m19Analysis) parameter(number int) *m19Item {
	if a.parameters == nil {
		a.parameters = map[int]*m19Item{}
	}
	held, named := a.parameters[number]
	if !named {
		held = &m19Item{alias: fmt.Sprintf("$%d", number), location: -1, parameter: true}
		a.parameters[number] = held
	}
	return held
}

// pin records that a conjunct holds a column to one of values.
func (a *m19Analysis) pin(ref m19Ref, values ...string) {
	if a.constants == nil {
		a.constants = map[m19Ref][]string{}
	}
	a.constants[ref] = append(a.constants[ref], values...)
}

// fail records the first reason the census cannot read the statement.
func (a *m19Analysis) fail(format string, args ...any) {
	if a.err == nil {
		a.err = fmt.Errorf(format, args...)
	}
}

// spanAt is the outermost fragment the statement's byte at location stands in.
func (a *m19Analysis) spanAt(location int) (m19Span, bool) {
	found, held := m19Span{}, false
	for _, span := range a.spans {
		if location < span.start || location >= span.end {
			continue
		}
		if !held || span.start < found.start {
			found, held = span, true
		}
	}
	return found, held
}

// statement reads one statement of the tree.
func (a *m19Analysis) statement(node any, parent *m19Scope) {
	kind, body, wrapped := m19Node(node)
	if !wrapped {
		return
	}
	switch kind {
	case "SelectStmt":
		a.selectBody(body, parent, nil)
	case "UpdateStmt", "DeleteStmt":
		// The rows the statement changes are the rows it read to find them, under its own
		// condition.
		scope := &m19Scope{parent: parent}
		a.rangeVar(body["relation"], scope, false)
		for _, from := range m19List(body["fromClause"]) {
			a.fromItem(from, scope)
		}
		for _, from := range m19List(body["usingClause"]) {
			a.fromItem(from, scope)
		}
		a.expression(body["whereClause"], scope, m19Context{conjunct: true})
		a.expression(body["targetList"], scope, m19Context{})
		a.expression(body["returningList"], scope, m19Context{})
	case "InsertStmt":
		// An insert reads no row of its target: it writes rows of its own.
		scope := &m19Scope{parent: parent}
		a.rangeVar(body["relation"], scope, true)
		a.statement(body["selectStmt"], scope)
		a.expression(body["onConflictClause"], scope, m19Context{})
		a.expression(body["returningList"], scope, m19Context{})
	default:
		a.expression(node, parent, m19Context{})
	}
}

// selectBody reads one SELECT and answers the columns it names. filtered are the items of the
// scopes above whose rows this select's own conditions hold: those of the EXISTS or the = ANY it
// stands in.
func (a *m19Analysis) selectBody(body map[string]any, parent *m19Scope, filtered []*m19Item) ([]m19Output, *m19Scope) {
	scope := &m19Scope{parent: parent, filtered: filtered}
	if with, held := body["withClause"].(map[string]any); held {
		a.withClause(with, scope)
	}
	if operation, _ := body["op"].(string); operation != "" && operation != "SETOP_NONE" {
		// Each arm of a set operation reads under the scope that holds the common table
		// expressions, and answers rows of its own: a column of the result comes from either.
		left, _ := a.selectArm(body["larg"], scope)
		right, _ := a.selectArm(body["rarg"], scope)
		return m19MergeOutputs(left, right), scope
	}
	for _, from := range m19List(body["fromClause"]) {
		a.fromItem(from, scope)
	}
	outputs := a.targetList(body["targetList"], scope)
	a.expression(body["whereClause"], scope, m19Context{conjunct: true})
	for _, clause := range []string{
		"distinctClause", "groupClause", "groupingSets", "havingClause", "windowClause", "valuesLists",
		"sortClause", "limitOffset", "limitCount", "lockingClause", "returningList",
	} {
		a.expression(body[clause], scope, m19Context{})
	}
	return outputs, scope
}

// selectArm reads one arm of a set operation, which the tree writes as a select's own body.
func (a *m19Analysis) selectArm(node any, parent *m19Scope) ([]m19Output, *m19Scope) {
	arm, held := node.(map[string]any)
	if !held {
		return nil, nil
	}
	return a.selectBody(arm, parent, nil)
}

// m19MergeOutputs is the columns a set operation answers: each column of the left arm beside the
// right arm's at its place, since a row of the result comes from either.
func m19MergeOutputs(left, right []m19Output) []m19Output {
	merged := make([]m19Output, 0, max(len(left), len(right)))
	for index := range max(len(left), len(right)) {
		output := m19Output{}
		if index < len(left) {
			output = m19Output{name: left[index].name, sources: slices.Clone(left[index].sources)}
		}
		if index < len(right) {
			if output.name == "" {
				output.name = right[index].name
			}
			output.sources = append(output.sources, right[index].sources...)
		}
		merged = append(merged, output)
	}
	return merged
}

// withClause reads the common table expressions of one statement. A recursive one is recorded
// before its own body is read, so the reference its body makes to it answers that body's columns
// once they are known.
func (a *m19Analysis) withClause(with map[string]any, scope *m19Scope) {
	recursive, _ := with["recursive"].(bool)
	for _, held := range m19List(with["ctes"]) {
		kind, cte, wrapped := m19Node(held)
		if !wrapped || kind != "CommonTableExpr" {
			continue
		}
		name := m19Text(cte["ctename"])
		item := &m19Item{alias: name, location: -1}
		if recursive {
			scope.cte(name, item)
		}
		item.outputs = m19RenameOutputs(a.query(cte["ctequery"], scope), cte["aliascolnames"])
		item.known = true
		scope.cte(name, item)
	}
}

// query reads a statement that answers columns, and answers them.
func (a *m19Analysis) query(node any, scope *m19Scope) []m19Output {
	kind, body, wrapped := m19Node(node)
	if wrapped && kind == "SelectStmt" {
		outputs, _ := a.selectBody(body, scope, nil)
		return outputs
	}
	a.statement(node, scope)
	return nil
}

// m19RenameOutputs is outputs under the names an alias gives them, where it gives any.
func m19RenameOutputs(outputs []m19Output, names any) []m19Output {
	for index, name := range m19List(names) {
		if index >= len(outputs) {
			break
		}
		outputs[index].name = m19Text(name)
	}
	return outputs
}

// fromItem reads one item of a FROM list into scope, and answers the items it added.
func (a *m19Analysis) fromItem(node any, scope *m19Scope) []*m19Item {
	kind, body, wrapped := m19Node(node)
	if !wrapped {
		return nil
	}
	switch kind {
	case "RangeVar":
		return a.rangeVar(node, scope, false)
	case "JoinExpr":
		left := a.fromItem(body["larg"], scope)
		right := a.fromItem(body["rarg"], scope)
		join := &m19Join{kind: m19Text(body["jointype"]), left: left, right: right}
		a.expression(body["quals"], scope, m19Context{conjunct: true, join: join})
		for _, using := range m19List(body["usingClause"]) {
			a.using(m19Text(using), join, scope)
		}
		return append(left, right...)
	case "RangeSubselect":
		lateral, _ := body["lateral"].(bool)
		under := scope
		if !lateral {
			under = &m19Scope{parent: scope.parent, ctes: scope.ctes}
		}
		outputs, _ := a.selectArmOf(body["subquery"], under)
		alias, columns := m19Alias(body["alias"])
		item := &m19Item{alias: alias, location: -1, outputs: m19RenameOutputs(outputs, columns), known: true}
		scope.items = append(scope.items, item)
		return []*m19Item{item}
	case "RangeFunction":
		a.expression(body["functions"], scope, m19Context{})
		alias, columns := m19Alias(body["alias"])
		if alias == "" {
			alias = m19FirstFunctionName(body["functions"])
		}
		item := &m19Item{alias: alias, location: -1}
		for _, name := range m19List(columns) {
			item.outputs = append(item.outputs, m19Output{name: m19Text(name)})
		}
		item.known = len(item.outputs) > 0
		scope.items = append(scope.items, item)
		return []*m19Item{item}
	default:
		a.fail("the census cannot read a %s in a FROM list", kind)
		return nil
	}
}

// selectArmOf reads a wrapped select and answers its columns.
func (a *m19Analysis) selectArmOf(node any, scope *m19Scope) ([]m19Output, *m19Scope) {
	kind, body, wrapped := m19Node(node)
	if !wrapped || kind != "SelectStmt" {
		a.statement(node, scope)
		return nil, nil
	}
	return a.selectBody(body, scope, nil)
}

// rangeVar reads one relation a statement names: a base table, or the common table expression its
// name stands for. written says the statement writes its rows rather than reading them.
func (a *m19Analysis) rangeVar(node any, scope *m19Scope, written bool) []*m19Item {
	body, held := node.(map[string]any)
	if !held {
		return nil
	}
	if kind, inner, wrapped := m19Node(node); wrapped && kind == "RangeVar" {
		body = inner
	}
	name, schemaName := m19Text(body["relname"]), m19Text(body["schemaname"])
	alias, _ := m19Alias(body["alias"])
	if alias == "" {
		alias = name
	}
	location := m19Location(body)
	if cte := scope.lookupCTE(name); cte != nil && (schemaName == "" || schemaName == "public") {
		item := &m19Item{alias: alias, location: -1, source: cte, known: true}
		scope.items = append(scope.items, item)
		return []*m19Item{item}
	}
	if schemaName != "" && schemaName != "public" {
		return nil // a catalogue read: pg_catalog holds no row of a project
	}
	switch a.schema.kinds[name] {
	case "r", "p", "v", "m":
		// A view answers the rows its own columns address, and a statement binds those columns
		// as it binds a table's: what the view reads to answer them is the view's own business,
		// stated once in a migration.
	case "":
		a.fail("the statement reads %s, which the schema does not hold", name)
		return nil
	default:
		a.fail("the statement reads %s, which is no relation the census can read", name)
		return nil
	}
	item := &m19Item{table: name, alias: alias, location: location, written: written}
	scope.items = append(scope.items, item)
	a.items = append(a.items, item)
	return []*m19Item{item}
}

// using records the equalities a join's USING list states: one column of each side, by name.
func (a *m19Analysis) using(column string, join *m19Join, scope *m19Scope) {
	left, leftHeld := a.itemWithColumn(join.left, column)
	right, rightHeld := a.itemWithColumn(join.right, column)
	if !leftHeld || !rightHeld {
		return
	}
	a.equate(m19Ref{item: left, column: column}, m19Ref{item: right, column: column}, scope, m19Context{conjunct: true, join: join})
}

// itemWithColumn is the one item of a side that answers a column.
func (a *m19Analysis) itemWithColumn(items []*m19Item, column string) (*m19Item, bool) {
	for _, item := range items {
		if item.table != "" && a.schema.columns[item.table][column] {
			return item, true
		}
		if slices.ContainsFunc(item.columns(), func(output m19Output) bool { return output.name == column }) {
			return item, true
		}
	}
	return nil, false
}

// targetList reads the columns a select names and answers them.
func (a *m19Analysis) targetList(node any, scope *m19Scope) []m19Output {
	var outputs []m19Output
	for _, held := range m19List(node) {
		kind, target, wrapped := m19Node(held)
		if !wrapped || kind != "ResTarget" {
			continue
		}
		value := target["val"]
		a.expression(value, scope, m19Context{})
		if star, all := a.starOutputs(value, scope); all {
			outputs = append(outputs, star...)
			continue
		}
		name := m19Text(target["name"])
		if name == "" {
			name = m19ColumnName(value)
		}
		args, _ := a.coalesceArgs(value, scope)
		outputs = append(outputs, m19Output{name: name, sources: a.sourcesOf(value, scope), coalesce: args})
	}
	return outputs
}

// starOutputs is the columns a `*` of a target list names, and whether the target is one.
func (a *m19Analysis) starOutputs(value any, scope *m19Scope) ([]m19Output, bool) {
	kind, body, wrapped := m19Node(value)
	if !wrapped || kind != "ColumnRef" {
		return nil, false
	}
	fields := m19List(body["fields"])
	if len(fields) == 0 {
		return nil, false
	}
	if last, _, held := m19Node(fields[len(fields)-1]); !held || last != "A_Star" {
		return nil, false
	}
	qualifier := ""
	if len(fields) > 1 {
		qualifier = m19Text(fields[len(fields)-2])
	}
	var outputs []m19Output
	for _, item := range scope.items {
		if qualifier != "" && item.alias != qualifier {
			continue
		}
		if item.table != "" {
			for _, column := range slices.Sorted(maps.Keys(a.schema.columns[item.table])) {
				outputs = append(outputs, m19Output{name: column, sources: []m19Ref{{item: item, column: column}}})
			}
			continue
		}
		for _, output := range item.columns() {
			outputs = append(outputs, m19Output{name: output.name, sources: []m19Ref{{item: item, column: output.name}}})
		}
	}
	return outputs, true
}

// sourcesOf is where the value of one expression comes from, for an expression that is one
// column or one parameter (`select $1::text as key`): a column of the result that holds a row's
// own value, or the parameter's, which a clause over it binds.
func (a *m19Analysis) sourcesOf(value any, scope *m19Scope) []m19Ref {
	if ref, held := a.refOf(value, scope); held {
		return []m19Ref{ref}
	}
	return nil
}

// m19ColumnName is the name a select gives a column it does not name itself: the column's own.
func m19ColumnName(value any) string {
	kind, body, wrapped := m19Node(value)
	if !wrapped {
		return ""
	}
	switch kind {
	case "ColumnRef":
		fields := m19List(body["fields"])
		if len(fields) == 0 {
			return ""
		}
		return m19Text(fields[len(fields)-1])
	case "TypeCast":
		return m19ColumnName(body["arg"])
	}
	return ""
}

// expression reads one expression of a statement, and everything under it.
func (a *m19Analysis) expression(node any, scope *m19Scope, ctx m19Context) {
	switch held := node.(type) {
	case []any:
		for _, item := range held {
			a.expression(item, scope, ctx)
		}
	case map[string]any:
		kind, body, wrapped := m19Node(node)
		if !wrapped {
			for _, field := range held {
				a.expression(field, scope, ctx.plain())
			}
			return
		}
		a.node(kind, body, node, scope, ctx)
	}
}

// node reads one node of the tree.
func (a *m19Analysis) node(kind string, body map[string]any, node any, scope *m19Scope, ctx m19Context) {
	switch kind {
	case "ColumnRef":
		if ref, held := a.resolve(body, scope); held {
			a.uses = append(a.uses, m19Use{ref: ref, location: m19Location(body)})
		}
	case "ParamRef":
		number, held := body["number"].(float64)
		if !held {
			return
		}
		a.uses = append(a.uses, m19Use{
			ref:      m19Ref{item: a.parameter(int(number)), column: fmt.Sprintf("$%d", int(number))},
			location: m19Location(body),
		})
	case "TypeCast":
		a.typeCast(body, scope, ctx)
	case "NullTest":
		// `node_kind is null` in a conjunct says the row names no node there: its address can
		// name nothing a project owns (m19KindsOfNoProject holds the empty kind).
		if ctx.conjunct && m19Text(body["nulltesttype"]) == "IS_NULL" {
			if ref, held := a.refOf(body["arg"], scope); held {
				a.pin(ref, "")
			}
		}
		a.expression(body["arg"], scope, ctx.plain())
	case "A_Expr":
		a.expression(body["lexpr"], scope, ctx.plain())
		a.expression(body["rexpr"], scope, ctx.plain())
		if !ctx.conjunct || m19OperatorName(body["name"]) != "=" {
			return
		}
		switch m19Text(body["kind"]) {
		case "AEXPR_OP":
			a.equateExpressions(body["lexpr"], body["rexpr"], scope, ctx)
			a.pinConstant(body["lexpr"], body["rexpr"], scope)
			a.pinConstant(body["rexpr"], body["lexpr"], scope)
		case "AEXPR_OP_ANY":
			// `comment_id = any($1::uuid[])`: every row's column equals one element of the
			// array, which a clause binds as a whole.
			recorded := len(a.equals)
			a.equateExpressions(body["lexpr"], body["rexpr"], scope, ctx)
			for _, equal := range a.equals[recorded:] {
				equal.oneWay = true
			}
		case "AEXPR_IN":
			a.pinConstant(body["lexpr"], body["rexpr"], scope)
		}
	case "BoolExpr":
		operator := m19Text(body["boolop"])
		if ctx.conjunct && operator == "OR_EXPR" {
			a.expression(body["args"], scope, ctx.plain())
			a.disjunction(body, scope, ctx)
			return
		}
		under := m19Context{join: ctx.join, conjunct: ctx.conjunct && operator == "AND_EXPR"}
		a.expression(body["args"], scope, under)
	case "SubLink":
		a.subLink(body, scope, ctx)
	case "FuncCall":
		call := m19Call{name: m19LastName(body["funcname"]), location: m19Location(body)}
		for _, arg := range m19List(body["args"]) {
			ref, _ := a.refOf(arg, scope)
			call.args = append(call.args, ref)
		}
		a.calls = append(a.calls, call)
		for name, field := range body {
			if name != "funcname" {
				a.expression(field, scope, ctx.plain())
			}
		}
	case "SelectStmt", "InsertStmt", "UpdateStmt", "DeleteStmt":
		a.statement(node, scope)
	default:
		for _, field := range body {
			a.expression(field, scope, ctx.plain())
		}
	}
}

// typeCast reads a cast. The viewer every clause reads is the parameter a statement casts to
// jsonb: it names no row of a table, so a clause that reads it binds nothing by it.
func (a *m19Analysis) typeCast(body map[string]any, scope *m19Scope, ctx m19Context) {
	if kind, parameter, wrapped := m19Node(body["arg"]); wrapped && kind == "ParamRef" && m19OperatorName(m19TypeName(body["typeName"])) == "jsonb" {
		if number, held := parameter["number"].(float64); held {
			a.parameter(int(number)).viewer = true
		}
	}
	for name, field := range body {
		if name != "typeName" {
			a.expression(field, scope, ctx.plain())
		}
	}
}

// pinConstant records the values a conjunct holds a column to, from an equality to a constant or
// a list of them: what a node's address can name, where the column says which kind it is. A list
// holding one value the census cannot read pins nothing.
func (a *m19Analysis) pinConstant(column, value any, scope *m19Scope) {
	ref, held := a.refOf(column, scope)
	if !held {
		return
	}
	if text, constant := m19ConstantText(value); constant {
		a.pin(ref, text)
		return
	}
	kind, list, wrapped := m19Node(value)
	if !wrapped || kind != "List" {
		return
	}
	var values []string
	for _, held := range m19List(list["items"]) {
		text, constant := m19ConstantText(held)
		if !constant {
			return
		}
		values = append(values, text)
	}
	a.pin(ref, values...)
}

// m19ConstantText is the text of a constant, and whether the node is one.
func m19ConstantText(node any) (string, bool) {
	kind, body, wrapped := m19Node(node)
	if !wrapped {
		return "", false
	}
	switch kind {
	case "TypeCast":
		return m19ConstantText(body["arg"])
	case "A_Const":
		text, held := body["sval"].(map[string]any)
		if !held {
			return "", false
		}
		return m19Text(text["sval"]), true
	}
	return "", false
}

// m19TypeName is the name of a type a cast names.
func m19TypeName(value any) any {
	name, held := value.(map[string]any)
	if !held {
		return nil
	}
	return name["names"]
}

// subLink reads a select inside an expression. One that stands in a conjunct of its reader's
// condition - an EXISTS, an = ANY - holds the rows of the items that reader reads, so an equality
// of its own binds them.
func (a *m19Analysis) subLink(body map[string]any, scope *m19Scope, ctx m19Context) {
	a.expression(body["testexpr"], scope, ctx.plain())
	kind := m19Text(body["subLinkType"])
	var filtered []*m19Item
	holds := ctx.conjunct && !ctx.inJoin() && (kind == "EXISTS_SUBLINK" || kind == "ANY_SUBLINK")
	if holds {
		filtered = scope.filters()
	}
	selectKind, selected, wrapped := m19Node(body["subselect"])
	if !wrapped || selectKind != "SelectStmt" {
		a.expression(body["subselect"], scope, ctx.plain())
		return
	}
	outputs, inner := a.selectBody(selected, scope, filtered)
	if kind == "EXPR_SUBLINK" && len(outputs) == 1 && len(outputs[0].sources) == 1 {
		// A scalar subquery's one column, which an equality over it reads as that column's.
		if a.scalars == nil {
			a.scalars = map[int]m19Ref{}
		}
		a.scalars[m19Location(body)] = outputs[0].sources[0]
	}
	if !holds || kind != "ANY_SUBLINK" || m19OperatorName(body["operName"]) != "=" || len(outputs) != 1 {
		return
	}
	// `x = any (select y from …)`: the rows of x are held to the rows the select answers, and the
	// select's own rows to the rows of x.
	left, held := a.refOf(body["testexpr"], scope)
	if !held {
		return
	}
	for _, source := range outputs[0].sources {
		a.record(left, source, func() []*m19Item { return append(scope.filters(), inner.items...) }, false)
	}
}

// disjunction keeps the equalities of each arm of an OR in a conjunct apart: an arm holds only the
// rows that take it, so its equalities bind nothing alone, and a row every arm holds by an
// equality of its own is held by the whole (`c.id = p.thread_id or c.reply_to = p.thread_id`).
// The arms were read as plain expressions already; this reads only their top-level equalities.
func (a *m19Analysis) disjunction(body map[string]any, scope *m19Scope, ctx m19Context) {
	arms := make([][]*m19Equality, 0, len(m19List(body["args"])))
	for _, arg := range m19List(body["args"]) {
		equals, coalesces, derivations := len(a.equals), len(a.coalesces), len(a.derivations)
		a.armEqualities(arg, scope, ctx)
		arms = append(arms, slices.Clone(a.equals[equals:]))
		a.equals, a.coalesces, a.derivations = a.equals[:equals], a.coalesces[:coalesces], a.derivations[:derivations]
	}
	a.disjunctions = append(a.disjunctions, arms)
}

// armEqualities records the equalities of one arm of an OR: its own, and those of the AND it is.
func (a *m19Analysis) armEqualities(node any, scope *m19Scope, ctx m19Context) {
	kind, body, wrapped := m19Node(node)
	if !wrapped {
		return
	}
	switch {
	case kind == "A_Expr" && m19OperatorName(body["name"]) == "=" &&
		(m19Text(body["kind"]) == "AEXPR_OP" || m19Text(body["kind"]) == "AEXPR_OP_ANY"):
		a.equateExpressions(body["lexpr"], body["rexpr"], scope, m19Context{conjunct: true, join: ctx.join})
	case kind == "BoolExpr" && m19Text(body["boolop"]) == "AND_EXPR":
		for _, arg := range m19List(body["args"]) {
			a.armEqualities(arg, scope, ctx)
		}
	}
}

// inJoin says the context is a join's condition.
func (c m19Context) inJoin() bool { return c.join != nil }

// equateExpressions records the equality of two expressions, a row of columns against a row of
// columns included.
func (a *m19Analysis) equateExpressions(left, right any, scope *m19Scope, ctx m19Context) {
	leftRow, leftIsRow := m19RowArgs(left)
	rightRow, rightIsRow := m19RowArgs(right)
	if leftIsRow && rightIsRow {
		for index := range min(len(leftRow), len(rightRow)) {
			a.equateExpressions(leftRow[index], rightRow[index], scope, ctx)
		}
		return
	}
	leftRef, leftHeld := a.refOf(left, scope)
	rightRef, rightHeld := a.refOf(right, scope)
	if leftHeld && rightHeld {
		a.equate(leftRef, rightRef, scope, ctx)
		return
	}
	// A column against coalesce(...) of columns: the row an issue or a document owns, read under
	// its owner's project (`coalesce(i.project_key, ar.project_key)`).
	column, other := leftRef, right
	if rightHeld {
		column, other = rightRef, left
	}
	if !leftHeld && !rightHeld {
		return
	}
	if args, coalesced := a.coalesceArgs(other, scope); coalesced {
		a.coalesces = append(a.coalesces, m19Coalesce{args: args, other: column, filter: a.filterOf(scope, ctx)})
		return
	}
	// A column against an expression of one column of one row (`m.id = (case when split_part(
	// e.id, '#', 1) ~ … then …::uuid end)`): the row whose own id parses to the column's value.
	if target, derived := a.derivedFrom(other, scope); derived {
		a.derivations = append(a.derivations, m19Derivation{value: column, target: target, filter: a.filterOf(scope, ctx)})
	}
}

// m19Derivation is one equality between a column and an expression of one column of one row
// that names that row (an id, a key, a node's address): where the column holds a bound value, the
// row is the one it names.
type m19Derivation struct {
	value, target m19Ref
	filter        func() []*m19Item
}

// derivedFrom is the one column an expression reads, where every column it reads is that one and
// it binds the row it is of (plain or a node's address), and whether there is one. An expression
// holding a select reads more than its own row, and is none.
func (a *m19Analysis) derivedFrom(node any, scope *m19Scope) (m19Ref, bool) {
	var columns []map[string]any
	if !m19ColumnRefs(node, &columns) || len(columns) == 0 {
		return m19Ref{}, false
	}
	var target m19Ref
	for index, column := range columns {
		ref, held := a.resolve(column, scope)
		if !held || index > 0 && ref != target {
			return m19Ref{}, false
		}
		target = ref
	}
	base := a.oneBase(target)
	if base == nil || base.item.parameter || !a.schema.binds[base.item.table][base.column] {
		return m19Ref{}, false
	}
	return *base, true
}

// m19ColumnRefs appends every ColumnRef under node, and answers false where node holds a select.
func m19ColumnRefs(node any, found *[]map[string]any) bool {
	switch held := node.(type) {
	case []any:
		for _, item := range held {
			if !m19ColumnRefs(item, found) {
				return false
			}
		}
	case map[string]any:
		kind, body, wrapped := m19Node(node)
		switch {
		case wrapped && kind == "ColumnRef":
			*found = append(*found, body)
			return true
		case wrapped && (kind == "SubLink" || kind == "SelectStmt"):
			return false
		}
		for _, field := range held {
			if !m19ColumnRefs(field, found) {
				return false
			}
		}
	}
	return true
}

// coalesceArgs are the columns a coalesce of columns reads, and whether the expression is one.
func (a *m19Analysis) coalesceArgs(node any, scope *m19Scope) ([]m19Ref, bool) {
	kind, body, wrapped := m19Node(node)
	if !wrapped || kind != "CoalesceExpr" {
		return nil, false
	}
	var args []m19Ref
	for _, arg := range m19List(body["args"]) {
		ref, held := a.refOf(arg, scope)
		if !held {
			return nil, false
		}
		args = append(args, ref)
	}
	return args, len(args) > 0
}

// m19Coalesce is one equality between a column and a coalesce of columns, with the items its
// filtering holds.
type m19Coalesce struct {
	args   []m19Ref
	other  m19Ref
	filter func() []*m19Item
}

// equate records one equality between two columns, with the items its filtering holds.
func (a *m19Analysis) equate(left, right m19Ref, scope *m19Scope, ctx m19Context) {
	a.record(left, right, a.filterOf(scope, ctx), ctx.join != nil)
}

// filterOf answers the items one condition holds: those its scope's WHERE filters, or the sides
// of the join whose condition it is (m19JoinFilters).
func (a *m19Analysis) filterOf(scope *m19Scope, ctx m19Context) func() []*m19Item {
	if ctx.join == nil {
		return scope.filters
	}
	join := ctx.join
	return func() []*m19Item { return m19JoinFilters(join) }
}

// record keeps one equality.
func (a *m19Analysis) record(left, right m19Ref, filter func() []*m19Item, inJoin bool) {
	a.equals = append(a.equals, &m19Equality{left: left, right: right, filter: filter, inJoin: inJoin})
}

// m19JoinFilters are the items a join's condition holds. An outer join answers every row of its
// own side whether the condition holds or not, so the condition holds that side only where
// something else of the statement drops the rows the join leaves null (a clause over the other
// side, which answers false for a row that is not there).
func m19JoinFilters(join *m19Join) []*m19Item {
	rejected := func(items []*m19Item) bool {
		return slices.ContainsFunc(items, func(item *m19Item) bool { return item.rejected })
	}
	switch join.kind {
	case "JOIN_LEFT":
		if rejected(join.right) {
			return append(slices.Clone(join.left), join.right...)
		}
		return join.right
	case "JOIN_RIGHT":
		if rejected(join.left) {
			return append(slices.Clone(join.left), join.right...)
		}
		return join.left
	case "JOIN_FULL":
		if rejected(join.left) && rejected(join.right) {
			return append(slices.Clone(join.left), join.right...)
		}
		return nil
	default:
		return append(slices.Clone(join.left), join.right...)
	}
}

// m19RowArgs are the columns of a row expression, and whether the expression is one.
func m19RowArgs(node any) ([]any, bool) {
	kind, body, wrapped := m19Node(node)
	if !wrapped || kind != "RowExpr" {
		return nil, false
	}
	return m19List(body["args"]), true
}

// refOf is the column one expression reads, for an expression that reads one.
func (a *m19Analysis) refOf(node any, scope *m19Scope) (m19Ref, bool) {
	kind, body, wrapped := m19Node(node)
	if !wrapped {
		return m19Ref{}, false
	}
	switch kind {
	case "ColumnRef":
		return a.resolve(body, scope)
	case "ParamRef":
		number, held := body["number"].(float64)
		if !held {
			return m19Ref{}, false
		}
		return m19Ref{item: a.parameter(int(number)), column: fmt.Sprintf("$%d", int(number))}, true
	case "TypeCast":
		return a.refOf(body["arg"], scope)
	case "SubLink":
		// `v.artifact_id = (select a.id from artifacts a where …)`: the subquery's one column,
		// which the expression's own walk read first.
		ref, held := a.scalars[m19Location(body)]
		return ref, held
	}
	return m19Ref{}, false
}

// resolve is the column of the statement one ColumnRef names: the item its qualifier names, or
// the one item of the nearest scope that answers a column of that name. A name the census cannot
// place - a function's parameter, a record of a PL/pgSQL body, a column of a function whose
// columns the statement does not name - is placed nowhere, and binds nothing.
func (a *m19Analysis) resolve(body map[string]any, scope *m19Scope) (m19Ref, bool) {
	var names []string
	for _, field := range m19List(body["fields"]) {
		kind, inner, wrapped := m19Node(field)
		if !wrapped || kind != "String" {
			return m19Ref{}, false // a star, or a subscript
		}
		names = append(names, m19Text(inner["sval"]))
	}
	if len(names) == 0 {
		return m19Ref{}, false
	}
	column := names[len(names)-1]
	if len(names) > 1 {
		qualifier := names[len(names)-2]
		for level := scope; level != nil; level = level.parent {
			for _, item := range level.items {
				if item.alias == qualifier {
					return m19Ref{item: item, column: column}, true
				}
			}
		}
		return m19Ref{}, false
	}
	for level := scope; level != nil; level = level.parent {
		var found []*m19Item
		uncertain := false
		for _, item := range level.items {
			switch {
			case item.table != "":
				if a.schema.columns[item.table][column] {
					found = append(found, item)
				}
			case item.known:
				if slices.ContainsFunc(item.columns(), func(output m19Output) bool { return output.name == column }) {
					found = append(found, item)
				}
			default:
				uncertain = true
			}
		}
		if len(found) == 1 {
			return m19Ref{item: found[0], column: column}, true
		}
		if len(found) > 1 {
			return m19Ref{}, false
		}
		if uncertain {
			break
		}
	}
	// A name no column of the statement answers is the function's argument of that name, where
	// the statement is a function's body: a column of its own takes it first, as Postgres reads
	// it.
	if number := slices.Index(a.arguments, column); number >= 0 {
		return m19Ref{item: a.parameter(number + 1), column: fmt.Sprintf("$%d", number+1)}, true
	}
	return m19Ref{}, false
}

// m19Node reads one node of the parse tree, which libpg_query writes as an object of one key, the
// node's type, holding its fields.
func m19Node(value any) (string, map[string]any, bool) {
	node, held := value.(map[string]any)
	if !held || len(node) != 1 {
		return "", nil, false
	}
	for kind, body := range node {
		if kind == "" || kind[0] < 'A' || kind[0] > 'Z' {
			return "", nil, false
		}
		fields, object := body.(map[string]any)
		if !object {
			return "", nil, false
		}
		return kind, fields, true
	}
	return "", nil, false
}

// m19List is a field the tree writes as a list of nodes.
func m19List(value any) []any {
	held, _ := value.([]any)
	return held
}

// m19Text is a field the tree writes as a string, or as a String node.
func m19Text(value any) string {
	if text, held := value.(string); held {
		return text
	}
	if kind, body, wrapped := m19Node(value); wrapped && kind == "String" {
		return m19Text(body["sval"])
	}
	return ""
}

// m19Location is where in the statement a node stands, -1 for a node no text names.
func m19Location(body map[string]any) int {
	location, held := body["location"].(float64)
	if !held {
		return -1
	}
	return int(location)
}

// m19Alias is the name a FROM item is read under and the names it gives its columns.
func m19Alias(value any) (string, any) {
	alias, held := value.(map[string]any)
	if !held {
		return "", nil
	}
	return m19Text(alias["aliasname"]), alias["colnames"]
}

// m19OperatorName is the operator an expression applies.
func m19OperatorName(value any) string {
	names := m19List(value)
	if len(names) == 0 {
		return ""
	}
	return m19Text(names[len(names)-1])
}

// m19LastName is the name of a function a call names, without its schema.
func m19LastName(value any) string { return m19OperatorName(value) }

// m19FirstFunctionName is the name of the first function a FROM list's item calls.
func m19FirstFunctionName(value any) string {
	for _, held := range m19List(value) {
		kind, body, wrapped := m19Node(held)
		if wrapped && kind == "List" {
			return m19FirstFunctionName(body["items"])
		}
		if wrapped && kind == "FuncCall" {
			return m19LastName(body["funcname"])
		}
	}
	return ""
}

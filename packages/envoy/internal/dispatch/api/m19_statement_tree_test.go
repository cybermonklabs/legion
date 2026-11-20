package api

// The reading of one statement's parse tree (m19_statement_binding_test.go holds the rule this
// serves): which rows each table of the statement answers, which of them a clause binds, and
// which an equality on a row's own owner column holds to a bound row.

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// m19Span is one SQL fragment of a statement: where its comment begins, where the expression it
// opens ends, the name it carries, and whether a row whose own column it reads is bound by it.
type m19Span struct {
	start, end int
	name       string
	binds      bool
}

// m19Spans reads every fragment of one statement, as Postgres's scanner reads it: a marker inside
// a string, a quoted name or a dollar-quoted body is no marker. A fragment runs from its comment
// to the end of what follows it: a parenthesised group, a call, or the rest of the group it
// stands in, which is the whole statement for the read that resolves the viewer.
func m19Spans(sql string) ([]m19Span, error) {
	var spans []m19Span
	for index := 0; index < len(sql); {
		after := m19Skip(sql, index)
		if !strings.HasPrefix(sql[index:], "/*") {
			index = after
			continue
		}
		comment := sql[index:after]
		span := m19Span{start: index}
		switch {
		case strings.HasPrefix(comment, access.Marker):
			span.name = strings.TrimSuffix(strings.TrimPrefix(comment, access.Marker), " */")
			binds, named := m19BindingFragments[span.name]
			if !named {
				return nil, fmt.Errorf("the statement binds %q, a fragment the census does not know: name it in m19BindingFragments, with whether a row whose column it reads is bound by it", span.name)
			}
			span.binds = binds
		case strings.HasPrefix(comment, refs.VisibilityMarker):
			// Every predicate the registry renders answers who may see the node it is over.
			span.name, span.binds = strings.TrimSuffix(strings.TrimPrefix(comment, refs.VisibilityMarker), " */"), true
		default:
			index = after
			continue
		}
		span.end = m19FragmentEnd(sql, after)
		spans = append(spans, span)
		index = after
	}
	return spans, nil
}

// m19FragmentEnd is where the expression a fragment's comment opens, which starts at index, ends.
func m19FragmentEnd(sql string, index int) int {
	open := m19SkipSpace(sql, index)
	if open < len(sql) && sql[open] == '(' {
		return m19CloseParen(sql, open)
	}
	word := open
	for word < len(sql) && m19IsNameByte(sql[word]) {
		word++
	}
	if call := m19SkipSpace(sql, word); word > open && call < len(sql) && sql[call] == '(' {
		if name := strings.ToLower(sql[open:word]); name != "with" && name != "select" && name != "case" {
			return m19CloseParen(sql, call)
		}
	}
	// A fragment that is a statement of its own: to the end of the group it stands in.
	return m19EnclosingEnd(sql, open)
}

// m19CloseParen is the index after the parenthesis that closes the one at open.
func m19CloseParen(sql string, open int) int {
	for index, depth := open, 0; index < len(sql); {
		switch sql[index] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return index + 1
			}
		}
		index = m19Skip(sql, index)
	}
	return len(sql)
}

// m19EnclosingEnd is where the group index stands in ends: its first unmatched parenthesis, or
// the end of the statement.
func m19EnclosingEnd(sql string, index int) int {
	for depth := 0; index < len(sql); {
		switch sql[index] {
		case '(':
			depth++
		case ')':
			if depth--; depth < 0 {
				return index
			}
		}
		index = m19Skip(sql, index)
	}
	return len(sql)
}

// m19Skip is the index after the lexeme of sql at index, as Postgres's scanner reads it: a
// string, a quoted name, a dollar-quoted body and a comment are each one lexeme, whatever they
// hold.
func m19Skip(sql string, index int) int {
	switch {
	case strings.HasPrefix(sql[index:], "--"):
		if end := strings.IndexByte(sql[index:], '\n'); end >= 0 {
			return index + end + 1
		}
		return len(sql)
	case strings.HasPrefix(sql[index:], "/*"):
		for at, depth := index, 0; at < len(sql); {
			switch {
			case strings.HasPrefix(sql[at:], "/*"):
				depth, at = depth+1, at+2
			case strings.HasPrefix(sql[at:], "*/"):
				if depth, at = depth-1, at+2; depth == 0 {
					return at
				}
			default:
				at++
			}
		}
		return len(sql)
	case sql[index] == '\'' || sql[index] == '"':
		quote := sql[index]
		for at := index + 1; at < len(sql); at++ {
			if sql[at] != quote {
				continue
			}
			if at+1 < len(sql) && sql[at+1] == quote {
				at++
				continue
			}
			return at + 1
		}
		return len(sql)
	case sql[index] == '$':
		tag := m19DollarTag(sql, index)
		if tag == "" {
			return index + 1
		}
		if end := strings.Index(sql[index+len(tag):], tag); end >= 0 {
			return index + len(tag) + end + len(tag)
		}
		return len(sql)
	}
	return index + 1
}

// m19DollarTag is the tag of the dollar quote opening at index ("$$", "$body$"), empty where the
// dollar opens no quote: a parameter ($1) or a dollar of its own.
func m19DollarTag(sql string, index int) string {
	for at := index + 1; at < len(sql); at++ {
		if sql[at] == '$' {
			return sql[index : at+1]
		}
		if !m19IsNameByte(sql[at]) || (sql[at] >= '0' && sql[at] <= '9' && at == index+1) {
			return ""
		}
	}
	return ""
}

func m19IsNameByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b|0x20 >= 'a' && b|0x20 <= 'z')
}

func m19SkipSpace(sql string, index int) int {
	for index < len(sql) && (sql[index] == ' ' || sql[index] == '\t' || sql[index] == '\n' || sql[index] == '\r') {
		index++
	}
	return index
}

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

// bind marks every column of the statement a clause binds, then every column an equality holds to
// a bound one, until nothing more is bound.
func (a *m19Analysis) bind() {
	a.spreadConstants()
	for _, use := range a.uses {
		span, inside := a.spanAt(use.location)
		if inside && span.binds && use.ref.item.table == "" && !use.ref.item.parameter {
			// A clause over a column of a subselect binds that column's value: a row whose own
			// key equals it is one the clause passed (`c.id = r.id::uuid`, r the arms the
			// registry's predicate read).
			use.ref.item.bind(use.ref.column)
		}
		for _, ref := range a.baseRefs(use.ref) {
			if inside {
				// Every fragment answers false where the row it reads is not there, so a join
				// that answers its row as null answers it to nobody.
				ref.item.rejected = true
			}
			if !inside || !span.binds {
				continue
			}
			if ref.item.parameter && !ref.item.viewer || a.schema.binds[ref.item.table][ref.column] {
				ref.item.bind(ref.column)
			}
		}
	}
	for _, equal := range a.equals {
		if equal.inJoin {
			continue
		}
		for _, side := range []m19Ref{equal.left, equal.right} {
			for _, ref := range a.baseRefs(side) {
				ref.item.rejected = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, equal := range a.equals {
			filters := equal.filter()
			for index, pair := range [][2]m19Ref{{equal.left, equal.right}, {equal.right, equal.left}} {
				if equal.oneWay && index == 0 {
					continue
				}
				from, to := pair[0], pair[1]
				// The equality filters the rows of the item it names, which may be a subselect
				// or a common table expression: every row of a table that reaches the answer
				// through it is one whose column met the equality.
				if !to.item.parameter && !slices.Contains(filters, to.item) {
					continue
				}
				if args, coalesced := a.coalesceOf(to, map[m19Ref]bool{}); coalesced {
					changed = a.bindOwned(from, args) || changed
					continue
				}
				for _, target := range a.baseRefs(to) {
					if target.item.boundBy[target.column] || !a.carries(from, target) {
						continue
					}
					target.item.bind(target.column)
					changed = true
				}
			}
		}
		for _, held := range a.coalesces {
			args := make([]m19Ref, 0, len(held.args))
			for _, arg := range held.args {
				if base := a.oneBase(arg); base != nil {
					args = append(args, *base)
				}
			}
			if len(args) != len(held.args) {
				continue
			}
			filters := held.filter()
			if row, owned := a.rowOwnedBy(args); owned && slices.Contains(filters, row.item) {
				changed = a.bindOwned(held.other, args) || changed
			}
			// The other way: a coalesce of bound rows' own columns is a value of a bound row, so
			// the row whose own key it is is bound (`p.key = coalesce(i.project_key,
			// ar.project_key)` over the ask's issue and document).
			target := a.oneBase(held.other)
			if target == nil || target.item.parameter || target.item.boundBy[target.column] ||
				!a.schema.plain[target.item.table][target.column] || !slices.Contains(filters, target.item) {
				continue
			}
			if !slices.ContainsFunc(args, func(arg m19Ref) bool { return !a.holds(arg) }) {
				target.item.bind(target.column)
				changed = true
			}
		}
		for _, derived := range a.derivations {
			target := derived.target
			if target.item.boundBy[target.column] || !slices.Contains(derived.filter(), target.item) || !a.valueHolds(derived.value) {
				continue
			}
			target.item.bind(target.column)
			changed = true
		}
		for _, arms := range a.disjunctions {
			changed = a.bindDisjunction(arms) || changed
		}
	}
}

// bindDisjunction binds a row every arm of an OR holds by an equality of its own, on the column
// the first arm binds it by, and answers whether it bound something new.
func (a *m19Analysis) bindDisjunction(arms [][]*m19Equality) bool {
	if len(arms) == 0 {
		return false
	}
	for _, equal := range arms[0] {
		for _, candidate := range a.boundTargets(equal) {
			if candidate.item.boundBy[candidate.column] {
				continue
			}
			held := !slices.ContainsFunc(arms[1:], func(arm []*m19Equality) bool {
				return !slices.ContainsFunc(arm, func(other *m19Equality) bool {
					return slices.ContainsFunc(a.boundTargets(other), func(target m19Ref) bool { return target.item == candidate.item })
				})
			})
			if held {
				candidate.item.bind(candidate.column)
				return true
			}
		}
	}
	return false
}

// boundTargets are the columns of tables one equality binds on its own: those of the side it
// filters that the other side carries a binding to.
func (a *m19Analysis) boundTargets(equal *m19Equality) []m19Ref {
	var found []m19Ref
	filters := equal.filter()
	for index, pair := range [][2]m19Ref{{equal.left, equal.right}, {equal.right, equal.left}} {
		if equal.oneWay && index == 0 {
			continue
		}
		from, to := pair[0], pair[1]
		if to.item.parameter || !slices.Contains(filters, to.item) {
			continue
		}
		for _, target := range a.baseRefs(to) {
			if a.carries(from, target) {
				found = append(found, target)
			}
		}
	}
	return found
}

// valueHolds answers whether a column or parameter holds a value a clause bound: a parameter or a
// subselect's column a clause read, or a column every row it comes from is bound for.
func (a *m19Analysis) valueHolds(ref m19Ref) bool {
	if ref.item == nil {
		return false
	}
	if a.boundValue(ref, map[m19Ref]bool{}) {
		return true
	}
	bases := a.baseRefs(ref)
	return len(bases) > 0 && !slices.ContainsFunc(bases, func(base m19Ref) bool { return !a.holds(base) })
}

// boundValue answers whether a parameter, or a column of a subselect, holds a value a clause
// bound: the column itself, or the one column it comes from.
func (a *m19Analysis) boundValue(ref m19Ref, seen map[m19Ref]bool) bool {
	if ref.item == nil || seen[ref] {
		return false
	}
	seen[ref] = true
	switch {
	case ref.item.parameter:
		return !ref.item.viewer && ref.item.boundBy[ref.column]
	case ref.item.table != "":
		return false
	case ref.item.boundBy[ref.column]:
		return true
	}
	for _, output := range ref.item.columns() {
		if output.name == ref.column && len(output.sources) == 1 {
			return a.boundValue(output.sources[0], seen)
		}
	}
	return false
}

// coalesceOf is the columns of tables a column of a subselect or a common table expression
// coalesces (`coalesce(i.project_key, ar.project_key) as project_key`), each one column of one
// table, and whether the column is one.
func (a *m19Analysis) coalesceOf(ref m19Ref, seen map[m19Ref]bool) ([]m19Ref, bool) {
	if ref.item == nil || ref.item.table != "" || ref.item.parameter || seen[ref] {
		return nil, false
	}
	seen[ref] = true
	for _, output := range ref.item.columns() {
		if output.name != ref.column {
			continue
		}
		if len(output.coalesce) > 0 {
			args := make([]m19Ref, 0, len(output.coalesce))
			for _, arg := range output.coalesce {
				base := a.oneBase(arg)
				if base == nil {
					return nil, false
				}
				args = append(args, *base)
			}
			return args, true
		}
		if len(output.sources) == 1 {
			return a.coalesceOf(output.sources[0], seen)
		}
	}
	return nil, false
}

// bindOwned binds the row whose owners a coalesce reads, where the value it equals holds:
// `coalesce(i.project_key, ar.project_key) = p.key`, p bound, over asks a joined to i and ar by
// its own issue and document, is the rule ItemOwnerClause states, so the ask is bound. It answers
// whether it bound something new.
func (a *m19Analysis) bindOwned(value m19Ref, args []m19Ref) bool {
	if !a.valueHolds(value) {
		return false
	}
	row, owned := a.rowOwnedBy(args)
	if !owned || row.item.boundBy[row.column] {
		return false
	}
	row.item.bind(row.column)
	return true
}

// rowOwnedBy is the one row every column of a coalesce is the project of: the row itself, or the
// owner the row names by an owner column of its own, joined by that column. It answers the row
// and the owner column, and whether there is one.
func (a *m19Analysis) rowOwnedBy(args []m19Ref) (m19Ref, bool) {
	var found m19Ref
	for index, arg := range args {
		if !a.schema.plain[arg.item.table][arg.column] {
			return m19Ref{}, false
		}
		rows := a.rowsOwnedThrough(arg)
		if index == 0 {
			if len(rows) != 1 {
				return m19Ref{}, false
			}
			found = rows[0]
			continue
		}
		if !slices.ContainsFunc(rows, func(row m19Ref) bool { return row.item == found.item }) {
			return m19Ref{}, false
		}
	}
	return found, found.item != nil
}

// rowsOwnedThrough are the rows of the statement whose owner arg's row is, each with the owner
// column that joins it: arg's row itself (by arg's column), every row an equality holds to arg's
// row by an owner column of its own, and every row a coalesce of its own owner columns names
// arg's row by (`a.id = coalesce(k.artifact_id, k.block_artifact_id)`).
func (a *m19Analysis) rowsOwnedThrough(arg m19Ref) []m19Ref {
	var rows []m19Ref
	for _, equal := range a.equals {
		for _, pair := range [][2]m19Ref{{equal.left, equal.right}, {equal.right, equal.left}} {
			child, parent := a.oneBase(pair[0]), a.oneBase(pair[1])
			if child == nil || parent == nil || parent.item != arg.item || child.item == arg.item {
				continue
			}
			if owner := a.schema.owners[child.item.table][child.column]; owner == (m19Owner{table: parent.item.table, column: parent.column}) {
				rows = append(rows, *child)
			}
		}
	}
	for _, held := range a.coalesces {
		parent := a.oneBase(held.other)
		if parent == nil || parent.item != arg.item || len(held.args) == 0 {
			continue
		}
		owner := m19Owner{table: parent.item.table, column: parent.column}
		child := a.oneBase(held.args[0])
		if child == nil || child.item == arg.item || slices.ContainsFunc(held.args, func(column m19Ref) bool {
			base := a.oneBase(column)
			return base == nil || base.item != child.item || a.schema.owners[base.item.table][base.column] != owner
		}) {
			continue
		}
		rows = append(rows, *child)
	}
	if len(rows) == 0 {
		rows = append(rows, arg)
	}
	return rows
}

// carries answers whether a column of the statement carries a binding to target across an
// equality: every column of a table its value comes from is bound and matches target. A column
// of a recursive common table expression comes from target itself too, through the expression's
// own recursion; a row the recursion adds is bound when the row it was reached from is, so that
// source is the induction's and not one the binding needs.
func (a *m19Analysis) carries(from, target m19Ref) bool {
	if a.boundValue(from, map[m19Ref]bool{}) {
		return a.schema.binds[target.item.table][target.column]
	}
	sources := slices.DeleteFunc(a.baseRefs(from), func(source m19Ref) bool { return source.item == target.item })
	if len(sources) == 0 {
		return false
	}
	for _, source := range sources {
		if !a.holds(source) || !a.matches(source, target) {
			return false
		}
	}
	return true
}

// matches answers whether an equality between two columns holds the rows of one to the rows of
// the other (m19Schema.matches). A parameter a clause may bind matches every column that binds
// a row or a node: a statement that binds the issue its parameter names holds every row whose
// own issue key is that parameter.
func (a *m19Analysis) matches(left, right m19Ref) bool {
	for _, pair := range [][2]m19Ref{{left, right}, {right, left}} {
		if pair[0].item.parameter {
			return !pair[0].item.viewer && !pair[1].item.parameter && a.schema.binds[pair[1].item.table][pair[1].column]
		}
	}
	return a.schema.matches(left, right)
}

// holds answers whether a bound column carries its binding across an equality: a parameter or a
// subselect's column a clause binds, one half of a node's address that is bound, or any column of
// a row that is bound by what owns it or which row it is. A row bound by its owner vouches for
// its own address (the block that states a link), never for the address it points at; a
// membership tuple bound by what it is a member of vouches for its member.
func (a *m19Analysis) holds(ref m19Ref) bool {
	if ref.item.parameter || ref.item.table == "" {
		return a.boundValue(ref, map[m19Ref]bool{})
	}
	if a.schema.inAddress(ref.item.table, ref.column) && !a.schema.plain[ref.item.table][ref.column] {
		if ref.item.boundBy[ref.column] {
			return true
		}
		if a.membership(ref.item) {
			return a.isBound(ref.item)
		}
		return m19OwnAddresses[a.schema.addressOf(ref.item.table, ref.column)] && a.boundByOwner(ref.item)
	}
	return a.isBound(ref.item)
}

// boundByOwner answers whether a clause or an equality bound an item by a column that names what
// owns it or which row it is.
func (a *m19Analysis) boundByOwner(item *m19Item) bool {
	for column := range item.boundBy {
		if a.schema.plain[item.table][column] {
			return true
		}
	}
	return false
}

// m19OwnAddresses are the prefixes of the addresses that name the node a row is of, rather than a
// node the row points at: a tuple's source, a store's own node, a block's own node.
var m19OwnAddresses = map[string]bool{"": true, "from": true, "node": true, "from_node": true}

// m19MembershipRelations are the tuple kinds that make their source a member of their target, so
// the target owns the source: a plan's or a step's part of its issue (workflowstore), a block's
// part of its document and of the block holding it.
var m19MembershipRelations = map[string]string{
	graphstore.TuplePartOf: "the source is part of the target (graphstore, workflowstore.RelationPartOf)",
	refs.TupleInDocument:   "the block is in the document",
}

// membership answers whether an item is a tuple the statement holds to membership relations
// alone, whose target owns its source.
func (a *m19Analysis) membership(item *m19Item) bool {
	addresses := a.schema.addresses[item.table]
	if _, to := addresses["to"]; !to {
		return false
	}
	values := a.constants[m19Ref{item: item, column: "kind"}]
	return len(values) > 0 && !slices.ContainsFunc(values, func(value string) bool {
		_, member := m19MembershipRelations[value]
		return !member
	})
}

// isBound answers whether an item's rows are held to the projects the reader sees: a column that
// names what owns them or which row each is; for a membership tuple, its target; or, for a row
// that names nodes of the graph, each side of it that can name a node a project owns. The
// addresses that name the row's own node (m19OwnAddresses: a block link's node_address and
// from_node) are one side, bound where one of them is; every address the row points at is a side
// of its own.
func (a *m19Analysis) isBound(item *m19Item) bool {
	if a.boundByOwner(item) {
		return true
	}
	if a.membership(item) && slices.ContainsFunc(a.schema.addresses[item.table]["to"].ids, func(id string) bool { return item.boundBy[id] }) {
		return true
	}
	bound := func(address m19Address) bool {
		return slices.ContainsFunc(address.ids, func(id string) bool { return item.boundBy[id] })
	}
	live, ownLive, ownBound := 0, false, false
	for prefix, address := range a.schema.addresses[item.table] {
		if !a.live(item, address) {
			continue
		}
		live++
		if m19OwnAddresses[prefix] {
			ownLive, ownBound = true, ownBound || bound(address)
			continue
		}
		if !bound(address) {
			return false
		}
	}
	return live > 0 && (!ownLive || ownBound)
}

// excused answers whether an item reads no project's row at all: a row that names nodes of the
// graph, each address of which the statement holds to kinds no project owns (m19KindsOfNoProject)
// - a priority, a person, a value - and that names no object by an owner column of its own; or a
// row of a store of nodes (graph_nodes) whose one address, its own, is held so, since its project
// is that node's.
func (a *m19Analysis) excused(item *m19Item) bool {
	addresses := a.schema.addresses[item.table]
	if len(addresses) == 0 {
		return false
	}
	for _, address := range addresses {
		if a.live(item, address) {
			return false
		}
	}
	if _, own := addresses[""]; own && len(addresses) == 1 {
		return true
	}
	for _, owner := range a.schema.owners[item.table] {
		if owner.table != "" {
			return false
		}
	}
	return true
}

// live answers whether an address of an item can name a node a project owns: its kind column is
// held to no constant, or to one kind that is not one of m19KindsOfNoProject.
func (a *m19Analysis) live(item *m19Item, address m19Address) bool {
	values := a.constants[m19Ref{item: item, column: address.kind}]
	return len(values) == 0 || slices.ContainsFunc(values, func(value string) bool {
		_, none := m19KindsOfNoProject[value]
		return !none
	})
}

// spreadConstants carries the constants a conjunct holds a column to across every equality of
// the statement, so `n.kind = t.from_kind and t.from_kind in ('step', 'plan')` holds both.
func (a *m19Analysis) spreadConstants() {
	for ref, values := range maps.Clone(a.constants) {
		for _, base := range a.baseRefs(ref) {
			if base != ref {
				a.pin(base, values...)
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, equal := range a.equals {
			left, right := a.oneBase(equal.left), a.oneBase(equal.right)
			if left == nil || right == nil {
				continue
			}
			for _, pair := range [][2]*m19Ref{{left, right}, {right, left}} {
				for _, value := range a.constants[*pair[0]] {
					if !slices.Contains(a.constants[*pair[1]], value) {
						a.pin(*pair[1], value)
						changed = true
					}
				}
			}
		}
	}
}

// baseRefs are the columns of tables one column of the statement answers: itself, for a column of
// a table or a parameter, and every column its value comes from, for a column of a subselect or a
// common table expression.
func (a *m19Analysis) baseRefs(ref m19Ref) []m19Ref {
	return a.baseRefsSeen(ref, map[m19Ref]bool{})
}

func (a *m19Analysis) baseRefsSeen(ref m19Ref, seen map[m19Ref]bool) []m19Ref {
	if ref.item == nil || seen[ref] {
		return nil
	}
	seen[ref] = true
	if ref.item.table != "" || ref.item.parameter {
		return []m19Ref{ref}
	}
	var found []m19Ref
	for _, output := range ref.item.columns() {
		if output.name != ref.column {
			continue
		}
		for _, source := range output.sources {
			found = append(found, a.baseRefsSeen(source, seen)...)
		}
	}
	return found
}

// oneBase is the one column of a table a column of the statement answers, nil where it answers
// none or several: a column of a set operation answers a column of either arm, and holding one
// of them holds no row of the other.
func (a *m19Analysis) oneBase(ref m19Ref) *m19Ref {
	found := a.baseRefs(ref)
	if len(found) != 1 {
		return nil
	}
	return &found[0]
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

package eventidtest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	pgparser "github.com/wasilibs/go-pgquery/parser"
)

// FindingKind is a condition the event-id guards must account for in a parsed SQL tree.
type FindingKind string

const (
	SequenceCall              FindingKind = "sequence call"
	SequenceAlter             FindingKind = "sequence alter"
	DynamicSQL                FindingKind = "dynamic SQL"
	EventInsertWithoutColumns FindingKind = "event insert without columns"
	EventInsertExplicitID     FindingKind = "event insert explicit id"
	DerivedEventTable         FindingKind = "derived event table"
)

// Finding is one unsafe SQL construct. Name identifies a called function where there is one, and
// Statement is the top-level statement of the analysed text that contains it, with Location its
// byte offset in that text: a sequence function's own call where the call is in that statement's
// text, else the statement's start, so a finding inside a routine body, a constant EXECUTE or a
// ts_stat query names the statement that holds it. Safe is a sequence call that draws no event id:
// the one pg_catalog.currval read of the latest event id (store.LatestEventID), or a nextval or
// setval of one plain string literal naming another sequence.
type Finding struct {
	Kind      FindingKind
	Name      string
	Safe      bool
	Statement string
	Location  int
}

// Analysis is the parser's account of SQL relevant to the event-id guards.
type Analysis struct {
	Findings               []Finding
	EventInserts           int
	EventInsertColumnLists int
}

type parsedStatement struct {
	stmt     json.RawMessage
	location int
	length   int
}

// Analyze reads every statement and nested statement in sql with PostgreSQL's parser. Its caller
// decides which locations are the broker's own insert path; every other finding is unsafe.
func Analyze(sql string) (analysis Analysis, err error) {
	defer func() {
		if recover() != nil {
			analysis = Analysis{}
			err = fmt.Errorf("PostgreSQL parser panicked")
		}
	}()
	statements, err := parseStatements(sql)
	if err != nil {
		return Analysis{}, err
	}
	walker := sqlWalker{seen: map[Finding]bool{}}
	for _, statement := range statements {
		walker.statement, walker.location = statementText(sql, statement)
		walker.walkSQLValue(statement.stmt)
	}
	return Analysis{
		Findings:               walker.findings,
		EventInserts:           walker.eventInserts,
		EventInsertColumnLists: walker.eventInsertColumnLists,
	}, nil
}

func parseStatements(sql string) ([]parsedStatement, error) {
	tree, err := pgparser.ParseToJSON(sql)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Stmts []struct {
			Stmt     json.RawMessage `json:"stmt"`
			Location int             `json:"stmt_location"`
			Length   int             `json:"stmt_len"`
		} `json:"stmts"`
	}
	if err := json.Unmarshal([]byte(tree), &parsed); err != nil {
		return nil, fmt.Errorf("decode PostgreSQL parse tree: %w", err)
	}
	statements := make([]parsedStatement, len(parsed.Stmts))
	for index, statement := range parsed.Stmts {
		if len(statement.Stmt) == 0 {
			return nil, fmt.Errorf("PostgreSQL parse tree has an empty statement")
		}
		statements[index] = parsedStatement{stmt: statement.Stmt, location: statement.Location, length: statement.Length}
	}
	return statements, nil
}

// statementText is a parsed statement's text, trimmed, and the byte offset in sql where that
// trimmed text starts.
func statementText(sql string, statement parsedStatement) (string, int) {
	start := statement.location
	if start < 0 || start >= len(sql) {
		start = 0
	}
	end := len(sql)
	if statement.length > 0 && start+statement.length <= len(sql) {
		end = start + statement.length
	}
	text := sql[start:end]
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	return strings.TrimSpace(trimmed), start + len(text) - len(trimmed)
}

type sqlWalker struct {
	statement              string
	location               int
	findings               []Finding
	seen                   map[Finding]bool
	eventInserts           int
	eventInsertColumnLists int
}

func (w *sqlWalker) add(kind FindingKind, name string) {
	w.addFinding(Finding{Kind: kind, Name: name, Statement: w.statement, Location: w.location})
}

func (w *sqlWalker) addFinding(finding Finding) {
	if !w.seen[finding] {
		w.seen[finding] = true
		w.findings = append(w.findings, finding)
	}
}

// addAnalysis takes in the findings of SQL nested in the statement being walked (a routine body,
// a constant EXECUTE, a ts_stat query, a PL/pgSQL expression). Their statement and location are
// the nested text's, which a reader of the guard's message cannot find, and for an expression
// they carry the guard's own wrapping, so each is named by the statement that holds it.
func (w *sqlWalker) addAnalysis(analysis Analysis) {
	for _, finding := range analysis.Findings {
		finding.Statement, finding.Location = w.statement, w.location
		w.addFinding(finding)
	}
	w.eventInserts += analysis.EventInserts
	w.eventInsertColumnLists += analysis.EventInsertColumnLists
}

func (w *sqlWalker) walkSQLValue(raw json.RawMessage) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		for kind, body := range object {
			switch kind {
			case "FuncCall":
				w.readFunction(body)
			case "InsertStmt":
				w.readInsert(body)
			case "MergeStmt":
				w.readMerge(body)
			case "CopyStmt":
				w.readCopy(body)
			case "CreateStmt":
				w.readCreateTable(body)
			case "CreateForeignTableStmt":
				w.readForeignTable(body)
			case "AlterTableStmt":
				w.readAlterTable(body)
			case "AlterSeqStmt":
				w.readAlterSequence(body, "sequence")
			case "RenameStmt", "AlterObjectSchemaStmt":
				w.readAlterSequence(body, "relation")
			case "DoStmt":
				w.readRoutine(body, true)
			case "CreateFunctionStmt", "CreateProcedureStmt":
				w.readRoutine(body, false)
			}
			w.walkSQLValue(body)
		}
		return
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		for _, value := range list {
			w.walkSQLValue(value)
		}
	}
}

func (w *sqlWalker) readFunction(raw json.RawMessage) {
	fields, ok := objectFields(raw)
	if !ok {
		return
	}
	name, ok := functionName(fields["funcname"])
	if !ok {
		return
	}
	location := w.location
	if at := rawInt(fields["location"]); at > location {
		location = at
	}
	switch name {
	case "nextval", "setval":
		w.addFinding(Finding{Kind: SequenceCall, Name: name, Safe: otherSequenceLiteral(fields, name), Statement: w.statement, Location: location})
	case "currval":
		w.addFinding(Finding{Kind: SequenceCall, Name: name, Safe: safeLatestEventID(fields), Statement: w.statement, Location: location})
	case "query_to_xml", "query_to_xmlschema", "query_to_xml_and_xmlschema", "cursor_to_xml", "cursor_to_xmlschema":
		w.add(DynamicSQL, name)
	case "ts_stat":
		// ts_stat(sqlquery text [, weights text]) runs its first argument.
		w.readQueryArgument(fields, 0, name)
	case "ts_rewrite":
		// ts_rewrite(query, select) runs its second argument; ts_rewrite(query, target,
		// substitute) takes three tsqueries and runs nothing.
		if args, _ := rawList(fields["args"]); len(args) != 3 {
			w.readQueryArgument(fields, 1, name)
		}
	}
}

// readQueryArgument reads the argument at index of a function that runs it as SQL text, as a
// constant EXECUTE is read: a string constant is parsed and walked, and anything else is SQL the
// guard cannot read.
func (w *sqlWalker) readQueryArgument(fields map[string]json.RawMessage, index int, name string) {
	args, _ := rawList(fields["args"])
	if index >= len(args) {
		w.add(DynamicSQL, name)
		return
	}
	query, ok := plainStringConstant(args[index])
	if !ok {
		w.add(DynamicSQL, name)
		return
	}
	analysis, err := Analyze(query)
	if err != nil {
		w.add(DynamicSQL, "unreadable "+name)
		return
	}
	w.addAnalysis(analysis)
}

// readAlterSequence reports an ALTER SEQUENCE, a rename or a move to another schema of
// events_id_seq, whose relation is the field named field: a restart or a negative increment hands
// out ids at or below a proven horizon, as a setval would, and a renamed or moved sequence is one
// the guards no longer read by name.
func (w *sqlWalker) readAlterSequence(raw json.RawMessage, field string) {
	fields, ok := objectFields(raw)
	if ok && eventIDSequenceRelation(fields[field]) {
		w.add(SequenceAlter, "alter sequence")
	}
}

func (w *sqlWalker) readInsert(raw json.RawMessage) {
	fields, ok := objectFields(raw)
	if !ok || !eventRelation(fields["relation"]) {
		return
	}
	w.eventInserts++
	columns, exists := rawList(fields["cols"])
	if !exists || len(columns) == 0 {
		w.add(EventInsertWithoutColumns, "")
		return
	}
	w.eventInsertColumnLists++
	for _, column := range columns {
		body, ok := namedNode(column, "ResTarget")
		if !ok {
			continue
		}
		fields, ok := objectFields(body)
		if ok && strings.EqualFold(rawString(fields["name"]), "id") {
			w.add(EventInsertExplicitID, "")
		}
	}
}

func (w *sqlWalker) readMerge(raw json.RawMessage) {
	fields, ok := objectFields(raw)
	if ok && eventRelation(fields["relation"]) {
		w.eventInserts++
	}
}

func (w *sqlWalker) readCopy(raw json.RawMessage) {
	fields, ok := objectFields(raw)
	if !ok || !eventRelation(fields["relation"]) {
		return
	}
	if rawBool(fields["is_from"]) {
		w.eventInserts++
	}
}

func (w *sqlWalker) readCreateTable(raw json.RawMessage) {
	fields, ok := objectFields(raw)
	if !ok {
		return
	}
	if relationsContainEvent(fields["inhRelations"]) || tableLikeUsesEvent(fields["tableElts"]) {
		w.add(DerivedEventTable, "")
	}
}

func (w *sqlWalker) readForeignTable(raw json.RawMessage) {
	fields, ok := objectFields(raw)
	if ok {
		w.readCreateTable(fields["base"])
	}
}

func (w *sqlWalker) readAlterTable(raw json.RawMessage) {
	fields, ok := objectFields(raw)
	if !ok {
		return
	}
	// ALTER SEQUENCE ... OWNER TO, and ALTER TABLE run on a sequence, parse as an ALTER TABLE.
	if eventIDSequenceRelation(fields["relation"]) {
		w.add(SequenceAlter, "alter sequence")
	}
	commands, ok := rawList(fields["cmds"])
	if !ok {
		return
	}
	for _, command := range commands {
		body, ok := namedNode(command, "AlterTableCmd")
		if !ok {
			continue
		}
		commandFields, ok := objectFields(body)
		if !ok {
			continue
		}
		switch rawString(commandFields["subtype"]) {
		case "AT_AddInherit":
			if eventRelation(commandFields["def"]) {
				w.add(DerivedEventTable, "")
			}
		case "AT_AttachPartition":
			if eventRelation(fields["relation"]) {
				w.add(DerivedEventTable, "")
			}
		case "AT_ColumnDefault", "AT_AddIdentity", "AT_SetIdentity", "AT_DropIdentity":
			// A new default or identity for events.id draws its ids from something other
			// than events_id_seq's one counter.
			if eventRelation(fields["relation"]) && strings.EqualFold(rawString(commandFields["name"]), "id") {
				w.add(SequenceAlter, "alter events.id default")
			}
		}
	}
}

func (w *sqlWalker) readRoutine(raw json.RawMessage, isDo bool) {
	fields, ok := objectFields(raw)
	if !ok {
		return
	}
	options := fields["options"]
	if isDo {
		options = fields["args"]
	}
	language, bodies, ok := routineParts(options)
	if !ok {
		w.add(DynamicSQL, "unreadable routine")
		return
	}
	if isDo && language == "" {
		language = "plpgsql"
	}
	for _, body := range bodies {
		switch strings.ToLower(language) {
		case "plpgsql":
			if isDo {
				w.readPLpgSQLBody(body)
			} else {
				w.readPLpgSQL(w.statement)
			}
		case "sql":
			analysis, err := Analyze(body)
			if err != nil {
				w.add(DynamicSQL, "unreadable SQL routine")
				continue
			}
			w.addAnalysis(analysis)
		default:
			w.add(DynamicSQL, "unreadable "+language+" routine")
		}
	}
}

func (w *sqlWalker) readPLpgSQLBody(body string) {
	w.readPLpgSQL("create function event_id_guard() returns trigger language plpgsql as $event_id_guard$" + body + "$event_id_guard$")
}

func (w *sqlWalker) readPLpgSQL(source string) {
	tree, err := pgparser.ParsePlPgSqlToJSON(source)
	if err != nil {
		w.add(DynamicSQL, "unreadable PL/pgSQL routine")
		return
	}
	w.walkPLValue(json.RawMessage(tree))
}

func (w *sqlWalker) walkPLValue(raw json.RawMessage) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		for kind, body := range object {
			switch kind {
			case "PLpgSQL_stmt_dynexecute":
				w.readDynamicExecution(body)
				continue
			case "PLpgSQL_stmt_dynfors":
				w.readDynamicField(body, "query")
				continue
			case "PLpgSQL_stmt_open", "PLpgSQL_stmt_return_query":
				w.readDynamicField(body, "dynquery")
				continue
			case "PLpgSQL_expr":
				w.readPLExpression(body)
				continue
			}
			w.walkPLValue(body)
		}
		return
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		for _, value := range list {
			w.walkPLValue(value)
		}
	}
}

func (w *sqlWalker) readDynamicExecution(raw json.RawMessage) {
	fields, ok := objectFields(raw)
	if !ok {
		w.add(DynamicSQL, "unreadable execute")
		return
	}
	w.readDynamicExpression(fields["query"])
	for name, value := range fields {
		if name != "query" {
			w.walkPLValue(value)
		}
	}
}

// readDynamicField reads the field named name of a PL/pgSQL statement as the SQL text it runs. A
// RETURN QUERY or an OPEN of a static query carries no dynquery: its query is an ordinary
// expression, which the walk of the statement's other fields reads, so a field that is absent is
// no dynamic SQL.
func (w *sqlWalker) readDynamicField(raw json.RawMessage, name string) {
	fields, ok := objectFields(raw)
	if !ok {
		w.add(DynamicSQL, "unreadable execute")
		return
	}
	if value, present := fields[name]; present && string(value) != "null" {
		w.readDynamicExpression(value)
	}
	for field, value := range fields {
		if field != name {
			w.walkPLValue(value)
		}
	}
}

func (w *sqlWalker) readDynamicExpression(raw json.RawMessage) {
	body, ok := namedNode(raw, "PLpgSQL_expr")
	if !ok {
		w.add(DynamicSQL, "unreadable execute")
		return
	}
	fields, ok := objectFields(body)
	if !ok {
		w.add(DynamicSQL, "unreadable execute")
		return
	}
	query := rawString(fields["query"])
	constant, ok := constantSQL(query)
	if !ok {
		w.add(DynamicSQL, "execute")
		return
	}
	analysis, err := Analyze(constant)
	if err != nil {
		w.add(DynamicSQL, "unreadable execute")
		return
	}
	w.addAnalysis(analysis)
}

func (w *sqlWalker) readPLExpression(raw json.RawMessage) {
	fields, ok := objectFields(raw)
	if !ok {
		return
	}
	query := rawString(fields["query"])
	if rawInt(fields["parseMode"]) >= 3 {
		if _, right, found := strings.Cut(query, ":="); found {
			query = right
		}
	}
	analysis, err := Analyze(query)
	if err != nil {
		analysis, err = Analyze("/* event-id guard */ select " + query)
	}
	if err != nil {
		w.add(DynamicSQL, "unreadable PL/pgSQL expression")
		return
	}
	w.addAnalysis(analysis)
}

func routineParts(raw json.RawMessage) (language string, bodies []string, ok bool) {
	options, ok := rawList(raw)
	if !ok {
		return "", nil, false
	}
	for _, option := range options {
		body, ok := namedNode(option, "DefElem")
		if !ok {
			continue
		}
		fields, ok := objectFields(body)
		if !ok {
			return "", nil, false
		}
		switch strings.ToLower(rawString(fields["defname"])) {
		case "language":
			language = rawStringValue(fields["arg"])
		case "as":
			bodies = append(bodies, stringValues(fields["arg"])...)
		}
	}
	return language, bodies, len(bodies) > 0
}

func constantSQL(expression string) (string, bool) {
	tree, err := pgparser.ParseToJSON("/* event-id guard */ select " + expression)
	if err != nil {
		return "", false
	}
	var parsed struct {
		Stmts []struct {
			Stmt json.RawMessage `json:"stmt"`
		} `json:"stmts"`
	}
	if json.Unmarshal([]byte(tree), &parsed) != nil || len(parsed.Stmts) != 1 {
		return "", false
	}
	selectBody, ok := namedNode(parsed.Stmts[0].Stmt, "SelectStmt")
	if !ok {
		return "", false
	}
	selectFields, ok := objectFields(selectBody)
	if !ok {
		return "", false
	}
	targets, ok := rawList(selectFields["targetList"])
	if !ok || len(targets) != 1 {
		return "", false
	}
	target, ok := namedNode(targets[0], "ResTarget")
	if !ok {
		return "", false
	}
	targetFields, ok := objectFields(target)
	if !ok {
		return "", false
	}
	constant, ok := namedNode(targetFields["val"], "A_Const")
	if !ok {
		return "", false
	}
	constantFields, ok := objectFields(constant)
	if !ok {
		return "", false
	}
	valueFields, ok := objectFields(constantFields["sval"])
	if !ok {
		return "", false
	}
	return rawString(valueFields["sval"]), true
}

func functionName(raw json.RawMessage) (string, bool) {
	parts, ok := rawList(raw)
	if !ok || len(parts) == 0 {
		return "", false
	}
	name := ""
	for _, part := range parts {
		body, ok := namedNode(part, "String")
		if !ok {
			return "", false
		}
		fields, ok := objectFields(body)
		if !ok {
			return "", false
		}
		name = rawString(fields["sval"])
	}
	return strings.ToLower(name), name != ""
}

func safeLatestEventID(fields map[string]json.RawMessage) bool {
	if !functionIs(fields["funcname"], "pg_catalog", "currval") {
		return false
	}
	args, ok := rawList(fields["args"])
	if !ok || len(args) != 1 {
		return false
	}
	call, ok := namedNode(args[0], "FuncCall")
	if !ok {
		return false
	}
	inner, ok := objectFields(call)
	if !ok || !functionIs(inner["funcname"], "pg_catalog", "pg_get_serial_sequence") {
		return false
	}
	args, ok = rawList(inner["args"])
	return ok && len(args) == 2 && stringConstant(args[0]) == "events" && stringConstant(args[1]) == "id"
}

// functionIs reports whether a function name's parts are exactly want. The parser has already
// folded each unquoted part to lower case, so a quoted part in another case ("PG_CATALOG") names
// another schema and is not matched.
func functionIs(raw json.RawMessage, want ...string) bool {
	parts, ok := rawList(raw)
	if !ok || len(parts) != len(want) {
		return false
	}
	for index, part := range parts {
		body, ok := namedNode(part, "String")
		if !ok {
			return false
		}
		fields, ok := objectFields(body)
		if !ok || rawString(fields["sval"]) != want[index] {
			return false
		}
	}
	return true
}

// sequenceNameLiteral is a regclass literal naming one relation: an optional schema and a name,
// each a plain or a double-quoted identifier, holding nothing a sequence's name does not need. A
// literal of digits alone is an OID, and a backslash, a quote or any other character can spell
// events_id_seq under standard_conforming_strings off or name it some other way, so none of them
// is matched.
var sequenceNameLiteral = regexp.MustCompile(`^\s*(?:("[\w.$\s\x{80}-\x{10FFFF}]*"|[a-zA-Z_\x{80}-\x{10FFFF}][\w$\x{80}-\x{10FFFF}]*)\s*\.\s*)?("[\w.$\s\x{80}-\x{10FFFF}]*"|[a-zA-Z_\x{80}-\x{10FFFF}][\w$\x{80}-\x{10FFFF}]*)\s*$`)

// otherSequenceLiteral reports a call of Postgres's own nextval or setval, bare or as
// pg_catalog's, whose sequence is one plain string literal naming a sequence other than
// events_id_seq: such a call draws no event id. A sequence named any other way (a cast, an
// expression, a parameter, an OID) is not one the guard can read, and a function of that name in
// another schema can draw in its body, so neither is safe.
func otherSequenceLiteral(fields map[string]json.RawMessage, name string) bool {
	if !functionIs(fields["funcname"], name) && !functionIs(fields["funcname"], "pg_catalog", name) {
		return false
	}
	args, ok := rawList(fields["args"])
	if !ok || len(args) == 0 {
		return false
	}
	literal, ok := plainStringConstant(args[0])
	if !ok {
		return false
	}
	parts := sequenceNameLiteral.FindStringSubmatch(literal)
	if parts == nil {
		return false
	}
	sequence := parts[2]
	if strings.HasPrefix(sequence, `"`) {
		sequence = strings.Trim(sequence, `"`)
	} else {
		sequence = asciiLower(sequence)
	}
	return sequence != "events_id_seq"
}

// asciiLower folds A to Z alone, as Postgres folds an unquoted identifier in a multibyte encoding.
func asciiLower(identifier string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, identifier)
}

// plainStringConstant is a string constant with no cast around it.
func plainStringConstant(raw json.RawMessage) (string, bool) {
	body, ok := namedNode(raw, "A_Const")
	if !ok {
		return "", false
	}
	fields, ok := objectFields(body)
	if !ok {
		return "", false
	}
	value, ok := objectFields(fields["sval"])
	if !ok {
		return "", false
	}
	return rawString(value["sval"]), true
}

func stringConstant(raw json.RawMessage) string {
	value, _ := plainStringConstant(raw)
	return value
}

// eventIDSequenceRelation reports a relation named events_id_seq, in any schema and either case.
func eventIDSequenceRelation(raw json.RawMessage) bool {
	body, ok := namedNode(raw, "RangeVar")
	if !ok {
		body = raw
	}
	fields, ok := objectFields(body)
	return ok && strings.EqualFold(rawString(fields["relname"]), "events_id_seq")
}

func eventRelation(raw json.RawMessage) bool {
	body, ok := namedNode(raw, "RangeVar")
	if !ok {
		body = raw
	}
	fields, ok := objectFields(body)
	if !ok || !strings.EqualFold(rawString(fields["relname"]), "events") {
		return false
	}
	schema := rawString(fields["schemaname"])
	return schema == "" || strings.EqualFold(schema, "public")
}

func relationsContainEvent(raw json.RawMessage) bool {
	relations, ok := rawList(raw)
	if !ok {
		return false
	}
	for _, relation := range relations {
		if eventRelation(relation) {
			return true
		}
	}
	return false
}

func tableLikeUsesEvent(raw json.RawMessage) bool {
	elements, ok := rawList(raw)
	if !ok {
		return false
	}
	for _, element := range elements {
		body, ok := namedNode(element, "TableLikeClause")
		if !ok {
			continue
		}
		fields, ok := objectFields(body)
		if ok && eventRelation(fields["relation"]) {
			return true
		}
	}
	return false
}

func objectFields(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, false
	}
	return fields, true
}

func namedNode(raw json.RawMessage, name string) (json.RawMessage, bool) {
	fields, ok := objectFields(raw)
	if !ok {
		return nil, false
	}
	body, ok := fields[name]
	return body, ok
}

func rawList(raw json.RawMessage) ([]json.RawMessage, bool) {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil, false
	}
	return list, true
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func rawBool(raw json.RawMessage) bool {
	var value bool
	return json.Unmarshal(raw, &value) == nil && value
}

func rawInt(raw json.RawMessage) int {
	var value int
	_ = json.Unmarshal(raw, &value)
	return value
}

func rawStringValue(raw json.RawMessage) string {
	body, ok := namedNode(raw, "String")
	if !ok {
		return ""
	}
	fields, ok := objectFields(body)
	if !ok {
		return ""
	}
	return rawString(fields["sval"])
}

func stringValues(raw json.RawMessage) []string {
	if value := rawStringValue(raw); value != "" {
		return []string{value}
	}
	body, ok := namedNode(raw, "List")
	if !ok {
		return nil
	}
	fields, ok := objectFields(body)
	if !ok {
		return nil
	}
	items, ok := rawList(fields["items"])
	if !ok {
		return nil
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		if value := rawStringValue(item); value != "" {
			values = append(values, value)
		}
	}
	return values
}

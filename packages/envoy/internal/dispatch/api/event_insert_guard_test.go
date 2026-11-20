//go:build !race

// The guard is static, as lock_level_test.go's is and for the reason given there: it reads the
// module's source, so the race detector adds nothing to it.

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/eventidtest"
)

// brokerEventInsert is the one source file allowed to insert an event.
const brokerEventInsert = "internal/dispatch/events/broker.go"

// brokerEventInserts is how many event inserts that file holds: the one a write phase queues
// (events.Broker.QueueAppendWithLogins). It keeps the invariants below, and the count is here so
// that an insert moving out of this scan's reach reds rather than passing unseen.
const brokerEventInserts = 1

// TestOnlyTheBrokerInsertsEvents enforces what every reader of the event log by id rests on: every
// row of it is written by events.Broker.Append, which keeps two invariants no other insert would.
//
// The writer invariant: Append's transaction holds its transaction id before the insert draws the
// row's id from events_id_seq (lockEventOwner takes it with pg_current_xact_id). The safe horizon's
// proof needs it (events/horizon.go): a transaction that drew an id at or below one an observer saw
// committed holds a transaction id below the observer's own, so once no event writer of this
// database holding a transaction id below the observer's is running, that id is final. A bare
// insert, in a transaction that has written nothing before it, takes its transaction id only as it
// writes the row, after its default drew the id, so an observation taken between the two could
// prove a horizon above an id still open. The
// relay (event_stream_relay.go), the delivery hub and `GET /references?since=` would then read past
// that id, and its row, committed later, would reach no open subscription until the page subscribed
// again -- a missing comment or ask on an open page, with nothing to show for it in any log.
//
// Owner sequence allocation: Append locks the event's owner row and takes the owner's next seq under
// that lock, so one owner's events commit in id order and their seqs run without a gap or a repeat.
// An insert elsewhere could write a seq out of that order, and the Legion daemon, which fences an
// issue's facts on their seq, would drop as stale an event whose seq it had already passed.
//
// Its reach is the statement as the module scan can assemble it (TestNoForUpdateOnOwnerTables):
// string literals and chains of literals and package-level constants, in every non-test Go file
// of the module. A test inserts events directly on purpose -- a backlog to page through, a row
// shape to serve -- so test sources are outside it, as they are outside that scan.
func TestOnlyTheBrokerInsertsEvents(t *testing.T) {
	t.Run("the parser reads every statement that writes rows into events", func(t *testing.T) {
		for _, row := range []struct {
			statement string
			writes    bool
		}{
			{"insert into events (seq, type) values (0, 't')", true},
			{`insert into "public"."events" (seq) values (0)`, true},
			{"merge into events e using (select $1::bigint as id) s on false when not matched then insert (id, seq) values (s.id, 0)", true},
			{"merge into public.events e using staged s on e.id = s.id when not matched then insert (seq) values (0)", true},
			{"copy events (id, seq, type) from stdin", true},
			{`copy "events" from '/tmp/events.csv'`, true},
			{"copy events to stdout", false},
			{"copy (select id from events) to stdout", false},
			{"insert into events_archive (id) values (1)", false},
			{"select id from events where id > $1", false},
		} {
			analysis, err := eventidtest.Analyze(row.statement)
			if err != nil {
				t.Errorf("%s: parse: %v", row.statement, err)
				continue
			}
			if got := analysis.EventInserts > 0; got != row.writes {
				t.Errorf("%s: read as a write into events: %t, want %t", row.statement, got, row.writes)
			}
		}
	})

	sources := moduleSources(t)
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	var offences []string
	broker := 0
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		for _, statement := range sqlTemplatesByFunction(source.file, constants) {
			analysis, err := eventidtest.Analyze(statement.statement)
			if err != nil || analysis.EventInserts == 0 {
				continue
			}
			if strings.HasSuffix(path, brokerEventInsert) {
				broker += analysis.EventInserts
				continue
			}
			offences = append(offences, path+": "+statement.statement)
		}
	}
	if len(offences) > 0 {
		t.Fatalf("only %s may insert into events, and these statements also do:\n\t%s\n"+
			"Append the event through events.Broker.Append instead. It takes its transaction id "+
			"before the insert draws the event's id, which the safe horizon every reader of the log "+
			"by id reads through needs (events/horizon.go): a row inserted without it can commit "+
			"behind a horizon that passed its id and reach no open subscription. It also allocates the "+
			"owner's seq under the owner's row lock.", brokerEventInsert, strings.Join(offences, "\n\t"))
	}
	if broker != brokerEventInserts {
		t.Fatalf("%s holds %d event inserts the scan can see, want %d: if an insert moved or was "+
			"written around a value this scan cannot resolve, this guard now passes by seeing "+
			"nothing", brokerEventInsert, broker, brokerEventInserts)
	}
}

// seedSequenceReset is the one source file allowed to reset events_id_seq after it fills an empty
// database offline, with no Dispatch serving it (cmd/dispatch-seed).
const seedSequenceReset = "cmd/dispatch-seed/write_events.go"

// seedTool is the one place allowed to copy rows into events: it fills an empty database offline.
const seedTool = "cmd/dispatch-seed/"

// The limit of this scan, stated: it reads a SQL value from string literals, package-level
// constants, a local bound once to either, a fmt.Sprintf format and `+` chains of those, and it
// splices the literals inside a run-time fragment into what it reads. A value it reads only in
// part, or that PostgreSQL's parser refuses, is held to knownUnreadableEventIDSQL when what it
// did read holds a word a draw needs (eventIDRiskWords). A fragment that spells none of those
// words can name no sequence and run no SQL text of its own, so the rest are read as far as
// they go and no further.

// eventIDOffences reads a SQL string with the PostgreSQL parser and reports each draw or unsafe
// construct outside the broker's insert path. A string the parser refuses is one offence of its
// own, so a row of this test states a parse failure as the guard's own reading of it.
func eventIDOffences(path, statement string) (offences []string, lists int) {
	offences, lists, parsed := eventIDScan(path, statement)
	if !parsed {
		return []string{path + ": cannot parse SQL (sha256=" + sqlDigest(statement) + "): " + statement}, 0
	}
	return offences, lists
}

// eventIDScan is eventIDOffences with the parser's verdict beside its findings: parsed is false
// where PostgreSQL's parser refused the text, which the module scan accounts for separately from
// a statement it could read.
func eventIDScan(path, statement string) (offences []string, lists int, parsed bool) {
	analysis, err := eventidtest.Analyze(statement)
	if err != nil {
		return nil, 0, false
	}
	for _, finding := range analysis.Findings {
		switch finding.Kind {
		case eventidtest.SequenceCall:
			if finding.Name == "setval" && strings.Contains(path, seedSequenceReset) {
				continue
			}
			if finding.Name == "currval" && finding.Safe {
				continue
			}
			offences = append(offences, path+": calls "+finding.Name+" outside the broker's event insert path: "+statement)
		case eventidtest.DynamicSQL:
			offences = append(offences, path+": runs SQL a static guard cannot read ("+finding.Name+"): "+statement)
		case eventidtest.EventInsertWithoutColumns:
			offences = append(offences, path+": inserts into events with no column list, which fills id by position: "+statement)
		case eventidtest.EventInsertExplicitID:
			offences = append(offences, path+": inserts an explicit event id: "+statement)
		case eventidtest.DerivedEventTable:
			offences = append(offences, path+": makes a table from events, which takes its id default or holds its rows: "+statement)
		}
	}
	return offences, analysis.EventInsertColumnLists, true
}

// eventIDRiskWords are the words a statement must hold for a run-time fragment in it to reach the
// event log: the sequence functions, the sequence's own name, the readers that run SQL text, and
// the three ways a table takes events' id default. A statement the parser could not read, or one
// a fragment completes, is held to the checked-in list only when it holds one of these, since a
// fragment that spells none of them cannot name the sequence or run SQL of its own
// (literalsWithin splices each fragment's literals into the text this reads).
var eventIDRiskWords = []string{
	"nextval", "setval", "currval", "events_id_seq", "query_to_xml", "cursor_to_xml",
	"insert into events", "inherits (events)", "partition of events", "like events", "execute",
}

func eventIDRiskWord(statement string) string {
	lower := strings.ToLower(statement)
	for _, word := range eventIDRiskWords {
		if strings.Contains(lower, word) {
			return word
		}
	}
	return ""
}

// eventCopies counts the pgx.Identifier literals in file whose table is events, as a CopyFrom into
// events names it: a copy writes rows through no insert, with their ids when its columns name id,
// so neither guard's reading of the insert can hold it.
func eventCopies(file *ast.File, constants map[string]string) int {
	copies := 0
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok || len(literal.Elts) == 0 {
			return true
		}
		selector, ok := literal.Type.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Identifier" {
			return true
		}
		if qualifier, ok := selector.X.(*ast.Ident); !ok || qualifier.Name != "pgx" {
			return true
		}
		table, ok := literalText(literal.Elts[len(literal.Elts)-1], file.Name.Name, constants)
		if ok && strings.ToLower(table) == "events" {
			copies++
		}
		return true
	})
	return copies
}

func scratchSourceEventIDOffences(t *testing.T, sql string) []string {
	t.Helper()
	source, err := parser.ParseFile(token.NewFileSet(), "scratch.go", "package scratch\nfunc planted() { database.Query(ctx, `"+sql+"`) }\n", 0)
	if err != nil {
		t.Fatalf("parse scratch source: %v", err)
	}
	templates := sqlTemplatesByFunction(source, map[string]string{})
	if len(templates) != 1 {
		t.Fatalf("the scratch source held %d query templates, want one", len(templates))
	}
	offences, _ := eventIDOffences("scratch.go:planted", templates[0].statement)
	return offences
}

// sqlLooking identifies the SQL statements that can create an event-id draw. A source string is
// SQL-looking when it starts with a query form or one of the structural forms below; set, create
// and alter require their SQL-only second word so CLI help and prose do not enter the reader.
func sqlLooking(statement string) bool {
	words := strings.Fields(strings.ToLower(statement))
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "begin", "copy", "delete", "do", "explain", "insert", "merge", "select", "update", "values":
		return true
	case "with":
		return len(words) > 1 && (words[1] == "recursive" || strings.Contains(statement, "select "))
	case "create":
		return len(words) > 1 && (words[1] == "function" || words[1] == "procedure" || words[1] == "table" || words[1] == "trigger" || words[1] == "view")
	case "alter":
		return len(words) > 1 && (words[1] == "function" || words[1] == "procedure" || words[1] == "table")
	case "set":
		return len(words) > 1 && (words[1] == "local" || words[1] == "session" || words[1] == "transaction" || words[1] == "constraints" || words[1] == "role" || words[1] == "search_path" || words[1] == "standard_conforming_strings")
	case "prepare":
		return strings.Contains(statement, " as ")
	}
	return false
}

// unreadableEventIDSQL is the checked-in account of the SQL values a static reader cannot read
// whole and that hold a word a draw needs (eventIDRiskWords). Each carries its file, the
// function it stands in, the sha256 of the text the scan read — the literals of its run-time
// fragments spliced in, so an edit to a fragment moves the digest and the entry goes stale — and
// the reason a reviewer gave for it. An unlisted one of them reds this guard, and so does a
// listed one the module no longer holds.
type unreadableEventIDSQL struct {
	file, function, digest, reason string
}

var knownUnreadableEventIDSQL = []unreadableEventIDSQL{
	{"../../../internal/dispatch/events/broker.go", "QueueAppendWithLogins", "13f336feaef55cb2a2f23a01bcaa79c98e1818c3822eb700becd8b594ac9e63e", "the broker's own event insert: its two gaps are the stored payload and the thread logins, built by this function alone, and its column list is read here (events/broker.go)"},
	{"../../../internal/explaintest/explaintest.go", "GenericPlan", "56d3c30e1b875bd5afe8431e2bc9025bcd2f26acb74c4a651c0aec5372381ec9", "an EXPLAIN of a prepared statement a test prepared: SQL's EXECUTE of a PREPARE, which runs no SQL text of its own"},
}

func sqlDigest(statement string) string {
	sum := sha256.Sum256([]byte(statement))
	return hex.EncodeToString(sum[:])
}

func unreadableEventIDSQLIndex(path, function, statement string) int {
	digest := sqlDigest(statement)
	for index, known := range knownUnreadableEventIDSQL {
		if known.file == path && known.function == function && known.digest == digest {
			return index
		}
	}
	return -1
}

func unreadableSQLTemplate(path, function string, template sqlTemplate) (index int, offence string) {
	index = unreadableEventIDSQLIndex(path, function, template.statement)
	if !template.complete && index < 0 {
		offence = path + ":" + function + ": cannot statically read SQL assembled at run time (sha256=" + sqlDigest(template.statement) + "): " + template.statement
	}
	return index, offence
}

func staleUnreadableSQL(used []bool) []string {
	var stale []string
	for index, known := range knownUnreadableEventIDSQL {
		if !used[index] {
			stale = append(stale, known.file+":"+known.function+": stale unreadable SQL entry: "+known.reason)
		}
	}
	return stale
}

// TestOnlyAnEventInsertsDefaultDrawsAnEventID enforces the other half of the safe horizon's proof:
// the event insert holds RowExclusiveLock before its default draws the id. It parses every
// SQL-looking source string and every substatement, including SQL in routine bodies.
func TestOnlyAnEventInsertsDefaultDrawsAnEventID(t *testing.T) {
	t.Run("the parser reads sequence calls and nested statements", func(t *testing.T) {
		for _, row := range []struct {
			statement string
			want      int
		}{
			{"select nextval('events_id_seq')", 1},
			{`select "pg_catalog"."setval"('16402', 1)`, 1},
			{`select pg_catalog.currval('other_seq')`, 1},
			{"insert into events (id, seq) values ($1, 0)", 1},
			{"insert into events select $1, 0", 1},
			{"create table event_children () inherits (events)", 1},
			{"alter table events attach partition event_children for values from (1) to (2)", 1},
			{"select pg_catalog.currval(pg_catalog.pg_get_serial_sequence('events', 'id'))", 0},
			{`create trigger events_before before insert on events for each row execute function f()`, 0},
			{`select query_to_xml('select next' || 'val(16402)', false, false, '')`, 1},
			{`select query_to_xmlschema('select 1', false, false, '', '')`, 1},
			{`select cursor_to_xml('cursor', 1, false, false, '')`, 1},
			{`select cursor_to_xmlschema('cursor', 1, false, false, '', '')`, 1},
			{`do $$ begin execute 'select nextval(''events_id_seq'')'; end $$`, 1},
			{`do $$ declare query text := 'select 1'; begin execute query; end $$`, 1},
			{"prepare read_events as select 1; execute read_events", 0},
			{"explain (format json) execute list_open_asks('nonexistent-issue')", 0},
			{"explain (format json) execute owed_keyed_sends(60, 86400, 100)", 0},
			{"explain (costs off) execute recorded_marks('00000000-0000-4000-8000-00000000000a')", 0},
			{"explain execute session_answers('session-1', now() - interval '72 hours', 'notifications.agent.session-1')", 0},
			{"explain (analyze, format json) execute generic_listing('LIST', '{}', '', null, '{}', false, '{}', false, null, null, '{}', false, false, true)", 0},
		} {
			offences, _ := eventIDOffences("a.go", row.statement)
			if len(offences) != row.want {
				t.Errorf("%s: %d offences %q, want %d", row.statement, len(offences), offences, row.want)
			}
		}
	})

	t.Run("the scratch Go source refuses every wrapped dynamic statement", testEventIDGuardReadsWrappedDynamicSQL)
	t.Run("a sink's own argument is read", testEventIDGuardReadsASinkArgument)
	t.Run("an unlisted Sprintf query fails closed", testEventIDGuardRefusesAnUnlistedFormattedQuery)
	t.Run("a stale unreadable SQL entry fails closed", testEventIDGuardRefusesAStaleUnreadableSQLEntry)
	sources := moduleSources(t)
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	var offences []string
	seen := map[string]bool{}
	offend := func(offence string) {
		if !seen[offence] {
			seen[offence] = true
			offences = append(offences, offence)
		}
	}
	usedUnreadable := make([]bool, len(knownUnreadableEventIDSQL))
	files := make([]*ast.File, 0, len(sources))
	for _, source := range sources {
		files = append(files, source.file)
	}
	forwarded := sqlSinkParameters(files)
	lists, seedCopies := 0, 0
	// Every non-test Go file of the module, as TestOnlyTheBrokerInsertsEvents reads them: a
	// statement of any package can reach Dispatch's own database.
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		// eventidtest is this guard's own reader: the SQL in it is the text it wraps a routine
		// body in before it parses it, and it opens no database.
		if strings.Contains(path, "/internal/dispatch/eventidtest/") {
			continue
		}
		for _, located := range sqlTemplatesReaching(source.file, constants, forwarded) {
			function := located.function
			if function == "" {
				function = "<package>"
			}
			if !sqlLooking(located.statement) {
				continue
			}
			found, held, parsed := eventIDScan(path+":"+function, located.statement)
			lists += held
			if !parsed || !located.complete {
				read := strings.TrimSpace(located.statement + " " + located.fragments)
				if index := unreadableEventIDSQLIndex(path, function, read); index >= 0 {
					usedUnreadable[index] = true
				} else if word := eventIDRiskWord(read); word != "" {
					reason := "the parser cannot read it"
					if parsed {
						reason = "a run-time fragment completes it"
					}
					offend(path + ":" + function + ": " + reason + " and it holds " + word +
						" (sha256=" + sqlDigest(read) + "): " + located.statement)
				}
			}
			for _, offence := range found {
				offend(offence)
			}
		}
		if copies := eventCopies(source.file, constants); copies > 0 {
			if strings.Contains(path, seedTool) {
				seedCopies += copies
			} else {
				offend(path + ": copies rows into events (a pgx.Identifier naming events), through no insert")
			}
		}
	}
	for _, stale := range staleUnreadableSQL(usedUnreadable) {
		offend(stale)
	}
	if len(offences) > 0 {
		t.Fatalf("an event id is drawn outside the column default of the insert that writes its row:\n\t%s\n"+
			"Let the insert's default draw every event id (events.Broker.Append): the safe horizon dates a "+
			"writer by the RowExclusiveLock its insert takes before the default draws (events/horizon.go), "+
			"and an id drawn any other way is held where the lock table cannot show it.", strings.Join(offences, "\n\t"))
	}
	if lists == 0 {
		t.Fatal("the scan saw no column list of an insert into events: if the broker's insert moved or " +
			"was written around a value this scan cannot resolve, this guard now passes by seeing nothing")
	}
	if seedCopies == 0 {
		t.Fatalf("the walk saw no pgx.Identifier naming events, not even %s's offline copies: if those "+
			"moved or name the table another way, the copy check now passes by seeing nothing", seedTool)
	}
}

// testEventIDGuardReadsASinkArgument plants a draw in the two places a sink takes its SQL that no
// other reader of this guard was built for: pgx.Batch.Queue's first argument, and the argument a
// caller hands to a helper that passes it on to a sink. The harmless row beside them is the
// control: the same readers over a query that draws nothing must raise nothing, so a row that
// refuses everything cannot pass for a row that reads what it refuses.
func testEventIDGuardReadsASinkArgument(t *testing.T) {
	const scratch = `package scratch

func planted(batch *pgx.Batch) { batch.Queue("select nextval('events_id_seq')") }

func forward(database *pgxpool.Pool, query string) { database.Exec(ctx, query) }

func caller(database *pgxpool.Pool) { forward(database, "select setval('events_id_seq', 1)") }

func harmless(batch *pgx.Batch) { batch.Queue("select 1") }
`
	source, err := parser.ParseFile(token.NewFileSet(), "scratch.go", scratch, 0)
	if err != nil {
		t.Fatal(err)
	}
	forwarded := sqlSinkParameters([]*ast.File{source})
	if !slices.Equal(forwarded["forward"], []int{1}) {
		t.Fatalf("the scan read forward's SQL parameter as %v, want [1]", forwarded["forward"])
	}
	refused := map[string]bool{}
	for _, template := range sqlTemplatesReaching(source, map[string]string{}, forwarded) {
		if offences, _ := eventIDOffences("scratch.go:"+template.function, template.statement); len(offences) > 0 {
			refused[template.statement] = true
		}
	}
	for _, statement := range []string{"select nextval('events_id_seq')", "select setval('events_id_seq', 1)"} {
		if !refused[statement] {
			t.Errorf("the guard did not refuse %q at its sink", statement)
		}
	}
	if refused["select 1"] {
		t.Error("the guard refused a query that draws nothing, so these rows show nothing")
	}
}

// testEventIDGuardReadsWrappedDynamicSQL plants each review spelling in a Go source string. The
// migration guard has the same cases in its scratch-migration table.
func testEventIDGuardReadsWrappedDynamicSQL(t *testing.T) {
	for _, sql := range []string{
		`explain analyze select query_to_xml('select next' || 'val(16402)', false, false, '')`,
		`set local statement_timeout = '1s'; select query_to_xml('select next' || 'val(16402)', false, false, '')`,
		`values (query_to_xml('select next' || 'val(16402)', false, false, ''))`,
		`copy (select query_to_xml('select next' || 'val(16402)', false, false, '')) to stdout`,
		`begin; do $$ declare query text := 'select 1'; begin execute query; end $$; commit`,
		`do $$ begin execute 'select nextval(' || 16402 || ')'; end $$`,
		`set local standard_conforming_strings = off; select nextval('events\137id\137seq')`,
		`select query_to_xml('select next' || 'val(16402)', false, false, '')`,
	} {
		if offences := scratchSourceEventIDOffences(t, sql); len(offences) == 0 {
			t.Errorf("the guard did not refuse dynamic SQL wrapped in %q", sql)
		}
	}

	source, err := parser.ParseFile(token.NewFileSet(), "scratch.go", "package scratch\nfunc planted() { query := `select query_to_xml('select next' || 'val(16402)', false, false, '')`; database.Query(ctx, query) }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	templates := sqlTemplatesByFunction(source, map[string]string{})
	refused := false
	for _, template := range templates {
		if offences, _ := eventIDOffences("scratch.go:planted", template.statement); len(offences) > 0 {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("the guard did not read a SQL string held in a local query variable: %#v", templates)
	}
}

// testEventIDGuardRefusesAnUnlistedFormattedQuery plants a query a fmt.Sprintf builds around a
// draw. The scan reads its format and the literals of its arguments, the parser cannot read the
// whole, and nothing lists it, so the guard refuses it.
func testEventIDGuardRefusesAnUnlistedFormattedQuery(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "scratch.go",
		"package scratch\nfunc planted() { database.Query(ctx, fmt.Sprintf(`select %s from t`, \"nextval('events_id_seq')\")) }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	templates := sqlTemplatesByFunction(source, map[string]string{})
	if len(templates) != 1 {
		t.Fatalf("the scratch source held %d query templates, want one", len(templates))
	}
	if templates[0].complete || !sqlLooking(templates[0].statement) {
		t.Fatalf("the Sprintf query did not read as an incomplete SQL value: %#v", templates[0])
	}
	read := strings.TrimSpace(templates[0].statement + " " + templates[0].fragments)
	if word := eventIDRiskWord(read); word == "" {
		t.Fatalf("the Sprintf query read as holding no word a draw needs: %q", read)
	}
	if index := unreadableEventIDSQLIndex("scratch.go", "planted", read); index >= 0 {
		t.Fatal("the planted Sprintf query is on the checked-in list, so this control proves nothing")
	}
}

func testEventIDGuardRefusesAStaleUnreadableSQLEntry(t *testing.T) {
	stale := staleUnreadableSQL(make([]bool, len(knownUnreadableEventIDSQL)))
	if len(stale) != len(knownUnreadableEventIDSQL) {
		t.Fatalf("removing every listed source left %d stale entries, want %d", len(stale), len(knownUnreadableEventIDSQL))
	}
}

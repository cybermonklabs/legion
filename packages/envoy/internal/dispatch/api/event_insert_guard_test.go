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
	"regexp"
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
// Its reach is every string value the module writes as the scan can assemble it
// (sqlTemplatesByFunction): string literals, chains of literals and package-level constants, a
// local bound once, and a fmt.Sprintf format, in every non-test Go file of the module. A value
// PostgreSQL's parser refuses - a Sprintf format, whose verbs are no SQL - is still read as text
// (eventWriteText), so a write cannot pass by failing to parse. A test inserts events directly on
// purpose -- a backlog to page through, a row shape to serve -- so test sources are outside it, as
// they are outside that scan.
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
	t.Run("a write the parser refuses is read as text", testBrokerGuardReadsAFormattedWrite)

	sources := moduleSources(t)
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	offences, broker := eventWritesOutsideTheBroker(sources, constants)
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

// eventsTable names events in each spelling Postgres resolves to it: bare, schema-qualified, and
// with either part quoted.
const eventsTable = `(?:"?public"?\s*\.\s*)?"?events\b"?`

// eventWriteText is a write of rows into events read as text: an insert or a merge into it, or a
// copy into it from a client or a file, with any whitespace between the words.
var eventWriteText = regexp.MustCompile(`(?i)\b(?:insert|merge)\s+into\s+` + eventsTable + `|\bcopy\s+` + eventsTable + `(?:\s*\([^)]*\))?\s+from\b`)

// eventWritesOutsideTheBroker is TestOnlyTheBrokerInsertsEvents' reading of sources: each value
// that writes rows into events outside brokerEventInsert, and how many such writes that file
// holds. A value the parser reads is judged by its tree; one it refuses, by eventWriteText.
func eventWritesOutsideTheBroker(sources []moduleSource, constants map[string]string) (offences []string, broker int) {
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		for _, statement := range sqlTemplatesByFunction(source.file, constants) {
			writes := 0
			if analysis, err := eventidtest.Analyze(statement.statement); err == nil {
				writes = analysis.EventInserts
			} else if eventWriteText.MatchString(statement.statement + " " + statement.fragments) {
				writes = 1
			}
			if writes == 0 {
				continue
			}
			if strings.HasSuffix(path, brokerEventInsert) {
				broker += writes
				continue
			}
			offences = append(offences, path+": "+statement.statement)
		}
	}
	return offences, broker
}

// testBrokerGuardReadsAFormattedWrite plants the three writes a fmt.Sprintf builds that the
// parser refuses for their verbs, and two that write no rows into events, which are the control:
// a reader that refused every refused parse would refuse those too.
func testBrokerGuardReadsAFormattedWrite(t *testing.T) {
	const scratch = "package scratch\n" +
		"func merged() { database.Exec(ctx, fmt.Sprintf(\"merge into events e using (%s) s on false when not matched then insert (id, seq) values (s.id, 0)\", source)) }\n" +
		"func qualified() { database.Exec(ctx, fmt.Sprintf(\"insert into public.events (%s) values (%s)\", columns, values)) }\n" +
		"func spaced() { database.Exec(ctx, fmt.Sprintf(\"insert into\\n\\tevents (%s) values (%s)\", columns, values)) }\n" +
		"func archived() { database.Exec(ctx, fmt.Sprintf(\"insert into events_archive (%s) values (%s)\", columns, values)) }\n" +
		"func read() { database.Query(ctx, fmt.Sprintf(\"select %s from events\", columns)) }\n"
	sources := scratchSources(t, scratch)
	offences, _ := eventWritesOutsideTheBroker(sources, map[string]string{})
	if len(offences) != 3 || !slices.ContainsFunc(offences, func(o string) bool { return strings.Contains(o, "merge into events") }) ||
		!slices.ContainsFunc(offences, func(o string) bool { return strings.Contains(o, "public.events") }) ||
		!slices.ContainsFunc(offences, func(o string) bool { return strings.Contains(o, "into\n\tevents") }) {
		t.Fatalf("offences = %q, want the merge, the qualified insert and the spaced insert, and neither control", offences)
	}
}

// scratchSources is one planted Go file, parsed as the module walk parses each of its files.
func scratchSources(t *testing.T, source string) []moduleSource {
	t.Helper()
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, "scratch/planted.go", source, 0)
	if err != nil {
		t.Fatalf("parse the planted source: %v", err)
	}
	return []moduleSource{{path: "scratch/planted.go", file: file, positions: positions}}
}

// seedSequenceReset is the one source file allowed to reset events_id_seq after it fills an empty
// database offline, with no Dispatch serving it (cmd/dispatch-seed).
const seedSequenceReset = "cmd/dispatch-seed/write_events.go"

// seedTool is the one place allowed to copy rows into events: it fills an empty database offline.
const seedTool = "cmd/dispatch-seed/"

// The limit of this scan, stated. It reads every string value of the module's non-test Go files
// (sqlTemplatesByFunction): literals, package-level constants, a local bound once to either, a
// fmt.Sprintf format and `+` chains of those, with each run-time fragment standing as `null` and
// the literals inside it spliced into what it reads. Every value is parsed, whatever its first
// word, and every finding of one that parses counts. A value the parser refuses, or one a run-time
// fragment completes, is refused when what the scan read of it holds a word a draw needs
// (eventIDRiskText), unless knownUnreadableEventIDSQL lists it; and each literal written inside
// such a value is read on its own as well, parsed and held to the same words, so a statement
// written whole in one literal is read whatever run-time value stands beside it. A literal no
// chain holds is a value of its own, so a draw spelled whole in any literal is seen wherever that
// literal goes: into a builder, through another function's return, into a struct field, to a
// helper that hands it to a sink. The word execute counts only in a value one of whose parts, at
// its start or after a semicolon, begins a statement past comments, parentheses, the `null`
// stand-in and a Sprintf verb (sqlLooking), since prose says it often.
//
// A sink is not itself checked. A check by sink was measured at this guard's head: the SQL
// argument of every Exec, Query, QueryRow, Prepare and Queue call that holds no string value the
// scan reads (sqlTemplateText) is 176 calls in the module. 3 are syscall.Exec, and 36 are Queue
// calls handed a closure or a write batch rather than SQL; the other 137 are database calls whose
// SQL is built at run time, such as inbox_read.go's tx.Query(query). Each of those is built from
// literals and constants this scan reads where they are written, and a check by sink would need
// about 137 listed entries, or type information, to tell its sinks apart.
//
// What stays outside: a function's or the sequence's name, or a statement's first word, split
// across literals that only run time joins (a `+` with a call between its halves, two writes to
// one builder), with no half a risk word; and SQL text a run-time value supplies whole. Review
// holds that line.

// eventIDOffences reads a SQL string with the PostgreSQL parser and reports each draw or unsafe
// construct outside the broker's insert path. A string the parser refuses is one offence of its
// own, so a row of this test states a parse failure as the guard's own reading of it.
func eventIDOffences(path, statement string) (offences []string, lists int) {
	findings, lists, parsed := eventIDScan(path, statement)
	if !parsed {
		return []string{path + ": cannot parse SQL (sha256=" + sqlDigest(statement) + "): " + statement}, 0
	}
	for _, finding := range findings {
		offences = append(offences, finding.offence)
	}
	return offences, lists
}

// eventIDFinding is one offence of a parsed statement. dynamic is SQL the statement runs as text
// (eventidtest.DynamicSQL), which a reviewed entry of knownUnreadableEventIDSQL can account for:
// such an entry says what that text is, where the module writes it, and that this scan reads it
// there.
type eventIDFinding struct {
	offence string
	dynamic bool
}

// eventIDScan is eventIDOffences with the parser's verdict beside its findings: parsed is false
// where PostgreSQL's parser refused the text, which the module scan accounts for separately from
// a statement it could read.
func eventIDScan(path, statement string) (findings []eventIDFinding, lists int, parsed bool) {
	analysis, err := eventidtest.Analyze(statement)
	if err != nil {
		return nil, 0, false
	}
	for _, finding := range analysis.Findings {
		switch finding.Kind {
		case eventidtest.SequenceCall:
			if finding.Safe || (finding.Name == "setval" && strings.Contains(path, seedSequenceReset)) {
				continue
			}
			findings = append(findings, eventIDFinding{offence: path + ": calls " + finding.Name + " outside the broker's event insert path: " + statement})
		case eventidtest.SequenceAlter:
			findings = append(findings, eventIDFinding{offence: path + ": " + finding.Name + " of the events id sequence or default: " + statement})
		case eventidtest.DynamicSQL:
			findings = append(findings, eventIDFinding{offence: path + ": runs SQL a static guard cannot read (" + finding.Name + "): " + statement, dynamic: true})
		case eventidtest.EventInsertWithoutColumns:
			findings = append(findings, eventIDFinding{offence: path + ": inserts into events with no column list, which fills id by position: " + statement})
		case eventidtest.EventInsertExplicitID:
			findings = append(findings, eventIDFinding{offence: path + ": inserts an explicit event id: " + statement})
		case eventidtest.DerivedEventTable:
			findings = append(findings, eventIDFinding{offence: path + ": makes a table from events, which takes its id default or holds its rows: " + statement})
		}
	}
	return findings, analysis.EventInsertColumnLists, true
}

// eventIDRiskText is what a SQL value must hold for a part the parser could not read to draw an
// event id, run SQL text of its own, or make or change what draws one: the sequence functions,
// lastval and their C names, pg_get_serial_sequence, the sequence's own name, the readers that run
// SQL text, a Unicode escape (which can spell any name), an alter of a sequence or of events.id's
// column, and a write into events or a table made from it in each spelling of the table
// (eventsTable). No word boundary follows the sequence functions' names, so nextval_oid and
// setval3_oid are read too.
var eventIDRiskText = regexp.MustCompile(`(?i)\b(?:nextval|setval|currval|lastval)|\b(?:pg_get_serial_sequence|query_to_xml(?:schema|_and_xmlschema)?|cursor_to_xml(?:schema)?|ts_stat|ts_rewrite)\b|events_id_seq|\bu&['"]|\balter\s+sequence\b|\b(?:insert|merge)\s+into\s+` + eventsTable +
	`|\bcopy\s+` + eventsTable + `|\binherits?\s*\(?\s*` + eventsTable + `|\bpartition\s+of\s+` + eventsTable + `|\blike\s+` + eventsTable + `|\battach\s+partition\b` +
	`|\balter\s+table\s+(?:if\s+exists\s+)?(?:only\s+)?` + eventsTable + `(?s:.*?)\balter\s+(?:column\s+)?"?id"?(?:\s|$)`)

// eventIDExecute is the one risk word read only in a value that looks like SQL (sqlLooking):
// prose and log messages say "execute" often, and only a statement runs an EXECUTE.
var eventIDExecute = regexp.MustCompile(`(?i)\bexecute\b`)

// eventIDRiskWord is the first risk word read holds, lowercased, or "" when it holds none.
func eventIDRiskWord(read string, looksLikeSQL bool) string {
	if word := eventIDRiskText.FindString(read); word != "" {
		return strings.ToLower(word)
	}
	if looksLikeSQL && eventIDExecute.MatchString(read) {
		return "execute"
	}
	return ""
}

// sqlLeader is what may come before a statement's first word: whitespace, comments, opening
// parentheses, the scan's own `null` stand-in for a run-time fragment (sqlTemplateText), and a
// fmt.Sprintf verb, which a run-time value fills.
var sqlLeader = regexp.MustCompile(`(?i)^(?:\s+|/\*(?s:.*?)\*/|--[^\n]*(?:\n|$)|\(|null\b|%[-+# 0-9.*\[\]]*[a-z])*`)

// sqlLooking reports a value of which some part begins as a statement does, at its start or after
// a semicolon, past any leader (sqlLeader): a query form or a structural form, and set, create,
// alter and start with their SQL-only second word so CLI help and prose do not count. It decides
// one thing only: whether the word execute in a value the parser could not read counts as a risk
// word (eventIDExecute).
func sqlLooking(statement string) bool {
	for _, part := range strings.Split(statement, ";") {
		if statementStart(part) {
			return true
		}
	}
	return false
}

func statementStart(part string) bool {
	words := strings.Fields(strings.ToLower(sqlLeader.ReplaceAllString(part, "")))
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "begin", "call", "copy", "declare", "delete", "do", "explain", "insert", "merge", "select", "update", "values", "with":
		return true
	case "create":
		return len(words) > 1 && slices.Contains([]string{"domain", "function", "materialized", "or", "procedure", "rule", "table", "temp", "temporary", "trigger", "unlogged", "view"}, words[1])
	case "alter":
		return len(words) > 1 && slices.Contains([]string{"domain", "function", "procedure", "sequence", "table", "view"}, words[1])
	case "set":
		return len(words) > 1 && slices.Contains([]string{"constraints", "local", "role", "search_path", "session", "standard_conforming_strings", "statement_timeout", "transaction"}, words[1])
	case "start":
		return len(words) > 1 && words[1] == "transaction"
	case "prepare":
		return strings.Contains(strings.ToLower(part), " as ")
	}
	return false
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

// unreadableEventIDSQL is the checked-in account of the SQL values a static reader cannot read
// whole and that hold a word a draw needs (eventIDRiskText), and of the statements that run SQL
// text (eventidtest.DynamicSQL) whose text a reviewer read where the module writes it. Each carries
// its file, the function it stands in, the sha256 of the text the scan read — the literals of its
// run-time fragments spliced in, so an edit to a fragment moves the digest and the entry goes
// stale — and the reason a reviewer gave for it. An unlisted one of them reds this guard, and so
// does a listed one the module no longer holds.
type unreadableEventIDSQL struct {
	file, function, digest, reason string
}

var knownUnreadableEventIDSQL = []unreadableEventIDSQL{
	{"../../../internal/dispatch/events/broker.go", "QueueAppendWithLogins", "13f336feaef55cb2a2f23a01bcaa79c98e1818c3822eb700becd8b594ac9e63e", "the broker's own event insert: its two gaps are the stored payload and the thread logins, built by this function alone, and its column list is read here (events/broker.go)"},
	{"../../../internal/explaintest/explaintest.go", "explainGenericPlan", "79bdb3ade5ce7d477af00703d62f046050c3aad983d65d29470c4f72453fdab1", "an EXPLAIN of a prepared statement a test prepared: SQL's EXECUTE of a PREPARE, which runs no SQL text of its own; the gaps are the EXPLAIN options, the statement's name and its literal arguments"},
	{"../../../internal/dispatch/api/related_terms.go", "searchTermStatsRebuild", "136b740e098712b02fe5d27277d33e4c4e827d9245a0f90dfd5a33497c28806c", "ts_stat runs $2, searchTermItemsTSStat: searchTermItemsSQL, a constant this scan reads where it is declared, with %1$L for the project key, which reads the project's issues, documents, comments, asks and messages; the gap is the same constant with $1::text for {project}"},
	{"../../../internal/dispatch/api/related_terms.go", "refreshSearchTermStats", "f2bdb879b8c2914e7723868aa6c2eff9518ebf8a7f1da19b73cec5bbb3c94569", "searchTermStatsRebuild as refreshSearchTermStats runs it, with searchTermItemsTSStat as $2: the entry above"},
	{"../../../cmd/dispatch-seed/term_statistics.go", "searchTermStatsRebuildBeforeSnapshots", "8ac76684b0f74b170155435f89192365a7baca6ec3b93d0484cdcd5a14cb0893", "the release before's searchTermStatsRebuild, run by the offline seed tool: ts_stat runs $2, searchTermItemsBeforeSnapshotsTSStat, built from searchTermItemsBeforeSnapshotsSQL, a constant this scan reads; the gap is that constant with $1::text for {project}"},
	{"../../../cmd/dispatch-seed/term_statistics.go", "refreshSearchTermStatsBeforeSnapshots", "a72b656be4b71ef5e43f69c0421ba3f85db44ad964b823ad822751dfac02401f", "searchTermStatsRebuildBeforeSnapshots as the seed tool runs it: the entry above"},
	{"../../../internal/pgmigrate/censuscheck.go", "censusForbiddenFunctions", "767eeeaca18f4ba7fab9ca7f932fa532f0160359cfb2a18d64688aefa525f6ec", "a word of the list pgmigrate's census refuses (query_to_xml), matched against a census's text and never sent to a database"},
	{"../../../internal/pgmigrate/censuscheck.go", "censusForbiddenFunctions", "349c5bdf969dbab1e973c4ee96bfd91c5ac0eb6f5f03b13ef76902378cb5a10b", "a word of the list pgmigrate's census refuses (ts_stat), matched against a census's text and never sent to a database"},
	{"../../../internal/pgmigrate/censuscheck.go", "censusForbiddenFunctions", "34b02e5a11ee12b0dbdf541a615c1026ed2e2ff6306e5dc749b305a115b8e144", "a word of the list pgmigrate's census refuses (ts_rewrite), matched against a census's text and never sent to a database"},
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

// staleEventIDEntries names each listed entry the scan did not use: the value it stood for is gone
// or changed, so the entry accounts for nothing and goes.
func staleEventIDEntries(scan eventIDSourceScan) []string {
	var stale []string
	for index, known := range knownUnreadableEventIDSQL {
		if !scan.usedUnreadable[index] {
			stale = append(stale, known.file+":"+known.function+": stale unreadable SQL entry: "+known.reason)
		}
	}
	return stale
}

// eventIDSourceScan is the id guard's reading of a set of Go sources: what it refuses, the column
// lists of inserts into events it read, the seed tool's copies into events it saw, and which
// checked-in entries it used.
type eventIDSourceScan struct {
	offences       []string
	lists          int
	seedCopies     int
	usedUnreadable []bool
}

// scanSourcesForEventIDDraws runs the id guard over sources. The guard runs it over the module,
// and its rows run it over planted files, so a row passes only where the module scan itself
// refuses.
func scanSourcesForEventIDDraws(sources []moduleSource, constants map[string]string) eventIDSourceScan {
	scan := eventIDSourceScan{usedUnreadable: make([]bool, len(knownUnreadableEventIDSQL))}
	seen := map[string]bool{}
	offend := func(offence string) {
		if !seen[offence] {
			seen[offence] = true
			scan.offences = append(scan.offences, offence)
		}
	}
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		// eventidtest is this guard's own reader: the SQL in it is the text it wraps a routine
		// body in before it parses it, and it opens no database.
		if strings.Contains(path, "/internal/dispatch/eventidtest/") {
			continue
		}
		for _, located := range sqlTemplatesByFunction(source.file, constants) {
			at := path + ":" + located.function
			read := strings.TrimSpace(located.statement + " " + located.fragments)
			listed := unreadableEventIDSQLIndex(path, located.function, read)
			if listed >= 0 {
				scan.usedUnreadable[listed] = true
			}
			findings, held, parsed := eventIDScan(at, located.statement)
			scan.lists += held
			for _, finding := range findings {
				if !finding.dynamic || listed < 0 {
					offend(finding.offence)
				}
			}
			if (parsed && located.complete) || listed >= 0 {
				continue
			}
			if word := eventIDRiskWord(read, sqlLooking(located.statement)); word != "" {
				reason := "the parser cannot read it"
				if parsed {
					reason = "a run-time fragment completes it"
				}
				offend(at + ": " + reason + " and it holds " + word + " (sha256=" + sqlDigest(read) + "): " + located.statement)
			}
			// A value the scan cannot read whole is read literal by literal too: each literal of
			// it is parsed on its own and held to the risk text, so a statement written whole in
			// one literal is read wherever a run-time value stands beside it.
			for _, literal := range located.literals {
				if literal == located.statement {
					continue
				}
				literalFindings, _, literalParsed := eventIDScan(at, literal)
				for _, finding := range literalFindings {
					offend(finding.offence)
				}
				if literalParsed {
					continue
				}
				if word := eventIDRiskWord(literal, sqlLooking(literal)); word != "" {
					offend(at + ": a literal of a value it cannot read whole holds " + word + ": " + literal)
				}
			}
		}
		if copies := eventCopies(source.file, constants); copies > 0 {
			if strings.Contains(path, seedTool) {
				scan.seedCopies += copies
			} else {
				offend(path + ": copies rows into events (a pgx.Identifier naming events), through no insert")
			}
		}
	}
	return scan
}

// TestOnlyAnEventInsertsDefaultDrawsAnEventID enforces the other half of the safe horizon's proof:
// the event insert holds RowExclusiveLock before its default draws the id. It parses every string
// value of the module and every statement nested in one, SQL in routine bodies included.
func TestOnlyAnEventInsertsDefaultDrawsAnEventID(t *testing.T) {
	t.Run("the parser reads sequence calls and nested statements", func(t *testing.T) {
		for _, row := range []struct {
			statement string
			want      int
		}{
			{"select nextval('events_id_seq')", 1},
			{`select "pg_catalog"."setval"('16402', 1)`, 1},
			{`select pg_catalog.currval('other_seq')`, 1},
			{"select nextval('other_seq')", 0},
			{"select setval('other_seq', 1, false)", 0},
			{"insert into events (id, seq) values ($1, 0)", 1},
			{"insert into events select $1, 0", 1},
			{"create table event_children () inherits (events)", 1},
			{"alter table events attach partition event_children for values from (1) to (2)", 1},
			{"alter sequence events_id_seq restart with 1", 1},
			{"select pg_catalog.currval(pg_catalog.pg_get_serial_sequence('events', 'id'))", 0},
			{"select id from events where id = (select pg_catalog.currval(pg_catalog.pg_get_serial_sequence('events', 'id')))", 0},
			{`create trigger events_before before insert on events for each row execute function f()`, 0},
			{`select query_to_xml('select next' || 'val(16402)', false, false, '')`, 1},
			{`select query_to_xmlschema('select 1', false, false, '', '')`, 1},
			{`select cursor_to_xml('cursor', 1, false, false, '')`, 1},
			{`select cursor_to_xmlschema('cursor', 1, false, false, '', '')`, 1},
			{`select ts_stat('select nextval(''events_id_seq'')::text::tsvector')`, 1},
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
	t.Run("the module scan refuses every planted draw and passes every control", testEventIDGuardReadsPlantedSources)
	t.Run("a listed entry the module no longer holds fails closed", func(t *testing.T) {
		empty := eventIDSourceScan{usedUnreadable: make([]bool, len(knownUnreadableEventIDSQL))}
		if stale := staleEventIDEntries(empty); len(stale) != len(knownUnreadableEventIDSQL) {
			t.Fatalf("a scan that used no entry left %d stale, want %d", len(stale), len(knownUnreadableEventIDSQL))
		}
	})

	sources := moduleSources(t)
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	// Every non-test Go file of the module, as TestOnlyTheBrokerInsertsEvents reads them: a
	// statement of any package can reach Dispatch's own database.
	scan := scanSourcesForEventIDDraws(sources, constants)
	offences := append(scan.offences, staleEventIDEntries(scan)...)
	if len(offences) > 0 {
		t.Fatalf("an event id is drawn outside the column default of the insert that writes its row:\n\t%s\n"+
			"Let the insert's default draw every event id (events.Broker.Append): the safe horizon dates a "+
			"writer by the RowExclusiveLock its insert takes before the default draws (events/horizon.go), "+
			"and an id drawn any other way is held where the lock table cannot show it.", strings.Join(offences, "\n\t"))
	}
	if scan.lists == 0 {
		t.Fatal("the scan saw no column list of an insert into events: if the broker's insert moved or " +
			"was written around a value this scan cannot resolve, this guard now passes by seeing nothing")
	}
	if scan.seedCopies == 0 {
		t.Fatalf("the walk saw no pgx.Identifier naming events, not even %s's offline copies: if those "+
			"moved or name the table another way, the copy check now passes by seeing nothing", seedTool)
	}
}

// scanPlanted runs the module scan over one planted file.
func scanPlanted(t *testing.T, source string) eventIDSourceScan {
	t.Helper()
	sources := scratchSources(t, source)
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	return scanSourcesForEventIDDraws(sources, constants)
}

// testEventIDGuardReadsPlantedSources plants each draw a review found, each in a file of its own,
// and runs the module scan over it: every one must be refused. The controls beside them are SQL
// the module writes today, or prose that names a risk word, and must pass, so a scan that refused
// everything could not pass for one that reads what it refuses.
func testEventIDGuardReadsPlantedSources(t *testing.T) {
	query := func(sql string) string {
		return "package scratch\nfunc planted() { database.Query(ctx, `" + sql + "`) }\n"
	}
	for _, row := range []struct {
		name, source string
		refused      bool
	}{
		{"explain analyze", query(`explain analyze select query_to_xml('select next' || 'val(16402)', false, false, '')`), true},
		{"set then select", query(`set local statement_timeout = '1s'; select query_to_xml('select next' || 'val(16402)', false, false, '')`), true},
		{"values", query(`values (query_to_xml('select next' || 'val(16402)', false, false, ''))`), true},
		{"copy of a query", query(`copy (select query_to_xml('select next' || 'val(16402)', false, false, '')) to stdout`), true},
		{"a transaction block around a do", query(`begin; do $$ declare query text := 'select 1'; begin execute query; end $$; commit`), true},
		{"a built execute", query(`do $$ begin execute 'select nextval(' || 16402 || ')'; end $$`), true},
		{"standard strings off", query(`set local standard_conforming_strings = off; select nextval('events\137id\137seq')`), true},
		{"an XML query built from parts", query(`select query_to_xml('select next' || 'val(16402)', false, false, '')`), true},
		{"a leading block comment", query(`/* note */ select nextval('events_id_seq')`), true},
		{"a leading line comment", query("-- note\nselect nextval('events_id_seq')"), true},
		{"a leading parenthesis", query(`(select nextval('events_id_seq'))`), true},
		{"create or replace function", query(`create or replace function draw() returns bigint language sql as $$ select nextval('events_id_seq') $$`), true},
		{"a temporary table like events", query(`create temp table copies (like events including defaults)`), true},
		{"an unlogged table like events", query(`create unlogged table copies (like events including defaults)`), true},
		{"a set without local", query(`set statement_timeout = '1s'; select nextval('events_id_seq')`), true},
		{"a cursor", query(`declare c cursor for select nextval('events_id_seq')`), true},
		{"a domain default", query(`create domain event_ids as bigint default nextval('events_id_seq')`), true},
		{"a values CTE", query(`with x as (values (nextval('events_id_seq'))) insert into staged table x`), true},
		{"an upper-case CTE", query(`WITH r AS (SELECT nextval('events_id_seq')) SELECT * FROM r`), true},
		{"a restart", query(`alter sequence events_id_seq restart with 1`), true},
		{"a restart in a do block", query(`do $$ begin alter sequence events_id_seq restart; end $$`), true},
		{"ts_stat", query(`select ts_stat('select nextval(''events_id_seq'')::text::tsvector')`), true},
		{"ts_rewrite", query(`select ts_rewrite('a'::tsquery, 'select nextval(''events_id_seq'')::text::tsquery, ''b''::tsquery')`), true},
		{"ts_stat of a built query", query(`select * from ts_stat(format($2::text, $1::text))`), true},
		{"a draw another function returns", "package scratch\nfunc expr() string { return \"nextval('events_id_seq')\" }\nfunc planted() { database.QueryRow(ctx, \"select \"+expr()) }\n", true},
		{"a builder", "package scratch\nfunc planted() { var b strings.Builder; b.WriteString(\"select \"); b.WriteString(\"nextval('events_id_seq')\"); database.QueryRow(ctx, b.String()) }\n", true},
		{"a Sprintf around a draw", "package scratch\nfunc planted() { database.Query(ctx, fmt.Sprintf(`select %s from t`, \"nextval('events_id_seq')\")) }\n", true},
		{"a query in a local", "package scratch\nfunc planted() { query := `select query_to_xml('select next' || 'val(16402)', false, false, '')`; database.Query(ctx, query) }\n", true},
		{"a draw queued on a batch", "package scratch\nfunc planted(batch *pgx.Batch) { batch.Queue(\"select nextval('events_id_seq')\") }\n", true},
		{"a draw handed to a helper that runs it", "package scratch\nfunc forward(database *pgxpool.Pool, query string) { database.Exec(ctx, query) }\nfunc planted(database *pgxpool.Pool) { forward(database, \"select setval('events_id_seq', 1)\") }\n", true},
		{"a draw held in a struct field", "package scratch\nvar steps = []step{{sql: \"select nextval('events_id_seq')\"}}\nfunc planted() { for _, s := range steps { database.Exec(ctx, s.sql) } }\n", true},
		{"control: a plain read", query(`select 1`), false},
		{"control: an EXPLAIN of a prepared statement", query(`explain (format json) execute list_open_asks('nonexistent-issue')`), false},
		{"control: a trigger's execute function", query(`create trigger events_before before insert on events for each row execute function f()`), false},
		{"control: the latest event id read", query(`insert into refs (event_id) values ((select pg_catalog.currval(pg_catalog.pg_get_serial_sequence('events', 'id'))))`), false},
		{"control: another sequence", query(`select nextval('other_seq')`), false},
		{"control: prose that says execute", "package scratch\nfunc planted() error { return fmt.Errorf(\"execute the plan: %w\", err) }\n", false},
		{"control: a log line that starts with execute and a verb", "package scratch\nfunc planted() error { return fmt.Errorf(\"execute %s: %w\", name, err) }\n", false},
	} {
		if got := len(scanPlanted(t, row.source).offences) > 0; got != row.refused {
			t.Errorf("%s: refused %t, want %t", row.name, got, row.refused)
		}
	}

	// The four shapes of review 6013136356, each a statement written whole in one literal beside a
	// run-time value, and one row for each part of the reading that refuses them that no other
	// row needs: each must be refused, and by the reading that names it.
	for _, row := range []struct{ name, source, names string }{
		{
			"a run-time value before a do block that executes a query",
			"package scratch\nfunc planted() { database.Exec(ctx, prefix()+\"do $$ declare q text := (select body from scripts); begin execute q; end $$\") }\n",
			"runs SQL a static guard cannot read (execute)",
		},
		{
			"a Sprintf whose format starts with a verb and a semicolon",
			"package scratch\nfunc planted() { database.Exec(ctx, fmt.Sprintf(\"%s; do $$ begin execute %s; end $$\", lead, query)) }\n",
			"the parser cannot read it and it holds execute",
		},
		{
			"a run-time fragment inside a quoted string",
			"package scratch\nfunc planted() { database.Exec(ctx, \"start transaction; do $$ begin execute 'select \"+part+\"'; end $$\") }\n",
			"a run-time fragment completes it and it holds execute",
		},
		{
			"a run-time value before a new default for events.id",
			"package scratch\nfunc planted() { database.Exec(ctx, prefix()+\"alter table events alter column id set default 0\") }\n",
			"alter events.id default of the events id sequence or default",
		},
		{
			"a Sprintf whose verb stands right before the statement",
			"package scratch\nfunc planted() { database.Exec(ctx, fmt.Sprintf(\"%s do $$ begin execute %s; end $$\", lead, query)) }\n",
			"the parser cannot read it and it holds execute",
		},
		{
			"a statement's first word in a literal of its own after a run-time value",
			"package scratch\nfunc planted() { database.Exec(ctx, prefix()+\"do\"+\" $$ begin execute q; end $$\") }\n",
			"the parser cannot read it and it holds execute",
		},
		{
			"a new default for events.id completed at run time",
			"package scratch\nfunc planted() { database.Exec(ctx, prefix()+\"alter table events alter column id set default \"+value) }\n",
			"holds alter table events alter column id",
		},
	} {
		offences := scanPlanted(t, row.source).offences
		if !slices.ContainsFunc(offences, func(offence string) bool { return strings.Contains(offence, row.names) }) {
			t.Errorf("%s: offences %q, want one naming %q", row.name, offences, row.names)
		}
	}
}

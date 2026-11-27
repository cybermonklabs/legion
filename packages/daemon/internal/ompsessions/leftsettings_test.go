package ompsessions

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// settingCaptures are the files testdata/settings must hold: the parameters of context user on
// PostgreSQL 16, on Cloud SQL for PostgreSQL 16 and on PostgreSQL 18, copied byte for byte from Oh
// My Pi's packages/coding-agent/test/session/fixtures/postgres-settings at
// v18.18.6-sami.20261122-091943, each headed by `#` lines that name the server, the date and the
// query it was read with.
var settingCaptures = []string{"postgresql-16.txt", "cloud-sql-postgresql-16.txt", "postgresql-18.txt"}

// storeTables are the five tables of Oh My Pi's session store at the pinned release (createTables'
// comment in ompsessions.go): a temporary table at each name is an entry of the class.
var storeTables = []string{"omp_session_files", "omp_session_files_parts", "omp_session_leases", "omp_session_files_blobs", "omp_session_files_refs"}

// captureNames is every parameter name of the captures in testdata/settings, each once, sorted, and
// the files it read: each line of a capture that is neither blank nor a `#` header line is one name.
// Each file of settingCaptures must be there; a capture added beside them is read too.
func captureNames(t *testing.T) (names, files []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "settings", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, capture := range settingCaptures {
		if !slices.Contains(files, filepath.Join("testdata", "settings", capture)) {
			t.Fatalf("UNKNOWN: testdata/settings holds no %s, so the class is not known (it holds %v)", capture, files)
		}
	}
	seen := map[string]bool{}
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.SplitSeq(string(body), "\n") {
			name := strings.TrimSpace(line)
			if name == "" || strings.HasPrefix(name, "#") {
				continue
			}
			seen[name] = true
		}
	}
	return slices.Sorted(maps.Keys(seen)), files
}

// The client's list of the class holds exactly the class: one entry for each parameter of context
// user that a capture of testdata/settings names (Oh My Pi's own captures of PostgreSQL 16, Cloud SQL
// for PostgreSQL 16 and PostgreSQL 18), one for role, and one for a temporary table at each of the
// five store tables' names, and nothing else. The test names each name of the class the list lacks,
// each entry that is not of the class, and each name more than one entry holds.
func TestLeftSettingsHoldsEveryParameterOfTheClass(t *testing.T) {
	names, files := captureNames(t)
	want := map[string]bool{"role": true}
	for _, name := range names {
		want[name] = true
	}
	for _, table := range storeTables {
		want[temporaryTable+table] = true
	}
	held := map[string]int{}
	for _, setting := range LeftSettings {
		held[setting.Name]++
	}
	var missing, extra, twice []string
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if held[name] == 0 {
			missing = append(missing, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(held)) {
		if !want[name] {
			extra = append(extra, name)
		}
		if held[name] > 1 {
			twice = append(twice, fmt.Sprintf("%s (%d entries)", name, held[name]))
		}
	}
	if len(missing) > 0 {
		t.Errorf("LeftSettings holds no entry for %d names of the class:\n\t%s", len(missing), strings.Join(missing, "\n\t"))
	}
	if len(extra) > 0 {
		t.Errorf("LeftSettings holds %d entries that are not of the class:\n\t%s", len(extra), strings.Join(extra, "\n\t"))
	}
	if len(twice) > 0 {
		t.Errorf("LeftSettings holds more than one entry for %d names:\n\t%s", len(twice), strings.Join(twice, "\n\t"))
	}
	t.Logf("the class: %d parameter names in %v, role and %d temporary tables, %d names in all; LeftSettings holds %d entries",
		len(names), files, len(storeTables), len(want), len(LeftSettings))
}

// exemptReasons are the two reasons an entry may be exempt for: an exempt entry's Reason starts with
// one of them, then says why it holds for the package's own statements.
var exemptReasons = []string{cannotChange, actsBetween}

// heldSetting is what an entry must be: its kind, its form, and the text after SET LOCAL <name> in
// OpenRead and in OpenWrite.
type heldSetting struct {
	kind        Kind
	form        Form
	read, write string
}

// heldSettings are the entries whose kind and values are fixed: the statements the openings start
// with, each with its value (application_name first, then exit_on_error and the timeouts, then the
// search_path, restrict_nonsystem_relation_kind and row_security pins, row_security off in a read
// alone), the other entries the openings set by SET LOCAL to a value of their own or TO DEFAULT, the
// ones only the set_config statement can set because a supported server lacks them, the six the
// BEGIN clause sets, and the two the code no longer depends on. Among them are the thirteen
// parameters the client depends on (statement_timeout, lock_timeout,
// idle_in_transaction_session_timeout, transaction_timeout, bytea_output, exit_on_error,
// client_encoding, default_tablespace, default_table_access_method, search_path,
// default_transaction_read_only, quote_all_identifiers and cloudsql_session_read_only), so none of
// them can be exempt.
var heldSettings = map[string]heldSetting{
	"application_name":                    {SetInOpening, SetLocal, "= 'legion-session-client'", "= 'legion-session-client'"},
	"exit_on_error":                       {SetInOpening, SetLocal, "TO DEFAULT", "TO DEFAULT"},
	"statement_timeout":                   {SetInOpening, SetLocal, "TO DEFAULT", "TO DEFAULT"},
	"lock_timeout":                        {SetInOpening, SetLocal, "TO DEFAULT", "TO DEFAULT"},
	"idle_in_transaction_session_timeout": {SetInOpening, SetLocal, "TO DEFAULT", "TO DEFAULT"},
	"search_path":                         {SetInOpening, SetLocal, "= pg_catalog, pg_temp", "= pg_catalog, pg_temp"},
	"restrict_nonsystem_relation_kind":    {SetInOpening, SetLocal, "= 'view, foreign-table'", "= 'view, foreign-table'"},
	"row_security":                        {SetInOpening, SetLocal, "= off", "TO DEFAULT"},
	"role":                                {SetInOpening, SetLocal, "TO DEFAULT", "TO DEFAULT"},
	"default_tablespace":                  {SetInOpening, SetLocal, "TO DEFAULT", "TO DEFAULT"},
	"default_table_access_method":         {SetInOpening, SetLocal, "TO DEFAULT", "TO DEFAULT"},
	"client_encoding":                     {SetInOpening, SetLocal, "= 'UTF8'", "= 'UTF8'"},
	"synchronous_commit":                  {SetInOpening, SetLocal, "= on", "= on"},
	"cloudsql_session_read_only":          {SetInOpening, SetConfig, "TO DEFAULT", "TO DEFAULT"},
	"transaction_timeout":                 {SetInOpening, SetConfig, "TO DEFAULT", "TO DEFAULT"},
	"default_transaction_isolation":       {SetByBegin, "", "", ""},
	"default_transaction_read_only":       {SetByBegin, "", "", ""},
	"default_transaction_deferrable":      {SetByBegin, "", "", ""},
	"transaction_isolation":               {SetByBegin, "", "", ""},
	"transaction_read_only":               {SetByBegin, "", "", ""},
	"transaction_deferrable":              {SetByBegin, "", "", ""},
	"bytea_output":                        {NoLongerADependency, "", "", ""},
	"quote_all_identifiers":               {NoLongerADependency, "", "", ""},
}

// setByConfig are the prefixes of the Cloud SQL parameters of the index advisor and the job
// scheduler, which Google's documentation does not describe: each is set by the set_config statement.
var setByConfig = []string{"google_db_advisor.", "google_job_scheduler."}

// heldLefts are the left statements the left-setting test must send for these entries at least.
var heldLefts = map[string]string{
	"statement_timeout":                   "SET statement_timeout = '50ms'",
	"lock_timeout":                        "SET lock_timeout = '50ms'",
	"idle_in_transaction_session_timeout": "SET idle_in_transaction_session_timeout = '50ms'",
	"client_encoding":                     "SET client_encoding = 'LATIN1'",
	"default_tablespace":                  "SET default_tablespace = 'pg_global'",
	"search_path":                         "SET search_path = 'pg_catalog'",
	"default_transaction_isolation":       "SET default_transaction_isolation = 'serializable'",
	"default_transaction_read_only":       "SET default_transaction_read_only = on",
	"exit_on_error":                       "SET exit_on_error = on",
	"row_security":                        "SET row_security = off",
	"application_name":                    "SET application_name = 'left'",
	"role":                                "SET ROLE left_no_rights",
}

// settingFaults is what is wrong with one entry by the rules every entry keeps, nil for nothing: one
// kind of the four; a set entry names its form and a value for each opening, SET LOCAL only for a
// parameter every supported server has, which a name some server lacks would fail the whole opening
// on, and set_config only for one some server lacks, at the reset value; an entry of another kind
// names neither; every entry gives its reason, an exempt one starting with one of exemptReasons; and
// every entry names its left statement.
func settingFaults(setting LeftSetting) []string {
	var faults []string
	switch setting.Kind {
	case SetInOpening:
		switch setting.Form {
		case SetLocal:
			if setting.Servers != "" {
				faults = append(faults, fmt.Sprintf("it is set by SET LOCAL, which fails the whole opening on a server that lacks it, and it is %s", setting.Servers))
			}
			if setting.ReadValue == "" || setting.WriteValue == "" {
				faults = append(faults, fmt.Sprintf("it is set by SET LOCAL with no value for each opening (%q in OpenRead, %q in OpenWrite)", setting.ReadValue, setting.WriteValue))
			}
		case SetConfig:
			if setting.Servers == "" {
				faults = append(faults, "it is set by set_config, the statement for a parameter some supported server lacks, and every supported server has it")
			}
			if setting.ReadValue != "TO DEFAULT" || setting.WriteValue != "TO DEFAULT" {
				faults = append(faults, fmt.Sprintf("set_config sets its reset value, TO DEFAULT, and the entry names %q in OpenRead and %q in OpenWrite", setting.ReadValue, setting.WriteValue))
			}
		default:
			faults = append(faults, fmt.Sprintf("it is set in the opening with no form of the two (%q)", setting.Form))
		}
	case SetByBegin, NoLongerADependency, Exempt:
		if setting.Form != "" || setting.ReadValue != "" || setting.WriteValue != "" {
			faults = append(faults, fmt.Sprintf("it is %s, and names a form (%q) or a value (%q, %q) of the opening", setting.Kind, setting.Form, setting.ReadValue, setting.WriteValue))
		}
	default:
		faults = append(faults, fmt.Sprintf("its kind %q is none of the four", setting.Kind))
	}
	switch setting.Servers {
	case "", CloudSQLOnly, PostgreSQL17AndLater, PostgreSQL18AndLater:
	default:
		faults = append(faults, fmt.Sprintf("its servers %q are none of the supported servers' kinds", setting.Servers))
	}
	if strings.TrimSpace(setting.Reason) == "" {
		faults = append(faults, "it gives no reason")
	}
	if setting.Kind == Exempt && !slices.ContainsFunc(exemptReasons, func(reason string) bool {
		return strings.HasPrefix(setting.Reason, reason) && strings.TrimSpace(strings.TrimPrefix(setting.Reason, reason)) != ""
	}) {
		faults = append(faults, fmt.Sprintf("it is exempt, and its reason %q does not start with one of the two reasons an entry may be exempt for and then say why it holds: %q", setting.Reason, exemptReasons))
	}
	if !strings.HasPrefix(setting.Left, "SET ") && !strings.HasPrefix(setting.Left, "CREATE TEMPORARY TABLE ") {
		faults = append(faults, fmt.Sprintf("its left statement %q is no SET and no CREATE TEMPORARY TABLE", setting.Left))
	}
	return faults
}

// Every entry of the list has one kind of the four and its reason, and names the statement that
// leaves it (settingFaults). The entries whose kind and values the requirements fix have them
// (heldSettings): application_name, exit_on_error, the timeouts, the search_path,
// restrict_nonsystem_relation_kind and row_security pins with row_security off in a read alone,
// role, default_tablespace, default_table_access_method, client_encoding and synchronous_commit by
// SET LOCAL; cloudsql_session_read_only, transaction_timeout and every Cloud SQL index advisor and
// job scheduler parameter by the set_config statement; the six transaction parameters by the BEGIN
// clause; bytea_output, quote_all_identifiers and each temporary table as no longer a dependency.
// heldSettings holds each of the thirteen parameters the client depends on, so none of them is
// exempt.
// The left statements the left-setting test needs at least are the ones it names (heldLefts).
func TestEveryLeftSettingHasOneKindAndItsReason(t *testing.T) {
	byName := map[string]LeftSetting{}
	for _, setting := range LeftSettings {
		byName[setting.Name] = setting
		for _, fault := range settingFaults(setting) {
			t.Errorf("%s: %s", setting.Name, fault)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(heldSettings)) {
		want := heldSettings[name]
		setting, ok := byName[name]
		if !ok {
			t.Errorf("%s: the list holds no entry, want it %s (%q, OpenRead %q, OpenWrite %q)", name, want.kind, want.form, want.read, want.write)
			continue
		}
		if got := (heldSetting{setting.Kind, setting.Form, setting.ReadValue, setting.WriteValue}); got != want {
			t.Errorf("%s: it is %s (%q, OpenRead %q, OpenWrite %q), want %s (%q, OpenRead %q, OpenWrite %q)",
				name, got.kind, got.form, got.read, got.write, want.kind, want.form, want.read, want.write)
		}
	}
	configured := 0
	for _, setting := range LeftSettings {
		if !slices.ContainsFunc(setByConfig, func(prefix string) bool { return strings.HasPrefix(setting.Name, prefix) }) {
			continue
		}
		configured++
		if setting.Kind != SetInOpening || setting.Form != SetConfig {
			t.Errorf("%s: it is %s (%q), want it set by the set_config statement: Google's documentation does not say what it does", setting.Name, setting.Kind, setting.Form)
		}
	}
	if configured == 0 {
		t.Errorf("the list holds no parameter of %v, the Cloud SQL index advisor's and job scheduler's", setByConfig)
	}
	for _, table := range storeTables {
		name := temporaryTable + table
		setting, ok := byName[name]
		if !ok {
			t.Errorf("%s: the list holds no entry", name)
			continue
		}
		if setting.Kind != NoLongerADependency {
			t.Errorf("%s: it is %s, want %s: every statement names public.<table>, and pg_temp is last on the pinned search_path", name, setting.Kind, NoLongerADependency)
		}
		if want := "CREATE TEMPORARY TABLE " + table + " (path text)"; setting.Left != want {
			t.Errorf("%s: it is left by %q, want %q", name, setting.Left, want)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(heldLefts)) {
		if setting, ok := byName[name]; ok && setting.Left != heldLefts[name] {
			t.Errorf("%s: it is left by %q, want %q", name, setting.Left, heldLefts[name])
		}
	}
	kinds := map[Kind]int{}
	for _, setting := range LeftSettings {
		kinds[setting.Kind]++
	}
	t.Logf("%d entries: %d %s, %d %s, %d %s, %d %s; %d of the set_config parameters %v", len(LeftSettings),
		kinds[SetInOpening], SetInOpening, kinds[SetByBegin], SetByBegin, kinds[NoLongerADependency], NoLongerADependency,
		kinds[Exempt], Exempt, configured, setByConfig)
}

// The statements every opening holds at fixed places: the application_name mark right after BEGIN,
// the statement that stops a transaction_timeout timer another client's value started at BEGIN, just
// before the last, and the start of the last, which sets each entry of the set_config form to its
// reset value.
const (
	applicationNameLine  = "SET LOCAL application_name = 'legion-session-client';"
	stopTransactionTimer = "SELECT pg_catalog.set_config(s.name, '0', true) FROM pg_catalog.pg_settings s WHERE s.name = 'transaction_timeout'"
	resetPrefix          = "SELECT pg_catalog.set_config(s.name, s.reset_val, true) FROM pg_catalog.pg_settings s WHERE s.name IN ("
)

// openingHead are the entries each opening sets first by SET LOCAL, in this order: application_name,
// so the server's log names every later statement; exit_on_error, so a later refusal in the opening
// ends no backend; the three timeouts; then the search_path, restrict_nonsystem_relation_kind and
// row_security pins. Every other SET LOCAL entry follows in the list's order.
var openingHead = []string{
	"application_name", "exit_on_error", "statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout",
	"search_path", "restrict_nonsystem_relation_kind", "row_security",
}

// placeholder is a bind parameter, which psql sends as it stands and the server then refuses.
var placeholder = regexp.MustCompile(`\$[0-9]`)

// doBlock is a DO statement, which loads PL/pgSQL into the backend, so that its parameters of context
// user join the class on every backend the client touched.
var doBlock = regexp.MustCompile(`(?i)\bDO\b`)

// plainSQLFaults is what keeps text, an opening as open.go writes it, from being plain SQL that a
// reader copies and sends with psql -c as it stands, nil for nothing: a bind parameter, a % that a Go
// format would read, a backslash, which psql reads as a meta-command, or a DO block; a line that is
// not one statement, or that does not end in its one semicolon where it is not the last; a first line
// other than begin, or a second other than the application_name mark; and a last statement other
// than the set_config one.
func plainSQLFaults(text, begin string) []string {
	var faults []string
	if placeholder.MatchString(text) {
		faults = append(faults, fmt.Sprintf("it holds a bind parameter %q, which psql sends as it stands", placeholder.FindString(text)))
	}
	if strings.Contains(text, "%") {
		faults = append(faults, "it holds a %, which a Go format would read")
	}
	if strings.Contains(text, `\`) {
		faults = append(faults, `it holds a \, which psql reads as a meta-command`)
	}
	if doBlock.MatchString(text) {
		faults = append(faults, "it holds a DO block, which loads PL/pgSQL and its parameters into the backend")
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		last := i == len(lines)-1
		switch {
		case line == "" || strings.TrimSpace(line) != line:
			faults = append(faults, fmt.Sprintf("line %d, %q, is blank or has space around it, want one statement", i+1, line))
		case !last && (!strings.HasSuffix(line, ";") || strings.Count(line, ";") != 1):
			faults = append(faults, fmt.Sprintf("line %d, %q, is not one statement ending in its one ;", i+1, line))
		case last && strings.Contains(line, ";"):
			faults = append(faults, fmt.Sprintf("the last line, %q, holds a ;, want one statement with none", line))
		}
	}
	if lines[0] != begin {
		faults = append(faults, fmt.Sprintf("its first line is %q, want %q", lines[0], begin))
	}
	if len(lines) < 3 {
		return append(faults, fmt.Sprintf("it holds %d lines, want BEGIN, the application_name mark, and at least the set_config statement", len(lines)))
	}
	if lines[1] != applicationNameLine {
		faults = append(faults, fmt.Sprintf("its second line is %q, want %q", lines[1], applicationNameLine))
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, resetPrefix) || !strings.HasSuffix(last, ")") {
		faults = append(faults, fmt.Sprintf("its last statement is %q, want %s…)", last, resetPrefix))
	}
	return faults
}

// Each opening is one const of open.go whose value is one raw string literal, so a reader copies it
// from the file and sends it with psql -c as it stands, as the check that sends each under Cloud
// SQL's locked read-only session does: plain SQL with no bind parameter, no % and no backslash, and
// no DO block; one statement on each line, each line but the last ending in a semicolon; BEGIN
// ISOLATION LEVEL READ COMMITTED with READ ONLY or READ WRITE first, the application_name mark second,
// and the set_config statement last. The test reads open.go with go/parser, and the literal it reads
// must be the constant the package compiles.
func TestTheOpeningsArePlainSQLThatPsqlSendsAsTheyStand(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "open.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string][]*ast.ValueSpec{}
	for _, decl := range file.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				if name.Name != "OpenRead" && name.Name != "OpenWrite" {
					continue
				}
				if general.Tok != token.CONST {
					t.Errorf("%s: open.go declares it with %s, want const", fset.Position(name.Pos()), general.Tok)
				}
				declared[name.Name] = append(declared[name.Name], value)
			}
		}
	}
	for _, opening := range []struct{ name, constant, begin string }{
		{"OpenRead", OpenRead, "BEGIN ISOLATION LEVEL READ COMMITTED READ ONLY;"},
		{"OpenWrite", OpenWrite, "BEGIN ISOLATION LEVEL READ COMMITTED READ WRITE;"},
	} {
		t.Run(opening.name, func(t *testing.T) {
			specs := declared[opening.name]
			if len(specs) != 1 {
				t.Fatalf("open.go declares %s %d times, want once", opening.name, len(specs))
			}
			if len(specs[0].Names) != 1 || len(specs[0].Values) != 1 {
				t.Fatalf("%s: open.go declares %d names with %d values in its spec, want it alone with one value",
					fset.Position(specs[0].Pos()), len(specs[0].Names), len(specs[0].Values))
			}
			literal, ok := specs[0].Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING || !strings.HasPrefix(literal.Value, "`") {
				t.Fatalf("%s: %s's value is %T, want one raw (backquoted) string literal, which a reader copies as it stands",
					fset.Position(specs[0].Values[0].Pos()), opening.name, specs[0].Values[0])
			}
			text, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("%s: %s's raw string literal does not unquote: %v", fset.Position(literal.Pos()), opening.name, err)
			}
			if text != opening.constant {
				t.Fatalf("UNKNOWN: the literal open.go writes for %s is not the constant the package compiles", opening.name)
			}
			for _, fault := range plainSQLFaults(text, opening.begin) {
				t.Errorf("%s: %s", opening.name, fault)
			}
			t.Logf("%s: %d lines, %d bytes", opening.name, strings.Count(text, "\n")+1, len(text))
		})
	}
}

// openingFaults is how opening, the text of OpenRead (read) or OpenWrite, differs from what
// LeftSettings marks as set in the opening message, nil for nothing. It reads each SET LOCAL
// statement's name and value, and the names of the last statement's IN list, each quoted. It names
// each entry the list marks set by SET LOCAL that the opening does not set, or sets to a value other
// than the list's for that opening, and each entry marked set by set_config that the IN list lacks;
// each name the opening sets that the list does not mark so; a SET LOCAL or a name of the IN list
// that comes twice or out of the order the openings keep (openingHead, then the list's order); and
// any other statement, but the one that stops a transaction_timeout timer, which must come just
// before the last statement.
func openingFaults(opening string, read bool) []string {
	var faults []string
	statements := strings.Split(opening, "\n")
	if !strings.HasPrefix(statements[0], "BEGIN ") {
		faults = append(faults, fmt.Sprintf("its first statement, %q, is no BEGIN", statements[0]))
	}
	local := map[string]string{}
	var localOrder, configOrder []string
	config := map[string]bool{}
	stopped := false
	for i := 1; i < len(statements); i++ {
		statement := strings.TrimSuffix(statements[i], ";")
		switch {
		case strings.HasPrefix(statement, "SET LOCAL "):
			name, value, _ := strings.Cut(strings.TrimPrefix(statement, "SET LOCAL "), " ")
			if _, twice := local[name]; twice {
				faults = append(faults, fmt.Sprintf("%s: the opening sets it by SET LOCAL twice", name))
			}
			local[name] = value
			localOrder = append(localOrder, name)
		case statement == stopTransactionTimer:
			stopped = true
			if i != len(statements)-2 {
				faults = append(faults, fmt.Sprintf("statement %d stops the transaction_timeout timer, and it is not just before the last statement", i+1))
			}
		case i == len(statements)-1 && strings.HasPrefix(statement, resetPrefix) && strings.HasSuffix(statement, ")"):
			for quoted := range strings.SplitSeq(strings.TrimSuffix(strings.TrimPrefix(statement, resetPrefix), ")"), ", ") {
				name := strings.TrimSuffix(strings.TrimPrefix(quoted, "'"), "'")
				if quoted != "'"+name+"'" || name == "" {
					faults = append(faults, fmt.Sprintf("the set_config statement names %q, want each name quoted", quoted))
					continue
				}
				if config[name] {
					faults = append(faults, fmt.Sprintf("%s: the set_config statement names it twice", name))
				}
				config[name] = true
				configOrder = append(configOrder, name)
			}
		default:
			faults = append(faults, fmt.Sprintf("statement %d, %q, is no SET LOCAL, no stop of the transaction_timeout timer and no last set_config statement", i+1, statements[i]))
		}
	}
	var wantLocal, wantConfig []string
	byName := map[string]LeftSetting{}
	for _, setting := range LeftSettings {
		byName[setting.Name] = setting
		if setting.Kind != SetInOpening {
			continue
		}
		value := setting.WriteValue
		if read {
			value = setting.ReadValue
		}
		switch setting.Form {
		case SetLocal:
			wantLocal = append(wantLocal, setting.Name)
			got, ok := local[setting.Name]
			switch {
			case !ok:
				faults = append(faults, fmt.Sprintf("%s: the list marks it set by SET LOCAL %s, and the opening does not set it", setting.Name, value))
			case got != value:
				faults = append(faults, fmt.Sprintf("%s: the opening sets it by SET LOCAL %s, and the list marks it %s", setting.Name, got, value))
			}
		case SetConfig:
			wantConfig = append(wantConfig, setting.Name)
			if !config[setting.Name] {
				faults = append(faults, fmt.Sprintf("%s: the list marks it set by the set_config statement, and the opening's set_config statement does not name it", setting.Name))
			}
		}
	}
	notMarked := func(name, how string) {
		setting, ok := byName[name]
		switch {
		case !ok:
			faults = append(faults, fmt.Sprintf("%s: the opening sets it by %s, and the list holds no entry for it", name, how))
		case setting.Kind != SetInOpening:
			faults = append(faults, fmt.Sprintf("%s: the opening sets it by %s, and the list does not mark it set in the opening (it is %s)", name, how, setting.Kind))
		default:
			faults = append(faults, fmt.Sprintf("%s: the opening sets it by %s, and the list does not mark it so (it marks it %s)", name, how, setting.Form))
		}
	}
	for _, name := range localOrder {
		if !slices.Contains(wantLocal, name) {
			notMarked(name, "SET LOCAL")
		}
	}
	for _, name := range configOrder {
		if !slices.Contains(wantConfig, name) {
			notMarked(name, "the set_config statement")
		}
	}
	ordered := slices.Concat(openingHead, slices.DeleteFunc(slices.Clone(wantLocal), func(name string) bool { return slices.Contains(openingHead, name) }))
	inBoth := func(names, other []string) []string {
		return slices.DeleteFunc(slices.Clone(names), func(name string) bool { return !slices.Contains(other, name) })
	}
	if got, want := inBoth(localOrder, ordered), inBoth(ordered, localOrder); !slices.Equal(got, want) {
		faults = append(faults, fmt.Sprintf("the SET LOCAL statements come in the order %v, want %v", got, want))
	}
	if got, want := inBoth(configOrder, wantConfig), inBoth(wantConfig, configOrder); !slices.Equal(got, want) {
		faults = append(faults, fmt.Sprintf("the set_config statement names %v in that order, want %v", got, want))
	}
	if slices.Contains(wantConfig, "transaction_timeout") && !stopped {
		faults = append(faults, "the opening does not stop the timer a left transaction_timeout starts at BEGIN before the set_config statement sets the parameter back")
	}
	return faults
}

// Each opening sets exactly the entries LeftSettings marks as set in the opening message, each in
// the form the list names and with the list's value for that opening, and no other name, and stops
// a left transaction_timeout's timer just before its last statement (openingFaults). Two controls
// show the comparison names each fault: a copy of OpenWrite without its SET LOCAL lock_timeout TO
// DEFAULT, which must be named as missing, and a copy with a SET LOCAL of the first entry the list
// marks exempt, which must be named as not marked.
func TestTheOpeningsSetEachEntryTheListMarksAndNoOther(t *testing.T) {
	const dropped = "SET LOCAL lock_timeout TO DEFAULT;\n"
	exemptIndex := slices.IndexFunc(LeftSettings, func(setting LeftSetting) bool { return setting.Kind == Exempt && setting.Servers == "" })
	if exemptIndex < 0 {
		t.Fatal("UNKNOWN: the list marks no entry every supported server has exempt, so the second control has none to add")
	}
	added := LeftSettings[exemptIndex].Name
	for _, control := range []struct{ name, opening, want string }{
		{"control: OpenWrite without " + strings.TrimSuffix(dropped, ";\n"), strings.Replace(OpenWrite, dropped, "", 1),
			"lock_timeout: the list marks it set by SET LOCAL TO DEFAULT, and the opening does not set it"},
		{"control: OpenWrite with SET LOCAL " + added + " TO DEFAULT",
			strings.Replace(OpenWrite, applicationNameLine+"\n", applicationNameLine+"\nSET LOCAL "+added+" TO DEFAULT;\n", 1),
			added + ": the opening sets it by SET LOCAL, and the list does not mark it set in the opening (it is exempt)"},
	} {
		t.Run(control.name, func(t *testing.T) {
			if control.opening == OpenWrite {
				t.Fatal("UNKNOWN: the control's change found no line of OpenWrite to change, so it is OpenWrite itself")
			}
			faults := openingFaults(control.opening, false)
			if !slices.Contains(faults, control.want) {
				t.Errorf("the comparison named\n\t%s\nwant it to name\n\t%s", strings.Join(faults, "\n\t"), control.want)
			}
			t.Logf("the comparison named: %s", strings.Join(faults, "; "))
		})
	}
	for _, opening := range []struct {
		name, text string
		read       bool
	}{{"OpenRead", OpenRead, true}, {"OpenWrite", OpenWrite, false}} {
		t.Run(opening.name, func(t *testing.T) {
			for _, fault := range openingFaults(opening.text, opening.read) {
				t.Errorf("%s: %s", opening.name, fault)
			}
		})
	}
}

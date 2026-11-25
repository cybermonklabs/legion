package api

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runbookPaths are Dispatch's runbook: Running Dispatch, whose "Signing people in" makes the
// broker's libpq URL the blocks name, and After a restore, whose recoveries after a Dispatch or a
// broker database restored or made again alone the tests here run block by block. Each lead the
// tests name starts one line of the two together.
var runbookPaths = []string{
	"../../../../../docs/site/src/content/docs/dispatch/running-dispatch.md",
	"../../../../../docs/site/src/content/docs/dispatch/after-a-restore.md",
}

// The leads of the runbook's paragraphs whose first ```sh block the tests here run.
const (
	brokerURLLead      = "Every block below reaches its database with `psql`, given a URL that holds no password"
	madeAgainLead      = "A broker database **made again**, rather than restored, has no such moment"
	brokerRestoredLead = "The other way round, when it is **the broker's database that was restored alone**"
	// repeatResetLead opens the read of whether a broker backup may predate the last reset's end.
	repeatResetLead = "A broker backup can also predate the end of a reset"
	// poolSweepAgainLead opens the paragraph that runs the pool-to-domain sweep again after a
	// broker restored alone.
	poolSweepAgainLead = "A broker backup can also predate a move from the Cognito pool to a domain"
)

// TestTheRunbookPassesNoBearerTokenOnACommandLine: a token in curl's arguments sits in
// /proc/<pid>/cmdline while the call runs, which every user of the box can read. The daemon's
// textcheck (TestNoTextGivesACredentialAsACommandArgument) holds every tracked file, this runbook
// among them, to no credential given as a command argument; this test holds the restore's
// directory read to the form that check asks for: its token written to a file the step makes,
// and read with -H @"$header".
func TestTheRunbookPassesNoBearerTokenOnACommandLine(t *testing.T) {
	t.Parallel()
	directory := strings.Join(runbookBlockAfter(t, resetDirectoryLead).lines, "\n")
	if !strings.Contains(directory, `-H @"$header"`) || !strings.Contains(directory, `printf 'Authorization: Bearer %s\n' "$DIRECTORY_TOKEN" >"$header"`) {
		t.Errorf("the runbook's directory read is %q; want its token written to a file and read with -H @\"$header\"", directory)
	}
}

// runbookPsqlCall is the one form a psql call of a runbook page takes: the password variable a
// PGPASSWORD before it names, and the URL variable it is given.
var runbookPsqlCall = regexp.MustCompile(`PGPASSWORD="\$([A-Z]+)_PGPASSWORD" psql "\$([A-Z_]+)"`)

// psqlCommand is psql named as a command, in any form: given a variable quoted or not, a URL or a
// keyword/value string written out, or nothing. psql_url and DISPATCH_PSQL_URL are other words.
var psqlCommand = regexp.MustCompile(`\bpsql\b`)

// runbookPsqlPages are the pages whose psql calls TestTheRunbookPassesNoDatabasePasswordOnACommandLine
// holds to runbookPsqlCall: the runbook's two pages, the store README, the delivery audit's and the
// comment-delivery run's (scripts/dispatch).
var runbookPsqlPages = append(append([]string{}, runbookPaths...), "../store/README.md", "../deliveryaudit/README.md",
	"../../../../../scripts/dispatch/README.md")

// TestTheRunbookPassesNoDatabasePasswordOnACommandLine: a database URL that holds its password,
// given to psql as an argument, sits in /proc/<pid>/cmdline while the call runs, which every user
// of the box can read. So every psql command in a fenced block of runbookPsqlPages is given its
// database's password-free URL, DISPATCH_PSQL_URL or BROKER_PSQL_URL, and that database's password
// in PGPASSWORD, which the environment carries rather than the command line: a psql named in any
// other form - a variable unquoted, a URL or a keyword/value string written out - fails. And the
// step that makes them (Signing people in) takes the password out of each server's URL, in its
// user info and in a password parameter, with the pool_ parameters libpq refuses, keeps every
// other parameter, reads a password the environment does not already hold from its input, and
// refuses, making no URL, a server's connection string whose password it cannot take out
// (psqlURLRows).
//
// Controls: the pages as they stood at 697b987b46 (psql "$DATABASE_URL" with the password in the
// URL); the step's sed without its user-info arm (the URL keeps the password); the delivery audit's
// README and the comment-delivery run's as they stood before this row named them (psql
// "$DATABASE_URL", twice each); the step as it stood at 74a730474c, without its two refusals (the
// keyword/value string and the unencoded / keep their passwords).
func TestTheRunbookPassesNoDatabasePasswordOnACommandLine(t *testing.T) {
	t.Parallel()
	calls := 0
	for _, path := range runbookPsqlPages {
		text, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		fenced := false
		for n, line := range strings.Split(string(text), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				fenced = !fenced
				continue
			}
			if !fenced {
				continue
			}
			commands := len(psqlCommand.FindAllStringIndex(line, -1))
			calls += commands
			sound := 0
			for _, call := range runbookPsqlCall.FindAllStringSubmatch(line, -1) {
				if call[2] == call[1]+"_PSQL_URL" && (call[1] == "DISPATCH" || call[1] == "BROKER") {
					sound++
				}
			}
			if sound != commands {
				t.Errorf("%s:%d names psql %d time(s), %d of them as PGPASSWORD=\"$DISPATCH_PGPASSWORD\" psql \"$DISPATCH_PSQL_URL\" or PGPASSWORD=\"$BROKER_PGPASSWORD\" psql \"$BROKER_PSQL_URL\"; want every one in that form: %s", path, n+1, commands, sound, strings.TrimSpace(line))
			}
		}
	}
	if calls == 0 {
		t.Fatal("UNKNOWN: the runbook's pages, the store README and the delivery audit's README hold no psql command in a fenced block, so nothing was checked")
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash is not installed, so the runbook's URL step cannot be run here: %v", err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "urls.sh")
	out := filepath.Join(dir, "urls")
	step := strings.Join(runbookBlockAfter(t, brokerURLLead).lines, "\n")
	if err := os.WriteFile(script, []byte(step+"\nprintf '%s\\n' \"$DISPATCH_PSQL_URL\" \"$BROKER_PSQL_URL\" \"$DISPATCH_PGPASSWORD\" \"$BROKER_PGPASSWORD\" >\"$OUT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// runStep runs the step with env after the test's own environment, less any password the
	// test's environment holds, and answers the four values it made, one per line, and what it
	// printed.
	runStep := func(t *testing.T, input string, env ...string) ([]string, string) {
		t.Helper()
		command := exec.Command("bash", script)
		command.Env = append(append(slices.DeleteFunc(os.Environ(), func(v string) bool {
			return strings.HasPrefix(v, "DISPATCH_PGPASSWORD=") || strings.HasPrefix(v, "BROKER_PGPASSWORD=")
		}), "OUT="+out), env...)
		command.Stdin = strings.NewReader(input)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("the URL step: %v\n%s", err, output)
		}
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(got), "\n"), "\n"), string(output)
	}
	// Each server URL carries a password in its user part, and Dispatch's in a password= parameter
	// too, as an operator's URL can; url.UserPassword writes the user part.
	dispatchURL := url.URL{Scheme: "postgres", User: url.UserPassword("dispatch", "d-secret"), Host: "db.example:5432",
		Path: "/dispatch", RawQuery: "sslmode=require&password=q-secret"}
	brokerURL := url.URL{Scheme: "postgres", User: url.UserPassword("broker", "b-secret"), Host: "db.example:5432",
		Path: "/broker", RawQuery: "pool_max_conns=8&sslmode=disable"}
	urls := []string{"DATABASE_URL=" + dispatchURL.String(), "BROKER_DATABASE_URL=" + brokerURL.String()}
	for _, test := range []struct {
		name, input string
		env         []string
		want        []string
	}{
		{"both passwords read from the terminal", "typed-d\ntyped-b\n", nil,
			[]string{"postgres://dispatch@db.example:5432/dispatch?sslmode=require", "postgres://broker@db.example:5432/broker?sslmode=disable", "typed-d", "typed-b"}},
		{"a password the environment holds, even empty, read from it", "typed-d\ntyped-b\n", []string{"DISPATCH_PGPASSWORD=from-the-environment", "BROKER_PGPASSWORD="},
			[]string{"postgres://dispatch@db.example:5432/dispatch?sslmode=require", "postgres://broker@db.example:5432/broker?sslmode=disable", "from-the-environment", ""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if lines, _ := runStep(t, test.input, append(append([]string{}, urls...), test.env...)...); !slices.Equal(lines, test.want) {
				t.Errorf("the URL step made %q; want %q, no URL holding a password", lines, test.want)
			}
		})
	}
	for _, row := range psqlURLRows {
		t.Run(row.name, func(t *testing.T) {
			lines, output := runStep(t, "", "DATABASE_URL="+row.server, "BROKER_DATABASE_URL="+brokerURL.String(),
				"DISPATCH_PGPASSWORD=", "BROKER_PGPASSWORD=")
			if lines[0] != row.want {
				t.Errorf("the URL step made %q of %q; want %q", lines[0], row.server, row.want)
			}
			if strings.Contains(lines[0], row.password) {
				t.Errorf("the URL step's URL %q of %q still holds its password %q", lines[0], row.server, row.password)
			}
			if row.refusal != "" && !strings.Contains(output, row.refusal) {
				t.Errorf("the URL step printed %q for %q; want its refusal, %q", output, row.server, row.refusal)
			}
			if row.refusal == "" && strings.Contains(output, "psql_url: refused") {
				t.Errorf("the URL step refused %q: %q", row.server, output)
			}
		})
	}
}

// psqlURLRows are the server connection strings the runbook's URL step is run on, each holding the
// password password, and the URL it must make of each, empty for one it refuses with refusal. A
// password in the query placed first, last or alone, and one percent-encoded in the user part, are
// taken out; a keyword/value string and a user part holding an unencoded / (which libpq and
// Dispatch's own server read as the end of the host) are refused, so the password is never handed
// to psql. url.UserPassword writes the encoded user part; the unencoded one, which no url.URL
// writes, is joined from its parts.
var psqlURLRows = []struct{ name, server, password, want, refusal string }{
	{"a password parameter first", "postgres://u@h:5432/db?password=p1-secret&sslmode=require", "p1-secret",
		"postgres://u@h:5432/db?sslmode=require", ""},
	{"a password parameter last", "postgres://u@h:5432/db?sslmode=require&password=p2-secret", "p2-secret",
		"postgres://u@h:5432/db?sslmode=require", ""},
	{"a password parameter alone", "postgres://u@h:5432/db?password=p3-secret", "p3-secret",
		"postgres://u@h:5432/db", ""},
	{"a percent-encoded password in the user part",
		(&url.URL{Scheme: "postgres", User: url.UserPassword("u", "p4/secret"), Host: "h:5432", Path: "/db"}).String(), "p4%2Fsecret",
		"postgres://u@h:5432/db", ""},
	{"an unencoded / in the user part", "postgres://u" + ":" + "p5/secret" + "@" + "h:5432/db", "p5/secret",
		"", "psql_url: refused: the URL's user part holds an unencoded /"},
	{"a keyword/value string", "host=h user=u password=p6-secret dbname=db", "p6-secret",
		"", "psql_url: refused: not a postgres:// URL"},
}

// runbookBlock is one ```sh or ```bash block of the runbook: the SQL statements of its psql
// heredocs, in order, each without its closing semicolon and with its psql variables (:'name')
// turned into named placeholders (@name); its other lines, the commands, as written; and every
// line of the block as it stands, for a test that runs the block itself.
type runbookBlock struct {
	statements, commands, lines []string
}

// psqlVariable is a psql variable interpolated as a literal, :'name'.
var psqlVariable = regexp.MustCompile(`:'([a-z_]+)'`)

// runbookBlockAfter is the first ```sh or ```bash block the runbook gives after the line starting
// with lead, its lines trimmed.
func runbookBlockAfter(t *testing.T, lead string) runbookBlock {
	t.Helper()
	lines := runbookLines(t)
	start := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), lead) })
	if start < 0 {
		t.Fatalf("the runbook has no line starting %q", lead)
	}
	var block runbookBlock
	var statement []string
	fenced, heredoc := false, false
	for _, line := range lines[start:] {
		line = strings.TrimSpace(line)
		switch {
		case !fenced:
			fenced = line == "```sh" || line == "```bash"
		case heredoc && line == "SQL":
			if len(statement) > 0 {
				t.Fatalf("the runbook's block after %q ends a heredoc inside a statement: %q", lead, statement)
			}
			heredoc = false
			block.lines = append(block.lines, line)
		case heredoc && strings.HasPrefix(line, `\`):
			// A psql meta-command inside the heredoc (\copy), which is no SQL statement.
			block.commands = append(block.commands, line)
			block.lines = append(block.lines, line)
		case heredoc:
			statement = append(statement, line)
			block.lines = append(block.lines, line)
			if strings.HasSuffix(line, ";") {
				block.statements = append(block.statements,
					psqlVariable.ReplaceAllString(strings.TrimSuffix(strings.Join(statement, "\n"), ";"), "@$1"))
				statement = nil
			}
		case line == "```":
			return block
		default:
			block.commands = append(block.commands, line)
			block.lines = append(block.lines, line)
			heredoc = strings.HasSuffix(line, "<<'SQL'") || strings.HasSuffix(line, "<<SQL")
		}
	}
	t.Fatalf("the runbook gives no closed ```sh or ```bash block after %q", lead)
	return block
}

// runbookLines is the runbook, line by line: each of runbookPaths in order, a blank line between.
func runbookLines(t *testing.T) []string {
	t.Helper()
	var lines []string
	for _, path := range runbookPaths {
		text, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the runbook: %v", err)
		}
		lines = append(append(lines, strings.Split(string(text), "\n")...), "")
	}
	return lines
}

// runbookParagraph is the paragraph of the runbook that starts with lead, as one line of text: the
// branches of the recovery that are instructions rather than blocks are read from it, so a test
// follows what the page says rather than what it remembers.
func runbookParagraph(t *testing.T, lead string) string {
	t.Helper()
	lines := runbookLines(t)
	start := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), lead) })
	if start < 0 {
		t.Fatalf("the runbook has no paragraph starting %q", lead)
	}
	var paragraph []string
	for _, line := range lines[start:] {
		if strings.TrimSpace(line) == "" {
			break
		}
		paragraph = append(paragraph, strings.TrimSpace(line))
	}
	return strings.Join(paragraph, " ")
}

// runbookDatabase is a pool a block's statements run on: Dispatch's (store.Pool) or the broker's
// (pgxpool.Pool).
type runbookDatabase interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// runStatements runs statements on database with args, as psql runs a heredoc: one between begin
// and commit in one transaction, otherwise each on its own.
func runStatements(t *testing.T, database runbookDatabase, statements []string, args pgx.NamedArgs) {
	t.Helper()
	ctx := context.Background()
	if len(statements) >= 2 && statements[0] == "begin" && statements[len(statements)-1] == "commit" {
		tx, err := database.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, statement := range statements[1 : len(statements)-1] {
			if _, err := tx.Exec(ctx, statement, args); err != nil {
				t.Fatalf("the runbook's %q: %v", statement, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("the runbook's commit: %v", err)
		}
		return
	}
	for _, statement := range statements {
		if _, err := database.Exec(ctx, statement, args); err != nil {
			t.Fatalf("the runbook's %q: %v", statement, err)
		}
	}
}

// queryRunbook runs the one statement of block on database with args and answers the text of each
// row's first column, as psql -At prints it.
func queryRunbook(t *testing.T, database runbookDatabase, block runbookBlock, args pgx.NamedArgs) []string {
	t.Helper()
	if len(block.statements) != 1 {
		t.Fatalf("the runbook's block %q holds %d statements, want the one read", block.commands, len(block.statements))
	}
	rows, err := database.Query(context.Background(), block.statements[0], args)
	if err != nil {
		t.Fatalf("the runbook's %q: %v", block.statements[0], err)
	}
	values, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		columns, err := row.Values()
		if err != nil || len(columns) == 0 || columns[0] == nil {
			return "", err
		}
		return fmt.Sprint(columns[0]), nil
	})
	if err != nil {
		t.Fatalf("the runbook's %q: %v", block.statements[0], err)
	}
	return values
}

// psqlURL is the URL psql reaches pool's database by: the pool's own connection string, with a
// search_path parameter (the broker's harness gives each test a schema of its own) moved into the
// options libpq takes, since libpq refuses a parameter it does not know.
func psqlURL(t *testing.T, pool interface{ Config() *pgxpool.Config }) string {
	t.Helper()
	parsed, err := url.Parse(pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse the pool's connection string: %v", err)
	}
	query := parsed.Query()
	if schema := query.Get("search_path"); schema != "" {
		query.Del("search_path")
		query.Set("options", "-csearch_path="+schema)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// psqlPassword is the password pool signs in with, which the runbook's blocks take from
// DISPATCH_PGPASSWORD or BROKER_PGPASSWORD rather than from their URL; "" for none. Handed to a
// block in its environment, it also keeps the runbook's URL step from asking the terminal for it.
func psqlPassword(pool interface{ Config() *pgxpool.Config }) string {
	return pool.Config().ConnConfig.Password
}

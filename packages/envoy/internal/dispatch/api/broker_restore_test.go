package api

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runbookPath is Dispatch's runbook, whose recovery after a Dispatch database restored or made again
// alone ("Signing people in") the tests here run block by block.
const runbookPath = "../../../../../docs/site/src/content/docs/dispatch/running-dispatch.md"

// The leads of the runbook's paragraphs whose first ```sh block the tests here run.
const (
	brokerURLLead      = "Every block below that reads the broker's database reaches it with `psql`"
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
	directory := strings.Join(runbookBlockAfter(t, resetDirectoryLead).lines, "\n")
	if !strings.Contains(directory, `-H @"$header"`) || !strings.Contains(directory, `printf 'Authorization: Bearer %s\n' "$DIRECTORY_TOKEN" >"$header"`) {
		t.Errorf("the runbook's directory read is %q; want its token written to a file and read with -H @\"$header\"", directory)
	}
}

// runbookBlock is one ```sh block of the runbook: the SQL statements of its psql heredocs, in
// order, each without its closing semicolon and with its psql variables (:'name') turned into
// named placeholders (@name); its other lines, the commands, as written; and every line of the
// block as it stands, for a test that runs the block itself.
type runbookBlock struct {
	statements, commands, lines []string
}

// psqlVariable is a psql variable interpolated as a literal, :'name'.
var psqlVariable = regexp.MustCompile(`:'([a-z_]+)'`)

// runbookBlockAfter is the first ```sh block the runbook gives after the line starting with lead,
// its lines trimmed.
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
			fenced = line == "```sh"
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
	t.Fatalf("the runbook gives no closed ```sh block after %q", lead)
	return block
}

// runbookLines is the runbook, line by line.
func runbookLines(t *testing.T) []string {
	t.Helper()
	text, err := os.ReadFile(runbookPath)
	if err != nil {
		t.Fatalf("read the runbook: %v", err)
	}
	return strings.Split(string(text), "\n")
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

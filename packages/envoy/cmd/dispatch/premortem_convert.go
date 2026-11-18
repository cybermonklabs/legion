package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// premortemConvertUsage is what `envoy-dispatch premortem-convert` prints for -h, a usage error
// and a refusal of its flags alike, so the one place its flags and its map file are explained is
// the command itself.
const premortemConvertUsage = `usage: envoy-dispatch premortem-convert --issue <KEY> --artifact <slug> --prevented-by <file> [--dry-run]

Converts the pre-mortem document <slug> of issue <KEY>, in the database DATABASE_URL names, into
nodes, in one transaction, as the server's own credential: one lens of kind pre-mortem under the
issue, whose verdict is the document's verdict followed by its two closing sections, "What a
world-class implementation looks like" and "The smallest first slice that proves the model", word
for word; under it one risk for risk 0 and each numbered risk of the document's newest version,
each with its heading, its early warning and its prevention and a prevented_by tuple to each check
the map names for it; then the document is retired, leaving its issue's document list, and its
address moves to the lens, so GET /api/v1/references?to=<its address> answers the lens. It prints
the report as JSON: the document, the lens, and each risk with its checks and its reason. A
document converted already is reported as such, naming its lens, and nothing is written.

  --issue         the issue the document belongs to
  --artifact      the document's slug
  --prevented-by  the map file, below
  --dry-run       write nothing: every write runs and is rolled back, and the report says what
                  the conversion would write

The map file is UTF-8 text, one line for each risk, in any order, of three fields separated by
one tab each:

  1. the risk's number as the document numbers it: 0 for risk 0, 1 to 10 for the numbered risks;
  2. none, or one or more check addresses separated by one space each, each of the form
     dispatch://<KEY>/artifact/verification-md#b-<block id>, a check block of any issue;
  3. the reason, plain text to the end of the line.

A blank line, and a line whose first character is #, are skipped. For example:

  # risk	checks	reason
  0	none	No check covers a slice that ships nothing visible.
  8	dispatch://CORE-1/artifact/verification-md#b-c-visibility-kinds	The kind census fails a kind with no predicate.

The command refuses, writing nothing and naming the line: a line of any other shape, a number the
document does not hold, a number twice, a risk the document holds with no line, and an address that
names no check block that is not gone.

Exit 0 when it converted, ran dry, or found the document converted already, printing the report; 1
when it refused (the map file, or a document that is none, no pre-mortem, or retired by anything
but a conversion), naming why and writing nothing; 2 when it could not run (a flag it does not
take, no DATABASE_URL, a database it could not reach, a write that failed).
`

// premortemConvert is `envoy-dispatch premortem-convert`, the command that runs the pre-mortem
// conversion (api.ConvertPremortem, dispatch://INFRA-1950/spec#b-r-premortem-nodes), which the
// route POST /api/v1/issues/{key}/artifacts/{slug}/premortem-convert runs too. It reads its map
// from the file --prevented-by names (parsePreventedBy), runs the conversion in one transaction of
// its own against DATABASE_URL as api.PremortemConverter, the server's own credential, and prints
// the report the route answers. It migrates nothing: the schema it writes into is the
// deployment's own. The document.retired event the retirement appends reaches every server's
// stream from its commit, as any committed event does (api's event relay), so the command
// publishes nothing itself.
func premortemConvert(ctx context.Context, args []string, databaseURL string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("premortem-convert", flag.ContinueOnError)
	flags.SetOutput(errOut)
	flags.Usage = func() { fmt.Fprint(errOut, premortemConvertUsage) }
	issue := flags.String("issue", "", "the issue the document belongs to")
	artifact := flags.String("artifact", "", "the document's slug")
	mapFile := flags.String("prevented-by", "", "the map file from each risk to its checks")
	dryRun := flags.Bool("dry-run", false, "write nothing, and report what the conversion would write")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "premortem-convert: unexpected argument %q; the flags are --issue, --artifact, --prevented-by and --dry-run\n\n%s",
			flags.Arg(0), premortemConvertUsage)
		return 2
	}
	for _, required := range []struct{ name, value string }{
		{"--issue", *issue}, {"--artifact", *artifact}, {"--prevented-by", *mapFile},
	} {
		if strings.TrimSpace(required.value) == "" {
			fmt.Fprintf(errOut, "premortem-convert: %s is required\n\n%s", required.name, premortemConvertUsage)
			return 2
		}
	}
	text, err := os.ReadFile(*mapFile)
	if err != nil {
		fmt.Fprintf(errOut, "premortem-convert: read the map file: %v\n", err)
		return 2
	}
	entries, lines, err := parsePreventedBy(string(text))
	if err != nil {
		fmt.Fprintf(errOut, "premortem-convert: %s:%v\n", *mapFile, err)
		return 1
	}
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(errOut, "premortem-convert: DATABASE_URL is required")
		return 2
	}
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(errOut, "premortem-convert: open the database: %v\n", err)
		return 2
	}
	defer database.Pool.Close()
	report, err := convertPremortem(ctx, database, *issue, *artifact, entries, *dryRun)
	var refusedMap *api.PremortemMapError
	var refusedDocument *api.PremortemDocumentError
	switch {
	case errors.As(err, &refusedMap) && refusedMap.Entry == 0:
		fmt.Fprintf(errOut, "premortem-convert: %s: risk %d has no line: %s\n", *mapFile, refusedMap.Risk, refusedMap.Problem)
		return 1
	case errors.As(err, &refusedMap):
		fmt.Fprintf(errOut, "premortem-convert: %s:%d (risk %d): %s\n", *mapFile, lines[refusedMap.Entry-1], refusedMap.Risk, refusedMap.Problem)
		return 1
	case errors.As(err, &refusedDocument):
		fmt.Fprintf(errOut, "premortem-convert: %v\n", refusedDocument)
		return 1
	case err != nil:
		fmt.Fprintf(errOut, "premortem-convert: %v\n", err)
		return 2
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(errOut, "premortem-convert: encode the report: %v\n", err)
		return 2
	}
	fmt.Fprintf(out, "%s\n", encoded)
	return 0
}

// convertPremortem runs api.ConvertPremortem on database in one transaction of its own, as the
// server's own credential, and commits it: what the command runs once its map file is read.
func convertPremortem(ctx context.Context, database *store.Store, issue, artifact string, entries []api.PremortemEntry, dryRun bool) (api.PremortemReport, error) {
	var report api.PremortemReport
	err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		var err error
		report, _, err = api.ConvertPremortem(ctx, tx, api.PremortemConverter, api.PremortemConverter, issue, artifact, entries, dryRun)
		return err
	})
	return report, err
}

// parsePreventedBy reads the map file the usage gives: one entry for each line that is neither
// blank nor opens with #, with the number of the line each entry came from, from 1. A line that is
// not three fields separated by one tab each - a number of digits, none or check addresses
// separated by one space each, and a reason that is not blank - and text that is not UTF-8 are
// refused, the error naming the line as `<line>: <why>`. Which numbers and checks the document
// takes is api.ConvertPremortem's to hold.
func parsePreventedBy(text string) ([]api.PremortemEntry, []int, error) {
	var entries []api.PremortemEntry
	var lines []int
	for index, line := range strings.Split(text, "\n") {
		number := index + 1
		line = strings.TrimSuffix(line, "\r")
		if !utf8.ValidString(line) {
			return nil, nil, fmt.Errorf("%d: the line is not UTF-8", number)
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			return nil, nil, fmt.Errorf("%d: the line holds %d fields, and a line is three separated by one tab each: the risk's number, none or its check addresses, and the reason", number, len(fields))
		}
		if fields[0] == "" || strings.Trim(fields[0], "0123456789") != "" || (len(fields[0]) > 1 && fields[0][0] == '0') {
			return nil, nil, fmt.Errorf("%d: the first field %q is no risk number: a number of digits, 0 for risk 0", number, fields[0])
		}
		risk, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, nil, fmt.Errorf("%d: the first field %q is no risk number: %v", number, fields[0], err)
		}
		checks := []string{}
		if fields[1] != "none" {
			checks = strings.Split(fields[1], " ")
			for _, check := range checks {
				if check == "" || check == "none" {
					return nil, nil, fmt.Errorf("%d: the second field %q is neither none nor check addresses separated by one space each", number, fields[1])
				}
			}
		}
		if strings.TrimSpace(fields[2]) == "" {
			return nil, nil, fmt.Errorf("%d: the reason is blank: the third field says why the checks prevent the risk, or why none does", number)
		}
		entries = append(entries, api.PremortemEntry{Risk: risk, Checks: checks, Reason: fields[2]})
		lines = append(lines, number)
	}
	return entries, lines, nil
}

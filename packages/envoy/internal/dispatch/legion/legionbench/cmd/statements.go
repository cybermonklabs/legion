package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The board's reads of the stored facts as the server ran them, with the arguments each was bound
// with, taken from Postgres's statement log (log_min_duration_statement 0, in jsonlog), so the
// facts reads' plans and times are measured on the statements a request of the board ran, with
// the board's keys and roots, as TestTheBoardsFactsReadUnderAGenericPlanStaysWithinItsBudget takes
// them from its traced pool. It is evals/evalbench's capture of #3976 (measure/capture.go),
// narrowed to two statements.

// factsRead is one of the board's reads of the stored facts as the statement log holds it: its
// text and its parameters' literals, in order.
type factsRead struct {
	SQL    string   `json:"sql"`
	Params []string `json:"params"`
}

// factsReads is the board's two reads of the stored facts: of the project's open rows
// (legionfacts.ReadOpen), and of the closed rows of the trees and the cards it shows
// (legionfacts.ReadShownClosed).
type factsReads struct {
	Open   factsRead `json:"open"`
	Closed factsRead `json:"closed"`
}

// openFactsReadStatement and closedFactsReadStatement match the board's two reads of the stored
// facts: the board's columns (growth the last of them) of the open rows alone, which the facts
// consumer's own read of the open rows (its issue keys alone) does not match, and of the closed
// rows of the trees by root and of the cards by key, the two arms of issue_facts keyed on
// closed_at.
var (
	openFactsReadStatement   = regexp.MustCompile(`(?s)\bgrowth\s+from issue_facts\s+where project_key = \$1 and closed_at is null\s*$`)
	closedFactsReadStatement = regexp.MustCompile(`(?s)\bgrowth\s+from issue_facts\s+where project_key = \$1 and root_key = any\(\$3\) and closed_at is not null\b.*\bunion all\b`)
)

// executed reads a duration line's statement: an extended-protocol execute or a simple query.
var executed = regexp.MustCompile(`(?s)^duration: [0-9.]+ ms  (?:execute [^:]*|statement): (.*)$`)

// setStatementLog turns Postgres's log of every statement, with its parameters, on or off. The
// role must be a superuser.
func setStatementLog(ctx context.Context, pool *store.Pool, on bool) error {
	change := `alter system reset log_min_duration_statement`
	if on {
		change = `alter system set log_min_duration_statement = 0`
	}
	if _, err := pool.Exec(ctx, change); err != nil {
		return fmt.Errorf("set the statement log (the role must be a superuser): %w", err)
	}
	if _, err := pool.Exec(ctx, `select pg_reload_conf()`); err != nil {
		return fmt.Errorf("reload Postgres's configuration: %w", err)
	}
	return nil
}

// captureFactsReads turns the statement log on, has read make the board's request, turns the log
// off, and answers the two facts reads the log holds from that request, waiting up to a minute for
// the logging collector to write them.
func captureFactsReads(ctx context.Context, pool *store.Pool, pgLog string, read func() error) (factsReads, error) {
	info, err := os.Stat(pgLog)
	if err != nil {
		return factsReads{}, fmt.Errorf("read the statement log's size: %w", err)
	}
	offset := info.Size()
	if err := setStatementLog(ctx, pool, true); err != nil {
		return factsReads{}, err
	}
	// The reload reaches each backend at its next command; a moment lets the server's pooled
	// connections take it before the request.
	time.Sleep(time.Second)
	readErr := read()
	if err := setStatementLog(ctx, pool, false); err != nil {
		return factsReads{}, err
	}
	if readErr != nil {
		return factsReads{}, readErr
	}
	deadline := time.Now().Add(time.Minute)
	for {
		found, err := readFactsReads(pgLog, offset)
		if err != nil || found != nil {
			if found == nil {
				return factsReads{}, err
			}
			return *found, err
		}
		if time.Now().After(deadline) {
			return factsReads{}, fmt.Errorf("the statement log %s holds no pair of the board's reads of issue_facts a minute after its request", pgLog)
		}
		time.Sleep(time.Second)
	}
}

// readFactsReads answers the last of each of the board's two reads of the stored facts the
// jsonlog file at path holds after offset, nil where it holds no pair of them.
func readFactsReads(path string, offset int64) (*factsReads, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open the statement log: %w", err)
	}
	defer file.Close()
	if _, err := file.Seek(offset, 0); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	var open, closed *factsRead
	for scanner.Scan() {
		var line struct {
			Message string `json:"message"`
			Detail  string `json:"detail"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) != nil {
			continue
		}
		match := executed.FindStringSubmatch(line.Message)
		if match == nil {
			continue
		}
		into := &open
		switch {
		case openFactsReadStatement.MatchString(match[1]):
		case closedFactsReadStatement.MatchString(match[1]):
			into = &closed
		default:
			continue
		}
		params, err := parseParameters(line.Detail)
		if err != nil {
			return nil, fmt.Errorf("read the parameters of the board's facts read: %w", err)
		}
		*into = &factsRead{SQL: match[1], Params: params}
	}
	if err := scanner.Err(); err != nil || open == nil || closed == nil {
		return nil, err
	}
	return &factsReads{Open: *open, Closed: *closed}, nil
}

// parseParameters reads a statement log's detail, `Parameters: $1 = '…', $2 = NULL`, into the
// parameters' literals in order, each as SQL writes it: quoted, or NULL.
func parseParameters(detail string) ([]string, error) {
	rest, found := strings.CutPrefix(detail, "Parameters: ")
	if !found {
		return nil, nil
	}
	var params []string
	for index := 1; rest != ""; index++ {
		head := "$" + strconv.Itoa(index) + " = "
		if !strings.HasPrefix(rest, head) {
			return nil, fmt.Errorf("parameter %d does not start %q", index, head)
		}
		rest = rest[len(head):]
		var literal string
		switch {
		case strings.HasPrefix(rest, "NULL"):
			literal, rest = "NULL", rest[len("NULL"):]
		case strings.HasPrefix(rest, "'"):
			end := 1
			for {
				next := strings.IndexByte(rest[end:], '\'')
				if next < 0 {
					return nil, fmt.Errorf("parameter %d has no closing quote", index)
				}
				end += next + 1
				if end < len(rest) && rest[end] == '\'' {
					end++
					continue
				}
				break
			}
			literal, rest = rest[:end], rest[end:]
		default:
			return nil, fmt.Errorf("parameter %d is neither quoted nor NULL", index)
		}
		params = append(params, literal)
		rest = strings.TrimPrefix(rest, ", ")
	}
	return params, nil
}

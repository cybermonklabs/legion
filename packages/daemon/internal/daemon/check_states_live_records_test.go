//go:build e2e

package daemon

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// statesStored is one row of check_results, ci_check_results or one of their retraction tables:
// the issue and check of a result (empty for a retraction, which names its result alone), its role
// and verdict, the session it was written under and the actor Dispatch took it from.
type statesStored struct {
	table, id, issue, check, role, verdict, commit, session, actorKind, actorID string
}

func (s statesStored) String() string {
	if s.issue == "" {
		return fmt.Sprintf("retraction of %s by %s %s (session %q)", s.id, s.actorKind, s.actorID, s.session)
	}
	return fmt.Sprintf("%s: %s %s of %s at %s as %s by %s %s (session %q)", s.id, s.check, s.verdict, s.issue, s.commit, s.role, s.actorKind, s.actorID, s.session)
}

// storedResults is every row of table in Dispatch's database, or of those where holds: a condition
// on the table's own columns, with args.
func (r *statesProof) storedResults(table, where string, args ...any) []statesStored {
	r.t.Helper()
	var query string
	switch table {
	case "check_results":
		query = `select id::text, issue_key, check_block_id, role, verdict, commit_sha, session, actor->>'kind', actor->>'id' from check_results`
	case "ci_check_results":
		query = `select id::text, issue_key, check_block_id, 'ci', verdict, commit_sha, session, actor->>'kind', actor->>'id' from ci_check_results`
	default:
		query = `select result_id::text, '', '', '', '', '', session, actor->>'kind', actor->>'id' from ` + table
	}
	if where != "" {
		query += ` where ` + where
	}
	rows, err := r.dispatchDB.Query(context.Background(), query+` order by 1`, args...)
	if err != nil {
		r.fail("results", "read Dispatch's %s: %v", table, err)
	}
	defer rows.Close()
	var stored []statesStored
	for rows.Next() {
		row := statesStored{table: table}
		var kind, actor *string
		if err := rows.Scan(&row.id, &row.issue, &row.check, &row.role, &row.verdict, &row.commit, &row.session, &kind, &actor); err != nil {
			r.fail("results", "read a row of Dispatch's %s: %v", table, err)
		}
		if kind != nil {
			row.actorKind = *kind
		}
		if actor != nil {
			row.actorID = *actor
		}
		stored = append(stored, row)
	}
	if err := rows.Err(); err != nil {
		r.fail("results", "read Dispatch's %s: %v", table, err)
	}
	return stored
}

// statesPublished is one row of Dispatch's check_states, the newest state Dispatch published of a
// check at a head, or, where check is empty, the head's row of check_state_sets, its set of
// checks. document is the Verification document the row was computed from, empty where the issue
// had none.
type statesPublished struct {
	check, head, state, reason, document string
	checks                               []string
}

func (p statesPublished) String() string {
	if p.check == "" {
		return fmt.Sprintf("the set at %s: [%s] (document %q)", p.head, strings.Join(p.checks, ","), p.document)
	}
	return fmt.Sprintf("%s at %s: %s (%s)", p.check, p.head, p.state, p.reason)
}

// publishedStates is each state Dispatch has published of issue's checks, as its check_states
// table holds the newest, and the set of checks of each head, as its check_state_sets holds it.
func (r *statesProof) publishedStates(issue *statesIssue) []statesPublished {
	r.t.Helper()
	rows, err := r.dispatchDB.Query(context.Background(), `
		select check_block_id, head_sha, state, reason, check_artifact_id::text, '{}'::text[]
		from check_states where issue_key = $1
		union all
		select '', head_sha, '', '', coalesce(check_artifact_id::text, ''), checks
		from check_state_sets where issue_key = $1
		order by 2, 1`, issue.key)
	if err != nil {
		r.fail("published-states", "read Dispatch's check_states of %s: %v", issue.key, err)
	}
	defer rows.Close()
	var states []statesPublished
	for rows.Next() {
		var state statesPublished
		if err := rows.Scan(&state.check, &state.head, &state.state, &state.reason, &state.document, &state.checks); err != nil {
			r.fail("published-states", "read a state of %s: %v", issue.key, err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		r.fail("published-states", "read Dispatch's check_states of %s: %v", issue.key, err)
	}
	if len(states) == 0 {
		r.fail("published-states", "UNKNOWN: Dispatch's check_states holds no state of %s, whose move the states decided", issue.key)
	}
	return states
}

// resultWrites is each request Dispatch's access log holds (accesslog.Message) that records or
// retracts a result, its fields by name.
func (r *statesProof) resultWrites() []map[string]string {
	r.t.Helper()
	routes := regexp.MustCompile(`^/api/v1/(issues/[^/]+/results|projects/[^/]+/results|results/[^/]+/retract)$`)
	var writes []map[string]string
	scanner := bufio.NewScanner(strings.NewReader(r.dispatch.output.String()))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if !strings.Contains(scanner.Text(), `msg="dispatch: request"`) {
			continue
		}
		fields := logfmtFields(scanner.Text())
		if fields["method"] == http.MethodPost && routes.MatchString(fields["path"]) {
			writes = append(writes, fields)
		}
	}
	if err := scanner.Err(); err != nil {
		r.fail("access-log", "read Dispatch's access log: %v", err)
	}
	return writes
}

// logfmtFields is one line of slog's text handler, which cmd/dispatch logs through, as its fields
// by key: a value is bare, or quoted as strconv.Quote quotes it.
func logfmtFields(line string) map[string]string {
	fields := map[string]string{}
	rest := strings.TrimSpace(line)
	for rest != "" {
		key, after, found := strings.Cut(rest, "=")
		if !found || strings.ContainsAny(key, " \"") {
			break
		}
		var value string
		if strings.HasPrefix(after, `"`) {
			quoted, err := strconv.QuotedPrefix(after)
			if err != nil {
				break
			}
			value, _ = strconv.Unquote(quoted)
			rest = strings.TrimLeft(after[len(quoted):], " ")
		} else {
			value, rest, _ = strings.Cut(after, " ")
			rest = strings.TrimLeft(rest, " ")
		}
		fields[key] = value
	}
	return fields
}

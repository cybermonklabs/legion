package asof_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The reads at a cut each have an index of their own on events (decision 15 of the plan, each
// measured on a copy of production's event log): the issue listing reads each
// issue's newest state through events_issue_state, its claim through events_issue_claim and when
// it last changed through events_issue_activity; the asks read each ask's newest state through
// events_ask_state and its follows through events_ask_follower; the comments and the messages
// read their own events through events_comment_state and events_message_state; the documents read
// their renames through events_artifact_renamed; the projects read their settings through
// events_project_setting; the repository mappings read their events through events_repo_project;
// and the bound of an event log's page reads the ids at the cut through events_created_at. A
// partial index applies only when the planner can prove that the read's condition implies the
// index's predicate. So a shadow whose
// type list or expression drifts from its index still answers right, but reads each event of each
// issue or ask again: on the copy, the issue listing took 1,215 ms without events_issue_state and
// 33 ms with it, and an issue's asks with their threads 19 s without events_comment_state. A bound
// written as `order by id desc limit 1` reads each event after the cut. The plan of each read,
// bound to a cut amid the seeded events, is what this test holds.
func TestTheShadowsUseTheirIndexes(t *testing.T) {
	w := newWorld(t)
	seedIndexedReads(t, w)
	cut := mustCut(t, "2026-11-01T01:40:00Z")
	keys := explaintest.NewIndexKeys(w.database.Pool)

	for _, read := range []struct {
		name, statement string
		arguments       []any
		index           string
		// whole is a read that may read all of its index: an index partial on the one type the
		// read names, of a few dozen events, which the read reads whole beside its keyed reads.
		whole bool
	}{
		{
			// The listing's own filters and order (api/issue_queries.go, issueListFilters and
			// issueListOrder), reduced to the ones this seed decides.
			name: "the issue listing",
			statement: `select i.key, i.status, i.rank from issues i
				where ($1 = '' or i.project_key = $1) and (not $2::boolean or i.closed_at is null)
				order by i.rank asc, i.created_at asc, i.key asc`,
			arguments: []any{"IDX", false},
			index:     "events_issue_state",
		},
		{
			// Every open ask of every project, as the inbox and the open asks read them.
			name:      "the open asks",
			statement: `select a.id, a.issue_key, a.question from asks a where a.state = 'open' order by a.created_at, a.id`,
			index:     "events_ask_state",
		},
		{
			name:      "the claim of each issue",
			statement: `select i.key, i.claimed_by from issues i where ($1 = '' or i.project_key = $1) order by i.key`,
			arguments: []any{"IDX"},
			index:     "events_issue_claim",
		},
		{
			name:      "when each issue last changed",
			statement: `select i.key, i.updated_at from issues i where ($1 = '' or i.project_key = $1) order by i.key`,
			arguments: []any{"IDX"},
			index:     "events_issue_activity",
		},
		{
			name:      "the comments of an issue",
			statement: `select c.id, c.body, c.resolved from comments c where c.issue_key = $1 order by c.created_at`,
			arguments: []any{"IDX-7"},
			index:     "events_comment_state",
		},
		{
			name:      "the messages of an issue",
			statement: `select m.id, m.body, m.edited_at from messages m where m.issue_key = $1 order by m.created_at`,
			arguments: []any{"IDX-7"},
			index:     "events_message_state",
		},
		{
			name:      "the followers of an ask",
			statement: `select f.session_id, f.since from ask_followers f where f.ask_id = md5('ask-7')::uuid`,
			index:     "events_ask_follower",
		},
		{
			name:      "the names of an issue's documents",
			statement: `select a.id, a.name from artifacts a where a.issue_key = $1 order by a.slug`,
			arguments: []any{"IDX-7"},
			index:     "events_artifact_renamed",
		},
		{
			name:      "the repository mappings",
			statement: `select r.repo, r.project from repo_projects r`,
			index:     "events_repo_project",
			whole:     true,
		},
		{
			name:      "the default assignee of each project",
			statement: `select p.key, p.default_assignee, p.gated_steps from projects p order by p.key`,
			index:     "events_project_setting",
			whole:     true,
		},
		{
			name:      "the bound of an event log's page",
			statement: asof.NewestEventIDStatement,
			index:     "events_created_at",
		},
		{
			// Which posts and asks of a thread changed after the cut (api/post_as_of.go): each
			// kind of item through the index of its own types.
			name:      "the comments changed after the cut",
			statement: asof.ItemsChangedAfterStatement,
			arguments: []any{[]string{"comment-7", "message-7", "ask-7"}, cut.Time()},
			index:     "events_comment_state",
		},
		{
			name:      "the messages changed after the cut",
			statement: asof.ItemsChangedAfterStatement,
			arguments: []any{[]string{"comment-7", "message-7", "ask-7"}, cut.Time()},
			index:     "events_message_state",
		},
		{
			name:      "the asks changed after the cut",
			statement: asof.ItemsChangedAfterStatement,
			arguments: []any{[]string{"comment-7", "message-7", "ask-7"}, cut.Time()},
			index:     "events_ask_state",
		},
	} {
		t.Run(read.name, func(t *testing.T) {
			bound, err := asof.Bind(read.statement, cut)
			if err != nil {
				t.Fatalf("Bind(%q): %v", read.statement, err)
			}
			plan := planWithoutSequentialScans(t, w, bound, read.arguments...)
			rule := explaintest.CondRequired
			if read.whole {
				rule = explaintest.CondOptional
			}
			keys.RequireIndexKeyed(t, plan, read.index, read.name+" at a cut", rule)
		})
	}
}

// Each partial index that a shadow reads names, in its predicate, the event types that its shadow
// reads, in its order. A migration writes the list by hand, since a migration is SQL that never
// changes once applied: a type the shadow reads later would leave the index unused, and a type
// the shadow no longer reads would keep its events in the index for nothing. The activity index
// names the whole-issue types as `type not in`, which its read names the same way.
func TestEachEventIndexNamesTheTypesItsShadowReads(t *testing.T) {
	for _, index := range []struct {
		name  string
		types []string
	}{
		{"events_issue_claim", asof.IssueClaimTypes()},
		{"events_issue_state", asof.IssueStateTypes()},
		{"events_ask_state", asof.AskStateTypes()},
		{"events_issue_activity", asof.IssueStateTypes()},
		{"events_project_setting", asof.ProjectSettingTypes()},
		{"events_comment_state", asof.CommentStateTypes()},
		{"events_message_state", asof.MessageStateTypes()},
		{"events_ask_follower", asof.AskFollowerTypes()},
	} {
		if types := indexedTypes(t, index.name); !slices.Equal(types, index.types) {
			t.Errorf("%s indexes the types %v, want its shadow's %v", index.name, types, index.types)
		}
	}
}

// indexedTypes are the types that the predicate `where type in (...)`, or `where type not in
// (...)`, of the one migration that builds index names, in its order.
func indexedTypes(t *testing.T, index string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "store", "migrations", "*_"+index+".up.sql"))
	if err != nil || len(files) != 1 {
		t.Fatalf("the migrations of %s: %v, %v; want one file", index, files, err)
	}
	content, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var statement []string
	for _, line := range strings.Split(string(content), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			statement = append(statement, line)
		}
	}
	predicate := regexp.MustCompile(`(?s)\bwhere\s+type\s+(?:not\s+)?in\s+\(([^)]*)\)`).FindStringSubmatch(strings.Join(statement, "\n"))
	if predicate == nil {
		t.Fatalf("%s holds no `where type in (...)`:\n%s", files[0], content)
	}
	var types []string
	for _, literal := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(predicate[1], -1) {
		types = append(types, literal[1])
	}
	return types
}

// seedIndexedReads writes 400 issues of project IDX, each with one ask, a message, 18 comments
// and a document, and the events their writers append: per issue an issue.created, a
// message.created, 18 comment.created, an issue.claimed, an issue.updated, an ask.opened, an
// ask.answered and an ask.follower_added, one a second from n × 30 seconds after
// 2026-11-01T00:00Z for issue n; and one mapping of a repository to IDX. So most of an issue's
// events are not the ones its state is read from, as in production, and the events run to 03:20,
// past the test's cut. It vacuums and analyzes the tables, so the planner reads these counts.
func seedIndexedReads(t *testing.T, w world) {
	t.Helper()
	w.exec(`insert into projects (key, name, default_assignee, created_at) values ('IDX', 'IDX', 'carol', '2026-11-01T00:00:00Z')`)
	w.exec(`
		insert into issues (key, project_key, number, title, rank, created_by, created_at, updated_at)
		select 'IDX-' || n, 'IDX', n, 'Issue ' || n, lpad(n::text, 4, '0'), '{"kind":"user","id":"alice"}',
			timestamptz '2026-11-01T00:00:00Z' + make_interval(secs => n * 30),
			timestamptz '2026-11-01T00:00:00Z' + make_interval(secs => n * 30)
		from generate_series(1, 400) n`)
	w.exec(`
		insert into asks (id, issue_key, author, question, options, created_at)
		select md5('ask-' || n)::uuid, 'IDX-' || n, '{"kind":"user","id":"alice"}', 'Ship it?', '[]',
			timestamptz '2026-11-01T00:00:00Z' + make_interval(secs => n * 30 + 23)
		from generate_series(1, 400) n`)
	w.exec(`
		insert into messages (id, issue_key, author, body, created_at)
		select md5('message-' || n)::uuid, 'IDX-' || n, '{"kind":"user","id":"alice"}', 'Hello',
			timestamptz '2026-11-01T00:00:00Z' + make_interval(secs => n * 30 + 2)
		from generate_series(1, 400) n`)
	w.exec(`
		insert into artifacts (id, issue_key, project_key, slug, name, kind, created_by, created_at)
		select md5('notes-' || n)::uuid, 'IDX-' || n, 'IDX', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"alice"}',
			timestamptz '2026-11-01T00:00:00Z' + make_interval(secs => n * 30 + 1)
		from generate_series(1, 400) n`)
	w.exec(`
		insert into comments (id, issue_key, author, body, created_at)
		select md5('comment-' || n || '-' || s)::uuid, 'IDX-' || n, '{"kind":"user","id":"alice"}', 'Looks right',
			timestamptz '2026-11-01T00:00:00Z' + make_interval(secs => n * 30 + s)
		from generate_series(1, 400) n, generate_series(3, 20) s`)
	w.exec(`
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		select 'IDX-' || n, s,
			case s when 1 then 'issue.created' when 2 then 'message.created' when 21 then 'issue.claimed'
				when 22 then 'issue.updated' when 23 then 'ask.opened' when 24 then 'ask.answered'
				when 25 then 'ask.follower_added' else 'comment.created' end,
			'{"kind":"user","id":"alice"}',
			case when s = 2 then jsonb_build_object('id', md5('message-' || n)::uuid)
				when s between 3 and 20 then jsonb_build_object('id', md5('comment-' || n || '-' || s)::uuid)
				when s = 21 then jsonb_build_object('claim', jsonb_build_object('actor', '{"kind":"user","id":"alice"}'::jsonb, 'at', '2026-11-01T00:00:00Z'))
				when s in (23, 24) then jsonb_build_object('id', md5('ask-' || n)::uuid)
				when s = 25 then jsonb_build_object('ask_id', md5('ask-' || n)::uuid, 'session_id', 'session-' || n)
				else '{}'::jsonb end,
			false,
			timestamptz '2026-11-01T00:00:00Z' + make_interval(secs => n * 30 + s)
		from generate_series(1, 400) n, generate_series(1, 25) s`)
	w.exec(`
		insert into events (project_key, seq, type, actor, payload, notify, created_at)
		values ('IDX', 1, 'settings.repo_project.updated', '{"kind":"user","id":"alice"}',
			'{"mapping": {"repo": "example/repo", "project": "IDX"}}', false, '2026-11-01T00:00:30Z')`)
	w.exec(`update issues set last_seq = 25`)
	w.exec(`update projects set last_seq = 1`)
	w.exec(`vacuum analyze events, issues, asks, comments, messages, artifacts`)
}

// planWithoutSequentialScans is the plan PostgreSQL makes for statement in a transaction of its
// own with sequential scans off. The seed is far smaller than production's event log, and a
// table this small is read fastest whole however a statement is written; with sequential scans
// off, the plan shows which index the statement's shape lets the planner read.
func planWithoutSequentialScans(t *testing.T, w world, statement string, arguments ...any) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	tx, err := w.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+statement, arguments...).Scan(&raw); err != nil {
		t.Fatalf("explain %q: %v", statement, err)
	}
	return explaintest.OnePlan(t, raw)
}

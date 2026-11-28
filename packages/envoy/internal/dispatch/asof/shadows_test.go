package asof_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
)

// shadowedTables are the tables the plan's decision 2 gives a shadow, events among them.
var shadowedTables = []string{
	"events", "issues", "asks", "comments", "messages", "artifacts", "artifact_versions",
	"artifact_reviews", "check_results", "check_result_retractions", "issue_links",
	"issue_external_links", "issue_components", "priority_tuples", "projects", "repo_projects",
	"ask_followers", "comment_deliveries", "message_deliveries",
}

// column is one column by its name, its type, as pg_type names it (int4, _text, timestamptz),
// and its collation, as information_schema names it: empty for a type that has none and for the
// database's default collation.
type column struct{ name, kind, collation string }

// A route at a cut runs its present statements, which name each column of a table, select * from
// it, or scan its columns by position. So each shadow answers every column of its table, by name,
// type and collation and in the table's order: a column a migration adds later fails here by name
// until its shadow answers it, and a text column read from a payload takes the database's default
// collation where the table names another (issues.rank is "C", so that it orders as its digits'
// bytes), so a statement that orders or compares it at a cut answers in another order. Each
// shadow also runs, bound in front of a statement at a cut of now, over a database with a row in
// every shadowed table written as the writers write them, and at that cut answers each table's
// rows.
func TestEveryShadowAnswersEveryColumnOfItsTable(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	shadowed := asof.ShadowedTables()
	if missing, extra := difference(shadowedTables, shadowed); len(missing)+len(extra) > 0 {
		t.Fatalf("the shadows hold %v: decision 2's tables %v have none, and %v are not decision 2's", shadowed, missing, extra)
	}

	cut := mustCut(t, "2026-11-10T00:00:00Z")
	for _, table := range shadowed {
		t.Run("columns of "+table, func(t *testing.T) {
			want := catalogColumns(t, w.database.Pool, "public", table)
			if len(want) == 0 {
				t.Fatalf("information_schema holds no column of public.%s", table)
			}
			got := shadowColumns(t, w, cut, table)
			for _, problem := range compareColumns(want, got) {
				t.Errorf("the %s shadow: %s", table, problem)
			}
		})
	}

	seedEveryShadowedTable(t, w)
	var now string
	if err := w.database.Pool.QueryRow(ctx, `select to_char(clock_timestamp() at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`).Scan(&now); err != nil {
		t.Fatalf("read the database's now: %v", err)
	}
	atNow := atCut(t, now)
	for _, table := range shadowed {
		t.Run("rows of "+table+" at a cut of now", func(t *testing.T) {
			var stored int
			if err := w.database.Pool.QueryRow(ctx, "select count(*) from "+table).Scan(&stored); err != nil {
				t.Fatalf("count the rows of %s: %v", table, err)
			}
			if stored == 0 {
				t.Fatalf("the seed wrote no row of %s", table)
			}
			// Every column of every row is read, so each expression of the shadow runs.
			rows, err := w.database.Pool.Query(atNow, "select * from "+table)
			if err != nil {
				t.Fatalf("select * from %s at a cut of now: %v", table, err)
			}
			answered := 0
			for rows.Next() {
				if _, err := rows.Values(); err != nil {
					t.Fatalf("read a row of %s at a cut of now: %v", table, err)
				}
				answered++
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("select * from %s at a cut of now: %v", table, err)
			}
			if answered != stored {
				t.Errorf("%s at a cut of now answers %d rows, want the %d it holds", table, answered, stored)
			}
		})
	}
}

// catalogQuerier is what catalogColumns reads the catalog through: the pool, or the transaction
// that holds a temporary view.
type catalogQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// catalogColumns are the columns of the relation schema.relation, as information_schema names
// them, in their order.
func catalogColumns(t *testing.T, catalog catalogQuerier, schema, relation string) []column {
	t.Helper()
	rows, err := catalog.Query(context.Background(), `
		select column_name, udt_name, coalesce(collation_name, '') as collation_name from information_schema.columns
		where table_schema = $1 and table_name = $2 order by ordinal_position`, schema, relation)
	read := scanAll(t, rows, err)
	columns := make([]column, len(read))
	for index, row := range read {
		columns[index] = column{name: row["column_name"].(string), kind: row["udt_name"].(string), collation: row["collation_name"].(string)}
	}
	return columns
}

// shadowColumns are the columns the shadow of table answers: the columns of a temporary view of a
// statement that reads it through the WITH list Shadows writes for it. PostgreSQL gives a view's
// column the type and the collation of its expression, which the catalog names as it names a
// table's, so the shadow's columns are read as the table's are. The view lives in a transaction
// that the read rolls back.
func shadowColumns(t *testing.T, w world, cut asof.Cut, table string) []column {
	t.Helper()
	ctx := context.Background()
	tx, err := w.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read of the %s shadow: %v", table, err)
	}
	defer tx.Rollback(ctx)
	const view = "shadow_columns"
	if _, err := tx.Exec(ctx, "create temporary view "+view+" as with "+asof.Shadows(cut, table)+"\nselect * from "+table); err != nil {
		t.Fatalf("read the %s shadow: %v", table, err)
	}
	var schema string
	if err := tx.QueryRow(ctx, `select pg_my_temp_schema()::regnamespace::text`).Scan(&schema); err != nil {
		t.Fatalf("name the temporary schema: %v", err)
	}
	return catalogColumns(t, tx, schema, view)
}

// compareColumns names each way got is not want: a column it lacks, one of another type or
// another collation, one the table does not have, and any other order.
func compareColumns(want, got []column) []string {
	var problems []string
	answered := map[string]column{}
	for _, column := range got {
		answered[column.name] = column
	}
	stored := map[string]bool{}
	for _, column := range want {
		stored[column.name] = true
		shadow, ok := answered[column.name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("no column %s (%s)", column.name, column.kind))
		case shadow.kind != column.kind:
			problems = append(problems, fmt.Sprintf("column %s is %s, want %s", column.name, shadow.kind, column.kind))
		case shadow.collation != column.collation:
			problems = append(problems, fmt.Sprintf("column %s has the collation %q, want %q", column.name, shadow.collation, column.collation))
		}
	}
	for _, column := range got {
		if !stored[column.name] {
			problems = append(problems, fmt.Sprintf("column %s (%s) is no column of the table", column.name, column.kind))
		}
	}
	if len(problems) == 0 && !slices.Equal(want, got) {
		problems = append(problems, fmt.Sprintf("columns in the order %v, want the table's %v", got, want))
	}
	return problems
}

// difference is what want holds that got lacks, and what got holds that want lacks.
func difference(want, got []string) (missing, extra []string) {
	for _, name := range want {
		if !slices.Contains(got, name) {
			missing = append(missing, name)
		}
	}
	for _, name := range got {
		if !slices.Contains(want, name) {
			extra = append(extra, name)
		}
	}
	return missing, extra
}

// seedEveryShadowedTable writes a row of every shadowed table, each with the events its writer
// appends beside it, so a shadow at a cut of now answers each row the table holds.
func seedEveryShadowedTable(t *testing.T, w world) {
	t.Helper()
	ctx := context.Background()
	made := when(t, "2026-11-10T08:00:00Z")
	w.project("ASOF", "carol", made.Add(-time.Hour))
	w.exec(`insert into repo_projects (repo, project, created_by, is_default, created_at) values ('acme/asof', 'ASOF', $1, true, $2)`,
		w.encode(alice), made)
	w.event(projectOwned("ASOF"), "settings.repo_project.updated", alice, map[string]any{
		"deleted": false,
		"mapping": model.RepoProject{Repo: "acme/asof", Project: "ASOF", Default: true, CreatedBy: alice, CreatedAt: made},
	}, made)

	blocker := newIssue("ASOF-2", "ASOF", 2, "The blocker", made)
	w.insertIssue(blocker)
	w.issueEvent("issue.created", blocker, made)

	reason := "a spike"
	issue := newIssue("ASOF-1", "ASOF", 1, "Every table", made)
	issue.BlockedBy = []string{blocker.Key}
	issue.ExternalLinks = []model.ExternalLink{{URL: "https://example.com/asof/1", Kind: "url"}}
	issue.Components = model.IssueComponents{Mode: "none", IDs: []string{}, Unknown: []string{}, Reason: &reason}
	issue.Claim = &model.IssueClaim{Actor: sessionActor("s-1"), At: made.Add(time.Minute)}
	w.insertIssue(issue)
	w.exec(`insert into issue_links (issue_key, kind, target_key, created_at) values ('ASOF-1', 'blocked_by', 'ASOF-2', $1)`, made)
	w.exec(`insert into issue_external_links (issue_key, url, kind) values ('ASOF-1', 'https://example.com/asof/1', 'url')`)
	w.exec(`insert into issue_components (issue_key, mode, reason, created_at, updated_at) values ('ASOF-1', 'none', $1, $2, $2)`, reason, made)
	unclaimed := issue
	unclaimed.Claim = nil
	w.issueEvent("issue.created", unclaimed, made)
	w.event(issueOwned("ASOF-1"), "issue.claimed", sessionActor("s-1"), model.IssueClaimEventPayload{
		Key: "ASOF-1", Status: model.IssueStatusTriage, Claim: issue.Claim, Reason: "claimed",
	}, made.Add(time.Minute))

	tx, err := w.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := prioritystore.Advance(ctx, tx, "ASOF-1", "ship", alice, made); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the advance: %v", err)
	}

	var spec string
	if err := w.database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, created_at)
		values ('ASOF-1', 'ASOF', 'spec', 'spec.md', 'doc', true, $1, $2) returning id::text`, w.encode(alice), made).Scan(&spec); err != nil {
		t.Fatalf("insert the spec: %v", err)
	}
	w.exec(`insert into artifact_versions (artifact_id, number, markdown, created_at) values ($1, 1, 'The spec of every table.', $2)`, spec, made)
	w.event(issueOwned("ASOF-1"), "artifact.created", alice, map[string]any{"artifact": map[string]any{"id": spec}}, made)
	w.exec(`insert into artifact_reviews (artifact_id, version, state, actor, created_at) values ($1, 1, 'approved', $2, $3)`,
		spec, w.encode(alice), made)
	var result string
	if err := w.database.Pool.QueryRow(ctx, `
		insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, role, commit_sha, verdict, actor, created_at)
		values ('ASOF-1', 'ASOF-1', $1, 'b1', 1, 'sha', 'tester', $2, 'pass', $3, $4) returning id::text`,
		spec, strings.Repeat("a", 40), w.encode(alice), made).Scan(&result); err != nil {
		t.Fatalf("insert a check result: %v", err)
	}
	w.exec(`insert into check_result_retractions (result_id, reason, actor, created_at) values ($1, 'wrong commit', $2, $3)`,
		result, w.encode(alice), made)

	// An ask a session opened, and a reply another session wrote in its thread: each follows it
	// (asks.FollowAuthor, called by the ask's and the reply's writes).
	askID := "00000000-0000-4000-8000-0000000000a1"
	key := "ASOF-1"
	ask := model.Ask{ID: askID, IssueKey: &key, Author: sessionActor("s-1"), Kind: "question", Question: "Ship it?",
		Options: []model.AskOption{{Label: "yes"}, {Label: "no"}}, Urgency: "med", State: "open", CreatedAt: made}
	w.exec(`insert into asks (id, issue_key, author, question, options, created_at) values ($1, 'ASOF-1', $2, 'Ship it?', $3, $4)`,
		askID, w.encode(ask.Author), w.encode(ask.Options), made)
	w.event(issueOwned(key), "ask.opened", ask.Author, model.NewAskEventPayload(ask, model.ReferenceChanges{}), made)
	w.exec(`insert into ask_followers (ask_id, session_id, since) values ($1, 's-1', $2)`, askID, made)

	replyID := "00000000-0000-4000-8000-0000000000c1"
	turn := "human"
	reply := newComment(replyID, key, sessionActor("s-2"), "Yes, ship it. @session:s-3", made.Add(time.Minute))
	reply.AskID, reply.Turn = &askID, &turn
	w.exec(`insert into comments (id, issue_key, author, body, ask_id, turn, created_at) values ($1, 'ASOF-1', $2, $3, $4, 'human', $5)`,
		replyID, w.encode(reply.Author), reply.Body, askID, reply.CreatedAt.Add(time.Millisecond))
	w.event(issueOwned(key), "comment.created", reply.Author, commentPayload(reply), reply.CreatedAt)
	w.exec(`insert into ask_followers (ask_id, session_id, since) values ($1, 's-2', $2)`, askID, reply.CreatedAt)
	w.exec(`insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, envelope_id, state, created_at)
		values ($1, 'session:s-3', 1, 'btw', 's-3', 'envelope-1', 'sent', $2)`, replyID, reply.CreatedAt)
	sessionID := "s-3"
	w.event(issueOwned(key), "comment.delivery", sessionActor("s-2"), model.CommentDeliveryEventPayload{
		CommentID: replyID, AskID: &askID, Target: "session:s-3", Attempt: 1, Delivery: "btw", SessionID: &sessionID, State: "sent",
	}, reply.CreatedAt.Add(time.Second))

	messageID := "00000000-0000-4000-8000-0000000000d1"
	target := "session:s-3"
	message := model.Message{ID: messageID, IssueKey: &key, Author: alice, Body: "Look at the spec.", Target: &target,
		CreatedAt: made.Add(2 * time.Minute), Deliveries: []model.MessageDelivery{}}
	w.exec(`insert into messages (id, issue_key, author, body, target, created_at) values ($1, 'ASOF-1', $2, $3, $4, $5)`,
		messageID, w.encode(alice), message.Body, target, message.CreatedAt)
	w.event(issueOwned(key), "message.created", alice, messagePayload(message), message.CreatedAt)
	w.exec(`insert into message_deliveries (message_id, attempt, delivery, session_id, envelope_id, state, requested_by, created_at)
		values ($1, 1, 'btw', 's-3', 'envelope-2', 'sent', $2, $3)`, messageID, w.encode(alice), message.CreatedAt)
	w.event(issueOwned(key), "message.delivery", alice, model.MessageDeliveryEventPayload{
		MessageID: messageID, Attempt: 1, Delivery: "btw", SessionID: "s-3", Target: target, State: "sent",
	}, message.CreatedAt.Add(time.Second))
}

// A comment and an issue message each read at a cut with the body they held then, by the one
// clock decision 2 names, the events': the body a later edit or delete replaced is the
// post_body_history row that write kept, for a write after INFRA-1523's history began; for an
// edit before it, the body of the newest event at the cut, unless that event cut it. A post
// deleted after the cut reads deleted, with its present deleted_at and no body (decision 14), as
// the present read gives it. A body longer than an event carries reads whole at a cut through
// the history row an edit after the cut kept.
func TestTheShadowsGiveAPostsPresentDeletedAtAndItsBodyAtTheCut(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	var (
		made     = when(t, "2026-11-10T08:00:00Z")
		legacy   = when(t, "2026-11-10T09:00:00Z") // an edit before the history began
		began    = when(t, "2026-11-10T10:00:00Z") // the history began
		edited   = when(t, "2026-11-10T11:00:00Z")
		deleted  = when(t, "2026-11-10T13:00:00Z")
		long     = strings.Repeat("A body past the event bound. ", 1200) // 34,800 UTF-16 units
		key      = "ASOF-1"
		postedAt = made.Add(time.Millisecond) // comments.created_at is clock_timestamp(), later than the event
	)
	if _, truncated := model.EventBody(long); !truncated {
		t.Fatalf("a body of %d bytes rides whole in an event; the test needs one the event cuts", len(long))
	}
	w.exec(`update schema_migrations set applied_at = $1 where version = 20261110224508`, began)
	w.project("ASOF", "carol", made.Add(-time.Hour))
	issue := newIssue(key, "ASOF", 1, "Posts", made.Add(-time.Hour))
	w.insertIssue(issue)
	w.issueEvent("issue.created", issue, issue.CreatedAt)

	// comment writes a comment as it stands now and its events: created at made with body, then
	// each edit, which keeps the body it replaced (only from the history's start: an edit before
	// it kept none), then a delete when deletedAt is set.
	type edit struct {
		at   time.Time
		body string
	}
	comment := func(id, body string, edits []edit, deletedAt *time.Time) {
		current := newComment(id, key, alice, body, made)
		w.exec(`insert into comments (id, issue_key, author, body, created_at) values ($1, $2, $3, $4, $5)`,
			id, key, w.encode(alice), body, postedAt)
		w.event(issueOwned(key), "comment.created", alice, commentPayload(current), made)
		for _, change := range edits {
			if !change.at.Before(began) {
				w.keepReplacedBody("comment_id", id, current.Body, change.at)
			}
			stamp := change.at.Format(time.RFC3339Nano)
			current.Body, current.EditedAt = change.body, &stamp
			w.exec(`update comments set body = $2, edited_at = $3 where id = $1`, id, change.body, change.at)
			w.event(issueOwned(key), "comment.edited", alice, commentPayload(current), change.at)
		}
		if deletedAt != nil {
			w.keepReplacedBody("comment_id", id, current.Body, *deletedAt)
			current.Body, current.DeletedAt, current.DeletedBy = "", deletedAt, &alice
			w.exec(`update comments set body = '', suggestion = null, deleted_at = $2, deleted_by = $3 where id = $1`,
				id, *deletedAt, w.encode(alice))
			w.event(issueOwned(key), "comment.deleted", alice, commentPayload(current), *deletedAt)
		}
	}
	// message is comment for an issue message (api/messages.go, api/message_corrections.go).
	message := func(id, body string, edits []edit, deletedAt *time.Time) {
		current := model.Message{ID: id, IssueKey: &key, Author: alice, Body: body, CreatedAt: made, Deliveries: []model.MessageDelivery{}}
		w.exec(`insert into messages (id, issue_key, author, body, created_at) values ($1, $2, $3, $4, $5)`,
			id, key, w.encode(alice), body, made)
		w.event(issueOwned(key), "message.created", alice, messagePayload(current), made)
		for _, change := range edits {
			w.keepReplacedBody("message_id", id, current.Body, change.at)
			current.Body, current.EditedAt = change.body, &change.at
			w.exec(`update messages set body = $2, edited_at = $3 where id = $1`, id, change.body, change.at)
			w.event(issueOwned(key), "message.edited", alice, messagePayload(current), change.at)
		}
		if deletedAt != nil {
			w.keepReplacedBody("message_id", id, current.Body, *deletedAt)
			current.Body, current.DeletedAt, current.DeletedBy = "", deletedAt, &alice
			w.exec(`update messages set body = '', deleted_at = $2, deleted_by = $3 where id = $1`, id, *deletedAt, w.encode(alice))
			w.event(issueOwned(key), "message.deleted", alice, messagePayload(current), *deletedAt)
		}
	}
	const (
		twiceEdited     = "00000000-0000-4000-8000-0000000000c1"
		deletedLater    = "00000000-0000-4000-8000-0000000000c2"
		longEditedLater = "00000000-0000-4000-8000-0000000000c3"
		longEditedEarly = "00000000-0000-4000-8000-0000000000c4"
		postedLater     = "00000000-0000-4000-8000-0000000000c5"
		messageEdited   = "00000000-0000-4000-8000-0000000000d1"
		messageDeleted  = "00000000-0000-4000-8000-0000000000d2"
	)
	comment(twiceEdited, "first", []edit{{edited, "second"}, {deleted, "third"}}, nil)
	comment(deletedLater, "doomed words", nil, &deleted)
	comment(longEditedLater, long, []edit{{deleted, "short"}}, nil)
	comment(longEditedEarly, long, []edit{{legacy, "short now"}}, nil)
	message(messageEdited, "first", []edit{{edited, "second"}, {deleted, "third"}}, nil)
	message(messageDeleted, "doomed words", nil, &deleted)
	later := newComment(postedLater, key, alice, "written after the cut", deleted)
	w.exec(`insert into comments (id, issue_key, author, body, created_at) values ($1, $2, $3, $4, $5)`,
		postedLater, key, w.encode(alice), later.Body, deleted.Add(time.Millisecond))
	w.event(issueOwned(key), "comment.created", alice, commentPayload(later), deleted)

	type post struct {
		body      string
		editedAt  *time.Time
		deletedAt *time.Time
		deletedBy bool
	}
	read := func(t *testing.T, cut, table string) map[string]post {
		t.Helper()
		rows, err := w.database.Pool.Query(atCut(t, cut),
			"select id::text, body, edited_at, deleted_at, deleted_by from "+table+" where issue_key = $1", key)
		posts := map[string]post{}
		for _, row := range scanAll(t, rows, err) {
			read := post{body: row["body"].(string), deletedBy: row["deleted_by"] != nil}
			if at, ok := row["edited_at"].(time.Time); ok {
				read.editedAt = &at
			}
			if at, ok := row["deleted_at"].(time.Time); ok {
				read.deletedAt = &at
			}
			posts[row["id"].(string)] = read
		}
		return posts
	}
	check := func(t *testing.T, posts map[string]post, id, what string, want post) {
		t.Helper()
		got, ok := posts[id]
		if !ok {
			t.Errorf("%s: the post is not in the shadow at the cut", what)
			return
		}
		if got.body != want.body {
			t.Errorf("%s: body %q at the cut, want %q", what, abbreviate(got.body), abbreviate(want.body))
		}
		if !sameTime(got.editedAt, want.editedAt) {
			t.Errorf("%s: edited_at %v at the cut, want %v", what, got.editedAt, want.editedAt)
		}
		if !sameTime(got.deletedAt, want.deletedAt) || got.deletedBy != want.deletedBy {
			t.Errorf("%s: deleted_at %v (deleted_by set %t) at the cut, want %v (%t)", what, got.deletedAt, got.deletedBy, want.deletedAt, want.deletedBy)
		}
	}

	t.Run("a cut after the history began", func(t *testing.T) {
		const cut = "2026-11-10T12:00:00Z"
		comments := read(t, cut, "comments")
		check(t, comments, twiceEdited, "a comment edited before and after the cut", post{body: "second", editedAt: &edited})
		check(t, comments, deletedLater, "a comment deleted after the cut", post{body: "", deletedAt: &deleted, deletedBy: true})
		check(t, comments, longEditedLater, "a long comment edited after the cut", post{body: long})
		check(t, comments, longEditedEarly, "a long comment edited before the cut", post{body: "short now", editedAt: &legacy})
		if _, ok := comments[postedLater]; ok {
			t.Errorf("a comment created after the cut is in the shadow at the cut")
		}
		messages := read(t, cut, "messages")
		check(t, messages, messageEdited, "a message edited before and after the cut", post{body: "second", editedAt: &edited})
		check(t, messages, messageDeleted, "a message deleted after the cut", post{body: "", deletedAt: &deleted, deletedBy: true})

		// The search vector is the body's at the cut, so a search of the present body finds
		// nothing there.
		var second, third int
		if err := w.database.Pool.QueryRow(atCut(t, cut), `
			select count(*) filter (where search @@ plainto_tsquery('english', 'second')),
			       count(*) filter (where search @@ plainto_tsquery('english', 'third'))
			from comments where id = $1`, twiceEdited).Scan(&second, &third); err != nil {
			t.Fatalf("search the comments at the cut: %v", err)
		}
		if second != 1 || third != 0 {
			t.Errorf("at the cut the comment's search finds 'second' %d times and 'third' %d times, want 1 and 0", second, third)
		}
	})

	t.Run("a cut before an edit made before the history began", func(t *testing.T) {
		const cut = "2026-11-10T08:30:00Z"
		comments := read(t, cut, "comments")
		check(t, comments, twiceEdited, "a comment edited twice after the cut", post{body: "first"})
		check(t, comments, deletedLater, "a comment deleted after the cut", post{body: "", deletedAt: &deleted, deletedBy: true})
		check(t, comments, longEditedLater, "a long comment edited after the history began", post{body: long})
		// No row keeps what that edit replaced, and the newest event at the cut carries only
		// the first part of the body: the body is '', which the answer names absent (reason T).
		check(t, comments, longEditedEarly, "a long comment edited before the history began", post{body: ""})
		messages := read(t, cut, "messages")
		check(t, messages, messageEdited, "a message edited twice after the cut", post{body: "first"})
	})

	t.Run("the present read is unchanged", func(t *testing.T) {
		var body string
		if err := w.database.Pool.QueryRow(ctx, `select body from comments where id = $1`, twiceEdited).Scan(&body); err != nil || body != "third" {
			t.Fatalf("the present body = %q (%v), want third", body, err)
		}
	})
}

// Two edits of one post can overlap: the second edit's transaction begins while the first one
// holds the post's lock, and the first writes its post_body_history row after the second began.
// So a history row's edited_at does not pair it with its edit. Its place does: the j-th row of a
// post in edited_at, id order is the row of the post's j-th edit or delete since the history
// began. At a cut between the two starts the post reads with the body the first edit wrote, the
// one the second edit replaced.
func TestABodyAtACutBetweenTwoOverlappingEditsIsTheFirstEditsBody(t *testing.T) {
	w := newWorld(t)
	var (
		made   = when(t, "2026-11-10T08:00:00Z")
		began  = when(t, "2026-11-10T09:00:00Z") // the history began
		first  = when(t, "2026-11-10T11:00:00Z") // the first edit's transaction begins
		second = when(t, "2026-11-10T11:00:01Z") // the second begins while the first holds the lock
		key    = "ASOF-1"
	)
	w.exec(`update schema_migrations set applied_at = $1 where version = 20261110224508`, began)
	w.project("ASOF", "carol", made.Add(-time.Hour))
	issue := newIssue(key, "ASOF", 1, "Posts", made.Add(-time.Hour))
	w.insertIssue(issue)
	w.issueEvent("issue.created", issue, issue.CreatedAt)

	const (
		commentID = "00000000-0000-4000-8000-0000000000e1"
		messageID = "00000000-0000-4000-8000-0000000000e2"
	)
	comment := newComment(commentID, key, alice, "draft", made)
	w.exec(`insert into comments (id, issue_key, author, body, created_at, edited_at) values ($1, $2, $3, 'final', $4, $5)`,
		commentID, key, w.encode(alice), made.Add(time.Millisecond), second)
	w.event(issueOwned(key), "comment.created", alice, commentPayload(comment), made)
	message := model.Message{ID: messageID, IssueKey: &key, Author: alice, Body: "draft", CreatedAt: made, Deliveries: []model.MessageDelivery{}}
	w.exec(`insert into messages (id, issue_key, author, body, created_at, edited_at) values ($1, $2, $3, 'final', $4, $5)`,
		messageID, key, w.encode(alice), made, second)
	w.event(issueOwned(key), "message.created", alice, messagePayload(message), made)

	// Each edit appends its event and keeps the body it replaced under the post's lock: the first
	// edit at a moment after the second began, the second once the first committed.
	for _, edit := range []struct {
		at, kept time.Time
		replaced string
		body     string
	}{
		{at: first, kept: second.Add(time.Millisecond), replaced: "draft", body: "revised"},
		{at: second, kept: second.Add(2 * time.Millisecond), replaced: "revised", body: "final"},
	} {
		stamp := edit.at.Format(time.RFC3339Nano)
		comment.Body, comment.EditedAt = edit.body, &stamp
		w.event(issueOwned(key), "comment.edited", alice, commentPayload(comment), edit.at)
		w.exec(`insert into post_body_history (comment_id, body, edited_by, edited_at) values ($1, $2, $3, $4)`,
			commentID, edit.replaced, w.encode(alice), edit.kept)
		message.Body, message.EditedAt = edit.body, &edit.at
		w.event(issueOwned(key), "message.edited", alice, messagePayload(message), edit.at)
		w.exec(`insert into post_body_history (message_id, body, edited_by, edited_at) values ($1, $2, $3, $4)`,
			messageID, edit.replaced, w.encode(alice), edit.kept)
	}

	for _, row := range []struct {
		name, cut, want string
	}{
		{name: "before both edits", cut: "2026-11-10T10:00:00Z", want: "draft"},
		{name: "between the two edits' starts", cut: "2026-11-10T11:00:00.5Z", want: "revised"},
		{name: "after both edits", cut: "2026-11-10T12:00:00Z", want: "final"},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, post := range []struct{ table, id string }{{"comments", commentID}, {"messages", messageID}} {
				var body string
				if err := w.database.Pool.QueryRow(atCut(t, row.cut), "select body from "+post.table+" where id = $1", post.id).Scan(&body); err != nil {
					t.Fatalf("read the %s row at %s: %v", post.table, row.cut, err)
				}
				if body != row.want {
					t.Errorf("%s at %s: body %q, want %q", post.table, row.cut, body, row.want)
				}
			}
		})
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func abbreviate(text string) string {
	if len(text) <= 40 {
		return text
	}
	return fmt.Sprintf("%s… (%d bytes)", text[:40], len(text))
}

// A priority's advances are a log of tuples, each asserting or retracting a relation at its
// time. At a cut the log holds the tuples written then, so the relation's newest tuple there is
// its state then: one retracted after the cut still stands, and one asserted after it does not.
// Read through prioritystore.AdvancesTupleRead, the reading of the log the issue read and the
// listing make (issueAdvancesTerms, api/issue_queries.go), and through the log's own history read.
func TestATupleAssertedOrRetractedAfterTheCutIsNotSeenAtTheCut(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	bob := model.Actor{Kind: "user", ID: "bob"}
	for _, step := range []struct {
		priority string
		advance  bool
		actor    model.Actor
		at       string
	}{
		{"ship", true, alice, "2026-11-10T09:00:00Z"},
		{"ship", false, bob, "2026-11-10T12:00:00Z"},
		{"polish", true, alice, "2026-11-10T12:00:00Z"},
	} {
		// Each relation is written in its own transaction, as api's writeIssueAdvances writes it.
		tx, err := w.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		relate := prioritystore.Retract
		if step.advance {
			relate = prioritystore.Advance
		}
		if err := relate(ctx, tx, "ASOF-1", step.priority, step.actor, when(t, step.at)); err != nil {
			t.Fatalf("relate ASOF-1 to %s: %v", step.priority, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	relations := func(t *testing.T, ctx context.Context) []string {
		t.Helper()
		rows, err := w.database.Pool.Query(ctx, `select to_id, asserted from (`+
			prioritystore.AdvancesTupleRead(`, t.asserted`, `and t.from_id = $1`)+`) newest order by to_id`, "ASOF-1")
		var stated []string
		for _, row := range scanAll(t, rows, err) {
			stated = append(stated, fmt.Sprintf("%s asserted=%t", row["to_id"], row["asserted"]))
		}
		return stated
	}
	if got, want := relations(t, atCut(t, "2026-11-10T11:00:00Z")), []string{"ship asserted=true"}; !slices.Equal(got, want) {
		t.Errorf("the relations at the cut are %v, want %v", got, want)
	}
	if got, want := relations(t, ctx), []string{"polish asserted=true", "ship asserted=false"}; !slices.Equal(got, want) {
		t.Errorf("the relations now are %v, want %v", got, want)
	}
	history, err := prioritystore.History(atCut(t, "2026-11-10T11:00:00Z"), w.database.Pool, prioritystore.KindIssue, "ASOF-1", prioritystore.RelationAdvances)
	if err != nil {
		t.Fatalf("read the log at the cut: %v", err)
	}
	if len(history) != 1 || history[0].ToID != "ship" || !history[0].Asserted {
		t.Errorf("the log at the cut is %+v, want the one tuple asserting ship", history)
	}
}

// An issue's state at a cut is its newest whole-issue payload at or before the cut by seq, the
// order its writes committed in under the owner's lock, not by created_at, the time each write
// began: a write that began earlier and committed later is the newer. Its claim is the newer of
// that payload's and the newest claim event's; last_seq is the newest seq at the cut, and
// updated_at the latest time among its events then. An issue created after the cut is in no
// shadow.
func TestTheIssueShadowTakesTheNewestPayloadBySeqAtTheCut(t *testing.T) {
	w := newWorld(t)
	made := when(t, "2026-11-10T09:00:00Z")
	w.project("ASOF", "carol", made.Add(-time.Hour))
	issue := newIssue("ASOF-1", "ASOF", 1, "Created", made)
	present := issue
	present.Title, present.UpdatedAt = "After the cut", when(t, "2026-11-10T12:00:00Z")
	present.Claim = &model.IssueClaim{Actor: sessionActor("s-1"), At: when(t, "2026-11-10T10:20:00Z")}
	w.insertIssue(present)

	w.issueEvent("issue.created", issue, made)
	second := issue
	second.Title, second.UpdatedAt = "Second by seq, later by clock", when(t, "2026-11-10T10:30:00Z")
	w.issueEvent("issue.updated", second, second.UpdatedAt)
	third := issue
	third.Title, third.UpdatedAt = "Third by seq, earlier by clock", when(t, "2026-11-10T10:10:00Z")
	w.issueEvent("issue.updated", third, third.UpdatedAt)
	w.event(issueOwned("ASOF-1"), "issue.claimed", sessionActor("s-1"), model.IssueClaimEventPayload{
		Key: "ASOF-1", Status: model.IssueStatusTriage, Claim: present.Claim, Reason: "claimed",
	}, present.Claim.At)
	w.issueEvent("issue.updated", present, present.UpdatedAt)

	late := newIssue("ASOF-2", "ASOF", 2, "Created after the cut", when(t, "2026-11-10T11:30:00Z"))
	w.insertIssue(late)
	w.issueEvent("issue.created", late, late.CreatedAt)

	rows, err := w.database.Pool.Query(atCut(t, "2026-11-10T11:00:00Z"),
		`select key, title, last_seq, updated_at, claimed_by->>'id' as claimed_by, claimed_at from issues order by key`)
	issues := scanAll(t, rows, err)
	if len(issues) != 1 || issues[0]["key"] != "ASOF-1" {
		t.Fatalf("the issues at the cut are %v, want ASOF-1 alone", issues)
	}
	got := issues[0]
	if got["title"] != third.Title {
		t.Errorf("title at the cut = %q, want %q, the payload newest by seq", got["title"], third.Title)
	}
	if got["last_seq"] != int32(4) {
		t.Errorf("last_seq at the cut = %v, want 4, the newest seq at the cut", got["last_seq"])
	}
	if at, _ := got["updated_at"].(time.Time); !at.Equal(second.UpdatedAt) {
		t.Errorf("updated_at at the cut = %v, want %s, the latest time of the issue's events then", got["updated_at"], second.UpdatedAt)
	}
	if got["claimed_by"] != "s-1" {
		t.Errorf("claimed_by at the cut = %v, want s-1, from the claim event newer than the payload", got["claimed_by"])
	}
	if at, _ := got["claimed_at"].(time.Time); !at.Equal(present.Claim.At) {
		t.Errorf("claimed_at at the cut = %v, want %s", got["claimed_at"], present.Claim.At)
	}
}

// closed_at at a cut is the newest payload's at the cut, whatever the row holds. A close's payload
// is its record: a close written now carries the stamp the row keeps, and a close written before
// this change (INFRA-4620) the start of its transaction, earlier than the row's stamp. Each close
// here is of that older kind, so its payload and the row disagree, and the answer is the
// payload's. A close the issue was reopened from since, or one a later close replaced, is the
// closed_at its own payload carried too. A closed issue's later rank move appends an issue.updated
// read back from the row, which carries the row's stamp: a cut after the move answers that stamp,
// and a cut before it answers the close's payload, both before the move and the later reopen are
// written and after.
func TestTheShadowClosedAtIsTheNewestPayloadsWhateverTheRowHolds(t *testing.T) {
	w := newWorld(t)
	made := when(t, "2026-11-10T09:00:00Z")
	closedAt := when(t, "2026-11-10T10:00:00Z")
	stamped := closedAt.Add(250 * time.Millisecond)
	closedAgain := when(t, "2026-11-10T13:00:00Z")
	stampedAgain := closedAgain.Add(250 * time.Millisecond)
	w.project("ASOF", "carol", made.Add(-time.Hour))

	history := func(key string, number int, reopen, closeAgain bool) {
		issue := newIssue(key, "ASOF", number, key, made)
		present := issue
		present.Status, present.ClosedAt = "done", &stamped
		if reopen {
			present.Status, present.ClosedAt = "todo", nil
		}
		if closeAgain {
			present.Status, present.ClosedAt = "done", &stampedAgain
		}
		w.insertIssue(present)
		w.issueEvent("issue.created", issue, made)
		closed := issue
		closed.Status, closed.ClosedAt = "done", &closedAt
		w.issueEvent("issue.closed", closed, closedAt)
		if reopen {
			reopened := issue
			reopened.Status = "todo"
			w.issueEvent("issue.updated", reopened, when(t, "2026-11-10T12:00:00Z"))
		}
		if closeAgain {
			again := issue
			again.Status, again.ClosedAt = "done", &closedAgain
			w.issueEvent("issue.closed", again, closedAgain)
		}
	}
	history("ASOF-1", 1, false, false)
	history("ASOF-2", 2, true, true)
	history("ASOF-3", 3, true, false)

	// ASOF-4 is closed and later moved in rank, which a closed issue takes; ASOF-5 is moved and
	// then reopened. Each rank move's payload is read back from the row, so it carries the
	// row's second stamp.
	laterIssues := map[string]model.Issue{}
	for number, key := range []string{"ASOF-4", "ASOF-5"} {
		issue := newIssue(key, "ASOF", number+4, key, made)
		laterIssues[key] = issue
		present := issue
		present.Status, present.ClosedAt, present.Rank = "done", &stamped, "n"
		if key == "ASOF-5" {
			present.Status, present.ClosedAt = "todo", nil
		}
		w.insertIssue(present)
		w.issueEvent("issue.created", issue, made)
		closed := issue
		closed.Status, closed.ClosedAt = "done", &closedAt
		w.issueEvent("issue.closed", closed, closedAt)
	}
	closedAtTheCut := func(t *testing.T, cut, key string) *time.Time {
		t.Helper()
		var got *time.Time
		if err := w.database.Pool.QueryRow(atCut(t, cut), `select closed_at from issues where key = $1`, key).Scan(&got); err != nil {
			t.Fatalf("read the issue at the cut: %v", err)
		}
		return got
	}
	for _, key := range []string{"ASOF-4", "ASOF-5"} {
		if got := closedAtTheCut(t, "2026-11-10T11:00:00Z", key); !sameTime(got, &closedAt) {
			t.Errorf("%s: closed_at = %v at the cut before its rank move is written, want its close's payload's %v", key, got, closedAt)
		}
	}
	for _, key := range []string{"ASOF-4", "ASOF-5"} {
		rankMoved := laterIssues[key]
		rankMoved.Status, rankMoved.ClosedAt, rankMoved.Rank = "done", &stamped, "n"
		w.issueEvent("issue.updated", rankMoved, when(t, "2026-11-10T12:00:00Z"))
	}
	reopened := laterIssues["ASOF-5"]
	reopened.Status, reopened.Rank = "todo", "n"
	w.issueEvent("issue.updated", reopened, when(t, "2026-11-10T12:30:00Z"))

	for _, row := range []struct {
		cut, key, why string
		want          *time.Time
	}{
		{"2026-11-10T11:00:00Z", "ASOF-1", "the close's payload, not the row's stamp", &closedAt},
		{"2026-11-10T11:00:00Z", "ASOF-2", "a close a later close replaced", &closedAt},
		{"2026-11-10T14:00:00Z", "ASOF-2", "the later close's payload, not the row's stamp", &closedAgain},
		{"2026-11-10T11:00:00Z", "ASOF-3", "a close the issue was reopened from: the payload's stamp", &closedAt},
		{"2026-11-10T12:30:00Z", "ASOF-3", "the reopen", nil},
		{"2026-11-10T09:30:00Z", "ASOF-1", "before the close", nil},
		{"2026-11-10T11:00:00Z", "ASOF-4", "a close a later rank move read back: the close's payload", &closedAt},
		{"2026-11-10T12:15:00Z", "ASOF-4", "the rank move, which carries the row's stamp", &stamped},
		{"2026-11-10T11:00:00Z", "ASOF-5", "a close whose first later write is a rank move, then reopened", &closedAt},
		{"2026-11-10T12:15:00Z", "ASOF-5", "the rank move of a close later reopened", &stamped},
		{"2026-11-10T12:45:00Z", "ASOF-5", "the reopen", nil},
	} {
		t.Run(row.key+" at "+row.cut, func(t *testing.T) {
			if got := closedAtTheCut(t, row.cut, row.key); !sameTime(got, row.want) {
				t.Errorf("closed_at = %v at the cut, want %v (%s)", got, row.want, row.why)
			}
		})
	}
}

// issues.rank is text collate "C" (migration 0049): a rank compares its base-62 digits as bytes,
// 0-9 before A-Z before a-z. The issues shadow reads the rank from the payload, so it names that
// collation too, and the issue listing's order (issueListOrder, api/issue_queries.go) puts the
// ranks U, Z and a in the order the present listing does, where the database's default collation
// would put a first.
func TestTheIssueListingAtACutOrdersRanksAsThePresentDoes(t *testing.T) {
	w := newWorld(t)
	made := when(t, "2026-11-10T09:00:00Z")
	w.project("ASOF", "carol", made.Add(-time.Hour))
	for number, rank := range []string{"a", "Z", "U"} {
		issue := newIssue(fmt.Sprintf("ASOF-%d", number+1), "ASOF", number+1, "Ranked "+rank, made)
		issue.Rank = rank
		w.insertIssue(issue)
		w.issueEvent("issue.created", issue, made)
	}
	const listing = `select i.key from issues i order by i.rank asc, i.created_at asc, i.key asc`
	read := func(ctx context.Context) []string {
		t.Helper()
		rows, err := w.database.Pool.Query(ctx, listing)
		if err != nil {
			t.Fatalf("list the issues: %v", err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("list the issues: %v", err)
		}
		return keys
	}
	want := []string{"ASOF-3", "ASOF-2", "ASOF-1"}
	if present := read(context.Background()); !slices.Equal(present, want) {
		t.Fatalf("the present listing orders the ranks U, Z, a as %v, want %v", present, want)
	}
	if got := read(atCut(t, "2026-11-10T10:00:00Z")); !slices.Equal(got, want) {
		t.Errorf("the listing at a cut orders the ranks U, Z, a as %v, want %v, the present order", got, want)
	}
}

// Migration 20261111210103 filled every project's default assignee with no event. Dispatch
// records a change of it from its first settings.project_default_assignee.updated
// (2026-11-15T07:01:28Z): at a cut the default is the newest such event's; else the present one
// when no such event is later than the cut and the cut is from that time on; else unknown, a null
// the answer names absent (reason D).
func TestTheProjectDefaultIsUnknownBeforeItsFirstEvent(t *testing.T) {
	w := newWorld(t)
	made := when(t, "2026-11-01T00:00:00Z")
	w.project("QUIET", "carol", made)
	w.project("MOVED", "carol", made)
	for _, change := range []struct{ assignee, at string }{
		{"bob", "2026-11-16T09:00:00Z"},
		{"carol", "2026-11-17T09:00:00Z"},
	} {
		w.event(projectOwned("MOVED"), "settings.project_default_assignee.updated", alice,
			map[string]any{"project": "MOVED", "assignee": change.assignee}, when(t, change.at))
	}
	for _, row := range []struct {
		cut, project string
		want         *string
	}{
		{"2026-11-14T00:00:00Z", "QUIET", nil},
		{"2026-11-14T00:00:00Z", "MOVED", nil},
		{"2026-11-15T12:00:00Z", "QUIET", new("carol")},
		{"2026-11-15T12:00:00Z", "MOVED", nil},
		{"2026-11-16T12:00:00Z", "MOVED", new("bob")},
		{"2026-11-18T00:00:00Z", "MOVED", new("carol")},
	} {
		t.Run(row.project+" at "+row.cut, func(t *testing.T) {
			var got *string
			if err := w.database.Pool.QueryRow(atCut(t, row.cut), `select default_assignee from projects where key = $1`, row.project).Scan(&got); err != nil {
				t.Fatalf("read the project at the cut: %v", err)
			}
			if (got == nil) != (row.want == nil) || (got != nil && *got != *row.want) {
				t.Errorf("default_assignee = %v at the cut, want %v", deref(got), deref(row.want))
			}
		})
	}
}

// Migration 20261119000230 added projects.default_repo and filled it from each project's
// repository mapping, whose default row named the project's default repository before it. From
// that migration on, each change of the default appends a project.updated that carries the
// project, which leaves default_repo out where the default is cleared. So at a cut the default is
// the newest such event's since the migration; else the present one, when no such event is later
// than the cut; else the mapping's default row, at the cut or, for a later cut, at the migration.
func TestTheProjectDefaultRepositoryAtACut(t *testing.T) {
	w := newWorld(t)
	w.exec(`update schema_migrations set applied_at = '2026-11-19T00:00:00Z' where version = 20261119000230`)
	made := when(t, "2026-11-01T00:00:00Z")
	w.project("MAPPED", "carol", made)
	w.project("CHANGED", "carol", made)
	for _, mapping := range []struct{ repo, at string }{
		{"acme/first", "2026-11-10T00:00:00Z"},
		{"acme/second", "2026-11-12T00:00:00Z"},
	} {
		w.event(projectOwned("MAPPED"), "settings.repo_project.updated", alice,
			map[string]any{"mapping": map[string]any{"repo": mapping.repo, "project": "MAPPED", "default": true}}, when(t, mapping.at))
	}
	// The second mapping took the default, so its switch demoted the first, as the writer does.
	w.event(projectOwned("MAPPED"), "settings.repo_project.updated", alice,
		map[string]any{"mapping": map[string]any{"repo": "acme/first", "project": "MAPPED", "default": false}}, when(t, "2026-11-12T00:00:00Z"))
	w.exec(`update projects set default_repo = 'acme/second' where key = 'MAPPED'`)
	w.event(projectOwned("CHANGED"), "project.updated", alice,
		map[string]any{"key": "CHANGED", "name": "CHANGED", "default_repo": "acme/set"}, when(t, "2026-11-20T00:00:00Z"))
	w.event(projectOwned("CHANGED"), "project.updated", alice,
		map[string]any{"key": "CHANGED", "name": "CHANGED"}, when(t, "2026-11-21T00:00:00Z"))
	for _, row := range []struct {
		cut, project string
		want         *string
		why          string
	}{
		{"2026-11-11T00:00:00Z", "MAPPED", new("acme/first"), "the mapping's default row at the cut, before the migration"},
		{"2026-11-13T00:00:00Z", "MAPPED", new("acme/second"), "the mapping's default row moved before the cut"},
		{"2026-11-20T00:00:00Z", "MAPPED", new("acme/second"), "no change since the migration: the present value"},
		{"2026-11-19T12:00:00Z", "CHANGED", nil, "a change after the cut, none before it: the mapping, which names none, at the migration"},
		{"2026-11-20T12:00:00Z", "CHANGED", new("acme/set"), "the newest change at the cut"},
		{"2026-11-22T00:00:00Z", "CHANGED", nil, "a change that leaves default_repo out clears it"},
	} {
		t.Run(row.project+" at "+row.cut, func(t *testing.T) {
			var got *string
			if err := w.database.Pool.QueryRow(atCut(t, row.cut), `select default_repo from projects where key = $1`, row.project).Scan(&got); err != nil {
				t.Fatalf("read the project at the cut: %v", err)
			}
			if (got == nil) != (row.want == nil) || (got != nil && *got != *row.want) {
				t.Errorf("default_repo = %v at the cut, want %v (%s)", deref(got), deref(row.want), row.why)
			}
		})
	}
}

func deref(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

// A document's title correction appends artifact.renamed with the name it replaced
// (api/artifact_rename.go), so at a cut a document carries the previous_name of its first rename
// after the cut, and the present name when none is later. A document made after the cut is in no
// shadow.
func TestAnArtifactRenamedAfterTheCutCarriesItsOldName(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := when(t, "2026-11-10T09:00:00Z")
	key := "ASOF-1"
	w.project("ASOF", "carol", made.Add(-time.Hour))
	issue := newIssue(key, "ASOF", 1, "Documents", made.Add(-time.Hour))
	w.insertIssue(issue)
	w.issueEvent("issue.created", issue, issue.CreatedAt)

	document := func(slug, name string, created time.Time) model.Artifact {
		var id string
		if err := w.database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by, created_at)
			values ($1, 'ASOF', $2, $3, 'doc', $4, $5) returning id::text`, key, slug, name, w.encode(alice), created).Scan(&id); err != nil {
			t.Fatalf("insert the document %s: %v", slug, err)
		}
		return model.Artifact{ID: id, IssueKey: &key, Project: "ASOF", Slug: slug, Name: name, Kind: "doc", CreatedBy: alice, CreatedAt: created}
	}
	notes := document("notes", "Draft notes", made)
	for _, rename := range []struct{ name, at string }{
		{"Plan notes", "2026-11-10T10:00:00Z"},
		{"Final notes", "2026-11-10T12:00:00Z"},
	} {
		previous := notes.Name
		notes.Name = rename.name
		w.exec(`update artifacts set name = $2 where id = $1`, notes.ID, rename.name)
		w.event(issueOwned(key), "artifact.renamed", alice, map[string]any{"artifact": notes, "previous_name": previous}, when(t, rename.at))
	}
	later := document("later", "Written later", when(t, "2026-11-10T11:30:00Z"))

	for _, row := range []struct{ cut, want string }{
		{"2026-11-10T09:30:00Z", "Draft notes"},
		{"2026-11-10T11:00:00Z", "Plan notes"},
		{"2026-11-10T13:00:00Z", "Final notes"},
	} {
		t.Run(row.cut, func(t *testing.T) {
			rows, err := w.database.Pool.Query(atCut(t, row.cut), `select id::text, name from artifacts where issue_key = $1`, key)
			names := map[string]string{}
			for _, read := range scanAll(t, rows, err) {
				names[read["id"].(string)] = read["name"].(string)
			}
			if names[notes.ID] != row.want {
				t.Errorf("the document's name at the cut = %q, want %q", names[notes.ID], row.want)
			}
			_, seen := names[later.ID]
			if exists := !when(t, row.cut).Before(later.CreatedAt); seen != exists {
				t.Errorf("the document made at %s is in the shadow: %t, want %t", later.CreatedAt, seen, exists)
			}
		})
	}
}

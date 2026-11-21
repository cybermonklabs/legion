package asof_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The shadows' tests seed rows and events in the shapes Dispatch's writers give them. The
// writers are package api's handlers, which need a whole server; these helpers write the same
// statements and the same payload types, and each names the writer it mirrors.

var alice = model.Actor{Kind: "user", ID: "alice"}

func sessionActor(id string) model.Actor { return model.Actor{Kind: "session", ID: id} }

// world is one test's database and the helpers that seed it.
type world struct {
	t        *testing.T
	database *store.Store
}

func newWorld(t *testing.T) world {
	t.Helper()
	return world{t: t, database: storetest.Open(t)}
}

// when is a time of the seed, given in RFC 3339.
func when(t *testing.T, raw string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed.UTC()
}

// atCut is a context that carries the cut raw names: every statement the store sends under it
// is bound to the shadows.
func atCut(t *testing.T, raw string) context.Context {
	t.Helper()
	return asof.WithCut(context.Background(), mustCut(t, raw))
}

func (w world) exec(sql string, args ...any) {
	w.t.Helper()
	if _, err := w.database.Pool.Exec(context.Background(), sql, args...); err != nil {
		w.t.Fatalf("seed (%s): %v", strings.Join(strings.Fields(sql), " "), err)
	}
}

func (w world) encode(value any) []byte {
	w.t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		w.t.Fatalf("encode %T: %v", value, err)
	}
	return encoded
}

// owner is the owner of an event as events_one_owner holds it: an issue or a project here.
type owner struct {
	issue, project *string
}

func issueOwned(key string) owner   { return owner{issue: &key} }
func projectOwned(key string) owner { return owner{project: &key} }

// event appends an event as events.Broker.QueueAppend does, in one statement: the owner's next
// seq, read from its row, and that row's last_seq (and an issue's updated_at, by greatest())
// moved to it. The event's created_at is at, the start of the transaction that wrote it.
func (w world) event(o owner, kind string, actor model.Actor, payload any, at time.Time) int64 {
	w.t.Helper()
	args := []any{o.issue, nil, o.project, kind, w.encode(actor), w.encode(payload), at}
	var id int64
	if err := w.database.Pool.QueryRow(context.Background(), `
		with appended as (
			insert into events (issue_key, artifact_id, project_key, seq, type, actor, payload, notify, created_at)
			values ($1, $2::uuid, $3, coalesce(
				(select last_seq from issues where key = $1) + 1,
				(select last_seq from artifacts where id = $2::uuid) + 1,
				(select last_seq from projects where key = $3) + 1,
				0), $4, $5, $6, false, $7)
			returning id, seq, created_at
		), issue as (
			update issues set last_seq = appended.seq, updated_at = greatest(issues.updated_at, appended.created_at)
			from appended where issues.key = $1
		), artifact as (
			update artifacts set last_seq = appended.seq from appended where artifacts.id = $2::uuid
		), project as (
			update projects set last_seq = appended.seq from appended where projects.key = $3
		)
		select id from appended
	`, args...).Scan(&id); err != nil {
		w.t.Fatalf("append %s: %v", kind, err)
	}
	return id
}

// project inserts a project as it stands now, made at created, and its project.created event
// (api/projects.go writes both).
func (w world) project(key, defaultAssignee string, created time.Time) {
	w.t.Helper()
	w.exec(`insert into projects (key, name, default_assignee, created_at) values ($1, $1, $2, $3)`,
		key, defaultAssignee, created)
	w.event(projectOwned(key), "project.created", alice,
		map[string]any{"key": key, "name": key, "created_at": created, "open_asks": 0, "gated_steps": []string{}}, created)
}

// newIssue is an issue as its payload carries it (model.Issue), with every list an empty list.
func newIssue(key, project string, number int, title string, created time.Time) model.Issue {
	return model.Issue{
		Key: key, Project: project, Number: number, Title: title, Status: model.IssueStatusTriage, Rank: "m",
		Labels: []string{}, BlockedBy: []string{}, Advances: []model.IssueAdvance{},
		Components:    model.IssueComponents{Mode: "inherit", IDs: []string{}, Unknown: []string{}},
		ExternalLinks: []model.ExternalLink{}, CreatedBy: &alice, CreatedAt: created, UpdatedAt: created,
	}
}

// insertIssue inserts the issues row as it stands now. Its events come after it, each with the
// issue as that write left it (issueEvent), as api/issue_create.go and api/issue_patch.go append
// them.
func (w world) insertIssue(issue model.Issue) {
	w.t.Helper()
	var claimedBy any
	var claimedAt *time.Time
	if issue.Claim != nil {
		claimedBy, claimedAt = w.encode(issue.Claim.Actor), &issue.Claim.At
	}
	w.exec(`
		insert into issues (key, project_key, number, title, status, labels, rank, created_by, created_at,
			updated_at, closed_at, assignee, priority, parent_key, route, repo, claimed_by, claimed_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
	`, issue.Key, issue.Project, issue.Number, issue.Title, issue.Status, issue.Labels, issue.Rank,
		w.encode(issue.CreatedBy), issue.CreatedAt, issue.UpdatedAt, issue.ClosedAt, issue.Assignee,
		issue.Priority, issue.Parent, issue.Route, issue.Repo, claimedBy, claimedAt)
}

// issueEvent appends a whole-issue event whose payload is issue as the write left it
// (model.NewIssueEventPayload).
func (w world) issueEvent(kind string, issue model.Issue, at time.Time) int64 {
	w.t.Helper()
	return w.event(issueOwned(issue.Key), kind, alice, model.NewIssueEventPayload(issue, model.ReferenceChanges{}), at)
}

// newComment is an issue's comment as its payload carries it.
func newComment(id, issueKey string, author model.Actor, body string, created time.Time) model.Comment {
	return model.Comment{
		ID: id, IssueKey: &issueKey, Author: author, Body: body, CreatedAt: created,
		Mentions: []model.Mention{}, Deliveries: []model.CommentDelivery{},
	}
}

// commentPayload is the payload of a comment.* event (model.CommentEventPayload), the body cut
// as an event carries it (model.EventBody).
func commentPayload(comment model.Comment) model.CommentEventPayload {
	body, truncated := model.EventBody(comment.Body)
	comment.Body = body
	return model.CommentEventPayload{Comment: comment, BodyTruncated: truncated}
}

// messagePayload is the payload of a message.* event (model.MessageEventPayload), the body cut as
// an event carries it.
func messagePayload(message model.Message) model.MessageEventPayload {
	body, truncated := model.EventBody(message.Body)
	message.Body = body
	return model.MessageEventPayload{Message: message, BodyTruncated: truncated}
}

// keepReplacedBody writes the post_body_history row an edit or a delete of a post writes in its
// own transaction (postBodyHistoryStep, api/post_history.go): the body it replaced, its actor, and
// edited_at, the column's clock_timestamp() as the statement runs, a moment after the
// transaction began at began.
func (w world) keepReplacedBody(column, postID, body string, began time.Time) {
	w.t.Helper()
	w.exec(`insert into post_body_history (`+column+`, body, edited_by, edited_at) values ($1, $2, $3, $4)`,
		postID, body, w.encode(alice), began.Add(time.Millisecond))
}

// scanAll reads every row of rows into maps of column to value, so a test names the columns it
// checks. It fails the test on any error.
func scanAll(t *testing.T, rows pgx.Rows, err error) []map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	collected, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		t.Fatalf("read the rows: %v", err)
	}
	return collected
}

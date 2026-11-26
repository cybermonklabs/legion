package intake_test

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	nulProject = "ACME"
	nulIssue   = "ACME-12"
	nulRepo    = drainRepo
	nulNumber  = 12
	nulHead    = "5d2a9c4e7b1f0a3c6e9b2d5f8a1c4e7b0d3f6a9c"
)

// A review's body is a person's own text, and Envoy carries it as GitHub sent it: a NUL in it
// reaches the daemon as the JSON escape \u0000, which PostgreSQL refuses in jsonb (SQLSTATE
// 22P05). A review the store refuses would be nacked and delivered again for as long as the
// daemon runs, and its round would never hear the decision. The consumer applies it instead: the
// round's decision holds the body with U+FFFD where the NUL was, and the GitHub consumer
// delivered the event once and acknowledged it.
func TestAReviewWhoseBodyHoldsNULIsAppliedOnceWithAReplacementCharacter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := testpg.ClonePool(t, "legion_intake_nul_test")
	seedNULIssue(t, pool, phase.Reviewing, "needs_review", claim.RoleReviewer)
	// The merge state is left unread, so the approval decides the round and the round waits there
	// for GitHub's answer: the issue stays in reviewing with the decision on its reviewer's row.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: nulIssue, Repo: nulRepo,
			Number: nulNumber, Branch: "legion/" + nulIssue, Base: "main", HeadSHA: nulHead})
	}); err != nil {
		t.Fatalf("seed the pull request of %s: %v", nulIssue, err)
	}
	js, stream, logs := runNULConsumers(t, pool)

	n := strconv.Itoa(nulNumber)
	// The listener's normalizer marshals the review's fields as strings, so the payload carries
	// the NUL as \u0000 and the envelope, which holds the payload as a string, as \\u0000.
	review := drainWebhookEnvelope("nul-review", drainSubjectPrefix+".pr."+n+".review", "review approved on "+nulRepo+"#"+n, map[string]string{
		"kind": "review", "action": "submitted", "repo": nulRepo, "number": n, "title": "change " + n, "parent_kind": "pr",
		"author": "maintainer", "url": "https://github.com/" + nulRepo + "/pull/" + n + "#review", "state": "approved",
		"review_id": "7012", "submitted_at": "2026-11-19T09:00:00Z", "commit_id": nulHead, "head_sha": nulHead,
		"body": "looks good\x00 ship it",
	})
	sent, err := js.Publish(requestContext(t), review.subject, review.data)
	if err != nil {
		t.Fatalf("publish %s: %v", review.subject, err)
	}

	acknowledgedOnce(t, stream, "legion-go-"+nulProject+"-github", sent.Sequence, logs)
	var body string
	if err := pool.QueryRow(ctx, `select coalesce(decision ->> 'body', '') from phases where issue = $1 and role = $2`,
		nulIssue, string(claim.RoleReviewer)).Scan(&body); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read the reviewer's decision of %s: %v", nulIssue, err)
	}
	if want := "looks good\uFFFD ship it"; body != want {
		t.Errorf("the round's decision holds the body %q, want %q", body, want)
	}
}

// An issue's title is a person's own text, and Dispatch carries a NUL in it as the JSON escape
// \u0000, which PostgreSQL refuses in a text column (SQLSTATE 22021). An edit the store refused
// would be nacked and delivered again for as long as the daemon runs, and every later event of
// the issue's would wait behind it on the Dispatch consumer. The consumer applies it instead: the
// record's title holds U+FFFD where the NUL was, and the Dispatch consumer delivered the event
// once and acknowledged it.
func TestAnIssueEventWhoseTitleHoldsNULIsAppliedOnceWithAReplacementCharacter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := testpg.ClonePool(t, "legion_intake_nul_test")
	seedNULIssue(t, pool, phase.Implementing, "in_progress", claim.RoleImplementer)
	js, stream, logs := runNULConsumers(t, pool)

	subject := "notifications.dispatch.issue." + nulProject + "." + nulIssue + ".issue.updated"
	// Dispatch marshals the event, so its payload carries the NUL as \u0000 and the envelope,
	// which holds the payload as a string, as \\u0000.
	event := drainJSON(map[string]any{
		"id": 4012, "issue_key": nulIssue, "project": nulProject, "seq": 2, "type": "issue.updated",
		"actor": map[string]string{"kind": "user", "id": "someone"}, "notify": true, "created_at": "2026-11-19T09:00:00Z",
		"payload": map[string]any{
			"key": nulIssue, "status": "in_progress", "title": "widgets\x00 renamed", "parent": nil, "rank": "M",
			"labels": []string{"legion"}, "resolved_repo": nulRepo, "blocked_by": []string{}, "claim": nil,
		},
	})
	sent, err := js.Publish(requestContext(t), subject, drainJSON(map[string]any{
		"event_id": "dispatch-4012", "source": "dispatch", "source_event_id": "4012", "topic": subject,
		"dedupe_key": "dispatch-4012", "issued_at": int64(1_795_510_800_000), "payload_summary": nulIssue + " issue updated",
		"payload": string(event), "trace_id": "dispatch-4012",
	}))
	if err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}

	acknowledgedOnce(t, stream, "legion-go-"+nulProject+"-dispatch", sent.Sequence, logs)
	var title string
	if err := pool.QueryRow(ctx, `select title from issues where key = $1`, nulIssue).Scan(&title); err != nil {
		t.Fatalf("read the title of %s: %v", nulIssue, err)
	}
	if want := "widgets\uFFFD renamed"; title != want {
		t.Errorf("the record's title of %s is %q, want %q", nulIssue, title, want)
	}
}

// runNULConsumers runs the daemon's durable consumers on a stream of their own with the handlers
// the daemon runs, applying facts through store.NewPool(pool), the pool every fact of the daemon
// is written through. The consumer and the handlers log at warn and above into the returned
// buffer, which is where a refused fact's error, and its SQLSTATE, is to be read. GitHub answers
// that every reviewer can write to the repository.
func runNULConsumers(t *testing.T, pool *pgxpool.Pool) (jetstream.JetStream, jetstream.Stream, *testlog.Buffer) {
	t.Helper()
	js := testnats.JetStream(t)
	stream := testnats.CreateStream(t, js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	logs := &testlog.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: nulProject, Repositories: drainRepositories{}, Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(records, engine, 10, nulProject, log)
	// The window bounds the review's store read (whether its pull request is recorded) and is not
	// the rows' subject: a fixed one let the machine decide whether a slow read was cut and its
	// message delivered a second time, which these rows count. It is what is left of the test
	// binary's deadline less 30 s (testwait.Remaining), so a read however slow is waited out.
	consumers, err := intake.OpenConsumers(t.Context(), js, store.NewPool(pool), intake.ConsumerSpec{
		Project: nulProject, Repositories: []ghrepo.Repository{ghrepo.MustParse(nulRepo)},
		AckWait: testwait.Remaining(t), NakDelay: time.Second, Logger: log,
		ReviewPermission: func(context.Context, intake.PullRequestReview) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	run, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- consumers.Run(run, blockers.NewMirror(records, nulProject), engine, admission) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	return js, stream, logs
}

// acknowledgedOnce waits for the durable consumer named to acknowledge the message at stream
// sequence seq, the only one its filter takes here, or to deliver it a second time, and fails the
// test unless it delivered that message once and acknowledged it, with what the consumer logged.
// A fact PostgreSQL refuses is naked and delivered again every NakDelay for as long as the
// consumer runs, so the second delivery ends the wait with the redelivery counted, and the wait
// has no bound of its own but what is left of the test binary's deadline less 30 s
// (testwait.Remaining): a fixed one let a slow store read fail the row. Its reads of the consumer
// end there too (requestContext).
func acknowledgedOnce(t *testing.T, stream jetstream.Stream, consumer string, seq uint64, logs *testlog.Buffer) {
	t.Helper()
	requests := requestContext(t)
	item, err := stream.Consumer(requests, consumer)
	if err != nil {
		t.Fatalf("read the %s consumer: %v", consumer, err)
	}
	started := time.Now()
	var info *jetstream.ConsumerInfo
	ended := testwait.Holds(testwait.Remaining(t), 50*time.Millisecond, func() bool {
		info, err = item.Info(requests)
		return err == nil && ((info.AckFloor.Stream >= seq && info.NumAckPending == 0) || info.Delivered.Consumer > 1)
	})
	if err != nil {
		t.Fatalf("read the %s consumer's info: %v", consumer, err)
	}
	if !ended || info.Delivered.Consumer != 1 || info.AckFloor.Stream != seq {
		t.Errorf("after %s the %s consumer delivered %d messages and acknowledged through stream sequence %d, want the event at %d delivered once and acknowledged; the consumer logged:\n%s",
			time.Since(started).Round(time.Millisecond), consumer, info.Delivered.Consumer, info.AckFloor.Stream, seq, logs.String())
	}
}

// requestContext is the context of a row's own JetStream requests: it ends where the row's hang
// guards end, the test binary's deadline less 30 s (testwait.Remaining), in place of the five
// seconds nats.go gives a request whose context has no deadline (its defaultAPITimeout), so a NATS
// server the machine holds is waited out and one that never answers fails the row by name.
func requestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testwait.Remaining(t))
	t.Cleanup(cancel)
	return ctx
}

// seedNULIssue records nulIssue as a root standing in at with the status given, slotted, with its
// Dispatch mirror row, its tree's lifecycle open under the workflow and the claim of the role
// working its phase, so the admission pass after each fact that runs one finds the tree as a
// running daemon leaves it (seedRedeliveredIssue says why each row is there).
func seedNULIssue(t *testing.T, pool *pgxpool.Pool, at phase.Phase, status string, role claim.Role) {
	t.Helper()
	ctx := context.Background()
	token, err := claim.ProjectToken(nulProject)
	if err != nil {
		t.Fatalf("the project token of %s: %v", nulProject, err)
	}
	held, err := claim.NewToken(token, nulIssue, role)
	if err != nil {
		t.Fatalf("the %s token of %s: %v", role, nulIssue, err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into issues (key, tree, project, title, parent, phase, generation, status, dispatch_status, rank,
			handed_over, last_dispatch_seq, repo, default_branch, github_repo)
			values ($1, $1, $2, 'widgets', null, $3, 1, $4, $4, 'M', true, 1, $5, 'main', $5)`,
			nulIssue, nulProject, string(at), status, nulRepo); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into dispatch_issues (key, project, status, blockers, dispatch_seq) values ($1, $2, $3, '{}', 1)`,
			nulIssue, nulProject, status); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority) values ($1, $2, 1, 'workflow')`,
			token, nulIssue); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, $2, $3, $3, $4, 1, 'ses', '/sessions/ses.jsonl', $5, 1, 0, 0, 0, 0)`,
			string(held), token, nulIssue, string(role), string(supervise.StateWorking)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into slots (issue, index, admitted_at) values ($1, 0, now())`, nulIssue)
		return err
	}); err != nil {
		t.Fatalf("seed %s: %v", nulIssue, err)
	}
}

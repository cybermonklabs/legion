//go:build basebuild

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testtmp"
)

// This child's two measurements, taken before its first line of product code (spec
// R11, plan T1), over the corpus one_inbox_scale_seed_test.go seeds:
//
//   - the participant set (TestOneInboxParticipantSetOfTheCorpus): R11's three statements over the
//     corpus on this build's own schema, which is the base's until the first product change. The
//     stop rule is R4's: a thread of the corpus above oneInboxParticipantBound participants stops
//     the work, and fails this test.
//   - the write baseline (TestOneInboxWriteBaseline): every write path R11 names, timed against
//     the build of the commit BASELINE_SHA names, booted over the database c-p99 names. Each
//     reading is the round trip on loopback, because the base build's own clock is not this
//     process's to read, and 200 of them give a 99th percentile that is the 198th reading rather
//     than the slowest. It counts too the rows one comment in the widest thread writes, and fails
//     where its per-participant rows pass two for each participant (R4's append and update).
//
// Each prints its readings on lines of their own, under the prefixes measurement-one: and
// measurement-two:, each with the moment it was taken, which is what the pull request body
// carries. Both run when LEGION_SCALE_TEST is 1, as a14 does (inbox_dismissed_scale_test.go):
// seeding the corpus and taking 200 timed readings of each write path costs minutes, which no
// ordinary run pays.

// The participant set, in exactly R11's words: the participants user_threads holds per thread, and
// the count over the writers user_threads cannot hold, the sessions, over comments and over messages
// (a lower bound for messages, whose root can be further up than in_reply_to).
const (
	oneInboxThreadsStatement  = `select max(n), percentile_disc(0.999) within group (order by n), count(*) from (select thread_id, count(*) as n from user_threads group by thread_id) t`
	oneInboxCommentsStatement = `select max(n) from (select coalesce(ask_id, reply_to, id) as root, count(distinct author->>'id') as n from comments group by 1) t`
	oneInboxMessagesStatement = `select max(n) from (select coalesce(in_reply_to, id) as root, count(distinct author->>'id') as n from messages group by 1) t`
)

// The base build's database and worktree, as the Verification preamble names them for c-p99.
const (
	oneInboxDatabaseEnv = "ISSUE_P99_DATABASE_URL"
	oneInboxDatabase    = "dispatch_infra_1152_p99"
	oneInboxWorktree    = "base-p99"
)

// oneInboxSettle bounds the wait for a build's process to drain what the corpus owes its background
// work: every comment the seed wrote is owed a block index and a grammar pass, which the build's
// own reconciliation pays off a batch at a time. A timing taken while it still runs is a timing of
// that work too.
const oneInboxSettle = 30 * time.Minute

// oneInboxOwedWork is what the base build still owes the corpus, as its block index tick counts it
// (docs/block_index.go): bodies owed a block-grammar pass, sources owed a grammar pass, and
// comments, messages and asks owed a block index of their own or their windows.
const oneInboxOwedWork = `
	select (select count(*) from block_grammar_owed),
	       (select count(*) from refs_grammar_owed),
	       (select count(*) from comment_block_state where owed_at is not null or windows_owed_at is not null)
	         + (select count(*) from message_block_state where owed_at is not null or windows_owed_at is not null)
	         + (select count(*) from ask_block_state where owed_at is not null or windows_owed_at is not null)`

// oneInboxOwedWorkHere is what this build still owes the corpus, as its own block index tick takes
// it: the same passes, with a window owed only where its window arm takes the item - one the index
// has reached or one holding a current block (refs.ItemsOwedWindows). An item marked whose state
// is neither is no work of this build's and stays marked, so oneInboxOwedWork never reaches zero
// over this build's corpus: on 2026-11-18 its 43,980 messages each held such a mark once this
// build's process had settled.
const oneInboxOwedWorkHere = `
	select (select count(*) from block_grammar_owed),
	       (select count(*) from refs_grammar_owed),
	       (select count(*) from comment_block_state s where s.owed_at is not null or (s.windows_owed_at is not null
	          and (s.body_sha256 is not null or exists (select 1 from comment_blocks b where b.comment_id = s.comment_id and b.position is not null))))
	         + (select count(*) from message_block_state s where s.owed_at is not null or (s.windows_owed_at is not null
	          and (s.body_sha256 is not null or exists (select 1 from message_blocks b where b.message_id = s.message_id and b.position is not null))))
	         + (select count(*) from ask_block_state s where s.owed_at is not null or (s.windows_owed_at is not null
	          and (s.body_sha256 is not null or exists (select 1 from ask_blocks b where b.ask_id = s.ask_id and b.position is not null))))`

// TestOneInboxParticipantSetOfTheCorpus is measurement one: the corpus seeded on a database of the
// test's own through this build's handler, and R11's three statements read over it.
func TestOneInboxParticipantSetOfTheCorpus(t *testing.T) {
	t.Parallel()
	if os.Getenv(dismissedScaleEnv) != "1" {
		t.Skipf("set %s=1 to take the participant set of the one-inbox corpus", dismissedScaleEnv)
	}
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: standInListener(t)})
	seedOneInboxCorpus(t, database, handlerCaller{handler: handler})

	ctx := context.Background()
	var widest, p999, threads int64
	if err := database.Pool.QueryRow(ctx, oneInboxThreadsStatement).Scan(&widest, &p999, &threads); err != nil {
		t.Fatalf("read the participants of each thread: %v", err)
	}
	t.Logf("measurement-one: user_threads max=%d p99.9=%d threads=%d taken_at=%s", widest, p999, threads, oneInboxNow())
	var commented int64
	if err := database.Pool.QueryRow(ctx, oneInboxCommentsStatement).Scan(&commented); err != nil {
		t.Fatalf("read the writers of each comment thread: %v", err)
	}
	t.Logf("measurement-one: comments max=%d taken_at=%s", commented, oneInboxNow())
	var messaged int64
	if err := database.Pool.QueryRow(ctx, oneInboxMessagesStatement).Scan(&messaged); err != nil {
		t.Fatalf("read the writers of each message thread: %v", err)
	}
	t.Logf("measurement-one: messages max=%d taken_at=%s", messaged, oneInboxNow())

	for _, read := range []struct {
		statement string
		widest    int64
	}{{"user_threads", widest}, {"comments", commented}, {"messages", messaged}} {
		if read.widest > oneInboxParticipantBound {
			t.Errorf("stop rule: a thread of the corpus holds %d participants by %s, above R4's bound of %d",
				read.widest, read.statement, oneInboxParticipantBound)
		}
	}
}

// TestOneInboxWriteBaseline is measurement two: the build of BASELINE_SHA, checked out as base-p99
// under a directory of the test's own in TMPDIR (testtmp.Dir) and booted over
// $ISSUE_P99_DATABASE_URL, which it migrates; the corpus seeded into that database once the
// migration has run, through its pool and through the base build's API; and every write path R11
// names timed against it, 200 readings each.
//
// The database runs with jit = off, set before the base build boots over it (turnJITOff) and read
// back on a new session before the first reading, which the first measurement-two: line prints with
// pg_jit_available (turnJITOff says why).
func TestOneInboxWriteBaseline(t *testing.T) {
	t.Parallel()
	if os.Getenv(dismissedScaleEnv) != "1" {
		t.Skipf("set %s=1 to take the one-inbox write baseline", dismissedScaleEnv)
	}
	databaseURL := baselineDatabaseURL(t, oneInboxDatabaseEnv, oneInboxDatabase)
	turnJITOff(t, "the base build's database", databaseURL)
	binary, commit := buildBaseline(t, filepath.Join(testtmp.Dir(t, oneInboxWorktree), oneInboxWorktree))
	server := bootBaseline(t, binary, databaseURL)
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open the database %s names: %v", oneInboxDatabaseEnv, err)
	}
	t.Cleanup(database.Pool.Close)

	base := &oneInboxSide{name: "the base build", server: server, database: database}
	base.corpus = seedOneInboxCorpus(t, database, server)
	settleOneInbox(t, base, oneInboxOwedWork)
	analyzeOneInbox(t, base)
	jit := requireJITOff(t, "the base build's database", databaseURL)

	t.Logf("measurement-two: base build %s, %d readings of each write path after one not counted, its database read on a new "+
		"session: %s, taken_at=%s", commit, oneInboxReadings, jit, oneInboxNow())
	for _, path := range oneInboxWritePaths() {
		measureOneInboxPath(t, path, base)
	}
	installOneInboxAudit(t, database)
	countOneInboxCommentRows(t, "measurement-two", base)
}

// oneInboxSide is one build a measurement drives: the server it calls over loopback, the database
// behind that server, and the corpus seeded on it.
type oneInboxSide struct {
	name     string
	server   *baselineServer
	database *store.Store
	corpus   *oneInboxCorpus
}

// analyzeOneInbox gives side's corpus the statistics a production table has, and the visibility
// map a freshly loaded table lacks: without them the planner reads the corpus as the empty tables
// it was migrated as.
func analyzeOneInbox(t *testing.T, side *oneInboxSide) {
	t.Helper()
	started := time.Now()
	if _, err := side.database.Pool.Exec(context.Background(), "vacuum analyze"); err != nil {
		t.Fatalf("vacuum and analyze %s's corpus: %v", side.name, err)
	}
	t.Logf("vacuumed and analyzed %s's corpus in %s", side.name, time.Since(started).Round(time.Millisecond))
}

// handlerCaller sends the corpus's requests to this build's handler in the process: a person by
// the X-Dispatch-User header newTestServer trusts, and the agent bearer by its shared token.
type handlerCaller struct {
	handler http.Handler
}

func (c handlerCaller) call(t *testing.T, login, method, path string, body any) apiAnswer {
	t.Helper()
	started := time.Now()
	var response *httptest.ResponseRecorder
	if login == "" {
		response = bearerRequest(t, c.handler, method, path, body)
	} else {
		response = dispatchRequest(t, c.handler, method, path, body, login)
	}
	return apiAnswer{status: response.Code, body: response.Body.Bytes(), took: time.Since(started)}
}

// settleOneInbox waits for side's process to drain what the corpus owes its background work, as
// owed counts it (oneInboxOwedWork, oneInboxOwedWorkHere), saying how much is left once a minute.
func settleOneInbox(t *testing.T, side *oneInboxSide, owed string) {
	t.Helper()
	started := time.Now()
	said := started
	for {
		var grammar, texts, items int64
		if err := side.database.Pool.QueryRow(context.Background(), owed).Scan(&grammar, &texts, &items); err != nil {
			t.Fatalf("read what %s owes the corpus: %v", side.name, err)
		}
		if grammar+texts+items == 0 {
			t.Logf("%s drained what the corpus owed it in %s", side.name, time.Since(started).Round(time.Second))
			return
		}
		if time.Since(started) > oneInboxSettle {
			t.Fatalf("after %s %s still owes %d block-grammar passes, %d grammar passes and %d item block indexes",
				oneInboxSettle, side.name, grammar, texts, items)
		}
		if time.Since(said) >= time.Minute {
			t.Logf("%s still owes %d block-grammar passes, %d grammar passes and %d item block indexes",
				side.name, grammar, texts, items)
			said = time.Now()
		}
		time.Sleep(2 * time.Second)
	}
}

// oneInboxPath is one write path a measurement times, on whichever build side names: its name in
// the printed line, one reading of it, and what the line prints beside the times, read once every
// reading is in. A reading puts back off the clock whatever it moved that the next reading would
// otherwise find moved, or writes to a target of its own the seed made one of per reading, so every
// reading measures the same write, and each build's readings write to its own corpus's targets.
// many marks a path that moves more than one ask a reading, whose times c-p99 prints with the
// count it moved and holds to no bound (R11): putProjectDefaultAssignee's move of many asks,
// project-default-assignee. Its move of one ask, project-default-assignee-one-ask, is no such path:
// c-p99 holds it, as every path not many, to its bound in oneInboxWriteBounds.
type oneInboxPath struct {
	name   string
	run    func(t *testing.T, side *oneInboxSide, reading int) time.Duration
	beside func(t *testing.T, side *oneInboxSide) string
	many   bool
}

// oneInboxWritePaths are the write paths R11 names, each found in the route table by its handler,
// in the order they are measured: a comment and a mention of a session and of a person
// (createComment), an issue message (createMessage), a broadcast (createBroadcast), a session's
// direct reply (replyMessage), an ask's answer (answerAsk), a dismissal (dismissInbox), Mark all
// read (markInboxRead), a thread read (putThreadRead), an agent's Clear and read
// (putUserAgentState), a snooze (putAskSnooze), and the three writes that move the person an ask
// waits on: patchIssue and putAskAskedOf, each over one ask, and putProjectDefaultAssignee, over
// the many asks that fall through to one project's default and over the one ask that falls through
// to another's. The inbox writes are the heavy person's, over their own history. The mention of a
// person names the heavy person, whose node holds the corpus's largest set of deliveries, and
// delivers to them as it mentions them.
func oneInboxWritePaths() []oneInboxPath {
	issue := func(side *oneInboxSide) string { return "/api/v1/issues/" + side.corpus.issue }
	return []oneInboxPath{
		{name: "comment", run: func(t *testing.T, side *oneInboxSide, reading int) time.Duration {
			// Into the thread at the bound, by one of its participants, so the participant set the
			// write moves stays the bound's.
			return oneInboxExpect(t, side.name+": a comment at the bound", side.server.call(t, oneInboxCommenter, http.MethodPost,
				issue(side)+"/comments", map[string]any{
					"body": fmt.Sprintf("Reading %d of a comment at the bound.", reading), "reply_to": side.corpus.widestRoot,
				}), http.StatusCreated).took
		}},
		{name: "comment-mention", run: func(t *testing.T, side *oneInboxSide, reading int) time.Duration {
			return oneInboxExpect(t, side.name+": a comment mentioning the session", side.server.call(t, oneInboxSolo, http.MethodPost,
				issue(side)+"/comments", map[string]any{
					"body": fmt.Sprintf("Reading %d of a mention.", reading), "reply_to": side.corpus.soloRoot,
					"mentions": []map[string]string{{"target": "session:" + oneInboxSession}},
				}), http.StatusCreated).took
		}},
		{name: "comment-mention-person", run: func(t *testing.T, side *oneInboxSide, reading int) time.Duration {
			return oneInboxExpect(t, side.name+": a comment mentioning the heavy person", side.server.call(t, oneInboxSolo, http.MethodPost,
				issue(side)+"/comments", map[string]any{
					"body":     fmt.Sprintf("Reading %d of a mention of dispatch://person/%s.", reading, oneInboxHeavy),
					"reply_to": side.corpus.soloRoot,
				}), http.StatusCreated).took
		}},
		{name: "issue-message", run: func(t *testing.T, side *oneInboxSide, reading int) time.Duration {
			return oneInboxExpect(t, side.name+": an issue message", side.server.call(t, oneInboxMessagers[0], http.MethodPost,
				issue(side)+"/messages", map[string]any{
					"body": fmt.Sprintf("Reading %d of an issue message.", reading), "in_reply_to": side.corpus.messageRoot,
				}), http.StatusCreated).took
		}},
		{name: "broadcast", run: func(t *testing.T, side *oneInboxSide, reading int) time.Duration {
			return oneInboxExpect(t, side.name+": a broadcast", side.server.call(t, oneInboxConverser, http.MethodPost, "/api/v1/broadcasts",
				map[string]any{
					"body": fmt.Sprintf("Reading %d of a broadcast.", reading), "delivery": "btw",
					"session_ids": []string{oneInboxSession, oneInboxOtherSession},
				}), http.StatusCreated).took
		}},
		{name: "direct-reply", run: func(t *testing.T, side *oneInboxSide, reading int) time.Duration {
			return oneInboxExpect(t, side.name+": the session's direct reply", side.server.call(t, "", http.MethodPost,
				"/api/v1/messages/"+side.corpus.directRoots[reading]+"/reply", map[string]any{
					"actor": oneInboxSessionActor(), "attempt": 1, "body": fmt.Sprintf("Reading %d of a session's reply.", reading),
				}), http.StatusCreated).took
		}},
		{name: "ask-answer", run: func(t *testing.T, side *oneInboxSide, reading int) time.Duration {
			return oneInboxExpect(t, side.name+": an ask's answer", side.server.call(t, oneInboxAnswerer, http.MethodPost,
				"/api/v1/asks/"+side.corpus.answerAsks[reading]+"/answer", map[string]any{
					"text": fmt.Sprintf("Reading %d of an answer.", reading), "expected_edited_at": nil,
				}), http.StatusOK).took
		}},
		{name: "dismissal", run: func(t *testing.T, side *oneInboxSide, _ int) time.Duration {
			// A thread the heavy person has read, so the dismissal moves no read mark, put back in
			// their Inbox afterwards. The version it carries is read first, as a page carries it.
			rows := []string{inboxDismissReplyPrefix + side.corpus.heavyRead[0]}
			body := dismissBody(t, side.database, oneInboxHeavy, rows)
			answer := oneInboxExpect(t, side.name+": a dismissal", side.server.call(t, oneInboxHeavy, http.MethodPost,
				"/api/v1/me/inbox/dismiss", body), http.StatusOK)
			requireOneInboxCount(t, side.name+": the dismissal", answer, "dismissed", 1)
			restored := oneInboxExpect(t, side.name+": the dismissal's restore", side.server.call(t, oneInboxHeavy, http.MethodPost,
				"/api/v1/me/inbox/dismiss", map[string]any{"threads": rows, "restore": true}), http.StatusOK)
			requireOneInboxCount(t, side.name+": the dismissal's restore", restored, "dismissed", 1)
			return answer.took
		}},
		{name: "mark-all-read", run: func(t *testing.T, side *oneInboxSide, _ int) time.Duration {
			answer := oneInboxExpect(t, side.name+": Mark all read", side.server.call(t, oneInboxHeavy, http.MethodPost,
				"/api/v1/me/inbox/read", map[string]any{"read_through": oneInboxMoment(0), "kind": []string{"reply", "agent"}}),
				http.StatusOK)
			requireOneInboxCount(t, side.name+": Mark all read", answer, "marked", len(side.corpus.heavyUnread))
			unreadOneInbox(t, side.database, side.corpus.heavyUnread)
			return answer.took
		}},
		{name: "thread-read", run: func(t *testing.T, side *oneInboxSide, _ int) time.Duration {
			thread := side.corpus.heavyUnread[0]
			answer := oneInboxExpect(t, side.name+": a thread read", side.server.call(t, oneInboxHeavy, http.MethodPut,
				"/api/v1/me/threads/"+thread+"/read", map[string]any{"read_through": oneInboxMoment(0)}), http.StatusNoContent)
			unreadOneInbox(t, side.database, []string{thread})
			return answer.took
		}},
		{name: "agent-clear-and-read", run: func(t *testing.T, side *oneInboxSide, _ int) time.Duration {
			return oneInboxExpect(t, side.name+": an agent's Clear and read", side.server.call(t, oneInboxConverser, http.MethodPut,
				"/api/v1/me/agents/"+oneInboxSession+"/state", map[string]any{
					"cleared_before": oneInboxMoment(0), "read_through": oneInboxMoment(0),
				}), http.StatusOK).took
		}},
		{name: "snooze", run: func(t *testing.T, side *oneInboxSide, _ int) time.Duration {
			snooze := "/api/v1/me/asks/" + side.corpus.snoozeAsk + "/snooze"
			answer := oneInboxExpect(t, side.name+": a snooze", side.server.call(t, oneInboxHeavy, http.MethodPut, snooze,
				map[string]any{"snoozed_until": oneInboxMoment(24 * time.Hour)}), http.StatusOK)
			oneInboxExpect(t, side.name+": the snooze's undo", side.server.call(t, oneInboxHeavy, http.MethodDelete, snooze, nil),
				http.StatusNoContent)
			return answer.took
		}},
		{name: "patch-issue-assignee", run: func(t *testing.T, side *oneInboxSide, reading int) time.Duration {
			// The seed set the first assignee, so each reading hands the issue, and the one ask it
			// holds, to the other one.
			return oneInboxExpect(t, side.name+": an assignee change", side.server.call(t, oneInboxHeavy, http.MethodPatch,
				"/api/v1/issues/"+side.corpus.movingIssue, map[string]any{"assignee": oneInboxAssignees[(reading+1)%2]}),
				http.StatusOK).took
		}, beside: func(t *testing.T, side *oneInboxSide) string {
			return fmt.Sprintf("asks_moved=%d", oneInboxAsksWaitingOn(t, side.database, `a.id = $1 and a.asked_of is null`,
				side.corpus.movingAsk, oneInboxAssignees[(oneInboxReadings+1)%2]))
		}},
		{name: "ask-asked-of", run: func(t *testing.T, side *oneInboxSide, reading int) time.Duration {
			// The ask waits on its issue's assignee, so each reading hands it to one of two people who
			// are not, in turn.
			return oneInboxExpect(t, side.name+": an ask handed on", side.server.call(t, oneInboxHeavy, http.MethodPut,
				"/api/v1/asks/"+side.corpus.movingAsk+"/asked-of", map[string]any{"asked_of": oneInboxAskedOf[reading%2]}),
				http.StatusOK).took
		}, beside: func(t *testing.T, side *oneInboxSide) string {
			return fmt.Sprintf("asks_moved=%d", oneInboxAsksWaitingOn(t, side.database, `a.id = $1 and a.asked_of is not null`,
				side.corpus.movingAsk, oneInboxAskedOf[oneInboxReadings%2]))
		}},
		{name: "project-default-assignee", many: true, run: func(t *testing.T, side *oneInboxSide, reading int) time.Duration {
			return oneInboxExpect(t, side.name+": a project's default moved", side.server.call(t, oneInboxHeavy, http.MethodPut,
				"/api/v1/projects/"+side.corpus.moveProject+"/default-assignee",
				map[string]any{"assignee": oneInboxDefaults[(reading+1)%2]}), http.StatusOK).took
		}, beside: func(t *testing.T, side *oneInboxSide) string {
			moved := oneInboxAsksWaitingOn(t, side.database, `p.key = $1 and a.asked_of is null and i.assignee is null`,
				side.corpus.moveProject, oneInboxDefaults[(oneInboxReadings+1)%2])
			if moved != oneInboxDefaultAsks {
				t.Fatalf("%s: the project's default moved %d asks, want the %d nobody holds", side.name, moved, oneInboxDefaultAsks)
			}
			return fmt.Sprintf("asks_moved=%d", moved)
		}},
		{name: "project-default-assignee-one-ask", run: func(t *testing.T, side *oneInboxSide, reading int) time.Duration {
			return oneInboxExpect(t, side.name+": a project's default moved over one ask", side.server.call(t, oneInboxHeavy, http.MethodPut,
				"/api/v1/projects/"+side.corpus.moveOneProject+"/default-assignee",
				map[string]any{"assignee": oneInboxDefaults[(reading+1)%2]}), http.StatusOK).took
		}, beside: func(t *testing.T, side *oneInboxSide) string {
			moved := oneInboxAsksWaitingOn(t, side.database, `p.key = $1 and a.asked_of is null and i.assignee is null`,
				side.corpus.moveOneProject, oneInboxDefaults[(oneInboxReadings+1)%2])
			if moved != 1 {
				t.Fatalf("%s: the project's default moved %d asks, want the one nobody holds", side.name, moved)
			}
			return fmt.Sprintf("asks_moved=%d", moved)
		}},
	}
}

// measureOneInboxPath takes one reading of path on side that is not counted - the first run of a
// statement on a fresh connection pays for the pages every later run finds cached - then
// oneInboxReadings that are, and prints their p50, p95 and p99, each the nearest-rank reading so
// every number is a run that happened.
func measureOneInboxPath(t *testing.T, path oneInboxPath, side *oneInboxSide) {
	t.Helper()
	path.run(t, side, 0)
	taken := make([]time.Duration, 0, oneInboxReadings)
	for reading := 1; reading <= oneInboxReadings; reading++ {
		taken = append(taken, path.run(t, side, reading))
	}
	beside := ""
	if path.beside != nil {
		beside = " " + path.beside(t, side)
	}
	figures, _ := oneInboxPercentiles(taken)
	t.Logf("measurement-two: %s %s n=%d%s taken_at=%s", path.name, figures, len(taken), beside, oneInboxNow())
}

// oneInboxAsksWaitingOn counts the open asks narrowed by where (over the ask a, its issue i and
// its project p, with $1 its argument) that wait on person, read through the one expression that
// decides who an ask waits on: a write that moved them names that person now.
func oneInboxAsksWaitingOn(t *testing.T, database *store.Store, where string, argument any, person string) int {
	t.Helper()
	var waiting int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from asks a`+askedOfPersonJoins+`
		where a.state = 'open' and `+where+` and `+askedOfPersonExpression+` = $2
	`, argument, person).Scan(&waiting); err != nil {
		t.Fatalf("count the asks waiting on %s: %v", person, err)
	}
	if waiting == 0 {
		t.Fatalf("no open ask waits on %s once the write that moves it to them has run", person)
	}
	return waiting
}

// unreadOneInbox puts the heavy person's read marks on threads back where the seed left them,
// before each thread's newest reply, so the next reading finds them unread again.
func unreadOneInbox(t *testing.T, database *store.Store, threads []string) {
	t.Helper()
	tag, err := database.Pool.Exec(context.Background(), `
		update user_threads set read_through = replied_at - interval '1 minute'
		where login = $1 and thread_id = any($2::uuid[])
	`, oneInboxHeavy, threads)
	if err != nil {
		t.Fatalf("put the read marks back: %v", err)
	}
	if tag.RowsAffected() != int64(len(threads)) {
		t.Fatalf("put back %d read marks, want %d", tag.RowsAffected(), len(threads))
	}
}

// requireOneInboxCount fails the test unless the write's answer counts want in field.
func requireOneInboxCount(t *testing.T, what string, answer apiAnswer, field string, want int) {
	t.Helper()
	counted := map[string]any{}
	if err := json.Unmarshal(answer.body, &counted); err != nil {
		t.Fatalf("decode %s's answer: %v: %s", what, err, answer.body)
	}
	if got, _ := counted[field].(float64); int(got) != want {
		t.Fatalf("%s answered %s=%v, want %d: %s", what, field, counted[field], want, answer.body)
	}
}

// oneInboxWritten is one table's rows that one operation of one transaction wrote, as the audit
// below recorded them.
type oneInboxWritten struct {
	relation, operation string
	rows                int64
}

// countOneInboxCommentRows writes one comment into side's widest thread and counts the rows its
// transaction wrote: the per-participant ones - its user_threads rows, and the tuples it appended
// that name a participant of the thread other than its author (the delivered_to of R4's append,
// and the end of a dismissal of theirs) - and the fixed ones, every other row, each printed apart
// under prefix. It fails where the per-participant rows pass two for each participant of the
// thread, the append and the update R4 allows; that failure is the stop rule's. The author's own
// tuples - authored_by, their participates_in and their read mark - are one each whatever the
// thread holds, so they are fixed rows.
//
// The rows are counted by an audit installed on side's database before this write
// (installOneInboxAudit), because a row's xmin alone undercounts: the base build's background work
// rewrites rows the write left - the outbox its event, the block index its owed marks - moments
// after it commits.
func countOneInboxCommentRows(t *testing.T, prefix string, side *oneInboxSide) {
	t.Helper()
	ctx := context.Background()
	database := side.database
	var before int64
	if err := database.Pool.QueryRow(ctx, `select coalesce(max(seq), 0) from graph_tuples`).Scan(&before); err != nil {
		t.Fatalf("%s: read the store's last tuple before the comment: %v", side.name, err)
	}
	id := oneInboxID(t, side.name+": the comment whose rows are counted", side.server.call(t, oneInboxCommenter, http.MethodPost,
		"/api/v1/issues/"+side.corpus.issue+"/comments",
		map[string]any{"body": "The comment whose rows are counted.", "reply_to": side.corpus.widestRoot}), http.StatusCreated)
	var xid int64
	if err := database.Pool.QueryRow(ctx, `select xmin::text::bigint from comments where id = $1`, id).Scan(&xid); err != nil {
		t.Fatalf("%s: read the transaction that wrote the comment: %v", side.name, err)
	}
	var participants, carried, tuples int64
	if err := database.Pool.QueryRow(ctx, `
		with participants as (select lower(login) as login from user_threads where thread_id = $1)
		select (select count(*) from participants),
		       (select count(*) from user_threads where thread_id = $1 and xmin::text::bigint = $2),
		       (select count(*) from graph_tuples t
		        where t.seq > $3 and t.xmin::text::bigint = $2
		          and ((t.to_kind = $4 and t.to_id <> $5 and t.to_id in (select login from participants))
		            or (t.from_kind = $4 and t.from_id <> $5 and t.from_id in (select login from participants))))
	`, side.corpus.widestRoot, xid, before, graphstore.KindPerson, oneInboxCommenter).Scan(&participants, &carried, &tuples); err != nil {
		t.Fatalf("%s: read the widest thread's rows and the tuples the comment appended: %v", side.name, err)
	}
	var threads, fixed, comments int64
	var parts []string
	for _, part := range oneInboxWrittenBy(t, database, xid) {
		rows := part.rows
		switch {
		case part.relation == "user_threads":
			threads += rows
			continue
		case part.relation == "graph_tuples" && part.operation == "INSERT":
			rows -= tuples
		case part.relation == "comments" && part.operation == "INSERT":
			comments += rows
		}
		if rows > 0 {
			fixed += rows
			parts = append(parts, fmt.Sprintf("%s.%s=%d", part.relation, strings.ToLower(part.operation), rows))
		}
	}
	if comments != 1 {
		t.Fatalf("%s: transaction %d inserted %d comments by the audit, want the one written: it is not the comment's", side.name, xid, comments)
	}
	if carried != threads {
		t.Fatalf("%s: the comment wrote %d user_threads rows by the audit and %d of the widest thread's rows carry its transaction",
			side.name, threads, carried)
	}
	perParticipant := threads + tuples
	t.Logf("%s: rows of one comment in the thread of %d participants on %s: per_participant=%d (user_threads=%d tuples=%d) "+
		"fixed=%d (%s) bound=%d taken_at=%s",
		prefix, participants, side.name, perParticipant, threads, tuples, fixed, strings.Join(parts, " "), 2*participants, oneInboxNow())
	if perParticipant > 2*participants {
		t.Errorf("stop rule: one comment in the thread of %d participants on %s wrote %d per-participant rows, above two for each",
			participants, side.name, perParticipant)
	}
}

// oneInboxWrittenBy is every table's rows each operation of transaction xid wrote, as the audit
// recorded them (installOneInboxAudit).
func oneInboxWrittenBy(t *testing.T, database *store.Store, xid int64) []oneInboxWritten {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select relation, operation, sum(rows)::bigint from one_inbox_written where xid = $1 group by 1, 2 order by 1, 2
	`, xid)
	if err != nil {
		t.Fatalf("read the rows transaction %d wrote: %v", xid, err)
	}
	written, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (oneInboxWritten, error) {
		var part oneInboxWritten
		err := row.Scan(&part.relation, &part.operation, &part.rows)
		return part, err
	})
	if err != nil {
		t.Fatalf("read the rows transaction %d wrote: %v", xid, err)
	}
	return written
}

// installOneInboxAudit records, for every statement that writes a table of the schema, the
// transaction it ran in, the table, the operation and how many rows it wrote, through a statement
// trigger reading the statement's transition table. It is installed over a build's database after
// the timings, so no reading pays for it, and removed when the test ends.
func installOneInboxAudit(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	exec := func(statement string) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("install the audit: %s: %v", statement, err)
		}
	}
	exec(`create table one_inbox_written (xid bigint not null, relation text not null, operation text not null, rows bigint not null)`)
	exec(`
		create function one_inbox_written() returns trigger language plpgsql as $$
		declare
			counted bigint;
		begin
			select count(*) into counted from written;
			if counted > 0 then
				insert into one_inbox_written
				values ((pg_current_xact_id()::text::numeric % 4294967296)::bigint, tg_table_name, tg_op, counted);
			end if;
			return null;
		end
		$$`)
	t.Cleanup(func() {
		for _, statement := range []string{`drop function one_inbox_written() cascade`, `drop table one_inbox_written`} {
			if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
				t.Errorf("remove the audit: %s: %v", statement, err)
			}
		}
	})
	tables := oneInboxStrings(t, database, `
		select c.relname::text from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = 'public' and c.relkind in ('r', 'p') and not c.relispartition and c.relname <> 'one_inbox_written'
		order by 1
	`)
	// One statement a table, each committed on its own, so the audit never holds a lock on one table
	// while it waits for another that the base build's own work holds.
	for _, table := range tables {
		for _, event := range []struct{ operation, transition string }{{"insert", "new"}, {"update", "new"}, {"delete", "old"}} {
			exec(fmt.Sprintf(`create trigger one_inbox_written_%s after %s on %s referencing %s table as written `+
				`for each statement execute function one_inbox_written()`,
				event.operation, event.operation, pgx.Identifier{table}.Sanitize(), event.transition))
		}
	}
}

// oneInboxMoment is now plus ahead, as a write's timestamp field takes it.
func oneInboxMoment(ahead time.Duration) string {
	return time.Now().Add(ahead).UTC().Format(time.RFC3339Nano)
}

// oneInboxNow is the moment a reading was taken, as `date -u +%FT%TZ` prints it.
func oneInboxNow() string {
	return time.Now().UTC().Format(time.RFC3339)
}

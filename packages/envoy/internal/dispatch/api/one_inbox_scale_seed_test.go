//go:build basebuild

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The corpus the check c-p99 names, seeded the way inbox_dismissed_scale_seed_test.go
// seeds a14's: the piles in bulk SQL through the database's own pool, inside transactions, with
// every trigger firing as it does for any insert, and what only a server writes whole (a project,
// an issue, an ask, a message and its delivery) through that server's API. One seed serves every
// measurement over it: this build's handler for the participant set, the base build booted as a
// process for the write baseline, and both builds for c-p99 (one_inbox_p99_test.go), which seeds
// it once on each build's database, so all of them read the same corpus.
//
// Every shape is named, written, and asserted before anything is read off it
// (assertOneInboxCorpus), because a measurement over a seed that did not land is worse than none:
//
//   - the widest thread: one comment thread on one issue in which oneInboxWidestThread distinct
//     people write, so user_threads holds that many rows for its thread_id - R4's bound;
//   - a thread of one participant;
//   - a reply chain exactly oneInboxCommentThreadDepth links long, the depth the outbox's walk up
//     a reply_to chain stops at;
//   - one person whose history (the threads their user_threads rows name, and the conversations
//     they started with a session) is oneInboxHeavyFactor times the median person's, among
//     oneInboxWidestThread ordinary people whose histories sit around that median;
//   - one person with no history at all;
//
// and what each write path the baseline times needs: an issue message thread, open asks on an
// issue whose assignee is set (one per reading, for the answer), an issue holding exactly one open
// ask (patchIssue and putAskAskedOf), a project whose default assignee is the only person
// oneInboxDefaultAsks open asks wait on and another whose default one open ask waits on
// (putProjectDefaultAssignee over many asks and over one), an ask the heavy person snoozes, and a
// session's conversation with a person: one direct root it answered and one per reading it has
// not, which are also where a broadcast and a Clear land.
//
// It fills both tables the one read's plan is held on to scale: comments with the threads above,
// and messages with each person's conversations, a session of their own answering each one-off
// message they sent it (seedOneInboxConversations) - at the 205 messages the threads alone hold,
// the planner chose sequential scans inside the read's message walks. The conversations add 43,980
// more, two to a conversation, and c-p99's scan rule (explainOneInboxRead) fails a run whose read
// still scans messages sequentially. And it holds the two sorts of ask each write that moves who an
// ask waits on is counted over (seedOneInboxMoves): one whose old person also wrote in its thread,
// and one whose old person holds nothing else there.

const (
	// oneInboxParticipantBound is R4's bound on the live participants of one thread, which the
	// stop rule holds the corpus to.
	oneInboxParticipantBound = 1_000
	// oneInboxWidestThread is how many people write in the corpus's widest thread: the bound
	// itself, so a write measured there costs what a write at the bound costs.
	oneInboxWidestThread = oneInboxParticipantBound
	// oneInboxMedianHistory is the median person's history. Ordinary person i holds
	// oneInboxHistory(i) threads, spread evenly on both sides of it, and the heavy person
	// oneInboxHeavyFactor times it: 2,000 threads.
	oneInboxMedianHistory = 20
	oneInboxHeavyFactor   = 100
	// oneInboxCommentThreadDepth is outbox.maxCommentThreadDepth (outbox/publisher.go), which this
	// package cannot read: the constant is unexported and nothing exported carries it, so it is
	// written here once and assertOneInboxCommentThreadDepth holds it to the declaration it copies.
	oneInboxCommentThreadDepth = 32
	// oneInboxReadings is how many readings the baseline takes of each write path: at 200 the 99th
	// percentile is the 198th reading, an order statistic rather than the slowest sample. A path
	// that uses up what it writes to (an ask answered, a delivery replied to) is seeded one more,
	// for the reading taken first and not counted.
	oneInboxReadings = 200
	// oneInboxDefaultAsks is how many open asks the moving project's default assignee is the only
	// person waiting for: more than one, so that write moves several. A count's move of many asks
	// moves as many (seedOneInboxMoves), half of each sort.
	oneInboxDefaultAsks = 10
)

// The corpus's own people, beside the ordinary population oneInboxPerson names. Every one of them is
// a person who has signed in (a people row), so each can be named as an assignee and sign in.
const (
	oneInboxHeavy = "heavy@scale.example"
	oneInboxEmpty = "empty@scale.example"
	oneInboxSolo  = "solo@scale.example"
	oneInboxChain = "chain@scale.example"
	// oneInboxDomain is what every login of the corpus ends in, which is how a statement tells the
	// corpus's people from the ones a test server seeds of its own.
	oneInboxDomain = "@scale.example"
)

// The sessions of the corpus: the one that answers the corpus's threads, asks its questions and
// holds its conversation, a second one a broadcast reaches beside it, and the one the heavy person
// converses with. Ordinary person i converses with a session of their own,
// oneInboxPersonSession(i).
const (
	oneInboxSession      = "scale-agent"
	oneInboxOtherSession = "scale-other"
	oneInboxHeavySession = "scale-heavy"
)

// The ordinary people who act in a write path. Each is one of the population, so the history the
// median is read from is theirs too. oneInboxMedianReader is a median person no write path names,
// the reader of the read measured over the widest thread, which every ordinary person writes in;
// oneInboxMovers are the people the counted moves of who an ask waits on hand asks between.
var (
	oneInboxCommenter    = oneInboxPerson(1)
	oneInboxAssignees    = [2]string{oneInboxPerson(3), oneInboxPerson(4)}
	oneInboxAskedOf      = [2]string{oneInboxPerson(5), oneInboxPerson(6)}
	oneInboxDefaults     = [2]string{oneInboxPerson(7), oneInboxPerson(8)}
	oneInboxAnswerer     = oneInboxPerson(9)
	oneInboxConverser    = oneInboxPerson(10)
	oneInboxMessagers    = [2]string{oneInboxPerson(11), oneInboxPerson(12)}
	oneInboxMedianReader = oneInboxPerson(17)
	oneInboxMovers       = [7]string{
		oneInboxPerson(901), oneInboxPerson(902), oneInboxPerson(903), oneInboxPerson(904),
		oneInboxPerson(905), oneInboxPerson(906), oneInboxPerson(907),
	}
)

// oneInboxPersonSession is the session ordinary person i converses with.
func oneInboxPersonSession(i int) string {
	return fmt.Sprintf("scale-c%04d", i)
}

// oneInboxPerson is ordinary person i's login, as the people seed spells it in SQL.
func oneInboxPerson(i int) string {
	return fmt.Sprintf("p%04d%s", i, oneInboxDomain)
}

// oneInboxHistory is how many threads ordinary person i writes in: the median, moved by an even
// step from -10 to +10 in a cycle of eleven, so the population sits on both sides of the median and
// the median person's history is oneInboxMedianHistory whichever few extra people stand beside it.
// One of those threads is the widest thread, which every ordinary person writes in.
func oneInboxHistory(i int) int {
	return oneInboxMedianHistory + ((i-1)%11-5)*2
}

// oneInboxCaller is the server a corpus is seeded through: this build's handler in the process or
// the base build booted as a process. login empty is the shared agent bearer, whose body names the
// session it acts as.
type oneInboxCaller interface {
	call(t *testing.T, login, method, path string, body any) apiAnswer
}

// oneInboxCorpus is what a seeded corpus holds that a measurement names: the issue the piles sit
// on and each shape's own rows, and the targets of each write path.
type oneInboxCorpus struct {
	issue        string
	widestRoot   string
	soloRoot     string
	chainDeepest string
	messageRoot  string
	// heavyRead and heavyUnread are the heavy person's threads they have and have not read: a
	// dismissal takes a read one, so it moves no read mark, and Mark all read marks every unread one.
	heavyRead   []string
	heavyUnread []string
	answerIssue string
	answerAsks  []string
	movingIssue string
	movingAsk   string
	snoozeAsk   string
	moveProject string
	moveIssue   string
	// moveOneProject is the project whose default the one open ask of its one issue falls through
	// to, so a move of that default moves one ask.
	moveOneProject string
	// answeredRoot is the conversation's root the session has answered; directRoots are the roots
	// it has not, each holding attempt 1, one per reading of the session's reply.
	answeredRoot string
	directRoots  []string
	// moves are the counted moves of who an ask waits on, over the two sorts of ask.
	moves []oneInboxMove
}

// oneInboxMovedAsk is one ask a counted move moves: the person it waits on before the move, and
// the tuple rows its move appends - three where that person also wrote in its thread, whose own
// write keeps their participates_in, four where they hold nothing else there.
type oneInboxMovedAsk struct {
	id, from string
	rows     int
}

// oneInboxMove is one counted move of a write that moves who asks wait on (patchIssue,
// putAskAskedOf, putProjectDefaultAssignee): the request that makes it, the person the asks wait
// on after it, and the asks it moves. many marks the write's move whose statements are held to
// its moves of one ask: a move of many asks, or, for putAskAskedOf, which moves one ask by its
// nature, the move of one ask among many on its issue.
type oneInboxMove struct {
	write, what  string
	method, path string
	body         map[string]any
	to           string
	asks         []oneInboxMovedAsk
	many         bool
}

// seedOneInboxCorpus writes the corpus through database and server and asserts it.
func seedOneInboxCorpus(t *testing.T, database *store.Store, server oneInboxCaller) *oneInboxCorpus {
	t.Helper()
	started := time.Now()
	seedOneInboxPeople(t, database)

	corpus := &oneInboxCorpus{moveProject: "MOVE", moveOneProject: "MOVEONE"}
	createOneInboxProject(t, server, "SCALE", oneInboxHeavy)
	corpus.issue = createOneInboxIssue(t, server, "SCALE", "The corpus every inbox reads", oneInboxHeavy)
	createOneInboxProject(t, server, "WRITE", oneInboxHeavy)
	corpus.answerIssue = createOneInboxIssue(t, server, "WRITE", "Questions the answerer answers", oneInboxAnswerer)
	corpus.movingIssue = createOneInboxIssue(t, server, "WRITE", "One question that changes hands", oneInboxAssignees[0])
	snoozeIssue := createOneInboxIssue(t, server, "WRITE", "A question the heavy person puts off", oneInboxHeavy)
	createOneInboxProject(t, server, corpus.moveProject, oneInboxDefaults[0])
	corpus.moveIssue = createOneInboxIssue(t, server, corpus.moveProject, "Questions nobody holds", oneInboxHeavy)
	createOneInboxProject(t, server, corpus.moveOneProject, oneInboxDefaults[0])
	moveOneIssue := createOneInboxIssue(t, server, corpus.moveOneProject, "The one question nobody holds", oneInboxHeavy)
	// Every issue is created with somebody on it, so each one whose questions fall to its project's
	// default is emptied afterwards, as a person does by hand.
	for _, issue := range []string{corpus.moveIssue, moveOneIssue} {
		oneInboxExpect(t, "unassign "+issue, server.call(t, oneInboxHeavy, http.MethodPatch,
			"/api/v1/issues/"+issue, map[string]any{"assignee": nil}), http.StatusOK)
	}

	corpus.widestRoot = seedOneInboxWidestThread(t, database, corpus.issue)
	corpus.soloRoot = seedOneInboxSoloThread(t, database, corpus.issue)
	corpus.chainDeepest = seedOneInboxChain(t, database, corpus.issue)
	seedOneInboxOrdinaryThreads(t, database, corpus.issue)
	corpus.heavyRead, corpus.heavyUnread = seedOneInboxHeavyThreads(t, database, corpus.issue)
	seedOneInboxConversations(t, database)

	corpus.messageRoot = seedOneInboxMessageThread(t, server, corpus.issue)
	for reading := range oneInboxReadings + 1 {
		corpus.answerAsks = append(corpus.answerAsks, openOneInboxAsk(t, server, corpus.answerIssue,
			fmt.Sprintf("Which way does question %d of the answerer go?", reading)))
	}
	corpus.movingAsk = openOneInboxAsk(t, server, corpus.movingIssue, "Who should hold this question next?")
	corpus.snoozeAsk = openOneInboxAsk(t, server, snoozeIssue, "Can the heavy person look at this later?")
	for index := range oneInboxDefaultAsks {
		openOneInboxAsk(t, server, corpus.moveIssue, fmt.Sprintf("Who takes unheld question %d?", index))
	}
	openOneInboxAsk(t, server, moveOneIssue, "Who takes the one unheld question?")
	corpus.answeredRoot = sendOneInboxDirect(t, server, "The first question the session answered.")
	oneInboxExpect(t, "the session's answer to its first question", server.call(t, "", http.MethodPost,
		"/api/v1/messages/"+corpus.answeredRoot+"/reply", map[string]any{
			"actor": oneInboxSessionActor(), "attempt": 1, "body": "The session's first answer.",
		}), http.StatusCreated)
	for reading := range oneInboxReadings + 1 {
		corpus.directRoots = append(corpus.directRoots,
			sendOneInboxDirect(t, server, fmt.Sprintf("Direct question %d, awaiting the session's reply.", reading)))
	}
	corpus.moves = seedOneInboxMoves(t, server)

	assertOneInboxCorpus(t, database, corpus)
	var comments, messages int64
	if err := database.Pool.QueryRow(context.Background(), `select (select count(*) from comments), (select count(*) from messages)`).
		Scan(&comments, &messages); err != nil {
		t.Fatalf("count the corpus's comments and messages: %v", err)
	}
	t.Logf("seeded the corpus in %s: a widest thread of %d participants, %d ordinary people around a median history of %d "+
		"threads and as many conversations, a heavy person of %d threads (%d unread) and %d conversations, a chain of %d links; "+
		"comments=%d messages=%d",
		time.Since(started).Round(time.Millisecond), oneInboxWidestThread, oneInboxWidestThread, oneInboxMedianHistory,
		len(corpus.heavyRead)+len(corpus.heavyUnread), len(corpus.heavyUnread), oneInboxHeavyFactor*oneInboxMedianHistory,
		oneInboxCommentThreadDepth, comments, messages)
	return corpus
}

// seedOneInboxPeople records every person of the corpus as one who has signed in.
func seedOneInboxPeople(t *testing.T, database *store.Store) {
	t.Helper()
	named := []string{oneInboxHeavy, oneInboxEmpty, oneInboxSolo, oneInboxChain}
	tag, err := database.Pool.Exec(context.Background(), `
		insert into people (email)
		select format('p%s%s', lpad(g::text, 4, '0'), $2::text) from generate_series(1, $1::int) g
		union all
		select unnest($3::text[])
	`, oneInboxWidestThread, oneInboxDomain, named)
	if err != nil {
		t.Fatalf("seed the corpus's people: %v", err)
	}
	if tag.RowsAffected() != int64(oneInboxWidestThread+len(named)) {
		t.Fatalf("seeded %d people, want %d", tag.RowsAffected(), oneInboxWidestThread+len(named))
	}
}

// createOneInboxProject creates the project key, defaulting to defaultAssignee, as the heavy
// person.
func createOneInboxProject(t *testing.T, server oneInboxCaller, key, defaultAssignee string) {
	t.Helper()
	oneInboxExpect(t, "create project "+key, server.call(t, oneInboxHeavy, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": key, "name": key + " of the one-inbox corpus", "default_assignee": defaultAssignee,
	}), http.StatusCreated)
}

// createOneInboxIssue creates an issue of project held by assignee, as the heavy person, and
// answers its key. It is forced past the near-copy check, which titles of one corpus would trip.
func createOneInboxIssue(t *testing.T, server oneInboxCaller, project, title, assignee string) string {
	t.Helper()
	answer := oneInboxExpect(t, "create issue "+title, server.call(t, oneInboxHeavy, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": project, "title": title, "assignee": assignee, "force": true,
	}), http.StatusCreated)
	var created struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(answer.body, &created); err != nil || created.Key == "" {
		t.Fatalf("read the issue %q back: %v: %s", title, err, answer.body)
	}
	return created.Key
}

// seedOneInboxWidestThread writes the thread at the bound: a root by the first ordinary person and
// one reply by each of the others, a second apart. Each reply's trigger moves every earlier
// participant's row (user_threads_record), so the rows are the ones a thread grown one writer at a
// time holds. The replies commit fifty to a transaction: inside one transaction every earlier
// version of a row stays live to the index scan each later reply's update makes, and the thread's
// half a million updates then take minutes rather than seconds.
func seedOneInboxWidestThread(t *testing.T, database *store.Store, issue string) string {
	t.Helper()
	const batch = 50
	ctx := context.Background()
	started := time.Now()
	var root string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body, created_at)
		values ($1, jsonb_build_object('kind', 'user', 'id', $2::text), 'The thread every ordinary person writes in.',
		        now() - interval '2 days')
		returning id::text
	`, issue, oneInboxPerson(1)).Scan(&root); err != nil {
		t.Fatalf("seed the widest thread's root: %v", err)
	}
	for first := 2; first <= oneInboxWidestThread; first += batch {
		last := min(first+batch-1, oneInboxWidestThread)
		tag, err := database.Pool.Exec(ctx, `
			insert into comments (issue_key, author, body, reply_to, created_at)
			select $1, jsonb_build_object('kind', 'user', 'id', format('p%s%s', lpad(g::text, 4, '0'), $2::text)),
			       'A reply at the bound, by participant ' || g, $3::uuid,
			       now() - interval '2 days' + (g || ' seconds')::interval
			from generate_series($4::int, $5::int) g
		`, issue, oneInboxDomain, root, first, last)
		if err != nil {
			t.Fatalf("seed the widest thread's replies %d-%d: %v", first, last, err)
		}
		if tag.RowsAffected() != int64(last-first+1) {
			t.Fatalf("seeded %d of the widest thread's replies %d-%d, want %d", tag.RowsAffected(), first, last, last-first+1)
		}
	}
	t.Logf("seeded the widest thread in %s", time.Since(started).Round(time.Millisecond))
	return root
}

// seedOneInboxSoloThread writes a thread of one participant: a root and a reply, both the solo
// person's, so the thread holds a reply and one user_threads row.
func seedOneInboxSoloThread(t *testing.T, database *store.Store, issue string) string {
	t.Helper()
	var root string
	oneInboxTransaction(t, database, "the thread of one", func(tx pgx.Tx) error {
		if err := tx.QueryRow(context.Background(), `
			insert into comments (issue_key, author, body, created_at)
			values ($1, jsonb_build_object('kind', 'user', 'id', $2::text), 'A thread nobody else writes in.', now() - interval '1 day')
			returning id::text
		`, issue, oneInboxSolo).Scan(&root); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `
			insert into comments (issue_key, author, body, reply_to, created_at)
			values ($1, jsonb_build_object('kind', 'user', 'id', $2::text), 'Still nobody else.', $3::uuid,
			        now() - interval '1 day' + interval '1 minute')
		`, issue, oneInboxSolo, root)
		return err
	})
	return root
}

// seedOneInboxChain writes a reply chain oneInboxCommentThreadDepth links long: a root and that
// many comments, each replying to the one before it rather than to the root, as legacy nested rows
// do, by the chain person and the session in turn. Its deepest comment is the deepest one whose
// root the outbox's walk up the chain still reaches. It answers that deepest comment.
func seedOneInboxChain(t *testing.T, database *store.Store, issue string) string {
	t.Helper()
	var parent *string
	oneInboxTransaction(t, database, "the reply chain", func(tx pgx.Tx) error {
		for depth := 0; depth <= oneInboxCommentThreadDepth; depth++ {
			author := map[string]string{"kind": "user", "id": oneInboxChain}
			if depth%2 == 1 {
				author = oneInboxSessionActor()
			}
			var id string
			if err := tx.QueryRow(context.Background(), `
				insert into comments (issue_key, author, body, reply_to, created_at)
				values ($1, $2, 'A link of the chain, ' || $3::int || ' deep.', $4::uuid,
				        now() - interval '3 days' + ($3::int || ' minutes')::interval)
				returning id::text
			`, issue, author, depth, parent).Scan(&id); err != nil {
				return err
			}
			parent = &id
		}
		return nil
	})
	return *parent
}

// seedOneInboxOrdinaryThreads writes each ordinary person's own threads beside the widest one: a
// root of theirs answered by the session, so each is a thread in their Inbox, and their history is
// oneInboxHistory(i). It writes a hundred people a statement, in one transaction.
func seedOneInboxOrdinaryThreads(t *testing.T, database *store.Store, issue string) {
	t.Helper()
	const batch = 100
	oneInboxTransaction(t, database, "the ordinary threads", func(tx pgx.Tx) error {
		for first := 1; first <= oneInboxWidestThread; first += batch {
			var logins []string
			var owned []int
			want := 0
			for i := first; i <= min(first+batch-1, oneInboxWidestThread); i++ {
				logins = append(logins, oneInboxPerson(i))
				// The widest thread is one of their history already.
				owned = append(owned, oneInboxHistory(i)-1)
				want += oneInboxHistory(i) - 1
			}
			tag, err := tx.Exec(context.Background(), `
				with people as (
					select login, threads, rank
					from unnest($2::text[], $3::int[]) with ordinality as people(login, threads, rank)
				),
				roots as (
					insert into comments (issue_key, author, body, created_at)
					select $1, jsonb_build_object('kind', 'user', 'id', people.login),
					       'A thread of ' || people.login || ', number ' || j,
					       now() - ((($4::int + people.rank) * 60 + j) || ' minutes')::interval
					from people, generate_series(1, people.threads) j
					returning id, created_at
				)
				insert into comments (issue_key, author, body, reply_to, created_at)
				select $1, jsonb_build_object('kind', 'session', 'id', $5::text), 'The session answers.', roots.id,
				       roots.created_at + interval '30 seconds'
				from roots
			`, issue, logins, owned, first-1, oneInboxSession)
			if err != nil {
				return fmt.Errorf("people %d-%d: %w", first, first+len(logins)-1, err)
			}
			if tag.RowsAffected() != int64(want) {
				return fmt.Errorf("people %d-%d: wrote %d threads, want %d", first, first+len(logins)-1, tag.RowsAffected(), want)
			}
		}
		return nil
	})
}

// seedOneInboxHeavyThreads writes the heavy person's history: oneInboxHeavyFactor times the
// median, each a root of theirs the session answered, and marks the older half read, as a person
// who has read some of their Inbox has it. It answers the read threads and the unread ones.
func seedOneInboxHeavyThreads(t *testing.T, database *store.Store, issue string) (read, unread []string) {
	t.Helper()
	threads := oneInboxHeavyFactor * oneInboxMedianHistory
	oneInboxTransaction(t, database, "the heavy person's threads", func(tx pgx.Tx) error {
		tag, err := tx.Exec(context.Background(), `
			with roots as (
				insert into comments (issue_key, author, body, created_at)
				select $1, jsonb_build_object('kind', 'user', 'id', $2::text), 'A thread of the heavy person, number ' || g,
				       now() - ((2 * g + 1) || ' minutes')::interval
				from generate_series(1, $3::int) g
				returning id, created_at
			)
			insert into comments (issue_key, author, body, reply_to, created_at)
			select $1, jsonb_build_object('kind', 'session', 'id', $4::text), 'The session answers the heavy person.', roots.id,
			       roots.created_at + interval '1 minute'
			from roots
		`, issue, oneInboxHeavy, threads, oneInboxSession)
		if err == nil && tag.RowsAffected() != int64(threads) {
			err = fmt.Errorf("wrote %d threads, want %d", tag.RowsAffected(), threads)
		}
		if err != nil {
			return err
		}
		tag, err = tx.Exec(context.Background(), `
			update user_threads set read_through = replied_at
			where login = $1 and thread_id in (
				select thread_id from user_threads where login = $1 order by replied_at, thread_id limit $2::int
			)
		`, oneInboxHeavy, threads/2)
		if err == nil && tag.RowsAffected() != int64(threads/2) {
			err = fmt.Errorf("read %d threads, want %d", tag.RowsAffected(), threads/2)
		}
		return err
	})
	read = oneInboxStrings(t, database, `
		select thread_id::text from user_threads where login = $1 and not unread order by thread_id
	`, oneInboxHeavy)
	unread = oneInboxStrings(t, database, `
		select thread_id::text from user_threads where login = $1 and unread order by thread_id
	`, oneInboxHeavy)
	return read, unread
}

// seedOneInboxConversations writes each person's conversations with a session of their own: as
// many one-off messages from ordinary person i to oneInboxPersonSession(i) as their history holds
// threads (oneInboxHistory(i)), and oneInboxHeavyFactor times the median from the heavy person to
// oneInboxHeavySession, each answered by that session thirty seconds later, the answer carrying its
// root's target as a session's reply does (insertSessionReply); then the person's read mark on that
// session (user_agent_read), at now, as a person who has read their conversations has it. It writes
// a hundred people a statement, in one transaction.
//
// Each message is owed a block index and its embedding windows by its insert trigger (migrations
// 0144 and 20261107194001), which a build's process pays off a batch a tick and this build's
// handler in the process never does. The seed takes both debts off the messages it wrote, in its
// own transaction, so neither build holds the blocks or windows of a body no read under measurement
// reads, and each build's process settles (settleOneInbox) on what the threads owe it alone rather
// than on 40,000 more bodies: with the window debt left on them, the first c-p99 run found the base
// build paying it off at about 2,400 items a minute.
func seedOneInboxConversations(t *testing.T, database *store.Store) {
	t.Helper()
	const batch = 100
	var logins, sessions []string
	var conversations, newest, step []int
	for i := 1; i <= oneInboxWidestThread; i++ {
		logins = append(logins, oneInboxPerson(i))
		sessions = append(sessions, oneInboxPersonSession(i))
		conversations = append(conversations, oneInboxHistory(i))
		newest, step = append(newest, i*60+30), append(step, 1)
	}
	logins = append(logins, oneInboxHeavy)
	sessions = append(sessions, oneInboxHeavySession)
	conversations = append(conversations, oneInboxHeavyFactor*oneInboxMedianHistory)
	newest, step = append(newest, 0), append(step, 2)
	ctx := context.Background()
	oneInboxTransaction(t, database, "the conversations", func(tx pgx.Tx) error {
		for first := 0; first < len(logins); first += batch {
			last := min(first+batch, len(logins))
			want := 0
			for _, count := range conversations[first:last] {
				want += 2 * count
			}
			var written []string
			if err := tx.QueryRow(ctx, `
				with people as (
					select * from unnest($1::text[], $2::text[], $3::int[], $4::int[], $5::int[])
					  as people(login, session, conversations, newest, step)
				),
				roots as (
					insert into messages (issue_key, author, body, target, created_at)
					select null, jsonb_build_object('kind', 'user', 'id', people.login),
					       'Question ' || j || ' of ' || people.login || ' to its session.', 'session:' || people.session,
					       now() - ((people.newest + people.step * j) || ' minutes')::interval - interval '20 seconds'
					from people, generate_series(1, people.conversations) j
					returning id, target, created_at
				),
				replies as (
					insert into messages (issue_key, author, body, target, in_reply_to, created_at)
					select null, jsonb_build_object('kind', 'session', 'id', substr(roots.target, length('session:') + 1)),
					       'The session answers.', roots.target, roots.id, roots.created_at + interval '30 seconds'
					from roots
					returning id
				)
				select array(select id::text from roots) || array(select id::text from replies)
			`, logins[first:last], sessions[first:last], conversations[first:last], newest[first:last], step[first:last]).
				Scan(&written); err != nil {
				return fmt.Errorf("people %d-%d: %w", first+1, last, err)
			}
			if len(written) != want {
				return fmt.Errorf("people %d-%d: wrote %d messages, want %d", first+1, last, len(written), want)
			}
			tag, err := tx.Exec(ctx, `
				update message_block_state set owed_at = null, windows_owed_at = null where message_id = any($1::uuid[])
			`, written)
			if err != nil {
				return fmt.Errorf("people %d-%d: take the block index and windows the messages owe off them: %w", first+1, last, err)
			}
			if tag.RowsAffected() != int64(want) {
				return fmt.Errorf("people %d-%d: took the block index off %d messages, want %d", first+1, last, tag.RowsAffected(), want)
			}
		}
		tag, err := tx.Exec(ctx, `
			insert into user_agent_read (login, session_id, read_through)
			select login, session, now() from unnest($1::text[], $2::text[]) as marks(login, session)
		`, logins, sessions)
		if err == nil && tag.RowsAffected() != int64(len(logins)) {
			err = fmt.Errorf("wrote %d read marks, want %d", tag.RowsAffected(), len(logins))
		}
		return err
	})
}

// seedOneInboxMoves writes, through server, the asks each counted move of who an ask waits on
// moves, over the two sorts of ask - the first, whose old person also wrote in its thread (a
// comment of theirs on the ask), and the second, whose old person holds nothing else there - and
// answers the moves, in the order they are counted: per write, its moves of one ask, then its move
// of many. Every ask is asked by the session, so no old person is its asker.
//
//   - patchIssue moves every open ask of the issue handed to nobody: an issue holding one ask of
//     each sort, then one holding oneInboxDefaultAsks, half of each sort.
//   - putAskAskedOf moves the one ask it hands on, which waits on the person it was handed to
//     before: one ask of each sort, then the second sort's ask on an issue holding
//     oneInboxDefaultAsks, whose count of statements is held to the one ask's.
//   - putProjectDefaultAssignee moves every open ask that falls through to the project's default:
//     a project holding one, then one holding oneInboxDefaultAsks, half of each sort.
func seedOneInboxMoves(t *testing.T, server oneInboxCaller) []oneInboxMove {
	t.Helper()
	movers := oneInboxMovers
	const project = "SORT"
	createOneInboxProject(t, server, project, oneInboxHeavy)
	opened := func(issue string, count int) []string {
		ids := make([]string, 0, count)
		for index := range count {
			ids = append(ids, openOneInboxAsk(t, server, issue, fmt.Sprintf("Which way does counted question %d of %s go?", index, issue)))
		}
		return ids
	}
	// sorted has from write in the thread of the first `first` of asks, which makes them the first
	// sort, and answers each with the rows its move appends.
	sorted := func(issue string, asks []string, from string, first int) []oneInboxMovedAsk {
		moved := make([]oneInboxMovedAsk, 0, len(asks))
		for index, id := range asks {
			rows := 4
			if index < first {
				rows = 3
				oneInboxExpect(t, from+" writes in the thread of ask "+id, server.call(t, from, http.MethodPost,
					"/api/v1/issues/"+issue+"/comments", map[string]any{
						"body": "A word in this question's thread from the person it waits on.", "ask_id": id,
					}), http.StatusCreated)
			}
			moved = append(moved, oneInboxMovedAsk{id: id, from: from, rows: rows})
		}
		return moved
	}
	type counted struct {
		what        string
		asks, first int
		many        bool
	}
	many := fmt.Sprintf("%d asks, half of each sort", oneInboxDefaultAsks)
	var moves []oneInboxMove
	for _, c := range []counted{{"one ask, first sort", 1, 1, false}, {"one ask, second sort", 1, 0, false}, {many, oneInboxDefaultAsks, oneInboxDefaultAsks / 2, true}} {
		issue := createOneInboxIssue(t, server, project, "Counted questions that change hands with their issue: "+c.what, movers[0])
		moved := sorted(issue, opened(issue, c.asks), movers[0], c.first)
		moves = append(moves, oneInboxMove{
			write: "patchIssue", what: c.what, method: http.MethodPatch, path: "/api/v1/issues/" + issue,
			body: map[string]any{"assignee": movers[1]}, to: movers[1], asks: moved, many: c.many,
		})
	}
	for _, c := range []counted{
		{"one ask, first sort", 1, 1, false}, {"one ask, second sort", 1, 0, false},
		{fmt.Sprintf("one ask of an issue holding %d, second sort", oneInboxDefaultAsks), oneInboxDefaultAsks, 0, true},
	} {
		issue := createOneInboxIssue(t, server, project, "Counted questions handed on: "+c.what, movers[2])
		ask := opened(issue, c.asks)[0]
		oneInboxExpect(t, "hand ask "+ask+" to "+movers[3], server.call(t, oneInboxHeavy, http.MethodPut,
			"/api/v1/asks/"+ask+"/asked-of", map[string]any{"asked_of": movers[3]}), http.StatusOK)
		moved := sorted(issue, []string{ask}, movers[3], c.first)
		moves = append(moves, oneInboxMove{
			write: "putAskAskedOf", what: c.what, method: http.MethodPut, path: "/api/v1/asks/" + ask + "/asked-of",
			body: map[string]any{"asked_of": movers[4]}, to: movers[4], asks: moved, many: c.many,
		})
	}
	for index, c := range []counted{{"one ask, second sort", 1, 0, false}, {many, oneInboxDefaultAsks, oneInboxDefaultAsks / 2, true}} {
		key := project + string(rune('A'+index))
		createOneInboxProject(t, server, key, movers[5])
		issue := createOneInboxIssue(t, server, key, "Counted questions nobody holds: "+c.what, oneInboxHeavy)
		oneInboxExpect(t, "unassign "+issue, server.call(t, oneInboxHeavy, http.MethodPatch,
			"/api/v1/issues/"+issue, map[string]any{"assignee": nil}), http.StatusOK)
		moved := sorted(issue, opened(issue, c.asks), movers[5], c.first)
		moves = append(moves, oneInboxMove{
			write: "putProjectDefaultAssignee", what: c.what, method: http.MethodPut,
			path: "/api/v1/projects/" + key + "/default-assignee",
			body: map[string]any{"assignee": movers[6]}, to: movers[6], asks: moved, many: c.many,
		})
	}
	return moves
}

// seedOneInboxMessageThread starts an issue message thread as the first messager and answers it
// as the second, so the thread the baseline's issue messages reply into holds two people.
func seedOneInboxMessageThread(t *testing.T, server oneInboxCaller, issue string) string {
	t.Helper()
	root := oneInboxID(t, "the issue message thread's root", server.call(t, oneInboxMessagers[0], http.MethodPost,
		"/api/v1/issues/"+issue+"/messages", map[string]any{"body": "A message thread of two people."}), http.StatusCreated)
	oneInboxExpect(t, "the issue message thread's reply", server.call(t, oneInboxMessagers[1], http.MethodPost,
		"/api/v1/issues/"+issue+"/messages", map[string]any{"body": "The second person answers.", "in_reply_to": root}),
		http.StatusCreated)
	return root
}

// openOneInboxAsk opens question on issue as the session, which is how an agent asks, and answers
// the ask's id. It waits on whoever the issue's chain names.
func openOneInboxAsk(t *testing.T, server oneInboxCaller, issue, question string) string {
	t.Helper()
	return oneInboxID(t, "open an ask on "+issue, server.call(t, "", http.MethodPost, "/api/v1/issues/"+issue+"/asks",
		map[string]any{"question": question, "actor": oneInboxSessionActor()}), http.StatusCreated)
}

// sendOneInboxDirect sends body to the session as the conversing person, a direct message of no
// issue, and answers its id once its delivery attempt has settled, which is when the route answers.
func sendOneInboxDirect(t *testing.T, server oneInboxCaller, body string) string {
	t.Helper()
	return oneInboxID(t, "a direct message to the session", server.call(t, oneInboxConverser, http.MethodPost,
		"/api/v1/agents/"+oneInboxSession+"/messages", map[string]any{"body": body, "delivery": "btw"}), http.StatusCreated)
}

func oneInboxSessionActor() map[string]string {
	return map[string]string{"kind": "session", "id": oneInboxSession}
}

// assertOneInboxCorpus holds the seed to every shape it names, in the tables the triggers keep and
// the routes wrote, before anything is read off it.
func assertOneInboxCorpus(t *testing.T, database *store.Store, corpus *oneInboxCorpus) {
	t.Helper()
	ctx := context.Background()
	assertOneInboxCommentThreadDepth(t)
	for _, counted := range []struct {
		what      string
		statement string
		args      []any
		want      int64
	}{
		{"user_threads rows of the widest thread", `select count(*) from user_threads where thread_id = $1`,
			[]any{corpus.widestRoot}, oneInboxWidestThread},
		{"people who wrote in the widest thread",
			`select count(distinct author->>'id') from comments where coalesce(ask_id, reply_to, id) = $1`,
			[]any{corpus.widestRoot}, oneInboxWidestThread},
		{"user_threads rows of the thread of one", `select count(*) from user_threads where thread_id = $1`,
			[]any{corpus.soloRoot}, 1},
		{"authors of the thread of one",
			`select count(distinct author->>'id') from comments where coalesce(ask_id, reply_to, id) = $1`,
			[]any{corpus.soloRoot}, 1},
		{"links from the chain's deepest comment to its root", `
			with recursive up as (
				select id, reply_to, 0 as depth from comments where id = $1
				union all
				select c.id, c.reply_to, up.depth + 1 from up join comments c on c.id = up.reply_to
				where up.depth <= $2
			)
			select coalesce(max(depth) filter (where reply_to is null), -1) from up
		`, []any{corpus.chainDeepest, oneInboxCommentThreadDepth}, oneInboxCommentThreadDepth},
		{"people in the issue message thread",
			`select count(distinct author->>'id') from messages where coalesce(in_reply_to, id) = $1`,
			[]any{corpus.messageRoot}, 2},
		{"open asks waiting on the answerer, on an issue the answerer holds", `
			select count(*) from asks a` + askedOfPersonJoins + `
			where a.issue_key = $1 and a.state = 'open' and i.assignee = $2 and ` + askedOfPersonExpression + ` = $2
		`, []any{corpus.answerIssue, oneInboxAnswerer}, oneInboxReadings + 1},
		{"open asks of the issue that changes hands", `select count(*) from asks where issue_key = $1 and state = 'open'`,
			[]any{corpus.movingIssue}, 1},
		{"the ask that changes hands, waiting on the issue's assignee", `
			select count(*) from asks a` + askedOfPersonJoins + `
			where a.id = $1 and a.asked_of is null and ` + askedOfPersonExpression + ` = $2
		`, []any{corpus.movingAsk, oneInboxAssignees[0]}, 1},
		{"the ask the heavy person snoozes, waiting on them", `
			select count(*) from asks a` + askedOfPersonJoins + `
			where a.id = $1 and a.state = 'open' and ` + askedOfPersonExpression + ` = $2
		`, []any{corpus.snoozeAsk, oneInboxHeavy}, 1},
		{"open asks nobody holds, waiting on the project's default", `
			select count(*) from asks a` + askedOfPersonJoins + `
			where p.key = $1 and a.state = 'open' and a.asked_of is null and i.assignee is null
			  and ` + askedOfPersonExpression + ` = $2
		`, []any{corpus.moveProject, oneInboxDefaults[0]}, oneInboxDefaultAsks},
		{"open asks of the moving project", `
			select count(*) from asks a join issues i on i.key = a.issue_key where i.project_key = $1 and a.state = 'open'
		`, []any{corpus.moveProject}, oneInboxDefaultAsks},
		{"open asks nobody holds of the project whose default moves one ask, waiting on its default", `
			select count(*) from asks a` + askedOfPersonJoins + `
			where p.key = $1 and a.state = 'open' and a.asked_of is null and i.assignee is null
			  and ` + askedOfPersonExpression + ` = $2
		`, []any{corpus.moveOneProject, oneInboxDefaults[0]}, 1},
		{"open asks of the project whose default moves one ask", `
			select count(*) from asks a join issues i on i.key = a.issue_key where i.project_key = $1 and a.state = 'open'
		`, []any{corpus.moveOneProject}, 1},
		{"the session's answer to the conversation's first root", `
			select count(*) from message_deliveries d join messages m on m.id = d.message_id
			where m.id = $1 and m.issue_key is null and d.session_id = $2 and d.reply_id is not null
		`, []any{corpus.answeredRoot, oneInboxSession}, 1},
		{"direct roots holding a delivered attempt 1 the session has not replied to", `
			select count(*) from message_deliveries d join messages m on m.id = d.message_id
			where m.id = any($1::uuid[]) and m.issue_key is null and m.in_reply_to is null and d.attempt = 1
			  and d.session_id = $2 and d.state = 'sent' and d.reply_id is null
		`, []any{corpus.directRoots, oneInboxSession}, oneInboxReadings + 1},
		{"the heavy person's unread threads", `select count(*) from user_threads where login = $1 and unread`,
			[]any{oneInboxHeavy}, int64(len(corpus.heavyUnread))},
	} {
		var got int64
		if err := database.Pool.QueryRow(ctx, counted.statement, counted.args...).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", counted.what, err)
		}
		if got != counted.want {
			t.Fatalf("the corpus holds %d %s, want %d", got, counted.what, counted.want)
		}
	}
	if len(corpus.heavyRead) == 0 || len(corpus.heavyUnread) == 0 {
		t.Fatalf("the heavy person holds %d read and %d unread threads, want both", len(corpus.heavyRead), len(corpus.heavyUnread))
	}

	// History is the threads a person's user_threads rows name, read over the corpus's own people
	// alone: a test server seeds people of its own, who would move the median.
	var median, people, heavy, empty int64
	if err := database.Pool.QueryRow(ctx, `
		with histories as (
			select p.email, count(u.thread_id) as n
			from people p left join user_threads u on u.login = p.email
			where p.email like '%' || $1::text
			group by p.email
		)
		select percentile_disc(0.5) within group (order by n), count(*),
		       coalesce(max(n) filter (where email = $2), -1), coalesce(max(n) filter (where email = $3), -1)
		from histories
	`, oneInboxDomain, oneInboxHeavy, oneInboxEmpty).Scan(&median, &people, &heavy, &empty); err != nil {
		t.Fatalf("read the corpus's histories: %v", err)
	}
	t.Logf("the corpus's %d people: median history %d threads, the heavy person %d, the empty one %d", people, median, heavy, empty)
	if median != oneInboxMedianHistory {
		t.Fatalf("the median person's history is %d threads, want %d", median, oneInboxMedianHistory)
	}
	if heavy != oneInboxHeavyFactor*median {
		t.Fatalf("the heavy person's history is %d threads, want %d times the median's %d", heavy, oneInboxHeavyFactor, median)
	}
	if empty != 0 {
		t.Fatalf("the person with no history holds %d threads (-1 is no such person), want 0", empty)
	}

	// Conversations are the one-off messages a person sent a session, read over the same people
	// (messages_direct_roots' own predicate), and the median reader holds the median of both.
	var conversations, heavyConversations, emptyConversations, readerConversations, readerThreads int64
	if err := database.Pool.QueryRow(ctx, `
		with conversations as (
			select p.email, count(m.id) as n
			from people p
			left join messages m on m.issue_key is null and m.in_reply_to is null and m.target like 'session:%'
			  and m.author->>'kind' = 'user' and lower(m.author->>'id') = p.email
			where p.email like '%' || $1::text
			group by p.email
		)
		select percentile_disc(0.5) within group (order by n),
		       coalesce(max(n) filter (where email = $2), -1), coalesce(max(n) filter (where email = $3), -1),
		       coalesce(max(n) filter (where email = $4), -1), (select count(*) from user_threads where login = $4)
		from conversations
	`, oneInboxDomain, oneInboxHeavy, oneInboxEmpty, oneInboxMedianReader).
		Scan(&conversations, &heavyConversations, &emptyConversations, &readerConversations, &readerThreads); err != nil {
		t.Fatalf("read the corpus's conversations: %v", err)
	}
	t.Logf("the corpus's conversations: median %d, the heavy person %d, the empty one %d; the median reader %s holds %d threads and %d conversations",
		conversations, heavyConversations, emptyConversations, oneInboxMedianReader, readerThreads, readerConversations)
	switch {
	case conversations != oneInboxMedianHistory:
		t.Fatalf("the median person holds %d conversations, want %d", conversations, oneInboxMedianHistory)
	case heavyConversations != oneInboxHeavyFactor*conversations:
		t.Fatalf("the heavy person holds %d conversations, want %d times the median's %d", heavyConversations, oneInboxHeavyFactor, conversations)
	case emptyConversations != 0:
		t.Fatalf("the person with no history holds %d conversations (-1 is no such person), want 0", emptyConversations)
	case readerThreads != oneInboxMedianHistory || readerConversations != oneInboxMedianHistory:
		t.Fatalf("the median reader %s holds %d threads and %d conversations, want %d of each",
			oneInboxMedianReader, readerThreads, readerConversations, oneInboxMedianHistory)
	}

	// Each counted move's asks are open and wait on the person the move takes them from, and that
	// person wrote in the thread of each ask of the first sort and of no ask of the second.
	for _, move := range corpus.moves {
		for _, ask := range move.asks {
			var waiting *string
			var wrote int64
			if err := database.Pool.QueryRow(ctx, `
				select `+askedOfPersonExpression+`,
				       (select count(*) from comments c where c.ask_id = a.id and c.author->>'id' = $2)
				from asks a`+askedOfPersonJoins+`
				where a.id = $1 and a.state = 'open'
			`, ask.id, ask.from).Scan(&waiting, &wrote); err != nil {
				t.Fatalf("read ask %s of %s's move %q: %v", ask.id, move.write, move.what, err)
			}
			if waiting == nil || *waiting != ask.from {
				t.Fatalf("ask %s of %s's move %q waits on %v, want %s", ask.id, move.write, move.what, waiting, ask.from)
			}
			if (wrote > 0) != (ask.rows == 3) {
				t.Fatalf("%s wrote %d times in the thread of ask %s, whose move appends %d rows (three is the sort they wrote in)",
					ask.from, wrote, ask.id, ask.rows)
			}
		}
	}
}

// assertOneInboxCommentThreadDepth holds oneInboxCommentThreadDepth to the declaration it copies,
// read from the outbox's own source: nothing exported carries the value, and a chain seeded to a
// depth the walk no longer stops at would measure a shape the bound no longer names.
func assertOneInboxCommentThreadDepth(t *testing.T) {
	t.Helper()
	if depth := meInboxWalkDepth(t); depth != oneInboxCommentThreadDepth {
		t.Fatalf("outbox.maxCommentThreadDepth is %d and the corpus's chain is %d links long: move oneInboxCommentThreadDepth with it",
			depth, oneInboxCommentThreadDepth)
	}
}

// oneInboxTransaction runs write in one transaction of its own, failing the test with what it
// was writing.
func oneInboxTransaction(t *testing.T, database *store.Store, what string, write func(pgx.Tx) error) {
	t.Helper()
	started := time.Now()
	if err := pgx.BeginFunc(context.Background(), database.Pool, write); err != nil {
		t.Fatalf("seed %s: %v", what, err)
	}
	t.Logf("seeded %s in %s", what, time.Since(started).Round(time.Millisecond))
}

// oneInboxExpect fails the test unless answer carries status, and answers it.
func oneInboxExpect(t *testing.T, what string, answer apiAnswer, status int) apiAnswer {
	t.Helper()
	if answer.status != status {
		t.Fatalf("%s: %d %s, want %d", what, answer.status, answer.body, status)
	}
	return answer
}

// oneInboxID is the id of what a write answered with status.
func oneInboxID(t *testing.T, what string, answer apiAnswer, status int) string {
	t.Helper()
	oneInboxExpect(t, what, answer, status)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer.body, &created); err != nil || created.ID == "" {
		t.Fatalf("read the id of %s: %v: %s", what, err, answer.body)
	}
	return created.ID
}

func oneInboxStrings(t *testing.T, database *store.Store, statement string, args ...any) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), statement, args...)
	if err != nil {
		t.Fatalf("read %s: %v", statement, err)
	}
	read, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %s: %v", statement, err)
	}
	return read
}

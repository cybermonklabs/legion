package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// How c-bound's database is built: R7's two issues and the issues around them, written with the
// store's own statements rather than through the routes, each item with what its route's reads
// need, then settled and vacuumed. The measurement itself is api/thread_pages_bound_test.go's; this
// file is what was in the database when it ran.

// The counts R7 names, and the issues around them that make a scan of a table cost more than a
// read of its index (the plan's decision 11).
const (
	boundAgentComments  = 9_252
	boundDaemonThreads  = 1_022
	boundAgentAsks      = 213
	boundPersonThreads  = 25
	boundSecondComments = 7
	boundSecondAsks     = 20
	// The first-page weights R7 sizes the second issue to, from production's heaviest first pages:
	// the rows alone, with display trees.
	boundSecondCommentBytes = 353_244
	boundSecondAskBytes     = 203_055
	boundOtherIssues        = 40
	boundOtherComments      = 1_000
	boundOtherMessages      = 300
	boundOtherAsks          = 50
	// boundBodyBytes is the seed command's item size (cmd/dispatch-seed's itemBodyBytes), and
	// boundShortBody the size the second issue's bodies start at before they are fitted.
	boundBodyBytes = 600
	boundShortBody = 64
	boundBatch     = 1_000
)

// The people and sessions that write: the person whose threads All and Mine read, two agents, and
// the project's Legion daemon, whose untargeted roots the person filters leave out.
const (
	boundProject    = "ACME"
	boundPerson     = "alice"
	boundAgent      = "0193f0a4-3b2c-7d5e-8f60-1a2b3c4d5e01"
	boundOtherAgent = "0193f0a4-3b2c-7d5e-8f60-1a2b3c4d5e02"
)

// boundBase is the time every seeded entry is dated from, so the order of the threads is the
// seed's and not the clock's.
var boundBase = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// boundShape is the seeded database, the server over it, and the threads each first page of the
// large issue must answer, newest first.
type boundShape struct {
	handler        http.Handler
	database       *store.Store
	large, second  string
	personComments []string
	personMessages []string
	agentMessages  []string
	agentAsks      []string
	personAsks     []string
}

// boundItem is one comment, message or ask as its row is written. author is the actor's JSON;
// parent is a comment's reply_to or a message's in_reply_to; ask is a comment's ask_id.
type boundItem struct {
	id, author, body    string
	parent, ask, target *string
	at                  time.Time
}

func boundActor(kind, id string) string {
	encoded, err := json.Marshal(model.Actor{Kind: kind, ID: id})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

var (
	boundPersonActor = boundActor("user", boundPerson)
	boundAgentActor  = boundActor("session", boundAgent)
	boundOtherActor  = boundActor("session", boundOtherAgent)
	boundDaemonActor = boundActor("session", legionDaemonSessionOf(boundProject+"-1"))
)

// boundWords are plain lowercase words: a body of them is one paragraph that JSON writes byte for
// byte, so its weight on a page grows by a known amount per character (fitBoundRows).
var boundWords = strings.Fields(`the page reads each thread of the issue and answers its newest
	entries first while the reader waits for the view to draw who wrote in which thread and when an
	agent posted its review of the plan against the spec`)

// boundText is size bytes of words, labelled so a body says what wrote it.
func boundText(label string, n, size int) string {
	var b strings.Builder
	b.Grow(size + 16)
	fmt.Fprintf(&b, "%s %d", label, n)
	for i := 0; b.Len() < size; i++ {
		b.WriteByte(' ')
		b.WriteString(boundWords[(n+i)%len(boundWords)])
	}
	body := []byte(b.String()[:size])
	if body[size-1] == ' ' {
		body[size-1] = 'a'
	}
	return string(body)
}

func boundAt(offset time.Duration) time.Time { return boundBase.Add(offset) }

func boundEntry(author, body string, at time.Time) boundItem {
	return boundItem{id: uuid.NewString(), author: author, body: body, at: at}
}

func (item boundItem) under(parent string) boundItem { item.parent = &parent; return item }
func (item boundItem) onAsk(ask string) boundItem    { item.ask = &ask; return item }
func (item boundItem) to(target string) boundItem    { item.target = &target; return item }

// seedBound builds the whole database: the two issues of R7, the 40 around them, every item's
// blocks settled, and statistics as a production table has them. It asserts the counts before the
// caller times anything, because a measurement over a seed that did not land is worse than none.
func seedBound(t *testing.T, handler http.Handler, database *store.Store) *boundShape {
	t.Helper()
	started := time.Now()
	large := createInteractionIssue(t, handler, boundProject, "The largest thread counts together", "A spec")
	second := createProjectIssue(t, handler, boundProject, "The heaviest first pages", "A spec")
	shape := &boundShape{handler: handler, database: database, large: large.Key, second: second}
	seedBoundLargeIssue(t, shape)
	secondComments, secondAsks := seedBoundSecondIssue(t, shape)
	seedBoundOtherIssues(t, database)
	// The second issue is fitted before anything is indexed, so each block is indexed from the
	// body the item is left holding.
	fitBoundRows(t, shape, second, "comments", "&person=in", boundSecondCommentBytes, secondComments)
	fitBoundRows(t, shape, second, "asks", "&thread=1", boundSecondAskBytes, secondAsks)
	settleBoundItems(t, database)
	// A freshly loaded table has no all-visible page, so the planner avoids an index-only scan on
	// it however good its statistics are (cmd/dispatch's README, "A large test database").
	if _, err := database.Pool.Exec(context.Background(), "vacuum (analyze)"); err != nil {
		t.Fatalf("vacuum the seed: %v", err)
	}
	assertBoundSeed(t, shape)
	t.Logf("seeded in %s: %s holds %d agent posts, %d daemon threads and %d asks above %d threads of each kind a person wrote in and %d between agents; %s is the heavy first pages; %d issues around them",
		time.Since(started).Round(time.Millisecond), shape.large, boundAgentComments, boundDaemonThreads,
		boundAgentAsks, boundPersonThreads, boundPersonThreads, shape.second, boundOtherIssues)
	return shape
}

// seedBoundLargeIssue writes R7's issue, oldest first: under everything, the threads a person
// wrote in and the agents' own message threads; then the 213 asks; then the daemon's 1,022
// threads; then the 9,252 agent posts, newest of all.
func seedBoundLargeIssue(t *testing.T, shape *boundShape) {
	t.Helper()
	var comments, messages, asks []boundItem
	for i := range boundPersonThreads {
		step := time.Duration(i) * time.Minute
		root := boundEntry(boundAgentActor, boundText("Review", i, boundBodyBytes), boundAt(step))
		comments = append(comments, root,
			boundEntry(boundPersonActor, boundText("Answer", i, boundBodyBytes), boundAt(step+30*time.Second)).under(root.id))
		shape.personComments = append(shape.personComments, root.id)

		asked := boundEntry(boundPersonActor, boundText("Question", i, boundBodyBytes), boundAt(2*time.Hour+step)).to("session:" + boundAgent)
		messages = append(messages, asked,
			boundEntry(boundAgentActor, boundText("Reply", i, boundBodyBytes), boundAt(2*time.Hour+step+30*time.Second)).under(asked.id))
		shape.personMessages = append(shape.personMessages, asked.id)

		handed := boundEntry(boundAgentActor, boundText("Handoff", i, boundBodyBytes), boundAt(4*time.Hour+step)).to("session:" + boundOtherAgent)
		messages = append(messages, handed,
			boundEntry(boundOtherActor, boundText("Taken", i, boundBodyBytes), boundAt(4*time.Hour+step+30*time.Second)).under(handed.id))
		shape.agentMessages = append(shape.agentMessages, handed.id)

		ask := boundEntry(boundAgentActor, boundText("Which way", i, boundBodyBytes), boundAt(6*time.Hour+step))
		asks = append(asks, ask)
		comments = append(comments,
			boundEntry(boundPersonActor, boundText("This way", i, boundBodyBytes), boundAt(6*time.Hour+step+30*time.Second)).onAsk(ask.id))
		shape.personAsks = append(shape.personAsks, ask.id)
	}
	for i := range boundAgentAsks {
		step := 24*time.Hour + time.Duration(i)*time.Minute
		ask := boundEntry(boundAgentActor, boundText("Agent question", i, boundBodyBytes), boundAt(step))
		asks = append(asks, ask)
		comments = append(comments,
			boundEntry(boundOtherActor, boundText("Agent note", i, boundBodyBytes), boundAt(step+30*time.Second)).onAsk(ask.id))
		shape.agentAsks = append(shape.agentAsks, ask.id)
	}
	for i := range boundDaemonThreads {
		step := 48*time.Hour + time.Duration(i)*time.Minute
		root := boundEntry(boundDaemonActor, boundText("Daemon", i, boundBodyBytes), boundAt(step))
		messages = append(messages, root,
			boundEntry(boundAgentActor, boundText("Seen", i, boundBodyBytes), boundAt(step+30*time.Second)).under(root.id))
	}
	for i := range boundAgentComments {
		author := boundAgentActor
		if i%2 == 1 {
			author = boundOtherActor
		}
		comments = append(comments, boundEntry(author, boundText("Agent post", i, boundBodyBytes), boundAt(72*time.Hour+time.Duration(i)*10*time.Second)))
	}
	writeBoundAsks(t, shape.database, shape.large, asks)
	writeBoundComments(t, shape.database, shape.large, comments)
	writeBoundMessages(t, shape.database, shape.large, messages)
	for _, ids := range [][]string{shape.personComments, shape.personMessages, shape.agentMessages, shape.agentAsks, shape.personAsks} {
		slices.Reverse(ids)
	}
}

// seedBoundSecondIssue writes the second issue of R7 with short bodies, which fitBoundRows then
// sizes: 7 comment threads a person is in (an agent's review and three replies, two of them the
// person's) and 20 asks, each with the person's reply and an agent's. It writes no message.
func seedBoundSecondIssue(t *testing.T, shape *boundShape) (comments, asks []boundRow) {
	t.Helper()
	var commentItems, askItems, askComments []boundItem
	short := func(label string, n int) string { return boundText(label, n, boundShortBody) }
	for i := range boundSecondComments {
		step := 120*time.Hour + time.Duration(i)*10*time.Minute
		root := boundEntry(boundAgentActor, short("Long review", i), boundAt(step))
		thread := []boundItem{root,
			boundEntry(boundPersonActor, short("Objection", i), boundAt(step+time.Minute)).under(root.id),
			boundEntry(boundAgentActor, short("Revision", i), boundAt(step+2*time.Minute)).under(root.id),
			boundEntry(boundPersonActor, short("Agreement", i), boundAt(step+3*time.Minute)).under(root.id),
		}
		commentItems = append(commentItems, thread...)
		for j, item := range thread {
			comments = append(comments, boundRow{table: "comments", item: item, weight: max(2-j, 1)})
		}
	}
	for i := range boundSecondAsks {
		step := 124*time.Hour + time.Duration(i)*10*time.Minute
		ask := boundEntry(boundAgentActor, short("Long question", i), boundAt(step))
		reply := boundEntry(boundPersonActor, short("Long answer", i), boundAt(step+time.Minute)).onAsk(ask.id)
		note := boundEntry(boundAgentActor, short("Follow-up", i), boundAt(step+2*time.Minute)).onAsk(ask.id)
		askItems = append(askItems, ask)
		askComments = append(askComments, reply, note)
		asks = append(asks,
			boundRow{table: "asks", item: ask, weight: 2},
			boundRow{table: "comments", item: reply, weight: 1},
			boundRow{table: "comments", item: note, weight: 1})
	}
	writeBoundAsks(t, shape.database, shape.second, askItems)
	writeBoundComments(t, shape.database, shape.second, append(commentItems, askComments...))
	return comments, asks
}

// seedBoundOtherIssues files the 40 issues around R7's two, each with 1,000 comments in 500
// threads, 300 message threads and 50 asks, dated before both.
func seedBoundOtherIssues(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		with filed as (
			insert into issues (key, project_key, number, title, created_by, rank)
			select $1 || '-' || n, $1, n, 'Neighbour ' || n, $2::jsonb, 'W' || lpad(n::text, 4, '0')
			from generate_series(3, $3::int + 2) n
			returning number
		)
		update projects set next_number = (select max(number) + 1 from filed) where key = $1
	`, boundProject, boundPersonActor, boundOtherIssues); err != nil {
		t.Fatalf("file the other issues: %v", err)
	}
	for issue := range boundOtherIssues {
		key := fmt.Sprintf("%s-%d", boundProject, issue+3)
		origin := -30*24*time.Hour + time.Duration(issue)*12*time.Hour
		var comments, messages, asks []boundItem
		for i := range boundOtherComments / 2 {
			step := origin + time.Duration(i)*time.Minute
			root := boundEntry(boundAgentActor, boundText("Neighbour post", i, boundBodyBytes), boundAt(step))
			author := boundPersonActor
			if i%2 == 1 {
				author = boundOtherActor
			}
			comments = append(comments, root, boundEntry(author, boundText("Neighbour reply", i, boundBodyBytes), boundAt(step+30*time.Second)).under(root.id))
		}
		for i := range boundOtherMessages {
			step := origin + time.Duration(i)*time.Minute + 10*time.Second
			root := boundEntry(boundAgentActor, boundText("Neighbour note", i, boundBodyBytes), boundAt(step))
			author := boundPersonActor
			if i%2 == 1 {
				root = root.to("session:" + boundOtherAgent)
				author = boundOtherActor
			}
			messages = append(messages, root, boundEntry(author, boundText("Neighbour answer", i, boundBodyBytes), boundAt(step+30*time.Second)).under(root.id))
		}
		for i := range boundOtherAsks {
			asks = append(asks, boundEntry(boundAgentActor, boundText("Neighbour question", i, boundBodyBytes), boundAt(origin+time.Duration(i)*time.Minute+20*time.Second)))
		}
		writeBoundAsks(t, database, key, asks)
		writeBoundComments(t, database, key, comments)
		writeBoundMessages(t, database, key, messages)
	}
}

// boundColumns are a batch's columns, as the unnest of each writer below takes them.
func boundColumns(items []boundItem) (ids, authors, bodies []string, parents, asks, targets []*string, ats []time.Time) {
	for _, item := range items {
		ids, authors, bodies = append(ids, item.id), append(authors, item.author), append(bodies, item.body)
		parents, asks, targets = append(parents, item.parent), append(asks, item.ask), append(targets, item.target)
		ats = append(ats, item.at)
	}
	return
}

// writeBoundComments inserts comments in batches, parents before their replies. A reply answers
// its root, as the create route records it.
func writeBoundComments(t *testing.T, database *store.Store, issue string, items []boundItem) {
	t.Helper()
	for batch := range slices.Chunk(items, boundBatch) {
		ids, authors, bodies, parents, asks, _, ats := boundColumns(batch)
		tag, err := database.Pool.Exec(context.Background(), `
			insert into comments (id, issue_key, author, body, reply_to, answers, ask_id, created_at)
			select u.id::uuid, $1, u.author::jsonb, u.body, u.parent::uuid, u.parent::uuid, u.ask::uuid, u.at
			from unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::timestamptz[])
			     as u(id, author, body, parent, ask, at)
		`, issue, ids, authors, bodies, parents, asks, ats)
		if err != nil || tag.RowsAffected() != int64(len(batch)) {
			t.Fatalf("seed %d comments on %s: %v (%d written)", len(batch), issue, err, tag.RowsAffected())
		}
	}
}

// writeBoundMessages inserts issue messages in batches, roots before their replies.
func writeBoundMessages(t *testing.T, database *store.Store, issue string, items []boundItem) {
	t.Helper()
	for batch := range slices.Chunk(items, boundBatch) {
		ids, authors, bodies, parents, _, targets, ats := boundColumns(batch)
		tag, err := database.Pool.Exec(context.Background(), `
			insert into messages (id, issue_key, author, body, target, in_reply_to, created_at)
			select u.id::uuid, $1, u.author::jsonb, u.body, u.target, u.parent::uuid, u.at
			from unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::timestamptz[])
			     as u(id, author, body, target, parent, at)
		`, issue, ids, authors, bodies, targets, parents, ats)
		if err != nil || tag.RowsAffected() != int64(len(batch)) {
			t.Fatalf("seed %d messages on %s: %v (%d written)", len(batch), issue, err, tag.RowsAffected())
		}
	}
}

// writeBoundAsks inserts asks in batches, each batch in one transaction with the ask.opened event
// of each ask, which every read of an ask resolves (events.OpenedEventIDs). The events take their
// sequences from issues.last_seq, as events.Append does, so the issue's counter stays the highest.
func writeBoundAsks(t *testing.T, database *store.Store, issue string, items []boundItem) {
	t.Helper()
	ctx := context.Background()
	for batch := range slices.Chunk(items, boundBatch) {
		ids, authors, questions, _, _, _, ats := boundColumns(batch)
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the asks of %s: %v", issue, err)
		}
		asked, err := tx.Exec(ctx, `
			insert into asks (id, issue_key, author, question, created_at)
			select u.id::uuid, $1, u.author::jsonb, u.question, u.at
			from unnest($2::text[], $3::text[], $4::text[], $5::timestamptz[]) as u(id, author, question, at)
		`, issue, ids, authors, questions, ats)
		if err != nil || asked.RowsAffected() != int64(len(batch)) {
			_ = tx.Rollback(ctx)
			t.Fatalf("seed %d asks on %s: %v (%d written)", len(batch), issue, err, asked.RowsAffected())
		}
		opened, err := tx.Exec(ctx, `
			with bumped as (
				update issues set last_seq = last_seq + cardinality($2::text[]) where key = $1 returning last_seq
			)
			insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			select $1, bumped.last_seq - cardinality($2::text[]) + u.n, 'ask.opened', u.author::jsonb,
			       jsonb_build_object('id', u.id), false, u.at
			from bumped, unnest($2::text[], $3::text[], $4::timestamptz[]) with ordinality as u(id, author, at, n)
		`, issue, ids, authors, ats)
		if err != nil || opened.RowsAffected() != int64(len(batch)) {
			_ = tx.Rollback(ctx)
			t.Fatalf("seed the opened events of %d asks on %s: %v (%d written)", len(batch), issue, err, opened.RowsAffected())
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the asks of %s: %v", issue, err)
		}
	}
}

// boundRow is one text of a second-issue row that fitBoundRows sizes: the table it is stored in,
// and its share of the row's characters.
type boundRow struct {
	table  string
	item   boundItem
	weight int
}

// fitBoundRows sizes the texts of one first page so its rows weigh want bytes, and at most 0.5 %
// more, read through the route with display trees. A text of these words weighs two bytes on the
// page per character, once as itself and once in its tree, so the first fit is read from the
// page's weight at the short bodies and the last text then takes up what is left. It never fails:
// a page whose rows do not grow as rows should is what the measurement reports.
func fitBoundRows(t *testing.T, shape *boundShape, issue, route, filters string, want int, rows []boundRow) {
	t.Helper()
	aim := want + want/400
	weigh := func() int {
		return readBoundPage(t, boundTarget(issue, route, filters), boundRead(t, shape, issue, route, filters).body).rowBytes()
	}
	weighed := weigh()
	chars, shares := 0, 0
	for _, row := range rows {
		chars, shares = chars+len(row.item.body), shares+row.weight
	}
	total := max(chars+(aim-weighed)/2, len(rows)*boundShortBody)
	spent := 0
	for i := range rows {
		size := max(total*rows[i].weight/shares, boundShortBody)
		if i == len(rows)-1 {
			size = max(total-spent, boundShortBody)
		}
		spent += size
		rows[i].item.body = boundText("Fitted", i, size)
	}
	resizeBoundRows(t, shape.database, rows)
	for tries := 0; ; tries++ {
		weighed = weigh()
		if weighed >= want && weighed <= aim+want/400 || tries == 3 {
			break
		}
		last := &rows[len(rows)-1]
		last.item.body = boundText("Fitted", len(rows)-1, max(len(last.item.body)+(aim-weighed)/2, boundShortBody))
		resizeBoundRows(t, shape.database, rows[len(rows)-1:])
	}
	t.Logf("fitted %s: its rows weigh %d bytes, for %d", boundTarget(issue, route, filters), weighed, want)
}

// resizeBoundRows stores each row's text in its column. Each write marks its item's blocks owed,
// which settleBoundItems settles with the rest.
func resizeBoundRows(t *testing.T, database *store.Store, rows []boundRow) {
	t.Helper()
	for _, column := range [][2]string{{"comments", "body"}, {"asks", "question"}} {
		var ids, texts []string
		for _, row := range rows {
			if row.table == column[0] {
				ids, texts = append(ids, row.item.id), append(texts, row.item.body)
			}
		}
		if len(ids) == 0 {
			continue
		}
		tag, err := database.Pool.Exec(context.Background(), `
			update `+column[0]+` set `+column[1]+` = u.text
			from unnest($1::text[], $2::text[]) as u(id, text) where `+column[0]+`.id = u.id::uuid
		`, ids, texts)
		if err != nil || tag.RowsAffected() != int64(len(ids)) {
			t.Fatalf("size %d %s: %v (%d written)", len(ids), column[0], err, tag.RowsAffected())
		}
	}
}

// settleBoundItems indexes every item the triggers marked owed, with the server's own indexers -
// refs.IndexItem for a comment's and a message's body, as cmd/dispatch-seed does, and for an ask
// refs.IndexItemIfOwed, the drain of its parts - in batches of one transaction each, walking the
// ids upwards so every pass moves on.
func settleBoundItems(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, owed := range []struct {
		kind               refs.ItemKind
		table, state, item string
	}{
		{refs.ItemComment, "comments", "comment_block_state", "comment_id"},
		{refs.ItemMessage, "messages", "message_block_state", "message_id"},
		{refs.ItemAsk, "asks", "ask_block_state", "ask_id"},
	} {
		body := "i.body, coalesce(i.edited_at, i.created_at)"
		if owed.kind == refs.ItemAsk {
			body = "'', i.created_at"
		}
		after := uuid.Nil.String()
		for {
			rows, err := database.Pool.Query(ctx, `
				select i.id::text, `+body+` from `+owed.table+` i join `+owed.state+` s on s.`+owed.item+` = i.id
				where s.owed_at is not null and i.id > $1::uuid order by i.id limit $2
			`, after, boundBatch)
			if err != nil {
				t.Fatalf("read the owed %s: %v", owed.table, err)
			}
			var batch []boundItem
			for rows.Next() {
				var item boundItem
				if err := rows.Scan(&item.id, &item.body, &item.at); err != nil {
					t.Fatalf("read the owed %s: %v", owed.table, err)
				}
				batch = append(batch, item)
			}
			if rows.Err() != nil {
				t.Fatalf("read the owed %s: %v", owed.table, rows.Err())
			}
			if len(batch) == 0 {
				break
			}
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin indexing %s: %v", owed.table, err)
			}
			for _, item := range batch {
				if owed.kind == refs.ItemAsk {
					_, err = refs.IndexItemIfOwed(ctx, tx, refs.ItemAsk, item.id, testServerURL)
				} else {
					_, err = refs.IndexItem(ctx, tx, owed.kind, item.id, item.body, testServerURL, item.at, text.ExtractAt(item.body, testServerURL), nil)
				}
				if err != nil {
					_ = tx.Rollback(ctx)
					t.Fatalf("index %s %s: %v", owed.kind, item.id, err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit the index of %d %s: %v", len(batch), owed.table, err)
			}
			after = batch[len(batch)-1].id
		}
	}
}

// assertBoundSeed holds the seed to R7's counts and to the state a settled server leaves, before
// anything is timed.
func assertBoundSeed(t *testing.T, shape *boundShape) {
	t.Helper()
	var posts, daemon, threads, asks, unopened, owed, neighbours int
	if err := shape.database.Pool.QueryRow(context.Background(), `
		select
			(select count(*) from comments c where c.issue_key = $1 and c.ask_id is null and c.reply_to is null
			   and c.anchor is null and c.author->>'kind' = 'session'
			   and not exists (select 1 from comments r where r.reply_to = c.id)),
			(select count(*) from messages where issue_key = $1 and in_reply_to is null and target is null
			   and author = $3::jsonb),
			(select count(*) from messages where issue_key = $1 and in_reply_to is null),
			(select count(*) from asks where issue_key = $1),
			(select count(*) from asks a where not exists (
			   select 1 from events e where e.type = 'ask.opened' and e.payload->>'id' = a.id::text)),
			(select count(*) from comment_block_state where owed_at is not null)
			 + (select count(*) from message_block_state where owed_at is not null)
			 + (select count(*) from ask_block_state where owed_at is not null),
			(select count(distinct issue_key) from comments where issue_key not in ($1, $2))
	`, shape.large, shape.second, boundDaemonActor).Scan(&posts, &daemon, &threads, &asks, &unopened, &owed, &neighbours); err != nil {
		t.Fatalf("count the seed: %v", err)
	}
	if posts != boundAgentComments || daemon != boundDaemonThreads || threads != boundDaemonThreads+2*boundPersonThreads ||
		asks != boundAgentAsks+boundPersonThreads || unopened != 0 || owed != 0 || neighbours != boundOtherIssues {
		t.Fatalf("seed: %d agent posts with no reply, %d daemon roots, %d message threads, %d asks, %d asks with no opened event, %d items owed their blocks, %d other issues with comments; want %d, %d, %d, %d, 0, 0, %d",
			posts, daemon, threads, asks, unopened, owed, neighbours, boundAgentComments, boundDaemonThreads,
			boundDaemonThreads+2*boundPersonThreads, boundAgentAsks+boundPersonThreads, boundOtherIssues)
	}
}

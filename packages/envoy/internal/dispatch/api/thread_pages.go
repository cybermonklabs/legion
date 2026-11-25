package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// threadActivityTime is the UTC layout for both last_activity_at and the time in before.
// Its six fixed fractional digits match the precision of the database sort key. The page
// statements cut times to microseconds, never round them, so a cursor marks the same position
// the reader saw.
const threadActivityTime = "2006-01-02T15:04:05.000000Z"

// threadPage is the position, size and filters of an issue's activity page, as parsed by
// parseThreadPage. An absent position means the first page, and empty filters narrow nothing.
type threadPage struct {
	limit  int
	before *threadPosition
	author string
	person string
}

// threadPosition is the sort key of one thread: its newest entry's time and its root's UUID.
// The UUID breaks ties so that a later page can start strictly below the previous page.
type threadPosition struct {
	at time.Time
	id string
}

// activity formats the time the page row and its cursor share.
func (p threadPosition) activity() string { return p.at.UTC().Format(threadActivityTime) }

// cursor writes before in the canonical form parseThreadPage accepts. The comment validator
// hashes this same form so a cached answer belongs to the position it was read from.
func (p threadPosition) cursor() string { return p.activity() + "," + p.id }

// parseThreadPage reads the query shared by the three issue thread lists. Without order or
// any page parameter, nil leaves the existing list unchanged. Page parameters require
// order=activity because the list's oldest-first order has no activity cursor. The page
// refuses other orders because its cursor has meaning only in activity order.
//
// A repeated key does not name one value. A non-integer or out-of-range limit does not name
// a supported size. A before that cannot round-trip as a UTC time and lower-case UUID does
// not mark a position this API wrote. An empty author must not silently disable the filter.
// Each caller supplies its allowed person values, so a route never ignores an unsupported one.
func parseThreadPage(query url.Values, people ...string) (*threadPage, error) {
	invalid := func(key string) error {
		return errorf(http.StatusBadRequest, "INVALID_QUERY", "%s must be %s", key, threadPageValues(key, people))
	}
	for _, key := range []string{"order", "limit", "before", "author", "person"} {
		if len(query[key]) > 1 {
			return nil, invalid(key)
		}
	}
	if !query.Has("order") {
		for _, key := range []string{"limit", "before", "author", "person"} {
			if !query.Has(key) {
				continue
			}
			if key == "person" && len(people) == 0 {
				return nil, invalid(key)
			}
			return nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "%s must be absent without order=activity; with it, %s", key, threadPageValues(key, people))
		}
		return nil, nil
	}
	if query.Get("order") != "activity" {
		return nil, invalid("order")
	}
	p := &threadPage{limit: contracts.DefaultThreadPageLimit, author: query.Get("author"), person: query.Get("person")}
	if limit, present, valid := parseQueryInt(query, "limit", 1, contracts.MaxThreadPageLimit); present {
		if !valid {
			return nil, invalid("limit")
		}
		p.limit = limit
	}
	if query.Has("before") {
		before := query.Get("before")
		at, id, _ := strings.Cut(before, ",")
		stamp, timeErr := time.Parse(threadActivityTime, at)
		parsed, idErr := uuid.Parse(id)
		pos := threadPosition{at: stamp, id: parsed.String()}
		if timeErr != nil || idErr != nil || pos.cursor() != before {
			return nil, invalid("before")
		}
		p.before = &pos
	}
	if query.Has("author") && p.author == "" {
		return nil, invalid("author")
	}
	if query.Has("person") && !slices.Contains(people, p.person) {
		return nil, invalid("person")
	}
	return p, nil
}

// threadPageFixedValues are the values each page parameter but person takes, as a refusal
// names them. They are the same on every route, so they are written once.
var threadPageFixedValues = map[string]string{
	"order":  "activity",
	"limit":  fmt.Sprintf("one integer from 1 to %d", contracts.MaxThreadPageLimit),
	"before": "one position: a UTC timestamp with six fractional digits and Z, a comma, and a lower-case UUID",
	"author": "one non-empty login or session ID",
}

// threadPageValues is the values key takes on a route whose person filter takes people, as a
// refusal of key names them. Only a refusal calls it, so an answered page builds no text.
func threadPageValues(key string, people []string) string {
	if key != "person" {
		return threadPageFixedValues[key]
	}
	if len(people) == 0 {
		return "absent (this route does not take a person filter)"
	}
	return "one value: " + strings.Join(people, " or ")
}

// selectThreadPage is the final select shared by the three page statements. It seeks below
// before, then sorts by newest activity and largest UUID first. The window counts the
// filtered threads after that position before the limit, so selectThreads computes remaining
// without a second read that could see a different set.
const selectThreadPage = `
 select id::text, last_activity_at, count(*) over ()
 from threads
 where $3::timestamptz is null or (last_activity_at, id) < ($3, $4::uuid)
 order by last_activity_at desc, id desc
 limit $5`

// args binds the common parameters in the order the page statements expect. Both cursor
// parameters are null on the first page, which disables the position filter.
func (p threadPage) args(key string) []any {
	var at, id any
	if p.before != nil {
		at, id = p.before.at, p.before.id
	}
	return []any{key, p.author, at, id, p.limit}
}

// selectedThreads is the ordered result of one page statement. The three page readers use
// ids to batch their follow-up reads and positions to keep the selected activity times.
type selectedThreads struct {
	positions []threadPosition
	before    *string
	remaining int
}

// ids are the selected threads' ids in page order, read from positions.
func (s selectedThreads) ids() []string {
	ids := make([]string, len(s.positions))
	for i, pos := range s.positions {
		ids[i] = pos.id
	}
	return ids
}

// selectThreads reads the page's positions and subtracts the selected rows from the window
// count. It gives the last row's cursor only when another row follows. A full page can still
// be the last one, so its length alone does not determine whether it has a cursor.
func selectThreads(ctx context.Context, q queryer, statement string, args ...any) (selectedThreads, error) {
	rows, err := q.Query(ctx, statement, args...)
	if err != nil {
		return selectedThreads{}, err
	}
	defer rows.Close()
	var result selectedThreads
	var total int
	for rows.Next() {
		var pos threadPosition
		if err := rows.Scan(&pos.id, &pos.at, &total); err != nil {
			return selectedThreads{}, err
		}
		result.positions = append(result.positions, pos)
	}
	if err := rows.Err(); err != nil {
		return selectedThreads{}, err
	}
	result.remaining = total - len(result.positions)
	if result.remaining > 0 {
		result.before = new(result.positions[len(result.positions)-1].cursor())
	}
	return result, nil
}

// threadAuthorMatches is the predicate all three page statements use for an entry's actor.
// User logins match without regard to case, as they do in user_threads. Session IDs match
// exactly, even if one happens to spell a person's login.
func threadAuthorMatches(actor string) string {
	return fmt.Sprintf(`(case %[1]s->>'kind'
  when 'user' then user_threads_login(%[1]s->>'id') = user_threads_login($2)
  when 'session' then %[1]s->>'id' = $2
  else false end)`, actor)
}

// commentThreadRow groups the root and replies from the existing flat list, shaped as that
// list shapes them and with replies oldest first, in commentRead's members. It adds
// last_activity_at for the thread.
type commentThreadRow struct {
	commentRead
	LastActivityAt string `json:"last_activity_at"`
}

// commentThreadPage is the issue comment route's activity answer. Its before and remaining
// refer to threads, not the individual comments and replies.
type commentThreadPage struct {
	Threads   []commentThreadRow `json:"threads"`
	Before    *string            `json:"before"`
	Remaining int                `json:"remaining"`
}

// commentThreadPageQuery selects a page of root IDs for readCommentThreadPage. Writers
// flatten reply_to to the root, so grouping needs no walk. Ask comments belong to their ask,
// and a group without its own root is not a comment thread.
//
// Creation times decide activity, including deleted entries but not edits. The author filter
// can match any entry, but artifact matches the root's anchor so replies stay in the thread.
// The person filter admits a user entry, an anchored root or a stored reply quote. The stored
// quote keeps the thread on text even when the reply's read hides the deleted parent's words.
var commentThreadPageQuery = `
 with threads as (
  select coalesce(reply_to, id) as id, max(created_at) as last_activity_at
  from comments
  where issue_key = $1 and ask_id is null
  group by coalesce(reply_to, id)
  having bool_or(reply_to is null)
   and ($2 = '' or bool_or(` + threadAuthorMatches("author") + `))
   and ($6 = '' or bool_or(reply_to is null and anchor->>'artifact_id' = $6))
   and ($7 = '' or bool_or(author->>'kind' = 'user' or
    (reply_to is null and anchor is not null) or replies_to_quote is not null))
 )` + selectThreadPage

// readCommentThreadPage builds only the threads selected by commentThreadPageQuery. Like the
// kept list in answerCommentList, it reads as everyReader so a cached answer cannot expose
// a reply block reserved for one caller.
func (s *server) readCommentThreadPage(ctx context.Context, key, artifact string, page threadPage, trees bool) (commentThreadPage, error) {
	pool := s.deps.Store.Pool
	selected, err := selectThreads(ctx, pool, commentThreadPageQuery, append(page.args(key), artifact, page.person)...)
	if err != nil {
		return commentThreadPage{}, err
	}
	result := commentThreadPage{Threads: make([]commentThreadRow, len(selected.positions)), Before: selected.before, Remaining: selected.remaining}
	if len(selected.positions) == 0 {
		return result, nil
	}
	rows, err := pool.Query(ctx, `select `+commentColumns+`
  from comments where (id = any($1::uuid[]) or reply_to = any($1::uuid[]))
   and issue_key = $2 and ask_id is null
  order by created_at, id`, selected.ids(), key)
	if err != nil {
		return commentThreadPage{}, err
	}
	comments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Comment, error) { return scanComment(row) })
	if err != nil {
		return commentThreadPage{}, err
	}
	comments, err = s.hydrateCommentSideTables(ctx, pool, everyReader, comments)
	if err != nil {
		return commentThreadPage{}, err
	}
	indexes := make(map[string]int, len(selected.positions))
	for i, pos := range selected.positions {
		indexes[pos.id] = i
		result.Threads[i] = commentThreadRow{commentRead: commentRead{Replies: []shapedComment{}}, LastActivityAt: pos.activity()}
	}
	reader := s.readDisplayTrees(ctx, trees)
	reads := shapedComments(comments, reader)
	// Resolve the trees before copying shaped comments into their threads.
	reader.resolve()
	for _, read := range reads {
		if read.ReplyTo == nil {
			result.Threads[indexes[read.ID]].Comment = read
		} else {
			thread := &result.Threads[indexes[*read.ReplyTo]]
			thread.Replies = append(thread.Replies, read)
		}
	}
	return result, nil
}

// messageThreadRow is the existing issue message list row with last_activity_at added.
type messageThreadRow struct {
	messageRead
	LastActivityAt string `json:"last_activity_at"`
}

// messageThreadPage is what listIssueMessages answers for order=activity. Its cursor and
// count refer to the filtered roots, not their replies.
type messageThreadPage struct {
	Threads   []messageThreadRow `json:"threads"`
	Before    *string            `json:"before"`
	Remaining int                `json:"remaining"`
}

// messageThreadPageQuery selects roots and their activity for readMessageThreadPage. A
// thread includes replies at every depth. Any entry can match author or supply a person's
// participation. Creation times decide activity, including deleted entries but not edits.
//
// Under person=in, a user entry or an untargeted root qualifies. Under person=none, the root
// must target a role or session and no entry may be by a user. Both filters exclude roots by
// the issue project's daemon from the walk's seed, so even a person's reply cannot bring
// them back. Without person, the daemon is an ordinary author.
var messageThreadPageQuery = `
 with recursive roots as (
  select id, author, created_at, target
  from messages
  where issue_key = $1 and in_reply_to is null
   and ($6 = '' or not (author->>'kind' = 'session' and author->>'id' = $7))
 ), entries as (
  select id as root_id, id, author, created_at from roots
  union all
  select e.root_id, m.id, m.author, m.created_at
  from entries e join messages m on m.in_reply_to = e.id
 ), activity as (
  select root_id as id, max(created_at) as last_activity_at, bool_or(author->>'kind' = 'user') as has_person
  from entries group by root_id
  having $2 = '' or bool_or(` + threadAuthorMatches("author") + `)
 ), threads as (
  select a.id, a.last_activity_at from activity a join roots r on r.id = a.id
  where $6 = ''
   or ($6 = 'in' and (a.has_person or r.target is null))
   or ($6 = 'none' and not a.has_person and r.target is not null)
 )` + selectThreadPage

// readMessageThreadPage selects roots first, then uses hydrateMessageThreads as the list
// does. This limits body and display tree work to the chosen threads.
func (s *server) readMessageThreadPage(ctx context.Context, who model.Actor, key string, page threadPage, trees bool) (messageThreadPage, error) {
	pool := s.deps.Store.Pool
	selected, err := selectThreads(ctx, pool, messageThreadPageQuery, append(page.args(key), page.person, legionDaemonSessionOf(key))...)
	if err != nil {
		return messageThreadPage{}, err
	}
	result := messageThreadPage{Threads: make([]messageThreadRow, 0, len(selected.positions)), Before: selected.before, Remaining: selected.remaining}
	if len(selected.positions) == 0 {
		return result, nil
	}
	rows, err := pool.Query(ctx, `select `+messageColumns+`
  from unnest($1::uuid[]) with ordinality picked(thread_id, pos)
  join messages on id = picked.thread_id order by picked.pos`, selected.ids())
	if err != nil {
		return messageThreadPage{}, err
	}
	roots, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Message, error) { return scanMessage(row) })
	if err != nil {
		return messageThreadPage{}, err
	}
	reads, err := s.hydrateMessageThreads(ctx, who, roots, trees)
	if err != nil {
		return messageThreadPage{}, err
	}
	for i, read := range reads {
		result.Threads = append(result.Threads, messageThreadRow{messageRead: read, LastActivityAt: selected.positions[i].activity()})
	}
	return result, nil
}

// askThreadRow is the existing issue ask list row, including thread when requested, with
// last_activity_at added.
type askThreadRow struct {
	issueAskRow
	LastActivityAt string `json:"last_activity_at"`
}

// askThreadPage is what listIssueAsks answers for order=activity. Its cursor and remaining
// count refer to asks matching the filters.
type askThreadPage struct {
	Threads   []askThreadRow `json:"threads"`
	Before    *string        `json:"before"`
	Remaining int            `json:"remaining"`
}

// askThreadPageQueryAll, askThreadPageQueryOpen and askThreadPageQueryAnswered select asks for
// readAskThreadPage, one statement per state. The ask, its ask_id comments, its current answer
// and its current resolution are the thread's entries. Each can match the author and decide
// the newest activity. The answer names a login and the resolution names an actor. Replaced
// answers and resolutions count for neither filter nor sort.
//
// A literal state lets even a generic plan use asks_open. The answer and resolution times
// are cut as JSON text before the timestamptz cast, which would otherwise round them and
// move the cursor away from the entry's time at microsecond precision.
//
// askThreadRowsQuery reads the selected asks as the list reads them, in page order.
var (
	askThreadPageQueryAll      = askThreadPageStatement("")
	askThreadPageQueryOpen     = askThreadPageStatement(" and a.state = 'open'")
	askThreadPageQueryAnswered = askThreadPageStatement(" and a.state = 'answered'")
	askThreadRowsQuery         = `select ` + askReadColumns + ` ` + askReadFrom + `
  join unnest($1::uuid[]) with ordinality picked(thread_id, pos) on a.id = picked.thread_id
  order by picked.pos`
)

// askThreadPageStatement is the ask page statement narrowed by stateFilter, a literal
// condition on a.state or nothing.
func askThreadPageStatement(stateFilter string) string {
	return `
 with threads as (
  select a.id, greatest(a.created_at, max(c.created_at),
   regexp_replace(a.answer->>'at', '(\.\d{6})\d+', '\1')::timestamptz,
   regexp_replace(a.resolution->>'at', '(\.\d{6})\d+', '\1')::timestamptz
  ) as last_activity_at
  from asks a left join comments c on c.ask_id = a.id
  where a.issue_key = $1` + stateFilter + `
  group by a.id
  having $2 = '' or ` + threadAuthorMatches("a.author") + `
   or bool_or(` + threadAuthorMatches("c.author") + `)
   or user_threads_login(a.answer->>'user') = user_threads_login($2)
   or ` + threadAuthorMatches("(a.resolution->'actor')") + `
 )` + selectThreadPage
}

// readAskThreadPage selects the asks before loading their details and uses the list's row
// builder, so thread=1 costs one set of batched reads over the page, not a fetch per ask.
func (s *server) readAskThreadPage(ctx context.Context, who model.Actor, key, state string, page threadPage, withThread, trees bool) (askThreadPage, error) {
	var statement string
	switch state {
	case "open":
		statement = askThreadPageQueryOpen
	case "answered":
		statement = askThreadPageQueryAnswered
	default:
		statement = askThreadPageQueryAll
	}
	pool := s.deps.Store.Pool
	selected, err := selectThreads(ctx, pool, statement, page.args(key)...)
	if err != nil {
		return askThreadPage{}, err
	}
	result := askThreadPage{Threads: make([]askThreadRow, 0, len(selected.positions)), Before: selected.before, Remaining: selected.remaining}
	if len(selected.positions) == 0 {
		return result, nil
	}
	asks, lastReplies, err := queryAskReads(ctx, pool, who, askThreadRowsQuery, selected.ids())
	if err != nil {
		return askThreadPage{}, err
	}
	reads, err := s.shapeIssueAskRows(ctx, who, asks, lastReplies, withThread, trees)
	if err != nil {
		return askThreadPage{}, err
	}
	for i, read := range reads {
		result.Threads = append(result.Threads, askThreadRow{issueAskRow: read, LastActivityAt: selected.positions[i].activity()})
	}
	return result, nil
}

// shapeIssueAskRows builds the rows shared by listIssueAsks and readAskThreadPage. One tree
// reader serves the asks and any requested threads so their texts can be resolved together.
func (s *server) shapeIssueAskRows(ctx context.Context, who model.Actor, asks []model.Ask, lastReplies map[string]model.AskLastReply, withThread, trees bool) ([]issueAskRow, error) {
	reader := s.readDisplayTrees(ctx, trees)
	rows := make([]issueAskRow, len(asks))
	for i, ask := range asks {
		rows[i].shapedAsk.Ask = ask
		rows[i].shapedAsk.shape(reader)
		if reply, ok := lastReplies[ask.ID]; ok {
			rows[i].LastReply = &reply
		}
	}
	if withThread {
		if err := s.attachIssueAskThreads(ctx, who, rows, reader); err != nil {
			return nil, err
		}
	}
	reader.resolve()
	return rows, nil
}

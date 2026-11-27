package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Every read behind an ask card's initial data: the orchestrator, its reply chain, and the
// per-ask edits, followers and reach (claimant, route and route holder). An Inbox card and an
// issue's ask list render from the same reads, each batched over every ask on the page.
// `inboxAskReplyChainsQuery` stays in comment_queries.go with `replyChainQuery`, the other
// recursion over `comments`, so the two can be read against each other; query_plan_test.go pins
// both from there.

// inboxAskEditsQuery is every recorded rewording of the asks $1 names (text[]), read off their
// ask.edited events in the order each was made, one row per edit (scanInboxAskEdit). GET
// /api/v1/me/inbox splices it into its one statement over the asks of its page.
const inboxAskEditsQuery = `
		select payload->>'id', payload->'previous', payload->'edited_by', created_at
		from events
		where type = 'ask.edited' and payload->>'id' = any($1::text[])
		order by payload->>'id', id
	`

// scanInboxAskEdit reads one row of inboxAskEditsQuery: the ask's id and the edit.
func scanInboxAskEdit(row pgx.Row) (string, model.AskEdit, error) {
	var askID string
	var previous, editedBy []byte
	var at time.Time
	if err := row.Scan(&askID, &previous, &editedBy, &at); err != nil {
		return "", model.AskEdit{}, fmt.Errorf("scan inbox ask edit: %w", err)
	}
	var edit model.AskEdit
	if err := json.Unmarshal(previous, &edit.Previous); err != nil {
		return "", model.AskEdit{}, fmt.Errorf("decode inbox ask edit previous: %w", err)
	}
	if err := json.Unmarshal(editedBy, &edit.EditedBy); err != nil {
		return "", model.AskEdit{}, fmt.Errorf("decode inbox ask edit editor: %w", err)
	}
	edit.At = timestampValue(at)
	return askID, edit, nil
}

func (s *server) loadInboxAskEdits(ctx context.Context, q queryer, askIDs []string) (map[string][]model.AskEdit, error) {
	edits := make(map[string][]model.AskEdit, len(askIDs))
	for _, askID := range askIDs {
		edits[askID] = []model.AskEdit{}
	}
	rows, err := q.Query(ctx, inboxAskEditsQuery, askIDs)
	if err != nil {
		return nil, fmt.Errorf("load inbox ask edits: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		askID, edit, err := scanInboxAskEdit(rows)
		if err != nil {
			return nil, err
		}
		edits[askID] = append(edits[askID], edit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load inbox ask edits: %w", err)
	}
	return edits, nil
}

// inboxAskFollowersQuery is every session following the asks $1 names (uuid[]), each ask's in the
// order they began following, one row per follower (scanInboxAskFollower). GET /api/v1/me/inbox
// splices it into its one statement over the asks of its page.
const inboxAskFollowersQuery = `
		select ask_id::text, session_id, since
		from ask_followers
		where ask_id = any($1::uuid[])
		order by ask_id, since, session_id
	`

// scanInboxAskFollower reads one row of inboxAskFollowersQuery: the ask's id and its follower.
func scanInboxAskFollower(row pgx.Row) (string, model.AskFollower, error) {
	var askID string
	var follower model.AskFollower
	if err := row.Scan(&askID, &follower.SessionID, &follower.Since); err != nil {
		return "", model.AskFollower{}, fmt.Errorf("scan inbox ask follower: %w", err)
	}
	return askID, follower, nil
}

func (s *server) loadInboxAskFollowers(ctx context.Context, q queryer, askIDs []string) (map[string][]model.AskFollower, error) {
	followers := make(map[string][]model.AskFollower, len(askIDs))
	for _, askID := range askIDs {
		followers[askID] = []model.AskFollower{}
	}
	rows, err := q.Query(ctx, inboxAskFollowersQuery, askIDs)
	if err != nil {
		return nil, fmt.Errorf("load inbox ask followers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		askID, follower, err := scanInboxAskFollower(rows)
		if err != nil {
			return nil, err
		}
		followers[askID] = append(followers[askID], follower)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load inbox ask followers: %w", err)
	}
	return followers, nil
}

// loadAskThreads batches the initial data each ask card renders, for the Inbox and for an
// issue's ask list. Both write paths normalise a reply under an ask to that ask's ask_id, so the
// recursive reply_to walk only picks up rows a pre-0043 server wrote or is still draining; it
// matches loadReplyChain exactly either way. Answers are not among the reads: an Inbox row is an
// open ask and can have none, so a caller that does want them (an issue's ask list asked for a
// thread) merges loadAskAnswersByAsk over the same ask ids itself rather than making every
// Inbox load scan `events` for answers that cannot exist. It reads on the pool, never inside a
// transaction, because the reach read ends with a listener call (loadAskReaches). The replies'
// blocks are read for who.
func (s *server) loadAskThreads(ctx context.Context, who model.Actor, askIDs []string, reader *displayTreeReader) (map[string]askThread, error) {
	threads := make(map[string]askThread, len(askIDs))
	for _, askID := range askIDs {
		threads[askID] = askThread{
			Replies:   []shapedComment{},
			Edits:     []askEditRead{},
			Answers:   []model.AskAnswerWithTree{},
			Followers: []model.AskFollower{},
		}
	}
	if len(askIDs) == 0 {
		return threads, nil
	}

	q := s.deps.Store.Pool
	replies, err := s.loadInboxAskReplyChains(ctx, q, who, askIDs)
	if err != nil {
		return nil, err
	}
	edits, err := s.loadInboxAskEdits(ctx, q, askIDs)
	if err != nil {
		return nil, err
	}
	followers, err := s.loadInboxAskFollowers(ctx, q, askIDs)
	if err != nil {
		return nil, err
	}
	reaches, err := s.loadAskReaches(ctx, askIDs)
	if err != nil {
		return nil, err
	}
	// Both the Inbox and an issue's ask list shape their threads here, over the reader of the
	// request that asked for them, so the replies' and the rewordings' texts ride in that one
	// read of the table and what it did not hold is stored back once.
	for askID, thread := range threads {
		thread.Replies = shapedComments(replies[askID], reader)
		thread.Edits = askEditReads(edits[askID], reader)
		thread.Followers = followers[askID]
		thread.askReach = reaches[askID]
		threads[askID] = thread
	}
	return threads, nil
}

// loadAskReaches reads every listed ask's askReach, keyed by ask id: the claimant only while a
// session holds the claim on the ask's issue (the same rule the outbox's readBatch
// reads), the issue's route, and that route's reach now (status and holder). The reach comes
// from one listener read for the whole batch (routeRosterOrUnknown), made only when some ask has
// a route and only once scanAskReaches has released its pooled connection, so the listener call
// never runs inside a transaction or with a connection held; a listener that is unconfigured or
// does not answer, or a failed read of the stored answer, marks every route's status unknown
// with a null holder and never fails the read, since the ask itself was read. A route whose
// holder has missed its heartbeats reads holder_unresponsive, naming that holder: the ask still
// reaches it, since what is sent to it waits for it.
func (s *server) loadAskReaches(ctx context.Context, askIDs []string) (map[string]askReach, error) {
	reaches, err := scanAskReaches(ctx, s.deps.Store.Pool, askIDs)
	if err != nil {
		return nil, err
	}
	routed := false
	for _, reach := range reaches {
		if reach.Route != nil {
			routed = true
			break
		}
	}
	if !routed {
		return reaches, nil
	}
	roster := s.routeRosterOrUnknown(ctx)
	for askID, reach := range reaches {
		reach.IssueRouteReach = roster.reach(reach.Route)
		reaches[askID] = reach
	}
	return reaches, nil
}

// askReachesQuery is the claimant and the route of each ask $1 names (uuid[]), one row per ask
// (scanAskReach): an empty id or route is none, and a project document's ask joins no issue. GET
// /api/v1/me/inbox splices it into its one statement over the asks of its page.
const askReachesQuery = `
		select a.id::text,
			case when i.claimed_by->>'kind' = 'session' then nullif(i.claimed_by->>'id', '') end,
			nullif(i.route, '')
		from asks a
		left join issues i on i.key = a.issue_key
		where a.id = any($1::uuid[])
	`

// scanAskReach reads one row of askReachesQuery: the ask's id and its claimant and route, the
// route's reach not yet read.
func scanAskReach(row pgx.Row) (string, askReach, error) {
	var askID string
	var reach askReach
	if err := row.Scan(&askID, &reach.Claimant, &reach.Route); err != nil {
		return "", askReach{}, fmt.Errorf("scan ask reach: %w", err)
	}
	return askID, reach, nil
}

// scanAskReaches reads every listed ask's claimant and route in one query and returns with the
// pooled connection released. An ask the query does not find keeps the zero askReach, so every
// key is null.
func scanAskReaches(ctx context.Context, q queryer, askIDs []string) (map[string]askReach, error) {
	reaches := make(map[string]askReach, len(askIDs))
	rows, err := q.Query(ctx, askReachesQuery, askIDs)
	if err != nil {
		return nil, fmt.Errorf("load ask reach: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		askID, reach, err := scanAskReach(rows)
		if err != nil {
			return nil, err
		}
		reaches[askID] = reach
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load ask reach: %w", err)
	}
	return reaches, nil
}

// loadInboxAskReplyChains groups inboxAskReplyChainsQuery's rows by the ask each belongs to,
// preserving the query's order within a thread, each reply's block read for who.
func (s *server) loadInboxAskReplyChains(ctx context.Context, q queryer, who model.Actor, askIDs []string) (map[string][]model.Comment, error) {
	replies := make(map[string][]model.Comment, len(askIDs))
	for _, askID := range askIDs {
		replies[askID] = []model.Comment{}
	}
	rows, err := q.Query(ctx, inboxAskReplyChainsQuery, askIDs)
	if err != nil {
		return nil, fmt.Errorf("load inbox ask replies: %w", err)
	}
	defer rows.Close()
	type threadedComment struct {
		askID   string
		comment model.Comment
	}
	comments := []threadedComment{}
	for rows.Next() {
		var askID string
		comment, err := scanComment(rows, &askID)
		if err != nil {
			return nil, fmt.Errorf("scan inbox ask reply: %w", err)
		}
		comments = append(comments, threadedComment{askID: askID, comment: comment})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load inbox ask replies: %w", err)
	}
	unhydrated := make([]model.Comment, len(comments))
	for index := range comments {
		unhydrated[index] = comments[index].comment
	}
	hydrated, err := s.hydrateCommentSideTables(ctx, q, who, unhydrated)
	if err != nil {
		return nil, fmt.Errorf("hydrate inbox ask replies: %w", err)
	}
	for index := range hydrated {
		replies[comments[index].askID] = append(replies[comments[index].askID], hydrated[index])
	}
	return replies, nil
}

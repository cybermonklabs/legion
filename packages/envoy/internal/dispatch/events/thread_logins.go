package events

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// ThreadLogins is the thread_logins an event names: every login with a Replies row
// (user_threads, migrations 0095-0097) on threadID, sorted. The rows are kept by triggers on the
// comment, message and ask writes, so a writer reads this in its own transaction after the write,
// and the list says whose Replies count the write can have moved. It is never nil, so a thread
// nobody has a row on is named as [] rather than left out. threadID is the thread as the
// triggers key it: an ask's id; a comment's ask, else the root it replies to, else itself; an
// issue message's root up in_reply_to. A writer whose write phase is one batch has the event's
// insert read them instead (ThreadLoginsOf). A direct (issue-less) message keeps no row here,
// and names its thread root's author instead (api/thread_logins.go, directThreadLogins).
func ThreadLogins(ctx context.Context, q Queryer, threadID string) ([]string, error) {
	logins := []string{}
	if err := q.QueryRow(ctx, threadLoginsQuery("$1"), threadID).Scan(&logins); err != nil {
		return nil, fmt.Errorf("read the logins of thread %s: %w", threadID, err)
	}
	return logins, nil
}

// threadLoginsQuery is ThreadLogins' query, its thread id the parameter param.
func threadLoginsQuery(param string) string {
	return `select coalesce(array_agg(login order by login), '{}') from user_threads where thread_id = ` + param + `::uuid`
}

// issueLoginsQuery is the status half of an issue event's thread_logins, for an event that
// closed or reopened the issue (IssueThreadLogins, IssueLoginsOf), its issue key the parameter
// param: every login with a Replies row on one of that issue's open asks, sorted and never nil.
// The Replies list leaves out an open ask on an open issue assigned to that login
// (api/replies.go, myRepliesOwner), so a close or a reopen moves exactly those rows and no
// other. It reads the asks through asks_open and their rows through user_threads_thread, so it
// never scans user_threads.
//
// An issue event carries logins on an assignee move too, the old and the new assignee, since the
// same rule moves an open ask's row out of one Inbox and into the other; the two sets are joined
// by api/thread_logins.go's issueWriteThreadLogins, which is the whole of what an issue event
// names, and by the `also` logins IssueLoginsOf unions into this query for a writer whose asks
// are still queued.
func issueLoginsQuery(param string) string {
	return `
		select coalesce(array_agg(distinct u.login order by u.login), '{}')
		from user_threads u
		where u.thread_id in (select a.id from asks a where a.issue_key = ` + param + ` and a.state = 'open')
	`
}

// IssueThreadLogins is issueLoginsQuery read in q, for a writer whose asks are written by the
// time it reads them. A writer whose write phase is one batch has the event's insert read them
// instead (IssueLoginsOf).
func IssueThreadLogins(ctx context.Context, q Queryer, issueKey string) ([]string, error) {
	logins := []string{}
	if err := q.QueryRow(ctx, issueLoginsQuery("$1"), issueKey).Scan(&logins); err != nil {
		return nil, fmt.Errorf("read the logins of issue %s's open asks: %w", issueKey, err)
	}
	return logins, nil
}

// Logins is the thread_logins an event's payload carries, read by the event's own insert
// (QueueAppendWithLogins) from user_threads as the transaction's earlier statements, and the
// triggers they fired, left it: the read ThreadLogins makes after the write, for a writer whose
// write phase is one batch, which reads nothing in Go between its statements. also is the logins
// the writer already knows, unioned into what the query reads.
type Logins struct {
	query func(param string) string
	key   string
	also  []string
}

// SQL is the expression reading the logins, its first parameter $first, with the arguments those
// parameters take. The logins the writer already knows are unioned in and the whole is sorted and
// deduplicated, so the event names each login once however it was found.
func (l *Logins) SQL(first int) (string, []any) {
	query := l.query(fmt.Sprintf("$%d", first))
	args := []any{any(l.key)}
	if len(l.also) == 0 {
		return query, args
	}
	return fmt.Sprintf(`
		select coalesce(array_agg(distinct login order by login), '{}')
		from (select unnest((%s)) as login union all select unnest($%d::text[])) as named
	`, query, first+1), append(args, l.also)
}

// ThreadLoginsOf is ThreadLogins of threadID, read by the event's insert.
func ThreadLoginsOf(threadID string) *Logins {
	return &Logins{query: threadLoginsQuery, key: threadID}
}

// IssueLoginsOf is the logins of issueKey's open asks (issueLoginsQuery), read by the event's
// insert: after the asks a close withdrew or a reopen restored earlier in the same batch. also is
// the logins the write already knows it moved, the assignee it took the issue from and the one it
// gave it to (api.issueWriteThreadLogins), unioned into them.
func IssueLoginsOf(issueKey string, also ...string) *Logins {
	return &Logins{query: issueLoginsQuery, key: issueKey, also: also}
}

// withThreadLogins is payload, the payload of an event that names thread_logins, carrying logins.
func withThreadLogins(payload any, logins []string) (any, error) {
	switch typed := payload.(type) {
	case model.AskEventPayload:
		typed.ThreadLogins = logins
		return typed, nil
	case model.AskAnsweredEventPayload:
		typed.ThreadLogins = logins
		return typed, nil
	case model.CommentEventPayload:
		typed.ThreadLogins = logins
		return typed, nil
	case model.MessageEventPayload:
		typed.ThreadLogins = logins
		return typed, nil
	case model.IssueEventPayload:
		typed.ThreadLogins = logins
		return typed, nil
	}
	return nil, fmt.Errorf("event payload %T names no thread_logins", payload)
}

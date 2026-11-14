package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// writing_check_reader.go decides who reads the prose of a write (INFRA-3231, R1): the writing
// check reads a write only when a person reads it. The server decides that person from the
// record, never from the text, before the check reads anything and before the write opens a
// transaction.

// readerRule is how the server finds, from the record, the person who reads the prose one route
// writes. writingCheckReaderRules gives every route the prose registry covers its rule, and
// TestEveryProseRouteNamesItsReaderRule holds it to them.
type readerRule string

const (
	// readerNewIssue is the person the new issue goes to: the login the create names, else the one
	// defaultAssignee gives it - the person the creating credential acts for, the parent's
	// assignee, the project's default assignee.
	readerNewIssue readerRule = "new_issue"
	// readerIssue is the assignee of the issue {key}, else its project's default assignee. A new
	// ask on the issue names no chosen person, so this is also the person it waits on.
	readerIssue readerRule = "issue"
	// readerProject is the default assignee of the project {key}.
	readerProject readerRule = "project"
	// readerDocument reads the document {id}'s owner: an issue's document as readerIssue does, a
	// project's as readerProject does, and a direct conversation's as readerConversation does. A
	// new ask or approval request on it names no chosen person, so this is also the person it
	// waits on.
	readerDocument readerRule = "document"
	// readerConversation is the person in the direct conversation with the session {session_id}:
	// the author of its newest thread when a person opened it.
	readerConversation readerRule = "conversation"
	// readerAsk is the person the ask {id} waits on: the person it was handed to, else the reader
	// of its issue or of its document's owner (textOwnerJoins), which is askedOfPersonExpression
	// with an ask on an issue's document read through that issue.
	readerAsk readerRule = "ask"
	// readerAskReply is the author of the ask {id} when a person asked it, else the reader of the
	// ask's issue or of its document's owner.
	readerAskReply readerRule = "ask_reply"
	// readerComment is the author of the comment reply_to names, or of the ask ask_id names, when
	// a person wrote it, else the reader of the issue {key} or the document {id}.
	readerComment readerRule = "comment"
	// readerCommentReply is the author of the comment {id} when a person wrote it, else the
	// reader of its issue or document.
	readerCommentReply readerRule = "comment_reply"
	// readerCommentEdit is the author of what the comment {id} answers - a comment or an ask -
	// when a person wrote it, else the reader of its issue or document.
	readerCommentEdit readerRule = "comment_edit"
	// readerMessage is the author of the message in_reply_to names when a person wrote it, else
	// the reader of the issue {key}.
	readerMessage readerRule = "message"
	// readerMessageReply is the author of the message {id} when a person wrote it; else the
	// reader of its issue; else, in a direct conversation, the person who opened it, and nobody
	// when a session did.
	readerMessageReply readerRule = "message_reply"
	// readerResult is the reader of the issue the result {id} was recorded on.
	readerResult readerRule = "result"
	// readerNone is no person: the text goes to agent sessions alone.
	readerNone readerRule = "none"
)

// writingCheckReaderRules is the reader rule of every route whose request carries prose, by
// "<method> <pattern>".
var writingCheckReaderRules = map[string]readerRule{
	"POST /api/v1/issues":                                 readerNewIssue,
	"PATCH /api/v1/issues/{key}":                          readerIssue,
	"POST /api/v1/issues/{key}/artifacts":                 readerIssue,
	"POST /api/v1/issues/{key}/artifacts/{slug}/edits":    readerIssue,
	"POST /api/v1/issues/{key}/artifacts/{slug}/versions": readerIssue,
	"POST /api/v1/issues/{key}/asks":                      readerIssue,
	"POST /api/v1/issues/{key}/results":                   readerIssue,
	"POST /api/v1/issues/{key}/comments":                  readerComment,
	"POST /api/v1/issues/{key}/messages":                  readerMessage,

	"POST /api/v1/projects/{key}/artifacts":                 readerProject,
	"POST /api/v1/projects/{key}/artifacts/{slug}/edits":    readerProject,
	"POST /api/v1/projects/{key}/artifacts/{slug}/versions": readerProject,

	"POST /api/v1/artifacts/{id}/edits":             readerDocument,
	"POST /api/v1/artifacts/{id}/rebuild":           readerDocument,
	"POST /api/v1/artifacts/{id}/reviews":           readerDocument,
	"POST /api/v1/artifacts/{id}/versions":          readerDocument,
	"POST /api/v1/artifacts/{id}/asks":              readerDocument,
	"POST /api/v1/artifacts/{id}/approval-requests": readerDocument,
	"POST /api/v1/artifacts/{id}/comments":          readerComment,

	"PATCH /api/v1/asks/{id}":        readerAsk,
	"POST /api/v1/asks/{id}/answer":  readerAskReply,
	"POST /api/v1/asks/{id}/decline": readerAskReply,
	"POST /api/v1/asks/{id}/resolve": readerAskReply,

	"POST /api/v1/comments/{id}/reply":  readerCommentReply,
	"PATCH /api/v1/comments/{id}":       readerCommentEdit,
	"POST /api/v1/messages/{id}/reply":  readerMessageReply,
	"POST /api/v1/results/{id}/retract": readerResult,

	"POST /api/v1/agents/{session_id}/artifacts": readerConversation,
	"POST /api/v1/agents/{session_id}/messages":  readerNone,
	"POST /api/v1/broadcasts":                    readerNone,
}

// writingCheckReader decides who reads the prose of the write r makes on route ("<method>
// <pattern>"), by the route's rule (writingCheckReaderRules), and answers that person's login, or
// "" where no person reads it. An ask's question, its options and an approval request's summary
// are read by the person the ask waits on; a reply to a person's text by that person; any other
// text on an issue by the issue's assignee, else the project's default assignee; a new issue by
// the person it goes to; a project's document and the text on it by the project's default
// assignee; and a direct conversation's text by the person in it. A send to an agent, a
// broadcast, a reply in a direct conversation between two sessions and a record that names
// nobody have no person as their reader. It reads the pool, so call it before any transaction.
// A record the write names that does not exist answers "": the write itself refuses it.
func (s *server) writingCheckReader(ctx context.Context, r *http.Request, route string, actor model.Actor, input any) (string, error) {
	rule, ok := writingCheckReaderRules[route]
	if !ok {
		return "", fmt.Errorf("route %s names no reader rule", route)
	}
	q := s.deps.Store.Pool
	switch rule {
	case readerNewIssue:
		create, _ := input.(createIssueInput)
		return newIssueReader(ctx, q, actor, create)
	case readerIssue:
		return issueReader(ctx, q, r.PathValue("key"))
	case readerProject:
		return projectReader(ctx, q, r.PathValue("key"))
	case readerDocument:
		return documentReader(ctx, q, r.PathValue("id"))
	case readerConversation:
		return conversationReader(ctx, q, r.PathValue("session_id"))
	case readerAsk:
		answered, err := readAnsweredAsk(ctx, q, r.PathValue("id"))
		return answered.waitsOn, err
	case readerAskReply:
		answered, err := readAnsweredAsk(ctx, q, r.PathValue("id"))
		return answered.reader(), err
	case readerComment:
		comment, _ := input.(commentInput)
		return newCommentReader(ctx, q, r, comment)
	case readerCommentReply:
		answered, err := readAnsweredComment(ctx, q, r.PathValue("id"))
		return answered.reader(), err
	case readerCommentEdit:
		return editedCommentReader(ctx, q, r.PathValue("id"))
	case readerMessage:
		message, _ := input.(createMessageInput)
		if message.InReplyTo != nil {
			answered, err := readAnsweredMessage(ctx, q, *message.InReplyTo)
			if err != nil || answered.author != "" {
				return answered.author, err
			}
		}
		return issueReader(ctx, q, r.PathValue("key"))
	case readerMessageReply:
		answered, err := readAnsweredMessage(ctx, q, r.PathValue("id"))
		return answered.reader(), err
	case readerResult:
		if _, err := uuid.Parse(r.PathValue("id")); err != nil {
			return "", nil
		}
		var issueKey string
		err := q.QueryRow(ctx, `select issue_key from check_results where id = $1`, r.PathValue("id")).Scan(&issueKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("read the issue of result %s: %w", r.PathValue("id"), err)
		}
		return issueReader(ctx, q, issueKey)
	case readerNone:
		return "", nil
	}
	return "", fmt.Errorf("route %s has reader rule %q, which names no read", route, rule)
}

// personLogin is the login of an author when the author is a person, "" for a session, Dispatch
// or any other writer.
func personLogin(kind, id string) string {
	if kind != "user" {
		return ""
	}
	return model.CanonicalLogin(id)
}

// readerOrNone answers login, or "" where the row a lookup reads does not exist.
func readerOrNone(login string, err error, what string) (string, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the reader of %s: %w", what, err)
	}
	return login, nil
}

// ownerReaderExpression is the reader of text on an issue or a project's document, over the
// issue aliased i, and the project aliased p of whichever of the issue and the document the row
// names, joined as askedOfPersonJoins joins them: the issue's assignee, else the project's
// default assignee.
const ownerReaderExpression = `coalesce(i.assignee, p.default_assignee, '')`

func issueReader(ctx context.Context, q queryer, key string) (string, error) {
	var login string
	err := q.QueryRow(ctx, `
		select `+ownerReaderExpression+`
		from issues i join projects p on p.key = i.project_key
		where i.key = $1`, key).Scan(&login)
	return readerOrNone(login, err, "issue "+key)
}

func projectReader(ctx context.Context, q queryer, key string) (string, error) {
	var login string
	err := q.QueryRow(ctx, `select coalesce(default_assignee, '') from projects where key = $1`, key).Scan(&login)
	return readerOrNone(login, err, "project "+key)
}

func documentReader(ctx context.Context, q queryer, id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", nil
	}
	var login string
	var session *string
	err := q.QueryRow(ctx, `
		select `+ownerReaderExpression+`, ar.session_id
		from artifacts ar
		left join issues i on i.key = ar.issue_key
		left join projects p on p.key = coalesce(i.project_key, ar.project_key)
		where ar.id = $1`, id).Scan(&login, &session)
	if session != nil && err == nil {
		return conversationReader(ctx, q, *session)
	}
	return readerOrNone(login, err, "document "+id)
}

// conversationReader is the person in the direct conversation with session: the author of the
// newest issue-less thread a person opened to it (messages_session_roots serves the read).
func conversationReader(ctx context.Context, q queryer, session string) (string, error) {
	var login string
	err := q.QueryRow(ctx, `
		select coalesce(author ->> 'id', '')
		from messages
		where target = $1 and in_reply_to is null and issue_key is null and author ->> 'kind' = 'user'
		order by created_at desc
		limit 1`, "session:"+session).Scan(&login)
	return readerOrNone(model.CanonicalLogin(login), err, "the direct conversation with session "+session)
}

// newIssueReader is readerNewIssue: the person createIssue gives the new issue, read before the
// create runs. An assignee the create will refuse names nobody here, and the create refuses it.
func newIssueReader(ctx context.Context, q queryer, actor model.Actor, input createIssueInput) (string, error) {
	var named string
	if len(input.Assignee) > 0 && json.Unmarshal(input.Assignee, &named) == nil && model.CanonicalLogin(named) != "" {
		return model.CanonicalLogin(named), nil
	}
	if login, acts := actorPerson(actor); acts {
		return login, nil
	}
	if input.Parent != nil && strings.TrimSpace(*input.Parent) != "" {
		parent := strings.TrimSpace(*input.Parent)
		var assignee *string
		err := q.QueryRow(ctx, `select assignee from issues where key = $1`, parent).Scan(&assignee)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("read the assignee of parent %s: %w", parent, err)
		}
		if assignee != nil {
			return *assignee, nil
		}
	}
	return projectReader(ctx, q, input.Project)
}

// answeredText is a text a write answers: its author's login when a person wrote it, and the
// reader of the issue or document it is on.
type answeredText struct {
	author, owner string
}

// reader is a reply's reader: the person it answers, else whoever reads the text's owner.
func (t answeredText) reader() string {
	if t.author != "" {
		return t.author
	}
	return t.owner
}

// answeredAsk is an ask a write answers, with the person it waits on.
type answeredAsk struct {
	answeredText
	waitsOn string
}

// textOwnerJoins joins the issue i, the document ar and the project p of a comment or an ask
// aliased by its first argument, for ownerReaderExpression. A text on a document carries the
// document's id and no issue_key, so the issue is the text's own or else the document's: a text
// on an issue's document is read by that issue's assignee, not by its project's default alone.
// A document in a direct conversation names neither, and its reader is read by
// conversationReader from ar.session_id.
func textOwnerJoins(text string) string {
	return `
		left join artifacts ar on ar.id = ` + text + `.artifact_id
		left join issues i on i.key = coalesce(` + text + `.issue_key, ar.issue_key)
		left join projects p on p.key = coalesce(i.project_key, ar.project_key)`
}

// textOwner is the reader of what a comment or an ask is on: owner as ownerReaderExpression read
// it, or the person in the direct conversation where the text is on a document of one.
func textOwner(ctx context.Context, q queryer, owner string, session *string) (string, error) {
	if session != nil {
		return conversationReader(ctx, q, *session)
	}
	return owner, nil
}

func readAnsweredAsk(ctx context.Context, q queryer, id string) (answeredAsk, error) {
	if _, err := uuid.Parse(id); err != nil {
		return answeredAsk{}, nil
	}
	var ask answeredAsk
	var kind, author, askedOf, owner string
	var session *string
	err := q.QueryRow(ctx, `
		select coalesce(a.author ->> 'kind', ''), coalesce(a.author ->> 'id', ''), coalesce(a.asked_of, ''),
			`+ownerReaderExpression+`, ar.session_id
		from asks a`+textOwnerJoins("a")+`
		where a.id = $1`, id).Scan(&kind, &author, &askedOf, &owner, &session)
	if errors.Is(err, pgx.ErrNoRows) {
		return answeredAsk{}, nil
	}
	if err != nil {
		return answeredAsk{}, fmt.Errorf("read the reader of ask %s: %w", id, err)
	}
	if ask.owner, err = textOwner(ctx, q, owner, session); err != nil {
		return answeredAsk{}, err
	}
	ask.author = personLogin(kind, author)
	ask.waitsOn = askedOf
	if ask.waitsOn == "" {
		ask.waitsOn = ask.owner
	}
	return ask, nil
}

// answeredComment is a comment a write answers, with what the comment itself answers.
type answeredComment struct {
	answeredText
	answers, askID *string
}

func readAnsweredComment(ctx context.Context, q queryer, id string) (answeredComment, error) {
	if _, err := uuid.Parse(id); err != nil {
		return answeredComment{}, nil
	}
	var comment answeredComment
	var kind, author, owner string
	var session *string
	err := q.QueryRow(ctx, `
		select coalesce(c.author ->> 'kind', ''), coalesce(c.author ->> 'id', ''), `+ownerReaderExpression+`,
			ar.session_id, coalesce(c.answers, c.reply_to)::text, c.ask_id::text
		from comments c`+textOwnerJoins("c")+`
		where c.id = $1`, id).Scan(&kind, &author, &owner, &session, &comment.answers, &comment.askID)
	if errors.Is(err, pgx.ErrNoRows) {
		return answeredComment{}, nil
	}
	if err != nil {
		return answeredComment{}, fmt.Errorf("read the reader of comment %s: %w", id, err)
	}
	if comment.owner, err = textOwner(ctx, q, owner, session); err != nil {
		return answeredComment{}, err
	}
	comment.author = personLogin(kind, author)
	return comment, nil
}

// newCommentReader is readerComment: the person whose comment or ask the new comment answers,
// else the reader of the issue {key} or the document {id} it is written on.
func newCommentReader(ctx context.Context, q queryer, r *http.Request, input commentInput) (string, error) {
	var author string
	var err error
	switch {
	case input.ReplyTo != nil:
		var answered answeredComment
		answered, err = readAnsweredComment(ctx, q, *input.ReplyTo)
		author = answered.author
	case input.AskID != nil:
		var answered answeredAsk
		answered, err = readAnsweredAsk(ctx, q, *input.AskID)
		author = answered.author
	}
	if err != nil || author != "" {
		return author, err
	}
	if key := r.PathValue("key"); key != "" {
		return issueReader(ctx, q, key)
	}
	return documentReader(ctx, q, r.PathValue("id"))
}

// editedCommentReader is readerCommentEdit: the reader of the comment {id} as it was written.
func editedCommentReader(ctx context.Context, q queryer, id string) (string, error) {
	edited, err := readAnsweredComment(ctx, q, id)
	if err != nil {
		return "", err
	}
	var author string
	switch {
	case edited.answers != nil:
		answered, err := readAnsweredComment(ctx, q, *edited.answers)
		if err != nil {
			return "", err
		}
		author = answered.author
	case edited.askID != nil:
		answered, err := readAnsweredAsk(ctx, q, *edited.askID)
		if err != nil {
			return "", err
		}
		author = answered.author
	}
	if author != "" {
		return author, nil
	}
	return edited.owner, nil
}

// readAnsweredMessage reads a message a write answers. Its owner is the reader of its issue, and
// for a message on no issue, which is a direct conversation, the person who opened the
// conversation: nobody where a session opened it.
func readAnsweredMessage(ctx context.Context, q queryer, id string) (answeredText, error) {
	if _, err := uuid.Parse(id); err != nil {
		return answeredText{}, nil
	}
	var message answeredText
	var kind, author string
	var issueKey *string
	err := q.QueryRow(ctx, `
		select coalesce(m.author ->> 'kind', ''), coalesce(m.author ->> 'id', ''), m.issue_key, `+ownerReaderExpression+`
		from messages m
		left join issues i on i.key = m.issue_key
		left join projects p on p.key = i.project_key
		where m.id = $1`, id).Scan(&kind, &author, &issueKey, &message.owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return answeredText{}, nil
	}
	if err != nil {
		return answeredText{}, fmt.Errorf("read the reader of message %s: %w", id, err)
	}
	message.author = personLogin(kind, author)
	if issueKey != nil || message.author != "" {
		return message, nil
	}
	err = q.QueryRow(ctx, messageThreadCTE+`
		select coalesce(m.author ->> 'kind', ''), coalesce(m.author ->> 'id', '')
		from messages m join thread t on t.id = m.id
		where t.in_reply_to is null`, id).Scan(&kind, &author)
	if err != nil {
		return answeredText{}, fmt.Errorf("read the person who opened the conversation of message %s: %w", id, err)
	}
	message.owner = personLogin(kind, author)
	return message, nil
}

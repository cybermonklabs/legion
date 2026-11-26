package dispatch

import (
	"context"
	"net/http"
	"net/url"
)

// The answer loop's writes, each made as the stopped agent's own session (sessionActor) rather than
// the daemon's (daemonActor): Dispatch accepts a reply to a delivery only from the session the
// delivery went to (packages/envoy/internal/dispatch/api message_delivery.go replyMessage,
// comment_delivery.go replyComment), and an answer reads as the agent's.

// Comment is a comment as an ask's or a thread's read lists it: its id, its author and its body.
type Comment struct {
	ID     string `json:"id"`
	Author Actor  `json:"author"`
	Body   string `json:"body"`
}

// AskThread is an ask as GET /api/v1/asks/{id} reads it with its thread: its id, its issue (nil
// for a document's ask), who opened it, and every reply on its thread, oldest first. The same
// route's state and closer are Ask's (HTTPClient.Ask), which leaves the thread undecoded.
type AskThread struct {
	ID       string
	IssueKey *string
	Author   Actor
	Replies  []Comment
}

// Thread is a comment's thread as GET /api/v1/comments/{id} reads it: the comment, its root, and
// every reply under it, oldest first.
type Thread struct {
	Root    Comment
	Replies []Comment
}

// ReplyToMessage answers a targeted message's delivery attempt as session, the attempt's own
// (POST /api/v1/messages/{id}/reply). Dispatch keeps one reply per attempt: a second call for an
// answered attempt posts nothing and answers the stored reply, so a re-post after a restart is
// safe.
func (c *HTTPClient) ReplyToMessage(ctx context.Context, messageID string, attempt int, session, body string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/messages/"+url.PathEscape(messageID)+"/reply", struct {
		Actor   actor  `json:"actor"`
		Attempt int    `json:"attempt"`
		Body    string `json:"body"`
	}{Actor: sessionActor(session), Attempt: attempt, Body: body}, nil)
}

// ReplyToComment answers a mention's delivery attempt as session, the attempt's own (POST
// /api/v1/comments/{id}/reply), naming the mention it answers (target). Dispatch keeps one reply
// per attempt, so a re-post after a restart posts nothing.
func (c *HTTPClient) ReplyToComment(ctx context.Context, commentID, target string, attempt int, session, body string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/comments/"+url.PathEscape(commentID)+"/reply", struct {
		Actor   actor  `json:"actor"`
		Target  string `json:"target"`
		Attempt int    `json:"attempt"`
		Body    string `json:"body"`
	}{Actor: sessionActor(session), Target: target, Attempt: attempt, Body: body}, nil)
}

// ReplyOnAsk comments on the ask's thread of issue as session (POST /api/v1/issues/{key}/comments
// with ask_id). Dispatch keeps no key for it, so a caller reads the thread first (AskThread) to
// post it once.
func (c *HTTPClient) ReplyOnAsk(ctx context.Context, issue, askID, session, body string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/issues/"+url.PathEscape(issue)+"/comments", struct {
		Actor actor  `json:"actor"`
		Body  string `json:"body"`
		AskID string `json:"ask_id"`
	}{Actor: sessionActor(session), Body: body, AskID: askID}, nil)
}

// ReplyInThread comments in the thread of commentID on issue as session (POST
// /api/v1/issues/{key}/comments with reply_to): Dispatch climbs reply_to to the thread's root and
// records commentID as the comment the reply answers. Dispatch keeps no key for it, so a caller
// reads the thread first (Thread) to post it once.
func (c *HTTPClient) ReplyInThread(ctx context.Context, issue, commentID, session, body string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/issues/"+url.PathEscape(issue)+"/comments", struct {
		Actor   actor  `json:"actor"`
		Body    string `json:"body"`
		ReplyTo string `json:"reply_to"`
	}{Actor: sessionActor(session), Body: body, ReplyTo: commentID}, nil)
}

// AskThread reads an ask with its replies (GET /api/v1/asks/{id}).
func (c *HTTPClient) AskThread(ctx context.Context, askID string) (AskThread, error) {
	var response struct {
		Ask struct {
			ID       string  `json:"id"`
			IssueKey *string `json:"issue_key"`
			Author   Actor   `json:"author"`
		} `json:"ask"`
		Replies []Comment `json:"replies"`
	}
	if err := c.request(ctx, http.MethodGet, "/api/v1/asks/"+url.PathEscape(askID), nil, &response); err != nil {
		return AskThread{}, err
	}
	return AskThread{ID: response.Ask.ID, IssueKey: response.Ask.IssueKey, Author: response.Ask.Author, Replies: response.Replies}, nil
}

// Thread reads a comment with every reply in its thread (GET /api/v1/comments/{id}).
func (c *HTTPClient) Thread(ctx context.Context, commentID string) (Thread, error) {
	var response struct {
		Comment Comment   `json:"comment"`
		Replies []Comment `json:"replies"`
	}
	if err := c.request(ctx, http.MethodGet, "/api/v1/comments/"+url.PathEscape(commentID), nil, &response); err != nil {
		return Thread{}, err
	}
	return Thread{Root: response.Comment, Replies: response.Replies}, nil
}

package api

// going_link_words.go keeps the words the model gave a link to an item in each answer of a run of
// the command bar's agent, and links each issue key the answer names bare that a read of the run
// returned. The bar draws an answer with chips (web features/going/ConversationView, MarkdownBody
// with chips): each link to an item shows the item in place of the link's words, and the eval's
// judge reads an answer the same way (going/eval's RenderAnswer). So a fact the model wrote as a
// link's words reached no one, though the prompt tells it to write each fact outside the link
// (going/chips' KeepLinkWords says what the eval measured), and an issue it named by a bare key,
// with no link, showed no chip, so no one read its title (going/chips' LinkBareKeys). The server
// writes each answer so as it stores the answer's frame (AgentListener.step): the run's events
// are the one text the bar's conversation, History and the eval all read an answer from. The
// server writes it rather than the agent since it reads each item the answer links from the
// store, as the bar reads it for its chip, and the run's tool results from its saved steps, where
// the agent holds only what its reads happened to return.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/going/chips"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingReplyFrame is the name of the frame of a model call of a run, which the agent saves with
// its step (going/agent's frameReply and replyFrame).
const goingReplyFrame = "reply"

// goingKeepLinkWords writes, in place, the text of each reply frame of frames that answers - one
// that names no tool call, which the bar draws as the run's answer (web
// features/going/run-frames.ts) - with each bare issue key that a tool result of run holds
// written as the issue's link (chips.LinkBareKeys, goingRunResults), so the answer links only
// items its run read, and then as chips.KeepLinkWords writes it, each item the text links read
// from q for run's person (goingAnswerItems). Every other frame, and a reply frame that writes as
// it came, stays as it came, byte for byte. A reply frame of tool calls is the header the bar
// writes over its calls in plain words, with no chip, so it stays as it came too.
func goingKeepLinkWords(ctx context.Context, q queryer, run string, frames []agentwire.Frame) error {
	results := &goingRunResults{ctx: ctx, q: q, run: run}
	items := &goingAnswerItems{ctx: ctx, q: q, run: run}
	for at, frame := range frames {
		var reply struct {
			Text  string   `json:"text"`
			Tools []string `json:"tools"`
		}
		if frame.Name != goingReplyFrame || json.Unmarshal(frame.Data, &reply) != nil || reply.Text == "" || len(reply.Tools) > 0 {
			continue
		}
		linked := chips.LinkBareKeys(reply.Text, results.returned)
		if results.err != nil {
			return results.err
		}
		kept := chips.KeepLinkWords(linked, items)
		if items.err != nil {
			return items.err
		}
		if kept == reply.Text {
			continue
		}
		// The frame decoded into a struct above, so it is an object, and its members decode.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(frame.Data, &fields); err != nil {
			return fmt.Errorf("read the reply frame of run %s: %w", run, err)
		}
		// A string always marshals, and so does a map of what it decoded into.
		fields["text"], _ = json.Marshal(kept)
		frames[at].Data, _ = json.Marshal(fields)
	}
	return nil
}

// goingRunResults is the result text of each tool step of a run, read from q once, when it is
// first asked of a key, so a step whose answer names no bare key reads none. Every tool step of
// the run is saved in an earlier step call than the model step whose reply answers, so the read
// holds them all. A read that fails keeps its error (err), and no key is returned after it.
type goingRunResults struct {
	ctx     context.Context
	q       queryer
	run     string
	results []string
	read    bool
	err     error
}

// returned reports whether key stands as a whole token in a tool result of the run
// (chips.HoldsKey): a read of the run returned it.
func (r *goingRunResults) returned(key string) bool {
	if !r.read {
		r.read = true
		rows, err := r.q.Query(r.ctx, `
			select body->>'result' from going_run_steps
			where run_id = $1::uuid and kind = $2 and body->>'result' is not null
			order by step
		`, r.run, agentwire.StepTool)
		if err == nil {
			r.results, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		if err != nil {
			r.err = fmt.Errorf("read the tool results of run %s: %w", r.run, err)
		}
	}
	return r.err == nil && slices.ContainsFunc(r.results, func(result string) bool { return chips.HoldsKey(result, key) })
}

// The reads of goingAnswerItems: an issue's status and title by its key, a document's or a file's
// name by its ref_key (its owner and its slug), an ask's question, a comment's body, and the body
// of a message on an issue ("" for none) by their ids, each where the reader bound last may see
// it. An id in no uuid spelling reads no row (store.CastableUUID).
var (
	goingAnswerIssueQuery = `select i.status, i.title from issues i where i.key = $1 and ` +
		refs.VisibleKind("issue", "i.key", "$2")
	goingAnswerDocumentQuery = `select a.name from artifacts a where a.ref_key = $1 and ` +
		refs.VisibleKind("artifact", "a.ref_key", "$2")
	goingAnswerAskQuery = `select k.question from asks k where k.id = ` + store.CastableUUID("$1::text") + ` and ` +
		refs.VisibleKind("ask", "k.id::text", "$2")
	goingAnswerCommentQuery = `select c.body from comments c where c.id = ` + store.CastableUUID("$1::text") + ` and ` +
		refs.VisibleKind("comment", "c.id::text", "$2")
	goingAnswerMessageQuery = `select m.body from messages m where m.id = ` + store.CastableUUID("$1::text") +
		` and coalesce(m.issue_key, '') = $2 and ` + refs.VisibleKind("message", "m.id::text", "$3")
)

// goingAnswerItems is what the bar draws for each item an answer links (chips.Items), read from q
// for the run's person, as the bar reads it for them: an item reserved for another author
// (refs.VisibleKind) is one it holds nothing of, as is one no row holds. It reads the person's
// login with its first item. A read that fails keeps its error (err), and nothing more is read.
type goingAnswerItems struct {
	ctx context.Context
	q   queryer
	run string
	// reader is the person's credential as a visibility predicate binds it (refs.VisibilityArg),
	// read once asked is set.
	reader any
	asked  bool
	err    error
}

// IssueChip is the issue's status and title.
func (r *goingAnswerItems) IssueChip(key string) (string, string, bool) {
	var status, title string
	found := r.read(goingAnswerIssueQuery, []any{&status, &title}, key)
	return status, title, found
}

// DocumentName is the name of the document or file of owner with slug.
func (r *goingAnswerItems) DocumentName(owner, slug string) (string, bool) {
	var name string
	found := r.read(goingAnswerDocumentQuery, []any{&name}, owner+"/"+slug)
	return name, found
}

// ItemText is the question of the ask, or the body of the comment or of the message on issue,
// stored under id.
func (r *goingAnswerItems) ItemText(kind, issue, id string) (string, bool) {
	var written string
	var found bool
	switch kind {
	case "ask":
		found = r.read(goingAnswerAskQuery, []any{&written}, id)
	case "comment":
		found = r.read(goingAnswerCommentQuery, []any{&written}, id)
	case "message":
		found = r.read(goingAnswerMessageQuery, []any{&written}, id, issue)
	}
	return written, found
}

// read scans the row query answers for args, and the person's credential after them, into into,
// and reports whether there was one.
func (r *goingAnswerItems) read(query string, into []any, args ...any) bool {
	if r.err != nil {
		return false
	}
	if !r.asked {
		var login string
		if err := r.q.QueryRow(r.ctx, `select author_login from going_runs where id = $1::uuid`, r.run).Scan(&login); err != nil {
			r.err = fmt.Errorf("read the person of run %s: %w", r.run, err)
			return false
		}
		r.reader, r.asked = refs.VisibilityArg(model.Actor{Kind: "user", ID: login}), true
	}
	err := r.q.QueryRow(r.ctx, query, append(args, r.reader)...).Scan(into...)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false
	case err != nil:
		r.err = fmt.Errorf("read an item the answer of run %s links: %w", r.run, err)
		return false
	}
	return true
}

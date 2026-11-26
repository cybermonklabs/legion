package api

// going_link_words.go keeps the words the model gave a link to an item in each answer of a run of
// the command bar's agent, links each issue key the answer names bare that a read of the run
// returned, and takes out of it each code block that holds a block the bar draws itself. The bar
// draws an answer with chips (web features/going/ConversationView, MarkdownBody with chips): the
// first link to each issue shows the issue's chip in place of the link's words, and the eval's
// judge reads an answer the same way (going/chips' RenderAnswer). So a fact the model wrote as the
// words of such a link reached no one, though the prompt tells it to write each fact outside the
// link (going/chips' KeepLinkWords says what the eval measured), and an issue it named by a bare
// key, with no link, showed no chip, so no one read its title (going/chips' LinkBareKeys). In 1 of
// 10 tries of "Chart the DEMO project's issues by status." at 9fc43c69 the answer wrote the block
// draw_block drew, its JSON in a code block whose info string was draw, which the bar drew as a
// box of code above the chart itself (goingDrawnFence). The server writes each answer so as it
// stores the answer's frame (AgentListener.step): the run's events are the one text the bar's
// conversation, History and the eval all read an answer from. The server writes it rather than
// the agent since it reads each item the answer links from the store, as the bar reads it for its
// chip, and the run's tool results from its saved steps, where the agent holds only what its reads
// happened to return. The agent's words call counts the words the bar draws of that same text
// (going_agent_words.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/going/chips"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingReplyFrame is the name of the frame of a model call of a run, which the agent saves with
// its step (going/agent's frameReply and replyFrame), and goingBlockFrame that of the block a
// draw_block call of the run drew, as the server answered it, which the agent saves with the
// call's tool step (going/agent's frameBlock).
const (
	goingReplyFrame = "reply"
	goingBlockFrame = "block"
)

// goingKeepLinkWords writes, in place, the text of each reply frame of frames that answers - one
// that names no tool call, which the bar draws as the run's answer (web
// features/going/run-frames.ts) - as goingStoredAnswer writes it for run, each tool result of run
// and each item the text links read from q for run's person. Every other frame, and a reply frame
// that writes as it came, stays as it came, byte for byte. A reply frame of tool calls is the
// header the bar writes over its calls in plain words, with no chip, so it stays as it came too.
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
		kept, err := goingStoredAnswer(reply.Text, results, items)
		if err != nil {
			return err
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

// goingStoredAnswer is text, a reply of a run that answers, as the server stores it in the reply's
// frame and the bar draws it, from the run's tool results (results) and the items it links
// (items): each fenced code block of it that holds a block the bar draws itself taken out
// (chips.DropCodeBlocks, goingRunResults.drawnFence), every other block staying byte for byte;
// then each bare issue key that a tool result of the run holds and that names an issue the run's
// person may read written as the issue's link (chips.LinkBareKeys), so the answer links only
// items its run read, and a word of a key's shape that is no issue, such as SHA-256 or UTF-8,
// stays text; and then as chips.KeepLinkWords writes it. It answers the error of a read that
// failed.
func goingStoredAnswer(text string, results *goingRunResults, items *goingAnswerItems) (string, error) {
	dropped := chips.DropCodeBlocks(text, results.drawnFence)
	if results.err != nil {
		return "", results.err
	}
	linked := chips.LinkBareKeys(dropped, func(key string) bool {
		if !results.returned(key) {
			return false
		}
		_, _, issue := items.IssueChip(key)
		return issue
	})
	if results.err != nil {
		return "", results.err
	}
	if items.err != nil {
		return "", items.err
	}
	kept := chips.KeepLinkWords(linked, items)
	if items.err != nil {
		return "", items.err
	}
	return kept, nil
}

// goingRunResults is the result text of each tool step of a run, read from q once, when it is
// first asked of a key, and each block the run drew, read from q once, when it is first asked of a
// code block that holds JSON, so an answer that names no bare key and holds no such block reads
// neither. Every tool step of the run is saved, with the block frame of a block it drew, in an
// earlier step call than the model step whose reply answers, so each read holds them all. A read
// that fails keeps its error (err), and nothing is found after it.
type goingRunResults struct {
	ctx     context.Context
	q       queryer
	run     string
	results []string
	// blocks is each block the run drew, its block frame's data as it decodes.
	blocks []any
	// read and drawn are set once results, and blocks, were read.
	read, drawn bool
	err         error
}

// returned reports whether key stands as a whole token in a tool result of the run
// (chips.HoldsKey): a read of the run returned it.
func (r *goingRunResults) returned(key string) bool {
	if !r.read && r.err == nil {
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

// loadBlocks reads the blocks the run drew, its events' block frames, once, and reports whether
// no read failed.
func (r *goingRunResults) loadBlocks() bool {
	if r.drawn || r.err != nil {
		return r.err == nil
	}
	r.drawn = true
	rows, err := r.q.Query(r.ctx, `select data from going_run_events where run_id = $1::uuid and name = $2`, r.run, goingBlockFrame)
	var frames [][]byte
	if err == nil {
		frames, err = pgx.CollectRows(rows, pgx.RowTo[[]byte])
	}
	if err != nil {
		r.err = fmt.Errorf("read the blocks run %s drew: %w", r.run, err)
		return false
	}
	for _, data := range frames {
		var block any
		if json.Unmarshal(data, &block) == nil {
			r.blocks = append(r.blocks, block)
		}
	}
	return true
}

// goingDrawFence is the info string of a code block that holds a block for the bar to draw.
const goingDrawFence = "draw"

// drawnFence reports whether a fenced code block of an answer, of info string info and with body,
// holds a block the bar draws itself beside the answer: the first word of its info string is
// draw, in any case, or its body is the JSON of a block the run drew, as that JSON decodes, its
// spacing and the order of its members aside. Such a block drawn in the answer too is a box of
// code the person scrolls sideways, whose numbers the model wrote (R3).
func (r *goingRunResults) drawnFence(info, body string) bool {
	if words := strings.Fields(info); len(words) > 0 && strings.EqualFold(words[0], goingDrawFence) {
		return true
	}
	var written any
	if json.Unmarshal([]byte(body), &written) != nil || !r.loadBlocks() {
		return false
	}
	return slices.ContainsFunc(r.blocks, func(block any) bool { return reflect.DeepEqual(block, written) })
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
// login with its first item where it was not given the person's credential (asked), and each item
// once: a read made again answers what the first answered. A read that fails keeps its error
// (err), and nothing more is read.
type goingAnswerItems struct {
	ctx context.Context
	q   queryer
	run string
	// reader is the person's credential as a visibility predicate binds it (refs.VisibilityArg),
	// read, or given, once asked is set.
	reader any
	asked  bool
	// rows is what each read scanned, by its query and its arguments, nil for no row.
	rows map[goingItemRead][]string
	err  error
}

// goingItemRead is one read of goingAnswerItems: its query, and its arguments as %q writes them.
type goingItemRead struct {
	query, args string
}

// IssueChip is the issue's status and title.
func (r *goingAnswerItems) IssueChip(key string) (string, string, bool) {
	var status, title string
	found := r.read(goingAnswerIssueQuery, []*string{&status, &title}, key)
	return status, title, found
}

// DocumentName is the name of the document or file of owner with slug.
func (r *goingAnswerItems) DocumentName(owner, slug string) (string, bool) {
	var name string
	found := r.read(goingAnswerDocumentQuery, []*string{&name}, owner+"/"+slug)
	return name, found
}

// ItemText is the question of the ask, or the body of the comment or of the message on issue,
// stored under id.
func (r *goingAnswerItems) ItemText(kind, issue, id string) (string, bool) {
	var written string
	var found bool
	switch kind {
	case "ask":
		found = r.read(goingAnswerAskQuery, []*string{&written}, id)
	case "comment":
		found = r.read(goingAnswerCommentQuery, []*string{&written}, id)
	case "message":
		found = r.read(goingAnswerMessageQuery, []*string{&written}, id, issue)
	}
	return written, found
}

// read scans the row query answers for args, and the person's credential after them, into into,
// and reports whether there was one; a read of the same query and args made before answers as it
// did (rows).
func (r *goingAnswerItems) read(query string, into []*string, args ...any) bool {
	if r.err != nil {
		return false
	}
	key := goingItemRead{query: query, args: fmt.Sprintf("%q", args)}
	if row, held := r.rows[key]; held {
		for at, value := range row {
			*into[at] = value
		}
		return row != nil
	}
	if !r.asked {
		var login string
		if err := r.q.QueryRow(r.ctx, `select author_login from going_runs where id = $1::uuid`, r.run).Scan(&login); err != nil {
			r.err = fmt.Errorf("read the person of run %s: %w", r.run, err)
			return false
		}
		r.reader, r.asked = refs.VisibilityArg(model.Actor{Kind: "user", ID: login}), true
	}
	scanned := make([]any, len(into))
	for at, value := range into {
		scanned[at] = value
	}
	err := r.q.QueryRow(r.ctx, query, append(args, r.reader)...).Scan(scanned...)
	var row []string
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		r.err = fmt.Errorf("read an item the answer of run %s links: %w", r.run, err)
		return false
	default:
		row = make([]string, len(into))
		for at, value := range into {
			row[at] = *value
		}
	}
	if r.rows == nil {
		r.rows = map[goingItemRead][]string{}
	}
	r.rows[key] = row
	return row != nil
}

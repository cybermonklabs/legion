package harness

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"text/template"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/claude"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
)

// writerFiles are the prompts of the writer the harness plays (changes 2, 3, 5, 6 and 7a), apart
// from the reader's and the grader's prompts/, so change 1's prompt digest does not move with
// them.
//
//go:embed writer/*.md
var writerFiles embed.FS

var writerPrompts = template.Must(template.New("writer").ParseFS(writerFiles, "writer/*.md"))

// WriterDigest is the SHA-256 of every writer prompt, by name: what a run that plays the writer
// was made with.
func WriterDigest() string {
	h := sha256.New()
	names, _ := fs.Glob(writerFiles, "writer/*.md")
	slices.Sort(names)
	for _, name := range names {
		data, _ := writerFiles.ReadFile(name)
		fmt.Fprintf(h, "%s %d\n", name, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writerData is what a writer prompt is filled from.
type writerData struct {
	Kind       string
	Ask        bool
	HasSession bool
}

// writerMaxTokens bounds the writer's reply: a document written again. llm asks again with twice
// the bound for a reply cut at it.
const writerMaxTokens = 16000

// The writer's prompts (writer/writer.md): its text written again, its action on a review's
// findings, its answer to a side question.
const (
	promptRewrite  = "writer-rewrite"
	promptFindings = "writer-findings"
	promptAnswer   = "writer-answer"
)

// The orders of the writer's user message (score --writer-order). writerSessionLast, the
// default, is the order the harness has always sent: Dispatch's record, the writer's text,
// Dispatch's message, then the session part. writerSessionFirst sends the record and the session
// part, then the text and the message: the order of the harness's first cache layout, kept so
// that a change can be scored with each order to see how much its effect depends on where its
// words sit.
const (
	writerSessionLast  = "session-last"
	writerSessionFirst = "session-first"
)

// Writer is the writer agent the harness plays (plan Decision 20): the Claude model of
// candidates.yaml's writer role, given Dispatch's record of the case, its own text, the message
// Dispatch sent it, and its session up to the text where the case has one. Only a claude.Client
// takes the session (claude.SessionText). Order is the order of its user message,
// writerSessionLast where it is "".
type Writer struct {
	Model *claude.Client
	Order string
}

// writerTurn is one message Dispatch sends the writer about a case's text.
type writerTurn struct {
	kind    string
	text    string
	record  string
	session claude.SessionText
	message string
}

// call makes the writer's call for prompt with the turn's inputs and its session, if any, as one
// user message in the writer's order. The session part follows the text before it after a blank
// line (claude.Client.CallWithSession). The record, the text, the message and the session part
// are embedded (messageTags), so each text the writer writes back is read unescaped (unembedded).
func (w *Writer) call(ctx context.Context, prompt, purpose string, turn writerTurn, schema json.RawMessage) (llm.Response, error) {
	var b strings.Builder
	if err := writerPrompts.ExecuteTemplate(&b, prompt, writerData{
		Kind: turn.kind, Ask: turn.kind == "ask", HasSession: !turn.session.Empty(),
	}); err != nil {
		return llm.Response{}, fmt.Errorf("fill the %s prompt: %w", prompt, err)
	}
	record, rest := strings.TrimSpace(turn.record)+"\n", tag("your_text", turn.text)+tag("dispatch_message", turn.message)
	req := llm.Request{Purpose: purpose, System: strings.TrimSpace(b.String()) + "\n", User: record + rest, Schema: schema, MaxTokens: writerMaxTokens}
	after := ""
	if w.Order == writerSessionFirst {
		req.User, after = record, rest
	}
	return w.Model.CallWithSession(ctx, req, turn.session.Embedded(messageTags), after)
}

// rewriteSchema is the schema of the writer's text written again.
var rewriteSchema = schemaJSON(object(property{"text", stringSchema}))

// Rewrite is the writer's text written again after message: the written rules' refusal (change
// 2) or the writing check's refusal (change 7a).
func (w *Writer) Rewrite(ctx context.Context, purpose string, turn writerTurn) (string, Call, error) {
	resp, err := w.call(ctx, promptRewrite, purpose, turn, rewriteSchema)
	call := callOf(resp)
	if err != nil {
		return "", call, err
	}
	var reply struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(resp.Value, &reply); err != nil || strings.TrimSpace(reply.Text) == "" {
		return "", call, fmt.Errorf("the writer's %s reply holds no text: %s", purpose, resp.Value)
	}
	return unembedded(strings.TrimSpace(reply.Text)), call, nil
}

// The writer's actions on a review's findings (change 3).
const (
	actionEdit = "edit"
	actionPost = "post"
)

// findingsSchema is the schema of the writer's action on a review's findings.
var findingsSchema = schemaJSON(object(
	property{numbered(0, "action"), enum(actionEdit, actionPost)},
	property{numbered(1, "text"), stringSchema},
	property{numbered(2, "reason"), stringSchema},
))

// FindingsAction is what the writer did with a review's findings: edited its text, or posted it as
// written with a reason.
type FindingsAction struct {
	Action string `json:"action"`
	Text   string `json:"text,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ActOnFindings is the writer's action on the frame of a review's findings (reviewer.FindingsFrame).
func (w *Writer) ActOnFindings(ctx context.Context, turn writerTurn) (FindingsAction, Call, error) {
	resp, err := w.call(ctx, promptFindings, "writer:findings", turn, findingsSchema)
	call := callOf(resp)
	if err != nil {
		return FindingsAction{}, call, err
	}
	var reply map[string]string
	if err := json.Unmarshal(resp.Value, &reply); err != nil {
		return FindingsAction{}, call, fmt.Errorf("the writer's findings reply: %w", err)
	}
	act := FindingsAction{Action: reply[numbered(0, "action")], Text: unembedded(strings.TrimSpace(reply[numbered(1, "text")])), Reason: unembedded(strings.TrimSpace(reply[numbered(2, "reason")]))}
	switch {
	case act.Action == actionEdit && act.Text == "":
		return act, call, fmt.Errorf("the writer edited its text to nothing: %s", resp.Value)
	case act.Action != actionEdit && act.Action != actionPost:
		return act, call, fmt.Errorf("the writer's action %q is neither edit nor post", act.Action)
	}
	return act, call, nil
}

// Answer is the writer's answer to Dispatch's side question about its text (changes 5 and 6):
// text the agent writes for the reader, which a model of any family may then read.
func (w *Writer) Answer(ctx context.Context, turn writerTurn) (string, Call, error) {
	resp, err := w.call(ctx, promptAnswer, "writer:answer", turn, nil)
	call := callOf(resp)
	if err != nil {
		return "", call, err
	}
	answer := unembedded(strings.TrimSpace(resp.Text))
	if answer == "" {
		return "", call, fmt.Errorf("the writer gave an empty answer")
	}
	return answer, call, nil
}

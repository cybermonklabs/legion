// Package claude is the harness's Claude client, called through the operator's model gateway,
// and the only code of the harness that reads a writer session's text (INFRA-3231's R2, after
// INFRA-3189's rule on transcript text: session text reaches Claude models only).
//
// Session text is SessionText, a type whose text no code outside this package can read: it has
// no exported field, no method that returns it, and it prints as a placeholder. The one method
// that sends it to a model is Client.CallWithSession, and a Client always calls a Claude model
// over Anthropic Messages. Package llm, which holds the Foundry client, cannot name SessionText,
// since this package imports llm. So a call that would send session text to a model of another
// family does not compile.
package claude

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
	"github.com/sjawhar/envoy/internal/dispatch/prompttags"
)

// SessionText is the part of a writer session that a case reads: the session up to the place
// where the text was written, cut to its newest characters (Excerpts).
type SessionText struct{ text string }

// Chars is how many bytes of text the session part holds; 0 for none.
func (s SessionText) Chars() int { return len(s.text) }

// Empty is whether the case has no session text.
func (s SessionText) Empty() bool { return s.text == "" }

// Digest is the SHA-256 of the session part, for a record of what a call read.
func (s SessionText) Digest() [sha256.Size]byte { return sha256.Sum256([]byte(s.text)) }

// Holds is whether the session part holds quote, both in QuoteForm. So the harness checks a
// grader's quote of a session part with no code outside this package reading the text.
func (s SessionText) Holds(quote string) bool {
	q := QuoteForm(quote)
	return q != "" && strings.Contains(QuoteForm(s.text), q)
}

// Embedded is the session part as a message holds it: each opening and closing of one of tags'
// tags escaped (prompttags), so the session text opens and closes no part of the message, the
// session part's own tag (SessionTag) among them. The harness sends it (CallWithSession) and
// digests it as a call's input; it looks the words a reply copies up in the session part as the
// export holds it (Holds), with the escape undone.
func (s SessionText) Embedded(tags *prompttags.Tags) SessionText {
	return SessionText{text: tags.Escape(s.text)}
}

// QuoteForm is text as a quote is looked up in it: lowercased, without Markdown's emphasis and
// code marks, with typographic quotes made plain and each run of white space one space. It is in
// this package so that a session part is looked up in the form every other text is (Holds).
func QuoteForm(text string) string {
	text = strings.NewReplacer("*", "", "`", "", "\u2019", "'", "\u2018", "'", "\u201c", "\"", "\u201d", "\"").Replace(strings.ToLower(text))
	return strings.Join(strings.Fields(text), " ")
}

// Format prints a placeholder in place of the text, whatever the verb, so no fmt call can carry
// the text into a string.
func (s SessionText) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "[session text: %d bytes]", len(s.text))
}

// Client is one Claude model at the model gateway.
type Client struct {
	model  agentloop.Model
	runner *llm.Runner
}

// New is the client of Claude model id at gateway (the gateway's base URL, which serves
// Anthropic Messages), with the key in keyFile, read on each call and never printed, at the
// reasoning effort effort ("" is none: thinking off), priced at price.
func New(gateway, keyFile, id, effort string, price agentloop.Price, runner *llm.Runner) (*Client, error) {
	switch {
	case gateway == "":
		return nil, errors.New("no model gateway: give --gateway or set ANTHROPIC_BASE_URL")
	case keyFile == "":
		return nil, errors.New("no gateway key: give --gateway-key-file")
	case id == "":
		return nil, errors.New("no Claude model id")
	}
	if effort != "" && !slices.Contains([]string{"off", "low", "medium", "high"}, effort) {
		return nil, fmt.Errorf("the effort of %s is %q, not off, low, medium or high", id, effort)
	}
	tokens := func(context.Context) (string, error) {
		key, err := os.ReadFile(keyFile)
		if err != nil {
			return "", fmt.Errorf("reading the gateway key file: %w", err)
		}
		return strings.TrimSpace(string(key)), nil
	}
	return &Client{
		model: agentloop.Model{
			Endpoint: agentloop.Endpoint{Kind: agentloop.AnthropicMessages, BaseURL: strings.TrimRight(gateway, "/"), Token: tokens},
			ID:       id,
			Effort:   cmp.Or(effort, "off"),
			Price:    price,
		},
		runner: runner,
	}, nil
}

// Name is the model's id.
func (c *Client) Name() string { return c.model.ID }

// Family is llm.FamilyClaude.
func (c *Client) Family() string { return llm.FamilyClaude }

// Model is the model as agentloop calls it, for a component that makes its own call (the
// reviewer's Rewrite and Define, the detector's reader). It carries no session text: only
// CallWithSession sends that.
func (c *Client) Model() agentloop.Model { return c.model }

// Call makes one call that reads no session text.
func (c *Client) Call(ctx context.Context, req llm.Request) (llm.Response, error) {
	return c.runner.Run(ctx, c.model, req)
}

// SessionTag is the tag the session part stands in.
const SessionTag = "writer_session"

// CallWithSession makes one call whose user message is req.User, then, after a blank line, the
// session part in its tag, then after. With after "" the session part ends the user message, as
// every call but the writer's under --writer-order session-first sends it. An empty session part
// adds nothing: the user message is req.User, then after.
func (c *Client) CallWithSession(ctx context.Context, req llm.Request, session SessionText, after string) (llm.Response, error) {
	if !session.Empty() {
		req.User = strings.TrimRight(req.User, "\n") + "\n\n<" + SessionTag + ">\n" + session.text + "\n</" + SessionTag + ">\n"
	}
	req.User += after
	return c.runner.Run(ctx, c.model, req)
}

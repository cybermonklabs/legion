package answer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// systemPrompt is the short system prompt the agent's resumed conversation runs under: who it is
// and that it answers while it is not running, its tools (the list, gh named, and the sentence on
// where gh runs and what every call names that the gh tool's description and its refusals carry
// too: ghtool.Usage.Checkout), the Legion footer the tool puts on what it posts, the work it
// declines, and the reply form.
func systemPrompt(req Request) string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = "`" + t.name + "`"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are the agent whose conversation follows: the %s of issue %s (session %s, phase %s).", req.Agent.Role, req.Agent.Issue, req.Agent.Session, req.Agent.Phase)
	b.WriteString(" You are not running now, and Legion has resumed your conversation for one message to you. Answer it as yourself. Your conversation is your first source: answer from it what it holds, and use your tools only for what it does not hold.\n\n")
	fmt.Fprintf(&b, "Your tools now: %s. ", strings.Join(names, ", "))
	fmt.Fprintf(&b, "The `gh` tool is GitHub's own command line with your GitHub access. %s Give it gh's arguments without the leading gh; its description lists the commands it takes and gives an example. ", req.Agent.GHUsage.Checkout())
	b.WriteString("Every comment or review you post with it carries your Legion footer, which the tool adds, so do not write one. ")
	b.WriteString("The other tools your conversation used (reading and editing files, running commands, your Legion and Envoy tools) are not available now: do not call them. ")
	fmt.Fprintf(&b, "This answer may make at most %d gh calls and %d model calls; your last model call offers no tool and must be your reply.\n\n", req.Bounds.GHCalls, req.Bounds.ModelCalls)
	b.WriteString("Decline work your tools cannot do, such as work that needs your working copy or a run of your phase: say plainly that you cannot do it now and what it needs, and do not promise to do it later. Say plainly when you do not know.\n\n")
	b.WriteString("Your reply is exactly one JSON object and nothing else, no code fence: {\"kind\": ..., \"text\": ..., \"needs\": ...}, all three always there. The kinds:\n")
	b.WriteString("- \"answered\": text is your answer to the message; needs is \"\".\n")
	b.WriteString("- \"not_answered\": you cannot answer from your conversation or what your tools read; text says so to the asker, and needs says what you would need, such as your working copy.\n")
	b.WriteString("- \"needs_work\": the message asks for work your tools cannot do; text tells the asker you cannot do it now, and needs names the work.\n")
	if req.MayAskNothing {
		b.WriteString("- \"none\": the message asks nothing of you; text is \"\" and needs is \"\".\n")
	}
	return b.String()
}

// lastCall is the message the answer's last model call carries (agentloop.Request.LastCall).
// That call offers no tool, which the system prompt says once, far above; a model not told so on
// the call itself has spent it on one more tool call it could not make, or on a reply with nothing
// in it. It also names the reply form's needs rule (parseReply), since that call's reply is never
// asked for again: a not_answered or needs_work reply naming no need there is malformed, and the
// asker hears nothing of what is needed. The message is the loop's own instruction, as the system
// prompt is, and the append holds no trace of it (Reply.Append).
const lastCall = "This is your last model call, and it offers no tool. Reply now, in the reply form, from what you already know. " +
	"A not_answered or needs_work reply must say in needs what you need, and needs is never empty for them."

// userMessage is the one user message after the conversation: what has changed on the issue, the
// question with who asked and where, and on a re-run the gh calls already made with their results.
func userMessage(req Request) string {
	var b strings.Builder
	if note := strings.TrimSpace(req.ChangeNote); note != "" {
		fmt.Fprintf(&b, "What has changed on your issue since you stopped:\n%s\n\n", note)
	}
	fmt.Fprintf(&b, "%s asked you, in %s:\n%s", req.Question.Asker, req.Question.Where, req.Question.Text)
	if len(req.Earlier) > 0 {
		b.WriteString("\n\nAn earlier attempt at this answer already made these gh calls; do not repeat a write among them:\n")
		for _, call := range req.Earlier {
			args, _ := json.Marshal(call.Args)
			fmt.Fprintf(&b, "\n%d. gh %s\n", call.Seq, args)
			switch {
			case call.Result == nil:
				b.WriteString("Outcome unknown: it may or may not have run.\n")
			case call.Exit == nil:
				fmt.Fprintf(&b, "Did not run:\n%s\n", *call.Result)
			default:
				fmt.Fprintf(&b, "Exit %d:\n%s\n", *call.Exit, *call.Result)
			}
		}
	}
	return b.String()
}

// form is a reply in the form.
type form struct {
	kind  Kind
	text  string
	needs string
}

// finalSchema is the JSON schema of the reply, which agentloop sends to the provider and checks
// the last reply against: exactly one object of the form, its kind one of those this message
// offers (none only for a message that may ask nothing), and its text and needs strings.
//
// Every property is required and none takes more than one type, because that is the shape a
// provider constrains its own generation to: measured against Anthropic's Messages API on
// 2026-11-07, a form whose needs was `["string","null"]` and left out of required was carried in
// output_config.format and then not enforced, and the model ran a one-sentence answer to the
// whole output token bound; with this shape the same question stopped at 77 output tokens with
// the object. What the schema cannot say as a constraint inside that strict subset, which takes
// no minLength, pattern or anyOf — a not_answered or needs_work reply must name a need, and every
// kind but none must carry text — its descriptions say to the model, parseReply checks after,
// and a reply parseReply refuses is asked for once again (answer).
func finalSchema(mayAskNothing bool) json.RawMessage {
	var schema replySchema
	schema.Type = "object"
	schema.Properties.Kind.Type = "string"
	schema.Properties.Kind.Enum = []Kind{Answered, NotAnswered, NeedsWork}
	schema.Properties.Text.Type = "string"
	schema.Properties.Text.Description = "What the asker reads. Never empty."
	schema.Properties.Needs.Type = "string"
	schema.Properties.Needs.Description = "For not_answered, what you would need to answer, such as your working copy; for needs_work, the work the message asks for. Never empty for those two kinds; empty for answered."
	if mayAskNothing {
		schema.Properties.Kind.Enum = append(schema.Properties.Kind.Enum, None)
		schema.Properties.Text.Description = "What the asker reads. Never empty, but for none, where it is empty."
		schema.Properties.Needs.Description = "For not_answered, what you would need to answer, such as your working copy; for needs_work, the work the message asks for. Never empty for those two kinds; empty for answered and for none."
	}
	schema.Required = []string{"kind", "text", "needs"}
	encoded, _ := json.Marshal(schema)
	return encoded
}

// replySchema is the shape of the reply form (finalSchema): kind, one of the request's kinds, and
// text and needs, two strings, all three required and no other property. Its fields are structs
// rather than maps so the form is declared in one fixed order, kind, text, needs, which agentloop
// sends as declared and the model writes its reply in (agentloop's doc.go, "The structured
// reply"); and the schema is one Go value marshalled once, so no text is joined into JSON by hand.
type replySchema struct {
	Type       string `json:"type"`
	Properties struct {
		Kind struct {
			Type string `json:"type"`
			Enum []Kind `json:"enum"`
		} `json:"kind"`
		Text  describedString `json:"text"`
		Needs describedString `json:"needs"`
	} `json:"properties"`
	Required             []string `json:"required"`
	AdditionalProperties bool     `json:"additionalProperties"`
}

// describedString is a string property of the reply form with the description the model reads.
type describedString struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

// parseReply reads the reply agentloop checked against finalSchema: its kind one the request
// offers, its text non-empty but for none, and its needs non-empty for not_answered and
// needs_work.
func parseReply(value json.RawMessage, mayAskNothing bool) (form, error) {
	var reply struct {
		Kind  *string `json:"kind"`
		Text  *string `json:"text"`
		Needs *string `json:"needs"`
	}
	if err := json.Unmarshal(value, &reply); err != nil {
		return form{}, fmt.Errorf("the reply is not the reply form: %w", err)
	}
	if reply.Kind == nil || reply.Text == nil {
		return form{}, errors.New("the reply has no kind or no text")
	}
	f := form{kind: Kind(*reply.Kind), text: *reply.Text}
	if reply.Needs != nil {
		f.needs = *reply.Needs
	}
	switch f.kind {
	case Answered:
	case NotAnswered, NeedsWork:
		if strings.TrimSpace(f.needs) == "" {
			return form{}, fmt.Errorf("a %s reply names no needs", f.kind)
		}
	case None:
		if !mayAskNothing {
			return form{}, errors.New("the reply is none, which this message does not offer")
		}
		return f, nil
	default:
		return form{}, fmt.Errorf("the reply's kind %q is none of the form's", f.kind)
	}
	if strings.TrimSpace(f.text) == "" {
		return form{}, fmt.Errorf("a %s reply with no text", f.kind)
	}
	return f, nil
}

// placeholderSentence is the sentence a reply whose text is a placeholder is asked for again with
// (errPlaceholder). It is stored and matched on read: agentloop sends it after the refused reply
// and records it on that reply's step, exchange carries it into the append
// (conversation.Turn.AskedAgain), and the appender writes it into the agent's conversation as a
// user message after the refused reply. A later answer's placeholder rule, matching it
// (askedAgain), leaves that message and the refused reply before it out of the words it reads
// (inFront.holds); the model is still sent the whole conversation.
const placeholderSentence = "your reply's text is a placeholder: answer the question from your conversation"

// errPlaceholder is the refusal of a reply whose text is a placeholder, asked for again with
// placeholderSentence.
var errPlaceholder = errors.New(placeholderSentence)

// placeholderSentences is every sentence a placeholder has been asked for again with, the
// current placeholderSentence and each earlier text, since conversations keep each one
// (askedAgain). A change to placeholderSentence appends the old text here rather than replacing
// it, or every exchange stored with the old text would stop being left out.
var placeholderSentences = []string{placeholderSentence}

// placeholder says whether text, the text of a reply of any kind but none, is a placeholder
// rather than an answer: trimmed, it is one or more letters and nothing else (no digit, no
// punctuation, no space), it is neither yes nor no, and holds says it is no word of what the
// agent has in front of it as it replies: the conversation, the question and the run's gh
// results (inFront.holds).
//
// The rule is this narrow because a wider one refuses right answers. Replayed on 2026-11-19 as
// whole loops, a guard refusing any one word or any text under 12 characters refused all 12
// terse right answers ("SBOM-ZINC-512", to "Reply with the marker alone"), and all 12 ended
// malformed after the one ask again. This rule refused none of 24 terse right answers (the
// marker, and "No" to a yes-or-no question) and caught the one "x" of 24 loops on a long
// question, which the ask again answered right.
func placeholder(text string, holds func(string) bool) bool {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) }) ||
		strings.EqualFold(text, "yes") || strings.EqualFold(text, "no") {
		return false
	}
	return !holds(text)
}

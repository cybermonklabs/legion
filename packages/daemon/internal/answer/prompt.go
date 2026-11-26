package answer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/sjawhar/legion/daemon/internal/dispatchtool"
	"github.com/sjawhar/legion/daemon/internal/websearch"
)

// systemPrompt is the short system prompt the agent's resumed conversation runs under: who it is
// and that it answers while it is not running; when it replies and when it calls a tool; where the
// daemon posts the reply, and that the agent posts no copy of it; its tools (the list of those its
// request offers, gh named, and the sentence on where gh runs and what every call names that the
// gh tool's description and its refusals carry too: ghtool.Usage.Checkout; dispatch, where the
// request offers it, named with the session it runs as, and when the agent can use it to write;
// and web_search, where the request offers it, with the sources its reply names), the Legion
// footer and the loop's mark the tools put on what they post, the bounds of each tool's calls and
// of the model calls, and, while the project is paused as the run starts (Request.Paused), that its
// tools run their reads and refuse every write; then the work it declines, and the reply form.
//
// It has the model reply as soon as it can answer, on whichever call that is, having made first
// each write the message asks for, and make a tool call only to do what the message asks for or
// for what the reply needs and the conversation does not hold: never one to try a tool, to wait or
// to pass a turn, and `--help` only before a command the message asks for. The bounds are
// limits, and the last call, which offers no tool (lastCall), is the latest the reply can come on,
// not the place it belongs. On proof run 45683361567, under a prompt saying the last call "must be
// your reply", of the Claude answers with no fault 6 ran to the 8-call bound and 2 to 7, each
// replying on or near its last call after calls that did nothing: a refused command, whoami,
// --help, and one file read seven times.
func systemPrompt(req Request) string {
	offered := req.offeredTools()
	names := make([]string, len(offered))
	for i, t := range offered {
		names[i] = "`" + t.name + "`"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are the agent whose conversation follows: the %s of issue %s (session %s, phase %s).", req.Agent.Role, req.Agent.Issue, req.Agent.Session, req.Agent.Phase)
	b.WriteString(" You are not running now. Legion resumed your conversation for one message to you. Answer it as yourself.\n\n")
	b.WriteString("Your conversation is your first source. Answer from it what it holds. " +
		"Reply as soon as you can answer. You can reply on any model call. " +
		"If the message asks for a write and you make it, make it before you reply. " +
		"Make a tool call only to do what the message asks for, or to get something that your reply needs and your conversation does not hold. " +
		"Never make a tool call to try a tool, to wait or to pass a turn. " +
		"Use `--help` only before a command that the message asks for.\n\n")
	b.WriteString("The daemon posts your reply where the message reached you:\n" +
		"- In the same comment thread.\n" +
		"- On the same ask.\n" +
		"- As the reply to the Dispatch message.\n" +
		"- To the Envoy session that asked.\n\n" +
		"Never post your answer or a copy of it with any tool, including `gh`.\n\n")
	fmt.Fprintf(&b, "Your tools now: %s. ", strings.Join(names, ", "))
	fmt.Fprintf(&b, "The `gh` tool is GitHub's own command line with your GitHub access. %s Give it gh's arguments without the leading gh. Its description lists the commands it takes and gives an example. ", req.Agent.GHUsage.Checkout())
	b.WriteString("Every comment or review that you post with it carries your Legion footer, which the tool adds. Do not write a footer.\n\n")
	if req.Dispatch != nil {
		fmt.Fprintf(&b, "The `dispatch` tool is Dispatch's own command line, run as your own session, %s. "+
			"Use it to read any issue. Its description lists the commands it runs, and it refuses every other command. "+
			"Every comment or message that you post with it carries the loop's mark, which the tool adds. "+
			"Never use `comment`, `message` or `send-to-agent` to post your answer or a copy of it. "+
			"Use a `dispatch` write only if the message explicitly asks you to:\n"+
			"- Post other content somewhere else, such as on another issue, in another thread, or to another agent.\n"+
			"- Resolve a comment.\n\n", req.Agent.Session)
	}
	if req.WebSearch != nil {
		b.WriteString("The `web_search` tool runs one web search for you. Use it only for facts that your conversation and your other tools do not hold. " +
			"If you use it, name in your reply the address of each source you use.\n\n")
	}
	b.WriteString("The other tools that your conversation used are not available now: reading and editing files, running commands, and your Legion and Envoy tools. Do not call them. ")
	bounds := []string{fmt.Sprintf("at most %d gh calls", req.Bounds.GHCalls)}
	if req.Dispatch != nil {
		bounds = append(bounds, fmt.Sprintf("at most %d dispatch calls, counted apart from its gh calls", req.Bounds.GHCalls))
	}
	if req.WebSearch != nil {
		bounds = append(bounds, fmt.Sprintf("at most %d web searches", websearch.MaxSearches))
	}
	and := " and "
	if len(bounds) > 1 {
		and = ", and "
	}
	fmt.Fprintf(&b, "This answer may make %s%s%d model calls. These are limits, not targets. "+
		"Your last model call offers no tool. If you reach it, reply on it.\n\n", strings.Join(bounds, ", "), and, req.Bounds.ModelCalls)
	if req.paused() {
		writes := "a gh comment, review, merge or other write"
		if req.Dispatch != nil {
			writes += ", and the dispatch writes " + strings.Join(dispatchtool.WriteCommands, ", ")
		}
		fmt.Fprintf(&b, "Legion has paused your project, and until it resumes the daemon makes no write in your name: your tools run their reads, and refuse every write (%s) as the call's result. When the message asks for a write, say in your reply that the project is paused and the write was not made.\n\n", writes)
	}
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

// lastCall is the message the answer's last model call carries (agentloop.Request.LastCall): the
// last call model_calls allows, or on Anthropic's Messages API the call after a reply out of form
// that agentloop dropped (finalSchema). That call offers no tool, which the system prompt says
// once, far above, as the limit a reply comes on at the latest, the model replying on whichever
// call it can answer on; this message is the one that tells the model to reply now. A model not
// told so on the call itself has spent it on one more tool call it could not make, or on a reply
// with nothing in it. It also names the reply form's needs rule (parseReply), since that call's
// reply is never asked for again: a not_answered or needs_work reply naming no need there is
// malformed, and the asker hears nothing of what is needed. The message is the loop's own
// instruction, as the system prompt is, and the append holds no trace of it (Reply.Append).
const lastCall = "This is your last model call, and it offers no tool. Reply now, in the reply form, from what you already know. " +
	"A not_answered or needs_work reply must say in needs what you need, and needs is never empty for them."

// userMessage is the one user message after the conversation: what has changed on the issue, the
// question with who asked and where, and on a re-run the tool calls already made, each named with
// its tool, with their results.
func userMessage(req Request) string {
	var b strings.Builder
	if note := strings.TrimSpace(req.ChangeNote); note != "" {
		fmt.Fprintf(&b, "What has changed on your issue since you stopped:\n%s\n\n", note)
	}
	fmt.Fprintf(&b, "%s asked you, in %s:\n%s", req.Question.Asker, req.Question.Where, req.Question.Text)
	if len(req.Earlier) > 0 {
		b.WriteString("\n\nAn earlier attempt at this answer already made these tool calls; do not repeat a write among them:\n")
		for _, call := range req.Earlier {
			args, _ := json.Marshal(call.Args)
			fmt.Fprintf(&b, "\n%d. %s %s\n", call.Seq, call.Tool, args)
			switch {
			case call.Result == nil:
				b.WriteString("Outcome unknown: it may or may not have run.\n")
			case call.RefusedBeforeRun():
				fmt.Fprintf(&b, "Did not run:\n%s\n", *call.Result)
			case call.Exit == nil:
				// It started and answered no exit: it could not start, or the daemon stopped while it
				// ran and a restart recorded what is known of it, so it is never said not to have run.
				fmt.Fprintf(&b, "No exit status:\n%s\n", *call.Result)
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
// every reply against: exactly one object of the form, its kind one of those this message offers
// (none only for a message that may ask nothing), and its text and needs strings. On OpenAI's
// Responses API every model call carries it. On Anthropic's Messages API only a call that offers
// no tool does, the answer's last: with the form on every call, Claude answers that needed no
// tool still made tool calls to the model_calls bound, and the requests of proof run 45683361567,
// replayed without it, replied 100 times in 162 against 19 with it (agentloop's doc.go, "The
// structured reply"). A reply out of form on a call that offered a tool is dropped there, never
// sent back and never appended (exchange), and the next call is the answer's last (lastCall).
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

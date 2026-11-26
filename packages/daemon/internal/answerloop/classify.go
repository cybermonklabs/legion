package answerloop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Kind is where a question came from, which says where its answer is posted.
type Kind string

const (
	// KindMessage is a Dispatch message sent to the agent (on an issue, or on its conversation
	// page), answered through the delivery attempt's reply route.
	KindMessage Kind = "message"
	// KindMention is a Dispatch comment mentioning the agent, answered through the mention's
	// delivery attempt.
	KindMention Kind = "mention"
	// KindAskReply is a comment on the thread of an ask the agent opened, or a person's answer to
	// that ask, answered on that thread.
	KindAskReply Kind = "ask_reply"
	// KindThreadReply is a reply in a comment thread that mentions nobody and follows the agent's
	// comment: the newest comment before it is the agent's, or a person's in a thread whose root is
	// the agent's (commentSkip). It is answered in that thread, and may ask nothing.
	KindThreadReply Kind = "thread_reply"
	// KindEnvoy is another agent's Envoy message, answered by a direct message back to its sender.
	KindEnvoy Kind = "envoy"
)

// AnswerMarker opens the marker every reply the loop posts carries, `<!-- legion-answer:<id> -->`:
// a hand-off carrying it is the loop's own answer and is never answered, and the loop reads an
// ask's thread for it before posting there again. It is intake's, the one constant, because the
// Dispatch decoder reads a comment carrying it as no fact of the workflow's (intake.AnswerMarker).
const AnswerMarker = intake.AnswerMarker

// answerDedupePrefix marks the loop's own Envoy publishes (a forward's dedupe key).
const answerDedupePrefix = "legion-answer:"

// outboxDedupePrefix marks the daemon's own notices (record.OutboxKey).
const outboxDedupePrefix = "legion-outbox:"

// Origin is a question as its hand-off carries it: where it came from and its identity, its text,
// who asked and where (for the model, and for the controller when it is passed on), whether the
// agent may reply it asks nothing, and what posting its answer needs.
type Origin struct {
	Kind          Kind
	Identity      string
	Text          string
	Asker         string
	Where         string
	Ref           string
	MayAskNothing bool
	// MessageID and Attempt are a message's delivery; CommentID, Target and Attempt a mention's,
	// and Thread or AskID the thread the mention is in, where Dispatch puts the reply to it: the
	// root of its comment thread, or the ask that thread hangs under (none for a root comment).
	MessageID string
	CommentID string
	Target    string
	Attempt   int
	// Issue and AskID are an ask reply's thread, and CommentID the reply on it (none for a
	// person's answer to the ask); Issue, CommentID and Thread a thread reply's: the issue, the
	// reply, and the root of its thread.
	Issue  string
	AskID  string
	Thread string
	// Sender and EventID are an Envoy message's: who to answer and the envelope answered.
	Sender  string
	EventID string
}

// Verdict is what a hand-off is: a question to answer, an answer of the loop's own to deliver
// into the conversation of the agent it was sent to (Delivery), a decision to pass to Decisions
// (a person's answer or reply on the agent's ask that also asks something is both), or neither,
// Because says why.
type Verdict struct {
	Question *Origin
	// Delivery is the loop's own answer, sent in one stopped agent's name to an agent that is
	// stopped too and so handed back to this daemon. It is never answered — no model call, no
	// spend, nothing posted — and is written into the asking agent's conversation as a message it
	// received, so that agent holds the answer when it runs again, as a running one holds what
	// the listener delivered to it live. Its Identity, the envelope's dedupe key, is the
	// delivery's own.
	Delivery *Origin
	Decision *Decision
	Because  string
	// ForAgent is a hand-off that is no question and no decision but is the agent's own: any
	// Dispatch event that reached it and asks it nothing, such as a message, a comment or a reply,
	// or the resolution, answer or edit of an ask it follows, or a message of a source no agent
	// answers. An agent whose claim the daemon released is given it through Wakes, as it would have
	// read it running. The loop's own answers and publishes, the daemon's notices and posts, the
	// broker's messages and a person's review of the agent's own approval request
	// (reviewForArchitect) are not, and wake nobody.
	ForAgent bool
}

// Reader reads what Classify decides a hand-off by: an ask's author (Dispatch.AskThread), and the
// thread of a comment a reply is in (Dispatch.Thread).
type Reader interface {
	AskThread(ctx context.Context, askID string) (dispatch.AskThread, error)
	Thread(ctx context.Context, commentID string) (dispatch.Thread, error)
}

// QuestionID is a question's own id: 32 hex digits of a hash of the project, its identity and the
// role token of the agent it was handed to, the same however often it is handed off and whichever
// conversation of that agent answers it.
func QuestionID(project, identity, token string) string {
	sum := sha256.Sum256([]byte(project + "\x00" + identity + "\x00" + token))
	return hex.EncodeToString(sum[:16])
}

// Classify decides, before any model call, what a hand-off for conv is. Answered: a Dispatch
// message sent to the agent; a comment that mentions it (its mention's delivery); a comment on the
// thread of an ask it opened (a follower that did not open the ask answers nothing); a reply in a
// comment thread that mentions nobody, which the one agent the thread names answers
// (commentSkip) and which may ask nothing; another agent's Envoy message, which may ask
// nothing. A decision (Decision), whose question part, if any, is answered too and may ask
// nothing: a person's answer to a question the agent asked, and a person's reply on the thread of
// an ask it opened that hands the ask back to it (intake.DispatchEvent.HandsTurnBack). Never: a
// root comment that does not mention the agent, an edit, a resolution, a delivery receipt, a
// claim, an issue event, the agent's own hand-back of an approval request to the person, an
// answer to an approval request (its document's review reaches the architect), a reply the agent
// already got (*.answered), the daemon's own notices and posts, the broker's (source envoy), and
// anything carrying the loop's own marker (its dedupe key, the `<!-- legion-answer:` marker, a
// Legion footer with an `answer` key), which an agent's message carrying it is delivered into
// that agent's conversation for instead (Delivery). Of the hand-offs never answered, a Dispatch
// event that is not the daemon's own post, not the loop's own answer and not a review of the
// agent's approval request, and a message of a source no agent answers, are the agent's own all
// the same (Verdict.ForAgent), which an agent whose claim is released is given through Wakes. The
// ask's author and the reply's thread are read from Dispatch.
func Classify(ctx context.Context, h intake.Handoff, conv store.AgentConversation, reader Reader) (Verdict, error) {
	env := h.Envelope
	switch {
	case strings.Contains(env.DedupeKey, answerDedupePrefix) || carriesLoopMark(env.PayloadSummary) || carriesLoopMark(env.Payload):
		// The loop's own answer, however it carries the mark, is never answered. One that came
		// as another agent's message is the answer this daemon sent in a stopped agent's name,
		// handed back because the asker is stopped too: it is delivered into the asker's
		// conversation. The loop's own publishes, which its dedupe key names (a question
		// forwarded to a role topic), carry no answer and deliver nothing; the loop holds that
		// question's own record already.
		if env.Source == "agent" && env.SourceSession != "" && !strings.Contains(env.DedupeKey, answerDedupePrefix) {
			origin, err := ReadOrigin(h)
			if err != nil {
				return Verdict{}, err
			}
			return Verdict{Delivery: &origin, Because: "the loop's own answer"}, nil
		}
		return Verdict{Because: "the loop's own answer"}, nil
	case strings.HasPrefix(env.DedupeKey, outboxDedupePrefix):
		return Verdict{Because: "the daemon's own notice"}, nil
	case env.Source == "envoy":
		return Verdict{Because: "the broker's own message"}, nil
	case env.Source == "dispatch":
		return classifyDispatch(ctx, h, conv, reader)
	case env.Source == "agent" && env.SourceSession != "":
		origin, err := ReadOrigin(h)
		if err != nil {
			return Verdict{}, err
		}
		return Verdict{Question: &origin}, nil
	default:
		return Verdict{Because: fmt.Sprintf("a message from %q no agent answers", env.Source), ForAgent: true}, nil
	}
}

// classifyDispatch is Classify of a Dispatch event. Every event it takes for no question and no
// decision is still the agent's own (Verdict.ForAgent), whatever it was not answered for: Dispatch
// routed it to the agent, which would have read it running, a resolution of an ask the agent
// follows as much as a message. Its reasons say why the loop does not answer an event, not that
// the event is another's. The loop's own answer and the daemon's own post are not the agent's,
// nor is a person's review of the agent's own approval request, which the daemon's workflow
// gives its architect (reviewForArchitect).
func classifyDispatch(ctx context.Context, h intake.Handoff, conv store.AgentConversation, reader Reader) (Verdict, error) {
	event, err := intake.DispatchOf(h.Envelope)
	if err != nil {
		return Verdict{Because: err.Error()}, nil
	}
	switch {
	case carriesLoopMark(event.Payload.Body):
		return Verdict{Because: "the loop's own answer"}, nil
	case strings.HasPrefix(event.Actor.ID, dispatch.DaemonSessionPrefix):
		return Verdict{Because: "the daemon's own post"}, nil
	}
	verdict, err := classifyDispatchEvent(ctx, h, conv, reader, event)
	verdict.ForAgent = err == nil && verdict.Question == nil && verdict.Decision == nil && verdict.Because != reviewForArchitect
	return verdict, err
}

// reviewForArchitect is why a person's review of the agent's own approval request is neither
// answered nor the agent's: it arrives again as the document's review, which the daemon's design
// gate relays to the agent's architect (workflow's dispatchAskClosed, record.RelaysAskAnswer).
const reviewForArchitect = "a review of the agent's document, which reaches its architect as the document's review"

// classifyDispatchEvent is classifyDispatch of an event that is neither the loop's own answer nor
// the daemon's own post.
func classifyDispatchEvent(ctx context.Context, h intake.Handoff, conv store.AgentConversation, reader Reader, event intake.DispatchEvent) (Verdict, error) {
	ownAsk := func(askID string) (bool, error) {
		ask, err := reader.AskThread(ctx, askID)
		if err != nil {
			return false, fmt.Errorf("read the author of ask %s: %w", askID, err)
		}
		return ask.Author.Kind == "session" && ask.Author.ID == h.TargetSession, nil
	}
	var decision *Decision
	switch event.Type {
	case "message.created":
		if event.Delivery == nil {
			return Verdict{Because: "a message not sent to the agent, with no delivery to answer"}, nil
		}
	case "comment.created":
		if event.Payload.AskID == nil {
			if because, err := commentSkip(ctx, h, event, reader); err != nil || because != "" {
				return Verdict{Because: because}, err
			}
			break
		}
		// A reply on an ask's thread can also mention the agent; it is a decision all the same.
		own, err := ownAsk(*event.Payload.AskID)
		if err != nil {
			return Verdict{}, err
		}
		switch {
		case own && event.HandsTurnBack():
			if decision, err = readDecision(event, conv); err != nil {
				return Verdict{Because: err.Error()}, nil
			}
		case !own && event.Delivery == nil:
			return Verdict{Because: "a reply on an ask the agent follows but did not open"}, nil
		}
	case "ask.answered":
		own, err := ownAsk(event.Payload.ID)
		switch {
		case err != nil:
			return Verdict{}, err
		case !own:
			return Verdict{Because: "an answer to an ask the agent did not open"}, nil
		case !event.Actor.Person():
			return Verdict{Because: "an answer no person gave"}, nil
		case event.Payload.Kind == "approval":
			return Verdict{Because: reviewForArchitect}, nil
		}
		if decision, err = readDecision(event, conv); err != nil {
			return Verdict{Because: err.Error()}, nil
		}
		if answer := event.Payload.Answer; answer == nil || answer.Text == nil || strings.TrimSpace(*answer.Text) == "" {
			return Verdict{Decision: decision, Because: "a decision whose answer has no text, which asks nothing"}, nil
		}
	default:
		return Verdict{Because: "a " + event.Type + " event, which asks the agent nothing"}, nil
	}
	origin, err := ReadOrigin(h)
	if err != nil {
		return Verdict{Decision: decision, Because: err.Error()}, nil
	}
	if decision != nil {
		// What the person decided may ask the agent nothing besides.
		origin.MayAskNothing = true
	}
	return Verdict{Question: &origin, Decision: decision}, nil
}

// commentSkip is why the agent h is for does not answer event, a comment on no ask's thread, or ""
// when it answers it. A comment that mentions the agent is answered through its mention's
// delivery; a root comment that does not is not answered. A reply in the comment thread of
// event.Payload.ReplyTo that delivers no mention is answered by one agent alone, and only when a
// person wrote it: an agent's reply there, the agent's own among them, is no question to the loop,
// nor is a reply that mentions anyone, which is answered only as a mention by each agent it
// mentions, nor one in a document's thread, which has no issue to answer on. Else
// the thread is read, and its answerer is the author of the newest comment before the reply when
// an agent wrote it, else the author of the thread's root when an agent wrote that, else nobody:
// the loop's own answer, posted in the agent's session, is the agent's comment. Dispatch sends a
// person's reply to every agent that wrote in the thread, each hand-off a question of its own, so
// the answerer is the same on every agent's hand-off, and the agent answers only when it is that
// answerer, compared as ownAsk compares an ask's author. A failed read is an error, so the
// hand-off is tried again.
func commentSkip(ctx context.Context, h intake.Handoff, event intake.DispatchEvent, reader Reader) (string, error) {
	p := event.Payload
	switch {
	case event.Delivery != nil:
		return "", nil
	case p.ReplyTo == nil:
		return "a root comment that does not mention the agent", nil
	case !event.Actor.Person():
		return "a reply in a comment thread that no person wrote", nil
	case len(p.Mentions) > 0:
		return "a reply in a comment thread that mentions someone, answered only as a mention by each agent it mentions", nil
	case eventIssue(event) == "":
		return "a reply in a document's comment thread, which has no issue thread to answer on", nil
	}
	root := *p.ReplyTo
	thread, err := reader.Thread(ctx, root)
	if err != nil {
		return "", fmt.Errorf("read the comment thread of %s: %w", root, err)
	}
	at := slices.IndexFunc(thread.Replies, func(c dispatch.Comment) bool { return c.ID == p.ID })
	if at < 0 {
		return fmt.Sprintf("a reply the comment thread of %s does not hold", root), nil
	}
	answerer, wrote := thread.Root.Author, "root"
	if at > 0 && thread.Replies[at-1].Author.Kind == "session" {
		answerer, wrote = thread.Replies[at-1].Author, "newest comment before the reply"
	}
	switch {
	case answerer.Kind != "session":
		return "a reply in a comment thread whose root and newest comment before the reply no agent wrote", nil
	case answerer.ID != h.TargetSession:
		return fmt.Sprintf("a reply in a comment thread that session %s answers, as the author of its %s", answerer.ID, wrote), nil
	}
	return "", nil
}

// readDecision is the decision a person's answer to the agent's ask, or their reply handing it
// back, carries for conv. Its id is the Dispatch event's for an answer (a person who changes their
// answer makes a new decision, a new event) and the comment's for a reply. An answer carries the
// ask's kind, which says whether the parked-decision relay sends it (Decision.AskKind); a comment
// event names none. The issue the event names is carried as the ask's own (Decision.AskIssue),
// the issue that relay reads, which is the agent's own only while its ask stands on it.
func readDecision(event intake.DispatchEvent, conv store.AgentConversation) (*Decision, error) {
	p := event.Payload
	issue := eventIssue(event)
	d := &Decision{Asker: event.Actor.Kind + ":" + event.Actor.ID, Conversation: conv, Text: decisionText(event), AskIssue: issue}
	switch event.Type {
	case "ask.answered":
		if event.ID <= 0 || p.ID == "" {
			return nil, fmt.Errorf("the Dispatch ask.answered names no event id or ask id")
		}
		d.ID, d.Ref = dispatchEventIdentity(event.ID), itemRef(issue, "ask", p.ID)
		d.Answered, d.AskKind = true, p.Kind
	default:
		if p.ID == "" {
			return nil, fmt.Errorf("the Dispatch %s names no comment id", event.Type)
		}
		d.ID, d.Ref = "dispatch:"+p.ID, itemRef(issue, "comment", p.ID)
	}
	return d, nil
}

// decisionText is what a person decided: an answer's selected options and its text, a reply's body.
func decisionText(event intake.DispatchEvent) string {
	answer := event.Payload.Answer
	if event.Type != "ask.answered" {
		return event.Payload.Body
	}
	if answer == nil {
		return ""
	}
	var parts []string
	if len(answer.Selected) > 0 {
		parts = append(parts, "Selected: "+strings.Join(answer.Selected, "; "))
	}
	if answer.Text != nil && strings.TrimSpace(*answer.Text) != "" {
		parts = append(parts, *answer.Text)
	}
	return strings.Join(parts, "\n\n")
}

// dispatchEventIdentity is the identity of a Dispatch event that is neither a message nor a
// comment, an ask's answer: its event id.
func dispatchEventIdentity(id int64) string { return "dispatch:" + strconv.FormatInt(id, 10) }

// eventIssue is the issue a Dispatch event's payload names, else the event's own, else "" (a
// document's or the agent's conversation page's).
func eventIssue(event intake.DispatchEvent) string {
	switch {
	case event.Payload.IssueKey != nil:
		return *event.Payload.IssueKey
	case event.IssueKey != nil:
		return *event.IssueKey
	}
	return ""
}

// itemRef is the Dispatch reference of an issue's ask or comment, `dispatch://<KEY>/<kind>/<id>`,
// or `dispatch <kind> <id>` for one on a document, which has no issue.
func itemRef(issue, kind, id string) string {
	if issue == "" {
		return "dispatch " + kind + " " + id
	}
	return "dispatch://" + issue + "/" + kind + "/" + id
}

// ReadOrigin reads what a question's hand-off says about it, without reading anything else: its
// kind, identity, text, asker and where to post its answer. It is how the loop reads a recorded
// question's hand-off again. A Dispatch asker is `<kind>:<id>` (`user:<login>`, or
// `session:<id>`); an Envoy asker is `session:<id>`, followed by the session's title in brackets
// when the listener stamped one on the envelope.
func ReadOrigin(h intake.Handoff) (Origin, error) {
	env := h.Envelope
	if env.Source != "dispatch" {
		text := env.Payload
		if text == "" {
			text = env.PayloadSummary
		}
		asker := "session:" + env.SourceSession
		if env.Sender != nil && env.Sender.SessionID == env.SourceSession && env.Sender.Title != "" {
			asker += " (" + env.Sender.Title + ")"
		}
		return Origin{
			Kind: KindEnvoy, Identity: env.DedupeKey, Text: text, Asker: asker,
			Where: "an Envoy message from session " + env.SourceSession, Ref: "envoy:" + env.EventID, MayAskNothing: true,
			Sender: env.SourceSession, EventID: env.EventID,
		}, nil
	}
	event, err := intake.DispatchOf(env)
	if err != nil {
		return Origin{}, err
	}
	p := event.Payload
	if p.ID == "" {
		return Origin{}, fmt.Errorf("the Dispatch %s of %s names no id", event.Type, env.EventID)
	}
	issue := eventIssue(event)
	o := Origin{Identity: "dispatch:" + p.ID, Text: p.Body, Asker: event.Actor.Kind + ":" + event.Actor.ID, Issue: issue}
	switch {
	case event.Type == "message.created" && event.Delivery != nil:
		o.Kind, o.MessageID, o.Attempt = KindMessage, p.ID, event.Delivery.Attempt
		if issue == "" {
			o.Where, o.Ref = "a message on the agent's Dispatch conversation page", "dispatch message "+p.ID
		} else {
			o.Where, o.Ref = "a Dispatch message on "+issue, "dispatch://"+issue+"/message/"+p.ID
		}
	case event.Type == "comment.created" && event.Delivery != nil:
		o.Kind, o.CommentID, o.Target, o.Attempt = KindMention, p.ID, event.Delivery.Target, event.Delivery.Attempt
		if p.ReplyTo != nil {
			o.Thread = *p.ReplyTo
		}
		if p.AskID != nil {
			o.AskID = *p.AskID
		}
		o.Where, o.Ref = "a Dispatch comment mentioning the agent on "+issue, "dispatch://"+issue+"/comment/"+p.ID
	case event.Type == "comment.created" && p.AskID != nil:
		if issue == "" {
			return Origin{}, fmt.Errorf("the reply %s is on a document's ask, which has no issue thread to answer on", p.ID)
		}
		o.Kind, o.CommentID, o.AskID = KindAskReply, p.ID, *p.AskID
		o.Where, o.Ref = "a reply on the agent's ask "+*p.AskID+" on "+issue, "dispatch://"+issue+"/comment/"+p.ID
		// A person's reply that hands the ask back to the agent is a decision, which may ask nothing.
		o.MayAskNothing = event.HandsTurnBack()
	case event.Type == "comment.created" && event.Delivery == nil && p.AskID == nil && p.ReplyTo != nil:
		if issue == "" {
			return Origin{}, fmt.Errorf("the reply %s is in a document's comment thread, which has no issue thread to answer on", p.ID)
		}
		// A reply in a thread may answer what came before it rather than ask: it may ask nothing.
		o.Kind, o.CommentID, o.Thread, o.MayAskNothing = KindThreadReply, p.ID, *p.ReplyTo, true
		o.Where, o.Ref = "a reply in the comment thread of "+*p.ReplyTo+" on "+issue, itemRef(issue, "comment", p.ID)
	case event.Type == "ask.answered":
		if issue == "" {
			return Origin{}, fmt.Errorf("the answer to %s is on a document's ask, which has no issue thread to answer on", p.ID)
		}
		if event.ID <= 0 {
			return Origin{}, fmt.Errorf("the Dispatch ask.answered of %s names no event id", env.EventID)
		}
		// A person's answer is a decision, which may ask nothing; what else it asks is answered on
		// the ask's thread.
		o.Kind, o.AskID, o.Identity, o.Text, o.MayAskNothing = KindAskReply, p.ID, dispatchEventIdentity(event.ID), decisionText(event), true
		o.Where, o.Ref = "an answer to the agent's ask "+p.ID+" on "+issue, itemRef(issue, "ask", p.ID)
	default:
		return Origin{}, fmt.Errorf("a Dispatch %s is no question", event.Type)
	}
	if event.Delivery != nil && o.Attempt < 1 {
		return Origin{}, fmt.Errorf("the Dispatch delivery of %s names no attempt", p.ID)
	}
	return o, nil
}

// legionFooter is a Legion footer, `<!-- legion: {…} -->`.
var legionFooter = regexp.MustCompile(`<!--\s*legion:\s*(\{[^<>]*\})\s*-->`)

// carriesLoopMark is whether text carries the loop's own marker, raw or as JSON escapes it, or a
// Legion footer with an `answer` key (a gh comment or review the loop's tool posted).
func carriesLoopMark(text string) bool {
	if strings.Contains(text, AnswerMarker) || strings.Contains(text, `\u003c!-- legion-answer:`) {
		return true
	}
	for _, match := range legionFooter.FindAllStringSubmatch(text, -1) {
		var footer map[string]json.RawMessage
		if json.Unmarshal([]byte(match[1]), &footer) == nil {
			if _, ok := footer["answer"]; ok {
				return true
			}
		}
	}
	return false
}

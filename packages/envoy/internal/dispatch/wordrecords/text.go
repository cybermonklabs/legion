package wordrecords

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
)

// text.go finds the texts people write in the event log, and reads each one with the agent text it
// answers, as the dataset command reads them (cmd/writing-check, "classify" and "The reply walk"):
// the same kinds of text, the same test of who wrote it, and a walk up the text's replies to the
// first agent text, else the agent's last text in the conversation or on the issue before it.

// carriedForward opens the reason Dispatch itself writes on an approval it carries forward
// (docs/approval_carry.go), which is no person's text.
const carriedForward = "Carried forward from the approval"

// mostReplySteps bounds the walk up a text's replies.
const mostReplySteps = 32

// eventActor is the part of a stored actor the job reads (model.Actor's JSON).
type eventActor struct {
	Kind  string  `json:"kind"`
	ID    string  `json:"id"`
	Owner *string `json:"owner"`
	Act   *struct {
		Session string `json:"session"`
	} `json:"act"`
}

func (a eventActor) actSession() string {
	if a.Act == nil {
		return ""
	}
	return a.Act.Session
}

// agent is whether the writing check reads the actor's writes: an agent session that is not a
// project's daemon, or a person's credential acting for one (writingcheck.Checks).
func (a eventActor) agent() bool {
	checked, _ := writingcheck.Checks(a.Kind, a.ID, a.actSession())
	return checked
}

// writtenBy answers the person who wrote an event's text, and whether a person wrote it. A person
// writes for themselves as a user actor with no session acting for them. A text a person typed in
// a session (Decision 26 of INFRA-3231's plan: the write carried Dispatch-Person-Typed: 1, and its
// event's payload holds person_typed: true) is that person's too: the person the actor names, or,
// for a session's own actor, the person its credential names (model.Actor.Owner, which a
// personal token sets); where it names none, such as a pod's shared token, the person is unknown,
// "", and the counts give the text apart.
func writtenBy(actor eventActor, personTyped bool) (person string, written bool) {
	switch {
	case actor.Kind == "user" && actor.actSession() == "":
		return model.CanonicalLogin(actor.ID), true
	case !personTyped:
		return "", false
	case actor.Kind == "user":
		return model.CanonicalLogin(actor.ID), true
	case actor.Kind == "session" && actor.Owner != nil:
		return model.CanonicalLogin(*actor.Owner), true
	case actor.Kind == "session":
		return "", true
	}
	return "", false
}

// payload is the part of a text event's payload the job reads: a comment's, a message's, an ask's
// (model.Ask) and a review's (model.ArtifactReviewEventPayload).
type payload struct {
	ID            string  `json:"id"`
	Kind          string  `json:"kind"`
	Body          string  `json:"body"`
	BodyTruncated bool    `json:"body_truncated"`
	IssueKey      *string `json:"issue_key"`
	ReplyTo       *string `json:"reply_to"`
	Answers       *string `json:"answers"`
	AskID         *string `json:"ask_id"`
	Anchor        *anchor `json:"anchor"`
	InReplyTo     *string `json:"in_reply_to"`
	Target        *string `json:"target"`
	BroadcastID   *string `json:"broadcast_id"`
	Question      string  `json:"question"`
	Options       []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
	Answer *struct {
		Text *string `json:"text"`
	} `json:"answer"`
	Resolution *struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	} `json:"resolution"`
	ArtifactID  string  `json:"artifact_id"`
	Version     int     `json:"version"`
	Reason      *string `json:"reason"`
	PersonTyped bool    `json:"person_typed"`
}

// anchor is the part of a document a comment points to.
type anchor struct {
	ArtifactID string `json:"artifact_id"`
	Version    int    `json:"version"`
	Quote      string `json:"quote"`
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// askText is an ask's question with its options, one line each.
func (p payload) askText() string {
	var b strings.Builder
	b.WriteString(p.Question)
	for _, o := range p.Options {
		b.WriteString("\n- " + o.Label)
		if o.Description != "" {
			b.WriteString(": " + o.Description)
		}
	}
	return b.String()
}

// textOf is the kind and the words of the text an event of eventType holds, "" for an event that
// holds none: a comment, a message, a question ask (an approval's question is Dispatch's), an
// answer with words, a note that declines, retracts or resolves an ask, and a review's reason
// (not the one Dispatch writes when it carries an approval forward).
func (p payload) textOf(eventType string) (kind, text string) {
	switch eventType {
	case "comment.created", "comment.answered":
		return "comment", p.Body
	case "message.created", "message.answered":
		return "message", p.Body
	case "ask.opened":
		if p.Kind == "approval" {
			return "", ""
		}
		return "ask", p.askText()
	case "ask.answered":
		if p.Answer != nil {
			return "answer", deref(p.Answer.Text)
		}
	case "ask.resolved":
		if p.Resolution != nil {
			switch p.Resolution.Kind {
			case "declined", "retracted", "resolved":
				return "resolution", p.Resolution.Reason
			}
		}
	case "artifact.approved", "artifact.changes_requested":
		if reason := deref(p.Reason); !strings.HasPrefix(strings.TrimSpace(reason), carriedForward) {
			return "review", reason
		}
	}
	return "", ""
}

// PersonTexts is the texts a person wrote among one event of the log, for WordRecordsIntake: none,
// or the one text it holds when a person wrote it (writtenBy) and it holds words. Every copy of a
// person's broadcast is one text.
func PersonTexts(event store.WordRecordsEvent) []store.WordRecordsText {
	var actor eventActor
	var p payload
	if json.Unmarshal(event.Actor, &actor) != nil || json.Unmarshal(event.Payload, &p) != nil {
		return nil
	}
	person, written := writtenBy(actor, p.PersonTyped)
	if !written {
		return nil
	}
	kind, text := p.textOf(event.Type)
	if kind == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	key := "event:" + strconv.FormatInt(event.ID, 10)
	if broadcast := deref(p.BroadcastID); kind == "message" && broadcast != "" {
		key = "broadcast:" + broadcast
	}
	return []store.WordRecordsText{{Key: key, Person: person, Kind: kind}}
}

// reader reads what the job classifies from the database.
type reader struct {
	pool *store.Pool
}

// input is the user message of one record's call (Input) and the agent text it answers ("" for
// none found): the person's text read from its event, its whole body read from its row where the
// event cut it.
func (r reader) input(ctx context.Context, q store.WordQuestion) (string, string, error) {
	var eventType string
	var actorJSON, payloadJSON []byte
	var issue *string
	var createdAt time.Time
	if err := r.pool.QueryRow(ctx, `select type, actor, payload, issue_key, created_at from events where id = $1`, q.EventID).
		Scan(&eventType, &actorJSON, &payloadJSON, &issue, &createdAt); err != nil {
		return "", "", fmt.Errorf("read event %d: %w", q.EventID, err)
	}
	var p payload
	if err := json.Unmarshal(payloadJSON, &p); err != nil {
		return "", "", fmt.Errorf("read event %d's payload: %w", q.EventID, err)
	}
	kind, text := p.textOf(eventType)
	if p.BodyTruncated {
		whole, err := r.wholeBody(ctx, kind, p.ID)
		if err != nil {
			return "", "", err
		}
		text = cmp.Or(whole, text)
	}
	quote := ""
	if p.Anchor != nil {
		quote = p.Anchor.Quote
	}
	agent, answers, err := r.answered(ctx, eventType, p, deref(issue), q.EventID, createdAt)
	if err != nil {
		return "", "", err
	}
	return Input(agent, quote, Text{Kind: kind, Text: text}), answers, nil
}

// wholeBody is the stored body of a comment or a message an event cut, "" where its row is gone.
func (r reader) wholeBody(ctx context.Context, kind, id string) (string, error) {
	table := map[string]string{"comment": "comments", "message": "messages"}[kind]
	if table == "" || uuid.Validate(id) != nil {
		return "", nil
	}
	var body string
	err := r.pool.QueryRow(ctx, `select body from `+table+` where id = $1::uuid`, id).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the whole body of %s %s: %w", kind, id, err)
	}
	return body, nil
}

// parentOf is the text an event's text replies to, "" for none: the comment it answers or the
// thread it replies in, the ask it replies on or answers, the document version it points to or
// reviews, the message it replies to.
func parentOf(eventType string, p payload) string {
	switch eventType {
	case "comment.created", "comment.answered":
		return commentParent(deref(p.Answers), deref(p.ReplyTo), deref(p.AskID), p.Anchor)
	case "message.created", "message.answered":
		if reply := deref(p.InReplyTo); reply != "" {
			return "message:" + reply
		}
	case "ask.answered", "ask.resolved":
		return "ask:" + p.ID
	case "artifact.approved", "artifact.changes_requested":
		return versionRef(p.ArtifactID, p.Version)
	}
	return ""
}

func commentParent(answers, replyTo, askID string, at *anchor) string {
	switch {
	case answers != "" || replyTo != "":
		return "comment:" + cmp.Or(answers, replyTo)
	case askID != "":
		return "ask:" + askID
	case at != nil:
		return versionRef(at.ArtifactID, at.Version)
	}
	return ""
}

func versionRef(artifactID string, number int) string {
	if artifactID == "" || number <= 0 {
		return ""
	}
	return "version:" + artifactID + ":" + strconv.Itoa(number)
}

// answered is the agent text an event's text answers, and its reference: up the text's replies to
// the first agent text with words; else, for a message on no issue, the agent's last message to
// its conversation before it; else the last agent text on its issue before it.
func (r reader) answered(ctx context.Context, eventType string, p payload, issue string, eventID int64, at time.Time) (Text, string, error) {
	ref := parentOf(eventType, p)
	seen := map[string]bool{}
	for steps := 0; ref != "" && !seen[ref] && steps < mostReplySteps; steps++ {
		seen[ref] = true
		text, agent, next, err := r.readRef(ctx, ref)
		if err != nil {
			return Text{}, "", err
		}
		if agent && strings.TrimSpace(text.Text) != "" {
			return text, ref, nil
		}
		ref = next
	}
	if session, ok := strings.CutPrefix(deref(p.Target), "session:"); ok && issue == "" && strings.HasPrefix(eventType, "message.") {
		var id, body string
		err := r.pool.QueryRow(ctx, `
			select id::text, body from messages
			where issue_key is null and author->>'kind' = 'session' and author->>'id' = $1 and created_at < $2
			order by created_at desc limit 1`, session, at).Scan(&id, &body)
		switch {
		case err == nil:
			return Text{Kind: "message", Text: body}, "message:" + id, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return Text{}, "", fmt.Errorf("read the conversation's last agent message: %w", err)
		}
	}
	if issue == "" {
		return Text{}, "", nil
	}
	return r.lastOnIssue(ctx, issue, eventID)
}

// lastOnIssue is the last text an agent wrote on the issue before the event: a comment, a message
// or a question ask.
func (r reader) lastOnIssue(ctx context.Context, issue string, before int64) (Text, string, error) {
	rows, err := r.pool.Query(ctx, `
		select type, actor, payload from events
		where issue_key = $1 and id < $2
			and type in ('comment.created', 'comment.answered', 'message.created', 'message.answered', 'ask.opened')
		order by seq desc limit 50`, issue, before)
	if err != nil {
		return Text{}, "", fmt.Errorf("read the issue's last agent text: %w", err)
	}
	type candidate struct {
		kind, text, id string
		truncated      bool
	}
	var found *candidate
	for rows.Next() && found == nil {
		var eventType string
		var actorJSON, payloadJSON []byte
		if err := rows.Scan(&eventType, &actorJSON, &payloadJSON); err != nil {
			rows.Close()
			return Text{}, "", fmt.Errorf("read the issue's last agent text: %w", err)
		}
		var actor eventActor
		var p payload
		if json.Unmarshal(actorJSON, &actor) != nil || !actor.agent() || json.Unmarshal(payloadJSON, &p) != nil {
			continue
		}
		if kind, text := p.textOf(eventType); kind != "" && strings.TrimSpace(text) != "" {
			found = &candidate{kind: kind, text: text, id: p.ID, truncated: p.BodyTruncated}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Text{}, "", fmt.Errorf("read the issue's last agent text: %w", err)
	}
	if found == nil {
		return Text{}, "", nil
	}
	if found.truncated {
		whole, err := r.wholeBody(ctx, found.kind, found.id)
		if err != nil {
			return Text{}, "", err
		}
		found.text = cmp.Or(whole, found.text)
	}
	return Text{Kind: found.kind, Text: found.text}, found.kind + ":" + found.id, nil
}

// readRef reads the text ref names ("comment:<id>", "ask:<id>", "message:<id>",
// "version:<document id>:<number>"): its words, whether an agent wrote it, and the text it in turn
// replies to. A text whose row is gone has no words and replies to nothing. An approval request's
// question is Dispatch's own, so its text is the version it asks about.
func (r reader) readRef(ctx context.Context, ref string) (text Text, agent bool, next string, err error) {
	kind, id, _ := strings.Cut(ref, ":")
	if kind == "version" {
		return r.readVersion(ctx, ref, id)
	}
	if uuid.Validate(id) != nil {
		return Text{}, false, "", nil
	}
	var author []byte
	switch kind {
	case "comment":
		var body string
		var replyTo, answers, askID *string
		var at *anchor
		var anchorJSON []byte
		err = r.pool.QueryRow(ctx, `
			select author, body, reply_to::text, answers::text, ask_id::text, anchor
			from comments where id = $1::uuid`, id).Scan(&author, &body, &replyTo, &answers, &askID, &anchorJSON)
		if err == nil && len(anchorJSON) > 0 {
			at = &anchor{}
			if json.Unmarshal(anchorJSON, at) != nil {
				at = nil
			}
		}
		text = Text{Kind: "comment", Text: body}
		next = commentParent(deref(answers), deref(replyTo), deref(askID), at)
	case "ask":
		var p payload
		var options []byte
		var approvalArtifact *string
		var approvalVersion *int
		err = r.pool.QueryRow(ctx, `
			select author, question, options, kind, approval->>'artifact_id', (approval->>'version')::int
			from asks where id = $1::uuid`, id).Scan(&author, &p.Question, &options, &p.Kind, &approvalArtifact, &approvalVersion)
		if err == nil && json.Unmarshal(options, &p.Options) != nil {
			p.Options = nil
		}
		if p.Kind == "approval" {
			if approvalVersion != nil {
				next = versionRef(deref(approvalArtifact), *approvalVersion)
			}
			return Text{}, false, next, noRow(err, ref)
		}
		text = Text{Kind: "ask", Text: p.askText()}
	case "message":
		var body string
		var inReplyTo *string
		err = r.pool.QueryRow(ctx, `select author, body, in_reply_to::text from messages where id = $1::uuid`, id).
			Scan(&author, &body, &inReplyTo)
		text = Text{Kind: "message", Text: body}
		if reply := deref(inReplyTo); reply != "" {
			next = "message:" + reply
		}
	default:
		return Text{}, false, "", nil
	}
	if err != nil {
		return Text{}, false, "", noRow(err, ref)
	}
	var writer eventActor
	if json.Unmarshal(author, &writer) == nil {
		agent = writer.agent()
	}
	return text, agent, next, nil
}

// readVersion reads a document version ("<document id>:<number>"): its Markdown, and whether an
// agent is one of its authors.
func (r reader) readVersion(ctx context.Context, ref, id string) (Text, bool, string, error) {
	artifact, numberText, _ := strings.Cut(id, ":")
	number, err := strconv.Atoi(numberText)
	if err != nil || uuid.Validate(artifact) != nil {
		return Text{}, false, "", nil
	}
	var markdown *string
	var authors []byte
	err = r.pool.QueryRow(ctx, `select markdown, authors from artifact_versions where artifact_id = $1::uuid and number = $2`,
		artifact, number).Scan(&markdown, &authors)
	if err != nil {
		return Text{}, false, "", noRow(err, ref)
	}
	var writers []eventActor
	agent := false
	if json.Unmarshal(authors, &writers) == nil {
		for _, writer := range writers {
			agent = agent || writer.agent()
		}
	}
	return Text{Kind: "document", Text: deref(markdown)}, agent, "", nil
}

// noRow is err, nil where the read found no row: a text that is gone ends the walk.
func noRow(err error, ref string) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("read %s: %w", ref, err)
}

package sessionanswers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The sessions of the answers fixture. answersReader is the session whose read the tests hold:
// Dispatch delivered to it items of each kind and of each relation the list holds. answersAsker
// wrote asks that the reader does not own, and its own list holds their answers and declines.
const (
	answersReader = "session-reader"
	answersAsker  = "session-asker"
)

// noiseSession is the nth of the 20 sessions that hold the deliveries of the other sessions'
// topics.
func noiseSession(n int) string { return fmt.Sprintf("session-noise-%02d", n) }

// personWordTypes are the event types that carry a person's words to a session: the types the
// read's events arm reads, which the predicate of its index holds.
var personWordTypes = []string{"ask.answered", "comment.created", "message.created", "message.answered", "ask.resolved"}

// The people and the agents that write the fixture's items.
var (
	alice  = model.Actor{Kind: "user", ID: "alice"}
	bob    = model.Actor{Kind: "user", ID: "bob"}
	reader = model.Actor{Kind: "session", ID: answersReader}
	asker  = model.Actor{Kind: "session", ID: answersAsker}
)

// roleTopic is the topic of the role that the issues routed to a role publish to. It names no
// session, so a delivery to it is in no session's list.
const roleTopic = contracts.RoleTopicPrefix + "reviewer"

// agentTopic is a session's own topic, whose deliveries the read lists for the session.
func agentTopic(session string) string { return contracts.LegacyAgentSubject(session) }

// answersFixture is the database the answers read is held to: what people and agents wrote, and
// what Dispatch delivered of it, each row timed back from now, the database's clock when the
// fixture began. Each item and each event is an insert of its own, as the route that writes it
// makes it, and the inserts go to the server in one batch (send).
type answersFixture struct {
	t        *testing.T
	database *store.Store
	now      time.Time
	batch    *pgx.Batch
	// seqs is the last sequence each owner's events took, by the owner's key.
	seqs map[string]int
	// events is each event the fixture wrote, with the topics its published_destinations hold.
	events []fixtureEvent
	// items are the items the edit steps change.
	items answersItems
}

// fixtureEvent is what the fixture wrote of one event: its type, who wrote it, its time and the
// topics the outbox recorded that it published it to.
type fixtureEvent struct {
	eventType string
	actor     model.Actor
	at        time.Time
	topics    []string
}

// eventOwner is the issue or the document an event belongs to, or neither (a direct message),
// with the stem of the topic that the outbox publishes the owner's events under
// (outbox.envelope).
type eventOwner struct {
	issueKey, artifactID *string
	key, topicStem       string
}

func issueOwner(key string) eventOwner {
	return eventOwner{issueKey: &key, key: key, topicStem: contracts.DispatchIssueTopicPrefix + key + "."}
}

// fixtureDocument is a document of project AN, which owns the comments and asks written on it.
type fixtureDocument struct {
	id, slug, name string
}

func (d fixtureDocument) owner() eventOwner {
	return eventOwner{artifactID: &d.id, key: d.id, topicStem: "notifications.dispatch.document.AN." + d.slug + "."}
}

// newAnswersFixture writes the fixture into a new database, then vacuums and analyzes it, so the
// planner costs each read over the fixture's statistics.
//
// Besides the items each session's list holds (writeItems), it writes three sets of events that
// the read must not read (writeNoise): events delivered to the reader in the window that are an
// agent's or of another type (issue.updated); 4,000 person events of the five types in the
// window, delivered to 20 other sessions and never to the reader; and 300 person events of the
// five types, 4 to 6 days old, delivered to the reader.
func newAnswersFixture(t *testing.T) *answersFixture {
	t.Helper()
	f := &answersFixture{t: t, database: storetest.Open(t), batch: &pgx.Batch{}, seqs: map[string]int{}}
	ctx := context.Background()
	if err := f.database.Pool.QueryRow(ctx, `select now()`).Scan(&f.now); err != nil {
		t.Fatalf("read the fixture's now: %v", err)
	}
	f.exec(`insert into projects (key, name, default_assignee) values ('AN', 'Answers', 'alice')`)
	// AN-1 is routed to the reader; AN-2 is routed to a role and claimed by the reader. AN-3
	// holds the other sessions' deliveries, and AN-4 the reader's deliveries before the window.
	f.issue("AN-1", 1, "The queue's database", new("session:"+answersReader), nil)
	f.issue("AN-2", 2, "The queue's pool", new("role:reviewer"), &reader)
	f.issue("AN-3", 3, "The other sessions' work", new("role:reviewer"), nil)
	f.issue("AN-4", 4, "The reader's earlier work", new("session:"+answersReader), nil)
	f.items = f.writeItems()
	f.writeNoise()
	f.send()
	if _, err := f.database.Pool.Exec(ctx, `vacuum (analyze)`); err != nil {
		t.Fatalf("vacuum and analyze the fixture: %v", err)
	}
	return f
}

// answersItems are items the fixture delivered to the reader that the edit steps change.
type answersItems struct {
	// answered is the reader's ask that a person answered, and inAnswersThread a person's reply
	// in its thread.
	answered        *fixtureAsk
	inAnswersThread *writtenComment
	// declined is the asker's ask, which the reader follows, that a person declined.
	declined *fixtureAsk
	// open is the reader's open ask, and inOpenThread a person's reply in its thread.
	open         *fixtureAsk
	inOpenThread *writtenComment
	// onIssue is a person's comment on an issue, and mention a person's comment that names the
	// reader.
	onIssue, mention *writtenComment
	// onIssueMessage is a person's message on the issue routed to the reader, and aimed a
	// person's message on an issue aimed at the reader.
	onIssueMessage, aimed *writtenMessage
}

// writeItems writes the items Dispatch delivered to the reader in the window: of each kind of
// item, and of each relation an item has to the session it reached. Each event's
// published_destinations are the topics the outbox would publish it to (outbox.eventDestinations):
// the owner's topic, the owner's route (AN-1's is the reader, AN-2's a role), the claimant (AN-2's
// is the reader), the authors the thread or the document names, then the ask's followers. A
// session that a mention delivered a comment to is no destination of it, and a message aimed at
// a session goes to its owner's topic alone, since the listener delivers it (message_deliveries).
func (f *answersFixture) writeItems() answersItems {
	minutes := func(n int) time.Time { return f.now.Add(-time.Duration(n) * time.Minute) }
	toReader, toAsker := agentTopic(answersReader), agentTopic(answersAsker)
	issueDocument := f.document(new("AN-1"), "spec", "Spec", answersReader)
	notes := f.document(nil, "queue-notes", "Queue notes", answersReader)
	var items answersItems

	// An answer of an ask the reader wrote (answered_your_ask), and one of an ask the asker wrote
	// on the issue the reader claims (answered_an_ask).
	items.answered = f.ask(issueOwner("AN-1"), reader, "Which database backs the queue?", minutes(120))
	f.answer(items.answered, alice, "Postgres.", minutes(50), toReader)
	f.answer(f.ask(issueOwner("AN-2"), asker, "How large is the pool?", minutes(118)), alice, "Eight connections.", minutes(48),
		roleTopic, toReader, toAsker)
	// A decline of an ask the reader follows (declined_your_ask), and one of an ask on the document
	// the reader wrote, which the reader does not own (declined_an_ask).
	items.declined = f.ask(issueOwner("AN-1"), asker, "May the queue drop old jobs?", minutes(116))
	f.follow(items.declined, answersReader)
	f.decline(items.declined, alice, "Ask the owner of the jobs.", minutes(46), toReader, toAsker)
	f.decline(f.ask(notes.owner(), asker, "Should the notes list each queue?", minutes(114)), alice, "", minutes(44), toReader, toAsker)
	// A person's replies in the threads of two of the reader's asks, answered and open
	// (replied_on_your_ask).
	items.inAnswersThread = f.comment(fixtureComment{issue: "AN-1", author: alice, body: "Postgres 18, to be exact.", ask: items.answered, at: minutes(42)})
	f.commentCreated(items.inAnswersThread, toReader)
	items.open = f.ask(issueOwner("AN-1"), reader, "Which jobs may wait?", minutes(41))
	items.inOpenThread = f.comment(fixtureComment{issue: "AN-1", author: alice, body: "Which queue do you mean?", ask: items.open, at: minutes(40)})
	f.commentCreated(items.inOpenThread, toReader)

	// A person's comment on the issue routed to the reader (commented_on_an_issue).
	items.onIssue = f.comment(fixtureComment{issue: "AN-1", author: alice, body: "The queue needs an index.", at: minutes(38)})
	f.commentCreated(items.onIssue, toReader)
	// A person's reply to a thread the reader began, on the issue it claims
	// (replied_in_a_comment_thread). The reader's own comment is an agent's and reaches no route.
	root := f.comment(fixtureComment{issue: "AN-2", author: reader, body: "I sized the pool at 4.", at: minutes(37)})
	f.commentCreated(root)
	f.commentCreated(f.comment(fixtureComment{issue: "AN-2", author: alice, body: "Make it 8.", replyTo: root, at: minutes(36)}),
		roleTopic, toReader)
	// A person's comment on the reader's document, and one on the issue anchored on the issue's
	// document (commented_on_a_document).
	f.commentCreated(f.comment(fixtureComment{document: &notes, author: alice, body: "Add the retry queue.", at: minutes(35)}), toReader)
	f.commentCreated(f.comment(fixtureComment{issue: "AN-1", author: alice, body: "This paragraph is out of date.", at: minutes(34),
		anchor: &model.Anchor{ArtifactID: issueDocument.id, MarkID: "mark-1", Version: 1, Quote: "The queue"}}), toReader)
	// A person's comment that names the reader, on the issue the reader claims (mentioned_you):
	// the mention delivered it, so its event goes to the role alone.
	items.mention = f.comment(fixtureComment{issue: "AN-2", author: alice, body: "@session-reader, look at the pool.", at: minutes(32)})
	f.commentCreated(items.mention, roleTopic)
	f.commentDelivery(items.mention, answersReader, "sent", minutes(32))

	// A person's messages (sent_you_a_message): one on the issue routed to the reader, one on an
	// issue aimed at the reader, which the listener sent it, and a direct message, which the
	// listener stored for the reader while it did not run. A direct message's event has no owner,
	// and so no topic.
	items.onIssueMessage = f.message(fixtureMessage{issue: "AN-1", author: alice, body: "Ship it today.", at: minutes(30)})
	f.messageCreated(items.onIssueMessage, toReader)
	items.aimed = f.message(fixtureMessage{issue: "AN-2", author: alice, body: "Please resize the pool.", target: new("session:" + answersReader), at: minutes(28)})
	f.messageCreated(items.aimed)
	f.messageDelivery(items.aimed, answersReader, "steer", "sent", minutes(28))
	direct := f.message(fixtureMessage{author: alice, body: "Are you free?", target: new("session:" + answersReader), at: minutes(26)})
	f.messageCreated(direct)
	f.messageDelivery(direct, answersReader, "btw", "queued", minutes(26))
	// A person's reply to a message the reader wrote (replied_to_your_message).
	asked := f.message(fixtureMessage{issue: "AN-1", author: reader, body: "Shall I ship it?", at: minutes(25)})
	f.messageCreated(asked)
	f.messageCreated(f.message(fixtureMessage{issue: "AN-1", author: alice, body: "Yes.", inReplyTo: asked, at: minutes(24)}), toReader)
	return items
}

// writeNoise writes the events the read must not read, with the items they name.
func (f *answersFixture) writeNoise() {
	// Events delivered to the reader in the window that the read leaves out: an agent's replies
	// in the thread of an ask the reader follows, which a follower route delivers whoever wrote
	// them, and a person's issue.updated events, which carry no words to a session.
	followed := f.ask(issueOwner("AN-1"), asker, "Should the queue retry?", f.now.Add(-61*time.Hour))
	f.follow(followed, answersReader)
	for i := range 40 {
		at := f.now.Add(-time.Duration(i+1) * 90 * time.Minute)
		f.commentCreated(f.comment(fixtureComment{issue: "AN-1", author: asker, body: fmt.Sprintf("Still looking (%d).", i), ask: followed, at: at}),
			agentTopic(answersReader))
		f.event(issueOwner("AN-1"), "issue.updated", alice, map[string]any{"key": "AN-1", "changed": []string{"status"}},
			at.Add(time.Minute), agentTopic(answersReader))
	}

	// 800 person events of each of the five types in the window, on AN-3, each delivered to one
	// of the 20 other sessions: 4,000 events.
	for i := range 800 {
		session := model.Actor{Kind: "session", ID: noiseSession(i % 20)}
		at := f.now.Add(-time.Duration(i+1) * 5 * time.Minute)
		topics := []string{roleTopic, agentTopic(session.ID)}
		f.answer(f.ask(issueOwner("AN-3"), session, fmt.Sprintf("Which shard (%d)?", i), at.Add(-time.Hour)), bob, fmt.Sprintf("Shard %d.", i), at, topics...)
		f.decline(f.ask(issueOwner("AN-3"), session, fmt.Sprintf("May I drop shard %d?", i), at.Add(-time.Hour)), bob, "", at, topics...)
		f.commentCreated(f.comment(fixtureComment{issue: "AN-3", author: bob, body: fmt.Sprintf("Shard %d is full.", i), at: at}), topics...)
		f.messageCreated(f.message(fixtureMessage{issue: "AN-3", author: bob, body: fmt.Sprintf("Look at shard %d.", i), at: at}), topics...)
		moved := f.message(fixtureMessage{issue: "AN-3", author: session, body: fmt.Sprintf("Shard %d moved.", i), at: at.Add(-time.Hour)})
		f.messageCreated(f.message(fixtureMessage{issue: "AN-3", author: bob, body: "Thanks.", inReplyTo: moved, at: at}), topics...)
	}

	// 60 person events of each of the five types delivered to the reader 4 to 6 days ago, on
	// AN-4: 300 events. A window of 7 days holds them, so the oracle reads them there.
	topics := []string{agentTopic(answersReader)}
	moved := f.message(fixtureMessage{issue: "AN-4", author: reader, body: "The old shard moved.", at: f.now.Add(-150 * time.Hour)})
	for i := range 60 {
		at := f.now.Add(-96*time.Hour - time.Duration(i)*48*time.Hour/60)
		f.answer(f.ask(issueOwner("AN-4"), asker, fmt.Sprintf("Which old shard (%d)?", i), at.Add(-time.Hour)), alice, fmt.Sprintf("Old shard %d.", i), at, topics...)
		f.decline(f.ask(issueOwner("AN-4"), asker, fmt.Sprintf("May I drop old shard %d?", i), at.Add(-time.Hour)), alice, "Not now.", at, topics...)
		f.commentCreated(f.comment(fixtureComment{issue: "AN-4", author: alice, body: fmt.Sprintf("Old shard %d is full.", i), at: at}), topics...)
		f.messageCreated(f.message(fixtureMessage{issue: "AN-4", author: alice, body: fmt.Sprintf("Look at old shard %d.", i), at: at}), topics...)
		f.messageCreated(f.message(fixtureMessage{issue: "AN-4", author: alice, body: fmt.Sprintf("Thanks (%d).", i), inReplyTo: moved, at: at}), topics...)
	}
}

// delivered counts the events the fixture delivered to session since since that the read must
// read: a person's, of the five types, whose published_destinations hold the session's topic.
func (f *answersFixture) delivered(session string, since time.Time) int {
	count := 0
	for _, event := range f.events {
		if event.actor.Kind == "user" && slices.Contains(personWordTypes, event.eventType) &&
			!event.at.Before(since) && slices.Contains(event.topics, agentTopic(session)) {
			count++
		}
	}
	return count
}

// exec queues one statement on the fixture's batch.
func (f *answersFixture) exec(statement string, arguments ...any) {
	f.batch.Queue(statement, arguments...)
}

// send sends the fixture's batch, every statement in the one transaction that a pipeline runs
// in, and fails the test at the first statement that fails.
func (f *answersFixture) send() {
	f.t.Helper()
	queued := f.batch.QueuedQueries
	results := f.database.Pool.SendBatch(context.Background(), f.batch)
	for _, statement := range queued {
		if _, err := results.Exec(); err != nil {
			results.Close()
			f.t.Fatalf("write the fixture: %s: %v", statement.SQL, err)
		}
	}
	if err := results.Close(); err != nil {
		f.t.Fatalf("write the fixture: %v", err)
	}
	f.batch = &pgx.Batch{}
}

// encode is value as the JSON that a jsonb column stores.
func (f *answersFixture) encode(value any) []byte {
	f.t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		f.t.Fatalf("encode %T: %v", value, err)
	}
	return encoded
}

// issue writes an issue of project AN, routed to route, and claimed by claimant where it is set.
func (f *answersFixture) issue(key string, number int, title string, route *string, claimant *model.Actor) {
	var claim []byte
	var claimedAt *time.Time
	if claimant != nil {
		claim = f.encode(claimant)
		claimedAt = new(f.now.Add(-70 * time.Hour))
	}
	f.exec(`
		insert into issues (key, project_key, number, title, route, created_by, rank, claimed_by, claimed_at)
		values ($1, 'AN', $2, $3, $4, $5, $6, $7, $8)
	`, key, number, title, route, f.encode(alice), fmt.Sprintf("U%d", number), claim, claimedAt)
}

// document writes a document of project AN that session created: the primary document of the
// issue issueKey names, or a document of the project where issueKey is nil.
func (f *answersFixture) document(issueKey *string, slug, name, session string) fixtureDocument {
	document := fixtureDocument{id: uuid.NewString(), slug: slug, name: name}
	f.exec(`
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, $2, 'AN', $3, $4, 'doc', $2::text is not null, $5)
	`, document.id, issueKey, slug, name, f.encode(model.Actor{Kind: "session", ID: session}))
	return document
}

// event writes an event of owner as the broker appends it, with the next sequence of its owner
// (0 for an event with no owner), at time at, and as the outbox leaves it once it published it:
// published_at at its time, and published_destinations the owner's topic and then topics, in the
// order eventDestinations (outbox/publisher.go) sends them. notify is set for a person's event,
// since an agent's own write wakes no route. An event with no owner has no topic of its own, so
// its published_destinations hold topics alone.
func (f *answersFixture) event(owner eventOwner, eventType string, actor model.Actor, payload any, at time.Time, topics ...string) {
	seq := 0
	destinations := []string{}
	var publishedAt *time.Time
	if owner.key != "" {
		f.seqs[owner.key]++
		seq = f.seqs[owner.key]
		destinations = append(destinations, owner.topicStem+eventType)
		publishedAt = &at
	}
	destinations = append(destinations, topics...)
	f.exec(`
		insert into events (issue_key, artifact_id, seq, type, actor, payload, notify, created_at, published_at, published_destinations)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, owner.issueKey, owner.artifactID, seq, eventType, f.encode(actor), f.encode(payload), actor.Kind == "user", at, publishedAt, destinations)
	f.events = append(f.events, fixtureEvent{eventType: eventType, actor: actor, at: at, topics: destinations})
}

// fixtureAsk is an ask the fixture wrote, with the owner of its events.
type fixtureAsk struct {
	model.Ask
	owner eventOwner
}

// ask writes an open ask of owner, and its author as its follower where the author is a session,
// as every route that opens an ask does.
func (f *answersFixture) ask(owner eventOwner, author model.Actor, question string, at time.Time) *fixtureAsk {
	ask := &fixtureAsk{owner: owner, Ask: model.Ask{
		ID: uuid.NewString(), IssueKey: owner.issueKey, ArtifactID: owner.artifactID, Author: author,
		Kind: "question", Question: question, Options: []model.AskOption{}, Urgency: "med", State: "open", CreatedAt: at,
	}}
	f.exec(`
		insert into asks (id, issue_key, artifact_id, author, question, created_at) values ($1, $2, $3, $4, $5, $6)
	`, ask.ID, ask.IssueKey, ask.ArtifactID, f.encode(author), question, at)
	if author.Kind == "session" {
		f.follow(ask, author.ID)
	}
	return ask
}

// follow makes session a follower of ask.
func (f *answersFixture) follow(ask *fixtureAsk, session string) {
	f.exec(`insert into ask_followers (ask_id, session_id, since) values ($1, $2, $3)`, ask.ID, session, ask.CreatedAt)
}

// answer writes person's answer to ask at time at, and its ask.answered event, delivered to
// topics. Its payload is the one the answer route writes (answerTransition): the ask as the answer
// leaves it, and the answer it replaced where it replaced one.
func (f *answersFixture) answer(ask *fixtureAsk, person model.Actor, text string, at time.Time, topics ...string) {
	previous := ask.Answer
	ask.State = "answered"
	ask.Answer = &model.AskAnswer{User: person.ID, Selected: []string{}, Text: &text, At: at}
	f.exec(`update asks set state = 'answered', answer = $2 where id = $1`, ask.ID, f.encode(ask.Answer))
	f.event(ask.owner, "ask.answered", person, model.AskAnsweredEventPayload{
		AskEventPayload: model.NewAskEventPayload(ask.Ask, model.ReferenceChanges{}),
		PreviousAnswer:  previous,
	}, at, topics...)
}

// decline writes person's decline of ask at time at, with note, and its ask.resolved event,
// delivered to topics. The resolution is the one the decline route writes
// (docs.WriteAskResolution), which the event carries in the ask.
func (f *answersFixture) decline(ask *fixtureAsk, person model.Actor, note string, at time.Time, topics ...string) {
	ask.State = "resolved"
	ask.Resolution = &model.AskResolution{
		Kind: model.AskResolutionDeclined, Reason: note, DispatchWords: model.DeclineDispatchWords, Actor: person, At: at,
	}
	f.exec(`update asks set state = 'resolved', resolution = $2 where id = $1`, ask.ID, f.encode(ask.Resolution))
	f.event(ask.owner, "ask.resolved", person, model.NewAskEventPayload(ask.Ask, model.ReferenceChanges{}), at, topics...)
}

// fixtureComment is a comment the fixture writes: on the issue it names, or on document; in the
// thread of replyTo or of ask; anchored where anchor is set.
type fixtureComment struct {
	issue    string
	document *fixtureDocument
	author   model.Actor
	body     string
	replyTo  *writtenComment
	ask      *fixtureAsk
	anchor   *model.Anchor
	at       time.Time
}

// writtenComment is a comment the fixture wrote, with the owner of its events, and the ask and
// the document its event's payload names.
type writtenComment struct {
	model.Comment
	owner    eventOwner
	ask      *fixtureAsk
	document *fixtureDocument
}

// comment writes the comment c describes.
func (f *answersFixture) comment(c fixtureComment) *writtenComment {
	written := &writtenComment{ask: c.ask, document: c.document, Comment: model.Comment{
		ID: uuid.NewString(), Author: c.author, Body: c.body, Anchor: c.anchor, CreatedAt: c.at,
		Mentions: []model.Mention{}, Deliveries: []model.CommentDelivery{},
	}}
	if c.document != nil {
		written.owner = c.document.owner()
		written.ArtifactID = &c.document.id
	} else {
		written.owner = issueOwner(c.issue)
		written.IssueKey = &c.issue
	}
	if c.replyTo != nil {
		written.ReplyTo = &c.replyTo.ID
		written.Answers = &c.replyTo.ID
	}
	if c.ask != nil {
		written.AskID = &c.ask.ID
	}
	var anchor []byte
	if c.anchor != nil {
		anchor = f.encode(c.anchor)
	}
	f.exec(`
		insert into comments (id, issue_key, artifact_id, author, body, reply_to, answers, ask_id, anchor, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, written.ID, written.IssueKey, written.ArtifactID, f.encode(c.author), c.body, written.ReplyTo, written.Answers,
		written.AskID, anchor, c.at)
	return written
}

// commentCreated writes c's comment.created event, delivered to topics, with the payload the
// comment route writes (model.CommentEventPayload): the comment, the document it is on, and the
// question and state of the ask whose thread it is in.
func (f *answersFixture) commentCreated(c *writtenComment, topics ...string) {
	f.event(c.owner, "comment.created", c.Author, f.commentPayload(c), c.CreatedAt, topics...)
}

// commentPayload is the payload of c's events.
func (f *answersFixture) commentPayload(c *writtenComment) model.CommentEventPayload {
	payload := model.CommentEventPayload{Comment: c.Comment}
	if c.document != nil {
		payload.ArtifactName, payload.ProjectKey, payload.ArtifactSlug = c.document.name, "AN", c.document.slug
	}
	if c.ask != nil {
		payload.AskQuestion, payload.AskState = c.ask.Question, c.ask.State
	}
	return payload
}

// fixtureMessage is a message the fixture writes: on the issue it names, or on none (a direct
// message); aimed at target where it is set; a reply to inReplyTo where that is set.
type fixtureMessage struct {
	issue     string
	author    model.Actor
	body      string
	target    *string
	inReplyTo *writtenMessage
	at        time.Time
}

// writtenMessage is a message the fixture wrote, with the owner of its event and the body of
// the message it replies to.
type writtenMessage struct {
	model.Message
	owner     eventOwner
	replyBody string
}

// message writes the message m describes.
func (f *answersFixture) message(m fixtureMessage) *writtenMessage {
	written := &writtenMessage{Message: model.Message{
		ID: uuid.NewString(), Author: m.author, Body: m.body, Target: m.target, CreatedAt: m.at,
		Deliveries: []model.MessageDelivery{},
	}}
	if m.issue != "" {
		written.owner = issueOwner(m.issue)
		written.IssueKey = &m.issue
	}
	if m.inReplyTo != nil {
		written.InReplyTo = &m.inReplyTo.ID
		written.replyBody = m.inReplyTo.Body
	}
	f.exec(`
		insert into messages (id, issue_key, author, body, target, in_reply_to, created_at) values ($1, $2, $3, $4, $5, $6, $7)
	`, written.ID, written.IssueKey, f.encode(m.author), m.body, m.target, written.InReplyTo, m.at)
	return written
}

// messageCreated writes m's event, delivered to topics: message.answered for a reply and
// message.created otherwise, as the message route chooses, with the payload it writes
// (model.MessageEventPayload).
func (f *answersFixture) messageCreated(m *writtenMessage, topics ...string) {
	eventType := "message.created"
	if m.InReplyTo != nil {
		eventType = "message.answered"
	}
	f.event(m.owner, eventType, m.Author, model.MessageEventPayload{Message: m.Message, ReplyBody: m.replyBody}, m.CreatedAt, topics...)
}

// commentDelivery writes the listener's delivery of c to session, in state, as a mention's send
// records it (comment_deliveries).
func (f *answersFixture) commentDelivery(c *writtenComment, session, state string, at time.Time) {
	f.exec(`
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state, created_at)
		values ($1, $2, 1, 'steer', $3, $4, $5)
	`, c.ID, "session:"+session, session, state, at)
}

// messageDelivery writes the listener's delivery of m to session, in state, as the send of a
// message aimed at a session records it (message_deliveries).
func (f *answersFixture) messageDelivery(m *writtenMessage, session, delivery, state string, at time.Time) {
	f.exec(`
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, created_at)
		values ($1, 1, $2, $3, $4, $5)
	`, m.ID, delivery, session, state, at)
}

// editComment writes its author's edit of c to body at time at, as the comment edit route writes
// it: the row's body and edited_at, and a comment.edited event, delivered to topics.
func (f *answersFixture) editComment(c *writtenComment, body string, at time.Time, topics ...string) {
	c.Body = body
	c.EditedAt = new(at.Format(time.RFC3339Nano))
	f.exec(`update comments set body = $2, edited_at = $3 where id = $1`, c.ID, body, at)
	f.event(c.owner, "comment.edited", c.Author, f.commentPayload(c), at, topics...)
}

// deleteComment writes its author's delete of c at time at, as the comment delete route writes
// it: the row's body cleared, its deleted_at and deleted_by, and a comment.deleted event,
// delivered to topics.
func (f *answersFixture) deleteComment(c *writtenComment, at time.Time, topics ...string) {
	c.Body, c.DeletedAt, c.DeletedBy = "", &at, &c.Author
	f.exec(`update comments set body = '', deleted_at = $2, deleted_by = $3 where id = $1`, c.ID, at, f.encode(c.Author))
	f.event(c.owner, "comment.deleted", c.Author, f.commentPayload(c), at, topics...)
}

// editMessage writes its author's edit of m to body at time at, as the message edit route writes
// it: the row's body and edited_at, and a message.edited event, delivered to topics.
func (f *answersFixture) editMessage(m *writtenMessage, body string, at time.Time, topics ...string) {
	m.Body, m.EditedAt = body, &at
	f.exec(`update messages set body = $2, edited_at = $3 where id = $1`, m.ID, body, at)
	f.event(m.owner, "message.edited", m.Author, model.MessageEventPayload{Message: m.Message, ReplyBody: m.replyBody}, at, topics...)
}

// deleteMessage writes its author's delete of m at time at, as the message delete route writes
// it: the row's body cleared, its deleted_at and deleted_by, and a message.deleted event,
// delivered to topics.
func (f *answersFixture) deleteMessage(m *writtenMessage, at time.Time, topics ...string) {
	m.Body, m.DeletedAt, m.DeletedBy = "", &at, &m.Author
	f.exec(`update messages set body = '', deleted_at = $2, deleted_by = $3 where id = $1`, m.ID, at, f.encode(m.Author))
	f.event(m.owner, "message.deleted", m.Author, model.MessageEventPayload{Message: m.Message}, at, topics...)
}

// rewordAsk writes its author's new wording of ask, question, at time at: the row's question and
// edited_at, and an ask.edited event, delivered to topics.
func (f *answersFixture) rewordAsk(ask *fixtureAsk, question string, at time.Time, topics ...string) {
	ask.Question = question
	ask.EditedAt = new(at.Format(time.RFC3339Nano))
	f.exec(`update asks set question = $2, edited_at = $3 where id = $1`, ask.ID, question, at)
	f.event(ask.owner, "ask.edited", ask.Author, model.NewAskEventPayload(ask.Ask, model.ReferenceChanges{}), at, topics...)
}

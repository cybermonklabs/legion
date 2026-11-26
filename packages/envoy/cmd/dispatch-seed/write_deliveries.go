package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/answerswindow"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// deliveryTypes are the five event types the agents' answers read lists (sessionanswers.query,
// reached_by_destination), the same five as answerswindow.DeliveredPersonEvent names, in the
// order the deliveries of one span take turns (deliveryPools.draw): an answer, a new message, a
// reply to a message, a comment and a decline (an ask.resolved whose resolution is of kind
// declined).
var deliveryTypes = []string{"ask.answered", "message.created", "message.answered", "comment.created", "ask.resolved"}

// documentTopicPrefix begins the owner topic of every event a document owns, as the outbox
// builds it (outbox's envelope): notifications.dispatch.document.<project>.<slug>.<type>.
const documentTopicPrefix = "notifications.dispatch.document."

// messageReplyPreviewRunes is how much of the message a reply answers its event carries as
// reply_body (api's maxMessageReplyPreview16, which messageReplyPreview cuts to).
const messageReplyPreviewRunes = 160

// oldDeliverySpan is how far back the old session's deliveries before the window reach.
const oldDeliverySpan = 60 * 24 * time.Hour

// eventOwner is the row an event is appended on: an issue, or a document no issue owns the item
// on. Each is an index into the plan, and exactly one of the two is at least zero.
type eventOwner struct {
	issue    int
	document int
}

// delivery is one event of a person's words the seed delivers to sessions: its type, the item
// it carries (an index into the plan's comments, messages or asks, by its type), the item's own
// time, which is the event's, the owner it is appended on, the sessions it reached, in the order
// their topics follow the owner's, and the words an answer or a decline writes. id and seq are
// set once every delivery is planned.
type delivery struct {
	eventType string
	item      int
	at        time.Time
	owner     eventOwner
	sessions  []string
	words     string
	id        int64
	seq       int
}

// deliverySpan is the times the items of one set of deliveries are chosen from, both ends held.
type deliverySpan struct {
	from, to time.Time
}

func (s deliverySpan) holds(at time.Time) bool {
	return !at.Before(s.from) && !at.After(s.to)
}

// draw is a time in the span to the second, which is when an answer or a decline the seed times
// itself is given.
func (s deliverySpan) draw(random *rand.Rand) time.Time {
	return s.from.Add(time.Duration(random.Int64N(int64(s.to.Sub(s.from)/time.Second)+1)) * time.Second)
}

// deliveryPools are the items one span's deliveries are drawn from, by event type, each in a
// shuffled order and drawn from its end. The two ask types share one pool, which the two spans
// share too: an ask is answered or declined once.
type deliveryPools map[string]*[]int

// draw takes the next item of turn's type, or, when that type has no item left, of the next
// type in deliveryTypes' order that has one. It answers false when no type has any.
func (p deliveryPools) draw(turn int) (string, int, bool) {
	for step := range deliveryTypes {
		eventType := deliveryTypes[(turn+step)%len(deliveryTypes)]
		pool := p[eventType]
		if held := len(*pool); held > 0 {
			item := (*pool)[held-1]
			*pool = (*pool)[:held-1]
			return eventType, item, true
		}
	}
	return "", 0, false
}

// deliverableComment is whether comment index is one the seed delivers. Its comment.created
// event records the comment as its write left it, so the comment is one the plan writes as it
// was created and one whose event this writer can build from the row: not edited, whose row
// holds the body after the edit; naming no block it replies to, whose event carries that block
// (api's hydrateCommentReplyBlocks); and not in the thread of an ask the plan leaves open, whose
// reply records a turn (api's commentThreadTarget.replyTurn) the seeded row does not hold. A
// comment the plan resolves was not resolved when it was written, so its event records it
// unresolved (commentCreatedPayload).
func (p *seedPlan) deliverableComment(index int) bool {
	comment := p.comments[index]
	if comment.Edited || comment.RepliesTo >= 0 || comment.AskPart {
		return false
	}
	return comment.Ask < 0 || p.asks[comment.Ask].State != "open"
}

// planDeliveries is every delivery the seed writes, in the order of their times, each with its
// id and its seq unset. All of it is drawn from a random source of its own, so the deliveries
// move no word the run's other steps draw.
//
// The times: the deliveries in the window are the items of the last 72 hours of the seed's span
// (seedSpan from baseTime), so the newest delivery is no earlier than the first instant of those
// 72 hours, and the rule's start (answerswindow.StartQuery) is no earlier than 72 hours before that
// instant. The old session's deliveries before the window are the items of the 60 days that end
// a day before that earliest start. So the rule's start sits between the two sets whatever the
// newest delivery's exact time. A comment or a message is delivered at its own time, and an
// answer or a decline at a time drawn in the span, after its ask was written: the asks the
// deliveries take were written before either span.
//
// The sessions: one session is the old one, and WindowSessions others take the window. Each
// delivery reaches one to three sessions. Each of the old session's deliveries before the window
// reaches it and up to two of the others, and so do the first OldSessionWindowDeliveries of the
// window, which the five types take in turn, so the old session's own read lists every kind of
// item. Every other delivery in the window reaches one to three of the others. A scale with no
// session beside the old one delivers to the old one alone.
//
// The items: the types take turns (deliveryTypes), and a type whose items in the span are used
// up gives its turn to the next. A comment, a root message (message.created) and a reply to one
// (message.answered) are taken from the span's own (deliverableComment, and a message naming no
// block it replies to, whose event would carry that block). An answer and a decline each take an
// ask the plan leaves open that is not a block ask, which writes its state into its document.
func (r *seedRun) planDeliveries() []delivery {
	plan := r.plan
	random := rand.New(rand.NewPCG(randomSeed+4, randomSeed^0x94d049bb133111eb))
	end := baseTime.Add(seedSpan)
	window := deliverySpan{from: end.Add(-answerswindow.Length), to: end}
	before := deliverySpan{to: window.from.Add(-answerswindow.Length - 24*time.Hour)}
	before.from = before.to.Add(-oldDeliverySpan)

	order := random.Perm(len(plan.sessions))
	oldSession := plan.sessions[order[0]]
	others := make([]string, 0, min(r.sizes.WindowSessions, len(order)-1))
	for _, index := range order[1:min(len(order), 1+r.sizes.WindowSessions)] {
		others = append(others, plan.sessions[index])
	}
	reach := func(withOld bool) []string {
		count := 1 + random.IntN(3)
		reached := make([]string, 0, count)
		if withOld || len(others) == 0 {
			reached = append(reached, oldSession)
		}
		for want := min(count, len(reached)+len(others)); len(reached) < want; {
			if session := others[random.IntN(len(others))]; !slices.Contains(reached, session) {
				reached = append(reached, session)
			}
		}
		return reached
	}

	var askPool []int
	for index, ask := range plan.asks {
		if ask.State == "open" && ask.BlockDoc < 0 && ask.CreatedAt.Before(before.from) {
			askPool = append(askPool, index)
		}
	}
	shuffle(random, askPool)

	deliveries := make([]delivery, 0, r.sizes.OldDeliveries+r.sizes.WindowDeliveries)
	spans := []struct {
		span    deliverySpan
		count   int
		withOld func(turn int) bool
	}{
		{before, r.sizes.OldDeliveries, func(int) bool { return true }},
		{window, r.sizes.WindowDeliveries, func(turn int) bool { return turn < r.sizes.OldSessionWindowDeliveries }},
	}
	for _, set := range spans {
		pools := r.deliveryPools(random, set.span, &askPool)
		for turn := range set.count {
			eventType, item, ok := pools.draw(turn)
			if !ok {
				break
			}
			planned := delivery{eventType: eventType, item: item}
			switch eventType {
			case "comment.created":
				comment := plan.comments[item]
				planned.at, planned.owner = comment.CreatedAt, eventOwner{comment.Issue, comment.Document}
			case "message.created", "message.answered":
				message := plan.messages[item]
				planned.at, planned.owner = message.CreatedAt, eventOwner{message.Issue, -1}
			default:
				ask := plan.asks[item]
				planned.at, planned.owner = set.span.draw(random), eventOwner{ask.Issue, ask.Document}
				planned.words = plan.vocabulary.phrase(random, 6)
			}
			planned.sessions = reach(set.withOld(turn))
			deliveries = append(deliveries, planned)
		}
	}
	slices.SortStableFunc(deliveries, func(a, b delivery) int { return a.at.Compare(b.at) })
	return deliveries
}

// deliveryPools is the items of span each type's deliveries are drawn from, askPool being the
// pool both ask types share.
func (r *seedRun) deliveryPools(random *rand.Rand, span deliverySpan, askPool *[]int) deliveryPools {
	var comments, roots, replies []int
	for index, comment := range r.plan.comments {
		if span.holds(comment.CreatedAt) && r.plan.deliverableComment(index) {
			comments = append(comments, index)
		}
	}
	for index, message := range r.plan.messages {
		if !span.holds(message.CreatedAt) || message.RepliesTo >= 0 {
			continue
		}
		if message.InReplyTo < 0 {
			roots = append(roots, index)
		} else {
			replies = append(replies, index)
		}
	}
	for _, pool := range [][]int{comments, roots, replies} {
		shuffle(random, pool)
	}
	return deliveryPools{
		"ask.answered": askPool, "ask.resolved": askPool,
		"comment.created": &comments, "message.created": &roots, "message.answered": &replies,
	}
}

func shuffle(random *rand.Rand, values []int) {
	random.Shuffle(len(values), func(a, b int) { values[a], values[b] = values[b], values[a] })
}

// writeDeliveries writes the events that deliver a person's words to agent sessions, which the
// agents' answers read (sessionanswers.List) lists: the five types of deliveryTypes, by the
// seed's person, each on the item it carries (planDeliveries). It writes events only, and the ask
// rows an answer and a decline leave: no comment_deliveries or message_deliveries row, which the
// listener's own sends write, and no delivered_to tuple, which the outbox's record of a send
// appends and no measured read takes.
//
// Each event is the one Dispatch's own writer appends for its item (deliveryPayload), owned by the
// item's owner and dated at the item's own time. Its seq follows the highest its owner carries,
// and its id follows every id written before it, the asks' opening events included, so an ask's
// ask.opened keeps the lowest id of its events (events.OpenedEventIDs); the sequence is then set
// past the last of them, as writeEvents sets it past the opening events. Its published_destinations
// is what the outbox records for it (outbox's eventDestinations): the owner's topic first, then
// each session's own topic (contracts.LegacyAgentSubject). It notifies, as a person's event does,
// and is published (published_at set), so a server started on the seeded database publishes
// nothing of it.
//
// Each batch takes the owner lock of every ask it answers or declines before any other statement,
// in one store.LockOwners call, as writeAsks takes its asks' owners (seedRun.askOwner). An answer
// indexes the ask's parts again, and the window write that ends that index takes the ask's owner
// lock (indexAskParts, refs.WriteItemWindows); the store's lock order has the owner lock first, so
// a batch that left it to the window write would take it while it already holds the asks row and
// the index rows the answer wrote. A decline takes it too, as the decline route takes it before it
// reads the ask (api's transitionAskTx). A batch that answers and declines no ask takes no lock.
func (r *seedRun) writeDeliveries(ctx context.Context) error {
	deliveries := r.planDeliveries()
	if len(deliveries) == 0 {
		return nil
	}
	lastSeqs, err := r.ownerLastSeqs(ctx, deliveries)
	if err != nil {
		return err
	}
	next := r.askEventIDEnd()
	for index := range deliveries {
		next++
		deliveries[index].id = next
		lastSeqs[deliveries[index].owner]++
		deliveries[index].seq = lastSeqs[deliveries[index].owner]
	}
	hasAnswers, err := r.commentsCarryAnswers(ctx)
	if err != nil {
		return err
	}
	columns := append(slices.Clone(eventColumns), "published_destinations")
	if err := r.inBatches(ctx, "deliveries", len(deliveries), batchItems,
		func(ctx context.Context, tx pgx.Tx, from, to int) error {
			// The owner lock of every ask the batch answers or declines comes first, in owner key
			// order, as the store's lock order has it: an answer's window write takes its ask's
			// owner lock (indexAskParts, refs.WriteItemWindows), which a batch holding the asks
			// row and the ask's index rows would otherwise take after them.
			var owners []store.Owner
			for _, planned := range deliveries[from:to] {
				switch planned.eventType {
				case "ask.answered", "ask.resolved":
					if owner := r.askOwner(r.plan.asks[planned.item]); owner != nil {
						owners = append(owners, *owner)
					}
				}
			}
			if err := store.LockOwners(ctx, tx, owners...); err != nil {
				return fmt.Errorf("the answered and declined asks' owners: %w", err)
			}
			rows := make([][]any, 0, to-from)
			for _, planned := range deliveries[from:to] {
				payload, err := r.deliveryPayload(ctx, tx, planned, hasAnswers)
				if err != nil {
					return err
				}
				encoded, err := json.Marshal(payload)
				if err != nil {
					return fmt.Errorf("encode the %s event of delivery %d: %w", planned.eventType, planned.id, err)
				}
				destinations := make([]string, 0, 1+len(planned.sessions))
				destinations = append(destinations, r.ownerTopic(planned.owner, planned.eventType))
				for _, session := range planned.sessions {
					destinations = append(destinations, contracts.LegacyAgentSubject(session))
				}
				issueKey, artifactID := r.ownerColumns(planned.owner)
				rows = append(rows, []any{
					planned.id, issueKey, artifactID, nil, planned.seq, planned.eventType, seedActor,
					string(encoded), true, planned.at, planned.at, destinations,
				})
			}
			_, err := tx.CopyFrom(ctx, pgx.Identifier{"events"}, columns, pgx.CopyFromRows(rows))
			return err
		}); err != nil {
		return err
	}
	if _, err := r.database.Pool.Exec(ctx, `select setval('events_id_seq', $1, true)`, next); err != nil {
		return fmt.Errorf("set the events sequence past the deliveries: %w", err)
	}
	return nil
}

// ownerColumns is the issue_key and the artifact_id an event of owner carries, one of them null.
func (r *seedRun) ownerColumns(owner eventOwner) (issueKey, artifactID any) {
	if owner.document >= 0 {
		return nil, r.plan.documents[owner.document].ID
	}
	return r.plan.issues[owner.issue].Key, nil
}

// ownerTopic is the topic the outbox publishes an event of owner under, its first destination
// (outbox's envelope): the issue's, or the document's under its project and slug.
func (r *seedRun) ownerTopic(owner eventOwner, eventType string) string {
	if owner.document >= 0 {
		document := r.plan.documents[owner.document]
		return documentTopicPrefix + r.plan.projects[document.Project].Key + "." + document.Slug + "." + eventType
	}
	return contracts.LegacyDispatchIssueSubject(r.plan.issues[owner.issue].Key, eventType)
}

// ownerLastSeqs is the highest seq each owner the deliveries are appended on carries, its filler
// events and its asks' opening events counted, which the deliveries' seqs continue from.
func (r *seedRun) ownerLastSeqs(ctx context.Context, deliveries []delivery) (map[eventOwner]int, error) {
	byIssue, byArtifact := map[string]eventOwner{}, map[string]eventOwner{}
	for _, planned := range deliveries {
		if planned.owner.document >= 0 {
			byArtifact[r.plan.documents[planned.owner.document].ID] = planned.owner
		} else {
			byIssue[r.plan.issues[planned.owner.issue].Key] = planned.owner
		}
	}
	issueKeys, artifactIDs := slices.Collect(maps.Keys(byIssue)), slices.Collect(maps.Keys(byArtifact))
	rows, err := r.database.Pool.Query(ctx, `
		select issue_key, artifact_id::text, max(seq) from events
		where issue_key = any($1) or artifact_id = any($2::uuid[])
		group by issue_key, artifact_id
	`, issueKeys, artifactIDs)
	if err != nil {
		return nil, fmt.Errorf("read the last seq of the deliveries' owners: %w", err)
	}
	defer rows.Close()
	lastSeqs := make(map[eventOwner]int, len(byIssue)+len(byArtifact))
	for rows.Next() {
		var issueKey, artifactID *string
		var seq int
		if err := rows.Scan(&issueKey, &artifactID, &seq); err != nil {
			return nil, fmt.Errorf("read the last seq of the deliveries' owners: %w", err)
		}
		if issueKey != nil {
			lastSeqs[byIssue[*issueKey]] = seq
		} else if artifactID != nil {
			lastSeqs[byArtifact[*artifactID]] = seq
		}
	}
	return lastSeqs, rows.Err()
}

// deliveryPayload is the payload of planned's event, built as Dispatch's own writer of its type
// builds it, from the item as the seeded rows hold it. The reference changes are empty on every
// one, as on the asks' opening events (askOpenedPayload): they tell a live subscriber which
// backlink counts to refresh, and a seeded run has no subscriber.
func (r *seedRun) deliveryPayload(ctx context.Context, tx pgx.Tx, planned delivery, hasAnswers bool) (any, error) {
	switch planned.eventType {
	case "comment.created":
		return commentCreatedPayload(ctx, tx, r.plan.comments[planned.item].ID, hasAnswers)
	case "message.created", "message.answered":
		return messagePayload(ctx, tx, r.plan.messages[planned.item].ID)
	default:
		return transitionAsk(ctx, tx, r.plan.asks[planned.item].ID, planned)
	}
}

// commentCreatedPayload is the comment.created payload of comment id, as the create route builds
// it (api's createCommentFor, commentEventPayload): the comment as its write left it, with the
// name of the document its anchor is in, the project and the slug of the document it is on, the
// thread it joined and the logins with a Replies row on that thread (events.ThreadLogins). The
// row is read as api's scanComment reads one. A delivered comment is not edited and names no
// block it replies to (deliverableComment), so its body is the one it was written with and it
// has no reply block; it was not resolved when it was written, whatever its row holds now, and
// it had no mention to deliver it, the seed writing none. A comment under an ask is in no open
// ask's thread, so its event names whose turn it is nowhere, and it names the ask's question, its
// state and the sessions that own it (api's askSessions): its followers, by when they followed,
// the seed's asks being a person's.
func commentCreatedPayload(ctx context.Context, tx pgx.Tx, id string, hasAnswers bool) (model.CommentEventPayload, error) {
	answers := "null::text"
	if hasAnswers {
		answers = "answers::text"
	}
	var comment model.Comment
	var author, anchor []byte
	if err := tx.QueryRow(ctx, `
		select id::text, issue_key, artifact_id::text, author, body, anchor, reply_to::text, ask_id::text,
		       turn, created_at, `+answers+`
		from comments where id = $1
	`, id).Scan(&comment.ID, &comment.IssueKey, &comment.ArtifactID, &author, &comment.Body, &anchor,
		&comment.ReplyTo, &comment.AskID, &comment.Turn, &comment.CreatedAt, &comment.Answers); err != nil {
		return model.CommentEventPayload{}, fmt.Errorf("read comment %s: %w", id, err)
	}
	if err := json.Unmarshal(author, &comment.Author); err != nil {
		return model.CommentEventPayload{}, fmt.Errorf("decode the author of comment %s: %w", id, err)
	}
	if len(anchor) > 0 {
		comment.Anchor = &model.Anchor{}
		if err := model.DecodeAnchor(anchor, comment.Anchor); err != nil {
			return model.CommentEventPayload{}, fmt.Errorf("decode the anchor of comment %s: %w", id, err)
		}
	}
	comment.Mentions, comment.Deliveries = []model.Mention{}, []model.CommentDelivery{}
	payload := model.CommentEventPayload{Comment: comment}
	payload.Body, payload.BodyTruncated = model.EventBody(comment.Body)
	if comment.Anchor != nil {
		if err := tx.QueryRow(ctx, `select name from artifacts where id = $1`, comment.Anchor.ArtifactID).
			Scan(&payload.ArtifactName); err != nil {
			return model.CommentEventPayload{}, fmt.Errorf("read the document comment %s is anchored in: %w", id, err)
		}
	}
	if comment.ArtifactID != nil {
		if err := tx.QueryRow(ctx, `select coalesce(project_key, ''), slug from artifacts where id = $1`, *comment.ArtifactID).
			Scan(&payload.ProjectKey, &payload.ArtifactSlug); err != nil {
			return model.CommentEventPayload{}, fmt.Errorf("read the document comment %s is on: %w", id, err)
		}
	}
	thread := comment.ID
	switch {
	case comment.AskID != nil:
		thread = *comment.AskID
		if err := tx.QueryRow(ctx, `select question, state from asks where id = $1`, *comment.AskID).
			Scan(&payload.AskQuestion, &payload.AskState); err != nil {
			return model.CommentEventPayload{}, fmt.Errorf("read the ask comment %s replies to: %w", id, err)
		}
		followers, err := asks.Followers(ctx, tx, *comment.AskID)
		if err != nil {
			return model.CommentEventPayload{}, err
		}
		payload.AskSessions = make([]string, 0, len(followers))
		for _, follower := range followers {
			payload.AskSessions = append(payload.AskSessions, follower.SessionID)
		}
	case comment.ReplyTo != nil:
		thread = *comment.ReplyTo
		payload.ThreadRootID = *comment.ReplyTo
	}
	logins, err := events.ThreadLogins(ctx, tx, thread)
	if err != nil {
		return model.CommentEventPayload{}, err
	}
	payload.ThreadLogins = logins
	return payload, nil
}

// messagePayload is the message.created or message.answered payload of message id, as the
// message route builds it (api's createStoredMessage, messageReplyThread.payload): the message as
// its write left it, with no delivery attempt, the seed's messages naming no target; for a reply,
// the head of the message it answers and the target of its thread's root, which no seeded message
// has; and the logins with a Replies row on its thread (events.ThreadLogins). A seeded reply
// answers its thread's root (planMessages), so the message it answers is that root.
func messagePayload(ctx context.Context, tx pgx.Tx, id string) (model.MessageEventPayload, error) {
	var message model.Message
	var author []byte
	if err := tx.QueryRow(ctx, `
		select id::text, issue_key, author, body, target, in_reply_to::text, broadcast_id::text, created_at
		from messages where id = $1
	`, id).Scan(&message.ID, &message.IssueKey, &author, &message.Body, &message.Target, &message.InReplyTo,
		&message.BroadcastID, &message.CreatedAt); err != nil {
		return model.MessageEventPayload{}, fmt.Errorf("read message %s: %w", id, err)
	}
	if err := json.Unmarshal(author, &message.Author); err != nil {
		return model.MessageEventPayload{}, fmt.Errorf("decode the author of message %s: %w", id, err)
	}
	message.Deliveries = []model.MessageDelivery{}
	payload := model.MessageEventPayload{Message: message}
	payload.Body, payload.BodyTruncated = model.EventBody(message.Body)
	root := message.ID
	if message.InReplyTo != nil {
		root = *message.InReplyTo
		var parentBody string
		var rootTarget *string
		if err := tx.QueryRow(ctx, `select body, target from messages where id = $1`, root).
			Scan(&parentBody, &rootTarget); err != nil {
			return model.MessageEventPayload{}, fmt.Errorf("read the message %s answers: %w", id, err)
		}
		payload.ReplyBody = text.HeadRunes(parentBody, messageReplyPreviewRunes)
		if rootTarget != nil {
			payload.ThreadTarget = *rootTarget
		}
	}
	logins, err := events.ThreadLogins(ctx, tx, root)
	if err != nil {
		return model.MessageEventPayload{}, err
	}
	payload.ThreadLogins = logins
	return payload, nil
}

// transitionAsk answers or declines ask id as planned says, as the answer and the decline routes
// do (api's answerTransition and declineAsk, over closeAskTx), and answers the event's payload.
// It runs under the ask's owner lock, which its batch takes before any other statement
// (writeDeliveries), as those routes take it before they read the ask (api's transitionAskTx,
// requireOpenOwner); the window write that ends an answer's index takes that lock again
// (refs.WriteItemWindows), which a transaction holding it passes at once. It reads the ask as
// those routes read it (api's readAskForTransition): its row, the document its anchor is in and
// its opening event. The seed's asks the deliveries take are open questions with no block
// (planDeliveries), so neither the document a block ask writes its state into nor an approval's
// document is read or written.
//
// An answer is the seed's person's, choosing the ask's first option where it offers any, with
// the planned words as its text, at the planned time; the row takes it, and the ask's parts are
// indexed again at the answer's time (refs.AskPartsAt), as the route indexes them, since the
// answer is a part of the ask. A decline records model.AskResolutionDeclined with the planned
// words as the person's note, the words Dispatch writes for the asking agent
// (model.DeclineDispatchWords), the person and the planned time, as docs.WriteAskResolution
// records one; the row takes it and the state a resolution leaves. A decline writes no part and
// moves none of the columns an ask's index is owed on (asks_item_blocks_owed), so its parts stay
// as they were indexed. The payload is the ask as the transition left it, with the logins with a
// Replies row on its thread read after the write (events.ThreadLogins): model.AskAnsweredEventPayload
// for an answer, with no answer replaced, and model.AskEventPayload for a decline.
func transitionAsk(ctx context.Context, tx pgx.Tx, id string, planned delivery) (any, error) {
	var project, slug, name *string
	var primary *bool
	ask, err := docs.ScanAsk(tx.QueryRow(ctx, `
		select `+docs.AskColumns+`, aa.project_key, aa.slug, aa.name, aa.is_primary
		from asks a
		left join artifacts aa on aa.id = `+store.CastableUUID(`a.anchor->>'artifact_id'`)+`
		where a.id = $1
	`, id), &project, &slug, &name, &primary)
	if err != nil {
		return nil, fmt.Errorf("read ask %s: %w", id, err)
	}
	if ask.State != "open" {
		return nil, fmt.Errorf("ask %s is %s, and a delivery answers or declines an open ask", id, ask.State)
	}
	if project != nil {
		ask.AnchorArtifact = &model.AskAnchorArtifact{Project: *project, Slug: *slug, Name: *name, Primary: *primary}
	}
	opened, err := events.OpenedEventIDs(ctx, tx, []string{id})
	if err != nil {
		return nil, fmt.Errorf("read the opening event of ask %s: %w", id, err)
	}
	if openedID, held := opened[id]; held {
		ask.OpenedEventID = &openedID
	}
	switch planned.eventType {
	case "ask.answered":
		selected := []string{}
		if len(ask.Options) > 0 {
			selected = []string{ask.Options[0].Label}
		}
		answer := model.AskAnswer{User: seedAuthor.ID, Selected: selected, Text: &planned.words, At: planned.at}
		encoded, err := json.Marshal(answer)
		if err != nil {
			return nil, fmt.Errorf("encode the answer of ask %s: %w", id, err)
		}
		if _, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1`, id, encoded); err != nil {
			return nil, fmt.Errorf("answer ask %s: %w", id, err)
		}
		ask.State, ask.Answer = "answered", &answer
		if err := indexAskParts(ctx, tx, ask, refs.AskPartsAt(ask)); err != nil {
			return nil, err
		}
		payload := model.AskAnsweredEventPayload{AskEventPayload: model.NewAskEventPayload(ask, model.ReferenceChanges{})}
		if payload.ThreadLogins, err = events.ThreadLogins(ctx, tx, id); err != nil {
			return nil, err
		}
		return payload, nil
	case "ask.resolved":
		resolution := model.AskResolution{
			Kind: model.AskResolutionDeclined, Reason: planned.words, DispatchWords: model.DeclineDispatchWords,
			Actor: seedAuthor, At: planned.at,
		}
		encoded, err := json.Marshal(resolution)
		if err != nil {
			return nil, fmt.Errorf("encode the decline of ask %s: %w", id, err)
		}
		if _, err := tx.Exec(ctx, `update asks set state = 'resolved', resolution = $2 where id = $1`, id, encoded); err != nil {
			return nil, fmt.Errorf("decline ask %s: %w", id, err)
		}
		ask.State, ask.Resolution = "resolved", &resolution
		payload := model.NewAskEventPayload(ask, model.ReferenceChanges{})
		if payload.ThreadLogins, err = events.ThreadLogins(ctx, tx, id); err != nil {
			return nil, err
		}
		return payload, nil
	}
	return nil, fmt.Errorf("delivery %d of ask %s has type %s, which closes no ask", planned.id, id, planned.eventType)
}

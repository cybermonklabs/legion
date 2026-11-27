package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

type commentInput struct {
	Body    string             `json:"body"`
	Anchor  *model.AnchorInput `json:"anchor"`
	ReplyTo *string            `json:"reply_to"`
	// ReplyToBlock is the id of the block the reply answers ("b2"), read from the item the same
	// request names (namedReplyBlock): a block of comment ReplyTo, checked against that comment
	// before reply_to climbs to the thread's head, or, with AskID in place of ReplyTo, a part of
	// that ask.
	ReplyToBlock *string `json:"reply_to_block"`
	AskID        *string `json:"ask_id"`
	Mentions     []struct {
		Target string `json:"target" required:"true"`
	} `json:"mentions"`
	Delivery *string `json:"delivery"`
	// Turn is who holds the turn after this ask reply: "agent" for a progress note
	// that keeps the ask waiting on its asker, "human" (the default for a session
	// author) when the human needs to act. Ignored for a human author, whose reply
	// always hands the turn to the agent. Rejected on a comment that is not an ask reply.
	Turn       *string `json:"turn"`
	Suggestion *struct {
		ReplaceWith string `json:"replace_with"`
	} `json:"suggestion"`
	Actor *model.Actor `json:"actor"`
}

// storedSuggestion is a suggestion as the create writes it into the row's suggestion column: the
// suggestion itself, and the kind its mark recorded when the comment was made (docs.SuggestionKind:
// "insert", "replace" or "delete"; the mark a quote anchor writes is a replace). An insert's mark
// wraps the words the suggestion itself put in the document, so its anchor's quote is a copy of
// the comment's own text until an accept makes those words document text, and the delete reads
// the kind (loadStoredSuggestion) to empty that quote once the mark is gone and answers no kind:
// an orphaned suggestion's, and a rejected one's, whose reject took the words out
// (deleteComment). An accept or a reject merges what it decides into the column, which keeps the
// kind (commentAction), so a decided suggestion keeps it. Only the delete reads the kind; every
// other read decodes the column into model.Suggestion, which has no kind and drops it, so no
// answer and no event carries it. A suggestion with no anchor has no mark and stores no kind.
type storedSuggestion struct {
	model.Suggestion
	Kind string `json:"kind,omitempty"`
}

// loadStoredSuggestion is the kind comment id's suggestion column holds (storedSuggestion), read
// in tx: "" for a row with no suggestion, for one with no anchor, and for one made before the
// create stored the kind.
func loadStoredSuggestion(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	var kind string
	err := tx.QueryRow(ctx, `
		select coalesce(suggestion->>'kind', '') from comments where id = $1
	`, id).Scan(&kind)
	return kind, err
}

// askReplyTurn is who holds the turn once an ask reply by actor is posted: a human's
// reply always hands it to the agent; a session's reply hands it to the human unless
// the request marks it a progress note (turn "agent").
func askReplyTurn(actor model.Actor, requested *string) string {
	if actor.Kind == "user" {
		return "agent"
	}
	if requested != nil {
		return *requested
	}
	return "human"
}

// commentParties is who one comment names: the delivering parties, each once and in the order
// the request's array and then the body name them, the login of each person the body names,
// each once and in the order it names them, and whether it names any party at all. It is the
// set every mention rule is keyed on and the set the deliveries are resolved from.
//
// A person is in people and never in targets: a person reads the record, receives no delivery
// and does not count toward the cap. Either a target or a person makes named true, which is what
// makes a typed /btw the command it looks like (validateCommentMentions).
type commentParties struct {
	targets []string
	people  []string
	named   bool
}

// partyTarget is the delivery route a party reference names - `session:<id>` for an agent,
// `role:<token>` for a role - or "" for a party kind that is neither: a person, whom nothing is
// delivered to, and any kind text.IsParty takes that this reader does not know.
func partyTarget(ref text.Ref) string {
	switch ref.Kind {
	case "session":
		return "session:" + ref.ID
	case "role":
		return "role:" + ref.ID
	}
	return ""
}

// checkMentionRoute refuses a target that model.ParseRoute cannot read, with the same code and
// reason for the body and the mentions array. The reference grammar can name sessions whose IDs
// have dots or colons, but those IDs cannot be used as delivery routes.
// A route of a valid form is accepted here even if nobody is available to receive it.
func checkMentionRoute(target string) error {
	if _, err := model.ParseRoute(target); err != nil {
		return errorf(http.StatusBadRequest, "MENTION_INPUT", "mention target must be role:<name> or session:<id>")
	}
	return nil
}

// commentPartiesOf is the party set of one comment: the targets the request's `mentions` array
// carries, as it wrote them, and then the parties its body names (R4), read with the shared
// reader. A party both name, and one the body names twice, is one party: the repeat is dropped
// here, before any rule reads the set. A target of a form no delivery route takes refuses the
// write (checkMentionRoute), however the caller names it. Only the array rejects repeats.
//
// A party reference a code context holds names nobody (text.Located.InCode): a body that writes
// one inside a fenced block, an indented block or a code span is showing the address rather than
// naming its party, which is how every renderer of the record draws it, so the party is not
// delivered to, not listed and not counted toward the cap. It is still a reference the graph
// indexes. The same party the body also names outside code is named by that occurrence, and a
// mention's address is no code context at all: the mention is `[@Name](address)`
// (`[@Planner](dispatch://agent/...)`), and only its address is read here, never its words.
func commentPartiesOf(input commentInput, body []text.Located) (commentParties, error) {
	parties := commentParties{targets: make([]string, 0, len(input.Mentions)+len(body))}
	seen := make(map[string]struct{}, len(input.Mentions)+len(body))
	for _, mention := range input.Mentions {
		if err := checkMentionRoute(mention.Target); err != nil {
			return commentParties{}, err
		}
		if _, duplicate := seen[mention.Target]; duplicate {
			return commentParties{}, errorf(http.StatusBadRequest, "MENTION_INPUT", "mentions must not repeat a target")
		}
		seen[mention.Target] = struct{}{}
		parties.targets = append(parties.targets, mention.Target)
	}
	parties.named = len(parties.targets) > 0
	for _, located := range body {
		if located.InCode || !located.IsParty() {
			continue
		}
		parties.named = true
		// A person is named by being one, not by being the party left over when no route is
		// written: a person reads the record and is never delivered to (R5).
		if located.Kind == "person" {
			// The same person named twice is one person, as a repeated target is one party. The
			// repeat is found in the set the targets are, under a `person:` key no route is
			// written as, since a body may name any number of people.
			key := "person:" + located.ID
			if _, repeated := seen[key]; !repeated {
				seen[key] = struct{}{}
				parties.people = append(parties.people, located.ID)
			}
			continue
		}
		// A party other than a person must name a route, not just a valid reference address.
		target := partyTarget(located.Ref)
		if err := checkMentionRoute(target); err != nil {
			return commentParties{}, err
		}
		if _, repeated := seen[target]; repeated {
			continue
		}
		seen[target] = struct{}{}
		parties.targets = append(parties.targets, target)
	}
	return parties, nil
}

// validateCommentMentions is the comment's delivery mode and its delivering targets, or the
// refusal its party set earns. Every refusal is keyed on that set (commentPartiesOf): a
// delivery mode with no party named anywhere, more delivering parties than the cap, a mode
// outside the three, a target of no known form, and an array that repeats a target. Each
// answers 400 MENTION_INPUT, as it always has for the array.
func validateCommentMentions(input commentInput, body []text.Located) (string, []string, error) {
	parties, err := commentPartiesOf(input, body)
	if err != nil {
		return "", nil, err
	}
	if !parties.named {
		if input.Delivery != nil {
			return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "delivery requires mentions")
		}
		return "", nil, nil
	}
	if len(parties.targets) > 10 {
		return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "mentions allow at most 10 targets")
	}
	delivery := "steer"
	if input.Delivery != nil {
		delivery = *input.Delivery
		if !validDelivery(delivery) {
			return "", nil, errorf(http.StatusBadRequest, "MENTION_INPUT", "delivery must be one of btw, aside, steer")
		}
	}
	return delivery, parties.targets, nil
}

type commentThreadTarget struct {
	ReplyTo *string
	// Answers is the comment the request named (reply_to, or the comment a delivery callback
	// answers), which the thread climb keeps while ReplyTo moves to the head of the thread. It
	// is set whenever the request named a comment, the head of its own thread included, so a
	// reader never has to guess which replies carry it; nil on a reply that names an ask alone
	// and on a root comment.
	Answers     *string
	AskID       *string
	ReplyRoot   *model.Comment
	AskQuestion string
	AskState    string
	// AskMoved says the ask is an approval request a version moved past the one its agent last
	// handed to the human (approvalMoved): it waits on its agent whatever a reply's turn says.
	AskMoved bool
	// AskAuthorSession is the session that wrote the thread's ask, nil when a person wrote it
	// or the thread is no ask's (describeAsk).
	AskAuthorSession *string
	// ReplyBlock is the block of the comment reply_to named that the reply answers, nil when it
	// names none: that comment's, not the thread head's that ReplyTo then holds.
	ReplyBlock *namedBlock
}

// openAsk says the thread hangs under an open ask, the only kind with a turn.
func (t commentThreadTarget) openAsk() bool {
	return t.AskID != nil && t.AskState == "open"
}

// replyTurn is who holds the turn once a reply into this thread is posted. Only an open ask
// has a turn to hold: a reply under an answered or resolved ask records none, so the column
// always means "who the ask waits on after this".
func (t commentThreadTarget) replyTurn(actor model.Actor, requested *string) *string {
	if !t.openAsk() {
		return nil
	}
	return new(askReplyTurn(actor, requested))
}

// waitingOnAfter is whom the thread's open ask waits on once a reply recording turn (replyTurn's)
// is its newest comment; "" when the thread is not an open ask's. It is waitingOnExpression for that
// row. The comment write holds the owner lock that every version write, hand-back and comment insert
// takes first, so the ask describeAsk read is the one the reply commits beside, and the reply is the
// thread's newest (migration 0065): a moved approval request waits on its agent, and otherwise the
// reply's own turn decides, since a hand-back can only have recorded an older reply.
func (t commentThreadTarget) waitingOnAfter(turn *string) string {
	switch {
	case !t.openAsk():
		return ""
	case t.AskMoved:
		return "agent"
	default:
		return *turn
	}
}

// eventThread is what a comment event says about the thread the comment joined, given the
// derived ask turn after this comment is its newest reply.
func (t commentThreadTarget) eventThread(waitingOn string) commentEventThread {
	thread := commentEventThread{AskQuestion: t.AskQuestion, AskState: t.AskState, AskWaitingOn: waitingOn}
	if t.ReplyTo != nil {
		thread.ThreadRootID = *t.ReplyTo
	}
	return thread
}

func (s *server) createComment(w http.ResponseWriter, r *http.Request) {
	s.createCommentFor(w, r, issueOwner(r.PathValue("key")))
}

// commentRecovery is the recovery point a keyed comment commits with itself
// (bindRequestKeyRecovery): the comment, whose mentions deliver it.
type commentRecovery struct {
	CommentID string `json:"comment_id"`
}

// replayComment answers a repeat of a keyed comment that committed: the comment with its mentions
// and deliveries as they stand, in the first answer's shape - its advice and ask_waiting_on kept -
// under the comment's own 201, with every mention it still owes put back on the mention sender's
// lanes behind the answer, as the first request's were. A mention owes its delivery when its
// process died after the comment committed and before the mention's one attempt was sent: that
// attempt is still pending, and its claim, if any, has lapsed. This process claims it before it
// queues it (claimQueuedMention), as a comment of its own commits its attempt claimed, so the
// delivery sweep leaves it to this process's lane; one another sender claimed first is left to
// that sender and not queued here at all. It is queued under its own number, so the listener drops
// a frame that did land, and the send binds to that number and to this process's claim, as the
// comment's own first send does (sendCommentMention, attempt 1), so no second attempt is opened.
// A mention someone retried from its card has more than one attempt, and owes nothing here. An
// attempt no lane takes - this process is stopping, or its queue is full - has its claim given
// back and is the delivery sweep's, as the first request's would have been.
func (s *server) replayComment(w http.ResponseWriter, r *http.Request, point commentRecovery, stored []byte) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	ctx, pool := r.Context(), s.deps.Store.Pool
	comment, err := s.loadComment(ctx, pool, point.CommentID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	owed, err := s.owedCommentMentions(ctx, comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if len(owed) > 0 {
		created, err := s.loadCommentCreatedEvent(ctx, pool, comment)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		sending := context.WithoutCancel(ctx)
		for _, mention := range owed {
			item := mentionSend{
				comment: comment, created: created, mention: mention, actor: comment.Author, attempt: 1,
				takeover: s.newTakeoverRetry(),
			}
			claimed, mine, err := s.claimQueuedMention(sending, item.key())
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if !mine {
				continue
			}
			item.claimed = claimed
			s.mentions.enqueue(sending, item)
		}
	}
	hydrated, err := s.hydrateCommentSideTables(ctx, pool, who, []model.Comment{comment})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.writeRefreshed(w, http.StatusCreated, stored, hydrated[0])
}

// owedCommentMentions is each mention of the comment whose one attempt is pending with no live
// claim (model.DeliveryClaimFreeSQL), in the mode it was opened with.
func (s *server) owedCommentMentions(ctx context.Context, commentID string) ([]ResolvedMention, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select target, min(delivery), count(*), coalesce(bool_and(state = 'pending' and `+model.DeliveryClaimFreeSQL+`), false)
		from comment_deliveries where comment_id = $1
		group by target
		order by target
	`, commentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owed []ResolvedMention
	for rows.Next() {
		var target, delivery string
		var attempts int
		var stranded bool
		if err := rows.Scan(&target, &delivery, &attempts, &stranded); err != nil {
			return nil, err
		}
		if attempts == 1 && stranded {
			owed = append(owed, ResolvedMention{Target: target, Delivery: delivery})
		}
	}
	return owed, rows.Err()
}

func (s *server) normalizeCommentThreadTarget(
	ctx context.Context,
	tx pgx.Tx,
	ledger *docs.Ledger,
	who model.Actor,
	owner owner,
	input commentInput,
) (commentThreadTarget, error) {
	if input.ReplyTo != nil && input.AskID != nil {
		return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to and ask_id cannot both be set")
	}
	if input.Anchor != nil && (input.ReplyTo != nil || input.AskID != nil) {
		return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "replies cannot carry anchors")
	}
	if input.Suggestion != nil && (input.ReplyTo != nil || input.AskID != nil) {
		return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "replies cannot carry suggestions")
	}
	if input.ReplyToBlock != nil && input.ReplyTo == nil && input.AskID == nil {
		return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT",
			"reply_to_block requires reply_to, the comment whose block it names, or ask_id, the ask whose part it names")
	}
	if input.ReplyTo != nil {
		if strings.TrimSpace(*input.ReplyTo) == "" {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must be a full comment id")
		}
		if _, err := uuid.Parse(*input.ReplyTo); err != nil {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must be a full comment id")
		}
		parent, err := s.loadOwnedComment(ctx, tx, owner, *input.ReplyTo)
		if err != nil {
			return commentThreadTarget{}, err
		}
		// A reply adds text under the post it answers, and a deleted post takes none: the
		// thread reply is refused as the delivery callback's reply is (comment_delivery.go's
		// replyComment), on the parent this walk has already locked, so the refusal costs no
		// read of its own. Only the comment the request named is judged: a reply under a live
		// comment stands whatever became of the root above it, and a reply written before the
		// delete keeps its text and its place.
		if parent.DeletedAt != nil {
			return commentThreadTarget{}, deletedPostError(refs.ItemComment)
		}
		target, err := s.threadHeadOf(ctx, tx, owner, parent)
		if err != nil || input.ReplyToBlock == nil {
			return target, err
		}
		target.ReplyBlock, err = s.namedReplyBlock(ctx, tx, ledger, who, refs.ItemComment, parent.ID, *input.ReplyToBlock)
		if err != nil {
			return commentThreadTarget{}, err
		}
		return target, nil
	}
	if input.AskID == nil {
		return commentThreadTarget{}, nil
	}
	if strings.TrimSpace(*input.AskID) == "" {
		return commentThreadTarget{}, s.askIDInputForOwner(ctx, tx, who, owner)
	}
	if _, err := uuid.Parse(*input.AskID); err != nil {
		return commentThreadTarget{}, s.askIDInputForOwner(ctx, tx, who, owner)
	}
	target, err := s.describeAsk(ctx, tx, owner, input.AskID)
	if err != nil || input.ReplyToBlock == nil {
		return target, err
	}
	target.ReplyBlock, err = s.namedReplyBlock(ctx, tx, ledger, who, refs.ItemAsk, *input.AskID, *input.ReplyToBlock)
	if err != nil {
		return commentThreadTarget{}, err
	}
	return target, nil
}

// namedReplyBlock is the block reply_to_block names, read from the item the same request named:
// the comment reply_to names, read under the owner's lock the thread climb took, or the ask
// ask_id names. It is never read from the thread's head: a reply to a comment in an ask's thread
// names a block of that comment, as the client and the tool that write it mean, where only a
// reply naming the ask names one of the ask's parts. The ask is ordered against a write of its
// own by its owner's lock (store.LockOwner), which this transaction took before it read either
// item (lockCommentOwner), and under which asks.go's readAskForTransition reads the row. A block
// who, the reply's writer, may not see is refused as one the item does not hold.
//
// It is planReplyBlock with the index the item owes queued on ledger, the write phase of this
// route, since this route's writes are one queued batch: the reply's own route is the only
// caller, and reply_to_block is the only field it reads, so the field and the code a refusal
// names are written here rather than passed in.
func (s *server) namedReplyBlock(
	ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, who model.Actor, kind refs.ItemKind, itemID, blockID string,
) (*namedBlock, error) {
	repair, block, err := s.planReplyBlock(ctx, tx, who, kind, itemID, blockID, "reply_to_block", "INVALID_COMMENT")
	if err != nil {
		return nil, err
	}
	if !repair.Owed() {
		return block, nil
	}
	return block, ledger.Queue(func(b *store.WriteBatch) error {
		repair.Queue(b)
		return nil
	})
}

// threadHeadOf climbs reply_to from a comment read under the owner's lock to the head of its
// thread: the ask the thread hangs under, or the thread's root comment when it hangs under no ask.
// The walk is bounded like every other parent walk (the outbox's thread walk, the issue ancestor
// walk): comments.reply_to has no acyclicity constraint, so a cyclic chain answers 400 instead of
// holding the transaction open.
//
// The climb flattens the thread into one level, which is what every thread read selects on, so
// the comment the writer answered - `from`, the one the request named - is kept beside the head
// as Answers and stored in comments.answers, and the graph's `answers` arm nests a reply under
// it (migration 20261105014808).
func (s *server) threadHeadOf(
	ctx context.Context, tx pgx.Tx, owner owner, from model.Comment,
) (commentThreadTarget, error) {
	target, err := s.threadHeadFrom(ctx, tx, owner, from)
	if err != nil {
		return commentThreadTarget{}, err
	}
	target.Answers = &from.ID
	return target, nil
}

// threadHeadFrom is threadHeadOf's climb alone, without the answered comment its caller adds.
func (s *server) threadHeadFrom(
	ctx context.Context, tx pgx.Tx, owner owner, from model.Comment,
) (commentThreadTarget, error) {
	root := from
	for depth := 1; ; depth++ {
		if root.AskID != nil {
			return s.describeAsk(ctx, tx, owner, root.AskID)
		}
		if root.ReplyTo == nil {
			return commentThreadTarget{ReplyTo: &root.ID, ReplyRoot: &root}, nil
		}
		if depth >= parentDepthCap {
			return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must identify a comment on this owner")
		}
		next, err := s.loadOwnedComment(ctx, tx, owner, *root.ReplyTo)
		if err != nil {
			return commentThreadTarget{}, err
		}
		root = next
	}
}

// describeAsk reads the ask on this owner that a reply joins, as the thread it heads, refusing one
// that belongs to another.
func (s *server) describeAsk(
	ctx context.Context, tx pgx.Tx, owner owner, askID *string,
) (commentThreadTarget, error) {
	target := commentThreadTarget{AskID: askID}
	err := tx.QueryRow(ctx, `
		select a.question, a.state, `+approvalMoved+`,
		       case when a.author->>'kind' = 'session' then a.author->>'id' end
		from asks a
		where a.id = $1
		  and a.issue_key is not distinct from $2
		  and a.artifact_id is not distinct from $3
	`, *askID, owner.IssueKey, owner.ArtifactID).Scan(&target.AskQuestion, &target.AskState, &target.AskMoved, &target.AskAuthorSession)
	if errors.Is(err, pgx.ErrNoRows) {
		return commentThreadTarget{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "ask_id must identify an ask on this owner")
	}
	return target, err
}

// askSessions is what a comment.created in askID's thread says about whose ask it is
// (model.CommentEventPayload.AskSessions), as the comment's write leaves them: author, the
// session that wrote the ask when one did (describeAsk's AskAuthorSession), then each other
// session asks.Followers reads, ordered by since and then session id, then replier, appended
// last, when it is a session none of those names, and [] when no session owns the ask. It reads
// in the route's read phase, where the read holds no transaction id and finds the followers
// committed before this write, but not the follow the write queues for a session that replies
// (asks.QueueFollowAuthor), which only Ledger.Commit sends: so the replier is added here rather
// than read.
func askSessions(ctx context.Context, tx pgx.Tx, askID string, author *string, replier model.Actor) ([]string, error) {
	followers, err := asks.Followers(ctx, tx, askID)
	if err != nil {
		return nil, err
	}
	sessions := make([]string, 0, len(followers)+2)
	if author != nil {
		sessions = append(sessions, *author)
	}
	for _, follower := range followers {
		if author == nil || follower.SessionID != *author {
			sessions = append(sessions, follower.SessionID)
		}
	}
	if replier.Kind == "session" && replier.ID != "" && !slices.Contains(sessions, replier.ID) {
		sessions = append(sessions, replier.ID)
	}
	return sessions, nil
}

// commentRoute is the owner's delivery route as the Envoy listener resolved it before the
// comment's transaction opened: the route string the pre-transaction read saw, and the session
// it names - the one its lookup pinned, or, when the route's own lookup went unanswered, the <id>
// of a session:<id> route (mentionedSession). checked is false where the write consults no route
// at all - a project document, a comment with no mentions, an owner that does not exist - and the
// locked row is then not compared against it. unanswered says the route string is one the comment
// mentions and the listener did not answer that mention's lookup in time; sessionID is then nil.
type commentRoute struct {
	checked    bool
	route      string
	sessionID  *string
	unanswered bool
}

// commentResolution is what the Envoy listener said about a comment before its transaction
// opened: the session each mention names, and the owner's route with the session it names.
// takeovers is each mention's take-over pacing (resolveThroughTakeover), which its sender carries
// on in after the commit, unless the send waits on a held lane first, which starts a window of its
// own at its delivery (mentionSend.takeover). decidesRoutes is set when the comment reaches
// sessions beyond its mentions, so the mentions were resolved through a take-over and the
// comment's other routes are decided on them; the comment.created event records it
// (mentions_decide_routes).
type commentResolution struct {
	mentions      []ResolvedMention
	takeovers     []takeoverRetry
	route         commentRoute
	decidesRoutes bool
}

// routeSuppression is what a comment's event says about the copy its owner's route sends
// (suppress_route, suppressed_route and suppressed_route_session_id): the copy is suppressed when a
// mention delivers the comment to the session the route reaches. Either the route names a session
// (its lookup pinned it, or its own lookup went unanswered and it is session:<id>) that a mention
// delivers the comment to, as far as creation knows (mentionedSession: each pinned session, and
// the <id> of an unanswered session:<id> mention), or the route string is the target of a mention
// whose lookup went unanswered - one string names one session - and is then suppressed by that
// string alone, so a route later moved to that session's direct form is not. The route copy still
// duplicates the mention for an unanswered role:X mention whose holder the route names under
// another string, and for a role:X route whose own lookup went unanswered while a mention of
// another string reaches its holder: neither names a session to compare.
//
// The suppression names a candidate only, and the outbox's other session routes (an author, the
// thread's sessions, the primary document's, the claimant, a follower) take the comment's mentions
// as their candidates the same way. The outbox decides each copy when it publishes
// (destinations.leftTo); publishFallbackCopies in internal/dispatch/outbox is the one description.
func (resolved commentResolution) routeSuppression() (suppressed bool, suppressedRoute string, suppressedSessionID *string) {
	route := resolved.route
	switch {
	case route.sessionID != nil:
		for _, mention := range resolved.mentions {
			if sessionID, named := mention.mentionedSession(); named && sessionID == *route.sessionID {
				return true, route.route, route.sessionID
			}
		}
	case route.unanswered:
		return true, route.route, nil
	}
	return false, "", nil
}

// commentWrite is the open comment transaction and what its locked owner said: the owner's
// lifecycle status (nil for a project document) and the resolution that row agreed with.
type commentWrite struct {
	tx     pgx.Tx
	status *string
	commentResolution
}

// resolveCommentDeliveries makes every Envoy listener lookup a comment needs, with no transaction
// open, so no listener call costs a pooled connection: each mention, and the owner's delivery route
// when the comment mentions anyone. A route the comment already mentions is resolved by that
// mention, so the ordinary mention comment makes no second listener call for it.
//
// The transaction decides from these which sessions the comment's other routes skip: the issue's
// route when a mention resolved to its session (suppress_route), and every session route the
// outbox publishes - the issue's claimant, the thread's sessions, the ask's followers, the
// document's authors - for each session a mention resolved to (the payload's mentions). A comment
// that has such a route - the issue's route is set, it replies to a thread, it is on a document,
// or readCommentRoutes found a session route on its issue or ask - resolves each mention and the
// route through a listener take-over (resolveThroughTakeover), because a mention that did not
// resolve here leaves those routes on, and the same mention resolved by its sender after the
// commit would reach a session those routes reach too: the session would hear the comment twice.
// So the comment.created event records that its routes were decided on these resolutions
// (mentions_decide_routes), and each mention's attempt 1 keeps its resolution for whichever sender
// finishes it, the delivery sweep and a Retry included (deliverResolvedCommentMention). A comment
// with no such route commits at once, and its mentions' senders wait out the take-over under the
// attempts the commit made durable, resolving the mention again. Every lookup runs at once, so a
// comment waits about one take-over window however many mentions it carries
// (validateCommentMentions allows ten).
// The lookups share the request's tracking mark because they call only the listener and never
// take a pooled connection; one that read the database would need a mark of its own, as each
// send on the mention sender's lanes takes (store.WithTransactionTracking in sendCommentMention).
func (s *server) resolveCommentDeliveries(
	ctx context.Context, owner owner, input commentInput, targets []string, delivery string,
) (commentResolution, error) {
	if len(targets) == 0 {
		return commentResolution{}, nil
	}
	route, sessionRoutes, err := s.readCommentRoutes(ctx, owner, input.AskID)
	if err != nil {
		return commentResolution{}, err
	}
	wait := route.route != "" || sessionRoutes || input.ReplyTo != nil || input.Anchor != nil || owner.ArtifactID != nil
	resolved := commentResolution{
		mentions:      make([]ResolvedMention, len(targets)),
		takeovers:     make([]takeoverRetry, len(targets)),
		route:         route,
		decidesRoutes: wait,
	}
	mentioned := slices.Index(targets, route.route)
	var routeResolution ResolvedMention
	var lookups sync.WaitGroup
	for index, target := range targets {
		lookups.Go(func() {
			resolved.mentions[index], resolved.takeovers[index] = s.resolveThroughTakeover(ctx, target, delivery, wait)
		})
	}
	if route.route != "" && mentioned < 0 {
		lookups.Go(func() { routeResolution, _ = s.resolveThroughTakeover(ctx, route.route, "steer", wait) })
	}
	lookups.Wait()
	switch {
	case route.route == "":
	case mentioned >= 0:
		resolved.route.sessionID = resolved.mentions[mentioned].SessionID
		resolved.route.unanswered = resolved.mentions[mentioned].unanswered
	default:
		// A route whose own lookup went unanswered still names its session when it is
		// session:<id> (mentionedSession); one that named no session pins none.
		if sessionID, named := routeResolution.mentionedSession(); named {
			resolved.route.sessionID = &sessionID
		}
	}
	return resolved, nil
}

// readCommentRoutes reads the owner's delivery route from the pool, locking nothing; the comment's
// transaction re-reads it under the owner's lock (lockCommentOwner). sessionRoutes reports, from
// the same read, whether the outbox reaches a session of the issue beyond the route and the
// comment's mentions whatever the comment's thread or anchor: the issue's claimant is a session
// (the outbox's addClaimantRoute), the ask the comment replies to by askID has a follower
// (addFollowerRoutes), or the issue's primary document credits a session
// (addPrimaryDocumentAuthors, which reads the credits through the outbox's documentCreditsSQL
// and sessionCreditSQL; the query below reads them the same way). Like the route, these are read
// before the transaction and not rechecked under the lock: the outbox reads them again when it
// publishes in any case.
func (s *server) readCommentRoutes(ctx context.Context, owner owner, askID *string) (commentRoute, bool, error) {
	if owner.IssueKey == nil {
		return commentRoute{}, false, nil
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return commentRoute{}, false, errorf(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	}
	// An ask id that is no uuid names no ask; the transaction refuses it (normalizeCommentThreadTarget).
	var ask *string
	if askID != nil {
		if _, err := uuid.Parse(*askID); err == nil {
			ask = askID
		}
	}
	var stored *string
	var sessionRoutes bool
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select i.route,
		       coalesce(i.claimed_by ->> 'kind' = 'session' and coalesce(i.claimed_by ->> 'id', '') <> '', false)
		       or exists (select 1 from ask_followers f where f.ask_id = $2::uuid)
		       or exists (
		           select 1
		           from artifacts a
		           where a.issue_key = i.key and a.is_primary and a.kind = 'doc'
		             and (a.created_by ->> 'kind' = 'session' and coalesce(a.created_by ->> 'id', '') <> ''
		                  or exists (
		                      select 1
		                      from artifact_versions v
		                      cross join lateral jsonb_array_elements(nullif(v.authors, 'null'::jsonb)) as credit(author)
		                      where v.artifact_id = a.id
		                        and credit.author ->> 'kind' = 'session' and coalesce(credit.author ->> 'id', '') <> ''
		                  ))
		       )
		from issues i
		where i.key = $1
	`, *owner.IssueKey, ask).Scan(&stored, &sessionRoutes)
	if errors.Is(err, pgx.ErrNoRows) {
		// The transaction answers for an owner that does not exist; there is nothing to resolve.
		return commentRoute{}, false, nil
	}
	if err != nil {
		return commentRoute{}, false, err
	}
	route := commentRoute{checked: true}
	if stored != nil {
		route.route = *stored
	}
	return route, sessionRoutes, nil
}

// beginCommentWrite opens the comment's transaction, takes its owner's lock, and verifies that the
// route the listener resolved is still the one the issue row carries, read under that lock. A
// route that moved in between leaves the resolution unusable, so the write is resolved once more
// rather than suppressing the wrong session's fan-out.
func (s *server) beginCommentWrite(ctx context.Context, owner owner, resolved commentResolution, write closedWrite) (commentWrite, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return commentWrite{}, err
	}
	// The transaction is the caller's on success only, so every refusal unwinds here: a later
	// check that forgot its own rollback would hold a pooled connection for the process's life.
	status, err := s.lockCommentOwner(ctx, tx, owner, resolved.route, write)
	if err != nil {
		_ = tx.Rollback(ctx)
		return commentWrite{}, err
	}
	return commentWrite{tx: tx, status: status, commentResolution: resolved}, nil
}

// lockCommentOwner takes the owner's lock inside an open comment transaction and returns its
// lifecycle status, read under that lock, refusing with errStaleResolution when the issue no
// longer carries the route the resolution was taken for. A closed issue still takes a comment -
// issue-level, a reply, a reply on an ask, one anchored to a document's quote - but not a
// suggestion: that is a proposed document edit, and accepting it, its only outcome, is refused
// on a closed issue, which is what write says (refuseClosed).
func (s *server) lockCommentOwner(ctx context.Context, tx pgx.Tx, owner owner, route commentRoute, write closedWrite) (*string, error) {
	status, open, err := ownerStatus(ctx, owner, lockedOwnerRead(tx))
	if err != nil {
		return nil, err
	}
	if err := refuseClosed(open, write); err != nil {
		return nil, err
	}
	if !route.checked {
		return status, nil
	}
	var stored *string
	if err := tx.QueryRow(ctx, `select route from issues where key = $1`, *owner.IssueKey).Scan(&stored); err != nil {
		return nil, err
	}
	locked := ""
	if stored != nil {
		locked = *stored
	}
	if locked != route.route {
		return nil, errStaleResolution
	}
	return status, nil
}

func (s *server) createCommentFor(w http.ResponseWriter, r *http.Request, owner owner) {
	var input commentInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A comment's body and its suggestion are markdown a reader is shown, so both reach the store
	// the way text written into a document reaches it (pmdoc.LineFeeds): the row, the event, the
	// mention delivery and the tree the display read makes of the body all see one line ending.
	input.Body = pmdoc.LineFeeds(input.Body)
	if input.Suggestion != nil {
		input.Suggestion.ReplaceWith = pmdoc.LineFeeds(input.Suggestion.ReplaceWith)
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if input.Suggestion == nil && strings.TrimSpace(input.Body) == "" {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "comment body is required")
		return
	}
	if err := blockBoundError(itemblocks.Check("body", input.Body)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A mention is a reference, so who this comment names is in its body (R4): the parties are
	// read beside any the request's own array carries. The grammar alone answers them
	// (text.ExtractAt, the reading text.Mentions itself begins with), so this costs no query:
	// a resolver can only add the issue a bare key names and the ask a `#id` names, and rewrite
	// an ask, comment, message or block id written short, and none of those is a party. A bare
	// key and a short id do change what a resolving read answers - a `#id` would not, the
	// comment having no id yet, so the document its `#id`s resolve against is nothing - but
	// none of them changes who the body names, which is all this read is for.
	bodyParties := text.ExtractAt(input.Body, s.deps.ServerURL)
	delivery, mentionTargets, err := validateCommentMentions(input, bodyParties)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	// Every Envoy listener lookup this comment needs happens here, with no transaction and no
	// pooled connection held (resolveCommentDeliveries). beginCommentWrite re-reads the issue's
	// route under the owner's lock and refuses a resolution the locked row no longer agrees with -
	// and refuses, under that same lock, an owner this pooled read already found missing (or, for
	// a suggestion, closed), which is why a comment the write cannot accept pays for no lookup
	// here.
	isSuggestion := input.Suggestion != nil
	// A suggestion is the one comment write a closed issue refuses (refuseClosed).
	write := takenWhenClosed
	if isSuggestion {
		write = refusedWhenClosed
	}
	if len(mentionTargets) > 0 {
		if err := s.refuseOwnerBeforeResolving(r.Context(), owner, write); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	start, err := resolveThenLock(r.Context(),
		func(ctx context.Context) (commentResolution, error) {
			return s.resolveCommentDeliveries(ctx, owner, input, mentionTargets, delivery)
		},
		func(ctx context.Context, resolved commentResolution) (commentWrite, error) {
			return s.beginCommentWrite(ctx, owner, resolved, write)
		},
		errorf(http.StatusConflict, "ROUTE_CHANGED",
			"the issue's delivery route changed while this comment was being written; post it again"),
	)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, status, resolvedMentions := start.tx, start.status, start.mentions
	// Only a pinned session is stored on a mention, its attempt and comment_mentions; an
	// unanswered mention stays unpinned there, and the sender resolves it after the response.
	mentions := make([]model.Mention, 0, len(resolvedMentions))
	for _, resolved := range resolvedMentions {
		mentions = append(mentions, model.Mention{
			Target: resolved.Target, Delivery: resolved.Delivery, SessionID: resolved.SessionID,
		})
	}
	defer tx.Rollback(r.Context())
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	// The owner lock above decided this comment is written, a closed issue's included, so its
	// anchor's document writes are comment marks (docs.WithCommentMarks); a suggestion's are not,
	// and the document service still refuses them on a closed issue.
	if !isSuggestion {
		documentCtx = docs.WithCommentMarks(documentCtx)
	}
	threadTarget, err := s.normalizeCommentThreadTarget(r.Context(), tx, ledger, actor, owner, input)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Turn != nil {
		if threadTarget.AskID == nil {
			writeError(w, "TURN_REQUIRES_ASK", http.StatusBadRequest, "turn is only valid on a reply to an ask")
			return
		}
		if *input.Turn != "human" && *input.Turn != "agent" {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "turn must be human or agent")
			return
		}
	}
	input.ReplyTo = threadTarget.ReplyTo
	input.AskID = threadTarget.AskID
	replyRoot := threadTarget.ReplyRoot
	turn := threadTarget.replyTurn(actor, input.Turn)
	rowID, err := commentRowID(r.Context(), tx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	markKind := docs.MarkComment
	if input.Suggestion != nil {
		markKind = docs.MarkSuggestion
	}
	// Everything up to Commit is the read phase (Docs.Join): the document operations and this
	// route queue their writes on the ledger, in the order made, and Commit sends them as one
	// batch. Write advice is read here, before the first write is queued, so its queries hold no
	// transaction id, and counts the comment.created this comment appends (writeAdvice).
	var advice *writeAdvice
	if owner.IssueKey != nil && status != nil {
		advice = s.writeAdvice(
			r.Context(), tx, "POST /api/v1/issues/{key}/comments", *owner.IssueKey, actor, adviceAsks{}, *status, "comment.created", false,
		)
	}
	anchor, artifactName, snapshot, err := s.resolveAnchor(documentCtx, tx, owner, input.Anchor, markKind, rowID, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	projection := model.Comment{Anchor: anchor}
	if input.Suggestion != nil {
		projection.Suggestion = &model.Suggestion{ReplaceWith: input.Suggestion.ReplaceWith}
	}
	projectionKind, err := s.commentProjectionKind(documentCtx, projection)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var suggestion *model.Suggestion
	if input.Suggestion != nil {
		suggestion = &model.Suggestion{ReplaceWith: input.Suggestion.ReplaceWith}
	}
	comment := model.Comment{
		ID: rowID, IssueKey: owner.IssueKey, ArtifactID: owner.ArtifactID, Author: actor, Body: input.Body, Anchor: anchor,
		ReplyTo: input.ReplyTo, Answers: threadTarget.Answers, AskID: input.AskID, Turn: turn, Suggestion: suggestion,
		Mentions: mentions, Deliveries: []model.CommentDelivery{}, RepliesToBlock: threadTarget.ReplyBlock.nodeID(),
	}
	stored, err := queueCommentRow(r.Context(), tx, ledger, &comment, projectionKind)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The recovery point in the comment's own transaction, queued right after its insert: a repeat
	// under the key is answered with this comment, its owed mentions queued behind the answer
	// (replayComment), however this request ends; a repeat that took the key over meanwhile
	// refuses it, and the comment rolls back.
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		return queueRequestKeyRecovery(r.Context(), b, commentRecovery{CommentID: rowID})
	}); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	waitingOn := threadTarget.waitingOnAfter(turn)
	if threadTarget.ReplyBlock != nil {
		// The comment's insert is queued, so the read that gives a stored reply its block finds
		// nothing yet: the block read in the read phase is carried here instead.
		issueKey, project, slug, err := s.ownerAddress(r.Context(), tx, owner)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		comment.ReplyBlock = threadTarget.ReplyBlock.replyBlock(issueKey, project, slug)
	}
	if comment.Anchor != nil {
		s.registerCommentAnchor(ledger, comment, actor, false)
		if err := s.deps.Docs.ProjectMark(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, nil, projectionKind), actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var reopenedRoot *model.Comment
	var reopenedArtifactName string
	if replyRoot != nil {
		root := *replyRoot
		// A reply reopens a resolved root, except a decided suggestion's: accepting or rejecting
		// it is final, so the reply joins the thread and leaves who decided it, and when, as they
		// were.
		decided := root.Suggestion != nil && root.Suggestion.Accepted != nil
		if root.Resolved && !decided {
			if err := queueExec(ledger, `
				update comments
				set resolved = false, resolved_by = null, resolved_at = null
				where id = $1
			`, root.ID); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			root.Resolved = false
			root.ResolvedBy = nil
			root.ResolvedAt = nil
			reopenedRoot = &root
			s.registerCommentAnchor(ledger, root, actor, true)
			reopenedArtifactName, err = s.commentArtifactName(r.Context(), tx, root)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
		if root.Anchor != nil {
			replies, err := s.loadReplyChain(r.Context(), tx, everyReader, "reply_to", root.ID)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			// The chain the table holds lacks this reply, whose insert is queued.
			replies = withReply(replies, comment)
			// A decided or orphaned suggestion's mark is gone, so it has no projection kind to
			// carry; the reply still lands and ProjectMark writes only the marks map, as a reopen
			// or a resolve of that suggestion does.
			rootProjectionKind := ""
			kind, kindErr := s.commentProjectionKind(documentCtx, root)
			if kindErr != nil && !errors.Is(kindErr, docs.ErrAnchorMissing) {
				s.writeHandlerError(w, kindErr)
				return
			}
			if kindErr == nil {
				rootProjectionKind = kind
			}
			if err := s.deps.Docs.ProjectMark(documentCtx, root.Anchor.ArtifactID, root.Anchor.MarkID, commentMarkRecord(root, replies, rootProjectionKind), actor); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
	}
	attempts := make([]model.CommentDelivery, len(resolvedMentions))
	claims := make([]time.Time, len(resolvedMentions))
	for index, mention := range resolvedMentions {
		if err := queueExec(ledger, `
			insert into comment_mentions (comment_id, target, delivery, resolved_session_id)
			values ($1, $2, $3, $4)
		`, comment.ID, mention.Target, mention.Delivery, mention.SessionID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		resolveError := (*string)(nil)
		if mention.ResolveError != "" {
			resolveError = &mention.ResolveError
		}
		// The attempt commits with its comment, pending and claimed by this process: the mention
		// sender holds that claim while the send waits on its recipient's lane and writes it again
		// while it waits (refreshHeldClaims), so the delivery sweep never takes a queued send's
		// attempt and sends it past the comments before it. An attempt no sender reaches - one
		// refused once the sender stopped, past the queue's bound, or still queued when the drain
		// ends - has its claim given back and is pending and unclaimed (mentionSender.enqueue,
		// mentionSender.wait); one a kill leaves queued keeps a claim nothing writes again, which
		// lapses a lease later. Either way the delivery sweep has it then (resumeOwedDeliveries).
		//
		// The insert is queued with the transaction's other writes, so the row it returns - the
		// attempt the answer carries and the claim the sender holds - is read by the statement's
		// own callback as the batch runs, into this mention's slot.
		if err := ledger.Queue(func(b *store.WriteBatch) error {
			b.Queue("", `
				insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, resolve_error, state, claimed_at)
				values ($1, $2, 1, $3, $4, $5, 'pending', now())
				returning `+commentDeliveryColumns+`, claimed_at`,
				comment.ID, mention.Target, mention.Delivery, mention.SessionID, resolveError,
			).QueryRow(func(row pgx.Row) error {
				attempt, err := scanCommentDelivery(row, &claims[index])
				attempts[index] = attempt
				return err
			})
			return nil
		}); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	referenceChanges, err := s.queueCommentReferences(r.Context(), tx, ledger, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events := []*model.Event{}
	if snapshot != nil {
		snapshotEvent, err := s.commitArtifactVersionEvent(ledger, owner, actor, anchor.ArtifactID, artifactName, *snapshot, nil)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, snapshotEvent)
	}
	if reopenedRoot != nil {
		payload, err := s.commentEventPayload(r.Context(), tx, reopenedRoot, reopenedArtifactName, commentEventThread{}, model.ReferenceChanges{})
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		event, err := s.queueEvent(ledger, owner.event(
			"comment.reopened",
			actor,
			payload,
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, event)
	}
	event, err := s.queueCommentCreated(
		r.Context(), tx, ledger, owner, &comment, artifactName, threadTarget, waitingOn, referenceChanges, start.commentResolution, stored,
	)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	// The test seam: a case holds the write here, with every event it appended and nothing
	// committed, or ends it in a rollback (server beforeCommentCommit). It is the write phase's
	// last statement, whose result is read once every statement before it has run and before the
	// commit. Nothing but a test sets it.
	if s.beforeCommentCommit != nil {
		if err := ledger.Queue(func(b *store.WriteBatch) error {
			b.Queue("", `select 1`).Exec(func(pgconn.CommandTag) error {
				return s.beforeCommentCommit(r.Context(), appendedEvents(events...))
			})
			return nil
		}); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), appendedEvents(events...)...)
	s.storeDisplayTrees(r.Context(), comment.ID, input.Body)
	// On a context the client's going away does not cancel: a send this sender refuses gives its
	// attempt's claim back, which must not be left undone because the caller hung up.
	//
	// Each send carries the take-over pacing its mention's resolution started
	// (resolveCommentDeliveries), so a comment waits about one listener take-over window in all,
	// here as it did when the request itself sent: a resolution that already waited that window
	// out settles with its outcome on the lane's first try. A send that waits on a held lane
	// first starts a window of its own at its delivery (mentionSend.takeover).
	sending := context.WithoutCancel(r.Context())
	for i, mention := range resolvedMentions {
		s.mentions.enqueue(sending, mentionSend{
			comment: comment, created: *event, mention: mention, actor: actor, attempt: 1,
			claimed: claims[i], takeover: start.takeovers[i],
		})
	}
	// The answer carries each attempt as it committed, pending: the mention sender settles it
	// after this response, and its comment.delivery receipt says how it ended.
	comment.Deliveries = attempts
	WriteJSON(w, http.StatusCreated, withAdvice(commentWriteResponse{shapedComment: newShapedComment(comment, wantsDisplayTrees(r)), AskWaitingOn: waitingOn}, advice))
}

// issueComment is a comment that a write in this process leaves on an issue, in the write's own
// transaction (issueWriteCall.comment): its author, and its words from the issue as the write
// reads it under its owner lock. It mentions nobody, replies to nothing and has no anchor, so it
// asks the Envoy listener nothing and its write needs no lookup before its transaction.
type issueComment struct {
	author model.Actor
	words  func(before model.Issue) string
}

// queueIssueComment queues a comment by author with body on the issue key in the transaction and
// on the ledger of the write that leaves it, which holds the issue's owner lock: its node and its
// row (queueCommentRow), its references, its comment.created event and what it states under that
// event (queueCommentCreated), in the order that the comment route queues them. The event is the
// author's, so a system author wakes nobody (events.Broker.Notify).
//
// The comment has no anchor and no artifact owns it, so it names no artifact's address
// (commentStored) and is never left out of the store: its node and its two base attributes are
// written as every comment's are. An author graphstore.AuthorOf names nobody of, as Dispatch's
// system actors are, writes no authored_by and no author's node (graphstore.QueueAuthored), which
// is what the store census reads off such a row (graphcensus's commentAuthor). It has no
// suggestion, so its row stores no suggestion and no kind (queueCommentRow).
//
// The body takes the comment route's own checks (createCommentFor): its line endings become line
// feeds (pmdoc.LineFeeds), then the block bounds and the refusal of a decision block hold it
// (itemblocks.Check), answered as that route answers them (blockBoundError), so a body they refuse
// is refused as the route refuses it and the write that leaves it rolls back. A blank body is a
// fault of the caller's own, and is refused as one. It answers the comment's event and its body as
// the row holds it.
func (s *server) queueIssueComment(
	ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, key string, author model.Actor, body string,
) (*model.Event, string, error) {
	body = pmdoc.LineFeeds(body)
	if strings.TrimSpace(body) == "" {
		return nil, "", fmt.Errorf("the comment that %s leaves on %s has no body", author.ID, key)
	}
	if err := blockBoundError(itemblocks.Check("body", body)); err != nil {
		return nil, "", err
	}
	id, err := commentRowID(ctx, tx)
	if err != nil {
		return nil, "", err
	}
	owner := issueOwner(key)
	comment := model.Comment{
		ID: id, IssueKey: owner.IssueKey, Author: author, Body: body,
		Mentions: []model.Mention{}, Deliveries: []model.CommentDelivery{},
	}
	stored, err := queueCommentRow(ctx, tx, ledger, &comment, "")
	if err != nil {
		return nil, "", err
	}
	references, err := s.queueCommentReferences(ctx, tx, ledger, comment)
	if err != nil {
		return nil, "", err
	}
	created, err := s.queueCommentCreated(ctx, tx, ledger, owner, &comment, "", commentThreadTarget{}, "", references, commentResolution{}, stored)
	return created, body, err
}

// commentRowID is the id of a new comment, read before its row is queued.
func commentRowID(ctx context.Context, tx pgx.Tx) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&id)
	return id, err
}

// queueCommentRow queues on ledger the comment's node and then the insert of comment's row, which
// names that node, and writes the row's created_at into comment. It answers what the comment is in
// the node and tuple store (commentStored), whose facts wait for the comment's event
// (queueCommentCreated). Its caller holds the owner lock that every comment insert takes first:
// the comment route's (lockCommentOwner), or that of the issue write that leaves the comment
// (writeIssue, lockIssueOwners).
//
// The row's suggestion column holds comment's suggestion with suggestionKind, the kind its mark
// recorded (storedSuggestion), which the delete reads (loadStoredSuggestion). Every caller states
// the kind: the comment route the kind its mark projection read (commentProjectionKind), and a
// caller whose comment has no suggestion or no anchor "". A comment with no suggestion stores none.
//
// The row's created_at is the clock at the insert (migration 0065's default), which orders an
// owner's comments as they commit because every comment insert holds the owner's lock. The insert
// is queued, so the clock is read here, at the insert's place and under that lock. That still
// orders it after every comment committed before this transaction took the lock and before every
// comment committed after it. It is written explicitly, so what the caller makes of the comment
// before the insert runs, its event and its mark record, carries the time the row will hold. The
// row, its node and the facts it projects carry that one time: the store's census digests the
// row's created_at against its node's made_at, and graph_edges answers a comment's edges from the
// row before its kind's cutover and from the tuples after it.
func queueCommentRow(
	ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, comment *model.Comment, suggestionKind string,
) (commentStoreWrite, error) {
	author, err := encodeJSON(comment.Author)
	if err != nil {
		return commentStoreWrite{}, err
	}
	var anchor any
	if comment.Anchor != nil {
		if anchor, err = encodeJSON(comment.Anchor); err != nil {
			return commentStoreWrite{}, err
		}
	}
	var suggestion any
	if comment.Suggestion != nil {
		if suggestion, err = encodeJSON(storedSuggestion{Suggestion: *comment.Suggestion, Kind: suggestionKind}); err != nil {
			return commentStoreWrite{}, err
		}
	}
	if err := tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&comment.CreatedAt); err != nil {
		return commentStoreWrite{}, err
	}
	// What this comment is in the node and tuple store: its node, queued before the row that names
	// it, and the facts the row is the projection of, which wait for the event that stores it. The
	// node write appends nothing (graphstore.QueueNodes): every fact of this write, the two base
	// attributes it states among them, is appended under that event, which is appended after the
	// row. A comment left out of the store (unprojected) has no node to write.
	project, parties, err := graphstore.ScopeOf(ctx, tx, comment.IssueKey, comment.ArtifactID)
	if err != nil {
		return commentStoreWrite{}, err
	}
	stored, err := commentStored(ctx, tx, comment.ID, comment.Author, comment.CreatedAt, comment.Anchor, comment.ArtifactID, project, parties)
	if err != nil {
		return commentStoreWrite{}, err
	}
	if !stored.unprojected {
		if err := ledger.Queue(func(b *store.WriteBatch) error {
			graphstore.QueueNodes(b, stored.written)
			return nil
		}); err != nil {
			return commentStoreWrite{}, err
		}
	}
	nodeKind, nodeAddress := stored.rowNode()
	if err := queueExec(ledger, `
		insert into comments (id, issue_key, artifact_id, author, body, anchor, reply_to, ask_id, turn, suggestion, replies_to_block, answers, node_kind, node_address, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`, comment.ID, comment.IssueKey, comment.ArtifactID, author, comment.Body, anchor, comment.ReplyTo, comment.AskID, comment.Turn,
		suggestion, comment.RepliesToBlock, comment.Answers, nodeKind, nodeAddress, comment.CreatedAt); err != nil {
		return commentStoreWrite{}, err
	}
	return stored, nil
}

// queueCommentReferences queues the index of comment's blocks and the reconciliation of its
// references (queueItemReferences), after its row (queueCommentRow), and answers what they move.
// The row is queued, so the fields that row will hold are handed over. They name the document
// that its `#id`s resolve against, which no read of the row can answer yet. Its short ids resolve
// for the comment's author, who writes it.
func (s *server) queueCommentReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, comment model.Comment) (model.ReferenceChanges, error) {
	pending := refs.PendingSource{IssueKey: comment.IssueKey, ArtifactID: comment.ArtifactID, ReplyTo: comment.ReplyTo, AskID: comment.AskID}
	if comment.Anchor != nil {
		pending.AnchorArtifactID = &comment.Anchor.ArtifactID
	}
	return s.queueItemReferences(ctx, tx, ledger, comment.Author, refs.ItemComment, comment.ID, comment.Body, pending, comment.CreatedAt, true)
}

// queueCommentCreated queues the comment.created event of comment on owner, after the comment's
// row and its references, and right after that event what the comment states under it (stored,
// which queueCommentRow answered): the comment as its write leaves it, in thread, with waitingOn
// (commentThreadTarget.waitingOnAfter), the references it moved, and what the listener's answers
// (resolved) decide about the comment's other routes (routeSuppression, mentions_decide_routes).
// The author of the comment is the event's actor.
func (s *server) queueCommentCreated(
	ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, owner owner, comment *model.Comment, artifactName string,
	thread commentThreadTarget, waitingOn string, references model.ReferenceChanges, resolved commentResolution,
	stored commentStoreWrite,
) (*model.Event, error) {
	payload, err := s.commentEventPayload(ctx, tx, comment, artifactName, thread.eventThread(waitingOn), references)
	if err != nil {
		return nil, err
	}
	payload.SuppressRoute, payload.SuppressedRoute, payload.SuppressedRouteSessionID = resolved.routeSuppression()
	payload.MentionsDecideRoutes = resolved.decidesRoutes
	if comment.AskID != nil {
		if payload.AskSessions, err = askSessions(ctx, tx, *comment.AskID, thread.AskAuthorSession, comment.Author); err != nil {
			return nil, err
		}
	}
	// The comment's thread_logins are read by its event's insert, after the comment's own insert
	// and the user_threads trigger it fires.
	event, err := s.queueLoggedEvent(ledger, owner.event("comment.created", comment.Author, payload),
		events.ThreadLoginsOf(commentThread(*comment)), "comment", comment.ID)
	if err != nil {
		return nil, err
	}
	// What the comment states, under the event that stored it, queued right after that event
	// (graphstore.LatestEvent): its node's cause, its two base attributes, and the artifacts it
	// is anchored to and owned by, with the move of its node row onto that event
	// (graphstore.QueueCaused). Then who wrote it, as the node and tuple store holds it: the
	// author's node and the comment's authored_by tuple, under the same event. A comment left out
	// of the store has no node to author, so this is skipped with the rest of its write. Then a
	// session writing in an ask's thread follows the ask, and is a node of the store from this
	// write on, caused by the same comment.created. It comes after the author's own write, which
	// takes the author's person node and their session node in one statement in ascending (kind,
	// id): a follow before it would take the session first and leave the person to that
	// statement, which is the pair in the other order, and two such writes racing over a session
	// and an owner the store holds neither of would deadlock (R17).
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		if !stored.unprojected {
			graphstore.QueueCaused(b, stored.written, graphstore.LatestEvent)
			if err := graphstore.QueueAuthored(b, graphstore.KindComment, comment.ID, comment.Author, comment.CreatedAt, graphstore.LatestEvent); err != nil {
				return err
			}
		}
		if comment.AskID != nil {
			if _, err := asks.QueueFollowAuthor(b, *comment.AskID, comment.Author, comment.CreatedAt, graphstore.LatestEvent); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return event, nil
}

// commentStoreWrite is what a comment is in the node and tuple store: its node, and the facts
// its row is the projection of (graphstore.CommentFacts).
//
// The node is written by the request itself (graphstore.QueueNodes), before the row, because the
// composite foreign key on (node_kind, node_address) is checked inside the statement that writes
// the row and there is no node to name otherwise. The facts are not: on the request path no event
// exists while the row is written, so they wait for the event that caused the write
// (graphstore.QueueCaused), queued right after it in the same write phase, where refs.QueueStamp
// already runs. The node's two base attributes wait with them: the write holds them for that
// event rather than letting the node write state them, so that they carry the event that stored
// the comment like every other fact of this write. The block index states its own nodes the same
// way (graphstore.Uncaused). Each writer of a comment skips both for a comment left out of the
// store (unprojected).
type commentStoreWrite struct {
	// written is the node with every fact of this write held for the event that stores the
	// comment (graphstore.PlanUncaused).
	written graphstore.Uncaused
	// unprojected is a comment any of whose relation ends is past the store's address bound: it
	// has no node and no tuples, and its row's node columns stay null, so the old comments arm of
	// graph_edges answers every one of its edges rather than the tuple arm answering it without
	// the edge the bound would drop (R22).
	unprojected bool
}

// commentStored is what the comment rowID states (graphstore.CommentFacts): its node,
// project-visible and live, in project or between parties (graphstore.ScopeOf of what it belongs
// to), made by its author at the row's own created_at, and the artifact it is anchored to and the
// artifact that owns it, each stated only where the comment has one. Both relations name the
// artifact by its address, which is its ref_key, so the two artifacts' addresses are read here, in
// the caller's read phase, in one statement.
func commentStored(
	ctx context.Context,
	tx pgx.Tx,
	rowID string,
	author model.Actor,
	at time.Time,
	anchor *model.Anchor,
	ownerArtifactID *string,
	project string,
	parties []string,
) (commentStoreWrite, error) {
	var artifacts []string
	if anchor != nil {
		artifacts = append(artifacts, anchor.ArtifactID)
	}
	if ownerArtifactID != nil {
		artifacts = append(artifacts, *ownerArtifactID)
	}
	addresses, err := artifactAddresses(ctx, tx, artifacts)
	if err != nil {
		return commentStoreWrite{}, err
	}
	anchorAddress, ownerAddress := "", ""
	if anchor != nil {
		anchorAddress = addresses[anchor.ArtifactID]
	}
	if ownerArtifactID != nil {
		ownerAddress = addresses[*ownerArtifactID]
	}
	// A comment any of whose relation ends is past the store's address bound is left whole out of
	// the store. An artifact's ref_key is bounded only by its own key's index (api/artifacts.go)
	// and can be longer than a tuple key holds beside the comment's address, so a node with one of
	// its two edges and the other dropped would be a comment the tuple arm answers short an edge
	// the old arm gave. Instead no node, no tuple and no stamp are written, and the old comments
	// arm answers both of its edges (graph_edges). The census counts it in the past-bound class.
	if (anchorAddress != "" && !graphstore.AddressFits(anchorAddress)) ||
		(ownerAddress != "" && !graphstore.AddressFits(ownerAddress)) {
		return commentStoreWrite{unprojected: true}, nil
	}
	node, tuples := graphstore.CommentFacts(nil, rowID, author, at, graphstore.NoEvent, anchorAddress, ownerAddress, project, parties)
	return commentStoreWrite{written: graphstore.PlanUncaused([]graphstore.Node{node}, tuples)}, nil
}

// rowNode is the node columns a comment's row names: the comment kind and the comment's own id,
// or null and null for a comment left out of the store (unprojected), whose row the old comments
// arm answers.
func (w commentStoreWrite) rowNode() (nodeKind, nodeAddress *string) {
	if w.unprojected {
		return nil, nil
	}
	node := w.written.Nodes[0]
	return &node.Kind, &node.ID
}

// artifactAddresses are the addresses the graph names the given artifacts by, their ref_keys,
// read in one statement: a comment anchored to the document it belongs to names one artifact
// twice, and an id no row answers is absent from the answer.
func artifactAddresses(ctx context.Context, q queryer, ids []string) (map[string]string, error) {
	addresses := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return addresses, nil
	}
	rows, err := q.Query(ctx, `select id::text, ref_key from artifacts where id = any($1::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the addresses of %d artifact: %w", len(ids), err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, refKey string
		if err := rows.Scan(&id, &refKey); err != nil {
			return nil, fmt.Errorf("read an artifact's address: %w", err)
		}
		addresses[id] = refKey
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the addresses of %d artifact: %w", len(ids), err)
	}
	return addresses, nil
}

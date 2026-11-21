package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// reviewer.go is the reviewer of INFRA-3231's R5 on the server's write path. An ask, a new issue,
// a comment and a message an agent writes to a person go through reviewWrite before their write
// opens a transaction. By the kind's setting (DISPATCH_REVIEWER_<KIND>) the reviewer holds the
// text (hold: it is stored in held_texts alone, and the write answers at once with the id the
// hold reserved), reviews it after it posts (after), or leaves it to the writing check (off). The
// review runs in the background, never inside the write's transaction or its request: a clean
// review posts the held text, a review with findings sends them to the writer session as a turn
// of its own (sendFindingsFrame) and waits for the writer for DISPATCH_REVIEWER_WRITER_WINDOW, and
// a model that fails or runs past DISPATCH_REVIEWER_TIMEOUT posts the text as written. The post
// runs the kind's own create path (writeAsk, writeIssue, writeComment, writeMessage) with the
// stored request, the recorded actor and the reserved id (held_texts.go). Every decision is one
// row of writing_check_events.

// The reviewer's choices for one kind of text (DISPATCH_REVIEWER_<KIND>).
const (
	// ReviewOff reads no text of the kind: the writing check alone reads it.
	ReviewOff = "off"
	// ReviewAfter posts the text at once and reviews it after the post, sending the findings to
	// the writer session.
	ReviewAfter = "after"
	// ReviewHold keeps the text from every person until the review ends and the writer acts.
	ReviewHold = "hold"
)

// What posts when the writer does not act on a held text in time (DISPATCH_REVIEWER_UNEDITED).
const (
	// UneditedRewrite posts the reviewer's rewrite, where it has one that keeps what a rewrite
	// keeps; else the text as written.
	UneditedRewrite = "rewrite"
	// UneditedAsWritten posts the text as the writer wrote it.
	UneditedAsWritten = "as_written"
)

// The outcomes the reviewer records in writing_check_events (store migration
// 20261119050002_writing_check_events_reader admits each).
const (
	// reviewHeld is a text the reviewer held.
	reviewHeld = "held"
	// reviewPassed is a held text whose review found nothing, posted.
	reviewPassed = "passed"
	// reviewEdited is the writer's edit of a held text (PATCH /api/v1/held/{id}).
	reviewEdited = "edited"
	// reviewPostedAsWritten is a held text posted as written over the reviewer's findings: by
	// the writer with a reason, or because the writer did not act and the setting says so.
	reviewPostedAsWritten = "posted_as_written"
	// reviewRewritePosted is a held text whose reviewer's rewrite posted in its place.
	reviewRewritePosted = "rewrite_posted"
	// reviewPostedUnreviewed is a text posted with no review: the model failed or ran past its
	// time bound.
	reviewPostedUnreviewed = "posted_unreviewed"
	// reviewReviewedAfter is a text reviewed after its post (DISPATCH_REVIEWER_<KIND>=after); its
	// rules are the rule sets the review found broken, none for a clean review.
	reviewReviewedAfter = "reviewed_after"
)

// reviewerActor is the actor the writer's frame comes from: Dispatch itself.
var reviewerActor = model.Actor{Kind: "system", ID: "dispatch-reviewer"}

// ReviewerConfig is the reviewer as cmd/dispatch configured it from DISPATCH_REVIEWER_*, which
// sets every field: the defaults of the settings it leaves unset are cmd/dispatch's, from the
// constants below, reviewer.DefaultRules and reviewer.DefaultTimeout.
type ReviewerConfig struct {
	// Asks, Issues, Comments and Messages are each kind's choice: ReviewOff, ReviewAfter or
	// ReviewHold. The choice also says whether an agent's edit of a posted text of the kind is
	// reviewed after the save (Decision 8): any choice but ReviewOff.
	Asks, Issues, Comments, Messages string
	// Documents is the choice for a document version an agent saves (DISPATCH_REVIEWER_DOCUMENTS):
	// ReviewOff or ReviewAfter; a version is never held.
	Documents string
	// DecisionBlocks is the choice for a decision block an agent writes in a document
	// (DISPATCH_REVIEWER_DECISION_BLOCKS): ReviewOff, ReviewAfter, or ReviewHold, which shows the
	// block in its document and keeps it out of every person's Inbox, counts and notices until
	// the review ends (decision_blocks.go).
	DecisionBlocks string
	// Rules are the rule sets the reviewer applies (DISPATCH_REVIEWER_RULES).
	Rules []reviewer.RuleSet
	// Model is the reviewer's model group, with its pacer's share and its calls' time bound; the
	// draft route calls it whatever the four choices say.
	Model reviewer.Model
	// Timeout bounds a whole review, the record's read and every model call
	// (DISPATCH_REVIEWER_TIMEOUT).
	Timeout time.Duration
	// WriterWindow is how long the writer session has to act on a held text's findings
	// (DISPATCH_REVIEWER_WRITER_WINDOW).
	WriterWindow time.Duration
	// Unedited is what posts when the writer does not act: UneditedRewrite or
	// UneditedAsWritten.
	Unedited string
}

// Reviewer is the reviewer one server runs; a nil *Reviewer is no reviewer model configured,
// which reviews nothing and answers the draft route 503.
type Reviewer struct {
	config ReviewerConfig
}

// DefaultWriterWindow is DISPATCH_REVIEWER_WRITER_WINDOW unset.
const DefaultWriterWindow = 5 * time.Minute

// NewReviewer is the reviewer cfg describes, every field of which its caller set.
func NewReviewer(cfg ReviewerConfig) *Reviewer {
	return &Reviewer{config: cfg}
}

// setting is the reviewer's choice for a text of kind; ReviewOff for no reviewer.
func (rv *Reviewer) setting(kind reviewer.Kind) string {
	if rv == nil {
		return ReviewOff
	}
	switch kind {
	case reviewer.KindAsk:
		return rv.config.Asks
	case reviewer.KindIssue:
		return rv.config.Issues
	case reviewer.KindComment:
		return rv.config.Comments
	case reviewer.KindMessage:
		return rv.config.Messages
	case reviewer.KindDocument:
		return rv.config.Documents
	case reviewer.KindDecisionBlock:
		return rv.config.DecisionBlocks
	}
	return ReviewOff
}

// reviewsAny reports whether the reviewer reads any kind of text, before or after its post.
func (rv *Reviewer) reviewsAny() bool {
	return rv != nil && slices.ContainsFunc(
		[]string{rv.config.Asks, rv.config.Issues, rv.config.Comments, rv.config.Messages, rv.config.Documents, rv.config.DecisionBlocks},
		func(choice string) bool { return choice != ReviewOff })
}

// staleClaim is how long a review or a post may hold a held text before the sweep takes it again:
// the review's own bound and a minute more, so a review a stopped process left is reviewed again
// and one still running is not.
func (rv *Reviewer) staleClaim() time.Duration {
	if rv == nil {
		return reviewer.DefaultTimeout + time.Minute
	}
	return rv.config.Timeout + time.Minute
}

// reviewedWrite is one write of a kind the reviewer reads, decoded: what reviewWrite needs to
// hold it, review it and answer for it. Each create path builds it from its decoded request
// (askCreate.reviewed, issueCreate.reviewed, commentCreate.reviewed, messageCreate.reviewed).
type reviewedWrite struct {
	kind reviewer.Kind
	// request is the decoded write: what a held text stores and its post runs.
	request any
	// text is the wording the reviewer reads.
	text reviewer.Text
	// subject places the text for its record (reviewer.BuildRecord); its Text, Writer, Reader and
	// At are set where the review runs.
	subject reviewer.Subject
	// project is the project the write names in itself, "" where it names an issue or a document
	// whose project is read.
	project string
	// names are the ids the write names that a held text may hold: the issue it is on or a new
	// issue's parent, and the comment, ask or message it answers.
	names []string
	// holdable is whether the reviewer may hold the write: every one but an issue imported from
	// GitHub, whose title is GitHub's.
	holdable bool
}

// postedReview is a review the write's handler starts once its write committed
// (DISPATCH_REVIEWER_<KIND>=after): reviewWrite decided it, reviewPosted runs it.
type postedReview struct {
	kind    reviewer.Kind
	text    reviewer.Text
	subject reviewer.Subject
	event   store.WritingCheckEvent
}

// reviewWrite is the reviewer's hook on a write of a kind it may hold. It reads the write when an
// agent session makes it (writingCheckReads; the Legion daemon's session is left out), no plugin
// generated it (generatedText), no person typed it in a session (personTypedWrite) and a person
// reads it (writingCheckReader), by the kind's setting: hold stores the write as a held text,
// answers the write's own response shape built from the held row with an added `held` object,
// starts the review in the background and answers false; after answers true with the review the
// handler starts once its write committed (reviewPosted); off leaves the write to the writing
// check (writingCheck). A person's typed write posts at once, recorded exempt_person_typed. A
// session's write that names a held text as its parent or as the text it answers is held beside
// it whatever the setting says, and posts right after it, through its own review where the
// setting is hold and no person typed it. A person signed in at a browser learns nothing of a held
// text: their write is the route's own, which finds no such text, as awaitHeldTarget answers
// their other writes. Call it as writingCheck is called: once the input is decoded and the actor
// resolved, before any transaction.
func (s *server) reviewWrite(w http.ResponseWriter, r *http.Request, actor model.Actor, input any, write reviewedWrite) (*postedReview, bool) {
	rv := s.deps.Reviewer
	if !rv.reviewsAny() || s.deps.Store == nil {
		s.writingCheck(r, actor, input)
		return nil, true
	}
	ctx := r.Context()
	var parent *heldParent
	_, human, err := s.optionalActor(unloggedResolution(r))
	if err == nil && !human {
		if parent, err = s.heldParentOf(ctx, write.names); err != nil {
			s.writeHandlerError(w, err)
			return nil, false
		}
	}
	method, route := routeOf(r)
	typed := personTypedWrite(r)
	if typed && parent == nil {
		// A person's own typed write posts at once, whatever the kind's setting (Decision 26).
		s.recordPersonTyped(r, actor, input, cmp.Or(write.project, s.projectOfText(ctx, write.subject)))
		return nil, true
	}
	setting := rv.setting(write.kind)
	reader, project := "", write.project
	reviews := false
	if checked, _ := writingCheckReads(actor); checked && !typed && setting != ReviewOff && !generatedText(r, method+" "+route) {
		if parent != nil {
			reader = parent.reader
		} else if reader, err = s.writingCheckReader(ctx, r, method+" "+route, actor, input); err != nil {
			slog.Warn("dispatch: decide the reader of a reviewed write", "route", method+" "+route, "error", err)
			reader = ""
		}
		reviews = reader != ""
	}
	if parent == nil && !reviews {
		s.writingCheck(r, actor, input)
		return nil, true
	}
	if parent != nil && project == "" {
		project = parent.project
	}
	if project == "" {
		project = s.projectOfText(ctx, write.subject)
	}
	event := store.WritingCheckEvent{
		Route: route, Method: method, ActorKind: actor.Kind, ActorID: actor.ID,
		TokenID: s.writingCheckTokenID(r, actor), Reader: reader, Project: project,
	}
	if actor.Act != nil {
		event.ActSession = actor.Act.Session
	}
	holds := reviews && setting == ReviewHold && write.holdable
	if parent == nil && !holds {
		subject := write.subject
		subject.Text, subject.Writer, subject.Reader = write.text, writerOf(actor), reader
		return &postedReview{kind: write.kind, text: write.text, subject: subject, event: event}, true
	}
	if !holds {
		// A write that waits on a held text and that the reviewer does not read before it posts
		// (a person's typed write among them): the writing check reads it now, as it reads any
		// write, and it posts right after the text it waits on.
		s.writingCheck(r, actor, input)
	}
	s.holdWrite(w, r, actor, write, heldWriteOf{
		route: method + " " + route, reader: reader, project: project, parent: parent, reviewed: holds, event: event,
		personTyped: typed,
	})
	return nil, false
}

// routeOf is the method and the pattern of the route r was served on.
func routeOf(r *http.Request) (method, route string) {
	method, route, found := strings.Cut(r.Pattern, " ")
	if !found {
		method, route = r.Method, r.URL.Path
	}
	return method, route
}

// writerOf is the session that wrote as actor: the session itself, or the one a person's
// credential acted for; "" for a person's own write and for the system.
func writerOf(actor model.Actor) string {
	if actor.Act != nil && actor.Act.Session != "" {
		return actor.Act.Session
	}
	if actor.Kind == "session" {
		return actor.ID
	}
	return ""
}

// projectOfText is the project of the issue or the document subject stands in, "" where it names
// neither or the read fails.
func (s *server) projectOfText(ctx context.Context, subject reviewer.Subject) string {
	var project string
	var err error
	switch {
	case subject.IssueKey != "":
		err = s.deps.Store.Pool.QueryRow(ctx, `select project_key from issues where key = $1`, subject.IssueKey).Scan(&project)
	case subject.DocumentID != "":
		if _, parseErr := uuid.Parse(subject.DocumentID); parseErr != nil {
			return ""
		}
		err = s.deps.Store.Pool.QueryRow(ctx, `
			select coalesce(i.project_key, ar.project_key, '')
			from artifacts ar left join issues i on i.key = ar.issue_key
			where ar.id = $1`, subject.DocumentID).Scan(&project)
	default:
		return ""
	}
	if err != nil {
		return ""
	}
	return project
}

// reviewPosted starts review, the review of a text the write posted under id, in the background:
// a nil review, which every write the reviewer does not read after its post gets, starts nothing.
// Its row records the id of the write ctx is the request of (writeIDOf), which the writing check's
// row of that write records too, so the counts count the text once; a review whose row names its
// write already keeps it.
func (s *server) reviewPosted(ctx context.Context, review *postedReview, id string) {
	if review == nil {
		return
	}
	review.subject.ID = id
	review.event.WriteID = cmp.Or(review.event.WriteID, writeIDOf(ctx))
	s.detached.goDetached(func() { s.runPostedReview(s.lifetime, *review, id) })
}

// runPostedReview reviews a posted text and records the review: reviewed_after with the rule sets
// it found broken, or posted_unreviewed where the model failed. Findings go to the writer session
// as a frame that holds nothing, which a writer that does not act leaves as it is.
func (s *server) runPostedReview(ctx context.Context, review postedReview, id string) {
	rv := s.deps.Reviewer
	reviewed, err := s.review(ctx, rv.config.Model, review.subject, true)
	if ctx.Err() != nil {
		return
	}
	event := review.event
	if err != nil {
		event.Outcome, event.Detail = reviewPostedUnreviewed, "the review after the post failed: "+err.Error()
		s.recordWritingCheck(ctx, event)
		return
	}
	event.Outcome, event.Rules = reviewReviewedAfter, findingRuleSets(reviewed.Findings)
	s.recordWritingCheck(ctx, event)
	if len(reviewed.Findings) == 0 {
		return
	}
	if s.writerGone(ctx, review.subject.Writer) {
		return
	}
	frame := reviewer.FindingsFrame{
		ID: id, Kind: review.kind, Findings: reviewed.Findings, Rewrite: reviewed.Rewrite,
		NotApplied: reviewed.NotApplied, PostsRewrite: rv.config.Unedited == UneditedRewrite,
	}
	if err := s.sendFindingsFrame(ctx, frame, review.subject.Writer, "posted:"+id); err != nil {
		slog.Warn("dispatch: send the reviewer's findings to the writer", "text", id, "error", err)
	}
}

// review reads the record of subject's text and reviews it with chosen, the reviewer's model group
// or the test model a draft's request names (requestModel), the whole bounded by
// DISPATCH_REVIEWER_TIMEOUT. readerWords is whether the record holds the words the reader asked
// about before: false for a draft a person at a browser asks for another person, who reads none
// of that person's words (readsWordsOf). A record it could not read is an error as a model's
// failure is.
func (s *server) review(ctx context.Context, chosen reviewer.Model, subject reviewer.Subject, readerWords bool) (reviewer.Reviewed, error) {
	rv := s.deps.Reviewer
	bounded, cancel := context.WithTimeout(ctx, rv.config.Timeout)
	defer cancel()
	record, err := reviewer.BuildRecord(bounded, s.deps.Store, subject)
	if err != nil {
		return reviewer.Reviewed{}, err
	}
	if !readerWords {
		record.ReaderWords = nil
	}
	reviewed, err := reviewer.Review(bounded, chosen, rv.config.Rules, record)
	if err == nil && bounded.Err() != nil {
		err = bounded.Err()
	}
	return reviewed, err
}

// findingRuleSets is the rule sets findings name, each once, in the order they first appear.
func findingRuleSets(findings []reviewer.Finding) []string {
	sets := []string{}
	for _, finding := range findings {
		if !slices.Contains(sets, string(finding.RuleSet)) {
			sets = append(sets, string(finding.RuleSet))
		}
	}
	return sets
}

// startReview reviews the held text id in the background, on the process's lifetime: a review
// that shutdown cuts off leaves the text in review, and the sweep reviews it again.
func (s *server) startReview(id string) {
	s.detached.goDetached(func() { s.reviewHeld(s.lifetime, id) })
}

// reviewHeld reviews the held text id when it is in review and no review holds it, and acts on
// what the review answers: a clean review posts it, findings go to the writer
// (heldFindings), and a model that failed or ran past its bound posts it as written, counted
// posted_unreviewed. A review of a wording the writer has edited since is dropped: the edit
// started a review of its own.
func (s *server) reviewHeld(ctx context.Context, id string) {
	row, claimed, err := s.claimHeldReview(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: take a held text for its review", "text", id, "error", err)
		}
		return
	}
	if !claimed {
		return
	}
	write, err := row.write()
	if err != nil {
		slog.Warn("dispatch: read a held text's request", "text", id, "error", err)
		s.decideHeld(ctx, row, heldDecision{outcome: reviewPostedUnreviewed, detail: "its request could not be read: " + err.Error()})
		return
	}
	subject := write.subject
	subject.Text, subject.Writer, subject.Reader, subject.At = write.text, row.writer, row.reader, row.writtenAt
	if write.kind == reviewer.KindDecisionBlock {
		// A held decision block is in its document already, and its writer may have reworded
		// it since the hold: the review reads the ask as it now stands.
		if subject, err = s.decisionBlockSubject(ctx, row); err != nil {
			s.decideHeld(ctx, row, heldDecision{outcome: reviewPostedUnreviewed, detail: "its ask could not be read: " + err.Error()})
			return
		}
	}
	reviewed, err := s.review(ctx, s.deps.Reviewer.config.Model, subject, true)
	if ctx.Err() != nil {
		return
	}
	switch {
	case err != nil:
		s.decideHeld(ctx, row, heldDecision{outcome: reviewPostedUnreviewed, detail: "the review failed: " + err.Error()})
	case len(reviewed.Findings) == 0:
		s.decideHeld(ctx, row, heldDecision{outcome: reviewPassed})
	default:
		s.heldFindings(ctx, row, reviewed)
	}
}

// heldFindings acts on a review of a held text that found problems. The findings and the rewrite
// are kept with the text. A writer session that has gone gets no time: the text posts at once as
// DISPATCH_REVIEWER_UNEDITED says. Any other writer gets the findings as a frame and
// DISPATCH_REVIEWER_WRITER_WINDOW to act, after which the sweep posts it the same way.
func (s *server) heldFindings(ctx context.Context, row heldRow, reviewed reviewer.Reviewed) {
	rv := s.deps.Reviewer
	findings, err := json.Marshal(reviewed.Findings)
	if err != nil {
		slog.Warn("dispatch: keep a held text's findings", "text", row.id, "error", err)
		return
	}
	var rewrite []byte
	if reviewed.Rewrite != nil {
		if rewrite, err = json.Marshal(reviewed.Rewrite); err != nil {
			slog.Warn("dispatch: keep a held text's rewrite", "text", row.id, "error", err)
			return
		}
	}
	notApplied := ""
	if reviewed.NotApplied != nil {
		notApplied = reviewed.NotApplied.Reason
	}
	row.findings, row.rewrite, row.notApplied = reviewed.Findings, reviewed.Rewrite, notApplied
	if s.writerGone(ctx, row.writer) {
		if _, err := s.deps.Store.Pool.Exec(ctx, `
			update held_texts set findings = $3, rewrite = $4, not_applied = nullif($5, ''), updated_at = now()
			where id = $1 and revision = $2 and state = 'reviewing'`, row.id, row.revision, findings, rewrite, notApplied); err != nil {
			slog.Warn("dispatch: keep a held text's findings", "text", row.id, "error", err)
			return
		}
		s.actUnedited(ctx, row, "the writer session is not running")
		return
	}
	var deadline time.Time
	err = s.deps.Store.Pool.QueryRow(ctx, `
		update held_texts
		set state = 'awaiting_writer', findings = $3, rewrite = $4, not_applied = nullif($5, ''),
			deadline = now() + $6::interval, claimed_at = null, updated_at = now()
		where id = $1 and revision = $2 and state = 'reviewing'
		returning deadline`,
		row.id, row.revision, findings, rewrite, notApplied, intervalOf(rv.config.WriterWindow)).Scan(&deadline)
	if err != nil {
		// An edit or a post overtook the review: what the writer did stands.
		return
	}
	frame := reviewer.FindingsFrame{
		ID: row.id, Kind: reviewer.Kind(row.kind), Findings: reviewed.Findings, Rewrite: reviewed.Rewrite,
		NotApplied: reviewed.NotApplied, Held: true, TimeLeft: time.Until(deadline),
		PostsRewrite: rv.config.Unedited == UneditedRewrite,
	}
	if err := s.sendFindingsFrame(ctx, frame, row.writer, "held:"+row.id+":"+strconv.Itoa(row.revision)); err != nil {
		slog.Warn("dispatch: send a held text's findings to its writer", "text", row.id, "error", err)
	}
}

// intervalOf is d as a Postgres interval.
func intervalOf(d time.Duration) string {
	return fmt.Sprintf("%d milliseconds", d.Milliseconds())
}

// actUnedited posts a held text whose writer did not act, as DISPATCH_REVIEWER_UNEDITED says: the
// reviewer's rewrite, marked as rewritten, where the setting asks for it and the review wrote one
// that keeps what a rewrite keeps, else the text as written. why says why the writer did not act.
func (s *server) actUnedited(ctx context.Context, row heldRow, why string) {
	if s.deps.Reviewer.config.Unedited == UneditedRewrite && row.rewrite != nil && row.notApplied == "" {
		s.decideHeld(ctx, row, heldDecision{outcome: reviewRewritePosted, detail: why})
		return
	}
	detail := why
	if row.rewrite != nil && row.notApplied != "" {
		detail += "; the reviewer's rewrite is not applied: " + row.notApplied
	}
	s.decideHeld(ctx, row, heldDecision{outcome: reviewPostedAsWritten, detail: detail})
}

// writerGone reports whether writer, a held text's writer session, has gone: Envoy has no record
// of it, or reads it gone. A process with no listener configured reaches no session, so its
// writers are gone too. A listener that did not answer reads as a writer still running: the window
// then decides.
func (s *server) writerGone(ctx context.Context, writer string) bool {
	if writer == "" || s.deps.Envoy == nil {
		return true
	}
	session, err := s.deps.Envoy.Session(ctx, writer)
	switch {
	case errors.Is(err, dispatchenvoy.ErrNotFound):
		return true
	case err != nil:
		return false
	}
	return session.State == dispatchenvoy.StateGone
}

// sendFindingsFrame sends writer the turn frame says, as Dispatch's system actor, through the
// listener: a message.created frame whose message is the text's own id, with no stored message
// row and no event, so no person's read or feed can show it. The writer's reply to it comes to
// POST /api/v1/messages/{id}/reply under the same id (answerHeldFrame).
func (s *server) sendFindingsFrame(ctx context.Context, frame reviewer.FindingsFrame, writer, key string) error {
	body, err := frame.Body()
	if err != nil {
		return err
	}
	return s.sendSystemFrame(ctx, systemFrame{
		id: frame.ID, body: body, writer: writer, key: key, mode: "steer", expects: contracts.ExpectsReplyOptional,
	})
}

// systemFrame is a delivery frame Dispatch sends a session as its system actor with no stored
// message: the id the frame's message carries (a held text's or a definition's), its body, the
// session, the send's idempotency key, the delivery mode and what it wants back.
type systemFrame struct {
	id, body, writer, key, mode string
	expects                     contracts.ExpectsReply
}

// sendSystemFrame sends frame through the listener: a message.created frame from Dispatch whose
// message id is frame.id, with no message row and no event, so no person's read or feed can show
// it. The session's reply comes to POST /api/v1/messages/{id}/reply under that id, which the
// route takes as the frame's answer (answerHeldFrame, answerDefinitionFrame).
func (s *server) sendSystemFrame(ctx context.Context, frame systemFrame) error {
	if s.deps.Envoy == nil || frame.writer == "" {
		return errors.New("no listener reaches the writer session")
	}
	target := "session:" + frame.writer
	message := model.Message{ID: frame.id, Author: reviewerActor, Body: frame.body, Target: &target, CreatedAt: time.Now().UTC()}
	encoded, err := json.Marshal(struct {
		Event    model.Event `json:"event"`
		Delivery any         `json:"delivery"`
	}{
		Event: model.Event{
			Type: "message.created", Actor: reviewerActor,
			Payload: messageReplyThread{}.payload(message, model.ReferenceChanges{}),
		},
		Delivery: map[string]any{"attempt": 1, "mode": frame.mode},
	})
	if err != nil {
		return fmt.Errorf("encode the frame: %w", err)
	}
	carried, _ := model.EventBody(frame.body)
	_, err = s.deps.Envoy.Send(ctx, dispatchenvoy.SendInput{
		TargetSession: frame.writer, Message: carried, Payload: encoded, IdempotencyKey: frame.key,
		ExpectsReply: frame.expects,
	})
	return err
}

// recordHeld records one decision about the held text row in writing_check_events, under the
// route, the actor, the reader and the project of the write that held it, and its id, the write's.
func (s *server) recordHeld(ctx context.Context, row heldRow, outcome string, rules []string, detail string) {
	method, route, _ := strings.Cut(row.route, " ")
	event := store.WritingCheckEvent{
		Route: route, Method: method, ActorKind: row.actor.Kind, ActorID: row.actor.ID, Outcome: outcome,
		Rules: rules, Detail: detail, Reader: row.reader, Project: row.project, WriteID: row.id,
	}
	if row.actor.Act != nil {
		event.ActSession, event.TokenID = row.actor.Act.Session, row.actor.Act.TokenID
	}
	s.recordWritingCheck(ctx, event)
}

// ReviewerSweepInterval is how often the sweep of held texts runs, and ReviewerSweepLease the
// lease one process runs it under (cmd/dispatch, duty.RunLeased).
const (
	ReviewerSweepInterval = 10 * time.Second
	ReviewerSweepLease    = "reviewer-sweep"
)

// sweepHeld is one pass of the reviewer's sweep, which one process runs every
// ReviewerSweepInterval under its lease (cmd/dispatch). It fails the held texts that wait on one
// that failed; reviews again each text a stopped process left in review, once the text it waits
// on, if any, has posted (claimHeldReview); posts, as DISPATCH_REVIEWER_UNEDITED says, each text
// whose writer has not acted in time or whose writer session has gone; and posts every decided
// text whose turn has come.
func (s *server) sweepHeld(ctx context.Context) {
	if s.deps.Store == nil {
		return
	}
	if err := s.failOrphanedHeld(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: fail the held texts that wait on a failed one", "error", err)
	}
	stale := intervalOf(s.deps.Reviewer.staleClaim())
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select h.id from held_texts h
		where h.state = 'reviewing' and coalesce(h.claimed_at, h.updated_at) < now() - $1::interval
			and (h.depends_on is null or exists (
				select 1 from held_texts p where p.id = h.depends_on and p.state = 'posted'))
		order by h.seq limit 50`, stale)
	if err == nil {
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			s.startReview(id)
		}
	} else if ctx.Err() == nil {
		slog.Warn("dispatch: read the held texts left in review", "error", err)
	}
	waiting, err := s.loadHeldRows(ctx, `state = 'awaiting_writer' order by seq limit 200`)
	if err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: read the held texts that wait on their writer", "error", err)
	}
	for _, row := range waiting {
		switch {
		case row.deadline != nil && !row.deadline.After(time.Now()):
			s.actUnedited(ctx, row, "the writer did not act within the time it had")
		default:
			if s.writerGone(ctx, row.writer) {
				s.actUnedited(ctx, row, "the writer session stopped running")
			}
		}
	}
	s.postReady()
}

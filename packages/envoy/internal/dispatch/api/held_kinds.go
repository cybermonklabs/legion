package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
)

// held_kinds.go is each kind of write the reviewer holds, as the one interface the hold, the edit,
// the post, the rewrite and the order of one writer's held texts read it through (heldRequest): a
// new ask (askCreate), a new issue (issueCreate), a comment (commentCreate), a message
// (messageCreate), a write kept whole (heldReplay) and a decision block's hold
// (decisionBlockHold). A new kind is one type that implements it.

// heldRequest is a held write's request, decoded: what a held text stores and its post runs.
type heldRequest interface {
	// answer is the answer the held write gets, in its route's own shape, built from what the hold
	// stored under the reserved id and from dry, its dry run, never nil (heldAnswerOf).
	answer(kind reviewer.Kind, id string, actor model.Actor, at time.Time, dry *heldDryRun, trees bool) any
	// advice is the advice the held write's answer carries, given advice, the write advice its dry
	// run found (nil for none), and parent, the held text it waits on (heldAdvice); nil for none.
	advice(s *server, r *http.Request, actor model.Actor, advice *writeAdvice, dry *heldDryRun, parent *heldParent) any
	// dryRun runs the write as actor's under id on its create path with held, a dry run's post,
	// which ends the path before its commit, and answers what the run found (dryRunHeld).
	dryRun(s *server, ctx context.Context, actor model.Actor, id string, held *heldPost) (heldDryRun, error)
	// post runs the write's create path for the held row, held marking it posted in the path's
	// own transaction (runHeldPost).
	post(s *server, ctx context.Context, row heldRow, held *heldPost) error
	// edited is the request with input's wording in place of its own, checked as the kind's create
	// checks it (editedRequest).
	edited(input heldEditInput) (heldRequest, error)
	// rewritten is the write of kind with rewrite's wording in place of its own, false for a kind
	// whose wording no rewrite replaces (withRewrite).
	rewritten(kind reviewer.Kind, rewrite reviewer.Text) (reviewedWrite, bool)
	// place is where the text posts, for the order of one writer's held texts there
	// (heldBeforeOf): the issue or the document of an ask or a comment, the session or the role a
	// message goes to, else its issue; a new issue's project; for a held reply, the place its hold
	// stored (heldReplay.Place). It is "" for a write whose order something else keeps: a write
	// kept beside a held text, by the held text its path names (awaitHeldTarget); a decision
	// block, by its place in its document. A new issue whose parent is held waits on that parent
	// instead, as any write that names a held text does (reviewWrite).
	place() string
	// beside is what the post of the write, held beside a held parent, writes and answers, read
	// off the held texts it waits on as hold reads them, or the refusal that post gives, in its
	// order (heldBeside). It reads on hold.tx and takes no other connection while that
	// transaction is open: a dry run it reads heldBeside runs before (besideHold.ran).
	beside(s *server, ctx context.Context, hold besideHold) (*heldDryRun, error)
	// postEvent is the event the post of the write, held under id, appends, as a session's write
	// advice names it (sessionWriteTypes), and, for an ask, the ask it opens to its writer, as
	// your_open_asks names it; "" and nil for a post that appends none of them (countWaited, and
	// what a write beside a held parent counts as its own, heldIssueAdvice).
	postEvent(id string) (string, *adviceAsk)
	// messageIssue is the issue the post of the held message writes it on, read on q where another
	// text holds it: a new message's own, and a reply to a delivery the delivered message's; ""
	// for a text that is no message (heldLink.messageIssue).
	messageIssue(ctx context.Context, q queryer) (string, error)
	// climb is where the climb of a reply to the held text, of kind, goes next on owner
	// (heldThreadHead): the comment the text replies to, or the ask whose thread it is in, both ""
	// where it heads its thread; refused where it is no comment on owner, as loadOwnedComment
	// refuses a posted one.
	climb(kind reviewer.Kind, owner owner) (replyTo, askID string, err error)
}

// projectPlace is the prefix of a new issue's place, its project's (issueCreate.place), which no
// place of another kind's text starts with (placeOf, messagePlaceOf).
const projectPlace = "project "

// placeOf is the place of a text on o (heldRequest.place): its issue, else its document.
func placeOf(o owner) string {
	switch {
	case o.IssueKey != nil:
		return "issue " + *o.IssueKey
	case o.ArtifactID != nil:
		return "document " + *o.ArtifactID
	}
	return ""
}

// messagePlaceOf is the place of a message to target, a session or a role
// (heldRequest.place), else of one on the issue issueKey; "" for neither.
func messagePlaceOf(target, issueKey string) string {
	switch {
	case target != "":
		return "to " + target
	case issueKey != "":
		return "issue " + issueKey
	}
	return ""
}

// refuseHeldEdit is an edit's refusal of a field its kind does not hold, or of an edit that
// changes nothing: 400 HELD_EDIT_INPUT.
func refuseHeldEdit(format string, args ...any) error {
	return errorf(http.StatusBadRequest, "HELD_EDIT_INPUT", format, args...)
}

// dryRunEnded is a create path's dry run as it ended: dry where it passed every check
// (errHeldDryRun), and its refusal where it did not. A path that committed broke the dry run.
func dryRunEnded(dry heldDryRun, err error) (heldDryRun, error) {
	if errors.Is(err, errHeldDryRun) {
		return dry, nil
	}
	if err == nil {
		return heldDryRun{}, errors.New("a held write's dry run committed")
	}
	return heldDryRun{}, err
}

// dryRunAdvice is the write advice a comment's or a message's dry run found, as its held answer
// merges it: none where the dry run found none, rather than a nil *writeAdvice inside an any.
func dryRunAdvice(advice *writeAdvice) any {
	if advice != nil {
		return advice
	}
	return nil
}

// The new ask.

func (c askCreate) answer(_ reviewer.Kind, id string, actor model.Actor, at time.Time, dry *heldDryRun, trees bool) any {
	ask := model.Ask{
		ID: id, IssueKey: c.Owner.IssueKey, ArtifactID: c.Owner.ArtifactID, Author: actor,
		Kind: c.Kind, Question: c.Question, Options: c.Options, Multiple: c.Multiple,
		Urgency: c.Urgency, State: "open", CreatedAt: at,
	}
	if wrote := dry.ask; wrote != nil {
		ask.Anchor, ask.AnchorArtifact, ask.WaitingOnPerson = heldAnchor(wrote.Anchor), wrote.AnchorArtifact, wrote.WaitingOnPerson
	}
	return newShapedAsk(ask, trees)
}

// advice is the ask's write advice with its related-item check, run here, before the id is
// reserved, as the write's own path runs it after its commit. An ask on a held issue searches
// that issue's project, which no issue row holds yet.
func (c askCreate) advice(s *server, r *http.Request, actor model.Actor, advice *writeAdvice, _ *heldDryRun, parent *heldParent) any {
	ctx := r.Context()
	input := relatedInput{
		text: c.Question, title: c.Question, source: suggestionSource{kind: "ask", actor: actor},
		question: true,
	}
	if c.Owner.IssueKey != nil {
		input.source.issueKey = *c.Owner.IssueKey
	} else if c.Owner.ArtifactID != nil {
		input.source.artifactID = *c.Owner.ArtifactID
	}
	if parent != nil {
		input.project = parent.project
	}
	input.trees = s.readDisplayTrees(ctx, wantsDisplayTrees(r))
	suggestions := s.relatedCheck(ctx, input)
	input.trees.resolve()
	return suggestedAdvice(advice, suggestions)
}

func (c askCreate) dryRun(s *server, ctx context.Context, actor model.Actor, id string, held *heldPost) (heldDryRun, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return heldDryRun{}, err
	}
	defer tx.Rollback(ctx)
	written, err := s.writeAsk(ctx, tx, c, actor, id, held)
	return dryRunEnded(heldDryRun{advice: written.advice, ask: &written.ask}, err)
}

func (c askCreate) post(s *server, ctx context.Context, row heldRow, held *heldPost) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	written, err := s.writeAsk(ctx, tx, c, row.actor, row.id, held)
	if err != nil {
		return err
	}
	s.announceAsk(ctx, written)
	return nil
}

// edited takes the ask's question and its options.
func (c askCreate) edited(input heldEditInput) (heldRequest, error) {
	if input.Title != nil || input.Spec != nil || input.Body != nil {
		return nil, refuseHeldEdit("an ask's edit takes its question and its options")
	}
	if input.Question == nil && input.Options == nil {
		return nil, refuseHeldEdit("an ask's edit takes its question, its options or both")
	}
	if input.Question != nil {
		if err := validateAskQuestion(*input.Question); err != nil {
			return nil, err
		}
		c.Question = *input.Question
	}
	if input.Options != nil {
		options := askOptionsOf(*input.Options)
		if err := validateAskOptions(options); err != nil {
			return nil, err
		}
		c.Options = options
	}
	if err := validateAskWording(c.Question, c.Options); err != nil {
		return nil, err
	}
	return c, nil
}

// rewritten takes the rewrite's question and options. The options keep no id: a stored ask's
// option ids are the index's to mint (reviewer.OptionWords).
func (c askCreate) rewritten(_ reviewer.Kind, rewrite reviewer.Text) (reviewedWrite, bool) {
	c.Question = rewrite.Body
	if rewrite.Options != nil {
		c.Options = reviewer.OptionWords(rewrite.Options)
	}
	return c.reviewed(), true
}

func (c askCreate) place() string { return placeOf(c.Owner) }

// beside reads, beside a held parent, whom the ask waits on - on a held issue, the assignee that
// issue's post writes (heldIssueAssignee) - and its anchor in that issue's spec (heldSpecAnchor),
// refused as the ask's post refuses it, with the document it quotes; and the advice of its issue
// (heldIssueAdvice).
func (c askCreate) beside(s *server, ctx context.Context, hold besideHold) (*heldDryRun, error) {
	key := deref(c.Owner.IssueKey)
	ask := &model.Ask{}
	var err error
	if issue, actor, held := hold.waited.issue(key); held {
		if ask.Anchor, err = heldSpecAnchor(issue, c.Anchor, docs.MarkAsk, hold.waited.keptBeside(key)); err != nil {
			return nil, err
		}
		if ask.Anchor != nil {
			ask.AnchorArtifact = &model.AskAnchorArtifact{Project: issue.Project, Slug: "spec", Name: "spec.md", Primary: true}
		}
		assignee, err := s.heldIssueAssignee(ctx, hold.tx, issue, actor, hold.waited)
		if err != nil {
			return nil, err
		}
		ask.WaitingOnPerson = deref(assignee)
	} else if ask.WaitingOnPerson, err = askWaitingOnPerson(ctx, hold.tx, c.Owner, nil); err != nil {
		return nil, err
	}
	appending, _ := c.postEvent(hold.id)
	advice, err := s.heldIssueAdvice(ctx, hold, key, routeOpenAsk, appending, adviceAsks{excluded: []string{hold.id}}, true)
	if err != nil {
		return nil, err
	}
	return &heldDryRun{ask: ask, advice: advice}, nil
}

// postEvent is ask.opened, and the ask its post opens.
func (c askCreate) postEvent(id string) (string, *adviceAsk) {
	return "ask.opened", &adviceAsk{ID: id, Question: c.Question}
}

// messageIssue is none: an ask is no message.
func (c askCreate) messageIssue(context.Context, queryer) (string, error) { return "", nil }

// climb refuses: an ask is no comment a reply's reply_to names.
func (c askCreate) climb(reviewer.Kind, owner) (string, string, error) {
	return "", "", replyToNotOnOwner()
}

// The new issue.

func (c issueCreate) answer(_ reviewer.Kind, id string, actor model.Actor, at time.Time, dry *heldDryRun, _ bool) any {
	number, _ := strconv.Atoi(strings.TrimPrefix(id, c.Project+"-"))
	labels := c.Labels
	if labels == nil {
		labels = []string{}
	}
	issue := model.Issue{
		Key: id, Project: c.Project, Number: number, Title: c.Title, Status: model.IssueStatusTriage,
		Priority: c.Priority, Labels: labels, Assignee: c.Assignee, BlockedBy: c.Blockers,
		CreatedBy: &actor, CreatedAt: at, UpdatedAt: at,
	}
	if issue.BlockedBy == nil {
		issue.BlockedBy = []string{}
	}
	if c.Parent != "" {
		issue.Parent = &c.Parent
	}
	heldIssueWrites(&issue, c, dry.issue)
	return issue
}

// advice is the issue's write advice with its related-item check, run here, before the id is
// reserved, over the spec as its seed stores it (dry.markdown, which a dry run's seed made), and
// what the near-copy gate told its create (issueCreate.gate), the note of a check not made
// included.
func (c issueCreate) advice(s *server, r *http.Request, actor model.Actor, advice *writeAdvice, dry *heldDryRun, _ *heldParent) any {
	ctx := r.Context()
	markdown := dry.markdown
	trees := s.readDisplayTrees(ctx, wantsDisplayTrees(r))
	suggestions := s.relatedCheck(ctx, relatedInput{
		project: c.Project,
		text:    c.Title + "\n" + markdown,
		title:   c.Title,
		source:  suggestionSource{kind: "issue", actor: actor},
		trees:   trees,
	})
	trees.resolve()
	c.gate.tell(suggestions)
	return suggestedAdvice(advice, suggestions)
}

func (c issueCreate) dryRun(s *server, ctx context.Context, actor model.Actor, _ string, held *heldPost) (heldDryRun, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return heldDryRun{}, err
	}
	defer tx.Rollback(ctx)
	written, err := s.writeIssue(ctx, tx, c, actor, 0, held)
	return dryRunEnded(heldDryRun{advice: written.advice, markdown: written.markdown, issue: &written.issue, assignee: written.issue.Assignee}, err)
}

// post files the issue under the number its hold reserved, after the near-copy gate's second
// read (recheckNearCopy), and tells its writer of a near copy found then (tellNearCopy).
func (c issueCreate) post(s *server, ctx context.Context, row heldRow, held *heldPost) error {
	number, err := strconv.Atoi(strings.TrimPrefix(row.id, c.Project+"-"))
	if err != nil || number < 1 {
		return errorf(http.StatusUnprocessableEntity, "HELD_REQUEST", "held issue %s names no number of project %s", row.id, c.Project)
	}
	create, copies := s.recheckNearCopy(ctx, c)
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	written, err := s.writeIssue(ctx, tx, create, row.actor, number, held)
	if errors.Is(err, errExternalIssueImported) {
		return errorf(http.StatusConflict, "EXTERNAL_IMPORTED", "the GitHub issue is imported already")
	}
	if err != nil {
		return err
	}
	s.announceIssue(ctx, written)
	s.tellNearCopy(ctx, row, copies)
	return nil
}

// edited takes the issue's title and its spec.
func (c issueCreate) edited(input heldEditInput) (heldRequest, error) {
	if input.Question != nil || input.Options != nil || input.Body != nil {
		return nil, refuseHeldEdit("an issue's edit takes its title and its spec")
	}
	if input.Title == nil && input.Spec == nil {
		return nil, refuseHeldEdit("an issue's edit takes its title, its spec or both")
	}
	if input.Title != nil {
		title := strings.TrimSpace(*input.Title)
		if title == "" {
			return nil, errorf(http.StatusBadRequest, "INVALID_ISSUE", "title is required")
		}
		if length := itemblocks.Units16(title); length > contracts.IssueTitleMax {
			return nil, capExceededError("title", length, contracts.IssueTitleMax)
		}
		c.Title = title
	}
	if input.Spec != nil {
		spec := *input.Spec
		c.Spec = &spec
	}
	return c, nil
}

// rewritten takes the rewrite's title and spec.
func (c issueCreate) rewritten(_ reviewer.Kind, rewrite reviewer.Text) (reviewedWrite, bool) {
	c.Title = rewrite.Title
	spec := rewrite.Body
	c.Spec = &spec
	return c.reviewed(), true
}

// place is the issue's project: one writer's new issues there, filed under keys reserved in the
// order written, post in that order.
func (c issueCreate) place() string { return projectPlace + c.Project }

// beside reads, beside its held parent, what the new issue's post writes: the refusals of its
// blockers, its parent and every other check of that post, with its spec as its seed renders it,
// off its dry run, which heldBeside ran before it opened hold.tx (heldIssueCreateError,
// besideHold.ran); the assignee its post files it under (heldIssueAssignee); and the advice of a
// new issue in triage under its labels and parent chain (heldIssueRow), with the blocks of its
// spec its dry run read off that rendering.
func (c issueCreate) beside(s *server, ctx context.Context, hold besideHold) (*heldDryRun, error) {
	assignee, err := s.heldIssueAssignee(ctx, hold.tx, c, hold.actor, hold.waited)
	if err != nil {
		return nil, err
	}
	advice := s.writeAdviceBefore(ctx, hold.tx, routeNewIssue, "", hold.actor, model.IssueStatusTriage, heldIssueRow(c, hold.waited), adviceAsks{})
	if advice != nil && hold.ran.advice != nil {
		advice.documentBlocks = hold.ran.advice.documentBlocks
	}
	return &heldDryRun{markdown: hold.ran.markdown, assignee: assignee, advice: advice}, nil
}

// postEvent is none a session's write advice counts: a new issue's advice is a new issue's.
func (c issueCreate) postEvent(string) (string, *adviceAsk) { return "", nil }

// messageIssue is none: an issue is no message.
func (c issueCreate) messageIssue(context.Context, queryer) (string, error) { return "", nil }

// climb refuses: an issue is no comment a reply's reply_to names.
func (c issueCreate) climb(reviewer.Kind, owner) (string, string, error) {
	return "", "", replyToNotOnOwner()
}

// The comment.

func (c commentCreate) answer(_ reviewer.Kind, id string, actor model.Actor, at time.Time, dry *heldDryRun, trees bool) any {
	comment := model.Comment{
		ID: id, IssueKey: c.Owner.IssueKey, ArtifactID: c.Owner.ArtifactID, Author: actor,
		Body: c.Input.Body, ReplyTo: c.Input.ReplyTo, AskID: c.Input.AskID, CreatedAt: at,
		Mentions: []model.Mention{}, Deliveries: []model.CommentDelivery{},
	}
	if c.Input.Suggestion != nil {
		comment.Suggestion = &model.Suggestion{ReplaceWith: c.Input.Suggestion.ReplaceWith}
	}
	if wrote := dry.comment; wrote != nil {
		heldCommentWrites(&comment, wrote)
	}
	return commentWriteResponse{shapedComment: newShapedComment(comment, trees), AskWaitingOn: dry.askWaitingOn}
}

// advice is the comment's write advice, which its dry run found.
func (c commentCreate) advice(_ *server, _ *http.Request, _ model.Actor, advice *writeAdvice, _ *heldDryRun, _ *heldParent) any {
	return dryRunAdvice(advice)
}

func (c commentCreate) dryRun(s *server, ctx context.Context, actor model.Actor, id string, held *heldPost) (heldDryRun, error) {
	written, err := s.writeComment(ctx, c, actor, id, held)
	return dryRunEnded(heldDryRun{advice: written.advice, comment: &written.comment, askWaitingOn: written.waitingOn}, err)
}

func (c commentCreate) post(s *server, ctx context.Context, row heldRow, held *heldPost) error {
	_, err := s.writeComment(ctx, c, row.actor, row.id, held)
	return err
}

// edited takes the comment's body.
func (c commentCreate) edited(input heldEditInput) (heldRequest, error) {
	if input.Question != nil || input.Options != nil || input.Title != nil || input.Spec != nil || input.Body == nil {
		return nil, refuseHeldEdit("a comment's edit takes its body")
	}
	body := pmdoc.LineFeeds(*input.Body)
	if c.Input.Suggestion == nil && strings.TrimSpace(body) == "" {
		return nil, errorf(http.StatusBadRequest, "INVALID_COMMENT", "comment body is required")
	}
	if err := blockBoundError(itemblocks.Check("body", body)); err != nil {
		return nil, err
	}
	c.Input.Body = body
	return c, nil
}

// rewritten takes the rewrite's body.
func (c commentCreate) rewritten(_ reviewer.Kind, rewrite reviewer.Text) (reviewedWrite, bool) {
	c.Input.Body = rewrite.Body
	return c.reviewed(), true
}

func (c commentCreate) place() string { return placeOf(c.Owner) }

// beside reads, beside a held parent, what the comment's post writes, in the order that post
// checks it: its thread fields (commentThreadInputError) and an ask_id that is no ask id
// (askIDInputError); the thread it joins (heldBesideThread); the block reply_to_block names in the
// comment reply_to names, else in the ask ask_id names, one of which a request naming a block
// names (heldNamedBlock); its turn (commentTurnError); its anchor in a held issue's spec, as the
// mark its post writes takes it (heldSpecAnchor); whom its ask waits on once it posts; and the
// advice of its issue (heldIssueAdvice).
func (c commentCreate) beside(s *server, ctx context.Context, hold besideHold) (*heldDryRun, error) {
	if err := commentThreadInputError(c.Input); err != nil {
		return nil, err
	}
	if c.Input.AskID != nil {
		if err := s.askIDInputError(ctx, hold.tx, hold.actor, c.Owner, *c.Input.AskID); err != nil {
			return nil, err
		}
	}
	thread, err := s.heldBesideThread(ctx, hold.tx, c.Owner, c.Input, hold.waited)
	if err != nil {
		return nil, err
	}
	comment := &model.Comment{ReplyTo: thread.ReplyTo, AskID: thread.AskID, Answers: thread.Answers}
	if c.Input.ReplyToBlock != nil {
		kind, item := refs.ItemComment, c.Input.ReplyTo
		if item == nil {
			kind, item = refs.ItemAsk, c.Input.AskID
		}
		block, err := s.heldNamedBlock(ctx, hold.tx, hold.actor, kind, *item, *c.Input.ReplyToBlock, "reply_to_block", "INVALID_COMMENT", hold.waited)
		if err != nil {
			return nil, err
		}
		issueKey, project, slug, err := s.ownerAddress(ctx, hold.tx, c.Owner)
		if err != nil {
			return nil, err
		}
		comment.ReplyBlock = block.replyBlock(issueKey, project, slug)
	}
	if err := commentTurnError(c.Input.Turn, thread.AskID); err != nil {
		return nil, err
	}
	key := deref(c.Owner.IssueKey)
	if issue, _, held := hold.waited.issue(key); held {
		if comment.Anchor, err = heldSpecAnchor(issue, c.Input.Anchor, commentMarkKind(c.Input.Suggestion != nil), hold.waited.keptBeside(key)); err != nil {
			return nil, err
		}
	}
	comment.Turn = thread.replyTurn(hold.actor, c.Input.Turn)
	appending, _ := c.postEvent(hold.id)
	advice, err := s.heldIssueAdvice(ctx, hold, key, routeIssueComment, appending, adviceAsks{}, false)
	if err != nil {
		return nil, err
	}
	return &heldDryRun{comment: comment, askWaitingOn: thread.waitingOnAfter(comment.Turn), advice: advice}, nil
}

// postEvent is comment.created.
func (c commentCreate) postEvent(string) (string, *adviceAsk) { return "comment.created", nil }

// messageIssue is none: a comment is no message.
func (c commentCreate) messageIssue(context.Context, queryer) (string, error) { return "", nil }

// climb is the comment the comment replies to and the ask whose thread it joins, refused where it
// is on another owner than the reply.
func (c commentCreate) climb(_ reviewer.Kind, on owner) (string, string, error) {
	if !sameOwner(c.Owner, on) {
		return "", "", replyToNotOnOwner()
	}
	return deref(c.Input.ReplyTo), deref(c.Input.AskID), nil
}

// The message.

func (c messageCreate) answer(_ reviewer.Kind, id string, actor model.Actor, at time.Time, dry *heldDryRun, trees bool) any {
	message := model.Message{
		ID: id, IssueKey: c.IssueKey, Author: actor, Body: c.Input.Body, Target: c.Input.Target,
		InReplyTo: c.Input.InReplyTo, CreatedAt: at, Deliveries: []model.MessageDelivery{},
	}
	if wrote := dry.message; wrote != nil {
		heldMessageWrites(&message, wrote)
	}
	return newShapedMessage(message, trees)
}

// advice is the message's write advice, which its dry run found.
func (c messageCreate) advice(_ *server, _ *http.Request, _ model.Actor, advice *writeAdvice, _ *heldDryRun, _ *heldParent) any {
	return dryRunAdvice(advice)
}

func (c messageCreate) dryRun(s *server, ctx context.Context, actor model.Actor, id string, held *heldPost) (heldDryRun, error) {
	message, advice, err := s.writeMessage(ctx, c, actor, id, held)
	return dryRunEnded(heldDryRun{advice: advice, message: &message}, err)
}

func (c messageCreate) post(s *server, ctx context.Context, row heldRow, held *heldPost) error {
	_, _, err := s.writeMessage(ctx, c, row.actor, row.id, held)
	return err
}

// edited takes the message's body, checked as a new message's is.
func (c messageCreate) edited(input heldEditInput) (heldRequest, error) {
	if input.Question != nil || input.Options != nil || input.Title != nil || input.Spec != nil || input.Body == nil {
		return nil, refuseHeldEdit("a message's edit takes its body")
	}
	c.Input.Body = *input.Body
	delivery, err := validateCreateMessage(&c.Input)
	if err != nil {
		return nil, err
	}
	c.Delivery = delivery
	return c, nil
}

// rewritten takes the rewrite's body.
func (c messageCreate) rewritten(_ reviewer.Kind, rewrite reviewer.Text) (reviewedWrite, bool) {
	c.Input.Body = rewrite.Body
	return c.reviewed(), true
}

func (c messageCreate) place() string {
	return messagePlaceOf(deref(c.Input.Target), deref(c.IssueKey))
}

// beside reads, beside a held parent, what the message's post writes, in the order that post
// checks it: the message it answers (heldReplyParentError), the block in_reply_to_block names in
// that message (heldNamedBlock), and the advice of its issue (heldIssueAdvice).
func (c messageCreate) beside(s *server, ctx context.Context, hold besideHold) (*heldDryRun, error) {
	if err := heldReplyParentError(ctx, hold.tx, c, hold.waited); err != nil {
		return nil, err
	}
	dry := &heldDryRun{}
	if c.Input.InReplyToBlock != nil {
		block, err := s.heldNamedBlock(ctx, hold.tx, hold.actor, refs.ItemMessage, deref(c.Input.InReplyTo), *c.Input.InReplyToBlock, "in_reply_to_block", "INVALID_MESSAGE", hold.waited)
		if err != nil {
			return nil, err
		}
		dry.message = &model.Message{ReplyBlock: block.replyBlock(c.IssueKey, "", "")}
	}
	appending, _ := c.postEvent(hold.id)
	var err error
	if dry.advice, err = s.heldIssueAdvice(ctx, hold, deref(c.IssueKey), routeIssueMessage, appending, adviceAsks{}, false); err != nil {
		return nil, err
	}
	return dry, nil
}

// postEvent is message.answered for a reply, else message.created.
func (c messageCreate) postEvent(string) (string, *adviceAsk) {
	if c.Input.InReplyTo != nil {
		return "message.answered", nil
	}
	return "message.created", nil
}

// messageIssue is the message's own issue.
func (c messageCreate) messageIssue(context.Context, queryer) (string, error) {
	return deref(c.IssueKey), nil
}

// climb refuses: a message is no comment a reply's reply_to names.
func (c messageCreate) climb(reviewer.Kind, owner) (string, string, error) {
	return "", "", replyToNotOnOwner()
}

// The write kept whole: a reply, or a write that names a held text.

func (c heldReplay) answer(kind reviewer.Kind, id string, actor model.Actor, at time.Time, dry *heldDryRun, trees bool) any {
	return replayAnswerOf(kind, c, id, actor, at, dry, trees)
}

// advice is none: the route's own answer carries what it carries.
func (c heldReplay) advice(*server, *http.Request, model.Actor, *writeAdvice, *heldDryRun, *heldParent) any {
	return nil
}

// dryRun runs the route through the server's own mux (runReplay). It passes where the route ends
// the dry run, which keeps the reply it wrote (heldPost.keep); any other answer of the route is
// the reply's own, in answered: a refusal, or a reply to an attempt already answered, which writes
// nothing and answers the stored reply as it does with the reviewer off (the reply routes'
// finishRepeat), so nothing is held for it.
func (c heldReplay) dryRun(s *server, ctx context.Context, _ model.Actor, _ string, held *heldPost) (heldDryRun, error) {
	// The reply's route reads what its hold learned of the writer's role (roleHolderReplyOf).
	held.roleHolder = c.RoleHolder
	answer, err := s.runReplay(ctx, c, held)
	switch {
	case err != nil:
		return heldDryRun{}, err
	case answer.isDryRunPass():
		var dry heldDryRun
		switch wrote := held.wrote.(type) {
		case commentWritten:
			dry.comment, dry.askWaitingOn = &wrote.comment, wrote.waitingOn
		case model.Message:
			dry.message = &wrote
		}
		return dry, nil
	}
	return heldDryRun{answered: answer}, nil
}

func (c heldReplay) post(s *server, ctx context.Context, row heldRow, held *heldPost) error {
	held.roleHolder = c.RoleHolder
	return s.postReplay(ctx, row, c, held)
}

// edited takes a held reply's body; a write kept whole that holds no text takes no edit.
func (c heldReplay) edited(input heldEditInput) (heldRequest, error) {
	if c.Text.Body == "" || input.Question != nil || input.Options != nil || input.Title != nil || input.Spec != nil || input.Body == nil {
		return nil, refuseHeldEdit("a held reply's edit takes its body")
	}
	body := pmdoc.LineFeeds(*input.Body)
	if strings.TrimSpace(body) == "" {
		return nil, refuseHeldEdit("a reply's body is required")
	}
	if err := blockBoundError(itemblocks.Check("body", body)); err != nil {
		return nil, err
	}
	edited, err := c.withBody(body)
	if err != nil {
		return nil, err
	}
	return edited, nil
}

// rewritten takes the rewrite's body into the kept request's body.
func (c heldReplay) rewritten(kind reviewer.Kind, rewrite reviewer.Text) (reviewedWrite, bool) {
	edited, err := c.withBody(rewrite.Body)
	if err != nil {
		return reviewedWrite{}, false
	}
	return edited.reviewed(kind), true
}

func (c heldReplay) place() string { return c.Place }

// beside reads, for a follow-up whose attempt its writer's earlier held reply answers, what its
// post writes under that reply (heldFollowUpBeside); its answer carries no write advice, as a
// write kept whole carries none. No other write kept whole is held beside a held text without
// running its dry run (heldWaitedBeside).
func (c heldReplay) beside(s *server, ctx context.Context, hold besideHold) (*heldDryRun, error) {
	return s.heldFollowUpBeside(ctx, hold.tx, c, hold.waited)
}

// postEvent is none a session's write advice counts: a reply's post appends comment.answered or
// message.answered (sessionWriteTypes counts neither), and a write kept whole names no issue its
// record keeps.
func (c heldReplay) postEvent(string) (string, *adviceAsk) { return "", nil }

// messageIssue is, for a reply to a delivered message, that message's issue, which the reply's
// post carries over (deliveredMessage).
func (c heldReplay) messageIssue(ctx context.Context, q queryer) (string, error) {
	issue, _, err := deliveredMessage(ctx, q, c.Subject.InReplyTo)
	return deref(issue), err
}

// climb is, for a reply to a mention, the comment it answers, on that comment's owner, which the
// climb reads next; any other write kept whole is no comment.
func (c heldReplay) climb(kind reviewer.Kind, _ owner) (string, string, error) {
	if kind != reviewer.KindComment {
		return "", "", replyToNotOnOwner()
	}
	return c.Subject.ReplyTo, "", nil
}

// The decision block's hold: the block is in its document already, so its hold answers nothing,
// runs no dry run, takes no edit and no rewrite of its request; its post ends the hold
// (releaseDecisionBlock).

func (c decisionBlockHold) answer(_ reviewer.Kind, id string, _ model.Actor, _ time.Time, _ *heldDryRun, _ bool) any {
	return map[string]string{"id": id}
}

func (c decisionBlockHold) advice(*server, *http.Request, model.Actor, *writeAdvice, *heldDryRun, *heldParent) any {
	return nil
}

func (c decisionBlockHold) dryRun(*server, context.Context, model.Actor, string, *heldPost) (heldDryRun, error) {
	// The request is no part of the text: an error a handler logs carries nothing of an actor.
	return heldDryRun{}, errors.New("a held write of a kind this server has no path for cannot run")
}

func (c decisionBlockHold) post(s *server, ctx context.Context, row heldRow, held *heldPost) error {
	return s.releaseDecisionBlock(ctx, row, held)
}

func (c decisionBlockHold) edited(heldEditInput) (heldRequest, error) {
	return nil, refuseHeldEdit("this held text takes no edit")
}

func (c decisionBlockHold) rewritten(reviewer.Kind, reviewer.Text) (reviewedWrite, bool) {
	return reviewedWrite{}, false
}

func (c decisionBlockHold) place() string { return "" }

// beside is never asked: a decision block's hold answers no write, and none waits beside a held
// text.
func (c decisionBlockHold) beside(*server, context.Context, besideHold) (*heldDryRun, error) {
	return nil, errors.New("a decision block's hold answers no write held beside a held text")
}

// postEvent is none: a decision block's post ends its hold, appending no event of a session's.
func (c decisionBlockHold) postEvent(string) (string, *adviceAsk) { return "", nil }

// messageIssue is none: a decision block is no message.
func (c decisionBlockHold) messageIssue(context.Context, queryer) (string, error) { return "", nil }

// climb refuses: a decision block is no comment a reply's reply_to names.
func (c decisionBlockHold) climb(reviewer.Kind, owner) (string, string, error) {
	return "", "", replyToNotOnOwner()
}

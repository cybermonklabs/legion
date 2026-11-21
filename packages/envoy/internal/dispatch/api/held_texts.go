package api

import (
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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// held_texts.go keeps the texts the reviewer holds (store migration 20261119050004_held_texts):
// the hold, which reserves the text's id and answers the write at once; the decisions the review,
// the writer and the sweep make; and the post, which runs the kind's own create path with the
// stored request, the recorded actor and the reserved id, in one transaction that also marks the
// held row posted. A held text that a later write names as its parent or as the text it answers
// has that write held beside it, reviewed once it posts where the reviewer reads that write, and
// posted right after it in the order the writes came.

// The states of a held text (held_texts.state).
const (
	heldReviewing      = "reviewing"
	heldAwaitingWriter = "awaiting_writer"
	heldPosting        = "posting"
	heldPosted         = "posted"
	heldFailed         = "failed"
)

// heldOpenStatesSQL is the states of a held text that has neither posted nor failed, as the SQL
// list `state in` reads: the predicate of the held_texts_open index (store migration
// 20261119050004_held_texts), which a query naming these states reads through.
const heldOpenStatesSQL = "('" + heldReviewing + "', '" + heldAwaitingWriter + "', '" + heldPosting + "')"

// heldNotReviewed is the outcome of a held text the reviewer does not read before it posts: a
// write that waits on a held text with its own kind's setting not hold.
const heldNotReviewed = "not_reviewed"

// heldRequestKind is the kind of a write kept whole beside a held text it names, which is no text
// of its own (awaitHeldTarget).
const heldRequestKind = "request"

// heldRow is one row of held_texts.
type heldRow struct {
	id, kind, route                 string
	request                         json.RawMessage
	original                        reviewer.Text
	actor                           model.Actor
	writer, reader, project         string
	issueKey                        string
	state                           string
	revision                        int
	reviewed                        bool
	findings                        []reviewer.Finding
	rewrite                         *reviewer.Text
	notApplied                      string
	outcome, reason, detail, answer string
	deadline                        *time.Time
	dependsOn                       string
	writtenAt                       time.Time
	postedAt                        *time.Time
	// personTyped is a write a person typed in a session, held beside the text it waits on: its
	// post marks its events person_typed (Decision 26).
	personTyped bool
}

// heldColumns are the columns scanHeld reads, in its order.
const heldColumns = `id, kind, route, request, original, actor, coalesce(writer, ''), coalesce(reader, ''),
	coalesce(project, ''), coalesce(issue_key, ''), state, revision, reviewed, findings, rewrite,
	coalesce(not_applied, ''), coalesce(outcome, ''), coalesce(reason, ''), coalesce(detail, ''),
	coalesce(writer_answer, ''), deadline, coalesce(depends_on, ''), written_at, posted_at, person_typed`

func scanHeld(row pgx.Row) (heldRow, error) {
	var held heldRow
	var original, actor, findings, rewrite []byte
	if err := row.Scan(&held.id, &held.kind, &held.route, &held.request, &original, &actor, &held.writer, &held.reader,
		&held.project, &held.issueKey, &held.state, &held.revision, &held.reviewed, &findings, &rewrite,
		&held.notApplied, &held.outcome, &held.reason, &held.detail, &held.answer, &held.deadline, &held.dependsOn,
		&held.writtenAt, &held.postedAt, &held.personTyped); err != nil {
		return heldRow{}, err
	}
	if err := json.Unmarshal(original, &held.original); err != nil {
		return heldRow{}, fmt.Errorf("read held text %s's original: %w", held.id, err)
	}
	if err := json.Unmarshal(actor, &held.actor); err != nil {
		return heldRow{}, fmt.Errorf("read held text %s's actor: %w", held.id, err)
	}
	if err := json.Unmarshal(findings, &held.findings); err != nil {
		return heldRow{}, fmt.Errorf("read held text %s's findings: %w", held.id, err)
	}
	if len(rewrite) > 0 {
		held.rewrite = &reviewer.Text{}
		if err := json.Unmarshal(rewrite, held.rewrite); err != nil {
			return heldRow{}, fmt.Errorf("read held text %s's rewrite: %w", held.id, err)
		}
	}
	return held, nil
}

// loadHeld reads the held text id; a held text that does not exist is 404 HELD_NOT_FOUND.
func (s *server) loadHeld(ctx context.Context, q queryer, id string, lock bool) (heldRow, error) {
	query := `select ` + heldColumns + ` from held_texts where id = $1`
	if lock {
		query += ` for update`
	}
	row, err := scanHeld(q.QueryRow(ctx, query, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return heldRow{}, errorf(http.StatusNotFound, "HELD_NOT_FOUND", "no held text has the id %s", id)
	}
	return row, err
}

// loadHeldRows reads the held texts where says, on the pool.
func (s *server) loadHeldRows(ctx context.Context, where string, args ...any) ([]heldRow, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `select `+heldColumns+` from held_texts where `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var held []heldRow
	for rows.Next() {
		row, err := scanHeld(rows)
		if err != nil {
			return nil, err
		}
		held = append(held, row)
	}
	return held, rows.Err()
}

// write is the held text's request, decoded by its kind: the write as it posts now, the writer's
// edits applied. A write kept whole (heldReplay) is decoded as one, whatever its kind.
func (h heldRow) write() (reviewedWrite, error) {
	if isReplay(h.request) {
		var replay heldReplay
		if err := json.Unmarshal(h.request, &replay); err != nil {
			return reviewedWrite{}, err
		}
		return replay.reviewed(reviewer.Kind(h.kind)), nil
	}
	switch reviewer.Kind(h.kind) {
	case reviewer.KindDecisionBlock:
		var hold decisionBlockHold
		if err := json.Unmarshal(h.request, &hold); err != nil {
			return reviewedWrite{}, err
		}
		return reviewedWrite{
			kind: reviewer.KindDecisionBlock, request: hold, text: h.original,
			subject: reviewer.Subject{AskID: hold.AskID, IssueKey: h.issueKey}, holdable: true,
		}, nil
	case reviewer.KindAsk:
		var create askCreate
		if err := json.Unmarshal(h.request, &create); err != nil {
			return reviewedWrite{}, err
		}
		return create.reviewed(), nil
	case reviewer.KindIssue:
		var create issueCreate
		if err := json.Unmarshal(h.request, &create); err != nil {
			return reviewedWrite{}, err
		}
		return create.reviewed(), nil
	case reviewer.KindComment:
		var create commentCreate
		if err := json.Unmarshal(h.request, &create); err != nil {
			return reviewedWrite{}, err
		}
		return create.reviewed(), nil
	case reviewer.KindMessage:
		var create messageCreate
		if err := json.Unmarshal(h.request, &create); err != nil {
			return reviewedWrite{}, err
		}
		return create.reviewed(), nil
	}
	return reviewedWrite{}, fmt.Errorf("held text %s is of kind %q, which no create path writes", h.id, h.kind)
}

// reviewed is the ask as the reviewer reads it: its question, options and choice, on its issue
// or in its document, at the quote its anchor names.
func (c askCreate) reviewed() reviewedWrite {
	subject := reviewer.Subject{IssueKey: deref(c.Owner.IssueKey), DocumentID: deref(c.Owner.ArtifactID)}
	if c.Anchor != nil {
		if _, err := uuid.Parse(c.Anchor.Artifact); err == nil {
			subject.DocumentID = c.Anchor.Artifact
		}
		subject.Quote = deref(c.Anchor.Quote)
	}
	return reviewedWrite{
		kind: reviewer.KindAsk, request: c, subject: subject, names: []string{deref(c.Owner.IssueKey)}, holdable: true,
		text: reviewer.Text{Kind: reviewer.KindAsk, Body: c.Question, Options: c.Options, Multiple: c.Multiple},
	}
}

// reviewed is the comment as the reviewer reads it: its body, in the thread it answers, on its
// issue or its document.
func (c commentCreate) reviewed() reviewedWrite {
	subject := reviewer.Subject{
		IssueKey: deref(c.Owner.IssueKey), DocumentID: deref(c.Owner.ArtifactID),
		ReplyTo: deref(c.Input.ReplyTo), AskID: deref(c.Input.AskID),
	}
	if c.Input.Anchor != nil {
		if _, err := uuid.Parse(c.Input.Anchor.Artifact); err == nil {
			subject.DocumentID = c.Input.Anchor.Artifact
		}
		subject.Quote = deref(c.Input.Anchor.Quote)
	}
	return reviewedWrite{
		kind: reviewer.KindComment, request: c, subject: subject, holdable: true,
		names: []string{deref(c.Owner.IssueKey), deref(c.Input.ReplyTo), deref(c.Input.AskID)},
		text:  reviewer.Text{Kind: reviewer.KindComment, Body: c.Input.Body},
	}
}

// reviewed is the message as the reviewer reads it: its body, in the conversation it answers, on
// its issue.
func (c messageCreate) reviewed() reviewedWrite {
	return reviewedWrite{
		kind: reviewer.KindMessage, request: c, holdable: true,
		subject: reviewer.Subject{IssueKey: deref(c.IssueKey), InReplyTo: deref(c.Input.InReplyTo)},
		names:   []string{deref(c.IssueKey), deref(c.Input.InReplyTo)},
		text:    reviewer.Text{Kind: reviewer.KindMessage, Body: c.Input.Body},
	}
}

// reviewed is the new issue as the reviewer reads it: its title and its spec, under its parent.
// An issue imported from GitHub is not held: its title is GitHub's.
func (c issueCreate) reviewed() reviewedWrite {
	return reviewedWrite{
		kind: reviewer.KindIssue, request: c, project: c.Project, holdable: c.External == "",
		subject: reviewer.Subject{IssueKey: c.Parent}, names: []string{c.Parent},
		text: reviewer.Text{Kind: reviewer.KindIssue, Title: c.Title, Body: deref(c.Spec)},
	}
}

// heldParent is the held text a write names, which it waits on.
type heldParent struct {
	id, state, reader, project string
}

// heldParentOf is the held text, not yet posted, among the ids a write names; nil where it names
// none. Where it names more than one, the latest held is the one it waits on.
func (s *server) heldParentOf(ctx context.Context, names []string) (*heldParent, error) {
	named := slices.DeleteFunc(slices.Clone(names), func(name string) bool { return strings.TrimSpace(name) == "" })
	if len(named) == 0 {
		return nil, nil
	}
	var parent heldParent
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select id, state, coalesce(reader, ''), coalesce(project, '')
		from held_texts
		where id = any($1) and state in `+heldOpenStatesSQL+` and kind <> 'decision_block'
		order by seq desc limit 1`, named).Scan(&parent.id, &parent.state, &parent.reader, &parent.project)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the held texts a write names: %w", err)
	}
	return &parent, nil
}

// heldWriteOf is what reviewWrite decided of a write it holds.
type heldWriteOf struct {
	route, reader, project string
	parent                 *heldParent
	// reviewed is whether the reviewer reads the text before it posts.
	reviewed bool
	// event is the writing_check_events row of the write, its outcome left to set.
	event store.WritingCheckEvent
	// personTyped is a write a person typed in a session (Decision 26), which waits on parent and
	// is never reviewed.
	personTyped bool
}

// holdWrite stores write as a held text and answers the write: its id reserved (an issue's key
// from its project's sequence, else a uuid), its request, its text as written, its actor, writer,
// reader and project, and the held text it waits on. A text the reviewer reads is recorded held
// and its review started; one it does not waits, decided, for its parent to post. A text that
// waits on no held text first runs its whole write in a transaction that is rolled back (dryRunHeld),
// so a write its own path refuses is refused now, as with the reviewer off, and never fails at
// its post; a reply its route answers with no new text is answered so now, and nothing is held.
// The answer carries the advice the write's own answer carries (heldAdvice), worked out before
// the id is reserved, and, for a new issue, the assignee its post writes: the one its dry run filed
// it under, else heldIssueAssignee's.
func (s *server) holdWrite(w http.ResponseWriter, r *http.Request, actor model.Actor, write reviewedWrite, of heldWriteOf) {
	ctx := r.Context()
	request, err := json.Marshal(write.request)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	original, err := json.Marshal(write.text)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	author, err := json.Marshal(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	id := uuid.NewString()
	var dry *heldDryRun
	if of.parent == nil {
		ran, err := s.dryRunHeld(write, actor, id)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if own := ran.answered; own != nil {
			own.writeTo(w)
			return
		}
		dry = &ran
	}
	advice := s.heldAdvice(r, write, actor, dry, of.parent)
	answered := write
	if create, isIssue := write.request.(issueCreate); isIssue {
		if dry != nil {
			create.Assignee = dry.assignee
		} else if create.Assignee, err = s.heldIssueAssignee(ctx, create, actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		answered.request = create
	}
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if write.kind == reviewer.KindIssue {
		create := write.request.(issueCreate)
		number, err := reserveIssueNumber(ctx, tx, create.Project)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		id = fmt.Sprintf("%s-%d", create.Project, number)
	}
	state, outcome := heldReviewing, ""
	if !of.reviewed {
		state, outcome = heldPosting, heldNotReviewed
	}
	dependsOn := ""
	if of.parent != nil {
		dependsOn = of.parent.id
	}
	var writtenAt time.Time
	if err := tx.QueryRow(ctx, `
		insert into held_texts (id, kind, route, request, original, actor, writer, reader, project, issue_key,
			state, reviewed, outcome, depends_on, person_typed)
		values ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), nullif($9, ''), nullif($10, ''),
			$11, $12, nullif($13, ''), nullif($14, ''), $15)
		returning written_at`,
		id, string(write.kind), of.route, request, original, author, writerOf(actor), of.reader, of.project,
		write.subject.IssueKey, state, of.reviewed, outcome, dependsOn, of.personTyped).Scan(&writtenAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Each row of this write records the held text's id as the write's (writeIDOf): the check's
	// row of it, the reviewer's rows of the held text (recordHeld) and the review of it after its
	// post (afterHeldPost), so the counts count the text once.
	if kept := writingCheckRowsFrom(ctx); kept != nil {
		kept.write = id
	}
	if of.reviewed {
		event := of.event
		event.Outcome, event.WriteID = reviewHeld, id
		s.recordWritingCheck(ctx, event)
		// A text that waits on a held text is reviewed once that text posts (claimHeldReview,
		// afterHeldPost); this start takes it at once where the text posted before this row was
		// committed, and nothing otherwise.
		s.startReview(id)
	} else {
		// Its parent may have posted while this write was held beside it.
		s.detached.goDetached(s.postReady)
	}
	answer := heldState{State: state, WaitsOn: dependsOn}
	WriteJSON(w, http.StatusCreated, heldAnswerOf(answered, id, actor, writtenAt, answer, wantsDisplayTrees(r), advice))
}

// reserveIssueNumber takes the next number of project's sequence, as an issue create takes it,
// for an issue the reviewer holds: its key exists at once, and its post files it under that key.
// It takes the project's owner lock first, as every writer of a projects row does: an issue
// create reads the number it takes under that lock and writes it after its read phase
// (writeIssue), so a reservation made between the two would move the number under it.
func reserveIssueNumber(ctx context.Context, tx pgx.Tx, project string) (int, error) {
	if err := store.LockOwner(ctx, tx, store.ProjectOwner(project)); err != nil {
		return 0, err
	}
	var number int
	err := tx.QueryRow(ctx, `
		update projects set next_number = next_number + 1 where key = $1 returning next_number - 1
	`, project).Scan(&number)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errorf(http.StatusNotFound, "PROJECT_NOT_FOUND", "project %s not found", project)
	}
	return number, err
}

// heldIssueAssignee is the assignee the post of create, a new issue the reviewer holds beside a
// held text it names, which runs no dry run, writes (writeIssue): the person it names, else
// defaultAssignee's chain read as it stands now - the person actor acts for, the parent's
// assignee, the project's default assignee. A parent that is itself a held issue, which no issues
// row holds yet, gives the assignee its own post writes.
func (s *server) heldIssueAssignee(ctx context.Context, create issueCreate, actor model.Actor) (*string, error) {
	if create.Assignee != nil {
		return create.Assignee, nil
	}
	if login, acts := actorPerson(actor); acts {
		return &login, nil
	}
	var parentAssignee *string
	if create.Parent != "" {
		err := s.deps.Store.Pool.QueryRow(ctx, `select assignee from issues where key = $1`, create.Parent).Scan(&parentAssignee)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			parent, err := s.loadHeld(ctx, s.deps.Store.Pool, create.Parent, false)
			if err != nil {
				return nil, err
			}
			write, err := parent.write()
			if err != nil {
				return nil, err
			}
			if parentCreate, isIssue := write.request.(issueCreate); isIssue {
				if parentAssignee, err = s.heldIssueAssignee(ctx, parentCreate, parent.actor); err != nil {
					return nil, err
				}
			}
		case err != nil:
			return nil, err
		}
	}
	var projectDefault string
	if err := s.deps.Store.Pool.QueryRow(ctx, `select default_assignee from projects where key = $1`, create.Project).Scan(&projectDefault); err != nil {
		return nil, err
	}
	return defaultAssignee(actor, parentAssignee, projectDefault), nil
}

// heldState is the `held` object a held write's answer carries beside the write's own shape.
type heldState struct {
	// State is the held text's state: reviewing, awaiting_writer, posting, posted or failed.
	State string `json:"state"`
	// Deadline is when the writer's time to act ends, null while none runs.
	Deadline *time.Time `json:"deadline"`
	// WaitsOn is the held text this one posts after, "" for none.
	WaitsOn string `json:"waits_on,omitempty"`
}

// heldAnswer is a write's answer with the `held` object merged into it, as advice is merged
// (mergedJSON): the write's own shape, built from the held row, and what holds it.
type heldAnswer struct {
	payload any
	held    heldState
}

func (answer heldAnswer) MarshalJSON() ([]byte, error) {
	return mergedJSON(answer.payload, "held", answer.held)
}

// heldAnswerOf is the answer a held write gets: today's response shape for its kind, built from
// what the hold stored under the reserved id, with the `held` object. A new issue is in triage, as
// every create files it, under the assignee its create names (holdWrite sets the one its post
// writes). advice, where not nil, is the advice the write's own answer carries (heldAdvice),
// merged in as that answer merges it.
func heldAnswerOf(write reviewedWrite, id string, actor model.Actor, at time.Time, held heldState, trees bool, advice any) heldAnswer {
	var payload any = map[string]string{"id": id}
	switch create := write.request.(type) {
	case askCreate:
		ask := model.Ask{
			ID: id, IssueKey: create.Owner.IssueKey, ArtifactID: create.Owner.ArtifactID, Author: actor,
			Kind: create.Kind, Question: create.Question, Options: create.Options, Multiple: create.Multiple,
			Urgency: create.Urgency, State: "open", CreatedAt: at,
		}
		payload = newShapedAsk(ask, trees)
	case commentCreate:
		comment := model.Comment{
			ID: id, IssueKey: create.Owner.IssueKey, ArtifactID: create.Owner.ArtifactID, Author: actor,
			Body: create.Input.Body, ReplyTo: create.Input.ReplyTo, AskID: create.Input.AskID, CreatedAt: at,
			Mentions: []model.Mention{}, Deliveries: []model.CommentDelivery{},
		}
		if create.Input.Suggestion != nil {
			comment.Suggestion = &model.Suggestion{ReplaceWith: create.Input.Suggestion.ReplaceWith}
		}
		payload = commentWriteResponse{shapedComment: newShapedComment(comment, trees)}
	case messageCreate:
		message := model.Message{
			ID: id, IssueKey: create.IssueKey, Author: actor, Body: create.Input.Body, Target: create.Input.Target,
			InReplyTo: create.Input.InReplyTo, CreatedAt: at, Deliveries: []model.MessageDelivery{},
		}
		payload = newShapedMessage(message, trees)
	case issueCreate:
		number, _ := strconv.Atoi(strings.TrimPrefix(id, create.Project+"-"))
		labels := create.Labels
		if labels == nil {
			labels = []string{}
		}
		issue := model.Issue{
			Key: id, Project: create.Project, Number: number, Title: create.Title, Status: model.IssueStatusTriage,
			Priority: create.Priority, Labels: labels, Assignee: create.Assignee, BlockedBy: create.Blockers,
			CreatedBy: &actor, CreatedAt: at, UpdatedAt: at,
		}
		if issue.BlockedBy == nil {
			issue.BlockedBy = []string{}
		}
		if create.Parent != "" {
			issue.Parent = &create.Parent
		}
		payload = issue
	case heldReplay:
		payload = replayAnswerOf(write.kind, create, id, actor, at, trees)
	}
	if advice != nil {
		payload = withRawAdvice(payload, advice)
	}
	return heldAnswer{payload: payload, held: held}
}

// heldAdvice is the advice a held write's answer carries, which is the advice the write's own
// answer carries: a writer the reviewer holds is told what it would have been told without the
// hold. dry is what the write's dry run found, nil where the write waits on a held text (parent)
// and ran none; its write advice is then nil, as for a write with no issue state to report. An
// issue and an ask also run their related-item check here, before the id is reserved, as the
// write's own path runs it after its commit, and an issue's carries what the near-copy gate told
// its create (issueCreate.gate), the note of a check not made included. The check is recorded
// nowhere: its rows name the issue or the ask it was run for, which does not exist until the held
// text posts. Nil for a write whose answer carries no advice.
func (s *server) heldAdvice(r *http.Request, write reviewedWrite, actor model.Actor, dry *heldDryRun, parent *heldParent) any {
	var advice *writeAdvice
	if dry != nil {
		advice = dry.advice
	}
	ctx := r.Context()
	switch create := write.request.(type) {
	case issueCreate:
		// The spec as the seed would store it, which the write's own check reads; the spec as sent
		// where no dry run seeded it.
		markdown := ""
		switch {
		case dry != nil:
			markdown = dry.markdown
		case create.Spec != nil && strings.TrimSpace(*create.Spec) != "":
			markdown = *create.Spec
		}
		trees := s.readDisplayTrees(ctx, wantsDisplayTrees(r))
		suggestions := s.relatedCheck(ctx, relatedInput{
			project: create.Project,
			text:    create.Title + "\n" + markdown,
			title:   create.Title,
			source:  suggestionSource{kind: "issue", actor: actor},
			trees:   trees,
		})
		trees.resolve()
		create.gate.tell(suggestions)
		return suggestedAdvice(advice, suggestions)
	case askCreate:
		input := relatedInput{
			text: create.Question, title: create.Question, source: suggestionSource{kind: "ask", actor: actor},
			question: true,
		}
		if create.Owner.IssueKey != nil {
			input.source.issueKey = *create.Owner.IssueKey
		} else if create.Owner.ArtifactID != nil {
			input.source.artifactID = *create.Owner.ArtifactID
		}
		// An ask on a held issue searches that issue's project, which no issue row holds yet.
		if parent != nil {
			input.project = parent.project
		}
		input.trees = s.readDisplayTrees(ctx, wantsDisplayTrees(r))
		suggestions := s.relatedCheck(ctx, input)
		input.trees.resolve()
		return suggestedAdvice(advice, suggestions)
	case commentCreate, messageCreate:
		if advice != nil {
			return advice
		}
	}
	return nil
}

// heldDryRun is what a held write's dry run found (dryRunHeld): answered, the answer the route of
// a reply kept whole gave in place of a pass (a refusal, or the stored reply of an attempt already
// answered), which the write gets as its own; otherwise advice, the write advice the write's own
// answer would carry (nil where its path gives none), and, for a new issue, markdown, its spec as
// its seed would store it, and assignee, the person its create files it under.
type heldDryRun struct {
	answered *replayAnswer
	advice   *writeAdvice
	markdown string
	assignee *string
}

// dryRunHeld runs write as actor's under id in a transaction it rolls back before the commit
// (heldPost's dry run), so every check the kind's own path makes refuses it now. A kind's create
// path answers its refusal as an error, and a write that passed with the advice it would have
// answered. A reply kept whole passes where its route ends the dry run (errHeldDryRun), and any
// other answer of its route is the reply's own, in answered: a refusal, or a reply to an attempt
// already answered, which writes nothing and answers the stored reply as it does with the reviewer
// off (the reply routes' finishRepeat), so nothing is held for it.
func (s *server) dryRunHeld(write reviewedWrite, actor model.Actor, id string) (heldDryRun, error) {
	ctx := s.postContext()
	held := &heldPost{id: id, dryRun: true}
	var dry heldDryRun
	var err error
	switch create := write.request.(type) {
	case askCreate:
		tx, beginErr := s.begin(ctx)
		if beginErr != nil {
			return heldDryRun{}, beginErr
		}
		defer tx.Rollback(ctx)
		var written askWritten
		written, err = s.writeAsk(ctx, tx, create, actor, id, held)
		dry.advice = written.advice
	case issueCreate:
		tx, beginErr := s.begin(ctx)
		if beginErr != nil {
			return heldDryRun{}, beginErr
		}
		defer tx.Rollback(ctx)
		var written issueWritten
		written, err = s.writeIssue(ctx, tx, create, actor, 0, held)
		dry.advice, dry.markdown, dry.assignee = written.advice, written.markdown, written.assignee
	case commentCreate:
		var written commentWritten
		written, err = s.writeComment(ctx, create, actor, id, held)
		dry.advice = written.advice
	case messageCreate:
		_, dry.advice, err = s.writeMessage(ctx, create, actor, id, held)
	case heldReplay:
		// The reply's route reads what its hold learned of the writer's role (roleHolderReplyOf).
		held.roleHolder = create.RoleHolder
		answer, err := s.runReplay(ctx, create, held)
		switch {
		case err != nil:
			return heldDryRun{}, err
		case answer.isDryRunPass():
			return heldDryRun{}, nil
		}
		return heldDryRun{answered: answer}, nil
	default:
		// The request is no part of the text: a write's request can hold its caller (heldReplay's
		// Caller), and an error a handler logs carries nothing of an actor.
		return heldDryRun{}, errors.New("a held write of a kind this server has no path for cannot run")
	}
	if errors.Is(err, errHeldDryRun) {
		return dry, nil
	}
	if err == nil {
		return heldDryRun{}, errors.New("a held write's dry run committed")
	}
	return heldDryRun{}, err
}

// errHeldNotPosting is a post that found its held text no longer to be posted: another process
// posted it first, or it failed. A route that a held text's post runs again answers it 409
// HELD_NOT_POSTING, which the post reads back as this (postReplay).
var errHeldNotPosting = errorf(http.StatusConflict, "HELD_NOT_POSTING", "the held text is not waiting to post")

// heldPost is a held text's post inside the kind's create path, which takes its held row in the
// create's transaction and marks it posted in the same transaction. A nil *heldPost is the
// write's own path, which holds nothing. A dry run (dryRun) takes no row, and ends the create's
// transaction before its commit with errHeldDryRun.
//
// A create path that runs its two phases (INFRA-636) - a read phase, then one batch of its writes
// - takes and marks the row in one statement of that batch (queuePosted) and ends a dry run once
// the batch has run (dryRunEnds; dryRunEndsOn runs a ledger's batch first), so the post holds no
// transaction id while the create reads and does its document work. A route that writes as it
// goes takes the row with its first write (lock), once its reads and its planning are done, and
// marks it before its commit (finish).
type heldPost struct {
	id     string
	dryRun bool
	// roleHolder is what a held reply's hold learned of whether its writer holds the role the
	// attempt it answers was sent for (heldReplay.RoleHolder), which the reply route reads in place
	// of asking the listener again (roleHolderReplyOf); nil for any other text, and for a reply
	// held before the hold kept it, whose route asks.
	roleHolder *roleHolderReply
}

// postedHeldSQL marks the held text $1, waiting to post ($2, heldPosting), posted: its post time
// is the transaction's.
const postedHeldSQL = `
	update held_texts set state = 'posted', posted_at = now(), claimed_at = null, updated_at = now()
	where id = $1 and state = $2`

// queuePosted queues on b, the create's write phase, the held row's mark as posted, which takes
// the row: it must still be waiting to post, and a post that finds it posted by another process
// first, or failed, answers errHeldNotPosting and its transaction rolls back. Its post time is
// the transaction's, which the posted row carries as its creation time. Queue it before any other
// write of the phase, so a second post of the same text waits on the first's row and then stops
// there. A dry run queues nothing.
func (h *heldPost) queuePosted(b *store.WriteBatch) {
	if h == nil || h.dryRun {
		return
	}
	b.Queue(fmt.Sprintf("take held text %s for its post", h.id), postedHeldSQL, h.id, heldPosting).Exec(func(tag pgconn.CommandTag) error {
		if tag.RowsAffected() != 1 {
			return errHeldNotPosting
		}
		return nil
	})
}

// queuePostedOn is queuePosted as a step of ledger's write phase (docs.Ledger.Queue).
func (h *heldPost) queuePostedOn(ledger *docs.Ledger) error {
	if h == nil || h.dryRun {
		return nil
	}
	return ledger.Queue(func(b *store.WriteBatch) error {
		h.queuePosted(b)
		return nil
	})
}

// dryRunEnds ends a hold's dry run before its commit: errHeldDryRun, with which its create path
// answers the advice it worked out. It is nil for every other run.
func (h *heldPost) dryRunEnds() error {
	if h != nil && h.dryRun {
		return errHeldDryRun
	}
	return nil
}

// dryRunEndsOn ends a hold's dry run of a create path that queues its writes on ledger: it runs
// the queued write phase without committing (docs.Ledger.RunWritePhase), so each statement of the
// batch runs and a refusal of the write phase refuses the dry run, then answers errHeldDryRun as
// dryRunEnds does. It runs nothing, and is nil, for every other run, whose caller commits.
func (h *heldPost) dryRunEndsOn(ctx context.Context, ledger *docs.Ledger) error {
	if h == nil || !h.dryRun {
		return nil
	}
	if err := ledger.RunWritePhase(ctx); err != nil {
		return err
	}
	return h.dryRunEnds()
}

// lock takes the held row for the post of a route that writes as it goes: it must still be
// waiting to post.
func (h *heldPost) lock(ctx context.Context, tx pgx.Tx) error {
	if h == nil || h.dryRun {
		return nil
	}
	var state string
	err := tx.QueryRow(ctx, `select state from held_texts where id = $1 for update`, h.id).Scan(&state)
	if err != nil {
		return fmt.Errorf("take held text %s for its post: %w", h.id, err)
	}
	if state != heldPosting {
		return errHeldNotPosting
	}
	return nil
}

// answers reports whether this run of a kind's create path answers a writer, and so works out the
// write's advice: the write's own run (nil) and a hold's dry run, whose advice the hold's answer
// carries (heldAdvice). A held text's post answers nobody.
func (h *heldPost) answers() bool {
	return h == nil || h.dryRun
}

// finish marks the held row posted, in the create's transaction, for a route that took it with
// lock, which refused every state but posting: its post time is the transaction's, which the
// posted row carries as its creation time. A dry run ends here, before the commit (dryRunEnds),
// and its create path answers errHeldDryRun with the advice it worked out.
func (h *heldPost) finish(ctx context.Context, tx pgx.Tx) error {
	if h == nil {
		return nil
	}
	if err := h.dryRunEnds(); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, postedHeldSQL, h.id, heldPosting)
	return err
}

// finishRepeat is finish for a reply route's answer to an attempt already answered, which writes
// no text and answers the stored reply: a held reply's post marks its row posted with it, as
// finish does, and a dry run goes on to that answer rather than end as a pass, so the reply is
// answered as it is with the reviewer off (dryRunHeld passes it back) and no text is held for it.
func (h *heldPost) finishRepeat(ctx context.Context, tx pgx.Tx) error {
	if h != nil && h.dryRun {
		return nil
	}
	return h.finish(ctx, tx)
}

// claimHeldReview takes the held text id for a review: it must be in review, with no review
// holding it or one held past staleClaim, and the held text it waits on, if any, must have posted.
// A text whose parent has not posted is read in a record that misses that parent (a new ask, a
// comment or a child on a held issue), so its review waits for the parent's post, which starts it
// (afterHeldPost).
func (s *server) claimHeldReview(ctx context.Context, id string) (heldRow, bool, error) {
	row, err := scanHeld(s.deps.Store.Pool.QueryRow(ctx, `
		update held_texts h set claimed_at = now()
		where h.id = $1 and h.state = 'reviewing'
			and (h.claimed_at is null or h.claimed_at < now() - $2::interval)
			and (h.depends_on is null or exists (
				select 1 from held_texts p where p.id = h.depends_on and p.state = 'posted'))
		returning `+heldColumns, id, intervalOf(s.deps.Reviewer.staleClaim())))
	if errors.Is(err, pgx.ErrNoRows) {
		return heldRow{}, false, nil
	}
	return row, err == nil, err
}

// heldDecision is what posts of a held text: the outcome, and the detail and the writer's reason
// the events row records.
type heldDecision struct {
	outcome, detail, reason string
}

// decideHeld decides what of the held text row posts and posts it: the row is set posting with
// the outcome where it is still in review or awaiting its writer at the wording the decision was
// made of, the outcome is recorded, and the text and every text whose turn comes after it post.
// A decision block's rewrite the block cannot hold is decided as the block written
// (applicableDecision).
func (s *server) decideHeld(ctx context.Context, row heldRow, decision heldDecision) {
	decision = s.applicableDecision(ctx, row, decision)
	tag, err := s.deps.Store.Pool.Exec(ctx, `
		update held_texts
		set state = 'posting', outcome = $3, detail = nullif($4, ''), reason = nullif($5, ''),
			deadline = null, claimed_at = null, updated_at = now()
		where id = $1 and revision = $2 and state in ('reviewing', 'awaiting_writer')`,
		row.id, row.revision, decision.outcome, decision.detail, decision.reason)
	if err != nil {
		slog.Warn("dispatch: decide a held text", "text", row.id, "outcome", decision.outcome, "error", err)
		return
	}
	if tag.RowsAffected() == 0 {
		return
	}
	detail := decision.detail
	if decision.reason != "" {
		detail = decision.reason
	}
	var rules []string
	if decision.outcome != reviewPassed && decision.outcome != reviewPostedUnreviewed {
		rules = findingRuleSets(row.findings)
	}
	s.recordHeld(ctx, row, decision.outcome, rules, detail)
	s.postReady()
}

// postReady posts every held text that is decided and whose turn has come: the text it waits on,
// if any, has posted, and every text that waits on that one and came before it has posted or
// failed. Each posts in the order the writes came. It runs on the post context (postContext),
// whatever set it off, so no request's values reach a post.
func (s *server) postReady() {
	if s.deps.Store == nil {
		return
	}
	ctx := s.postContext()
	for range 20 {
		rows, err := s.deps.Store.Pool.Query(ctx, `
			select h.id from held_texts h
			left join held_texts p on p.id = h.depends_on
			where h.state = 'posting'
				and (h.claimed_at is null or h.claimed_at < now() - $1::interval)
				and (h.depends_on is null or p.state = 'posted')
				and not exists (
					select 1 from held_texts e
					where e.depends_on = h.depends_on and e.seq < h.seq and e.state in `+heldOpenStatesSQL+`)
			order by h.seq limit 50`, intervalOf(s.deps.Reviewer.staleClaim()))
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("dispatch: read the held texts ready to post", "error", err)
			}
			return
		}
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		if len(ids) == 0 {
			return
		}
		posted := 0
		for _, id := range ids {
			if s.postHeld(ctx, id) {
				posted++
			}
		}
		if posted == 0 {
			return
		}
	}
}

// postHeld posts the held text id, which is decided: it takes the row for the post, runs the
// kind's create path with the stored request (the reviewer's rewrite in its wording where the
// outcome is rewrite_posted), the recorded actor and the reserved id, and reports whether it
// posted. A post refused - an error its route would answer below 500 (handlerRefusalOf): the
// create path's own refusal, and a document's (a quote anchor gone, a document too large, a row
// gone), which no later post changes - fails the held text with the refusal, and its writer is
// told (failHeld). A refusal a later post can clear (heldPostRetried), and any other failure,
// leave it for the sweep to post again.
//
// Each of its writes commits in a transaction of the shared pool. ctx is the post context
// (postContext), which carries no request's Idempotency-Key, so those transactions take no commit
// step of a request's: the post is the held text's own work, which the reviewer's sweep makes when
// no request does, and never a write of the request that set it off, PUT /api/v1/held/{id}/state's
// among them. The one step a post's transaction takes is its own mark of a kept write's row
// (postReplay).
func (s *server) postHeld(ctx context.Context, id string) bool {
	row, err := s.claimHeldPost(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: take a held text for its post", "text", id, "error", err)
		}
		return false
	}
	err = s.runHeldPost(ctx, row)
	switch {
	case err == nil:
		s.afterHeldPost(ctx, row)
		return true
	case errors.Is(err, errHeldNotPosting):
		return false
	}
	if refusal := handlerRefusalOf(err); refusal.status < http.StatusInternalServerError && !heldPostRetried(refusal) {
		refused := refusal.apiError()
		slog.Warn("dispatch: a held text's post was refused", "text", row.id, "code", refused.code, "error", refused.message)
		s.failHeld(ctx, row, refused)
		return false
	}
	if ctx.Err() == nil {
		slog.Warn("dispatch: post a held text", "text", row.id, "error", err)
	}
	if releaseErr := s.releaseHeldPost(context.WithoutCancel(ctx), row.id); releaseErr != nil {
		slog.Warn("dispatch: release a held text's post", "text", row.id, "error", releaseErr)
	}
	return false
}

// heldPostRetried reports whether refusal, a held text's post's, is one a later post can clear,
// so the text is left for the sweep to post again rather than failed: a 429 (EDIT_QUEUE_FULL, a
// document's queue of conditional edits full for now), DOC_NEWER_CONTENT (a newer build wrote the
// document while two builds serve one database: the post goes through on that build, and on this
// one once it is replaced) and DOCUMENT_UNLOADABLE (a history that does not load, the state a
// rebuild repairs).
func heldPostRetried(refusal handlerRefusal) bool {
	return refusal.status == http.StatusTooManyRequests || refusal.code == codeDocNewerContent ||
		refusal.code == codeDocumentUnloadable
}

// claimHeldPost takes the held text id for its post: it must be posting, with no post holding it
// or one held past staleClaim. It answers pgx.ErrNoRows where the text is not one to take.
func (s *server) claimHeldPost(ctx context.Context, id string) (heldRow, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return heldRow{}, err
	}
	defer tx.Rollback(ctx)
	row, err := scanHeld(tx.QueryRow(ctx, `
		update held_texts set claimed_at = now()
		where id = $1 and state = 'posting' and (claimed_at is null or claimed_at < now() - $2::interval)
		returning `+heldColumns, id, intervalOf(s.deps.Reviewer.staleClaim())))
	if err != nil {
		return heldRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return heldRow{}, err
	}
	return row, nil
}

// failHeld fails row, a held text still posting whose post was answered refused, and tells its
// writer the text was not posted, with the refusal (tellNotPosted). Every held text that waits on
// it fails with it at once, and each of their writers is told too (failOrphanedHeld).
func (s *server) failHeld(ctx context.Context, row heldRow, refused *apiError) {
	failed, err := s.failHeldPost(ctx, row.id, "the post was refused: "+refused.code+": "+refused.message)
	if err != nil {
		slog.Warn("dispatch: fail a held text", "text", row.id, "error", err)
		return
	}
	if failed {
		s.tellNotPosted(ctx, row.writer, reviewer.PostFailedFrame{
			ID: row.id, Kind: reviewer.Kind(row.kind), Code: refused.code, Message: refused.message,
		})
	}
	if err := s.failOrphanedHeld(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: fail the held texts that wait on a failed one", "error", err)
	}
}

// failHeldPost fails the held text id, still posting, with detail: its post was refused. It
// reports whether this call failed it, so its writer is told once.
func (s *server) failHeldPost(ctx context.Context, id, detail string) (bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		update held_texts set state = 'failed', detail = $2, claimed_at = null, updated_at = now()
		where id = $1 and state = 'posting'`, id, detail)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, tx.Commit(ctx)
}

// tellNotPosted tells writer, the session that wrote the held text frame names, that Dispatch did
// not post it (reviewer.PostFailedFrame), through tellWriter.
func (s *server) tellNotPosted(ctx context.Context, writer string, frame reviewer.PostFailedFrame) {
	body, err := frame.Body()
	if err != nil {
		slog.Warn("dispatch: write the frame of a held text not posted", "text", frame.ID, "error", err)
		return
	}
	s.tellWriter(ctx, systemFrame{
		id: frame.ID, body: body, writer: writer, key: "held-not-posted:" + frame.ID, mode: "steer",
		expects: contracts.ExpectsReplyUnset,
	}, "")
}

// tellNearCopy tells the writer of row, a held issue that posted, that it posted as a near copy of
// copies, the issues the gate run again at its post found (recheckNearCopy), so it can fold one
// into the other (reviewer.NearCopyFrame), through tellWriter; nothing where copies is empty.
func (s *server) tellNearCopy(ctx context.Context, row heldRow, copies []model.DuplicateCandidate) {
	if len(copies) == 0 {
		return
	}
	frame := reviewer.NearCopyFrame{ID: row.id}
	keys := make([]string, 0, len(copies))
	for _, candidate := range copies {
		frame.Copies = append(frame.Copies, reviewer.NearCopy{Key: candidate.Key, Title: candidate.Title})
		keys = append(keys, candidate.Key)
	}
	body, err := frame.Body()
	if err != nil {
		slog.Warn("dispatch: write the frame of a held issue posted as a near copy", "text", row.id, "error", err)
		return
	}
	s.tellWriter(ctx, systemFrame{
		id: row.id, body: body, writer: row.writer, key: "held-near-copy:" + row.id, mode: "steer",
		expects: contracts.ExpectsReplyUnset,
	}, "posted as a near copy of "+strings.Join(keys, ", "))
}

// tellWriter sends frame, a notice about the held text frame.id that wants no reply, to its writer
// session, as the findings frame is sent (sendSystemFrame), and adds to the held text's detail
// about, where given, and whether its writer was told: a text with no writer session, or whose
// writer session has gone, gets no frame, and a send that failed is named with its error. A
// notice leaves expects_reply out (ExpectsReplyUnset) rather than write the literal false, which a
// listener built before #2903 answers 400 to (commentExpectsReply says why).
func (s *server) tellWriter(ctx context.Context, frame systemFrame, about string) {
	told := "its writer was told"
	switch {
	case frame.writer == "":
		told = "its writer was not told: it has no writer session"
	case s.writerGone(ctx, frame.writer):
		told = "its writer was not told: its writer session has gone"
	default:
		if err := s.sendSystemFrame(ctx, frame); err != nil {
			slog.Warn("dispatch: tell a held text's writer", "text", frame.id, "session", frame.writer, "error", err)
			told = "its writer was not told: " + err.Error()
		}
	}
	note := told
	if about != "" {
		note = about + "; " + told
	}
	if err := s.noteHeld(ctx, frame.id, note); err != nil {
		slog.Warn("dispatch: record what a held text's writer was told", "text", frame.id, "error", err)
	}
}

// noteHeld adds note to the held text id's detail.
func (s *server) noteHeld(ctx context.Context, id, note string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		update held_texts set detail = concat_ws('; ', detail, $2::text), updated_at = now() where id = $1`, id, note); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// releaseHeldPost gives the held text id, still posting, back for the next post to take.
func (s *server) releaseHeldPost(ctx context.Context, id string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		update held_texts set claimed_at = null where id = $1 and state = 'posting'`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// runHeldPost runs the held text's create path. A write a person typed posts with its events
// marked theirs (store.WithPersonTyped).
func (s *server) runHeldPost(ctx context.Context, row heldRow) error {
	write, err := row.write()
	if err != nil {
		return errorf(http.StatusUnprocessableEntity, "HELD_REQUEST", "the held text's request could not be read: %v", err)
	}
	if row.outcome == reviewRewritePosted && row.rewrite != nil {
		write = withRewrite(write, *row.rewrite)
	}
	if row.personTyped {
		ctx = store.WithPersonTyped(ctx)
	}
	held := &heldPost{id: row.id}
	switch create := write.request.(type) {
	case askCreate:
		tx, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		written, err := s.writeAsk(ctx, tx, create, row.actor, row.id, held)
		if err != nil {
			return err
		}
		s.announceAsk(ctx, written)
	case issueCreate:
		number, err := strconv.Atoi(strings.TrimPrefix(row.id, create.Project+"-"))
		if err != nil || number < 1 {
			return errorf(http.StatusUnprocessableEntity, "HELD_REQUEST", "held issue %s names no number of project %s", row.id, create.Project)
		}
		create, copies := s.recheckNearCopy(ctx, create)
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
	case commentCreate:
		if _, err := s.writeComment(ctx, create, row.actor, row.id, held); err != nil {
			return err
		}
	case messageCreate:
		if _, _, err := s.writeMessage(ctx, create, row.actor, row.id, held); err != nil {
			return err
		}
	case heldReplay:
		held.roleHolder = create.RoleHolder
		return s.postReplay(ctx, row, create, held)
	case decisionBlockHold:
		return s.releaseDecisionBlock(ctx, row, held)
	}
	return nil
}

// afterHeldPost is what follows a held text's post: each held text in review that waits on it has
// its review started, which waited for this post (claimHeldReview, reviewDependents); and a text
// the reviewer did not read before it posted is reviewed after the post where its kind's setting
// says so.
func (s *server) afterHeldPost(ctx context.Context, row heldRow) {
	s.reviewDependents(ctx, row.id)
	if row.reviewed || row.reader == "" || row.kind == heldRequestKind || s.deps.Reviewer.setting(reviewer.Kind(row.kind)) == ReviewOff {
		return
	}
	if checked, _ := writingCheckReads(row.actor); !checked {
		return
	}
	write, err := row.write()
	if err != nil {
		return
	}
	method, route, _ := strings.Cut(row.route, " ")
	subject := write.subject
	subject.Text, subject.Writer, subject.Reader = write.text, row.writer, row.reader
	event := store.WritingCheckEvent{
		Route: route, Method: method, ActorKind: row.actor.Kind, ActorID: row.actor.ID, Reader: row.reader, Project: row.project,
		WriteID: row.id,
	}
	if row.actor.Act != nil {
		event.ActSession, event.TokenID = row.actor.Act.Session, row.actor.Act.TokenID
	}
	s.reviewPosted(ctx, &postedReview{kind: write.kind, text: write.text, subject: subject, event: event}, row.id)
}

// reviewDependents starts the review of each held text in review that waits on id, a held text
// that has posted, in the order they were held.
func (s *server) reviewDependents(ctx context.Context, id string) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select id from held_texts where depends_on = $1 and state = 'reviewing' order by seq`, id)
	if err == nil {
		var dependents []string
		if dependents, err = pgx.CollectRows(rows, pgx.RowTo[string]); err == nil {
			for _, dependent := range dependents {
				s.startReview(dependent)
			}
			return
		}
	}
	if ctx.Err() == nil {
		slog.Warn("dispatch: read the held texts in review that wait on a posted one", "text", id, "error", err)
	}
}

// withRewrite is write with the wording of rewrite in place of its own: an ask's question and
// options, an issue's title and spec, a comment's or a message's body. The options keep no id: a
// stored ask's option ids are the index's to mint (reviewer.OptionWords).
func withRewrite(write reviewedWrite, rewrite reviewer.Text) reviewedWrite {
	switch create := write.request.(type) {
	case askCreate:
		create.Question = rewrite.Body
		if rewrite.Options != nil {
			create.Options = reviewer.OptionWords(rewrite.Options)
		}
		return create.reviewed()
	case issueCreate:
		create.Title = rewrite.Title
		spec := rewrite.Body
		create.Spec = &spec
		return create.reviewed()
	case commentCreate:
		create.Input.Body = rewrite.Body
		return create.reviewed()
	case messageCreate:
		create.Input.Body = rewrite.Body
		return create.reviewed()
	case heldReplay:
		if edited, err := create.withBody(rewrite.Body); err == nil {
			return edited.reviewed(write.kind)
		}
	}
	return write
}

// failOrphanedHeld fails each held text that waits on one that failed, which can never post, and
// tells each one's writer it was not posted, naming the held text it waited on (tellNotPosted).
func (s *server) failOrphanedHeld(ctx context.Context) error {
	for range 10 {
		orphans, err := s.failWaitingOnFailed(ctx)
		if err != nil || len(orphans) == 0 {
			return err
		}
		for _, orphan := range orphans {
			s.tellNotPosted(ctx, orphan.writer, reviewer.PostFailedFrame{
				ID: orphan.id, Kind: reviewer.Kind(orphan.kind), WaitedOn: orphan.waitedOn,
			})
		}
	}
	return nil
}

// heldOrphan is a held text failWaitingOnFailed failed: its id, kind and writer, and the held text
// it waited on.
type heldOrphan struct {
	id, kind, writer, waitedOn string
}

// failWaitingOnFailed fails, in one statement, each held text that waits on one that failed, and
// answers them: each is failed by this call alone, so its writer is told once.
func (s *server) failWaitingOnFailed(ctx context.Context) ([]heldOrphan, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		update held_texts h
		set state = 'failed', detail = 'the held text it waits on, ' || p.id || ', failed to post',
			claimed_at = null, updated_at = now()
		from held_texts p
		where p.id = h.depends_on and p.state = 'failed' and h.state in `+heldOpenStatesSQL+`
		returning h.id, h.kind, coalesce(h.writer, ''), p.id`)
	if err != nil {
		return nil, err
	}
	var orphans []heldOrphan
	for rows.Next() {
		var orphan heldOrphan
		if err := rows.Scan(&orphan.id, &orphan.kind, &orphan.writer, &orphan.waitedOn); err != nil {
			rows.Close()
			return nil, err
		}
		orphans = append(orphans, orphan)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return orphans, tx.Commit(ctx)
}

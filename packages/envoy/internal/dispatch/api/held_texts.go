package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// heldReviewWaitsSQL holds where the held text h reads the text it waits on (depends_on): a write
// kept beside the held text its path names (awaitHeldTarget), and a write whose request names that
// text (a child of a held issue, an ask or a comment on one, a reply to a held comment, a reply
// after its writer's earlier reply to the same delivery, heldReplay.EarlierReply). Its review
// waits for that text to post (claimHeldReview), and it fails where that text fails
// (failWaitingOnFailed). A text that waits only for its turn behind its writer's earlier held text
// at its place (heldBeforeOf) names none: its review runs at once, only its post waits for its
// turn, and where that text fails it waits on what that text waited on.
const heldReviewWaitsSQL = `(h.kind = '` + heldRequestKind + `' or
	jsonb_path_exists(h.request, '$.** ? (@ == $id)', jsonb_build_object('id', h.depends_on)))`

// heldNamedOpenSQL holds where the held text h names, beside the text it waits on (depends_on), a
// held text held before it that has neither posted nor failed and that its post reads: a new
// issue's blocker (issueCreate.Blockers) and an issue a kept write's body names (heldReplay.Names,
// a PATCH's blocked_by, parent and rank neighbours). Its post waits until each has posted or
// failed (postReady), as the write it runs refuses a blocker, a parent or a neighbour no row holds;
// a text held after h, which h could not have named, is none of them. Each is read by its primary
// key.
const heldNamedOpenSQL = `exists (
	select 1 from held_texts n
	where n.id in (
			select jsonb_array_elements_text(h.request->'blockers')
			union all select jsonb_array_elements_text(h.request->'names'))
		and n.state in ` + heldOpenStatesSQL + ` and n.seq < h.seq)`

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

// reviewed is the new issue as the reviewer reads it: its title and its spec, under its parent,
// waiting on its blockers where a held text holds one. An issue imported from GitHub is not held:
// its title is GitHub's.
func (c issueCreate) reviewed() reviewedWrite {
	return reviewedWrite{
		kind: reviewer.KindIssue, request: c, project: c.Project, holdable: c.External == "",
		subject: reviewer.Subject{IssueKey: c.Parent}, names: []string{c.Parent}, waits: c.Blockers,
		text: reviewer.Text{Kind: reviewer.KindIssue, Title: c.Title, Body: deref(c.Spec)},
	}
}

// heldParent is the held text a write names, which it waits on.
type heldParent struct {
	id, state, reader, project string
}

// heldParentOf is the held text, not yet posted, among the ids a write names, nil where it names
// none held: the newest held one of names, and where none of names is held, the newest held one of
// waits, the ids beside them its post reads (reviewedWrite.waits, heldBodyNames). One read answers
// both.
func (s *server) heldParentOf(ctx context.Context, names, waits []string) (*heldParent, error) {
	ids := slices.DeleteFunc(slices.Concat(names, waits), func(id string) bool { return strings.TrimSpace(id) == "" })
	if len(ids) == 0 {
		return nil, nil
	}
	var parent heldParent
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select id, state, coalesce(reader, ''), coalesce(project, '')
		from held_texts
		where id = any($1) and state in `+heldOpenStatesSQL+` and kind <> 'decision_block'
		order by coalesce(id = any($2), false) desc, seq desc limit 1`, ids, names).Scan(&parent.id, &parent.state, &parent.reader, &parent.project)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the held texts a write names: %w", err)
	}
	return &parent, nil
}

// heldBeforeOf is the newest held text, not yet posted, that writer wrote to place
// (heldRequest.place), "" where it holds none there or place is "": the text a held ask, comment,
// message, reply or new issue posts after, so one writer's held texts at one place post in the
// order written, whichever review ends first. It reads the id, the kind and the request of the
// writer's open texts of the kinds that post at such a place, newest first - new issues at a
// project's place (issueCreate.place), asks, comments and messages (replies among them) at any
// other (placeOf, messagePlaceOf) - and stops at the first one there.
func (s *server) heldBeforeOf(ctx context.Context, writer, place string) (string, error) {
	if writer == "" || place == "" {
		return "", nil
	}
	kinds := []string{string(reviewer.KindAsk), string(reviewer.KindComment), string(reviewer.KindMessage)}
	if strings.HasPrefix(place, projectPlace) {
		kinds = []string{string(reviewer.KindIssue)}
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select id, kind, request from held_texts
		where writer = $1 and state in `+heldOpenStatesSQL+` and kind = any($2)
		order by seq desc`, writer, kinds)
	if err != nil {
		return "", fmt.Errorf("read the writer's held texts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var held heldRow
		if err := rows.Scan(&held.id, &held.kind, &held.request); err != nil {
			return "", fmt.Errorf("read the writer's held texts: %w", err)
		}
		if write, err := held.write(); err == nil && write.request.place() == place {
			return held.id, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read the writer's held texts: %w", err)
	}
	return "", nil
}

// heldWriteOf is what reviewWrite decided of a write it holds.
type heldWriteOf struct {
	route, reader, project string
	parent                 *heldParent
	// before is the writer's held text at the place the write posts to (heldBeforeOf), which it
	// posts after; "" for none, and where parent is set.
	before string
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
// reader and project, and the held text it waits on: its parent, else the writer's held text at
// its place (of.before). A text the reviewer reads is recorded held and its review started; one it
// does not waits, decided, for its parent to post. A text that names no held text first runs its
// whole write in a transaction that is rolled back (dryRunHeld), so a write its own path refuses
// is refused now, as with the reviewer off, and never fails at its post; a reply its route
// answers with no new text is answered so now, and nothing is held. An ask, comment, message or
// reply beside a held parent, which no dry run can write before that parent posts, runs none, and
// reads what its post writes off the held texts it waits on instead (heldBeside), read once
// (heldWaitedBeside), but for a reply whose route writes it then as it writes it now, which runs
// its dry run. A new issue beside a held parent runs its dry run at its hold, with no parent and
// none of the held issues it waits on among its blockers (heldIssueCreateError), and reads off it
// the spec its seed renders, and off the held texts it waits on its assignee and its advice
// (issueCreate.beside). Each is refused now where its post would refuse it. A text that waits
// only for its turn counts in its advice the held texts that post before it, as its post will
// find them posted (countWaited); a new issue's advice is a new issue's, which none of them
// writes to.
// The answer carries the advice the write's own answer carries (heldAdvice), worked out before
// the id is reserved, and, for a new issue, the assignee its post writes, which the dry run or
// heldBeside read (heldDryRun.assignee). What else the post writes it reads off the dry run, or
// off heldBeside's reading (heldAnswerOf).
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
	var waited *heldChain
	if of.parent != nil {
		if waited, err = s.heldWaitedBeside(ctx, write, of.parent.id); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var dry *heldDryRun
	if waited == nil {
		if dry = s.heldDryRunOrAnswer(w, write, actor, id); dry == nil {
			return
		}
		// The dry run read the issue as it stands; the post finds it once the held texts that post
		// before this one (of.before, each it waits on, and each that waits on the same text as one
		// of those and came first) have posted.
		if of.before != "" && dry.advice != nil && write.kind != reviewer.KindIssue {
			turn, err := s.heldWaitedOn(ctx, of.before)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			countWaited(dry.advice, write.subject.IssueKey, actor, turn.first)
		}
	} else if dry, err = s.heldBeside(ctx, write, actor, id, *waited); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	advice := s.heldAdvice(r, write, actor, dry, of.parent)
	answered := write
	if create, isIssue := write.request.(issueCreate); isIssue {
		create.Assignee = dry.assignee
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
	dependsOn := of.before
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
		// A text whose review reads the held text it waits on is reviewed once that text posts
		// (claimHeldReview, afterHeldPost); this start takes it at once where the text posted
		// before this row was committed, or where it waits only for its turn at its place
		// (of.before), and nothing otherwise.
		s.startReview(id)
	} else {
		// Its parent may have posted while this write was held beside it.
		s.detached.goDetached(s.postReady)
	}
	answer := heldState{State: state, WaitsOn: dependsOn}
	WriteJSON(w, http.StatusCreated, heldAnswerOf(answered, id, actor, writtenAt, answer, dry, wantsDisplayTrees(r), advice))
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

// heldIssueAssignee is the assignee the post of create, a new issue whose parent may be a held
// issue among waited (the held texts a text beside it waits on), writes (writeIssue): the person
// it names, else defaultAssignee's chain - the person actor acts for, the parent's assignee, the
// project's default assignee. A parent that is a held issue, which no issues row holds yet, gives
// the assignee its own post writes, so the chain climbs the held issues above create (issuesFrom)
// to the first that names a person or whose writer acts for one, else to the issue a row holds
// above them, whose assignee is read on q as it stands now, as the project's default is.
func (s *server) heldIssueAssignee(ctx context.Context, q queryer, create issueCreate, actor model.Actor, waited heldChain) (*string, error) {
	if create.Assignee != nil {
		return create.Assignee, nil
	}
	if login, acts := actorPerson(actor); acts {
		return &login, nil
	}
	above, from := waited.issuesFrom(create.Parent)
	for _, held := range above {
		if held.create.Assignee != nil {
			return held.create.Assignee, nil
		}
		if login, acts := actorPerson(held.actor); acts {
			return &login, nil
		}
	}
	var parentAssignee *string
	if from != "" {
		if err := q.QueryRow(ctx, `select assignee from issues where key = $1`, from).Scan(&parentAssignee); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	var projectDefault string
	if err := q.QueryRow(ctx, `select default_assignee from projects where key = $1`, create.Project).Scan(&projectDefault); err != nil {
		return nil, err
	}
	return defaultAssignee(actor, parentAssignee, projectDefault), nil
}

// heldBeside is what the post of write, a text beside a held parent that waits on waited
// (heldWaitedBeside), writes and answers, which no dry run can write before that parent posts, in
// a dry run's shape for heldAnswerOf and heldAdvice: what its kind reads (heldRequest.beside) off
// the records of the held texts it waits on and, where a text it names has posted or is no held
// text, off that text's row as its post reads it, with the write advice its post answers, all in
// one read transaction it rolls back once. That transaction opens after a new issue's dry run
// (heldIssueCreateError), the one dry run such a reading runs, which takes a connection of its own
// (dryRunHeld): a request that held the transaction's connection while it waited for another
// could wait on the shared pool for ever, which store.ErrNestedAcquire cannot refuse here, the dry
// run's context carrying a mark of its own. It reads:
//   - an ask: whom it waits on - on a held issue, the assignee that issue's post writes
//     (heldIssueAssignee) - and an anchor in a held issue's spec (heldSpecAnchor), with the
//     document it quotes;
//   - a comment: the thread it joins (heldBesideThread), so its reply_to, its ask_id, the comment
//     it answers, its turn and whom its ask waits on once it posts; the block reply_to_block
//     names in the comment reply_to names, else in the ask ask_id names (heldNamedBlock); and an
//     anchor in a held issue's spec;
//   - a message: the block in_reply_to_block names in the message it answers (heldNamedBlock);
//   - a new issue: its assignee, and its spec as its seed renders it, which its related-item check
//     and its advice read off its dry run (heldIssueCreateError);
//   - a follow-up reply whose attempt its writer's earlier held reply answers: the reply it is
//     filed under and the delivery's issue and target (heldFollowUpBeside).
//
// It leaves out what only the posts make: the anchor's document id, block and version in a held
// issue's spec, and the whole anchor where a write kept beside that issue posts first
// (heldSpecAnchor); the node id of the block a reply names in a held text, which only that text's
// index makes; and the mentions a comment's post resolves from the sessions the listener finds
// then.
//
// A write its post would refuse is refused now, as a dry run refuses a write that names no held
// text, with the refusal its post gives, in the order its post makes the checks: what the
// request says alone (commentThreadInputError, anchorInputError, an ask_id that is no ask id); an
// item it names, a held one among waited off its record and any other as its post reads it (a
// comment or an ask on another owner, a message on another issue, a block the item does not
// hold); its turn (commentTurnError); an anchor in a held issue's spec, against the spec that
// issue's post seeds (heldSpecAnchor); a new issue's blockers, parent and every other check of
// its post (heldIssueCreateError); and a follow-up that names a block (heldFollowUpBeside).
func (s *server) heldBeside(ctx context.Context, write reviewedWrite, actor model.Actor, id string, waited heldChain) (*heldDryRun, error) {
	hold := besideHold{actor: actor, id: id, waited: waited}
	if create, isIssue := write.request.(issueCreate); isIssue {
		ran, err := s.heldIssueCreateError(ctx, s.deps.Store.Pool, create, actor, id, waited)
		if err != nil {
			return nil, err
		}
		hold.ran = ran
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	hold.tx = tx
	return write.request.beside(s, ctx, hold)
}

// besideHold is what the hold of a text beside a held parent reads what its post writes with
// (heldRequest.beside): the hold's one read transaction, which heldBeside rolls back once, the
// text's actor and its reserved id, the held texts it waits on, and for a new issue the dry run
// heldBeside ran before it opened that transaction (heldIssueCreateError). A beside reads on tx
// alone: nothing it calls takes another connection while tx is open.
type besideHold struct {
	tx     pgx.Tx
	actor  model.Actor
	id     string
	waited heldChain
	ran    heldDryRun
}

// heldIssueAdvice is the write advice the post of a text on issue, beside a held parent, answers
// for route (writeAdvice), counting appending, the event that post appends, and asks, what it
// moves of the issue's open asks, and opens, whether it puts a question to a person: as that post
// will find the issue once the held texts it waits on have posted, a posted issue as it now
// stands, and a held one as a new issue in triage, under its labels and parent chain
// (heldIssueRow) and with no review of its spec; and each held text that posts first and that the
// same session writes to that issue counted as written (countWaited). Nil for a text on no issue
// (one on a document), and on an issue neither posted nor held, which its post refuses.
func (s *server) heldIssueAdvice(ctx context.Context, hold besideHold, issue, route, appending string, asks adviceAsks, opens bool) (*writeAdvice, error) {
	if issue == "" {
		return nil, nil
	}
	var status string
	var row *adviceIssueRow
	switch err := hold.tx.QueryRow(ctx, `select status from issues where key = $1`, issue).Scan(&status); {
	case errors.Is(err, pgx.ErrNoRows):
		create, _, held := hold.waited.issue(issue)
		if !held {
			return nil, nil
		}
		created := heldIssueRow(create, hold.waited)
		status, row = model.IssueStatusTriage, &created
	case err != nil:
		return nil, err
	}
	advice := s.adviseWrite(ctx, hold.tx, route, issue, hold.actor, asks, status, appending, opens, row)
	if advice == nil {
		return nil, nil
	}
	if opens && row == nil {
		advice.StandingReview = s.standingReviewAdvice(ctx, hold.tx, nil, route, issue)
	}
	countWaited(advice, issue, hold.actor, hold.waited.first)
	return advice, nil
}

// heldSpecAnchor is the anchor the post of a text anchored in create, a held issue, writes
// (resolveAnchor), with kind the mark it takes there, read before that issue's post seeds its
// spec, or the refusal that post gives it, in its order: the anchor's own fields
// (anchorInputError); then, unless a write kept beside that issue posts before the text (kept),
// which can make the document or the quote the anchor names and so leaves both to the text's post,
// any document but the spec, the one document that issue's post makes, named by its slug since its
// id is minted then, which loadAnchorArtifact finds none of; a mark id, since no mark stands in a
// document not yet seeded (docs.ErrAnchorMissing, as VerifyMark answers one it does not find); and
// the quote, as the mark of kind takes it in the spec that post seeds (docs.QuoteInSeed). The
// anchor states the text that mark covers as its quote, not orphaned; nil for no anchor, and for
// one left to the post.
func heldSpecAnchor(create issueCreate, input *model.AnchorInput, kind docs.MarkKind, kept bool) (*model.Anchor, error) {
	if input == nil {
		return nil, nil
	}
	if err := anchorInputError(input); err != nil {
		return nil, err
	}
	if kept {
		return nil, nil
	}
	if strings.TrimSpace(input.Artifact) != "spec" {
		return nil, pgx.ErrNoRows
	}
	if input.Quote == nil {
		return nil, docs.ErrAnchorMissing
	}
	anchored, err := docs.QuoteInSeed(deref(create.Spec), kind, *input.Quote, input.Occurrence)
	if err != nil {
		return nil, err
	}
	return &model.Anchor{Quote: anchored.Quote}, nil
}

// heldNamedBlock is block blockID of the text itemID, of kind, that a reply beside a held parent
// names (reply_to_block, in_reply_to_block), as that reply's post reads it (planReplyBlock): for
// a held text among waited, which no index reaches before it posts, the block as that text's post
// will index it, read off the text its record holds (refs.NewItemBlock: a comment's or a
// message's body, an ask's question and options), with no node id, which only that index makes;
// for a posted one, the block its index holds now, read in tx for who, the reply's writer. A block
// the text does not hold, or who may not see, is refused now, as its post would refuse it: 400 with
// code, naming field.
func (s *server) heldNamedBlock(ctx context.Context, tx pgx.Tx, who model.Actor, kind refs.ItemKind, itemID, blockID, field, code string, waited heldChain) (*namedBlock, error) {
	link, held := waited.link(itemID)
	if !held {
		_, block, err := s.planReplyBlock(ctx, tx, who, kind, itemID, blockID, field, code)
		return block, err
	}
	if link.decodeErr != nil {
		return nil, link.decodeErr
	}
	var blocks []itemblocks.Block
	switch text := link.decoded.text; {
	case link.kind != string(kind):
		// A held text of another kind holds no block of this one.
	case kind == refs.ItemAsk:
		blocks = refs.AskParts(model.Ask{Question: text.Body, Options: text.Options})
	default:
		blocks = itemblocks.Split(text.Body)
	}
	excerpt, found := refs.NewItemBlock(blocks, blockID)
	if !found {
		return nil, notABlock(kind, itemID, blockID, field, code)
	}
	return &namedBlock{kind: kind, item: itemID, id: blockID, excerpt: excerpt}, nil
}

// heldReplyParentError is the refusal the post of message, beside a held parent, gives the message
// it answers (loadMessageReplyParent): a held message among waited on another issue than message,
// as that message's post writes it (heldLink.messageIssue); any other, read in tx as that post
// reads it, since no held text message waits on writes it. Nil for a message that answers none.
func heldReplyParentError(ctx context.Context, tx pgx.Tx, message messageCreate, waited heldChain) error {
	if message.Input.InReplyTo == nil {
		return nil
	}
	link, held := waited.link(*message.Input.InReplyTo)
	if !held {
		_, err := loadMessageReplyParent(ctx, tx, message.IssueKey, message.Input.Target, message.Input.InReplyTo)
		return err
	}
	issue, err := link.messageIssue(ctx, tx)
	if err != nil {
		return err
	}
	if issue != deref(message.IssueKey) {
		return replyParentInvalid(message.IssueKey)
	}
	return nil
}

// deliveredMessage is the issue and the target of the delivered message id, which a held reply to
// it carries over (heldReplay.messageIssue, heldFollowUpBeside).
func deliveredMessage(ctx context.Context, q queryer, id string) (issueKey, target *string, err error) {
	err = q.QueryRow(ctx, `select issue_key, target from messages where id = $1`, id).Scan(&issueKey, &target)
	return issueKey, target, err
}

// heldIssueCreateError is the refusal the post of create, a new issue beside a held parent or a
// held blocker among waited, gives it (writeIssue), made at its hold in that post's order: a
// blocker outside the project a scoped caller may write, a posted one by its row and a held one by
// the project its held row names (heldForeignTargets), which that post refuses before it reads
// the parent; a parent of another project; a parent too deep, counting the held issues above it,
// which no issues row holds yet, and the chain of rows above those (refuseChainTooDeep); and every
// other check of that post, which no held text decides, by its dry run with no parent and with
// none of the held issues it waits on among its blockers: every blocker a held text holds, which
// its post waits for (heldNamedOpenSQL), whether or not it stands in waited. A parent posted
// meanwhile, its dry run reads as its post does. The dry run is the answer: what its seed made of
// the spec, which does not depend on the parent (heldDryRun.markdown, and the blocks its advice
// read off it).
func (s *server) heldIssueCreateError(ctx context.Context, q queryer, create issueCreate, actor model.Actor, id string, waited heldChain) (heldDryRun, error) {
	heldBlockers, err := heldIssuesAmong(ctx, q, create.Blockers)
	if err != nil {
		return heldDryRun{}, err
	}
	if create.CallerProject != "" {
		foreign, err := foreignTargets(ctx, q, create.Project, create.Blockers)
		if err != nil {
			return heldDryRun{}, err
		}
		if err := refuseForeignBlocker(create.CallerProject, append(foreign, heldForeignTargets(heldBlockers, create.Project)...)); err != nil {
			return heldDryRun{}, err
		}
	}
	check := create
	if parent, _, held := waited.issue(create.Parent); held {
		if parent.Project != create.Project {
			return heldDryRun{}, parentOtherProject()
		}
		above, from := waited.issuesFrom(create.Parent)
		if err := refuseChainTooDeep(ctx, q, "", create.Parent, from, len(above)); err != nil {
			return heldDryRun{}, err
		}
		check.Parent = ""
	}
	check.Blockers = slices.DeleteFunc(slices.Clone(create.Blockers), func(key string) bool {
		_, isHeld := heldBlockers[key]
		return isHeld
	})
	return s.dryRunHeld(check.reviewed(), actor, id)
}

// heldIssuesAmong is the project of each held issue among keys that has neither posted nor
// failed, by its key: a key no issues row holds yet, which a write naming it waits on
// (heldNamedOpenSQL).
func heldIssuesAmong(ctx context.Context, q queryer, keys []string) (map[string]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select id, coalesce(project, '') from held_texts
		where id = any($1) and kind = 'issue' and state in `+heldOpenStatesSQL, keys)
	if err != nil {
		return nil, fmt.Errorf("read the held issues a write names: %w", err)
	}
	held := map[string]string{}
	for rows.Next() {
		var key, project string
		if err := rows.Scan(&key, &project); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the held issues a write names: %w", err)
		}
		held[key] = project
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the held issues a write names: %w", err)
	}
	return held, nil
}

// heldIssueRow is the row the post of create, a new issue, leaves as a write's advice reads it
// (adviceIssueRow), where its parent may itself be a held issue among waited that no issues row
// holds yet: its labels with each held ancestor's above it (issuesFrom), which the legion-label
// walk would read off their rows, and as its parent the nearest ancestor a row holds, from which
// the walk goes on.
func heldIssueRow(create issueCreate, waited heldChain) adviceIssueRow {
	row := adviceIssueRow{labels: slices.Clone(create.Labels)}
	above, from := waited.issuesFrom(create.Parent)
	for _, held := range above {
		row.labels = append(row.labels, held.create.Labels...)
	}
	if from != "" {
		row.parent = &from
	}
	return row
}

// heldWaitedBeside is what write, held beside the held text parent, waits on (heldWaitedOn), which
// heldBeside reads what its post writes off; nil where write runs its dry run all the same, its
// route writing it then as it writes it now: a reply that waits on its writer's earlier held reply
// to the delivery, but for a follow-up whose attempt such a reply answers (attemptReply), which is
// filed under that reply, written by its post alone (heldFollowUpBeside). Any other such reply
// answers its attempt as it stands now, which those posts leave as it is: one that is no follow-up
// answers an attempt of its own, and a follow-up finds its attempt's reply posted already, or
// none. The held texts are read once, for the choice and for what heldBeside reads, so a reply
// that posts in between cannot answer the one otherwise than the other.
func (s *server) heldWaitedBeside(ctx context.Context, write reviewedWrite, parent string) (*heldChain, error) {
	replay, isReplay := write.request.(heldReplay)
	if isReplay && (write.kind != reviewer.KindMessage || !replay.followUp()) {
		return nil, nil
	}
	waited, err := s.heldWaitedOn(ctx, parent)
	if err != nil {
		return nil, err
	}
	if isReplay {
		if _, answered := waited.attemptReply(replay); !answered {
			return nil, nil
		}
	}
	return &waited, nil
}

// heldFollowUpBeside is what the post of replay, a follow-up whose attempt its writer's earlier
// held reply among waited answers (attemptReply), writes (insertSessionReply under that reply):
// the reply it is filed under, and the delivery's issue and target, which that reply carries over
// from the delivered message, read on q. A follow-up that names a block of the delivered message
// is refused now, as the route refuses one on an answered attempt (followUpNamesNoBlock). waited
// is the reading heldWaitedBeside chose this by, so it holds that reply.
func (s *server) heldFollowUpBeside(ctx context.Context, q queryer, replay heldReplay, waited heldChain) (*heldDryRun, error) {
	reply, answered := waited.attemptReply(replay)
	if !answered {
		return nil, errors.New("the held reply a follow-up is filed under is not among the held texts it waits on")
	}
	var input messageReplyInput
	if json.Unmarshal(replay.Body, &input) == nil && input.InReplyToBlock != nil {
		return nil, followUpNamesNoBlock(replay.Subject.InReplyTo)
	}
	wrote := &model.Message{InReplyTo: &reply.id}
	var err error
	if wrote.IssueKey, wrote.Target, err = deliveredMessage(ctx, q, replay.Subject.InReplyTo); err != nil {
		return nil, err
	}
	return &heldDryRun{message: wrote}, nil
}

// heldLink is one held text a text beside a held parent, or after one for its turn, waits on
// (heldWaitedOn): the columns of its row that its readers read, and its request decoded once by
// its kind (heldRow.write), with the error of a request that does not decode.
type heldLink struct {
	heldRow
	decoded   reviewedWrite
	decodeErr error
}

// heldChain is what a text held beside a held text, or after one for its turn, waits on
// (heldWaitedOn): links, that text and, in turn, each held text not yet posted that it waits on
// (depends_on), the nearest first; and first, every held text not yet posted that posts before the
// text does, in the order they post (postReady): at each of links, the furthest first, the open
// texts that wait on the same text as it and came before it, then it; and last the open texts that
// wait on the nearest of links, all of which came before the text.
type heldChain struct {
	links []heldLink
	first []heldLink
}

// heldWaitedQuery reads, for heldWaitedOn, the held text $1 and each it waits on in turn, not yet
// posted, up to $2 of them (chain), and every open text that posts before a text that waits on $1
// and is not held yet (ahead): those of chain, the open texts that wait on the same text as one of
// them and came before it, and the open texts that wait on $1; furthest from $1 first, and at each
// place in the order held.
const heldWaitedQuery = `
	with recursive chain as (
		select h.id, h.depends_on, h.seq, 1 as depth
		from held_texts h where h.id = $1 and h.state in ` + heldOpenStatesSQL + `
		union all
		select h.id, h.depends_on, h.seq, chain.depth + 1
		from chain join held_texts h on h.id = chain.depends_on
		where h.state in ` + heldOpenStatesSQL + ` and chain.depth < $2
	), ahead as (
		select id, depth, true as linked from chain
		union all
		select e.id, chain.depth, false
		from chain join held_texts e on e.depends_on = chain.depends_on and e.seq < chain.seq
		where e.state in ` + heldOpenStatesSQL + `
		union all
		select e.id, 0, false from held_texts e where e.depends_on = $1 and e.state in ` + heldOpenStatesSQL + `
	)
	select h.id, h.kind, h.request, h.actor, coalesce(h.issue_key, ''), coalesce(h.depends_on, ''), ahead.linked
	from ahead join held_texts h on h.id = ahead.id
	order by ahead.depth desc, h.seq`

// heldWaitedOn is what a text that waits on the held text id waits on (heldChain), in one
// statement, each request decoded once.
func (s *server) heldWaitedOn(ctx context.Context, id string) (heldChain, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, heldWaitedQuery, id, parentDepthCap)
	if err != nil {
		return heldChain{}, fmt.Errorf("read the held texts held text %s waits on: %w", id, err)
	}
	defer rows.Close()
	var chain heldChain
	for rows.Next() {
		var link heldLink
		var actor []byte
		var linked bool
		if err := rows.Scan(&link.id, &link.kind, &link.request, &actor, &link.issueKey, &link.dependsOn, &linked); err != nil {
			return heldChain{}, fmt.Errorf("read the held texts held text %s waits on: %w", id, err)
		}
		if err := json.Unmarshal(actor, &link.actor); err != nil {
			return heldChain{}, fmt.Errorf("read held text %s's actor: %w", link.id, err)
		}
		link.decoded, link.decodeErr = link.write()
		chain.first = append(chain.first, link)
		if linked {
			chain.links = append(chain.links, link)
		}
	}
	if err := rows.Err(); err != nil {
		return heldChain{}, fmt.Errorf("read the held texts held text %s waits on: %w", id, err)
	}
	slices.Reverse(chain.links)
	return chain, nil
}

// link is the held text id among the chain's links.
func (chain heldChain) link(id string) (heldLink, bool) {
	for _, link := range chain.links {
		if link.id == id {
			return link, true
		}
	}
	return heldLink{}, false
}

// issue is the new issue key among the chain's links, with the actor its post writes as.
func (chain heldChain) issue(key string) (issueCreate, model.Actor, bool) {
	link, held := chain.link(key)
	if !held || link.decodeErr != nil {
		return issueCreate{}, model.Actor{}, false
	}
	create, isIssue := link.decoded.request.(issueCreate)
	return create, link.actor, isIssue
}

// heldIssue is a new issue among a chain's links, with the actor its post writes as.
type heldIssue struct {
	create issueCreate
	actor  model.Actor
}

// issuesFrom is the new issues among the chain's links on the chain of parents from key up to its
// root, key first, and from, the nearest issue of that chain none of them is, which a row holds,
// "" for none: a held issue waits on its held parent (holdWrite), so the held issues above one
// among links are among them too.
func (chain heldChain) issuesFrom(key string) (held []heldIssue, from string) {
	for from = key; from != "" && len(held) <= parentDepthCap; {
		create, actor, isHeld := chain.issue(from)
		if !isHeld {
			break
		}
		held = append(held, heldIssue{create: create, actor: actor})
		from = create.Parent
	}
	return held, from
}

// attemptReply is the held reply among the chain's links that answers replay's attempt, the
// nearest: its writer's reply to the same delivery attempt that is no follow-up, which its post
// makes the attempt's reply.
func (chain heldChain) attemptReply(replay heldReplay) (heldLink, bool) {
	attempt := replyAttemptOf(replay.Body)
	for _, link := range chain.links {
		earlier, isReplay := link.decoded.request.(heldReplay)
		if !isReplay || link.decodeErr != nil || earlier.followUp() ||
			earlier.Subject.InReplyTo != replay.Subject.InReplyTo || replyAttemptOf(earlier.Body) != attempt {
			continue
		}
		return link, true
	}
	return heldLink{}, false
}

// keptBeside reports whether a write kept beside the held text id (holdRequest), not yet posted,
// posts before the text the chain is read for (first).
func (chain heldChain) keptBeside(id string) bool {
	return slices.ContainsFunc(chain.first, func(link heldLink) bool {
		return link.kind == heldRequestKind && link.dependsOn == id
	})
}

// messageIssue is the issue the post of the held text writes its message on
// (heldRequest.messageIssue), read on q; "" for a text that is no message.
func (link heldLink) messageIssue(ctx context.Context, q queryer) (string, error) {
	if reviewer.Kind(link.kind) != reviewer.KindMessage {
		return "", nil
	}
	if link.decodeErr != nil {
		return "", link.decodeErr
	}
	return link.decoded.request.messageIssue(ctx, q)
}

// countWaited counts on advice, the write advice of actor's write on issue, each text of first
// (the held texts that post before the write, in the order they post) that the same session
// writes to that issue, as the write's post finds it once posted: the event its post appends
// (heldRequest.postEvent, sessionWriteTypes) and, for an ask, one of the session's open asks
// (your_open_asks), newer than every ask that is open now.
func countWaited(advice *writeAdvice, issue string, actor model.Actor, first []heldLink) {
	if actor.Kind != "session" || issue == "" {
		return
	}
	for _, held := range first {
		if held.issueKey != issue || held.actor.Kind != "session" || held.actor.ID != actor.ID || held.decodeErr != nil {
			continue
		}
		event, ask := held.decoded.request.postEvent(held.id)
		if slices.Contains(sessionWriteTypes, event) {
			advice.SessionWritesSinceHuman++
		}
		if ask != nil && len(advice.YourOpenAsks) < adviceOpenAskLimit {
			advice.YourOpenAsks = append(advice.YourOpenAsks, *ask)
		}
	}
}

// heldBesideThread is the thread a comment beside a held parent joins once the held texts it waits
// on (waited) have posted, as its post's climb finds it (normalizeCommentThreadTarget): the ask
// ask_id names, or the head of the thread of the comment reply_to names, with that comment as the
// one it answers; read off the record of a held text among waited and the row of a posted one,
// in tx.
func (s *server) heldBesideThread(ctx context.Context, tx pgx.Tx, owner owner, input commentInput, waited heldChain) (commentThreadTarget, error) {
	if input.AskID != nil {
		return s.heldAskThread(ctx, tx, owner, *input.AskID, waited)
	}
	if input.ReplyTo == nil {
		return commentThreadTarget{}, nil
	}
	target, err := s.heldThreadHead(ctx, tx, owner, *input.ReplyTo, waited)
	if err != nil {
		return commentThreadTarget{}, err
	}
	target.Answers = input.ReplyTo
	return target, nil
}

// heldAskThread is the thread of the ask askID as a reply joins it: a held ask among waited as it
// posts, open, and moved past no version, refused where it is on another owner than the reply, as
// describeAsk refuses a posted one; a posted one as describeAsk reads it.
func (s *server) heldAskThread(ctx context.Context, tx pgx.Tx, owner owner, askID string, waited heldChain) (commentThreadTarget, error) {
	if link, held := waited.link(askID); held {
		if link.decodeErr != nil {
			return commentThreadTarget{}, link.decodeErr
		}
		if create, isAsk := link.decoded.request.(askCreate); isAsk {
			if !sameOwner(create.Owner, owner) {
				return commentThreadTarget{}, askIDNotOnOwner()
			}
			return commentThreadTarget{AskID: &askID, AskQuestion: create.Question, AskState: "open"}, nil
		}
	}
	return s.describeAsk(ctx, tx, owner, &askID)
}

// sameOwner reports whether a and b name one owner: the same issue, or the same document.
func sameOwner(a, b owner) bool {
	return deref(a.IssueKey) == deref(b.IssueKey) && deref(a.ArtifactID) == deref(b.ArtifactID)
}

// heldThreadHead is the head of the thread of the comment commentID as a reply's climb finds it
// (threadHeadFrom): for a held text among waited, the next step its kind takes
// (heldRequest.climb) - the thread of the ask it names, else the text itself where it replies to
// none, else the head of the thread of the one it replies to - refused where it is no comment on
// the reply's owner, as loadOwnedComment refuses a posted one; for a posted comment,
// threadHeadFrom's.
func (s *server) heldThreadHead(ctx context.Context, tx pgx.Tx, owner owner, commentID string, waited heldChain) (commentThreadTarget, error) {
	for depth := 1; depth <= parentDepthCap; depth++ {
		link, held := waited.link(commentID)
		if !held {
			comment, err := s.loadOwnedComment(ctx, tx, owner, commentID)
			if err != nil {
				return commentThreadTarget{}, err
			}
			return s.threadHeadFrom(ctx, tx, owner, comment)
		}
		if link.decodeErr != nil {
			return commentThreadTarget{}, link.decodeErr
		}
		replyTo, askID, err := link.decoded.request.climb(reviewer.Kind(link.kind), owner)
		if err != nil {
			return commentThreadTarget{}, err
		}
		switch {
		case askID != "":
			return s.heldAskThread(ctx, tx, owner, askID, waited)
		case replyTo == "":
			return commentThreadTarget{ReplyTo: &link.id}, nil
		}
		commentID = replyTo
	}
	return commentThreadTarget{}, replyToNotOnOwner()
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
// writes). dry is the write's dry run, or, for a text beside a held parent, what heldBeside read
// off the held texts it waits on. What the post writes beside the request's own fields is read off
// it - an ask's anchor, the document its anchor quotes and the person it waits on; a comment's
// anchor, turn, mentions, the block it replies to, the agent text it answers and whom its ask
// waits on once it posts; a message's block it replies to; an issue's repository, relations to
// priorities and components (heldIssueWrites) - less what only the post makes: an anchor's mark
// id, the sequence of an ask's ask.opened event, an issue's rank, document and sequence, the
// deliveries a post sends, and, beside a held parent, what heldBeside leaves out. advice, where
// not nil, is the advice the write's own answer carries (heldAdvice), merged in as that answer
// merges it.
func heldAnswerOf(write reviewedWrite, id string, actor model.Actor, at time.Time, held heldState, dry *heldDryRun, trees bool, advice any) heldAnswer {
	payload := write.request.answer(write.kind, id, actor, at, dry, trees)
	if advice != nil {
		payload = withRawAdvice(payload, advice)
	}
	return heldAnswer{payload: payload, held: held}
}

// heldAnchor is the anchor a dry run wrote, as a held answer states it: its document, block, quote
// and version, and no mark id, which only the post's own mark makes.
func heldAnchor(wrote *model.Anchor) *model.Anchor {
	if wrote == nil {
		return nil
	}
	anchor := *wrote
	anchor.MarkID = ""
	return &anchor
}

// heldIssueWrites sets on issue, a held issue's answer, what the post of create writes beside its
// row's own columns (writeIssue): its repository, its relations to priorities and its components,
// with the external links a read answers, none, since a held create is never an import
// (issueCreate's reviewedWrite). Where a dry run filed the issue (filed), each is the one it read
// back, resolved as a read of the posted issue resolves it: the repository it targets, and the
// priorities and the components it inherits from its parent. Beside a held parent that no issues
// row holds yet, each is the create's own: the repository it names, the priorities it states and
// the components it chose, which is the inheriting issue's attachment where it chose none. A dry
// run ran there too, but with no parent (heldIssueCreateError), so the issue it filed inherits
// nothing of the parent the post files it under, and issueCreate.beside keeps none of that issue
// (filed is nil), only its spec as its seed renders it and the blocks its advice reads off it.
func heldIssueWrites(issue *model.Issue, create issueCreate, filed *model.Issue) {
	if filed != nil {
		issue.Repo, issue.ResolvedRepo = filed.Repo, filed.ResolvedRepo
		issue.Advances, issue.Components, issue.ExternalLinks = filed.Advances, filed.Components, filed.ExternalLinks
		return
	}
	issue.Repo, issue.ResolvedRepo = create.Repo, create.Repo
	issue.Advances = make([]model.IssueAdvance, 0, len(create.Advances))
	for _, id := range create.Advances {
		issue.Advances = append(issue.Advances, model.IssueAdvance{ID: id})
	}
	issue.Components = model.IssueComponents{Mode: "inherit", IDs: []string{}, Unknown: []string{}}
	if chose := create.Components; create.ComponentsProvided && chose != nil && chose.Mode != "inherit" {
		issue.Components.Mode = chose.Mode
		if chose.Mode == "explicit" {
			issue.Components.IDs = chose.IDs
		} else {
			issue.Components.Reason = &chose.Reason
		}
	}
	issue.ExternalLinks = []model.ExternalLink{}
}

// heldCommentWrites sets on comment, a held comment's answer, what its dry run wrote (wrote) beside
// the request's own fields: the comment it replies to and its ask, since the post climbs a reply
// to the head of its thread, the root comment or the ask (threadHeadOf), and keeps the comment the
// request named as answers; its anchor with no mark id (heldAnchor), its turn, the block it
// replies to and the agent text it answers; and its mentions, where the dry run found any.
func heldCommentWrites(comment *model.Comment, wrote *model.Comment) {
	comment.ReplyTo, comment.AskID = wrote.ReplyTo, wrote.AskID
	comment.Anchor, comment.Turn, comment.Answers = heldAnchor(wrote.Anchor), wrote.Turn, wrote.Answers
	comment.ReplyBlock, comment.RepliesToBlock = wrote.ReplyBlock, wrote.RepliesToBlock
	if wrote.Mentions != nil {
		comment.Mentions = wrote.Mentions
	}
}

// heldMessageWrites sets on message, a held message's answer, what its dry run wrote (wrote)
// beside the request's own fields: the block it replies to.
func heldMessageWrites(message *model.Message, wrote *model.Message) {
	message.ReplyBlock, message.RepliesToBlock = wrote.ReplyBlock, wrote.RepliesToBlock
}

// heldAdvice is the advice a held write's answer carries, which is the advice the write's own
// answer carries: a writer the reviewer holds is told what it would have been told without the
// hold. dry is what the write's dry run found, or heldBeside's reading beside a held parent, whose
// write advice is nil for a write with no issue state to report. An issue and an ask also run
// their related-item check here, before the id is reserved, as the write's own path runs it after
// its commit, and an issue's carries what the near-copy gate told its create (issueCreate.gate),
// the note of a check not made included. The check is recorded nowhere: its rows name the issue or
// the ask it was run for, which does not exist until the held text posts. Nil for a write whose
// answer carries no advice.
func (s *server) heldAdvice(r *http.Request, write reviewedWrite, actor model.Actor, dry *heldDryRun, parent *heldParent) any {
	return write.request.advice(s, r, actor, dry.advice, dry, parent)
}

// heldDryRun is what a held write's dry run found (dryRunHeld): answered, the answer the route of
// a reply kept whole gave in place of a pass (a refusal, or the stored reply of an attempt already
// answered), which the write gets as its own; otherwise advice, the write advice the write's own
// answer would carry (nil where its path gives none), and what the dry run wrote and read back
// before its rollback, the one of its kind: ask, comment, message, or, for a new issue, issue,
// with markdown, its spec as its seed would store it. A comment, a new one or a reply, comes with
// askWaitingOn, whom the open ask of its thread waits on once it posts, "" for a comment in no open
// ask's thread (the answer's ask_waiting_on). A new issue comes with assignee, the person its post
// files it under: the one its dry run filed it under, or, beside a held parent, where heldBeside
// reads what its post writes and keeps no issue of its dry run, which filed it with no parent,
// heldIssueAssignee's.
type heldDryRun struct {
	answered     *replayAnswer
	advice       *writeAdvice
	markdown     string
	ask          *model.Ask
	comment      *model.Comment
	askWaitingOn string
	message      *model.Message
	issue        *model.Issue
	assignee     *string
}

// dryRunHeld runs write as actor's under id in a transaction it rolls back before the commit
// (heldPost's dry run), so every check the kind's own path makes refuses it now. A kind's create
// path answers its refusal as an error, and a write that passed with the advice it would have
// answered. A reply kept whole passes where its route ends the dry run (errHeldDryRun), and any
// other answer of its route is the reply's own, in answered: a refusal, or a reply to an attempt
// already answered, which writes nothing and answers the stored reply as it does with the reviewer
// off (the reply routes' finishRepeat), so nothing is held for it.
func (s *server) dryRunHeld(write reviewedWrite, actor model.Actor, id string) (heldDryRun, error) {
	return write.request.dryRun(s, s.postContext(), actor, id, &heldPost{id: id, dryRun: true})
}

// heldDryRunOrAnswer runs write's dry run as actor's under id (dryRunHeld) and answers what it
// found; nil where the dry run ended the write, which it has answered on w: its error, or the
// answer its route gave in place of a pass (answered), and the caller writes nothing more.
func (s *server) heldDryRunOrAnswer(w http.ResponseWriter, write reviewedWrite, actor model.Actor, id string) *heldDryRun {
	ran, err := s.dryRunHeld(write, actor, id)
	if err != nil {
		s.writeHandlerError(w, err)
		return nil
	}
	if own := ran.answered; own != nil {
		own.writeTo(w)
		return nil
	}
	return &ran
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
	// wrote is what a dry run's route of a reply kept whole wrote before the dry run ended (keep),
	// which the held answer reads (dryRunHeld): a message reply, as its post writes it, and a
	// comment reply as commentWritten holds one, with whom its ask waits on once it posts.
	wrote any
}

// keep keeps wrote, the text a reply route wrote, on a dry run, which ends before its commit;
// nothing on a post or the route's own write.
func (h *heldPost) keep(wrote any) {
	if h != nil && h.dryRun {
		h.wrote = wrote
	}
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
// holding it or one held past staleClaim, and the held text it waits on, if its record reads that
// text (heldReviewWaitsSQL), must have posted. A text whose parent has not posted is read in a
// record that misses that parent (a new ask, a comment or a child on a held issue), so its review
// waits for the parent's post, which starts it (afterHeldPost). A text that waits only for its
// turn at its place (heldBeforeOf) is reviewed at once.
func (s *server) claimHeldReview(ctx context.Context, id string) (heldRow, bool, error) {
	row, err := scanHeld(s.deps.Store.Pool.QueryRow(ctx, `
		update held_texts h set claimed_at = now()
		where h.id = $1 and h.state = 'reviewing'
			and (h.claimed_at is null or h.claimed_at < now() - $2::interval)
			and (h.depends_on is null or not `+heldReviewWaitsSQL+` or exists (
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
// if any, has posted, every text that waits on that one and came before it has posted or failed,
// and every held text it names beside that one (heldNamedOpenSQL) has posted or failed. Each
// posts in the order the writes came. It runs on the post context (postContext), whatever set it
// off, so no request's values reach a post.
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
				and not `+heldNamedOpenSQL+`
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
// rebuild repairs). The reviewer's rewrite of a held decision block (rewriteRefusal) reads such a
// refusal the same way: no reason the rewrite cannot be written, as the sweep posts the block
// again. A person's Rewrite, which no later write takes up again, fails with such a refusal's own
// message (askRewriteRefusal).
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
// writer the text was not posted, with the refusal (tellNotPosted). Every held text that reads it
// fails with it at once, and each of their writers is told too; a held text that waited on it only
// for its turn takes its turn and posts (failOrphanedHeld).
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
	moved, err := s.failOrphanedHeld(ctx)
	if err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: fail the held texts that wait on a failed one", "error", err)
	}
	if moved {
		// A text that waited on row for its turn may post now.
		s.detached.goDetached(s.postReady)
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
	return write.request.post(s, ctx, row, &heldPost{id: row.id})
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
	if writingCheckExemption(row.actor) != "" {
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
	if rewritten, ok := write.request.rewritten(write.kind, rewrite); ok {
		return rewritten
	}
	return write
}

// failOrphanedHeld settles each held text that waits on one that failed (failWaitingOnFailed): a
// text that reads it can never post, and fails, and its writer is told it was not posted, naming
// the held text it waited on (tellNotPosted); a text that waited on it only for its turn takes its
// turn. It reports whether it moved any text, whose turn may have come.
func (s *server) failOrphanedHeld(ctx context.Context) (bool, error) {
	moved := false
	for range 10 {
		orphans, movedNow, err := s.failWaitingOnFailed(ctx)
		moved = moved || movedNow
		if err != nil || (len(orphans) == 0 && !movedNow) {
			return moved, err
		}
		for _, orphan := range orphans {
			s.tellNotPosted(ctx, orphan.writer, reviewer.PostFailedFrame{
				ID: orphan.id, Kind: reviewer.Kind(orphan.kind), WaitedOn: orphan.waitedOn,
			})
		}
	}
	return moved, nil
}

// heldOrphan is a held text failWaitingOnFailed failed: its id, kind and writer, and the held text
// it waited on.
type heldOrphan struct {
	id, kind, writer, waitedOn string
}

// failWaitingOnFailed settles, in one transaction, each held text that waits on one that failed. A
// text that reads it (heldReviewWaitsSQL) fails, in one statement, and is answered: each is failed
// by this call alone, so its writer is told once. A text that waits on it only for its turn at its
// place (heldBeforeOf) waits on what the failed text waited on instead, on none where that waited
// on none, so it keeps its turn and posts; the answer says whether any moved.
func (s *server) failWaitingOnFailed(ctx context.Context) ([]heldOrphan, bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		update held_texts h
		set state = 'failed', detail = 'the held text it waits on, ' || p.id || ', failed to post',
			claimed_at = null, updated_at = now()
		from held_texts p
		where p.id = h.depends_on and p.state = 'failed' and h.state in `+heldOpenStatesSQL+` and `+heldReviewWaitsSQL+`
		returning h.id, h.kind, coalesce(h.writer, ''), p.id`)
	if err != nil {
		return nil, false, err
	}
	var orphans []heldOrphan
	for rows.Next() {
		var orphan heldOrphan
		if err := rows.Scan(&orphan.id, &orphan.kind, &orphan.writer, &orphan.waitedOn); err != nil {
			rows.Close()
			return nil, false, err
		}
		orphans = append(orphans, orphan)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	moved, err := tx.Exec(ctx, `
		update held_texts h set depends_on = p.depends_on, updated_at = now()
		from held_texts p
		where p.id = h.depends_on and p.state = 'failed' and h.state in `+heldOpenStatesSQL+` and not `+heldReviewWaitsSQL)
	if err != nil {
		return nil, false, err
	}
	return orphans, moved.RowsAffected() > 0, tx.Commit(ctx)
}

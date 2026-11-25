package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// issueUpdatedNow is the value every write that stamps an issue's updated_at itself sets it to:
// the statement's own moment, clock_timestamp(), and never below what the row already holds. A
// write's transaction begins before it takes the issue's locks, and now() is that beginning, so
// a write that waited behind another one would otherwise stamp a time older than the one that
// write committed, and updated_at would move backwards; the page's issue cache keeps the newest
// updated_at it has seen and drops an older answer. An event append keeps its own rule,
// greatest(updated_at, the event's created_at) (events.Broker.Append), which never moves the
// column backwards either.
const issueUpdatedNow = "greatest(updated_at, clock_timestamp())"

// updateIssueInput is the body of PATCH /api/v1/issues/{key}.
type updateIssueInput struct {
	Title         *string               `json:"title"`
	Status        *string               `json:"status"`
	Rank          *rankInput            `json:"rank"`
	Labels        *[]string             `json:"labels"`
	LabelsAdd     *[]string             `json:"labels_add"`
	ExpectedSeq   *int64                `json:"expected_sequence"`
	Priority      json.RawMessage       `json:"priority"`
	Route         *string               `json:"route"`
	ExternalLinks *[]model.ExternalLink `json:"external_links"`
	Assignee      json.RawMessage       `json:"assignee"`
	Parent        json.RawMessage       `json:"parent"`
	BlockedBy     *[]string             `json:"blocked_by"`
	// Advances is the replacement list of priority ids this issue advances ([] clears it), and
	// AdvancesRevision the revision the read this write is answering returned. A write that
	// carries the revision is a whole-set write; one that carries none writes single acts and
	// may change at most one relation (refuseAdvancesRevision).
	Advances         *[]string       `json:"advances"`
	AdvancesRevision *int64          `json:"advances_revision"`
	Components       json.RawMessage `json:"components"`
	Repo             json.RawMessage `json:"repo"`
	Actor            *model.Actor    `json:"actor"`
	// RefuseOpenForeignChildren asks, beside a status of retro or done, that the write be refused
	// while a child of another project is open (refuseOpenForeignChildren). The Legion daemon's
	// sign_off and close_root send it; a person's status write does not, and is taken as before.
	RefuseOpenForeignChildren bool `json:"refuse_open_foreign_children"`
}

// patchIssue is PATCH /api/v1/issues/{key}: it decodes the body, resolves who writes it, and
// answers what the write core (writeIssue) wrote.
func (s *server) patchIssue(w http.ResponseWriter, r *http.Request) {
	var input updateIssueInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writer, err := s.requestWriter(r, input.Actor)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	callerProject, _ := s.callerProject(r)
	written, err := s.writeIssue(r.Context(), issueWriteCall{
		writer:        writer,
		key:           r.PathValue("key"),
		fields:        input,
		callerProject: callerProject,
		outliving:     outlivingTheClient(r),
		adviceRoute:   "PATCH /api/v1/issues/{key}",
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, withAdvice(written.issue, written.advice))
}

// issueWriteCall is one write of one issue through the PATCH's write core (writeIssue).
type issueWriteCall struct {
	// writer is who writes: its actor is the actor every row and event of the write records.
	writer issueWriter
	// key is the issue the write writes.
	key string
	// fields are the fields the write writes, as the PATCH's body decodes them. writeIssue
	// parses and checks them as the PATCH does.
	fields updateIssueInput
	// callerProject is the one project the request's credential may write, or empty for a
	// credential that names none (callerProject).
	callerProject string
	// outliving is a context that does not end when the client goes away (outlivingTheClient):
	// the close's settlement before the transaction, and its ask sweep after the commit, run on
	// it.
	outliving context.Context
	// adviceRoute is the route that the write's advice names (writeAdviceBefore).
	adviceRoute string
	// comment is the comment that the write leaves on the issue, or nil for none. It is written in
	// the write's own transaction, after the write's own events, so the write and its comment
	// commit together or not at all. A write that changes nothing leaves no comment.
	comment *issueComment
	// step is what the write of one step of a status change (status_change.go) or of R25's fix
	// (parent_status_fix.go) carries beside the PATCH's fields, or nil for a PATCH.
	step *issueWriteCallStep
}

// issueWriteCallStep is what the write of one step of a status change (POST
// /api/v1/issues/{key}/status-change) or of R25's fix (RunParentStatusFixAtBoot) carries beside
// the PATCH's fields. Each step is a write of its own, and it decides its write from what it reads
// under its own owner lock, not from the plan that the call read before its first step.
type issueWriteCallStep struct {
	// named is the issue that the status change names, or empty for a step of R25's fix, which
	// names none. A fenced step, or a step of a keyed call (claim), that holds its owner lock reads
	// back the ids of every event the step appended on it (issueWriteResult.namedEvents).
	named string
	// fence is the call's fence on named, or nil for a call that sent no expected_sequence.
	fence *issueWriteFence
	// claim is the request key's claim of a keyed status change, nil for a call with no key and
	// for a step of R25's fix: a step that writes adds its entry to the call's recovery point
	// under it (queueStatusChangeStep).
	claim *requestKeyClaim
	// leaves reports, from the issue as the step reads it under its owner lock, that the step
	// leaves the issue as it stands: the step writes no row, no event and no comment, and rolls
	// its transaction back. It is nil for a step that always writes: the named issue's own.
	leaves func(before model.Issue) bool
	// moved is what the write of a step of an option changes, from the issue as the step reads it
	// under its owner lock, which the comment on named lists (statusChangeRun.namedComment). A
	// step of a keyed call that writes keeps it in the call's recovery point (queueStatusChangeStep).
	// It is nil for the named issue's own step and for a step of R25's fix.
	moved func(before model.Issue) statusChangeMove
	// sweepsLater is that the caller runs the ask sweep of the step's close itself, after the
	// call's last step (issueWriteResult.closed), so that no fence of the call reads the events of
	// the sweep. Only the named issue's own step sets it.
	sweepsLater bool
	// options is the options of the status change that the step is a step of, which writeIssue
	// puts on the step's one-step plan. So the step's own check of R26 and R24 reads the options
	// that the planning check read (planStatusChangeUnderLocks). A step of R25's fix carries none.
	options statusChangeOptions
}

// issueWriteFence is the fence of a status change that sent expected_sequence: the sequence it
// sent, and the ids of the events on the named issue that the call counts as its own. Each step
// locks the named issue beside its own issue, and refuses 409 ISSUE_CHANGED when an event on the
// named issue after that sequence is not one of the call's own.
type issueWriteFence struct {
	sequence int64
	own      map[int64]bool
}

// refuseForeignEvents is the fence of one step, which holds the named issue's owner lock: 409
// ISSUE_CHANGED when an event on named after the fenced sequence is not one of the call's own.
func (fence *issueWriteFence) refuseForeignEvents(ctx context.Context, tx pgx.Tx, named string) error {
	rows, err := tx.Query(ctx, `select id from events where issue_key = $1 and seq > $2 order by id`, named, fence.sequence)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !fence.own[id] {
			return errIssueChanged()
		}
	}
	return nil
}

// errIssueChanged is the 409 ISSUE_CHANGED of a write whose expected_sequence is no longer the
// issue's last event.
func errIssueChanged() error {
	return errorf(http.StatusConflict, "ISSUE_CHANGED", "issue changed since the hand-over read")
}

// issueWriteResult is what writeIssue wrote.
type issueWriteResult struct {
	// issue is the issue as the write answers it.
	issue model.Issue
	// advice is the advice the write answers beside the issue (withAdvice).
	advice *writeAdvice
	// previousLastSeq is the issue's last_seq as the write read it under the issue's owner
	// lock, before the write appended any event.
	previousLastSeq int
	// wrote is false for a write that found nothing to change and wrote nothing, and for a step
	// of a status change that left its issue as it stands (issueWriteCallStep.leaves).
	wrote bool
	// appended is each event the write appended, with its owner and its id, in the order the
	// write queued them.
	appended []*model.Event
	// namedEvents is, for a fenced step or a step of a keyed call that holds the owner lock of the
	// issue the call names, the ids of the events on that issue after its last_seq as the step
	// read it under that lock: every event the step appended there, those that document work
	// appends through the ledger's event sink included. They are what the fence of a later step
	// of the call counts as its own. It is empty for every other write.
	namedEvents []int64
	// closed is that the write closed the issue, and withdrawn is the asks that the close
	// withdrew: what the close's ask sweep takes (sweepClosedIssueAsks). writeIssue runs that
	// sweep itself, except for a step whose caller sweeps later (issueWriteCallStep.sweepsLater).
	closed    bool
	withdrawn []model.Ask
	// moved is what the write of a step of an option changed (issueWriteCallStep.moved): nil for
	// every other write, and for a write that wrote nothing.
	moved *statusChangeMove
}

// writeIssue is the PATCH's write core. It parses the call's fields, takes the owner locks of
// the issue and of every issue the write names, decides the write's refusals under them, and
// writes the issue's row and everything beside it, the call's comment included, in a transaction
// of its own, which it commits. Then it publishes what it appended. Each refusal is an error that
// writeHandlerError answers with the status, code and text the PATCH answers.
func (s *server) writeIssue(ctx context.Context, call issueWriteCall) (issueWriteResult, error) {
	input, actor := call.fields, call.writer.actor
	var blockers []string
	if input.BlockedBy != nil {
		var err error
		blockers, err = normalizeIssueBlockers(*input.BlockedBy)
		if err != nil {
			return issueWriteResult{}, err
		}
	}
	var advances []string
	if input.Advances != nil {
		var err error
		advances, err = normalizeIssueAdvances(*input.Advances)
		if err != nil {
			return issueWriteResult{}, err
		}
	}
	if input.Title != nil {
		*input.Title = strings.TrimSpace(*input.Title)
		if *input.Title == "" {
			return issueWriteResult{}, errorf(http.StatusBadRequest, "INVALID_ISSUE", "title must not be blank")
		}
		if length := itemblocks.Units16(*input.Title); length > contracts.IssueTitleMax {
			return issueWriteResult{}, capExceededError("title", length, contracts.IssueTitleMax)
		}
	}
	status := ""
	if input.Status != nil {
		status = strings.TrimSpace(*input.Status)
		if status == "" {
			return issueWriteResult{}, errorf(http.StatusBadRequest, "INVALID_ISSUE", "status must not be blank")
		}
		if !model.IsIssueStatus(status) {
			return issueWriteResult{}, errorf(http.StatusBadRequest, "INVALID_STATUS", "status is not in the Legion lifecycle")
		}
	}
	if input.RefuseOpenForeignChildren && status != "retro" && status != "done" {
		return issueWriteResult{}, errorf(http.StatusBadRequest, "INVALID_ISSUE", "refuse_open_foreign_children goes only with status retro or done")
	}
	if input.Route != nil && *input.Route != "" {
		if _, err := model.ParseRoute(*input.Route); err != nil {
			return issueWriteResult{}, errorf(http.StatusBadRequest, "ROUTE_INVALID", "invalid route")
		}
	}
	var labels, labelsAdd []string
	if input.Labels != nil && input.LabelsAdd != nil {
		return issueWriteResult{}, errorf(http.StatusBadRequest, "LABELS_INPUT", "labels and labels_add cannot both be set")
	}
	if input.Labels != nil {
		var err error
		labels, err = normalizeIssueLabels(*input.Labels)
		if err != nil {
			return issueWriteResult{}, err
		}
	}
	if input.LabelsAdd != nil {
		var err error
		labelsAdd, err = normalizeIssueLabels(*input.LabelsAdd)
		if err != nil {
			return issueWriteResult{}, err
		}
	}
	priority, priorityProvided, err := parseIssuePriority(input.Priority)
	if err != nil {
		return issueWriteResult{}, err
	}
	assignee, assigneeProvided, err := s.parseIssueAssignee(ctx, input.Assignee)
	if err != nil {
		return issueWriteResult{}, err
	}
	parent, parentProvided, err := parseIssueParent(input.Parent)
	if err != nil {
		return issueWriteResult{}, err
	}
	components, componentsProvided, err := parseIssueComponents(input.Components)
	if err != nil {
		return issueWriteResult{}, err
	}
	repo, repoProvided, err := parseIssueRepo(input.Repo, "INVALID_ISSUE")
	if err != nil {
		return issueWriteResult{}, err
	}
	repo = s.renamedRepo(repo)
	// Each link URL is stored under its repository's new name where a rename pair renames it
	// away (refs.Renames.CanonicalURL), as every write of a stored URL is. It is rewritten here,
	// before the closed-issue gate compares the links with the stored ones, so a link resent
	// under the old name is the link the issue already has.
	if input.ExternalLinks != nil {
		for i := range *input.ExternalLinks {
			(*input.ExternalLinks)[i].URL = s.deps.Renames.CanonicalURL((*input.ExternalLinks)[i].URL)
		}
	}

	key := call.key
	parentKey := ""
	if parent != nil {
		parentKey = *parent
	}
	// A credential that names one project is refused a target or a parent outside it where its
	// project is read, since this route's own address names neither (project_scope.go). It is read
	// first here, with no lock held and before a close settles the issue's documents or the
	// transaction begins, so a write the credential may not make settles nothing; the
	// pre-handler check already held the issue to the credential's project, which is therefore
	// the issue's own, and a status change holds each issue a step of it writes to that project
	// before its first step (refuseStepsOutsideCallerProject). The lock step refuses it again
	// before any lock, and writeBlockedBy or validateIssueParent under them.
	if call.callerProject != "" && (parentKey != "" || len(blockers) > 0) {
		if _, err := foreignDependencyTargets(ctx, s.deps.Store.Pool, call.callerProject, call.callerProject, blockers, parentKey); err != nil {
			return issueWriteResult{}, err
		}
	}
	// Whether the write closes the issue (closing) waits for the issue's owner lock; whether it
	// asks to is known now.
	closes := input.Status != nil && status == "done"
	// A close is checked once here too, on the pool and with no lock held, for who may close an
	// issue Legion runs (judgeSpecWriter), so that a close the check refuses does not settle the
	// issue's documents first. It only spares that settlement: the check under the owner locks
	// below decides, after the write's other refusals, and answers the refusal. A step skips it:
	// the status change's plan ran the check on each of its steps under its locks
	// (planStatusChangeUnderLocks), and R25's fix closes no issue.
	if closes && (call.step != nil || !s.spareSettlementOfARefusedClose(ctx, call.writer, key)) {
		// Before the transaction, which settlement's own would queue behind on the issue's owner
		// lock.
		s.settleIssueBeforeClose(call.outliving, key)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return issueWriteResult{}, err
	}
	defer tx.Rollback(ctx)
	// A close withdraws the issue's open asks and a reopen restores them, writing their blocks
	// into the issue's documents, which reach their rooms through the ledger's commit. That
	// document work runs before the patch's first write, holding no transaction id (INFRA-636); its
	// writes, the asks' and every event this patch appends are queued on the ledger and run, in the
	// order made, after the patch's own row writes, which stay inline because their results steer
	// it (a component or blocker refusal, a taken external link).
	documentCtx, ledger := s.deps.Docs.Join(ctx, tx)
	defer ledger.Discard()

	// A write that asks for the open-child check of an acceptance is a dependency write too: the
	// check and the status it guards run under the deployment dependency lock that every create
	// with a parent and every reparent takes, so no child is filed between the two.
	dependencyWrite := input.BlockedBy != nil || parentProvided || input.RefuseOpenForeignChildren
	// The lock step below reads each target's project again and refuses a scoped caller one
	// outside it before any lock is taken (lockIssueDependencyWrite).
	if dependencyWrite || input.Rank != nil {
		var project string
		if err := tx.QueryRow(ctx, `select project_key from issues where key = $1`, key).Scan(&project); err != nil {
			return issueWriteResult{}, err
		}
		if dependencyWrite {
			if err := lockIssueDependencyWrite(ctx, tx, project, call.callerProject, blockers, parentKey); err != nil {
				return issueWriteResult{}, err
			}
		}
		if input.Rank != nil {
			if err := lockProjectRankAllocation(ctx, tx, project); err != nil {
				return issueWriteResult{}, err
			}
		}
	}

	// A PATCH takes the owner lock of every issue it names in one lock order before it reads any
	// of them: the issue, its current parent when a status change or a reparent appends to it, the
	// proposed parent and the rank neighbours (lockPatchOwners, lockPatchOwnersAndParent). It never
	// holds one of those locks while waiting for another that a second write holds. Validation, the
	// dependency walk and the rank reads below are plain reads of issues whose locks it holds.
	named := make([]string, 0, 3)
	if parent != nil {
		named = append(named, *parent)
	}
	if input.Rank != nil {
		// Each neighbour is trimmed once, here, so the key the lock below takes is the key
		// rankForInput reads.
		if input.Rank.After != nil {
			*input.Rank.After = strings.TrimSpace(*input.Rank.After)
			named = append(named, *input.Rank.After)
		}
		if input.Rank.Before != nil {
			*input.Rank.Before = strings.TrimSpace(*input.Rank.Before)
			named = append(named, *input.Rank.Before)
		}
	}
	// A fenced step of a status change locks the issue the call names beside its own, in the same
	// one lock order, and decides the fence first, before every other answer of the step.
	step := call.step
	if step != nil && step.fence != nil {
		named = append(named, step.named)
	}
	lock := lockPatchOwners
	if input.Status != nil || parentProvided {
		lock = lockPatchOwnersAndParent
	}
	if err := lock(ctx, tx, key, named...); err != nil {
		return issueWriteResult{}, err
	}
	if step != nil && step.fence != nil {
		if err := step.fence.refuseForeignEvents(ctx, tx, step.named); err != nil {
			return issueWriteResult{}, err
		}
	}
	before, err := s.loadIssue(ctx, tx, key)
	if err != nil {
		return issueWriteResult{}, err
	}
	if input.ExpectedSeq != nil && *input.ExpectedSeq != int64(before.LastSeq) {
		return issueWriteResult{}, errIssueChanged()
	}
	// A step of a status change decides here, from the issue as it reads it under its own lock,
	// whether it writes at all. A step that leaves the issue as it stands rolls back.
	if step != nil && step.leaves != nil && step.leaves(before) {
		return issueWriteResult{issue: before, previousLastSeq: before.LastSeq}, nil
	}
	// A fenced step, or a step of a keyed call, that holds the owner lock of the issue the call
	// names reads back, in its write phase, every event it appends there after the last_seq it
	// reads here: the named issue's own step, a fenced step, and the step of a direct child, whose
	// parent lockPatchOwnersAndParent locked. The read is a plain one, before the first write. A
	// step of an unfenced call with no request key reads none, since nothing reads those events.
	namedLastSeq, holdsNamed := 0, false
	if step != nil {
		switch {
		case step.fence == nil && step.claim == nil:
		case step.named == key:
			namedLastSeq, holdsNamed = before.LastSeq, true
		case step.fence != nil || (before.Parent != nil && *before.Parent == step.named):
			if err := tx.QueryRow(ctx, `select last_seq from issues where key = $1`, step.named).Scan(&namedLastSeq); err != nil {
				return issueWriteResult{}, err
			}
			holdsNamed = true
		}
	}
	// The call's comment is worded from the issue as the write reads it under its owner lock, and
	// so is what a step of an option changes.
	var commentBody string
	if call.comment != nil {
		commentBody = call.comment.words(before)
	}
	var moved *statusChangeMove
	if step != nil && step.moved != nil {
		move := step.moved(before)
		moved = &move
	}
	if parentProvided && parent != nil {
		if err := validateIssueParent(ctx, tx, key, call.callerProject, *parent); err != nil {
			return issueWriteResult{}, err
		}
	}
	closing := closes && before.ClosedAt == nil
	reopening := input.Status != nil && !closes && before.ClosedAt != nil
	if before.ClosedAt != nil {
		// A closed issue takes only its rank (board order), its component attachment
		// (classified without reopening), and an external_links write that adds no URL the issue
		// does not already have (so a link can move to the issue that replaced it; a same-set
		// resend or a kept URL's retyped kind rewrites the rows and emits issue.updated the same
		// as any other accepted write here, it just adds nothing new) — alone or beside a
		// reopening status; every other field, its repository included, and closing it again,
		// waits for a reopen.
		addsLink := input.ExternalLinks != nil && !isExternalLinksSubset(*input.ExternalLinks, before.ExternalLinks)
		others := input.Title != nil || input.Labels != nil || input.LabelsAdd != nil || priorityProvided || input.Route != nil || addsLink || assigneeProvided || parentProvided || input.BlockedBy != nil || input.Advances != nil || repoProvided
		if others || (input.Status != nil && !reopening) || (input.Status == nil && input.Rank == nil && !componentsProvided && input.ExternalLinks == nil) {
			return issueWriteResult{}, errorf(http.StatusConflict, "ISSUE_CLOSED", "issue is closed")
		}
	}
	// labels_add merges into the labels the issue holds under its owner lock. The merge is made
	// here, so the rule's plan below holds the labels the write leaves, and a merge past the cap
	// is refused where the row's columns are set, below, so the refusals keep their order.
	var labelsMerged error
	if input.LabelsAdd != nil {
		labels, labelsMerged = mergeIssueLabels(before.Labels, labelsAdd)
	}
	afterParent := before.Parent
	if parentProvided {
		afterParent = parent
	}
	statusChanged := input.Status != nil && before.Status != status
	parentChanged := parentProvided && !stringPointersEqual(before.Parent, afterParent)
	var labelsAfter *[]string
	if input.Labels != nil || (input.LabelsAdd != nil && labelsMerged == nil) {
		labelsAfter = &labels
	}
	var statusAfter *string
	if input.Status != nil {
		statusAfter = &status
	}
	// The status guard judges every write, including a same-status write or a label edit.
	// It runs before the parent-status rule, even when that rule is off.
	plan := issueWritePlanOfPatch(before, statusAfter, parentProvided, parent, labelsAfter)
	plan.unjudged = !s.deps.ParentStatusRule.On()
	if step != nil {
		plan.options = step.options
	}
	conflict, err := s.refuseIssueWritePlan(ctx, tx, call.writer, plan)
	if err != nil {
		return issueWriteResult{}, err
	}
	if conflict != nil {
		return issueWriteResult{}, conflict
	}
	if input.RefuseOpenForeignChildren {
		// An open child of another project refuses the acceptance with 409 FOREIGN_CHILD_OPEN,
		// which writeHandlerError answers with the children it names.
		if err := refuseOpenForeignChildren(ctx, tx, key, before.Project); err != nil {
			return issueWriteResult{}, err
		}
	}
	// The check of who writes an issue Legion runs (spec_writer.go): a move under a parent, a
	// close, a reopen, and a write of the labels or the parent that takes the issue out of
	// Legion. It runs here, under the owner locks and after the write's other refusals, and
	// before the close's and the reopen's document work and the row update, so a refused write
	// writes none of its fields.
	if err := refuseSpecWriterOfIssueWrite(ctx, tx, call.writer.specWriter(), plan.steps[0]); err != nil {
		return issueWriteResult{}, err
	}
	// The `advances` revision rule is decided here, beside the other refusals of this handler
	// and before the first row is written: a refused write leaves the issue exactly as it was.
	// The held set and the revision are the issue as this write read it under its owner lock,
	// which every writer of the issue's own relations takes (an issue create and an issue patch:
	// writeIssueAdvances), so two writes of one issue decide the rule one after the other rather
	// than together. Its read is a plain one, so the rule holds no transaction id.
	var heldAdvances []string
	if input.Advances != nil {
		heldAdvances = ownAdvances(before.Advances)
		if err := refuseAdvancesRevision(heldAdvances, before.AdvancesRevision, input.AdvancesRevision, advances); err != nil {
			return issueWriteResult{}, err
		}
	}
	// The default assignee of the issue's project, for an assignee move that leaves one side
	// holding nobody: a question on an issue nobody holds reaches its project's default, the
	// chain's last step (askedOfPersonExpression), so that side of the move names the default
	// rather than nobody (movedAssignees below). Without it, an issue nobody held gaining an
	// assignee would name only the new one, and the default's open Inbox would keep a row the
	// server no longer gives them. It is read here, under the issue's owner lock and before the
	// patch's first write, so it holds no transaction id; a patch that names no assignee, or one
	// whose both sides name a person or both nobody, reads no project.
	var projectDefault string
	if assigneeProvided && (before.Assignee == nil) != (assignee == nil) {
		if err := tx.QueryRow(ctx, `select default_assignee from projects where key = $1`, before.Project).Scan(&projectDefault); err != nil {
			return issueWriteResult{}, err
		}
	}
	// Closing an issue releases whatever claim it held: the work is finished, so nobody is
	// on it. No other status move touches the claim — a claim is an explicit act
	// (issue_claim.go), and the status is also how humans track work.
	// previousClaim is set only when this write closes the issue, so it alone says both that a
	// claim is being released and whose it was.
	var previousClaim *model.IssueClaim
	if closing {
		previousClaim = before.Claim
	}
	// Every provided column lands in one UPDATE, updated_at with it; a components-only PATCH
	// still touches updated_at. The row write runs before the components write so a component
	// validation error surfaces after it, and the transaction rolls both back. updated_at is the
	// statement's own moment (issueUpdatedNow), never the transaction's start: the locks above may
	// have waited while another write committed a later stamp on this issue.
	var sets []string
	args := []any{key}
	set := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = $"+strconv.Itoa(len(args)))
	}
	if input.Title != nil {
		set("title", *input.Title)
	}
	if input.Status != nil {
		args = append(args, status)
		placeholder := "$" + strconv.Itoa(len(args))
		sets = append(sets, "status = "+placeholder, "closed_at = case when "+placeholder+" = 'done' then now() else null end")
	}
	if input.Rank != nil {
		issueRank, err := s.rankForInput(ctx, tx, before.Project, key, *input.Rank)
		if err != nil {
			return issueWriteResult{}, err
		}
		set("rank", issueRank)
	}
	if priorityProvided {
		set("priority", priority)
	}
	if assigneeProvided {
		set("assignee", assignee)
	}
	if parentProvided {
		var parentValue any
		if parent != nil {
			parentValue = *parent
		}
		set("parent_key", parentValue)
	}
	if input.LabelsAdd != nil {
		if labelsMerged != nil {
			return issueWriteResult{}, labelsMerged
		}
		set("labels", labels)
	}
	if input.Labels != nil {
		set("labels", labels)
	}
	if input.Route != nil {
		var route any
		if *input.Route != "" {
			route = *input.Route
		}
		set("route", route)
	}
	if repoProvided {
		set("repo", repo)
	}
	changed := len(sets) > 0 || componentsProvided || input.BlockedBy != nil || input.Advances != nil
	// The close's and the reopen's document work (prepareWithdrawal, prepareRestoration): it reads
	// the asks, which none of the patch's row writes below touches, and queues its writes. It runs
	// before the advice so the advice knows which asks the patch withdraws or restores; its writes
	// are queued, not made, so the advice's queries still hold no transaction id. The reopen's
	// second half runs here too (recordRestoredAsksTx): its follow of the versions a settlement
	// wrote while the issue was closed reads and renders those versions, document work that must
	// not run once the issue's update below has given the transaction its id, and it plans the
	// restored asks' parts before the follow plans the parts of the request it moves. Its writes
	// and events queue after the document writes and before everything the patch queues below,
	// which is where its writes run in the write phase.
	var closingAsks, reopenedAsks []model.Ask
	var restoredEvents []*model.Event
	switch {
	case closing:
		closingAsks, err = s.prepareWithdrawal(documentCtx, tx, key, actor)
	case reopening:
		if reopenedAsks, err = s.prepareRestoration(documentCtx, tx, key, actor); err == nil {
			restoredEvents, err = s.recordRestoredAsksTx(documentCtx, ledger, key, reopenedAsks, actor)
		}
	}
	if err != nil {
		return issueWriteResult{}, err
	}
	// The advice is read here, before the patch's first write, so its queries hold no transaction
	// id (INFRA-636), and it is read for the issue as the patch leaves it: its status, its labels
	// and its parent (writeAdviceBefore), and the open asks the close withdraws or the reopen
	// restores, whose rows the patch writes afterwards (adviceAsks). Nothing else the patch writes
	// changes what it reads.
	afterRow := adviceIssueRow{project: before.Project, parent: afterParent, labels: before.Labels}
	afterStatus := before.Status
	if input.Status != nil {
		afterStatus = status
	}
	if labelsAfter != nil {
		afterRow.labels = *labelsAfter
	}
	advice := s.writeAdviceBefore(ctx, tx, call.adviceRoute, key, actor, afterStatus, afterRow, adviceAsks{
		excluded: askIDs(closingAsks),
		opened:   askIDs(reopenedAsks),
	})
	// The write phase: the statements from here on run back to back.
	if changed {
		sets = append(sets, "updated_at = "+issueUpdatedNow)
		if _, err := tx.Exec(ctx, `update issues set `+strings.Join(sets, ", ")+` where key = $1`, args...); err != nil {
			return issueWriteResult{}, err
		}
	}
	if closing && s.deps.ParentStatusRule.On() {
		// The close's second read of R24 (openChildrenOfAClose): a create under this issue whose
		// share lock of its row came first made the update above wait for its commit, and this
		// read sees that new child.
		conflict, err := openChildrenOfAClose(ctx, tx, key)
		if err != nil {
			return issueWriteResult{}, err
		}
		if conflict != nil {
			return issueWriteResult{}, conflict
		}
	}
	if previousClaim != nil {
		// Closing releases the claim through the one writer of those columns.
		if err := writeIssueClaim(ctx, tx, key, nil); err != nil {
			return issueWriteResult{}, err
		}
	}
	if componentsProvided {
		if err := writeIssueComponents(ctx, tx, key, before.Project, *components); err != nil {
			return issueWriteResult{}, err
		}
	}
	if input.BlockedBy != nil {
		if err := writeBlockedBy(ctx, tx, key, before.Project, call.callerProject, blockers); err != nil {
			return issueWriteResult{}, err
		}
	}
	if input.Advances != nil {
		if err := writeIssueAdvances(ctx, tx, key, heldAdvances, advances, actor, time.Now().UTC()); err != nil {
			return issueWriteResult{}, err
		}
	}
	if parentProvided && parent != nil {
		if err := assertParentLeavesNoDependencyCycle(ctx, tx, key, *parent); err != nil {
			return issueWriteResult{}, err
		}
	}

	if input.ExternalLinks != nil {
		if _, err := tx.Exec(ctx, `delete from issue_external_links where issue_key = $1`, key); err != nil {
			return issueWriteResult{}, err
		}
		for _, link := range *input.ExternalLinks {
			if err := validateExternalURL(link.URL); err != nil {
				return issueWriteResult{}, err
			}
			kind := link.Kind
			if kind == "" {
				kind = "url"
			}
			// A URL links exactly one issue (issue_external_links_url). This issue's own
			// links are already deleted above, so an owner found here is another issue
			// (or the same URL repeated earlier in this request).
			var owner string
			err := tx.QueryRow(ctx, `select issue_key from issue_external_links where url = $1`, link.URL).Scan(&owner)
			if err == nil {
				return issueWriteResult{}, errorf(http.StatusConflict, "EXTERNAL_LINK_TAKEN", "%s is already linked from %s", link.URL, owner)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return issueWriteResult{}, err
			}
			if _, err := tx.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, $3)`, key, link.URL, kind); err != nil {
				if isUniqueViolation(err) {
					return issueWriteResult{}, errorf(http.StatusConflict, "EXTERNAL_LINK_TAKEN", "%s is already linked from another issue", link.URL)
				}
				return issueWriteResult{}, err
			}
		}
		changed = true
	}
	if !changed {
		// Nothing was written, so the row the advice read is the stored one.
		if err := ledger.Commit(ctx); err != nil {
			return issueWriteResult{}, err
		}
		return issueWriteResult{issue: before, advice: advice, previousLastSeq: before.LastSeq}, nil
	}
	// Every owner this transaction appends to must be in one LockOwners call before the
	// first append: the issue itself, the status-change parent, and both ends of a reparent.
	// lockPatchOwnersAndParent already holds each of those owner locks, so LockOwners returns at once.
	owners := []model.Event{issueOwner(key).event("", actor, nil)}
	ownerKeys := map[string]bool{key: true}
	addOwner := func(parentKey *string) {
		if parentKey == nil || ownerKeys[*parentKey] {
			return
		}
		ownerKeys[*parentKey] = true
		owners = append(owners, issueOwner(*parentKey).event("", actor, nil))
	}
	if statusChanged {
		addOwner(afterParent)
	}
	if parentChanged {
		addOwner(before.Parent)
		addOwner(afterParent)
	}
	if len(owners) > 1 {
		if err := s.deps.Events.LockOwners(ctx, tx, owners...); err != nil {
			return issueWriteResult{}, err
		}
	}
	// The asks' rows and events come before the status event, and their document writes, queued
	// above, before either. A reopen's are queued above already, with its follow's.
	var withdrawn []model.Ask
	appended := append([]*model.Event{}, restoredEvents...)
	if closing {
		if withdrawn, appended, err = s.recordWithdrawnAsksTx(documentCtx, tx, ledger, key, closingAsks, actor); err != nil {
			return issueWriteResult{}, err
		}
	}
	// The row as the patch's inline writes left it. Its last_seq is the sequence of the event
	// stored before this transaction's: every event the write phase appends on the issue is still
	// queued, the asks' and the ones their document writes queued beside them alike, so the
	// response's last_seq and the status event's payload are filled in from what those appends
	// took (appendedIssueLastSeq, model.IssueEventPayload.WithOwnerLastSeq) rather than counted
	// here. A close's closed_at is its first stamp here, the start of its transaction: the stamp
	// queued before the status event below is the row's final one, which that event's insert and
	// the response take.
	after, err := s.loadIssue(ctx, tx, key)
	if err != nil {
		return issueWriteResult{}, err
	}
	eventType := "issue.updated"
	if statusChanged && after.Status == "done" {
		eventType = "issue.closed"
	}
	// What the write changed, the issue's ancestors, and, for a status or an assignee move, whose
	// Inbox rows it can move. A retitle keeps the issue's child_of and blocked_by edges but moves
	// the excerpt they show, so it names their targets; a blocked_by change moves the count of
	// each blocker it added or removed, so it names those.
	changedFields := issueChangedFields(before, after)
	edgeChanges, err := issueEdgeChanges(ctx, tx, before, after, slices.Contains(changedFields, "title"))
	if err != nil {
		return issueWriteResult{}, err
	}
	payload, err := newIssueEventPayload(ctx, tx, after, edgeChanges, changedFields)
	if err != nil {
		return issueWriteResult{}, err
	}
	// nil on a write that left the assignee alone, which is what names the status move's logins
	// and nobody else. A move names the person the issue's questions reached before it and the
	// person they reach after it: the assignee on each side, and the project's default, read
	// above, on a side where the issue holds nobody, as askedOfPersonExpression resolves it.
	var movedAssignees []string
	if slices.Contains(changedFields, "assignee") {
		reached := func(side *string) string {
			if side == nil {
				return projectDefault
			}
			return *side
		}
		movedAssignees = []string{reached(before.Assignee), reached(after.Assignee)}
	}
	// A close or a reopen names the logins with a row on the issue's open asks once the asks'
	// rows are written, and those are still queued here, so the event's own insert reads them and
	// unions in the people the assignee move moved (events.IssueLoginsOf). Any other move names
	// only those people, which this transaction already knows. The Replies list leaves out an
	// open ask on an open issue that waits on the reader (api/replies.go, myRepliesOwner), so
	// moving the assignee takes the asks nobody has handed on out of one person's list and puts
	// them into the other's; an ask somebody handed on waits on that person wherever its issue
	// goes, so the move leaves its row where it is.
	var logins *events.Logins
	if statusChanged || movedAssignees != nil {
		if statusChanged && slices.Contains(changedFields, "closed_at") {
			logins = events.IssueLoginsOf(key, movedLogins(nil, movedAssignees...)...)
		} else {
			payload.ThreadLogins = movedLogins(nil, movedAssignees...)
		}
	}
	if closing {
		// The close stamps closed_at again, at the moment the statement runs (clock_timestamp()),
		// over the start of the transaction (now()) that the update above wrote. The document
		// append gate takes a browser's update the room observed no later than closeMargin after
		// closed_at as one made while the issue was open (docs.closeMargin). The asks' withdrawal
		// holds each document it writes locked until this commit, a flush to every process and the
		// waits on other writers included. A keystroke that waited on that lock reaches the gate
		// only after the commit, and was made while the issue was still open, however long the
		// withdrawal ran. So the stamp must come after every document write of the close, and it
		// does. The withdrawal queued its document writes in the read phase (prepareWithdrawal),
		// and the asks' rows and events after them (recordWithdrawnAsksTx). No statement after this
		// one writes a document: the events below, the call's comment, which has no anchor and
		// names no artifact (queueIssueComment), a step's read of its events, the statements that
		// Commit puts after the queue (the settlement credit's rows and the announcement of the
		// documents appended to, docs.Ledger.commitWrites), and a keyed step's recovery point after
		// the batch. They run in the moment between the stamp and the commit that closeMargin
		// covers.
		//
		// The stamp is queued right before the status event, whose insert takes closed_at from the
		// row (events.Broker.QueueAppend). So the stored issue.closed and its copy that is
		// published after the commit carry this stamp, which is the closed_at that a read at a past
		// time gives (asof's issuesShadow). So do the response, which the statement's own callback
		// reads as the batch runs, and a keyed step's entry, which its point stores once the batch
		// has run. The second write of closed_at fires migration 20261101181500's trigger again,
		// whose notification Postgres folds into the first (same channel, same payload, one
		// transaction).
		if err := ledger.Queue(func(b *store.WriteBatch) error {
			b.Queue("", `
				update issues set closed_at = clock_timestamp() where key = $1 and closed_at is not null returning closed_at
			`, key).QueryRow(func(row pgx.Row) error { return row.Scan(&after.ClosedAt) })
			return nil
		}); err != nil {
			return issueWriteResult{}, err
		}
	}
	event, err := s.queueLoggedEvent(ledger, issueOwner(key).event(eventType, actor, payload), logins, "", "")
	if err != nil {
		return issueWriteResult{}, err
	}
	appended = append(appended, event)
	if previousClaim != nil {
		released, err := s.queueEvent(ledger, claimEvent("issue.released", key, actor, after, previousClaim, "closed"))
		if err != nil {
			return issueWriteResult{}, err
		}
		appended = append(appended, released)
	}
	if statusChanged && after.Parent != nil {
		childEvent, err := s.queueEvent(ledger, issueOwner(*after.Parent).event(
			"child.status",
			actor,
			map[string]any{"child_key": after.Key, "from": before.Status, "to": after.Status},
		))
		if err != nil {
			return issueWriteResult{}, err
		}
		appended = append(appended, childEvent)
	}
	if parentChanged {
		if before.Parent != nil {
			removedEvent, err := s.queueEvent(ledger, issueOwner(*before.Parent).event(
				"child.removed", actor, map[string]any{"child_key": after.Key},
			))
			if err != nil {
				return issueWriteResult{}, err
			}
			appended = append(appended, removedEvent)
		}
		if after.Parent != nil {
			addedEvent, err := s.queueEvent(ledger, issueOwner(*after.Parent).event(
				"child.added", actor, map[string]any{"child_key": after.Key},
			))
			if err != nil {
				return issueWriteResult{}, err
			}
			appended = append(appended, addedEvent)
		}
	}
	if call.comment != nil {
		// The call's comment comes after the write's own events, on the issue's owner, which the
		// write's lock holds. Its body is the one the row holds, which the comment route's checks
		// leave (queueIssueComment).
		commented, stored, err := s.queueIssueComment(ctx, tx, ledger, key, call.comment.author, commentBody)
		if err != nil {
			return issueWriteResult{}, err
		}
		appended = append(appended, commented)
		commentBody = stored
	}
	// A step that holds the named issue's lock reads back the ids of the events on it after the
	// last_seq it read under that lock. The ids exist only once the write phase's batch has run,
	// so the statement derives them in its own SQL, after the step's last event insert. They hold
	// what document work appended there through the ledger's event sink too, which appended does
	// not. A step of a keyed call reads them with the statement that writes the call's recovery
	// point (queueStatusChangeStep), run right after the batch rather than on it
	// (docs.Ledger.AfterWrites), with the issue as this write answers it - its last_seq the one its
	// own events reached and a close's closed_at the stamp queued before its status event - which
	// are known only once the batch's callbacks have read them. A step that holds no lock of the
	// named issue adds no ids, and an unkeyed step writes no point.
	var namedEvents []int64
	if step != nil {
		switch {
		case step.claim != nil:
			var closedAsks []string
			if closing && key == step.named {
				closedAsks = askIDs(withdrawn)
			}
			if err := ledger.AfterWrites(func(b *store.WriteBatch) error {
				answered := after
				answered.LastSeq = appendedIssueLastSeq(key, appended)
				return queueStatusChangeStep(b, step, answeredStatusChangeEntry(answered, before.LastSeq), moved,
					namedLastSeq, holdsNamed, closedAsks, &namedEvents)
			}); err != nil {
				return issueWriteResult{}, err
			}
		case holdsNamed:
			if err := ledger.Queue(func(b *store.WriteBatch) error {
				b.Queue("read back the events of the named issue", `
					select coalesce(array_agg(id order by id), '{}') from events where issue_key = $1 and seq > $2
				`, step.named, namedLastSeq).QueryRow(func(row pgx.Row) error { return row.Scan(&namedEvents) })
				return nil
			}); err != nil {
				return issueWriteResult{}, err
			}
		}
	}
	if err := ledger.Commit(ctx); err != nil {
		return issueWriteResult{}, err
	}
	after.LastSeq = appendedIssueLastSeq(key, appended)
	// A change of closed_at reaches the document rooms of every process, this one's included, from
	// the commit itself: migration 20261101181500's trigger notifies, and each process closes its
	// rooms of the issue's documents so their browsers reconnect with the scope the issue now
	// allows.
	s.publish(ctx, appendedEvents(appended...)...)
	if call.comment != nil {
		s.storeDisplayTrees(ctx, key, commentBody)
	}
	if closing && (step == nil || !step.sweepsLater) {
		// The commit's notification closes the issue's rooms on every process; the sweep catches
		// the ask blocks written since the close forked each document (sweepClosedIssueAsks).
		// It runs after the publish so every event reaches subscribers in commit order. A step of
		// a status change that sweeps later leaves it to its caller, which runs it after the
		// call's last step with the asks this answers.
		s.sweepClosedIssueAsks(call.outliving, key, withdrawn, actor)
	}
	return issueWriteResult{
		issue: after, advice: advice, previousLastSeq: before.LastSeq, wrote: true, appended: appended,
		namedEvents: namedEvents, closed: closing, withdrawn: withdrawn, moved: moved,
	}, nil
}

// appendedIssueLastSeq is the issue's last_seq once the write phase has run: the sequence of the
// last event it appended on the issue's own owner row, which that insert took from the row and
// wrote back to it. It is read from the events rather than counted, because a count of what the
// route queued misses what the asks' document writes queued beside them - the anchor refreshes of
// a document a close or a reopen rewrote - which the same owner row numbers too (INFRA-636).
// appended holds the events as the write phase left them, so this runs once its batch has run:
// after Commit, or in a step that follows the batch (docs.Ledger.AfterWrites).
func appendedIssueLastSeq(key string, appended []*model.Event) int {
	last := 0
	for _, event := range appended {
		if event.IssueKey != nil && *event.IssueKey == key && event.Seq > last {
			last = event.Seq
		}
	}
	return last
}

// askIDs is the ids of asks, in their order.
func askIDs(asks []model.Ask) []string {
	ids := make([]string, 0, len(asks))
	for _, ask := range asks {
		ids = append(ids, ask.ID)
	}
	return ids
}

// mergeIssueLabels adds labels to existing without dropping a concurrent writer's labels. The total
// still passes through the same cap and de-duplication as a full labels replacement.
func mergeIssueLabels(existing, added []string) ([]string, error) {
	return normalizeIssueLabels(append(append([]string{}, existing...), added...))
}

// isExternalLinksSubset reports whether every URL in requested already belongs to existing. The
// closed-issue gate (above) is what this decides.
func isExternalLinksSubset(requested, existing []model.ExternalLink) bool {
	existingURLs := make(map[string]bool, len(existing))
	for _, link := range existing {
		existingURLs[link.URL] = true
	}
	for _, link := range requested {
		if !existingURLs[link.URL] {
			return false
		}
	}
	return true
}

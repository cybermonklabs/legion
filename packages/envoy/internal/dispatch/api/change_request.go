package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/specchange"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"github.com/sjawhar/envoy/internal/dispatch/specstore"
)

// The change request of an issue and its end (D6, D10 of the work-units plan; R23, R26, R27).
// GET /api/v1/issues/{key}/change-request reads what a version of an issue's spec proposes against
// the approved spec of its repository, which checks its Verification document changes, and the
// spec's conclusion, judged against the reviewer's final list (specchange.Judge). The three lists
// of outcomes of a pull request's head are records of their own (spec_outcome_records), written by
// POST /api/v1/issues/{key}/outcomes and read by GET /api/v1/issues/{key}/outcomes. GET
// /api/v1/issues/{key}/change-request/ending answers whether the change may land at a head: the
// Legion daemon's merger fence and a merger's own command read it for a tree whose design gate
// opened with no person, because its change request proposes nothing.

// specOutcomesRecordedEvent is the event a recorded list of outcomes appends on its issue, which
// the daemon reads to judge again the ending of a change in merging.
const specOutcomesRecordedEvent = "spec.outcomes_recorded"

// The three lists of outcomes of a pull request's head (R27): the author's, the reviewer's own,
// written before the reviewer reads the author's, and the reviewer's final list.
const (
	outcomeListAuthor   = "author"
	outcomeListReviewer = "reviewer"
	outcomeListFinal    = "final"
)

// The reasons the ending of a change waits (D10, R25).
const (
	endingNoFinalListForHead           = "no_final_list_for_head"
	endingFinalListOfAnotherHead       = "final_list_of_another_head"
	endingNoConclusion                 = "no_conclusion"
	endingIncomplete                   = "incomplete"
	endingUncoveredWithoutApproval     = "uncovered_without_approval"
	endingProposesWithoutApproval      = "proposes_without_approval"
	endingChangesChecksWithoutApproval = "changes_checks_without_approval"
)

// recordOutcomesInput is the body of POST /api/v1/issues/{key}/outcomes.
type recordOutcomesInput struct {
	PullRequest string              `json:"pull_request" required:"true"`
	Head        string              `json:"head" required:"true"`
	List        string              `json:"list" required:"true" enum:"author,reviewer,final"`
	Outcomes    []model.SpecOutcome `json:"outcomes" required:"true"`
	Actor       *model.Actor        `json:"actor"`
}

// outcomeRecordColumns selects one spec_outcome_records row in the order scanOutcomeRecord reads
// it.
const outcomeRecordColumns = `id, issue_key, pull_request, head, list, outcomes, actor, recorded_at`

func scanOutcomeRecord(row pgx.Row) (model.SpecOutcomeRecord, error) {
	var record model.SpecOutcomeRecord
	var outcomes, actor []byte
	if err := row.Scan(&record.ID, &record.Issue, &record.PullRequest, &record.Head, &record.List, &outcomes, &actor, &record.RecordedAt); err != nil {
		return model.SpecOutcomeRecord{}, err
	}
	if err := json.Unmarshal(outcomes, &record.Outcomes); err != nil {
		return model.SpecOutcomeRecord{}, fmt.Errorf("decode the outcomes of record %d: %w", record.ID, err)
	}
	if err := json.Unmarshal(actor, &record.Actor); err != nil {
		return model.SpecOutcomeRecord{}, fmt.Errorf("decode the actor of record %d: %w", record.ID, err)
	}
	if record.Outcomes == nil {
		record.Outcomes = []model.SpecOutcome{}
	}
	return record, nil
}

// pullRequestOf reads raw as `owner/name#number` into the form a record stores it in
// (refs.ParseExternalRef), or refuses it 400 INVALID_PULL_REQUEST.
func pullRequestOf(raw string) (string, error) {
	repo, number, ok := refs.ParseExternalRef(raw)
	if !ok {
		return "", errorf(http.StatusBadRequest, "INVALID_PULL_REQUEST",
			"pull_request %q is not a pull request: give it as owner/repo#number, such as acme/app#12", raw)
	}
	return repo + "#" + number, nil
}

// headOf refuses head 400 INVALID_HEAD unless it is a whole commit id, 40 lowercase hex
// characters (resultCommitSHA: the rule a check result's commit is held to).
func headOf(head string) error {
	if !resultCommitSHA.MatchString(head) {
		return errorf(http.StatusBadRequest, "INVALID_HEAD",
			"head %q is not a commit: give the 40 lowercase hex characters of the pull request's head", head)
	}
	return nil
}

// listOf refuses list 400 INVALID_LIST unless it is one of the three lists.
func listOf(list string) error {
	switch list {
	case outcomeListAuthor, outcomeListReviewer, outcomeListFinal:
		return nil
	}
	return errorf(http.StatusBadRequest, "INVALID_LIST", "list %q is not a list: give author, reviewer or final", list)
}

// outcomesOf is the outcomes of a record as it stores them, each text trimmed, or the refusal 400
// INVALID_OUTCOME of the first fault specchange.CheckOutcomes finds. A missing list is refused:
// an empty list is the record of a list with no outcome, which a writer sends as [].
func outcomesOf(outcomes []model.SpecOutcome) ([]model.SpecOutcome, error) {
	if outcomes == nil {
		return nil, errorf(http.StatusBadRequest, "INVALID_OUTCOME", "outcomes is required: send [] to record an empty list")
	}
	if err := specchange.CheckOutcomes(specOutcomes(outcomes)); err != nil {
		return nil, errorf(http.StatusBadRequest, "INVALID_OUTCOME", "%s", err.Error())
	}
	stored := make([]model.SpecOutcome, len(outcomes))
	for index, outcome := range outcomes {
		stored[index] = model.SpecOutcome{Text: strings.TrimSpace(outcome.Text), Statement: outcome.Statement, Restored: outcome.Restored}
	}
	return stored, nil
}

// specOutcomes is a record's outcomes as specchange judges them.
func specOutcomes(outcomes []model.SpecOutcome) []specchange.Outcome {
	judged := make([]specchange.Outcome, len(outcomes))
	for index, outcome := range outcomes {
		judged[index] = specchange.Outcome{Text: outcome.Text, Statement: outcome.Statement, Restored: outcome.Restored}
	}
	return judged
}

// issueNotFound is the refusal of a route of an issue Dispatch does not hold.
func issueNotFound(key string) error {
	return errorf(http.StatusNotFound, "ISSUE_NOT_FOUND", "no issue %s", key)
}

// POST /api/v1/issues/{key}/outcomes
//
// recordOutcomes records one list of outcomes of a pull request's head for the issue, as a row of
// its own, and appends spec.outcomes_recorded on the issue in the same transaction. A writer of
// the issue records it: a person, or a session naming itself in actor. The lists are R27's three,
// and only an independent reviewer writes the final list. The ending reads the final list from
// any issue of the tree (finalListQuery), so who wrote what of a pull request is read from every
// record of that pull request, whichever issue holds it, under a lock on the pull request, so that
// two records of one pull request are judged one after the other. The record refuses a reviewer or
// final list from an actor that recorded the author's list of the pull request (403
// OUTCOME_LIST_BY_AUTHOR), an author's list from an actor that recorded a reviewer or final list
// of it (403 OUTCOME_LIST_BY_REVIEWER), a final list while no author's list of the pull request at
// that head is recorded (409 OUTCOME_AUTHOR_LIST_FIRST), and a final list from an actor that
// recorded no reviewer's list at that head (409 OUTCOME_REVIEWER_LIST_FIRST). The reviewer's own
// list needs no author's list: the reviewer writes it before it reads the author's. A closed issue
// takes a record too: the list of a merged head is a fact of the merge.
func (s *server) recordOutcomes(w http.ResponseWriter, r *http.Request) {
	var input recordOutcomesInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	pullRequest, err := pullRequestOf(input.PullRequest)
	if err == nil {
		err = headOf(input.Head)
	}
	if err == nil {
		err = listOf(input.List)
	}
	var outcomes []model.SpecOutcome
	if err == nil {
		outcomes, err = outcomesOf(input.Outcomes)
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	record := model.SpecOutcomeRecord{Issue: r.PathValue("key"), PullRequest: pullRequest, Head: input.Head, List: input.List, Outcomes: outcomes, Actor: actor}
	written, event, err := s.writeOutcomeRecord(r.Context(), record)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), event)
	WriteJSON(w, http.StatusCreated, written)
}

// writeOutcomeRecord writes record and its event in one transaction under the issue's owner lock
// and the pull request's lock (lockPullRequestOutcomes), after the refusals recordOutcomes names,
// and answers the row and the event to publish.
func (s *server) writeOutcomeRecord(ctx context.Context, record model.SpecOutcomeRecord) (model.SpecOutcomeRecord, model.Event, error) {
	outcomes, err := json.Marshal(record.Outcomes)
	if err != nil {
		return model.SpecOutcomeRecord{}, model.Event{}, err
	}
	actor, err := json.Marshal(record.Actor)
	if err != nil {
		return model.SpecOutcomeRecord{}, model.Event{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return model.SpecOutcomeRecord{}, model.Event{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := s.lockIssue(ctx, tx, record.Issue); errors.Is(err, pgx.ErrNoRows) {
		return model.SpecOutcomeRecord{}, model.Event{}, issueNotFound(record.Issue)
	} else if err != nil {
		return model.SpecOutcomeRecord{}, model.Event{}, err
	}
	if err := lockPullRequestOutcomes(ctx, tx, record.PullRequest); err != nil {
		return model.SpecOutcomeRecord{}, model.Event{}, err
	}
	// What the actor and every writer recorded of this pull request before, on any issue, read
	// after the lock: whether the actor recorded the author's list, a reviewer's or final list, or
	// a reviewer's list at this head, and whether anyone recorded the author's list at this head.
	var authored, reviewed, reviewedHead, authorListed bool
	if err := tx.QueryRow(ctx, `
		select
			coalesce(bool_or(list = 'author' and mine), false),
			coalesce(bool_or(list in ('reviewer', 'final') and mine), false),
			coalesce(bool_or(list = 'reviewer' and head = $4 and mine), false),
			coalesce(bool_or(list = 'author' and head = $4), false)
		from (
			select list, head, actor->>'kind' = $2 and actor->>'id' = $3 as mine
			from spec_outcome_records where pull_request = $1
		) held
	`, record.PullRequest, record.Actor.Kind, record.Actor.ID, record.Head).Scan(&authored, &reviewed, &reviewedHead, &authorListed); err != nil {
		return model.SpecOutcomeRecord{}, model.Event{}, fmt.Errorf("read the lists recorded of %s: %w", record.PullRequest, err)
	}
	switch {
	case record.List == outcomeListAuthor && reviewed:
		return model.SpecOutcomeRecord{}, model.Event{}, errorf(http.StatusForbidden, "OUTCOME_LIST_BY_REVIEWER",
			"you recorded a reviewer's list of %s, so you cannot record its author's list. The author records it.", record.PullRequest)
	case record.List != outcomeListAuthor && authored:
		return model.SpecOutcomeRecord{}, model.Event{}, errorf(http.StatusForbidden, "OUTCOME_LIST_BY_AUTHOR",
			"you recorded the author's list of %s, so you cannot record its %s list. An independent reviewer records it.", record.PullRequest, record.List)
	case record.List == outcomeListFinal && !authorListed:
		return model.SpecOutcomeRecord{}, model.Event{}, errorf(http.StatusConflict, "OUTCOME_AUTHOR_LIST_FIRST",
			"no author's list of %s at head %s is recorded. The author records its list at that head first; then record the final list.",
			record.PullRequest, record.Head)
	case record.List == outcomeListFinal && !reviewedHead:
		return model.SpecOutcomeRecord{}, model.Event{}, errorf(http.StatusConflict, "OUTCOME_REVIEWER_LIST_FIRST",
			"you recorded no reviewer's list of %s at head %s. Record your own list at that head before you read the author's list; then record the final list.",
			record.PullRequest, record.Head)
	}
	if err := tx.QueryRow(ctx, `
		insert into spec_outcome_records (issue_key, pull_request, head, list, outcomes, actor)
		values ($1, $2, $3, $4, $5::jsonb, $6::jsonb)
		returning id, recorded_at
	`, record.Issue, record.PullRequest, record.Head, record.List, outcomes, actor).Scan(&record.ID, &record.RecordedAt); err != nil {
		return model.SpecOutcomeRecord{}, model.Event{}, fmt.Errorf("record the %s list of %s for %s: %w", record.List, record.PullRequest, record.Issue, err)
	}
	event, err := s.appendEvent(ctx, tx, issueOwner(record.Issue).event(specOutcomesRecordedEvent, record.Actor, model.SpecOutcomesRecordedEventPayload{
		Issue: record.Issue, Record: record.ID, PullRequest: record.PullRequest, Head: record.Head, List: record.List, Outcomes: len(record.Outcomes),
	}))
	if err != nil {
		return model.SpecOutcomeRecord{}, model.Event{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.SpecOutcomeRecord{}, model.Event{}, err
	}
	return record, event, nil
}

// lockPullRequestOutcomes takes, for the rest of tx, the lock on the lists of outcomes of
// pullRequest, in the form a record stores it (pullRequestOf): the scope its writer refusals read,
// every issue of every tree, so two records of one pull request on two issues are judged one after
// the other. It is taken after the issue's owner lock (lockIssue), the one order every writer of a
// record takes the two in. hashtext is 32 bits wide, so two pull requests can share the key; they
// then take turns on their records and on nothing else.
func lockPullRequestOutcomes(ctx context.Context, tx pgx.Tx, pullRequest string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('dispatch-spec-outcomes'), hashtext($1))`, pullRequest); err != nil {
		return fmt.Errorf("lock the lists of outcomes of %s: %w", pullRequest, err)
	}
	return nil
}

// GET /api/v1/issues/{key}/outcomes
//
// listOutcomeRecords answers every list of outcomes recorded for the issue, oldest first,
// narrowed by ?pull_request=, ?head= and ?list= where they are given.
func (s *server) listOutcomeRecords(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key, query := r.PathValue("key"), r.URL.Query()
	var pullRequest string
	var err error
	if raw := query.Get("pull_request"); raw != "" {
		pullRequest, err = pullRequestOf(raw)
	}
	head, list := query.Get("head"), query.Get("list")
	if err == nil && head != "" {
		err = headOf(head)
	}
	if err == nil && list != "" {
		err = listOf(list)
	}
	pool, poolErr := s.readyPool()
	if err == nil {
		err = poolErr
	}
	if err == nil {
		err = requireIssue(r.Context(), pool, key)
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := pool.Query(r.Context(), `
		select `+outcomeRecordColumns+` from spec_outcome_records
		where issue_key = $1 and ($2 = '' or pull_request = $2) and ($3 = '' or head = $3) and ($4 = '' or list = $4)
		order by id
	`, key, pullRequest, head, list)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.SpecOutcomeRecord, error) { return scanOutcomeRecord(row) })
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if records == nil {
		records = []model.SpecOutcomeRecord{}
	}
	WriteJSON(w, http.StatusOK, records)
}

// requireIssue refuses an issue Dispatch does not hold, 404 ISSUE_NOT_FOUND.
func requireIssue(ctx context.Context, q queryer, key string) error {
	var held bool
	if err := q.QueryRow(ctx, `select exists(select 1 from issues where key = $1)`, key).Scan(&held); err != nil {
		return fmt.Errorf("read whether %s exists: %w", key, err)
	}
	if !held {
		return issueNotFound(key)
	}
	return nil
}

// finalListQuery reads the newest final list recorded for the issue $1 or any issue under it, at
// the head $2 where $2 is not empty. A change's work runs in its tree, so its reviewer records the
// final list on whichever issue of the tree holds the pull request.
const finalListQuery = `
	with recursive tree (key) as (
		select key from issues where key = $1
		union
		select i.key from issues i join tree t on i.parent_key = t.key
	)
	select ` + outcomeRecordColumns + ` from spec_outcome_records
	where issue_key in (select key from tree) and list = 'final' and ($2 = '' or head = $2)
	order by id desc
	limit 1`

// finalList is the newest final list recorded for key or an issue under it, at head where head is
// not "", or nil where none is.
func finalList(ctx context.Context, q queryer, key, head string) (*model.SpecOutcomeRecord, error) {
	record, err := scanOutcomeRecord(q.QueryRow(ctx, finalListQuery, key, head))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read the final list of %s: %w", key, err)
	}
	return &record, nil
}

// changeRequestUnreadable is the refusal of a version a change request cannot be read from: an
// error, never an answer that reads as a request that proposes nothing.
func changeRequestUnreadable(err error) error {
	return errorf(http.StatusConflict, "CHANGE_REQUEST_UNREADABLE",
		"%s. Dispatch cannot read the change request from it. Write a new version of the document.", err.Error())
}

// changeRequestState is a change request as the read and the ending share it: the answer, the
// spec it was read from, and the latest version of that spec.
type changeRequestState struct {
	read   model.ChangeRequestRead
	spec   model.Artifact
	latest int
}

// GET /api/v1/issues/{key}/change-request
func (s *server) getChangeRequest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	version := 0
	if query.Has("version") {
		number, err := strconv.Atoi(query.Get("version"))
		if err != nil || number < 1 {
			writeError(w, "INVALID_VERSION", http.StatusBadRequest,
				fmt.Sprintf("version %q is not a version: give a whole number from 1, or leave it out for the latest", query.Get("version")))
			return
		}
		version = number
	}
	head, headGiven := query.Get("head"), query.Has("head")
	if headGiven {
		if err := headOf(head); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	key := r.PathValue("key")
	state, err := s.readChangeRequest(r.Context(), pool, key, version, head)
	if err == nil && state.read.Conclusion != nil {
		state.read.Conclusion.Answers, err = s.conclusionAnswers(r.Context(), pool, key, state.spec.ID, state.read.Conclusion.Block)
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, state.read)
}

// readChangeRequest reads the change request of the issue key at version of its spec (0 for the
// latest), with its conclusion judged against the newest final list recorded for the issue or an
// issue under it, at head where head is not "".
//
// An issue whose repository is an enabled spec source of its project proposes each statement,
// component and interface of its spec that adds, changes, removes or is stale against the
// approved spec of that repository (specchange.Classify), and changes each check of its
// Verification document's latest settled version that does the same; a fault of a block counts as
// a proposal of its document's kind. A stale node the approved spec already holds as the block
// writes it — the issue's own request once a person's approval wrote it — is approved; every other
// proposal and changed check, and every such fault, waits for a person's approval
// (ProposesUnapproved, ChangesChecksUnapproved, R25). The read names the approved spec it compared
// against (ApprovedSpecMark, specstore.State.Mark), which another issue's approval moves while
// this issue's documents stand, so a reader can order two reads of one version. Any other issue
// proposes nothing and changes no check, reads no approved spec (mark 0), and its conclusion is
// judged with nothing approved. A version Dispatch cannot read is refused 409
// CHANGE_REQUEST_UNREADABLE, never read as one that proposes nothing.
func (s *server) readChangeRequest(ctx context.Context, q queryer, key string, version int, head string) (changeRequestState, error) {
	issue, err := s.loadIssue(ctx, q, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return changeRequestState{}, issueNotFound(key)
	}
	if err != nil {
		return changeRequestState{}, err
	}
	spec, err := artifactMetadata(ctx, q, issue.PrimaryArtifactID)
	if err != nil {
		return changeRequestState{}, err
	}
	latest, err := settledVersionNumber(ctx, q, spec.ID)
	if err != nil {
		return changeRequestState{}, err
	}
	if version == 0 {
		version = latest
	}
	if version < 1 || version > latest {
		return changeRequestState{}, errorf(http.StatusNotFound, "VERSION_NOT_FOUND", "the spec of %s holds no version %d; its latest is %d", key, version, latest)
	}
	statements, err := specVersionText(ctx, q, spec, version)
	if errors.Is(err, pgx.ErrNoRows) {
		return changeRequestState{}, errorf(http.StatusNotFound, "VERSION_NOT_FOUND", "the spec of %s holds no version %d", key, version)
	}
	if err != nil {
		return changeRequestState{}, err
	}
	repo, held, err := issueSpecSource(ctx, q, issue)
	if err != nil {
		return changeRequestState{}, err
	}
	read := model.ChangeRequestRead{
		Issue: issue.Key, Version: version, SpecSource: held, Repo: repo,
		Proposals: []model.ChangeRequestNode{}, ChangedChecks: []model.ChangeRequestNode{}, Problems: []model.ChangeRequestProblem{},
		JudgmentReasons: []model.ChangeRequestJudgmentReason{},
	}
	verification, verified, err := s.verificationDocument(ctx, q, issue.Key, "")
	if err != nil {
		return changeRequestState{}, err
	}
	if verified {
		if read.VerificationVersion, err = settledVersionNumber(ctx, q, verification.ID); err != nil {
			return changeRequestState{}, err
		}
	}
	approved := func(string) bool { return false }
	if held {
		var checks *specchange.VersionText
		if read.VerificationVersion > 0 {
			text, err := specVersionText(ctx, q, verification, read.VerificationVersion)
			if err != nil {
				return changeRequestState{}, err
			}
			checks = &text
		}
		request, err := specchange.FromVersions(issue.Key, statements, checks)
		if err != nil {
			return changeRequestState{}, changeRequestUnreadable(err)
		}
		state, err := specstore.Read(ctx, q, issue.Project, repo)
		if err != nil {
			return changeRequestState{}, err
		}
		read.ApprovedSpecMark = state.Mark
		approvedSpec, err := state.Approved()
		if err != nil {
			return changeRequestState{}, err
		}
		if err := proposalsOf(&read, request, approvedSpec, state, verification.RefKey); err != nil {
			return changeRequestState{}, err
		}
		approved = func(id string) bool {
			if history := state.Nodes[id]; history != nil {
				current := history.Current()
				return current != nil && current.Kind == specchange.KindStatement
			}
			return false
		}
	}
	conclusion, err := specchange.ConclusionOf(statements)
	if err != nil {
		return changeRequestState{}, changeRequestUnreadable(err)
	}
	final, err := finalList(ctx, q, issue.Key, head)
	if err != nil {
		return changeRequestState{}, err
	}
	var finalOutcomes []specchange.Outcome
	if final != nil {
		finalOutcomes = specOutcomes(final.Outcomes)
		read.FinalList = &model.SpecFinalList{ID: final.ID, PullRequest: final.PullRequest, Head: final.Head, RecordedAt: final.RecordedAt}
	}
	judgment := specchange.Judge(conclusion, finalOutcomes, approved)
	read.Judgment = judgment.State
	for _, reason := range judgment.Reasons {
		read.JudgmentReasons = append(read.JudgmentReasons, model.ChangeRequestJudgmentReason{Code: reason.Code, Detail: reason.Detail, Outcome: reason.Outcome})
	}
	if conclusion != nil {
		read.Conclusion = conclusionRead(conclusion)
	}
	return changeRequestState{read: read, spec: spec, latest: latest}, nil
}

// proposalsOf fills read's proposals, changed checks and problems from request, compared with the
// approved spec state holds, in the order of the request's blocks, and whether a proposal or a
// changed check waits for a person (heldAsWritten). A check is read from the Verification
// document, whose address is verification, so a fault of a block there is a fault of the checks,
// and every other fault one of the statements, components and interfaces.
func proposalsOf(read *model.ChangeRequestRead, request specchange.Request, approved specgraph.Approved, state specstore.State, verification string) error {
	// The issue's own acceptance checks are no part of the change request (specchange.OfTheSpec,
	// the rule the approval's write judges by too), so they hold no gate.
	request = specchange.OfTheSpec(request, approved)
	changes, stale, problems, err := specchange.Classify(request, approved)
	if err != nil {
		return err
	}
	changed := make(map[string]specchange.Change, len(changes))
	for _, change := range changes {
		if _, first := changed[change.ID]; !first {
			changed[change.ID] = change
		}
	}
	staleOf := make(map[string]specchange.Stale, len(stale))
	for _, node := range stale {
		staleOf[node.ID] = node
	}
	current := func(id string) int {
		if history := state.Nodes[id]; history != nil {
			if version := history.Current(); version != nil {
				return version.Version
			}
		}
		return 0
	}
	add := func(kind string, node model.ChangeRequestNode) {
		if kind == specchange.KindCheck {
			read.ChangedChecks = append(read.ChangedChecks, node)
			return
		}
		read.Proposals = append(read.Proposals, node)
	}
	for _, proposal := range request.Proposals {
		if node, isStale := staleOf[proposal.ID]; isStale {
			delete(staleOf, proposal.ID)
			approvedAsWritten, err := heldAsWritten(approved, proposal, node)
			if err != nil {
				return err
			}
			add(proposal.Kind, model.ChangeRequestNode{ID: proposal.ID, Kind: proposal.Kind, What: "stale", Base: node.Base, Current: node.Current,
				Block: proposal.Block, Approved: approvedAsWritten})
			continue
		}
		change, held := changed[proposal.ID]
		if !held || !change.Writes() {
			continue
		}
		delete(changed, proposal.ID)
		add(change.Kind, model.ChangeRequestNode{ID: change.ID, Kind: change.Kind, What: change.What, Base: change.Base, Current: current(change.ID), Block: change.Block})
	}
	for _, problem := range append(slices.Clone(request.Problems), problems...) {
		read.Problems = append(read.Problems, model.ChangeRequestProblem{Block: problem.Pos.File, Rule: problem.Rule, Detail: problem.Detail})
		// A problem of form that keeps the node (a claim of two sentences) is listed, but proposes
		// nothing and changes no check: the approval write reads it so too (specstore's nodeLosses),
		// and a node it touches that does change is a proposal or a changed check of its own.
		if !problem.LosesNode() {
			continue
		}
		if strings.HasPrefix(problem.Pos.File, verification+"#") {
			read.ChangesChecks, read.ChangesChecksUnapproved = true, true
		} else {
			read.Proposes, read.ProposesUnapproved = true, true
		}
	}
	unapproved := func(node model.ChangeRequestNode) bool { return !node.Approved }
	read.Proposes = read.Proposes || len(read.Proposals) > 0
	read.ChangesChecks = read.ChangesChecks || len(read.ChangedChecks) > 0
	read.ProposesUnapproved = read.ProposesUnapproved || slices.ContainsFunc(read.Proposals, unapproved)
	read.ChangesChecksUnapproved = read.ChangesChecksUnapproved || slices.ContainsFunc(read.ChangedChecks, unapproved)
	return nil
}

// heldAsWritten says whether approved holds proposal, a stale node, as its block writes it, so no
// person's approval is owed for it: the approved spec removed the node and the block removes it
// too, or the approved spec's current version of the node is the block's node (specchange.Digest,
// which reads no base). A person's approval of the request writes it so and leaves the block's base
// behind. A node the approved spec holds otherwise — changed by another approval, or never held —
// waits for the person.
func heldAsWritten(approved specgraph.Approved, proposal specchange.Proposal, stale specchange.Stale) (bool, error) {
	if stale.RemovedNode {
		return proposal.Removes, nil
	}
	node, held := approved.Nodes[proposal.ID]
	if !held || proposal.Removes || len(node.Versions) == 0 {
		return false, nil
	}
	current, err := specchange.VersionNode(proposal.ID, node.Versions[len(node.Versions)-1])
	if err != nil {
		return false, err
	}
	return specchange.Digest(current) == specchange.Digest(proposal.Node), nil
}

// conclusionRead is conclusion as the read answers it, with no answer yet (conclusionAnswers).
func conclusionRead(conclusion *specchange.Conclusion) *model.ChangeRequestConclusion {
	read := &model.ChangeRequestConclusion{
		Block: conclusion.Block, Statement: conclusion.Statement, Difference: conclusion.Difference, NoStatement: conclusion.NoStatement,
		Evidence: append([]string{}, conclusion.Evidence...), Outcomes: []model.SpecOutcome{}, Answers: []model.ConclusionAnswer{},
	}
	for _, outcome := range conclusion.Outcomes {
		read.Outcomes = append(read.Outcomes, model.SpecOutcome{Text: outcome.Text, Statement: outcome.Statement, Restored: outcome.Restored})
	}
	return read
}

// conclusionAnswers is each answer to the conclusion block of the spec specID of the issue key,
// oldest first: each comment of the issue anchored inside that block. The comments are read as
// the issue's comment list reads them for every reader (loadOwnerComments), so a comment reserved
// for its author answers here as it answers there. A quote anchor names the innermost block that
// holds the quote, a paragraph of the conclusion for an answer that quotes its text, so an anchor
// answers the conclusion when the conclusion is on its block's path in the live document
// (docs.BlockPath). A block the live document no longer holds answers nothing; any other failure
// to read the document is the read's error, never an answer left out.
func (s *server) conclusionAnswers(ctx context.Context, q queryer, key, specID, block string) ([]model.ConclusionAnswer, error) {
	comments, err := s.loadOwnerComments(ctx, q, everyReader, issueOwner(key), specID)
	if err != nil {
		return nil, fmt.Errorf("read the answers to the conclusion of %s: %w", key, err)
	}
	answers := []model.ConclusionAnswer{}
	for _, comment := range comments {
		if comment.Anchor == nil || comment.Anchor.BlockID == nil || *comment.Anchor.BlockID == "" {
			continue
		}
		inside := *comment.Anchor.BlockID == block
		if !inside {
			path, err := s.deps.Docs.BlockPath(ctx, specID, *comment.Anchor.BlockID)
			if errors.Is(err, pmdoc.ErrTargetNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			inside = slices.ContainsFunc(path.Path, func(entry model.BlockPathEntry) bool { return entry.ID == block })
		}
		if inside {
			answers = append(answers, model.ConclusionAnswer{Comment: comment.ID, Author: comment.Author, Body: comment.Body, CreatedAt: comment.CreatedAt})
		}
	}
	return answers, nil
}

// GET /api/v1/issues/{key}/change-request/ending
//
// getChangeRequestEnding answers whether the change of the issue may land at ?head= (D10). The
// spec is read at its latest version, and its checks at the Verification document's latest
// settled version; its conclusion is judged against the final list recorded at head for the issue
// or an issue under it, else against none. Its reasons, in this order: no final list anywhere
// (no_final_list_for_head), or final lists of other heads alone (final_list_of_another_head); then
// no conclusion (no_conclusion), an incomplete one (incomplete), or an uncovered one with no
// standing approval of the latest spec version by a person (uncovered_without_approval); then a
// proposal the approved spec does not hold as written (proposes_without_approval), and a changed
// check it does not hold as written (changes_checks_without_approval, R25). The exemption that let
// the change start with no person counts for nothing here: an uncovered outcome waits for the
// person's approval of the whole spec (R26), and a proposal or a changed check waits until a
// person's approval writes it into the approved spec. It is ready exactly when no reason holds.
func (s *server) getChangeRequestEnding(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	head := r.URL.Query().Get("head")
	if err := headOf(head); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ending, err := s.changeRequestEnding(r.Context(), pool, r.PathValue("key"), head)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, ending)
}

// changeRequestEnding is the ending of the change of the issue key at head.
func (s *server) changeRequestEnding(ctx context.Context, q queryer, key, head string) (model.ChangeRequestEnding, error) {
	state, err := s.readChangeRequest(ctx, q, key, 0, head)
	if err != nil {
		return model.ChangeRequestEnding{}, err
	}
	read := state.read
	spec, err := s.loadArtifact(ctx, q, state.spec.ID)
	if err != nil {
		return model.ChangeRequestEnding{}, err
	}
	ending := model.ChangeRequestEnding{
		Issue: read.Issue, Head: head, Version: read.Version, Reasons: []model.ChangeRequestEndingReason{},
		Judgment: read.Judgment, JudgmentReasons: read.JudgmentReasons, FinalList: read.FinalList,
		Approved: spec.Approval != nil && spec.Approval.State == "approved" && spec.Approval.Version != nil && *spec.Approval.Version == state.latest,
	}
	wait := func(code, detail string, args ...any) {
		ending.Reasons = append(ending.Reasons, model.ChangeRequestEndingReason{Code: code, Detail: fmt.Sprintf(detail, args...)})
	}
	if read.FinalList == nil {
		other, err := finalList(ctx, q, key, "")
		if err != nil {
			return model.ChangeRequestEnding{}, err
		}
		if other == nil {
			wait(endingNoFinalListForHead, "No reviewer recorded a final list of outcomes for %s or an issue under it. The reviewer records its final list at head %s.", key, head)
		} else {
			wait(endingFinalListOfAnotherHead, "The final lists recorded for %s are of other heads; the newest is of %s. The reviewer records its final list at head %s.", key, other.Head, head)
		}
	}
	switch read.Judgment {
	case specchange.JudgmentNone:
		wait(endingNoConclusion, "Version %d of the spec of %s holds no conclusion block. Write the conclusion in the spec.", read.Version, key)
	case specchange.JudgmentIncomplete:
		wait(endingIncomplete, "The conclusion in version %d of the spec of %s breaks %d of its rules, which judgment_reasons names. Correct the conclusion in the spec.",
			read.Version, key, len(read.JudgmentReasons))
	case specchange.JudgmentUncovered:
		if !ending.Approved {
			wait(endingUncoveredWithoutApproval, "An outcome is behaviour that no approved statement covers, and no person approved version %d of the spec of %s. Request the person's approval of the whole spec.",
				read.Version, key)
		}
	}
	if read.ProposesUnapproved {
		wait(endingProposesWithoutApproval, "Version %d of the spec of %s proposes a statement, component or interface that the approved spec does not hold. "+
			"No person approved it. Request the person's approval of the whole spec.", read.Version, key)
	}
	if read.ChangesChecksUnapproved {
		wait(endingChangesChecksWithoutApproval, "Version %d of the Verification document of %s changes a check of the approved spec. "+
			"No person approved the change. Request the person's approval of the whole spec, with its Verification document.", read.VerificationVersion, key)
	}
	ending.State = "waiting"
	if len(ending.Reasons) == 0 {
		ending.State = "ready"
	}
	return ending, nil
}

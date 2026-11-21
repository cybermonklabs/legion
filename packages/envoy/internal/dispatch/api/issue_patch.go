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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// deleteIssueExternalLinks deletes every external link of the issue key, in tx: a PATCH that
// replaces the links writes each one it sends again. The statement that gives a pull request's
// issue reads the links as they stand at each read (delivery's attributedTerms), so a link this
// write gives or takes away reaches every reader of the pull requests it names with no mark.
func deleteIssueExternalLinks(ctx context.Context, tx pgx.Tx, key string) error {
	_, err := tx.Exec(ctx, `delete from issue_external_links where issue_key = $1`, key)
	return err
}

// linkedPullRequestLockSpace is the first key of the transaction locks a link write takes on each
// pull request it links (lockLinkedPullRequests), so they share no key with another advisory lock.
const linkedPullRequestLockSpace = "dispatch-issue-external-link"

// linkFormsSQL is every link, lowercased, that holds what link holds, link being an expression of
// one URL: where link names a pull request (delivery.LinkedPullRequestSQL, the link write's rule),
// each link of that pull request, in its /pull/ and its /issues/ form under each name whose
// canonical name is its repository's (delivery.PullRequestLinksSQL), which is what the statement
// reads as that pull request; and else link itself, lowercased, a URL in any letter case. The
// link check of a write and of a create compares other issues' links with it under lower(), which
// issue_external_links_url_lower serves with one probe a form.
func linkFormsSQL(link string) string {
	return `coalesce((select ` + delivery.PullRequestLinksSQL("lp.repo", "lp.number") + ` from ` +
		delivery.LinkedPullRequestSQL(link) + ` lp), array[lower(` + link + `)])`
}

// linkLockFormSQL is the one text every link that holds what link holds reads as (linkFormsSQL):
// for a link of a pull request, its /pull/ URL under the canonical name of its repository,
// lowercased, and for any other URL, the URL lowercased.
func linkLockFormSQL(link string) string {
	return `coalesce((select lower('https://github.com/' || ` + refs.CanonicalRepoSQL("lp.repo") + ` || '/pull/' || lp.number) from ` +
		delivery.LinkedPullRequestSQL(link) + ` lp), lower(` + link + `))`
}

// lockLinkedPullRequests takes, in tx, one transaction lock for each pull request the links name,
// under the one form all of its links read as (linkLockFormSQL), so /pull/N and /issues/N of one
// pull request, in any letter case and under any name of its repository, take one lock. The link
// check of a write reads committed rows, so without the lock two writes at once could each find
// the other's link absent and both store it on two issues. With it, the second write waits for
// the first to commit or roll back, and its check then reads the first's row and refuses with 409
// EXTERNAL_LINK_TAKEN. The forms are locked one at a time in byte order, so two writes that link
// the same pull requests take their locks in one order and cannot wait on each other. A URL that
// names no pull request takes a lock of its own text in lower case, which costs one lock and holds
// nothing else back. Two forms whose hashes meet only make one write wait for the other.
func lockLinkedPullRequests(ctx context.Context, tx pgx.Tx, links []model.ExternalLink) error {
	urls := make([]string, 0, len(links))
	for _, link := range links {
		urls = append(urls, link.URL)
	}
	rows, err := tx.Query(ctx, `
		select distinct `+linkLockFormSQL("u.url")+` collate "C" as form
		from unnest($1::text[]) u(url) order by form`, urls)
	if err != nil {
		return fmt.Errorf("read the pull requests the links name: %w", err)
	}
	forms, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the pull requests the links name: %w", err)
	}
	for _, form := range forms {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1), hashtext($2))`,
			linkedPullRequestLockSpace, form); err != nil {
			return fmt.Errorf("lock the links to %s: %w", form, err)
		}
	}
	return nil
}

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
}

func (s *server) patchIssue(w http.ResponseWriter, r *http.Request) {
	var input updateIssueInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	var blockers []string
	if input.BlockedBy != nil {
		var err error
		blockers, err = normalizeIssueBlockers(*input.BlockedBy)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var advances []string
	if input.Advances != nil {
		var err error
		advances, err = normalizeIssueAdvances(*input.Advances)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.Title != nil {
		*input.Title = strings.TrimSpace(*input.Title)
		if *input.Title == "" {
			writeError(w, "INVALID_ISSUE", http.StatusBadRequest, "title must not be blank")
			return
		}
		if length := itemblocks.Units16(*input.Title); length > contracts.IssueTitleMax {
			capExceeded(w, "title", length, contracts.IssueTitleMax)
			return
		}
	}
	status := ""
	if input.Status != nil {
		status = strings.TrimSpace(*input.Status)
		if status == "" {
			writeError(w, "INVALID_ISSUE", http.StatusBadRequest, "status must not be blank")
			return
		}
		if !model.IsIssueStatus(status) {
			writeError(w, "INVALID_STATUS", http.StatusBadRequest, "status is not in the Legion lifecycle")
			return
		}
	}
	if input.Route != nil && *input.Route != "" {
		if _, err := model.ParseRoute(*input.Route); err != nil {
			writeError(w, "ROUTE_INVALID", http.StatusBadRequest, "invalid route")
			return
		}
	}
	var labels, labelsAdd []string
	if input.Labels != nil && input.LabelsAdd != nil {
		writeError(w, "LABELS_INPUT", http.StatusBadRequest, "labels and labels_add cannot both be set")
		return
	}
	if input.Labels != nil {
		var err error
		labels, err = normalizeIssueLabels(*input.Labels)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.LabelsAdd != nil {
		var err error
		labelsAdd, err = normalizeIssueLabels(*input.LabelsAdd)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	priority, priorityProvided, err := parseIssuePriority(input.Priority)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	assignee, assigneeProvided, err := s.parseIssueAssignee(r.Context(), input.Assignee)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	parent, parentProvided, err := parseIssueParent(input.Parent)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	components, componentsProvided, err := parseIssueComponents(input.Components)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repo, repoProvided, err := parseIssueRepo(input.Repo)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	key := r.PathValue("key")
	// Whether the write closes the issue (closing) waits for the issue's owner lock; whether it
	// asks to is known now.
	closes := input.Status != nil && status == "done"
	if closes {
		// Before the transaction, which settlement's own would queue behind on the issue's owner
		// lock.
		s.settleIssueBeforeClose(outlivingTheClient(r), key)
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// A close withdraws the issue's open asks and a reopen restores them, writing their blocks
	// into the issue's documents, which reach their rooms through the ledger's commit. That
	// document work runs before the patch's first write, holding no transaction id (INFRA-636); its
	// writes, the asks' and every event this patch appends are queued on the ledger and run, in the
	// order made, after the patch's own row writes, which stay inline because their results steer
	// it (a component or blocker refusal, a taken external link).
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()

	dependencyWrite := input.BlockedBy != nil || parentProvided
	// A credential that names one project is refused a target outside it where a target's
	// project is read, since this route's own address names no target (project_scope.go): the
	// lock step refuses it before any lock, and writeBlockedBy again under them.
	callerProject, _ := s.callerProject(r)
	if dependencyWrite || input.Rank != nil {
		var project string
		if err := tx.QueryRow(r.Context(), `select project_key from issues where key = $1`, key).Scan(&project); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if dependencyWrite {
			if err := lockIssueDependencyWrite(r.Context(), tx, project, callerProject, blockers); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
		if input.Rank != nil {
			if err := lockProjectRankAllocation(r.Context(), tx, project); err != nil {
				s.writeHandlerError(w, err)
				return
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
	lock := lockPatchOwners
	if input.Status != nil || parentProvided {
		lock = lockPatchOwnersAndParent
	}
	if err := lock(r.Context(), tx, key, named...); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	before, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.ExpectedSeq != nil && *input.ExpectedSeq != int64(before.LastSeq) {
		writeError(w, "ISSUE_CHANGED", http.StatusConflict, "issue changed since the hand-over read")
		return
	}
	if parentProvided && parent != nil {
		if err := validateIssueParent(r.Context(), tx, key, before.Project, *parent); err != nil {
			s.writeHandlerError(w, err)
			return
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
			writeError(w, "ISSUE_CLOSED", http.StatusConflict, "issue is closed")
			return
		}
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
			s.writeHandlerError(w, err)
			return
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
		if err := tx.QueryRow(r.Context(), `select default_assignee from projects where key = $1`, before.Project).Scan(&projectDefault); err != nil {
			s.writeHandlerError(w, err)
			return
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
		issueRank, err := s.rankForInput(r.Context(), tx, before.Project, key, *input.Rank)
		if err != nil {
			s.writeHandlerError(w, err)
			return
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
		labels, err = mergeIssueLabels(before.Labels, labelsAdd)
		if err != nil {
			s.writeHandlerError(w, err)
			return
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
		s.writeHandlerError(w, err)
		return
	}
	// The advice is read here, before the patch's first write, so its queries hold no transaction
	// id (INFRA-636), and it is read for the issue as the patch leaves it: its status, its labels
	// and its parent (writeAdviceBefore), and the open asks the close withdraws or the reopen
	// restores, whose rows the patch writes afterwards (adviceAsks). Nothing else the patch writes
	// changes what it reads.
	afterRow := adviceIssueRow{parent: before.Parent, labels: before.Labels}
	afterStatus := before.Status
	if input.Status != nil {
		afterStatus = status
	}
	if parentProvided {
		afterRow.parent = parent
	}
	if input.Labels != nil || input.LabelsAdd != nil {
		afterRow.labels = labels
	}
	advice := s.writeAdviceBefore(r.Context(), tx, "PATCH /api/v1/issues/{key}", key, actor, afterStatus, afterRow, adviceAsks{
		excluded: askIDs(closingAsks),
		opened:   askIDs(reopenedAsks),
	})
	// The write phase: the statements from here on run back to back.
	if changed {
		sets = append(sets, "updated_at = "+issueUpdatedNow)
		if _, err := tx.Exec(r.Context(), `update issues set `+strings.Join(sets, ", ")+` where key = $1`, args...); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if previousClaim != nil {
		// Closing releases the claim through the one writer of those columns.
		if err := writeIssueClaim(r.Context(), tx, key, nil); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if componentsProvided {
		if err := writeIssueComponents(r.Context(), tx, key, before.Project, *components); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.BlockedBy != nil {
		if err := writeBlockedBy(r.Context(), tx, key, before.Project, callerProject, blockers); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.Advances != nil {
		if err := writeIssueAdvances(r.Context(), tx, key, heldAdvances, advances, actor, time.Now().UTC()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if parentProvided && parent != nil {
		if err := assertParentLeavesNoDependencyCycle(r.Context(), tx, key, *parent); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}

	if input.ExternalLinks != nil {
		if err := deleteIssueExternalLinks(r.Context(), tx, key); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		heldBefore := make([]string, 0, len(before.ExternalLinks))
		for _, link := range before.ExternalLinks {
			heldBefore = append(heldBefore, link.URL)
		}
		// The check below reads committed rows, so two writes at once, one of /pull/N and one of
		// /issues/N of one pull request on two issues, could each find nothing and both store
		// their link. Each write takes the lock of each pull request it links first, in one order,
		// so the second waits for the first to commit and its check then reads the first's row.
		if err := lockLinkedPullRequests(r.Context(), tx, *input.ExternalLinks); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		sent := make(map[string]bool, len(*input.ExternalLinks))
		for _, link := range *input.ExternalLinks {
			if err := validateExternalURL(link.URL); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			// A URL links exactly one issue (issue_external_links_url), so one write that sends
			// it twice cannot store it.
			if sent[link.URL] {
				writeError(w, "EXTERNAL_LINK_TAKEN", http.StatusConflict, fmt.Sprintf("%s is repeated in this write", link.URL))
				return
			}
			sent[link.URL] = true
			kind := link.Kind
			if kind == "" {
				kind = "url"
			}
			// A pull request's issue is read through every link that names it (delivery's
			// attributedTerms), so another issue that holds a link of that pull request holds it
			// here: its /pull/N or /issues/N form, in any letter case, under the name its
			// repository has today or one it was renamed away from (linkFormsSQL), each under
			// lower(), which issue_external_links_url_lower serves. Another URL is held by another
			// issue that holds it in any letter case. Two issues that held one pull request before
			// this check give it to the issue whose key comes first in byte order, as the
			// statement ends any tie. The check compares with other issues only, and never refuses
			// a link this issue held before the write, in any case, form or name: the client
			// sends the issue's links again with each write, and an issue of a pair two writes at
			// once stored keeps its own. The lesser key in byte order is named, so the answer turns
			// on no row order.
			var owner string
			forms := linkFormsSQL("$1::text")
			err := tx.QueryRow(r.Context(), `
				select l.issue_key from issue_external_links l
				where lower(l.url) = any(`+forms+`) and l.issue_key <> $2
				  and not exists (select 1 from unnest($3::text[]) b(url) where lower(b.url) = any(`+forms+`))
				order by l.issue_key collate "C" limit 1`, link.URL, key, heldBefore).Scan(&owner)
			if err == nil {
				writeError(w, "EXTERNAL_LINK_TAKEN", http.StatusConflict, fmt.Sprintf("%s is already linked from %s", link.URL, owner))
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				s.writeHandlerError(w, err)
				return
			}
			if _, err := tx.Exec(r.Context(), `insert into issue_external_links (issue_key, url, kind) values ($1, $2, $3)`, key, link.URL, kind); err != nil {
				if isUniqueViolation(err) {
					writeError(w, "EXTERNAL_LINK_TAKEN", http.StatusConflict, fmt.Sprintf("%s is already linked from another issue", link.URL))
					return
				}
				s.writeHandlerError(w, err)
				return
			}
		}
		// No event of a pull request carries a link to it. So each pull request that this write
		// links owes a read of GitHub, in any repository, where no head row of it is stored. The
		// next reconcile pass reads it and stores its head (R5 of the results lookup,
		// delivery.OweLinkedPullRequestReads).
		if err := delivery.OweLinkedPullRequestReads(r.Context(), tx, addedLinks(heldBefore, *input.ExternalLinks)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		changed = true
	}
	if !changed {
		// Nothing was written, so the row the advice read is the stored one.
		if err := ledger.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, withAdvice(before, advice))
		return
	}
	statusChanged := input.Status != nil && before.Status != status
	afterParent := before.Parent
	if parentProvided {
		afterParent = parent
	}
	parentChanged := parentProvided && !stringPointersEqual(before.Parent, afterParent)
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
		if err := s.deps.Events.LockOwners(r.Context(), tx, owners...); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// The asks' rows and events come before the status event, and their document writes, queued
	// above, before either. A reopen's are queued above already, with its follow's.
	var withdrawn []model.Ask
	appended := append([]*model.Event{}, restoredEvents...)
	if closing {
		if withdrawn, appended, err = s.recordWithdrawnAsksTx(documentCtx, tx, ledger, key, closingAsks, actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// The row as the patch's inline writes left it. Its last_seq is the sequence of the event
	// stored before this transaction's: every event the write phase appends on the issue is still
	// queued, the asks' and the ones their document writes queued beside them alike, so the
	// response's last_seq and the status event's payload are filled in from what those appends
	// took (appendedIssueLastSeq, model.IssueEventPayload.WithOwnerLastSeq) rather than counted
	// here.
	after, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
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
	edgeChanges, err := issueEdgeChanges(r.Context(), tx, before, after, slices.Contains(changedFields, "title"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	payload, err := newIssueEventPayload(r.Context(), tx, after, edgeChanges, changedFields)
	if err != nil {
		s.writeHandlerError(w, err)
		return
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
	event, err := s.queueLoggedEvent(ledger, issueOwner(key).event(eventType, actor, payload), logins, "", "")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	appended = append(appended, event)
	if previousClaim != nil {
		released, err := s.queueEvent(ledger, claimEvent("issue.released", key, actor, after, previousClaim, "closed"))
		if err != nil {
			s.writeHandlerError(w, err)
			return
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
			s.writeHandlerError(w, err)
			return
		}
		appended = append(appended, childEvent)
	}
	if parentChanged {
		if before.Parent != nil {
			removedEvent, err := s.queueEvent(ledger, issueOwner(*before.Parent).event(
				"child.removed", actor, map[string]any{"child_key": after.Key},
			))
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			appended = append(appended, removedEvent)
		}
		if after.Parent != nil {
			addedEvent, err := s.queueEvent(ledger, issueOwner(*after.Parent).event(
				"child.added", actor, map[string]any{"child_key": after.Key},
			))
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			appended = append(appended, addedEvent)
		}
	}
	if closing {
		// The close's last statement before its commit stamps closed_at again, at the moment it
		// runs (clock_timestamp()), over the start of the transaction (now()) the update above
		// wrote. It is queued last, so the write phase runs it after the asks' withdrawal, their
		// document writes and every event queued above. The document append gate takes a
		// browser's update the room observed no later than closeMargin after closed_at as one
		// made while the issue was open (docs.closeMargin), and the asks' withdrawal holds each
		// document it writes locked until this commit, a flush to every process and the waits on
		// other writers included: a keystroke that waited on that lock reaches the gate only
		// after the commit, and was made while the issue was still open however long the
		// transaction ran. The second write of closed_at fires migration 20261101181500's trigger
		// again, whose notification Postgres folds into the first (same channel, same payload,
		// one transaction). The events appended above carry the first stamp; the response carries
		// this one, the stored value, which the statement's own callback reads as the batch runs.
		if err := ledger.Queue(func(b *store.WriteBatch) error {
			b.Queue("", `
				update issues set closed_at = clock_timestamp() where key = $1 and closed_at is not null returning closed_at
			`, key).QueryRow(func(row pgx.Row) error { return row.Scan(&after.ClosedAt) })
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
	after.LastSeq = appendedIssueLastSeq(key, appended)
	// A change of closed_at reaches the document rooms of every process, this one's included, from
	// the commit itself: migration 20261101181500's trigger notifies, and each process closes its
	// rooms of the issue's documents so their browsers reconnect with the scope the issue now
	// allows.
	s.publish(r.Context(), appendedEvents(appended...)...)
	if closing {
		// The commit's notification closes the issue's rooms on every process; the sweep catches
		// the ask blocks written since the close forked each document (sweepClosedIssueAsks).
		// It runs after the publish so every event reaches subscribers in commit order.
		s.sweepClosedIssueAsks(outlivingTheClient(r), key, withdrawn, actor)
	}
	WriteJSON(w, http.StatusOK, withAdvice(after, advice))
}

// appendedIssueLastSeq is the issue's last_seq once the write phase has run: the sequence of the
// last event it appended on the issue's own owner row, which that insert took from the row and
// wrote back to it. It is read from the events rather than counted, because a count of what the
// route queued misses what the asks' document writes queued beside them - the anchor refreshes of
// a document a close or a reopen rewrote - which the same owner row numbers too (INFRA-636).
// appended holds the events as the write phase left them, so this runs after Commit.
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

// addedLinks is each URL of sent that held does not have in any letter case: the links that a
// write adds to an issue. The statement ties a link to a pull request without case
// (delivery's attributedTerms), so a URL held before in other letters adds no link.
func addedLinks(held []string, sent []model.ExternalLink) []string {
	var added []string
	for _, link := range sent {
		if !slices.ContainsFunc(held, func(url string) bool { return strings.EqualFold(url, link.URL) }) {
			added = append(added, link.URL)
		}
	}
	return added
}

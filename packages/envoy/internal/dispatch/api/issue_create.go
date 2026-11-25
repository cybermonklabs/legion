package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/rank"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// parseIssuePriority decodes the tri-state `priority` field of an issue write. Absent →
// (nil, false): leave it alone. JSON null → (nil, true): clear it. An integer 0..3 → its
// value. Anything else → 400 INVALID_PRIORITY.
func parseIssuePriority(raw json.RawMessage) (priority *int, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value < 0 || value > 3 {
		return nil, true, errorf(http.StatusBadRequest, "INVALID_PRIORITY", "priority must be an integer from 0 to 3 or null")
	}
	return &value, true, nil
}

// issueDuplicateError is the 409 POSSIBLE_DUPLICATE body POST /api/v1/issues answers an
// unforced create whose title overlaps an existing issue's.
type issueDuplicateError struct {
	Candidates []model.DuplicateCandidate `json:"candidates"`
	Code       string                     `json:"code"`
	Error      string                     `json:"error"`
}

// createIssueInput is the body of POST /api/v1/issues.
type createIssueInput struct {
	// Project is required of every create, an import of an external owner/repo#n included:
	// Dispatch finds no project from a repository. Title is not required of a caller: an import
	// takes the external ref as its title when the body names none, and only a create that names
	// neither a title nor an external ref is refused ("title is required").
	Project   string   `json:"project" required:"true"`
	Title     string   `json:"title"`
	Parent    *string  `json:"parent"`
	BlockedBy []string `json:"blocked_by"`
	// Advances is the priorities the new issue advances, a list of priority ids. A create
	// carries the whole list and no revision, and is never refused for it: the issue holds no
	// relation yet, so there is no read to be answering (refuseAdvancesRevision).
	Advances   []string        `json:"advances"`
	External   string          `json:"external"`
	Force      bool            `json:"force"`
	Spec       *string         `json:"spec"`
	Labels     []string        `json:"labels"`
	Priority   json.RawMessage `json:"priority"`
	Assignee   json.RawMessage `json:"assignee"`
	Components json.RawMessage `json:"components"`
	Repo       json.RawMessage `json:"repo"`
	Actor      *model.Actor    `json:"actor"`
}

func (s *server) createIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input createIssueInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	blockers, err := normalizeIssueBlockers(input.BlockedBy)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	advances, err := normalizeIssueAdvances(input.Advances)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	input.Project = strings.TrimSpace(input.Project)
	input.Title = strings.TrimSpace(input.Title)
	parentKey := ""
	if input.Parent != nil {
		parentKey = strings.TrimSpace(*input.Parent)
	}

	var externalRepo, externalNumber string
	if input.External != "" {
		// An import names its project in the body, as every other create does: Dispatch finds
		// no project from a repository. The refusal comes before the scope check and before the
		// ref is resolved, so a create that names no project reads nothing and writes nothing.
		if input.Project == "" {
			writeError(w, "INVALID_ISSUE", http.StatusBadRequest, "project is required when external is set")
			return
		}
		var err error
		externalRepo, externalNumber, err = externalRef(input.External)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// The one write whose project its address does not name, so withinProjectScope leaves it
	// here by its class (scopeProjectInBody). It runs once the body's project is read, and so
	// before the external ref is resolved to an issue already imported: a scoped caller naming
	// one imported into another project is refused rather than answered that project's issue,
	// which respondWithExistingExternalIssue would also record a related-check outcome on.
	// Nothing is written before this point.
	// callerProject is empty for a credential that names none, which is every caller that
	// exists today; the dependency lock step, the parent's read and writeBlockedBy below read it
	// again, since neither a blocked_by target's project nor the parent's is part of this address.
	callerProject, scoped := s.callerProject(r)
	if scoped && callerProject != input.Project {
		refuseWriteOutsideProjectScope(w, callerProject, input.Project)
		return
	}
	// A scoped caller is refused a parent or a blocked_by target outside its project here too,
	// with no lock held, before the near-copy gate and every other write before the transaction,
	// so a create it may not make records no near-copy check and gets no 409 in place of its 403.
	// The lock step and the reads under the locks refuse it again, the authoritative refusal.
	if scoped && (parentKey != "" || len(blockers) > 0) {
		if _, err := foreignDependencyTargets(r.Context(), s.deps.Store.Pool, input.Project, callerProject, blockers, parentKey); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.External != "" {
		existingKey, err := s.resolveIssueRef(r.Context(), input.External)
		if err == nil {
			s.answerExistingExternalIssue(w, r, input.External, existingKey, input.Project, actor)
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			s.writeHandlerError(w, err)
			return
		}
		if input.Title == "" {
			input.Title = input.External
		}
	}
	if !projectKeyPattern.MatchString(input.Project) {
		writeError(w, "INVALID_ISSUE", http.StatusBadRequest, projectKeyProblem("project", input.Project))
		return
	}
	if input.Title == "" {
		writeError(w, "INVALID_ISSUE", http.StatusBadRequest, "title is required")
		return
	}
	// Before the duplicate check, which reads the new title against every title in the project.
	if length := itemblocks.Units16(input.Title); length > contracts.IssueTitleMax {
		capExceeded(w, "title", length, contracts.IssueTitleMax)
		return
	}
	labels, err := normalizeIssueLabels(input.Labels)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	priority, _, err := parseIssuePriority(input.Priority)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A create names a person or nobody: no assignee key and a JSON null both leave assignee nil,
	// and defaultAssignee below fills it in, so a new issue always has somebody on it. Only the
	// PATCH clears an issue's assignee.
	assignee, _, err := s.parseIssueAssignee(r.Context(), input.Assignee)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	components, componentsProvided, err := parseIssueComponents(input.Components)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repo, _, err := parseIssueRepo(input.Repo, "INVALID_ISSUE")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repo = s.renamedRepo(repo)
	// The near-copy gate, run once for every create it covers whatever `force` says: a forced
	// create's record of what it went past must be the refusal an unforced create would have met,
	// and two reads would drift. The gate bounds and logs its own read (duplicateGate), and a read
	// that does not answer refuses nothing: the create goes in and is told that no near-copy check
	// was made, forced or not, a forced one also recording that it was forced with no issues named.
	//
	// A create naming a GitHub issue runs no gate at all: it imports an issue somebody else
	// already filed and refusing it would leave that issue unimported. It takes the zero
	// outcome, which records nothing anywhere, so the event cannot say a check was made that
	// never ran. Every record of the gate - the 409, the event's `forced`, `title_matches`,
	// `likely_duplicate` and `near_copy_check` - is settled here, in the one place that knows
	// whether the gate ran, and copied below rather than derived again from `force` and
	// `external` at each site.
	//
	// The gate's candidate set leaves out the new issue's whole ancestor chain, which is read
	// here, before the transaction. A parent that does not exist yields an empty chain and still
	// gets its own refusal inside the transaction, so no error changes order.
	var outcome nearCopyOutcome
	// nearCopyRecord is this create's near_copy_checks row, holding outcome.Check, the gate's
	// answer, at each write: at the 409, or once the create has committed (recordNearCopyCheck),
	// or in the create's transaction where recordInTx says so. A create the gate never ran for
	// (outcome.Check nil) writes nothing, and a create refused later for another reason writes
	// nothing either.
	var nearCopyRecord nearCopyCheckRow
	// recordInTx is set where the decision is made, for a near copy let in because its refusal's
	// row could not be confirmed (nearCopyOutcome.unrecorded): its row is written in the create's
	// transaction, under the id the refusal used, rather than after the commit.
	recordInTx := false
	if input.External == "" {
		ancestors := []string{}
		if parentKey != "" {
			chain, err := refs.IssueAncestors(r.Context(), s.deps.Store.Pool, parentKey)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			ancestors = append([]string{parentKey}, chain...)
		}
		spec := ""
		if input.Spec != nil {
			spec = *input.Spec
		}
		outcome = s.runNearCopyGate(r.Context(), input.Project, input.Title, spec, ancestors, input.Force)
		nearCopyRecord = newNearCopyCheckRow(s.newNearCopyRowID(), input.Project, actor, input.Title, spec, parentKey, outcome.Check)
		if len(outcome.Refuse) > 0 {
			if s.recordNearCopyRefusal(r.Context(), nearCopyRecord, outcome.Check) {
				WriteJSON(w, http.StatusConflict, issueDuplicateError{
					Candidates: outcome.Refuse,
					Code:       "POSSIBLE_DUPLICATE",
					Error:      fmt.Sprintf("possible duplicate of %s: %s", outcome.Refuse[0].Key, outcome.Refuse[0].Title),
				})
				return
			}
			// A refusal is answered only once its row is known to be written, so a count of the
			// gate's refusals from near_copy_checks misses none. Otherwise it is a check not made,
			// as a read past the gate's bound is: the create goes in, the filer is told what the
			// gate found, and the create's row is written in its transaction under the same id, so
			// it replaces the refused row if that insert landed unseen.
			outcome = outcome.unrecorded()
			recordInTx = true
		}
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// A create with blockers can close a wait cycle, and a create with a parent adds a link the
	// depth cap counts (refuseParentTooDeep): both take the dependency locks before the project's
	// owner lock, the number and the rank (lockIssueDependencyWrite). A new issue with neither
	// takes none. The blockers and the parent may each be an issue of another project, whose
	// dependency lock the write takes too.
	if len(blockers) > 0 || parentKey != "" {
		if err := lockIssueDependencyWrite(r.Context(), tx, input.Project, callerProject, blockers, parentKey); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var parentAssignee *string
	if parentKey != "" {
		var parentProject string
		err := tx.QueryRow(r.Context(), `select project_key, assignee from issues where key = $1`, parentKey).Scan(&parentProject, &parentAssignee)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "PARENT_INPUT", http.StatusBadRequest, fmt.Sprintf("parent issue %s not found", parentKey))
			return
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// A parent may be an issue of any project, but not for a credential that may write one
		// project alone: the authoritative refusal, under the locks, of what the lock step
		// refused before them.
		if callerProject != "" && parentProject != callerProject {
			s.writeHandlerError(w, refuseParentOutsideCallerProject(callerProject, parentKey, parentProject))
			return
		}
		// A parent of another project passes on no assignee, as it passes on no repository,
		// components or legion label: the chain's parent step (defaultAssignee) reads only a
		// parent of the new issue's own project, so such an issue falls to its own project's
		// default.
		if parentProject != input.Project {
			parentAssignee = nil
		}
		if err := refuseParentTooDeep(r.Context(), tx, "", parentKey); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// The project's owner lock comes before its row's update: every writer of a projects row
	// takes it first (an event append, the default assignee's and the default repository's PUTs,
	// the gated steps' publish), so none of them holds the row while it waits on the lock this
	// create holds.
	//
	// The create's read phase (INFRA-636): the spec's parse, render and encoding (SeedText), its
	// task count, its reference index and the advice all run here, before the update of the
	// project's next_number - the create's first write, which every later statement follows - so
	// none of that work holds the transaction's id. The issue's key is read now, in the statement
	// after the lock: next_number is written by an issue create alone, under the project's owner
	// lock this one holds, so the update below takes exactly this number. The spec's id is taken
	// in the same read, before its row is written (gen_random_uuid assigns no transaction id), so
	// the document's writes, which name it, are queued here with its version, its task count and
	// its index, and run in the write phase after the artifact's insert. An issue created without
	// a spec gets an empty primary document.
	var number int
	var artifactID string
	if err := store.LockOwnerAndRead(r.Context(), tx, store.ProjectOwner(input.Project), `
		select next_number, gen_random_uuid()::text from projects where key = $1
	`, []any{input.Project}, &number, &artifactID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	key := fmt.Sprintf("%s-%d", input.Project, number)
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var markdown string
	if input.Spec != nil && strings.TrimSpace(*input.Spec) != "" {
		markdown = *input.Spec
	}
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	// The issue's row and its spec's artifacts row are both written below, after the document
	// work, so what they will hold is recorded for the reads of this transaction that need it.
	// The address is the key the spec's insert names - the issue's key and the 'spec' slug -
	// the issue itself, whose own row this transaction writes in the same phase, which is why
	// the parent goes with it, and the spec's own dispatch:// address
	// (refs.DocumentAddress). A block pointer of the seeded spec is held to that address
	// (docs.addressOfDocument), and a spec citing the issue it opens, by key or at that
	// address, resolves to the two rows (refs.Pending). parentKey is "" for a root issue, the
	// null parent_key the insert below writes.
	ledger.RegisterPendingIssue(key)
	ledger.RegisterPendingArtifact(artifactID, refs.DocumentAddress{
		RefKey:        key + "/spec",
		IssueKey:      key,
		Ref:           refs.ArtifactRef(&key, input.Project, nil, "spec", "doc", true),
		PendingParent: parentKey,
	})
	markdown, err = s.deps.Docs.SeedText(documentCtx, artifactID, markdown, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	specVersion, err := ledger.QueueVersion(r.Context(), artifactID, markdown, []model.Actor{actor}, nil)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The spec is a stored text, so its display tree is stored once this transaction commits
	// (docs.Ledger.storeVersionTrees); this route queues the version row itself.
	ledger.WroteVersionText(artifactID, markdown)
	// documentReadBack is the rendering the seed stored, read back by its own parse (docs'
	// Ledger.ReadBackOf), which the task count and the advice's blocks read in place of parsing
	// the same markdown again (recordDocumentTaskProgress, documentBlocksFor).
	documentReadBack := ledger.ReadBackOf(artifactID, markdown)
	// The count is queued after the version's insert, so it names that version (LEGION-542).
	if err := recordDocumentTaskProgress(documentCtx, tx, artifactID, documentReadBack, markdown); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	specChanges, specBlocks, err := s.deps.Docs.IndexDocumentReferences(documentCtx, artifactID, specVersion.Number, markdown)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var parentRef *string
	if parentKey != "" {
		parentRef = &parentKey
	}
	// A new issue is in triage (the column's default), and its row - its labels and its parent -
	// is not written yet (writeAdviceBefore).
	advice := s.writeAdviceBefore(
		r.Context(), tx, "POST /api/v1/issues", key, actor, model.IssueStatusTriage,
		adviceIssueRow{project: input.Project, parent: parentRef, labels: labels}, adviceAsks{},
	)

	// The write phase: the statements from here on run back to back.
	var written int
	var projectDefaultAssignee string
	if err := tx.QueryRow(r.Context(), `
		update projects
		set next_number = next_number + 1
		where key = $1
		-- default_assignee as it stands: the column names a person and refuses nothing
		-- (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql), so the
		-- chain's last step always lands and this statement reads no other table for it.
		returning next_number - 1, default_assignee
	`, input.Project).Scan(&written, &projectDefaultAssignee); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if written != number {
		s.writeHandlerError(w, fmt.Errorf("project %s numbered issue %d under its owner lock, not the %d it read", input.Project, written, number))
		return
	}
	// The project's stored default is the chain's last step, and this update already holds the
	// project row: the decision moves below it so the create takes no round trip and no lock of
	// its own for it.
	if assignee == nil {
		assignee = defaultAssignee(actor, parentAssignee, projectDefaultAssignee)
	}
	if err := lockProjectRankAllocation(r.Context(), tx, input.Project); err != nil {
		s.writeHandlerError(w, err)
		return
	}

	lastRank, err := lastProjectRank(r.Context(), tx, input.Project, "")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issueRank := rank.Between(lastRank, "")
	// R24 with the rule on: a new issue is in triage, so only a done parent holds the rule
	// against it (createParentConflict). A create holds no owner lock of its parent, so the
	// parent's row is read `for share`, as the statement just before the insert. A close of the
	// parent whose update ran first makes this read wait for its commit and read done. A close
	// whose update comes after this read waits for this create to commit, and its second read
	// sees the new child (openChildrenOfAClose). With the rule off the create reads nothing here.
	if parentKey != "" && s.deps.ParentStatusRule.On() {
		var parentTitle, parentStatus string
		if err := tx.QueryRow(r.Context(), `
			select title, status from issues where key = $1 for share
		`, parentKey).Scan(&parentTitle, &parentStatus); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if conflict := createParentConflict(parentKey, parentTitle, parentStatus); conflict != nil {
			s.writeHandlerError(w, conflict)
			return
		}
	}

	if _, err := tx.Exec(r.Context(), `
		insert into issues (key, project_key, number, title, parent_key, created_by, labels, priority, rank, assignee, repo)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, key, input.Project, number, input.Title, parentRef, actorJSON, labels, priority, issueRank, assignee, repo); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := writeBlockedBy(r.Context(), tx, key, input.Project, callerProject, blockers); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The new issue holds no relation, so every id it names is stated and none is retracted.
	if len(advances) > 0 {
		if err := writeIssueAdvances(r.Context(), tx, key, nil, advances, actor, time.Now().UTC()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if componentsProvided {
		if err := writeIssueComponents(r.Context(), tx, key, input.Project, *components); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.External != "" {
		// The link is stored under the repository's new name where a rename pair renames it
		// away, as every write of a stored URL is (refs.Renames.CanonicalURL).
		url := s.deps.Renames.CanonicalURL(externalURL(externalRepo, externalNumber))
		// resolveIssueRef read the links before this transaction began, so a link write of the
		// other form of this number on another issue may have committed since, or be in flight.
		// The lock of the pull request (lockLinkedPullRequests) waits for such a write, and the
		// read under it sees its row, so the number is answered as the issue that holds it.
		if err := lockLinkedPullRequests(r.Context(), tx, s.deps.Renames, []model.ExternalLink{{URL: url}}); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// Another issue holds the link where it holds any link of the pull request or issue the
		// URL names, in any form, letter case or name of its repository (linkFormsSQL), as the
		// statement reads them and the issue PATCH's check compares them.
		var taken bool
		if err := tx.QueryRow(r.Context(), `
			select exists (select 1 from issue_external_links
				where lower(url) = any(`+linkFormsSQL(s.deps.Renames, "$1::text")+`))
		`, url).Scan(&taken); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if !taken {
			if _, err := tx.Exec(r.Context(), `
				insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github_issue')
			`, key, url); err != nil {
				if !isUniqueViolation(err) {
					s.writeHandlerError(w, err)
					return
				}
				taken = true
			}
		}
		if taken {
			if err := tx.Rollback(r.Context()); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			existingKey, err := s.resolveIssueRef(r.Context(), input.External)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			s.answerExistingExternalIssue(w, r, input.External, existingKey, input.Project, actor)
			return
		}
		// The link is GitHub's /issues/ form of the number, which GitHub also serves a pull
		// request at, so the number owes a read as a link by hand does (patchIssue): where it is
		// a pull request, the next reconcile pass stores its head and the link attributes it.
		if err := s.deps.Attributor.OweLinkedPullRequestReads(r.Context(), tx, []string{externalURL(externalRepo, externalNumber)}); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}

	if _, err := tx.Exec(r.Context(), `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, $2, $3, 'spec', 'spec.md', 'doc', true, $4)
	`, artifactID, key, input.Project, actorJSON); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issue, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issue.LastSeq++
	// A new issue's blocked_by edges move each blocker's count, beside what its spec cites.
	blockerChanges, err := issueEdgeChanges(r.Context(), tx, unsetIssue, issue, false)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A new issue changed every field it holds a value in, measured from an issue with none.
	payload, err := newIssueEventPayload(r.Context(), tx, issue, refs.MergeChanges(specChanges, blockerChanges), issueChangedFields(unsetIssue, issue))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	payload.Forced = outcome.Forced
	// The issue.created event and its stamp of the spec's new edges follow the spec's queued
	// writes: the stamp needs the index's edges written, and the document's pending-settlement row
	// comes before the event.
	event, err := s.queueStampedEvent(ledger, issueOwner(key).event("issue.created", actor, payload), "artifact", artifactID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// What the spec's block index stated waits for the issue's own event, queued right after it:
	// that event is the cause every node and tuple of the pass carries (graphstore.QueueCaused).
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		graphstore.QueueCaused(b, specBlocks, graphstore.LatestEvent)
		return nil
	}); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	nearCopyRecord.issue = key
	// A near copy let in because its refusal's row could not be confirmed (recordInTx) writes its
	// row in this transaction, under the id the refusal used: the create and the row that replaces
	// a refused row under that id commit together or not at all, so no committed create keeps a
	// refused row. It is one of the create's inline writes, after its first (the project's next
	// number, which already holds the projects row its foreign key shares) and so after the read
	// phase's owner lock, document work and advice; its result steers the route, since a row that
	// cannot land admits no create. It binds values worked out before the transaction opened
	// (newNearCopyCheckRow), so the spec's hash and the candidates' encoding run outside the
	// create's transaction id. Every other row is written after the commit.
	if recordInTx {
		if err := s.writeNearCopyCheck(r.Context(), tx, nearCopyRecord, outcome.Check); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A seeded spec never passed through a live edit, so nothing has queued the document
	// closer that indexes its ask blocks and repairs block ids; queue it now that the
	// transaction is durable.
	s.deps.Docs.ScheduleSettlement(artifactID)
	s.publish(r.Context(), *event)
	if !recordInTx {
		s.recordNearCopyCheck(r.Context(), nearCopyRecord, outcome.Check)
	}
	if advice != nil && input.Spec != nil && strings.TrimSpace(*input.Spec) != "" {
		advice.documentBlocks = documentBlocksFor(documentReadBack, markdown)
	}
	// The related-item check runs after the write committed, whatever writeAdvice
	// answered, and waits at most relatedAllowance; its result rides the advice, or advice of its
	// own when the write-advice read failed. An answered ask it offers carries the display tree
	// of that answer where this request asked for the trees, which its reader fills before the
	// response is written.
	trees := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	suggestions := s.relatedCheck(r.Context(), relatedInput{
		project: input.Project,
		text:    input.Title + "\n" + markdown,
		title:   input.Title,
		source:  suggestionSource{kind: "issue", issueKey: key, actor: actor},
		record:  true,
		trees:   trees,
	})
	trees.resolve()
	suggestions.TitleMatches = outcome.TitleMatches
	suggestions.LikelyDuplicate = outcome.LikelyDuplicate
	suggestions.NearCopyCheck = outcome.NearCopyCheck
	WriteJSON(w, http.StatusCreated, withSuggestions(issue, advice, suggestions))
}

// duplicateGateTimeout bounds the near-copy gate's whole read, on the context and as the read
// transaction's own statement_timeout. Every create waits on it before it is written: past it the
// create goes in and is told that no near-copy check was made, which is what a slow read must cost
// rather than a project nobody can write to.
//
// 500 ms came from the replay on an idle machine (TestNearCopyRefusalReplay: p50 135 ms, p95
// 228 ms, max 566 ms), which predicted it would cut 0.17% of creates. In production it cut 9 of
// INFRA's first 29 creates in near_copy_checks (2026-11-17, build 4055c4a5, reason bound). 2 s is an
// estimate, not a measurement: the rows written before read_ms and read_stage held no read time
// past the bound. If production's reads are a uniform multiple of the replay's (about three times,
// which puts the replay's 70th percentile at 500 ms), 2 s covers the replay's slowest read. How long
// each read takes, and the step a read the bound cuts was in, is read from near_copy_checks'
// read_ms and read_stage.
const duplicateGateTimeout = 2 * time.Second

// nearCopyOutcome is everything the near-copy gate decides for one create, settled once by
// runNearCopyGate and then only copied: the 409 the create is refused with, what the
// issue.created event records, what the response tells the filer, and the near_copy_checks row.
// The zero value is the create the gate never ran for - an external create - which records
// nothing and says nothing, so no reader of the event log is told of a check that was never made.
type nearCopyOutcome struct {
	// Refuse is what a 409 POSSIBLE_DUPLICATE names, and is empty on every create that goes in:
	// a create the gate refuses is never written.
	Refuse []model.DuplicateCandidate
	// Forced is what the issue.created event stores about a create that went in with `force`, in
	// the three states dispatch://INFRA-1160/spec#b-r3 names:
	//
	//	forced past a refusal       {"past": ["INFRA-724"]}
	//	the check did not answer    {"past": []}
	//	the refusal would not have stopped it, no force, or no check  nothing
	//
	// Past carries no omitempty, so the empty list survives the write and a reader can tell the
	// second state from the third. The weekly count reads it from the event.
	Forced *model.ForcedCreate
	// TitleMatches is advice.suggestions.title_matches: the issues a forced create went past,
	// which every released client prints as a near copy it forced past. An unforced create never
	// fills it, whatever the gate found - its nudge rides in LikelyDuplicate, a field of its own,
	// because Dispatch deploys before the CLI reaches agents and a nudge carried here would tell
	// every nudged filer it forced past something it did not.
	TitleMatches []model.DuplicateCandidate
	// LikelyDuplicate is advice.suggestions.likely_duplicate: the closest open issue of an
	// unforced create the gate nudged rather than refused.
	LikelyDuplicate *model.DuplicateCandidate
	// NearCopyCheck is advice.suggestions.near_copy_check: why the create went in unchecked,
	// empty when the gate answered.
	NearCopyCheck string
	// Check is the gate's answer as its near_copy_checks row records it (recordNearCopyCheck),
	// nil for a create the gate never ran for.
	Check *nearCopyCheck
}

// runNearCopyGate runs the gate for a create and settles every record of it at once. Only a
// create the gate covers reaches it: createIssue leaves the zero outcome for an external create,
// whose gate never runs.
//
// The four states are the gate's own: it did not answer, it found nothing to say, it found a
// refusal the create forced past, or it found one the create is refused with. Each sets what
// that state records and leaves the rest zero, so no site downstream reads `force` or `external`
// again and no record can disagree with another.
func (s *server) runNearCopyGate(
	ctx context.Context, project, title, spec string, ancestors []string, force bool,
) nearCopyOutcome {
	gate := s.duplicateGate(ctx, project, title, spec, ancestors)
	refusals := gate.refusals()
	check := &nearCopyCheck{force: force, candidates: gate.Candidates, readMS: gate.ReadMS, readStage: gate.ReadStage}
	switch {
	case !gate.answered():
		check.outcome, check.reason = nearCopyNotMade, gate.NotMade
		outcome := nearCopyOutcome{NearCopyCheck: s.nearCopyNotMadeNote(gate.NotMade), Check: check}
		if force {
			outcome.Forced = &model.ForcedCreate{Past: []string{}}
		}
		return outcome
	case len(refusals) == 0:
		check.outcome = nearCopyPassed
		if force {
			return nearCopyOutcome{Check: check}
		}
		nudge := gate.nudge()
		if nudge != nil {
			check.outcome = nearCopyNudged
		}
		return nearCopyOutcome{LikelyDuplicate: nudge, Check: check}
	case !force:
		check.outcome = nearCopyRefused
		return nearCopyOutcome{Refuse: refusals, Check: check}
	}
	check.outcome = nearCopyForced
	past := make([]string, 0, len(refusals))
	for _, candidate := range refusals {
		past = append(past, candidate.Key)
	}
	return nearCopyOutcome{Forced: &model.ForcedCreate{Past: past}, TitleMatches: refusals, Check: check}
}

// unrecorded is the outcome of a refusal whose near_copy_checks row could not be written: a
// check not made (reason unrecorded), which lets the create go in, as a read past the gate's
// bound does. Its row keeps the candidates the refusal read, and the note names the issue the
// refusal would have named, so the filer can still read it and fold the create into it.
func (outcome nearCopyOutcome) unrecorded() nearCopyOutcome {
	check := *outcome.Check
	check.outcome, check.reason = nearCopyNotMade, nearCopyNotMadeUnrecorded
	return nearCopyOutcome{NearCopyCheck: nearCopyCheckUnrecorded(outcome.Refuse[0]), Check: &check}
}

// answerExistingExternalIssue answers a create naming external, a GitHub issue Dispatch already
// holds as existingKey: with that issue where it is of project, and else with 400
// EXTERNAL_PROJECT_MISMATCH naming the reference, the issue that holds it and both projects,
// since the create asked for an issue of project. The create answers so wherever it finds the
// issue: before it writes, and after its write found the link taken by a write that committed in
// between, so a create racing a link write answers as a create after it does.
func (s *server) answerExistingExternalIssue(
	w http.ResponseWriter,
	r *http.Request,
	external string,
	existingKey string,
	project string,
	actor model.Actor,
) {
	var existingProject string
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		select project_key from issues where key = $1
	`, existingKey).Scan(&existingProject); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if existingProject != project {
		writeError(
			w,
			"EXTERNAL_PROJECT_MISMATCH",
			http.StatusBadRequest,
			fmt.Sprintf(
				"%s is already linked to %s of project %s, so it cannot be imported into project %s",
				external,
				existingKey,
				existingProject,
				project,
			),
		)
		return
	}
	s.respondWithExistingExternalIssue(w, r, existingKey, project, actor)
}

// respondWithExistingExternalIssue answers a create naming a GitHub issue Dispatch already holds
// with that issue. Nothing was created, so no related-item check is made, and the result says so
// (and the check is recorded as not made, reason already_exists).
func (s *server) respondWithExistingExternalIssue(
	w http.ResponseWriter,
	r *http.Request,
	existingKey string,
	project string,
	actor model.Actor,
) {
	adviceReadTx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer adviceReadTx.Rollback(r.Context())
	var existingStatus string
	// The owner lock, then the read in a statement of its own (store.LockOwnerAndRead).
	if err := store.LockOwnerAndRead(r.Context(), adviceReadTx, store.IssueOwner(existingKey), `
		select status from issues where key = $1
	`, []any{existingKey}, &existingStatus); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	existing, err := s.loadIssue(r.Context(), adviceReadTx, existingKey)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	advice := s.writeAdvice(
		r.Context(), adviceReadTx, "POST /api/v1/issues", existingKey, actor, adviceAsks{}, existingStatus, "", false,
	)
	if err := adviceReadTx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	suggestions := s.relatedCheckNotRun(r.Context(), relatedInput{
		project: project,
		source:  suggestionSource{kind: "issue", issueKey: existingKey, actor: actor},
		record:  true,
	}, relatedReasonAlreadyExists, "Dispatch already holds this issue; nothing was created")
	WriteJSON(w, http.StatusOK, withSuggestions(existing, advice, suggestions))
}

func isUniqueViolation(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505"
}

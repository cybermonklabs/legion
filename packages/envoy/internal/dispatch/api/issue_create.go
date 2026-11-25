package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
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
	Project string  `json:"project" required:"true"`
	Title   string  `json:"title"`
	Parent  *string `json:"parent"`
	// Needs is the issues the new issue waits on, and BlockedBy the same list under the spelling
	// a client of the release before sends; a body naming both names the same issues in each
	// (resolveIssueWaits).
	Needs     []string `json:"needs"`
	BlockedBy []string `json:"blocked_by"`
	// DuplicateOf is the key of an issue of the project the new issue repeats, and FiledFrom the
	// canonical reference of the public ask, comment or message of the project it was filed out
	// of, each a string or null (resolveDuplicateOf, resolveFiledFrom).
	DuplicateOf json.RawMessage `json:"duplicate_of"`
	FiledFrom   json.RawMessage `json:"filed_from"`
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
	waits, _, err := resolveIssueWaits(optionalList(input.Needs), optionalList(input.BlockedBy))
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
	// exists today; the dependency lock step, the parent's read and writeIssueNeeds below read it
	// again, since neither a wait's target's project nor the parent's is part of this address.
	callerProject, scoped := s.callerProject(r)
	if scoped && callerProject != input.Project {
		refuseWriteOutsideProjectScope(w, callerProject, input.Project)
		return
	}
	// A scoped caller is refused a parent or a wait's target outside its project here too, with
	// no lock held, before the near-copy gate and every other write before the transaction, so a
	// create it may not make records no near-copy check and gets no 409 in place of its 403. The
	// lock step and the reads under the locks refuse it again, the authoritative refusal.
	if scoped && (parentKey != "" || len(waits.targets) > 0) {
		if _, err := foreignDependencyTargets(r.Context(), s.deps.Store.Pool, input.Project, callerProject, waits, parentKey); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.External != "" {
		existingKey, err := s.resolveIssueRef(r.Context(), input.External)
		if err == nil {
			var existingProject string
			if err := s.deps.Store.Pool.QueryRow(r.Context(), `
				select project_key from issues where key = $1
			`, existingKey).Scan(&existingProject); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if existingProject != input.Project {
				writeError(
					w,
					"EXTERNAL_PROJECT_MISMATCH",
					http.StatusBadRequest,
					fmt.Sprintf(
						"%s is already linked to %s of project %s, so it cannot be imported into project %s",
						input.External,
						existingKey,
						existingProject,
						input.Project,
					),
				)
				return
			}
			s.respondWithExistingExternalIssue(w, r, existingKey, input.Project, actor)
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

	// Whether the credential is a person's, for the check of a create under a parent
	// (refuseSpecWriter), read before the transaction opens (specWriterOf). A create under no
	// parent is not checked and reads nothing for it.
	var writer specWriter
	if parentKey != "" {
		writer = s.specWriterOf(r, actor)
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// A create with waits can close a wait cycle, and a create with a parent adds a link the
	// depth cap counts (refuseParentTooDeep): both take the dependency locks before the project's
	// owner lock, the number and the rank (lockIssueDependencyWrite). A new issue with neither
	// takes none. The waits and the parent may each be an issue of another project, whose
	// dependency lock the write takes too.
	if len(waits.targets) > 0 || parentKey != "" {
		if err := lockIssueDependencyWrite(r.Context(), tx, input.Project, callerProject, waits, parentKey); err != nil {
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
		// The check of who writes the spec of an issue Legion runs, on the new row as the create
		// leaves it: its labels, and its parent's chain, whose architects pass. A parent of another
		// project ends that chain at the new row, so the check judges the new issue as a root of
		// its own project, which it does not judge. It runs before the project's owner lock and the
		// spec's document call, and before the create's first write.
		if err := refuseSpecWriter(r.Context(), tx, writer, specWrite{
			act: specWriteCreate, under: parentKey, labels: labels, project: input.Project,
		}); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// The new issue's duplicate_of and filed_from are read against the store before anything is
	// written, so a refused one leaves no issue. The new issue holds no key yet, so none names it.
	duplicateOf, _, err := resolveDuplicateOf(r.Context(), tx, "", input.Project, input.DuplicateOf)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	filedFrom, _, err := resolveFiledFrom(r.Context(), tx, input.Project, input.FiledFrom)
	if err != nil {
		s.writeHandlerError(w, err)
		return
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
	// The spec's row and its node are dated with one time read from the database's clock under
	// the project's owner lock this create holds (graphstore.StatementTime), as every create of a
	// document dates them. It is read here, in the read phase, so it holds no transaction id.
	specRef := graphstore.ArtifactRefKey(key, input.Project, "", "spec")
	createdAt, err := graphstore.StatementTime(r.Context(), tx, graphstore.KindArtifact, specRef)
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
	// An issue is born converted - its spec a fragment of the issue's room from its first write,
	// and a query node - only while every live process serves the issue room (decision 7 of
	// this branch's plan, R5): a process of the release before would admit the spec's
	// socket to the spec's own room. It is read here, in the read phase, and so is everything the
	// room's documents are planned from (planConvertedIssueRoom), before the spec's seed, which
	// then writes the spec's fragment of the room.
	bornConverted, err := fleetServesIssueRoom(r.Context(), tx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The issue's row and its spec's artifacts row are both written below, after the document
	// work, so what they will hold is recorded for the reads of this transaction that need it.
	// The address is the key the spec's insert names - the issue's key and the 'spec' slug -
	// the issue itself, whose own row this transaction writes in the same phase, which is why
	// the parent goes with it, and the spec's own dispatch:// address
	// (refs.DocumentAddress). A block pointer of the seeded spec is held to that address
	// (docs.addressOfDocument), and a spec citing the issue it opens, by key or at that
	// address, resolves to the two rows (refs.Pending). parentKey is "" for a root issue, the
	// null parent_key the insert below writes. The spec's node is a query node where the issue
	// is born converted.
	ledger.RegisterPendingIssue(key)
	specQuery := ""
	if bornConverted {
		specQuery = graphstore.QuerySpec
	}
	ledger.RegisterPendingArtifact(artifactID, refs.DocumentAddress{
		RefKey:        key + "/spec",
		IssueKey:      key,
		Ref:           refs.ArtifactRef(&key, input.Project, nil, "spec", "doc", true),
		PendingParent: parentKey,
		Project:       input.Project,
		Band:          refs.DocumentBand(&key, "doc", true, nil, "spec"),
	}, specQuery)
	var room convertedIssueRoom
	if bornConverted {
		if room, err = s.planConvertedIssueRoom(documentCtx, tx, ledger, key, input.Project, parentKey, artifactID, actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	markdown, err = s.deps.Docs.SeedText(documentCtx, artifactID, markdown, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// An issue born converted under a parent is a view on the spec of the nearest issue of its
	// parent's chain that holds requirements and is no view itself, while that issue is not done,
	// and a view takes no requirement of its own, as a write to one of its documents takes none
	// (docs' RefuseRequirementOnView): a requirement of its spec is refused with that issue's name.
	// The new issue's row is not written yet, so the chain is read from its parent
	// (docs.RefuseRequirementUnder). Nothing of the create is written: every write of it so far is
	// queued or in this transaction, which the refusal rolls back. A spec let through holding a
	// requirement block marks the new issue's spec its own on this create's ledger, so a create
	// whose block index the reconciliation takes up later keeps the mark too.
	spec := ledger.ReadBackOf(artifactID, markdown)
	if spec == nil {
		if spec, err = pmdoc.ParseRendering(markdown); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if err := docs.RefuseRequirementUnder(documentCtx, tx, key, parentKey, bornConverted, spec); err != nil {
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
	// The spec's first version is the projection of its version node, queued after the spec's
	// block nodes and caused by the issue's own event with them (docs.WriteVersionNode). The spec's
	// node is written below, a query node where the issue is born converted, so this version is
	// then a query version holding the query's definition version.
	specOwner := graphstore.VersionOwner{RefKey: specRef, Project: input.Project}
	if bornConverted {
		specOwner.Query, specOwner.DefinitionVersion = true, graphstore.QueryDefinitionVersion
	}
	specVersionNode, err := docs.WriteVersionNode(documentCtx, tx, specOwner, specVersion.Number, actor, specVersion.CreatedAt)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	specBlocks = graphstore.Joined(specBlocks, specVersionNode)
	// An issue born converted writes its decisions query's version 1 once its spec is read back:
	// the rendering of the decisions the spec holds (queueDecisionsFirstVersion).
	if bornConverted {
		if err := s.queueDecisionsFirstVersion(documentCtx, tx, ledger, &room, key, input.Project, actor, spec); err != nil {
			s.writeHandlerError(w, err)
			return
		}
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

	// The issue's node and its spec's node go in before the two rows, in one write that takes
	// them in the store's order, each in the issue's project, with the spec's part_of the issue
	// (R16 of this branch's spec); what that write states waits for the issue.created event
	// below (graphstore.QueueCaused). It is one of the create's inline writes, before the
	// ledger's: the spec's version and block index run at Commit, and the index's projection rows
	// name the spec's node. Each row names its node in the same transaction. Both are nodes this
	// write creates (graphstore.Created). The item filed_from names is stated before them where
	// the store holds no node of it (stateFiledFromNode). The node write and the issue's row go
	// to the server as one batch (store.WriteBatch), the node write first, as createStoredMessage
	// writes a message's node and row: the node write adds no round trip while the create holds
	// its transaction id, which the xid window tests count.
	if err := stateFiledFromNode(r.Context(), tx, filedFrom); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issueNode := graphstore.Created(graphstore.IssueNode(key, input.Project, actor, createdAt))
	specNode := graphstore.Created(graphstore.ArtifactNode(specRef, input.Project, "", actor, createdAt))
	specFacts := []graphstore.Tuple{graphstore.ArtifactPartOf(specRef, key, actor, createdAt)}
	if bornConverted {
		specNode = graphstore.Created(graphstore.QueryNode(specRef, input.Project, graphstore.QuerySpec, actor, createdAt))
		specFacts = graphstore.AttributeFacts(specFacts, specNode)
	}
	issueNodes := graphstore.PlanUncaused([]graphstore.Node{issueNode, specNode}, specFacts)
	var issueCreatedAt time.Time
	if err := store.RunWrite(r.Context(), tx, func(b *store.WriteBatch) error {
		graphstore.QueueNodes(b, issueNodes)
		b.Queue("", `
			insert into issues (key, project_key, number, title, parent_key, created_by, labels, priority, rank, assignee, repo, node_kind, node_address)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $1)
			returning created_at
		`, key, input.Project, number, input.Title, parentRef, actorJSON, labels, priority, issueRank, assignee, repo, graphstore.KindIssue).QueryRow(func(row pgx.Row) error {
			return row.Scan(&issueCreatedAt)
		})
		return nil
	}); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The relations the new issue states, each caused by issue.created below
	// (queueIssueRelations): its child_of, dated at its row's created_at, which the arm over the
	// rows that name no node dates the same edge by, its waits, whose rows are written here, its
	// duplicate_of and filed_from, and its affects to each component of its own, whose rows are
	// written below (writeIssueComponents).
	var relations issueRelationFacts
	if parentKey != "" {
		relations.claim(key, graphstore.TupleChildOf, graphstore.KindIssue, parentKey, issueCreatedAt)
	}
	if len(waits.targets) > 0 {
		// A new issue holds no wait yet, so none is read (heldIssueNeeds' zero value).
		needs, err := writeIssueNeeds(r.Context(), tx, key, input.Project, callerProject, waits, heldIssueNeeds{}, issueCreatedAt)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		relations.tuples = append(relations.tuples, needs...)
	}
	if duplicateOf != nil {
		relations.claim(key, graphstore.TupleDuplicateOf, graphstore.KindIssue, *duplicateOf, issueCreatedAt)
	}
	if filedFrom != nil {
		relations.claim(key, graphstore.TupleFiledFrom, filedFrom.kind, filedFrom.id, issueCreatedAt)
	}
	// The new issue holds no relation, so every id it names is stated and none is retracted.
	if len(advances) > 0 {
		if err := writeIssueAdvances(r.Context(), tx, key, nil, advances, actor, time.Now().UTC()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if componentsProvided {
		affects, err := writeIssueComponents(r.Context(), tx, key, input.Project, *components, issueCreatedAt)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		relations.tuples = append(relations.tuples, affects...)
	}
	if input.External != "" {
		// The link is stored under the repository's new name where a rename pair renames it
		// away, as every write of a stored URL is (refs.Renames.CanonicalURL).
		if _, err := tx.Exec(r.Context(), `
			insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github_issue')
		`, key, s.deps.Renames.CanonicalURL(externalURL(externalRepo, externalNumber))); err != nil {
			if !isUniqueViolation(err) {
				s.writeHandlerError(w, err)
				return
			}
			if err := tx.Rollback(r.Context()); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			existingKey, err := s.resolveIssueRef(r.Context(), input.External)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			s.respondWithExistingExternalIssue(w, r, existingKey, input.Project, actor)
			return
		}
	}

	if _, err := tx.Exec(r.Context(), `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by, created_at, node_kind, node_address)
		values ($1, $2, $3, 'spec', 'spec.md', 'doc', true, $4, $5, $6, $7)
	`, artifactID, key, input.Project, actorJSON, createdAt, graphstore.KindArtifact, specRef); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// An issue born converted takes, in its write phase, the rows its read phase planned: its tree,
	// whose Yjs document is the issue's room, the spec's document_rooms row naming it, and its
	// decisions query node (createConvertedIssueRoom), queued at the head of the write phase,
	// after the spec's row above, which they name, and before every statement the read phase
	// queued, so every queued statement that resolves the spec's room reads the room (T12b of this
	// branch's plan). They steer nothing this route answers, so they run in the write phase's batch
	// and not inline inside the transaction's id window.
	var roomNodes graphstore.Uncaused
	if bornConverted {
		if roomNodes, err = s.createConvertedIssueRoom(ledger, key, input.Project, artifactID, room, actor, createdAt); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	issue, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issue.LastSeq++
	// The issue's duplicate_of and filed_from are tuples its event causes, appended after that
	// event (queueIssueRelations), so the read above holds neither yet: the payload and the answer
	// carry what the write states.
	issue.DuplicateOf = duplicateOf
	if filedFrom != nil {
		issue.FiledFrom = &filedFrom.ref
	}
	// A new issue's waits move each target's count, beside what its spec cites.
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
	// What the spec's block index stated, and what the issue's and the spec's node write and the
	// room's documents stated, wait for the issue's own event, queued right after it: that event is
	// the cause every node and tuple of those writes carries (graphstore.QueueCaused), and of the
	// relations the issue states (queueIssueRelations). The room's pass states its references and
	// block mentions under the tree artifact, the room's own key (docs.IndexDocumentReferences), so
	// they take the issue's event there too.
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		if room.tree != "" {
			refs.QueueStamp(b, "artifact", room.tree)
		}
		graphstore.QueueCaused(b, graphstore.Joined(issueNodes, roomNodes), graphstore.LatestEvent)
		graphstore.QueueCaused(b, specBlocks, graphstore.LatestEvent)
		if err := queueIssueRelations(b, relations, actor); err != nil {
			return err
		}
		// The issue's first nodes are this write's, and every writer of its tree from here on
		// keeps its head rows, so they are whole from its birth and the issue is marked so.
		graphstore.QueueHeads(b, slices.Concat(graphstore.HeldHeads(graphstore.Joined(issueNodes, roomNodes)),
			graphstore.HeldHeads(specBlocks), relations.heads()))
		graphstore.QueueHeadMark(b, key)
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

// fleetServesIssueRoom reports whether every live server process (processes.Live, read through
// tx) lists the issue room in its capabilities: the test an issue is born converted by (decision 7
// of the issue-tree plan). With no live process it holds, as every fleet test of this
// server reads an empty fleet, so a process that has not registered yet - a test's, a tool's -
// serves no older room.
func fleetServesIssueRoom(ctx context.Context, tx pgx.Tx) (bool, error) {
	live, err := processes.Live(ctx, tx)
	if err != nil {
		return false, err
	}
	for _, process := range live {
		if !process.Lists(processes.CapabilityIssueRoom) {
			return false, nil
		}
	}
	return true, nil
}

// issueDecisionsName is the name an issue's decisions query node is created under, from which its
// slug is taken (nextArtifactSlug). It is no decisions document's: decisions_documents(), which
// the decisions-tree spec's decisions step selects by, takes a role of `decisions` or the slug
// `decisions-md`, and this row carries no role and never that slug.
const issueDecisionsName = "decisions"

// convertedIssueRoom is what an issue born converted holds beside its spec's row (T12b of
// this branch's plan), as the create's read phase planned it (planConvertedIssueRoom):
// its tree artifact, whose Yjs document is the issue's room, and its decisions query node, each by
// the id and the slug its row will take, and what the read phase's document work held for the
// issue's event - the two documents' first versions' nodes and the decisions query's index. The
// zero value is an issue that is not born converted.
type convertedIssueRoom struct {
	tree, treeSlug           string
	decisions, decisionsSlug string
	held                     graphstore.Uncaused
}

// planConvertedIssueRoom is the read phase of an issue born converted: it draws the ids of the
// tree artifact and the decisions query (gen_random_uuid, which assigns no transaction id), takes
// their slugs among the issue's documents (nextArtifactSlug), and records the two rows and the
// spec's document_rooms row, which the write phase writes (createConvertedIssueRoom), on the
// ledger: the tree and the query as pending artifacts, so the document work below and the
// spec's seed read their addresses, and the spec as edited in the tree's room
// (Ledger.RegisterPendingRoom), so the spec's seed writes the spec's fragment of the room; and it
// announces the spec's move into that room (docs.AnnounceDocumentRooms), a notification Postgres
// delivers at the commit, which assigns the transaction no id and so is sent here. Then it
// does the tree's work, queued on the ledger and run at the commit, after its row: its own
// fragment seeded empty, so an edit of the tree finds a document inside the Proof schema; and its
// version 1, empty, with its version node, as every empty document is created with, so a
// settlement of the room always finds the tree's latest version. The decisions query's version 1
// is written once the spec is seeded (queueDecisionsFirstVersion), since it stores the rendering
// of the decisions the spec holds. ctx is the context the caller joined to tx
// (docs.Service.Join), and the caller holds the project's owner lock.
func (s *server) planConvertedIssueRoom(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, key, project, parent, specID string, actor model.Actor) (convertedIssueRoom, error) {
	var room convertedIssueRoom
	if err := tx.QueryRow(ctx, `select gen_random_uuid()::text, gen_random_uuid()::text`).Scan(&room.tree, &room.decisions); err != nil {
		return convertedIssueRoom{}, err
	}
	var err error
	issue := artifactTarget{IssueKey: &key}
	if room.treeSlug, err = s.nextArtifactSlug(ctx, tx, issue, issueTreeName); err != nil {
		return convertedIssueRoom{}, fmt.Errorf("take the issue tree's slug: %w", err)
	}
	if room.decisionsSlug, err = s.nextArtifactSlug(ctx, tx, issue, issueDecisionsName); err != nil {
		return convertedIssueRoom{}, fmt.Errorf("take the decisions query's slug: %w", err)
	}
	pending := func(id, slug, kind string) refs.DocumentAddress {
		return refs.DocumentAddress{
			RefKey: graphstore.ArtifactRefKey(key, project, "", slug), IssueKey: key,
			Ref:           refs.ArtifactRef(&key, project, nil, slug, kind, false),
			PendingParent: parent, Project: project,
			Band: refs.DocumentBand(&key, kind, false, nil, slug),
		}
	}
	ledger.RegisterPendingArtifact(room.tree, pending(room.tree, room.treeSlug, "tree"), "")
	ledger.RegisterPendingArtifact(room.decisions, pending(room.decisions, room.decisionsSlug, "doc"), graphstore.QueryDecisions)
	ledger.RegisterPendingRoom(specID, room.tree)
	if err := docs.AnnounceDocumentRooms(ctx, tx, room.tree, specID); err != nil {
		return convertedIssueRoom{}, err
	}
	if _, err := s.deps.Docs.SeedText(ctx, room.tree, "", actor); err != nil {
		return convertedIssueRoom{}, fmt.Errorf("seed the issue tree's own fragment: %w", err)
	}
	authors := []model.Actor{actor}
	treeVersion, err := ledger.QueueVersion(ctx, room.tree, "", authors, nil)
	if err != nil {
		return convertedIssueRoom{}, fmt.Errorf("write the issue tree's first version: %w", err)
	}
	if err := recordDocumentTaskProgress(ctx, tx, room.tree, nil, ""); err != nil {
		return convertedIssueRoom{}, err
	}
	treeVersionNode, err := docs.WriteVersionNode(ctx, tx, graphstore.VersionOwner{
		RefKey: graphstore.ArtifactRefKey(key, project, "", room.treeSlug), Project: project,
	}, treeVersion.Number, actor, treeVersion.CreatedAt)
	if err != nil {
		return convertedIssueRoom{}, err
	}
	room.held = treeVersionNode
	return room, nil
}

// queueDecisionsFirstVersion queues version 1 of the decisions query node of an issue born
// converted, in the create's read phase, once its spec is seeded and read back (spec): the
// rendering of the decisions query's answer at the create (R15), which the spec's decisions and
// their readings are, each a node the spec's pass writes in the create's batch (docs.DecisionsOfSpec),
// with its version node, a query version holding the query's definition version; and that version
// indexed in the create's transaction (docs.API.IndexDocumentReferences), handed no text, so it
// indexes no block, no member and no reference of the version: each node the rendering prints is a
// node of the issue, indexed as one, and the tree holds no member of the decisions query node. The
// pass states the version, so its version_renders row goes in the create's batch with its cut, and
// its record's digest is filled after the COMMIT (docs' fillOwedDigests). What it states joins
// room's held facts, which the issue's event causes.
func (s *server) queueDecisionsFirstVersion(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, room *convertedIssueRoom, key, project string, actor model.Actor, spec *pmdoc.Node) error {
	rendered, err := docs.DecisionsOfSpec(spec)
	if err != nil {
		return fmt.Errorf("render the decisions query's first version: %w", err)
	}
	decisionsVersion, err := ledger.QueueVersion(ctx, room.decisions, rendered, []model.Actor{actor}, nil)
	if err != nil {
		return fmt.Errorf("write the decisions query's first version: %w", err)
	}
	decisionsVersionNode, err := docs.WriteVersionNode(ctx, tx, graphstore.VersionOwner{
		RefKey: graphstore.ArtifactRefKey(key, project, "", room.decisionsSlug), Project: project,
		Query: true, DefinitionVersion: graphstore.QueryDefinitionVersion,
	}, decisionsVersion.Number, actor, decisionsVersion.CreatedAt)
	if err != nil {
		return err
	}
	_, indexed, err := s.deps.Docs.IndexDocumentReferences(ctx, room.decisions, decisionsVersion.Number, "")
	if err != nil {
		return fmt.Errorf("index the decisions query's first version: %w", err)
	}
	room.held = graphstore.Joined(room.held, decisionsVersionNode, indexed)
	return nil
}

// createConvertedIssueRoom queues, at the head of ledger's write phase (docs.Ledger.QueueFirst),
// the rows room planned beside the spec's row specID, which the caller has written by then: the
// tree artifact's node and rows, and the issue's row of issue_views saying it was born converted
// (createIssueTree); the spec's document_rooms row naming the tree, ahead of every statement the
// read phase queued, which resolve the spec's room through it, and announced in the read phase
// (planConvertedIssueRoom); and the decisions query's node and row (createDecisionsQuery), each
// node before the row naming it. They steer nothing the route answers, so they run in the write
// phase's batch and not inline inside the transaction's id window. The caller holds the new
// issue's owner lock, which every writer of a document_rooms row holds first. It answers what the
// node writes and the read phase's document work state, which the caller causes under its event.
func (s *server) createConvertedIssueRoom(ledger *docs.Ledger, key, project, specID string, room convertedIssueRoom, actor model.Actor, at time.Time) (graphstore.Uncaused, error) {
	tree := issueTreeWrite(room.treeSlug, key, project, actor, at)
	decisions := decisionsQueryWrite(room.decisionsSlug, key, project, actor, at)
	if err := ledger.QueueFirst(func(b *store.WriteBatch) error {
		if err := s.createIssueTree(b, tree, room.tree, room.treeSlug, key, project, actor); err != nil {
			return err
		}
		b.Queue("join the spec to its issue's room", `insert into document_rooms (artifact_id, room) values ($1, $2)`, specID, room.tree)
		return s.createDecisionsQuery(b, decisions, room.decisions, room.decisionsSlug, key, project, actor)
	}); err != nil {
		return graphstore.Uncaused{}, err
	}
	return graphstore.Joined(tree, decisions, room.held), nil
}

// queueIssueRoomJoin is the room join createConvertedIssueRoom queues, for a document whose row's
// insert is queued on ledger, an upload's (queueNewArtifact): it records on the ledger that the
// document is edited in room (docs.Ledger.RegisterPendingRoom), so the document work that follows
// writes the document's
// fragment of the room, and queues the document_rooms row right after that insert, whose key it
// names, and before any write of the document. The announcement is a notification, which
// Postgres delivers at the commit, so it is sent at once and assigns the transaction no id.
func queueIssueRoomJoin(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, room, document string) error {
	ledger.RegisterPendingRoom(document, room)
	if err := queueExec(ledger, `insert into document_rooms (artifact_id, room) values ($1, $2)`, document, room); err != nil {
		return err
	}
	return docs.AnnounceDocumentRooms(ctx, tx, room, document)
}

// documentJoinsIssueRoom is whether a new document of a converted issue, whose slug is slug, is
// made a fragment of the issue's room: only one of the issue's three role documents - its spec,
// which createConvertedIssueRoom joins as the issue is created, and its Verification document and
// plan (roleSlugs), which an upload creates - from its first write (R5 of this branch's spec,
// version 112). Every other document keeps a room of its own, as every document of an
// unconverted issue does. The room's state is one update ygo decodes, whose bound the documents in
// it share (docs' refuseUndecodableSeed refuses a seed past it), so a document no role names would
// spend that bound for the three, and the heaviest documents the element limit admits filled it in
// about a dozen uploads. A role document's fixed short name always gives an address the store can
// hold (graphstore.AddressFits), so the address bound this test once read is never what keeps a
// document out: a block of the room names the node of the document whose fragment holds it, and a
// role document's node always fits.
func documentJoinsIssueRoom(slug string) bool {
	if slug == "spec" {
		return true
	}
	for _, named := range roleSlugs {
		if named == slug {
			return true
		}
	}
	return false
}

// decisionsQueryWrite is the node write of the decisions query of the issue key in project under
// slug, planned (graphstore.PlanUncaused) for createDecisionsQuery to queue: a query node of query
// `decisions` (graphstore.QueryNode), made by actor at at, with its part_of the issue.
func decisionsQueryWrite(slug, key, project string, actor model.Actor, at time.Time) graphstore.Uncaused {
	refKey := graphstore.ArtifactRefKey(key, project, "", slug)
	node := graphstore.Created(graphstore.QueryNode(refKey, project, graphstore.QueryDecisions, actor, at))
	return graphstore.PlanUncaused([]graphstore.Node{node},
		graphstore.AttributeFacts([]graphstore.Tuple{graphstore.ArtifactPartOf(refKey, key, actor, at)}, node))
}

// createDecisionsQuery queues on b the decisions query node id of the issue key in project, under
// slug: a doc row with no fragment and no document_rooms row (D6 of this branch's plan), whose
// node is written, as written plans it (decisionsQueryWrite), before the row that names it. Its
// version 1, empty, with its version node and its index, is the read phase's
// (planConvertedIssueRoom). The row carries no role: the read projects `decisions` from the node's
// query value. What the node write states waits for the caller's event, which the caller causes it
// under.
func (s *server) createDecisionsQuery(b *store.WriteBatch, written graphstore.Uncaused, id, slug, key, project string, actor model.Actor) error {
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		return err
	}
	graphstore.QueueNodes(b, written)
	b.Queue("create the decisions query", `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by, node_kind, node_address)
		values ($1, $2, $3, $4, $5, 'doc', false, $6, $7, $8)
	`, id, key, project, slug, issueDecisionsName, actorJSON, graphstore.KindArtifact, graphstore.ArtifactRefKey(key, project, "", slug))
	return nil
}

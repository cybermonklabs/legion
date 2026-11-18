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
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/rank"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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
	// Project and Title are not required of a caller: a create that names an external
	// owner/repo#n takes the project from that repository's mapping and the title from the
	// external issue (:92-127, :169-170), and only a create that names neither is refused.
	Project string  `json:"project"`
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
	usingDefaultProject := false
	if input.External != "" {
		var err error
		externalRepo, externalNumber, err = externalRef(input.External)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		project, err := s.repoProject(r.Context(), externalRepo)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if project == "" {
			project = s.deps.DefaultProject
			if project == "" {
				writeError(w, "PROJECT_UNMAPPED", http.StatusBadRequest,
					"repository is not mapped in repository settings and DISPATCH_DEFAULT_PROJECT is not configured")
				return
			}
			usingDefaultProject = true
		}
		if input.Project != "" && input.Project != project {
			writeError(
				w,
				"EXTERNAL_PROJECT_MISMATCH",
				http.StatusBadRequest,
				fmt.Sprintf(
					"external issue project %s must match repository settings project %s",
					input.Project,
					project,
				),
			)
			return
		}
		input.Project = project
	}
	// The one write whose project its address does not name, so withinProjectScope leaves it
	// here by its class (scopeProjectInBody). It runs the moment that project is settled —
	// read from the body above, or from the repository the external ref names — and so before
	// the ref is resolved to an issue already imported: a scoped caller naming one imported
	// into another project is refused rather than answered that project's issue, which
	// respondWithExistingExternalIssue would also record a related-check outcome on. Nothing
	// is written before this point.
	// callerProject is empty for a credential that names none, which is every caller that
	// exists today; the dependency lock step and writeIssueNeeds below read it again, since a
	// wait's target's project is no part of this address.
	callerProject, scoped := s.callerProject(r)
	if scoped && callerProject != input.Project {
		refuseWriteOutsideProjectScope(w, callerProject, input.Project)
		return
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
						"external issue project %s must match repository settings project %s",
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
	if usingDefaultProject {
		input.Labels = append(input.Labels, repoLabelPrefix+externalRepo)
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
	repo, _, err := parseIssueRepo(input.Repo)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
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
	// here, before the transaction. A parent that does not exist or sits in another project
	// yields an empty chain and still gets its own refusal inside the transaction, so no error
	// changes order.
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
		nearCopyRecord = nearCopyCheckRow{
			id: s.newNearCopyRowID(), project: input.Project, actor: actor,
			title: input.Title, spec: spec, parent: parentKey,
		}
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
	// A create with waits can close a wait cycle, and a create with a parent adds a link the
	// depth cap counts (refuseParentTooDeep): both take the dependency locks before the number
	// or the rank (lockIssueDependencyWrite). A new issue with neither takes none. Only the
	// waits can name another project, since a parent is of the issue's own project.
	if len(waits.targets) > 0 || parentKey != "" {
		if err := lockIssueDependencyWrite(r.Context(), tx, input.Project, callerProject, waits); err != nil {
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
		if parentProject != input.Project {
			writeError(w, "PARENT_INPUT", http.StatusBadRequest, "parent must be in the same project")
			return
		}
		if err := refuseParentTooDeep(r.Context(), tx, "", parentKey); err != nil {
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
	var number int
	var projectDefaultAssignee string
	if err := tx.QueryRow(r.Context(), `
		update projects
		set next_number = next_number + 1
		where key = $1
		-- default_assignee as it stands: the column names a person and refuses nothing
		-- (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql), so the
		-- chain's last step always lands and this statement reads no other table for it.
		returning next_number - 1, default_assignee
	`, input.Project).Scan(&number, &projectDefaultAssignee); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The project's stored default is the chain's last step, and this update already holds the
	// project row: the decision moves below it so the create takes no round trip and no lock of
	// its own for it.
	if assignee == nil {
		assignee = defaultAssignee(actor, parentAssignee, projectDefaultAssignee)
	}
	key := fmt.Sprintf("%s-%d", input.Project, number)
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var parent any
	if parentKey != "" {
		parent = parentKey
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

	// The issue's node and its spec's node go in before the two rows, in one write that takes
	// them in the store's order, each in the issue's project, with the spec's part_of the issue
	// (R16 of dispatch://INFRA-1253); what that write states waits for the issue.created event
	// below (graphstore.Caused). Each row names its node in the same transaction. Both are nodes
	// this write creates (graphstore.Created). The item filed_from names is stated before them
	// where the store holds no node of it (stateFiledFromNode).
	if err := stateFiledFromNode(r.Context(), tx, filedFrom); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	createdAt := time.Now().UTC()
	specRefKey := graphstore.ArtifactRefKey(key, input.Project, "", "spec")
	issueNode := graphstore.Created(graphstore.IssueNode(key, input.Project, actor, createdAt))
	specNode := graphstore.Created(graphstore.ArtifactNode(specRefKey, input.Project, "", actor, createdAt))
	issueNodes, err := graphstore.WriteUncaused(r.Context(), tx, []graphstore.Node{issueNode, specNode},
		[]graphstore.Tuple{graphstore.ArtifactPartOf(specRefKey, key, actor, createdAt)})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var issueCreatedAt time.Time
	if err := tx.QueryRow(r.Context(), `
		insert into issues (key, project_key, number, title, parent_key, created_by, labels, priority, rank, assignee, repo, node_kind, node_address)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $1)
		returning created_at
	`, key, input.Project, number, input.Title, parent, actorJSON, labels, priority, issueRank, assignee, repo, graphstore.KindIssue).Scan(&issueCreatedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The relations the new issue states, each caused by issue.created below
	// (stateIssueRelations): its child_of, dated at its row's created_at, which the arm over the
	// rows that name no node dates the same edge by, its waits, whose rows are written here, and
	// its duplicate_of and filed_from.
	var relations issueRelationFacts
	if parentKey != "" {
		relations.claim(key, graphstore.TupleChildOf, graphstore.KindIssue, parentKey, issueCreatedAt)
	}
	if len(waits.targets) > 0 {
		needs, err := writeIssueNeeds(r.Context(), tx, key, input.Project, callerProject, waits, issueCreatedAt)
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
		if err := writeIssueComponents(r.Context(), tx, key, input.Project, *components); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.External != "" {
		if _, err := tx.Exec(r.Context(), `
			insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github_issue')
		`, key, externalURL(externalRepo, externalNumber)); err != nil {
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

	var artifactID string
	if err := tx.QueryRow(r.Context(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, node_kind, node_address)
		values ($1, $2, 'spec', 'spec.md', 'doc', true, $3, $4, $5)
		returning id::text
	`, key, input.Project, actorJSON, graphstore.KindArtifact, specRefKey).Scan(&artifactID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// An issue created without a spec gets an empty primary document.
	var markdown string
	if input.Spec != nil && strings.TrimSpace(*input.Spec) != "" {
		markdown = *input.Spec
	}
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	markdown, err = s.deps.Docs.SeedText(documentCtx, artifactID, markdown, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	authors, err := encodeJSON([]model.Actor{actor})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The seed's own rendering is what this row stores, so the row records what it was rendered
	// from (docs.Ledger.RenderStampOf): a text read of a document still standing there serves the
	// stored markdown rather than rendering the document again.
	stamp := ledger.RenderStampOf(artifactID, markdown)
	var specCreated time.Time
	if err := tx.QueryRow(r.Context(), docs.RenderStampHead+`
		insert into artifact_versions (artifact_id, number, markdown, authors, doc_update_version, `+docs.RenderStampColumns+`)
		values ($1, 1, $2, $3, coalesce((select max(version) from doc_updates where artifact_id = $1), 0),
			`+docs.RenderStampValues+`, $4, $5)
		returning created_at
	`, artifactID, markdown, authors, stamp.Token, stamp.Renderer).Scan(&specCreated); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The spec is a stored text, so its display tree is stored once this transaction commits
	// (docs.Ledger.storeVersionTrees); this route writes the version row itself.
	ledger.WroteVersionText(artifactID, markdown)
	// documentReadBack is the rendering the seed stored, read back by its own parse (docs'
	// Ledger.ReadBackOf), which the task count and the advice's blocks read in place of parsing
	// the same markdown again (recordDocumentTaskProgress, documentBlocksFor).
	documentReadBack := ledger.ReadBackOf(artifactID, markdown)
	// The version row exists now, so the count names it (LEGION-542).
	err = recordDocumentTaskProgress(r.Context(), tx, artifactID, documentReadBack, markdown)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	specChanges, specBlocks, err := s.deps.Docs.IndexDocumentReferences(documentCtx, artifactID, 1, markdown)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The spec's first version is the projection of its version node, written after the spec's
	// block nodes and caused by the issue's own event with them (docs.WriteVersionNode). The spec's
	// node was written above as a document's.
	specVersion, err := docs.WriteVersionNode(r.Context(), tx, graphstore.VersionOwner{RefKey: specRefKey, Project: input.Project},
		1, actor, specCreated)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	specBlocks = graphstore.Joined(specBlocks, specVersion)
	issue, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issue.LastSeq++
	// The issue's duplicate_of and filed_from are tuples its event causes, appended after that
	// event (stateIssueRelations), so the read above holds neither yet: the payload and the answer
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
	event, err := s.appendEvent(r.Context(), tx, issueOwner(key).event("issue.created", actor, payload))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refs.Stamp(r.Context(), tx, "artifact", artifactID, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// What the spec's block index stated waits for the issue's own event: it is the cause every
	// node and tuple of that pass carries (graphstore.Caused).
	if err := graphstore.Caused(r.Context(), tx, specBlocks, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := graphstore.Caused(r.Context(), tx, issueNodes, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := stateIssueRelations(r.Context(), tx, relations, actor, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	advice := s.writeAdvice(
		r.Context(), tx, "POST /api/v1/issues", key, actor, "", issue.Status, false,
	)
	nearCopyRecord.issue = key
	// A near copy let in because its refusal's row could not be confirmed (recordInTx) writes its
	// row in this transaction, under the id the refusal used: the create and the row that replaces
	// a refused row under that id commit together or not at all, so no committed create keeps a
	// refused row. Every other row is written after the commit.
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
	s.publish(r.Context(), event)
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
// estimate, not a measurement: the rows hold no read time past the bound. If production's reads
// are a uniform multiple of the replay's (about three times, which puts the replay's 70th
// percentile at 500 ms), 2 s covers the replay's slowest read. How many reads 2 s still cuts is
// read from near_copy_checks after it is deployed.
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
	check := &nearCopyCheck{force: force, candidates: gate.Candidates}
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
	if err := adviceReadTx.QueryRow(r.Context(), `
		select status from issues where key = $1 for no key update
	`, existingKey).Scan(&existingStatus); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	existing, err := s.loadIssue(r.Context(), adviceReadTx, existingKey)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	advice := s.writeAdvice(
		r.Context(), adviceReadTx, "POST /api/v1/issues", existingKey, actor, "", existingStatus, false,
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

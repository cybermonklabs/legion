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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/contracts"
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
	// Project and Title are not required of a caller: a create that names an external
	// owner/repo#n takes the project from that repository's mapping (s.repoProject, or
	// DefaultProject when the repository is mapped nowhere) and, when the body names no title,
	// takes the ref itself for one; only a create that names neither is refused
	// (projectKeyPattern, "title is required").
	Project   string   `json:"project"`
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
	// exists today; the dependency lock step and writeBlockedBy below read it again, since a
	// blocked_by target's project is no part of this address.
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
	// nearCopy is this create's near_copy_checks row, holding outcome.Check, the gate's answer,
	// at each write: at the 409, or with the issue's filing (issueNearCopy), which a held issue's
	// post makes as much as this request. A create the gate never ran for (nil) writes nothing, and
	// a create refused later for another reason writes nothing either.
	var nearCopy *issueNearCopy
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
		nearCopy = &issueNearCopy{
			row:   newNearCopyCheckRow(s.newNearCopyRowID(), input.Project, actor, input.Title, spec, parentKey, outcome.Check),
			check: outcome.Check,
		}
		if len(outcome.Refuse) > 0 {
			if s.recordNearCopyRefusal(r.Context(), nearCopy.row, outcome.Check) {
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
			// gate found, and the create's row is written in the transaction that files the issue,
			// under the same id, so it replaces the refused row if that insert landed unseen.
			outcome = outcome.unrecorded()
			nearCopy.check, nearCopy.inTx = outcome.Check, true
		}
	}
	// The reviewer's hook, which reads the title, the spec and a components reason with the writing
	// check before anything is written or locked: either may call a model, and no transaction waits
	// on it. It comes after the near-copy gate, so a near copy the gate refuses is refused above,
	// held or not, and nothing is held; a held create's answer tells the filer what the gate found
	// (issueCreate.gate, heldAdvice).
	create := issueCreate{
		Project: input.Project, Title: input.Title, Parent: parentKey, Blockers: blockers, Advances: advances,
		Labels: labels, Priority: priority, Assignee: assignee, Components: components,
		ComponentsProvided: componentsProvided, Repo: repo, Spec: input.Spec, External: input.External,
		ExternalRepo: externalRepo, ExternalNumber: externalNumber, CallerProject: callerProject,
		Forced: outcome.Forced, NearCopy: nearCopy, gate: outcome,
	}
	after, proceed := s.reviewWrite(w, r, actor, input, create.reviewed())
	if !proceed {
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	written, err := s.writeIssue(r.Context(), tx, create, actor, 0, nil)
	if errors.Is(err, errExternalIssueImported) {
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
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.announceIssue(r.Context(), written)
	issue, advice, key := written.issue, written.advice, written.issue.Key
	s.reviewPosted(r.Context(), after, key)
	// The related-item check runs after the write committed, whatever writeAdvice
	// answered, and waits at most relatedAllowance; its result rides the advice, or advice of its
	// own when the write-advice read failed. An answered ask it offers carries the display tree
	// of that answer where this request asked for the trees, which its reader fills before the
	// response is written.
	trees := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	suggestions := s.relatedCheck(r.Context(), relatedInput{
		project: input.Project,
		text:    input.Title + "\n" + written.markdown,
		title:   input.Title,
		source:  suggestionSource{kind: "issue", issueKey: key, actor: actor},
		record:  true,
		trees:   trees,
	})
	trees.resolve()
	outcome.tell(suggestions)
	WriteJSON(w, http.StatusCreated, withSuggestions(issue, advice, suggestions))
}

// errExternalIssueImported is writeIssue finding the GitHub issue its create names imported
// already, by another create that committed first: createIssue answers with that issue.
var errExternalIssueImported = errors.New("the external issue is imported already")

// issueCreate is an issue a write files, decoded and checked (createIssue): what writeIssue
// writes, and what a held issue stores, so that its post files the issue the write itself would
// have filed. Forced is what the near-copy gate recorded of a forced create (nearCopyOutcome), and
// NearCopy the gate's answer the filing records (issueNearCopy).
type issueCreate struct {
	Project            string              `json:"project"`
	Title              string              `json:"title"`
	Parent             string              `json:"parent,omitempty"`
	Blockers           []string            `json:"blockers,omitempty"`
	Advances           []string            `json:"advances,omitempty"`
	Labels             []string            `json:"labels"`
	Priority           *int                `json:"priority,omitempty"`
	Assignee           *string             `json:"assignee,omitempty"`
	Components         *componentsInput    `json:"components,omitempty"`
	ComponentsProvided bool                `json:"components_provided,omitempty"`
	Repo               *string             `json:"repo,omitempty"`
	Spec               *string             `json:"spec,omitempty"`
	External           string              `json:"external,omitempty"`
	ExternalRepo       string              `json:"external_repo,omitempty"`
	ExternalNumber     string              `json:"external_number,omitempty"`
	CallerProject      string              `json:"caller_project,omitempty"`
	Forced             *model.ForcedCreate `json:"forced,omitempty"`
	NearCopy           *issueNearCopy      `json:"near_copy,omitempty"`
	// gate is the near-copy gate's whole answer to the request, which the request's own answer
	// tells the filer, held or not (nearCopyOutcome.tell). It is not stored: a held issue's post
	// answers nobody.
	gate nearCopyOutcome
}

// issueNearCopy is the near_copy_checks row of the near-copy gate's answer to a create: the
// create the gate answered (row) and the answer (check), which the issue's filing writes once it
// knows the issue's key. writeIssue writes it in the filing's transaction where inTx says so, and
// announceIssue after the commit otherwise. issueCreate carries it, so a held issue's post writes
// the row of the answer its create was given, under the id chosen for it, with the title and the
// spec the gate read, whatever an edit or a Rewrite changed since. inTx is a near copy let in
// because its refusal's row could not be confirmed (nearCopyOutcome.unrecorded): its row replaces
// a refused row under the same id, so it commits with the issue or not at all.
type issueNearCopy struct {
	row   nearCopyCheckRow
	check *nearCopyCheck
	inTx  bool
}

// issueNearCopyJSON is issueNearCopy as a held issue stores it in its request.
type issueNearCopyJSON struct {
	ID         string                         `json:"id"`
	Project    string                         `json:"project"`
	Actor      model.Actor                    `json:"actor"`
	Title      string                         `json:"title"`
	Spec       string                         `json:"spec"`
	Parent     string                         `json:"parent,omitempty"`
	Outcome    string                         `json:"outcome"`
	Reason     string                         `json:"reason,omitempty"`
	Force      bool                           `json:"force,omitempty"`
	Candidates []model.NearCopyCheckCandidate `json:"candidates"`
	InTx       bool                           `json:"in_tx,omitempty"`
}

func (n issueNearCopy) MarshalJSON() ([]byte, error) {
	candidates := make([]model.NearCopyCheckCandidate, 0, len(n.check.candidates))
	for _, candidate := range n.check.candidates {
		candidates = append(candidates, model.NearCopyCheckCandidate{Key: candidate.Key, Title: candidate.Title, Score: candidate.Score})
	}
	return json.Marshal(issueNearCopyJSON{
		ID: n.row.id, Project: n.row.project, Actor: n.row.actor, Title: n.row.title, Spec: n.row.spec,
		Parent: n.row.parent, Outcome: n.check.outcome, Reason: n.check.reason, Force: n.check.force,
		Candidates: candidates, InTx: n.inTx,
	})
}

func (n *issueNearCopy) UnmarshalJSON(data []byte) error {
	var stored issueNearCopyJSON
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	candidates := make([]scoredDuplicate, 0, len(stored.Candidates))
	for _, candidate := range stored.Candidates {
		candidates = append(candidates, scoredDuplicate{
			DuplicateCandidate: model.DuplicateCandidate{Key: candidate.Key, Title: candidate.Title}, Score: candidate.Score,
		})
	}
	check := &nearCopyCheck{outcome: stored.Outcome, reason: stored.Reason, force: stored.Force, candidates: candidates}
	*n = issueNearCopy{
		row:   newNearCopyCheckRow(stored.ID, stored.Project, stored.Actor, stored.Title, stored.Spec, stored.Parent, check),
		check: check,
		inTx:  stored.InTx,
	}
	return nil
}

// filed is n for the issue its create filed under key, nil for a create the gate never ran for.
func (n *issueNearCopy) filed(key string) *issueNearCopy {
	if n == nil {
		return nil
	}
	filed := *n
	filed.row.issue = key
	return &filed
}

// issueWritten is what writeIssue wrote: the issue, its issue.created event, the advice the
// write answers with (nil for a held issue's post, which answers nobody), its spec's document,
// the spec as the seed stored it, and the near-copy gate's row the filing writes after its commit
// (nil where it wrote it in its transaction or has none). A hold's dry run, which writes no issue,
// answers the advice, the spec and assignee, the person the issue is filed under.
type issueWritten struct {
	issue      model.Issue
	event      model.Event
	advice     *writeAdvice
	artifactID string
	markdown   string
	nearCopy   *issueNearCopy
	assignee   *string
}

// writeIssue files create as actor's issue in tx, the issue's rows, its spec's first version and
// its issue.created event, and commits tx. number is the issue's number: 0 takes the next of its
// project's sequence, which createIssue does, and a held issue's post passes the number its hold
// reserved (held, whose row the write phase takes first and marks posted in the same
// transaction). A hold's dry run answers errHeldDryRun, before the commit, with what the write
// would have answered: its advice, its spec as the seed stored it and its assignee.
func (s *server) writeIssue(ctx context.Context, tx pgx.Tx, create issueCreate, actor model.Actor, number int, held *heldPost) (issueWritten, error) {
	parentKey, assignee := create.Parent, create.Assignee
	// A create with blockers can close a wait cycle, and a create with a parent adds a link the
	// depth cap counts (refuseParentTooDeep): both take the dependency locks before the project's
	// owner lock, the number and the rank (lockIssueDependencyWrite). A new issue with neither
	// takes none. Only the blockers can name another project, since a parent is of the issue's own
	// project.
	if len(create.Blockers) > 0 || parentKey != "" {
		if err := lockIssueDependencyWrite(ctx, tx, create.Project, create.CallerProject, create.Blockers); err != nil {
			return issueWritten{}, err
		}
	}
	var parentAssignee *string
	if parentKey != "" {
		var parentProject string
		err := tx.QueryRow(ctx, `select project_key, assignee from issues where key = $1`, parentKey).Scan(&parentProject, &parentAssignee)
		if errors.Is(err, pgx.ErrNoRows) {
			return issueWritten{}, errorf(http.StatusBadRequest, "PARENT_INPUT", "parent issue %s not found", parentKey)
		}
		if err != nil {
			return issueWritten{}, err
		}
		if parentProject != create.Project {
			return issueWritten{}, errorf(http.StatusBadRequest, "PARENT_INPUT", "parent must be in the same project")
		}
		if err := refuseParentTooDeep(ctx, tx, "", parentKey); err != nil {
			return issueWritten{}, err
		}
	}
	// The project's owner lock comes before its row's update: every writer of a projects row
	// takes it first (an event append, a mapping write, the default assignee's PUT, the gated
	// steps' publish, a held issue's number reservation), so none of them holds the row while it
	// waits on the lock this create holds.
	//
	// The create's read phase (INFRA-636): the spec's parse, render and encoding (SeedText), its
	// task count, its reference index and the advice all run here, before the update of the
	// project's next_number - the create's first write, which every later statement follows - so
	// none of that work holds the transaction's id. The issue's key is read now, in the statement
	// after the lock: next_number is written under the project's owner lock alone, by an issue
	// create and a held issue's reservation (reserveIssueNumber), and this create holds that lock,
	// so the update below takes exactly this number. A held issue's post files the number its hold
	// reserved, writes no next_number, and reads the project's default assignee here instead,
	// under the same lock. The spec's id is taken in the same read, before its row is written
	// (gen_random_uuid assigns no transaction id), so the document's writes, which name it, are
	// queued here with its version, its task count and its index, and run in the write phase after
	// the artifact's insert. An issue created without a spec gets an empty primary document.
	reserved := number != 0
	var artifactID string
	var projectDefaultAssignee string
	if !reserved {
		if err := store.LockOwnerAndRead(ctx, tx, store.ProjectOwner(create.Project), `
			select next_number, gen_random_uuid()::text from projects where key = $1
		`, []any{create.Project}, &number, &artifactID); err != nil {
			return issueWritten{}, err
		}
	} else if err := store.LockOwnerAndRead(ctx, tx, store.ProjectOwner(create.Project), `
		-- default_assignee as it stands: the column names a person and refuses nothing
		-- (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql).
		select default_assignee, gen_random_uuid()::text from projects where key = $1
	`, []any{create.Project}, &projectDefaultAssignee, &artifactID); err != nil {
		return issueWritten{}, err
	}
	key := fmt.Sprintf("%s-%d", create.Project, number)
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		return issueWritten{}, err
	}
	hasSpec := create.Spec != nil && strings.TrimSpace(*create.Spec) != ""
	var markdown string
	if hasSpec {
		markdown = *create.Spec
	}
	documentCtx, ledger := s.deps.Docs.Join(ctx, tx)
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
		Ref:           refs.ArtifactRef(&key, create.Project, nil, "spec", "doc", true),
		PendingParent: parentKey,
	})
	markdown, err = s.deps.Docs.SeedText(documentCtx, artifactID, markdown, actor)
	if err != nil {
		return issueWritten{}, err
	}
	specVersion, err := ledger.QueueVersion(ctx, artifactID, markdown, []model.Actor{actor}, nil)
	if err != nil {
		return issueWritten{}, err
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
		return issueWritten{}, err
	}
	specChanges, err := s.deps.Docs.IndexDocumentReferences(documentCtx, artifactID, specVersion.Number, markdown)
	if err != nil {
		return issueWritten{}, err
	}
	var parentRef *string
	if parentKey != "" {
		parentRef = &parentKey
	}
	// A new issue is in triage (the column's default), and its row - its labels and its parent -
	// is not written yet (writeAdviceBefore). A held issue's post answers nobody.
	var advice *writeAdvice
	if held.answers() {
		advice = s.writeAdviceBefore(
			ctx, tx, "POST /api/v1/issues", key, actor, model.IssueStatusTriage,
			adviceIssueRow{parent: parentRef, labels: create.Labels}, adviceAsks{},
		)
		// The spec's blocks come from the rendering the seed read back above, so they are worked
		// out here, before the write phase's first statement takes the transaction's id, and never
		// between two of its statements (INFRA-636's window).
		if advice != nil && hasSpec {
			advice.documentBlocks = documentBlocksFor(documentReadBack, markdown)
		}
	}

	// The write phase: the statements from here on run back to back. A held issue's post takes
	// and marks its held row first (heldPost.queuePosted).
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		held.queuePosted(b)
		return nil
	}); err != nil {
		return issueWritten{}, err
	}
	if !reserved {
		var written int
		if err := tx.QueryRow(ctx, `
			update projects
			set next_number = next_number + 1
			where key = $1
			-- default_assignee as it stands: the column names a person and refuses nothing
			-- (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql), so the
			-- chain's last step always lands and this statement reads no other table for it.
			returning next_number - 1, default_assignee
		`, create.Project).Scan(&written, &projectDefaultAssignee); err != nil {
			return issueWritten{}, err
		}
		if written != number {
			return issueWritten{}, fmt.Errorf("project %s numbered issue %d under its owner lock, not the %d it read", create.Project, written, number)
		}
	}
	// The project's stored default is the chain's last step, read with the project row this
	// create already holds: the update above returns it, and a held issue's post, which writes no
	// next number, read it under the owner lock. So the decision takes no round trip and no lock
	// of its own.
	if assignee == nil {
		assignee = defaultAssignee(actor, parentAssignee, projectDefaultAssignee)
	}
	if err := lockProjectRankAllocation(ctx, tx, create.Project); err != nil {
		return issueWritten{}, err
	}

	lastRank, err := lastProjectRank(ctx, tx, create.Project, "")
	if err != nil {
		return issueWritten{}, err
	}
	issueRank := rank.Between(lastRank, "")

	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, created_by, labels, priority, rank, assignee, repo)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, key, create.Project, number, create.Title, parentRef, actorJSON, create.Labels, create.Priority, issueRank, assignee, create.Repo); err != nil {
		return issueWritten{}, err
	}
	if err := writeBlockedBy(ctx, tx, key, create.Project, create.CallerProject, create.Blockers); err != nil {
		return issueWritten{}, err
	}
	// The new issue holds no relation, so every id it names is stated and none is retracted.
	if len(create.Advances) > 0 {
		if err := writeIssueAdvances(ctx, tx, key, nil, create.Advances, actor, time.Now().UTC()); err != nil {
			return issueWritten{}, err
		}
	}
	if create.ComponentsProvided {
		if err := writeIssueComponents(ctx, tx, key, create.Project, *create.Components); err != nil {
			return issueWritten{}, err
		}
	}
	if create.External != "" {
		if _, err := tx.Exec(ctx, `
			insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github_issue')
		`, key, externalURL(create.ExternalRepo, create.ExternalNumber)); err != nil {
			if isUniqueViolation(err) {
				return issueWritten{}, errExternalIssueImported
			}
			return issueWritten{}, err
		}
	}

	if _, err := tx.Exec(ctx, `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, $2, $3, 'spec', 'spec.md', 'doc', true, $4)
	`, artifactID, key, create.Project, actorJSON); err != nil {
		return issueWritten{}, err
	}
	issue, err := s.loadIssue(ctx, tx, key)
	if err != nil {
		return issueWritten{}, err
	}
	issue.LastSeq++
	// A new issue's blocked_by edges move each blocker's count, beside what its spec cites.
	blockerChanges, err := issueEdgeChanges(ctx, tx, unsetIssue, issue, false)
	if err != nil {
		return issueWritten{}, err
	}
	// A new issue changed every field it holds a value in, measured from an issue with none.
	payload, err := newIssueEventPayload(ctx, tx, issue, refs.MergeChanges(specChanges, blockerChanges), issueChangedFields(unsetIssue, issue))
	if err != nil {
		return issueWritten{}, err
	}
	payload.Forced = create.Forced
	// The issue.created event and its stamp of the spec's new edges follow the spec's queued
	// writes: the stamp needs the index's edges written, and the document's pending-settlement row
	// comes before the event.
	event, err := s.queueStampedEvent(ledger, issueOwner(key).event("issue.created", actor, payload), "artifact", artifactID)
	if err != nil {
		return issueWritten{}, err
	}
	// A near copy let in because its refusal's row could not be confirmed (issueNearCopy.inTx)
	// writes its row in this transaction, under the id the refusal used: the issue and the row that
	// replaces a refused row under that id commit together or not at all, so no committed issue
	// keeps a refused row. It is one of the create's inline writes, after its first - the project's
	// next number, which already holds the projects row its foreign key shares, or, in a held
	// issue's post, which writes no next number, the mark of its held row - and so after the
	// read phase's owner lock, document work and advice; its result steers the route, since a row
	// that cannot land admits no create. It binds values worked out before the transaction opened
	// (newNearCopyCheckRow), so the spec's hash and the candidates' encoding run outside the
	// create's transaction id. Every other row is written after the commit (announceIssue).
	nearCopy := create.NearCopy.filed(key)
	if nearCopy != nil && nearCopy.inTx {
		if err := s.writeNearCopyCheck(ctx, tx, nearCopy.row, nearCopy.check); err != nil {
			return issueWritten{}, err
		}
		nearCopy = nil
	}
	if err := held.dryRunEndsOn(ctx, ledger); err != nil {
		return issueWritten{advice: advice, markdown: markdown, assignee: assignee}, err
	}
	if err := ledger.Commit(ctx); err != nil {
		return issueWritten{}, err
	}
	return issueWritten{
		issue: issue, event: *event, advice: advice, artifactID: artifactID, markdown: markdown,
		nearCopy: nearCopy,
	}, nil
}

// announceIssue is what follows an issue's commit, a held issue's post as much as the write's
// own: its spec's settlement is queued, its event published, and the near-copy gate's row not
// written in its transaction is written (recordNearCopyCheck).
func (s *server) announceIssue(ctx context.Context, written issueWritten) {
	// A seeded spec never passed through a live edit, so nothing has queued the document
	// closer that indexes its ask blocks and repairs block ids; queue it now that the
	// transaction is durable.
	s.deps.Docs.ScheduleSettlement(written.artifactID)
	s.publish(ctx, written.event)
	if near := written.nearCopy; near != nil {
		s.recordNearCopyCheck(ctx, near.row, near.check)
	}
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

// tell puts what the gate tells the filer on suggestions, the create's related-item check result:
// the issues a forced create went past, the likely duplicate, and why no check was made.
func (outcome nearCopyOutcome) tell(suggestions *model.Suggestions) {
	suggestions.TitleMatches = outcome.TitleMatches
	suggestions.LikelyDuplicate = outcome.LikelyDuplicate
	suggestions.NearCopyCheck = outcome.NearCopyCheck
}

// recheckNearCopy runs the near-copy gate once more for create, a held issue, at its post: against
// the project as it now stands, so an issue filed while it was held is read, and against its title
// and spec as they now stand, after an edit or a Rewrite. Nobody waits on a post to be refused, so
// a near copy found here refuses nothing: the issue posts as a create forced past it posts, its
// issue.created event recording the issues it went past (forced) and its near_copy_checks row this
// answer, under the id the hold chose. copies are the near copies the hold's own record did not
// already name, which its writer is told of (tellNearCopy). Where the gate finds none, or does not
// answer, create is left as the hold settled it.
func (s *server) recheckNearCopy(ctx context.Context, create issueCreate) (issueCreate, []model.DuplicateCandidate) {
	if create.NearCopy == nil || create.NearCopy.check == nil {
		return create, nil
	}
	ancestors := []string{}
	if create.Parent != "" {
		chain, err := refs.IssueAncestors(ctx, s.deps.Store.Pool, create.Parent)
		if err != nil {
			slog.Warn("dispatch: read a held issue's ancestors for its near-copy check at the post",
				"project", create.Project, "error", err)
			return create, nil
		}
		ancestors = append([]string{create.Parent}, chain...)
	}
	spec := ""
	if create.Spec != nil {
		spec = *create.Spec
	}
	// Run as a forced create's, which settles every record of a create that goes in past a refusal.
	outcome := s.runNearCopyGate(ctx, create.Project, create.Title, spec, ancestors, true)
	if len(outcome.TitleMatches) == 0 {
		return create, nil
	}
	var named []string
	if create.Forced != nil {
		named = create.Forced.Past
	}
	copies := slices.DeleteFunc(slices.Clone(outcome.TitleMatches), func(candidate model.DuplicateCandidate) bool {
		return slices.Contains(named, candidate.Key)
	})
	// The row keeps whether the create asked for force, which the run above does not.
	check := *outcome.Check
	check.force = create.NearCopy.check.force
	// The row is built again over the title and the spec as they now stand and the candidates
	// this run read (newNearCopyCheckRow), under the id the hold chose.
	recorded := *create.NearCopy
	row := recorded.row
	recorded.row = newNearCopyCheckRow(row.id, row.project, row.actor, create.Title, spec, row.parent, &check)
	recorded.check = &check
	create.NearCopy, create.Forced = &recorded, outcome.Forced
	return create, copies
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

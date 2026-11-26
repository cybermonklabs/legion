package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The record of every answer the near-copy gate gives a create. Before it only a create forced
// past a refusal left a trace, its `forced` record on the issue.created event (nearCopyOutcome.
// Forced): a refused create wrote nothing anywhere, and no create kept the scores the gate read,
// so how often the live gate refuses a create that is no copy, or lets a copy in, could not be
// counted on the creates it actually scored. A near_copy_checks row is one answer, read back
// through GET /api/v1/near-copy-checks and GET /api/v1/near-copy-checks/{id}.

// The outcomes near_copy_checks.outcome holds, one for each state runNearCopyGate settles.
const (
	nearCopyRefused = "refused"
	nearCopyForced  = "forced"
	nearCopyNudged  = "nudged"
	nearCopyPassed  = "passed"
	nearCopyNotMade = "not_made"
)

// The reasons near_copy_checks.reason gives for a check not made: why the gate's read did not
// answer (duplicateGateResult.NotMade), or that it found a near copy whose refusal could not be
// recorded (nearCopyOutcome.unrecorded).
const (
	nearCopyNotMadeBound      = "bound"
	nearCopyNotMadeFailed     = "failed"
	nearCopyNotMadeUnrecorded = "unrecorded"
)

// The steps of the gate's read (readDuplicateCandidates), as near_copy_checks.read_stage records
// the one a read was in when it ended (duplicateGateResult.ReadStage): taking a pool connection and
// opening the read transaction with its statement_timeout, picking the new text's rarest words,
// reading the lexemes the project's issue keys give, and scoring the project's open issues, which
// every answered read ends in.
const (
	nearCopyStageConnect = "connect"
	nearCopyStageTerms   = "terms"
	nearCopyStageKeys    = "keys"
	nearCopyStageScore   = "score"
)

// nearCopyRecordTimeout bounds the first attempt of each write recordNearCopyRefusal and
// recordNearCopyCheck make: a refusal's row, and the row of a create after its commit. Such a row
// is written on a context of its own, so a create whose client has gone still leaves its record,
// and the bound keeps a write that cannot land from holding the answer up longer than this. A
// create's row the bound ends is tried once more in the background (retryRecord); a refusal's is
// not, since the create it answers goes in as unrecorded instead. The row of a create let in as
// unrecorded is written in the create's transaction, on the request's context, and lands or fails
// with it.
const nearCopyRecordTimeout = time.Second

// nearCopyChecksPageMax is the most rows one page of GET /api/v1/near-copy-checks holds, and how
// many it holds when ?limit= is absent. A page row carries no spec (GET
// /api/v1/near-copy-checks/{id} reads one), so its size is bounded by the title's cap and five
// candidates, never by what a create sent.
const nearCopyChecksPageMax = 1000

// nearCopyCheck is the gate's answer for one create as its near_copy_checks row records it,
// settled by runNearCopyGate beside everything else the gate decides, so the record cannot
// disagree with the 409 or the issue.created event.
type nearCopyCheck struct {
	outcome string
	// reason is duplicateGateResult.NotMade for a check not made, and empty otherwise.
	reason     string
	force      bool
	candidates []scoredDuplicate
	// readMS and readStage are duplicateGateResult.ReadMS and ReadStage: how long the gate's read
	// took and the step it ended in. Every check runNearCopyGate settles holds both.
	readMS    *int
	readStage string
}

// nearCopyCheckRow is one near_copy_checks row to write: the create the gate answered, with the
// candidates the gate read, which no outcome after the gate changes (nearCopyOutcome.unrecorded
// keeps them). The rest of the gate's answer, nearCopyOutcome.Check, is passed beside it to each
// write, so it has one place.
type nearCopyCheckRow struct {
	// id is chosen before the row's first write (newNearCopyRowID), so the row a create let in past
	// an unconfirmed refusal writes in its transaction lands on that refusal's row if it exists.
	id      string
	project string
	actor   model.Actor
	title   string
	spec    string
	parent  string
	// issue is the key the create wrote, empty for a refused create.
	issue string
	// specSHA256 is spec's hex sha256 and candidates the JSON of the candidates the gate read,
	// worked out once when the row is built (newNearCopyCheckRow), before the create's transaction
	// opens, so the write in that transaction only binds them; encodeErr is why the candidates
	// could not be encoded, which each write answers instead of writing.
	specSHA256 string
	candidates []byte
	encodeErr  error
}

// newNearCopyCheckRow is the row of a create that sent title, spec and parent to project as actor,
// under id, whose gate answered check.
func newNearCopyCheckRow(id, project string, actor model.Actor, title, spec, parent string, check *nearCopyCheck) nearCopyCheckRow {
	candidates := make([]model.NearCopyCheckCandidate, 0, len(check.candidates))
	for _, candidate := range check.candidates {
		candidates = append(candidates, model.NearCopyCheckCandidate{
			Key: candidate.Key, Title: candidate.Title, Score: candidate.Score,
		})
	}
	encoded, err := encodeJSON(candidates)
	sum := sha256.Sum256([]byte(spec))
	return nearCopyCheckRow{
		id: id, project: project, actor: actor, title: title, spec: spec, parent: parent,
		specSHA256: hex.EncodeToString(sum[:]), candidates: encoded, encodeErr: err,
	}
}

// nearCopyCheckInsert writes one row under the id its create chose and answers that id when the
// row landed. A refused row already under that id is a refusal whose insert landed though the
// server never read its answer, so the create went in instead: the row of that create takes its
// place, in the create's own transaction, and one answer never leaves both a refused row and an
// issue. Only a refused row is ever replaced, and only by another outcome, so a write that finds
// any other row under its id changes nothing and answers no row.
const nearCopyCheckInsert = `
	insert into near_copy_checks
		(id, project_key, actor_kind, actor_id, outcome, reason, force, title, spec, spec_sha256,
		 parent_key, issue_key, candidates, refuse_score, nudge_score, build_commit, read_ms, read_stage)
	values ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
	on conflict (id) do update
	   set outcome = excluded.outcome, reason = excluded.reason, spec = excluded.spec,
	       issue_key = excluded.issue_key, candidates = excluded.candidates
	 where near_copy_checks.outcome = 'refused' and excluded.outcome <> 'refused'
	returning id::text
`

// newNearCopyRowID is the id of one create's near_copy_checks row, chosen before its first write.
func (s *server) newNearCopyRowID() string {
	if s.nearCopyRowID != nil {
		return s.nearCopyRowID()
	}
	return uuid.NewString()
}

// writeNearCopyCheck writes the row of check, the gate's answer, for the create row through q, the
// pool or the create's transaction, and errs unless the row is known to have landed. An insert
// that answers no row found another row under its id (nearCopyCheckInsert): it landed when that
// row is this very answer, an earlier attempt of this write that landed unseen (retryRecord says
// how one can), and any other answer under the id is an error.
func (s *server) writeNearCopyCheck(ctx context.Context, q queryer, row nearCopyCheckRow, check *nearCopyCheck) error {
	if row.encodeErr != nil {
		return row.encodeErr
	}
	// A refused create keeps its spec here, since no issue holds it; every other row's issue does.
	var spec any
	if check.outcome == nearCopyRefused {
		spec = row.spec
	}
	var written string
	err := q.QueryRow(ctx, nearCopyCheckInsert,
		row.id, row.project, row.actor.Kind, row.actor.ID, check.outcome, nullableText(check.reason),
		check.force, row.title, spec, row.specSHA256, nullableText(row.parent),
		nullableText(row.issue), row.candidates, duplicateRefuseScore, duplicateNudgeScore,
		nullableText(s.deps.BuildCommit), check.readMS, nullableText(check.readStage),
	).Scan(&written)
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var outcome string
	var reason, issue *string
	if err := q.QueryRow(ctx, `
		select outcome, reason, issue_key from near_copy_checks where id = $1::uuid
	`, row.id).Scan(&outcome, &reason, &issue); err != nil {
		return fmt.Errorf("near_copy_checks row %s took no write and could not be read back: %w", row.id, err)
	}
	if outcome != check.outcome || deref(reason) != check.reason || deref(issue) != row.issue {
		return fmt.Errorf("near_copy_checks row %s already holds another answer (%s)", row.id, outcome)
	}
	return nil
}

// recordNearCopyRefusal writes the row of a refusal, check, on a bounded context of its own, and
// reports whether it is known to have landed. createIssue answers a refusal only when it has, so
// every refusal has its row; otherwise it lets the create in as unrecorded and writes that
// create's row in its own transaction (writeNearCopyCheck), which replaces the refused row if the
// refusal's insert landed after all.
func (s *server) recordNearCopyRefusal(ctx context.Context, row nearCopyCheckRow, check *nearCopyCheck) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nearCopyRecordTimeout)
	defer cancel()
	if err := s.writeNearCopyCheck(ctx, s.deps.Store.Pool, row, check); err != nil {
		slog.Warn("dispatch: the near-copy refusal's record was not written, so the create goes in unrecorded",
			"project", row.project, "id", row.id, "error", err)
		return false
	}
	return true
}

// recordNearCopyCheck writes the row of a create that went in, after its commit, on a bounded
// context of its own; a create the gate never ran for (check nil) has no row. The create is
// written by then and its answer waits for this write, so a first attempt that cannot land in
// nearCopyRecordTimeout for a reason a retry can cure, such as a migration's lock on a table its
// foreign keys name, is tried once more in the background (retryRecord), and a row that still
// cannot be written is only logged.
func (s *server) recordNearCopyCheck(ctx context.Context, row nearCopyCheckRow, check *nearCopyCheck) {
	if check == nil {
		return
	}
	write := func(ctx context.Context) error { return s.writeNearCopyCheck(ctx, s.deps.Store.Pool, row, check) }
	first, cancel := context.WithTimeout(context.WithoutCancel(ctx), nearCopyRecordTimeout)
	err := write(first)
	cancel()
	if err == nil {
		return
	}
	s.retryRecord(err, write, "dispatch: the near-copy check's record was not written",
		"project", row.project, "outcome", check.outcome, "issue", row.issue, "id", row.id)
}

// nearCopyCheckColumns are a row's columns as scanNearCopyCheck reads them, but for its spec,
// which each query selects last: the row's own for GET /api/v1/near-copy-checks/{id}, and null for
// a page, which carries no spec.
const nearCopyCheckColumns = `id::text, created_at, project_key, actor_kind, actor_id, outcome, reason,
       force, title, spec_sha256, parent_key, issue_key, candidates, refuse_score, nudge_score,
       build_commit, read_ms, read_stage`

// nearCopyChecksPageQuery reads one page of a project's rows ($1), within [$2, $3) when given,
// after the row whose created_at and id are $4 and $5 when given, oldest first, at most $6, of a
// project the reader ($7) may see (access.ProjectClause): a project it may not see answers the
// empty page a project nobody holds does. A row's candidates are issues of its own project
// (duplicateGate reads `i.project_key = $1`), so the clause on the row holds them too.
var nearCopyChecksPageQuery = `
select ` + nearCopyCheckColumns + `, null::text
  from near_copy_checks
 where project_key = $1
   and ($2::timestamptz is null or created_at >= $2)
   and ($3::timestamptz is null or created_at < $3)
   and ($4::timestamptz is null or (created_at, id) > ($4, $5::uuid))
   and ` + access.ProjectClause("project_key", "$7") + `
 order by created_at, id
 limit $6`

// scanNearCopyCheck reads one row a query of nearCopyCheckColumns and a spec selects into check.
func scanNearCopyCheck(row pgx.Row, check *model.NearCopyCheck) error {
	return row.Scan(
		&check.ID, &check.CreatedAt, &check.Project, &check.ActorKind, &check.ActorID,
		&check.Outcome, &check.Reason, &check.Force, &check.Title, &check.SpecSHA256,
		&check.Parent, &check.Issue, &check.Candidates, &check.RefuseScore, &check.NudgeScore,
		&check.BuildCommit, &check.ReadMS, &check.ReadStage, &check.Spec,
	)
}

// getNearCopyChecks answers GET /api/v1/near-copy-checks: model.NearCopyChecksPage, the rows of
// ?project= recorded within [from, to), oldest first, at most ?limit= of them, after the row
// ?after= names. It reads one more row than the page holds to know whether another page follows.
func (s *server) getNearCopyChecks(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	page := model.NearCopyChecksPage{
		Project: strings.TrimSpace(query.Get("project")),
		Checks:  []model.NearCopyCheck{},
	}
	if !contracts.ValidProjectKey(page.Project) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("project", page.Project))
		return
	}
	from, to, err := parseOptionalWindow(query)
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	page.From, page.To = from, to
	limit, err := parseEventLimit(query.Get("limit"), nearCopyChecksPageMax)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var afterAt *time.Time
	var afterID *string
	if raw := strings.TrimSpace(query.Get("after")); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, fmt.Sprintf("after %q is not a near-copy check id", raw))
			return
		}
		id := parsed.String()
		var at time.Time
		err = pool.QueryRow(r.Context(), `
			select created_at from near_copy_checks where id = $1::uuid and project_key = $2 and `+access.ProjectClause("project_key", "$3")+`
		`, id, page.Project, refs.VisibilityArg(who)).Scan(&at)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest,
				fmt.Sprintf("after %s is no near-copy check of project %s", raw, page.Project))
			return
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		afterAt, afterID = &at, &id
	}
	rows, err := pool.Query(r.Context(), nearCopyChecksPageQuery,
		page.Project, page.From, page.To, afterAt, afterID, limit+1, refs.VisibilityArg(who))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var check model.NearCopyCheck
		if err := scanNearCopyCheck(rows, &check); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		page.Checks = append(page.Checks, check)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if len(page.Checks) > limit {
		page.Checks = page.Checks[:limit]
		next := page.Checks[limit-1].ID
		page.NextAfter = &next
	}
	WriteJSON(w, http.StatusOK, page)
}

// getNearCopyCheck answers GET /api/v1/near-copy-checks/{id}: the one row of that id, a refused
// row with the spec it refused, or 404 NEAR_COPY_CHECK_NOT_FOUND, which a row of a project the
// reader may not see answers too (access.ProjectClause).
func (s *server) getNearCopyCheck(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	if !requireUUIDPath(w, r, "near_copy_check") {
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	id := r.PathValue("id")
	var check model.NearCopyCheck
	err = scanNearCopyCheck(pool.QueryRow(r.Context(), `
		select `+nearCopyCheckColumns+`, spec from near_copy_checks where id = $1::uuid and `+access.ProjectClause("project_key", "$2")+`
	`, id, refs.VisibilityArg(who)), &check)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, "NEAR_COPY_CHECK_NOT_FOUND", http.StatusNotFound, "near-copy check "+id+" not found")
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, check)
}

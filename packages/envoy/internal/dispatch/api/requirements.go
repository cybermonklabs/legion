package api

import (
	"fmt"
	"math"
	"net/http"

	"github.com/jackc/pgx/v5"
)

const (
	requirementsDefaultLimit = 50
	requirementsMaxLimit     = 200
)

// requirementLine is one `requirement` block the index holds, as this route answers it: where it
// lives, the two attributes it was asked for, and enough of its words to recognise it without a
// second read. It is deliberately not the whole block: a caller listing every requirement of one
// status wants to choose among them, and reads the ones it chooses through the document.
type requirementLine struct {
	Issue      string `json:"issue"`
	ArtifactID string `json:"artifact_id"`
	BlockID    string `json:"block_id"`
	Ref        string `json:"ref"`
	Target     string `json:"target"`
	Status     string `json:"status"`
	Excerpt    string `json:"excerpt"`
}

// listRequirementsResponse is one page: the matches, and how many the filters match in all.
type listRequirementsResponse struct {
	Requirements []requirementLine `json:"requirements"`
	Total        int               `json:"total"`
	Limit        int               `json:"limit"`
	Offset       int               `json:"offset"`
}

// parseRequirementsPage reads the page this route always answers.
func parseRequirementsPage(r *http.Request) (limit, offset int, err error) {
	query := r.URL.Query()
	limit = requirementsDefaultLimit
	if value, present, valid := parseQueryInt(query, "limit", 1, requirementsMaxLimit); present {
		if !valid {
			return 0, 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "limit must be one integer from 1 to %d", requirementsMaxLimit)
		}
		limit = value
	}
	if value, present, valid := parseQueryInt(query, "offset", 0, math.MaxInt); present {
		if !valid {
			return 0, 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "offset must be one non-negative integer")
		}
		offset = value
	}
	return limit, offset, nil
}

// listRequirements answers GET /api/v1/requirements: the `requirement` blocks whose `target` or
// `status` attribute is the one asked for, newest change first.
//
// One of target or status is required, and a request carrying neither is refused rather than
// answered with every requirement in the corpus. The point of this route is that each filter is
// an index scan (20261110184749's two partial indexes); a filterless list would be the sequential
// read this route exists to replace, and it would be the slowest call in the deployment while
// looking like the cheapest.
func (s *server) listRequirements(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	target, status := query.Get("target"), query.Get("status")
	if target == "" && status == "" {
		s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY",
			"one of target or status is required: this route lists the requirements matching an attribute, and never every requirement"))
		return
	}
	limit, offset, err := parseRequirementsPage(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	// Both predicates are written so a missing filter is a true constant the planner drops, which
	// keeps one statement for the three shapes (target alone, status alone, both) and lets each
	// shape reach its own partial index.
	rows, queryErr := s.deps.Store.Pool.Query(r.Context(), `
		select a.issue_key,
		       b.artifact_id::text,
		       b.block_id,
		       b.node_id,
		       coalesce(b.requirement_target, ''),
		       coalesce(b.requirement_status, ''),
		       b.excerpt,
		       count(*) over () as total
		from document_blocks b
		join artifacts a on a.id = b.artifact_id
		where b.type = 'requirement'
		  and ($1 = '' or b.requirement_target = $1)
		  and ($2 = '' or b.requirement_status = $2)
		order by b.changed_at desc, a.issue_key, b.block_id
		limit $3 offset $4
	`, target, status, limit, offset)
	if queryErr != nil {
		s.writeHandlerError(w, fmt.Errorf("list the requirements of target %q and status %q: %w", target, status, queryErr))
		return
	}
	total := 0
	lines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (requirementLine, error) {
		var line requirementLine
		err := row.Scan(&line.Issue, &line.ArtifactID, &line.BlockID, &line.Ref,
			&line.Target, &line.Status, &line.Excerpt, &total)
		return line, err
	})
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list the requirements of target %q and status %q: %w", target, status, err))
		return
	}
	if lines == nil {
		lines = []requirementLine{}
	}
	WriteJSON(w, http.StatusOK, listRequirementsResponse{
		Requirements: lines, Total: total, Limit: limit, Offset: offset,
	})
}

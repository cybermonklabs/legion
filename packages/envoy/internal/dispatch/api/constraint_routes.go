package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// --- POST and GET /api/v1/issues/{key}/constraints ----------------------------------------------

// constraintInput is the body of a constraint under an issue: a standard with its reference, or a
// measurement with its value and where it was taken, and nothing of the other shape.
type constraintInput struct {
	Actor *model.Actor `json:"actor"`
	// Standard is the standard a requirement is held to, with its Reference.
	Standard  string `json:"standard"`
	Reference string `json:"reference"`
	// Measurement is what was measured, with its Value and where it was taken (TakenWhere).
	Measurement string `json:"measurement"`
	Value       string `json:"value"`
	TakenWhere  string `json:"taken_where"`
	// Visibility is who the constraint is for: project, the default, or author. A constraint
	// stands under its issue, which every reader of the project sees, so it takes project where
	// the body names none.
	Visibility string `json:"visibility" enum:"project,author"`
}

// constraintShape is the refusal of a constraint body that is neither of a constraint's shapes,
// empty for one that is: a standard with its reference, or a measurement with its value and where
// it was taken, with nothing of the other.
func (input constraintInput) constraintShape() string {
	standard := !blankText(input.Standard) || !blankText(input.Reference)
	measurement := !blankText(input.Measurement) || !blankText(input.Value) || !blankText(input.TakenWhere)
	const shapes = "a constraint is a standard with its reference (standard, reference), or a measurement with its value and where it was taken (measurement, value, taken_where)"
	switch {
	case standard && measurement:
		return shapes + ", and this one names fields of both"
	case standard && (blankText(input.Standard) || blankText(input.Reference)):
		return shapes + ", and this standard leaves out " + missingFields(map[string]string{
			"standard": input.Standard, "reference": input.Reference,
		})
	case measurement && (blankText(input.Measurement) || blankText(input.Value) || blankText(input.TakenWhere)):
		return shapes + ", and this measurement leaves out " + missingFields(map[string]string{
			"measurement": input.Measurement, "value": input.Value, "taken_where": input.TakenWhere,
		})
	case !standard && !measurement:
		return shapes + ", and this one names neither"
	}
	return ""
}

// missingFields names the fields of one shape a body leaves blank, in the order the shape names
// them.
func missingFields(fields map[string]string) string {
	var missing []string
	for _, name := range []string{"standard", "reference", "measurement", "value", "taken_where"} {
		if value, named := fields[name]; named && blankText(value) {
			missing = append(missing, name)
		}
	}
	return strings.Join(missing, " and ")
}

// constraintRead is one constraint of an issue: its address, `dispatch://constraint/<uuid>`, the
// fields of its shape (each null where it holds none), the person or session its authored_by
// tuple names (the session its body names for one written on the shared bearer, and none where
// the write named only a workload), when it was written, and its state and visibility.
type constraintRead struct {
	Address     string           `json:"address"`
	ID          string           `json:"id"`
	Standard    *string          `json:"standard"`
	Reference   *string          `json:"reference"`
	Measurement *string          `json:"measurement"`
	Value       *string          `json:"value"`
	TakenWhere  *string          `json:"taken_where"`
	Author      *reasoningAuthor `json:"author"`
	CreatedAt   time.Time        `json:"created_at"`
	State       string           `json:"state" enum:"live,gone,proposed,settled,overtaken"`
	Visibility  string           `json:"visibility" enum:"project,author"`
}

// issueConstraints is the constraints under one issue, in the order they were written there.
type issueConstraints struct {
	Issue       string           `json:"issue"`
	Constraints []constraintRead `json:"constraints"`
}

// constraintAttributes are a constraint's fields as its row carries them: each one the body
// names, by its attribute's name.
func (input constraintInput) constraintAttributes() map[string]any {
	attributes := map[string]any{}
	for name, value := range map[string]string{
		graphstore.AttributeStandard: input.Standard, graphstore.AttributeReference: input.Reference,
		graphstore.AttributeMeasurement: input.Measurement, graphstore.AttributeValue: input.Value,
		graphstore.AttributeTakenWhere: input.TakenWhere,
	} {
		if !blankText(value) {
			attributes[name] = value
		}
	}
	return attributes
}

// namedOrNull is text where it holds something, and null where it is blank.
func namedOrNull(text string) *string {
	if blankText(text) {
		return nil
	}
	return &text
}

// writeConstraint is POST /api/v1/issues/{key}/constraints: one constraint under the issue,
// placed there by its part_of tuple to the issue's node, with the fields of its shape as
// attributes of its row. A body that is neither shape is 400 CONSTRAINT_INPUT and stores nothing.
// A requirement points at it through POST .../nodes/{id}/pointers (constrained_by).
func (s *server) writeConstraint(w http.ResponseWriter, r *http.Request) {
	var input constraintInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	caller, ok := s.reasoningCallerOf(w, r, input.Actor)
	if !ok {
		return
	}
	if refusal := input.constraintShape(); refusal != "" {
		writeError(w, codeConstraintInput, http.StatusBadRequest, refusal)
		return
	}
	if _, err := graphstore.VisibilityFor(input.Visibility, caller.proving); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ctx, key := r.Context(), r.PathValue("key")
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	at, err := transactionNow(ctx, tx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	id := uuid.NewString()
	written, err := writeReasoningNodes(ctx, tx, reasoningWrite{
		reasoningCaller: caller, issue: key, at: at,
		nodes: []reasoningNode{{
			kind: graphstore.KindConstraint, id: id, visibility: input.Visibility,
			parentKind: graphstore.KindIssue, parentID: key, attributes: input.constraintAttributes(),
		}},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The issue's row is the first thing the write reads, under the issue's owner lock
		// (lockIssueOwner), so an issue nobody holds is this.
		s.writeHandlerError(w, errorf(http.StatusNotFound, "NOT_FOUND", "issue not found"))
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	node := written[0]
	WriteJSON(w, http.StatusCreated, constraintRead{
		Address: refs.StoreRef(graphstore.KindConstraint, id), ID: id,
		Standard: namedOrNull(input.Standard), Reference: namedOrNull(input.Reference),
		Measurement: namedOrNull(input.Measurement), Value: namedOrNull(input.Value),
		TakenWhere: namedOrNull(input.TakenWhere), Author: writtenAuthor(node),
		CreatedAt: node.MadeAt, State: node.State, Visibility: node.Visibility,
	})
}

// issueConstraintsQuery reads the constraints under one issue ($1) for a reader ($3): each
// constraint whose current part_of tuple points at the issue's node, in the order those tuples
// were appended, that is not gone ($2) and that the reader may see through the predicate of its
// kind (refs.VisibleKindRow). Beside each ride its fields, its stored state and visibility, read
// off the row the read joins (refs.StoredRowOf), and the far end of its current authored_by tuple
// (refs.CurrentFarEndSQL).
var issueConstraintsQuery = `
	select held.id, held.made_at,
	       held.attributes->>'` + graphstore.AttributeStandard + `', held.attributes->>'` + graphstore.AttributeReference + `',
	       held.attributes->>'` + graphstore.AttributeMeasurement + `', held.attributes->>'` + graphstore.AttributeValue + `',
	       held.attributes->>'` + graphstore.AttributeTakenWhere + `',
	       ` + refs.StoredRowOf(`held`) + `,
	       author.to_kind, author.to_id
	from (
		` + graphstore.CurrentTupleRead(`t.from_id`, `t.from_id, t.seq, t.asserted`,
	`t.to_kind = '`+graphstore.KindIssue+`' and t.to_id = $1 and t.kind = '`+graphstore.TuplePartOf+`'
			and t.from_kind = '`+graphstore.KindConstraint+`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `
	) placed
	join graph_nodes held on held.kind = '` + graphstore.KindConstraint + `' and held.id = placed.from_id
	left join lateral (
		` + refs.CurrentFarEndSQL(`held.kind`, `held.id`, graphstore.TupleAuthoredBy) + `
	) author on true
	where placed.asserted and held.state <> $2 and ` + refs.VisibleKindRow(graphstore.KindConstraint, "held", "$3") + `
	order by placed.seq`

// listIssueConstraints is GET /api/v1/issues/{key}/constraints: the constraints under the issue
// the caller may see, in the order they were written there, each with its fields, its author, its
// state and its visibility. A constraint reserved for its author is in its author's list alone,
// and an issue nobody holds is 404 NOT_FOUND. An issue that holds a constraint exists, so only an
// empty list asks whether the issue does.
func (s *server) listIssueConstraints(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	ctx, key := r.Context(), r.PathValue("key")
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := pool.Query(ctx, issueConstraintsQuery, key, graphstore.StateGone, refs.VisibilityArg(reader))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read := issueConstraints{Issue: key, Constraints: []constraintRead{}}
	for rows.Next() {
		var constraint constraintRead
		var authorKind, authorID *string
		if err := rows.Scan(&constraint.ID, &constraint.CreatedAt, &constraint.Standard, &constraint.Reference,
			&constraint.Measurement, &constraint.Value, &constraint.TakenWhere, &constraint.State, &constraint.Visibility,
			&authorKind, &authorID); err != nil {
			rows.Close()
			s.writeHandlerError(w, fmt.Errorf("scan a constraint of %s: %w", key, err))
			return
		}
		constraint.Address = refs.StoreRef(graphstore.KindConstraint, constraint.ID)
		constraint.CreatedAt = constraint.CreatedAt.UTC()
		if authorKind != nil && authorID != nil {
			constraint.Author = &reasoningAuthor{Kind: *authorKind, ID: *authorID}
		}
		read.Constraints = append(read.Constraints, constraint)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, fmt.Errorf("read the constraints of %s: %w", key, err))
		return
	}
	if len(read.Constraints) == 0 {
		if err := s.requireIssueExists(ctx, key); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	WriteJSON(w, http.StatusOK, read)
}

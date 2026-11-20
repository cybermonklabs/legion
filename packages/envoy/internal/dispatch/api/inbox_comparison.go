package api

import (
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/inboxcompare"
)

// GET /api/v1/inbox/comparison is the gate of dispatch://INFRA-1152 (R8): the newest run of
// `envoy-dispatch inbox-compare` (internal/dispatch/inboxcompare), read from its row in
// inbox_comparison_runs and from nothing else, as counts alone. It answers none of the run's
// differences, which name people and their rows, so the page that states the gate
// (web/src/features/inbox/ComparisonPage.tsx) shows every signed-in person the same figures and
// nobody's mail. The commit that moves every caller off the three reads the one read replaces
// deletes it, with the comparison, the page and the table.

// inboxComparisonShape is one shape of the corpus the gate requires, with the rows the run found
// of it.
type inboxComparisonShape struct {
	Shape string `json:"shape"`
	Rows  int    `json:"rows"`
}

// inboxComparisonRun is a run's row as the gate reads it.
type inboxComparisonRun struct {
	ID              int64     `json:"id"`
	RanAt           time.Time `json:"ran_at"`
	DifferenceCount int       `json:"difference_count"`
	PeopleChecked   int       `json:"people_checked"`
	PeopleUncovered int       `json:"people_uncovered"`
	// Shapes is every shape the gate requires, in the order the comparison names them, each with
	// the rows the run found of it.
	Shapes []inboxComparisonShape `json:"shapes"`
	// MissingShapes names each shape the run found no row of: a run missing one is refused as
	// proof, whatever it counted.
	MissingShapes []string `json:"missing_shapes"`
	// Met is the gate over this run: no difference, nobody uncovered and every shape held.
	Met bool `json:"met"`
}

// inboxComparisonResponse is what GET /api/v1/inbox/comparison answers: the newest run, null
// before the comparison has stored one.
type inboxComparisonResponse struct {
	Run *inboxComparisonRun `json:"run"`
}

func (s *server) getInboxComparison(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	newest, err := inboxcompare.Newest(r.Context(), s.deps.Store.Pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if newest == nil {
		WriteJSON(w, http.StatusOK, inboxComparisonResponse{})
		return
	}
	run := &inboxComparisonRun{
		ID: newest.ID, RanAt: newest.RanAt, DifferenceCount: newest.DifferenceCount,
		PeopleChecked: newest.PeopleChecked, PeopleUncovered: newest.PeopleUncovered,
		Shapes: []inboxComparisonShape{}, MissingShapes: []string{}, Met: newest.Met(),
	}
	for _, shape := range inboxcompare.Shapes {
		run.Shapes = append(run.Shapes, inboxComparisonShape{Shape: shape, Rows: newest.Shapes[shape]})
	}
	run.MissingShapes = append(run.MissingShapes, newest.MissingShapes()...)
	WriteJSON(w, http.StatusOK, inboxComparisonResponse{Run: run})
}

// legion_timeline.go serves GET /api/v1/legion/issues/{key}/timeline: one issue's Legion
// timeline (dispatch://INFRA-979), computed by the metrics package from the same population the
// metrics route reads, so the timeline's hours are the hours the metrics count for the issue.
package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// legionTimelineColumn is the board column a timeline phase is drawn as: its id, which names the
// colour the issues table gives it, and its label.
type legionTimelineColumn struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// legionTimelineResponse is the timeline with the board column of each phase it names, so a page
// draws a phase with the label and colour the issues table gives it and keeps no mapping of its
// own.
type legionTimelineResponse struct {
	metrics.IssueTimeline
	Project    string                          `json:"project"`
	Columns    map[string]legionTimelineColumn `json:"columns"`
	ComputedAt time.Time                       `json:"computed_at"`
}

// legionTimelinePhaseColumn is the board column a timeline phase falls in: the board's own
// mapping, with the two phases the timeline splits `admitted` into.
func legionTimelinePhaseColumn(phase string) (string, bool) {
	switch phase {
	case "design":
		return legionColumnDesign, true
	case "queued":
		return legionColumnQueue, true
	}
	column, ok := legionPhaseColumns[phase]
	return column, ok
}

func (s *server) getLegionIssueTimeline(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	var project string
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select project_key from issues where key = $1`, key).Scan(&project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := legion.GetSettings(r.Context(), s.deps.Store.Pool, project); err != nil {
		writeError(w, legionNotConfiguredCode, http.StatusNotFound, legionNotConfiguredMessage)
		return
	}
	now := time.Now().UTC()
	population, err := metrics.ReadIssue(r.Context(), s.deps.Store.Pool, s.deps.GitHub, project, key, now)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	timeline, ok := metrics.Timeline(population, key)
	if !ok {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "no such issue")
		return
	}
	labels := map[string]string{}
	for _, column := range legionBoardColumnOrder {
		labels[column.id] = column.label
	}
	columns := map[string]legionTimelineColumn{}
	note := func(phase string) {
		if id, ok := legionTimelinePhaseColumn(phase); ok {
			columns[phase] = legionTimelineColumn{ID: id, Label: labels[id]}
		}
	}
	for _, span := range timeline.Phases {
		note(span.Name)
	}
	for _, child := range timeline.Children {
		for _, span := range child.Phases {
			note(span.Name)
		}
	}
	WriteJSON(w, http.StatusOK, legionTimelineResponse{
		IssueTimeline: timeline, Project: project, Columns: columns, ComputedAt: now,
	})
}

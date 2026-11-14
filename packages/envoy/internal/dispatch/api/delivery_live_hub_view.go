package api

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// parseDeliveryLiveView resolves a subscribed view as the matching GET route reads its query, or
// answers the error frame refusing it: INVALID_FRAME for a view of no known shape, INVALID_QUERY
// for one the route would refuse. What each kind reads of a view is its own entry's `parse`
// (live_frames.go), so the only thing decided here is what every kind shares.
func parseDeliveryLiveView(view DeliveryLiveView) (deliveryLiveSpec, *LiveError) {
	classes, known := deliveryLiveKinds[view.Kind]
	if !known {
		return deliveryLiveSpec{}, invalidLiveView("INVALID_FRAME", "view.kind must be timeline, measures, pipeline, legion or evals")
	}
	if view.Project != nil && !classes.project {
		return deliveryLiveSpec{}, invalidLiveView("INVALID_FRAME", "only a legion or an evals view names a project")
	}
	spec := deliveryLiveSpec{kind: view.Kind}
	if refusal := classes.parse(&spec, view); refusal != nil {
		return deliveryLiveSpec{}, refusal
	}
	return spec, nil
}

// invalidLiveView is the error frame refusing a subscribed view.
func invalidLiveView(code, message string) *LiveError {
	refusal := liveRefusal("", code, message)
	return &refusal
}

// parseDeliveryWindowView reads a timeline or measures view: its window, and the query the
// matching GET route reads everything but the window from.
func parseDeliveryWindowView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	named, refusal := spec.readWindowAndQuery(view, "a "+view.Kind+" view")
	if refusal != nil {
		return refusal
	}
	spec.key = view.Kind + "|" + named + "|" + spec.query.Encode()
	return nil
}

// readWindowAndQuery reads a view that names a window and a query, as what names it: its window
// into spec, and its query without from and to, which the window names. It answers how the key
// names the window (readWindow).
func (spec *deliveryLiveSpec) readWindowAndQuery(view DeliveryLiveView, what string) (string, *LiveError) {
	if view.At != nil {
		return "", invalidLiveView("INVALID_FRAME", what+" names a window, not an at")
	}
	if view.Window == nil {
		return "", invalidLiveView("INVALID_FRAME", what+" names its window")
	}
	spec.query = url.Values{}
	if view.Query != nil {
		query, err := url.ParseQuery(*view.Query)
		if err != nil {
			return "", invalidLiveView("INVALID_QUERY", "query does not parse: "+err.Error())
		}
		spec.query = query
	}
	// The window names from and to; the query is everything else the GET route reads.
	spec.query.Del("from")
	spec.query.Del("to")
	return spec.readWindow(view.Window)
}

// parsePipelineLiveView reads a pipeline view: it names its window as a timeline view does, or an
// at, which is the seven days ending on its day, or neither, which is the seven days ending at
// the present. It reads no query: the Pipeline page follows none of the timeline's facets.
func parsePipelineLiveView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	if view.Query != nil {
		return invalidLiveView("INVALID_FRAME", "a pipeline view names a window or an at, not a query")
	}
	switch {
	case view.Window != nil && view.At != nil:
		return invalidLiveView("INVALID_FRAME", "a pipeline view names a window or an at, not both")
	case view.Window != nil:
		named, refusal := spec.readWindow(view.Window)
		if refusal != nil {
			return refusal
		}
		spec.key = "pipeline|" + named + "|"
	case view.At != nil:
		at, err := parseOptionalRFC3339(url.Values{"at": {*view.At}}, "at", time.Time{})
		if err != nil {
			return invalidLiveView("INVALID_QUERY", err.Error())
		}
		at = at.UTC()
		spec.at = &at
		spec.key = "pipeline|" + at.Format(time.RFC3339Nano) + "|"
	default:
		spec.key = "pipeline|now|"
	}
	return nil
}

// parseLegionLiveView reads a legion view: it names the project whose board it shows and nothing
// else, since the board has no window and the project is the whole of what
// GET /api/v1/projects/{key}/legion/board reads.
func parseLegionLiveView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	if view.Window != nil || view.At != nil || view.Query != nil {
		return invalidLiveView("INVALID_FRAME", "a legion view names a project, not a window, an at or a query")
	}
	project, refusal := readLiveProject(view, "a legion view")
	if refusal != nil {
		return refusal
	}
	spec.project = project
	spec.key = legionBoardView + "|" + project + "|"
	return nil
}

// parseEvalsLiveView reads an evals view: the project whose evals it shows, its window, as a
// timeline view names one, and its query, the query string GET /api/v1/projects/{key}/evals
// would be sent without from and to: the compare range (compare_from and compare_to), the
// repositories (repo), the kind (kind) and the page (limit and offset). A query the route
// refuses, a key it does not take among them, is refused here, by the route's own reading of it
// (parseEvalListQuery), before the view is held.
func parseEvalsLiveView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	project, refusal := readLiveProject(view, "an evals view")
	if refusal != nil {
		return refusal
	}
	named, refusal := spec.readWindowAndQuery(view, "an evals view")
	if refusal != nil {
		return refusal
	}
	if _, err := parseEvalListQuery(spec.query, evalListQueryKeys); err != nil {
		return invalidLiveView("INVALID_QUERY", err.Error())
	}
	spec.project = project
	spec.key = evalsView + "|" + project + "|" + named + "|" + spec.query.Encode()
	return nil
}

// readLiveProject reads the project a view names, as the project routes read the key in their
// path, or answers the refusal of the view, which what names in its message.
func readLiveProject(view DeliveryLiveView, what string) (string, *LiveError) {
	if view.Project == nil {
		return "", invalidLiveView("INVALID_FRAME", what+" names its project")
	}
	project := strings.TrimSpace(*view.Project)
	if !projectKeyPattern.MatchString(project) {
		return "", invalidLiveView("INVALID_QUERY", projectKeyProblem("project", project))
	}
	return project, nil
}

// readWindow reads a view's window into spec, as the GET routes read from and to, and answers how
// the key names it: a preset by its days, whose end stays at the present, or a custom range by its
// exact ends.
func (spec *deliveryLiveSpec) readWindow(window *DeliveryLiveWindow) (string, *LiveError) {
	refuse := func(code, message string) (string, *LiveError) {
		refusal := liveRefusal("", code, message)
		return "", &refusal
	}
	switch {
	case window.Preset != nil && window.From == nil && window.To == nil:
		if !slices.Contains(deliveryLivePresets, *window.Preset) {
			return refuse("INVALID_FRAME", "window.preset must be 7, 14, 28 or 84")
		}
		spec.preset = *window.Preset
		return "preset:" + strconv.Itoa(spec.preset), nil
	case window.Preset == nil && window.From != nil && window.To != nil:
		from, to, err := parseDeliveryWindow(url.Values{"from": {*window.From}, "to": {*window.To}})
		if err != nil {
			return refuse("INVALID_QUERY", err.Error())
		}
		spec.from, spec.to = from.UTC(), to.UTC()
		return spec.from.Format(time.RFC3339Nano) + "," + spec.to.Format(time.RFC3339Nano), nil
	default:
		return refuse("INVALID_FRAME", "a window is a preset or a from and to")
	}
}

// pipelineWindowAt is a pipeline view's window at now: its preset's days or its pinned at's seven,
// ending at the present, or the custom range it named, which never moves.
func (spec deliveryLiveSpec) pipelineWindowAt(now time.Time) pipelineWindow {
	switch {
	case !spec.to.IsZero():
		return pipelineWindowBetween(spec.from, spec.to)
	case spec.at != nil:
		return pipelineWindowEndingAt(*spec.at, measures.ShownDayCount)
	case spec.preset > 0:
		return pipelineWindowEndingAt(now, spec.preset)
	default:
		return pipelineWindowEndingAt(now, measures.ShownDayCount)
	}
}

// pipelineRolls is whether a pipeline view's window ends at the present, so it moves at midnight
// and as a result leaves the failing-jobs table; a pinned at and a custom range never move.
func (spec deliveryLiveSpec) pipelineRolls() bool {
	return spec.at == nil && spec.to.IsZero()
}

package api

import (
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// parseDeliveryLiveView resolves a subscribed view as the matching GET route reads its query, or
// answers the error frame refusing it: INVALID_FRAME for a view of no known shape, and for one the
// route would refuse, the code that route answers. What each kind reads of a view is its own
// entry's `parse` (live_frames.go), so the only thing decided here is what every kind shares.
func parseDeliveryLiveView(view DeliveryLiveView) (deliveryLiveSpec, *LiveError) {
	classes, known := deliveryLiveKinds[view.Kind]
	if !known {
		return deliveryLiveSpec{}, invalidLiveView("INVALID_FRAME", "view.kind must be timeline, measures, pipeline, legion or evals")
	}
	if view.Project != nil && !classes.reads.namesProject() {
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
// matching GET route reads everything but the window from, refused where that route would refuse
// it for naming a key it reads one value of more than once (repeatedQueryKey).
func parseDeliveryWindowView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	named, refusal := spec.readWindowAndQuery(view, "a "+view.Kind+" view")
	if refusal != nil {
		return refusal
	}
	if err := repeatedQueryKey(spec.query, deliveryWindowOneValueKeys); err != nil {
		return invalidLiveView("INVALID_QUERY", err.Error())
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

// parsePipelineLiveView reads a pipeline view: it names at most an at, which is the seven days
// ending on that day; with none it is the seven days ending at the present. It reads no window
// and no query, since the Pipeline page follows neither the timeline's window nor its facets. The
// at is read as the route reads it (parsePipelineAt), so an at the route refuses, one whose first
// shown day falls before the year 1 among them, is refused here before the view is held.
func parsePipelineLiveView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	if view.Window != nil || view.Query != nil {
		return invalidLiveView("INVALID_FRAME", "a pipeline view names at most an at")
	}
	spec.key = "pipeline|now|"
	if view.At != nil {
		at, err := parsePipelineAt(url.Values{"at": {*view.At}}, time.Time{})
		if err != nil {
			return invalidLiveView("INVALID_QUERY", err.Error())
		}
		spec.at = &at
		spec.key = "pipeline|" + at.Format(time.RFC3339Nano) + "|"
	}
	return nil
}

// parseLegionLiveView reads a legion view: it names the project whose board it shows and nothing
// else, since the board has no window and the project is the whole of what
// GET /api/v1/projects/{key}/legion/board reads. A badly spelled key is refused with the code and
// the words the board route answers it with (legionProject), which are every project route's
// (INVALID_PROJECT, projectKeyProblem), so a person who meets the mistake at either door learns
// one name for it. A project that does not exist is the read's to refuse, as the route's is
// (readLegionBoardView, loadProject).
func parseLegionLiveView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	if view.Window != nil || view.At != nil || view.Query != nil {
		return invalidLiveView("INVALID_FRAME", "a legion view names a project, not a window, an at or a query")
	}
	project, refusal := readLiveProject(view, "a legion view", "INVALID_PROJECT")
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
// refuses, a key it does not take among them, is refused here, by the route's own reading of the
// query the view's read sends it (evalsQuery, parseEvalListQuery), before the view is held. That
// query holds the view's own from and to: a custom window readWindow takes can still end its days
// past the year 9999, which the route refuses (evalDaysAnswerable), and a view held with it would
// read and fail every deliveryLiveRetry for as long as it stood, its subscribers told nothing.
func parseEvalsLiveView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	project, refusal := readLiveProject(view, "an evals view", "INVALID_QUERY")
	if refusal != nil {
		return refusal
	}
	named, refusal := spec.readWindowAndQuery(view, "an evals view")
	if refusal != nil {
		return refusal
	}
	if _, err := parseEvalListQuery(spec.evalsQuery(time.Now().UTC()), evalListQueryKeys); err != nil {
		return invalidLiveView("INVALID_QUERY", err.Error())
	}
	spec.project = project
	spec.key = evalsView + "|" + project + "|" + named + "|" + spec.query.Encode()
	return nil
}

// evalsQuery is the query an evals view's read sends GET /api/v1/projects/{key}/evals at now: the
// view's own query, with its window's from and to, a preset's ending at now.
func (spec deliveryLiveSpec) evalsQuery(now time.Time) url.Values {
	from, to := spec.from, spec.to
	if spec.preset > 0 {
		from, to = now.Add(-spec.days()), now
	}
	query := maps.Clone(spec.query)
	query.Set("from", from.Format(time.RFC3339Nano))
	query.Set("to", to.Format(time.RFC3339Nano))
	return query
}

// readLiveProject reads the project a view names, as the project routes read the key in their
// path, or answers the refusal of the view, which what names in its message. A key that is no
// project key is refused with badKey, the code the view's own refusal of it carries.
func readLiveProject(view DeliveryLiveView, what, badKey string) (string, *LiveError) {
	if view.Project == nil {
		return "", invalidLiveView("INVALID_FRAME", what+" names its project")
	}
	project := strings.TrimSpace(*view.Project)
	if !projectKeyPattern.MatchString(project) {
		return "", invalidLiveView(badKey, projectKeyProblem("project", project))
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

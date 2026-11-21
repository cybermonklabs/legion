package api

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// parseDeliveryLiveView resolves a subscribed view as the matching GET route reads its query, or
// answers the error frame refusing it: INVALID_FRAME for a view of no known shape, and for one the
// route would refuse, the code that route answers. What each kind reads of a view is its own
// entry's `parse` (live_frames.go), so the only thing decided here is what every kind shares.
func parseDeliveryLiveView(view DeliveryLiveView) (deliveryLiveSpec, *LiveError) {
	classes, known := deliveryLiveKinds[view.Kind]
	if !known {
		return deliveryLiveSpec{}, invalidLiveView("INVALID_FRAME", "view.kind must be timeline, measures, pipeline or legion")
	}
	if view.Project != nil && classes.reads != viewReadsLegion {
		return deliveryLiveSpec{}, invalidLiveView("INVALID_FRAME", "only a legion view names a project")
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
	if view.At != nil {
		return invalidLiveView("INVALID_FRAME", "a "+view.Kind+" view names a window, not an at")
	}
	if view.Window == nil {
		return invalidLiveView("INVALID_FRAME", "a "+view.Kind+" view names its window")
	}
	spec.query = url.Values{}
	if view.Query != nil {
		query, err := url.ParseQuery(*view.Query)
		if err != nil {
			return invalidLiveView(apicode.InvalidQuery.V1Name(), "query does not parse: "+err.Error())
		}
		spec.query = query
	}
	// The window names from and to; the query is everything else the GET route reads.
	spec.query.Del("from")
	spec.query.Del("to")
	named, refusal := spec.readWindow(view.Window)
	if refusal != nil {
		return refusal
	}
	spec.key = view.Kind + "|" + named + "|" + spec.query.Encode()
	return nil
}

// parsePipelineLiveView reads a pipeline view: it names at most an at, which is the seven days
// ending on that day; with none it is the seven days ending at the present. It reads no window
// and no query, since the Pipeline page follows neither the timeline's window nor its facets.
func parsePipelineLiveView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	if view.Window != nil || view.Query != nil {
		return invalidLiveView("INVALID_FRAME", "a pipeline view names at most an at")
	}
	spec.key = "pipeline|now|"
	if view.At != nil {
		at, err := parseOptionalRFC3339(url.Values{"at": {*view.At}}, "at", time.Time{})
		if err != nil {
			return invalidLiveView(apicode.InvalidQuery.V1Name(), err.Error())
		}
		at = at.UTC()
		spec.at = &at
		spec.key = "pipeline|" + at.Format(time.RFC3339Nano) + "|"
	}
	return nil
}

// parseLegionLiveView reads a legion view: it names the project whose board it shows and nothing
// else, since the board has no window and the project is all GET /api/v1/projects/{key}/legion/board reads.
func parseLegionLiveView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	if view.Window != nil || view.At != nil || view.Query != nil {
		return invalidLiveView("INVALID_FRAME", "a legion view names a project, not a window, an at or a query")
	}
	if view.Project == nil {
		return invalidLiveView("INVALID_FRAME", "a legion view names its project")
	}
	project := strings.TrimSpace(*view.Project)
	// A badly spelled key is refused with the code and the words the board route answers it with
	// (legionProject), which are every project route's (INVALID_PROJECT, projectKeyProblem), so a
	// person who meets the mistake at either door learns one name for it. A project that does not
	// exist is the read's to refuse, as the route's is (readLegionBoardView, loadProject).
	if !projectKeyPattern.MatchString(project) {
		return invalidLiveView(apicode.InvalidProject.V1Name(), projectKeyProblem("project", project))
	}
	spec.project = project
	spec.key = legionBoardView + "|" + project + "|"
	return nil
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
			return refuse(apicode.InvalidQuery.V1Name(), err.Error())
		}
		spec.from, spec.to = from.UTC(), to.UTC()
		return spec.from.Format(time.RFC3339Nano) + "," + spec.to.Format(time.RFC3339Nano), nil
	default:
		return refuse("INVALID_FRAME", "a window is a preset or a from and to")
	}
}

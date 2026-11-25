package api

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// parseDeliveryLiveView resolves a subscribed view as the matching GET route reads its query, or
// answers the error frame refusing it: INVALID_FRAME for a view of no known shape, and for one the
// route would refuse, the code that route answers. What each kind reads of a view is its own
// entry's `parse` (live_frames.go), so the only thing decided here is what every kind shares. The
// spec it answers names no reader yet (readFor).
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
			return invalidLiveView("INVALID_QUERY", "query does not parse: "+err.Error())
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
	spec.shape = view.Kind + "|" + named + "|" + spec.query.Encode()
	return nil
}

// parsePipelineLiveView reads a pipeline view: it names at most an at, which is the seven days
// ending on that day; with none it is the seven days ending at the present. It reads no window
// and no query, since the Pipeline page follows neither the timeline's window nor its facets.
func parsePipelineLiveView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	if view.Window != nil || view.Query != nil {
		return invalidLiveView("INVALID_FRAME", "a pipeline view names at most an at")
	}
	spec.shape = "pipeline|now|"
	if view.At != nil {
		at, err := parseOptionalRFC3339(url.Values{"at": {*view.At}}, "at", time.Time{})
		if err != nil {
			return invalidLiveView("INVALID_QUERY", err.Error())
		}
		at = at.UTC()
		spec.at = &at
		spec.shape = "pipeline|" + at.Format(time.RFC3339Nano) + "|"
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
	if !contracts.ValidProjectKey(project) {
		return invalidLiveView("INVALID_PROJECT", projectKeyProblem("project", project))
	}
	spec.project = project
	spec.shape = legionBoardView + "|" + project + "|"
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
			return refuse("INVALID_QUERY", err.Error())
		}
		spec.from, spec.to = from.UTC(), to.UTC()
		return spec.from.Format(time.RFC3339Nano) + "," + spec.to.Format(time.RFC3339Nano), nil
	default:
		return refuse("INVALID_FRAME", "a window is a preset or a from and to")
	}
}

// readFor makes spec the view reader reads: the reader, its part of the key (deliveryReaderKey),
// and the key itself, the view's shape and that part.
func (spec *deliveryLiveSpec) readFor(reader model.Actor) {
	spec.reader, spec.viewer = reader, deliveryReaderKey(reader)
	spec.key = spec.shape + "|" + spec.viewer
}

// deliveryReaderKey is the part of a view's key its reader decides: whether the reader is the
// server's own viewer and whether it is open; and, for a reader that does not see every object
// (seesEveryObject), the projects it sees, which the timeline's rule reads of a viewer
// (readDeliveryRows: access.ProjectClause on each pull request's issue, and openness for one that
// names none and for a run that shipped none), and its person and session, which the event rule
// the view's session titles are read under reads (delivery.ResolveSessionTitles under
// readableEvents: its party and own-session arms), as access.SessionClause's own-session arm does
// for the session groups (groupsShown). Readers that agree on the key read the same rows and share
// one view. A reader that sees every object is shown every row, whatever projects it sees, so where
// no project is private every open viewer shares the one view it always did, and a new project
// leaves it there: the view is read for the newest of its readers (recheckSubs), which sees the new
// project, and no page of it is sent a snapshot for the project (R9). A reader with no viewer is
// the closed one, which sees nothing.
func deliveryReaderKey(reader model.Actor) string {
	viewer := reader.Viewer
	if viewer == nil {
		viewer = &model.Viewer{}
	}
	key := "all=" + strconv.FormatBool(viewer.All) + ",open=" + strconv.FormatBool(viewer.Open)
	if !seesEveryObject(reader) {
		key += ",sees=" + strings.Join(viewer.Sees, ",") + ",person=" + viewer.Person + ",session=" + viewer.Session
	}
	return key
}

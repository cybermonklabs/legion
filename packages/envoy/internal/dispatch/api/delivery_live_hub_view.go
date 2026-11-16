package api

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// parseDeliveryLiveView resolves a subscribed view as the matching GET route reads its query, or
// answers the error frame refusing it: INVALID_FRAME for a view of no known shape, INVALID_QUERY
// for one the route would refuse.
func parseDeliveryLiveView(view DeliveryLiveView) (deliveryLiveSpec, *LiveError) {
	invalid := func(code, message string) (deliveryLiveSpec, *LiveError) {
		refusal := liveRefusal("", code, message)
		return deliveryLiveSpec{}, &refusal
	}
	spec := deliveryLiveSpec{kind: view.Kind}
	switch view.Kind {
	case deliveryLiveTimeline, deliveryLiveMeasures:
		if view.At != nil {
			return invalid("INVALID_FRAME", "a "+view.Kind+" view names a window, not an at")
		}
		if view.Window == nil {
			return invalid("INVALID_FRAME", "a "+view.Kind+" view names its window")
		}
		spec.query = url.Values{}
		if view.Query != nil {
			query, err := url.ParseQuery(*view.Query)
			if err != nil {
				return invalid("INVALID_QUERY", "query does not parse: "+err.Error())
			}
			spec.query = query
		}
		// The window names from and to; the query is everything else the GET route reads.
		spec.query.Del("from")
		spec.query.Del("to")
		window := view.Window
		switch {
		case window.Preset != nil && window.From == nil && window.To == nil:
			if !slices.Contains(deliveryLivePresets, *window.Preset) {
				return invalid("INVALID_FRAME", "window.preset must be 7, 14, 28 or 84")
			}
			spec.preset = *window.Preset
			spec.key = view.Kind + "|preset:" + strconv.Itoa(spec.preset) + "|" + spec.query.Encode()
		case window.Preset == nil && window.From != nil && window.To != nil:
			from, to, err := parseDeliveryWindow(url.Values{"from": {*window.From}, "to": {*window.To}})
			if err != nil {
				return invalid("INVALID_QUERY", err.Error())
			}
			spec.from, spec.to = from.UTC(), to.UTC()
			spec.key = view.Kind + "|" + spec.from.Format(time.RFC3339Nano) + "," + spec.to.Format(time.RFC3339Nano) + "|" + spec.query.Encode()
		default:
			return invalid("INVALID_FRAME", "a window is a preset or a from and to")
		}
	case deliveryLivePipeline:
		if view.Window != nil || view.Query != nil {
			return invalid("INVALID_FRAME", "a pipeline view names at most an at")
		}
		spec.key = "pipeline|now|"
		if view.At != nil {
			at, err := parseOptionalRFC3339(url.Values{"at": {*view.At}}, "at", time.Time{})
			if err != nil {
				return invalid("INVALID_QUERY", err.Error())
			}
			at = at.UTC()
			spec.at = &at
			spec.key = "pipeline|" + at.Format(time.RFC3339Nano) + "|"
		}
	default:
		return invalid("INVALID_FRAME", "view.kind must be timeline, measures or pipeline")
	}
	return spec, nil
}

// deliveryReaderKey is the part of a view's key its reader decides: whether the reader is the
// server's own viewer, whether it is open, and the projects it sees, which are all that
// access.ProjectClause and access.RepositoryClause read of a viewer. Readers that agree on the
// three read the same rows and share one view, so where no project is private every open viewer
// shares the one view it always did. A reader with no viewer is the closed one, which sees nothing.
func deliveryReaderKey(reader model.Actor) string {
	viewer := reader.Viewer
	if viewer == nil {
		viewer = &model.Viewer{}
	}
	return "all=" + strconv.FormatBool(viewer.All) + ",open=" + strconv.FormatBool(viewer.Open) +
		",sees=" + strings.Join(viewer.Sees, ",")
}

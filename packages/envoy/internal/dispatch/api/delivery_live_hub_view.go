package api

import (
	"encoding/json"
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
// entry's `parse` (live_frames.go), so the only things decided here are what every kind shares:
// that the kind is one of the six, and that only a kind that reads one project
// (viewReads.namesProject: a project's daemon or its evals) is given a project; a legion_issue
// view, the one such kind that names no project, refuses one itself (parseLegionIssueLiveView).
//
// An issue is refused by nothing here. The socket's decode (decodeSocketClientFrame, a plain
// json.Unmarshal, which type-checks every member its struct declares) accepts an `issue` member
// of any JSON type, since DeliveryLiveView holds it raw, and only legion_issue's parse reads it
// (parseLegionIssueLiveView). The timeline, measures, pipeline, legion and evals kinds ignore it
// whatever its type, as they did before any kind read one, so a frame a deployed page sends with
// one is answered exactly as it was and never becomes INVALID_FRAME.
func parseDeliveryLiveView(view DeliveryLiveView) (deliveryLiveSpec, *LiveError) {
	classes, known := deliveryLiveKinds[view.Kind]
	if !known {
		return deliveryLiveSpec{}, invalidLiveView("INVALID_FRAME", "view.kind must be timeline, measures, pipeline, legion, legion_issue or evals")
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
// repositories (repo), the kind (kind) and the page (limit and offset). A project that is no
// project key is refused PROJECT_NOT_FOUND, as the route answers it, since no project row holds
// such a key (loadProject); one the key's shape allows and no row holds is the read's to refuse.
// A query the route refuses, a key it does not take among them, is refused here, by the route's
// own reading of the query the view's read sends it (evalsQuery, parseEvalListQuery), before the
// view is held. That query holds the view's own from and to: a custom window readWindow takes can
// still end its days past the year 9999, which the route refuses (evalDaysAnswerable), and a view
// held with it would read and fail every deliveryLiveRetry for as long as it stood, its
// subscribers told nothing.
func parseEvalsLiveView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	project, refusal := readLiveProject(view, "an evals view", errProjectNotFound.code)
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

// parseLegionIssueLiveView reads a legion_issue view: it names the issue whose Legion state it
// shows and nothing else, since that state has no window and the key is all
// GET /api/v1/issues/{key}/legion reads. It is the one kind that reads an `issue` member, which
// the decode holds raw whatever its JSON type (DeliveryLiveView.Issue): it reads it as a string,
// and refuses one that is absent, null, empty or of another type INVALID_FRAME in this kind's own
// words, where every other kind ignores the member (parseDeliveryLiveView). It refuses a project
// itself too: the shared gate lets a project through to every kind that reads a project's daemon,
// this one included, and this view's project is its key's, never one the frame names.
//
// The view's project is the key's prefix before its first "-" (legionProjectOf, the rule
// legionDaemonSessionOf reads a key's project by): no route moves an issue to another project, so
// the prefix is the issue row's project_key, and a read of that project's daemon, or a write of
// its Legion setting, marks the view (dirtyLegionProject). The key is read as the route reads its
// path, unchecked: one Dispatch holds no issue of is the read's to refuse, NOT_FOUND in the
// route's words (readLegionIssueView).
func parseLegionIssueLiveView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	if view.Project != nil || view.Window != nil || view.At != nil || view.Query != nil {
		return invalidLiveView("INVALID_FRAME", "a legion_issue view names an issue, not a project, a window, an at or a query")
	}
	// A null member decodes as no string, and so as the empty key, which is refused with the rest.
	var issue string
	if len(view.Issue) == 0 || json.Unmarshal(view.Issue, &issue) != nil || issue == "" {
		return invalidLiveView("INVALID_FRAME", "a legion_issue view names its issue, a non-empty string")
	}
	spec.issue = issue
	spec.project = legionProjectOf(spec.issue)
	spec.key = legionIssueView + "|" + spec.issue + "|"
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

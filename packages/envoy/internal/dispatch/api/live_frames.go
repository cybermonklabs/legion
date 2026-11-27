package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// The Go mirror of packages/contracts/src/dispatch-api.ts's Live* and DeliveryLive* frames: what an
// open page and Dispatch send each other over GET /ws/live, the page's one live socket. A page
// subscribes to views by kind. A delivery view (LEGION-690) is answered a snapshot (the matching
// GET route's body and a revision per key), then only deltas: list items by id, map entries by
// key, counts two levels deep, and the other fields whole. The events view is the event feed,
// event by event, and the agent view one session's live conversation.

// The six delivery-hub views a page can subscribe to, by the GET route each mirrors. The first
// three answer the Delivery and Pipeline pages' bodies; the fourth is the Legion board of one
// project (legion_board.go), and the fifth one issue's Legion state (legion_issue.go). Neither
// Legion view has a window: each shows what the daemon's state says now. The sixth is one
// project's evals (evals.go), the list read over a window and a compare range.
const (
	deliveryLiveTimeline = "timeline"
	deliveryLiveMeasures = "measures"
	deliveryLivePipeline = "pipeline"
	legionBoardView      = "legion"
	legionIssueView      = "legion_issue"
	evalsView            = "evals"
)

// The two views beside the delivery views: the event feed, and one session's live conversation.
const (
	liveEventsView = "events"
	liveAgentView  = "agent"
)

// DeliveryLiveWindow is a window by preset (7, 14, 28 or 84 days), whose end stays at the
// present, or by the exact from/to of a custom range: exactly one of Preset and the From/To pair.
type DeliveryLiveWindow struct {
	Preset *int    `json:"preset,omitempty"`
	From   *string `json:"from,omitempty"`
	To     *string `json:"to,omitempty"`
}

// DeliveryLiveView is one view a page shows. A timeline or measures view names its Window and
// Query, the query string the page would send to the GET route without from/to (search and every
// facet); a pipeline view names at most an At; a legion view names its Project and nothing else,
// since the board has no window; a legion_issue view names its Issue and nothing else, which is
// all GET /api/v1/issues/{key}/legion reads; and an evals view names its Project, its Window and
// its Query, the query string the page would send to GET /api/v1/projects/{key}/evals without
// from/to (the compare range, the repositories, the kind and the page).
type DeliveryLiveView struct {
	Kind    string              `json:"kind"`
	Window  *DeliveryLiveWindow `json:"window,omitempty"`
	Query   *string             `json:"query,omitempty"`
	At      *string             `json:"at,omitempty"`
	Project *string             `json:"project,omitempty"`
	// Issue is the one issue a legion_issue view names, the key GET /api/v1/issues/{key}/legion
	// reads (dispatch://INFRA-4869), held as the frame's raw JSON. The socket's decode is a plain
	// json.Unmarshal (decodeSocketClientFrame), which type-checks every member its struct declares,
	// so a typed field would refuse a frame whose issue is a number or an object on any kind, a
	// frame every kind before this one subscribed. Raw, the decode accepts an `issue` member of any
	// JSON type on every kind; only legion_issue's parse reads it, as a string, and refuses what is
	// not one (parseLegionIssueLiveView), and every other kind ignores it whatever its type.
	Issue json.RawMessage `json:"issue,omitempty"`
}

// DeliveryLiveRevs is what a page holds, as revisions (deliveryLiveRevision) in the frame's own
// sections: by list and item id, by map and key, by count map, facet and value, and by the name
// of each field sent whole.
type DeliveryLiveRevs struct {
	Items  map[string]map[string]string            `json:"items"`
	Maps   map[string]map[string]string            `json:"maps"`
	Counts map[string]map[string]map[string]string `json:"counts"`
	Fields map[string]string                       `json:"fields"`
}

// DeliveryLiveRevChanges is a delta's revision changes, in DeliveryLiveRevs' shape; nil (JSON
// null) for a key that left. A field sent whole never leaves, so Fields holds no null.
type DeliveryLiveRevChanges struct {
	Items  map[string]map[string]*string            `json:"items,omitempty"`
	Maps   map[string]map[string]*string            `json:"maps,omitempty"`
	Counts map[string]map[string]map[string]*string `json:"counts,omitempty"`
	Fields map[string]string                        `json:"fields,omitempty"`
}

// LiveView is one view a subscription follows: a delivery view, by DeliveryLiveView's fields; the
// events view, by its kind alone; or an agent view, which names the session it watches.
type LiveView struct {
	DeliveryLiveView
	SessionID *string `json:"session_id,omitempty"`
}

// LiveSubscribe starts the subscription Sub names, following View, in place of any the socket
// already holds under Sub, of whatever kind. Resume, from a reconnecting page, is its view's kind's:
// a DeliveryLiveResume for a delivery view, an EventsLiveResume for the events view, and none for
// an agent view. It stays raw until the socket has read the view's kind, and is then decoded as
// that kind's alone, so a resume of another kind's shape is refused.
type LiveSubscribe struct {
	Type   string          `json:"type"`
	Sub    string          `json:"sub"`
	View   LiveView        `json:"view"`
	Resume json.RawMessage `json:"resume,omitempty"`
}

// DeliveryLiveResume is a reconnecting page's last applied change and its revisions: a delivery
// view's resume, from which Dispatch answers the delta.
type DeliveryLiveResume struct {
	Seq  string           `json:"seq"`
	Revs DeliveryLiveRevs `json:"revs"`
}

// EventsLiveResume is the events view's resume: the decimal id of the last event the page applied.
type EventsLiveResume struct {
	Since string `json:"since"`
}

// LiveUnsubscribe drops one subscription, of any kind.
type LiveUnsubscribe struct {
	Type string `json:"type"`
	Sub  string `json:"sub"`
}

// DeliveryLiveSnapshot is a subscription's first answer: exactly the matching GET route's body
// for the resolved window, the change position it reflects, when that change was recorded (nil
// while the log is empty), and the revision of every key the body holds.
type DeliveryLiveSnapshot struct {
	Type      string           `json:"type"`
	Sub       string           `json:"sub"`
	Seq       string           `json:"seq"`
	ChangedAt *time.Time       `json:"changed_at"`
	Body      json.RawMessage  `json:"body"`
	Revs      DeliveryLiveRevs `json:"revs"`
}

// DeliveryLiveItemChanges is one list's changes: each item new or changed, whole, in the body's
// order, and the ids of the items that left, sorted.
type DeliveryLiveItemChanges struct {
	Upsert []json.RawMessage `json:"upsert"`
	Remove []string          `json:"remove"`
}

// DeliveryLiveDelta is every later answer: only what differs from what the page holds. Set
// replaces fields whole, window among them whenever it moved, and never window alone; Maps and
// Counts carry a key's new value, or null when it left.
type DeliveryLiveDelta struct {
	Type      string                                `json:"type"`
	Sub       string                                `json:"sub"`
	Seq       string                                `json:"seq"`
	ChangedAt *time.Time                            `json:"changed_at"`
	Set       map[string]json.RawMessage            `json:"set"`
	Items     map[string]DeliveryLiveItemChanges    `json:"items"`
	Maps      map[string]map[string]json.RawMessage `json:"maps"`
	Counts    map[string]map[string]map[string]*int `json:"counts"`
	Revs      DeliveryLiveRevChanges                `json:"revs"`
}

// DeliveryLiveFreshness goes to every subscriber when the freshness row's verdict changes, and to
// a subscription right after its first answer, so a page that was not subscribed at a change still
// learns it; never otherwise. It carries the freshness object and the sources past their threshold
// ("prs", "runs", "ci", "events").
type DeliveryLiveFreshness struct {
	Type      string                         `json:"type"`
	Freshness delivery.DeliveryFreshnessView `json:"freshness"`
	Stale     []string                       `json:"stale"`
}

// LiveEventsStart opens every events subscription, before any LiveEventFrame: Since is the decimal
// id the subscription starts after, the page's resume clamped to the feed's head, or the head when
// the page sent none. ReplayComplete is true when the page resumed from an id at or below the
// head: the subscription replays every event after it before it goes live, so the page missed
// nothing and owes no refresh for the gap. A subscription that names no resume, and one whose
// resume is above the head (an id of a log this one replaced), was replayed nothing it missed and
// says false.
type LiveEventsStart struct {
	Type           string `json:"type"`
	Sub            string `json:"sub"`
	Since          string `json:"since"`
	ReplayComplete bool   `json:"replay_complete"`
}

// LiveEventFrame is one event of the feed: its decimal id and the event's own JSON encoding, byte
// for byte, which is what a page reads every event as.
type LiveEventFrame struct {
	Type  string          `json:"type"`
	Sub   string          `json:"sub"`
	ID    string          `json:"id"`
	Event json.RawMessage `json:"event"`
}

// LiveAgentFrame is one message of a session's agent stream: Event names it ("responder",
// "replay" or "frame"), and Data is the session's own JSON, which Dispatch relays and never parses.
type LiveAgentFrame struct {
	Type  string          `json:"type"`
	Sub   string          `json:"sub"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// LiveEnded says Dispatch ended the subscription Sub: it fell behind ("behind"), or the relay
// dropped its viewer ("dropped"). The page subscribes it again at once.
type LiveEnded struct {
	Type   string `json:"type"`
	Sub    string `json:"sub"`
	Reason string `json:"reason"`
}

// liveEndedBehind is an ended frame's reason for a subscription that fell behind: an events
// subscription the relay dropped, for a full buffer or a reset log, or one whose read failed; an
// agent view whose buffer filled.
const liveEndedBehind = "behind"

// liveEndedDropped is an ended frame's reason for an agent view the relay dropped, as a lost
// connection does, or whose relay call failed.
const liveEndedDropped = "dropped"

// liveEnded is the frame ending sub, for reason.
func liveEnded(sub, reason string) LiveEnded {
	return LiveEnded{Type: "ended", Sub: sub, Reason: reason}
}

// LiveError refuses a frame or a subscription, by the code the matching GET route would answer
// (INVALID_QUERY, DELIVERY_NOT_CONFIGURED, the board's INVALID_PROJECT, PROJECT_NOT_FOUND and
// LEGION_NOT_CONFIGURED, one issue's NOT_FOUND, and the evals view's INVALID_QUERY for a query
// its route refuses and PROJECT_NOT_FOUND for a project no row holds, one that is no project key
// among them), the socket's own (INVALID_FRAME, TOO_MANY_SUBSCRIPTIONS) or an agent view's
// (OAUTH_SCOPE_REQUIRED, SESSION_ID_INPUT, STREAM_UNAVAILABLE). Sub names the subscription it
// refuses, and is empty for a frame Dispatch could not read a sub from.
type LiveError struct {
	Type    string `json:"type"`
	Sub     string `json:"sub,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// LiveHeartbeat goes out every StreamHeartbeat beside the socket's control ping and carries
// nothing: a browser never shows a ping to the page's script, so this frame is how an open page
// tells a quiet socket from a dead one.
type LiveHeartbeat struct {
	Type string `json:"type"`
}

// liveHeartbeat is the one heartbeat frame.
var liveHeartbeat = LiveHeartbeat{Type: "heartbeat"}

// viewReads is what a view kind's answer is read from, and the hub derives from it everything that
// differs between the sources: what moves the view, whether its spec holds a project, whether it
// judges freshness, and what its subscribers are refused with (refusal).
type viewReads int

const (
	// viewReadsDelivery is the deployment's delivery rows: a change of one can move the view
	// (touchedBy), its read holds the delivery settings, its freshness is the hub's one verdict,
	// and a deployment with no delivery settings is DELIVERY_NOT_CONFIGURED.
	viewReadsDelivery viewReads = iota + 1
	// viewReadsLegion is one project's Legion daemon as the Legion intake recorded it, and for its
	// board the stored per-issue facts of the issues the board shows: the view's spec holds that
	// project, each record of its daemon or a write of its Legion setting marks it
	// (dirtyLegionProject), and so does each connection of the hub's listener (dirtyLegionViews),
	// since the change log carries neither; the one delivery change that touches one is the write
	// of a facts row of a board's project (touchedBy); it judges no freshness. A project that
	// names no daemon is LEGION_NOT_CONFIGURED to the board alone, whose read answers
	// legion.ErrNoSettings for it: one issue's read answers an issue of such a project with its
	// configured false body (readLegionIssue), so a legion_issue view is never refused it.
	viewReadsLegion
	// viewReadsEvals is one project's eval registry rows: the view names that project, the
	// changes that touch it are its project's readings, versions and day rows (touchedBy), it
	// judges no freshness, and a project no row holds is the read's PROJECT_NOT_FOUND.
	viewReadsEvals
)

// namesProject is whether a view that reads r is one project's, so its spec holds that project
// and the shared gate (parseDeliveryLiveView) lets a frame's project through to its parse: the
// Legion board, one issue's Legion state and the evals list are each one project's (a
// legion_issue view's project is its key's, and its parse refuses one the frame names), and the
// delivery views are the deployment's.
func (r viewReads) namesProject() bool {
	return r == viewReadsLegion || r == viewReadsEvals
}

// refusal is the code and message a view's subscribers are refused with when its read answered
// err, and refused false for an err that is a failed read, which the hub retries. A deployment
// that configured nothing the view reads is told so: DELIVERY_NOT_CONFIGURED to a delivery view,
// and LEGION_NOT_CONFIGURED only to a Legion board, the one Legion view whose read answers
// legion.ErrNoSettings. An *apiError is refused as the matching GET route answers it: a Legion
// board or an evals list of a project that does not exist is loadProject's 404
// PROJECT_NOT_FOUND, and a legion_issue view of a key Dispatch holds no issue of is
// readLegionIssue's 404 NOT_FOUND.
func (r viewReads) refusal(err error) (code, message string, refused bool) {
	var answered *apiError
	switch {
	case r == viewReadsDelivery && errors.Is(err, delivery.ErrNoSettings):
		return deliveryNotConfiguredCode, deliveryNotConfiguredMessage, true
	case r == viewReadsLegion && errors.Is(err, legion.ErrNoSettings):
		return legionNotConfiguredCode, legionNotConfiguredMessage, true
	case errors.As(err, &answered):
		return answered.code, answered.message, true
	}
	return "", "", false
}

// deliveryLiveClasses is everything the hub knows of one view kind: how a delta sends its body's
// fields, and what the hub does differently for it — what it reads, and the two functions that
// read a subscription and an answer. A kind's behaviour is declared here and nowhere else, so the
// hub's shared code branches on no kind's name and a new kind is one entry rather than a line in
// each of its files. What a view names beside its kind is its parse's to read and refuse, but for
// a project: the shared gate (parseDeliveryLiveView) refuses one on every kind that reads no one
// project (viewReads.namesProject: the timeline, measures and pipeline) before that kind's parse
// runs. An existing kind's parse ignores an `issue` member of any type, as it always did, and
// only legion_issue's reads one.
type deliveryLiveClasses struct {
	// body is the response struct the matching GET route writes.
	body reflect.Type
	// lists are sent item by item, each item by its "id" field.
	lists []string
	// maps are sent entry by entry, by key, an entry whole when its revision changes; each names
	// the subfields of an entry its revision leaves out (they are still sent, with the rest, when
	// the entry is).
	maps map[string][]string
	// counts are {facet: {value: count}}, sent count by count.
	counts []string
	// whole fields are sent whole when their revision changes; each names the subfields its
	// revision leaves out (they are still sent, with the rest, when the field is).
	whole map[string][]string
	// withChanges are sent with any delta when they moved, but never make a delta by themselves.
	withChanges []string
	// out are never in a delta: computed_at moves on every compute, and freshness goes out as
	// its own frame.
	out []string

	// reads is what the view's answer is read from (viewReads).
	reads viewReads
	// unownedP0 is whether the view shows the unowned P0 list, so a write of a P0 issue moves it
	// even where its own population does not name the issue.
	unownedP0 bool
	// evalVersions are the evals whose current version the view's answer reads, by slug: the
	// targets the timeline's and the measures' KPIs and the Pipeline's lead-time cards are judged
	// by, and pr-ci's min_n the Pipeline ranks its slowest jobs by, each of the deploy repository's
	// project. A new version of one in any project marks the view (touchedBy), and so does the
	// refresher's commit that moves which project that is (delivery.JudgingMoved).
	evalVersions []string
	// parse reads a subscribed view's own fields into its spec, or refuses it: what the matching
	// GET route reads of a query, including the key one view of this kind is held under.
	parse func(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError
	// read is the kind's own read and compute, outside the hub's lock: it fills the parts of the
	// read its kind holds and answers the body, which the caller encodes by these classes.
	read func(h *deliveryHub, ctx context.Context, spec deliveryLiveSpec, now time.Time, into *deliveryViewRead) (any, error)
}

// deliveryLiveKinds is each view kind's classes. The timeline's waiting entries have no id, and
// are few, so they are sent whole; its measures are sent whole on a change of anything but their
// own computed_at, window and freshness.
var deliveryLiveKinds = map[string]deliveryLiveClasses{
	deliveryLiveTimeline: {
		body:  reflect.TypeFor[deliveryTimelineWithMeasures](),
		lists: []string{"prs", "runs", "session_groups"},
		maps: map[string][]string{
			"issue_titles": nil, "session_titles": nil, "session_pr_counts": nil, "components": nil,
		},
		counts:       []string{"facet_counts", "color_counts"},
		whole:        map[string][]string{"waiting": nil, "measures": {"computed_at", "window", "freshness"}},
		withChanges:  []string{"window"},
		out:          []string{"freshness"},
		reads:        viewReadsDelivery,
		unownedP0:    true,
		evalVersions: evals.DeliveryTargetSlugs,
		parse:        parseDeliveryWindowView,
		read:         (*deliveryHub).readDeliveryView,
	},
	deliveryLiveMeasures: {
		body:         reflect.TypeFor[deliveryMeasuresResponse](),
		whole:        map[string][]string{"measures": nil, "flags_source": nil, "unowned_p0": nil, "targets": nil, "status": nil},
		withChanges:  []string{"window"},
		out:          []string{"computed_at", "freshness"},
		reads:        viewReadsDelivery,
		unownedP0:    true,
		evalVersions: evals.DeliveryTargetSlugs,
		parse:        parseDeliveryWindowView,
		read:         (*deliveryHub).readDeliveryView,
	},
	deliveryLivePipeline: {
		body: reflect.TypeFor[deliveryPipelineResponse](),
		whole: map[string][]string{
			"days": nil, "pr_checks": nil, "staging_e2e": nil, "staging_apply": nil, "production_apply": nil,
			"merge_to_production": nil, "opened_to_merge": nil, "targets": nil, "status": nil,
		},
		out:          []string{"computed_at", "freshness"},
		reads:        viewReadsDelivery,
		evalVersions: evals.PipelineSlugs,
		parse:        parsePipelineLiveView,
		read:         (*deliveryHub).readPipelineView,
	},
	// The Legion board is one project's small, whole picture, so every field of it but its
	// computed_at and its issues is sent whole: its twelve columns are a fixed order rather than
	// a list a page merges by id (the page's applyDelta sorts each list it knows, which a column
	// order would have to be taught), and the header and the settings the daemon reports each
	// move as a block.
	//
	// The issues map is sent entry by entry, by issue key, because the facts consumer writes one
	// issue's row per change: a whole field would resend every issue's stored facts whenever one
	// issue's moved, and a project holds a row for every issue Legion ever worked.
	//
	// An entry is sent only when its stored row moved. Each read closes every open row at its own
	// clock (legionfacts.Row.At), so an open issue's hours, work time, slot hours and last moment
	// of work, and the as_of they are of, differ on every recompute, each record the Legion intake
	// makes of the daemon among them; the entry's revision leaves out every value the entry's
	// growth names (legionBoardGrowth's fields, which are the entry's own names for the values it
	// grows) and their as_of, so a recompute that moved only the clock sends no entry, where it
	// would otherwise resend every open issue's (about 2,600 at INFRA's size), and a value that
	// comes to grow is left out once its rate joins the growth. What the revision keeps moves
	// whenever the stored row does: a write of a row whose values moved stamps its updated_at
	// (legionfacts.WriteProject), and its growth, why it waits, its close and every other value
	// the entry carries are kept as stored. An entry whose values grow carries its as_of and its
	// growth, so a page closes the values it holds at its own clock by the rule the read closes
	// them by; one whose values stand carries null for both. An entry that is sent goes whole, its
	// values grown to the moment of the recompute that sent it.
	legionBoardView: {
		body: reflect.TypeFor[legionBoardResponse](),
		maps: map[string][]string{
			"issues": append(slices.Sorted(maps.Keys(bodyKeysOf(reflect.TypeFor[legionBoardGrowth]()))), "as_of"),
		},
		whole: map[string][]string{
			"project": nil, "header": nil, "columns": nil, "settings": nil,
		},
		out:   []string{"computed_at"},
		reads: viewReadsLegion,
		parse: parseLegionLiveView,
		read:  (*deliveryHub).readLegionBoardView,
	},
	// One issue's Legion state is as small and whole as the board, and carries no computed time:
	// every field is sent whole, nothing is left out, and a recompute that finds nothing moved
	// sends nothing. No Dispatch event moves it, so its read names no issue: a read of its
	// project's daemon and a write of that project's Legion setting do (legion.StateChannel,
	// dirtyLegionProject), and so does a write of one of the issue's runs that stores something
	// new (workflowstore.RunsChannel, dirtyLegionIssue).
	legionIssueView: {
		body: reflect.TypeFor[legionIssueResponse](),
		whole: map[string][]string{
			"issue": nil, "project": nil, "configured": nil, "state": nil, "read": nil, "runs_revision": nil,
		},
		reads: viewReadsLegion,
		parse: parseLegionIssueLiveView,
		read:  (*deliveryHub).readLegionIssueView,
	},
	// One project's evals: the body of GET /api/v1/projects/{key}/evals, read by the route's own
	// function (readEvalList). Its items are sent eval by eval, by id, since a write moves one
	// eval's rows at a time (a reading, a version of its definition, the refresher's rewrite of
	// its day rows); every other field is sent whole. The window is whole UTC days, which for a
	// preset move only when a day turns, so a page holding it is sent it only then; the compare
	// range is the one the view's query names, which never moves. The freshness's refresher age is
	// measured at the moment of the read, so its revision leaves that age out, and the hub's read
	// folds in its coarse step instead (refresherAgeStep): it goes with the freshness when the
	// folded rows' own age moves, or the step does, which the view's expiry reads (readEvalsView).
	//
	// It shows the unowned-p0 eval, whose count the list reads from the project's stored row of
	// today alone, so a P0 issue's event, which moves no stored row, marks no evals view: the
	// refresher's rewrite of that row, which the same event wakes, logs the eval_days change that
	// marks the views of that project alone.
	evalsView: {
		body:  reflect.TypeFor[evalListResponse](),
		lists: []string{"items"},
		whole: map[string][]string{
			"total": nil, "limit": nil, "offset": nil, "window": nil, "compare": nil,
			"freshness": {"refresher_age_seconds"},
		},
		reads: viewReadsEvals,
		parse: parseEvalsLiveView,
		read:  (*deliveryHub).readEvalsView,
	},
}

// classes is the view kind's own entry; a spec the hub holds always names a kind the table has,
// since parseDeliveryLiveView refuses every other one.
func (spec deliveryLiveSpec) classes() deliveryLiveClasses {
	return deliveryLiveKinds[spec.kind]
}

// deliveryLiveRevision is a value's revision: the 64-bit FNV-1a of its JSON encoding, as 16
// lowercase hex digits.
func deliveryLiveRevision(encoded []byte) string {
	hash := fnv.New64a()
	_, _ = hash.Write(encoded)
	return hex.EncodeToString(hash.Sum(nil))
}

// refresherAgeLastStep is the refresher age past which refresherAgeStep moves no step more: some
// two years, past any age a deployment's refresher keeps, and short of the doubling that would
// overflow a time.Duration.
const refresherAgeLastStep = time.Minute << 20

// refresherAgeStep is the coarse form of the refresher's age that an evals view keeps in its
// freshness's revision, which leaves the exact age out, and the age at which it next moves: 1
// under a minute, and one more for each doubling of the age from a minute on (2 from one minute, 3
// from two, 4 from four), so its page is sent the age again within twice the time it last held,
// and a read whose age moved within one step sends nothing. Past refresherAgeLastStep the step
// moves no more, and next is 0.
func refresherAgeStep(age time.Duration) (step int, next time.Duration) {
	step, next = 1, time.Minute
	for age >= next {
		if next >= refresherAgeLastStep {
			return step + 1, 0
		}
		step, next = step+1, 2*next
	}
	return step, next
}

// foldRefresherAgeStep folds step, the coarse refresher age an evals body states
// (refresherAgeStep), into the revision of its freshness, the field that states the age: e is a
// body just encoded, which no subscriber holds yet.
func (e *deliveryLiveEncoded) foldRefresherAgeStep(step int) {
	freshness := e.fields["freshness"]
	freshness.rev = deliveryLiveRevision([]byte(freshness.rev + "/" + strconv.Itoa(step)))
	e.fields["freshness"], e.revs.Fields["freshness"] = freshness, freshness.rev
}

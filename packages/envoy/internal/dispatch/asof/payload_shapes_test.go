package asof_test

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
)

// testdata/capture.sh writes both: the shape list, one row per event type and top-level key set
// of the payloads in production's log with the id of the first event of that shape, and the first
// event of each shape of a type a shadow reads, as the event log answered it and redacted, as
// payloads/<type>-<first_id>.json.
const (
	payloadShapesFile = "testdata/payload_shapes.tsv"
	payloadsDirectory = "testdata/payloads"
)

// shapeReaders seed a captured event of each type whose payload a shadow reads, with the rows the
// shadow reads beside it, and hold each field the shadow takes from the payload to the payload.
var shapeReaders = map[string]func(*shapeSeed){
	"issue.created":            readIssueState,
	"issue.updated":            readIssueState,
	"issue.closed":             readIssueState,
	"issue.claimed":            readIssueClaim,
	"issue.released":           readIssueClaim,
	"ask.opened":               readAskState,
	"ask.answered":             readAskState,
	"ask.edited":               readAskState,
	"ask.resolved":             readAskState,
	"ask.handed_back":          readAskState,
	"ask.asked_of":             readAskState,
	"ask.anchor_refreshed":     readAskState,
	"ask.follower_added":       readAskFollower,
	"ask.follower_removed":     readAskFollower,
	"comment.created":          readCommentState,
	"comment.edited":           readCommentState,
	"comment.resolved":         readCommentState,
	"comment.reopened":         readCommentState,
	"comment.anchor_refreshed": readCommentState,
	"comment.delivery":         readCommentDelivery,
	"message.created":          readMessageState,
	"message.answered":         readMessageState,
	"message.delivery":         readMessageDelivery,
	"project.created":          readProjectSteps,
	"project.updated":          readProjectSteps,
	"settings.project_default_assignee.updated": readProjectDefault,
	"settings.repo_project.updated":             readRepoProject,
}

// Each shadow reads a table's past from the payloads of its events, and production's log holds
// each payload in the shape of the build that wrote it: an older event lacks each key added after
// it. For the first event of each shape of each type that a shadow reads, captured from production
// and redacted, the test seeds the event and the rows that the shadow reads beside it, built from
// the event's own ids. Then it reads the shadowed row at a cut after the event. A field the shadow
// takes from a key the shape has is the payload's value. A field whose key the shape lacks, or
// holds as null, is the column's default, or a field that AbsentFields names for the routes that
// answer the table. It is never another value. The present rows hold other values, so a shadow
// that reads the row in place of the payload fails.
//
// First the inventory: each row of the shape list whose type the shadows name has a reader here
// and its captured event, and each captured event is such a row.
func TestEachShadowReadsEveryPayloadShapeProductionHolds(t *testing.T) {
	named := eventTypesTheShadowsName()
	captured := map[string]bool{}
	for _, name := range payloadFixtureNames(t) {
		captured[name] = true
	}
	var shapes []payloadShape
	for _, shape := range readPayloadShapes(t) {
		switch {
		case !named[shape.eventType]:
			continue
		case shapeReaders[shape.eventType] == nil:
			t.Errorf("the shadows read %s events and production holds them (first event %d), but no reader of this test reads them",
				shape.eventType, shape.firstID)
		case !captured[shape.name()]:
			t.Errorf("%s/%s.json is missing: testdata/capture.sh captures the first event of each shape of a type a shadow reads",
				payloadsDirectory, shape.name())
		default:
			shapes = append(shapes, shape)
		}
		delete(captured, shape.name())
	}
	for _, name := range slices.Sorted(maps.Keys(captured)) {
		t.Errorf("%s/%s.json is the first event of no shape of a type a shadow reads: no reader reads it", payloadsDirectory, name)
	}
	for _, eventType := range slices.Sorted(maps.Keys(shapeReaders)) {
		if !named[eventType] {
			t.Errorf("this test reads %s events, and no shadow reads them", eventType)
		}
	}

	w := newWorld(t)
	defaults := &columnDefaults{w: w, values: map[string]any{}}
	for _, shape := range shapes {
		t.Run(shape.name(), func(t *testing.T) {
			shapeReaders[shape.eventType](seedShape(t, w, defaults, readCapturedEvent(t, shape)))
		})
	}
}

// eventTypeLiteral is an event type as a SQL string literal: words of lower-case letters and
// underscores, joined by dots ('issue.created', 'settings.repo_project.updated').
var eventTypeLiteral = regexp.MustCompile(`'([a-z_]+(?:\.[a-z_]+)+)'`)

// eventTypesTheShadowsName are the event types that the shadows' SQL names: the types whose events
// a shadow reads.
func eventTypesTheShadowsName() map[string]bool {
	named := map[string]bool{}
	sql := asof.Shadows(asof.CutAt(time.Now()), asof.ShadowedTables()...)
	for _, match := range eventTypeLiteral.FindAllStringSubmatch(sql, -1) {
		named[match[1]] = true
	}
	return named
}

// payloadShape is one row of the shape list.
type payloadShape struct {
	eventType string
	firstID   int64
	// keys are the payload's top-level keys, sorted and joined by commas. The measurement that
	// made the list wrote _broken for a payload it could not parse, which holds a \u0000 escape.
	keys string
}

// name is the name of the shape's captured event in payloadsDirectory, less .json.
func (s payloadShape) name() string { return s.eventType + "-" + strconv.FormatInt(s.firstID, 10) }

// readPayloadShapes reads the shape list: a TSV with the columns type, first_id, first_at, events
// and payload_keys, under a header that names them.
func readPayloadShapes(t *testing.T) []payloadShape {
	t.Helper()
	raw, err := os.ReadFile(payloadShapesFile)
	if err != nil {
		t.Fatalf("read the shape list: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if lines[0] != "type\tfirst_id\tfirst_at\tevents\tpayload_keys" {
		t.Fatalf("%s starts %q, want the header type, first_id, first_at, events, payload_keys", payloadShapesFile, lines[0])
	}
	shapes := make([]payloadShape, 0, len(lines)-1)
	for number, line := range lines[1:] {
		columns := strings.Split(line, "\t")
		if len(columns) != 5 {
			t.Fatalf("%s line %d holds %d columns, want 5", payloadShapesFile, number+2, len(columns))
		}
		firstID, err := strconv.ParseInt(columns[1], 10, 64)
		if err != nil {
			t.Fatalf("%s line %d: first_id %q: %v", payloadShapesFile, number+2, columns[1], err)
		}
		shapes = append(shapes, payloadShape{eventType: columns[0], firstID: firstID, keys: columns[4]})
	}
	return shapes
}

// payloadFixtureNames are the names of the captured events in payloadsDirectory, less .json.
func payloadFixtureNames(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(payloadsDirectory, "*.json"))
	if err != nil {
		t.Fatalf("list the captured events: %v", err)
	}
	names := make([]string, len(paths))
	for index, path := range paths {
		names[index] = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	return names
}

// capturedEvent is a captured event, as GET /api/v1/events/log answers an event.
type capturedEvent struct {
	ID         int64           `json:"id"`
	IssueKey   *string         `json:"issue_key"`
	ArtifactID *string         `json:"artifact_id"`
	Project    string          `json:"project"`
	Seq        int             `json:"seq"`
	Type       string          `json:"type"`
	Actor      json.RawMessage `json:"actor"`
	CreatedAt  time.Time       `json:"created_at"`
	Payload    json.RawMessage `json:"payload"`
	// payload is Payload, decoded.
	payload map[string]any
}

// readCapturedEvent reads the captured event of shape, and fails unless it is the shape's first
// event and its payload holds the shape's keys.
func readCapturedEvent(t *testing.T, shape payloadShape) capturedEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(payloadsDirectory, shape.name()+".json"))
	if err != nil {
		t.Fatalf("read the captured event: %v", err)
	}
	var event capturedEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("decode the captured event: %v", err)
	}
	if event.ID != shape.firstID || event.Type != shape.eventType {
		t.Fatalf("the captured event is %s event %d, want %s event %d", event.Type, event.ID, shape.eventType, shape.firstID)
	}
	if err := json.Unmarshal(event.Payload, &event.payload); err != nil {
		t.Fatalf("decode the captured payload: %v", err)
	}
	if keys := strings.Join(slices.Sorted(maps.Keys(event.payload)), ","); shape.keys != "_broken" && keys != shape.keys {
		t.Fatalf("the captured payload holds the keys %s, want the shape's %s", keys, shape.keys)
	}
	return event
}

// presentActor and presentApproval are what the seeded rows hold where the payload holds its own
// value: a shadow that reads them is reading the row.
var (
	presentActor    = []byte(`{"kind":"user","id":"present"}`)
	presentApproval = []byte(`{"artifact_id":"00000000-0000-4000-8000-00000000a99a","name":"The present document","version":1,"requested_version":1,` +
		`"documents":[{"artifact_id":"00000000-0000-4000-8000-00000000a99a","name":"The present document","version":1,"requested_version":1}]}`)
)

// shapeSeed is the world of one captured event: a transaction, rolled back when the subtest ends,
// that holds the project ACME (the redaction's one project), the row of the event's owner, and
// what the event's reader seeds. The cut is a second after the event.
type shapeSeed struct {
	t        *testing.T
	tx       pgx.Tx
	defaults *columnDefaults
	event    capturedEvent
	payload  map[string]any
	cut      asof.Cut
	// issue, artifact and project are the event's owner as events holds it: the issue or the
	// document that the envelope names, else the project for an event in the project's sequence
	// (seq above 0), else none, as for a direct message.
	issue, artifact, project *string
	seq                      int
}

// seedShape opens the captured event's world: the project ACME and the row of the event's owner,
// with values that are not the payload's.
func seedShape(t *testing.T, w world, defaults *columnDefaults, event capturedEvent) *shapeSeed {
	t.Helper()
	ctx := context.Background()
	tx, err := w.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	s := &shapeSeed{t: t, tx: tx, defaults: defaults, event: event, payload: event.payload, cut: asof.CutAt(event.CreatedAt.Add(time.Second))}
	made := event.CreatedAt.Add(-24 * time.Hour)
	s.exec(`insert into projects (key, name, default_assignee, gated_steps, created_at)
		values ('ACME', 'ACME', 'present-assignee', '{spec}', $1)`, made)
	switch {
	case event.IssueKey != nil:
		s.issue = event.IssueKey
		key := *event.IssueKey
		number, err := strconv.Atoi(key[strings.LastIndex(key, "-")+1:])
		if err != nil {
			t.Fatalf("the issue key %q: %v", key, err)
		}
		status := otherThan([]string{"icebox", "backlog"}, s.optionalString("status"))
		s.exec(`insert into issues (key, project_key, number, title, status, labels, rank, assignee, route,
				created_by, created_at, updated_at, claimed_by, claimed_at)
			values ($1, 'ACME', $2, 'The present title', $3, '{present-label}', 'present', 'present-assignee', 'role:present',
				$4, $5, $5, $4, $5)`, key, number, status, presentActor, made)
	case event.ArtifactID != nil:
		s.artifact = event.ArtifactID
		s.exec(`insert into artifacts (id, project_key, slug, name, kind, created_by, created_at)
			values ($1, 'ACME', 'present-document', 'The present document', 'doc', $2, $3)`, *event.ArtifactID, presentActor, made)
	case event.Seq > 0:
		s.project = &event.Project
	}
	return s
}

func (s *shapeSeed) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.tx.Exec(context.Background(), sql, args...); err != nil {
		s.t.Fatalf("seed (%s): %v", strings.Join(strings.Fields(sql), " "), err)
	}
}

// appendEvent appends an event of the captured event's owner and actor, next in the owner's
// sequence, offset seconds and ids after the captured event: 0 is the captured event itself, a
// negative offset an event before it, and an offset above 1 an event after the cut. payload is
// the captured payload's raw JSON or a value to encode.
func (s *shapeSeed) appendEvent(offset int64, eventType string, payload any) {
	s.t.Helper()
	encoded, ok := payload.(json.RawMessage)
	if !ok {
		var err error
		if encoded, err = json.Marshal(payload); err != nil {
			s.t.Fatalf("encode the %s payload: %v", eventType, err)
		}
	}
	seq := 0
	if s.issue != nil || s.artifact != nil || s.project != nil {
		s.seq++
		seq = s.seq
	}
	s.exec(`insert into events (id, issue_key, artifact_id, project_key, seq, type, actor, payload, notify, created_at)
		values ($1, $2, $3::uuid, $4, $5, $6, $7, $8, false, $9)`,
		s.event.ID+offset, s.issue, s.artifact, s.project, seq, eventType, []byte(s.event.Actor), []byte(encoded),
		s.event.CreatedAt.Add(time.Duration(offset)*time.Second))
}

// appendCaptured appends the captured event as it was captured.
func (s *shapeSeed) appendCaptured() { s.appendEvent(0, s.event.Type, s.event.Payload) }

// read runs sql at the cut, bound to the shadows as the store binds a statement under a cut.
func (s *shapeSeed) read(sql string, args ...any) []map[string]any {
	s.t.Helper()
	return s.readAt(s.cut, sql, args...)
}

// readAt runs sql at cut, bound to the shadows.
func (s *shapeSeed) readAt(cut asof.Cut, sql string, args ...any) []map[string]any {
	s.t.Helper()
	bound, err := asof.Bind(sql, cut)
	if err != nil {
		s.t.Fatalf("bind (%s): %v", sql, err)
	}
	rows, err := s.tx.Query(context.Background(), bound, args...)
	return scanAll(s.t, rows, err)
}

// value runs sql at the cut, a statement that answers one row of one jsonb column v, and decodes v.
func (s *shapeSeed) value(sql string, args ...any) any {
	s.t.Helper()
	rows := s.read(sql, args...)
	if len(rows) != 1 {
		s.t.Fatalf("%s answers %d rows at the cut, want one", sql, len(rows))
	}
	var decoded any
	if err := json.Unmarshal([]byte(rows[0]["v"].(string)), &decoded); err != nil {
		s.t.Fatalf("decode the answer of %s: %v", sql, err)
	}
	return decoded
}

// payloadString is the payload's string member key; the test fails without one.
func (s *shapeSeed) payloadString(key string) string {
	s.t.Helper()
	value, ok := s.payload[key].(string)
	if !ok || value == "" {
		s.t.Fatalf("the payload holds no %s", key)
	}
	return value
}

// optionalString is the payload's string member key, or "" when it holds none.
func (s *shapeSeed) optionalString(key string) string {
	value, _ := s.payload[key].(string)
	return value
}

// payloadField is a column that a shadow takes from its event's payload, and the payload member
// that it takes it from.
type payloadField struct {
	column string
	member []string
	kind   valueKind
}

func field(column string, kind valueKind, member ...string) payloadField {
	return payloadField{column: column, member: member, kind: kind}
}

// valueKind is how a field is compared: the column rendered as a JSON value in SQL, and the
// payload's member as the same JSON value in Go.
type valueKind int

const (
	textValue         valueKind = iota // text, or a uuid read as text
	nonEmptyTextValue                  // text that the shadow reads as null when it is empty (nullif)
	timeValue                          // timestamptz, at microseconds in UTC
	jsonbValue                         // jsonb
	textsValue                         // text[]; a member that is not an array gives none
	scalarValue                        // a boolean or a number
)

const timeLayout = "2006-01-02T15:04:05.000000Z"

// render is expr as a jsonb value of the kind.
func (k valueKind) render(expr string) string {
	switch k {
	case textValue, nonEmptyTextValue:
		return "to_jsonb((" + expr + ")::text)"
	case timeValue:
		return `to_jsonb(to_char((` + expr + `) at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))`
	case jsonbValue:
		return "(" + expr + ")::jsonb"
	default:
		return "to_jsonb(" + expr + ")"
	}
}

// fromPayload is the JSON value that a column of the kind holds when the shadow reads it from
// value, a payload member that is not null.
func (k valueKind) fromPayload(value any) (any, error) {
	switch k {
	case textValue, nonEmptyTextValue:
		text, ok := value.(string)
		if !ok {
			raw, err := json.Marshal(value)
			return string(raw), err
		}
		if text == "" && k == nonEmptyTextValue {
			return nil, nil
		}
		return text, nil
	case timeValue:
		text, _ := value.(string)
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return nil, err
		}
		return parsed.UTC().Format(timeLayout), nil
	case textsValue:
		list, _ := value.([]any)
		texts := make([]any, len(list))
		for index, element := range list {
			if text, ok := element.(string); ok {
				texts[index] = text
				continue
			}
			raw, err := json.Marshal(element)
			if err != nil {
				return nil, err
			}
			texts[index] = string(raw)
		}
		return texts, nil
	default:
		return value, nil
	}
}

// member is the payload's member at path: a null on the way reads as a null member, and a member
// missing on the way reads as one the payload lacks.
func member(payload map[string]any, path []string) (value any, present bool) {
	value = payload
	for _, key := range path {
		if value == nil {
			return nil, true
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		if value, ok = object[key]; !ok {
			return nil, false
		}
	}
	return value, true
}

// checkFields reads at the cut the one row of table that where selects, and holds each of fields to
// the payload. It reads every column of the row first, as a route's select * does, so each
// expression of the shadow (a cast, a search vector) runs over the payload's shape.
func (s *shapeSeed) checkFields(table string, fields []payloadField, where string, args ...any) {
	s.t.Helper()
	if rows := s.read("select * from "+table+" where "+where, args...); len(rows) != 1 {
		s.t.Fatalf("%s at the cut holds %d rows where %s %v, want the one row this event gives", table, len(rows), where, args)
	}
	pairs := make([]string, len(fields))
	for index, field := range fields {
		pairs[index] = "'" + field.column + "', " + field.kind.render(field.column)
	}
	rows := s.read("select jsonb_build_object("+strings.Join(pairs, ", ")+")::text as v from "+table+" where "+where, args...)
	var got map[string]any
	if err := json.Unmarshal([]byte(rows[0]["v"].(string)), &got); err != nil {
		s.t.Fatalf("decode the row of %s: %v", table, err)
	}
	for _, field := range fields {
		want, source := s.expected(table, field)
		if !reflect.DeepEqual(got[field.column], want) {
			s.t.Errorf("%s.%s at the cut is %s, want %s, %s", table, field.column, show(got[field.column]), show(want), source)
		}
	}
}

// expected is the value that field takes from the payload, and where that value comes from.
func (s *shapeSeed) expected(table string, field payloadField) (any, string) {
	s.t.Helper()
	path := strings.Join(field.member, ".")
	value, present := member(s.payload, field.member)
	switch {
	case !present:
		return s.defaults.of(s.t, table, field), "the column's default: the shape has no " + path
	case value == nil:
		return s.defaults.of(s.t, table, field), "the column's default: the payload's " + path + " is null"
	}
	want, err := field.kind.fromPayload(value)
	if err != nil {
		s.t.Fatalf("the payload's %s: %v", path, err)
	}
	return want, "the payload's " + path
}

func show(value any) string {
	raw, _ := json.Marshal(value)
	return abbreviate(string(raw))
}

// columnDefaults are the defaults of the shadowed tables' columns, each rendered as a value of the
// kind it is compared as, read once from the migrated database. A column with no default has null.
type columnDefaults struct {
	w      world
	values map[string]any
}

func (d *columnDefaults) of(t *testing.T, table string, field payloadField) any {
	t.Helper()
	key := table + "." + field.column
	if value, ok := d.values[key]; ok {
		return value
	}
	ctx := context.Background()
	var expression *string
	if err := d.w.database.Pool.QueryRow(ctx, `select column_default from information_schema.columns
		where table_schema = 'public' and table_name = $1 and column_name = $2`, table, field.column).Scan(&expression); err != nil {
		t.Fatalf("read the default of %s: %v", key, err)
	}
	var value any
	if expression != nil {
		var rendered string
		if err := d.w.database.Pool.QueryRow(ctx, "select "+field.kind.render(*expression)+"::text").Scan(&rendered); err != nil {
			t.Fatalf("evaluate the default of %s (%s): %v", key, *expression, err)
		}
		if err := json.Unmarshal([]byte(rendered), &value); err != nil {
			t.Fatalf("decode the default of %s: %v", key, err)
		}
	}
	d.values[key] = value
	return value
}

// otherThan is the first of values that is not value: what a seeded row holds where it must hold
// a word of a closed set other than the payload's.
func otherThan(values []string, value string) string {
	for _, candidate := range values {
		if candidate != value {
			return candidate
		}
	}
	return values[0]
}

// requireAbsent fails for each route and pointer that AbsentFields does not hold: the pointer names
// a field that the shape does not give, which a past-time answer of the route leaves out.
func (s *shapeSeed) requireAbsent(why string, routes [][2]string) {
	s.t.Helper()
	for _, route := range routes {
		if !asof.Absent(route[0], route[1]) {
			s.t.Errorf("%s, so %s leaves out %s at a cut; AbsentFields holds no such field for that route", why, route[0], route[1])
		}
	}
}

// ownerIssue is the key of the issue that owns the captured event; the test fails for an event
// that no issue owns.
func (s *shapeSeed) ownerIssue() string {
	s.t.Helper()
	if s.issue == nil {
		s.t.Fatalf("no issue owns the captured %s event", s.event.Type)
	}
	return *s.issue
}

// issueStateFields are the columns of issues that the shadow takes from the newest whole-issue
// payload at the cut (model.IssueEventPayload).
var issueStateFields = []payloadField{
	field("title", textValue, "title"),
	field("status", textValue, "status"),
	field("labels", textsValue, "labels"),
	field("parent_key", textValue, "parent"),
	field("route", textValue, "route"),
	field("closed_at", timeValue, "closed_at"),
	field("rank", textValue, "rank"),
	field("priority", scalarValue, "priority"),
	field("assignee", textValue, "assignee"),
	field("claimed_by", jsonbValue, "claim", "actor"),
	field("claimed_at", timeValue, "claim", "at"),
	field("repo", textValue, "repo"),
}

// readIssueState: an issue.created, issue.updated or issue.closed payload is the whole issue. The
// issues shadow takes the fields above from it, and updated_at, the later of the payload's and
// its event's time (the issue's one event at the cut). The issue's links, its external links and
// its own component attachment are its blocked_by, external_links and components. The projects
// shadow counts an issue.created after a cut by the payload's project, since the create numbered
// the issue from its project's next number.
func readIssueState(s *shapeSeed) {
	key := s.ownerIssue()
	s.appendCaptured()
	s.checkFields("issues", issueStateFields, "key = $1", key)

	updated := s.event.CreatedAt
	if raw, ok := s.payload["updated_at"].(string); ok {
		stated, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			s.t.Fatalf("the payload's updated_at: %v", err)
		}
		if stated.After(updated) {
			updated = stated
		}
	}
	if got, want := s.value(`select `+timeValue.render("updated_at")+`::text as v from issues where key = $1`, key),
		updated.UTC().Format(timeLayout); got != want {
		s.t.Errorf("issues.updated_at at the cut is %v, want %s, the later of the payload's updated_at and the event's time", got, want)
	}

	// Both sides sort by bytes: the database's collation could order the redacted text otherwise.
	var targets []string
	list, _ := s.payload["blocked_by"].([]any)
	for _, element := range list {
		if target, ok := element.(string); ok {
			targets = append(targets, target)
		}
	}
	slices.Sort(targets)
	blockers := []any{}
	for _, target := range targets {
		blockers = append(blockers, target)
	}
	if got := s.value(`select coalesce(jsonb_agg(target_key order by target_key collate "C"), '[]'::jsonb)::text as v
		from issue_links where issue_key = $1`, key); !reflect.DeepEqual(got, blockers) {
		s.t.Errorf("issue_links at the cut name %s, want the payload's blocked_by %s", show(got), show(blockers))
	}

	// A link stored with no kind is a url (api/issue_patch.go).
	var pairs [][2]string
	list, _ = s.payload["external_links"].([]any)
	for _, element := range list {
		link, _ := element.(map[string]any)
		url, ok := link["url"].(string)
		if !ok {
			continue
		}
		kind, _ := link["kind"].(string)
		if kind == "" {
			kind = "url"
		}
		pairs = append(pairs, [2]string{url, kind})
	}
	slices.SortFunc(pairs, func(a, b [2]string) int { return cmp.Or(strings.Compare(a[0], b[0]), strings.Compare(a[1], b[1])) })
	links := []any{}
	for _, pair := range pairs {
		links = append(links, []any{pair[0], pair[1]})
	}
	if got := s.value(`select coalesce(jsonb_agg(jsonb_build_array(url, kind) order by url collate "C", kind collate "C"), '[]'::jsonb)::text as v
		from issue_external_links where issue_key = $1`, key); !reflect.DeepEqual(got, links) {
		s.t.Errorf("issue_external_links at the cut are %s, want the payload's external_links %s", show(got), show(links))
	}

	attachment := []any{}
	if components, ok := s.payload["components"].(map[string]any); ok {
		mode, _ := components["mode"].(string)
		if (mode == "explicit" || mode == "none") && components["inherited_from"] == nil {
			attachment = append(attachment, []any{mode, components["reason"]})
		}
	}
	if got := s.value(`select coalesce(jsonb_agg(jsonb_build_array(mode, reason)), '[]'::jsonb)::text as v
		from issue_components where issue_key = $1`, key); !reflect.DeepEqual(got, attachment) {
		s.t.Errorf("issue_components at the cut are %s, want the issue's own attachment in the payload's components %s",
			show(got), show(attachment))
	}

	if s.event.Type == "issue.created" {
		var present int
		if err := s.tx.QueryRow(context.Background(), `select next_number from public.projects where key = 'ACME'`).Scan(&present); err != nil {
			s.t.Fatalf("read the project's next number: %v", err)
		}
		before := asof.CutAt(s.event.CreatedAt.Add(-time.Second))
		rows := s.readAt(before, `select next_number from projects where key = 'ACME'`)
		if len(rows) != 1 || rows[0]["next_number"] != int32(present-1) {
			s.t.Errorf("projects.next_number before the issue's creation is %v, want %d, one less than now, by the payload's project", rows, present-1)
		}
	}
}

// readIssueClaim: an issue.claimed or issue.released payload holds the issue's claim alone
// (model.IssueClaimEventPayload), and the shadow takes the claim from it when it is newer by seq
// than the newest whole-issue payload. The issue's creation before it holds another claim.
func readIssueClaim(s *shapeSeed) {
	key := s.ownerIssue()
	s.appendEvent(-1, "issue.created", map[string]any{
		"key": key, "title": "The present title", "status": "triage",
		"claim": map[string]any{"actor": json.RawMessage(presentActor), "at": s.event.CreatedAt.Add(-time.Second)},
	})
	s.appendCaptured()
	s.checkFields("issues", []payloadField{
		field("claimed_by", jsonbValue, "claim", "actor"),
		field("claimed_at", timeValue, "claim", "at"),
	}, "key = $1", key)
}

// insertAsk inserts the row of an open ask of kind, owned as the captured event is owned. Its
// question, options and approval are not the payload's.
func (s *shapeSeed) insertAsk(id, kind string) {
	s.t.Helper()
	var approval any
	if kind == "approval" {
		approval = presentApproval
	}
	s.exec(`insert into asks (id, issue_key, artifact_id, author, kind, question, options, approval, created_at)
		values ($1, $2, $3::uuid, $4, $5, 'The present question', '[]', $6, $7)`,
		id, s.issue, s.artifact, presentActor, kind, approval, s.event.CreatedAt.Add(-time.Hour))
}

// askStateFields are the columns of asks that the shadow takes from the newest ask.* payload at
// the cut (model.AskEventPayload). Where the payload carries no block_artifact, block_artifact_id
// is the row's, which no write changes after the insert; the seeded row holds none, the column's
// default.
var askStateFields = []payloadField{
	field("question", textValue, "question"),
	field("options", jsonbValue, "options"),
	field("multiple", scalarValue, "multiple"),
	field("urgency", textValue, "urgency"),
	field("anchor", jsonbValue, "anchor"),
	field("state", textValue, "state"),
	field("answer", jsonbValue, "answer"),
	field("resolution", jsonbValue, "resolution"),
	field("edited_at", timeValue, "edited_at"),
	field("approval", jsonbValue, "approval"),
	field("block_id", textValue, "block_id"),
	field("block_artifact_id", textValue, "block_artifact", "id"),
	field("asked_of", textValue, "asked_of"),
	field("asked_of_by", textValue, "asked_of_by"),
	field("asked_of_at", timeValue, "asked_of_at"),
}

// askRoutesApprovalDocuments are the routes that answer an ask's approval, each with the pointer of
// its documents.
var askRoutesApprovalDocuments = [][2]string{
	{"GET /api/v1/issues/{key}", "/open_asks/*/approval/documents"},
	{"GET /api/v1/issues/{key}/asks", "/*/approval/documents"},
	{"GET /api/v1/artifacts/{id}/asks", "/*/approval/documents"},
	{"GET /api/v1/asks/{id}", "/ask/approval/documents"},
	{"GET /api/v1/inbox", "/*/approval/documents"},
}

// readAskState: the payload of each event of an ask's state is the whole ask. An approval written
// before production's event 186,599 holds no documents, which a migration later filled in the row
// with no event (reason B). An ask.opened by a session makes the session a follower of the ask.
func readAskState(s *shapeSeed) {
	id := s.payloadString("id")
	s.insertAsk(id, s.payloadString("kind"))
	s.appendCaptured()
	s.checkFields("asks", askStateFields, "id = $1", id)
	if approval, ok := s.payload["approval"].(map[string]any); ok {
		if _, listed := approval["documents"]; !listed {
			s.requireAbsent("the payload's approval holds no documents", askRoutesApprovalDocuments)
		}
	}
	if s.event.Type == "ask.opened" {
		s.checkFollowers(id, s.authorFollows(), "the ask's opening by its author")
	}
}

// authorFollows is the follower that the payload's author makes of an ask in its thread: a
// session author follows from the event's time, and any other author follows not.
func (s *shapeSeed) authorFollows() []any {
	author, _ := s.payload["author"].(map[string]any)
	if id, _ := author["id"].(string); author["kind"] == "session" && id != "" {
		return []any{[]any{id, s.event.CreatedAt.UTC().Format(timeLayout)}}
	}
	return []any{}
}

// checkFollowers holds the followers of the ask at the cut, each [session_id, since], to want.
func (s *shapeSeed) checkFollowers(askID string, want []any, source string) {
	s.t.Helper()
	got := s.value(`select coalesce(jsonb_agg(jsonb_build_array(session_id, `+timeValue.render("since")+`) order by session_id),
		'[]'::jsonb)::text as v from ask_followers where ask_id = $1`, askID)
	if !reflect.DeepEqual(got, want) {
		s.t.Errorf("the ask's followers at the cut are %s, want %s, from %s", show(got), show(want), source)
	}
}

// readAskFollower: a follower event names the ask and the session (ask_id, session_id). A follow
// makes the session a follower from the event's time; an unfollow ends the follow before it.
func readAskFollower(s *shapeSeed) {
	askID, session := s.payloadString("ask_id"), s.payloadString("session_id")
	s.insertAsk(askID, "question")
	want := []any{[]any{session, s.event.CreatedAt.UTC().Format(timeLayout)}}
	if s.event.Type == "ask.follower_removed" {
		s.appendEvent(-1, "ask.follower_added", map[string]any{"ask_id": askID, "session_id": session})
		want = []any{}
	}
	s.appendCaptured()
	s.checkFollowers(askID, want, "the payload's session_id")
}

// commentStateFields are the columns of comments that the shadow takes from the newest comment.*
// payload at the cut (model.CommentEventPayload).
var commentStateFields = []payloadField{
	field("anchor", jsonbValue, "anchor"),
	field("resolved", scalarValue, "resolved"),
	field("suggestion", jsonbValue, "suggestion"),
	field("resolved_by", jsonbValue, "resolved_by"),
	field("resolved_at", timeValue, "resolved_at"),
	field("edited_at", timeValue, "edited_at"),
}

// commentBodyRoutes and messageBodyRoutes are the routes that answer a post's body, each with the
// pointer of the body.
var (
	commentBodyRoutes = [][2]string{
		{"GET /api/v1/issues/{key}/comments", "/*/body"},
		{"GET /api/v1/artifacts/{id}/comments", "/*/body"},
		{"GET /api/v1/comments/{id}", "/comment/body"},
	}
	messageBodyRoutes = [][2]string{
		{"GET /api/v1/issues/{key}/messages", "/*/message/body"},
		{"GET /api/v1/issues/{key}/messages/{id}", "/message/body"},
		{"GET /api/v1/messages/{id}", "/message/body"},
	}
)

// readCommentState: the payload of each event of a comment's state is the whole comment. A
// comment exists at the cut from its creating event (comment.created, comment.answered), which the
// seed writes before an event of any other type. A reply in an ask's thread by a session makes the
// session a follower of the ask. checkLegacyBody holds the comment's body at the cut.
func readCommentState(s *shapeSeed) {
	id := s.payloadString("id")
	askID := s.optionalString("ask_id")
	var thread *string
	if askID != "" {
		thread = &askID
		s.insertAsk(askID, "question")
	}
	s.exec(`insert into comments (id, issue_key, artifact_id, author, body, ask_id, created_at)
		values ($1, $2, $3::uuid, $4, 'The present body', $5, $6)`,
		id, s.issue, s.artifact, presentActor, thread, s.event.CreatedAt.Add(time.Millisecond))
	creates := s.event.Type == "comment.created" || s.event.Type == "comment.answered"
	if !creates {
		s.appendEvent(-1, "comment.created", map[string]any{"id": id})
	}
	s.appendCaptured()
	s.checkFields("comments", commentStateFields, "id = $1", id)
	if creates && thread != nil {
		s.checkFollowers(askID, s.authorFollows(), "the payload's author")
	}
	s.checkLegacyBody("comments", "comment.edited", id, commentBodyRoutes)
}

// checkLegacyBody: an edit after the cut that began before post_body_history did (migration
// 20261110224508) kept no row of the body that it replaced. So the shadow gives the body of the
// post's newest event at the cut, the captured event: its body, or none where the event cut the
// body (body_truncated), and then every route that answers the post leaves the body out (reason
// T).
func (s *shapeSeed) checkLegacyBody(table, editType, id string, routes [][2]string) {
	s.t.Helper()
	s.exec(`update schema_migrations set applied_at = $1 where version = 20261110224508`, s.event.CreatedAt.Add(time.Hour))
	s.appendEvent(2, editType, map[string]any{"id": id, "body": "An edit after the cut"})
	want := s.optionalString("body")
	if truncated, _ := s.payload["body_truncated"].(bool); truncated {
		want = ""
		s.requireAbsent("the payload cut the body", routes)
	}
	rows := s.read("select body from "+table+" where id = $1", id)
	if len(rows) != 1 {
		s.t.Fatalf("%s at the cut holds %d rows of the post, want one", table, len(rows))
	}
	if got := rows[0]["body"]; got != want {
		s.t.Errorf("%s.body at the cut, before an edit made before post_body_history began, is %q, want %q, from the payload's body and body_truncated",
			table, abbreviate(got.(string)), abbreviate(want))
	}
}

// insertMessage inserts the row of a message made at the time made, owned as the captured event is
// owned, with the payload's target. A message of no issue needs a target.
func (s *shapeSeed) insertMessage(id string, made time.Time) {
	s.t.Helper()
	var target *string
	if value := s.optionalString("target"); value != "" {
		target = &value
	}
	s.exec(`insert into messages (id, issue_key, author, body, target, created_at)
		values ($1, $2, $3, 'The present body', $4, $5)`, id, s.issue, presentActor, target, made)
}

// readMessageState: the payload of each event of a message's state is the whole message. The
// shadow reads only an issue message's events, since only an issue message is edited or deleted,
// so of a direct message it takes nothing from a payload.
func readMessageState(s *shapeSeed) {
	id := s.payloadString("id")
	s.insertMessage(id, s.event.CreatedAt)
	s.appendCaptured()
	s.checkFields("messages", []payloadField{field("edited_at", timeValue, "edited_at")}, "id = $1", id)
	if s.issue != nil {
		s.checkLegacyBody("messages", "message.edited", id, messageBodyRoutes)
	}
}

// The words of a delivery attempt's closed sets (comment_deliveries_state_check, its delivery
// check, and message_deliveries' two).
var (
	deliveryStates = []string{"pending", "sent", "queued", "failed"}
	deliveryModes  = []string{"btw", "aside", "steer"}
)

// readCommentDelivery: a comment.delivery receipt dates an attempt's state
// (model.CommentDeliveryEventPayload). The shadow takes the attempt's state and what the send
// settled with it from the newest receipt at the cut. The seeded attempt holds other values.
func readCommentDelivery(s *shapeSeed) {
	commentID, target := s.payloadString("comment_id"), s.payloadString("target")
	attempt, _ := s.payload["attempt"].(float64)
	duplicate, _ := s.payload["duplicate"].(bool)
	s.exec(`insert into comments (id, issue_key, artifact_id, author, body, created_at)
		values ($1, $2, $3::uuid, $4, 'The present body', $5)`,
		commentID, s.issue, s.artifact, presentActor, s.event.CreatedAt.Add(-time.Minute))
	s.exec(`insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, envelope_id, state, error, duplicate, created_at)
		values ($1, $2, $3, $4, 'present-session', 'present-envelope', $5, 'The present error', $6, $7)`,
		commentID, target, int(attempt), otherThan(deliveryModes, s.optionalString("delivery")),
		otherThan(deliveryStates, s.optionalString("state")), !duplicate, s.event.CreatedAt.Add(-time.Second))
	s.appendCaptured()
	s.checkFields("comment_deliveries", []payloadField{
		field("delivery", textValue, "delivery"),
		field("session_id", textValue, "session_id"),
		field("state", textValue, "state"),
		field("error", nonEmptyTextValue, "error"),
		field("reply_id", textValue, "reply_id"),
		field("duplicate", scalarValue, "duplicate"),
	}, "comment_id = $1 and target = $2 and attempt = $3", commentID, target, int(attempt))
}

// readMessageDelivery: a message.delivery receipt dates an attempt's state
// (model.MessageDeliveryEventPayload), as readCommentDelivery says. A direct message's receipt is
// an event of no owner.
func readMessageDelivery(s *shapeSeed) {
	messageID := s.payloadString("message_id")
	attempt, _ := s.payload["attempt"].(float64)
	duplicate, _ := s.payload["duplicate"].(bool)
	s.insertMessage(messageID, s.event.CreatedAt.Add(-time.Minute))
	s.exec(`insert into message_deliveries (message_id, attempt, delivery, session_id, envelope_id, state, error, duplicate, created_at)
		values ($1, $2, $3, 'present-session', 'present-envelope', $4, 'The present error', $5, $6)`,
		messageID, int(attempt), otherThan(deliveryModes, s.optionalString("delivery")),
		otherThan(deliveryStates, s.optionalString("state")), !duplicate, s.event.CreatedAt.Add(-time.Second))
	s.appendCaptured()
	s.checkFields("message_deliveries", []payloadField{
		field("delivery", textValue, "delivery"),
		field("session_id", textValue, "session_id"),
		field("state", textValue, "state"),
		field("error", nonEmptyTextValue, "error"),
		field("duplicate", scalarValue, "duplicate"),
	}, "message_id = $1 and attempt = $2", messageID, int(attempt))
}

// readProjectSteps: a project.created or project.updated payload is the project
// (api/project_gated_steps.go), and the shadow takes its gated steps from the newest at the cut.
func readProjectSteps(s *shapeSeed) {
	s.appendCaptured()
	s.checkFields("projects", []payloadField{field("gated_steps", textsValue, "gated_steps")}, "key = $1", "ACME")
}

// readProjectDefault: a settings.project_default_assignee.updated payload names the project's
// default assignee, which the shadow takes from the newest at the cut.
func readProjectDefault(s *shapeSeed) {
	s.appendCaptured()
	s.checkFields("projects", []payloadField{field("default_assignee", textValue, "assignee")}, "key = $1", "ACME")
}

// readRepoProject: a settings.repo_project.updated payload is a repository's mapping
// ({"mapping": model.RepoProject, "deleted"}), which the shadow takes from the newest of the
// repository's at the cut. A deleted mapping is no row.
func readRepoProject(s *shapeSeed) {
	s.appendCaptured()
	mapping, _ := s.payload["mapping"].(map[string]any)
	repo, _ := mapping["repo"].(string)
	if deleted, _ := s.payload["deleted"].(bool); deleted {
		if rows := s.read(`select repo from repo_projects where repo = $1`, repo); len(rows) != 0 {
			s.t.Errorf("repo_projects at the cut holds the deleted mapping of the repository: %v", rows)
		}
		return
	}
	s.checkFields("repo_projects", []payloadField{
		field("project", textValue, "mapping", "project"),
		field("created_by", jsonbValue, "mapping", "created_by"),
		field("created_at", timeValue, "mapping", "created_at"),
		field("is_default", scalarValue, "mapping", "default"),
		field("renamed_to", textValue, "mapping", "renamed_to"),
	}, "repo = $1", repo)
}

package going

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A block is drawn beside the answer when the typed words name its kind (R2): the words decide,
// and no confidence does. The model writes the block's spec - the
// read that fills it, that read's arguments, the table and fields it draws, never a number - and
// the server runs that read again as the run's author and fills the block from its answer alone,
// so every number a block shows is a value the read returned (R2, R9).

// The seven kinds of block, the closed list of R2, and BlockNone, the kind a structured reply
// that draws no block names.
const (
	BlockChart       = "chart"
	BlockProgress    = "progress"
	BlockTable       = "table"
	BlockIssueList   = "issue_list"
	BlockAgentStatus = "agent_status"
	BlockMetric      = "metric"
	BlockTimeline    = "timeline"
	BlockNone        = "none"
)

// BlockKinds is every kind, in the order a call is told them.
var BlockKinds = []string{BlockChart, BlockProgress, BlockTable, BlockIssueList, BlockAgentStatus, BlockMetric, BlockTimeline}

// Chart marks.
const (
	MarkBar  = "bar"
	MarkLine = "line"
)

// The bounds of what one block carries, so a block stays a block and not a dump of a read: a
// chart's points, a table's and a list's rows, a progress block's bars and a timeline's events.
const (
	chartPointMax    = 60
	tableRowMax      = 50
	progressRowMax   = 20
	timelineEventMax = 100
	// chartPointMin is the fewest points a chart plots: one point is no series (R9).
	chartPointMin = 2
)

// The tables a kind with a fixed table reads, which the server's reads write their rows under
// (internal/dispatch/api's going reads), so a read and the block it fills name one table.
const (
	TableIssues   = "issues"
	TableSessions = "sessions"
	TableProgress = "progress"
	TableEvents   = "events"
)

// The fields of those tables' rows a block draws: an issue list's issues, an agent status's
// sessions, a timeline's events and a progress block's bars by default. A read writes each of its
// rows' fields under these names.
const (
	FieldKey          = "key"
	FieldTitle        = "title"
	FieldStatus       = "status"
	FieldSessionID    = "session_id"
	FieldState        = "state"
	FieldOpenAsks     = "open_asks"
	FieldLastActivity = "last_activity"
	FieldAt           = "at"
	FieldLane         = "lane"
	FieldLabel        = "label"
	FieldDone         = "done"
	FieldTotal        = "total"
)

var (
	issueListFields   = []string{FieldKey, FieldTitle, FieldStatus}
	agentStatusFields = []string{FieldSessionID, FieldTitle, FieldState, FieldOpenAsks, FieldLastActivity}
	timelineFields    = []string{FieldAt, FieldLane, FieldLabel}
)

// blockSources is the reads that fill each kind. A kind with none of its reads among the run's
// is not offered, and a spec naming a read outside its kind's is dropped.
var blockSources = map[string][]string{
	BlockChart:       {ReadProgress, ReadArchitecture, ReadProjectActivity, ReadDelivery, ReadRoster},
	BlockTable:       {ReadProgress, ReadArchitecture, ReadProjectActivity, ReadDelivery, ReadRoster},
	BlockMetric:      {ReadProgress, ReadArchitecture, ReadProjectActivity, ReadDelivery, ReadRoster},
	BlockProgress:    {ReadProgress, ReadArchitecture},
	BlockIssueList:   {ReadProgress},
	BlockAgentStatus: {ReadRoster},
	BlockTimeline:    {ReadDelivery},
}

// blockLexicon is the words that name each kind, matched as whole words in the
// typed words lowercased. "bar" and "line" name a chart only beside chart, graph or plot, since
// alone they are words of every other kind of question ("the command bar", "in line with").
var blockLexicon = []struct {
	kind  string
	words *regexp.Regexp
}{
	{BlockChart, regexp.MustCompile(`\b(charts?|charted|graphs?|graphed|plots?|plotted|(bar|line) (charts?|graphs?|plots?))\b`)},
	{BlockTable, regexp.MustCompile(`\b(tables?|tabulated?|tabular)\b`)},
	{BlockProgress, regexp.MustCompile(`\b(progress|how far along)\b`)},
	{BlockIssueList, regexp.MustCompile(`\b(issue list|list of (the |all )?(open |closed )?issues|list (the |all )?(open |closed )?issues)\b`)},
	{BlockAgentStatus, regexp.MustCompile(`\b(agent status|agents status|agents' status|status of (the |all )?agents|who is working|who's working|whos working|who are working)\b`)},
	{BlockMetric, regexp.MustCompile(`\b(metrics?|numbers?|count|how many)\b`)},
	{BlockTimeline, regexp.MustCompile(`\b(timelines?)\b`)},
}

// KindsNamed is every kind the typed words name, in BlockKinds' order.
func KindsNamed(words string) []string {
	text := strings.ToLower(strings.ReplaceAll(words, "\u2019", "'"))
	var named []string
	for _, entry := range blockLexicon {
		if entry.words.MatchString(text) {
			named = append(named, entry.kind)
		}
	}
	return named
}

// Drawn reports whether a block of kind may be drawn beside an answer to words: the words name
// the kind. A block the model writes for any other question is dropped.
func Drawn(kind, words string) bool {
	return slices.Contains(KindsNamed(words), kind)
}

// OfferedKinds is the kinds an answer to words may draw, given the reads the run has: each kind
// the words name that one of the run's reads fills.
func OfferedKinds(words string, reads map[string]ReadFunc) []string {
	var offered []string
	for _, kind := range KindsNamed(words) {
		if slices.ContainsFunc(blockSources[kind], func(source string) bool { _, held := reads[source]; return held }) {
			offered = append(offered, kind)
		}
	}
	return offered
}

// blockGuides is what a call is told of each kind's spec: the JSON object block.spec holds.
var blockGuides = map[string]string{
	BlockChart:       `{"source": a read tool, "params": its arguments, "table": a table of that read, "x": the field along the axis (a week, a status, a component, a day), "y": a numeric field, "mark": "bar" or "line"}: one series of at least two points.`,
	BlockTable:       `{"source": a read tool, "params": its arguments, "table": a table of that read, "columns": [the fields shown, in order]}.`,
	BlockMetric:      `{"source": a read tool, "params": its arguments, "table": a table of that read holding one row, "field": its numeric field, "label": what the number counts, "compare": another numeric field of the row or "", "compare_label": what that one counts or ""}.`,
	BlockProgress:    `{"source": "read_progress" or "read_architecture", "params": its arguments, "table": "progress" for read_progress or "components" for read_architecture, "label": the field naming each bar ("label" or "title"), "done": the done field, "total": the total field}.`,
	BlockIssueList:   `{"source": "read_progress", "params": its arguments}: the read's issues table, each issue's key, title and status.`,
	BlockAgentStatus: `{"source": "read_roster", "params": {}}: every session's title, state, open asks and last activity.`,
	BlockTimeline:    `{"source": "read_delivery", "params": {"view": "timeline", ...}}: the window's events, each dated in its lane.`,
}

// BlockGuide is what the structured call is told of the blocks it may write: none, or each kind
// offered with its spec. A number is never part of a spec: the server fills the block from the
// read the spec names.
func BlockGuide(offered []string) string {
	var out strings.Builder
	if len(offered) == 0 {
		out.WriteString("\n\nWrite block kind none: these words ask for no block.\n")
		return out.String()
	}
	out.WriteString("\n\nYou may write one block beside the answer, of a kind the words name. Write its spec as the JSON text of an object naming the read that fills it, ")
	out.WriteString("exactly as you would call that tool, and the table and fields it is drawn from; never write a number in a spec, since every number is the read's. ")
	out.WriteString("Write block kind none when no read you made answers it.\n")
	for _, kind := range offered {
		out.WriteString("\n--- block\nkind: ")
		out.WriteString(kind)
		out.WriteString("\nspec: ")
		out.WriteString(blockGuides[kind])
		out.WriteString("\n")
	}
	return out.String()
}

// ErrNoBlock is an answer that drew no block: kind none, or no kind at all.
var ErrNoBlock = errors.New("going: the answer drew no block")

// Block is a block spec the server parsed against its kind's schema: what it is drawn as, and
// the read that fills it.
type Block struct {
	Kind   string
	Title  string
	Source string
	Params json.RawMessage
	spec   blockSpec
}

// blockSpec is the part of every kind's spec that names its read.
type blockSpec struct {
	Source string          `json:"source"`
	Params json.RawMessage `json:"params"`
	Table  string          `json:"table,omitempty"`

	// chart
	X    string `json:"x,omitempty"`
	Y    string `json:"y,omitempty"`
	Mark string `json:"mark,omitempty"`
	// table
	Columns []string `json:"columns,omitempty"`
	// metric
	Field        string `json:"field,omitempty"`
	Label        string `json:"label,omitempty"`
	Compare      string `json:"compare,omitempty"`
	CompareLabel string `json:"compare_label,omitempty"`
	// progress
	Done  string `json:"done,omitempty"`
	Total string `json:"total,omitempty"`
}

// The fields each kind's spec takes, beside source and params: a spec naming any other is no
// spec of that kind and is dropped, which is what keeps a number the model wrote out of a block.
var blockSpecFields = map[string][]string{
	BlockChart:       {"table", "x", "y", "mark"},
	BlockTable:       {"table", "columns"},
	BlockMetric:      {"table", "field", "label", "compare", "compare_label"},
	BlockProgress:    {"table", "label", "done", "total"},
	BlockIssueList:   {},
	BlockAgentStatus: {},
	BlockTimeline:    {},
}

// ParseBlock holds the block the answer wrote to its kind's schema: a kind of the seven, a spec
// that is one JSON object naming only that kind's fields, a source among the kind's reads, the
// fields it draws named, and nothing written where a field holds a name. ErrNoBlock is kind none;
// any other error is a block that is dropped.
func ParseBlock(final FinalBlock) (Block, error) {
	kind := strings.TrimSpace(final.Kind)
	if kind == "" || kind == BlockNone {
		return Block{}, ErrNoBlock
	}
	takes, known := blockSpecFields[kind]
	if !known {
		return Block{}, fmt.Errorf("going: %q is no kind of block", kind)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(final.Spec), &fields); err != nil || fields == nil {
		return Block{}, fmt.Errorf("going: the %s block's spec is no JSON object", kind)
	}
	for name := range fields {
		if name != "source" && name != "params" && !slices.Contains(takes, name) {
			return Block{}, fmt.Errorf("going: a %s block's spec takes no %q", kind, name)
		}
	}
	var spec blockSpec
	if err := json.Unmarshal([]byte(final.Spec), &spec); err != nil {
		return Block{}, fmt.Errorf("going: the %s block's spec does not fit its schema: %w", kind, err)
	}
	if !slices.Contains(blockSources[kind], spec.Source) {
		return Block{}, fmt.Errorf("going: a %s block is filled by %s, not by %q", kind, strings.Join(blockSources[kind], ", "), spec.Source)
	}
	params := bytes.TrimSpace(spec.Params)
	if len(params) == 0 || bytes.Equal(params, []byte("null")) {
		params = []byte(`{}`)
	}
	if params[0] != '{' {
		return Block{}, fmt.Errorf("going: the %s block's params is no JSON object", kind)
	}
	spec.Params = params
	if err := spec.settle(kind); err != nil {
		return Block{}, err
	}
	return Block{Kind: kind, Title: strings.TrimSpace(final.Title), Source: spec.Source, Params: spec.Params, spec: spec}, nil
}

// settle checks the fields kind draws are named, and fills the ones a kind fixes or defaults.
func (s *blockSpec) settle(kind string) error {
	required := func(names ...string) error {
		values := map[string]string{"table": s.Table, "x": s.X, "y": s.Y, "field": s.Field, "label": s.Label, "done": s.Done, "total": s.Total}
		for _, name := range names {
			if strings.TrimSpace(values[name]) == "" {
				return fmt.Errorf("going: a %s block's spec names no %s", kind, name)
			}
		}
		return nil
	}
	switch kind {
	case BlockChart:
		s.Mark = cmp.Or(strings.TrimSpace(s.Mark), MarkBar)
		if s.Mark != MarkBar && s.Mark != MarkLine {
			return fmt.Errorf("going: a chart is drawn as %s or %s, not %q", MarkBar, MarkLine, s.Mark)
		}
		return required("table", "x", "y")
	case BlockTable:
		if len(s.Columns) == 0 || slices.Contains(s.Columns, "") {
			return fmt.Errorf("going: a table's spec names no columns")
		}
		return required("table")
	case BlockMetric:
		s.Label = cmp.Or(strings.TrimSpace(s.Label), strings.TrimSpace(s.Field))
		if s.Compare != "" {
			s.CompareLabel = cmp.Or(strings.TrimSpace(s.CompareLabel), strings.TrimSpace(s.Compare))
		} else {
			s.CompareLabel = ""
		}
		return required("table", "field")
	case BlockProgress:
		s.Table = cmp.Or(strings.TrimSpace(s.Table), TableProgress)
		s.Label = cmp.Or(strings.TrimSpace(s.Label), FieldLabel)
		s.Done = cmp.Or(strings.TrimSpace(s.Done), FieldDone)
		s.Total = cmp.Or(strings.TrimSpace(s.Total), FieldTotal)
	case BlockIssueList:
		s.Table = TableIssues
	case BlockAgentStatus:
		s.Table = TableSessions
	case BlockTimeline:
		s.Table = TableEvents
	}
	return nil
}

// BlockData is the block frame's data (contracts GoingBlockEvent): the kind, the title, the spec
// as the server settled it - the read, the table and the fields drawn, with every default filled
// - and the rows of the read's table that the block shows, each holding only the fields drawn.
type BlockData struct {
	Kind  string           `json:"kind"`
	Title string           `json:"title"`
	Spec  json.RawMessage  `json:"spec"`
	Rows  []map[string]any `json:"rows"`
}

// Fill builds the block from the read its spec names, and from nothing else: the rows are the
// read's table's own, cut to the fields the block draws. A block the read cannot fill is not
// drawn - a table or field the read does not hold, a value that is no number where a number is
// drawn, a chart of fewer than two points (R9), a metric whose table is not one row - and the
// error says which.
func (b Block) Fill(read Read) (BlockData, error) {
	rows, err := normalizedTable(read, b.spec.Table)
	if err != nil {
		return BlockData{}, err
	}
	var drawn []map[string]any
	switch b.Kind {
	case BlockChart:
		drawn, err = fillChart(b.spec, rows)
	case BlockTable:
		drawn, err = project(rows, b.spec.Columns, tableRowMax, nil)
	case BlockMetric:
		drawn, err = fillMetric(b.spec, rows)
	case BlockProgress:
		drawn, err = fillProgress(b.spec, rows)
	case BlockIssueList:
		drawn, err = project(rows, issueListFields, tableRowMax, nil)
	case BlockAgentStatus:
		drawn, err = project(rows, agentStatusFields, tableRowMax, nil)
	case BlockTimeline:
		drawn, err = fillTimeline(rows)
	default:
		err = fmt.Errorf("going: %q is no kind of block", b.Kind)
	}
	if err != nil {
		return BlockData{}, err
	}
	if len(drawn) == 0 {
		return BlockData{}, fmt.Errorf("going: the %s block's read holds no row to draw", b.Kind)
	}
	spec, err := json.Marshal(b.spec)
	if err != nil {
		return BlockData{}, err
	}
	return BlockData{Kind: b.Kind, Title: b.Title, Spec: spec, Rows: drawn}, nil
}

// normalizedTable is the named table of read with every value as JSON reads it back, numbers as
// json.Number, so a field is a number exactly when the read wrote one, whatever Go type it held.
func normalizedTable(read Read, table string) ([]map[string]any, error) {
	rows, held := read.Tables[table]
	if !held {
		return nil, fmt.Errorf("going: the read holds no table %q", table)
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var normalized []map[string]any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

// project is rows cut to fields, at most max of them. A field no row holds is a field the read
// does not have, and the block is not drawn; numeric names fields that must be numbers in every
// row kept, and a row whose value is not one is left out.
func project(rows []map[string]any, fields []string, max int, numeric []string) ([]map[string]any, error) {
	for _, field := range fields {
		if !slices.ContainsFunc(rows, func(row map[string]any) bool { _, held := row[field]; return held }) && len(rows) > 0 {
			return nil, fmt.Errorf("going: the read's rows hold no field %q", field)
		}
	}
	drawn := make([]map[string]any, 0, min(len(rows), max))
	for _, row := range rows {
		if len(drawn) == max {
			break
		}
		if slices.ContainsFunc(numeric, func(field string) bool { _, ok := row[field].(json.Number); return !ok }) {
			continue
		}
		kept := make(map[string]any, len(fields))
		for _, field := range fields {
			kept[field] = row[field]
		}
		drawn = append(drawn, kept)
	}
	return drawn, nil
}

// fillChart is one series: x along the axis, y a number, at least chartPointMin points.
func fillChart(spec blockSpec, rows []map[string]any) ([]map[string]any, error) {
	drawn, err := project(rows, []string{spec.X, spec.Y}, chartPointMax, []string{spec.Y})
	if err != nil {
		return nil, err
	}
	drawn = slices.DeleteFunc(drawn, func(row map[string]any) bool {
		switch row[spec.X].(type) {
		case string, json.Number:
			return false
		}
		return true
	})
	if len(drawn) < chartPointMin {
		return nil, fmt.Errorf("going: the chart's read holds %d point(s), and a chart plots at least %d", len(drawn), chartPointMin)
	}
	return drawn, nil
}

// fillMetric is one number with its label and at most one comparison, read from a table of one
// row: a table of more is no one number.
func fillMetric(spec blockSpec, rows []map[string]any) ([]map[string]any, error) {
	if len(rows) != 1 {
		return nil, fmt.Errorf("going: a metric reads a table of one row, and %q holds %d", spec.Table, len(rows))
	}
	fields := []string{spec.Field}
	numeric := []string{spec.Field}
	if spec.Compare != "" {
		fields = append(fields, spec.Compare)
		numeric = append(numeric, spec.Compare)
	}
	drawn, err := project(rows, fields, 1, numeric)
	if err != nil {
		return nil, err
	}
	if len(drawn) == 0 {
		return nil, fmt.Errorf("going: the metric's field is no number in its read")
	}
	return drawn, nil
}

// fillProgress is one bar per row: a label, and done of total, total above zero.
func fillProgress(spec blockSpec, rows []map[string]any) ([]map[string]any, error) {
	drawn, err := project(rows, []string{spec.Label, spec.Done, spec.Total}, progressRowMax, []string{spec.Done, spec.Total})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(drawn, func(row map[string]any) bool {
		total, err := row[spec.Total].(json.Number).Float64()
		return err != nil || total <= 0
	}), nil
}

// fillTimeline is the read's dated events in their lanes, each with a time a reader can place.
func fillTimeline(rows []map[string]any) ([]map[string]any, error) {
	drawn, err := project(rows, timelineFields, len(rows), nil)
	if err != nil {
		return nil, err
	}
	drawn = slices.DeleteFunc(drawn, func(row map[string]any) bool {
		at, isText := row[FieldAt].(string)
		_, lane := row[FieldLane].(string)
		if !isText || !lane {
			return true
		}
		_, err := time.Parse(time.RFC3339, at)
		return err != nil
	})
	if len(drawn) > timelineEventMax {
		drawn = drawn[len(drawn)-timelineEventMax:]
	}
	return drawn, nil
}

// RunBlockKinds are the kinds a block of a run is drawn as: a chart, a table, or one number.
var RunBlockKinds = []string{BlockChart, BlockTable, BlockMetric}

// RunBlock is a block of a run (settlement 9 of the plan): its kind and title, the saved tool
// results of the run it reads, each by its step and its call's number within the step, the chain
// the server runs over their rows (transform.go), and the columns of the chain's answer it draws
// - a chart's x, y and mark, a table's columns, a metric's field and label and the field it is
// compared with. The model writes it as the arguments of draw_block, and the server draws it from
// the run's saved results alone (Draw).
type RunBlock struct {
	Kind         string    `json:"kind"`
	Title        string    `json:"title"`
	Steps        []StepRef `json:"steps"`
	Chain        Chain     `json:"chain"`
	X            string    `json:"x,omitempty"`
	Y            string    `json:"y,omitempty"`
	Mark         string    `json:"mark,omitempty"`
	Columns      []string  `json:"columns,omitempty"`
	Field        string    `json:"field,omitempty"`
	Label        string    `json:"label,omitempty"`
	Compare      string    `json:"compare,omitempty"`
	CompareLabel string    `json:"compare_label,omitempty"`
}

// StepRef names one saved tool result of a run: its step, its call's number within the step, and
// where its rows are in its answer, empty for the answer itself where it is a list, or its one
// field that holds a list.
type StepRef struct {
	Step int    `json:"step"`
	Call int    `json:"call"`
	Rows string `json:"rows,omitempty"`
}

// standaloneNumber matches a number written on its own in text a block draws, such as "42" or
// "3.5%": a number of the model's, which a title or a label may not carry. A digit inside a word,
// as in P0 or Q3, is part of a name.
var standaloneNumber = regexp.MustCompile(`(^|[^\p{L}\p{N}_])[+-]?\p{Nd}[\p{Nd},.]*%?($|[^\p{L}\p{N}_])`)

// ParseRunBlock holds the spec a run's model wrote to RunBlock's schema. A spec that holds a
// number is refused - a JSON number anywhere but a step and a call naming a saved result (numberIn),
// or a number written on its own in the title or a label - so every number the block shows is the
// server's. So is a spec of a kind no block of a run is, one naming a field RunBlock has not, which
// is told every argument draw_block takes (runBlockArguments), one that names no step or the same
// step twice, one whose chain refuses a step, and one that names no column to draw.
func ParseRunBlock(spec json.RawMessage) (RunBlock, error) {
	decoder := json.NewDecoder(bytes.NewReader(spec))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return RunBlock{}, errors.New("going: a block spec is one JSON object")
	}
	if at, number, held := numberIn(fields); held {
		return RunBlock{}, fmt.Errorf("going: a block spec holds no number, and its %s holds %s: every number a block shows is one the server's chain makes from the run's saved results, so name the steps and the chain that make it", at, number)
	}
	var block RunBlock
	strict := json.NewDecoder(bytes.NewReader(spec))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&block); err != nil {
		return RunBlock{}, fmt.Errorf("going: the block spec does not fit draw_block's arguments: %v. %s", err, runBlockArguments)
	}
	block.Title = strings.TrimSpace(block.Title)
	for _, drawn := range []struct{ name, text string }{{"title", block.Title}, {"label", block.Label}, {"compare_label", block.CompareLabel}} {
		if number := standaloneNumber.FindString(drawn.text); number != "" {
			return RunBlock{}, fmt.Errorf("going: a block spec holds no number, and its %s writes %q: every number a block shows is the server's, so write the %s without it", drawn.name, strings.TrimSpace(number), drawn.name)
		}
	}
	if !slices.Contains(RunBlockKinds, block.Kind) {
		return RunBlock{}, fmt.Errorf("going: a block of a run is a %s, not %q", strings.Join(RunBlockKinds, ", a "), block.Kind)
	}
	if len(block.Steps) == 0 {
		return RunBlock{}, errors.New("going: a block spec names the steps whose saved results it reads, and this one names none")
	}
	for index := range block.Steps {
		ref := &block.Steps[index]
		ref.Call = cmp.Or(ref.Call, 1)
		if ref.Step < 1 || ref.Call < 1 {
			return RunBlock{}, fmt.Errorf("going: step %d, call %d is no step of a run: each counts from 1", ref.Step, ref.Call)
		}
		if slices.ContainsFunc(block.Steps[:index], func(other StepRef) bool { return other.Step == ref.Step && other.Call == ref.Call }) {
			return RunBlock{}, fmt.Errorf("going: the block spec names step %d, call %d twice, which would read its rows twice", ref.Step, ref.Call)
		}
	}
	if err := block.Chain.check(); err != nil {
		return RunBlock{}, err
	}
	// The chain's answer is the table a block of a run draws, so the spec settle checks names it;
	// settle fills the chart's mark and the metric's labels as it does for an answer's block.
	settled := blockSpec{Table: "the chain's answer", X: block.X, Y: block.Y, Mark: block.Mark, Columns: block.Columns,
		Field: block.Field, Label: block.Label, Compare: block.Compare, CompareLabel: block.CompareLabel}
	if err := settled.settle(block.Kind); err != nil {
		return RunBlock{}, err
	}
	block.Mark, block.Label, block.CompareLabel = settled.Mark, settled.Label, settled.CompareLabel
	return block, nil
}

// runBlockArguments is what a spec that does not fit RunBlock's shape is told of it: each of
// draw_block's arguments, so the model can write the spec again in one call.
const runBlockArguments = "draw_block takes kind (chart, table or metric), title, steps - each {step, call, rows} naming a saved tool result of this run - " +
	"and chain - each {op, ...} of filter (field and one of is, is_not, in, has, since, before or present), group (field, per, as), count (as), sum, mean, min and max (field, as) and sort (field, order) - " +
	"then x, y and mark for a chart, columns for a table, or field, label, compare and compare_label for a metric"

// numberIn is the first JSON number of a spec, in its fields' order, with where it is, outside
// the places a spec writes one: a step's step and call. A step or a call under any other name is
// still a result the model names, not a number it writes, so it is left to the strict decode,
// which names the field it is under (a spec whose steps are under data is refused for data).
func numberIn(spec map[string]any) (string, json.Number, bool) {
	var walk func(value any, at string) (string, json.Number, bool)
	walk = func(value any, at string) (string, json.Number, bool) {
		switch value := value.(type) {
		case json.Number:
			return at, value, true
		case []any:
			for index, element := range value {
				if where, number, held := walk(element, fmt.Sprintf("%s[%d]", at, index)); held {
					return where, number, true
				}
			}
		case map[string]any:
			for _, name := range slices.Sorted(maps.Keys(value)) {
				if _, isNumber := value[name].(json.Number); isNumber && (name == "step" || name == "call") {
					continue
				}
				if where, number, held := walk(value[name], at+"."+name); held {
					return where, number, true
				}
			}
		}
		return "", "", false
	}
	for _, name := range slices.Sorted(maps.Keys(spec)) {
		if _, isNumber := spec[name].(json.Number); isNumber && (name == "step" || name == "call") {
			continue
		}
		if where, number, held := walk(spec[name], name); held {
			return where, number, true
		}
	}
	return "", "", false
}

// StepNumbers is the number of each step the block reads.
func (b RunBlock) StepNumbers() []int {
	steps := make([]int, 0, len(b.Steps))
	for _, ref := range b.Steps {
		steps = append(steps, ref.Step)
	}
	return steps
}

// Draw draws the block from results, the run's saved tool results, and from nothing else: the
// rows of each step it names, refused unless they are the whole of what their reads list
// (readPage, wholeRows), through its chain, cut to the columns it draws. Every number the block
// holds is one a result held or a step of the chain made. An error is a refusal the model can act
// on: it names what to read, or what to write instead; where the data was not whole it says so.
func (b RunBlock) Draw(results []Result) (BlockData, error) {
	pages := make([]resultPage, 0, len(b.Steps))
	for _, ref := range b.Steps {
		at := slices.IndexFunc(results, func(result Result) bool { return result.Step == ref.Step && result.Call == ref.Call })
		if at < 0 {
			return BlockData{}, fmt.Errorf("going: the run saved no tool result at step %d, call %d: name each result by the step and call it names", ref.Step, ref.Call)
		}
		page, err := readPage(ref, results[at])
		if err != nil {
			return BlockData{}, err
		}
		pages = append(pages, page)
	}
	rows, err := wholeRows(pages)
	if err != nil {
		return BlockData{}, err
	}
	answer, err := b.Chain.Run(rows)
	if err != nil {
		return BlockData{}, err
	}
	var drawn []map[string]any
	switch b.Kind {
	case BlockChart:
		drawn, err = fillChart(blockSpec{X: b.X, Y: b.Y}, flattened(answer, b.X, b.Y))
	case BlockTable:
		drawn, err = project(flattened(answer, b.Columns...), b.Columns, tableRowMax, nil)
	case BlockMetric:
		drawn, err = fillMetric(blockSpec{Table: "the chain's answer", Field: b.Field, Compare: b.Compare}, flattened(answer, b.Field, b.Compare))
	}
	if err != nil {
		return BlockData{}, err
	}
	if len(drawn) == 0 {
		return BlockData{}, fmt.Errorf("going: the %s block's chain answers no row to draw", b.Kind)
	}
	spec, err := json.Marshal(b)
	if err != nil {
		return BlockData{}, err
	}
	return BlockData{Kind: b.Kind, Title: b.Title, Spec: spec, Rows: drawn}, nil
}

// flattened is rows with each of columns that names a nested field (a.b) set to that field's
// value, so a block draws it as one column; rows themselves are left as they are.
func flattened(rows []map[string]any, columns ...string) []map[string]any {
	nested := slices.DeleteFunc(slices.Clone(columns), func(column string) bool { return !strings.Contains(column, ".") })
	if len(nested) == 0 {
		return rows
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		copied := maps.Clone(row)
		for _, column := range nested {
			if value, held := fieldValue(row, column); held {
				copied[column] = value
			}
		}
		out = append(out, copied)
	}
	return out
}

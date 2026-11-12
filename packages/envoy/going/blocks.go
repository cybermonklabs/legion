package going

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A block is drawn beside the answer when the typed words name its kind, or, for a chart alone,
// when the decision's display choice is a chart at or above the chart-display threshold (R2,
// decision 17): the words decide, and no confidence does. The model writes the block's spec - the
// read that fills it, that read's arguments, the table and fields it draws, never a number - and
// the server runs that read again as the run's author and fills the block from its answer alone,
// so every number a block shows is a value the read returned (R2, R9).

// The seven kinds of block, the closed list of R2.
const (
	BlockChart       = "chart"
	BlockProgress    = "progress"
	BlockTable       = "table"
	BlockIssueList   = "issue_list"
	BlockAgentStatus = "agent_status"
	BlockMetric      = "metric"
	BlockTimeline    = "timeline"
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

// The tables a kind with a fixed table reads, and the fields its rows are drawn with.
const (
	tableIssues   = "issues"
	tableSessions = "sessions"
	tableProgress = "progress"
	tableEvents   = "events"
)

var (
	issueListFields   = []string{"key", "title", "status"}
	agentStatusFields = []string{"session_id", "title", "state", "open_asks", "last_activity"}
	timelineFields    = []string{"at", "lane", "label"}
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

// blockLexicon is the words that name each kind (decision 17), matched as whole words in the
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
// the kind, or it is a chart and chart says the decision settled on drawing one unasked. A block
// the model writes for any other question is dropped.
func Drawn(kind, words string, chart bool) bool {
	if kind == BlockChart && chart {
		return true
	}
	return slices.Contains(KindsNamed(words), kind)
}

// OfferedKinds is the kinds an answer to words may draw, given whether the decision settled on a
// chart and the reads the run has: each kind Drawn allows that one of the run's reads fills.
func OfferedKinds(words string, chart bool, reads map[string]ReadFunc) []string {
	var offered []string
	for _, kind := range BlockKinds {
		if !Drawn(kind, words, chart) {
			continue
		}
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
func BlockGuide(offered []string, chart bool) string {
	var out strings.Builder
	if len(offered) == 0 {
		out.WriteString("\n\nWrite block kind none: these words ask for no block.\n")
		return out.String()
	}
	if chart && slices.Contains(offered, BlockChart) {
		out.WriteString("\n\nThe answer is best shown as a chart: write a chart block beside it.\n")
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
	decoder := json.NewDecoder(strings.NewReader(final.Spec))
	if err := decoder.Decode(&spec); err != nil {
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
		s.Mark = cmpOr(s.Mark, MarkBar)
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
		s.Label = cmpOr(s.Label, s.Field)
		if s.Compare != "" {
			s.CompareLabel = cmpOr(s.CompareLabel, s.Compare)
		} else {
			s.CompareLabel = ""
		}
		return required("table", "field")
	case BlockProgress:
		s.Table = cmpOr(s.Table, tableProgress)
		s.Label = cmpOr(s.Label, "label")
		s.Done = cmpOr(s.Done, "done")
		s.Total = cmpOr(s.Total, "total")
	case BlockIssueList:
		s.Table = tableIssues
	case BlockAgentStatus:
		s.Table = tableSessions
	case BlockTimeline:
		s.Table = tableEvents
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
		at, isText := row["at"].(string)
		_, lane := row["lane"].(string)
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

// cmpOr is the first of values that is not empty, after trimming.
func cmpOr(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

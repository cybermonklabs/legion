package going

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A block of a run (RunBlock) names the tool results of the run it reads and a chain of steps,
// which the server runs over their rows to make the rows the block draws (R3, settlement 9 of the
// plan). Every number such a block shows is one a step of the chain made, or one a saved result
// held: the model writes none.

// The eight steps of a chain.
const (
	TransformFilter = "filter"
	TransformGroup  = "group"
	TransformCount  = "count"
	TransformSum    = "sum"
	TransformMean   = "mean"
	TransformMin    = "min"
	TransformMax    = "max"
	TransformSort   = "sort"
)

// Transforms is every step of a chain, in the order the model is told them.
var Transforms = []string{TransformFilter, TransformGroup, TransformCount, TransformSum, TransformMean, TransformMin, TransformMax, TransformSort}

// The periods a group buckets a time by: its day, its week (from the Monday it falls in, in UTC),
// its month and its year, each written as the date or the part of one it names.
const (
	PerDay   = "day"
	PerWeek  = "week"
	PerMonth = "month"
	PerYear  = "year"
)

// Periods is every period a group buckets a time by, in the order the model is told them.
var Periods = []string{PerDay, PerWeek, PerMonth, PerYear}

// GroupNone is the group of a row whose field is null, missing, or an empty list.
const GroupNone = "none"

// Transform is one step of a chain as a block spec writes it: Op names the step, and only the
// fields that step takes are set. Every value a step is given is text, so a spec carries no
// number (RunBlock).
//
//   - filter keeps the rows whose Field meets exactly one condition: Is (its value, as text, is
//     this), IsNot (it is not), In (it is one of these), Has (it is a list holding this), Since
//     (it is a time at or after this one), Before (it is a time before this one), or Present (it
//     holds a value, null and missing holding none, or it does not). Once the chain has grouped,
//     a filter reads the columns of the groups.
//   - group makes one row of each value of Field, Per bucketing a time by its period, a list
//     putting its row in the group of each value it holds; the row's one column is the value,
//     named As, else Per, else Field. A chain groups once.
//   - count, sum, mean, min and max write, in each group, how many rows it holds, and the sum,
//     the mean, the least and the greatest of their Field's numbers, a row whose Field is null
//     or missing counted in count alone; the column is named As, else the step. Before any group
//     they read every row as one group.
//   - sort orders the rows by Field, asc by default or desc (Order), a row with no value last.
type Transform struct {
	Op      string   `json:"op"`
	Field   string   `json:"field,omitempty"`
	Is      *string  `json:"is,omitempty"`
	IsNot   *string  `json:"is_not,omitempty"`
	In      []string `json:"in,omitempty"`
	Has     *string  `json:"has,omitempty"`
	Since   string   `json:"since,omitempty"`
	Before  string   `json:"before,omitempty"`
	Present *bool    `json:"present,omitempty"`
	Per     string   `json:"per,omitempty"`
	As      string   `json:"as,omitempty"`
	Order   string   `json:"order,omitempty"`
}

// Chain is the steps a block runs over its rows, in order.
type Chain []Transform

// check refuses a step that is none of the eight, or that is not given what it takes: a field
// where it reads one, one condition for a filter, a period, a time or an order it knows, and
// nothing another step takes.
func (t Transform) check() error {
	conditions := 0
	for _, set := range []bool{t.Is != nil, t.IsNot != nil, len(t.In) > 0, t.Has != nil, t.Since != "", t.Before != "", t.Present != nil} {
		if set {
			conditions++
		}
	}
	switch {
	case !slices.Contains(Transforms, t.Op):
		return fmt.Errorf("%q is none of the steps %s", t.Op, strings.Join(Transforms, ", "))
	case t.Op == TransformFilter && conditions != 1:
		return fmt.Errorf("a filter takes exactly one of is, is_not, in, has, since, before and present, and this one takes %d", conditions)
	case t.Op != TransformFilter && conditions > 0:
		return fmt.Errorf("a %s takes no condition: only a filter does", t.Op)
	case t.Op == TransformCount && t.Field != "":
		return fmt.Errorf("a count takes no field: it counts the rows of each group")
	case t.Op != TransformCount && strings.TrimSpace(t.Field) == "":
		return fmt.Errorf("a %s takes a field", t.Op)
	case t.Per != "" && t.Op != TransformGroup:
		return fmt.Errorf("a %s takes no per: only a group does", t.Op)
	case t.Per != "" && !slices.Contains(Periods, t.Per):
		return fmt.Errorf("a group's per is day, week, month or year, not %q", t.Per)
	case t.As != "" && slices.Contains([]string{TransformFilter, TransformSort}, t.Op):
		return fmt.Errorf("a %s takes no as: it writes no column", t.Op)
	case t.Order != "" && t.Op != TransformSort:
		return fmt.Errorf("a %s takes no order: only a sort does", t.Op)
	case t.Order != "" && t.Order != "asc" && t.Order != "desc":
		return fmt.Errorf("a sort's order is asc or desc, not %q", t.Order)
	}
	for _, at := range []string{t.Since, t.Before} {
		if at == "" {
			continue
		}
		if _, ok := parseTime(at); !ok {
			return fmt.Errorf("%q is no time: write one as 2026-11-16 or 2026-11-16T09:00:00Z", at)
		}
	}
	return nil
}

// check refuses a chain with a step check refuses, naming the step by its place from 1.
func (c Chain) check() error {
	for index, step := range c {
		if err := step.check(); err != nil {
			return fmt.Errorf("going: step %d of the chain: %w", index+1, err)
		}
	}
	return nil
}

// chainRow is one row as a chain's steps go: its columns, and once the chain has grouped, the
// rows of the group it is, which count, sum, mean, min and max read.
type chainRow struct {
	columns map[string]any
	members []map[string]any
}

// Run runs the chain over rows, each value as JSON reads it back with numbers as json.Number,
// and answers the rows its last step leaves, each its columns. Every number in them is a value
// of rows or one a step made.
func (c Chain) Run(rows []map[string]any) ([]map[string]any, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	table := make([]chainRow, 0, len(rows))
	for _, row := range rows {
		table = append(table, chainRow{columns: row})
	}
	// grouped is the step that grouped the rows, "" before any: a group, or an aggregate that
	// came before any group and read every row as one group.
	grouped := ""
	for index, step := range c {
		var err error
		switch step.Op {
		case TransformFilter:
			// The filter's one time, where its condition is one, is read once for every row.
			bound, _ := parseTime(cmp.Or(step.Since, step.Before))
			table = slices.DeleteFunc(table, func(row chainRow) bool { return !step.keeps(row.columns, bound) })
		case TransformGroup:
			switch grouped {
			case "":
				table, err = step.group(table)
				grouped = step.Op
			case TransformGroup:
				err = fmt.Errorf("a chain groups once, and its rows are grouped already")
			default:
				err = fmt.Errorf("the %s before it read every row as one group: put the group before the %s, so it writes one row of each group", grouped, grouped)
			}
		case TransformCount, TransformSum, TransformMean, TransformMin, TransformMax:
			if grouped == "" {
				members := make([]map[string]any, 0, len(table))
				for _, row := range table {
					members = append(members, row.columns)
				}
				table, grouped = []chainRow{{columns: map[string]any{}, members: members}}, step.Op
			}
			for at := range table {
				if table[at].columns[cmp.Or(step.As, step.Op)], err = step.aggregate(table[at].members); err != nil {
					break
				}
			}
		case TransformSort:
			step.sort(table)
		}
		if err != nil {
			return nil, fmt.Errorf("going: step %d of the chain (%s): %w", index+1, step.Op, err)
		}
	}
	answer := make([]map[string]any, 0, len(table))
	for _, row := range table {
		answer = append(answer, row.columns)
	}
	return answer, nil
}

// keeps reports whether row meets the filter's one condition; bound is the condition's time, read
// from Since or Before, where it is one of those.
func (t Transform) keeps(row map[string]any, bound time.Time) bool {
	value, _ := fieldValue(row, t.Field)
	text, isText := valueText(value)
	switch {
	case t.Present != nil:
		return (value != nil) == *t.Present
	case t.Is != nil:
		return isText && text == *t.Is
	case t.IsNot != nil:
		return !isText || text != *t.IsNot
	case len(t.In) > 0:
		return isText && slices.Contains(t.In, text)
	case t.Has != nil:
		list, isList := value.([]any)
		return isList && slices.ContainsFunc(list, func(element any) bool {
			text, isText := valueText(element)
			return isText && text == *t.Has
		})
	case t.Since != "":
		at, isTime := valueTime(value)
		return isTime && !at.Before(bound)
	case t.Before != "":
		at, isTime := valueTime(value)
		return isTime && at.Before(bound)
	}
	return false
}

// group makes one row of each value of the step's field across table, in the order each value
// first comes. A group's key is its value as text, the text a filter's is compares (valueText),
// so a 1 and a "1" are one group as a filter on "1" keeps both; and a row is a member of each of
// its groups once, whatever a list of it holds twice.
func (t Transform) group(table []chainRow) ([]chainRow, error) {
	column := cmp.Or(t.As, t.Per, t.Field)
	var groups []chainRow
	at := map[string]int{}
	var joined []int
	for index, row := range table {
		keys, err := t.groupKeys(row.columns)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", index+1, err)
		}
		joined = joined[:0]
		for _, key := range keys {
			text, _ := valueText(key)
			place, held := at[text]
			if !held {
				place = len(groups)
				at[text] = place
				groups = append(groups, chainRow{columns: map[string]any{column: key}})
			}
			if slices.Contains(joined, place) {
				continue
			}
			joined = append(joined, place)
			groups[place].members = append(groups[place].members, row.columns)
		}
	}
	return groups, nil
}

// groupKeys is each group row belongs to: GroupNone for no value and for an empty list, each
// value of a list, and its period where the step has one.
func (t Transform) groupKeys(row map[string]any) ([]any, error) {
	value, _ := fieldValue(row, t.Field)
	values := []any{value}
	if list, isList := value.([]any); isList {
		if len(list) == 0 {
			return []any{GroupNone}, nil
		}
		values = list
	}
	keys := make([]any, 0, len(values))
	for _, value := range values {
		if t.Per != "" {
			at, isTime := valueTime(value)
			if !isTime {
				return nil, fmt.Errorf("%s holds %s, which is no time to group by %s: filter on present first to keep only the rows that hold one", t.Field, valueShown(value), t.Per)
			}
			keys = append(keys, period(at, t.Per))
			continue
		}
		switch value.(type) {
		case nil:
			keys = append(keys, GroupNone)
		case string, json.Number:
			keys = append(keys, value)
		case bool:
			text, _ := valueText(value)
			keys = append(keys, text)
		default:
			return nil, fmt.Errorf("%s holds %s, which no group is made of: group by one field of it, as %s.<field>", t.Field, valueShown(value), t.Field)
		}
	}
	return keys, nil
}

// period is the day, week, month or year at falls in, in UTC, written as the date it starts on,
// or the month or the year alone.
func period(at time.Time, per string) string {
	at = at.UTC()
	switch per {
	case PerWeek:
		monday := at.AddDate(0, 0, -((int(at.Weekday()) + 6) % 7))
		return monday.Format(time.DateOnly)
	case PerMonth:
		return at.Format("2006-01")
	case PerYear:
		return at.Format("2006")
	}
	return at.Format(time.DateOnly)
}

// aggregate is the step's number over members: how many they are, or the sum, the mean, the
// least or the greatest of their field's numbers, nil for the mean, the least or the greatest of
// none. A value that is no number, null and missing apart, is refused, naming it.
func (t Transform) aggregate(members []map[string]any) (any, error) {
	if t.Op == TransformCount {
		return json.Number(strconv.Itoa(len(members))), nil
	}
	var numbers []json.Number
	for _, member := range members {
		value, _ := fieldValue(member, t.Field)
		switch number := value.(type) {
		case nil:
		case json.Number:
			numbers = append(numbers, number)
		default:
			return nil, fmt.Errorf("%s holds %s, which is no number to take the %s of", t.Field, valueShown(value), t.Op)
		}
	}
	if t.Op == TransformSum {
		return sum(numbers)
	}
	if len(numbers) == 0 {
		return nil, nil
	}
	best, bestValue := numbers[0], 0.0
	total := 0.0
	for index, number := range numbers {
		value, err := number.Float64()
		if err != nil {
			return nil, fmt.Errorf("%s holds %s, past what a number holds", t.Field, number)
		}
		total += value
		if index == 0 || (t.Op == TransformMin && value < bestValue) || (t.Op == TransformMax && value > bestValue) {
			best, bestValue = number, value
		}
	}
	if t.Op == TransformMean {
		return formatNumber(total / float64(len(numbers)))
	}
	return best, nil
}

// sum is the sum of numbers: exact while every one is a whole number and the sum fits, else as
// float64 adds them.
func sum(numbers []json.Number) (json.Number, error) {
	var whole int64
	exact := true
	for _, number := range numbers {
		value, err := number.Int64()
		next := whole + value
		if err != nil || (value > 0 && next < whole) || (value < 0 && next > whole) {
			exact = false
			break
		}
		whole = next
	}
	if exact {
		return json.Number(strconv.FormatInt(whole, 10)), nil
	}
	total := 0.0
	for _, number := range numbers {
		value, err := number.Float64()
		if err != nil {
			return "", fmt.Errorf("%s is past what a number holds", number)
		}
		total += value
	}
	return formatNumber(total)
}

// formatNumber writes value as JSON writes a number, refusing one past what float64 holds.
func formatNumber(value float64) (json.Number, error) {
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return "", fmt.Errorf("the answer is past what a number holds")
	}
	return json.Number(strconv.FormatFloat(value, 'f', -1, 64)), nil
}

// sort orders table by the step's field, stably, a row with no value last whatever the order.
// Each row's place is read once (orderOf), not at each comparison.
func (t Transform) sort(table []chainRow) {
	type placed struct {
		row   chainRow
		order orderKey
	}
	rows := make([]placed, len(table))
	for index, row := range table {
		value, _ := fieldValue(row.columns, t.Field)
		rows[index] = placed{row: row, order: orderOf(value)}
	}
	slices.SortStableFunc(rows, func(a, b placed) int {
		switch {
		case !a.order.held && !b.order.held:
			return 0
		case !a.order.held:
			return 1
		case !b.order.held:
			return -1
		}
		order := a.order.compare(b.order)
		if t.Order == "desc" {
			order = -order
		}
		return order
	})
	for index, row := range rows {
		table[index] = row.row
	}
}

// orderKey is where a value sorts: numbers by their value, before text, which is ordered as
// written (a time as RFC 3339 writes it orders as its moment); anything else after both, by its
// JSON text. held is false for null and missing.
type orderKey struct {
	held   bool
	rank   int
	number float64
	text   string
}

// orderOf is value's orderKey.
func orderOf(value any) orderKey {
	switch value := value.(type) {
	case nil:
		return orderKey{}
	case json.Number:
		number, _ := value.Float64()
		return orderKey{held: true, rank: 0, number: number}
	case string:
		return orderKey{held: true, rank: 1, text: value}
	}
	return orderKey{held: true, rank: 2, text: valueShown(value)}
}

// compare orders two held keys.
func (k orderKey) compare(other orderKey) int {
	if byRank := cmp.Compare(k.rank, other.rank); byRank != 0 {
		return byRank
	}
	if k.rank == 0 {
		return cmp.Compare(k.number, other.number)
	}
	return strings.Compare(k.text, other.text)
}

// fieldValue is the value at path in row, its parts apart by dots, each naming a field of the
// object before it; false where the row holds none.
func fieldValue(row map[string]any, path string) (any, bool) {
	if value, held := row[path]; held {
		return value, true
	}
	var value any = row
	for part := range strings.SplitSeq(path, ".") {
		object, isObject := value.(map[string]any)
		if !isObject {
			return nil, false
		}
		if value, isObject = object[part]; !isObject {
			return nil, false
		}
	}
	return value, true
}

// valueText is a value as text a filter compares: text itself, a number as JSON writes it, true
// or false; false for null, a list and an object.
func valueText(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case json.Number:
		return value.String(), true
	case bool:
		return strconv.FormatBool(value), true
	}
	return "", false
}

// valueTime is a value as the time it writes, RFC 3339 or a date alone; false for any other.
func valueTime(value any) (time.Time, bool) {
	text, isText := value.(string)
	if !isText {
		return time.Time{}, false
	}
	return parseTime(text)
}

// parseTime reads text as RFC 3339, or as a date alone, which is its midnight in UTC.
func parseTime(text string) (time.Time, bool) {
	if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return at, true
	}
	if at, err := time.Parse(time.DateOnly, text); err == nil {
		return at, true
	}
	return time.Time{}, false
}

// valueShown is a value as an error names it: its JSON text, at most 80 bytes of it, cut where a
// character starts.
func valueShown(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	if len(encoded) > 80 {
		cut := 80
		for cut > 0 && !utf8.RuneStart(encoded[cut]) {
			cut--
		}
		return string(encoded[:cut]) + "…"
	}
	return string(encoded)
}

// Result is one tool result of a run as its saved step holds it, which a block's chain reads: the
// step and the call that made it, the tool it called and the arguments it called it with, and the
// status and body it answered. NotWhole is why the body is not the call's whole answer - the
// server cut it at 1 MiB, or the step saved only the first bytes the model was given - and Unread
// why the step holds no answer a chain reads at all, a refusal or a tool's own words; each is ""
// where it does not apply.
type Result struct {
	Step, Call int
	Tool       string
	Arguments  json.RawMessage
	Status     int
	Body       []byte
	NotWhole   string
	Unread     string
}

// resultPage is the rows a block reads of one result, and where they stand in the list the read
// pages through: the rows offset to offset+len(rows) of total, where the answer says so (paged),
// and the cursor of its next page, or that more rows follow it with none, where it says those.
type resultPage struct {
	ref           StepRef
	result        Result
	rows          []map[string]any
	paged         bool
	offset, total int
	next          string
	more          bool
}

// readPage is the rows ref names of result, refused where the result holds no rows a chain may
// read: no answer, a status that is no success, a body that is not whole or no JSON, or no list
// of objects where ref says the rows are.
func readPage(ref StepRef, result Result) (resultPage, error) {
	switch {
	case result.Unread != "":
		return resultPage{}, fmt.Errorf("going: step %d holds no answer a chain reads: %s", ref.Step, result.Unread)
	case result.Status/100 != 2:
		return resultPage{}, fmt.Errorf("going: step %d's call answered status %d, which holds no rows", ref.Step, result.Status)
	case result.NotWhole != "":
		return resultPage{}, fmt.Errorf("going: the data was not whole: step %d's result %s. Read the rest - in pages, or with a narrower read - and ask for the block again naming each step", ref.Step, result.NotWhole)
	}
	decoder := json.NewDecoder(bytes.NewReader(result.Body))
	decoder.UseNumber()
	var body any
	if err := decoder.Decode(&body); err != nil {
		return resultPage{}, fmt.Errorf("going: step %d's result is no JSON, so it holds no rows a chain reads", ref.Step)
	}
	page := resultPage{ref: ref, result: result}
	list, err := rowsOf(body, ref)
	if err != nil {
		return resultPage{}, err
	}
	page.rows = make([]map[string]any, 0, len(list))
	for index, element := range list {
		row, isObject := element.(map[string]any)
		if !isObject {
			return resultPage{}, fmt.Errorf("going: row %d of step %d's rows is %s, no object of fields", index+1, ref.Step, valueShown(element))
		}
		page.rows = append(page.rows, row)
	}
	if object, isObject := body.(map[string]any); isObject {
		total, hasTotal := wholeNumber(object["total"])
		offset, hasOffset := wholeNumber(object["offset"])
		page.paged, page.total, page.offset = hasTotal && hasOffset, total, offset
		page.next, _ = object["next_cursor"].(string)
		page.more, _ = object["has_more"].(bool)
	}
	return page, nil
}

// rowsOf is the list of rows of body: at ref.Rows where ref names it, else the body where it is
// a list, else the one field of the body that holds a list of objects.
func rowsOf(body any, ref StepRef) ([]any, error) {
	if ref.Rows != "" {
		object, _ := body.(map[string]any)
		value, held := fieldValue(object, ref.Rows)
		list, isList := value.([]any)
		if !held || !isList {
			return nil, fmt.Errorf("going: step %d's result holds no list at %q", ref.Step, ref.Rows)
		}
		return list, nil
	}
	if list, isList := body.([]any); isList {
		return list, nil
	}
	object, isObject := body.(map[string]any)
	if !isObject {
		return nil, fmt.Errorf("going: step %d's result is %s, which holds no rows", ref.Step, valueShown(body))
	}
	var lists []string
	for _, name := range slices.Sorted(maps.Keys(object)) {
		if list, isList := object[name].([]any); isList && (len(list) == 0 || isObjectValue(list[0])) {
			lists = append(lists, name)
		}
	}
	if len(lists) != 1 {
		return nil, fmt.Errorf("going: name the rows of step %d (its rows): the field of its result that holds them, one of: %s", ref.Step, strings.Join(lists, ", "))
	}
	return object[lists[0]].([]any), nil
}

// isObjectValue reports whether value is a JSON object.
func isObjectValue(value any) bool {
	_, isObject := value.(map[string]any)
	return isObject
}

// wholeNumber is value as a whole number at or above zero.
func wholeNumber(value any) (int, bool) {
	number, isNumber := value.(json.Number)
	if !isNumber {
		return 0, false
	}
	whole, err := strconv.Atoi(number.String())
	return whole, err == nil && whole >= 0
}

// wholeRows is the rows of pages, in their order, refused where they are not the whole of every
// list they page through: a page whose next cursor no page among them was read with, a page that
// says more rows follow it with no cursor, or pages of one paged read that leave rows of it out,
// its last page among them. Where two pages of one read overlap, the rows the earlier one holds
// are read once. The reads are judged in the order of their keys, so a block with two reads that
// are not whole is refused for the same one every time.
func wholeRows(pages []resultPage) ([]map[string]any, error) {
	for _, page := range pages {
		switch {
		case page.next != "" && !slices.ContainsFunc(pages, func(other resultPage) bool { return readWithCursor(other.result.Arguments, page.next) }):
			return nil, fmt.Errorf("going: the data was not whole: step %d read a page of %s whose rows go on past it, and no step this block names read its next page (cursor %q). Read it, name it among the steps, and ask for the block again",
				page.ref.Step, page.result.Tool, page.next)
		case page.next == "" && page.more:
			return nil, fmt.Errorf("going: the data was not whole: step %d read a page of %s that says more rows follow it. Read the rest, name each step, and ask for the block again", page.ref.Step, page.result.Tool)
		}
	}
	reads := map[string][]int{}
	for index, page := range pages {
		if page.paged {
			key := page.result.Tool + " " + argumentsBesidePaging(page.result.Arguments)
			reads[key] = append(reads[key], index)
		}
	}
	kept := make([][]map[string]any, len(pages))
	for index, page := range pages {
		kept[index] = page.rows
	}
	for _, read := range slices.Sorted(maps.Keys(reads)) {
		indexes := reads[read]
		slices.SortStableFunc(indexes, func(a, b int) int { return cmp.Compare(pages[a].offset, pages[b].offset) })
		covered, total := 0, pages[indexes[0]].total
		for _, index := range indexes {
			page := pages[index]
			if page.offset > covered {
				return nil, fmt.Errorf("going: the data was not whole: of the %d rows %s lists, rows %d to %d were read by no step this block names. Read them, name each step, and ask for the block again",
					page.total, page.result.Tool, covered+1, page.offset)
			}
			kept[index] = page.rows[min(covered-page.offset, len(page.rows)):]
			covered = max(covered, page.offset+len(page.rows))
			total = max(total, page.total)
		}
		if covered < total {
			last := pages[indexes[len(indexes)-1]]
			return nil, fmt.Errorf("going: the data was not whole: step %d read up to row %d of the %d rows %s lists, and no step this block names read its last page. Read the rest, name each step, and ask for the block again",
				last.ref.Step, covered, total, last.result.Tool)
		}
	}
	var rows []map[string]any
	for _, page := range kept {
		rows = append(rows, page...)
	}
	return rows, nil
}

// readWithCursor reports whether arguments, a call's, hold cursor as the value of one of their
// fields: the call that read the page cursor names.
func readWithCursor(arguments json.RawMessage, cursor string) bool {
	var fields map[string]any
	if json.Unmarshal(arguments, &fields) != nil {
		return false
	}
	for _, value := range fields {
		if value == cursor {
			return true
		}
	}
	return false
}

// argumentsBesidePaging is arguments, a call's, without its offset and its limit, as one JSON
// text with its fields in order: the pages of one read share it.
func argumentsBesidePaging(arguments json.RawMessage) string {
	var fields map[string]any
	if json.Unmarshal(arguments, &fields) != nil {
		return string(arguments)
	}
	delete(fields, "offset")
	delete(fields, "limit")
	encoded, _ := json.Marshal(fields)
	return string(encoded)
}

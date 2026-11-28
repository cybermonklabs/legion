package charts

import (
	"encoding/json"
	"fmt"
	"slices"
)

// The groups a step line is drawn in (Step.Group): one line for each col of the series, the
// default where a chart names no group, and further one for each row or each row group.
const (
	GroupCol      = "col"
	GroupRow      = "row"
	GroupRowGroup = "row_group"
)

// Step is one series a declaration draws as a step line over measured time, and the group each of
// its lines is (GroupCol, GroupRow or GroupRowGroup): a line starts at its window's start at the
// value of its group's reading before the window (dispatch://INFRA-2918, D20 and D21).
type Step struct {
	Series string
	Group  string
}

// Steps is every series that declared, a list of charts the validator accepted, draws as a step
// line over measured time, each with its group, each pair once, in the order the list names them.
// A step line is a chart of curve step whose x is measured time (xForm's time form: an xy or a band
// with neither an x series, x steps nor panels), and its series are its y's and each y option's.
// A list of no such chart answers none. The error is for declared that is no JSON.
func Steps(declared json.RawMessage) ([]Step, error) {
	value, err := decode(declared)
	if err != nil {
		return nil, fmt.Errorf("read the charts: %w", err)
	}
	var steps []Step
	for _, chart := range listOr(value) {
		chart, ok := asObject(chart)
		if !ok || !is(chart.get("curve"), "step") || chart.has("panels") {
			continue
		}
		kind, _ := chart.get("kind").(string)
		if xForm(kind, objectOr(chart.get("x")), chart.get("stack")) != formTime {
			continue
		}
		group := GroupCol
		if is(chart.get("group"), GroupRow) || is(chart.get("group"), GroupRowGroup) {
			group = chart.get("group").(string)
		}
		for _, axis := range append([]any{chart.get("y")}, listOr(chart.get("y_options"))...) {
			axis, ok := asObject(axis)
			if !ok {
				continue
			}
			if series, named := axis.get("series").(string); named && !slices.Contains(steps, Step{Series: series, Group: group}) {
				steps = append(steps, Step{Series: series, Group: group})
			}
		}
	}
	return steps, nil
}

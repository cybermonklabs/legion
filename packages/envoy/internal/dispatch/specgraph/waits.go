package specgraph

import (
	"errors"
	"fmt"
)

// Unit is a work unit as the workflow spec defines it, as far as a wait reads it (R29): the
// statements it serves, the units it needs, and the statements it waits on, each nil when it is
// not set and an empty list for none. JSON tags are the form of units.json.
type Unit struct {
	ID      string   `json:"id" input:"nonzero"`
	Serves  []string `json:"serves" input:"present: [] serves no statement"`
	Needs   []string `json:"needs" input:"present: [] needs no unit"`
	WaitsOn []string `json:"waits_on" input:"present: [] waits on no statement"`
}

// unitsFault is the refusal of the first unit of units that sets no id, an id of no letter and no
// digit (hasWord), which would read as a unit named "" or named nothing, the id of a unit before
// it, which would read as one unit with the relations of both and can close a cycle that the units
// do not have, no serves, needs or waits_on, which would read as one with none and hide a cycle of
// waits (R29), or one of them that holds an id of no letter and no digit, which names no statement
// or unit; and "" when each unit is whole. The reader of units.json and WaitCycle both refuse units
// by it.
func unitsFault(units []Unit) string {
	ids := make(map[string]bool, len(units))
	for i, u := range units {
		if u.ID == "" {
			return fmt.Sprintf("unit %d has no id", i+1)
		}
		if !hasWord(u.ID) {
			return fmt.Sprintf("unit %d: its id has no letter and no digit, so it names no unit", i+1)
		}
		if ids[u.ID] {
			return fmt.Sprintf("unit %s is in the list two times", quoted(u.ID))
		}
		ids[u.ID] = true
		for _, list := range []struct {
			member, what string
			ids          []string
		}{{"serves", "statement", u.Serves}, {"needs", "unit", u.Needs}, {"waits_on", "statement", u.WaitsOn}} {
			if list.ids == nil {
				return fmt.Sprintf("unit %s: it sets no %s; a unit with none sets an empty list", quoted(u.ID), list.member)
			}
			for _, id := range list.ids {
				if !hasWord(id) {
					return fmt.Sprintf("unit %s: %s holds %s, which names no %s", quoted(u.ID), list.member, quoted(id), list.what)
				}
			}
		}
	}
	return ""
}

// WaitCycle is a cycle of waits among units, or nil when there is none. A unit waits on each unit
// it needs, and a wait on a statement counts as a wait on each unit that serves that statement
// (R29: "For the cycle rule of the workflow spec, a wait on a statement counts as a wait on each
// unit that serves that statement"). The cycle is the shortest from the smallest unit id that is on
// a cycle, written unit, statement, unit, …, back to that unit, with no statement between two units
// where the first needs the second; of two steps to one unit, a need comes first, then the
// statement with the smallest id. Only the rule is here: refusing the wait that closes a cycle is
// the workflow capability's. It gives an error, and no cycle, for units that unitsFault refuses: a
// unit with no id, or with the id of a unit before it, or that does not set serves, needs or
// waits_on, or a name in them that names nothing.
func WaitCycle(units []Unit) ([]string, error) {
	if fault := unitsFault(units); fault != "" {
		return nil, errors.New("specgraph: " + fault)
	}
	servedBy := map[string][]string{}
	for _, u := range units {
		for _, s := range u.Serves {
			servedBy[s] = append(servedBy[s], u.ID)
		}
	}
	edges := make(map[string][]edge, len(units))
	for _, u := range units {
		for _, needed := range u.Needs {
			edges[u.ID] = append(edges[u.ID], edge{to: needed})
		}
		for _, s := range u.WaitsOn {
			for _, server := range servedBy[s] {
				edges[u.ID] = append(edges[u.ID], edge{to: server, relation: s})
			}
		}
	}
	loops := findLoops(edges)
	if len(loops) == 0 {
		return nil, nil
	}
	// findLoops writes unit, relation, unit, …; a need has no statement.
	cycle := make([]string, 0, len(loops[0]))
	for _, step := range loops[0] {
		if step != "" {
			cycle = append(cycle, step)
		}
	}
	return cycle, nil
}

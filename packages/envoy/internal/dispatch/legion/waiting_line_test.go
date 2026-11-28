package legion

import (
	"reflect"
	"slices"
	"testing"
)

// TestSameLineComparesTheFieldsTheLineReads: two blocks build the same line where neither is
// recorded, or both are and their Waiting and each Blocked entry's Issue and BlockedBy agree, in
// order, whatever their cap, P0 holders, active slots and live trees; a move of any field the
// line reads, or a block recorded beside none, builds another. Each case is asked both ways round.
//
// Every field of Admission is classified by reflection, as one the line reads or one it does not
// read, and moved alone: SameLine must answer another line for a move of a field the line reads,
// and the same line for a move of one it does not. A field the daemon's block gains, as it gained
// `p0Holders`, fails here until it is named on one side.
func TestSameLineComparesTheFieldsTheLineReads(t *testing.T) {
	capOf := func(slots int) *int { return &slots }
	block := Admission{
		Cap: capOf(3), P0Holders: capOf(1), Active: []string{"A-1"}, Trees: []string{"A-1"},
		Waiting: []string{"A-2", "A-3"},
		Blocked: []BlockedIssue{{Issue: "A-4", BlockedBy: []string{"A-5", "A-6"}}},
	}
	moved := func(move func(*Admission)) *Admission {
		next := block
		next.Active, next.Trees = slices.Clone(block.Active), slices.Clone(block.Trees)
		next.Waiting = append([]string(nil), block.Waiting...)
		next.Blocked = []BlockedIssue{{Issue: block.Blocked[0].Issue, BlockedBy: append([]string(nil), block.Blocked[0].BlockedBy...)}}
		move(&next)
		return &next
	}
	for _, test := range []struct {
		name string
		a, b *Admission
		same bool
	}{
		{"no block either side", nil, nil, true},
		{"a block beside none", &block, nil, false},
		{"an empty block beside none", &Admission{}, nil, false},
		{"the same block", &block, moved(func(*Admission) {}), true},
		{"the cap, the P0 holders, the active slots and the live trees alone moved", &block, moved(func(next *Admission) {
			next.Cap, next.P0Holders, next.Active, next.Trees = capOf(5), nil, []string{"A-1", "A-7"}, nil
		}), true},
		{"an empty Waiting beside a missing one", &Admission{Waiting: []string{}}, &Admission{}, true},
		{"the Waiting reordered", &block, moved(func(next *Admission) { next.Waiting = []string{"A-3", "A-2"} }), false},
		{"an issue joined the Waiting", &block, moved(func(next *Admission) { next.Waiting = append(next.Waiting, "A-8") }), false},
		{"another issue blocked", &block, moved(func(next *Admission) { next.Blocked[0].Issue = "A-9" }), false},
		{"a blocker finished", &block, moved(func(next *Admission) { next.Blocked[0].BlockedBy = []string{"A-5"} }), false},
		{"the blockers reordered", &block, moved(func(next *Admission) { next.Blocked[0].BlockedBy = []string{"A-6", "A-5"} }), false},
		{"a second issue blocked", &block, moved(func(next *Admission) {
			next.Blocked = append(next.Blocked, BlockedIssue{Issue: "A-10", BlockedBy: []string{"A-5"}})
		}), false},
	} {
		for _, order := range [][2]*Admission{{test.a, test.b}, {test.b, test.a}} {
			if got := SameLine(order[0], order[1]); got != test.same {
				t.Errorf("%s: SameLine(%+v, %+v) = %t, want %t", test.name, order[0], order[1], got, test.same)
			}
		}
	}

	// The fields NewWaitingLine reads, which SameLine compares, and the fields it does not read.
	reads := map[string]bool{"Waiting": true, "Blocked": true}
	notRead := map[string]bool{"Cap": true, "P0Holders": true, "Active": true, "Trees": true}
	fields := reflect.TypeFor[Admission]()
	for index := range fields.NumField() {
		name := fields.Field(index).Name
		switch {
		case reads[name] && notRead[name]:
			t.Errorf("Admission.%s is named both as a field the line reads and as one it does not", name)
			continue
		case !reads[name] && !notRead[name]:
			t.Errorf("Admission.%s is neither compared by SameLine nor named as a field the line does not read", name)
			continue
		}
		next := moved(func(next *Admission) { moveField(t, name, reflect.ValueOf(next).Elem().Field(index)) })
		for _, order := range [][2]*Admission{{&block, next}, {next, &block}} {
			if got := SameLine(order[0], order[1]); got == reads[name] {
				t.Errorf("a move of Admission.%s alone: SameLine(%+v, %+v) = %t, want %t", name, order[0], order[1], got, !reads[name])
			}
		}
	}
}

// moveField moves one field of an admission block to another value: a pointer to none where it
// points to one, or to the zero value where it points to none, and a list one entry longer.
func moveField(t *testing.T, name string, field reflect.Value) {
	t.Helper()
	switch field.Kind() {
	case reflect.Pointer:
		if field.IsNil() {
			field.Set(reflect.New(field.Type().Elem()))
		} else {
			field.SetZero()
		}
	case reflect.Slice:
		field.Set(reflect.Append(field, reflect.Zero(field.Type().Elem())))
	default:
		t.Fatalf("Admission.%s is a %s, which moveField cannot move: give it a move of its own", name, field.Kind())
	}
}

// TestAPlaceCountsEveryKeyTheLineNamesAsOneIssuesReadDoes: the daemon's line names two issues it
// reports nothing of, one before each issue it reports and one twice. The line answers each
// reported issue's place as one issue's read answers it (Admission.Place): its index in the whole
// line plus one, the unreported keys counted, and its first naming where it is named twice. An
// issue the daemon reports nothing of carries no card and is given no place, and an issue the line
// does not name none either. A place counted among the reported issues alone puts A-2 first and
// A-4 second, where one issue's read puts them second and fourth.
func TestAPlaceCountsEveryKeyTheLineNamesAsOneIssuesReadDoes(t *testing.T) {
	t.Parallel()
	admission := &Admission{Waiting: []string{"A-1", "A-2", "A-3", "A-4", "A-2"}}
	reported := map[string]bool{"A-2": true, "A-4": true, "A-5": true}
	line := NewWaitingLine(WaitingLineInput{
		Admission: admission,
		Issue: func(key string) (LineIssue, bool) {
			return LineIssue{Phase: "admitted"}, reported[key]
		},
		Tree: func(key string) (TreeNode, bool) { return TreeNode{}, false },
	})
	for _, key := range []string{"A-2", "A-4"} {
		want, _, _ := admission.Place(key)
		got := line.Place(key)
		if got == nil || *got != want {
			t.Errorf("the line places %s at %v; one issue's read places it at %d", key, placeText(got), want)
		}
	}
	for _, key := range []string{"A-1", "A-3", "A-5"} {
		if got := line.Place(key); got != nil {
			t.Errorf("the line places %s at %d, an issue that carries no card or that it does not name", key, *got)
		}
	}
}

// placeText is a place as a failure prints it: its number, or none.
func placeText(place *int) any {
	if place == nil {
		return "none"
	}
	return *place
}

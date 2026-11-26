package evals

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestEveryComputationFieldIsChangedComparedAndNamedAsOne: the fields the catalogue owns are
// stated once, as Computation and SeriesComputation, and every rule over them reads that one
// statement: each field has a change of the same name in the register write
// (ComputationChange, SeriesComputationChange) that names it as the body spells it (computationField
// refuses it on a computed eval) and writes it (applyTo), and a value of it that differs makes two
// computations differ (same: the seed writes a version where the release changed one). A field
// added to one struct and missed in one of those fails here, rather than being taken and then
// overwritten by the next seed, or refused and still carried forward.
func TestEveryComputationFieldIsChangedComparedAndNamedAsOne(t *testing.T) {
	t.Parallel()
	link := Link{Rel: "via", Href: "https://ci.example/runs", RecordedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	differing := map[reflect.Kind]any{reflect.String: "changed", reflect.Bool: true}
	for _, pair := range []struct {
		owned, change reflect.Type
		same          func(a, b reflect.Value) bool
		named         func(change reflect.Value) string
		applied       func(change reflect.Value) reflect.Value
	}{
		{
			owned: reflect.TypeFor[Computation](), change: reflect.TypeFor[ComputationChange](),
			same:  func(a, b reflect.Value) bool { return a.Interface().(Computation).same(b.Interface().(Computation)) },
			named: func(change reflect.Value) string { return change.Interface().(ComputationChange).named() },
			applied: func(change reflect.Value) reflect.Value {
				var next Computation
				change.Interface().(ComputationChange).applyTo(&next)
				return reflect.ValueOf(next)
			},
		},
		{
			owned: reflect.TypeFor[SeriesComputation](), change: reflect.TypeFor[SeriesComputationChange](),
			same: func(a, b reflect.Value) bool {
				return a.Interface().(SeriesComputation).same(b.Interface().(SeriesComputation))
			},
			named: func(change reflect.Value) string { return change.Interface().(SeriesComputationChange).named() },
			applied: func(change reflect.Value) reflect.Value {
				var next SeriesComputation
				change.Interface().(SeriesComputationChange).applyTo(&next, link.RecordedAt)
				return reflect.ValueOf(next)
			},
		},
	} {
		if pair.owned.NumField() != pair.change.NumField() {
			t.Errorf("%s has %d fields and %s %d, want one change for each field", pair.owned.Name(), pair.owned.NumField(), pair.change.Name(), pair.change.NumField())
		}
		for index := range pair.owned.NumField() {
			field := pair.owned.Field(index)
			tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			value := reflect.New(field.Type).Elem()
			switch {
			case field.Type == reflect.TypeFor[[]FailureMode]():
				value = reflect.ValueOf([]FailureMode{{Mode: "A mode.", Guard: "A guard."}})
			case field.Type == reflect.TypeFor[[]Link]():
				value = reflect.ValueOf([]Link{link})
			default:
				value = reflect.ValueOf(differing[field.Type.Kind()]).Convert(field.Type)
			}
			changed := reflect.New(pair.owned).Elem()
			changed.Field(index).Set(value)
			if pair.same(reflect.New(pair.owned).Elem(), changed) {
				t.Errorf("%s.same reads two computations that differ in %s as the same", pair.owned.Name(), field.Name)
			}
			changeField, held := pair.change.FieldByName(field.Name)
			if !held {
				t.Errorf("%s has no change of %s.%s", pair.change.Name(), pair.owned.Name(), field.Name)
				continue
			}
			change := reflect.New(pair.change).Elem()
			change.FieldByIndex(changeField.Index).FieldByName("Set").SetBool(true)
			change.FieldByIndex(changeField.Index).FieldByName("Value").Set(value)
			if named := pair.named(change); named != tag {
				t.Errorf("a change setting %s.%s names %q, want %q, as the body spells it", pair.change.Name(), field.Name, named, tag)
			}
			if got := pair.applied(change).Field(index); !reflect.DeepEqual(got.Interface(), value.Interface()) {
				t.Errorf("%s's applyTo leaves %s %v, want %v", pair.change.Name(), field.Name, got.Interface(), value.Interface())
			}
		}
	}
}

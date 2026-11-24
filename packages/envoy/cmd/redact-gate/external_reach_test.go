package main

import (
	"go/token"
	"go/types"
	"testing"
)

// TestAReflectionOperandReachesAnExternalErrorTypeThroughEveryEdgeTheBoxsWalkFollows: the
// reflection census refuses an operand that reaches one of gate.external_errors (external) and
// one that reaches the box (contains) through the same edges, so a type that holds an external
// error type in a channel, which a reflection of its value reaches as it reaches a channel of the
// box, is refused as one that holds it in a slice or a field is; a type that holds none is not.
func TestAReflectionOperandReachesAnExternalErrorTypeThroughEveryEdgeTheBoxsWalkFollows(t *testing.T) {
	pkg := types.NewPackage("example.com/e", "e")
	named := func(name string) *types.Named {
		return types.NewNamed(types.NewTypeName(token.NoPos, pkg, name, nil), types.NewStruct(nil, nil), nil)
	}
	external, box := named("Err"), named("Box")
	w := &walker{g: &gate{inv: inventory{Gate: gateConfig{External: []string{"example.com/e.Err"}}}}, boxPkg: "example.com/e", boxName: "Box"}
	field := func(name string, t types.Type) *types.Struct {
		return types.NewStruct([]*types.Var{types.NewField(token.NoPos, pkg, name, t, false)}, nil)
	}
	for _, tc := range []struct {
		name    string
		of      func(types.Type) types.Type
		reaches bool
	}{
		{"the type itself", func(t types.Type) types.Type { return t }, true},
		{"a pointer", func(t types.Type) types.Type { return types.NewPointer(t) }, true},
		{"a slice of pointers", func(t types.Type) types.Type { return types.NewSlice(types.NewPointer(t)) }, true},
		{"a field", func(t types.Type) types.Type { return field("err", t) }, true},
		{"a channel", func(t types.Type) types.Type { return types.NewChan(types.SendRecv, t) }, true},
		{"a channel of pointers", func(t types.Type) types.Type { return types.NewChan(types.SendRecv, types.NewPointer(t)) }, true},
		{"a map's value that is a channel", func(t types.Type) types.Type {
			return types.NewMap(types.Typ[types.String], types.NewChan(types.RecvOnly, t))
		}, true},
		{"a field that is a channel", func(t types.Type) types.Type { return field("errs", types.NewChan(types.SendOnly, t)) }, true},
		{"a string, which holds neither", func(types.Type) types.Type { return types.Typ[types.String] }, false},
	} {
		// Control: the box's own walk reaches the box through the same shape, so the edge is one
		// the census follows for the box.
		if got := w.contains(tc.of(box), map[types.Type]bool{}); got != tc.reaches {
			t.Fatalf("control: contains(%s of the box) = %t, want %t", tc.name, got, tc.reaches)
		}
		if got := w.external(tc.of(external), map[types.Type]bool{}); got != tc.reaches {
			t.Errorf("external(%s of %s) = %t, want %t, as contains answers for the box", tc.name, external, got, tc.reaches)
		}
	}
}

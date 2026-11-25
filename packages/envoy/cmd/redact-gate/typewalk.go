package main

import "go/types"

// edges are the edges the one walk of a type's structure (reaches) follows, each from a type to a
// type a value of it holds. With no field set, a walk follows a named type to its underlying
// type, a pointer, a slice and an array to its element, a map to its key and its value, and a
// struct to each of its fields, and no walk follows an interface's methods. A walk whose question
// needs other edges, or fewer, sets them here; its caller's doc says why.
type edges struct {
	chans       bool                              // a channel to its element
	signatures  bool                              // a function type to its parameters' and results' types
	typeArgs    bool                              // a named type to its type arguments, in place of its underlying type
	topPointer  bool                              // a pointer only at the top: the type walked, or the underlying type of a named type at the top
	skipMapKeys bool                              // a map to its value alone, not its key
	field       func(s *types.Struct, i int) bool // which fields of a struct it follows; nil follows every one
}

// reaches reports whether a value of t holds a type leaf accepts: t itself, or a type e's edges
// reach from it. leaf is given each type with its aliases resolved. seen holds the types already
// walked, each walked once, so a recursive type ends the walk.
func (e edges) reaches(t types.Type, leaf func(types.Type) bool, seen map[types.Type]bool) bool {
	return e.walk(t, true, leaf, seen)
}

// walk is reaches from t, which is at the top (top) or below it.
func (e edges) walk(t types.Type, top bool, leaf func(types.Type) bool, seen map[types.Type]bool) bool {
	t = types.Unalias(t)
	if seen[t] {
		return false
	}
	seen[t] = true
	if leaf(t) {
		return true
	}
	switch t := t.(type) {
	case *types.Named:
		if !e.typeArgs {
			return e.walk(t.Underlying(), top, leaf, seen)
		}
		for i := range t.TypeArgs().Len() {
			if e.walk(t.TypeArgs().At(i), false, leaf, seen) {
				return true
			}
		}
	case *types.Pointer:
		return (top || !e.topPointer) && e.walk(t.Elem(), false, leaf, seen)
	case *types.Slice:
		return e.walk(t.Elem(), false, leaf, seen)
	case *types.Array:
		return e.walk(t.Elem(), false, leaf, seen)
	case *types.Map:
		return !e.skipMapKeys && e.walk(t.Key(), false, leaf, seen) || e.walk(t.Elem(), false, leaf, seen)
	case *types.Chan:
		return e.chans && e.walk(t.Elem(), false, leaf, seen)
	case *types.Struct:
		for i := range t.NumFields() {
			if (e.field == nil || e.field(t, i)) && e.walk(t.Field(i).Type(), false, leaf, seen) {
				return true
			}
		}
	case *types.Signature:
		if !e.signatures {
			return false
		}
		for _, tuple := range []*types.Tuple{t.Params(), t.Results()} {
			for i := range tuple.Len() {
				if e.walk(tuple.At(i).Type(), false, leaf, seen) {
					return true
				}
			}
		}
	}
	return false
}

// holdsBox reports whether a value of t holds the box, boxPkg.boxName, anywhere: the box itself,
// or through a pointer, a field (embedded or not), a slice, an array, a map's key or value, or a
// channel, which holds what is sent on it.
func holdsBox(t types.Type, boxPkg, boxName string, seen map[types.Type]bool) bool {
	return edges{chans: true}.reaches(t, func(t types.Type) bool {
		n, ok := t.(*types.Named)
		return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == boxPkg && n.Obj().Name() == boxName
	}, seen)
}

package storetext

import "reflect"

// maxDepth bounds how deep Strings reads into a value: past it, what a value holds is left as it
// is. A fact is a tree a few levels deep, so only a cycle of pointers reaches the bound, which is
// what keeps Strings from following one for ever.
const maxDepth = 64

// Strings is v with Text applied to every string reachable from it: v itself when it is a
// string, and the strings its exported struct fields, the elements of its slices and arrays, the
// keys and values of its maps, and what its pointers and interfaces point to hold, at any depth up
// to maxDepth. A value of a type whose underlying type is string counts as a string.
//
// A value that arrives from outside the daemon can hold a NUL, or a byte that is not UTF-8, in a
// title, a check run's name or a session id, and the store keeps U+FFFD in its place; compared
// as it arrived with what the store holds of it, the value differs from its own stored copy. So
// such a value is given through the store's own rule before it is compared.
//
// When the rule changes none of its strings, Strings returns v itself: its slices, maps and
// pointers are the caller's own, and nothing is allocated but the copies reflection makes of a
// non-empty map's keys and values to read them (reflect.Value.MapKeys and MapIndex). The check
// only reads v, and never lets it escape, so a v of a concrete type costs no allocation to be
// read either. Otherwise Strings returns a copy made only along each path that leads to a string
// the rule changes, and never writes into the caller's value: a struct is copied, a pointer points
// to a copy of what it pointed to, and a slice or map is a new one; every slice, map and pointer
// on no such path is the caller's own. Two map keys of string kind that the rule makes one keep
// the value of the greater original key, as JSON does, so the result does not depend on a map's
// order.
//
// What Strings leaves as it is:
//   - bytes ([]byte, json.RawMessage): no text rule reads them, and where a byte slice holds a JSON
//     value stored in a jsonb column, the store's codec applies JSON to it there;
//   - an unexported field, which reflection can read but not set: Strings keeps it in a copy as it
//     was, and reads nothing behind it.
func Strings[T any](v T) T {
	if !holdsChange(reflect.ValueOf(v), 0) {
		return v
	}
	return cleaned(reflect.ValueOf(v), 0).Interface().(T)
}

// holdsChange reports whether v, read at depth, reaches a string that Text changes. It only reads
// v, and lets nothing of it escape: it is the check Strings makes before it copies anything. It
// reads a map by its keys (MapKeys) rather than through a reflect.MapIter, which would let v
// escape and cost a v of a concrete type its allocation on every call.
func holdsChange(v reflect.Value, depth int) bool {
	if depth > maxDepth {
		return false
	}
	switch v.Kind() {
	case reflect.String:
		return !storable(v.String())
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && holdsChange(v.Elem(), depth+1)
	case reflect.Struct:
		t := v.Type()
		for i := range v.NumField() {
			if t.Field(i).IsExported() && holdsChange(v.Field(i), depth+1) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if !mayHoldText(v.Type().Elem()) {
			return false
		}
		for i := range v.Len() {
			if holdsChange(v.Index(i), depth+1) {
				return true
			}
		}
	case reflect.Map:
		t := v.Type()
		keys, elems := mayHoldText(t.Key()), mayHoldText(t.Elem())
		if v.Len() == 0 || !keys && !elems {
			return false
		}
		for _, key := range v.MapKeys() {
			if keys && holdsChange(key, depth+1) || elems && holdsChange(v.MapIndex(key), depth+1) {
				return true
			}
		}
	}
	return false
}

// mayHoldText reports whether a value of type t can reach a string at all: a string can, and so
// can a struct, a pointer, an interface or a map, and a slice or an array of any of those. A
// number, a bool, and a slice or array of bytes or numbers cannot, so Strings never reads their
// elements.
func mayHoldText(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.String, reflect.Struct, reflect.Pointer, reflect.Interface, reflect.Map:
		return true
	case reflect.Slice, reflect.Array:
		return mayHoldText(t.Elem())
	}
	return false
}

// cleaned is v, read at depth, which holdsChange reports true of, with Text applied to each string
// it reaches: a new value along each path to a string the rule changes, sharing everything else
// with v.
func cleaned(v reflect.Value, depth int) reflect.Value {
	t := v.Type()
	switch v.Kind() {
	case reflect.String:
		out := reflect.New(t).Elem()
		out.SetString(Text(v.String()))
		return out
	case reflect.Pointer:
		out := reflect.New(t.Elem())
		out.Elem().Set(cleaned(v.Elem(), depth+1))
		return out
	case reflect.Interface:
		out := reflect.New(t).Elem()
		out.Set(cleaned(v.Elem(), depth+1))
		return out
	case reflect.Struct:
		out := reflect.New(t).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if field := v.Field(i); t.Field(i).IsExported() && holdsChange(field, depth+1) {
				out.Field(i).Set(cleaned(field, depth+1))
			}
		}
		return out
	case reflect.Slice, reflect.Array:
		var out reflect.Value
		if v.Kind() == reflect.Slice {
			out = reflect.MakeSlice(t, v.Len(), v.Len())
			reflect.Copy(out, v)
		} else {
			out = reflect.New(t).Elem()
			out.Set(v)
		}
		for i := range v.Len() {
			if element := v.Index(i); holdsChange(element, depth+1) {
				out.Index(i).Set(cleaned(element, depth+1))
			}
		}
		return out
	case reflect.Map:
		out := reflect.MakeMapWithSize(t, v.Len())
		// originals is, for a map whose keys are strings, the key each entry of out had in v, by
		// the key it has in out.
		var originals map[string]string
		if t.Key().Kind() == reflect.String {
			originals = make(map[string]string, v.Len())
		}
		var entries reflect.MapIter
		entries.Reset(v)
		for entries.Next() {
			original, elem := entries.Key(), entries.Value()
			key := original
			if holdsChange(key, depth+1) {
				key = cleaned(key, depth+1)
			}
			if originals != nil {
				if kept, found := originals[key.String()]; found && kept > original.String() {
					continue
				}
				originals[key.String()] = original.String()
			}
			if holdsChange(elem, depth+1) {
				elem = cleaned(elem, depth+1)
			}
			out.SetMapIndex(key, elem)
		}
		return out
	}
	return v
}

// packages/envoy/internal/testwait/reading.go and packages/daemon/internal/testwait/reading.go are
// the same file, as stacks.go is: each module's testwait gives up the same way.

package testwait

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unsafe"
)

// describe is how a give-up prints a wait's last reading: as fmt's %+v prints it, but a pointer
// prints what it points to, or <nil>, wherever it sits in the reading. %+v prints a pointer below
// the reading's top as an address, and calls no method of an unexported field, so a reading whose
// field is a *time.Time printed 0xc045105710 and left a reader unable to tell a nil from a stale
// time from the one waited for (dispatch://INFRA-3904/comment/049949a9-9bea-4166-97c9-b2f3d3a33e3f).
// A value whose type has a Format, Error or String method prints by it, as %+v prints it,
// unexported fields included; a pointer to a struct, an array, a slice or a map prints with a
// leading &, as %+v prints one at the top; a pointer, slice or map already being printed prints
// as <cycle>; a channel, a function and an unsafe.Pointer, which hold nothing a reader could
// read, print as %+v prints them; and a map prints its entries in the order of their printed keys.
func describe(value any) string {
	if value == nil {
		return "<nil>"
	}
	root := reflect.New(reflect.TypeOf(value)).Elem()
	root.Set(reflect.ValueOf(value))
	d := describer{path: map[place]bool{}}
	d.value(root)
	return d.out.String()
}

// describer is one describe: the text so far, and the pointers, slices and maps it is inside, so
// a reading that holds itself ends.
type describer struct {
	out  strings.Builder
	path map[place]bool
}

// place is a pointer, a slice or a map by what it points at: its kind, address and length.
type place struct {
	kind    reflect.Kind
	address uintptr
	length  int
}

// readable is v with its methods callable and its fields addressable: an unexported field's
// value, which reflect lets no caller interface or call, read through its address, and a value that
// has no address (a map's entry) copied to one that does.
func readable(v reflect.Value) reflect.Value {
	if !v.CanInterface() && v.CanAddr() {
		v = reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
	}
	if v.CanInterface() && !v.CanAddr() {
		addressed := reflect.New(v.Type()).Elem()
		addressed.Set(v)
		v = addressed
	}
	return v
}

func (d *describer) value(v reflect.Value) {
	v = readable(v)
	switch v.Kind() {
	case reflect.Invalid:
		d.out.WriteString("<nil>")
		return
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			d.out.WriteString("<nil>")
			return
		}
	}
	if v.CanInterface() {
		switch v.Interface().(type) {
		case fmt.Formatter, error, fmt.Stringer:
			fmt.Fprintf(&d.out, "%+v", v.Interface())
			return
		}
	}
	switch v.Kind() {
	case reflect.Interface:
		d.value(v.Elem())
	case reflect.Pointer:
		d.within(v, func() {
			switch v.Elem().Kind() {
			case reflect.Struct, reflect.Array, reflect.Slice, reflect.Map:
				d.out.WriteByte('&')
			}
			d.value(v.Elem())
		})
	case reflect.Struct:
		d.out.WriteByte('{')
		for i := range v.NumField() {
			if i > 0 {
				d.out.WriteByte(' ')
			}
			d.out.WriteString(v.Type().Field(i).Name + ":")
			d.value(v.Field(i))
		}
		d.out.WriteByte('}')
	case reflect.Slice, reflect.Array:
		d.within(v, func() {
			d.out.WriteByte('[')
			for i := range v.Len() {
				if i > 0 {
					d.out.WriteByte(' ')
				}
				d.value(v.Index(i))
			}
			d.out.WriteByte(']')
		})
	case reflect.Map:
		d.within(v, func() {
			entries := make([][2]string, 0, v.Len())
			for key, entry := range v.Seq2() {
				entries = append(entries, [2]string{d.part(key), d.part(entry)})
			}
			slices.SortFunc(entries, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
			d.out.WriteString("map[")
			for i, entry := range entries {
				if i > 0 {
					d.out.WriteByte(' ')
				}
				d.out.WriteString(entry[0] + ":" + entry[1])
			}
			d.out.WriteByte(']')
		})
	default:
		fmt.Fprintf(&d.out, "%+v", v)
	}
}

// within prints what print writes for v, a pointer, an array, a slice or a map, once per path: a
// pointer, slice or map met again inside its own printing prints as <cycle>. An array has no
// address of its own to come back to, and an empty slice or map holds nothing that could.
func (d *describer) within(v reflect.Value, print func()) {
	if v.Kind() == reflect.Array || (v.Kind() != reflect.Pointer && v.Len() == 0) {
		print()
		return
	}
	at := place{kind: v.Kind(), address: v.Pointer()}
	if v.Kind() == reflect.Slice {
		at.length = v.Len()
	}
	if d.path[at] {
		d.out.WriteString("<cycle>")
		return
	}
	d.path[at] = true
	defer delete(d.path, at)
	print()
}

// part is what d prints for v, as a string of its own, sharing d's path.
func (d *describer) part(v reflect.Value) string {
	inner := describer{path: d.path}
	inner.value(v)
	return inner.out.String()
}

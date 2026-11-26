package testwait

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// descriptionLimit is the most bytes a failed wait prints of its last reading. A failure's reading
// must be useful but not turn a shared or cyclic graph into megabytes of CI output.
const descriptionLimit = 4 << 10

// descriptionDepth is how many value containers a reading prints below its root.
const descriptionDepth = 8

// descriptionSliceValues is how many values of a slice or array a reading prints.
const descriptionSliceValues = 32

// Describe is how a give-up prints its last reading. It describes values the caller handed it by
// value: structs, arrays and the first values of a slice are walked only to descriptionDepth and
// descriptionSliceValues, and the whole text is cut at descriptionLimit. A pointer is never
// followed into a struct, slice, map or other live object: it prints its address, as %+v did.
// Pointers to a basic value, or to a value that prints by Format, Error or String, are followed;
// their caller is responsible for handing a snapshot when such a value can be written. A map is
// an object behind a reference too, so it prints its address rather than iterating live entries.
// This keeps a give-up from racing a writer it does not own; a caller that needs fields behind a
// pointer takes their snapshot under its own lock and hands that value to its wait.
func Describe(value any) string {
	if value == nil {
		return "<nil>"
	}
	d := describer{path: map[place]bool{}}
	d.value(reflect.ValueOf(value), 0)
	return d.out.String()
}

// describer is one Describe: its bounded text and the references it follows, so a pointer chain
// that reaches itself ends. type is part of place: a pointer to a struct and a pointer to that
// struct's first field can have the same address but are not the same reference.
type describer struct {
	out       strings.Builder
	path      map[place]bool
	truncated bool
}

type place struct {
	kind    reflect.Kind
	typ     reflect.Type
	address uintptr
	length  int
}

func (d *describer) text(text string) {
	if d.truncated {
		return
	}
	left := descriptionLimit - d.out.Len()
	if len(text) <= left {
		d.out.WriteString(text)
		return
	}
	const cut = "<truncated>"
	if left >= len(cut) {
		d.out.WriteString(cut)
	} else if left > 0 {
		d.out.WriteString(text[:left])
	}
	d.truncated = true
}

func (d *describer) value(v reflect.Value, depth int) {
	if d.truncated {
		return
	}
	if !v.IsValid() {
		d.text("<nil>")
		return
	}
	if depth > descriptionDepth {
		d.text("<depth>")
		return
	}
	if printsByMethod(v) {
		d.text(fmt.Sprintf("%+v", v.Interface()))
		return
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			d.text("<nil>")
			return
		}
		d.value(v.Elem(), depth+1)
	case reflect.Pointer:
		if v.IsNil() {
			d.text("<nil>")
			return
		}
		if followsPointer(v) {
			d.follow(v, depth)
			return
		}
		d.pointer(v)
	case reflect.Struct:
		d.text("{")
		for i := range v.NumField() {
			if i > 0 {
				d.text(" ")
			}
			d.text(v.Type().Field(i).Name + ":")
			d.value(v.Field(i), depth+1)
		}
		d.text("}")
	case reflect.Slice, reflect.Array:
		d.values(v, depth)
	case reflect.Map:
		// MapRange races a map writer. The map is not a value snapshot, so name only its reference.
		d.pointer(v)
	case reflect.Bool:
		d.text(strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		d.text(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		d.text(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		d.text(strconv.FormatFloat(v.Float(), 'g', -1, v.Type().Bits()))
	case reflect.Complex64, reflect.Complex128:
		d.text(fmt.Sprint(v.Complex()))
	case reflect.String:
		d.text(v.String())
	default:
		d.pointer(v)
	}
}

func printsByMethod(v reflect.Value) bool {
	if !v.CanInterface() {
		return false
	}
	switch v.Interface().(type) {
	case fmt.Formatter, error, fmt.Stringer:
		return true
	default:
		return false
	}
}

func followsPointer(v reflect.Value) bool {
	if printsByMethod(v) {
		return true
	}
	switch v.Elem().Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.String:
		return true
	default:
		return false
	}
}

func (d *describer) follow(v reflect.Value, depth int) {
	at := place{kind: v.Kind(), typ: v.Type(), address: v.Pointer()}
	if d.path[at] {
		d.text("<cycle>")
		return
	}
	d.path[at] = true
	defer delete(d.path, at)
	d.value(v.Elem(), depth+1)
}

func (d *describer) values(v reflect.Value, depth int) {
	at := place{kind: v.Kind(), typ: v.Type(), length: v.Len()}
	if v.Kind() == reflect.Slice && v.Len() > 0 {
		at.address = v.Pointer()
	}
	if at.address != 0 && d.path[at] {
		d.text("<cycle>")
		return
	}
	if at.address != 0 {
		d.path[at] = true
		defer delete(d.path, at)
	}
	d.text("[")
	limit := min(v.Len(), descriptionSliceValues)
	for i := range limit {
		if i > 0 {
			d.text(" ")
		}
		d.value(v.Index(i), depth+1)
	}
	if v.Len() > limit {
		if limit > 0 {
			d.text(" ")
		}
		d.text("<more>")
	}
	d.text("]")
}

func (d *describer) pointer(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.UnsafePointer:
		d.text(fmt.Sprintf("0x%x", v.Pointer()))
	case reflect.Func:
		d.text(fmt.Sprintf("0x%x", v.Pointer()))
	default:
		d.text("<" + v.Type().String() + ">")
	}
}

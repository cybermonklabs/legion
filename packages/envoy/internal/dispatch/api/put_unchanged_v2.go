package api

import (
	"reflect"
	"time"
)

// putSetter is who set what a /api/v2 PUT would set again: the caller its object records as having
// set its present state and fields (setBy), or anyone, for an object that records no setter
// (noSetterRecorded).
type putSetter struct {
	who    string
	anyone bool
}

// setBy is the setter an object records, in the form the handler compares its caller in: the
// canonical login of the person who closed an ask or set a comment's state, the actor that
// accepted a delivery attempt, the holder of a claim. An empty who is a setter nobody matches.
func setBy(who string) putSetter {
	return putSetter{who: who}
}

// noSetterRecorded is an object that records no setter - a follower, a repository mapping, an
// architecture source - whose equal PUT is a no-op whoever sends it, since no caller is recorded as
// having set it.
var noSetterRecorded = putSetter{anyone: true}

// putChangesNothing reports whether a /api/v2 PUT is the no-op of dispatch://INFRA-4071/spec R8:
// its whole body, sent, equals the object's current state and fields, current, and caller is the
// setter the object records, or the object records none. Such a PUT answers 200 with the object
// and writes nothing - no row, no event, no delivery - and any other is a change, which its
// handler makes as it makes every change: an answer that replaces an earlier one writes its event,
// and a first answer from another person to an answered ask stays 409 ASK_ANSWERED.
//
// current is the object's present state written in the body's own type T, by the handler that
// read it; a member of the body that is no field of the object - a precondition such as
// expected_answer_at - is copied from sent into current before the call, so the comparison is of
// the object alone. The two are compared member by member: a nil and an empty slice or map are
// equal, two times are equal at one instant whatever zone each is written in, and two pointers
// are equal where both are nil or what they point to is.
//
// Every /api/v2 PUT answers 200 with the object, the follower PUT included, which /api/v1 answers
// 204.
func putChangesNothing[T any](sent, current T, setter putSetter, caller string) bool {
	if !setter.anyone && (setter.who == "" || setter.who != caller) {
		return false
	}
	return sameMembers(reflect.ValueOf(&sent).Elem(), reflect.ValueOf(&current).Elem())
}

// sameMembers reports whether two values of one type hold the same state, as putChangesNothing
// compares a body with an object's present state.
func sameMembers(sent, current reflect.Value) bool {
	if sent.Type() == timeType {
		return sent.Interface().(time.Time).Equal(current.Interface().(time.Time))
	}
	switch sent.Kind() {
	case reflect.Pointer, reflect.Interface:
		if sent.IsNil() || current.IsNil() {
			return sent.IsNil() == current.IsNil()
		}
		if sent.Kind() == reflect.Interface && sent.Elem().Type() != current.Elem().Type() {
			return false
		}
		return sameMembers(sent.Elem(), current.Elem())
	case reflect.Struct:
		// An unexported field is none of a body's members, since encoding/json neither reads
		// nor writes one, but an embedded struct's are, which it promotes.
		for index := range sent.NumField() {
			field := sent.Type().Field(index)
			if (field.IsExported() || field.Anonymous) && !sameMembers(sent.Field(index), current.Field(index)) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Array:
		if sent.Len() != current.Len() {
			return false
		}
		for index := range sent.Len() {
			if !sameMembers(sent.Index(index), current.Index(index)) {
				return false
			}
		}
		return true
	case reflect.Map:
		if sent.Len() != current.Len() {
			return false
		}
		for _, key := range sent.MapKeys() {
			other := current.MapIndex(key)
			if !other.IsValid() || !sameMembers(sent.MapIndex(key), other) {
				return false
			}
		}
		return true
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return false
	}
	return sent.Equal(current)
}

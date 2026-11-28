package asof

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
)

// A past-time answer of a route says which cut it answers, and which members it leaves out
// (decisions 4 and 5 of the plan). The route names each member that it leaves out by its place
// in the answer: an RFC 6901 JSON Pointer whose array members are numbers (/open_asks/1/revision)
// or *, which stands for every member of the array. WriteHeaders names the cut and, for each
// such member, the pointer of AbsentFields that holds it. OmitAbsent takes the members out of
// the encoded answer, so that each one is omitted and never written null.

// AbsentPointer is the pointer of AbsentFields[route] that names member, and false when the
// table holds no pointer for it. A segment * of the table's pointer names any member of an
// array: a number, or * itself. Every other segment names only itself.
func AbsentPointer(route, member string) (string, bool) {
	segments := strings.Split(member, "/")
	for _, field := range AbsentFields[route] {
		if pointerNames(strings.Split(field.Pointer, "/"), segments) {
			return field.Pointer, true
		}
	}
	return "", false
}

// pointerNames reports whether the segments of a pointer of AbsentFields name the segments of a
// member.
func pointerNames(pointer, member []string) bool {
	if len(pointer) != len(member) {
		return false
	}
	for index, segment := range pointer {
		if segment == member[index] {
			continue
		}
		if segment != "*" || !isArrayIndex(member[index]) {
			return false
		}
	}
	return true
}

// isArrayIndex reports whether segment is an RFC 6901 array index: 0, or digits that do not begin
// with 0.
func isArrayIndex(segment string) bool {
	if segment == "" || (len(segment) > 1 && segment[0] == '0') {
		return false
	}
	for index := range len(segment) {
		if !isDigit(segment[index]) {
			return false
		}
	}
	return true
}

// WriteHeaders sets on header the headers of a past-time answer of route at cut, before its body:
// Dispatch-As-Of (contracts.DispatchAsOfHeader), the cut, and, when the answer leaves out any
// member, Dispatch-As-Of-Absent (contracts.DispatchAsOfAbsentHeader), the pointer of
// AbsentFields that holds each member (AbsentPointer), each once (HeaderValue).
//
// A member that AbsentFields does not hold for route is a programming error. Then WriteHeaders
// sets no Dispatch-As-Of-Absent and returns an error that names each such member: an answer names
// only what the table holds for its route.
func WriteHeaders(header http.Header, route string, cut Cut, members []string) error {
	header.Set(contracts.DispatchAsOfHeader, cut.String())
	if len(members) == 0 {
		return nil
	}
	pointers := make([]string, 0, len(members))
	var unheld []string
	for _, member := range members {
		pointer, held := AbsentPointer(route, member)
		if !held {
			unheld = append(unheld, member)
			continue
		}
		pointers = append(pointers, pointer)
	}
	if len(unheld) > 0 {
		return fmt.Errorf("asof: AbsentFields holds no pointer for %s, which the answer of %s leaves out",
			strings.Join(unheld, ", "), route)
	}
	header.Set(contracts.DispatchAsOfAbsentHeader, HeaderValue(pointers))
	return nil
}

// OmitAbsent is body, one JSON value, with each member that a pointer of members names taken out
// of the object that holds it. Every other byte of body stays as it is: the order of the members,
// their spelling and the whitespace after the value. A member that body does not hold is not an
// error, since an answer leaves out a member at the places where the records do not give it. A
// pointer that names an element of an array, which is no member of an object, is an error, and
// so is a body that is not one JSON value.
func OmitAbsent(body []byte, members []string) ([]byte, error) {
	if len(members) == 0 {
		return body, nil
	}
	paths := make([][]string, 0, len(members))
	for _, member := range members {
		if !strings.HasPrefix(member, "/") {
			return nil, fmt.Errorf("asof: %q is not a JSON pointer to a member", member)
		}
		segments := strings.Split(member[1:], "/")
		for index, segment := range segments {
			segments[index] = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		}
		paths = append(paths, segments)
	}
	value := bytes.TrimRight(body, " \t\r\n")
	omitted, err := omitMembers(value, paths)
	if err != nil {
		return nil, err
	}
	return append(omitted, body[len(value):]...), nil
}

// omitMembers is value, one JSON value, with the members that paths name taken out. Each path is
// the unescaped segments of a pointer below value.
func omitMembers(value []byte, paths [][]string) ([]byte, error) {
	trimmed := bytes.TrimLeft(value, " \t\r\n")
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("asof: the answer holds no JSON value")
	}
	switch trimmed[0] {
	case '{':
		return omitObjectMembers(trimmed, paths)
	case '[':
		return omitArrayMembers(trimmed, paths)
	}
	// A pointer that goes on below a string, a number, a boolean or null reaches nothing.
	return value, nil
}

// omitObjectMembers is the object value with the members that paths name taken out, and each
// path that goes on below a member applied to that member.
func omitObjectMembers(value []byte, paths [][]string) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("asof: read the answer's object: %w", err)
	}
	out := make([]byte, 0, len(value))
	out = append(out, '{')
	written := false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("asof: read a member name of the answer: %w", err)
		}
		name, _ := token.(string)
		var member json.RawMessage
		if err := decoder.Decode(&member); err != nil {
			return nil, fmt.Errorf("asof: read the answer's member %q: %w", name, err)
		}
		omitted := false
		var below [][]string
		for _, path := range paths {
			if path[0] != name {
				continue
			}
			if len(path) == 1 {
				omitted = true
				break
			}
			below = append(below, path[1:])
		}
		if omitted {
			continue
		}
		if len(below) > 0 {
			if member, err = omitMembers(member, below); err != nil {
				return nil, err
			}
		}
		if written {
			out = append(out, ',')
		}
		written = true
		encodedName, err := json.Marshal(name)
		if err != nil {
			return nil, fmt.Errorf("asof: write the answer's member name %q: %w", name, err)
		}
		out = append(out, encodedName...)
		out = append(out, ':')
		out = append(out, member...)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("asof: read the end of the answer's object: %w", err)
	}
	return append(out, '}'), nil
}

// omitArrayMembers is the array value with each path applied to the elements it names: the
// element its number names, or every element for *. The paths are grouped once, before the
// elements are read: the paths of * apply to each element, and the paths of a number only to
// the element of that number.
func omitArrayMembers(value []byte, paths [][]string) ([]byte, error) {
	var every [][]string
	numbered := map[string][][]string{}
	for _, path := range paths {
		if len(path) == 1 {
			return nil, fmt.Errorf("asof: the pointer segment %q names an element of an array, not a member of an object", path[0])
		}
		if path[0] == "*" {
			every = append(every, path[1:])
		} else {
			numbered[path[0]] = append(numbered[path[0]], path[1:])
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("asof: read the answer's array: %w", err)
	}
	out := make([]byte, 0, len(value))
	out = append(out, '[')
	for index := 0; decoder.More(); index++ {
		var element json.RawMessage
		if err := decoder.Decode(&element); err != nil {
			return nil, fmt.Errorf("asof: read element %d of the answer's array: %w", index, err)
		}
		below := every
		if len(numbered) > 0 {
			if own := numbered[strconv.Itoa(index)]; len(own) > 0 {
				below = append(slices.Clip(every), own...)
			}
		}
		if len(below) > 0 {
			var err error
			if element, err = omitMembers(element, below); err != nil {
				return nil, err
			}
		}
		if index > 0 {
			out = append(out, ',')
		}
		out = append(out, element...)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("asof: read the end of the answer's array: %w", err)
	}
	return append(out, ']'), nil
}

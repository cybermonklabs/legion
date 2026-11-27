// legion_board_json.go writes the Legion board's issues entries (legionBoardFacts and the
// legionBoardGrowth inside each) without reflection. A board of INFRA's size carries thousands
// of entries, every one of them encoded on each GET and on each recompute of an open `legion`
// view, where the live hub writes the body from the entries' own encodings
// (legionBoardResponse's encodedMap, composeDeliveryBody) rather than reading them back out of
// the body's.
//
// Each method writes exactly the bytes encoding/json writes by reflection for the same value, so
// the GET's body and the live push's stay one body however either is encoded: the fields in
// their declared order under their tags' names, null for a nil pointer or map, a map's keys in
// sorted order, each float as encoding/json writes a float64, each time as its own location
// writes it in RFC 3339 with its nanoseconds (time.Time's MarshalJSON), and each string as
// encoding/json escapes it. A value encoding/json refuses — NaN, an infinity, a year outside
// [0,9999], an offset of a day or more — is refused here too, so no body holds what no reader
// could parse. TestAnIssuesEntryIsWrittenAsEncodingJSONWritesItsFields holds the bytes to
// encoding/json's, field by field and edge by edge.
package api

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"
)

// legionBoardFactsBytes is the room an entry's encoding is begun with: an open issue's entry,
// with its moments to the nanosecond, its four columns and its growth, takes about 600 bytes.
const legionBoardFactsBytes = 640

// MarshalJSON writes the entry as encoding/json writes its fields (legion_board_json.go).
func (f legionBoardFacts) MarshalJSON() ([]byte, error) {
	w := entryJSON{out: make([]byte, 0, legionBoardFactsBytes)}
	w.out = append(w.out, `{"opened_at":`...)
	w.time(f.OpenedAt)
	w.out = append(w.out, `,"closed_at":`...)
	w.moment(f.ClosedAt)
	w.out = append(w.out, `,"hours_by_phase":`...)
	w.hours(f.HoursByPhase)
	w.out = append(w.out, `,"work_hours":`...)
	w.float(f.WorkHours)
	w.out = append(w.out, `,"review_rounds":`...)
	w.out = strconv.AppendInt(w.out, int64(f.ReviewRounds), 10)
	w.out = append(w.out, `,"root_key":`...)
	w.out = appendJSONString(w.out, f.RootKey)
	w.out = append(w.out, `,"tree_depth":`...)
	w.out = strconv.AppendInt(w.out, int64(f.TreeDepth), 10)
	w.out = append(w.out, `,"slot_hours":`...)
	w.float(f.SlotHours)
	w.out = append(w.out, `,"waiting_reason":`...)
	w.out = appendJSONString(w.out, f.WaitingReason)
	w.out = append(w.out, `,"work_first_at":`...)
	w.moment(f.WorkFirstAt)
	w.out = append(w.out, `,"work_last_at":`...)
	w.moment(f.WorkLastAt)
	w.out = append(w.out, `,"updated_at":`...)
	w.time(f.UpdatedAt)
	w.out = append(w.out, `,"as_of":`...)
	w.moment(f.AsOf)
	w.out = append(w.out, `,"growth":`...)
	if f.Growth == nil {
		w.out = append(w.out, "null"...)
	} else {
		f.Growth.write(&w)
	}
	w.out = append(w.out, '}')
	return w.done()
}

// MarshalJSON writes the growth as encoding/json writes its fields (legion_board_json.go).
func (g legionBoardGrowth) MarshalJSON() ([]byte, error) {
	w := entryJSON{out: make([]byte, 0, 128)}
	g.write(&w)
	return w.done()
}

// write appends the growth's encoding to w: its own encoding, and the growth field of its entry's.
func (g legionBoardGrowth) write(w *entryJSON) {
	w.out = append(w.out, `{"hours_by_phase":`...)
	w.hours(g.HoursByPhase)
	w.out = append(w.out, `,"work_hours":`...)
	w.float(g.WorkHours)
	w.out = append(w.out, `,"slot_hours":`...)
	w.float(g.SlotHours)
	w.out = append(w.out, `,"work_last_at":`...)
	w.float(g.WorkLastAt)
	w.out = append(w.out, '}')
}

// entryJSON is one encoding as it is appended, and the first value it met that encoding/json
// refuses, which the encoding then answers in place of its bytes.
type entryJSON struct {
	out []byte
	err error
}

// done is the encoding, or the first value it refused.
func (w *entryJSON) done() ([]byte, error) {
	if w.err != nil {
		return nil, w.err
	}
	return w.out, nil
}

// refuse keeps the first value the encoding refused.
func (w *entryJSON) refuse(err error) {
	if w.err == nil {
		w.err = err
	}
}

// float appends value as encoding/json writes a float64: the shortest decimal that reads back
// as the same value, in plain form, but in exponent form below 1e-6 and from 1e21 up, its
// exponent with no leading zero (1e-7, not 1e-07). It refuses NaN and the infinities, which JSON
// has no number for.
func (w *entryJSON) float(value float64) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		w.refuse(errors.New("json: unsupported value: " + strconv.FormatFloat(value, 'g', -1, 64)))
		return
	}
	format := byte('f')
	if abs := math.Abs(value); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	w.out = strconv.AppendFloat(w.out, value, format, -1, 64)
	if n := len(w.out); format == 'e' && w.out[n-4] == 'e' && w.out[n-3] == '-' && w.out[n-2] == '0' {
		w.out[n-2] = w.out[n-1]
		w.out = w.out[:n-1]
	}
}

// time appends at as time.Time's MarshalJSON writes it: RFC 3339 with the nanoseconds it holds,
// in at's own location. It refuses what that method refuses: a year outside [0,9999], which RFC
// 3339's four digits cannot write, and an offset of a whole day or more.
func (w *entryJSON) time(at time.Time) {
	if year := at.Year(); year < 0 || year > 9999 {
		w.refuse(errors.New("Time.MarshalJSON: year outside of range [0,9999]"))
		return
	}
	if _, offset := at.Zone(); offset <= -24*3600 || offset >= 24*3600 {
		w.refuse(errors.New("Time.MarshalJSON: timezone hour outside of range [0,23]"))
		return
	}
	w.out = append(at.AppendFormat(append(w.out, '"'), time.RFC3339Nano), '"')
}

// moment appends at's time, or null where there is none.
func (w *entryJSON) moment(at *time.Time) {
	if at == nil {
		w.out = append(w.out, "null"...)
		return
	}
	w.time(*at)
}

// hours appends a map of hours by column id, its ids in sorted order, or null for a nil map.
func (w *entryJSON) hours(hours map[string]float64) {
	if hours == nil {
		w.out = append(w.out, "null"...)
		return
	}
	// The board has twelve columns, so the ids sort on the stack.
	var held [16]string
	columns := held[:0]
	for column := range hours {
		columns = append(columns, column)
	}
	slices.Sort(columns)
	w.out = append(w.out, '{')
	for index, column := range columns {
		if index > 0 {
			w.out = append(w.out, ',')
		}
		w.out = append(appendJSONString(w.out, column), ':')
		w.float(hours[column])
	}
	w.out = append(w.out, '}')
}

// appendJSONString appends s to out as encoding/json writes a string, a map's keys among them. A
// string of ASCII that holds no control character and none of the five characters encoding/json
// escapes (the quote, the backslash and the three it escapes for HTML) is written between quotes
// as it stands, which is every column id, issue key, root key and waiting reason Legion writes;
// any other is written by encoding/json itself, so its escapes and its handling of invalid UTF-8
// are that package's own.
func appendJSONString(out []byte, s string) []byte {
	for index := range len(s) {
		if c := s[index]; c < ' ' || c >= utf8.RuneSelf || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			// A string always encodes: encoding/json writes invalid UTF-8 as the replacement
			// character rather than refusing it.
			quoted, _ := json.Marshal(s)
			return append(out, quoted...)
		}
	}
	return append(append(append(out, '"'), s...), '"')
}

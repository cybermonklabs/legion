package legionfacts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"
)

// decodeNumbers decodes a stored object of numbers, a row's hours by column (hours_by_phase) or
// its growth, into the map encoding/json decodes it to, without encoding/json's reflection: the
// board's reads decode both columns of every row they answer, thousands of rows a read, and
// json.Unmarshal of the two was about half of the time those reads spent reading their rows.
// BenchmarkReadOpenAtInfrasSize, 2,600 open rows, measured a median of 54.7 ms a read with
// json.Unmarshal and 44.2 ms with this, six runs of each in two interleaved rounds (2026-11-27, in
// a pod of two shared cores).
//
// It reads the object JSON writes, in any whitespace, each key a string and each value a number
// as JSON writes one, and answers each number as encoding/json does, the nearest float64
// (strconv.ParseFloat), and each key as encoding/json decodes it, a key written twice keeping its
// last value. Anything else it refuses with an error: null and a null value among them, which
// encoding/json would take, since neither column holds either (each is `jsonb not null default
// '{}'`, written from a Go map of numbers by upsert's storedMap). The empty object is an empty
// map, never nil, as encoding/json answers it.
// TestAStoredObjectOfNumbersDecodesAsEncodingJSONDecodesIt holds it to encoding/json's answer.
func decodeNumbers(stored []byte) (map[string]float64, error) {
	r := numbersReader{in: stored}
	r.space()
	if !r.next('{') {
		return nil, r.refuse("an object")
	}
	// Every member holds one colon outside its strings, so their count bounds the members from
	// above, and the map is made once at its size.
	values := make(map[string]float64, bytes.Count(stored, []byte{':'}))
	r.space()
	if r.next('}') {
		return values, r.end()
	}
	for {
		key, err := r.key()
		if err != nil {
			return nil, err
		}
		r.space()
		if !r.next(':') {
			return nil, r.refuse("a colon after a key")
		}
		r.space()
		value, err := r.number()
		if err != nil {
			return nil, err
		}
		values[key] = value
		r.space()
		if r.next(',') {
			r.space()
			continue
		}
		if !r.next('}') {
			return nil, r.refuse("a comma or the object's close")
		}
		return values, r.end()
	}
}

// numbersReader is decodeNumbers' place in its input.
type numbersReader struct {
	in []byte
	at int
}

// refuse is the error for input that is not what the reader wants next, which want names.
func (r *numbersReader) refuse(want string) error {
	if r.at >= len(r.in) {
		return fmt.Errorf("decode a stored object of numbers: the input ends where it wants %s", want)
	}
	return fmt.Errorf("decode a stored object of numbers: %q at byte %d, where it wants %s", r.in[r.at], r.at, want)
}

// space passes JSON's whitespace.
func (r *numbersReader) space() {
	for r.at < len(r.in) {
		switch r.in[r.at] {
		case ' ', '\t', '\n', '\r':
			r.at++
		default:
			return
		}
	}
}

// next passes c where it is the next byte, and answers whether it was.
func (r *numbersReader) next(c byte) bool {
	if r.at < len(r.in) && r.in[r.at] == c {
		r.at++
		return true
	}
	return false
}

// end refuses anything but whitespace after the object.
func (r *numbersReader) end() error {
	r.space()
	if r.at != len(r.in) {
		return r.refuse("nothing after the object")
	}
	return nil
}

// key reads a member's key. A key of ASCII that holds no escape and no control character is the
// bytes between its quotes, which is every key Legion writes; any other is decoded by
// encoding/json itself, so its escapes, its surrogates and its control characters are that
// package's to read or refuse.
func (r *numbersReader) key() (string, error) {
	if !r.next('"') {
		return "", r.refuse("a key")
	}
	start, plain := r.at, true
	for r.at < len(r.in) {
		switch c := r.in[r.at]; {
		case c == '"':
			r.at++
			if plain {
				return string(r.in[start : r.at-1]), nil
			}
			var key string
			if err := json.Unmarshal(r.in[start-1:r.at], &key); err != nil {
				return "", fmt.Errorf("decode a stored object of numbers: the key %s: %w", r.in[start-1:r.at], err)
			}
			return key, nil
		case c == '\\':
			// An escape: the byte after the backslash, a quote among them, is the escape's.
			plain = false
			r.at += 2
		case c < ' ' || c >= utf8.RuneSelf:
			plain = false
			r.at++
		default:
			r.at++
		}
	}
	r.at = len(r.in)
	return "", r.refuse("a key's close")
}

// number reads a value, a number as JSON writes one: an optional minus, an integer part with no
// leading zero, an optional fraction and an optional exponent, each of at least one digit.
func (r *numbersReader) number() (float64, error) {
	start := r.at
	r.next('-')
	switch {
	case r.next('0'):
	case r.digits() == 0:
		return 0, r.refuse("a number")
	}
	if r.next('.') && r.digits() == 0 {
		return 0, r.refuse("a digit after a number's point")
	}
	if r.next('e') || r.next('E') {
		if !r.next('+') {
			r.next('-')
		}
		if r.digits() == 0 {
			return 0, r.refuse("a digit of a number's exponent")
		}
	}
	value, err := strconv.ParseFloat(string(r.in[start:r.at]), 64)
	if errors.Is(err, strconv.ErrRange) {
		return 0, fmt.Errorf("decode a stored object of numbers: %s is past a float64", r.in[start:r.at])
	}
	return value, err
}

// digits passes a run of decimal digits, and answers how many it passed.
func (r *numbersReader) digits() int {
	start := r.at
	for r.at < len(r.in) && '0' <= r.in[r.at] && r.in[r.at] <= '9' {
		r.at++
	}
	return r.at - start
}

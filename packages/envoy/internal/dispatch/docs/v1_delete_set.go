package docs

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"github.com/reearth/ygo/crdt"
)

// eachV1DeleteRange calls each with every range of update's delete set, update being a V1 update,
// in the order the update holds them, and reports whether the update read through to its delete
// set's end. A V1 update holds its structs first and its delete set after them, so the structs are
// read too, but only to pass them: each struct's header, its origins or its parent, and the length
// of its content, with no content decoded, no value built and nothing allocated. The library has
// no such reader: crdt.ParseUpdateMeta reads the structs and never the delete set, and
// crdt.ContentIDsFromUpdateV1, the one reader of an update's delete set it offers, decodes every
// struct and value into a document of its own first, with no bound on the values it builds.
//
// It passes a struct as the library's own scan of a frame passes one (pendingScanner.item and
// .content, crdt/pending_budget.go:758-960 in theorem-labs/ygo v1.55.1-theorem.9), so the two read
// the same bytes as the same structs: a GC or Skip run is its length; an item is its info byte, its
// origin and right origin where the byte says it has them and its parent where it has neither, and
// then its content by the byte's low five bits - a deletion's length, a JSON item's values as JSON
// text and, where the first does not read as JSON and starts as a lib0 Any value does, as lib0 Any
// values, as Yjs builds before the JSON text wrote them; binary and string content, an embed and a
// format as length-prefixed bytes; a type's reference and, for an element or a hook, its name; Any
// content as lib0 Any values; a subdocument's guid and options; and a move's three numbers. A
// struct of any other kind, or bytes that end before the delete set does, read as false, and so
// does an Any value nested past the depth the library reads (maxAnyDepth).
//
// What it reads costs a pass over the update's bytes and nothing else, so a caller may read it
// where it reads the update's header (refusedBeforeApply, carriesBeyond), under the load slot the
// hook takes for an update past unslottedApplyBytes (weighPeerUpdate).
func eachV1DeleteRange(update []byte, each func(client crdt.ClientID, clock, length uint64)) bool {
	r := v1Reader{rest: update, ok: true}
	for groups := r.uint(); groups > 0 && r.ok; groups-- {
		structs := r.uint()
		r.uint() // the group's client
		r.uint() // the clock its first struct starts at
		for ; structs > 0 && r.ok; structs-- {
			r.passStruct()
		}
	}
	for clients := r.uint(); clients > 0 && r.ok; clients-- {
		client := crdt.ClientID(r.uint())
		for ranges := r.uint(); ranges > 0 && r.ok; ranges-- {
			clock, length := r.uint(), r.uint()
			if r.ok {
				each(client, clock, length)
			}
		}
	}
	return r.ok
}

// maxAnyDepth is how deep eachV1DeleteRange follows lib0 Any arrays and maps inside one another:
// the depth past which the library's own reader refuses a value (anycodec.MaxDepth, 100, in
// theorem-labs/ygo v1.55.1-theorem.9), so a value it refuses does not read here either.
const maxAnyDepth = 100

// The low five bits of a V1 struct's info byte that name what it is (crdt/update.go's wire tags in
// theorem-labs/ygo v1.55.1-theorem.9), and the three high bits that say what an item carries.
const (
	v1GC       = 0
	v1Deleted  = 1
	v1JSON     = 2
	v1Binary   = 3
	v1String   = 4
	v1Embed    = 5
	v1Format   = 6
	v1Type     = 7
	v1Any      = 8
	v1Doc      = 9
	v1Skip     = 10
	v1Move     = 11
	v1Origin   = 0x80
	v1Right    = 0x40
	v1Keyed    = 0x20
	v1TagBits  = 0x1f
	v1TypeRefs = 6 // YXmlText, the highest type reference Yjs defines
)

// v1Reader reads a V1 update's bytes from rest, and ok goes false at the first that do not read.
type v1Reader struct {
	rest []byte
	ok   bool
}

// uint reads a lib0 varuint, which binary.Uvarint reads as it is written.
func (r *v1Reader) uint() uint64 {
	if !r.ok {
		return 0
	}
	value, read := binary.Uvarint(r.rest)
	if read <= 0 {
		r.ok = false
		return 0
	}
	r.rest = r.rest[read:]
	return value
}

// byte reads one byte.
func (r *v1Reader) byte() byte {
	if !r.ok || len(r.rest) == 0 {
		r.ok = false
		return 0
	}
	b := r.rest[0]
	r.rest = r.rest[1:]
	return b
}

// passVarint passes a lib0 varint: a sign and six bits in its first byte and seven in each byte
// after, eight bytes at most, as the library reads one (anycodec.Decoder.ReadVarInt).
func (r *v1Reader) passVarint() {
	for range 8 {
		if r.byte() < 0x80 {
			return
		}
	}
	r.ok = false
}

// skip passes n bytes.
func (r *v1Reader) skip(n uint64) {
	if !r.ok || n > uint64(len(r.rest)) {
		r.ok = false
		return
	}
	r.rest = r.rest[n:]
}

// bytes reads lib0 length-prefixed bytes - a string, a JSON text, a binary value - and answers
// them, a slice of the update.
func (r *v1Reader) bytes() []byte {
	n := r.uint()
	if !r.ok || n > uint64(len(r.rest)) {
		r.ok = false
		return nil
	}
	read := r.rest[:n]
	r.rest = r.rest[n:]
	return read
}

// passStruct passes one struct of a client group.
func (r *v1Reader) passStruct() {
	info := r.byte()
	tag := info & v1TagBits
	if tag == v1GC || tag == v1Skip {
		r.uint()
		return
	}
	if info&v1Origin != 0 {
		r.uint()
		r.uint()
	}
	if info&v1Right != 0 {
		r.uint()
		r.uint()
	}
	if info&(v1Origin|v1Right) == 0 {
		if r.byte() == 1 {
			r.bytes() // a root, by its name
		} else {
			r.uint() // the item it is inside, by its id
			r.uint()
		}
		if info&v1Keyed != 0 {
			r.bytes()
		}
	}
	r.passContent(tag)
}

// passContent passes an item's content of kind tag.
func (r *v1Reader) passContent(tag byte) {
	switch tag {
	case v1Deleted:
		r.uint()
	case v1JSON:
		n := r.uint()
		if n > uint64(len(r.rest)) {
			r.ok = false
			return
		}
		start := r.rest
		if r.passJSON(n) {
			return
		}
		// Not JSON text: an earlier Yjs build wrote the same values as lib0 Any, which the
		// library reads where the bytes start as one does (decodeContent's fallback).
		r.rest, r.ok = start, len(start) > 0 && start[0] >= 116 && start[0] <= 127
		for ; n > 0 && r.ok; n-- {
			r.passAny(0)
		}
	case v1Binary, v1String, v1Embed:
		r.bytes()
	case v1Format:
		r.bytes()
		r.bytes()
	case v1Type:
		ref := r.byte()
		if ref > v1TypeRefs {
			r.ok = false
		}
		if ref == 3 || ref == 5 {
			r.bytes() // an element's or a hook's name
		}
	case v1Any:
		for n := r.uint(); n > 0 && r.ok; n-- {
			r.passAny(0)
		}
	case v1Doc:
		r.bytes()
		r.passAny(0)
	case v1Move:
		r.uint()
		r.uint()
		r.uint()
	default:
		r.ok = false
	}
}

// passJSON passes n JSON texts and reports whether each read as one, as the library's frame
// scan judges them (validatePendingJSON): valid UTF-8, and JSON whose every number parses as a
// float64, or the text "undefined". It leaves r where the texts end only where all of them read.
func (r *v1Reader) passJSON(n uint64) bool {
	for ; n > 0; n-- {
		raw := r.bytes()
		if !r.ok || !utf8.Valid(raw) {
			return false
		}
		if bytes.Equal(raw, []byte("undefined")) {
			continue
		}
		if !json.Valid(raw) || !jsonNumbersParse(raw) {
			return false
		}
	}
	return true
}

// jsonNumbersParse reports whether every number of raw, valid JSON, parses as a float64.
func jsonNumbersParse(raw []byte) bool {
	for j := 0; j < len(raw); j++ {
		switch c := raw[j]; {
		case c == '"':
			for j++; raw[j] != '"'; j++ {
				if raw[j] == '\\' {
					j++
				}
			}
		case c == '-' || c >= '0' && c <= '9':
			start := j
			for j < len(raw) && (raw[j] >= '0' && raw[j] <= '9' || raw[j] == '-' || raw[j] == '+' || raw[j] == '.' || raw[j] == 'e' || raw[j] == 'E') {
				j++
			}
			if _, err := strconv.ParseFloat(string(raw[start:j]), 64); err != nil {
				return false
			}
			j--
		}
	}
	return true
}

// passAny passes one lib0 Any value, nested depth values deep.
func (r *v1Reader) passAny(depth int) {
	if depth > maxAnyDepth {
		r.ok = false
		return
	}
	switch tag := r.byte(); tag {
	case 127, 126, 121, 120: // undefined, null, false, true
	case 125: // an integer, a lib0 varint
		r.passVarint()
	case 124: // a float32
		r.skip(4)
	case 123, 122: // a float64, a bigint64
		r.skip(8)
	case 119: // a string
		if !utf8.Valid(r.bytes()) {
			r.ok = false
		}
	case 118: // a map: its entries, each a key and a value
		for n := r.uint(); n > 0 && r.ok; n-- {
			r.bytes()
			r.passAny(depth + 1)
		}
	case 117: // an array
		for n := r.uint(); n > 0 && r.ok; n-- {
			r.passAny(depth + 1)
		}
	case 116: // bytes
		r.bytes()
	default:
		r.ok = false
	}
}

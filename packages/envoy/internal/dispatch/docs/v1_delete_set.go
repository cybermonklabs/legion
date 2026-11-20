package docs

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"github.com/reearth/ygo/crdt"
)

// eachV1Range reads update, a V1 update, in one pass: it calls structs with each struct's client,
// the clock it starts at and the clocks it takes, and deletes with every range of its delete set,
// each in the order the update holds them, and reports whether the update read through to its
// delete set's end. A V1 update holds its structs first and its delete set after them. A struct
// is read only to pass it: its header, its origins or its parent, and the length of its content,
// with no content decoded, no value built and nothing allocated. The library has no such reader:
// crdt.ParseUpdateMeta reads the structs and never the delete set, and keeps no more of a client's
// structs than the lowest clock they start at and the highest they reach, and
// crdt.ContentIDsFromUpdateV1, the one reader of an update's struct ranges and delete set it
// offers, decodes every struct and value into a document of its own first, with no bound on the
// values it builds. Each callback is called as the reader reaches what it reads, before it has
// read the bytes after that, so what a callback was called with is the update's only where the
// reader reports true.
//
// A struct takes the clocks the library's decoder counts for it (decodeStructsV1 and
// contentIDsFromStructs, crdt/merge.go and crdt/contentmap.go in theorem-labs/ygo
// v1.55.1-theorem.9): a GC run its length, and an item its content's length - a deletion's
// length, a JSON or Any item's count of values, a string's UTF-16 units, and one for content of
// any other kind. A Skip run, which only stands for clocks the update does not hold, and a struct
// of no clocks are passed and not called, as the decoder keeps neither. A struct reaching past
// the highest clock the decoder lets one reach (maxV1Clock) reads as false. A client named by two
// groups has the structs of both called, where the decoder keeps the later group's alone; Yjs and
// ygo write each client's structs as one group.
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
// where it reads the update's header (refusedBeforeApply, parkedNeeds, heldAgainst), under the
// load slot the hook takes for an update past unslottedApplyBytes or declaring more than
// unslottedApplyStructs structs (weighPeerUpdate).
func eachV1Range(update []byte, structs, deletes func(client crdt.ClientID, clock, length uint64)) bool {
	r := v1Reader{rest: update, ok: true}
	for groups := r.uint(); groups > 0 && r.ok; groups-- {
		count := r.uint()
		client := crdt.ClientID(r.uint())
		clock := r.uint()
		for ; count > 0 && r.ok; count-- {
			length, skip := r.passStruct()
			if !r.ok || clock > maxV1Clock || length > maxV1Clock+1-clock {
				r.ok = false
				break
			}
			if !skip && length > 0 {
				structs(client, clock, length)
			}
			clock += length
		}
	}
	for clients := r.uint(); clients > 0 && r.ok; clients-- {
		client := crdt.ClientID(r.uint())
		for ranges := r.uint(); ranges > 0 && r.ok; ranges-- {
			clock, length := r.uint(), r.uint()
			if r.ok {
				deletes(client, clock, length)
			}
		}
	}
	return r.ok
}

// declaredStructs is how many structs update, a V1 update, declares in the headers of its client
// groups, each group's count held to half the bytes left after its header, since a struct takes
// two bytes at least. It is what the library's apply decodes of the update: it reads each group's
// structs one by one, Skip runs included, counting each against its value limit before it reads it,
// and reserves a one-client update's list of structs for its header's count held to the same half
// of the bytes left (decodeStructs, crdt/update.go:912-948 in theorem-labs/ygo
// v1.55.1-theorem.11). It passes each struct as eachV1Range does, building nothing, and stops where
// the bytes stop reading, past which the decoder reads nothing either: it refuses the update there.
// So the hook can weigh what an apply's decode will build before it decodes anything
// (weighPeerUpdate).
func declaredStructs(update []byte) uint64 {
	r := v1Reader{rest: update, ok: true}
	var structs uint64
	for groups := r.uint(); groups > 0 && r.ok; groups-- {
		count := r.uint()
		r.uint() // the group's client
		r.uint() // and the clock its first struct starts at
		if !r.ok {
			break
		}
		structs += min(count, uint64(len(r.rest)/2))
		for ; count > 0 && r.ok; count-- {
			r.passStruct()
		}
	}
	return structs
}

// maxV1Clock is the highest clock the library's decoder lets a struct reach, 2^53 - 1, the largest
// integer a JavaScript number holds exactly (structEnd, crdt/update.go in theorem-labs/ygo
// v1.55.1-theorem.9): a struct that starts or ends past it reads as false.
const maxV1Clock = 1<<53 - 1

// maxAnyDepth is how deep eachV1Range follows lib0 Any arrays and maps inside one another:
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

// The type references of a V1 type content (v1Type) that carry a name after the reference, as Yjs
// numbers them: an element's tag and a hook's.
const (
	v1TypeXmlElement = 3
	v1TypeXmlHook    = 5
)

// v1Reader reads a V1 update's bytes from rest, and ok goes false at the first that do not read.
type v1Reader struct {
	rest []byte
	ok   bool
}

// uint reads a lib0 varuint (readUvarint).
func (r *v1Reader) uint() uint64 {
	if !r.ok {
		return 0
	}
	value, rest, ok := readUvarint(r.rest)
	if !ok {
		r.ok = false
		return 0
	}
	r.rest = rest
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

// passStruct passes one struct of a client group and answers the clocks it takes, and whether it
// is a Skip run, which holds none of the clocks it passes.
func (r *v1Reader) passStruct() (length uint64, skip bool) {
	info := r.byte()
	tag := info & v1TagBits
	if tag == v1GC || tag == v1Skip {
		return r.uint(), tag == v1Skip
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
	return r.passContent(tag), false
}

// passContent passes an item's content of kind tag and answers the clocks it takes: a deletion's
// length, a JSON or Any item's count of values, a string's UTF-16 units, and one for content of
// any other kind (crdt.Content.Len).
func (r *v1Reader) passContent(tag byte) uint64 {
	switch tag {
	case v1Deleted:
		return r.uint()
	case v1JSON:
		n := r.uint()
		if n > uint64(len(r.rest)) {
			r.ok = false
			return 0
		}
		start := r.rest
		if r.passJSON(n) {
			return n
		}
		// Not JSON text: an earlier Yjs build wrote the same values as lib0 Any, which the
		// library reads where the bytes start as one does (decodeContent's fallback).
		r.rest, r.ok = start, len(start) > 0 && start[0] >= 116 && start[0] <= 127
		for left := n; left > 0 && r.ok; left-- {
			r.passAny(0)
		}
		return n
	case v1String:
		return utf16Units(r.bytes())
	case v1Binary, v1Embed:
		r.bytes()
	case v1Format:
		r.bytes()
		r.bytes()
	case v1Type:
		ref := r.byte()
		if ref > v1TypeRefs {
			r.ok = false
		}
		if ref == v1TypeXmlElement || ref == v1TypeXmlHook {
			r.bytes() // an element's or a hook's name
		}
	case v1Any:
		n := r.uint()
		for left := n; left > 0 && r.ok; left-- {
			r.passAny(0)
		}
		return n
	case v1Doc:
		r.bytes()
		r.passAny(0)
	case v1Move:
		r.uint()
		r.uint()
		r.uint()
	default:
		r.ok = false
		return 0
	}
	return 1
}

// utf16Units is the UTF-16 units text, UTF-8, takes, which are the clocks a string item takes
// (crdt.ContentString.Len): one for each byte that starts a character and one more for each that
// starts a character of four bytes, one past U+FFFF, as the library's frame scan counts them
// (utf16Units, crdt/pending_budget.go in theorem-labs/ygo v1.55.1-theorem.9).
func utf16Units(text []byte) uint64 {
	var units uint64
	for _, b := range text {
		if b&0xc0 != 0x80 {
			units++
		}
		if b >= 0xf0 {
			units++
		}
	}
	return units
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

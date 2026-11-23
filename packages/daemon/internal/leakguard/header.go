package leakguard

import "strings"

// objectLinks is what one object's header says beside its records: the object a tag points to,
// which can be a tag the push sends too, and whether a commit names an encoding other than UTF-8,
// in which case its stored text is not the UTF-8 the rules match (Guard.inUTF8).
type objectLinks struct {
	object        string
	otherEncoding bool
}

// objectRecords hands emit one commit or tag, as git stores it (`git cat-file commit` or `git
// cat-file tag`, which raw is; tag says which), as records whose labels start with label.
//
// The header is read by git's own rules, which read a field by its place (places): a commit's
// tree, then its parents, then its author and its committer; a tag's object, then its type, its
// name and its tagger. Of those, an object id git reads as a link (a commit's tree and parents, a
// tag's object) is not read, since it names no text of anyone's, and an identity is read as its
// name and address, without the time and zone git writes after them (identity). Every other
// field is read whole, its name included, wherever it stands: a field out of git's place is one
// git does not read as structure, so it is text, whatever its name. A commit's mergetag field is
// the whole of a signed tag the merge was made from, which the push carries inside the commit, so
// it is read as a tag of its own under "<label> mergetag". Then the message is one record.
//
// A signature is read like any other text, a commit's gpgsig field and a tag's signature alike: its
// armour lines name nothing, and none of this package's rules can match random base64 by chance
// in practice (each needs six or more given letters between two non-letters), while a block a
// pusher writes by hand can hold anything.
//
// The object a tag points to is the one its first field names, which is the one git follows, and
// is empty where that field is not an object id. The object is held whole, as one commit's
// message always was; nothing of the push's diff is.
func objectRecords(label, raw string, tag bool, emit func(Record) error) (objectLinks, error) {
	var links objectLinks
	// The header ends at the first blank line. A field's further lines each start with a space, a
	// blank one included, so no field holds one.
	head, message, _ := strings.Cut(raw, "\n\n")
	fields := headerFields(head)
	for k, place := range places(fields, tag) {
		f := fields[k]
		var record Record
		switch {
		case place == aLink:
			if tag {
				links.object = strings.ToLower(f.value)
			}
			continue
		case place == anIdentity:
			record = Record{Label: label + " " + f.name, Text: identity(f.value)}
		case f.name == "mergetag" && !tag:
			if _, err := objectRecords(label+" mergetag", f.value, true, emit); err != nil {
				return links, err
			}
			continue
		default:
			if !tag && f.name == "encoding" && !utf8Name(f.value) {
				links.otherEncoding = true
			}
			record = Record{Label: label + " header", Text: f.name + " " + f.value}
		}
		if err := emit(record); err != nil {
			return links, err
		}
	}
	return links, emit(Record{Label: label + " message", Text: message})
}

// place is what git reads one field of an object's header as.
type place uint8

const (
	// readWhole is a field git reads no structure from, which the rules read whole.
	readWhole place = iota
	// aLink is an object id git follows: a commit's tree and parents, a tag's object.
	aLink
	// anIdentity is an author, committer or tagger git reads by its format.
	anIdentity
)

// places is what git reads each of fields as, by its place, as git's parsers and its object check
// read them: a commit is its tree, then the parent fields straight after it, then its author, then
// its committer; a tag is its object, then its type, its name and its tagger. A field of those
// names anywhere else, a second tree or parent or a committer after another field, is one git
// does not read as structure, and is read whole.
func places(fields []field, tag bool) []place {
	out := make([]place, len(fields))
	at := func(k int, name string) bool { return k < len(fields) && fields[k].name == name }
	if tag {
		if at(0, "object") && objectID(fields[0].value) {
			out[0] = aLink
			if at(1, "type") && at(2, "tag") && at(3, "tagger") {
				out[3] = anIdentity
			}
		}
		return out
	}
	if !at(0, "tree") || !objectID(fields[0].value) {
		return out
	}
	out[0] = aLink
	k := 1
	for ; at(k, "parent") && objectID(fields[k].value); k++ {
		out[k] = aLink
	}
	if at(k, "author") {
		out[k] = anIdentity
		k++
	}
	if at(k, "committer") {
		out[k] = anIdentity
	}
	return out
}

// utf8Name is whether name, a commit's encoding field, names UTF-8 in either spelling git takes
// for it, in any case: a commit that names it is UTF-8 already, as one that names no encoding is.
func utf8Name(name string) bool {
	return strings.EqualFold(name, "UTF-8") || strings.EqualFold(name, "UTF8")
}

// field is one field of an object's header: its name, and its value, a value of several lines
// with its lines joined by line breaks and the space git writes before each line after the first
// taken off.
type field struct {
	name  string
	value string
}

// headerFields is head, an object's header without the blank line that ends it, as its fields.
// A field's value is its first line's rest and each line after it that starts with a space, which
// is cut out of head once, as one span, with the space after each of its line breaks taken off: a
// value built a line at a time would copy itself again for each line, and a merged tag's field
// holds a line for each line of that tag's message.
func headerFields(head string) []field {
	var fields []field
	for len(head) > 0 {
		end := 0
		for {
			next := strings.IndexByte(head[end:], '\n')
			if next < 0 {
				end = len(head)
				break
			}
			end += next
			if end+1 >= len(head) || head[end+1] != ' ' {
				break
			}
			end++
		}
		span := head[:end]
		head = head[min(end+1, len(head)):]
		if span == "" {
			continue
		}
		name, value, _ := strings.Cut(span, " ")
		fields = append(fields, field{name: name, value: strings.ReplaceAll(value, "\n ", "\n")})
	}
	return fields
}

// identity is an author's, committer's or tagger's field without the time and zone git writes at
// its end, " <seconds> <+hhmm or -hhmm>": the name and the address. A field that does not end in
// them is read whole, and so is whatever stands between the address and them.
func identity(value string) string {
	rest, zone, ok := cutLast(value)
	if !ok || len(zone) != 5 || (zone[0] != '+' && zone[0] != '-') || !digits(zone[1:]) {
		return value
	}
	who, when, ok := cutLast(rest)
	if !ok || !digits(when) {
		return value
	}
	return who
}

// cutLast is s cut at its last space: what stands before it and what after.
func cutLast(s string) (before, after string, ok bool) {
	i := strings.LastIndexByte(s, ' ')
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// digits is whether s is one or more ASCII digits.
func digits(s string) bool {
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// objectID is whether value is one object id, as long as either of git's hashes writes one, in
// hex of either case, as git reads one.
func objectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range []byte(value) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

package texts

import (
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// The bounds of carried words, in UTF-16 units, as Dispatch counts a body.
const (
	// OneAct is the bound of the words of one act, where a text carries one act.
	OneAct = 4000
	// EachOfSeveral is the bound of the words of each act, where a text carries several acts.
	EachOfSeveral = 1000
	// Summary is the bound of a worker's summary.
	Summary = 1000
)

// Words are the words of another author that a text carries in a block of their own.
type Words struct {
	// Text is the words as the event carried them, after the bound.
	Text string
	// By names the author. Set it only where the fields of the text do not name the author.
	By string
	// At is the time of the act. Set it only where the fields of the text do not give the time.
	At time.Time
	// Field is the path of the words in the payload, such as kept[1].answer, where the words
	// are a field of a record in a list. Render sets it for the Words of a list's records.
	Field string
	// Rest is the count of UTF-16 units that the bound cut off.
	Rest int
	// More is true where the event marked its body as cut, or reached the daemon as a
	// payload_omitted stand-in. Then the count of the rest is not known.
	More bool
}

// WordsMeta is what a notice's payload carries for one field that holds another author's
// words, in the payload's words object, keyed by the path of that field.
type WordsMeta struct {
	By   string    `json:"by,omitempty"`
	At   time.Time `json:"at,omitzero"`
	Rest int       `json:"rest,omitempty"`
	More bool      `json:"more,omitempty"`
}

// wordsTag is the struct tag of a string field of a notice or a role-message record that holds
// another author's words: texts:"words".
const wordsTag = "words"

// WordsPaths lists the path of each field of record that holds another author's words and is
// set: a string field tagged texts:"words" that is not empty, and a Words or *Words field that
// carries words, a cut or a mark that more follows. A path is the field's json name after the json
// names of the objects that hold it, joined by dots, with the index of a record in a list after the
// list's name, counted from 0: summary, kept[1].answer, catch_up.kept.decisions[1].answer. An
// embedded struct with no json tag gives its fields in its place, as encoding/json reads it. The
// paths come in the order of the fields' declaration.
//
// The words object of a notice's or a role message's payload is keyed by these paths, with a
// WordsMeta for each.
func WordsPaths(record any) []string {
	return wordsPathsIn(nil, reflect.ValueOf(record), "")
}

// wordsPathsIn appends the paths of the words fields that v holds at path: v's own fields for a
// struct, the fields of each record for a list, and nothing for any other value.
func wordsPathsIn(paths []string, v reflect.Value, path string) []string {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return paths
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == timeType {
			return paths
		}
		return wordsPathsOf(paths, v, path)
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			paths = wordsPathsIn(paths, v.Index(i), path+"["+strconv.Itoa(i)+"]")
		}
	}
	return paths
}

// wordsPathsOf appends the paths of the words fields of the struct v, whose own path is path
// (empty for the record itself).
func wordsPathsOf(paths []string, v reflect.Value, path string) []string {
	for i := range v.NumField() {
		field := v.Type().Field(i)
		tag, tagged := field.Tag.Lookup("json")
		if tag == "-" || !field.IsExported() {
			continue
		}
		value := v.Field(i)
		if field.Anonymous && !tagged {
			paths = wordsPathsIn(paths, value, path)
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = field.Name
		}
		if path != "" {
			name = path + "." + name
		}
		switch {
		case field.Type == wordsType:
			if !value.Interface().(Words).empty() {
				paths = append(paths, name)
			}
		case field.Type == wordsPtrType:
			if !value.IsNil() && !value.Elem().Interface().(Words).empty() {
				paths = append(paths, name)
			}
		case field.Tag.Get("texts") == wordsTag:
			if value.Kind() == reflect.String && value.Len() > 0 {
				paths = append(paths, name)
			}
		default:
			paths = wordsPathsIn(paths, value, name)
		}
	}
	return paths
}

// Bound cuts text to its first limit UTF-16 units. It returns that head and the count of
// UTF-16 units it cut off. A character outside the Basic Multilingual Plane counts two units,
// and Bound never splits it: where its two units would cross the limit, the head leaves it out.
// A byte that is not valid UTF-8 decodes as U+FFFD, one unit, as a JSON decoder reads it.
func Bound(text string, limit int) (head string, rest int) {
	units := 0
	for i, r := range text {
		n := utf16.RuneLen(r)
		if units+n > limit {
			return text[:i], Units(text[i:])
		}
		units += n
	}
	return text, 0
}

// Carry bounds text to limit and gives the Words that a text carries. Where the event marked
// its body as cut (more), the Words say that more follows and give no count of the rest.
// Otherwise they give the count of the UTF-16 units that the bound cut off. The caller sets By
// and At where the fields of the text do not name them.
func Carry(text string, limit int, more bool) Words {
	head, rest := Bound(text, limit)
	if more {
		return Words{Text: head, More: true}
	}
	return Words{Text: head, Rest: rest}
}

// Block draws w in a block of its own, the block that carries another author's words after a
// text's fields (dispatch://INFRA-3686/spec R1), as the whole of its text: its tag is the one
// blockTags gives a block with nothing before it and no other block beside it. The opening tag
// carries the attributes by, at, field, rest and more, in that order, each only where it is set.
// Then come a new line, the words exactly as given, a new line and the closing tag. Render draws
// the blocks of a text together (drawBlocks), since the tag of each is chosen against the lines
// before them and the words of them all.
func Block(name string, w Words) string {
	return drawBlock(blockTags(nil, []block{{name: name, words: w}})[0], w)
}

// block is one block of a text: the name of the field that holds the words, and the words.
type block struct {
	name  string
	words Words
}

// drawBlocks draws the blocks of one text, in their order, each under the tag blockTags gives it
// against before, the lines of the text ahead of the blocks.
func drawBlocks(before []string, blocks []block) []string {
	tags := blockTags(before, blocks)
	drawn := make([]string, len(blocks))
	for i, b := range blocks {
		drawn[i] = drawBlock(tags[i], b.words)
	}
	return drawn
}

// InFront is parts and then text, each joined to the next by a blank line: the notes a start puts
// in front of a task the daemon drew (supervise's Delivery.message), and the task. A part can hold
// words that are not the daemon's: a process's cause quotes the runtime's, and those can carry a
// tail of the role's own log, a task's own lines among it. So every block of text (drawnBlocks) is
// drawn again, with its attributes and its words as they were, under the tag blockTags gives it
// against the whole of what is sent: the parts, the lines of text outside its blocks and the words
// of every block. A task can be several records, each joined to the one before by a blank line
// (Joined: a start's own record, then the news record of each audience), and the blocks of every
// record are drawn again together, so nothing in front of any block opens or closes its tag, as
// nothing does in the task alone. Each block of the text keeps the tag it had wherever the parts
// hold none of those tags, the parts stay word for word, and a text with no block is joined as it
// is. A card, which the TypeScript readers draw again by this rule (testdata/in-front.json), is
// one record. A text that no record drew holds no record line (Drawn): an operator's task, or a
// task an earlier release queued, which is prose. Such a text goes in front of no note by this
// rule but by InFrontOfWords, which keeps it as its author wrote it. Its caller tells the two apart
// by the text itself and not by the run the task is of, since a task an earlier release queued
// carries a run as a task of this release does.
func InFront(parts []string, text string) string {
	if len(parts) == 0 {
		return text
	}
	return joinParts(append(slices.Clip(parts), text), func(i int) bool { return i == len(parts) })
}

// InFrontOfWords is parts and then text, each joined to the next by a blank line, where text is
// words another author wrote that no record drew, which hold no record line (Drawn): an operator's
// task, or a task an earlier release queued, which is prose (supervise's Delivery.message), in
// front of which a start puts its notes. The words go as their author wrote them, tags and all:
// their lines are no record's blocks, and nothing renames them. Each block of a part (drawnBlocks)
// is drawn again instead, with its attributes and its words as they were, under the tag blockTags
// gives it against the whole of what is sent, the words among it, so no line of the words opens or
// closes the tag of a part's block. Records this release drew can follow the words: the news record
// of each audience after a start an earlier release queued (Joined). They begin at the first line
// after a blank line that opens a record (recordsAfter), and their blocks are drawn again as a
// part's are, so nothing in front of them opens or closes their tags either. A part or a record
// that holds no block is joined as it is, so where none holds a block the parts and the text are
// joined word for word.
func InFrontOfWords(parts []string, text string) string {
	if len(parts) == 0 {
		return text
	}
	words := len(parts)
	all := make([]string, 0, len(parts)+2)
	all = append(all, parts...)
	if at := recordsAfter(text); at >= 0 {
		all = append(all, text[:at-len("\n\n")], text[at:])
	} else {
		all = append(all, text)
	}
	return joinParts(all, func(i int) bool { return i != words })
}

// Joined is records, each joined to the next by a blank line: a start's own text and then the
// news record of each audience a delivery speaks to (the daemon's followedBy). Each record Render
// drew had its blocks drawn against that record alone, and the words of one record can hold the
// tag of another's block: a heading of a changed section can quote the closing tag of the start's
// reason block. So every block of every record Render drew (drawnBlocks) is drawn again, with its
// attributes and its words as they were, under the tag blockTags gives it against all the other
// text of the joined task, the lines outside the blocks of every record and the words of every
// block, so nothing in the task opens or closes a block's tag but that block's own lines, with no
// note in front as behind one (InFront). A block keeps the tag Render gave it wherever no other
// record holds that tag. A start an earlier release queued is prose, which holds no record line
// (Drawn): it goes as its author wrote it, tags and all, as an operator's task goes behind a note
// (InFrontOfWords), and the blocks of the records after it take tags chosen against it, so no line
// of it opens or closes their tags and none of its lines is renamed. An empty record is no part,
// and one record alone comes back as it was given.
func Joined(records ...string) string {
	parts := slices.DeleteFunc(slices.Clone(records), func(record string) bool { return record == "" })
	return joinParts(parts, func(i int) bool { return Drawn(parts[i]) })
}

// Drawn says whether text is a text Render drew, or several records joined behind such a text
// (Joined): whether it opens with a record line, `kind: ` and a kind on the list, as its first line,
// or as its second behind the lead line of a kind whose record has one (CheckLookRecord.Lead). It
// is how a caller tells, from the text itself, whether this release drew it. A task an earlier
// release queued is prose and holds no record line, though it carries a run as a task of this
// release does, and so does an operator's task, which carries none; main's look task opens with
// the lead this release's opens with, and goes on with prose. The controller's start message, a
// fixed sentence with no field (ControllerStartRecord), holds no record line either, nor a block,
// so InFront and InFrontOfWords send it alike.
func Drawn(text string) bool {
	first, rest, _ := strings.Cut(text, "\n")
	if _, ok := recordKind(first); ok {
		return true
	}
	second, _, _ := strings.Cut(rest, "\n")
	entry, ok := recordKind(second)
	_, led := entry.Record.(leader)
	return ok && led
}

// recordKind is the entry of the kind line opens a record of: a line `kind: ` and a kind on the
// list, as Render writes a record's first field (TaskHead).
func recordKind(line string) (Entry, bool) {
	kind, ok := strings.CutPrefix(line, "kind: ")
	if !ok {
		return Entry{}, false
	}
	return Lookup(Kind(kind))
}

// recordsAfter is where the records Render drew that follow words no record drew begin in text:
// the first line after a blank line that opens a record (recordKind), as Joined joins the news of
// each audience after a start an earlier release queued. It is -1 where no record follows. Each
// line is read at most twice.
func recordsAfter(text string) int {
	for from := 0; ; {
		at := strings.Index(text[from:], "\n\nkind: ")
		if at < 0 {
			return -1
		}
		at += from + len("\n\n")
		line, _, _ := strings.Cut(text[at:], "\n")
		if _, ok := recordKind(line); ok {
			return at
		}
		from = at
	}
}

// joinParts is parts, each joined to the next by a blank line, with every block of each part that
// drawn says a record drew (drawnBlocks) drawn again, its opening tag's attributes and its words as
// they were, under the tag blockTags gives it against everything else that is sent: every part
// another author wrote, which goes as written, the text of each drawn part outside its blocks, and
// the words of every block. It is the one rule InFront, InFrontOfWords and Joined join by. Parts
// that hold no drawn block are joined as they are.
func joinParts(parts []string, drawn func(part int) bool) string {
	found := make([][]drawnBlock, len(parts))
	var before []string
	var blocks []block
	size := 0
	for i, part := range parts {
		size += len(part) + len("\n\n")
		if !drawn(i) {
			before = append(before, part)
			continue
		}
		found[i] = drawnBlocks(part)
		// The text outside the blocks: ahead of the first, between one record's run and the next
		// (the next record's fields), and after the last. Two blocks of one run stand a new line
		// apart, which holds no tag.
		from := 0
		for _, d := range found[i] {
			if between := part[from:d.start]; between != "\n" {
				before = append(before, between)
			}
			blocks = append(blocks, block{name: d.name(), words: Words{Text: d.words}})
			from = d.end
		}
		before = append(before, part[from:])
	}
	if len(blocks) == 0 {
		return strings.Join(parts, "\n\n")
	}
	tags := blockTags(before, blocks)
	var b strings.Builder
	// Room for a suffix of a few characters on each block's two tags.
	b.Grow(size + 8*len(blocks))
	for i, part := range parts {
		if i > 0 {
			b.WriteString("\n\n")
		}
		from := 0
		for _, d := range found[i] {
			b.WriteString(part[from:d.start])
			// The block as drawBlock draws it, its opening tag's attributes as that tag wrote them.
			b.WriteString("<")
			b.WriteString(tags[0])
			b.WriteString(d.attributes)
			b.WriteString(">\n")
			b.WriteString(d.words)
			b.WriteString("\n</")
			b.WriteString(tags[0])
			b.WriteString(">")
			tags = tags[1:]
			from = d.end
		}
		b.WriteString(part[from:])
	}
	return b.String()
}

// drawnOpening is the opening tag of a block as drawBlock draws it, a line of its own: the tag,
// then each attribute as writeAttribute writes it, whose value holds no double quote and no angle
// bracket, since writeAttribute escapes all three. It is the TypeScript reader's rule too
// (OPENING, packages/envoy-client/src/words-block.ts), and testdata/in-front.json holds the two
// to one output: a line whose value holds a raw <, > or " opens no block for either.
var drawnOpening = regexp.MustCompile(`^<([A-Za-z_][A-Za-z0-9_]*(?:-[1-9][0-9]*)?)((?: [a-z]+="[^"<>]*")*)>$`)

// drawnBlock is one block of a drawn text: where its opening line starts and where its closing
// tag ends, its tag, the attributes of its opening tag as written, and its words.
type drawnBlock struct {
	start, end int
	tag        string
	attributes string
	words      string
}

// name is the name of the field whose words the block holds: its tag without the -n that a step
// past held tags added (blockTags).
func (d drawnBlock) name() string {
	if at := strings.LastIndexByte(d.tag, '-'); at >= 0 {
		return d.tag[:at]
	}
	return d.tag
}

// drawnBlocks is the blocks of text, in their order: a text Render drew, or several such records,
// each joined to the one before by a blank line (Joined, the daemon's followedBy). No line of a
// record ahead of its blocks starts with <, since a field is a TOON line and the lead and the kept
// sentences are the daemon's own, so a record's first such line opens its first block. A block
// closes on the first line after its opening that is its closing tag, since no words of a block
// hold that tag (blockTags), and the next block opens on the line after. A record's run of blocks
// ends at the first line that opens no block. Render ends a record with its last block, so where
// that line is blank the next record starts after it, and its first line that starts with < opens
// its first block in turn; a record with no block has no such line, and the search goes on into
// the record after it. The blocks end at a run that ends on any other line (a reviewer's round
// line after the last record, reviewround.Ending, or a line of a text Render did not draw), and at
// a record whose first line that starts with < opens no block, or opens one that never closes:
// what follows stays as it is. So they are empty where the first line that starts with < opens
// none. A text Render did not draw can still hold lines of a block's form, such as two `<details>`
// sections of a log an operator pasted into a task, or of a tester's summary that a task an
// earlier release queued quotes, which this reads as blocks: such a text is never read here. It
// holds no record line (Drawn), which is how its caller tells it from the text itself, whatever
// run the task is of, and its caller joins it as its author wrote it (InFrontOfWords, and Joined
// for a start an earlier release queued), reading only the records drawn after it. One pass over
// text.
func drawnBlocks(text string) []drawnBlock {
	var blocks []drawnBlock
	for record := 0; record < len(text); {
		at := record
		if text[at] != '<' {
			line := strings.Index(text[at:], "\n<")
			if line < 0 {
				break
			}
			at += line + 1
		}
		ran := -1
		for at < len(text) && text[at] == '<' {
			opened := strings.IndexByte(text[at:], '\n')
			if opened < 0 {
				break
			}
			match := drawnOpening.FindStringSubmatch(text[at : at+opened])
			if match == nil {
				break
			}
			wordsAt := at + opened + 1
			closing := "\n</" + match[1] + ">"
			closed := strings.Index(text[wordsAt:], closing)
			if closed < 0 {
				break
			}
			end := wordsAt + closed + len(closing)
			if end < len(text) && text[end] != '\n' {
				break
			}
			blocks = append(blocks, drawnBlock{start: at, end: end, tag: match[1], attributes: match[2], words: text[wordsAt : wordsAt+closed]})
			at, ran = end+1, end
		}
		if ran < 0 || !strings.HasPrefix(text[ran:], "\n\n") {
			break
		}
		record = ran + 2
	}
	return blocks
}

// drawBlock draws w under tag.
func drawBlock(tag string, w Words) string {
	var b strings.Builder
	b.Grow(len(tag)*2 + len(w.Text) + 64)
	b.WriteString("<")
	b.WriteString(tag)
	if w.By != "" {
		writeAttribute(&b, "by", w.By)
	}
	if !w.At.IsZero() {
		writeAttribute(&b, "at", formatTime(w.At))
	}
	if w.Field != "" {
		writeAttribute(&b, "field", w.Field)
	}
	if w.Rest > 0 {
		writeAttribute(&b, "rest", strconv.Itoa(w.Rest))
	}
	if w.More {
		writeAttribute(&b, "more", "true")
	}
	b.WriteString(">\n")
	b.WriteString(w.Text)
	b.WriteString("\n</")
	b.WriteString(tag)
	b.WriteString(">")
	return b.String()
}

// empty says whether w carries nothing at all: no words, no cut and no mark that more follows.
func (w Words) empty() bool {
	return w.Text == "" && w.Rest == 0 && !w.More
}

// blockTags is the tag of each of blocks, the blocks of one text in their order, after the lines
// before: the first of the block's name, name-1, name-2 and on that neither a line before the
// blocks nor the words of any block of the text hold as <tag or </tag, and that no earlier block
// took. The lines before are the whole text ahead of the first block, its lead, its fields and its
// kept sentences, which can hold another author's words too: an issue's title, a check's name, an
// option of an ask. So nothing in the text opens or closes the tag of a block but that block's own
// lines, and no two blocks share a tag: a reader that pairs each opening tag with the next closing
// tag of its name reads each block's words as its own. The test is a plain substring test, so
// text that holds <name-1 or <names also steps past name.
//
// The text is read once, for every name at once (tagNames), and each block's tag is then looked
// up in what that read holds, so a text is drawn in time linear in its length whatever the count
// of its names. A name's next block starts where its last block's tag stopped: every suffix below
// that one is held or taken, and stays so. limit, the length of the lines, the length of the
// words and the count of the blocks, bounds every tag the text can need, since each held number
// and each block takes away at most one candidate.
func blockTags(before []string, blocks []block) []string {
	limit := len(blocks)
	for _, line := range before {
		limit += len(line)
	}
	for _, b := range blocks {
		limit += len(b.words.Text)
	}
	names, ids := newTagNames(blocks, limit)
	for _, line := range before {
		names.hold(line)
	}
	for _, b := range blocks {
		names.hold(b.words.Text)
	}
	tags := make([]string, len(blocks))
	taken := make(map[string]bool, len(blocks))
	next := make([]int, len(names.held))
	for i, b := range blocks {
		id := ids[i]
		for n := next[id]; ; n++ {
			if names.held[id][n] {
				continue
			}
			tag := b.name
			if n > 0 {
				tag += "-" + strconv.Itoa(n)
			}
			if !taken[tag] {
				tags[i], taken[tag], next[id] = tag, true, n+1
				break
			}
		}
	}
	return tags
}

// tagNames is the names of a text's blocks, as a trie whose node 0 is the empty name, and what the
// text holds of each name's tags (hold).
type tagNames struct {
	// edges is the node each node's edge of one byte leads to.
	edges map[tagEdge]int
	// name is, for each node, one more than the index of the name that ends there, 0 where none
	// does.
	name []int
	// held is, for each name, each n up to limit whose tag a text read holds: name for n = 0 and
	// name-n otherwise. It is nil for a name whose tags no text read holds.
	held  []map[int]bool
	limit int
}

// tagEdge is an edge of the trie: the node it leaves and the byte it reads.
type tagEdge struct {
	from int
	b    byte
}

// newTagNames is the trie of the names of blocks, which holds nothing yet, and the index of each
// block's name in it: blocks of one name share one index.
func newTagNames(blocks []block, limit int) (*tagNames, []int) {
	names := &tagNames{edges: map[tagEdge]int{}, name: []int{0}, limit: limit}
	ids := make([]int, len(blocks))
	for i, b := range blocks {
		node := 0
		for j := range len(b.name) {
			edge := tagEdge{from: node, b: b.name[j]}
			child, ok := names.edges[edge]
			if !ok {
				child = len(names.name)
				names.name = append(names.name, 0)
				names.edges[edge] = child
			}
			node = child
		}
		if names.name[node] == 0 {
			names.held = append(names.held, nil)
			names.name[node] = len(names.held)
		}
		ids[i] = names.name[node] - 1
	}
	return names, ids
}

// hold adds what text holds of the names' tags. At each < of text, past a / that follows it, the
// names are walked along the text that follows: each name the walk reaches is one the text holds
// as <name or </name, so it is held at 0, and after <name- or </name- each number whose digits
// begin the digits there is held too (<name-12 holds 1 and 12). A name is a field's json name, of
// letters, digits and underscores, so a walk reads no further than the run of those after its <,
// and no two runs meet, since a run holds no <: text is read once. A text is read on its own,
// which finds what the joined text holds: a tag holds no new line.
func (t *tagNames) hold(text string) {
	for at := strings.IndexByte(text, '<'); at >= 0; {
		from := at + 1
		if from < len(text) && text[from] == '/' {
			from++
		}
		node := 0
		for i := from; ; i++ {
			if id := t.name[node]; id > 0 {
				t.holdSuffixes(id-1, text[i:])
			}
			if i == len(text) {
				break
			}
			child, ok := t.edges[tagEdge{from: node, b: text[i]}]
			if !ok {
				break
			}
			node = child
		}
		next := strings.IndexByte(text[at+1:], '<')
		if next < 0 {
			return
		}
		at += 1 + next
	}
}

// holdSuffixes holds the name of index id at 0, and, where after, the text after the name, is -
// and a number with no leading zero, each number up to limit that the number's digits begin.
func (t *tagNames) holdSuffixes(id int, after string) {
	held := t.held[id]
	if held == nil {
		held = map[int]bool{}
		t.held[id] = held
	}
	held[0] = true
	digits, suffixed := strings.CutPrefix(after, "-")
	if !suffixed || digits == "" || digits[0] < '1' || digits[0] > '9' {
		return
	}
	for n, j := 0, 0; j < len(digits) && digits[j] >= '0' && digits[j] <= '9'; j++ {
		if n = n*10 + int(digits[j]-'0'); n > t.limit {
			break
		}
		held[n] = true
	}
}

// writeAttribute writes one attribute of an opening tag. The value escapes markup, control
// characters below U+0020 and the three unescaped line breaks as XML character references.
func writeAttribute(b *strings.Builder, key, value string) {
	b.WriteString(" ")
	b.WriteString(key)
	b.WriteString(`="`)
	start := 0
	for i, r := range value {
		switch {
		case r == '&':
			b.WriteString(value[start:i])
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString(value[start:i])
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString(value[start:i])
			b.WriteString("&gt;")
		case r == '"':
			b.WriteString(value[start:i])
			b.WriteString("&quot;")
		case r < 0x20 || unescapedBreak(r):
			b.WriteString(value[start:i])
			b.WriteString("&#")
			var digits [8]byte
			b.Write(strconv.AppendInt(digits[:0], int64(r), 10))
			b.WriteString(";")
		default:
			continue
		}
		start = i + utf8.RuneLen(r)
	}
	b.WriteString(value[start:])
	b.WriteString(`"`)
}

// Units is the count of UTF-16 units of text, as Dispatch counts a body and every bound of carried
// words counts (Bound), each byte that is not valid UTF-8 one unit (U+FFFD).
func Units(text string) int {
	n := 0
	for _, r := range text {
		n += utf16.RuneLen(r)
	}
	return n
}

// formatTime writes t as the texts write a time: RFC 3339 in UTC, with the fraction of a second
// only where t has one.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

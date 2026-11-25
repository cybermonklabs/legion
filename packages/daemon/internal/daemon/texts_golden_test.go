package daemon

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// updateTexts rewrites testdata/texts.golden.json from the builders (go test ./internal/daemon
// -run TestEveryKindOnTheListStatesItsFields -update). CI never passes it, so a builder whose text
// changed fails the test until the file is written again and read in review.
var updateTexts = flag.Bool("update", false, "rewrite testdata/texts.golden.json, the golden file of the daemon's texts")

// goldenTextsPath is the golden file of every kind on the one list (texts.Entries). The envoy-client
// test of the card reads it too: each task with its blocks marked, each notice and role message as
// the payload the daemon publishes.
const goldenTextsPath = "testdata/texts.golden.json"

// goldenFile is the golden file: every sample, in the order goldenSamples builds them.
type goldenFile struct {
	Samples []goldenSample `json:"samples"`
}

// goldenSample is one rendering of one kind on the list. Each kind is rendered at least twice
// (Sample 1 and 2, with different values in every field) in each of its variants. A task is its
// text, with the spans of R1's blocks and of the sentences R8 keeps. A notice or a role message is
// the summary and the payload the daemon publishes (notify.Publisher.Publish encodes the payload
// with json.Marshal): an object, or the hook prompt's text as a JSON string.
type goldenSample struct {
	Kind    texts.Kind      `json:"kind"`
	Variant string          `json:"variant"`
	Sample  int             `json:"sample"`
	Reader  claim.Role      `json:"reader"`
	Form    texts.Form      `json:"form"`
	Urgency texts.Urgency   `json:"urgency"`
	Issue   string          `json:"issue"`
	Text    string          `json:"text,omitempty"`
	Spans   []goldenSpan    `json:"spans,omitempty"`
	Summary string          `json:"summary,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`

	// want is the value of each fact the builder was given, by the field path the sample names it
	// at (goldenFields): what main's builder of the kind put in its text, which the sample must
	// carry (goldenMainFacts). The test sets it from its own inputs, never from the builder's
	// output.
	want map[string]string
}

// goldenSpan marks a part of a task's text that is not a field line: a block of R1 (another
// author's words, Part "block", Name its tag) or a sentence R8 keeps word for word (Part "kept",
// KeptBy what keeps it). Start and End are offsets of the text in UTF-16 code units, as a
// JavaScript string indexes it: text.slice(start, end) is the span.
type goldenSpan struct {
	Part   string `json:"part"`
	Name   string `json:"name,omitempty"`
	KeptBy string `json:"kept_by,omitempty"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
}

// goldenID names a sample in a failure.
func (s goldenSample) id() string {
	return fmt.Sprintf("%s/%s#%d", s.Kind, s.Variant, s.Sample)
}

// The check c-daemon-texts-are-facts, first test: one sample of each kind on the list, the phase
// assignment and each shape of ArchitectStartTask included, each kind rendered twice with
// different values in every field, written to the golden file. A tree of 60 issues with titles of
// 1,000 characters (35 done, 5 children not yet released, 3 in accepting), 173 blocked issues and
// 30 silent workers, a children-first notice of 30 open children, a CI settlement and a
// review-stuck head of 60 checks with 25 that failed or were cancelled (the head has no skipped
// conclusion yet, dispatch://INFRA-4135's R1, so none is skipped and the 35 others passed) and of
// 30 commit statuses with 3 in state failure or error (cybermonklabs/legion#4299), and an
// update round with a new image and 30 changed settings. The titles and words carry the hostile
// values of TOON (true, null, -x, #x, 1e5, 0123, 123456e78901, a comma, a colon, a tab, a quote, a
// backslash, a character outside the Basic Multilingual Plane, an empty string).
//
// It fails on each rule of the check (goldenProblems, goldenPairProblems), on each fact main's
// builders wrote that a sample lacks (goldenMainFacts), on each sentence and field R8 keeps that a
// sample lacks (goldenKept), and on a golden file that differs from what the builders write now.
func TestEveryKindOnTheListStatesItsFields(t *testing.T) {
	samples := goldenSamples(t)

	rendered := map[texts.Kind]bool{}
	pairs := map[string][]*goldenSample{}
	for i := range samples {
		s := &samples[i]
		rendered[s.Kind] = true
		key := string(s.Kind) + "/" + s.Variant
		pairs[key] = append(pairs[key], s)
		for _, problem := range goldenProblems(s) {
			t.Errorf("%s: %s", s.id(), problem)
		}
	}
	for _, entry := range texts.Entries() {
		if !rendered[entry.Kind] {
			t.Errorf("kind %s of the list has no sample", entry.Kind)
		}
	}
	for key, pair := range pairs {
		if len(pair) != 2 || pair[0].Sample != 1 || pair[1].Sample != 2 {
			t.Errorf("%s: %d renderings, want the two of samples 1 and 2", key, len(pair))
			continue
		}
		for _, problem := range goldenPairProblems(pair[0], pair[1]) {
			t.Errorf("%s: %s", key, problem)
		}
	}
	for _, facts := range goldenMainFacts {
		if !slices.ContainsFunc(samples, func(s goldenSample) bool { return facts.covers(&s) }) {
			t.Errorf("main's facts of %s (%s, from %s) match no sample", facts.kind, strings.Join(facts.variants, ","), facts.from)
		}
	}
	for _, kept := range goldenKept {
		if !slices.ContainsFunc(samples, func(s goldenSample) bool { return kept.covers(&s) }) {
			t.Errorf("what %s keeps of %s (%s) matches no sample", kept.keptBy, kept.kind, kept.what)
		}
	}

	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(goldenFile{Samples: samples}); err != nil {
		t.Fatalf("encode the golden file: %v", err)
	}
	if *updateTexts {
		if err := os.MkdirAll(filepath.Dir(goldenTextsPath), 0o755); err != nil {
			t.Fatalf("make the golden file's directory: %v", err)
		}
		if err := os.WriteFile(goldenTextsPath, encoded.Bytes(), 0o644); err != nil {
			t.Fatalf("write the golden file: %v", err)
		}
		return
	}
	held, err := os.ReadFile(goldenTextsPath)
	if err != nil {
		t.Fatalf("read the golden file (write it with `go test ./internal/daemon -run TestEveryKindOnTheListStatesItsFields -update`): %v", err)
	}
	if !bytes.Equal(held, encoded.Bytes()) {
		t.Fatalf("%s is stale: the builders write other texts now; read the change, then write it with "+
			"`go test ./internal/daemon -run TestEveryKindOnTheListStatesItsFields -update`\n%s", goldenTextsPath, goldenDiff(held, encoded.Bytes()))
	}
}

// goldenDiff is the first line where held and now differ, with the line of each.
func goldenDiff(held, now []byte) string {
	a, b := strings.Split(string(held), "\n"), strings.Split(string(now), "\n")
	for i := range max(len(a), len(b)) {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return fmt.Sprintf("line %d\n held: %s\n  now: %s", i+1, goldenCut(x), goldenCut(y))
		}
	}
	return "no line differs"
}

// goldenCut is s cut for a failure message.
func goldenCut(s string) string {
	if runes := []rune(s); len(runes) > 300 {
		return string(runes[:300]) + "…"
	}
	return s
}

// The sentences of a text about a person's act that send its reader to fetch the words the text
// could carry (R1), as main's resume and kept-decisions tasks wrote them.
var goldenFetchPhrases = []string{
	"read that thread", "read it.", "Read each before you continue", "Read the review", "Read its thread",
	"read the issue's asks and its spec",
}

// goldenProblems is every rule of the check's first test that s breaks on its own.
func goldenProblems(s *goldenSample) []string {
	entry, ok := texts.Lookup(s.Kind)
	if !ok {
		return []string{"its kind is on no list"}
	}
	var problems []string
	if s.Form != entry.Form {
		problems = append(problems, fmt.Sprintf("form %s, but the list gives %s", s.Form, entry.Form))
	}
	if !slices.Contains(entry.Readers, s.Reader) {
		problems = append(problems, fmt.Sprintf("reader %s is none of the kind's readers %v", s.Reader, entry.Readers))
	}
	fields, more := goldenFieldsOf(s, entry)
	problems = append(problems, more...)
	problems = append(problems, goldenNamesProblems(s, fields)...)
	if s.Kind == texts.CheckLook {
		first, _, _ := strings.Cut(s.Text, "\n")
		if lead := "Production check of " + s.Issue + ": " + s.want["title"] + "."; first != lead {
			problems = append(problems, fmt.Sprintf("its first line is %q, not %q", goldenCut(first), goldenCut(lead)))
		}
	}
	for _, phrase := range goldenFetchPhrases {
		if strings.Contains(strings.ToLower(fields.prose), strings.ToLower(phrase)) {
			problems = append(problems, fmt.Sprintf("it sends its reader to fetch words it could carry: %q", phrase))
		}
	}
	for _, facts := range goldenMainFacts {
		if facts.covers(s) {
			problems = append(problems, facts.problems(s, fields)...)
		}
	}
	for _, kept := range goldenKept {
		if kept.covers(s) {
			problems = append(problems, kept.problems(s, fields)...)
		}
	}
	return problems
}

// goldenFields is what one sample states: each field by its path, as the encoder's lines or the
// payload give it, and the parts that are no field.
type goldenFields struct {
	// values is each field by its path: key, key[i], key[i].field, words.summary.by; a list's
	// count at key# and a list of primitives joined by commas at key; a block's words at its
	// field path (summary, kept[1].answer), and its attributes at path@by and path@at.
	values map[string]string
	// prose is what the text says that is neither a block's words nor a field that carries
	// another author's words: a task's lines and a payload's string values but its words fields.
	prose string
	// kept is each line of the text that R8 keeps.
	kept []string
}

// goldenFieldsOf reads s as its form gives it, and returns every problem of its shape: a line of
// a task that is not the encoder's, a key the kind's record does not declare, a key twice, a
// payload that is no object, a words field its words object does not name with its author and
// time.
func goldenFieldsOf(s *goldenSample, entry texts.Entry) (goldenFields, []string) {
	fields := goldenFields{values: map[string]string{}}
	switch {
	case s.Form == texts.Task:
		if len(s.Payload) > 0 || s.Text == "" {
			return fields, []string{"a task with no text, or with a payload"}
		}
		return fields, goldenTaskFields(s, entry, &fields)
	case s.Kind == texts.HookPrompt:
		var text string
		if err := json.Unmarshal(s.Payload, &text); err != nil {
			return fields, []string{fmt.Sprintf("the hook prompt is published as %s, not as its text", goldenCut(string(s.Payload)))}
		}
		return fields, goldenHookFields(s, text, &fields)
	}
	return fields, goldenPayloadFields(s, entry, &fields)
}

// goldenOpenTag is a line that opens a block of R1 (texts.Block): the tag, then its attributes.
var goldenOpenTag = regexp.MustCompile(`^<([a-z_]+(?:-[0-9]+)?)((?: [a-z_]+="[^"]*")*)>$`)

// goldenAttribute is one attribute of an opening tag.
var goldenAttribute = regexp.MustCompile(` ([a-z_]+)="([^"]*)"`)

// goldenLine is a line of the encoder: a key: value line, a list's key[n]: line, a table's
// key[n]{fields}: header. A key that is not a plain name is quoted.
var goldenLine = regexp.MustCompile(`^([A-Za-z_][\w.]*|"(?:[^"\\]|\\.)*")(?:\[([0-9]+)\](?:\{([^}]*)\})?)?:(?: (.*))?$`)

// goldenTaskFields reads a task's text: R8's kept lines, R1's blocks, and every other line as a
// line of the encoder whose key the kind's record declares. It records the spans of the blocks
// and of the kept lines on s.
func goldenTaskFields(s *goldenSample, entry texts.Entry, fields *goldenFields) []string {
	var problems []string
	declared := goldenDeclared(reflect.TypeOf(entry.Record))
	kept := map[string]string{}
	for _, item := range goldenKept {
		if item.covers(s) && item.lines != nil {
			for _, line := range item.lines(s.want) {
				kept[line] = item.keptBy
			}
		}
	}
	text := s.Text
	s.Spans = nil
	seen := map[string]bool{}
	var prose strings.Builder
	offset := 0
	for offset <= len(text) {
		end := strings.IndexByte(text[offset:], '\n')
		if end < 0 {
			end = len(text)
		} else {
			end += offset
		}
		line := text[offset:end]
		next := end + 1
		switch {
		case kept[line] != "":
			s.Spans = append(s.Spans, goldenSpan{Part: "kept", KeptBy: kept[line], Start: goldenUnits(text[:offset]), End: goldenUnits(text[:end])})
			fields.kept = append(fields.kept, line)
		case goldenOpenTag.MatchString(line):
			blockEnd, problem := goldenBlock(text, offset, end, fields)
			if problem != "" {
				return append(problems, problem)
			}
			tag := goldenOpenTag.FindStringSubmatch(line)[1]
			s.Spans = append(s.Spans, goldenSpan{Part: "block", Name: tag, Start: goldenUnits(text[:offset]), End: goldenUnits(text[:blockEnd])})
			end, next = blockEnd, blockEnd+1
		default:
			prose.WriteString(line)
			prose.WriteString("\n")
			match := goldenLine.FindStringSubmatch(line)
			if match == nil {
				problems = append(problems, fmt.Sprintf("line %q is no line of the encoder's form", goldenCut(line)))
				break
			}
			key, err := goldenKey(match[1])
			if err != nil {
				problems = append(problems, fmt.Sprintf("line %q: %v", goldenCut(line), err))
				break
			}
			if seen[key] {
				problems = append(problems, fmt.Sprintf("key %s comes twice", key))
			}
			seen[key] = true
			field, declaredKey := declared[key]
			if !declaredKey {
				problems = append(problems, fmt.Sprintf("key %s is no field the %s record declares", key, s.Kind))
			}
			rows, problem := goldenEncoderLine(key, match, field, text, end, fields)
			if problem != "" {
				problems = append(problems, problem)
			}
			for range rows {
				if next > len(text) {
					problems = append(problems, fmt.Sprintf("table %s has fewer rows than its header names", key))
					break
				}
				rowEnd := strings.IndexByte(text[next:], '\n')
				if rowEnd < 0 {
					rowEnd = len(text)
				} else {
					rowEnd += next
				}
				prose.WriteString(text[next:rowEnd])
				prose.WriteString("\n")
				end, next = rowEnd, rowEnd+1
			}
		}
		offset = next
	}
	fields.prose = prose.String()
	return problems
}

// goldenUnits is the count of UTF-16 code units of s.
func goldenUnits(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// goldenBlock reads the block of R1 whose opening tag is text[start:end]: the words up to the
// first line that closes the tag, which the words cannot hold (texts.Block), and the attributes.
// It records the words at their path (the field attribute, else the block's name less its -N) and
// returns where the block ends.
func goldenBlock(text string, start, end int, fields *goldenFields) (int, string) {
	match := goldenOpenTag.FindStringSubmatch(text[start:end])
	tag := match[1]
	closing := "\n</" + tag + ">"
	at := strings.Index(text[end:], closing)
	if at < 0 {
		return 0, fmt.Sprintf("block %s has no closing tag", tag)
	}
	words := text[min(end+1, end+at) : end+at]
	blockEnd := end + at + len(closing)
	if blockEnd < len(text) && text[blockEnd] != '\n' {
		return 0, fmt.Sprintf("block %s: text follows its closing tag on its line", tag)
	}
	name, _, _ := strings.Cut(tag, "-")
	attributes := map[string]string{}
	for _, attribute := range goldenAttribute.FindAllStringSubmatch(match[2], -1) {
		attributes[attribute[1]] = goldenUnescape(attribute[2])
	}
	path := name
	if field := attributes["field"]; field != "" {
		path = field
	}
	fields.values[path] = words
	for key, value := range attributes {
		fields.values[path+"@"+key] = value
	}
	return blockEnd, ""
}

// goldenUnescape is an attribute value as texts.Block escaped it.
func goldenUnescape(value string) string {
	value = regexp.MustCompile(`&#([0-9]+);`).ReplaceAllStringFunc(value, func(ref string) string {
		n, _ := strconv.Atoi(ref[2 : len(ref)-1])
		return string(rune(n))
	})
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&amp;", "&").Replace(value)
}

// goldenKey is a line's key, unquoted where the encoder quoted it.
func goldenKey(key string) (string, error) {
	if strings.HasPrefix(key, `"`) {
		return strconv.Unquote(key)
	}
	return key, nil
}

// goldenEncoderLine records the field of one line of the encoder (match, goldenLine's) whose key
// the record declares as field, and returns the rows of a table that follow it. A table's rows
// are its next lines, each indented by two spaces.
func goldenEncoderLine(key string, match []string, field reflect.Type, text string, end int, fields *goldenFields) ([]int, string) {
	count, header, rest := match[2], match[3], match[4]
	if count == "" {
		value, err := goldenValue(rest)
		if err != nil {
			return nil, fmt.Sprintf("field %s: %v", key, err)
		}
		if rest == "[]" {
			fields.values[key+"#"] = "0"
			return nil, ""
		}
		fields.values[key] = value
		return nil, ""
	}
	n, _ := strconv.Atoi(count)
	fields.values[key+"#"] = count
	if table := strings.HasPrefix(match[0][len(match[1])+len(count)+2:], "{"); !table {
		items, err := goldenSplit(rest)
		if err != nil || len(items) != n {
			return nil, fmt.Sprintf("list %s: %d items for a header of %d (%v)", key, len(items), n, err)
		}
		for i, item := range items {
			fields.values[fmt.Sprintf("%s[%d]", key, i)] = item
		}
		fields.values[key] = strings.Join(items, ",")
		return nil, ""
	}
	if rest != "" {
		return nil, fmt.Sprintf("table %s has values on its header line", key)
	}
	names := strings.Split(header, ",")
	var element map[string]reflect.Type
	if field != nil {
		element = goldenDeclared(goldenElem(field))
	}
	for _, name := range names {
		if _, ok := element[name]; !ok {
			return nil, fmt.Sprintf("table %s: column %s is no field its rows declare", key, name)
		}
	}
	rows := make([]int, n)
	next := end + 1
	for i := range n {
		if next > len(text) {
			return nil, fmt.Sprintf("table %s has fewer rows than its header names", key)
		}
		rowEnd := strings.IndexByte(text[next:], '\n')
		if rowEnd < 0 {
			rowEnd = len(text)
		} else {
			rowEnd += next
		}
		row := text[next:rowEnd]
		if !strings.HasPrefix(row, "  ") || strings.HasPrefix(row, "   ") {
			return nil, fmt.Sprintf("table %s: row %q is not indented by two spaces", key, goldenCut(row))
		}
		cells, err := goldenSplit(row[2:])
		if err != nil || len(cells) != len(names) {
			return nil, fmt.Sprintf("table %s: row %q has %d cells for %d columns (%v)", key, goldenCut(row), len(cells), len(names), err)
		}
		for j, name := range names {
			fields.values[fmt.Sprintf("%s[%d].%s", key, i, name)] = cells[j]
		}
		next = rowEnd + 1
	}
	return rows, ""
}

// goldenValue decodes one primitive of the encoder: a quoted string, unquoted, or the text as it
// stands.
func goldenValue(text string) (string, error) {
	if strings.HasPrefix(text, `"`) {
		return strconv.Unquote(text)
	}
	return text, nil
}

// goldenSplit splits the comma-delimited primitives of a list or a row, a quoted one whole.
func goldenSplit(text string) ([]string, error) {
	var items []string
	for text != "" || len(items) == 0 {
		var item string
		if strings.HasPrefix(text, `"`) {
			end := 1
			for end < len(text) && text[end] != '"' {
				if text[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(text) {
				return items, fmt.Errorf("an unclosed quote in %q", goldenCut(text))
			}
			item, text = text[:end+1], text[end+1:]
		} else {
			item, text, _ = strings.Cut(text, ",")
			items = append(items, item)
			if text == "" {
				break
			}
			continue
		}
		value, err := goldenValue(item)
		if err != nil {
			return items, err
		}
		items = append(items, value)
		if text == "" {
			break
		}
		if !strings.HasPrefix(text, ",") {
			return items, fmt.Errorf("no comma after a quoted value in %q", goldenCut(text))
		}
		text = text[1:]
	}
	return items, nil
}

// goldenHookFields reads the hook prompt's text in the order R8 keeps (record.HookPrompt.Message,
// dispatch://INFRA-2551's R3, R5 and R7): its Legion hook first line, a blank line, the hook's words
// exactly as written, a blank line, then the facts as lines of the encoder.
func goldenHookFields(s *goldenSample, text string, fields *goldenFields) []string {
	first := fmt.Sprintf("Legion hook %s on %s: work went back from %s to %s.", s.want["hook"], s.Issue, s.want["from"], s.want["to"])
	head := first + "\n\n" + s.want["prompt"] + "\n\n"
	if !strings.HasPrefix(text, head) {
		return []string{fmt.Sprintf("the hook prompt %q does not open with its first line and then the hook's words as written", goldenCut(text))}
	}
	fields.kept = append(fields.kept, first)
	facts := goldenSample{Kind: s.Kind, Variant: s.Variant, Text: strings.TrimPrefix(text, head), want: s.want}
	return goldenTaskFields(&facts, texts.Entry{Kind: texts.HookPrompt, Record: texts.HookFacts{}}, fields)
}

// goldenPayloadFields reads a notice's or a role message's payload: an object whose every key, at
// every depth, the kind's record declares, each once, whose words object names each field that
// carries another author's words by its path with its author and time.
func goldenPayloadFields(s *goldenSample, entry texts.Entry, fields *goldenFields) []string {
	var problems []string
	if !bytes.HasPrefix(bytes.TrimSpace(s.Payload), []byte("{")) {
		return []string{fmt.Sprintf("it is published as %s, not as an object", goldenCut(string(s.Payload)))}
	}
	if duplicates := goldenDuplicateKeys(s.Payload); len(duplicates) > 0 {
		problems = append(problems, fmt.Sprintf("keys come twice: %v", duplicates))
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(s.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return append(problems, fmt.Sprintf("its payload is no JSON object: %v", err))
	}
	if entry.Form == texts.Notice || entry.Form == texts.ControllerNotice {
		keys := record.NoticeKeys(s.Kind)
		for key := range payload {
			if !slices.Contains(keys, key) {
				problems = append(problems, fmt.Sprintf("key %s is no key the %s record declares (record.NoticeKeys)", key, s.Kind))
			}
		}
	}
	problems = append(problems, goldenDeclaredPayload(reflect.TypeOf(entry.Record), payload, "")...)
	goldenFlatten(payload, "", fields.values)

	// The words fields as the record reads them back from the payload (texts.WordsPaths).
	held := reflect.New(reflect.TypeOf(entry.Record))
	if err := json.Unmarshal(s.Payload, held.Interface()); err != nil {
		return append(problems, fmt.Sprintf("its payload does not decode into the %s record: %v", s.Kind, err))
	}
	paths := texts.WordsPaths(held.Interface())
	words, _ := payload["words"].(map[string]any)
	for _, path := range paths {
		meta, ok := words[path].(map[string]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("field %s carries another author's words, and the words object does not name it", path))
			continue
		}
		if by, _ := meta["by"].(string); by == "" {
			problems = append(problems, fmt.Sprintf("the words object names %s with no author", path))
		}
		if at, _ := meta["at"].(string); at == "" {
			problems = append(problems, fmt.Sprintf("the words object names %s with no time", path))
		}
	}
	for path := range words {
		if !slices.Contains(paths, path) {
			problems = append(problems, fmt.Sprintf("the words object names %s, which carries no words", path))
		}
	}
	var prose strings.Builder
	for _, path := range slices.Sorted(goldenKeys(fields.values)) {
		if strings.HasPrefix(path, "words.") || slices.Contains(paths, path) || strings.HasSuffix(path, "#") {
			continue
		}
		prose.WriteString(fields.values[path])
		prose.WriteString("\n")
	}
	fields.prose = prose.String()
	return problems
}

// goldenKeys is the keys of m.
func goldenKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

// goldenDuplicateKeys is each key that comes twice in one object of encoded, at any depth.
func goldenDuplicateKeys(encoded []byte) []string {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	var duplicates []string
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name := key.(string)
				if seen[name] {
					duplicates = append(duplicates, name)
				}
				seen[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case json.Delim('['):
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return []string{err.Error()}
	}
	return duplicates
}

// goldenFlatten records each value of payload at its path: objects by their keys joined by dots,
// a list's items by their index, a list's count at path#, and a list of primitives joined by commas
// at its own path.
func goldenFlatten(value any, path string, values map[string]string) {
	switch v := value.(type) {
	case map[string]any:
		for key, inner := range v {
			at := key
			if path != "" {
				at = path + "." + key
			}
			goldenFlatten(inner, at, values)
		}
	case []any:
		values[path+"#"] = strconv.Itoa(len(v))
		var items []string
		for i, inner := range v {
			goldenFlatten(inner, fmt.Sprintf("%s[%d]", path, i), values)
			switch inner.(type) {
			case map[string]any, []any:
			default:
				items = append(items, values[fmt.Sprintf("%s[%d]", path, i)])
			}
		}
		if len(items) == len(v) {
			values[path] = strings.Join(items, ",")
		}
	case string:
		values[path] = v
	case json.Number:
		values[path] = v.String()
	case bool:
		values[path] = strconv.FormatBool(v)
	case nil:
		values[path] = "null"
	}
}

// goldenDeclared is the json name of each field of the struct t declares, with the field's type:
// the fields of an embedded struct with no tag in its place, as encoding/json reads it.
func goldenDeclared(t reflect.Type) map[string]reflect.Type {
	declared := map[string]reflect.Type{}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return declared
	}
	for i := range t.NumField() {
		field := t.Field(i)
		tag, tagged := field.Tag.Lookup("json")
		if tag == "-" {
			continue
		}
		if field.Anonymous && !tagged {
			for name, inner := range goldenDeclared(field.Type) {
				declared[name] = inner
			}
			continue
		}
		if !tagged || !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		declared[name] = field.Type
	}
	return declared
}

// goldenElem is the type a list of t holds, or t.
func goldenElem(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// goldenDeclaredPayload is each key of payload, at any depth, that the type t of its value does
// not declare.
func goldenDeclaredPayload(t reflect.Type, value any, path string) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var problems []string
	switch v := value.(type) {
	case map[string]any:
		switch {
		case t.Kind() == reflect.Map:
			for key, inner := range v {
				problems = append(problems, goldenDeclaredPayload(t.Elem(), inner, path+"."+key)...)
			}
		case t.Kind() == reflect.Struct && t != reflect.TypeFor[time.Time]():
			declared := goldenDeclared(t)
			for key, inner := range v {
				field, ok := declared[key]
				if !ok {
					problems = append(problems, fmt.Sprintf("key %s is no key its record declares", strings.TrimPrefix(path+"."+key, ".")))
					continue
				}
				problems = append(problems, goldenDeclaredPayload(field, inner, path+"."+key)...)
			}
		default:
			problems = append(problems, fmt.Sprintf("%s is an object where its record declares a %s", strings.TrimPrefix(path, "."), t))
		}
	case []any:
		if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
			return []string{fmt.Sprintf("%s is a list where its record declares a %s", strings.TrimPrefix(path, "."), t)}
		}
		for i, inner := range v {
			problems = append(problems, goldenDeclaredPayload(t.Elem(), inner, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return problems
}

// goldenNamesProblems is the check's rule that a text names its kind, and its issue where it has
// one: a task's kind: and issue: lines (the look task's first line names its issue, the hook
// prompt's its kind and issue, and the controller's start message its kind, word for word), and a
// payload's kind and issue keys under the summary `<kind> on <KEY>`.
func goldenNamesProblems(s *goldenSample, fields goldenFields) []string {
	var problems []string
	switch {
	case s.Kind == texts.ControllerStart:
		if !slices.ContainsFunc(fields.kept, func(line string) bool { return strings.HasPrefix(line, "Legion controller start:") }) {
			problems = append(problems, "no line names the controller's start")
		}
		return problems
	case s.Kind == texts.HookPrompt:
		if !slices.ContainsFunc(fields.kept, func(line string) bool { return strings.HasPrefix(line, "Legion hook ") }) {
			problems = append(problems, "no first line names the hook prompt")
		}
	case fields.values["kind"] != string(s.Kind):
		problems = append(problems, fmt.Sprintf("no line names its kind: kind is %q", fields.values["kind"]))
	}
	switch {
	case s.Issue == "":
	case s.Kind == texts.CheckLook:
		if !slices.ContainsFunc(fields.kept, func(line string) bool { return strings.HasPrefix(line, "Production check of "+s.Issue+": ") }) {
			problems = append(problems, "its first line does not name its issue")
		}
	case s.Kind == texts.HookPrompt:
		if !slices.ContainsFunc(fields.kept, func(line string) bool { return strings.Contains(line, " on "+s.Issue+": ") }) {
			problems = append(problems, "its first line does not name its issue")
		}
	case fields.values["issue"] != s.Issue:
		problems = append(problems, fmt.Sprintf("no line names its issue %s: issue is %q", s.Issue, fields.values["issue"]))
	}
	if s.Form != texts.Task && s.Summary != string(s.Kind)+" on "+s.Issue {
		problems = append(problems, fmt.Sprintf("summary %q, want %q", s.Summary, string(s.Kind)+" on "+s.Issue))
	}
	return problems
}

// goldenPairProblems is the check's rule over a kind's two renderings: no field but the kind
// shares a run of more than 40 characters between them, apart from what R8 keeps. Each field is
// read by its path with the index of a list taken out, its values in each rendering joined by a
// character the other never holds, so a run shared is a run of one value of each.
func goldenPairProblems(a, b *goldenSample) []string {
	entry, _ := texts.Lookup(a.Kind)
	fa, _ := goldenFieldsOf(a, entry)
	fb, _ := goldenFieldsOf(b, entry)
	exempt := map[string]bool{"kind": true}
	for _, item := range goldenKept {
		if item.covers(a) {
			for _, key := range item.keys {
				exempt[key] = true
			}
		}
	}
	join := func(values map[string]string, separator string) map[string]string {
		joined := map[string]string{}
		for path, value := range values {
			if strings.HasSuffix(path, "#") || strings.Contains(path, "@") {
				continue
			}
			field := goldenIndex.ReplaceAllString(path, "[]")
			if exempt[field] || exempt[strings.SplitN(field, ".", 2)[0]] || exempt[strings.SplitN(field, "[", 2)[0]] {
				continue
			}
			if goldenPrimitiveList(values, path) {
				continue
			}
			joined[field] += value + separator
		}
		return joined
	}
	ja, jb := join(fa.values, "\x00"), join(fb.values, "\x01")
	var problems []string
	for _, field := range slices.Sorted(goldenKeys(ja)) {
		other, ok := jb[field]
		if !ok {
			continue
		}
		if run := goldenSharedRun(ja[field], other, 41); run != "" {
			problems = append(problems, fmt.Sprintf("field %s shares the run %q between its two renderings", field, run))
		}
	}
	return problems
}

// goldenIndex is a list index in a field path.
var goldenIndex = regexp.MustCompile(`\[[0-9]+\]`)

// goldenPrimitiveList says whether values holds path as a list of primitives joined, whose items
// it holds at path[i] too.
func goldenPrimitiveList(values map[string]string, path string) bool {
	_, list := values[path+"#"]
	return list
}

// goldenSharedRun is a run of n characters that a and b both hold, or "".
func goldenSharedRun(a, b string, n int) string {
	ra, rb := []rune(a), []rune(b)
	if len(ra) < n || len(rb) < n {
		return ""
	}
	windows := make(map[string]bool, len(ra)-n+1)
	for i := 0; i+n <= len(ra); i++ {
		windows[string(ra[i:i+n])] = true
	}
	for i := 0; i+n <= len(rb); i++ {
		if window := string(rb[i : i+n]); windows[window] {
			return window
		}
	}
	return ""
}

// goldenFactsOf is a kind's facts that main's builder of it wrote (each id, reference, version,
// commit, phase and gate state), read from the builder named at b4a566cb, for the variants named
// (none: every variant). A key is the field path the sample carries the fact at. A link is a
// Dispatch item, which the sample names by a dispatch:// link and never by a bare id.
type goldenFactsOf struct {
	kind     texts.Kind
	variants []string
	from     string
	keys     []string
	links    []string
}

func (f goldenFactsOf) covers(s *goldenSample) bool {
	return s.Kind == f.kind && (len(f.variants) == 0 || slices.Contains(f.variants, s.Variant))
}

// problems is each fact of f that s lacks, or names by a bare id where a dispatch:// link belongs.
func (f goldenFactsOf) problems(s *goldenSample, fields goldenFields) []string {
	var problems []string
	for _, key := range append(slices.Clone(f.keys), f.links...) {
		want, given := s.want[key]
		if !given {
			problems = append(problems, fmt.Sprintf("the test gives no value of %s, a fact %s writes", key, f.from))
			continue
		}
		if got, ok := fields.values[key]; !ok || got != want {
			problems = append(problems, fmt.Sprintf("it lacks %s = %q, a fact %s writes (it has %q)", key, goldenCut(want), f.from, goldenCut(got)))
		}
	}
	for _, key := range f.links {
		if got := fields.values[key]; !strings.HasPrefix(got, "dispatch://") {
			problems = append(problems, fmt.Sprintf("%s names a Dispatch item by %q, not by its dispatch:// link", key, got))
		}
	}
	return problems
}

// goldenKeptOf is one thing R8 keeps in a kind's text, with what keeps it: lines a task holds
// word for word, and fields a sample holds with the values given (an empty value: any value).
// The fields are exempt from the rule over two renderings (goldenPairProblems), since R8 keeps
// them as they are.
type goldenKeptOf struct {
	kind     texts.Kind
	variants []string
	what     string
	keptBy   string
	lines    func(want map[string]string) []string
	keys     []string
	values   func(want map[string]string) map[string]string
}

func (k goldenKeptOf) covers(s *goldenSample) bool {
	return s.Kind == k.kind && (len(k.variants) == 0 || slices.Contains(k.variants, s.Variant))
}

// problems is each line and field of k that s lacks.
func (k goldenKeptOf) problems(s *goldenSample, fields goldenFields) []string {
	var problems []string
	if k.lines != nil {
		for _, line := range k.lines(s.want) {
			if !slices.Contains(fields.kept, line) {
				problems = append(problems, fmt.Sprintf("it lacks %q, which %s keeps word for word (%s)", goldenCut(line), k.keptBy, k.what))
			}
		}
	}
	if k.values != nil {
		for key, want := range k.values(s.want) {
			got, ok := fields.values[key]
			if !ok || want != "" && got != want {
				problems = append(problems, fmt.Sprintf("it lacks %s = %q, which %s keeps (%s); it has %q", key, want, k.keptBy, k.what, goldenCut(got)))
			}
		}
	}
	return problems
}

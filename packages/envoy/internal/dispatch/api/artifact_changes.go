package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The ways a top-level unit or a typed block of a document stands between two of its versions
// (artifactSectionChange, artifactBlockChange): the later version holds it otherwise than the
// earlier, holds it and the earlier does not, or the earlier holds it and the later does not. A
// unit - a `##` section, or a run of the text outside every section - has a fourth, moved: both
// versions hold it with the same text, and its place among the document's top-level units is
// another (movedUnits). A block has no fourth: a block that moves changes the text of the units
// it moves within or between, and those are named.
const (
	changeChanged = "changed"
	changeAdded   = "added"
	changeRemoved = "removed"
	changeMoved   = "moved"
)

// The places a run of the text outside every `##` section stands at (artifactSectionChange's
// Place): above the first `##` section, or directly after one, which AfterHeading and
// AfterOccurrence name.
const (
	placeBeforeFirstSection = "before_first_section"
	placeAfterSection       = "after_section"
)

// artifactChanges is the body of GET /api/v1/artifacts/{id}/changes: what changed in a document
// between two of its stored versions, read off their markdown. It names what it compared - the
// document and the two versions - so a caller that keeps it can check what it holds.
//
// Both versions are compared with their decision blocks cut out (docs.WithoutAskBlocks), so an
// answer a person gave, or an ask added, reworded or moved, changes no section and no block.
//
// Exactly one of three things is true of a change set, and compareVersions decides which in one
// place. DecisionOnly says the cut is the whole change: the two stored texts differ, and they cut
// to the same text. Unchanged says the two versions differ in nothing any unit or block holds:
// their stored texts are the same, or they cut to texts that name no unit and no block and are
// not decision-only. Otherwise Sections or Blocks names at least one item. So a change set that
// names nothing always says which of the two it is, and one that says neither always names what
// changed: a version that only puts its sections in another order names the sections it moved.
type artifactChanges struct {
	ArtifactID   string `json:"artifact_id"`
	From         int    `json:"from"`
	To           int    `json:"to"`
	DecisionOnly bool   `json:"decision_only"`
	Unchanged    bool   `json:"unchanged"`
	// Sections are the document's top-level units whose text differs, that moved, or that one
	// version alone holds (cutUnits): each `##` section, and each run of the text outside every
	// `##` section - the title and what stands above the first `##`, or what follows a `#`
	// heading up to the next `##` - under the heading "" and occurrence 0 with the place it
	// stands at. They are in the later version's document order, a moved unit where the later
	// version holds it, and then the units only the earlier version holds, in its order.
	Sections []artifactSectionChange `json:"sections"`
	// Blocks are the typed blocks whose text or any attribute differs, in the same order
	// (cutTypedBlocks), each named by an id its own stored version carries. No ask block is one
	// of them.
	Blocks []artifactBlockChange `json:"blocks"`
}

// artifactSectionChange is one top-level unit of a document that changed. A `##` section is
// named by its heading's text as a heading anchor reads it (cutUnits: pmdoc.TextContent, a line
// break in it a line feed), and by how many sections before it carry that text, 0 for the first;
// a `##` heading with no words is counted from occurrence 1. The heading "" with occurrence 0 is
// one run of the text outside every `##` section, and Place says which: before_first_section, or
// after_section with AfterHeading and AfterOccurrence naming the `##` section that stands directly
// before the run. The place is the run's in the later version for a changed, added or moved run,
// and in the earlier version for a removed one. A `##` section carries none of the three. Change
// is changed, added, removed or moved (changeChanged and the three beside it).
type artifactSectionChange struct {
	Heading         string  `json:"heading"`
	Occurrence      int     `json:"occurrence"`
	Change          string  `json:"change" enum:"changed,added,removed,moved"`
	Place           string  `json:"place,omitempty" enum:"before_first_section,after_section"`
	AfterHeading    *string `json:"after_heading,omitempty"`
	AfterOccurrence *int    `json:"after_occurrence,omitempty"`
}

// artifactBlockChange is one typed block that changed: its `#id`, its type in the later version
// (in the earlier one for a removed block), and how it changed.
type artifactBlockChange struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Change string `json:"change" enum:"changed,added,removed"`
}

// getArtifactChanges answers what changed between versions from and to of one document
// (artifactChanges). It reads the document as GET …/versions/{number} reads it: an authenticated
// caller, and the artifact's own row by its id (loadArtifactRowByID), the row that route's read
// finds and the 404 it answers, without the versions and the approval that read goes on to carry,
// which this one uses neither of and which say nothing of who may read the document. So a caller
// reads here exactly what it may read there. An id that names no artifact is that route's 404
// ARTIFACT_NOT_FOUND, and a version the document does not have is its 404 NOT_FOUND. The artifact
// is read before the query is, so an id that names nothing answers that 404 whatever the query
// holds, as idRoutes says it does. Both numbers must be version numbers a document can have
// (parseVersionNumber) and from below to (400 INVALID_VERSION).
//
// It reads the two versions' stored markdown and parses it, never the block index, so it answers
// for a large document as soon as both versions are stored, without waiting for the index's
// deferred pass over its blocks (deferBlockIndexBlocks).
func (s *server) getArtifactChanges(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	who, ok := s.artifactReaderOf(w, r)
	if !ok {
		return
	}
	artifact, err := loadArtifactRowByID(r.Context(), s.deps.Store.Pool, who, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	query := r.URL.Query()
	from, fromValid := parseVersionNumber(query.Get("from"))
	to, toValid := parseVersionNumber(query.Get("to"))
	if !fromValid || !toValid {
		writeError(w, "INVALID_VERSION", http.StatusBadRequest, fmt.Sprintf("from and to must be version numbers from 1 to %d", maxIntegerColumn))
		return
	}
	if from >= to {
		writeError(w, "INVALID_VERSION", http.StatusBadRequest, fmt.Sprintf("from (%d) must be below to (%d)", from, to))
		return
	}
	before, after, err := s.twoVersionsMarkdown(r.Context(), artifact.ID, from, to)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	changes, err := compareVersions(before, after)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	changes.ArtifactID, changes.From, changes.To = artifact.ID, from, to
	WriteJSON(w, http.StatusOK, changes)
}

// twoVersionsMarkdown is the stored markdown of versions from and to of one document, in one
// read (versionsMarkdown). A version the document does not have is the 404 NOT_FOUND GET
// …/versions/{number} answers for it, naming the version.
func (s *server) twoVersionsMarkdown(ctx context.Context, artifactID string, from, to int) (string, string, error) {
	markdown, err := s.versionsMarkdown(ctx, artifactID, []int{from, to})
	if err != nil {
		return "", "", err
	}
	for _, number := range []int{from, to} {
		if _, held := markdown[number]; !held {
			return "", "", errorf(http.StatusNotFound, "NOT_FOUND", "the document has no version %d", number)
		}
	}
	return markdown[from], markdown[to], nil
}

// cutVersion is one stored version as a change set compares it: the text it cuts to once its
// decision blocks are left out, that cut text's top-level units in document order (cutUnits),
// and its typed blocks.
type cutVersion struct {
	text   string
	units  []cutUnit
	blocks []cutBlock
}

// cutUnit is one top-level unit of a cut version: a `##` section, or one run of the text outside
// every section. key names it across the two versions (placeKey): a section's is its own from the
// cut, and a run's holds its text alone until keyRuns names it across the two. text is its text
// with the blank lines at its end removed. after is where a run stands: the key of the `##`
// section directly before it, or the zero key, which no `##` section has, for a run above the
// first section. A section's after is the zero key and is never read.
type cutUnit struct {
	key   placeKey
	text  string
	after sectionKey
}

// isRun reports whether the unit is a run of the text outside every section: a run's key holds
// the zero section key, which no `##` section's does (placeKey).
func (unit cutUnit) isRun() bool {
	return unit.key.section == sectionKey{}
}

// named is the unit as a change set names it, with how it changed (artifactSectionChange): a
// `##` section by its heading and occurrence, and a run under the heading "" and occurrence 0
// with the place it stands at in the version it was read from.
func (unit cutUnit) named(change string) artifactSectionChange {
	if !unit.isRun() {
		return artifactSectionChange{Heading: unit.key.section.heading, Occurrence: unit.key.section.occurrence, Change: change}
	}
	named := artifactSectionChange{Change: change, Place: placeBeforeFirstSection}
	if unit.after != (sectionKey{}) {
		heading, occurrence := unit.after.heading, unit.after.occurrence
		named.Place, named.AfterHeading, named.AfterOccurrence = placeAfterSection, &heading, &occurrence
	}
	return named
}

// placeKey names one top-level unit of a cut version across its versions: a `##` section by its
// key (section), and a run of the text outside every section by its own text (run) and then in one
// of two ways (keyRuns). A run both versions hold with the same text at the same place is named by
// that place (placed, at), and every other run by how many of the other runs of its version before
// it hold the same text (occurrence). A run's section is the zero key, which no `##` section has,
// and a run's text is never empty, so a run and a section never share a placeKey and no unit has
// the zero placeKey; placed tells a run named by its place from one named by its occurrence.
type placeKey struct {
	section    sectionKey
	run        string
	placed     bool
	at         sectionKey
	occurrence int
}

// cutBlock is one typed block of a cut version, by its block id.
type cutBlock struct {
	id   string
	node *pmdoc.Node
}

// compareVersions is the change set from the stored markdown before to the stored markdown after
// (artifactChanges), its document and version numbers left for the caller to fill in.
//
// It names what differs first (nameChanges), and then says whether the change set is unchanged,
// here and in no other place: one that is not decision-only and names no unit and no block is
// unchanged, whatever its two texts were. So no pair of texts can answer a change that names
// nothing. Two stored texts that are the same are never parsed, since they cut to one text and
// name nothing, and the same line makes them unchanged.
func compareVersions(before, after string) (artifactChanges, error) {
	changes := artifactChanges{Sections: []artifactSectionChange{}, Blocks: []artifactBlockChange{}}
	if before != after {
		earlier, err := readCutVersion(before)
		if err != nil {
			return artifactChanges{}, err
		}
		later, err := readCutVersion(after)
		if err != nil {
			return artifactChanges{}, err
		}
		if earlier.text == later.text {
			changes.DecisionOnly = true
		} else {
			nameChanges(&changes, earlier, later)
		}
	}
	changes.Unchanged = !changes.DecisionOnly && len(changes.Sections) == 0 && len(changes.Blocks) == 0
	return changes, nil
}

// nameChanges adds to changes each top-level unit and each typed block that differs between
// earlier and later, in the order changesByKey reports them. A `##` section is paired across the
// versions by its key. A run of the text outside every section is named by three rules, in turn:
//
//   - a run both versions hold is one run in both: first a run with the same text at the same
//     place, and then, among the runs left, the n-th run of a text in each version (keyRuns). Such
//     a run is in its place when it stands in the longest common subsequence of the two versions'
//     units, and moved, where the later version holds it, when it stands outside it (movedUnits);
//   - of the runs left, two that stand at one place, above the first section or after the same
//     `##` section, are one run whose text changed (runsPairedByPlace);
//   - a run left after that is added when the later version holds it, and removed when the
//     earlier does.
func nameChanges(changes *artifactChanges, earlier, later cutVersion) {
	keyRuns(earlier.units, later.units)
	paired := runsPairedByPlace(earlier.units, later.units)
	changesByKey(earlier.units, later.units,
		func(unit cutUnit) placeKey {
			if partner, held := paired[unit.key]; held {
				return partner
			}
			return unit.key
		},
		func(a, b cutUnit) bool { return a.text == b.text },
		movedUnits(earlier.units, later.units),
		func(unit cutUnit, change string) {
			changes.Sections = append(changes.Sections, unit.named(change))
		})
	changesByKey(earlier.blocks, later.blocks,
		func(block cutBlock) string { return block.id },
		func(a, b cutBlock) bool { return a.node.Equal(b.node) },
		nil,
		func(block cutBlock, change string) {
			changes.Blocks = append(changes.Blocks, artifactBlockChange{ID: block.id, Type: block.node.Type, Change: change})
		})
}

// keyRuns sets the key of each run of the text outside every section of both versions, which
// cutUnits keyed by its text alone, so that the runs both versions hold share one key (placeKey).
// A run of earlier and a run of later with the same text at the same place, above the first
// section or after the same `##` section, share a key naming that place: a place holds at most
// one run of a version, so the text and the place name one run in each. Every other run is keyed
// by its text and by how many of the runs left in its version before it hold the same text, so
// the n-th of the runs left with one text in earlier and in later share a key, which is a run that
// moved. A run that both versions hold where they hold it is never counted among the others, so
// one copy of a repeated text added or removed shifts the count of no other copy.
func keyRuns(earlier, later []cutUnit) {
	type spot struct {
		text  string
		after sectionKey
	}
	inEarlier := map[spot]struct{}{}
	for _, unit := range earlier {
		if unit.isRun() {
			inEarlier[spot{unit.text, unit.after}] = struct{}{}
		}
	}
	inBoth := map[spot]struct{}{}
	for _, unit := range later {
		if _, held := inEarlier[spot{unit.text, unit.after}]; unit.isRun() && held {
			inBoth[spot{unit.text, unit.after}] = struct{}{}
		}
	}
	for _, units := range [][]cutUnit{earlier, later} {
		left := map[string]int{}
		for index := range units {
			unit := &units[index]
			if !unit.isRun() {
				continue
			}
			if _, held := inBoth[spot{unit.text, unit.after}]; held {
				unit.key = placeKey{run: unit.text, placed: true, at: unit.after}
				continue
			}
			unit.key = placeKey{run: unit.text, occurrence: left[unit.text]}
			left[unit.text]++
		}
	}
}

// readCutVersion parses one stored version with the parser every read of a stored text uses
// (pmdoc.ParseRendering, as typedBlocksIn and docs.CutDecisionBlocks parse), cuts its decision
// blocks out of the tree and renders what is left once, which is the text docs.CutDecisionBlocks
// answers for it, with the offset of every block in that text.
func readCutVersion(markdown string) (cutVersion, error) {
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return cutVersion{}, err
	}
	cut := docs.WithoutAskBlocks(tree)
	text, offsets, err := pmdoc.RenderWithBlockOffsets(cut)
	if err != nil {
		return cutVersion{}, err
	}
	return cutVersion{text: text, units: cutUnits(cut, text, offsets), blocks: cutTypedBlocks(cut, markdown)}, nil
}

// cutUnits cuts a version's cut text into its top-level units, in document order: its `##`
// sections and the runs of the text no `##` section holds. A top-level `##` heading opens a
// section, the next top-level heading of its level or higher closes it - `#` included, which opens
// none - and a deeper heading is text of the section it stands in. A section is keyed by its
// heading's text and by how many sections before it carry that text, and its text runs from its
// heading to where it closes, with the blank lines at its end removed. Only a top-level heading
// cuts, since one inside a quote, a list or a callout is text of the block that holds it. A `##`
// heading with no words is keyed from occurrence 1, so no section takes the zero key.
//
// Each heading's level and text are read off its node and never off the cut text, since a heading
// is not always written with `#`s: the renderer writes a `#` or `##` heading that holds a line
// break as its lines and an underline (renderer.heading's setext form), as the browser editor
// writes it. The text is the heading's as a heading anchor reads it (pmdoc.TextContent, which
// FindHeading matches a title against): its words without markdown's syntax, a line break in it a
// line feed, so a reader handed a section's heading can find the section by it. The parse writes
// a heading's level as an int.
//
// A run is all the text between two sections, or above the first, or below the last: the title
// and the text above the first `##`, or the text from a `#` heading that closes a section to the
// next `##`, with the blank lines at its end removed. So each place holds at most one run, and a
// change to the title or to the text under a `#` heading is named like a change to any section.
// A run stands after the section that closed where it starts, or above the first section
// (cutUnit's after), and is keyed by its text alone: keyRuns keys it across the two versions.
func cutUnits(doc *pmdoc.Node, text string, offsets []pmdoc.BlockOffset) []cutUnit {
	headings := map[string]*pmdoc.Node{}
	for _, child := range doc.Children {
		if child.Type == "heading" {
			id, _ := child.Attrs[pmdoc.BlockIDAttr].(string)
			headings[id] = child
		}
	}
	var units []cutUnit
	seen := map[string]int{"": 1}
	// key is the open section's key, and last the key of the section that closed last: the zero
	// key until one has, which is the place of a run above the first section.
	var key, last sectionKey
	// start is where the open section starts, -1 while none is open, and free where the text
	// outside every section that runs up to the next section starts.
	start, free := -1, 0
	keepOutside := func(end int) {
		if run := strings.TrimRight(text[free:end], "\n"); run != "" {
			units = append(units, cutUnit{key: placeKey{run: run}, text: run, after: last})
		}
	}
	closeAt := func(end int) {
		if start >= 0 {
			units = append(units, cutUnit{key: placeKey{section: key}, text: strings.TrimRight(text[start:end], "\n")})
			start, free, last = -1, end, key
		}
	}
	for _, offset := range offsets {
		node, top := headings[offset.ID]
		if !top {
			continue
		}
		level, _ := node.Attrs["level"].(int)
		if level < 1 || level > 2 {
			continue
		}
		closeAt(offset.From)
		if level == 2 {
			keepOutside(offset.From)
			heading := pmdoc.TextContent(node)
			key = sectionKey{heading: heading, occurrence: seen[heading]}
			seen[heading]++
			start = offset.From
		}
	}
	closeAt(len(text))
	keepOutside(len(text))
	return units
}

// cutTypedBlocks is every typed block of a cut tree whose id its stored version carries (stored,
// that version's markdown), in document order: the cut left no ask block in it, so these are its
// requirements, checks, plan tasks and callouts, and whatever other type the block schema
// declares.
//
// The parse gives every block an id of its own (pmdoc.EnsureBlockIDs): it keeps an id the stored
// text names for the first block that names it, and mints a fresh one for each later block that
// names the same id, an ask block among them, since the ask is cut only after the ids are
// stamped. A minted id is in no stored version, so a change named under it would be an addition
// and a removal of blocks neither document holds, which the daemon keeps for good. Such a block
// is left out. The check is made on the stored text itself: a stored version is a rendering, so
// it names each id the way the renderer writes a typed block's id (pmdoc.TypedBlockIDAttribute),
// and an id the parse minted, a fresh random one, is in no stored text.
//
// The stored text is read once, at the first typed block, and each block's id is then looked up
// in what that read found (writtenIDs), with the answer a search of the whole text for it would
// give. So the work grows with the text's size, never with its size times its blocks, and every
// request reads two versions.
func cutTypedBlocks(doc *pmdoc.Node, stored string) []cutBlock {
	var written *writtenIDs
	var blocks []cutBlock
	pmdoc.Walk(doc, func(node *pmdoc.Node) bool {
		if !pmdoc.IsTypedBlock(node.Type) {
			return true
		}
		if written == nil {
			written = writtenIDsIn(stored)
		}
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); written.names(id) {
			blocks = append(blocks, cutBlock{id: id, node: node})
		}
		return true
	})
	return blocks
}

// writtenIDs is what one stored text holds where a typed block's opening line could write an id.
// pmdoc.TypedBlockIDAttribute writes an id one of two ways: `#` and the id, for an id holding none
// of the characters a written `#id` cannot hold, `#` among them, or `id="…"`, the id's `"` written
// as a character reference. shortcuts is the text after each `#` of the stored text up to its next
// `#` or its end, sorted, and quoted is the text between each `id="` and the first `"` after it.
type writtenIDs struct {
	shortcuts []string
	quoted    map[string]struct{}
}

// writtenIDsIn reads stored once, from its first byte to its last, for its `#`s and its `"`s
// (writtenIDs). A `"` closes the value of the `id="` before it, and is itself the end of an `id="`
// that opens the next value when `id=` stands before it: `id="aid="b"` holds both `id="aid="` and
// `id="b"`. A text after a `#` that is empty holds no id, since every id written after a `#` has a
// character, and is not kept. Each step of the scan tells writtenIDReads what it reads: each
// search for the next `#` or `"`, which reads up to and with the byte it finds, or to the text's
// end where it finds none, each value it keeps, and the `id=` it looks for before each `"`. So the
// counted total is the scan's own work, and a scan that read the text again for each id would
// count each of those reads.
func writtenIDsIn(stored string) *writtenIDs {
	written := &writtenIDs{quoted: map[string]struct{}{}}
	// hash is where the last `#` stands and value where the open `id="` value starts, -1 for none.
	hash, value := -1, -1
	shortcut := func(end int) {
		if hash >= 0 && end > hash+1 {
			written.shortcuts = append(written.shortcuts, stored[hash+1:end])
		}
	}
	for at := 0; at < len(stored); at++ {
		next := strings.IndexAny(stored[at:], `#"`)
		if next < 0 {
			readOf(len(stored) - at)
			break
		}
		readOf(next + 1)
		at += next
		if stored[at] == '#' {
			shortcut(at)
			hash = at
			continue
		}
		if value >= 0 {
			readOf(at - value)
			written.quoted[stored[value:at]] = struct{}{}
			value = -1
		}
		readOf(min(at, len("id=")))
		if strings.HasSuffix(stored[:at], "id=") {
			value = at + 1
		}
	}
	shortcut(len(stored))
	slices.SortFunc(written.shortcuts, compareWritten)
	return written
}

// names reports whether the stored text holds the id as pmdoc.TypedBlockIDAttribute writes it,
// which is what strings.Contains answers for the text and the written id. Written `id="…"`, the
// value holds no `"`, so the text holds it exactly where one of its `id="`s is followed by the
// value and then its first `"`: where the value is one of quoted. Written `#` and the id, the id
// holds no `#`, so the text holds it exactly where the id begins the text after one of its `#`s:
// where it begins one of shortcuts. Of the sorted shortcuts, one begins with the id exactly when
// the first that is not below the id does, since every text between the id and a text that
// begins with it begins with it too.
func (written *writtenIDs) names(id string) bool {
	spelled := pmdoc.TypedBlockIDAttribute(id)
	if shortcut, ok := strings.CutPrefix(spelled, "#"); ok {
		at, _ := slices.BinarySearchFunc(written.shortcuts, shortcut, compareWritten)
		if at == len(written.shortcuts) {
			return false
		}
		readOf(len(shortcut))
		return strings.HasPrefix(written.shortcuts[at], shortcut)
	}
	quoted := strings.TrimSuffix(strings.TrimPrefix(spelled, `id="`), `"`)
	readOf(len(quoted))
	_, held := written.quoted[quoted]
	return held
}

// compareWritten is strings.Compare, which reads at most the shorter of the two texts, and tells
// writtenIDReads so.
func compareWritten(a, b string) int {
	readOf(min(len(a), len(b)))
	return strings.Compare(a, b)
}

// writtenIDReads, while a test sets it, is told how many bytes each step of a typed-block cut
// reads at most: each step of the scan of the stored text (writtenIDsIn), each comparison of two
// texts (compareWritten), and each look up of a written id (writtenIDs.names). A test bounds that
// work by the size of the text, as pmdoc's growsLinearly (pmdoc/linear_test.go) bounds a parse's
// and a render's, but by counting what the cut does rather than timing it on a machine another
// process may load. The server leaves it nil.
var writtenIDReads func(bytes int)

// readOf tells writtenIDReads, when a test has set it, of a read of bytes bytes.
func readOf(bytes int) {
	if writtenIDReads != nil {
		writtenIDReads(bytes)
	}
}

// movedUnits is the keys of the units both versions hold whose place among the document's
// top-level units is another in later than in earlier (cutUnits): its `##` sections and each run
// of the text outside every section, a run standing above the first `##` or opening at a `#`
// heading. A `##` section stands under one `#` heading or another only as the runs around it say,
// so a section moved across a `#` heading keeps the order of the `##` sections, and only the order
// of all the units shows the move.
//
// The two orders are the units both versions hold, each in its own version's order. A unit is in
// its place when it is in one longest common subsequence of the two orders, and moved when it is
// outside it, so the units named moved are the fewest that put earlier's order into later's. Both
// orders hold the same units, each once, so that subsequence is a longest sequence of later's
// units whose places in earlier rise.
//
// Where several sequences are that long, the one taken keeps the later version's earliest units:
// its first unit stands as early in later as the first of any sequence that long can, then its
// second, and so on. So of two units that swapped places, the one later holds second is moved,
// and a section lifted from last to first is the one moved, its neighbours' sequence being the
// only longest one. A run's key holds its text (keyRuns), so a run moved is a `#` heading's text
// standing at another place, and a section moved across a `#` heading names whichever of the two
// the later version holds second.
//
// The keys are of a unit's place alone: changesByKey names changed a section whose text differs,
// wherever it stands, and moved only a unit whose text is the same.
func movedUnits(earlier, later []cutUnit) map[placeKey]struct{} {
	place := make(map[placeKey]int, len(earlier))
	for index, unit := range earlier {
		place[unit.key] = index
	}
	// units are the units both versions hold in later's order, and places each one's index in
	// earlier.
	var units []placeKey
	var places []int
	for _, unit := range later {
		if at, holds := place[unit.key]; holds {
			units = append(units, unit.key)
			places = append(places, at)
		}
	}
	// longest[i] is the length of the longest rising sequence that starts at places[i], read from
	// the right: starts[n] is the highest place a sequence of n+1 starts at among the places read
	// so far, so starts falls as n grows, and a sequence starting at places[i] extends one of each
	// length whose start is above it.
	longest := make([]int, len(places))
	var starts []int
	for i := len(places) - 1; i >= 0; i-- {
		n := sort.Search(len(starts), func(n int) bool { return starts[n] < places[i] })
		longest[i] = n + 1
		if n == len(starts) {
			starts = append(starts, places[i])
		} else {
			starts[n] = places[i]
		}
	}
	// The sequence is taken from the left: each unit is its next when a sequence as long as the
	// rest of it starts there above its last place, and every other unit is moved.
	moved := map[placeKey]struct{}{}
	need, last := len(starts), -1
	for i, at := range places {
		if longest[i] == need && at > last {
			need, last = need-1, at
			continue
		}
		moved[units[i]] = struct{}{}
	}
	return moved
}

// runsPairedByPlace pairs the runs of the text outside every section that the other version
// holds no run of the same key for (nameChanges' second rule): a run of earlier whose key later
// does not hold, and a run of later whose key earlier does not hold, that stand at the same place,
// above the first section or after the same `##` section. It answers, for each such run of
// earlier, its partner's key, which changesByKey compares it under, so the two are one run whose
// text changed. A place holds at most one run of a version (cutUnits), so a run has one partner
// at most, and earlier holds no unit under its partner's key. A run with no partner keeps its own
// key, so changesByKey names it added or removed.
func runsPairedByPlace(earlier, later []cutUnit) map[placeKey]placeKey {
	keys := func(units []cutUnit) map[placeKey]struct{} {
		held := make(map[placeKey]struct{}, len(units))
		for _, unit := range units {
			held[unit.key] = struct{}{}
		}
		return held
	}
	earlierKeys, laterKeys := keys(earlier), keys(later)
	// left is each run of later whose key earlier does not hold, by the place it stands at.
	left := map[sectionKey]placeKey{}
	for _, unit := range later {
		if _, held := earlierKeys[unit.key]; unit.isRun() && !held {
			left[unit.after] = unit.key
		}
	}
	paired := map[placeKey]placeKey{}
	for _, unit := range earlier {
		if _, held := laterKeys[unit.key]; !unit.isRun() || held {
			continue
		}
		if partner, found := left[unit.after]; found {
			paired[unit.key] = partner
		}
	}
	return paired
}

// changesByKey compares one kind of item of two versions by key, and hands report each item that
// changed with how it changed: first each item of later that earlier does not hold (added), holds
// otherwise, as same judges (changed), or holds as it is at another place, as moved names (moved),
// in later's order, and then each item of earlier that later does not hold (removed), in earlier's
// order. moved is nil for a kind of item whose place is never named.
func changesByKey[T any, K comparable](earlier, later []T, key func(T) K, same func(a, b T) bool, moved map[K]struct{}, report func(item T, change string)) {
	held := make(map[K]T, len(earlier))
	for _, item := range earlier {
		held[key(item)] = item
	}
	kept := make(map[K]struct{}, len(later))
	for _, item := range later {
		name := key(item)
		kept[name] = struct{}{}
		before, holds := held[name]
		_, elsewhere := moved[name]
		switch {
		case !holds:
			report(item, changeAdded)
		case !same(before, item):
			report(item, changeChanged)
		case elsewhere:
			report(item, changeMoved)
		}
	}
	for _, item := range earlier {
		if _, holds := kept[key(item)]; !holds {
			report(item, changeRemoved)
		}
	}
}

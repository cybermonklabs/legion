package leakguard

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// NotesRef is the ref the guard's blob record is kept under: git notes on blobs, one note per file
// version, which `legion leak-check record` writes (Guard.WriteRecord) and a push reads
// (Guard.readCover). A clone that pushes reads the record only once it has fetched this ref.
//
// A note is lines of the form "pass <key>", one per key a writer recorded the blob under, in
// lower-case hex. Its last line is the one written last, and a writer keeps at most recordLines of
// them. The line says that no content pattern of the rules the key names matches any line of the
// blob (rules.anyLineMatches), so a push whose rules have that key needs to read none of the
// blob's lines.
//
// Whoever can write this ref in the pushing clone, or in the repository the clone fetches it from,
// can make a push skip a file version's content. Nothing here protects against that: the ref is as
// trusted as the repository's branches are.
const NotesRef = "refs/notes/legion-leak-guard"

// recordTag is the first bytes the record's key is a sha256 of, which names this record and its
// first shape: a change of what a line of a note means is a new tag, and so a new key.
const recordTag = "legion-leak-guard record 1\x00"

// gitleaksModule is the detection engine's module, whose version the key holds.
const gitleaksModule = "github.com/zricethezav/gitleaks/v8"

// recordLines is how many lines a note keeps: the writer adds its own line last and drops the
// oldest past this many, so the keys of several releases can share one note.
const recordLines = 8

// recordBatchBlobs and recordBatchBytes bound one read of the record writer: at most this many
// blobs and this many bytes of them, one blob larger than the bytes alone.
const (
	recordBatchBlobs = 256
	recordBatchBytes = 64 << 20
)

// pathspecBytes is the most pathspec one diff-tree of a covered read is given, so that a commit
// of many uncovered files is read in several gits rather than one whose arguments the kernel
// refuses.
const pathspecBytes = 128 << 10

// errNoRecord is a repository that holds no NotesRef at all, which a push reads every file of and
// says nothing about.
var errNoRecord = errors.New("no blob record")

// recordKeyOf is the key of a record made under rules (the rule file's bytes), engine (the
// gitleaks module's version) and goVersion (runtime.Version): the lower-case hex sha256 of
// recordTag, the hex sha256 of rules, a zero byte, engine, a zero byte and goVersion. A change of
// any of them is a new key, and a record under the old key then covers nothing.
func recordKeyOf(rules, engine, goVersion string) string {
	rulesSum := sha256.Sum256([]byte(rules))
	sum := sha256.New()
	sum.Write([]byte(recordTag))
	sum.Write([]byte(hex.EncodeToString(rulesSum[:])))
	sum.Write([]byte{0})
	sum.Write([]byte(engine))
	sum.Write([]byte{0})
	sum.Write([]byte(goVersion))
	return hex.EncodeToString(sum.Sum(nil))
}

// engineVersion is the version of the gitleaks module this binary was built with, its
// replacement's where the module is replaced, and "" where the build says none: a binary built
// from a work tree of the engine has no version that names what it detects.
func engineVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path != gitleaksModule {
			continue
		}
		if dep.Replace != nil {
			dep = dep.Replace
		}
		if dep.Version == "(devel)" {
			return ""
		}
		return dep.Version
	}
	return ""
}

// recordKey is this binary's key (recordKeyOf of the embedded rule file, the engine's version and
// Go's), or why it has none: an engine version the build does not say; rules that decode what
// they read, whose decoded forms the writer does not check; and a rule that matches a path alone
// only where its keywords are in the text, which the reader's probe of a path (cover.coverable)
// would not see. With no key, no record is written and none is read.
func (g *Guard) recordKey(engine *rules) (string, error) {
	if engine.detector.MaxDecodeDepth > 0 {
		return "", errors.New("the rules decode what they read, and the record checks no decoded form")
	}
	for _, rule := range engine.detector.Config.Rules {
		if rule.Regex == nil && len(rule.Keywords) > 0 {
			return "", fmt.Errorf("the rule %q matches a path only where its keywords are in the text", rule.RuleID)
		}
	}
	version := engineVersion()
	if version == "" {
		return "", errors.New("the build names no version of the detection engine")
	}
	return recordKeyOf(cmp.Or(g.keyRules, ruleFile), version, runtime.Version()), nil
}

// cover is what the blob record covers for one push: the blob ids whose note holds this binary's
// key, and, of the file versions the push adds, the ones the record covered and the ones the
// rules read.
type cover struct {
	engine *rules
	blobs  map[string]bool
	// covered and read are the push's new file versions by blob id: the ones the record covered,
	// whose lines were not read, and the ones the rules read.
	covered, read map[string]bool
}

// readCover is the record of the repository in dir, for a push whose rules are engine, or nil,
// which reads every file version: where the repository holds no NotesRef (said nowhere), and where
// the ref names no commit, cannot be read, or has no key this binary can read it under, which is
// said on Options.Out. A record never refuses a push.
func (g *Guard) readCover(ctx context.Context, dir string, engine *rules) *cover {
	blobs, err := g.readRecord(ctx, dir, engine)
	if err != nil {
		if !errors.Is(err, errNoRecord) {
			g.say("leak guard: the blob record %s could not be read, so the rules read every file version of the push: %v", NotesRef, err)
		}
		return nil
	}
	return &cover{engine: engine, blobs: blobs, covered: map[string]bool{}, read: map[string]bool{}}
}

// report says how many of the push's new file versions the record covered and how many the rules
// read. A push with no record says nothing.
func (c *cover) report(out io.Writer) {
	if c == nil || out == nil {
		return
	}
	fmt.Fprintf(out, "leak guard: the blob record %s covered %d of the file versions the push adds, and the rules read the other %d\n", NotesRef, len(c.covered), len(c.read))
}

// say writes one line to Options.Out, where there is one.
func (g *Guard) say(format string, args ...any) {
	if g.opts.Out != nil {
		fmt.Fprintf(g.opts.Out, format+"\n", args...)
	}
}

// readRecord is the set of blob ids the record in dir covers under this binary's key: the notes
// tree listed once, and each distinct note read once.
func (g *Guard) readRecord(ctx context.Context, dir string, engine *rules) (map[string]bool, error) {
	tip, err := g.notesTip(ctx, dir)
	if err != nil {
		return nil, err
	}
	key, err := g.recordKey(engine)
	if err != nil {
		return nil, err
	}
	notes, err := g.listNotes(ctx, dir, tip)
	if err != nil {
		return nil, err
	}
	texts, err := g.noteTexts(ctx, dir, notes)
	if err != nil {
		return nil, err
	}
	line := "pass " + key
	blobs := map[string]bool{}
	for blob, n := range notes {
		if holdsLine(texts[n.note], line) {
			blobs[blob] = true
		}
	}
	return blobs, nil
}

// notesTip is the commit NotesRef names in dir: errNoRecord where there is no such ref, and an
// error where it names something that is no commit.
func (g *Guard) notesTip(ctx context.Context, dir string) (string, error) {
	status, id, err := g.revParse(ctx, dir, NotesRef)
	switch {
	case err != nil:
		return "", err
	case status == 1:
		return "", errNoRecord
	}
	status, tip, err := g.revParse(ctx, dir, id+"^{commit}")
	switch {
	case err != nil:
		return "", err
	case status == 1:
		return "", fmt.Errorf("%s names %s, which is no commit", NotesRef, id)
	}
	return tip, nil
}

// revParse is `git rev-parse --verify --quiet name` in dir: status 0 and the id, status 1 for a
// name that resolves to nothing, and an error for anything else.
func (g *Guard) revParse(ctx context.Context, dir, name string) (int, string, error) {
	var stderr bytes.Buffer
	status, out, err := g.run(ctx, child{argv: gitArgv("rev-parse", "--verify", "--quiet", name), dir: dir, stderr: &stderr})
	switch {
	case err != nil:
		return 0, "", err
	case status != 0 && status != 1:
		return 0, "", fmt.Errorf("git rev-parse exited %d: %s", status, gitSaid(stderr.String()))
	case status == 1:
		return 1, strings.TrimSpace(name), nil
	}
	return 0, strings.TrimSpace(out), nil
}

// note is one note of the record: the blob holding its text, and the path of the notes tree it is
// at, which a writer replacing it needs where that path has another fan-out than the writer's.
type note struct {
	note, path string
}

// listNotes is the notes tree of tip, listed once, by the blob each note is on: a path's
// components joined are that blob's id. An entry whose joined path is no object id is no note, as
// git notes reads it, and is left out.
func (g *Guard) listNotes(ctx context.Context, dir, tip string) (map[string]note, error) {
	out, err := g.git(ctx, dir, "ls-tree", "-r", "-z", "--full-tree", tip)
	if err != nil {
		return nil, err
	}
	notes := map[string]note{}
	for entry := range strings.SplitSeq(out, "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" {
			continue
		}
		blob := strings.ReplaceAll(path, "/", "")
		if len(blob) != len(tip) || !isHex(blob) {
			continue
		}
		notes[strings.ToLower(blob)] = note{note: fields[2], path: path}
	}
	return notes, nil
}

// noteTexts is the text of each distinct note blob of notes, each read once.
func (g *Guard) noteTexts(ctx context.Context, dir string, notes map[string]note) (map[string]string, error) {
	texts := map[string]string{}
	var ids []string
	for _, n := range notes {
		if _, ok := texts[n.note]; !ok {
			texts[n.note] = ""
			ids = append(ids, n.note)
		}
	}
	if len(ids) == 0 {
		return texts, nil
	}
	err := g.catBlobs(ctx, dir, ids, func(id string, body []byte) error {
		texts[id] = string(body)
		return nil
	})
	return texts, err
}

// holdsLine is whether text has line as one of its lines, exactly.
func holdsLine(text, line string) bool {
	for have := range strings.SplitSeq(text, "\n") {
		if have == line {
			return true
		}
	}
	return false
}

// isHex is whether s is all lower- or upper-case hex digits.
func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return s != ""
}

// catBlobs reads the blobs ids name with one `git cat-file --batch`, in order, handing each to
// each as it is read: one blob is held at a time. An id that is no blob is an error.
func (g *Guard) catBlobs(ctx context.Context, dir string, ids []string, each func(id string, body []byte) error) error {
	_, err := g.gitWith(ctx, dir, child{
		stdin: strings.NewReader(strings.Join(ids, "\n") + "\n"),
		stdout: func(out io.Reader) error {
			in := bufio.NewReaderSize(out, 64<<10)
			for range ids {
				header, err := in.ReadString('\n')
				if err != nil {
					return fmt.Errorf("git cat-file ended before every blob was read: %w", err)
				}
				fields := strings.Fields(header)
				if len(fields) != 3 || fields[1] != "blob" {
					return fmt.Errorf("git cat-file answered %q where a blob was asked for", strings.TrimSpace(header))
				}
				size, err := strconv.Atoi(fields[2])
				if err != nil || size < 0 {
					return fmt.Errorf("git cat-file answered %q where a blob was asked for", strings.TrimSpace(header))
				}
				body := make([]byte, size+1)
				if _, err := io.ReadFull(in, body); err != nil || body[size] != '\n' {
					return fmt.Errorf("git cat-file ended inside blob %s", fields[0])
				}
				if err := each(fields[0], body[:size]); err != nil {
					return err
				}
			}
			_, err := io.Copy(io.Discard, in)
			return err
		},
	}, "cat-file", "--batch")
	return err
}

// rawEntry is one entry of a commit's raw diff (`diff-tree --raw -z`, with --combined-all-paths
// for a merge): the header as git wrote it, its modes, ids and status, and its paths, the last of
// which is the path the commit leaves the file at. An ordinary diff's rename has two paths, the
// source first; a merge's entry has one per parent and then the result's.
type rawEntry struct {
	header string
	paths  []string
	// mode and blob are the result's: the file's mode and blob id in the commit itself, a blob of
	// all zeros where the commit leaves no file at the path.
	mode, blob string
}

// path is the path the commit leaves the file at.
func (e rawEntry) path() string {
	return e.paths[len(e.paths)-1]
}

// rawEntries is one commit's raw diff, in the shape its parents call for (diffTree), with pathspec
// where that is not nil.
func (g *Guard) rawEntries(ctx context.Context, dir, commit string, parents int, pathspec []string) ([]rawEntry, error) {
	args := []string{"--raw", "-z"}
	if parents > 1 {
		args = append(args, "--combined-all-paths")
	}
	args = diffTree(parents, append(args, commit)...)
	if pathspec != nil {
		args = append(append(args, "--"), pathspec...)
	}
	out, err := g.git(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(out, "\x00")
	var entries []rawEntry
	for i := 0; i < len(fields); {
		header := fields[i]
		i++
		if header == "" {
			continue
		}
		colons := len(header) - len(strings.TrimLeft(header, ":"))
		parts := strings.Fields(header[colons:])
		if colons == 0 || len(parts) != 2*(colons+1)+1 {
			return nil, fmt.Errorf("git diff-tree --raw answered an entry of another shape: %q", header)
		}
		paths := 1
		switch status := parts[len(parts)-1]; {
		case colons > 1:
			paths = colons + 1
		case strings.HasPrefix(status, "R"), strings.HasPrefix(status, "C"):
			paths = 2
		}
		if i+paths > len(fields) {
			return nil, fmt.Errorf("git diff-tree --raw answered an entry with too few paths: %q", header)
		}
		entries = append(entries, rawEntry{
			header: header,
			paths:  fields[i : i+paths],
			mode:   parts[colons],
			blob:   parts[2*colons+1],
		})
		i += paths
	}
	return entries, nil
}

// coverable is whether the record may stand for the lines of a file at path in mode: a regular
// file or a symlink, whose diff lines are lines of its blob (a gitlink's one line is not), at a
// path no rule refuses by itself.
//
// A path rule with no pattern refuses every line record of a file at its path, whatever the text,
// so today a push names such a file once for each line it adds. A file at such a path is read as
// if no record covered it, so its report is today's line for line, and the push is refused either
// way. The probe is a record of the path with an empty text: the detector answers a path rule's
// finding for it whatever the text, and a content pattern that matched an empty text would match
// every line, so it leaves no file coverable, which is stricter.
func (c *cover) coverable(path, mode string) bool {
	switch mode {
	case "100644", "100755", "120000":
	default:
		return false
	}
	return len(c.engine.detector.DetectContext(context.Background(), detectFragment(Record{Label: path}))) == 0
}

// coveredRead hands emit the added lines of one commit, as readPatch over the whole diff would,
// without reading the lines of a file whose new version the record covers.
//
// The commit's raw diff, in the same shape as its patch, names each file's new blob. Where it
// covers none of them the whole patch is read; where it covers all of them nothing is. Otherwise
// the patch is read for the uncovered files alone, each with the paths it came from, so that a
// rename is still read as one: in pathspec chunks of pathspecBytes, and only after a raw diff of
// each chunk is checked to be exactly the whole raw diff's entries for those files, in its order.
// A restricted diff can pair renames otherwise than the whole one (git's rename limit, its
// basename heuristic), and its lines would then not be the lines the whole patch reads. Where any
// chunk differs, the whole patch is read, leaving out the covered files' lines. Either way, the
// records are the whole patch's, in its order, less the lines of covered files.
func (g *Guard) coveredRead(ctx context.Context, dir, commit string, parents int, cover *cover, emit func(Record) error) error {
	entries, err := g.rawEntries(ctx, dir, commit, parents, nil)
	if err != nil {
		return err
	}
	skip := map[string]bool{}
	var uncovered []rawEntry
	for _, entry := range entries {
		if zeroSHA(entry.blob) {
			continue
		}
		if cover.blobs[entry.blob] && cover.coverable(entry.path(), entry.mode) {
			skip[entry.path()] = true
			cover.covered[entry.blob] = true
			continue
		}
		uncovered = append(uncovered, entry)
		cover.read[entry.blob] = true
	}
	switch {
	case len(skip) == 0:
		return g.readPatch(ctx, dir, commit, parents, nil, nil, emit)
	case len(uncovered) == 0:
		return nil
	}
	chunks := pathspecChunks(uncovered)
	for _, chunk := range chunks {
		got, err := g.rawEntries(ctx, dir, commit, parents, chunk.pathspec)
		if err != nil {
			return err
		}
		if !sameEntries(got, chunk.entries) {
			return g.readPatch(ctx, dir, commit, parents, nil, skip, emit)
		}
	}
	for _, chunk := range chunks {
		if err := g.readPatch(ctx, dir, commit, parents, chunk.pathspec, skip, emit); err != nil {
			return err
		}
	}
	return nil
}

// pathspecChunk is some uncovered entries of a commit, in the whole diff's order, and the literal
// pathspec of every path they name.
type pathspecChunk struct {
	entries  []rawEntry
	pathspec []string
}

// pathspecChunks parts entries into chunks of at most pathspecBytes of pathspec, an entry alone
// where its paths are more; an entry's paths are never parted.
func pathspecChunks(entries []rawEntry) []pathspecChunk {
	var chunks []pathspecChunk
	var current pathspecChunk
	size := 0
	for _, entry := range entries {
		var specs []string
		for _, path := range entry.paths {
			spec := ":(literal)" + path
			if !slices.Contains(specs, spec) {
				specs = append(specs, spec)
			}
		}
		cost := 0
		for _, spec := range specs {
			cost += len(spec) + 1
		}
		if len(current.entries) > 0 && size+cost > pathspecBytes {
			chunks = append(chunks, current)
			current, size = pathspecChunk{}, 0
		}
		current.entries = append(current.entries, entry)
		current.pathspec = append(current.pathspec, specs...)
		size += cost
	}
	if len(current.entries) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

// sameEntries is whether got is want, entry for entry and in order.
func sameEntries(got, want []rawEntry) bool {
	return slices.EqualFunc(got, want, func(a, b rawEntry) bool {
		return a.header == b.header && slices.Equal(a.paths, b.paths)
	})
}

// RecordSummary is what one run of the record writer did with the file versions of the commits it
// was given: the ones a note already covered under its key, the ones it read and recorded, and
// the ones it read where a rule's content pattern matched a line, which it did not record.
type RecordSummary struct {
	Covered, Recorded, NotRecorded int
}

// ErrRecordMoved is a write of the record that found NotesRef moved since it read it: another
// writer wrote the record meanwhile. Nothing was recorded, and the next run writes again.
var ErrRecordMoved = errors.New(NotesRef + " moved while this run read the blobs, so nothing was recorded: run again")

// WriteRecord records, in the repository in dir, every blob of the trees of revs that no content
// pattern of the rules matches any line of, under this binary's key (recordKey). Gitlinks are no
// blobs and are skipped, and so is every blob whose note already holds the key's line. The rest
// are read in batches of at most recordBatchBlobs blobs and recordBatchBytes of them, one larger
// blob alone, and checked in parallel (rules.anyLineMatches), each line as a push's diff would form
// it, with every content pattern and none of the allowlists, keywords or entropy that only take a
// finding away: a blob it records holds nothing a push's rule pass could find in its lines.
//
// The new notes are written as one commit of NotesRef by git fast-import, on the commit the ref
// named when this run read it. fast-import refuses where another writer moved the ref meanwhile,
// and the answer is then ErrRecordMoved. A note's other lines are kept, at most recordLines in
// all, the new line last.
func (g *Guard) WriteRecord(ctx context.Context, dir string, revs []string) (RecordSummary, error) {
	var summary RecordSummary
	engine, err := compiled()
	if err != nil {
		return summary, err
	}
	key, err := g.recordKey(engine)
	if err != nil {
		return summary, err
	}
	tip, err := g.notesTip(ctx, dir)
	switch {
	case errors.Is(err, errNoRecord):
		tip = ""
	case err != nil:
		return summary, err
	}
	notes, texts := map[string]note{}, map[string]string{}
	if tip != "" {
		if notes, err = g.listNotes(ctx, dir, tip); err != nil {
			return summary, err
		}
		if texts, err = g.noteTexts(ctx, dir, notes); err != nil {
			return summary, err
		}
	}
	blobs, err := g.treeBlobs(ctx, dir, revs)
	if err != nil {
		return summary, err
	}
	line := "pass " + key
	var pending []treeBlob
	for _, blob := range blobs {
		if n, ok := notes[blob.id]; ok && holdsLine(texts[n.note], line) {
			summary.Covered++
			continue
		}
		pending = append(pending, blob)
	}
	var recorded []string
	for len(pending) > 0 {
		batch := recordBatch(pending)
		pending = pending[len(batch):]
		clean, err := g.cleanBlobs(ctx, dir, engine, batch)
		if err != nil {
			return summary, err
		}
		for i, blob := range batch {
			if clean[i] {
				recorded = append(recorded, blob.id)
			} else {
				summary.NotRecorded++
			}
		}
	}
	if g.beforeImport != nil {
		g.beforeImport()
	}
	if len(recorded) > 0 {
		if err := g.importNotes(ctx, dir, tip, notes, texts, recorded, line); err != nil {
			return summary, err
		}
	}
	summary.Recorded = len(recorded)
	return summary, nil
}

// treeBlob is one blob of a tree the writer reads, and its size.
type treeBlob struct {
	id   string
	size int64
}

// treeBlobs is every distinct blob of the trees of revs, in the order they are first met: each
// tree listed with `ls-tree -r -l`, its gitlinks left out.
func (g *Guard) treeBlobs(ctx context.Context, dir string, revs []string) ([]treeBlob, error) {
	seen := map[string]bool{}
	var blobs []treeBlob
	for _, rev := range revs {
		status, commit, err := g.revParse(ctx, dir, rev+"^{commit}")
		switch {
		case err != nil:
			return nil, err
		case status == 1:
			return nil, fmt.Errorf("%q names no commit", rev)
		}
		out, err := g.git(ctx, dir, "ls-tree", "-r", "-z", "-l", "--full-tree", commit)
		if err != nil {
			return nil, err
		}
		for entry := range strings.SplitSeq(out, "\x00") {
			meta, _, ok := strings.Cut(entry, "\t")
			fields := strings.Fields(meta)
			if !ok || len(fields) != 4 || fields[1] != "blob" || seen[fields[2]] {
				continue
			}
			size, err := strconv.ParseInt(fields[3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("git ls-tree answered a blob of no size: %q", meta)
			}
			seen[fields[2]] = true
			blobs = append(blobs, treeBlob{id: fields[2], size: size})
		}
	}
	return blobs, nil
}

// recordBatch is the first batch of pending the writer reads at once: at most recordBatchBlobs
// blobs and recordBatchBytes of them, and the first blob alone where it is larger than that.
func recordBatch(pending []treeBlob) []treeBlob {
	var size int64
	for i, blob := range pending {
		if i == recordBatchBlobs || (i > 0 && size+blob.size > recordBatchBytes) {
			return pending[:i]
		}
		size += blob.size
	}
	return pending
}

// cleanBlobs reads batch with one cat-file and answers, blob for blob, whether no content pattern
// of the rules matches any of its lines. The batch is held whole while its blobs are checked, one
// goroutine per processor.
func (g *Guard) cleanBlobs(ctx context.Context, dir string, engine *rules, batch []treeBlob) ([]bool, error) {
	ids := make([]string, len(batch))
	for i, blob := range batch {
		ids[i] = blob.id
	}
	bodies := make([][]byte, 0, len(batch))
	err := g.catBlobs(ctx, dir, ids, func(_ string, body []byte) error {
		bodies = append(bodies, body)
		return nil
	})
	if err != nil {
		return nil, err
	}
	clean := make([]bool, len(batch))
	var checks errgroup.Group
	checks.SetLimit(runtime.GOMAXPROCS(0))
	for i, body := range bodies {
		checks.Go(func() error {
			clean[i] = !engine.anyLineMatches(body)
			return nil
		})
	}
	_ = checks.Wait()
	return clean, ctx.Err()
}

// importNotes writes one commit of NotesRef on tip (none where tip is ""), adding line to the note
// of each blob of recorded, with git fast-import. Each note's other lines are kept, the oldest
// dropped past recordLines; a note at a path of another fan-out than the writer's is moved to its
// own. A refused update of the ref, where it no longer names tip, is ErrRecordMoved.
func (g *Guard) importNotes(ctx context.Context, dir, tip string, notes map[string]note, texts map[string]string, recorded []string, line string) error {
	var stream strings.Builder
	stream.WriteString("feature done\n")
	marks := map[string]int{}
	var files strings.Builder
	for _, blob := range recorded {
		var kept []string
		if n, ok := notes[blob]; ok {
			for have := range strings.SplitSeq(texts[n.note], "\n") {
				if have != "" && have != line {
					kept = append(kept, have)
				}
			}
		}
		kept = append(kept[max(0, len(kept)-(recordLines-1)):], line)
		text := strings.Join(kept, "\n") + "\n"
		mark, ok := marks[text]
		if !ok {
			mark = len(marks) + 1
			marks[text] = mark
			fmt.Fprintf(&stream, "blob\nmark :%d\ndata %d\n%s\n", mark, len(text), text)
		}
		path := blob[:2] + "/" + blob[2:]
		if n, ok := notes[blob]; ok && n.path != path {
			fmt.Fprintf(&files, "D %s\n", n.path)
		}
		fmt.Fprintf(&files, "M 100644 :%d %s\n", mark, path)
	}
	message := fmt.Sprintf("legion leak-check record: %d file versions\n", len(recorded))
	fmt.Fprintf(&stream, "commit %s\ncommitter legion leak-check <legion-leak-check@localhost> %d +0000\ndata %d\n%s",
		NotesRef, time.Now().Unix(), len(message), message)
	if tip != "" {
		fmt.Fprintf(&stream, "from %s\n", tip)
	}
	stream.WriteString(files.String())
	stream.WriteString("\ndone\n")
	_, err := g.gitWith(ctx, dir, child{stdin: strings.NewReader(stream.String())}, "fast-import", "--quiet")
	if err == nil {
		return nil
	}
	if now, readErr := g.notesTip(ctx, dir); readErr == nil && now != tip {
		return fmt.Errorf("%w (%v)", ErrRecordMoved, err)
	}
	return err
}

package leakguard

import (
	"context"
	"fmt"
	"iter"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// commitRecords is one commit as records. The identities, the commit's other headers and the
// message come from the commit itself, and the added and renamed paths and the added lines from
// its files: those at which it differs from its parent where it has at most one (commitChanges),
// and those at which it differs from every parent where it is a merge (mergeChanges), each shape's
// files read from their blobs alike (Guard.addedLines). The records come in one order whatever the
// shape, so that a report lists its lines alike from one run to the next: the identities, the
// other headers in the commit's own order, the message's records (appendMessage), the paths in
// path order, then the lines in path order and, within a file, the file's own order.
//
// The identities are the author's and the committer's name and email as git show formats them.
// The message is read as the commit object holds it, which is what the destination keeps and
// serves, and as git show formats it where that differs, since git decodes it by the commit's
// encoding header and cuts it at its first zero byte (appendMessage). A commit publishes every
// header it holds, and git show formats no other: a merge of a signed tag holds that whole tag
// object in its mergetag header, the tag's name, its tagger and its message, and a signed commit
// holds its signature. So each header is read too, from the commit object as the push's one
// cat-file --batch answers it (batch, Guard.pushRecords), a header of more than one line with its
// every line (objectHeaders), as a record of the whole header, its name with its value
// (headerRecord), unless the commit's other records hold it already: a tree or a parent line that
// holds an object id, which names nothing, and an author or a committer line that holds the
// identity read as that record and its date alone (identityLine), which an identity git show
// decodes by the encoding header does not. git show formats the last author line and the last
// committer line of a commit that holds two, so an earlier one that holds anything else is read as
// a header of its own. A header that holds a zero byte is read decoded too, as a message is
// (appendDecoded).
//
// A record about a file carries that file's path as its label and the line or the path as its
// text, so that the engine reads the path as a path: the rule file's one path-scoped allowlist
// is anchored on a whole path, and a label carrying anything else would take it away.
func (g *Guard) commitRecords(ctx context.Context, dir, commit string, batch *blobBatch) ([]Record, error) {
	// git writes the separator for itself, as %x00 in the format: an argument of this process's
	// own holding a zero byte is a child that cannot be started at all. %P comes before the
	// body, which is the one field that can hold a newline, so the parents are read with the
	// identities and the message. They are %P's rather than the commit object's parent lines:
	// %P is git's own view of the parents, and a shallow clone's boundary commit has none in it
	// (in a --depth 1 clone %P prints nothing where cat-file prints two parent lines), so its
	// files are read as a root commit's, the clone holding no parent to compare them with.
	const separator = "%x00"
	out, err := g.git(ctx, dir, "show", "--no-patch", "--no-color",
		"--format="+separator+"%an <%ae>"+separator+"%cn <%ce>"+separator+"%P"+separator+"%B"+separator, commit)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\x00")
	if len(parts) < 5 {
		return nil, fmt.Errorf("git show %s answered no identity, parents and message", short(commit))
	}
	object, err := batch.object(commit, "commit")
	if err != nil {
		return nil, err
	}
	parents := strings.Fields(parts[3])
	label := "commit " + short(commit)
	records := []Record{
		{Label: label + " author", Text: parts[1]},
		{Label: label + " committer", Text: parts[2]},
	}
	headers, message, _ := strings.Cut(object, "\n\n")
	for name, value := range objectHeaders(headers) {
		if (name == "tree" || name == "parent") && isObjectID(value) ||
			name == "author" && identityLine(value, parts[1]) ||
			name == "committer" && identityLine(value, parts[2]) {
			continue
		}
		records = appendDecoded(records, headerRecord(label, name, value, commitHeaders))
	}
	records = appendMessage(records, label, message, parts[4])
	var paths []string
	var changes []fileChange
	kind := "commit"
	if len(parents) > 1 {
		kind = "merge"
		paths, changes, err = g.mergeChanges(ctx, dir, commit, parents)
	} else {
		paths, changes, err = g.commitChanges(ctx, dir, kind, commit, len(parents))
	}
	if err != nil {
		return nil, err
	}
	return g.fileRecords(ctx, dir, kind, commit, records, paths, changes)
}

// treeRecords are the records of a tree a push publishes, which a ref can be at and a tag chain
// can end at: the tree is read as a root commit's files are, each path it holds a file or a
// submodule at, and each line of each text file, blank ones aside (commitChanges,
// Guard.addedLines), with no identity and no message, which a tree has none of.
func (g *Guard) treeRecords(ctx context.Context, dir, tree string) ([]Record, error) {
	paths, changes, err := g.commitChanges(ctx, dir, "tree", tree, 0)
	if err != nil {
		return nil, err
	}
	return g.fileRecords(ctx, dir, "tree", tree, nil, paths, changes)
}

// blobRecords are the records of a blob a push publishes, which a ref can be at and a tag chain can
// end at with no tree to give it a path: each line of it, blank ones aside, as of a file a commit
// adds, read through the batch a commit's files are read through and within the same bound
// (Guard.addedLines). A binary blob adds no line, and a text blob above blobBound refuses the push
// as a setup failure naming it and its size. Each record is labelled "blob <short>", as a commit's
// identities and message are labelled by the commit. The lines are not copied, as a commit's are:
// they are the whole blob but its blank lines and line endings, so its records hold it anyway.
func (g *Guard) blobRecords(ctx context.Context, dir, blob string) ([]Record, error) {
	batch, err := g.readBlobs(ctx, dir)
	if err != nil {
		return nil, err
	}
	file, over, err := batch.text(blob, g.blobBound)
	if err == nil && over > 0 {
		err = fmt.Errorf("blob %s is %d bytes, more than the %d bytes a blob is read up to", short(blob), over, g.blobBound)
	}
	if err = batch.close(err); err != nil {
		return nil, err
	}
	label := "blob " + short(blob)
	var records []Record
	for line := range textLines(file) {
		records = append(records, Record{Label: label, Text: line})
	}
	return records, nil
}

// fileRecords are records with the records of the files of a commit, a merge or a tree (kind), id,
// appended: one for each of paths, its added and renamed paths, in path order, then one for each
// line added at changes (Guard.addedLines), in path order and, within a file, in the file's own
// order.
func (g *Guard) fileRecords(ctx context.Context, dir, kind, id string, records []Record, paths []string, changes []fileChange) ([]Record, error) {
	added, err := g.addedLines(ctx, dir, kind, id, changes)
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	for _, path := range paths {
		records = append(records, Record{Label: path, Text: path})
	}
	// Stable, so that the lines of one file keep the order they were read in, which is the file's.
	slices.SortStableFunc(added, func(a, b Record) int { return strings.Compare(a.Label, b.Label) })
	return append(records, added...), nil
}

// fileChange is a file of a commit's that lines can be added at: the commit's file at path, a
// blob, and held, the files it is compared with, each a blob a parent has at from, which is path
// but for a file a commit of one parent renames. A line of the commit's file that none of held has
// is one the commit adds (Guard.addedLines).
type fileChange struct {
	path, from string
	blob       string
	held       []string
}

// commitChanges are the paths a commit of at most one parent, or a tree (kind), id, adds or
// renames a file to, which are the paths the destination would hold that it does not hold
// already, and the files it changes that lines can be added at (fileChange), as one diff-tree
// lists them against its parent:
//
//	git diff-tree -r -M --no-commit-id -z [<empty tree>] <commit or tree>
//
// A root commit and a tree have no parent, and are listed against the tree that holds nothing
// (emptyTree), so that each file either holds is one it adds: a diff-tree of either alone lists
// nothing. Renames are found (-M), so that a file the commit renames is compared with the file it
// is renamed from. Of each file the commit has as a blob, a regular file or a symbolic link:
//
//   - a modification or a rename that leaves the parent's blob as it is, which changes the file's
//     mode or its path alone, adds no line, and nothing of it is read;
//   - any other modification or rename is compared with the parent's file it changes, where that
//     is a blob;
//   - and an added file, the new side of a change of type, and any other change is compared with
//     no file, so that its every line is added.
//
// A file the commit removes, and a submodule, which holds no line of this repository's, add none.
// What is read grows with the commit's changes alone, in the list diff-tree writes and the blobs
// cat-file answers, and no git's command line grows with it.
func (g *Guard) commitChanges(ctx context.Context, dir, kind, id string, parents int) (paths []string, changes []fileChange, err error) {
	args := []string{"diff-tree", "-r", "-M", "--no-commit-id", "-z"}
	if parents == 0 {
		args = append(args, emptyTree(id))
	}
	out, err := g.git(ctx, dir, append(args, id)...)
	if err != nil {
		return nil, nil, err
	}
	what := kind + " " + short(id)
	for entry, err := range rawEntries(out, what) {
		switch {
		case err != nil:
			return nil, nil, err
		case entry.commit != "":
			// --no-commit-id writes no commit's id.
			return nil, nil, shapeError(what)
		}
		if entry.status == 'A' || entry.status == 'R' {
			paths = append(paths, entry.path)
		}
		if !entry.ourKind.blob() {
			continue
		}
		change := fileChange{path: entry.path, from: entry.from, blob: entry.ours}
		if entry.status == 'M' || entry.status == 'R' {
			if entry.theirs == entry.ours {
				continue
			}
			if entry.theirKind.blob() {
				change.held = []string{entry.theirs}
			}
		}
		changes = append(changes, change)
	}
	return paths, changes, nil
}

// emptyTree is the tree that holds nothing, in a repository whose object ids are as long as id: a
// SHA-256 one where id is 64 hex digits, and a SHA-1 one else. git knows that tree in every
// repository, whether the repository holds it or not.
func emptyTree(id string) string {
	if len(id) == 64 {
		return "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321"
	}
	return "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
}

// addedLines are the lines a commit, a merge or a tree (kind), id, adds at changes, each as a
// record of the file it is added to: the lines of each change's file, blank ones aside, that none
// of the files it is compared with holds, in the order the commit's file has them. The text
// compared is a line with its line ending off (lines). A line one of those files holds is text the
// destination holds already, or that is read where the commit holding it is pushed, wherever the
// commit puts it, since the rules read a line's text alone. A tree's every change is compared with
// no file (commitChanges), so every line of its text files is read.
//
// The files are read as blobs, with cat-file --batch, which applies no attribute, so a file
// .gitattributes marks -diff or binary is read as any other is, the pushed commit's own
// .gitattributes included. A file is binary as git's own diff tells one where no attribute says
// otherwise: a zero byte in its first 8000 bytes (xdiff-interface.c's buffer_is_binary). A binary
// file of the commit's adds no line, and a binary file it is compared with holds none.
//
// The read is bounded, since an issue pod's roles share one memory limit. A text file it takes,
// the commit's own or one it is compared with, is read only where it is at most blobBound bytes,
// and one above that refuses the push as a setup failure naming the file and its size. That is the
// one answer that publishes no line unread: a file passed over would pass every line of it, and a
// file read in part would pass the rest. Of a binary file, the read holds its first 8000 bytes
// alone, whatever its size: cat-file writes the rest, which is passed over unkept. What the read
// holds is one change's: the commit's file, its lines no file read so far holds, and the file being
// read. A file it is compared with is never asked of cat-file (blobBatch) where no line of the
// commit's file is left to take, which is every one of them where the commit's file is binary; so
// such a file the repository lacks, or that a partial clone's fetch of fails, refuses nothing. A
// commit with no change to read starts no git.
func (g *Guard) addedLines(ctx context.Context, dir, kind, id string, changes []fileChange) ([]Record, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	// ours names the file of a change, and theirs a file it is compared with, a parent's, which no
	// change of a tree's has.
	ours := kind + " " + short(id) + "'s"
	theirs := "the parent of " + ours
	if kind == "merge" {
		theirs = "a parent of " + ours
	}
	batch, err := g.readBlobs(ctx, dir)
	if err != nil {
		return nil, err
	}
	// read is the text of blob, the file at path whose owner whose names, and the failure that
	// names it where it is a text file above the bound.
	read := func(blob, whose, path string) (string, error) {
		file, over, err := batch.text(blob, g.blobBound)
		if err == nil && over > 0 {
			err = fmt.Errorf("%s file %s is %d bytes, more than the %d bytes a %s's file is read up to", whose, maskPath(path), over, g.blobBound, kind)
		}
		return file, err
	}
	var records []Record
	add := func(change fileChange) error {
		file, err := read(change.blob, ours, change.path)
		if err != nil {
			return err
		}
		// A record's text is a copy, so that the file it was read from is not held for it.
		if len(change.held) == 0 {
			// Compared with no file, the commit's file adds its every line.
			for line := range textLines(file) {
				records = append(records, Record{Label: change.path, Text: strings.Clone(line)})
			}
			return nil
		}
		// own are the lines of the commit's file that no file read so far holds.
		own := map[string]bool{}
		for line := range textLines(file) {
			own[line] = true
		}
		for _, blob := range change.held {
			if len(own) == 0 {
				break
			}
			held, err := read(blob, theirs, change.from)
			if err != nil {
				return err
			}
			for line := range lines(held) {
				delete(own, line)
				if len(own) == 0 {
					break
				}
			}
		}
		for line := range lines(file) {
			if own[line] {
				records = append(records, Record{Label: change.path, Text: strings.Clone(line)})
			}
		}
		return nil
	}
	for _, change := range changes {
		if err = add(change); err != nil {
			break
		}
	}
	if err = batch.close(err); err != nil {
		return nil, err
	}
	return records, nil
}

// lines are the lines of file, each with its line ending off: a trailing "\n", then one trailing
// "\r".
func lines(file string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for line := range strings.Lines(file) {
			if !yield(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")) {
				return
			}
		}
	}
}

// textLines are the lines of file (lines) that hold more than white space, the lines a record is
// made of: a blank line names nothing.
func textLines(file string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for line := range lines(file) {
			if strings.TrimSpace(line) != "" && !yield(line) {
				return
			}
		}
	}
}

// objectHeaders are the headers of a commit or a tag object, as git cat-file writes them ahead of
// the blank line its message follows: each header's name and value, in the order of headers. A
// header is a line "<name> <value>", or a name alone, whose value is empty, and each line after it
// that starts with a space carries its value on, which is how git writes a value of more than one
// line: a merge's mergetag, which holds the whole tag object the merge is of, or a signature. Such
// a value is its lines joined by newlines, each with that space off.
func objectHeaders(headers string) iter.Seq2[string, string] {
	return func(yield func(name, value string) bool) {
		for rest := headers; rest != ""; {
			header := rest
			_, rest, _ = strings.Cut(rest, "\n")
			for strings.HasPrefix(rest, " ") {
				_, rest, _ = strings.Cut(rest, "\n")
			}
			name, value, _ := strings.Cut(strings.TrimSuffix(header[:len(header)-len(rest)], "\n"), " ")
			if !yield(name, strings.ReplaceAll(value, "\n ", "\n")) {
				return
			}
		}
	}
}

// commitHeaders and tagHeaders are the names of the headers git itself writes in a commit object
// and in a tag object, as git 2.47.3's source writes them: a commit's tree, parent, author,
// committer and encoding (commit.c's standard_header_field), its mergetag (commit.c's
// handle_signed_tag) and its signature (commit.c's gpg_sig_headers), and a tag's object, type, tag
// and tagger (builtin/tag.c's create_tag) and the signature of its other hash, which git writes
// as a commit's (builtin/tag.c's do_sign, through commit.c's add_header_signature).
var (
	commitHeaders = []string{"tree", "parent", "author", "committer", "encoding", "mergetag", "gpgsig", "gpgsig-sha256"}
	tagHeaders    = []string{"object", "type", "tag", "tagger", "gpgsig", "gpgsig-sha256"}
)

// headerRecord is the record of a header, name and value (objectHeaders), of the commit or the tag
// label names ("commit 1a2b3c4"). Its text is the whole header, its name with its value, since the
// object publishes both and a header can be a name alone; it is a copy, so that a record of one
// header does not hold the whole object. Its label names the header where the name is one of own,
// the names git itself writes, and is "<label> header" for any other: such a name is the push's
// own text, and a label carries it into the report with no rule but the path rules reading it.
func headerRecord(label, name, value string, own []string) Record {
	text := name + " " + value
	if !slices.Contains(own, name) {
		name = "header"
	}
	return Record{Label: label + " " + name, Text: text}
}

// identityDate is what follows the identity in a commit's author or committer line: the date, as
// seconds since the epoch and a zone (" 1700000000 +0000").
var identityDate = regexp.MustCompile(`^ [0-9]+ [+-][0-9]{4}$`)

// identityLine is whether value, the value of an author or a committer line of a commit's, is
// identity, the name and email git show formats of that line (%an <%ae>, %cn <%ce>), followed by
// its date alone (identityDate): a line the commit's record of that identity holds already, the
// date aside, which names nothing.
func identityLine(value, identity string) bool {
	date, ok := strings.CutPrefix(value, identity)
	return ok && identityDate.MatchString(date)
}

// appendMessage is records with the records of message appended, the message of the commit or the
// tag label names ("commit 1a2b3c4") as its object holds it, after its first blank line: one of
// message itself, "<label> message", with its decodes where it holds a zero byte (appendDecoded),
// "<label> message decoded as UTF-16, big-endian" and so on, whatever encoding header the object
// holds or lacks; then one of shown, the message as git show formats it, where that differs,
// "<label> message as git shows it", which a tag's never does (Guard.tagRecords).
//
// git show cuts a message at its first zero byte, so shown is not decoded, and decodes every
// message that holds none through iconv, by the encoding header, which the record of shown reads.
// So the decodes, keyed on the zero byte and not on the name of the encoding, cover every name
// iconv takes for those encodings, however it is spelled: glibc's iconv_open drops all but
// letters, digits and "_-.,:/" from a name, and honours a byte-order mark only under the names
// that give no order (measured with glibc 2.41 and git 2.47.3 in an issue pod).
func appendMessage(records []Record, label, message, shown string) []Record {
	records = appendDecoded(records, Record{Label: label + " message", Text: message})
	if shown != message {
		records = append(records, Record{Label: label + " message as git shows it", Text: shown})
	}
	return records
}

// appendDecoded is records with record appended and, where its text holds a zero byte, one record
// of that text decoded in each of four ways (wideText): "<label> decoded as UTF-16, big-endian",
// then "UTF-16, little-endian", "UTF-32, big-endian" and "UTF-32, little-endian", where <label> is
// record's; and one of the text with every zero byte taken out, "<label> with its zero bytes taken
// out". A message and every header of a commit or a tag are read so (appendMessage, headerRecord's
// callers): git commit refuses a zero byte in a message, and git's own check one in a header, so
// only a crafted object holds one, and a destination that does not check what it receives keeps
// it. An ASCII name in UTF-16 or UTF-32 holds a zero byte beside each letter. The text with its
// zero bytes taken out holds such a name at any offset and in either order, which the four
// decodes, aligned on the text's first byte, do not: a reader that scans the bytes without regard
// to alignment, such as strings -el, finds such text at an offset they are not aligned on, such as
// UTF-32 text two bytes in, where a header's name can put its value, and every name a rule reads
// is ASCII.
func appendDecoded(records []Record, record Record) []Record {
	records = append(records, record)
	if strings.IndexByte(record.Text, 0) < 0 {
		return records
	}
	for _, wide := range []struct {
		name   string
		size   int
		little bool
	}{
		{"UTF-16, big-endian", 2, false},
		{"UTF-16, little-endian", 2, true},
		{"UTF-32, big-endian", 4, false},
		{"UTF-32, little-endian", 4, true},
	} {
		records = append(records, Record{Label: record.Label + " decoded as " + wide.name, Text: wideText(record.Text, wide.size, wide.little)})
	}
	return append(records, Record{Label: record.Label + " with its zero bytes taken out", Text: strings.ReplaceAll(record.Text, "\x00", "")})
}

// wideText is text decoded from UTF-16 (size 2) or UTF-32 (size 4) from its first byte,
// little-endian where little and big-endian else, with one U+FEFF it opens with, a byte-order mark
// of that order, left out. A unit that is no character, an unpaired surrogate, or in UTF-32 a
// surrogate or a value above U+10FFFF, and the bytes after the last whole unit, are each read as
// U+FFFD, and the decode goes on: what follows them is read, and nothing fails the push.
func wideText(text string, size int, little bool) string {
	// unit is the code unit at the head of s, which holds one.
	unit := func(s string) rune {
		var r rune
		for i := range size {
			b := s[i]
			if little {
				b = s[size-1-i]
			}
			r = r<<8 | rune(b)
		}
		return r
	}
	rest := text
	if len(rest) >= size && unit(rest) == '\ufeff' {
		rest = rest[size:]
	}
	var decoded strings.Builder
	decoded.Grow(len(rest))
	for ; len(rest) >= size; rest = rest[size:] {
		r := unit(rest)
		if size == 2 && utf16.IsSurrogate(r) && len(rest) >= 4 {
			if pair := utf16.DecodeRune(r, unit(rest[2:])); pair != utf8.RuneError {
				r, rest = pair, rest[2:]
			}
		}
		decoded.WriteRune(r)
	}
	if rest != "" {
		decoded.WriteRune(utf8.RuneError)
	}
	return decoded.String()
}

// rawEntry is one entry of diff-tree's raw format, which -z writes as
// ":<parent's mode> <commit's mode> <parent's object> <commit's object> <status>[<score>]" and
// then the path, the path the file is from first for a rename or a copy; or the id of the commit
// a diff is of, which diff-tree --stdin writes ahead of each commit's entries.
type rawEntry struct {
	// commit is the id that heads a diff, and "" for an entry of a change.
	commit string
	// theirKind and theirs are the kind of file the parent has and its object, and ourKind and
	// ours the commit's: a blob where the kind is one (fileKind.blob).
	theirKind, ourKind fileKind
	theirs, ours       string
	// status is diff-tree's letter for the change: A, C, D, M, R, T, U or X.
	status byte
	// path is where the commit has the file, and from is where the parent has the file it
	// changes: the same path but for a rename or a copy.
	from, path string
}

// rawEntries are the entries of out, which a diff-tree of what ("commit 1a2b3c4") wrote in its raw
// format with -z, in the order it wrote them. An entry of a shape diff-tree does not write is an
// error, and the last: a list misread would be of other files than the commit's.
func rawEntries(out, what string) iter.Seq2[rawEntry, error] {
	return func(yield func(rawEntry, error) bool) {
		rest := out
		// field is the next field of out, and false where none is left.
		field := func() (string, bool) {
			if rest == "" {
				return "", false
			}
			var next string
			next, rest, _ = strings.Cut(rest, "\x00")
			return next, true
		}
		for {
			head, ok := field()
			switch {
			case !ok:
				return
			case head == "":
				continue
			case !strings.HasPrefix(head, ":"):
				if !yield(rawEntry{commit: head}, nil) {
					return
				}
				continue
			}
			meta := strings.Fields(head[1:])
			if len(meta) != 5 {
				yield(rawEntry{}, shapeError(what))
				return
			}
			theirKind, theirMode := kindOf(meta[0])
			ourKind, ourMode := kindOf(meta[1])
			entry := rawEntry{theirKind: theirKind, ourKind: ourKind, theirs: meta[2], ours: meta[3], status: meta[4][0]}
			entry.from, ok = field()
			entry.path = entry.from
			if ok && (entry.status == 'R' || entry.status == 'C') {
				entry.path, ok = field()
			}
			if !theirMode || !ourMode || !ok || entry.from == "" || entry.path == "" {
				yield(rawEntry{}, shapeError(what))
				return
			}
			if !yield(entry, nil) {
				return
			}
		}
	}
}

// shapeError is the failure of a diff-tree of what ("commit 1a2b3c4") that wrote an entry of a
// shape diff-tree does not write.
func shapeError(what string) error {
	return fmt.Errorf("git diff-tree of %s wrote an entry of a shape it does not write", what)
}

// fileKind is the kind of file a mode of diff-tree's raw format names, its bits under 0o170000: a
// regular file, and a symbolic link, whose target is its content, are each a blob git holds; a
// gitlink (0o160000) is a submodule's commit, of which this repository holds no lines; and 0 is no
// file, the 000000 of the side of a change that has none.
type fileKind uint32

const (
	regularFile  fileKind = 0o100000
	symbolicLink fileKind = 0o120000
)

// kindOf is the kind of file mode names, and false where mode is no octal number.
func kindOf(mode string) (fileKind, bool) {
	bits, err := strconv.ParseUint(mode, 8, 32)
	return fileKind(bits) & 0o170000, err == nil
}

// blob is whether the kind is a file git holds as a blob, which lines are read from.
func (k fileKind) blob() bool {
	return k == regularFile || k == symbolicLink
}

package survival

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

// A file's change, as either patch source states it: GitHub's diff media type (three lines of
// context), git's own `log -p -U0 -M`, or a files[] page of GitHub's commit route. ParseDiff and
// FilesPatch port scripts-feasibility/patchlib.py (parse_git_diff, zero_hunks, api_file_diff) and
// answer what it answers, file for file and hunk for hunk, but in four places, each a path or a
// malformed hunk the reference misreads. Of the fixtures in testdata/diffs, only the quoted path
// reads otherwise than the reference reads it, by the first:
//
//   - A path of a --- or +++ line is unquoted before its a/ or b/ prefix is stripped, and the TAB
//     git writes after such a path when it holds a space is dropped. patchlib.py strips the prefix
//     first, so it keeps it on a quoted path, and its unquote drops a quoted path's last character
//     whatever it is, so it keeps the closing quote where a TAB follows it; and it keeps the TAB
//     on an unquoted path with a space. Its replay then keys one file by two paths.
//   - The paths of a `diff --git` line are read where they are quoted, or where " b/" occurs in
//     them, which patchlib.py leaves unread until a later header line names them.
//   - A submodule entry (mode 160000) is marked, so the replay skips it as it skips a binary file;
//     patchlib.py reads its `Subproject commit` line as a line of a file.
//   - A hunk's lines are counted against its header's counts, and a hunk that holds more or fewer
//     lines than they say is an error, where patchlib.py reads on.

// The statuses of a FileDiff: the words patchlib.py gives a header's kind, which are also the
// statuses GitHub's files[] entries carry.
const (
	StatusAdded    = "added"
	StatusRemoved  = "removed"
	StatusModified = "modified"
	StatusRenamed  = "renamed"
	StatusCopied   = "copied"
)

// Hunk is one run of removed and added lines with no context line inside it, in the
// zero-context form patchlib.py's zero_hunks reads both sources into: OldStart is the 1-based old
// line the run starts at, or, where it removes nothing, the old line it inserts before; NewStart
// is the same in the new file; Removed and Added are the lines it removes and adds. A hunk of
// GitHub's three-line context that context lines split is several of these, so a patch git wrote
// with -U0 and the same patch GitHub wrote reduce to the same hunks.
type Hunk struct {
	OldStart int
	Removed  int
	NewStart int
	Added    int
}

// FileDiff is one file's change: its path before (OldPath, "" for a file the change adds) and
// after (NewPath, "" for a file it deletes), its Status, whether git wrote it as binary, whether
// it is a submodule entry (mode 160000), and its hunks. A mode change alone, a rename with no edit
// and an empty file added have no hunk. A symbolic link is a file whose one line is its target.
type FileDiff struct {
	OldPath   string
	NewPath   string
	Status    string
	Binary    bool
	Submodule bool
	Hunks     []Hunk
}

var (
	diffGitPrefix = []byte("diff --git ")
	hunkPrefix    = []byte("@@")
)

// ParseDiff reads every file of one commit's patch: the text GitHub's diff media type serves for
// the commit or compare route, or one commit's part of `git log -p -U0 -M`. Header lines are read
// only before a file's first hunk, and a hunk's lines by its counts, so a removed line whose own
// text starts "-- " is never read as a header. It refuses a hunk header it cannot read and a hunk
// whose lines its counts do not hold. Each path it answers shares no memory with text.
func ParseDiff(text []byte) ([]FileDiff, error) {
	var reader diffReader
	// The text after the last newline is no line: a text that ends with one ends there.
	for number := 1; len(text) > 0; number++ {
		line, rest, _ := bytes.Cut(text, []byte{'\n'})
		if err := reader.line(line); err != nil {
			return nil, fmt.Errorf("survival: line %d of the patch: %w", number, err)
		}
		text = rest
	}
	if err := reader.flush(); err != nil {
		return nil, fmt.Errorf("survival: the end of the patch: %w", err)
	}
	return reader.files, nil
}

// diffReader is ParseDiff's state: the files read, and the one being read.
type diffReader struct {
	files    []FileDiff
	open     bool
	inHeader bool
	file     FileDiff
	hunks    hunkReader
}

// line reads one line of the patch, which holds no newline.
func (r *diffReader) line(line []byte) error {
	if bytes.HasPrefix(line, diffGitPrefix) {
		if err := r.flush(); err != nil {
			return err
		}
		r.open, r.inHeader = true, true
		r.file = FileDiff{Status: StatusModified}
		r.file.OldPath, r.file.NewPath = gitLinePaths(line[len(diffGitPrefix):])
		return nil
	}
	switch {
	case !r.open:
		return nil
	case bytes.HasPrefix(line, hunkPrefix):
		r.inHeader = false
		return r.hunks.line(line)
	case !r.inHeader:
		return r.hunks.line(line)
	}
	r.header(line)
	return nil
}

// header reads one header line of the file being read.
func (r *diffReader) header(line []byte) {
	is := func(prefix string) bool { return bytes.HasPrefix(line, []byte(prefix)) }
	after := func(prefix string) []byte { return line[len(prefix):] }
	switch {
	case is("--- "):
		r.file.OldPath = headerPath(after("--- "))
	case is("+++ "):
		r.file.NewPath = headerPath(after("+++ "))
	case is("rename from "):
		r.file.OldPath, r.file.Status = namePath(after("rename from ")), StatusRenamed
	case is("rename to "):
		r.file.NewPath, r.file.Status = namePath(after("rename to ")), StatusRenamed
	case is("copy from "):
		r.file.OldPath, r.file.Status = namePath(after("copy from ")), StatusCopied
	case is("copy to "):
		r.file.NewPath, r.file.Status = namePath(after("copy to ")), StatusCopied
	case is("Binary files"), is("GIT binary patch"):
		r.file.Binary = true
	case is("new file mode"):
		r.file.Status, r.file.OldPath = StatusAdded, ""
	case is("deleted file mode"):
		r.file.Status, r.file.NewPath = StatusRemoved, ""
	}
	// git writes a submodule entry's mode on its new file, deleted file, old or new mode or index
	// line.
	if bytes.HasSuffix(line, []byte(" 160000")) && (is("new file mode") || is("deleted file mode") ||
		is("old mode") || is("new mode") || is("index ")) {
		r.file.Submodule = true
	}
}

// flush ends the file being read, if any, and keeps it.
func (r *diffReader) flush() error {
	if !r.open {
		return nil
	}
	hunks, err := r.hunks.end()
	if err != nil {
		return err
	}
	r.file.Hunks = hunks
	r.files = append(r.files, r.file)
	r.open, r.hunks = false, hunkReader{}
	return nil
}

// hunkReader is zero_hunks of patchlib.py as a reader of one file's body, a line at a time:
// each @@ header sets the old and new line the hunk starts at, a run of removed and added lines
// is a Hunk, and a context line ends a run. It also counts each hunk's lines against its
// header's counts.
type hunkReader struct {
	hunks            []Hunk
	started          bool
	oldLine, newLine int
	oldLeft, newLeft int
	run              Hunk
}

// line reads one line of a file's body.
func (h *hunkReader) line(line []byte) error {
	if bytes.HasPrefix(line, hunkPrefix) {
		if err := h.whole(); err != nil {
			return err
		}
		h.emit()
		oldStart, oldCount, newStart, newCount, err := hunkHeader(line)
		if err != nil {
			return err
		}
		h.started = true
		h.oldLine, h.oldLeft, h.newLine, h.newLeft = oldStart, oldCount, newStart, newCount
		return nil
	}
	if !h.started {
		return errors.New("a line of a hunk before its header")
	}
	if len(line) > 0 && line[0] == '\\' { // \ No newline at end of file
		return nil
	}
	switch {
	case len(line) > 0 && line[0] == '-':
		if h.oldLeft == 0 {
			return errors.New("a removed line past its hunk's counts")
		}
		h.oldLeft--
		h.begin()
		h.run.Removed++
		h.oldLine++
	case len(line) > 0 && line[0] == '+':
		if h.newLeft == 0 {
			return errors.New("an added line past its hunk's counts")
		}
		h.newLeft--
		h.begin()
		h.run.Added++
		h.newLine++
	case h.oldLeft == 0 && h.newLeft == 0 && len(line) == 0:
		// An empty line after a hunk's last line, before the next header: the end of a file's
		// body, not a line of it.
		h.emit()
	case h.oldLeft == 0 || h.newLeft == 0:
		return errors.New("a context line past its hunk's counts")
	default:
		// A context line, or the bare empty line some writers give a blank context line.
		h.oldLeft--
		h.newLeft--
		h.emit()
		h.oldLine++
		h.newLine++
	}
	return nil
}

// begin starts a run at the current lines when none is open.
func (h *hunkReader) begin() {
	if h.run.Removed == 0 && h.run.Added == 0 {
		h.run.OldStart, h.run.NewStart = h.oldLine, h.newLine
	}
}

// emit keeps the open run, if any, as a hunk.
func (h *hunkReader) emit() {
	if h.run.Removed > 0 || h.run.Added > 0 {
		h.hunks = append(h.hunks, h.run)
	}
	h.run = Hunk{}
}

// whole says whether the hunk being read holds every line its header counts.
func (h *hunkReader) whole() error {
	if h.oldLeft > 0 || h.newLeft > 0 {
		return fmt.Errorf("a hunk cut short, %d old and %d new lines short of its counts", h.oldLeft, h.newLeft)
	}
	return nil
}

// end ends the body and answers its hunks.
func (h *hunkReader) end() ([]Hunk, error) {
	if err := h.whole(); err != nil {
		return nil, err
	}
	h.emit()
	return h.hunks, nil
}

// hunkHeader reads a hunk's header as zero_hunks reads it, `@@ -a,b +c,d @@`: the text between
// its two @@, cut at " +"; a count left out is 1. A side whose count is 0 is shifted by one, so
// both sources mean the line the run starts at, or the line it inserts before: git with -U0
// writes an insertion as `-a,0`, a the line before it, where the same change inside a GitHub
// hunk leaves the old line on the line after it.
func hunkHeader(line []byte) (oldStart, oldCount, newStart, newCount int, err error) {
	part := line[len(hunkPrefix):]
	if end := bytes.Index(part, hunkPrefix); end >= 0 {
		part = part[:end]
	}
	old, added, found := bytes.Cut(part, []byte(" +"))
	if !found {
		return 0, 0, 0, 0, fmt.Errorf("the hunk header %q has no new side", line)
	}
	oldStart, oldCount, err = hunkSide(bytes.TrimLeft(bytes.TrimSpace(old), "-"))
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("the old side of the hunk header %q: %w", line, err)
	}
	newStart, newCount, err = hunkSide(bytes.TrimSpace(added))
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("the new side of the hunk header %q: %w", line, err)
	}
	return oldStart, oldCount, newStart, newCount, nil
}

// hunkSide reads one side of a hunk header, `start,count` or `start`, with zero_hunks' shift of
// an empty side.
func hunkSide(side []byte) (start, count int, err error) {
	first, second, hasCount := bytes.Cut(side, []byte{','})
	if start, err = strconv.Atoi(string(first)); err != nil {
		return 0, 0, err
	}
	count = 1
	if hasCount {
		if count, err = strconv.Atoi(string(second)); err != nil {
			return 0, 0, err
		}
		if string(second) == "0" {
			start++
		}
	}
	if start < 0 || count < 0 {
		return 0, 0, fmt.Errorf("a negative line or count in %q", side)
	}
	return start, count, nil
}

// gitLinePaths is the two paths of a `diff --git` line, after its "diff --git ": quoted ones
// unquoted, and unquoted ones cut at their one " b/", or at the middle where both halves are
// one path; "" where it cannot tell. Each is stripped of its a/ or b/ prefix.
func gitLinePaths(rest []byte) (oldPath, newPath string) {
	if len(rest) > 0 && rest[0] == '"' {
		first, after, ok := cQuoted(rest)
		if !ok || len(after) < 2 || after[0] != ' ' {
			return "", ""
		}
		second := string(after[1:])
		if after[1] == '"' {
			unquoted, tail, ok := cQuoted(after[1:])
			if !ok || len(tail) > 0 {
				return "", ""
			}
			second = unquoted
		}
		return stripPrefix(first), stripPrefix(second)
	}
	separator := []byte(" b/")
	switch bytes.Count(rest, separator) {
	case 0:
		return "", ""
	case 1:
		old, added, _ := bytes.Cut(rest, separator)
		return stripPrefix(string(old)), string(added)
	}
	half := (len(rest) - len("a/") - len(separator)) / 2
	if bytes.HasPrefix(rest, []byte("a/")) && len(rest) == 2*half+len("a/")+len(separator) &&
		bytes.Equal(rest[len("a/")+half:len("a/")+half+len(separator)], separator) &&
		bytes.Equal(rest[len("a/"):len("a/")+half], rest[len(rest)-half:]) {
		path := string(rest[len(rest)-half:])
		return path, path
	}
	return "", ""
}

// headerPath is the path of a --- or +++ line, after its "--- " or "+++ ": "" for /dev/null, the
// TAB git writes after a path that holds a space dropped, unquoted, and stripped of its a/ or b/
// prefix.
func headerPath(raw []byte) string {
	raw = bytes.TrimSuffix(raw, []byte{'\t'})
	if string(raw) == "/dev/null" {
		return ""
	}
	return stripPrefix(namePath(raw))
}

// namePath is a path git writes alone on a line, as a rename's or a copy's: unquoted where git
// quoted it, and as it stands otherwise.
func namePath(raw []byte) string {
	if len(raw) > 0 && raw[0] == '"' {
		if path, rest, ok := cQuoted(raw); ok && len(rest) == 0 {
			return path
		}
	}
	return string(raw)
}

// stripPrefix is path with its a/ or b/ prefix taken off, as patchlib.py's strip_prefix.
func stripPrefix(path string) string {
	if len(path) >= 2 && (path[:2] == "a/" || path[:2] == "b/") {
		return path[2:]
	}
	return path
}

// cQuoted reads the C-style quoted string git writes for a path that holds a quote, a backslash,
// a control character or (unless core.quotePath is off) a byte above 0x7f: from the opening
// quote that starts raw to its closing one, with git's escapes (\a \b \t \n \v \f \r \" \\ and
// three octal digits). It answers the path, what follows the closing quote, and whether raw held
// such a string.
func cQuoted(raw []byte) (string, []byte, bool) {
	out := make([]byte, 0, len(raw))
	for index := 1; index < len(raw); index++ {
		switch c := raw[index]; c {
		case '"':
			return string(out), raw[index+1:], true
		case '\\':
			index++
			if index >= len(raw) {
				return "", nil, false
			}
			switch escaped := raw[index]; escaped {
			case 'a':
				out = append(out, '\a')
			case 'b':
				out = append(out, '\b')
			case 't':
				out = append(out, '\t')
			case 'n':
				out = append(out, '\n')
			case 'v':
				out = append(out, '\v')
			case 'f':
				out = append(out, '\f')
			case 'r':
				out = append(out, '\r')
			case '"', '\\':
				out = append(out, escaped)
			case '0', '1', '2', '3':
				if index+2 >= len(raw) || !isOctal(raw[index+1]) || !isOctal(raw[index+2]) {
					return "", nil, false
				}
				out = append(out, (escaped-'0')<<6|(raw[index+1]-'0')<<3|(raw[index+2]-'0'))
				index += 2
			default:
				return "", nil, false
			}
		default:
			out = append(out, c)
		}
	}
	return "", nil, false
}

// isOctal says whether c is an octal digit.
func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// FilesEntry is one entry of a files[] page of GitHub's commit route, the fields of it FilesPatch
// reads. Patch is nil where GitHub left the patch out: for a binary file, which has no change of
// lines, and for a file whose patch is too large to serve.
type FilesEntry struct {
	Filename         string  `json:"filename"`
	PreviousFilename string  `json:"previous_filename"`
	Status           string  `json:"status"`
	Patch            *string `json:"patch"`
	Changes          int     `json:"changes"`
}

// FilesPatch reads a files[] page into file diffs, as patchlib.py's api_file_diff: a file's old
// path is its previous_filename, or its own name unless it is added; a removed file has no new
// path; a file with changes and no patch is withheld, which api_file_diff marks binary; and a
// patch is read as one file's body. It answers how many files were withheld: those, and those
// whose patch is not whole by its hunks' counts, so a caller with withheld files holds no whole
// patch of the commit.
func FilesPatch(files []FilesEntry) (diffs []FileDiff, withheld int) {
	diffs = make([]FileDiff, 0, len(files))
	for _, entry := range files {
		diff := FileDiff{OldPath: entry.PreviousFilename, NewPath: entry.Filename, Status: entry.Status}
		if diff.OldPath == "" && entry.Status != StatusAdded {
			diff.OldPath = entry.Filename
		}
		if entry.Status == StatusRemoved {
			diff.NewPath = ""
		}
		switch {
		case entry.Patch == nil:
			if entry.Changes > 0 {
				diff.Binary = true
				withheld++
			}
		case *entry.Patch != "":
			hunks, err := patchHunks([]byte(*entry.Patch))
			if err != nil {
				diff.Binary = true
				withheld++
				break
			}
			diff.Hunks = hunks
		}
		diffs = append(diffs, diff)
	}
	return diffs, withheld
}

// patchHunks reads one file's body, a files[] entry's patch.
func patchHunks(patch []byte) ([]Hunk, error) {
	var reader hunkReader
	for len(patch) > 0 {
		line, rest, _ := bytes.Cut(patch, []byte{'\n'})
		if err := reader.line(line); err != nil {
			return nil, err
		}
		patch = rest
	}
	return reader.end()
}

package leakguard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// gitPins is the configuration every git this package starts reads last: no hook and no
// file-system monitor runs, whatever the repository's own configuration names; no replacement
// object stands in for a commit, so the records read are the commits the push would send; and a
// path is printed as it is, rather than with its non-ASCII bytes escaped, so that what the rules
// read is the path the destination would hold.
var gitPins = []string{
	"--no-replace-objects",
	"-c", "core.quotePath=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
}

// gitArgv is one git command with the pins in front of it, which is how every git of this
// package is started: a shape written out twice is one a later pin can be added to once.
func gitArgv(args ...string) []string {
	return append(append([]string{"git"}, gitPins...), args...)
}

// Update is one line of what git hands a pre-push hook on standard input: the ref being pushed,
// the commit it is at, the ref it is being pushed to, and the commit the destination holds there.
// A sha of all zeros means the ref is not there: on the local side a deletion, on the remote side
// a ref the push creates.
type Update struct {
	LocalRef  string
	LocalSHA  string
	RemoteRef string
	RemoteSHA string
}

// Line is the update as git writes it to a pre-push hook, which is how the shared check reads it.
func (u Update) Line() string {
	return strings.Join([]string{u.LocalRef, u.LocalSHA, u.RemoteRef, u.RemoteSHA}, " ")
}

// ref is the name the destination would hold, which is the name a push publishes. A deletion
// names only the remote ref; a push of a local ref to no remote ref at all can only report the
// local one.
func (u Update) ref() string {
	if u.RemoteRef != "" {
		return u.RemoteRef
	}
	return u.LocalRef
}

// ParseUpdates reads git's pre-push lines, four fields a line, as a `legion leak-check` standing
// in for a pre-push hook reads them from its own standard input. A line with another field count
// is refused rather than guessed at: a check that misread its input would scan the wrong commits.
// So is a line whose sha is not the full object name git writes, 40 or 64 lowercase hex digits
// (objectName): every git the guard starts takes the sha as an argument, where a value beginning
// with "-" would read as an option, and a refusal that quoted a value of other bytes could carry a
// terminal's control bytes. The refusal names the line and never quotes the field.
func ParseUpdates(r io.Reader) ([]Update, error) {
	var updates []Update
	scanner := bufio.NewScanner(r)
	for number := 1; scanner.Scan(); number++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 {
			return nil, fmt.Errorf("a pre-push line holds %d fields, not the four git writes", len(fields))
		}
		if !objectName(fields[1]) || !objectName(fields[3]) {
			return nil, fmt.Errorf("pre-push line %d holds a sha that is not the 40 or 64 lowercase hex digits git writes", number)
		}
		updates = append(updates, Update{LocalRef: fields[0], LocalSHA: fields[1], RemoteRef: fields[2], RemoteSHA: fields[3]})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return updates, nil
}

// objectName is whether sha is a full object name as git writes one to a pre-push hook: 40
// lowercase hex digits under SHA-1, 64 under SHA-256, all zeros for a ref that is not there.
func objectName(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	return strings.Trim(sha, "0123456789abcdef") == ""
}

// PushRequest is one push: the repository it is made from, the remote it is made to, and what it
// would move there.
type PushRequest struct {
	// Dir is the repository the push is made from, which the shared check and the git commands
	// that read the records both run in.
	Dir string
	// Remote is the remote's name, as git hands it to a pre-push hook ("origin").
	Remote string
	// URL is the remote's URL, as git hands it to a pre-push hook. The destination repository is
	// read from it (RepoFromURL).
	URL string
	// Updates is what the push would move, one entry per ref.
	Updates []Update
}

// PushName is what a refusal and a setup failure of a push call the write: "the push to origin",
// and "the push" where the caller has no remote's name. It is exported because a caller that
// reports a failure of its own before the guard runs — `legion leak-check pre-push` reading
// git's update lines — must name the write in the same words the guard would, and two copies of
// one sentence are two that drift apart.
func PushName(remote string) string {
	if remote == "" {
		return "the push"
	}
	return "the push to " + remote
}

// Push checks a push before it is made. The shared check runs in its pre-push shape — the update
// lines on its standard input, the repository as its working directory — and then, for a
// destination GitHub reports public, this repository's own rules read the push's records: each
// ref name, and for each commit the destination does not already hold, the author, the
// committer, the message, each added or renamed path, and each added line. Under a deployment
// that runs no shared check (sharedCheck) the rules are the whole of it.
//
// The records are scanned one at a time as git writes them (pass), so the rule pass never holds
// the push's whole diff.
func (g *Guard) Push(ctx context.Context, req PushRequest) error {
	what := PushName(req.Remote)
	check, err := g.sharedCheck(what)
	if err != nil {
		return err
	}
	defer check.remove()
	if check.path != "" {
		var lines strings.Builder
		for _, update := range req.Updates {
			lines.WriteString(update.Line())
			lines.WriteString("\n")
		}
		// bash runs the check rather than the check running itself: that is the shape git's own
		// pre-push hook calls it in, and the one the check documents for a push.
		if err := g.shared(ctx, what, child{
			argv:  []string{"bash", check.path, req.Remote, req.URL},
			dir:   req.Dir,
			stdin: strings.NewReader(lines.String()),
		}); err != nil {
			return err
		}
	}
	public, err := g.scanThrough(ctx, what, RepoFromURL(req.URL))
	if err != nil {
		return err
	}
	if !public {
		return nil
	}
	pass, err := g.rulePass(ctx, what)
	if err != nil {
		return err
	}
	if err := g.pushRecords(ctx, req, pass.add); err != nil && pass.err == nil {
		return &SetupFailure{What: what, Why: "the push's commits could not be read", Err: err}
	}
	return pass.end(what)
}

// pushRecords hands emit everything a push would publish, as records: one per ref name, and for
// each commit the push would add, one for the author identity, one for the committer identity,
// one for the message, one for each added or renamed path and one for each added line. A commit
// reached by two of the push's refs is read once. An error of emit stops the read and is the
// answer.
func (g *Guard) pushRecords(ctx context.Context, req PushRequest, emit func(Record) error) error {
	read := map[string]bool{}
	for _, update := range req.Updates {
		if ref := update.ref(); ref != "" {
			if err := emit(RefRecord(ref)); err != nil {
				return err
			}
		}
		commits, err := g.commits(ctx, req, update)
		if err != nil {
			return err
		}
		for _, commit := range commits {
			if read[commit] {
				continue
			}
			read[commit] = true
			if err := g.commitRecords(ctx, req.Dir, commit, emit); err != nil {
				return err
			}
		}
	}
	return nil
}

// commits are the commits one update would add to the destination, oldest first: the commits
// the local side has, less the ones the destination is taken to hold already. It holds every
// commit on one of the repository's remote-tracking refs (the named remote's, or every remote's
// where the push names none): a commit there was fetched from the destination, so the push does
// not publish it. Where the destination has a commit at the ref and the repository has it too,
// it also holds that commit and everything before it. The remote-tracking refs count for a ref
// the destination has as much as for a new one. Without them, a forward merge (the default
// branch merged onto an older tip of the branch) would read every commit of the default branch
// since that tip, not only the merge and the branch's own commits. A deletion adds no commit.
//
// The local sha is read as `<sha>^{commit}`: every annotated tag in front of it peeled off, and
// a commit required at the end. A ref can hold a blob or a tree, or a tag of one, and git pushes
// that object; rev-list given it bare lists no commit and exits 0, so the push would add nothing
// to read and pass with the object's text unread. Given ^{commit}, git refuses with exit 128
// ("expected commit type, but the object dereferences to blob type"), which Guard.git makes an
// error of and Guard.Push a setup failure, as for any record git could not read. The "--" ends
// the revisions, so git names the revision it refused and says nothing of a path.
func (g *Guard) commits(ctx context.Context, req PushRequest, update Update) ([]string, error) {
	if update.LocalSHA == "" || zeroSHA(update.LocalSHA) {
		return nil, nil
	}
	argv := []string{"rev-list", "--reverse", update.LocalSHA + "^{commit}", "--not"}
	held, err := g.holds(ctx, req.Dir, update.RemoteSHA)
	if err != nil {
		return nil, err
	}
	if held {
		argv = append(argv, update.RemoteSHA)
	}
	remotes := "--remotes"
	if req.Remote != "" {
		remotes += "=" + req.Remote
	}
	argv = append(argv, remotes, "--")
	out, err := g.git(ctx, req.Dir, argv...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// holds is whether the repository has the commit sha names: false for a ref the push creates
// (a sha of all zeros), and false for a remote sha this repository has never fetched, where the
// remote-tracking refs alone say what the destination holds.
func (g *Guard) holds(ctx context.Context, dir, sha string) (bool, error) {
	if sha == "" || zeroSHA(sha) {
		return false, nil
	}
	status, _, err := g.run(ctx, child{argv: gitArgv("cat-file", "-e", sha+"^{commit}"), dir: dir})
	if err != nil {
		return false, err
	}
	return status == 0, nil
}

// zeroSHA is whether sha is git's "this ref is not there": all zeros, of whatever length the
// repository's hash is.
func zeroSHA(sha string) bool {
	return sha != "" && strings.Trim(sha, "0") == ""
}

// commitRecords hands emit one commit as records. The identities and the message come from the
// commit itself; the added and renamed paths and the added lines come from its diff, read in the
// shape its number of parents calls for (diffTree).
//
// A record about a file carries that file's path as its label and the line or the path as its
// text, so that the engine reads the path as a path: the rule file's one path-scoped allowlist
// is anchored on a whole path, and a label carrying anything else would take it away.
func (g *Guard) commitRecords(ctx context.Context, dir, commit string, emit func(Record) error) error {
	// git writes the separator for itself, as %x00 in the format: an argument of this process's
	// own holding a zero byte is a child that cannot be started at all. %P comes before the
	// body, which is the one field that can hold a newline, so the parents are read without a
	// second git.
	const separator = "%x00"
	out, err := g.git(ctx, dir, "show", "--no-patch", "--no-color",
		"--format="+separator+"%an <%ae>"+separator+"%cn <%ce>"+separator+"%P"+separator+"%B"+separator, commit)
	if err != nil {
		return err
	}
	parts := strings.Split(out, "\x00")
	if len(parts) < 5 {
		return fmt.Errorf("git show %s answered no identity, parents and message", short(commit))
	}
	parents := len(strings.Fields(parts[3]))
	for _, record := range []Record{
		{Label: "commit " + short(commit) + " author", Text: parts[1]},
		{Label: "commit " + short(commit) + " committer", Text: parts[2]},
		{Label: "commit " + short(commit) + " message", Text: parts[4]},
	} {
		if err := emit(record); err != nil {
			return err
		}
	}
	if err := g.addedPaths(ctx, dir, commit, parents, emit); err != nil {
		return err
	}
	return g.addedLines(ctx, dir, commit, parents, emit)
}

// diffTree is a diff-tree of one commit, in the shape its parents call for: --root for a root
// commit, whose diff is otherwise empty, and -c for a merge.
//
// -c is the combined diff, which is the merge's own resolution: the lines of the result that
// stand in none of the parents, and the files modified from all of them. That is what a merge
// publishes of its own and what no other record of this push holds. Everything else the merge
// carries came from one of its parents, and a parent is either a commit this same push adds —
// read as records of its own, since Guard.commits lists every commit the destination does not
// reach — or a commit the destination already holds. --cc, the dense form, drops more than this
// (a hunk whose result matches one parent exactly), and -m, the diff against each parent in
// turn, reads whole branches the destination already has; between the three, -c is the one that
// reads the resolution and nothing the destination can already read.
//
// A git that refuses the option answers a non-zero status, which Guard.git makes an error of and
// Guard.Push a setup failure: a diff that could not be read refuses the push, and is never a
// commit read as clean.
func diffTree(parents int, args ...string) []string {
	argv := []string{"diff-tree", "-r", "-M", "--no-commit-id"}
	switch {
	case parents == 0:
		argv = append(argv, "--root")
	case parents > 1:
		argv = append(argv, "-c")
	}
	return append(argv, args...)
}

// addedPaths hands emit, as a record of each, the paths one commit adds or renames a file to,
// which are the paths the destination would hold that it does not hold already. They are read as
// git writes them, one field at a time (eachField).
//
// A merge's combined diff writes one status letter per parent and one path, with no path a
// rename came from, where an ordinary diff writes one letter and, for a rename, the two paths.
// Both are read here: a status of as many letters as the commit has parents is the combined
// shape.
func (g *Guard) addedPaths(ctx context.Context, dir, commit string, parents int, emit func(Record) error) error {
	combined := parents > 1
	// pending is how many paths of the current entry are still to come; the last is the one the
	// destination learns. Zero is a field that is a status.
	pending := 0
	_, err := g.gitWith(ctx, dir, func(out io.Reader) error {
		return eachField(out, 0, func(field []byte) error {
			field = bytes.TrimSuffix(field, []byte{0})
			if pending == 0 {
				switch status := string(field); {
				case status == "":
				case !combined && strings.HasPrefix(status, "R"):
					// A rename is three fields: the status, the path it was at, and the path it
					// is at. The destination learns the new path alone.
					pending = 2
				default:
					pending = 1
				}
				return nil
			}
			pending--
			if pending > 0 {
				return nil
			}
			path := string(field)
			return emit(Record{Label: path, Text: path})
		})
	}, diffTree(parents, "--name-status", "--diff-filter=AR", "-z", commit)...)
	return err
}

// addedLines hands emit the lines one commit adds, each as a record of the file it is added to.
// The diff is read with no context lines, so every line of it that a patch marks as added is one,
// and it is read as git writes it, one line held at a time (eachField).
//
// A merge's combined diff marks each line with one column per parent, so a line added against
// any of them begins with that many characters, one of which is a "+" ("++text" for a line of
// the resolution's own, in a merge of two). The whole prefix comes off, and a line added against
// one parent alone is read as added: the destination gains it from this commit whatever a parent
// holds. The "+++ b/<path>" that names the file is read only where a header stands, between a
// "diff --" line and the hunk that follows it, so a line of content that reads like one is not
// mistaken for the name of a file.
//
// Every file is read as text (--text). Without it git prints "Binary files … differ" and no line
// at all for a file that holds a NUL byte, or whose path a `-diff` attribute or a driver with
// `binary = true` covers, from any attributes source (the commit's own .gitattributes,
// .git/info/attributes, core.attributesFile, the system file); the destination still gets the
// file, so its text would pass unread. --text puts a file's bytes in the diff whatever the
// attributes say, which leaves them one way to change what is read, a driver's textconv or an
// external diff command, and diff-tree runs neither unless asked; --no-textconv and --no-ext-diff
// say so here rather than leave it to a default.
//
// A merge's combined diff does not honour --text: for such a file it still prints "Binary files
// differ" under its "diff --combined <path>" header. With --full-index that header's index line
// names the file's blob in each parent and in the merge, and with --raw the lines ahead of the
// patch name each file's mode in each parent and in the merge. Each file printed so is read again
// from those blobs, as text (mergeBinaryLines): no pathspec, so neither the path's bytes nor a
// GIT_*_PATHSPECS variable the pusher's environment carries can make the read miss it. A
// "Binary files" line under any other diff, or under a combined header whose path, blob ids or
// modes cannot be read, is an error, so a git that leaves a file unread refuses the push rather
// than passing it.
func (g *Guard) addedLines(ctx context.Context, dir, commit string, parents int, emit func(Record) error) error {
	args := []string{"--unified=0", "--no-color", "--full-index", "--text", "--no-textconv", "--no-ext-diff"}
	if parents > 1 {
		args = append(args, "--raw")
	}
	binary, err := g.diffLines(ctx, dir, commit, parents > 1, max(parents, 1), emit, diffTree(parents, append(args, commit)...)...)
	if err != nil {
		return err
	}
	for _, file := range binary {
		if err := g.mergeBinaryLines(ctx, dir, commit, file, emit); err != nil {
			return err
		}
	}
	return nil
}

// gitlinkMode is the mode git gives a submodule's entry, whose id names a commit of another
// repository rather than a blob of this one.
const gitlinkMode = "160000"

// binaryFile is a file a merge's combined diff printed as binary: its path, which labels its
// records and plays no part in reading it, and the blob ids its index line names, and the modes its
// raw line names, for the merge's first parent and for the merge itself; an id of all zeros where
// the file is not there.
type binaryFile struct {
	path, first, result, firstMode, resultMode string
}

// mergeBinaryLines hands emit, labelled with the file's path, the lines file gains in the merge
// against the merge's first parent, read as text from the two blobs. Every line of the merge's
// own resolution stands in neither parent, so it is a line the file gains against the first
// parent; the read also takes the lines it gains from the other parents, which is more than the
// resolution, but only for these files, and each line once.
//
// The cases, in the order they are decided. A file the merge deletes, and a submodule's entry in
// the merge, whose id is a commit of another repository, publish no text and are not read. A file
// new against the first parent, or that was a submodule's entry there, is read whole
// (Guard.blobLines): the first parent's diff showed at most "Subproject commit <id>", so no read
// of this push or of the destination has read the blob's text, even where the merge's blob id is
// the id that entry named. A file whose blob the merge keeps from a first parent that held it as a
// file, changing only its mode or its type (a file made executable, or a symlink), publishes no
// text that parent lacks and is not read; the combined diff lists it because its mode differs
// from every parent's. Every other file is read as the diff of its two blobs, and a read that
// prints no "+++" line, a diff of nothing, is an error: the two blobs differ.
func (g *Guard) mergeBinaryLines(ctx context.Context, dir, commit string, file binaryFile, emit func(Record) error) error {
	switch {
	case zeroSHA(file.result), file.resultMode == gitlinkMode:
		return nil
	case zeroSHA(file.first), file.firstMode == gitlinkMode:
		return g.blobLines(ctx, dir, file, emit)
	case file.first == file.result:
		return nil
	}
	read, header := false, true
	_, err := g.gitWith(ctx, dir, func(out io.Reader) error {
		return eachField(out, '\n', func(line []byte) error {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case bytes.HasPrefix(line, []byte("diff --")):
				header = true
			case header && bytes.HasPrefix(line, []byte("Binary files ")):
				return fmt.Errorf("git diff printed a blob of merge %s as binary despite --text", short(commit))
			case header && bytes.HasPrefix(line, []byte("+++ ")):
				read = true
			case bytes.HasPrefix(line, []byte("@@")):
				header = false
			case header:
			case bytes.HasPrefix(line, []byte("+")):
				if text := line[1:]; len(bytes.TrimSpace(text)) != 0 {
					return emit(Record{Label: file.path, Text: string(text)})
				}
			}
			return nil
		})
	}, "diff", "--text", "--no-textconv", "--no-ext-diff", "--no-color", "--unified=0", file.first, file.result, "--")
	if err != nil {
		return err
	}
	if !read {
		return fmt.Errorf("git diff read no diff of a file the combined diff of merge %s printed as binary", short(commit))
	}
	return nil
}

// blobLines hands emit each line of file's blob in the merge, labelled with its path: the read of a
// file new against the merge's first parent, every line of which the merge adds against it.
func (g *Guard) blobLines(ctx context.Context, dir string, file binaryFile, emit func(Record) error) error {
	_, err := g.gitWith(ctx, dir, func(out io.Reader) error {
		return eachField(out, '\n', func(line []byte) error {
			if text := bytes.TrimRight(line, "\r\n"); len(bytes.TrimSpace(text)) != 0 {
				return emit(Record{Label: file.path, Text: string(text)})
			}
			return nil
		})
	}, "cat-file", "blob", file.result)
	return err
}

// diffLines runs the diff-tree args name and hands emit each line it marks as added, the first
// width characters of a line being its marks. Where combined, it answers the files whose combined
// diff printed "Binary files differ", each with the blob ids of its "index" line and the modes of
// its raw line, for the caller to read again; anywhere else such a line is an error, and so is one
// under a header whose path, blob ids or modes cannot be read, and a raw line that cannot be read.
// The error names the commit and never the path, which is the push's own text.
func (g *Guard) diffLines(ctx context.Context, dir, commit string, combined bool, width int, emit func(Record) error, args ...string) ([]binaryFile, error) {
	var binary []binaryFile
	// modes are the first parent's and the merge's mode of each path the raw lines name, which
	// come ahead of the patch, before its first header.
	modes := map[string][2]string{}
	patched := false
	path, header := "", false
	var file binaryFile
	readable, indexed := false, false
	unread := func() error {
		return fmt.Errorf("git diff-tree printed a file of commit %s as binary despite --text", short(commit))
	}
	_, err := g.gitWith(ctx, dir, func(out io.Reader) error {
		return eachField(out, '\n', func(line []byte) error {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case combined && !patched && bytes.HasPrefix(line, []byte(":")):
				raw, first, result, ok := combinedRaw(string(line))
				if !ok {
					return fmt.Errorf("git diff-tree printed a raw line of commit %s that could not be read", short(commit))
				}
				modes[raw] = [2]string{first, result}
			case bytes.HasPrefix(line, []byte("diff --")):
				path, header, indexed, patched = "", true, false, true
				if combined {
					file = binaryFile{}
					file.path, readable = combinedPath(string(line))
				}
			case combined && header && bytes.HasPrefix(line, []byte("index ")):
				file.first, file.result, indexed = combinedIndex(string(line))
			case header && bytes.HasPrefix(line, []byte("Binary files ")):
				if !combined || !readable || !indexed {
					return unread()
				}
				mode, ok := modes[file.path]
				if !ok {
					return unread()
				}
				file.firstMode, file.resultMode = mode[0], mode[1]
				binary = append(binary, file)
			case header && bytes.HasPrefix(line, []byte("+++ ")):
				path = diffPath(string(line[len("+++ "):]))
			case bytes.HasPrefix(line, []byte("@@")):
				header = false
			case header || path == "" || len(line) < width:
			case bytes.Contains(line[:width], []byte("+")):
				if text := line[width:]; len(bytes.TrimSpace(text)) != 0 {
					return emit(Record{Label: path, Text: string(text)})
				}
			}
			return nil
		})
	}, args...)
	return binary, err
}

// combinedRaw is the path a combined diff's raw line names, decoded byte for byte (gitUnquote),
// and the modes it gives the merge's first parent and the merge, and whether the line is of that
// shape: one ':' per parent, a mode for each parent and the merge, an id for each, the status,
// a tab and the path.
func combinedRaw(line string) (path, first, result string, ok bool) {
	parents := len(line) - len(strings.TrimLeft(line, ":"))
	meta, quoted, found := strings.Cut(line[parents:], "\t")
	fields := strings.Fields(meta)
	if parents < 2 || !found || len(fields) != 2*(parents+1)+1 {
		return "", "", "", false
	}
	path, ok = gitUnquote(quoted)
	return path, fields[0], fields[parents], ok && path != ""
}

// combinedIndex is the blob ids a combined diff's "index <parent>,<parent>..<merge>" line names,
// under --full-index: the first parent's and the merge's, and whether the line is of that shape,
// with two or more parents and every id a full object name.
func combinedIndex(line string) (first, result string, ok bool) {
	parents, result, found := strings.Cut(strings.TrimPrefix(line, "index "), "..")
	ids := strings.Split(parents, ",")
	if !found || len(ids) < 2 || !objectName(result) {
		return "", "", false
	}
	for _, id := range ids {
		if !objectName(id) {
			return "", "", false
		}
	}
	return ids[0], result, true
}

// combinedPath is the path a combined diff's "diff --combined <path>" header names, with git's C
// quoting undone byte for byte (gitUnquote), and whether it could be read. The header carries no
// a/ or b/ prefix and no trailing tab.
func combinedPath(line string) (string, bool) {
	field, ok := strings.CutPrefix(line, "diff --combined ")
	if !ok {
		field, ok = strings.CutPrefix(line, "diff --cc ")
	}
	if !ok || field == "" {
		return "", false
	}
	path, ok := gitUnquote(field)
	return path, ok && path != ""
}

// gitUnquote is field with git's C quoting undone byte for byte, and whether it could be; a field
// git did not quote comes back as it is. git quotes a path that holds a control byte, a '"' or a
// '\' (its quote_c_style), writing \a \b \t \n \v \f \r \" \\ or a three-digit octal escape, and
// under core.quotePath=false it leaves every byte of 0x80 and above raw. strconv.Unquote would
// turn such a raw byte that is not UTF-8 into U+FFFD, and a record's label would then not be the
// path the destination holds.
func gitUnquote(field string) (string, bool) {
	if !strings.HasPrefix(field, `"`) {
		return field, true
	}
	if len(field) < 2 || field[len(field)-1] != '"' {
		return "", false
	}
	in := field[1 : len(field)-1]
	out := make([]byte, 0, len(in))
	octal := func(c byte) bool { return c >= '0' && c <= '7' }
	for i := 0; i < len(in); i++ {
		switch c := in[i]; c {
		case '"':
			return "", false
		case '\\':
		default:
			out = append(out, c)
			continue
		}
		i++
		if i >= len(in) {
			return "", false
		}
		switch e := in[i]; e {
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
			out = append(out, e)
		case '0', '1', '2', '3':
			if i+2 >= len(in) || !octal(in[i+1]) || !octal(in[i+2]) {
				return "", false
			}
			out = append(out, (e-'0')<<6|(in[i+1]-'0')<<3|(in[i+2]-'0'))
			i += 2
		default:
			return "", false
		}
	}
	return string(out), true
}

// eachField hands each field of r that ends in delim to fn, delim included where it has one, as
// a slice fn must copy what it keeps of: a line of a diff for '\n', a field of a -z listing for 0.
// It holds one field at a time, however long: a field longer than its buffer is gathered whole
// before fn sees it. An error of fn stops the read and is the answer.
func eachField(r io.Reader, delim byte, fn func(field []byte) error) error {
	in := bufio.NewReaderSize(r, 64<<10)
	var long []byte
	for {
		chunk, err := in.ReadSlice(delim)
		if errors.Is(err, bufio.ErrBufferFull) {
			long = append(long, chunk...)
			continue
		}
		field := chunk
		if len(long) > 0 {
			long = append(long, chunk...)
			field = long
		}
		if len(field) > 0 {
			if err := fn(field); err != nil {
				return err
			}
		}
		// A long field's buffer is let go rather than kept for the rest of the read.
		long = nil
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// diffPath is the path a patch's "+++" line names, with the b/ prefix off and git's C quoting of a
// path with a byte git will not print raw undone byte for byte (gitUnquote). A deletion's
// /dev/null names no file. git ends the line with a tab where the path holds a space, and that tab
// is no part of the path: a tab of the path itself is always C-quoted.
func diffPath(field string) string {
	field = strings.TrimSuffix(field, "\t")
	if field == "/dev/null" {
		return ""
	}
	if unquoted, ok := gitUnquote(field); ok {
		field = unquoted
	}
	return strings.TrimPrefix(field, "b/")
}

// short is a commit as a report names it.
func short(commit string) string {
	return commit[:min(len(commit), 7)]
}

// git runs one git command in dir and answers its standard output, failing where git itself does:
// a record this package could not read is a setup failure, never a push read as clean. A failure
// quotes only what git wrote on its standard error (gitSaid), never its standard output, which is
// the push's own content (a diff, a commit's message) and can be large, while git says why it
// failed on its standard error. Reading the two apart also keeps a warning git writes on its
// standard error out of the records parsed from its output.
func (g *Guard) git(ctx context.Context, dir string, args ...string) (string, error) {
	return g.gitWith(ctx, dir, nil, args...)
}

// gitWith is git with its standard output read by stdout as git writes it (Guard.readOutput),
// where stdout is not nil, rather than answered whole: a push's added paths and diff are read so.
func (g *Guard) gitWith(ctx context.Context, dir string, stdout func(io.Reader) error, args ...string) (string, error) {
	var stderr bytes.Buffer
	c := child{argv: gitArgv(args...), dir: dir, stderr: &stderr}
	var status int
	var out string
	var err error
	if stdout == nil {
		status, out, err = g.run(ctx, c)
	} else {
		status, err = g.readOutput(ctx, c, stdout)
	}
	if err != nil {
		return "", procgroup.Said(err, gitSaid(stderr.String()))
	}
	if status != 0 {
		return "", fmt.Errorf("git %s exited %d: %s", args[0], status, gitSaid(stderr.String()))
	}
	return out, nil
}

// gitErrorBytes is the most of git's standard error a failure of Guard.git quotes: git says why it
// failed in a line or a few, and a failure that wrote more is cut there rather than carried whole
// into the setup failure a role reads.
const gitErrorBytes = 4096

// gitSaid is what a failure of Guard.git quotes of the standard error git wrote: trimmed and, past
// gitErrorBytes, its last gitErrorBytes bytes, after the count of the bytes left out before them.
// The end is what is kept because git writes its `fatal:` line last, after any warnings. git can
// quote a remote's URL, a token in its userinfo or its query, so the end is cut by redact.Tail,
// which reads the whole of it through the URL rule first: a cut made first could start inside that
// userinfo or query and leave a part of it where the rule no longer finds it. The cut moves forward
// to the start of a character, looking at most utf8.UTFMax bytes ahead and staying put where none
// starts there, so a valid character is never split and an invalid run cannot take the whole
// quote.
func gitSaid(stderr string) string {
	said, cut := redact.Tail(strings.TrimSpace(stderr), gitErrorBytes)
	if cut == 0 {
		return said
	}
	return fmt.Sprintf("(%d bytes left out) …%s", cut, said)
}

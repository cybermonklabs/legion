package leakguard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"

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
func ParseUpdates(r io.Reader) ([]Update, error) {
	var updates []Update
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 {
			return nil, fmt.Errorf("a pre-push line holds %d fields, not the four git writes", len(fields))
		}
		updates = append(updates, Update{LocalRef: fields[0], LocalSHA: fields[1], RemoteRef: fields[2], RemoteSHA: fields[3]})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return updates, nil
}

// PushRequest is one push: the repository it is made from, the remote it is made to, and what it
// would move there.
type PushRequest struct {
	// Dir is the repository the push is made from, which the shared check and the git commands
	// that read the records both run in.
	Dir string
	// Remote is the remote's name, as git hands it to a pre-push hook ("origin"): for a push to a
	// URL rather than to a remote (`git push <url>`), git hands the URL as typed here.
	Remote string
	// URL is the remote's URL, as git hands it to a pre-push hook. The destination repository is
	// read from it (RepoFromURL).
	URL string
	// Updates is what the push would move, one entry per ref.
	Updates []Update
}

// PushName is what a refusal and a setup failure of a push call the write: "the push to origin",
// the remote as git named it to the hook and as shownText shows it, and "the push" where the
// caller has no remote's name. It is exported because a caller that reports a failure of its own
// before the guard runs — `legion leak-check pre-push` reading git's update lines — must name the
// write in the same words the guard would, and two copies of one sentence are two that drift
// apart.
func PushName(remote string) string {
	if remote == "" {
		return "the push"
	}
	return "the push to " + shownText(remote)
}

// Push checks a push before it is made. The shared check runs in its pre-push shape — the update
// lines on its standard input, the repository as its working directory — and then, where a rule is
// named and for a destination GitHub reports public, the deployment's private-name rules read the
// push's records: each ref name, and for each commit the destination does not already hold, the
// author, the committer, the message, each added or renamed path, and each added line. Where no
// rule is named, no visibility is read and no git reads a commit. Under a deployment that runs no
// shared check (sharedCheck) the rules are the whole of it.
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
	rules, err := g.rules(what)
	if err != nil {
		return err
	}
	if rules == nil {
		return nil
	}
	public, err := g.scanThrough(ctx, what, RepoFromURL(req.URL))
	if err != nil {
		return err
	}
	if !public {
		return nil
	}
	pass := rules.newPass(ctx)
	if err := g.pushRecords(ctx, req, pass.add); err != nil && pass.err == nil {
		return &SetupFailure{What: what, Why: "the push's commits could not be read", Err: err}
	}
	return pass.end(what)
}

// pushRecords hands emit everything a push would publish, as records: one per ref name, and for
// each commit the push would add, one for the author identity, one for the committer identity,
// one for the message, one for each added or renamed path and one for each added line. A commit
// reached by two of the push's refs is read once. A deletion adds no commit, and a push of
// deletions alone reads nothing of the repository. An error of emit stops the read and is the
// answer.
func (g *Guard) pushRecords(ctx context.Context, req PushRequest, emit func(Record) error) error {
	read := map[string]bool{}
	var held []string
	heldRead := false
	// from is what the refusal of a shallow push names to fetch from: the remote the push names,
	// or for a push to a URL, that URL with its userinfo and its query removed, since a token in
	// either is not to be printed.
	from := ""
	for _, update := range req.Updates {
		if ref := update.ref(); ref != "" {
			if err := emit(RefRecord(ref)); err != nil {
				return err
			}
		}
		if update.LocalSHA == "" || zeroSHA(update.LocalSHA) {
			continue
		}
		if !heldRead {
			var named bool
			var err error
			if held, named, err = g.heldRemotes(ctx, req); err != nil {
				return err
			}
			from = redact.URLCredentials(req.URL)
			if named {
				from = req.Remote
			}
			heldRead = true
		}
		commits, err := g.commits(ctx, req.Dir, held, from, update)
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

// heldRemotes are the remotes whose remote-tracking refs say what the push's destination holds:
// each remote whose every URL, fetch and push, names the repository the push's URL names
// (sameRepository), so that its tracking refs hold what the destination has, and among whose
// refs no other remote's fetch can file one (filedByAnother). Where the push names a remote of the
// repository, which named answers, that remote is the only one it can hold. git hands a pre-push
// hook the URL where the remote's name goes for a push to a URL (`git push <url>`), and no
// tracking ref is filed under a URL, so such a push can hold any remote. The URLs on both sides
// are git's own, after its insteadOf rewriting. A push that holds no remote has every commit read
// that the destination's ref does not reach: a remote that is not shown to be the destination
// says nothing of what it holds.
//
// What is read is the repository as it is now, so two kinds of ref are held as a remote's
// although its repository need not hold them, as `--remotes=<name>` held them before: refs the
// remote fetched before a `git remote set-url` to another repository, and refs under
// refs/remotes/<name>/ that no configuration files there, which a fetch given a refspec on its
// command line (`git fetch <url> <src>:refs/remotes/<name>/<ref>`) and `git update-ref` write.
//
// The URLs are read with `git remote get-url`, which prints one URL a line and nothing else, and
// not from `git remote -v`, which writes a blobless clone's filter after its fetch URL. The
// refspecs are read from `git config -z --list`, which, unlike a `--get-regexp` that matches
// nothing, exits 0 whatever the configuration holds, and from the legacy files under
// .git/remotes (legacyFetchDestinations).
func (g *Guard) heldRemotes(ctx context.Context, req PushRequest) (held []string, named bool, err error) {
	out, err := g.git(ctx, req.Dir, "remote")
	if err != nil {
		return nil, false, err
	}
	// One name a line; a remote's name holds no space, since git refuses a remote whose tracking
	// refs would have an invalid name.
	names := strings.Fields(out)
	candidates := names
	if named = slices.Contains(names, req.Remote); named {
		candidates = []string{req.Remote}
	}
	config, err := g.git(ctx, req.Dir, "config", "-z", "--list")
	if err != nil {
		return nil, false, err
	}
	files := fetchDestinations(config)
	if err := g.legacyFetchDestinations(ctx, req.Dir, files); err != nil {
		return nil, false, err
	}
	elsewhere := func(url string) bool { return url != "" && !sameRepository(url, req.URL) }
	for _, name := range candidates {
		if filedByAnother(name, files) {
			continue
		}
		fetch, err := g.git(ctx, req.Dir, "remote", "get-url", "--all", "--", name)
		if err != nil {
			return nil, false, err
		}
		push, err := g.git(ctx, req.Dir, "remote", "get-url", "--push", "--all", "--", name)
		if err != nil {
			return nil, false, err
		}
		// A URL can hold a space (a path on this machine), so the URLs are split on the lines git
		// prints them on, and the empty string after the last line is no URL.
		urls := strings.Split(fetch+push, "\n")
		if strings.TrimSpace(fetch+push) != "" && !slices.ContainsFunc(urls, elsewhere) {
			held = append(held, name)
		}
	}
	return held, named, nil
}

// sameRepository is whether two URLs git reaches name one repository: the same repository on
// GitHub (RepoFromURL, letter case aside) where either names one there, so that a token in the
// userinfo, ssh's scp syntax and the https form all name one repository, and the same URL where
// neither does.
func sameRepository(a, b string) bool {
	if repoA, repoB := RepoFromURL(a), RepoFromURL(b); repoA != "" || repoB != "" {
		return strings.EqualFold(repoA, repoB)
	}
	return a == b
}

// fetchDestinations are, for each remote, the refs its fetch refspecs (remote.<name>.fetch) file
// what it fetches at, read from `git config -z --list` (fetchDestination).
func fetchDestinations(config string) map[string][]string {
	files := map[string][]string{}
	for entry := range strings.SplitSeq(config, "\x00") {
		key, refspec, _ := strings.Cut(entry, "\n")
		name, ok := strings.CutPrefix(key, "remote.")
		if !ok {
			continue
		}
		if name, ok = strings.CutSuffix(name, ".fetch"); !ok {
			continue
		}
		if destination, ok := fetchDestination(refspec); ok {
			files[name] = append(files[name], destination)
		}
	}
	return files
}

// legacyFetchDestinations adds to files the destinations of the fetch refspecs in the legacy
// remote files, .git/remotes/<name>, which `git fetch <name>` still reads where the configuration
// gives the remote no URL (git's read_remotes_file, which builds without WITH_BREAKING_CHANGES
// keep), although `git remote` does not list such a remote. A fetch refspec is a line that begins
// "Pull:", after the white space around it. Every file is read, whether or not the configuration
// also names its remote: a refspec counted that git would not use only keeps a remote unheld. The
// directory is the one git reads, under the common directory a linked worktree shares; where it
// is not there, there is no file.
func (g *Guard) legacyFetchDestinations(ctx context.Context, dir string, files map[string][]string) error {
	path, err := g.git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "remotes")
	if err != nil {
		return err
	}
	remotes := strings.TrimSuffix(path, "\n")
	entries, err := os.ReadDir(remotes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(remotes, entry.Name()))
		if err != nil {
			return err
		}
		for line := range strings.Lines(string(content)) {
			refspec, ok := strings.CutPrefix(strings.TrimRightFunc(line, unicode.IsSpace), "Pull:")
			if !ok {
				continue
			}
			if destination, ok := fetchDestination(strings.TrimLeftFunc(refspec, unicode.IsSpace)); ok {
				files[entry.Name()] = append(files[entry.Name()], destination)
			}
		}
	}
	return nil
}

// fetchDestination is the ref a fetch refspec files what it fetches at: its destination, a
// pattern where it holds a "*". A refspec with no destination, and a negative one, files no ref.
func fetchDestination(refspec string) (string, bool) {
	if strings.HasPrefix(refspec, "^") {
		return "", false
	}
	_, destination, ok := strings.Cut(strings.TrimPrefix(refspec, "+"), ":")
	return destination, ok && destination != ""
}

// filedByAnother is whether a remote other than name has a fetch refspec that can file a ref under
// refs/remotes/<name>/, every ref of which `--remotes=<name>` takes: a destination there, as a
// remote named <name>/<more> has by default, or a pattern whose "*" can reach there, such as a
// mirror's refs/*. A destination outside refs/ is one git qualifies by rules of its own, and is
// taken to reach anywhere.
func filedByAnother(name string, files map[string][]string) bool {
	own := "refs/remotes/" + name + "/"
	for other, destinations := range files {
		if other == name {
			continue
		}
		for _, destination := range destinations {
			head, _, pattern := strings.Cut(destination, "*")
			if !strings.HasPrefix(destination, "refs/") || strings.HasPrefix(head, own) || pattern && strings.HasPrefix(own, head) {
				return true
			}
		}
	}
	return false
}

// commits are the commits one update would add to the destination, oldest first: the commits
// the local side has, less the ones the destination is taken to hold already. It holds every
// commit on a remote-tracking ref of the held remotes (heldRemotes): a commit there was fetched
// from the destination, so the push does not publish it. Where the destination has a commit at
// the ref and the repository has it too, it also holds that commit and everything before it. The
// remote-tracking refs count for a ref the destination has as much as for a new one. Without them,
// a forward merge (the default branch merged onto an older tip of the branch) would read every
// commit of the default branch since that tip, not only the merge and the branch's own commits.
//
// In a shallow repository the walk from a remote-tracking ref stops at a shallow commit, whose
// parents the repository does not hold, so the destination's commits beneath it read as the
// push's own: a scan of history the destination has published, refused over a commit it already
// holds. A commit of the push with parents the repository does not hold is therefore refused
// rather than read (shallowFloor).
func (g *Guard) commits(ctx context.Context, dir string, held []string, from string, update Update) ([]string, error) {
	// --parents writes each commit's parents after it, as the repository holds them: none for a
	// root commit, and none for a shallow one.
	argv := []string{"rev-list", "--reverse", "--parents", update.LocalSHA, "--not"}
	has, err := g.holds(ctx, dir, update.RemoteSHA)
	if err != nil {
		return nil, err
	}
	if has {
		argv = append(argv, update.RemoteSHA)
	}
	for _, remote := range held {
		argv = append(argv, "--remotes="+remote)
	}
	out, err := g.git(ctx, dir, argv...)
	if err != nil {
		return nil, err
	}
	var commits []string
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) == 1 {
			if err := g.shallowFloor(ctx, dir, fields[0], from); err != nil {
				return nil, err
			}
		}
		commits = append(commits, fields[0])
	}
	return commits, nil
}

// shallowFloor is an error where commit, which the repository holds no parent of, is a shallow
// commit rather than a root commit: its own object names parents, which a fetch with a depth left
// out. The error names the commit and the fetch that brings its parents in from the push's own
// destination, from (pushRecords), and the default remote's fetch where from is empty.
func (g *Guard) shallowFloor(ctx context.Context, dir, commit, from string) error {
	object, err := g.git(ctx, dir, "cat-file", "commit", commit)
	if err != nil {
		return err
	}
	header, _, _ := strings.Cut(object, "\n\n")
	if !strings.HasPrefix(header, "parent ") && !strings.Contains(header, "\nparent ") {
		return nil
	}
	fetch := strings.TrimSpace("git fetch --unshallow " + from)
	return fmt.Errorf("commit %s is a shallow commit of this repository, which holds none of its parents, so which of the commits beneath it the destination holds cannot be read; `%s` fetches them, and the push can then be made again", short(commit), fetch)
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
// A record about a file carries that file's path as its label and as its Path, which is what the
// engine's path rules and a rule file's path-scoped allowlist read, and the path or the line as its
// text; an added line carries the line of the file it is added at as its FirstLine (addedLines).
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
			return emit(Record{Label: path, Text: path, Path: path})
		})
	}, diffTree(parents, "--name-status", "--diff-filter=AR", "-z", commit)...)
	return err
}

// addedLines hands emit the lines one commit adds, each as a record of the file it is added to and
// of the line of that file it stands at. The diff is read with no context lines, so every line of
// it that a patch marks as added is one, and it is read as git writes it, one line held at a time
// (eachField).
//
// A merge's combined diff marks each line with one column per parent, so a line added against
// any of them begins with that many characters, one of which is a "+" ("++text" for a line of
// the resolution's own, in a merge of two). The whole prefix comes off, and a line added against
// one parent alone is read as added: the destination gains it from this commit whatever a parent
// holds. The "+++ b/<path>" that names the file (with a tab after the path where it holds a
// space, which diffPath takes off) is read only where a header stands, between a "diff --" line
// and the hunk that follows it, so a line of content that reads like one is not mistaken for the
// name of a file.
//
// A line's place is counted from its hunk's header (hunkStart), one up for each line of the hunk
// that the new file holds: in a diff of one parent a "+" line, and in a combined diff a line none
// of whose columns is a "-", since a "-" in a column is a line of that parent the result does not
// hold. A context line would count too, though no context is asked for, and "\ No newline at end
// of file" is no line of the file at all.
func (g *Guard) addedLines(ctx context.Context, dir, commit string, parents int, emit func(Record) error) error {
	width := max(parents, 1)
	path, header := "", false
	// next is the line of the new file the hunk's next line of that file stands at.
	next := 0
	_, err := g.gitWith(ctx, dir, func(out io.Reader) error {
		return eachField(out, '\n', func(line []byte) error {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case bytes.HasPrefix(line, []byte("diff --")):
				path, header = "", true
			case header && bytes.HasPrefix(line, []byte("+++ ")):
				path = diffPath(string(line[len("+++ "):]))
			case bytes.HasPrefix(line, []byte("@@")):
				header = false
				start, err := hunkStart(string(line), width)
				if err != nil {
					return fmt.Errorf("git diff-tree %s: %w", short(commit), err)
				}
				next = start
			case header || path == "" || len(line) < width:
			case len(bytes.Trim(line[:width], " +-")) != 0:
				// "\ No newline at end of file": no line of either side.
			case bytes.Contains(line[:width], []byte("-")):
				// A line of a parent that the new file does not hold.
			default:
				if text := line[width:]; bytes.Contains(line[:width], []byte("+")) && len(bytes.TrimSpace(text)) != 0 {
					if err := emit(Record{Label: path, Text: string(text), Path: path, FirstLine: next}); err != nil {
						return err
					}
				}
				next++
			}
			return nil
		})
	}, diffTree(parents, "--unified=0", "--no-color", commit)...)
	return err
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

// hunkStart is the line of the new file a hunk header says its first line of that file stands
// at: the c of "@@ -a,b +c,d @@", and in a merge's combined diff the last range of
// "@@@ -a,b -c,d +e,f @@@", which follows one range for each of the width parents. A count of 1
// is left out of a range (git writes "+c" for "+c,1"), and a header that names no such line is
// refused rather than counted from a guess.
func hunkStart(header string, width int) (int, error) {
	fields := strings.Fields(header)
	if len(fields) > width+1 {
		if result, ok := strings.CutPrefix(fields[width+1], "+"); ok {
			first, _, _ := strings.Cut(result, ",")
			if line, err := strconv.Atoi(first); err == nil && line >= 0 {
				return line, nil
			}
		}
	}
	return 0, fmt.Errorf("the hunk header %q names no line of the new file", header)
}

// diffPath is the path a patch's "+++" line names, with the b/ prefix off and the C quoting a
// path with a byte git will not print raw undone. A deletion's /dev/null names no file.
//
// git writes a tab after the name where the path holds a space (diff.c; measured with git 2.47.3:
// `+++ b/docs/a b.md<tab>`, and `+++ "b/docs/a \"b\" c.md"<tab>` where it quotes the name too),
// and one trailing tab comes off before the name is unquoted: kept, it would end the record's
// label and Path, leave a quoted name quoted, and keep a path-scoped allowlist anchored on the
// path from clearing the file. A raw tab there is always git's and never the path's, since git
// C-quotes a tab in a path as `\t`, under core.quotePath=false too. A merge's combined diff writes
// no tab after the name (combine-diff.c, dump_quoted_path), and the trim leaves its name as it is.
func diffPath(field string) string {
	field = strings.TrimSuffix(field, "\t")
	if field == "/dev/null" {
		return ""
	}
	if unquoted, err := strconv.Unquote(field); err == nil && strings.HasPrefix(field, `"`) {
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

package leakguard

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
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
	// Remote is the remote's name, as git hands it to a pre-push hook ("origin").
	Remote string
	// URL is the remote's URL, as git hands it to a pre-push hook. The destination repository is
	// read from it (RepoFromURL).
	URL string
	// Updates is what the push would move, one entry per ref.
	Updates []Update
}

// Push checks a push before it is made. The shared check runs in its pre-push shape — the update
// lines on its standard input, the repository as its working directory — and then, for a
// destination GitHub reports public, this repository's own rules read the push's records: each
// ref name, and for each commit the destination does not already hold, the author, the
// committer, the message, each added or renamed path, and each added line.
func (g *Guard) Push(ctx context.Context, req PushRequest) error {
	what := "the push"
	if req.Remote != "" {
		what += " to " + req.Remote
	}
	check, err := g.Resolve()
	if err != nil {
		return &SetupFailure{What: what, Why: whyNoCheck, Err: err}
	}
	var lines strings.Builder
	for _, update := range req.Updates {
		lines.WriteString(update.Line())
		lines.WriteString("\n")
	}
	// bash runs the check rather than the check running itself: that is the shape git's own
	// pre-push hook calls it in, and the one the check documents for a push.
	if err := g.shared(ctx, what, child{
		argv:  []string{"bash", check, req.Remote, req.URL},
		dir:   req.Dir,
		stdin: strings.NewReader(lines.String()),
	}); err != nil {
		return err
	}
	public, err := g.scanThrough(ctx, what, RepoFromURL(req.URL))
	if err != nil {
		return err
	}
	if !public {
		return nil
	}
	records, err := g.pushRecords(ctx, req)
	if err != nil {
		return &SetupFailure{What: what, Why: "the push's commits could not be read", Err: err}
	}
	return g.rules(ctx, what, records)
}

// pushRecords is everything a push would publish, as records: one per ref name, and for each
// commit the push would add, one for the author identity, one for the committer identity, one
// for the message, one for each added or renamed path and one for each added line. A commit
// reached by two of the push's refs is read once.
func (g *Guard) pushRecords(ctx context.Context, req PushRequest) ([]Record, error) {
	var records []Record
	read := map[string]bool{}
	for _, update := range req.Updates {
		if ref := update.ref(); ref != "" {
			records = append(records, RefRecord(ref))
		}
		commits, err := g.commits(ctx, req, update)
		if err != nil {
			return nil, err
		}
		for _, commit := range commits {
			if read[commit] {
				continue
			}
			read[commit] = true
			of, err := g.commitRecords(ctx, req.Dir, commit)
			if err != nil {
				return nil, err
			}
			records = append(records, of...)
		}
	}
	return records, nil
}

// commits are the commits one update would add to the destination, oldest first. Where the
// destination already holds a commit at that ref, they are the commits the local side has that
// the remote one does not. Where the ref is new, there is no such commit to start from, so the
// destination is taken to hold what the repository's remote-tracking refs say it holds: a commit
// already on one of them has been fetched from the destination, so the push does not add it. A
// deletion adds no commit at all.
func (g *Guard) commits(ctx context.Context, req PushRequest, update Update) ([]string, error) {
	if update.LocalSHA == "" || zeroSHA(update.LocalSHA) {
		return nil, nil
	}
	argv := []string{"rev-list", "--reverse", update.LocalSHA, "--not"}
	held, err := g.holds(ctx, req.Dir, update.RemoteSHA)
	if err != nil {
		return nil, err
	}
	switch {
	case held:
		argv = append(argv, update.RemoteSHA)
	case req.Remote != "":
		argv = append(argv, "--remotes="+req.Remote)
	default:
		argv = append(argv, "--remotes")
	}
	out, err := g.git(ctx, req.Dir, argv...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// holds is whether the repository has the commit sha names: false for a ref the push creates
// (a sha of all zeros), and false for a remote sha this repository has never fetched, where the
// commits the destination holds have to be read from the remote-tracking refs instead.
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

// commitRecords is one commit as records. The identities and the message come from the commit
// itself; the added and renamed paths and the added lines come from its diff against its first
// parent, which is what a merge commit of its own adds — nothing — and what an ordinary commit
// adds.
//
// A record about a file carries that file's path as its label and the line or the path as its
// text, so that the engine reads the path as a path: the rule file's one path-scoped allowlist
// is anchored on a whole path, and a label carrying anything else would take it away.
func (g *Guard) commitRecords(ctx context.Context, dir, commit string) ([]Record, error) {
	// git writes the separator for itself, as %x00 in the format: an argument of this process's
	// own holding a zero byte is a child that cannot be started at all.
	const separator = "%x00"
	out, err := g.git(ctx, dir, "show", "--no-patch", "--no-color",
		"--format="+separator+"%an <%ae>"+separator+"%cn <%ce>"+separator+"%B"+separator, commit)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\x00")
	if len(parts) < 4 {
		return nil, fmt.Errorf("git show %s answered no identity and message", short(commit))
	}
	records := []Record{
		{Label: "commit " + short(commit) + " author", Text: parts[1]},
		{Label: "commit " + short(commit) + " committer", Text: parts[2]},
		{Label: "commit " + short(commit) + " message", Text: parts[3]},
	}
	paths, err := g.addedPaths(ctx, dir, commit)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		records = append(records, Record{Label: path, Text: path})
	}
	added, err := g.addedLines(ctx, dir, commit)
	if err != nil {
		return nil, err
	}
	return append(records, added...), nil
}

// addedPaths are the paths one commit adds or renames a file to, which are the paths the
// destination would hold that it does not hold already.
func (g *Guard) addedPaths(ctx context.Context, dir, commit string) ([]string, error) {
	out, err := g.git(ctx, dir, "diff-tree", "-r", "-M", "--no-commit-id", "--name-status", "--diff-filter=AR", "-z", commit)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(out, "\x00")
	var paths []string
	for i := 0; i < len(fields); i++ {
		switch status := fields[i]; {
		case status == "":
		case strings.HasPrefix(status, "R"):
			// A rename is three fields: the status, the path it was at, and the path it is at.
			// The destination learns the new path alone.
			if i+2 < len(fields) {
				paths = append(paths, fields[i+2])
			}
			i += 2
		default:
			if i+1 < len(fields) {
				paths = append(paths, fields[i+1])
			}
			i++
		}
	}
	return paths, nil
}

// addedLines are the lines one commit adds, each as a record of the file it is added to. The
// diff is read with no context lines, so every line of it that a patch marks as added is one.
func (g *Guard) addedLines(ctx context.Context, dir, commit string) ([]Record, error) {
	out, err := g.git(ctx, dir, "diff-tree", "-r", "-M", "--no-commit-id", "--unified=0", "--no-color", commit)
	if err != nil {
		return nil, err
	}
	var records []Record
	path := ""
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "+++ "):
			path = diffPath(strings.TrimPrefix(line, "+++ "))
		case strings.HasPrefix(line, "+") && path != "":
			if text := line[1:]; strings.TrimSpace(text) != "" {
				records = append(records, Record{Label: path, Text: text})
			}
		}
	}
	return records, nil
}

// diffPath is the path a patch's "+++" line names, with the b/ prefix off and the C quoting a
// path with a byte git will not print raw undone. A deletion's /dev/null names no file.
func diffPath(field string) string {
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

// git runs one git command in dir and answers its standard output and error together, failing
// where git itself does: a record this package could not read is a setup failure, never a push
// read as clean.
func (g *Guard) git(ctx context.Context, dir string, args ...string) (string, error) {
	argv := gitArgv(args...)
	status, printed, err := g.run(ctx, child{argv: argv, dir: dir})
	if err != nil {
		return "", err
	}
	if status != 0 {
		return "", fmt.Errorf("git %s exited %d: %s", args[0], status, strings.TrimSpace(printed))
	}
	return printed, nil
}

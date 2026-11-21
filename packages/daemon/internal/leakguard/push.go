package leakguard

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// gitPins is the configuration every git this package starts reads last: no hook and no
// file-system monitor runs, whatever the repository's own configuration names; no replacement
// object stands in for a commit, so the records read are the commits the push would send; a path
// is printed as it is, rather than with its non-ASCII bytes escaped, so that what the rules read
// is the path the destination would hold; and git show writes UTF-8, whatever
// i18n.logOutputEncoding the repository's configuration or a `git -c` on the push
// (GIT_CONFIG_PARAMETERS) names, which a command line's -c outranks: in UTF-16 each letter would
// hold a zero byte, and the fields Guard.commitRecords splits git show's answer into at zero bytes
// would be single letters, a merge's parents read as one.
var gitPins = []string{
	"--no-replace-objects",
	"-c", "core.quotePath=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "i18n.logOutputEncoding=UTF-8",
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

// deletion is whether the update deletes its ref: the local side names no commit, so it adds
// none.
func (u Update) deletion() bool {
	return namesNoCommit(u.LocalSHA)
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
	// that read the records both run in, and which a URL that is a relative local path is read
	// against (Guard.advertised).
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
//
// A push to a URL rather than to a remote, `git push https://user:token@github.com/...`, hands the
// hook that URL as the remote's name, and every refusal and setup failure of the push is printed
// for whoever reads the log, so the name is written without the user and password and the query
// a URL can carry, where a token can be (redact.URLCredentials), as a failure that names the URL
// itself is.
func PushName(remote string) string {
	if remote == "" {
		return "the push"
	}
	return "the push to " + redact.URLCredentials(remote)
}

// Push checks a push before it is made. The shared check runs in its pre-push shape — the update
// lines on its standard input, the repository as its working directory — and then, for a
// destination GitHub reports public, or one the guard cannot name, such as a local path
// (scanThrough), this repository's own private-name rules read the push's records (pushRecords):
// each ref name, for each annotated tag the destination does not already hold, the name the tag
// holds, the tagger, each other header and the message, for each commit the destination does not
// already hold, the author, the committer, each other header, such as the mergetag a merge of a
// signed tag holds, the message, each added or renamed path, and each added line, and for a blob
// or a tree a ref or a tag is at, which the destination does not already hold, each line of the
// blob, or each path of the tree and each line of its files. Under a deployment that runs no
// shared check (sharedCheck) the rules are the whole of it.
//
// The rules read no commit the destination already holds, which is a commit one of its branches
// or tags reaches as the destination itself reports them at the push, nor a tag object, a blob or
// a tree one of them is at: one git ls-remote of the URL git hands the hook, run outside the
// repository and with no git configuration but its own command line's, so that no configuration
// leads the read elsewhere, and bounded (Guard.advertised), made where an update is no deletion.
// A read that fails, or that its bound stops, refuses the push as a setup failure naming the URL.
// No ref of the repository's own says what the destination holds: git moves a remote's
// refs/remotes/<remote>/* for a push to whatever URL the remote pushes to at the time, a local
// bare one a role points it at for one run (`git remote set-url --push origin <bare>`) included,
// and a ref can be written by hand, so a commit only such a ref reaches is still one the push
// publishes.
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
	// held are the commits the destination holds and objects the tag objects, blobs and trees it
	// holds at its refs, not read at all for a push of deletions alone, which adds none of them.
	var held, objects []string
	if slices.ContainsFunc(req.Updates, func(update Update) bool { return !update.deletion() }) {
		if held, objects, err = g.advertised(ctx, req.Dir, req.URL); err != nil {
			return &SetupFailure{What: what, Why: "the destination's refs could not be read", Err: err}
		}
	}
	records, err := g.pushRecords(ctx, req, held, objects)
	if err != nil {
		return &SetupFailure{What: what, Why: "what the push adds could not be read", Err: err}
	}
	return g.rules(ctx, what, records)
}

// pushRecords is everything a push would publish, as records: one per ref name, for each tag
// object the push would add, one for its name, one for the tagger, one for each other header and
// one for the message (Guard.tagRecords), for each commit the push would add, one for the author
// identity, one for the committer identity, one for each other header (Guard.commitRecords), one
// for the message, one for each added or renamed path and one for each added line, and for each
// blob or tree the push would add, one for each line of the blob (Guard.blobRecords), or one for
// each path of the tree and each line of its files (Guard.treeRecords). The commits an update
// adds are those its local side reaches and neither its remote side nor any of held does
// (Guard.commits), held being the commits the destination holds at its branches and tags
// (Guard.advertised), and the tag objects it adds are the one its local side is at, where that is
// a tag, and each tag that tag is of, down to the first of objects, the tag objects, blobs and
// trees the destination holds at its branches and tags. The blob or the tree it adds is the one
// its local side is at, or the one the last of those tags is of, where that is not one of
// objects, and such an update is not walked for commits, since a blob or a tree reaches none. A
// commit, a tag, a blob or a tree reached by two of the push's refs is read once, and a deletion
// adds none.
//
// The commit objects, whose headers a commit's records hold, are read through one cat-file --batch
// for the whole push (blobBatch), started at the first commit read, rather than one git apiece.
// The tag objects are not: a tag is read fetching nothing (fetchesNothing), where a commit's read
// can fetch what a partial clone lacks, and one git runs under one environment.
func (g *Guard) pushRecords(ctx context.Context, req PushRequest, held, objects []string) (records []Record, err error) {
	// not is held as rev-list's negative revisions, a line ^<sha> each, written once for every
	// update's read of the commits it adds (Guard.commits).
	var not strings.Builder
	for _, sha := range held {
		not.WriteString("^")
		not.WriteString(sha)
		not.WriteString("\n")
	}
	// batch is the push's read of its commit objects, ended once the push is read or has failed.
	var batch *blobBatch
	defer func() {
		if batch != nil {
			err = batch.close(err)
		}
	}()
	// read are the commits, the tag objects, the blobs and the trees read already, and objects,
	// which are never read.
	read := map[string]bool{}
	for _, object := range objects {
		read[object] = true
	}
	for _, update := range req.Updates {
		if ref := update.ref(); ref != "" {
			records = append(records, RefRecord(ref))
		}
		if update.deletion() {
			continue
		}
		tagged, kind, object, err := g.tagRecords(ctx, req.Dir, update.LocalSHA, read)
		if err != nil {
			return nil, err
		}
		records = append(records, tagged...)
		if kind == "blob" || kind == "tree" {
			read[object] = true
			var of []Record
			if kind == "blob" {
				of, err = g.blobRecords(ctx, req.Dir, object)
			} else {
				of, err = g.treeRecords(ctx, req.Dir, object)
			}
			if err != nil {
				return nil, err
			}
			records = append(records, of...)
			continue
		}
		commits, err := g.commits(ctx, req.Dir, not.String(), update)
		if err != nil {
			return nil, err
		}
		for _, commit := range commits {
			if read[commit] {
				continue
			}
			read[commit] = true
			if batch == nil {
				if batch, err = g.readBlobs(ctx, req.Dir); err != nil {
					return nil, err
				}
			}
			of, err := g.commitRecords(ctx, req.Dir, commit, batch)
			if err != nil {
				return nil, err
			}
			records = append(records, of...)
		}
	}
	return records, nil
}

// tagRecords are the records of the tag object sha names, where it names one, and of each tag that
// tag is of in turn, until the object a tag is of is no tag: one for each header of each tag's but
// the object it is of and that object's type, which the chain is followed by, and those of its
// message, labelled as a commit's headers and message are (Guard.commitRecords): "tag <short> name"
// for the name the tag holds, and every other header read whole, its name with its value, labelled
// by its name where git itself writes a tag header of that name, such as "tag <short> tagger" for
// the tagger, and "tag <short> header" else (headerRecord, tagHeaders), a header of more than one
// line with its every line (objectHeaders). A header that holds a zero byte is read decoded too, as
// a message is (appendDecoded). The message is read as the tag object holds it, which is what git
// shows of it too, and decoded by the guard where it holds a zero byte (appendMessage).
// The object and the type followed are the first object line and the first type line, which are
// the ones git follows a tag by, and a later line of either is a header like any other. A tag
// object holds its own name, which it publishes as it publishes its message, and a chain's inner
// tag publishes a name no ref need name. A tag in read, one the destination holds or one this push
// read already, is not read, nor is the tag it is of, which the destination holds or this push
// read with it. A tag object's headers name the object it is of and that object's type, so git is
// asked the type of sha alone:
//
//	git cat-file -t <sha>
//	git cat-file tag <sha>
//
// each run in dir and fetching nothing (fetchesNothing), as the reads of what dir has are. It
// answers too the object the chain ends at, sha itself where sha names no tag, and its type, kind,
// which Guard.pushRecords reads where it is a blob or a tree; and neither where that object is in
// read, or the chain ends at a tag in read. A lightweight tag is a ref at a commit, a blob or a
// tree, whose records are its name and that object's.
func (g *Guard) tagRecords(ctx context.Context, dir, sha string, read map[string]bool) (records []Record, kind, object string, err error) {
	if read[sha] {
		return nil, "", "", nil
	}
	if kind, err = g.gitFed(ctx, dir, fetchesNothing, "", "cat-file", "-t", sha); err != nil {
		return nil, "", "", err
	}
	kind = strings.TrimSpace(kind)
	for kind == "tag" && !read[sha] {
		read[sha] = true
		out, err := g.gitFed(ctx, dir, fetchesNothing, "", "cat-file", "tag", sha)
		if err != nil {
			return nil, "", "", err
		}
		// The headers, then a blank line and the message (objectHeaders).
		headers, message, _ := strings.Cut(out, "\n\n")
		label := "tag " + short(sha)
		sha, kind = "", ""
		for name, value := range objectHeaders(headers) {
			switch {
			case name == "object" && sha == "":
				sha = value
			case name == "type" && kind == "":
				kind = value
			case name == "tag":
				records = appendDecoded(records, Record{Label: label + " name", Text: value})
			default:
				records = appendDecoded(records, headerRecord(label, name, value, tagHeaders))
			}
		}
		records = appendMessage(records, label, message, message)
	}
	if read[sha] {
		return records, "", "", nil
	}
	return records, kind, sha, nil
}

// short is an object, a commit, a tag, a blob or a tree, as a report names it.
func short(id string) string {
	return id[:min(len(id), 7)]
}

// git is gitFed with nothing of its own in git's environment and nothing on its standard input:
// it answers git's standard output alone, failing where git itself does. Each of its callers reads
// what git answers as data: git show's identities and message split on zero bytes, and diff-tree's
// list of a commit's changes. That output is the push's own content (a diff, a commit's message)
// and can be large, while git says why it failed on its standard error, so a failure quotes only
// what git wrote there, capped (gitExited, gitSaid), never its standard output.
func (g *Guard) git(ctx context.Context, dir string, args ...string) (string, error) {
	return g.gitFed(ctx, dir, nil, "", args...)
}

// gitFed runs one git command in dir under env, with input on its standard input, none where
// input is empty, and answers its standard output alone, failing where git itself does: a record
// this package could not read is a setup failure, never a push read as clean. What it answers is
// read as data — a list of commits, a list of changes split on zero bytes — so a warning git
// writes to its standard error must not land in it: a warning written in the middle of a path
// diff-tree lists would cut the path in two, and the lines added to that file would be read as
// lines of a file of another name. That error is what a failure carries (gitExited). The reads of
// what dir has run under an env that fetches nothing (Guard.advertised, Guard.tagRecords); every
// other caller passes none.
func (g *Guard) gitFed(ctx context.Context, dir string, env []string, input string, args ...string) (string, error) {
	var stdin io.Reader
	if input != "" {
		stdin = strings.NewReader(input)
	}
	var stderr strings.Builder
	status, out, err := g.run(ctx, child{
		argv:   gitArgv(args...),
		dir:    dir,
		env:    env,
		stdin:  stdin,
		stderr: &stderr,
	})
	if err != nil {
		return "", procgroup.Said(err, gitSaid(stderr.String()))
	}
	if status != 0 {
		return "", gitExited("git "+args[0], status, stderr.String())
	}
	return out, nil
}

// gitExited is the failure of a git, what ("git cat-file"), that exited status, quoting stderr,
// git's own account of the failure, as gitSaid quotes it.
func gitExited(what string, status int, stderr string) error {
	return fmt.Errorf("%s exited %d: %s", what, status, gitSaid(stderr))
}

// gitErrorBytes is the most of git's standard error a failure of a git quotes (gitExited): git says
// why it failed in a line or a few, and a failure that wrote more is cut there rather than carried
// whole into the setup failure a role reads.
const gitErrorBytes = 4096

// gitSaid is what a failure of a git quotes of the standard error git wrote (gitExited): stderr
// trimmed and, past gitErrorBytes, its last gitErrorBytes bytes, after the count of the bytes left
// out before them, cut by redact.Tail, which first takes the user and password and the query of
// each URL out of the whole of it (redact.URLCredentials). They are taken out of the whole of
// stderr before it is cut: a lazy fetch of a partial clone that fails names its promisor remote,
// and a read of a push's destination names the URL git handed the hook, either of which can carry
// a token as `user:token@` or in its query, and a cut that fell inside it would leave the token's
// end quoted where the rule no longer finds it. The end is what is kept because git writes its
// `fatal:` line last, after any warnings. The cut moves forward to the start of a character,
// looking at most utf8.UTFMax bytes ahead and staying put where none starts there, so a valid
// character is never split and an invalid run cannot take the whole quote.
func gitSaid(stderr string) string {
	said, cut := redact.Tail(strings.TrimSpace(stderr), gitErrorBytes)
	if cut == 0 {
		return said
	}
	return fmt.Sprintf("(%d bytes left out) …%s", cut, said)
}

package leakguard

import (
	"cmp"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// destinationAt is a bare repository of the test's own holding each of refspecs, which a push
// from repo by the bare's path writes there as `git push` reads one ("<commit>:refs/heads/main"):
// a push by path writes no remote-tracking ref of repo's. It answers the bare's path, the URL a
// push to it is made to. The guard reads what a push's destination holds with git ls-remote of
// that URL, run outside the repository and with no git configuration but its own command line's
// (Guard.advertised), so the URL reaches the bare by itself; it names no repository on
// github.com, so the guard scans the push without reading a visibility (Guard.scanThrough).
func destinationAt(t *testing.T, fixture fixturegit.Runner, repo string, refspecs ...string) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "destination.git")
	fixture.Run(repo, fixturegit.InitBare(bare, "main")...)
	if len(refspecs) > 0 {
		fixture.Run(repo, append([]string{"git", "push", "--quiet", bare}, refspecs...)...)
	}
	return bare
}

// commitFiles writes files into repo's working tree, each path to its content, a directory a path
// names made where it is not there, commits the working tree with message, and answers the commit.
// It is the commit of every fixture of these tests that commits what it wrote on top of HEAD, a
// merge git holds unfinished included; a fixture whose commit has parents of its own choosing
// writes its tree and runs commit-tree itself.
func commitFiles(t *testing.T, fixture fixturegit.Runner, repo string, files map[string]string, message string) string {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture.Run(repo, "git", "add", "-A")
	fixture.Run(repo, "git", "commit", "--quiet", "-m", message)
	return fixture.Run(repo, "git", "rev-parse", "HEAD")
}

// A push's records are read from the commits it would add, out of a real repository: the
// message, the author identity, each other header of the commit's, each added line of each file,
// and the ref name are each read, and a commit the destination already holds is not read at all.
// Every case here pushes one commit that names something private in one of those places, and the
// last one names nothing anywhere and is passed.
func TestAPushsRecordsAreReadFromItsCommits(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	// The fixture's own git runs under an environment that reaches no repository, remote,
	// credential or configuration but the test's (docs/testing.md).
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	base := commitFiles(t, fixture, repo, map[string]string{"README.md": "one\n"}, "the commit the destination holds")
	// The push's destination holds the base commit on its main, which it advertises: that is what
	// makes it a commit no push adds. The push is made to it by its path.
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main")
	branch := func(t *testing.T, name string) {
		t.Helper()
		fixture.Run(repo, "git", "checkout", "--quiet", "-B", name, base)
	}
	// crafted writes a commit object on base, of base's tree, whose lines after its tree and its
	// parent are headers, and whose message names nothing, and answers it: git commit writes no
	// header but its own, and git hash-object writes any a commit can hold.
	tree := fixture.Run(repo, "git", "rev-parse", base+"^{tree}")
	crafted := func(t *testing.T, headers string) string {
		t.Helper()
		return fixture.Fed(repo, "tree "+tree+"\nparent "+base+"\n"+headers+"\na message of no interest\n", "git", "hash-object", "-t", "commit", "-w", "--stdin")
	}
	const (
		authorLine    = "author builder <builder@example.invalid> 1700000000 +0000\n"
		committerLine = "committer builder <builder@example.invalid> 1700000000 +0000\n"
	)

	for _, tc := range []struct {
		name string
		// build makes the case's commit on a branch of its own and answers its sha.
		build func(t *testing.T) string
		// ref is the ref the destination would hold, "refs/heads/main" where the case leaves it
		// empty.
		ref string
		// newRef is whether the destination holds that ref already: a new one has no sha of its
		// own to read the push's commits from, so they are read from the destination's other refs.
		newRef bool
		// want is the report, as a function of the commit's sha, and nil where the push passes.
		want func(head string) []string
	}{
		{
			name: "the message names a host",
			build: func(t *testing.T) string {
				branch(t, "message")
				return commitFiles(t, fixture, repo, map[string]string{"README.md": "one\ntwo\n"}, "moved the run to "+theHost+" this morning")
			},
			want: func(head string) []string {
				return []string{"commit " + short(head) + " message: " + theClass}
			},
		},
		{
			name: "the author identity names a host",
			build: func(t *testing.T) string {
				branch(t, "author")
				author := fixture.With("GIT_AUTHOR_EMAIL=builder@" + theHost)
				return commitFiles(t, author, repo, map[string]string{"README.md": "one\nthree\n"}, "a message of no interest")
			},
			want: func(head string) []string {
				return []string{"commit " + short(head) + " author: " + theClass}
			},
		},
		{
			name: "an added line of a tracked file names a host",
			build: func(t *testing.T) string {
				branch(t, "line")
				return commitFiles(t, fixture, repo, map[string]string{"README.md": "one\nthe runner answers at " + theHost + "\n"}, "a message of no interest")
			},
			want: func(string) []string { return []string{"README.md: " + theClass} },
		},
		{
			name: "the ref name names a host",
			build: func(t *testing.T) string {
				branch(t, "ref-name")
				return commitFiles(t, fixture, repo, map[string]string{"README.md": "one\nfour\n"}, "a message of no interest")
			},
			ref:    "refs/heads/legion/" + theHost,
			newRef: true,
			want:   func(string) []string { return []string{"the ref name: " + theClass} },
		},
		{
			name: "a header of the commit's own beyond its identities names a host",
			build: func(t *testing.T) string {
				return crafted(t, authorLine+committerLine+"build-host "+theHost+"\n")
			},
			want: func(head string) []string {
				// git writes no header of that name, so its label names it as a header alone.
				return []string{"commit " + short(head) + " header: " + theClass}
			},
		},
		{
			name: "an author line the author identity is not read from names a host",
			build: func(t *testing.T) string {
				// git show formats the last author line of a commit that holds two, the clean one,
				// and git's own check of a commit reads no line after its committer.
				return crafted(t, "author builder <builder@"+theHost+"> 1700000000 +0000\n"+committerLine+authorLine)
			},
			want: func(head string) []string {
				return []string{"commit " + short(head) + " author: " + theClass}
			},
		},
		{
			name: "an author line of the read identity whose date is followed by a host",
			build: func(t *testing.T) string {
				// git show formats the last author line's name and email alone, so only the
				// header's own read holds what follows its date.
				return crafted(t, authorLine+committerLine+"author builder <builder@example.invalid> 1700000000 +0000 "+theHost+"\n")
			},
			want: func(head string) []string {
				return []string{"commit " + short(head) + " author: " + theClass}
			},
		},
		{
			name: "a tree line after the committer names a host",
			build: func(t *testing.T) string {
				// git reads a commit's tree from its first line alone, and its own check of a
				// commit reads no line after the committer, so a later tree line is a header.
				return crafted(t, authorLine+committerLine+"tree "+theHost+"\n")
			},
			want: func(head string) []string {
				return []string{"commit " + short(head) + " tree: " + theClass}
			},
		},
		{
			name: "a parent line after the committer names a host",
			build: func(t *testing.T) string {
				return crafted(t, authorLine+committerLine+"parent "+theHost+"\n")
			},
			want: func(head string) []string {
				return []string{"commit " + short(head) + " parent: " + theClass}
			},
		},
		{
			name: "a commit that names nothing",
			build: func(t *testing.T) string {
				branch(t, "clean")
				return commitFiles(t, fixture, repo, map[string]string{"docs/notes.md": "a line of ordinary prose\n"}, "a message of no interest")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := tc.build(t)
			ref, remoteSHA := tc.ref, base
			if ref == "" {
				ref = "refs/heads/main"
			}
			if tc.newRef {
				remoteSHA = noSHA
			}
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:    repo,
				Remote: "origin",
				URL:    remote,
				Updates: []Update{{
					LocalRef:  ref,
					LocalSHA:  head,
					RemoteRef: ref,
					RemoteSHA: remoteSHA,
				}},
			})
			var want []string
			if tc.want != nil {
				want = tc.want(head)
			}
			wantRefused(t, err, want)
		})
	}
}

// annotatedTag writes the annotated tag name of target in repo with message, made by tagger's
// identity, and answers the tag object. A tag of a tag is written with no advice of git's.
func annotatedTag(t *testing.T, tagger fixturegit.Runner, repo, name, target, message string) string {
	t.Helper()
	tagger.Run(repo, "git", "-c", "advice.nestedTag=false", "tag", "--annotate", "--message", message, name, target)
	return tagger.Run(repo, "git", "rev-parse", "refs/tags/"+name)
}

// An annotated tag publishes its name, its tagger, each other header it holds and its message
// with it, so a push of one reads each, as records labelled as a commit's identity, headers and
// message are, and reads the tag a tag is of in turn, whose name no ref of the push need name.
// Every tag here is of the commit the destination holds on its main, so the push adds no commit,
// and each is pushed to refs/tags/pushed, a ref the destination does not hold, which names nothing.
// A tag the destination already holds, at a ref of its own, is not read again: its message names
// a host here, and a push of it to another ref passes.
func TestAnAnnotatedTagsTaggerAndMessageAreRead(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	base := commitFiles(t, fixture, repo, map[string]string{"README.md": "one\n"}, "the commit the destination holds")
	held := annotatedTag(t, fixture, repo, "held", base, "moved the run to "+theHost+" this morning")
	destination := destinationAt(t, fixture, repo, base+":refs/heads/main", "refs/tags/held:refs/tags/held")
	// crafted writes a tag object of base named x, whose lines after its tagger are headers and
	// whose message names nothing, and answers it: git tag writes no header but its own, and git
	// hash-object writes any a tag can hold.
	crafted := func(t *testing.T, headers string) string {
		t.Helper()
		return fixture.Fed(repo, "object "+base+"\ntype commit\ntag x\ntagger builder <builder@example.invalid> 1700000000 +0000\n"+headers+"\na message of no interest\n", "git", "hash-object", "-t", "tag", "-w", "--stdin")
	}
	for _, tc := range []struct {
		name string
		// build writes the case's tags and answers the one the push is of, and the report, nil
		// where the push passes.
		build func(t *testing.T) (pushed string, want []string)
	}{
		{
			name: "the message names a host",
			build: func(t *testing.T) (string, []string) {
				pushed := annotatedTag(t, fixture, repo, "message", base, "moved the run to "+theHost+" this morning")
				return pushed, []string{"tag " + short(pushed) + " message: " + theClass}
			},
		},
		{
			name: "the tagger identity names a host",
			build: func(t *testing.T) (string, []string) {
				pushed := annotatedTag(t, fixture.With("GIT_COMMITTER_EMAIL=builder@"+theHost), repo, "tagger", base, "a message of no interest")
				return pushed, []string{"tag " + short(pushed) + " tagger: " + theClass}
			},
		},
		{
			name: "the message of the tag a tag is of names a host",
			build: func(t *testing.T) (string, []string) {
				inner := annotatedTag(t, fixture, repo, "inner", base, "moved the run to "+theHost+" this morning")
				return annotatedTag(t, fixture, repo, "outer", inner, "a message of no interest"), []string{"tag " + short(inner) + " message: " + theClass}
			},
		},
		{
			name: "the name of the tag a tag is of names a host",
			build: func(t *testing.T) (string, []string) {
				inner := annotatedTag(t, fixture, repo, "inner-"+theHost, base, "a message of no interest")
				return annotatedTag(t, fixture, repo, "named", inner, "a message of no interest"), []string{"tag " + short(inner) + " name: " + theClass}
			},
		},
		{
			name: "a header of the tag's own beyond its name and its tagger names a host",
			build: func(t *testing.T) (string, []string) {
				// git writes no tag header of that name, so its label names it as a header alone.
				pushed := crafted(t, "release-host "+theHost+"\n")
				return pushed, []string{"tag " + short(pushed) + " header: " + theClass}
			},
		},
		{
			name: "an object line after the one the tag is of names a host",
			build: func(t *testing.T) (string, []string) {
				// git follows a tag by its first object line, so a later one is a header.
				pushed := crafted(t, "object "+theHost+"\n")
				return pushed, []string{"tag " + short(pushed) + " object: " + theClass}
			},
		},
		{
			name: "a tag that names nothing",
			build: func(t *testing.T) (string, []string) {
				return annotatedTag(t, fixture, repo, "clean", base, "a message of no interest"), nil
			},
		},
		{
			name: "a tag the destination holds",
			build: func(t *testing.T) (string, []string) {
				return held, nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pushed, want := tc.build(t)
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     destination,
				Updates: []Update{{LocalRef: "refs/tags/pushed", LocalSHA: pushed, RemoteRef: "refs/tags/pushed", RemoteSHA: noSHA}},
			})
			wantRefused(t, err, want)
		})
	}
}

// A header is its name and its value, and a commit or a tag object publishes both: a crafted
// header whose name names a host, and one that is a name alone, land at a destination that runs
// git's own check of what it receives (receive.fsckObjects). So the push reads the name too, and
// refuses each, whatever label its report gives the line. The report never holds the text it
// refused (refusal).
func TestAHeaderWhoseNameNamesAHostIsRead(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	base := commitFiles(t, fixture, repo, map[string]string{"README.md": "one\n"}, "the commit the destination holds")
	destination := destinationAt(t, fixture, repo, base+":refs/heads/main")
	tree := fixture.Run(repo, "git", "rev-parse", base+"^{tree}")
	const identity = "builder <builder@example.invalid> 1700000000 +0000"
	for _, tc := range []struct {
		name   string
		kind   string
		header string
		ref    string
	}{
		{name: "a commit's header whose name names a host", kind: "commit", header: "x-" + theHost + " 1\n", ref: "refs/heads/pushed"},
		{name: "a commit's header that is a name alone naming a host", kind: "commit", header: "x-" + theHost + "\n", ref: "refs/heads/pushed"},
		{name: "a tag's header whose name names a host", kind: "tag", header: "x-" + theHost + " 1\n", ref: "refs/tags/pushed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := "tree " + tree + "\nparent " + base + "\nauthor " + identity + "\ncommitter " + identity + "\n"
			if tc.kind == "tag" {
				object = "object " + base + "\ntype commit\ntag x\ntagger " + identity + "\n"
			}
			pushed := fixture.Fed(repo, object+tc.header+"\na message of no interest\n", "git", "hash-object", "-t", tc.kind, "-w", "--stdin", "--literally")
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     destination,
				Updates: []Update{{LocalRef: tc.ref, LocalSHA: pushed, RemoteRef: tc.ref, RemoteSHA: noSHA}},
			})
			refused := refusal(t, err)
			if len(refused.Lines) == 0 {
				t.Fatalf("the refusal names no line: %v", err)
			}
			for _, line := range refused.Lines {
				if !strings.HasSuffix(line, ": "+theClass) {
					t.Errorf("the report line %q does not name the header's class, %q", line, theClass)
				}
			}
		})
	}
}

// A commit's message is published as the object holds it, whatever git show makes of it: under an
// encoding header that names UTF-16LE, git show decodes an ASCII message into other characters, so
// the host it names is in no line git show prints, while the object, which the destination keeps
// and serves to every clone, holds it as written. So the push reads the message the object holds,
// and refuses one that names a host, whatever label its report gives the line. git accepts such a
// commit, and so does a destination that checks what it receives (receive.fsckObjects). The report
// never holds the text it refused (refusal).
func TestAMessageIsReadAsTheCommitObjectHoldsIt(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	base := commitFiles(t, fixture, repo, map[string]string{"README.md": "one\n"}, "the commit the destination holds")
	destination := destinationAt(t, fixture, repo, base+":refs/heads/main")
	tree := fixture.Run(repo, "git", "rev-parse", base+"^{tree}")
	const identity = "builder <builder@example.invalid> 1700000000 +0000"
	message := "the runner answers at " + theHost + "\n"
	if len(message)%2 == 1 {
		// An even length, which UTF-16LE decodes whole, so git show prints no byte of it as
		// written.
		message = "a " + message[1:]
	}
	pushed := fixture.Fed(repo, "tree "+tree+"\nparent "+base+"\nauthor "+identity+"\ncommitter "+identity+"\nencoding UTF-16LE\n\n"+message, "git", "hash-object", "-t", "commit", "-w", "--stdin")
	if shown := fixture.Run(repo, "git", "show", "--no-patch", "--format=%B", pushed); strings.Contains(shown, theHost) {
		t.Fatalf("git show printed the message as written, so this case reads nothing git show does not: %q", shown)
	}
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	err := guard.Push(bound(t), PushRequest{
		Dir:     repo,
		Remote:  "origin",
		URL:     destination,
		Updates: []Update{{LocalRef: "refs/heads/pushed", LocalSHA: pushed, RemoteRef: "refs/heads/pushed", RemoteSHA: noSHA}},
	})
	refused := refusal(t, err)
	if len(refused.Lines) == 0 {
		t.Fatalf("the refusal names no line: %v", err)
	}
	for _, line := range refused.Lines {
		if !strings.HasSuffix(line, ": "+theClass) {
			t.Errorf("the report line %q does not name the message's class, %q", line, theClass)
		}
	}
}

// wide is text, which is ASCII, in UTF-16 (size 2) or UTF-32 (size 4), little-endian where little
// and big-endian else: each of its bytes as one code unit.
func wide(text string, size int, little bool) string {
	var out strings.Builder
	pad := strings.Repeat("\x00", size-1)
	for _, b := range []byte(text) {
		if !little {
			out.WriteString(pad)
		}
		out.WriteByte(b)
		if little {
			out.WriteString(pad)
		}
	}
	return out.String()
}

// A message in UTF-16 or UTF-32 holds a zero byte beside each ASCII letter, so the host it names is
// in no line git show prints, which ends at the first such byte, nor in any run of the object's
// bytes, while a reader that decodes the message by the encoding its object's header names reads
// the host whole. So the push decodes a commit's or a tag's message that holds a zero byte itself,
// as UTF-16 and as UTF-32 in each order, whatever the encoding header names, however it spells it
// (glibc's iconv reads "UTF 16LE" as UTF-16LE), or where there is none; a byte-order mark is no
// part of the text, and one of the other order, which glibc reads as a character under a name that
// gives an order, decides nothing; a run that decodes to no character is read as U+FFFD and the
// decode goes on; text at an offset none of those decodes is aligned on is read with the message's
// zero bytes taken out; and the push refuses one that names a host, whatever label its report
// gives the line. UTF-16 text one byte off is the other order's text aligned, which a decode
// reads; UTF-32 text two bytes off, and UTF-16 text that one zero byte parts into two alignments,
// are each read only with the zero bytes taken out. A message in UTF-16LE from its first byte is
// refused by its decode itself, not only with its zero bytes taken out. A message in either that
// names nothing is passed. The author line of a commit under such a header is read as the object
// holds it, since git show decodes the identity with the message (identityLine). Each object is
// written with --literally: git's own check of a commit, hash-object's and a destination's that
// runs receive.fsckObjects, refuses a zero byte in its body (nulInCommit), but a destination that
// runs no such check, git's default, keeps it, and the check takes such a tag. The report never
// holds the text it refused (refusal).
func TestAMessageInUTF16OrUTF32IsReadDecoded(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	base := commitFiles(t, fixture, repo, map[string]string{"README.md": "one\n"}, "the commit the destination holds")
	destination := destinationAt(t, fixture, repo, base+":refs/heads/main")
	tree := fixture.Run(repo, "git", "rev-parse", base+"^{tree}")
	const identity = "builder <builder@example.invalid> 1700000000 +0000"
	named := "the runner answers at " + theHost + "\n"
	// cut parts named between the two letters of the host's "ts" label, so that neither part holds
	// the domain a rule reads, and a message of the two parts at two alignments holds it in no
	// aligned decode.
	cut := strings.LastIndex(named, ".") - 1
	for _, tc := range []struct {
		name string
		// tag is whether the object pushed is a tag rather than a commit, author the commit's
		// author line, identity where the case leaves it empty, and encoding the value of the
		// object's encoding header, which it holds none of where the case leaves it empty.
		tag      bool
		author   string
		encoding string
		message  string
		// named is whether the push is refused, and decoded, where set, the record after
		// "commit <short> " that a report line must name: a decode, which the message with its zero
		// bytes taken out does not stand in for.
		named   bool
		decoded string
	}{
		{name: "a message in UTF-16LE", encoding: "UTF-16LE", message: wide(named, 2, true), named: true, decoded: "message decoded as UTF-16, little-endian"},
		{name: "a message in UTF-16LE after its byte-order mark", encoding: "UTF-16LE", message: "\xff\xfe" + wide(named, 2, true), named: true},
		{name: "a message in UTF-16LE after its byte-order mark under UTF-16", encoding: "UTF-16", message: "\xff\xfe" + wide(named, 2, true), named: true},
		{name: "a message in UTF-16BE with no byte-order mark under UTF-16", encoding: "UTF-16", message: wide(named, 2, false), named: true},
		{name: "a message in UTF-16LE with no byte-order mark under UTF-16", encoding: "UTF-16", message: wide(named, 2, true), named: true},
		{name: "a message in UTF-32LE with no byte-order mark under UTF-32", encoding: "UTF-32", message: wide(named, 4, true), named: true},
		{name: "a message in UTF-16LE under utf-16le", encoding: "utf-16le", message: wide(named, 2, true), named: true},
		{name: "a message in UTF-16LE under a name with a suffix iconv takes off", encoding: "UTF-16LE//IGNORE", message: wide(named, 2, true), named: true},
		{name: "a message in UTF-32LE", encoding: "UTF-32LE", message: wide(named, 4, true), named: true},
		{name: "a message in UTF-16LE with an unpaired surrogate before the host", encoding: "UTF-16LE", message: wide("a ", 2, true) + "\x00\xd8" + wide(named, 2, true), named: true},
		{name: "a tag's message in UTF-16LE", tag: true, encoding: "UTF-16LE", message: wide(named, 2, true), named: true},
		{name: "an author line under UTF-16LE", author: "builder <builder@" + theHost + "> 1700000000 +0000", encoding: "UTF-16LE", message: "a message of no interest\n", named: true},
		{name: "a message in UTF-16LE after a big-endian byte-order mark under UTF-16LE", encoding: "UTF-16LE", message: "\xfe\xff" + wide(named, 2, true), named: true},
		{name: "a message in UTF-16LE after a big-endian byte-order mark under UCS-2", encoding: "UCS-2", message: "\xfe\xff" + wide(named, 2, true), named: true},
		{name: "a message in UTF-16LE under OSF00010100", encoding: "OSF00010100", message: wide(named, 2, true), named: true},
		{name: "a message in UTF-16LE under UTF 16LE", encoding: "UTF 16LE", message: wide(named, 2, true), named: true},
		{name: "a message in UTF-16LE under no encoding header", message: wide(named, 2, true), named: true},
		{name: "a message in UTF-32BE under OSF00010104", encoding: "OSF00010104", message: wide(named, 4, false), named: true},
		{name: "a message in UTF-32LE under utf@32le", encoding: "utf@32le", message: wide(named, 4, true), named: true},
		{name: "a message in UTF-16LE after one stray byte", encoding: "UTF-16LE", message: "x" + wide(named, 2, true), named: true},
		{name: "a message in UTF-32LE after two stray bytes", encoding: "UTF-32LE", message: "xx" + wide(named, 4, true), named: true},
		{name: "a message in UTF-16LE that one zero byte parts at the host into two alignments", encoding: "UTF-16LE", message: wide(named[:cut], 2, true) + "\x00" + wide(named[cut:], 2, true), named: true},
		{name: "a message in UTF-16LE that names nothing", encoding: "UTF-16LE", message: wide("a message of no interest\n", 2, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, ref, object := "commit", "refs/heads/pushed", "tree "+tree+"\nparent "+base+"\nauthor "+cmp.Or(tc.author, identity)+"\ncommitter "+identity+"\n"
			if tc.tag {
				kind, ref, object = "tag", "refs/tags/pushed", "object "+base+"\ntype commit\ntag x\ntagger "+identity+"\n"
			}
			header := ""
			if tc.encoding != "" {
				header = "encoding " + tc.encoding + "\n"
			}
			pushed := fixture.Fed(repo, object+header+"\n"+tc.message, "git", "hash-object", "-t", kind, "-w", "--stdin", "--literally")
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     destination,
				Updates: []Update{{LocalRef: ref, LocalSHA: pushed, RemoteRef: ref, RemoteSHA: noSHA}},
			})
			if !tc.named {
				wantRefused(t, err, nil)
				return
			}
			refused := refusal(t, err)
			if len(refused.Lines) == 0 {
				t.Fatalf("the refusal names no line: %v", err)
			}
			for _, line := range refused.Lines {
				if !strings.HasSuffix(line, ": "+theClass) {
					t.Errorf("the report line %q does not name the message's class, %q", line, theClass)
				}
			}
			if want := "commit " + short(pushed) + " " + tc.decoded + ": " + theClass; tc.decoded != "" && !slices.Contains(refused.Lines, want) {
				t.Errorf("the report is %q, want a line %q", refused.Lines, want)
			}
		})
	}
}

// A header holds its value as its object does, and a crafted one can hold a zero byte, as a
// message can: a host in UTF-16 or UTF-32 is in no run of the header's bytes, while a reader that
// decodes the header reads it whole. git's own check refuses a zero byte in a header of a commit
// or a tag (nulInHeader), hash-object's and a destination's that runs receive.fsckObjects, but a
// destination that runs no such check, git's default, keeps it. So the push reads each header of a
// commit's or a tag's, the tag's own name among them, as it reads a message
// (TestAMessageInUTF16OrUTF32IsReadDecoded): decoded four ways from the header's first byte, and
// with its zero bytes taken out, and refuses one that names a host, whatever label its report
// gives the line. "x-note " is seven bytes, so UTF-16 text after it is one byte off the header's
// first byte, and after one more byte it is on it, where the decode itself refuses it; UTF-32 text
// one byte off is read only with the zero bytes taken out. A header in UTF-16LE that names nothing
// is passed. Each object is written with --literally, which skips that check. The report never
// holds the text it refused (refusal).
func TestAHeaderInUTF16OrUTF32IsReadDecoded(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	base := commitFiles(t, fixture, repo, map[string]string{"README.md": "one\n"}, "the commit the destination holds")
	destination := destinationAt(t, fixture, repo, base+":refs/heads/main")
	tree := fixture.Run(repo, "git", "rev-parse", base+"^{tree}")
	const identity = "builder <builder@example.invalid> 1700000000 +0000"
	// named is a header's value with no line break, which would end the header.
	named := "the runner answers at " + theHost
	const clean = "a note of no interest"
	for _, tc := range []struct {
		name string
		// tag is whether the object pushed is a tag rather than a commit, header a header line the
		// case adds to it, if any, and tagName the value of a tag's name header, "x" where the case
		// leaves it empty.
		tag     bool
		header  string
		tagName string
		// named is whether the push is refused, and decoded, where set, the record after
		// "commit <short> " that a report line must name, as in
		// TestAMessageInUTF16OrUTF32IsReadDecoded.
		named   bool
		decoded string
	}{
		{name: "a commit's header in UTF-16LE", header: "x-note " + wide(named, 2, true), named: true},
		{name: "a commit's header in UTF-32BE", header: "x-note " + wide(named, 4, false), named: true},
		{name: "a commit's header in UTF-16LE after one stray byte", header: "x-note x" + wide(named, 2, true), named: true, decoded: "header decoded as UTF-16, little-endian"},
		{name: "a tag's header in UTF-16LE", tag: true, header: "x-note " + wide(theHost, 2, true), named: true},
		{name: "a tag's name in UTF-16LE", tag: true, tagName: wide(theHost, 2, true), named: true},
		{name: "a commit's header in UTF-16LE that names nothing", header: "x-note " + wide(clean, 2, true)},
		{name: "a tag's header in UTF-16LE that names nothing", tag: true, header: "x-note " + wide(clean, 2, true)},
		{name: "a tag's name in UTF-16LE that names nothing", tag: true, tagName: wide(clean, 2, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, ref, object := "commit", "refs/heads/pushed", "tree "+tree+"\nparent "+base+"\nauthor "+identity+"\ncommitter "+identity+"\n"
			if tc.tag {
				kind, ref, object = "tag", "refs/tags/pushed", "object "+base+"\ntype commit\ntag "+cmp.Or(tc.tagName, "x")+"\ntagger "+identity+"\n"
			}
			if tc.header != "" {
				object += tc.header + "\n"
			}
			pushed := fixture.Fed(repo, object+"\na message of no interest\n", "git", "hash-object", "-t", kind, "-w", "--stdin", "--literally")
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     destination,
				Updates: []Update{{LocalRef: ref, LocalSHA: pushed, RemoteRef: ref, RemoteSHA: noSHA}},
			})
			if !tc.named {
				wantRefused(t, err, nil)
				return
			}
			refused := refusal(t, err)
			if len(refused.Lines) == 0 {
				t.Fatalf("the refusal names no line: %v", err)
			}
			for _, line := range refused.Lines {
				if !strings.HasSuffix(line, ": "+theClass) {
					t.Errorf("the report line %q does not name the header's class, %q", line, theClass)
				}
			}
			if want := "commit " + short(pushed) + " " + tc.decoded + ": " + theClass; tc.decoded != "" && !slices.Contains(refused.Lines, want) {
				t.Errorf("the report is %q, want a line %q", refused.Lines, want)
			}
		})
	}
}

// A message git show decodes by its commit's encoding header into other bytes, such as one in
// ISO-8859-1, is read both as the object holds it, which a reader of the destination's object
// reads, and as git show prints it, which a reader of git log reads, the second a record of its
// own. iconv decodes ISO-8859-1, so the guard decodes none of it itself. A push of such a message
// that names a host is refused, whatever label its report gives the line, and one that names
// nothing is passed. The report never holds the text it refused (refusal).
func TestAMessageGitShowsDecodedIsReadAsGitShowsItToo(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	base := commitFiles(t, fixture, repo, map[string]string{"README.md": "one\n"}, "the commit the destination holds")
	destination := destinationAt(t, fixture, repo, base+":refs/heads/main")
	tree := fixture.Run(repo, "git", "rev-parse", base+"^{tree}")
	const who = "builder <builder@example.invalid>"
	for _, tc := range []struct {
		name    string
		message string
		named   bool
	}{
		{name: "an ISO-8859-1 message that names a host", message: "caf\xe9 at " + theHost + "\n", named: true},
		{name: "an ISO-8859-1 message that names nothing", message: "caf\xe9 of no interest\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pushed := fixture.Fed(repo, "tree "+tree+"\nparent "+base+"\nauthor "+who+" 1700000000 +0000\ncommitter "+who+" 1700000000 +0000\nencoding ISO-8859-1\n\n"+tc.message, "git", "hash-object", "-t", "commit", "-w", "--stdin")
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			ctx := bound(t)
			batch, err := guard.readBlobs(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			records, err := guard.commitRecords(ctx, repo, pushed, batch)
			if err = batch.close(err); err != nil {
				t.Fatalf("read the commit: %v", err)
			}
			label := "commit " + short(pushed)
			want := []Record{
				{Label: label + " author", Text: who},
				{Label: label + " committer", Text: who},
				{Label: label + " encoding", Text: "encoding ISO-8859-1"},
				{Label: label + " message", Text: tc.message},
				{Label: label + " message as git shows it", Text: strings.ReplaceAll(tc.message, "\xe9", "\u00e9")},
			}
			if !slices.Equal(records, want) {
				t.Errorf("the records are %q, want %q", records, want)
			}
			err = guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     destination,
				Updates: []Update{{LocalRef: "refs/heads/pushed", LocalSHA: pushed, RemoteRef: "refs/heads/pushed", RemoteSHA: noSHA}},
			})
			if !tc.named {
				wantRefused(t, err, nil)
				return
			}
			refused := refusal(t, err)
			if len(refused.Lines) == 0 {
				t.Fatalf("the refusal names no line: %v", err)
			}
			for _, line := range refused.Lines {
				if !strings.HasSuffix(line, ": "+theClass) {
					t.Errorf("the report line %q does not name the message's class, %q", line, theClass)
				}
			}
		})
	}
}

// A merge of a signed tag holds that whole tag object in its mergetag header, the tag's name, its
// tagger, its message and its signature, which the merge publishes whether or not the tag itself
// is pushed, so a push of the merge reads that header as a record labelled by the merge, as its
// identities and message are. git writes the header for a signed tag alone, so each tag here is
// signed, with an ed25519 key of the test's own under gpg.format=ssh, and each merge is made onto
// main with a message that names nothing. The destination holds the base on main and the tagged
// commit on a branch of its own, so the push of main adds the merge alone, and no tag is pushed:
// what a case names reaches the push through the mergetag header alone.
func TestAMergeOfASignedTagHasItsMergetagRead(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen on this machine, which git signs a tag with under gpg.format=ssh")
	}
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	base := commitFiles(t, fixture, repo, map[string]string{"README.md": "one\n"}, "the commit the destination holds")
	fixture.Run(repo, "git", "checkout", "--quiet", "-b", "release")
	release := commitFiles(t, fixture, repo, map[string]string{"release.md": "a line of the release\n"}, "the release")
	destination := destinationAt(t, fixture, repo, base+":refs/heads/main", release+":refs/heads/release")
	key := filepath.Join(t.TempDir(), "key")
	fixture.Run(root, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "a key of the test's own", "-f", key)
	for _, tc := range []struct {
		name string
		// tag is the signed tag of the release the merge is of, made by tagger with message.
		tag, message string
		tagger       fixturegit.Runner
		// named is whether the tag names a host, which refuses the push for the merge's mergetag.
		named bool
	}{
		{
			name:    "the tagger of the tag a merge is of names a host",
			tag:     "tagger",
			message: "a message of no interest",
			tagger:  fixture.With("GIT_COMMITTER_EMAIL=builder@" + theHost),
			named:   true,
		},
		{
			name:    "the message of the tag a merge is of names a host",
			tag:     "message",
			message: "moved the run to " + theHost + " this morning",
			tagger:  fixture,
			named:   true,
		},
		{
			name:    "a merge of a signed tag that names nothing",
			tag:     "clean",
			message: "a message of no interest",
			tagger:  fixture,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.tagger.Run(repo, "git", "-c", "gpg.format=ssh", "-c", "user.signingkey="+key+".pub", "tag", "-s", "-m", tc.message, tc.tag, release)
			fixture.Run(repo, "git", "checkout", "--quiet", "-B", "main", base)
			fixture.Run(repo, "git", "merge", "--quiet", "--no-ff", "-m", "Merge the release", tc.tag)
			merge := fixture.Run(repo, "git", "rev-parse", "HEAD")
			if !strings.Contains(fixture.Run(repo, "git", "cat-file", "commit", merge), "\nmergetag object ") {
				t.Fatalf("the merge %s holds no mergetag header, so the case would prove nothing", short(merge))
			}
			var want []string
			if tc.named {
				want = []string{"commit " + short(merge) + " mergetag: " + theClass}
			}
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			wantRefused(t, guard.Push(bound(t), mainPush(repo, base, merge, "origin", destination)), want)
		})
	}
}

// A ref can be at a blob or a tree, and so can the tag a tag chain ends at, and a push of one
// publishes that content with no commit around it. A blob is read as a file a commit adds that has
// no path, each line of it a record labelled by the blob; a tree is read as a root commit's files
// are, each path it holds, which a path that names something refuses alone, and each line of each
// of its text files. Each file is read within the bound a commit's file is, 64 bytes here: a
// binary blob adds no line, and a text file above the bound refuses the push as a setup failure
// that names it. Every case pushes refs/tags/pushed, a ref the destination does not hold, which
// names nothing. A blob the destination holds, at a tag of its own, is not read again: it names a
// host here, and a push of it to another ref passes.
func TestABlobOrATreeARefOrATagIsAtIsRead(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	base := commitFiles(t, fixture, repo, map[string]string{"README.md": "one\n"}, "the commit the destination holds")
	named := "the runner answers at " + theHost + "\n"
	big := strings.Repeat("a line of ordinary prose\n", 4)
	const failed = "LEAK CHECK SETUP ERROR: the push to origin: what the push adds could not be read: "
	// blob writes content as a blob and answers it.
	blob := func(t *testing.T, content string) string {
		t.Helper()
		return fixture.Fed(repo, content, "git", "hash-object", "-w", "--stdin")
	}
	// tree writes a tree of README.md and docs/notes.md, the second holding notes, and answers it.
	tree := func(t *testing.T, notes string) string {
		t.Helper()
		docs := fixture.Fed(repo, "100644 blob "+blob(t, notes)+"\tnotes.md\n", "git", "mktree")
		return fixture.Fed(repo, "100644 blob "+blob(t, "one\n")+"\tREADME.md\n040000 tree "+docs+"\tdocs\n", "git", "mktree")
	}
	held := blob(t, "held "+named)
	destination := destinationAt(t, fixture, repo, base+":refs/heads/main", held+":refs/tags/held")
	for _, tc := range []struct {
		name string
		// build writes the case's objects and answers the one the push is at, and the report, nil
		// where the push passes.
		build func(t *testing.T) (pushed string, want []string)
		// failure is the setup failure the push is refused with, as a function of the object it is
		// at, and nil where there is none.
		failure func(pushed string) string
	}{
		{
			name: "an annotated tag of a blob that names a host",
			build: func(t *testing.T) (string, []string) {
				of := blob(t, "one\n"+named)
				return annotatedTag(t, fixture, repo, "blob-named", of, "a message of no interest"), []string{"blob " + short(of) + ": " + theClass}
			},
		},
		{
			name: "a ref at a blob that names a host",
			build: func(t *testing.T) (string, []string) {
				pushed := blob(t, named+"two\n")
				return pushed, []string{"blob " + short(pushed) + ": " + theClass}
			},
		},
		{
			name: "a ref at a tree whose file names a host",
			build: func(t *testing.T) (string, []string) {
				return tree(t, "one\n"+named), []string{"docs/notes.md: " + theClass}
			},
		},
		{
			name: "an annotated tag of a tree whose file names a host",
			build: func(t *testing.T) (string, []string) {
				return annotatedTag(t, fixture, repo, "tree-named", tree(t, named+"two\n"), "a message of no interest"), []string{"docs/notes.md: " + theClass}
			},
		},
		{
			name: "a ref at a tree whose path alone names the deployment repository",
			build: func(t *testing.T) (string, []string) {
				// The directory is spelled in two pieces for the reason theHost is, and a report
				// masks it (TestTheReportMasksAPathComponent). Its file is empty, so the file's path
				// is the one record the tree adds: a line of the file would be a record labelled by
				// that path, which the path rules read as well.
				docs := fixture.Fed(repo, "100644 blob "+blob(t, "")+"\tnotes.md\n", "git", "mktree")
				pushed := fixture.Fed(repo, "040000 tree "+docs+"\t"+"agen"+"t-c\n", "git", "mktree")
				const class = "names the private deployment repository or its Dispatch project key"
				return pushed, []string{"<name>/notes.md: " + class, "<name>/notes.md: " + class + " in its path"}
			},
		},
		{
			name: "a tag of a blob that names nothing",
			build: func(t *testing.T) (string, []string) {
				return annotatedTag(t, fixture, repo, "blob-clean", blob(t, "a line of ordinary prose\n"), "a message of no interest"), nil
			},
		},
		{
			name: "a ref at a tree that names nothing",
			build: func(t *testing.T) (string, []string) {
				return tree(t, "a line of ordinary prose\n"), nil
			},
		},
		{
			name: "a binary blob",
			build: func(t *testing.T) (string, []string) {
				return blob(t, "\x00"+named), nil
			},
		},
		{
			name: "a blob the destination holds",
			build: func(t *testing.T) (string, []string) {
				return held, nil
			},
		},
		{
			name: "a text blob above the bound",
			build: func(t *testing.T) (string, []string) {
				return blob(t, big), nil
			},
			failure: func(pushed string) string {
				return failed + "blob " + short(pushed) + " is 100 bytes, more than the 64 bytes a blob is read up to"
			},
		},
		{
			name: "a tree whose text file is above the bound",
			build: func(t *testing.T) (string, []string) {
				return tree(t, big), nil
			},
			failure: func(pushed string) string {
				return failed + "tree " + short(pushed) + "'s file docs/notes.md is 100 bytes, more than the 64 bytes a tree's file is read up to"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pushed, want := tc.build(t)
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			guard.blobBound = 64
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     destination,
				Updates: []Update{{LocalRef: "refs/tags/pushed", LocalSHA: pushed, RemoteRef: "refs/tags/pushed", RemoteSHA: noSHA}},
			})
			if tc.failure != nil {
				wantSetupFailure(t, err, tc.failure(pushed))
				return
			}
			wantRefused(t, err, want)
		})
	}
}

// pushOf is a repository whose base the destination holds on its main, the base writing the
// files held, and head, the commit the push adds, on that base, which makes edit's change to the
// working tree, where edit is not nil, and then writes the files added, each path to its content.
// Where side is not nil, head is a merge, in the shape of TestAMergesOwnResolutionIsRead: of
// main, at a commit of its own on the base, and of a branch side, whose commit on the base writes
// the files side names, with the files added written by the merge's resolution. The destination
// holds side's commit too, at a branch of its own, so that the push adds main's commit and the
// merge alone: a file side writes is read as a parent's file of the merge, and never as a file of
// a commit the push adds. It answers too the URL the push is made to, the destination's path
// (destinationAt).
func pushOf(t *testing.T, held, side, added map[string]string, edit func(t *testing.T, repo string)) (repo, base, head, destination string) {
	t.Helper()
	home, root := t.TempDir(), t.TempDir()
	repo = filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	files := map[string]string{"README.md": "one\n"}
	maps.Copy(files, held)
	base = commitFiles(t, fixture, repo, files, "the commit the destination holds")
	if side != nil {
		fixture.Run(repo, "git", "checkout", "--quiet", "-b", "side")
		files := map[string]string{"side.md": "a line of the side branch\n"}
		maps.Copy(files, side)
		commitFiles(t, fixture, repo, files, "the side commit")
		fixture.Run(repo, "git", "checkout", "--quiet", "main")
		commitFiles(t, fixture, repo, map[string]string{"main.md": "a line of the main branch\n"}, "the main commit")
		fixture.Run(repo, "git", "merge", "--quiet", "--no-ff", "--no-commit", "side")
	}
	if edit != nil {
		edit(t, repo)
	}
	head = commitFiles(t, fixture, repo, added, "the commit the push adds")
	refspecs := []string{base + ":refs/heads/main"}
	if side != nil {
		refspecs = append(refspecs, "side:refs/heads/side")
	}
	return repo, base, head, destinationAt(t, fixture, repo, refspecs...)
}

// mainPush is the push of head to main, where the destination holds base, to remote at url.
func mainPush(repo, base, head, remote, url string) PushRequest {
	return PushRequest{
		Dir:     repo,
		Remote:  remote,
		URL:     url,
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: base}},
	}
}

// What git answers is read from its standard output alone, and a warning git writes to its
// standard error is no part of it. git writes a warning at once and its output in blocks, so a
// warning can land in the middle of a path in the list of a commit's changes, and read together
// with the output, it would cut that path in two: the line the commit adds to the file would be
// read as a line of a file of another name. The stand-in git here writes the warning diff-tree
// writes of a commit with too many files to find its renames among, in the middle of the path of
// the file the commit adds a line to, and runs the real git for everything else.
func TestAWarningGitWritesIsNoPartOfWhatItAnswers(t *testing.T) {
	bin := t.TempDir()
	listed := filepath.Join(t.TempDir(), "listed")
	writeProgram(t, bin, "git", `git=`+shQuote(fixturegit.CompiledGit(t))+` listed=`+shQuote(listed)+`
case " $* " in
*" diff-tree "*)
  "$git" "$@" >"$listed" || exit
  at=$(grep -boa notes "$listed" | head -n 1 | cut -d: -f1)
  head -c "$((at + 5))" "$listed"
  echo 'warning: exhaustive rename detection was skipped due to too many files.' >&2
  tail -c "+$((at + 6))" "$listed"
  exit 0 ;;
esac
exec "$git" "$@"
`)
	repo, base, head, destination := pushOf(t, map[string]string{"notes.md": "one\n"}, nil, map[string]string{"notes.md": "one\nthe runner answers at " + theHost + "\n"}, nil)
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, pathWith(bin))
	err := guard.Push(bound(t), mainPush(repo, base, head, "origin", destination))
	wantRefused(t, err, []string{"notes.md: " + theClass})
}

// git show writes in the encoding i18n.logOutputEncoding names, which a push can set: in the
// repository's own configuration, or with a `git -c` on the push, which git hands the pre-push
// hook as GIT_CONFIG_PARAMETERS ('i18n.logOutputEncoding'='UTF-16', measured with git 2.47.3).
// In UTF-16 each letter git show writes holds a zero byte, so the fields the guard splits its
// answer into at zero bytes would be single letters (Guard.commitRecords), the parents among
// them: a merge would be read as a commit of one parent, and so would a root commit, and
// diff-tree, asked of either with no parent named, lists none of its files. The guard's git show
// writes UTF-8 whatever the push sets (gitPins), so each case's line that names a host, the one a
// merge's resolution adds and the one a root commit adds, refuses the push.
func TestAPushIsReadWhateverLogOutputEncodingItSets(t *testing.T) {
	named := map[string]string{"notes.md": "the runner answers at " + theHost + "\n"}
	for _, tc := range []struct {
		name string
		// push makes the case's repository, whose commit adds named, and answers the push of it.
		push func(t *testing.T) PushRequest
		// env is the guard's environment beyond the test's own (guardWith).
		env []string
	}{
		{
			name: "a merge's own line, from a repository whose configuration sets UTF-16",
			push: func(t *testing.T) PushRequest {
				repo, base, head, destination := pushOf(t, nil, map[string]string{}, named, nil)
				fixturegit.Isolated(t, t.TempDir()).Run(repo, "git", "config", "i18n.logOutputEncoding", "UTF-16")
				return mainPush(repo, base, head, "origin", destination)
			},
		},
		{
			name: "a root commit's line, under a git -c on the push that sets UTF-16",
			push: func(t *testing.T) PushRequest {
				orphan := func(t *testing.T, repo string) {
					fixturegit.Isolated(t, t.TempDir()).Run(repo, "git", "checkout", "--quiet", "--orphan", "orphan")
				}
				repo, _, head, destination := pushOf(t, nil, nil, named, orphan)
				return PushRequest{Dir: repo, Remote: "origin", URL: destination, Updates: []Update{{LocalRef: "refs/heads/orphan", LocalSHA: head, RemoteRef: "refs/heads/orphan", RemoteSHA: noSHA}}}
			},
			env: []string{"GIT_CONFIG_PARAMETERS='i18n.logOutputEncoding'='UTF-16'"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, tc.env...)
			wantRefused(t, guard.Push(bound(t), tc.push(t)), []string{"notes.md: " + theClass})
		})
	}
}

// ParseUpdates reads git's own pre-push lines, which is how a `legion leak-check` standing in for
// a pre-push hook reads what the push would move, and refuses a line of another shape rather than
// guessing at it.
func TestParseUpdates(t *testing.T) {
	lines := "refs/heads/main abc refs/heads/main def\n\nrefs/heads/work 123 refs/heads/work " + noSHA + "\n"
	updates, err := ParseUpdates(strings.NewReader(lines))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []Update{
		{LocalRef: "refs/heads/main", LocalSHA: "abc", RemoteRef: "refs/heads/main", RemoteSHA: "def"},
		{LocalRef: "refs/heads/work", LocalSHA: "123", RemoteRef: "refs/heads/work", RemoteSHA: noSHA},
	}
	if !slices.Equal(updates, want) {
		t.Errorf("read %v, want %v", updates, want)
	}
	if updates[0].Line() != "refs/heads/main abc refs/heads/main def" {
		t.Errorf("the line is %q", updates[0].Line())
	}
	if _, err := ParseUpdates(strings.NewReader("refs/heads/main abc\n")); err == nil {
		t.Error("a line of three fields was read as a pre-push line")
	}
}

// A git read of a push's records that fails quotes what git wrote on its standard error, never its
// standard output, which is the push's own content: a stand-in git that writes a megabyte of a
// diff's added lines to its standard output and its reason to its standard error is refused with
// the reason alone. A standard error past gitErrorBytes is cut to its end, which holds git's
// `fatal:` line, after the count of the bytes left out. Each stand-in is the one git first on PATH.
func TestAFailedGitReadQuotesItsCappedStandardErrorAlone(t *testing.T) {
	failingGit := func(t *testing.T, script string) *Guard {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script+"\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return New(Options{Env: append(fixtureenv.Isolated(t.TempDir()), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))})
	}
	const content = "+the pushed line"
	t.Run("a megabyte of the push's content on standard output", func(t *testing.T) {
		guard := failingGit(t, "yes '"+content+"' | head -c 1048576\necho 'fatal: bad object HEAD' >&2")
		_, err := guard.git(bound(t), t.TempDir(), "diff-tree", "HEAD")
		if want := "git diff-tree exited 1: fatal: bad object HEAD"; err == nil || err.Error() != want {
			t.Errorf("git = %.200v, want %q", err, want)
		}
	})
	t.Run("a standard error past the cap, its reason last", func(t *testing.T) {
		const reason = "fatal: unable to read tree"
		guard := failingGit(t, "yes 'warning: refname is ambiguous' | head -n 4000 >&2\necho '"+reason+"' >&2")
		_, err := guard.git(bound(t), t.TempDir(), "diff-tree", "HEAD")
		if err == nil {
			t.Fatal("git = nil, want its failure")
		}
		said := strings.TrimPrefix(err.Error(), "git diff-tree exited 1: ")
		count, kept, found := strings.Cut(said, " bytes left out) …")
		if !found || !strings.HasPrefix(count, "(") || len(kept) > gitErrorBytes || !strings.HasSuffix(kept, "\n"+reason) {
			t.Errorf("git quoted %d bytes, from %q to %q, want the count of the bytes left out and at most the last %d bytes of its standard error, its reason included",
				len(said), said[:min(len(said), 40)], said[max(0, len(said)-40):], gitErrorBytes)
		}
	})
	// Two-byte characters then one byte put the cut on the second byte of a character.
	t.Run("a cut inside a character moves to the next one", func(t *testing.T) {
		said := gitSaid(strings.Repeat("é", gitErrorBytes) + "x")
		if want := "(4098 bytes left out) …" + strings.Repeat("é", gitErrorBytes/2-1) + "x"; said != want {
			t.Errorf("gitSaid = %d bytes ending %q, want %d bytes ending %q: whole characters after the count",
				len(said), said[max(0, len(said)-25):], len(want), want[len(want)-25:])
		}
	})
	// Bytes that start no character still leave gitErrorBytes of them quoted, never none.
	t.Run("a run that starts no character keeps the whole cap", func(t *testing.T) {
		said := gitSaid(strings.Repeat("\x80", gitErrorBytes+10))
		if want := "(10 bytes left out) …" + strings.Repeat("\x80", gitErrorBytes); said != want {
			t.Errorf("gitSaid = %d bytes, want %d: the count of 10 and the last %d bytes", len(said), len(want), gitErrorBytes)
		}
	})
	// The cut falls where the token of a URL's userinfo would be, had the userinfo not been taken
	// out of the whole of the standard error first: a cut there leaves the token's end quoted with
	// no "://" before it for redact.URLCredentials to find.
	t.Run("a cut that would fall inside a URL's credential quotes none of it", func(t *testing.T) {
		line := "fatal: unable to access '" + credentialedURL + "': the host did not answer\n"
		at := strings.Index(line, credentialToken) + len(credentialToken)/2
		stderr := filepath.Join(t.TempDir(), "stderr")
		if err := os.WriteFile(stderr, []byte(line+strings.Repeat("w", at+gitErrorBytes-len(line))), 0o600); err != nil {
			t.Fatal(err)
		}
		guard := failingGit(t, "cat '"+stderr+"' >&2")
		_, err := guard.git(bound(t), t.TempDir(), "ls-remote", "origin")
		if err == nil {
			t.Fatal("git = nil, want its failure")
		}
		if tail := credentialToken[len(credentialToken)/2:] + "@"; strings.Contains(err.Error(), tail) {
			t.Errorf("the failure quotes the end of the URL's token, %q: %.200v", tail, err)
		}
		wantNoCredential(t, err)
	})
}

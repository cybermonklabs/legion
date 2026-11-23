package leakguard

import (
	hexpkg "encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// namedBase64 is one line of base64 that holds a /-joined name the repository rule refuses: "+" and
// "/" are base64's own characters, and to the rule the edges of a word, so a pusher can write a name
// into a signature block that looks like its bytes. It is built out of two pieces for the reason
// theHost is.
const namedBase64 = "iQEzBAABCAAdFiEE+agen" + "tc/Rk9vYmFyYmF6"

// realSignature is the body of a signature as gpg writes one: lines of base64 of bytes no one chose,
// which name nothing.
var realSignature = []string{
	"N6J2uESZoaRNSGa/6bbtyYZZwW3iRMn1K4niXPvgrZnm+GxII6rH3eBF62kpp7nZ",
	"22ROFC4a1MjTl4PjDKwFumg0gHZhvHe+M4U8IxcpR1uQniKMArF0M5lEE1d2t0n1",
	"ZTGhowIaG8ArFFfqftw1yOwQUHEnlNa5xbUeBZqeRz0BybXGGIC/EAkjUlP1LUQY",
	"UaK/iyiPQLs5HWnMh9M+CMKEgWBhKURRQc3VBcUfikaESACi/mtfJZN6SAYYH5NJ",
	"Hoq7SnKonEaSrb8UxvlAT8YRptIhEUDLi7EpNtdqUILsjIzYwihVOYzBTNCpzIXx",
	"fmWK21t+drWYaL4Zdw879ABvSMlV5t2WUMprSBTOVbjGS79MZ5z2I788",
}

// utf16le is ASCII text written as UTF-16LE, two bytes a character, as `git tag -F` takes it.
func utf16le(text string) string {
	var out strings.Builder
	for _, c := range []byte(text) {
		out.WriteByte(c)
		out.WriteByte(0)
	}
	return out.String()
}

// namedLine is a line that starts with the deployment repository's name, so that a read that took
// its first character off would no longer find the name. It is built out of two pieces for the
// reason theHost is.
const namedLine = "agen" + "t-c holds the run"

// fixtureTagger is the tagger of a tag a test writes field by field, which names nothing.
const fixtureTagger = "T <t@example.invalid>"

// signature is a signature block as git writes one at the end of a tag's message and as a
// commit's gpgsig field, with lines as its body. No key made it, and no git here checks it.
func signature(lines ...string) string {
	return "-----BEGIN PGP SIGNATURE-----\n\n" + strings.Join(lines, "\n") + "\n=Rk9v\n-----END PGP SIGNATURE-----\n"
}

// continued is value as a field of an object's header writes it: each line after the first
// starts with a space. value ends with a line break, which ends the field.
func continued(value string) string {
	return strings.ReplaceAll(strings.TrimSuffix(value, "\n"), "\n", "\n ") + "\n"
}

// stored writes raw into repo's object store as an object of kind, and answers its id: how a
// test makes a tag or a commit field by field, with a signature no key made or a field git does
// not know, which no command that makes a tag or a commit writes. git checks the object's format
// as it writes it, so each object here is one git accepts.
func stored(t *testing.T, fixture fixturegit.Runner, repo, kind, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), kind)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture.Run(repo, "git", "hash-object", "-t", kind, "-w", path)
}

// storedTag writes a tag of target named name, with tagger and message, field by field, and the
// ref refs/tags/<name> at it, and answers its id. extra are fields written after the tagger, each
// with its line break.
func storedTag(t *testing.T, fixture fixturegit.Runner, repo, name, target, tagger, message string, extra ...string) string {
	t.Helper()
	kind := fixture.Run(repo, "git", "cat-file", "-t", target)
	sha := stored(t, fixture, repo, "tag", "object "+target+"\ntype "+kind+"\ntag "+name+
		"\ntagger "+tagger+" 1700000000 +0000\n"+strings.Join(extra, "")+"\n"+message)
	fixture.Run(repo, "git", "update-ref", "refs/tags/"+name, sha)
	return sha
}

// heldRepo is a repository whose one commit the destination holds, on origin/main, and the id of
// that commit. Its message names a host, so a push that read it would be refused: what a push of a
// ref to it is refused for is what the push adds of its own.
func heldRepo(t *testing.T) (fixturegit.Runner, string, string) {
	t.Helper()
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.Run(repo, "git", "add", "-A")
	fixture.Run(repo, "git", "commit", "--quiet", "-m", "moved the run to "+theHost)
	held := fixture.Run(repo, "git", "rev-parse", "HEAD")
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main")
	return fixture, repo, held
}

// A pushed annotated tag is read: its message, its tagger and the name it was made with, which the
// push sends to the destination with the tag. rev-list follows a tag to its commit and reads
// nothing of the tag, and the commit here is one the destination holds, so each of these is the
// whole of what the push adds. A tag that points to a tag is read down to the inner one, which the
// push sends too, through the object its first field names, as git follows it: a second object
// field after the tagger is text. A tag's signature is read like the rest of its message, every
// line of it: a name written into its base64 is refused, and a real signature names nothing. A
// message written in UTF-16 is read with its NUL bytes taken out. A lightweight tag is a ref that
// holds the commit, and adds nothing; nor does a tag the destination holds at the ref.
func TestAPushedAnnotatedTagIsRead(t *testing.T) {
	fixture, repo, held := heldRepo(t)
	// tag makes an annotated tag of target named name, with message, under env, and answers it.
	tag := func(t *testing.T, name, target, message string, env ...string) string {
		t.Helper()
		fixture.With(env...).Run(repo, "git", "tag", "-a", name, "-m", message, target)
		return fixture.Run(repo, "git", "rev-parse", "refs/tags/"+name)
	}
	// madeTag is a tag of the held commit written field by field, with message as its message.
	madeTag := func(t *testing.T, name, message string) string {
		t.Helper()
		return storedTag(t, fixture, repo, name, held, fixtureTagger, message)
	}

	runPushCases(t, repo, []pushCase{
		{
			name: "the message names a host",
			build: func(t *testing.T) (Update, []string) {
				sha := tag(t, "message", held, "moved the run to "+theHost)
				return newRef("refs/tags/message", sha), []string{"tag " + short(sha) + " message: " + theClass}
			},
		},
		{
			name: "the tagger names a host",
			build: func(t *testing.T) (Update, []string) {
				sha := tag(t, "tagger", held, "a message of no interest", "GIT_COMMITTER_EMAIL=builder@"+theHost)
				return newRef("refs/tags/tagger", sha), []string{"tag " + short(sha) + " tagger: " + theClass}
			},
		},
		{
			// Pushed to a ref of another name, the tag still carries the one it was made with.
			name: "the name the tag was made with names a host",
			build: func(t *testing.T) (Update, []string) {
				sha := tag(t, theHost, held, "a message of no interest")
				update := newRef("refs/tags/"+theHost, sha)
				update.RemoteRef = "refs/tags/renamed"
				return update, []string{"tag " + short(sha) + " header: " + theClass}
			},
		},
		{
			name: "a tag of a tag whose message names a host",
			build: func(t *testing.T) (Update, []string) {
				inner := tag(t, "inner", held, "moved the run to "+theHost)
				outer := tag(t, "outer", "inner", "a message of no interest")
				if raw := fixture.Run(repo, "git", "cat-file", "tag", outer); !strings.HasPrefix(raw, "object "+inner+"\ntype tag\n") {
					t.Fatalf("the outer tag does not point to the inner one:\n%s", raw)
				}
				return newRef("refs/tags/outer", outer), []string{"tag " + short(inner) + " message: " + theClass}
			},
		},
		{
			// git follows the first field, so the push sends the inner tag; a guard that followed
			// the second would read the held commit in its place and never the inner tag.
			name: "a tag of a tag with a second object field that names the held commit",
			build: func(t *testing.T) (Update, []string) {
				inner := tag(t, "inner-two", held, "moved the run to "+theHost)
				outer := storedTag(t, fixture, repo, "outer-two", inner, fixtureTagger, "a message of no interest\n",
					"object "+held+"\n")
				if peeled := fixture.Run(repo, "git", "rev-parse", outer+"^{tag}", "refs/tags/outer-two^{}"); peeled != outer+"\n"+held {
					t.Fatalf("git reads the outer tag as %q, want it and, peeled, the held commit through the inner tag", peeled)
				}
				return newRef("refs/tags/outer-two", outer), []string{"tag " + short(inner) + " message: " + theClass}
			},
		},
		{
			name: "a /-joined repository name inside the signature's base64",
			build: func(t *testing.T) (Update, []string) {
				sha := madeTag(t, "signed", "a message of no interest\n"+signature(namedBase64))
				return newRef("refs/tags/signed", sha), []string{"tag " + short(sha) + " message: " + repositoryClass}
			},
		},
		{
			name: "a real signature",
			build: func(t *testing.T) (Update, []string) {
				return newRef("refs/tags/real-signed", madeTag(t, "real-signed", "a message of no interest\n"+signature(realSignature...))), nil
			},
		},
		{
			name: "a message in UTF-16 names a host",
			build: func(t *testing.T) (Update, []string) {
				sha := madeTag(t, "utf16", utf16le("moved the run to "+theHost+"\n"))
				return newRef("refs/tags/utf16", sha), []string{"tag " + short(sha) + " message: " + theClass}
			},
		},
		{
			name: "the same bytes in the message",
			build: func(t *testing.T) (Update, []string) {
				sha := madeTag(t, "unsigned", "a message of no interest\n"+namedBase64+"\n")
				return newRef("refs/tags/unsigned", sha), []string{"tag " + short(sha) + " message: " + repositoryClass}
			},
		},
		{
			// git's signature is the last block alone; an armoured block before it is message.
			name: "the same bytes in an armoured block before the signature",
			build: func(t *testing.T) (Update, []string) {
				sha := madeTag(t, "armoured", "a message of no interest\n-----BEGIN PGP MESSAGE-----\n\n"+namedBase64+
					"\n-----END PGP MESSAGE-----\n"+signature("Rk9vYmFy"))
				return newRef("refs/tags/armoured", sha), []string{"tag " + short(sha) + " message: " + repositoryClass}
			},
		},
		{
			// git counts a line after the signature's end in the signature, and git show prints
			// it. Built out of two pieces for the reason theHost is.
			name: "a line after the signature's end, of base64's characters, names the repository",
			build: func(t *testing.T) (Update, []string) {
				sha := madeTag(t, "after-end", "a message of no interest\n"+signature("Rk9vYmFy")+"notes/agen"+"tc/today\n")
				return newRef("refs/tags/after-end", sha), []string{"tag " + short(sha) + " message: " + repositoryClass}
			},
		},
		{
			// A second END line after the signature's own leaves the lines between the two read:
			// the signature ends at the first. Built out of two pieces for the reason theHost is.
			name: "a line before a second END line after the signature names the repository",
			build: func(t *testing.T) (Update, []string) {
				sha := madeTag(t, "second-end", "a message of no interest\n"+signature("Rk9vYmFy")+"notes/agen"+"tc/today\n-----END X\n")
				return newRef("refs/tags/second-end", sha), []string{"tag " + short(sha) + " message: " + repositoryClass}
			},
		},
		{
			name: "the signature's END line with text after it names a host",
			build: func(t *testing.T) (Update, []string) {
				sha := madeTag(t, "end-text", "a message of no interest\n-----BEGIN PGP SIGNATURE-----\n\nRk9vYmFy\n-----END moved the run to "+theHost+"\n")
				return newRef("refs/tags/end-text", sha), []string{"tag " + short(sha) + " message: " + theClass}
			},
		},
		{
			name: "the signature's start line with text after it names a host",
			build: func(t *testing.T) (Update, []string) {
				sha := madeTag(t, "begin-text", "a message of no interest\n-----BEGIN PGP SIGNATURE----- moved the run to "+theHost+"\n\nRk9vYmFy\n-----END PGP SIGNATURE-----\n")
				return newRef("refs/tags/begin-text", sha), []string{"tag " + short(sha) + " message: " + theClass}
			},
		},
		{
			// A capitalised name is all an END line's own shape allows, so only the END line that
			// pairs with the signature's start is skipped. Built out of two pieces for the reason
			// theHost is.
			name: "the signature's END line names the company in capitals",
			build: func(t *testing.T) (Update, []string) {
				sha := madeTag(t, "end-caps", "a message of no interest\n-----BEGIN PGP SIGNATURE-----\n\nRk9vYmFy\n-----END TRAJEC"+"TORY-----\n")
				return newRef("refs/tags/end-caps", sha), []string{"tag " + short(sha) + " message: names the company"}
			},
		},
		{
			// The destination holds the tag at that ref already, so the push moves nothing.
			name: "a tag the destination holds at that ref",
			build: func(t *testing.T) (Update, []string) {
				sha := tag(t, "held", held, "moved the run to "+theHost)
				return Update{LocalRef: "refs/tags/held", LocalSHA: sha, RemoteRef: "refs/tags/held", RemoteSHA: sha}, nil
			},
		},
		{
			name: "a lightweight tag",
			build: func(t *testing.T) (Update, []string) {
				fixture.Run(repo, "git", "tag", "light", held)
				return newRef("refs/tags/light", held), nil
			},
		},
		{
			// A chain of tags that ends at a commit peels to it, so rev-list's ^{commit} takes it.
			name: "a tag of a tag that names nothing",
			build: func(t *testing.T) (Update, []string) {
				tag(t, "clean-inner", held, "a message of no interest")
				return newRef("refs/tags/clean-outer", tag(t, "clean-outer", "clean-inner", "a message of no interest")), nil
			},
		},
		{
			name: "an annotated tag that names nothing",
			build: func(t *testing.T) (Update, []string) {
				return newRef("refs/tags/clean", tag(t, "clean", held, "a message of no interest")), nil
			},
		},
	})
}

// A commit's whole header is read by git's rules, not only its identities and message. A merge of a
// signed tag carries that tag in its header (mergetag), its tagger and message included, and the
// push sends the commit alone: the tag is read there as a pushed tag is. A field git does not know
// is read, and so is a field of git's own names out of git's place, which git reads no structure
// from: a second committer is read whole, and a parent after the committer is no parent, so the
// commit's diff is read in the shape git's own parents call for. A merge's parents are git's
// whatever the case of their hex. A message stored in another encoding than UTF-8 is read as git
// shows it, whichever encoding field comes first. A signature is read like any text, a commit's
// (gpgsig) and a merged tag's alike: a name written into its base64 is refused, as are the same
// bytes in a field git does not know, and a real signature names nothing. Each commit here adds no
// line of a file unless its case says so, so its header and message are the whole of what it adds.
func TestACommitsWholeHeaderIsRead(t *testing.T) {
	fixture, repo, held := heldRepo(t)
	heldTree := fixture.Run(repo, "git", "rev-parse", held+"^{tree}")
	onMain := func(head string) Update {
		return Update{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: held}
	}
	// committed is a commit of name.md, holding content, on a branch of name from base.
	committed := func(t *testing.T, name, base, content string) string {
		t.Helper()
		fixture.Run(repo, "git", "checkout", "--quiet", "-B", name, base)
		if err := os.WriteFile(filepath.Join(repo, name+".md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		fixture.Run(repo, "git", "add", "-A")
		fixture.Run(repo, "git", "commit", "--quiet", "-m", "a commit of no interest")
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	// merged is a merge onto the held commit of a side commit through a signed tag of it, made
	// field by field with tagger and message, as `git merge` of a signed tag writes it.
	merged := func(t *testing.T, name, tagger, message string) string {
		t.Helper()
		side := committed(t, "side-"+name, held, "a line of the side branch\n")
		storedTag(t, fixture, repo, name, side, tagger, message)
		fixture.Run(repo, "git", "checkout", "--quiet", "-B", "merge-"+name, held)
		fixture.Run(repo, "git", "merge", "--quiet", "--no-ff", "-m", "a merge of no interest", name)
		merge := fixture.Run(repo, "git", "rev-parse", "HEAD")
		if raw := fixture.Run(repo, "git", "cat-file", "commit", merge); !strings.Contains(raw, "\nmergetag object "+side+"\n") {
			t.Fatalf("git merge wrote no mergetag field into %s:\n%s", merge, raw)
		}
		return merge
	}
	// madeOn is a commit of tree on top of the held commit, written field by field with fields
	// after its committer and message after them; made is one of the held commit's tree and a
	// message of no interest.
	madeOn := func(t *testing.T, tree, fields, message string) string {
		t.Helper()
		return stored(t, fixture, repo, "commit", "tree "+tree+"\nparent "+held+
			"\nauthor A <a@example.invalid> 1700000000 +0000\ncommitter C <c@example.invalid> 1700000000 +0000\n"+
			fields+"\n"+message)
	}
	made := func(t *testing.T, fields string) string {
		t.Helper()
		return madeOn(t, heldTree, fields, "a message of no interest\n")
	}
	// refused is a case's update of head and its one line of report, about head's record.
	refused := func(head, record, class string) (Update, []string) {
		return onMain(head), []string{"commit " + short(head) + " " + record + ": " + class}
	}

	runPushCases(t, repo, []pushCase{
		{
			name: "a merged tag's message names a host",
			build: func(t *testing.T) (Update, []string) {
				head := merged(t, "message", fixtureTagger, "moved the run to "+theHost+"\n"+signature("Rk9vYmFy"))
				return refused(head, "mergetag message", theClass)
			},
		},
		{
			name: "a merged tag's tagger names a host",
			build: func(t *testing.T) (Update, []string) {
				head := merged(t, "tagger", "T <t@"+theHost+">", "a message of no interest\n"+signature("Rk9vYmFy"))
				return refused(head, "mergetag tagger", theClass)
			},
		},
		{
			name: "a /-joined repository name inside a merged tag's signature",
			build: func(t *testing.T) (Update, []string) {
				head := merged(t, "signed", fixtureTagger, "a message of no interest\n"+signature(namedBase64))
				return refused(head, "mergetag message", repositoryClass)
			},
		},
		{
			name: "a merged tag's real signature",
			build: func(t *testing.T) (Update, []string) {
				return onMain(merged(t, "real-signed", fixtureTagger, "a message of no interest\n"+signature(realSignature...))), nil
			},
		},
		{
			name: "a field git does not know names a host",
			build: func(t *testing.T) (Update, []string) {
				return refused(made(t, "x-note the run moved to "+theHost+"\n"), "header", theClass)
			},
		},
		{
			// git reads the first committer alone; this one is a field of text after it.
			name: "a second committer with text after its address",
			build: func(t *testing.T) (Update, []string) {
				return refused(made(t, "committer D <d@example.invalid> moved the run to "+theHost+"\n"), "header", theClass)
			},
		},
		{
			// git reads the parents straight after the tree alone: this commit has one parent to
			// git, and its added line is read in full, its first character included.
			name: "a parent field after the committer",
			build: func(t *testing.T) (Update, []string) {
				tree := fixture.Run(repo, "git", "rev-parse", committed(t, "extra-parent", held, namedLine+"\n")+"^{tree}")
				return onMain(madeOn(t, tree, "parent "+held+"\n", "a message of no interest\n")),
					[]string{"extra-parent.md: " + repositoryClass}
			},
		},
		{
			// git reads hex of either case as a parent, so this is a merge, and its resolution's
			// own line is in its combined diff alone.
			name: "a merge whose second parent is written in capitals",
			build: func(t *testing.T) (Update, []string) {
				side := committed(t, "upper-side", held, "a line of the side branch\n")
				committed(t, "upper-main", held, "a line of the main branch\n")
				fixture.Run(repo, "git", "merge", "--quiet", "--no-ff", "--no-commit", "upper-side")
				if err := os.WriteFile(filepath.Join(repo, "upper-resolution.md"), []byte("the runner answers at "+theHost+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				fixture.Run(repo, "git", "add", "-A")
				fixture.Run(repo, "git", "commit", "--quiet", "-m", "a merge of no interest")
				raw := fixture.Out(repo, "git", "cat-file", "commit", "HEAD")
				upper := strings.Replace(raw, "\nparent "+side+"\n", "\nparent "+strings.ToUpper(side)+"\n", 1)
				if upper == raw {
					t.Fatalf("the merge names no second parent %s:\n%s", side, raw)
				}
				head := stored(t, fixture, repo, "commit", upper)
				if parents := fixture.Run(repo, "git", "rev-list", "--parents", "-n", "1", head); len(strings.Fields(parents)) != 3 {
					t.Fatalf("git reads %q, want a merge of two parents", parents)
				}
				return onMain(head), []string{"upper-resolution.md: " + theClass}
			},
		},
		{
			name: "a /-joined repository name inside a commit's signature",
			build: func(t *testing.T) (Update, []string) {
				return refused(made(t, "gpgsig "+continued(signature(namedBase64))), "header", repositoryClass)
			},
		},
		{
			name: "a commit's real signature",
			build: func(t *testing.T) (Update, []string) {
				return onMain(made(t, "gpgsig "+continued(signature(realSignature...)))), nil
			},
		},
		{
			name: "a commit's gpgsig END line with text after it names a host",
			build: func(t *testing.T) (Update, []string) {
				head := made(t, "gpgsig "+continued("-----BEGIN PGP SIGNATURE-----\n\nRk9vYmFy\n-----END moved the run to "+theHost+"\n"))
				return refused(head, "header", theClass)
			},
		},
		{
			name: "a commit's gpgsig start line with text after it names a host",
			build: func(t *testing.T) (Update, []string) {
				head := made(t, "gpgsig "+continued("-----BEGIN PGP SIGNATURE----- moved the run to "+theHost+"\n\nRk9vYmFy\n-----END PGP SIGNATURE-----\n"))
				return refused(head, "header", theClass)
			},
		},
		{
			name: "the same bytes in a field git does not know",
			build: func(t *testing.T) (Update, []string) {
				return refused(made(t, "x-note "+namedBase64+"\n"), "header", repositoryClass)
			},
		},
		{
			name: "a line of a signature block that is not base64",
			build: func(t *testing.T) (Update, []string) {
				head := made(t, "gpgsig "+continued(signature("Comment: the run moved to "+theHost, "Rk9vYmFy")))
				return refused(head, "header", theClass)
			},
		},
		{
			// The no-break space is Latin-1's byte, which is no character of UTF-8: the stored
			// bytes name the repository to the rules read as Windows-1252, and the text git shows
			// does as UTF-8. Built out of two pieces for the reason theHost is.
			name: "a message in another encoding names the repository",
			build: func(t *testing.T) (Update, []string) {
				head := madeOn(t, heldTree, "encoding ISO-8859-1\n", "the run moved to agen"+"t\xa0c today\n")
				return onMain(head), []string{"commit " + short(head) + " message: " + repositoryClass, "commit " + short(head) + " message in UTF-8: " + repositoryClass}
			},
		},
		{
			// An encoding git cannot convert from leaves the stored bytes, a Latin-1 byte among
			// them, as what git shows too: the name on the same line is ASCII, and read.
			name: "a message in an encoding git cannot convert, a name beside a byte that is not UTF-8",
			build: func(t *testing.T) (Update, []string) {
				head := madeOn(t, heldTree, "encoding X-NO-SUCH\n", "the run moved to "+theHost+" \xe9t\xe9\n")
				return onMain(head), []string{"commit " + short(head) + " message: " + theClass, "commit " + short(head) + " message in UTF-8: " + theClass}
			},
		},
		{
			name: "a field of a commit in an encoding git cannot convert, a name beside a byte that is not UTF-8",
			build: func(t *testing.T) (Update, []string) {
				return refused(made(t, "encoding X-NO-SUCH\nx-note the run moved to "+theHost+" \xe9t\xe9\n"), "header", theClass)
			},
		},
		{
			// The rules know a no-break space as a separator; Latin-1 writes it as one byte, which
			// read as UTF-8 is no character, so the text is read as Windows-1252 too. Built out of
			// two pieces for the reason theHost is.
			name: "a message in an encoding git cannot convert, a name joined by a Latin-1 no-break space",
			build: func(t *testing.T) (Update, []string) {
				head := madeOn(t, heldTree, "encoding X-NO-SUCH\n", "the run moved to agen"+"t\xa0c today\n")
				return onMain(head), []string{"commit " + short(head) + " message: " + repositoryClass, "commit " + short(head) + " message in UTF-8: " + repositoryClass}
			},
		},
		{
			// Windows-1252 writes an en dash, a separator the rules know, as the one byte 0x96.
			name: "an added line of a Windows-1252 file, a name joined by an en dash",
			build: func(t *testing.T) (Update, []string) {
				return onMain(committed(t, "cp1252-line", held, "the run moved to agen"+"t\x96c today\n")), []string{"cp1252-line.md: " + repositoryClass}
			},
		},
		{
			name: "an added line that names a host beside a byte that is not UTF-8",
			build: func(t *testing.T) (Update, []string) {
				return onMain(committed(t, "latin1-line", held, "the runner answers at "+theHost+" \xe9t\xe9\n")), []string{"latin1-line.md: " + theClass}
			},
		},
		{
			// git converts from the first encoding field; a second that names UTF-8 changes
			// nothing of what git shows.
			name: "a message in another encoding, with a second encoding field that names UTF-8",
			build: func(t *testing.T) (Update, []string) {
				head := madeOn(t, heldTree, "encoding ISO-8859-1\nencoding UTF-8\n", "the run moved to agen"+"t\xa0c today\n")
				return onMain(head), []string{"commit " + short(head) + " message: " + repositoryClass, "commit " + short(head) + " message in UTF-8: " + repositoryClass}
			},
		},
	})
}

// A ref can hold a blob or a tree, or a tag of one, and git pushes that object, with text the
// rules would refuse: no commit's read reaches it. Each such push is refused as a setup failure in
// git's own words, which name the object asked for and no path, and never the object's text.
func TestAPushOfAnObjectThatIsNotACommitIsRefused(t *testing.T) {
	fixture, repo, _ := heldRepo(t)
	path := filepath.Join(t.TempDir(), "note")
	if err := os.WriteFile(path, []byte("the runner answers at "+theHost+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := fixture.Run(repo, "git", "hash-object", "-w", path)
	tree := stored(t, fixture, repo, "tree", "100644 note\x00"+string(mustHex(t, blob)))
	for _, tc := range []struct {
		name string
		// build answers the update that pushes the case's object, and the type it peels to.
		build func(t *testing.T) (Update, string)
	}{
		{"a lightweight tag of a blob", func(t *testing.T) (Update, string) {
			return newRef("refs/tags/raw-blob", blob), "blob"
		}},
		{"a lightweight tag of a tree", func(t *testing.T) (Update, string) {
			return newRef("refs/tags/raw-tree", tree), "tree"
		}},
		{"an annotated tag of a blob", func(t *testing.T) (Update, string) {
			return newRef("refs/tags/blob-tag", storedTag(t, fixture, repo, "blob-tag", blob, fixtureTagger, "a message of no interest\n")), "blob"
		}},
		{"a tag of a tag of a blob", func(t *testing.T) (Update, string) {
			inner := storedTag(t, fixture, repo, "blob-inner", blob, fixtureTagger, "a message of no interest\n")
			return newRef("refs/tags/blob-outer", storedTag(t, fixture, repo, "blob-outer", inner, fixtureTagger, "a message of no interest\n")), "blob"
		}},
		{"a branch that names a blob", func(t *testing.T) (Update, string) {
			return newRef("refs/heads/raw-blob", blob), "blob"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			update, kind := tc.build(t)
			failed := setupFailure(t, pushOf(t, guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public), repo, update))
			says(t, failed, update.LocalSHA+"^{commit}: expected commit type, but the object dereferences to "+kind+" type")
			if strings.Contains(failed.Error(), theHost) {
				t.Errorf("the setup failure %q quotes the object's text", failed.Error())
			}
		})
	}
}

// mustHex is the object id hex as the bytes a tree entry holds it in.
func mustHex(t *testing.T, hex string) []byte {
	t.Helper()
	id, err := hexpkg.DecodeString(hex)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A field of many lines is read in one pass over it: a merged tag of 40,000 lines, 2 MB, is read
// with an allocation of a few times its size, where a value built a line at a time would copy
// itself again for each line, which for this one is tens of gigabytes and, measured, 13 s under
// load. The allocation, unlike the time, does not depend on the machine's load. The message comes
// back whole, its lines as they were.
func TestAFieldOfManyLinesIsReadInOnePass(t *testing.T) {
	const lines = 40_000
	var message strings.Builder
	for i := range lines {
		fmt.Fprintf(&message, "line %d of a long tag message of nothing in particular\n", i)
	}
	tag := "object " + strings.Repeat("a", 40) + "\ntype commit\ntag long\ntagger " + fixtureTagger +
		" 1700000000 +0000\n\n" + message.String()
	raw := "tree " + strings.Repeat("b", 40) + "\nauthor A <a@example.invalid> 1700000000 +0000\n" +
		"committer C <c@example.invalid> 1700000000 +0000\nmergetag " + continued(tag) + "\na merge of no interest\n"
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	started := time.Now()
	var read string
	_, err := objectRecords("commit x", raw, false, func(r Record) error {
		if r.Label == "commit x mergetag message" {
			read = r.Text
		}
		return nil
	})
	took := time.Since(started)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("a merged tag of %d lines, %d bytes, read in %s with %d bytes allocated", lines, len(raw), took, allocated)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(message.String(), "\n"); read != want {
		t.Fatalf("the merged tag's message is %d bytes, want its %d bytes as they were", len(read), len(want))
	}
	if limit := 8 * uint64(len(raw)); allocated > limit {
		t.Errorf("reading %d bytes allocated %d bytes, want at most %d: a field read once costs a few copies of it", len(raw), allocated, limit)
	}
}

// A tag that could not be read refuses the push, as every read of a push's records that fails
// does: a tag read as clean because git did not answer would pass what it carries. The stand-in
// git fails the one read and runs the real git for every other command.
func TestAFailedTagReadRefusesThePush(t *testing.T) {
	git := fixturegit.CompiledGit(t)
	fixture, repo, held := heldRepo(t)
	fixture.Run(repo, "git", "tag", "-a", "clean", "-m", "a message of no interest", held)
	sha := fixture.Run(repo, "git", "rev-parse", "refs/tags/clean")
	for _, read := range []string{"cat-file -t", "cat-file tag"} {
		t.Run(read, func(t *testing.T) {
			guard := gitFirst(t, "case \" $* \" in\n"+
				"*\" "+read+" \"*) echo 'fatal: the stand-in fails this read' >&2; exit 128 ;;\n"+
				"*) exec '"+git+"' \"$@\" ;;\nesac\n")
			err := pushOf(t, guard, repo, newRef("refs/tags/clean", sha))
			says(t, setupFailure(t, err), "the push's commits and tags could not be read: git cat-file exited 128: fatal: the stand-in fails this read")
		})
	}
}

package leakguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
)

// GhRequest is one gh call, as the caller is about to make it.
type GhRequest struct {
	// Args is the whole argv the caller would hand gh, gh's own name aside.
	Args []string
	// Stdin is what the caller would hand gh on its standard input, nil for nothing. It is read
	// only where the shared check asks for it, and what is read comes back from GhCall for the
	// caller to hand gh in its place.
	Stdin io.Reader
	// Repo is the destination repository, as "owner/name". Empty where the caller knows no
	// destination, which the guard reads as public (Guard.scanThrough).
	Repo string
}

// whatGhCall is what a refusal and a setup failure of this shape call the write. Both halves of
// the check — the first call and the one that follows it with the buffer — report the one call.
const whatGhCall = "the gh call"

// GhCall checks a gh call before it is made, in the shared check's gh-guard shape, and answers
// the standard input it read.
//
// The check reads a call that takes its body on standard input in two goes: the first call exits
// 4, meaning "this call reads standard input; buffer it and ask me again", and the second call is
// handed the buffer. The buffer is a regular file, never a pipe: the check reads it twice, and
// the check itself refuses a pipe for that reason. The bytes come back so the caller can hand
// them to gh, because by then this package has read the caller's standard input and the caller
// cannot read it again; a nil answer means standard input was never read and the caller's own is
// still whole.
func (g *Guard) GhCall(ctx context.Context, req GhRequest) ([]byte, error) {
	check, err := g.Resolve()
	if err != nil {
		return nil, &SetupFailure{What: whatGhCall, Why: whyNoCheck, Err: err}
	}
	argv := append([]string{check, "gh-guard"}, req.Args...)
	status, printed, err := g.run(ctx, child{argv: argv})
	if err != nil {
		return nil, &SetupFailure{What: whatGhCall, Why: "the shared public leak check could not be run", Err: err}
	}
	var buffered []byte
	if status == checkWantsStdin {
		buffered, status, printed, err = g.askAgain(ctx, argv, req)
		if err != nil {
			return buffered, err
		}
	}
	if err := readStatus(whatGhCall, status, printed); err != nil {
		return buffered, err
	}
	public, err := g.scanThrough(ctx, whatGhCall, req.Repo)
	if err != nil {
		return buffered, err
	}
	if !public {
		return buffered, nil
	}
	records := []Record{{Label: "the gh call's arguments", Text: strings.Join(req.Args, " ")}}
	if len(buffered) > 0 {
		records = append(records, Record{Label: "the gh call's standard input", Text: string(buffered)})
	}
	// The text of each file the call would send, read here and not before the visibility answer:
	// a private or internal destination runs no rule pass, and reading its files would cost the
	// opens for nothing.
	for _, named := range ghFileArgs(req.Args) {
		text, err := readGhFile(named)
		if err != nil {
			return buffered, err
		}
		records = append(records, Record{Label: named.path, Text: text})
	}
	return buffered, g.rules(ctx, whatGhCall, records)
}

// maxGhFile is the most this package reads from one file a gh call would send. GitHub itself
// refuses a pull request or issue body longer than 65,536 characters, and a body is the largest
// thing these options carry: `gh api --input` wraps one in a JSON request, which that escaping
// and the rest of the request leave well inside a mebibyte. A mebibyte is sixteen times that
// body, so no call GitHub would take meets the bound, and a file past it is one an argument
// names by mistake. Such a file is a setup failure rather than a pass — the call is refused
// either way — and the rule pass never reads a gigabyte into memory to run ten regexes over.
const maxGhFile = 1 << 20

// ghFile is one file a gh call would send as content: the option that named it, which a setup
// failure names, and the path the argv wrote, which is the record's label.
type ghFile struct {
	option, path string
}

// ghFileOptions are the options whose whole value is the path of a file gh sends to GitHub as
// content, in gh 2.98.0's spelling — the version the worker image pins, whose help is
// cmd/legion/testdata/gh-help, which is where each of these was read.
var ghFileOptions = map[string]bool{
	// The body of a pull request, an issue, a comment, a review or a revert. Twelve commands of
	// the pinned reference carry it: `awk '/^### /{cmd=$0} /--body-file/{print cmd}'
	// testdata/gh-help/reference.txt | sort -u`.
	"--body-file": true,
	// `gh release create` and `gh release edit`'s release notes.
	"--notes-file": true,
	// `gh api`'s HTTP request body.
	"--input": true,
	// `gh agent-task create`'s task description.
	"--from-file": true,
}

// ghFileArgs are the files args would have gh send to GitHub as content, each named once and in
// the order the argv writes them. A gh call carries its text in a file as readily as in an
// argument — `gh api --method PATCH repos/<owner>/<repo>/pulls/<n> -F body=@body.md` is how a
// Legion role writes a pull request's body — so each file's text is a record of its own beside
// the record of the argv, which holds the file's name and never its text.
//
// What is read is ghFileOptions, each in its separate and its attached spelling
// (`--body-file=<path>`), and two more shapes of one short option:
//
//   - `-F <value>`: gh gives -F to --body-file, --notes-file, --from-file and --field, and to
//     nothing else (`grep -ho -- '-F, --[a-z-]*' testdata/gh-help/*.txt | sort -u`), so a -F is
//     read by its value rather than its name. A value holding no `=` is one of the three file
//     options' (`-F body.md`); a value of the shape `name=@<path>` is --field's file; any other
//     `name=value` is a literal field and names no file.
//   - `--field <name>=@<path>`: `gh api`'s field read from a file. A --field whose value holds
//     no `=` names no file — `gh project field-list --field <name>` names a field of a project —
//     so only the `@` shape is read.
//
// The attached short spelling `-F<value>` is read as well, and it is the only short cluster
// read: a `-F` anywhere but the front of a short argument is the value of the option before it
// (`-dF` is `--desc F`), and reading that as a file would refuse a call over a file nobody
// named.
//
// This reads the argv with no gh to tell it which other options take a value, so an option's
// value that itself begins with `-F` (a `--body` whose first word is `-Fixed`) is read as a -F
// of its own and refuses the call, naming a file nobody meant. That is the direction this errs
// in: the other way round is `gh release create -F notes.md`, the documented short spelling,
// going to GitHub unread.
//
// A value of `-`, and an `@-`, is standard input rather than a file. The shared check's exit-4
// handshake buffers that input and GhCall records it already (askAgain), so there is nothing
// here to open.
//
// Four things are deliberately left out:
//
//   - `-f`/`--raw-field`, which sends its value literally: `gh api -f body=@x` posts the two
//     characters `@x` and opens no file.
//   - `--env-file` (`gh secret set`, `gh variable set`), whose values are secrets GitHub stores
//     and never publishes. A secret is the shared check's to read, not this rule file's, whose
//     four classes are a repository name, a company name, an internal host and a Tailscale
//     MagicDNS host.
//   - `--template`, which names a template of the repository (`gh pr create --template
//     "pull_request_template.md"`) or a Go template for gh's own output (`-t, --template
//     string`), neither of them a file the command sends.
//   - `gh gist create <filename>…`, whose positional arguments are files gh publishes, and
//     every other positional file: `gh release create <tag> <asset>…`, `gh alias import
//     <filename>`. Reading a positional would mean a copy of gh's grammar in here. An option's
//     name says what its value is in every command that carries it, while a positional's
//     meaning is the leaf's — a local file in `gist create`, a name inside the gist in `gist
//     edit <id> <filename>`, a number in `pr comment <number>` — and this reads argv with no gh
//     to ask, as cmd/legion's own reader asks gh's help. Half a copy would read as cover where
//     there is none: `gist edit --add <path>` publishes a file too, and its short `-a` is
//     `--all` in other commands, so it cannot be added to the table above. What these files
//     keep is the shared check, which reads every argument of the call, and the spec's own line
//     on release assets, that reading inside them is the check's policy and not this issue's
//     (dispatch://INFRA-1527, Goals and non-goals). No Legion role writes a gist: the roles
//     post pull request and issue text, which the options above carry.
//
// Arguments past a bare `--` are read as nothing: gh's parser takes no option after one, so a
// `--body-file` there is a word of some other program's and names no file gh would send.
func ghFileArgs(args []string) []ghFile {
	var files []ghFile
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		option, value, attached := arg, "", false
		if strings.HasPrefix(arg, "-F") {
			option, value, attached = "-F", arg[len("-F"):], len(arg) > len("-F")
		} else if name, after, found := strings.Cut(arg, "="); found {
			option, value, attached = name, after, true
		}
		field := option == "-F" || option == "--field"
		if !field && !ghFileOptions[option] {
			continue
		}
		if !attached {
			if i+1 == len(args) {
				// An option with no value at all: gh refuses the call itself, and there is no
				// file here to read.
				break
			}
			i++
			value = args[i]
		}
		path := value
		if field {
			_, after, found := strings.Cut(value, "=")
			switch {
			case found && strings.HasPrefix(after, "@"):
				path = strings.TrimPrefix(after, "@")
			case found || option != "-F":
				continue
			}
		}
		// A path of "-" is standard input, and a file named twice is read once: a second record
		// of one file is a second open and a second line of the same report.
		if path == "" || path == "-" {
			continue
		}
		if slices.ContainsFunc(files, func(file ghFile) bool { return file.path == path }) {
			continue
		}
		files = append(files, ghFile{option: option, path: path})
	}
	return files
}

// readGhFile is the text of one file a gh call would send. A file that cannot be opened, cannot
// be read, or is longer than maxGhFile is a setup failure naming the option that named it: a
// guard that could not read what the call publishes has cleared nothing, and a call it cleared
// would be the hole this closes.
//
// The path is masked in the message as a report masks one, and the error under it is reduced to
// what went wrong: os reports an open failure as a *fs.PathError whose own message repeats the
// path unmasked, which would carry out of this process the very name the mask is for.
func readGhFile(named ghFile) (string, error) {
	failed := func(why string, err error) error {
		var path *fs.PathError
		if errors.As(err, &path) {
			err = path.Err
		}
		return &SetupFailure{
			What: whatGhCall,
			Why:  "the file " + named.option + " names, " + maskPath(named.path) + ", " + why,
			Err:  err,
		}
	}
	file, err := os.Open(named.path)
	if err != nil {
		return "", failed("could not be opened", err)
	}
	defer file.Close()
	// One byte past the bound, so a file at the bound reads whole and a longer one is known to
	// be longer without reading it. The stat is not asked for: a named pipe and a character
	// device report a size of zero and would read on past it.
	text, err := io.ReadAll(io.LimitReader(file, maxGhFile+1))
	if err != nil {
		return "", failed("could not be read", err)
	}
	if len(text) > maxGhFile {
		return "", failed(fmt.Sprintf("is longer than %d bytes, which is more text than GitHub takes through it", maxGhFile), nil)
	}
	return string(text), nil
}

// askAgain buffers the caller's standard input to a regular file and asks the check again, with
// that file as a descriptor of the child's and PUBLIC_LEAK_CHECK_STDIN naming it. The file is the
// test's and the pod's own temporary directory's, and it is gone before this returns: the bytes
// go back to the caller in memory, which is where they came from.
func (g *Guard) askAgain(ctx context.Context, argv []string, req GhRequest) ([]byte, int, string, error) {
	buffered := []byte{}
	if req.Stdin != nil {
		read, err := io.ReadAll(req.Stdin)
		if err != nil {
			return nil, 0, "", &SetupFailure{What: whatGhCall, Why: "the call's standard input could not be read", Err: err}
		}
		buffered = read
	}
	file, err := os.CreateTemp("", "leakguard-stdin-")
	if err != nil {
		return buffered, 0, "", &SetupFailure{What: whatGhCall, Why: "the call's standard input could not be buffered", Err: err}
	}
	defer func() {
		file.Close()
		os.Remove(file.Name())
	}()
	if _, err := file.Write(buffered); err != nil {
		return buffered, 0, "", &SetupFailure{What: whatGhCall, Why: "the call's standard input could not be buffered", Err: err}
	}
	// The child's descriptor is a dup of this one and shares its offset, so the file is rewound
	// here and not by whoever reads it.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return buffered, 0, "", &SetupFailure{What: whatGhCall, Why: "the call's standard input could not be buffered", Err: err}
	}
	// ExtraFiles' first entry is the child's descriptor 3, which is the one named here.
	const buffer = 3
	status, printed, err := g.run(ctx, child{
		argv:  argv,
		files: []*os.File{file},
		env: []string{
			fmt.Sprintf("PUBLIC_LEAK_CHECK_STDIN=/dev/fd/%d", buffer),
			// The check's own advice on exit 4 is for a caller that cannot buffer. This one
			// just did, so the advice is turned off.
			"PUBLIC_LEAK_CHECK_STDIN_BUFFERS=1",
		},
	})
	if err != nil {
		return buffered, 0, "", &SetupFailure{What: whatGhCall, Why: "the shared public leak check could not be run", Err: err}
	}
	if status == checkWantsStdin {
		return buffered, 0, printed, &SetupFailure{
			What: whatGhCall,
			Why:  "the shared public leak check asked for standard input again after it was given",
			Err:  printedError(printed),
		}
	}
	return buffered, status, printed, nil
}

package leakguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
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
	// Files are the files this call would send to GitHub as content, read by a caller that
	// parses the argv as gh does. `legion gh` is that caller: it reads the command's own help
	// for every option's arity before it reaches the guard (internal/ghroute's TargetOf), so it
	// knows a `-dF <file>` and a `-F=<file>` for what they are, which a reader of the argv
	// alone cannot.
	//
	// Nil is a caller that parsed none, and the guard then reads the argv with a table of its
	// own (ghFileArgs). `legion gh` hands nil only from the answers of its parse that send
	// nothing (`--version`, a group with no leaf, `--help`), so in this binary the table is the
	// fail-closed answer for a future caller that hands the guard bare arguments, not a path
	// any write takes today. An empty, non-nil slice is a caller that parsed and found no file,
	// and the table is not consulted for it.
	Files []GhFile
	// WritesNothing is a call its caller found sends no text into any repository: a read, which
	// `legion gh` admits only by a grammar of the words the call is typed with, one the shared
	// check reads as a read too (internal/ghroute's Target.WritesNothing holds the rule). Where
	// the resolved check is the one that rule was proven against (ProvenCheck), the guard runs
	// the shared check, which refuses a credential in every call it reads as a read, and skips
	// what only a write needs: the destination's visibility read and the private-name pass, which
	// protect the text a write leaves in a public repository. Under any other check the call gets
	// the whole guard.
	WritesNothing bool
	// ProvenCheck is the sha256, in hex, of the shared check's public_leak_check.py that the
	// caller's rule for WritesNothing was proven against. `legion gh` hands the digest compiled
	// into it (cmd/legion's ghProvenCheckSHA256, ghroute.ReadsProvenCheckSHA256) for every call,
	// whatever checker its role is told (CheckerVar). WritesNothing is honoured only where the
	// program the check runs hashes to it (provenCheck) — under a deployment's digest the
	// public_leak_check.py that digest covered, hashed from the bytes the guard read before the
	// run — so a check of another version, or one edited under the same tag, gets the whole guard,
	// and so does every call where it is empty.
	ProvenCheck string
	// StdinSent is a call that sends its standard input to GitHub as content: a file option
	// given `-`, or a field given `@-`. A caller that parsed the command (Files not nil) says so;
	// for a bare argv the guard reads it from the argv (ghFileArgs). It is read only where no
	// shared check runs (sharedCheck). The check's own handshake buffers standard input for every
	// call that reads it (askAgain), and without the check the guard buffers it itself for such a
	// call, so the private-name pass reads the body the call publishes.
	StdinSent bool
}

// GhAnswer is what GhCall cleared a call with: the standard input it read, for the caller to hand
// gh in its place, nil where it read none and the caller's own is still whole; and whether the
// shared check was the whole of the guard (GhRequest.WritesNothing honoured), which is the one
// case that read no visibility and ran no private-name pass.
type GhAnswer struct {
	Stdin       []byte
	CheckedOnly bool
}

// checkProgram is the shared check's program, beside the script Resolve finds, whose sha256 says
// which check is installed: the script only runs it.
const checkProgram = "public_leak_check.py"

// provenCheck says whether the program check runs is the one proven names, read before the check
// runs: under a deployment's digest, the public_leak_check.py that digest covered, from the bytes
// the guard read and runs (sharedRun.program); under none, the public_leak_check.py beside the
// check Resolve found, the script's own directory once its symbolic links are followed. A check
// with no such file, one whose file cannot be read, and one whose file holds anything else are
// not, and neither is any check where proven is empty.
func provenCheck(check sharedRun, proven string) bool {
	if proven == "" {
		return false
	}
	program := check.program
	if program == nil {
		dir, err := runningCheckDir(check.path)
		if err != nil {
			return false
		}
		if program, err = readManifestFile(dir, checkProgram); err != nil {
			return false
		}
	}
	sum := sha256.Sum256(program)
	return strings.EqualFold(hex.EncodeToString(sum[:]), proven)
}

// WhatGhCall is what a refusal and a setup failure of this shape call the write. Both halves of
// the check — the first call and the one that follows it with the buffer — report the one call,
// and the gh tool's own setup failure names the call by it too (ghtool), so the two read alike.
const WhatGhCall = "the gh call"

// GhCall checks a gh call before it is made, in the shared check's gh-guard shape, and answers
// the standard input it read and whether the shared check was the whole of it, which it is for
// a call that writes nothing under the check its caller's rule was proven against
// (GhRequest.WritesNothing, GhRequest.ProvenCheck).
//
// The check reads a call that takes its body on standard input in two goes: the first call exits
// 4, meaning "this call reads standard input; buffer it and ask me again", and the second call is
// handed the buffer. The buffer is a regular file, never a pipe: the check reads it twice, and
// the check itself refuses a pipe for that reason. The bytes come back so the caller can hand
// them to gh, because by then this package has read the caller's standard input and the caller
// cannot read it again; a nil Stdin means standard input was never read and the caller's own is
// still whole.
func (g *Guard) GhCall(ctx context.Context, req GhRequest) (GhAnswer, error) {
	buffered, checkedOnly, err := g.ghCall(ctx, req)
	return GhAnswer{Stdin: buffered, CheckedOnly: checkedOnly}, err
}

// ghCall is GhCall's work: the standard input it read, whether the shared check was the whole of
// the guard, and the refusal or setup failure, if any. Under a deployment that runs no shared
// check (sharedCheck) the visibility read and the private-name pass are the whole of it, the
// standard input a call sends as content read by the guard itself (StdinSent).
func (g *Guard) ghCall(ctx context.Context, req GhRequest) ([]byte, bool, error) {
	check, err := g.sharedCheck(WhatGhCall)
	if err != nil {
		return nil, false, err
	}
	defer check.remove()
	files, stdinSent := req.Files, req.StdinSent
	if files == nil {
		files, stdinSent = ghFileArgs(req.Args)
	}
	var buffered []byte
	switch {
	case check.path != "":
		// Read before the check runs, so nothing the run or anyone else writes meanwhile decides it.
		checkedOnly := req.WritesNothing && provenCheck(check, req.ProvenCheck)
		if buffered, err = g.ghGuard(ctx, check, req); err != nil {
			return buffered, false, err
		}
		if checkedOnly {
			// A read leaves no text in any repository, so neither the destination's visibility
			// nor the private-name pass is read for it: the check above, which refuses a
			// credential in a call it reads as a read, is all it needs. Under a check its
			// caller's rule was not proven against, the check may read the call as a write, so
			// the guard goes on whole.
			return buffered, true, nil
		}
	case stdinSent:
		if buffered, err = readStdin(req); err != nil {
			return nil, false, err
		}
	}
	public, err := g.scanThrough(ctx, WhatGhCall, req.Repo)
	if err != nil {
		return buffered, false, err
	}
	if !public {
		return buffered, false, nil
	}
	records := []Record{{Label: "the gh call's arguments", Text: strings.Join(req.Args, " ")}}
	if len(buffered) > 0 {
		records = append(records, jsonAlso(Record{Label: "the gh call's standard input", Text: string(buffered)})...)
	}
	// The text of each file the call would send, read here and not before the visibility answer:
	// a private or internal destination runs no rule pass, and reading its files would cost the
	// opens for nothing.
	if req.Files == nil {
		if upload, found := ghUnreadUploadArg(req.Args); found {
			return buffered, false, &SetupFailure{
				What: WhatGhCall,
				Why:  upload + " uploads the file it names, which this guard does not read, so the call sends nothing; write the text in --body or --body-file, which it reads",
			}
		}
	}
	for _, named := range files {
		text, err := readGhFile(named)
		if err != nil {
			return buffered, false, err
		}
		record := Record{Label: named.Path, Text: text}
		if named.Option == inputOption {
			records = append(records, jsonAlso(record)...)
			continue
		}
		records = append(records, record)
	}
	return buffered, false, g.rules(ctx, WhatGhCall, records)
}

// ghGuard runs the shared check in its gh-guard shape over req, and answers the standard input it
// buffered for the check's second go (askAgain), nil where the check asked for none, and the
// refusal or setup failure of the check's answer, if any (readStatus).
func (g *Guard) ghGuard(ctx context.Context, check sharedRun, req GhRequest) ([]byte, error) {
	argv := append([]string{check.path, "gh-guard"}, req.Args...)
	status, printed, err := g.run(ctx, child{argv: argv})
	if err != nil {
		return nil, sharedNotRun(WhatGhCall, err, printed)
	}
	var buffered []byte
	if status == checkWantsStdin {
		if buffered, status, printed, err = g.askAgain(ctx, argv, check.dir, req); err != nil {
			return buffered, err
		}
	}
	return buffered, readStatus(WhatGhCall, status, printed)
}

// jsonAlso is record and, where its text is a JSON document, a second record of the strings
// that document decodes to.
//
// A JSON string's escapes hide its text from the rules: `\u0074` is a letter no pattern reads,
// and an escape inside a name parts the two halves of a word two of these rules need whole. What
// GitHub stores is the decoded string, so the decoded string is what a rule must read; the raw
// bytes are read as well, since a name can stand unescaped in a part of the document no string
// of it decodes to. The shared check reads a JSON text both ways for this same reason (its
// README's decode_all).
//
// Two of a gh call's records are JSON and get this, and two are not:
//
//   - `gh api --input <file>`, whose whole file is the request body GitHub parses as JSON, and
//     the buffered standard input, which is that same body for `gh api --input -` and the shared
//     check's exit-4 handshake has already read. Both are decoded where they parse, and a text
//     that is no JSON document — a Markdown body on standard input — is left as the one record
//     it was.
//   - The argv. `gh api -f name=value` and `-F name=value` send the value as the literal string
//     it is, gh doing the escaping, so a `\u0074` written in an argument reaches GitHub's REST
//     API as those six characters and the rules read what GitHub would show. A GraphQL query is
//     the exception: `gh api graphql -f query=…` hands GitHub a document whose string literals
//     GitHub's own parser decodes, and this pass reads the query as it was written.
//   - A `-F name=@<path>` field's file, whose bytes gh makes one JSON string of, escaping them
//     itself: the file holds the text GitHub shows, which is the record already read.
func jsonAlso(record Record) []Record {
	var held any
	if err := json.Unmarshal([]byte(record.Text), &held); err != nil {
		return []Record{record}
	}
	var decoded strings.Builder
	jsonStrings(held, &decoded)
	if decoded.Len() == 0 {
		return []Record{record}
	}
	// The label says which record this is of, so a report names the one the writer can find, and
	// it is not the path of a file: the one path-scoped allowlist of the rule file is anchored
	// on a whole path, and this record is of a document's strings rather than of that file.
	return []Record{record, {Label: record.Label + ", decoded as JSON", Text: decoded.String()}}
}

// jsonStrings writes every string a decoded JSON document holds, an object's names as well as
// its values, one to a line. An object's names are written in order, so that two runs over one
// document read the same text: Go hands a map's keys out in no order of its own.
func jsonStrings(value any, out *strings.Builder) {
	switch held := value.(type) {
	case string:
		out.WriteString(held)
		out.WriteString("\n")
	case []any:
		for _, item := range held {
			jsonStrings(item, out)
		}
	case map[string]any:
		for _, name := range slices.Sorted(maps.Keys(held)) {
			out.WriteString(name)
			out.WriteString("\n")
			jsonStrings(held[name], out)
		}
	}
}

// maxGhFile is the most this package reads from one file a gh call would send, and from the
// standard input it buffers. GitHub itself refuses a pull request or issue body longer than
// 65,536 characters, and a body is the largest thing these options carry: `gh api --input`
// wraps one in a JSON request, which that escaping and the rest of the request leave well
// inside a mebibyte. A mebibyte is sixteen times that body, so no call GitHub would take meets
// the bound, and a file past it is one an argument names by mistake. Such a file is a setup
// failure rather than a pass — the call is refused either way — and the rule pass never reads a
// gigabyte into memory to run ten regexes over.
const maxGhFile = 1 << 20

// GhFile is one file a gh call would send as content: the option that named it, which a setup
// failure names, and the path the argv wrote, which is the record's label.
type GhFile struct {
	Option, Path string
}

// inputOption is `gh api`'s request body, the one file option whose file is itself a JSON
// document (jsonAlso).
const inputOption = "--input"

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

// ghUnreadUploads are the options whose value names a file gh uploads to GitHub that this
// package does not read: gh 2.104.0's --attach, of `gh issue` and `gh pr` comment, create and
// edit and of `gh issue artifact create` and `edit`, which uploads an image (an SVG's text among
// them, up to 10 MiB) or a video (up to 100 MiB) as an asset GitHub serves. It has no short
// spelling. The fallback refuses a call naming one (ghUnreadUploadArg) rather than read a file
// of that kind and size; `legion gh`, which hands the guard the files it parsed, refuses it
// before the guard runs.
var ghUnreadUploads = map[string]bool{"--attach": true}

// ghUnreadUploadArg is the first option of ghUnreadUploads args names, in its separate or its
// attached spelling (`--attach=<path>`), before a bare `--`, and whether there is one. It reads
// the argv with no gh to say which options take a value, so an option's value that is itself
// `--attach` refuses the call too, the direction ghFileArgs errs in as well.
func ghUnreadUploadArg(args []string) (string, bool) {
	for _, arg := range args {
		if arg == "--" {
			return "", false
		}
		name, _, _ := strings.Cut(arg, "=")
		if ghUnreadUploads[name] {
			return name, true
		}
	}
	return "", false
}

// ghFileArgs are the files args would have gh send to GitHub as content, each named once and in
// the order the argv writes them, and whether args send gh's standard input as content as well.
// A gh call carries its text in a file as readily as in an
// argument — `gh api --method PATCH repos/<owner>/<repo>/pulls/<n> -F body=@body.md` is how a
// Legion role writes a pull request's body — so each file's text is a record of its own beside
// the record of the argv, which holds the file's name and never its text.
//
// This is the fallback, for a caller that hands the guard a bare argv (GhRequest.Files nil). The
// caller that can do better does: `legion gh` parses the command with gh's own help of the leaf,
// so it knows every option's arity and reads shapes no table can — a short cluster whose last
// letter is the option (`-dF <path>`, which is `--draft --body-file <path>` in `gh pr create`
// and `--desc F` in `gh gist create`), and an attached `-F=<path>` — and hands the files it
// found. What is read here instead is ghFileOptions, each in its separate and its attached
// spelling (`--body-file=<path>`), and two more shapes of one short option:
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
// A value of `-`, and an `@-`, is standard input rather than a file, which is answered apart:
// the shared check's exit-4 handshake buffers that input and GhCall records it already
// (askAgain), and under no shared check GhCall buffers it itself (GhRequest.StdinSent), so there
// is nothing here to open.
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
func ghFileArgs(args []string) (files []GhFile, stdin bool) {
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
		if path == "-" {
			stdin = true
			continue
		}
		if path == "" {
			continue
		}
		if slices.ContainsFunc(files, func(file GhFile) bool { return file.Path == path }) {
			continue
		}
		files = append(files, GhFile{Option: option, Path: path})
	}
	return files, stdin
}

// readGhFile is the text of one file a gh call would send. A file that cannot be opened, cannot
// be read, or is longer than maxGhFile is a setup failure naming the option that named it: a
// guard that could not read what the call publishes has cleared nothing, and a call it cleared
// would be the hole this closes.
//
// The path is masked in the message as a report masks one, and the error under it is reduced to
// what went wrong: os reports an open failure as a *fs.PathError whose own message repeats the
// path unmasked, which would carry out of this process the very name the mask is for.
func readGhFile(named GhFile) (string, error) {
	failed := func(why string, err error) error {
		var path *fs.PathError
		if errors.As(err, &path) {
			err = path.Err
		}
		return &SetupFailure{
			What: WhatGhCall,
			Why:  "the file " + named.Option + " names, " + maskPath(named.Path) + ", " + why,
			Err:  err,
		}
	}
	file, err := os.Open(named.Path)
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

// readStdin is the caller's standard input, read whole, and the empty buffer where the caller
// hands none.
//
// The read is bounded as a named file's is, by maxGhFile and for the same reasons: the bytes are
// held in memory and scanned there, and no body GitHub would take comes near a mebibyte. Input
// past the bound is a setup failure, which refuses the call — the guard has not read what it
// would publish — and never a truncated buffer handed on as if it were the whole.
func readStdin(req GhRequest) ([]byte, error) {
	if req.Stdin == nil {
		return []byte{}, nil
	}
	// One byte past the bound, so input at the bound is read whole and longer input is known to
	// be longer without reading it.
	read, err := io.ReadAll(io.LimitReader(req.Stdin, maxGhFile+1))
	if err != nil {
		return nil, &SetupFailure{What: WhatGhCall, Why: "the call's standard input could not be read", Err: err}
	}
	if len(read) > maxGhFile {
		return nil, &SetupFailure{
			What: WhatGhCall,
			Why:  fmt.Sprintf("the call's standard input is longer than %d bytes, which is more text than GitHub takes through it", maxGhFile),
		}
	}
	return read, nil
}

// askAgain buffers the caller's standard input to a regular file (readStdin) and asks the check
// again, with that file as a descriptor of the child's and PUBLIC_LEAK_CHECK_STDIN naming it. The
// file is made in dir, the private copy's directory under a deployment's digest (sharedRun.dir):
// the check reads the file and the caller hands gh the bytes in memory, so a file another role
// could open by its name and rewrite before the check reads it — the issue's scratch, where every
// role's TMPDIR is, is every role's to write — would let the check clear bytes gh never sends. With
// no digest told, dir is "" and the file is made in the temporary directory, as before the
// setting. It is gone before this returns: the bytes go back to the caller in memory, which is
// where they came from.
func (g *Guard) askAgain(ctx context.Context, argv []string, dir string, req GhRequest) ([]byte, int, string, error) {
	buffered, err := readStdin(req)
	if err != nil {
		return nil, 0, "", err
	}
	file, err := os.CreateTemp(dir, "leakguard-stdin-")
	if err != nil {
		return buffered, 0, "", &SetupFailure{What: WhatGhCall, Why: "the call's standard input could not be buffered", Err: err}
	}
	defer func() {
		file.Close()
		os.Remove(file.Name())
	}()
	if _, err := file.Write(buffered); err != nil {
		return buffered, 0, "", &SetupFailure{What: WhatGhCall, Why: "the call's standard input could not be buffered", Err: err}
	}
	// The child's descriptor is a dup of this one and shares its offset, so the file is rewound
	// here and not by whoever reads it.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return buffered, 0, "", &SetupFailure{What: WhatGhCall, Why: "the call's standard input could not be buffered", Err: err}
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
		return buffered, 0, "", sharedNotRun(WhatGhCall, err, printed)
	}
	if status == checkWantsStdin {
		return buffered, 0, printed, &SetupFailure{
			What: WhatGhCall,
			Why:  "the shared public leak check asked for standard input again after it was given",
			Err:  printedError(printed),
		}
	}
	return buffered, status, printed, nil
}

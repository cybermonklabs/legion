package leakguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// GhRequest is one gh call, as the caller is about to make it.
type GhRequest struct {
	// Args is the whole argv the caller would hand gh, gh's own name aside.
	Args []string
	// Stdin is what the caller would hand gh on its standard input, nil for nothing. It is read
	// where the shared check asks for it and where the guard reads it itself (Sent says where),
	// and what is read comes back from GhCall for the caller to hand gh in its place.
	Stdin io.Reader
	// Repo is the destination repository, as "owner/name". Empty where the caller knows no
	// destination, which the guard reads as public (Guard.scanThrough).
	Repo string
	// Sent is what this call would send to GitHub beside its argv (GhSent): the files it sends
	// as content, the use it makes of standard input, whether it sends that input at all and the
	// fields whose value it is, read by a caller that parses the argv as gh does. `legion gh` is
	// that caller: it reads the command's own help for every option's arity before it reaches
	// the guard (cmd/legion's ghTargetOf), so it knows a `-dF <file>` and a `-F=<file>` for what
	// they are, which a reader of the argv alone cannot. Every one of those answers is the
	// caller's where Sent is not nil, and the argv's tables are consulted for none of them.
	//
	// Nil is a caller that parsed nothing, and the guard then reads all of it from the argv with
	// tables of its own (readGhArgv). `legion gh` hands nil only for a command its parse finds
	// sends nothing (cmd/legion's ghTarget.sent lists those commands). So in this binary the
	// tables are the fail-closed answer for a future caller that hands the guard bare arguments,
	// not a path any write takes today.
	//
	// A call that sends its standard input as content (GhSent.StdinSent), as a file option given
	// `-`, a field given `@-` and `gh workflow run --json` do, has that input read by the guard
	// itself, where a rule is named (Guard.rules) and the destination is public, wherever the
	// shared check's handshake did not buffer it already (askAgain): under a deployment that runs
	// no shared check (sharedCheck), and under a check that cleared the call without asking for
	// it, as a check need not ask for an input it clears. So the private-name pass reads what the
	// call publishes whichever check ran, and gh is handed the bytes that pass read. A private or
	// internal destination runs no rule pass, so there the guard reads no input the check did not
	// ask for, and gh reads the caller's own.
	Sent *GhSent
	// WritesNothing is a call its caller found sends no text into any repository: a read, which
	// `legion gh` admits only by a grammar in its code of the words the call is typed with, one
	// the shared check reads as a read too (cmd/legion's ghWritesNothing holds the rule). Where
	// the resolved check is the one that rule was proven against (ProvenCheck), the guard runs
	// the shared check, which refuses a credential in every call it reads as a read, and skips
	// what only a write needs: the destination's visibility read and the private-name pass, which
	// protect the text a write leaves in a public repository. Under any other check the call gets
	// the whole guard.
	WritesNothing bool
	// ProvenCheck is the sha256, in hex, of the shared check's public_leak_check.py that the
	// caller's rule for WritesNothing was proven against. `legion gh` hands the digest compiled
	// into it (cmd/legion's ghReadsProvenCheck) for every call, whatever checker its role is told
	// (CheckerVar). WritesNothing is honoured only where the program the check runs hashes to it
	// (provenCheck) — under a deployment's digest the public_leak_check.py that digest covered,
	// hashed from the bytes the guard read before the run — so a check of another version, or one
	// edited under the same tag, gets the whole guard, and so does every call where it is empty.
	ProvenCheck string
}

// StdinUse is what a gh call makes of its standard input, which says how the guard reads that
// input (stdinRecords). It is named by the use and not by the option that named the input,
// because one option means different things in different commands: `-F` is `--field` in `gh api`,
// where `-F <name>=@-` makes the input the value of a field, and `--body-file` in `gh pr create`,
// where `-F -` makes it the body.
type StdinUse int

const (
	// StdinUnknown is a use nothing knows: a caller that names none, over an argv that names
	// none, or names two that differ, only one of which can have the input. The input is read
	// both ways, the raw record and the strings of a JSON document beside it, which errs toward
	// reading more.
	StdinUnknown StdinUse = iota
	// StdinRequestBody is `gh api --input -`: the input is the request body, read as the call's
	// endpoint says (jsonBody). So is `gh workflow run --json`'s, the run's inputs as one JSON
	// object, each of which GitHub keeps as a value of its own.
	StdinRequestBody
	// StdinField is a `-F <name>=@-` or a `--field <name>=@-` of `gh api` or `gh workflow run`:
	// the input is the value of the fields that name it, and is read by their names.
	StdinField
	// StdinText is any other option's `-` — `--body-file -`, `--notes-file -`, `--from-file -`,
	// and the `-F -` that is one of them — whose input is text GitHub stores as it stands.
	StdinText
)

// GhAnswer is what GhCall cleared a call with.
type GhAnswer struct {
	// Stdin is the standard input the guard read, for the caller to hand gh in its place, and nil
	// where it read none and the caller's own is still whole.
	Stdin []byte
	// Read is how much of the guard the call had.
	Read GhRead
}

// GhRead is how much of the guard GhCall ran over a call it cleared. A call has one of these and
// never two, so they are one value and not a flag for each.
type GhRead int

const (
	// GhReadWhole is the whole guard: the shared check where the deployment runs one, the
	// destination's visibility and, where the destination is public, the private-name pass over
	// what the call sends.
	GhReadWhole GhRead = iota
	// GhReadCheckedOnly is the shared check as the whole of the guard (GhRequest.WritesNothing
	// honoured): the call read no visibility and had no private-name step.
	GhReadCheckedOnly
	// GhReadNoRules is the shared check alone, where the deployment runs one, because no rule is
	// named: the call skipped the private-name step, and read no visibility, no file the call
	// sends and no standard input the shared check did not ask for.
	GhReadNoRules
)

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

// whatGhCall is what a refusal and a setup failure of this shape call the write. Both halves of
// the check — the first call and the one that follows it with the buffer — report the one call.
const whatGhCall = "the gh call"

// GhCall checks a gh call before it is made, in the shared check's gh-guard shape, and answers
// the standard input it read and how much of the guard the call had (GhRead): the shared check
// alone for a call that writes nothing under the check its caller's rule was proven against
// (GhRequest.WritesNothing, GhRequest.ProvenCheck) and for a call under no named rule, and the
// whole guard for every other.
//
// The check reads a call that takes its body on standard input in two goes: the first call exits
// 4, meaning "this call reads standard input; buffer it and ask me again", and the second call is
// handed the buffer. The buffer is a regular file, never a pipe: the check reads it twice, and
// the check itself refuses a pipe for that reason. The bytes come back so the caller can hand
// them to gh, because by then this package has read the caller's standard input and the caller
// cannot read it again; a nil Stdin means standard input was never read and the caller's own is
// still whole. Where the check did not buffer the input, the guard reads it itself as
// GhRequest.Sent says.
//
// A `gh api` call whose body GitHub stores as gh sends it (ghEndpointStoredAsSent), a release
// asset's upload among them, has that body read whole and as its decoded JSON strings beside it
// (jsonBody), and such a call whose fields are its body is refused as a setup failure
// (ghArgv.fieldsAsBody).
func (g *Guard) GhCall(ctx context.Context, req GhRequest) (GhAnswer, error) {
	var answer GhAnswer
	check, err := g.sharedCheck(whatGhCall)
	if err != nil {
		return answer, err
	}
	defer check.remove()
	if check.path != "" {
		// Read before the check runs, so nothing the run or anyone else writes meanwhile decides it.
		checkedOnly := req.WritesNothing && provenCheck(check, req.ProvenCheck)
		if answer.Stdin, err = g.ghGuard(ctx, check, req); err != nil {
			return answer, err
		}
		if checkedOnly {
			// A read leaves no text in any repository, so neither the destination's visibility
			// nor the private-name pass is read for it: the check above, which refuses a
			// credential in a call it reads as a read, is all it needs. Under a check its
			// caller's rule was not proven against, the check may read the call as a write, so
			// the guard goes on whole.
			answer.Read = GhReadCheckedOnly
			return answer, nil
		}
	}
	rules, err := g.rules(whatGhCall)
	if err != nil {
		return answer, err
	}
	if rules == nil {
		answer.Read = GhReadNoRules
		return answer, nil
	}
	public, err := g.scanThrough(ctx, whatGhCall, req.Repo)
	if err != nil {
		return answer, err
	}
	if !public {
		return answer, nil
	}
	// What the call would send, read here and not before the visibility answer: a private or
	// internal destination runs no rule pass, so its files are never opened and its standard
	// input goes to gh as the check left it. The argv is read once (readGhArgv), and what the call
	// sends is the caller's parse where it handed one and the argv's tables where it did not
	// (GhRequest.Sent).
	call := readGhArgv(req.Args)
	if call.endpoint.kind == ghEndpointStoredAsSent && call.fieldsAsBody {
		// gh would send the fields as one JSON document, which GitHub stores as sent, two
		// fields on one line, and which the guard reads one field at a time (readLeaf).
		return answer, &SetupFailure{
			What: whatGhCall,
			Why:  "GitHub stores the body of a call to " + shownText(call.endpoint.text) + " as gh sends it, and gh sends this call's fields as that body, one JSON document, which this guard reads one field at a time; send the asset's bytes with --input <file> or --input -",
		}
	}
	sent := req.Sent
	if sent == nil {
		if call.upload != "" {
			return answer, &SetupFailure{
				What: whatGhCall,
				Why:  call.upload + " uploads the file it names, which this guard does not read, so the call sends nothing; write the text in --body or --body-file, which it reads",
			}
		}
		sent = &call.sent
	}
	if answer.Stdin == nil && sent.StdinSent() {
		// askAgain answers a buffer, empty or not, whenever the check asked, so nil is an input
		// nobody has read and the caller's is still whole (GhRequest.Sent says why it is read).
		if answer.Stdin, err = readStdin(req); err != nil {
			return answer, err
		}
	}
	records := call.records
	if len(answer.Stdin) > 0 {
		stdin := Record{Label: "the gh call's standard input", Text: string(answer.Stdin)}
		records = append(records, stdinRecords(stdin, sent.stdinUse, sent.stdinFields, call.endpoint.kind)...)
	}
	for _, named := range sent.Files {
		text, err := readGhFile(named)
		if err != nil {
			return answer, err
		}
		record := Record{Label: named.Path, Text: text}
		switch {
		case call.endpoint.kind == ghEndpointGraphQL && named.Field == graphQLQuery:
			records = append(records, queryRecords(record)...)
		case named.Option == inputOption:
			records = append(records, jsonBody(record, call.endpoint.kind)...)
		default:
			records = append(records, record)
		}
	}
	return answer, rules.Check(ctx, whatGhCall, records...)
}

// ghGuard runs the shared check in its gh-guard shape over req, and answers the standard input it
// buffered for the check's second go (askAgain), nil where the check asked for none, and the
// refusal or setup failure of the check's answer, if any (readStatus).
func (g *Guard) ghGuard(ctx context.Context, check sharedRun, req GhRequest) ([]byte, error) {
	argv := modeArgv(check.path, "gh-guard", req.Args...)
	status, printed, err := g.run(ctx, child{argv: argv})
	if err != nil {
		return nil, sharedNotRun(whatGhCall, err, printed)
	}
	var buffered []byte
	if status == checkWantsStdin {
		if buffered, status, printed, err = g.askAgain(ctx, argv, check.dir, req); err != nil {
			return buffered, err
		}
	}
	return buffered, readStatus(whatGhCall, status, printed)
}

// stdinRecords is the records of record, the call's buffered standard input, by the use the call
// makes of it (GhSent.UsesStdin), fields, the names of the fields whose value it is
// (GhSent.stdinFields), and endpoint, the kind of the call's endpoint (ghEndpointKind). As the
// request body it is read as the endpoint says (jsonBody). As text it is one record: a body
// option's input is the body. Of no known use it is read both ways, the raw record and the strings
// of a JSON document beside it (rawAndJSON), which errs toward reading more.
//
// As fields' value it is text too, one record, since gh makes one JSON string of a field's bytes
// itself; but for the query of a `gh api graphql` call (ghEndpointGraphQL), whose bytes gh sends
// as the query, which GitHub parses, so they are read one literal at a time (graphQLRecords), as
// the same query is from a file. All of the fields `query` is the query alone, with no raw
// record, which would join its literals on one line. Some `query` and some not is a call whose
// one input goes to one field, which gh's order of reading decides and the guard does not guess:
// the raw record and the literals are both read. None `query` is text. A use nobody knows in a
// `gh api graphql` call whose argv names fields of standard input is read as those fields' value.
func stdinRecords(record Record, use StdinUse, fields []string, endpoint ghEndpointKind) []Record {
	graphQL := endpoint == ghEndpointGraphQL
	if use == StdinUnknown && graphQL && len(fields) > 0 {
		use = StdinField
	}
	switch use {
	case StdinRequestBody:
		return jsonBody(record, endpoint)
	case StdinText:
		return []Record{record}
	case StdinField:
		if !graphQL || !slices.Contains(fields, graphQLQuery) {
			return []Record{record}
		}
		literals, lexed := graphQLRecords(record)
		switch {
		case !lexed:
			// A query that does not lex is read as written.
			return []Record{record}
		case slices.ContainsFunc(fields, func(name string) bool { return name != graphQLQuery }):
			return append([]Record{record}, literals...)
		}
		return literals
	}
	return rawAndJSON(record, graphQL)
}

// maxGhFile is the most this package reads from one file a gh call would send, and from the
// standard input it buffers. GitHub itself refuses a pull request or issue body longer than
// 65,536 characters, and a body is the largest thing these options carry to its REST API:
// `gh api --input` wraps one in a JSON request, which that escaping and the rest of the request
// leave well inside a mebibyte. A mebibyte is sixteen times that body, so no call to the REST API
// meets the bound. A release asset uploaded through `gh api --input` (ghEndpointStoredAsSent) can
// be larger, and GitHub takes one of up to 2 GiB; the guard refuses one past the bound, since it
// has not read it. Such a file is a setup failure rather than a pass — the call is refused either
// way — and the rule pass never reads a gigabyte into memory to run its regexes over.
const maxGhFile = 1 << 20

// maxGhFileSaid is what a setup failure says of a file or a standard input longer than maxGhFile,
// after the length it names: readGhFile's and readStdin's.
const maxGhFileSaid = ", the most this guard reads of what a call sends"

// readGhFile is the text of one file a gh call would send, read to maxGhFile (readBounded). A
// file that cannot be opened, cannot be read, or is longer than maxGhFile is a setup failure
// naming the option that named it and the path as it was written, each as shownText shows it: a
// guard that could not read what the call publishes has cleared nothing, and a call it cleared
// would be the hole this closes. The error under it is what went wrong alone, since the message
// names the path already.
func readGhFile(named GhFile) (string, error) {
	text, err := readBounded(named.Path, maxGhFile)
	var failed *boundedReadError
	if !errors.As(err, &failed) {
		return string(text), err
	}
	why := failed.phrase
	if failed.longer {
		why += maxGhFileSaid
	}
	return "", &SetupFailure{
		What: whatGhCall,
		Why:  "the file " + shownText(named.Option) + " names, " + shownText(named.Path) + ", " + why,
		Err:  failed.err,
	}
}

// readStdin is the caller's standard input, read whole, and the empty buffer where the caller
// hands none.
//
// The read is bounded as a named file's is, by maxGhFile and for the same reasons: the bytes are
// held in memory and scanned there, and no body GitHub's REST API would take comes near a
// mebibyte, though a release asset can (maxGhFile). Input past the bound is a setup failure,
// which refuses the call — the guard has not read what it would publish — and never a truncated
// buffer handed on as if it were the whole.
func readStdin(req GhRequest) ([]byte, error) {
	if req.Stdin == nil {
		return []byte{}, nil
	}
	// One byte past the bound, so input at the bound is read whole and longer input is known to
	// be longer without reading it.
	read, err := io.ReadAll(io.LimitReader(req.Stdin, maxGhFile+1))
	if err != nil {
		return nil, &SetupFailure{What: whatGhCall, Why: "the call's standard input could not be read", Err: err}
	}
	if len(read) > maxGhFile {
		return nil, &SetupFailure{
			What: whatGhCall,
			Why:  fmt.Sprintf("the call's standard input is longer than %d bytes%s", maxGhFile, maxGhFileSaid),
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
		return buffered, 0, "", sharedNotRun(whatGhCall, err, printed)
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

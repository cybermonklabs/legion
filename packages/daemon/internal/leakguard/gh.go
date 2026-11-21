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
	"strconv"
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
	// StdinRequestBody is `gh api --input -`: the input is the request body, one JSON document
	// GitHub parses. So is `gh workflow run --json`'s, the run's inputs as one JSON object, each
	// of which GitHub keeps as a value of its own.
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
		records = append(records, stdinRecords(stdin, sent.stdinUse, sent.stdinFields, call.graphQL)...)
	}
	for _, named := range sent.Files {
		text, err := readGhFile(named)
		if err != nil {
			return answer, err
		}
		record := Record{Label: named.Path, Text: text}
		switch {
		case call.graphQL && named.Field == graphQLQuery:
			records = append(records, queryRecords(record)...)
		case named.Option == inputOption:
			records = append(records, jsonBody(record, call.graphQL)...)
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
	argv := append([]string{check.path, "gh-guard"}, req.Args...)
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

// ghArgv is a gh call's argv as readGhArgv reads it: every answer GhCall needs of the argv, from
// one walk of it.
type ghArgv struct {
	// graphQL is whether the call sends GitHub a GraphQL query: a `gh api` call whose endpoint is
	// GitHub's GraphQL endpoint under any spelling (ghAPIGraphQLEndpoint). A call it is true of is
	// what this package's comments call `gh api graphql`.
	graphQL bool
	// records are the records of the argv, in the order it writes them.
	records []Record
	// sent is what the argv's options name for gh to send, in the order the argv writes them
	// (GhSent): the files, the use of standard input, whether it is sent and the fields whose
	// value it is; and upload is the first option of ghUnreadUploads it names, "" for none. These
	// are the fallback, for a caller that hands the guard a bare argv (GhRequest.Sent nil).
	sent   GhSent
	upload string
}

// readGhArgv reads args, a gh call's argv, once, for everything GhCall asks of it: whether the
// call is GraphQL, the records the rules read, the files it sends, the use it makes of standard
// input and the fields that read that input. One walk answers all of them, so that no two answers
// read one argv two ways: a reader that takes an option's value for an option of its own reads a
// field where another reads a title.
//
// The command is read first (ghLeafOf). Two commands are read by the arity of each of their
// options, `gh api` and `gh workflow run` (readLeaf), which are the two whose help gives them
// gh's field options; every other command is read with no arity but that of its file options
// (readOther).
//
// Every argument is a record of its own, "the gh call's argument <i>", i its index in args, which
// holds the argument as it was written, and no record joins two: a rule that needs two words on
// one line matches only where one argument holds both, and the joined argv would put every
// argument on one line. A field of `gh api` and `gh workflow run` is read as two records, its
// name and its value, in place of the argument that holds it (readLeaf). Arguments past a bare
// `--` that ends gh's options are read as arguments, with no field among them, and name no file:
// gh's parser takes no option after one, so a `-f` or a `--body-file` there is a word of some
// other program's, and reading the words costs nothing.
func readGhArgv(args []string) ghArgv {
	argv := ghArgv{records: make([]Record, 0, len(args))}
	leaf, start := ghLeafOf(args)
	if leaf == nil {
		argv.readOther(args)
		return argv
	}
	for i := range start {
		argv.argument(args, i)
	}
	argv.readLeaf(args, leaf, start)
	return argv
}

// ghLeaf is a command of gh's whose options readGhArgv reads by their arity, as gh's own parser
// does: its path, and the long options and the letters of the short ones that take a value
// written as the argument after them. Every other option of it takes none, and a long option
// written `--name=value` holds its own. The rest of it is what readLeaf reads in one of these
// commands and not in the other, so that readLeaf asks the leaf and never which leaf it is.
type ghLeaf struct {
	path  []string
	long  map[string]bool
	short string
	// endpoint is whether the command's first positional argument is an endpoint, which says
	// whether the call is GraphQL (ghAPIGraphQLEndpoint): `gh api`'s.
	endpoint bool
	// bodyOption is the option whose value names the request body, a file or, as `-`, standard
	// input: `gh api`'s --input (inputOption). "" is a command with none, and no option.
	bodyOption string
	// stdinBodyOption is the boolean option that, true, makes standard input the request body:
	// `gh workflow run`'s --json (jsonInputsOption). "" is a command with none, and no option.
	stdinBodyOption string
}

// ghLeaves are the commands of ghLeaf: `gh api` and `gh workflow run`, the two whose help gives
// them gh's field options (ghFieldOptions).
var (
	ghAPILeaf = ghLeaf{
		path: []string{"api"}, long: ghAPIValueOptions, short: ghAPIShortValueOptions,
		endpoint: true, bodyOption: inputOption,
	}
	ghWorkflowRunLeaf = ghLeaf{
		path: []string{"workflow", "run"}, long: ghWorkflowRunValueOptions, short: ghWorkflowRunShortValueOptions,
		stdinBodyOption: jsonInputsOption,
	}
	ghLeaves = []*ghLeaf{&ghAPILeaf, &ghWorkflowRunLeaf}
)

// ghAPIValueOptions are `gh api`'s long options that take a value, and ghAPIShortValueOptions the
// letters of its short ones, in gh 2.98.0's help (cmd/legion/testdata/gh-help/api.txt, which
// TestGhLeafOptionTablesAreTheirHelps holds them to). Its other options (`--include`,
// `--paginate`, `--silent`, `--slurp`, `--verbose`, `--allow-escape-sequences`, `--help`) take
// none.
var ghAPIValueOptions = map[string]bool{
	"--cache":     true,
	"--field":     true,
	"--header":    true,
	"--hostname":  true,
	"--input":     true,
	"--jq":        true,
	"--method":    true,
	"--preview":   true,
	"--raw-field": true,
	"--template":  true,
}

const ghAPIShortValueOptions = "FHXfpqt"

// ghWorkflowRunValueOptions are `gh workflow run`'s long options that take a value, and
// ghWorkflowRunShortValueOptions the letters of its short ones, in gh 2.98.0's help
// (cmd/legion/testdata/gh-help/workflow_run.txt, which TestGhLeafOptionTablesAreTheirHelps holds
// them to), the inherited -R/--repo among them. Its other options take none: `--help`, and
// `--json`, a boolean that makes standard input the run's inputs (jsonInputsOption, readLeaf).
var ghWorkflowRunValueOptions = map[string]bool{
	"--field":     true,
	"--raw-field": true,
	"--ref":       true,
	"--repo":      true,
}

const ghWorkflowRunShortValueOptions = "FRfr"

// ghFieldOptions are gh's field options, which `gh api` and `gh workflow run` have and no other
// command does, each true where its value is typed: a typed field's `@<path>` reads its value
// from the file, and its `@-` from standard input, where a raw field sends its value literally,
// `@` and all (`gh api -f body=@x` posts the two characters `@x` and opens no file).
var ghFieldOptions = map[string]bool{
	"-f":          false,
	"--raw-field": false,
	"-F":          true,
	"--field":     true,
}

// ghLeafOf is the command of ghLeaves args runs and the index of its first argument past the
// command's own words, or nil where args runs any other. The command is the words of args that
// are not options, read until they are a path of ghLeaves or a word leaves every path. -R and
// --repo, which gh takes before and between those words and `legion gh` does too (`-R <v>`,
// `-R<v>`, `--repo <v>`, `--repo=<v>`), are passed over with their value, and so is any other
// option there, `--help` and `-h` among them, which takes none. A bare `--` there ends gh's
// options before any command is named.
func ghLeafOf(args []string) (*ghLeaf, int) {
	var words []string
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--":
			return nil, 0
		case arg == "-R" || arg == "--repo":
			i++
		case len(arg) > 1 && arg[0] == '-':
		default:
			words = append(words, arg)
			further := false
			for _, leaf := range ghLeaves {
				if len(leaf.path) < len(words) || !slices.Equal(leaf.path[:len(words)], words) {
					continue
				}
				if len(leaf.path) == len(words) {
					return leaf, i + 1
				}
				further = true
			}
			if !further {
				return nil, 0
			}
		}
	}
	return nil, 0
}

// readLeaf reads args from start, the arguments of leaf, a command of ghLeaves, as gh's parser
// reads them. An option of leaf's table takes the next argument as its value, and that value is
// never read as an option of its own: `gh api -t '-fix <name>=<word>'` is a Go template, read
// whole. A short cluster is read as pflag reads one: the letters before the first that takes a
// value take none (`-i` is `--include`), and that letter ends the cluster, taking the rest of the
// argument, an `=` before it dropped, or the next argument where it is the last letter. So
// `-iF query=<v>`, `-iFquery=<v>` and `-if query=<v>` are fields, and a cluster whose value letter
// is any other (`-iX POST`) is no field.
//
// A field, the `<name>=<value>` given to a field option (ghFieldOptions), is two records: "the gh
// call's field <name>", which holds the value after the first `=`, and "the gh call's field
// <name> (name)", which holds the name (field). A call's fields are values GitHub stores apart —
// the title and the body of `gh api …/issues -f title=… -f body=…`, the inputs of a workflow run,
// a gist's file name in `-f 'files[<name>][content]=…'` — and are read apart for the reason
// arguments are. The value of every other option is the one record of the argument that holds it.
//
// A field's value is the literal string it is: gh does the JSON escaping, so a `\u0074` written in
// an argument reaches GitHub's REST API as those six characters, and the rules read what GitHub
// would show. The one field that is not is a GraphQL query, `query` in a `gh api graphql` call,
// whose literals GitHub's own parser decodes: it is read one literal at a time (queryRecords),
// once the endpoint, which may come after it, says the call is one. A `-F query=@<path>` reads the
// query from the file, which is read so (GhFile.Field), and a `-F query=@-` from standard input,
// which is read so too (stdinRecords); the field's own value is then the path or the `@-`, read as
// written.
//
// `gh api`'s endpoint (ghLeaf.endpoint) is its first positional argument, the first past the bare
// `--` where none comes before it; a `-` alone is a positional to pflag. Its `--input <path>`
// (ghLeaf.bodyOption) names the request body's file, and its `--input -` standard input
// (GhSent.Names).
//
// `gh workflow run`'s `--json` (ghLeaf.stdinBodyOption) is a boolean, `--json` or
// `--json=<bool>`, whose last value is the one pflag keeps; a value that is no boolean leaves it
// as it was, since gh refuses that call itself. Where it ends true, gh reads the run's inputs from
// standard input as one JSON object, so after the walk standard input is named as a request body
// (GhSent.UsesStdin), by the rule of every other option that names it. The argument itself is
// read as any argument is.
func (argv *ghArgv) readLeaf(args []string, leaf *ghLeaf, start int) {
	// endpointNext is whether the next positional argument is the endpoint, which in a command
	// that has one it is until the endpoint is read; queries are the indices in records of the
	// query fields' values, read as GraphQL where the call is; and stdinBody is whether the
	// leaf's stdinBodyOption is true.
	endpointNext, queries, stdinBody := leaf.endpoint, []int(nil), false
read:
	for i := start; i < len(args); i++ {
		arg := args[i]
		var option, value string
		var attached bool
		switch {
		case arg == "--":
			if endpointNext && i+1 < len(args) {
				argv.graphQL = ghAPIGraphQLEndpoint(args[i+1])
			}
			for ; i < len(args); i++ {
				argv.argument(args, i)
			}
			break read
		case strings.HasPrefix(arg, "--"):
			option, value, attached = strings.Cut(arg, "=")
			if !attached && !leaf.long[option] {
				argv.argument(args, i)
				if option == leaf.stdinBodyOption {
					stdinBody = true
				}
				continue
			}
		case len(arg) > 1 && arg[0] == '-':
			letter := 1 + strings.IndexAny(arg[1:], leaf.short)
			if letter == 0 {
				argv.argument(args, i)
				continue
			}
			option, value, attached = "-"+arg[letter:letter+1], strings.TrimPrefix(arg[letter+1:], "="), letter+1 < len(arg)
		default:
			argv.argument(args, i)
			if endpointNext {
				endpointNext, argv.graphQL = false, ghAPIGraphQLEndpoint(arg)
			}
			continue
		}
		at := i
		if !attached {
			argv.argument(args, i)
			if i+1 == len(args) {
				// An option with no value at all: gh refuses the call itself.
				break
			}
			i++
			at, value = i, args[i]
		}
		if typed, field := ghFieldOptions[option]; field {
			if query := argv.field(args, at, option, value, typed); query >= 0 {
				queries = append(queries, query)
			}
			continue
		}
		argv.argument(args, at)
		// An option here is never "", which is the leaf's word for an option it does not have.
		switch option {
		case leaf.bodyOption:
			argv.sent.Names(option, value, "", StdinRequestBody)
		case leaf.stdinBodyOption:
			if on, err := strconv.ParseBool(value); err == nil {
				stdinBody = on
			}
		}
	}
	if stdinBody {
		argv.sent.UsesStdin(StdinRequestBody)
	}
	if argv.graphQL {
		argv.records = spliceQueries(argv.records, queries)
	}
}

// spliceQueries is records with the record at each index of queries, ascending, replaced by the
// records of the GraphQL query it holds (queryRecords): the values of a `gh api graphql` call's
// query fields (readLeaf), which are queries once the endpoint, which may come after them, says
// the call is one.
func spliceQueries(records []Record, queries []int) []Record {
	if len(queries) == 0 {
		return records
	}
	spliced := make([]Record, 0, len(records))
	for k, record := range records {
		if len(queries) > 0 && queries[0] == k {
			queries = queries[1:]
			spliced = append(spliced, queryRecords(record)...)
			continue
		}
		spliced = append(spliced, record)
	}
	return spliced
}

// field reads value, the value of the field option option held by args[at] (readLeaf), typed
// where option is -F or --field, and answers the index in records of the field's value where that
// value is a field named query, which is the query should the call be `gh api graphql`, and -1
// where it is not. A value with no `=` (`-f <name>`, gh's empty array `key[]`) is the argument,
// read whole. A typed field whose value is `@<path>` names a file gh reads the value from, and
// `@-` standard input (GhSent.Names); such a field's value is the `@<path>` as written, and no
// query.
func (argv *ghArgv) field(args []string, at int, option, value string, typed bool) int {
	name, text, found := strings.Cut(value, "=")
	if !found {
		argv.argument(args, at)
		return -1
	}
	label := "the gh call's field " + name
	argv.records = append(argv.records, Record{Label: label + " (name)", Text: name})
	valueAt := len(argv.records)
	argv.records = append(argv.records, Record{Label: label, Text: text})
	if path, fromFile := strings.CutPrefix(text, "@"); typed && fromFile {
		argv.sent.Names(option, path, name, StdinField)
		return -1
	}
	if name != graphQLQuery {
		return -1
	}
	return valueAt
}

// readOther reads args, the argv of a command none of ghLeaves is. Each argument is one record,
// read whole, whatever it looks like: -f and -F are no field options there and mean other things
// — `-F` is `--body-file` in `gh pr create`, `-f` is `--force` in `gh label create` — so a title
// written `-fix <name>=<word>` is a title, which a reader taking it for a field would part into
// two records that a rule needing two words reads apart.
//
// The files it names are the values of ghFileOptions, each in its separate and its attached
// spelling (`--body-file=<path>`), and of `-F <path>`, `-F<path>` and `-F=<path>`, the last as
// pflag reads a short option. gh gives -F to --body-file, --notes-file and --from-file, and outside
// `gh api` and `gh workflow run` to nothing else (`grep -ho -- '-F, --[a-z-]*'
// testdata/gh-help/*.txt | sort -u`), so a -F is a file option whichever of the three the command
// has. A `-F` anywhere but the front of a short argument is not read: it is the value of the
// option before it in `gh gist create` (`-dF` is `--desc F`), and reading it as a file would
// refuse a call over a file nobody named, while in `gh pr create` `-dF` is `--draft --body-file`,
// which only the leaf's own help can say, and `legion gh` reads it so (GhRequest.Sent).
//
// No other option's arity is known here, so an option's value that itself begins with `-F` (a
// `--body` whose first word is `-Fixed`) is read as a -F of its own and refuses the call, naming a
// file nobody meant; and so is a value that is itself `--attach`, which is read as the upload
// (ghUnreadUploads). That is the direction this errs in: the other way round is `gh release create
// -F notes.md`, the documented short spelling, going to GitHub unread.
func (argv *ghArgv) readOther(args []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			for ; i < len(args); i++ {
				argv.argument(args, i)
			}
			return
		}
		argv.argument(args, i)
		name, after, found := strings.Cut(arg, "=")
		if ghUnreadUploads[name] && argv.upload == "" {
			argv.upload = name
		}
		var option, value string
		var attached bool
		switch {
		case strings.HasPrefix(arg, "-F"):
			option, value, attached = "-F", strings.TrimPrefix(arg[2:], "="), len(arg) > 2
		case ghFileOptions[name]:
			option, value, attached = name, after, found
		default:
			continue
		}
		if !attached {
			if i+1 == len(args) {
				// An option with no value at all: gh refuses the call itself, and there is no
				// file here to read.
				break
			}
			i++
			argv.argument(args, i)
			value = args[i]
		}
		argv.sent.Names(option, value, "", StdinText)
	}
}

// GhSent is what a gh call's options name for gh to send to GitHub beside its argv: the files it
// sends as content, the use it makes of standard input, whether it sends that input at all, and
// the fields whose value it is. Its two methods are the one rule for what a value an option
// names is (Names, UsesStdin): a path a file named once, `-` standard input with the use its
// option makes of it, and two uses that differ no known use. Both readers of a gh call keep to
// it: this package's own, over a bare argv in the argv's order (readGhArgv), and cmd/legion's,
// over its parse of the command by gh's help, in the order the options' names sort (its
// ghSentFiles). So the two name what one option's value sends the same way.
//
// Finding the values is each reader's own work — which options a call names, and the `<path>` of
// a field's `<name>=@<path>` — and the two read some calls apart: `gh pr create -dF <path>` is
// `--draft --body-file <path>` to cmd/legion, which reads the leaf's help, and no file to this
// package's reader, which cannot tell it from `gh gist create`'s `--desc F` (readOther). What a
// call whose caller handed its parse sends is that parse's alone (GhRequest.Sent), so this
// package's reading decides it only where no caller parsed the call.
//
// The zero GhSent has named nothing, which is the parse of a call that sends nothing beside its
// argv.
type GhSent struct {
	// Files are the files named, each once, in the order they were named.
	Files []GhFile
	// stdinUse is the use the call makes of standard input: the first use named, and
	// StdinUnknown where none is or where a second that differs from it is (UsesStdin).
	stdinUse StdinUse
	// stdinNamed is whether any use of standard input is named, which tells a first use from a
	// second.
	stdinNamed bool
	// stdinFields are the names of the fields whose value is standard input, each `-F <name>=@-`
	// and `--field <name>=@-`, in the order they were named, by which the guard reads that input
	// (stdinRecords). A -f or --raw-field sends `@-` as those two characters and reads no input,
	// so it names none.
	stdinFields []string
}

// Names reads path, the value of option where it names a file gh sends (field, the name of the
// field the file's text is the value of, "" for any other option): standard input, used as use
// says, where path is `-`; nothing where it is empty; and a file to send for any other path,
// which is named once, since a second record of one file is a second open and a second line of
// the same report.
//
// Standard input is no file to open: GhCall reads it as GhRequest.Sent says. What is kept is that
// it is named (StdinSent), the use the option makes of it (UsesStdin), and the field's name.
func (sent *GhSent) Names(option, path, field string, use StdinUse) {
	switch path {
	case "":
	case "-":
		if field != "" {
			sent.stdinFields = append(sent.stdinFields, field)
		}
		sent.UsesStdin(use)
	default:
		if !slices.ContainsFunc(sent.Files, func(file GhFile) bool { return file.Path == path }) {
			sent.Files = append(sent.Files, GhFile{Option: option, Path: path, Field: field})
		}
	}
}

// UsesStdin names standard input with use: for an option's `-` (Names), and on its own for an
// option that makes standard input an input with no path to name it by, `gh workflow run --json`,
// whose true makes it the run's inputs (StdinRequestBody). Two that name it with uses that differ
// leave its use StdinUnknown, in whichever order they come, since only one of them can have the
// input, and the guard then reads it both ways.
func (sent *GhSent) UsesStdin(use StdinUse) {
	switch {
	case !sent.stdinNamed:
		sent.stdinUse, sent.stdinNamed = use, true
	case sent.stdinUse != use:
		sent.stdinUse = StdinUnknown
	}
}

// StdinSent is whether any use of standard input is named (UsesStdin): whether the call sends its
// standard input to GitHub as content, which decides whether GhCall reads it (GhRequest.Sent).
func (sent *GhSent) StdinSent() bool {
	return sent.stdinNamed
}

// argument records args[i], as it was written, as "the gh call's argument <i>".
func (argv *ghArgv) argument(args []string, i int) {
	argv.records = append(argv.records, Record{Label: "the gh call's argument " + strconv.Itoa(i), Text: args[i]})
}

// stdinRecords is the records of record, the call's buffered standard input, by the use the call
// makes of it (GhSent.UsesStdin), and fields, the names of the fields whose value it is
// (GhSent.stdinFields). As the request body it is read as JSON (jsonBody). As text it is one
// record: a body option's input is the body. Of no known use it is read both ways, the raw record
// and the strings of a JSON document beside it, which errs toward reading more.
//
// As fields' value it is text too, one record, since gh makes one JSON string of a field's bytes
// itself; but for the query of a `gh api graphql` call (graphQL), whose bytes gh sends as the
// query, which GitHub parses, so they are read one literal at a time (graphQLRecords), as the
// same query is from a file. All of the fields `query` is the query alone, with no raw record,
// which would join its literals on one line. Some `query` and some not is a call whose one input
// goes to one field, which gh's order of reading decides and the guard does not guess: the raw
// record and the literals are both read. None `query` is text. A use nobody knows in a `gh api
// graphql` call whose argv names fields of standard input is read as those fields' value.
func stdinRecords(record Record, use StdinUse, fields []string, graphQL bool) []Record {
	if use == StdinUnknown && graphQL && len(fields) > 0 {
		use = StdinField
	}
	switch use {
	case StdinRequestBody:
		return jsonBody(record, graphQL)
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
	decoded, _ := jsonFields(record, graphQL)
	return append([]Record{record}, decoded...)
}

// maxGhFile is the most this package reads from one file a gh call would send, and from the
// standard input it buffers. GitHub itself refuses a pull request or issue body longer than
// 65,536 characters, and a body is the largest thing these options carry: `gh api --input`
// wraps one in a JSON request, which that escaping and the rest of the request leave well
// inside a mebibyte. A mebibyte is sixteen times that body, so no call GitHub would take meets
// the bound, and a file past it is one an argument names by mistake. Such a file is a setup
// failure rather than a pass — the call is refused either way — and the rule pass never reads a
// gigabyte into memory to run its regexes over.
const maxGhFile = 1 << 20

// GhFile is one file a gh call would send as content: the option that named it, which a setup
// failure names; the path the argv wrote, which is the record's label; and, for a file a field
// names (`--field <name>=@<path>`, `-F <name>=@<path>`), the field's name, "" for every other
// option. The name is what says that a file is a GraphQL query, `query` in a `gh api graphql`
// call, which is read one literal at a time (graphQLRecords).
type GhFile struct {
	Option, Path, Field string
}

// inputOption is `gh api`'s request body, the one option whose file is itself a JSON document
// (jsonBody), and one of the two whose standard input is (stdinRecords).
const inputOption = "--input"

// jsonInputsOption is the other: `gh workflow run --json`, a boolean that makes standard input
// the run's inputs as one JSON object (readLeaf). It names no file. In every other gh command
// `--json` takes the names of the fields to print, and makes no input of standard input.
const jsonInputsOption = "--json"

// ghFileOptions are the options of the commands readOther reads whose whole value is the path of
// a file gh sends to GitHub as content, in gh 2.98.0's spelling — the version the worker image
// pins, whose help is cmd/legion/testdata/gh-help, which is where each of these was read. A gh
// call carries its text in a file as readily as in an argument, so each file's text is a record of
// its own beside the records of the argv, which hold the file's name and never its text. `gh
// api`'s files are readLeaf's: its --input, and the `@<path>` of a typed field, which `gh
// workflow run` reads too — `gh api --method PATCH repos/<owner>/<repo>/pulls/<n> -F
// body=@body.md` is how a Legion role writes a pull request's body.
//
// These are the fallback, for a caller that hands the guard a bare argv (GhRequest.Sent nil).
// The caller that can do better does: `legion gh` parses the command with gh's own help of the
// leaf, so it knows every option's arity and reads a shape no table can — a short cluster whose
// last letter is the option (`-dF <path>`, which is `--draft --body-file <path>` in `gh pr
// create` and `--desc F` in `gh gist create`) — and hands the files it found.
//
// Four things are deliberately left out:
//
//   - `-f`/`--raw-field`, which sends its value literally (ghFieldOptions) and opens no file.
//   - `--env-file` (`gh secret set`, `gh variable set`), whose values are secrets GitHub stores
//     and never publishes. A secret is the shared check's to read, not the private-name rules',
//     which are names a deployment keeps out of public repositories.
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
var ghFileOptions = map[string]bool{
	// The body of a pull request, an issue, a comment, a review or a revert. Twelve commands of
	// the pinned reference carry it: `awk '/^### /{cmd=$0} /--body-file/{print cmd}'
	// testdata/gh-help/reference.txt | sort -u`.
	"--body-file": true,
	// `gh release create` and `gh release edit`'s release notes.
	"--notes-file": true,
	// `gh agent-task create`'s task description.
	"--from-file": true,
}

// ghUnreadUploads are the options whose value names a file gh uploads to GitHub that this
// package does not read: gh 2.104.0's --attach, of `gh issue` and `gh pr` comment, create and
// edit and of `gh issue artifact create` and `edit`, which uploads an image (an SVG's text among
// them, up to 10 MiB) or a video (up to 100 MiB) as an asset GitHub serves. It has no short
// spelling, and `gh api` and `gh workflow run` have none of them. The fallback refuses a call
// naming one, in its separate or its attached spelling (`--attach=<path>`), before a bare `--`
// (ghArgv.upload), rather than read a file of that kind and size; `legion gh`, which hands the
// guard its parse (GhRequest.Sent), refuses it before the guard runs.
var ghUnreadUploads = map[string]bool{"--attach": true}

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
		why += ", which is more text than GitHub takes through it"
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
		return nil, &SetupFailure{What: whatGhCall, Why: "the call's standard input could not be read", Err: err}
	}
	if len(read) > maxGhFile {
		return nil, &SetupFailure{
			What: whatGhCall,
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

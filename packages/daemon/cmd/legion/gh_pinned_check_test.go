package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// ghPinnedCheckPortSHA256 is the sha256 of the public_leak_check.py ghPinnedCheckReads was ported
// from, leak-check-v21's. TestGhReadsAreProvenAgainstThePinnedCheck fails while it is not
// ghReadsProvenCheck's sha256, so a bump of the pin fails until this port moves with it.
const ghPinnedCheckPortSHA256 = "5e5e0e00e4932ac307b79f53ac43e7cb156ed9dd38f01780022e69c18cce6354"

// ghPinnedCheckReads says whether the pinned check, the public_leak_check.py whose sha256 is
// ghPinnedCheckPortSHA256, reads argv, a gh call's arguments, as a read in its gh_guard: whether
// it returns for the call before it looks up any target. It reads the call as that check does
// with no GH_HOST, which no call of these tests sets.
//
// The read is the check's, part for part (its gh_guard, is_read, collect and api_read): `--help`,
// `-h` and `--version` first; then a call that begins with an option, whose words after -R/--repo
// values and other options are read by is_read, and which the check refuses, so no read, where
// they are not; then `gh api` (ghPinnedAPIRead); and any other call by is_read over its first two
// words (ghPinnedIsRead).
//
// Four parts differ from the check. Two are stricter here: a GraphQL call is never a read here,
// where the check reads a query with no mutation as one, and `gh secret`, which the check lets go
// before it reads anything, is read here as is_read reads it. Two are looser, and in each the
// check refuses the call before it looks up any target, so neither makes a call the port reads as
// a read cost the check a lookup: the check refuses a first word that is a gh alias in
// GH_CONFIG_DIR's config (gh_alias), which this reads as any other word, and `legion gh` refuses
// such a call itself before the guard; and the check exits 2 on a missing or unreadable file an
// argument names (file_ok), which this never looks for, so `api --input f --input=
// repos/acme/widgets/pulls` with no file f is a read here. No call these tests count as a read
// meets any of the four.
func ghPinnedCheckReads(argv []string) bool {
	first := ""
	if len(argv) > 0 {
		first = argv[0]
	}
	switch {
	case first == "--help" || first == "-h" || first == "--version":
		return true
	case strings.HasPrefix(first, "-"):
		var words []string
		skip := false
		for _, word := range argv {
			if !skip && (word == "-R" || word == "--repo") {
				skip = true
				continue
			}
			if skip || strings.HasPrefix(word, "-") {
				skip = false
				continue
			}
			words = append(words, word)
		}
		return ghPinnedIsRead(words)
	case first == "api":
		return ghPinnedAPIRead(argv[1:])
	}
	return ghPinnedIsRead(argv)
}

// ghPinnedReadAll and ghPinnedReadTwo are the pinned check's READ_ALL and READ_TWO, copied whole:
// the first words of a call it reads as a read, and the first and second words, joined by `:`,
// of the others.
var (
	ghPinnedReadAll = ghWordSet("auth browse completion config alias extension help licenses search status version --help -h --version")
	ghPinnedReadTwo = ghWordSet("pr:view pr:list issue:view issue:list run:view run:list repo:view repo:list " +
		"release:view release:list workflow:view workflow:list project:view project:list ruleset:view ruleset:list " +
		"release:download pr:status issue:status pr:checks pr:diff pr:checkout run:watch run:download repo:clone " +
		"repo:set-default ruleset:check variable:get cache:list label:list org:list gpg-key:list ssh-key:list " +
		"codespace:list variable:list attestation:verify attestation:download")
)

// ghPinnedIsRead is the pinned check's is_read over words: an empty first word, a first word of
// READ_ALL, or a first and second word of READ_TWO.
func ghPinnedIsRead(words []string) bool {
	first, second := "", ""
	if len(words) > 0 {
		first = words[0]
	}
	if len(words) > 1 {
		second = words[1]
	}
	return first == "" || ghPinnedReadAll[first] || ghPinnedReadTwo[first+":"+second]
}

// ghPinnedAPIValueOptions are the pinned check's API_VALUE_OPTS, the long gh api options its
// collect takes a value for in the next word, and ghPinnedFieldOptions are its FIELD_ARGS.
var (
	ghPinnedAPIValueOptions = ghWordSet("--method --field --raw-field --header --jq --template --preview --cache --input --hostname")
	ghPinnedFieldOptions    = ghWordSet("-f -F --field --raw-field")
)

// ghPinnedAPIRead is the pinned check's api_read over its collect's parse of args, the words of a
// gh api call after `api`.
//
// collect reads each word in turn. The word after an option still waiting for its value is that
// value; `--name=value` is the option and its value; a long option of API_VALUE_OPTS waits for its
// value; and a single-dash word is scanned for the first of `XfFHpqt`, the short options that take
// a value, and the rest of the word after that letter, past one `=`, is that option's value. An
// option left with an empty value waits for the next word again, unless its word ends in `=`, and
// a field option sets the field mark either way. The first word that begins with no `-` and is no
// option and no value is the endpoint. The last method is kept, uppercased, and the last host and
// the last input as given.
//
// api_read then reads a read where the host is github.com, the endpoint holds no `://` and is not
// GitHub's GraphQL one, and the call has no input, and the method, or the field mark where no
// method is given, is one of "0", "GET" and "HEAD": the check's `(st.M or str(st.F)) in ("0",
// "GET", "HEAD")`, with the field mark 0 or 1. The endpoint is GraphQL's where it is `graphql` once
// its leading slashes are gone and it is cut at its first `?` or `#` (the check's
// graphql_endpoint, which also strips a leading scheme and host: an endpoint holding `://` is no
// read before that matters).
func ghPinnedAPIRead(args []string) bool {
	var endpoint, method, host, input, pending string
	field := false
	for _, word := range args {
		option, value := "", ""
		switch {
		case pending != "":
			option, value, pending = pending, word, ""
		case strings.HasPrefix(word, "--") && strings.Contains(word, "="):
			option, value, _ = strings.Cut(word, "=")
		case ghPinnedAPIValueOptions[word]:
			option = word
		case len(word) > 1 && word[0] == '-' && word[1] != '-':
			if at := strings.IndexAny(word[1:], "XfFHpqt"); at >= 0 {
				option, value = "-"+word[1+at:2+at], strings.TrimPrefix(word[2+at:], "=")
			}
		}
		if option != "" && value == "" && !strings.HasSuffix(word, "=") {
			pending, option = option, ""
			if ghPinnedFieldOptions[pending] {
				field = true
			}
		}
		if !strings.HasPrefix(word, "-") && option == "" && pending == "" && endpoint == "" {
			endpoint = word
		}
		switch {
		case option == "-X" || option == "--method":
			method = strings.ToUpper(value)
		case option == "--hostname":
			host = value
		case option == "--input":
			input = value
		case ghPinnedFieldOptions[option]:
			field = true
		}
	}
	if host == "" {
		host = "github.com"
	}
	// The check lowercases the host and compares it with github.com. Only an ASCII host of the
	// same length can match, and EqualFold over the same number of bytes is that test.
	if len(host) != len("github.com") || !strings.EqualFold(host, "github.com") || strings.Contains(endpoint, "://") {
		return false
	}
	path := strings.TrimLeft(endpoint, "/")
	if end := strings.IndexAny(path, "?#"); end >= 0 {
		path = path[:end]
	}
	if path == "graphql" || input != "" {
		return false
	}
	if method != "" {
		return method == "0" || method == "GET" || method == "HEAD"
	}
	return !field
}

// ghWordSet is the set of the words in words, separated by spaces.
func ghWordSet(words string) map[string]bool {
	set := map[string]bool{}
	for _, word := range strings.Fields(words) {
		set[word] = true
	}
	return set
}

// ghCheckReadingLikeThePinnedCheck is a stand-in shared check for the call args, which does with
// it what the pinned check does, by ghPinnedCheckReads, decided here for the call the test runs:
// for a call that check reads as a read, nothing; for any other, what that check does for one
// whose text matched a rule other than a credential's on a repository it must look up (a call
// that begins with an option it refuses instead, and no test gives this one such a call). It reads
// the target's visibility, through gh, in the argv and under the variable that check's vis_of
// uses, and records `lookup <slug>` in its own record before the read and what the read did after
// it: its exit status, when it started and ended, and what it printed (ghCheckReadOf). The target
// is LEAK_CHECK_STANDIN_TARGET, which stands for the slug the check's own targets would find.
// Either way it then clears the call, as that check clears such a call to a private repository,
// and it records each run, as leakguardtest.Recording does, so a test reads that it was handed
// args. `legion gh` answers its visibility read itself (answerVisibilityRead), by the guard's REST
// read, so it reaches the stand-in gh as ghGuardLookup, as the guard's own read does: the record is
// what tells the two apart.
func ghCheckReadingLikeThePinnedCheck(args []string) string {
	script := "cat >/dev/null 2>&1\n"
	if !ghPinnedCheckReads(args) {
		script += `if [ -n "${LEAK_CHECK_STANDIN_TARGET-}" ]; then
	printf 'lookup %s\n' "$LEAK_CHECK_STANDIN_TARGET" >>"${LEAK_CHECK_RECORD:-/dev/null}"
	started=$(date +%s%N)
	said=$(PUBLIC_LEAK_CHECK_VISIBILITY_READ=$LEAK_CHECK_STANDIN_TARGET GH_HOST=github.com \
		gh api --hostname github.com "repos/$LEAK_CHECK_STANDIN_TARGET" --jq .visibility 2>&1 </dev/null)
	status=$?
	printf 'read exit %s started %s ended %s said %s\n' "$status" "$started" "$(date +%s%N)" "$(printf '%s' "$said" | tr '\n' ' ')" >>"${LEAK_CHECK_RECORD:-/dev/null}"
fi
`
	}
	return leakguardtest.Recording(script + "exit 0\n")
}

// ghCheckRead is what ghCheckReadingLikeThePinnedCheck recorded of its visibility read: the exit
// status of its gh, how long that gh ran, and what it printed, standard output and error together
// on one line.
type ghCheckRead struct {
	status  int
	ran     time.Duration
	printed string
}

// ghCheckReadOf is the visibility read the stand-in recorded in checked, its record, and whether
// it recorded one.
func ghCheckReadOf(t *testing.T, checked string) (ghCheckRead, bool) {
	t.Helper()
	for line := range strings.Lines(checked) {
		rest, ok := strings.CutPrefix(line, "read exit ")
		if !ok {
			continue
		}
		head, printed, _ := strings.Cut(strings.TrimSuffix(rest, "\n"), " said ")
		var read ghCheckRead
		var started, ended int64
		if _, err := fmt.Sscanf(head, "%d started %d ended %d", &read.status, &started, &ended); err != nil {
			t.Fatalf("the shared check recorded its visibility read as %q, which this cannot read: %v", line, err)
		}
		read.ran, read.printed = time.Duration(ended-started), strings.TrimSpace(printed)
		return read, true
	}
	return ghCheckRead{}, false
}

// ghBoundStop is the words internal/leakguard says of a child its context's deadline stopped
// (stoppedError.said), which `legion gh` prints when visibilityAnswerBound stops the read it
// answers the shared check's own visibility read with. Were the wording to change, such a read
// would still fail ghCheckRead.failure, by its status and what it printed.
const ghBoundStop = "was stopped by its bound"

// failure is how the read failed, and "" where it answered public: the stand-in gh's answer,
// which `legion gh` hands back from the guard's REST read.
func (r ghCheckRead) failure() string {
	switch {
	case strings.Contains(r.printed, ghBoundStop):
		return fmt.Sprintf("the shared check's visibility read was stopped by its bound after %.1f s: %q", r.ran.Seconds(), r.printed)
	case r.status != 0 || r.printed != "public":
		return fmt.Sprintf("the shared check's visibility read exited %d after %.1f s and printed %q; want exit 0 and public, the stand-in gh's answer, which `legion gh` hands back from the guard's REST read", r.status, r.ran.Seconds(), r.printed)
	}
	return ""
}

// ghGuardLookup is what the stand-in gh records of a REST visibility read of acme/widgets: the
// guard's own, which asks for the response's headers, and the shared check's, which `legion gh`
// answers by that same read (answerVisibilityRead).
const ghGuardLookup = "visibility api --hostname github.com repos/acme/widgets --include --jq .visibility\n"

// ghSpelling is a call as typed (argv) and the leaf path its words resolve to (path).
type ghSpelling struct{ argv, path []string }

// ghAPISpellingGroups are the groups of words ghGeneratedSpellings writes gh api calls of: each
// read option ghAPIReadOptions names, its value in the next word; and the shapes found to divide
// gh's parse and the check's collect, and those next to them: an option with an empty value or
// with none, `--`, a single-dash word holding `=`, a value joined to its option or beginning with
// `-`, a method other than exactly GET, a field, an input, a second endpoint and a whole URL.
var ghAPISpellingGroups = [][]string{
	{"--paginate"}, {"--slurp"}, {"--jq", ".x"}, {"-q", ".[].number"}, {"--template", "{{.}}"}, {"-t", "{{.}}"}, {"--cache", "1h"},
	{"-X", "GET"}, {"--method", "GET"}, {"-H", "Accept: application/json"}, {"--header", "Accept: application/json"},
	{"-q", ""}, {"--jq", ""}, {"-H", ""}, {"-q"}, {"-H"}, {"-X"},
	{"--"}, {"-i=false"}, {"-X=GET"}, {"--jq=.x"}, {"-q.x"}, {"-XGET"}, {"-H", "-x"},
	{"-XPOST"}, {"-X", "POST"}, {"-X", "get"}, {"--method=GET"}, {"-f", "x=y"}, {"--input", "-"},
	{"graphql"}, {"https://api.github.com/repos/acme/widgets/pulls"},
}

// ghGeneratedSpellings are the gh api calls of up to three groups of ghAPISpellingGroups, in every
// order and with repetition, with the endpoint repos/acme/widgets/pulls before, between or after
// them; and each leaf of ghReadLeaves, by its call in ghReadLeafCalls, given each option its
// grammar names, with a value where it takes one.
func ghGeneratedSpellings() []ghSpelling {
	var spellings []ghSpelling
	api := []string{"api"}
	var grow func(groups [][]string)
	grow = func(groups [][]string) {
		for at := range len(groups) + 1 {
			argv := slices.Concat(api, slices.Concat(groups[:at]...), []string{ghAPIReadEndpoints[0]}, slices.Concat(groups[at:]...))
			spellings = append(spellings, ghSpelling{argv: argv, path: api})
		}
		if len(groups) < 3 {
			for _, group := range ghAPISpellingGroups {
				grow(append(slices.Clone(groups), group))
			}
		}
	}
	grow(nil)
	for _, leaf := range slices.Sorted(maps.Keys(ghReadLeaves)) {
		options := ghReadLeaves[leaf]
		for _, option := range slices.Sorted(maps.Keys(options)) {
			argv := append(slices.Clone(ghReadLeafCalls[leaf]), option)
			if options[option] {
				argv = append(argv, "a-value")
			}
			spellings = append(spellings, ghSpelling{argv: argv, path: strings.Fields(leaf)})
		}
	}
	return spellings
}

// A call counts as a read only where the shared check counts it as one, so no read spends a
// visibility read in the check either.
//
// Over the spellings ghGeneratedSpellings writes, the cross products of gh api's read options and
// of the shapes found to divide gh's parse and collect's, and each listed leaf with each option
// its grammar names, every call ghWritesNothing admits as a read, whether or not its parse would
// succeed, is one ghPinnedCheckReads reads as a read too: 0 calls read otherwise. So is every
// call of ghReadCalls, which TestLegionGhChecksAReadAndReadsNoVisibility runs end to end. This is
// the test that holds the gh api grammar, -X/--method GET and -H/--header among it, to the pinned
// check, and it starts no process.
//
// And end to end, against ghCheckReadingLikeThePinnedCheck, a call outside the read grammar is
// guarded whole. Where the check reads it as no read — a listed leaf by its alias `pr ls`, a gh
// api endpoint written as a whole URL, GitHub's GraphQL endpoint with a query or a fragment after
// it (`graphql?x=1`, `/graphql#x`), `-i=` with a false value, which collect reads as `-f`, `--`
// before a word collect reads as a method, an empty value before a second option collect then
// takes as that value, so that the word after it is the method or the endpoint, a method other
// than GET, a field and an input — the check looks up its target's visibility, and the guard reads
// it too where the call names acme/widgets, so it costs what it cost on main and no less. Where
// the check reads it as a read — `--` before an endpoint, an empty value before a method, which
// collect takes as that value while gh sends the method, a value after `=` or joined to its short
// option (`--method=GET`, `-XGET`), an empty header and one beginning with `-`, and a listed leaf
// given a value after `=` — it costs the guard's read alone. The shared check's lookup reaches gh
// as the guard's REST read, which `legion gh` answers it with, and the check records it. A lookup
// that did not answer public fails as what it was, stopped by visibilityAnswerBound after so many
// seconds or ending with its own status and words, and not as a read missing from gh's count.
func TestLegionGhReadsOnlyWhatTheSharedCheckReadsAsARead(t *testing.T) {
	t.Run("the generated spellings", func(t *testing.T) {
		spellings := ghGeneratedSpellings()
		admitted, mismatches := 0, 0
		for _, spelling := range spellings {
			if !ghWritesNothing(spelling.argv, spelling.path) {
				continue
			}
			admitted++
			if !ghPinnedCheckReads(spelling.argv) {
				mismatches++
				t.Errorf("legion gh reads %q as a read, and the pinned check's gh_guard as no read", spelling.argv)
			}
		}
		if admitted == 0 {
			t.Fatalf("ghWritesNothing admits none of %d generated spellings", len(spellings))
		}
		t.Logf("%d spellings generated, %d admitted as reads, %d of them read otherwise by the check", len(spellings), admitted, mismatches)
		for _, args := range ghReadCalls() {
			if !ghPinnedCheckReads(args) {
				t.Errorf("legion gh's read %q is no read to the pinned check's gh_guard", args)
			}
		}
	})
	run := ghGuardRig(t)
	for _, tc := range []struct {
		args                   []string
		checkLooks, guardLooks bool
	}{
		{[]string{"pr", "ls", "-R", "acme/widgets"}, true, true},
		{[]string{"api", "https://api.github.com/repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "graphql?x=1"}, true, false},
		{[]string{"api", "/graphql#x"}, true, false},
		{[]string{"api", "-i=false", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "repos/acme/widgets/pulls", "-i=f"}, true, true},
		{[]string{"api", "-i=F", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "-i=FALSE", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "-i=False", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "--", "-XPOST"}, true, false},
		{[]string{"api", "--", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "-q", "", "-q", "-XPOST", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "--jq", "", "--jq", "graphql", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "repos/acme/widgets/pulls", "--jq", "", "-XPOST"}, false, true},
		{[]string{"api", "--jq=.x", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "-q.x", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "-H", "", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "-H", "-x", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "--method=GET", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "-XGET", "repos/acme/widgets/pulls"}, false, true},
		{[]string{"api", "-X", "POST", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "-f", "x=y", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"api", "--input", "-", "repos/acme/widgets/pulls"}, true, true},
		{[]string{"pr", "view", "1", "--repo=acme/widgets"}, false, true},
	} {
		t.Run("guarded whole: "+strings.Join(tc.args, " "), func(t *testing.T) {
			got := run(t, ghCheckAt(t, ghCheckReadingLikeThePinnedCheck(tc.args)), "", tc.args...)
			if got.code != 7 {
				t.Errorf("legion gh %q = exit %d, stderr %q; want gh's own 7", tc.args, got.code, got.stderr)
			}
			if want := "checked gh-guard " + strings.Join(tc.args, " ") + "\n"; !strings.HasPrefix(got.checked, want) {
				t.Errorf("the shared check recorded %q, want its run of the call first, %q", got.checked, want)
			}
			looked := strings.Contains(got.checked, "lookup acme/widgets\n")
			if looked != tc.checkLooks {
				t.Errorf("the shared check read the visibility: %v, want %v: %q", looked, tc.checkLooks, got.checked)
			}
			read, recorded := ghCheckReadOf(t, got.checked)
			if looked && !recorded {
				t.Errorf("the shared check looked up the visibility and recorded no read after it, so what that read did cannot be told: %q", got.checked)
			}
			wantReads := 0
			for _, looks := range []bool{tc.checkLooks, tc.guardLooks} {
				if looks {
					wantReads++
				}
			}
			if recorded && read.failure() != "" {
				// gh's count would then miss a read the record already explains: one the bound
				// stopped before it reached gh, or one that reached gh and failed after.
				t.Error(read.failure())
			} else if reads := strings.Count(got.gh, ghGuardLookup); reads != wantReads {
				t.Errorf("gh was asked the visibility %d times, want %d (the check's read %v, the guard's %v): %q", reads, wantReads, tc.checkLooks, tc.guardLooks, got.gh)
			}
			if strings.Contains(got.log, `visibility="checked-only"`) {
				t.Errorf("the call log reads a call the guard read whole as checked-only: %q", got.log)
			}
		})
	}
}

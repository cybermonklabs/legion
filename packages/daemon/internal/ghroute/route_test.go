package ghroute

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// ghHelpDir holds the real gh's help of each command the resolver's tests read, saved from gh
// 2.98.0 (testdata/gh-help/VERSION) in a fresh configuration, one file per path, its words joined
// by "_", and gh's own help (`gh --help`) as gh.txt. It stays beside `legion gh`, whose tests read
// it too, with the script that saves it (testdata/regenerate-gh-help.sh) and the CI check that
// ties its version to the worker image's gh (.github/scripts/check-gh-version.sh).
var ghHelpDir = filepath.Join("..", "..", "cmd", "legion", "testdata", "gh-help")

// savedGhHelp answers gh's help of path from ghHelpDir as the real gh does
// (ghroutetest.SavedHelp, which `legion gh`'s tests read the same directory with).
var savedGhHelp = ghroutetest.SavedHelp(ghHelpDir)

// noGitRepository is the git remotes of a working directory that is no git repository: none.
var noGitRepository = ghroutetest.NoGitRepository[Remote]

// parseGhHelp reads gh's own help: a group by its COMMANDS section, with every command and help
// topic it lists and the gh aliases and extensions among them; a leaf's canonical path, a cobra
// alias its ALIASES section lists; every option's arity, pflag's optional value, a shorthand taken
// by another option than --help, and what its value selects; and the arguments its USAGE names,
// each of a kind the tables know or none. It refuses a help it cannot read whole, and the help a gh
// alias prints, its command's, which lists no such alias.
func TestParseGhHelpReadsRealGhHelp(t *testing.T) {
	read := func(path ...string) ghCommand {
		t.Helper()
		text, err := savedGhHelp(path)
		if err != nil {
			t.Fatal(err)
		}
		cmd, err := parseGhHelp(path, text)
		if err != nil {
			t.Fatalf("parseGhHelp(%q): %v", path, err)
		}
		return cmd
	}
	kind := func(slot ghSlot) string {
		kind, unknown := slot.kind()
		if unknown != "" {
			return "unknown <" + unknown + ">"
		}
		return kind
	}
	for _, group := range [][]string{{}, {"pr"}, {"repo"}, {"repo", "deploy-key"}, {"search"}} {
		if cmd := read(group...); !cmd.group || !slices.Equal(cmd.path, group) {
			t.Errorf("gh %s's help reads as %+v, want the group %q", strings.Join(group, " "), cmd, group)
		}
	}
	if gh := read(); !gh.ghAliases["co"] || gh.ghAliases["pr"] || len(gh.extensions) != 0 {
		t.Errorf("gh's own help lists the aliases %v and the extensions %v, want gh's default alias co alone", gh.ghAliases, gh.extensions)
	}
	if gh := read(); !gh.listed["pr"] || !gh.listed["api"] || !gh.listed["copilot"] || !gh.listed["environment"] || gh.listed["stack"] || gh.listed["cs"] {
		t.Errorf("gh's own help lists %v, want its commands and help topics, and no official-extension stub or cobra alias", gh.listed)
	}
	view := read("pr", "view")
	if _, h := view.shorts['h']; view.group || !view.flags["json"].value || view.flags["web"].value || !view.readsGHRepo() || view.shorts['R'] != "repo" || h {
		t.Errorf("gh pr view reads as %+v", view)
	}
	if len(view.slots) != 1 || kind(view.slots[0]) != "url" {
		t.Errorf("gh pr view's arguments read as %+v, want one URL-capable slot", view.slots)
	}
	if ls := read("pr", "ls"); !slices.Equal(ls.path, []string{"pr", "list"}) || ls.group {
		t.Errorf("gh pr ls reads as %+v, want its command, pr list, whose help lists the alias", ls)
	}
	if add := read("repo", "deploy-key", "add"); !add.flags["title"].value || add.group {
		t.Errorf("gh repo deploy-key add reads as %+v, want --title taking a value", add)
	}
	if view.flags["repo"].selects != "repository" || view.flags["json"].selects != "" || view.flags["web"].selects != "nothing" {
		t.Errorf("gh pr view's -R selects %q, --json %q and --web %q, want a repository, none of the table's, and nothing", view.flags["repo"].selects, view.flags["json"].selects, view.flags["web"].selects)
	}
	if edit := read("issue", "edit"); edit.flags["add-blocked-by"].selects != "url" || edit.flags["parent"].selects != "url" || edit.flags["title"].selects != "" {
		t.Errorf("gh issue edit's --add-blocked-by selects %q, --parent %q and --title %q, want an issue's URL, an issue's URL, and none of the table's", edit.flags["add-blocked-by"].selects, edit.flags["parent"].selects, edit.flags["title"].selects)
	}
	if create := read("repo", "create"); kind(create.slots[0]) != "new-repository" {
		t.Errorf("gh repo create's argument reads as %+v, want the repository it creates", create.slots)
	}
	if clone := read("repo", "clone"); !clone.passthrough || len(clone.slots) != 2 || kind(clone.slots[0]) != "repository" || kind(clone.slots[1]) != "" {
		t.Errorf("gh repo clone reads as %+v, want a repository slot, a directory and a -- passthrough", clone)
	}
	if transfer := read("issue", "transfer"); len(transfer.slots) != 2 || kind(transfer.slots[1]) != "second-repository" {
		t.Errorf("gh issue transfer reads as %+v, want its destination a second repository", transfer.slots)
	}
	if browse := read("browse"); !browse.flags["commit"].value || !browse.flags["commit"].optional || browse.shorts['c'] != "commit" || kind(browse.slots[0]) != "number" {
		t.Errorf("gh browse reads as %+v, want -c, --commit taking a value only after = and an argument naming a repository only as OWNER/REPO#N", browse)
	}
	if status := read("auth", "status"); status.shorts['h'] != "hostname" {
		t.Errorf("gh auth status's -h reads as %q, want --hostname", status.shorts['h'])
	}
	if edit := read("issue", "edit"); len(edit.slots) != 1 || kind(edit.slots[0]) != "url" || edit.slots[0].variadic || !edit.slots[0].takesMany() {
		t.Errorf("gh issue edit reads as %+v, want one URL-capable slot of one or more arguments spelled with no ...", edit.slots)
	}
	if search := read("search", "issues"); search.readsGHRepo() || kind(search.slots[0]) != "query" {
		t.Errorf("gh search issues reads as %+v, want a query and no inherited -R", search)
	}
	if set := read("secret", "set"); set.flags["repos"].placeholder != "repositories" || !set.flags["org"].value || kind(set.slots[0]) != "" {
		t.Errorf("gh secret set reads as %+v, want --repos repositories, --org taking a value and a secret's name", set)
	}
	drift, err := ghroutetest.EditedHelp(savedGhHelp, ghroutetest.SpellingItsURLOtherwise)([]string{"pr", "view"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd, err := parseGhHelp([]string{"pr", "view"}, drift); err != nil || kind(cmd.slots[0]) != "unknown <URL>" {
		t.Errorf("pr view's help spelling <URL> reads as %+v, %v; want its argument of a kind this does not know", cmd.slots, err)
	}

	co, err := savedGhHelp([]string{"co"})
	if err != nil {
		t.Fatal(err)
	}
	prView := []string{"pr", "view"}
	for _, tc := range []struct {
		name string
		path []string
		help string
		want string
	}{
		{"no usage", prView, "FLAGS\n  --help   Show help for command\n", "names no usage"},
		{"no --help", prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  -w, --web   Open\n", "lists no --help"},
		{"an option line it cannot read", prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  --help   Show help\n  -w --web   Open\n", "cannot read the options"},
		{"a line of an options section naming no option", prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  --help   Show help\n           for this command\n", "names no option"},
		{"an option at the margin of an options section", prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  --help   Show help\n-w, --web   Open\n", "at the margin of its FLAGS"},
		{"an optional-value marker that names no type", prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  --help   Show help\n  -w, --web [=true|false]   Open\n", "optional-value marker names no type"},
		{"an option listed twice", prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  -w, --web   Open\n\nINHERITED FLAGS\n  --help   Show help\n  --web string   Open\n", "lists --web again"},
		{"a shorthand listed twice", prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  --help   Show help\n  -w, --web   Open\n  -w, --watch   Watch\n", "already gives --web"},
		{"an option line marking its option boolean and naming a value", prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  --help   Show help\n  -w, --web[=true|false] string   Open\n", "marks its option boolean and names a value"},
		{"a parent's help", prView, "USAGE\n  gh pr <command> [flags]\n", "gh has no command"},
		{"gh co's, pr checkout's, which lists no alias co", []string{"co"}, co, "is a gh alias"},
		{"a passthrough leaf passing on something it does not know", []string{"repo", "archive"}, "USAGE\n  gh repo archive [<repository>] [-- <git-args>...]\n\nFLAGS\n  --help   Show help\n", `passes "<git-args>" on after ` + "`--`"},
	} {
		if _, err := parseGhHelp(tc.path, tc.help); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseGhHelp of %s = %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
	if count, err := parseGhHelp(prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  -v, --verbose count   More\n\nINHERITED FLAGS\n  --help   Show help\n"); err != nil || !count.flags["verbose"].uncertain {
		t.Errorf("a count option reads as %+v, %v; want an option whose arity its help does not say", count.flags, err)
	}
	if later, err := parseGhHelp(prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  --mirror string   Read the pull request from this repository instead\n  --notes string   Notes\n\nINHERITED FLAGS\n  --help   Show help\n"); err != nil || later.flags["mirror"].selects != "unknown" || later.flags["notes"].selects != "" {
		t.Errorf("a later gh's options read as %+v, %v; want --mirror, which names a repository, unknown and --notes none of the table's", later.flags, err)
	}
	// A later gh that renames `<gitflags>` keeps a clone leaf's passed-on flags judged
	// (ghCloneLeaves), so round 2's clone route cannot reopen through a new name; the same
	// rename on a leaf that clones nothing is refused above. Control: today's name is judged.
	for _, tc := range []struct{ path, help string }{
		{"repo clone", "USAGE\n  gh repo clone <repository> [<directory>] [-- <git-args>...]\n\nFLAGS\n  --help   Show help\n"},
		{"repo fork", "USAGE\n  gh repo fork [<repository>] [-- <git-flags>...] [flags]\n\nFLAGS\n  --clone   Clone the fork\n  --help   Show help\n"},
		{"gist clone", "USAGE\n  gh gist clone <gist> [<directory>] [-- <args>...]\n\nFLAGS\n  --help   Show help\n"},
		{"repo clone", "USAGE\n  gh repo clone <repository> [<directory>] [-- <gitflags>...]\n\nFLAGS\n  --help   Show help\n"},
	} {
		cmd, err := parseGhHelp(strings.Fields(tc.path), tc.help)
		if err != nil || !cmd.passesGitFlags {
			t.Errorf("%s's help %q reads as passesGitFlags=%v, %v; want its passed-on arguments judged as git clone's flags", tc.path, tc.help, cmd.passesGitFlags, err)
			continue
		}
		if _, err := cmd.parse([]string{"acme/widgets", "--", "-c", "http.https://github.com/.sslVerify=false"}); err == nil || !strings.Contains(err.Error(), `hands "-c" to git clone`) {
			t.Errorf("%s under the help %q took -- -c ...: %v, want it refused", tc.path, tc.help, err)
		}
	}
}

// Every argument gh's usages name, across every command its reference lists, is of a kind the
// tables know, but for the few TargetOf refuses on purpose: what `gh copilot` and `gh extension
// exec` hand another program, codespace arguments of no fixed shape, and gh api's endpoint, which
// it reads itself. So the pinned gh refuses no command for an argument this does not know, and a
// later gh's new spelling is refused rather than read as naming nothing. Each name's cardinality
// holds too: a plural name (`{<numbers> | <urls>}`, one or more issues) is one the tables take as
// one or more arguments even with no `...`, and every slot taking more than one fits one argument
// more than the usage has slots, wherever the slot stands.
func TestEveryArgumentGhsReferenceNamesIsOfAKnownKind(t *testing.T) {
	refused := map[string]bool{"args": true, "command": true, "flags": true, "...": true, "public": true, "private": true, "org": true, "endpoint": true}
	for name := range refused {
		if _, known := ghSlotKinds[name]; known {
			t.Errorf("ghSlotKinds knows <%s>, which TargetOf refuses on purpose", name)
		}
	}
	// The one name gh spells ending in "s" that stands for one argument.
	singular := map[string]bool{"alias": true}
	leaves, many := 0, 0
	for _, usage := range ghReferenceUsages(t) {
		if ghUsageNamesSubcommands(usage.args) {
			continue
		}
		leaves++
		spelled := "gh " + strings.Join(usage.path, " ") + " " + usage.args
		slots, _ := ghSlots(usage.path, usage.args)
		for _, slot := range slots {
			for _, arg := range slot.args {
				switch {
				case !arg.known && !refused[arg.name]:
					t.Errorf("`%s` names <%s>, of no kind the tables know", spelled, arg.name)
				case arg.known && strings.HasSuffix(arg.name, "s") && !singular[arg.name] && !arg.kind.many:
					t.Errorf("`%s` names <%s>, a plural, which the tables take as one argument", spelled, arg.name)
				}
			}
			if slot.takesMany() {
				many++
				if placed, ok := (&ghCommand{slots: slots}).slotsFor(len(slots) + 1); !ok || !slices.ContainsFunc(placed, func(s ghSlot) bool { return slices.Equal(s.args, slot.args) }) {
					t.Errorf("`%s` takes one or more arguments in %v, and %d arguments do not fit its usage there: %v", spelled, slot.args, len(slots)+1, placed)
				}
			}
		}
	}
	if leaves < 150 || many < 10 {
		t.Errorf("gh's reference lists %d commands this reads and %d arguments taking more than one, want every one of its at least 150 and 10", leaves, many)
	}
}

// Every option of every command gh's reference lists whose help mentions a repository, an owner,
// an organization or a URL is one ghOptionKinds holds, as a selector or as naming nothing, so the
// pinned gh refuses no command for an option this does not know, and an option a later gh adds
// that names one is refused rather than read as naming nothing. No entry names nothing while its
// placeholder is a repository's, an owner's or an organization's.
func TestEveryOptionGhsReferenceNamesARepositoryWithIsInTheTable(t *testing.T) {
	options, mentioning := 0, 0
	for _, usage := range ghReferenceUsages(t) {
		for _, line := range usage.flags {
			cmd := ghCommand{flags: map[string]ghFlag{}, shorts: map[byte]string{}}
			if err := cmd.addFlag(line); err != nil {
				t.Fatalf("gh %s: %v", strings.Join(usage.path, " "), err)
			}
			for name, flag := range cmd.flags {
				options++
				if flag.value && (ghMentionsSelector.MatchString(flag.description) || ghMentionsSelector.MatchString(flag.placeholder)) {
					mentioning++
				}
				if kind := ghOptionKind(usage.path, name, flag); kind == "unknown" {
					t.Errorf("gh %s's --%s %s (%q) names a repository, an owner or a URL, and ghOptionKinds holds no kind for it", strings.Join(usage.path, " "), name, flag.placeholder, flag.description)
				}
			}
		}
	}
	for key, kind := range ghOptionKinds {
		if kind == "nothing" && regexp.MustCompile(`(?i)repo|owner|org`).MatchString(key[strings.LastIndex(key, " ")+1:]) {
			t.Errorf("ghOptionKinds names %q as naming nothing, while its placeholder names a repository or an owner", key)
		}
	}
	if options < 1000 || mentioning < 50 {
		t.Errorf("gh's reference lists %d options and %d naming a repository, an owner or a URL, want every one of its at least 1000 and 50", options, mentioning)
	}
}

// ghReferenceUsage is one command of gh's reference (testdata/gh-help/reference.txt): its path,
// the arguments its usage names after it, and its option lines.
type ghReferenceUsage struct {
	path  []string
	args  string
	flags []string
}

// ghReferenceUsages reads every command of gh's saved reference.
func ghReferenceUsages(t *testing.T) []ghReferenceUsage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ghHelpDir, "reference.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var usages []ghReferenceUsage
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "#") {
			words := strings.Fields(strings.Trim(strings.TrimSpace(strings.TrimLeft(line, "#")), "`"))
			if len(words) < 2 || words[0] != "gh" {
				continue
			}
			n := 1
			for n < len(words) && ghCommandWord.MatchString(words[n]) {
				n++
			}
			usages = append(usages, ghReferenceUsage{path: words[1:n], args: strings.Join(words[n:], " ")})
			continue
		}
		if len(usages) > 0 && ghFlagLine.MatchString(line) {
			usages[len(usages)-1].flags = append(usages[len(usages)-1].flags, line)
		}
	}
	return usages
}

// TargetOf reads the owner a gh command targets from every selector gh's own help names, and
// fails closed: an argument it cannot place is refused, naming it, and so is a command naming two
// owners; only a command naming no repository at all is not named, once the working directory's
// git remotes name no owner but the token's, and a command gh only prints the help or version of
// sends nothing. GH_REPO names the owner only when nothing explicit does, and only for a leaf that
// reads it. A read naming only owners the bundle holds no directory for and no repository a
// configured rename moves, read in a working directory whose remotes name no github.com
// repository for it, runs with GH_CONFIG_DIR's directory (R4 of dispatch://INFRA-3399); every
// other call naming such an owner is routed and refused as with no R4. The rows that read no git
// remote are ghroutetest.FailsClosedCalls, read under savedGhHelp, and
// ghroutetest.HBeforeTheCommandCalls, a -h before the command's name, read under gh 2.98.0's help
// of every command, saved whole; `legion gh`'s tests also resolve both through its keep of gh's
// help. The rows below read the working directory's, or a bundle on disk.
func TestGhTargetOfFailsClosed(t *testing.T) {
	walked := ghroutetest.HBeforeTheCommandCalls
	for _, table := range []struct {
		calls []ghroutetest.TargetCall
		help  func([]string) (string, error)
	}{
		{calls: ghroutetest.FailsClosedCalls, help: savedGhHelp},
		{calls: walked.Calls, help: savedGhHelpOf(t, walked.Version)},
	} {
		for _, tc := range table.calls {
			target, err := TargetOf(tc.Argv, tc.Env, tc.Help(table.help), noGitRepository, ghroutetest.OwnersOwnDirectories)
			switch {
			case tc.Err != "":
				if err == nil || !strings.Contains(err.Error(), tc.Err) {
					t.Errorf("TargetOf(%q, %v) = %+v, %v; want a refusal containing %q", tc.Argv, tc.Env, target, err, tc.Err)
				}
			case err != nil || target.Owner != tc.Owner:
				t.Errorf("TargetOf(%q, %v) = %+v, %v; want the owner %q", tc.Argv, tc.Env, target, err, tc.Owner)
			}
		}
	}
	// A pane of the issue owner acme, whose GH_CONFIG_DIR is acme's directory of the bundle.
	pane := map[string]string{"GH_CONFIG_DIR": "/owners/acme", "LEGION_GH_OWNERS_DIR": "/owners"}
	paneWith := func(name, value string) map[string]string {
		env := map[string]string{name: value}
		maps.Copy(env, pane)
		return env
	}
	remotes := func(listed ...Remote) func() ([]Remote, error) {
		return func() ([]Remote, error) { return listed, nil }
	}
	acmeClone := Remote{name: "origin", url: "https://github.com/Acme/widgets.git"}
	sjawharClone := Remote{name: "origin", url: "https://github.com/sjawhar/legion"}
	// The marked fork checkout of dispatch://INFRA-3399: acme's fork, whose upstream is sjawhar's
	// and whose origin carries gh's default marker, followed by more lines.
	forkUpstream := Remote{name: "upstream", url: "git@github.com:sjawhar/legion.git"}
	originBase := markerLine("remote.origin.gh-resolved base")
	markedFork := func(more ...Remote) func() ([]Remote, error) {
		return remotes(append([]Remote{acmeClone, forkUpstream, originBase}, more...)...)
	}
	for _, tc := range []struct {
		argv    []string
		env     map[string]string
		remotes func() ([]Remote, error)
		owner   string // "" with no refusal: not named
		err     string // a refusal containing this
	}{
		// The final gates' at 96e1672cd, as round 4 reads them. A command naming no repository acts
		// on the working directory's: origin's fetch URL alone names its owner where the leaf
		// honours gh's default marker and the marker makes origin's repository gh's own (the
		// marked fork checkout's rows below); otherwise every github.com remote and gh-resolved
		// setting names an owner, and they must name one.
		{argv: []string{"pr", "view", "1"}, env: pane, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"pr", "view", "1"}, env: pane, remotes: remotes(acmeClone, originBase), owner: "acme"},
		{argv: []string{"pr", "view", "1"}, env: pane, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"repo", "view"}, env: pane, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"api", "repos/{owner}/{repo}"}, env: pane, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(Remote{name: "origin", url: "https://github.com/octocat/hello"}), owner: "octocat"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, Remote{name: "upstream", url: "git@github.com:sjawhar/legion.git"}), err: `more than one owner, acme (the working directory's remote "origin" (https://github.com/Acme/widgets.git)) and sjawhar (the working directory's remote "upstream" (git@github.com:sjawhar/legion.git))`},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, markerLine("remote.origin.gh-resolved sjawhar/legion")), err: "sjawhar (the working directory's remote.origin.gh-resolved sjawhar/legion)"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, Remote{name: "fork", url: "ssh://git@ssh.github.com:443/Sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, Remote{name: "work", url: "git@gh-work:sjawhar/legion.git"}), err: "is an ssh remote whose host gh may read as github.com"},
		// A remote whose host holds a letter beyond ASCII is refused, not passed over as another
		// host's: gh lowers `gİthub.com` (U+0130) to github.com and acts on its repository.
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(Remote{name: "origin", url: "https://g\u0130thub.com/acme/widgets.git"}), err: "whose host holds a letter beyond ASCII"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, Remote{name: "mirror", url: "https://gitlab.example.com/sjawhar/legion"}, Remote{name: "local", url: "/srv/legion"}), owner: "acme"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_CONFIG_DIR": "/owners", "LEGION_GH_OWNERS_DIR": "/owners"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"pr", "list"}, env: pane, remotes: func() ([]Remote, error) { return nil, fmt.Errorf("bad config line 3") }, err: "cannot be read (bad config line 3)"},
		{argv: []string{"-R", "acme/widgets", "pr", "view", "1"}, env: pane, remotes: remotes(sjawharClone), owner: "acme"},
		{argv: []string{"pr", "list"}, env: paneWith("GH_REPO", "acme/widgets"), remotes: remotes(sjawharClone), owner: "acme"},
		{argv: []string{"pr", "view", "--help"}, env: pane, remotes: remotes(sjawharClone)},
		// R1 of dispatch://INFRA-3399, in the marked fork checkout unless a row says otherwise: its
		// conditions as unit rows, among them the remote lists real git cannot print (a push line
		// alone, two origin fetch lines) and the marker lines it writes in no config file (a dotted
		// key with no remote, a key with no dot). The leaf must honour the marker: a group gh builds
		// without it (secret) takes the first of gh's sorted remotes, upstream, and repo set-default
		// reads every remote's network unless --view or --unset is true, which a value that is no
		// boolean is not, the last value given each option deciding, as pflag reads it.
		{argv: []string{"secret", "set", "TOKEN", "--body", "dummy"}, remotes: markedFork(), err: "more than one owner"},
		{argv: []string{"repo", "set-default", "--view"}, remotes: markedFork(), owner: "acme"},
		{argv: []string{"repo", "set-default", "-v"}, remotes: markedFork(), owner: "acme"},
		{argv: []string{"repo", "set-default", "--unset"}, remotes: markedFork(), owner: "acme"},
		{argv: []string{"repo", "set-default", "-u"}, remotes: markedFork(), owner: "acme"},
		{argv: []string{"repo", "set-default", "--view=maybe", "acme/widgets"}, remotes: markedFork(), err: "more than one owner"},
		{argv: []string{"repo", "set-default", "--view", "--view=false", "acme/widgets"}, remotes: markedFork(), err: "more than one owner"},
		{argv: []string{"repo", "set-default", "--view=false", "--view"}, remotes: markedFork(), owner: "acme"},
		{argv: []string{"repo", "set-default", "--unset", "--unset=false", "acme/widgets"}, remotes: markedFork(), err: "more than one owner"},
		{argv: []string{"repo", "set-default", "--unset=false", "--unset"}, remotes: markedFork(), owner: "acme"},
		// One marker line gh reads must be left: a dotted key gh gives origin, `base` or empty, is
		// a second, while a key with no dot, which names gh no remote, a key with no value and an
		// empty value on a remote other than origin, as gh names it, are none.
		{argv: []string{"pr", "list"}, remotes: markedFork(markerLine("remote.origin.fork.gh-resolved base")), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: markedFork(markerLine("origin base")), owner: "acme"},
		{argv: []string{"pr", "list"}, remotes: markedFork(markerLine("remote.origin.gh-resolved")), owner: "acme"},
		{argv: []string{"pr", "list"}, remotes: markedFork(markerLine("remote.origin.fork.gh-resolved ")), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: markedFork(markerLine("remote.upstream.x.gh-resolved ")), owner: "acme"},
		// That line must be origin's `base`: `origin base` alone is no marker gh reads.
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, forkUpstream, markerLine("origin base")), err: "more than one owner"},
		// origin must have one fetch line, whose URL gh keeps as github.com's with nothing the
		// wrapper cannot read: two fetch lines, a push line alone and an ssh URL are refused; a
		// port is no part of gh's host; a local path gh drops, taking upstream's repository, so
		// every remote counts and sjawhar is the one owner. gh api naming nothing reads the marker.
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, Remote{name: "origin", url: "https://github.com/acme/other.git"}, forkUpstream, originBase), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(Remote{name: "origin", url: "https://github.com/Acme/widgets.git", push: true}, forkUpstream, originBase), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(Remote{name: "origin", url: "ssh://git@github.com/acme/widgets"}, forkUpstream, originBase), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(Remote{name: "origin", url: "https://github.com:443/acme/widgets"}, forkUpstream, originBase), owner: "acme"},
		{argv: []string{"pr", "list"}, remotes: remotes(Remote{name: "origin", url: "/srv/widgets"}, forkUpstream, originBase), owner: "sjawhar"},
		{argv: []string{"api", "graphql", "-f", "query=q"}, remotes: markedFork(), owner: "acme"},
		// Round 4's: a remote URL gh reads as github.com's whatever its scheme, as gh's own parser
		// reads it (deep 2, quality A, final B1).
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, Remote{name: "upstream", url: "git+https://github.com/sjawhar/legion.git"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, Remote{name: "upstream", url: "ftp://github.com/sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, Remote{name: "upstream", url: "ftps://github.com/sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, Remote{name: "upstream", url: "file://github.com/sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, Remote{name: "upstream", url: "github.com://sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, Remote{name: "upstream", url: "git@github.com/sjawhar:legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(Remote{name: "origin", url: "git+https://github.com/sjawhar/legion"}), owner: "sjawhar"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, Remote{name: "upstream", url: "GIT+HTTPS://github.com/sjawhar/legion"}), err: "is an ssh remote whose host gh may read as github.com"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, Remote{name: "upstream", url: "https://github.com/%zz/legion"}), err: "is a URL this cannot read as gh does"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, Remote{name: "upstream", url: "https://github.com/sjawhar/legion/extra"}), err: "names no OWNER/REPO"},
		// Round 4's: an option or another repository naming one owner while gh takes the command's
		// own repository from the working directory or GH_REPO (deep 1, quality B, final B2).
		{argv: []string{"pr", "comment", "5", "--body", "https://github.com/sjawhar/legion/pull/2"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"issue", "edit", "5", "--add-blocked-by", "https://github.com/sjawhar/legion/issues/9"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"pr", "comment", "1", "--body", "https://github.com/acme/widgets/pull/9"}, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"pr", "comment", "1", "--body", "https://github.com/sjawhar/legion/pull/2"}, remotes: remotes(Remote{name: "origin", url: "https://github.com/octocat/hello"}), owner: "octocat"},
		{argv: []string{"issue", "close", "3", "--duplicate-of", "https://github.com/acme/widgets/issues/1"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"issue", "edit", "3", "--add-sub-issue", "https://github.com/acme/widgets/issues/1"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"issue", "create", "--title", "t", "--body", "b", "--parent", "https://github.com/acme/widgets/issues/1"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"repo", "sync", "--source", "acme/widgets"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"issue", "develop", "3", "--branch-repo", "acme/widgets", "--name", "x"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"repo", "fork", "--org", "sjawhar", "--clone=false", "--remote=false"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"repo", "fork", "acme/widgets", "--org", "sjawhar", "--clone=false"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion"}, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion"}, remotes: remotes(acmeClone), err: "more than one owner"},
		// Round 4's minor findings: options mentioning URLs or a team, and project link and unlink,
		// which read GH_REPO with -R a plain string (deep 3, deep 4).
		{argv: []string{"issue", "create", "--title", "t", "--body", "b", "--blocked-by", "1,https://github.com/sjawhar/legion/issues/5"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"issue", "create", "--title", "t", "--body", "b", "--blocking", "https://github.com/acme/widgets/issues/5"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"project", "link", "1"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		// Round 5's: gh takes a passthrough leaf's repository from the first argument even after
		// `--` (deep 1); `repo set-default` reads every remote's network whatever its argument
		// names (deep 2, quality 1); project link and unlink read no repository of their own
		// with --team (quality 2); a pr command reads OWNER/REPO#N and a URL other than a pull
		// request's as a branch of its own repository (final F1).
		{argv: []string{"repo", "fork", "--", "sjawhar/legion"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"repo", "clone", "--", "sjawhar/legion"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"repo", "set-default", "sjawhar/legion"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"repo", "set-default", "sjawhar/legion"}, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"project", "link", "1", "--team", "sjawhar/core"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"pr", "comment", "sjawhar/legion#2", "--body", "hi"}, remotes: remotes(Remote{name: "origin", url: "https://github.com/octocat/hello"}), owner: "octocat"},
		{argv: []string{"pr", "view", "sjawhar/legion#2"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/issues/5"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/tree/x"}, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/2/files"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/5", "--title", "x"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		// `gh agent-task` sends the token to the Copilot API host GitHub names, so it is refused
		// whatever it names, a pull request's URL among them, before any repository is read.
		{argv: []string{"agent-task", "view", "https://github.com/sjawhar/legion/pull/1"}, remotes: remotes(acmeClone), err: "`gh agent-task` sends the token"},
	} {
		target, err := TargetOf(tc.argv, tc.env, savedGhHelp, tc.remotes, ghroutetest.OwnersOwnDirectories)
		switch {
		case tc.err != "":
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("TargetOf(%q, %v) = %+v, %v; want a refusal containing %q", tc.argv, tc.env, target, err, tc.err)
			}
		case err != nil || target.Owner != tc.owner:
			t.Errorf("TargetOf(%q, %v) = %+v, %v; want the owner %q", tc.argv, tc.env, target, err, tc.owner)
		}
	}

	// R4 of dispatch://INFRA-3399, against a bundle written as the daemon writes one
	// (ghconfig.WriteBundle) and routed by DirFor: directories for acme and sjawhar and none for
	// any other owner, which DirFor answers with a bare OwnerNotServedError; and two configured
	// renames, octocat/moved -> sjawhar/moved, whose `to` owner has a directory, and sjawhar/gone
	// -> nobody/gone, whose `to` owner has none, which it refuses naming the pair around that
	// error. The pane is acme's over that bundle (bundlePane). A call that writes nothing, whose
	// every item is an owner with no directory and whose working directory's remotes name no
	// github.com repository for it, runs with GH_CONFIG_DIR's directory: in a working directory
	// that is no git repository, and in a checkout whose remotes the reader reads for no call
	// naming its repository, acme's clone and the marked fork checkout. Every other call gets the
	// routing and the refusal it gets with no fallback (selectors.dir): a write, a GraphQL
	// mutation, a read beside an owner the bundle serves, a read with no GH_CONFIG_DIR, a read
	// whose remotes were read for it (whether or not the remote's entry was kept), and a read of a
	// pair's repository.
	root := filepath.Join(t.TempDir(), "gh")
	expires := time.Now().Add(time.Hour)
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{
		Default: "acme",
		Dirs: map[string]ghconfig.Rendered{
			"acme":    ghconfig.Render("ghs_acme_write", "implement", expires),
			"sjawhar": ghconfig.Render("ghs_sjawhar_write", "implement", expires),
		},
		Pairs: []ghrepo.Rename{ghrepo.MustRename("octocat/moved", "sjawhar/moved"), ghrepo.MustRename("sjawhar/gone", "nobody/gone")},
	}); err != nil {
		t.Fatal(err)
	}
	bundlePane := map[string]string{"GH_CONFIG_DIR": ghconfig.Dir(root, "acme"), "LEGION_GH_OWNERS_DIR": root}
	routed := func(owner, name string) (string, error) { return DirFor(root, owner, name) }
	const unserved = `the role's GitHub App is not installed on "octocat"`
	octocatClone := Remote{name: "origin", url: "https://github.com/octocat/hello.git"}
	for _, tc := range []struct {
		argv    []string
		env     map[string]string
		remotes func() ([]Remote, error) // a working directory that is no git repository when nil
		dir     string                   // the directory the command runs with, where it is not refused
		err     string                   // a refusal containing this
	}{
		// Reads naming two owners with no directory, by a search query's qualifiers and by a
		// GraphQL query's variables.
		{argv: []string{"search", "issues", "owner:octocat", "org:hubot"}, env: bundlePane, dir: bundlePane["GH_CONFIG_DIR"]},
		{argv: []string{"api", "graphql", "-f", "owner=octocat", "-f", "org=hubot", "-f", "query=query($owner:String!,$org:String!){a:repositoryOwner(login:$owner){login} b:organization(login:$org){login}}"}, env: bundlePane, dir: bundlePane["GH_CONFIG_DIR"]},
		// Reads naming octocat/hello, by an argument, -R, gh api's endpoint and a GraphQL query's
		// variables, in acme's clone and in the marked fork checkout: the reader reads no remote
		// for a call naming its repository, so the checkout's remotes name none for it.
		{argv: []string{"repo", "view", "octocat/hello", "--json", "nameWithOwner"}, env: bundlePane, remotes: remotes(acmeClone), dir: bundlePane["GH_CONFIG_DIR"]},
		{argv: []string{"repo", "view", "octocat/hello", "--json", "nameWithOwner"}, env: bundlePane, remotes: markedFork(), dir: bundlePane["GH_CONFIG_DIR"]},
		{argv: []string{"pr", "list", "-R", "octocat/hello", "--json", "number"}, env: bundlePane, remotes: remotes(acmeClone), dir: bundlePane["GH_CONFIG_DIR"]},
		{argv: []string{"pr", "list", "-R", "octocat/hello", "--json", "number"}, env: bundlePane, remotes: markedFork(), dir: bundlePane["GH_CONFIG_DIR"]},
		{argv: []string{"api", "repos/octocat/hello"}, env: bundlePane, remotes: remotes(acmeClone), dir: bundlePane["GH_CONFIG_DIR"]},
		{argv: []string{"api", "repos/octocat/hello"}, env: bundlePane, remotes: markedFork(), dir: bundlePane["GH_CONFIG_DIR"]},
		{argv: []string{"api", "graphql", "-f", "owner=octocat", "-f", "name=hello", "-f", "query=query($owner:String!,$name:String!){repository(owner:$owner,name:$name){nameWithOwner}}"}, env: bundlePane, remotes: remotes(acmeClone), dir: bundlePane["GH_CONFIG_DIR"]},
		{argv: []string{"api", "graphql", "-f", "owner=octocat", "-f", "name=hello", "-f", "query=query($owner:String!,$name:String!){repository(owner:$owner,name:$name){nameWithOwner}}"}, env: bundlePane, remotes: markedFork(), dir: bundlePane["GH_CONFIG_DIR"]},
		// A read for which gh reads the working directory's remotes and finds none on github.com
		// still runs with GH_CONFIG_DIR's directory: only a github.com remote read for it counts.
		{argv: []string{"pr", "view", "https://github.com/octocat/hello/tree/main", "--json", "number"}, env: bundlePane, remotes: remotes(Remote{name: "origin", url: "https://gitlab.example.com/acme/widgets.git"}), dir: bundlePane["GH_CONFIG_DIR"]},
		// A read where no GH_CONFIG_DIR names the directory a command naming no owner runs with.
		{argv: []string{"repo", "view", "octocat/hello", "--json", "name"}, err: unserved},
		// A write naming an owner with no directory, and a GraphQL mutation naming one.
		{argv: []string{"issue", "create", "-R", "octocat/hello", "--title", "t", "--body", "b"}, env: bundlePane, err: unserved},
		{argv: []string{"api", "graphql", "-f", "owner=octocat", "-f", `query=mutation{addStar(input:{starrableId:"x"}){clientMutationId}}`}, env: bundlePane, err: unserved},
		// A read naming an owner with no directory beside one the bundle serves.
		{argv: []string{"api", "graphql", "-f", "owner=octocat", "-f", "org=sjawhar", "-f", "query={viewer{login}}"}, env: bundlePane, err: unserved},
		// Reads for which gh takes a repository from the working directory's remotes: an owner
		// named by a GraphQL query's variable beside a {repo} gh fills from the remote; a URL gh's
		// pr finder reads as a branch of the command's own repository, where the remote is acme's,
		// and where it is the URL's own repository, whose entry add drops; and an endpoint's
		// {owner}, in a clone of the repository the endpoint names.
		{argv: []string{"api", "graphql", "-F", "owner=octocat", "-F", "name={repo}", "-f", "query=query($owner:String!,$name:String!){repository(owner:$owner,name:$name){id}}"}, env: bundlePane, remotes: remotes(octocatClone), err: unserved},
		{argv: []string{"pr", "view", "https://github.com/octocat/hello/tree/main", "--json", "number"}, env: bundlePane, remotes: remotes(acmeClone), err: unserved},
		{argv: []string{"pr", "view", "https://github.com/octocat/hello/tree/main", "--json", "number"}, env: bundlePane, remotes: remotes(octocatClone), err: unserved},
		{argv: []string{"api", "repos/octocat/hello/pulls?head={owner}:x"}, env: bundlePane, remotes: remotes(octocatClone), err: unserved},
		// The repository a configured rename pair moves: refused naming the pair where its `to`
		// owner has no directory, though its `from` owner has one, and served from the `to`
		// owner's directory where that owner has one, though its `from` owner has none.
		{argv: []string{"repo", "view", "sjawhar/gone", "--json", "name"}, env: bundlePane, err: "the configured rename sjawhar/gone -> nobody/gone"},
		{argv: []string{"repo", "view", "octocat/moved", "--json", "name"}, env: bundlePane, dir: ghconfig.Dir(root, "sjawhar")},
	} {
		listed := tc.remotes
		if listed == nil {
			listed = noGitRepository
		}
		target, err := TargetOf(tc.argv, tc.env, savedGhHelp, listed, routed)
		switch {
		case tc.err != "":
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("TargetOf(%q, %v) = %+v, %v; want a refusal containing %q", tc.argv, tc.env, target, err, tc.err)
			}
		case err != nil || target.Dir != tc.dir:
			t.Errorf("TargetOf(%q, %v) = %+v, %v; want the directory %q", tc.argv, tc.env, target, err, tc.dir)
		}
	}
}

// Beside the owner, TargetOf reads the repository a command names, which is the repository the
// leak guard is told the call acts on (leakguard.GhRequest.Repo): a `<repository>` argument, -R,
// a pull request's URL, a search repo: qualifier, `gh api`'s repos/<owner>/<repo>/… endpoint,
// GH_REPO and the working directory's remotes each name one. The name is keyed as gh reads a name
// of it (ghconfig.RepositoryName), so `Legion` and `widgets.git` are `legion` and `widgets`, and
// two spellings of one repository are one. They must all name the one repository: a command
// naming none, one naming two of the one owner, and one where a selector named the owner alone —
// an `<owner>` argument, orgs/<owner>/ or users/<owner>/, an owner:/org:/user: qualifier, a
// team, an owner option — name none, and the command runs with the owner's directory all the
// same. The rows that read no git remote are ghroutetest.OneRepositoryCalls, which `legion gh`'s
// tests also resolve through its keep of gh's help; the rows below read the working directory's.
func TestGhTargetOfNamesTheOneRepositoryEverySelectorNames(t *testing.T) {
	for _, tc := range ghroutetest.OneRepositoryCalls {
		target, err := TargetOf(tc.Argv, tc.Env, tc.Help(savedGhHelp), noGitRepository, ghroutetest.OwnersOwnDirectories)
		if err != nil || target.Owner != tc.Owner || target.Repo != tc.Repo {
			t.Errorf("TargetOf(%q, %v) = %+v, %v; want the owner %q and the repository %q", tc.Argv, tc.Env, target, err, tc.Owner, tc.Repo)
		}
	}
	remotes := func(listed ...Remote) func() ([]Remote, error) {
		return func() ([]Remote, error) { return listed, nil }
	}
	acmeClone := Remote{name: "origin", url: "https://github.com/Acme/widgets.git"}
	for _, tc := range []struct {
		argv    []string
		remotes func() ([]Remote, error)
		owner   string
		repo    string // "" where the selectors name no one repository
	}{
		// One repository, named by the working directory's remotes.
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone), owner: "acme", repo: "widgets"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, markerLine("remote.origin.gh-resolved base")), owner: "acme", repo: "widgets"},
		{argv: []string{"pr", "list"}, remotes: remotes(markerLine("remote.origin.gh-resolved acme/widgets")), owner: "acme", repo: "widgets"},
		// The same repository named twice is still the one: the leaf's own argument beside the
		// working directory's remote.
		{argv: []string{"pr", "view", "1", "-R", "acme/widgets"}, remotes: remotes(acmeClone), owner: "acme", repo: "widgets"},
		// Two spellings of the one repository are one repository, not two: a remote's clone URL
		// carries the `.git` the directory's name has not.
		{argv: []string{"pr", "view", "1", "-R", "acme/Widgets"}, remotes: remotes(acmeClone), owner: "acme", repo: "widgets"},
		// Two repositories of the one owner: the command reaches both, so it runs as the owner.
		{argv: []string{"issue", "edit", "5", "--add-blocked-by", "https://github.com/acme/other/issues/9"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"pr", "view", "https://github.com/acme/other/tree/x"}, remotes: remotes(acmeClone), owner: "acme"},
	} {
		target, err := TargetOf(tc.argv, nil, savedGhHelp, tc.remotes, ghroutetest.OwnersOwnDirectories)
		if err != nil || target.Owner != tc.owner || target.Repo != tc.repo {
			t.Errorf("TargetOf(%q) = %+v, %v; want the owner %q and the repository %q", tc.argv, target, err, tc.owner, tc.repo)
		}
	}
}

// A read naming an owner the bundle holds no gh directory for, through any selector the selector
// reader (fromCommand, fromOwnRepository) counts, runs with the directory a call naming no owner
// runs with, GH_CONFIG_DIR's (R4 of dispatch://INFRA-3399): octocat's repository or account, read
// through each, runs with acme's directory, the pane's, and is not refused, and the same read
// naming sjawhar, whom the bundle serves, runs with sjawhar's directory as selectors.dir routes a
// served owner. R4 is a rule over a class, so the class is read from the code and from gh's own
// saved help rather than from memory, at both saved gh releases: each kind of argument
// (ghSlot.kind) and of option (ghOptionKind) the help of a leaf of readLeaves gives, but the
// kinds that select nothing, must have a row reading octocat through it, and a leaf an older help
// does not list is skipped at that release. To those come, by name, the members no help lists:
// the endpoint forms of gh api writesNothing admits, a GraphQL query's owner variables
// (graphQLQueryRead), a search query's repo:, owner:, user: and org: qualifiers, and GH_REPO.
// Two members the selector reader counts are no read writesNothing admits, so they keep the
// OwnerNotServedError refusal of octocat: a whole URL as gh api's endpoint, since
// apiRESTEndpoint admits a relative path alone, and a search endpoint's q given as a field,
// since apiReadOptions holds neither -f nor -F, and gh sends a call with a field as a POST
// (gh api's help: "adding request parameters will automatically switch the request method to
// `POST`").
func TestGhTargetOfRoutesEverySelectorKindOfARead(t *testing.T) {
	// The pane of the issue owner acme, whose bundle under /owners serves acme and sjawhar and
	// holds no directory for octocat, which dirFor answers as DirFor answers an owner it holds
	// none for: a bare OwnerNotServedError, wrapping nothing.
	pane := map[string]string{"GH_CONFIG_DIR": "/owners/acme", "LEGION_GH_OWNERS_DIR": "/owners"}
	dirFor := func(owner, _ string) (string, error) {
		if owner == "acme" || owner == "sjawhar" {
			return ghconfig.Dir("/owners", owner), nil
		}
		return "", &leakguard.OwnerNotServedError{Owner: owner, OwnersDir: "/owners"}
	}
	const refusal = `the role's GitHub App is not installed on "octocat"`
	// Each row is octocat's read, with the leaf whose path it is typed with and the kind it reads
	// octocat through: an argument's or an option's as the help's kinds are named below, or, for a
	// member no help lists, what it is. GH_REPO, where the row sets it, joins the pane's
	// environment. The read naming sjawhar is the same row with sjawhar in octocat's place.
	// refused marks a member the selector reader counts that is no read writesNothing admits.
	type read struct {
		leaf, kind string
		argv       []string
		env        map[string]string
		refused    bool
	}
	const viaRepo = "option --repo (repository)"
	reads := []read{
		{leaf: "pr view", kind: "argument url", argv: []string{"pr", "view", "https://github.com/octocat/hello/pull/1", "--json", "number"}},
		{leaf: "pr view", kind: viaRepo, argv: []string{"pr", "view", "1", "-R", "octocat/hello", "--json", "number"}},
		{leaf: "pr list", kind: viaRepo, argv: []string{"pr", "list", "-R", "octocat/hello"}},
		{leaf: "pr diff", kind: "argument url", argv: []string{"pr", "diff", "https://github.com/octocat/hello/pull/1"}},
		{leaf: "pr diff", kind: viaRepo, argv: []string{"pr", "diff", "1", "-R", "octocat/hello"}},
		{leaf: "pr checks", kind: "argument url", argv: []string{"pr", "checks", "https://github.com/octocat/hello/pull/1"}},
		{leaf: "pr checks", kind: viaRepo, argv: []string{"pr", "checks", "1", "-R", "octocat/hello"}},
		{leaf: "pr status", kind: viaRepo, argv: []string{"pr", "status", "-R", "octocat/hello"}},
		{leaf: "run view", kind: viaRepo, argv: []string{"run", "view", "1", "-R", "octocat/hello"}},
		{leaf: "run list", kind: viaRepo, argv: []string{"run", "list", "-R", "octocat/hello"}},
		{leaf: "run watch", kind: viaRepo, argv: []string{"run", "watch", "1", "-R", "octocat/hello"}},
		{leaf: "issue view", kind: "argument url", argv: []string{"issue", "view", "https://github.com/octocat/hello/issues/1", "--json", "number"}},
		{leaf: "issue view", kind: viaRepo, argv: []string{"issue", "view", "1", "-R", "octocat/hello"}},
		{leaf: "issue list", kind: viaRepo, argv: []string{"issue", "list", "-R", "octocat/hello"}},
		{leaf: "issue status", kind: viaRepo, argv: []string{"issue", "status", "-R", "octocat/hello"}},
		{leaf: "repo view", kind: "argument repository", argv: []string{"repo", "view", "octocat/hello", "--json", "name"}},
		{leaf: "repo list", kind: "argument owner", argv: []string{"repo", "list", "octocat", "--json", "name"}},
		{leaf: "release view", kind: viaRepo, argv: []string{"release", "view", "v1", "-R", "octocat/hello"}},
		{leaf: "release list", kind: viaRepo, argv: []string{"release", "list", "-R", "octocat/hello"}},
		{leaf: "workflow view", kind: viaRepo, argv: []string{"workflow", "view", "ci.yml", "-R", "octocat/hello"}},
		{leaf: "workflow list", kind: viaRepo, argv: []string{"workflow", "list", "-R", "octocat/hello"}},
		{leaf: "label list", kind: viaRepo, argv: []string{"label", "list", "-R", "octocat/hello"}},
		{leaf: "cache list", kind: viaRepo, argv: []string{"cache", "list", "-R", "octocat/hello"}},
		{leaf: "search code", kind: viaRepo, argv: []string{"search", "code", "hello", "--repo", "octocat/hello"}},
		{leaf: "search code", kind: "option --owner (owner)", argv: []string{"search", "code", "hello", "--owner", "octocat"}},
		{leaf: "search commits", kind: viaRepo, argv: []string{"search", "commits", "fix", "--repo", "octocat/hello"}},
		{leaf: "search commits", kind: "option --owner (owner)", argv: []string{"search", "commits", "fix", "--owner", "octocat"}},
		{leaf: "search issues", kind: viaRepo, argv: []string{"search", "issues", "bug", "--repo", "octocat/hello"}},
		{leaf: "search issues", kind: "option --owner (owner)", argv: []string{"search", "issues", "bug", "--owner", "octocat"}},
		{leaf: "search issues", kind: "option --project (owner/name)", argv: []string{"search", "issues", "--project", "octocat/1", "--json", "number"}},
		{leaf: "search prs", kind: viaRepo, argv: []string{"search", "prs", "fix", "--repo", "octocat/hello"}},
		{leaf: "search prs", kind: "option --owner (owner)", argv: []string{"search", "prs", "fix", "--owner", "octocat"}},
		{leaf: "search prs", kind: "option --project (owner/name)", argv: []string{"search", "prs", "--project", "octocat/1", "--json", "number"}},
		{leaf: "search repos", kind: "option --owner (owner)", argv: []string{"search", "repos", "--owner", "octocat", "--json", "name"}},
		// gh api's endpoint forms, the GraphQL variables naming an owner and GH_REPO, which no
		// help lists.
		{leaf: "api", kind: "endpoint repos/<owner>/<repo>/…", argv: []string{"api", "repos/octocat/hello/pulls"}},
		{leaf: "api", kind: "endpoint repos/<owner>/<repo>", argv: []string{"api", "repos/octocat/hello"}},
		{leaf: "api", kind: "endpoint orgs/<owner>", argv: []string{"api", "orgs/octocat"}},
		{leaf: "api", kind: "endpoint users/<owner>", argv: []string{"api", "users/octocat"}},
		{leaf: "api", kind: "GraphQL variable owner", argv: []string{"api", "graphql", "-f", "owner=octocat", "-f", "query=query($owner: String!) { repositoryOwner(login: $owner) { login } }"}},
		{leaf: "api", kind: "GraphQL variable org", argv: []string{"api", "graphql", "-f", "org=octocat", "-f", "query=query($org: String!) { organization(login: $org) { login } }"}},
		{leaf: "api", kind: "GraphQL variable organization", argv: []string{"api", "graphql", "-f", "organization=octocat", "-f", "query=query($organization: String!) { organization(login: $organization) { login } }"}},
		{leaf: "api", kind: "GraphQL variable login", argv: []string{"api", "graphql", "-f", "login=octocat", "-f", "query=query($login: String!) { user(login: $login) { login } }"}},
		{leaf: "pr list", kind: "GH_REPO", argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "octocat/hello"}},
		// The two members that are no read: octocat's gets the OwnerNotServedError refusal.
		{leaf: "api", kind: "endpoint https://api.github.com/…", argv: []string{"api", "https://api.github.com/repos/octocat/hello"}, refused: true},
		{leaf: "api", kind: "a search endpoint's q field", argv: []string{"api", "search/issues", "-f", "q=repo:octocat/hello"}, refused: true},
	}
	// A search query's qualifiers naming an owner, as the query words of every search leaf, and
	// as the q of gh api's search endpoint.
	for _, qualifier := range []string{"repo:octocat/hello", "owner:octocat", "user:octocat", "org:octocat"} {
		for _, leaf := range []string{"search code", "search commits", "search issues", "search prs", "search repos"} {
			reads = append(reads, read{leaf: leaf, kind: "argument query", argv: append(strings.Fields(leaf), qualifier)})
		}
		reads = append(reads, read{leaf: "api", kind: "endpoint search/<kind>?q=…", argv: []string{"api", "search/issues?q=" + qualifier}})
	}
	covered := map[[2]string]bool{}
	for _, r := range reads {
		covered[[2]string{r.leaf, r.kind}] = true
	}
	// The kinds no read leaf carries are out of R4's reach, since every leaf whose help gives one
	// is outside readLeaves, and so writes as writesNothing reads it: a repository the command
	// creates (new-repository: repo create's <name>), another repository it acts on beside its
	// own (second-repository: repo sync's, issue transfer's and label clone's), the repositories
	// of an --org (org-repositories: secret set's and variable set's --repos), an option naming an
	// issue or a pull request by URL (url: issue edit's --add-blocked-by, project item-add's
	// --url), and a host (host: --hostname, gh api's, auth's and attestation's). No row reads
	// octocat through one, and a read leaf or option that gives one fails here, to be given rows
	// and R4 a reading of it.
	outOfReach := map[string]bool{"org-repositories": true, "url": true, "host": true}
	for _, version := range []string{"2.98.0", "2.104.0"} {
		t.Run(version, func(t *testing.T) {
			help := savedGhHelpOf(t, version)
			// lists says whether this release's help lists path: whether its parent's help
			// lists its last word.
			lists := func(path []string) bool {
				parent, err := readGhCommand(path[:len(path)-1], help)
				if err != nil {
					t.Fatalf("gh %s: %v", version, err)
				}
				return parent.listed[path[len(path)-1]]
			}
			leaves := 0
			for _, leaf := range slices.Sorted(maps.Keys(readLeaves)) {
				path := strings.Fields(leaf)
				if !lists(path) {
					continue
				}
				leaves++
				cmd, err := readGhCommand(path, help)
				if err != nil {
					t.Errorf("gh %s: %v", version, err)
					continue
				}
				var kinds []string
				for _, slot := range cmd.slots {
					if slot.takes("new-repository") || slot.takes("second-repository") {
						t.Errorf("gh %s's `gh %s` takes %v, a repository it creates or acts on beside its own, out of R4's reach", version, leaf, slot.args)
					}
					// An argument of no kind, or a number, names nothing the selector reader
					// counts (fromCommand); one whose name no table knows is listed, and no row
					// reads octocat through it, since fromCommand refuses it.
					switch kind, unknown := slot.kind(); {
					case unknown != "":
						kinds = append(kinds, "argument <"+unknown+"> of no known kind")
					case kind != "" && kind != "number":
						kinds = append(kinds, "argument "+kind)
					}
				}
				for _, name := range slices.Sorted(maps.Keys(cmd.flags)) {
					switch kind := ghOptionKind(cmd.path, name, cmd.flags[name]); {
					case outOfReach[kind]:
						t.Errorf("gh %s's `gh %s` takes --%s, a selector of kind %q, out of R4's reach", version, leaf, name, kind)
					case kind != "" && kind != "nothing":
						kinds = append(kinds, "option --"+name+" ("+kind+")")
					}
				}
				for _, kind := range kinds {
					if !covered[[2]string{leaf, kind}] {
						t.Errorf("gh %s's `gh %s` gives the %s, and no row reads octocat through it", version, leaf, kind)
					}
				}
			}
			rows := 0
			for _, r := range reads {
				if !lists(strings.Fields(r.leaf)) {
					continue
				}
				rows++
				name := "gh " + strings.Join(r.argv, " ")
				if repo := r.env["GH_REPO"]; repo != "" {
					name = "GH_REPO=" + repo + " " + name
				}
				t.Run(name, func(t *testing.T) {
					for _, owner := range []string{"octocat", "sjawhar"} {
						t.Run(owner, func(t *testing.T) {
							argv := make([]string, len(r.argv))
							for i, word := range r.argv {
								argv[i] = strings.ReplaceAll(word, "octocat", owner)
							}
							env := maps.Clone(pane)
							for key, value := range r.env {
								env[key] = strings.ReplaceAll(value, "octocat", owner)
							}
							target, err := TargetOf(argv, env, help, noGitRepository, dirFor)
							switch {
							case owner == "sjawhar":
								if err != nil || target.Dir != ghconfig.Dir("/owners", "sjawhar") {
									t.Errorf("TargetOf(%q, %v) at gh %s = %+v, %v; want sjawhar's directory", argv, env, version, target, err)
								}
							case r.refused:
								if err == nil || !strings.Contains(err.Error(), refusal) {
									t.Errorf("TargetOf(%q, %v) at gh %s = %+v, %v; want main's refusal, %q", argv, env, version, target, err, refusal)
								}
							case err != nil || target.Dir != pane["GH_CONFIG_DIR"]:
								t.Errorf("TargetOf(%q, %v) at gh %s = %+v, %v; want GH_CONFIG_DIR's directory, %s", argv, env, version, target, err, pane["GH_CONFIG_DIR"])
							}
						})
					}
				})
			}
			t.Logf("gh %s: %d read leaves, %d rows", version, leaves, rows)
			if leaves == 0 || rows == 0 {
				t.Errorf("gh %s lists %d read leaves and %d rows ran, want each read leaf and every row", version, leaves, rows)
			}
		})
	}
}

// Every refusal names the argument it could not place and how to name the repository instead:
// -R, or, for gh api, which has none, the endpoint; for a gh alias, the command it stands for
// (ghroutetest.RefusalAdvice).
func TestGhTargetOfRefusalsSayHowToNameTheRepository(t *testing.T) {
	for argv, want := range ghroutetest.RefusalAdvice {
		_, err := TargetOf(strings.Fields(argv), nil, savedGhHelp, noGitRepository, ghroutetest.OwnersOwnDirectories)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "sends no token") {
			t.Errorf("TargetOf(%q) = %v, want a refusal saying it sends no token and %q", argv, err, want)
		}
	}
}

// TargetOf names the leaf a command runs as gh's own help names it, a cobra alias read as its
// command, and none for a command that runs no leaf: gh's own help, its version, a group alone.
func TestTargetOfNamesTheLeafCommand(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want []string
	}{
		{[]string{"pr", "ls", "-R", "sjawhar/legion"}, []string{"pr", "list"}},
		{[]string{"-R", "sjawhar/legion", "pr", "view", "1", "--help"}, []string{"pr", "view"}},
		{[]string{"api", "repos/sjawhar/legion"}, []string{"api"}},
		{[]string{"pr"}, nil},
		{[]string{"--version"}, nil},
		{[]string{"help", "pr", "view"}, nil},
	} {
		target, err := TargetOf(tc.argv, nil, savedGhHelp, noGitRepository, ghroutetest.OwnersOwnDirectories)
		if err != nil || !slices.Equal(target.Command, tc.want) {
			t.Errorf("TargetOf(%q) = %+v, %v; want the command %q", tc.argv, target, err, tc.want)
		}
	}
}

// gh reads the repository of a command naming none from the working directory's git remotes, and
// ReadGitRemotes reads them with git as gh does: every fetch and push URL `git remote -v` prints,
// insteadOf applied, a partial clone's fetch line too (git prints its filter after it, as
// `(fetch) [blob:none]`), and every remote's gh-resolved setting; a directory that is no git
// repository has none.
func TestReadGitRemotesReadsWhatGhReads(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	repository := t.TempDir()
	t.Chdir(repository)
	if remotes, err := ReadGitRemotes(context.Background(), os.Environ()); err != nil || remotes != nil {
		t.Errorf("ReadGitRemotes outside a git repository = %v, %v; want none", remotes, err)
	}
	// One home for every fixture of this test: each command reads the repository it is given and
	// the identity Isolated supplies, and nothing of the home but that.
	fixture := fixturegit.Isolated(t, t.TempDir())
	for _, args := range [][]string{
		fixturegit.InitWorkTree("", "")[1:],
		{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
		{"config", "remote.origin.pushurl", "https://github.com/acme/pushed.git"},
		{"remote", "add", "upstream", "gh:sjawhar/legion"},
		{"config", "url.git@github.com:.insteadOf", "gh:"},
		{"config", "remote.origin.gh-resolved", "base"},
		{"config", "remote.upstream.promisor", "true"},
		{"config", "remote.upstream.partialclonefilter", "blob:none"},
	} {
		// ReadGitRemotes reads the process's working directory, so the fixture runs in the same
		// directory by name rather than inheriting whatever the process has, and it carries an
		// environment of its own rather than the test process's. The two t.Setenv lines above are
		// not for this git: ReadGitRemotes runs in-process, in the test binary, and reads them
		// there, while fixtureenv.Isolated keeps this git off any repository, configuration or
		// credential a GIT_ variable planted back after the guard could name.
		fixture.Run(repository, append([]string{"git"}, args...)...)
	}
	remotes, err := ReadGitRemotes(context.Background(), os.Environ())
	want := []Remote{
		{name: "origin", url: "https://github.com/acme/widgets.git"},
		{name: "origin", url: "https://github.com/acme/pushed.git", push: true},
		{name: "upstream", url: "git@github.com:sjawhar/legion"},
		{name: "upstream", url: "git@github.com:sjawhar/legion", push: true},
		{name: "origin", marker: true, key: "remote.origin.gh-resolved", resolved: "base", valued: true},
	}
	if err != nil || !slices.Equal(remotes, want) {
		t.Errorf("ReadGitRemotes = %+v, %v; want %+v", remotes, err, want)
	}
}

// gh reads every line `git config --get-regexp` prints for a gh-resolved key, giving it the
// remote its key's second `.`-separated part names and skipping one with no value (cli/cli
// git/client.go, populateResolvedRemotes), so ReadGitRemotes hands each on as a marker line, in
// git's order, with git's own key and value and whether git printed one: a key given two values,
// a marker on a dotted remote name, an empty value and a key set with none.
func TestReadGitRemotesReadsEveryMarkerLineGitPrints(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	repository := t.TempDir()
	t.Chdir(repository)
	fixture := fixturegit.Isolated(t, t.TempDir())
	for _, args := range [][]string{
		fixturegit.InitWorkTree("", "")[1:],
		{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
		{"config", "remote.origin.gh-resolved", "base"},
		{"config", "--add", "remote.origin.gh-resolved", "sjawhar/legion"},
		// The dotted remote has a URL: a marker's section alone is a remote `git remote -v`
		// prints with none, a line ReadGitRemotes refuses.
		{"remote", "add", "origin.fork", "https://github.com/acme/fork.git"},
		{"config", "remote.origin.fork.gh-resolved", "base"},
		{"remote", "add", "upstream", "git@github.com:sjawhar/legion.git"},
		{"config", "remote.upstream.gh-resolved", ""},
	} {
		// The fixture configures the repository ReadGitRemotes reads, named rather than
		// inherited, under an environment of its own, for the reason the test above gives.
		fixture.Run(repository, append([]string{"git"}, args...)...)
	}
	// git config writes no key without a value, so that line is written into the repository's
	// configuration file itself.
	config, err := os.OpenFile(filepath.Join(repository, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.WriteString("[remote \"upstream\"]\n\tgh-resolved\n"); err != nil {
		t.Fatal(err)
	}
	if err := config.Close(); err != nil {
		t.Fatal(err)
	}
	remotes, err := ReadGitRemotes(context.Background(), os.Environ())
	want := []Remote{
		{name: "origin", url: "https://github.com/acme/widgets.git"},
		{name: "origin", url: "https://github.com/acme/widgets.git", push: true},
		{name: "origin.fork", url: "https://github.com/acme/fork.git"},
		{name: "origin.fork", url: "https://github.com/acme/fork.git", push: true},
		{name: "upstream", url: "git@github.com:sjawhar/legion.git"},
		{name: "upstream", url: "git@github.com:sjawhar/legion.git", push: true},
		{name: "origin", marker: true, key: "remote.origin.gh-resolved", resolved: "base", valued: true},
		{name: "origin", marker: true, key: "remote.origin.gh-resolved", resolved: "sjawhar/legion", valued: true},
		{name: "origin.fork", marker: true, key: "remote.origin.fork.gh-resolved", resolved: "base", valued: true},
		{name: "upstream", marker: true, key: "remote.upstream.gh-resolved", valued: true},
		{name: "upstream", marker: true, key: "remote.upstream.gh-resolved"},
	}
	if err != nil || !slices.Equal(remotes, want) {
		t.Errorf("ReadGitRemotes = %+v, %v; want %+v", remotes, err, want)
	}
}

// git prints a remote's URL and partial clone filter as they are configured, so whitespace in
// either lets gh's greedy reading of the line take another URL than the one before its first
// ` (`: ReadGitRemotes refuses such a line, naming it, rather than count an owner gh may not act
// on. The three shapes the gates probed, each gh acting on evilown's repository: a filter holding a
// second `<url> (fetch)`, the same after a GitHub URL, and a URL holding a second URL.
func TestReadGitRemotesRefusesALineGhReadsOtherwise(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	for _, tc := range []struct{ name, url, filter string }{
		{"a filter holding a second URL", "https://example.com/a/b", "blob:none https://github.com/evilown/y (fetch)"},
		{"a filter closing and reopening its brackets", "https://github.com/acme/widgets", "x] https://github.com/evilown/y (fetch) [y"},
		{"a URL holding a second URL", "https://example.com/x https://github.com/evilown/y", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository := t.TempDir()
			t.Chdir(repository)
			config := [][]string{fixturegit.InitWorkTree("", "")[1:], {"remote", "add", "origin", tc.url}}
			if tc.filter != "" {
				config = append(config, []string{"config", "remote.origin.promisor", "true"}, []string{"config", "remote.origin.partialclonefilter", tc.filter})
			}
			fixture := fixturegit.Isolated(t, t.TempDir())
			for _, args := range config {
				// The fixture configures the repository ReadGitRemotes reads, which is the
				// directory this case chdirs into, named here rather than inherited, and its
				// environment is its own too, for the reason the test above gives.
				fixture.Run(repository, append([]string{"git"}, args...)...)
			}
			if remotes, err := ReadGitRemotes(context.Background(), os.Environ()); err == nil || !strings.Contains(err.Error(), "git remote -v printed") || !strings.Contains(err.Error(), "evilown") {
				t.Errorf("ReadGitRemotes = %+v, %v; want the line naming evilown refused", remotes, err)
			}
		})
	}
}

// The files a call would send to GitHub as content are read out of the same parse that reads the
// owner, so the leak guard reads each file's text (leakguard.GhRequest.Sent) and needs no second
// reader of the argv. The parse knows each option's arity from the leaf's own help, which is what
// a table of option names cannot: `-F` is --body-file in `gh pr create` and --field in `gh api`,
// a short cluster gives its last letter the next argument (`-dF <path>` is `--draft --body-file
// <path>`), and an attached value may carry an `=` of its own (`-F=<path>`). A file a field names
// carries the field's name. A value of `-`, an `@-`, a field given a literal and a raw field name
// no file to open; a `-` and an `@-` are standard input instead, which the parse answers is sent,
// with the use the option that named it makes of it, or none where two options with different
// uses did, and with the name of each field whose value it is: `query` in a `gh api graphql`
// call is what makes the guard read that input one GraphQL literal at a time. `gh workflow run
// --json`, at a last value of true, names it as a request body; `--json` of any other command
// takes the fields to print and names none. Each command here has a leaf, so its answer is never
// nil, a command that sends nothing among them: the guard then reads nothing of it from the argv
// again. The answer is compared whole, since the names of the fields of standard input are
// leakguard's own and can be seen no other way (ghroutetest.SentFileCalls).
func TestGhTargetOfReadsTheFilesTheCallWouldSend(t *testing.T) {
	for _, tc := range ghroutetest.SentFileCalls {
		t.Run(tc.Name, func(t *testing.T) {
			target, err := TargetOf(tc.Argv, nil, savedGhHelp, noGitRepository, ghroutetest.OwnersOwnDirectories)
			if err != nil {
				t.Fatalf("TargetOf(%q) = %v", tc.Argv, err)
			}
			if target.Sent == nil {
				t.Fatalf("TargetOf(%q) handed no parse of what the call sends, so the guard would read the argv again", tc.Argv)
			}
			if !reflect.DeepEqual(target.Sent, tc.Want) {
				t.Errorf("TargetOf(%q) read what the call sends as %+v, want %+v", tc.Argv, target.Sent, tc.Want)
			}
		})
	}
}

// gh 2.104.0's --attach uploads the file it names to GitHub, an SVG's text among what it takes,
// and the leak guard reads no such file, so TargetOf refuses a call giving one, naming the
// option, before anything runs, on each of the eight leaves whose help lists it. On the six issue
// and pr leaves the same calls without it read as they did, so the option alone is what is
// refused; and at gh 2.98.0, which has no --attach, each is refused already, as an option its help
// does not list. On `gh issue artifact create` and `edit` --attach is refused by name too; a
// create that names a file by argument, which it also uploads, is refused for an argument this
// does not know.
func TestGhTargetOfRefusesAnUploadTheGuardDoesNotRead(t *testing.T) {
	latest := savedGhHelpOf(t, "2.104.0")
	rows := 0
	for _, argv := range [][]string{
		{"issue", "comment", "5", "-R", "acme/widgets", "--body", "hi", "--attach", "notes.svg"},
		{"issue", "create", "-R", "acme/widgets", "--title", "t", "--body", "b", "--attach", "notes.svg"},
		{"issue", "edit", "5", "-R", "acme/widgets", "--attach=notes.svg#alt"},
		{"pr", "comment", "5", "-R", "acme/widgets", "--attach", "notes.svg#alt"},
		{"pr", "create", "-R", "acme/widgets", "--title", "t", "--body", "b", "--attach", "notes.svg"},
		{"pr", "edit", "5", "-R", "acme/widgets", "--attach", "notes.svg"},
	} {
		attach := slices.IndexFunc(argv, func(arg string) bool { return strings.HasPrefix(arg, "--attach") })
		t.Run(strings.Join(argv[:attach], " "), func(t *testing.T) {
			rows++
			_, err := TargetOf(argv, nil, latest, noGitRepository, ghroutetest.OwnersOwnDirectories)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q uploads the file it names to GitHub, which the leak guard does not read", argv[attach])) {
				t.Errorf("TargetOf(%q) at gh 2.104.0 = %v, want a refusal naming %q", argv, err, argv[attach])
			}
			end := attach + 2
			if strings.Contains(argv[attach], "=") {
				end = attach + 1
			}
			without := slices.Delete(slices.Clone(argv), attach, end)
			if target, err := TargetOf(without, nil, latest, noGitRepository, ghroutetest.OwnersOwnDirectories); err != nil || target.Owner != "acme" {
				t.Errorf("TargetOf(%q) at gh 2.104.0 = %+v, %v; want acme's call", without, target, err)
			}
			if _, err := TargetOf(argv, nil, savedGhHelp, noGitRepository, ghroutetest.OwnersOwnDirectories); err == nil || !strings.Contains(err.Error(), "is no option") {
				t.Errorf("TargetOf(%q) at gh 2.98.0 = %v, want --attach refused as no option", argv, err)
			}
		})
	}
	if rows != 6 {
		t.Errorf("%d rows ran, want 6", rows)
	}
	for _, tc := range []struct {
		argv   []string
		refuse string
	}{
		{[]string{"issue", "artifact", "create", "5", "-R", "acme/widgets", "--attach", "notes.svg"}, `"--attach" uploads the file it names to GitHub, which the leak guard does not read`},
		{[]string{"issue", "artifact", "edit", "142", "2", "-R", "acme/widgets", "--attach", "./flow.png"}, `"--attach" uploads the file it names to GitHub, which the leak guard does not read`},
		{[]string{"issue", "artifact", "create", "5", "plan.md", "-R", "acme/widgets"}, "an argument this does not know"},
	} {
		if _, err := TargetOf(tc.argv, nil, latest, noGitRepository, ghroutetest.OwnersOwnDirectories); err == nil || !strings.Contains(err.Error(), tc.refuse) {
			t.Errorf("TargetOf(%q) at gh 2.104.0 = %v, want a refusal containing %q", tc.argv, err, tc.refuse)
		}
	}
}

// TargetOf refuses, before gh runs, what has gh's git recurse into a submodule or take
// configuration from the call: `pr checkout --recurse-submodules`, and any git flag a clone leaf
// (repo clone, repo fork, gist clone) passes on that cloneGitFlagTakes does not hold, spelled in
// full, abbreviated, joined, with `=`, or after a second `--`. It reads every flag passed on, not
// the first alone: the second review's `-- --depth 1 -c ...` is refused at its `-c`. The flags it
// holds, a directory first, pass, scripts/e2e's fixture clone among them, at gh 2.104.0 and 2.98.0.
func TestGhTargetOfRefusesRecursionAndUnreadCloneFlags(t *testing.T) {
	off, proxy := "http.https://github.com/.sslVerify=false", "http.https://github.com/.proxy=http://127.0.0.1:1"
	for _, version := range []string{"2.104.0", "2.98.0"} {
		help := savedGhHelpOf(t, version)
		for _, tc := range []struct {
			argv   []string
			refuse string // "" for a call that passes
		}{
			{[]string{"pr", "checkout", "1", "-R", "acme/widgets", "--recurse-submodules"}, `"--recurse-submodules" runs`},
			{[]string{"pr", "checkout", "1", "-R", "acme/widgets", "--recurse-submodules=false"}, `"--recurse-submodules=false" runs`},
			{[]string{"repo", "clone", "acme/widgets", "w2", "--", "-c", off, "-c", proxy}, `hands "-c" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--depth", "1", "-c", off}, `hands "-c" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--config", off}, `hands "--config" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--config=" + off}, `hands "--config=` + off + `" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--conf", off}, `hands "--conf" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--recurse-submodules"}, `hands "--recurse-submodules" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--recurse-submodules=lib"}, `hands "--recurse-submodules=lib" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--recursive"}, `hands "--recursive" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--template", "/tmp/example-template"}, `hands "--template" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--template=/tmp/example-template"}, `hands "--template=/tmp/example-template" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "-u", "/tmp/example-upload-pack"}, `hands "-u" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--upload-pack", "/tmp/example-upload-pack"}, `hands "--upload-pack" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--reference", "/tmp/example-reference"}, `hands "--reference" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--reference-if-able", "/tmp/example-reference"}, `hands "--reference-if-able" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--separate-git-dir", "/tmp/example-git-dir"}, `hands "--separate-git-dir" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "-qn"}, `hands "-qn" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "-bmain"}, `hands "-bmain" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "-b=main"}, `hands "-b=main" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--quiet=true"}, `hands "--quiet=true" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "--depth"}, `hands "--depth" to git clone with no value`},
			{[]string{"repo", "clone", "acme/widgets", "widgets", "extra"}, `hands "extra" to git clone`},
			{[]string{"repo", "clone", "acme/widgets", "--", "widgets", "--", "--recursive"}, `hands "--" to git clone`},
			{[]string{"repo", "fork", "acme/widgets", "--clone", "--", "--recursive"}, `hands "--recursive" to git clone`},
			{[]string{"repo", "fork", "acme/widgets", "--clone", "--", "-c", off}, `hands "-c" to git clone`},
			{[]string{"gist", "clone", "0123456789abcdef", "--", "-c", "core.hooksPath=/tmp/example-hooks"}, `hands "-c" to git clone`},
			{[]string{"pr", "checkout", "1", "-R", "acme/widgets"}, ""},
			{[]string{"repo", "clone", "acme/widgets", "widgets", "--", "--quiet", "--depth", "1", "--branch", "main"}, ""},
			{[]string{"repo", "clone", "acme/widgets", "--", "--depth=1", "--filter=blob:none", "--single-branch", "-b", "main", "--no-tags", "--origin", "up"}, ""},
			{[]string{"repo", "clone", "--", "acme/widgets", "widgets", "--bare", "-q", "--branch", "--recursive"}, ""},
			{[]string{"repo", "fork", "acme/widgets", "--clone", "--", "--depth", "1"}, ""},
			{[]string{"gist", "clone", "0123456789abcdef", "notes", "--", "--quiet"}, ""},
			{[]string{"pr", "checkout", "1", "-R", "acme/widgets", "--worktree", "/tmp/example-worktree"}, ""},
		} {
			_, err := TargetOf(tc.argv, nil, help, noGitRepository, ghroutetest.OwnersOwnDirectories)
			switch {
			case tc.refuse == "" && err != nil:
				t.Errorf("TargetOf(%q) at gh %s = %v, want no refusal", tc.argv, version, err)
			case tc.refuse != "" && (err == nil || !strings.Contains(err.Error(), tc.refuse)):
				t.Errorf("TargetOf(%q) at gh %s = %v, want a refusal containing %q", tc.argv, version, err, tc.refuse)
			}
		}
	}
}

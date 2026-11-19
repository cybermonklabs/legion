package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// ghHelpDir holds the real gh's help of each command the resolver's tests read, saved from gh
// 2.98.0 (testdata/gh-help/VERSION) in a fresh configuration, one file per path, its words joined
// by "_", and gh's own help (`gh --help`) as gh.txt.
const ghHelpDir = "testdata/gh-help"

// savedGhHelp answers gh's help of path from ghHelpDir as the real gh does: a path it has no file
// for prints the help of the nearest parent it has (gh's answer to a subcommand it does not know),
// and one with no such parent at all is an error (gh's `unknown command`, exit 1).
func savedGhHelp(path []string) (string, error) {
	if len(path) == 0 {
		data, err := os.ReadFile(filepath.Join(ghHelpDir, "gh.txt"))
		return string(data), err
	}
	for p := path; len(p) > 0; p = p[:len(p)-1] {
		data, err := os.ReadFile(filepath.Join(ghHelpDir, strings.Join(p, "_")+".txt"))
		if err == nil {
			return string(data), nil
		}
	}
	return "", fmt.Errorf("unknown command %q for \"gh\"", path[0])
}

// editedGhHelp answers gh's help as savedGhHelp does, with old replaced by new in the help of
// each path edits names (its words joined by spaces, "" for gh's own): the help a gh with other
// aliases or extensions, or a later gh spelling its usages otherwise, prints.
func editedGhHelp(edits map[string][2]string) func([]string) (string, error) {
	return func(path []string) (string, error) {
		text, err := savedGhHelp(path)
		if edit, ok := edits[strings.Join(path, " ")]; ok && err == nil {
			if !strings.Contains(text, edit[0]) {
				return "", fmt.Errorf("the saved help of %q holds no %q to edit", path, edit[0])
			}
			text = strings.Replace(text, edit[0], edit[1], 1)
		}
		return text, err
	}
}

// The help of a gh with an extension, `hello`, and an alias of its own under pr, `pr pv2`, beside
// gh's default alias co; and of a gh whose usage of pr view spells its URL <URL>, as no gh this
// was read from does.
var (
	ghWithAnExtensionAndANestedAlias = editedGhHelp(map[string][2]string{
		"":   {"ALIAS COMMANDS\n", "EXTENSION COMMANDS\n  hello:         Extension hello\n\nALIAS COMMANDS\n"},
		"pr": {"\nFLAGS\n", "\nALIAS COMMANDS\n  pv2:           Alias for \"pr view -R sjawhar/legion\"\n\nFLAGS\n"},
	})
	ghSpellingItsURLOtherwise = editedGhHelp(map[string][2]string{
		"pr view": {"<url>", "<URL>"},
	})
)

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
	drift, err := ghSpellingItsURLOtherwise([]string{"pr", "view"})
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
// tables know, but for the few ghTargetOf refuses on purpose: what `gh copilot` and `gh extension
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
			t.Errorf("ghSlotKinds knows <%s>, which ghTargetOf refuses on purpose", name)
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

// ghTargetOf reads the owner a gh command targets from every selector gh's own help names, and
// fails closed: an argument it cannot place is refused, naming it, and so is a command naming two
// owners; only a command naming no repository at all is not named, once the working directory's
// git remotes name no owner but the token's, and a command gh only prints the help or version of
// sends nothing. GH_REPO names the owner only when nothing explicit does, and only for a leaf that
// reads it.
func TestGhTargetOfFailsClosed(t *testing.T) {
	acme := map[string]string{"GH_REPO": "acme/widgets"}
	// A pane of the issue owner acme, whose GH_CONFIG_DIR is acme's directory of the bundle.
	pane := map[string]string{"GH_CONFIG_DIR": "/owners/acme", "LEGION_GH_OWNERS_DIR": "/owners"}
	paneWith := func(name, value string) map[string]string {
		env := map[string]string{name: value}
		maps.Copy(env, pane)
		return env
	}
	remotes := func(listed ...ghRemote) func() ([]ghRemote, error) {
		return func() ([]ghRemote, error) { return listed, nil }
	}
	acmeClone := ghRemote{name: "origin", url: "https://github.com/Acme/widgets.git"}
	sjawharClone := ghRemote{name: "origin", url: "https://github.com/sjawhar/legion"}
	// A gh whose pr view lists an option a later gh might add, naming a repository.
	ghWithARepositoryOption := editedGhHelp(map[string][2]string{
		"pr view": {"\nINHERITED FLAGS\n", "      --mirror string    Read the pull request from this repository instead\n\nINHERITED FLAGS\n"},
	})
	for _, tc := range []struct {
		argv    []string
		env     map[string]string
		help    func([]string) (string, error) // savedGhHelp when nil
		remotes func() ([]ghRemote, error)     // a working directory that is no git repository when nil
		owner   string                         // "" with no refusal: not named
		err     string                         // a refusal containing this
	}{
		// The final gates' at 96e1672cd, as round 4 reads them. A command naming no repository acts
		// on the working directory's: every github.com remote and gh-resolved setting names an
		// owner, and they must name one.
		{argv: []string{"pr", "view", "1"}, env: pane, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"pr", "view", "1"}, env: pane, remotes: remotes(acmeClone, ghRemote{name: "origin", resolved: "base"}), owner: "acme"},
		{argv: []string{"pr", "view", "1"}, env: pane, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"repo", "view"}, env: pane, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"api", "repos/{owner}/{repo}"}, env: pane, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(ghRemote{name: "origin", url: "https://github.com/octocat/hello"}), owner: "octocat"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "git@github.com:sjawhar/legion.git"}), err: `more than one owner, acme (the working directory's remote "origin" (https://github.com/Acme/widgets.git)) and sjawhar (the working directory's remote "upstream" (git@github.com:sjawhar/legion.git))`},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, ghRemote{name: "origin", resolved: "sjawhar/legion"}), err: "sjawhar (the working directory's remote.origin.gh-resolved sjawhar/legion)"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, ghRemote{name: "fork", url: "ssh://git@ssh.github.com:443/Sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, ghRemote{name: "work", url: "git@gh-work:sjawhar/legion.git"}), err: "is an ssh remote whose host gh may read as github.com"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, ghRemote{name: "mirror", url: "https://gitlab.example.com/sjawhar/legion"}, ghRemote{name: "local", url: "/srv/legion"}), owner: "acme"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_CONFIG_DIR": "/owners", "LEGION_GH_OWNERS_DIR": "/owners"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"pr", "list"}, env: pane, remotes: func() ([]ghRemote, error) { return nil, fmt.Errorf("bad config line 3") }, err: "cannot be read (bad config line 3)"},
		{argv: []string{"-R", "acme/widgets", "pr", "view", "1"}, env: pane, remotes: remotes(sjawharClone), owner: "acme"},
		{argv: []string{"pr", "list"}, env: paneWith("GH_REPO", "acme/widgets"), remotes: remotes(sjawharClone), owner: "acme"},
		{argv: []string{"pr", "view", "--help"}, env: pane, remotes: remotes(sjawharClone)},
		// Round 4's: a remote URL gh reads as github.com's whatever its scheme, as gh's own parser
		// reads it (deep 2, quality A, final B1).
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "git+https://github.com/sjawhar/legion.git"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "ftp://github.com/sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "ftps://github.com/sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "file://github.com/sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "github.com://sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "git@github.com/sjawhar:legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(ghRemote{name: "origin", url: "git+https://github.com/sjawhar/legion"}), owner: "sjawhar"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "GIT+HTTPS://github.com/sjawhar/legion"}), err: "is an ssh remote whose host gh may read as github.com"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "https://github.com/%zz/legion"}), err: "is a URL this cannot read as gh does"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "https://github.com/sjawhar/legion/extra"}), err: "names no OWNER/REPO"},
		// Round 4's: an option or another repository naming one owner while gh takes the command's
		// own repository from the working directory or GH_REPO (deep 1, quality B, final B2).
		{argv: []string{"pr", "comment", "5", "--body", "https://github.com/sjawhar/legion/pull/2"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"issue", "edit", "5", "--add-blocked-by", "https://github.com/sjawhar/legion/issues/9"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"pr", "comment", "1", "--body", "https://github.com/acme/widgets/pull/9"}, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"pr", "comment", "1", "--body", "https://github.com/sjawhar/legion/pull/2"}, remotes: remotes(ghRemote{name: "origin", url: "https://github.com/octocat/hello"}), owner: "octocat"},
		{argv: []string{"issue", "close", "3", "--duplicate-of", "https://github.com/acme/widgets/issues/1"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"issue", "edit", "3", "--add-sub-issue", "https://github.com/acme/widgets/issues/1"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"issue", "create", "--title", "t", "--body", "b", "--parent", "https://github.com/acme/widgets/issues/1"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"repo", "sync", "--source", "acme/widgets"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"issue", "develop", "3", "--branch-repo", "acme/widgets", "--name", "x"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"issue", "close", "3", "--duplicate-of", "https://github.com/sjawhar/legion/issues/5"}, env: acme, err: "more than one owner, acme (GH_REPO acme/widgets) and sjawhar"},
		{argv: []string{"repo", "fork", "--org", "sjawhar", "--clone=false", "--remote=false"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"repo", "fork", "acme/widgets", "--org", "sjawhar", "--clone=false"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion"}, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion"}, remotes: remotes(acmeClone), err: "more than one owner"},
		// Round 4's minor findings: options mentioning URLs or a team, and project link and unlink,
		// which read GH_REPO with -R a plain string (deep 3, deep 4).
		{argv: []string{"issue", "create", "--title", "t", "--body", "b", "--blocked-by", "1,https://github.com/sjawhar/legion/issues/5"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"issue", "create", "--title", "t", "--body", "b", "--blocking", "https://github.com/acme/widgets/issues/5"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"project", "link", "1"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"project", "link", "1", "--owner", "acme"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, err: "more than one owner"},
		{argv: []string{"project", "unlink", "1", "--owner", "acme", "--team", "sjawhar/core"}, err: "more than one owner"},
		{argv: []string{"project", "link", "1", "--team", "core", "-R", "acme/widgets"}, err: `"core" names no owner`},
		// Round 5's: gh takes a passthrough leaf's repository from the first argument even after
		// `--` (deep 1); `repo set-default` reads every remote's network whatever its argument
		// names (deep 2, quality 1); project link and unlink read no repository of their own
		// with --team (quality 2); a pr command reads OWNER/REPO#N and a URL other than a pull
		// request's as a branch of its own repository (final F1).
		{argv: []string{"repo", "fork", "--", "sjawhar/legion"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"repo", "fork", "--org", "acme", "--", "sjawhar/legion"}, err: "more than one owner"},
		{argv: []string{"repo", "fork", "--", "sjawhar/legion", "--depth=1"}, owner: "sjawhar"},
		{argv: []string{"repo", "clone", "--", "sjawhar/legion"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		// What a clone leaf hands git clone after its repository is git's, not gh's: a value
		// naming another owner's repository there routes nothing. Only the git clone flags
		// ghCloneGitFlagTakes holds pass, so a -R there, which git clone has no use for, is refused.
		{argv: []string{"repo", "clone", "--", "sjawhar/legion", "--depth=1", "--branch", "acme/w"}, owner: "sjawhar"},
		{argv: []string{"repo", "clone", "--", "sjawhar/legion", "--depth=1", "-R", "acme/w"}, err: `hands "-R" to git clone`},
		{argv: []string{"repo", "set-default", "sjawhar/legion"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"repo", "set-default", "sjawhar/legion"}, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"project", "link", "1", "--team", "sjawhar/core"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"project", "unlink", "1", "--team", "sjawhar/core"}, env: acme, owner: "sjawhar"},
		{argv: []string{"pr", "comment", "sjawhar/legion#2", "--body", "hi"}, remotes: remotes(ghRemote{name: "origin", url: "https://github.com/octocat/hello"}), owner: "octocat"},
		{argv: []string{"pr", "view", "sjawhar/legion#2"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/issues/5"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/tree/x"}, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/2/files"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/5", "--title", "x"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		// --version is gh's alone; before anything, gh reads the next word as a command, an
		// alias's or an extension's among them.
		{argv: []string{"--version"}},
		{argv: []string{"--version", "hello"}, help: ghWithAnExtensionAndANestedAlias, err: `"--version" comes before the command's name`},
		{argv: []string{"--version", "co"}, err: `"--version" comes before the command's name`},
		{argv: []string{"-R", "sjawhar/legion", "--version", "hello"}, help: ghWithAnExtensionAndANestedAlias, err: `"--version" comes before the command's name`},
		{argv: []string{"--help", "--version"}, err: `"--version" comes before the command's name`},
		// A first word gh's own help does not list, whose help gh may answer by installing an
		// extension, and the commands that run another program on gh's token.
		{argv: []string{"stack", "view"}, err: `"stack" is no command gh's own help lists`},
		{argv: []string{"ext", "exec", "hello"}, err: `"ext" is no command gh's own help lists`},
		{argv: []string{"extension", "exec", "hello"}, err: "`gh extension exec` runs another program"},
		{argv: []string{"help", "extension", "exec"}, err: "`gh extension exec` runs another program"},
		// Options name a repository as arguments do: by ghOptionKinds, an issue by URL among them,
		// and one a later gh adds refused; a URL in an option naming none is text.
		{argv: []string{"issue", "edit", "1", "--add-blocked-by", "https://github.com/sjawhar/legion/issues/2", "-R", "acme/widgets"}, err: "more than one owner"},
		{argv: []string{"issue", "edit", "1", "--add-sub-issue", "2", "-R", "acme/widgets"}, owner: "acme"},
		{argv: []string{"issue", "create", "--title", "t", "--body", "b", "--parent", "https://github.com/sjawhar/legion/issues/1"}, owner: "sjawhar"},
		{argv: []string{"repo", "sync", "acme/widgets", "--source", "sjawhar/legion"}, err: "more than one owner"},
		{argv: []string{"pr", "comment", "1", "--body", "https://github.com/sjawhar/legion/pull/2", "-R", "acme/widgets"}, owner: "acme"},
		{argv: []string{"pr", "comment", "1", "--body", "https://example.com/sjawhar/legion", "-R", "acme/widgets"}, owner: "acme"},
		{argv: []string{"repo", "edit", "acme/widgets", "--homepage", "https://github.com/sjawhar/legion"}, owner: "acme"},
		{argv: []string{"pr", "view", "1", "--mirror", "sjawhar/legion", "-R", "acme/widgets"}, help: ghWithARepositoryOption, err: "names a repository, an owner, a team or a URL in a way this does not know"},
		// gh api's placeholders as gh reads them, the colon form too, in a typed field's value.
		{argv: []string{"api", "graphql", "-F", "o=:owner", "-F", "r=:repo", "-f", "query=q"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"api", "repos/:owner/:repo/pulls"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"api", "graphql", "-f", "o=:owner", "-f", "query=q"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}},
		{argv: []string{"api", "repos/{owner}/{repo}/commits/{branch}"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, owner: "sjawhar"},
		// A pr command reads OWNER/REPO#N as a branch of its own repository. `gh agent-task`
		// sends the token to the Copilot API host GitHub names, so it is refused whatever it
		// names, an issue URL and a pull request's alike; and `--editor` runs an editor after the
		// guard has read the call. Each is refused before any repository is read. (`gh gist
		// edit` and `gh auth login`, `logout`, `refresh` and `switch`, whose help these saved
		// files do not hold, are refused in TestRealGhAndGitSendTheTargetOwnersToken, with the
		// real gh.)
		{argv: []string{"agent-task", "view", "sjawhar/legion#1"}, err: "`gh agent-task` sends the token to the Copilot API host"},
		{argv: []string{"agent-task", "view", "https://github.com/sjawhar/legion/pull/1"}, remotes: remotes(acmeClone), err: "`gh agent-task` sends the token"},
		{argv: []string{"issue", "comment", "1", "-R", "acme/widgets", "-e"}, err: `"-e" runs an editor program`},
		{argv: []string{"pr", "comment", "1", "-R", "acme/widgets", "--editor"}, err: `"--editor" runs an editor program`},
		// -e is refused where it is --editor, and taken where the leaf's help gives it another
		// option: `gh secret set`'s -e is --env.
		{argv: []string{"secret", "set", "TOKEN", "-R", "acme/widgets", "-e", "production", "--body", "x"}, owner: "acme"},
		{argv: []string{"pr", "view", "sjawhar/legion#1", "-R", "acme/widgets"}, owner: "acme"},
		// The first re-review's reproductions.
		{argv: []string{"-R", "sjawhar/legion", "pr", "view", "1", "--json", "title"}, owner: "sjawhar"},
		{argv: []string{"--repo=sjawhar/legion", "pr", "view", "1", "--json", "title"}, owner: "sjawhar"},
		{argv: []string{"api", "--", "repos/sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"pr", "view", "--json", "title", "--", "https://github.com/sjawhar/legion/pull/1"}, owner: "sjawhar"},
		{argv: []string{"repo", "deploy-key", "add", "key.pub", "-R", "sjawhar/legion", "--title", "-Racme/widgets"}, owner: "sjawhar"},
		{argv: []string{"pr", "--json", "number", "view", "https://github.com/sjawhar/legion/pull/1"}, err: `"--json" comes before the command's name`},
		// The first review's reproductions.
		{argv: []string{"api", "repos/sjawhar/legion"}, env: acme, owner: "sjawhar"},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--json", "title"}, env: acme, owner: "sjawhar"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1", "-R", "acme/widgets"}, err: "more than one owner, acme (--repo acme/widgets) and sjawhar"},
		{argv: []string{"repo", "view", "--json", "nameWithOwner", "sjawhar/legion"}, env: acme, owner: "sjawhar"},
		// The second re-review's reproductions. --help is a boolean, the last of its values
		// deciding, before the command's name or after; only true is help.
		{argv: []string{"api", "repos/sjawhar/legion", "--help=false"}, owner: "sjawhar"},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false"}, owner: "sjawhar"},
		{argv: []string{"--help", "pr", "view", "1", "-R", "sjawhar/legion", "--help=false"}, owner: "sjawhar"},
		{argv: []string{"pr", "--help", "view", "1", "-R", "sjawhar/legion", "--help=0"}, owner: "sjawhar"},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false", "--help"}},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false", "-h"}},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=maybe"}, err: `"--help=maybe" gives --help a value that is no boolean`},
		{argv: []string{"--help=false", "api", "repos/sjawhar/legion"}, owner: "sjawhar"},
		// A name in --repos is a repository of the --org.
		{argv: []string{"secret", "set", "TOKEN", "--org", "sjawhar", "--repos", "legion", "--body", "dummy", "-R", "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"secret", "set", "TOKEN", "-o", "sjawhar", "-r", "legion,smoke", "--body", "dummy"}, owner: "sjawhar"},
		{argv: []string{"secret", "set", "TOKEN", "--org", "sjawhar", "--repos", "legion,acme/widgets", "--body", "dummy"}, err: "more than one owner"},
		{argv: []string{"secret", "set", "TOKEN", "--user", "--repos", "legion", "--body", "dummy"}, err: "no --org says whose"},
		// pflag's optional value after a shorthand: -c takes its default, and the rest of the
		// word is more shorthands.
		{argv: []string{"browse", "-c", "-R", "sjawhar/legion", "--no-browser"}, owner: "sjawhar"},
		{argv: []string{"browse", "-cnR", "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"browse", "-c=abc", "-R", "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"browse", "-c", "abc"}},
		// A gh alias, which may carry a repository of its own, and an extension, which reads gh's
		// token itself, are refused before their help is read, at any depth, and so is the Copilot
		// CLI gh runs.
		{argv: []string{"co", "https://github.com/sjawhar/legion/pull/1"}, err: `"co" is a gh alias`},
		{argv: []string{"pr", "pv2", "1"}, help: ghWithAnExtensionAndANestedAlias, err: `"pv2" is a gh alias`},
		{argv: []string{"hello", "-R", "sjawhar/legion"}, help: ghWithAnExtensionAndANestedAlias, err: `"hello" is a gh extension`},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion"}, help: ghWithAnExtensionAndANestedAlias, owner: "sjawhar"},
		{argv: []string{"pr", "ls", "-R", "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"copilot"}, err: "`gh copilot` runs another program"},
		// A search query names its owner in plain qualifiers, one to an argument, or not at all.
		{argv: []string{"search", "issues", "repo:sjawhar/legion", "is:open"}, owner: "sjawhar"},
		{argv: []string{"search", "issues", "bug OR crash"}, env: acme},
		{argv: []string{"search", "issues", "(repo:sjawhar/legion)"}, err: `"(repo:sjawhar/legion)" is a search qualifier this cannot read for certain`},
		{argv: []string{"search", "issues", "repo:sjawhar/legion is:open"}, err: "is a search qualifier this cannot read for certain"},
		{argv: []string{"search", "issues", `label:"help wanted"`}, err: "is a search qualifier this cannot read for certain"},
		{argv: []string{"search", "issues", "repo:sjawhar/legion", "OR", "bug"}, err: `"OR" joins the query's`},
		{argv: []string{"search", "issues", "(", "repo:sjawhar/legion", ")"}, err: `"(" joins the query's`},
		{argv: []string{"search", "issues", "--", "bug", "-repo:acme/widgets"}, err: `"-repo:acme/widgets" excludes an owner`},
		{argv: []string{"search", "issues", "bug", "-repo:acme/widgets"}, err: `"-repo:acme/widgets" names -r`},
		{argv: []string{"api", "search/issues?q=repo:sjawhar/legion+is:open"}, owner: "sjawhar"},
		{argv: []string{"api", "-X", "GET", "search/issues", "-f", "q=repo:sjawhar/legion is:open"}, owner: "sjawhar"},
		{argv: []string{"api", "search/issues?q=%28repo:sjawhar/legion%29"}, err: "is a search qualifier this cannot read for certain"},
		{argv: []string{"api", "-X", "GET", "search/issues", "-F", "q=@query.txt"}, err: "reads the search query from a file"},
		// gh sends its token only to GH_HOST, and GH_TOKEN or GITHUB_TOKEN in place of any file's.
		{argv: []string{"api", "repos/sjawhar/legion"}, env: map[string]string{"GH_HOST": "ghe.example.com"}, err: `GH_HOST is "ghe.example.com"`},
		{argv: []string{"api", "repos/sjawhar/legion"}, env: map[string]string{"GH_HOST": "GitHub.com"}, owner: "sjawhar"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_TOKEN": "ghp_x"}, err: "GH_TOKEN is set"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GITHUB_TOKEN": "ghp_x"}, err: "GITHUB_TOKEN is set"},
		// An argument the leaf's usage names in a way this does not know, or has no place for.
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1"}, help: ghSpellingItsURLOtherwise, err: "<URL>"},
		{argv: []string{"pr", "view", "1"}, help: ghSpellingItsURLOtherwise, err: "<URL>"},
		{argv: []string{"pr", "view", "1", "2"}, err: `"2" is an argument`},
		{argv: []string{"pr", "list", "sjawhar/legion"}, err: `"sjawhar/legion" is an argument`},
		{argv: []string{"api", "repos/acme/widgets", "repos/sjawhar/legion"}, err: `"repos/sjawhar/legion" is an argument`},
		// The third re-review's: gh's help of an extension runs the extension, so the target of
		// `gh help` is read as a command's path, a gh alias or an extension refused before anything
		// runs; gh's own help and its topics are not. And a plural slot, `{<numbers> | <urls>}`,
		// takes one or more arguments, every one a selector.
		{argv: []string{"help"}},
		{argv: []string{"help", "pr", "view"}},
		{argv: []string{"help", "environment"}},
		{argv: []string{"help", "co"}, err: `"co" is a gh alias`},
		{argv: []string{"help", "hello"}, help: ghWithAnExtensionAndANestedAlias, err: `"hello" is a gh extension`},
		{argv: []string{"help", "pr", "pv2"}, help: ghWithAnExtensionAndANestedAlias, err: `"pv2" is a gh alias`},
		{argv: []string{"-R", "acme/widgets", "help", "hello"}, help: ghWithAnExtensionAndANestedAlias, err: `"hello" is a gh extension`},
		{argv: []string{"help", "--bogus", "hello"}, err: "\"--bogus\" is an option of `gh help`"},
		{argv: []string{"issue", "edit", "1", "2", "--title", "x", "-R", "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/1", "https://github.com/sjawhar/legion/issues/2", "--title", "x"}, owner: "sjawhar"},
		{argv: []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/1", "https://github.com/acme/widgets/issues/2", "--title", "x"}, err: "more than one owner"},
		// A URL is one in any case, as gh parses it.
		{argv: []string{"pr", "view", "HTTPS://GITHUB.COM/sjawhar/legion/pull/1"}, owner: "sjawhar"},
		{argv: []string{"api", "HTTPS://API.GITHUB.COM/repos/sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"api", "REPOS/sjawhar/legion"}, owner: "sjawhar"},
		// A command naming no repository, and GH_REPO where the leaf reads it.
		{argv: []string{"pr", "list"}},
		{argv: []string{"pr", "list"}, env: acme, owner: "acme"},
		{argv: []string{"api", "graphql", "-f", "query=q", "-F", "owner=Sjawhar", "-F", "name=legion"}, env: acme, owner: "sjawhar"},
		{argv: []string{"api", "graphql", "-f", "query=q", "-F", "o=sjawhar", "-F", "id=PRRT_x"}},
		{argv: []string{"api", "user"}, env: acme},
		{argv: []string{"api", "graphql", "-f", "query={viewer{login}}"}, env: acme},
		{argv: []string{"pr"}},
		{argv: nil},
		{argv: []string{"--help"}},
		{argv: []string{"-h", "pr", "view", "1", "-R", "acme/w"}},
		{argv: []string{"pr", "view", "1", "--help"}},
		{argv: []string{"pr", "view", "1", "-R", "acme/w", "-h"}},
		{argv: []string{"api", "graphql", "-f", "query={viewer{login}}"}},
		{argv: []string{"api", "graphql", "-F", "owner={owner}", "-f", "query=q"}, env: acme, owner: "acme"},
		{argv: []string{"api", "repos/{owner}/{repo}/pulls"}},
		{argv: []string{"api", "repos/{owner}/{repo}/pulls"}, env: acme, owner: "acme"},
		{argv: []string{"auth", "status"}, env: acme},
		{argv: []string{"auth", "status", "-h", "github.com"}},
		{argv: []string{"browse", "--commit"}},
		{argv: []string{"search", "issues", "is:open"}, env: acme},
		// Explicit selectors.
		{argv: []string{"api", "-X", "GET", "https://api.github.com/orgs/Acme/teams"}, owner: "acme"},
		{argv: []string{"pr", "-R", "sjawhar/legion", "view", "1"}, owner: "sjawhar"},
		{argv: []string{"pr", "create", "--fill", "-R", "github.com/Acme/widgets"}, owner: "acme"},
		{argv: []string{"repo", "clone", "sjawhar/legion", "--", "--depth=1", "--branch", "acme/w"}, owner: "sjawhar"},
		{argv: []string{"repo", "create", "sjawhar/new", "--private"}, owner: "sjawhar"},
		{argv: []string{"repo", "create", "newrepo", "--private"}},
		{argv: []string{"repo", "list", "Sjawhar"}, owner: "sjawhar"},
		{argv: []string{"search", "issues", "--owner", "acme", "bug"}, owner: "acme"},
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion", "-R", "sjawhar/other"}, owner: "sjawhar"},
		{argv: []string{"secret", "set", "TOKEN", "--org", "acme"}, owner: "acme"},
		// A managed user's login ends in `_<shortcode>`, which is a GitHub login, and is keyed
		// lowercased as every owner is.
		{argv: []string{"pr", "view", "1", "-R", "Mona-Cat_Octo/widgets"}, owner: "mona-cat_octo"},
		// Refusals.
		{argv: []string{"pr", "view", "1", "--bogus"}, err: `"--bogus" is no option`},
		{argv: []string{"pr", "view", "1", "-Z"}, err: "names -Z"},
		{argv: []string{"pr", "nonexist"}, err: "gh has no command `gh pr nonexist`"},
		{argv: []string{"nonexist"}, err: `"nonexist" is no command gh's own help lists`},
		{argv: []string{"--bogus", "pr", "list"}, err: `"--bogus" comes before the command's name`},
		{argv: []string{"api", "repositories/12345"}, err: "by its id"},
		{argv: []string{"api", "https://ghe.example.com/api/v3/repos/acme/w"}, err: "another host"},
		{argv: []string{"api", "--hostname", "ghe.example.com", "repos/acme/w"}, err: "another host"},
		{argv: []string{"api", "repos/../x/y"}, err: `"..", which is no GitHub login`},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "../x/y"}, err: `names another host`},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "../y"}, err: "no GitHub login"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "widgets"}, err: "names no owner"},
		{argv: []string{"repo", "view", "widgets"}, err: "names no owner"},
		{argv: []string{"pr", "view", "1", "-R"}, err: "has no value"},
		{argv: []string{"pr", "view", "1", "-R="}, err: `"=" names no owner`},
		{argv: []string{"search", "issues", "--owner", "acme", "repo:sjawhar/x"}, err: "more than one owner"},
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"label", "clone", "acme/widgets", "-R", "sjawhar/legion"}, err: "more than one owner"},
		{argv: []string{"secret", "set", "TOKEN", "--org", "acme", "-R", "sjawhar/x"}, err: "more than one owner"},
		{argv: []string{"auth", "status", "-h", "ghe.example.com"}, err: "another host"},
	} {
		help, listed := tc.help, tc.remotes
		if help == nil {
			help = savedGhHelp
		}
		if listed == nil {
			listed = noGitRepository
		}
		target, err := ghTargetOfKept(t, tc.argv, tc.env, help, listed, ownersOwnDirectories)
		switch {
		case tc.err != "":
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ghTargetOf(%q, %v) = %+v, %v; want a refusal containing %q", tc.argv, tc.env, target, err, tc.err)
			}
		case err != nil || target.owner != tc.owner:
			t.Errorf("ghTargetOf(%q, %v) = %+v, %v; want the owner %q", tc.argv, tc.env, target, err, tc.owner)
		}
	}
}

// Every refusal names the argument it could not place and how to name the repository instead:
// -R, or, for gh api, which has none, the endpoint; for a gh alias, the command it stands for.
func TestGhTargetOfRefusalsSayHowToNameTheRepository(t *testing.T) {
	for argv, want := range map[string]string{
		"pr view 1 --bogus":              "pass -R <owner>/<repo>",
		"api repositories/12345":         "name it in the endpoint, repos/<owner>/<repo>/…",
		"pr --json n view 1":             "pass -R <owner>/<repo>",
		"co 1":                           "run the gh command it stands for, with -R <owner>/<repo>",
		"copilot":                        "run gh's own commands, with -R <owner>/<repo>",
		"search issues (repo:acme/w)":    "pass -R <owner>/<repo>",
		"api search/issues?q=(repo:a/b)": "name it in the endpoint",
	} {
		_, err := ghTargetOfKept(t, strings.Fields(argv), nil, savedGhHelp, noGitRepository, ownersOwnDirectories)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "sends no token") {
			t.Errorf("ghTargetOf(%q) = %v, want a refusal saying it sends no token and %q", argv, err, want)
		}
	}
}

// Beside the owner, ghTargetOf reads the repository a command names, which is the directory
// `legion gh` runs it with where the bundle holds one: a `<repository>` argument, -R, a pull
// request's URL, OWNER/REPO#N where the leaf reads it, a search repo: qualifier, `gh api`'s
// repos/<owner>/<repo>/… endpoint, GH_REPO and the working directory's remotes each name one.
// The name is keyed as the directory holding its token is named for it, so `Legion` and
// `widgets.git` are `legion` and `widgets`, and two spellings of one repository are one. They
// must all name the one repository: a command naming none, one naming two of the one owner, and
// one where a selector
// named the owner alone — an `<owner>` argument, orgs/<owner>/ or users/<owner>/, an
// owner:/org:/user: qualifier, a team, an owner option — name none, and the command runs as the
// owner.
func TestGhTargetOfNamesTheOneRepositoryEverySelectorNames(t *testing.T) {
	remotes := func(listed ...ghRemote) func() ([]ghRemote, error) {
		return func() ([]ghRemote, error) { return listed, nil }
	}
	acmeClone := ghRemote{name: "origin", url: "https://github.com/Acme/widgets.git"}
	for _, tc := range []struct {
		argv    []string
		env     map[string]string
		remotes func() ([]ghRemote, error) // a working directory that is no git repository when nil
		owner   string
		repo    string // "" where the selectors name no one repository
	}{
		// One repository, named by each selector that names one.
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion"}, owner: "sjawhar", repo: "legion"},
		{argv: []string{"--repo=sjawhar/Legion", "pr", "view", "1"}, owner: "sjawhar", repo: "legion"},
		{argv: []string{"repo", "clone", "sjawhar/legion"}, owner: "sjawhar", repo: "legion"},
		{argv: []string{"repo", "clone", "github.com/sjawhar/legion"}, owner: "sjawhar", repo: "legion"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/2/files"}, owner: "sjawhar", repo: "legion"},
		{argv: []string{"search", "issues", "repo:sjawhar/legion", "is:open"}, owner: "sjawhar", repo: "legion"},
		{argv: []string{"api", "repos/sjawhar/legion/pulls/1/files"}, owner: "sjawhar", repo: "legion"},
		{argv: []string{"api", "REPOS/sjawhar/Legion"}, owner: "sjawhar", repo: "legion"},
		{argv: []string{"api", "search/issues?q=repo:sjawhar/legion+is:open"}, owner: "sjawhar", repo: "legion"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "sjawhar/Legion"}, owner: "sjawhar", repo: "legion"},
		{argv: []string{"api", "repos/{owner}/{repo}/pulls"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, owner: "sjawhar", repo: "legion"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone), owner: "acme", repo: "widgets"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "origin", resolved: "base"}), owner: "acme", repo: "widgets"},
		{argv: []string{"pr", "list"}, remotes: remotes(ghRemote{name: "origin", resolved: "acme/widgets"}), owner: "acme", repo: "widgets"},
		// The same repository named twice is still the one: the leaf's own argument beside the
		// working directory's remote, and an option's URL beside -R.
		{argv: []string{"pr", "view", "1", "-R", "acme/widgets"}, remotes: remotes(acmeClone), owner: "acme", repo: "widgets"},
		{argv: []string{"pr", "comment", "1", "--body", "https://github.com/acme/widgets/pull/9", "-R", "acme/widgets"}, owner: "acme", repo: "widgets"},
		// Two spellings of the one repository are one repository, not two: the pull request's
		// URL spells it as GitHub shows it and -R as the bundle's directory is named, and a
		// remote's clone URL carries the `.git` the directory's name has not.
		{argv: []string{"pr", "view", "https://github.com/sjawhar/Legion-Smoke/pull/7", "-R", "sjawhar/legion-smoke"}, owner: "sjawhar", repo: "legion-smoke"},
		{argv: []string{"pr", "view", "1", "-R", "acme/Widgets"}, remotes: remotes(acmeClone), owner: "acme", repo: "widgets"},
		// Two repositories of the one owner: the command reaches both, so it runs as the owner.
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion", "-R", "sjawhar/other"}, owner: "sjawhar"},
		{argv: []string{"issue", "edit", "5", "--add-blocked-by", "https://github.com/acme/other/issues/9"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"pr", "view", "https://github.com/acme/other/tree/x"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"secret", "set", "TOKEN", "-o", "sjawhar", "-r", "legion,smoke", "--body", "dummy"}, owner: "sjawhar"},
		// A selector naming the owner alone: whatever else of the owner's gh resolves is in
		// reach, so the one repository another selector names is not the command's alone.
		{argv: []string{"repo", "list", "Sjawhar"}, owner: "sjawhar"},
		{argv: []string{"api", "orgs/acme/teams"}, owner: "acme"},
		{argv: []string{"api", "users/sjawhar/repos"}, owner: "sjawhar"},
		{argv: []string{"search", "issues", "--owner", "acme", "bug"}, owner: "acme"},
		{argv: []string{"search", "issues", "org:sjawhar"}, owner: "sjawhar"},
		{argv: []string{"project", "link", "1", "--team", "sjawhar/core"}, owner: "sjawhar"},
		{argv: []string{"secret", "set", "TOKEN", "--org", "acme"}, owner: "acme"},
		{argv: []string{"api", "graphql", "-f", "query=q", "-F", "owner=Sjawhar", "-F", "name=legion"}, owner: "sjawhar"},
		{argv: []string{"api", "repos/sjawhar/{repo}/pulls"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, owner: "sjawhar"},
		// A command naming no owner at all names no repository either.
		{argv: []string{"pr", "list"}},
		{argv: []string{"api", "user"}},
	} {
		listed := tc.remotes
		if listed == nil {
			listed = noGitRepository
		}
		target, err := ghTargetOfKept(t, tc.argv, tc.env, savedGhHelp, listed, ownersOwnDirectories)
		if err != nil || target.owner != tc.owner || target.repo != tc.repo {
			t.Errorf("ghTargetOf(%q, %v) = %+v, %v; want the owner %q and the repository %q", tc.argv, tc.env, target, err, tc.owner, tc.repo)
		}
	}
}

// noGitRepository is the git remotes of a working directory that is no git repository: none.
func noGitRepository() ([]ghRemote, error) { return nil, nil }

// ownersOwnDirectories is the routing of a bundle under /owners that holds a directory for every
// owner and configures no rename (ghConfigDirFor's answer over such a bundle): every repository,
// and every owner named alone, is answered from its owner's own directory, so a command naming
// two owners routes to two directories.
func ownersOwnDirectories(owner, _ string) (string, error) {
	return ghconfig.Dir("/owners", owner), nil
}

// gh reads the repository of a command naming none from the working directory's git remotes, and
// readGitRemotes reads them with git as gh does: every fetch and push URL `git remote -v` prints,
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
	if remotes, err := readGitRemotes(); err != nil || remotes != nil {
		t.Errorf("readGitRemotes outside a git repository = %v, %v; want none", remotes, err)
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
		// readGitRemotes reads the process's working directory, so the fixture runs in the same
		// directory by name rather than inheriting whatever the process has, and it carries an
		// environment of its own rather than the test process's. The two t.Setenv lines above are
		// not for this git: readGitRemotes runs in-process, in the test binary, and reads them
		// there, while fixtureenv.Isolated keeps this git off any repository, configuration or
		// credential a GIT_ variable planted back after the guard could name.
		fixture.Run(repository, append([]string{"git"}, args...)...)
	}
	remotes, err := readGitRemotes()
	want := []ghRemote{
		{name: "origin", url: "https://github.com/acme/widgets.git"},
		{name: "origin", url: "https://github.com/acme/pushed.git"},
		{name: "upstream", url: "git@github.com:sjawhar/legion"},
		{name: "upstream", url: "git@github.com:sjawhar/legion"},
		{name: "origin", resolved: "base"},
	}
	if err != nil || !slices.Equal(remotes, want) {
		t.Errorf("readGitRemotes = %+v, %v; want %+v", remotes, err, want)
	}
}

// git prints a remote's URL and partial clone filter as they are configured, so whitespace in
// either lets gh's greedy reading of the line take another URL than the one before its first
// ` (`: readGitRemotes refuses such a line, naming it, rather than count an owner gh may not act
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
				// The fixture configures the repository readGitRemotes reads, which is the
				// directory this case chdirs into, named here rather than inherited, and its
				// environment is its own too, for the reason the test above gives.
				fixture.Run(repository, append([]string{"git"}, args...)...)
			}
			if remotes, err := readGitRemotes(); err == nil || !strings.Contains(err.Error(), "git remote -v printed") || !strings.Contains(err.Error(), "evilown") {
				t.Errorf("readGitRemotes = %+v, %v; want the line naming evilown refused", remotes, err)
			}
		})
	}
}

// legionGh runs this test binary as `legion gh args…` in its own process, since a run execs the
// real gh in place of the process, with env (and nothing of this process's environment but PATH),
// in a working directory that is no git repository.
func legionGh(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return legionGhIn(t, t.TempDir(), env, "", args...)
}

// ghWrapperDir is a directory holding a pane's `gh` wrapper, a script running this test binary as
// `legion gh`, for the head of a test's PATH.
func ghWrapperDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexec '"+os.Args[0]+"' gh \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// legionCommand is the one environment a `legion` command of these tests is given: this test
// binary as `legion args…` in the working directory dir, with env and nothing of this process's
// own but PATH, whose head is a pane's `gh` wrapper (ghWrapperDir) unless env names a PATH of its
// own. A test that drives the run itself — one that signals it, or starts it under a shell of its
// own — takes the command and runs it, so that base environment is built in one place.
func legionCommand(t *testing.T, dir string, env map[string]string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = dir
	cmd.Env = []string{testMainEnv + "=1", "PATH=" + ghWrapperDir(t) + ":" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	return cmd
}

// testGhChildEnviron is what this package's test binary, run as `legion`, hands gh beside the
// allowlist (ghChildTestEnviron): testMainEnv, so the `gh` wrapper gh's git runs as its credential
// helper runs this binary as `legion` again; every variable of environ the stand-in real gh of
// these tests reads (GH_STANDIN_*); and the pairs testGhChildEnv names. None of these is one
// ghChildEnviron keeps, which is the point of naming them here and nowhere in the binary. The
// stand-ins tell the guard's visibility read by its argv, as the real gh would have to
// (ghStandInPrefix), so no Legion variable naming that read is handed on.
func testGhChildEnviron(environ []string) []string {
	out := []string{testMainEnv + "=1"}
	for _, pair := range environ {
		if name, _, _ := strings.Cut(pair, "="); strings.HasPrefix(name, "GH_STANDIN_") {
			out = append(out, pair)
		}
	}
	for _, line := range strings.Split(os.Getenv(testGhChildEnv), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// legionGhCommand is the command legionGhIn runs: legionCommand's `legion gh args…`.
func legionGhCommand(t *testing.T, dir string, env map[string]string, args ...string) *exec.Cmd {
	t.Helper()
	return legionCommand(t, dir, env, append([]string{"gh"}, args...)...)
}

// legionGhIn runs `legion gh args…` as legionGh does, in the working directory dir with stdin,
// under the environment legionGhCommand builds, and answers what it exited with and printed.
func legionGhIn(t *testing.T, dir string, env map[string]string, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := legionGhCommand(t, dir, env, args...)
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &out, &errb
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), out.String(), errb.String()
	}
	if err != nil {
		t.Fatalf("legion gh %v: %v", args, err)
	}
	return 0, out.String(), errb.String()
}

// `legion gh` execs the real gh (LEGION_GH_PATH) as `gh`, with its arguments untouched and
// GH_CONFIG_DIR pointed at the directory of the owner the command names — one naming one of
// that owner's repositories, two of them, and one where a selector named the owner alone run
// with that one directory alike — and at the issue's owner's (the GH_CONFIG_DIR it was
// given) when the command names no owner, reading --help=false as no help. It
// reads each command from the real gh's own help. The real gh runs with GH_PATH naming the `gh`
// wrapper first on PATH, so the git gh runs asks `legion gh auth git-credential`, which answers by
// the path's owner as `legion git-credential` does; with useHttpPath on for github.com, and gh's
// update check and prompts off. A command naming an owner the bundle holds no directory for is
// refused, exit 2, naming the owner, and so are one it cannot read, a gh alias, and any command
// when PATH's first gh is the real gh; the real gh never runs. In a clone of an owner's
// repository, a command naming none runs as that repository's. Each of the three variables it needs is
// required, and an unset one is named with exit 2; --help with no real gh to hand it to is
// legion's usage.
func TestLegionGhExecsTheRealGhWithTheOwnersDirectory(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	// The stub answers `[<command>] --help` with the saved help of that command, gh's own for none,
	// as the real gh would, and every other run with its argv and GH_CONFIG_DIR, then the rest of
	// the environment `legion gh` gives it.
	stub := filepath.Join(t.TempDir(), "gh")
	script := ghStandInPrefix(help) +
		"printf '%s|%s|%s\\n' \"$0\" \"$GH_CONFIG_DIR\" \"$*\"\n" +
		"printf 'env %s|%s|%s=%s|%s=%s|%s=%s|%s=%s|%s=%s|%s|%s|%s|%s|%s|%s\\n' \"$GH_PATH\" \"$GIT_CONFIG_COUNT\" " +
		"\"$GIT_CONFIG_KEY_0\" \"$GIT_CONFIG_VALUE_0\" \"$GIT_CONFIG_KEY_1\" \"$GIT_CONFIG_VALUE_1\" \"$GIT_CONFIG_KEY_2\" \"$GIT_CONFIG_VALUE_2\" " +
		"\"$GIT_CONFIG_KEY_3\" \"$GIT_CONFIG_VALUE_3\" \"$GIT_CONFIG_KEY_4\" \"$GIT_CONFIG_VALUE_4\" " +
		"\"$GH_NO_UPDATE_NOTIFIER\" \"$GH_PROMPT_DISABLED\" \"$GH_EDITOR\" \"$GH_TELEMETRY\" \"$GIT_CONFIG_GLOBAL\" \"$GIT_CONFIG_NOSYSTEM\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	wrappers := ghWrapperDir(t)
	// Every call passes the leak guard before gh runs, and a guard that can resolve no shared
	// check refuses it, so these runs carry a stand-in check that clears every call
	// (ghCheckPasses): what this test reads is which directory's token gh is run with, not what
	// the guard refuses, which TestLegionGhChecksTheCallWithTheLeakGuard reads.
	env := map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"PATH": wrappers + ":" + os.Getenv("PATH"), "PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
	}
	// gh's git gets http.sslVerify on, submodule recursion off and useHttpPath for github.com,
	// reads no global or system file, and gh gets an editor that fails and its telemetry off
	// (ghChildForced, ghForcedGitConfig).
	wantEnv := "env " + filepath.Join(wrappers, "gh") + "|5|http.sslVerify=true|submodule.recurse=false|fetch.recurseSubmodules=false|push.recurseSubmodules=no|credential.https://github.com.useHttpPath=true|1|1|" + ghNoEditor + "|0|/dev/null|1\n"
	for _, tc := range []struct {
		args []string
		dir  string
	}{
		{[]string{"api", "repos/sjawhar/legion-smoke", "--jq", ".full_name"}, filepath.Join(root, "sjawhar")},
		{[]string{"pr", "create", "-R", "acme/widgets", "--fill"}, filepath.Join(root, "acme")},
		{[]string{"pr", "list"}, defaultDir},
		{[]string{"pr", "view", "1", "-R", "acme/widgets", "--help=false"}, filepath.Join(root, "acme")},
		{[]string{"api", "repos/sjawhar/legion-smoke", "--help=false", "-q", "."}, filepath.Join(root, "sjawhar")},
		// A command naming one repository, by -R and by `gh api`'s endpoint, is its owner's
		// directory's too: the owner's lease reaches every repository its installation holds,
		// so the bundle holds no narrower token to run such a command with.
		{[]string{"pr", "view", "1", "-R", "sjawhar/legion"}, filepath.Join(root, "sjawhar")},
		{[]string{"api", "repos/sjawhar/legion/pulls/1", "--jq", ".title"}, filepath.Join(root, "sjawhar")},
		{[]string{"issue", "transfer", "3", "sjawhar/legion", "-R", "sjawhar/legion-smoke"}, filepath.Join(root, "sjawhar")},
		{[]string{"repo", "list", "sjawhar"}, filepath.Join(root, "sjawhar")},
		// A repository of an owner the project maps nothing of: the bundle holds that owner's
		// directory because the role's App is installed on the account, and the token in it
		// reaches every repository of that installation.
		{[]string{"pr", "create", "-R", "theorem-labs/agent-infra", "--fill"}, filepath.Join(root, "theorem-labs")},
	} {
		code, stdout, stderr := legionGh(t, env, tc.args...)
		want := stub + "|" + tc.dir + "|" + strings.Join(tc.args, " ") + "\n" + wantEnv
		if code != 0 || stdout != want {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and %q", tc.args, code, stdout, stderr, want)
		}
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"pr", "view", "https://github.com/other/thing/pull/1"}, `the role's GitHub App is not installed on "other"`},
		{[]string{"api", "repos/Other/thing"}, `the role's GitHub App is not installed on "other"`},
		{[]string{"pr", "view", "1", "--bogus"}, `"--bogus" is no option`},
		{[]string{"issue", "lock", "1"}, "cannot read `gh issue lock`"},
		{[]string{"co", "1"}, `"co" is a gh alias`},
	} {
		code, stdout, stderr := legionGh(t, env, tc.args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 containing %q and no gh run", tc.args, code, stdout, stderr, tc.want)
		}
	}

	// gh's own git asks the wrapper, which answers with the token of the owner in git's path.
	for path, token := range map[string]string{"sjawhar/legion-smoke.git": "ghs_sjawhar_write", "acme/widgets.git": "ghs_acme_write"} {
		code, stdout, stderr := legionGhIn(t, t.TempDir(), env, "protocol=https\nhost=github.com\npath="+path+"\n\n", "auth", "git-credential", "get")
		if want := "username=x-access-token\npassword=" + token + "\n"; code != 0 || stdout != want {
			t.Errorf("legion gh auth git-credential get for %s = exit %d, stdout %q, stderr %q; want %q", path, code, stdout, stderr, want)
		}
	}
	if code, stdout, _ := legionGhIn(t, t.TempDir(), env, "protocol=https\nhost=github.com\n\n", "auth", "git-credential", "get"); code == 0 || stdout != "" {
		t.Errorf("legion gh auth git-credential get with no path = exit %d, stdout %q; want no answer", code, stdout)
	}
	// With the real gh first on PATH, gh's own git would answer with GH_CONFIG_DIR's token.
	unwrapped := map[string]string{"PATH": filepath.Dir(stub) + ":" + os.Getenv("PATH")}
	maps.Copy(unwrapped, map[string]string{"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root})
	if code, stdout, stderr := legionGh(t, unwrapped, "pr", "list"); code != 2 || stdout != "" || !strings.Contains(stderr, "is not the wrapper that runs `legion gh`") {
		t.Errorf("legion gh with the real gh first on PATH = exit %d, stdout %q, stderr %q; want exit 2 naming it", code, stdout, stderr)
	}
	// Nor may another wrapper of the machine's own lead PATH: gh would make it its git helper.
	shims := t.TempDir()
	if err := os.WriteFile(filepath.Join(shims, "gh"), []byte("#!/bin/sh\nGH_TOKEN=$(cat ~/.token) exec '"+stub+"' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	shimmed := map[string]string{"PATH": shims + ":" + wrappers + ":" + os.Getenv("PATH")}
	maps.Copy(shimmed, map[string]string{"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root})
	if code, stdout, stderr := legionGh(t, shimmed, "pr", "list"); code != 2 || stdout != "" || !strings.Contains(stderr, "is not the wrapper that runs `legion gh`") {
		t.Errorf("legion gh with another gh wrapper first on PATH = exit %d, stdout %q, stderr %q; want exit 2 naming it", code, stdout, stderr)
	}

	// In a clone of sjawhar's repository, a command naming none acts on sjawhar's repository.
	clone := t.TempDir()
	fixture := fixturegit.Runner{TB: t, Env: []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}}
	for _, args := range [][]string{fixturegit.InitWorkTree("", "")[1:], {"remote", "add", "origin", "https://github.com/sjawhar/legion-smoke"}} {
		fixture.Run(clone, append([]string{"git"}, args...)...)
	}
	for _, args := range [][]string{{"pr", "list"}, {"pr", "list", "-R", "sjawhar/legion-smoke"}} {
		if code, stdout, stderr := legionGhIn(t, clone, env, "", args...); code != 0 || !strings.HasPrefix(stdout, stub+"|"+filepath.Join(root, "sjawhar")+"|") {
			t.Errorf("legion gh %q in that clone = exit %d, stdout %q, stderr %q; want sjawhar's directory", args, code, stdout, stderr)
		}
	}
	if code, stdout, stderr := legionGhIn(t, clone, env, "", "pr", "list", "-R", "acme/widgets"); code != 0 || !strings.HasPrefix(stdout, stub+"|"+filepath.Join(root, "acme")+"|") {
		t.Errorf("legion gh pr list -R acme/widgets in that clone = exit %d, stdout %q, stderr %q; want acme's directory", code, stdout, stderr)
	}

	for _, missing := range []string{"LEGION_GH_PATH", "GH_CONFIG_DIR", "LEGION_GH_OWNERS_DIR"} {
		partial := map[string]string{}
		for name, value := range env {
			if name != missing {
				partial[name] = value
			}
		}
		code, stdout, stderr := legionGh(t, partial, "pr", "list")
		if code != 2 || stdout != "" || !strings.Contains(stderr, missing+" is unset") {
			t.Errorf("legion gh without %s = exit %d, stdout %q, stderr %q; want exit 2 naming it", missing, code, stdout, stderr)
		}
	}

	code, _, stderr := legionGh(t, map[string]string{}, "--help")
	if code != 0 || !usageOf(stderr, "gh") {
		t.Errorf("legion gh --help with no LEGION_GH_PATH = exit %d, stderr %q; want exit 0 and its usage", code, stderr)
	}
}

// `legion gh` never runs itself as the real gh. A LEGION_GH_PATH that is not an absolute path,
// which exec would look up on PATH, whose first gh is the wrapper, is refused, exit 2, before
// anything runs, and so is one that is the wrapper first on PATH, or another script of the
// wrapper's form. One that leads back to `legion gh` only when it runs — a machine's own gh shim
// that hands every call to the next gh on PATH, which is the wrapper, as the operator's knives
// shim did — reaches `legion gh` once more, under the variable every real gh this command runs is
// given (leakguard.RunByRealGhVar), and that run refuses, exit 2, so the help read fails and
// nothing runs again. gh's own git credential helper still answers under that variable, and the
// real gh a call runs is given it. The `legion` the wrappers here run counts its starts and stops
// a chain at five, so code that loops fails this test rather than running away; before the
// refusals, each case below started `legion gh` five times.
//
// The bound holds under a caller's GH_TOKEN, with which the guard's own visibility reads skip the
// help reads (ghVisibilityRead): the read's real gh, the shim, leads back once and that run
// refuses. It holds through the leak guard too, which blanks the variable for its children. The
// guard's own read for a push a real gh makes, whose `legion leak-check pre-push` runs under the
// variable, is two `legion gh` starts deep: the one the read runs through the wrapper, and the one
// the shim that read runs, for its help read or for the read itself under the token, leads back
// to, which refuses. The shared check's own read, which the check makes itself, is three deep: the
// `legion gh` that answers it (answerVisibilityRead), the one that answer's REST read runs through
// the wrapper, and the one that read's shim leads back to, which refuses. A refusal that let the
// guard's reads through under the variable loops under the token until the cap stops it.
func TestLegionGhRefusesARealGhThatRunsLegionGhAgain(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	starts := filepath.Join(t.TempDir(), "starts")
	legion := filepath.Join(t.TempDir(), "legion")
	counting := "#!/bin/sh\necho start >>'" + starts + "'\n[ \"$(wc -l <'" + starts + "')\" -le 4 ] || exit 97\n" +
		testMainEnv + "=1 exec '" + os.Args[0] + "' \"$@\"\n"
	wrappers, elsewhere := t.TempDir(), t.TempDir()
	shim := filepath.Join(t.TempDir(), "gh")
	for path, script := range map[string]string{
		legion:                         counting,
		filepath.Join(wrappers, "gh"):  "#!/bin/sh\nexec '" + legion + "' gh \"$@\"\n",
		filepath.Join(elsewhere, "gh"): "#!/bin/sh\nexec '" + legion + "' gh \"$@\"\n",
		shim:                           "#!/bin/sh\nexec '" + filepath.Join(wrappers, "gh") + "' \"$@\"\n",
	} {
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	startsOf := func(t *testing.T) int {
		t.Helper()
		raw, err := os.ReadFile(starts)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(raw), "start\n")
	}
	envFor := func(gh string) map[string]string {
		return map[string]string{
			"LEGION_GH_PATH": gh, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
			"PATH": wrappers + ":" + os.Getenv("PATH"), "PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
		}
	}
	for _, tc := range []struct {
		name, gh, want string
		starts         int
	}{
		{"a bare name", "gh", `LEGION_GH_PATH is not an absolute path: "gh"`, 0},
		{"the wrapper first on PATH", filepath.Join(wrappers, "gh"), "is a wrapper that runs `legion gh`", 0},
		{"a wrapper elsewhere", filepath.Join(elsewhere, "gh"), "is a wrapper that runs `legion gh`", 0},
		{"a shim that runs the wrapper", shim, "the real gh " + shim + " ran `legion gh` again", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Remove(starts); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			code, stdout, stderr := legionGh(t, envFor(tc.gh), "pr", "list")
			if got := startsOf(t); code != 2 || stdout != "" || !strings.Contains(stderr, tc.want) || got != tc.starts {
				t.Errorf("legion gh pr list with LEGION_GH_PATH %s = exit %d, stdout %q, stderr %q, and `legion gh` started %d more times; want exit 2 containing %q and %d more starts",
					tc.gh, code, stdout, stderr, got, tc.want, tc.starts)
			}
		})
	}

	pushDir, pushLine := aPushFixture(t, "a commit of nothing in particular")
	const slug = "acme/widgets"
	for _, tc := range []struct {
		name  string
		token bool
		// args is the `legion gh` call the case makes, and nil for the pre-push hook of a push
		// to slug that the shim, as the real gh, makes.
		args   []string
		starts int
	}{
		{"the guard's REST read under GH_TOKEN", true, leakguard.VisibilityReadWithHeadersArgs(slug), 1},
		{"the guard's GraphQL read under GH_TOKEN", true, leakguard.VisibilityGraphQLArgs(slug), 1},
		{"the pre-push hook of a push the real gh makes", false, nil, 2},
		{"the pre-push hook of a push the real gh makes under GH_TOKEN", true, nil, 2},
	} {
		t.Run("a shim that runs the wrapper: "+tc.name, func(t *testing.T) {
			if err := os.Remove(starts); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			env := envFor(shim)
			if tc.token {
				env["GH_TOKEN"] = "ghs_caller"
			}
			var code int
			var stdout, stderr string
			if tc.args != nil {
				env[leakguard.VisibilityReadVar] = slug
				code, stdout, stderr = legionGh(t, env, tc.args...)
			} else {
				env[leakguard.RunByRealGhVar] = shim
				code, stderr = legionLeakCheckPrePush(t, pushDir, env, pushLine, "https://github.com/"+slug+".git")
			}
			want := "the real gh " + shim + " ran `legion gh` again"
			if got := startsOf(t); code != 2 || stdout != "" || !strings.Contains(stderr, want) || got != tc.starts {
				t.Errorf("%s with LEGION_GH_PATH %s = exit %d, stdout %q, stderr %q, and `legion gh` started %d more times; want exit 2 containing %q and %d more starts",
					tc.name, shim, code, stdout, stderr, got, want, tc.starts)
			}
		})
	}

	// The shared check's own read of slug, made by a stand-in check of the hook of a push the real
	// gh makes. The push names a local path, so the hook's guard reads no visibility of its own and
	// the check's read is where every start comes from. The `legion gh` that answers the read finds
	// no visibility, since the real gh its read reaches leads back, and answers exit 1, which the
	// check records with what that `legion gh` said.
	for _, token := range []bool{false, true} {
		name := "a shim that runs the wrapper: the shared check's own read in the hook of a push the real gh makes"
		if token {
			name += " under GH_TOKEN"
		}
		t.Run(name, func(t *testing.T) {
			if err := os.Remove(starts); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			record := filepath.Join(t.TempDir(), "read")
			readsVisibility := "cat >/dev/null 2>&1\n" +
				leakguard.VisibilityReadVar + "=" + slug + " gh " + strings.Join(leakguard.VisibilityReadArgs(slug), " ") + " >/dev/null 2>'" + record + "'\n" +
				"echo \"exit $?\" >'" + record + ".status'\nexit 0\n"
			env := envFor(shim)
			env["PUBLIC_LEAK_CHECK"] = leakguardtest.Write(t, t.TempDir(), readsVisibility)
			env[leakguard.RunByRealGhVar] = shim
			if token {
				env["GH_TOKEN"] = "ghs_caller"
			}
			code, printed := legionLeakCheckPrePush(t, pushDir, env, pushLine, pushDir+".git")
			if code != leakguard.StatusClean || printed != "" {
				t.Errorf("legion leak-check pre-push = exit %d, stderr %q; want exit 0 and nothing printed", code, printed)
			}
			want := "the real gh " + shim + " ran `legion gh` again"
			if status, said, got := readRecord(t, record+".status"), readRecord(t, record), startsOf(t); status != "exit 1\n" || !strings.Contains(said, want) || got != 3 {
				t.Errorf("the shared check's own read with LEGION_GH_PATH %s = %q, stderr %q, and `legion gh` started %d more times; want exit 1 containing %q and 3 more starts",
					shim, status, said, got, want)
			}
		})
	}

	// gh's own git asks the wrapper under the variable, and the wrapper still answers.
	marked := envFor(shim)
	marked[leakguard.RunByRealGhVar] = shim
	if code, stdout, stderr := legionGhIn(t, t.TempDir(), marked, "protocol=https\nhost=github.com\npath=acme/widgets.git\n\n", "auth", "git-credential", "get"); code != 0 || stdout != "username=x-access-token\npassword=ghs_acme_write\n" {
		t.Errorf("legion gh auth git-credential get under %s = exit %d, stdout %q, stderr %q; want acme's token", leakguard.RunByRealGhVar, code, stdout, stderr)
	}

	// The real gh a call runs is given the variable, naming itself.
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)+"printf 'run by %s\\n' \"$"+leakguard.RunByRealGhVar+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := legionGh(t, envFor(stub), "pr", "list"); code != 0 || stdout != "run by "+stub+"\n" {
		t.Errorf("legion gh pr list with a real gh = exit %d, stdout %q, stderr %q; want exit 0 and %q", code, stdout, stderr, "run by "+stub+"\n")
	}
}

// The `gh` each runtime puts first on PATH, the image's packages/daemon/docker/gh and the script a
// tmux pane's launcher directory holds (workerbin), is one `legion gh` accepts as its wrapper
// (ghWrapperScript), so the git gh runs names it as its helper; a machine's own wrapper is not.
func TestTheRuntimesGhWrappersAreOnesLegionGhAccepts(t *testing.T) {
	image, err := os.ReadFile(filepath.Join("..", "..", "docker", "gh"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := workerbin.Install(root, "/opt/legion state/legion", "/usr/bin:/bin"); err != nil {
		t.Fatal(err)
	}
	pane, err := os.ReadFile(filepath.Join(workerbin.LauncherDir(root), "gh"))
	if err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string][]byte{"the image's": image, "a tmux pane's": pane} {
		if !ghWrapperScript.Match(script) {
			t.Errorf("%s gh wrapper %q is not one legion gh accepts", name, script)
		}
	}
	for _, script := range []string{"#!/bin/sh\nexec \"/opt/legion state/legion\" gh \"$@\"\n", "#!/bin/sh\nexec '/opt/legion'\\''s/legion' gh \"$@\"\n", "#!/bin/sh\nexec /opt/legion/bin/legion gh \"$@\"\n"} {
		if !ghWrapperScript.MatchString(script) {
			t.Errorf("legion gh refuses %q as its wrapper", script)
		}
	}
	for _, script := range []string{
		"#!/bin/sh\nGH_TOKEN=x exec /usr/bin/gh \"$@\"\n", "#!/bin/sh\nexec /usr/bin/gh \"$@\"\n", "\x7fELF",
		"#!/bin/sh\nexec \"$HOME/legion\" gh \"$@\"\n", "#!/bin/sh\nexec $(cat /x) gh \"$@\"\n", "#!/bin/sh\nexec `cat /x` gh \"$@\"\n",
		// Two commands, a redirect, a tilde and a glob: shell, not one literal path.
		"#!/bin/sh\nexec /x|/bin/sh gh \"$@\"\n", "#!/bin/sh\nexec /x&/bin/sh gh \"$@\"\n", "#!/bin/sh\nexec /x;/bin/sh gh \"$@\"\n",
		"#!/bin/sh\nexec /x>/dev/null gh \"$@\"\n", "#!/bin/sh\nexec ~/legion gh \"$@\"\n", "#!/bin/sh\nexec /opt/*/legion gh \"$@\"\n",
		// An option to the shell's exec, and a path relative to wherever gh runs.
		"#!/bin/sh\nexec -a gh \"$@\"\n", "#!/bin/sh\nexec '-a' gh \"$@\"\n", "#!/bin/sh\nexec \"-a\" gh \"$@\"\n",
		"#!/bin/sh\nexec legion gh \"$@\"\n", "#!/bin/sh\nexec 'bin/legion' gh \"$@\"\n",
	} {
		if ghWrapperScript.MatchString(script) {
			t.Errorf("legion gh accepts %q as its wrapper", script)
		}
	}

	// The whole script is read, to the bound: a wrapper at the bound passes, and one a byte past it
	// is refused by the bound alone, its first ghWrapperMaxBytes+1 bytes a whole wrapper, whatever
	// follows; the pattern's anchors refuse anything that goes on after a wrapper within it.
	wrapperOf := func(size int) string {
		head, tail := "#!/bin/sh\nexec '/", "' gh \"$@\"\n"
		return head + strings.Repeat("a", size-len(head)-len(tail)) + tail
	}
	atBound, pastBound := wrapperOf(ghWrapperMaxBytes), wrapperOf(ghWrapperMaxBytes+1)
	if !ghWrapperScript.MatchString(pastBound) {
		t.Fatalf("the pattern refuses a wrapper of %d bytes, so the bound is not what refuses it", len(pastBound))
	}
	for name, tc := range map[string]struct {
		script string
		want   bool
	}{
		"a wrapper at the bound":              {atBound, true},
		"a wrapper a byte past it":            {pastBound, false},
		"a wrapper a byte past it, then more": {pastBound + "rm -rf ~\n", false},
		"a wrapper at the bound, then more":   {atBound + "rm -rf ~\n", false},
		"a short wrapper, then more":          {"#!/bin/sh\nexec '/opt/legion/bin/legion' gh \"$@\"\nrm -rf ~\n", false},
		"a wrapper of a short path":           {"#!/bin/sh\nexec '/opt/legion/bin/legion' gh \"$@\"\n", true},
	} {
		if got, err := isGhWrapper(strings.NewReader(tc.script)); err != nil || got != tc.want {
			t.Errorf("isGhWrapper(%s, %d bytes) = %t, %v; want %t", name, len(tc.script), got, err, tc.want)
		}
	}
}

// The git gh runs reads credential.useHttpPath from GIT_CONFIG_COUNT's entries, added after any
// the environment already holds and in place of a stale entry at the next index; a count that is
// no number is refused.
func TestWithGitHubUseHTTPPathAddsToGitsConfiguration(t *testing.T) {
	got, err := withGitHubUseHTTPPath([]string{"A=1", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.x", "GIT_CONFIG_VALUE_0=y", "GIT_CONFIG_KEY_1=stale", "GIT_CONFIG_VALUE_1=stale"})
	want := []string{"A=1", "GIT_CONFIG_KEY_0=core.x", "GIT_CONFIG_VALUE_0=y", "GIT_CONFIG_KEY_1=credential.https://github.com.useHttpPath", "GIT_CONFIG_VALUE_1=true", "GIT_CONFIG_COUNT=2"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("withGitHubUseHTTPPath = %q, %v; want %q", got, err, want)
	}
	if got, err := withGitHubUseHTTPPath([]string{"A=1"}); err != nil || !slices.Equal(got, []string{"A=1", "GIT_CONFIG_KEY_0=credential.https://github.com.useHttpPath", "GIT_CONFIG_VALUE_0=true", "GIT_CONFIG_COUNT=1"}) {
		t.Errorf("withGitHubUseHTTPPath with no count = %q, %v", got, err)
	}
	if _, err := withGitHubUseHTTPPath([]string{"GIT_CONFIG_COUNT=x"}); err == nil || !strings.Contains(err.Error(), "no count") {
		t.Errorf("withGitHubUseHTTPPath with GIT_CONFIG_COUNT=x = %v, want a refusal", err)
	}
}

// ghStandInPrefix is what a stand-in real gh of this package answers with first, and what every
// one of them carries after any line of its own that records its run (loggingGh). It answers
// `[<command>] --help` with the saved help of that command, gh's own for none, which is the help
// the command resolver reads; and it answers the leak guard's own visibility reads — `gh api
// --hostname github.com repos/<slug> [--include] --jq .visibility`, and the GraphQL read `gh api
// --hostname github.com graphql --include -f query=… -f owner=… -f name=…` — with a visibility,
// the word GH_STANDIN_VISIBILITY names or public, recording it in GH_STANDIN_RECORD where the test
// names one. It tells those reads by their argv, as the real gh would have to: `legion gh` runs gh
// from an allowlist of its environment, which holds no Legion variable naming the read
// (ghChildEnviron). Without that answer the guard's read of a destination's visibility would get
// a stand-in's ordinary output and refuse every call it cleared, since a visibility it cannot read
// is a setup failure.
func ghStandInPrefix(help string) string {
	return ghStandInHelp(help) +
		"case \" $* \" in\n" +
		ghVisibilityReadArgv + ")\n" +
		"  printf 'visibility %s\\n' \"$*\" >>\"${GH_STANDIN_RECORD:-/dev/null}\"\n" +
		"  echo \"${GH_STANDIN_VISIBILITY:-public}\"\n" +
		"  exit 0;;\n" +
		"esac\n"
}

// ghVisibilityReadArgv is the shell `case` pattern that matches the leak guard's own visibility
// reads by their argv, as a stand-in gh must tell them: the REST read, `gh api --hostname
// github.com repos/<slug> [--include] --jq .visibility`, and the GraphQL read, `gh api --hostname
// github.com graphql --include -f query=… -f owner=… -f name=…`, each matched against " $* ".
const ghVisibilityReadArgv = "\\ api\\ --hostname\\ github.com\\ *\\ --jq\\ .visibility\\ |\\ api\\ --hostname\\ github.com\\ graphql\\ --include\\ -f\\ query=*\\ -f\\ owner=*\\ -f\\ name=*\\ "

// ghStandInHelp is the start of every stand-in gh of this package: a shell script that answers
// `[<command>] --help` with the saved help of that command, gh's own for none, and falls through
// to what the caller appends for every other run.
func ghStandInHelp(help string) string {
	return "#!/bin/sh\nfor last; do :; done\n" +
		"if [ \"$last\" = --help ]; then\n" +
		"  path=$(printf '%s ' \"$@\" | sed -e 's/--help $//' -e 's/ $//' -e 's/ /_/g')\n" +
		"  exec cat '" + help + "'/\"${path:-gh}\".txt\nfi\n"
}

// ghRefusal is what the stand-in checks of this package print when they refuse a `legion gh`
// call: the check's own report, which is the whole of what a refusal says about it.
const ghRefusal = "LEAK CHECK: refusing the gh call: one of ours"

// The stand-in shared public leak checks `legion gh`'s guard is driven against, which are
// leakguardtest's. Each records the call it was given in LEAK_CHECK_RECORD, so a test can read
// whether the guard ran at all, and each reads its standard input, as the real check does, so
// nothing is left in a pipe when it exits. ghCheckRefuses answers as the check answers a write
// it refuses: its report, and exit 3.
var (
	ghCheckPasses  = leakguardtest.Recording(leakguardtest.Passes)
	ghCheckRefuses = leakguardtest.Recording(leakguardtest.Refusing(ghRefusal))
)

// ghCheckReading is leakguardtest.Reading under this package's own report: the check's two-go
// shape for a call whose body is on standard input, where the first run exits 4 to say the call
// reads standard input and the second, handed the buffer as a file, records what it read and
// ends with status — 0 for a call it clears, 3 for one it refuses.
func ghCheckReading(status int) string {
	report := ""
	if status != 0 {
		report = ghRefusal
	}
	return leakguardtest.Reading(status, report)
}

// readRecord is what a stand-in wrote into its record file, and "" where it never wrote one.
func readRecord(t *testing.T, path string) string {
	t.Helper()
	recorded, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(recorded)
}

// `legion gh` hands every call to the leak guard before it runs gh, and runs gh not at all for a
// call the guard refuses: a title, a body and a body on standard input that the shared check
// refuses are each exit 2 with the check's own report, and gh is never started. A call the check
// clears reaches gh with its arguments untouched, and gh's own exit status comes back through
// `legion gh` — the stand-in exits 7 and `legion gh` exits 7 — with the standard input the guard
// buffered handed to gh in this process's place. A call that writes nothing runs the check too,
// with no visibility read (TestLegionGhChecksAReadAndReadsNoVisibility).
//
// Two calls pass unchecked. `legion gh auth git-credential` answers git as it always did, with
// no check run at all, since reading its standard input would break git's protocol. And a
// visibility read of the shared check's or of the guard's own (the GraphQL read the guard makes
// when GitHub refuses its REST read for a spent rate limit) passes when it is exactly that read
// for exactly the repository PUBLIC_LEAK_CHECK_VISIBILITY_READ names — without it a pod's gh,
// which is this command, would call the check, whose own read would call it again, without end —
// and is not logged, since the call it reads for logs it. The variable makes no other call one
// of those reads: a write under it, another read under it, and the same read under a variable
// naming no repository are each checked like anything else. The shared check's own read this
// command answers itself, through the guard's REST read, which reaches gh.
//
// A push made with the caller's own token carries GH_TOKEN or GITHUB_TOKEN into the guard, and so
// into each of those reads. Each read runs under that token, for a repository of an owner the
// bundle holds no directory for too. The token widens nothing else. Under it, this command runs
// the guard's two visibility reads, the shared check's own read, the exact `gh api rate_limit` and
// `gh auth git-credential`, and refuses every other call before gh runs, naming the token. So
// another read under the variable, the guard's read under no variable, and the guard's read of
// another repository than the variable names are each refused.
func TestLegionGhChecksTheCallWithTheLeakGuard(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	// The gh of these cases records every run it is handed — its arguments and its standard
	// input — and exits 7, a status nothing in `legion gh` itself answers with, so a test can
	// read that gh's own status is what came back.
	stub := filepath.Join(t.TempDir(), "gh")
	script := ghStandInPrefix(help) +
		"printf 'gh %s\\n' \"$*\" >>\"$GH_STANDIN_RECORD\"\n" +
		"printf 'stdin %s\\n' \"$(cat)\" >>\"$GH_STANDIN_RECORD\"\n" +
		"exit 7\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	wrappers := ghWrapperDir(t)
	// call runs `legion gh args…` with stdin against a stand-in check whose body is check, and
	// answers its status, what it printed, what gh recorded and what the check recorded.
	call := func(t *testing.T, check, stdin string, extra map[string]string, args ...string) (code int, stdout, stderr, ghRan, checked string) {
		t.Helper()
		records := t.TempDir()
		ghRecord, checkRecord := filepath.Join(records, "gh"), filepath.Join(records, "check")
		env := map[string]string{
			"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
			"PATH": wrappers + ":" + os.Getenv("PATH"), "PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), check),
			"GH_STANDIN_RECORD": ghRecord, "LEAK_CHECK_RECORD": checkRecord,
		}
		maps.Copy(env, extra)
		code, stdout, stderr = legionGhIn(t, t.TempDir(), env, stdin, args...)
		return code, stdout, stderr, readRecord(t, ghRecord), readRecord(t, checkRecord)
	}
	ranGh := func(recorded string) bool { return strings.Contains(recorded, "gh ") }

	for _, tc := range []struct {
		name  string
		check string
		stdin string
		args  []string
	}{
		{"a title", ghCheckRefuses, "", []string{"pr", "create", "-R", "acme/widgets", "--title", "a title of this call"}},
		{"a body", ghCheckRefuses, "", []string{"pr", "create", "-R", "acme/widgets", "--body", "a body of this call"}},
		{"a body on standard input", ghCheckReading(3), "a body on standard input\n", []string{"pr", "create", "-R", "acme/widgets", "--body-file", "-"}},
	} {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			code, stdout, stderr, ghRan, checked := call(t, tc.check, tc.stdin, nil, tc.args...)
			if code != 2 || !strings.Contains(stderr, "LEAK CHECK: refusing") {
				t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 and the guard's refusal", tc.args, code, stdout, stderr)
			}
			if ranGh(ghRan) {
				t.Errorf("gh ran: %q", ghRan)
			}
			if checked == "" {
				t.Errorf("the shared check was never run for %q", tc.args)
			}
		})
	}

	t.Run("cleared: a call the check passes reaches gh, and gh's status comes back", func(t *testing.T) {
		args := []string{"pr", "create", "-R", "acme/widgets", "--title", "a title of this call", "--body", "a body of this call"}
		code, stdout, stderr, ghRan, checked := call(t, ghCheckPasses, "", nil, args...)
		if code != 7 {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want gh's own 7", args, code, stdout, stderr)
		}
		if want := "gh " + strings.Join(args, " ") + "\n"; !strings.Contains(ghRan, want) {
			t.Errorf("gh recorded %q, wants %q", ghRan, want)
		}
		if checked == "" {
			t.Errorf("the shared check was never run for %q", args)
		}
	})

	t.Run("cleared: the standard input the guard read is handed to gh", func(t *testing.T) {
		body := "a body on standard input\n"
		args := []string{"pr", "create", "-R", "acme/widgets", "--body-file", "-"}
		code, stdout, stderr, ghRan, checked := call(t, ghCheckReading(0), body, nil, args...)
		if code != 7 {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want gh's own 7", args, code, stdout, stderr)
		}
		if want := "stdin " + strings.TrimSuffix(body, "\n") + "\n"; !strings.Contains(ghRan, want) {
			t.Errorf("gh read %q on its standard input, want %q", ghRan, want)
		}
		if want := "checked stdin " + strings.TrimSuffix(body, "\n") + "\n"; !strings.Contains(checked, want) {
			t.Errorf("the check read %q, want %q", checked, want)
		}
	})

	t.Run("unchecked: auth git-credential", func(t *testing.T) {
		code, stdout, _, ghRan, checked := call(t, ghCheckRefuses, "protocol=https\nhost=github.com\npath=acme/widgets.git\n\n", nil, "auth", "git-credential", "get")
		if want := "username=x-access-token\npassword=ghs_acme_write\n"; code != 0 || stdout != want {
			t.Errorf("legion gh auth git-credential get = exit %d, stdout %q; want %q", code, stdout, want)
		}
		if checked != "" || ranGh(ghRan) {
			t.Errorf("the check recorded %q and gh %q, want neither run", checked, ghRan)
		}
	})

	t.Run("unchecked: the shared check's own visibility read", func(t *testing.T) {
		visibility := []string{"api", "--hostname", "github.com", "repos/sjawhar/legion", "--jq", ".visibility"}
		state := t.TempDir()
		code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", map[string]string{leakguard.VisibilityReadVar: "sjawhar/legion", "XDG_STATE_HOME": state}, visibility...)
		if code != 0 || stdout != "public\n" {
			t.Errorf("the check's own visibility read = exit %d, stdout %q, stderr %q; want exit 0 and the visibility", code, stdout, stderr)
		}
		if checked != "" {
			t.Errorf("the shared check ran for its own visibility read: %q", checked)
		}
		if want := "visibility " + strings.Join(leakguard.VisibilityReadWithHeadersArgs("sjawhar/legion"), " ") + "\n"; ghRan != want {
			t.Errorf("gh recorded %q, want the guard's REST read, %q", ghRan, want)
		}
		if log := readRecord(t, filepath.Join(state, "legion-gh", "calls.log")); log != "" {
			t.Errorf("the check's own visibility read was logged as a call of its own: %q", log)
		}
	})

	t.Run("unchecked: the guard's own GraphQL visibility read", func(t *testing.T) {
		graphql := leakguard.VisibilityGraphQLArgs("sjawhar/legion")
		code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", map[string]string{leakguard.VisibilityReadVar: "sjawhar/legion"}, graphql...)
		if code != 0 || stdout != "public\n" {
			t.Errorf("the guard's own GraphQL read = exit %d, stdout %q, stderr %q; want exit 0 and gh's answer", code, stdout, stderr)
		}
		if checked != "" {
			t.Errorf("the shared check ran for the guard's own GraphQL read: %q", checked)
		}
		if want := "visibility " + strings.Join(graphql, " ") + "\n"; ghRan != want {
			t.Errorf("gh recorded %q, want %q", ghRan, want)
		}
	})

	// A push a real gh makes reaches the guard under the variable every real gh this command runs
	// is given (leakguard.RunByRealGhVar), through git's pre-push hook, and so does a shared check
	// that hook runs itself. The shared check's own read is answered under it, through the guard's
	// REST read, which reaches gh because the guard blanks the variable for its children, under a
	// caller's token as without one. The guard's own reads made under the variable are refused,
	// exit 2, and gh never runs: the guard runs none of them under it, so only a real gh that leads
	// back to this command makes one so.
	for _, token := range []string{"", "GH_TOKEN"} {
		under := "under " + leakguard.RunByRealGhVar
		marked := map[string]string{leakguard.RunByRealGhVar: stub, leakguard.VisibilityReadVar: "sjawhar/legion"}
		if token != "" {
			under += " and " + token
			marked[token] = "ghs_caller"
		}
		t.Run("unchecked "+under+": the shared check's own visibility read", func(t *testing.T) {
			args := leakguard.VisibilityReadArgs("sjawhar/legion")
			code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", marked, args...)
			if code != 0 || stdout != "public\n" {
				t.Errorf("legion gh %q %s = exit %d, stdout %q, stderr %q; want exit 0 and the visibility", args, under, code, stdout, stderr)
			}
			if checked != "" {
				t.Errorf("the shared check ran for its own visibility read: %q", checked)
			}
			if want := "visibility " + strings.Join(leakguard.VisibilityReadWithHeadersArgs("sjawhar/legion"), " ") + "\n"; ghRan != want {
				t.Errorf("gh recorded %q, want the guard's REST read, %q", ghRan, want)
			}
		})
		for _, read := range []struct {
			name string
			args []string
		}{
			{"REST", leakguard.VisibilityReadWithHeadersArgs("sjawhar/legion")},
			{"GraphQL", leakguard.VisibilityGraphQLArgs("sjawhar/legion")},
		} {
			t.Run("refused "+under+": the guard's own "+read.name+" visibility read", func(t *testing.T) {
				code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", marked, read.args...)
				if want := "the real gh " + stub + " ran `legion gh` again"; code != 2 || stdout != "" || !strings.Contains(stderr, want) {
					t.Errorf("legion gh %q %s = exit %d, stdout %q, stderr %q; want exit 2 containing %q", read.args, under, code, stdout, stderr, want)
				}
				if ghRan != "" || checked != "" {
					t.Errorf("gh recorded %q and the check %q, want neither run", ghRan, checked)
				}
			})
		}
	}

	for _, token := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		for _, slug := range []string{"sjawhar/legion", "octo/elsewhere"} {
			rest := "visibility " + strings.Join(leakguard.VisibilityReadWithHeadersArgs(slug), " ") + "\n"
			for _, read := range []struct {
				name string
				args []string
				want string
			}{
				{"the shared check's own visibility read", leakguard.VisibilityReadArgs(slug), rest},
				{"the guard's own REST visibility read", leakguard.VisibilityReadWithHeadersArgs(slug), rest},
				{"the guard's own GraphQL visibility read", leakguard.VisibilityGraphQLArgs(slug), "visibility " + strings.Join(leakguard.VisibilityGraphQLArgs(slug), " ") + "\n"},
			} {
				t.Run("unchecked under "+token+": "+read.name+" of "+slug, func(t *testing.T) {
					code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", map[string]string{token: "ghs_caller", leakguard.VisibilityReadVar: slug}, read.args...)
					if code != 0 || stdout != "public\n" {
						t.Errorf("legion gh %q under %s = exit %d, stdout %q, stderr %q; want exit 0 and the visibility", read.args, token, code, stdout, stderr)
					}
					if checked != "" {
						t.Errorf("the shared check ran for a visibility read: %q", checked)
					}
					if ghRan != read.want {
						t.Errorf("gh recorded %q, want %q", ghRan, read.want)
					}
				})
			}
		}
		for _, tc := range []struct {
			name string
			env  map[string]string
			args []string
		}{
			{"another read under the variable", map[string]string{leakguard.VisibilityReadVar: "sjawhar/legion"}, []string{"api", "--hostname", "github.com", "repos/sjawhar/legion", "--jq", ".name"}},
			{"the guard's read under no variable", nil, leakguard.VisibilityReadWithHeadersArgs("sjawhar/legion")},
			{"the guard's read of another repository than the variable's", map[string]string{leakguard.VisibilityReadVar: "sjawhar/other"}, leakguard.VisibilityReadWithHeadersArgs("sjawhar/legion")},
		} {
			t.Run("refused under "+token+": "+tc.name, func(t *testing.T) {
				extra := map[string]string{token: "ghs_caller"}
				maps.Copy(extra, tc.env)
				code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", extra, tc.args...)
				if want := token + " is set"; code != 2 || !strings.Contains(stderr, want) {
					t.Errorf("legion gh %q under %s = exit %d, stdout %q, stderr %q; want exit 2 and %q", tc.args, token, code, stdout, stderr, want)
				}
				if checked != "" || ghRan != "" {
					t.Errorf("the check recorded %q and gh %q; want neither run", checked, ghRan)
				}
			})
		}
	}

	for _, tc := range []struct {
		name string
		slug string
		args []string
	}{
		{"a write under the same variable", "sjawhar/legion", []string{"api", "--method", "PATCH", "--hostname", "github.com", "repos/sjawhar/legion", "--jq", ".visibility"}},
		{"another argv under the same variable", "sjawhar/legion", []string{"api", "--hostname", "github.com", "repos/sjawhar/legion", "--jq", ".name"}},
		{"a variable naming no repository", "sjawhar/legion/pulls", []string{"api", "--hostname", "github.com", "repos/sjawhar/legion/pulls", "--jq", ".visibility"}},
		{"the GraphQL read of another repository under the variable", "sjawhar/legion", leakguard.VisibilityGraphQLArgs("sjawhar/other")},
		{"another GraphQL query under the variable", "sjawhar/legion", graphQLOf("query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { description } }")},
		{"a GraphQL mutation under the variable", "sjawhar/legion", graphQLOf(inventedMutation)},
	} {
		t.Run("checked: "+tc.name, func(t *testing.T) {
			code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", map[string]string{leakguard.VisibilityReadVar: tc.slug}, tc.args...)
			if code != 2 || !strings.Contains(stderr, "LEAK CHECK: refusing") {
				t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 and the guard's refusal", tc.args, code, stdout, stderr)
			}
			if checked == "" || ranGh(ghRan) {
				t.Errorf("the check recorded %q and gh %q; want the call checked and gh not run", checked, ghRan)
			}
		})
	}
}

// graphQLOf is the guard's GraphQL visibility read of sjawhar/legion with query in place of the
// guard's own, and inventedMutation a mutation to put there: `legion gh` must check either one
// like any other call, whatever PUBLIC_LEAK_CHECK_VISIBILITY_READ names.
func graphQLOf(query string) []string {
	return []string{"api", "--hostname", "github.com", "graphql", "--include", "-f", "query=" + query, "-f", "owner=sjawhar", "-f", "name=legion"}
}

const inventedMutation = "mutation($owner: String!, $name: String!) { createIssue(input: {repositoryId: $owner, title: $name}) { issue { id } } }"

// ghVisibilityRead lets through the guard's own REST and GraphQL visibility reads, and
// sharedCheckVisibilityRead the shared check's visibility callback, each for exactly the
// repository PUBLIC_LEAK_CHECK_VISIBILITY_READ names, and nothing else: neither takes the other's
// read, and neither takes the GraphQL read of another repository, another query, a mutation, the
// read with a field added, the read with no variable, or a variable naming no repository.
func TestGhVisibilityReadLetsThroughOnlyTheExactReads(t *testing.T) {
	const slug = "sjawhar/legion"
	env := map[string]string{leakguard.VisibilityReadVar: slug}
	graphql := leakguard.VisibilityGraphQLArgs(slug)
	for _, tc := range []struct {
		name string
		argv []string
		env  map[string]string
		// guards is whether ghVisibilityRead takes argv, checks whether sharedCheckVisibilityRead does.
		guards, checks bool
	}{
		{"the shared check's callback", leakguard.VisibilityReadArgs(slug), env, false, true},
		{"the guard's REST read", leakguard.VisibilityReadWithHeadersArgs(slug), env, true, false},
		{"the guard's GraphQL read", graphql, env, true, false},
		{"the callback with no variable", leakguard.VisibilityReadArgs(slug), map[string]string{}, false, false},
		{"the callback of another repository", leakguard.VisibilityReadArgs("sjawhar/other"), env, false, false},
		{
			"the callback under a variable naming no repository",
			leakguard.VisibilityReadArgs("sjawhar/legion/pulls"),
			map[string]string{leakguard.VisibilityReadVar: "sjawhar/legion/pulls"},
			false, false,
		},
		{"the GraphQL read of another repository", leakguard.VisibilityGraphQLArgs("sjawhar/other"), env, false, false},
		{"another GraphQL query", graphQLOf("{viewer{login}}"), env, false, false},
		{"a GraphQL mutation", graphQLOf(inventedMutation), env, false, false},
		{"the GraphQL read with a field added", append(slices.Clone(graphql), "-f", "extra=1"), env, false, false},
		{"the GraphQL read with no variable", graphql, map[string]string{}, false, false},
		{
			"the GraphQL read under a variable naming no repository",
			leakguard.VisibilityGraphQLArgs("sjawhar/legion/pulls"),
			map[string]string{leakguard.VisibilityReadVar: "sjawhar/legion/pulls"},
			false, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, read := range []struct {
				name string
				of   func([]string, map[string]string) (ghrepo.Repository, bool)
				want bool
			}{{"ghVisibilityRead", ghVisibilityRead, tc.guards}, {"sharedCheckVisibilityRead", sharedCheckVisibilityRead, tc.checks}} {
				repo, got := read.of(tc.argv, tc.env)
				if got != read.want {
					t.Errorf("%s(%q) = %v, want %v", read.name, tc.argv, got, read.want)
				}
				if got && repo.String() != slug {
					t.Errorf("%s(%q) read the repository %q, want %s", read.name, tc.argv, repo, slug)
				}
			}
		})
	}
}

// The guard's own visibility reads run with the token of the directory the repository they read
// ROUTES to, the REST read and the GraphQL read alike. The REST read names its repository in its
// endpoint; the GraphQL read names it only in variables, which the resolver reads as an owner
// alone, so its directory comes from the repository PUBLIC_LEAK_CHECK_VISIBILITY_READ names,
// which ghVisibilityRead has matched to the read's exact argv.
//
// A bundle holds one directory per owner and none per repository since INFRA-2543, so what tells
// the two halves apart is a configured rename rather than a repository directory: with the pair
// sjawhar/legion -> cybermonklabs/legion and the App installed on both owners, both reads of
// sjawhar/legion take cybermonklabs', the owner whose installation holds the repository GitHub
// serves under that name, and both reads of sjawhar/legion-smoke, which the pair does not move,
// take sjawhar's. Were the GraphQL read resolved by its owner alone, the sjawhar/legion half
// would take sjawhar's and this row would fail.
//
// Under the caller's own GH_TOKEN or GITHUB_TOKEN both reads run with that token, which gh sends
// in place of any directory's: they keep the GH_CONFIG_DIR they were given and are routed by
// nothing, for octo/elsewhere, whose owner the bundle holds no directory for, as much as for a
// repository it does hold one for (#4219).
func TestLegionGhRunsTheGuardsVisibilityReadsWithTheRepositorysOrTheCallersToken(t *testing.T) {
	root := pairRoutedBundle(t)
	defaultDir := ghconfig.Dir(root, "cybermonklabs")
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	// The stub records the GH_CONFIG_DIR, the GH_TOKEN, the GITHUB_TOKEN and the argv of every
	// run, then answers as the package's stand-in does: a help request with the saved help, a
	// visibility read with public.
	stub := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"$GH_CONFIG_DIR\" \"$GH_TOKEN\" \"$GITHUB_TOKEN\" \"$*\" >>\"${GH_STANDIN_RECORD:-/dev/null}\"\n" +
		strings.TrimPrefix(ghStandInPrefix(help), "#!/bin/sh\n") + "exit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	wrappers := ghWrapperDir(t)
	for _, tc := range []struct {
		slug, token, dir string
	}{
		{"sjawhar/legion", "", ghconfig.Dir(root, "cybermonklabs")},
		{"sjawhar/legion-smoke", "", ghconfig.Dir(root, "sjawhar")},
		{"sjawhar/legion", "GH_TOKEN", defaultDir},
		{"octo/elsewhere", "GITHUB_TOKEN", defaultDir},
	} {
		for _, read := range []struct {
			name string
			args []string
		}{
			{"REST", leakguard.VisibilityReadWithHeadersArgs(tc.slug)},
			{"GraphQL", leakguard.VisibilityGraphQLArgs(tc.slug)},
		} {
			t.Run(read.name+" "+tc.slug+" "+tc.token, func(t *testing.T) {
				record := filepath.Join(t.TempDir(), "gh")
				env := map[string]string{
					"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
					"PATH": wrappers + ":" + os.Getenv("PATH"), "GH_STANDIN_RECORD": record,
					leakguard.VisibilityReadVar: tc.slug,
				}
				tokens := map[string]string{"GH_TOKEN": "", "GITHUB_TOKEN": ""}
				if tc.token != "" {
					env[tc.token], tokens[tc.token] = "ghs_caller", "ghs_caller"
				}
				code, stdout, stderr := legionGh(t, env, read.args...)
				if code != 0 || stdout != "public\n" {
					t.Fatalf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and gh's answer", read.args, code, stdout, stderr)
				}
				if want := tc.dir + "|" + tokens["GH_TOKEN"] + "|" + tokens["GITHUB_TOKEN"] + "|" + strings.Join(read.args, " ") + "\n"; !strings.Contains(readRecord(t, record), want) {
					t.Errorf("gh recorded\n%s\nwant the read run with %s:\n%s", readRecord(t, record), tc.dir, want)
				}
			})
		}
	}
}

// The other half of the octo/elsewhere row above: a visibility read runs under the caller's token
// for an owner the bundle holds no directory for, and the SAME read with no caller token is
// refused, exit 2, naming that owner. That is what makes the caller-token arm worth having rather
// than a widening of what any read may reach: without a token of its own the read is still routed
// through the bundle (ghConfigDirFor), which holds nothing for octo, and #4219's second reason —
// that a read with the bundle's directory would be refused for a repository the caller's token
// writes — is this refusal.
//
// One gate holds it: the routing in the ghVisibilityCall arm, which refuses the unheld owner from
// the read's own repository. ghTargetOf never reads these reads, which read no gh help
// (TestLegionGhReadsNoHelpForTheGuardsVisibilityReads).
func TestLegionGhRefusesTheGuardsVisibilityReadForAnUnheldOwnerWithoutACallerToken(t *testing.T) {
	root := pairRoutedBundle(t)
	defaultDir := ghconfig.Dir(root, "cybermonklabs")
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)+"exit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	wrappers := ghWrapperDir(t)
	const slug = "octo/elsewhere"
	for _, read := range []struct {
		name string
		args []string
	}{
		{"REST", leakguard.VisibilityReadWithHeadersArgs(slug)},
		{"GraphQL", leakguard.VisibilityGraphQLArgs(slug)},
	} {
		t.Run(read.name, func(t *testing.T) {
			env := map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"PATH": wrappers + ":" + os.Getenv("PATH"), leakguard.VisibilityReadVar: slug,
			}
			code, stdout, stderr := legionGh(t, env, read.args...)
			const refusal = `the role's GitHub App is not installed on "octo"`
			if code != 2 || stdout != "" || !strings.Contains(stderr, refusal) {
				t.Errorf("legion gh %q with no caller token = exit %d, stdout %q, stderr %q; want exit 2 and %s", read.args, code, stdout, stderr, refusal)
			}
		})
	}
}

// `legion gh` answers the shared check's own visibility read itself, by the guard's rule, and
// prints what `gh api ... --jq .visibility` prints: the word and a newline, exit 0; or, where no
// read answers, nothing on standard output and exit 1, which the check reads as an unknown
// visibility and refuses. The guard's reads come back through `legion gh` to the stand-in gh: the
// REST read, then, with the installation's REST limit spent, the GraphQL read, then a stand-in
// git's ls-remote, which can answer only public. Neither stream carries the shared check's own
// refusal, which only the check prints.
//
// The read is fresh: the check reads another remote of a push with it, and counts the commits of
// one it reads public as held, which it does not scan, so a public the guard kept for a
// repository made private since must not answer it. A kept public, which the guard's own
// Visibility does answer with, is passed over for the read, and no answer is kept. For a
// repository of an owner the bundle holds no directory for, `legion gh` refuses the guard's REST
// read before any gh runs, and ls-remote alone answers: public, or nothing.
func TestLegionGhAnswersTheSharedChecksVisibilityRead(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	wrappers := ghWrapperDir(t)
	const slug = "sjawhar/legion"
	rest := func(visibility string) string {
		return "printf '%s\\r\\n' 'HTTP/2.0 200 OK' 'X-Ratelimit-Remaining: 4999' ''\nprintf '%s\\n' " + visibility + "\nexit 0\n"
	}
	restSpent := fmt.Sprintf("printf '%%s\\r\\n' 'HTTP/2.0 403 Forbidden' 'X-Ratelimit-Remaining: 0' 'X-Ratelimit-Reset: %d' ''\n", time.Now().Add(30*time.Minute).Unix()) +
		`printf '%s\n' '{"message":"API rate limit exceeded for installation ID 1."}'` + "\n" +
		"echo 'gh: API rate limit exceeded for installation ID 1. (HTTP 403)' >&2\nexit 1\n"
	restFails := "printf '%s\\r\\n' 'HTTP/2.0 500 Internal Server Error' ''\nprintf '%s\\n' '{\"message\":\"invented\"}'\nexit 1\n"
	graphQL := func(body string, exit int) string {
		return "echo graphql >> \"$GH_STANDIN_RECORD\"\n" +
			"printf '%s\\r\\n' 'HTTP/2.0 200 OK' 'X-Ratelimit-Remaining: 12221' ''\n" +
			"printf '%s' '" + body + "'\n" + fmt.Sprintf("exit %d\n", exit)
	}
	answers := func(visibility string) string {
		return graphQL(`{"data":{"repository":{"nameWithOwner":"`+slug+`","visibility":"`+visibility+`"}}}`, 0)
	}
	graphQLLimited := graphQL(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit already exceeded"}]}`, 1)
	graphQLNotFound := graphQL(`{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository"}]}`, 1)
	lists := "printf '%s\\tHEAD\\n' " + strings.Repeat("1", 40) + "\nexit 0\n"
	refused := "echo 'fatal: could not read Username for '\\''https://github.com'\\'': terminal prompts disabled' >&2\nexit 128\n"
	// keptFiles is each file of the guard's kept visibility answers under state, with its content.
	keptFiles := func(t *testing.T, state string) map[string]string {
		t.Helper()
		dir := filepath.Join(state, "legion-gh", "visibility")
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}
		}
		if err != nil {
			t.Fatal(err)
		}
		files := make(map[string]string, len(entries))
		for _, entry := range entries {
			content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files[entry.Name()] = string(content)
		}
		return files
	}
	// keepPublic plants the guard's kept public answer for slug under state, as Guard.Visibility
	// keeps one, and fails the test unless the guard's own Visibility then answers public from it
	// without a read.
	keepPublic := func(t *testing.T, state string) {
		t.Helper()
		dir := filepath.Join(state, "legion-gh", "visibility")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		entry := filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(slug))))
		if err := os.WriteFile(entry, []byte(`{"visibility":"public"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		visibility, err := leakguard.New(leakguard.Options{
			Env: []string{"XDG_STATE_HOME=" + state},
			Visibility: func(context.Context, string) (leakguard.Visibility, error) {
				return "", errors.New("the kept answer was not read")
			},
		}).Visibility(context.Background(), slug)
		if err != nil || visibility != leakguard.Public {
			t.Fatalf("the guard's own Visibility answered %q, %v from the kept entry; want the kept public", visibility, err)
		}
	}
	for _, tc := range []struct {
		name string
		// slug is the repository the check reads, sjawhar/legion where it is empty.
		slug          string
		rest, graphql string
		git           string
		// kept is whether the guard holds a kept public answer for the repository when the read
		// is made.
		kept bool
		// want is what legion gh prints, empty where it answers nothing and exits 1; reads is
		// the reads the stand-ins recorded, in order.
		want  string
		reads string
	}{
		{name: "REST answers public", rest: rest("public"), want: "public\n", reads: "rest\n"},
		{name: "REST answers private", rest: rest("private"), want: "private\n", reads: "rest\n"},
		{name: "a kept public, and REST answers private", kept: true, rest: rest("private"), want: "private\n", reads: "rest\n"},
		{
			name: "a kept public, and REST is limited and GraphQL answers private", kept: true,
			rest: restSpent, graphql: answers("PRIVATE"), want: "private\n", reads: "rest\ngraphql\n",
		},
		{name: "REST is limited and GraphQL answers public", rest: restSpent, graphql: answers("PUBLIC"), want: "public\n", reads: "rest\ngraphql\n"},
		{name: "REST is limited and GraphQL answers private", rest: restSpent, graphql: answers("PRIVATE"), want: "private\n", reads: "rest\ngraphql\n"},
		{name: "REST is limited and GraphQL answers internal", rest: restSpent, graphql: answers("INTERNAL"), want: "internal\n", reads: "rest\ngraphql\n"},
		{name: "REST and GraphQL are limited and ls-remote answers", rest: restSpent, graphql: graphQLLimited, git: lists, want: "public\n", reads: "rest\ngraphql\ngit\n"},
		{name: "REST and GraphQL are limited and ls-remote is refused", rest: restSpent, graphql: graphQLLimited, git: refused, reads: "rest\ngraphql\ngit\n"},
		{name: "REST is limited and GraphQL could not resolve the repository", rest: restSpent, graphql: graphQLNotFound, git: lists, reads: "rest\ngraphql\n"},
		{name: "REST fails another way", rest: restFails, graphql: answers("PRIVATE"), git: lists, reads: "rest\n"},
		{name: "an unserved owner's repository ls-remote lists", slug: "other-owner/invented-repo", rest: rest("private"), git: lists, want: "public\n", reads: "git\n"},
		{name: "an unserved owner's repository GitHub asks ls-remote a credential for", slug: "other-owner/invented-repo", rest: rest("private"), git: refused, reads: "git\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := cmp.Or(tc.slug, slug)
			bin, record, state := t.TempDir(), filepath.Join(t.TempDir(), "reads"), t.TempDir()
			if tc.kept {
				keepPublic(t, state)
			}
			kept := keptFiles(t, state)
			stub := filepath.Join(bin, "real-gh")
			script := "#!/bin/sh\nfor last; do :; done\n" +
				"if [ \"$last\" = --help ]; then\n" +
				"  path=$(printf '%s ' \"$@\" | sed -e 's/--help $//' -e 's/ $//' -e 's/ /_/g')\n" +
				"  exec cat '" + help + "'/\"${path:-gh}\".txt\nfi\n" +
				"if [ \"$4\" = graphql ]; then\n  :\n" + tc.graphql + "fi\n" +
				"echo rest >> \"$GH_STANDIN_RECORD\"\n" + tc.rest
			if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho git >> \"$GH_STANDIN_RECORD\"\n"+tc.git), 0o700); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := legionGh(t, map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"PATH": wrappers + ":" + bin + ":" + os.Getenv("PATH"), "GH_STANDIN_RECORD": record,
				"XDG_STATE_HOME": state, leakguard.VisibilityReadVar: repo,
			}, leakguard.VisibilityReadArgs(repo)...)
			switch {
			case tc.want != "" && (code != 0 || stdout != tc.want):
				t.Errorf("legion gh = exit %d, stdout %q, stderr %q; want exit 0 and %q", code, stdout, stderr, tc.want)
			case tc.want == "" && (code != 1 || stdout != ""):
				t.Errorf("legion gh = exit %d, stdout %q, stderr %q; want exit 1 and nothing on standard output", code, stdout, stderr)
			}
			if got := readRecord(t, record); got != tc.reads {
				t.Errorf("the stand-ins recorded the reads %q, want %q", got, tc.reads)
			}
			if got := keptFiles(t, state); !maps.Equal(got, kept) {
				t.Errorf("the guard's kept answers are %q after the read, want %q: the read keeps none", got, kept)
			}
			for _, refusal := range []string{"cannot read the visibility", "nothing is pushed"} {
				if strings.Contains(stdout+stderr, refusal) {
					t.Errorf("legion gh printed the shared check's own refusal %q: stdout %q, stderr %q", refusal, stdout, stderr)
				}
			}
		})
	}
}

// Two wrapped calls to one repository reuse its public visibility answer, and their audit entries
// identify the call without preserving the text that the command would send.
func TestLegionGhKeepsVisibilityAndLogsCalls(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	record := filepath.Join(t.TempDir(), "gh")
	stub := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)+"exit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"GH_CONFIG_DIR":        defaultDir,
		"GH_STANDIN_RECORD":    record,
		"LEGION_GH_OWNERS_DIR": root,
		"LEGION_GH_PATH":       stub,
		"LEGION_ISSUE":         "issue-9",
		"LEGION_ROLE":          "tester",
		"PUBLIC_LEAK_CHECK":    leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
		"XDG_STATE_HOME":       state,
	}
	args := []string{"pr", "create", "-R", "acme/widgets", "--title", "argument text must not be logged"}
	for range 2 {
		if code, _, stderr := legionGhIn(t, t.TempDir(), env, "", args...); code != 0 {
			t.Fatalf("legion gh %q = exit %d, stderr %q; want success", args, code, stderr)
		}
	}
	if got := strings.Count(readRecord(t, record), "visibility "); got != 1 {
		t.Errorf("the visibility read reached gh %d times, want once", got)
	}
	log, err := os.ReadFile(filepath.Join(state, "legion-gh", "calls.log"))
	if err != nil {
		t.Fatalf("read the call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 2 {
		t.Errorf("the call log has %d lines, want two: %q", len(lines), log)
	}
	for i, line := range lines {
		wants := []string{"tester", "issue-9", "acme/widgets", "pr create", "status=0"}
		if i == 0 {
			wants = append(wants, `visibility="github"`)
		} else {
			wants = append(wants, `visibility="kept"`)
		}
		for _, want := range wants {
			if !strings.Contains(line, want) {
				t.Errorf("the call log line %q does not name %q", line, want)
			}
		}
	}
	if strings.Contains(string(log), "argument text must not be logged") {
		t.Errorf("the call log holds command argument text: %q", log)
	}
}

// Concurrent processes share one log and rotate it as one operation, so no appended line that
// fits after the oversized prior content is lost.
func TestAppendGhCallLogKeepsConcurrentLinesThatFit(t *testing.T) {
	if repository := os.Getenv("LEGION_TEST_GH_CALL_LOG_REPOSITORY"); repository != "" {
		appendGhCallLog(map[string]string{"XDG_STATE_HOME": os.Getenv("LEGION_TEST_GH_CALL_LOG_STATE")}, repository, "api", "", 0)
		return
	}

	const calls = 128
	state := t.TempDir()
	dir := filepath.Join(state, "legion-gh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "calls.log")
	if err := os.WriteFile(path, append(bytes.Repeat([]byte{'x'}, ghCallLogBytes), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, calls)
	for i := range calls {
		repository := fmt.Sprintf("repo-%03d", i)
		go func() {
			command := exec.Command(os.Args[0], "-test.run=^TestAppendGhCallLogKeepsConcurrentLinesThatFit$")
			command.Env = append(os.Environ(),
				"LEGION_TEST_GH_CALL_LOG_REPOSITORY="+repository,
				"LEGION_TEST_GH_CALL_LOG_STATE="+state,
			)
			errs <- command.Run()
		}()
	}
	for range calls {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range calls {
		want := fmt.Sprintf(`repository="repo-%03d"`, i)
		if got := strings.Count(string(content), want); got != 1 {
			t.Errorf("the call log records %q %d times, want once", want, got)
		}
	}
}

// The rate-limit endpoint is a read with no repository content, so it bypasses the guard: no
// check runs and no visibility is read. A GET of another endpoint writes nothing, so it runs the
// check, and a check that refuses it refuses it, but it reads no visibility: where the visibility
// read fails, as it does in an hour whose REST limit is spent, the GET still reaches gh. A write
// to the same repository still reads its visibility, and is refused when that read fails.
func TestLegionGhLetsOnlyTheRateLimitReadBypassTheGuard(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	repository := t.TempDir()
	fixture := fixturegit.Isolated(t, t.TempDir())
	for _, args := range [][]string{
		fixturegit.InitWorkTree("", "")[1:],
		{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
	} {
		fixture.Run(repository, append([]string{"git"}, args...)...)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	script := strings.Replace(ghStandInPrefix(help), "  echo \"${GH_STANDIN_VISIBILITY:-public}\"\n  exit 0;;\n", "  echo unavailable >&2\n  exit 1;;\n", 1) + "exit 0\n"
	if !strings.Contains(script, "echo unavailable") {
		t.Fatal("the stand-in's visibility answer was not replaced, so this test would read a visibility")
	}
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	root, defaultDir := gitCredentialBundle(t)
	for _, tc := range []struct {
		name      string
		args      []string
		check     string
		wantCode  int
		wantReads int
	}{
		{"the GET, under a check that would refuse it", []string{"api", "rate_limit"}, ghCheckRefuses, 0, 0},
		{"a POST", []string{"api", "rate_limit", "--method", "POST"}, ghCheckRefuses, 2, 0},
		{"another endpoint's GET under a check that refuses it", []string{"api", "user"}, ghCheckRefuses, 2, 0},
		{"another endpoint's GET while the visibility read fails", []string{"api", "repos/acme/widgets/pulls"}, ghCheckPasses, 0, 0},
		{"another endpoint's write while the visibility read fails", []string{"api", "repos/acme/widgets/issues", "--method", "POST"}, ghCheckPasses, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "gh")
			env := map[string]string{
				"GH_CONFIG_DIR":        defaultDir,
				"GH_STANDIN_RECORD":    record,
				"LEGION_GH_OWNERS_DIR": root,
				"LEGION_GH_PATH":       stub,
				"PUBLIC_LEAK_CHECK":    ghCheckAt(t, tc.check),
				testProvenCheckEnv:     ghProvenProgramSHA256,
			}
			if code, _, stderr := legionGhIn(t, repository, env, "", tc.args...); code != tc.wantCode {
				t.Fatalf("legion gh %q = exit %d, stderr %q; want exit %d", tc.args, code, stderr, tc.wantCode)
			}
			if reads := strings.Count(readRecord(t, record), "visibility "); reads != tc.wantReads {
				t.Errorf("the visibility read reached gh %d times, want %d", reads, tc.wantReads)
			}
		})
	}
}

// A SIGTERM or a SIGHUP sent to `legion gh`'s own pid reaches the gh it is waiting for, which is
// what an exec'd gh — the shape this replaced — ended on. Neither signal reaches gh by itself:
// the child is given no process group of its own on purpose, so a terminal's SIGINT reaches it
// with the rest of the group, but a signal sent to one pid reaches that pid alone. Without the
// forwarding a SIGTERM would end at main's own signal.NotifyContext, whose cancellation the
// child's context has taken off, and gh would run on with nobody waiting for it, while a SIGHUP
// would kill `legion gh` where it stood and leave gh running.
//
// The stand-in gh records the signal it caught and exits with a status of its own, so what this
// reads is both halves: gh ended on the signal, and its status came back through `legion gh`.
func TestLegionGhForwardsATerminatingSignalToGh(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
		code   int
	}{
		{"SIGTERM", syscall.SIGTERM, 42},
		{"SIGHUP", syscall.SIGHUP, 43},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := t.TempDir()
			ghRecord, running := filepath.Join(records, "gh"), filepath.Join(records, "running")
			// The stand-in traps both signals, says it is running once the traps are set, and
			// then waits in ticks short enough that a trap runs within one of them: a shell runs
			// no trap while a foreground command of its own is still going. The wait gives up
			// after thirty seconds and exits 9, so a forwarding that never arrives is a failure
			// naming it rather than a test binary that hangs until its deadline.
			stub := filepath.Join(t.TempDir(), "gh")
			script := ghStandInPrefix(help) +
				"trap 'printf \"gh caught TERM\\n\" >>\"$GH_STANDIN_RECORD\"; exit 42' TERM\n" +
				"trap 'printf \"gh caught HUP\\n\" >>\"$GH_STANDIN_RECORD\"; exit 43' HUP\n" +
				": >\"$GH_STANDIN_RUNNING\"\n" +
				"ticks=0\nwhile [ \"$ticks\" -lt 600 ]; do sleep 0.05; ticks=$((ticks+1)); done\n" +
				"printf 'gh caught nothing\\n' >>\"$GH_STANDIN_RECORD\"\nexit 9\n"
			if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			command := legionGhCommand(t, t.TempDir(), map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
				"GH_STANDIN_RECORD": ghRecord, "GH_STANDIN_RUNNING": running,
			}, "pr", "list")
			// Standard output and standard error are a file of the test's own rather than a
			// pipe: gh's own `sleep` outlives the shell that trapped the signal by a tick, and
			// a pipe would hold Wait until that grandchild let go of it.
			printed, err := os.Create(filepath.Join(records, "printed"))
			if err != nil {
				t.Fatal(err)
			}
			defer printed.Close()
			command.Stdout, command.Stderr = printed, printed
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			// The signal is sent once gh itself is running: before that it would reach `legion
			// gh` while the guard still has the call, which is no forwarding this test can read.
			testwait.Eventually(t, "the stand-in gh to be running", func() bool {
				_, err := os.Stat(running)
				return err == nil
			})
			// The signal goes to this one pid, never to the group: gh shares this test binary's
			// own group, and a group signal would end the test run with it.
			if err := command.Process.Signal(tc.signal); err != nil {
				t.Fatal(err)
			}
			waited := command.Wait()
			exit, ok := waited.(*exec.ExitError)
			if !ok {
				t.Fatalf("legion gh under %s = %v, want gh's own status: %s", tc.name, waited, readRecord(t, printed.Name()))
			}
			if exit.ExitCode() != tc.code {
				t.Errorf("legion gh under %s exited %d, want gh's own %d: %s", tc.name, exit.ExitCode(), tc.code, readRecord(t, printed.Name()))
			}
			if want := "gh caught " + strings.TrimPrefix(tc.name, "SIG") + "\n"; !strings.Contains(readRecord(t, ghRecord), want) {
				t.Errorf("gh recorded %q, want %q: the signal never reached it", readRecord(t, ghRecord), want)
			}
		})
	}
}

// A `legion gh` started with SIGHUP ignored leaves it ignored for gh. That is how `nohup gh …`
// runs: nohup ignores the signal and execs the wrapper, which execs this command, and SIG_IGN
// is kept across both. Arming the forwarding for it would install a handler — the signal "will
// no longer be ignored", as os/signal's doc puts it — and execve resets a caught signal to its
// default action, so gh would start with the hangup set to kill and die of the first one, which
// is the whole of what nohup was asked to prevent.
//
// The run is under a shell that ignores the signal, as nohup does, and the stand-in gh sets a
// trap of its own: a shell keeps ignoring a signal that was ignored when it started, trap
// command or not, so a gh that records catching the hangup is a gh whose hangup the arming
// un-ignored. Nothing here is forwarded, and what the test reads is that `legion gh` sat
// through the hangup and came back with the status of a gh that ended on its own.
func TestLegionGhLeavesAnIgnoredHangupIgnoredForGh(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	records := t.TempDir()
	ghRecord, running := filepath.Join(records, "gh"), filepath.Join(records, "running")
	release := filepath.Join(records, "release")
	// The stand-in traps the hangup, says it is running once the trap is set, and then waits
	// for the test's release in ticks short enough that a trap runs within one of them: a shell
	// runs no trap while a foreground command of its own is still going. The wait gives up
	// after ten seconds, so a release that never comes is a failure naming it rather than a
	// test binary that hangs until its deadline.
	stub := filepath.Join(t.TempDir(), "gh")
	script := ghStandInPrefix(help) +
		"trap 'printf \"gh caught the hangup\\n\" >>\"$GH_STANDIN_RECORD\"; exit 44' HUP\n" +
		": >\"$GH_STANDIN_RUNNING\"\n" +
		"ticks=0\nwhile [ ! -f \"$GH_STANDIN_RELEASE\" ] && [ \"$ticks\" -lt 200 ]; do sleep 0.05; ticks=$((ticks+1)); done\n" +
		"printf 'gh survived the hangup\\n' >>\"$GH_STANDIN_RECORD\"\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	command := legionGhCommand(t, t.TempDir(), map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
		"GH_STANDIN_RECORD": ghRecord, "GH_STANDIN_RUNNING": running, "GH_STANDIN_RELEASE": release,
	}, "pr", "list")
	// `sh -c 'trap "" HUP; exec …'` is what nohup does to the command it runs, and the exec is
	// the wrapper's: `legion gh` starts here with the hangup ignored and no handler of its own,
	// exactly as it does under `nohup gh …` on a pane's PATH.
	command.Args = append([]string{"/bin/sh", "-c", `trap "" HUP; exec "$@"`, "sh"}, command.Args...)
	command.Path = "/bin/sh"
	// Standard output and standard error are a file of the test's own rather than a pipe, for
	// the reason the forwarding test above gives: gh's own `sleep` outlives it by a tick.
	printed, err := os.Create(filepath.Join(records, "printed"))
	if err != nil {
		t.Fatal(err)
	}
	defer printed.Close()
	command.Stdout, command.Stderr = printed, printed
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	testwait.Eventually(t, "the stand-in gh to be running", func() bool {
		_, err := os.Stat(running)
		return err == nil
	})
	// The hangup goes to this one pid, never to the group: gh shares this test binary's own
	// group, and a group signal would end the test run with it.
	if err := command.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	// The run is given a second to end on that hangup before gh is released. Where the hangup
	// is forwarded, gh catches it and ends the run inside that second, and the failure says so;
	// where it stays ignored, nothing happens in it, which is what the rest of this reads.
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case waited := <-done:
		t.Fatalf("legion gh ended on a hangup it was started ignoring: %v; gh recorded %q, printed %q",
			waited, readRecord(t, ghRecord), readRecord(t, printed.Name()))
	case <-time.After(time.Second):
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if waited := <-done; waited != nil {
		t.Fatalf("legion gh = %v, want gh's own 0: %s", waited, readRecord(t, printed.Name()))
	}
	if want := "gh survived the hangup\n"; readRecord(t, ghRecord) != want {
		t.Errorf("gh recorded %q, want %q", readRecord(t, ghRecord), want)
	}
}

// A signal that reaches `legion gh` before it has armed its own forwarding ends the command
// there, with the status a shell reads a death by that signal as, and gh is never started.
// Until that arming the signal's reader is main's own signal.NotifyContext for a SIGINT or a
// SIGTERM, and hangupCancels for a SIGHUP: each cancels the command's context with a cause naming
// the signal and raises it no further, and the context the child is given has that cancellation
// taken off. Without the read of that cause gh would start and run to the end as if nothing had
// been sent, and `legion gh` would answer with gh's own status.
//
// The window is everything this command does before the child, and here it is held open in the
// guard's read of the repository's visibility, which this command makes through PATH's gh before
// it answers: the `gh` first on PATH is a pane's wrapper whose legion is a script that records
// that it is holding and waits for GH_STANDIN_RELEASE, up to ten seconds, and a tick more once
// that file is there, before it runs `legion gh`. That read reads no gh help of its own
// (TestLegionGhReadsNoHelpForTheGuardsVisibilityReads), so the hold is in the wrapper rather than
// in a help the stand-in answers. The signal lands inside the hold rather than in a race. The
// call is the shared check's own visibility read, the one call the guard never sees, so the
// whole of the held work is this command's own: the check of these runs refuses everything, and
// a run that reached it would exit 2 rather than on the signal.
//
// Each `legion gh` here is this test binary run as `legion`, through main and its cobra command
// line (run, passThrough), so the context runGh is handed is the one main's signal.NotifyContext
// cancels, which endedBySignal reads the cause of. A command line that handed runGh another
// context fails the SIGINT and the SIGTERM rows: they answer the visibility, exit 0, with gh run
// for the read.
func TestLegionGhStartsNoGhForASignalThatCameBeforeTheForwarding(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	visibility := leakguard.VisibilityReadArgs("sjawhar/legion")
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
		code   int
	}{
		{"SIGINT", syscall.SIGINT, 130},
		{"SIGTERM", syscall.SIGTERM, 143},
		{"SIGHUP", syscall.SIGHUP, 129},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := t.TempDir()
			ghRecord, release := filepath.Join(records, "gh"), filepath.Join(records, "release")
			// The stand-in is the prefix alone: it records the visibility read it would answer
			// if it were ever started as gh.
			stub := filepath.Join(t.TempDir(), "gh")
			if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)), 0o700); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			hold := filepath.Join(bin, "legion")
			holding := "#!/bin/sh\nprintf 'legion gh held\\n' >>\"$GH_STANDIN_RECORD\"\nticks=0\n" +
				"while [ ! -f \"$GH_STANDIN_RELEASE\" ] && [ \"$ticks\" -lt 200 ]; do sleep 0.05; ticks=$((ticks+1)); done\n" +
				"sleep 0.05\nexec '" + os.Args[0] + "' \"$@\"\n"
			if err := os.WriteFile(hold, []byte(holding), 0o700); err != nil {
				t.Fatal(err)
			}
			wrappers := t.TempDir()
			if err := os.WriteFile(filepath.Join(wrappers, "gh"), []byte("#!/bin/sh\nexec '"+hold+"' gh \"$@\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			command := legionGhCommand(t, t.TempDir(), map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"PATH":                      wrappers + ":" + os.Getenv("PATH"),
				"PUBLIC_LEAK_CHECK":         leakguardtest.Write(t, t.TempDir(), ghCheckRefuses),
				leakguard.VisibilityReadVar: "sjawhar/legion",
				"GH_STANDIN_RECORD":         ghRecord, "GH_STANDIN_RELEASE": release,
			}, visibility...)
			printed, err := os.Create(filepath.Join(records, "printed"))
			if err != nil {
				t.Fatal(err)
			}
			defer printed.Close()
			command.Stdout, command.Stderr = printed, printed
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			testwait.Eventually(t, "the guard's visibility read to be holding", func() bool {
				return strings.Contains(readRecord(t, ghRecord), "legion gh held\n")
			})
			// The signal goes to this one pid, as the forwarding test's does, and the hold is
			// released after it: the hold waits a tick of its own once the file is there, so
			// the signal is taken while the run is still inside the window.
			if err := command.Process.Signal(tc.signal); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			waited := command.Wait()
			exit, ok := waited.(*exec.ExitError)
			if !ok {
				t.Fatalf("legion gh under %s = %v, want 128+%d: %s", tc.name, waited, tc.signal, readRecord(t, printed.Name()))
			}
			if exit.ExitCode() != tc.code {
				t.Errorf("legion gh under %s exited %d, want %d: %s", tc.name, exit.ExitCode(), tc.code, readRecord(t, printed.Name()))
			}
			if want := "legion gh held\n"; readRecord(t, ghRecord) != want {
				t.Errorf("gh recorded %q, want %q: gh was started for a signal that came before it", readRecord(t, ghRecord), want)
			}
		})
	}
}

// The files a call would send to GitHub as content are read out of the same parse that reads the
// owner, so the leak guard reads each file's text (leakguard.GhRequest.Files) and needs no second
// reader of the argv. The parse knows each option's arity from the leaf's own help, which is what
// a table of option names cannot: `-F` is --body-file in `gh pr create` and --field in `gh api`,
// a short cluster gives its last letter the next argument (`-dF <path>` is `--draft --body-file
// <path>`), and an attached value may carry an `=` of its own (`-F=<path>`). A value of `-`, an
// `@-`, a field given a literal and a raw field name no file to open.
func TestGhTargetOfReadsTheFilesTheCallWouldSend(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want []leakguard.GhFile
	}{
		{
			name: "a short cluster whose last letter is the option",
			argv: []string{"pr", "create", "-R", "acme/widgets", "-dF", "body.md"},
			want: []leakguard.GhFile{{Option: "--body-file", Path: "body.md"}},
		},
		{
			name: "a short option with its value attached by an equals sign",
			argv: []string{"pr", "create", "-R", "acme/widgets", "-F=body.md"},
			want: []leakguard.GhFile{{Option: "--body-file", Path: "body.md"}},
		},
		{
			name: "a separate --body-file",
			argv: []string{"pr", "comment", "1", "-R", "acme/widgets", "--body-file", "body.md"},
			want: []leakguard.GhFile{{Option: "--body-file", Path: "body.md"}},
		},
		{
			name: "gh api's field read from a file",
			argv: []string{"api", "repos/acme/widgets/pulls/1", "--method", "PATCH", "-F", "body=@body.md"},
			want: []leakguard.GhFile{{Option: "--field", Path: "body.md"}},
		},
		{
			name: "gh api's request body",
			argv: []string{"api", "repos/acme/widgets/pulls", "--input", "request.json"},
			want: []leakguard.GhFile{{Option: "--input", Path: "request.json"}},
		},
		{
			name: "a raw field, which sends its value literally",
			argv: []string{"api", "repos/acme/widgets/pulls/1", "-f", "body=@body.md"},
			want: []leakguard.GhFile{},
		},
		{
			name: "a field given a literal value",
			argv: []string{"api", "repos/acme/widgets/pulls/1", "-F", "body=text"},
			want: []leakguard.GhFile{},
		},
		{
			name: "a body option whose value is standard input",
			argv: []string{"pr", "comment", "1", "-R", "acme/widgets", "--body-file", "-"},
			want: []leakguard.GhFile{},
		},
		{
			name: "a field read from standard input",
			argv: []string{"api", "repos/acme/widgets/pulls/1", "-F", "body=@-"},
			want: []leakguard.GhFile{},
		},
		{
			name: "the same file named twice",
			argv: []string{"api", "repos/acme/widgets/pulls/1", "-F", "body=@body.md", "--input", "body.md"},
			want: []leakguard.GhFile{{Option: "--field", Path: "body.md"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := ghTargetOfKept(t, tc.argv, nil, savedGhHelp, noGitRepository, ownersOwnDirectories)
			if err != nil {
				t.Fatalf("ghTargetOf(%q) = %v", tc.argv, err)
			}
			if !slices.Equal(target.files, tc.want) {
				t.Fatalf("ghTargetOf(%q) read the files %v, want %v", tc.argv, target.files, tc.want)
			}
		})
	}
}

// gh 2.104.0's --attach uploads the file it names to GitHub, an SVG's text among what it takes,
// and the leak guard reads no such file, so `legion gh` refuses a call giving one, naming the
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
			_, err := ghTargetOf(argv, nil, latest, noGitRepository, ownersOwnDirectories)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q uploads the file it names to GitHub, which the leak guard does not read", argv[attach])) {
				t.Errorf("ghTargetOf(%q) at gh 2.104.0 = %v, want a refusal naming %q", argv, err, argv[attach])
			}
			end := attach + 2
			if strings.Contains(argv[attach], "=") {
				end = attach + 1
			}
			without := slices.Delete(slices.Clone(argv), attach, end)
			if target, err := ghTargetOf(without, nil, latest, noGitRepository, ownersOwnDirectories); err != nil || target.owner != "acme" {
				t.Errorf("ghTargetOf(%q) at gh 2.104.0 = %+v, %v; want acme's call", without, target, err)
			}
			if _, err := ghTargetOf(argv, nil, savedGhHelp, noGitRepository, ownersOwnDirectories); err == nil || !strings.Contains(err.Error(), "is no option") {
				t.Errorf("ghTargetOf(%q) at gh 2.98.0 = %v, want --attach refused as no option", argv, err)
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
		if _, err := ghTargetOf(tc.argv, nil, latest, noGitRepository, ownersOwnDirectories); err == nil || !strings.Contains(err.Error(), tc.refuse) {
			t.Errorf("ghTargetOf(%q) at gh 2.104.0 = %v, want a refusal containing %q", tc.argv, err, tc.refuse)
		}
	}
}

// A gh directory whose hosts.yml or config.yml sends gh's requests elsewhere is refused, naming
// the file and the key and never the value: api_host in a host's block, http_unix_socket at the
// top of config.yml or in a host's block, at any depth and in any shape gh might read it as a key,
// a list's even-indexed items among them (go-gh's FindEntry reads a list as pairs). So is a
// config.yml holding any other hosts block or an alias named `help`, and a file that is no regular
// file (a named pipe, a symbolic link, a directory) or is larger than any the daemon writes, which
// is refused without being read, so a named pipe stalls nothing. The files the daemon renders, a
// config.yml holding aliases, one naming an alias after a command `legion gh` refuses (which gh
// never expands), a routing key's name as a value, and a directory holding neither file pass; one
// that does not parse is refused.
func TestGhConfigShapeRefusesADirectoryThatSendsElsewhere(t *testing.T) {
	hosts := ghconfig.Hosts("ghs_acme_write")
	fifo := func(dir, name string) error { return syscall.Mkfifo(filepath.Join(dir, name), 0o600) }
	link := func(text string) func(dir, name string) error {
		return func(dir, name string) error {
			target := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(target, []byte(text), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, filepath.Join(dir, name))
		}
	}
	directory := func(dir, name string) error { return os.Mkdir(filepath.Join(dir, name), 0o700) }
	large := func(dir, name string) error {
		return os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("#", ghconfig.MaxFileBytes+1)), 0o600)
	}
	at := func(plant func(dir, name string) error, name string) func(dir string) error {
		return func(dir string) error { return plant(dir, name) }
	}
	for _, tc := range []struct {
		name          string
		hosts, config string // "" writes no such file
		plant         func(dir string) error
		want          string // "" for a directory that passes
	}{
		{name: "the files the daemon renders", hosts: hosts, config: ghconfig.Config},
		{name: "a config.yml holding aliases", hosts: hosts, config: ghconfig.Config + "aliases:\n    co: pr checkout\n"},
		{name: "an alias named after a refused command", hosts: hosts, config: ghconfig.Config + "aliases:\n    agent-task: pr view\n"},
		{name: "api_host as a list's value", hosts: "github.com: [user, api_host]\n", config: ghconfig.Config},
		{name: "neither file"},
		{name: "api_host in hosts.yml's github.com block", hosts: hosts + "    api_host: api.example.invalid\n", config: ghconfig.Config, want: "hosts.yml sets api_host"},
		{name: "api_host under the account", hosts: strings.Replace(hosts, "            oauth_token: ghs_acme_write\n", "            oauth_token: ghs_acme_write\n            api_host: api.example.invalid\n", 1), config: ghconfig.Config, want: "hosts.yml sets api_host"},
		{name: "http_unix_socket at the top of config.yml", hosts: hosts, config: ghconfig.Config + "http_unix_socket: /tmp/example.sock\n", want: "config.yml sets http_unix_socket"},
		{name: "api_host in a flow list of hosts.yml", hosts: "github.com: [oauth_token, ghs_acme_write, user, x-access-token, api_host, api.example.invalid]\n", config: ghconfig.Config, want: "hosts.yml sets api_host"},
		{name: "api_host in a block list of hosts.yml", hosts: "github.com:\n    - api_host\n    - api.example.invalid\n", config: ghconfig.Config, want: "hosts.yml sets api_host"},
		{name: "config.yml as a list", hosts: hosts, config: "[http_unix_socket, /tmp/example.sock]\n", want: "config.yml sets http_unix_socket"},
		{name: "http_unix_socket in a host's block of config.yml", hosts: hosts, config: ghconfig.Config + "hosts:\n    github.com:\n        http_unix_socket: /tmp/example.sock\n", want: "config.yml sets http_unix_socket"},
		{name: "any other hosts block in config.yml", hosts: hosts, config: ghconfig.Config + "hosts:\n    github.com:\n        editor: vi\n", want: "config.yml holds a hosts block"},
		{name: "an alias named help", hosts: hosts, config: ghconfig.Config + "aliases:\n    help: api repos/sjawhar/legion\n", want: "config.yml names an alias `help`"},
		{name: "a hosts.yml that does not parse", hosts: "github.com: [\n", config: ghconfig.Config, want: "hosts.yml does not parse as YAML"},
		{name: "a named pipe at hosts.yml", config: ghconfig.Config, plant: at(fifo, ghconfig.HostsFile), want: "hosts.yml is no regular file"},
		{name: "a named pipe at config.yml", hosts: hosts, plant: at(fifo, ghconfig.ConfigFile), want: "config.yml is no regular file"},
		{name: "a symbolic link at hosts.yml", config: ghconfig.Config, plant: at(link(hosts), ghconfig.HostsFile), want: "hosts.yml is no regular file"},
		{name: "a symbolic link at config.yml", hosts: hosts, plant: at(link(ghconfig.Config), ghconfig.ConfigFile), want: "config.yml is no regular file"},
		{name: "a directory at hosts.yml", config: ghconfig.Config, plant: at(directory, ghconfig.HostsFile), want: "hosts.yml is no regular file"},
		{name: "a directory at config.yml", hosts: hosts, plant: at(directory, ghconfig.ConfigFile), want: "config.yml is no regular file"},
		{name: "a hosts.yml larger than any the daemon writes", config: ghconfig.Config, plant: at(large, ghconfig.HostsFile), want: "hosts.yml is larger than any file the daemon writes"},
		{name: "a config.yml larger than any the daemon writes", hosts: hosts, plant: at(large, ghconfig.ConfigFile), want: "config.yml is larger than any file the daemon writes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, text := range map[string]string{ghconfig.HostsFile: tc.hosts, ghconfig.ConfigFile: tc.config} {
				if text != "" {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.plant != nil {
				if err := tc.plant(dir); err != nil {
					t.Fatal(err)
				}
			}
			// A check that opened a named pipe would wait on it, so each runs with a bound.
			done := make(chan error, 1)
			go func() { done <- ghConfigShape(dir) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("ghConfigShape did not return within 5 s")
			}
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("ghConfigShape = %v, want none", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("ghConfigShape = %v, want a refusal containing %q", err, tc.want)
			case err != nil && (strings.Contains(err.Error(), "example.invalid") || strings.Contains(err.Error(), "example.sock") || strings.Contains(err.Error(), "vi\n")):
				t.Fatalf("ghConfigShape's refusal %q quotes the value", err)
			}
		})
	}
}

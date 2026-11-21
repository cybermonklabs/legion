package ghtool

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// allowedCommands are the tool's commands by first word, each with the second words it allows:
// nil for every one but those deniedSubcommands names. Cobra's `ls` alias of list is listed where
// list is. Everything else is refused before anything runs: extension, alias, config, auth,
// codespace, browse, gist, secret, ssh-key, gpg-key, every repo command but view, every release
// command but view and list, every issue command but view, list and comment, and the rest.
var allowedCommands = map[string]map[string]bool{
	"api":      nil,
	"pr":       nil,
	"run":      nil,
	"workflow": nil,
	"search":   nil,
	"repo":     {"view": true},
	"release":  {"view": true, "list": true, "ls": true},
	"issue":    {"view": true, "list": true, "ls": true, "comment": true},
	"label":    {"list": true, "ls": true},
	"cache":    {"list": true, "ls": true},
}

// deniedSubcommands are the subcommands of an allowed group the tool refuses: `pr checkout`
// (`pr co`), which runs git into the working directory, and `run download`, which writes
// artifacts to the daemon's disk.
var deniedSubcommands = map[string]map[string]bool{
	"pr":  {"checkout": true, "co": true},
	"run": {"download": true},
}

// allowedList is the allow list as a refusal and the tool's usage name it.
const allowedList = "api, pr (not pr checkout), run (not run download), workflow, repo view, release view|list, search, issue view|list|comment, label list, cache list"

// Usage is how a call of the gh tool is made for one agent, in the words the tool's description,
// the answer's system prompt and a refusal of a call naming no command of the tool's (allowed)
// share, so the three never disagree: where gh runs and so what every call names, the agent's
// repository and its issue's pull request, the commands the tool takes, and one call that runs
// as written. A model told only that a call was refused, or what gh said of a call that named no
// repository, calls again much the same way; told how to call the tool, it calls it right.
type Usage struct {
	// Repository is the repository the agent's issue works, and PullRequest the number of the
	// issue's pull request the daemon's record holds, 0 where it holds none.
	Repository  ghrepo.Repository
	PullRequest int
}

// Checkout says where gh runs and so what every call names, then the agent's repository and,
// where the record holds one, its issue's pull request. gh runs in the answer's empty working
// directory, where no checkout has a git remote to read a repository from, so a call naming none
// fails (noRepository).
func (u Usage) Checkout() string {
	var b strings.Builder
	b.WriteString("gh runs in an empty directory with no checkout of any repository, so every call names its repository: " +
		"-R <owner>/<repo>, a github.com URL, or, for gh api, a repos/<owner>/<repo>/… endpoint. ")
	fmt.Fprintf(&b, "Your issue's repository is %s", u.Repository)
	if u.PullRequest > 0 {
		fmt.Fprintf(&b, ", and its pull request is #%d", u.PullRequest)
	}
	b.WriteString(".")
	return b.String()
}

// Example is one call that runs as written against the repository, gh's arguments without gh:
// a read of the issue's pull request where the record holds one, and otherwise of the
// repository's newest pull requests.
func (u Usage) Example() []string {
	if u.PullRequest > 0 {
		return []string{"pr", "view", strconv.Itoa(u.PullRequest), "-R", u.Repository.String(), "--json", "state,headRefOid,reviews"}
	}
	return []string{"pr", "list", "-R", u.Repository.String(), "--limit", "5"}
}

// String is the whole usage: Checkout, the commands the tool takes (allowedList) and Example as
// the tool's input.
func (u Usage) String() string {
	input, _ := json.Marshal(struct {
		Args []string `json:"args"`
	}{u.Example()})
	return fmt.Sprintf("%s The gh tool takes only these commands: %s. For example: %s.", u.Checkout(), allowedList, input)
}

// commandWords are the first two words of argv that name its command, as gh reads them: past the
// -R/--repo and --help gh lets come before the command's name. It stops at any other option, which
// it answers as a word, so the allow list refuses it by name.
func commandWords(argv []string) []string {
	var words []string
	for i := 0; i < len(argv) && len(words) < 2; i++ {
		arg := argv[i]
		switch {
		case arg == "-R" || arg == "--repo":
			i++
		case strings.HasPrefix(arg, "--repo=") || (strings.HasPrefix(arg, "-R") && len(arg) > 2) ||
			arg == "--help" || strings.HasPrefix(arg, "--help=") || arg == "-h":
		case strings.HasPrefix(arg, "-"):
			if len(words) == 0 {
				return []string{arg}
			}
			return words
		default:
			words = append(words, arg)
		}
	}
	return words
}

// allowed refuses a command whose words are none of the tool's, naming the refused word, and
// ends the refusal with usage, the agent's Usage as the tool's description gives it: a call with
// no command at all, which is what gh's own --help or -h alone reads as, and one naming gh's
// --version, a command gh has and the tool does not, or one the allow list refuses, is a model
// that does not know how the tool is called, and one told only what it did wrong guesses again.
func allowed(words []string, usage string) string {
	if len(words) == 0 {
		return "a call names no gh command. " + usage
	}
	subcommands, ok := allowedCommands[words[0]]
	refused := ""
	switch {
	case !ok:
		refused = words[0]
	case len(words) > 1 && deniedSubcommands[words[0]][words[1]]:
		refused = words[1]
	case subcommands != nil && (len(words) < 2 || !subcommands[words[1]]):
		refused = strings.Join(words, " ")
	}
	if refused == "" {
		return ""
	}
	return fmt.Sprintf("`gh %s` is not one of the gh tool's commands (%q is refused). %s", strings.Join(words, " "), refused, usage)
}

// refusal is the reason argv, whose command is words, is refused before anything runs, "" for
// none: a command the allow list does not hold (allowed, ending with usage), an option opening a
// browser (--web or -w, anywhere), `gh api --hostname`, which reaches another host, and an option
// reading a file or standard input on the daemon's host (--body-file, a pr or issue command's -F,
// pr create's --recover, a typed field's @<path> for workflow run) — gh api's own options are
// read with its footer (withFooter).
//
// A command whose options the tool holds (specOf: every command the allow list holds) is read
// as pflag reads it (parseArgs) and refused by the long name of an option it gave, never by the
// value an option was given: a body, a title, a merge subject, a search query or a jq
// expression is text, `--body "-worker.Stop() is gone, so the leak is fixed"` among it, and a
// body quoting a removed line of a diff is ordinary. A write form parseArgs cannot read is left
// to withFooter, which reads the same arguments and names what it cannot read; any other form
// it cannot read, one a later gh's new option spells among them, falls back to reading every
// argument, as a command the tool holds no options of does: without the command's options a
// value cannot be told from an option, and a browser the model opens is never worth the doubt.
func refusal(argv, words []string, usage string) string {
	if why := allowed(words, usage); why != "" {
		return why
	}
	if spec, key := specOf(words); spec != nil {
		parsed, err := parseArgs(argv, spec, key)
		_, write := writes[key]
		if err == nil {
			return optionRefusal(parsed, write)
		}
		if write {
			// withFooter reads the same arguments and names what it cannot read.
			return ""
		}
	}
	for i, arg := range argv {
		switch {
		case arg == "--web" || strings.HasPrefix(arg, "--web=") || strings.ContainsRune(shortRun(arg), 'w'):
			return fmt.Sprintf("%q opens a web browser (-w, --web), which the gh tool never does; an option that is not --web is spelled by its long name", arg)
		case arg == "--body-file" || strings.HasPrefix(arg, "--body-file="):
			return fmt.Sprintf("%q reads the text from a file, to which the gh tool cannot add the agent's footer; pass the text itself with --body", arg)
		case (words[0] == "pr" || words[0] == "issue") && strings.ContainsRune(shortRun(arg), 'F'):
			return fmt.Sprintf("%q reads the text from a file (-F, --body-file), to which the gh tool cannot add the agent's footer; pass the text itself with --body (an option that is not -F is spelled by its long name)", arg)
		case arg == "--recover" || strings.HasPrefix(arg, "--recover="):
			return fmt.Sprintf("%q reads a file on the daemon's host; pass the title and body with --title and --body", arg)
		case words[0] == "workflow":
			if value, ok := typedField(argv, i); ok && fieldReadsFile(value) {
				return fmt.Sprintf("%q reads a field's value from a file or standard input; pass the value itself with -f", value)
			}
		}
	}
	return ""
}

// optionRefusal is the reason the options of a command, read against the command's own
// (parseArgs), refuse it before anything runs, "" for none: one opening a browser, one reading
// the text from a file, in either spelling, pr create's --recover, which reads a file an
// earlier run left on the daemon's host, and, for a command that is no write form (write), a
// typed field reading a file (`workflow run -F key=@<path>`). A write form's own fields and the
// forms its footer cannot be added to are withFooter's, which reads the text it posts (gh api's
// -F key=@<path>, --input and --hostname, --editor, a GraphQL mutation).
func optionRefusal(parsed parsedArgs, write bool) string {
	for _, o := range parsed.options {
		switch o.name {
		case "web":
			return fmt.Sprintf("%q opens a web browser (-w, --web), which the gh tool never does", o.arg)
		case "body-file":
			return fmt.Sprintf("%q reads the text from a file (-F, --body-file), to which the gh tool cannot add the agent's footer; pass the text itself with --body", o.arg)
		case "recover":
			return fmt.Sprintf("%q reads a file on the daemon's host; pass the title and body with --title and --body", o.arg)
		case "field":
			if !write && fieldReadsFile(o.value) {
				return fmt.Sprintf("%q reads a field's value from a file or standard input; pass the value itself with -f", o.value)
			}
		}
	}
	return ""
}

// shortRun is the letters of a single-dash option word up to its first other character: the
// shorthands it may stand for, -cw for -c and -w, or a shorthand and the start of its value, -Ffile.
// A word that is no single-dash option has none, and neither does -R, whose value the rest of the
// word is (-Rowner/repo).
func shortRun(arg string) string {
	if len(arg) < 2 || arg[0] != '-' || arg[1] == '-' || arg[1] == 'R' {
		return ""
	}
	end := 1
	for end < len(arg) && (arg[end] >= 'a' && arg[end] <= 'z' || arg[end] >= 'A' && arg[end] <= 'Z') {
		end++
	}
	return arg[1:end]
}

// typedField is the key=value a typed field option (-F, --field) at argv[i] gives, and whether
// argv[i] is one.
func typedField(argv []string, i int) (string, bool) {
	arg := argv[i]
	switch {
	case (arg == "-F" || arg == "--field") && i+1 < len(argv):
		return argv[i+1], true
	case strings.HasPrefix(arg, "--field="):
		return strings.TrimPrefix(arg, "--field="), true
	case strings.HasPrefix(arg, "-F") && len(arg) > 2:
		return strings.TrimPrefix(strings.TrimPrefix(arg, "-F"), "="), true
	}
	return "", false
}

// fieldReadsFile says whether a typed field's key=value reads its value from a file or standard
// input, as gh reads `@<path>` and `@-`.
func fieldReadsFile(field string) bool {
	_, value, _ := strings.Cut(field, "=")
	return strings.HasPrefix(value, "@")
}

// canonicalRefusal refuses a command gh's own help names otherwise than its words do, when that
// changes what the tool allows or adds a footer to: a cobra alias of a refused command (`pr co`
// for pr checkout), refused as allowed refuses the command itself, usage and all, or an alias of
// a command whose footer the tool adds by its words.
func canonicalRefusal(words, command []string, usage string) string {
	if command == nil {
		return ""
	}
	if why := allowed(command[:min(2, len(command))], usage); why != "" {
		return why
	}
	if spoken, read := writeOf(words), writeOf(command); spoken != read {
		return fmt.Sprintf("`gh %s` runs `gh %s`; call it by that name", strings.Join(words, " "), strings.Join(command, " "))
	}
	return ""
}

// footer is the Legion footer of a comment or review the agent posts in answer to question with
// call seq: the agent's session and phase, which the daemon's review intake reads
// (intake.PullRequestReview.LegionSession); for a review, head, the head the agent's recorded
// round read (Agent.Head), which a review App's review decides a round by
// (intake.PullRequestReview.LegionHead), and "" for a comment, whose footer names none; and the
// answer key, <question>.<seq>, by which a restart finds it (Lookup). json.Marshal escapes `>`,
// so nothing in it ends the comment early.
func footer(agent Agent, question string, seq int, head string) string {
	encoded, err := json.Marshal(struct {
		Session string `json:"session"`
		Phase   string `json:"phase"`
		Head    string `json:"head,omitempty"`
		Answer  string `json:"answer"`
	}{agent.Session, agent.Phase, head, question + "." + strconv.Itoa(seq)})
	if err != nil {
		panic(err) // four strings always encode
	}
	return footerMarker + " " + string(encoded) + " -->"
}

// footerMarker opens a Legion footer (intake's legionFooterMarker).
const footerMarker = "<!-- legion:"

// flagSpec is one option of a command the tool reads argv against: its shorthand (0 for none)
// and whether it takes a value.
type flagSpec struct {
	short byte
	value bool
}

// commandSpec is a command's options by long name, from gh 2.98.0's help.
type commandSpec map[string]flagSpec

var (
	helpFlag    = flagSpec{short: 'h'}
	repoFlag    = flagSpec{short: 'R', value: true}
	commentSpec = commandSpec{
		"body": {'b', true}, "body-file": {'F', true}, "create-if-none": {}, "delete-last": {}, "edit-last": {},
		"editor": {short: 'e'}, "web": {short: 'w'}, "yes": {}, "repo": repoFlag, "help": helpFlag,
	}
	reviewSpec = commandSpec{
		"approve": {short: 'a'}, "body": {'b', true}, "body-file": {'F', true}, "comment": {short: 'c'},
		"request-changes": {short: 'r'}, "repo": repoFlag, "help": helpFlag,
	}
	closeSpec  = commandSpec{"comment": {'c', true}, "delete-branch": {short: 'd'}, "repo": repoFlag, "help": helpFlag}
	reopenSpec = commandSpec{"comment": {'c', true}, "repo": repoFlag, "help": helpFlag}
	apiSpec    = commandSpec{
		"allow-escape-sequences": {}, "cache": {value: true}, "field": {'F', true}, "header": {'H', true},
		"hostname": {value: true}, "include": {short: 'i'}, "input": {value: true}, "jq": {'q', true},
		"method": {'X', true}, "paginate": {}, "preview": {'p', true}, "raw-field": {'f', true}, "silent": {},
		"slurp": {}, "template": {'t', true}, "verbose": {}, "help": helpFlag,
	}
)

// writes are the commands that post a comment or a review, by their words, and their options.
var writes = map[string]commandSpec{
	"pr comment":    commentSpec,
	"issue comment": commentSpec,
	"pr review":     reviewSpec,
	"pr close":      closeSpec,
	"pr reopen":     reopenSpec,
	"api":           apiSpec,
}

// optionSpecs are the options of every command the allow list holds, by the words that name it,
// from gh 2.98.0's own help (the reference `legion gh`'s tests keep,
// cmd/legion/testdata/gh-help/reference.txt, which TestEveryAllowedCommandsOptionsAreGhsOwn
// reads this against, and the CI check ties to the worker image's gh). The tool reads a call's
// arguments against them so that it never reads a value as an option: a body, a title, a merge
// subject, a search query or a jq expression beginning with a hyphen is text (refusal). The
// inherited --help and -R/--repo are listed where gh's help lists them for the command.
var optionSpecs = map[string]commandSpec{
	"pr checks": {
		"fail-fast": {}, "interval": {'i', true}, "jq": {'q', true}, "json": {value: true}, "required": {},
		"template": {'t', true}, "watch": {}, "web": {short: 'w'}, "repo": repoFlag, "help": helpFlag,
	},
	"pr close":   closeSpec,
	"pr comment": commentSpec,
	"pr create": {
		"assignee": {'a', true}, "base": {'B', true}, "body": {'b', true}, "body-file": {'F', true},
		"draft": {short: 'd'}, "dry-run": {}, "editor": {short: 'e'}, "fill": {short: 'f'}, "fill-first": {},
		"fill-verbose": {}, "head": {'H', true}, "label": {'l', true}, "milestone": {'m', true},
		"no-maintainer-edit": {}, "project": {'p', true}, "recover": {value: true}, "reviewer": {'r', true},
		"template": {'T', true}, "title": {'t', true}, "web": {short: 'w'}, "repo": repoFlag, "help": helpFlag,
	},
	"pr diff": {
		"allow-escape-sequences": {}, "color": {value: true}, "exclude": {'e', true}, "name-only": {}, "patch": {},
		"web": {short: 'w'}, "repo": repoFlag, "help": helpFlag,
	},
	"pr edit": {
		"add-assignee": {value: true}, "add-label": {value: true}, "add-project": {value: true},
		"add-reviewer": {value: true}, "base": {'B', true}, "body": {'b', true}, "body-file": {'F', true},
		"milestone": {'m', true}, "remove-assignee": {value: true}, "remove-label": {value: true},
		"remove-milestone": {}, "remove-project": {value: true}, "remove-reviewer": {value: true},
		"title": {'t', true}, "repo": repoFlag, "help": helpFlag,
	},
	"pr list": {
		"app": {value: true}, "assignee": {'a', true}, "author": {'A', true}, "base": {'B', true},
		"draft": {short: 'd'}, "head": {'H', true}, "jq": {'q', true}, "json": {value: true}, "label": {'l', true},
		"limit": {'L', true}, "search": {'S', true}, "state": {'s', true}, "template": {'t', true},
		"web": {short: 'w'}, "repo": repoFlag, "help": helpFlag,
	},
	"pr lock": {"reason": {'r', true}, "repo": repoFlag, "help": helpFlag},
	"pr merge": {
		"admin": {}, "author-email": {'A', true}, "auto": {}, "body": {'b', true}, "body-file": {'F', true},
		"delete-branch": {short: 'd'}, "disable-auto": {}, "match-head-commit": {value: true}, "merge": {short: 'm'},
		"rebase": {short: 'r'}, "squash": {short: 's'}, "subject": {'t', true}, "repo": repoFlag, "help": helpFlag,
	},
	"pr ready":  {"undo": {}, "repo": repoFlag, "help": helpFlag},
	"pr reopen": reopenSpec,
	"pr revert": {
		"body": {'b', true}, "body-file": {'F', true}, "draft": {short: 'd'}, "title": {'t', true}, "repo": repoFlag,
		"help": helpFlag,
	},
	"pr review": reviewSpec,
	"pr status": {
		"conflict-status": {short: 'c'}, "jq": {'q', true}, "json": {value: true}, "template": {'t', true},
		"repo": repoFlag, "help": helpFlag,
	},
	"pr unlock":        {"repo": repoFlag, "help": helpFlag},
	"pr update-branch": {"rebase": {}, "repo": repoFlag, "help": helpFlag},
	"pr view": {
		"comments": {short: 'c'}, "jq": {'q', true}, "json": {value: true}, "template": {'t', true},
		"web": {short: 'w'}, "repo": repoFlag, "help": helpFlag,
	},
	"run cancel": {"force": {}, "repo": repoFlag, "help": helpFlag},
	"run delete": {"repo": repoFlag, "help": helpFlag},
	"run list": {
		"all": {short: 'a'}, "branch": {'b', true}, "commit": {'c', true}, "created": {value: true},
		"event": {'e', true}, "jq": {'q', true}, "json": {value: true}, "limit": {'L', true}, "status": {'s', true},
		"template": {'t', true}, "user": {'u', true}, "workflow": {'w', true}, "repo": repoFlag, "help": helpFlag,
	},
	"run rerun": {"debug": {short: 'd'}, "failed": {}, "job": {'j', true}, "repo": repoFlag, "help": helpFlag},
	"run view": {
		"attempt": {'a', true}, "exit-status": {}, "job": {'j', true}, "jq": {'q', true}, "json": {value: true},
		"log": {}, "log-failed": {}, "template": {'t', true}, "verbose": {short: 'v'}, "web": {short: 'w'},
		"repo": repoFlag, "help": helpFlag,
	},
	"run watch":        {"compact": {}, "exit-status": {}, "interval": {'i', true}, "repo": repoFlag, "help": helpFlag},
	"workflow disable": {"repo": repoFlag, "help": helpFlag},
	"workflow enable":  {"repo": repoFlag, "help": helpFlag},
	"workflow list": {
		"all": {short: 'a'}, "jq": {'q', true}, "json": {value: true}, "limit": {'L', true}, "template": {'t', true},
		"repo": repoFlag, "help": helpFlag,
	},
	"workflow run": {
		"field": {'F', true}, "json": {}, "raw-field": {'f', true}, "ref": {'r', true}, "repo": repoFlag,
		"help": helpFlag,
	},
	"workflow view": {
		"ref": {'r', true}, "web": {short: 'w'}, "yaml": {short: 'y'}, "repo": repoFlag, "help": helpFlag,
	},
	"search code": {
		"extension": {value: true}, "filename": {value: true}, "jq": {'q', true}, "json": {value: true},
		"language": {value: true}, "limit": {'L', true}, "match": {value: true}, "owner": {value: true},
		"repo": {'R', true}, "size": {value: true}, "template": {'t', true}, "web": {short: 'w'}, "help": helpFlag,
	},
	"search commits": {
		"author": {value: true}, "author-date": {value: true}, "author-email": {value: true},
		"author-name": {value: true}, "committer": {value: true}, "committer-date": {value: true},
		"committer-email": {value: true}, "committer-name": {value: true}, "hash": {value: true}, "jq": {'q', true},
		"json": {value: true}, "limit": {'L', true}, "merge": {}, "order": {value: true}, "owner": {value: true},
		"parent": {value: true}, "repo": {'R', true}, "sort": {value: true}, "template": {'t', true},
		"tree": {value: true}, "visibility": {value: true}, "web": {short: 'w'}, "help": helpFlag,
	},
	"search issues": {
		"app": {value: true}, "archived": {}, "assignee": {value: true}, "author": {value: true},
		"closed": {value: true}, "commenter": {value: true}, "comments": {value: true}, "created": {value: true},
		"include-prs": {}, "interactions": {value: true}, "involves": {value: true}, "jq": {'q', true},
		"json": {value: true}, "label": {value: true}, "language": {value: true}, "limit": {'L', true}, "locked": {},
		"match": {value: true}, "mentions": {value: true}, "milestone": {value: true}, "no-assignee": {},
		"no-label": {}, "no-milestone": {}, "no-project": {}, "order": {value: true}, "owner": {value: true},
		"project": {value: true}, "reactions": {value: true}, "repo": {'R', true}, "search-type": {value: true},
		"sort": {value: true}, "state": {value: true}, "team-mentions": {value: true}, "template": {'t', true},
		"updated": {value: true}, "visibility": {value: true}, "web": {short: 'w'}, "help": helpFlag,
	},
	"search prs": {
		"app": {value: true}, "archived": {}, "assignee": {value: true}, "author": {value: true}, "base": {'B', true},
		"checks": {value: true}, "closed": {value: true}, "commenter": {value: true}, "comments": {value: true},
		"created": {value: true}, "draft": {}, "head": {'H', true}, "interactions": {value: true},
		"involves": {value: true}, "jq": {'q', true}, "json": {value: true}, "label": {value: true},
		"language": {value: true}, "limit": {'L', true}, "locked": {}, "match": {value: true},
		"mentions": {value: true}, "merged": {}, "merged-at": {value: true}, "milestone": {value: true},
		"no-assignee": {}, "no-label": {}, "no-milestone": {}, "no-project": {}, "order": {value: true},
		"owner": {value: true}, "project": {value: true}, "reactions": {value: true}, "repo": {'R', true},
		"review": {value: true}, "review-requested": {value: true}, "reviewed-by": {value: true},
		"sort": {value: true}, "state": {value: true}, "team-mentions": {value: true}, "template": {'t', true},
		"updated": {value: true}, "visibility": {value: true}, "web": {short: 'w'}, "help": helpFlag,
	},
	"search repos": {
		"archived": {}, "created": {value: true}, "followers": {value: true}, "forks": {value: true},
		"good-first-issues": {value: true}, "help-wanted-issues": {value: true}, "include-forks": {value: true},
		"jq": {'q', true}, "json": {value: true}, "language": {value: true}, "license": {value: true},
		"limit": {'L', true}, "match": {value: true}, "number-topics": {value: true}, "order": {value: true},
		"owner": {value: true}, "size": {value: true}, "sort": {value: true}, "stars": {value: true},
		"template": {'t', true}, "topic": {value: true}, "updated": {value: true}, "visibility": {value: true},
		"web": {short: 'w'}, "repo": repoFlag, "help": helpFlag,
	},
	"repo view": {
		"branch": {'b', true}, "jq": {'q', true}, "json": {value: true}, "template": {'t', true}, "web": {short: 'w'},
		"repo": repoFlag, "help": helpFlag,
	},
	"release list": {
		"exclude-drafts": {}, "exclude-pre-releases": {}, "jq": {'q', true}, "json": {value: true},
		"limit": {'L', true}, "order": {'O', true}, "template": {'t', true}, "repo": repoFlag, "help": helpFlag,
	},
	"release view": {
		"jq": {'q', true}, "json": {value: true}, "template": {'t', true}, "web": {short: 'w'}, "repo": repoFlag,
		"help": helpFlag,
	},
	"issue comment": commentSpec,
	"issue list": {
		"app": {value: true}, "assignee": {'a', true}, "author": {'A', true}, "jq": {'q', true},
		"json": {value: true}, "label": {'l', true}, "limit": {'L', true}, "mention": {value: true},
		"milestone": {'m', true}, "search": {'S', true}, "state": {'s', true}, "template": {'t', true},
		"type": {value: true}, "web": {short: 'w'}, "repo": repoFlag, "help": helpFlag,
	},
	"issue view": {
		"comments": {short: 'c'}, "jq": {'q', true}, "json": {value: true}, "template": {'t', true},
		"web": {short: 'w'}, "repo": repoFlag, "help": helpFlag,
	},
	"label list": {
		"jq": {'q', true}, "json": {value: true}, "limit": {'L', true}, "order": {value: true}, "search": {'S', true},
		"sort": {value: true}, "template": {'t', true}, "web": {short: 'w'}, "repo": repoFlag, "help": helpFlag,
	},
	"cache list": {
		"jq": {'q', true}, "json": {value: true}, "key": {'k', true}, "limit": {'L', true}, "order": {'O', true},
		"ref": {'r', true}, "sort": {'S', true}, "template": {'t', true}, "repo": repoFlag, "help": helpFlag,
	},
}

// ghAliases are the cobra aliases gh's help lists for a command of optionSpecs, each with the
// words that command is held under: `gh pr ls` runs `gh pr list`, whose options it is given.
var ghAliases = map[string]string{
	"pr new":      "pr create",
	"pr ls":       "pr list",
	"run ls":      "run list",
	"workflow ls": "workflow list",
	"release ls":  "release list",
	"issue ls":    "issue list",
	"label ls":    "label list",
	"cache ls":    "cache list",
}

// specOf is the options of the command words name, by the key they are held under, nil for a
// command the tool holds none of: `gh api`, whose second word is its endpoint, by its first word
// alone, and every other by its two, a cobra alias read as the command it runs (ghAliases).
func specOf(words []string) (commandSpec, string) {
	if len(words) == 0 {
		return nil, ""
	}
	if key := writeOf(words); key == "api" {
		return apiSpec, key
	}
	if len(words) < 2 {
		return nil, ""
	}
	key := words[0] + " " + words[1]
	if canonical, ok := ghAliases[key]; ok {
		key = canonical
	}
	return optionSpecs[key], key
}

// writeOf is the key of writes a command's words name, "" for none.
func writeOf(words []string) string {
	if len(words) > 0 && words[0] == "api" {
		return "api"
	}
	if len(words) > 1 {
		if _, ok := writes[words[0]+" "+words[1]]; ok {
			return words[0] + " " + words[1]
		}
	}
	return ""
}

// reads are the commands of the allow list that send GitHub no write, by the words optionSpecs
// holds them under; so is every search. Every other command the tool runs writes, or may
// (Writes): a comment or review it adds the footer to (writes), and every write that posts none,
// pr merge, pr edit, run rerun and workflow run among them, which run as they are (withFooter).
var reads = map[string]bool{
	"pr checks": true, "pr diff": true, "pr list": true, "pr status": true, "pr view": true,
	"run list": true, "run view": true, "run watch": true, "workflow list": true, "workflow view": true,
	"repo view": true, "release list": true, "release view": true, "issue list": true, "issue view": true,
	"label list": true, "cache list": true,
}

// Writes are the calls of calls, as answer_tool_calls records them, that write and that the tool
// let gh run, in their order: every one but a read (reads, and a gh api GET, HEAD or GraphQL
// query), a call asking for help, and one the tool refused, which it records with no owner
// (Answer.refuse) and gh never started. A call the daemon stopped during is one, since gh may have
// made its write. Each is the one type every record of a gh write carries, the reply the loop
// posts, the controller's notice and the questions `legion state` lists alike
// (record.AgentQuestionWrite): its seq among the question's calls; its command words as gh reads
// them (`pr merge`), for gh api its method and endpoint (`api PUT
// repos/acme/widgets/pulls/7/merge`); the repository its arguments name, "<owner>/<name>", or ""
// where they name none; and gh's exit status, nil where gh answered none. Writes reads a call's
// seq, arguments, owner and exit, and nothing else of it.
func Writes(calls []store.AnswerToolCall) []record.AgentQuestionWrite {
	var written []record.AgentQuestionWrite
	for _, call := range calls {
		if call.Owner == "" {
			continue
		}
		if command, repository, ok := writeCommand(call.Args); ok {
			written = append(written, record.AgentQuestionWrite{Seq: call.Seq, Command: command, Repository: repository, Exit: call.Exit})
		}
	}
	return written
}

// writeCommand is the command words and repository of argv, a call the tool ran, and whether it
// writes (Writes). A call whose arguments parseArgs cannot read is judged by its words alone: it
// writes unless they name a read, and it names no repository, so no write is left out for want of
// a reading. A gh api POST to graphql is a query, since the tool refuses a mutation before it runs
// (apiWithFooter).
func writeCommand(argv []string) (string, string, bool) {
	words := commandWords(argv)
	spec, key := specOf(words)
	if key == "" {
		return "", "", false
	}
	parsed, err := parseArgs(argv, spec, key)
	read := err == nil
	if read && parsed.set("help") {
		return "", "", false
	}
	if key == "api" {
		if !read || len(parsed.positionals) < 2 {
			return "api", "", true
		}
		endpoint, method := apiPath(parsed.positionals[1]), apiMethod(parsed)
		if method == "GET" || method == "HEAD" || endpoint == "graphql" {
			return "", "", false
		}
		repository := ""
		if m := apiRepository.FindStringSubmatch(endpoint); m != nil {
			if named, err := ghrepo.Parse("its endpoint's repository", m[1]); err == nil {
				repository = named.String()
			}
		}
		return "api " + method + " " + endpoint, repository, true
	}
	if reads[key] || words[0] == "search" {
		return "", "", false
	}
	repository := ""
	if read {
		repository, _, _, _ = namedTarget(parsed)
	}
	return key, repository, true
}

// apiRepository is the repository a gh api REST endpoint names, $1, `<owner>/<name>`.
var apiRepository = regexp.MustCompile(`^repos/([^/]+/[^/]+)(?:/|$)`)

// occurrence is one option given: its long name, the argument that spelled it (a shorthand run's
// whole word, `-ew`, for each option of the run), and for one taking a value the value and the
// index of the argument whose suffix the value is (the next argument, or the rest of the option's
// own word); for a boolean, its value and index -1.
type occurrence struct {
	name  string
	arg   string
	value string
	index int
}

// parsedArgs is a command's arguments as pflag reads them against its spec. end is the index of
// the `--` that ends its options, len(argv) where none does: where an option added to the call
// goes, since pflag reads every argument after that `--` as a positional.
type parsedArgs struct {
	options     []occurrence
	positionals []string
	end         int
}

// given are the values of the options named name, in order.
func (p parsedArgs) given(name string) []occurrence {
	var given []occurrence
	for _, o := range p.options {
		if o.name == name {
			given = append(given, o)
		}
	}
	return given
}

// set says whether the boolean option name was given true, its last value deciding.
func (p parsedArgs) set(name string) bool {
	given := p.given(name)
	if len(given) == 0 {
		return false
	}
	on, err := strconv.ParseBool(given[len(given)-1].value)
	return err == nil && on
}

// parseArgs reads argv, a whole command's arguments, as pflag reads them against spec: long
// options with = or the next argument, shorthand runs, `--` ending the options. An option spec
// does not hold is refused, since the tool cannot then be certain where the text it adds the
// footer to is.
func parseArgs(argv []string, spec commandSpec, command string) (parsedArgs, error) {
	shorts := map[byte]string{}
	for name, flag := range spec {
		if flag.short != 0 {
			shorts[flag.short] = name
		}
	}
	unknown := func(arg string) error {
		return fmt.Errorf("%q is no option of `gh %s` the gh tool reads; it reads the command's options to add the agent's footer to what it posts", arg, command)
	}
	parsed := parsedArgs{end: len(argv)}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			parsed.positionals = append(parsed.positionals, argv[i+1:]...)
			parsed.end = i
			return parsed, nil
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg[2:], "=")
			flag, ok := spec[name]
			if !ok {
				return parsedArgs{}, unknown(arg)
			}
			switch {
			case !flag.value:
				if !hasValue {
					value = "true"
				}
				parsed.options = append(parsed.options, occurrence{name: name, arg: arg, value: value, index: -1})
			case hasValue:
				parsed.options = append(parsed.options, occurrence{name: name, arg: arg, value: value, index: i})
			case i+1 < len(argv):
				i++
				parsed.options = append(parsed.options, occurrence{name: name, arg: arg, value: argv[i], index: i})
			default:
				return parsedArgs{}, fmt.Errorf("%q has no value", arg)
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			for rest := arg[1:]; rest != ""; {
				name, ok := shorts[rest[0]]
				if !ok {
					return parsedArgs{}, unknown(arg)
				}
				rest = rest[1:]
				switch {
				case !spec[name].value:
					parsed.options = append(parsed.options, occurrence{name: name, arg: arg, value: "true", index: -1})
				case len(rest) > 1 && rest[0] == '=':
					parsed.options = append(parsed.options, occurrence{name: name, arg: arg, value: rest[1:], index: i})
					rest = ""
				case rest != "":
					parsed.options = append(parsed.options, occurrence{name: name, arg: arg, value: rest, index: i})
					rest = ""
				case i+1 < len(argv):
					i++
					parsed.options = append(parsed.options, occurrence{name: name, arg: arg, value: argv[i], index: i})
				default:
					return parsedArgs{}, fmt.Errorf("%q has no value", arg)
				}
			}
		default:
			parsed.positionals = append(parsed.positionals, arg)
		}
	}
	return parsed, nil
}

// withFooter is argv with the agent's footer added to every comment or review it posts, or the
// reason it is refused: a comment command's --body (pr comment, issue comment) and a closing or
// reopening --comment get the footer after their text; a review's --body too, and a bare
// --approve, --request-changes or --comment gets --body with the footer alone; a gh api write (any
// method but GET and HEAD) gets it after every literal body field (-f body=…, -F body=…), and a
// POST creating or submitting a review, whose body is optional, gets `-f body=<footer>` when it
// has none. An option it adds goes before a `--` ending the call's options (parsedArgs.end),
// where gh reads it as the option it is and not as a positional, so the call with its footer
// routes as argv does. A form whose text the tool cannot read is refused: --editor, gh api's
// --input and a typed field's @<path>, and a GraphQL mutation, which has a REST endpoint to use
// instead; so is gh api's --hostname (--body-file and a pr or issue command's -F are refused
// before, by refusal). Every other command, and a write that posts no comment or review (pr
// merge, run rerun, workflow run), runs as it is.
func withFooter(argv, words []string, foot string) ([]string, string) {
	key := writeOf(words)
	spec, ok := writes[key]
	if !ok {
		return argv, ""
	}
	parsed, err := parseArgs(argv, spec, key)
	if err != nil {
		return nil, err.Error()
	}
	run := slices.Clone(argv)
	appendTo := func(name string) bool {
		given := parsed.given(name)
		for _, o := range given {
			run[o.index] += "\n\n" + foot
		}
		return len(given) > 0
	}
	switch key {
	case "pr comment", "issue comment", "pr review":
		if parsed.set("editor") {
			return nil, "--editor opens an editor, which the gh tool has none of; pass the text itself with --body"
		}
		if !appendTo("body") && key == "pr review" && (parsed.set("approve") || parsed.set("request-changes") || parsed.set("comment")) {
			run = slices.Insert(run, parsed.end, "--body", foot)
		}
	case "pr close", "pr reopen":
		appendTo("comment")
	case "api":
		return apiWithFooter(run, parsed, foot)
	}
	return run, ""
}

// pullRequest is the pull request a review is submitted on, as its call names it: its
// repository, `<owner>/<name>`, and its number.
type pullRequest struct {
	repo, number string
}

// reviewNamesNoPullRequest opens the refusal of a review whose pull request its arguments do not
// name by repository and number, whose head the tool reads before it runs (Answer.headRefusal).
const reviewNamesNoPullRequest = "the gh tool reads the head of the pull request a review is on before it submits the review, and this one names no pull request it can read: "

// reviewOf reads argv, whose command is words, for the review it submits: the pull request it is
// on, whether it submits one at all, and the reason it is refused before anything runs, "" for
// none. A review is any `pr review` (--approve, --request-changes or --comment, bare or with a
// body), a gh api POST creating or submitting one (apiReviewCreate), and a gh api PUT replacing a
// submitted one's body (apiReviewBody), whose last footer is the one the review intake reads; a
// call asking for help submits nothing, and neither does a form parseArgs cannot read, which
// withFooter refuses. A review's dismissal and every comment are no review.
//
// A review App's review decides a round only when its footer names the pull request's current
// head (workflow's refusesRound), and the head the footer names is head, the one the agent's
// recorded round read (Agent.Head). So a review is refused when the record names no head, since
// it would decide nothing; when its arguments do not name its pull request by repository and
// number (a URL, or a number or #number with -R), since the tool reads that pull request's head
// before it runs (Answer.headRefusal); and when it is a gh api review whose commit_id field names
// another commit than head, the commit GitHub would hold the review to.
func reviewOf(argv, words []string, head string) (pullRequest, bool, string) {
	key := writeOf(words)
	if key != "pr review" && key != "api" {
		return pullRequest{}, false, ""
	}
	parsed, err := parseArgs(argv, writes[key], key)
	if err != nil || parsed.set("help") {
		return pullRequest{}, false, ""
	}
	var endpoint []string
	if key == "api" {
		if len(parsed.positionals) > 1 {
			switch path := apiPath(parsed.positionals[1]); apiMethod(parsed) {
			case "POST":
				endpoint = apiReviewCreate.FindStringSubmatch(path)
			case "PUT":
				endpoint = apiReviewBody.FindStringSubmatch(path)
			}
		}
		if endpoint == nil {
			return pullRequest{}, false, ""
		}
	}
	if head == "" {
		return pullRequest{}, true, "your record names no head your round read (your role's phases row records no commit it completed against), " +
			"and a review decides a round only when its footer names the head it read and that head is the pull request's current one, " +
			"so a review from here would decide nothing and is not submitted. The round waits for the reviewer itself; answer with that."
	}
	if key == "pr review" {
		repo, number, selector, err := namedTarget(parsed)
		switch {
		case err != nil:
			return pullRequest{}, true, reviewNamesNoPullRequest + err.Error()
		case number == "" && selector == "":
			return pullRequest{}, true, reviewNamesNoPullRequest + "it names no pull request, and the answer's empty working directory has no branch gh could find one by; name it by its number with -R <owner>/<repo>, or by its URL"
		case number == "":
			return pullRequest{}, true, reviewNamesNoPullRequest + fmt.Sprintf("its selector %q is no pull request number, #number or URL; name it by its number with -R <owner>/<repo>, or by its URL", selector)
		}
		return pullRequest{repo: repo, number: number}, true, ""
	}
	repo, err := ghrepo.Parse("its endpoint's repository", endpoint[1])
	if err != nil {
		return pullRequest{}, true, reviewNamesNoPullRequest + err.Error()
	}
	if number := endpoint[2]; strings.Trim(number, "0123456789") != "" {
		return pullRequest{}, true, reviewNamesNoPullRequest + fmt.Sprintf("its endpoint's pull request %q is no number", number)
	}
	for _, field := range append(parsed.given("raw-field"), parsed.given("field")...) {
		if name, value, _ := strings.Cut(field.value, "="); name == "commit_id" && value != head {
			return pullRequest{}, true, fmt.Sprintf("the review's commit_id %q is not %s, the head your recorded round read: a review from here is of that head alone, so give it as commit_id or give none", value, head)
		}
	}
	return pullRequest{repo: repo.String(), number: endpoint[2]}, true, ""
}

// apiReviewCreate is a gh api endpoint that creates or submits a pull request review, whose body
// is optional: a POST to it posts a review with no text unless it is given one. $1 is the pull
// request's repository, $2 its number.
var apiReviewCreate = regexp.MustCompile(`^repos/([^/]+/[^/]+)/pulls/([^/]+)/reviews(?:/[^/]+/events)?$`)

// apiReviewBody is a gh api endpoint that replaces a submitted review's body with a PUT: the body
// the review intake reads the review's last Legion footer from, so the head its earlier footer
// named is lost unless the new body names one. $1 is the pull request's repository, $2 its number.
var apiReviewBody = regexp.MustCompile(`^repos/([^/]+/[^/]+)/pulls/([^/]+)/reviews/[^/]+$`)

// graphQLMutation finds a mutation operation in a GraphQL document.
var graphQLMutation = regexp.MustCompile(`\bmutation\b`)

// apiMethod is the HTTP method a gh api call makes: GET, POST once it passes any field, and the
// last --method it names, upper-cased.
func apiMethod(parsed parsedArgs) string {
	method := "GET"
	if len(parsed.given("field"))+len(parsed.given("raw-field")) > 0 {
		method = "POST"
	}
	if given := parsed.given("method"); len(given) > 0 {
		method = strings.ToUpper(given[len(given)-1].value)
	}
	return method
}

// apiWithFooter is withFooter's gh api: run the call's arguments, parsed as gh api reads them.
func apiWithFooter(run []string, parsed parsedArgs, foot string) ([]string, string) {
	if len(parsed.given("hostname")) > 0 {
		return nil, "--hostname sends the request to another host than github.com, which the gh tool never reaches"
	}
	if len(parsed.given("input")) > 0 {
		return nil, "--input reads the request's body from a file or standard input, to which the gh tool cannot add the agent's footer; pass each field with -f"
	}
	fields := append(parsed.given("raw-field"), parsed.given("field")...)
	for _, field := range parsed.given("field") {
		if fieldReadsFile(field.value) {
			return nil, fmt.Sprintf("%q reads a field's value from a file or standard input; pass the value itself with -f", field.value)
		}
	}
	endpoint := ""
	if len(parsed.positionals) > 1 {
		endpoint = apiPath(parsed.positionals[1])
	}
	if endpoint == "graphql" {
		for _, field := range fields {
			if name, value, _ := strings.Cut(field.value, "="); name == "query" && graphQLMutation.MatchString(value) {
				return nil, "a GraphQL mutation cannot carry the agent's footer; make the write through its REST endpoint (a comment: repos/<owner>/<repo>/issues/<number>/comments with -f body=<text>)"
			}
		}
	}
	method := apiMethod(parsed)
	if method == "GET" || method == "HEAD" {
		return run, ""
	}
	bodies := 0
	for _, field := range fields {
		if name, _, _ := strings.Cut(field.value, "="); name == "body" {
			run[field.index] += "\n\n" + foot
			bodies++
		}
	}
	if bodies == 0 && method == "POST" && apiReviewCreate.MatchString(endpoint) {
		run = slices.Insert(run, parsed.end, "-f", "body="+foot)
	}
	return run, ""
}

// apiPath is a gh api endpoint as a path, for matching: no API host, leading slash or query.
// https://api.github.com/ is the one host form a call that runs can carry, since the routing
// refuses every other absolute endpoint before gh starts (ghroute's selectors.endpoint).
func apiPath(endpoint string) string {
	path := endpoint
	if lower := strings.ToLower(path); strings.HasPrefix(lower, "https://api.github.com/") {
		path = path[len("https://api.github.com/"):]
	}
	path, _, _ = strings.Cut(strings.TrimPrefix(path, "/"), "?")
	return path
}

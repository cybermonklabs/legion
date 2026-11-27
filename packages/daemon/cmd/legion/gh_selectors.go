package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// selectors is every repository a command's selectors name, and every owner one names with no
// repository of it, each once and with the argument that first named it (named): what the
// bundle routes the command by (ghConfigDirFor), so a repository a configured rename moved is
// one key and its owner's every other repository another, whichever owner names them; whether an
// argument names the command's own repository (targeted), so gh resolves none of its own;
// whether gh api's arguments hold a placeholder gh fills from the command's own repository
// (readsBase); and whether the working directory's git remotes named a github.com repository
// for the command (readRemote).
type selectors struct {
	api       bool
	named     []ghNamed
	targeted  bool
	readsBase bool
	// readRemote is set by fromWorkingDirectory for every remote line it counts that names a
	// github.com repository, before add, which drops a line naming what a selector already
	// named: the remotes are read after the command's own words, so a remote naming the
	// repository an argument names leaves no entry of its own, and still says gh may act on the
	// working directory's repository. A call whose remotes were read so is never run with the
	// fallback directory (dir): R4 of dispatch://INFRA-3399 lets a read through only where the
	// reader "finds no github.com remote there, whether or not it keeps that remote's entry".
	readRemote bool
}

// ghNamed is one thing a command's selectors name, keyed as the bundle routes it: a repository,
// its lowercased owner and its name keyed as gh reads a name of it (ghconfig.RepositoryName), or
// an owner named with no repository of it, whose name is "". kind is the slot that first named it
// ("a -R value", "a repository argument", "the working directory's remote …"), never the value
// as the person wrote it, which a refusal names with the owner and the name read from it.
type ghNamed struct {
	owner, name, kind string
}

// shown is the named thing as a refusal names it: its slot, and the owner and the name read from
// the value, which add's callers have checked before recording them (selectors.add).
func (n ghNamed) shown() string {
	if n.name == "" {
		return n.kind + " naming " + n.owner
	}
	return n.kind + " naming " + n.owner + "/" + n.name
}

// add records owner and name as named by the slot kind, unless a selector already named them. A
// refusal prints all three (ghNamed.shown), so every caller passes an owner ghconfig.IsOwner
// admitted and a name, if any, ghrepo.IsName admitted once its `.git` was trimmed, and a kind built
// from literals, help-listed names and ghRemote.what, which prints a remote's URL or a marker's
// value only as a checked OWNER/REPO.
func (s *selectors) add(owner, name, kind string) {
	if !slices.ContainsFunc(s.named, func(named ghNamed) bool { return named.owner == owner && named.name == name }) {
		s.named = append(s.named, ghNamed{owner: owner, name: name, kind: kind})
	}
}

// repository records the owner and the repository value names: a selector naming one repository
// of one owner, from the slot kind. The owner is lowercased, as a bundle's directory names an
// owner, and the repository is keyed as gh reads a name of it (ghconfig.RepositoryName), so two
// spellings of one repository — `Legion-Smoke` in a pull request's URL beside `-R
// sjawhar/legion-smoke`, or a remote's `widgets.git` beside `widgets` — are the one repository
// the command names rather than two, which would leave the call's record naming no repository at
// all (onlyRepository).
func (s *selectors) repository(value, kind string) error {
	owner, repo, err := repositoryTarget(value, kind, s.api)
	if err == nil {
		s.add(owner, ghconfig.RepositoryName(repo), kind)
	}
	return err
}

// ownerAlone records owner as named with no repository of it, the one recording both owner and
// pairOwner make: the command reaches whatever of that owner's gh resolves, so the directory it
// needs for that owner is the owner's own.
func (s *selectors) ownerAlone(owner, kind string) {
	s.add(owner, "", kind)
}

// owner records the owner value names, from the slot kind, and no repository.
func (s *selectors) owner(value, kind string) error {
	owner, err := checkedOwner(value, kind, s.api)
	if err == nil {
		s.ownerAlone(owner, kind)
	}
	return err
}

// pairOwner records the owner of value, OWNER/TEAM or OWNER/NUMBER, from the slot kind, and no
// repository: a team and a project are none of the owner's repositories. The pair is read as a
// repository's name only to take the owner out of it.
func (s *selectors) pairOwner(value, kind string) error {
	owner, _, err := repositoryTarget(value, kind, s.api)
	if err == nil {
		s.ownerAlone(owner, kind)
	}
	return err
}

// onlyOwner is the one owner every selector of the command named, "" where they named none and
// where they named two: a command naming two names of one repository, the name a configured
// rename moved it away from and the one it moved to, names two owners and runs all the same
// (dir).
func (s *selectors) onlyOwner() string {
	if len(s.named) == 0 || slices.ContainsFunc(s.named, func(named ghNamed) bool { return named.owner != s.named[0].owner }) {
		return ""
	}
	return s.named[0].owner
}

// onlyRepository is the one repository every selector of the command named, keyed as gh reads a
// name of it (ghconfig.RepositoryName, the key selectors.repository records): "" where a
// selector named an owner alone, where the selectors named two repositories, and where they
// named none. It is the repository the leak guard is told the call acts on
// (leakguard.GhRequest.Repo), with its owner (onlyOwner), which a command naming two of them, or
// none, names none of.
func (s *selectors) onlyRepository() string {
	if len(s.named) != 1 {
		return ""
	}
	return s.named[0].name
}

// dir is the gh directory the command runs with: the one directory every repository its
// selectors name, and every owner one names alone, is routed to by dirFor, which is the bundle's
// own pick (ghConfigDirFor), and "" for a command whose selectors name none. A routing dirFor
// refuses — an owner the bundle holds no directory for, a pair whose `to` owner it holds none for
// — is the refusal of the command. So is a command whose selectors route to two directories,
// naming each directory by the owner it is the directory of (ghconfig.Dir) and the arguments
// routed to it: the bundle answers each of those repositories from its own directory, so
// whichever one gh ran with, it would read the other's with a token that may not reach them, and
// answer for them with less than GitHub holds and no error at all. Two repositories of one owner
// are two directories where a configured rename moved one of them, and two names of one
// repository, the name a rename moved it away from and the one it moved to, are one directory.
//
// fallback is the directory a command naming no owner runs with, GH_CONFIG_DIR's, for a call that
// writes nothing, and "" for every other call (ghTargetOf). Given one, a call that names at least
// one item, whose every item dirFor answers as an owner the bundle holds no directory for, and
// whose working directory's git remotes named no github.com repository for it (readRemote), runs
// with the fallback rather than being refused (R4 of dispatch://INFRA-3399): the bundle holds no
// token of that owner's, and a call that writes nothing changes nothing on GitHub with the pane's
// own. Every other call gets the answer above, exactly as with no fallback: the first refusal in
// the order its items were named, or the routing of them all.
func (s *selectors) dir(dirFor func(owner, name string) (string, error), fallback string) (string, error) {
	// fallsBack stays true while every item so far is an owner the bundle holds no directory for.
	fallsBack := fallback != "" && !s.readRemote
	var refusal error
	var dirs []string
	routed := map[string][]string{}
	for _, named := range s.named {
		dir, err := dirFor(named.owner, named.name)
		// A bare OwnerNotServedError, asserted on the error itself and never looked for inside
		// it (errors.As), is ghConfigDirFor's answer for an owner the bundle holds no directory
		// for, and nothing else is. A repository a configured rename pair moves never counts as
		// one: where the pair's `to` owner has a directory, dirFor serves the repository from it,
		// which this routes as every served item, and where that owner has none, the refusal
		// names the pair and wraps the OwnerNotServedError inside its own words.
		if _, unserved := err.(*leakguard.OwnerNotServedError); !unserved {
			fallsBack = false
		}
		if err != nil && refusal == nil {
			refusal = err
		}
		switch {
		case refusal != nil && !fallsBack:
			return "", refusal
		case err != nil:
			continue
		}
		if _, seen := routed[dir]; !seen {
			dirs = append(dirs, dir)
		}
		routed[dir] = append(routed[dir], named.shown())
	}
	if refusal != nil {
		// Every item is an owner the bundle holds no directory for.
		return fallback, nil
	}
	switch len(dirs) {
	case 0:
		return "", nil
	case 1:
		return dirs[0], nil
	}
	listed := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		listed = append(listed, fmt.Sprintf("%s (%s)", filepath.Base(dir), strings.Join(routed[dir], ", ")))
	}
	slices.Sort(listed)
	return "", fmt.Errorf("the command routes to the gh directories of more than one owner, %s, so it sends no token; name one repository", strings.Join(listed, " and "))
}

// ghPullRequestPath is the path of the one URL gh's pr finder reads as naming its pull request,
// and so its repository, `/OWNER/REPO/pull/N`; it reads any other URL as the name of a branch of
// the command's own repository.
var ghPullRequestPath = regexp.MustCompile(`^/[^/]+/[^/]+/pull/[0-9]+`)

// fromCommand collects the owners the leaf's arguments and options name, and whether one names
// the command's own repository: -R, a repository, owner or URL argument (through gh's pr finder,
// only a pull request's), or its quirk's target option.
func (s *selectors) fromCommand(cmd *ghCommand, parsed ghArgs) error {
	leaf, quirk := cmd.quoted(), quirkOf(cmd.path)
	s.targeted = len(parsed.values["repo"]) > 0 && cmd.readsGHRepo()
	if quirk.targetOption != "" && len(parsed.values[quirk.targetOption]) > 0 {
		s.targeted = true
	}
	if s.api {
		if err := s.fromAPI(parsed); err != nil {
			return err
		}
	} else {
		placed, ok := cmd.slotsFor(len(parsed.positionals))
		if !ok {
			return cannotPlace(ordinalArgument(len(cmd.slots)+1), "has no place in "+leaf+"'s usage", false)
		}
		var query []string
		for i, arg := range parsed.positionals {
			kind, unknown := placed[i].kind()
			if unknown != "" {
				return cannotPlace(ordinalArgument(i+1), fmt.Sprintf("fills <%s> of %s's usage, an argument this does not know", unknown, leaf), false)
			}
			switch kind {
			case "repository":
				if err := s.repository(arg, "a repository argument"); err != nil {
					return err
				}
				s.targeted = true
			case "new-repository":
				// OWNER/NAME names the new repository's owner; a name alone, none.
				if strings.Contains(arg, "/") {
					if err := s.repository(arg, "a repository argument"); err != nil {
						return err
					}
					s.targeted = true
				}
			case "second-repository":
				// Another repository the command acts on beside its own, which gh resolves
				// (fromOwnRepository).
				if err := s.repository(arg, "a repository argument"); err != nil {
					return err
				}
			case "owner":
				if err := s.owner(arg, "an owner argument"); err != nil {
					return err
				}
				s.targeted = true
			case "url":
				if isWebURL(arg) {
					if err := s.repository(arg, "a URL argument"); err != nil {
						return err
					}
					// gh's pr finder acts on the URL's repository only for a pull request's
					// URL; any other names a branch of its own, so both owners count.
					if u, err := url.Parse(arg); !quirk.pullRequestFinder || (err == nil && ghPullRequestPath.MatchString(u.Path)) {
						s.targeted = true
					}
				}
			case "query":
				query = append(query, arg)
			}
		}
		if err := s.query(query); err != nil {
			return err
		}
	}
	return s.fromOptions(cmd, parsed)
}

// fromOwnRepository collects the owner of the repository gh takes as the command's own when no
// argument names it and the leaf reads one (readsOwnRepository), and, for a command naming no
// owner at all, wherever gh might: GH_REPO for a leaf that reads it, gh api's base placeholders
// among them, and otherwise the working directory's git remotes (fromWorkingDirectory): origin's
// alone where the leaf honours gh's default marker (honoursDefaultMarker) and the marker makes
// origin's repository gh's own, every one otherwise. A leaf whose quirk reads every remote reads
// them whatever names its repository. So an option or a second repository naming one owner while
// gh acts on another's repository names two owners, and is refused unless one directory answers
// for both (dir).
func (s *selectors) fromOwnRepository(cmd *ghCommand, parsed ghArgs, env map[string]string, remotes func() ([]ghRemote, error)) error {
	if quirkOf(cmd.path).readsAllRemotes {
		return s.fromWorkingDirectory(remotes, cmd.honoursDefaultMarker(parsed))
	}
	reads := !s.targeted && cmd.readsOwnRepository(len(parsed.positionals))
	if s.api {
		reads = s.readsBase
	}
	if !reads && len(s.named) > 0 {
		return nil
	}
	if repo := env["GH_REPO"]; repo != "" && (cmd.readsGHRepo() || (s.api && s.readsBase)) {
		return s.repository(repo, "the GH_REPO value")
	}
	return s.fromWorkingDirectory(remotes, cmd.honoursDefaultMarker(parsed))
}

// fromAPI collects the owners gh api's endpoint and fields name: the endpoint's (endpoint); a
// GraphQL query's variable named for an owner (`-F owner=<login>`), unless it holds a placeholder;
// and a search endpoint's q field, its query. A placeholder gh fills from the command's own
// repository, in the endpoint or a typed (-F) field's value, which gh fills and a raw (-f) one's it
// does not, sets readsBase. Any other GraphQL query names no repository gh could resolve.
func (s *selectors) fromAPI(parsed ghArgs) error {
	if len(parsed.positionals) > 1 {
		return cannotPlace(ordinalArgument(2), "has no place in `gh api`'s usage", true)
	}
	search, graphql := false, false
	if len(parsed.positionals) == 1 {
		endpoint := parsed.positionals[0]
		s.readsBase = readsBaseRepository(endpoint)
		graphql = endpoint == "graphql"
		var err error
		if search, err = s.endpoint(endpoint); err != nil {
			return err
		}
	}
	for _, option := range []string{"field", "raw-field"} {
		for _, field := range parsed.values[option] {
			key, value, _ := strings.Cut(field, "=")
			if option == "field" && readsBaseRepository(value) {
				s.readsBase = true
			}
			switch {
			case graphql && (key == "owner" || key == "org" || key == "organization" || key == "login") && !ghPlaceholder.MatchString(value):
				if err := s.owner(value, "a GraphQL owner field"); err != nil {
					return err
				}
			case search && key == "q":
				if option == "field" && strings.HasPrefix(value, "@") {
					return cannotPlace(ghShown(field, "the q field"), "reads the search query from a file", true)
				}
				if err := s.query(strings.Fields(value)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// fromOptions collects the owners the leaf's options name, by what each selects (ghOptionKind): a
// repository or an owner, each of a comma-separated list; a repository of --org's, a name alone
// that organization's; a team or a project, whose owner is the first half of its name and whose
// second half is no repository; github.com alone as a host; a web URL; nothing. An option this
// cannot read is refused.
func (s *selectors) fromOptions(cmd *ghCommand, parsed ghArgs) error {
	orgs := parsed.values["org"]
	for _, name := range slices.Sorted(maps.Keys(parsed.values)) {
		flag := cmd.flags[name]
		if s.api && (name == "field" || name == "raw-field") {
			continue
		}
		for _, value := range parsed.values[name] {
			switch flag.selects {
			case "unknown":
				return cannotPlace(strconv.Quote("--"+name), "is an option whose help names a repository, an owner, a team or a URL in a way this does not know", s.api)
			case "", "nothing":
				continue
			}
			for _, item := range strings.Split(value, ",") {
				kind := "a --" + name + " value"
				if name == "repo" {
					kind = "a -R value"
				}
				var err error
				switch flag.selects {
				case "host":
					if !isASCII(item) || strings.ToLower(item) != "github.com" {
						err = cannotPlace(kind, "names another host than github.com", s.api)
					}
				case "owner":
					err = s.owner(item, kind)
				case "org-repositories":
					// gh reads a name alone as a repository of the --org, whose owner that is.
					switch {
					case strings.Contains(item, "/"):
						err = s.repository(item, kind)
					case len(orgs) == 0:
						err = cannotPlace(ghShown(item, "a repository named by its name alone"), "is given to --"+name+", and no --org says whose", s.api)
					}
				case "repository":
					err = s.repository(item, kind)
				case "owner/name":
					err = s.pairOwner(item, kind)
				case "url":
					if isWebURL(item) {
						err = s.repository(item, kind)
					}
				}
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ghQualifier is a search query's argument that gh passes on, and GitHub reads, as one qualifier:
// a name, a colon and a value holding no space, quote, parenthesis or backslash, negated by a
// leading "-".
var ghQualifier = regexp.MustCompile(`^(-?)([A-Za-z][A-Za-z-]*):([^\s"'()\\]+)$`)

// query collects the owners a search query names in its repo:<owner>/<repo>, owner:, user: and
// org: qualifiers, each word of words one argument gh joins into the query. A word holding a colon
// that is no plain qualifier is refused, since gh quotes a word holding a space and GitHub reads a
// quoted or grouped qualifier otherwise. So is a query naming an owner whose scope is not that
// owner's: one excluding an owner (`-repo:`), or joining its words with OR, NOT or parentheses.
// Each word it refuses is named by its place.
func (s *selectors) query(words []string) error {
	var named bool
	joined := ""
	for _, word := range words {
		if !strings.Contains(word, ":") {
			if joined == "" && (strings.EqualFold(word, "OR") || strings.EqualFold(word, "NOT") || strings.ContainsAny(word, "()")) {
				joined = word
			}
			continue
		}
		qualifier := ghQualifier.FindStringSubmatch(word)
		if qualifier == nil {
			return cannotPlace(ghShown(word, "a word of the search query"), "is a search qualifier this cannot read for certain, since gh and GitHub read a quoted, spaced or grouped one otherwise", s.api)
		}
		switch name := strings.ToLower(qualifier[2]); name {
		case "repo", "owner", "user", "org":
			if qualifier[1] == "-" {
				return cannotPlace(ghShown(word, "a -repo:, -owner:, -user: or -org: qualifier of the search query"), "excludes an owner, so the search's scope is no one owner's", s.api)
			}
			if name == "repo" {
				if err := s.repository(qualifier[3], "a repo: qualifier"); err != nil {
					return err
				}
			} else if err := s.owner(qualifier[3], "an "+name+": qualifier"); err != nil {
				return err
			}
			named = true
		}
	}
	if named && joined != "" {
		return cannotPlace(ghShown(joined, "an OR, a NOT or a parenthesis of the search query"), "joins the query's repo:/owner: qualifiers with others, so the search's scope is no one owner's", s.api)
	}
	return nil
}

// endpoint collects the owner a gh api endpoint names, and the repository where it names one:
// repos/<owner>/<repo>, repos/<owner>/, orgs/<owner>/ or users/<owner>/, with or without an https
// URL's host before it, one of ghAPIHosts, whose paths are read alike, and a search endpoint's q,
// reporting whether it is a search endpoint, whose q field is its query too. A placeholder in the
// owner's place names none (GH_REPO's, or the working directory's, fills it), and one in the
// repository's names the owner alone, since what fills it is read where gh reads it
// (fromOwnRepository); a repository named by id (repositories/<id>), any other host (isGhAPIHost)
// and any other scheme, http among them, over which gh would send the owner's token unencrypted,
// are errors. gh reads an endpoint holding "://" as a whole URL. An endpoint it refuses is named
// by its place: a token stands in its query (`repositories/1?access_token=<token>`) as readily as
// in its host's user info.
func (s *selectors) endpoint(endpoint string) (search bool, err error) {
	path := endpoint
	if strings.Contains(endpoint, "://") {
		parsed, err := url.Parse(endpoint)
		if err != nil || !isGhAPIHost(parsed.Host) {
			return false, cannotPlace(ghShown(endpoint, "the endpoint's host"), "is another host than "+strings.Join(ghAPIHosts, " or "), true)
		}
		if parsed.Scheme != "https" {
			return false, cannotPlace(ghShown(endpoint, "the endpoint"), "is no https URL", true)
		}
		path = parsed.Path
		if parsed.RawQuery != "" {
			path += "?" + parsed.RawQuery
		}
	}
	path, rawQuery, _ := strings.Cut(strings.TrimPrefix(path, "/"), "?")
	segments := strings.Split(path, "/")
	switch first := strings.ToLower(segments[0]); {
	case first == "repositories":
		return false, cannotPlace(ghShown(endpoint, "the endpoint's repositories/<id> path"), "names a repository by its id", true)
	case (first == "repos" || first == "orgs" || first == "users") && len(segments) > 1:
		if ghPlaceholder.MatchString(segments[1]) {
			return false, nil
		}
		if first == "repos" && len(segments) > 2 && segments[2] != "" && !ghPlaceholder.MatchString(segments[2]) {
			return false, s.repository(segments[1]+"/"+segments[2], "the endpoint")
		}
		return false, s.owner(segments[1], "the endpoint")
	case first == "search":
		query, err := url.ParseQuery(rawQuery)
		if err != nil {
			return true, cannotPlace(ghShown(endpoint, "the endpoint's query string"), "is one this cannot read", true)
		}
		for _, q := range query["q"] {
			if err := s.query(strings.Fields(q)); err != nil {
				return true, err
			}
		}
		return true, nil
	}
	return false, nil
}

// ghAPIHosts are the hosts a gh api endpoint may name in a whole URL: GitHub's REST API, and the
// host a release's assets are uploaded to (repos/<owner>/<repo>/releases/<id>/assets).
var ghAPIHosts = []string{"api.github.com", "uploads.github.com"}

// isGhAPIHost says whether host is one of ghAPIHosts, in any ASCII case and with no port: a host
// spelled in ASCII alone (isASCII), lowered.
func isGhAPIHost(host string) bool {
	return isASCII(host) && slices.Contains(ghAPIHosts, strings.ToLower(host))
}

// isASCII says whether s holds no rune above unicode.MaxASCII: the test every comparison of a host
// name here makes before it lowers the name with strings.ToLower, which on ASCII text lowers A-Z
// alone. Unicode case mapping takes some letters onto ASCII ones, strings.ToLower `İ` (U+0130) to
// `i` and the Kelvin sign (U+212A) to `k`, and strings.EqualFold `ſ` (U+017F) onto `s`, so
// `gİthub.com` would compare as github.com and `uploadſ.github.com` as uploads.github.com.
func isASCII(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r > unicode.MaxASCII })
}

// ghPlaceholder is gh api's placeholder as gh reads it, in its endpoint and its typed fields'
// values: :owner, :repo or :branch, or a name in braces, of which gh fills {owner}, {repo} and
// {branch} and leaves any other as it is.
var ghPlaceholder = regexp.MustCompile(`(\:(owner|repo|branch)\b|\{[a-z]+\})`)

// readsBaseRepository says whether s holds a placeholder gh fills from the command's own
// repository, GH_REPO's or the working directory's: its owner or its name, not its branch.
func readsBaseRepository(s string) bool {
	return slices.ContainsFunc(ghPlaceholder.FindAllString(s, -1), func(p string) bool {
		return p == ":owner" || p == ":repo" || p == "{owner}" || p == "{repo}"
	})
}

// isWebURL says whether s is an http or https URL, as gh reads a URL argument: its scheme in any
// case.
func isWebURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// isGitHubHost says whether host is github.com as gh reads a host: github.com, or one of its
// subdomains (www.github.com, ssh.github.com), in any ASCII case. A host holding a rune above ASCII
// is none (isASCII), though gh, which lowers a host with strings.ToLower, reads `gİthub.com` as
// github.com: a caller that passes over a host this calls none, rather than refusing it, refuses
// such a host first (ghRemote.repository).
func isGitHubHost(host string) bool {
	if !isASCII(host) {
		return false
	}
	host = strings.ToLower(host)
	return host == "github.com" || strings.HasSuffix(host, ".github.com")
}

// repositoryTarget is the lowercased owner and the name of a repository named as gh names one —
// OWNER/REPO, HOST/OWNER/REPO, or a github.com URL, its host github.com or a subdomain as gh
// normalizes it (isGitHubHost) — given in the slot kind ("a -R value", "a repository argument"),
// refused when it names no owner, another host, an owner that is no GitHub login, or a name of a
// character no GitHub repository's name holds (ghrepo.IsName). The name is the second segment as
// gh reads it, whatever follows it (a pull request's number, a tree's path), as the argument
// spells it. The caller that records it keys it as gh reads a name of it (selectors.repository,
// ghconfig.RepositoryName), so two spellings of one repository are one selector.
//
// A refusal names the value by its slot alone (ghShown): a URL holding a password, or a user name
// other than an ssh:// or git+ssh:// URL's SSH login (`ssh://git@github.com/o/r`,
// `git+ssh://git@github.com/o/r`, which gh reads as ssh and which place as before, as the remote
// rule reads them), a value with no scheme holding an "@", a ":", a "?" or a "#" in any segment, on
// a github.com host or another, where a user name, a password or a query stands
// (`x-access-token:<token>@ghe.example/o/r`, `<token>@github.com:o/r`,
// `ghe.example/x:<token>@ghe.example/o/r`, `ghe.example/o/r?t=<token>`), and a URL whose owner
// segment holds the user info a person put there
// (`https://github.com/x-access-token:<token>@github.com/o/r`), named an owner that is no login.
func repositoryTarget(value, kind string, api bool) (owner, repo string, err error) {
	shown := ghShown(value, kind)
	host, rest := "", value
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		if _, hasPassword := parsed.User.Password(); parsed.User != nil && (hasPassword || !slices.Contains([]string{"ssh", "git+ssh"}, strings.ToLower(parsed.Scheme))) {
			return "", "", cannotPlace(shown, "holds a user name or a password, which no GitHub repository's URL does, and is not printed", api)
		}
		host, rest = parsed.Hostname(), strings.Trim(parsed.Path, "/")
	} else {
		if strings.ContainsAny(value, "@:?#") {
			return "", "", cannotPlace(shown, "holds an \"@\", a \":\", a \"?\" or a \"#\" where a GitHub repository's name holds none, and is not printed, since a credential stands there", api)
		}
		if parts := strings.Split(strings.Trim(value, "/"), "/"); len(parts) >= 3 {
			host, rest = parts[0], strings.Join(parts[1:], "/")
		}
	}
	if host != "" && !isGitHubHost(host) {
		return "", "", cannotPlace(shown, "names another host than github.com", api)
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", cannotPlace(shown, "names no owner", api)
	}
	if !ghrepo.IsName(strings.TrimSuffix(parts[1], ".git")) {
		return "", "", cannotPlace(shown, "names a repository whose name holds a character no GitHub repository's name does", api)
	}
	owner, err = checkedOwner(parts[0], kind, api)
	return owner, parts[1], err
}

// checkedOwner is owner lowercased, given in the slot kind, refused when it is no GitHub login and
// then named by its place alone.
func checkedOwner(owner, kind string, api bool) (string, error) {
	lower := strings.ToLower(owner)
	if !ghconfig.IsOwner(lower) {
		return "", cannotPlace(kind, "names "+ghShown(owner, "an owner that is not a GitHub login"), api)
	}
	return lower, nil
}

// ghRemote is one line of the working directory's git remotes that gh reads to find the
// repository of a command naming none: a URL line, a remote's fetch or push (push) URL as `git
// remote -v` prints it, or a marker line (marker), a gh-resolved setting, gh's default marker
// (`gh repo set-default`'s), as `git config --get-regexp` prints it: its key, its value
// (resolved), "base" for the remote's own repository or [HOST/]OWNER/REPO for another, and whether
// git printed a value at all (valued), which it does not for a key set with none. name is the
// remote's, for a marker line its key without `remote.` and `.gh-resolved`.
type ghRemote struct {
	name          string
	url           string
	push          bool
	marker        bool
	key, resolved string
	valued        bool
}

// ghMarkerLine is the marker line of one line `git config --get-regexp` prints, `<key> <value>`,
// or `<key>` alone for a key set with no value.
func ghMarkerLine(line string) ghRemote {
	key, value, valued := strings.Cut(line, " ")
	name := strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".gh-resolved")
	return ghRemote{name: name, marker: true, key: key, resolved: value, valued: valued}
}

// what is the remote line as a refusal names it. A URL line is named by its remote's name and, when
// it names a github.com OWNER/REPO this reads (repository), by its URL as remoteShown prints it;
// any other URL, which no rule has read as a repository's and whose path can hold a token
// (`https://github.com/acme/x-access-token:<token>`, where the URL rule finds no user info), as
// redact.URLNotPrinted. A marker line is named by its key and its value as markerShown prints it.
func (r ghRemote) what() string {
	if r.marker {
		return fmt.Sprintf("remote.%s.gh-resolved %s", r.name, markerShown(r.resolved))
	}
	if owner, _, _ := r.repository(); owner == "" {
		return fmt.Sprintf("remote %q %s", r.name, redact.URLNotPrinted)
	}
	return fmt.Sprintf("remote %q (%s)", r.name, remoteShown(r.url))
}

// markerShown is a gh-resolved marker's value as a refusal prints it: as written when it is empty,
// `base` or the OWNER/REPO gh writes there, an owner that is a GitHub login and a name a
// repository's (ghrepo.IsName, its `.git` trimmed), with nothing before or after it; anything
// else, which only a person writes, as redact.URLNotPrinted. A host before it, a URL's user info
// or text after the name (`github.com/acme/widgets/<token>`, which repositoryTarget reads as
// acme/widgets) can hold a token the URL rule does not see.
func markerShown(value string) string {
	if value == "" || value == "base" {
		return value
	}
	if owner, name, ok := strings.Cut(value, "/"); ok && ghconfig.IsOwner(strings.ToLower(owner)) && ghrepo.IsName(strings.TrimSuffix(name, ".git")) {
		return value
	}
	return redact.URLNotPrinted
}

// remoteShown is a remote's URL as legion gh prints it: read as gh reads it (ghParseGitURL, which
// makes an scp-like `git@host:owner/repo` an ssh URL), its query and fragment not at all, its path,
// the repository it names, as written, all through redact.URL, so its user info prints as the
// mark: a token written into a remote URL (`https://x-access-token:<token>@github.com/owner/repo`,
// which a CI checkout writes) printed whole. An ssh URL's user name alone (`ssh://git@host/…`, the
// scp-like `git@host:…`) prints as the mark too, its host with it: the rule cannot tell an SSH
// login from a token written as the user (`<token>@gh-work:owner/repo`), and every command's
// stderr passes the URL rule (newRoot) whatever this writes. A local path, with no ":", "@", "?"
// or "#", prints as written: it holds no scheme, user info, query or fragment. Anything else gh's
// parser reads as no URL with a host prints as redact.URLNotPrinted.
func remoteShown(raw string) string {
	if !strings.ContainsAny(raw, ":@?#") {
		return raw
	}
	u, err := ghParseGitURL(raw)
	if err != nil {
		return redact.URLNotPrinted
	}
	return redact.URL(u.String())
}

// printGhRefusal writes err to stderr as `legion gh: <err>`, through redact.URLCredentials as
// printRefusal writes every other command's refusal, a URL a person put in a selector (GH_REPO,
// -R), a git config key or a remote's marker included.
func printGhRefusal(stderr io.Writer, err error) {
	fmt.Fprintf(stderr, "legion gh: %s\n", redact.URLCredentials(err.Error()))
}

// ghRemoteSchemes are the URL schemes gh's git.ParseURL reads as they are; a remote with no such
// prefix and a colon it reads as scp-like ssh.
var ghRemoteSchemes = []string{"ssh:", "git+ssh:", "git:", "http:", "git+https:", "https:", "ftp:", "ftps:", "file:"}

// ghParseGitURL reads a git remote's URL as gh does (cli/cli's git.ParseURL, gh 2.65.0 through
// 2.98.0): a value with none of ghRemoteSchemes' prefixes, a colon and no backslash becomes an ssh
// URL, its first colon the path's first slash (`[user@]host:path`, `github.com://owner/repo`
// among them); git+https and git+ssh are https and ssh; and an ssh URL's path loses a doubled
// leading slash and its host its port. gh then takes the repository from the host and path
// whatever the scheme.
func ghParseGitURL(raw string) (*url.URL, error) {
	known := slices.ContainsFunc(ghRemoteSchemes, func(scheme string) bool { return strings.HasPrefix(raw, scheme) })
	if !known && strings.Contains(raw, ":") && !strings.Contains(raw, `\`) {
		raw = "ssh://" + strings.Replace(raw, ":", "/", 1)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "git+https":
		u.Scheme = "https"
	case "git+ssh":
		u.Scheme = "ssh"
	}
	if u.Scheme == "ssh" {
		if strings.HasPrefix(u.Path, "//") {
			u.Path = strings.TrimPrefix(u.Path, "/")
		}
		u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
	}
	return u, nil
}

// repository is the lowercased owner and the name of the github.com repository the remote line
// names, read as gh reads it (ghParseGitURL), "" for one naming none gh reads: a remote with no
// host (a local path) or on another host, and a marker line with no value, an empty one or
// "base", whose remote's own URL is read beside it. A URL line's name must have a repository
// name's characters once its `.git` is trimmed (ghrepo.IsName), as a marker's and a -R value's
// must, since the refusal of two owners prints it (ghNamed.shown) and a token can be written into
// the URL's path (`https://github.com/acme/x-access-token:<token>@github.com`, `…/acme/<token>%40x`).
// It is then carried on as the line spells it, `.git` and all, and fromWorkingDirectory keys it
// with ghconfig.RepositoryName. why says why a line is one this cannot read: a URL gh's parser
// refuses, a remote whose host holds a letter beyond ASCII (isASCII), which gh's lowering may make
// github.com, an ssh remote on another host, which gh may read as an alias of github.com from
// ssh's configuration, and a github.com one naming no OWNER/REPO whose owner is a GitHub login
// and whose name is a repository name.
func (r ghRemote) repository() (owner, repo, why string) {
	if r.marker {
		if r.resolved == "" || r.resolved == "base" {
			return "", "", ""
		}
		owner, repo, err := repositoryTarget(r.resolved, "", false)
		if err != nil {
			return "", "", "names no github.com repository this can read"
		}
		return owner, repo, ""
	}
	u, err := ghParseGitURL(r.url)
	switch {
	case err != nil:
		return "", "", "is a URL this cannot read as gh does"
	case u.Hostname() == "":
		return "", "", ""
	case !isASCII(u.Hostname()):
		return "", "", "is a remote whose host holds a letter beyond ASCII, which gh may lower to github.com"
	case u.Scheme == "ssh" && !isGitHubHost(u.Hostname()):
		return "", "", "is an ssh remote whose host gh may read as github.com through ssh's configuration"
	case !isGitHubHost(u.Hostname()):
		return "", "", ""
	}
	parts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 3)
	if owner = strings.ToLower(parts[0]); len(parts) != 2 || parts[1] == "" || !ghconfig.IsOwner(owner) || !ghrepo.IsName(strings.TrimSuffix(parts[1], ".git")) {
		return "", "", "names no OWNER/REPO whose owner is a GitHub login and whose name is a repository name"
	}
	return owner, parts[1], ""
}

// isPlainGitHubURL says whether the URL line names a github.com repository gh reads with nothing
// the wrapper cannot read: gh's parser reads it (ghParseGitURL) as no ssh URL, whose host gh
// rewrites through ssh's own configuration (go-gh pkg/ssh/ssh.go), on an ASCII host (isASCII) gh's
// normalizeHostname (cli/cli internal/ghrepo/repo.go: a leading lower-case `www.` stripped, then
// lower-cased) makes github.com, the one host context/remote.go's FilterByHosts keeps, and it
// names an OWNER/REPO whose owner is a GitHub login and whose name is a repository name
// (repository).
func (r ghRemote) isPlainGitHubURL() bool {
	u, err := ghParseGitURL(r.url)
	if err != nil || u.Scheme == "ssh" || !isASCII(u.Hostname()) || strings.ToLower(strings.TrimPrefix(u.Hostname(), "www.")) != "github.com" {
		return false
	}
	owner, _, _ := r.repository()
	return owner != ""
}

// readsAsMarker says whether the marker line counts among the markers gh reads (cli/cli
// git/client.go, populateResolvedRemotes): git printed a value, and the key has a second
// `.`-separated part, gh's name of its remote (`remote.origin.fork.gh-resolved` is origin's). An
// empty value only clears its remote's marker, which counts on origin, whose `base` it may clear,
// and on no other remote, where a marker it could clear is a line of its own.
func (r ghRemote) readsAsMarker() bool {
	_, rest, named := strings.Cut(r.key, ".")
	remote, _, _ := strings.Cut(rest, ".")
	return r.valued && named && (r.resolved != "" || remote == "origin")
}

// markedOrigin is the index in remotes of origin's fetch line, and whether gh's default marker
// makes that line's repository the one gh resolves: of the marker lines gh reads as one
// (readsAsMarker), one is left, `remote.origin.gh-resolved base`, and origin has one fetch line,
// whose URL gh keeps as a github.com remote with nothing the wrapper cannot read
// (isPlainGitHubURL). gh then takes origin, the first remote marked "base" (cli/cli
// context/context.go, ResolvedRemotes.BaseRepo), and its repository from that fetch URL
// (context/remote.go, TranslateRemotes), whatever origin's push URLs and the other remotes name.
func markedOrigin(remotes []ghRemote) (fetch int, ok bool) {
	var marker ghRemote
	markers, fetches := 0, 0
	for i, remote := range remotes {
		switch {
		case remote.marker && remote.readsAsMarker():
			markers++
			marker = remote
		case !remote.marker && !remote.push && remote.name == "origin":
			fetches++
			fetch = i
		}
	}
	return fetch, markers == 1 && marker.key == "remote.origin.gh-resolved" && marker.resolved == "base" &&
		fetches == 1 && remotes[fetch].isPlainGitHubURL()
}

// fromWorkingDirectory collects the owner and the repository of the github.com repositories the
// working directory's git remotes name (remotes), from which gh takes the repository of a command
// it resolves none for. Every line is read, whether or not it counts, and the first one this
// cannot read is refused; a working directory that is no git repository names none. Where the
// command honours gh's default marker (honoursMarker) and that marker makes origin's repository
// gh's own (markedOrigin), origin's fetch line alone counts, before anything is routed, so an
// upstream whose owner has no gh directory is never routed. Otherwise every line counts, since gh
// may act on any: the first of its sorted remotes, an `upstream` before an `origin`, the
// repository a marker names, or, for repo set-default, every remote's network. So a workspace
// whose remotes name two owners names two, as one whose remotes name two repositories of one
// owner names two repositories, and so no one repository. Each is routed as every selector is
// (dir), so remotes whose repositories two directories answer for are refused. Every line that
// counts and names a github.com repository sets readRemote, whether or not add keeps its entry.
func (s *selectors) fromWorkingDirectory(remotes func() ([]ghRemote, error), honoursMarker bool) error {
	advice := "name the repository with -R <owner>/<repo>"
	if s.api {
		advice = "name it in the endpoint, repos/<owner>/<repo>/…"
	}
	listed, err := remotes()
	if err != nil {
		return fmt.Errorf("cannot tell which repository the command targets: the working directory's git remotes, from which gh takes the command's own repository, cannot be read (%v), so it sends no token; %s", err, advice)
	}
	fetch, marked := 0, false
	if honoursMarker {
		fetch, marked = markedOrigin(listed)
	}
	for i, remote := range listed {
		owner, repo, why := remote.repository()
		if why != "" {
			return fmt.Errorf("cannot tell which repository the command targets: the working directory's %s %s, so it sends no token; %s", remote.what(), why, advice)
		}
		if owner != "" && (!marked || i == fetch) {
			s.readRemote = true
			s.add(owner, ghconfig.RepositoryName(repo), "the working directory's "+remote.what())
		}
	}
	return nil
}

// gitRemoteLine is a line of `git remote -v` as git prints it: `<name>\t<url> (fetch)` or
// ` (push)`, a fetch line of a remote with a partial clone filter followed by ` [<filter>]`
// (builtin/remote.c). git prints the URL and the filter as they are configured, so either can hold
// whitespace, and gh's reading (ghRemoteLine) then takes another URL from the line; the URL and the
// filter accepted here hold none of the whitespace gh's `\s` matches.
var gitRemoteLine = regexp.MustCompile(`^([^\t]+)\t([^\t\n\f\r ]+) \((fetch|push)\)( \[[^\t\n\f\r ]+\])?$`)

// ghRemoteLine is gh's own reading of a `git remote -v` line (remoteRE, cli/cli v2.98.0
// git/client.go:26): unanchored and greedy, so gh takes the URL before the line's last
// whitespace-preceded `(fetch)` or `(push)`, and trims the name and URL it captures.
var ghRemoteLine = regexp.MustCompile(`(.+)\s+(.+)\s+\((push|fetch)\)`)

// readGitRemotes is the working directory's git remotes as gh reads them, with git itself: every
// fetch and push URL `git remote -v` prints, each with its kind, a partial clone's fetch line
// included (git prints the remote's filter after it, `(fetch) [blob:none]`), and every gh-resolved
// line `git config --get-regexp` prints (ghMarkerLine); none for a directory that is no git
// repository, or where no git runs, since gh then reads none either. A URL line is read only when
// it has git's shape (gitRemoteLine, a filter on a fetch line alone) and gh's own reading of it
// (ghRemoteLine) names the same remote, URL and kind; any other line is refused, since gh could
// act on a repository this reading never counted. Both reads run under ctx (gitLines).
func readGitRemotes(ctx context.Context) ([]ghRemote, error) {
	lines, err := gitLines(ctx, "remote", "-v")
	if err != nil {
		return nil, err
	}
	var remotes []ghRemote
	for _, line := range lines {
		git := gitRemoteLine.FindStringSubmatch(line)
		gh := ghRemoteLine.FindStringSubmatch(line)
		if git == nil || (git[3] == "push" && git[4] != "") || gh == nil ||
			strings.TrimSpace(gh[1]) != git[1] || strings.TrimSpace(gh[2]) != git[2] || gh[3] != git[3] {
			// The line is named by its remote alone: its URL or filter holds whitespace, where the
			// URL rule ends a URL, so a credential holding a space would print past the rule.
			name, _, tabbed := strings.Cut(line, "\t")
			if !tabbed {
				return nil, errors.New("git remote -v printed a line with no tab, which legion gh cannot read as gh does; the line is not printed")
			}
			return nil, fmt.Errorf("git remote -v printed a line for the remote %q that legion gh cannot read as gh does (its URL or filter holds whitespace, or it has no git's shape); the line is not printed", name)
		}
		remotes = append(remotes, ghRemote{name: git[1], url: git[2], push: git[3] == "push"})
	}
	if lines == nil {
		return nil, nil
	}
	resolved, err := gitLines(ctx, "config", "--get-regexp", `^remote\..*\.gh-resolved$`)
	if err != nil {
		return nil, err
	}
	for _, line := range resolved {
		remotes = append(remotes, ghMarkerLine(line))
	}
	return remotes, nil
}

// gitLines runs git with args in the working directory and answers the lines it prints: none for
// a directory that is no git repository, where no git runs, or, for `git config --get-regexp`,
// where nothing matches. It runs with the environment gh runs with (ghChildEnviron), so it reads
// the repository, the remotes and the configuration gh's own git reads, and no GIT_DIR, global
// file or url.<base>.insteadOf of the caller's that gh's git never sees.
//
// It takes no lock: `git remote -v` and `git config --get-regexp` read configuration and write
// nothing, so neither can leave a lock file behind for the next reader of the repository. It runs
// under ctx, runGh's beforeGh, in a process group of its own that its bound or the end of ctx stops
// whole (procgroup.Configure): a git, or a wrapper of it, whose child holds the output pipe would
// otherwise keep the call waiting past the bound for as long as that child lives, since a stop of
// git alone leaves the pipe open, and the terminal's signals reach that group only through ctx
// (runGh's rule). A git that exits while a child still holds the pipe is waited for no longer than
// procgroup's WaitDelay, and its read is then refused, since a remote list cut short could route
// the call to another owner's directory.
func gitLines(ctx context.Context, args ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, programbound.Of(&ghGitReadBound))
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	procgroup.Configure(cmd)
	cmd.Env = append(ghChildEnviron(os.Environ(), nil), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil, fmt.Errorf("git %s exited, but something it started still held its output %s later, so what was read may be cut short", strings.Join(args, " "), procgroup.WaitDelayNow())
	}
	end := procgroup.Ended(err)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, nil
	case end.Ran() && strings.Contains(stderr.String(), "not a git repository"):
		return nil, nil
	case args[0] == "config" && end.ExitedWith(1):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	var lines []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	if lines == nil {
		lines = []string{}
	}
	return lines, nil
}

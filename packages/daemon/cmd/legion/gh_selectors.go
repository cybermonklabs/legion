package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

// selectors is every owner a command names, each with the argument that names it; every
// repository its selectors name, by name alone, as the argument names it (the command's owners
// are held to one, so a repository of another owner is a refusal and never a second key);
// whether a selector named an owner and no repository of it (ownerOnly), which leaves the command
// no one repository even where another selector names one; whether an argument names the
// command's own repository (targeted), so gh resolves none of its own; and whether gh api's
// arguments hold a placeholder gh fills from the command's own repository (readsBase).
type selectors struct {
	api       bool
	owners    map[string]string
	repos     map[string]bool
	ownerOnly bool
	targeted  bool
	readsBase bool
}

func (s *selectors) add(owner, what string) {
	if _, ok := s.owners[owner]; !ok {
		s.owners[owner] = what
	}
}

// repository records the owner and the repository value names: a selector naming one repository
// of one owner. The owner is lowercased, as a bundle's directory names an owner, and the
// repository is keyed as the directory holding its token is named for it
// (ghconfig.RepositoryName), so two spellings of one repository — `Legion-Smoke` in a pull
// request's URL beside `-R sjawhar/legion-smoke`, or a remote's `widgets.git` beside `widgets` —
// are the one repository the command names rather than two, which would send it to the owner's
// own directory.
func (s *selectors) repository(value, what string) error {
	owner, repo, err := repositoryTarget(value, what, s.api)
	if err == nil {
		s.add(owner, what)
		s.repos[ghconfig.RepositoryName(repo)] = true
	}
	return err
}

// ownerAlone records owner as named with no repository of it (ownerOnly), the one recording both
// owner and pairOwner make: the command reaches whatever of that owner's gh resolves, so its
// directory is the owner's own.
func (s *selectors) ownerAlone(owner, what string) {
	s.add(owner, what)
	s.ownerOnly = true
}

// owner records the owner value names and no repository.
func (s *selectors) owner(value, what string) error {
	owner, err := checkedOwner(value, what, s.api)
	if err == nil {
		s.ownerAlone(owner, what)
	}
	return err
}

// pairOwner records the owner of value, OWNER/TEAM or OWNER/NUMBER, and no repository: a team and
// a project are none of the owner's repositories. The pair is read as a repository's name only
// to take the owner out of it.
func (s *selectors) pairOwner(value, what string) error {
	owner, _, err := repositoryTarget(value, what, s.api)
	if err == nil {
		s.ownerAlone(owner, what)
	}
	return err
}

// onlyRepository is the one repository every selector of the command named, as the bundle's
// directory holding its token is named for it (ghconfig.RepositoryName, the key
// selectors.repository records): "" where a selector named the command's owner alone, where the
// selectors named two repositories, and where they named none. A command whose selectors agree
// on one repository is the one whose narrowest token the bundle may hold; every other reaches
// whatever else of the owner's gh resolves, and so the owner's own directory. The owner is its
// caller's, the one owner the command names (ghTargetOf), since a second owner is a refusal
// before this is asked.
func (s *selectors) onlyRepository() string {
	if s.ownerOnly || len(s.repos) != 1 {
		return ""
	}
	for name := range s.repos {
		return name
	}
	return ""
}

// ghRepositoryNumber is an issue or pull request named OWNER/REPO#N, which gh reads in a number's
// place as that repository's only for a leaf whose quirk says so; a pr leaf reads it as the name
// of a branch of its own repository.
var ghRepositoryNumber = regexp.MustCompile(`^([^/\s#]+/[^/\s#]+)#[0-9]+$`)

// ghPullRequestPath is the path of the one URL gh's pr finder reads as naming its pull request,
// and so its repository, `/OWNER/REPO/pull/N`; it reads any other URL as the name of a branch of
// the command's own repository.
var ghPullRequestPath = regexp.MustCompile(`^/[^/]+/[^/]+/pull/[0-9]+`)

// fromCommand collects the owners the leaf's arguments and options name, and whether one names
// the command's own repository: -R, a repository, owner or URL argument (through gh's pr finder,
// only a pull request's), OWNER/REPO#N where the leaf reads it, or its quirk's target option.
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
			return cannotPlace(parsed.positionals[len(cmd.slots)], "is an argument "+leaf+"'s usage has no place for", false)
		}
		var query []string
		for i, arg := range parsed.positionals {
			kind, unknown := placed[i].kind()
			if unknown != "" {
				return cannotPlace(arg, fmt.Sprintf("fills <%s> of %s's usage, an argument this does not know", unknown, leaf), false)
			}
			if placed[i].takes("number") && quirk.readsRepositoryNumber && ghRepositoryNumber.MatchString(arg) {
				if err := s.repository(ghRepositoryNumber.FindStringSubmatch(arg)[1], "the issue or pull request "+arg); err != nil {
					return err
				}
				s.targeted = true
				continue
			}
			switch kind {
			case "repository":
				if err := s.repository(arg, "the repository "+arg); err != nil {
					return err
				}
				s.targeted = true
			case "new-repository":
				// OWNER/NAME names the new repository's owner; a name alone, none.
				if strings.Contains(arg, "/") {
					if err := s.repository(arg, "the repository "+arg); err != nil {
						return err
					}
					s.targeted = true
				}
			case "second-repository":
				// Another repository the command acts on beside its own, which gh resolves
				// (fromOwnRepository).
				if err := s.repository(arg, "the repository "+arg); err != nil {
					return err
				}
			case "owner":
				if err := s.owner(arg, "the owner "+arg); err != nil {
					return err
				}
				s.targeted = true
			case "url":
				if isWebURL(arg) {
					if err := s.repository(arg, "the URL "+arg); err != nil {
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
// gh acts on another's repository names two owners, and is refused.
func (s *selectors) fromOwnRepository(cmd *ghCommand, parsed ghArgs, env map[string]string, remotes func() ([]ghRemote, error)) error {
	if quirkOf(cmd.path).readsAllRemotes {
		return s.fromWorkingDirectory(remotes, cmd.honoursDefaultMarker(parsed))
	}
	reads := !s.targeted && cmd.readsOwnRepository(len(parsed.positionals))
	if s.api {
		reads = s.readsBase
	}
	if !reads && len(s.owners) > 0 {
		return nil
	}
	if repo := env["GH_REPO"]; repo != "" && (cmd.readsGHRepo() || (s.api && s.readsBase)) {
		return s.repository(repo, "GH_REPO "+repo)
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
		return cannotPlace(parsed.positionals[1], "is an argument `gh api`'s usage has no place for", true)
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
				if err := s.owner(value, "the field "+field); err != nil {
					return err
				}
			case search && key == "q":
				if option == "field" && strings.HasPrefix(value, "@") {
					return cannotPlace(field, "reads the search query from a file", true)
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
				return cannotPlace("--"+name+" "+value, "is an option whose help names a repository, an owner, a team or a URL in a way this does not know", s.api)
			case "", "nothing":
				continue
			}
			for _, item := range strings.Split(value, ",") {
				what := "--" + name + " " + item
				var err error
				switch flag.selects {
				case "host":
					if !strings.EqualFold(item, "github.com") {
						err = cannotPlace(what, "names another host than github.com", s.api)
					}
				case "owner":
					err = s.owner(item, what)
				case "org-repositories":
					// gh reads a name alone as a repository of the --org, whose owner that is.
					switch {
					case strings.Contains(item, "/"):
						err = s.repository(item, what)
					case len(orgs) == 0:
						err = cannotPlace(what, "names a repository by its name alone, and no --org says whose", s.api)
					}
				case "repository":
					err = s.repository(item, what)
				case "owner/name":
					err = s.pairOwner(item, what)
				case "url":
					if isWebURL(item) {
						err = s.repository(item, what)
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
func (s *selectors) query(words []string) error {
	var named, joined string
	for _, word := range words {
		if !strings.Contains(word, ":") {
			if joined == "" && (strings.EqualFold(word, "OR") || strings.EqualFold(word, "NOT") || strings.ContainsAny(word, "()")) {
				joined = word
			}
			continue
		}
		qualifier := ghQualifier.FindStringSubmatch(word)
		if qualifier == nil {
			return cannotPlace(word, "is a search qualifier this cannot read for certain, since gh and GitHub read a quoted, spaced or grouped one otherwise", s.api)
		}
		switch strings.ToLower(qualifier[2]) {
		case "repo", "owner", "user", "org":
			if qualifier[1] == "-" {
				return cannotPlace(word, "excludes an owner, so the search's scope is no one owner's", s.api)
			}
			if strings.EqualFold(qualifier[2], "repo") {
				if err := s.repository(qualifier[3], "the qualifier "+word); err != nil {
					return err
				}
			} else if err := s.owner(qualifier[3], "the qualifier "+word); err != nil {
				return err
			}
			if named == "" {
				named = word
			}
		}
	}
	if named != "" && joined != "" {
		return cannotPlace(joined, "joins the query's repo:/owner: qualifiers with others, so the search's scope is no one owner's ("+named+" and more)", s.api)
	}
	return nil
}

// endpoint collects the owner a gh api endpoint names, and the repository where it names one:
// repos/<owner>/<repo>, repos/<owner>/, orgs/<owner>/ or users/<owner>/, with or without the
// API's host before it, and a search endpoint's q, reporting whether it is a search endpoint,
// whose q field is its query too. A placeholder in the owner's place names none (GH_REPO's, or
// the working directory's, fills it), and one in the repository's names the owner alone, since
// what fills it is read where gh reads it (fromOwnRepository); a repository named by id
// (repositories/<id>) and another host are errors. gh reads an endpoint holding "://" as a whole
// URL.
func (s *selectors) endpoint(endpoint string) (search bool, err error) {
	path := endpoint
	if strings.Contains(endpoint, "://") {
		parsed, err := url.Parse(endpoint)
		if err != nil || !strings.EqualFold(parsed.Host, "api.github.com") {
			return false, cannotPlace(endpoint, "names another host than api.github.com", true)
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
		return false, cannotPlace(endpoint, "names a repository by its id", true)
	case (first == "repos" || first == "orgs" || first == "users") && len(segments) > 1:
		if ghPlaceholder.MatchString(segments[1]) {
			return false, nil
		}
		if first == "repos" && len(segments) > 2 && segments[2] != "" && !ghPlaceholder.MatchString(segments[2]) {
			return false, s.repository(segments[1]+"/"+segments[2], "the endpoint "+endpoint)
		}
		return false, s.owner(segments[1], "the endpoint "+endpoint)
	case first == "search":
		query, err := url.ParseQuery(rawQuery)
		if err != nil {
			return true, cannotPlace(endpoint, "has a query string this cannot read", true)
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
// subdomains (www.github.com, ssh.github.com), in any case.
func isGitHubHost(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" || strings.HasSuffix(host, ".github.com")
}

// repositoryTarget is the lowercased owner and the name of a repository named as gh names one —
// OWNER/REPO, HOST/OWNER/REPO, or a github.com URL, its host github.com or a subdomain as gh
// normalizes it (isGitHubHost) — refused when it names no owner, another host, or an owner that
// is no GitHub login. The name is the second segment as gh reads it, whatever follows it (a pull
// request's number, a tree's path), as the argument spells it. The caller that records it keys
// it as the directory holding its token is named for it (selectors.repository,
// ghconfig.RepositoryName), so two spellings of one repository are one selector;
// the lookup normalizes it again, which changes nothing for a name GitHub holds, and decides
// whether a bundle can hold a directory of that name, since a name no directory can hold is one
// the bundle holds none for.
func repositoryTarget(value, what string, api bool) (owner, repo string, err error) {
	host, rest := "", value
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		host, rest = parsed.Hostname(), strings.Trim(parsed.Path, "/")
	} else if parts := strings.Split(strings.Trim(value, "/"), "/"); len(parts) >= 3 {
		host, rest = parts[0], strings.Join(parts[1:], "/")
	}
	if host != "" && !isGitHubHost(host) {
		return "", "", cannotPlace(value, "names another host than github.com", api)
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", cannotPlace(value, "names no owner", api)
	}
	owner, err = checkedOwner(parts[0], what, api)
	return owner, parts[1], err
}

// checkedOwner is owner lowercased, refused when it is no GitHub login.
func checkedOwner(owner, what string, api bool) (string, error) {
	lower := strings.ToLower(owner)
	if !ghconfig.IsOwner(lower) {
		return "", cannotPlace(what, fmt.Sprintf("names the owner %q, which is no GitHub login", owner), api)
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

// what is the remote line as a refusal names it.
func (r ghRemote) what() string {
	if r.marker {
		return fmt.Sprintf("remote.%s.gh-resolved %s", r.name, r.resolved)
	}
	return fmt.Sprintf("remote %q (%s)", r.name, r.url)
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
// "base", whose remote's own URL is read beside it. The name is carried on as the line spells it,
// `.git` and all, which the one gh directory lookup normalizes (repositoryGHConfigDir). why says
// why a line is one this cannot read: a URL gh's parser refuses, an ssh remote on another host,
// which gh may read as an alias of github.com from ssh's configuration, and a github.com one
// naming no OWNER/REPO whose owner is a GitHub login.
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
	case u.Scheme == "ssh" && !isGitHubHost(u.Hostname()):
		return "", "", "is an ssh remote whose host gh may read as github.com through ssh's configuration"
	case !isGitHubHost(u.Hostname()):
		return "", "", ""
	}
	parts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 3)
	if owner = strings.ToLower(parts[0]); len(parts) != 2 || parts[1] == "" || !ghconfig.IsOwner(owner) {
		return "", "", "names no OWNER/REPO whose owner is a GitHub login"
	}
	return owner, parts[1], ""
}

// isPlainGitHubURL says whether the URL line names a github.com repository gh reads with nothing
// the wrapper cannot read: gh's parser reads it (ghParseGitURL) as no ssh URL, whose host gh
// rewrites through ssh's own configuration (go-gh pkg/ssh/ssh.go), on a host gh's
// normalizeHostname (cli/cli internal/ghrepo/repo.go: a leading lower-case `www.` stripped, then
// lower-cased) makes github.com, the one host context/remote.go's FilterByHosts keeps, and it
// names an OWNER/REPO whose owner is a GitHub login (repository).
func (r ghRemote) isPlainGitHubURL() bool {
	u, err := ghParseGitURL(r.url)
	if err != nil || u.Scheme == "ssh" || strings.ToLower(strings.TrimPrefix(u.Hostname(), "www.")) != "github.com" {
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
// it resolves none for. Every line is read first, and one this cannot read is refused; a working
// directory that is no git repository names none. Where the command honours gh's default marker
// (honoursMarker) and that marker makes origin's repository gh's own (markedOrigin), origin's
// fetch line alone counts. Otherwise every line counts, since gh may act on any: the first of its
// sorted remotes, an `upstream` before an `origin`, the repository a marker names, or, for repo
// set-default, every remote's network. So a workspace whose remotes name two owners names two —
// as one whose remotes name two repositories of one owner names two repositories, and so no one
// repository. Two lines spelling the one repository differently, a URL's `repo.git` beside a
// gh-resolved `owner/repo`, are two names here too, which leaves the command the owner's own
// directory: the wider of the two tokens, and never another repository's.
func (s *selectors) fromWorkingDirectory(remotes func() ([]ghRemote, error), honoursMarker bool) error {
	advice := "name the repository with -R <owner>/<repo>"
	if s.api {
		advice = "name it in the endpoint, repos/<owner>/<repo>/…"
	}
	listed, err := remotes()
	if err != nil {
		return fmt.Errorf("cannot tell which repository the command targets: the working directory's git remotes, from which gh takes the command's own repository, cannot be read (%v), so it sends no token; %s", err, advice)
	}
	for _, remote := range listed {
		if _, _, why := remote.repository(); why != "" {
			return fmt.Errorf("cannot tell which repository the command targets: the working directory's %s %s, so it sends no token; %s", remote.what(), why, advice)
		}
	}
	if honoursMarker {
		if fetch, ok := markedOrigin(listed); ok {
			listed = listed[fetch : fetch+1]
		}
	}
	for _, remote := range listed {
		if owner, repo, _ := remote.repository(); owner != "" {
			s.add(owner, "the working directory's "+remote.what())
			s.repos[ghconfig.RepositoryName(repo)] = true
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
// act on a repository this reading never counted.
func readGitRemotes() ([]ghRemote, error) {
	lines, err := gitLines("remote", "-v")
	if err != nil {
		return nil, err
	}
	var remotes []ghRemote
	for _, line := range lines {
		git := gitRemoteLine.FindStringSubmatch(line)
		gh := ghRemoteLine.FindStringSubmatch(line)
		if git == nil || (git[3] == "push" && git[4] != "") || gh == nil ||
			strings.TrimSpace(gh[1]) != git[1] || strings.TrimSpace(gh[2]) != git[2] || gh[3] != git[3] {
			return nil, fmt.Errorf("git remote -v printed %q", line)
		}
		remotes = append(remotes, ghRemote{name: git[1], url: git[2], push: git[3] == "push"})
	}
	if lines == nil {
		return nil, nil
	}
	resolved, err := gitLines("config", "--get-regexp", `^remote\..*\.gh-resolved$`)
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
// where nothing matches.
//
// It takes no lock: `git remote -v` and `git config --get-regexp` read configuration and write
// nothing, so neither can leave a lock file behind for the next reader of the repository. R8 of
// dispatch://INFRA-3098 gives a process group of its own and the SIGTERM-then-grace-then-SIGKILL
// stop to the two command runners of internal/workspace, whose git and jj do take locks, and
// leaves a read that takes none as it is: this child needs neither the group nor the grace.
func gitLines(args ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, nil
	case errors.As(err, &exit) && strings.Contains(stderr.String(), "not a git repository"):
		return nil, nil
	case errors.As(err, &exit) && args[0] == "config" && exit.ExitCode() == 1:
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

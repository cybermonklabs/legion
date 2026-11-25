package replay

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// keptRepository is the one repository a neutralized window names as the record named it: this
// repository, which is public, and whose name a reader of a replay needs to read its commits.
const keptRepository = "cybermonklabs/legion"

// NeutralizeConfig is one neutralization of a recorded window (Neutralize).
type NeutralizeConfig struct {
	// In is the window's directory, and Out the directory the neutral window is written to, which
	// must be missing or empty.
	In, Out string
	// Project is the Dispatch project key the window's own project becomes (ACME for the
	// repository's test data). Every other project a window names becomes OTHERA, OTHERB, ….
	Project string
	// Map is the file the map of every name replaced is written to (NeutralMap). It holds every
	// private name of the window, so it must be outside Out, and a file already there is refused
	// rather than written over.
	Map string
	// Deny is words no file of the neutral window may hold in any case, a private name no rule of
	// the neutralizer knows to be one (an organization's): each is replaced with "withheld"
	// wherever it stands in a value, and one left anywhere, a member's name included, fails the
	// neutralization. A word that is part of "withheld" is refused, since its replacement would
	// hold it again.
	Deny []string
}

// NeutralMap is every name a neutralization replaced, by what it names, each from the record's
// name to its neutral one: what turns a neutral window's names back into the record's, which is
// why it is written outside the neutral window and never committed. Addresses holds each network
// address by its canonical form (netip.Addr's String).
type NeutralMap struct {
	Projects  map[string]string `json:"projects"`
	Keys      map[string]string `json:"keys"`
	Repos     map[string]string `json:"repos"`
	People    map[string]string `json:"people"`
	Sessions  map[string]string `json:"sessions"`
	Titles    map[string]string `json:"titles"`
	Machines  map[string]string `json:"machines"`
	Hosts     map[string]string `json:"hosts"`
	Addresses map[string]string `json:"addresses"`
	Paths     map[string]string `json:"paths"`
	IDs       map[string]string `json:"ids"`
	Digests   map[string]string `json:"digests"`
	Links     map[string]string `json:"links"`
	Names     map[string]string `json:"names"`
	Text      map[string]string `json:"text"`
}

// Neutralize writes the window in cfg.In to cfg.Out with every private name in it renamed, the
// same name to the same neutral name in every file: each issue key of every project (in the
// order of its number within its project) and each claim token, workspace and daemon's name that
// spells one; each repository but this one; each actor's and session's id, each session's title,
// machine and host; each IPv4 and IPv6 address, a name of the deployment's network, to one of the
// documentation ranges (RFC 5737's 192.0.2.0/24, then its 198.51.100.0/24 and 203.0.113.0/24;
// RFC 3849's 2001:db8::/32), keeping the loopback and unspecified addresses and every address
// already in a documentation range; each person's login and email; each uuid and document
// digest; each link outside this repository; each label but Legion's, component and priority;
// and the free text of Dispatch's events (titles, summaries, reasons, questions, answers, bodies)
// to numbered neutral text. The inputs of the signals are renamed by the same rules: a pod's name
// and a pull request's branch have their keys and the logins of the window's people renamed
// (renameLogins), the launcher's lines and the pods' memory the names their shapes show, the
// merge queue's events their GitHub accounts (login, numeric id, node id and links), the
// repository of their file and the repository their url names, the url keeping its form, the
// project's settings events as Dispatch's events (a token's name and a source's last error among
// them), and the default assignee as a person. It changes values alone: every member, JSON type
// and list length of every line is the record's, so the neutral
// window is read by the same readers. The daemon's msgs, document names, ranks, statuses and times
// are kept, and so are the daemon's own words in its lines, their names renamed. It writes the map
// to cfg.Map and returns it. It refuses a map
// inside cfg.Out or already there, an Out that is In or holds files, a deny word that is part of
// withheld, the word every deny word is replaced with, whose replacement would put it back, a
// window holding more IPv4 addresses than the documentation ranges hold, and a neutral window that
// still holds a deny word or a key of a project it renamed.
func Neutralize(cfg NeutralizeConfig) (NeutralMap, error) {
	if !claim.IsIssueKey(cfg.Project + "-1") {
		return NeutralMap{}, fmt.Errorf("replay: neutralize: %q is no Dispatch project key", cfg.Project)
	}
	if err := checkNeutralPaths(cfg); err != nil {
		return NeutralMap{}, err
	}
	n, err := newNeutralizer(cfg)
	if err != nil {
		return NeutralMap{}, err
	}
	w, err := ReadWindow(cfg.In)
	if err != nil {
		return NeutralMap{}, fmt.Errorf("replay: neutralize: %w", err)
	}
	parsed, err := parseWindow(w)
	if err != nil {
		return NeutralMap{}, fmt.Errorf("replay: neutralize: %w", err)
	}
	parsed.visit(func(v any, from source) { n.walk(v, field{source: from}, false) })
	for _, repo := range slices.Sorted(maps.Keys(w.Merges)) {
		n.addRepo(repo)
		for _, merge := range w.Merges[repo] {
			n.register(merge.Branch, field{source: sourceMerges, key: "branch"})
		}
	}
	for _, repo := range slices.Sorted(maps.Keys(w.Queue)) {
		n.addRepo(repo)
	}
	n.number(w.Project)

	out := w
	header, err := n.rewrite(parsed.header, sourceWindow)
	if err != nil {
		return NeutralMap{}, err
	}
	if err := json.Unmarshal(header, &out); err != nil {
		return NeutralMap{}, fmt.Errorf("replay: neutralize: read the neutral window.json back: %w", err)
	}
	if out.Log, err = n.rewriteAll(parsed.log, sourceLog); err != nil {
		return NeutralMap{}, err
	}
	// Each input is rewritten in one order (parsedWindow.visit's), so each kind of name is
	// numbered the same on every run.
	out.Events = make(map[string][]json.RawMessage, len(parsed.events))
	for _, key := range slices.Sorted(maps.Keys(parsed.events)) {
		if out.Events[n.key(key)], err = n.rewriteAll(parsed.events[key], sourceEvent); err != nil {
			return NeutralMap{}, err
		}
	}
	out.Merges = make(map[string][]Merge, len(w.Merges))
	for _, repo := range slices.Sorted(maps.Keys(w.Merges)) {
		renamed := make([]Merge, len(w.Merges[repo]))
		for i, merge := range w.Merges[repo] {
			merge.Branch = n.visit(merge.Branch, field{source: sourceMerges, key: "branch"})
			renamed[i] = merge
		}
		out.Merges[n.repo(repo)] = renamed
	}
	// An input of the signals the window does not hold stays missing (out holds w's nil).
	if w.Settings != nil {
		if out.Settings, err = n.rewriteAll(parsed.settings, sourceEvent); err != nil {
			return NeutralMap{}, err
		}
	}
	if w.Launcher != nil {
		if out.Launcher, err = rewriteTyped[LauncherLine](n, parsed.launcher); err != nil {
			return NeutralMap{}, err
		}
	}
	if w.Memory != nil {
		if out.Memory, err = rewriteTyped[MemorySample](n, parsed.memory); err != nil {
			return NeutralMap{}, err
		}
	}
	if w.Provider != nil {
		if out.Provider, err = rewriteTyped[ProviderMinute](n, parsed.provider); err != nil {
			return NeutralMap{}, err
		}
	}
	if w.Queue != nil {
		out.Queue = make(map[string][]json.RawMessage, len(w.Queue))
		for _, repo := range slices.Sorted(maps.Keys(parsed.queue)) {
			if out.Queue[n.repo(repo)], err = n.rewriteAll(parsed.queue[repo], sourceQueue); err != nil {
				return NeutralMap{}, err
			}
		}
	}
	if n.err != nil {
		return NeutralMap{}, n.err
	}
	_, statErr := os.Stat(cfg.Out)
	if err := out.Write(cfg.Out); err != nil {
		return NeutralMap{}, fmt.Errorf("replay: neutralize: %w", err)
	}
	if err := n.checkWritten(cfg.Out); err != nil {
		// The output was missing or empty: what is in it now is this run's alone, and a neutral
		// window that still holds a private name is left for no one to commit.
		if errors.Is(statErr, fs.ErrNotExist) {
			os.RemoveAll(cfg.Out)
		} else if entries, readErr := os.ReadDir(cfg.Out); readErr == nil {
			for _, entry := range entries {
				os.RemoveAll(filepath.Join(cfg.Out, entry.Name()))
			}
		}
		return NeutralMap{}, err
	}
	if err := writeNeutralMap(cfg.Map, n.m); err != nil {
		return NeutralMap{}, err
	}
	return n.m, nil
}

// checkNeutralPaths refuses the paths of a neutralization that would put the map in reach of the
// neutral window, write over a map, or write the neutral window over the record or beside other
// files: a map inside Out or already there, an Out that is In or holds files. Each path is read
// with the links on its way resolved, so a link does not carry the map inside.
func checkNeutralPaths(cfg NeutralizeConfig) error {
	if cfg.Map == "" || cfg.Out == "" || cfg.In == "" {
		return errors.New("replay: neutralize: the input, the output and the map must each be named")
	}
	in, out, mapFile := resolvedPath(cfg.In), resolvedPath(cfg.Out), resolvedPath(cfg.Map)
	if within(out, mapFile) {
		return fmt.Errorf("replay: neutralize: the map %s is inside the output %s, which would publish every name it hides; write it outside", cfg.Map, cfg.Out)
	}
	if _, err := os.Lstat(cfg.Map); err == nil {
		return fmt.Errorf("replay: neutralize: the map %s is there already; neutralize writes a new one and never over one", cfg.Map)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("replay: neutralize: %w", err)
	}
	if in == out {
		return fmt.Errorf("replay: neutralize: the output %s is the input", cfg.Out)
	}
	return emptyDir(cfg.Out)
}

// resolvedPath is path made absolute with the links of its longest existing part resolved: the
// path a later write of it reaches.
func resolvedPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	rest := ""
	for dir := abs; ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return abs
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

// within says whether path is dir or under it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}

// writeNeutralMap writes m to file, readable by its owner alone, refusing a file that is there.
func writeNeutralMap(file string, m NeutralMap) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("replay: neutralize: %w", err)
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("replay: neutralize: write the map: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("replay: neutralize: write the map: %w", err)
	}
	return f.Close()
}

// source is which input of a window a value is read from, which decides what some of its members
// name: a reason in a Dispatch event is a person's text, in a daemon line the daemon's words. A
// settings event is a Dispatch event, and is read as one.
type source int

const (
	sourceWindow source = iota
	sourceLog
	sourceEvent
	sourceMerges
	// sourceRecord is the signals' records whose words are their writers' own: the launcher's
	// lines, the pods' memory and the provider's minutes, every name in which is found by its
	// shape, as in the daemon's lines.
	sourceRecord
	// sourceQueue is the merge queue's events as GitHub serves them, whose actors are GitHub's
	// accounts.
	sourceQueue
)

// field is where a string stands in a line: its input, the name of the member it is the value of
// (or the list it is an element of), the name of the member holding that member's object, and the
// kind member of that object where it has one (an actor's: user, session, system).
type field struct {
	source source
	key    string
	parent string
	kind   string
}

// parsedWindow is a window's inputs and window.json as ordered JSON values (decodeOrdered), each
// in the order the neutralizer reads them, so the numbering of names is the same on every run.
type parsedWindow struct {
	header                     any
	log                        []any
	events                     map[string][]any
	settings                   []any
	launcher, memory, provider []any
	queue                      map[string][]any
}

// parseWindow is w as ordered JSON values. window.json is w's own fields as Write writes them, and
// each input of the signals the window holds its lines as Write writes them.
func parseWindow(w Window) (parsedWindow, error) {
	header, err := json.Marshal(w)
	if err != nil {
		return parsedWindow{}, err
	}
	p := parsedWindow{events: make(map[string][]any, len(w.Events)), queue: make(map[string][]any, len(w.Queue))}
	if p.header, err = decodeOrdered(header); err != nil {
		return parsedWindow{}, err
	}
	if p.log, err = decodeAll(w.Log, "log line"); err != nil {
		return parsedWindow{}, err
	}
	for key, events := range w.Events {
		if p.events[key], err = decodeAll(events, "event of "+key); err != nil {
			return parsedWindow{}, err
		}
	}
	if p.settings, err = decodeAll(w.Settings, "settings event"); err != nil {
		return parsedWindow{}, err
	}
	for _, input := range []struct {
		into  *[]any
		what  string
		lines func() ([]json.RawMessage, error)
	}{
		{&p.launcher, "launcher line", func() ([]json.RawMessage, error) { return marshalLines(w.Launcher) }},
		{&p.memory, "memory sample", func() ([]json.RawMessage, error) { return marshalLines(w.Memory) }},
		{&p.provider, "provider minute", func() ([]json.RawMessage, error) { return marshalLines(w.Provider) }},
	} {
		lines, err := input.lines()
		if err == nil {
			*input.into, err = decodeAll(lines, input.what)
		}
		if err != nil {
			return parsedWindow{}, err
		}
	}
	for repo, lines := range w.Queue {
		if p.queue[repo], err = decodeAll(lines, "queue line of "+repo); err != nil {
			return parsedWindow{}, err
		}
	}
	return p, nil
}

// decodeAll is each of lines as an ordered JSON value, a line that does not read named as the
// what of its place.
func decodeAll(lines []json.RawMessage, what string) ([]any, error) {
	values := make([]any, 0, len(lines))
	for i, line := range lines {
		value, err := decodeOrdered(line)
		if err != nil {
			return nil, fmt.Errorf("%s %d: %w", what, i+1, err)
		}
		values = append(values, value)
	}
	return values, nil
}

// visit hands each value of p to do with its input, window.json first, then the log in its order,
// then each issue's events by key, then the settings events, the launcher's lines, the pods'
// memory and the provider's minutes, each in its order, and each repository's queue by repository.
func (p parsedWindow) visit(do func(v any, from source)) {
	do(p.header, sourceWindow)
	for _, line := range p.log {
		do(line, sourceLog)
	}
	for _, key := range slices.Sorted(maps.Keys(p.events)) {
		do(key, sourceEvent)
		for _, event := range p.events[key] {
			do(event, sourceEvent)
		}
	}
	for _, event := range p.settings {
		do(event, sourceEvent)
	}
	for _, line := range slices.Concat(p.launcher, p.memory, p.provider) {
		do(line, sourceRecord)
	}
	for _, repo := range slices.Sorted(maps.Keys(p.queue)) {
		for _, line := range p.queue[repo] {
			do(line, sourceQueue)
		}
	}
}

// rewriteTyped is values, lines of one of the signals' records (sourceRecord), rewritten and read
// back as T.
func rewriteTyped[T any](n *neutralizer, values []any) ([]T, error) {
	lines, err := n.rewriteAll(values, sourceRecord)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(lines))
	for i, line := range lines {
		var value T
		if err := json.Unmarshal(line, &value); err != nil {
			return nil, fmt.Errorf("replay: neutralize: read the neutral line %d back: %w", i+1, err)
		}
		out = append(out, value)
	}
	return out, nil
}

// neutralizer renames the names of one window (Neutralize): a first walk over every value
// registers the projects, issue keys and repositories (walk with rewrite false), number gives each
// key its neutral one, and a second walk rewrites every string (visit), naming every other kind
// of name in the order it meets them.
type neutralizer struct {
	project string
	deny    []*regexp.Regexp
	m       NeutralMap

	// sourceProjects is every project of the window in the order first met, and keys every issue
	// key of each.
	sourceProjects []string
	keys           map[string]map[int]bool
	// repos is every repository the window names but keptRepository, in the order first met.
	repos []string
	// values is every string the first walk met outside a person's text, which collectKeys reads
	// for keys and collectAddresses for documentation addresses once every project is registered.
	values []string
	// logins is every person's login the first walk met (a Dispatch login that is no email, a
	// GitHub account's), which a pod's name or a pull request's branch can spell (renameLogins).
	logins map[string]bool
	// names is the matcher of every name a string can hold anywhere (rename), built by number.
	names *regexp.Regexp
	// counters is the next number of each kind of neutral name.
	counters map[string]int
	// documented is each address of a documentation range the window holds, which it keeps and so
	// no renamed address may become; err is the first address the documentation ranges had no
	// neutral address left for, which refuses the neutralization.
	documented map[netip.Addr]bool
	err        error
}

// withheld is what every deny word is replaced with wherever it stands in a value (visit).
const withheld = "withheld"

// newNeutralizer is the neutralizer of cfg, before any name is registered. It refuses a deny word
// that withheld holds, in any case: such a word's replacement would hold the word again, and the
// check of the bytes written (checkWritten) would refuse every window that holds it.
func newNeutralizer(cfg NeutralizeConfig) (*neutralizer, error) {
	n := &neutralizer{project: cfg.Project, keys: make(map[string]map[int]bool), logins: make(map[string]bool), counters: make(map[string]int),
		documented: make(map[netip.Addr]bool), m: NeutralMap{
			Projects: map[string]string{}, Keys: map[string]string{}, Repos: map[string]string{}, People: map[string]string{},
			Sessions: map[string]string{}, Titles: map[string]string{}, Machines: map[string]string{}, Hosts: map[string]string{},
			Addresses: map[string]string{}, Paths: map[string]string{}, IDs: map[string]string{}, Digests: map[string]string{},
			Links: map[string]string{}, Names: map[string]string{}, Text: map[string]string{},
		}}
	for _, word := range cfg.Deny {
		if word = strings.TrimSpace(word); word != "" {
			deny := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(word))
			if deny.MatchString(withheld) {
				return nil, fmt.Errorf("replay: neutralize: the deny word %q is part of %q, the word every deny word is replaced with, so its replacement would hold it again", word, withheld)
			}
			n.deny = append(n.deny, deny)
		}
	}
	return n, nil
}

// uuidShape is a uuid's shape, which a string that is one uuid (wholeUUID) and a uuid standing in
// a string (namesPattern) both take.
const uuidShape = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

// The patterns of the names the neutralizer finds by their shape.
var (
	// anyKey is an issue key standing anywhere in a string, of any project, in either case; a string
	// that is one key is one claim.IsIssueKey takes.
	anyKey = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9]*)-([0-9]+)\b`)
	// wholeUUID is a string that is one uuid.
	wholeUUID = regexp.MustCompile(`^` + uuidShape + `$`)
	// daemonMachine is the machine name of a deployment's daemon, legion-<project token>-daemon. A
	// Legion role's session title is legionTitle (trace.go), which the trace reads its claim from.
	daemonMachine = regexp.MustCompile(`^legion-([a-z0-9]+)-daemon$`)
	// githubRepoURL is a link to a repository on GitHub, its owner/name and the rest of its path.
	githubRepoURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(.*)$`)
	// githubAPIRepoURL is a link to a repository in GitHub's REST API, as the url of each merge
	// queue event is (ParseQueueLine), its owner/name and the rest of its path.
	githubAPIRepoURL = regexp.MustCompile(`^https://api\.github\.com/repos/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(.*)$`)
	// workspacePath is a Legion workspace on a pod, /legion/workspaces/<owner>/<name>/<issue>….
	workspacePath = regexp.MustCompile(`^/legion/workspaces/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(.*)$`)
	// anyAddress is a network address standing anywhere in a string (addressShape).
	anyAddress = regexp.MustCompile(addressShape)
)

// addressShape is the shape of a network address in a string: an IPv4 address in dotted decimal,
// an IPv6 address that ends in an IPv4 one, or an IPv6 address written whole or with a run of
// zero groups cut to "::". It has no capturing group, so namesPattern holds it as one of its
// alternatives. A match is an address only where netip reads it as one (neutralizer.address).
const addressShape = `\b(?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\b` +
	`|(?:[0-9A-Fa-f]{0,4}:){2,6}(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b` +
	`|\b(?:[0-9A-Fa-f]{1,4}:){7}[0-9A-Fa-f]{1,4}\b` +
	`|(?:\b[0-9A-Fa-f]{1,4}(?::[0-9A-Fa-f]{1,4}){0,6})?::(?:[0-9A-Fa-f]{1,4}(?::[0-9A-Fa-f]{1,4}){0,6}\b)?`

// ipv4Documentation and ipv6Documentation are the address ranges set aside for documentation,
// RFC 5737's three and RFC 3849's one: an address in one names no deployment's network, so a
// neutral window keeps it, and every neutral address is drawn from them, the IPv4 ones from the
// three in turn.
var (
	ipv4Documentation = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24")}
	ipv6Documentation = netip.MustParsePrefix("2001:db8::/32")
)

// Field rules: how the string value of a member is renamed, by the member's name. A member no
// rule names is renamed by rename, which finds every name a string holds by its shape.
var (
	// keptMembers are members whose value is a word of the daemon's or of Dispatch's, which a
	// replay reads as written, or a value with no name in it: never renamed.
	keptMembers = map[string]bool{"msg": true, "level": true, "time": true, "type": true, "name": true, "slug": true,
		"rank": true, "changed": true, "lines": true, "rule": true, "commit": true, "collector": true, "minute": true}
	// freeTextMembers are members of a Dispatch event whose value is text a person or an agent
	// wrote, or Dispatch's own text that quotes their names (a settings source's last error):
	// each value becomes numbered neutral text.
	freeTextMembers = map[string]bool{"title": true, "summary": true, "reason": true, "question": true, "text": true,
		"body": true, "description": true, "label": true, "selected": true, "note": true, "message": true, "comment": true,
		"last_error": true}
	// personMembers are members of a Dispatch event whose value is a person's login or email.
	personMembers = map[string]bool{"assignee": true, "user": true, "login": true, "thread_logins": true, "asked_of": true,
		"asked_of_by": true}
	// repoMembers are members whose value is a repository as owner/name: an issue's, the one
	// Dispatch resolved for it, and the one a repository's mapping was renamed to.
	repoMembers = map[string]bool{"repo": true, "resolved_repo": true, "renamed_to": true}
	// spelledMembers are members whose value can spell a person's login and an issue key: a pod's
	// name and a pull request's branch (renameLogins).
	spelledMembers = map[string]bool{"pod": true, "branch": true, "head_ref": true, "base_ref": true}
)

// walk reads v under f: registering its projects, keys and repositories where rewrite is false,
// and answering v with every string renamed (visit) where it is true; a reading that registers
// answers v itself. An object's members are walked in order, each under its own name; a list's
// elements under the list's.
func (n *neutralizer) walk(v any, f field, rewrite bool) any {
	switch value := v.(type) {
	case jsonObject:
		kind := ""
		for _, member := range value {
			if member.key == "kind" {
				kind, _ = member.value.(string)
			}
		}
		if !rewrite {
			for _, member := range value {
				n.walk(member.value, field{source: f.source, key: member.key, parent: f.key, kind: kind}, false)
			}
			return v
		}
		out := make(jsonObject, len(value))
		for i, member := range value {
			out[i] = jsonMember{key: member.key, value: n.walk(member.value, field{source: f.source, key: member.key, parent: f.key, kind: kind}, true)}
		}
		if f.source == sourceEvent {
			numberFollowsKey(out)
		}
		return out
	case []any:
		if !rewrite {
			for _, element := range value {
				n.walk(element, f, false)
			}
			return v
		}
		out := make([]any, len(value))
		for i, element := range value {
			out[i] = n.walk(element, f, true)
		}
		return out
	case string:
		if rewrite {
			return n.visit(value, f)
		}
		n.register(value, f)
	case json.Number:
		// A GitHub account's numeric id names the account as its login does.
		if rewrite && f.source == sourceQueue && f.parent == "actor" && f.key == "id" {
			return json.Number(n.named(n.m.IDs, "github-id", "github:"+value.String(), func(i int) string { return strconv.Itoa(i) }))
		}
	}
	return v
}

// numberFollowsKey sets an issue's number to its neutral key's, where object is an issue that
// carries both (Dispatch's issue payload).
func numberFollowsKey(object jsonObject) {
	var key string
	for _, member := range object {
		if member.key == "key" {
			key, _ = member.value.(string)
		}
	}
	if !claim.IsIssueKey(key) {
		return
	}
	_, number, _ := strings.Cut(key, "-")
	for i, member := range object {
		if _, isNumber := member.value.(json.Number); member.key == "number" && isNumber {
			object[i].value = json.Number(number)
		}
	}
}

// register takes from s, standing at f, the projects, keys, repositories and logins the first walk
// registers: a project member's value, a value that is one issue key, a repository member's value,
// the repository of a workspace or of a link to GitHub or to its REST API, the window's events'
// keys, and a person's login that is no email, in a Dispatch event or as a GitHub account's login
// in the merge queue.
func (n *neutralizer) register(s string, f field) {
	switch {
	case f.key == "project":
		n.addProject(s)
	case f.source == sourceEvent && freeTextMembers[f.key]:
		return
	case repoMembers[f.key]:
		n.addRepo(s)
	case !strings.Contains(s, "@") && ((f.source == sourceEvent && (personMembers[f.key] || (f.key == "id" && f.kind == "user"))) ||
		(f.source == sourceQueue && f.key == "login")):
		n.logins[s] = true
	}
	if match := workspacePath.FindStringSubmatch(s); match != nil {
		n.addRepo(match[1])
	}
	if match := githubRepoURL.FindStringSubmatch(s); match != nil {
		n.addRepo(match[1])
	}
	if match := githubAPIRepoURL.FindStringSubmatch(s); match != nil {
		n.addRepo(match[1])
	}
	if claim.IsIssueKey(s) {
		project, _, _ := strings.Cut(s, "-")
		n.addProject(project)
	}
	n.values = append(n.values, s)
}

// addProject registers project, once, in the order first met.
func (n *neutralizer) addProject(project string) {
	if _, known := n.keys[project]; !known && claim.IsIssueKey(project+"-1") {
		n.keys[project] = make(map[int]bool)
		n.sourceProjects = append(n.sourceProjects, project)
	}
}

// addRepo registers repo, an owner/name other than keptRepository, once.
func (n *neutralizer) addRepo(repo string) {
	owner, name, ok := strings.Cut(repo, "/")
	if ok && owner != "" && name != "" && !strings.Contains(name, "/") && repo != keptRepository && !slices.Contains(n.repos, repo) {
		n.repos = append(n.repos, repo)
	}
}

// number gives every project and key its neutral name, once the first walk has registered them
// and the keys of those projects that stand anywhere in the window are found: the window's own
// project the configured one and each other OTHERA, OTHERB, … in the order met; each project's
// keys 1, 2, … in the order of their numbers, so the neutral keys sort as the record's did within
// a project. It finds the documentation addresses the window holds, which no neutral address may
// be, and builds the matcher rename finds names with.
func (n *neutralizer) number(own string) {
	n.addProject(own)
	n.collectKeys()
	n.collectAddresses()
	n.values = nil
	others := 0
	for _, project := range n.sourceProjects {
		if project == own {
			n.m.Projects[project] = n.project
			continue
		}
		others++
		n.m.Projects[project] = "OTHER" + letters(others)
	}
	for _, project := range n.sourceProjects {
		numbers := slices.Sorted(maps.Keys(n.keys[project]))
		for i, number := range numbers {
			n.m.Keys[fmt.Sprintf("%s-%d", project, number)] = fmt.Sprintf("%s-%d", n.m.Projects[project], i+1)
		}
	}
	for i, repo := range n.repos {
		n.m.Repos[repo] = fmt.Sprintf("acme/repo-%d", i+1)
	}
	n.m.Repos[keptRepository] = keptRepository
	n.names = n.namesPattern()
}

// collectKeys registers every key of a registered project that stands in a string anywhere in
// the window, in either case, so a key no member holds whole (in a claim token, a path, a title)
// is numbered with the rest. A project's keys are only those of the projects registered: a word
// such as UTF-8 is no key.
func (n *neutralizer) collectKeys() {
	for _, s := range n.values {
		for _, match := range anyKey.FindAllStringSubmatch(s, -1) {
			project := strings.ToUpper(match[1])
			if keys, known := n.keys[project]; known && (match[1] == project || match[1] == strings.ToLower(project)) {
				if number, err := strconv.Atoi(match[2]); err == nil {
					keys[number] = true
				}
			}
		}
	}
}

// collectAddresses registers every address of a documentation range that stands in a string
// anywhere in the window: the record's own, which the neutral window keeps, so no address renamed
// becomes one and two names stay two.
func (n *neutralizer) collectAddresses() {
	for _, s := range n.values {
		for _, match := range anyAddress.FindAllString(s, -1) {
			if addr, err := netip.ParseAddr(match); err == nil && documentationAddress(addr.Unmap()) {
				n.documented[addr.Unmap()] = true
			}
		}
	}
}

// letters is the n-th name of the spreadsheet column kind: A, B, …, Z, AA, AB, ….
func letters(n int) string {
	var name []byte
	for ; n > 0; n = (n - 1) / 26 {
		name = append([]byte{byte('A' + (n-1)%26)}, name...)
	}
	return string(name)
}

// namesPattern is the matcher of every name rename finds in a string by its shape, as one
// alternation so a string is read once, left to right, and a name already renamed is never read
// again: a link, an email, a uuid, a network address, a registered repository, a host name of
// three labels or more, the daemon's Dispatch session of a registered project, a claim token's or
// a machine's legion-<project token>-, a key of a registered project in either case, and a
// registered project's own key standing alone.
func (n *neutralizer) namesPattern() *regexp.Regexp {
	var repos, upper, lower []string
	for _, repo := range slices.SortedFunc(slices.Values(n.repos), func(a, b string) int { return len(b) - len(a) }) {
		repos = append(repos, regexp.QuoteMeta(repo))
	}
	for _, project := range n.sourceProjects {
		upper = append(upper, regexp.QuoteMeta(project))
		lower = append(lower, regexp.QuoteMeta(strings.ToLower(project)))
	}
	alternatives := []string{
		`(?P<link>https?://[^\s"'<>(){}\[\]]+)`,
		`(?P<email>[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,})`,
		`(?P<uuid>` + uuidShape + `)`,
		`(?P<address>` + addressShape + `)`,
	}
	if len(repos) > 0 {
		alternatives = append(alternatives, `(?P<repo>\b(?:`+strings.Join(repos, "|")+`)\b)`)
	}
	alternatives = append(alternatives, `(?P<host>\b(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.){2,}[a-z]{2,}\b)`)
	if len(upper) > 0 {
		alternatives = append(alternatives,
			`(?P<daemon>legion-daemon:(?:`+strings.Join(upper, "|")+`)\b)`,
			`(?P<token>\blegion-(?:`+strings.Join(lower, "|")+`)-)`,
			`(?P<key>\b(?:`+strings.Join(upper, "|")+`|`+strings.Join(lower, "|")+`)-[0-9]+\b)`,
			`(?P<project>\b(?:`+strings.Join(upper, "|")+`)\b)`)
	}
	return regexp.MustCompile(strings.Join(alternatives, "|"))
}

// visit is s, standing at f, with its names renamed: by the rule of f's member where one names
// it, and otherwise by rename; then every deny word in it replaced. A kept member's value is the
// daemon's or Dispatch's own word and is never changed, a deny word in it included, which
// checkWritten then refuses.
func (n *neutralizer) visit(s string, f field) string {
	if s == "" || keptMembers[f.key] {
		return s
	}
	renamed := n.byMember(s, f)
	for _, word := range n.deny {
		renamed = word.ReplaceAllString(renamed, withheld)
	}
	return renamed
}

// byMember renames s by the rule of the member it is the value of (the field rules above).
func (n *neutralizer) byMember(s string, f field) string {
	switch {
	case f.key == "project":
		return n.projectName(s)
	case spelledMembers[f.key]:
		return n.renameLogins(n.rename(s))
	case f.source == sourceLog:
		return n.rename(s)
	case f.source == sourceWindow && f.key == "defaultAssignee":
		return n.person(s)
	case f.source == sourceQueue && f.key == "login":
		return n.person(s)
	case f.source == sourceQueue && f.parent == "actor" && f.key == "node_id":
		return n.named(n.m.IDs, "github-node", s, func(i int) string { return fmt.Sprintf("node-%d", i) })
	case f.source == sourceEvent && f.key == "token_name":
		return n.named(n.m.Names, "token", s, func(i int) string { return fmt.Sprintf("token-%d", i) })
	case f.source == sourceEvent && freeTextMembers[f.key]:
		return n.named(n.m.Text, "text", s, func(i int) string { return fmt.Sprintf("text %d", i) })
	case f.source == sourceEvent && (personMembers[f.key] || (f.key == "id" && f.kind == "user")):
		return n.person(s)
	case f.source == sourceEvent && f.key == "id" && f.kind == "session":
		if strings.HasPrefix(s, "legion-daemon:") || wholeUUID.MatchString(s) {
			return n.rename(s)
		}
		return n.named(n.m.Sessions, "session", s, func(i int) string { return fmt.Sprintf("session-%d", i) })
	case f.source == sourceEvent && f.key == "id" && f.kind == "system":
		return s
	case f.key == "session_title":
		if match := legionTitle.FindStringSubmatch(s); match != nil {
			prefix := "Legion " + match[1] + " · "
			if neutral, known := n.m.Projects[match[2]]; known {
				return prefix + neutral
			}
			return prefix + n.rename(match[2])
		}
		return n.named(n.m.Titles, "title", s, func(i int) string { return fmt.Sprintf("session %d", i) })
	case f.key == "machine":
		if match := daemonMachine.FindStringSubmatch(s); match != nil {
			if neutral, known := n.m.Projects[strings.ToUpper(match[1])]; known {
				return "legion-" + strings.ToLower(neutral) + "-daemon"
			}
		}
		return n.named(n.m.Machines, "machine", s, func(i int) string { return fmt.Sprintf("machine-%d", i) })
	case f.key == "host":
		if s == "omp" { // the harness a Legion session runs in, no machine's name
			return s
		}
		return n.named(n.m.Hosts, "host", s, func(i int) string { return fmt.Sprintf("host-%d", i) })
	case f.key == "cwd":
		return n.workdir(s)
	case repoMembers[f.key]:
		return n.repo(s)
	case f.key == "url":
		return n.link(s)
	case f.key == "content_sha256":
		return n.named(n.m.Digests, "digest", s, func(i int) string {
			sum := sha256.Sum256([]byte(fmt.Sprintf("neutral digest %d", i)))
			return hex.EncodeToString(sum[:])
		})
	case f.key == "labels" && f.source == sourceEvent:
		if strings.EqualFold(s, dispatch.LegionLabel) {
			return s
		}
		return n.named(n.m.Names, "label", s, func(i int) string { return fmt.Sprintf("label-%d", i) })
	case f.parent == "components" && (f.key == "ids" || f.key == "unknown"):
		return n.named(n.m.Names, "component", s, func(i int) string { return fmt.Sprintf("component-%d", i) })
	case f.parent == "advances" && f.key == "id":
		return n.named(n.m.Names, "priority", s, func(i int) string { return fmt.Sprintf("priority-%d", i) })
	}
	return n.rename(s)
}

// rename is s with every name it holds by its shape renamed (namesPattern), the rest of it kept.
func (n *neutralizer) rename(s string) string {
	matches := n.names.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	groups := n.names.SubexpNames()
	var out strings.Builder
	last := 0
	for _, match := range matches {
		out.WriteString(s[last:match[0]])
		name := s[match[0]:match[1]]
		for group := 1; group < len(groups); group++ {
			if match[2*group] < 0 {
				continue
			}
			switch groups[group] {
			case "link":
				name = n.link(name)
			case "email":
				name = n.person(name)
			case "uuid":
				name = n.named(n.m.IDs, "uuid", strings.ToLower(name), func(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", i) })
			case "address":
				name = n.address(name)
			case "repo":
				name = n.repo(name)
			case "host":
				name = n.named(n.m.Hosts, "hostname", name, func(i int) string { return fmt.Sprintf("host-%d.example.invalid", i) })
			case "daemon":
				name = "legion-daemon:" + n.projectName(strings.TrimPrefix(name, "legion-daemon:"))
			case "token":
				name = "legion-" + strings.ToLower(n.projectName(strings.ToUpper(strings.TrimSuffix(strings.TrimPrefix(name, "legion-"), "-")))) + "-"
			case "key":
				name = n.key(name)
			case "project":
				name = n.projectName(name)
			}
			break
		}
		out.WriteString(name)
		last = match[1]
	}
	out.WriteString(s[last:])
	return out.String()
}

// renameLogins is s, a pod's name or a pull request's branch, with each person's login it spells
// renamed to that person's neutral login: a login the window names elsewhere (logins), standing
// in s between characters that are no letter or digit, the longest first, so a login inside a
// longer one is not renamed in its place.
func (n *neutralizer) renameLogins(s string) string {
	logins := slices.SortedFunc(maps.Keys(n.logins), func(a, b string) int { return cmp.Or(len(b)-len(a), strings.Compare(a, b)) })
	for _, login := range logins {
		var out strings.Builder
		last := 0
		for from := 0; ; {
			i := strings.Index(s[from:], login)
			if i < 0 {
				break
			}
			start, end := from+i, from+i+len(login)
			if bounded(s, start, end) {
				out.WriteString(s[last:start])
				out.WriteString(n.person(login))
				last = end
			}
			from = end
		}
		out.WriteString(s[last:])
		s = out.String()
	}
	return s
}

// bounded says whether s[start:end] stands between characters of s that are no letter or digit,
// or s's ends.
func bounded(s string, start, end int) bool {
	word := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' }
	return (start == 0 || !word(s[start-1])) && (end == len(s) || !word(s[end]))
}

// named is the neutral name of s in names, made with neutral from the next number of kind where
// s has none yet.
func (n *neutralizer) named(names map[string]string, kind, s string, neutral func(int) string) string {
	if name, known := names[s]; known {
		return name
	}
	n.counters[kind]++
	names[s] = neutral(n.counters[kind])
	return names[s]
}

// address is the neutral form of s, a match of addressShape: an address of the documentation
// ranges, the same for each address however it is written (its canonical form, an IPv4 address
// written as an IPv6 one read as the IPv4 one). s is kept where it reads as no address, and where
// it is a loopback or an unspecified address or one of a documentation range, none of which names
// a deployment's network. Where the ranges have no neutral address left, s is kept and the first
// such address refuses the neutralization (neutralizer.err).
func (n *neutralizer) address(s string) string {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return s
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsUnspecified() || documentationAddress(addr) {
		return s
	}
	if neutral, known := n.m.Addresses[addr.String()]; known {
		return neutral
	}
	neutral, ok := n.neutralAddress(addr.Is4())
	if !ok {
		if n.err == nil {
			n.err = fmt.Errorf("replay: neutralize: the window holds more IPv4 addresses than the %d the documentation ranges hold",
				len(ipv4Documentation)*254)
		}
		return s
	}
	n.m.Addresses[addr.String()] = neutral.String()
	return neutral.String()
}

// neutralAddress is the next address of the documentation ranges that no address of the window
// is: for IPv4, 192.0.2.1 to 192.0.2.254, then the same of 198.51.100.0/24 and of 203.0.113.0/24,
// and false once those are spent; for IPv6, 2001:db8::1 on.
func (n *neutralizer) neutralAddress(ipv4 bool) (netip.Addr, bool) {
	for {
		var neutral netip.Addr
		if ipv4 {
			i := n.counters["ipv4"]
			if i == len(ipv4Documentation)*254 {
				return netip.Addr{}, false
			}
			n.counters["ipv4"]++
			octets := ipv4Documentation[i/254].Addr().As4()
			octets[3] = byte(i%254 + 1)
			neutral = netip.AddrFrom4(octets)
		} else {
			n.counters["ipv6"]++
			octets := ipv6Documentation.Addr().As16()
			binary.BigEndian.PutUint64(octets[8:], uint64(n.counters["ipv6"]))
			neutral = netip.AddrFrom16(octets)
		}
		if !n.documented[neutral] {
			return neutral, true
		}
	}
}

// documentationAddress says whether addr is in one of the documentation ranges.
func documentationAddress(addr netip.Addr) bool {
	return ipv6Documentation.Contains(addr) || slices.ContainsFunc(ipv4Documentation, func(r netip.Prefix) bool { return r.Contains(addr) })
}

// projectName is a registered project's neutral key; a project the first walk did not register
// is kept, since it was never one (a word in a project member that names none).
func (n *neutralizer) projectName(project string) string {
	if neutral, known := n.m.Projects[project]; known {
		return neutral
	}
	return project
}

// key is the neutral key of key, in the case it was written in: a claim token and a workspace
// spell a key in lower case.
func (n *neutralizer) key(key string) string {
	neutral, known := n.m.Keys[strings.ToUpper(key)]
	if !known {
		return key
	}
	if key == strings.ToLower(key) {
		return strings.ToLower(neutral)
	}
	return neutral
}

// person is the neutral name of a person's login or email: person-<n>, at example.invalid for an
// email.
func (n *neutralizer) person(s string) string {
	if strings.Contains(s, "@") {
		return n.named(n.m.People, "email", s, func(i int) string { return fmt.Sprintf("person-%d@example.invalid", i) })
	}
	return n.named(n.m.People, "login", s, func(i int) string { return fmt.Sprintf("person-%d", i) })
}

// repo is the neutral owner/name of a repository: keptRepository as it is, every other its own
// acme/repo-<n>. A value that is no owner/name is renamed as any other string is (rename).
func (n *neutralizer) repo(repo string) string {
	if neutral, known := n.m.Repos[repo]; known {
		return neutral
	}
	registered := len(n.repos)
	if n.addRepo(repo); len(n.repos) == registered {
		return n.rename(repo)
	}
	n.m.Repos[repo] = fmt.Sprintf("acme/repo-%d", len(n.repos))
	return n.m.Repos[repo]
}

// link is the neutral form of a link: one to this repository, or to a registered repository on
// GitHub or in its REST API, keeps its form with its repository renamed and the names in the rest
// of it, so a merge queue event's url still names its repository (ParseQueueLine); any other is a
// numbered link of example.invalid, since its host and path are the record's.
func (n *neutralizer) link(link string) string {
	for _, form := range []struct {
		pattern *regexp.Regexp
		prefix  string
	}{{githubRepoURL, "https://github.com/"}, {githubAPIRepoURL, "https://api.github.com/repos/"}} {
		if match := form.pattern.FindStringSubmatch(link); match != nil {
			if neutral, known := n.m.Repos[match[1]]; known {
				return form.prefix + neutral + n.rename(match[2])
			}
		}
	}
	return n.named(n.m.Links, "link", link, func(i int) string { return fmt.Sprintf("https://example.invalid/link-%d", i) })
}

// workdir is the neutral form of a session's working directory: a Legion workspace with its
// repository and the names after it renamed, another path under /legion with its names renamed,
// and any other path, which is a person's machine's, a numbered /work/<n>.
func (n *neutralizer) workdir(path string) string {
	if match := workspacePath.FindStringSubmatch(path); match != nil {
		return "/legion/workspaces/" + n.repo(match[1]) + n.rename(match[2])
	}
	if strings.HasPrefix(path, "/legion/") || path == "/legion" {
		return n.rename(path)
	}
	return n.named(n.m.Paths, "path", path, func(i int) string { return fmt.Sprintf("/work/%d", i) })
}

// rewrite is v, read from from, rewritten and written as compact JSON.
func (n *neutralizer) rewrite(v any, from source) ([]byte, error) {
	var out bytes.Buffer
	if err := encodeOrdered(&out, n.walk(v, field{source: from}, true)); err != nil {
		return nil, fmt.Errorf("replay: neutralize: %w", err)
	}
	return out.Bytes(), nil
}

// rewriteAll is each of values rewritten (rewrite).
func (n *neutralizer) rewriteAll(values []any, from source) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(values))
	for _, value := range values {
		line, err := n.rewrite(value, from)
		if err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, nil
}

// checkWritten refuses a neutral window in dir that still holds a deny word, anywhere in any
// file, or a key, a claim token's project or the daemon's session of a project it renamed to
// another name: the proof, over the bytes written, that the rules missed none.
func (n *neutralizer) checkWritten(dir string) error {
	var left []*regexp.Regexp
	left = append(left, n.deny...)
	neutral := slices.Collect(maps.Values(n.m.Projects))
	for _, project := range n.sourceProjects {
		if slices.Contains(neutral, project) {
			continue // a record's name that is also a neutral one cannot be told apart by its spelling
		}
		quoted := regexp.QuoteMeta(project)
		left = append(left, regexp.MustCompile(`(?i)\b`+quoted+`-[0-9]+\b|legion-`+strings.ToLower(quoted)+`-|legion-daemon:`+quoted+`\b`))
	}
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, pattern := range left {
			if found := pattern.Find(data); found != nil {
				return fmt.Errorf("replay: neutralize: %s still holds %q, a name no rule renamed", path, found)
			}
		}
		return nil
	})
}

// jsonObject is a JSON object whose members keep the order they were written in, so a line
// rewritten by the neutralizer reads as the daemon wrote it, member for member.
type jsonObject []jsonMember

// jsonMember is one member of a jsonObject.
type jsonMember struct {
	key   string
	value any
}

// decodeOrdered reads data as one JSON value: an object as a jsonObject, a list as []any, a number
// as the json.Number written, and a string, a bool or null as encoding/json reads them.
func decodeOrdered(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON value")
	}
	return value, nil
}

func decodeValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}
	switch delim {
	case '{':
		object := jsonObject{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			value, err := decodeValue(decoder)
			if err != nil {
				return nil, err
			}
			object = append(object, jsonMember{key: key.(string), value: value})
		}
		_, err = decoder.Token()
		return object, err
	case '[':
		list := []any{}
		for decoder.More() {
			value, err := decodeValue(decoder)
			if err != nil {
				return nil, err
			}
			list = append(list, value)
		}
		_, err = decoder.Token()
		return list, err
	}
	return nil, fmt.Errorf("unexpected %v", delim)
}

// encodeOrdered writes v, a value decodeOrdered reads, as compact JSON: members in their order,
// numbers as written, strings with no HTML escaping, each string through one encoder of out.
func encodeOrdered(out *bytes.Buffer, v any) error {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	return orderedEncoder{out: out, strings: encoder}.encode(v)
}

// orderedEncoder is encodeOrdered's writer: the buffer it writes to and the encoder of its strings.
type orderedEncoder struct {
	out     *bytes.Buffer
	strings *json.Encoder
}

// encode writes v (encodeOrdered).
func (e orderedEncoder) encode(v any) error {
	switch value := v.(type) {
	case jsonObject:
		e.out.WriteByte('{')
		for i, member := range value {
			if i > 0 {
				e.out.WriteByte(',')
			}
			if err := e.encodeString(member.key); err != nil {
				return err
			}
			e.out.WriteByte(':')
			if err := e.encode(member.value); err != nil {
				return err
			}
		}
		e.out.WriteByte('}')
	case []any:
		e.out.WriteByte('[')
		for i, element := range value {
			if i > 0 {
				e.out.WriteByte(',')
			}
			if err := e.encode(element); err != nil {
				return err
			}
		}
		e.out.WriteByte(']')
	case string:
		return e.encodeString(value)
	case json.Number:
		e.out.WriteString(value.String())
	case bool:
		e.out.WriteString(strconv.FormatBool(value))
	case nil:
		e.out.WriteString("null")
	default:
		return fmt.Errorf("no JSON value: %T", v)
	}
	return nil
}

// encodeString writes s as a JSON string, with no HTML escaping.
func (e orderedEncoder) encodeString(s string) error {
	if err := e.strings.Encode(s); err != nil {
		return err
	}
	e.out.Truncate(e.out.Len() - 1) // Encode ends the value with a newline
	return nil
}

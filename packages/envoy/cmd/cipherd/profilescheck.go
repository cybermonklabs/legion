package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// profilesCommand and profilesCheckCommand are the two words that read a profiles file instead of
// serving: `broker profiles check`.
const (
	profilesCommand      = "profiles"
	profilesCheckCommand = "check"
)

// profilesCheckForm is the subcommand as a person writes it, which its usage line and every
// message that points at it name.
const profilesCheckForm = profilesCommand + " " + profilesCheckCommand + " <file> [--against <old file>] [--secrets-json <file>] [--private-upstream '<host>=<url> ...']"

// profilesCheckUsage is the one line every usage error of the subcommand prints, in the form the
// end-sessions usage line takes.
const profilesCheckUsage = "broker: usage: broker " + profilesCheckForm + ", one profiles file and nothing else"

// profilesCheckName opens every line the subcommand writes.
const profilesCheckName = profilesCommand + " " + profilesCheckCommand

// removedAirlockCheckCommand is the subcommand that checked the airlock's rules file, which the
// profiles file replaced: it is refused naming `profiles check`, so a deployment's pull-request
// check that still runs it fails rather than reading a file of the old shape.
const removedAirlockCheckCommand = "airlock-check"

// secretIDPrefix opens the id of each agent secret in the list --secrets-json reads: the list
// `gcloud secrets list --filter='name~agent-secrets-'` prints, whose names end in
// .../secrets/agent-secrets-<slug>, the secret's name as policy.SlugToName reads its slug.
const secretIDPrefix = "agent-secrets-"

// runProfilesCheck reads the profiles file args names and says whether the broker would serve it,
// with the broker's own reader and no second reader of the format: policy.ParseProfiles, which the
// boot runs too, so that what this agrees to is exactly what the boot agrees to but for the one
// read it has no setting for, each requester service against the deployment's registered services
// (readProfiles). A pull request that changes the deployment's profiles.json runs it, so the file
// a reviewer is asked to read is one the broker has already agreed to serve.
//
// On a file it would serve it writes the report on stdout: what the file adds and removes against
// the file --against names, or against an empty profiles file when that flag is absent, which is
// how a whole file reads as added. The report is of the file's kinds and then its profiles. A kind
// or a profile added or removed is written whole, every field of it, the ones the file leaves out
// among them, and a profile's block it does not have as that block's none. For a kind or a profile
// both files hold it writes each value of a list added or removed and each other field as old →
// new. A kind's fields are its hosts, headers, variables, ways, allow rows, placeholder_prefix and
// max_grant; a profile's are its source, tier, requesters and maxGrant, the kind its proxy key names
// as a string, its own allow rows, its GitHub accounts, repositories, permissions and value, its
// Google scopes, and a dpop block's hosts, headers, allow and kind. Every field is in the report
// because each decides what a credential may reach or who may have it. What is left for the
// reviewer is whether each is right. Who decides a request for a credential is no field of the
// file: it is the source key's own judge and guidance tags.
//
// It refuses, exit 1 and one line on stderr per fault, naming the file: a file ParseProfiles
// refuses (one line, naming the kind or the profile and the field, or the parse position for a
// file that is not JSON; a proxy block of any kind but dpop is refused naming kinds); a dpop block
// the boot refuses for its shape (config.MintedEntryRules); and, for each profile with an allow of
// its own whose kind it knows, each row whose host that kind does not bind and the rows that
// kind's allow does not cover (proxyrules.Outside), so a profile narrows its kind and never widens
// it, as the policy load holds it (policy.ReasonProfileAllowHost,
// policy.ReasonProfileAllowOutsideKind). A profile's kind is its source's proxy tag, or else the
// kind its proxy string names (policy's proxied). The file alone gives the proxy string; the tag
// is in the store, which --secrets-json reads. Without that flag a profile with an allow and no
// proxy string gets one line saying its rows were not held to a kind, so a reader knows what it did
// not see. With it, a profile with an allow whose source carries no proxy-narrowed=true tag is
// refused too (policy.ReasonProfileAllowNotNarrowed), and so are a proxy string that names another
// kind than its source's tag (policy.ReasonProxyKindConflict) and a profile with an allow behind no
// kind (policy.ReasonProfileSourceNotProxied).
//
// With --against it also writes, for each name behind the proxy under both files, each row of its
// allow under the file that its allow under the old file does not cover (proxyrules.Covers), under
// the heading widens, and each host the change adds to the hosts the name reaches. A name's allow
// is its profile's own where it has one, else its kind's, else its proxy block's; a missing allow
// allows every method and path. A widening and an added host are listed, not refused: the
// reviewer reads them, and a gate that must see none reads this list.
//
// With --secrets-json, the output of `gcloud secrets list --format='json(name,annotations)'` for
// the agent secrets, it writes each secret's proxy and proxy-narrowed tags, then each name behind
// the proxy under the old file and how the file serves it, and exits 1 when a name leaves the
// proxy: a name behind a proxy block, a kind or a dpop block under the old file that the file puts
// behind none (servingOf).
//
// A usage error is exit 2 and the usage line, and so is a --private-upstream value that is not one
// BROKER_PROXY_PRIVATE_UPSTREAM takes. It reads no configuration, opens no database and binds
// nothing: the files and the flags are all it reads.
//
// The dpop blocks are held to every rule of the boot's that the file alone decides
// (config.MintedEntryRules). Their tie to the private destinations is the boot's to check with the
// broker's own BROKER_PROXY_PRIVATE_UPSTREAM: given that value as --private-upstream, this runs
// the boot's whole check (config.ProxyMintedRules), and without it, it writes, for each dpop
// block, one line saying that tie was not checked.
//
// The --against file is decoded and never held to the file's rules (policy.DecodeProfiles): the
// profiles in force may be a file this broker would refuse, one in the form of proxy blocks the
// kinds replaced among them (policy.Profiles.LegacyProxy), and what changed is the point of the
// report, which would otherwise also refuse the pull request that moves that file to kinds.
func runProfilesCheck(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("broker "+profilesCheckName, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, profilesCheckUsage) }
	against := flags.String("against", "", "the profiles file in force now, the one the report is made against")
	secretsJSON := flags.String("secrets-json", "", "the agent secrets and their annotations, as gcloud secrets list --format='json(name,annotations)' prints them, whose proxy and proxy-narrowed tags the policy load reads")
	upstreamFlag := flags.String("private-upstream", "", "the <host>=<url> entries the broker's BROKER_PROXY_PRIVATE_UPSTREAM holds, against which the dpop blocks' hosts are checked")
	// The subcommand's form puts the file first and the flag after it, and flag.Parse stops at
	// the first argument that is not a flag, so the files are taken out one at a time and what
	// follows each is parsed again. A flag then works wherever it is written, and a second file
	// is still a usage error rather than an argument that is quietly ignored.
	var files []string
	rest := args
	for {
		if err := flags.Parse(rest); err != nil {
			// Parse has written what it refused and then the usage line.
			return 2
		}
		if flags.NArg() == 0 {
			break
		}
		files = append(files, flags.Arg(0))
		rest = flags.Args()[1:]
	}
	if len(files) != 1 || strings.TrimSpace(files[0]) == "" {
		flags.Usage()
		return 2
	}
	path := strings.TrimSpace(files[0])
	now, err := readProfilesFile(path, policy.ParseProfiles)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", profilesCheckName, err)
		return 1
	}
	rules := now.FileRules()
	if err := config.MintedEntryRules(rules); err != nil {
		fmt.Fprintf(stderr, "%s: %s: %v\n", profilesCheckName, path, err)
		return 1
	}
	if text := strings.TrimSpace(*upstreamFlag); text != "" {
		upstreams, err := config.ParsePrivateUpstreams(text)
		for _, upstream := range upstreams {
			if err != nil {
				break
			}
			// The broker's start refuses a destination no dial reaches; this knows no dial
			// allow, the local stack's alone, so it refuses every such destination.
			err = config.PrivateUpstreamDialable(upstream, nil)
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s: --private-upstream: %v\n", profilesCheckName, err)
			flags.Usage()
			return 2
		}
		if err := config.ProxyMintedRules(rules, upstreams); err != nil {
			fmt.Fprintf(stderr, "%s: %s: %v\n", profilesCheckName, path, err)
			return 1
		}
	}
	var tags *storeTags
	if name := strings.TrimSpace(*secretsJSON); name != "" {
		if tags, err = readStoreTags(name); err != nil {
			fmt.Fprintf(stderr, "%s: --secrets-json %v\n", profilesCheckName, err)
			return 1
		}
	}
	if refusals := allowRefusals(now, tags); len(refusals) > 0 {
		for _, refusal := range refusals {
			fmt.Fprintf(stderr, "%s: %s: %s\n", profilesCheckName, path, refusal)
		}
		return 1
	}
	var old policy.Profiles
	oldName := "an empty profiles file, so every kind and profile reads as added"
	compared := strings.TrimSpace(*against) != ""
	if compared {
		name := strings.TrimSpace(*against)
		if old, err = readProfilesFile(name, policy.DecodeProfiles); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", profilesCheckName, err)
			return 1
		}
		oldName = name
	}
	fmt.Fprintf(stdout, "%s: %s is valid: %s and %s\n", profilesCheckName, path, counted(len(now.Names()), "profile"), counted(len(now.Kinds()), "kind"))
	if strings.TrimSpace(*upstreamFlag) == "" {
		for _, b := range rules.Names {
			if b.Inline != nil && b.Way == proxyrules.WayDPoP {
				fmt.Fprintf(stdout, "%s: %s binds %s; its tie to BROKER_PROXY_PRIVATE_UPSTREAM was not checked (no --private-upstream)\n", profilesCheckName, b.Name, strings.Join(b.Inline.Hosts, " "))
			}
		}
	}
	if tags == nil {
		writeAllowsNotChecked(stdout, now)
	}
	fmt.Fprintf(stdout, "%s: compared against %s\n", profilesCheckName, oldName)
	writeFileChanges(stdout, old, now)
	if compared {
		writeWidens(stdout, old, now, tags, oldName, path)
	}
	if tags == nil {
		return 0
	}
	if leaving := writeServed(stdout, old, now, tags, oldName, path); len(leaving) > 0 {
		fmt.Fprintf(stderr, "%s: %s: %s the proxy: %s\n", profilesCheckName, path, leaveCount(len(leaving)), strings.Join(leaving, ", "))
		return 1
	}
	return 0
}

// counted counts n things of a file in words, as the end-sessions subcommand counts the sessions
// it ended: "1 profile", "2 kinds", "2 entries".
func counted(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	if stem, ok := strings.CutSuffix(thing, "y"); ok {
		return fmt.Sprintf("%d %sies", n, stem)
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// leaveCount says how many names leave the proxy, in words.
func leaveCount(n int) string {
	if n == 1 {
		return "1 name leaves"
	}
	return fmt.Sprintf("%d names leave", n)
}

// The words the report writes for a field the file leaves out, where a word other than the
// value's own says what the broker does.
const (
	// sourcesWord is a tier, requesters or maxGrant the profile takes from its source.
	sourcesWord = "the source's"
	// everyRepository is a github block with no repositories: its token reaches every repository
	// of the installation.
	everyRepository = "every repository the installation reaches"
	// noneWord is a block the profile does not have, a list that names nothing, a profile's proxy
	// key that names no kind as a string, a profile with no allow of its own, and a kind's
	// max_grant left out, which caps no secret's grant.
	noneWord = "none"
	// anyPath is an allow that is missing (nil), which allows any method and path
	// (proxyrules.allowOK).
	anyPath = "any method and path"
	// nothingWord is an allow that is present and empty, which allows nothing.
	nothingWord = "nothing"
)

// defaultPrefix is a kind's placeholder_prefix left out: its placeholders open with the default
// prefix.
var defaultPrefix = "the default, " + placeholder.Prefix

// reportField is one field of a kind or a profile as the report writes it, under the name the
// file's JSON gives it: a list, whose values each take a line of their own and are added and
// removed one at a time, or one text. joiner is what separates a list's values when the field is
// written as one text (" " where it is empty): ", " for allow rows, which hold spaces themselves.
type reportField struct {
	name   string
	list   bool
	items  []string
	text   string
	joiner string
}

// reportBlock is one block of a profile as the report writes it: its fields, nil when the profile
// has no such block.
type reportBlock struct {
	name   string
	fields []reportField
}

// entryReport is one kind or one profile as the report writes it: its own fields and then its
// blocks, each in the order the file's JSON gives them. A kind has no block.
type entryReport struct {
	fields []reportField
	blocks []reportBlock
}

// listField is a field of a list's values.
func listField(name string, items []string) reportField {
	return reportField{name: name, list: true, items: items}
}

// textField is a field of one text.
func textField(name, text string) reportField {
	return reportField{name: name, text: text}
}

// listOr is a list field, or the text absent when the file leaves the list out (nil).
func listOr(name string, items []string, absent string) reportField {
	if items == nil {
		return textField(name, absent)
	}
	return listField(name, items)
}

// allowField is an allow list as a field: one value per row (rowText), or, for the two lists that
// mean something other than what they list, absent for a missing list (a kind's allows any method
// and path; a profile's is no allow of its own) and nothing for an empty one.
func allowField(name string, rows []proxyrules.Allow, absent string) reportField {
	switch {
	case rows == nil:
		return textField(name, absent)
	case len(rows) == 0:
		return textField(name, nothingWord)
	}
	items := make([]string, len(rows))
	for i, row := range rows {
		items[i] = rowText(row)
	}
	return reportField{name: name, list: true, items: items, joiner: ", "}
}

// kindReport is every field of k as the report writes it, a field the file leaves out in the words
// that say what the broker does: the default header (proxyrules.Kind.HeaderNames), the default
// placeholder prefix, and no cap on a secret's grant.
func kindReport(k *proxyrules.Kind) *entryReport {
	return &entryReport{fields: []reportField{
		listField("hosts", k.Hosts),
		listField("headers", k.HeaderNames()),
		listField("variables", k.Variables),
		listField("ways", k.Ways),
		allowField("allow", k.Allow, anyPath),
		textField("placeholder_prefix", cmp.Or(k.PlaceholderPrefix, defaultPrefix)),
		textField("max_grant", cmp.Or(k.MaxGrant, noneWord)),
	}}
}

// profileReport is every field of p, a profile of profiles, as the report writes it, in
// policy.ParseProfiles' form: a source written out, accounts lowercased and sorted, repositories
// sorted, a value's default written out, so that two spellings of one profile report alike. Its
// proxy block is its dpop block, or, in an old file DecodeProfiles read, the proxy block the kinds
// replaced (policy.Profiles.LegacyProxy).
func profileReport(profiles policy.Profiles, p *policy.Profile) *entryReport {
	maxGrant := sourcesWord
	if p.MaxGrant != 0 {
		maxGrant = p.MaxGrant.String()
	}
	report := &entryReport{fields: []reportField{
		textField("source", p.Source),
		textField("tier", cmp.Or(p.Tier, sourcesWord)),
		listOr("requesters", p.Requesters, sourcesWord),
		textField("maxGrant", maxGrant),
		textField("proxy", cmp.Or(p.ProxyKind, noneWord)),
		allowField("allow", p.Allow, noneWord),
	}}
	github := reportBlock{name: "github"}
	if b := p.GitHub; b != nil {
		permissions := make([]string, 0, len(b.Permissions))
		for _, permission := range slices.Sorted(maps.Keys(b.Permissions)) {
			permissions = append(permissions, permission+" "+b.Permissions[permission])
		}
		github.fields = []reportField{
			listField("github.accounts", b.Accounts),
			listOr("github.repositories", b.Repositories, everyRepository),
			listField("github.permissions", permissions),
			textField("github.value", b.Value),
		}
	}
	google := reportBlock{name: "google"}
	if b := p.Google; b != nil {
		google.fields = []reportField{listField("google.scopes", b.Scopes)}
	}
	proxy := reportBlock{name: "proxy block"}
	if b := cmp.Or(p.Proxy, profiles.LegacyProxy(p.Name)); b != nil {
		proxy.fields = []reportField{
			listField("proxy.hosts", b.Hosts),
			listField("proxy.headers", proxyrules.Kind{Headers: b.Headers}.HeaderNames()),
			textField("proxy.allow", allowText(b.Allow)),
			textField("proxy.kind", shown(b.Kind)),
		}
	}
	report.blocks = []reportBlock{github, google, proxy}
	return report
}

// writeFileChanges writes the report: the kinds and then the profiles, one line per fact, each
// part in name order and then each entry's fields in the file's order, so that two runs on the
// same pair of files write the same bytes and a reviewer can read two reports against each other.
// It writes one line saying so when the two files say the same thing.
//
// Where a file repeats a name, the entry read is the first one, as policy.Profiles.Lookup and
// proxyrules.Rules.Kind, and so the broker itself, read it. The new file is validated before this
// runs and a repeated name is a refusal there; only an old file can hold one.
func writeFileChanges(out io.Writer, old, now policy.Profiles) {
	kindOf := func(file policy.Profiles) func(string) *entryReport {
		rules := file.FileRules()
		return func(name string) *entryReport {
			if k := rules.Kind(name); k != nil {
				return kindReport(k)
			}
			return nil
		}
	}
	profileOf := func(file policy.Profiles) func(string) *entryReport {
		return func(name string) *entryReport {
			if p := file.Lookup(name); p != nil {
				return profileReport(file, p)
			}
			return nil
		}
	}
	kinds := writeChanges(out, "kind", sortedUnion(kindNames(old), kindNames(now)), kindOf(old), kindOf(now))
	profiles := writeChanges(out, "profile", sortedUnion(old.Names(), now.Names()), profileOf(old), profileOf(now))
	if !kinds && !profiles {
		fmt.Fprintf(out, "%s: nothing changed\n", profilesCheckName)
	}
}

// kindNames is the name of each kind file declares, in the file's order.
func kindNames(file policy.Profiles) []string {
	kinds := file.Kinds()
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = k.Name
	}
	return names
}

// writeChanges writes what the entries of one part of the file, its kinds or its profiles, named
// subject in each line, have changed between the old file and the new one, over names, and reports
// whether it wrote anything. was and is answer an entry of a name in the old and the new file, nil
// for a name the file holds none of.
func writeChanges(out io.Writer, subject string, names []string, was, is func(string) *entryReport) bool {
	changed := false
	for _, name := range names {
		label := fmt.Sprintf("%s %q", subject, name)
		w, i := was(name), is(name)
		switch {
		case i == nil:
			fmt.Fprintf(out, "%s: removed\n", label)
			writeWholeEntry(out, label, w)
			changed = true
		case w == nil:
			fmt.Fprintf(out, "%s: added\n", label)
			writeWholeEntry(out, label, i)
			changed = true
		default:
			if writeEntryChanges(out, label, w, i) {
				changed = true
			}
		}
	}
	return changed
}

// writeWholeEntry writes every field of a kind or a profile added or removed, and each block it
// does not have as that block's none.
func writeWholeEntry(out io.Writer, label string, report *entryReport) {
	writeFields(out, label, report.fields)
	for _, block := range report.blocks {
		if block.fields == nil {
			fmt.Fprintf(out, "%s: %s %s\n", label, block.name, noneWord)
			continue
		}
		writeFields(out, label, block.fields)
	}
}

// writeFields writes each field whole: a list one line per value, or none when it names nothing,
// and a text on one line.
func writeFields(out io.Writer, label string, fields []reportField) {
	for _, field := range fields {
		if !field.list {
			fmt.Fprintf(out, "%s: %s %s\n", label, field.name, field.text)
			continue
		}
		if len(field.items) == 0 {
			fmt.Fprintf(out, "%s: %s %s\n", label, field.name, noneWord)
		}
		for _, item := range field.items {
			fmt.Fprintf(out, "%s: %s %s\n", label, field.name, item)
		}
	}
}

// writeEntryChanges writes what one kind or profile both files hold has changed, and reports
// whether it wrote anything: each field's change, and a block one file has and the other does not
// as that block added or removed, with each of its fields.
func writeEntryChanges(out io.Writer, label string, was, is *entryReport) bool {
	changed := false
	for i := range was.fields {
		if writeFieldChanges(out, label, was.fields[i], is.fields[i]) {
			changed = true
		}
	}
	for i, wasBlock := range was.blocks {
		isBlock := is.blocks[i]
		switch {
		case wasBlock.fields == nil && isBlock.fields == nil:
		case wasBlock.fields == nil:
			fmt.Fprintf(out, "%s: %s added\n", label, isBlock.name)
			writeFields(out, label, isBlock.fields)
			changed = true
		case isBlock.fields == nil:
			fmt.Fprintf(out, "%s: %s removed\n", label, wasBlock.name)
			writeFields(out, label, wasBlock.fields)
			changed = true
		default:
			for j := range wasBlock.fields {
				if writeFieldChanges(out, label, wasBlock.fields[j], isBlock.fields[j]) {
					changed = true
				}
			}
		}
	}
	return changed
}

// writeFieldChanges writes what one field has changed, and reports whether it wrote anything: for
// two lists one line for every value in exactly one of them, sorted, and otherwise the field's old
// text → its new one. The values are compared as policy.ParseProfiles answers them: a host and a
// header as the file spells them, since a change of case is a change to the file even where the
// airlock matches without regard to it, and a login lowercased, as GitHub compares it.
func writeFieldChanges(out io.Writer, label string, was, is reportField) bool {
	if was.list && is.list {
		changed := false
		for _, value := range sortedUnion(was.items, is.items) {
			switch {
			case !slices.Contains(was.items, value):
				fmt.Fprintf(out, "%s: %s %s added\n", label, is.name, value)
				changed = true
			case !slices.Contains(is.items, value):
				fmt.Fprintf(out, "%s: %s %s removed\n", label, is.name, value)
				changed = true
			}
		}
		return changed
	}
	if wasText, isText := fieldText(was), fieldText(is); wasText != isText {
		fmt.Fprintf(out, "%s: %s %s → %s\n", label, is.name, wasText, isText)
		return true
	}
	return false
}

// fieldText is a field as one text: a list's values joined by its joiner, or none when it names
// nothing.
func fieldText(field reportField) string {
	if !field.list {
		return field.text
	}
	if len(field.items) == 0 {
		return noneWord
	}
	return strings.Join(field.items, cmp.Or(field.joiner, " "))
}

// allowText reads an allow list as one text of the report, with the two lists that mean something
// other than what they list in words (proxyrules.allowOK: a missing list allows any method and
// path, an empty one allows nothing).
func allowText(allow []proxyrules.Allow) string {
	switch {
	case allow == nil:
		return anyPath
	case len(allow) == 0:
		return nothingWord
	}
	rows := make([]string, len(allow))
	for i, a := range allow {
		rows[i] = rowText(a)
	}
	return strings.Join(rows, ", ")
}

// rowText is one allow row as the report and its refusals write it: its method and path prefix,
// and " on " and its host where it names one, as the policy load's refusals name a row.
func rowText(row proxyrules.Allow) string {
	text := shown(row.Method) + " " + shown(row.PathPrefix)
	if row.Host != "" {
		text += " on " + row.Host
	}
	return text
}

// shown writes an empty field as a pair of quotes, so that a line of the report never ends in a
// blank where a reader cannot tell an empty value from a missing word.
func shown(value string) string {
	if value == "" {
		return `""`
	}
	return value
}

// sortedUnion is every value in any of lists, once each, sorted.
func sortedUnion(lists ...[]string) []string {
	values := map[string]struct{}{}
	for _, list := range lists {
		for _, v := range list {
			values[v] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(values))
}

// storeTags is the agent secrets --secrets-json lists, by name, each with its annotations: what
// the policy load reads of a secret's proxy and proxy-narrowed tags, without its value. skipped
// counts the entries whose id is no agent secret's, which it does not read.
type storeTags struct {
	path    string
	secrets map[string]map[string]string
	skipped int
}

// proxy is the kind the proxy tag of the secret name names, "" for none or a secret the list does
// not hold.
func (s *storeTags) proxy(name string) string {
	return s.secrets[name][policy.TagProxy]
}

// narrowed reports whether the secret name carries proxy-narrowed=true.
func (s *storeTags) narrowed(name string) bool {
	return s.secrets[name][policy.TagProxyNarrowed] == policy.ProxyNarrowedTrue
}

// stored reports whether the list holds the secret name.
func (s *storeTags) stored(name string) bool {
	_, ok := s.secrets[name]
	return ok
}

// names is every secret's name, sorted.
func (s *storeTags) names() []string {
	return slices.Sorted(maps.Keys(s.secrets))
}

// readStoreTags reads the --secrets-json file at path: a JSON list in which every entry is an
// object with a name and annotations that are an object of strings or absent, as `gcloud secrets
// list --format='json(name,annotations)'` prints them. An entry whose id (the name's last path
// element) is agent-secrets- and a slug of an agent secret's name is that secret, named as the
// broker names it (policy.SlugToName); any other entry is skipped and counted. It refuses a file
// it cannot read, one that is not such a list, an entry with no name, a secret listed twice, and a
// list of no agent secret, which shows nothing of the store's tags and so would read as a store in
// which no secret is behind the proxy. Each refusal names the file.
func readStoreTags(path string) (*storeTags, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Name        *string           `json:"name"`
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("%s: is not the list of secrets gcloud secrets list prints, each with a name and its annotations: %v", path, err)
	}
	tags := &storeTags{path: path, secrets: map[string]map[string]string{}}
	for i, entry := range entries {
		if entry.Name == nil || *entry.Name == "" {
			return nil, fmt.Errorf("%s: entry %d has no name, so it names no secret", path, i)
		}
		id := (*entry.Name)[strings.LastIndexByte(*entry.Name, '/')+1:]
		slug, agent := strings.CutPrefix(id, secretIDPrefix)
		name := policy.SlugToName(slug)
		if again, err := policy.NameToSlug(name); !agent || err != nil || again != slug {
			tags.skipped++
			continue
		}
		if tags.stored(name) {
			return nil, fmt.Errorf("%s: lists the secret %s twice", path, *entry.Name)
		}
		annotations := entry.Annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		tags.secrets[name] = annotations
	}
	if len(tags.secrets) == 0 {
		return nil, fmt.Errorf("%s: lists no agent secret (no name ending in /secrets/%s<slug>), so it shows nothing of the store's tags", path, secretIDPrefix)
	}
	return tags, nil
}

// allowRefusals is each fault of the file's profiles that the policy load would refuse a profile
// for and that the file, with the tags where --secrets-json gave them, decides, one text per fault
// naming the profile, in the file's order. For a profile whose source's proxy tag names one kind
// and whose proxy string another: that conflict (policy.ReasonProxyKindConflict). For a profile
// with an allow of its own, under the kind its source's tag, or else its proxy string, names: each
// row whose host that kind does not bind (proxyrules.HostBound), and the rows the kind's allow does
// not cover (proxyrules.Outside, the cover rule the policy load holds a profile to); and, with the
// tags read, a source that carries no proxy-narrowed=true tag, a tag that names a kind the file does
// not declare, and a profile with an allow behind no kind at all. A dpop profile has no allow of its
// own (ParseProfiles refuses one).
func allowRefusals(now policy.Profiles, tags *storeTags) []string {
	rules := now.FileRules()
	var refusals []string
	for _, name := range now.Names() {
		p := now.Lookup(name)
		label := fmt.Sprintf("profile %q", name)
		var tag string
		if tags != nil {
			tag = tags.proxy(p.Source)
		}
		if tag != "" && p.ProxyKind != "" && tag != p.ProxyKind {
			refusals = append(refusals, fmt.Sprintf("%s: its proxy string names the kind %s, and its source %s's %s tag names %s", label, p.ProxyKind, p.Source, policy.TagProxy, tag))
			continue
		}
		if p.Allow == nil {
			continue
		}
		kindName := cmp.Or(tag, p.ProxyKind)
		if kindName == "" {
			if tags != nil {
				refusals = append(refusals, fmt.Sprintf("%s: its allow narrows its source %s only at Cipher's proxy, and neither %s's %s tag nor the profile's proxy string names a kind", label, p.Source, p.Source, policy.TagProxy))
			}
			continue
		}
		kind := rules.Kind(kindName)
		if kind == nil {
			// ParseProfiles holds a proxy string to the file's kinds, so this is a tag's kind.
			refusals = append(refusals, fmt.Sprintf("%s: its source %s's %s tag names the kind %s, which the file does not declare", label, p.Source, policy.TagProxy, kindName))
			continue
		}
		if tags != nil && !tags.narrowed(p.Source) {
			refusals = append(refusals, fmt.Sprintf("%s: its allow narrows its source %s at the proxy, and %s carries no %s=%s tag", label, p.Source, p.Source, policy.TagProxyNarrowed, policy.ProxyNarrowedTrue))
		}
		for _, row := range p.Allow {
			if row.Host != "" && !proxyrules.HostBound(kind.Hosts, row.Host) {
				refusals = append(refusals, fmt.Sprintf("%s: allow row %s names the host %s, which the kind %s does not bind (%s)", label, rowText(row), row.Host, kind.Name, strings.Join(kind.Hosts, ", ")))
			}
		}
		if outside := proxyrules.Outside(kind.Allow, p.Allow); outside != nil {
			rows := make([]string, len(outside))
			for i, row := range outside {
				rows[i] = rowText(row)
			}
			verb := "rows %s are"
			if len(rows) == 1 {
				verb = "row %s is"
			}
			refusals = append(refusals, fmt.Sprintf("%s: allow "+verb+" outside the kind %s's allow (%s), and a profile narrows its kind and never widens it", label, strings.Join(rows, " and "), kind.Name, allowText(kind.Allow)))
		}
	}
	return refusals
}

// writeAllowsNotChecked writes, for a run with no --secrets-json, one line for each profile with an
// allow of its own saying what of it was not checked, since the store's tags decide it: for a
// profile with no proxy string, its kind (its source's proxy tag), so its rows were held to no
// kind; and for one with a proxy string, whose rows allowRefusals held to that kind, its source's
// proxy-narrowed tag.
func writeAllowsNotChecked(out io.Writer, now policy.Profiles) {
	for _, name := range now.Names() {
		p := now.Lookup(name)
		switch {
		case p.Allow == nil:
		case p.ProxyKind == "":
			fmt.Fprintf(out, "%s: profile %q has an allow, and its kind is its source %s's %s tag, which only --secrets-json reads: its rows were not held to that kind's allow and hosts, nor %s to the %s tag\n",
				profilesCheckName, name, p.Source, policy.TagProxy, p.Source, policy.TagProxyNarrowed)
		default:
			fmt.Fprintf(out, "%s: profile %q has an allow, and its source %s's %s tag was not read (no --secrets-json)\n", profilesCheckName, name, p.Source, policy.TagProxyNarrowed)
		}
	}
}

// behind says whether a name is behind Cipher's proxy under one file (servingOf).
type behind int

const (
	// notBehind is a name the file puts behind no proxy block, kind or dpop block.
	notBehind behind = iota
	// behindProxy is a name behind one of them.
	behindProxy
	// behindUnread is a name whose kind would be a source's proxy tag, which only --secrets-json
	// reads.
	behindUnread
)

// serving is how a name is served under one file: whether it is behind the proxy and in words how
// (or why not, or why that is not known); the allow its requests are held to, its profile's own
// where it has one, else its kind's or its block's; and the hosts its credential reaches, its
// kind's or its block's, or, where every row of its profile's allow names a host, those hosts
// alone, since a row with a host allows a request to that host alone. allowKnown and hostsKnown are
// false where they are its kind's and its kind is a tag no one read.
type serving struct {
	state      behind
	how        string
	allow      []proxyrules.Allow
	allowKnown bool
	hosts      []string
	hostsKnown bool
}

// servingOf is how file serves name, as the policy load would serve it, with the store's tags
// where --secrets-json gave them (tags, nil for none):
//
//   - a profile with a proxy block, which only an old file read by DecodeProfiles holds
//     (policy.Profiles.LegacyProxy), or with a dpop block, is behind that block;
//   - another profile is behind the kind its source's proxy tag names, or else the kind its proxy
//     string names (policy's proxied); behind none when neither names one or the kind is no kind
//     of the file; and unread when it has no proxy string and the tags were not read, unless the
//     file declares no kind at all, which no tag can name;
//   - a name no profile holds is its stored secret's by its own name, behind the kind its own
//     proxy tag names; but not served by its own name at all when profiles of other names use it
//     as their source (policy.Profiles.Of), and behind nothing when the list holds no such secret.
func servingOf(file policy.Profiles, name string, tags *storeTags) serving {
	rules := file.FileRules()
	declares := len(rules.Kinds) > 0
	byKind := func(s serving, kindName, via string) serving {
		k := rules.Kind(kindName)
		if k == nil {
			return serving{state: notBehind, how: fmt.Sprintf("%s names the kind %s, which the file does not declare", via, kindName)}
		}
		s.state, s.how = behindProxy, fmt.Sprintf("behind the kind %s, by %s", k.Name, via)
		if !s.allowKnown {
			s.allow, s.allowKnown = k.Allow, true
		}
		if !s.hostsKnown {
			s.hosts, s.hostsKnown = k.Hosts, true
		}
		return s
	}
	if p := file.Lookup(name); p != nil {
		if b := cmp.Or(file.LegacyProxy(name), p.Proxy); b != nil {
			words := "its proxy block"
			if b.Kind == proxyrules.WayDPoP {
				words = "its dpop block"
			}
			return serving{state: behindProxy, how: "behind " + words, allow: b.Allow, allowKnown: true, hosts: b.Hosts, hostsKnown: true}
		}
		s := serving{allow: p.Allow, allowKnown: p.Allow != nil}
		s.hosts, s.hostsKnown = rowHosts(p.Allow)
		var tag string
		if tags != nil {
			tag = tags.proxy(p.Source)
		}
		switch {
		case tag != "":
			return byKind(s, tag, fmt.Sprintf("its source %s's %s tag", p.Source, policy.TagProxy))
		case p.ProxyKind != "":
			return byKind(s, p.ProxyKind, "its proxy string")
		case !declares:
			return serving{state: notBehind, how: "the profile has no proxy block, and the file declares no kind"}
		case tags == nil:
			s.state, s.how = behindUnread, fmt.Sprintf("its kind is its source %s's %s tag, which only --secrets-json reads", p.Source, policy.TagProxy)
			return s
		}
		return serving{state: notBehind, how: fmt.Sprintf("the profile names no kind, and its source %s carries no %s tag", p.Source, policy.TagProxy)}
	}
	if of := file.Of(name); len(of) > 0 {
		return serving{state: notBehind, how: "no profile holds the name, and its stored secret is served only as the profiles " + strings.Join(of, ", ")}
	}
	switch {
	case !declares:
		return serving{state: notBehind, how: "no profile holds the name, and the file declares no kind"}
	case tags == nil:
		return serving{state: behindUnread, how: fmt.Sprintf("no profile holds the name, and its stored secret is behind a kind only by its own %s tag, which only --secrets-json reads", policy.TagProxy)}
	case !tags.stored(name):
		return serving{state: notBehind, how: "no profile holds the name, and --secrets-json lists no secret of it"}
	case tags.proxy(name) == "":
		return serving{state: notBehind, how: fmt.Sprintf("no profile holds the name, and its stored secret carries no %s tag", policy.TagProxy)}
	}
	return byKind(serving{}, tags.proxy(name), fmt.Sprintf("its own %s tag", policy.TagProxy))
}

// rowHosts is the hosts an allow narrows its kind's to, and whether it narrows them: the hosts its
// rows name, each once, when every row names one, since a row with a host allows a request to that
// host alone; an empty list, which reaches no host, for an allow that allows nothing; and no
// narrowing for a missing allow or one with a row of no host, which leave the kind's hosts.
func rowHosts(allow []proxyrules.Allow) ([]string, bool) {
	if allow == nil {
		return nil, false
	}
	hosts := []string{}
	for _, row := range allow {
		if row.Host == "" {
			return nil, false
		}
		if !slices.Contains(hosts, row.Host) {
			hosts = append(hosts, row.Host)
		}
	}
	return hosts, true
}

// writeWidens writes, for each name behind the proxy under both files, each row of its allow under
// the new file (newName) that its allow under the old one (oldName) does not cover
// (proxyrules.Covers), as `widens "<name>": <row>`, a missing allow under the new file as "any
// method and path", which only a missing allow under the old one covers; then each host it reaches
// under the new file that it did not under the old one (proxyrules.HostBound), as `hosts "<name>":
// <host> added`. It writes "widens: none" when no row widens and nothing went uncompared. A name
// whose allow or hosts are a kind that a source's proxy tag names on one side, with no tags read,
// gets a line saying it was not compared and why; where that is so on both sides, the names are
// counted on one line.
func writeWidens(out io.Writer, old, now policy.Profiles, tags *storeTags, oldName, newName string) {
	var names []string
	if tags != nil {
		names = tags.names()
	}
	var widened, hosts []string
	notCompared, unread := 0, 0
	for _, name := range sortedUnion(old.Names(), now.Names(), names) {
		was, is := servingOf(old, name, tags), servingOf(now, name, tags)
		if was.state == notBehind || is.state == notBehind {
			continue
		}
		if was.state == behindUnread && is.state == behindUnread && !(was.allowKnown && is.allowKnown) {
			unread++
			continue
		}
		if !was.allowKnown || !is.allowKnown {
			widened = append(widened, fmt.Sprintf("widens %q: not compared: %s", name, unreadWords(was, is, oldName, newName)))
			notCompared++
			continue
		}
		rows := []string(nil)
		switch {
		case is.allow == nil && was.allow != nil:
			rows = []string{anyPath}
		case is.allow != nil:
			for _, row := range proxyrules.Outside(was.allow, is.allow) {
				rows = append(rows, rowText(row))
			}
		}
		for _, row := range rows {
			widened = append(widened, fmt.Sprintf("widens %q: %s", name, row))
		}
		if !was.hostsKnown || !is.hostsKnown {
			hosts = append(hosts, fmt.Sprintf("hosts %q: not compared: %s", name, unreadWords(was, is, oldName, newName)))
			continue
		}
		for _, host := range is.hosts {
			if !proxyrules.HostBound(was.hosts, host) {
				hosts = append(hosts, fmt.Sprintf("hosts %q: %s added", name, host))
			}
		}
	}
	switch {
	case len(widened) == 0 && unread == 0:
		fmt.Fprintln(out, "widens: none")
	case len(widened) == notCompared:
		fmt.Fprintln(out, "widens: none among the names compared")
	}
	for _, line := range append(widened, hosts...) {
		fmt.Fprintln(out, line)
	}
	if unread > 0 {
		fmt.Fprintf(out, "%s: %s may be behind a kind by a source's %s tag under both files, which only --secrets-json reads, so their allow and hosts were not compared\n", profilesCheckName, counted(unread, "name"), policy.TagProxy)
	}
}

// unreadWords is why a name's allow or hosts were not compared: the file, or the files, under
// which they are a kind no tag read gave, and why.
func unreadWords(was, is serving, oldName, newName string) string {
	var why []string
	if was.state == behindUnread {
		why = append(why, fmt.Sprintf("under %s %s", oldName, was.how))
	}
	if is.state == behindUnread {
		why = append(why, fmt.Sprintf("under %s %s", newName, is.how))
	}
	return strings.Join(why, "; ")
}

// writeServed writes, for a run with --secrets-json, each secret's proxy and proxy-narrowed tags
// as the list gives them, then each name behind the proxy under the old file (oldName) and how the
// new file (newName) serves it (servingOf), and answers the names that leave the proxy: behind it
// under the old file and behind nothing under the new one, which the release would then hand to an
// approved command as a value, or not serve at all.
func writeServed(out io.Writer, old, now policy.Profiles, tags *storeTags, oldName, newName string) []string {
	read := counted(len(tags.secrets), "agent secret")
	if tags.skipped > 0 {
		read += fmt.Sprintf(" (%s of no agent secret not read)", counted(tags.skipped, "entry"))
	}
	fmt.Fprintf(out, "%s: tags read from %s: %s\n", profilesCheckName, tags.path, read)
	for _, name := range tags.names() {
		for _, tag := range []string{policy.TagProxy, policy.TagProxyNarrowed} {
			if value, tagged := tags.secrets[name][tag]; tagged {
				fmt.Fprintf(out, "secret %q: %s %s\n", name, tag, shown(value))
			}
		}
	}
	var served []string
	for _, name := range sortedUnion(old.Names(), tags.names()) {
		if servingOf(old, name, tags).state == behindProxy {
			served = append(served, name)
		}
	}
	if len(served) == 0 {
		fmt.Fprintf(out, "%s: no name is behind the proxy under %s, so none can leave it\n", profilesCheckName, oldName)
		return nil
	}
	fmt.Fprintf(out, "%s: %s behind the proxy under %s, each under %s:\n", profilesCheckName, counted(len(served), "name"), oldName, newName)
	var leaving []string
	for _, name := range served {
		is := servingOf(now, name, tags)
		if is.state == behindProxy {
			fmt.Fprintf(out, "served %q: %s\n", name, is.how)
			continue
		}
		fmt.Fprintf(out, "served %q: leaves the proxy: %s\n", name, is.how)
		leaving = append(leaving, name)
	}
	if len(leaving) == 0 {
		fmt.Fprintf(out, "%s: no name leaves the proxy\n", profilesCheckName)
	} else {
		fmt.Fprintf(out, "%s: %s the proxy\n", profilesCheckName, leaveCount(len(leaving)))
	}
	return leaving
}

package main

import (
	"cmp"
	"flag"
	"fmt"
	"io"
	"maps"
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

// runProfilesCheck reads the profiles file args names and says whether the broker would serve it,
// with the broker's own code and no second reader or copy of its rules: policy.ParseProfiles, as
// the boot reads the file, and the policy load's own rules for each name behind Cipher's proxy.
// A pull request that changes the deployment's profiles.json runs it, so the file a reviewer is
// asked to read is one the broker has already agreed to serve.
//
// On a file it would serve it writes the report on stdout: what the file adds and removes against
// the file --against names, or against an empty profiles file, by which a whole file reads as
// added; first the kinds, then the profiles. A kind or profile added or removed is written whole,
// every field of it, the ones the file leaves out among them, and a block a profile does not have
// as that block's none. For one both files hold it writes each value of a list added or removed and
// each other field as old → new. Every field decides what a credential may reach or who may have
// it, so what is left for the reviewer is whether each is right.
//
// It refuses, exit 1 and one line on stderr per fault naming the file: a file ParseProfiles refuses
// (naming the kind or the profile and the field, or the parse position); a dpop block the boot
// refuses for its shape (config.MintedEntryRules); and each fault for which the policy load refuses
// a name behind the proxy, in the load's words (proxyFaults). The file gives a profile's proxy
// string; a source's tags are in the store, which --secrets-json reads, the output of `gcloud
// secrets list --format='json(name,annotations)'` for the agent secrets. Without it a profile with
// a proxy string is held to all the file decides of it, and one with none, in a file that declares
// a kind, to nothing, since its kind is its source's tag: such a profile gets an UNKNOWN line, and
// a run that refuses nothing exits 3 (exitUnknown) after the whole report, never 0.
//
// With --against it also writes, under the heading widens, each row of a name's allow under the
// file that its allow under the old file does not cover (proxyrules.Covers), for each name behind
// the proxy under both, and each host the change adds to a name (servingOf); with no tags read, in
// place of the names behind a kind by their own tag, each kind both files declare (writeWidens). A
// widening and an added host are listed, not refused: the reviewer reads them, and a gate that must
// see none reads the list. With --secrets-json it writes each secret's proxy and proxy-narrowed
// tags and each name behind the proxy under the old file, with how the file serves it, and exits 1
// when one leaves it.
// The old file is decoded and held to no rule (policy.DecodeProfiles): it may be one this broker
// refuses, proxy blocks the kinds replaced among them (policy.Profiles.LegacyProxy), and the report
// would otherwise refuse the pull request that moves it to kinds.
//
// A dpop block's tie to a private destination is the boot's to check with its own
// BROKER_PROXY_PRIVATE_UPSTREAM: given that value as --private-upstream, this runs the boot's whole
// check (config.ProxyMintedRules); without it, it writes one line per dpop block saying the tie was
// not checked. A usage error, a --private-upstream value that setting does not take among them, is
// exit 2 and the usage line. It reads no configuration, opens no database and binds nothing.
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
	refused, unknown := proxyFaults(now, tags)
	if len(refused) > 0 {
		for _, fault := range refused {
			fmt.Fprintf(stderr, "%s: %s: %s\n", profilesCheckName, path, fault)
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
	for _, line := range unknown {
		fmt.Fprintf(stdout, "%s: UNKNOWN: %s\n", profilesCheckName, line)
	}
	fmt.Fprintf(stdout, "%s: compared against %s\n", profilesCheckName, oldName)
	writeFileChanges(stdout, old, now)
	uncompared, kindsAlone := 0, 0
	if compared {
		uncompared, kindsAlone = writeWidens(stdout, old, now, tags, oldName, path)
	}
	if tags != nil {
		if leaving := writeServed(stdout, old, now, tags, oldName, path); len(leaving) > 0 {
			fmt.Fprintf(stderr, "%s: %s: %s leaving the proxy: %s\n", profilesCheckName, path, counted(len(leaving), "name"), strings.Join(leaving, ", "))
			return 1
		}
	}
	var unread []string
	if len(unknown) > 0 {
		unread = append(unread, "the kind of "+eachOf(len(unknown), "profile")+", its source's proxy tag")
	}
	if uncompared > 0 {
		unread = append(unread, "the allow or hosts of "+eachOf(uncompared, "name")+" against "+oldName)
	}
	if kindsAlone > 0 {
		unread = append(unread, "the names behind "+eachOf(kindsAlone, "kind")+" by their own proxy tag, compared as their kind alone")
	}
	if len(unread) > 0 {
		fmt.Fprintf(stderr, "%s: %s: UNKNOWN: not read (no --secrets-json): %s; nothing it read is refused\n", profilesCheckName, path, strings.Join(unread, ", and "))
		return exitUnknown
	}
	return 0
}

// eachOf is n things in words where each is meant: "1 profile", "each of 2 profiles".
func eachOf(n int, thing string) string {
	if n == 1 {
		return counted(n, thing)
	}
	return "each of " + counted(n, thing)
}

// exitUnknown is the exit status of a run that refuses nothing it read but could not read what the
// store's tags alone give: a profile's kind, a name's allow and hosts against the old file, or the
// names behind a kind by their own proxy tag. It is 3, not 1, which says the broker would refuse
// the file: a caller that cannot read the store (a pull-request job with no Secret Manager access)
// can accept 3 by its number and still fail on 1 and 2, and a gate that must know fails on it;
// with 1 for both, only the text tells them apart.
const exitUnknown = 3

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

// allowField is an allow list as a field: one value per row (policy.RowWords), or, for the two
// lists that mean something other than what they list, absent for a missing list (a kind's allows
// any method and path; a profile's is no allow of its own) and nothing for an empty one.
func allowField(name string, rows []proxyrules.Allow, absent string) reportField {
	switch {
	case rows == nil:
		return textField(name, absent)
	case len(rows) == 0:
		return textField(name, nothingWord)
	}
	items := make([]string, len(rows))
	for i, row := range rows {
		items[i] = policy.RowWords(row)
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
		rows[i] = policy.RowWords(a)
	}
	return strings.Join(rows, ", ")
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
//     string names (policy.ProfileKind); behind none when neither names one or the kind is no kind
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
		via := "its proxy string"
		if tag != "" {
			via = fmt.Sprintf("its source %s's %s tag", p.Source, policy.TagProxy)
		}
		switch kind := policy.ProfileKind(p, tag); {
		case kind != "":
			return byKind(s, kind, via)
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
// the new file (newName) that its allow under the old one (oldName) does not cover (widenedRows),
// as `widens "<name>": <row>`; then each host it reaches under the new file that it did not under
// the old one (addedHosts), as `hosts "<name>": <host> added`. A name whose allow or hosts are a
// kind that a source's proxy tag names on one side, with no tags read, gets a line saying it was
// not compared and why; where that is so on both sides, the names are counted on one line.
//
// With no tags read it cannot name a stored secret behind a kind by its own proxy tag, whose
// allow and hosts are its kind's, so it compares each kind both files declare directly instead,
// as `widens kind "<kind>": <row>` and `hosts kind "<kind>": <host> added`, and counts each such
// kind as unknown. With the tags each such secret is a name, compared as the others are.
//
// It writes "widens: none" when no row widens and nothing went uncompared, and "widens: none
// among what was compared" when no row widens of what it compared. It answers how many names it
// could not compare and how many kinds it compared in place of the names behind them, each an
// unknown of the run.
func writeWidens(out io.Writer, old, now policy.Profiles, tags *storeTags, oldName, newName string) (names, kinds int) {
	var stored []string
	if tags != nil {
		stored = tags.names()
	}
	var widened, hosts, notes []string
	found, notCompared, unread, hostsUnread := 0, 0, 0, 0
	for _, name := range sortedUnion(old.Names(), now.Names(), stored) {
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
		for _, row := range widenedRows(was.allow, is.allow) {
			widened = append(widened, fmt.Sprintf("widens %q: %s", name, row))
			found++
		}
		if !was.hostsKnown || !is.hostsKnown {
			hosts = append(hosts, fmt.Sprintf("hosts %q: not compared: %s", name, unreadWords(was, is, oldName, newName)))
			hostsUnread++
			continue
		}
		for _, host := range addedHosts(was.hosts, is.hosts) {
			hosts = append(hosts, fmt.Sprintf("hosts %q: %s added", name, host))
		}
	}
	if tags == nil {
		oldRules := old.FileRules()
		for _, name := range sortedUnion(kindNames(now)) {
			was := oldRules.Kind(name)
			if was == nil {
				continue
			}
			is := now.FileRules().Kind(name)
			kinds++
			for _, row := range widenedRows(was.Allow, is.Allow) {
				widened = append(widened, fmt.Sprintf("widens kind %q: %s", name, row))
				found++
			}
			for _, host := range addedHosts(was.Hosts, is.Hosts) {
				hosts = append(hosts, fmt.Sprintf("hosts kind %q: %s added", name, host))
			}
		}
	}
	if unread > 0 {
		notes = append(notes, fmt.Sprintf("%s: %s may be behind a kind by a source's %s tag under both files, which only --secrets-json reads, so their allow and hosts were not compared", profilesCheckName, counted(unread, "name"), policy.TagProxy))
	}
	if kinds > 0 {
		notes = append(notes, fmt.Sprintf("%s: the kinds both files declare were compared on their own (%s): a stored secret behind one by its own %s tag, which only --secrets-json reads, takes that kind's allow and hosts, and none was named", profilesCheckName, counted(kinds, "kind"), policy.TagProxy))
	}
	switch {
	case found > 0:
	case notCompared+unread+kinds == 0:
		fmt.Fprintln(out, "widens: none")
	default:
		fmt.Fprintln(out, "widens: none among what was compared")
	}
	for _, line := range slices.Concat(widened, hosts, notes) {
		fmt.Fprintln(out, line)
	}
	return unread + notCompared + hostsUnread, kinds
}

// widenedRows is each row of the allow is that the allow was does not cover (proxyrules.Covers),
// in the words of a row (policy.RowWords); a missing allow is "any method and path", which only a
// missing allow covers.
func widenedRows(was, is []proxyrules.Allow) []string {
	if is == nil {
		if was == nil {
			return nil
		}
		return []string{anyPath}
	}
	var rows []string
	for _, row := range proxyrules.Outside(was, is) {
		rows = append(rows, policy.RowWords(row))
	}
	return rows
}

// addedHosts is each host of is that no host of was binds (proxyrules.HostBound), in is's order.
func addedHosts(was, is []string) []string {
	var added []string
	for _, host := range is {
		if !proxyrules.HostBound(was, host) {
			added = append(added, host)
		}
	}
	return added
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
		fmt.Fprintf(out, "%s: %s leaving the proxy\n", profilesCheckName, counted(len(leaving), "name"))
	}
	return leaving
}

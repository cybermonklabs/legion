package main

import (
	"cmp"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/config"
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
const profilesCheckForm = profilesCommand + " " + profilesCheckCommand + " <file> [--against <old file>] [--private-upstream '<host>=<url> ...' [--cluster-ranges <CIDR prefixes>]]"

// profilesCheckUsage is the one line every usage error of the subcommand prints, in the form the
// end-sessions usage line takes.
const profilesCheckUsage = "broker: usage: broker " + profilesCheckForm + ", one profiles file and nothing else"

// profilesCheckName opens every line the subcommand writes.
const profilesCheckName = profilesCommand + " " + profilesCheckCommand

// privateUpstreamFlag is the flag that gives the subcommand the private destinations, the value
// BROKER_PROXY_PRIVATE_UPSTREAM gives the broker.
const privateUpstreamFlag = "private-upstream"

// clusterRangesFlag is the flag that gives the subcommand the broker's cluster ranges, the value
// BROKER_AIRLOCK_CLUSTER_RANGES gives the broker, against which each private destination's address
// is judged.
const clusterRangesFlag = "cluster-ranges"

// removedAirlockCheckCommand is the subcommand that checked the airlock's rules file, which the
// profiles file's proxy blocks replaced: it is refused naming `profiles check`, so a deployment's
// pull-request check that still runs it fails rather than reading a file of the old shape.
const removedAirlockCheckCommand = "airlock-check"

// runProfilesCheck reads the profiles file args names and says whether the broker would serve it,
// with the broker's own reader and no second reader of the format: policy.ParseProfiles, which the
// boot runs too, and then the boot's own checks of the dpop blocks - the results profile's
// (config.ResultsEntryRules) and every rule of a dpop block the file alone decides
// (config.MintedEntryRules) - so that what this agrees to is exactly what the boot agrees to but
// for the reads it has no setting for: each requester service against the deployment's registered
// services (readProfiles), each dpop block's host against the private destinations
// (config.ProxyMintedRules), which it checks only when --private-upstream gives it the
// destinations, and each destination's address against the broker's cluster ranges, which it
// checks only when --cluster-ranges gives it them too. A pull request that changes the
// deployment's profiles.json runs it, so the file a reviewer is asked to read is one the broker has
// already agreed to serve.
//
// --private-upstream takes the value BROKER_PROXY_PRIVATE_UPSTREAM gives the broker, <host>=<url>
// entries read by the parser config.Load reads the setting with (config.ParsePrivateUpstreams),
// and holds the file to config.ProxyMintedRules with those destinations, the check the boot of a
// broker with them makes: each dpop block binds a destination's host, and each destination needs a
// dpop block. It holds each destination to the boot's address rule too
// (config.PrivateUpstreamAddressRules, netaddr.DestinationRefusal): localhost and an address a
// destination is on no broker - loopback, link-local, the metadata servers, an IPv6 form that
// carries an IPv4 address and the rest of netaddr.DestinationNever - are refused as the boot
// refuses them. --cluster-ranges takes the value BROKER_AIRLOCK_CLUSTER_RANGES gives the broker,
// read by the parser config.Load reads that setting with (config.ParseAirlockClusterRanges), and
// with it the rule is the boot's whole one: an address outside the private ranges and outside
// every range given is refused, and one inside a range given is valid. Without it, such an
// address, which only a broker's BROKER_AIRLOCK_CLUSTER_RANGES admit, is valid with one line, per
// destination, that says it was not judged against the cluster ranges (no --cluster-ranges); a
// private address and a name need no range and get no such line. --cluster-ranges given empty is
// a broker given no ranges, and without a --private-upstream that names a destination it is a
// usage error, since the ranges judge nothing else here. Without --private-upstream, or with it
// blank, a file that holds dpop blocks is still valid, and the report says, in one line for each
// block after the line that says so, which host the block binds and that its tie to
// BROKER_PROXY_PRIVATE_UPSTREAM was not checked, since this run did not see the destinations.
//
// On a file it would serve it exits 0 and writes the report on stdout: what the file adds and
// removes against the file --against names, or against an empty profiles file when that flag is
// absent, which is how a whole file reads as added. A profile added or removed is written whole,
// every field of it, the ones the file leaves to the source among them, and a block it does not
// have as that block's none. For a profile both files hold it writes each value of a list added or
// removed (requesters, accounts, repositories, permissions, scopes, hosts, headers) and each other
// field as old → new, and a block added or removed whole. Every field is in the report because each
// decides what a credential may reach or who may have it: its source, tier, requesters and
// maxGrant; its accounts, repositories, permissions and value; its scopes; and where it may go,
// the proxy's hosts, headers, the methods and paths it may travel on (allow), the sort of
// credential (kind) and the longest grant a judge may give (judge), with the guidance the judge
// reads. What is left for the reviewer is whether each is right for its source.
//
// On a file it would not serve it exits 1 with one line on stderr naming the file and what the
// refusal says: the profile and the field, or the parse position for a file that is not JSON. A
// usage error is exit 2 and the usage line, and so is a --private-upstream value that is not one
// BROKER_PROXY_PRIVATE_UPSTREAM takes - one its parser refuses, or a destination whose address the
// boot refuses - and a --cluster-ranges value its parser refuses, each with the refusal, which
// quotes no part of a URL a credential can sit in and no element of the ranges. It reads no
// configuration, opens no database and binds nothing: the file, the --against file and the flags
// are all it reads.
//
// The --against file is decoded and never held to the file's rules (policy.DecodeProfiles): the
// profiles in force may be a file this broker would refuse, and what changed is the point of the
// report, which would otherwise also refuse the pull request that fixes that file.
func runProfilesCheck(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("broker "+profilesCheckName, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, profilesCheckUsage) }
	against := flags.String("against", "", "the profiles file in force now, the one the report is made against")
	upstreamFlag := flags.String(privateUpstreamFlag, "", "the <host>=<url> entries the broker's BROKER_PROXY_PRIVATE_UPSTREAM holds, against which the dpop blocks' hosts and each destination's address are checked")
	clusterRanges := flags.String(clusterRangesFlag, "", "the value BROKER_AIRLOCK_CLUSTER_RANGES gives the broker, comma-separated CIDR prefixes, against which each destination's address is checked")
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
	// The destinations are read here and not by a flag.Value, whose refusal the flag package
	// writes with the value quoted whole: config.ParsePrivateUpstreams, the parser config.Load
	// reads the setting with, refuses quoting no part of a URL a credential can sit in. A flag
	// given blank names no destination, as the setting does.
	rangesGiven := false
	flags.Visit(func(f *flag.Flag) { rangesGiven = rangesGiven || f.Name == clusterRangesFlag })
	text := strings.TrimSpace(*upstreamFlag)
	upstreamGiven := text != ""
	var upstreams []config.PrivateUpstream
	if upstreamGiven {
		var err error
		if upstreams, err = config.ParsePrivateUpstreams(text); err != nil {
			fmt.Fprintf(stderr, "%s: --%s: %v\n", profilesCheckName, privateUpstreamFlag, err)
			flags.Usage()
			return 2
		}
	}
	// The cluster ranges are read the same way, by the parser config.Load reads the setting with,
	// which names an element's position and never the element. They judge the destinations'
	// addresses and nothing else here, so without a destination they are a usage error rather than
	// a flag that checks nothing; given empty, they are a broker given no ranges.
	var ranges []netip.Prefix
	if rangesGiven {
		if !upstreamGiven {
			fmt.Fprintf(stderr, "%s: --%s judges the address of each destination --%s gives, and was given without one\n", profilesCheckName, clusterRangesFlag, privateUpstreamFlag)
			flags.Usage()
			return 2
		}
		var err error
		if ranges, err = config.ParseAirlockClusterRanges(*clusterRanges); err != nil {
			fmt.Fprintf(stderr, "%s: --%s: %v\n", profilesCheckName, clusterRangesFlag, err)
			flags.Usage()
			return 2
		}
	}
	// Each destination's address, held as the boot holds it: given the cluster ranges, the boot's
	// whole rule; without them, an address a destination is on no broker is refused, and one only
	// the cluster ranges could admit is said not to be judged against them. A destination whose
	// address the boot refuses is no value BROKER_PROXY_PRIVATE_UPSTREAM takes, so its refusal is
	// a usage error, as the parser's is.
	var unjudged []netip.Addr
	for _, upstream := range upstreams {
		addr, err := config.PrivateUpstreamAddressRules(upstream, ranges, rangesGiven)
		if err != nil {
			fmt.Fprintf(stderr, "%s: --%s: %v\n", profilesCheckName, privateUpstreamFlag, err)
			flags.Usage()
			return 2
		}
		if addr.IsValid() {
			unjudged = append(unjudged, addr)
		}
	}
	now, err := readProfilesFile(path, policy.ParseProfiles)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", profilesCheckName, err)
		return 1
	}
	// The dpop blocks, held as the boot holds them: the results profile's own rules
	// (config.ResultsEntryRules, with no result entries) and every rule the file alone decides
	// (config.MintedEntryRules), and, given the destinations, the boot's whole check of the blocks
	// against them (config.ProxyMintedRules), of which those rules are the first part.
	rules := now.ProxyRules()
	if err := config.ResultsEntryRules(rules, false); err != nil {
		fmt.Fprintf(stderr, "%s: %s: %v\n", profilesCheckName, path, err)
		return 1
	}
	if upstreamGiven {
		err = config.ProxyMintedRules(rules, upstreams)
	} else {
		err = config.MintedEntryRules(rules)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s: %v\n", profilesCheckName, path, err)
		return 1
	}
	var old policy.Profiles
	oldName := "an empty profiles file, so every profile reads as added"
	if name := strings.TrimSpace(*against); name != "" {
		if old, err = readProfilesFile(name, policy.DecodeProfiles); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", profilesCheckName, err)
			return 1
		}
		oldName = name
	}
	fmt.Fprintf(stdout, "%s: %s is valid: %s\n", profilesCheckName, path, profileCount(len(now.Names())))
	if !upstreamGiven {
		for _, entry := range rules.Secrets {
			if entry.Kind == proxyrules.KindDPoP {
				fmt.Fprintf(stdout, "%s: %s binds %s; its tie to BROKER_PROXY_PRIVATE_UPSTREAM was not checked (no --%s)\n", profilesCheckName, entry.Name, strings.Join(entry.Hosts, " "), privateUpstreamFlag)
			}
		}
	}
	for _, addr := range unjudged {
		fmt.Fprintf(stdout, "%s: BROKER_PROXY_PRIVATE_UPSTREAM names %s, an address outside the private ranges, which a broker takes as a destination only inside its BROKER_AIRLOCK_CLUSTER_RANGES; it was not judged against the cluster ranges (no --%s)\n",
			profilesCheckName, addr, clusterRangesFlag)
	}
	fmt.Fprintf(stdout, "%s: compared against %s\n", profilesCheckName, oldName)
	writeProfilesChanges(stdout, old, now)
	return 0
}

// profileCount counts the profiles of a file in words, as the end-sessions subcommand counts the
// sessions it ended.
func profileCount(n int) string {
	if n == 1 {
		return "1 profile"
	}
	return fmt.Sprintf("%d profiles", n)
}

// The words the report writes for a field the file leaves out, where a word other than the
// value's own says what the broker does.
const (
	// sourcesWord is a tier, requesters or maxGrant the profile takes from its source.
	sourcesWord = "the source's"
	// everyRepository is a github block with no repositories: its token reaches every repository
	// of the installation.
	everyRepository = "every repository the installation reaches"
	// noneWord is a block the profile does not have, and a list that names nothing.
	noneWord = "none"
)

// reportField is one field of a profile as the report writes it, under the name the file's JSON
// gives it: a list, whose values each take a line of their own and are added and removed one at a
// time, or one text.
type reportField struct {
	name  string
	list  bool
	items []string
	text  string
}

// reportBlock is one block of a profile as the report writes it: its fields, nil when the profile
// has no such block.
type reportBlock struct {
	name   string
	fields []reportField
}

// profileReport is one profile as the report writes it: its own fields and then its three
// blocks, each in the order the file's JSON gives them.
type profileReport struct {
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

// reportOf is every field of p as the report writes it, in policy.ParseProfiles' form: a source
// written out, accounts lowercased and sorted, repositories sorted, a value's default written
// out, so that two spellings of one profile report alike.
func reportOf(p *policy.Profile) profileReport {
	maxGrant := sourcesWord
	if p.MaxGrant != 0 {
		maxGrant = p.MaxGrant.String()
	}
	report := profileReport{fields: []reportField{
		textField("source", p.Source),
		textField("tier", cmp.Or(p.Tier, sourcesWord)),
		listOr("requesters", p.Requesters, sourcesWord),
		textField("maxGrant", maxGrant),
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
	proxy := reportBlock{name: "proxy"}
	if b := p.Proxy; b != nil {
		proxy.fields = []reportField{
			listField("proxy.hosts", b.Hosts),
			listField("proxy.headers", proxyrules.SecretRule{Headers: b.Headers}.HeaderNames()),
			textField("proxy.allow", allowText(b.Allow)),
			textField("proxy.kind", shown(b.Kind)),
			textField("proxy.judge", judgeText(b.Judge)),
			textField("proxy.guidance", strconv.Quote(b.Guidance)),
		}
	}
	report.blocks = []reportBlock{github, google, proxy}
	return report
}

// writeProfilesChanges writes the report: one line per fact, grouped by profile and in a stable
// order, profile names sorted and then each profile's fields in the file's order, so that two runs
// on the same pair of files write the same bytes and a reviewer can read two reports against each
// other. It writes one line saying so when the two files say the same thing.
//
// Where a file repeats a profile's name, the profile read is the first one, as Profiles.Lookup
// and so the broker itself reads it. The new file is validated before this runs and a repeated
// name is a refusal there; only an old file can hold one.
func writeProfilesChanges(out io.Writer, old, now policy.Profiles) {
	changed := false
	for _, name := range sortedUnion(old.Names(), now.Names()) {
		was, is := old.Lookup(name), now.Lookup(name)
		switch {
		case is == nil:
			fmt.Fprintf(out, "profile %q: removed\n", name)
			writeWholeProfile(out, name, reportOf(was))
			changed = true
		case was == nil:
			fmt.Fprintf(out, "profile %q: added\n", name)
			writeWholeProfile(out, name, reportOf(is))
			changed = true
		default:
			if writeProfileChanges(out, name, reportOf(was), reportOf(is)) {
				changed = true
			}
		}
	}
	if !changed {
		fmt.Fprintf(out, "%s: nothing changed\n", profilesCheckName)
	}
}

// writeWholeProfile writes every field of a profile added or removed, and each block it does not
// have as that block's none.
func writeWholeProfile(out io.Writer, name string, report profileReport) {
	writeFields(out, name, report.fields)
	for _, block := range report.blocks {
		if block.fields == nil {
			fmt.Fprintf(out, "profile %q: %s %s\n", name, block.name, noneWord)
			continue
		}
		writeFields(out, name, block.fields)
	}
}

// writeFields writes each field whole: a list one line per value, or none when it names nothing,
// and a text on one line.
func writeFields(out io.Writer, name string, fields []reportField) {
	for _, field := range fields {
		if !field.list {
			fmt.Fprintf(out, "profile %q: %s %s\n", name, field.name, field.text)
			continue
		}
		if len(field.items) == 0 {
			fmt.Fprintf(out, "profile %q: %s %s\n", name, field.name, noneWord)
		}
		for _, item := range field.items {
			fmt.Fprintf(out, "profile %q: %s %s\n", name, field.name, item)
		}
	}
}

// writeProfileChanges writes what one profile both files hold has changed, and reports whether it
// wrote anything: each field's change, and a block one file has and the other does not as that
// block added or removed, with each of its fields.
func writeProfileChanges(out io.Writer, name string, was, is profileReport) bool {
	changed := false
	for i := range was.fields {
		if writeFieldChanges(out, name, was.fields[i], is.fields[i]) {
			changed = true
		}
	}
	for i, wasBlock := range was.blocks {
		isBlock := is.blocks[i]
		switch {
		case wasBlock.fields == nil && isBlock.fields == nil:
		case wasBlock.fields == nil:
			fmt.Fprintf(out, "profile %q: %s added\n", name, isBlock.name)
			writeFields(out, name, isBlock.fields)
			changed = true
		case isBlock.fields == nil:
			fmt.Fprintf(out, "profile %q: %s removed\n", name, wasBlock.name)
			writeFields(out, name, wasBlock.fields)
			changed = true
		default:
			for j := range wasBlock.fields {
				if writeFieldChanges(out, name, wasBlock.fields[j], isBlock.fields[j]) {
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
func writeFieldChanges(out io.Writer, name string, was, is reportField) bool {
	if was.list && is.list {
		changed := false
		for _, value := range sortedUnion(was.items, is.items) {
			switch {
			case !slices.Contains(was.items, value):
				fmt.Fprintf(out, "profile %q: %s %s added\n", name, is.name, value)
				changed = true
			case !slices.Contains(is.items, value):
				fmt.Fprintf(out, "profile %q: %s %s removed\n", name, is.name, value)
				changed = true
			}
		}
		return changed
	}
	if wasText, isText := fieldText(was), fieldText(is); wasText != isText {
		fmt.Fprintf(out, "profile %q: %s %s → %s\n", name, is.name, wasText, isText)
		return true
	}
	return false
}

// fieldText is a field as one text: a list's values joined by spaces, or none when it names
// nothing.
func fieldText(field reportField) string {
	if !field.list {
		return field.text
	}
	if len(field.items) == 0 {
		return noneWord
	}
	return strings.Join(field.items, " ")
}

// allowText reads a proxy block's allow list as a line of the report, with the two lists that
// mean something other than what they list in words (proxyrules.allowOK: a missing list allows
// any method and path, an empty one allows nothing).
func allowText(allow []proxyrules.Allow) string {
	switch {
	case allow == nil:
		return "any method and path"
	case len(allow) == 0:
		return "nothing"
	}
	rows := make([]string, len(allow))
	for i, a := range allow {
		rows[i] = shown(a.Method) + " " + shown(a.PathPrefix)
	}
	return strings.Join(rows, ", ")
}

// judgeText reads a proxy block's judge lifetime as a line of the report. No judge at all and a
// judge of zero seconds are different answers, so the empty one is a word and not a blank.
func judgeText(judge *uint32) string {
	if judge == nil {
		return noneWord
	}
	return fmt.Sprintf("%d seconds", *judge)
}

// shown writes an empty field as a pair of quotes, so that a line of the report never ends in a
// blank where a reader cannot tell an empty value from a missing word.
func shown(value string) string {
	if value == "" {
		return `""`
	}
	return value
}

// sortedUnion is every value in either list, once each, sorted.
func sortedUnion(was, is []string) []string {
	values := make(map[string]struct{}, len(was)+len(is))
	for _, v := range was {
		values[v] = struct{}{}
	}
	for _, v := range is {
		values[v] = struct{}{}
	}
	return slices.Sorted(maps.Keys(values))
}
